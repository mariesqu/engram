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

// TestSearchMemories_EmbeddedQuotesNoError verifies that SearchMemories does not
// return a SQL error when the query contains embedded double-quote characters.
// Regression for: strings.Trim stripping only leading/trailing quotes left
// interior quotes intact, causing FTS5 "unterminated string" errors.
func TestSearchMemories_EmbeddedQuotesNoError(t *testing.T) {
	s := openTempStore(t)

	// Insert a row so there is something to search against.
	_, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('sync-eq-1', 'sess1', 'memory', 'manual', 'embedded quote test', 'no special content', 'testproj', 'project', 'w1')
	`)
	if err != nil {
		t.Fatalf("insert seed: %v", err)
	}

	// These two inputs triggered "SQL logic error: unterminated string" before the fix.
	problemInputs := []string{
		`a"b`,
		`foo" OR title:"bar`,
	}
	for _, q := range problemInputs {
		_, err := s.SearchMemories(q, "testproj", 10)
		if err != nil {
			t.Errorf("SearchMemories(%q) returned unexpected error: %v", q, err)
		}
	}
}

// TestSearchMemories_EmbeddedQuoteMatchesCleanedTerms verifies that a query
// whose terms contain embedded quotes still finds rows whose content matches
// the cleaned (de-quoted) terms.
func TestSearchMemories_EmbeddedQuoteMatchesCleanedTerms(t *testing.T) {
	s := openTempStore(t)

	_, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('sync-eq-2', 'sess1', 'memory', 'manual', 'hexagonal architecture', 'ports and adapters pattern', 'testproj', 'project', 'w1')
	`)
	if err != nil {
		t.Fatalf("insert seed: %v", err)
	}

	// `hex"agonal` cleans to `hexagonal`, which is in the title — must be found.
	results, err := s.SearchMemories(`hex"agonal`, "testproj", 10)
	if err != nil {
		t.Fatalf("SearchMemories with embedded quote: %v", err)
	}
	found := false
	for _, r := range results {
		if r.SyncID == "sync-eq-2" {
			found = true
		}
	}
	if !found {
		t.Errorf("cleaned term 'hexagonal' (from 'hex\"agonal') should match the seeded row")
	}
}

// TestSearchMemories_MultiWordNormalQuery verifies that a plain multi-word query
// (no special characters) still works correctly after the sanitizeFTS change —
// regression guard for existing behaviour.
func TestSearchMemories_MultiWordNormalQuery(t *testing.T) {
	s := openTempStore(t)

	_, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('sync-mw-1', 'sess1', 'memory', 'manual', 'clean architecture principles', 'dependency inversion', 'testproj', 'project', 'w1')
	`)
	if err != nil {
		t.Fatalf("insert seed: %v", err)
	}

	// Normal multi-word query — must find the row and not error.
	results, err := s.SearchMemories("clean architecture", "testproj", 10)
	if err != nil {
		t.Fatalf("SearchMemories multi-word: %v", err)
	}
	found := false
	for _, r := range results {
		if r.SyncID == "sync-mw-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("multi-word query 'clean architecture' should match seeded row")
	}
}

// TestSearchMemories_OperatorLikeInputNeutralized verifies that FTS5 operator
// keywords (OR, AND, title:) in the query do not get executed as operators —
// they must be wrapped in quotes and treated as literal terms.
func TestSearchMemories_OperatorLikeInputNeutralized(t *testing.T) {
	s := openTempStore(t)

	// Insert a row whose content contains the word "OR" literally.
	_, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('sync-op-1', 'sess1', 'memory', 'manual', 'operator test', 'choose this OR that pattern', 'testproj', 'project', 'w1')
	`)
	if err != nil {
		t.Fatalf("insert seed: %v", err)
	}

	// These inputs would cause FTS5 operator errors if not sanitized.
	operatorInputs := []string{
		"OR",
		"title:foo",
		"foo OR bar",
		"NOT memory",
	}
	for _, q := range operatorInputs {
		_, err := s.SearchMemories(q, "testproj", 10)
		if err != nil {
			t.Errorf("SearchMemories(%q) with operator-like input should not error: %v", q, err)
		}
	}
}

