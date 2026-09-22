package localstore

// nul_repair.go implements FUP-004's automatic NUL-byte repair: before this
// project rejected U+0000 on the way in (mutation.ValidateTextFields) and on
// the way out (cloudserve's own wire validation, and centralstore.Apply's
// data-exception classification), a node could enqueue an outbox entry
// carrying it. Central now refuses that mutation permanently — FUP-004b parks
// it — and it would stay parked forever with no path back, since the STORED
// payload is what keeps getting rejected. This file repairs it in place: strip
// U+0000, re-derive the content-addressed mutation_id from the sanitized
// payload, and un-park the entry so the syncer's next push cycle can actually
// succeed.

import (
	"fmt"
	"time"

	"github.com/mariesqu/engram/internal/mutation"
)

// RepairUnackedNULMutations finds every UNACKED (acked_at IS NULL — pending
// OR parked, deliberately both: see the package doc above) outbox entry whose
// canonical payload DECODES to a text field containing U+0000, and repairs
// each one in its own transaction.
//
// Detection decodes the JSON first rather than grepping the stored payload's
// raw bytes for a literal 0x00: json.Marshal always ESCAPES a NUL rune as the
// six-character sequence `\u0000`, so the payload TEXT never contains a raw
// NUL byte even when the mutation it encodes does — only after JSON decoding
// does `\u0000` become an actual U+0000 rune in a Go string. mutation.
// ValidateCanonicalPayloadText is the SAME check centralstore.Apply and
// cloudserve's wire boundary already use to reject these payloads, reused
// here as the detector so "does this need repair" can never disagree with
// "would this get rejected".
//
// Scope is UNACKED only, on purpose: an already-pushed mutation's effect is
// central's problem now — central presumably already applied (or rejected)
// those exact bytes, and retroactively rewriting this node's local state to
// point at a different mutation_id than the one central actually saw would
// desynchronize the two rather than fix anything.
//
// "Cheap" per the FUP-004 requirement: the SQL side filters on acked_at IS
// NULL — the same leading column idx_sync_mutations_drain indexes — which in
// steady state is small by construction (DrainOutbox keeps it drained); the
// decode-and-validate pass then runs once per unacked row, in memory.
//
// Returns the number of rows successfully repaired. A single row's repair
// failing (or turning out to be a no-op — see repairOneNULMutation) does not
// abort the scan; every candidate is attempted independently.
func (s *Store) RepairUnackedNULMutations() (int, error) {
	rows, err := s.db.Query(`SELECT local_seq, mutation_id, payload FROM sync_mutations WHERE acked_at IS NULL`)
	if err != nil {
		return 0, fmt.Errorf("RepairUnackedNULMutations: query: %w", err)
	}

	type candidate struct {
		localSeq   int64
		mutationID string
		payload    string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.localSeq, &c.mutationID, &c.payload); err != nil {
			rows.Close()
			return 0, fmt.Errorf("RepairUnackedNULMutations: scan: %w", err)
		}
		if mutation.ValidateCanonicalPayloadText([]byte(c.payload)) != nil {
			candidates = append(candidates, c)
		}
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return 0, fmt.Errorf("RepairUnackedNULMutations: rows: %w", rowsErr)
	}

	repaired := 0
	for _, c := range candidates {
		ok, err := s.repairOneNULMutation(c.localSeq, c.mutationID, c.payload)
		if err != nil {
			return repaired, fmt.Errorf("RepairUnackedNULMutations: local_seq=%d: %w", c.localSeq, err)
		}
		if ok {
			repaired++
		}
	}
	return repaired, nil
}

