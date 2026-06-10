# Proposal: tray-ui — Loopback Control API + Per-Project Policy + Visual Config + Embedded Web UI + Windows Tray + HTTP MCP Transport

## Intent

Today engram is configured exclusively through flags and env vars (`--db`, `--central-url`, `--writer-id`, `ENGRAM_WRITER_KEY`, `--sync-interval`), with no visible connection status, no way to opt a project out of central sync, and no UI. Every Claude Code session spawns a fresh stdio daemon (`cmd/engram/daemon.go`) that opens the SQLite file directly; cross-process write coordination relies on SQLite's OS-level writer lock plus `busy_timeout`, and there is no authoritative owner of node-local state.

This change gives engram a **control plane**: ONE loopback HTTP control API that owns local-node state, with the **tray, web UI, and CLI all as thin clients** of that single API. Users get visible central-connectivity status, per-project local/remote/omit selection, and a visual configuration surface that replaces the env-vars-only setup — while CLI parity is preserved for advanced and headless use.

Success looks like: a user can see whether engram is connected to central, flip a project to `local-only` or `omitted` from a tray menu or a browser, configure the writer credentials without exporting env vars, and have all of that enforced provably at the sync boundary — with stdio MCP still working unchanged for anyone who never opts in.

## Scope

### In Scope
- **Loopback control API** (`127.0.0.1:7700` default) with bearer-token auth, origin checks, and the endpoint surface for status, config, central connect/disconnect, project policy, and sync trigger.
- **Per-node per-project policy** (`synced | local-only | omitted`) stored in a new local SQLite `project_policy` table (schema v9 → v10), enforced at push time and pull time.
- **Config file** at `%APPDATA%\engram\config.json` replacing flags/env as the primary config source (flags/env remain valid overrides), with DPAPI-encrypted writer-key at rest on Windows.
- **Embedded web UI** (HTMX + server-rendered HTML, `go:embed`) served from the resident daemon at `/ui/`, following the `old_code/internal/cloud/dashboard` pattern.
- **Windows system tray** via hand-rolled `Shell_NotifyIcon` (`golang.org/x/sys/windows`, `//go:build windows`): status indicator + context menu (Open UI, Sync Now, Connection status, Quit).
- **Opt-in HTTP MCP transport** (`mcp-go` v0.44 `StreamableHTTPServer`) behind a `--transport http` flag; **stdio remains the default indefinitely**.
- **CLI thin clients**: `engram status`, `engram config get/set`, `engram projects list`, `engram projects policy`, `engram sync now`, `engram ui`.

### Out of Scope (each = its own future change)
- **Semantic search / embeddings** — orthogonal; not touched here.
- **Cross-platform tray** (macOS/Linux native tray) — Windows-first tray only; non-Windows gets `engram ui` (browser launch) as graceful degradation.
- **Non-Windows secret storage** (Keychain / Secret Service / libsecret) — non-Windows falls back to env-only key management in this change.
- **Deprecating stdio / forcing the resident daemon** — stdio stays the default; HTTP transport is purely opt-in.
- **Central-side policy or admin controls** — policy here is strictly node-local; central needs no changes.
- **Outbox garbage collection / cleanup job** for long-lived `local-only` projects — noted as a caveat, deferred.

## Capabilities

### New Capabilities
- `control-api`: loopback HTTP control plane owning node-local state; token-authenticated; the single source the tray, web UI, and CLI all talk to.
- `project-policy`: per-node `synced | local-only | omitted` policy with push-time and pull-time enforcement; new local schema v10 table.
- `visual-config`: `%APPDATA%\engram\config.json` config file + DPAPI-encrypted writer-key at rest on Windows; runtime-mutable vs restart-required settings distinguished.
- `web-ui`: embedded HTMX/server-rendered UI (`go:embed`) for status, projects, config, and sync — loopback-only.
- `tray`: Windows-only `Shell_NotifyIcon` tray with status + context menu; auto-launches the resident daemon.
- `mcp-http-transport`: opt-in Streamable HTTP MCP transport alongside the default stdio transport.

### Modified Capabilities
- `syncer` (Push / SyncAllProjects): gains policy-aware filtering — push drain skips `local-only`/`omitted` entries; pull excludes those projects.
- `localstore` (schema): additive v10 migration adding `project_policy`.
- `daemon` (`cmd/engram`): gains resident-mode wiring (HTTP server, control API, optional HTTP MCP transport) layered on the existing stdio daemon.

