package localstore

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// openTempStoreAtVersion opens a fresh store at dbPath and returns it.
// Used by migration tests that need to assert on a specific user_version.
func openStoreAt(t *testing.T, dbPath string) *Store {
	t.Helper()
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", dbPath, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// ── CreateSession / GetSession round-trip ─────────────────────────────────────

func TestCreateSession_GetSession_RoundTrip(t *testing.T) {
	s := openTempStore(t)

	if err := s.CreateSession("sess-1", "engram", "/home/user/engram"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := s.GetSession("sess-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	if got.ID != "sess-1" {
		t.Errorf("ID: got %q, want %q", got.ID, "sess-1")
	}
	if got.Project != "engram" {
		t.Errorf("Project: got %q, want %q", got.Project, "engram")
	}
	if got.Directory != "/home/user/engram" {
		t.Errorf("Directory: got %q, want %q", got.Directory, "/home/user/engram")
	}
	if got.StartedAt.IsZero() {
		t.Error("StartedAt should not be zero")
	}
	if got.EndedAt != nil {
		t.Errorf("EndedAt should be nil on a new session, got %v", got.EndedAt)
	}
	if got.Summary != nil {
		t.Errorf("Summary should be nil on a new session, got %q", *got.Summary)
	}
}

// ── GetSession not found ───────────────────────────────────────────────────────

func TestGetSession_NotFound(t *testing.T) {
	s := openTempStore(t)

	_, err := s.GetSession("does-not-exist")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("GetSession missing: got %v, want ErrSessionNotFound", err)
	}
}

// ── EndSession sets ended_at and summary ──────────────────────────────────────

func TestEndSession_SetsEndedAtAndSummary(t *testing.T) {
	s := openTempStore(t)

	if err := s.CreateSession("sess-end", "myproject", "/src"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	before := time.Now().UTC().Truncate(time.Second)

	if err := s.EndSession("sess-end", "all done"); err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	after := time.Now().UTC().Add(time.Second)

	got, err := s.GetSession("sess-end")
	if err != nil {
		t.Fatalf("GetSession after EndSession: %v", err)
	}
	if got.EndedAt == nil {
		t.Fatal("EndedAt is nil after EndSession")
	}
	if got.EndedAt.Before(before) || got.EndedAt.After(after) {
		t.Errorf("EndedAt %v not in expected range [%v, %v]", got.EndedAt, before, after)
	}
	if got.Summary == nil {
		t.Fatal("Summary is nil after EndSession")
	}
	if *got.Summary != "all done" {
		t.Errorf("Summary: got %q, want %q", *got.Summary, "all done")
	}
}

// ── EndSession no-op on unknown id ────────────────────────────────────────────

func TestEndSession_NoOpOnUnknownID(t *testing.T) {
	s := openTempStore(t)
	// Should not error even when the id does not exist.
	if err := s.EndSession("nonexistent", "summary"); err != nil {
		t.Errorf("EndSession on unknown id should be a no-op, got: %v", err)
	}
}

// ── RecentSessions ordering and limit ─────────────────────────────────────────

func TestRecentSessions_OrderingAndLimit(t *testing.T) {
	s := openTempStore(t)

	// Insert three sessions.
	for _, id := range []string{"s1", "s2", "s3"} {
		if err := s.CreateSession(id, "proj", "/dir"); err != nil {
			t.Fatalf("CreateSession %q: %v", id, err)
		}
	}

	// All three — no limit restriction.
	all, err := s.RecentSessions("proj", 10)
	if err != nil {
		t.Fatalf("RecentSessions: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("RecentSessions: got %d results, want 3", len(all))
	}

	// Limit to 2.
	limited, err := s.RecentSessions("proj", 2)
	if err != nil {
		t.Fatalf("RecentSessions limit=2: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("RecentSessions limit=2: got %d results, want 2", len(limited))
	}
}

// ── RecentSessions default limit ──────────────────────────────────────────────

func TestRecentSessions_DefaultLimit(t *testing.T) {
	s := openTempStore(t)

	for i := range 7 {
		id := "session-" + string(rune('a'+i))
		if err := s.CreateSession(id, "p", "/d"); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}

	// limit <= 0 should default to 5.
	res, err := s.RecentSessions("p", 0)
	if err != nil {
		t.Fatalf("RecentSessions limit=0: %v", err)
	}
	if len(res) != 5 {
		t.Errorf("RecentSessions default limit: got %d, want 5", len(res))
	}
}

// ── RecentSessions project filter ─────────────────────────────────────────────

func TestRecentSessions_ProjectFilter(t *testing.T) {
	s := openTempStore(t)

	if err := s.CreateSession("alpha", "proj-a", "/a"); err != nil {
		t.Fatalf("CreateSession alpha: %v", err)
	}
	if err := s.CreateSession("beta", "proj-b", "/b"); err != nil {
		t.Fatalf("CreateSession beta: %v", err)
	}

	res, err := s.RecentSessions("proj-a", 10)
	if err != nil {
		t.Fatalf("RecentSessions: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 result for proj-a, got %d", len(res))
	}
	if res[0].ID != "alpha" {
		t.Errorf("expected session id %q, got %q", "alpha", res[0].ID)
	}
}

// ── RecentSessions empty project returns all ──────────────────────────────────

func TestRecentSessions_EmptyProjectReturnsAll(t *testing.T) {
	s := openTempStore(t)

	for _, pair := range [][2]string{{"s1", "proj-a"}, {"s2", "proj-b"}, {"s3", "proj-c"}} {
		if err := s.CreateSession(pair[0], pair[1], "/d"); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}

	res, err := s.RecentSessions("", 10)
	if err != nil {
		t.Fatalf("RecentSessions empty project: %v", err)
	}
	if len(res) != 3 {
		t.Errorf("expected 3 results across all projects, got %d", len(res))
	}
}

// ── CreateSession upsert does not overwrite populated project ─────────────────

func TestCreateSession_UpsertDoesNotOverwriteProject(t *testing.T) {
	s := openTempStore(t)

	if err := s.CreateSession("upsert-id", "original", "/dir"); err != nil {
		t.Fatalf("first CreateSession: %v", err)
	}
	// Second call with a different project must NOT overwrite "original".
	if err := s.CreateSession("upsert-id", "overwrite", "/newdir"); err != nil {
		t.Fatalf("second CreateSession: %v", err)
	}

	got, err := s.GetSession("upsert-id")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Project != "original" {
		t.Errorf("Project was overwritten: got %q, want %q", got.Project, "original")
	}
	if got.Directory != "/dir" {
		t.Errorf("Directory was overwritten: got %q, want %q", got.Directory, "/dir")
	}
}

// ── normalizeProject lowercases and collapses separators ──────────────────────

func TestNormalizeProject(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"MyProject", "myproject"},
		{"  spaces  ", "spaces"},
		{"a--b", "a-b"},
		{"a__b", "a_b"},
		{"", ""},
	}
	for _, c := range cases {
		got := normalizeProject(c.input)
		if got != c.want {
			t.Errorf("normalizeProject(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// ── CreateSession normalizes project name ─────────────────────────────────────

func TestCreateSession_NormalizesProject(t *testing.T) {
	s := openTempStore(t)

	if err := s.CreateSession("n1", "MyProject", "/dir"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := s.GetSession("n1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Project != "myproject" {
		t.Errorf("Project not normalized: got %q, want %q", got.Project, "myproject")
	}
}

// ── v7 migration: applies on a fresh store (user_version already 7) ───────────

func TestMigrateV6ToV7_FreshStore(t *testing.T) {
	s := openTempStore(t)

	// A fresh store should be at version 7.
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if ver != currentSchemaVersion {
		t.Errorf("user_version: got %d, want %d", ver, currentSchemaVersion)
	}

	// sessions table must exist and be usable.
	if err := s.CreateSession("fresh-sess", "p", "/d"); err != nil {
		t.Fatalf("CreateSession on fresh store: %v", err)
	}
}

// ── v7 migration: applies on a v6 store (simulates upgrade) ──────────────────

func TestMigrateV6ToV7_ExistingV6Store(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "v6.db")

	// Step 1: Open to create a fresh v7 store, then manually downgrade to v6 by
	// dropping the sessions table and resetting user_version. This simulates a
	// store that was created before v7 existed.
	{
		s, err := Open(dbPath)
		if err != nil {
			t.Fatalf("Open (initial): %v", err)
		}
		if _, err := s.db.Exec(`DROP TABLE IF EXISTS sessions`); err != nil {
			_ = s.Close()
			t.Fatalf("DROP TABLE sessions: %v", err)
		}
		if _, err := s.db.Exec(`PRAGMA user_version = 6`); err != nil {
			_ = s.Close()
			t.Fatalf("PRAGMA user_version = 6: %v", err)
		}
		_ = s.Close()
	}

	// Step 2: Re-open — Open calls runMigrations, which must apply v6→v7.
	s := openStoreAt(t, dbPath)

	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if ver != 7 {
		t.Errorf("after re-open, user_version = %d, want 7", ver)
	}

	// sessions table must exist and be usable after migration.
	if err := s.CreateSession("migrated-sess", "p", "/d"); err != nil {
		t.Fatalf("CreateSession after migration: %v", err)
	}
	got, err := s.GetSession("migrated-sess")
	if err != nil {
		t.Fatalf("GetSession after migration: %v", err)
	}
	if got.ID != "migrated-sess" {
		t.Errorf("GetSession.ID = %q, want %q", got.ID, "migrated-sess")
	}
}
