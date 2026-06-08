package syncer

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// retryabler is a duck-typed interface matched by remote.StatusError (and any
// Central implementation that wants to signal retryability). Using a local
// interface keeps internal/syncer free of any import from internal/remote —
// the Loop works with ANY Central that surfaces this method.
type retryabler interface {
	Retryable() bool
}

// isRetryable classifies a sync error without importing internal/remote.
//
//   - nil                        → not applicable (no error)
//   - any error with Retryable() → delegates to its Retryable method (e.g. remote.StatusError: 5xx → true, 4xx → false)
//   - any other err              → true  (network/IO/local errors are transient by default)
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	var r retryabler
	if errors.As(err, &r) {
		return r.Retryable()
	}
	return true // unknown transport/local error → treat as transient
}

// Config holds tunables for a Loop. Zero values are replaced by defaults via
// applyDefaults. All durations must be positive after defaulting.
type Config struct {
	// Interval is the cadence between periodic syncs when idle (no trigger, no
	// backoff). Default: 30s.
	Interval time.Duration

	// Debounce is how long the loop waits after a Trigger() call before
	// actually running the sync, coalescing rapid successive triggers into one
	// sync. Default: 1s.
	Debounce time.Duration

	// BackoffMin is the initial backoff wait after the first retryable error.
	// Default: 1s.
	BackoffMin time.Duration

	// BackoffMax caps the exponential backoff so a failing server is never
	// starved indefinitely. Default: 2min. Normalized up to BackoffMin if a caller
	// configures it lower, so the cap is never below the floor.
	BackoffMax time.Duration

	// Logger is used for debug/warn messages. Default: slog.Default().
	Logger *slog.Logger
}

