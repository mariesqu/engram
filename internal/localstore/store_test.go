package localstore

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// openTempStore creates a temp SQLite file and opens it via Open.
// The file is cleaned up when the test ends.
func openTempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestApplySchema_Idempotent verifies that calling ApplySchema twice on the
// same DB does not error (all DDL is IF NOT EXISTS).
func TestApplySchema_Idempotent(t *testing.T) {
	s := openTempStore(t)
	// Call ApplySchema a second time directly — must be idempotent.
	if err := ApplySchema(s.db); err != nil {
		t.Fatalf("ApplySchema second call failed: %v", err)
	}
}

// TestApplySchema_TablesExist confirms that the expected tables were created.
func TestApplySchema_TablesExist(t *testing.T) {
	s := openTempStore(t)
	tables := []string{
		"memories",
		"memory_tombstones",
		"memory_relations",
		"sync_mutations",
		"sync_state",
		"applied_mutations",
	}
	for _, tbl := range tables {
		var name string
		err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found: %v", tbl, err)
		}
	}
}

// TestApplySchema_FTS5VirtualTableExists checks the memories_fts virtual table.
func TestApplySchema_FTS5VirtualTableExists(t *testing.T) {
	s := openTempStore(t)
	var name string
	err := s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='memories_fts'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("memories_fts virtual table not found: %v", err)
	}
}

// TestApplySchema_TriggersExist confirms FTS maintenance triggers were created.
func TestApplySchema_TriggersExist(t *testing.T) {
	s := openTempStore(t)
	triggers := []string{"mem_fts_insert", "mem_fts_delete", "mem_fts_update"}
	for _, tr := range triggers {
		var name string
		err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='trigger' AND name=?`, tr,
		).Scan(&name)
		if err != nil {
			t.Errorf("trigger %q not found: %v", tr, err)
		}
	}
}

// TestEntityTypeCheck_InvalidRejected verifies the CHECK constraint rejects
// an unknown entity_type value.
func TestEntityTypeCheck_InvalidRejected(t *testing.T) {
	s := openTempStore(t)
	_, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES
		  ('s-inv', 'sess1', 'INVALID', 'manual', 'T', 'C', 'p', 'project', 'w')
	`)
	if err == nil {
		t.Error("expected CHECK constraint error for invalid entity_type, got nil")
	}
}

// TestEntityTypeCheck_MemoryNoStatus verifies 'memory' rows do not require status.
func TestEntityTypeCheck_MemoryNoStatus(t *testing.T) {
	s := openTempStore(t)
	_, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('s-mem-nostatus', 'sess', 'memory', 'manual', 'T', 'C', 'p', 'project', 'w')
	`)
	if err != nil {
		t.Errorf("memory row without status should succeed: %v", err)
	}
}

// TestEntityTypeCheck_NonMemoryRequiresStatus verifies non-memory rows need status.
func TestEntityTypeCheck_NonMemoryRequiresStatus(t *testing.T) {
	s := openTempStore(t)
	// 'change' without status — CHECK(entity_type='memory' OR status IS NOT NULL) must fire.
	_, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('s-change-nostatus', 'sess', 'change', 'manual', 'T', 'C', 'p', 'project', 'w')
	`)
	if err == nil {
		t.Error("non-memory row without status should fail CHECK constraint")
	}
}

// TestParentCheck_OrphanSpecRejected verifies that inserting a 'spec' row
// without a parent_sync_id is rejected by the hierarchy CHECK constraint.
func TestParentCheck_OrphanSpecRejected(t *testing.T) {
	s := openTempStore(t)
	_, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, status, title, content, project, scope, writer_id)
		VALUES
		  ('s-orphan-spec', 'sess1', 'spec', 'manual', 'draft', 'T', 'C', 'p', 'project', 'w')
	`)
	if err == nil {
		t.Error("expected CHECK constraint error for spec with NULL parent_sync_id, got nil")
	}
}

// TestParentCheck_OrphanTaskRejected verifies that inserting a 'task' row
// without a parent_sync_id is rejected by the hierarchy CHECK constraint.
func TestParentCheck_OrphanTaskRejected(t *testing.T) {
	s := openTempStore(t)
	_, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, status, title, content, project, scope, writer_id)
		VALUES
		  ('s-orphan-task', 'sess1', 'task', 'manual', 'todo', 'T', 'C', 'p', 'project', 'w')
	`)
	if err == nil {
		t.Error("expected CHECK constraint error for task with NULL parent_sync_id, got nil")
	}
}

