# memory-model Specification

## Purpose

Defines the canonical typed memory record — the single persistent unit in the engine. Every piece of agent knowledge, SDD artifact, and coding standard is stored as a `memory` row differentiated by `entity_type`. This spec governs schema shape, field semantics, parent relations, FTS5 indexing, and lifecycle columns (version, tombstone).

---

## Requirements

### Requirement: Polymorphic memories table

The system MUST store all entity types in a single `memories` table with an `entity_type` TEXT column acting as a discriminator. The table MUST NOT be split into per-type tables in this change.

Valid `entity_type` values MUST include: `memory`, `change`, `spec`, `task`, `standard`, `plan`.

#### Scenario: Save a generic memory record

- GIVEN the store is initialized
- WHEN a write is issued with `entity_type = 'memory'`
- THEN the row is persisted with the supplied `entity_type`, `title`, `content`, `project`, and `scope`

#### Scenario: Save a typed SDD entity

- GIVEN the store is initialized
- WHEN a write is issued with `entity_type = 'task'` and `status = 'todo'`
- THEN the row is persisted with `entity_type = 'task'` and `status = 'todo'`

#### Scenario: Reject unknown entity_type

- GIVEN the store is initialized
- WHEN a write is issued with an `entity_type` not in the valid set
- THEN the write MUST be rejected with a validation error before any DB write occurs

---

### Requirement: Required and optional fields

Each `memories` row MUST carry: `sync_id` (globally unique), `session_id`, `entity_type`, `type`, `title`, `content`, `project`, `scope`, `version`, `created_at`, `updated_at`.

The row SHOULD carry: `topic_key`, `status`, `parent_id`, `review_after`, `expires_at`, `deleted_at`.

The row MAY carry: `embedding`, `embedding_model`, `embedding_created_at`.

The `embedding` column MUST be reserved in the schema but MUST NOT be populated by this change.

#### Scenario: Minimal valid write

- GIVEN a write request omitting all optional fields
- WHEN the request is submitted
- THEN the row is inserted with `version = 1`, auto-generated `sync_id`, and `created_at = updated_at = now()`

#### Scenario: Embedding column reserved but empty

- GIVEN a row written in this change
- WHEN the row is read back
- THEN `embedding` IS NULL and no error is raised about the column's absence

---

### Requirement: SDD entity status lifecycle

Rows with `entity_type` in (`change`, `spec`, `task`, `standard`, `plan`) MUST support a `status` TEXT column.

Allowed status values per type:

| entity_type | Allowed status values |
|-------------|----------------------|
| `change`    | `planning`, `in-progress`, `done`, `archived` |
| `spec`      | `draft`, `approved`, `superseded` |
| `task`      | `todo`, `in-progress`, `done`, `blocked` |
| `standard`  | `active`, `deprecated` |
| `plan`      | `draft`, `active`, `archived` |

The system MUST NOT enforce status validity at the DB constraint level in this change; enforcement is application-level.

#### Scenario: Task status transition

- GIVEN a task row with `status = 'todo'`
- WHEN an update sets `status = 'in-progress'`
- THEN the row reflects `status = 'in-progress'` and `updated_at` is refreshed

---

### Requirement: Parent relations via parent_sync_id — strict SDD hierarchy

A `memories` row MAY reference another row via `parent_sync_id TEXT REFERENCES memories(sync_id)`. This establishes single-parent containment (task→spec, spec→change, plan→change).

**Hierarchy enforcement (DB-level CHECK):**

| entity_type | `parent_sync_id` requirement |
|-------------|------------------------------|
| `spec`      | MUST NOT be NULL             |
| `task`      | MUST NOT be NULL             |
| `plan`      | MUST NOT be NULL             |
| `memory`    | MAY be NULL (root-level OK)  |
| `change`    | MAY be NULL (root-level OK)  |
| `standard`  | MAY be NULL (root-level OK)  |

The DB CHECK constraint enforces: `entity_type IN ('memory','change','standard') OR parent_sync_id IS NOT NULL`.

NOTE: a hard deferred foreign key from `parent_sync_id` to `memories(sync_id)` is intentionally NOT added in this change. Blocking parent referential integrity would reject out-of-order mutations during sync apply. Defer-and-replay enforcement is deferred to PR3/PR4.

#### Scenario: Spec linked to a change

- GIVEN a `change` row with `sync_id = 'chg-001'`
- WHEN a `spec` row is written with `parent_sync_id = 'chg-001'`
- THEN the spec row is stored and a query for children of `'chg-001'` returns the spec

#### Scenario: Orphan spec rejected

