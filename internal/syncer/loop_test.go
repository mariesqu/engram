package syncer_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/syncer"
	"github.com/mariesqu/engram/internal/domain"
)

// ── mock Central ─────────────────────────────────────────────────────────────

// mockCentral is a test-only Central that:
//   - signals syncCh on every PullSince call (so tests can wait for N syncs)
//   - returns the configured error on each call
//   - optionally switches error after N calls (for backoff-recovery tests)
type mockCentral struct {
	mu      sync.Mutex
	syncCh  chan struct{} // buffered; receives one signal per PullSince call
	callN   atomic.Int64 // total calls to PullSince

	// errFn, if set, is called with the call index and returns the error for
	// that call. Overrides staticErr when set.
	errFn func(n int64) error

	// staticErr is returned when errFn is nil.
	staticErr error
}

func newMockCentral(bufSize int) *mockCentral {
	return &mockCentral{syncCh: make(chan struct{}, bufSize)}
}

// Apply is a no-op (unit tests don't push real mutations).
func (m *mockCentral) Apply(_ context.Context, _ domain.Mutation) error {
	return nil
}

// PullSince signals syncCh and returns the configured error.
func (m *mockCentral) PullSince(_ context.Context, _ string, _ int64, _ int) ([]domain.Mutation, error) {
	n := m.callN.Add(1)
	// Non-blocking send: the buffer is sized generously; if it's full the test
	// is already consuming signals, and an extra signal would only cause an
	// extra assertion success — but we don't want to block the loop goroutine.
	select {
	case m.syncCh <- struct{}{}:
	default:
	}
	m.mu.Lock()
	fn := m.errFn
	se := m.staticErr
	m.mu.Unlock()
	if fn != nil {
		return nil, fn(n)
	}
	return nil, se
}

// setStaticErr sets the error returned on every subsequent call.
func (m *mockCentral) setStaticErr(err error) {
	m.mu.Lock()
	m.staticErr = err
	m.errFn = nil
	m.mu.Unlock()
}

// setErrFn sets a per-call error function.
func (m *mockCentral) setErrFn(fn func(n int64) error) {
	m.mu.Lock()
	m.errFn = fn
	m.staticErr = nil
	m.mu.Unlock()
}

// drain discards any buffered sync signals so a subsequent waitN counts only
// syncs that occur after this point (used by the backoff-recovery test to ignore
// signals buffered during the error window).
func (m *mockCentral) drain() {
	for {
		select {
		case <-m.syncCh:
		default:
			return
		}
	}
}

// waitN waits for exactly n sync signals with a generous bounded timeout.
// Returns the number received; < n means timeout.
func (m *mockCentral) waitN(n int, timeout time.Duration) int {
	got := 0
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for got < n {
		select {
		case <-m.syncCh:
			got++
		case <-deadline.C:
			return got
		}
	}
	return got
}

// ── retryabler error ─────────────────────────────────────────────────────────

// retryableErr satisfies the retryabler interface with a configurable return.
type retryableErr struct {
	retryable bool
	msg       string
}

func (e *retryableErr) Error() string    { return e.msg }
func (e *retryableErr) Retryable() bool { return e.retryable }

var (
	errRetryable    = &retryableErr{retryable: true, msg: "server 500"}
	errNonRetryable = &retryableErr{retryable: false, msg: "client 400"}
)

// ── node factory ─────────────────────────────────────────────────────────────

