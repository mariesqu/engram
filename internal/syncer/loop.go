package syncer

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
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
//   - nil            → not applicable (no error)
//   - *StatusError   → delegates to its Retryable() method (5xx → true, 4xx → false)
//   - any other err  → true  (network/IO/local errors are transient by default)
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
	// starved indefinitely. Default: 2min.
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
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Loop runs syncer.Sync in the background on a periodic schedule with
// debounced on-demand triggering and exponential backoff on retryable errors.
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
	project string
	cfg     Config

	// triggerCh carries Trigger() signals. Size 1: a pending signal is as good
	// as N pending signals — one sync drains all local writes.
	triggerCh chan struct{}

	// started guards against double-Start.
	started atomic.Bool

	// stopOnce ensures Stop() is idempotent.
	stopOnce sync.Once

	// cancel is set by Start to cancel the run goroutine's context.
	cancel context.CancelFunc

	// done is closed by the run goroutine just before it exits, allowing Stop()
	// to block until the goroutine has fully wound down.
	done chan struct{}
}

// NewLoop constructs a Loop. cfg zero values are replaced by defaults.
func NewLoop(node *Node, central Central, project string, cfg Config) *Loop {
	return &Loop{
		node:      node,
		central:   central,
		project:   project,
		cfg:       applyDefaults(cfg),
		triggerCh: make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
}

// Start launches the background sync goroutine and returns immediately.
// The goroutine exits when ctx is cancelled or Stop() is called.
// Calling Start more than once panics.
func (l *Loop) Start(ctx context.Context) {
	if !l.started.CompareAndSwap(false, true) {
		panic("syncer.Loop: Start called more than once")
	}
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

// Stop signals the goroutine to exit and blocks until it does. It is
// idempotent and safe to call after ctx cancellation.
func (l *Loop) Stop() {
	l.stopOnce.Do(func() {
		if l.cancel != nil {
			l.cancel()
		}
	})
	// Block until the goroutine reports done, regardless of how many times Stop
	// is called (the done channel is only ever closed once).
	<-l.done
}

// run is the goroutine body. It exits when runCtx is cancelled.
func (l *Loop) run(ctx context.Context) {
	defer close(l.done)

	var (
		backoff   time.Duration // 0 = no active backoff
		triggered bool          // a Trigger() arrived but we haven't synced yet
	)

	// Drain any trigger that was fired before Start (harmless, but tidy).
	select {
	case <-l.triggerCh:
		triggered = true
	default:
	}

	timer := time.NewTimer(l.waitDuration(triggered, backoff))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-l.triggerCh:
			// A trigger arrived. Set the flag; reset the timer to use the shorter
			// debounce wait (but never shorter than any active backoff floor).
			triggered = true
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(l.waitDuration(triggered, backoff))

		case <-timer.C:
			// Timer fired — run one sync.
			triggered = false // consumed

			pushed, pulled, err := Sync(ctx, l.node, l.central, l.project)
			if err != nil {
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
					l.cfg.Logger.Warn("syncer.Loop: sync failed (retryable), backing off",
						"error", err,
						"backoff", backoff,
						"node", l.node.Name,
					)
				} else {
					// Non-retryable (4xx): clear backoff, log, resume normal cadence.
					// Do NOT hot-loop — the next sync waits a full Interval.
					backoff = 0
					l.cfg.Logger.Warn("syncer.Loop: sync failed (non-retryable), resuming normal cadence",
						"error", err,
						"node", l.node.Name,
					)
				}
			} else {
				// Success: clear backoff, resume normal cadence.
				backoff = 0
				l.cfg.Logger.Debug("syncer.Loop: sync ok",
					"pushed", pushed,
					"pulled", pulled,
					"node", l.node.Name,
				)
			}

			// Re-arm the timer for the next cycle.
			timer.Reset(l.waitDuration(triggered, backoff))
		}
	}
}

// waitDuration computes how long to wait before the next sync.
//
// Rules:
//  1. If a trigger is pending, use Debounce (respond quickly to local writes).
//  2. Otherwise use Interval (periodic cadence).
//  3. Backoff is always a FLOOR: never sync sooner than backoff, even when
//     triggered (so a failing server is never hammered).
func (l *Loop) waitDuration(triggered bool, backoff time.Duration) time.Duration {
	base := l.cfg.Interval
	if triggered {
		base = l.cfg.Debounce
	}
	if backoff > base {
		return backoff
	}
	return base
}
