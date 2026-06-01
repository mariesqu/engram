// Package domain contains the pure reconciliation logic for engram.
// Decide() and writeWins() are pure functions: they depend only on the Reader
// port and carry no I/O, no database, no side effects.
//
// All six two-writer convergence invariants are encoded here:
//   INV1 — topic_key identity convergence (one record per topic)
//   INV2 — monotonic seq (enforced by central BIGSERIAL; respected here by seq tiebreaker)
//   INV3 — no lost updates (version-guarded LWW)
//   INV4 — no soft-delete resurrection (tombstone check before upsert)
//   INV5 — idempotent re-apply (applied_mutations seen-set)
//   INV6 — independent new writes preserved (distinct sync_ids never conflict)
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
//  5. Dispatch on Op.
//
// The returned Decision carries TargetSyncID (the row the adapter must address,
// which may differ from m.SyncID when resolved via topic_key) and Undelete
// (true when the adapter must clear deleted_at and remove the tombstone row).
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
	tombstoneSuperseded := false
	if ts, err := tx.FindTombstone(m.SyncID, m.TopicKey, m.Project, m.Scope); err == nil && ts != nil {
		if m.Op == OpUpsert && !writeWins(m, ts.DeletedAt, ts.Version, 0) {
			return noop // tombstoned and the incoming write is not newer
		}
		// writeWins against the tombstone: adapter must clear it on supersede.
		tombstoneSuperseded = true
	}

	switch m.Op {
	case OpDelete:
		// INV4: write tombstone atomically (adapter executes this).
		// Use the RESOLVED row's sync_id when a live row was found via FindByTopic
		// (cross-writer convergence: the row may have been stored under a different
		// sync_id Y than the incoming delete's sync_id Z). This mirrors the P1-a
		// fix for ActionUpdate so the adapter always addresses the correct row.
		target := m.SyncID
		if cur != nil {
			target = cur.SyncID
		}
		return Decision{Action: ActionWriteTombstone, TargetSyncID: target}

	case OpUpsert:
		if cur == nil {
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
		if writeWins(m, cur.UpdatedAt, cur.Version, cur.Seq) {
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
// stored state. Priority order (per design decision 3 — clock-skew-proof):
//  1. updated_at (wall-clock) — primary comparator.
//  2. version    — monotonic counter; tiebreaker when timestamps equal.
//  3. seq        — server-assigned BIGSERIAL; final tiebreaker (INV2).
//
// Returns false on full equality (deterministic no-op when all dimensions match).
func writeWins(m Mutation, curUpdatedAt time.Time, curVersion int, curSeq int64) bool {
	if !m.UpdatedAt.Equal(curUpdatedAt) {
		return m.UpdatedAt.After(curUpdatedAt)
	}
	if m.Version != curVersion {
		return m.Version > curVersion
	}
	return m.Seq > curSeq
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
