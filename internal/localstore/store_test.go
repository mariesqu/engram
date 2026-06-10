package localstore

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
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
		{`a"b`, `"ab"`}, // interior quote removed
		{`foo" OR title:"bar`, `"foo" "OR" "title:bar"`}, // quotes from injection attempt removed
		{`"already quoted"`, `"already" "quoted"`},       // outer quotes (existing behaviour)
		{`a""b`, `"ab"`}, // multiple interior quotes
		{`"`, ``},        // token that is only a quote — skipped
		{`" "`, ``},      // two all-quote tokens — both skipped
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
// accepted: parent_sync_id is a soft (unvalidated) reference, so the child inserts
// and its parent arrives later via its own mutation (eventual consistency, no replay).
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

// ── Schema version and tombstone identity tests ──────────────────────────────

// TestFreshDB_SchemaVersionIsCurrent verifies that a fresh Open reaches
// currentSchemaVersion (v4 — the v3→v4 migration adds the
// memory_tombstones_topic_uidx partial UNIQUE index, schema-enforcing
// ≤1-tombstone-per-topic).
func TestFreshDB_SchemaVersionIsCurrent(t *testing.T) {
	s := openTempStore(t)

	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if ver != currentSchemaVersion {
		t.Errorf("fresh DB user_version = %d; want %d", ver, currentSchemaVersion)
	}
}

// createV2DB builds a SQLite file at user_version=2 whose memories and
// memory_tombstones tables LACK the last_write_mutation_id column — i.e. a DB
// created before the v2→v3 migration. Returns the path. This is the precondition
// for TestMigration_V2ToV3_LastWriteMutationIDAdded (existing-DB upgrade proof).
func createV2DB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "v2.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("createV2DB: open: %v", err)
	}
	defer db.Close()

	// memories WITHOUT last_write_mutation_id (the pre-v3 column set).
	if _, err := db.Exec(`CREATE TABLE memories (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		sync_id         TEXT NOT NULL UNIQUE,
		session_id      TEXT NOT NULL,
		entity_type     TEXT NOT NULL DEFAULT 'memory',
		type            TEXT NOT NULL,
		status          TEXT,
		title           TEXT NOT NULL,
		content         TEXT NOT NULL DEFAULT '',
		project         TEXT NOT NULL DEFAULT '',
		scope           TEXT NOT NULL DEFAULT 'project',
		topic_key       TEXT,
		parent_sync_id  TEXT,
		version         INTEGER NOT NULL DEFAULT 1,
		seq             INTEGER NOT NULL DEFAULT 0,
		writer_id       TEXT NOT NULL DEFAULT '',
		normalized_hash TEXT,
		embedding       BLOB,
		embedding_model TEXT,
		embedding_created_at TEXT,
		created_at      TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at      TEXT NOT NULL DEFAULT (datetime('now')),
		deleted_at      TEXT,
		review_after    TEXT,
		expires_at      TEXT
	)`); err != nil {
		t.Fatalf("createV2DB: create memories: %v", err)
	}

	// memory_tombstones WITHOUT last_write_mutation_id (the pre-v3 column set).
	if _, err := db.Exec(`CREATE TABLE memory_tombstones (
		sync_id    TEXT PRIMARY KEY,
		project    TEXT NOT NULL DEFAULT '',
		scope      TEXT NOT NULL DEFAULT 'project',
		topic_key  TEXT,
		deleted_at TEXT NOT NULL,
		deleted_by TEXT NOT NULL,
		version    INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatalf("createV2DB: create memory_tombstones: %v", err)
	}

	// Seed one row in each so we can prove existing data survives the ALTER.
	if _, err := db.Exec(`INSERT INTO memories
		(sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('v2-mem-1', 'sess-v2', 'memory', 'manual', 'V2 Row', 'v2 content', 'eng', 'project', 'w-v2')`); err != nil {
		t.Fatalf("createV2DB: seed memories: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO memory_tombstones
		(sync_id, project, scope, deleted_at, deleted_by, version)
		VALUES ('v2-tomb-1', 'eng', 'project', '2025-01-01T00:00:00Z', 'w-v2', 1)`); err != nil {
		t.Fatalf("createV2DB: seed memory_tombstones: %v", err)
	}

	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatalf("createV2DB: set user_version: %v", err)
	}
	return path
}

