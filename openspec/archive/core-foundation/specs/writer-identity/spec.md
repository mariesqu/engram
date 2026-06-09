# writer-identity Specification

## Purpose

Defines the minimal per-writer identity required for the two-writer convergence spike. Every mutation MUST carry a stable writer identifier so that attribution is auditable and multi-writer scenarios are unambiguous. This is spike-grade identity — not a full auth system. Full JWT/refresh/RBAC is a future change.

---

## Requirements

### Requirement: Every mutation carries a writer_id

Every mutation pushed to the central store MUST include a `writer_id` field identifying the originating writer. The central store MUST persist `writer_id` on the `memories` row (or its associated mutation record). Mutations without `writer_id` MUST be rejected.

#### Scenario: Mutation with writer_id accepted

- GIVEN a writer with `writer_id = 'writer-alice'`
- WHEN the writer pushes a mutation including `writer_id`
- THEN the mutation is accepted and the stored row records `writer_id = 'writer-alice'`

#### Scenario: Mutation without writer_id rejected

- GIVEN an incoming push with no `writer_id` field
- WHEN the central store processes the push
- THEN the request is rejected with a validation error before any row is written

---

### Requirement: Per-writer signed token (minimum)

Each writer MUST be issued a signed token that the central store validates on every push. The token MUST encode at minimum: `writer_id` and an expiry. The central store MUST reject mutations accompanied by an invalid or expired token.

The signing mechanism MUST be symmetric (HMAC-SHA256) or asymmetric (RS256) — implementation choice belongs to the design phase. This spec requires only that a signed token mechanism EXISTS and is ENFORCED.

#### Scenario: Valid token accepted

- GIVEN a writer presents a valid, unexpired token encoding `writer_id = 'writer-bob'`
- WHEN the writer pushes a mutation
- THEN the push is accepted and `writer_id` on the stored row matches `'writer-bob'`

#### Scenario: Expired token rejected

- GIVEN a writer presents a token that has passed its expiry
- WHEN the writer pushes a mutation
- THEN the central store returns HTTP 401 and no row is written

#### Scenario: Tampered token rejected

- GIVEN a writer presents a token with a manipulated signature
- WHEN the central store validates the token
- THEN the request is rejected with HTTP 401

---

### Requirement: writer_id on LWW audit trail

When LWW discards an incoming write (because the stored row is newer), the central store MUST log the discarded write's `writer_id`, `updated_at`, and `sync_id` to an audit log for observability. The audit log MUST NOT be stored in the `memories` table itself.

#### Scenario: LWW discard is auditable

- GIVEN Writer B's mutation is discarded because Writer A's is newer
- WHEN the discard happens
- THEN an audit entry is written recording `writer_id = 'writer-b'`, the incoming `sync_id`, and the reason `'lww_discarded'`

---

### Requirement: Distinct writer_ids required in spike

The convergence spike MUST use at least two writers with distinct `writer_id` values. The spike harness MUST NOT reuse the same `writer_id` across simulated writers.

#### Scenario: Spike has two distinct writers

- GIVEN the spike initializes two writer processes/goroutines
- WHEN each writer is configured
- THEN Writer A has `writer_id = 'writer-a'` and Writer B has `writer_id = 'writer-b'` (or equivalent distinct values)
- AND both writer_ids are reflected in the stored mutation rows after the spike completes

---

### Requirement: writer_id propagated to local store

The local store MUST record `writer_id` on locally-written rows so that attribution is preserved before the push cycle completes.

#### Scenario: Local write captures writer_id

- GIVEN a local write is issued by a writer with `writer_id = 'writer-carol'`
- WHEN the row is saved to the local SQLite store
- THEN querying the row returns `writer_id = 'writer-carol'`

#### Scenario: Remote mutation retains original writer_id after pull

- GIVEN Writer A's mutation (writer_id = 'writer-a') is pulled from central by Writer B's client
- WHEN the mutation is applied to Writer B's local store
- THEN the locally stored row reflects `writer_id = 'writer-a'` (origin preserved, not overwritten)
