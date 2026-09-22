# Pre-Push Parity Fixes

## Objective

Fix the three pre-push defects found in the review of `feat/upstream-parity` before the branch is pushed.

## Problem

- Writes whose directory came from the daemon's own working directory (`directory_source=daemon_cwd`) are not blocked, and the daemon now starts in `%APPDATA%\engram`, so directory-less saves are silently filed under project `engram`.
- Hook output hardcodes the `mcp__engram__` tool prefix; plugin installs expose `mcp__plugin_engram_engram__`, so the bootstrap and pointer name tools that do not exist.
- `engram setup hooks` re-encodes `settings.json` with HTML escaping (`&&` becomes `&&`) and replaces a symlinked `settings.json` with a regular file.

## Scope

- Block writes (mem_save, mem_save_prompt, mem_session_start, mem_session_summary) when the directory source is `daemon_cwd` and no explicit project is given; report `writes_blocked=true` from `mem_current_project`.
- Make hook-emitted tool references valid for both the global MCP registration and the plugin install.
- Encode settings without HTML escaping and write through a symlinked settings file to its target.

## Constraints

- No push or pull request.
- Conventional commits, no AI attribution trailers.
- Explicit `project` must keep working for daemon_cwd callers.

## Configuration

- TDD: off; no project or session TDD setting was found
- Test runner: `go test`
- Verification: `go build ./...`, `go vet ./...`, `go test ./cmd/... ./internal/...` (the three `TestRun_Daemon*` env failures also fail on origin/main on this machine and are known environmental failures)
- RDD: off (global)
- Delivery strategy: exception-ok (appended to the unpushed branch)
- Forecast: under 400 authored lines

## Tasks

- [x] **FIX-001 — Block daemon_cwd writes**
  - Route: delegated (writer; tools.go + hook.go + tests)
  - Commit: ef436fc
- [x] **FIX-002 — Prefix-agnostic tool references in hooks**
  - Route: delegated (same writer)
  - Commit: a55b1e1
- [x] **FIX-003 — Non-escaping, symlink-safe settings writes**
  - Route: delegated (same writer)
  - Commit: 490ea88
- [x] **FIX-004 — Scope the daemon_cwd write block to the shared HTTP daemon only**
  - Route: delegated (same writer)
  - Reason: FIX-001's daemon_cwd refusal was a real regression for the
    per-client `engram daemon --transport stdio` setup README.md documents
    (the MCP client spawns that daemon IN the project directory, so its cwd
    genuinely IS the workspace) — it cannot be told apart from the shared
    resident daemon's junk cwd without a signal from how the daemon itself
    was started.
  - Commit: this commit (doc folded in; hash reported to the coordinator directly since a commit cannot record its own hash)

## Progress

- Created after review of the 20 unpushed commits.
- FIX-001 done. `resolveSaveProject` and `handleSessionStart` now refuse a
  `dirSourceDaemonCwd` directory (no "directory"/"cwd" argument reached the
  daemon) unless the caller names an explicit project; `mem_current_project`
  reports `writes_blocked=true` for the same condition. Reads (`resolveReadProject`)
  stay lenient, unchanged. Updated the hook.go comment that undercounted the
  conditions writes_blocked composes, and the two agent-facing description
  strings (tools.go mem_current_project description, instructions.go) that
  listed the blocking reasons without mentioning "no directory at all".
  - Tests changed because they encoded the old (buggy) daemon_cwd-succeeds
    behavior, not new coverage: `TestDaemonTool_MemSave_NoDirectoryStillUsesCwd`
    → renamed `TestDaemonTool_MemSave_NoDirectoryIsRefused` (client_directory_test.go);
    `TestDaemonTool_MemSessionStart_ExplicitProjectCorrectsStoredRow` now seeds
    the misfiled row via `store.CreateSession` directly instead of relying on
    the handler to misfile it (client_directory_test.go);
    `TestDaemonTool_MemSave_CreatesObservation`, `TestDaemonTool_MemSave_InvalidConfig`,
    `TestDaemonTool_MemSessionSummary_CreatesSessionSummary` (daemon_test.go) and
    `TestDaemonTool_MemSessionSummary_SessionIDDefault` (session_default_test.go)
    now pass an explicit `project` (or, for InvalidConfig, an explicit
    `directory`) since they were relying on a bare daemon_cwd write to succeed
    and were not testing project resolution itself.
  - Tests added: `TestCurrentProject_DaemonCwdFallbackIsFlagged` now asserts
    `writes_blocked=true`; `TestWriteTools_RefuseEveryWritesBlockedDirectory`
    gained a "daemon cwd (no directory sent at all)" fixture; new
    `TestWriteTools_DaemonCwdWithExplicitProjectSucceeds` (tools_current_project_test.go).
  - Verification: `go build ./...` clean; `go vet ./...` clean;
    `go test ./cmd/... ./internal/... -count=1` → only the 3 known
    environmental failures (TestRun_DaemonMissingDB,
    TestRun_DaemonCentralURLMissingWriterID, TestRun_DaemonCentralURLMissingWriterKey).

