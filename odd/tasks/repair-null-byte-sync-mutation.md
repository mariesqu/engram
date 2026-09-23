# Repair Null-Byte Sync Mutation

## Objective

Sanitize the literal U+0000 byte from the affected local memory and pending mutation, retry synchronization, and prove the repaired mutation is acknowledged by the configured Aurora central store.

## Problem

Local mutation sequence 7880 contains a NUL byte in captured PDF content. PostgreSQL rejects its canonical JSON payload with SQLSTATE 22P05, permanently wedging the pending outbox entry.

## Scope

- Back up and integrity-check the local SQLite database.
- Atomically repair memory ID 5995 and mutation sequence 7880 while preserving logical version and timestamps.
- Recompute the content-addressed mutation ID.
- Restart Engram, retry synchronization, and verify local and remote state.

## Constraints

- Remove only U+0000; preserve U+0001 and all other content.
- Use strict preconditions and rollback on any mismatch.
- Do not modify Aurora directly; remote access remains read-only.
- Engram recovery mirror is pending because this session has no Engram MCP tools.

## Configuration

- TDD: not applicable to the one-off data repair
- Verification: SQLite integrity and invariant checks, sync acknowledgement, read-only Aurora confirmation
- Delivery strategy: ask-on-risk
- Forecast: under 150 authored lines

## Tasks

- [x] **SYNC-001 — Repair and synchronize mutation 7880**
  - Route: delegated
  - Trigger: coordinated process, SQLite, synchronization, and remote verification steps
  - Acceptance: no NUL remains; repaired mutation ID is internally consistent and acknowledged; Aurora contains the repaired mutation once
  - Checks: backup and live database integrity `ok`; guarded transaction committed; repaired mutation acknowledged locally; read-only Aurora query found the repaired mutation exactly once
  - Runtime harness: `engram sync now` exited 0; mutation 7880 received `acked_at=2026-09-18T16:25:16.4813118Z`
  - Rollback boundary: `engram.db.pre-null-repair-20260918T162417Z.bak` restores the complete pre-repair database, but also discards every later local change
  - Commit evidence: this recovery record's `fix(sync): record null-byte mutation repair` work-unit commit

## Known Identity

- Old mutation ID: `878b8e53b4ed5c026eb376544e78a609ce07a8030db276f12d3e457256ba9b13`
- Expected repaired mutation ID: `ca4c663eda1aa72dfb8583827a2c06242b3d1b40df37af441d67adcb288c1c2d`
- Memory: `id=5995`, `sync_id=obs-ee91431fc6dc6066`

## Result

- Repaired mutation ID: `ca4c663eda1aa72dfb8583827a2c06242b3d1b40df37af441d67adcb288c1c2d`
- Payload SHA-256 matches the repaired mutation ID.
- U+0000 count is zero in both the memory and payload; the existing U+0001 is preserved.
- Version 1, local sequence 7880, timestamp, and writer identity are preserved.
- Aurora contains the repaired mutation and materialized memory exactly once; the old mutation remains absent.
- Engram processes and ports 7700/8080 were restored.

## Follow-up

The repaired mutation is no longer blocking. Current status now advances through later outbox entries but reports a separate HTTP 500 at sequence 8635. That mutation is outside this repair and must be diagnosed independently rather than conflated with the confirmed NUL-byte incident.

## Next Step

Diagnose the later sequence 8635 failure as a separate incident if full queue health is required.