// applyDefaults fills in zero (unset) Config fields with their defaults.
func applyDefaults(c Config) Config {
	if c.Interval <= 0 {
		c.Interval = 30 * time.Second
	}
	if c.Debounce <= 0 {
		c.Debounce = 1 * time.Second
	}
	if c.BackoffMin <= 0 {
		c.BackoffMin = 1 * time.Second
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 2 * time.Minute
	}
	// Normalize: the cap must never be below the floor. If a caller sets
	// BackoffMin > BackoffMax, raise the cap to the floor so the first backoff
	// (= BackoffMin) never exceeds the documented cap.
	if c.BackoffMax < c.BackoffMin {
		c.BackoffMax = c.BackoffMin
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Loop runs SyncAllProjects in the background on a periodic schedule with
// debounced on-demand triggering and exponential backoff on retryable errors.
//
// Each tick calls SyncAllProjects: Push once (drain outbox → central) then Pull
// each project the local store knows about, using per-project pull cursors. This
// is correct for any number of projects — including zero (no-op pull round) and
// one (degenerate case identical to the old single-project loop).
//
// Construct with NewLoop; start with Start(ctx); request an early sync with
// Trigger(); stop with Stop() or by cancelling the ctx.
//
// The Loop does NOT own the Node's store — the caller is responsible for
// opening and closing the store. Crucially, Stop() blocks until the goroutine
// exits, so the caller can safely close the store immediately after Stop()
// returns.
type Loop struct {
	node    *Node
	central Central
	cfg     Config

	// triggerCh carries Trigger() signals. Size 1: a pending signal is as good
	// as N pending signals — one sync drains all local writes.
	triggerCh chan struct{}

	// mu serializes Start/Stop lifecycle state. It guards started, stopped, and
	// cancel so that a concurrent Stop cannot observe started==true while cancel
	// is still nil (which would be a data race).
	mu      sync.Mutex
	started bool
	stopped bool

	// cancel is set by Start under mu to cancel the run goroutine's context.
	// Safe to read in Stop because Stop checks started==true under the same mu
	// before reading cancel, guaranteeing it is non-nil.
	cancel context.CancelFunc

	// done is closed by the run goroutine just before it exits, allowing Stop()
	// to block until the goroutine has fully wound down.
	done chan struct{}
}

// NewLoop constructs a Loop. cfg zero values are replaced by defaults.
// The Loop drives SyncAllProjects per tick — no project parameter is required.
func NewLoop(node *Node, central Central, cfg Config) *Loop {
	return &Loop{
		node:      node,
		central:   central,
		cfg:       applyDefaults(cfg),
		triggerCh: make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
}

// Start launches the background sync goroutine and returns immediately.
// The goroutine exits when ctx is cancelled or Stop() is called.
// Calling Start more than once panics.
//
// Start and Stop are safe to call concurrently: the mutex serializes lifecycle
// transitions so cancel is always published before started becomes visible.
func (l *Loop) Start(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started {
		panic("syncer.Loop: Start called more than once")
	}
	l.started = true
	runCtx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	go l.run(runCtx)
}

// Trigger requests a sync "soon" (after Debounce). It is non-blocking: if a
// trigger is already pending the call is a no-op. The caller must never block
// on Trigger — it is designed to be called right after a local write.
func (l *Loop) Trigger() {
	select {
	case l.triggerCh <- struct{}{}:
	default:
		// A trigger is already queued; one sync will handle all pending writes.
	}
}

// Stop signals the goroutine to exit and blocks until it does.
//
// Stop is idempotent and safe to call concurrently with Start, with itself, or
// after ctx cancellation.
//
// If the Loop was never Started, Stop is a no-op and returns immediately (so a
// caller can put Stop in a cleanup path even when Start was conditional).
//
// The done-channel wait happens outside the mutex so that the lock is not held
// while blocking — avoiding a deadlock if run() ever tries to acquire mu
// (it does not, but the pattern is correct regardless).
func (l *Loop) Stop() {
	l.mu.Lock()
	if !l.started {
		l.mu.Unlock()
		return // never started: nothing to cancel, done is never closed
	}
	if !l.stopped {
		l.stopped = true
		l.cancel() // safe: started==true guarantees cancel is set (both under mu)
	}
	l.mu.Unlock()

	// Wait for the goroutine to fully exit outside the lock.
	<-l.done
}

// run is the goroutine body. It exits when runCtx is cancelled.
// run reads only immutable fields (node, central, project, cfg, triggerCh, done)
// and the runCtx it receives as a parameter. It never reads mu-guarded fields
// (started, stopped, cancel), keeping it entirely mutex-free.
func (l *Loop) run(ctx context.Context) {
	defer close(l.done)

	var (
		backoff       time.Duration // 0 = no active backoff
		triggered     bool          // a Trigger() arrived but we haven't synced yet
		intervalFloor bool          // true → next wait is floored at Interval (non-retryable error)
	)

	// Drain any trigger that was fired before Start (harmless, but tidy).
	select {
	case <-l.triggerCh:
		triggered = true
	default:
	}

	timer := time.NewTimer(l.waitDuration(triggered, backoff, intervalFloor))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-l.triggerCh:
			// A trigger arrived. Set the flag; reset the timer to use the shorter
			// debounce wait (but never shorter than any active backoff or interval floor).
			triggered = true
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(l.waitDuration(triggered, backoff, intervalFloor))

		case <-timer.C:
			// Timer fired — run one multi-project sync round.
			triggered = false // consumed

			pushed, pulled, err := SyncAllProjects(ctx, l.node, l.central)
			if err != nil {
				if ctx.Err() != nil {
					// Context cancelled (Stop or parent shutdown) mid-sync: exit
					// cleanly without logging the cancellation as a sync failure.
					return
				}
				if isRetryable(err) {
					// Exponential backoff: first failure → BackoffMin; subsequent → *2 up to max.
					if backoff == 0 {
						backoff = l.cfg.BackoffMin
					} else {
						backoff *= 2
						if backoff > l.cfg.BackoffMax {
							backoff = l.cfg.BackoffMax
						}
					}
					intervalFloor = false
					l.cfg.Logger.Warn("syncer.Loop: sync failed (retryable), backing off",
						"error", err,
						"backoff", backoff,
						"node", l.node.Name,
					)
				} else {
					// Non-retryable (4xx): clear exponential backoff but floor the next
					// wait (and any Trigger before it) at Interval so a persistent 4xx is
					// not hammered. One Interval cooldown, then normal cadence resumes.
					backoff = 0
					intervalFloor = true
					l.cfg.Logger.Warn("syncer.Loop: sync failed (non-retryable), cooling down for one Interval",
						"error", err,
						"node", l.node.Name,
					)
				}
			} else {
				// Success: clear backoff and interval floor, resume normal cadence.
				backoff = 0
				intervalFloor = false
				l.cfg.Logger.Debug("syncer.Loop: sync ok",
					"pushed", pushed,
					"pulled", pulled,
					"node", l.node.Name,
				)
			}

			// Re-arm the timer for the next cycle.
			timer.Reset(l.waitDuration(triggered, backoff, intervalFloor))
		}
	}
}

// waitDuration computes how long to wait before the next sync.
//
// Rules:
//  1. If a trigger is pending, use Debounce (respond quickly to local writes).
//  2. Otherwise use Interval (periodic cadence).
//  3. Backoff is always a FLOOR: never sync sooner than backoff, even when
//     triggered (so a failing server is never hammered on retryable errors).
//  4. intervalFloor is an additional one-cycle floor at Interval: applied after
//     a non-retryable error so a persistent 4xx is not hammered even via Trigger.
func (l *Loop) waitDuration(triggered bool, backoff time.Duration, intervalFloor bool) time.Duration {
	base := l.cfg.Interval
	if triggered {
		base = l.cfg.Debounce
	}
	floor := backoff
	if intervalFloor && l.cfg.Interval > floor {
		floor = l.cfg.Interval
	}
	if floor > base {
		return floor
	}
	return base
}
