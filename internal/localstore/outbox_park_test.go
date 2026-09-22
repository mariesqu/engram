package localstore

// Tests for FUP-004's outbox failure-tracking / parking primitives: a
// permanently-rejected (or undecodable) sync_mutations row must stop being
// resent by DrainOutbox while staying visible and recoverable — never silently
// dropped. See sync.go's "outbox failure tracking / parking" section.

import (
	"errors"
	"testing"
	"time"
)

// TestParkMutation_ExcludesFromDrainOutbox is the core contract: a parked
// entry must never come back from DrainOutbox, so the syncer stops resending
// a mutation central will never accept.
func TestParkMutation_ExcludesFromDrainOutbox(t *testing.T) {
	s := openTempStore(t)

	if _, err := s.LocalWrite(upsertMut("sync-park-1", "sdd/test/park1", "one", 1, baseT.Add(1*time.Second))); err != nil {
		t.Fatalf("LocalWrite: %v", err)
	}
	entries, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 pending entry, got %d", len(entries))
	}
	seq := entries[0].LocalSeq

	if err := s.ParkMutation(seq, "central rejected this mutation: 422"); err != nil {
		t.Fatalf("ParkMutation: %v", err)
	}

	after, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox after park: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("DrainOutbox returned %d entries after park, want 0", len(after))
	}
}

// TestParkMutation_AppearsInListParked covers the visibility half: a parked
// entry must be findable, with its attempts/last_error/project populated, for
// mem_doctor and `engram sync parked`.
func TestParkMutation_AppearsInListParked(t *testing.T) {
	s := openTempStore(t)

	if _, err := s.LocalWrite(upsertMut("sync-park-2", "sdd/test/park2", "two", 1, baseT.Add(1*time.Second))); err != nil {
		t.Fatalf("LocalWrite: %v", err)
	}
	entries, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	seq := entries[0].LocalSeq

	if err := s.ParkMutation(seq, "rejected by constraint \"x\" (SQLSTATE 23514)"); err != nil {
		t.Fatalf("ParkMutation: %v", err)
	}

	parked, err := s.ListParked()
	if err != nil {
		t.Fatalf("ListParked: %v", err)
	}
	if len(parked) != 1 {
		t.Fatalf("ListParked returned %d entries, want 1", len(parked))
	}
	p := parked[0]
	if p.LocalSeq != seq {
		t.Errorf("LocalSeq = %d, want %d", p.LocalSeq, seq)
	}
	if p.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", p.Attempts)
	}
	if p.LastError == "" {
		t.Error("LastError is empty, want the park reason")
	}
	if p.Project != "engram" {
		t.Errorf("Project = %q, want %q (decoded from the stored payload)", p.Project, "engram")
	}
	if p.ParkedAt.IsZero() {
		t.Error("ParkedAt is zero, want a timestamp")
	}
}

// TestUnparkMutation_RestoresToDrainOutbox is the `engram sync retry`
// primitive: un-parking must make DrainOutbox return the entry again and reset
// attempts to 0.
func TestUnparkMutation_RestoresToDrainOutbox(t *testing.T) {
	s := openTempStore(t)

	if _, err := s.LocalWrite(upsertMut("sync-park-3", "sdd/test/park3", "three", 1, baseT.Add(1*time.Second))); err != nil {
		t.Fatalf("LocalWrite: %v", err)
	}
	entries, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	seq := entries[0].LocalSeq

	if err := s.ParkMutation(seq, "temporary test rejection"); err != nil {
		t.Fatalf("ParkMutation: %v", err)
	}
	if err := s.UnparkMutation(seq); err != nil {
		t.Fatalf("UnparkMutation: %v", err)
	}

	after, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox after unpark: %v", err)
	}
	if len(after) != 1 || after[0].LocalSeq != seq {
		t.Fatalf("DrainOutbox after unpark = %+v, want exactly [local_seq=%d]", after, seq)
	}

	parked, err := s.ListParked()
	if err != nil {
		t.Fatalf("ListParked: %v", err)
	}
	if len(parked) != 0 {
		t.Errorf("ListParked returned %d entries after unpark, want 0", len(parked))
	}

	// attempts reset to 0 — confirmed via a fresh park, which increments from 0.
	if err := s.ParkMutation(seq, "re-park to check the reset"); err != nil {
		t.Fatalf("re-ParkMutation: %v", err)
	}
	reparked, err := s.ListParked()
	if err != nil {
		t.Fatalf("ListParked after re-park: %v", err)
	}
	if len(reparked) != 1 || reparked[0].Attempts != 1 {
		t.Errorf("ListParked after re-park = %+v, want exactly one entry with Attempts=1 (reset by unpark)", reparked)
	}
}

// TestUnparkMutation_ErrorsWhenNotParked pins the guard: un-parking a
// non-existent or never-parked local_seq must fail loudly (ErrMutationNotParked),
// not silently no-op — a CLI that reported success for a typo'd --seq would be
// worse than one that errors.
func TestUnparkMutation_ErrorsWhenNotParked(t *testing.T) {
	s := openTempStore(t)

	if _, err := s.LocalWrite(upsertMut("sync-park-4", "sdd/test/park4", "four", 1, baseT.Add(1*time.Second))); err != nil {
		t.Fatalf("LocalWrite: %v", err)
	}
	entries, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	seq := entries[0].LocalSeq // never parked

	if err := s.UnparkMutation(seq); !errors.Is(err, ErrMutationNotParked) {
		t.Errorf("UnparkMutation(not parked) = %v, want ErrMutationNotParked", err)
	}
	if err := s.UnparkMutation(999999); !errors.Is(err, ErrMutationNotParked) {
		t.Errorf("UnparkMutation(nonexistent) = %v, want ErrMutationNotParked", err)
	}
}