// TestParentCheck_OrphanPlanRejected verifies that inserting a 'plan' row
// without a parent_sync_id is rejected by the hierarchy CHECK constraint.
func TestParentCheck_OrphanPlanRejected(t *testing.T) {
	s := openTempStore(t)
	_, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, status, title, content, project, scope, writer_id)
		VALUES
		  ('s-orphan-plan', 'sess1', 'plan', 'manual', 'draft', 'T', 'C', 'p', 'project', 'w')
	`)
	if err == nil {
		t.Error("expected CHECK constraint error for plan with NULL parent_sync_id, got nil")
	}
}

// TestParentCheck_ParentedSpecAccepted verifies that inserting a 'spec' row
// WITH a valid parent_sync_id is accepted by the hierarchy CHECK constraint.
func TestParentCheck_ParentedSpecAccepted(t *testing.T) {
	s := openTempStore(t)
	// Insert the parent 'change' row first.
	_, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, status, title, content, project, scope, writer_id)
		VALUES
		  ('chg-parent-1', 'sess1', 'change', 'manual', 'planning', 'Parent Change', 'C', 'p', 'project', 'w')
	`)
	if err != nil {
		t.Fatalf("insert parent change: %v", err)
	}
	// Now insert the spec with a parent.
	_, err = s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, status, title, content, project, scope, writer_id, parent_sync_id)
		VALUES
		  ('s-parented-spec', 'sess1', 'spec', 'manual', 'draft', 'T', 'C', 'p', 'project', 'w', 'chg-parent-1')
	`)
	if err != nil {
		t.Errorf("spec with parent_sync_id should be accepted: %v", err)
	}
}

// TestParentCheck_MemoryNullParentAccepted verifies that inserting a 'memory' row
// with NULL parent_sync_id is accepted (root-level memories are always valid).
func TestParentCheck_MemoryNullParentAccepted(t *testing.T) {
	s := openTempStore(t)
	_, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES
		  ('s-root-memory', 'sess1', 'memory', 'manual', 'T', 'C', 'p', 'project', 'w')
	`)
	if err != nil {
		t.Errorf("memory row with NULL parent_sync_id should be accepted: %v", err)
	}
}

// TestParentCheck_ChangeNullParentAccepted verifies that inserting a 'change' row
// with NULL parent_sync_id is accepted.
func TestParentCheck_ChangeNullParentAccepted(t *testing.T) {
	s := openTempStore(t)
	_, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, status, title, content, project, scope, writer_id)
		VALUES
		  ('s-root-change', 'sess1', 'change', 'manual', 'planning', 'T', 'C', 'p', 'project', 'w')
	`)
	if err != nil {
		t.Errorf("change row with NULL parent_sync_id should be accepted: %v", err)
	}
}

// TestParentCheck_StandardNullParentAccepted verifies that inserting a 'standard' row
// with NULL parent_sync_id is accepted.
func TestParentCheck_StandardNullParentAccepted(t *testing.T) {
	s := openTempStore(t)
	_, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, status, title, content, project, scope, writer_id)
		VALUES
		  ('s-root-standard', 'sess1', 'standard', 'manual', 'active', 'T', 'C', 'p', 'project', 'w')
	`)
	if err != nil {
		t.Errorf("standard row with NULL parent_sync_id should be accepted: %v", err)
	}
}

