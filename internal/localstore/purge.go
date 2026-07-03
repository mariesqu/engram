package localstore

// purge.go implements the LOCAL side of the remote-purge feature: a per-writer
// generation counter (purge_epoch) lives on the central cloud_writer_keys row.
// On daemon startup, if central's epoch for this writer is ahead of the epoch
// this node last honored, the node purges its local SYNCED data (never
// local-only or omitted projects — see PurgeForResync) and lets the normal
// autosync pull loop re-populate it from central.
//
// honored_purge_epoch is persisted in daemon_meta (a generic single-row-per-key
// metadata table, schema v12) so it survives daemon restarts: without durable
// storage, EVERY startup would see remote_epoch > 0 (honored defaults to the
// Go zero value 0) and re-purge forever.

import (
	"database/sql"
	"fmt"
	"strconv"
)

// honoredPurgeEpochKey is the daemon_meta row key that stores the highest
// central purge_epoch this node has already purged for.
const honoredPurgeEpochKey = "honored_purge_epoch"

// HonoredPurgeEpoch returns the highest central purge_epoch this node has
// already honored (purged local SYNCED data for). A fresh store — or a store
// that has never seen a purge — returns 0, meaning "no purge honored yet."
func (s *Store) HonoredPurgeEpoch() (int64, error) {
	var raw string
	err := s.db.QueryRow(
		`SELECT value FROM daemon_meta WHERE key = ?`, honoredPurgeEpochKey,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("HonoredPurgeEpoch: %w", err)
	}
	epoch, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("HonoredPurgeEpoch: stored value %q is not a valid integer: %w", raw, err)
	}
	return epoch, nil
}

// SetHonoredPurgeEpoch persists newEpoch as the highest honored purge_epoch.
// It is exported standalone (in addition to being folded into PurgeForResync's
// single transaction) so callers/tests can set it independently of a purge —
// e.g. to seed a store's baseline epoch without running a destructive purge.
func (s *Store) SetHonoredPurgeEpoch(newEpoch int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("SetHonoredPurgeEpoch: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	if err := setHonoredPurgeEpochTx(tx, newEpoch); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("SetHonoredPurgeEpoch: commit: %w", err)
	}
	return nil
}

