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

- [x] 5.1 (PR3a) Create `internal/centralstore/schema.go` — `ApplySchema(ctx, pool)`: `central_mutations` (BIGSERIAL seq PK, mutation_id UNIQUE, writer_id, payload JSONB, occurred_at); `central_memories` (sync_id PK, embedding BYTEA reserved, partial `central_memories_topic_uidx` on `topic_key,project,scope WHERE topic_key IS NOT NULL AND deleted_at IS NULL`); `central_tombstones` (+ partial `central_tombstones_topic_uidx`); `cloud_sync_audit`; all indexes. Idempotent.
- [x] 5.2 (PR3a + PR3b) Create `internal/centralstore/store.go` + `apply.go` — `Store` over `pgxpool.Pool`; `domain.Reader` impl + write primitives (`InsertMutation` RETURNING seq, `UpsertMemory`, `WriteTombstone`); `Store.Apply(ctx, m)`: Postgres tx, INV5 idempotency, INSERT RETURNING seq, `domain.Decide` via `decideReader`, guarded upsert/tombstone keyed by `Decision.TargetSyncID`. Implemented as the Decide-driven adapter (mirrors localstore.Apply) rather than a free `PushApply`. 23 acceptance tests PASS.
- [x] 5.3 (PR3c) Create `internal/centralstore/pull.go` — `Store.PullSince(ctx, project, sinceSeq, limit) ([]domain.Mutation, error)`: `WHERE project=$1 AND seq>$2 ORDER BY seq ASC LIMIT $3`; decodes each row via `mutation.FromCanonicalPayload`; fills MutationID/Seq/OccurredAt/Payload; asserts strict ascending seq. 28 acceptance tests PASS (5 new).

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

> IMPLEMENTATION NOTE (PR4): the harness was built as a UNIFIED push/pull driver
> (`internal/spike/harness.go`: `Node`, `Central` port, `Push`/`Pull`/`Sync`/`SyncAll`)
> on top of a real local SYNC API added to localstore (`LocalWrite`, `DrainOutbox`,
> `AckMutation`, `PullCursor`/`SetPullCursor`, `ApplyPulled`) — NOT a one-shot
> `RunSpike`/`spike.go`. Writer-identity HMAC (Phase 6) is deferred per scope; the
> harness mints two writers via distinct `WriterID` strings on the mutations. The
> convergence tests prove all SIX invariants end-to-end (not just INV2/INV4).

- [x] 7.1 Create `internal/spike/harness.go` — `Node` (one localstore.Store + outbox/cursor), `Central` port (Apply + PullSince, satisfied by *centralstore.Store), `Push`/`Pull`/`Sync`/`SyncAll`. Plus local sync API in `internal/localstore/sync.go` (LocalWrite/DrainOutbox/AckMutation/PullCursor/SetPullCursor/ApplyPulled/PendingCount) + `Store.DB()` accessor. 5 real-SQLite unit tests for the sync API.
- [x] 7.2 Create acceptance test files (`//go:build acceptance`): `convergence_acceptance_test.go` (embedded-postgres TestMain on own port + isolated-schema + node factory, mirrors centralstore_test; ENGRAM_TEST_PG_DSN override), `invariants_acceptance_test.go`, `helpers_acceptance_test.go`, `tsseq_probe_acceptance_test.go`. ACTUALLY RAN via embedded-postgres; 8 tests PASS, stable -count=2.
- [x] 7.3 `TestConvergence_INV2_MonotonicSeq` — interleaved A/B pushes; central_mutations.seq strictly increasing; central seq propagates onto a replica row on accept (assertPulledTopicHasSeq).
- [x] 7.4 `TestConvergence_INV4_NoResurrection` — A deletes T; B STALE upsert (older) stays deleted everywhere; strictly-newer upsert revives (deleted_at cleared on A.local/B.local/central; FindByTopic returns it). Plus `TestTsSeqProbe_EqualTimestampTombstoneTie` (the empirical ts.Seq boundary finding).
- [x] 7.5 `TestConvergence_INV1_TopicConvergence` — full push/pull cycle; one live row per topic on A.local/B.local/central with the winning content + central canonical sync_id. (Plus `TestConvergence_FullBidirectionalSettles`: A.local == B.local == central for a mixed live+tombstone workload.)
- [x] 7.6 `TestConvergence_INV5_Idempotent` — repeated sync no double-apply; central max seq + mutation count + version stable on no-op rounds. (Plus INV3 `TestConvergence_INV3_NoLostUpdate` and INV6 `TestConvergence_INV6_IndependentWrites`.)

---

## Phase 8: Wiring + Smoke Test

> Design: `cmd/engram/main.go` wires adapters; spike run validates end-to-end path before archive.

- [ ] 8.1 Wire `cmd/engram/main.go`: open local SQLite; open Postgres (from `ENGRAM_PG_DSN` env); instantiate Identity from env; define a thin `PushAndPull` function that pushes a test mutation, reads back seq, pulls since 0, applies locally — used as smoke check only (not production transport).
- [ ] 8.2 Run `go build ./...` — zero errors, zero CGO, single static binary target confirmed.
- [ ] 8.3 Run `go vet ./...` and `go test ./internal/domain/... ./internal/localstore/... ./internal/mutation/...` (no build tags) — all pass without Postgres.
- [ ] 8.4 Run acceptance suite: `go test -tags acceptance ./internal/spike/... -v` with `ENGRAM_TEST_PG_DSN` set; all 4 acceptance tests pass.