// TestFTS_InsertTrigger verifies that inserting a memory row populates FTS.
func TestFTS_InsertTrigger(t *testing.T) {
	s := openTempStore(t)

	_, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('sync-fts-1', 'sess1', 'memory', 'manual', 'Hexagonal Architecture', 'Learn about ports and adapters', 'eng', 'project', 'w1')
	`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// FTS search for a term in the title.
	var count int
	err = s.db.QueryRow(
		`SELECT count(*) FROM memories_fts WHERE memories_fts MATCH '"Hexagonal"'`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("FTS query: %v", err)
	}
	if count == 0 {
		t.Error("FTS insert trigger did not index the new row")
	}
}

// TestFTS_DeleteTrigger verifies that deleting a memory removes it from FTS.
func TestFTS_DeleteTrigger(t *testing.T) {
	s := openTempStore(t)

	_, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('sync-del-1', 'sess1', 'memory', 'manual', 'Temporary Memory', 'Will be deleted', 'eng', 'project', 'w1')
	`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	_, err = s.db.Exec(`DELETE FROM memories WHERE sync_id = 'sync-del-1'`)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	var count int
	err = s.db.QueryRow(
		`SELECT count(*) FROM memories_fts WHERE memories_fts MATCH '"Temporary"'`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("FTS query after delete: %v", err)
	}
	if count != 0 {
		t.Error("FTS delete trigger did not remove the deleted row from the index")
	}
}

// TestFTS_UpdateTrigger verifies that updating a row refreshes FTS.
func TestFTS_UpdateTrigger(t *testing.T) {
	s := openTempStore(t)

	_, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('sync-upd-1', 'sess1', 'memory', 'manual', 'Old Title', 'Old content', 'eng', 'project', 'w1')
	`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	_, err = s.db.Exec(`
		UPDATE memories SET title='New Title', content='New content' WHERE sync_id='sync-upd-1'
	`)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	// Old term must be gone.
	var oldCount int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memories_fts WHERE memories_fts MATCH '"Old"'`,
	).Scan(&oldCount); err != nil {
		t.Fatalf("FTS query old: %v", err)
	}
	if oldCount != 0 {
		t.Errorf("FTS update trigger did not remove old terms, oldCount=%d", oldCount)
	}

	// New term must be present.
	var newCount int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memories_fts WHERE memories_fts MATCH '"New"'`,
	).Scan(&newCount); err != nil {
		t.Fatalf("FTS query new: %v", err)
	}
	if newCount == 0 {
		t.Error("FTS update trigger did not index new terms")
	}
}

// TestSoftDelete_ExcludedFromSearch verifies soft-deleted rows are excluded
// from SearchMemories results.
func TestSoftDelete_ExcludedFromSearch(t *testing.T) {
	s := openTempStore(t)

	_, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('sync-soft-1', 'sess1', 'memory', 'manual', 'Soft Deleted Row', 'Should not appear', 'eng', 'project', 'w1')
	`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Soft-delete by setting deleted_at.
	_, err = s.db.Exec(
		`UPDATE memories SET deleted_at=? WHERE sync_id='sync-soft-1'`,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		t.Fatalf("soft-delete update: %v", err)
	}

	// Verify the row is still in the table (physical delete must not happen).
	var count int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memories WHERE sync_id='sync-soft-1' AND deleted_at IS NOT NULL`,
	).Scan(&count); err != nil || count == 0 {
		t.Fatal("soft-deleted row should still exist in memories table with deleted_at set")
	}

	// SearchMemories must filter it out.
	results, err := s.SearchMemories("Soft Deleted Row", "eng", 10)
	if err != nil {
		t.Fatalf("SearchMemories: %v", err)
	}
	for _, r := range results {
		if r.SyncID == "sync-soft-1" {
			t.Error("soft-deleted row appeared in SearchMemories results")
		}
	}
}

// TestTombstone_WrittenAtomically verifies Apply WriteTombstone sets deleted_at
// on the memories row AND inserts the tombstone row in one transaction.
func TestTombstone_WrittenAtomically(t *testing.T) {
	s := openTempStore(t)

	// Insert a seed record first.
	_, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('sync-tomb-1', 'sess1', 'memory', 'manual', 'To Be Tombstoned', 'content', 'eng', 'project', 'w1')
	`)
	if err != nil {
		t.Fatalf("insert seed: %v", err)
	}

	m := domain.Mutation{
		MutationID: "mut-tomb-1",
		Op:         domain.OpDelete,
		SyncID:     "sync-tomb-1",
		SessionID:  "sess1",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "To Be Tombstoned",
		Content:    "content",
		Project:    "eng",
		Scope:      "project",
		Version:    1,
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
		WriterID:   "w1",
	}
	if err := Apply(s.db, domain.ActionWriteTombstone, m); err != nil {
		t.Fatalf("Apply WriteTombstone: %v", err)
	}

	// Verify deleted_at set on memories row.
	var deletedAt sql.NullString
	if err := s.db.QueryRow(
		`SELECT deleted_at FROM memories WHERE sync_id='sync-tomb-1'`,
	).Scan(&deletedAt); err != nil {
		t.Fatalf("query deleted_at: %v", err)
	}
	if !deletedAt.Valid {
		t.Error("deleted_at not set after WriteTombstone")
	}

	// Verify tombstone row exists.
	var tombCount int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_tombstones WHERE sync_id='sync-tomb-1'`,
	).Scan(&tombCount); err != nil {
		t.Fatalf("query tombstone: %v", err)
	}
	if tombCount == 0 {
		t.Error("tombstone row not inserted after WriteTombstone")
	}
}

// TestSanitizeFTS verifies the FTS5 operator-stripping logic.
func TestSanitizeFTS(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"simple query", `"simple" "query"`},
		{"fix auth bug", `"fix" "auth" "bug"`},
		{`"already quoted"`, `"already" "quoted"`}, // strips then re-quotes
		{"AND OR NOT", `"AND" "OR" "NOT"`},
		{"", ""},
	}
	for _, tc := range cases {
		got := sanitizeFTS(tc.input)
		if got != tc.want {
			t.Errorf("sanitizeFTS(%q) = %q; want %q", tc.input, got, tc.want)
		}
	}
}

// TestReader_FindBySyncID verifies the Reader port FindBySyncID implementation.
func TestReader_FindBySyncID(t *testing.T) {
	s := openTempStore(t)

	_, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('sync-r1', 'sess1', 'memory', 'manual', 'Reader Test', 'content', 'eng', 'project', 'w1')
	`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	rec, err := s.FindBySyncID("sync-r1")
	if err != nil {
		t.Fatalf("FindBySyncID: %v", err)
	}
	if rec == nil {
		t.Fatal("FindBySyncID returned nil for existing sync_id")
	}
	if rec.Title != "Reader Test" {
		t.Errorf("Title = %q; want %q", rec.Title, "Reader Test")
	}

	// Non-existent → nil, no error.
	rec2, err := s.FindBySyncID("does-not-exist")
	if err != nil {
		t.Fatalf("FindBySyncID(non-existent): unexpected error: %v", err)
	}
	if rec2 != nil {
		t.Error("FindBySyncID(non-existent) returned non-nil")
	}
}

