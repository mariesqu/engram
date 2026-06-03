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
//
// v1 → v2: drop and recreate FTS maintenance triggers from the shared constants
//
//	below. Existing DBs at user_version=1 may have the OLD mem_fts_update
//	trigger (VALUES form without the WHERE OLD.deleted_at IS NULL guard) that
//	causes SQLITE_CORRUPT_VTAB (267) on any undelete sequence. The migration
//	is a cheap drop+recreate of all three FTS triggers; no data is touched.
//
// v2 → v3: add the last_write_mutation_id column to BOTH memories and
//
//	memory_tombstones. It carries the WINNING write's content-addressed
//	mutation_id and becomes the FINAL LWW tiebreaker (replacing the canonical
//	PK sync_id, which is divergent across replicas for the same topic). The
//	migration ALTER TABLE ADD COLUMNs on both tables, guarded by PRAGMA
//	table_info so a fresh DB (where ApplySchema already created the column) is
//	a no-op. Existing rows default to '' (yields to any incoming write at the
//	astronomically-rare exact tie — see writeWins doc comment).
//
// v3 → v4: add memory_tombstones_topic_uidx — a partial UNIQUE index on
//   memory_tombstones(topic_key, project, scope)
//   WHERE topic_key IS NOT NULL AND topic_key <> ''
// schema-enforcing the ≤1-tombstone-per-topic invariant (INV-B) that was
// previously only logic-guaranteed by Decide's canonical re-targeting. Mirrors
// central_tombstones_topic_uidx in the central store. CREATE UNIQUE INDEX IF NOT
// EXISTS is idempotent so fresh DBs (where ApplySchema already created the index)
// are a no-op.
const currentSchemaVersion = 4

// ── Shared FTS DDL constants (single source of truth) ───────────────────────
//
// These constants are used by BOTH ApplySchema (fresh DB) and rebuildMemoriesTable
// (v0→v1 table rebuild) AND migrateV1ToV2 (v1→v2 trigger fix). Having ONE source
// of truth prevents the kind of drift that caused the original CORRUPT_VTAB bug:
// ApplySchema had the fixed trigger while rebuildMemoriesTable still had the old one.

// ftsVirtualTableDDL is the CREATE VIRTUAL TABLE statement for the FTS5 index.
const ftsVirtualTableDDL = `CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
	title,
	content,
	type,
	entity_type,
	status,
	project,
	topic_key,
	content='memories',
	content_rowid='id'
)`

// ftsTriggerInsert is the AFTER INSERT trigger that adds live rows to the FTS index.
// Rows inserted with deleted_at already set are intentionally skipped (the WHEN guard)
// because they are not "visible" — adding them would cause a phantom match in searches.
const ftsTriggerInsert = `CREATE TRIGGER IF NOT EXISTS mem_fts_insert
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
END`

// ftsTriggerDelete is the AFTER DELETE trigger that removes rows from the FTS index.
const ftsTriggerDelete = `CREATE TRIGGER IF NOT EXISTS mem_fts_delete
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
END`

// ftsTriggerUpdate is the AFTER UPDATE trigger that keeps the FTS index in sync.
//
// The FTS 'delete' command is only issued when OLD.deleted_at IS NULL.
// Rows inserted with deleted_at set are NOT in the FTS index (the INSERT trigger
// skips them via WHEN NEW.deleted_at IS NULL). Trying to 'delete' a non-indexed
// rowid from an FTS5 external-content table causes SQLITE_CORRUPT_VTAB (267).
// The conditional SELECT ... WHERE OLD.deleted_at IS NULL guards against that.
const ftsTriggerUpdate = `CREATE TRIGGER IF NOT EXISTS mem_fts_update
	AFTER UPDATE ON memories
BEGIN
	INSERT INTO memories_fts(memories_fts, rowid, title, content, type, entity_type, status, project, topic_key)
	SELECT
		'delete',
		OLD.id,
		OLD.title,
		OLD.content,
		OLD.type,
		OLD.entity_type,
		COALESCE(OLD.status, ''),
		OLD.project,
		COALESCE(OLD.topic_key, '')
	WHERE OLD.deleted_at IS NULL;
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
END`

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
	last_write_mutation_id TEXT NOT NULL DEFAULT '',
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

	if ver < 2 {
		if err := migrateV1ToV2(db); err != nil {
			return err
		}
		// Keep ver in sync (migrateV1ToV2 also persists PRAGMA user_version = 2) so a
		// future `if ver < 3` block evaluates against the correct version.
		ver = 2
	}

	if ver < 3 {
		if err := migrateV2ToV3(db); err != nil {
			return err
		}
		// Keep ver in sync (migrateV2ToV3 also persists PRAGMA user_version = 3) so a
		// future `if ver < 4` block evaluates against the correct version.
		ver = 3
	}

	if ver < 4 {
		if err := migrateV3ToV4(db); err != nil {
			return err
		}
		// Keep ver in sync so future migration cases evaluate the correct version.
		ver = 4
	}

	// ver is read by the `if ver < N` conditions above. This blank read consumes the
	// final `ver = 4` assignment so it is not flagged as ineffectual (SA4006); the
	// value stays in sync for any future `if ver < 5` migration block.
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

