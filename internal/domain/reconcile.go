// Package domain contains the pure reconciliation logic for engram.
// Decide() and writeWins() are pure functions: they depend only on the Reader
// port and carry no I/O, no database, no side effects.
//
// All six two-writer convergence invariants are encoded here:
//
//	INV1 — topic_key identity convergence (one record per topic)
//	INV2 — monotonic seq (enforced by central BIGSERIAL; pull/cursor ordering only)
//	INV3 — no lost updates (version-guarded LWW)
//	INV4 — no soft-delete resurrection (tombstone check before upsert)
//	INV5 — idempotent re-apply (applied_mutations seen-set)
//	INV6 — independent new writes preserved (distinct sync_ids never conflict)
//
// UNIFORM LWW MODEL — applies to EVERY write (upsert or delete):
//
// The four-level total order  updated_at → version → writer_id → sync_id
// governs ALL conflicts, including delete-vs-live-row. A delete supersedes the
// current LIVE row only if it WINS that order; a stale or tie-losing delete is a
// NoOp against a live row. When there is no live row (cur == nil) the gate is
// skipped and the delete tombstones unconditionally (pure tombstone / cross-writer
// re-tombstone paths are unchanged).
//
// This symmetry is what makes every store converge regardless of push order or
// which writer holds the higher identity: upserts and deletes compete on the
// same inputs, so every store computes the same winner from the same payload
// fields — no central back-channel is needed.
//
// Tiebreaker rationale — identity fields, NOT central seq:
// Central seq is ASYMMETRIC — a node's own authored rows/tombstones keep seq=0
// until the central-assigned seq is pulled back, so the authoring node and
// central would compute different seq-based tie winners → permanent split-brain.
// writer_id and sync_id are derived from the mutation payload and are
// REPLICA-IDENTICAL: every store derives them from the same data. Divergence at
// the exact (updated_at, version) tie is STRUCTURALLY IMPOSSIBLE.
package domain

import "time"

