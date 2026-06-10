//go:build acceptance

package embedding_test

// sidecar_acceptance_test.go — task 2.9
//
// Acceptance tests for OllamaSidecarProvider wired through NewGated.
// Uses an httptest.Server as the Ollama stub — no real Ollama binary required.
//
// Build tag: acceptance — run with:
//
//	go test -tags acceptance ./internal/embedding/...
//
// Test suite: TestAcceptance_Sidecar_ConsentGate_Suite
//   - Sub-case A: local-only project + remote=false + consent=true → rows embedded.
//   - Sub-case B: local-only project + remote=false + consent=false → zero calls.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
)

// ollamaSidecarStub starts an httptest.Server that returns deterministic
// embeddings of size dims for every POST /api/embeddings request.
func ollamaSidecarStub(t *testing.T, dims int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vec := make([]float32, dims)
		for i := range vec {
			vec[i] = 0.1
		}
		resp := map[string][]float32{"embedding": vec}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runSidecarLoop opens a temp store, inserts rows for the given project,
// wires the OllamaSidecarProvider through NewGated, runs the backfill loop
// once, and returns (store, mock-to-check-provider-calls, hadEmbedding).
func runSidecarLoop(
	t *testing.T,
	serverURL string,
	dims int,
	project string,
	policy localstore.Policy,
	remote bool,
	consent bool,
	syncIDs []string,
) (embeddedCount int, providerCallCount int) {
	t.Helper()

	st, err := localstore.Open(filepath.Join(t.TempDir(), "acc.db"))
	if err != nil {
		t.Fatalf("localstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Insert rows.
	for i, sid := range syncIDs {
		insertRow(t, st, sid, project, "acceptance text "+string(rune('A'+i)))
	}

	// Static policy checker.
	checker := &staticPolicyChecker{
		policies: map[string]localstore.Policy{project: policy},
	}

	// Wire OllamaSidecarProvider as the inner of NewGated.
	inner := embedding.NewOllamaSidecar("nomic-embed-text", dims,
		embedding.WithOllamaHost(serverURL),
		embedding.WithOllamaTimeout(5*time.Second),
	)
	var gated embedding.EmbeddingProvider
	if consent {
		gated = embedding.NewGated(inner, checker, remote, true)
	} else {
		gated = embedding.NewGated(inner, checker, remote)
	}

	loop := embedding.NewLoop(st, gated, embedding.LoopConfig{
		Interval:   10 * time.Second,
		Debounce:   1 * time.Millisecond,
		BatchPause: -1,
		BatchSize:  100,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	loop.Start(ctx)
	loop.Trigger()
	time.Sleep(500 * time.Millisecond)
	loop.Stop()

	// Count embedded rows.
	for _, sid := range syncIDs {
		var count int
		_ = st.DB().QueryRow(
			`SELECT COUNT(*) FROM memories WHERE sync_id=? AND embedding IS NOT NULL`, sid,
		).Scan(&count)
		embeddedCount += count
	}

	return embeddedCount, -1 // providerCallCount not tracked here; use hasEmbedding proxy
}

// TestAcceptance_Sidecar_ConsentGate_Suite exercises the two consent sub-cases
// using a real OllamaSidecarProvider wired through NewGated.
func TestAcceptance_Sidecar_ConsentGate_Suite(t *testing.T) {
	const dims = 4

	t.Run("consent=true_embeds_local_only", func(t *testing.T) {
		stub := ollamaSidecarStub(t, dims)

		syncIDs := []string{"acc-c1", "acc-c2", "acc-c3"}
		embedded, _ := runSidecarLoop(t, stub.URL, dims,
			"local-proj", localstore.PolicyLocalOnly,
			false /* local provider */, true, /* consent */
			syncIDs,
		)

		if embedded != len(syncIDs) {
			t.Errorf("consent=true: got %d embedded rows, want %d", embedded, len(syncIDs))
		}
	})

	t.Run("consent=false_zeros_local_only", func(t *testing.T) {
		stub := ollamaSidecarStub(t, dims)

		syncIDs := []string{"acc-nc1", "acc-nc2"}
		embedded, _ := runSidecarLoop(t, stub.URL, dims,
			"local-proj", localstore.PolicyLocalOnly,
			false /* local provider */, false, /* no consent */
			syncIDs,
		)

		if embedded != 0 {
			t.Errorf("consent=false: got %d embedded rows, want 0", embedded)
		}
	})
}