// openNode opens a fresh localstore in t.TempDir and wraps it as a syncer.Node.
func openNode(t *testing.T, name string) *syncer.Node {
	t.Helper()
	dir := t.TempDir()
	st, err := localstore.Open(filepath.Join(dir, name+".db"))
	if err != nil {
		t.Fatalf("openNode %s: %v", name, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return syncer.NewNode(name, st)
}

// ── fast config ──────────────────────────────────────────────────────────────

// fastCfg returns a Config with sub-millisecond durations so tests complete in
// tens of milliseconds.
func fastCfg() syncer.Config {
	return syncer.Config{
		Interval:   5 * time.Millisecond,
		Debounce:   2 * time.Millisecond,
		BackoffMin: 3 * time.Millisecond,
		BackoffMax: 20 * time.Millisecond,
	}
}

// ── Tests ────────────────────────────────────────────────────────────────────

// TestLoop_Periodic: with a short Interval, the loop fires repeatedly.
// We wait for ≥3 signals on syncCh (each PullSince = one sync).
func TestLoop_Periodic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	central := newMockCentral(20)
	node := openNode(t, "periodic")
	l := syncer.NewLoop(node, central, "testproject", fastCfg())
	l.Start(ctx)
	defer l.Stop()

	got := central.waitN(3, 2*time.Second)
	if got < 3 {
		t.Errorf("periodic: wanted ≥3 syncs in 2s, got %d", got)
	}
}

// TestLoop_TriggerDebounce: a burst of Trigger() calls coalesces into ONE sync
// after Debounce. We verify exactly one sync fires for a tight burst.
func TestLoop_TriggerDebounce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := fastCfg()
	cfg.Interval = 10 * time.Second  // long interval → periodic syncs won't interfere
	cfg.Debounce = 5 * time.Millisecond

	central := newMockCentral(20)
	node := openNode(t, "debounce")
	l := syncer.NewLoop(node, central, "testproject", cfg)
	l.Start(ctx)

	// Fire a burst of triggers. With a buffered channel of size 1, only 1 pending
	// signal is queued at most; the rest are coalesced (no-ops).
	for i := 0; i < 10; i++ {
		l.Trigger()
	}

	// Wait for the first sync to fire (the debounced one).
	got := central.waitN(1, 2*time.Second)
	if got < 1 {
		t.Fatal("debounce: no sync fired after Trigger() burst")
	}

	// Allow a bit more time, then assert the sync count is still exactly 1
	// (no periodic sync should fire for 10s).
	time.Sleep(20 * time.Millisecond)

	// Drain any additional signals to count total.
	total := 1
drain:
	for {
		select {
		case <-central.syncCh:
			total++
		default:
			break drain
		}
	}
	if total > 2 {
		// Tolerate 2 to account for one potential timer rearm race on slow CI,
		// but more than that means debounce is broken.
		t.Errorf("debounce: trigger burst resulted in %d syncs; want 1 (±1 for timer race)", total)
	}

	l.Stop()
}

// TestLoop_BackoffRetryable: a retryable error causes increasing wait times,
// so fewer syncs happen in a fixed window compared to the success case.
// On recovery (no error), cadence resumes.
func TestLoop_BackoffRetryable(t *testing.T) {
	cfg := fastCfg()
	cfg.Interval = 3 * time.Millisecond
	cfg.BackoffMin = 30 * time.Millisecond  // noticeable relative to Interval
	cfg.BackoffMax = 200 * time.Millisecond

	// Phase 1: success baseline — count syncs in 30ms.
	{
		ctx, cancel := context.WithCancel(context.Background())
		central := newMockCentral(50)
		node := openNode(t, "backoff-baseline")
		l := syncer.NewLoop(node, central, "testproject", cfg)
		l.Start(ctx)
		time.Sleep(30 * time.Millisecond)
		cancel()
		l.Stop()
		baseline := int(central.callN.Load())
		if baseline < 3 {
			t.Skipf("backoff baseline: only %d syncs in 30ms — environment too slow for this test", baseline)
		}
		t.Logf("baseline syncs in 30ms: %d", baseline)
	}

	// Phase 2: retryable error — count syncs in 30ms (expect far fewer due to
	// BackoffMin=30ms floor).
	{
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		central := newMockCentral(50)
		central.setStaticErr(errRetryable)
		node := openNode(t, "backoff-retry")
		l := syncer.NewLoop(node, central, "testproject", cfg)
		l.Start(ctx)
		time.Sleep(30 * time.Millisecond)
		retryCount := int(central.callN.Load())
		t.Logf("retryable-error syncs in 30ms: %d", retryCount)

		// The first backoff is 30ms, so we expect at most ~1 sync in 30ms after
		// the first failure (the first sync fires immediately, then backoff kicks in).
		if retryCount > 4 {
			t.Errorf("backoff: got %d syncs during backoff window; expected ≤4 (backoff should throttle)", retryCount)
		}

		// Phase 3: clear error → the loop must resume syncing.
		central.setStaticErr(nil)
		central.drain() // discard backoff-window signals so waitN counts only post-recovery syncs
		// waitN counts from now: prove ≥3 fresh syncs land after the error clears.
		if got := central.waitN(3, 2*time.Second); got < 3 {
			t.Errorf("recovery: loop did not resume syncing after error cleared (got %d/3 syncs)", got)
		}
		l.Stop()
	}
}

