# Design: core-foundation — Typed Memory Model + Central-Authoritative Reconciliation

> Product name: **engram** (locked). Module path below is a PLACEHOLDER — the real
> path is derived from the repo git origin during `sdd-apply`. Do NOT reuse
> `old_code`'s `github.com/Gentleman-Programming/engram`.

## Technical Approach

A single polymorphic `memories` table (discriminated by `entity_type`) backs every memory and SDD entity, indexed by one FTS5 virtual table maintained through triggers. The **central Postgres store** is authoritative: it assigns a monotonic `BIGSERIAL seq` to every accepted mutation, enforces `UNIQUE(topic_key, project, scope)`, and applies version-guarded, tombstone-respecting upserts. Clients (local SQLite) push pending mutations, receive assigned `seq`s, then poll `since_seq` and re-apply locally through the SAME guarded apply path. This directly fixes engram's three apply bugs (topic_key fork, no LWW guard, soft-delete resurrection) confirmed in `old_code/internal/store/store.go:5516` and `:5573`. Hexagonal layering keeps the apply algorithm pure and storage-agnostic so it is unit-testable against an in-process mock central. Single static Windows binary, pure-Go SQLite (no CGO).

## Architecture Decisions

| # | Decision | Choice | Rejected | Rationale |
|---|----------|--------|----------|-----------|
| 1 | Schema shape | Single polymorphic table + `entity_type` + CHECK | Typed tables per entity | One FTS index, one apply path, additive migrations, 1:1 engram retrofit. Typed tables only if type-specific NOT-NULL enforcement provably blocks (V2). |
| 2 | Local driver | `modernc.org/sqlite` (pure Go) | `mattn/go-sqlite3` (CGO) | FTS5 is all core needs; clean single Windows binary. CGO unlocks `sqlite-vec` but breaks cross-compile; deferred to semantic-search change. |
| 3 | Canonical order | `updated_at` (wall-clock) → `version` → `writer_id` → `sync_id` | Client timestamp LWW alone | Clock-skew-proof. `updated_at` is primary LWW key; `version` then `writer_id`/`sync_id` are deterministic tiebreakers. This total order governs ALL conflicts — upsert-vs-row, upsert-vs-tombstone, AND delete-vs-live-row. A delete supersedes a live row only if it wins the same `writeWins` comparison; a stale or tie-losing delete is a NoOp. This symmetric treatment eliminates both the stale-delete split-brain and the exact-tie-upserter-higher split-brain. Central `seq` is the pull-cursor/journal ordering authority only — NOT used for LWW tiebreaking (seq is asymmetric: a node's own rows keep seq=0 until the central-assigned value is pulled, which creates split-brain at the exact tie). |
| 4 | Idempotency key | Content-addressed `mutation_id` (SHA-256 of canonical payload) | DB unique on payload | Reuses `chunkcodec.ChunkID` pattern (`old_code/.../chunkcodec.go:13`); re-apply is a cheap seen-set lookup, no row churn. |
| 5 | Apply purity | Pure `Reconciler.Decide()` returns an `Action`; adapters execute | Apply logic inside SQL handlers | Same decision function runs local + central + mock → invariants are unit-provable without Postgres. |
| 6 | Writer identity | Per-writer ID + HMAC-signed token; `writer_id` audit column | Full JWT/RBAC | Spike needs distinct attributed writers only. Full auth is a later change. |

## Local Store Schema (SQLite — modernc.org/sqlite)

Pragmas (verbatim from `old_code` `:602`): `journal_mode=WAL`, `busy_timeout=5000`, `synchronous=NORMAL`, `foreign_keys=ON`, `SetMaxOpenConns(1)`.

```sql
CREATE TABLE IF NOT EXISTS sessions (
  id          TEXT PRIMARY KEY,
  project     TEXT NOT NULL,
  directory   TEXT NOT NULL,
  writer_id   TEXT NOT NULL,
  started_at  TEXT NOT NULL DEFAULT (datetime('now')),
  ended_at    TEXT
);

CREATE TABLE IF NOT EXISTS memories (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,   -- local only, NOT synced
  sync_id         TEXT NOT NULL UNIQUE,                -- portable cross-machine identity
  session_id      TEXT NOT NULL REFERENCES sessions(id),
  entity_type     TEXT NOT NULL DEFAULT 'memory'
                    CHECK (entity_type IN ('memory','change','spec','task','standard','plan')),
  type            TEXT NOT NULL,                       -- engram subtype: bugfix|decision|...
  status          TEXT,                                -- SDD lifecycle (draft|active|done|archived)
  title           TEXT NOT NULL,
  content         TEXT NOT NULL,
  project         TEXT NOT NULL DEFAULT '',
  scope           TEXT NOT NULL DEFAULT 'project',
  topic_key       TEXT,
  parent_sync_id  TEXT REFERENCES memories(sync_id),   -- task->spec, spec->change; soft FK only
  -- NOTE: no hard deferred FK on parent_sync_id — blocking parent referential integrity rejects
  -- out-of-order mutations during sync apply. Defer-and-replay enforcement is deferred to PR3/PR4.
  version         INTEGER NOT NULL DEFAULT 1,           -- bumped per update (LWW guard)
  seq             INTEGER NOT NULL DEFAULT 0,           -- last central seq applied (0 = local-only)
  writer_id       TEXT NOT NULL,                        -- audit: who wrote last
  normalized_hash TEXT,                                 -- dedupe/idempotency aid
  embedding       BLOB,                                 -- RESERVED, not populated this change
  created_at      TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at      TEXT NOT NULL DEFAULT (datetime('now')),
  deleted_at      TEXT,                                 -- soft delete (mirror of tombstone)
  -- Typed shape enforced at DB level (single-table CHECK strategy):
  --   status:  non-memory rows MUST have a status value
  --   parent:  spec/task/plan MUST reference a parent; memory/change/standard MAY be root
  CHECK (entity_type = 'memory'   OR status IS NOT NULL),
  CHECK (entity_type IN ('memory','change','standard') OR parent_sync_id IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS idx_mem_topic  ON memories(topic_key, project, scope, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_mem_parent ON memories(parent_sync_id);
CREATE INDEX IF NOT EXISTS idx_mem_etype  ON memories(entity_type, status);
CREATE INDEX IF NOT EXISTS idx_mem_deleted ON memories(deleted_at);

-- Tombstone table (the fix for resurrection — engram had this for prompts, not observations)
CREATE TABLE IF NOT EXISTS memory_tombstones (
  sync_id     TEXT PRIMARY KEY,
  project     TEXT,
  scope       TEXT,
  topic_key   TEXT,
  deleted_at  TEXT NOT NULL,
  deleted_by  TEXT NOT NULL,    -- writer_id
  version     INTEGER NOT NULL DEFAULT 0
);

-- Optional single-parent relations promoted to a table for future M:N (out of scope to use)
CREATE TABLE IF NOT EXISTS memory_relations (
  from_sync_id TEXT NOT NULL REFERENCES memories(sync_id),
  to_sync_id   TEXT NOT NULL REFERENCES memories(sync_id),
  rel_type     TEXT NOT NULL DEFAULT 'parent',
  PRIMARY KEY (from_sync_id, to_sync_id, rel_type)
);

-- Push journal: local writes enqueue here (push ordering only; central assigns real seq)
CREATE TABLE IF NOT EXISTS sync_mutations (
  local_seq   INTEGER PRIMARY KEY AUTOINCREMENT,
  mutation_id TEXT NOT NULL UNIQUE,    -- content-addressed (SHA-256 of canonical payload)
  entity      TEXT NOT NULL,
  entity_key  TEXT NOT NULL,           -- sync_id
  op          TEXT NOT NULL,           -- 'upsert' | 'delete'
  payload     TEXT NOT NULL,
  writer_id   TEXT NOT NULL,
  occurred_at TEXT NOT NULL DEFAULT (datetime('now')),
  acked_at    TEXT
);

CREATE TABLE IF NOT EXISTS sync_state (
  target_key      TEXT PRIMARY KEY DEFAULT 'central',
  last_acked_seq  INTEGER NOT NULL DEFAULT 0,
  last_pulled_seq INTEGER NOT NULL DEFAULT 0
);

-- Applied-mutation seen-set for cross-machine idempotent re-apply (invariant 5)
CREATE TABLE IF NOT EXISTS applied_mutations (
  mutation_id TEXT PRIMARY KEY,
  applied_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- FTS5 external-content + triggers (pattern from old_code :704 / :985)
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
  title, content, type, entity_type, status, project, topic_key,
  content='memories', content_rowid='id'
);
CREATE TRIGGER IF NOT EXISTS mem_fts_insert AFTER INSERT ON memories BEGIN
  INSERT INTO memories_fts(rowid, title, content, type, entity_type, status, project, topic_key)
  VALUES (new.id, new.title, new.content, new.type, new.entity_type, new.status, new.project, new.topic_key);
END;
CREATE TRIGGER IF NOT EXISTS mem_fts_delete AFTER DELETE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, title, content, type, entity_type, status, project, topic_key)
  VALUES ('delete', old.id, old.title, old.content, old.type, old.entity_type, old.status, old.project, old.topic_key);
END;
CREATE TRIGGER IF NOT EXISTS mem_fts_update AFTER UPDATE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, title, content, type, entity_type, status, project, topic_key)
  VALUES ('delete', old.id, old.title, old.content, old.type, old.entity_type, old.status, old.project, old.topic_key);
  INSERT INTO memories_fts(rowid, title, content, type, entity_type, status, project, topic_key)
  VALUES (new.id, new.title, new.content, new.type, new.entity_type, new.status, new.project, new.topic_key);
END;
```

`sanitizeFTS(query)` (port from `old_code`) wraps user terms to block FTS5 operator injection.

## Central Store Schema (Postgres)

```sql
-- Authoritative mutation journal: server assigns the canonical order (the tiebreaker)
CREATE TABLE IF NOT EXISTS central_mutations (
  seq         BIGSERIAL PRIMARY KEY,            -- canonical monotonic order (invariant 2)
  mutation_id TEXT NOT NULL UNIQUE,             -- content-addressed; dedupes re-push (invariant 5)
  project     TEXT NOT NULL,
  entity      TEXT NOT NULL,
  entity_key  TEXT NOT NULL,                    -- sync_id
  op          TEXT NOT NULL CHECK (op IN ('upsert','delete')),
  payload     JSONB NOT NULL,
  writer_id   TEXT NOT NULL,                    -- audit / attribution
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_central_mut_seq ON central_mutations(seq);

-- Canonical record (materialized authoritative state)
CREATE TABLE IF NOT EXISTS central_memories (
  sync_id      TEXT PRIMARY KEY,
  entity_type  TEXT NOT NULL,
  type         TEXT NOT NULL,
  status       TEXT,
  title        TEXT NOT NULL,
  content      TEXT NOT NULL,
  project      TEXT NOT NULL DEFAULT '',
  scope        TEXT NOT NULL DEFAULT 'project',
  topic_key    TEXT,
  parent_sync_id TEXT,
  version      INTEGER NOT NULL DEFAULT 1,
  writer_id    TEXT NOT NULL,                   -- last writer (audit)
  created_by   TEXT NOT NULL,                   -- first writer (audit)
  embedding    BYTEA,                           -- pgvector-ready; NOT used this change (note below)
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at   TIMESTAMPTZ
);
-- THE critical fix for Bug A: topic identity is unique, so it cannot fork (invariant 1)
CREATE UNIQUE INDEX IF NOT EXISTS central_memories_topic_uidx
  ON central_memories(topic_key, project, scope)
  WHERE topic_key IS NOT NULL AND deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS central_tombstones (
  sync_id    TEXT PRIMARY KEY,
  topic_key  TEXT, project TEXT, scope TEXT,
  deleted_at TIMESTAMPTZ NOT NULL,
  deleted_by TEXT NOT NULL,
  version    INTEGER NOT NULL DEFAULT 0
);
```

> **pgvector note (NOT used in this change)**: `embedding` is reserved as `BYTEA`.
> The semantic-search change will `CREATE EXTENSION vector` and migrate the column to
> `VECTOR(1536)` + an HNSW index. No embeddings are written or read here.

## The Apply-Path Algorithm (the heart)

One pure decision function drives BOTH push-apply (central) and pull-apply (local). Each numbered step is tagged with the invariant(s) it satisfies.

```go
// Pure, storage-agnostic. Returns the action an adapter must execute.
// Tx provides reads only; the adapter performs the write the Action names.
func Decide(tx Reader, m Mutation) Action {
    // (5) Idempotent re-apply: content-addressed id already applied -> no-op.
    if tx.MutationApplied(m.ID) {                     // INV 5
        return Action{Kind: NoOp, Reason: "already applied"}
    }

    // (1) Identity resolution: prefer topic identity, else sync_id.
    var cur *Record
    if m.TopicKey != "" {                             // INV 1
        cur = tx.FindByTopic(m.TopicKey, m.Project, m.Scope) // unique key
    }
    if cur == nil {
        cur = tx.FindBySyncID(m.SyncID)               // INV 6 (no-topic writes keyed by sync_id)
    }

    // (4) Tombstone guard BEFORE any upsert: a delete at >= write time blocks resurrection.
    if ts := tx.FindTombstone(m.SyncID, m.TopicKey, m.Project, m.Scope); ts != nil { // INV 4
        if m.Op == Upsert && !writeWins(m, ts.DeletedAt, ts.Version, ts.DeletedBy, ts.SyncID) {
            return Action{Kind: NoOp, Reason: "tombstoned, stale write"}
        }
    }

    switch m.Op {
    case Delete:
        // Uniform LWW gate: a delete supersedes the live row only if it wins the
        // total order. When cur != nil and the delete loses writeWins, it is a NoOp
        // (prevents stale-delete split-brain). When cur == nil the gate is skipped
        // (pure tombstone or cross-writer re-tombstone paths).
        if cur != nil && !writeWins(m, cur.UpdatedAt, cur.Version, cur.WriterID, cur.SyncID) {
            return Action{Kind: NoOp, Reason: "stale delete, live row newer"}
        }
        return Action{Kind: WriteTombstone, Mutation: m} // sets deleted_at + tombstone row; INV 4
    case Upsert:
        if cur == nil {
            return Action{Kind: Insert, Mutation: m}     // first write; INV 1, 6
        }
        // (3) Version-guarded LWW: newer-by-(updated_at, version, writer_id, sync_id) wins.
        if writeWins(m, cur.UpdatedAt, cur.Version, cur.WriterID, cur.SyncID) {  // INV 3
            return Action{Kind: Update, Target: cur.SyncID, Mutation: m} // INV 1 converges to one row
        }
        return Action{Kind: NoOp, Reason: "stale, local newer"}                // INV 3 no lost update
    }
    return Action{Kind: NoOp}
}

// Deterministic ordering — clock-skew safe. Final tiebreaker uses stable, replica-identical
// fields (writer_id then sync_id) so every store computes the same winner from the same
// inputs. Central seq is NOT used: a node's own rows keep seq=0 until the pull back-patches
// it, making seq ASYMMETRIC and unsafe as a tie-break (causes split-brain at the exact tie).
func writeWins(m Mutation, curUpdatedAt time.Time, curVersion int, curWriterID, curSyncID string) bool {
    if !m.UpdatedAt.Equal(curUpdatedAt) {          // primary: wall-clock LWW
        return m.UpdatedAt.After(curUpdatedAt)
    }
    if m.Version != curVersion {                    // secondary: monotonic version
        return m.Version > curVersion
    }
    if m.WriterID != curWriterID {                  // tertiary: writer identity
        return m.WriterID > curWriterID
    }
    return m.SyncID > curSyncID                     // final: sync_id (full equality → false)
}
```

**Central push-apply** (single Postgres tx per mutation): assign `seq` via `INSERT ... RETURNING seq` (`old_code/.../cloudstore.go:761`); run `Decide`; execute the Action with SQL guards that mirror the decision — `UPDATE ... WHERE updated_at < $in OR (updated_at = $in AND version < $vin)` and `ON CONFLICT (topic_key,project,scope) DO UPDATE ... WHERE` the same predicate. The `mutation_id UNIQUE` makes a re-pushed batch a no-op at the DB layer too (defense in depth for INV 5).

**Local pull-apply**: `GET since_seq` returns rows `WHERE seq > last_pulled ORDER BY seq ASC` (`old_code/.../cloudstore.go:1148`) → apply each through the SAME `Decide` in seq order (INV 2) → record `applied_mutations(mutation_id)` and advance `last_pulled_seq`. Delete writes tombstone + sets `deleted_at`; it never executes a bare `deleted_at = NULL` (the engram bug at `:5573`).

## Writer Identity Mechanism

Each writer holds a stable `writer_id` and an HMAC-SHA256 token `sig = HMAC(sharedSecret, writer_id)`. Every push carries `(writer_id, sig)`; the central store verifies `sig` (constant-time compare) and stamps `writer_id` onto `central_mutations.writer_id`, `central_memories.writer_id`/`created_by`, and tombstone `deleted_by`. Pulled mutations carry `writer_id` so the local audit column is populated on apply. This is attribution-grade only — no expiry, refresh, or RBAC (deferred to a future auth change). Token lives in config/env; `domain` exposes `Identity{WriterID, Sign(), Verify()}` so the spike can mint two distinct writers trivially.

## Two-Writer Convergence Spike

`internal/spike` runs two in-process `Writer` instances (each own SQLite file) against a central. Two central adapters share one `Decide`: an in-process mock (`map`-backed, used by unit tests) and the real Postgres adapter (acceptance only). Unit-mockable = invariants 1, 3, 5, 6 (pure decision + mock journal, no DB). Real-Postgres-only = invariant 2 (`BIGSERIAL` monotonicity) and end-to-end push/pull for invariant 4 under real transactions.

| Inv | Scenario | Concrete assertion |
|-----|----------|--------------------|
| 1 Identity | A upsert topic `sdd/test/explore` @T=100; B same @T=50; full sync | both locals: `COUNT(*) WHERE topic_key=...` = 1 AND content = A's |
| 2 Seq order | push 3 mutations interleaved; pull `since_seq=0` | received `seq` strictly ascending; applied order == seq order regardless of `occurred_at` |
| 3 No lost update | row @v2 T=100 exists; apply older @v1 T=50 | record unchanged (`version=2`, content original); `Decide` returns `NoOp` |
| 4 No resurrection | A delete @T=200; B upsert @T=150; sync both | both locals `deleted_at IS NOT NULL`; tombstone present; upsert `NoOp` |
| 5 Idempotent | apply same `mutation_id` twice | second apply `NoOp`; `version`, row count unchanged; `applied_mutations` has 1 row |
| 6 Independent | A and B each upsert NEW no-topic memory concurrently; sync | both `sync_id`s present on both locals; zero conflicts raised |

Throwaway harness (deleted/archived after proof) — not a production transport.

## Module & Package Layout

Module path: `PLACEHOLDER/engram` — **set from the repo git origin during `sdd-apply`**; do NOT hardcode `old_code`'s `github.com/Gentleman-Programming/engram`. Hexagonal, single Windows binary.

```
<module>/
├── cmd/engram/main.go            # wires adapters; static Windows binary
├── internal/
│   ├── domain/                   # pure core (no I/O)
│   │   ├── memory.go             # Memory, EntityType, Mutation, Tombstone, Action
│   │   ├── reconcile.go          # Decide(), writeWins() — invariants live here
│   │   └── identity.go           # Identity{WriterID, Sign, Verify}
│   ├── localstore/               # SQLite adapter (modernc.org/sqlite)
│   │   ├── schema.go             # DDL above + pragmas + FTS triggers
│   │   ├── store.go              # Reader/Writer ports; FTS search; sanitizeFTS
│   │   └── apply.go              # executes domain.Action locally (pull-apply)
│   ├── centralstore/             # Postgres adapter
│   │   ├── schema.go             # central DDL; BIGSERIAL; UNIQUE topic; tombstones
│   │   └── apply.go              # seq assign + guarded upsert (push-apply)
│   ├── mutation/                 # content-addressed id (SHA-256 canonical) + canonicalize
│   └── spike/                    # two-writer convergence harness (throwaway)
└── go.mod                        # module <module>/engram
```

Ports (`domain`): `Reader` (FindByTopic, FindBySyncID, FindTombstone, MutationApplied), `Writer` (Insert/Update/WriteTombstone/RecordApplied). Local and central each implement them; `Decide` depends only on `Reader`, so it is fully unit-testable.

## File Changes

| File | Action | Description |
|------|--------|-------------|
| `internal/domain/memory.go` | Create | Core types, `Action`, ports |
| `internal/domain/reconcile.go` | Create | `Decide` + `writeWins` (all invariants) |
| `internal/domain/identity.go` | Create | HMAC writer identity |
| `internal/localstore/{schema,store,apply}.go` | Create | SQLite schema, FTS, pull-apply |
| `internal/centralstore/{schema,apply}.go` | Create | Postgres schema, seq, push-apply |
| `internal/mutation/*.go` | Create | Content-addressed id + canonicalize |
| `internal/spike/*.go` | Create | Convergence harness (throwaway) |
| `cmd/engram/main.go`, `go.mod` | Create | Binary entrypoint; module path placeholder |

## Testing Strategy

| Layer | What | Approach |
|-------|------|----------|
| Unit | `Decide`/`writeWins` invariants 1,3,5,6; FTS roundtrip; canonical id determinism | table-driven, in-process mock `Reader`/`Writer`; no DB |
| Integration | Local SQLite apply + tombstone + FTS triggers | real `modernc.org/sqlite` temp file |
| Acceptance | Invariants 2 & 4 end-to-end; `BIGSERIAL` monotonicity | real Postgres (dev), two writers, full push/pull |

## Migration / Rollout

No migration — greenfield, isolated from `old_code/`. Postgres schema is dev-only; drop the database to reset. Revert = delete the new `internal/` packages and `cmd/`. Engram untouched.

## Open Questions

- [ ] Final product name is **engram** (locked here) — confirm no collision with the existing `old_code` engram if both ever run side by side (out of scope to resolve now).
- [ ] Shared-secret distribution for the HMAC writer token (env vs config file) — pick during apply; does not block design.
