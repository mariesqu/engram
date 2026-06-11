package importer_test

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mariesqu/engram/internal/importer"
	"github.com/mariesqu/engram/internal/localstore"
)

// ── fixture helpers ───────────────────────────────────────────────────────────

// legacyDDL is the minimal DDL needed to seed a source DB that matches the
// old-generation engram schema (observations, user_prompts, sessions).
// Sourced from old_code/internal/store/store_legacy_ddl_test.go.
const legacyDDL = `
CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    project    TEXT NOT NULL,
    directory  TEXT NOT NULL,
    started_at TEXT NOT NULL DEFAULT (datetime('now')),
    ended_at   TEXT,
    summary    TEXT
);

CREATE TABLE IF NOT EXISTS observations (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    sync_id              TEXT,
    session_id           TEXT    NOT NULL,
    type                 TEXT    NOT NULL,
    title                TEXT    NOT NULL,
    content              TEXT    NOT NULL,
    tool_name            TEXT,
    project              TEXT,
    scope                TEXT    NOT NULL DEFAULT 'project',
    topic_key            TEXT,
    normalized_hash      TEXT,
    revision_count       INTEGER NOT NULL DEFAULT 1,
    duplicate_count      INTEGER NOT NULL DEFAULT 1,
    last_seen_at         TEXT,
    created_at           TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at           TEXT    NOT NULL DEFAULT (datetime('now')),
    deleted_at           TEXT,
    review_after         TEXT,
    expires_at           TEXT,
    embedding            BLOB,
    embedding_model      TEXT,
    embedding_created_at TEXT,
    FOREIGN KEY (session_id) REFERENCES sessions(id)
);

CREATE TABLE IF NOT EXISTS user_prompts (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    sync_id    TEXT,
    session_id TEXT    NOT NULL,
    content    TEXT    NOT NULL,
    project    TEXT,
    created_at TEXT    NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (session_id) REFERENCES sessions(id)
);

CREATE TABLE IF NOT EXISTS prompt_tombstones (
    sync_id    TEXT PRIMARY KEY,
    session_id TEXT,
    project    TEXT,
    deleted_at TEXT NOT NULL DEFAULT (datetime('now'))
);
`

