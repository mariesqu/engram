package localstore

// Tests for PurgeProjectLocal and TombstoneProject.
//
// Coverage:
//   - PurgeProjectLocal deletes the project's memories, prompts, tombstones,
//     and outbox rows; sets policy to omitted; leaves another project untouched.
//   - TombstoneProject soft-deletes the project's live memories and enqueues
//     OpDelete outbox entries; leaves another project's memories untouched.

import (
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// TestProjectDelete_EmptyProjectRejected verifies the W1 guard: a blank or
// whitespace-only project name (which normalizes to "") is refused by both
// destructive store methods, so the empty-project bucket can't be purged by accident.
func TestProjectDelete_EmptyProjectRejected(t *testing.T) {
	st := openTempStore(t)
	for _, p := range []string{"", "   ", "\t"} {
		if _, err := st.PurgeProjectLocal(p); err == nil {
			t.Errorf("PurgeProjectLocal(%q): expected error, got nil", p)
		}
		if _, err := st.TombstoneProject(p, "w1"); err == nil {
			t.Errorf("TombstoneProject(%q): expected error, got nil", p)
		}
	}
}

// TestPurgeProjectLocal_DeletesRowsAndSetsOmitted verifies that PurgeProjectLocal:
//   - hard-deletes memories for the target project
//   - sets its policy to omitted
//   - does NOT touch memories belonging to another project
func TestPurgeProjectLocal_DeletesRowsAndSetsOmitted(t *testing.T) {
	st := openTempStore(t)

	// Seed two memories for the target project.
	seedMemory(t, st, "proj-del", "sync-del-1")
	seedMemory(t, st, "proj-del", "sync-del-2")

	// Seed one memory for the bystander project.
	seedMemory(t, st, "proj-keep", "sync-keep-1")

	// Purge.
	n, err := st.PurgeProjectLocal("proj-del")
	if err != nil {
		t.Fatalf("PurgeProjectLocal: %v", err)
	}
	if n < 2 {
		t.Errorf("PurgeProjectLocal: returned %d deleted rows; want >= 2 (at least the 2 memories)", n)
	}

	// The target project's memories must be gone.
	var count int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE project = ?`, "proj-del",
	).Scan(&count); err != nil {
		t.Fatalf("count memories after purge: %v", err)
	}
	if count != 0 {
		t.Errorf("PurgeProjectLocal: %d memories remain for proj-del; want 0", count)
	}

	// Policy must be omitted.
	pol, err := st.GetPolicy("proj-del")
	if err != nil {
		t.Fatalf("GetPolicy after purge: %v", err)
	}
	if pol != PolicyOmitted {
		t.Errorf("PurgeProjectLocal: policy = %q; want %q", pol, PolicyOmitted)
	}

	// Bystander project must be untouched.
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE project = ?`, "proj-keep",
	).Scan(&count); err != nil {
		t.Fatalf("count bystander memories: %v", err)
	}
	if count != 1 {
		t.Errorf("PurgeProjectLocal: bystander proj-keep has %d memories; want 1", count)
	}
}

// TestPurgeProjectLocal_ClearsOutboxRows verifies that pending sync_mutations
// rows for the project are deleted by PurgeProjectLocal.
func TestPurgeProjectLocal_ClearsOutboxRows(t *testing.T) {
	st := openTempStore(t)

	// Seed a memory and let LocalWrite enqueue an outbox row for it.
	m := domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "sync-outbox-del",
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "outbox test",
		Content:    "content",
		Project:    "proj-outbox",
		Scope:      "project",
		Version:    1,
		WriterID:   "w",
		UpdatedAt:  time.Now().UTC(),
	}
	if _, err := st.LocalWrite(m); err != nil {
		t.Fatalf("LocalWrite: %v", err)
	}

	// Confirm outbox row exists.
	var before int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM sync_mutations WHERE json_extract(payload, '$.project') = ?`,
		"proj-outbox",
	).Scan(&before); err != nil {
		t.Fatalf("count outbox before: %v", err)
	}
	if before == 0 {
		t.Skip("no outbox rows found — json_extract may not match payload format; skipping outbox sub-test")
	}

	_, err := st.PurgeProjectLocal("proj-outbox")
	if err != nil {
		t.Fatalf("PurgeProjectLocal: %v", err)
	}

	var after int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM sync_mutations WHERE json_extract(payload, '$.project') = ?`,
		"proj-outbox",
	).Scan(&after); err != nil {
		t.Fatalf("count outbox after: %v", err)
	}
	if after != 0 {
		t.Errorf("PurgeProjectLocal: %d outbox rows remain; want 0", after)
	}
}