// TestMigration_V2ToV3_LastWriteMutationIDAdded is the existing-DB upgrade proof
// for the final-tiebreaker fix. A v2 DB (no last_write_mutation_id on either
// table) is opened; the migration chain MUST add the column to BOTH memories and
// memory_tombstones AND add memory_tombstones_topic_uidx, bump user_version to
// currentSchemaVersion, and PRESERVE pre-existing rows with the column defaulted
// to ”.
func TestMigration_V2ToV3_LastWriteMutationIDAdded(t *testing.T) {
	path := createV2DB(t)

	s, err := Open(path) // triggers runMigrations → migrateV2ToV3 → migrateV3ToV4
	if err != nil {
		t.Fatalf("Open v2 DB: %v", err)
	}
	defer s.Close()

	// 1. user_version upgraded to currentSchemaVersion.
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if ver != currentSchemaVersion {
		t.Errorf("user_version = %d; want %d", ver, currentSchemaVersion)
	}

	// 2. Both tables now have the column.
	for _, table := range []string{"memories", "memory_tombstones"} {
		has, err := columnExists(s.db, table, "last_write_mutation_id")
		if err != nil {
			t.Fatalf("columnExists(%s): %v", table, err)
		}
		if !has {
			t.Errorf("%s: last_write_mutation_id column missing after v2→v3 migration", table)
		}
	}

	// 3. Pre-existing rows preserved with the column defaulted to ''.
	var memVal, tombVal string
	if err := s.db.QueryRow(
		`SELECT last_write_mutation_id FROM memories WHERE sync_id='v2-mem-1'`,
	).Scan(&memVal); err != nil {
		t.Fatalf("read migrated memories row: %v", err)
	}
	if memVal != "" {
		t.Errorf("migrated memories row last_write_mutation_id = %q; want '' (backfill default)", memVal)
	}
	if err := s.db.QueryRow(
		`SELECT last_write_mutation_id FROM memory_tombstones WHERE sync_id='v2-tomb-1'`,
	).Scan(&tombVal); err != nil {
		t.Fatalf("read migrated tombstone row: %v", err)
	}
	if tombVal != "" {
		t.Errorf("migrated tombstone row last_write_mutation_id = %q; want '' (backfill default)", tombVal)
	}

	// 4. Re-running migrations on the now-current DB is a no-op (idempotent).
	if err := runMigrations(s.db); err != nil {
		t.Errorf("re-running migrations on current DB must be a no-op, got: %v", err)
	}
}

// TestTombstone_MemoryTombstonesHasNoSeqColumn verifies that memory_tombstones
// does NOT have a seq column — it was removed when the tiebreaker changed from
// central seq to (writer_id, then the winning mutation_id via
// last_write_mutation_id). Having no seq column prevents the old tombstone seq
// roundtrip and proves the schema is clean.
func TestTombstone_MemoryTombstonesHasNoSeqColumn(t *testing.T) {
	s := openTempStore(t)

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
	if seqFound {
		t.Error("memory_tombstones must NOT have a seq column after the identity-tiebreaker change")
	}
}

// TestTombstone_WriterIDRoundtrip verifies that a tombstone written via Apply
// stores deleted_by (writer_id) AND last_write_mutation_id (the winning delete's
// content-addressed id), and that FindTombstone returns both populated.
// deleted_by is the penultimate tiebreaker field used by writeWins;
// last_write_mutation_id is the FINAL tiebreaker field.
func TestTombstone_WriterIDRoundtrip(t *testing.T) {
	s := openTempStore(t)

	// Seed the memories row that will be soft-deleted.
	if _, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('sync-wid-rt', 'sess1', 'memory', 'manual', 'WriterID Roundtrip', 'content', 'eng', 'project', 'writer-X')
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const wantWriter = "writer-X"
	m := domain.Mutation{
		MutationID: "mut-wid-rt",
		Op:         domain.OpDelete,
		SyncID:     "sync-wid-rt",
		SessionID:  "sess1",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "WriterID Roundtrip",
		Content:    "content",
		Project:    "eng",
		Scope:      "project",
		Version:    1,
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
		WriterID:   wantWriter,
	}
	if err := Apply(s.db, domain.Decision{Action: domain.ActionWriteTombstone, TargetSyncID: m.SyncID}, m); err != nil {
		t.Fatalf("Apply WriteTombstone: %v", err)
	}

	ts, err := s.FindTombstone("sync-wid-rt", nil, "eng", "project")
	if err != nil {
		t.Fatalf("FindTombstone: %v", err)
	}
	if ts == nil {
		t.Fatal("FindTombstone: expected tombstone, got nil")
	}
	if ts.DeletedBy != wantWriter {
		t.Errorf("ts.DeletedBy = %q; want %q (identity tiebreaker field)", ts.DeletedBy, wantWriter)
	}
	if ts.SyncID != m.SyncID {
		t.Errorf("ts.SyncID = %q; want %q (tombstone identity / PK)", ts.SyncID, m.SyncID)
	}
	if ts.LastWriteMutationID != m.MutationID {
		t.Errorf("ts.LastWriteMutationID = %q; want %q (final tiebreaker field — the winning delete's id)",
			ts.LastWriteMutationID, m.MutationID)
	}
}

// ── v3→v4 migration and topic-unique index enforcement tests ─────────────────

