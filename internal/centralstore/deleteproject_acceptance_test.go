//go:build acceptance

// Package centralstore_test — acceptance coverage for Store.DeleteProject against a
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

// TestStore_DeleteProject verifies that DeleteProject removes rows from all five
// central tables for the target project and returns the total rows affected.
// A second project's rows must remain untouched.
func TestStore_DeleteProject(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	const projDel = "delete-project-target"
	const projKeep = "delete-project-bystander"

	// ── Seed target project ───────────────────────────────────────────────────

	// central_mutations + central_memories via Apply (upsert).
	mDel1 := testMutation("mut-dp-del-1", "sync-dp-del-1", projDel, domain.OpUpsert)
	mDel2 := testMutation("mut-dp-del-2", "sync-dp-del-2", projDel, domain.OpUpsert)
	for _, m := range []domain.Mutation{mDel1, mDel2} {
		if err := store.Apply(ctx, m); err != nil {
			t.Fatalf("Apply target: %v", err)
		}
	}

	// central_tombstones via WriteTombstone.
	mTomb := testMutation("mut-dp-tomb-1", "sync-dp-del-tomb-1", projDel, domain.OpDelete)
	if err := store.WriteTombstone(ctx, mTomb.SyncID, mTomb); err != nil {
		t.Fatalf("WriteTombstone target: %v", err)
	}

	// central_user_prompts — insert directly.
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO central_user_prompts (sync_id, session_id, content, project, writer_id)
		VALUES ($1, $2, $3, $4, $5)`,
		"prompt-dp-del-1", "sess-dp", "test prompt", projDel, "w",
	); err != nil {
		t.Fatalf("INSERT central_user_prompts target: %v", err)
	}

	// central_prompt_tombstones — insert directly.
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO central_prompt_tombstones (sync_id, session_id, project, deleted_at, deleted_by)
		VALUES ($1, $2, $3, now(), $4)`,
		"prompt-tomb-dp-del-1", "sess-dp", projDel, "w",
	); err != nil {
		t.Fatalf("INSERT central_prompt_tombstones target: %v", err)
	}

	// ── Seed bystander project ────────────────────────────────────────────────

	mKeep := testMutation("mut-dp-keep-1", "sync-dp-keep-1", projKeep, domain.OpUpsert)
	if err := store.Apply(ctx, mKeep); err != nil {
		t.Fatalf("Apply bystander: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO central_user_prompts (sync_id, session_id, content, project, writer_id)
		VALUES ($1, $2, $3, $4, $5)`,
		"prompt-dp-keep-1", "sess-dp", "keep prompt", projKeep, "w",
	); err != nil {
		t.Fatalf("INSERT central_user_prompts bystander: %v", err)
	}

	// ── Delete target project ─────────────────────────────────────────────────

	n, err := store.DeleteProject(ctx, projDel)
	if err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	// At minimum: 2 mutations + 2 memories + 1 tombstone + 1 prompt + 1 prompt tombstone = 7 rows.
	// (ApplySchema may create more for the mutation journal, but the floor is 7.)
	if n < 7 {
		t.Errorf("DeleteProject: returned %d total rows; want >= 7", n)
	}

	// ── Assert target project is gone ─────────────────────────────────────────

	tables := []string{
		"central_mutations",
		"central_memories",
		"central_tombstones",
		"central_user_prompts",
		"central_prompt_tombstones",
	}
	for _, tbl := range tables {
		var count int
		if err := store.Pool().QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tbl+" WHERE project = $1", projDel,
		).Scan(&count); err != nil {
			t.Fatalf("count %s after delete: %v", tbl, err)
		}
		if count != 0 {
			t.Errorf("DeleteProject: %d rows remain in %s for %q; want 0", count, tbl, projDel)
		}
	}

	// ── Assert bystander project is untouched ─────────────────────────────────

	// central_mutations for bystander.
	var mutCount int
	if err := store.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM central_mutations WHERE project = $1`, projKeep,
	).Scan(&mutCount); err != nil {
		t.Fatalf("count bystander mutations: %v", err)
	}
	if mutCount == 0 {
		t.Errorf("DeleteProject: bystander project %q lost its central_mutations rows", projKeep)
	}

	// central_memories for bystander.
	var memCount int
	if err := store.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM central_memories WHERE project = $1`, projKeep,
	).Scan(&memCount); err != nil {
		t.Fatalf("count bystander memories: %v", err)
	}
	if memCount == 0 {
		t.Errorf("DeleteProject: bystander project %q lost its central_memories rows", projKeep)
	}

	// central_user_prompts for bystander.
	var promptCount int
	if err := store.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM central_user_prompts WHERE project = $1`, projKeep,
	).Scan(&promptCount); err != nil {
		t.Fatalf("count bystander prompts: %v", err)
	}
	if promptCount == 0 {
		t.Errorf("DeleteProject: bystander project %q lost its central_user_prompts rows", projKeep)
	}
}

// TestStore_DeleteProject_Empty verifies that DeleteProject on a project that
// has no rows returns 0 without error.
func TestStore_DeleteProject_Empty(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	n, err := store.DeleteProject(ctx, "nonexistent-project-xyz")
	if err != nil {
		t.Fatalf("DeleteProject on empty project: %v", err)
	}
	if n != 0 {
		t.Errorf("DeleteProject on empty project: got %d; want 0", n)
	}
}

// TestStore_DeleteProject_EmptyName verifies that DeleteProject rejects an
// empty project name before touching the database.
func TestStore_DeleteProject_EmptyName(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	_, err := store.DeleteProject(ctx, "")
	if err == nil {
		t.Fatal("DeleteProject with empty project: expected error, got nil")
	}
}