// migrateV1ToV2 replaces the three FTS maintenance triggers with the versions
// from the shared package-level constants. This is necessary because:
//
//   - ApplySchema uses CREATE TRIGGER IF NOT EXISTS — a no-op when the trigger
//     already exists (even with a different body).
//   - A DB at user_version=1 may still have the OLD mem_fts_update trigger (the
//     VALUES form without WHERE OLD.deleted_at IS NULL) that causes
//     SQLITE_CORRUPT_VTAB (267) on any undelete sequence.
//
// The fix is a cheap drop+recreate of all three triggers from the shared
// constants (ftsTriggerInsert, ftsTriggerDelete, ftsTriggerUpdate). No table
// data is touched; the operation is idempotent and safe to re-run.
func migrateV1ToV2(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback() //nolint:errcheck
		}
	}()

	// Drop all three triggers unconditionally so CREATE TRIGGER runs even when
	// the old version already exists under the same name.
	for _, drop := range []string{
		`DROP TRIGGER IF EXISTS mem_fts_insert`,
		`DROP TRIGGER IF EXISTS mem_fts_delete`,
		`DROP TRIGGER IF EXISTS mem_fts_update`,
	} {
		if _, err = tx.Exec(drop); err != nil {
			return err
		}
	}

	// Recreate from the shared authoritative constants.
	for _, stmt := range []string{ftsTriggerInsert, ftsTriggerDelete, ftsTriggerUpdate} {
		if _, err = tx.Exec(stmt); err != nil {
			return err
		}
	}

	if _, err = tx.Exec(`PRAGMA user_version = 2`); err != nil {
		return err
	}
	err = tx.Commit()
	return err
}

