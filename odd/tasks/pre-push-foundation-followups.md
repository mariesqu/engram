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
- [x] **FUP-004b — Client: park permanently rejected outbox entries** — Route: delegated writer
- [x] **FUP-004c — Client: repair unacked NUL mutations** — Route: delegated writer
- [x] **FUP-004d — Visibility: doctor check + CLI for parked mutations** — Route: delegated writer
- [x] **FUP-005a — Client: stamp created_at from occurred_at** — Route: delegated writer
- [x] **FUP-005b — Server: serve original creation times** — Route: delegated writer
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

- FUP-004b done (delegated writer). Schema v16 adds sync_mutations
  attempts/last_error/last_attempt_at/parked_at (ALTER, guarded by
  columnExists like v2→v3) plus idx_sync_mutations_drain ON
  (acked_at, parked_at, local_seq). Gotcha: ApplySchema runs on every Open,
  BEFORE runMigrations, and CREATE TABLE IF NOT EXISTS no-ops against an
  EXISTING legacy sync_mutations table — an unconditional CREATE INDEX
  naming parked_at in ApplySchema's stmts list therefore failed
  ("no such column: parked_at") against the v0/v4 legacy-DB test fixtures the
  instant this binary opened them, since the column does not exist until
  migrateV15ToV16 runs afterward. Fixed by moving the index creation out of
  the unconditional stmts list into a columnExists-guarded call after the
  loop (skips on a legacy DB; migrateV15ToV16 creates the identical
  idxSyncMutationsDrainDDL once the column exists).
  localstore/sync.go: new Store.ParkMutation/RecordPushFailure/
  UnparkMutation/DiscardMutation/ListParked + ParkedEntry +
  ErrMutationNotParked. DrainOutbox now filters `parked_at IS NULL` and, on a
  decode failure, parks that one row (AFTER closing the SELECT's rows —
  SetMaxOpenConns(1) would deadlock an UPDATE issued while rows are still
  open) instead of failing the whole call.
  syncer/syncer.go: Push classifies each Apply failure via isParkableRejection
  (400/413/422 → ParkMutation, stop only that sync_id's group, do not cancel
  siblings) vs. everything else (RecordPushFailure, propagate — cancels
  siblings as before). SyncAllProjects now classifies a non-nil push error via
  isFatalPushError (401/403): fatal skips pull entirely (unchanged from
  before); every other push failure (parked or retryable) is folded into the
  existing errs aggregate and pull proceeds regardless — the literal "push
  failure must no longer skip pull" requirement.
  Tests added: `internal/localstore/outbox_park_test.go` (8 tests: park
  excludes from DrainOutbox, ListParked visibility incl. decoded project,
  unpark restores + resets attempts, unpark/discard error on a non-parked
  seq, discard excludes permanently, RecordPushFailure without parking,
  DrainOutbox parks an undecodable row and keeps draining the rest).
  `internal/syncer/park_test.go` (6 tests: park on 422, group-stop-only
  (two versions of one sync_id, second never reaches Apply; a different
  group unaffected), retryable failure records+propagates+not-parked,
  401/403 skips pull entirely incl. zero PullSince calls, 422 still pulls,
  network-error-with-no-status still pulls). No existing assertion changed;
  every pre-existing syncer test (including
  TestSyncAllProjects_PartialFailure, which predates FUP-004 and exercises
  the old early-return shape) still passes unmodified.
  Verification: `go build ./...`: ok. `go vet ./...`: ok.
  `go test ./cmd/... ./internal/... -count=1` (ENGRAM_DSN unset): all
  packages ok except the three known environmental failures.
  `go test ./internal/spike/... -tags acceptance -count=1`: ok (convergence
  proofs unaffected).
  Commit: pending (recorded after commit).