- GIVEN no parent row exists
- WHEN a `spec` row is written with `parent_sync_id = NULL`
- THEN the write MUST be rejected by the DB CHECK constraint

#### Scenario: Orphan task rejected

- GIVEN no parent row exists
- WHEN a `task` row is written with `parent_sync_id = NULL`
- THEN the write MUST be rejected by the DB CHECK constraint

#### Scenario: Orphan plan rejected

- GIVEN no parent row exists
- WHEN a `plan` row is written with `parent_sync_id = NULL`
- THEN the write MUST be rejected by the DB CHECK constraint

#### Scenario: Root-level memory with no parent

- GIVEN no parent row exists
- WHEN a `memory` row is written with `parent_sync_id = NULL`
- THEN the row is stored without error

#### Scenario: Root-level change with no parent

- GIVEN no parent row exists
- WHEN a `change` row is written with `parent_sync_id = NULL`
- THEN the row is stored without error

#### Scenario: Root-level standard with no parent

- GIVEN no parent row exists
- WHEN a `standard` row is written with `parent_sync_id = NULL`
- THEN the row is stored without error

---

### Requirement: version field for optimistic concurrency

Every `memories` row MUST have a `version INTEGER NOT NULL DEFAULT 1`. The `version` MUST be incremented by 1 on each successful update. Writes MUST carry the expected `version`; if the stored `version` differs, the write MUST be rejected.

#### Scenario: Successful version-guarded update

- GIVEN a row at `version = 3`
- WHEN an update arrives with expected version `3`
- THEN the update is applied and `version` becomes `4`

#### Scenario: Stale version rejected

- GIVEN a row at `version = 3`
- WHEN an update arrives with expected version `2`
- THEN the write is rejected and the row remains unchanged at `version = 3`

---

### Requirement: Soft-delete via deleted_at (tombstone column)

The system MUST support soft-deletion by setting `deleted_at TIMESTAMPTZ` on the row. Soft-deleted rows MUST NOT appear in normal read queries unless explicitly requested. The system MUST NOT physically delete rows from the `memories` table.

#### Scenario: Soft delete a row

- GIVEN a live row (deleted_at IS NULL)
- WHEN a delete operation is issued
- THEN `deleted_at` is set to the current timestamp and the row is excluded from default reads

#### Scenario: Soft-deleted row excluded from search

- GIVEN a row with `deleted_at` set
- WHEN a full-text search query is executed
- THEN the deleted row is NOT returned

---

### Requirement: Local FTS5 index (SQLite only)

The local SQLite store MUST maintain a virtual FTS5 table indexed over `title`, `content`, `entity_type`, `type`, `project`, `topic_key` columns from `memories`.

The FTS5 index MUST be kept in sync with the base table via INSERT, UPDATE, and DELETE triggers.

The system MUST sanitize user-supplied FTS5 queries to prevent operator injection before executing them.

#### Scenario: Full-text search returns relevant row

- GIVEN a row with `title = 'Authentication bug fix'` and `entity_type = 'memory'`
- WHEN a search query `authentication` is executed
- THEN the row is returned in results

#### Scenario: FTS query with special characters is sanitized

- GIVEN a search query containing `AND OR` (bare FTS5 operators)
- WHEN the query is passed to the search function
- THEN the sanitizer wraps/escapes terms and no SQL error is raised

#### Scenario: FTS index reflects update

- GIVEN a row with `title = 'Old title'` in the FTS index
- WHEN the row is updated with `title = 'New title'`
- THEN searching for `'New title'` returns the row and `'Old title'` does NOT

---

### Requirement: Local SQLite WAL mode and pragmas

The local SQLite connection MUST be configured with: `PRAGMA journal_mode = WAL`, `PRAGMA busy_timeout = 5000`, `PRAGMA synchronous = NORMAL`, `PRAGMA foreign_keys = ON`. Maximum open connections MUST be set to 1. The application MUST retry on SQLITE_BUSY/LOCKED with exponential backoff.

#### Scenario: Concurrent write under busy lock

- GIVEN the SQLite store is open with WAL mode
- WHEN a second write attempt arrives while the first holds a write lock
- THEN the second write retries with backoff and succeeds within the busy_timeout window

---

### Requirement: Local data directory on Windows

The local SQLite database MUST be stored at a path derived from `os.UserHomeDir()`. The path MUST use standard Go filepath joining (no hardcoded separators). The system MUST handle Windows long-path scenarios.

#### Scenario: Data directory resolved on Windows

- GIVEN `os.UserHomeDir()` returns `C:\Users\alice`
- WHEN the store is initialized
- THEN the database file is created at `C:\Users\alice\.<engine-name>\local.db` (no path separator errors)