// Decide examines the current state via tx and returns a Decision the adapter
// must execute. It is a pure function: same inputs always produce same output.
//
// Call sequence per design pseudocode:
//  1. INV5 — bail early if mutation_id already applied.
//  2. INV1 — look up existing record by topic_key (canonical identity).
//  3. INV6 — fall back to sync_id lookup (no-topic writes keyed by sync_id).
//  4. INV4 — check tombstone before allowing upsert.
//  5. Cross-writer convergence — when no live/sync row resolved but a tombstone
//     exists for the topic identity, recover the canonical sync_id from the
//     tombstone so deletes and revives address the EXISTING identity instead of
//     minting a new one.
//  6. Dispatch on Op.
//
// The returned Decision carries TargetSyncID (the row the adapter must address,
// which may differ from m.SyncID when resolved via topic_key) and Undelete
// (true when the adapter must clear deleted_at and remove the tombstone row).
//
// CROSS-WRITER STATE SPACE (the hardening this function enforces):
//
// When a topic's canonical row is SOFT-DELETED under sync_id Y and a new mutation
// arrives under a DIFFERENT sync_id X, neither FindByTopic (live-only, skips Y)
// nor FindBySyncID(X) (misses Y) resolves the canonical identity. Resolving Y from
// the tombstone (ts.SyncID) prevents two failure modes:
//
//   • Re-delete: a second delete would INSERT a duplicate tombstone (PK = X),
//     leaving two tombstones for one topic and making FindTombstone-by-topic
//     (LIMIT 1, no ORDER BY) non-deterministic. Fix: re-tombstone Y.
//   • Upsert-after-delete: a superseding upsert would INSERT a new row X and clear
//     Y's tombstone, orphaning the dead row Y (no tombstone) — Y could later revive
//     into a SECOND live row (INV1 violation). Fix: revive Y in place when a row for
//     Y exists; only insert when the tombstone is "pure" (no row ever existed).
//
// Structural invariants enforced across the whole state space:
//
//	INV-A: at most ONE live row per (topic_key, project, scope).
//	INV-B: at most ONE tombstone per topic identity.
func Decide(tx Reader, m Mutation) Decision {
	noop := Decision{Action: NoOp, TargetSyncID: m.SyncID}

	// INV5: idempotent re-apply guard.
	if applied, err := tx.MutationApplied(m.MutationID); err == nil && applied {
		return noop
	}

	// INV1 / INV6: resolve current record.
	var cur *Record
	if m.TopicKey != nil && *m.TopicKey != "" {
		cur, _ = tx.FindByTopic(*m.TopicKey, m.Project, m.Scope) // INV1 identity
	}
	if cur == nil {
		cur, _ = tx.FindBySyncID(m.SyncID) // INV6 no-topic writes
	}

	// INV4: tombstone check — must happen BEFORE processing an upsert.
	// ts is HOISTED out of the guard so the switch below can recover the canonical
	// identity from it when cur == nil (cross-writer convergence).
	//
	// Tiebreaker: when updated_at and version are both equal, writeWins falls
	// through to (writer_id, then sync_id) — both stable, replica-identical fields
	// so every store computes the same winner (see package-level comment).
	// ts.DeletedBy is the tombstone writer_id; ts.SyncID is the tombstone identity.
	var ts *Tombstone
	if t, err := tx.FindTombstone(m.SyncID, m.TopicKey, m.Project, m.Scope); err == nil {
		ts = t
	}
	tombstoneSuperseded := false
	if ts != nil {
		if m.Op == OpUpsert && !writeWins(m, ts.DeletedAt, ts.Version, ts.DeletedBy, ts.SyncID) {
			return noop // tombstoned and the incoming write is not newer
		}
		// writeWins against the tombstone: adapter must clear it on supersede.
		tombstoneSuperseded = true
	}

	switch m.Op {
	case OpDelete:
		// A delete competes on the SAME total order as an upsert
		// (updated_at → version → writer_id → sync_id). It supersedes the current
		// LIVE row only if it WINS that order; a stale or tie-losing delete against
		// a live row is a NoOp. This is what makes deletes symmetric with the pull
		// side and with upsert-vs-upsert, so every store converges regardless of
		// push order or which writer holds the higher identity.
		//
		// When cur == nil (no live row) the gate is skipped — the delete still
		// tombstones unconditionally: either it is the first delete of this identity
		// (pure tombstone) or the canonical row was already soft-deleted by a prior
		// writer and the INV-B re-tombstone hardening below is unchanged.
		if cur != nil && !writeWins(m, cur.UpdatedAt, cur.Version, cur.WriterID, cur.SyncID) {
			return noop
		}

		// INV4 / INV-B: write tombstone atomically (adapter executes this).
		//
		// TargetSyncID must address the CANONICAL identity so the adapter's
		// INSERT OR REPLACE re-tombstones the same row instead of minting a second
		// tombstone. Resolution priority:
		//   1. cur.SyncID      — a live (or sync_id-resolved) row was found.
		//   2. ts.SyncID       — no row resolved but a tombstone exists for this
		//                        topic identity (the canonical row was already
		//                        soft-deleted under Y; re-tombstone Y).
		//   3. m.SyncID        — first delete of an otherwise-unknown identity
		//                        (pure tombstone for m's own sync_id).
		target := m.SyncID
		switch {
		case cur != nil:
			target = cur.SyncID
		case ts != nil:
			target = ts.SyncID
		}
		return Decision{Action: ActionWriteTombstone, TargetSyncID: target}

	case OpUpsert:
		if cur == nil {
			// No live row and no sync_id-resolved row. If a tombstone for this
			// topic identity was superseded, the canonical row may still exist as a
			// SOFT-DELETED row under ts.SyncID (a different writer's identity). In
			// that case we must REVIVE that row in place — inserting m.SyncID would
			// orphan the dead row and risk a second live row later (INV-A breach).
			if tombstoneSuperseded && ts != nil && ts.SyncID != m.SyncID {
				if prior, _ := tx.FindBySyncID(ts.SyncID); prior != nil {
					// Canonical row exists (soft-deleted) under Y — revive it.
					return Decision{
						Action:       ActionUpdate,
						TargetSyncID: ts.SyncID,
						Undelete:     true,
					}
				}
				// Pure tombstone (no row ever existed for Y): fall through to insert
				// m's own identity and clear the stale tombstone.
			}
			// INV1, INV6: first write for this identity — insert.
			// Undelete is true when a tombstone for this identity was superseded
			// (the record was soft-deleted; adapter must clear it to make it live).
			return Decision{
				Action:       ActionInsert,
				TargetSyncID: m.SyncID,
				Undelete:     tombstoneSuperseded,
			}
		}
		// INV3: version-guarded LWW — newer write wins.
		if writeWins(m, cur.UpdatedAt, cur.Version, cur.WriterID, cur.SyncID) {
			// INV1: update converges to one row.
			// TargetSyncID is the RESOLVED row's sync_id (may differ from m.SyncID
			// when resolved via FindByTopic — the P1-a convergence fix).
			// Undelete is true when the resolved row is currently soft-deleted OR
			// a tombstone for this identity was superseded.
			return Decision{
				Action:       ActionUpdate,
				TargetSyncID: cur.SyncID,
				Undelete:     tombstoneSuperseded || (cur.DeletedAt != nil),
			}
		}
		return noop // INV3: older write discarded

	default:
		return noop
	}
}

