package localstore

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSchemaVersionIsThirteen pins the schema version bump for the
// obsidian-narrative Phase B narratives table (v12 -> v13).
func TestSchemaVersionIsThirteen(t *testing.T) {
	if currentSchemaVersion != 13 {
		t.Fatalf("currentSchemaVersion = %d, want 13", currentSchemaVersion)
	}

	st := openTempStore(t)
	var ver int
	if err := st.DB().QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if ver != 13 {
		t.Errorf("user_version after Open() on a fresh DB = %d, want 13", ver)
	}
}

// narrativesColumnInfo is one row of PRAGMA table_info(narratives).
type narrativesColumnInfo struct {
	name string
	pk   int
}

func readNarrativesColumns(t *testing.T, db *sql.DB) []narrativesColumnInfo {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(narratives)`)
	if err != nil {
		t.Fatalf("table_info(narratives): %v", err)
	}
	defer rows.Close()

	var cols []narrativesColumnInfo
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			t.Fatalf("table_info scan: %v", err)
		}
		cols = append(cols, narrativesColumnInfo{name: name, pk: pk})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows.Err: %v", err)
	}
	return cols
}

// TestFreshDBHasNarrativesTable verifies that Open() on a fresh DB (which
// runs ApplySchema then runMigrations end to end) leaves the narratives
// table present with the exact 13-column set, PRIMARY KEY (project,
// topic_prefix), and NONE of sync_id/writer_id/entity_type — the table is
// outside the sync plane entirely (decision #4751 decision 2).
func TestFreshDBHasNarrativesTable(t *testing.T) {
	st := openTempStore(t)

	var name string
	if err := st.DB().QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='narratives'`,
	).Scan(&name); err != nil {
		t.Fatalf("narratives table not found: %v", err)
	}

	cols := readNarrativesColumns(t, st.DB())

	wantCols := []string{
		"project", "topic_prefix", "body", "source_hash", "model",
		"template_version", "renderer_version", "source_count",
		"source_writers", "unverified_paths", "truncated", "stale",
		"generated_at",
	}
	if len(cols) != len(wantCols) {
		gotNames := make([]string, len(cols))
		for i, c := range cols {
			gotNames[i] = c.name
		}
		t.Fatalf("narratives has %d columns %v, want exactly %d %v", len(cols), gotNames, len(wantCols), wantCols)
	}
	gotSet := make(map[string]bool, len(cols))
	for _, c := range cols {
		gotSet[c.name] = true
	}
	for _, want := range wantCols {
		if !gotSet[want] {
			t.Errorf("narratives is missing expected column %q", want)
		}
	}

	for _, forbidden := range []string{"sync_id", "writer_id", "entity_type"} {
		if gotSet[forbidden] {
			t.Errorf("narratives has column %q — narratives are outside the sync plane entirely (decision #4751 decision 2) and must never carry it", forbidden)
		}
	}

	// PRIMARY KEY (project, topic_prefix): both columns carry pk > 0, and no
	// other column does.
	pkCols := map[string]int{}
	for _, c := range cols {
		if c.pk > 0 {
			pkCols[c.name] = c.pk
		}
	}
	if len(pkCols) != 2 || pkCols["project"] == 0 || pkCols["topic_prefix"] == 0 {
		t.Errorf("narratives PRIMARY KEY columns = %v, want exactly {project, topic_prefix}", pkCols)
	}
}

// TestMigrateV12ToV13 verifies that a DB wound back to user_version=12 (with
// the narratives table dropped, so the migration must actually CREATE it)
// advances to v13 with the table present after runMigrations.
func TestMigrateV12ToV13(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v12.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := ApplySchema(db); err != nil {
		t.Fatalf("ApplySchema: %v", err)
	}
	for _, stmt := range []string{
		`PRAGMA user_version = 12`,
		`DROP TABLE IF EXISTS narratives`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations from v12: %v", err)
	}

	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if ver != currentSchemaVersion {
		t.Errorf("user_version = %d; want %d (currentSchemaVersion)", ver, currentSchemaVersion)
	}

	var tblName string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='narratives'`,
	).Scan(&tblName); err != nil {
		t.Errorf("narratives table not found after migration: %v", err)
	}
}

// TestMigrateV12ToV13IsIdempotent verifies running runMigrations twice from
// v12 does not error and does not create a duplicate narratives table entry.
func TestMigrateV12ToV13IsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v12-idem.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := ApplySchema(db); err != nil {
		t.Fatalf("ApplySchema: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 12`); err != nil {
		t.Fatalf("set user_version=12: %v", err)
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("first runMigrations: %v", err)
	}
	if err := runMigrations(db); err != nil {
		t.Fatalf("second runMigrations (idempotent) must not error: %v", err)
	}

	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='narratives'`,
	).Scan(&count); err != nil {
		t.Fatalf("count narratives table entries: %v", err)
	}
	if count != 1 {
		t.Errorf("sqlite_master has %d 'narratives' table entries, want exactly 1 (no duplicate)", count)
	}
}

func readSQLiteMasterSQL(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var sqlText string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE name = ?`, name,
	).Scan(&sqlText); err != nil {
		t.Fatalf("read sqlite_master.sql for %q: %v", name, err)
	}
	return sqlText
}

