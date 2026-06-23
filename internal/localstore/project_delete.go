package localstore

import "fmt"

// PurgeProjectLocal hard-deletes all local data for the named project in one
// transaction and then sets the project's policy to omitted so sync will not
// re-pull the project in the future.
//
// Tables cleared (in dependency order to avoid FK-like issues):
//   - memories          (the FTS delete trigger fires automatically on each DELETE)
//   - user_prompts
//   - memory_tombstones
//   - prompt_tombstones
//   - sync_mutations    (outbox — pending pushes for the project)
//
// sync_state and pull_cursors are NOT project-scoped (sync_state is a single
// global row; pull_cursors rows are identified by (target_key, project) but
// there is no harm in leaving them — they become stale and are simply never
// advanced again once the policy is set to omitted).
//
// The FTS virtual table (memories_fts) is kept in sync by the mem_fts_delete
// trigger that fires on every DELETE FROM memories row. Do NOT touch it directly.
//
// s.mu is held for the duration of the transaction to serialise this write with
// other writers (LocalWrite, SetPolicy, etc.) — consistent with the pattern in
// write_queue.go and other mutating paths.
//
// Returns the total number of rows deleted across all tables.
func (s *Store) PurgeProjectLocal(project string) (int, error) {
	project = normalizeProject(project)
	// Guard: a whitespace-only argument normalizes to "" and would otherwise
	// silently purge the empty-project bucket. Refuse it (the caller must name a
	// real project) — destructive ops must not act on an accidental blank.
	if project == "" {
		return 0, fmt.Errorf("PurgeProjectLocal: project must not be empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var total int64

	// 1. memories — FTS delete trigger fires automatically on each deleted row.
	res, err := tx.Exec(`DELETE FROM memories WHERE project = ?`, project)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	total += n

	// 2. user_prompts
	res, err = tx.Exec(`DELETE FROM user_prompts WHERE project = ?`, project)
	if err != nil {
		return 0, err
	}
	n, _ = res.RowsAffected()
	total += n

	// 3. memory_tombstones
	res, err = tx.Exec(`DELETE FROM memory_tombstones WHERE project = ?`, project)
	if err != nil {
		return 0, err
	}
	n, _ = res.RowsAffected()
	total += n

	// 4. prompt_tombstones
	res, err = tx.Exec(`DELETE FROM prompt_tombstones WHERE project = ?`, project)
	if err != nil {
		return 0, err
	}
	n, _ = res.RowsAffected()
	total += n

	// 5. sync_mutations outbox — uses entity_key which is the sync_id, not project.
	//    The project is stored in the payload JSON, but filtering by entity is
	//    expensive. The sync_mutations table stores the writer_id but NOT the project
	//    directly. We use a JSON extraction: payload->'project'. However, to keep
	//    the implementation simple and avoid JSON parsing complexity in SQLite,
	//    we rely on the fact that sync_mutations stores entity_key = sync_id, and
	//    each sync_id's project is in the memories/tombstones tables which we just
	//    deleted. Any pending mutations for deleted sync_ids will naturally fail
	//    on push (record no longer exists) and be ignored.
	//
	//    However, the spec says to clear outbox rows for the project. The payload
	//    column is TEXT JSON. SQLite's json_extract can filter by project:
	res, err = tx.Exec(
		`DELETE FROM sync_mutations WHERE json_extract(payload, '$.project') = ?`,
		project,
	)
	if err != nil {
		return 0, err
	}
	n, _ = res.RowsAffected()
	total += n

	// 6. Set project policy to omitted inside the same transaction so the operation
	//    is atomic: if anything above failed, the policy is not changed either.
	_, err = tx.Exec(
		`INSERT INTO project_policy (project, policy, updated_at)
		 VALUES (?, 'omitted', datetime('now'))
		 ON CONFLICT(project) DO UPDATE SET
		     policy     = 'omitted',
		     updated_at = datetime('now')`,
		project,
	)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	// Invalidate the policy cache so the next GetPolicy read reflects 'omitted'.
	s.policyMu.Lock()
	delete(s.policyCache, project)
	s.policyMu.Unlock()

	return int(total), nil
}

// TombstoneProject soft-deletes every live memory in the named project by
// calling the existing DeleteMemory method for each one. DeleteMemory writes
// an OpDelete mutation through LocalWrite → outbox, so the deletions propagate
// to all synced nodes via the normal push/pull cycle.
//
// s.mu is NOT held around DeleteMemory — DeleteMemory acquires it internally.
// Holding it here would deadlock.
//
// PARTIAL PROGRESS: each memory is tombstoned in its own transaction (DeleteMemory
// is atomic per row), so this operation is NOT all-or-nothing. On an error part-way
// through it returns the count of memories successfully tombstoned SO FAR together
// with the error. It is safe to re-run: already-tombstoned rows are no longer live
// and won't be re-selected, so a re-run completes the remainder.
//
// Returns the number of memories that were tombstoned.
func (s *Store) TombstoneProject(project, writerID string) (int, error) {
	project = normalizeProject(project)
	// Guard: refuse a blank/whitespace-only project (see PurgeProjectLocal).
	if project == "" {
		return 0, fmt.Errorf("TombstoneProject: project must not be empty")
	}

	// Select all live memory IDs for this project in a single read.
	rows, err := s.db.Query(
		`SELECT id FROM memories WHERE project = ? AND deleted_at IS NULL`,
		project,
	)
	if err != nil {
		return 0, err
	}

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// Soft-delete each one via DeleteMemory (acquires s.mu internally).
	count := 0
	for _, id := range ids {
		if err := s.DeleteMemory(id, writerID); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
