// Package localstore implements the SQLite adapter for the engram local store.
// It uses modernc.org/sqlite (pure Go — no CGO) with WAL mode.
package localstore

import (
	"database/sql"
	"strings"
)

// currentSchemaVersion is the schema version this binary expects.
// Increment this constant and add a migration case in runMigrations whenever
// a non-additive schema change is required (ALTER-incompatible in SQLite).
//
// v0 → v1: rebuild memories table to drop the legacy `parent_sync_id REFERENCES
//
//	memories(sync_id)` FK that was present before PR #6. The FK is enforced
//	immediately by SQLite (not deferred), which would reject out-of-order
//	children during sync pull. Referential integrity is enforced at the
//	application level via defer-and-replay (PR3/PR4).
const currentSchemaVersion = 1

// memoriesTableDDL is the authoritative CREATE TABLE statement for the memories
// table. It is shared between ApplySchema (fresh DB) and migrateV0ToV1 (rebuild)
// so both always produce the same column set and constraints.
//
// parent_sync_id carries NO REFERENCES clause — see currentSchemaVersion comment.
const memoriesTableDDL = `CREATE TABLE IF NOT EXISTS memories (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	sync_id         TEXT    NOT NULL UNIQUE,
	session_id      TEXT    NOT NULL,
	entity_type     TEXT    NOT NULL DEFAULT 'memory'
	                CHECK(entity_type IN ('memory','change','spec','task','standard','plan')),
	type            TEXT    NOT NULL,
	status          TEXT,
	title           TEXT    NOT NULL,
	content         TEXT    NOT NULL DEFAULT '',
	project         TEXT    NOT NULL DEFAULT '',
	scope           TEXT    NOT NULL DEFAULT 'project',
	topic_key       TEXT,
	parent_sync_id  TEXT,
	version         INTEGER NOT NULL DEFAULT 1,
	seq             INTEGER NOT NULL DEFAULT 0,
	writer_id       TEXT    NOT NULL DEFAULT '',
	normalized_hash TEXT,
	embedding       BLOB,
	embedding_model TEXT,
	embedding_created_at TEXT,
	created_at      TEXT    NOT NULL DEFAULT (datetime('now')),
	updated_at      TEXT    NOT NULL DEFAULT (datetime('now')),
	deleted_at      TEXT,
	review_after    TEXT,
	expires_at      TEXT,
	-- Application-level invariants encoded as CHECK constraints.
	-- Only 'memory' rows may omit status.
	CHECK(entity_type = 'memory' OR status IS NOT NULL),
	-- SDD hierarchy: spec/task/plan MUST belong to a parent; memory/change/standard MAY be root.
	-- NOTE: parent_sync_id carries NO REFERENCES clause. A REFERENCES clause with
	-- PRAGMA foreign_keys = ON is enforced immediately (not deferred), which would
	-- reject out-of-order mutations during sync pull (child arrives before parent).
	-- Referential integrity is enforced at the application level via defer-and-replay
	-- (see PR3/PR4). The non-null CHECK below ensures spec/task/plan rows always
	-- declare a parent, but the referenced parent may not yet be present locally.
	CHECK(entity_type IN ('memory','change','standard') OR parent_sync_id IS NOT NULL)
)`

// memoryRelationsTableDDL is the authoritative CREATE TABLE statement for the
// memory_relations table. It is shared between ApplySchema (fresh DB) and
// migrateV0ToV1 (rebuild) so both always produce the same schema.
//
// REFERENCES clauses are intentionally omitted — see memoriesTableDDL comment.
const memoryRelationsTableDDL = `CREATE TABLE IF NOT EXISTS memory_relations (
	from_sync_id TEXT NOT NULL,
	to_sync_id   TEXT NOT NULL,
	rel_type     TEXT NOT NULL DEFAULT 'parent',
	PRIMARY KEY (from_sync_id, to_sync_id, rel_type)
)`

// runMigrations inspects PRAGMA user_version and applies any pending migrations
// in order.  It is idempotent: a DB already at currentSchemaVersion is a no-op.
// Migrations are applied inside individual transactions so a failure leaves the
// DB at the last successfully applied version.
func runMigrations(db *sql.DB) error {
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		return err
	}

	if ver < 1 {
		if err := migrateV0ToV1(db); err != nil {
			return err
		}
		ver = 1
	}

	// Future migrations: if ver < 2 { migrateV1ToV2(db); ver = 2 } etc.
	_ = ver
	return nil
}

