package localstore

// sync.go is the local store's SYNC SURFACE — the minimal API a node needs to
// participate in the push/pull cycle with the central store:
//
//   • LocalWrite   — apply a NEW local write through the SAME domain.Decide path
//                    used by pull-apply, AND enqueue it into the sync_mutations
//                    outbox so it can later be pushed to central.
//   • DrainOutbox  — list pending (unacked) outbox entries in local order.
//   • AckMutation  — mark an outbox entry pushed (sets acked_at) and advance the
//                    push cursor (sync_state.last_acked_seq).
//   • PullCursor / SetPullCursor — get/set sync_state.last_pulled_seq for the
//                    central target.
//
// This is NOT throwaway: it is the local store's real sync API. The in-process
// spike harness uses it; a future network transport will use the same methods.
// The harness lives in internal/spike; this file deliberately keeps zero test
// knowledge.

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/mutation"
)

// defaultTargetKey is the sync_state row that tracks the single central target.
// The schema seeds exactly one row with this key (see schema.go ApplySchema).
const defaultTargetKey = "central"

// OutboxEntry is one pending row in the sync_mutations push journal, decoded
// back into a domain.Mutation plus the local push-ordering key (LocalSeq).
//
// Mutation carries the full content reconstructed from the canonical Payload
// via mutation.FromCanonicalPayload (Op, EntityType, SyncID, WriterID and the
// content fields are all decoded from the JSON payload). The identity and
// ordering fields that live in the row but not inside the payload — MutationID,
// Payload itself, and OccurredAt — are filled from the corresponding columns
// after decoding. The entry is ready to push to central without touching the
// memories table again.
type OutboxEntry struct {
	// LocalSeq is the sync_mutations.local_seq AUTOINCREMENT — the local push
	// order. AckMutation advances last_acked_seq to this value.
	LocalSeq int64
	// Mutation is the fully-reconstructed mutation ready to push to central.
	Mutation domain.Mutation
}

