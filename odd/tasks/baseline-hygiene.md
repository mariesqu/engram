# Baseline Hygiene (Roadmap Step 0)

## Objective

Make the v1.6.1 codebase a safe base for the provenance roadmap (`sdd/explore/codex-claude-relevance-2026`): hermetic tests, navigable MCP/hook sources, and one response convention for every MCP tool.

## Problem

- `cmd/engram` tests fall back to the real user config (`%APPDATA%\engram\config.json`) because nothing isolates `ENGRAM_CONFIG_DIR`/`APPDATA`/`HOME`. `TestRun_DaemonMissingDB` opens the user's live `engram.db`; the three `TestRun_Daemon*` negative tests pass falsely on CI and fail on any machine with an install.
- `cmd/engram/tools.go` (2687 lines) and `cmd/engram/hook.go` (1641 lines) mix every tool family and every hook concern in one file each.
- Tool responses are inconsistent: 4 tools return JSON, 12 return prose, `mem_save` returns prose plus a YAML-like block; id inputs are numeric in some tools and `sync_id` in others.

## Scope

- BH-001 Hermetic test environment for `cmd/engram` (default and acceptance builds).
- BH-002 Split `tools.go` by tool family — pure move, no behavior change.
- BH-003 Split `hook.go` by concern — pure move, no behavior change.
- BH-004 Small cleanups found during mapping: duplicated id parsing, zero-arg `fmt.Sprintf`, `urlQueryEscape` duplicate of `net/url`, stale `PR-N` history comments, stale test comments.
- BH-005 Unified response envelope: keep each tool's current first line, append a fenced JSON block with structured fields (`ok`, `id`, `sync_id`, tool-specific fields); accept numeric id or `sync_id` wherever an observation is referenced and always return both.

## Constraints

- BH-002/BH-003 are pure moves: identical registration order, no renamed symbols, reviewable with `git diff --color-moved`.
- BH-005 must keep the hook's prefix match (`"No previous session memories found."`), the `mem_current_project` JSON contract, and the documented `mem_save` conflict fields working; update README, `docs/agent-instructions.md` and `instructions.go` alongside.
- Conventional commits, no AI attribution trailers. Push and PR per the chosen chain strategy; merges follow repository policy.

## Configuration

- TDD: off; no project or session TDD setting found
- Test runner: `go test` (acceptance: `-tags acceptance`)
- Verification: `go build ./...`, `go vet ./...`, `go vet -tags acceptance ./...`, `go test ./... -count=1`, `go test -tags acceptance ./... -count=1 -timeout 900s` — after BH-001 all must be fully green (no known environmental failures remain)
- RDD: off (global)
- Delivery strategy: ask-on-risk (forecast exceeds 400 authored lines because of the file moves); chain strategy: stacked-to-main (user choice, 2026-09-23) — one PR per task BH-001 → BH-005, each merged to main after CI and review-bot threads resolve
- Slice boundaries: BH-001 PR (~80 lines); BH-002 and BH-003 are pure moves that cannot fit 400 changed lines (moves count twice) — `size:exception` to be requested from the maintainer; BH-004 PR; BH-005 PR
- Forecast: BH-001 ~80, BH-002 ~2700 moved, BH-003 ~1650 moved, BH-004 ~80, BH-005 ~400-600

## Tasks

