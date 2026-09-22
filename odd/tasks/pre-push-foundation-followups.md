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
- [x] **FUP-003b — Dedup only near-simultaneous deliveries** — Route: delegated writer
- [x] **FUP-004a — Server: 422 for permanent Apply errors** — Route: delegated writer
- [ ] **FUP-004b — Client: park permanently rejected outbox entries** — Route: delegated writer
- [ ] **FUP-004c — Client: repair unacked NUL mutations** — Route: delegated writer
- [ ] **FUP-004d — Visibility: doctor check + CLI for parked mutations** — Route: delegated writer
- [ ] **FUP-005a — Client: stamp created_at from occurred_at** — Route: delegated writer
- [ ] **FUP-005b — Server: serve original creation times** — Route: delegated writer
- [ ] **FUP-005c — Client: backfill created_at once per project** — Route: delegated writer

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
  Commit: 597e123.

- FUP-003b done (delegated writer). The occurrence marker in hookClaimOccurrence
  blocked a repeat of the identical (session, event, payload) key for its full
  24h TTL, so a user resending the same text later in a session (a retyped
  "continue") lost the second save, not just the double-install race it was
  built for. Added hookOccurrenceDedupWindow (30s): hookClaimOccurrence now
  checks the existing marker's age via hookStateAge — inside the window it
  still blocks (near-simultaneous duplicate delivery), past it the marker is
  refreshed via hookTouchState and the save proceeds (a new occurrence that
  happens to hash the same).
  Tests added: `cmd/engram/hook_test.go`
  `TestHookUserPromptSubmit_DedupWindowExpiryStillSaves` (end-to-end: two
  identical runHook calls, marker backdated past the window between them, both
  save), `TestHookClaimOccurrence_DedupWindowBoundary` (unit: blocks
  immediately, allows past the window, re-blocks immediately after the
  refresh). No existing assertion changed.
  Verification: `go build ./...`: ok. `go vet ./...`: ok.
  `go test ./cmd/... ./internal/... -count=1`: all packages ok except the three
  known environmental failures.
  Commit: dd8068d.

- FUP-004a done (delegated writer). Added `transport.ErrPermanent` sentinel
  (internal/transport/errors.go), mirroring ErrResponseTooLarge's pattern:
  implementations wrap it with %w for a mutation Apply rejects for a
  DETERMINISTIC reason (our own validation, or a Postgres data exception /
  constraint violation, SQLSTATE class 22/23), building the message ONLY from
  structured identifiers (constraint/column name, SQLSTATE) — never
  pgErr.Message/.Detail/.Hint, which Postgres can fill with the actual
  offending row value. `centralstore.Apply` (apply.go) now wraps: the two
  existing Go-side validation errors directly (already-safe text), and any
  DB error via a new `permanentDataError` helper that classifies SQLSTATE class
  22/23 (excluding 23505/unique_violation, which isUniqueViolation already
  treats as an idempotent no-op earlier in the same function).
  `cloudserve.handlePush` now checks `errors.Is(err, transport.ErrPermanent)`
  before the generic 500 branch and returns 422 with `err.Error()` verbatim
  (safe by the sentinel's contract).
  Tests added: `internal/centralstore/apply_internal_test.go`
  `TestPermanentDataError` (fake pgconn.PgError, no Postgres — covers class
  22/23/08, the 23505 carve-out, wrapped-error unwrapping, and asserts the
  constructed message never contains the fixture's raw Message/Detail/Hint
  text); `internal/cloudserve/server_test.go`
  `TestHandlePush_PermanentApplyError_Returns422`. No existing assertion
  changed; `TestHandlePush_ApplyError_Returns500` (plain error, not wrapped)
  still asserts 500, confirming the two paths stay distinct.
  Verification: `go build ./...`: ok. `go vet ./...`: ok.
  `go test ./cmd/... ./internal/... -count=1` (ENGRAM_DSN unset): all packages
  ok except the three known environmental failures in cmd/engram.
  centralstore/cloudserve non-acceptance unit tests ran (no Postgres/DSN
  needed); acceptance-tagged Postgres tests were not run (see report).
  Commit: pending (recorded after commit).

## Next Step

FUP-004b (client: park permanently rejected outbox entries — schema v16 +
sync_mutations attempts/last_error/parked_at + Push/DrainOutbox changes),
then FUP-004c (NUL repair), FUP-004d (doctor + CLI), then FUP-005a/b/c
(created_at on the wire).