// seedSourceDB creates a raw old-generation SQLite file, applies legacyDDL, and
// inserts a representative set of rows (see inline comments).  Returns the file
// path; the caller is responsible for cleanup via t.TempDir().
//
// Seeded rows:
//  1. session "sess-1" for project "engram"
//  2. live observation with explicit sync_id "obs-live-1" (topic "t/live")
//  3. live observation with NULL sync_id (id will be assigned by AUTOINCREMENT)
//  4. soft-deleted observation (deleted_at set) — must be SKIPPED
//  5. two observations for topic "t/topic" with different created_at — latest must win
//  6. observation for project "" (NULL in source) — NULL project maps to ""
//  7. live user_prompt with explicit sync_id "prompt-live-1"
//  8. live user_prompt with NULL sync_id
func seedSourceDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "old_engram.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("seedSourceDB: open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		t.Fatalf("seedSourceDB: WAL: %v", err)
	}
	if _, err := db.Exec(legacyDDL); err != nil {
		t.Fatalf("seedSourceDB: DDL: %v", err)
	}

	stmts := []string{
		// 1. session
		`INSERT INTO sessions (id, project, directory, started_at)
		 VALUES ('sess-1', 'engram', '/home/user/engram', '2024-01-01 10:00:00')`,

		// 2. live obs with explicit sync_id
		`INSERT INTO observations
		   (sync_id, session_id, type, title, content, project, scope, topic_key, created_at, updated_at)
		 VALUES ('obs-live-1', 'sess-1', 'decision', 'Live Decision', 'content live',
		         'engram', 'project', 't/live', '2024-01-01 11:00:00', '2024-01-01 12:00:00')`,

		// 3. live obs with NULL sync_id
		`INSERT INTO observations
		   (sync_id, session_id, type, title, content, project, scope, created_at, updated_at)
		 VALUES (NULL, 'sess-1', 'bugfix', 'Null SyncID', 'content null',
		         'engram', 'project', '2024-01-02 09:00:00', '2024-01-02 10:00:00')`,

		// 4. soft-deleted obs (must be skipped)
		`INSERT INTO observations
		   (sync_id, session_id, type, title, content, project, scope, created_at, updated_at, deleted_at)
		 VALUES ('obs-deleted-1', 'sess-1', 'manual', 'Deleted Obs', 'deleted content',
		         'engram', 'project', '2024-01-03 09:00:00', '2024-01-03 10:00:00', '2024-01-03 11:00:00')`,

		// 5a. first revision of topic "t/topic"
		`INSERT INTO observations
		   (sync_id, session_id, type, title, content, project, scope, topic_key, created_at, updated_at)
		 VALUES ('obs-topic-v1', 'sess-1', 'architecture', 'Topic V1', 'v1 content',
		         'engram', 'project', 't/topic', '2024-01-04 09:00:00', '2024-01-04 09:00:00')`,

		// 5b. second revision of same topic — later created_at/updated_at
		`INSERT INTO observations
		   (sync_id, session_id, type, title, content, project, scope, topic_key, created_at, updated_at)
		 VALUES ('obs-topic-v2', 'sess-1', 'architecture', 'Topic V2', 'v2 content',
		         'engram', 'project', 't/topic', '2024-01-04 10:00:00', '2024-01-04 10:00:00')`,

		// 6. observation with NULL project
		`INSERT INTO observations
		   (sync_id, session_id, type, title, content, project, scope, created_at, updated_at)
		 VALUES ('obs-null-proj', 'sess-1', 'manual', 'Null Project', 'np content',
		         NULL, 'project', '2024-01-05 09:00:00', '2024-01-05 10:00:00')`,

		// 7. prompt with explicit sync_id
		`INSERT INTO user_prompts (sync_id, session_id, content, project, created_at)
		 VALUES ('prompt-live-1', 'sess-1', 'remember this', 'engram', '2024-01-01 11:30:00')`,

		// 8. prompt with NULL sync_id
		`INSERT INTO user_prompts (sync_id, session_id, content, project, created_at)
		 VALUES (NULL, 'sess-1', 'second prompt', 'engram', '2024-01-02 11:30:00')`,
	}

	for i, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seedSourceDB stmt[%d]: %v\n%s", i, err, s)
		}
	}
	return path
}