// TestSanitizeFTS_InteriorQuotesStripped verifies that sanitizeFTS removes
// interior double-quotes, not just leading/trailing ones.
func TestSanitizeFTS_InteriorQuotesStripped(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{`a"b`, `"ab"`},                    // interior quote removed
		{`foo" OR title:"bar`, `"foo" "OR" "title:bar"`}, // quotes from injection attempt removed
		{`"already quoted"`, `"already" "quoted"`},      // outer quotes (existing behaviour)
		{`a""b`, `"ab"`},                   // multiple interior quotes
		{`"`, ``},                           // token that is only a quote — skipped
		{`" "`, ``},                         // two all-quote tokens — both skipped
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

// TestParentCheck_OutOfOrderChildAccepted verifies that a spec/task/plan whose
// parent_sync_id references a sync_id that does NOT yet exist in memories is
// accepted (out-of-order child arrives first; defer-and-replay reconciles later).
//
// With `REFERENCES memories(sync_id)` + `PRAGMA foreign_keys = ON` this INSERT
// would fail with "FOREIGN KEY constraint failed" — that is the P1 bug.
// After the fix (REFERENCES clause removed) it must succeed.
func TestParentCheck_OutOfOrderChildAccepted(t *testing.T) {
	s := openTempStore(t)

	// "parent-not-yet-arrived" does NOT exist in memories — parent hasn't synced yet.
	_, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, status, title, content, project, scope, writer_id, parent_sync_id)
		VALUES
		  ('child-task-oor', 'sess1', 'task', 'manual', 'todo', 'Orphaned Task', 'content', 'p', 'project', 'w', 'parent-not-yet-arrived')
	`)
	if err != nil {
		t.Errorf("out-of-order child insert should succeed (no enforced FK), got: %v", err)
	}
}

// TestParentCheck_OrphanSpecStillRejected verifies that inserting a 'spec' row
// with a NULL parent_sync_id is STILL rejected by the hierarchy CHECK constraint
// after the FK removal (the CHECK must survive the fix).
func TestParentCheck_OrphanSpecStillRejected(t *testing.T) {
	s := openTempStore(t)
	_, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, status, title, content, project, scope, writer_id)
		VALUES
		  ('spec-no-parent-post-fix', 'sess1', 'spec', 'manual', 'draft', 'T', 'C', 'p', 'project', 'w')
	`)
	if err == nil {
		t.Error("spec with NULL parent_sync_id must still be rejected by hierarchy CHECK after FK removal")
	}
}

// ── Schema migration tests (Bug B / Codex) ───────────────────────────────────

