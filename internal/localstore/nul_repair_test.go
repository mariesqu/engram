package localstore

// Tests for FUP-004c's automatic NUL-byte repair (nul_repair.go). Every
// fixture here hand-inserts raw rows carrying U+0000 rather than going
// through LocalWrite: LocalWrite has sanitized new local writes since the
// fix this repair exists to clean up AFTER, so a NUL-carrying row can only
// exist here as a stand-in for the pre-fix state these tests target.

import (
	"database/sql"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/mutation"
)

// seedNULOutboxAndMemory hand-inserts a matched pair: a sync_mutations row and
// its materialized memories row, both carrying the SAME (pre-sanitization)
// mutation_id and NUL-containing content — exactly the corrupted-but-linked
// state RepairUnackedNULMutations must repair consistently across both. Also
// inserts the applied_mutations marker so its re-pointing can be asserted.
// Returns the local_seq and the OLD (NUL-derived) mutation_id.
func seedNULOutboxAndMemory(t *testing.T, s *Store, syncID string, parked bool) (int64, string) {
	t.Helper()

	m := domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "bad\x00title",
		Content:    "bad\x00content",
		Project:    "engram",
		Scope:      "project",
		Version:    1,
		WriterID:   "writer-nul",
		UpdatedAt:  baseT,
	}
	payload := mutation.CanonicalPayload(m)
	mutID := mutation.NewMutationID(payload)

	if _, err := s.db.Exec(`
		INSERT INTO sync_mutations (mutation_id, entity, entity_key, op, payload, writer_id, occurred_at)
		VALUES (?, 'memory', ?, 'upsert', ?, 'writer-nul', ?)`,
		mutID, syncID, string(payload), baseT.Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("seed sync_mutations: %v", err)
	}

	var localSeq int64
	if err := s.db.QueryRow(`SELECT local_seq FROM sync_mutations WHERE mutation_id = ?`, mutID).
		Scan(&localSeq); err != nil {
		t.Fatalf("read seeded local_seq: %v", err)
	}

	if parked {
		if _, err := s.db.Exec(
			`UPDATE sync_mutations SET parked_at = ?, attempts = 1, last_error = 'rejected: U+0000' WHERE local_seq = ?`,
			baseT.Format(time.RFC3339Nano), localSeq,
		); err != nil {
			t.Fatalf("park seeded row: %v", err)
		}
	}

	if _, err := s.db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content, project, scope,
		   version, writer_id, last_write_mutation_id, created_at, updated_at)
		VALUES (?, ?, 'memory', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		syncID, m.SessionID, m.Type, m.Title, m.Content, m.Project, m.Scope,
		m.Version, m.WriterID, mutID,
		baseT.Format(time.RFC3339Nano), baseT.Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("seed memories row: %v", err)
	}

	if _, err := s.db.Exec(`INSERT INTO applied_mutations(mutation_id) VALUES (?)`, mutID); err != nil {
		t.Fatalf("seed applied_mutations: %v", err)
	}

	return localSeq, mutID
}

