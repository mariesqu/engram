package localstore

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// ── shared helpers for the v13/v14 existing-DB migration tests ───────────────

// openRawSchemaDB opens a bare *sql.DB at a temp path with the CURRENT schema
// applied, and returns it alongside the path. It is the starting point for an
// "existing DB" migration test: apply everything, then wind the DB back to the
// version under test by undoing the one change that version introduced.
func openRawSchemaDB(t *testing.T, name string) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ApplySchema(db); err != nil {
		t.Fatalf("ApplySchema: %v", err)
	}
	return db, path
}

// insertRawMemory writes one memories row with explicit created_at/review_after/
// deleted_at, bypassing the write path entirely — a migration test needs rows
// that look the way the OLD binary left them, not the way the current one writes.
// Pass "" for a NULL review_after or a live (non-deleted) row.
func insertRawMemory(t *testing.T, db *sql.DB, syncID, typ, createdAt, reviewAfter, deletedAt string) {
	t.Helper()
	var ra, del any
	if reviewAfter != "" {
		ra = reviewAfter
	}
	if deletedAt != "" {
		del = deletedAt
	}
	if _, err := db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id,
		   created_at, updated_at, review_after, deleted_at)
		VALUES (?, 'sess1', 'memory', ?, 'alpha title', 'alpha beta content', 'mig', 'project', 'w1',
		        ?, ?, ?, ?)`,
		syncID, typ, createdAt, createdAt, ra, del,
	); err != nil {
		t.Fatalf("insert %q: %v", syncID, err)
	}
}

// memoryColumnExists reports whether memories carries the named column.
func memoryColumnExists(t *testing.T, db *sql.DB, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(memories)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(memories): %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			dflt       sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &primaryKey); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	return false
}

// schemaVersion reads PRAGMA user_version.
func schemaVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	return ver
}

// rawReviewAfter reads review_after for one row, ok=false meaning SQL NULL.
func rawReviewAfter(t *testing.T, db *sql.DB, syncID string) (string, bool) {
	t.Helper()
	var raw sql.NullString
	if err := db.QueryRow(
		`SELECT review_after FROM memories WHERE sync_id = ?`, syncID,
	).Scan(&raw); err != nil {
		t.Fatalf("read review_after of %q: %v", syncID, err)
	}
	if !raw.Valid || raw.String == "" {
		return "", false
	}
	return raw.String, true
}

// assertSearchStillWorks reopens the migrated file through Open (the real upgrade
// path: ApplySchema, then runMigrations) and runs a search through it. It is the
// end-to-end half of every migration assertion here — a column can be present and
// the rows intact while the FTS index or an ORDER BY expression that references
// the column is broken.
func assertSearchStillWorks(t *testing.T, path string, wantRows int) {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q) after migration: %v", path, err)
	}
	defer s.Close() //nolint:errcheck // test cleanup

	got, _, err := s.SearchMemoriesFiltered("alpha", "mig", 10, SearchFilter{})
	if err != nil {
		t.Fatalf("SearchMemoriesFiltered after migration: %v", err)
	}
	if len(got) != wantRows {
		t.Errorf("search after migration returned %d rows, want %d", len(got), wantRows)
	}
}

// ── v12 → v13 ────────────────────────────────────────────────────────────────

// TestMigration_V12ToV13_ExistingDB is the existing-DB upgrade proof for the
// pinned column. The fixture drops the column from a fully-built schema and winds
// user_version back to 12, so migrateV12ToV13 has to actually run the ALTER
// instead of no-opping against a column ApplySchema already created — the failure
// mode a fresh-DB-only test cannot see.
//
// It asserts the three things an upgrade owes the user: the column arrives,
// pre-existing rows survive it (unpinned, which is the only correct default for a
// memory saved before pinning existed), and search still answers — the FTS ORDER
// BY references m.pinned, so a half-applied migration breaks every query.
func TestMigration_V12ToV13_ExistingDB(t *testing.T) {
	db, path := openRawSchemaDB(t, "v12.db")

	insertRawMemory(t, db, "v12-mem-1", "manual", "2023-01-15 10:00:00", "", "")
	insertRawMemory(t, db, "v12-mem-2", "decision", "2023-02-15 10:00:00", "", "")

	for _, stmt := range []string{
		`ALTER TABLE memories DROP COLUMN pinned`,
		`PRAGMA user_version = 12`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("wind back to v12 (%s): %v", stmt, err)
		}
	}
	if memoryColumnExists(t, db, "pinned") {
		t.Fatal("pre-condition: pinned column still present after DROP COLUMN")
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations from v12: %v", err)
	}

	if ver := schemaVersion(t, db); ver != currentSchemaVersion {
		t.Errorf("user_version = %d after runMigrations, want %d", ver, currentSchemaVersion)
	}
	if !memoryColumnExists(t, db, "pinned") {
		t.Fatal("pinned column missing after migrateV12ToV13")
	}

	rows, err := db.Query(`SELECT sync_id, pinned FROM memories ORDER BY sync_id`)
	if err != nil {
		t.Fatalf("read migrated rows: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var syncID string
		var pinned int
		if err := rows.Scan(&syncID, &pinned); err != nil {
			t.Fatalf("scan migrated row: %v", err)
		}
		if pinned != 0 {
			t.Errorf("row %q came out of the migration pinned=%d, want 0", syncID, pinned)
		}
		seen++
	}
	if seen != 2 {
		t.Errorf("migration preserved %d rows, want 2", seen)
	}

	// Re-running migrations on the migrated DB must be a no-op.
	if err := runMigrations(db); err != nil {
		t.Errorf("re-running migrations on a current-version DB: %v", err)
	}
	if ver := schemaVersion(t, db); ver != currentSchemaVersion {
		t.Errorf("user_version = %d after no-op re-run, want %d", ver, currentSchemaVersion)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close raw DB: %v", err)
	}
	assertSearchStillWorks(t, path, 2)
}

// ── v13 → v14 ────────────────────────────────────────────────────────────────

// TestMigration_V13ToV14_ExistingDB is the existing-DB upgrade proof for the
// session index and the review_after backfill.
//
// The backfill assertions are the interesting half. Dating each row's window from
// its OWN created_at (not from the migration instant) is what makes a two-year-old
// decision surface as needs_review the moment you upgrade, instead of being handed
// a fresh six months by an unrelated binary update. The three negative cases are
// equally load-bearing: a row that already carries a review_after is per-node state
// the migration must not overwrite, a soft-deleted row is not a row, and a type
// with no decay entry keeps NULL — which in this store means "fall back to
// updated_at + window", not "never due".
func TestMigration_V13ToV14_ExistingDB(t *testing.T) {
	db, path := openRawSchemaDB(t, "v13.db")

	const (
		oldCreated  = "2023-01-15 10:00:00"
		prevMarked  = "2030-01-15 10:00:00"
		deletedWhen = "2023-06-15 10:00:00"
	)

	insertRawMemory(t, db, "v13-decision", "decision", oldCreated, "", "")
	insertRawMemory(t, db, "v13-policy", "policy", oldCreated, "", "")
	insertRawMemory(t, db, "v13-preference", "preference", oldCreated, "", "")
	insertRawMemory(t, db, "v13-bugfix", "bugfix", oldCreated, "", "")
	insertRawMemory(t, db, "v13-already-marked", "decision", oldCreated, prevMarked, "")
	insertRawMemory(t, db, "v13-deleted", "decision", oldCreated, "", deletedWhen)

	for _, stmt := range []string{
		`DROP INDEX IF EXISTS idx_mem_session`,
		`PRAGMA user_version = 13`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("wind back to v13 (%s): %v", stmt, err)
		}
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations from v13: %v", err)
	}

	if ver := schemaVersion(t, db); ver != currentSchemaVersion {
		t.Errorf("user_version = %d after runMigrations, want %d", ver, currentSchemaVersion)
	}

	var idxName string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_mem_session'`,
	).Scan(&idxName); err != nil {
		t.Errorf("idx_mem_session not found after migrateV13ToV14: %v", err)
	}

	created := parseTime(oldCreated)
	for _, tc := range []struct {
		syncID string
		months int
	}{
		{"v13-decision", 6},
		{"v13-policy", 12},
		{"v13-preference", 3},
	} {
		got, ok := rawReviewAfter(t, db, tc.syncID)
		if !ok {
			t.Errorf("%s: review_after still NULL after the backfill", tc.syncID)
			continue
		}
		want := created.AddDate(0, tc.months, 0).Format(sqliteTimeLayout)
		if got != want {
			t.Errorf("%s: review_after = %q, want %q (created_at +%d months)",
				tc.syncID, got, want, tc.months)
		}
	}

	if got, ok := rawReviewAfter(t, db, "v13-bugfix"); ok {
		t.Errorf("v13-bugfix: review_after = %q, want NULL — bugfix has no decay entry", got)
	}
	if got, ok := rawReviewAfter(t, db, "v13-already-marked"); !ok || got != prevMarked {
		t.Errorf("v13-already-marked: review_after = %q (set=%v), want the pre-existing %q untouched",
			got, ok, prevMarked)
	}
	if got, ok := rawReviewAfter(t, db, "v13-deleted"); ok {
		t.Errorf("v13-deleted: review_after = %q, want NULL — a soft-deleted row is not backfilled", got)
	}

	// Re-running migrations on the migrated DB must be a no-op — including the
	// backfill, which is guarded by review_after IS NULL.
	if err := runMigrations(db); err != nil {
		t.Errorf("re-running migrations on a current-version DB: %v", err)
	}
	if got, _ := rawReviewAfter(t, db, "v13-decision"); got != created.AddDate(0, 6, 0).Format(sqliteTimeLayout) {
		t.Errorf("v13-decision: review_after moved to %q on a no-op re-run", got)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close raw DB: %v", err)
	}
	// Five live rows — v13-deleted is soft-deleted and must not surface.
	assertSearchStillWorks(t, path, 5)
}