// openDest opens a fresh destination store in a temp dir.
func openDest(t *testing.T) *localstore.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "dest.db")
	s, err := localstore.Open(path)
	if err != nil {
		t.Fatalf("openDest: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// openSrcRO opens the source file in read-only mode via importer.OpenSourceReadOnly.
func openSrcRO(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := importer.OpenSourceReadOnly(path)
	if err != nil {
		t.Fatalf("OpenSourceReadOnly: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// fileHash returns the SHA-256 of the file at path.
func fileHash(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("fileHash: open: %v", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("fileHash: copy: %v", err)
	}
	return h.Sum(nil)
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestImport_FullCounts verifies that a seeded source DB produces the expected
// import counts (imported, skipped-deleted, etc.).
func TestImport_FullCounts(t *testing.T) {
	srcPath := seedSourceDB(t)
	dst := openDest(t)
	srcDB := openSrcRO(t, srcPath)

	imp := importer.New(dst, "import")
	st, err := imp.Run(srcDB, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 1 session
	if st.SessionsImported != 1 {
		t.Errorf("SessionsImported = %d; want 1", st.SessionsImported)
	}
	if st.SessionsSkipped != 0 {
		t.Errorf("SessionsSkipped = %d; want 0", st.SessionsSkipped)
	}

	// 5 live observations (obs-live-1, null-sync-id, obs-topic-v1, obs-topic-v2,
	// obs-null-proj); 1 soft-deleted skipped.
	if st.MemoriesImported != 5 {
		t.Errorf("MemoriesImported = %d; want 5", st.MemoriesImported)
	}
	if st.MemoriesDeleted != 1 {
		t.Errorf("MemoriesDeleted = %d; want 1", st.MemoriesDeleted)
	}
	if st.MemoriesSkipped != 0 {
		t.Errorf("MemoriesSkipped = %d; want 0", st.MemoriesSkipped)
	}

	// 2 live prompts
	if st.PromptsImported != 2 {
		t.Errorf("PromptsImported = %d; want 2", st.PromptsImported)
	}
	if st.PromptsSkipped != 0 {
		t.Errorf("PromptsSkipped = %d; want 0", st.PromptsSkipped)
	}
}

// TestImport_Idempotent verifies that re-running the import produces zero new
// writes (all rows are skipped-existing) and the total imported count is 0.
func TestImport_Idempotent(t *testing.T) {
	srcPath := seedSourceDB(t)
	dst := openDest(t)
	srcDB := openSrcRO(t, srcPath)

	imp := importer.New(dst, "import")

	// First run.
	if _, err := imp.Run(srcDB, false); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Second run must import nothing new.
	st2, err := imp.Run(srcDB, false)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if st2.MemoriesImported != 0 {
		t.Errorf("idempotent: MemoriesImported = %d; want 0", st2.MemoriesImported)
	}
	if st2.PromptsImported != 0 {
		t.Errorf("idempotent: PromptsImported = %d; want 0", st2.PromptsImported)
	}
	if st2.SessionsImported != 0 {
		t.Errorf("idempotent: SessionsImported = %d; want 0", st2.SessionsImported)
	}
	// Skipped counts should equal first-run imported counts.
	if st2.MemoriesSkipped != 5 {
		t.Errorf("idempotent: MemoriesSkipped = %d; want 5", st2.MemoriesSkipped)
	}
	if st2.PromptsSkipped != 2 {
		t.Errorf("idempotent: PromptsSkipped = %d; want 2", st2.PromptsSkipped)
	}
	if st2.SessionsSkipped != 1 {
		t.Errorf("idempotent: SessionsSkipped = %d; want 1", st2.SessionsSkipped)
	}
}

// TestImport_DryRun verifies that dry-run produces correct counts but writes nothing.
func TestImport_DryRun(t *testing.T) {
	srcPath := seedSourceDB(t)
	dst := openDest(t)
	srcDB := openSrcRO(t, srcPath)

	imp := importer.New(dst, "import")
	st, err := imp.Run(srcDB, true)
	if err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}

	// Counts are non-zero.
	if st.MemoriesImported != 5 {
		t.Errorf("dry-run MemoriesImported = %d; want 5", st.MemoriesImported)
	}
	if st.PromptsImported != 2 {
		t.Errorf("dry-run PromptsImported = %d; want 2", st.PromptsImported)
	}

	// No actual writes: the destination store has no memories.
	recs, err := dst.SearchMemories("Live Decision", "engram", 10)
	if err != nil {
		t.Fatalf("SearchMemories after dry-run: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("dry-run: expected 0 memories in dest, got %d", len(recs))
	}
}

// TestImport_DeletedSkipped verifies soft-deleted observations are not imported.
func TestImport_DeletedSkipped(t *testing.T) {
	srcPath := seedSourceDB(t)
	dst := openDest(t)
	srcDB := openSrcRO(t, srcPath)

	imp := importer.New(dst, "import")
	st, err := imp.Run(srcDB, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.MemoriesDeleted != 1 {
		t.Errorf("MemoriesDeleted = %d; want 1", st.MemoriesDeleted)
	}

	// The deleted obs must NOT appear in the destination.
	rec, err := dst.FindBySyncID("obs-deleted-1")
	if err != nil {
		t.Fatalf("FindBySyncID obs-deleted-1: %v", err)
	}
	if rec != nil {
		t.Error("soft-deleted source observation must not appear in destination")
	}
}

// TestImport_TopicConvergence verifies that two revisions of the same topic_key
// result in a single live row containing the LATEST content ("v2 content").
func TestImport_TopicConvergence(t *testing.T) {
	srcPath := seedSourceDB(t)
	dst := openDest(t)
	srcDB := openSrcRO(t, srcPath)

	imp := importer.New(dst, "import")
	if _, err := imp.Run(srcDB, false); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rec, err := dst.FindByTopic("t/topic", "engram", "project")
	if err != nil {
		t.Fatalf("FindByTopic: %v", err)
	}
	if rec == nil {
		t.Fatal("FindByTopic returned nil — topic not imported")
	}
	if rec.Content != "v2 content" {
		t.Errorf("topic content = %q; want %q", rec.Content, "v2 content")
	}
}

// TestImport_TimestampsPreserved verifies that the imported memory's UpdatedAt
// reflects the source row's updated_at, not the import time.
func TestImport_TimestampsPreserved(t *testing.T) {
	srcPath := seedSourceDB(t)
	dst := openDest(t)
	srcDB := openSrcRO(t, srcPath)

	imp := importer.New(dst, "import")
	if _, err := imp.Run(srcDB, false); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rec, err := dst.FindBySyncID("obs-live-1")
	if err != nil {
		t.Fatalf("FindBySyncID: %v", err)
	}
	if rec == nil {
		t.Fatal("FindBySyncID returned nil for obs-live-1")
	}

	wantUpdatedAt := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	if !rec.UpdatedAt.Equal(wantUpdatedAt) {
		t.Errorf("UpdatedAt = %v; want %v", rec.UpdatedAt, wantUpdatedAt)
	}
}

// TestImport_OutboxPopulated verifies that imported memories produce outbox
// entries (PendingCount > 0), meaning they will sync to central.
func TestImport_OutboxPopulated(t *testing.T) {
	srcPath := seedSourceDB(t)
	dst := openDest(t)
	srcDB := openSrcRO(t, srcPath)

	imp := importer.New(dst, "import")
	if _, err := imp.Run(srcDB, false); err != nil {
		t.Fatalf("Run: %v", err)
	}

	n, err := dst.PendingCount()
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if n == 0 {
		t.Error("PendingCount = 0 after import; expected outbox entries for sync")
	}
}

// TestImport_OmittedProjectSkipped verifies that rows belonging to a project with
// policy=omitted are skipped and counted in MemoriesOmitted / PromptsOmitted.
func TestImport_OmittedProjectSkipped(t *testing.T) {
	srcPath := seedSourceDB(t)
	dst := openDest(t)
	srcDB := openSrcRO(t, srcPath)

	// Set "engram" project to omitted so all its rows are refused.
	if err := dst.SetPolicy("engram", localstore.PolicyOmitted); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	imp := importer.New(dst, "import")
	st, err := imp.Run(srcDB, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// All "engram" observations must be omitted (obs-live-1, null-sync, obs-topic-v1/v2,
	// obs-null-proj is project="" which is NOT "engram" so it passes through).
	// obs-deleted-1 is skipped before policy check.
	if st.MemoriesOmitted < 4 {
		t.Errorf("MemoriesOmitted = %d; want >= 4", st.MemoriesOmitted)
	}
	if st.MemoriesImported > 1 {
		t.Errorf("MemoriesImported = %d; want <= 1 (only null-project row)", st.MemoriesImported)
	}

	// Prompts for "engram" must be omitted.
	if st.PromptsOmitted < 2 {
		t.Errorf("PromptsOmitted = %d; want >= 2", st.PromptsOmitted)
	}
}

// TestImport_ReadOnlySource verifies that the source file is unchanged after import
// by comparing its SHA-256 hash before and after.
func TestImport_ReadOnlySource(t *testing.T) {
	srcPath := seedSourceDB(t)
	hashBefore := fileHash(t, srcPath)

	dst := openDest(t)
	srcDB := openSrcRO(t, srcPath)

	imp := importer.New(dst, "import")
	if _, err := imp.Run(srcDB, false); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = srcDB.Close()

	hashAfter := fileHash(t, srcPath)
	if fmt.Sprintf("%x", hashBefore) != fmt.Sprintf("%x", hashAfter) {
		t.Error("source file was modified during import — read-only guarantee violated")
	}
}

// TestImport_SameFileRefused is covered at the CLI layer (cmd/engram/import.go),
// but we verify OpenSourceReadOnly fails fast on a non-existent path.
func TestImport_MissingSource(t *testing.T) {
	_, err := importer.OpenSourceReadOnly("/nonexistent/path/engram.db")
	if err == nil {
		t.Error("expected error opening non-existent source path, got nil")
	}
}

// TestImport_InvalidSource verifies a friendly error when the source file exists
// but does not have the expected old-generation tables.
func TestImport_InvalidSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not_engram.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE foo (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	_ = db.Close()

	srcDB, err := importer.OpenSourceReadOnly(path)
	if err != nil {
		t.Fatalf("OpenSourceReadOnly: %v", err)
	}
	defer srcDB.Close()

	dst := openDest(t)
	imp := importer.New(dst, "import")
	_, err = imp.Run(srcDB, false)
	if err == nil {
		t.Error("expected error for non-engram source DB, got nil")
	}
	if !containsAny(err.Error(), "not an old-generation engram database") {
		t.Errorf("error message %q does not contain expected fragment", err.Error())
	}
}

// TestImport_NullProjectMapsToEmpty verifies observations with NULL project map
// to "" in the destination store.
func TestImport_NullProjectMapsToEmpty(t *testing.T) {
	srcPath := seedSourceDB(t)
	dst := openDest(t)
	srcDB := openSrcRO(t, srcPath)

	imp := importer.New(dst, "import")
	if _, err := imp.Run(srcDB, false); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rec, err := dst.FindBySyncID("obs-null-proj")
	if err != nil {
		t.Fatalf("FindBySyncID obs-null-proj: %v", err)
	}
	if rec == nil {
		t.Fatal("obs-null-proj not found in destination")
	}
	if rec.Project != "" {
		t.Errorf("Project = %q; want empty string", rec.Project)
	}
}

// TestImport_DeterministicSyncIDForNullSource verifies that a NULL source sync_id
// gets a stable deterministic ID ("import-obs-<id>") so re-runs can skip it.
func TestImport_DeterministicSyncIDForNullSource(t *testing.T) {
	srcPath := seedSourceDB(t)
	dst := openDest(t)
	srcDB := openSrcRO(t, srcPath)

	imp := importer.New(dst, "import")
	if _, err := imp.Run(srcDB, false); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Obtain the auto-assigned id of the NULL-sync_id observation from source.
	var nullObsID int64
	err := srcDB.QueryRow(
		`SELECT id FROM observations WHERE sync_id IS NULL LIMIT 1`,
	).Scan(&nullObsID)
	if err != nil {
		t.Fatalf("get NULL obs id: %v", err)
	}

	expectedSyncID := fmt.Sprintf("import-obs-%d", nullObsID)
	rec, err := dst.FindBySyncID(expectedSyncID)
	if err != nil {
		t.Fatalf("FindBySyncID %q: %v", expectedSyncID, err)
	}
	if rec == nil {
		t.Errorf("deterministic sync_id %q not found in destination", expectedSyncID)
	}
}

// ── helper ────────────────────────────────────────────────────────────────────

func containsAny(s string, substrings ...string) bool {
	for _, sub := range substrings {
		if len(sub) > 0 {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
