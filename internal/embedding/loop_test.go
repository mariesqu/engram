package embedding_test

// loop_test.go — task 1b.1
//
// Unit tests for the embedding backfill Loop. Uses a real temp SQLite store
// (via localstore.Open) and a recordingMockProvider as the inner provider of
// a gated EmbeddingProvider. No real API key required.
//
// The recording-mock is wired as the 'inner' of NewGated so the gate is real —
// exactly the pattern mandated by the spec ("real-gate testing pattern is BINDING").

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
)

// ── Test helpers ─────────────────────────────────────────────────────────────

// openLoopStore opens a temp SQLite store for loop tests.
func openLoopStore(t *testing.T) *localstore.Store {
	t.Helper()
	st, err := localstore.Open(filepath.Join(t.TempDir(), "loop_test.db"))
	if err != nil {
		t.Fatalf("localstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// insertRow inserts a bare memory row into the store with embedding=NULL.
func insertRow(t *testing.T, st *localstore.Store, syncID, project, title string) {
	t.Helper()
	_, err := st.DB().Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content, project, scope,
		   version, writer_id, last_write_mutation_id, created_at, updated_at)
		VALUES (?, 'sess1', 'memory', 'manual', ?, '', ?, 'project',
		        1, 'w1', 'mut-'||?, datetime('now'), datetime('now'))`,
		syncID, title, project, syncID)
	if err != nil {
		t.Fatalf("insertRow %q: %v", syncID, err)
	}
}

// hasEmbedding reports whether the row with the given sync_id has a non-NULL embedding.
func hasEmbedding(t *testing.T, st *localstore.Store, syncID string) bool {
	t.Helper()
	var count int
	err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE sync_id=? AND embedding IS NOT NULL`, syncID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("hasEmbedding %q: %v", syncID, err)
	}
	return count > 0
}

// embeddingModel returns the embedding_model value for a row, or "" if NULL.
func embeddingModel(t *testing.T, st *localstore.Store, syncID string) string {
	t.Helper()
	var model string
	err := st.DB().QueryRow(
		`SELECT COALESCE(embedding_model, '') FROM memories WHERE sync_id=?`, syncID,
	).Scan(&model)
	if err != nil {
		t.Fatalf("embeddingModel %q: %v", syncID, err)
	}
	return model
}

// newSyncedPolicyChecker returns a staticPolicyChecker with all listed projects
// set to PolicySynced and a fallback to PolicyLocalOnly for unknown projects.
func newSyncedPolicyChecker(projects ...string) *staticPolicyChecker {
	policies := make(map[string]localstore.Policy, len(projects))
	for _, p := range projects {
		policies[p] = localstore.PolicySynced
	}
	return &staticPolicyChecker{policies: policies}
}

// ── Race-proof synchronization helpers ───────────────────────────────────────
//
// Fixed time.Sleep windows flake under `go test -race` (2-10x slowdown): the
// triggered tick may not have finished — or started — inside the window. These
// helpers poll the OUTCOME instead of guessing a duration.

// waitFor polls cond every 10ms until it holds or the deadline passes.
// It does NOT fail the test on timeout — the caller's assertions report the
// actual mismatch with their own messages.
func waitFor(d time.Duration, cond func() bool) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitQuiescent polls until the mock's observable state (callCount+totalTexts)
// has been STABLE for `stable`, capped at `max`. Used where the expected
// outcome is "nothing (more) happens" — a condition that cannot be polled
// positively. The stability window comfortably exceeds debounce + scheduling
// latency even under the race detector.
func waitQuiescent(mock *recordingMockProvider, stable, max time.Duration) {
	deadline := time.Now().Add(max)
	lastCalls, lastTexts := mock.callCount(), mock.totalTexts()
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		c, x := mock.callCount(), mock.totalTexts()
		if c != lastCalls || x != lastTexts {
			lastCalls, lastTexts = c, x
			stableSince = time.Now()
			continue
		}
		if time.Since(stableSince) >= stable {
			return
		}
	}
}