// createV3DB builds a SQLite file at user_version=3 — i.e. a DB produced by the
// v2→v3 migration (has last_write_mutation_id on both tables) but NOT yet migrated
// to v4 (memory_tombstones_topic_uidx is absent). Returns the path.
func createV3DB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "v3.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("createV3DB: open: %v", err)
	}
	defer db.Close()

	// memories with last_write_mutation_id (post-v3, pre-v4 column set).
	if _, err := db.Exec(`CREATE TABLE memories (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		sync_id         TEXT NOT NULL UNIQUE,
		session_id      TEXT NOT NULL,
		entity_type     TEXT NOT NULL DEFAULT 'memory',
		type            TEXT NOT NULL,
		status          TEXT,
		title           TEXT NOT NULL,
		content         TEXT NOT NULL DEFAULT '',
		project         TEXT NOT NULL DEFAULT '',
		scope           TEXT NOT NULL DEFAULT 'project',
		topic_key       TEXT,
		parent_sync_id  TEXT,
		version         INTEGER NOT NULL DEFAULT 1,
		seq             INTEGER NOT NULL DEFAULT 0,
		writer_id       TEXT NOT NULL DEFAULT '',
		last_write_mutation_id TEXT NOT NULL DEFAULT '',
		normalized_hash TEXT,
		embedding       BLOB,
		embedding_model TEXT,
		embedding_created_at TEXT,
		created_at      TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at      TEXT NOT NULL DEFAULT (datetime('now')),
		deleted_at      TEXT,
		review_after    TEXT,
		expires_at      TEXT
	)`); err != nil {
		t.Fatalf("createV3DB: create memories: %v", err)
	}

	// memory_tombstones with last_write_mutation_id but NO topic unique index.
	if _, err := db.Exec(`CREATE TABLE memory_tombstones (
		sync_id    TEXT PRIMARY KEY,
		project    TEXT NOT NULL DEFAULT '',
		scope      TEXT NOT NULL DEFAULT 'project',
		topic_key  TEXT,
		deleted_at TEXT NOT NULL,
		deleted_by TEXT NOT NULL,
		version    INTEGER NOT NULL DEFAULT 0,
		last_write_mutation_id TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("createV3DB: create memory_tombstones: %v", err)
	}

	// Seed one tombstone row so we can prove existing data survives migration.
	if _, err := db.Exec(`INSERT INTO memory_tombstones
		(sync_id, project, scope, topic_key, deleted_at, deleted_by, version, last_write_mutation_id)
		VALUES ('v3-tomb-1', 'eng', 'project', 'sdd/v3/topic', '2025-01-01T00:00:00Z', 'w-v3', 1, 'mut-v3')`); err != nil {
		t.Fatalf("createV3DB: seed memory_tombstones: %v", err)
	}

	if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatalf("createV3DB: set user_version: %v", err)
	}
	return path
}

// TestMigration_V3ToV4_TopicUniqueIndexAdded is the existing-DB upgrade proof
// for the ≤1-tombstone-per-topic schema enforcement. A v3 DB (has
// last_write_mutation_id, no topic unique index) is opened; migrateV3ToV4 MUST:
//   - add memory_tombstones_topic_uidx
//   - bump user_version to currentSchemaVersion (4)
//   - preserve pre-existing tombstone rows
//
// Idempotency: a fresh DB (ApplySchema already has the index) re-opening also
// succeeds (CREATE UNIQUE INDEX IF NOT EXISTS is a no-op).
func TestMigration_V3ToV4_TopicUniqueIndexAdded(t *testing.T) {
	path := createV3DB(t)

	s, err := Open(path) // triggers runMigrations → migrateV3ToV4
	if err != nil {
		t.Fatalf("Open v3 DB: %v", err)
	}
	defer s.Close()

	// 1. user_version upgraded to currentSchemaVersion.
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if ver != currentSchemaVersion {
		t.Errorf("user_version = %d; want %d", ver, currentSchemaVersion)
	}

	// 2. The index now exists in sqlite_master.
	var idxName string
	if err := s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='memory_tombstones_topic_uidx'`,
	).Scan(&idxName); err != nil {
		t.Fatalf("memory_tombstones_topic_uidx missing from sqlite_master: %v", err)
	}
	if idxName != "memory_tombstones_topic_uidx" {
		t.Errorf("index name = %q; want %q", idxName, "memory_tombstones_topic_uidx")
	}

	// 3. Pre-existing tombstone row is preserved.
	var syncID string
	if err := s.db.QueryRow(
		`SELECT sync_id FROM memory_tombstones WHERE sync_id='v3-tomb-1'`,
	).Scan(&syncID); err != nil {
		t.Fatalf("pre-existing tombstone row missing after migration: %v", err)
	}

	// 4. Re-running migrations on the now-current DB is a no-op (idempotent).
	if err := runMigrations(s.db); err != nil {
		t.Errorf("re-running migrations on current DB must be a no-op, got: %v", err)
	}

	// 5. Idempotency for fresh DBs: open a brand-new store (ApplySchema creates
	// the index) and confirm user_version == currentSchemaVersion, no error.
	fresh := openTempStore(t)
	var freshVer int
	if err := fresh.db.QueryRow(`PRAGMA user_version`).Scan(&freshVer); err != nil {
		t.Fatalf("fresh DB PRAGMA user_version: %v", err)
	}
	if freshVer != currentSchemaVersion {
		t.Errorf("fresh DB user_version = %d; want %d", freshVer, currentSchemaVersion)
	}
}

