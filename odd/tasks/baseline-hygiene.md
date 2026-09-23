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
- [ ] **BH-002 — Split tools.go by tool family** — Route: delegated writer
- [ ] **BH-003 — Split hook.go by concern** — Route: delegated writer
- [ ] **BH-004 — Mapping cleanups** — Route: delegated writer
- [ ] **BH-005 — Unified response envelope + dual id acceptance** — Route: delegated writer

## Progress

- Mapping complete (read-only explorer): isolation root cause traced to config-file fallback in `daemon.go:257-349` and stdio transport default returning exit 0 on empty stdin.

## Next Step

Choose chain strategy, then BH-001.
