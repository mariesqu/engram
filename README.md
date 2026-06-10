# engram

engram is a local-first memory engine for AI coding agents. It exposes a persistent observation store over the [Model Context Protocol](https://modelcontextprotocol.io/) (MCP) stdio transport, backed by SQLite. An agent that uses engram accumulates memories across sessions — decisions, bug fixes, architectural choices, gotchas — and can search or retrieve them in future sessions without re-reading the codebase.

The daemon runs as a single process on the developer's machine. It requires no server, no cloud account, and no network access for basic use. When multiple machines or team members need to share the same memory corpus, an optional central sync layer (Postgres + HTTP + HMAC) replicates mutations across nodes with last-write-wins reconciliation and per-writer authentication.

engram is pure Go (no CGO) and has no binary dependencies beyond the compiled binary itself.

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│  AI coding agent (Claude Code, OpenCode, …)             │
│                                                         │
│  MCP tools: mem_save, mem_search, mem_context, …        │
└──────────────────────┬──────────────────────────────────┘
                       │ MCP stdio (JSON-RPC)
                       ▼
┌─────────────────────────────────────────────────────────┐
│  engram daemon                                          │
│  localstore (SQLite, WAL, FTS5, auto-migrate)           │
└──────────────────────┬──────────────────────────────────┘
                       │ HTTP + HMAC-SHA256  (optional)
                       ▼
┌─────────────────────────────────────────────────────────┐
│  engram central server                                  │
│  centralstore (Postgres, auto-migrate)                  │
└─────────────────────────────────────────────────────────┘
```

In local-only mode the bottom tier is absent. The daemon writes only to the local SQLite file and no network traffic occurs.

## Features

- **9 MCP tools** for session tracking, memory write, search, and conflict resolution
- **SQLite store** — single file, WAL mode, FTS5 full-text search, automatic schema migration on open
- **Local-only mode** — no network, no credentials required
- **Optional central sync** — push/pull over HTTP with HMAC-SHA256 per-writer authentication
- **Autosync** — configurable interval (default 30 s) plus immediate trigger on every write
- **Conflict detection** — post-save BM25 candidate scoring; `mem_judge` for verdict recording
- **Prompt capture** — `mem_save_prompt` persists the user's prompt; `mem_save` auto-attaches it
- **Project auto-detection** — resolves project name from git remote, repo root, or `.engram/config.json`
- **Pure Go, CGO_ENABLED=0** — no C toolchain required

## Build from source

Requires Go 1.26.1 or later (see `go.mod`).

```bash
git clone https://github.com/mariesqu/engram.git
cd engram
CGO_ENABLED=0 go build -o engram ./cmd/engram
```

The resulting `engram` binary is statically linked and self-contained.

For a smaller, stripped release binary:

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o engram ./cmd/engram
```

## Quickstart: local-only mode

No server, no credentials. All data stays in a local SQLite file.

```bash
# Build
CGO_ENABLED=0 go build -o engram ./cmd/engram

# Start the daemon (blocks; connect your MCP client before interacting)
./engram daemon --db ~/.engram/memories.db
```

The daemon logs to stderr and listens for MCP JSON-RPC on stdin/stdout. Your MCP client (see [Wiring into an MCP client](#wiring-into-an-mcp-client)) connects to it via stdio.

The SQLite file is created automatically on first run. Schema migrations are applied on every open; existing data is preserved.

## Quickstart: central sync

Central sync requires a running Postgres instance and an `engram serve` process. Each writer node needs its own HMAC key.

### 1. Start the central server

```bash
export ENGRAM_DSN="postgres://user:pass@localhost:5432/engram?sslmode=disable"
./engram serve --addr :8080
```

`engram serve` applies the Postgres schema automatically on startup. TLS must be terminated upstream (load balancer, reverse proxy); the server listens on plain HTTP.

### 2. Provision a writer key

Run this once per node (or per machine/agent identity). The key is printed once and never stored in plaintext — save it to a secret manager immediately.

```bash
./engram keys provision --dsn "$ENGRAM_DSN" my-laptop
# Output:
# provisioned writer "my-laptop"
# key (hex): <64 hex chars>
# WARNING: this is the writer's HMAC secret — shown ONCE.
# Store it securely and configure the node's client with it.
```

To revoke a key:

```bash
./engram keys revoke --dsn "$ENGRAM_DSN" my-laptop
```

### 3. Start the daemon with sync enabled

```bash
export ENGRAM_WRITER_KEY="<hex key from step 2>"

./engram daemon \
  --db ~/.engram/memories.db \
  --central-url http://your-central-host:8080 \
  --writer-id my-laptop \
  --sync-interval 30s
```

`ENGRAM_WRITER_KEY` is environment-only and never a CLI flag. This prevents the 64-character secret from appearing in `--help` output, process lists, or shell history.

On Windows (PowerShell):

```powershell
$env:ENGRAM_WRITER_KEY = "<hex key>"
.\engram.exe daemon --db "$env:APPDATA\engram\memories.db" `
  --central-url http://your-central-host:8080 `
  --writer-id my-laptop
```

## Resident daemon

The `--http` flag switches the daemon from stdio MCP mode to a long-running HTTP control plane. In this mode the daemon:

- Binds to `127.0.0.1:<port>` (default `7700`; override with `--http-port`)
- Writes a `daemon.json` discovery file next to the database on startup
- Removes `daemon.json` on clean shutdown (SIGINT / SIGTERM)
- Refuses to start if another healthy engram daemon is already running on the same port

```bash
# Start the resident daemon (stays running; connect via engram status / engram ui)
./engram daemon --db ~/.engram/memories.db --http

# Custom port
./engram daemon --db ~/.engram/memories.db --http --http-port 8800
```

### daemon.json

`daemon.json` is written atomically (temp-file + rename) next to the database file. It contains the port, bearer token, PID, and start timestamp. The file is user-read-only (`0600` permissions; DACL user-only on Windows). CLI subcommands (`engram status`, `engram ui`) read it to connect — no environment variables required.

```json
{
  "port": 7700,
  "token": "<64 hex chars>",
  "pid": 12345,
  "started_at": "2026-06-01T10:00:00Z"
}
```

The bearer token rotates on every daemon start. CLI clients re-read `daemon.json` automatically on a `401` response.

### Config file

The resident daemon persists its configuration to a `config.json` file in a platform-specific directory:

| Platform | Default location                                          |
|----------|-----------------------------------------------------------|
| Linux    | `$XDG_CONFIG_HOME/engram/config.json` (or `~/.config/engram/config.json`) |
| macOS    | `~/Library/Application Support/engram/config.json`        |
| Windows  | `%APPDATA%\engram\config.json`                            |

Override the directory with `ENGRAM_CONFIG_DIR`.

The file is written atomically (temp file + rename) so a crash during a write never produces a partial read. Absent file is not an error — the daemon uses defaults.

**Writer key storage:** on Windows the writer key is encrypted with DPAPI (user-scope `CryptProtectData`) before being written to `config.json`. The ciphertext is base64-encoded in the `encrypted_writer_key` field. The plaintext never touches disk. On other platforms the writer key must be supplied via `ENGRAM_WRITER_KEY` each time; it is not stored in `config.json`.

`ENGRAM_WRITER_KEY` always takes precedence over the file-stored (DPAPI-encrypted) key.

**Precedence order (highest to lowest):**

1. CLI flags
2. Environment variables (`ENGRAM_WRITER_KEY`, `ENGRAM_CENTRAL_URL`, …)
3. `config.json`
4. Built-in defaults

### Control API

All endpoints require `Authorization: Bearer <token>`. Responses include `Cache-Control: no-store`.

| Method  | Path                                       | Description                                            |
|---------|--------------------------------------------|--------------------------------------------------------|
| `GET`   | `/api/v1/status`                           | Connection state, last sync result, version            |
| `GET`   | `/api/v1/config`                           | Redacted daemon configuration (writer key masked)      |
| `PUT`   | `/api/v1/config`                           | Patch runtime-mutable or restart-required config       |
| `GET`   | `/api/v1/projects`                         | List of projects with their effective policy           |
| `PUT`   | `/api/v1/projects/{project}/policy`        | Set the sync policy for a project                      |
| `POST`  | `/api/v1/central/connect`                  | Connect to a central server (seals writer key)         |
| `POST`  | `/api/v1/central/disconnect`               | Disconnect from central (clears credentials)           |
| `POST`  | `/api/v1/sync/trigger`                     | Trigger an immediate sync cycle (202; 409 if offline)  |

Mutating endpoints (`PUT`, `POST`) additionally require an `Origin: http://127.0.0.1:<port>` header.

### CLI subcommands

```bash
# Print status of the running resident daemon
./engram status --db ~/.engram/memories.db

# Open the web UI in the default browser
./engram ui --db ~/.engram/memories.db

# Read current daemon configuration (writer key is redacted)
./engram config get --db ~/.engram/memories.db

# Change a runtime-mutable setting (takes effect immediately, no restart)
./engram config set sync_interval 45s --db ~/.engram/memories.db
./engram config set log_level debug    --db ~/.engram/memories.db

# Trigger an immediate sync cycle
./engram sync now --db ~/.engram/memories.db
```

All subcommands read `daemon.json` from the same directory as `--db`. If no daemon is running, they exit non-zero with a clear error message.

**Config keys**

| Key             | Mutable at runtime | Description                                    |
|-----------------|--------------------|------------------------------------------------|
| `sync_interval` | Yes                | Autosync cadence (Go duration, e.g. `30s`)     |
| `log_level`     | Yes                | Log verbosity: `debug`, `info`, `warn`, `error`|
| `db_path`       | No (restart)       | Path to the local SQLite database              |
| `http_port`     | No (restart)       | Control API TCP port                           |
| `transport`     | No (restart)       | MCP transport mode: `stdio` or `http`          |

`writer_key` and `central_url` are managed exclusively via `engram central connect / disconnect` — they are rejected by `PUT /api/v1/config`.

### HTTP MCP transport

By default, `engram daemon --http` runs the MCP layer over **stdio** — the daemon still expects a JSON-RPC client on stdin/stdout even while the HTTP control plane is active. If you run multiple MCP clients against a single resident daemon (e.g. Claude Code, Cursor, and a CI bot all sharing the same memory store), you can switch to the Streamable HTTP MCP transport instead:

```bash
# Expose MCP over HTTP on the same loopback listener as the control API
./engram daemon --db ~/.engram/memories.db --http --transport http
```

With `--transport http` the daemon mounts the MCP server at `/mcp` on the **same port** as the control API — no second port is opened. The endpoint uses [Streamable HTTP (stateless mode)](https://modelcontextprotocol.io/specification/2025-03-26/basic/transports#streamable-http) so each MCP request is an independent HTTP round-trip; no persistent SSE connection is required.

`--transport http` requires `--http`. Running it without `--http` is a startup error.

**Authentication**

`/mcp` requires the same bearer token as `/api`:

```
Authorization: Bearer <token from daemon.json>
```

Requests without a valid `Authorization` header receive `401 Unauthorized`. The token rotates on every daemon start; read it fresh from `daemon.json`.

**Wiring Claude Code**

Add an entry to your `claude_desktop_config.json` (or equivalent MCP host config):

```json
{
  "mcpServers": {
    "engram": {
      "type": "http",
      "url": "http://127.0.0.1:7700/mcp",
      "headers": {
        "Authorization": "Bearer <token>"
      }
    }
  }
}
```

Replace `7700` with the actual port and `<token>` with the value from `daemon.json`. Re-read `daemon.json` after every daemon restart — the token is rotated on start.

The `"type": "http"` value matches the Claude Code `.mcp.json` schema; other MCP clients may name the Streamable HTTP transport differently (e.g. `"streamable-http"`) — check your client’s documentation.

**Mode matrix**

| Flags | MCP transport | `/mcp` endpoint |
|-------|---------------|-----------------|
| _(none — default)_ | stdio | absent |
| `--http` | stdio | absent |
| `--http --transport http` | Streamable HTTP | present, token-gated |

`--transport stdio` (explicit) is identical to the default; stdio is never removed or altered by `--transport http`.

### Per-project policy

Each project has an effective sync policy that controls how its observations move between the local store and central. The policy is evaluated at push/pull time — changing it takes effect on the next sync cycle with no schema migration.

| Policy      | Default when                   | Behaviour                                                                                      |
|-------------|-------------------------------|-----------------------------------------------------------------------------------------------|
| `synced`    | Central is configured         | Normal bidirectional sync: local writes are pushed to central; central writes are pulled down. |
| `local-only`| Central is **not** configured | Observations are written to the local SQLite file only. Push and pull are suppressed. Accumulated outbox entries drain automatically if the policy is later flipped to `synced`. |
| `omitted`   | Never (explicit only)         | `mem_save` and `mem_save_prompt` refuse writes for this project with a clear MCP error. Nothing is written, no outbox entry is created. |

**Flip behaviour**

- `local-only` → `synced`: queued outbox entries drain on the next push cycle. No manual intervention required.
- `synced` → `local-only`: push and pull stop immediately. Observations already on central remain there unchanged.
- Any → `omitted`: future writes are refused. Pre-existing data is unaffected.

**CLI**

List all known projects and their effective policies:

```bash
engram projects list --db ~/.engram/memories.db
# OUTPUT:
# PROJECT        POLICY
# my-project     synced
# private-notes  local-only
```

Set the policy for a project:

```bash
engram projects policy my-project local-only --db ~/.engram/memories.db
engram projects policy my-project synced      --db ~/.engram/memories.db
engram projects policy my-project omitted     --db ~/.engram/memories.db
```

The `--db` flag can be replaced by the `ENGRAM_DB` environment variable for all `projects` subcommands.

**Control API**

```
GET  /api/v1/projects                        → list of {name, policy} for all known projects
PUT  /api/v1/projects/{project}/policy       → {"policy": "synced|local-only|omitted"}
```

`PUT` requires a valid `Authorization: Bearer <token>` header. The token is read from the `daemon.json` file written by the running daemon.

## Web UI

The resident daemon (`engram daemon --http`) serves a browser-based dashboard at `http://127.0.0.1:<port>/ui/`.

### Opening the UI

```bash
# Opens the default browser at /ui/ — token exchange happens automatically.
engram ui --db ~/.engram/memories.db
```

`engram ui` reads `daemon.json`, constructs a tokenized URL (`/ui/?token=...`), and opens your default browser. The server exchanges the token for an `HttpOnly`, `SameSite=Strict` session cookie and redirects to `/ui/` — the token leaves the address bar immediately.

### Available surfaces

| Path | Surface | Auth |
|------|---------|------|
| `/ui/` | **Status page** — central connected state, last sync result (pushed/pulled counts, error), daemon version. Auto-refreshes every 3 s via HTMX polling. | session cookie |
| `/ui/status` | **Status partial** — HTMX polling fragment (no full page wrapper). | session cookie |
| `/ui/projects` | **Projects** — table of all known projects with their effective policy badge (`synced`, `local-only`, `omitted`) and a per-row policy selector. | session cookie |
| `/ui/config` | **Configuration** — editable `sync_interval`; `central_url` and `writer_key` shown read-only (managed via the connect/disconnect actions on the Status page). | session cookie |

### Mutating actions (PR-④b)

All mutating actions (POST) require both a valid session cookie **and** a CSRF double-submit token. The CSRF token is embedded as a hidden field in every form and sent as an `X-CSRF-Token` header by HTMX.

| Action | Route | Notes |
|--------|-------|-------|
| Change project policy | `POST /ui/projects/{name}/policy` | Calls `Store.SetPolicy`; returns refreshed projects rows via HTMX swap. |
| Update sync interval | `POST /ui/config` | Validates Go duration server-side; returns updated config form. Shows restart-required notice when applicable. |
| Trigger sync now | `POST /ui/sync` | Calls `SyncController.TriggerNow`; button hidden/disabled when central is not connected. Returns status partial. |
| Disconnect from central | `POST /ui/disconnect` | Calls `SyncController.Disconnect`; includes HTMX confirm dialog. Returns status partial. |
| Connect to central | `POST /ui/connect` | Fields: `central_url`, `writer_id`, `writer_key` (password input). Calls `SyncController.Reconnect`. On failure, shows a friendly error (`invalid writer_key` or `credential validation failed`); **writer_key is never echoed back**. |

### Session and security

- **Session cookie** (`engram_session`): `HttpOnly`, `SameSite=Strict`, `Secure=false` (loopback — no TLS on localhost), scoped to `Path=/ui/`.
- **CSRF cookie** (`engram_csrf`): NOT `HttpOnly` (double-submit pattern — the template reads it to populate the hidden field), `SameSite=Strict`, `Path=/ui/`. Set at the same time as the session cookie.
- **CSRF validation**: every mutating POST must carry a matching `X-CSRF-Token` header or `csrf_token` form field; constant-time comparison against the CSRF cookie value. Missing or mismatched → 403.
- **Origin check**: mutating POSTs with an `Origin` header must match `http://127.0.0.1:<port>` exactly. Absent `Origin` is allowed (non-browser CLI clients). Present-mismatched → 403.
- Token exchange: `GET /ui/?token=<bearer>` validates the token, issues both the session and CSRF cookies atomically, then redirects to `/ui/` — the token leaves the address bar immediately.
- If the session expires, the browser shows a 401 page with a hint to re-run `engram ui`.
- Static assets (`htmx.min.js`, `styles.css`) are fully embedded in the binary — no CDN, no internet access required.
- HTMX version 2.0.4 is vendored at `internal/webui/static/htmx.min.js`.
- **writer_key no-echo guarantee**: the raw writer key is used only to call `SyncController.Reconnect` and is never stored, logged, or present in any HTTP response body.

## Windows tray

On Windows, `engram tray` provides a persistent system-tray icon that gives quick access to the daemon status and common actions.

### Starting the tray

```powershell
.\engram.exe tray --db "$env:APPDATA\engram\memories.db"
```

Or with `ENGRAM_DB` set:

```powershell
$env:ENGRAM_DB = "$env:APPDATA\engram\memories.db"
.\engram.exe tray
```

### Daemon auto-launch

If the resident daemon is not already running when `engram tray` is invoked, the tray will:

1. Spawn `engram daemon --http --db <path>` as a fully detached background process (`DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP`).
2. Wait for `daemon.json` to appear next to the database file.
3. Poll `GET /api/v1/status` until a valid engram response is received (bounded: 10 s, 500 ms intervals).
4. Display the tray icon once the daemon is healthy.

If the daemon is already running and healthy, the tray attaches to it without starting a second instance.

### Menu items

| Item | Condition | Action |
|------|-----------|--------|
| **Connected** / **Disconnected** | always | Status label — non-interactive |
| **Open UI** | daemon running | Opens the browser at `http://127.0.0.1:<port>/ui/` with the session token pre-loaded |
| **Connect to central** | not connected | Opens the web UI to the connect form |
| **Disconnect from central** | connected | Calls `POST /api/v1/central/disconnect` — stops sync, clears credentials |
| **Sync Now** | connected | Calls `POST /api/v1/sync/trigger` — triggers an immediate sync cycle |
| **Quit** | always | Stops the tray process (daemon continues running in the background) |

The status label and **Sync Now** enabled/disabled state refresh every 5 seconds from a background poll of `GET /api/v1/status`.

### Threading model

The Win32 message pump runs on a dedicated goroutine that holds `runtime.LockOSThread()` for its entire lifetime — Win32 window and message APIs are thread-affine and require all calls to happen on the same OS thread. Menu actions (HTTP calls, browser launches) are dispatched to a separate worker goroutine via a buffered channel so the pump thread never blocks on I/O.

### Non-Windows

The tray subcommand is Windows-only. On Linux and macOS, use `engram ui` to open the web dashboard in your default browser:

```bash
engram ui --db ~/.engram/memories.db
```

### Icon

The tray icon is a minimal monochrome 16×16 + 32×32 ICO file embedded in the binary. The file is located at `internal/tray/engram.ico` and was generated by `internal/tray/gen_ico/gen_ico.go` using only the Go standard library (no external tools required). To regenerate:

```bash
cd internal/tray && go run gen_ico/gen_ico.go
```

The generator writes a standard ICO file with two BITMAPINFOHEADER + XOR/AND bitmap images. The glyph is a centered solid square visible at both sizes on any DPI setting.

## CLI reference

```
engram serve    [--addr <addr>] [--dsn <dsn>]
engram keys     provision [--dsn <dsn>] <writer-id>
engram keys     revoke    [--dsn <dsn>] <writer-id>
engram daemon   [--db <path>] [--central-url <url>] [--writer-id <id>] [--sync-interval <dur>] [--http] [--http-port <port>]
engram status   [--db <path>]
engram ui       [--db <path>]
engram tray     [--db <path>]  (Windows only)
engram projects list   [--db <path>]
engram projects policy [--db <path>] <project> <policy>
engram config   get          [--db <path>]
engram config   set <key> <value>  [--db <path>]
engram sync     now          [--db <path>]
```

### Environment variables

| Variable              | Used by                              | Default  | Description                                                      |
|-----------------------|--------------------------------------|----------|------------------------------------------------------------------|
| `ENGRAM_ADDR`         | `serve`                              | `:8080`  | Listen address for the central HTTP server                       |
| `ENGRAM_DSN`          | `serve`, `keys`                      | —        | Postgres DSN (required)                                          |
| `ENGRAM_DB`           | `daemon`, `status`, `ui`, `tray`, `projects`, `config`, `sync` | — | Path to the local SQLite database (required) |
| `ENGRAM_CENTRAL_URL`  | `daemon`                             | —        | Central server URL; omit for local-only mode                     |
| `ENGRAM_WRITER_ID`    | `daemon`                             | —        | Writer identity; required when `ENGRAM_CENTRAL_URL` is set       |
| `ENGRAM_WRITER_KEY`   | `daemon`                             | —        | Hex-encoded 32-byte HMAC key; **env only**; required with sync   |
| `ENGRAM_SYNC_INTERVAL`| `daemon`                             | `30s`    | Autosync cadence (Go duration string, e.g. `1m`, `30s`)          |
| `ENGRAM_CONFIG_DIR`   | `daemon`                             | platform | Override the config file directory (see Config file section)     |

Flags take precedence over environment variables. `ENGRAM_WRITER_KEY` has no corresponding flag and always takes precedence over the file-stored key.

### Exit codes

| Code | Meaning                                                              |
|------|----------------------------------------------------------------------|
| `0`  | Success                                                              |
| `1`  | Subcommand error (missing required flag, validation failure, runtime)|
| `2`  | Top-level usage error (no args, unknown subcommand, `--help`)        |

## Wiring into an MCP client

### Claude Code

```bash
claude mcp add engram -- /path/to/engram daemon --db /home/you/.engram/memories.db
```

With central sync:

```bash
claude mcp add engram \
  -e ENGRAM_WRITER_KEY="<hex key from provisioning>" \
  -- /path/to/engram daemon \
       --db /home/you/.engram/memories.db \
       --central-url http://your-central:8080 \
       --writer-id your-node-id
```

### `.mcp.json` (manual config)

Place this in your project root or `~/.mcp.json`:

```json
{
  "mcpServers": {
    "engram": {
      "command": "/path/to/engram",
      "args": [
        "daemon",
        "--db", "/home/you/.engram/memories.db"
      ]
    }
  }
}
```

With central sync and a writer key supplied via environment:

```json
{
  "mcpServers": {
    "engram": {
      "command": "/path/to/engram",
      "args": [
        "daemon",
        "--db", "/home/you/.engram/memories.db",
        "--central-url", "http://your-central:8080",
        "--writer-id", "your-node-id"
      ],
      "env": {
        "ENGRAM_WRITER_KEY": "<hex-key>"
      }
    }
  }
}
```

> **Note on `ENGRAM_WRITER_KEY` in `.mcp.json`:** storing the secret directly in a committed file is convenient for local dev but inappropriate for shared repos. Prefer injecting it via your shell profile or a secrets manager and omitting the `env` block from the committed config.

### Project name override

If project auto-detection picks the wrong name (e.g. in a monorepo), create `.engram/config.json` in the relevant directory:

```json
{ "project_name": "my-project" }
```

## MCP tools

The daemon exposes 9 tools to the connected agent.

| Tool                  | Purpose                                                                              |
|-----------------------|--------------------------------------------------------------------------------------|
| `mem_session_start`   | Register the start of a coding session; resolves and stores the project name         |
| `mem_session_end`     | Mark a session as completed with an optional summary                                 |
| `mem_save`            | Save an observation (decision, bug fix, discovery, …) to persistent memory           |
| `mem_save_prompt`     | Save the user's prompt text so `mem_save` can auto-attach it to the next observation |
| `mem_get_observation` | Retrieve the full untruncated content of an observation by numeric ID                |
| `mem_search`          | Full-text search across all observations; filterable by type, project, scope         |
| `mem_context`         | Assemble recent sessions and observations into a context summary for the agent       |
| `mem_session_summary` | Save a structured end-of-session summary (Goal / Discoveries / Accomplished / …)    |
| `mem_judge`           | Record a verdict on a conflict candidate surfaced by `mem_save`                      |

### Conflict detection

After every `mem_save`, the daemon runs an FTS5/BM25 similarity scan against existing memories. When candidates above a relevance threshold are found, `mem_save` returns a judgment envelope:

```
Memory saved: "my title" (id=42, project="myproject")
CONFLICT REVIEW PENDING — 1 candidate(s); use mem_judge to record verdicts.
judgment_required: true
judgment_status: pending
judgment_id: rel-<hex>
...
```

The agent calls `mem_judge` with the `judgment_id` and one of: `related`, `compatible`, `scoped`, `conflicts_with`, `supersedes`, `not_conflict`.

## Development

```bash
# Build
CGO_ENABLED=0 go build ./cmd/engram

# Vet
go vet ./...
go vet -tags acceptance ./...

# Unit tests (all packages, no external services)
go test ./... -count=1

# Acceptance tests (require embedded-postgres; no Docker needed)
go test -tags acceptance ./... -count=1 -timeout 120s
```

The acceptance suite uses [`github.com/fergusstrange/embedded-postgres`](https://github.com/fergusstrange/embedded-postgres) to spin up a real Postgres instance in-process. No Docker or external database is required.

## License

MIT — see [LICENSE](LICENSE).
