package localstore

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver; registers "sqlite" driver name

	"github.com/mariesqu/engram/internal/domain"
)

// Store is the local SQLite adapter. It implements domain.Reader.
//
// Concurrency model: all write paths (AddObservation, LocalWrite, ApplyPulled,
// AckMutation, SetPullCursor, SetPullCursorFor, CreateSession, EndSession) are
// serialized under mu. Read paths (FindByTopic, FindBySyncID, SearchMemories,
// GetObservation, RecentObservations, Search*, RecentSessions, PullCursor,
// PullCursorFor, DrainOutbox, PendingCount, ListProjects) do NOT take mu — they
// may run concurrently with each other and with an in-flight write (WAL mode
// provides snapshot isolation for readers).
//
// Re-entrancy: public write methods acquire mu.Lock() once and delegate to
// unexported *Locked helpers, avoiding deadlock when one write method calls
// another (e.g. AddObservation calls localWriteLocked, not LocalWrite).
//
// Why mu AND SetMaxOpenConns(1): the single connection serializes each individual
// SQL statement, but NOT a multi-statement read-modify-write. mu makes the whole
// sequence atomic — e.g. AddObservation's version pre-read → write → PK resolution
// must not have a concurrent ApplyPulled commit in between, which would make the
// pre-read stale and let the re-save tie/lose the LWW. mu is therefore NOT
// redundant with SetMaxOpenConns(1); removing it reintroduces that race. See
// TestWriteQueue_VersionProgressionNoCollision.
type Store struct {
	db *sql.DB
	mu sync.Mutex // serializes all write operations (read-modify-write atomicity)
}

// Open opens (or creates) the SQLite database at path, applies WAL pragmas,
// sets max-open-conns to 1, and runs ApplySchema.
// On Windows, path should be an absolute path; long-path prefixes are handled
// by filepath.Join in callers and Go's os package.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("localstore.Open: sql.Open: %w", err)
	}

	// Single-writer rule: SQLite WAL allows concurrent readers but only one
	// writer at a time. One open connection eliminates SQLITE_BUSY contention.
	db.SetMaxOpenConns(1)

	// Pragmas verbatim from old_code store.go:602.
	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("localstore.Open: pragma %q: %w", p, err)
		}
	}

	if err := ApplySchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("localstore.Open: ApplySchema: %w", err)
	}

	if err := runMigrations(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("localstore.Open: runMigrations: %w", err)
	}

	return &Store{db: db}, nil
}

// Close releases the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB exposes the underlying *sql.DB for READ-ONLY raw queries (the sync harness,
// tests, and the Reader implementation). Mirrors centralstore.Store.Pool().
//
// WRITES must go through the Store's typed methods, which hold s.mu to serialize
// the write-queue. Passing DB() to the package-level Apply (or running any raw
// write) from production or a concurrent goroutine BYPASSES s.mu and reintroduces
// the write race. The connection is configured with SetMaxOpenConns(1).
func (s *Store) DB() *sql.DB {
	return s.db
}

// ── domain.Reader implementation ─────────────────────────────────────────────

// rowQuerier is the minimal interface shared by *sql.DB and *sql.Tx that is
// sufficient for the Reader query cores. Both types implement QueryRow with
// the same signature, allowing the *Q helper functions below to be called from
// either a live connection (via s.db) or an in-flight transaction (via tx).
type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// findByTopicQ is the query core for FindByTopic, parameterised over rowQuerier
// so it can run inside or outside a transaction.
func findByTopicQ(q rowQuerier, topicKey, project, scope string) (*domain.Record, error) {
	const query = `
		SELECT sync_id, session_id, entity_type, type, title, content,
		       project, scope, version, writer_id, last_write_mutation_id,
		       topic_key, status, parent_sync_id,
		       created_at, updated_at, deleted_at
		FROM memories
		WHERE topic_key = ?
		  AND project   = ?
		  AND scope     = ?
		  AND deleted_at IS NULL
		LIMIT 1`
	return scanRecord(q.QueryRow(query, topicKey, project, scope))
}

// findBySyncIDQ is the query core for FindBySyncID.
func findBySyncIDQ(q rowQuerier, syncID string) (*domain.Record, error) {
	const query = `
		SELECT sync_id, session_id, entity_type, type, title, content,
		       project, scope, version, writer_id, last_write_mutation_id,
		       topic_key, status, parent_sync_id,
		       created_at, updated_at, deleted_at
		FROM memories
		WHERE sync_id = ?
		LIMIT 1`
	return scanRecord(q.QueryRow(query, syncID))
}

// findTombstoneQ is the query core for FindTombstone.
func findTombstoneQ(q rowQuerier, syncID string, topicKey *string, project, scope string) (*domain.Tombstone, error) {
	const bySyncID = `
		SELECT sync_id, project, scope, topic_key, deleted_at, deleted_by, version, last_write_mutation_id
		FROM memory_tombstones
		WHERE sync_id = ?
		LIMIT 1`
	ts, err := scanTombstone(q.QueryRow(bySyncID, syncID))
	if err != nil {
		return nil, err
	}
	if ts != nil {
		return ts, nil
	}
	if topicKey == nil || *topicKey == "" {
		return nil, nil
	}
	const byTopic = `
		SELECT sync_id, project, scope, topic_key, deleted_at, deleted_by, version, last_write_mutation_id
		FROM memory_tombstones
		WHERE topic_key = ? AND project = ? AND scope = ?
		LIMIT 1`
	return scanTombstone(q.QueryRow(byTopic, *topicKey, project, scope))
}

