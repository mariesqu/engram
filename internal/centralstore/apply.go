package centralstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mariesqu/engram/internal/domain"
)

// Apply is the central (push-apply) reconciliation: it takes a single mutation
// pushed by a client, assigns the authoritative central seq, runs the SAME pure
// domain.Decide() that the local pull-apply uses, and materializes the resulting
// Decision into central_memories / central_tombstones — all inside ONE Postgres
// transaction.
//
// This is the central twin of localstore.Apply. The shapes mirror each other on
// purpose: Decide produces the Decision; the adapter dispatches on
// Decision.Action using Decision.TargetSyncID and Decision.Undelete.
//
// Flow:
//
//  1. INV5 idempotency — check MutationApplied(m.MutationID) on the pool FIRST.
//     A re-pushed mutation that already landed is a cheap no-op: return nil
//     without opening a transaction.
//  2. Begin a transaction. Every subsequent read and write runs on this tx so
//     Decide sees a consistent snapshot and the whole reconciliation is atomic.
//  3. Assign seq — InsertMutation on the tx returns the BIGSERIAL seq (INV2,
//     the canonical monotonic order for pull cursors / journal ordering). The seq
//     is the central_mutations.seq journal value; it is NOT stored in
//     central_memories (the materialized-row copy was removed). It is passed to
//     applyDecision for signature compatibility only.
//     central_mutations.mutation_id is UNIQUE: a concurrent duplicate push that
//     races past step 1 surfaces here as SQLSTATE 23505 — treated as an
//     idempotent no-op (rollback, return nil). This UNIQUE is the durable
//     applied-marker; there is no separate applied_mutations table centrally.
//  4. Decide — run domain.Decide against the tx. The mutation was just inserted
//     in step 3, so a naive central Reader.MutationApplied would now report it
//     applied and Decide would NoOp every push. We pass Decide a thin Reader
//     (decideReader) whose MutationApplied always returns false; idempotency is
//     already guaranteed by step 1 plus the InsertMutation UNIQUE. Decide's
//     other reads (FindByTopic / FindBySyncID / FindTombstone) run on the SAME
//     tx so they observe a consistent snapshot.
//  5. Materialize the Decision on the tx (mirrors localstore.Apply):
//     - ActionInsert / ActionUpdate → UpsertMemory(TargetSyncID). When
//       Decision.Undelete is set, the upsert already clears deleted_at on the
//       row (the ON CONFLICT SET deleted_at = NULL), and we additionally remove
//       the central_tombstones row so the revived record is fully live.
//     - ActionWriteTombstone → WriteTombstone(TargetSyncID) AND set
//       central_memories.deleted_at on the target row. The central WriteTombstone
//       primitive writes only the tombstone row, so the deleted_at flag is set
//       here — matching localstore.execWriteTombstone, which does both.
//     - NoOp → nothing.
//  6. Commit. Any error rolls back the whole transaction.
//
// LWW tiebreaker: the final (updated_at, version) tie is resolved by
// (writer_id, then the WINNING mutation's content-addressed mutation_id carried
// by last_write_mutation_id) — replica-identical payload-derived fields so every
// store computes the same winner. NOT the canonical PK sync_id (which is
// divergent across replicas for the same topic). Central seq is NOT used as a
// tiebreaker; it serves only as the pull-cursor / journal ordering authority
// (see writeWins in domain/reconcile.go).
func (s *Store) Apply(ctx context.Context, m domain.Mutation) error {
	// Normalize TopicKey at store entry: fold &"" → nil so '' never reaches any
	// central index. Every partial topic index uses `WHERE topic_key IS NOT NULL`,
	// which is the complete no-topic exclusion once '' is normalised away here.
	// Note: Apply does not recompute Payload/MutationID; localstore.LocalWrite is
	// responsible for normalizing before CanonicalPayload/NewMutationID. This is
	// defense-in-depth to keep '' out of central topic indexes for imperfect callers.
	m = domain.NormalizeTopicKey(m)

	// Step 1 — INV5 idempotency: a mutation that already landed is a no-op.
	// Checked on the pool before opening a transaction so re-pushes are cheap.
	if applied, err := s.MutationApplied(m.MutationID); err != nil {
		return fmt.Errorf("Apply: idempotency check: %w", err)
	} else if applied {
		return nil
	}

	// Step 2 — begin the reconciliation transaction.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("Apply: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	// Step 3 — assign the authoritative central seq. The UNIQUE(mutation_id) on
	// central_mutations is the durable applied-marker; a concurrent duplicate push
	// that races past step 1 hits 23505 here and is treated as an idempotent no-op.
	// seq is the BIGSERIAL journal seq (central_mutations.seq). It is passed to
	// applyDecision for compatibility but is no longer stored in central_memories.
	seq, err := insertMutationQ(ctx, tx, m)
	if err != nil {
		if isUniqueViolation(err) {
			// Duplicate mutation_id — another push already recorded it. Roll back
			// (defer) and report success: the effect is already (or concurrently)
			// applied. INV5 holds even under a race.
			return nil
		}
		return fmt.Errorf("Apply: insert mutation: %w", err)
	}

	// Step 4 — run the pure Decide against this tx's snapshot. decideReader forces
	// MutationApplied=false so the just-inserted mutation does not make Decide NoOp.
	d := domain.Decide(&decideReader{ctx: ctx, q: tx}, m)

	// Step 5 — materialize the Decision atomically on the tx.
	if err = applyDecision(ctx, tx, d, m, seq); err != nil {
		return err
	}

	// Step 6 — commit.
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("Apply: commit: %w", err)
	}
	committed = true
	return nil
}

