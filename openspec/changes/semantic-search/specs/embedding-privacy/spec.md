# Embedding Privacy Specification

## Purpose

Defines the privacy gate that sits between all memory text and the embedding
provider. This is the highest-stakes correctness invariant in the semantic-search
change: a bug here sends `omitted` or `local-only` memory text to a remote API,
violating the user's explicit privacy expectation. The gate is normative — it is
not optional, not configurable, and not bypassable.

---

## Requirements

### Requirement: Single choke-point before every provider call

Every provider call — whether triggered by the on-write path (post-`LocalWrite`)
or the background backfill loop — SHALL pass through a single gate function
before the `EmbeddingProvider.Embed` method is invoked.

There SHALL be NO code path that calls `provider.Embed` without first passing
through this gate. The gate is the only location where policy is consulted for
embedding eligibility; duplicating the check at call-sites is explicitly
forbidden (it would allow future call-sites to bypass it by omission).

The gate function signature (normative shape):

```go
// EligibleForEmbedding reports whether the memory with the given project is
// permitted to be embedded given the current policy and provider.
// It returns false (not an error) when the row must be skipped silently.
func EligibleForEmbedding(policy localstore.Policy, providerIsRemote bool) bool
```

The gate MUST be a pure, synchronous, zero-IO function. It takes the resolved
policy (already fetched from `GetPolicy`) and a boolean that signals whether the
configured provider sends text off-node (`true` for `RemoteOpenAIProvider`,
`false` for `NoopProvider` and any future local-sidecar provider once consent is
granted in PR-2). The gate returns `true` only when embedding is permitted.

> Headless testable: yes — unit test the gate as a pure function with all
> six input combinations.

#### Scenario: Gate — all six policy × provider-locality combinations

| policy       | providerIsRemote | EligibleForEmbedding |
|--------------|------------------|----------------------|
| `omitted`    | `true`           | `false`              |
| `omitted`    | `false`          | `false`              |
| `local-only` | `true`           | `false`              |
| `local-only` | `false`          | `false` (PR-1; PR-2 adds consent) |
| `synced`     | `true`           | `true`               |
| `synced`     | `false`          | `true`               |

- GIVEN each row of the table above
- WHEN `EligibleForEmbedding(policy, providerIsRemote)` is called
- THEN it returns the value in the last column

---

### Requirement: omitted projects — provably zero provider calls

When a project's policy is `omitted`, its memory text SHALL NEVER be passed to
any `EmbeddingProvider`, whether on the write path or the backfill path.

This requirement SHALL be proven headlessly using a **recording mock provider**:
a test `EmbeddingProvider` that records every `texts` slice it receives.

> Headless testable: yes — required.

#### Scenario: omitted project — recording mock receives zero calls on write

- GIVEN a recording mock provider
- AND project "secret" has policy `omitted`
- WHEN `mem_save` writes an observation to "secret"
- THEN the recording mock records ZERO `Embed` calls
- AND the `memories` row for "secret" has `embedding IS NULL`

#### Scenario: omitted project — recording mock receives zero calls during backfill

- GIVEN a recording mock provider
- AND the `memories` table contains 3 rows for project "secret" (policy `omitted`) with `embedding IS NULL`
- WHEN the backfill loop runs to completion
- THEN the recording mock records ZERO `Embed` calls carrying any of "secret"'s text

---

### Requirement: local-only projects — provably zero remote provider calls (PR-1)

When a project's policy is `local-only`, its memory text SHALL NEVER be sent to
a **remote** provider (i.e., any provider where `providerIsRemote == true`). This
holds for both the on-write path and the backfill path.

In PR-1 (remote-only provider scope), `local-only` is effectively treated the
same as `omitted` for embedding eligibility: no embedding is produced. PR-2
introduces the `OllamaSidecarProvider` (`providerIsRemote == false`) and a
SEPARATE explicit consent setting; without that consent, even a local sidecar
MUST NOT embed `local-only` text.

The `local-only` embargo SHALL be proven using the same recording mock technique
as the `omitted` requirement.

> Headless testable: yes — required.

#### Scenario: local-only project — recording mock receives zero calls on write (remote provider)

- GIVEN a recording mock provider configured as remote
- AND project "private" has policy `local-only`
- WHEN `mem_save` writes an observation to "private"
- THEN the recording mock records ZERO `Embed` calls
- AND the `memories` row has `embedding IS NULL`

#### Scenario: local-only project — recording mock receives zero calls during backfill (remote provider)

- GIVEN a recording mock remote provider
- AND 5 rows for project "private" (policy `local-only`) with `embedding IS NULL`
- WHEN the backfill loop runs
- THEN the recording mock records ZERO `Embed` calls containing "private" text

---

### Requirement: synced projects are embeddable with a configured provider

When a project's policy is `synced` and a real provider (non-Noop) is configured
with a valid key, its memory text SHALL be embedded on the write path (async)
and/or during backfill.

> Headless testable: yes.

#### Scenario: synced project text IS embedded

- GIVEN a recording mock remote provider
- AND project "open" has policy `synced`
- WHEN `mem_save` writes an observation to "open" (on-write embed triggered)
- THEN the recording mock receives an `Embed` call
- AND the call's `texts` slice contains the memory's title and/or content

---

### Requirement: Policy flip during backfill — gate re-evaluated per row

The gate is evaluated per row at the moment the backfill loop processes that
row. If a project's policy changes mid-backfill (e.g., `synced` → `omitted`),
rows processed AFTER the flip SHALL be gated out and left with `embedding IS
NULL`. The loop MUST NOT read policy once for a whole batch; it MUST call
`GetPolicy` for each row (or at minimum once per project per batch iteration).

> Headless testable: yes — inject a policy store that flips mid-run.

#### Scenario: Policy flip mid-backfill — post-flip rows skipped

- GIVEN 10 rows for project "flip-me" (policy `synced`) with `embedding IS NULL`
- AND a recording mock provider
- AND a test harness that changes project "flip-me" policy to `omitted` after 5 rows are processed
- WHEN the backfill loop runs
- THEN the recording mock receives `Embed` calls for at most 5 rows (those before the flip)
- AND the remaining rows still have `embedding IS NULL` after the loop completes

---

### Requirement: Embedding consent is decoupled from sync policy (PR-2 forward-reference)

The gate for local-sidecar embedding on `local-only` projects SHALL require a
SEPARATE explicit consent boolean (`embedding_local_consent` or equivalent),
distinct from the sync policy. This consent setting is out of scope for PR-1
but its absence MUST be the safe default: without the consent field (or with it
`false`), `local-only` projects are never embedded even by a local-sidecar
provider.

This requirement is stated here to prevent PR-1 from wiring the gate in a way
that would implicitly authorize sidecar embedding (e.g., by treating
`providerIsRemote == false` as unconditionally eligible for `local-only`).

> Headless testable: yes — the gate's `local-only + non-remote` cell returns
> `false` in PR-1, confirmed by the gate unit test table.

---

> **Headless testability**: All requirements in this spec are provable headlessly
> using in-process recording mock providers and direct SQLite inspection.
> The privacy proof — recording mock asserts zero calls for `omitted`/`local-only`
> text — is a MANDATORY test, not optional.