// repairOneNULMutation repairs one outbox row inside a SINGLE transaction, so
// a crash mid-repair can never leave sync_mutations, applied_mutations, and
// the materialized memories/memory_tombstones rows disagreeing about which
// mutation_id is current. Returns (false, nil) for a benign no-op: the
// payload no longer decodes (DrainOutbox's own decode-failure park handles
// that case), sanitizing produced an IDENTICAL payload (nothing to repair —
// the NUL match came from something the substring scan sees but the decoded
// text fields don't, e.g. a stray byte elsewhere in the JSON), or a
// concurrent caller already acked/repaired the row first.
func (s *Store) repairOneNULMutation(localSeq int64, oldMutationID, payload string) (bool, error) {
	m, decErr := mutation.FromCanonicalPayload([]byte(payload))
	if decErr != nil {
		return false, nil // not this function's job — DrainOutbox parks it
	}
	m.MutationID = oldMutationID

	sanitized := mutation.SanitizeTextFields(m)
	newPayload := mutation.CanonicalPayload(sanitized)
	newMutationID := mutation.NewMutationID(newPayload)
	if newMutationID == oldMutationID {
		return false, nil // sanitizing changed nothing derivable — no repair needed
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Re-check under the transaction (and the write lock): a concurrent
	// AckMutation, ParkMutation, or another repair pass may have already
	// resolved this row between the outer scan and here.
	var stillPending int
	if err := tx.QueryRow(
		`SELECT count(*) FROM sync_mutations WHERE local_seq = ? AND acked_at IS NULL AND mutation_id = ?`,
		localSeq, oldMutationID,
	).Scan(&stillPending); err != nil {
		return false, fmt.Errorf("recheck: %w", err)
	}
	if stillPending == 0 {
		return false, nil
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(
		`UPDATE sync_mutations
		   SET mutation_id = ?, payload = ?, parked_at = NULL, attempts = 0,
		       last_error = ?, last_attempt_at = ?
		 WHERE local_seq = ?`,
		newMutationID, string(newPayload),
		fmt.Sprintf("auto-repaired U+0000 (was mutation_id=%s)", oldMutationID), now,
		localSeq,
	); err != nil {
		return false, fmt.Errorf("update sync_mutations: %w", err)
	}

	// applied_mutations is the INV5 idempotency marker this LOCAL write left
	// behind (every non-NoOp local write records one — see applyTx). UPDATE the
	// primary key in place, not delete+insert: the id changing IS the correction,
	// and this is the same row's marker either way.
	if _, err := tx.Exec(
		`UPDATE applied_mutations SET mutation_id = ? WHERE mutation_id = ?`,
		newMutationID, oldMutationID,
	); err != nil {
		return false, fmt.Errorf("update applied_mutations: %w", err)
	}

	// last_write_mutation_id is the FINAL LWW tiebreaker (see writeWins) — it
	// must track the id this mutation is now known by. The WHERE guard (still
	// equals the OLD id) is load-bearing: if a NEWER write has since superseded
	// this row, its last_write_mutation_id already names that newer mutation,
	// and this repair must not touch a row it no longer owns.
	if _, err := tx.Exec(
		`UPDATE memories SET last_write_mutation_id = ? WHERE sync_id = ? AND last_write_mutation_id = ?`,
		newMutationID, m.SyncID, oldMutationID,
	); err != nil {
		return false, fmt.Errorf("update memories.last_write_mutation_id: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE memory_tombstones SET last_write_mutation_id = ? WHERE sync_id = ? AND last_write_mutation_id = ?`,
		newMutationID, m.SyncID, oldMutationID,
	); err != nil {
		return false, fmt.Errorf("update memory_tombstones.last_write_mutation_id: %w", err)
	}

	// The materialized memories row itself: this node's own copy carries the
	// SAME corrupted text the outbox entry did (LocalWrite materializes and
	// enqueues from the identical sanitized-or-not mutation). Gated on the
	// row's last_write_mutation_id NOW equal to newMutationID (set by the
	// UPDATE immediately above) — the same "still this exact write" guard, so
	// a superseding write's fresher content is never clobbered by this repair.
	// A prompt mutation's sync_id matches no memories row, so this — and the
	// last_write_mutation_id updates above — are a harmless no-op for it;
	// prompts have no last_write_mutation_id column to repair in the first
	// place (see promptTombstonesTableDDL's doc comment).
	if _, err := tx.Exec(
		`UPDATE memories
		   SET title = ?, content = ?, type = ?, session_id = ?, project = ?, scope = ?,
		       topic_key = ?, status = ?, writer_id = ?, parent_sync_id = ?
		 WHERE sync_id = ? AND last_write_mutation_id = ?`,
		sanitized.Title, sanitized.Content, sanitized.Type, sanitized.SessionID,
		sanitized.Project, sanitized.Scope,
		nullStr(sanitized.TopicKey), nullStr(sanitized.Status), sanitized.WriterID, nullStr(sanitized.ParentSyncID),
		m.SyncID, newMutationID,
	); err != nil {
		return false, fmt.Errorf("update materialized memories row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return true, nil
}
