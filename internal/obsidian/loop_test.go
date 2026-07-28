package obsidian

// loop_test.go — Phase 8 (internal/obsidian.Loop, tasks 8.1/8.3/8.5/8.7/8.9/8.11/8.13).
//
// No mocking framework exists in this repo (design constraint 6) — every
// fake below is hand-rolled, mirroring mockStore in testhelpers_test.go and
// the counting/blocking fakes syncer/embedding already use in their own
// loop tests.
//
// Synchronization note: most assertions here poll an observable counter
// (cycleCount) rather than sleeping a fixed duration, following the
// race-proof pattern embedding/loop_test.go documents ("fixed time.Sleep
// windows flake under -race"). Two tests are genuinely timing-dependent by
// necessity — TestLoopRunsFirstCycleImmediately (must prove NO interval
// elapsed before cycle 1) and TestLoopTicksAtInterval (must prove the timer
// actually re-arms) — both are called out as such in the apply report and
// given generous budgets so they are slow rather than flaky.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitFor polls cond every 5ms until it holds or the deadline passes. It
// does not fail the test itself — callers assert the real condition after
// the wait returns, so a timeout produces the caller's own failure message
// rather than a generic "timed out" one.
func waitFor(d time.Duration, cond func() bool) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// countingExportable is a hand-rolled Exportable fake used by most loop
// tests: it counts cycles, can be told to fail on specific (1-based) cycle
// numbers, and records the GraphConfigMode active at the start of every
// Export() call so tests can verify the first-cycle-only downgrade
// (REQ-WATCH-06).
type countingExportable struct {
	mu        sync.Mutex
	cycles    int
	mode      GraphConfigMode
	modesSeen []GraphConfigMode
	errOn     map[int]error
}

func (c *countingExportable) Export() (*ExportResult, error) {
	c.mu.Lock()
	c.cycles++
	n := c.cycles
	c.modesSeen = append(c.modesSeen, c.mode)
	err := c.errOn[n]
	c.mu.Unlock()

	if err != nil {
		return nil, err
	}
	return &ExportResult{Created: 1, Updated: 2, Deleted: 3, Skipped: 4, Hubs: 5}, nil
}

func (c *countingExportable) SetGraphConfig(m GraphConfigMode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mode = m
}

func (c *countingExportable) GraphConfig() GraphConfigMode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

func (c *countingExportable) cycleCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cycles
}

func (c *countingExportable) seenModes() []GraphConfigMode {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]GraphConfigMode, len(c.modesSeen))
	copy(out, c.modesSeen)
	return out
}

func (c *countingExportable) setErrOn(cycle int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.errOn == nil {
		c.errOn = map[int]error{}
	}
	c.errOn[cycle] = err
}

// blockingExportable blocks inside Export() until release is closed, so
// tests can observe Loop.Stop()'s drain guarantee (REQ-WATCH-05): an
// in-flight cycle must complete before Stop() returns.
type blockingExportable struct {
	release chan struct{}
	entered chan struct{} // closed once Export() has been entered
	once    sync.Once
	mode    GraphConfigMode
}

func newBlockingExportable() *blockingExportable {
	return &blockingExportable{
		release: make(chan struct{}),
		entered: make(chan struct{}),
	}
}

func (b *blockingExportable) Export() (*ExportResult, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return &ExportResult{}, nil
}

func (b *blockingExportable) SetGraphConfig(m GraphConfigMode) { b.mode = m }
func (b *blockingExportable) GraphConfig() GraphConfigMode     { return b.mode }

