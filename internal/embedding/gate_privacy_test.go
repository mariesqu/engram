package embedding_test

// gate_privacy_test.go — task 5.1
//
// Recording-mock proofs that the privacy gate correctly routes calls based on
// policy. These tests target the WRITE-path gate behaviour: omitted and
// local-only projects receive zero embed calls; synced projects receive their text.
//
// No real API key is ever required.

import (
	"context"
	"testing"

	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
)

// staticPolicyChecker is a test-local PolicyChecker that maps project → policy.
type staticPolicyChecker struct {
	policies map[string]localstore.Policy
}

func (s *staticPolicyChecker) GetPolicy(project string) (localstore.Policy, error) {
	if p, ok := s.policies[project]; ok {
		return p, nil
	}
	return localstore.PolicyLocalOnly, nil // safe default
}

// TestGatePrivacy_OmittedProject_ZeroCalls verifies that a gated provider
// makes NO calls to the inner provider when the project policy is "omitted".
func TestGatePrivacy_OmittedProject_ZeroCalls(t *testing.T) {
	mock := &recordingMockProvider{}
	checker := &staticPolicyChecker{
		policies: map[string]localstore.Policy{
			"private-project": localstore.PolicyOmitted,
		},
	}
	gated := embedding.NewGated(mock, checker, true /* remote */)

	_, err := gated.Embed(context.Background(), "private-project", []string{"hello", "world"})
	if err != embedding.ErrEmbeddingGated {
		t.Errorf("expected ErrEmbeddingGated for omitted+remote, got: %v", err)
	}
	if mock.callCount() != 0 {
		t.Errorf("inner provider called %d times for omitted project, want 0", mock.callCount())
	}
}

// TestGatePrivacy_LocalOnlyProject_ZeroCalls verifies that a gated remote
// provider makes NO calls to the inner provider when the project policy is
// "local-only" (semantics: "never sync or embed remotely").
func TestGatePrivacy_LocalOnlyProject_ZeroCalls(t *testing.T) {
	mock := &recordingMockProvider{}
	checker := &staticPolicyChecker{
		policies: map[string]localstore.Policy{
			"local-proj": localstore.PolicyLocalOnly,
		},
	}
	gated := embedding.NewGated(mock, checker, true /* remote */)

	_, err := gated.Embed(context.Background(), "local-proj", []string{"secret text"})
	if err != embedding.ErrEmbeddingGated {
		t.Errorf("expected ErrEmbeddingGated for local-only+remote, got: %v", err)
	}
	if mock.callCount() != 0 {
		t.Errorf("inner provider called %d times for local-only project, want 0", mock.callCount())
	}
}

// TestGatePrivacy_SyncedProject_ReceivesTexts verifies that a gated remote
// provider DOES call the inner provider when the project policy is "synced".
// It also checks that all submitted texts are forwarded.
func TestGatePrivacy_SyncedProject_ReceivesTexts(t *testing.T) {
	mock := &recordingMockProvider{}
	checker := &staticPolicyChecker{
		policies: map[string]localstore.Policy{
			"shared-proj": localstore.PolicySynced,
		},
	}
	gated := embedding.NewGated(mock, checker, true /* remote */)

	texts := []string{"alpha", "beta", "gamma"}
	vecs, err := gated.Embed(context.Background(), "shared-proj", texts)
	if err != nil {
		t.Fatalf("expected no error for synced+remote, got: %v", err)
	}
	if len(vecs) != len(texts) {
		t.Errorf("got %d vectors, want %d", len(vecs), len(texts))
	}
	if mock.callCount() != 1 {
		t.Errorf("inner provider called %d times, want 1", mock.callCount())
	}
	if mock.totalTexts() != len(texts) {
		t.Errorf("inner provider received %d texts, want %d", mock.totalTexts(), len(texts))
	}
	// Verify the exact texts were forwarded unchanged.
	for i, want := range texts {
		if got := mock.Calls[0][i]; got != want {
			t.Errorf("text[%d]: got %q, want %q", i, got, want)
		}
	}
}

