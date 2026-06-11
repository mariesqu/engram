# Security Policy

## Reporting a vulnerability

Please report security vulnerabilities **privately** via
[GitHub Security Advisories](https://github.com/mariesqu/engram/security/advisories/new)
— do not open a public issue for anything exploitable.

You can expect an acknowledgment within a few days. Please include reproduction
steps and the affected surface (local daemon, control API, web UI, sync
transport, or embedding pipeline).

## Threat model (what engram defends, by design)

- **Local control plane** (`/api`, `/ui`, `/mcp`): binds `127.0.0.1` only;
  bearer token from `daemon.json` (user-only ACL, rotated per start); the web
  UI adds session-bound CSRF + Origin checks + a self-only CSP.
- **Secrets**: the sync writer key and the embedding API key are env-first and
  DPAPI-sealed at rest on Windows; they are never flags, never logged, never
  echoed in any API response, and never appear in `--help`.
- **Sync transport**: per-writer HMAC request signing; the central server
  rejects writer-id forgery (a mutation claiming another writer's identity).
- **Embedding privacy**: per-project policy gates every provider call —
  `omitted` projects are never embedded, `local-only` text never reaches a
  remote provider, and a local sidecar requires explicit consent. Plain-http
  embedding endpoints are rejected for non-loopback hosts.

## Scope notes

- The control plane's threat model is **single-user loopback**; it is not
  designed to be exposed on a network interface.
- Acceptance tests run against embedded-postgres; no production credentials
  are required or used anywhere in the test suite.
