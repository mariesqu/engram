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
// push to central. It acquires the write lock and delegates to localWriteLocked
// so the entire decide+apply+enqueue sequence is atomic with respect to any
// concurrent write (AddObservation, ApplyPulled, etc.).
//
// The returned Mutation is the normalized mutation (Payload + MutationID filled
// in) so callers can inspect the derived ID.
func (s *Store) LocalWrite(m domain.Mutation) (domain.Mutation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.localWriteLocked(m)
}

// localWriteLocked is the mutex-free core of LocalWrite. Callers MUST hold s.mu
// before calling this method. Using a separate locked/unlocked split avoids
// deadlock: AddObservation acquires mu once and calls localWriteLocked directly
// rather than calling the public LocalWrite which would attempt a recursive lock.
//
//  1. Derive the canonical Payload and MutationID if the caller left them unset
//     (MutationID = NewMutationID(CanonicalPayload(m))). This makes the local
//     write content-addressed and idempotent on re-apply (INV5).
//  2. Open a SQLite transaction, then run domain.Decide(&txReader{tx}, m) INSIDE
//     the transaction so the decision and the subsequent apply see the same
//     consistent snapshot. With db.SetMaxOpenConns(1) the single connection is
//     held by the tx for its entire duration, so a concurrent write on another
//     goroutine is excluded by mu before it can even begin the transaction —
//     the whole decide+apply+enqueue sequence is atomic.
//  3. Execute applyTx (the local state change) AND enqueueOutboxTx (the outbox
//     INSERT) inside that SAME transaction. Both operations commit together or
//     not at all: a crash between the two can never leave the local memory table
//     updated without a corresponding outbox entry.
func (s *Store) localWriteLocked(m domain.Mutation) (domain.Mutation, error) {
	m = normalizeMutation(m)

	// Open the transaction FIRST so that Decide, applyTx, and enqueueOutboxTx all
	// run on the same snapshot.
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
	// same *sql.Tx, so Decide sees a consistent snapshot.
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
// NormalizeTopicKey runs FIRST so that when normalizeMutation derives the
// canonical payload (and therefore the content-addressed MutationID), no-topic
// writes always reflect nil — &"" and nil converge — and '' never reaches any
// index (every partial topic index uses `WHERE topic_key IS NOT NULL`, which is
// complete once '' is normalised away at store entry).
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
	s.mu.Lock()
	defer s.mu.Unlock()

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
	s.mu.Lock()
	defer s.mu.Unlock()

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

// PullCursorFor returns the last central seq this store has pulled and applied
// for the given project (pull_cursors.last_pulled_seq WHERE target_key='central'
// AND project=?). A missing row means no pulls have been made for that
// project yet and 0 is returned.
//
// PullCursorFor is the per-project replacement for PullCursor. The old
// PullCursor / SetPullCursor methods track a GLOBAL cursor (sync_state) which
// skips interleaved projects when multiple projects are pulled from central's
// single BIGSERIAL journal — see schema.go v5→v6 migration comment for the
// full correctness argument.
func (s *Store) PullCursorFor(project string) (int64, error) {
	var seq int64
	err := s.db.QueryRow(
		`SELECT last_pulled_seq FROM pull_cursors WHERE target_key = ? AND project = ?`,
		defaultTargetKey, project,
	).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("PullCursorFor(%q): %w", project, err)
	}
	return seq, nil
}

// SetPullCursorFor advances pull_cursors.last_pulled_seq to seq for the given
// project under the default central target. The cursor is monotonic: a seq
// lower than the stored value is ignored (re-pulling an older window must never
// rewind the cursor). The UPSERT creates the row if absent.
//
// Monotonicity proof: the ON CONFLICT … WHERE clause makes the UPDATE a no-op
// when excluded.last_pulled_seq ≤ pull_cursors.last_pulled_seq — identical to
// the existing SetPullCursor pattern.
func (s *Store) SetPullCursorFor(project string, seq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO pull_cursors (target_key, project, last_pulled_seq)
		VALUES (?, ?, ?)
		ON CONFLICT(target_key, project) DO UPDATE SET
		  last_pulled_seq = excluded.last_pulled_seq
		WHERE excluded.last_pulled_seq > pull_cursors.last_pulled_seq`,
		defaultTargetKey, project, seq,
	)
	if err != nil {
		return fmt.Errorf("SetPullCursorFor(%q, %d): %w", project, seq, err)
	}
	return nil
}

// ListProjects returns the distinct project names known to this store, derived
// from memories and memory_tombstones. This is the set of projects the autosync
// Loop should pull from central.
//
// The union of both tables covers all projects that have ever had a write
// (memories) or a delete (memory_tombstones) applied locally — whether those
// writes originated locally or were pulled from central. The result is sorted
// alphabetically so the caller iterates in a stable order.
func (s *Store) ListProjects() ([]string, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT project FROM memories
		UNION
		SELECT DISTINCT project FROM memory_tombstones
		ORDER BY project`)
	if err != nil {
		return nil, fmt.Errorf("ListProjects: query: %w", err)
	}
	defer rows.Close()

	var projects []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("ListProjects: scan: %w", err)
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListProjects: rows: %w", err)
	}
	return projects, nil
}

// ApplyPulled applies a mutation pulled FROM central to this local store. It
// acquires the write lock so it is mutually exclusive with LocalWrite and
// AddObservation — no interleaving between a local write's version pre-read and
// its commit is possible.
//
// It runs the SAME domain.Decide → applyTx path as LocalWrite, but does NOT
// enqueue anything into the outbox (a pulled mutation must not be re-pushed).
//
// The mutation arrives carrying its central Seq, MutationID and Payload (from
// PullSince); those are preserved as-is. Decide's INV5 guard (MutationApplied)
// plus applyTx's applied_mutations INSERT make a re-pulled mutation a no-op.
func (s *Store) ApplyPulled(m domain.Mutation) error {
	s.mu.Lock()
	defer s.mu.Unlock()

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