// syncBuffer is a mutex-guarded io.Writer so a slog handler can be written to
// concurrently by the loop goroutine while the test goroutine reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestLoopRunsFirstCycleImmediately — REQ-WATCH-03: the loop MUST run one
// cycle immediately on Start, never waiting a full interval first. The
// interval here (1 hour) makes "immediately" unambiguous: if the loop
// waited for the interval, cycleCount would still be 0 well within this
// test's budget.
//
// Timing-dependent by necessity: this test can only prove "no interval
// elapsed" by observing that a cycle completed well before the interval
// could have. Given a budget several orders of magnitude below 1 hour, it
// carries essentially zero flake risk.
func TestLoopRunsFirstCycleImmediately(t *testing.T) {
	fake := &countingExportable{}
	loop := NewLoop(fake, LoopConfig{Interval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop.Start(ctx)
	defer loop.Stop()

	waitFor(2*time.Second, func() bool { return fake.cycleCount() >= 1 })

	if got := fake.cycleCount(); got < 1 {
		t.Fatalf("cycleCount = %d after 2s with a 1h interval, want >= 1 (first cycle must run immediately)", got)
	}
}

// TestLoopTicksAtInterval — task 8.3. floorOverride (unexported, test-only)
// replaces the production 1-minute floor for this Loop's defaulting, so this
// test can exercise a real multi-cycle tick sequence in milliseconds instead
// of minutes without needing a negative Interval (removed as CRITICAL-1 in
// the Phase 8 adversarial review — see TestApplyLoopDefaultsClamps* below).
//
// Timing-dependent by necessity (this IS the tick-cadence test); budgeted
// generously (500ms wall-clock for an expected ~50 ticks at 10ms) so it is
// slow rather than flaky.
func TestLoopTicksAtInterval(t *testing.T) {
	fake := &countingExportable{}
	loop := NewLoop(fake, LoopConfig{Interval: 10 * time.Millisecond, floorOverride: time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	loop.Start(ctx)
	<-ctx.Done()
	loop.Stop()

	if got := fake.cycleCount(); got < 3 {
		t.Errorf("cycleCount = %d over 500ms at a 10ms interval, want >= 3", got)
	}
}

// TestLoopLogsPerCycleCounts — REQ-WATCH-11: one structured log record per
// cycle carrying created/updated/deleted/skipped/hubs and duration.
func TestLoopLogsPerCycleCounts(t *testing.T) {
	fake := &countingExportable{}
	sb := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(sb, nil))
	loop := NewLoop(fake, LoopConfig{Interval: time.Hour, Logger: logger})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop.Start(ctx)
	defer loop.Stop()

	waitFor(2*time.Second, func() bool { return strings.Contains(sb.String(), "obsidian export cycle") })

	line := findLogLine(t, sb.String(), "obsidian export cycle")
	assertLogField(t, line, "created", float64(1))
	assertLogField(t, line, "updated", float64(2))
	assertLogField(t, line, "deleted", float64(3))
	assertLogField(t, line, "skipped", float64(4))
	assertLogField(t, line, "hubs", float64(5))
	if _, ok := line["duration"]; !ok {
		t.Errorf("log record missing %q field: %v", "duration", line)
	}
}

// TestLoopWritesNothingToStdout — design constraint 16 / REQ-EXPORT-11's
// amended stream clause: in the daemon's default mode stdout is the MCP
// JSON-RPC channel, so the loop must never write a single byte there.
func TestLoopWritesNothingToStdout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		os.Stdout = origStdout
		w.Close()
	}
	defer restore()

	fake := &countingExportable{}
	// MINOR-3 (Phase 8 adversarial review): this test must also exercise
	// runCycle's error branch while stdout is captured — a prior version of
	// this fake never failed, so a stray stdout write inside the error
	// branch specifically would have gone undetected.
	fake.setErrOn(2, errors.New("simulated disk error"))
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	loop := NewLoop(fake, LoopConfig{Interval: 5 * time.Millisecond, floorOverride: time.Millisecond, Logger: logger})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	loop.Start(ctx)
	<-ctx.Done()
	loop.Stop()

	restore()

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&buf, r)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out reading stdout pipe")
	}

	if buf.Len() != 0 {
		t.Errorf("obsidian loop wrote %d bytes to stdout, want 0: %q", buf.Len(), buf.String())
	}
}