// migrateV0ToV1 drops legacy REFERENCES FKs from the memories and
// memory_relations tables via the standard SQLite table-rebuild pattern.
//
// memories: drops `parent_sync_id REFERENCES memories(sync_id)`.
// memory_relations: drops `from_sync_id/to_sync_id REFERENCES memories(sync_id)`.
//
// For each table:
//  1. Disable FK enforcement for the duration of the rebuild.
//  2. In a transaction: rename old table, create corrected table, copy all rows,
//     recreate dependent objects (FTS + indexes for memories), drop old table.
//  3. Re-enable FK enforcement.
//  4. Set PRAGMA user_version = 1.
//
// If a table does NOT contain a REFERENCES clause (already clean or non-existent),
// that table's rebuild is skipped — avoids pointless O(n) copies on fresh DBs.
func migrateV0ToV1(db *sql.DB) error {
	// ── memories ─────────────────────────────────────────────────────────────
	var memoriesSQLNullable sql.NullString
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='memories'`,
	).Scan(&memoriesSQLNullable); err != nil {
		return err
	}

	if memoriesSQLNullable.Valid && strings.Contains(
		strings.ToUpper(memoriesSQLNullable.String), "REFERENCES MEMORIES",
	) {
		if err := rebuildMemoriesTable(db); err != nil {
			return err
		}
	}
	// If memories doesn't exist yet (fresh DB mid-Open), ApplySchema will create
	// it correctly — nothing to rebuild.

	// ── memory_relations ─────────────────────────────────────────────────────
	var relSQLNullable sql.NullString
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='memory_relations'`,
	).Scan(&relSQLNullable); err != nil {
		return err
	}

	// Detect legacy REFERENCES:
	// - Original v0 DDL: `REFERENCES memories(sync_id)` → contains "REFERENCES MEMORIES"
	// - After rebuildMemoriesTable renames memories→memories_old, SQLite rewrites
	//   FKs in memory_relations to `REFERENCES "memories_old"(sync_id)` → contains
	//   "REFERENCES" (the quoted identifier survives ToUpper as "MEMORIES_OLD").
	//   We check for "REFERENCES" plus any suffix that contains "MEMORIES" to cover
	//   both `REFERENCES memories` and `REFERENCES "memories_old"`.
	if relSQLNullable.Valid && strings.Contains(
		strings.ToUpper(relSQLNullable.String), "REFERENCES",
	) && strings.Contains(
		strings.ToUpper(relSQLNullable.String), "MEMORIES",
	) {
		if err := rebuildMemoryRelationsTable(db); err != nil {
			return err
		}
	}

	// Bump user_version regardless of whether any rebuild was needed so that
	// runMigrations does not re-enter v0→v1 on the next Open.
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		return err
	}
	return nil
}

