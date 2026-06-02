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
	if err := Apply(s.db, domain.Decision{Action: domain.ActionWriteTombstone, TargetSyncID: m.SyncID}, m); err != nil {
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

	mutX := domain.Mutation{
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
	}
	if err := Apply(s.db, domain.Decision{Action: domain.ActionInsert, TargetSyncID: mutX.SyncID}, mutX); err != nil {
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
	if err := Apply(s.db, domain.Decision{Action: domain.ActionInsert, TargetSyncID: m.SyncID}, m); err != nil {
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
	if err := Apply(s.db, domain.Decision{Action: domain.ActionUpdate, TargetSyncID: m.SyncID}, m); err != nil {
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
	if err := Apply(s.db, domain.Decision{Action: domain.ActionInsert, TargetSyncID: m.SyncID}, m); err != nil {
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
	// NOTE: memory_relations is created separately below with the legacy FK columns
	// to accurately simulate a v0 DB created from the original bf04dbb schema.
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

	// Legacy memory_relations WITH REFERENCES FKs (Bug B: the original bf04dbb schema).
	// These must be removed by migrateV0ToV1 so out-of-order relation inserts succeed
	// under PRAGMA foreign_keys = ON.  Created without IF NOT EXISTS because the table
	// must not exist yet (we intentionally omitted it from the otherTables loop above).
	if _, err := db.Exec(`CREATE TABLE memory_relations (
		from_sync_id TEXT NOT NULL REFERENCES memories(sync_id),
		to_sync_id   TEXT NOT NULL REFERENCES memories(sync_id),
		rel_type     TEXT NOT NULL DEFAULT 'parent',
		PRIMARY KEY (from_sync_id, to_sync_id, rel_type)
	)`); err != nil {
		t.Fatalf("createLegacyDB: create legacy memory_relations with FKs: %v", err)
	}

	// Seed a valid relation (both sync_ids present in memories at this point).
	if _, err := db.Exec(`
		INSERT INTO memory_relations (from_sync_id, to_sync_id, rel_type)
		VALUES ('chg-legacy-1', 'task-legacy-1', 'parent')
	`); err != nil {
		t.Fatalf("createLegacyDB: seed legacy relation: %v", err)
	}

	return path
}

// TestMigration_V0ToV1_LegacyFKDropped is the RED proof test for Bug B.
// It simulates an existing user DB that was created with the old FK-bearing schema,
// then calls Open() (which must run the v0->v1 migration) and asserts:
//
//  1. user_version is upgraded to currentSchemaVersion
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

// TestMigration_V0ToV1_IndexesRebuiltOnNewTable is the RED proof test for Bug A.
// After migrating a legacy DB, all four indexes that were defined on the original
// memories table must exist on the NEW memories table — not be lost when
// memories_old is dropped.
//
// Bug: ALTER TABLE memories RENAME TO memories_old preserves the existing
// idx_mem_* index names pointing at memories_old. The subsequent
// CREATE INDEX IF NOT EXISTS ... statements silently no-op (name already exists),
// so after DROP TABLE memories_old the new memories has zero indexes.
//
// Fix: explicitly DROP INDEX IF EXISTS idx_mem_* before recreating them so the
// CREATE INDEX statements run against the new table.
func TestMigration_V0ToV1_IndexesRebuiltOnNewTable(t *testing.T) {
	path := createLegacyDB(t)

	// Open triggers the v0→v1 migration.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy DB: %v", err)
	}
	defer s.Close()

	// Assert all four expected indexes are present on memories (not memories_old).
	expectedIndexes := []string{
		"idx_mem_topic",
		"idx_mem_parent",
		"idx_mem_entity_status",
		"idx_mem_deleted",
	}
	for _, idx := range expectedIndexes {
		var name string
		err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='index' AND name=? AND tbl_name='memories'`,
			idx,
		).Scan(&name)
		if err != nil {
			t.Errorf("index %q missing on memories after v0→v1 migration: %v", idx, err)
		}
	}
}

// TestMigration_V0ToV1_RelationsFKsDropped is the RED proof test for Bug B.
// A legacy DB whose memory_relations table carries REFERENCES FKs must have
// those FKs removed by migrateV0ToV1 so out-of-order relation inserts succeed
// under PRAGMA foreign_keys = ON.
//
// Bug: migrateV0ToV1 only rebuilt memories; memory_relations was left with its
// legacy REFERENCES clauses.  Additionally, after rebuildMemoriesTable renames
// memories→memories_old then drops memories_old, SQLite rewrites the REFERENCES
// target in memory_relations to "memories_old" — so any FK check would fail with
// "no such table: main.memories_old".
//
// Fix: in migrateV0ToV1, detect any REFERENCES … MEMORIES (or "memories_old")
// in memory_relations DDL and rebuild it using memoryRelationsTableDDL (no FKs),
// preserving any existing rows.
func TestMigration_V0ToV1_RelationsFKsDropped(t *testing.T) {
	path := createLegacyDB(t)

	// Open triggers the v0→v1 migration.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy DB: %v", err)
	}
	defer s.Close()

	// After migration, inserting a relation whose to_sync_id does NOT exist
	// in memories must succeed (FK removed).  Before the fix this fails with
	// "FOREIGN KEY constraint failed" or "no such table: main.memories_old".
	_, err = s.db.Exec(`
		INSERT INTO memory_relations (from_sync_id, to_sync_id, rel_type)
		VALUES ('chg-legacy-1', 'absent-target-sync-id', 'ref')
	`)
	if err != nil {
		t.Errorf("out-of-order relation insert must succeed after memory_relations FK migration, got: %v", err)
	}

	// Existing seeded relation row must be preserved.
	var count int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_relations WHERE from_sync_id='chg-legacy-1' AND to_sync_id='task-legacy-1'`,
	).Scan(&count); err != nil {
		t.Fatalf("query preserved relation: %v", err)
	}
	if count != 1 {
		t.Errorf("expected seeded relation row to be preserved after migration, got count=%d", count)
	}
}

// ── Bug B migration test ─────────────────────────────────────────────────────

// createV1DBWithBuggyTrigger creates a SQLite DB that simulates a post-#6/pre-#8
// state: user_version = 1, FTS virtual table present, but mem_fts_update uses the
// OLD buggy form (VALUES(...) without WHERE OLD.deleted_at IS NULL guard).
//
// This is the exact trigger that causes SQLITE_CORRUPT_VTAB (error 267) when an
// undelete sequence is executed: INSERT deleted_at row + UPDATE SET deleted_at=NULL.
// The update trigger unconditionally issues a FTS 'delete' for the OLD rowid, but
// the row was never in the FTS index (because the insert trigger skips rows where
// deleted_at IS NOT NULL), causing corruption.
//
// Returns the db path. Caller is responsible for cleanup (use t.TempDir).
func createV1DBWithBuggyTrigger(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "v1buggy.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("createV1DBWithBuggyTrigger: open: %v", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		t.Fatalf("createV1DBWithBuggyTrigger: WAL: %v", err)
	}

	// Apply full current schema (creates correct triggers via ApplySchema).
	if err := ApplySchema(db); err != nil {
		t.Fatalf("createV1DBWithBuggyTrigger: ApplySchema: %v", err)
	}

	// Now REPLACE mem_fts_update with the OLD buggy version that lacks the
	// WHERE OLD.deleted_at IS NULL guard. This simulates a DB that was created
	// before the trigger fix landed (pre-PR #8 / post-PR #6 state).
	if _, err := db.Exec(`DROP TRIGGER IF EXISTS mem_fts_update`); err != nil {
		t.Fatalf("createV1DBWithBuggyTrigger: drop trigger: %v", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER mem_fts_update
		AFTER UPDATE ON memories
	BEGIN
		INSERT INTO memories_fts(memories_fts, rowid, title, content, type, entity_type, status, project, topic_key)
		VALUES (
			'delete',
			OLD.id,
			OLD.title,
			OLD.content,
			OLD.type,
			OLD.entity_type,
			COALESCE(OLD.status, ''),
			OLD.project,
			COALESCE(OLD.topic_key, '')
		);
		INSERT INTO memories_fts(rowid, title, content, type, entity_type, status, project, topic_key)
		SELECT
			NEW.id,
			NEW.title,
			NEW.content,
			NEW.type,
			NEW.entity_type,
			COALESCE(NEW.status, ''),
			NEW.project,
			COALESCE(NEW.topic_key, '')
		WHERE NEW.deleted_at IS NULL;
	END`); err != nil {
		t.Fatalf("createV1DBWithBuggyTrigger: install buggy trigger: %v", err)
	}

	// Set user_version = 1 to simulate a DB that went through the v0->v1 migration
	// (FK removal) but has not yet had the trigger fix migration applied.
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("createV1DBWithBuggyTrigger: set user_version: %v", err)
	}

	return path
}

// TestMigration_V1ToV2_BuggyTriggerReplaced is the RED proof test for Bug B.
//
// It creates a DB at user_version=1 with the OLD buggy mem_fts_update trigger
// (VALUES form, no WHERE OLD.deleted_at IS NULL guard), then opens it via Open()
// (which must run migrateV1ToV2 and replace the trigger).
//
// After Open, the test executes the exact sequence that triggers SQLITE_CORRUPT_VTAB
// with the old trigger:
//  1. INSERT a row with deleted_at set (soft-deleted from birth — not in FTS index)
//  2. UPDATE that row to clear deleted_at (undelete sequence)
//
// With the old trigger, step 2 issues FTS 'delete' for a rowid not in the index →
// SQLITE_CORRUPT_VTAB (error 267).
// After migrateV1ToV2, the guarded trigger is installed → no error.
//
// Also asserts user_version == currentSchemaVersion after the migration.
func TestMigration_V1ToV2_BuggyTriggerReplaced(t *testing.T) {
	path := createV1DBWithBuggyTrigger(t)

	// Open triggers runMigrations → migrateV1ToV2.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open v1-buggy DB: %v", err)
	}
	defer s.Close()

	// 1. user_version must equal currentSchemaVersion after migration.
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if ver != currentSchemaVersion {
		t.Errorf("user_version = %d; want %d (currentSchemaVersion)", ver, currentSchemaVersion)
	}

	// 2. Execute the undelete sequence that triggers CORRUPT_VTAB with the old trigger.
	//    Step A: INSERT a row that starts soft-deleted (deleted_at set).
	//            The INSERT trigger skips it (WHEN NEW.deleted_at IS NULL is false)
	//            so the rowid is NOT in the FTS index.
	if _, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content,
		   project, scope, writer_id, deleted_at)
		VALUES ('v1buggy-test', 'sess1', 'memory', 'manual', 'BugB Title',
		        'BugB content', 'eng', 'project', 'w1', '2025-01-01T00:00:00Z')
	`); err != nil {
		t.Fatalf("INSERT soft-deleted row: %v", err)
	}

	//    Step B: UPDATE to clear deleted_at (undelete).
	//            With the OLD trigger: unconditional FTS 'delete' for OLD.id
	//            which is NOT in the index → SQLITE_CORRUPT_VTAB (267).
	//            With the NEW guarded trigger: WHERE OLD.deleted_at IS NULL
	//            is false → FTS 'delete' is skipped → no error.
	_, err = s.db.Exec(`UPDATE memories SET deleted_at=NULL WHERE sync_id='v1buggy-test'`)
	if err != nil {
		t.Errorf("BUG B: UPDATE to undelete returned error (expected none after trigger migration): %v", err)
	}
}

// ── v2→v3 migration tests ─────────────────────────────────────────────────────

// createV2DBWithoutSeq creates a SQLite DB that simulates an existing v2 DB:
// memory_tombstones WITHOUT the seq column, user_version = 2.
// The caller opens the returned path via Open() to trigger the v2→v3 migration.
// Returns the db path; caller is responsible for cleanup (use t.TempDir).
func createV2DBWithoutSeq(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "v2.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("createV2DBWithoutSeq: open: %v", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		t.Fatalf("createV2DBWithoutSeq: WAL: %v", err)
	}

	// Apply the full current schema first (gets the correct memories table, FTS,
	// triggers, etc.) then immediately drop and recreate memory_tombstones WITHOUT
	// the seq column to simulate the v2 schema.
	if err := ApplySchema(db); err != nil {
		t.Fatalf("createV2DBWithoutSeq: ApplySchema: %v", err)
	}

	// Replace memory_tombstones with the v2 form (no seq column).
	if _, err := db.Exec(`DROP TABLE IF EXISTS memory_tombstones`); err != nil {
		t.Fatalf("createV2DBWithoutSeq: drop: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE memory_tombstones (
		sync_id    TEXT    PRIMARY KEY,
		project    TEXT    NOT NULL DEFAULT '',
		scope      TEXT    NOT NULL DEFAULT 'project',
		topic_key  TEXT,
		deleted_at TEXT    NOT NULL,
		deleted_by TEXT    NOT NULL,
		version    INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatalf("createV2DBWithoutSeq: create v2 tombstones: %v", err)
	}

	// Seed a tombstone row so we can verify rows are preserved through migration.
	if _, err := db.Exec(`
		INSERT INTO memory_tombstones (sync_id, project, scope, deleted_at, deleted_by, version)
		VALUES ('sync-v2-tomb', 'proj', 'project', '2025-01-01T00:00:00Z', 'writer-v2', 1)
	`); err != nil {
		t.Fatalf("createV2DBWithoutSeq: seed tombstone: %v", err)
	}

	// Set user_version = 2 to mark this as a pre-v3 DB.
	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatalf("createV2DBWithoutSeq: set user_version: %v", err)
	}

	return path
}

// TestMigration_V2ToV3_SeqColumnAdded is the authoritative proof test for the
// v2→v3 migration. It creates a DB at user_version=2 with memory_tombstones
// that lacks the seq column, opens it via Open() (which runs migrateV2ToV3),
// and asserts:
//
//  1. user_version == 3 after migration.
//  2. The seq column now exists on memory_tombstones.
//  3. Existing tombstone rows are preserved (seq defaults to 0).
//  4. A tombstone written with a non-zero seq round-trips via FindTombstone.
func TestMigration_V2ToV3_SeqColumnAdded(t *testing.T) {
	path := createV2DBWithoutSeq(t)

	// Open triggers runMigrations → migrateV2ToV3.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open v2 DB: %v", err)
	}
	defer s.Close()

	// 1. user_version must be 3.
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if ver != currentSchemaVersion {
		t.Errorf("user_version = %d; want %d", ver, currentSchemaVersion)
	}

	// 2. seq column must exist on memory_tombstones.
	seqFound := false
	rows, err := s.db.Query(`PRAGMA table_info(memory_tombstones)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			t.Fatalf("table_info scan: %v", err)
		}
		if name == "seq" {
			seqFound = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows.Err: %v", err)
	}
	if !seqFound {
		t.Error("v2→v3 migration: seq column not found on memory_tombstones after migration")
	}

	// 3. Existing tombstone row preserved; seq defaults to 0.
	var seqVal int64
	if err := s.db.QueryRow(
		`SELECT seq FROM memory_tombstones WHERE sync_id = 'sync-v2-tomb'`,
	).Scan(&seqVal); err != nil {
		t.Fatalf("query preserved tombstone: %v", err)
	}
	if seqVal != 0 {
		t.Errorf("preserved tombstone seq = %d; want 0 (DEFAULT backfill)", seqVal)
	}

	// 4. Round-trip: write a tombstone with seq=7, read it back via FindTombstone.
	if _, err := s.db.Exec(`
		INSERT INTO memory_tombstones (sync_id, project, scope, deleted_at, deleted_by, version, seq)
		VALUES ('sync-v3-roundtrip', 'proj', 'project', '2025-06-01T00:00:00Z', 'writer-v3', 2, 7)
	`); err != nil {
		t.Fatalf("insert roundtrip tombstone: %v", err)
	}
	ts, err := s.FindTombstone("sync-v3-roundtrip", nil, "proj", "project")
	if err != nil {
		t.Fatalf("FindTombstone: %v", err)
	}
	if ts == nil {
		t.Fatal("FindTombstone: expected tombstone, got nil")
	}
	if ts.Seq != 7 {
		t.Errorf("tombstone roundtrip: ts.Seq = %d; want 7", ts.Seq)
	}
}

// TestMigration_V2ToV3_FreshDB_Idempotent verifies that Open on a FRESH DB
// (where ApplySchema already creates memory_tombstones WITH seq) reaches
// user_version=3 without erroring — the migration must not attempt a duplicate
// ALTER TABLE.
func TestMigration_V2ToV3_FreshDB_Idempotent(t *testing.T) {
	s := openTempStore(t)

	// user_version must be 3 on a fresh DB.
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if ver != currentSchemaVersion {
		t.Errorf("fresh DB user_version = %d; want %d", ver, currentSchemaVersion)
	}

	// seq column must exist.
	seqFound := false
	rows, err := s.db.Query(`PRAGMA table_info(memory_tombstones)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			t.Fatalf("table_info scan: %v", err)
		}
		if name == "seq" {
			seqFound = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows.Err: %v", err)
	}
	if !seqFound {
		t.Error("fresh DB: seq column missing on memory_tombstones")
	}
}

// TestTombstone_SeqRoundtrip verifies that a tombstone written with a specific
// seq (via Apply WriteTombstone) is returned with ts.Seq populated by
// FindTombstone. This is the integration proof that the localstore wires seq
// end-to-end.
func TestTombstone_SeqRoundtrip(t *testing.T) {
	s := openTempStore(t)

	// Seed the memories row that will be soft-deleted.
	if _, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('sync-seq-rt', 'sess1', 'memory', 'manual', 'Seq Roundtrip', 'content', 'eng', 'project', 'w1')
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const wantSeq int64 = 42
	m := domain.Mutation{
		MutationID: "mut-seq-rt",
		Op:         domain.OpDelete,
		SyncID:     "sync-seq-rt",
		SessionID:  "sess1",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Seq Roundtrip",
		Content:    "content",
		Project:    "eng",
		Scope:      "project",
		Version:    1,
		Seq:        wantSeq,
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
		WriterID:   "w1",
	}
	if err := Apply(s.db, domain.Decision{Action: domain.ActionWriteTombstone, TargetSyncID: m.SyncID}, m); err != nil {
		t.Fatalf("Apply WriteTombstone: %v", err)
	}

	ts, err := s.FindTombstone("sync-seq-rt", nil, "eng", "project")
	if err != nil {
		t.Fatalf("FindTombstone: %v", err)
	}
	if ts == nil {
		t.Fatal("FindTombstone: expected tombstone, got nil")
	}
	if ts.Seq != wantSeq {
		t.Errorf("ts.Seq = %d; want %d", ts.Seq, wantSeq)
	}
}

// Compile-time check: Store must satisfy domain.Reader.
var _ domain.Reader = (*Store)(nil)

// Ensure fmt is used (for syncID in type check tests if needed elsewhere).
var _ = fmt.Sprintf