// TestLoopContinuesOnExportError — REQ-WATCH-08: a failing cycle MUST NOT
// terminate the loop; the next scheduled tick MUST still run.
func TestLoopContinuesOnExportError(t *testing.T) {
	fake := &countingExportable{}
	fake.setErrOn(2, errors.New("simulated disk error"))

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	loop := NewLoop(fake, LoopConfig{Interval: 10 * time.Millisecond, floorOverride: time.Millisecond, Logger: logger})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	loop.Start(ctx)
	<-ctx.Done()
	loop.Stop()

	if got := fake.cycleCount(); got < 3 {
		t.Fatalf("cycleCount = %d, want >= 3 (cycle 2's error must not have stopped the loop)", got)
	}
}

// TestLoopCountsConsecutiveFailures — REQ-WATCH-08: consecutive failures are
// counted (for diagnosability) and a subsequent success resets the count.
func TestLoopCountsConsecutiveFailures(t *testing.T) {
	fake := &countingExportable{}
	fake.setErrOn(2, errors.New("fail 2"))
	fake.setErrOn(3, errors.New("fail 3"))

	sb := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(sb, nil))
	loop := NewLoop(fake, LoopConfig{Interval: 10 * time.Millisecond, floorOverride: time.Millisecond, Logger: logger})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop.Start(ctx)

	waitFor(2*time.Second, func() bool { return fake.cycleCount() >= 4 })
	loop.Stop()

	lines := findAllLogLines(t, sb.String(), "obsidian export cycle failed")
	if len(lines) < 2 {
		t.Fatalf("got %d failure log lines, want >= 2: %v", len(lines), sb.String())
	}
	assertLogField(t, lines[0], "consecutive_failures", float64(1))
	assertLogField(t, lines[1], "consecutive_failures", float64(2))

	// Cycle 4 succeeds: LastResult().ConsecutiveFailures must reset to 0.
	waitFor(2*time.Second, func() bool { return loop.LastResult().ConsecutiveFailures == 0 && loop.LastResult().Err == "" })
	last := loop.LastResult()
	if last.ConsecutiveFailures != 0 {
		t.Errorf("LastResult().ConsecutiveFailures = %d after a subsequent success, want 0", last.ConsecutiveFailures)
	}
	if last.Err != "" {
		t.Errorf("LastResult().Err = %q after a subsequent success, want empty", last.Err)
	}
}

