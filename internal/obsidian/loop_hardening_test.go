package obsidian

// loop_hardening_test.go — fixes from the Phase 8 adversarial gate review
// (sdd/obsidian-export-rebuild/verify-report-phase8-loop): CRITICAL-1,
// MAJOR-1, MAJOR-2 (mutations M1, M2, M3, M4, M12), MINOR-1. See loop.go's
// updated doc comments for the corresponding implementation changes.

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"
)

// --- CRITICAL-1: negative Interval must never bypass the floor -------------

// TestApplyLoopDefaultsClampsNegativeInterval proves the fast spin is gone:
// a small negative Interval (which used to be the "no floor, no warning"
// test sentinel) is now clamped to the production floor, with a warning,
// exactly like any other sub-floor value.
func TestApplyLoopDefaultsClampsNegativeInterval(t *testing.T) {
	sb := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(sb, nil))

	got := applyLoopDefaults(LoopConfig{Interval: -1 * time.Nanosecond, Logger: logger})

	if got.Interval != loopIntervalFloor {
		t.Errorf("Interval = %v, want %v (the production floor) for a -1ns input", got.Interval, loopIntervalFloor)
	}
	if !strings.Contains(sb.String(), "interval below the 1-minute floor") {
		t.Errorf("expected a clamp warning to be logged for a negative interval, got: %s", sb.String())
	}
}

// TestApplyLoopDefaultsClampsMinInt64Interval proves the overflow case is
// handled without arithmetic negation: time.Duration(math.MinInt64) negates
// back to itself under two's-complement overflow, which is exactly what made
// the old `c.Interval = -c.Interval` sentinel path spin forever (a negative
// timer duration fires immediately, repeatedly). The fix never negates —
// it only ever compares against the floor and replaces with it.
func TestApplyLoopDefaultsClampsMinInt64Interval(t *testing.T) {
	sb := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(sb, nil))

	got := applyLoopDefaults(LoopConfig{Interval: time.Duration(math.MinInt64), Logger: logger})

	if got.Interval != loopIntervalFloor {
		t.Errorf("Interval = %v, want %v (the production floor) for time.Duration(math.MinInt64)", got.Interval, loopIntervalFloor)
	}
	if got.Interval <= 0 {
		t.Fatalf("Interval = %v is still non-positive: a timer built from this would still spin", got.Interval)
	}
	if !strings.Contains(sb.String(), "interval below the 1-minute floor") {
		t.Errorf("expected a clamp warning to be logged for math.MinInt64, got: %s", sb.String())
	}
}

// TestApplyLoopDefaultsDefaultsZeroToTenMinutes pins the documented
// zero-value default (mutation M3: changing 10m -> 1s must be caught).
func TestApplyLoopDefaultsDefaultsZeroToTenMinutes(t *testing.T) {
	got := applyLoopDefaults(LoopConfig{})
	if got.Interval != 10*time.Minute {
		t.Errorf("Interval = %v, want 10m (the documented zero-value default)", got.Interval)
	}
}

// TestApplyLoopDefaultsClampsPositiveSubMinuteInterval exercises the
// PRODUCTION 1-minute floor (no floorOverride) with a POSITIVE sub-minute
// value. Mutation M2 from the Phase 8 adversarial review: before this test
// existed, no test anywhere passed a positive sub-minute interval, so
// deleting the clamp+warning entirely was undetected by the suite.
func TestApplyLoopDefaultsClampsPositiveSubMinuteInterval(t *testing.T) {
	sb := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(sb, nil))

	got := applyLoopDefaults(LoopConfig{Interval: 30 * time.Second, Logger: logger})

	if got.Interval != time.Minute {
		t.Errorf("Interval = %v, want 1m (REQ-WATCH-04's production floor)", got.Interval)
	}
	line := findLogLine(t, sb.String(), "obsidian.Loop: interval below the 1-minute floor, clamping")
	assertLogField(t, line, "requested", float64(30*time.Second))
	assertLogField(t, line, "clamped_to", float64(time.Minute))
}

// TestApplyLoopDefaultsLeavesNormalIntervalUnchanged is the control case:
// a normal, already-valid interval must pass through unchanged and log no
// warning.
func TestApplyLoopDefaultsLeavesNormalIntervalUnchanged(t *testing.T) {
	sb := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(sb, nil))

	got := applyLoopDefaults(LoopConfig{Interval: 5 * time.Minute, Logger: logger})

	if got.Interval != 5*time.Minute {
		t.Errorf("Interval = %v, want unchanged 5m", got.Interval)
	}
	if strings.Contains(sb.String(), "clamping") {
		t.Errorf("a normal interval must not log a clamp warning, got: %s", sb.String())
	}
}

// --- MAJOR-1: Start(cancelledCtx) must not run a full cycle ----------------