- [x] **BH-001 — Hermetic cmd/engram tests** — Route: delegated writer
  - Shared `isolateUserEnv()` helper (untagged file) pointing ENGRAM_CONFIG_DIR, APPDATA, LOCALAPPDATA, HOME, USERPROFILE, XDG_CONFIG_HOME, XDG_CACHE_HOME at one temp dir and clearing ENGRAM_*; `testmain_test.go` with `//go:build !acceptance`; the existing acceptance `TestMain` (`serve_acceptance_test.go:43`) calls the helper; the three daemon tests also set their own `ENGRAM_CONFIG_DIR`.
  - Accept: full suite green on a machine with a real install and a running daemon; no test touches the real config dir.
  - Done (2026-09-23): `cmd/engram/testenv_test.go` (new, untagged) — `isolateUserEnv()` + sanity test `TestIsolateUserEnv_PointsInsideTempDir`; `cmd/engram/testmain_test.go` (new, `//go:build !acceptance`) — `TestMain` calling it; `cmd/engram/serve_acceptance_test.go`'s existing acceptance `TestMain` now calls it first; `cmd/engram/daemon_test.go`'s three `TestRun_Daemon*` negative tests each also set `ENGRAM_CONFIG_DIR` to `t.TempDir()` and their comments now explain the config-file fallback chain they're guarding against.
    Other packages checked for the same leak, no fix needed: `internal/config/configdir_env_test.go` (reads real `os.UserConfigDir()` in one sub-test but only asserts a path suffix, no file I/O); `internal/tray`, `internal/updater` (no `UserConfigDir`/`UserHomeDir`/`DefaultConfigDir` references at all); the `cacheRoot()` helper duplicated across `cmd/engram` and several `internal/*` acceptance test files (`centralstore`, `syncer`, `remote`, `cloudserve`, `spike`) falls back to `os.UserHomeDir()` only for an embedded-postgres binary download cache, unrelated to engram's own config/db — left as-is since GOPATH is set on this machine so it never reaches that fallback anyway. `cmd/engram/hook_test.go` already isolates its own state dir per test (`isolateHookStateDir`); `setup_test.go`'s `TestSetupHooksPath_HonoursHostEnvironment` and `daemonspawn_test.go`'s `TestSpawnWorkingDir_FallsBackToHome` read `os.UserHomeDir()` for direct comparison (not real-file access) and pass unchanged under the now-isolated temp home; `connect_test.go`'s `resolveConnectDBPath`/`runConnectCmd` tests already set `ENGRAM_CONFIG_DIR` per test.
    Proof: `go build ./...` clean; `go vet ./...` and `go vet -tags acceptance ./...` clean; `go test ./... -count=1` fully green (26.4s for `cmd/engram`) with the real `ENGRAM_DB`/`ENGRAM_CENTRAL_URL`/`ENGRAM_WRITER_ID`/`ENGRAM_WRITER_KEY`/`ENGRAM_DSN`/`ENGRAM_EMBEDDING_KEY` env vars left set; `go test -tags acceptance ./... -count=1 -timeout 900s` fully green (166.3s for `cmd/engram`, embedded-postgres cache reused via `GOPATH`, no re-download). `%APPDATA%\engram\config.json` and `engram.db` mtimes unchanged before/after both runs; `engram.db-wal` mtime did change (Sep 23 16:06:02 → 16:14:17), which is the live daemon's own background writes, not the test run — not treated as proof by itself, per the task's own caveat. Definitive proof is `TestIsolateUserEnv_PointsInsideTempDir`, which asserts `APPDATA`/`ENGRAM_CONFIG_DIR` resolve inside `os.TempDir()` at test time; it passed in both builds.
    Commit: see PR.
- [x] **BH-002 — Split tools.go by tool family** — Route: delegated writer
  - `cmd/engram/tools.go` (2687→44 lines: `registerTools` only, now calling 8 `registerXTools`); `cmd/engram/tools_helpers.go` (352, new) — arg-description consts, `directoryArg` + methods, `readDirectoryArg`/`newDirectoryArg`, `directoryAwareTools`, `resolveProjectDir`/`resolveReadProject`; `cmd/engram/tools_current_project.go` (322, new) — `registerCurrentProjectTools`, `handleCurrentProject`, `currentProjectEnvelope`, `applyPolicyBlock`, `setHints`; `cmd/engram/tools_session.go` (356, new) — `registerSessionTools`, `handleSessionStart`/`handleSessionEnd`/`handleSessionSummary`; `cmd/engram/tools_save.go` (552, new) — `registerSaveTools`, `resolveSaveProject`, `handleSave`, `handleSavePrompt`, `handleUpdate`, `handleSuggestTopicKey`, `triggerSync`; `cmd/engram/tools_search.go` (433, new) — `registerSearchTools`, `handleGetObservation`, `toolObservationID`, `handleSearch`, `handleContext`, `toolSearchOffset`/`toolSearchTime`; `cmd/engram/tools_pin.go` (108, new) — `registerPinTools`, `handlePin`; `cmd/engram/tools_judge_similar.go` (262, new) — `registerJudgeSimilarTools`, `handleJudge`, `handleMemSimilar`, `parseObservationID`; `cmd/engram/tools_review.go` (277, new) — `registerReviewTools`, `handleReview`, `handleMergeProjects`, `nearVariantProject`/`normalizeForDrift`; `cmd/engram/tools_doctor.go` (118, new) — `registerDoctorTools`, `handleDoctor`, `handleDoctorWithRunner`.
  - Pure move, `size:exception` applied (maintainer-approved, this PR only): every `srv.AddTool(...)` definition and every handler/helper body copied byte-for-byte; no symbol renamed, no logic edited, no comment reworded. Move-proof script (sorted-multiset diff of non-blank, non-package/import lines, old `tools.go` vs the 10 new/changed files, minus the 8 new `registerXTools` signatures/braces/call-lines) reports 0/0 — PASS. `git diff -M --color-moved=zebra --stat main`: 10 files changed, 2789(+)/2652(-); 5293 of 5517 diff-body lines carry git's move-detected color. Imports trimmed per file (built via `go build` unused/undefined-symbol iteration, no `goimports` in `GOBIN`); CRLF preserved (repo is `core.autocrlf=true`) — `gofmt -w` normalizes to LF, so the 10 files were reconverted to CRLF after formatting and rebuilt/retested clean.
  - Deviation: `registerTools`'s call-site order changed from the flat original AddTool sequence to 8 grouped `registerXTools(...)` calls (one per family) — session tools split across positions 2/3/18 in the original couldn't stay both grouped-by-family AND positionally interleaved, so grouping won over interleaving. Verified harmless, not just declared so: `mcp-go@v1.1.0` `AddTool`→`AddTools` stores into `s.tools map[string]*ServerTool` (server.go:1027+) — no order-dependent state; `ListTools()` (server.go:1119) returns a plain unordered map copy; the `tools/list` RPC path (`filteredTools`, server.go:1900) always `sort.Strings`s tool names regardless of insertion order. Every existing test in `cmd/engram` reads `ListTools()` by name or ranges the map (never asserts insertion order). A throwaway test (`TestZZOrderVerify_AllToolsRegisteredExactlyOnce`, not committed) confirmed all 18 names register exactly once, byte-identical to the pre-split set.
  - Done (2026-09-23).
  - Proof: `go build ./...` clean; `go vet ./...` clean; `go vet -tags acceptance ./...` clean; `go test ./... -count=1` fully green (all packages `ok`, `cmd/engram` 34.6s); `go test -tags acceptance ./cmd/engram/ -count=1 -timeout 900s` fully green (82.9s). No known environmental failures.
  - Commit: see PR.