// createLegacyDB creates a raw SQLite file with the OLD memories table that
// includes `parent_sync_id TEXT REFERENCES memories(sync_id)` and seeds it with
// representative rows.  It does NOT call Open so the new migration code is not
// invoked.  The caller opens the returned path via Open to trigger migration.
//
// Returns the db path.  The caller is responsible for cleanup (use t.TempDir).
func createLegacyDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("createLegacyDB: open: %v", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		t.Fatalf("createLegacyDB: WAL: %v", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("createLegacyDB: foreign_keys: %v", err)
	}

	// OLD memories table — identical to the pre-fix schema with the FK clause.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS memories (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		sync_id         TEXT    NOT NULL UNIQUE,
		session_id      TEXT    NOT NULL,
		entity_type     TEXT    NOT NULL DEFAULT 'memory'
		                CHECK(entity_type IN ('memory','change','spec','task','standard','plan')),
		type            TEXT    NOT NULL,
		status          TEXT,
		title           TEXT    NOT NULL,
		content         TEXT    NOT NULL DEFAULT '',
		project         TEXT    NOT NULL DEFAULT '',
		scope           TEXT    NOT NULL DEFAULT 'project',
		topic_key       TEXT,
		parent_sync_id  TEXT REFERENCES memories(sync_id),
		version         INTEGER NOT NULL DEFAULT 1,
		seq             INTEGER NOT NULL DEFAULT 0,
		writer_id       TEXT    NOT NULL DEFAULT '',
		normalized_hash TEXT,
		embedding       BLOB,
		embedding_model TEXT,
		embedding_created_at TEXT,
		created_at      TEXT    NOT NULL DEFAULT (datetime('now')),
		updated_at      TEXT    NOT NULL DEFAULT (datetime('now')),
		deleted_at      TEXT,
		review_after    TEXT,
		expires_at      TEXT,
		CHECK(entity_type = 'memory' OR status IS NOT NULL),
		CHECK(entity_type IN ('memory','change','standard') OR parent_sync_id IS NOT NULL)
	)`); err != nil {
		t.Fatalf("createLegacyDB: create memories: %v", err)
	}

	// Other tables that ApplySchema creates — using simple stubs so Open does not fail.
	otherTables := []string{
		`CREATE TABLE IF NOT EXISTS memory_tombstones (
			sync_id    TEXT    PRIMARY KEY,
			project    TEXT    NOT NULL DEFAULT '',
			scope      TEXT    NOT NULL DEFAULT 'project',
			topic_key  TEXT,
			deleted_at TEXT    NOT NULL,
			deleted_by TEXT    NOT NULL,
			version    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS memory_relations (
			from_sync_id TEXT NOT NULL,
			to_sync_id   TEXT NOT NULL,
			rel_type     TEXT NOT NULL DEFAULT 'parent',
			PRIMARY KEY (from_sync_id, to_sync_id, rel_type)
		)`,
		`CREATE TABLE IF NOT EXISTS sync_mutations (
			local_seq    INTEGER PRIMARY KEY AUTOINCREMENT,
			mutation_id  TEXT    NOT NULL UNIQUE,
			entity       TEXT    NOT NULL DEFAULT '',
			entity_key   TEXT    NOT NULL DEFAULT '',
			op           TEXT    NOT NULL CHECK(op IN ('upsert','delete')),
			payload      TEXT    NOT NULL DEFAULT '',
			writer_id    TEXT    NOT NULL DEFAULT '',
			occurred_at  TEXT    NOT NULL DEFAULT (datetime('now')),
			acked_at     TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS sync_state (
			target_key       TEXT PRIMARY KEY DEFAULT 'central',
			last_acked_seq   INTEGER NOT NULL DEFAULT 0,
			last_pulled_seq  INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS applied_mutations (
			mutation_id TEXT    PRIMARY KEY,
			applied_at  TEXT    NOT NULL DEFAULT (datetime('now'))
		)`,
		`INSERT OR IGNORE INTO sync_state(target_key) VALUES ('central')`,
	}
	for _, s := range otherTables {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("createLegacyDB: stub table: %v", err)
		}
	}

	// Seed representative rows:
	// 1. A root 'change' (no parent needed).
	if _, err := db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, status, title, content, project, scope, writer_id)
		VALUES ('chg-legacy-1', 'sess-leg', 'change', 'manual', 'planning', 'Legacy Change', 'searchable migration content', 'eng', 'project', 'w-leg')
	`); err != nil {
		t.Fatalf("createLegacyDB: insert root change: %v", err)
	}

	// 2. A 'task' that references the root change (valid FK so it inserts).
	if _, err := db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, status, title, content, project, scope, writer_id, parent_sync_id)
		VALUES ('task-legacy-1', 'sess-leg', 'task', 'manual', 'todo', 'Legacy Task', 'task content', 'eng', 'project', 'w-leg', 'chg-legacy-1')
	`); err != nil {
		t.Fatalf("createLegacyDB: insert child task: %v", err)
	}

	return path
}

// TestMigration_V0ToV1_LegacyFKDropped is the RED proof test for Bug B.
// It simulates an existing user DB that was created with the old FK-bearing schema,
// then calls Open() (which must run the v0->v1 migration) and asserts:
//
//  1. user_version is upgraded to currentSchemaVersion (1)
//  2. Out-of-order child insert SUCCEEDS (FK is gone) — FAILS before migration
//  3. All original rows are preserved
//  4. FTS search still returns a seeded row (FTS index rebuilt)
//  5. Hierarchy CHECK still rejects a NULL-parent spec/task/plan
func TestMigration_V0ToV1_LegacyFKDropped(t *testing.T) {
	path := createLegacyDB(t)

	// Open triggers the migration.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy DB: %v", err)
	}
	defer s.Close()

	// 1. user_version must equal currentSchemaVersion.
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if ver != currentSchemaVersion {
		t.Errorf("user_version = %d; want %d", ver, currentSchemaVersion)
	}

	// 2. Out-of-order child insert must succeed (FK removed by migration).
	// Before migration this fails with "FOREIGN KEY constraint failed".
	_, err = s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, status, title, content, project, scope, writer_id, parent_sync_id)
		VALUES
		  ('task-oor-migrated', 'sess-leg', 'task', 'manual', 'todo', 'OOR Task', 'c', 'eng', 'project', 'w', 'parent-not-present')
	`)
	if err != nil {
		t.Errorf("out-of-order child insert must succeed after FK migration, got: %v", err)
	}

	// 3. All original rows preserved.
	var count int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memories WHERE sync_id IN ('chg-legacy-1','task-legacy-1')`,
	).Scan(&count); err != nil {
		t.Fatalf("count preserved rows: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 preserved legacy rows; got %d", count)
	}

	// 4. FTS search works — the seeded 'change' row has "migration" in its content.
	results, err := s.SearchMemories("migration", "eng", 10)
	if err != nil {
		t.Fatalf("SearchMemories after migration: %v", err)
	}
	found := false
	for _, r := range results {
		if r.SyncID == "chg-legacy-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("FTS: seeded row 'chg-legacy-1' not found after migration rebuild")
	}

	// 5. Hierarchy CHECK must still reject a NULL-parent spec.
	_, err = s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, status, title, content, project, scope, writer_id)
		VALUES
		  ('spec-no-parent-migrated', 'sess-leg', 'spec', 'manual', 'draft', 'T', 'C', 'eng', 'project', 'w')
	`)
	if err == nil {
		t.Error("hierarchy CHECK must still reject NULL-parent spec after migration")
	}
}