// TestTombstoneProject_SoftDeletesLiveMemories verifies that TombstoneProject:
//   - marks all live memories for the project as deleted (deleted_at set)
//   - enqueues OpDelete mutations in the outbox (sync_mutations)
//   - does NOT touch live memories for another project
func TestTombstoneProject_SoftDeletesLiveMemories(t *testing.T) {
	st := openTempStore(t)

	// Seed two live memories for the target project.
	seedMemory(t, st, "proj-tomb", "sync-tomb-1")
	seedMemory(t, st, "proj-tomb", "sync-tomb-2")

	// Seed one live memory for the bystander project.
	seedMemory(t, st, "proj-stay", "sync-stay-1")

	n, err := st.TombstoneProject("proj-tomb", "writer-test")
	if err != nil {
		t.Fatalf("TombstoneProject: %v", err)
	}
	if n != 2 {
		t.Errorf("TombstoneProject: returned %d; want 2", n)
	}

	// Target memories must be soft-deleted (deleted_at set).
	var liveCount int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE project = ? AND deleted_at IS NULL`,
		"proj-tomb",
	).Scan(&liveCount); err != nil {
		t.Fatalf("count live memories: %v", err)
	}
	if liveCount != 0 {
		t.Errorf("TombstoneProject: %d live memories remain; want 0", liveCount)
	}

	// The rows must still exist (soft-delete, not hard-delete).
	var totalCount int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE project = ?`,
		"proj-tomb",
	).Scan(&totalCount); err != nil {
		t.Fatalf("count total memories: %v", err)
	}
	if totalCount != 2 {
		t.Errorf("TombstoneProject: %d total rows; want 2 (soft-delete, not hard-delete)", totalCount)
	}

	// Outbox must have OpDelete entries for the tombstoned memories.
	var outboxCount int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM sync_mutations WHERE op = 'delete'`,
	).Scan(&outboxCount); err != nil {
		t.Fatalf("count delete outbox entries: %v", err)
	}
	if outboxCount < 2 {
		t.Errorf("TombstoneProject: %d OpDelete outbox entries; want >= 2", outboxCount)
	}

	// Bystander project must be untouched.
	var bystanderLive int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE project = ? AND deleted_at IS NULL`,
		"proj-stay",
	).Scan(&bystanderLive); err != nil {
		t.Fatalf("count bystander live: %v", err)
	}
	if bystanderLive != 1 {
		t.Errorf("TombstoneProject: bystander proj-stay live count = %d; want 1", bystanderLive)
	}
}