- FUP-004c done (delegated writer). New internal/localstore/nul_repair.go:
  `Store.RepairUnackedNULMutations()` scans unacked (acked_at IS NULL —
  pending or parked) sync_mutations rows and repairs each candidate in its
  own transaction: sanitize via mutation.SanitizeTextFields, re-derive
  payload+mutation_id, then update sync_mutations (new id/payload, un-park,
  reset attempts), applied_mutations (re-point the PK), and — ONLY where
  last_write_mutation_id still equals the OLD id (a newer write must never be
  clobbered) — memories/memory_tombstones.last_write_mutation_id plus the
  materialized memories row's own text fields. Wired into syncer.Push,
  immediately before DrainOutbox, best-effort (a repair failure logs and lets
  the push cycle continue for every other entry).
  GOTCHA (saved to memory): a raw-byte scan for NUL in the stored payload
  never matches — encoding/json escapes U+0000 as the six-character
  `\u0000` sequence, so the on-disk payload TEXT never contains a literal
  0x00 byte even when the mutation it encodes does. Fixed by using
  `mutation.ValidateCanonicalPayloadText` (the same decode-based check
  centralstore.Apply/cloudserve already use to reject these payloads) as the
  detector instead of any raw-byte/SQL `instr(...,char(0))` scan.
  Tests added: `internal/localstore/nul_repair_test.go` (5 tests: repairs a
  pending entry incl. applied_mutations re-pointing, repairs+un-parks a
  parked entry so DrainOutbox returns it again, skips clobbering a
  materialized row a NEWER write already superseded while still repairing
  the outbox entry itself, ignores acked rows, no-candidates is a clean
  no-op). No existing assertion changed.
  Verification: `go build ./...`: ok. `go vet ./...`: ok.
  `go test ./cmd/... ./internal/... -count=1` (ENGRAM_DSN unset): all
  packages ok except the three known environmental failures.
  Commit: pending (recorded after commit).

- FUP-004d done (delegated writer), closing FUP-004. New
  `parked_mutations` diagnostic check (internal/diagnostic/checks.go,
  registered in registry.go): one Finding per parked entry (local_seq,
  12-char mutation_id prefix, project, entity, attempts, last_error),
  safe_next_step naming both `engram sync retry`/`discard`. Store interface
  gained `ListParked`. `SyncBacklogCheck`'s underlying
  `localstore.Store.SyncBacklog()` now filters `parked_at IS NULL` too, so a
  parked entry is reported ONCE (by the new check), not doubled into the
  "waiting to push" backlog count it no longer represents.
  New CLI: `engram sync parked` (list), `engram sync retry <seq|all>`
  (un-park + reset attempts), `engram sync discard <seq>` (acks the entry in
  place without ever pushing it — see Store.DiscardMutation's doc comment
  from FUP-004b for why acked-not-deleted is the safer choice; prints what
  it discarded, reading the entry back before the discard). Unlike `sync
  now` (talks to the running daemon's control API), these three open the
  local store directly via `localstore.Open` — the same pattern `engram
  projects consolidate` already uses — since SQLite's WAL mode makes that
  safe alongside a running daemon and these subcommands are useful whether
  or not one is up. retry/discard use the same two-pass flag parse as
  consolidate so a flag may come before OR after the positional local_seq.
  Tests added: `internal/diagnostic/diagnostic_test.go`
  (`TestParkedMutationsCheck`, 3 cases: local-only stays quiet, empty is ok,
  one entry produces a bounded-prefix finding naming both CLI commands);
  updated the hardcoded `TestRegistry_IsDeterministicAndComplete` count
  (7→8, a legitimate change — a new check was actually registered).
  `cmd/engram/sync_parked_test.go` (10 tests covering list/retry-one/
  retry-all/discard against a real temp SQLite store, plus missing-db,
  missing-arg, and not-parked error paths). No other existing assertion
  changed. README documents the new subcommands and the parked-mutation
  concept.
  Verification: `go build ./...`: ok. `go vet ./...`: ok.
  `go test ./cmd/... ./internal/... -count=1` (ENGRAM_DSN unset): all
  packages ok except the three known environmental failures.
  Commit: pending (recorded after commit).

- FUP-005a done (delegated writer). execInsert (internal/localstore/apply.go)
  now sets created_at from m.OccurredAt (falling back to time.Now() only
  when OccurredAt is zero — a caller that predates the field), formatted
  with the existing sqliteTimeLayout constant so it matches every row the
  SQL DEFAULT already wrote. review_after is now computed from that same
  createdAt instead of time.Now(), mirroring the migrateV13ToV14 backfill's
  reasoning. execUpdate already never touched created_at — confirmed
  unchanged, added the regression test. occurred_at already traveled the
  wire end-to-end (WireMutation.OccurredAt, central storage, PullSince) —
  the gap was purely that execInsert never read it; no syncwire/centralstore
  change was needed for this half. importer.go's doc comment updated (it
  already set OccurredAt correctly; its rows now automatically get their
  true creation date with no importer code change).
  Tests added: `internal/localstore/created_at_test.go` (6 tests: created_at
  from OccurredAt, zero-OccurredAt now-fallback, review_after dated from
  createdAt not now for a decay type, execUpdate never changes created_at,
  RecentObservations orders a scrambled-arrival mix of very-old/old/new rows
  correctly by created_at DESC, SearchFilter.CreatedFrom/CreatedTo correctly
  separates an old pulled row from a fresh local one). No existing assertion
  changed; the whole existing localstore/importer/syncer suite passed
  unmodified against the new execInsert behavior.
  Verification: `go build ./...`: ok. `go vet ./...`: ok.
  `go test ./internal/localstore/... ./internal/importer/... -count=1`
  (ENGRAM_DSN unset): ok.
  Commit: pending (recorded after commit).

