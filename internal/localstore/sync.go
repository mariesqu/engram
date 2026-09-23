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
	"errors"
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
	if err := validateSuppliedPayload(m); err != nil {
		return m, fmt.Errorf("LocalWrite: %w", err)
	}
	m = normalizeLocalMutation(m)

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

	// EntityPrompt dispatch: prompt mutations bypass domain.Decide entirely —
	// they have no topic_key / LWW reconciliation.  applyPromptTx handles
	// insert-if-missing by sync_id and the tombstone resurrection guard.
	// enqueueOutboxTx still runs for BOTH branches so every local write
	// (memory or prompt) is pushed to central on the next sync cycle.
	if m.EntityType == domain.EntityPrompt {
		if err = applyPromptTx(tx, m); err != nil {
			return m, fmt.Errorf("LocalWrite: applyPromptTx: %w", err)
		}
	} else {
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
	}

	// Enqueue the outbox entry regardless of entity type. Prompts must be pushed
	// to central just like memories.  INSERT OR IGNORE on UNIQUE mutation_id
	// makes a duplicate enqueue a no-op (INV5 at the outbox layer).
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
// writes always reflect nil — &"" and nil converge — and ” never reaches any
// index (every partial topic index uses `WHERE topic_key IS NOT NULL`, which is
// complete once ” is normalised away at store entry).
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

// normalizeLocalMutation prepares a newly-created local mutation. When no
// canonical payload was supplied, the sanitized fields are the sole source of
// truth for materialization, payload construction, and mutation-ID derivation.
// Any caller-supplied ID without a payload is therefore replaced by the hash of
// the payload we construct. An externally supplied payload remains immutable.
func normalizeLocalMutation(m domain.Mutation) domain.Mutation {
	if len(m.Payload) == 0 {
		m = mutation.SanitizeTextFields(m)
		m = domain.NormalizeTopicKey(m)
		m.Payload = mutation.CanonicalPayload(m)
		m.MutationID = mutation.NewMutationID(m.Payload)
	}
	return normalizeMutation(m)
}

