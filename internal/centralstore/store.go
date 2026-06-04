package centralstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// querier is the subset of the pgx API shared by *pgxpool.Pool and pgx.Tx.
// Both the pool (autocommit, one statement per acquired conn) and a transaction
// (many statements on one conn) satisfy it, so the read/write cores below can run
// either standalone (public Store methods) or inside a single Apply transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ── domain.Reader implementation ─────────────────────────────────────────────
//
// The public Store methods satisfy domain.Reader against the pool with a
// background context. Each delegates to a ctx+querier core (the *Q functions)
// so the exact same SQL also runs inside an Apply transaction against a pgx.Tx.

// FindByTopic returns the live (non-deleted) record for the given
// (topicKey, project, scope) triple, or nil if none exists.
// Deterministic because central_memories_topic_uidx enforces ≤1 live row.
func (s *Store) FindByTopic(topicKey, project, scope string) (*domain.Record, error) {
	return findByTopicQ(context.Background(), s.pool, topicKey, project, scope)
}

// FindBySyncID returns the record for the given sync_id regardless of
// deletion state (including soft-deleted rows), or nil if not found.
func (s *Store) FindBySyncID(syncID string) (*domain.Record, error) {
	return findBySyncIDQ(context.Background(), s.pool, syncID)
}

// FindTombstone returns the tombstone for the given sync_id or topic_key,
// or nil if no tombstone exists. Prefers sync_id; falls back to topic_key.
// Deterministic because central_tombstones_topic_uidx enforces ≤1 tombstone
// per topic identity.
func (s *Store) FindTombstone(syncID string, topicKey *string, project, scope string) (*domain.Tombstone, error) {
	return findTombstoneQ(context.Background(), s.pool, syncID, topicKey, project, scope)
}

// MutationApplied reports whether a mutation with the given ID has already
// been applied (idempotency guard — Invariant 5).
func (s *Store) MutationApplied(mutationID string) (bool, error) {
	return mutationAppliedQ(context.Background(), s.pool, mutationID)
}

// ── ctx+querier read cores (shared by pool and tx) ───────────────────────────

func findByTopicQ(ctx context.Context, q querier, topicKey, project, scope string) (*domain.Record, error) {
	const sql = `
		SELECT sync_id, session_id, entity_type, type, title, content,
		       project, scope, version, writer_id, last_write_mutation_id,
		       topic_key, status, parent_sync_id,
		       created_at, updated_at, deleted_at
		FROM central_memories
		WHERE topic_key   = $1
		  AND project     = $2
		  AND scope       = $3
		  AND deleted_at IS NULL
		LIMIT 1`
	return scanRecord(q.QueryRow(ctx, sql, topicKey, project, scope))
}

func findBySyncIDQ(ctx context.Context, q querier, syncID string) (*domain.Record, error) {
	const sql = `
		SELECT sync_id, session_id, entity_type, type, title, content,
		       project, scope, version, writer_id, last_write_mutation_id,
		       topic_key, status, parent_sync_id,
		       created_at, updated_at, deleted_at
		FROM central_memories
		WHERE sync_id = $1
		LIMIT 1`
	return scanRecord(q.QueryRow(ctx, sql, syncID))
}

