package embedding_test

// loop_privacy_test.go — task 1b.2
//
// LOOP-PATH PRIVACY PROOF (the headline test of PR-1b).
//
// These tests assert that permanently-gated projects (omitted / local-only)
// are NEVER passed to the inner provider via the backfill loop, even when
// their rows have embedding IS NULL. The recording mock is wired as the
// 'inner' of NewGated (real-gate pattern — binding constraint from PR-1).
//
// Three tests:
//  1. Omitted project: rows stay NULL, mock receives zero calls.
//  2. Local-only + remote provider: rows stay NULL, mock receives zero calls.
//  3. PolicyFlip mid-backfill: rows inserted AFTER the policy is flipped to
//     omitted are never reached; rows inserted before the flip (synced) ARE
//     embedded before the flip takes effect.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
)

// mutablePolicyChecker is a thread-safe PolicyChecker that maps ALL projects
// to a single mutable policy value. Used to simulate a mid-session policy flip.
type mutablePolicyChecker struct {
	mu     sync.Mutex
	policy localstore.Policy
}

func (m *mutablePolicyChecker) GetPolicy(_ string) (localstore.Policy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.policy, nil
}

func (m *mutablePolicyChecker) setPolicy(p localstore.Policy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.policy = p
}

// TestLoop_Omitted_RecordingMock_ZeroCalls is the headline privacy proof:
//
//	Store has: synced project (2 rows) + omitted project (3 rows).
//	The loop runs with NewGated(recordingMock, checker, remote=true).
//	Expected: mock receives ONLY the 2 synced texts.
//	          3 omitted rows remain NULL after the loop.
//	          The loop does NOT spin hot on the permanently-gated rows.
func TestLoop_Omitted_RecordingMock_ZeroCalls(t *testing.T) {
	st := openLoopStore(t)

	insertRow(t, st, "synced-1", "open-proj", "public note alpha")
	insertRow(t, st, "synced-2", "open-proj", "public note beta")
	insertRow(t, st, "omitted-1", "secret-proj", "private note one")
	insertRow(t, st, "omitted-2", "secret-proj", "private note two")
	insertRow(t, st, "omitted-3", "secret-proj", "private note three")

	mock := newRecordingMock(4)
	checker := &staticPolicyChecker{
		policies: map[string]localstore.Policy{
			"open-proj":   localstore.PolicySynced,
			"secret-proj": localstore.PolicyOmitted,
		},
	}

	gated := embedding.NewGated(mock, checker, true /* remote */)
	loop := embedding.NewLoop(st, gated, embedding.LoopConfig{
		Interval:   10 * time.Second,
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

	// Synced rows MUST be embedded.
	if !hasEmbedding(t, st, "synced-1") {
		t.Error("synced-1 should be embedded")
	}
	if !hasEmbedding(t, st, "synced-2") {
		t.Error("synced-2 should be embedded")
	}

	// Omitted rows MUST remain NULL — text NEVER reaches the provider.
	if hasEmbedding(t, st, "omitted-1") {
		t.Error("omitted-1 must NOT be embedded (policy: omitted)")
	}
	if hasEmbedding(t, st, "omitted-2") {
		t.Error("omitted-2 must NOT be embedded (policy: omitted)")
	}
	if hasEmbedding(t, st, "omitted-3") {
		t.Error("omitted-3 must NOT be embedded (policy: omitted)")
	}

	// Provider received ONLY the synced texts.
	if mock.totalTexts() != 2 {
		t.Errorf("provider received %d texts, want 2 (synced only)", mock.totalTexts())
	}

	// Verify no secret-proj text leaked to the provider.
	for _, call := range mock.Calls {
		for _, text := range call {
			if text == "private note one" || text == "private note two" || text == "private note three" {
				t.Errorf("omitted text leaked to provider: %q", text)
			}
		}
	}
}

// TestLoop_LocalOnly_Remote_RecordingMock_ZeroCalls proves that local-only rows
// are never sent to a remote provider via the backfill loop.
//
//	Store has: synced project (1 row) + local-only project (2 rows).
//	Expected: mock receives only the 1 synced text.
//	          2 local-only rows remain NULL.
func TestLoop_LocalOnly_Remote_RecordingMock_ZeroCalls(t *testing.T) {
	st := openLoopStore(t)

	insertRow(t, st, "sync-1", "shared", "shareable content")
	insertRow(t, st, "local-1", "private-local", "sensitive data A")
	insertRow(t, st, "local-2", "private-local", "sensitive data B")

	mock := newRecordingMock(4)
	checker := &staticPolicyChecker{
		policies: map[string]localstore.Policy{
			"shared":        localstore.PolicySynced,
			"private-local": localstore.PolicyLocalOnly,
		},
	}

	gated := embedding.NewGated(mock, checker, true /* remote — text must not leave node */)
	loop := embedding.NewLoop(st, gated, embedding.LoopConfig{
		Interval:   10 * time.Second,
		Debounce:   1 * time.Millisecond,
		BatchPause: -1,
		BatchSize:  100,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	loop.Start(ctx)
	loop.Trigger()
	waitFor(8*time.Second, func() bool { return mock.totalTexts() >= 1 })
	loop.Stop()

	// Synced row must be embedded.
	if !hasEmbedding(t, st, "sync-1") {
		t.Error("sync-1 should be embedded")
	}

	// Local-only rows must remain NULL.
	if hasEmbedding(t, st, "local-1") {
		t.Error("local-1 must NOT be embedded (policy: local-only + remote provider)")
	}
	if hasEmbedding(t, st, "local-2") {
		t.Error("local-2 must NOT be embedded (policy: local-only + remote provider)")
	}

	// Provider received exactly 1 text (from synced project only).
	if mock.totalTexts() != 1 {
		t.Errorf("provider received %d texts, want 1 (synced only)", mock.totalTexts())
	}

	// Verify no local-only text leaked.
	for _, call := range mock.Calls {
		for _, text := range call {
			if text == "sensitive data A" || text == "sensitive data B" {
				t.Errorf("local-only text leaked to remote provider: %q", text)
			}
		}
	}
}

// TestLoop_LocalOnly_Local_ConsentTrue_Embeds proves that a local-only project
// IS embedded when the provider is local (remote=false) AND consent=true.
//
//	Store has: local-only project (2 rows).
//	Gate: NewGated(mock, checker, remote=false, consent=true).
//	Expected: mock receives 2 texts; rows gain embeddings.
func TestLoop_LocalOnly_Local_ConsentTrue_Embeds(t *testing.T) {
	st := openLoopStore(t)

	insertRow(t, st, "local-c1", "consented-proj", "local private A")
	insertRow(t, st, "local-c2", "consented-proj", "local private B")

	mock := newRecordingMock(4)
	checker := &staticPolicyChecker{
		policies: map[string]localstore.Policy{
			"consented-proj": localstore.PolicyLocalOnly,
		},
	}

	// remote=false, consent=true → local-only project IS eligible.
	gated := embedding.NewGated(mock, checker, false /* local provider */, true /* consent */)
	loop := embedding.NewLoop(st, gated, embedding.LoopConfig{
		Interval:   10 * time.Second,
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

	// Both rows must have embeddings.
	if !hasEmbedding(t, st, "local-c1") {
		t.Error("local-c1 should be embedded (local provider + consent=true)")
	}
	if !hasEmbedding(t, st, "local-c2") {
		t.Error("local-c2 should be embedded (local provider + consent=true)")
	}

	// Provider received exactly 2 texts.
	if mock.totalTexts() != 2 {
		t.Errorf("provider received %d texts, want 2", mock.totalTexts())
	}
}

// TestLoop_LocalOnly_Local_ConsentFalse_ZeroCalls proves that a local-only project
// is NOT embedded when the provider is local but consent=false (default).
//
//	Store has: local-only project (2 rows).
//	Gate: NewGated(mock, checker, remote=false) — consent defaults to false.
//	Expected: mock receives 0 texts; rows remain NULL.
func TestLoop_LocalOnly_Local_ConsentFalse_ZeroCalls(t *testing.T) {
	st := openLoopStore(t)

	insertRow(t, st, "local-nc1", "no-consent-proj", "sensitive A")
	insertRow(t, st, "local-nc2", "no-consent-proj", "sensitive B")

	mock := newRecordingMock(4)
	checker := &staticPolicyChecker{
		policies: map[string]localstore.Policy{
			"no-consent-proj": localstore.PolicyLocalOnly,
		},
	}

	// remote=false, no consent arg → consent defaults to false → gated.
	gated := embedding.NewGated(mock, checker, false /* local provider */)
	loop := embedding.NewLoop(st, gated, embedding.LoopConfig{
		Interval:   10 * time.Second,
		Debounce:   1 * time.Millisecond,
		BatchPause: -1,
		BatchSize:  100,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	loop.Start(ctx)
	loop.Trigger()
	time.Sleep(300 * time.Millisecond)
	loop.Stop()

	// Both rows must remain NULL.
	if hasEmbedding(t, st, "local-nc1") {
		t.Error("local-nc1 must NOT be embedded (local-only + consent=false)")
	}
	if hasEmbedding(t, st, "local-nc2") {
		t.Error("local-nc2 must NOT be embedded (local-only + consent=false)")
	}

	// Provider received zero calls.
	if mock.totalTexts() != 0 {
		t.Errorf("provider received %d texts, want 0", mock.totalTexts())
	}
}

// TestLoop_PolicyFlip_MidBackfill_PostFlipRowsSkipped verifies that rows added
// AFTER a project's policy is flipped to omitted are not embedded, while rows
// that were in a synced project before the flip ARE embedded on the first pass.
//
// This models the real-world scenario where a user changes a project's policy
// from "synced" to "omitted" mid-session. Existing synced rows that were already
// embedded retain their embeddings. New rows added after the flip stay NULL.
//
// Test plan:
//  1. Insert 2 rows in "flip-proj" with PolicySynced → run loop → both embedded.
//  2. Flip "flip-proj" to PolicyOmitted in the checker.
//  3. Insert 2 more rows in "flip-proj" → run loop → new rows stay NULL.
func TestLoop_PolicyFlip_MidBackfill_PostFlipRowsSkipped(t *testing.T) {
	st := openLoopStore(t)

	// Step 1: Insert rows while policy is synced.
	insertRow(t, st, "pre-flip-1", "flip-proj", "content before flip A")
	insertRow(t, st, "pre-flip-2", "flip-proj", "content before flip B")

	// flipChecker allows the test to mutate the policy between runs.
	flipChecker := &mutablePolicyChecker{policy: localstore.PolicySynced}

	mock := newRecordingMock(4)
	gated := embedding.NewGated(mock, flipChecker, true)

	makeLoop := func() *embedding.Loop {
		return embedding.NewLoop(st, gated, embedding.LoopConfig{
			Interval:   10 * time.Second,
			Debounce:   1 * time.Millisecond,
			BatchPause: -1,
			BatchSize:  100,
		})
	}

	runOnce := func(l *embedding.Loop, mock *recordingMockProvider) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		l.Start(ctx)
		l.Trigger()
		// Quiescence covers both phases: the positive pre-flip run AND the
		// negative post-flip run (where waiting for a count would never end).
		waitQuiescent(mock, 600*time.Millisecond, 8*time.Second)
		l.Stop()
	}

	// First run: policy is synced; pre-flip rows should be embedded.
	runOnce(makeLoop(), mock)

	if !hasEmbedding(t, st, "pre-flip-1") {
		t.Error("pre-flip-1 should be embedded (policy was synced)")
	}
	if !hasEmbedding(t, st, "pre-flip-2") {
		t.Error("pre-flip-2 should be embedded (policy was synced)")
	}

	// Step 2: Flip the policy to omitted.
	flipChecker.setPolicy(localstore.PolicyOmitted)

	// Step 3: Insert new rows AFTER the flip.
	insertRow(t, st, "post-flip-1", "flip-proj", "content after flip A")
	insertRow(t, st, "post-flip-2", "flip-proj", "content after flip B")

	callsBefore := mock.callCount()

	// Second run: policy is now omitted.
	runOnce(makeLoop(), mock)

	// New rows must NOT be embedded.
	if hasEmbedding(t, st, "post-flip-1") {
		t.Error("post-flip-1 must NOT be embedded (policy flipped to omitted)")
	}
	if hasEmbedding(t, st, "post-flip-2") {
		t.Error("post-flip-2 must NOT be embedded (policy flipped to omitted)")
	}

	// Pre-flip rows retain their existing embeddings.
	if !hasEmbedding(t, st, "pre-flip-1") {
		t.Error("pre-flip-1 lost its embedding after policy flip (should be retained)")
	}
	if !hasEmbedding(t, st, "pre-flip-2") {
		t.Error("pre-flip-2 lost its embedding after policy flip (should be retained)")
	}

	// Provider received zero additional calls after the flip.
	if mock.callCount() != callsBefore {
		t.Errorf("provider received %d extra calls after policy flip, want 0", mock.callCount()-callsBefore)
	}
}