// TestReader_FindByTopic verifies FindByTopic returns the live record.
func TestReader_FindByTopic(t *testing.T) {
	s := openTempStore(t)
	topicKey := "sdd/test/explore"

	_, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id, topic_key)
		VALUES ('sync-topic-1', 'sess1', 'memory', 'manual', 'Topic Memory', 'content', 'eng', 'project', 'w1', ?)
	`, topicKey)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	rec, err := s.FindByTopic(topicKey, "eng", "project")
	if err != nil {
		t.Fatalf("FindByTopic: %v", err)
	}
	if rec == nil {
		t.Fatal("FindByTopic returned nil for existing topic_key")
	}
	if rec.SyncID != "sync-topic-1" {
		t.Errorf("SyncID = %q; want sync-topic-1", rec.SyncID)
	}

	// Soft-deleted row must not be returned.
	_, err = s.db.Exec(
		`UPDATE memories SET deleted_at=? WHERE sync_id='sync-topic-1'`,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	rec2, err := s.FindByTopic(topicKey, "eng", "project")
	if err != nil {
		t.Fatalf("FindByTopic after soft-delete: %v", err)
	}
	if rec2 != nil {
		t.Error("FindByTopic should return nil for soft-deleted row")
	}
}

// TestReader_MutationApplied verifies idempotency tracking.
func TestReader_MutationApplied(t *testing.T) {
	s := openTempStore(t)

	applied, err := s.MutationApplied("mut-x")
	if err != nil {
		t.Fatalf("MutationApplied (before): %v", err)
	}
	if applied {
		t.Error("mutation should not be applied yet")
	}

	if err := Apply(s.db, domain.ActionInsert, domain.Mutation{
		MutationID: "mut-x",
		Op:         domain.OpUpsert,
		SyncID:     "sync-x",
		SessionID:  "sess1",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "T",
		Content:    "C",
		Project:    "p",
		Scope:      "project",
		Version:    1,
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
		WriterID:   "w1",
	}); err != nil {
		t.Fatalf("Apply Insert: %v", err)
	}

	applied, err = s.MutationApplied("mut-x")
	if err != nil {
		t.Fatalf("MutationApplied (after): %v", err)
	}
	if !applied {
		t.Error("mutation should be recorded as applied after Apply Insert")
	}
}

// TestApply_Insert verifies the Apply Insert action creates the row.
func TestApply_Insert(t *testing.T) {
	s := openTempStore(t)
	m := domain.Mutation{
		MutationID: "mut-insert-1",
		Op:         domain.OpUpsert,
		SyncID:     "sync-ins-1",
		SessionID:  "sess1",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Inserted Memory",
		Content:    "Inserted content",
		Project:    "eng",
		Scope:      "project",
		Version:    1,
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
		WriterID:   "w1",
	}
	if err := Apply(s.db, domain.ActionInsert, m); err != nil {
		t.Fatalf("Apply Insert: %v", err)
	}

	var title string
	if err := s.db.QueryRow(
		`SELECT title FROM memories WHERE sync_id='sync-ins-1'`,
	).Scan(&title); err != nil {
		t.Fatalf("query after insert: %v", err)
	}
	if title != "Inserted Memory" {
		t.Errorf("Title = %q; want 'Inserted Memory'", title)
	}
}

// TestApply_Update verifies the Apply Update action modifies the row.
func TestApply_Update(t *testing.T) {
	s := openTempStore(t)

	// Seed row.
	_, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id, version)
		VALUES ('sync-upd-apply-1', 'sess1', 'memory', 'manual', 'Old Title', 'Old', 'eng', 'project', 'w1', 1)
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := domain.Mutation{
		MutationID: "mut-upd-1",
		Op:         domain.OpUpsert,
		SyncID:     "sync-upd-apply-1",
		SessionID:  "sess1",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Updated Title",
		Content:    "Updated content",
		Project:    "eng",
		Scope:      "project",
		Version:    2,
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
		WriterID:   "w1",
	}
	if err := Apply(s.db, domain.ActionUpdate, m); err != nil {
		t.Fatalf("Apply Update: %v", err)
	}

	var title string
	var version int
	if err := s.db.QueryRow(
		`SELECT title, version FROM memories WHERE sync_id='sync-upd-apply-1'`,
	).Scan(&title, &version); err != nil {
		t.Fatalf("query after update: %v", err)
	}
	if title != "Updated Title" {
		t.Errorf("Title after Update = %q; want 'Updated Title'", title)
	}
	if version != 2 {
		t.Errorf("Version after Update = %d; want 2", version)
	}
}

// TestFTSRoundtrip_Insert verifies the full FTS roundtrip via Apply Insert +
// SearchMemories — the primary integration scenario.
func TestFTSRoundtrip_Insert(t *testing.T) {
	s := openTempStore(t)
	topicKey := "sdd/roundtrip/test"
	m := domain.Mutation{
		MutationID: "mut-rt-1",
		Op:         domain.OpUpsert,
		SyncID:     "sync-rt-1",
		SessionID:  "sess1",
		EntityType: domain.EntityMemory,
		Type:       "decision",
		Title:      "FTS Roundtrip Decision",
		Content:    "Architecture decision for hexagonal layering",
		Project:    "eng",
		Scope:      "project",
		Version:    1,
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
		WriterID:   "w1",
		TopicKey:   &topicKey,
	}
	if err := Apply(s.db, domain.ActionInsert, m); err != nil {
		t.Fatalf("Apply Insert: %v", err)
	}

	results, err := s.SearchMemories("hexagonal", "eng", 10)
	if err != nil {
		t.Fatalf("SearchMemories: %v", err)
	}
	found := false
	for _, r := range results {
		if r.SyncID == "sync-rt-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("FTS roundtrip: inserted row not found by SearchMemories, got %d results", len(results))
	}
}

// Compile-time check: Store must satisfy domain.Reader.
var _ domain.Reader = (*Store)(nil)

// Ensure fmt is used (for syncID in type check tests if needed elsewhere).
var _ = fmt.Sprintf
