# Embedding Backfill Specification

## Purpose

Defines the background loop that populates `embedding IS NULL` rows with
vectors from the configured `EmbeddingProvider`, plus the on-write async
embedding path. Both paths share the same idempotency predicate, the same
per-row privacy gate, and the same skip-on-error strategy so that neither can
block the write path or crash the daemon.

---

## Requirements

### Requirement: On-write embedding — write path never blocks on the provider

When a `LocalWrite` succeeds for a `synced` project with a non-Noop provider
configured, the daemon SHALL attempt to embed the newly written row
asynchronously. "Asynchronous" means the `mem_save` tool response is returned to
the caller BEFORE the embedding call is initiated or awaited. The write path MUST
NOT wait for the provider response.

A provider failure on the on-write path SHALL leave the row with `embedding IS
NULL` and make it eligible for the backfill loop. No error is returned to the
MCP caller.

The on-write path is subject to the same privacy gate as the backfill loop
(see embedding-privacy spec). It MUST call `EligibleForEmbedding` before calling
`provider.Embed`.

> Headless testable: yes.

#### Scenario: On-write async — tool responds before embedding completes

- GIVEN a provider with an artificial 200ms delay
- WHEN `mem_save` writes an observation to a `synced` project
- THEN the `mem_save` tool response is received well before 200ms elapses
- AND the row eventually has `embedding IS NOT NULL` (after the delay resolves)

#### Scenario: On-write — provider error leaves row for backfill

- GIVEN a provider that always returns an error
- WHEN `mem_save` writes to a `synced` project
- THEN the tool returns success
- AND the row has `embedding IS NULL`

---

### Requirement: Backfill loop — idempotency predicate

The backfill loop SHALL select rows using the predicate:

```sql
WHERE embedding IS NULL AND deleted_at IS NULL
```

Additionally, when the configured model has changed (i.e., `ModelName()` differs
from the currently stored `embedding_model`), the loop SHALL also process rows
where:

```sql
WHERE embedding_model != <current_model> AND deleted_at IS NULL
```

This combined predicate ensures:
1. Unembedded rows are always caught.
2. Model-change re-embedding happens via the same loop, no separate migration
   tool required.

Running the loop when no eligible rows exist MUST be a no-op (no provider calls,
no writes). Running the loop twice on the same unchanged store MUST produce no
duplicate embeddings and no spurious writes.

> Headless testable: yes.

#### Scenario: Idempotency — loop run twice produces no duplicates

- GIVEN 5 rows with `embedding IS NULL` for project "open" (policy `synced`)
- AND a recording mock provider that returns deterministic vectors
- WHEN the backfill loop is run to completion twice
- THEN each row has exactly one embedding (not two)
- AND the provider received exactly 5 `Embed` calls total (not 10)

#### Scenario: Loop is a no-op when all rows are already embedded

- GIVEN all `memories` rows have `embedding IS NOT NULL` for the current model
- WHEN the backfill loop runs
- THEN the provider receives zero `Embed` calls
- AND no rows are updated

#### Scenario: Model change — stale rows are re-embedded

- GIVEN 3 rows with `embedding_model = "old-model"` and valid embeddings
- AND the configured provider's `ModelName()` returns `"text-embedding-3-small"`
- WHEN the backfill loop runs
- THEN the 3 rows are re-embedded and their `embedding_model` updated to `"text-embedding-3-small"`

---

### Requirement: Backfill loop — batch size and rate limiting

The loop SHALL process rows in batches of at most `100` texts per `Embed` call.
After each successful batch the loop SHALL pause for at least `1 second` before
fetching the next batch. This prevents runaway API costs and rate-limit
violations.

The batch size and inter-batch delay SHOULD be configurable at construction time
(defaulting to 100 and 1s respectively) to facilitate fast-running tests.

> Headless testable: yes — configure batch_size=2, delay=0 in tests.

#### Scenario: Batch size respected — rows split across multiple calls

- GIVEN 250 eligible rows (policy `synced`)
- AND a recording mock provider
- AND backfill loop constructed with `batch_size=100`
- WHEN the backfill loop runs to completion
- THEN the provider received exactly 3 `Embed` calls (100 + 100 + 50)

---

### Requirement: Backfill loop — per-row privacy gate

Before including a row in a batch for `Embed`, the loop SHALL call
`GetPolicy(row.Project)` and pass the result through the `EligibleForEmbedding`
gate. Rows that fail the gate SHALL be silently skipped (NOT counted as errors,
NOT written, NOT retried in the current run). They remain with `embedding IS
NULL` and will be re-evaluated on the next loop run or when policy changes.

The gate check is per-row. A single batch MAY skip rows from ineligible projects
and embed rows from eligible projects in the same iteration.

> Headless testable: yes — mix of synced and omitted rows in same store.