// columnExists reports whether table already has a column named col, via
// PRAGMA table_info. Used by migrateV2ToV3 to make the ADD COLUMN idempotent:
// a fresh DB where ApplySchema already created the column must NOT be re-altered
// (SQLite ADD COLUMN of a duplicate name errors).
func columnExists(q rowScanner, table, col string) (bool, error) {
	rows, err := q.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

// rowScanner is the minimal query surface columnExists needs (satisfied by both
// *sql.DB and *sql.Tx).
type rowScanner interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// migrateV2ToV3 adds the last_write_mutation_id column to BOTH memories and
// memory_tombstones. The column carries the WINNING write's content-addressed
// mutation_id and is the FINAL LWW tiebreaker (replacing the canonical PK
// sync_id, which is divergent across replicas for the same topic_key).
//
// Each ADD COLUMN is guarded by PRAGMA table_info: on a FRESH DB, ApplySchema
// already created the column with the new DDL, so the migration must skip it
// (SQLite errors on a duplicate ADD COLUMN). On an EXISTING DB at user_version=2
// the column is absent and gets added with DEFAULT '' so existing rows are
// backfilled to empty (which yields to any incoming write at the exact tie).
//
// All steps run in ONE transaction with the robust UNCONDITIONAL
// defer tx.Rollback() + return tx.Commit() pattern: Commit succeeds → the
// deferred Rollback is a harmless no-op; any error → the deferred Rollback
// reverts everything and user_version stays at 2.
func migrateV2ToV3(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	for _, table := range []string{"memories", "memory_tombstones"} {
		exists, err := columnExists(tx, table, "last_write_mutation_id")
		if err != nil {
			return err
		}
		if exists {
			continue // fresh DB — ApplySchema already added the column
		}
		if _, err := tx.Exec(
			`ALTER TABLE ` + table + ` ADD COLUMN last_write_mutation_id TEXT NOT NULL DEFAULT ''`,
		); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`PRAGMA user_version = 3`); err != nil {
		return err
	}
	return tx.Commit()
}

// migrateV3ToV4 adds the memory_tombstones_topic_uidx partial UNIQUE index on
// memory_tombstones(topic_key, project, scope) WHERE topic_key IS NOT NULL AND
// topic_key <> '' (the domain treats both NULL and '' as "no topic").
//
// This is the defense-in-depth close-out for INV-B (≤1 tombstone per topic
// identity). Previously the invariant was only logic-guaranteed by Decide's
// canonical re-targeting; this migration makes it SCHEMA-ENFORCED, mirroring
// central_tombstones_topic_uidx in the central store.
//
// CREATE UNIQUE INDEX IF NOT EXISTS is idempotent: a fresh DB created by
// ApplySchema already has the index, so this migration is a no-op there.
//
// IMPORTANT — pre-existing topic duplicates: if a v3 DB somehow accumulated two
// tombstones for the same (topic_key, project, scope) (possible only through
// direct DB writes or a bug that bypassed Decide), CREATE UNIQUE INDEX will fail
// with SQLITE_CONSTRAINT. This is intentional — such a state violates INV-B and
// must be repaired before the migration can succeed. In practice Decide never
// produces duplicates, so legitimate DBs will always migrate cleanly.
//
// All work runs inside ONE transaction with the unconditional defer tx.Rollback()
// + return tx.Commit() pattern: Commit succeeds → deferred Rollback is a no-op;
// any error → deferred Rollback reverts everything and user_version stays at 3.
func migrateV3ToV4(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS memory_tombstones_topic_uidx
		ON memory_tombstones(topic_key, project, scope)
		WHERE topic_key IS NOT NULL AND topic_key <> ''`); err != nil {
		return err
	}

	if _, err := tx.Exec(`PRAGMA user_version = 4`); err != nil {
		return err
	}
	return tx.Commit()
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

	// c. Copy all rows. BOTH the target and source column lists are explicit and
	// enumerate the LEGACY v0 column set — last_write_mutation_id is intentionally
	// OMITTED (it does not exist on a v0 DB; the new table's DEFAULT '' backfills
	// it). Listing the target columns is REQUIRED: the new memories table has the
	// extra last_write_mutation_id column, so a bare `INSERT INTO memories SELECT …`
	// would mismatch the column count. The later migrateV2ToV3 is a no-op for this
	// fresh-rebuilt table because ApplySchema already created the column.
	if _, err = tx.Exec(`INSERT INTO memories
		(id, sync_id, session_id, entity_type, type, status, title, content,
		 project, scope, topic_key, parent_sync_id, version, seq, writer_id,
		 normalized_hash, embedding, embedding_model, embedding_created_at,
		 created_at, updated_at, deleted_at, review_after, expires_at)
	SELECT
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

	// e. Recreate FTS virtual table and triggers from the shared authoritative
	//    constants. Using constants here (not inline literals) ensures this path
	//    and ApplySchema always produce the same trigger bodies — preventing the
	//    drift that originally caused SQLITE_CORRUPT_VTAB (267).
	ftsStmts := []string{
		ftsVirtualTableDDL,
		ftsTriggerInsert,
		ftsTriggerDelete,
		ftsTriggerUpdate,
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

	err = tx.Commit()
	return err
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

	err = tx.Commit()
	return err
}

// ApplySchema creates all tables, indexes, FTS5 virtual table, and triggers
// in db. All statements use IF NOT EXISTS / CREATE INDEX IF NOT EXISTS so
// the function is fully idempotent and safe to call on every Open.
func ApplySchema(db *sql.DB) error {
	stmts := []string{
		// ── Core memories table ──────────────────────────────────────────────
		memoriesTableDDL,

		// ── memory_tombstones — prevent soft-delete resurrection (INV 4) ────
		// deleted_by (writer_id) is a tiebreaker used by writeWins when updated_at
		// and version tie; last_write_mutation_id (the winning delete's content-
		// addressed id) is the FINAL tiebreaker when deleted_by also ties. Unlike
		// sync_id (the canonical PK, divergent across replicas), deleted_by and
		// last_write_mutation_id are replica-identical — see writeWins doc comment.
		`CREATE TABLE IF NOT EXISTS memory_tombstones (
			sync_id    TEXT    PRIMARY KEY,
			project    TEXT    NOT NULL DEFAULT '',
			scope      TEXT    NOT NULL DEFAULT 'project',
			topic_key  TEXT,
			deleted_at TEXT    NOT NULL,
			deleted_by TEXT    NOT NULL,
			version    INTEGER NOT NULL DEFAULT 0,
			last_write_mutation_id TEXT NOT NULL DEFAULT ''
		)`,

		// ── memory_tombstones_topic_uidx — INV-B schema enforcement ─────────────
		// Partial UNIQUE: ≤1 tombstone per live topic identity (INV-B, defense-in-depth).
		// Mirrors central_tombstones_topic_uidx in the central store.
		// The predicate excludes BOTH NULL and '' topic_key: the domain treats both as
		// "no topic" (Decide/FindByTopic use TopicKey != nil && *TopicKey != ""), so
		// genuine no-topic records must NOT be constrained. Without the '' exclusion,
		// multiple no-topic deletes that round-trip as topic_key='' in the same
		// (project, scope) would collide and raise spurious UNIQUE violations.
		`CREATE UNIQUE INDEX IF NOT EXISTS memory_tombstones_topic_uidx
			ON memory_tombstones(topic_key, project, scope)
			WHERE topic_key IS NOT NULL AND topic_key <> ''`,

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
		// Using the shared constant ensures ApplySchema and rebuildMemoriesTable
		// always produce the same virtual table definition.
		ftsVirtualTableDDL,

		// ── FTS maintenance triggers ─────────────────────────────────────────
		// Using the shared constants ensures all code paths (fresh DB, v0→v1
		// rebuild, and v1→v2 trigger migration) always install the same trigger
		// bodies — preventing the drift that originally caused CORRUPT_VTAB (267).
		ftsTriggerInsert,
		ftsTriggerDelete,
		ftsTriggerUpdate,

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