- FUP-005b done (delegated writer). DEVIATION from the briefing's literal
  "GET /api/v1/created-at" example: implemented as **POST /v1/created-at**
  in cloudserve (not controlapi) — every existing cloudserve route
  (push/pull/projects/unshare/state) is POST with a JSON body under `/v1/`,
  and the auth middleware HMAC-signs method+path+body, so a bodyless GET
  would need a different signing scheme than every sibling route. Matched
  the established convention instead of the example's literal spelling;
  the capability-gating/compat behavior (501 when unsupported, 404 from an
  old server) is unaffected by the verb choice.
  New: `syncwire.CreatedAtRequest/Entry/Response` (keyset-paged by sync_id,
  `after`=last-seen sync_id, empty Entries=drained — mirrors PullRequest's
  own paging contract). `centralstore.Store.OriginalCreatedAt` —
  `MIN(occurred_at) GROUP BY entity_key` on central_mutations (the
  append-only journal already holding every push's occurred_at across a
  sync_id's whole version history), clamped to [1,2000]/default 500 like
  PullSince. New index `idx_cmut_project_entity_key ON central_mutations
  (project, entity_key)`, additive/idempotent (central schema.go). cloudserve:
  new optional `createdAtLister` capability (mirrors projectLister/
  projectDeleter/writerPurgeEpoch) + `handleCreatedAt`, registered at
  `POST /v1/created-at`, same withAuth wrapping as every other route.
  Tests added: `internal/cloudserve/server_createdat_test.go` (6 tests:
  page returned + args forwarded, keyset paging reaches an empty/drained
  page, missing-project 400, no-capability 501, store-error 500).
  `internal/centralstore/created_at_acceptance_test.go` (NEW acceptance
  file, `//go:build acceptance`, 3 tests: earliest-occurred_at-wins across
  two versions of one sync_id, keyset paging visits every entry exactly
  once in order, project scoping). Ran against REAL Postgres
  (embedded-postgres auto-started, ENGRAM_DSN unset — no shared UAT DB
  touched): all 3 new tests passed, plus the FULL existing
  `internal/centralstore` (70s) and `internal/cloudserve` (40s)
  acceptance suites, confirming the new index/query didn't regress
  anything already covered there.
  No existing assertion changed.
  Verification: `go build ./...`: ok. `go vet ./...`: ok.
  `go test ./cmd/... ./internal/... -count=1` (ENGRAM_DSN unset): all
  packages ok except the three known environmental failures.
  `go test ./internal/centralstore/... ./internal/cloudserve/... -tags
  acceptance -count=1` (ENGRAM_DSN unset, embedded-postgres): ok.
  Commit: pending (recorded after commit).

## Next Step

FUP-005c (client backfill, once per project).
