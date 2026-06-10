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
	// BatchPause: the zero value gets the 1s SPEC DEFAULT — a zero-value
	// LoopConfig must be rate-limit-safe by construction, not by advice.
	// Tests that genuinely want no pause pass a NEGATIVE value.
	if c.BatchPause == 0 {
		c.BatchPause = 1 * time.Second
	}
	if c.BatchPause < 0 {
		c.BatchPause = 0
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
// Livelock + starvation safety: permanently-gated rows (omitted/local-only
// projects) have embedding=NULL and ARE matched by the predicate forever. The
// tick pages with a KEYSET CURSOR (id > afterID, ORDER BY id) that advances
// past every page unconditionally — gated rows are skipped in-memory but can
// neither make a page repeat (livelock) nor occupy every page slot ahead of
// eligible rows (starvation). The tick terminates when the cursor exhausts
// the predicate (SelectEmbeddable returns zero rows).
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
	// Keyset cursor: strictly advances every page, so gated rows can neither
	// livelock the tick nor starve eligible rows behind them.
	var afterID int64
	// TICK-scoped (not page-scoped) failure tracking: a success on page 1
	// followed by a provider failure on page 2 is a PARTIAL tick — it must not
	// trigger backoff (spec: batch failure surfaces no error from the tick).
	anySucceeded := false

	for {
		if ctx.Err() != nil {
			return nil // cancelled — not a provider error
		}

		rows, err := localstore.SelectEmbeddable(db, currentModel, batchSize, afterID)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil // cursor exhausted — every eligible row this tick was visited
		}
		afterID = rows[len(rows)-1].ID // advance the cursor PAST this page

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

		var pageErr error

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
				pageErr = embedErr
				continue
			}

			// Provider succeeded for this project group.
			anySucceeded = true

			// Write each normalised vector back via UpdateEmbeddingStale (the
			// model-mismatch variant — 0 rows affected when a concurrent embedder
			// already stamped the current model; a concurrent CONTENT edit resets
			// the embedding to NULL in execUpdate, re-enqueuing the row).
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

		if pageErr != nil {
			if anySucceeded {
				// PARTIAL tick failure (this page or an earlier one succeeded
				// somewhere): stop the tick WITHOUT error — backoff would punish
				// the healthy projects for one transient 429/5xx. Failed rows
				// stay NULL and retry at the normal cadence next tick (spec:
				// batch failure surfaces no error from the tick).
				l.cfg.Logger.Warn("embedding.Loop: partial tick failure — failed rows retry next tick",
					"error", pageErr,
				)
				return nil
			}
			// TOTAL failure (zero successes anywhere this tick): surface it so
			// the caller backs off — the provider is likely down and paging on
			// would hammer it for nothing.
			return pageErr
		}
		// Gated-only pages fall through: the cursor advance above is the
		// termination guarantee — the next page is always NEW rows.
	}
}
