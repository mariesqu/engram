package localstore

import "fmt"

// CountLiveByProject returns the number of live (non-deleted) memory rows for the
// named project (normalized). Used by the `projects consolidate` dry-run to
// report how many memories WOULD be renamed before --yes executes the merge.
func (s *Store) CountLiveByProject(project string) (int, error) {
	project = normalizeProject(project)
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE project = ? AND deleted_at IS NULL`,
		project,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("CountLiveByProject(%q): %w", project, err)
	}
	return n, nil
}

// MergeProject renames every local row belonging to project `from` so it lives
// under project `to`, in ONE transaction. It is a LOCAL-ONLY rename (proposal
// phase A): it does NOT enqueue outbox entries and does NOT propagate to central
// — each node merges independently. memories rows simply have their project
// column rewritten; their sync_ids and versions are untouched.
//
// Tables touched:
//   - memories      → UPDATE project = to WHERE project = from
//   - project_policy → keyed by project (PK). If a `to` row already exists, keep
//     it and delete the `from` row (target policy wins); else rename in place.
//   - pull_cursors  → keyed by (target_key, project). For each `from` cursor, if
//     a matching (target_key, to) row exists keep the target and drop the source;
//     else rename in place. Renaming first would raise a UNIQUE/PK conflict where
//     a target row already exists, so the conflicting source rows are deleted
//     up-front and the survivors renamed.
//
// from/to are normalized (lowercased/trimmed) like all project handling. An
// empty from/to or from == to (after normalization) is rejected.
//
// Returns the number of memories, project_policy, and pull_cursor rows moved
// (renamed or deduped) for the source project. The policy cache is invalidated
// for both names so the next GetPolicy read is correct.
func (s *Store) MergeProject(from, to string) (memories, policies, cursors int, err error) {
	from = normalizeProject(from)
	to = normalizeProject(to)
	if from == "" || to == "" {
		return 0, 0, 0, fmt.Errorf("MergeProject: from and to must both be non-empty")
	}
	if from == to {
		return 0, 0, 0, fmt.Errorf("MergeProject: from and to must differ (both %q after normalization)", from)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	// 0. Topic-key collisions. A live source memory whose (topic_key, scope) already
	//    has a LIVE row in the TARGET project would, after the rename, leave TWO live
	//    rows for the same (topic_key, project, scope) — breaking the one-live-row
	//    topic invariant (memories has only a NON-unique topic index, so nothing in
	//    the DB catches it and FindByTopic's LIMIT 1 would resolve arbitrarily).
	//    Soft-delete the SOURCE row in that case — target wins, mirroring the
	//    policy/cursor dedup. Soft-delete via deleted_at is FTS-safe (the
	//    mem_fts_update trigger drops the now-deleted row from the index). This MUST
	//    run BEFORE the rename so the EXISTS check still sees target rows under `to`.
	if _, err = tx.Exec(
		`UPDATE memories SET deleted_at = datetime('now')
		 WHERE project = ?
		   AND deleted_at IS NULL
		   AND topic_key IS NOT NULL AND topic_key <> ''
		   AND EXISTS (
		       SELECT 1 FROM memories t
		       WHERE t.project = ?
		         AND t.deleted_at IS NULL
		         AND t.topic_key = memories.topic_key
		         AND t.scope = memories.scope
		   )`,
		from, to,
	); err != nil {
		return 0, 0, 0, fmt.Errorf("MergeProject: topic-collision dedup: %w", err)
	}

	// 1. memories — rename the survivors. The FTS update trigger fires per row and
	//    re-indexes under the new project automatically. Soft-deleted collision rows
	//    are moved too (as inert tombstones), fully emptying the source project.
	res, err := tx.Exec(`UPDATE memories SET project = ? WHERE project = ?`, to, from)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("MergeProject: memories: %w", err)
	}
	movedMemories, _ := res.RowsAffected()
	memories = int(movedMemories)

	// 2. project_policy — PK is `project`. Delete the source row when a target row
	//    already exists (target policy wins), otherwise rename it.
	res, err = tx.Exec(
		`DELETE FROM project_policy
		 WHERE project = ? AND EXISTS (SELECT 1 FROM project_policy WHERE project = ?)`,
		from, to,
	)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("MergeProject: project_policy dedup: %w", err)
	}
	dedupedPolicies, _ := res.RowsAffected()

	res, err = tx.Exec(`UPDATE project_policy SET project = ? WHERE project = ?`, to, from)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("MergeProject: project_policy rename: %w", err)
	}
	renamedPolicies, _ := res.RowsAffected()
	policies = int(dedupedPolicies + renamedPolicies)

	// 3. pull_cursors — PK is (target_key, project). Delete each source cursor for
	//    which a target cursor with the same target_key already exists, then rename
	//    the survivors. The target cursor wins (it reflects the canonical project's
	//    pull position; keeping the source could replay or skip seqs).
	res, err = tx.Exec(
		`DELETE FROM pull_cursors
		 WHERE project = ?
		   AND EXISTS (
		       SELECT 1 FROM pull_cursors t
		       WHERE t.project = ? AND t.target_key = pull_cursors.target_key
		   )`,
		from, to,
	)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("MergeProject: pull_cursors dedup: %w", err)
	}
	dedupedCursors, _ := res.RowsAffected()

	res, err = tx.Exec(`UPDATE pull_cursors SET project = ? WHERE project = ?`, to, from)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("MergeProject: pull_cursors rename: %w", err)
	}
	renamedCursors, _ := res.RowsAffected()
	cursors = int(dedupedCursors + renamedCursors)

	if err = tx.Commit(); err != nil {
		return 0, 0, 0, fmt.Errorf("MergeProject: commit: %w", err)
	}

	// Invalidate the policy cache for both names so the next read is correct.
	s.policyMu.Lock()
	delete(s.policyCache, from)
	delete(s.policyCache, to)
	s.policyMu.Unlock()

	return memories, policies, cursors, nil
}