// TestDiscardMutation_ExcludesFromListParkedAndDrainOutbox: discarding a
// parked entry must remove it from BOTH the parked list and DrainOutbox
// (acked, exactly like a successful push), permanently.
func TestDiscardMutation_ExcludesFromListParkedAndDrainOutbox(t *testing.T) {
	s := openTempStore(t)

	if _, err := s.LocalWrite(upsertMut("sync-park-5", "sdd/test/park5", "five", 1, baseT.Add(1*time.Second))); err != nil {
		t.Fatalf("LocalWrite: %v", err)
	}
	entries, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	seq := entries[0].LocalSeq

	if err := s.ParkMutation(seq, "will be discarded"); err != nil {
		t.Fatalf("ParkMutation: %v", err)
	}
	if err := s.DiscardMutation(seq); err != nil {
		t.Fatalf("DiscardMutation: %v", err)
	}

	if after, err := s.DrainOutbox(0); err != nil {
		t.Fatalf("DrainOutbox after discard: %v", err)
	} else if len(after) != 0 {
		t.Errorf("DrainOutbox returned %d entries after discard, want 0", len(after))
	}
	if parked, err := s.ListParked(); err != nil {
		t.Fatalf("ListParked after discard: %v", err)
	} else if len(parked) != 0 {
		t.Errorf("ListParked returned %d entries after discard, want 0", len(parked))
	}

	// A second discard must fail — it is no longer a parked, unacked row.
	if err := s.DiscardMutation(seq); !errors.Is(err, ErrMutationNotParked) {
		t.Errorf("second DiscardMutation = %v, want ErrMutationNotParked", err)
	}
}

// TestRecordPushFailure_UpdatesAttemptsWithoutParking covers the RETRYABLE
// path: attempts/last_error must be recorded, but the entry stays fully
// pending (still returned by DrainOutbox) — a 5xx must never be auto-parked.
func TestRecordPushFailure_UpdatesAttemptsWithoutParking(t *testing.T) {
	s := openTempStore(t)

	if _, err := s.LocalWrite(upsertMut("sync-park-6", "sdd/test/park6", "six", 1, baseT.Add(1*time.Second))); err != nil {
		t.Fatalf("LocalWrite: %v", err)
	}
	entries, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	seq := entries[0].LocalSeq

	if err := s.RecordPushFailure(seq, "remote: server returned 503: overloaded"); err != nil {
		t.Fatalf("RecordPushFailure: %v", err)
	}

	after, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox after failure record: %v", err)
	}
	if len(after) != 1 || after[0].LocalSeq != seq {
		t.Fatalf("DrainOutbox after failure record = %+v, want the entry still pending", after)
	}
	if parked, err := s.ListParked(); err != nil {
		t.Fatalf("ListParked: %v", err)
	} else if len(parked) != 0 {
		t.Errorf("ListParked returned %d entries, want 0 — a retryable failure must not park", len(parked))
	}

	var attempts int
	var lastError string
	if err := s.db.QueryRow(
		`SELECT attempts, last_error FROM sync_mutations WHERE local_seq = ?`, seq,
	).Scan(&attempts, &lastError); err != nil {
		t.Fatalf("query attempts/last_error: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if lastError == "" {
		t.Error("last_error is empty, want the recorded failure")
	}
}

// TestDrainOutbox_ParksUndecodablePayloadAndContinues is the other DrainOutbox
// half: a row whose OWN stored payload cannot be decoded (a pre-fix NUL-byte
// row, storage corruption) must be parked with the decode error, and the call
// must still succeed and return every OTHER pending entry — one bad row must
// not wedge the whole outbox.
func TestDrainOutbox_ParksUndecodablePayloadAndContinues(t *testing.T) {
	s := openTempStore(t)

	if _, err := s.LocalWrite(upsertMut("sync-park-7", "sdd/test/park7", "good", 1, baseT.Add(1*time.Second))); err != nil {
		t.Fatalf("LocalWrite (good): %v", err)
	}

	// Hand-insert a row with garbage (undecodable) payload, simulating a
	// pre-fix corrupt entry that bypassed LocalWrite's own validation.
	if _, err := s.db.Exec(`
		INSERT INTO sync_mutations (mutation_id, entity, entity_key, op, payload, writer_id, occurred_at)
		VALUES ('mut-undecodable', 'memory', 'sync-park-8', 'upsert', 'not valid canonical payload', 'w', ?)`,
		baseT.Add(2*time.Second).Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert undecodable row: %v", err)
	}

	entries, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("DrainOutbox returned %d entries, want 1 (the good one, undecodable parked)", len(entries))
	}
	if entries[0].Mutation.Content != "good" {
		t.Errorf("DrainOutbox returned content %q, want the decodable row's content", entries[0].Mutation.Content)
	}

	parked, err := s.ListParked()
	if err != nil {
		t.Fatalf("ListParked: %v", err)
	}
	if len(parked) != 1 {
		t.Fatalf("ListParked returned %d entries, want 1 (the undecodable row)", len(parked))
	}
	if parked[0].MutationID != "mut-undecodable" {
		t.Errorf("parked mutation_id = %q, want %q", parked[0].MutationID, "mut-undecodable")
	}
	if parked[0].LastError == "" {
		t.Error("parked entry's LastError is empty, want the decode failure")
	}

	// A second DrainOutbox call must not try to re-park the same row (it is
	// already parked, hence excluded) and must keep returning the good one.
	again, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("second DrainOutbox: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("second DrainOutbox returned %d entries, want 1", len(again))
	}
}
