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

## CLI reference

```
engram serve  [--addr <addr>] [--dsn <dsn>]
engram keys   provision [--dsn <dsn>] <writer-id>
engram keys   revoke    [--dsn <dsn>] <writer-id>
engram daemon [--db <path>] [--central-url <url>] [--writer-id <id>] [--sync-interval <dur>]
```

### Environment variables

| Variable              | Used by         | Default  | Description                                                      |
|-----------------------|-----------------|----------|------------------------------------------------------------------|
| `ENGRAM_ADDR`         | `serve`         | `:8080`  | Listen address for the central HTTP server                       |
| `ENGRAM_DSN`          | `serve`, `keys` | —        | Postgres DSN (required)                                          |
| `ENGRAM_DB`           | `daemon`        | —        | Path to the local SQLite database (required)                     |
| `ENGRAM_CENTRAL_URL`  | `daemon`        | —        | Central server URL; omit for local-only mode                     |
| `ENGRAM_WRITER_ID`    | `daemon`        | —        | Writer identity; required when `ENGRAM_CENTRAL_URL` is set       |
| `ENGRAM_WRITER_KEY`   | `daemon`        | —        | Hex-encoded 32-byte HMAC key; **env only**; required with sync   |
| `ENGRAM_SYNC_INTERVAL`| `daemon`        | `30s`    | Autosync cadence (Go duration string, e.g. `1m`, `30s`)          |

Flags take precedence over environment variables. `ENGRAM_WRITER_KEY` has no corresponding flag.

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