// mutationAppliedQ is the query core for MutationApplied.
func mutationAppliedQ(q rowQuerier, mutationID string) (bool, error) {
	var count int
	err := q.QueryRow(
		`SELECT count(*) FROM applied_mutations WHERE mutation_id = ?`, mutationID,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("mutationAppliedQ: %w", err)
	}
	return count > 0, nil
}

// txReader adapts an in-flight *sql.Tx to domain.Reader so that domain.Decide
// reads from the same snapshot the pending writes will commit against. This
// mirrors centralstore's decideReader pattern.
//
// With db.SetMaxOpenConns(1) the single connection is held by the transaction
// for its entire duration, so Decide + applyTx + enqueueOutboxTx are all
// serialized on that connection — no interleaving from a concurrent goroutine
// is possible while the tx is open.
type txReader struct {
	tx *sql.Tx
}

func (r *txReader) FindByTopic(topicKey, project, scope string) (*domain.Record, error) {
	return findByTopicQ(r.tx, topicKey, project, scope)
}

func (r *txReader) FindBySyncID(syncID string) (*domain.Record, error) {
	return findBySyncIDQ(r.tx, syncID)
}

func (r *txReader) FindTombstone(syncID string, topicKey *string, project, scope string) (*domain.Tombstone, error) {
	return findTombstoneQ(r.tx, syncID, topicKey, project, scope)
}

func (r *txReader) MutationApplied(mutationID string) (bool, error) {
	return mutationAppliedQ(r.tx, mutationID)
}

// FindByTopic returns the live (non-deleted) record for the given
// (topicKey, project, scope) triple, or nil if none exists.
func (s *Store) FindByTopic(topicKey, project, scope string) (*domain.Record, error) {
	return findByTopicQ(s.db, topicKey, project, scope)
}

// FindBySyncID returns the record for the given sync_id, or nil if not found.
func (s *Store) FindBySyncID(syncID string) (*domain.Record, error) {
	return findBySyncIDQ(s.db, syncID)
}

// FindTombstone returns the tombstone for the given sync_id or topic_key,
// or nil if no tombstone exists.
func (s *Store) FindTombstone(syncID string, topicKey *string, project, scope string) (*domain.Tombstone, error) {
	return findTombstoneQ(s.db, syncID, topicKey, project, scope)
}

// MutationApplied reports whether a mutation with the given ID has already been
// applied (idempotency guard — Invariant 5).
func (s *Store) MutationApplied(mutationID string) (bool, error) {
	return mutationAppliedQ(s.db, mutationID)
}

// ── Search ────────────────────────────────────────────────────────────────────

