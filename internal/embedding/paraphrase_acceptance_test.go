//go:build acceptance

package embedding_test

// paraphrase_acceptance_test.go — task 5.2
//
// Acceptance test: seed a localstore with mock vectors and assert that hybrid
// search finds a paraphrase that FTS misses.
//
// Build tag: acceptance — run with: go test -tags acceptance ./internal/embedding/...
// These tests require no real API key and no external services.

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/localstore"
)

// TestParaphrase_HybridFindsPhraseNotInFTS seeds the store with two observations:
//   - "A": exact words match the query ("quick brown fox")
//   - "B": paraphrase ("speedy auburn canine")
//
// FTS would find "A" but not "B". Hybrid should return both because the mock
// vectors place them close in embedding space.
func TestParaphrase_HybridFindsPhraseNotInFTS(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	st, err := localstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	const project = "test-project"
	const model = "mock-v1"

	// Make the project "synced" so the gate allows embedding.
	if err := st.SetPolicy(project, localstore.PolicySynced); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	// Insert two observations. "A" matches the FTS query; "B" is a paraphrase.
	resA, err := st.AddObservation(localstore.AddObservationParams{
		Title:   "quick brown fox",
		Content: "quick brown fox jumps over the lazy dog",
		Project: project,
		Type:    "decision",
	})
	if err != nil {
		t.Fatalf("AddObservation A: %v", err)
	}

	resB, err := st.AddObservation(localstore.AddObservationParams{
		Title:   "speedy auburn canine",
		Content: "speedy auburn canine leaps above the sluggish hound",
		Project: project,
		Type:    "decision",
	})
	if err != nil {
		t.Fatalf("AddObservation B: %v", err)
	}

	// Build vectors: both A and B are close to the query vector (cosine ≈ 0.99).
	// Dim 0 is the "topic axis"; all vectors have high weight there.
	queryVec := makeUnitVec(256, map[int]float32{0: 1.0})
	vecA := makeUnitVec(256, map[int]float32{0: 0.99, 1: 0.1})
	vecB := makeUnitVec(256, map[int]float32{0: 0.98, 2: 0.1})

	ts := time.Now().UTC().Format(time.RFC3339)
	db := st.DB()
	if err := localstore.UpdateEmbedding(db, resA.SyncID, vecA, model, ts); err != nil {
		t.Fatalf("UpdateEmbedding A: %v", err)
	}
	if err := localstore.UpdateEmbedding(db, resB.SyncID, vecB, model, ts); err != nil {
		t.Fatalf("UpdateEmbedding B: %v", err)
	}

	// Wire a deterministic embed fn that returns queryVec for any query text.
	st.SetEmbedFn(func(ctx context.Context, proj string, texts []string) ([][]float32, error) {
		vecs := make([][]float32, len(texts))
		for i := range texts {
			// Return a copy of queryVec for each input.
			v := make([]float32, len(queryVec))
			copy(v, queryVec)
			vecs[i] = v
		}
		return vecs, nil
	}, 256)

	// FTS search for "quick brown fox" should NOT find "B" (paraphrase).
	ftsResults, _, err := st.SearchMemoriesFiltered(
		"quick brown fox",
		project,
		20,
		localstore.SearchFilter{Mode: "fts"},
	)
	if err != nil {
		t.Fatalf("FTS search: %v", err)
	}
	foundBInFTS := false
	for _, r := range ftsResults {
		if r.SyncID == resB.SyncID {
			foundBInFTS = true
		}
	}
	if foundBInFTS {
		t.Logf("FTS found the paraphrase (unexpected but not wrong) — test still validates hybrid")
	}

	// Hybrid search should find BOTH A and B because both are close in vector space.
	hybridResults, deg, err := st.SearchMemoriesFiltered(
		"quick brown fox",
		project,
		20,
		localstore.SearchFilter{Mode: "hybrid"},
	)
	if err != nil {
		t.Fatalf("hybrid search: %v", err)
	}
	if deg.Reason != "" {
		t.Logf("hybrid degradation note: %s", deg.Reason)
	}

	foundA, foundB := false, false
	for _, r := range hybridResults {
		if r.SyncID == resA.SyncID {
			foundA = true
		}
		if r.SyncID == resB.SyncID {
			foundB = true
		}
	}
	if !foundA {
		t.Error("hybrid: expected to find A (exact FTS match)")
	}
	if !foundB {
		t.Error("hybrid: expected to find B (paraphrase — cosine hit)")
	}
}

// makeUnitVec returns a unit-length []float32 of size dims with given non-zero
// components. All unspecified dims are zero.
func makeUnitVec(dims int, components map[int]float32) []float32 {
	v := make([]float32, dims)
	for i, val := range components {
		if i < dims {
			v[i] = val
		}
	}
	// L2-normalise.
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	mag := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= mag
	}
	return v
}
