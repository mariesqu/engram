# Design: tray-ui — Resident Daemon Control Plane + Per-Project Policy + DPAPI Config + HTMX UI + Windows Tray + HTTP MCP

> Module path: `github.com/mariesqu/engram` (verified from go.mod:1). This design is
> grounded in the CURRENT tree, not `old_code/` (read-only reference for UI patterns only).

## Technical Approach

Stand up an **opt-in resident daemon** that owns the single SQLite file, the autosync `Loop`, and a loopback HTTP server. That server multiplexes three concerns on ONE listener: a versioned JSON **control API** (`/api/v1/...`), an embedded **HTMX web UI** (`/ui/...`), and an optional **Streamable HTTP MCP** transport (`/mcp`). The tray, the browser, and the new CLI subcommands are ALL thin HTTP clients of that control API — there is exactly one authoritative owner of node-local state. stdio MCP (`cmd/engram/daemon.go`) is untouched and remains the default indefinitely; the resident daemon materializes only when the user runs `engram daemon --http`, `engram tray`, or `--transport http`.

Per-project policy (`synced | local-only | omitted`) lives in a new local `project_policy` table (schema v9→v10, additive) and is enforced by **filtering the existing sync paths** — `DrainOutbox` already decodes each entry's project from its canonical payload (sync.go:221), so the push filter is a per-entry policy lookup that simply **skips** (does not ack, does not delete) non-`synced` entries; the pull filter excludes non-`synced` projects from the `ListProjects`-driven loop. `omitted` is stronger: the MCP write handlers check policy BEFORE `AddObservation`/`AddPrompt` and refuse, so nothing is written and no outbox entry is ever created. This is "save locally, decide at the sync boundary": a policy flip re-evaluates eligibility on the next drain with zero outbox surgery.

Config moves to `%APPDATA%\engram\config.json` (flags/env remain higher-precedence overrides) with the writer key DPAPI-encrypted at rest on Windows behind a build-tagged interface (env-only no-op elsewhere). Pure Go, `CGO_ENABLED=0`, single static binary preserved throughout. The SOLE dependency-graph change is promoting `golang.org/x/sys` indirect→direct (go.mod:31).

## Architecture Decisions

