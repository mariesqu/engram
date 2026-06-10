# MCP HTTP Transport Specification

## Purpose

Defines the opt-in Streamable HTTP MCP transport available alongside the default stdio transport. stdio remains the default indefinitely; the HTTP transport is strictly opt-in via a flag.

---

## Requirements

### Requirement: stdio is the default transport

The daemon MUST default to stdio MCP transport when no `--transport` flag is provided. Existing daemon invocations (no flags change) MUST behave identically to pre-change behavior. All existing stdio tests MUST remain green.

#### Scenario: Default invocation uses stdio

- GIVEN `engram daemon` is started with no `--transport` flag
- WHEN an MCP client connects via stdin/stdout
- THEN the daemon responds to MCP requests exactly as before this change

#### Scenario: Existing daemon tests remain green

- GIVEN the test suite including `TestRun_DaemonHelp_DoesNotLeakWriterKey` and all other daemon tests
- WHEN the test suite is run after this change
- THEN all tests pass without modification

---

### Requirement: --transport http opt-in flag

When started with `--transport http`, the daemon SHALL use `mcp-go`'s `NewStreamableHTTPServer` to serve the MCP protocol over HTTP. The HTTP MCP server MUST be mounted at the path `/mcp` on the **same loopback listener and port** as the control API (the top-level `http.ServeMux` routes `/api/v1`, `/ui`, and `/mcp` on a single `net.Listener`). There is no separate MCP port; `--mcp-http-port` is not a valid flag. The `--transport stdio` flag SHALL be accepted as an explicit no-op (same as the default).

#### Scenario: HTTP transport starts with --transport http

- GIVEN `engram daemon --transport http` is invoked
- WHEN an MCP client connects via HTTP to the MCP HTTP port
- THEN the daemon processes MCP tool calls over HTTP

#### Scenario: --transport stdio is accepted as default

- GIVEN `engram daemon --transport stdio` is invoked
- WHEN an MCP client connects via stdin/stdout
- THEN the daemon behaves identically to default (no-flag) invocation

---

### Requirement: Identical tool surface on both transports

The HTTP MCP transport MUST expose the same set of tools as the stdio transport. No tool SHALL be available on one transport but not the other. Tool behavior, input schemas, and output schemas MUST be identical regardless of transport.

#### Scenario: All MCP tools available via HTTP transport

- GIVEN the daemon is running with `--transport http`
- WHEN an MCP client calls any tool (e.g., `mem_save`, `mem_search`, `mem_get_observation`)
- THEN the tool is available and returns the same result as it would via stdio

#### Scenario: Policy enforcement applies on HTTP transport

- GIVEN project "omitted-proj" has policy `omitted` and the daemon uses HTTP transport
- WHEN an MCP client calls `mem_save` with `project: "omitted-proj"` via the HTTP transport
- THEN the tool returns an error and writes nothing (same as stdio behavior)

---

### Requirement: HTTP transport is opt-in — no forced migration

The HTTP transport MUST NOT be required for existing users. Users who never set `--transport http` MUST NOT observe any behavioral change. The HTTP transport is intended for users running the resident daemon who want to connect additional MCP clients over HTTP.

#### Scenario: No behavioral change for users not opting in

- GIVEN an existing Claude Code config that starts `engram daemon` with no `--transport` flag
- WHEN this change is deployed
- THEN the Claude Code session works identically to before — no config change required

---

### Requirement: HTTP MCP transport acceptance test

A dedicated acceptance test SHALL verify that the HTTP MCP transport can handle at least one complete tool round-trip (`mem_save` + `mem_search` or equivalent) over the HTTP connection.

#### Scenario: HTTP MCP round-trip acceptance

- GIVEN the daemon is started with `--transport http`
- WHEN an MCP client calls `mem_save` then `mem_search` via the HTTP transport
- THEN both calls succeed and the search returns the saved observation

---

> **Headless testability**: All requirements are provable headlessly. stdio regression is proven by the existing test suite. HTTP transport acceptance is proven by an automated acceptance test against the running HTTP server. No tray or browser required.