// runLoopOnce constructs a loop with fast-forward config (no interval wait,
// tiny batch), runs one tick synchronously, and returns the mock.
// It uses the REAL gate (NewGated with the mock as inner).
func runLoopOnce(t *testing.T, st *localstore.Store, mock *recordingMockProvider, checker embedding.PolicyChecker, batchSize int) {
	t.Helper()
	gated := embedding.NewGated(mock, checker, true /* remote */)
	loop := embedding.NewLoop(st, gated, embedding.LoopConfig{
		Interval:   100 * time.Millisecond,
		Debounce:   1 * time.Millisecond,
		BatchPause: -1, // negative = explicitly no pause (0 now defaults to the 1s spec value)
		BatchSize:  batchSize,
	})
	// runTick is private, so we start the loop and immediately trigger + wait.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	loop.Start(ctx)
	loop.Trigger()
	// Quiescence, not a fixed sleep: callers include no-op runs (zero expected
	// calls), so we wait until the mock has been stable for a window that
	// dwarfs debounce + scheduling latency even under -race.
	waitQuiescent(mock, 600*time.Millisecond, 8*time.Second)
	loop.Stop()
}

// ── Tests ────────────────────────────────────────────────────────────────────

// TestLoop_Idempotency_TwoRuns_NoDuplicates (spec: idempotency scenario)
// Running the loop twice on 5 rows must embed each exactly once.
// The second run must make ZERO provider calls.
func TestLoop_Idempotency_TwoRuns_NoDuplicates(t *testing.T) {
	st := openLoopStore(t)

	for i := 0; i < 5; i++ {
		syncID := "row-" + string(rune('A'+i))
		insertRow(t, st, syncID, "open", "title "+syncID)
	}

	mock := newRecordingMock(4)
	checker := newSyncedPolicyChecker("open")

	// First run: must embed all 5 rows.
	runLoopOnce(t, st, mock, checker, 100)
	if mock.totalTexts() != 5 {
		t.Errorf("run-1: provider received %d texts, want 5", mock.totalTexts())
	}

	// Second run: all rows already embedded; provider must receive ZERO calls.
	callsBefore := mock.callCount()
	runLoopOnce(t, st, mock, checker, 100)
	if mock.callCount() != callsBefore {
		t.Errorf("run-2: provider was called %d extra times, want 0", mock.callCount()-callsBefore)
	}
}

// TestLoop_NoOp_WhenAllEmbedded (spec: no-op scenario)
func TestLoop_NoOp_WhenAllEmbedded(t *testing.T) {
	st := openLoopStore(t)

	// Pre-embed all rows using UpdateEmbedding so the BLOB is valid.
	preVec := localstore.L2Normalize([]float32{1, 0, 0, 0})
	for i := 0; i < 3; i++ {
		syncID := "pre-" + string(rune('0'+i))
		insertRow(t, st, syncID, "open", "title")
		// model = "mock" matches the recordingMockProvider.ModelName() so loop skips.
		if err := localstore.UpdateEmbedding(st.DB(), syncID, preVec, "mock", "2024-01-01T00:00:00Z"); err != nil {
			t.Fatalf("pre-embed %q: %v", syncID, err)
		}
	}

	mock := newRecordingMock(4)
	checker := newSyncedPolicyChecker("open")
	runLoopOnce(t, st, mock, checker, 100)

	if mock.callCount() != 0 {
		t.Errorf("provider called %d times for pre-embedded rows, want 0", mock.callCount())
	}
}