| # | Decision | Choice | Rejected | Rationale |
|---|----------|--------|----------|-----------|
| 1 | Process model | Resident daemon as control plane; stdio default; HTTP MCP opt-in | A1 stdio shim / A2 separate coordinator | One process owns SQLite + Loop + control API + UI. Tray/web/CLI are thin clients. A2 leaves multi-process SQLite races and gives policy no owner. LOCKED in proposal. |
| 2 | Control API package | New `internal/controlapi`, mirrors cloudserve middleware/JSON-error patterns | Extend cloudserve | cloudserve is the CENTRAL (Postgres, HMAC, Internet-facing) surface; conflating it with a loopback node-local plane couples two threat models. Borrow the *patterns* (Server struct, `Handler()` mux, `withAuth`, `writeJSON`/`writeError`), not the package. |
| 3 | Control-API auth | Bearer token (32-byte hex) in user-ACL'd `daemon.json` next to DB; `Origin`/`Host` allowlist to `127.0.0.1:PORT`; `Cache-Control: no-store`; bind `127.0.0.1` only | mTLS / OS-pipe / unauth loopback | Loopback bind + token reduces the threat model to same-machine processes. Unauth loopback is unsafe (any local process/browser tab could drive it). mTLS is overkill for localhost. |
| 4 | Policy storage & filter | `project_policy(project PK, policy, updated_at)` v10 table; push filter = per-entry policy lookup in `DrainOutbox` consumer; pull filter = exclude in `SyncAllProjects` | SQL JOIN of outbox↔policy; in-memory cache | Per-entry lookup keeps the outbox project-agnostic and needs no SQL coupling. A small policy map is cached in the store and invalidated on `SetPolicy` (reads dominate; writes are rare UI clicks). |
| 5 | `omitted` enforcement point | MCP write handlers (`handleSave`, `handleSavePrompt`) check policy BEFORE the store write; ALSO gate pull-LISTING so omitted projects never arrive | Refuse inside `LocalWrite`; gate only `ApplyPulled` | Handler-level refusal returns a clean MCP error to the agent and writes nothing (no row, no outbox). Gating pull-listing means an omitted project is never even pulled, so `ApplyPulled` never sees one — no second guard needed for the steady state (a belt-and-suspenders `ApplyPulled` check is added for the flip-to-omitted race; see Failure Modes). |
| 6 | Config precedence | flags > env > `config.json` > built-in default | file > env > flags | Preserves today's behavior (an operator overriding via flag/env must win over a stale file). LOCKED-confirmed. Writer key: `ENGRAM_WRITER_KEY` env always wins; otherwise decrypt from file (Windows) or require env (non-Windows). |
| 7 | Secret at rest | DPAPI `CryptProtectData`/`CryptUnprotectData` via `x/sys/windows` behind `SecretBox` interface; `//go:build windows` impl + non-Windows env-only no-op | Plaintext file / AES-with-derived-key | DPAPI binds to the Windows user, is pure syscall (no CGO), survives restart. Decrypt-fail (user/machine changed) → treat as "no stored key", fall back to env, surface a re-enter prompt in the UI. |
| 8 | UI rendering | `html/template` (stdlib) + `go:embed` + vendored `htmx.min.js`; server-rendered partials | `a-h/templ` (old_code) / React SPA | **DISAGREE with old_code precedent**: old_code uses `templ`, a CODE-GEN dependency requiring a build step. The proposal LOCKED "no JS build step, single static binary." `html/template` needs no codegen and no new module. Same HTMX partial-swap UX. |
| 9 | UI status refresh | HTMX polling (`hx-trigger="every 3s"`) on the status fragment | SSE / WebSocket | Polling a loopback endpoint every few seconds is trivially cheap and needs no long-lived connection bookkeeping in the daemon. SSE adds streaming lifecycle for no benefit at this cardinality. |
| 10 | UI auth | Token→cookie exchange: `GET /ui/` with `?token=` (or a one-time login form) sets an `HttpOnly`, `SameSite=Strict` session cookie; mutations carry a CSRF token (double-submit) | Reuse bearer token in JS | Browsers cannot send a bearer header on a top-level navigation; a cookie is the natural browser session. CSRF double-submit defends the policy/config mutation routes (the token in a hidden form field must match the cookie). |
| 11 | Tray | `Shell_NotifyIcon` via `x/sys/windows`, `//go:build windows`, dedicated `LockOSThread` message-pump goroutine; embedded `.ico` | getlantern/systray / fyne | Only `CGO_ENABLED=0` tray with zero NEW deps. Win32 message pump MUST own a locked OS thread. Non-Windows builds exclude the package entirely. |
| 12 | HTTP MCP transport | `mcp-go` `NewStreamableHTTPServer` mounted at `/mcp` on the SAME listener; stateless mode; token-protected | Separate port / stateful sessions | One listener, path-routed (`/api/v1`, `/ui`, `/mcp`). Stateless avoids server-side session state for a single-user loopback daemon. Token-protected because Claude Code HTTP MCP config supports custom headers. |
| 13 | Schema migration | Additive `migrateV9ToV10` mirroring the existing idempotent pattern (schema.go); seed defaults from central-configured state | Down-migration | v10 is purely additive (new table); v9 code ignores it (rollback-safe). Down-migration is unnecessary; a hard revert drops the table + resets `user_version=9`. |

## Loopback Server Topology (one listener, three concerns)

```
engram daemon --http  (resident mode)
        │
        ├── localstore.Store  (owns the SQLite file — the SAME store stdio uses)
        ├── syncer.Loop       (autosync; gains policy-aware filtering)
        └── http.Server @ 127.0.0.1:<port>   (default 7700)
                 │  ServeMux:
                 ├── /api/v1/...   → controlapi.Server  (bearer-token middleware)
                 │      GET  /api/v1/status
                 │      GET  /api/v1/config            (writer key REDACTED)
                 │      PUT  /api/v1/config
                 │      GET  /api/v1/projects          (each with policy)
                 │      PUT  /api/v1/projects/{project}/policy
                 │      POST /api/v1/central/connect | /api/v1/central/disconnect
                 │      POST /api/v1/sync              (trigger one cycle)
                 ├── /ui/...       → webui handlers    (cookie session + CSRF)
                 │      GET  /ui/  /ui/projects  /ui/config  /ui/status (partial)
                 │      POST /ui/projects/{project}/policy  /ui/config  /ui/sync
                 └── /mcp          → StreamableHTTPServer (opt-in, --transport http)
```

