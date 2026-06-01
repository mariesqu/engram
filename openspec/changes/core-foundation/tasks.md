# Tasks: core-foundation — Typed Memory Model + Central-Authoritative Reconciliation

## Review Workload Forecast

| Field | Value |
|-------|-------|
| Estimated changed lines | 1,100 – 1,450 (new files only; greenfield) |
| 400-line budget risk | High |
| Chained PRs recommended | Yes |
| Suggested split | PR 1: scaffold + domain + local schema → PR 2: Decide() + unit tests → PR 3: central schema + writer identity → PR 4: spike + acceptance tests |
| Delivery strategy | ask-on-risk |
| Chain strategy | pending |

Decision needed before apply: Yes
Chained PRs recommended: Yes
Chain strategy: pending
400-line budget risk: High

### Suggested Work Units

| Unit | Goal | Likely PR | Notes |
|------|------|-----------|-------|
| 1 | Go module scaffold + hexagonal layout + domain types | PR 1 | base = main; pure types, no I/O; ~200 lines |
| 2 | Local SQLite schema + FTS5 + pragmas + apply adapter | PR 1 | base = main; depends on domain types; ~300 lines; combined with unit 1 if risk accepted |
| 3 | Pure `Decide()` + `writeWins()` + unit tests INV 1,3,5,6 | PR 2 | base = PR 1 branch; zero DB deps; fully unit-testable |
| 4 | Central Postgres schema + push-apply adapter | PR 3 | base = PR 2 branch; needs real Postgres for tests |
| 5 | Writer identity (HMAC token + audit stamping) | PR 3 | base = PR 2 branch; can land same PR as unit 4 |
| 6 | Two-writer convergence spike + acceptance tests INV 2,4 | PR 4 | base = PR 3 branch; throwaway harness; marks proof-of-convergence |

---

## Phase 1: Go Module Scaffold + Hexagonal Package Layout

> Spec traceable: memory-model (required fields, pragmas, Windows path); design (module layout, ports).
> No external dependencies resolved yet — module path set from `git remote get-url origin` at apply time.

- [x] 1.1 Run `git remote get-url origin` to resolve the real module path; create `go.mod` with that path + `go 1.22` minimum; add `modernc.org/sqlite`, `github.com/lib/pq` (or `pgx/v5`), and standard test deps.
- [x] 1.2 Create directory tree: `cmd/engram/`, `internal/domain/`, `internal/localstore/`, `internal/centralstore/`, `internal/mutation/`, `internal/spike/`.
- [x] 1.3 Create `cmd/engram/main.go` — empty `main()` stub with a build-tag comment; verifies `go build ./...` passes from the start.

---

## Phase 2: Domain Types

> Spec traceable: memory-model (sync_id, entity_type, version, deleted_at, embedding reserved, parent_id); reconciliation (Action, ports); writer-identity (Identity).

- [x] 2.1 Create `internal/domain/memory.go` — define `EntityType` string type + valid set constant; `Record` struct (all required + optional + reserved embedding fields per spec); `Tombstone` struct; `Mutation` struct (Op, SyncID, TopicKey, Project, Scope, Version, Seq, UpdatedAt, Payload, WriterID, MutationID); `Action` type + constants (NoOp, Insert, Update, WriteTombstone).
- [x] 2.2 Create `internal/domain/ports.go` — define `Reader` interface (FindByTopic, FindBySyncID, FindTombstone, MutationApplied) and `Writer` interface (Insert, Update, WriteTombstone, RecordApplied); these are the ONLY seams Decide() depends on.
- [x] 2.3 Create `internal/domain/identity.go` — `Identity` struct with `WriterID string`; `Sign(secret []byte) string` (HMAC-SHA256 hex); `Verify(secret []byte, sig string) bool` (constant-time compare).
- [x] 2.4 Create `internal/mutation/mutation.go` — `CanonicalPayload(m Mutation) []byte` (deterministic JSON marshal, sorted keys); `NewMutationID(payload []byte) string` (SHA-256 hex of canonical payload); unit test: same inputs → same ID, different content → different ID.

---

## Phase 3: Local SQLite Schema + FTS5 + Pragmas

> Spec traceable: memory-model (WAL pragmas, FTS5, soft-delete, Windows path, max 1 conn, retry on SQLITE_BUSY).

