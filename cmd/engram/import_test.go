package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// legacyImportDDL mirrors the old-generation engram schema used in
// internal/importer tests — kept local so cmd tests have no upward dependency
// on the importer test package.
const legacyImportDDL = `
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    project TEXT NOT NULL,
    directory TEXT NOT NULL,
    started_at TEXT NOT NULL DEFAULT (datetime('now')),
    ended_at TEXT,
    summary TEXT
);
CREATE TABLE IF NOT EXISTS observations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    sync_id TEXT,
    session_id TEXT NOT NULL,
    type TEXT NOT NULL,
    title TEXT NOT NULL,
    content TEXT NOT NULL,
    tool_name TEXT,
    project TEXT,
    scope TEXT NOT NULL DEFAULT 'project',
    topic_key TEXT,
    normalized_hash TEXT,
    revision_count INTEGER NOT NULL DEFAULT 1,
    duplicate_count INTEGER NOT NULL DEFAULT 1,
    last_seen_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    deleted_at TEXT,
    review_after TEXT,
    expires_at TEXT,
    embedding BLOB,
    embedding_model TEXT,
    embedding_created_at TEXT
);
CREATE TABLE IF NOT EXISTS user_prompts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    sync_id TEXT,
    session_id TEXT NOT NULL,
    content TEXT NOT NULL,
    project TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS prompt_tombstones (
    sync_id TEXT PRIMARY KEY,
    session_id TEXT,
    project TEXT,
    deleted_at TEXT NOT NULL DEFAULT (datetime('now'))
);
`

// makeOldDB creates a minimal old-generation engram SQLite file and returns its
// path. Used by CLI-level import tests.
func makeOldDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("makeOldDB: open: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(legacyImportDDL); err != nil {
		t.Fatalf("makeOldDB: DDL: %v", err)
	}
	// Seed a session and one observation.
	if _, err := db.Exec(`INSERT INTO sessions (id,project,directory) VALUES ('s1','p1','/d')`); err != nil {
		t.Fatalf("makeOldDB: session: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO observations
		(sync_id,session_id,type,title,content,project,scope,created_at,updated_at)
		VALUES ('obs-cli-1','s1','manual','CLI Title','CLI content','p1','project','2024-02-01 10:00:00','2024-02-01 11:00:00')`); err != nil {
		t.Fatalf("makeOldDB: observation: %v", err)
	}
	_ = db.Close()
	return path
}

// makeNewDB creates a fresh destination engram.db path (not yet opened by Store).
func makeNewDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "new.db")
}

// TestRunImportCmd_Basic verifies the import command exits 0 and writes data.
func TestRunImportCmd_Basic(t *testing.T) {
	oldPath := makeOldDB(t)
	newPath := makeNewDB(t)

	code := run([]string{"import", "--from", oldPath, "--db", newPath})
	if code != 0 {
		t.Fatalf("run import: exit code %d; want 0", code)
	}

	// Verify the destination DB has the imported observation.
	db, err := sql.Open("sqlite", newPath)
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories WHERE sync_id='obs-cli-1'`).Scan(&count); err != nil {
		t.Fatalf("query memories: %v", err)
	}
	if count != 1 {
		t.Errorf("memories count = %d; want 1", count)
	}
}

// TestRunImportCmd_DryRun verifies --dry-run writes nothing.
func TestRunImportCmd_DryRun(t *testing.T) {
	oldPath := makeOldDB(t)
	newPath := makeNewDB(t)

	code := run([]string{"import", "--from", oldPath, "--db", newPath, "--dry-run"})
	if code != 0 {
		t.Fatalf("dry-run import: exit code %d; want 0", code)
	}

	// The dest DB is created by localstore.Open but should have 0 memories.
	db, err := sql.Open("sqlite", newPath)
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&count); err != nil {
		t.Fatalf("query memories: %v", err)
	}
	if count != 0 {
		t.Errorf("dry-run: memories count = %d; want 0", count)
	}
}

// TestRunImportCmd_SameFile verifies refusing import when --from == --db.
func TestRunImportCmd_SameFile(t *testing.T) {
	oldPath := makeOldDB(t)

	code := run([]string{"import", "--from", oldPath, "--db", oldPath})
	if code == 0 {
		t.Error("expected non-zero exit when --from == --db; got 0")
	}
}

// TestRunImportCmd_MissingFrom verifies exit 1 when source does not exist.
func TestRunImportCmd_MissingFrom(t *testing.T) {
	newPath := makeNewDB(t)
	code := run([]string{"import", "--from", "/no/such/file.db", "--db", newPath})
	if code == 0 {
		t.Error("expected non-zero exit for missing source; got 0")
	}
}

// TestRunImportCmd_MissingDB verifies exit 1 when --db is omitted.
func TestRunImportCmd_MissingDB(t *testing.T) {
	oldPath := makeOldDB(t)
	code := run([]string{"import", "--from", oldPath})
	if code == 0 {
		t.Error("expected non-zero exit when --db omitted; got 0")
	}
}

// TestRunImportCmd_InvalidSource verifies exit 1 for a non-engram source.
func TestRunImportCmd_InvalidSource(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "notengram.db")
	db, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE foo (id INTEGER)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	_ = db.Close()

	newPath := makeNewDB(t)
	code := run([]string{"import", "--from", srcPath, "--db", newPath})
	if code == 0 {
		t.Error("expected non-zero exit for invalid source schema; got 0")
	}
}

// TestRunImportCmd_Help verifies --help returns exit 2.
func TestRunImportCmd_Help(t *testing.T) {
	code := run([]string{"import", "--help"})
	// flag.ErrHelp causes ContinueOnError to return it; runImportCmd returns nil,
	// so the top-level run returns 0 (same as other help exits in this binary).
	if code != 0 {
		t.Errorf("import --help: exit code %d; want 0", code)
	}
}