The control plane exposes node-local state through a small port set that `controlapi` depends on (so it is mock-testable and never imports `cmd/engram` glue):

```go
// internal/controlapi ports (satisfied by the wired daemon).
type Store interface {
    ListProjectsWithPolicy() ([]ProjectPolicy, error)
    SetPolicy(project string, p Policy) error
    GetPolicy(project string) (Policy, error)
}
type SyncController interface {
    Status() syncer.Status   // central reachable? last sync result/time?
    TriggerNow(ctx context.Context) error
    Disconnect() error       // halt push/pull
    Reconnect(cfg CentralConfig) error
}
type ConfigStore interface {
    Load() (config.Redacted, error) // never returns the key
    Apply(config.Patch) error       // re-derives credentials; restart-required flags noted
}
```

## Local Store Schema — v10 (additive)

```sql
-- project_policy: per-node policy for each project. There is exactly ONE row per
-- normalized project name. Absence of a row means "use the default rule"
-- (synced when central is configured, local-only otherwise) — the policy is
-- materialized lazily on first read/flip, so a fresh project needs no insert.
CREATE TABLE IF NOT EXISTS project_policy (
  project    TEXT PRIMARY KEY,                 -- normalizeProject() form (lowercased/trimmed)
  policy     TEXT NOT NULL DEFAULT 'synced'
               CHECK (policy IN ('synced','local-only','omitted')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
```

`migrateV9ToV10` follows the established single-transaction `defer tx.Rollback(); return tx.Commit()` pattern (schema.go:600+): `CREATE TABLE IF NOT EXISTS project_policy` (no-op on a fresh DB that `ApplySchema` already created), then `PRAGMA user_version = 10`. `currentSchemaVersion` bumps 9→10; a new `if ver < 10 { migrateV9ToV10(db); ver = 10 }` block is added to `runMigrations`, and the trailing `_ = ver` comment updates to anticipate v11.

> **DESIGN NOTE — default materialization**: We do NOT backfill a row for every existing project at migration time. The default is computed at read time from the central-configured state. This keeps the migration O(1) and means the "default for new projects" rule (synced-with-central / local-only-without) is always correct even as central config changes later. The success criterion "migration defaults existing projects to synced/local-only" is satisfied by the read-time default, asserted via `GetPolicy`/`ListProjectsWithPolicy`, NOT by counting inserted rows.

## Policy Enforcement — the three touch points

**1. Push (the outbox drain).** `DrainOutbox` returns each entry with its `Mutation` reconstructed from the canonical payload, so `Mutation.Project` is available per entry without any schema change (sync.go:221-233). The push filter sits in the drain CONSUMER (a new `policy`-aware variant of `syncer.Push`):

```
for each entry in DrainOutbox:
    pol := store.GetPolicy(entry.Mutation.Project)   // cached map lookup
    if pol != Synced:        // local-only OR omitted
        continue             // SKIP: do NOT central.Apply, do NOT AckMutation
    central.Apply(entry); AckMutation(entry.LocalSeq)
```

Skipped entries stay UNACKED in the outbox. A flip `local-only → synced` makes them eligible on the very next drain — eligibility is re-evaluated each pass, so no migration of outbox rows is needed. This is the mechanism behind the "flip drains the outbox" success criterion.

> **DESIGN CHOICE — per-entry lookup vs SQL join**: Rejected joining `sync_mutations` against `project_policy` in SQL because the outbox stores project only inside the JSON payload (not a column), and `DrainOutbox` already decodes it. A cached policy map (loaded once, invalidated on `SetPolicy`) makes the per-entry check a hash lookup. This keeps `DrainOutbox` itself project-agnostic and unchanged.