// TestGate_LocalOnly_Local_ConsentTrue_Embeds verifies that a local-only project
// IS embedded when the provider is local (remote=false) and consent=true.
func TestGate_LocalOnly_Local_ConsentTrue_Embeds(t *testing.T) {
	mock := &recordingMockProvider{dims: 4, vecValue: 0.5}
	checker := &staticPolicyChecker{
		policies: map[string]localstore.Policy{
			"local-proj": localstore.PolicyLocalOnly,
		},
	}
	// remote=false, consent=true → eligible.
	gated := embedding.NewGated(mock, checker, false /* local */, true /* consent */)

	ctx := context.Background()
	vecs, err := gated.Embed(ctx, "local-proj", []string{"private text"})
	if err != nil {
		t.Fatalf("expected no error for local-only+local+consent=true, got: %v", err)
	}
	if len(vecs) != 1 {
		t.Errorf("got %d vectors, want 1", len(vecs))
	}
	if mock.callCount() != 1 {
		t.Errorf("inner provider called %d times, want 1", mock.callCount())
	}
}

// TestGate_LocalOnly_Local_ConsentFalse_Gated verifies that a local-only project
// is NOT embedded when the provider is local but consent=false (default).
func TestGate_LocalOnly_Local_ConsentFalse_Gated(t *testing.T) {
	mock := &recordingMockProvider{dims: 4}
	checker := &staticPolicyChecker{
		policies: map[string]localstore.Policy{
			"local-proj": localstore.PolicyLocalOnly,
		},
	}
	// remote=false, no consent arg → consent defaults to false → gated.
	gated := embedding.NewGated(mock, checker, false /* local */)

	ctx := context.Background()
	_, err := gated.Embed(ctx, "local-proj", []string{"private text"})
	if err != embedding.ErrEmbeddingGated {
		t.Errorf("expected ErrEmbeddingGated for local-only+local+consent=false, got: %v", err)
	}
	if mock.callCount() != 0 {
		t.Errorf("inner provider called %d times, want 0", mock.callCount())
	}
}

// TestGatePrivacy_MultipleProjects_IsolatedCalls verifies that projects with
// different policies are handled independently within a single gate instance.
func TestGatePrivacy_MultipleProjects_IsolatedCalls(t *testing.T) {
	mock := &recordingMockProvider{}
	checker := &staticPolicyChecker{
		policies: map[string]localstore.Policy{
			"synced-a":   localstore.PolicySynced,
			"omitted-b":  localstore.PolicyOmitted,
			"synced-c":   localstore.PolicySynced,
			"local-only": localstore.PolicyLocalOnly,
		},
	}
	gated := embedding.NewGated(mock, checker, true /* remote */)

	ctx := context.Background()

	// Two synced calls should succeed.
	if _, err := gated.Embed(ctx, "synced-a", []string{"a1"}); err != nil {
		t.Errorf("synced-a: unexpected error: %v", err)
	}
	if _, err := gated.Embed(ctx, "synced-c", []string{"c1", "c2"}); err != nil {
		t.Errorf("synced-c: unexpected error: %v", err)
	}

	// Gated calls should return ErrEmbeddingGated.
	if _, err := gated.Embed(ctx, "omitted-b", []string{"secret"}); err != embedding.ErrEmbeddingGated {
		t.Errorf("omitted-b: expected ErrEmbeddingGated, got: %v", err)
	}
	if _, err := gated.Embed(ctx, "local-only", []string{"private"}); err != embedding.ErrEmbeddingGated {
		t.Errorf("local-only: expected ErrEmbeddingGated, got: %v", err)
	}

	// Inner provider should have been called exactly twice (synced-a + synced-c).
	if mock.callCount() != 2 {
		t.Errorf("inner provider called %d times, want 2", mock.callCount())
	}
	if mock.totalTexts() != 3 { // "a1" + "c1" + "c2"
		t.Errorf("inner provider received %d total texts, want 3", mock.totalTexts())
	}
}
