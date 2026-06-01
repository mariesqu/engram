// Package localstore implements the SQLite adapter for the engram local store.
// It uses modernc.org/sqlite (pure Go — no CGO) with WAL mode.
package localstore

import "database/sql"

// ApplySchema creates all tables, indexes, FTS5 virtual table, and triggers
// in db. All statements use IF NOT EXISTS / CREATE INDEX IF NOT EXISTS so
// the function is fully idempotent and safe to call on every Open.
func ApplySchema(db *sql.DB) error {
	stmts := []string{
		// ── Core memories table ──────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS memories (
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
		)`,

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
		// NOTE: REFERENCES clauses are intentionally omitted here for the same
		// reason as parent_sync_id on memories: PRAGMA foreign_keys = ON enforces
		// them immediately, which would break out-of-order sync. Referential
		// integrity is enforced at the application level.
		`CREATE TABLE IF NOT EXISTS memory_relations (
			from_sync_id TEXT NOT NULL,
			to_sync_id   TEXT NOT NULL,
			rel_type     TEXT NOT NULL DEFAULT 'parent',
			PRIMARY KEY (from_sync_id, to_sync_id, rel_type)
		)`,

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
