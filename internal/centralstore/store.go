package centralstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mariesqu/engram/internal/domain"
)

// Store is the central Postgres adapter. It wraps a pgxpool.Pool and implements
// domain.Reader. The pool is safe for concurrent use — pgxpool manages
// connection acquisition internally.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres at dsn, creates the pgxpool, and applies the
// central schema idempotently. The returned Store is ready to use.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("centralstore.Open: pgxpool.New: %w", err)
	}

	// Ping to verify connectivity before returning.
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("centralstore.Open: ping: %w", err)
	}

	s := &Store{pool: pool}
	if err = ApplySchema(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("centralstore.Open: ApplySchema: %w", err)
	}
	return s, nil
}

// Close releases all pool connections.
func (s *Store) Close() {
	s.pool.Close()
}

// Pool exposes the underlying pgxpool so callers (e.g. tests) can run raw
// queries without going through the Store API.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// ── domain.Reader implementation ─────────────────────────────────────────────

// FindByTopic returns the live (non-deleted) record for the given
// (topicKey, project, scope) triple, or nil if none exists.
// Deterministic because central_memories_topic_uidx enforces ≤1 live row.
func (s *Store) FindByTopic(topicKey, project, scope string) (*domain.Record, error) {
	const q = `
		SELECT sync_id, session_id, entity_type, type, title, content,
		       project, scope, version, seq, writer_id,
		       topic_key, status, parent_sync_id,
		       created_at, updated_at, deleted_at
		FROM central_memories
		WHERE topic_key   = $1
		  AND project     = $2
		  AND scope       = $3
		  AND deleted_at IS NULL
		LIMIT 1`
	row := s.pool.QueryRow(context.Background(), q, topicKey, project, scope)
	return scanRecord(row)
}

// FindBySyncID returns the record for the given sync_id regardless of
// deletion state (including soft-deleted rows), or nil if not found.
func (s *Store) FindBySyncID(syncID string) (*domain.Record, error) {
	const q = `
		SELECT sync_id, session_id, entity_type, type, title, content,
		       project, scope, version, seq, writer_id,
		       topic_key, status, parent_sync_id,
		       created_at, updated_at, deleted_at
		FROM central_memories
		WHERE sync_id = $1
		LIMIT 1`
	row := s.pool.QueryRow(context.Background(), q, syncID)
	return scanRecord(row)
}

