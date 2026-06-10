package embedding_test

// degradation_test.go — task 5.3
//
// Full degradation matrix: verifies that SearchMemoriesFiltered returns the
// correct SearchDegradation.Reason for every combination of mode × failure scenario.
//
// Invariant: mode="" and mode="fts" NEVER set Reason (byte-identical FTS path).
// mode="semantic" and mode="hybrid" set Reason only when they had to degrade.
//
// No real API key required.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
)

// degradationCase describes a single row in the degradation matrix table test.
type degradationCase struct {
	name string

	// mode is the SearchFilter.Mode value.
	mode string

	// embedFnErr, if non-nil, is the error the embed fn will return.
	embedFnErr error

	// noEmbedFn, if true, leaves the embed fn unwired (nil).
	noEmbedFn bool

	// gatedProject, if true, sets the project policy to "omitted" (gate blocks embed).
	gatedProject bool

	// useRealGate, if true, wires the embed fn through the REAL gated provider
	// (NewGated + recording mock) instead of a policy-ignoring closure, and the
	// test additionally asserts the inner provider received ZERO calls. This is
	// the end-to-end query-side privacy proof — a closure that ignores policy
	// exercises the wrong degradation path entirely (round-1 review HIGH).
	useRealGate bool

	// wantReason is the expected SearchDegradation.Reason value.
	// Empty string means "no degradation note expected".
	wantReason string
}

var degradationMatrix = []degradationCase{
	// ── FTS baseline (MUST never set Reason) ─────────────────────────────────
	{
		name:       "fts_mode_no_embed_fn",
		mode:       "fts",
		noEmbedFn:  true,
		wantReason: "", // FTS path — never sets Reason
	},
	{
		name:       "empty_mode_no_embed_fn",
		mode:       "",
		noEmbedFn:  true,
		wantReason: "", // same byte-identical FTS path
	},
	{
		name:         "fts_mode_gated_project",
		mode:         "fts",
		gatedProject: true,
		wantReason:   "", // FTS path — never sets Reason even if project is gated
	},

	// ── Semantic mode degradation scenarios ──────────────────────────────────
	{
		name:       "semantic_no_embed_fn",
		mode:       "semantic",
		noEmbedFn:  true,
		wantReason: "semantic search unavailable: not configured; showing keyword results",
	},
	{
		name:       "semantic_embed_fn_error",
		mode:       "semantic",
		embedFnErr: errors.New("provider timeout"),
		// A transient provider failure is NOT a policy denial — the search path
		// distinguishes ErrEmbeddingGated (policy) from any other error.
		wantReason: "semantic search unavailable: provider error; showing keyword results",
	},
	{
		name:         "semantic_gated_project",
		mode:         "semantic",
		gatedProject: true,
		useRealGate:  true,
		wantReason:   "semantic search unavailable for this project's policy; showing keyword results",
	},
	{
		name: "semantic_no_vectors_in_db",
		mode: "semantic",
		// embed fn is wired but no observations have embeddings yet.
		wantReason: "semantic results not ready; showing keyword results",
	},

	// ── Hybrid mode degradation scenarios ─────────────────────────────────────
	{
		name:       "hybrid_no_embed_fn",
		mode:       "hybrid",
		noEmbedFn:  true,
		wantReason: "semantic search unavailable: not configured; showing keyword results",
	},
	{
		name:       "hybrid_embed_fn_error",
		mode:       "hybrid",
		embedFnErr: errors.New("network error"),
		wantReason: "semantic search unavailable: provider error; showing keyword results",
	},
	{
		name:         "hybrid_gated_project",
		mode:         "hybrid",
		gatedProject: true,
		useRealGate:  true,
		wantReason:   "semantic search unavailable for this project's policy; showing keyword results",
	},
	{
		name: "hybrid_no_vectors_in_db",
		mode: "hybrid",
		// embed fn is wired but no observations have embeddings.
		wantReason: "semantic results not ready (1 pending); showing keyword results",
	},

	// ── Semantic mode happy path (no degradation) ─────────────────────────────
	// No wantReason — covered by integration tests in search_test.go.
}

func TestDegradation_Matrix(t *testing.T) {
	for _, tc := range degradationMatrix {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "test.db")

			st, err := localstore.Open(dbPath)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer st.Close()

			const project = "proj"
			policy := localstore.PolicySynced
			if tc.gatedProject {
				policy = localstore.PolicyOmitted
			}
			if err := st.SetPolicy(project, policy); err != nil {
				t.Fatalf("SetPolicy: %v", err)
			}

			var mock *recordingMockProvider
			switch {
			case tc.useRealGate:
				// End-to-end query-side gate: the REAL gated provider wrapping a
				// recording mock, exactly as the daemon wires it. The store's own
				// policy table is the checker, so the gate sees the live policy.
				mock = &recordingMockProvider{}
				gated := embedding.NewGated(mock, st, true /* remote */)
				st.SetEmbedFn(func(ctx context.Context, proj string, texts []string) ([][]float32, error) {
					return gated.Embed(ctx, proj, texts)
				}, 256)
			case !tc.noEmbedFn:
				embedErr := tc.embedFnErr
				st.SetEmbedFn(func(_ context.Context, _ string, texts []string) ([][]float32, error) {
					if embedErr != nil {
						return nil, embedErr
					}
					vecs := make([][]float32, len(texts))
					for i := range texts {
						v := make([]float32, 256)
						v[0] = 1.0 // unit vector
						vecs[i] = v
					}
					return vecs, nil
				}, 256)
			}

			// Add a FTS-searchable observation so the FTS path has something
			// to return. This prevents an unrelated "no results" from obscuring
			// whether the degradation path was taken.
			_, addErr := st.AddObservation(localstore.AddObservationParams{
				Title:   "test observation for degradation",
				Content: "some content",
				Project: project,
				Type:    "decision",
			})
			if addErr != nil {
				t.Fatalf("AddObservation: %v", addErr)
			}

			_, deg, err := st.SearchMemoriesFiltered(
				"test observation",
				project,
				10,
				localstore.SearchFilter{Mode: tc.mode},
			)
			if err != nil {
				t.Fatalf("SearchMemoriesFiltered: %v", err)
			}

			// The query-side privacy proof: with the real gate on an omitted
			// project, the inner provider must have received ZERO calls — the
			// query text never left the building.
			if tc.useRealGate && mock != nil && mock.callCount() != 0 {
				t.Errorf("PRIVACY VIOLATION: gated project query reached the provider (%d calls)", mock.callCount())
			}

			if tc.wantReason == "" {
				if deg.Reason != "" {
					t.Errorf("expected no degradation reason, got: %q", deg.Reason)
				}
			} else {
				if deg.Reason == "" {
					t.Errorf("expected degradation reason %q, got empty", tc.wantReason)
				} else if deg.Reason != tc.wantReason {
					t.Errorf("degradation reason mismatch:\n  got:  %q\n  want: %q",
						deg.Reason, tc.wantReason)
				}
			}
		})
	}
}
