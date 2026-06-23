package centralstore

import (
	"context"
	"fmt"
)

// DeleteProject hard-deletes all central data for the named project across the
// five central tables in one pgx transaction. This is an ADMIN-only operation
// that bypasses tombstone propagation: the deletion does NOT write tombstones and
// therefore does NOT propagate to other synced nodes.
//
// Tables cleared (project is a bound parameter — never interpolated into SQL):
//   - central_mutations       (the push-journal entries for the project)
//   - central_memories        (the materialized memory rows)
//   - central_tombstones      (the delete records for the project)
//   - central_user_prompts    (the captured prompts for the project)
//   - central_prompt_tombstones (the prompt delete records)
//
// Returns the total RowsAffected across all five tables.
// Returns an error if project is empty (rejected before any DB contact).
func (s *Store) DeleteProject(ctx context.Context, project string) (int64, error) {
	if project == "" {
		return 0, fmt.Errorf("centralstore.DeleteProject: project must not be empty")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("centralstore.DeleteProject: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	var total int64

	// 1. central_mutations — the push journal.
	tag, err := tx.Exec(ctx,
		`DELETE FROM central_mutations WHERE project = $1`, project)
	if err != nil {
		return 0, fmt.Errorf("centralstore.DeleteProject: delete central_mutations: %w", err)
	}
	total += tag.RowsAffected()

	// 2. central_memories — the materialized memory rows.
	tag, err = tx.Exec(ctx,
		`DELETE FROM central_memories WHERE project = $1`, project)
	if err != nil {
		return 0, fmt.Errorf("centralstore.DeleteProject: delete central_memories: %w", err)
	}
	total += tag.RowsAffected()

	// 3. central_tombstones — soft-delete records for the project.
	tag, err = tx.Exec(ctx,
		`DELETE FROM central_tombstones WHERE project = $1`, project)
	if err != nil {
		return 0, fmt.Errorf("centralstore.DeleteProject: delete central_tombstones: %w", err)
	}
	total += tag.RowsAffected()

	// 4. central_user_prompts — captured prompts for the project.
	tag, err = tx.Exec(ctx,
		`DELETE FROM central_user_prompts WHERE project = $1`, project)
	if err != nil {
		return 0, fmt.Errorf("centralstore.DeleteProject: delete central_user_prompts: %w", err)
	}
	total += tag.RowsAffected()

	// 5. central_prompt_tombstones — prompt delete records for the project.
	tag, err = tx.Exec(ctx,
		`DELETE FROM central_prompt_tombstones WHERE project = $1`, project)
	if err != nil {
		return 0, fmt.Errorf("centralstore.DeleteProject: delete central_prompt_tombstones: %w", err)
	}
	total += tag.RowsAffected()

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("centralstore.DeleteProject: commit: %w", err)
	}

	return total, nil
}
