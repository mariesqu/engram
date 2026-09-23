package localstore

// Tests for FUP-005c's local half (created_at_backfill.go): applying a page
// of server-supplied original creation times, and tracking per-project
// completion.

import (
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// TestBackfillCreatedAt_UsesTheOlderOfTheTwo is the core contract: a server
// value OLDER than the local row's created_at replaces it; a server value
// that is NOT older (equal or newer) leaves the local value untouched.
func TestBackfillCreatedAt_UsesTheOlderOfTheTwo(t *testing.T) {
	s := openTempStore(t)

	local := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.ApplyPulled(pulledMut("sync-bf-older", "manual", "content", 1, local, local)); err != nil {
		t.Fatalf("ApplyPulled (older case): %v", err)
	}
	if err := s.ApplyPulled(pulledMut("sync-bf-newer", "manual", "content", 1, local, local)); err != nil {
		t.Fatalf("ApplyPulled (newer case): %v", err)
	}

	serverOlder := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	serverNewer := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	n, err := s.BackfillCreatedAt([]domain.CreatedAtEntry{
		{SyncID: "sync-bf-older", CreatedAt: serverOlder},
		{SyncID: "sync-bf-newer", CreatedAt: serverNewer},
	})
	if err != nil {
		t.Fatalf("BackfillCreatedAt: %v", err)
	}
	if n != 1 {
		t.Errorf("BackfillCreatedAt updated %d row(s), want 1 (only the older-server-value case)", n)
	}

	var olderCreatedAt, newerCreatedAt string
	if err := s.db.QueryRow(`SELECT created_at FROM memories WHERE sync_id = 'sync-bf-older'`).Scan(&olderCreatedAt); err != nil {
		t.Fatalf("query sync-bf-older: %v", err)
	}
	if err := s.db.QueryRow(`SELECT created_at FROM memories WHERE sync_id = 'sync-bf-newer'`).Scan(&newerCreatedAt); err != nil {
		t.Fatalf("query sync-bf-newer: %v", err)
	}
	if got := parseTime(olderCreatedAt); !got.Equal(serverOlder) {
		t.Errorf("sync-bf-older created_at = %v, want the OLDER server value %v", got, serverOlder)
	}
	if got := parseTime(newerCreatedAt); !got.Equal(local) {
		t.Errorf("sync-bf-newer created_at = %v, want it UNCHANGED at %v (server value was newer)", got, local)
	}
}

// TestBackfillCreatedAt_SkipsUnknownSyncID: a sync_id this node has never
// pulled has no local row to correct — it must be silently skipped, not
// treated as an error.
func TestBackfillCreatedAt_SkipsUnknownSyncID(t *testing.T) {
	s := openTempStore(t)

	n, err := s.BackfillCreatedAt([]domain.CreatedAtEntry{
		{SyncID: "sync-bf-nowhere", CreatedAt: time.Now().UTC()},
	})
	if err != nil {
		t.Fatalf("BackfillCreatedAt: %v", err)
	}
	if n != 0 {
		t.Errorf("BackfillCreatedAt updated %d row(s), want 0 (no local row for this sync_id)", n)
	}
}

// TestBackfillCreatedAt_EmptyEntriesIsANoOp confirms the zero-entries path
// short-circuits without touching the database.
func TestBackfillCreatedAt_EmptyEntriesIsANoOp(t *testing.T) {
	s := openTempStore(t)

	n, err := s.BackfillCreatedAt(nil)
	if err != nil {
		t.Fatalf("BackfillCreatedAt(nil): %v", err)
	}
	if n != 0 {
		t.Errorf("BackfillCreatedAt(nil) = %d, want 0", n)
	}
}

// TestCreatedAtBackfillDone_TracksCompletionPerProject covers the
// once-per-project marker: absent by default, present (and true) only after
// MarkCreatedAtBackfillDone, and scoped to the named project alone.
func TestCreatedAtBackfillDone_TracksCompletionPerProject(t *testing.T) {
	s := openTempStore(t)

	done, err := s.CreatedAtBackfillDone("proj-a")
	if err != nil {
		t.Fatalf("CreatedAtBackfillDone (before): %v", err)
	}
	if done {
		t.Error("CreatedAtBackfillDone = true before any MarkCreatedAtBackfillDone call")
	}

	if err := s.MarkCreatedAtBackfillDone("proj-a"); err != nil {
		t.Fatalf("MarkCreatedAtBackfillDone: %v", err)
	}

	done, err = s.CreatedAtBackfillDone("proj-a")
	if err != nil {
		t.Fatalf("CreatedAtBackfillDone (after): %v", err)
	}
	if !done {
		t.Error("CreatedAtBackfillDone = false after MarkCreatedAtBackfillDone")
	}

	// A DIFFERENT project must be unaffected.
	otherDone, err := s.CreatedAtBackfillDone("proj-b")
	if err != nil {
		t.Fatalf("CreatedAtBackfillDone (other project): %v", err)
	}
	if otherDone {
		t.Error("CreatedAtBackfillDone(proj-b) = true, want false — marking proj-a must not leak to proj-b")
	}

	// A repeat call must be a harmless no-op (INSERT OR IGNORE), not an error.
	if err := s.MarkCreatedAtBackfillDone("proj-a"); err != nil {
		t.Errorf("MarkCreatedAtBackfillDone (repeat): %v", err)
	}
}