- [x] **BH-003 — Split hook.go by concern** — Route: delegated writer
  - `cmd/engram/hook.go` (1641→296 lines: usage, per-event time budgets, `hookInput`, `runHookCmd` top-level dispatch, stdin read/decode); `cmd/engram/hook_transport.go` (205, new) — daemon access: `newToolClient`, `hookRemaining`, `dialHook`, `mcpBridge.callTool`, `parseToolResult`, `jsonFromMCPBody`, plus `urlQueryEscape` (moved, not replaced); `cmd/engram/hook_project.go` (279, new) — project resolution (`hookResolveProject`, `hookProject`, `hookProjectFromSession`) plus the per-session project cache (`hookProjectCacheTTL`, `hookNow`/`hookDetectionFingerprint`, `hookCachedProjectFile`, `hookProjectFingerprint`, `hookCacheProject`, `hookCachedProject`); `cmd/engram/hook_events.go` (583, new) — the four per-event handlers: session-start/post-compaction, user-prompt-submit, subagent-stop, session-end, and their private helpers; `cmd/engram/hook_state.go` (321, new) — state files: markers, hashing, clearing, the FUP-003 occurrence-dedup section, claim/age/touch.
  - Pure move, `size:exception` applied (maintainer-approved, this PR only): every declaration copied byte-for-byte from its mapped line range (1-305 / 306-478+1628-1641 / 479-620+1502-1627 / 621-1193 / 1194-1501), no symbol renamed, no logic edited, no comment reworded — including `urlQueryEscape`, moved as-is per the maintainer's explicit instruction (its `net/url` duplication is BH-004's, not this task's). Move-proof script (sorted-multiset diff of non-blank, non-package/import lines, old `hook.go` read via `git show main:cmd/engram/hook.go` decoded as UTF-8 bytes vs the 5 new/changed files): 1506 lines on each side, 0 missing / 0 extra — PASS. `git diff -M --cached --stat main`: 5 files changed, 1388(+)/1345(-); `git diff -M --cached --color-moved=zebra --color=always main`: 2684 of 2733 diff-body (+/-) lines carry git's move-detected (zebra) color. Imports rebuilt per file against `go build` (no `goimports` in `GOBIN`); CRLF preserved (repo is `core.autocrlf=true`) — `gofmt -w` normalized the 5 files to LF, reconverted to CRLF after formatting, rebuilt/vetted clean.
  - Done (2026-09-23).
  - Proof: `go build ./...` clean; `go vet ./...` clean; `go vet -tags acceptance ./...` clean; `go test ./... -count=1` fully green (all packages `ok`, `cmd/engram` 27.0s); `go test -tags acceptance ./cmd/engram/ -count=1 -timeout 900s` fully green (47.8s); `go test ./cmd/engram/ -run 'TestHook' -count=20` fully green (82.5s, no flakes). No known environmental failures.
  - Commit: see PR.
- [ ] **BH-004 — Mapping cleanups** — Route: delegated writer
  - New note from BH-003 mapping: `cmd/engram/connect.go:751`'s comment ("see directoryAwareTools in tools.go") is stale since BH-002 — `directoryAwareTools` now lives in `cmd/engram/tools_helpers.go`. Fold this into BH-004's stale-comment cleanup pass.
- [ ] **BH-005 — Unified response envelope + dual id acceptance** — Route: delegated writer

## Progress

- Mapping complete (read-only explorer): isolation root cause traced to config-file fallback in `daemon.go:257-349` and stdio transport default returning exit 0 on empty stdin.

- BH-001: parent follow-up keeps `ENGRAM_TEST_*` harness overrides (e.g. `ENGRAM_TEST_PG_DSN`) out of the unset list.

## Next Step

BH-002 and BH-003 PRs through CI and review bot, then BH-004 (mapping cleanups, including the `connect.go:751` stale comment noted above).
