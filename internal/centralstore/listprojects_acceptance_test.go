//go:build acceptance

// Package centralstore_test — acceptance coverage for Store.ListProjects against a
// real Postgres instance (embedded-postgres, started once per package in TestMain
// defined in store_acceptance_test.go).
//
// Per-test isolation: each test uses newIsolatedStore so it runs in its own schema.
package centralstore_test

import (
	"context"
	"testing"

	"github.com/mariesqu/engram/internal/domain"
)

// TestStore_ListProjects verifies that ListProjects returns the correct sorted,
// distinct, non-empty set of project names from central_mutations.
//
// Coverage:
//   - Two distinct projects are returned after applying mutations for each.
//   - A mutation with an empty project (inserted via Store.InsertMutation with
//     project="") is NOT included in the result.
//   - The result is sorted ascending.
//   - Duplicate projects from multiple mutations yield only one entry.
func TestStore_ListProjects(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	// Apply mutations for two distinct projects via the full Apply path.
	// Apply calls InsertMutation internally, so the project appears in
	// central_mutations (which is what ListProjects queries).
	projAlpha := "listprojects-alpha"
	projBeta := "listprojects-beta"

	mAlpha1 := testMutation("mut-lp-alpha-1", "sync-lp-alpha-1", projAlpha, domain.OpUpsert)
	mAlpha2 := testMutation("mut-lp-alpha-2", "sync-lp-alpha-2", projAlpha, domain.OpUpsert)
	mBeta1 := testMutation("mut-lp-beta-1", "sync-lp-beta-1", projBeta, domain.OpUpsert)

	for _, m := range []struct {
		name string
		mut  domain.Mutation
	}{
		{"alpha-1", mAlpha1},
		{"alpha-2", mAlpha2},
		{"beta-1", mBeta1},
	} {
		if err := store.Apply(ctx, m.mut); err != nil {
			t.Fatalf("Apply %s: %v", m.name, err)
		}
	}

	// Also insert a mutation with an empty project name directly into
	// central_mutations to confirm it is excluded from ListProjects output.
	// We use InsertMutation which bypasses the higher-level Apply path and
	// writes a raw row.
	mEmpty := testMutation("mut-lp-empty-1", "sync-lp-empty-1", "" /* empty project */, domain.OpUpsert)
	if _, err := store.InsertMutation(ctx, mEmpty); err != nil {
		// If InsertMutation rejects an empty project at the DB level (CHECK constraint)
		// we skip this part of the test — the WHERE project <> '' clause still works.
		t.Logf("InsertMutation with empty project rejected (DB constraint): %v — skip empty-project check", err)
	}

	got, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}

	// Must contain exactly the two non-empty project names.
	// (The empty-project row may or may not have been inserted, but either way
	// it must not appear in the output.)
	if len(got) != 2 {
		t.Fatalf("ListProjects: got %v (len=%d), want exactly [%q, %q]",
			got, len(got), projAlpha, projBeta)
	}

	// Results must be sorted ascending.
	if got[0] != projAlpha {
		t.Errorf("ListProjects[0] = %q; want %q", got[0], projAlpha)
	}
	if got[1] != projBeta {
		t.Errorf("ListProjects[1] = %q; want %q", got[1], projBeta)
	}

	// Verify no duplicates: applying alpha-1 and alpha-2 must still yield only
	// one "alpha" entry. (Already covered by len==2, but assert explicitly.)
	for _, p := range got {
		if p == "" {
			t.Errorf("ListProjects returned an empty-string project name: %v", got)
		}
	}
}

// TestStore_ListProjects_Empty verifies that ListProjects returns a nil or
// empty slice (no error) when no mutations have been applied.
func TestStore_ListProjects_Empty(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	got, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects on empty store: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListProjects on empty store: got %v; want empty", got)
	}
}