// TestLoop_ModelChange_ReEmbedsStale (spec: model-change scenario)
func TestLoop_ModelChange_ReEmbedsStale(t *testing.T) {
	st := openLoopStore(t)

	// Insert 3 rows with "old-model" embedding using UpdateEmbedding directly.
	// This produces a valid BLOB of the correct format.
	oldVec := localstore.L2Normalize([]float32{1, 0, 0, 0})
	for i := 0; i < 3; i++ {
		syncID := "stale-" + string(rune('A'+i))
		insertRow(t, st, syncID, "open", "title")
		if err := localstore.UpdateEmbedding(st.DB(), syncID, oldVec, "old-model", "2024-01-01T00:00:00Z"); err != nil {
			t.Fatalf("set old embedding %q: %v", syncID, err)
		}
	}

	// Mock with ModelName() = "mock" (not "old-model") → rows are stale → re-embed.
	mock := newRecordingMock(4)
	checker := newSyncedPolicyChecker("open")
	runLoopOnce(t, st, mock, checker, 100)

	if mock.totalTexts() != 3 {
		t.Errorf("provider received %d texts, want 3 (stale rows)", mock.totalTexts())
	}
	// Verify the rows now have embedding_model = "mock".
	for i := 0; i < 3; i++ {
		syncID := "stale-" + string(rune('A'+i))
		if got := embeddingModel(t, st, syncID); got != "mock" {
			t.Errorf("row %q: embedding_model = %q, want mock", syncID, got)
		}
	}
}

