# Restore Shared MCP Registrations

## Objective

Configure Codex, Claude, and OpenCode with exactly three MCP servers: Context7, Playwright, and Engram, with every Engram registration using `engram connect` against one shared database-backed daemon.

## Problem

The Gentle AI installation left Codex without Playwright and registered Engram as a per-client `engram mcp` process. Claude and OpenCode retain all three servers, but their Engram entries also bypass the shared daemon bridge.

## Why

All three CLIs should expose the same MCP tool set while sharing one resident Engram instance and database.

## Scope

- Update the effective Codex user MCP configuration.
- Update the Claude user-scope Engram registration.
- Update the OpenCode user MCP configuration.
- Verify each CLI/config exposes exactly Context7, Playwright, and Engram.

## Constraints

- Preserve existing Context7 and Playwright settings where already present.
- Use `C:\Users\mtl.mesquivel\go\bin\engram.exe connect --db C:\Users\mtl.mesquivel\AppData\Roaming\engram\engram.db` for Engram.
- Do not push or open a pull request.
- Engram recovery mirror is pending because the current session has no Engram MCP tools available.

## Configuration

- TDD: not applicable (user-level CLI configuration)
- Verification: CLI registration listing plus exact config readback
- Delivery strategy: ask-on-risk
- Forecast: under 100 authored lines

## Tasks

- [x] **MCP-001 — Restore and unify registrations**
  - Route: delegated
  - Trigger: three non-trivial user configuration files across distinct CLIs
  - Acceptance: all clients register Context7, Playwright, and Engram; Engram uses `connect --db` everywhere
  - Checks: `codex mcp list` shows exactly three enabled registrations; `claude mcp list` reports Context7, Playwright, and Engram connected; `opencode mcp list` reports exactly three connected servers
  - Runtime harness: all three CLI registration/health listings completed successfully outside the restricted sandbox
  - Rollback boundary: restore the timestamped backups for the three user configuration files
  - Commit evidence: this recovery record's `chore(config): record shared MCP registration recovery` work-unit commit

## Progress

- Read-only mapping completed; shared daemon health confirmed on port 7700.
- Codex now registers Context7, Playwright, and Engram; Engram uses `connect --db`.
- Claude reports the three user MCPs connected; Engram uses `connect --db`.
- OpenCode reports exactly three connected MCPs; Engram uses `connect --db`.
- Backups created with suffix `20260918114543.bak` beside each original configuration.
- Engram recovery mirror remains pending because this session cannot access Engram MCP tools until restart.

## Next Step

Restart the active Codex session so it reloads the corrected MCP catalog.