- FIX-002 done. `cmd/engram/hook.go` hardcoded the `mcp__engram__` prefix in
  three narrative places (hookProtocolPointer, hookBootstrapContext's "call X
  first" sentence, hookSaveNudge) and in the "Available tools" list. Narrative
  sentences now use BARE tool names + "provided by the engram MCP server" /
  "(from the engram MCP server)" wording — a bare name is valid regardless of
  which prefix the host actually exposes, so nothing there can name a
  nonexistent tool. The "Available tools" list (hookBootstrapContext) is the
  one place fully-qualified names matter: it exists specifically so a host
  that defers MCP tool loading surfaces engram's tools once something names
  them the way the host itself registered them, and the hook process cannot
  tell at execution time whether the host used the global MCP registration
  (`mcp__engram__X`) or the Claude Code plugin install
  (`mcp__plugin_engram_engram__X`, from plugin.json naming the plugin
  "engram" and .mcp.json naming the server "engram" too). Chose to list BOTH
  prefixed forms there rather than guess one, per the task's documented
  fallback — the cost is one longer comma-joined line; the alternative (bare
  names in that list) risks the exact defect this fix targets if the host's
  loader genuinely keys off the fully-qualified string. Added
  hookToolPrefixGlobal/hookToolPrefixPlugin constants so both forms are
  defined once. Checked plugin/**/hooks, docs/agent-instructions.md and
  instructions.go for the same hardcoding — none found, no changes needed
  there.
  - Tests changed: `TestHookSessionStart_NoDaemon_StillPrintsThePointer` now
    asserts the bare name and that the global-only prefix is absent (plugin
    naming works); `TestHookUserPromptSubmit_FirstPromptBootstraps` now checks
    the "call X first" sentence for the bare name and the "Available tools"
    list for BOTH prefixed forms of mem_save (hook_test.go).
  - Verification: `go build ./...` clean; `go vet ./...` clean;
    `go test ./cmd/... ./internal/... -count=1` → only the 3 known
    environmental failures.

- FIX-003 done, three independent problems in `cmd/engram/setup.go`:
  (a) `json.Marshal`/`MarshalIndent` HTML-escape JSON strings by default —
  even INSIDE an already-encoded `json.RawMessage` being merged through — so
  a user's existing hook command containing `&&`/`<`/`>` came back rewritten
  as six-character unicode escapes on every merge. Added a `marshalJSON`
  helper (`json.NewEncoder` + `SetEscapeHTML(false)`, `SetIndent` for the one
  caller that needs indentation) and routed all three encode call sites
  through it; the Encoder's trailing newline is trimmed so callers keep
  controlling their own exactly as before (byte-stable otherwise).
  (b) `writeHookSettings`'s atomic write (temp file + `os.Rename`) replaced a
  symlinked settings.json with a regular file, since `os.Rename` over a
  symlink path replaces the dirent itself. It now `os.Lstat`s the path, and
  when it is a symlink resolves it with `filepath.EvalSymlinks` and performs
  both the atomic write and the `.bak` against that resolved target, so the
  link survives; a dangling link (EvalSymlinks failing) falls back to the
  pre-fix behavior, no worse than before.
  (c) The `engramHookPack` doc comment claimed the shipped plugin packs
  invoke "the plugin's own copy of the binary" — false: `plugin/claude-code/hooks/hooks.json`
  and `plugin/codex/hooks/hooks.json` both call bare `"engram hook <event>"`
  from PATH, identically to the settings-merge path, and
  `TestEngramHookPack_MatchesShippedPack` already pins them byte-equal.
  Corrected the comment.
  - Gotcha: writing literal `\uXXXX`-style escape-sequence TEXT (as opposed to
    an actual escaped character) into a tool-call parameter got silently
    unescaped by a layer of the tool/transport pipeline before it reached the
    file (e.g. intended source text `&` landed in the file as `&`) —
    this corrupted both a doc comment and a test assertion on the first
    attempt. Fixed by describing escapes in prose in comments, and by
    building the check strings from `string(rune(0x5C))` + `"u0026"` etc. at
    Go runtime in the test instead of spelling the escape sequence out as
    literal source text.
  - Tests added: `TestSetupHooks_PreservesUserCommandsWithHTMLCharacters`
    (mergeHookSettings round-trips `"a && b > c"` byte-identical, no unicode
    escapes present); `TestSetupHooks_WritesThroughASymlinkTarget` (a
    symlinked settings.json stays a symlink, pointing at the same target,
    after `engram setup hooks`; the `.bak` lives next to the target). The
    symlink test SKIPPED on this machine: `os.Symlink` failed with "A
    required privilege is not held by the client" (Windows, no Developer
    Mode / SeCreateSymbolicLinkPrivilege) — the graceful-skip path the task
    asked for, exercised for real, not just written defensively.
  - Verification: `go build ./...` clean; `go vet ./...` clean;
    `go test ./cmd/... ./internal/... -count=1` → only the 3 known
    environmental failures. `gofmt -l` flags every .go file in the repo,
    touched or not (pre-existing CRLF line endings on this Windows checkout,
    confirmed with `gofmt -d` showing a whole-file diff of line-ending-only
    changes) — not a regression, left alone.