// applyDecision dispatches the Decision onto the central tables using the tx.
// It mirrors localstore.Apply's switch so the local and central adapters stay
// structurally identical (same Decision contract, same dispatch).
func applyDecision(ctx context.Context, tx pgx.Tx, d domain.Decision, m domain.Mutation, seq int64) error {
	switch d.Action {
	case domain.NoOp:
		// Stored state already correct (INV3 older write discarded, or INV4
		// tombstoned stale write blocked). The mutation row + seq are still
		// recorded — that is the applied-marker and the authoritative order.
		return nil

	case domain.ActionInsert, domain.ActionUpdate:
		// Both insert and update funnel through the same upsert primitive keyed by
		// the resolved TargetSyncID (INV1 convergence: cross-writer updates address
		// the canonical row Y, not the incoming sync_id X). The upsert's
		// ON CONFLICT (sync_id) DO UPDATE ... SET deleted_at = NULL already revives
		// a soft-deleted row, so Undelete needs no extra deleted_at clear here.
		if err := upsertMemoryQ(ctx, tx, d.TargetSyncID, m, seq); err != nil {
			return err
		}
		if d.Undelete {
			// Remove the tombstone row so the revived record is fully live and
			// FindByTopic / FindTombstone no longer see it as deleted. Mirrors
			// localstore.execClearTombstone.
			if err := clearTombstoneQ(ctx, tx, d.TargetSyncID, m); err != nil {
				return err
			}
		}
		return nil

	case domain.ActionWriteTombstone:
		// Write the tombstone row keyed by the resolved TargetSyncID (cross-writer
		// deletes re-tombstone the canonical identity Y, avoiding a second tombstone
		// under X and a central_tombstones_topic_uidx violation).
		if err := writeTombstoneQ(ctx, tx, d.TargetSyncID, m); err != nil {
			return err
		}
		// The central WriteTombstone primitive only writes the tombstone row, so we
		// set deleted_at on the canonical memory row here — mirroring
		// localstore.execWriteTombstone, which soft-deletes the row AND tombstones it.
		if err := setDeletedAtQ(ctx, tx, d.TargetSyncID, m); err != nil {
			return err
		}
		return nil

	default:
		return fmt.Errorf("Apply: unknown action %d", d.Action)
	}
}