- [x] 3.1 Create `internal/localstore/schema.go` — `ApplySchema(db *sql.DB) error`; emits all DDL: `memories`, `memory_tombstones`, `memory_relations`, `sync_mutations`, `sync_state`, `applied_mutations`; all indexes; FTS5 virtual table `memories_fts`; three triggers (`mem_fts_insert`, `mem_fts_delete`, `mem_fts_update`). Idempotent: `CREATE TABLE IF NOT EXISTS`, `CREATE INDEX IF NOT EXISTS`.
- [x] 3.2 Create `internal/localstore/store.go` — `Open(path string) (*Store, error)`: resolves Windows path via `os.UserHomeDir()` + `filepath.Join`; sets pragmas (WAL, busy_timeout=5000, synchronous=NORMAL, foreign_keys=ON); `SetMaxOpenConns(1)`; calls `ApplySchema`; implements `domain.Reader` interface methods.
- [x] 3.3 Add `sanitizeFTS(query string) string` to `internal/localstore/store.go` — port of old_code sanitize logic: wraps each term in quotes, strips FTS5 operator characters; unit test: operators stripped, phrases quoted.
- [x] 3.4 Create `internal/localstore/apply.go` — `Apply(db *sql.DB, a domain.Action, m domain.Mutation) error`; executes Insert / Update / WriteTombstone / RecordApplied in a single SQLite transaction; implements `domain.Writer` interface; marks `applied_mutations` row on every non-NoOp action.
- [x] 3.5 Integration test (`internal/localstore/store_test.go`): open temp SQLite file; insert a record; verify FTS roundtrip (insert → FTS search by title); soft-delete → verify row excluded from FTS; tombstone written atomically.

---

## Phase 4: Pure `Decide()` + `writeWins()` + Unit Tests (INV 1, 3, 5, 6)

> Spec traceable: reconciliation INV 1 (topic identity), INV 3 (no lost update), INV 5 (idempotent), INV 6 (independent writes).
> All tests use in-process mock Reader/Writer — zero DB.

- [x] 4.1 Create `internal/domain/reconcile.go` — implement `Decide(tx domain.Reader, m domain.Mutation) domain.Action` and `writeWins(m domain.Mutation, curUpdatedAt time.Time, curVersion int, curSeq int64) bool` exactly per design pseudocode. No imports outside `domain` package.
- [x] 4.2 Create `internal/domain/mock_reader_test.go` — in-process `mockReader` backed by a `map[string]*Record` for topic and sync_id lookups, a tombstone map, and an applied-mutations set; implements `domain.Reader`.
- [x] 4.3 Write unit test `TestDecide_INV1_TopicIdentityConvergence` (`internal/domain/reconcile_test.go`): Writer A upserts topic_key="sdd/test/explore" at T+100; Writer B upserts same at T+50; apply B → NoOp; apply A → Update; assert exactly one record with A's content.
- [x] 4.4 Write unit test `TestDecide_INV3_NoLostUpdate`: seed record at version=2, updatedAt=T+100; apply mutation version=1, updatedAt=T+50 → assert NoOp; stored row unchanged.
- [x] 4.5 Write unit test `TestDecide_INV5_IdempotentReApply`: apply same mutation_id twice; assert second call returns NoOp; applied_mutations has exactly one entry; version unchanged.
- [x] 4.6 Write unit test `TestDecide_INV6_IndependentWrites`: Writer A and B each upsert distinct sync_ids with no topic_key; apply both; assert both records inserted; zero conflicts.
- [x] 4.7 Write unit test `TestWriteWins_Tiebreakers`: verify primary wall-clock wins; then equal timestamp, higher version wins; then equal timestamp+version, higher seq wins.

---

## Phase 5: Central Postgres Schema + Push-Apply Adapter

> Spec traceable: reconciliation (central BIGSERIAL, UNIQUE topic index, tombstones, guarded UPDATE).

- [ ] 5.1 Create `internal/centralstore/schema.go` — `ApplySchema(db *sql.DB) error`; emits DDL for `central_mutations` (BIGSERIAL seq PK, mutation_id UNIQUE, writer_id, payload JSONB, occurred_at); `central_memories` (sync_id PK, all fields, embedding BYTEA reserved, `UNIQUE INDEX central_memories_topic_uidx` partial on `topic_key,project,scope WHERE topic_key IS NOT NULL AND deleted_at IS NULL`); `central_tombstones`; all indexes. Idempotent.
- [ ] 5.2 Create `internal/centralstore/apply.go` — `PushApply(db *sql.DB, m domain.Mutation) (seq int64, err error)`: wraps in Postgres transaction; inserts into `central_mutations` with `INSERT...RETURNING seq`; builds `pgReader` implementing `domain.Reader` against Postgres tables; calls `domain.Decide(pgReader, m)`; executes Action with SQL guards (`UPDATE...WHERE updated_at < $incoming_updated_at OR (updated_at = $incoming_updated_at AND version < $incoming_version)`); `ON CONFLICT(topic_key,project,scope) DO UPDATE WHERE` same predicate; returns assigned seq.
- [ ] 5.3 Create `internal/centralstore/pull.go` — `PullSince(db *sql.DB, sinceSeq int64) ([]domain.Mutation, error)`: `SELECT ... FROM central_mutations WHERE seq > $1 ORDER BY seq ASC`; unmarshals JSONB payload into Mutation.