// LocalWrite applies a brand-new LOCAL write to this store and enqueues it for
// push to central. It is the local-write twin of pull-apply:
//
//  1. Derive the canonical Payload and MutationID if the caller left them unset
//     (MutationID = NewMutationID(CanonicalPayload(m))). This makes the local
//     write content-addressed and idempotent on re-apply (INV5).
//  2. Open a SQLite transaction, then run domain.Decide(&txReader{tx}, m) INSIDE
//     the transaction so the decision and the subsequent apply see the same
//     consistent snapshot. With db.SetMaxOpenConns(1) the single connection is
//     held by the tx for its entire duration, so a concurrent LocalWrite or
//     ApplyPulled on another goroutine cannot interleave between Decide and
//     applyTx — the whole decide+apply+enqueue sequence is atomic.
//  3. Execute applyTx (the local state change) AND enqueueOutboxTx (the outbox
//     INSERT) inside that SAME transaction. Both operations commit together or
//     not at all: a crash between the two can never leave the local memory table
//     updated without a corresponding outbox entry.
//
// The returned Mutation is the normalized mutation (Payload + MutationID filled
// in) so callers can inspect the derived ID.
func (s *Store) LocalWrite(m domain.Mutation) (domain.Mutation, error) {
	m = normalizeMutation(m)

	// Open the transaction FIRST so that Decide, applyTx, and enqueueOutboxTx all
	// run on the same snapshot. This mirrors centralstore.Apply's pattern where
	// Decide is run against a decideReader wrapping the in-flight transaction.
	tx, err := s.db.Begin()
	if err != nil {
		return m, fmt.Errorf("LocalWrite: begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Decide inside the transaction: txReader routes all Reader calls through the
	// same *sql.Tx, so Decide sees a consistent snapshot that includes no writes
	// from concurrent goroutines that haven't committed yet.
	d := domain.Decide(&txReader{tx: tx}, m)

	// The outbox is populated even when the local Decision is NoOp. A local NoOp
	// means the local store already reflects this mutation (or an equivalent
	// newer write), but the central store may not — it assigns authoritative seqs
	// independently. Forwarding the mutation lets central reconcile with its own
	// Decide path. INSERT OR IGNORE on the UNIQUE mutation_id makes a true
	// idempotent re-enqueue a no-op at the SQL layer (INV5).
	if d.Action != domain.NoOp {
		if err = applyTx(tx, d, m); err != nil {
			return m, fmt.Errorf("LocalWrite: applyTx: %w", err)
		}
	}
	if err = enqueueOutboxTx(tx, m); err != nil {
		return m, fmt.Errorf("LocalWrite: enqueueOutboxTx: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return m, fmt.Errorf("LocalWrite: commit: %w", err)
	}
	return m, nil
}

// normalizeMutation fills Payload and MutationID from the canonical encoding
// when the caller left them unset, and defaults OccurredAt to now. It never
// overrides values the caller already supplied (so re-applying a pulled mutation
// keeps its central MutationID/Payload).
//
// NormalizeTopicKey runs FIRST so the canonical payload (and therefore the
// content-addressed MutationID) always reflects nil for no-topic writes —
// &"" and nil converge to the same mutation_id, and '' never reaches any index
// (every partial topic index uses `WHERE topic_key IS NOT NULL`, which is the
// complete no-topic exclusion once '' is normalised away at store entry).
func normalizeMutation(m domain.Mutation) domain.Mutation {
	m = domain.NormalizeTopicKey(m) // fold &"" → nil before payload/ID derivation
	if len(m.Payload) == 0 {
		m.Payload = mutation.CanonicalPayload(m)
	}
	if m.MutationID == "" {
		m.MutationID = mutation.NewMutationID(m.Payload)
	}
	if m.OccurredAt.IsZero() {
		m.OccurredAt = time.Now().UTC()
	}
	return m
}

// enqueueOutboxTx inserts the mutation into sync_mutations on the given
// transaction. INSERT OR IGNORE on the UNIQUE mutation_id makes a duplicate
// enqueue a no-op (idempotent local write — INV5 at the outbox layer).
// The caller owns the transaction lifecycle (Begin/Commit/Rollback).
func enqueueOutboxTx(tx *sql.Tx, m domain.Mutation) error {
	_, err := tx.Exec(`
		INSERT OR IGNORE INTO sync_mutations
		  (mutation_id, entity, entity_key, op, payload, writer_id, occurred_at)
		VALUES (?,?,?,?,?,?,?)`,
		m.MutationID,
		string(m.EntityType),
		m.SyncID,
		string(m.Op),
		string(m.Payload),
		m.WriterID,
		m.OccurredAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("enqueueOutboxTx: %w", err)
	}
	return nil
}

// DrainOutbox returns the pending (acked_at IS NULL) outbox entries in local
// push order (local_seq ASC), up to limit. limit <= 0 returns all pending rows.
//
// Each entry's Mutation is reconstructed from the stored canonical Payload via
// mutation.FromCanonicalPayload, then the identity/ordering fields that live in
// the row but not in the payload (MutationID, OccurredAt, Payload) are filled in.
// The entry is ready to push to central exactly as-is.
func (s *Store) DrainOutbox(limit int) ([]OutboxEntry, error) {
	q := `
		SELECT local_seq, mutation_id, payload, occurred_at
		FROM sync_mutations
		WHERE acked_at IS NULL
		ORDER BY local_seq ASC`
	args := []any{}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("DrainOutbox: query: %w", err)
	}
	defer rows.Close()

	var out []OutboxEntry
	for rows.Next() {
		var (
			localSeq      int64
			mutationID    string
			payload       string
			occurredAtStr string
		)
		if err := rows.Scan(&localSeq, &mutationID, &payload, &occurredAtStr); err != nil {
			return nil, fmt.Errorf("DrainOutbox: scan: %w", err)
		}

		m, err := mutation.FromCanonicalPayload([]byte(payload))
		if err != nil {
			return nil, fmt.Errorf("DrainOutbox: decode payload (mutation_id=%s): %w", mutationID, err)
		}
		m.MutationID = mutationID
		m.Payload = []byte(payload)
		t := parseTime(occurredAtStr)
		if t.IsZero() {
			return nil, fmt.Errorf("DrainOutbox: mutation_id=%s: occurred_at %q is not a valid timestamp", mutationID, occurredAtStr)
		}
		m.OccurredAt = t

		out = append(out, OutboxEntry{LocalSeq: localSeq, Mutation: m})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("DrainOutbox: rows: %w", err)
	}
	return out, nil
}

// AckMutation marks the outbox entry with the given local_seq as pushed (sets
// acked_at = now) and advances the push cursor (sync_state.last_acked_seq) to
// localSeq when it is ahead of the stored value. Both writes run in one tx so the
// outbox marker and the cursor stay consistent.
//
// If the UPDATE matches no row — because localSeq does not exist or the row is
// already acked — AckMutation returns an error and does NOT advance the cursor.
// This prevents the cursor from drifting ahead of genuinely pushed mutations.
func (s *Store) AckMutation(localSeq int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("AckMutation: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	var res sql.Result
	if res, err = tx.Exec(
		`UPDATE sync_mutations SET acked_at = ? WHERE local_seq = ? AND acked_at IS NULL`,
		now, localSeq,
	); err != nil {
		return fmt.Errorf("AckMutation: mark acked: %w", err)
	}

	// Guard: if no row was updated the caller supplied a wrong local_seq (does not
	// exist) or already-acked local_seq. Do NOT advance the cursor in that case —
	// the cursor must only move when a real pending row is acked.
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("AckMutation: rows affected: %w", err)
	}
	if affected == 0 {
		// Roll back via defer; return a named error so callers can detect the case.
		err = fmt.Errorf("AckMutation: no pending outbox row for local_seq=%d", localSeq)
		return err
	}

	// Advance the monotonic push cursor (never move it backwards).
	if _, err = tx.Exec(
		`UPDATE sync_state SET last_acked_seq = ?
		 WHERE target_key = ? AND last_acked_seq < ?`,
		localSeq, defaultTargetKey, localSeq,
	); err != nil {
		return fmt.Errorf("AckMutation: advance cursor: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("AckMutation: commit: %w", err)
	}
	return nil
}

// PendingCount returns the number of unacked rows currently in the outbox.
// Handy for harness assertions (e.g. confirming a push drained everything).
func (s *Store) PendingCount() (int, error) {
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM sync_mutations WHERE acked_at IS NULL`,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("PendingCount: %w", err)
	}
	return n, nil
}

// PullCursor returns the last central seq this store has pulled and applied
// (sync_state.last_pulled_seq for the central target). A fresh store returns 0.
func (s *Store) PullCursor() (int64, error) {
	var seq int64
	err := s.db.QueryRow(
		`SELECT last_pulled_seq FROM sync_state WHERE target_key = ?`,
		defaultTargetKey,
	).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("PullCursor: %w", err)
	}
	return seq, nil
}

// SetPullCursor advances sync_state.last_pulled_seq to seq for the central
// target. The cursor is monotonic: a seq lower than the stored value is ignored
// (re-pulling an older window must never rewind the cursor). The default
// 'central' row is seeded by ApplySchema; UPSERT guards the case where it is
// somehow absent.
func (s *Store) SetPullCursor(seq int64) error {
	_, err := s.db.Exec(`
		INSERT INTO sync_state (target_key, last_pulled_seq)
		VALUES (?, ?)
		ON CONFLICT(target_key) DO UPDATE SET
		  last_pulled_seq = excluded.last_pulled_seq
		WHERE excluded.last_pulled_seq > sync_state.last_pulled_seq`,
		defaultTargetKey, seq,
	)
	if err != nil {
		return fmt.Errorf("SetPullCursor: %w", err)
	}
	return nil
}

// ApplyPulled applies a mutation pulled FROM central to this local store. It is
// a thin convenience wrapper used by the pull half of the sync harness: it runs
// the SAME domain.Decide → applyTx path as LocalWrite, but does NOT enqueue
// anything into the outbox (a pulled mutation must not be re-pushed).
//
// Like LocalWrite, Decide runs INSIDE the transaction (via txReader) so the
// decision and the apply see the same consistent snapshot — a concurrent
// LocalWrite that commits between Decide and apply cannot make the decision stale.
//
// The mutation arrives carrying its central Seq, MutationID and Payload (from
// PullSince); those are preserved as-is. Decide's INV5 guard (MutationApplied)
// plus applyTx's applied_mutations INSERT make a re-pulled mutation a no-op.
func (s *Store) ApplyPulled(m domain.Mutation) error {
	// Defensive normalisation: a well-behaved central store already sends nil for
	// no-topic mutations, but fold &"" → nil here so '' never reaches any local
	// index (every partial topic index uses `WHERE topic_key IS NOT NULL`, which
	// is the complete no-topic exclusion once '' is normalised away at store entry).
	m = domain.NormalizeTopicKey(m)

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("ApplyPulled: begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	d := domain.Decide(&txReader{tx: tx}, m)
	if d.Action != domain.NoOp {
		if err = applyTx(tx, d, m); err != nil {
			return fmt.Errorf("ApplyPulled: applyTx: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("ApplyPulled: commit: %w", err)
	}
	return nil
}
