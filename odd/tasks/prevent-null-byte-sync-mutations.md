# Prevent Null-Byte Sync Mutations

## Objective

Prevent new local writes containing U+0000 from creating PostgreSQL-incompatible sync mutations, return actionable client errors for unsanitized external mutations, and prove the behavior with focused and broad tests.

## Problem

SQLite and Go strings accept U+0000, while PostgreSQL `jsonb` rejects its JSON escape. A captured binary fragment therefore produced a permanently failing outbox mutation and an opaque central HTTP 500.

## Scope

- Sanitize U+0000 from newly created local memory and prompt mutations before canonical payload and mutation-ID derivation.
- Preserve all other characters, including U+0001.
- Reject externally supplied canonical payloads containing U+0000 with an actionable client error before PostgreSQL persistence.
- Add focused unit/integration coverage and run the relevant Go test suite.
- Run blind dual adversarial review after functional checks pass.

## Constraints

- Never sanitize payload bytes independently from the materialized mutation.
- Never rewrite an existing externally supplied payload/ID pair.
- Sanitized materialized fields, canonical payload, and content-addressed mutation ID must remain identical representations of the same logical write.
- No push or pull request.
- Engram recovery mirror remains pending because this session has no Engram MCP tools.

## Configuration

- TDD: off; no project or session TDD setting was found
- Test runner: `go test`
- Verification: focused package tests, broader suite, runtime PostgreSQL acceptance path where configured, and adversarial dual review
- Delivery strategy: exception-ok
- Forecast: under 400 authored lines
- Actual authored change: 702 lines; maintainer approved one commit on `feat/upstream-parity` instead of chained delivery

## Tasks

- [x] **NUL-001 — Sanitize local mutations before identity derivation**
  - Route: delegated
  - Trigger: coordinated production and test changes across multiple Go files
  - Acceptance: local memory and prompt writes remove only U+0000 before persistence, payload generation, and hashing
  - Checks: focused mutation/localstore/sync tests passed
  - Runtime evidence: local memory and prompt tests prove materialized fields, payload, and hash share the sanitized representation
  - Rollback boundary: revert the local mutation normalization and its focused tests
  - Commit evidence: pending final work-unit commit

- [x] **NUL-002 — Reject incompatible external payloads and verify end to end**
  - Route: delegated
  - Trigger: central HTTP/store boundary plus acceptance coverage
  - Acceptance: externally supplied U+0000 payloads return an actionable 4xx error rather than PostgreSQL HTTP 500; valid payloads remain unchanged
  - Checks: targeted packages, isolated full `go test ./...`, PostgreSQL acceptance test, and diff check passed
  - Runtime evidence: cloudserve tests prove hidden NUL forms return actionable HTTP 400 with zero central Apply calls
  - Rollback boundary: revert wire/central validation and focused tests together with NUL-001
  - Commit evidence: pending final work-unit commit

## Verification Evidence

- Targeted packages: passed.
- Full isolated `go test -count=1 ./...`: passed on clean rerun after one transient Windows temp-file rename failure also passed independently.
- PostgreSQL acceptance test `TestApply_RejectsNULBeforePostgresPersistence`: passed.
- `git diff --cached --check`: passed before this progress-only documentation update.
- Judgment Day round 1 found one confirmed critical wire-boundary bypass for hidden JSON fields.
- The bounded correction added full-token validation at `FromWire` plus focused unknown-field, object-key, and shadowed-duplicate tests.
- Both judges approved the scoped re-judgment with no remaining findings.
- Final independent verification passed against immutable code target `8f175c9343ae38e4a91dde2253e37af18afb7b72`.

## Next Step

Create one Conventional Commit on `feat/upstream-parity` under the approved size exception, without pushing.