// TestLoop_NonRetryableNoHotLoop: a non-retryable (4xx) error must not cause
// hot-looping. The loop should respect the Interval even after a non-retryable
// failure.
func TestLoop_NonRetryableNoHotLoop(t *testing.T) {
	cfg := fastCfg()
	cfg.Interval = 8 * time.Millisecond
	cfg.BackoffMin = 5 * time.Second // BackoffMin intentionally large — must NOT apply to non-retryable

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	central := newMockCentral(50)
	central.setStaticErr(errNonRetryable)
	node := openNode(t, "nonretryable")
	l := syncer.NewLoop(node, central, "testproject", cfg)
	l.Start(ctx)

	// Allow 80ms = 10 × Interval. Expect roughly 8-12 syncs (Interval cadence).
	time.Sleep(80 * time.Millisecond)
	cancel()
	l.Stop()

	count := int(central.callN.Load())
	t.Logf("non-retryable syncs in 80ms: %d", count)

	// Hot-loop would give hundreds. Normal cadence gives ~10.
	if count > 30 {
		t.Errorf("non-retryable: %d syncs in 80ms — looks like a hot-loop (want ≤30)", count)
	}
	if count < 3 {
		t.Errorf("non-retryable: only %d syncs in 80ms — loop may have stalled", count)
	}
}

// TestLoop_Stop_BlocksUntilExit: Stop() must not return until the goroutine
// has exited (so it is safe to close the store immediately after).
func TestLoop_Stop_BlocksUntilExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	central := newMockCentral(10)
	node := openNode(t, "stop")
	l := syncer.NewLoop(node, central, "testproject", fastCfg())
	l.Start(ctx)

	// Let at least one sync fire so the goroutine is in its main loop.
	central.waitN(1, 2*time.Second)

	// Stop must return in a bounded time. If it hangs, the test times out.
	done := make(chan struct{})
	go func() {
		l.Stop()
		close(done)
	}()

	select {
	case <-done:
		// good
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() did not return within 3s")
	}
}

// TestLoop_Stop_Idempotent: calling Stop() multiple times must not panic or
// block.
func TestLoop_Stop_Idempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	central := newMockCentral(10)
	node := openNode(t, "stop-idem")
	l := syncer.NewLoop(node, central, "testproject", fastCfg())
	l.Start(ctx)
	central.waitN(1, 2*time.Second)

	// Multiple Stop calls must all return promptly.
	l.Stop()
	l.Stop()
	l.Stop()
}

// TestLoop_Stop_AfterCtxCancel: Stop() after ctx is already cancelled must
// return promptly (does not hang waiting for a goroutine that already exited).
func TestLoop_Stop_AfterCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	central := newMockCentral(10)
	node := openNode(t, "stop-after-cancel")
	l := syncer.NewLoop(node, central, "testproject", fastCfg())
	l.Start(ctx)
	central.waitN(1, 2*time.Second)

	// Cancel first, then Stop.
	cancel()
	done := make(chan struct{})
	go func() {
		l.Stop()
		close(done)
	}()
	select {
	case <-done:
		// good
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() after ctx cancel did not return within 3s")
	}
}

// TestLoop_NoSyncsAfterStop: after Stop() returns, no further syncs must occur
// on the Central.
func TestLoop_NoSyncsAfterStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	central := newMockCentral(20)
	node := openNode(t, "no-after-stop")
	l := syncer.NewLoop(node, central, "testproject", fastCfg())
	l.Start(ctx)

	// Wait for a couple syncs, then Stop.
	central.waitN(2, 2*time.Second)
	l.Stop()

	// Capture count after stop.
	countAtStop := central.callN.Load()

	// Wait a bit longer — if the goroutine is still running it would add more.
	time.Sleep(50 * time.Millisecond)

	countAfter := central.callN.Load()
	if countAfter != countAtStop {
		t.Errorf("syncs continued after Stop(): %d → %d", countAtStop, countAfter)
	}
}