// TestMigration_V13ToV14_FreshDB pins that a store created today already carries
// the index, so the migration is the ONLY thing old DBs need and fresh ones get
// it from ApplySchema.
func TestMigration_V13ToV14_FreshDB(t *testing.T) {
	s := openTempStore(t)

	var name string
	if err := s.DB().QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_mem_session'`,
	).Scan(&name); err != nil {
		t.Fatalf("idx_mem_session missing on a fresh DB: %v", err)
	}
	if ver := schemaVersion(t, s.DB()); ver != currentSchemaVersion {
		t.Errorf("fresh DB user_version = %d, want %d", ver, currentSchemaVersion)
	}
}

// TestMigration_V13ToV14_BackfilledRowIsNeedsReview closes the loop on the
// backfill: the point of dating the window from created_at is not the column
// value, it is that ReviewStatus then reports the row as STALE. A backfill that
// wrote now+6mo would satisfy a column assertion and still leave a two-year-old
// decision reading as trustworthy.
func TestMigration_V13ToV14_BackfilledRowIsNeedsReview(t *testing.T) {
	db, path := openRawSchemaDB(t, "v13-status.db")

	old := time.Now().UTC().AddDate(-2, 0, 0).Format(sqliteTimeLayout)
	insertRawMemory(t, db, "v13-stale-decision", "decision", old, "", "")

	if _, err := db.Exec(`PRAGMA user_version = 13`); err != nil {
		t.Fatalf("wind back to v13: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw DB: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open after winding back: %v", err)
	}
	defer s.Close() //nolint:errcheck // test cleanup

	var id int64
	if err := s.DB().QueryRow(
		`SELECT id FROM memories WHERE sync_id = 'v13-stale-decision'`,
	).Scan(&id); err != nil {
		t.Fatalf("read backfilled row id: %v", err)
	}

	status, err := s.ReviewStatusForID(id)
	if err != nil {
		t.Fatalf("ReviewStatusForID: %v", err)
	}
	if status != ReviewStatusNeedsReview {
		t.Errorf("a two-year-old decision reads as %q after the backfill, want %q",
			status, ReviewStatusNeedsReview)
	}
}