// TestMigrationLeavesMemoriesUntouched verifies the v12->v13 migration is
// PURELY additive: memories' row count, its entity_type CHECK clause text,
// and the full FTS trigger set are all byte-identical before and after.
// Widening the CHECK would require a table rebuild and is explicitly NOT in
// scope for this migration.
func TestMigrationLeavesMemoriesUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v12-untouched.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := ApplySchema(db); err != nil {
		t.Fatalf("ApplySchema: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 12`); err != nil {
		t.Fatalf("set user_version=12: %v", err)
	}

	if _, err := db.Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES ('v12-mem-1', 'sess1', 'memory', 'manual', 'Pre-v13', 'pre-v13 content', 'p', 'project', 'w')
	`); err != nil {
		t.Fatalf("seed memories row: %v", err)
	}

	beforeMemoriesSQL := readSQLiteMasterSQL(t, db, "memories")
	beforeTriggers := map[string]string{
		"mem_fts_insert": readSQLiteMasterSQL(t, db, "mem_fts_insert"),
		"mem_fts_delete": readSQLiteMasterSQL(t, db, "mem_fts_delete"),
		"mem_fts_update": readSQLiteMasterSQL(t, db, "mem_fts_update"),
	}
	var beforeCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&beforeCount); err != nil {
		t.Fatalf("count memories before migration: %v", err)
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations from v12: %v", err)
	}

	afterMemoriesSQL := readSQLiteMasterSQL(t, db, "memories")
	if beforeMemoriesSQL != afterMemoriesSQL {
		t.Errorf("memories table DDL changed by the v12->v13 migration:\nbefore: %s\nafter:  %s", beforeMemoriesSQL, afterMemoriesSQL)
	}
	if !strings.Contains(afterMemoriesSQL, "CHECK(entity_type IN ('memory','change','spec','task','standard','plan'))") {
		t.Error("memories entity_type CHECK clause is missing or altered after migration")
	}

	for name, before := range beforeTriggers {
		after := readSQLiteMasterSQL(t, db, name)
		if before != after {
			t.Errorf("trigger %q changed by the v12->v13 migration:\nbefore: %s\nafter:  %s", name, before, after)
		}
	}

	var afterCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&afterCount); err != nil {
		t.Fatalf("count memories after migration: %v", err)
	}
	if afterCount != beforeCount {
		t.Errorf("memories row count changed by the v12->v13 migration: before=%d after=%d", beforeCount, afterCount)
	}
}

// TestNarrativeProseIsNotFTSIndexed verifies narrative prose can never
// surface in mem_search: the FTS triggers are all ON memories, so this
// sibling table is invisible to them.
func TestNarrativeProseIsNotFTSIndexed(t *testing.T) {
	st := openTempStore(t)

	const distinctiveToken = "zzyzxNarrativeOnlyToken12345"
	if err := st.UpsertNarrative(NarrativeRow{
		Project:         "proj",
		TopicPrefix:     "topic",
		Body:            "This narrative mentions " + distinctiveToken + " exactly once.",
		SourceHash:      "hash1",
		Model:           "fake-model",
		TemplateVersion: "pt-1",
		RendererVersion: "rv-1",
		SourceCount:     3,
		GeneratedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("UpsertNarrative: %v", err)
	}

	var count int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM memories_fts WHERE memories_fts MATCH ?`, distinctiveToken,
	).Scan(&count); err != nil {
		t.Fatalf("memories_fts MATCH query: %v", err)
	}
	if count != 0 {
		t.Errorf("memories_fts MATCH %q returned %d rows, want 0 — narrative prose must never surface in mem_search", distinctiveToken, count)
	}
}

// TestV13DBIsReadableUnderPhaseABinarySemantics pins the git-revert rollback
// property: runMigrations is a chain of `if ver < N` blocks with NO
// forward-version guard, so a binary compiled against an OLDER
// currentSchemaVersion that opens a NEWER database skips every migration
// block, finds ApplySchema's DDL all IF NOT EXISTS, and returns nil rather
// than rejecting the database.
//
// GUARD: it asserts the ABSENCE of a clause that was never there. Mutation
// proof: temporarily add `if ver > currentSchemaVersion { return err }`
// inside runMigrations and confirm this test fails.
func TestV13DBIsReadableUnderPhaseABinarySemantics(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "schema.go", nil, 0)
	if err != nil {
		t.Fatalf("parse schema.go: %v", err)
	}

	var fn *ast.FuncDecl
	ast.Inspect(f, func(n ast.Node) bool {
		if fd, ok := n.(*ast.FuncDecl); ok && fd.Name.Name == "runMigrations" {
			fn = fd
			return false
		}
		return true
	})
	if fn == nil {
		t.Fatal("runMigrations function not found in schema.go")
	}

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		be, ok := n.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		if be.Op != token.GTR && be.Op != token.GEQ {
			return true
		}
		forward := (identNamed(be.X, "ver") && identNamed(be.Y, "currentSchemaVersion")) ||
			(identNamed(be.X, "currentSchemaVersion") && identNamed(be.Y, "ver"))
		if forward {
			t.Errorf("runMigrations contains a forward-version comparison (ver %s currentSchemaVersion) — "+
				"an older binary opening a newer database must silently skip every `if ver < N` "+
				"block and return nil, not reject the database. This is the git-revert rollback "+
				"property; do NOT add a forward-version guard.", be.Op)
		}
		return true
	})

	// Behavioural half: running runMigrations on a DB already AT the current
	// schema version is a true no-op — the same property an older binary
	// needs when it finds nothing pending: it does not reject the DB.
	st := openTempStore(t)
	var before int
	if err := st.DB().QueryRow(`PRAGMA user_version`).Scan(&before); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if err := runMigrations(st.DB()); err != nil {
		t.Errorf("runMigrations on a DB already at the current version: %v", err)
	}
	var after int
	if err := st.DB().QueryRow(`PRAGMA user_version`).Scan(&after); err != nil {
		t.Fatalf("user_version after re-run: %v", err)
	}
	if after != before {
		t.Errorf("user_version changed from %d to %d on a no-op re-run", before, after)
	}
}

func identNamed(e ast.Expr, name string) bool {
	ident, ok := e.(*ast.Ident)
	return ok && ident.Name == name
}