// TestMigration_FreshDB_NoRebuild verifies that a fresh DB (created by ApplySchema)
// is set to currentSchemaVersion on first Open without a pointless table rebuild.
// The user_version must equal currentSchemaVersion and the table must not have an FK.
func TestMigration_FreshDB_NoRebuild(t *testing.T) {
	s := openTempStore(t)

	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if ver != currentSchemaVersion {
		t.Errorf("fresh DB user_version = %d; want %d", ver, currentSchemaVersion)
	}

	// Must also accept out-of-order children (same proof as above, regression guard).
	_, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, status, title, content, project, scope, writer_id, parent_sync_id)
		VALUES
		  ('fresh-oor-child', 'sess1', 'task', 'manual', 'todo', 'OOR Task', 'c', 'eng', 'project', 'w', 'absent-parent')
	`)
	if err != nil {
		t.Errorf("fresh DB: out-of-order child must succeed, got: %v", err)
	}
}

// TestMigration_Idempotent verifies that running Open twice on the same DB
// (already at currentSchemaVersion) does not error or corrupt data.
func TestMigration_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "idem.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	// Seed a row.
	if _, err := s1.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('idem-mem-1', 'sess1', 'memory', 'manual', 'Idempotent Test', 'content', 'eng', 'project', 'w1')
	`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	_ = s1.Close()

	// Second Open on the same (already-migrated) DB.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()

	var title string
	if err := s2.db.QueryRow(
		`SELECT title FROM memories WHERE sync_id='idem-mem-1'`,
	).Scan(&title); err != nil {
		t.Fatalf("query after second Open: %v", err)
	}
	if title != "Idempotent Test" {
		t.Errorf("title after second Open = %q; want 'Idempotent Test'", title)
	}
}

// Compile-time check: Store must satisfy domain.Reader.
var _ domain.Reader = (*Store)(nil)

// Ensure fmt is used (for syncID in type check tests if needed elsewhere).
var _ = fmt.Sprintf
