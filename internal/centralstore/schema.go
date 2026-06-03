// Package centralstore implements the Postgres adapter for the engram central
// store. It is the seq authority: all BIGSERIAL seq values originate here.
// No CGO — uses pgx/v5 (pure-Go Postgres driver).
package centralstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ApplySchema creates all central tables and indexes idempotently.
// It is safe to call on every connect — all DDL uses IF NOT EXISTS /
// CREATE INDEX IF NOT EXISTS so repeat calls are cheap no-ops.
//
// Schema overview:
//
//   - central_mutations: authoritative journal; assigns monotonic BIGSERIAL seq
//     (used for pull cursors and journal ordering only, NOT for LWW tiebreaking).
//   - central_memories:  canonical materialized state; partial UNIQUE on topic
//     identity enforces INV-A at the DB level. Does NOT carry a seq column —
//     the materialized-row seq was removed (dead field; pull cursor uses
//     sync_state.last_pulled_seq / Mutation.Seq from central_mutations directly).
//   - central_tombstones: records every soft-delete; partial UNIQUE on topic
//     prevents duplicate tombstones (INV-B). deleted_by + last_write_mutation_id
//     are the final tiebreaker fields used by writeWins (writer_id → winning
//     mutation_id, replica-identical — NOT the canonical PK sync_id).
//   - cloud_sync_audit:  push/pull audit trail (written by PR3b onwards).
func ApplySchema(ctx context.Context, pool *pgxpool.Pool) error {
	stmts := []string{
		// ── central_mutations ────────────────────────────────────────────────────
		// seq is BIGSERIAL PRIMARY KEY — the authoritative monotonic counter.
		// mutation_id UNIQUE prevents duplicate pushes (INV5 defense-in-depth).
		`CREATE TABLE IF NOT EXISTS central_mutations (
			seq         BIGSERIAL    PRIMARY KEY,
			mutation_id TEXT         NOT NULL UNIQUE,
			entity      TEXT         NOT NULL DEFAULT '',
			entity_key  TEXT         NOT NULL DEFAULT '',
			op          TEXT         NOT NULL CHECK(op IN ('upsert','delete')),
			payload     JSONB        NOT NULL DEFAULT '{}',
			writer_id   TEXT         NOT NULL DEFAULT '',
			project     TEXT         NOT NULL DEFAULT '',
			occurred_at TIMESTAMPTZ  NOT NULL DEFAULT now()
		)`,

		`CREATE INDEX IF NOT EXISTS idx_cmut_project_seq
			ON central_mutations(project, seq)`,

		// ── central_memories ─────────────────────────────────────────────────────
		// Canonical materialized read model. sync_id is the portable identity.
		// embedding is BYTEA reserved for pgvector (not populated in this change).
		// Note: seq was removed in the v4→v5 close-out (the materialized-row copy
		// was dead; the pull cursor uses sync_state.last_pulled_seq / Mutation.Seq
		// from central_mutations). The LWW tiebreaker uses last_write_mutation_id.
		`CREATE TABLE IF NOT EXISTS central_memories (
			sync_id        TEXT         PRIMARY KEY,
			session_id     TEXT         NOT NULL DEFAULT '',
			entity_type    TEXT         NOT NULL DEFAULT 'memory'
			               CHECK(entity_type IN ('memory','change','spec','task','standard','plan')),
			type           TEXT         NOT NULL DEFAULT '',
			status         TEXT,
			title          TEXT         NOT NULL DEFAULT '',
			content        TEXT         NOT NULL DEFAULT '',
			project        TEXT         NOT NULL DEFAULT '',
			scope          TEXT         NOT NULL DEFAULT 'project',
			topic_key      TEXT,
			parent_sync_id TEXT,
			version        INT          NOT NULL DEFAULT 1,
			writer_id      TEXT         NOT NULL DEFAULT '',
			last_write_mutation_id TEXT NOT NULL DEFAULT '',
			created_by     TEXT         NOT NULL DEFAULT '',
			embedding      BYTEA,
			created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
			updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
			deleted_at     TIMESTAMPTZ,
			-- Application-level invariants encoded as CHECK constraints (parity with local memories table).
			-- Only 'memory' rows may omit status.
			CHECK(entity_type = 'memory' OR status IS NOT NULL),
			-- SDD hierarchy: spec/task/plan MUST belong to a parent; memory/change/standard MAY be root.
			CHECK(entity_type IN ('memory','change','standard') OR parent_sync_id IS NOT NULL)
		)`,

		// CRITICAL: partial UNIQUE on (topic_key, project, scope) WHERE deleted_at IS NULL
		// — the central store is the AUTHORITY; it enforces INV-A (at most one LIVE row per
		// topic identity) at the DB level.  No two live rows may share the same topic identity.
		// This index is the convergence enforcer — it makes topic_key identity fork impossible.
		`CREATE UNIQUE INDEX IF NOT EXISTS central_memories_topic_uidx
			ON central_memories(topic_key, project, scope)
			WHERE topic_key IS NOT NULL AND deleted_at IS NULL`,

		`CREATE INDEX IF NOT EXISTS idx_cmem_project_updated
			ON central_memories(project, updated_at DESC)`,

		`CREATE INDEX IF NOT EXISTS idx_cmem_deleted
			ON central_memories(deleted_at)
			WHERE deleted_at IS NOT NULL`,

		// ── Existing-DB upgrade: drop dead materialized-row seq ──────────────────
		// These are idempotent on a fresh DB (no seq column, no idx_cmem_seq index
		// to drop). On an existing DB they remove the dead column and its index.
		// DROP INDEX IF EXISTS and DROP COLUMN IF EXISTS never error when absent.
		`DROP INDEX IF EXISTS idx_cmem_seq`,
		`ALTER TABLE central_memories DROP COLUMN IF EXISTS seq`,

		// ── central_tombstones ───────────────────────────────────────────────────
		// One row per soft-deleted identity.  sync_id PK prevents duplicate rows
		// for the same sync_id.  The partial UNIQUE on topic identity (INV-B)
		// ensures FindTombstone-by-topic is deterministic (≤1 result).
		// deleted_by (writer_id) is a tiebreaker used by writeWins when updated_at
		// and version tie; last_write_mutation_id (the winning delete's content-
		// addressed id) is the FINAL tiebreaker when deleted_by also ties. Unlike
		// sync_id (the canonical PK, divergent across replicas for the same topic),
		// deleted_by and last_write_mutation_id are replica-identical (see writeWins
		// doc comment in domain/reconcile.go).
		`CREATE TABLE IF NOT EXISTS central_tombstones (
			sync_id    TEXT         PRIMARY KEY,
			project    TEXT         NOT NULL DEFAULT '',
			scope      TEXT         NOT NULL DEFAULT 'project',
			topic_key  TEXT,
			deleted_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
			deleted_by TEXT         NOT NULL DEFAULT '',
			version    INT          NOT NULL DEFAULT 0,
			last_write_mutation_id TEXT NOT NULL DEFAULT ''
		)`,

		// Partial UNIQUE: ≤1 tombstone per live topic identity (INV-B).
		// Allows topic_key IS NULL rows (non-topic records) to share NULL freely.
		`CREATE UNIQUE INDEX IF NOT EXISTS central_tombstones_topic_uidx
			ON central_tombstones(topic_key, project, scope)
			WHERE topic_key IS NOT NULL`,

		// ── Existing-DB upgrade: last_write_mutation_id ──────────────────────────
		// ADD COLUMN IF NOT EXISTS makes this idempotent on both fresh DBs (the
		// CREATE TABLE above already declared the column) and pre-existing central
		// DBs created before this column existed. It carries the WINNING write's
		// content-addressed mutation_id and is the FINAL LWW tiebreaker (replacing
		// the canonical PK sync_id, which is divergent across replicas for the same
		// topic_key). Existing rows default to '' (yield to any incoming write at
		// the exact tie). This mirrors the local store's migrateV2ToV3.
		`ALTER TABLE central_memories
			ADD COLUMN IF NOT EXISTS last_write_mutation_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE central_tombstones
			ADD COLUMN IF NOT EXISTS last_write_mutation_id TEXT NOT NULL DEFAULT ''`,

		// ── cloud_sync_audit ─────────────────────────────────────────────────────
		// Push/pull audit trail.  Populated by PR3b push-apply; created here so the
		// table exists and schema is idempotent.
		`CREATE TABLE IF NOT EXISTS cloud_sync_audit (
			id          BIGSERIAL    PRIMARY KEY,
			writer_id   TEXT         NOT NULL DEFAULT '',
			project     TEXT         NOT NULL DEFAULT '',
			action      TEXT         NOT NULL DEFAULT '',
			outcome     TEXT         NOT NULL DEFAULT '',
			reason_code TEXT         NOT NULL DEFAULT '',
			metadata    JSONB        NOT NULL DEFAULT '{}',
			created_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
		)`,

		`CREATE INDEX IF NOT EXISTS idx_audit_project_created
			ON cloud_sync_audit(project, created_at DESC)`,
	}

	for i, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("ApplySchema: stmt %d: %w", i, err)
		}
	}
	return nil
}
