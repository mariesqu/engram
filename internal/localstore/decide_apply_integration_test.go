package localstore

// Real-SQLite integration tests for the Decide→Apply contract.
//
// These tests drive domain.Decide against the real Store (not a mock) and then
// call Apply with the returned Decision.  They prove two P1 invariants that the
// mock-only unit tests cannot catch:
//
//   INV1 (topic convergence)  — P1-a: Apply MUST update the row with sync_id Y
//     that Decide resolved via FindByTopic, NOT the incoming sync_id X, so the
//     newer write is never silently lost.
//
//   INV4 (tombstone undelete) — P1-b: when a write supersedes a tombstone,
//     Apply MUST clear deleted_at on the memories row AND delete the
//     memory_tombstones row so the revived record becomes live.
//
// Both tests are written FIRST — they fail (RED) before the Decision contract
// and adapter changes are in place, confirming the bugs are real.

import (
	"database/sql"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// baseT is a reference instant for test timestamps.
var baseT = time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

// ─────────────────────────────────────────────────────────────────────────────
// TestINV1_TopicConvergence_RealSQLite (P1-a)
//
// Scenario: machine-A writes topic T with sync_id "sync-Y" at T+50 (older).
//           machine-B then applies a mutation for the same topic T with
//           sync_id "sync-X" at T+100 (newer).
//
//   Decide should return Decision{Action: ActionUpdate, TargetSyncID: "sync-Y"}.
//   Apply must UPDATE memories WHERE sync_id = "sync-Y" (NOT "sync-X").
//
// After apply there MUST be exactly ONE row for topic T, it MUST hold B's
// content, and sync_id "sync-Y" MUST be updated in place (no second row).
//
// This FAILS with the bare-ActionUpdate adapter: execUpdate uses
//   WHERE sync_id = m.SyncID ("sync-X") → 0 rows updated → B's write is LOST.
// ─────────────────────────────────────────────────────────────────────────────
func TestINV1_TopicConvergence_RealSQLite(t *testing.T) {
	s := openTempStore(t)
	project, scope := "engram", "project"
	topic := "sdd/test/topic-convergence"

	// ── Seed A's row (older, already in DB) ──────────────────────────────────
	syncY := "sync-Y"
	tOlder := baseT.Add(50 * time.Second)
	_, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content,
		   project, scope, topic_key, version, seq, writer_id, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		syncY, "sess-A", "memory", "manual", "A title", "A content",
		project, scope, topic, 1, 1, "writer-A",
		tOlder.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		t.Fatalf("seed A row: %v", err)
	}

	// ── Build B's mutation (newer, distinct sync_id) ──────────────────────────
	syncX := "sync-X"
	tNewer := baseT.Add(100 * time.Second)
	tk := topic
	mutB := domain.Mutation{
		MutationID: "mut-B",
		Op:         domain.OpUpsert,
		SyncID:     syncX,
		SessionID:  "sess-B",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "B title",
		Content:    "B content — should win",
		Project:    project,
		Scope:      scope,
		TopicKey:   &tk,
		Version:    2,
		Seq:        2,
		UpdatedAt:  tNewer,
		OccurredAt: tNewer,
		WriterID:   "writer-B",
	}

	// ── Decide + Apply ────────────────────────────────────────────────────────
	d := domain.Decide(s, mutB)
	if d.Action != domain.ActionUpdate {
		t.Fatalf("Decide: want ActionUpdate (topic resolved to sync-Y); got %v", d.Action)
	}
	if d.TargetSyncID != syncY {
		t.Fatalf("Decide: want TargetSyncID=%q; got %q", syncY, d.TargetSyncID)
	}

	if err := Apply(s.db, d, mutB); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// ── Assertions ────────────────────────────────────────────────────────────

	// 1. Exactly one row for this topic.
	var rowCount int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memories WHERE topic_key=? AND project=? AND scope=?`,
		topic, project, scope,
	).Scan(&rowCount); err != nil {
		t.Fatalf("count rows for topic: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("INV1: expected exactly 1 row for topic %q; got %d (B's write may have inserted a duplicate or was lost)", topic, rowCount)
	}

	// 2. The surviving row holds B's content (newer write wins).
	var content, syncID string
	if err := s.db.QueryRow(
		`SELECT content, sync_id FROM memories WHERE topic_key=? AND project=? AND scope=?`,
		topic, project, scope,
	).Scan(&content, &syncID); err != nil {
		t.Fatalf("query surviving row: %v", err)
	}
	if content != mutB.Content {
		t.Errorf("INV1: surviving row content = %q; want %q (B's content). sync_id=%q",
			content, mutB.Content, syncID)
	}

	// 3. The row is the ORIGINAL row (sync_id Y, updated in-place — not a new row with sync_id X).
	if syncID != syncY {
		t.Errorf("INV1: surviving row sync_id = %q; want %q (should update in-place, not insert new)", syncID, syncY)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestINV4_TombstoneUndelete_RealSQLite (P1-b)
//
// Scenario: a record was soft-deleted (deleted_at set, tombstone row written).
//           A strictly-newer upsert for the same identity then arrives through
//           Decide→Apply.
//
//   Decide should return Decision{Action: ActionInsert, Undelete: true}.
//   Apply must:
//     • Clear deleted_at on the memories row (SET deleted_at = NULL)
//     • DELETE the memory_tombstones row
//
// After apply the row MUST be LIVE (deleted_at IS NULL), the tombstone row MUST
// be GONE, and FindByTopic / SearchMemories MUST return it.
//
// This FAILS with the current adapter: ActionInsert blindly INSERTs a new row
// (which hits UNIQUE constraint on sync_id) and never clears the tombstone,
// leaving the record invisible.  (With a bare-ActionUpdate it would hit 0 rows.)
// ─────────────────────────────────────────────────────────────────────────────
func TestINV4_TombstoneUndelete_RealSQLite(t *testing.T) {
	s := openTempStore(t)
	project, scope := "engram", "project"
	syncID := "sync-undelete"
	topic := "sdd/test/undelete"

	tDeleted := baseT.Add(50 * time.Second)
	tRevived := baseT.Add(100 * time.Second)
	deletedAtStr := tDeleted.UTC().Format(time.RFC3339Nano)

	// ── Seed the soft-deleted row ─────────────────────────────────────────────
	tk := topic
	_, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content,
		   project, scope, topic_key, version, seq, writer_id, updated_at, deleted_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		syncID, "sess-A", "memory", "manual", "A title", "A content",
		project, scope, topic, 1, 1, "writer-A",
		deletedAtStr, deletedAtStr,
	)
	if err != nil {
		t.Fatalf("seed deleted row: %v", err)
	}

	// ── Seed the tombstone row ────────────────────────────────────────────────
	_, err = s.db.Exec(`
		INSERT INTO memory_tombstones
		  (sync_id, project, scope, topic_key, deleted_at, deleted_by, version)
		VALUES (?,?,?,?,?,?,?)`,
		syncID, project, scope, topic, deletedAtStr, "writer-A", 1,
	)
	if err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}

	// ── Build the superseding mutation (strictly newer) ───────────────────────
	mut := domain.Mutation{
		MutationID: "mut-revive",
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess-B",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Revived title",
		Content:    "Revived content — should be live",
		Project:    project,
		Scope:      scope,
		TopicKey:   &tk,
		Version:    2,
		Seq:        5,
		UpdatedAt:  tRevived,
		OccurredAt: tRevived,
		WriterID:   "writer-B",
	}

	// ── Decide ────────────────────────────────────────────────────────────────
	d := domain.Decide(s, mut)
	if d.Action != domain.ActionInsert && d.Action != domain.ActionUpdate {
		t.Fatalf("Decide: want ActionInsert or ActionUpdate for tombstone-supersede; got %v", d.Action)
	}
	if !d.Undelete {
		t.Errorf("Decide: want Undelete=true for tombstone-supersede; got false")
	}

	// ── Apply ─────────────────────────────────────────────────────────────────
	if err := Apply(s.db, d, mut); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// ── Assertions ────────────────────────────────────────────────────────────

	// 1. Row is LIVE (deleted_at IS NULL).
	var deletedAt sql.NullString
	if err := s.db.QueryRow(
		`SELECT deleted_at FROM memories WHERE sync_id=?`, syncID,
	).Scan(&deletedAt); err != nil {
		t.Fatalf("query deleted_at after undelete: %v", err)
	}
	if deletedAt.Valid {
		t.Errorf("INV4: row still has deleted_at=%q after undelete; expected NULL", deletedAt.String)
	}

	// 2. Tombstone row is GONE.
	var tombCount int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_tombstones WHERE sync_id=?`, syncID,
	).Scan(&tombCount); err != nil {
		t.Fatalf("count tombstones: %v", err)
	}
	if tombCount != 0 {
		t.Errorf("INV4: tombstone row still exists after undelete; expected 0 rows")
	}

	// 3. FindByTopic returns the record (it is now live).
	rec, err := s.FindByTopic(topic, project, scope)
	if err != nil {
		t.Fatalf("FindByTopic after undelete: %v", err)
	}
	if rec == nil {
		t.Fatal("INV4: FindByTopic returned nil after undelete; expected live record")
	}
	if rec.Content != mut.Content {
		t.Errorf("INV4: revived record content = %q; want %q", rec.Content, mut.Content)
	}

	// 4. SearchMemories finds it (FTS index reflects the undelete).
	results, err := s.SearchMemories("Revived", project, 10)
	if err != nil {
		t.Fatalf("SearchMemories after undelete: %v", err)
	}
	found := false
	for _, r := range results {
		if r.SyncID == syncID {
			found = true
		}
	}
	if !found {
		t.Errorf("INV4: revived record not returned by SearchMemories (FTS not updated?)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestApply_NormalSameSyncIDUpdate_StillWorks (regression)
//
// A normal update where m.SyncID == the stored row's sync_id (no topic-based
// resolution) must still work correctly after the Decision contract change.
// ─────────────────────────────────────────────────────────────────────────────
func TestApply_NormalSameSyncIDUpdate_StillWorks(t *testing.T) {
	s := openTempStore(t)
	project, scope := "engram", "project"
	syncID := "sync-same"

	tOld := baseT
	_, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content,
		   project, scope, version, seq, writer_id, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		syncID, "sess-A", "memory", "manual", "Old", "Old content",
		project, scope, 1, 1, "writer-A",
		tOld.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	tNew := baseT.Add(100 * time.Second)
	mut := domain.Mutation{
		MutationID: "mut-same",
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess-A",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "New",
		Content:    "New content",
		Project:    project,
		Scope:      scope,
		Version:    2,
		Seq:        2,
		UpdatedAt:  tNew,
		OccurredAt: tNew,
		WriterID:   "writer-A",
	}

	d := domain.Decide(s, mut)
	if d.Action != domain.ActionUpdate {
		t.Fatalf("Decide: want ActionUpdate; got %v", d.Action)
	}
	if d.TargetSyncID != syncID {
		t.Fatalf("Decide: want TargetSyncID=%q; got %q", syncID, d.TargetSyncID)
	}

	if err := Apply(s.db, d, mut); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var content string
	if err := s.db.QueryRow(
		`SELECT content FROM memories WHERE sync_id=?`, syncID,
	).Scan(&content); err != nil {
		t.Fatalf("query after update: %v", err)
	}
	if content != "New content" {
		t.Errorf("regression: content = %q; want 'New content'", content)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestINV4_TombstoneOnly_UpsertCreatesRow (Bug A)
//
// Scenario: a DELETE mutation arrives for a sync_id that has NO memories row
// (this can happen when execWriteTombstone runs on a sync_id that was never in
// the local store — the UPDATE hits 0 rows but the tombstone INSERT always runs).
// State: memory_tombstones has a row for sync_id; memories has NO row.
//
// A strictly-newer UPSERT for that same sync_id then arrives through Decide→Apply.
//
//   Decide should return Decision{Action: ActionInsert, Undelete: true}
//     (cur == nil because no memories row exists; tombstoneSuperseded == true).
//   Apply must:
//     • INSERT the new memories row (NOT update 0 rows)
//     • DELETE the memory_tombstones row (tombstone cleared)
//
// After apply:
//   • memories row EXISTS with the new content (deleted_at IS NULL)
//   • memory_tombstones row is GONE
//   • FindBySyncID and FindByTopic return the live record
//
// Before the fix, execUndeleteUpdate runs an UPDATE on a non-existent row →
// 0 rows updated → the INSERT is silently dropped while the tombstone is cleared.
// ─────────────────────────────────────────────────────────────────────────────
func TestINV4_TombstoneOnly_UpsertCreatesRow(t *testing.T) {
	s := openTempStore(t)
	project, scope := "engram", "project"
	syncID := "sync-tombonly"
	topic := "sdd/test/tombstone-only"

	tDeleted := baseT.Add(50 * time.Second)
	tRevived := baseT.Add(100 * time.Second)
	deletedAtStr := tDeleted.UTC().Format(time.RFC3339Nano)

	// ── Seed ONLY the tombstone row — no memories row exists ─────────────────
	tk := topic
	_, err := s.db.Exec(`
		INSERT INTO memory_tombstones
		  (sync_id, project, scope, topic_key, deleted_at, deleted_by, version)
		VALUES (?,?,?,?,?,?,?)`,
		syncID, project, scope, topic, deletedAtStr, "writer-A", 1,
	)
	if err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}

	// Confirm no memories row exists for this sync_id.
	var memCount int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memories WHERE sync_id=?`, syncID,
	).Scan(&memCount); err != nil {
		t.Fatalf("count memories before: %v", err)
	}
	if memCount != 0 {
		t.Fatalf("precondition: expected 0 memories rows, got %d", memCount)
	}

	// ── Build the superseding mutation (strictly newer) ───────────────────────
	mut := domain.Mutation{
		MutationID: "mut-tombonly-revive",
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess-B",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "TombstoneOnly Revived",
		Content:    "Created from tombstone-only state",
		Project:    project,
		Scope:      scope,
		TopicKey:   &tk,
		Version:    2,
		Seq:        5,
		UpdatedAt:  tRevived,
		OccurredAt: tRevived,
		WriterID:   "writer-B",
	}

	// ── Decide ────────────────────────────────────────────────────────────────
	d := domain.Decide(s, mut)
	if d.Action != domain.ActionInsert {
		t.Fatalf("Decide: want ActionInsert (cur==nil); got %v", d.Action)
	}
	if !d.Undelete {
		t.Errorf("Decide: want Undelete=true (tombstone superseded); got false")
	}

	// ── Apply ─────────────────────────────────────────────────────────────────
	if err := Apply(s.db, d, mut); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// ── Assertions ────────────────────────────────────────────────────────────

	// 1. memories row EXISTS and is LIVE (deleted_at IS NULL).
	var deletedAt sql.NullString
	var content string
	err = s.db.QueryRow(
		`SELECT deleted_at, content FROM memories WHERE sync_id=?`, syncID,
	).Scan(&deletedAt, &content)
	if err == sql.ErrNoRows {
		t.Fatal("BUG A: memories row does not exist after ActionInsert+Undelete (write was lost)")
	}
	if err != nil {
		t.Fatalf("query memories after apply: %v", err)
	}
	if deletedAt.Valid {
		t.Errorf("BUG A: row still has deleted_at=%q after apply; expected NULL", deletedAt.String)
	}
	if content != mut.Content {
		t.Errorf("BUG A: row content = %q; want %q", content, mut.Content)
	}

	// 2. Tombstone row is GONE.
	var tombCount int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_tombstones WHERE sync_id=?`, syncID,
	).Scan(&tombCount); err != nil {
		t.Fatalf("count tombstones: %v", err)
	}
	if tombCount != 0 {
		t.Errorf("BUG A: tombstone row still exists after apply; expected 0 rows")
	}

	// 3. FindBySyncID returns the live record.
	rec, err := s.FindBySyncID(syncID)
	if err != nil {
		t.Fatalf("FindBySyncID after apply: %v", err)
	}
	if rec == nil {
		t.Fatal("BUG A: FindBySyncID returned nil after ActionInsert+Undelete")
	}
	if rec.Content != mut.Content {
		t.Errorf("BUG A: FindBySyncID content = %q; want %q", rec.Content, mut.Content)
	}

	// 4. FindByTopic also returns it.
	rec2, err := s.FindByTopic(topic, project, scope)
	if err != nil {
		t.Fatalf("FindByTopic after apply: %v", err)
	}
	if rec2 == nil {
		t.Fatal("BUG A: FindByTopic returned nil after ActionInsert+Undelete")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestINV4_CrossWriterDelete_ConvergesToResolvedRow
//
// Scenario: machine-A writes topic T with sync_id "sync-Y" (writer-A).
//           machine-C sends a DELETE for the same topic_key T but with a
//           DIFFERENT sync_id "sync-Z" (writer-C — has never written this topic
//           locally, but intends to delete it).
//
//   Decide resolves the live row via FindByTopic → cur.SyncID = "sync-Y".
//   Decision must be Decision{Action: ActionWriteTombstone, TargetSyncID: "sync-Y"}.
//   Apply must:
//     • UPDATE memories SET deleted_at=... WHERE sync_id="sync-Y"  (NOT "sync-Z")
//     • INSERT tombstone row with sync_id="sync-Y" + topic_key=T
//
// After apply:
//   • Row "sync-Y" is soft-deleted (deleted_at IS NOT NULL)
//   • FindByTopic returns nil (row is invisible to live lookups)
//   • Tombstone exists for sync_id="sync-Y" AND for topic_key=T
//   • No spurious row "sync-Z" is created in memories
//
// Before the fix, execWriteTombstone uses m.SyncID ("sync-Z") for the UPDATE →
// 0 rows affected → row "sync-Y" stays VISIBLE. A spurious tombstone for "sync-Z"
// is written, but the live topic row is never deleted.
// ─────────────────────────────────────────────────────────────────────────────
func TestINV4_CrossWriterDelete_ConvergesToResolvedRow(t *testing.T) {
	s := openTempStore(t)
	project, scope := "engram", "project"
	topic := "sdd/test/cross-writer-delete"
	syncY := "sync-Y"
	syncZ := "sync-Z"

	tWritten := baseT.Add(50 * time.Second)
	tDeleted := baseT.Add(100 * time.Second)

	// ── Seed A's live row (topic T, sync_id Y) ───────────────────────────────
	tk := topic
	_, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content,
		   project, scope, topic_key, version, seq, writer_id, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		syncY, "sess-A", "memory", "manual", "A title", "A content",
		project, scope, topic, 1, 1, "writer-A",
		tWritten.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		t.Fatalf("seed row Y: %v", err)
	}

	// ── Build DELETE mutation from writer-C with a different sync_id (Z) ─────
	mutDel := domain.Mutation{
		MutationID: "mut-del-cross",
		Op:         domain.OpDelete,
		SyncID:     syncZ,
		SessionID:  "sess-C",
		EntityType: domain.EntityMemory,
		Project:    project,
		Scope:      scope,
		TopicKey:   &tk,
		Version:    2,
		Seq:        5,
		UpdatedAt:  tDeleted,
		OccurredAt: tDeleted,
		WriterID:   "writer-C",
	}

	// ── Decide ────────────────────────────────────────────────────────────────
	d := domain.Decide(s, mutDel)
	if d.Action != domain.ActionWriteTombstone {
		t.Fatalf("Decide: want ActionWriteTombstone for cross-writer delete; got %v", d.Action)
	}
	// TargetSyncID must be the RESOLVED row's sync_id (Y), NOT the mutation's own sync_id (Z).
	if d.TargetSyncID != syncY {
		t.Fatalf("Decide: want TargetSyncID=%q (resolved row); got %q (should not be m.SyncID=%q)",
			syncY, d.TargetSyncID, syncZ)
	}

	// ── Apply ─────────────────────────────────────────────────────────────────
	if err := Apply(s.db, d, mutDel); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// ── Assertions ────────────────────────────────────────────────────────────

	// 1. Row Y is soft-deleted (deleted_at IS NOT NULL).
	var deletedAt sql.NullString
	err = s.db.QueryRow(
		`SELECT deleted_at FROM memories WHERE sync_id=?`, syncY,
	).Scan(&deletedAt)
	if err == sql.ErrNoRows {
		t.Fatal("BUG: row Y disappeared entirely — expected soft-delete (deleted_at set)")
	}
	if err != nil {
		t.Fatalf("query row Y deleted_at: %v", err)
	}
	if !deletedAt.Valid {
		t.Error("BUG: row Y still has deleted_at=NULL after cross-writer delete — row stays VISIBLE (should be soft-deleted)")
	}

	// 2. FindByTopic returns nil (topic is invisible to live lookups).
	rec, err := s.FindByTopic(topic, project, scope)
	if err != nil {
		t.Fatalf("FindByTopic after delete: %v", err)
	}
	if rec != nil {
		t.Errorf("BUG: FindByTopic returned live record after delete (sync_id=%q); expected nil", rec.SyncID)
	}

	// 3. Tombstone covers the RESOLVED row Y (by sync_id=Y).
	var tombSyncID string
	err = s.db.QueryRow(
		`SELECT sync_id FROM memory_tombstones WHERE sync_id=?`, syncY,
	).Scan(&tombSyncID)
	if err == sql.ErrNoRows {
		t.Errorf("BUG: no tombstone found for sync_id=%q (should cover the resolved row Y)", syncY)
	} else if err != nil {
		t.Fatalf("query tombstone by sync_id Y: %v", err)
	}

	// 4. Tombstone is also findable by topic_key (covers the topic identity).
	var tombByTopic sql.NullString
	err = s.db.QueryRow(
		`SELECT sync_id FROM memory_tombstones WHERE topic_key=? AND project=? AND scope=?`,
		topic, project, scope,
	).Scan(&tombByTopic)
	if err == sql.ErrNoRows {
		t.Errorf("BUG: no tombstone found for topic_key=%q (should cover the topic identity)", topic)
	} else if err != nil {
		t.Fatalf("query tombstone by topic_key: %v", err)
	}

	// 5. No spurious row Z created in memories.
	var zCount int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memories WHERE sync_id=?`, syncZ,
	).Scan(&zCount); err != nil {
		t.Fatalf("count memories for sync_id Z: %v", err)
	}
	if zCount != 0 {
		t.Errorf("spurious row created for m.SyncID=%q (DELETE should not insert a new row)", syncZ)
	}
}

// ═════════════════════════════════════════════════════════════════════════════
// STATE-SPACE HARDENING — cross-writer tombstone identity convergence.
//
// The tests below drive Decide→Apply against a real store across multi-step
// cross-writer sequences and assert the two structural invariants directly:
//
//   INV-A: at most ONE live (deleted_at IS NULL) row per (topic_key,project,scope).
//   INV-B: at most ONE tombstone row per topic identity (no duplicate tombstones).
//
// Helper assertions count rows so the proofs are unambiguous.
// ═════════════════════════════════════════════════════════════════════════════

// countLiveRowsForTopic returns the number of live (deleted_at IS NULL) rows for
// the given topic identity.
func countLiveRowsForTopic(t *testing.T, s *Store, topic, project, scope string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memories
		   WHERE topic_key=? AND project=? AND scope=? AND deleted_at IS NULL`,
		topic, project, scope,
	).Scan(&n); err != nil {
		t.Fatalf("countLiveRowsForTopic: %v", err)
	}
	return n
}

// countRowsForTopic returns the total number of rows (live + soft-deleted) for the
// given topic identity.
func countRowsForTopic(t *testing.T, s *Store, topic, project, scope string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memories WHERE topic_key=? AND project=? AND scope=?`,
		topic, project, scope,
	).Scan(&n); err != nil {
		t.Fatalf("countRowsForTopic: %v", err)
	}
	return n
}

// countTombstonesForTopic returns the number of tombstone rows that cover the
// given topic identity (by topic_key).
func countTombstonesForTopic(t *testing.T, s *Store, topic, project, scope string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_tombstones WHERE topic_key=? AND project=? AND scope=?`,
		topic, project, scope,
	).Scan(&n); err != nil {
		t.Fatalf("countTombstonesForTopic: %v", err)
	}
	return n
}

// writeTopic seeds a fresh live row for a topic via Decide→Apply (OpUpsert).
func writeTopic(t *testing.T, s *Store, mutID, syncID, topic, project, scope, content string, version int, seq int64, at time.Time) {
	t.Helper()
	tk := topic
	m := domain.Mutation{
		MutationID: mutID,
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "title",
		Content:    content,
		Project:    project,
		Scope:      scope,
		TopicKey:   &tk,
		Version:    version,
		Seq:        seq,
		UpdatedAt:  at,
		OccurredAt: at,
		WriterID:   "writer-" + syncID,
	}
	d := domain.Decide(s, m)
	if err := Apply(s.db, d, m); err != nil {
		t.Fatalf("writeTopic(%s): Apply: %v", syncID, err)
	}
}

// deleteTopic issues a cross-writer DELETE for a topic via Decide→Apply.
func deleteTopic(t *testing.T, s *Store, mutID, syncID, topic, project, scope string, version int, seq int64, at time.Time) domain.Decision {
	t.Helper()
	tk := topic
	m := domain.Mutation{
		MutationID: mutID,
		Op:         domain.OpDelete,
		SyncID:     syncID,
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Project:    project,
		Scope:      scope,
		TopicKey:   &tk,
		Version:    version,
		Seq:        seq,
		UpdatedAt:  at,
		OccurredAt: at,
		WriterID:   "writer-" + syncID,
	}
	d := domain.Decide(s, m)
	if err := Apply(s.db, d, m); err != nil {
		t.Fatalf("deleteTopic(%s): Apply: %v", syncID, err)
	}
	return d
}

// ─────────────────────────────────────────────────────────────────────────────
// TestINV_CrossWriterReDelete_SingleTombstone (Codex confirmed bug — RED before)
//
// Sequence (three distinct writers, one topic):
//   1. write topic T under sync_id Y  (writer-Y)
//   2. delete T via sync_id Z         (writer-Z) → tombstones the canonical row Y
//   3. delete T AGAIN via sync_id W   (writer-W) → must RE-tombstone the SAME
//                                       identity Y, not mint a second tombstone.
//
// After the sequence:
//   • exactly ONE tombstone covers topic T (INV-B) — keyed by Y.
//   • zero live rows for T (INV-A).
//
// BEFORE THE FIX: step 3 has cur==nil (Y is soft-deleted, invisible to the
// live-only FindByTopic; FindBySyncID(W) misses). The OpDelete branch then uses
// target = m.SyncID = W and execWriteTombstone INSERTs a SECOND tombstone (PK W).
// → TWO tombstones for topic T; FindTombstone-by-topic (LIMIT 1, no ORDER BY) is
// then non-deterministic. This test asserts exactly 1 tombstone → RED before.
// ─────────────────────────────────────────────────────────────────────────────
func TestINV_CrossWriterReDelete_SingleTombstone(t *testing.T) {
	s := openTempStore(t)
	project, scope := "engram", "project"
	topic := "sdd/test/cross-writer-redelete"

	syncY, syncZ, syncW := "sync-Y", "sync-Z", "sync-W"
	tWrite := baseT.Add(10 * time.Second)
	tDel1 := baseT.Add(50 * time.Second)
	tDel2 := baseT.Add(90 * time.Second)

	// 1. write T under Y
	writeTopic(t, s, "mut-write", syncY, topic, project, scope, "Y content", 1, 1, tWrite)

	// 2. delete T via Z (cross-writer) → tombstones canonical row Y
	deleteTopic(t, s, "mut-del-z", syncZ, topic, project, scope, 2, 2, tDel1)

	// 3. delete T again via W (cross-writer) → must re-tombstone Y, not mint W
	dW := deleteTopic(t, s, "mut-del-w", syncW, topic, project, scope, 3, 3, tDel2)

	// Decide for the second delete must resolve to the existing identity Y.
	if dW.Action != domain.ActionWriteTombstone {
		t.Fatalf("re-delete: want ActionWriteTombstone; got %v", dW.Action)
	}
	if dW.TargetSyncID != syncY {
		t.Errorf("re-delete: TargetSyncID = %q; want %q (reuse existing tombstone identity, not mint W)",
			dW.TargetSyncID, syncY)
	}

	// INV-B: exactly ONE tombstone for the topic.
	if got := countTombstonesForTopic(t, s, topic, project, scope); got != 1 {
		t.Errorf("INV-B: tombstones for topic %q = %d; want exactly 1 (duplicate tombstone minted)", topic, got)
	}

	// The single tombstone must be keyed by Y (the canonical identity).
	var tombSync string
	if err := s.db.QueryRow(
		`SELECT sync_id FROM memory_tombstones WHERE topic_key=? AND project=? AND scope=?`,
		topic, project, scope,
	).Scan(&tombSync); err != nil {
		t.Fatalf("query tombstone sync_id: %v", err)
	}
	if tombSync != syncY {
		t.Errorf("tombstone sync_id = %q; want %q (canonical identity)", tombSync, syncY)
	}

	// No spurious memories row for W or Z.
	for _, sid := range []string{syncZ, syncW} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM memories WHERE sync_id=?`, sid).Scan(&n); err != nil {
			t.Fatalf("count memories for %s: %v", sid, err)
		}
		if n != 0 {
			t.Errorf("spurious memories row for %q; want 0", sid)
		}
	}

	// INV-A: zero live rows for the topic.
	if got := countLiveRowsForTopic(t, s, topic, project, scope); got != 0 {
		t.Errorf("INV-A: live rows for topic %q = %d; want 0 (all deletes)", topic, got)
	}

	// FindByTopic returns nil (topic invisible to live lookups).
	if rec, err := s.FindByTopic(topic, project, scope); err != nil {
		t.Fatalf("FindByTopic: %v", err)
	} else if rec != nil {
		t.Errorf("FindByTopic returned live record %q after re-delete; want nil", rec.SyncID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestINV_CrossWriterUpsertAfterDelete_RevivesCanonical (sibling — RED before)
//
// Sequence (one topic):
//   1. write topic T under sync_id Y (writer-Y)
//   2. delete T via Y               (tombstones Y; row Y soft-deleted)
//   3. upsert T via sync_id X newer (writer-X) — supersedes the tombstone.
//      Must REVIVE the canonical row Y in place — NOT insert a new row X that
//      orphans the dead row Y.
//
// After step 3:
//   • exactly ONE live row for T (INV-A), and it is sync_id Y (revived in place).
//   • zero/cleared tombstones for T (INV-B).
//   • total rows for T == 1 (no orphan dead row left behind).
//
// Then step 4 (the resurrection probe):
//   4. a STALE topic-less upsert for sync_id Y arrives. Because Y was orphaned
//      (dead row, no tombstone) in the buggy path, FindBySyncID(Y) would find the
//      soft-deleted row and a winning write could revive it into a SECOND live
//      row → INV1 violation. With the fix there is no orphan, so this must NOT
//      create a second live row.
//
// BEFORE THE FIX: step 3 has cur==nil (Y soft-deleted, X never stored). The
// OpUpsert branch returns ActionInsert{X, Undelete:true}; Apply INSERTs a new row
// X and execClearTombstone (by topic_key) clears tombstone Y — leaving dead row Y
// WITHOUT a tombstone. Two rows for T (X live, Y dead). This test asserts exactly
// 1 total row for T → RED before.
// ─────────────────────────────────────────────────────────────────────────────
func TestINV_CrossWriterUpsertAfterDelete_RevivesCanonical(t *testing.T) {
	s := openTempStore(t)
	project, scope := "engram", "project"
	topic := "sdd/test/upsert-after-delete"

	syncY, syncX := "sync-Y", "sync-X"
	tWrite := baseT.Add(10 * time.Second)
	tDel := baseT.Add(50 * time.Second)
	tRevive := baseT.Add(90 * time.Second)

	// 1. write T under Y
	writeTopic(t, s, "mut-write", syncY, topic, project, scope, "Y content", 1, 1, tWrite)

	// 2. delete T via Y (same writer) → tombstone Y, row Y soft-deleted
	deleteTopic(t, s, "mut-del", syncY, topic, project, scope, 2, 2, tDel)

	// 3. upsert T via X (newer) → must revive canonical Y, not insert X
	tk := topic
	mutX := domain.Mutation{
		MutationID: "mut-upsert-x",
		Op:         domain.OpUpsert,
		SyncID:     syncX,
		SessionID:  "sess-X",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "X title",
		Content:    "X content — supersedes tombstone",
		Project:    project,
		Scope:      scope,
		TopicKey:   &tk,
		Version:    3,
		Seq:        5,
		UpdatedAt:  tRevive,
		OccurredAt: tRevive,
		WriterID:   "writer-X",
	}
	dX := domain.Decide(s, mutX)
	if !dX.Undelete {
		t.Errorf("upsert-after-delete: want Undelete=true; got false")
	}
	// Must converge on the canonical identity Y (revive in place), not mint X.
	if dX.TargetSyncID != syncY {
		t.Errorf("upsert-after-delete: TargetSyncID = %q; want %q (revive canonical, not mint X)",
			dX.TargetSyncID, syncY)
	}
	if err := Apply(s.db, dX, mutX); err != nil {
		t.Fatalf("apply upsert X: %v", err)
	}

	// INV-A: exactly ONE live row for the topic.
	if got := countLiveRowsForTopic(t, s, topic, project, scope); got != 1 {
		t.Errorf("INV-A: live rows for topic %q = %d; want exactly 1", topic, got)
	}
	// Total rows for the topic == 1 (no orphan dead row left behind).
	if got := countRowsForTopic(t, s, topic, project, scope); got != 1 {
		t.Errorf("INV-A: total rows for topic %q = %d; want exactly 1 (orphan dead row left behind)", topic, got)
	}
	// The surviving live row is Y (revived in place) with X's content.
	var liveSync, liveContent string
	if err := s.db.QueryRow(
		`SELECT sync_id, content FROM memories
		   WHERE topic_key=? AND project=? AND scope=? AND deleted_at IS NULL`,
		topic, project, scope,
	).Scan(&liveSync, &liveContent); err != nil {
		t.Fatalf("query live row: %v", err)
	}
	if liveSync != syncY {
		t.Errorf("live row sync_id = %q; want %q (canonical revived in place)", liveSync, syncY)
	}
	if liveContent != mutX.Content {
		t.Errorf("live row content = %q; want %q (newer write wins)", liveContent, mutX.Content)
	}
	// INV-B: tombstone for the topic is cleared.
	if got := countTombstonesForTopic(t, s, topic, project, scope); got != 0 {
		t.Errorf("INV-B: tombstones for topic %q = %d; want 0 (cleared on revive)", topic, got)
	}
	// No spurious row X.
	var xCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM memories WHERE sync_id=?`, syncX).Scan(&xCount); err != nil {
		t.Fatalf("count X rows: %v", err)
	}
	if xCount != 0 {
		t.Errorf("spurious row for sync_id X = %d; want 0 (must revive Y, not insert X)", xCount)
	}

	// ── 4. Resurrection probe: stale topic-less upsert for orphaned Y ─────────
	// In the buggy path Y was a dead row WITHOUT a tombstone; this stale write
	// could revive it into a SECOND live row. With the fix Y is already the live
	// canonical row, so a stale (older) write must be a NoOp and must not create
	// a second live row.
	staleMutY := domain.Mutation{
		MutationID: "mut-stale-y",
		Op:         domain.OpUpsert,
		SyncID:     syncY, // no topic_key — keyed purely by sync_id
		SessionID:  "sess-Y",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "stale Y",
		Content:    "stale Y content",
		Project:    project,
		Scope:      scope,
		Version:    1,
		Seq:        1,
		UpdatedAt:  tDel, // older than the revive — must lose
		OccurredAt: tDel,
		WriterID:   "writer-Y",
	}
	dStale := domain.Decide(s, staleMutY)
	if err := Apply(s.db, dStale, staleMutY); err != nil {
		t.Fatalf("apply stale Y: %v", err)
	}
	// Still exactly one live row for the topic (no resurrection of a second row).
	if got := countLiveRowsForTopic(t, s, topic, project, scope); got != 1 {
		t.Errorf("INV-A after stale probe: live rows for topic %q = %d; want exactly 1 (orphan Y revived into a 2nd live row)", topic, got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestINV_PureTombstoneUpsert_CreatesSingleLiveRow
//
// Sequence:
//   1. delete an UNKNOWN sync_id U for topic T (no memories row ever existed) →
//      writes a pure tombstone (UPDATE hits 0 rows, tombstone INSERT runs).
//   2. upsert that same identity (topic T, sync_id U) newer → supersedes the
//      tombstone.
//
// After step 2:
//   • exactly ONE live row for T (INV-A).
//   • tombstone cleared (INV-B).
//
// This is the pure-tombstone branch: cur==nil AND no row exists for ts.SyncID, so
// the only correct outcome is ActionInsert{U, Undelete:true} with the stale
// tombstone cleared. (Distinct from the revive case where a soft-deleted row for
// ts.SyncID exists.)
// ─────────────────────────────────────────────────────────────────────────────
func TestINV_PureTombstoneUpsert_CreatesSingleLiveRow(t *testing.T) {
	s := openTempStore(t)
	project, scope := "engram", "project"
	topic := "sdd/test/pure-tombstone-upsert"
	syncU := "sync-U"

	tDel := baseT.Add(50 * time.Second)
	tRevive := baseT.Add(90 * time.Second)

	// 1. delete an unknown sync_id → pure tombstone (no memories row).
	deleteTopic(t, s, "mut-del-u", syncU, topic, project, scope, 1, 1, tDel)

	// Confirm precondition: tombstone exists, no memories row.
	if got := countTombstonesForTopic(t, s, topic, project, scope); got != 1 {
		t.Fatalf("precondition: tombstones for topic = %d; want 1", got)
	}
	if got := countRowsForTopic(t, s, topic, project, scope); got != 0 {
		t.Fatalf("precondition: memories rows for topic = %d; want 0 (pure tombstone)", got)
	}

	// 2. upsert that identity newer → supersede tombstone, create the row.
	tk := topic
	mutU := domain.Mutation{
		MutationID: "mut-upsert-u",
		Op:         domain.OpUpsert,
		SyncID:     syncU,
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "U title",
		Content:    "U content — created from pure tombstone",
		Project:    project,
		Scope:      scope,
		TopicKey:   &tk,
		Version:    2,
		Seq:        5,
		UpdatedAt:  tRevive,
		OccurredAt: tRevive,
		WriterID:   "writer-U",
	}
	dU := domain.Decide(s, mutU)
	if dU.Action != domain.ActionInsert {
		t.Fatalf("pure-tombstone upsert: want ActionInsert; got %v", dU.Action)
	}
	if !dU.Undelete {
		t.Errorf("pure-tombstone upsert: want Undelete=true; got false")
	}
	if dU.TargetSyncID != syncU {
		t.Errorf("pure-tombstone upsert: TargetSyncID = %q; want %q", dU.TargetSyncID, syncU)
	}
	if err := Apply(s.db, dU, mutU); err != nil {
		t.Fatalf("apply upsert U: %v", err)
	}

	// INV-A: exactly one live row.
	if got := countLiveRowsForTopic(t, s, topic, project, scope); got != 1 {
		t.Errorf("INV-A: live rows for topic %q = %d; want exactly 1", topic, got)
	}
	// INV-B: tombstone cleared.
	if got := countTombstonesForTopic(t, s, topic, project, scope); got != 0 {
		t.Errorf("INV-B: tombstones for topic %q = %d; want 0 (cleared)", topic, got)
	}
	// FindByTopic returns the live record.
	rec, err := s.FindByTopic(topic, project, scope)
	if err != nil {
		t.Fatalf("FindByTopic: %v", err)
	}
	if rec == nil {
		t.Fatal("FindByTopic returned nil; want live record")
	}
	if rec.Content != mutU.Content {
		t.Errorf("live record content = %q; want %q", rec.Content, mutU.Content)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestINV4_StaleUpsertAgainstTombstone_StaysDeleted (regression)
//
// A stale upsert (older than the tombstone) must still be blocked.
// The row must remain soft-deleted and the tombstone must remain present.
// ─────────────────────────────────────────────────────────────────────────────
func TestINV4_StaleUpsertAgainstTombstone_StaysDeleted(t *testing.T) {
	s := openTempStore(t)
	project, scope := "engram", "project"
	syncID := "sync-stale-tomb"

	tStale := baseT.Add(50 * time.Second)
	tDeleted := baseT.Add(100 * time.Second)
	deletedAtStr := tDeleted.UTC().Format(time.RFC3339Nano)

	// Seed soft-deleted row.
	_, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content,
		   project, scope, version, seq, writer_id, updated_at, deleted_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		syncID, "sess-A", "memory", "manual", "T", "C",
		project, scope, 1, 1, "writer-A",
		deletedAtStr, deletedAtStr,
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Seed tombstone (newer than incoming stale upsert).
	_, err = s.db.Exec(`
		INSERT INTO memory_tombstones (sync_id, project, scope, deleted_at, deleted_by, version)
		VALUES (?,?,?,?,?,?)`,
		syncID, project, scope, deletedAtStr, "writer-A", 1,
	)
	if err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}

	// Stale upsert: older than tombstone.
	mut := domain.Mutation{
		MutationID: "mut-stale",
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess-B",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Stale",
		Content:    "Stale content",
		Project:    project,
		Scope:      scope,
		Version:    1,
		Seq:        1,
		UpdatedAt:  tStale, // older than tombstone
		OccurredAt: tStale,
		WriterID:   "writer-B",
	}

	d := domain.Decide(s, mut)
	if d.Action != domain.NoOp {
		t.Fatalf("Decide: stale upsert against tombstone must be NoOp; got %v", d.Action)
	}

	// Apply NoOp — must be a no-op.
	if err := Apply(s.db, d, mut); err != nil {
		t.Fatalf("Apply NoOp: %v", err)
	}

	// Row must remain soft-deleted.
	var deletedAt sql.NullString
	if err := s.db.QueryRow(
		`SELECT deleted_at FROM memories WHERE sync_id=?`, syncID,
	).Scan(&deletedAt); err != nil {
		t.Fatalf("query deleted_at: %v", err)
	}
	if !deletedAt.Valid {
		t.Error("regression: row was resurrected after stale upsert against tombstone")
	}

	// Tombstone must still be present.
	var tombCount int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_tombstones WHERE sync_id=?`, syncID,
	).Scan(&tombCount); err != nil {
		t.Fatalf("count tombstones: %v", err)
	}
	if tombCount == 0 {
		t.Error("regression: tombstone was removed after stale upsert (should remain)")
	}
}