**2. Pull (the project loop).** `SyncAllProjects` calls `ListProjects()` then pulls each (syncer.go:193-205). The pull filter excludes non-`synced` projects:

```
for proj in ListProjects():
    if store.GetPolicy(proj) != Synced { continue }   // local-only AND omitted skip pull
    Pull(ctx, n, central, proj)
```

`omitted` projects are excluded from the pull list, so a remote mutation for an omitted project NEVER arrives at `ApplyPulled` in steady state. `local-only` projects also skip pull (they keep local writes but receive nothing from central) — note this means a `synced → local-only` flip stops both push and pull, and already-pushed central data is NOT unpublished (LOCKED).

**3. Write (`omitted` refusal).** In `handleSave` and `handleSavePrompt`, AFTER `resolveSaveProject` resolves the project (tools.go:485) and BEFORE `AddObservation`/`AddPrompt`:

```
project := resolveSaveProject(...)
if store.GetPolicy(project) == Omitted {
    return mcp.NewToolResultError("project %q is omitted: capture refused"), nil
}
... AddObservation(...)   // writes row + enqueues outbox only when not omitted
```

This covers BOTH `mem_save` and `mem_save_prompt` (and the auto prompt-capture inside `handleSave` is naturally skipped because the whole handler returns early). It does NOT touch `LocalWrite` — refusal is a tool-layer policy decision, keeping `localstore` free of policy semantics (the store enforces convergence invariants; policy is a node concern above it).

## Token & Daemon Discovery — `daemon.json`

A single discovery file, written by the resident daemon at startup and read by CLI/tray, lives next to the DB (same directory, same user-only ACL approach as the existing DB file):

```json
// %APPDATA%\engram\daemon.json   (0600 / Windows user-only ACL)
{ "port": 7700, "token": "<64 hex chars>", "pid": 12345, "started_at": "..." }
```