// writeWins reports whether the incoming mutation should overwrite the current
// stored state. Priority order (clock-skew-proof, symmetric/convergent):
//
//  1. updated_at (wall-clock) — primary comparator.
//  2. version    — monotonic counter; tiebreaker when timestamps equal.
//  3. writer_id  — stable, replica-identical string; final tiebreaker on the
//     exact (updated_at, version) tie. Higher string wins (arbitrary but consistent).
//  4. sync_id    — stable, content-addressed identity; resolved only when writer_id
//     is also equal. Higher string wins; full equality returns false (deterministic
//     no-op).
//
// Why identity fields and not central seq: central seq is ASYMMETRIC — a node's
// own authored rows/tombstones keep seq=0 (AckMutation never back-patches it;
// self-authored mutations pulled back are INV5 NoOps), so the author and central
// compute different tie winners → permanent split-brain. writer_id and sync_id are
// derived from the mutation payload and are REPLICA-IDENTICAL: every store computes
// the same winner from the same inputs, making split-brain at the tie structurally
// impossible with no central back-channel required.
func writeWins(m Mutation, curUpdatedAt time.Time, curVersion int, curWriterID, curSyncID string) bool {
	if !m.UpdatedAt.Equal(curUpdatedAt) {
		return m.UpdatedAt.After(curUpdatedAt)
	}
	if m.Version != curVersion {
		return m.Version > curVersion
	}
	// Final tiebreaker: deterministic on STABLE, replica-identical fields (writer_id
	// then sync_id) so EVERY store computes the same winner from the same inputs.
	// This makes the exact (updated_at,version) tie converge with NO central seq
	// back-channel. "Higher wins" is arbitrary but consistent; full equality returns
	// false (deterministic no-op).
	if m.WriterID != curWriterID {
		return m.WriterID > curWriterID
	}
	return m.SyncID > curSyncID
}

// String returns a human-readable label for the Action constant.
// Used in test failure messages.
func (a Action) String() string {
	switch a {
	case NoOp:
		return "NoOp"
	case ActionInsert:
		return "Insert"
	case ActionUpdate:
		return "Update"
	case ActionWriteTombstone:
		return "WriteTombstone"
	default:
		return "Unknown"
	}
}