// TestTombstone_TopicUniqueIndex_SchemaEnforcesOnePerTopic is the enforcement
// proof for INV-B. It demonstrates that the schema now REJECTS a second tombstone
// for the same (topic_key, project, scope) under a DIFFERENT sync_id — a raw
// INSERT that intentionally bypasses Decide's canonical re-targeting.
//
// Setup: write a tombstone for topic T under sync-X via execWriteTombstone (which
// uses ON CONFLICT(sync_id) DO UPDATE, so it goes through the full store path).
// Adversarial probe: attempt a raw INSERT of a second tombstone for T under
// sync-Y (a different PK) — this simulates any future code path that forgets to
// call Decide and tries to insert a duplicate topic tombstone directly.
// Assert: the INSERT is REJECTED with a UNIQUE constraint error, proving the
// index is active and the invariant is schema-enforced.
func TestTombstone_TopicUniqueIndex_SchemaEnforcesOnePerTopic(t *testing.T) {
	s := openTempStore(t)

	tk := "sdd/test/topic-uidx-enforcement"
	tptr := &tk

	// Seed the memories row that will be soft-deleted.
	if _, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, topic_key, writer_id)
		VALUES ('sync-uidx-X', 'sess1', 'memory', 'manual', 'Topic Uidx X', 'content', 'eng', 'project', ?, 'writer-X')
	`, tk); err != nil {
		t.Fatalf("seed memories for sync-X: %v", err)
	}

	// Write the first tombstone for topic T under sync-X via the store path.
	now := time.Now().UTC()
	mX := domain.Mutation{
		MutationID: "mut-uidx-X",
		Op:         domain.OpDelete,
		SyncID:     "sync-uidx-X",
		SessionID:  "sess1",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Topic Uidx X",
		Content:    "content",
		Project:    "eng",
		Scope:      "project",
		TopicKey:   tptr,
		Version:    1,
		UpdatedAt:  now,
		OccurredAt: now,
		WriterID:   "writer-X",
	}
	if err := Apply(s.db, domain.Decision{Action: domain.ActionWriteTombstone, TargetSyncID: mX.SyncID}, mX); err != nil {
		t.Fatalf("Apply WriteTombstone for sync-X: %v", err)
	}

	// Confirm exactly one tombstone for topic T exists.
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM memory_tombstones WHERE topic_key=? AND project='eng' AND scope='project'`, tk,
	).Scan(&count); err != nil {
		t.Fatalf("count tombstones for T after first write: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 tombstone after first write, got %d", count)
	}

	// Adversarial probe: raw INSERT of a second tombstone under a different sync-Y.
	// This bypasses Decide's re-targeting and hits the unique index directly.
	_, insertErr := s.db.Exec(`
		INSERT INTO memory_tombstones
		  (sync_id, project, scope, topic_key, deleted_at, deleted_by, version, last_write_mutation_id)
		VALUES ('sync-uidx-Y', 'eng', 'project', ?, datetime('now'), 'writer-Y', 1, 'mut-uidx-Y')
	`, tk)
	if insertErr == nil {
		t.Fatal("expected UNIQUE constraint error on second tombstone for same topic, got nil — index is NOT enforcing the invariant")
	}
	// Verify it is a constraint violation (not some other error).
	if !strings.Contains(insertErr.Error(), "UNIQUE") && !strings.Contains(insertErr.Error(), "unique") {
		t.Errorf("expected a UNIQUE constraint error, got: %v", insertErr)
	}

	// Assert still exactly one tombstone for T (the second INSERT was rejected).
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM memory_tombstones WHERE topic_key=? AND project='eng' AND scope='project'`, tk,
	).Scan(&count); err != nil {
		t.Fatalf("count tombstones for T after failed second insert: %v", err)
	}
	if count != 1 {
		t.Errorf("expected still 1 tombstone after rejected insert, got %d", count)
	}
}

// TestTombstone_TopicUniqueIndex_NoTopicEmptyStringNotConstrained verifies the
// partial index treats topic_key=” as "no topic" (matching the domain's
// TopicKey != nil && *TopicKey != "" semantics) and does NOT constrain it. Two
// genuine no-topic tombstones that round-trip as topic_key=” under different
// sync_ids in the same (project, scope) must BOTH persist, since the ” exclusion
// in the index predicate prevents spurious UNIQUE violations.
func TestTombstone_TopicUniqueIndex_NoTopicEmptyStringNotConstrained(t *testing.T) {
	s := openTempStore(t)

	// Two no-topic tombstones with topic_key='' under DIFFERENT sync_ids, same
	// (project, scope). With the '' exclusion they are both "no topic" and coexist.
	for _, sync := range []string{"sync-empty-A", "sync-empty-B"} {
		if _, err := s.db.Exec(`
			INSERT INTO memory_tombstones
			  (sync_id, project, scope, topic_key, deleted_at, deleted_by, version, last_write_mutation_id)
			VALUES (?, 'eng', 'project', '', datetime('now'), 'writer', 1, ?)
		`, sync, "mut-"+sync); err != nil {
			t.Fatalf("insert no-topic ('') tombstone %s must succeed (index must not constrain ''): %v", sync, err)
		}
	}

	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM memory_tombstones WHERE topic_key='' AND project='eng' AND scope='project'`,
	).Scan(&count); err != nil {
		t.Fatalf("count empty-topic tombstones: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 coexisting no-topic ('') tombstones, got %d — the index must treat '' as no-topic", count)
	}
}

