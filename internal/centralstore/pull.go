package centralstore

import (
	"context"
	"fmt"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/mutation"
)

// defaultPullLimit is the maximum number of mutations returned by PullSince
// when the caller passes limit <= 0.
const defaultPullLimit = 1000

// PullSince returns mutations that were inserted into central_mutations for the
// given project after sinceSeq (exclusive), in strictly ascending seq order.
//
// Each row's canonical payload is decoded back into a domain.Mutation via
// mutation.FromCanonicalPayload; the caller-facing fields that are NOT part of
// the canonical payload (MutationID, Seq, OccurredAt, Payload) are filled from
// the central_mutations row.
//
// The returned slice is safe to replay in order: seq[i+1] > seq[i] always holds.
//
// Guard: if limit <= 0 the function uses defaultPullLimit (1000). The caller
// MUST NOT assume an empty return means there are no more rows; it means
// seq > sinceSeq yielded 0 rows at this moment.
func (s *Store) PullSince(ctx context.Context, project string, sinceSeq int64, limit int) ([]domain.Mutation, error) {
	return pullSinceQ(ctx, s.pool, project, sinceSeq, limit)
}

// pullSinceQ is the ctx+querier core for PullSince. It runs on any querier
// (pool or tx) so callers that already hold a transaction can reuse the same
// SQL without acquiring a second connection.
func pullSinceQ(ctx context.Context, q querier, project string, sinceSeq int64, limit int) ([]domain.Mutation, error) {
	if limit <= 0 {
		limit = defaultPullLimit
	}

	const sql = `
		SELECT seq, mutation_id, payload, occurred_at
		FROM   central_mutations
		WHERE  project = $1
		  AND  seq     > $2
		ORDER BY seq ASC
		LIMIT $3`

	rows, err := q.Query(ctx, sql, project, sinceSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("PullSince: query: %w", err)
	}
	defer rows.Close()

	var out []domain.Mutation
	for rows.Next() {
		var (
			seq        int64
			mutationID string
			payload    []byte
			occurredAt time.Time
		)
		if err := rows.Scan(&seq, &mutationID, &payload, &occurredAt); err != nil {
			return nil, fmt.Errorf("PullSince: scan row: %w", err)
		}

		m, err := mutation.FromCanonicalPayload(payload)
		if err != nil {
			return nil, fmt.Errorf("PullSince: decode payload for seq=%d mutation_id=%s: %w", seq, mutationID, err)
		}

		// Fill in the fields that live in central_mutations but are NOT encoded
		// in the canonical payload (they are assigned by the central store, not
		// derived from the mutation content).
		m.MutationID = mutationID
		m.Seq = seq
		m.OccurredAt = occurredAt.UTC()
		m.Payload = payload

		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("PullSince: rows: %w", err)
	}

	// Defensive: pgx returns rows in SELECT order (seq ASC), but assert the
	// contract explicitly so callers can rely on it.
	for i := 1; i < len(out); i++ {
		if out[i].Seq <= out[i-1].Seq {
			return nil, fmt.Errorf("PullSince: seq not strictly ascending at index %d (%d <= %d) — central invariant violated", i, out[i].Seq, out[i-1].Seq)
		}
	}

	return out, nil
}