// TestLoop_BatchSize_Respected (spec: batch-size scenario)
// 250 eligible rows with batchSize=100 should produce 3 Embed calls (100+100+50).
func TestLoop_BatchSize_Respected(t *testing.T) {
	st := openLoopStore(t)

	for i := 0; i < 250; i++ {
		syncID := "batch-" + string([]byte{byte('A' + i/26), byte('A' + i%26)})
		insertRow(t, st, syncID, "open", "title")
	}

	mock := newRecordingMock(4)
	checker := newSyncedPolicyChecker("open")

	gated := embedding.NewGated(mock, checker, true)
	loop := embedding.NewLoop(st, gated, embedding.LoopConfig{
		Interval:   100 * time.Millisecond,
		Debounce:   1 * time.Millisecond,
		BatchPause: -1,
		BatchSize:  100,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Start(ctx)
	loop.Trigger()
	waitFor(10*time.Second, func() bool { return mock.totalTexts() >= 250 })
	loop.Stop()

	// All 250 texts embedded via 3 calls: 100+100+50.
	if mock.callCount() != 3 {
		t.Errorf("embed call count = %d, want 3 (100+100+50)", mock.callCount())
	}
	if mock.totalTexts() != 250 {
		t.Errorf("total texts embedded = %d, want 250", mock.totalTexts())
	}
}

// TestLoop_BatchFailure_ContinuesRemainingBatches (spec: batch-failure scenario)
// 6 rows in batches of 3: call 1 succeeds, call 2 fails.
// Rows 1-3 should be embedded; rows 4-6 remain NULL.
func TestLoop_BatchFailure_ContinuesRemainingBatches(t *testing.T) {
	st := openLoopStore(t)

	for i := 0; i < 6; i++ {
		syncID := "fail-" + string(rune('A'+i))
		insertRow(t, st, syncID, "open", "title "+string(rune('A'+i)))
	}

	// Deterministic failure injection: the SECOND Embed call (and later) fail.
	// Configured before the loop starts — no goroutine, no timing race.
	errMock := newRecordingMock(4)
	errMock.failOnCall(2, errors.New("provider error on call 2"))

	checker := newSyncedPolicyChecker("open")
	gated := embedding.NewGated(errMock, checker, true)
	loop := embedding.NewLoop(st, gated, embedding.LoopConfig{
		Interval:   100 * time.Millisecond,
		Debounce:   1 * time.Millisecond,
		BatchPause: -1,
		BatchSize:  3,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	loop.Start(ctx)
	loop.Trigger()
	waitFor(8*time.Second, func() bool { return errMock.callCount() >= 2 })
	loop.Stop() // Stop blocks until the goroutine exits — the real barrier

	// Exactly: call 1 (rows A-C) succeeded, call 2 (rows D-F) failed.
	if errMock.callCount() < 2 {
		t.Fatalf("provider called %d times, want >= 2", errMock.callCount())
	}
	embedded := 0
	for i := 0; i < 6; i++ {
		if hasEmbedding(t, st, "fail-"+string(rune('A'+i))) {
			embedded++
		}
	}
	if embedded != 3 {
		t.Errorf("embedded = %d rows, want exactly 3 (first batch only — second failed)", embedded)
	}
}

// TestLoop_GatedRowsDoNotStarveEligible (the keyset-cursor regression proof):
// 2×BatchSize OMITTED rows are inserted FIRST (lowest ids), then synced rows
// behind them. Without the id-cursor, every page would return the same gated
// rows, the tick would end "caught up", and the eligible rows would never be
// reached. With the cursor the tick pages PAST the gated block.
func TestLoop_GatedRowsDoNotStarveEligible(t *testing.T) {
	st := openLoopStore(t)

	for i := 0; i < 6; i++ { // 2×BatchSize=3 gated rows first (low ids)
		insertRow(t, st, fmt.Sprintf("gated-%02d", i), "private", "secret text")
	}
	for i := 0; i < 3; i++ { // eligible rows BEHIND the gated block
		insertRow(t, st, fmt.Sprintf("open-%02d", i), "open", "public text")
	}

	mock := newRecordingMock(4)
	checker := &staticPolicyChecker{policies: map[string]localstore.Policy{
		"private": localstore.PolicyOmitted,
		"open":    localstore.PolicySynced,
	}}
	gated := embedding.NewGated(mock, checker, true)
	loop := embedding.NewLoop(st, gated, embedding.LoopConfig{
		Interval:   time.Hour, // single triggered tick
		Debounce:   time.Millisecond,
		BatchPause: -1,
		BatchSize:  3,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	loop.Start(ctx)
	loop.Trigger()
	waitFor(8*time.Second, func() bool { return mock.totalTexts() >= 3 })
	loop.Stop()

	for i := 0; i < 3; i++ {
		if !hasEmbedding(t, st, fmt.Sprintf("open-%02d", i)) {
			t.Errorf("open-%02d not embedded — gated rows starved the eligible ones (cursor regression)", i)
		}
	}
	for i := 0; i < 6; i++ {
		if hasEmbedding(t, st, fmt.Sprintf("gated-%02d", i)) {
			t.Errorf("gated-%02d was embedded — PRIVACY VIOLATION", i)
		}
	}
	for _, texts := range mock.receivedTexts() {
		for _, txt := range texts {
			if txt == "secret text" || len(txt) >= 6 && txt[:6] == "secret" {
				t.Error("PRIVACY VIOLATION: gated text reached the provider")
			}
		}
	}
}

// TestLoop_Resume_AfterInterrupt (spec: resumability scenario)
// Cancel the context mid-backfill, then re-run — remaining rows are embedded.
func TestLoop_Resume_AfterInterrupt(t *testing.T) {
	st := openLoopStore(t)

	for i := 0; i < 10; i++ {
		syncID := "resume-" + string(rune('A'+i))
		insertRow(t, st, syncID, "open", "title")
	}

	mock := newRecordingMock(4)
	checker := newSyncedPolicyChecker("open")

	// First run: cancel after processing 4 rows (batch=4, 1 batch, then cancel).
	gated := embedding.NewGated(mock, checker, true)
	loop := embedding.NewLoop(st, gated, embedding.LoopConfig{
		Interval:   100 * time.Millisecond,
		Debounce:   1 * time.Millisecond,
		BatchPause: -1,
		BatchSize:  4,
	})
	ctx1, cancel1 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel1()
	loop.Start(ctx1)
	loop.Trigger()
	<-ctx1.Done()
	loop.Stop()

	// Count how many were embedded in the first run.
	embeddedAfterRun1 := 0
	for i := 0; i < 10; i++ {
		if hasEmbedding(t, st, "resume-"+string(rune('A'+i))) {
			embeddedAfterRun1++
		}
	}

	// Second run: must embed remaining rows; already-embedded ones untouched.
	mock2 := newRecordingMock(4)
	gated2 := embedding.NewGated(mock2, checker, true)
	loop2 := embedding.NewLoop(st, gated2, embedding.LoopConfig{
		Interval:   100 * time.Millisecond,
		Debounce:   1 * time.Millisecond,
		BatchPause: -1,
		BatchSize:  100,
	})
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	loop2.Start(ctx2)
	loop2.Trigger()
	waitQuiescent(mock2, 600*time.Millisecond, 8*time.Second)
	loop2.Stop()

	// All 10 rows must be embedded after 2 runs.
	for i := 0; i < 10; i++ {
		syncID := "resume-" + string(rune('A'+i))
		if !hasEmbedding(t, st, syncID) {
			t.Errorf("row %q still has no embedding after 2 runs", syncID)
		}
	}
	// Second mock must not have re-embedded rows from run 1.
	if mock2.totalTexts() >= 10 {
		t.Errorf("run-2 embedded %d rows, expected < 10 (some were done in run-1)", mock2.totalTexts())
	}
}

// TestLoop_MixedPolicy_OnlySyncedEmbedded verifies that omitted/local-only rows
// stay NULL while synced rows are embedded (gate tested on the loop path).
func TestLoop_MixedPolicy_OnlySyncedEmbedded(t *testing.T) {
	st := openLoopStore(t)

	insertRow(t, st, "synced-1", "open-proj", "open memory 1")
	insertRow(t, st, "synced-2", "open-proj", "open memory 2")
	insertRow(t, st, "omitted-1", "secret-proj", "private 1")
	insertRow(t, st, "omitted-2", "secret-proj", "private 2")

	mock := newRecordingMock(4)
	checker := &staticPolicyChecker{
		policies: map[string]localstore.Policy{
			"open-proj":   localstore.PolicySynced,
			"secret-proj": localstore.PolicyOmitted,
		},
	}

	gated := embedding.NewGated(mock, checker, true)
	loop := embedding.NewLoop(st, gated, embedding.LoopConfig{
		Interval:   100 * time.Millisecond,
		Debounce:   1 * time.Millisecond,
		BatchPause: -1,
		BatchSize:  100,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	loop.Start(ctx)
	loop.Trigger()
	waitFor(8*time.Second, func() bool { return mock.totalTexts() >= 2 })
	loop.Stop()

	// Synced rows should be embedded.
	if !hasEmbedding(t, st, "synced-1") {
		t.Error("synced-1 should be embedded")
	}
	if !hasEmbedding(t, st, "synced-2") {
		t.Error("synced-2 should be embedded")
	}
	// Omitted rows must NOT be embedded.
	if hasEmbedding(t, st, "omitted-1") {
		t.Error("omitted-1 must NOT be embedded (omitted policy)")
	}
	if hasEmbedding(t, st, "omitted-2") {
		t.Error("omitted-2 must NOT be embedded (omitted policy)")
	}
	// Provider must have received only synced texts.
	if mock.totalTexts() != 2 {
		t.Errorf("provider received %d texts, want 2 (synced only)", mock.totalTexts())
	}
}

// TestLoop_Trigger_CoalescesRapidCalls ensures multiple rapid Trigger() calls
// result in at most 2 completed ticks (not one per call).
func TestLoop_Trigger_CoalescesRapidCalls(t *testing.T) {
	st := openLoopStore(t)
	insertRow(t, st, "coalesce-row", "open", "title")

	mock := newRecordingMock(4)
	checker := newSyncedPolicyChecker("open")
	gated := embedding.NewGated(mock, checker, true)
	loop := embedding.NewLoop(st, gated, embedding.LoopConfig{
		Interval:   10 * time.Second,
		Debounce:   50 * time.Millisecond,
		BatchPause: -1,
		BatchSize:  100,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	loop.Start(ctx)

	// Fire 10 rapid triggers.
	for i := 0; i < 10; i++ {
		loop.Trigger()
	}
	// Quiescence: the assertion is an UPPER bound (coalesced into <= 2 ticks) —
	// we wait until activity stops, never for a count to be reached.
	waitQuiescent(mock, 600*time.Millisecond, 8*time.Second)
	loop.Stop()

	// All triggers coalesced into ≤ 2 ticks (debounce coalesces them into 1).
	if mock.callCount() > 2 {
		t.Errorf("callCount = %d after coalesced triggers, want ≤ 2", mock.callCount())
	}
}

// TestLoop_NilSafe_TriggerAndStop verifies nil-receiver safety on Trigger and Stop.
func TestLoop_NilSafe_TriggerAndStop(t *testing.T) {
	var l *embedding.Loop
	// Must not panic.
	l.Trigger()
	l.Stop()
}