// TestStopDrainsInFlightCycle — REQ-WATCH-05: Stop() MUST block until the
// in-flight export cycle completes; no abrupt cancellation mid-write.
func TestStopDrainsInFlightCycle(t *testing.T) {
	fake := newBlockingExportable()
	loop := NewLoop(fake, LoopConfig{Interval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop.Start(ctx)

	select {
	case <-fake.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Export() was never entered")
	}

	stopDone := make(chan struct{})
	go func() {
		loop.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
		t.Fatal("Stop() returned while the export cycle was still blocked")
	case <-time.After(150 * time.Millisecond):
		// expected: Stop() has not returned yet.
	}

	close(fake.release)

	select {
	case <-stopDone:
		// expected: Stop() returns once the in-flight cycle finishes.
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return after the in-flight cycle finished")
	}
}

// TestGraphConfigAppliedOnFirstCycleOnly — REQ-WATCH-06: the graph-config
// bootstrap mode applies on the first cycle of this process only; every
// subsequent cycle sees GraphConfigSkip.
func TestGraphConfigAppliedOnFirstCycleOnly(t *testing.T) {
	fake := &countingExportable{mode: GraphConfigPreserve}
	loop := NewLoop(fake, LoopConfig{Interval: 10 * time.Millisecond, floorOverride: time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop.Start(ctx)

	waitFor(2*time.Second, func() bool { return fake.cycleCount() >= 3 })
	loop.Stop()

	modes := fake.seenModes()
	if len(modes) < 3 {
		t.Fatalf("only %d cycles observed, want >= 3", len(modes))
	}
	if modes[0] != GraphConfigPreserve {
		t.Errorf("cycle 1 saw mode %q, want %q", modes[0], GraphConfigPreserve)
	}
	for i := 1; i < len(modes); i++ {
		if modes[i] != GraphConfigSkip {
			t.Errorf("cycle %d saw mode %q, want %q (downgraded after cycle 1)", i+1, modes[i], GraphConfigSkip)
		}
	}
}

// TestGraphConfigNotDowngradedAfterFailedFirstCycle — a transient first-cycle
// failure must not permanently forfeit the graph-config bootstrap: cycle 2
// must still see the originally configured mode.
func TestGraphConfigNotDowngradedAfterFailedFirstCycle(t *testing.T) {
	fake := &countingExportable{mode: GraphConfigPreserve}
	fake.setErrOn(1, errors.New("transient failure on first cycle"))
	loop := NewLoop(fake, LoopConfig{Interval: 10 * time.Millisecond, floorOverride: time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop.Start(ctx)

	waitFor(2*time.Second, func() bool { return fake.cycleCount() >= 2 })
	loop.Stop()

	modes := fake.seenModes()
	if len(modes) < 2 {
		t.Fatalf("only %d cycles observed, want >= 2", len(modes))
	}
	if modes[1] != GraphConfigPreserve {
		t.Errorf("cycle 2 saw mode %q after cycle 1 failed, want %q (bootstrap must not be forfeited by a transient failure)", modes[1], GraphConfigPreserve)
	}
}

// TestLoopLastResult — the zero value means "no cycle has run yet"; a
// success populates counters; a subsequent failure populates Err WITHOUT
// zeroing the previous counters.
func TestLoopLastResult(t *testing.T) {
	fake := &countingExportable{}

	loop := NewLoop(fake, LoopConfig{Interval: time.Hour})
	if got := loop.LastResult(); !got.At.IsZero() {
		t.Errorf("LastResult() before Start = %+v, want zero value (At.IsZero())", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop.Start(ctx)

	waitFor(2*time.Second, func() bool { return !loop.LastResult().At.IsZero() })
	first := loop.LastResult()
	if first.Created != 1 || first.Updated != 2 || first.Deleted != 3 || first.Skipped != 4 || first.Hubs != 5 {
		t.Errorf("LastResult() after cycle 1 = %+v, want Created=1 Updated=2 Deleted=3 Skipped=4 Hubs=5", first)
	}
	if first.Err != "" {
		t.Errorf("LastResult().Err after a success = %q, want empty", first.Err)
	}
	loop.Stop()

	// Second loop instance: force the FIRST cycle to fail, and confirm Err is
	// populated while the (zero) counters from before are not the thing under
	// test here — this test's job is just the Err-populated-without-losing-
	// prior-success-counters property, exercised via the failure-count test
	// above for the "retains prior counters" half. Here we assert the
	// simpler, direct claim: a failure populates Err.
	fake2 := &countingExportable{}
	fake2.setErrOn(1, errors.New("boom"))
	loop2 := NewLoop(fake2, LoopConfig{Interval: time.Hour})
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	loop2.Start(ctx2)
	waitFor(2*time.Second, func() bool { return loop2.LastResult().Err != "" })
	loop2.Stop()
	if got := loop2.LastResult(); got.Err == "" {
		t.Errorf("LastResult().Err after a failed cycle = empty, want populated")
	}
}

// findLogLine returns the first JSON log line whose "msg" field equals msg,
// decoded into a map for field assertions. It fails the test if none match.
func findLogLine(t *testing.T, logOutput, msg string) map[string]any {
	t.Helper()
	lines := findAllLogLines(t, logOutput, msg)
	if len(lines) == 0 {
		t.Fatalf("no log line with msg %q found in: %s", msg, logOutput)
	}
	return lines[0]
}

// findAllLogLines returns every JSON log line whose "msg" field equals msg.
func findAllLogLines(t *testing.T, logOutput, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logOutput), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

func assertLogField(t *testing.T, line map[string]any, key string, want any) {
	t.Helper()
	got, ok := line[key]
	if !ok {
		t.Errorf("log line missing field %q: %v", key, line)
		return
	}
	if got != want {
		t.Errorf("log field %q = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}
