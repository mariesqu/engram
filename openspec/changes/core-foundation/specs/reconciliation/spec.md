# reconciliation Specification

## Purpose

Defines the central-authoritative reconciliation contract. The central store (Postgres) is the single source of truth for canonical ordering, identity deduplication, and conflict resolution. This spec formally encodes the six two-writer convergence invariants that the spike MUST prove.

---

## Requirements

### Requirement: Central store assigns monotonic seq (Invariant 2)

The central store MUST assign a monotonic BIGSERIAL `seq` to every accepted mutation at INSERT time. `seq` values MUST be assigned by the central store only — clients MUST NOT supply or influence `seq`. Clients polling `GET /mutations?since_seq={n}` MUST receive mutations in strict ascending `seq` order.

#### Scenario: Push accepted and seq assigned

- GIVEN a client pushes a mutation with no `seq` field
- WHEN the central store accepts the mutation
- THEN the row is assigned a `seq` value greater than all previously assigned values
- AND the client receives the assigned `seq` in the response

#### Scenario: Poll returns mutations in seq order

- GIVEN the central store has mutations at seq 10, 11, 12
- WHEN a client polls `since_seq = 9`
- THEN the response contains seq 10, 11, 12 in that exact ascending order

#### Scenario: Client clock skew does not affect ordering

- GIVEN Writer A pushes a mutation with `updated_at` far in the future (clock skew)
- WHEN the central store applies it
- THEN the row's `seq` is still assigned by server insertion order, not by `updated_at`

---

### Requirement: Topic-key identity convergence (Invariant 1)

The central store MUST enforce `UNIQUE(topic_key, project, scope)` (excluding NULL topic_key and soft-deleted rows) so that two writes to the same `(topic_key, project, scope)` tuple converge to exactly ONE row — the one with the greater `updated_at`.

When a topic_key conflict is detected, the central store MUST apply Last-Write-Wins (LWW) by `updated_at`: the incoming write is applied only if `incoming.updated_at > stored.updated_at`. If the condition is not met, the incoming write MUST be silently discarded (not an error).

#### Scenario: Duplicate topic_key write from two writers — newer wins

- GIVEN Writer A has written `topic_key='sdd/test/explore'` at `updated_at = T+100`
- WHEN Writer B pushes the same `topic_key` with `updated_at = T+50` (older)
- THEN the central store discards B's write
- AND after both writers complete a full sync cycle, BOTH local stores contain EXACTLY ONE row for that topic_key with A's content

#### Scenario: Duplicate topic_key write — even newer replaces current

- GIVEN a row for `topic_key='sdd/test/spec'` stored at `updated_at = T+50`
- WHEN a write arrives for the same topic_key with `updated_at = T+100`
- THEN the central store applies the update and the row reflects the new content

#### Scenario: New write without topic_key does not trigger deduplication

- GIVEN no topic_key is set on an incoming write
- WHEN the central store processes the write
- THEN the row is inserted as a new independent row without conflict checks on topic_key

---

### Requirement: No lost updates — LWW version guard (Invariant 3)

Every UPDATE on the central store MUST include a guard: the update is applied only when `stored.updated_at < incoming.updated_at` AND `stored.version < incoming.version`. An older arriving write MUST NOT overwrite a newer stored row.

When the guard condition is not met, the central store MUST return the current row state with a 409 Conflict status so the client can decide to discard or merge.

#### Scenario: Newer write stored; older write arrives late

- GIVEN the central store holds a row at `updated_at = T+100, version = 5`
- WHEN a write arrives for the same row with `updated_at = T+50, version = 3` (older)
- THEN the central store rejects the update
- AND the stored row remains at `updated_at = T+100, version = 5`
- AND the response carries HTTP 409 with the current row

#### Scenario: Version guard as secondary tiebreaker on equal timestamps

- GIVEN two writes share the same `updated_at` but carry `version = 2` (incoming) vs `version = 3` (stored)
- WHEN the incoming write is processed
- THEN the stored row (version 3) is preserved; incoming (version 2) is discarded

---

### Requirement: No soft-delete resurrection — tombstone blocks upsert (Invariant 4)

The system MUST maintain a `memory_tombstones` table recording `(sync_id, deleted_at, deleted_by, version, seq)` for every deleted row. When processing an upsert for a given `sync_id`, the central store MUST check the tombstone table first. A tombstone MUST be written atomically with the soft-delete of the `memories` row.