- **Generation**: 32 random bytes → hex (64 chars), `crypto/rand`. Written atomically (temp file + rename) with a restrictive ACL BEFORE the listener accepts connections.
- **Rotation**: token is regenerated on every daemon start (stable for the daemon's lifetime). Port change is restart-required (a running daemon owns its port).
- **Discovery**: CLI and tray read `daemon.json` on each invocation to learn `(port, token)`. No env var needed for the common case.
- **Concurrent-CLI-read concern (proposal-flagged)**: because the daemon writes `daemon.json` atomically (rename), a CLI either sees the complete old file or the complete new file — never a torn write. A CLI that read a stale token (daemon restarted mid-call) gets a 401 and re-reads the file once before failing with "daemon not running / restart in progress."
- **Windows ACL**: set the file DACL to the current user only (no inherited ACEs) via `x/sys/windows` (`SetNamedSecurityInfo`) in the Windows build; on non-Windows, `0600` perms. Encapsulated in the same `//go:build` split as the secret box so the control-API code is OS-agnostic.

## Resident Daemon Lifecycle

- **Flag shape**: extend the existing `daemon` subcommand with `--http` (enable resident mode) and `--http-port` (default 7700, or `config.json` `http.port`). stdio remains when `--http` is absent. `--transport http` (PR6) additionally mounts `/mcp` and, when set, the stdio listener is replaced by the HTTP MCP transport on the same server.
- **Bind & collision**: bind `127.0.0.1:<port>`. If the port is in use, probe `GET /api/v1/status` with the token from `daemon.json`: if a healthy engram daemon answers, this invocation REFUSES with "daemon already running on :PORT (pid N)" and exits non-zero (no second owner of the SQLite file). If the port is held by something else, fail with a clear bind error.
- **Tray auto-launch**: the tray, on start, reads `daemon.json` and pings status; if absent/unreachable it spawns `engram daemon --http` detached (Windows `CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS` via `os/exec` + `SysProcAttr`), waits for `daemon.json` + a healthy status (bounded retry), then drives it as a client.
- **Crash/restart**: the daemon is stateless beyond the SQLite file; a supervisor (or the tray re-launching) restarts it. On restart it rewrites `daemon.json` with a fresh token. No PID-file locking beyond the port-probe owner check (SQLite's `SetMaxOpenConns(1)` + WAL already serialize the single owner).

## Config Package — `internal/config`

```
internal/config/
├── config.go        # Config struct, Load (file→merge), Save (atomic), Redact, Patch/Apply, precedence
├── secret.go        # SecretBox interface: Seal([]byte) ([]byte, error); Open([]byte) ([]byte, error)
├── secret_windows.go  //go:build windows  — DPAPI CryptProtectData/CryptUnprotectData via x/sys/windows
└── secret_other.go    //go:build !windows — env-only no-op: Seal/Open return ErrNoSecretStore
```

- **Shape**: `Config{ DB, Central{URL, WriterID}, EncryptedWriterKey []byte, HTTP{Port}, SyncInterval }`. The writer key is stored as DPAPI ciphertext; it is decrypted only when assembling sync credentials, never logged, never serialized in `Redact()`.
- **Precedence (LOCKED)**: flags > env > file > default, applied as a merge in the daemon's config resolution (replacing the env-only resolution in daemon.go:115-167). The existing "resolve env AFTER flag.Parse so secrets never enter flag defaults" guarantee is preserved (`--help` must not leak the key — `TestRun_DaemonHelp_DoesNotLeakWriterKey` stays green).
- **Atomic writes**: marshal → write temp file in the same dir → `os.Rename` over `config.json`. Same atomicity as `daemon.json`.
- **DPAPI decrypt failure** (user/machine changed, or ciphertext from another profile): `Open` returns an error → config treats the stored key as absent → falls back to `ENGRAM_WRITER_KEY` env → if also absent, central credentials are unavailable and the status endpoint reports "writer key required" so the UI prompts a re-enter. Never a hard crash.

## Web UI — `internal/webui`

```
internal/webui/
├── embed.go         # //go:embed templates static  → two embed.FS
├── webui.go         # Mount(mux, deps): registers /ui routes, session cookie, CSRF
├── render.go        # html/template parse-on-init; renderPartial / renderPage helpers
├── templates/       # layout.html, status.html (partial), projects.html, config.html
└── static/          # htmx.min.js (vendored), styles.css
```

- **Routes**: `GET /ui/` (full page), `GET /ui/status` (HTMX partial, `hx-trigger="every 3s"`), `GET /ui/projects`, `POST /ui/projects/{project}/policy` (toggle), `GET /ui/config`, `POST /ui/config`, `POST /ui/sync`. Mutating routes return the refreshed partial (HTMX swap) or `HX-Redirect`.
- **Auth flow**: tray "Open UI" opens `http://127.0.0.1:PORT/ui/?token=<token>`; the handler validates the token against `daemon.json`, sets an `HttpOnly`/`SameSite=Strict`/`Secure=false`(loopback) session cookie, and strips the token from the URL via redirect. Subsequent navigation uses the cookie.
- **CSRF**: a per-session CSRF token is embedded as a hidden field in every form and as a cookie; mutating POSTs validate the double-submit match. `Origin`/`Host` are additionally checked against `127.0.0.1:PORT`.
- **Server-rendered, no JS build**: `html/template` auto-escapes; HTMX (one vendored JS file) drives partial swaps. No `templ`, no React, no bundler.

## Tray — `internal/tray` (`//go:build windows`)

```
internal/tray/
├── tray_windows.go  # Shell_NotifyIcon add/modify/delete; WndProc; menu build; client calls
├── pump_windows.go  # message loop on a LockOSThread'd goroutine
├── icon.go          # //go:embed engram.ico
└── tray_stub.go     # //go:build !windows — Run returns ErrUnsupported (engram ui fallback)
```

- **Threading**: the Win32 message pump (`GetMessage`/`TranslateMessage`/`DispatchMessage`) MUST run on a single OS thread for the lifetime of the window. The pump goroutine calls `runtime.LockOSThread()` first and never unlocks. All `Shell_NotifyIcon` and window calls happen on that thread; menu-action handlers POST to the control API via a separate goroutine (or a buffered channel) so a slow HTTP call never blocks the pump.
- **Menu → control-API mapping**: "Open UI" → launch browser at `/ui/?token=`; "Sync Now" → `POST /api/v1/sync`; "Connection: <status>" (disabled label, refreshed from `GET /api/v1/status`); "Quit" → stop pump + (optionally) signal the daemon. The icon glyph/tooltip reflects connected/disconnected/syncing from polled status.
- **Behavior matrix**: (daemon running, central configured) → normal; (daemon running, no central) → "local-only" indicator, connect via UI; (daemon not running) → tray auto-launches it; (launch fails) → balloon error + "Open UI" disabled.
- **Icon**: a single embedded monochrome `.ico` avoids most DPI/dark-mode rendering issues.

## HTTP MCP Transport (PR6)

- **Flag**: `--transport stdio|http` (default `stdio`). With `http`, the daemon mounts `mcp-go`'s `NewStreamableHTTPServer(mcpServer)` at `/mcp` on the same loopback listener instead of `ServeStdio`.
- **Shared listener**: `/api/v1`, `/ui`, `/mcp` are distinct path prefixes on one `http.Server` (route via the top-level mux). No second port.
- **Auth interplay**: `/mcp` is token-protected with the same bearer token (Claude Code HTTP MCP config supports custom headers, e.g. `Authorization: Bearer <token>`). Docs spell out the config block for opt-in users.
- **Stateless mode**: chosen for a single-user loopback daemon — no server-side MCP session table. stdio remains the documented default; this transport is purely additive.

## Module & Package Layout

```
cmd/engram/
├── daemon.go            # MODIFIED: --http / --http-port resident wiring; --transport (PR6)
├── controlclient.go     # NEW: thin HTTP client (reads daemon.json) for CLI subcommands
├── status.go config.go projects.go sync.go ui.go   # NEW: CLI thin-client subcommands
├── tray.go              # NEW: `engram tray` (delegates to internal/tray; stub off-Windows)
└── tools.go             # MODIFIED: omitted-refusal in handleSave / handleSavePrompt
internal/
├── controlapi/          # NEW: Server, Handler() mux, bearer+origin middleware, JSON handlers
├── webui/               # NEW: html/template + go:embed HTMX UI
├── config/              # NEW: config.json load/save/merge + SecretBox (DPAPI / env)
├── tray/                # NEW: //go:build windows Shell_NotifyIcon + message pump
├── localstore/
│   ├── schema.go        # MODIFIED: v10 migration + project_policy DDL
│   └── policy.go        # NEW: GetPolicy/SetPolicy/ListProjectsWithPolicy + cache
└── syncer/
    ├── syncer.go        # MODIFIED: policy-aware Push drain + SyncAllProjects pull filter
    └── loop.go          # MODIFIED: expose Status() for the control API
go.mod                   # MODIFIED: golang.org/x/sys indirect → direct (sole change)
```

## Failure Modes & Mitigations

| Failure | Effect | Mitigation |
|---------|--------|------------|
| Port 7700 already owned by an engram daemon | Two SQLite owners would race | Startup probes `GET /api/v1/status`; a healthy engram answer → REFUSE + exit non-zero. |
| Port owned by a non-engram process | Bind fails | Clear bind error; suggest `--http-port`. |
| Stale `daemon.json` token (daemon restarted mid-call) | CLI 401 | CLI re-reads `daemon.json` once on 401, then errors "daemon not running / restarting." |
| DPAPI decrypt fails (user/machine change) | Stored key unusable | Treat as absent → env fallback → status reports "writer key required"; UI re-enter. Never crash. |
| Flip `synced → omitted` while a mutation for that project is mid-pull | A row could land for an omitted project | Pull-listing excludes omitted (steady state); a belt-and-suspenders `ApplyPulled` policy check drops omitted mutations during the flip race. |
| Outbox unbounded growth for permanent `local-only` projects | Disk creep | Documented caveat; GC deferred (out of scope). `omitted` does not contribute (no write). |
| Message pump blocks on a slow control-API call | Tray UI freezes | Menu handlers dispatch HTTP off the pump thread (goroutine/channel); pump only does Win32 calls. |
| Browser tab on another origin tries to drive `/api/v1` | Local CSRF / drive-by | `Origin`/`Host` allowlist + bearer token (API) and CSRF double-submit (UI). |
| Token leaks via `--help`/logs | Secret exposure | Token only in `daemon.json` (ACL'd); never a flag default; config `Redact()` strips the key; existing no-leak daemon test stays green. |

## Testing Strategy (per PR — headless discipline)

| PR | Layer | Approach |
|----|-------|----------|
| 1 control-api | Acceptance (httptest) | Spin the real control-API `Handler()` over a real temp SQLite store; assert token-missing→401, bad `Origin`→403, bind is `127.0.0.1`, status/config(redacted)/projects shapes. CLI `status`/`ui` against the live handler. |
| 2 project-policy | Acceptance + unit | Real store + in-process central: `PUT policy=local-only` → push sends nothing for that project, `synced` pushes; `omitted` `mem_save`/`mem_save_prompt` error + assert zero rows/zero outbox; flip `local-only→synced` then one sync drains the outbox; v10 migration idempotency (run twice). |
| 3 config-file | Acceptance + Windows-gated | config.json round-trip identical; DPAPI seal/open round-trip (`//go:build windows` test); `Redact()` never returns the key; connect/disconnect flips status. |
| 4 web-ui | HTTP handler tests | `/ui/*` route renders; policy toggle POST mutates + returns partial; CSRF/origin rejection on mutating routes; token→cookie exchange. |
| 5 tray | Headless unit + manual | Test the menu MODEL and menu-action→control-API-call MAPPING (mocked HTTP client, behavior matrix); `Shell_NotifyIcon` syscalls are mocked. Manual checklist: icon appears, actions fire, non-Windows compiles without the package. |
| 6 mcp-http-transport | Acceptance + regression | HTTP MCP transport end-to-end against the mounted `/mcp`; `--transport stdio` (default) regression incl. `TestRun_DaemonHelp_DoesNotLeakWriterKey`. |

## Disagreements / Tightenings vs. Proposal & Exploration

- **UI engine: `html/template` (stdlib), NOT `templ`** — the old_code dashboard precedent uses `a-h/templ`, a code-gen dependency with a build step. The proposal LOCKED "no JS build step, single static binary, zero new modules." Reusing `templ` would violate that (new module + `go generate` step). `html/template` + go:embed delivers the same HTMX partial-swap UX with NO new dependency. **This is the one place I deliberately diverge from the cited precedent.**
- **Package boundary: new `internal/controlapi`, NOT extending `cloudserve`** — cloudserve is the central/Postgres/HMAC/Internet surface. The control plane is loopback/SQLite/bearer-token/single-user. Same patterns, separate package, separate threat model. (Proposal said `internal/control`; I name it `controlapi` to avoid ambiguity with Go's reserved-ish "control" and to read as a noun — a minor naming tightening.)
- **Default policy is read-time computed, not backfilled at migration** — the success criterion's wording ("migration defaults existing projects to synced/local-only") is satisfied by a read-time default keyed on central-configured state, keeping the migration O(1) and correct even when central config changes later. Asserted via `GetPolicy`, not row counts.
- **`omitted` belt-and-suspenders on `ApplyPulled`** — the proposal locates enforcement at write + push + pull-listing. I ADD a defensive `ApplyPulled` policy check to close the brief flip-to-omitted race where an in-flight pull could otherwise land a row. Cheap, and it makes "omitted writes nothing locally" hold even under concurrent flips.
- **Tray HTTP calls strictly off the pump thread** — not contradicted by the proposal, but a hard design constraint I am elevating: the Win32 message pump must never block on an HTTP round-trip, or the tray (and potentially the shell) hangs.

## Open Questions (do not block tasks)

- [ ] Exact Windows ACL API surface for `daemon.json`/`config.json` (`SetNamedSecurityInfo` vs creating with a prebuilt SDDL) — pick during PR1/PR3 apply; both are `x/sys/windows`, no new dep.
- [ ] Whether `engram tray` should, on "Quit", also stop a daemon it auto-launched vs leave it running — default leave-running; revisit if users expect Quit to fully shut down.
```