// TestRepairUnackedNULMutations_RepairsPendingEntry covers the plain (never
// parked) case: a pending outbox entry with U+0000 must be sanitized in
// place, get a NEW mutation_id, and its materialized memories row must be
// sanitized the same way with last_write_mutation_id following along.
func TestRepairUnackedNULMutations_RepairsPendingEntry(t *testing.T) {
	s := openTempStore(t)
	localSeq, oldMutID := seedNULOutboxAndMemory(t, s, "sync-nul-pending", false)

	n, err := s.RepairUnackedNULMutations()
	if err != nil {
		t.Fatalf("RepairUnackedNULMutations: %v", err)
	}
	if n != 1 {
		t.Fatalf("RepairUnackedNULMutations repaired %d row(s), want 1", n)
	}

	var newMutID, payload string
	var parkedAt, lastError sql.NullString
	var attempts int
	if err := s.db.QueryRow(
		`SELECT mutation_id, payload, parked_at, attempts, last_error FROM sync_mutations WHERE local_seq = ?`,
		localSeq,
	).Scan(&newMutID, &payload, &parkedAt, &attempts, &lastError); err != nil {
		t.Fatalf("read repaired sync_mutations row: %v", err)
	}
	if newMutID == oldMutID {
		t.Error("mutation_id did not change — the repair must re-derive a new one")
	}
	if containsNUL(payload) {
		t.Errorf("repaired payload still contains U+0000: %q", payload)
	}
	if parkedAt.Valid {
		t.Error("parked_at is set after repair, want NULL — a repaired entry must un-park")
	}
	if attempts != 0 {
		t.Errorf("attempts = %d after repair, want 0 (reset)", attempts)
	}

	var title, content, memMutID string
	if err := s.db.QueryRow(
		`SELECT title, content, last_write_mutation_id FROM memories WHERE sync_id = 'sync-nul-pending'`,
	).Scan(&title, &content, &memMutID); err != nil {
		t.Fatalf("read repaired memories row: %v", err)
	}
	if containsNUL(title) || containsNUL(content) {
		t.Errorf("materialized row still contains U+0000: title=%q content=%q", title, content)
	}
	if memMutID != newMutID {
		t.Errorf("memories.last_write_mutation_id = %q, want the new mutation_id %q", memMutID, newMutID)
	}

	var appliedCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM applied_mutations WHERE mutation_id = ?`, newMutID).
		Scan(&appliedCount); err != nil {
		t.Fatalf("query applied_mutations: %v", err)
	}
	if appliedCount != 1 {
		t.Errorf("applied_mutations has %d row(s) for the new mutation_id, want 1", appliedCount)
	}
	var oldStillPresent int
	if err := s.db.QueryRow(`SELECT count(*) FROM applied_mutations WHERE mutation_id = ?`, oldMutID).
		Scan(&oldStillPresent); err != nil {
		t.Fatalf("query applied_mutations (old id): %v", err)
	}
	if oldStillPresent != 0 {
		t.Errorf("applied_mutations still has the OLD mutation_id, want it re-pointed, not duplicated")
	}
}

// TestRepairUnackedNULMutations_RepairsParkedEntryAndPushIsRetried covers the
// realistic path: a PARKED entry (central already rejected the corrupted
// bytes) must be un-parked by the repair, and the resulting clean entry must
// come back from DrainOutbox for the syncer to actually push it.
func TestRepairUnackedNULMutations_RepairsParkedEntryAndPushIsRetried(t *testing.T) {
	s := openTempStore(t)
	localSeq, _ := seedNULOutboxAndMemory(t, s, "sync-nul-parked", true)

	// Sanity: parked entries are excluded from DrainOutbox before repair.
	before, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox (before repair): %v", err)
	}
	for _, e := range before {
		if e.LocalSeq == localSeq {
			t.Fatal("the parked seed row appeared in DrainOutbox before repair — fixture is wrong")
		}
	}

	if n, err := s.RepairUnackedNULMutations(); err != nil {
		t.Fatalf("RepairUnackedNULMutations: %v", err)
	} else if n != 1 {
		t.Fatalf("repaired %d row(s), want 1", n)
	}

	after, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox (after repair): %v", err)
	}
	found := false
	for _, e := range after {
		if e.LocalSeq == localSeq {
			found = true
			if containsNUL(e.Mutation.Content) || containsNUL(e.Mutation.Title) {
				t.Errorf("DrainOutbox returned still-corrupted content: title=%q content=%q",
					e.Mutation.Title, e.Mutation.Content)
			}
		}
	}
	if !found {
		t.Fatal("the repaired entry did not come back from DrainOutbox — un-park did not take effect")
	}
}

// TestRepairUnackedNULMutations_SkipsSupersededRow proves the guard: if a
// NEWER write has already replaced the corrupted row's last_write_mutation_id,
// the repair must not clobber that fresher content — it still repairs the
// OUTBOX entry (so it can push and stop wedging its group) but leaves the
// materialized memories row alone.
func TestRepairUnackedNULMutations_SkipsSupersededRow(t *testing.T) {
	s := openTempStore(t)
	localSeq, _ := seedNULOutboxAndMemory(t, s, "sync-nul-superseded", false)

	// A newer write lands on the SAME sync_id, moving last_write_mutation_id
	// away from the corrupted outbox entry's id — simulating a local edit (or a
	// pulled newer version) that arrived before the repair ran.
	if _, err := s.db.Exec(
		`UPDATE memories SET title = 'fresh title', content = 'fresh content',
		        version = 2, last_write_mutation_id = 'newer-mutation-id'
		 WHERE sync_id = 'sync-nul-superseded'`,
	); err != nil {
		t.Fatalf("simulate newer write: %v", err)
	}

	if n, err := s.RepairUnackedNULMutations(); err != nil {
		t.Fatalf("RepairUnackedNULMutations: %v", err)
	} else if n != 1 {
		t.Fatalf("repaired %d row(s), want 1 (the outbox entry itself still repairs)", n)
	}

	var title, content, memMutID string
	if err := s.db.QueryRow(
		`SELECT title, content, last_write_mutation_id FROM memories WHERE sync_id = 'sync-nul-superseded'`,
	).Scan(&title, &content, &memMutID); err != nil {
		t.Fatalf("read memories row: %v", err)
	}
	if title != "fresh title" || content != "fresh content" {
		t.Errorf("the superseding write was clobbered: title=%q content=%q", title, content)
	}
	if memMutID != "newer-mutation-id" {
		t.Errorf("last_write_mutation_id = %q, want it untouched (\"newer-mutation-id\")", memMutID)
	}

	// The outbox entry itself is still cleanly repaired — it can push now.
	entries, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.LocalSeq == localSeq {
			found = true
		}
	}
	if !found {
		t.Error("the outbox entry was not repaired/un-parked despite the memories row being superseded")
	}
}

// TestRepairUnackedNULMutations_IgnoresAckedRows: an already-pushed mutation
// is out of scope by design — central already saw those exact bytes.
func TestRepairUnackedNULMutations_IgnoresAckedRows(t *testing.T) {
	s := openTempStore(t)
	localSeq, oldMutID := seedNULOutboxAndMemory(t, s, "sync-nul-acked", false)

	if _, err := s.db.Exec(
		`UPDATE sync_mutations SET acked_at = ? WHERE local_seq = ?`,
		baseT.Format(time.RFC3339Nano), localSeq,
	); err != nil {
		t.Fatalf("ack the seed row: %v", err)
	}

	n, err := s.RepairUnackedNULMutations()
	if err != nil {
		t.Fatalf("RepairUnackedNULMutations: %v", err)
	}
	if n != 0 {
		t.Errorf("repaired %d acked row(s), want 0", n)
	}

	var mutID string
	if err := s.db.QueryRow(`SELECT mutation_id FROM sync_mutations WHERE local_seq = ?`, localSeq).
		Scan(&mutID); err != nil {
		t.Fatalf("read: %v", err)
	}
	if mutID != oldMutID {
		t.Errorf("acked row's mutation_id changed to %q, want it untouched (%q)", mutID, oldMutID)
	}
}

// TestRepairUnackedNULMutations_NoCandidatesIsANoOp confirms the common case
// (nothing to repair) costs nothing and errors nothing.
func TestRepairUnackedNULMutations_NoCandidatesIsANoOp(t *testing.T) {
	s := openTempStore(t)

	if _, err := s.LocalWrite(upsertMut("sync-clean", "sdd/test/clean", "clean content", 1, baseT)); err != nil {
		t.Fatalf("LocalWrite: %v", err)
	}

	n, err := s.RepairUnackedNULMutations()
	if err != nil {
		t.Fatalf("RepairUnackedNULMutations: %v", err)
	}
	if n != 0 {
		t.Errorf("repaired %d row(s) on a clean outbox, want 0", n)
	}
}

func containsNUL(s string) bool {
	for _, r := range s {
		if r == 0 {
			return true
		}
	}
	return false
}