// TestTombstone_TopicUniqueIndex_ReDeleteUnaffected verifies that the normal
// re-delete path (Decide re-targets to the existing tombstone's sync_id) is
// completely unaffected by the unique index. execWriteTombstone uses ON CONFLICT
// (sync_id) DO UPDATE, so a re-delete with the same targetSyncID simply updates
// the mutable fields in-place — the topic unique index is never triggered.
func TestTombstone_TopicUniqueIndex_ReDeleteUnaffected(t *testing.T) {
	s := openTempStore(t)

	tk := "sdd/test/topic-uidx-redelete"
	tptr := &tk

	// Seed the memories row.
	if _, err := s.db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, topic_key, writer_id)
		VALUES ('sync-redelete-X', 'sess1', 'memory', 'manual', 'ReDelete Test', 'content', 'eng', 'project', ?, 'writer-X')
	`, tk); err != nil {
		t.Fatalf("seed memories: %v", err)
	}

	now := time.Now().UTC()
	mFirst := domain.Mutation{
		MutationID: "mut-redelete-1",
		Op:         domain.OpDelete,
		SyncID:     "sync-redelete-X",
		SessionID:  "sess1",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "ReDelete Test",
		Content:    "content",
		Project:    "eng",
		Scope:      "project",
		TopicKey:   tptr,
		Version:    1,
		UpdatedAt:  now,
		OccurredAt: now,
		WriterID:   "writer-X",
	}
	if err := Apply(s.db, domain.Decision{Action: domain.ActionWriteTombstone, TargetSyncID: mFirst.SyncID}, mFirst); err != nil {
		t.Fatalf("first Apply WriteTombstone: %v", err)
	}

	// Re-delete: same targetSyncID, different MutationID (idempotent re-apply
	// or a newer delete that Decide re-targeted to the canonical sync_id).
	mSecond := mFirst
	mSecond.MutationID = "mut-redelete-2"
	mSecond.UpdatedAt = now.Add(time.Second)
	mSecond.Version = 2
	if err := Apply(s.db, domain.Decision{Action: domain.ActionWriteTombstone, TargetSyncID: mFirst.SyncID}, mSecond); err != nil {
		t.Fatalf("re-delete Apply WriteTombstone: %v (ON CONFLICT(sync_id) path must succeed)", err)
	}

	// Still exactly one tombstone for topic T.
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM memory_tombstones WHERE topic_key=? AND project='eng' AND scope='project'`, tk,
	).Scan(&count); err != nil {
		t.Fatalf("count tombstones: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 tombstone after re-delete, got %d (must be idempotent)", count)
	}

	// The tombstone row reflects the SECOND delete's metadata.
	var mutID string
	if err := s.db.QueryRow(
		`SELECT last_write_mutation_id FROM memory_tombstones WHERE sync_id='sync-redelete-X'`,
	).Scan(&mutID); err != nil {
		t.Fatalf("read tombstone last_write_mutation_id: %v", err)
	}
	if mutID != mSecond.MutationID {
		t.Errorf("last_write_mutation_id = %q; want %q (second delete's id)", mutID, mSecond.MutationID)
	}
}

// ── migrateV4ToV5 migration tests ────────────────────────────────────────────