// setDeletedAtQ soft-deletes the canonical memory row identified by targetSyncID,
// stamping the deletion's version, writer, and the winning delete's content-
// addressed mutation_id (last_write_mutation_id) so the row reflects the tombstone.
// The version/writer_id/mutation_id come from the DELETE mutation (the
// authoritative deletion context), matching localstore.execWriteTombstone's
// UPDATE of the memories row. last_write_mutation_id MUST be stamped here so the
// soft-deleted row carries the same final-tiebreaker value as the tombstone — a
// later delete-vs-live-row comparison reads cur.LastWriteMutationID.
//
// The WHERE clause is intentionally unconditional on deleted_at: if the row is
// already soft-deleted (cross-writer re-delete), this refreshes the deletion
// metadata, which is harmless and keeps the row and tombstone consistent.
func setDeletedAtQ(ctx context.Context, qr querier, targetSyncID string, m domain.Mutation) error {
	const sql = `
		UPDATE central_memories
		SET deleted_at = $1, version = $2, writer_id = $3, last_write_mutation_id = $4
		WHERE sync_id = $5`
	if _, err := qr.Exec(ctx, sql, m.UpdatedAt.UTC(), m.Version, m.WriterID, m.MutationID, targetSyncID); err != nil {
		return fmt.Errorf("setDeletedAt: %w", err)
	}
	return nil
}

// clearTombstoneQ removes the tombstone for a revived record so it is fully live
// again. It deletes by sync_id (primary identity) and, when a topic_key is
// present, also by (topic_key, project, scope) to clear any stale topic-keyed
// tombstone. Mirrors localstore.execClearTombstone.
func clearTombstoneQ(ctx context.Context, qr querier, targetSyncID string, m domain.Mutation) error {
	if _, err := qr.Exec(ctx,
		`DELETE FROM central_tombstones WHERE sync_id = $1`,
		targetSyncID,
	); err != nil {
		return fmt.Errorf("clearTombstone (by sync_id): %w", err)
	}
	if m.TopicKey != nil && *m.TopicKey != "" {
		if _, err := qr.Exec(ctx,
			`DELETE FROM central_tombstones WHERE topic_key = $1 AND project = $2 AND scope = $3`,
			*m.TopicKey, m.Project, m.Scope,
		); err != nil {
			return fmt.Errorf("clearTombstone (by topic_key): %w", err)
		}
	}
	return nil
}

// decideReader adapts an in-flight transaction to domain.Reader for the duration
// of one Apply. Its FindByTopic / FindBySyncID / FindTombstone read the tx so
// Decide sees the same snapshot the writes will commit against.
//
// MutationApplied ALWAYS returns false: Apply has already inserted the mutation
// row (step 3) to claim the seq, so a truthful MutationApplied would make Decide
// short-circuit to NoOp on every push. Idempotency is enforced by Apply's step-1
// pool check plus the central_mutations.mutation_id UNIQUE constraint — not by
// Decide's INV5 branch at push time.
type decideReader struct {
	ctx context.Context
	q   querier
}

func (r *decideReader) FindByTopic(topicKey, project, scope string) (*domain.Record, error) {
	return findByTopicQ(r.ctx, r.q, topicKey, project, scope)
}

func (r *decideReader) FindBySyncID(syncID string) (*domain.Record, error) {
	return findBySyncIDQ(r.ctx, r.q, syncID)
}

func (r *decideReader) FindTombstone(syncID string, topicKey *string, project, scope string) (*domain.Tombstone, error) {
	return findTombstoneQ(r.ctx, r.q, syncID, topicKey, project, scope)
}

// MutationApplied always reports false. See decideReader's doc comment.
func (r *decideReader) MutationApplied(string) (bool, error) {
	return false, nil
}

// isUniqueViolation reports whether err is a Postgres unique_violation (SQLSTATE
// 23505). Used to treat a racing duplicate central_mutations insert as an
// idempotent no-op.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