func findTombstoneQ(ctx context.Context, q querier, syncID string, topicKey *string, project, scope string) (*domain.Tombstone, error) {
	const bySyncID = `
		SELECT sync_id, project, scope, topic_key, deleted_at, deleted_by, version, last_write_mutation_id
		FROM central_tombstones
		WHERE sync_id = $1
		LIMIT 1`
	ts, err := scanTombstone(q.QueryRow(ctx, bySyncID, syncID))
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
		FROM central_tombstones
		WHERE topic_key = $1 AND project = $2 AND scope = $3
		LIMIT 1`
	return scanTombstone(q.QueryRow(ctx, byTopic, *topicKey, project, scope))
}

func mutationAppliedQ(ctx context.Context, q querier, mutationID string) (bool, error) {
	const sql = `SELECT 1 FROM central_mutations WHERE mutation_id = $1 LIMIT 1`
	var dummy int
	err := q.QueryRow(ctx, sql, mutationID).Scan(&dummy)
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
//
// central_mutations.payload is JSONB NOT NULL. If m.Payload is nil or empty the
// INSERT would pass NULL, violating the NOT NULL constraint. We default to the
// empty JSON object '{}' in that case without mutating the caller's Mutation.
func (s *Store) InsertMutation(ctx context.Context, m domain.Mutation) (seq int64, err error) {
	return insertMutationQ(ctx, s.pool, m)
}

// insertMutationQ is the ctx+querier core for InsertMutation. Running it on an
// Apply transaction's pgx.Tx makes the seq assignment and the durable
// applied-marker (central_mutations.mutation_id UNIQUE) part of the same atomic
// unit as the reconciliation.
func insertMutationQ(ctx context.Context, q querier, m domain.Mutation) (seq int64, err error) {
	payload := m.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}

	const sql = `
		INSERT INTO central_mutations
		  (mutation_id, entity, entity_key, op, payload, writer_id, project, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING seq`
	err = q.QueryRow(ctx, sql,
		m.MutationID,
		string(m.EntityType),
		m.SyncID,
		string(m.Op),
		payload,
		m.WriterID,
		m.Project,
		m.OccurredAt.UTC(),
	).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("InsertMutation: %w", err)
	}
	return seq, nil
}

// UpsertMemory inserts or updates a row in central_memories keyed by
// targetSyncID. targetSyncID is the resolved row identity returned by
// domain.Decide (Decision.TargetSyncID). For same-writer upserts
// targetSyncID == m.SyncID; for cross-writer topic convergence it is the
// canonical row's sync_id Y (which may differ from m.SyncID X). Using
// targetSyncID as the primary key ensures ON CONFLICT(sync_id) addresses the
// correct canonical row and avoids hitting central_memories_topic_uidx with a
// second live row under a different sync_id.
//
// The caller (Apply) is responsible for running Decide() and passing only
// winning mutations here. central_memories carries no seq column — the LWW
// tiebreaker is last_write_mutation_id, and the journal/ordering seq lives in
// central_mutations (with the client's local sync_state pull cursor).
func (s *Store) UpsertMemory(ctx context.Context, targetSyncID string, m domain.Mutation) error {
	return upsertMemoryQ(ctx, s.pool, targetSyncID, m)
}

// upsertMemoryQ is the ctx+querier core for UpsertMemory. Apply runs it on its
// transaction so the canonical-state write commits atomically with the mutation
// journal entry and the applied-marker.
func upsertMemoryQ(ctx context.Context, qr querier, targetSyncID string, m domain.Mutation) error {
	// created_at is intentionally omitted from the INSERT column list so the
	// server's DEFAULT now() applies. This makes created_at server-authoritative
	// and immune to client clock skew. created_at must NOT appear in the
	// ON CONFLICT DO UPDATE SET list either (it stays immutable after first
	// creation). updated_at IS updated — it is the client logical write time used
	// by writeWins() as the LWW tiebreaker across writers.
	//
	// seq (the materialized-row copy of central_mutations.seq) was removed: it was
	// dead weight — the journal/ordering authority is central_mutations.seq, and the
	// pull cursor is the CLIENT's local sync_state.last_pulled_seq (a local SQLite
	// table) advanced via Mutation.Seq; no production code read the materialized copy.
	//
	// Parameter order ($1..$15):
	//   $1  targetSyncID  $2  session_id    $3  entity_type   $4  type
	//   $5  status        $6  title         $7  content       $8  project
	//   $9  scope         $10 topic_key     $11 parent_sync_id $12 version
	//   $13 writer_id (used twice: writer_id column AND created_by column)
	//   $14 updated_at    $15 last_write_mutation_id
	const q = `
		INSERT INTO central_memories
		  (sync_id, session_id, entity_type, type, status, title, content,
		   project, scope, topic_key, parent_sync_id, version, writer_id,
		   created_by, updated_at, last_write_mutation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$13,$14,$15)
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
		  writer_id      = EXCLUDED.writer_id,
		  updated_at     = EXCLUDED.updated_at,
		  last_write_mutation_id = EXCLUDED.last_write_mutation_id,
		  deleted_at     = NULL`
	_, err := qr.Exec(ctx, q,
		targetSyncID,         // $1  — canonical row identity (may differ from m.SyncID)
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
		m.WriterID,           // $13 — used for both writer_id AND created_by on INSERT
		m.UpdatedAt.UTC(),    // $14 — client logical write time (LWW tiebreaker); NOT used for created_at
		m.MutationID,         // $15 — winning write's content-addressed id; final LWW tiebreaker
	)
	if err != nil {
		return fmt.Errorf("UpsertMemory: %w", err)
	}
	return nil
}

// WriteTombstone inserts or updates a row in central_tombstones keyed by
// targetSyncID. targetSyncID is the resolved row identity returned by
// domain.Decide (Decision.TargetSyncID). For same-writer deletes
// targetSyncID == m.SyncID; for cross-writer re-deletes of an already-
// tombstoned topic it is the canonical tombstone's sync_id Y. Using
// targetSyncID as the primary key ensures ON CONFLICT(sync_id) reuses the
// canonical tombstone and avoids hitting central_tombstones_topic_uidx with a
// second tombstone under a different sync_id (which would be a unique violation).
//
// Metadata (project, scope, topic_key, version, writer_id) always comes from m
// because the DELETE mutation carries the authoritative deletion context.
func (s *Store) WriteTombstone(ctx context.Context, targetSyncID string, m domain.Mutation) error {
	return writeTombstoneQ(ctx, s.pool, targetSyncID, m)
}

// writeTombstoneQ is the ctx+querier core for WriteTombstone. Apply runs it on
// its transaction so the tombstone row and the deleted_at flag on
// central_memories are set within one atomic unit.
//
// The identity tiebreaker fields are deleted_by (writer_id) and
// last_write_mutation_id (the winning delete's content-addressed mutation_id) —
// both replica-identical, so every store computes the same winner. sync_id is the
// tombstone's PK/identity, NOT a tiebreaker (it diverges across replicas).
// See writeWins doc comment in domain/reconcile.go.
func writeTombstoneQ(ctx context.Context, qr querier, targetSyncID string, m domain.Mutation) error {
	const q = `
		INSERT INTO central_tombstones
		  (sync_id, project, scope, topic_key, deleted_at, deleted_by, version, last_write_mutation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (sync_id) DO UPDATE SET
		  project    = EXCLUDED.project,
		  scope      = EXCLUDED.scope,
		  topic_key  = EXCLUDED.topic_key,
		  deleted_at = EXCLUDED.deleted_at,
		  deleted_by = EXCLUDED.deleted_by,
		  version    = EXCLUDED.version,
		  last_write_mutation_id = EXCLUDED.last_write_mutation_id`
	_, err := qr.Exec(ctx, q,
		targetSyncID,      // $1 — canonical tombstone identity (may differ from m.SyncID)
		m.Project,         // $2
		m.Scope,           // $3
		m.TopicKey,        // $4 (nil → NULL)
		m.UpdatedAt.UTC(), // $5
		m.WriterID,        // $6
		m.Version,         // $7
		m.MutationID,      // $8 — winning delete's content-addressed id; final LWW tiebreaker
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
		&r.Project, &r.Scope, &r.Version, &r.WriterID, &r.LastWriteMutationID,
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
		&deletedAt, &ts.DeletedBy, &ts.Version, &ts.LastWriteMutationID,
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
