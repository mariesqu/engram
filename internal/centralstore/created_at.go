package centralstore

import (
	"context"
	"fmt"

	"github.com/mariesqu/engram/internal/domain"
)

// createdAtDefaultLimit / createdAtMaxLimit mirror cloudserve's own pull
// clamping (pullDefaultLimit/pullMaxLimit): a caller that sends limit <= 0
// gets the default page size, and no caller may ask for more than the max in
// one round-trip regardless of what it requests.
const (
	createdAtDefaultLimit = 500
	createdAtMaxLimit     = 2000
)

// OriginalCreatedAt returns one PAGE of (sync_id, earliest occurred_at) pairs
// for project, ordered by sync_id ascending, starting strictly after the
// after cursor ("" for the first page). limit is clamped to
// [1, createdAtMaxLimit], defaulting to createdAtDefaultLimit when <= 0 —
// the same clamping discipline PullSince/collectPullBatch already apply.
//
// MIN(occurred_at) GROUP BY entity_key is the FUP-005 answer to "when was
// this memory originally written": central_mutations is the append-only
// journal every push (from every writer, across this identity's whole
// version history) is recorded in, so the earliest occurred_at for a sync_id
// is the true original creation time — independent of which writer's push
// happened to be the one a given node pulled first, and independent of
// execInsert's own created_at (which only reflects what THAT node observed).
//
// An empty result (nil, nil) signals the project is fully paged — the
// client's backfill loop stops there, mirroring PullSince's own
// empty-batch-means-drained contract.
//
// Returns []domain.CreatedAtEntry, not a syncwire type: this mirrors
// PullSince/Apply, which exchange domain.Mutation rather than
// syncwire.WireMutation — the wire DTO belongs at the actual HTTP boundary
// (cloudserve's handler converts this to syncwire.CreatedAtEntry for JSON).
func (s *Store) OriginalCreatedAt(ctx context.Context, project, after string, limit int) ([]domain.CreatedAtEntry, error) {
	switch {
	case limit <= 0:
		limit = createdAtDefaultLimit
	case limit > createdAtMaxLimit:
		limit = createdAtMaxLimit
	}

	rows, err := s.pool.Query(ctx, `
		SELECT entity_key, MIN(occurred_at)
		FROM central_mutations
		WHERE project = $1 AND entity_key > $2
		GROUP BY entity_key
		ORDER BY entity_key
		LIMIT $3`,
		project, after, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("centralstore.OriginalCreatedAt(%q): query: %w", project, err)
	}
	defer rows.Close()

	var out []domain.CreatedAtEntry
	for rows.Next() {
		var e domain.CreatedAtEntry
		if err := rows.Scan(&e.SyncID, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("centralstore.OriginalCreatedAt(%q): scan: %w", project, err)
		}
		e.CreatedAt = e.CreatedAt.UTC()
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("centralstore.OriginalCreatedAt(%q): rows: %w", project, err)
	}
	return out, nil
}