// setHonoredPurgeEpochTx is the tx-scoped core shared by SetHonoredPurgeEpoch
// and PurgeForResync, so the epoch write can be folded into the SAME
// transaction as the destructive purge (atomicity: a crash between "data
// purged" and "epoch recorded" must be impossible, or a restart would either
// re-purge harmlessly — acceptable — or, worse, record the epoch without
// having purged — NOT acceptable, which is why this is the LAST statement in
// PurgeForResync's transaction, after all deletes).
func setHonoredPurgeEpochTx(tx *sql.Tx, newEpoch int64) error {
	_, err := tx.Exec(
		`INSERT INTO daemon_meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		honoredPurgeEpochKey, strconv.FormatInt(newEpoch, 10),
	)
	if err != nil {
		return fmt.Errorf("setHonoredPurgeEpochTx: %w", err)
	}
	return nil
}

// PurgeResult reports the per-table row counts deleted by PurgeForResync, plus
// the set of projects that were purged (policy == synced at purge time) and
// the new honored epoch recorded. It is intended for startup logging.
type PurgeResult struct {
	Projects           []string
	MemoriesDeleted    int64
	UserPromptsDeleted int64
	MemoryTombstones   int64
	PromptTombstones   int64
	SyncMutations      int64
	AppliedMutations   int64
	PullCursorsDeleted int64
	NewHonoredEpoch    int64
}

// PurgeForResync hard-deletes all LOCAL data for every project whose policy is
// PolicySynced, resets those projects' pull cursors so the next sync cycle
// re-pulls everything from seq 0, and records newEpoch as the honored purge
// epoch — all in ONE SQLite transaction.
//
// Scope — what is and is NOT touched:
//   - Projects with policy 'synced' (INCLUDING projects with no explicit
//     project_policy row, whose EFFECTIVE default is 'synced' whenever central
//     is configured — the same default GetPolicy/ListProjectsWithPolicy compute,
//     and the SAME filter syncer.SyncAllProjects uses to decide which projects
//     to pull): memories, user_prompts, memory_tombstones, prompt_tombstones,
//     sync_mutations (outbox), and this node's applied_mutations records for
//     those projects are hard-deleted, and their pull_cursors rows are removed.
//   - Projects with policy 'local-only' or 'omitted' are NEVER touched — their
//     data was never in central (local-only) or was deliberately excluded
//     (omitted), so deleting it would be UNRECOVERABLE data loss with no
//     server-side copy to re-pull from. This is the central safety invariant
//     of the whole remote-purge feature.
//   - project_policy rows are NEVER modified (unlike PurgeProjectLocal, which
//     sets the project's policy to 'omitted'). Setting it to omitted here would
//     make sync discovery permanently skip the project — the opposite of what a
//     resync needs: the whole point is for the NEXT sync cycle to rediscover and
//     re-pull the project from central. See project_delete.go's PurgeProjectLocal
//     doc comment for the operation this deliberately does NOT mirror.
//
// The applied_mutations decision (defense-in-depth reasoning, read before
// modifying):
//
// applied_mutations is keyed by mutation_id ONLY (see schema.go) — it carries
// no project column and no payload, so once the memories/tombstones rows for a
// purged project are deleted, there is NO SURVIVING JOIN KEY that lets this
// function derive "which mutation_ids belonged to the purged projects." Two
// options were considered:
//
//  1. Derive project membership from sync_mutations.payload (which DOES carry
//     project via JSON) before deleting sync_mutations, and delete only the
//     matching applied_mutations rows. This is INCOMPLETE: sync_mutations only
//     contains entries this node itself pushed (or attempted to push) —
//     mutations pulled FROM central and applied via ApplyPulled are recorded in
//     applied_mutations but NEVER touch sync_mutations (ApplyPulled explicitly
//     does not enqueue). A purged project's applied_mutations rows are
//     overwhelmingly PULLED mutations, so this approach would miss almost all
//     of them.
//  2. Clear the ENTIRE applied_mutations table. This is what PurgeForResync
//     does. It is SAFE for every project in scope:
//     - For purged (synced) projects: this is required. If any applied_mutations
//     row for a purged project survived, the re-pull's ApplyPulled call would
//     see MutationApplied(mutation_id) == true (INV5 idempotency guard) and
//     silently NO-OP every re-pulled mutation — the purged project would come
//     back from central as an EMPTY project forever. This is the exact bug the
//     task spec calls out.
//     - For local-only/omitted projects: those projects' local data is left
//     untouched by this function, and they do not participate in pull (the
//     policy filter in SyncAllProjects excludes them), so clearing their
//     applied_mutations rows has NO OBSERVABLE EFFECT — those rows exist only
//     to make an already-impossible future pull idempotent. There is nothing to
//     re-apply, so there is nothing to protect against re-applying.
//     Net effect: clearing the whole table is equivalent in observable behavior
//     to clearing only the purged projects' rows, but is derivable and correct
//     TODAY given the schema, whereas the "surgical" approach is provably
//     incomplete. This is documented here so a future reader who tightens
//     applied_mutations to be project-scoped can revisit this decision.
//
// Returns the per-table counts (for startup logging) and any error. On error
// the transaction is rolled back — no partial purge, no epoch recorded.
func (s *Store) PurgeForResync(newEpoch int64) (PurgeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Enumerate candidate projects the SAME way the autosync pull loop does:
	// ListProjects()'s memories ∪ memory_tombstones ∪ user_prompts ∪
	// prompt_tombstones union (sync.go). This is deliberately NOT
	// ListProjectsWithPolicy(), whose name discovery only sources from live
	// (non-deleted) memories ∪ project_policy — a synced-by-default project
	// that exists ONLY via soft-deleted memories and/or user_prompts/
	// tombstones (no memories row, no explicit project_policy row) would be
	// silently invisible to that query and skipped by a purge, even though the
	// pull loop still tracks (and re-pulls into) it. Using ListProjects() here
	// guarantees every project the pull loop can touch is also a purge
	// candidate. Each candidate's effective policy is then resolved with
	// GetPolicy (the same policy-resolution semantics — including the
	// central-aware default-to-synced rule — that ListProjectsWithPolicy and
	// syncer.SyncAllProjects rely on).
	//
	// Both ListProjects() and GetPolicy() MUST run BEFORE s.db.Begin() below:
	// the store's *sql.DB is configured with SetMaxOpenConns(1) (see store.go),
	// so once a transaction holds the single connection, ANY other query
	// against s.db — including these calls' own s.db.Query/QueryRow — blocks
	// forever waiting for a connection that will never free up until the
	// transaction commits or rolls back. Resolving names and policies first
	// (still under s.mu, which excludes a concurrent SetPolicy from
	// interleaving) avoids that self-deadlock entirely. GetPolicy has its own
	// policyCache guarded by policyMu — calling it here, pre-Begin, is safe and
	// populates/reads that cache exactly as any other caller would.
	names, err := s.ListProjects()
	if err != nil {
		return PurgeResult{}, fmt.Errorf("PurgeForResync: list projects: %w", err)
	}
	var projects []ProjectPolicy
	for _, name := range names {
		pol, err := s.GetPolicy(name)
		if err != nil {
			return PurgeResult{}, fmt.Errorf("PurgeForResync: get policy for %q: %w", name, err)
		}
		projects = append(projects, ProjectPolicy{Name: name, Policy: pol})
	}

	tx, err := s.db.Begin()
	if err != nil {
		return PurgeResult{}, fmt.Errorf("PurgeForResync: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var result PurgeResult
	for _, pp := range projects {
		if pp.Policy != PolicySynced {
			continue // local-only / omitted: never touched by a remote purge
		}
		result.Projects = append(result.Projects, pp.Name)

		n, err := purgeProjectRowsTx(tx, pp.Name)
		if err != nil {
			return PurgeResult{}, fmt.Errorf("PurgeForResync: purge project %q: %w", pp.Name, err)
		}
		result.MemoriesDeleted += n.memories
		result.UserPromptsDeleted += n.userPrompts
		result.MemoryTombstones += n.memoryTombstones
		result.PromptTombstones += n.promptTombstones
		result.SyncMutations += n.syncMutations

		cn, err := tx.Exec(
			`DELETE FROM pull_cursors WHERE target_key = ? AND project = ? COLLATE NOCASE`,
			defaultTargetKey, pp.Name,
		)
		if err != nil {
			return PurgeResult{}, fmt.Errorf("PurgeForResync: reset pull cursor for %q: %w", pp.Name, err)
		}
		affected, _ := cn.RowsAffected()
		result.PullCursorsDeleted += affected
	}

	// See the applied_mutations doc block above for why clearing the whole
	// table (rather than a per-project delete) is the correct, and only
	// derivable, choice given the current schema.
	if len(result.Projects) > 0 {
		am, err := tx.Exec(`DELETE FROM applied_mutations`)
		if err != nil {
			return PurgeResult{}, fmt.Errorf("PurgeForResync: clear applied_mutations: %w", err)
		}
		affected, _ := am.RowsAffected()
		result.AppliedMutations = affected
	}

	if err := setHonoredPurgeEpochTx(tx, newEpoch); err != nil {
		return PurgeResult{}, fmt.Errorf("PurgeForResync: %w", err)
	}
	result.NewHonoredEpoch = newEpoch

	if err := tx.Commit(); err != nil {
		return PurgeResult{}, fmt.Errorf("PurgeForResync: commit: %w", err)
	}

	// Invalidate the policy cache defensively: PurgeForResync never writes
	// project_policy, so no entries actually go stale, but clearing it here
	// costs nothing and guards against a future edit that DOES touch policy
	// rows inside this transaction forgetting to invalidate the cache.
	s.policyMu.Lock()
	s.policyCache = make(map[string]Policy)
	s.policyMu.Unlock()

	return result, nil
}

// projectRowCounts holds the per-table deleted-row counts for one project,
// returned by purgeProjectRowsTx.
type projectRowCounts struct {
	memories         int64
	userPrompts      int64
	memoryTombstones int64
	promptTombstones int64
	syncMutations    int64
}

// purgeProjectRowsTx hard-deletes project's rows from the five tables a
// synced project can have data in, matching case-insensitively exactly like
// PurgeProjectLocal (central-pulled rows keep their original case; local
// writes lowercase the project). It does NOT touch project_policy or
// pull_cursors — callers handle those explicitly (PurgeForResync resets the
// cursor but deliberately leaves policy untouched, unlike PurgeProjectLocal).
func purgeProjectRowsTx(tx *sql.Tx, project string) (projectRowCounts, error) {
	var n projectRowCounts

	res, err := tx.Exec(`DELETE FROM memories WHERE project = ? COLLATE NOCASE`, project)
	if err != nil {
		return n, fmt.Errorf("delete memories: %w", err)
	}
	n.memories, _ = res.RowsAffected()

	res, err = tx.Exec(`DELETE FROM user_prompts WHERE project = ? COLLATE NOCASE`, project)
	if err != nil {
		return n, fmt.Errorf("delete user_prompts: %w", err)
	}
	n.userPrompts, _ = res.RowsAffected()

	res, err = tx.Exec(`DELETE FROM memory_tombstones WHERE project = ? COLLATE NOCASE`, project)
	if err != nil {
		return n, fmt.Errorf("delete memory_tombstones: %w", err)
	}
	n.memoryTombstones, _ = res.RowsAffected()

	res, err = tx.Exec(`DELETE FROM prompt_tombstones WHERE project = ? COLLATE NOCASE`, project)
	if err != nil {
		return n, fmt.Errorf("delete prompt_tombstones: %w", err)
	}
	n.promptTombstones, _ = res.RowsAffected()

	// sync_mutations has no project column directly — the project lives inside
	// the JSON payload, exactly as PurgeProjectLocal handles it.
	res, err = tx.Exec(
		`DELETE FROM sync_mutations WHERE json_extract(payload, '$.project') = ? COLLATE NOCASE`,
		project,
	)
	if err != nil {
		return n, fmt.Errorf("delete sync_mutations: %w", err)
	}
	n.syncMutations, _ = res.RowsAffected()

	return n, nil
}