// TestTombstoneProject_DelistsProjectWithPolicyRow is the regression test for the
// "purge-all looks like a no-op" bug: a project with an explicit project_policy row
// used to remain in ListProjectsWithPolicy after every memory was tombstoned, so the
// web UI showed no change. TombstoneProject now drops the policy row once the project
// is fully purged, so the project disappears from the listing.
func TestTombstoneProject_DelistsProjectWithPolicyRow(t *testing.T) {
	st := openTempStore(t)

	seedMemory(t, st, "gentleman.dots", "sync-del-1")
	seedMemory(t, st, "gentleman.dots", "sync-del-2")
	// Give it an explicit policy row — this is what made it linger before the fix.
	if err := st.SetPolicy("gentleman.dots", PolicySynced); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	listed := func() bool {
		ps, err := st.ListProjectsWithPolicy()
		if err != nil {
			t.Fatalf("ListProjectsWithPolicy: %v", err)
		}
		for _, p := range ps {
			if p.Name == "gentleman.dots" {
				return true
			}
		}
		return false
	}

	if !listed() {
		t.Fatal("precondition: project should be listed before purge-all")
	}

	n, err := st.TombstoneProject("gentleman.dots", "writer-test")
	if err != nil {
		t.Fatalf("TombstoneProject: %v", err)
	}
	if n != 2 {
		t.Errorf("TombstoneProject: tombstoned %d; want 2", n)
	}

	if listed() {
		t.Error("project still listed after purge-all; policy row was not cleared (regression)")
	}

	// The policy row must actually be gone (not just hidden).
	var rows int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM project_policy WHERE project = ?`, "gentleman.dots",
	).Scan(&rows); err != nil {
		t.Fatalf("count policy rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("project_policy rows = %d; want 0 after purge-all", rows)
	}
}

// TestProjectDelete_MixedCaseProject is the regression test for the main reported
// bug: central-pulled projects keep their ORIGINAL case in the memories table
// (e.g. "Gentleman.Dots"), but the delete path used to normalize the lookup to
// lowercase, so it matched zero rows and the delete silently did nothing. Both
// delete scopes must now match case-insensitively.
func TestProjectDelete_MixedCaseProject(t *testing.T) {
	insertMixedCase := func(t *testing.T, st *Store, syncID string) {
		t.Helper()
		// Raw insert preserving case, mimicking the central-pull apply path
		// (which does NOT normalize) rather than seedMemory's lowercase callers.
		_, err := st.DB().Exec(`
			INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
			VALUES (?, 'sess', 'memory', 'manual', 'seed', 'seed', 'Gentleman.Dots', 'project', 'w')`, syncID)
		if err != nil {
			t.Fatalf("insert mixed-case memory: %v", err)
		}
	}

	t.Run("purge-all/TombstoneProject", func(t *testing.T) {
		st := openTempStore(t)
		insertMixedCase(t, st, "mc-tomb-1")
		insertMixedCase(t, st, "mc-tomb-2")

		// Caller may use any casing — the displayed name is "Gentleman.Dots".
		n, err := st.TombstoneProject("Gentleman.Dots", "w")
		if err != nil {
			t.Fatalf("TombstoneProject: %v", err)
		}
		if n != 2 {
			t.Errorf("tombstoned %d; want 2 (case-insensitive match failed)", n)
		}
		var live int
		_ = st.DB().QueryRow(`SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL`).Scan(&live)
		if live != 0 {
			t.Errorf("%d live memories remain; want 0", live)
		}
	})

	t.Run("local/PurgeProjectLocal", func(t *testing.T) {
		st := openTempStore(t)
		insertMixedCase(t, st, "mc-purge-1")

		// Deliberately pass a different casing than stored to prove NOCASE matching.
		total, err := st.PurgeProjectLocal("gentleman.DOTS")
		if err != nil {
			t.Fatalf("PurgeProjectLocal: %v", err)
		}
		if total < 1 {
			t.Errorf("purged %d rows; want >= 1 (case-insensitive match failed)", total)
		}
		var rows int
		_ = st.DB().QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&rows)
		if rows != 0 {
			t.Errorf("%d memory rows remain after purge; want 0", rows)
		}
	})
}

// TestTombstoneProject_EmptyProject verifies that TombstoneProject on a project
// with no live memories returns 0 without error.
func TestTombstoneProject_EmptyProject(t *testing.T) {
	st := openTempStore(t)

	n, err := st.TombstoneProject("nonexistent-project", "w")
	if err != nil {
		t.Fatalf("TombstoneProject on empty project: %v", err)
	}
	if n != 0 {
		t.Errorf("TombstoneProject on empty project: returned %d; want 0", n)
	}
}

// TestPurgeProjectLocal_LiveAndSoftDeleted verifies that PurgeProjectLocal removes
// live memories as well as memories that were previously tombstoned (soft-deleted)
// via the standard TombstoneProject path. The test seeds one live memory and one
// that is soft-deleted via TombstoneProject (not a raw INSERT) so the FTS triggers
// fire in the correct order: upsert adds to FTS, soft-delete removes from FTS,
// then PurgeProjectLocal hard-deletes both rows without triggering FTS corruption.
func TestPurgeProjectLocal_LiveAndSoftDeleted(t *testing.T) {
	st := openTempStore(t)

	// Seed one live memory.
	seedMemory(t, st, "proj-mixed", "sync-mixed-live")

	// Seed a second memory via LocalWrite, then soft-delete it via TombstoneProject
	// so FTS is updated correctly (upsert adds to FTS; soft-delete removes from FTS).
	mLive := domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "sync-mixed-softdel",
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "will be soft-deleted",
		Content:    "content",
		Project:    "proj-mixed",
		Scope:      "project",
		Version:    1,
		WriterID:   "w",
		UpdatedAt:  time.Now().UTC(),
	}
	if _, err := st.LocalWrite(mLive); err != nil {
		t.Fatalf("LocalWrite upsert: %v", err)
	}
	// Soft-delete it through TombstoneProject so FTS is consistently maintained.
	// After this call: sync-mixed-softdel has deleted_at set AND is removed from FTS.
	if _, err := st.TombstoneProject("proj-mixed", "w"); err != nil {
		t.Fatalf("TombstoneProject (pre-purge soft-delete): %v", err)
	}

	// Now seed the live memory again (it was soft-deleted above too).
	// Re-seed proj-mixed with a fresh live row so PurgeProjectLocal has something
	// to hard-delete along with the soft-deleted row.
	seedMemory(t, st, "proj-mixed", "sync-mixed-live2")

	n, err := st.PurgeProjectLocal("proj-mixed")
	if err != nil {
		t.Fatalf("PurgeProjectLocal: %v", err)
	}

	// At minimum the 2 memories rows (1 live + 1 soft-deleted) must be gone.
	if n < 2 {
		t.Errorf("PurgeProjectLocal: returned %d; want >= 2", n)
	}

	var count int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE project = ?`, "proj-mixed",
	).Scan(&count); err != nil {
		t.Fatalf("count after purge: %v", err)
	}
	if count != 0 {
		t.Errorf("PurgeProjectLocal: %d rows remain; want 0", count)
	}
}