// validateSuppliedPayload validates, but never rewrites, an externally supplied
// canonical payload. New local mutations have no payload and are sanitized by
// normalizeMutation instead.
func validateSuppliedPayload(m domain.Mutation) error {
	if len(m.Payload) == 0 {
		return nil
	}
	if err := mutation.ValidateCanonicalPayloadText(m.Payload); err != nil {
		return fmt.Errorf("invalid supplied canonical payload: %w", err)
	}
	if err := mutation.ValidateTextFields(m); err != nil {
		return fmt.Errorf("invalid supplied mutation: %w", err)
	}
	return nil
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

// DrainOutbox returns the pending (acked_at IS NULL AND parked_at IS NULL)
// outbox entries in local push order (local_seq ASC), up to limit. limit <= 0
// returns all pending rows.
//
// Each entry's Mutation is reconstructed from the stored canonical Payload via
// mutation.FromCanonicalPayload, then the identity/ordering fields that live in
// the row but not in the payload (MutationID, OccurredAt, Payload) are filled in.
// The entry is ready to push to central exactly as-is.
//
// parked_at IS NULL excludes entries Push has already given up on (see
// ParkMutation): central permanently rejected one, or — the case handled right
// here — this node could never decode its own stored payload (a pre-fix
// NUL-byte row). A row that fails to decode is PARKED with the decode error as
// last_error and skipped, rather than failing the whole call: one bad row must
// not wedge every other project's push behind it forever.
//
// A parked entry also BLOCKS every later unacked entry of the same sync_id
// (entity_key — the key Push groups its ordered version chains by): those rows
// are withheld too (see blockedBehindParkedSQL), because pushing version N+1
// while version N sits rejected would apply the chain out of order. The block is
// lifted by exactly the two operator actions on the parked head: UnparkMutation
// makes the head drainable again (it sorts first, so the chain resumes in order),
// and DiscardMutation acks it (so it no longer counts as an unacked park).
func (s *Store) DrainOutbox(limit int) ([]OutboxEntry, error) {
	q := `
		SELECT s.local_seq, s.entity_key, s.mutation_id, s.payload, s.occurred_at
		FROM sync_mutations s
		WHERE s.acked_at IS NULL AND s.parked_at IS NULL
		  AND NOT ` + blockedBehindParkedSQL + `
		ORDER BY s.local_seq ASC`
	args := []any{}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("DrainOutbox: query: %w", err)
	}

	// undecodable collects rows to park AFTER rows is closed below — the local
	// store's single connection (SetMaxOpenConns(1)) means an UPDATE issued
	// while this SELECT's rows are still open would deadlock waiting for the
	// very connection they are holding.
	type undecodableRow struct {
		localSeq int64
		reason   string
	}
	var out []OutboxEntry
	var undecodable []undecodableRow
	// parkedNow holds the entity_keys of rows parked by THIS call (undecodable):
	// the SQL block above only sees parks that already existed, so any later row
	// of the same chain in this same result set is withheld here instead. Rows
	// arrive in local_seq order, so every later row of the chain follows its head.
	parkedNow := map[string]bool{}

	for rows.Next() {
		var (
			localSeq      int64
			entityKey     string
			mutationID    string
			payload       string
			occurredAtStr string
		)
		if err := rows.Scan(&localSeq, &entityKey, &mutationID, &payload, &occurredAtStr); err != nil {
			rows.Close()
			return nil, fmt.Errorf("DrainOutbox: scan: %w", err)
		}
		if entityKey != "" && parkedNow[entityKey] {
			continue // blocked behind a row this call is about to park
		}

		m, decErr := mutation.FromCanonicalPayload([]byte(payload))
		if decErr != nil {
			undecodable = append(undecodable, undecodableRow{
				localSeq: localSeq,
				reason:   fmt.Sprintf("DrainOutbox: decode payload (mutation_id=%s): %v", mutationID, decErr),
			})
			parkedNow[entityKey] = true
			continue
		}
		m.MutationID = mutationID
		m.Payload = []byte(payload)
		t := parseTime(occurredAtStr)
		if t.IsZero() {
			undecodable = append(undecodable, undecodableRow{
				localSeq: localSeq,
				reason: fmt.Sprintf("DrainOutbox: mutation_id=%s: occurred_at %q is not a valid timestamp",
					mutationID, occurredAtStr),
			})
			parkedNow[entityKey] = true
			continue
		}
		m.OccurredAt = t

		out = append(out, OutboxEntry{LocalSeq: localSeq, Mutation: m})
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return nil, fmt.Errorf("DrainOutbox: rows: %w", rowsErr)
	}

	for _, b := range undecodable {
		if err := s.ParkMutation(b.localSeq, b.reason); err != nil {
			return nil, fmt.Errorf("DrainOutbox: park undecodable row (local_seq=%d): %w", b.localSeq, err)
		}
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

// ── outbox failure tracking / parking (FUP-004) ─────────────────────────────
//
// central can reject a pushed mutation PERMANENTLY (HTTP 400/413/422 —
// malformed, too large, or a deterministic data problem transport.ErrPermanent
// classifies) instead of transiently (5xx, network trouble). Resending a
// permanent rejection every push cycle forever accomplishes nothing but noise
// and wasted round-trips, so the syncer parks that single outbox entry instead:
// DrainOutbox stops returning it, but it stays UNACKED (never silently
// discarded) so an operator can inspect, retry, or explicitly discard it.

// blockedBehindParkedSQL is the predicate (over sync_mutations aliased s) that
// is true when s sits BEHIND a parked entry of its own sync_id chain: an
// unacked, parked row with the same entity_key and a lower local_seq. Such a
// row is withheld from DrainOutbox and counted as blocked rather than pending
// (SyncBacklog, ListParked) until the parked head is retried or discarded. An
// empty entity_key names no chain, so it never blocks anything. Backed by
// idx_sync_mutations_parked_chain, a partial index over exactly the parked,
// unacked rows — normally none, so the probe is a near-free index lookup.
const blockedBehindParkedSQL = `EXISTS (
		SELECT 1 FROM sync_mutations p
		WHERE p.entity_key = s.entity_key AND s.entity_key <> ''
		  AND p.parked_at IS NOT NULL AND p.acked_at IS NULL
		  AND p.local_seq < s.local_seq)`

// ErrMutationNotParked is returned by UnparkMutation and DiscardMutation when
// localSeq does not name a currently-parked, unacked row — either it was never
// parked, it was already un-parked/discarded, or it does not exist.
var ErrMutationNotParked = errors.New("localstore: local_seq is not a parked outbox entry")

// RecordPushFailure stamps a RETRYABLE push failure (a 5xx, a network error,
// or anything the syncer did not classify as permanent): attempts is
// incremented and last_error/last_attempt_at are updated. parked_at is left
// untouched — the entry remains fully pending, and DrainOutbox will return it
// again on the next push cycle. A no-op (not an error) if localSeq is already
// acked or does not exist: this is best-effort visibility, not a correctness
// guard, and must never be the reason a push cycle fails.
func (s *Store) RecordPushFailure(localSeq int64, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(
		`UPDATE sync_mutations
		   SET attempts = attempts + 1, last_error = ?, last_attempt_at = ?
		 WHERE local_seq = ? AND acked_at IS NULL`,
		errMsg, now, localSeq,
	); err != nil {
		return fmt.Errorf("RecordPushFailure(%d): %w", localSeq, err)
	}
	return nil
}

// ParkMutation marks a single outbox entry as permanently rejected: central
// will never accept it as pushed, so the entry must stop being resent, but it
// stays UNACKED (parked, not silently dropped) so `engram sync retry` can
// un-park it later. attempts/last_error/last_attempt_at are stamped the same
// as RecordPushFailure — a park IS a failure, just one the syncer has stopped
// retrying on its own. A no-op (not an error) if localSeq is already acked or
// does not exist, for the same reason as RecordPushFailure.
func (s *Store) ParkMutation(localSeq int64, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(
		`UPDATE sync_mutations
		   SET attempts = attempts + 1, last_error = ?, last_attempt_at = ?, parked_at = ?
		 WHERE local_seq = ? AND acked_at IS NULL`,
		errMsg, now, now, localSeq,
	); err != nil {
		return fmt.Errorf("ParkMutation(%d): %w", localSeq, err)
	}
	return nil
}

// UnparkMutation clears parked_at and resets attempts to 0, making the entry
// eligible for DrainOutbox again on the next push cycle — the `engram sync
// retry` primitive. last_error/last_attempt_at are left as a historical
// record of why it was parked; the next real attempt overwrites them. It also
// releases the later entries of its sync_id chain blocked behind it: they
// drain again right after it, in local_seq order.
// Returns ErrMutationNotParked if localSeq does not currently name a parked,
// unacked row.
func (s *Store) UnparkMutation(localSeq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(
		`UPDATE sync_mutations SET parked_at = NULL, attempts = 0
		 WHERE local_seq = ? AND acked_at IS NULL AND parked_at IS NOT NULL`,
		localSeq,
	)
	if err != nil {
		return fmt.Errorf("UnparkMutation(%d): %w", localSeq, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("UnparkMutation(%d): rows affected: %w", localSeq, err)
	}
	if affected == 0 {
		return fmt.Errorf("UnparkMutation(%d): %w", localSeq, ErrMutationNotParked)
	}
	return nil
}

// DiscardMutation marks a parked entry as acked WITHOUT ever pushing it — the
// operator has decided this mutation's effect is not worth reconciling
// centrally. It sets acked_at, the SAME field a successful push sets, so
// DrainOutbox never returns it again; parked_at (and attempts/last_error) are
// left in place so the row still reads as "discarded, not pushed" in any later
// audit rather than looking like an ordinary successful push. Once acked it no
// longer blocks its sync_id chain, so the later entries behind it drain again.
//
// A hard DELETE was considered and rejected: sync_mutations is this node's
// only durable record that the mutation was ever attempted, and deleting it
// buys nothing central doesn't already guarantee on its own (mutation_id is
// UNIQUE there, so a resurrected local copy could never silently re-apply
// either way) — keeping the row is strictly safer than discarding evidence.
// Returns ErrMutationNotParked if localSeq does not currently name a parked,
// unacked row.
func (s *Store) DiscardMutation(localSeq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.Exec(
		`UPDATE sync_mutations SET acked_at = ?
		 WHERE local_seq = ? AND acked_at IS NULL AND parked_at IS NOT NULL`,
		now, localSeq,
	)
	if err != nil {
		return fmt.Errorf("DiscardMutation(%d): %w", localSeq, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("DiscardMutation(%d): rows affected: %w", localSeq, err)
	}
	if affected == 0 {
		return fmt.Errorf("DiscardMutation(%d): %w", localSeq, ErrMutationNotParked)
	}
	return nil
}

// ParkedEntry is one parked outbox row — visibility-only, for mem_doctor and
// `engram sync parked`.
type ParkedEntry struct {
	LocalSeq   int64
	MutationID string
	Entity     string
	// Project is decoded from the stored payload on a best-effort basis: a row
	// parked because ITS OWN payload could not be decoded (see DrainOutbox)
	// reports "" here rather than failing the whole listing — local_seq and
	// LastError are the useful fields for that case, and both are retry/
	// discard by local_seq, which needs no successful decode.
	Project       string
	Attempts      int
	LastError     string
	LastAttemptAt time.Time
	ParkedAt      time.Time
	// BlockedBehind counts the later, un-parked, unacked entries of this entry's
	// own sync_id chain that DrainOutbox withholds because of it (see
	// blockedBehindParkedSQL) — up to the next parked entry of the chain, if any,
	// so no blocked row is counted under two heads. They go out, in order, once
	// this entry is retried or discarded.
	BlockedBehind int
}

// ListParked returns every parked (never-to-be-resent) outbox entry, oldest
// (lowest local_seq) first.
func (s *Store) ListParked() ([]ParkedEntry, error) {
	rows, err := s.db.Query(`
		SELECT p.local_seq, p.mutation_id, p.entity, p.payload,
		       p.attempts, COALESCE(p.last_error, ''), COALESCE(p.last_attempt_at, ''), COALESCE(p.parked_at, ''),
		       (SELECT COUNT(*) FROM sync_mutations b
		         WHERE b.entity_key = p.entity_key AND p.entity_key <> ''
		           AND b.local_seq > p.local_seq
		           AND b.acked_at IS NULL AND b.parked_at IS NULL
		           AND NOT EXISTS (
		             SELECT 1 FROM sync_mutations q
		             WHERE q.entity_key = p.entity_key
		               AND q.parked_at IS NOT NULL AND q.acked_at IS NULL
		               AND q.local_seq > p.local_seq AND q.local_seq < b.local_seq))
		FROM sync_mutations p
		WHERE p.parked_at IS NOT NULL AND p.acked_at IS NULL
		ORDER BY p.local_seq ASC`)
	if err != nil {
		return nil, fmt.Errorf("ListParked: query: %w", err)
	}
	defer rows.Close()

	var out []ParkedEntry
	for rows.Next() {
		var (
			e                         ParkedEntry
			payload                   string
			lastAttemptRaw, parkedRaw string
		)
		if err := rows.Scan(&e.LocalSeq, &e.MutationID, &e.Entity, &payload,
			&e.Attempts, &e.LastError, &lastAttemptRaw, &parkedRaw, &e.BlockedBehind); err != nil {
			return nil, fmt.Errorf("ListParked: scan: %w", err)
		}
		if m, decErr := mutation.FromCanonicalPayload([]byte(payload)); decErr == nil {
			e.Project = m.Project
		}
		e.LastAttemptAt = parseTime(lastAttemptRaw)
		e.ParkedAt = parseTime(parkedRaw)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListParked: rows: %w", err)
	}
	return out, nil
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
// from memories, memory_tombstones, user_prompts, and prompt_tombstones.  This
// is the set of projects the autosync Loop should pull from central.
//
// The union of all four tables covers every project that has ever had a write
// or a delete applied locally — whether those writes originated locally or were
// pulled from central.  Including user_prompts and prompt_tombstones ensures
// that a node which only captured prompts (no memories) still participates in
// pull for its projects.  The result is sorted alphabetically so the caller
// iterates in a stable order.
func (s *Store) ListProjects() ([]string, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT project FROM memories
		UNION
		SELECT DISTINCT project FROM memory_tombstones
		UNION
		SELECT DISTINCT project FROM user_prompts
		UNION
		SELECT DISTINCT project FROM prompt_tombstones
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
//
// Belt-and-suspenders policy check (PR-②): the steady-state pull filter in
// SyncAllProjects excludes non-synced projects before calling Pull at all, so
// ApplyPulled should never receive a mutation for an omitted project.  This
// check is a defensive guard for the flip-to-omitted race window, where an
// in-flight pull cycle has already fetched mutations from central before the
// policy flip became visible to SyncAllProjects.  That window is bounded by one
// full Pull call, which drains until central returns an empty batch — up to
// syncer.maxPullBatchesPerCall round-trips, not a single fetch — so a flip landing
// mid-drain can be followed by several more batches for the now-omitted project.
// The guard's behaviour is unchanged and remains correct for every one of them:
// the mutation is refused with ErrOmittedProject — an ERROR, not a silent skip —
// so the caller (syncer.Pull) does NOT advance the per-project cursor past it,
// and the error also ends the drain immediately.  If it were silently dropped
// the cursor would move past the mutation and a later flip back to synced would
// never re-pull it: permanent, invisible data loss.  With the error the cursor
// stays put and the flip-back re-pulls the mutation cleanly.
func (s *Store) ApplyPulled(m domain.Mutation) error {
	if err := validateSuppliedPayload(m); err != nil {
		return fmt.Errorf("ApplyPulled: %w", err)
	}
	// Defensive policy check (outside the write lock — GetPolicy is safe for
	// concurrent reads).
	pol, err := s.GetPolicy(m.Project)
	if err != nil {
		return fmt.Errorf("ApplyPulled: policy check for project %q: %w", m.Project, err)
	}
	if pol == PolicyOmitted {
		return fmt.Errorf("ApplyPulled: project %q: %w", m.Project, ErrOmittedProject)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Normalize symmetrically with localWriteLocked: fold &"" → nil topic keys and,
	// ONLY when a field is unset, derive Payload/MutationID/OccurredAt. A well-behaved
	// central store already sends Payload/MutationID/OccurredAt (so for real pulls this
	// just folds the topic key and preserves the central identity); calling the full
	// normalizeMutation closes the latent asymmetry for callers that hand ApplyPulled a
	// bare domain.Mutation.
	m = normalizeMutation(m)

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("ApplyPulled: begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// EntityPrompt dispatch: same branch as localWriteLocked.  Pulled prompt
	// mutations bypass domain.Decide and are materialized via applyPromptTx.
	// No outbox enqueue — pulled mutations must never be re-pushed.
	if m.EntityType == domain.EntityPrompt {
		if err = applyPromptTx(tx, m); err != nil {
			return fmt.Errorf("ApplyPulled: applyPromptTx: %w", err)
		}
	} else {
		d := domain.Decide(&txReader{tx: tx}, m)
		if d.Action != domain.NoOp {
			if err = applyTx(tx, d, m); err != nil {
				return fmt.Errorf("ApplyPulled: applyTx: %w", err)
			}
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("ApplyPulled: commit: %w", err)
	}
	return nil
}