**Tombstone supersede rule (precise — authoritative):**

An incoming upsert supersedes an existing tombstone if and only if it wins the full four-level tiebreaker chain against the tombstone's `(deleted_at, version, deleted_by, sync_id)`:

1. `incoming.updated_at > tombstone.deleted_at` — the incoming write is strictly newer (wall-clock). If so, the upsert MUST supersede (delete is revived).
2. If timestamps are equal: `incoming.version > tombstone.version` — higher version wins. If so, the upsert MUST supersede.
3. If timestamps and versions are both equal: `incoming.writer_id > tombstone.deleted_by` — higher writer_id wins. If so, the upsert MUST supersede.
4. If timestamps, versions, and writer_ids are all equal: `incoming.sync_id > tombstone.sync_id` — higher sync_id wins. Full equality returns false (deterministic no-op).

**Why identity fields and not central seq:**
Central `seq` is ASYMMETRIC — a node's own tombstones keep `seq=0` permanently (AckMutation never back-patches the central-assigned seq; self-authored mutations pulled back are INV5 NoOps). This means the authoring node and central computed different seq-based tie winners, causing permanent split-brain. `writer_id` and `sync_id` are derived from the mutation payload and are REPLICA-IDENTICAL: every store derives them from the same data with no central back-channel. Divergence at the exact (updated_at, version) tie is STRUCTURALLY IMPOSSIBLE.

This is identical to the `writeWins(incoming, tombstone.deleted_at, tombstone.version, tombstone.deleted_by, tombstone.sync_id)` function used for live-record conflicts. The rule is consistent: (writer_id, sync_id) is the identity tiebreaker in ALL conflict paths.

**Block condition**: An upsert MUST be blocked (NoOp) when `writeWins` returns false against the tombstone — i.e., the incoming write does NOT win the chain above.

#### Scenario: Delete followed by late-arriving update — delete wins (timestamp clear)

- GIVEN Writer A deletes `sync_id = 'mem-42'` at `deleted_at = T+200, version = 2` (tombstone written)
- WHEN Writer B pushes an update with `updated_at = T+150, version = 1`
- THEN `writeWins(B, T+200, 2, ...)` returns false (`T+150 < T+200`)
- AND the upsert MUST be blocked (NoOp)
- AND after full sync both local stores show `mem-42` as deleted

#### Scenario: Update strictly newer than tombstone (timestamp) supersedes

- GIVEN a tombstone for `sync_id = 'mem-99'` at `deleted_at = T+100, version = 1`
- WHEN a write arrives with `updated_at = T+200, version = 2`
- THEN `writeWins(incoming, T+100, 1, ...)` returns true (`T+200 > T+100`)
- AND the tombstone MUST be superseded, the row undeleted and updated
- AND `deleted_at` MUST be cleared on the memories row

#### Scenario: Equal timestamp and version — writer_id is the final tiebreaker (higher writer_id supersedes)

- GIVEN a tombstone for `sync_id = 'mem-55'` at `deleted_at = T, version = 1, deleted_by = 'writer-A'`
- WHEN a write arrives with `updated_at = T` (equal), `version = 1` (equal), `writer_id = 'writer-B'` (higher)
- THEN `writeWins(incoming, T, 1, 'writer-A', ...)` returns true (`'writer-B' > 'writer-A'`)
- AND the tombstone MUST be superseded (delete is revived; action is Insert or Update, NOT NoOp)

#### Scenario: Equal timestamp, version, and writer_id — sync_id is the final tiebreaker

- GIVEN a tombstone for `sync_id = 'sync-A'` at `deleted_at = T, version = 1, deleted_by = 'writer-X'`
- WHEN a write arrives with `updated_at = T` (equal), `version = 1` (equal), `writer_id = 'writer-X'` (equal), `sync_id = 'sync-Z'` (higher)
- THEN `writeWins(incoming, T, 1, 'writer-X', 'sync-A')` returns true (`'sync-Z' > 'sync-A'`)
- AND the tombstone MUST be superseded

#### Scenario: Equal timestamp, version, and writer_id — lower or equal sync_id is blocked

