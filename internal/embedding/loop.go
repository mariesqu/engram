package embedding

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/mariesqu/engram/internal/localstore"
)

// LoopConfig holds tunables for a Loop. Zero values are replaced by defaults via
// applyLoopDefaults. All durations must be positive after defaulting.
type LoopConfig struct {
	// Interval is the cadence between periodic backfill passes when idle.
	// Default: 60s.
	Interval time.Duration

	// Debounce is how long the loop waits after a Trigger() call before
	// actually running, coalescing rapid successive triggers into one pass.
	// Default: 1s.
	Debounce time.Duration

	// BackoffMin is the initial backoff wait after the first provider error.
	// Default: 1s.
	BackoffMin time.Duration

	// BackoffMax caps the exponential backoff. Default: 2min.
	BackoffMax time.Duration

	// BatchSize is the maximum number of rows to process per Embed call.
	// Default: 100. Keep small enough to avoid large API payloads.
	BatchSize int

	// BatchPause is the delay between consecutive batch calls within one tick.
	// Default: 1s (rate-limit guard per spec).
	BatchPause time.Duration

	// Logger is used for debug/warn messages. Default: slog.Default().
	Logger *slog.Logger
}

// applyLoopDefaults fills in zero (unset) LoopConfig fields with their defaults.
func applyLoopDefaults(c LoopConfig) LoopConfig {
	if c.Interval <= 0 {
		c.Interval = 60 * time.Second
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
	if c.BackoffMax < c.BackoffMin {
		c.BackoffMax = c.BackoffMin
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	// BatchPause: 0 means "no pause between batches" (valid for tests or
	// callers that manage rate-limiting externally). Only apply the default
	// when a negative sentinel is passed, which callers should never do.
	// Production code should set BatchPause explicitly (1s is the spec default).
	if c.BatchPause < 0 {
		c.BatchPause = 1 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Loop runs the embedding backfill in the background on a periodic schedule
// with debounced on-demand triggering and exponential backoff on provider errors.
//
// On each tick the loop:
//  1. SELECTs a batch of rows where embedding IS NULL OR embedding_model != current.
//  2. Groups by project; per-project group: calls gated.Embed (privacy gate enforced).
//     Rows from gated (omitted/local-only) projects return ErrEmbeddingGated → skip silently.
//  3. Calls localstore.UpdateEmbedding per row (L2-normalised vector, current model, ts).
//  4. Sleeps BatchPause between consecutive batch Embed calls (rate-limit guard).
//  5. Stops for this tick when SelectEmbeddable returns zero rows (caught up).
//
// Livelock safety: permanently-gated rows (omitted/local-only projects) have
// embedding=NULL and ARE returned by SelectEmbeddable on every tick. The loop
// skips them in-memory and continues to the next project group. When all remaining
// rows are gated, the eligible per-project embed calls are all no-ops; on the next
// batch fetch there are no more ungated rows with NULL embedding, so
// SelectEmbeddable still returns rows (the gated ones) — BUT we track whether any
// eligible (non-gated) rows were processed in this batch; if none were processed,
// the tick terminates. This guarantees termination: the tick is "caught up" when
// no ELIGIBLE work remains, not when no rows match the predicate.
//
// Construct with NewLoop; start with Start(ctx); request an early pass with
// Trigger(); stop with Stop() or by cancelling the ctx.
//
// Stop() blocks until the goroutine fully exits so the caller can safely close
// the store immediately after Stop() returns.
type Loop struct {
	store     localstore.EmbeddableStore
	gated     EmbeddingProvider
	cfg       LoopConfig
	triggerCh chan struct{}

	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// NewLoop constructs a backfill Loop. cfg zero values are replaced by defaults.
// store must satisfy localstore.EmbeddableStore (*localstore.Store in production).
// gated must be the output of NewGated — never a raw provider.
func NewLoop(store localstore.EmbeddableStore, gated EmbeddingProvider, cfg LoopConfig) *Loop {
	return &Loop{
		store:     store,
		gated:     gated,
		cfg:       applyLoopDefaults(cfg),
		triggerCh: make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
}

// Start launches the background backfill goroutine and returns immediately.
// Calling Start more than once panics.
func (l *Loop) Start(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started {
		panic("embedding.Loop: Start called more than once")
	}
	l.started = true
	runCtx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	go l.run(runCtx)
}

// Trigger requests a backfill pass "soon" (after Debounce). Non-blocking: if a
// trigger is already pending the call is a no-op.
//
// Nil-safe: when called on a nil *Loop (Noop provider mode) it is a no-op.
func (l *Loop) Trigger() {
	if l == nil {
		return
	}
	select {
	case l.triggerCh <- struct{}{}:
	default:
		// A trigger is already queued; the pending pass will cover this write.
	}
}

// Stop signals the goroutine to exit and blocks until it does.
// Idempotent and nil-safe: if the loop is nil or was never started, Stop is a no-op.
func (l *Loop) Stop() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if !l.started {
		l.mu.Unlock()
		return
	}
	if !l.stopped {
		l.stopped = true
		l.cancel()
	}
	l.mu.Unlock()
	<-l.done
}

// run is the goroutine body. It exits when ctx is cancelled.
// It mirrors syncer.Loop.run in structure (trigger coalescing + timer + backoff).
func (l *Loop) run(ctx context.Context) {
	defer close(l.done)

	var (
		backoff   time.Duration
		triggered bool
	)

	// Drain any pre-Start trigger.
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
			triggered = true
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(l.waitDuration(triggered, backoff))

		case <-timer.C:
			triggered = false

			if err := l.runTick(ctx); err != nil {
				if ctx.Err() != nil {
					return // cancelled mid-tick; exit cleanly
				}
				// Provider error: exponential backoff.
				if backoff == 0 {
					backoff = l.cfg.BackoffMin
				} else {
					backoff *= 2
					if backoff > l.cfg.BackoffMax {
						backoff = l.cfg.BackoffMax
					}
				}
				l.cfg.Logger.Warn("embedding.Loop: tick failed, backing off",
					"error", err,
					"backoff", backoff,
				)
			} else {
				backoff = 0
			}

			timer.Reset(l.waitDuration(triggered, backoff))
		}
	}
}

// waitDuration mirrors syncer.Loop.waitDuration:
// triggers use Debounce, idle uses Interval, backoff is always a floor.
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

// runTick performs one complete backfill pass. It processes rows in batches until
// no eligible (non-gated) rows remain. Gated rows are skipped silently.
//
// Returns a non-nil error only on provider failure (not for gating or ctx cancel).
func (l *Loop) runTick(ctx context.Context) error {
	currentModel := l.gated.ModelName()
	now := time.Now().UTC().Format(time.RFC3339)
	db := l.store.RawDB()
	batchSize := l.cfg.BatchSize
	firstBatch := true

	for {
		if ctx.Err() != nil {
			return nil // cancelled — not a provider error
		}

		rows, err := localstore.SelectEmbeddable(db, currentModel, batchSize)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil // all rows embedded or predicate matches zero
		}

		// Rate-limit pause between consecutive Embed calls, but not before the first.
		if !firstBatch && l.cfg.BatchPause > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(l.cfg.BatchPause):
			}
		}
		firstBatch = false

		// Group by project (preserving insertion order) so the gate is evaluated
		// per-project group rather than per-row — one Embed call per project group.
		type projectBatch struct {
			syncIDs []string
			texts   []string
		}
		projectOrder := make([]string, 0, len(rows))
		projectBatches := make(map[string]*projectBatch, len(rows))

		for i := range rows {
			proj := rows[i].Project
			if _, seen := projectBatches[proj]; !seen {
				projectOrder = append(projectOrder, proj)
				projectBatches[proj] = &projectBatch{}
			}
			pb := projectBatches[proj]
			pb.syncIDs = append(pb.syncIDs, rows[i].SyncID)
			pb.texts = append(pb.texts, rows[i].Text)
		}

		// Track whether ANY project group produced eligible (non-gated) work.
		// If this full batch was entirely gated, terminate the tick to prevent livelock.
		anyEligible := false
		var tickErr error

		for _, proj := range projectOrder {
			if ctx.Err() != nil {
				return nil
			}
			pb := projectBatches[proj]

			vecs, embedErr := l.gated.Embed(ctx, proj, pb.texts)
			if errors.Is(embedErr, ErrEmbeddingGated) {
				// Policy denies this project — skip silently (spec: "NOT counted as errors").
				l.cfg.Logger.Debug("embedding.Loop: project gated, skipping", "project", proj)
				continue
			}
			if embedErr != nil {
				// Transient provider error — log, record, skip this project group.
				// The rows remain NULL and are retried on the next tick.
				l.cfg.Logger.Warn("embedding.Loop: provider error for project",
					"project", proj,
					"error", embedErr,
				)
				tickErr = embedErr
				continue
			}

			// Provider succeeded for this project — mark eligible work done.
			anyEligible = true

			// Write each normalised vector back. UpdateEmbedding is idempotent:
			// AND embedding IS NULL means a race with a concurrent write is safe.
			for i, vec := range vecs {
				normVec := localstore.L2Normalize(vec)
				if ue := localstore.UpdateEmbeddingStale(db, pb.syncIDs[i], normVec, currentModel, now); ue != nil {
					l.cfg.Logger.Warn("embedding.Loop: UpdateEmbedding failed (non-fatal)",
						"sync_id", pb.syncIDs[i],
						"error", ue,
					)
					// Non-fatal: row stays NULL, retried next tick.
				}
			}
		}

		// If a provider error occurred for at least one project, surface it so the
		// caller can apply backoff. (Gated-only skips do NOT surface errors.)
		if tickErr != nil {
			return tickErr
		}

		// Livelock guard: if none of the batch rows were eligible (all gated),
		// terminate this tick. There is no useful work to do until policy changes.
		if !anyEligible {
			return nil
		}
	}
}