---

## Phase 6: Writer Identity + Audit Stamping

> Spec traceable: writer-identity (every mutation carries writer_id, HMAC token, constant-time verify, audit trail on LWW discards).

- [ ] 6.1 Extend `cmd/engram/main.go` (or a `config.go` sibling): read `ENGRAM_WRITER_ID` and `ENGRAM_WRITER_SECRET` from env; call `domain.Identity{}.Verify(secret, sig)` on startup; fail fast with clear error if invalid.
- [ ] 6.2 Add `internal/centralstore/auth.go` — `ValidatePush(writerID, sig string, secret []byte) error`: constant-time HMAC verify; returns typed `ErrUnauthorized` on failure; all `PushApply` calls go through this gate first.
- [ ] 6.3 Create `internal/centralstore/audit.go` — `RecordLWWDiscard(db *sql.DB, writerID string, syncID string, updatedAt time.Time, reason string) error`: inserts into a `central_lww_discards` table (writerID, syncID, updatedAt, reason, recorded_at); add DDL for this table to `schema.go`.
- [ ] 6.4 Wire audit call: in `centralstore/apply.go`, when `Decide` returns `NoOp` on an Upsert (LWW discard), call `RecordLWWDiscard` before returning.
- [ ] 6.5 Unit test `TestIdentity_HMACRoundtrip` (`internal/domain/identity_test.go`): Sign then Verify passes; tampered sig fails; different secret fails.

---

## Phase 7: Two-Writer Convergence Spike + Acceptance Tests (INV 2, 4)

> Spec traceable: reconciliation INV 2 (monotonic seq), INV 4 (no resurrection); design (spike harness, two writers, real Postgres).

- [ ] 7.1 Create `internal/spike/spike.go` — `RunSpike(centralDSN string, secret []byte) error`: initializes two `Identity` instances (writerA, writerB) with distinct `WriterID`s; opens two temp SQLite stores (A.db, B.db) and one shared Postgres central; exports a `Writer` helper that calls `mutation.NewMutationID`, signs, and calls `centralstore.PushApply`.
- [ ] 7.2 Create `internal/spike/spike_test.go` — build tag `//go:build acceptance`; requires `ENGRAM_TEST_PG_DSN` env var; each test calls `RunSpike` or exercises scenario directly.
- [ ] 7.3 Write acceptance test `TestINV2_MonotonicSeq`: push 3 mutations interleaved from A and B; call `PullSince(0)`; assert `seqs[i+1] > seqs[i]` for all i; assert pull order == seq order regardless of `occurred_at`.
- [ ] 7.4 Write acceptance test `TestINV4_NoResurrection`: A pushes delete at T+200 (tombstone written); B pushes upsert for same sync_id at T+150; apply B → assert Decide returns NoOp (tombstone.deleted_at >= mutation.updated_at); after pull, both local stores have `deleted_at NOT NULL`; tombstone row present.
- [ ] 7.5 Verify INV 1 end-to-end with real Postgres (`TestINV1_E2E_TopicIdentityConvergence`): same scenario as unit test 4.3 but through `PushApply` and `PullSince`; both SQLite stores converge to one row with A's content.
- [ ] 7.6 Verify INV 5 end-to-end (`TestINV5_E2E_IdempotentReApply`): push same mutation_id twice to central; assert `central_mutations` has exactly one row for that ID; no version churn.

---

## Phase 8: Wiring + Smoke Test

> Design: `cmd/engram/main.go` wires adapters; spike run validates end-to-end path before archive.

- [ ] 8.1 Wire `cmd/engram/main.go`: open local SQLite; open Postgres (from `ENGRAM_PG_DSN` env); instantiate Identity from env; define a thin `PushAndPull` function that pushes a test mutation, reads back seq, pulls since 0, applies locally — used as smoke check only (not production transport).
- [ ] 8.2 Run `go build ./...` — zero errors, zero CGO, single static binary target confirmed.
- [ ] 8.3 Run `go vet ./...` and `go test ./internal/domain/... ./internal/localstore/... ./internal/mutation/...` (no build tags) — all pass without Postgres.
- [ ] 8.4 Run acceptance suite: `go test -tags acceptance ./internal/spike/... -v` with `ENGRAM_TEST_PG_DSN` set; all 4 acceptance tests pass.