// TestLoop_BackoffIsFloorForTrigger: even when triggered, the loop must NOT
// sync sooner than the active backoff floor (so a failing server is not
// hammered even if the user keeps writing locally).
func TestLoop_BackoffIsFloorForTrigger(t *testing.T) {
	cfg := fastCfg()
	cfg.Interval = 5 * time.Second      // long, won't fire periodically
	cfg.Debounce = 1 * time.Millisecond // very short debounce
	cfg.BackoffMin = 40 * time.Millisecond
	cfg.BackoffMax = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	central := newMockCentral(20)
	// First call returns error to engage backoff; subsequent calls succeed.
	central.setErrFn(func(n int64) error {
		if n == 1 {
			return errRetryable
		}
		return nil
	})

	node := openNode(t, "backoff-floor")
	l := syncer.NewLoop(node, central, "testproject", cfg)
	l.Start(ctx)

	// Trigger the first sync (Interval=5s so we can't wait for periodic).
	l.Trigger()
	// Wait for the first sync (which will fail and engage BackoffMin=40ms).
	if got := central.waitN(1, 2*time.Second); got < 1 {
		t.Fatal("first sync never fired")
	}

	// Immediately fire a burst of triggers. With BackoffMin=40ms, the next sync
	// must not happen for at least 40ms.
	start := time.Now()
	for i := 0; i < 5; i++ {
		l.Trigger()
	}

	// Wait for ONE more sync signal (the debounce-triggered but backoff-floored one).
	if got := central.waitN(1, 2*time.Second); got < 1 {
		t.Fatal("triggered sync never fired after backoff window")
	}
	elapsed := time.Since(start)

	// The second sync must have been delayed by at least BackoffMin (minus a
	// 10ms tolerance for scheduling jitter).
	if elapsed < cfg.BackoffMin-10*time.Millisecond {
		t.Errorf("backoff floor not respected: triggered sync fired %v after triggers; want ≥%v",
			elapsed, cfg.BackoffMin-10*time.Millisecond)
	}
	t.Logf("backoff floor: triggered sync fired %v after triggers (BackoffMin=%v)", elapsed, cfg.BackoffMin)

	l.Stop()
}

// TestLoop_IsRetryable_Classification: verify the duck-typed error classifier
// without importing internal/remote. We use our own retryableErr type which
// mirrors remote.StatusError's interface.
//
// The test works by measuring how long it takes for a Trigger()-ed sync to fire
// after an error. After a retryable error the backoff floor delays the sync;
// after a non-retryable error or nil the loop resumes at normal cadence
// (no backoff) so the Trigger fires in ~Debounce.
func TestLoop_IsRetryable_Classification(t *testing.T) {
	cases := []struct {
		name        string
		firstErr    error
		wantBackoff bool // true → second sync should be delayed by BackoffMin
	}{
		{"retryable_5xx", errRetryable, true},
		{"non-retryable_4xx", errNonRetryable, false},
		{"plain_error", errors.New("network timeout"), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const backoffMin = 40 * time.Millisecond
			cfg := fastCfg()
			cfg.Interval = 10 * time.Second // long — periodic syncs won't interfere
			cfg.Debounce = 1 * time.Millisecond
			cfg.BackoffMin = backoffMin
			cfg.BackoffMax = 200 * time.Millisecond

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// First call returns tc.firstErr; all subsequent return nil.
			central := newMockCentral(10)
			central.setErrFn(func(n int64) error {
				if n == 1 {
					return tc.firstErr
				}
				return nil
			})

			node := openNode(t, "classify-"+tc.name)
			l := syncer.NewLoop(node, central, "testproject", cfg)
			l.Start(ctx)

			// Trigger the first sync (Interval=10s so periodic won't fire in time).
			l.Trigger()
			// Wait for the first sync (will return tc.firstErr).
			if got := central.waitN(1, 2*time.Second); got < 1 {
				t.Fatal("first sync never fired")
			}

			// Now measure how long until Trigger() results in a second sync.
			start := time.Now()
			l.Trigger()
			if got := central.waitN(1, 2*time.Second); got < 1 {
				t.Fatalf("triggered sync never fired")
			}
			elapsed := time.Since(start)

			if tc.wantBackoff {
				// Retryable: backoff floor must delay the triggered sync.
				if elapsed < backoffMin-10*time.Millisecond {
					t.Errorf("%s: expected backoff ≥%v, triggered sync fired in %v",
						tc.name, backoffMin, elapsed)
				}
			} else {
				// Non-retryable: no backoff, Trigger fires in ~Debounce (well under BackoffMin).
				if elapsed > backoffMin {
					t.Errorf("%s: unexpected delay %v — non-retryable should not engage backoff (want < %v)",
						tc.name, elapsed, backoffMin)
				}
			}
			t.Logf("%s: triggered sync after first error in %v (wantBackoff=%v)", tc.name, elapsed, tc.wantBackoff)
			l.Stop()
		})
	}
}

// TestLoop_StopBeforeStart_NoDeadlock proves Stop() is a safe no-op when the Loop
// was never Started — it must return immediately, not block forever on the
// never-closed done channel.
func TestLoop_StopBeforeStart_NoDeadlock(t *testing.T) {
	l := syncer.NewLoop(nil, nil, "proj", syncer.Config{})

	done := make(chan struct{})
	go func() {
		l.Stop() // never Started: must return immediately, not deadlock
		close(done)
	}()

	select {
	case <-done:
		// Stop returned promptly — correct.
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() before Start() deadlocked")
	}
}
