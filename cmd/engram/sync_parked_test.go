package main

// Tests for `engram sync parked|retry|discard` (FUP-004d). Unlike `sync now`
// (which talks to a running daemon over the control API — see
// config_sync_test.go), these subcommands open the local store directly
// (openSyncStore), so tests seed a real temp SQLite file and assert against
// both the printed output and the resulting store state.

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/localstore"
)

// seedParkedDB opens a fresh store at a temp path, writes one mutation, parks
// its outbox entry, closes the store, and returns the db path plus the
// resulting local_seq — ready for a CLI subcommand to open via --db.
func seedParkedDB(t *testing.T, syncID string) (dbPath string, localSeq int64) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "sync-parked.db")

	store, err := localstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	if _, err := store.LocalWrite(domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "title",
		Content:    "content",
		Project:    "cli-park-project",
		Scope:      "project",
		WriterID:   "writer",
	}); err != nil {
		t.Fatalf("LocalWrite: %v", err)
	}

	entries, err := store.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("DrainOutbox returned %d entries, want 1", len(entries))
	}
	localSeq = entries[0].LocalSeq

	if err := store.ParkMutation(localSeq, "rejected by constraint \"x\" (SQLSTATE 23514)"); err != nil {
		t.Fatalf("ParkMutation: %v", err)
	}
	return dbPath, localSeq
}

func TestSyncParkedCmd_ListsParkedEntries(t *testing.T) {
	dbPath, seq := seedParkedDB(t, "sync-cli-list")

	out := captureStdout(t, func() {
		if err := runSyncCmd([]string{"parked", "--db", dbPath}); err != nil {
			t.Fatalf("sync parked: %v", err)
		}
	})

	for _, want := range []string{
		"local_seq=" + strconv.FormatInt(seq, 10),
		"project=\"cli-park-project\"",
		"entity=memory",
		"attempts=1",
		"rejected by constraint",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}
}

func TestSyncParkedCmd_NoneParked(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sync-empty.db")
	store, err := localstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Close()

	out := captureStdout(t, func() {
		if err := runSyncCmd([]string{"parked", "--db", dbPath}); err != nil {
			t.Fatalf("sync parked: %v", err)
		}
	})
	if !strings.Contains(out, "no parked mutations") {
		t.Errorf("output = %q, want the no-parked-mutations message", out)
	}
}

func TestSyncParkedCmd_MissingDB(t *testing.T) {
	t.Setenv("ENGRAM_DB", "")
	if err := runSyncCmd([]string{"parked"}); err == nil {
		t.Error("sync parked without --db should return an error")
	}
}

func TestSyncRetryCmd_SingleSeq(t *testing.T) {
	dbPath, seq := seedParkedDB(t, "sync-cli-retry")

	out := captureStdout(t, func() {
		if err := runSyncCmd([]string{"retry", strconv.FormatInt(seq, 10), "--db", dbPath}); err != nil {
			t.Fatalf("sync retry: %v", err)
		}
	})
	if !strings.Contains(out, "retried local_seq="+strconv.FormatInt(seq, 10)) {
		t.Errorf("output = %q, want confirmation of the retried seq", out)
	}

	store, err := localstore.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store.Close()

	parked, err := store.ListParked()
	if err != nil {
		t.Fatalf("ListParked: %v", err)
	}
	if len(parked) != 0 {
		t.Errorf("ListParked after retry = %+v, want empty", parked)
	}
	entries, err := store.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if len(entries) != 1 || entries[0].LocalSeq != seq {
		t.Errorf("DrainOutbox after retry = %+v, want exactly [local_seq=%d]", entries, seq)
	}
}

func TestSyncRetryCmd_All(t *testing.T) {
	dbPath, seq1 := seedParkedDB(t, "sync-cli-retry-all-1")

	store, err := localstore.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := store.LocalWrite(domain.Mutation{
		Op: domain.OpUpsert, SyncID: "sync-cli-retry-all-2", SessionID: "sess",
		EntityType: domain.EntityMemory, Type: "manual", Title: "t2", Content: "c2",
		Project: "cli-park-project", Scope: "project", WriterID: "writer",
	}); err != nil {
		t.Fatalf("LocalWrite 2: %v", err)
	}
	entries, err := store.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	var seq2 int64
	for _, e := range entries {
		if e.Mutation.SyncID == "sync-cli-retry-all-2" {
			seq2 = e.LocalSeq
		}
	}
	if seq2 == 0 {
		t.Fatal("second entry not found in outbox")
	}
	if err := store.ParkMutation(seq2, "also rejected"); err != nil {
		t.Fatalf("ParkMutation 2: %v", err)
	}
	store.Close()

	out := captureStdout(t, func() {
		if err := runSyncCmd([]string{"retry", "all", "--db", dbPath}); err != nil {
			t.Fatalf("sync retry all: %v", err)
		}
	})
	if !strings.Contains(out, "retried 2 mutation(s)") {
		t.Errorf("output = %q, want it to report 2 retried", out)
	}

	store2, err := localstore.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()
	parked, err := store2.ListParked()
	if err != nil {
		t.Fatalf("ListParked: %v", err)
	}
	if len(parked) != 0 {
		t.Errorf("ListParked after retry-all = %+v, want empty", parked)
	}
	_ = seq1
}

func TestSyncRetryCmd_NotParkedErrors(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sync-retry-notparked.db")
	store, err := localstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Close()

	if err := runSyncCmd([]string{"retry", "999", "--db", dbPath}); err == nil {
		t.Error("sync retry on a non-existent local_seq should return an error")
	}
}

func TestSyncRetryCmd_RequiresArg(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sync-retry-noarg.db")
	if err := runSyncCmd([]string{"retry", "--db", dbPath}); err == nil {
		t.Error("sync retry with no argument should return an error")
	}
}

func TestSyncDiscardCmd_DiscardsAndPrints(t *testing.T) {
	dbPath, seq := seedParkedDB(t, "sync-cli-discard")

	out := captureStdout(t, func() {
		if err := runSyncCmd([]string{"discard", strconv.FormatInt(seq, 10), "--db", dbPath}); err != nil {
			t.Fatalf("sync discard: %v", err)
		}
	})
	for _, want := range []string{"discarded local_seq=" + strconv.FormatInt(seq, 10), "cli-park-project"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}

	store, err := localstore.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store.Close()
	parked, err := store.ListParked()
	if err != nil {
		t.Fatalf("ListParked: %v", err)
	}
	if len(parked) != 0 {
		t.Errorf("ListParked after discard = %+v, want empty", parked)
	}
	entries, err := store.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("DrainOutbox after discard = %+v, want empty — a discarded entry must never be pushed", entries)
	}
}

func TestSyncDiscardCmd_RequiresArg(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sync-discard-noarg.db")
	if err := runSyncCmd([]string{"discard", "--db", dbPath}); err == nil {
		t.Error("sync discard with no argument should return an error")
	}
}

func TestSyncDiscardCmd_NotParkedErrors(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sync-discard-notparked.db")
	store, err := localstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Close()

	err = runSyncCmd([]string{"discard", "999", "--db", dbPath})
	if err == nil {
		t.Error("sync discard on a non-existent local_seq should return an error")
	}
	if !errors.Is(err, localstore.ErrMutationNotParked) {
		t.Errorf("error = %v, want it to wrap ErrMutationNotParked", err)
	}
}
