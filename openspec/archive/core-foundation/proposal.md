# Proposal: core-foundation — Typed Memory Model + Central-Authoritative Reconciliation

> Working title: **NEWENGINE** (name NOT finalized — see Naming).

## Intent

Build the foundational data model and a CORRECT multi-writer reconciliation model for a new in-house AI memory engine (Go, Windows-first) for spec-driven, multi-agent coding teams. Engram's apply path is broken — topic_key forks into duplicate rows, no LWW version guard (last-by-arrival wins), soft-deletes resurrect on re-sync. This change owns the engine ("Posture B") and proves convergence with a two-writer spike. Reuses engram's IDEAS (FTS5, content-addressed chunks, BIGSERIAL ordering), not its bugs.

## Scope

### In Scope
- Typed memory data model (Change, Spec, Task+status, Standard, Plan as first-class via `entity_type`).
- Central-authoritative reconciliation: server-assigned monotonic `seq` (BIGSERIAL) as canonical tiebreaker; correct LWW with version guard; identity on `(topic_key, project, scope)`; real tombstones.
- Local store schema (SQLite + FTS5) + central store schema (Postgres).
- Minimal per-writer identity for write attribution (spike-grade, not a full auth system).
- A two-writer convergence spike proving the invariants below.

### Out of Scope (each = its own future change)
- Windows tray / process model · browser UI · real sync transport wiring (HTTP vs gRPC) · semantic search / embeddings (`embedding` column reserved, NOT populated) · enhanced MCP server.

## Capabilities

### New Capabilities
- `memory-model`: typed memory record + SDD entity discriminator, parent relations, version/tombstone columns, local FTS5 indexing.
- `reconciliation`: central-authoritative apply with seq ordering, LWW guard, topic identity convergence, tombstone-blocked resurrection, idempotent re-apply.
- `writer-identity`: minimal per-writer identity propagated to writes for attribution in the spike.

### Modified Capabilities
- None (greenfield).

## Approach

Single polymorphic `memories` table (`entity_type` discriminator). Local SQLite (FTS5 search). Central Postgres assigns `seq`; clients push pending mutations, central applies LWW-guarded upserts, clients poll `since_seq` and re-apply locally. Decisions:

| # | Decision | Recommendation | Rationale |
|---|----------|----------------|-----------|
| 1 | Schema shape | **Single polymorphic table + `entity_type`** | Additive migrations, one FTS index + one apply path, 1:1 engram retrofit. Typed tables deferred to V2 only if type-specific NOT-NULL enforcement provably blocks. |
| 2 | Local SQLite driver | **Pure-Go `modernc.org/sqlite`** | core-foundation needs only FTS5, which pure-Go supports → clean single Windows binary, no CGO toolchain. CGO/`mattn` unlocks local `sqlite-vec` but complicates Windows builds. **Deferrable**: revisit at the semantic-search change; central pgvector covers vectors meanwhile. NOT a now-blocker. |
| 3 | Central store | **Postgres** | BIGSERIAL `seq` ordering + `UNIQUE(topic_key,project,scope)` upsert + pgvector-ready. Proven in engram cloudstore. Managed-vs-self-host deferred. |
| 4 | Writer identity (spike) | **Per-writer ID + signed token (minimum)** | Multi-writer convergence requires distinct attributed writers. Define minimum: writer id in every mutation + audit field. Full JWT/refresh/RBAC = future auth change. |

## Affected Areas

| Area | Impact | Description |
|------|--------|-------------|
| `internal/store/` (local) | New | `memories` schema, FTS5 triggers, WAL pragmas, LWW apply. |
| `internal/central/` (Postgres) | New | `seq` ordering, upsert, tombstones, LWW guard. |
| `internal/spike/` | New | Two-writer convergence harness (throwaway in-process/HTTP). |
| `old_code/` | Reference only | Patterns reused; not modified. |

## Risks

| Risk | Likelihood | Mitigation |
|------|------------|------------|
| Clock-skew breaks `updated_at` LWW | Med | Central `seq` is authoritative tiebreaker; add `version` int as secondary guard; warn on future timestamps. |
| Spike is integration-level (real Postgres, 2 procs) | Med | In-process mock central for unit tests; real Postgres for acceptance only. |
| Pure-Go choice blocks local vectors later | Low | Accepted for V1; semantic-search change re-decides (CGO or embedding sidecar). |
| Windows SQLite path / long-path | Low | `os.UserHomeDir()` → `%USERPROFILE%\.newengine`; test path handling. |

## Rollback Plan

Greenfield, isolated from `old_code/`. Revert = delete new `internal/` packages + spike; no production system depends on this yet. Postgres schema is dev-only; drop the database. Engram remains untouched and operational.

## Dependencies

- Postgres (dev instance) for the convergence spike.
- Go toolchain (Windows). No CGO required for this change.

## Success Criteria — Two-Writer Convergence Invariants

> Contract for spec/design/tasks. The spike MUST prove all of these.

- [ ] **Identity convergence**: A saves topic_key `sdd/test/explore` @T=100, B saves same key @T=50; after full sync BOTH local stores hold EXACTLY ONE row, content from A (T=100 wins).
- [ ] **Monotonic-seq ordering**: clients applying `since_seq` receive/apply mutations in strict server seq order, independent of client clocks.
- [ ] **No lost updates**: central UPDATE carries `WHERE updated_at < incoming` (+ version guard); older arriving writes never overwrite newer rows.
- [ ] **No soft-delete resurrection**: A deletes @T=200, B updates same row @T=150; after sync row stays deleted on both (tombstone.deleted_at ≥ payload.updated_at blocks upsert).
- [ ] **Idempotent re-apply**: re-applying the same content-addressed mutation is a no-op (no duplicate rows, no version churn).
- [ ] **Independent new writes preserved**: A and B each save NEW no-topic_key memories concurrently; after sync BOTH survive (sync_id uniqueness, no false conflict).

## Naming

Name NOT finalized. Candidates for the user to choose:
- **Mnemo** — Greek "memory"; short, ownable, evokes recall.
- **Recall** — plain-English, instantly conveys the function; check trademark collisions.
- **Engrama** — keeps lineage with engram while signaling a distinct, owned product.