// SearchMemories performs an FTS5 search over live (non-deleted) memories in
// the given project and returns up to limit results ordered by BM25 rank.
func (s *Store) SearchMemories(query, project string, limit int) ([]*domain.Record, error) {
	if limit <= 0 {
		limit = 10
	}
	ftsQ := sanitizeFTS(query)
	if ftsQ == "" {
		return nil, nil
	}
	const q = `
		SELECT m.sync_id, m.session_id, m.entity_type, m.type, m.title, m.content,
		       m.project, m.scope, m.version, m.writer_id, m.last_write_mutation_id,
		       m.topic_key, m.status, m.parent_sync_id,
		       m.created_at, m.updated_at, m.deleted_at
		FROM memories_fts fts
		JOIN memories m ON m.id = fts.rowid
		WHERE memories_fts MATCH ?
		  AND m.project = ?
		  AND m.deleted_at IS NULL
		ORDER BY fts.rank
		LIMIT ?`
	rows, err := s.db.Query(q, ftsQ, project, limit)
	if err != nil {
		return nil, fmt.Errorf("SearchMemories: %w", err)
	}
	defer rows.Close()

	var results []*domain.Record
	for rows.Next() {
		r, err := scanRecordFromRows(rows)
		if err != nil {
			return nil, fmt.Errorf("SearchMemories scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ── sanitizeFTS ───────────────────────────────────────────────────────────────

// sanitizeFTS wraps each word in quotes so FTS5 doesn't choke on special chars
// or operator keywords. Port of old_code store.go:6227.
// "fix auth bug" → `"fix" "auth" "bug"`
//
// Interior double-quotes are removed entirely (not just leading/trailing) before
// re-wrapping, so a query like `a"b` or `foo" OR title:"bar` never produces an
// unterminated FTS5 string literal. Tokens that become empty after cleaning are
// skipped to avoid emitting bare `""` into the FTS expression.
func sanitizeFTS(query string) string {
	words := strings.Fields(query)
	out := make([]string, 0, len(words))
	for _, w := range words {
		// Remove ALL double-quote characters (not just leading/trailing) to
		// prevent interior quotes from producing unterminated FTS5 literals.
		w = strings.ReplaceAll(w, `"`, "")
		if w == "" {
			continue // skip tokens that were entirely quote characters
		}
		out = append(out, `"`+w+`"`)
	}
	return strings.Join(out, " ")
}

// ── scan helpers ──────────────────────────────────────────────────────────────

// scanRecord reads one Record from a single *sql.Row. Returns (nil, nil) on
// sql.ErrNoRows.
func scanRecord(row *sql.Row) (*domain.Record, error) {
	var r domain.Record
	var topicKey, status, parentSyncID sql.NullString
	var deletedAt sql.NullString
	var createdAtStr, updatedAtStr string

	err := row.Scan(
		&r.SyncID, &r.SessionID, &r.EntityType, &r.Type, &r.Title, &r.Content,
		&r.Project, &r.Scope, &r.Version, &r.WriterID, &r.LastWriteMutationID,
		&topicKey, &status, &parentSyncID,
		&createdAtStr, &updatedAtStr, &deletedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if topicKey.Valid {
		r.TopicKey = &topicKey.String
	}
	if status.Valid {
		r.Status = &status.String
	}
	if parentSyncID.Valid {
		r.ParentSyncID = &parentSyncID.String
	}
	r.CreatedAt = parseTime(createdAtStr)
	r.UpdatedAt = parseTime(updatedAtStr)
	if deletedAt.Valid {
		t := parseTime(deletedAt.String)
		r.DeletedAt = &t
	}
	return &r, nil
}

// scanRecordFromRows reads one Record from an open *sql.Rows cursor.
func scanRecordFromRows(rows *sql.Rows) (*domain.Record, error) {
	var r domain.Record
	var topicKey, status, parentSyncID sql.NullString
	var deletedAt sql.NullString
	var createdAtStr, updatedAtStr string

	err := rows.Scan(
		&r.SyncID, &r.SessionID, &r.EntityType, &r.Type, &r.Title, &r.Content,
		&r.Project, &r.Scope, &r.Version, &r.WriterID, &r.LastWriteMutationID,
		&topicKey, &status, &parentSyncID,
		&createdAtStr, &updatedAtStr, &deletedAt,
	)
	if err != nil {
		return nil, err
	}
	if topicKey.Valid {
		r.TopicKey = &topicKey.String
	}
	if status.Valid {
		r.Status = &status.String
	}
	if parentSyncID.Valid {
		r.ParentSyncID = &parentSyncID.String
	}
	r.CreatedAt = parseTime(createdAtStr)
	r.UpdatedAt = parseTime(updatedAtStr)
	if deletedAt.Valid {
		t := parseTime(deletedAt.String)
		r.DeletedAt = &t
	}
	return &r, nil
}

// scanRecordWithIDFromRows reads one Record plus its integer primary key from an
// open *sql.Rows cursor. The SELECT must list `id` as the FIRST column, followed
// by the standard record columns. Used by the search/recent read paths so the
// mem_search → mem_get_observation(id) workflow surfaces a real id.
func scanRecordWithIDFromRows(rows *sql.Rows) (*domain.Record, error) {
	var r domain.Record
	var topicKey, status, parentSyncID sql.NullString
	var deletedAt sql.NullString
	var createdAtStr, updatedAtStr string

	err := rows.Scan(
		&r.ID,
		&r.SyncID, &r.SessionID, &r.EntityType, &r.Type, &r.Title, &r.Content,
		&r.Project, &r.Scope, &r.Version, &r.WriterID, &r.LastWriteMutationID,
		&topicKey, &status, &parentSyncID,
		&createdAtStr, &updatedAtStr, &deletedAt,
	)
	if err != nil {
		return nil, err
	}
	if topicKey.Valid {
		r.TopicKey = &topicKey.String
	}
	if status.Valid {
		r.Status = &status.String
	}
	if parentSyncID.Valid {
		r.ParentSyncID = &parentSyncID.String
	}
	r.CreatedAt = parseTime(createdAtStr)
	r.UpdatedAt = parseTime(updatedAtStr)
	if deletedAt.Valid {
		t := parseTime(deletedAt.String)
		r.DeletedAt = &t
	}
	return &r, nil
}

func scanTombstone(row *sql.Row) (*domain.Tombstone, error) {
	var ts domain.Tombstone
	var topicKey sql.NullString
	var deletedAtStr string

	err := row.Scan(
		&ts.SyncID, &ts.Project, &ts.Scope, &topicKey,
		&deletedAtStr, &ts.DeletedBy, &ts.Version, &ts.LastWriteMutationID,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if topicKey.Valid {
		ts.TopicKey = &topicKey.String
	}
	ts.DeletedAt = parseTime(deletedAtStr)
	return &ts, nil
}

// parseTime parses an RFC3339Nano or SQLite datetime('now') formatted string.
// Returns zero time on error.
func parseTime(s string) time.Time {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05Z",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