// FindTombstone returns the tombstone for the given sync_id or topic_key,
// or nil if no tombstone exists. Prefers sync_id; falls back to topic_key.
// Deterministic because central_tombstones_topic_uidx enforces ≤1 tombstone
// per topic identity.
func (s *Store) FindTombstone(syncID string, topicKey *string, project, scope string) (*domain.Tombstone, error) {
	const bySyncID = `
		SELECT sync_id, project, scope, topic_key, deleted_at, deleted_by, version
		FROM central_tombstones
		WHERE sync_id = $1
		LIMIT 1`
	ts, err := scanTombstone(s.pool.QueryRow(context.Background(), bySyncID, syncID))
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
		SELECT sync_id, project, scope, topic_key, deleted_at, deleted_by, version
		FROM central_tombstones
		WHERE topic_key = $1 AND project = $2 AND scope = $3
		LIMIT 1`
	return scanTombstone(s.pool.QueryRow(context.Background(), byTopic, *topicKey, project, scope))
}

// MutationApplied reports whether a mutation with the given ID has already
// been applied (idempotency guard — Invariant 5).
func (s *Store) MutationApplied(mutationID string) (bool, error) {
	const q = `SELECT 1 FROM central_mutations WHERE mutation_id = $1 LIMIT 1`
	var dummy int
	err := s.pool.QueryRow(context.Background(), q, mutationID).Scan(&dummy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("MutationApplied: %w", err)
	}
	return true, nil
}

// ── Write primitives (building blocks for PR3b Decide-driven Apply) ──────────

// InsertMutation inserts a row into central_mutations and returns the BIGSERIAL
// seq assigned by Postgres. This is the seq authority: the returned seq is the
// canonical monotonic order for this mutation.
func (s *Store) InsertMutation(ctx context.Context, m domain.Mutation) (seq int64, err error) {
	const q = `
		INSERT INTO central_mutations
		  (mutation_id, entity, entity_key, op, payload, writer_id, project, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING seq`
	err = s.pool.QueryRow(ctx, q,
		m.MutationID,
		string(m.EntityType),
		m.SyncID,
		string(m.Op),
		m.Payload,
		m.WriterID,
		m.Project,
		m.OccurredAt.UTC(),
	).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("InsertMutation: %w", err)
	}
	return seq, nil
}

// UpsertMemory inserts or updates a row in central_memories.
// On conflict on sync_id the row is updated with the incoming values.
// The caller (PR3b Decide-driven Apply) is responsible for running Decide()
// and passing only winning mutations here.
func (s *Store) UpsertMemory(ctx context.Context, m domain.Mutation, seq int64) error {
	const q = `
		INSERT INTO central_memories
		  (sync_id, session_id, entity_type, type, status, title, content,
		   project, scope, topic_key, parent_sync_id, version, seq, writer_id,
		   created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$14,$15,$15)
		ON CONFLICT (sync_id) DO UPDATE SET
		  session_id     = EXCLUDED.session_id,
		  entity_type    = EXCLUDED.entity_type,
		  type           = EXCLUDED.type,
		  status         = EXCLUDED.status,
		  title          = EXCLUDED.title,
		  content        = EXCLUDED.content,
		  project        = EXCLUDED.project,
		  scope          = EXCLUDED.scope,
		  topic_key      = EXCLUDED.topic_key,
		  parent_sync_id = EXCLUDED.parent_sync_id,
		  version        = EXCLUDED.version,
		  seq            = EXCLUDED.seq,
		  writer_id      = EXCLUDED.writer_id,
		  updated_at     = EXCLUDED.updated_at,
		  deleted_at     = NULL`
	_, err := s.pool.Exec(ctx, q,
		m.SyncID,             // $1
		m.SessionID,          // $2
		string(m.EntityType), // $3
		m.Type,               // $4
		m.Status,             // $5  (nil → NULL)
		m.Title,              // $6
		m.Content,            // $7
		m.Project,            // $8
		m.Scope,              // $9
		m.TopicKey,           // $10 (nil → NULL)
		m.ParentSyncID,       // $11 (nil → NULL)
		m.Version,            // $12
		seq,                  // $13
		m.WriterID,           // $14 (used for both writer_id AND created_by on INSERT via $14 twice)
		m.UpdatedAt.UTC(),    // $15 (used for both created_at AND updated_at on INSERT via $15 twice)
	)
	if err != nil {
		return fmt.Errorf("UpsertMemory: %w", err)
	}
	return nil
}

// WriteTombstone inserts a row into central_tombstones. Uses INSERT OR REPLACE
// semantics (ON CONFLICT DO UPDATE) so re-deleting the same sync_id is safe.
func (s *Store) WriteTombstone(ctx context.Context, m domain.Mutation) error {
	const q = `
		INSERT INTO central_tombstones
		  (sync_id, project, scope, topic_key, deleted_at, deleted_by, version)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (sync_id) DO UPDATE SET
		  project    = EXCLUDED.project,
		  scope      = EXCLUDED.scope,
		  topic_key  = EXCLUDED.topic_key,
		  deleted_at = EXCLUDED.deleted_at,
		  deleted_by = EXCLUDED.deleted_by,
		  version    = EXCLUDED.version`
	_, err := s.pool.Exec(ctx, q,
		m.SyncID,
		m.Project,
		m.Scope,
		m.TopicKey,
		m.UpdatedAt.UTC(),
		m.WriterID,
		m.Version,
	)
	if err != nil {
		return fmt.Errorf("WriteTombstone: %w", err)
	}
	return nil
}

// ── scan helpers ──────────────────────────────────────────────────────────────

func scanRecord(row pgx.Row) (*domain.Record, error) {
	var r domain.Record
	var topicKey, status, parentSyncID *string
	var deletedAt *time.Time
	var createdAt, updatedAt time.Time

	err := row.Scan(
		&r.SyncID, &r.SessionID, &r.EntityType, &r.Type, &r.Title, &r.Content,
		&r.Project, &r.Scope, &r.Version, &r.Seq, &r.WriterID,
		&topicKey, &status, &parentSyncID,
		&createdAt, &updatedAt, &deletedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.TopicKey = topicKey
	r.Status = status
	r.ParentSyncID = parentSyncID
	r.CreatedAt = createdAt.UTC()
	r.UpdatedAt = updatedAt.UTC()
	if deletedAt != nil {
		t := deletedAt.UTC()
		r.DeletedAt = &t
	}
	return &r, nil
}

func scanTombstone(row pgx.Row) (*domain.Tombstone, error) {
	var ts domain.Tombstone
	var topicKey *string
	var deletedAt time.Time

	err := row.Scan(
		&ts.SyncID, &ts.Project, &ts.Scope, &topicKey,
		&deletedAt, &ts.DeletedBy, &ts.Version,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	ts.TopicKey = topicKey
	ts.DeletedAt = deletedAt.UTC()
	return &ts, nil
}