#### Scenario: Mixed-policy batch — only synced rows are embedded

- GIVEN a store with 4 rows: 2 for project "open" (policy `synced`) and 2 for project "secret" (policy `omitted`)
- AND all 4 rows have `embedding IS NULL`
- AND a recording mock provider
- WHEN the backfill loop runs
- THEN the provider receives `Embed` calls only for "open" texts
- AND the 2 "secret" rows still have `embedding IS NULL` after completion

---

### Requirement: Backfill loop — skip-on-error, retry next tick

When a provider call fails for a batch, the rows in that batch SHALL be left
with `embedding IS NULL`. The loop SHALL log the error and continue to the next
batch. The loop MUST NOT abort the entire backfill on a single batch error.

Rows that failed will be retried on the next loop invocation (they still match
the `embedding IS NULL` predicate). This is the "retry next tick" behavior —
no per-row error state, no dead-letter queue.

> Headless testable: yes — inject a provider that fails on the second call.

#### Scenario: Batch failure — loop continues, failed rows remain for next run

- GIVEN 6 rows (all `synced`, all `embedding IS NULL`) in batches of 3
- AND a provider that succeeds on call 1 (rows 1-3) and errors on call 2 (rows 4-6)
- WHEN the backfill loop runs
- THEN rows 1-3 have `embedding IS NOT NULL`
- AND rows 4-6 still have `embedding IS NULL`
- AND no error is surfaced to the caller (the loop returns without error)

---

### Requirement: Backfill loop — stops when caught up, resumable

The loop SHALL stop naturally when no eligible rows remain (the `embedding IS
NULL` predicate returns zero rows). It MUST NOT run indefinitely when the work
is done.

The loop SHALL be resumable: if the daemon is stopped mid-backfill, restarting
the daemon and re-running the loop picks up exactly the remaining
`embedding IS NULL` rows. No checkpoint table or offset is required; the
predicate itself is the cursor.

> Headless testable: yes.

#### Scenario: Loop stops when all rows are embedded

- GIVEN a store with 10 rows, all `embedding IS NULL`
- AND a working provider
- WHEN the backfill loop runs
- THEN it terminates (does not spin indefinitely)
- AND all 10 rows have `embedding IS NOT NULL`

#### Scenario: Resumability — interrupted backfill picks up remaining rows

- GIVEN 10 rows with `embedding IS NULL`
- AND the loop is stopped (context cancelled) after embedding 4 rows
- WHEN the loop is restarted with a new context
- THEN the remaining 6 rows are embedded
- AND the 4 already-embedded rows are not re-embedded (idempotency)

---

### Requirement: Backfill loop — observability

`GET /api/v1/status` SHALL include an `embedding_backfill` sub-object with at
minimum:

```json
"embedding_backfill": {
  "pending": <integer>,    // rows with embedding IS NULL AND policy-eligible (live, non-deleted)
  "provider": "<string>"   // ModelName() of the current provider, or "noop"
}
```

The `pending` count SHALL reflect only rows that the backfill loop will actually
process — i.e., rows with `embedding IS NULL AND deleted_at IS NULL` where the
project's policy permits embedding. Omitted projects and non-consented local-only
projects are excluded from the count. This ensures `pending` reflects work the
loop will do, not forever-pending rows it correctly skips.

The count reflects the current store state at the time of the request. It is a
best-effort count (it does not need to be transactionally exact); a ±1 race with
a concurrent write is acceptable.

A `pending` value of `0` means the backfill is caught up (all eligible rows are embedded).

If no provider is configured (NoopProvider), `provider` SHALL be `"noop"` and
`pending` SHALL still be returned (allowing the user to see how many rows would
be backfilled if they configure a provider).

> RECONCILED at archive: per design decision 3 (policy-gated backfill), the pending
> count must count only eligible rows the loop will embed, not raw NULL rows in
> omitted/gated projects that the loop correctly never touches.

> Headless testable: yes — integration test against embedded store.

#### Scenario: Status shows pending count

- GIVEN a store with 7 rows with `embedding IS NULL` and a `synced` project
- WHEN `GET /api/v1/status` is called
- THEN the response includes `"embedding_backfill": {"pending": 7, "provider": "..."}`

#### Scenario: Status shows zero pending when all rows embedded

- GIVEN all `memories` rows have `embedding IS NOT NULL`
- WHEN `GET /api/v1/status` is called
- THEN `"embedding_backfill": {"pending": 0, ...}` is present in the response

---

> **Headless testability**: All requirements in this spec are provable headlessly
> with a fast-forward loop configuration (batch_size=2, delay=0) and an
> in-process recording mock provider. The status endpoint test uses an embedded
> SQLite store and the existing `httptest` infrastructure. No tray or browser required.