// TestStartWithCancelledContextSkipsFirstCycle — Start called with an
// already-cancelled ctx must not run a cycle at all: a cancelled context
// means the loop is not starting, so REQ-WATCH-03's "run one cycle
// immediately when it starts" does not apply.
func TestStartWithCancelledContextSkipsFirstCycle(t *testing.T) {
	fake := &countingExportable{}
	loop := NewLoop(fake, LoopConfig{Interval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled BEFORE Start is ever called

	loop.Start(ctx)
	loop.Stop()

	if got := fake.cycleCount(); got != 0 {
		t.Errorf("cycleCount = %d after Start with an already-cancelled ctx, want 0 (no cycle should run)", got)
	}
}

// --- MAJOR-2: five previously-undisclosed properties --------------------

// TestRecordFailurePreservesPriorCounters — mutation M1: recordFailure must
// NOT wipe Created/Updated/Deleted/Skipped/Hubs recorded by an earlier
// success. It updates only At/Err/ConsecutiveFailures.
func TestRecordFailurePreservesPriorCounters(t *testing.T) {
	fake := &countingExportable{}
	loop := NewLoop(fake, LoopConfig{Interval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop.Start(ctx)
	waitFor(2*time.Second, func() bool { return fake.cycleCount() >= 1 })
	loop.Stop()

	before := loop.LastResult()
	if before.Created != 1 || before.Updated != 2 || before.Deleted != 3 || before.Skipped != 4 || before.Hubs != 5 {
		t.Fatalf("precondition failed: LastResult() after cycle 1 = %+v, want the counters countingExportable.Export returns", before)
	}

	// Exercise recordFailure directly (unit-level): it must preserve the
	// counters set above.
	loop.recordFailure(errors.New("simulated failure"), 1)

	after := loop.LastResult()
	if after.Created != before.Created || after.Updated != before.Updated ||
		after.Deleted != before.Deleted || after.Skipped != before.Skipped || after.Hubs != before.Hubs {
		t.Errorf("recordFailure wiped prior counters: before=%+v after=%+v", before, after)
	}
	if after.Err != "simulated failure" {
		t.Errorf("after.Err = %q, want %q", after.Err, "simulated failure")
	}
	if after.ConsecutiveFailures != 1 {
		t.Errorf("after.ConsecutiveFailures = %d, want 1", after.ConsecutiveFailures)
	}
}

// TestLoopLogsShutdownMessage — mutation M4 / REQ-WATCH-05: Stop() must be
// preceded by exactly the shutdown log record the run loop emits when ctx is
// cancelled between cycles.
func TestLoopLogsShutdownMessage(t *testing.T) {
	fake := &countingExportable{}
	sb := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(sb, nil))
	loop := NewLoop(fake, LoopConfig{Interval: time.Hour, Logger: logger})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop.Start(ctx)
	waitFor(2*time.Second, func() bool { return fake.cycleCount() >= 1 })
	loop.Stop()

	if !strings.Contains(sb.String(), "obsidian export loop stopped") {
		t.Errorf("expected the REQ-WATCH-05 shutdown log record after Stop(), got: %s", sb.String())
	}
}

// TestRecordSuccessStampsAt — mutation M12: a successful cycle must stamp
// ExportOutcome.At to (approximately) the completion time, not leave it zero.
func TestRecordSuccessStampsAt(t *testing.T) {
	fake := &countingExportable{}
	loop := NewLoop(fake, LoopConfig{Interval: time.Hour})

	before := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop.Start(ctx)
	waitFor(2*time.Second, func() bool { return !loop.LastResult().At.IsZero() })
	loop.Stop()
	after := time.Now().UTC()

	at := loop.LastResult().At
	if at.IsZero() {
		t.Fatal("LastResult().At is zero after a successful cycle, want a stamped time")
	}
	if at.Before(before) || at.After(after) {
		t.Errorf("LastResult().At = %v, want between %v and %v (the cycle's real completion window)", at, before, after)
	}
}

// --- MINOR-1: (nil, nil) from Exportable must not panic --------------------

// nilResultExportable is a contract-violating Exportable: it returns
// (nil, nil) — no error, but also no result. *Exporter never does this
// (verified), but Exportable is exported and any implementation could.
type nilResultExportable struct{}

func (nilResultExportable) Export() (*ExportResult, error) { return nil, nil }
func (nilResultExportable) SetGraphConfig(GraphConfigMode) {}

// TestRunCycleNilExportResultDoesNotPanic — MINOR-1: before the fix, this
// panics INSIDE the loop's own background goroutine when recordSuccess
// dereferences a nil *ExportResult. An unrecovered panic there is not
// something the test itself can recover (it happens on a different
// goroutine) — it crashes the whole test binary, which is exactly the
// "kills the daemon process" failure mode described in the review. The
// fixed behaviour instead treats it as a recorded, logged failure.
func TestRunCycleNilExportResultDoesNotPanic(t *testing.T) {
	loop := NewLoop(nilResultExportable{}, LoopConfig{Interval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop.Start(ctx)
	waitFor(2*time.Second, func() bool { return loop.LastResult().Err != "" })
	loop.Stop()

	last := loop.LastResult()
	if last.Err == "" {
		t.Fatalf("LastResult().Err is empty after Export returned (nil, nil); want it recorded as a failure instead of panicking")
	}
	if last.ConsecutiveFailures != 1 {
		t.Errorf("LastResult().ConsecutiveFailures = %d, want 1", last.ConsecutiveFailures)
	}
}