- FIX-004 done. FIX-001's blanket daemon_cwd refusal broke a real, documented
  setup (README.md:457): a per-client `engram daemon --transport stdio`,
  which the MCP client spawns IN the project directory, so its cwd genuinely
  IS the workspace — that daemon and the SHARED resident daemon (`--transport
  http`) look identical to a tool handler (both are `dirSourceDaemonCwd`),
  so the fix needed a signal from how the daemon was actually started.
  `daemonCwdIsWorkspace bool` (computed in `buildDaemon` as `cfg.mcpTransport
  == "stdio"`, an explicit parameter — no package-level mutable global) is
  now threaded through `registerTools` into `handleCurrentProject`,
  `handleSessionStart`, `handleSave`, `handleSavePrompt`,
  `handleSessionSummary`, `resolveSaveProject` and `currentProjectEnvelope`.
  When true, the `dirSourceDaemonCwd` refusal in `resolveSaveProject` and
  `handleSessionStart` does not fire, and `currentProjectEnvelope` does not
  set `writes_blocked` for it — `directory_source` still reports
  `"daemon_cwd"` (unchanged label), with a softer hint explaining the cwd is
  trusted because this is a per-client stdio daemon. Reads were already
  unaffected (resolveReadProject never consulted the flag). Left
  `hook.go:531`'s `answersAboutTheDaemon` alone — hooks always talk to the
  shared HTTP daemon through `engram connect`, so `daemonCwdIsWorkspace` is
  always false on that path and nothing there needed to change (confirmed:
  `TestHook*` all still pass).
  - No FIX-001 test needed reverting: every existing `buildDaemon(daemonCfg{...})`
    call in the test suite leaves `mcpTransport` at its Go zero value `""`,
    which is `!= "stdio"`, so `daemonCwdIsWorkspace` is `false` by default —
    identical to pre-FIX-004 behaviour. Confirmed by running the full suite
    unchanged before adding new tests: same 3 known failures, nothing else.
  - Tests added (tools_current_project_test.go):
    `TestWriteTools_StdioDaemonCwdIsTrusted` (`mcpTransport: "stdio"`, no
    directory/no project → `mem_save` succeeds under the chdir'd project,
    `mem_current_project` reports `writes_blocked=false`,
    `directory_source="daemon_cwd"`); `TestWriteTools_HttpDaemonCwdStaysBlocked`
    (`mcpTransport: "http"` explicit → refusal and `writes_blocked=true`,
    same as the FIX-001 default).
  - Docs: `README.md`'s `writes_blocked` causes table (~line 999) was missing
    `daemon_cwd` as a cause entirely (a FIX-001 gap, not just a FIX-004 one) —
    added it, with the stdio-daemon exception noted. Line ~457 (the
    coordinator's cited line) already correctly described the daemon's own
    cwd as "the right answer for a per-client stdio daemon" and needed no
    change — that was about directory *resolution*, not the write refusal.
  - Verification: `go build ./...` clean; `go vet ./...` clean;
    `go test ./cmd/... ./internal/... -count=1` → only the 3 known
    environmental failures.

## Next Step

None — all four fixes are done and verified.