- GIVEN a tombstone for `sync_id = 'sync-Z'` at `deleted_at = T, version = 1, deleted_by = 'writer-X'`
- WHEN a write arrives with `updated_at = T` (equal), `version = 1` (equal), `writer_id = 'writer-X'` (equal), `sync_id = 'sync-A'` (lower)
- THEN `writeWins(incoming, T, 1, 'writer-X', 'sync-Z')` returns false (`'sync-A' < 'sync-Z'`)
- AND the upsert MUST be blocked (NoOp)

#### Scenario: Tombstone written atomically with soft-delete

- GIVEN a live row with `sync_id = 'mem-77'`
- WHEN a delete operation is issued
- THEN both the `memories.deleted_at` field and the `memory_tombstones` row are written in the same transaction
- AND a failure in either write causes a full rollback

---

### Requirement: Idempotent re-apply (Invariant 5)

Re-applying the same mutation (same `sync_id`, same `updated_at`, same content hash) MUST be a no-op. The central store MUST NOT create duplicate rows, increment version, or update timestamps when the incoming write is identical to the stored row.

#### Scenario: Re-applying the same push is a no-op

- GIVEN a mutation with `sync_id = 'mem-5'` has already been applied at `updated_at = T`
- WHEN the same mutation is pushed again (network retry)
- THEN the row count for `sync_id = 'mem-5'` remains 1
- AND `version` is unchanged
- AND `updated_at` is unchanged

#### Scenario: Re-applying same content-addressed chunk skips insert

- GIVEN a content-addressed chunk ID is already recorded as applied
- WHEN a push batch includes the same chunk ID
- THEN the system skips the apply step and returns success without modifying any row

---

### Requirement: Independent new writes preserved (Invariant 6)

Concurrent writes to DIFFERENT records (no shared topic_key) MUST all survive after sync. The system MUST NOT treat distinct `sync_id` values as conflicting even when pushed near-simultaneously.

#### Scenario: Two writers each create new records concurrently

- GIVEN Writer A writes a new record with `sync_id = 'mem-a1'` (no topic_key)
- AND Writer B writes a new record with `sync_id = 'mem-b1'` (no topic_key) at approximately the same time
- WHEN both push their mutations to central
- THEN after full sync both local stores contain BOTH rows (`mem-a1` and `mem-b1`)

#### Scenario: Two writers create records with different topic_keys

- GIVEN Writer A writes `topic_key = 'sdd/proj-a/spec'`
- AND Writer B writes `topic_key = 'sdd/proj-b/spec'`
- WHEN both push their mutations to central
- THEN both rows are present after sync — no false conflict

---

### Requirement: Central unique constraint on topic identity

The central Postgres schema MUST define:

```
UNIQUE INDEX memories_topic_identity_uidx
  ON memories(topic_key, project, scope)
  WHERE topic_key IS NOT NULL AND deleted_at IS NULL
```

This constraint MUST be the enforcement point for identity convergence. Upsert logic MUST use `ON CONFLICT` on this index.

#### Scenario: DB constraint prevents duplicate topic rows

- GIVEN a row for `(topic_key='k', project='p', scope='project')` exists
- WHEN a second INSERT for the same tuple is attempted without using the upsert path
- THEN the DB raises a unique constraint violation

---

### Requirement: Client push/pull cycle

The client MUST implement a push/pull cycle:

1. Write locally → enqueue in `sync_mutations` (local, auto-increment seq for push ordering).
2. Push batch of pending mutations to central (`POST /mutations`).
3. Central assigns BIGSERIAL seq, applies LWW-guarded upsert, returns assigned seqs.
4. Client acks local mutations (marks `acked_at`).
5. Client polls central `GET /mutations?since_seq={last_pulled}`, receives remote mutations.
6. Client applies remote mutations locally using the same LWW guard.
7. Client advances `last_pulled_seq` to the max received seq.

The push step MUST NOT block the pull step; they MAY run sequentially or concurrently.

#### Scenario: Client pushes and pulls in one cycle

- GIVEN a client has 2 pending local mutations and the central store has 1 remote mutation not yet seen
- WHEN the client executes a sync cycle
- THEN 2 mutations are pushed, their seqs are acked, and 1 remote mutation is pulled and applied locally

#### Scenario: Pull with no new mutations is a no-op

- GIVEN a client is fully up-to-date (last_pulled_seq = central max seq)
- WHEN the client polls since_seq
- THEN the response is empty and the client's last_pulled_seq is unchanged
