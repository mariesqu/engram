# Pre-Push Foundation Follow-ups

## Objective

Close the remaining review findings on `feat/upstream-parity` before the branch is pushed.

## Problem

- Project-scoped queries filter with `LOWER(project) = ?`, which cannot use `idx_mem_project`; `mem_context` and `RecentSessions` scan the table.
- `hookProject` falls back to the session's project whenever cwd resolution returns "", including a present-but-refused cwd, bypassing the refusal.
- Installing the Claude Code plugin and `engram setup hooks` together runs every hook twice (duplicate prompt and subagent saves).
- Outbox entries rejected permanently by the server (e.g. pre-fix NUL payloads) block the sync queue forever.
- `created_at` is not carried on the wire, so pulled rows get local arrival time; review_after backfill, date filters and session ordering are wrong on pulled machines.

## Scope

- FUP-001 index `LOWER(project)` and push project filters into derived tables.
- FUP-002 hook session fallback only when cwd is empty.
- FUP-003 duplicate-install protection for hooks.
- FUP-004 permanent-rejection handling for the outbox, plus repair of unacked NUL entries.
- FUP-005 original creation time carried through sync.

## Constraints

- No push or pull request.
- Conventional commits, no AI attribution trailers.
- Sync changes must stay compatible with older peers and the existing cloud server in both directions.
- Additive, idempotent schema migrations only.

## Configuration

- TDD: off; no project or session TDD setting was found
- Test runner: `go test`
- Verification: `go build ./...`, `go vet ./...`, `go test ./cmd/... ./internal/... -count=1`; known environmental failures: TestRun_DaemonMissingDB, TestRun_DaemonCentralURLMissingWriterID, TestRun_DaemonCentralURLMissingWriterKey
- RDD: off (global)
- Delivery strategy: exception-ok (appended to the unpushed branch)

## Tasks

- [x] **FUP-001 — Index LOWER(project)** — Route: delegated writer
- [ ] **FUP-002 — Hook session fallback only for empty cwd** — Route: delegated writer
- [ ] **FUP-003 — Duplicate-install hook protection** — Route: delegated writer
- [ ] **FUP-004 — Outbox permanent-rejection handling** — Route: pending design (explorer mapping sync)
- [ ] **FUP-005 — created_at on the wire** — Route: pending design (explorer mapping sync)

## Progress

- Created after pre-push-parity-fixes closed.
- FUP-001 done (delegated writer). Added schema v15: expression indexes
  `idx_mem_project_lower ON memories(LOWER(project))` and
  `idx_sessions_project_lower ON sessions(LOWER(project))`, installed through
  ApplySchema, migrateV14ToV15, and rebuildMemoriesTable's drop/recreate list (so
  a legacy v0 DB doesn't silently lose the new memories index on rebuild — the
  v0-rebuild regression test derives its expected set from a fresh store, so it
  picked the new index up with no test change needed). Also moved RecentSessions'
  project filter into the derived table (`lm`) so the grouped newest-memory-per-
  session pass is scoped to one project's memories instead of the whole table.
  Tests added: `internal/localstore/project_lower_index_test.go`
  (`TestCountPinned_UsesProjectLowerIndex`,
  `TestRecentObservations_UsesProjectLowerIndex`,
  `TestRecentSessions_UsesProjectLowerIndexes`,
  `TestRecentSessions_DerivedTableScopedToProject`). No existing assertions were
  changed. Verification: `go build ./...`: ok. `go vet ./...`: ok.
  `go test ./cmd/... ./internal/... -count=1`: all packages ok except the three
  known environmental failures (TestRun_DaemonMissingDB,
  TestRun_DaemonCentralURLMissingWriterID, TestRun_DaemonCentralURLMissingWriterKey).
  Commit: pending (recorded after commit).

## Next Step

FUP-002, then FUP-003, each as its own commit; parallel read-only sync mapping
for FUP-004/005 stays out of scope for this writer.