// createV4DB creates a minimal v4 schema SQLite DB at a temp path.
// The memories table includes seq INTEGER NOT NULL DEFAULT 0 (the column removed
// by migrateV4ToV5). user_version is set to 4 so Open/runMigrations enters the
// v4→v5 path. A seed row is inserted so we can prove data survives the migration.
func createV4DB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "v4.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("createV4DB: sql.Open: %v", err)
	}
	defer db.Close()

	// Create the memories table with seq (the v4 schema).
	if _, err := db.Exec(`CREATE TABLE memories (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		sync_id         TEXT    NOT NULL UNIQUE,
		session_id      TEXT    NOT NULL,
		entity_type     TEXT    NOT NULL DEFAULT 'memory',
		type            TEXT    NOT NULL,
		status          TEXT,
		title           TEXT    NOT NULL,
		content         TEXT    NOT NULL DEFAULT '',
		project         TEXT    NOT NULL DEFAULT '',
		scope           TEXT    NOT NULL DEFAULT 'project',
		topic_key       TEXT,
		parent_sync_id  TEXT,
		version         INTEGER NOT NULL DEFAULT 1,
		seq             INTEGER NOT NULL DEFAULT 0,
		writer_id       TEXT    NOT NULL DEFAULT '',
		last_write_mutation_id TEXT NOT NULL DEFAULT '',
		normalized_hash TEXT,
		embedding       BLOB,
		embedding_model TEXT,
		embedding_created_at TEXT,
		created_at      TEXT    NOT NULL DEFAULT (datetime('now')),
		updated_at      TEXT    NOT NULL DEFAULT (datetime('now')),
		deleted_at      TEXT,
		review_after    TEXT,
		expires_at      TEXT
	)`); err != nil {
		t.Fatalf("createV4DB: create memories: %v", err)
	}

	// Create remaining required tables so Open/ApplySchema can run idempotently.
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS memory_tombstones (
			sync_id    TEXT PRIMARY KEY,
			project    TEXT NOT NULL DEFAULT '',
			scope      TEXT NOT NULL DEFAULT 'project',
			topic_key  TEXT,
			deleted_at TEXT NOT NULL,
			deleted_by TEXT NOT NULL DEFAULT '',
			version    INTEGER NOT NULL DEFAULT 0,
			last_write_mutation_id TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS memory_relations (
			from_sync_id TEXT NOT NULL,
			to_sync_id   TEXT NOT NULL,
			rel_type     TEXT NOT NULL DEFAULT 'parent',
			PRIMARY KEY (from_sync_id, to_sync_id, rel_type)
		)`,
		`CREATE TABLE IF NOT EXISTS sync_mutations (
			local_seq    INTEGER PRIMARY KEY AUTOINCREMENT,
			mutation_id  TEXT NOT NULL UNIQUE,
			entity       TEXT NOT NULL DEFAULT '',
			entity_key   TEXT NOT NULL DEFAULT '',
			op           TEXT NOT NULL,
			payload      TEXT NOT NULL DEFAULT '',
			writer_id    TEXT NOT NULL DEFAULT '',
			occurred_at  TEXT NOT NULL DEFAULT (datetime('now')),
			acked_at     TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS sync_state (
			target_key       TEXT PRIMARY KEY DEFAULT 'central',
			last_acked_seq   INTEGER NOT NULL DEFAULT 0,
			last_pulled_seq  INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS applied_mutations (
			mutation_id TEXT PRIMARY KEY,
			applied_at  TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`INSERT OR IGNORE INTO sync_state(target_key) VALUES ('central')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("createV4DB: setup: %v", err)
		}
	}

	// Seed one memories row so we can verify data survives the DROP COLUMN.
	if _, err := db.Exec(`INSERT INTO memories
		(sync_id, session_id, entity_type, type, title, content, project, scope, seq, writer_id)
		VALUES ('v4-mem-1', 'sess-v4', 'memory', 'manual', 'V4 Row', 'v4 content', 'eng', 'project', 42, 'w-v4')`); err != nil {
		t.Fatalf("createV4DB: seed: %v", err)
	}

	if _, err := db.Exec(`PRAGMA user_version = 4`); err != nil {
		t.Fatalf("createV4DB: user_version: %v", err)
	}
	return path
}

// TestMigration_V4ToV5_SeqColumnDropped verifies that opening a v4 DB (which has
// memories.seq) runs migrateV4ToV5 and drops the column. Pre-existing rows must
// survive; user_version must reach currentSchemaVersion; and re-running is a no-op.
func TestMigration_V4ToV5_SeqColumnDropped(t *testing.T) {
	path := createV4DB(t)

	s, err := Open(path) // triggers runMigrations → …V4→V5
	if err != nil {
		t.Fatalf("Open v4 DB: %v", err)
	}
	defer s.Close()

	// 1. user_version upgraded to currentSchemaVersion.
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if ver != currentSchemaVersion {
		t.Errorf("user_version = %d; want %d", ver, currentSchemaVersion)
	}

	// 2. seq column is gone from memories.
	has, err := columnExists(s.db, "memories", "seq")
	if err != nil {
		t.Fatalf("columnExists(memories, seq): %v", err)
	}
	if has {
		t.Error("memories.seq column must be dropped by v4→v5 migration")
	}

	// 3. Pre-existing row survived (data is intact).
	var content string
	if err := s.db.QueryRow(
		`SELECT content FROM memories WHERE sync_id='v4-mem-1'`,
	).Scan(&content); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if content != "v4 content" {
		t.Errorf("migrated row content = %q; want %q", content, "v4 content")
	}

	// 4. Re-running migrations on the now-current DB is a no-op (idempotent).
	if err := runMigrations(s.db); err != nil {
		t.Errorf("re-running migrations on current DB must be a no-op: %v", err)
	}
}

// TestMigration_V4ToV5_FreshDB_NoSeqColumn verifies that a fresh DB opened via
// Open never has a memories.seq column (ApplySchema no longer creates it, and
// migrateV4ToV5 is a no-op when the column is absent).
func TestMigration_V4ToV5_FreshDB_NoSeqColumn(t *testing.T) {
	s := openTempStore(t) // fresh DB via Open → ApplySchema + runMigrations

	has, err := columnExists(s.db, "memories", "seq")
	if err != nil {
		t.Fatalf("columnExists: %v", err)
	}
	if has {
		t.Error("fresh DB must NOT have a memories.seq column (removed from schema in v5)")
	}

	// user_version must be at currentSchemaVersion.
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if ver != currentSchemaVersion {
		t.Errorf("fresh DB user_version = %d; want %d", ver, currentSchemaVersion)
	}
}

// TestMigration_V8ToV9_FreshDB verifies that a fresh DB opened via Open:
//   - has user_version == 9 (currentSchemaVersion)
//   - has user_prompts and prompt_tombstones tables
//   - accepts an insert into both tables (column set is correct)
func TestMigration_V8ToV9_FreshDB(t *testing.T) {
	s := openTempStore(t)

	// user_version must be at currentSchemaVersion.
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if ver != currentSchemaVersion {
		t.Errorf("fresh DB user_version = %d; want %d (currentSchemaVersion)", ver, currentSchemaVersion)
	}

	// Both tables must exist.
	for _, tbl := range []string{"user_prompts", "prompt_tombstones"} {
		var name string
		if err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&name); err != nil {
			t.Errorf("table %q not found after migrateV8ToV9: %v", tbl, err)
		}
	}

	// Insert into user_prompts — verifies column set matches DDL.
	_, err := s.db.Exec(`
		INSERT INTO user_prompts (sync_id, session_id, content, project, writer_id)
		VALUES ('prompt-v9-1', 'sess-v9', 'hello world', 'engram', 'writer-test')
	`)
	if err != nil {
		t.Fatalf("INSERT into user_prompts on fresh DB: %v", err)
	}

	// Insert into prompt_tombstones — verifies column set matches DDL.
	_, err = s.db.Exec(`
		INSERT INTO prompt_tombstones (sync_id, session_id, project, deleted_at, deleted_by)
		VALUES ('prompt-v9-1', 'sess-v9', 'engram', datetime('now'), 'writer-test')
	`)
	if err != nil {
		t.Fatalf("INSERT into prompt_tombstones on fresh DB: %v", err)
	}
}

// TestMigration_V8ToV9_ExistingDB verifies that migrateV8ToV9 applies correctly
// to a DB that was at user_version=8 (the conflict_relations version).  It
// manually constructs such a DB, runs runMigrations, then asserts:
//   - user_version is 9 after migration
//   - user_prompts and prompt_tombstones tables exist and are writable
//   - re-running runMigrations on the migrated DB is a no-op (idempotent)
func TestMigration_V8ToV9_ExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v8.db")

	// Open a raw DB and apply the full current schema, then wind user_version
	// back to 8 to simulate a DB that hasn't seen the v8→v9 migration yet.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open v8 DB: %v", err)
	}
	defer db.Close()

	if err := ApplySchema(db); err != nil {
		t.Fatalf("ApplySchema: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 8`); err != nil {
		t.Fatalf("set user_version=8: %v", err)
	}

	// Drop the v9 tables so migrateV8ToV9 must actually CREATE them — a real v8 DB
	// has no user_prompts/prompt_tombstones. Without this, ApplySchema already made
	// them and the migration's CREATE IF NOT EXISTS would be a silent no-op that
	// could not catch a DDL typo.
	for _, drop := range []string{`DROP TABLE IF EXISTS user_prompts`, `DROP TABLE IF EXISTS prompt_tombstones`} {
		if _, err := db.Exec(drop); err != nil {
			t.Fatalf("drop v9 table: %v", err)
		}
	}

	// Insert a pre-existing memory to confirm data survives the migration.
	if _, err := db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('v8-mem-1', 'sess1', 'memory', 'manual', 'Pre-v9 Memory', 'pre-v9 content', 'p', 'project', 'w')
	`); err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}

	// Now run migrations — should advance from v8 all the way to the current
	// schema version (v10 as of PR-②).
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations from v8: %v", err)
	}

	// 1. user_version must now be at the current schema version.
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if ver != currentSchemaVersion {
		t.Errorf("user_version = %d after runMigrations; want %d", ver, currentSchemaVersion)
	}

	// 2. Both new tables must exist.
	for _, tbl := range []string{"user_prompts", "prompt_tombstones"} {
		var name string
		if err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&name); err != nil {
			t.Errorf("table %q not found after migrateV8ToV9: %v", tbl, err)
		}
	}

	// 3. Both tables must be writable.
	if _, err := db.Exec(`
		INSERT INTO user_prompts (sync_id, session_id, content, project, writer_id)
		VALUES ('prompt-migrated-1', 'sess1', 'migrated prompt', 'p', 'w')
	`); err != nil {
		t.Fatalf("INSERT user_prompts after migration: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO prompt_tombstones (sync_id, session_id, project, deleted_at, deleted_by)
		VALUES ('prompt-migrated-1', 'sess1', 'p', datetime('now'), 'w')
	`); err != nil {
		t.Fatalf("INSERT prompt_tombstones after migration: %v", err)
	}

	// 4. Pre-existing memory data survived.
	var content string
	if err := db.QueryRow(
		`SELECT content FROM memories WHERE sync_id='v8-mem-1'`,
	).Scan(&content); err != nil {
		t.Fatalf("read pre-migration row after migration: %v", err)
	}
	if content != "pre-v9 content" {
		t.Errorf("pre-migration row content = %q; want %q", content, "pre-v9 content")
	}

	// 5. Re-running migrations is idempotent.
	if err := runMigrations(db); err != nil {
		t.Errorf("re-running migrations on current-version DB must be a no-op: %v", err)
	}
}

// ── PR-② schema v10 migration tests ──────────────────────────────────────────

// TestMigration_V9ToV10_TableCreated verifies that migrateV9ToV10 creates the
// project_policy table and bumps user_version to 10.
func TestMigration_V9ToV10_TableCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v9.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Bootstrap schema, then wind back to v9 and drop the project_policy table
	// so the migration must actually CREATE it.
	if err := ApplySchema(db); err != nil {
		t.Fatalf("ApplySchema: %v", err)
	}
	for _, stmt := range []string{
		`PRAGMA user_version = 9`,
		`DROP TABLE IF EXISTS project_policy`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations from v9: %v", err)
	}

	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if ver != 10 {
		t.Errorf("user_version = %d; want 10", ver)
	}

	var tblName string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='project_policy'`,
	).Scan(&tblName); err != nil {
		t.Errorf("project_policy table not found after migrateV9ToV10: %v", err)
	}
}

// TestMigration_V9ToV10_Idempotent verifies that running runMigrations on a DB
// already at v10 is a no-op and does not return an error.
func TestMigration_V9ToV10_Idempotent(t *testing.T) {
	st := openTempStore(t)

	var ver int
	if err := st.DB().QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if ver != currentSchemaVersion {
		t.Fatalf("pre-condition: user_version=%d, want %d", ver, currentSchemaVersion)
	}

	// Re-running on a current-version DB must be a no-op.
	if err := runMigrations(st.DB()); err != nil {
		t.Errorf("runMigrations on current-version DB: %v", err)
	}
	// user_version must not change.
	if err := st.DB().QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("user_version after re-run: %v", err)
	}
	if ver != currentSchemaVersion {
		t.Errorf("user_version changed to %d after no-op re-run; want %d", ver, currentSchemaVersion)
	}
}

// TestSchema_V10_CheckConstraint verifies that the CHECK constraint on the
// project_policy table rejects INSERT attempts with invalid policy values.
func TestSchema_V10_CheckConstraint(t *testing.T) {
	st := openTempStore(t)

	// Valid values must be accepted.
	for _, valid := range []string{"synced", "local-only", "omitted"} {
		if _, err := st.DB().Exec(
			`INSERT OR REPLACE INTO project_policy (project, policy, updated_at)
			 VALUES (?, ?, datetime('now'))`, "test-proj-"+valid, valid,
		); err != nil {
			t.Errorf("valid policy %q rejected: %v", valid, err)
		}
	}

	// An invalid value must be rejected by the CHECK constraint.
	_, err := st.DB().Exec(
		`INSERT INTO project_policy (project, policy, updated_at)
		 VALUES ('bad-proj', 'unknown', datetime('now'))`,
	)
	if err == nil {
		t.Error("expected CHECK constraint violation for policy='unknown', got nil")
	}
}

// Compile-time check: Store must satisfy domain.Reader.
var _ domain.Reader = (*Store)(nil)

// Ensure fmt is used (for syncID in type check tests if needed elsewhere).
var _ = fmt.Sprintf