// rebuildMemoriesTable performs the SQLite table-rebuild to drop the legacy FK:
//
//  1. `PRAGMA foreign_keys = OFF` — required during the rename/create/copy cycle.
//  2. In a transaction:
//     a. Rename memories → memories_old.
//     b. Re-create memories using memoriesTableDDL (no FK).
//     c. Copy all rows from memories_old.
//     d. Drop FTS virtual table + triggers (they reference the old rowid mapping).
//     e. Recreate FTS virtual table + triggers via the shared DDL statements.
//     f. Rebuild FTS index from the copied rows.
//     g. DROP INDEX IF EXISTS for all four idx_mem_* names — necessary because
//        ALTER TABLE RENAME preserves index names on memories_old, so
//        CREATE INDEX IF NOT EXISTS would silently no-op (name already exists).
//        Dropping the names first lets the CREATE INDEX run against the new table.
//     h. Recreate indexes on the new memories table.
//     i. Drop memories_old (this also drops any indexes that survived on it).
//  3. `PRAGMA foreign_keys = ON`.
//
// All steps inside one transaction: if any step fails, the rename is rolled back
// and the DB remains on the old schema (user_version stays 0).
func rebuildMemoriesTable(db *sql.DB) error {
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer db.Exec(`PRAGMA foreign_keys = ON`) //nolint:errcheck

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback() //nolint:errcheck
		}
	}()

	// a. Rename old table.
	if _, err = tx.Exec(`ALTER TABLE memories RENAME TO memories_old`); err != nil {
		return err
	}

	// b. Create corrected table (no FK).
	// Strip IF NOT EXISTS so we get a clean error if something is wrong.
	newDDL := strings.Replace(memoriesTableDDL, "IF NOT EXISTS ", "", 1)
	if _, err = tx.Exec(newDDL); err != nil {
		return err
	}

	// c. Copy all rows (column list explicit — same set in old and new table).
	if _, err = tx.Exec(`INSERT INTO memories SELECT
		id, sync_id, session_id, entity_type, type, status, title, content,
		project, scope, topic_key, parent_sync_id, version, seq, writer_id,
		normalized_hash, embedding, embedding_model, embedding_created_at,
		created_at, updated_at, deleted_at, review_after, expires_at
	FROM memories_old`); err != nil {
		return err
	}

	// d. Drop FTS virtual table and its triggers (they depend on memories rowids).
	for _, drop := range []string{
		`DROP TRIGGER IF EXISTS mem_fts_insert`,
		`DROP TRIGGER IF EXISTS mem_fts_delete`,
		`DROP TRIGGER IF EXISTS mem_fts_update`,
		`DROP TABLE  IF EXISTS memories_fts`,
	} {
		if _, err = tx.Exec(drop); err != nil {
			return err
		}
	}

	// e. Recreate FTS virtual table and triggers.
	ftsStmts := []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
			title,
			content,
			type,
			entity_type,
			status,
			project,
			topic_key,
			content='memories',
			content_rowid='id'
		)`,
		`CREATE TRIGGER IF NOT EXISTS mem_fts_insert
			AFTER INSERT ON memories
			WHEN NEW.deleted_at IS NULL
		BEGIN
			INSERT INTO memories_fts(rowid, title, content, type, entity_type, status, project, topic_key)
			VALUES (
				NEW.id,
				NEW.title,
				NEW.content,
				NEW.type,
				NEW.entity_type,
				COALESCE(NEW.status, ''),
				NEW.project,
				COALESCE(NEW.topic_key, '')
			);
		END`,
		`CREATE TRIGGER IF NOT EXISTS mem_fts_delete
			AFTER DELETE ON memories
		BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, title, content, type, entity_type, status, project, topic_key)
			VALUES (
				'delete',
				OLD.id,
				OLD.title,
				OLD.content,
				OLD.type,
				OLD.entity_type,
				COALESCE(OLD.status, ''),
				OLD.project,
				COALESCE(OLD.topic_key, '')
			);
		END`,
		`CREATE TRIGGER IF NOT EXISTS mem_fts_update
			AFTER UPDATE ON memories
		BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, title, content, type, entity_type, status, project, topic_key)
			VALUES (
				'delete',
				OLD.id,
				OLD.title,
				OLD.content,
				OLD.type,
				OLD.entity_type,
				COALESCE(OLD.status, ''),
				OLD.project,
				COALESCE(OLD.topic_key, '')
			);
			INSERT INTO memories_fts(rowid, title, content, type, entity_type, status, project, topic_key)
			SELECT
				NEW.id,
				NEW.title,
				NEW.content,
				NEW.type,
				NEW.entity_type,
				COALESCE(NEW.status, ''),
				NEW.project,
				COALESCE(NEW.topic_key, '')
			WHERE NEW.deleted_at IS NULL;
		END`,
	}
	for _, s := range ftsStmts {
		if _, err = tx.Exec(s); err != nil {
			return err
		}
	}

	// f. Rebuild FTS index from the now-populated memories table.
	if _, err = tx.Exec(`INSERT INTO memories_fts(memories_fts) VALUES('rebuild')`); err != nil {
		return err
	}

	// g. Drop old index names before recreating.
	//
	// After ALTER TABLE RENAME, SQLite preserves each existing index name and
	// keeps it pointing at memories_old.  The subsequent CREATE INDEX IF NOT
	// EXISTS statements would silently no-op because the names already exist —
	// leaving the new memories table with zero indexes.  DROP INDEX IF EXISTS
	// frees the names so the CREATE INDEX statements run against the new table.
	//
	// Note: memories_old still exists at this point; its rowid mapping is about
	// to be destroyed by DROP TABLE memories_old in step (i).  Dropping the
	// index names here is safe because we no longer need them on memories_old.
	dropIdxStmts := []string{
		`DROP INDEX IF EXISTS idx_mem_topic`,
		`DROP INDEX IF EXISTS idx_mem_parent`,
		`DROP INDEX IF EXISTS idx_mem_entity_status`,
		`DROP INDEX IF EXISTS idx_mem_deleted`,
	}
	for _, s := range dropIdxStmts {
		if _, err = tx.Exec(s); err != nil {
			return err
		}
	}

	// h. Recreate indexes on the new memories table.
	idxStmts := []string{
		`CREATE INDEX IF NOT EXISTS idx_mem_topic
			ON memories(topic_key, project, scope, updated_at DESC)
			WHERE topic_key IS NOT NULL AND deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_mem_parent
			ON memories(parent_sync_id)
			WHERE parent_sync_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_mem_entity_status
			ON memories(entity_type, status)`,
		`CREATE INDEX IF NOT EXISTS idx_mem_deleted
			ON memories(deleted_at)
			WHERE deleted_at IS NOT NULL`,
	}
	for _, s := range idxStmts {
		if _, err = tx.Exec(s); err != nil {
			return err
		}
	}

	// i. Drop the old table (and any residual indexes on it).
	if _, err = tx.Exec(`DROP TABLE memories_old`); err != nil {
		return err
	}

	return tx.Commit()
}

// rebuildMemoryRelationsTable performs the SQLite table-rebuild to drop the
// legacy REFERENCES FKs from memory_relations:
//
//  1. `PRAGMA foreign_keys = OFF` — required during the rename/create/copy cycle.
//  2. In a transaction:
//     a. Rename memory_relations → memory_relations_old.
//     b. Re-create memory_relations using memoryRelationsTableDDL (no FKs).
//     c. Copy all rows from memory_relations_old.
//     d. Drop memory_relations_old.
//  3. `PRAGMA foreign_keys = ON`.
//
// All steps inside one transaction.
func rebuildMemoryRelationsTable(db *sql.DB) error {
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer db.Exec(`PRAGMA foreign_keys = ON`) //nolint:errcheck

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback() //nolint:errcheck
		}
	}()

	// a. Rename old table.
	if _, err = tx.Exec(`ALTER TABLE memory_relations RENAME TO memory_relations_old`); err != nil {
		return err
	}

	// b. Create corrected table (no FKs).
	// Strip IF NOT EXISTS so we get a clean error if something is wrong.
	newDDL := strings.Replace(memoryRelationsTableDDL, "IF NOT EXISTS ", "", 1)
	if _, err = tx.Exec(newDDL); err != nil {
		return err
	}

	// c. Copy all rows.
	if _, err = tx.Exec(`INSERT INTO memory_relations SELECT from_sync_id, to_sync_id, rel_type FROM memory_relations_old`); err != nil {
		return err
	}

	// d. Drop old table.
	if _, err = tx.Exec(`DROP TABLE memory_relations_old`); err != nil {
		return err
	}

	return tx.Commit()
}

// ApplySchema creates all tables, indexes, FTS5 virtual table, and triggers
// in db. All statements use IF NOT EXISTS / CREATE INDEX IF NOT EXISTS so
// the function is fully idempotent and safe to call on every Open.
func ApplySchema(db *sql.DB) error {
	stmts := []string{
		// ── Core memories table ──────────────────────────────────────────────
		memoriesTableDDL,

		// ── memory_tombstones — prevent soft-delete resurrection (INV 4) ────
		`CREATE TABLE IF NOT EXISTS memory_tombstones (
			sync_id    TEXT    PRIMARY KEY,
			project    TEXT    NOT NULL DEFAULT '',
			scope      TEXT    NOT NULL DEFAULT 'project',
			topic_key  TEXT,
			deleted_at TEXT    NOT NULL,
			deleted_by TEXT    NOT NULL,
			version    INTEGER NOT NULL DEFAULT 0
		)`,

		// ── memory_relations — reserved for future M:N promotion ─────────────
		// REFERENCES clauses intentionally omitted — see memoriesTableDDL comment.
		memoryRelationsTableDDL,

		// ── sync_mutations — outbound push journal ───────────────────────────
		`CREATE TABLE IF NOT EXISTS sync_mutations (
			local_seq    INTEGER PRIMARY KEY AUTOINCREMENT,
			mutation_id  TEXT    NOT NULL UNIQUE,
			entity       TEXT    NOT NULL DEFAULT '',
			entity_key   TEXT    NOT NULL DEFAULT '',
			op           TEXT    NOT NULL CHECK(op IN ('upsert','delete')),
			payload      TEXT    NOT NULL DEFAULT '',
			writer_id    TEXT    NOT NULL DEFAULT '',
			occurred_at  TEXT    NOT NULL DEFAULT (datetime('now')),
			acked_at     TEXT
		)`,

		// ── sync_state — tracks last push-ack and last pull seq ──────────────
		`CREATE TABLE IF NOT EXISTS sync_state (
			target_key       TEXT PRIMARY KEY DEFAULT 'central',
			last_acked_seq   INTEGER NOT NULL DEFAULT 0,
			last_pulled_seq  INTEGER NOT NULL DEFAULT 0
		)`,

		// ── applied_mutations — idempotency guard (INV 5) ────────────────────
		`CREATE TABLE IF NOT EXISTS applied_mutations (
			mutation_id TEXT    PRIMARY KEY,
			applied_at  TEXT    NOT NULL DEFAULT (datetime('now'))
		)`,

		// ── Indexes ──────────────────────────────────────────────────────────
		`CREATE INDEX IF NOT EXISTS idx_mem_topic
			ON memories(topic_key, project, scope, updated_at DESC)
			WHERE topic_key IS NOT NULL AND deleted_at IS NULL`,

		`CREATE INDEX IF NOT EXISTS idx_mem_parent
			ON memories(parent_sync_id)
			WHERE parent_sync_id IS NOT NULL`,

		`CREATE INDEX IF NOT EXISTS idx_mem_entity_status
			ON memories(entity_type, status)`,

		`CREATE INDEX IF NOT EXISTS idx_mem_deleted
			ON memories(deleted_at)
			WHERE deleted_at IS NOT NULL`,

		// ── FTS5 virtual table over memories ────────────────────────────────
		// content=memories with content_rowid=id means FTS is a shadow/external
		// index: we manage it manually via triggers.
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
			title,
			content,
			type,
			entity_type,
			status,
			project,
			topic_key,
			content='memories',
			content_rowid='id'
		)`,

		// ── FTS maintenance triggers ─────────────────────────────────────────
		// INSERT: add new row to FTS index.
		`CREATE TRIGGER IF NOT EXISTS mem_fts_insert
			AFTER INSERT ON memories
			WHEN NEW.deleted_at IS NULL
		BEGIN
			INSERT INTO memories_fts(rowid, title, content, type, entity_type, status, project, topic_key)
			VALUES (
				NEW.id,
				NEW.title,
				NEW.content,
				NEW.type,
				NEW.entity_type,
				COALESCE(NEW.status, ''),
				NEW.project,
				COALESCE(NEW.topic_key, '')
			);
		END`,

		// DELETE: remove row from FTS index using the FTS 'delete' command.
		`CREATE TRIGGER IF NOT EXISTS mem_fts_delete
			AFTER DELETE ON memories
		BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, title, content, type, entity_type, status, project, topic_key)
			VALUES (
				'delete',
				OLD.id,
				OLD.title,
				OLD.content,
				OLD.type,
				OLD.entity_type,
				COALESCE(OLD.status, ''),
				OLD.project,
				COALESCE(OLD.topic_key, '')
			);
		END`,

		// UPDATE: remove old index entry, add new one. Also handles soft-delete:
		// if deleted_at becomes non-NULL, remove from FTS without re-inserting.
		`CREATE TRIGGER IF NOT EXISTS mem_fts_update
			AFTER UPDATE ON memories
		BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, title, content, type, entity_type, status, project, topic_key)
			VALUES (
				'delete',
				OLD.id,
				OLD.title,
				OLD.content,
				OLD.type,
				OLD.entity_type,
				COALESCE(OLD.status, ''),
				OLD.project,
				COALESCE(OLD.topic_key, '')
			);
			INSERT INTO memories_fts(rowid, title, content, type, entity_type, status, project, topic_key)
			SELECT
				NEW.id,
				NEW.title,
				NEW.content,
				NEW.type,
				NEW.entity_type,
				COALESCE(NEW.status, ''),
				NEW.project,
				COALESCE(NEW.topic_key, '')
			WHERE NEW.deleted_at IS NULL;
		END`,

		// ── Seed the default sync_state row ──────────────────────────────────
		`INSERT OR IGNORE INTO sync_state(target_key) VALUES ('central')`,
	}

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	return nil
}
