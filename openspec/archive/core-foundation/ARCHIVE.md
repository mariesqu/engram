# Archive Report: core-foundation

**Status**: COMPLETE — archived 2026-06-09.

## Outcome

The change delivered everything it set out to prove, and the project then grew
far past its scope. The engine is **feature-complete**: full acceptance suite
green (12 packages, embedded-postgres convergence proofs) on `main`.

### Delivered by this change (Phases 1–5, 7)

- Typed memory model (`entity_type` discriminator, single polymorphic table).
- Central-authoritative reconciliation: server-assigned `seq`, LWW with
  version guard, `(topic_key, project, scope)` identity, real tombstones.
- Local SQLite (pure-Go `modernc.org/sqlite`, FTS5) + central Postgres stores.
- Two-writer convergence spike proving **all six invariants** (identity
  convergence, monotonic seq, no lost updates, no resurrection, idempotent
  re-apply, independent writes preserved).

### Superseded (Phases 6, 8 — see notes in tasks.md)

The spike-grade writer-identity and smoke-wiring tasks were replaced by
stronger production implementations built in later phases:

- Per-writer HMAC request signing (`internal/wireauth`) + server-side key
  verifier and writer_id forgery check (`internal/cloudserve`, 403 on
  mismatch, entity-agnostic).
- Real HTTP transport (`internal/remote` + `internal/syncer` autosync loop)
  instead of the throwaway `PushAndPull` smoke check.

### Deferred (future observability change)

- Tasks 6.3/6.4: `central_lww_discards` audit table + `RecordLWWDiscard` on
  LWW NoOp. Correctness does not depend on it (discards are deterministic via
  `domain.Decide`); revisit if discard visibility is ever needed in operations.

## What was built after this change (out of its scope, now shipped)

- HTTP transport phase: central server, sync wire, per-writer HMAC auth.
- Local-daemon MVP: 9 MCP tools over stdio (`cmd/engram serve`).
- Multi-project autosync (per-project pull cursors over a global journal seq).
- Write-queue (all local writes serialized).
- Conflict detection (FTS candidates → judgment envelope → `mem_judge`).
- Synced prompts (`EntityPrompt` riding the entity-agnostic journal unchanged).

## Naming

The proposal's working title was NEWENGINE with candidate names. The project
shipped as **engram** (module `github.com/mariesqu/engram`).
