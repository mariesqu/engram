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
