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
// Mutation carries the full content (decoded from the canonical Payload) plus
// MutationID, Op, EntityType (entity), SyncID (entity_key) and WriterID read
// straight from the row, so the harness can push it to central without touching
// the memories table again.
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
//  2. Run domain.Decide(localReader, m) against THIS store, then localstore.Apply
//     the resulting Decision — exactly the path a pulled mutation takes — so the
//     local memories/tombstone state is updated through the one guarded apply.
//  3. Enqueue the mutation into sync_mutations (the outbox) so DrainOutbox/push
//     can deliver it to central later.
//
// The returned Mutation is the normalized mutation (Payload + MutationID filled
// in) so callers can inspect the derived ID.
//
// Note: steps 2 and 3 are not wrapped in a single SQLite transaction here.
// Apply already commits its own tx; the outbox INSERT uses INSERT OR IGNORE on
// the UNIQUE mutation_id, so a re-run of the same logical write is a no-op in
// the outbox too. For the in-process spike this ordering is sufficient; a
// production transport would fold both into one tx (a documented follow-up).
func (s *Store) LocalWrite(m domain.Mutation) (domain.Mutation, error) {
	m = normalizeMutation(m)

	// Apply locally through the SAME Decide path pull-apply uses.
	d := domain.Decide(s, m)
	if err := Apply(s.db, d, m); err != nil {
		return m, fmt.Errorf("LocalWrite: apply: %w", err)
	}

	// Enqueue into the outbox for push.
	if err := s.enqueueOutbox(m); err != nil {
		return m, fmt.Errorf("LocalWrite: enqueue: %w", err)
	}
	return m, nil
}

// normalizeMutation fills Payload and MutationID from the canonical encoding
// when the caller left them unset, and defaults OccurredAt to now. It never
// overrides values the caller already supplied (so re-applying a pulled mutation
// keeps its central MutationID/Payload).
func normalizeMutation(m domain.Mutation) domain.Mutation {
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

// enqueueOutbox inserts the mutation into sync_mutations. INSERT OR IGNORE on the
// UNIQUE mutation_id makes a duplicate enqueue a no-op (idempotent local write).
func (s *Store) enqueueOutbox(m domain.Mutation) error {
	_, err := s.db.Exec(`
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
		return fmt.Errorf("enqueueOutbox: %w", err)
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
		m.OccurredAt = parseTime(occurredAtStr)

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
	if _, err = tx.Exec(
		`UPDATE sync_mutations SET acked_at = ? WHERE local_seq = ? AND acked_at IS NULL`,
		now, localSeq,
	); err != nil {
		return fmt.Errorf("AckMutation: mark acked: %w", err)
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
// the SAME domain.Decide(localReader, m) → Apply path as a local write, but does
// NOT enqueue anything into the outbox (a pulled mutation must not be re-pushed).
//
// The mutation arrives carrying its central Seq, MutationID and Payload (from
// PullSince); those are preserved as-is. Decide's INV5 guard (MutationApplied)
// plus Apply's applied_mutations INSERT make a re-pulled mutation a no-op.
func (s *Store) ApplyPulled(m domain.Mutation) error {
	d := domain.Decide(s, m)
	if err := Apply(s.db, d, m); err != nil {
		return fmt.Errorf("ApplyPulled: %w", err)
	}
	return nil
}