## Approach

Stand up a **resident daemon** that owns the SQLite file and the sync loop, binds a loopback HTTP server, and exposes a versioned control API (`/api/v1/...`) plus an embedded web UI (`/ui/`). The tray and CLI are thin HTTP clients of that API; there is exactly one control plane. Per-project policy lives in a new local `project_policy` table and is enforced by filtering the existing outbox-drain (push) and project-pull paths — **save locally, decide at the sync boundary** — so flips between policies need no outbox surgery. Config moves to a JSON file under `%APPDATA%`, with the writer key DPAPI-encrypted on Windows and env-only elsewhere. stdio MCP remains the default; HTTP MCP transport is an opt-in flag for users who run the resident daemon.

### Decision Table

| # | Decision | Recommendation | Rationale |
|---|----------|----------------|-----------|
| 1 | Process model | **A3 — resident daemon as the control plane; HTTP MCP transport opt-in; stdio default** | A3 is architecturally cleanest: one process owns SQLite + the sync loop + the control API + the UI; tray/web/CLI are all thin clients. We do NOT force migration — stdio stays default indefinitely (hard constraint). The resident daemon is opt-in: it materializes when the user starts the tray or runs `--transport http`. Per-MCP-client stdio daemons (today's model) keep working unchanged. We explicitly reject A1 (stdio shim per session — extra hop + per-session process) and A2 (separate coordinator — leaves the multi-process SQLite races and gives policy no authoritative owner). |
| 2 | Tray library | **Hand-rolled `Shell_NotifyIcon` via `golang.org/x/sys/windows`, `//go:build windows`** | Only truly `CGO_ENABLED=0` tray option on Windows with **zero new direct deps** — `golang.org/x/sys` is already an INDIRECT dep (go.mod:31, `v0.42.0`); this change promotes it to a direct dep, which is the one dependency-graph change and is explicitly called out. getlantern/fyne/energye all add a new direct dep and/or require CGO off-Windows. Non-Windows degrades to `engram ui` (browser). |
| 3 | UI stack | **HTMX + server-rendered HTML, `go:embed`** | No JS build step, single static binary preserved. Mirrors the proven `old_code/internal/cloud/dashboard` pattern (HTMX), adapted to a loopback subset (status / projects / config / sync). A React/Vue SPA would add a separate build artifact to embed. |
| 4 | Secret handling (writer key) | **DPAPI-encrypted in `config.json` on Windows (user-bound); env-only fallback non-Windows/CI; `ENGRAM_WRITER_KEY` env always wins when set** (LOCKED) | DPAPI (`CryptProtectData`/`CryptUnprotectData` via `golang.org/x/sys/windows`) binds the key to the current Windows user, is pure syscall (no CGO), and gives usable persistence — the user need not re-export the secret every restart. The existing env-only design (`daemon.go:49` — "never a flag", verified) is preserved as the override and the non-Windows path. The key is NEVER a flag default; `--help` must not leak it (existing regression guard `TestRun_DaemonHelp_DoesNotLeakWriterKey` stays green). |
| 5 | Policy: `omitted` semantics | **REFUSE capture: `mem_save` AND `mem_save_prompt` return an error for omitted projects; NOTHING is written locally** (LOCKED) | Stronger than local-only. The tool handler checks project policy BEFORE `LocalWrite`; an omitted project is a hard "do not record this project here" signal to the agent. `local-only` = capture locally, never sync. `synced` = normal push+pull. |
| 6 | Policy: default for new projects | **`synced` when central is configured (opt OUT per project); `local-only` when no central configured** (LOCKED) | Preserves current behavior exactly (without central, nothing pushes anyway). Convenience-first when central is present; privacy is opt-out per project via the UI/CLI. |
| 7 | Policy enforcement points | **Push-time + pull-time filtering on the existing paths** | `Push` drains the outbox project-agnostically today (verified `syncer.go:78`, comment `syncer.go:174`). Add a filter: skip drained entries whose project is `local-only`/`omitted` (entry stays UNACKED in the outbox). Pull: exclude those projects from the `ListProjects`-driven pull. No outbox schema change. |
| 8 | Flip-transition semantics | **`local-only`→`synced`: outbox drains naturally (now-eligible unacked entries push); pull resumes from the existing per-project cursor. `synced`→`local-only`: stop future push/pull; already-pushed central data is NOT unpublished.** | The "don't ack, don't push" design means a flip needs no migration: eligibility is re-evaluated each drain. `omitted`→anything only affects future writes (omitted never wrote locally). |
| 9 | Config file location | **`%APPDATA%\engram\config.json`** | Windows-first convention (Claude Code et al. use `%APPDATA%`). Token file lives alongside the DB / config dir with user-only ACL. |

### Outbox-accumulation caveat (long-term `local-only` projects)

Because `local-only`/`omitted`-but-already-written entries are **skipped at push time rather than deleted**, a project left `local-only` indefinitely accumulates unacked outbox entries that never drain. This is intentional (so a later flip to `synced` drains everything), but it means the outbox grows unbounded for permanently-local projects. A cleanup/GC job is **out of scope** for this change and flagged as a known follow-up. Note `omitted` does NOT contribute to this growth (omitted refuses the write entirely, so no outbox entry is ever created).

## Affected Areas

| Area | Impact | Description |
|------|--------|-------------|
| `cmd/engram/daemon.go` | Modified | Resident-mode wiring: loopback HTTP server, control API mount, optional `--transport http` (Streamable HTTP MCP) alongside the existing stdio path. stdio path unchanged by default. |
| `cmd/engram/` (new subcommands) | New | `status`, `config`, `projects`, `sync`, `ui` thin-client CLI subcommands; `tray` subcommand (Windows). |
| `internal/control/` (new) | New | Control API handlers, token auth, origin checks, status/config/policy/sync endpoints. |
| `internal/webui/` (new) | New | `go:embed` HTMX templates + static assets, served at `/ui/`. |
| `internal/config/` (new) | New | `config.json` load/save, DPAPI encrypt/decrypt (`//go:build windows`) + env fallback. |
| `internal/tray/` (new, `//go:build windows`) | New | `Shell_NotifyIcon` icon + message pump + context menu; daemon auto-launch. |
| `internal/localstore/schema.go` | Modified | Additive v10 migration: `project_policy` table; bump `currentSchemaVersion` 9→10 + `PRAGMA user_version`. |
| `internal/localstore/` (policy accessors) | New/Modified | Read/write project policy; expose policy to write-path and sync-path. |
| `internal/syncer/syncer.go` (Push, SyncAllProjects) | Modified | Policy-aware push-drain filtering + pull-project exclusion. |
| MCP tool handlers (`mem_save`, `mem_save_prompt`) | Modified | Pre-`LocalWrite` policy check: `omitted` → return error. |
| `go.mod` | Modified | Promote `golang.org/x/sys` from indirect → direct (the single dependency-graph change; zero NEW deps). |
| `old_code/` | Reference only | UI patterns reused (dashboard, autosync status); NEVER modified. |

## Risks

| Risk | Likelihood | Mitigation |
|------|------------|------------|
| `Shell_NotifyIcon` brittleness across Win10/Win11, dark mode, DPI | Med | Minimal monochrome single-color icon avoids most rendering issues; tray is opt-in and non-Windows degrades to `engram ui`. Message pump isolated in `internal/tray/` behind `//go:build windows` so non-Windows builds never compile it. |
| DPAPI is Windows-only | Med (by design) | Explicitly scoped: non-Windows/CI uses env-only key management. `ENGRAM_WRITER_KEY` env always wins. Cross-platform keyring is a separate future change. |
| Control-API security posture (local-privilege-escalation) | Med | Loopback-only bind (`127.0.0.1`), bearer token in a user-ACL'd file next to the DB, `Origin` checks restricting to `http://127.0.0.1:PORT`, `no-store` on API responses. Threat model is reduced to same-machine processes, not network attackers. The writer key is never returned by the config endpoint (redacted). |
| Process-model confusion (stdio vs resident/HTTP) | Med | stdio is and remains the DEFAULT; resident daemon + HTTP MCP are strictly opt-in. Docs spell out the Claude Code HTTP MCP config shape only for opt-in users. No existing stdio config breaks. |
| Outbox unbounded growth for permanent `local-only` projects | Low | Documented caveat; GC deferred. `omitted` does not contribute. Not a v1 blocker. |
| Token-file race / stale token on port change | Low | Token regenerated on bind; port change is restart-required; CLI re-reads the token file each call. |
| Schema v10 migration on a populated store | Low | Additive table only (no data rewrite); migration idempotency tested; `user_version` gate prevents re-runs. |

## Rollback Plan

The change is additive and opt-in. Rollback strategy:
- **Per-PR**: each of the 6 PRs is independently revertible (stacked-to-main, adversarially review-gated, ~≤400 lines).
- **Resident daemon / HTTP transport**: opt-in flags; reverting them leaves stdio (default) untouched — existing Claude Code stdio configs keep working.
- **Schema v10**: additive `project_policy` table; rolling back code leaves the table inert (no reads = no effect). A down-migration is unnecessary; the table is ignored by v9 code paths. If a hard revert is required, drop `project_policy` and reset `PRAGMA user_version = 9`.
- **Config file**: if `config.json` is absent or invalid, the daemon falls back to flags/env (current behavior), so removing the file restores the old config surface.
- **Tray/web UI/control API**: isolated new packages (`internal/control`, `internal/webui`, `internal/tray`, `internal/config`); deleting them + the CLI subcommands restores the pre-change daemon.

## Dependencies

- **`golang.org/x/sys`**: promoted from INDIRECT (go.mod:31, `v0.42.0`) to DIRECT. This is the ONLY dependency-graph change. **Zero new modules added** (Shell_NotifyIcon and DPAPI both live in `golang.org/x/sys/windows`).
- **`mcp-go` v0.44** (already present): `NewStreamableHTTPServer` for the opt-in HTTP MCP transport.
- **HTMX**: vendored as a static asset embedded via `go:embed` (no Go module dependency).
- Pure Go, `CGO_ENABLED=0`, single static binary preserved throughout.

## Success Criteria

> Contract for spec / design / tasks. All must be provable, most headlessly via the control API.

- [ ] **Policy enforcement (headless)**: With central configured, a project set to `local-only` via the control API produces NO central pushes for that project's writes (acceptance test drives policy via `PUT /api/v1/projects/{project}/policy` and asserts central received nothing), while a `synced` project pushes normally — all without the tray or browser.
- [ ] **`omitted` refuses capture**: For an `omitted` project, `mem_save` AND `mem_save_prompt` return an error and write NOTHING locally (no row in `memories`, no outbox entry). Verified by asserting the error and an unchanged local store.
- [ ] **Flip `local-only`→`synced` drains the outbox**: Writes made while `local-only` accumulate unacked outbox entries; after flipping to `synced` and one sync cycle, those entries push to central and are acked — proving "don't ack, don't push" eligibility re-evaluation, with no outbox schema change.
- [ ] **Status reflects connectivity truthfully**: `GET /api/v1/status` reports central as connected/disconnected matching the actual `central_url`/credential state and last sync result; disconnecting via `POST /api/v1/central/disconnect` flips status to disconnected and halts push/pull.
- [ ] **stdio unchanged (no regression)**: The default `engram daemon` (stdio MCP) behaves identically to pre-change; HTTP MCP transport only engages under `--transport http`. Existing daemon tests stay green, including `TestRun_DaemonHelp_DoesNotLeakWriterKey`.
- [ ] **Config round-trip + secret safety**: `config.json` written and re-read yields identical settings; the writer key is DPAPI-encrypted at rest on Windows (decrypts only for the same user), env-only on non-Windows; the config endpoint NEVER returns the key; `--help` never leaks it.
- [ ] **Control-API auth**: requests without the correct bearer token are rejected (401); requests with a disallowed `Origin` are rejected; the API binds only to `127.0.0.1`.
- [ ] **Schema v10 migration idempotent**: migrating a v9 store adds `project_policy`, sets `user_version=10`, defaults existing projects to `synced` (central configured) / `local-only` (no central), and re-running the migration is a no-op.
- [ ] **CLI parity**: `engram status`, `engram projects list/policy`, `engram config get/set`, `engram sync now` produce results equivalent to the corresponding control-API calls; with no daemon running they return a clear "daemon not running" error (no direct-DB fallback).
- [ ] **Windows tray (Windows CI / manual)**: tray icon appears, context menu actions (Open UI, Sync Now, Quit) invoke the matching control-API calls; non-Windows builds compile without the tray package and `engram ui` opens the browser.

## Delivery Plan — 6 Chained PRs (stacked-to-main, each adversarially review-gated, ~≤400 lines)

> Each PR targets `main` and merges in order before the next begins (LOCKED: stacked-to-main). Every PR passes a fresh-context adversarial review.

**PR 1 — `tray-ui/control-api` (~350 lines)** — control plane skeleton
- Resident-daemon mode: `--http-port` flag (default 7700) + loopback HTTP server.
- Control API skeleton: `GET /api/v1/status`, `GET /api/v1/config` (redacted), `GET /api/v1/projects`.
- Bearer-token auth (token file next to DB, user ACL) + origin checks.
- `engram status` and `engram ui` thin-client CLI subcommands.
- Tests: token auth, origin rejection, handler unit tests. No tray, no web UI yet.

**PR 2 — `tray-ui/project-policy` (~300 lines)** — the policy engine
- `project_policy` table (schema v9→v10 migration, default rule baked in).
- Push-time + pull-time filtering in `syncer.Push` / `SyncAllProjects`.
- `omitted` write-refusal in `mem_save` / `mem_save_prompt` handlers.
- Control API: `GET /api/v1/projects` (with policy) + `PUT /api/v1/projects/{project}/policy`; `engram projects list|policy` CLI.
- Tests: policy filtering, flip-drain, omitted-refusal, migration idempotency. (Validates most headless success criteria.)

**PR 3 — `tray-ui/config-file` (~250 lines)** — visual config backend
- `%APPDATA%\engram\config.json` load/save; flags/env remain overrides.
- DPAPI-encrypted writer-key (`//go:build windows`) + env fallback; `ENGRAM_WRITER_KEY` still wins.
- Control API: `PUT /api/v1/config`, `POST /api/v1/central/connect|disconnect`; `engram config get/set`, `engram sync now`.
- Tests: config round-trip, DPAPI encrypt/decrypt (Windows-gated), secret-redaction, connect/disconnect status.

**PR 4 — `tray-ui/web-ui` (~400 lines, chain if larger)** — the browser surface
- `go:embed` HTMX UI: status page, projects list with policy toggles, config form, sync trigger button.
- Served at `GET /ui/...` from the resident daemon; `engram ui` opens the browser.
- Tests: HTTP handler tests for UI routes; CSRF/origin on mutating UI actions.

**PR 5 — `tray-ui/tray` (~250 lines, `//go:build windows`)** — the Windows tray
- `Shell_NotifyIcon` icon + goroutine message pump; context menu: Open UI, Sync Now, Connection status, Quit.
- Auto-launch the resident daemon from the tray; status reflected in the icon/menu.
- `engram tray` subcommand (Windows). Non-Windows builds exclude the package.
- Tests: message-pump / menu-action unit tests (mocked Shell_NotifyIcon).

**PR 6 — `tray-ui/mcp-http-transport` (~200 lines)** — opt-in transport (migration PR)
- `--transport` flag: `stdio` (DEFAULT) or `http` (`NewStreamableHTTPServer`).
- Docs: Claude Code HTTP MCP config shape for opt-in users; stdio remains default.
- Tests: HTTP MCP transport acceptance test; stdio-default regression.

## Disagreements / Tightenings vs. the Exploration

- **`omitted` is now LOCKED to REFUSE-capture** (exploration left it as an open question between "save-locally" and "refuse"). This is a tightening: `omitted` writes nothing locally (no row, no outbox entry), so it does NOT contribute to outbox accumulation — a strictly cleaner story than the exploration's two-option framing.
- **Writer-key persistence is LOCKED to DPAPI on Windows + env fallback**, with `ENGRAM_WRITER_KEY` always winning — chosen over the exploration's safer "env-only, re-enter each restart" alternative because usability won, with the env override preserving the existing no-leak guarantees.
- **Default policy is LOCKED to `synced` when central is configured** (exploration weighed `synced` vs `local-only`); `synced` preserves current behavior exactly and makes privacy opt-out per project.
- **`golang.org/x/sys` indirect→direct promotion is called out explicitly** (exploration noted it in passing). This is the SOLE dependency-graph change; zero new modules.
