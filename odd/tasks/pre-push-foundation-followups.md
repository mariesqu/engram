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
- [x] **FUP-002 — Hook session fallback only for empty cwd** — Route: delegated writer
- [x] **FUP-003 — Duplicate-install hook protection** — Route: delegated writer
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
  Commit: 6539c20.

- FUP-002 done (delegated writer). `hookProject` (cmd/engram/hook.go) fell back
  to the session's registered project whenever `hookResolveProject` returned ""
  — including a cwd that was PRESENT but REFUSED (relative, missing directory,
  ambiguous monorepo parent), silently overriding that refusal with a guess.
  Fixed: the fallback now only fires when `strings.TrimSpace(in.CWD) == ""`;
  a refused-but-present cwd returns "" (no save) instead of falling through.
  Doc comment rewritten to state the invariant explicitly.
  Tests added: `cmd/engram/hook_test.go`
  `TestHookSubagentStop_RefusedCwdDoesNotFallBackToSessionsProject` (two
  subtests: relative cwd "." and a missing-directory path, both against a
  session pre-registered with a real project — asserts zero saves). No
  existing assertion was changed; the existing
  `TestHookSubagentStop_FallsBackToTheSessionsProject` already covers the
  empty-cwd-still-falls-back case and continues to pass unchanged.
  Verification: `go build ./...`: ok. `go vet ./...`: ok.
  `go test ./cmd/... ./internal/... -count=1`: all packages ok except the three
  known environmental failures.
  Commit: af20172.

- FUP-003 done (delegated writer), both layers.
  (a) Runtime dedup (cmd/engram/hook.go): a new `hookClaimOccurrence(sessionID,
  event, payload)` claims an exclusive-create marker keyed on (session id hash,
  event, hash of the occurrence payload — the prompt text or the subagent
  closing message), reusing the existing `hookClaimState` O_CREATE|O_EXCL
  primitive the first-prompt bootstrap already uses. Wired into
  `hookUserPromptSubmit` and `hookSubagentStop`, immediately before their
  respective `mem_save_prompt`/`mem_save` calls, so a claim that never reaches
  the save (unresolved project) never burns the marker. Empty session id always
  claims (no dedup possible without a session to key on — matches the existing
  bootstrap-marker carve-out) and any non-ErrExist claim failure is fail-open
  (inherited unchanged from `hookClaimState`). `hookClearState` now also
  globs-and-removes a session's occurrence markers (`hookClearOccurrenceMarkers`,
  since they have no fixed name), and a cheap cooldown-gated TTL sweep
  (`hookSweepStaleOccurrenceMarkers`, 24h TTL, checked at most once per hour)
  backstops sessions that never reach session-end. Refactored the session-id
  hash out of `hookStateFile` into shared `hookSessionHashHex` so the two
  marker-naming paths cannot drift.
  (b) Setup warning (cmd/engram/setup.go): `warnIfPluginAlsoInstalled` reads the
  claude-code settings.json's `enabledPlugins` key (conservatively — only a
  `map[string]bool` or a `[]string` shape naming "engram"; any other shape is
  silently skipped) and prints a stderr warning before the merge writes.
  README.md's "Installing" section now states the exclusivity next to the
  setup-hooks instructions regardless of detection.
  Tests added: `cmd/engram/hook_test.go`
  (`TestHookUserPromptSubmit_DuplicateDeliverySavesPromptOnce`,
  `TestHookUserPromptSubmit_DifferentPromptsBothSaved`,
  `TestHookSubagentStop_DuplicateDeliverySavesReportOnce`,
  `TestHookSubagentStop_DifferentReportsBothSaved`,
  `TestHookClaimOccurrence_EmptySessionNeverDedupes`,
  `TestHookClaimState_NonExistErrorFailsOpen`,
  `TestHookSessionEnd_ClearsOccurrenceMarkers`,
  `TestHookSweepStaleOccurrenceMarkers_RemovesOnlyStaleOnes`);
  `cmd/engram/setup_test.go` (`TestPluginEntryMentionsEngram`,
  `TestWarnIfPluginAlsoInstalled_WarnsOnlyWhenDetectable`,
  `TestSetupHooks_WarnsWhenPluginAlreadyEnabled`). No existing assertion was
  changed. The claim-dir-failure test exercises the shared `hookClaimState`
  primitive directly (a NUL byte in the path — invalid on every OS, rejected by
  the os package before any syscall) rather than through a real permission
  failure, which is not portably reproducible on Windows.
  Verification: `go build ./...`: ok. `go vet ./...`: ok.
  `go test ./cmd/... ./internal/... -count=1`: all packages ok except the three
  known environmental failures.
  Commit: pending (recorded after commit).

## Next Step

FUP-001..003 are closed. FUP-004/005 (outbox permanent-rejection handling,
created_at on the wire) remain pending design/exploration of
internal/syncer, internal/syncwire, internal/localstore/sync.go, apply.go and
internal/centralstore — explicitly out of scope for this writer, which was
read-only-restricted from those files.
