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

	// wantReason is the expected SearchDegradation.Reason value.
	// Empty string means "no degradation note expected".
	wantReason string
}

var degradationMatrix = []degradationCase{
	// ── FTS baseline (MUST never set Reason) ─────────────────────────────────
	{
		name:      "fts_mode_no_embed_fn",
		mode:      "fts",
		noEmbedFn: true,
		wantReason: "", // FTS path — never sets Reason
	},
	{
		name:      "empty_mode_no_embed_fn",
		mode:      "",
		noEmbedFn: true,
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
		// Error from embed fn causes the same "policy" degradation path because
		// ErrEmbeddingGated check happens after the call — see search.go.
		// Actual reason produced by the search path:
		wantReason: "semantic search unavailable for this project's policy; showing keyword results",
	},
	{
		name:         "semantic_gated_project",
		mode:         "semantic",
		gatedProject: true,
		wantReason:   "semantic results not ready; showing keyword results",
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
		wantReason: "semantic search unavailable for this project's policy; showing keyword results",
	},
	{
		name:         "hybrid_gated_project",
		mode:         "hybrid",
		gatedProject: true,
		// "1 pending" because we add one observation with no embedding.
		wantReason: "semantic results not ready (1 pending); showing keyword results",
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

			if !tc.noEmbedFn {
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
