// Package domain contains the pure core types and invariants for engram.
// No I/O, no database — only types and interfaces.
package domain

import "time"

// EntityType discriminates rows in the polymorphic memories table.
type EntityType string

const (
	EntityMemory   EntityType = "memory"
	EntityChange   EntityType = "change"
	EntitySpec     EntityType = "spec"
	EntityTask     EntityType = "task"
	EntityStandard EntityType = "standard"
	EntityPlan     EntityType = "plan"
)

// ValidEntityType reports whether et is a recognised EntityType.
func ValidEntityType(et EntityType) bool {
	switch et {
	case EntityMemory, EntityChange, EntitySpec, EntityTask, EntityStandard, EntityPlan:
		return true
	}
	return false
}

// Record is the in-memory representation of a row in the memories table.
// All fields match the column set defined in the local-store schema.
type Record struct {
	// Required
	SyncID    string     `db:"sync_id"`
	SessionID string     `db:"session_id"`
	EntityType EntityType `db:"entity_type"`
	Type      string     `db:"type"`
	Title     string     `db:"title"`
	Content   string     `db:"content"`
	Project   string     `db:"project"`
	Scope     string     `db:"scope"`
	Version   int        `db:"version"`
	WriterID string     `db:"writer_id"`
	// LastWriteMutationID is the content-addressed mutation_id of the WINNING
	// write that last materialized this row. Unlike SyncID (the canonical row PK,
	// fixed at first-insert), this is overwritten on every in-place update/tombstone
	// with the winning mutation's id, so it is REPLICA-IDENTICAL: every store that
	// converges on the same winning write carries the same value. It is the final
	// LWW tiebreaker — see writeWins in reconcile.go.
	LastWriteMutationID string `db:"last_write_mutation_id"`
	CreatedAt time.Time  `db:"created_at"`
	UpdatedAt time.Time  `db:"updated_at"`

	// Optional
	TopicKey      *string    `db:"topic_key"`
	Status        *string    `db:"status"`
	ParentSyncID  *string    `db:"parent_sync_id"`
	ReviewAfter   *time.Time `db:"review_after"`
	ExpiresAt     *time.Time `db:"expires_at"`
	DeletedAt     *time.Time `db:"deleted_at"`
	NormalizedHash *string   `db:"normalized_hash"`

	// Reserved — never populated in this change
	Embedding      []byte  `db:"embedding"`
	EmbeddingModel *string `db:"embedding_model"`
	EmbeddingCreatedAt *time.Time `db:"embedding_created_at"`
}

// Tombstone records a soft-delete so resurrection is blocked (Invariant 4).
type Tombstone struct {
	SyncID    string    `db:"sync_id"`
	Project   string    `db:"project"`
	Scope     string    `db:"scope"`
	TopicKey  *string   `db:"topic_key"`
	DeletedAt time.Time `db:"deleted_at"`
	DeletedBy string    `db:"deleted_by"` // writer_id — used as tiebreaker by writeWins
	Version   int       `db:"version"`
	// LastWriteMutationID is the content-addressed mutation_id of the WINNING
	// delete that wrote this tombstone. It is the final LWW tiebreaker when
	// DeletedAt, Version, and DeletedBy all tie. Unlike SyncID (the canonical PK,
	// which may differ across replicas for the same topic), it is overwritten with
	// the winning delete's id on every tombstone write, so it is REPLICA-IDENTICAL
	// — every store computes the same winner. See writeWins doc comment.
	LastWriteMutationID string `db:"last_write_mutation_id"`
}

// NormalizeTopicKey canonicalizes a "no topic" mutation: an empty-string TopicKey
// (&"") is treated identically to nil by the reconciliation logic, so it is folded
// to nil here. This guarantees a single on-disk representation of "no topic" (NULL),
// matching every partial topic index's `WHERE topic_key IS NOT NULL` predicate, and
// gives no-topic writes a canonical content-addressed mutation_id regardless of
// whether the caller passed nil or &"".
func NormalizeTopicKey(m Mutation) Mutation {
	if m.TopicKey != nil && *m.TopicKey == "" {
		m.TopicKey = nil
	}
	return m
}

// Op is the mutation operation type.
type Op string

const (
	OpUpsert Op = "upsert"
	OpDelete Op = "delete"
)

// Mutation is the unit of work carried by the push/pull cycle.
// MutationID is content-addressed (SHA-256 of canonical payload).
type Mutation struct {
	MutationID string
	Op         Op
	SyncID     string
	SessionID  string
	EntityType EntityType
	Type       string
	Title      string
	Content    string
	Project    string
	Scope      string
	TopicKey   *string
	ParentSyncID *string
	Status     *string
	Version    int
	// Seq is the JOURNAL seq assigned by central_mutations (BIGSERIAL). It is set
	// by PullSince from the central_mutations row and used by the pull cursor
	// (sync_state.last_pulled_seq) and the harness to advance the pull position.
	// It is NOT a materialized-row field and is NOT stored in memories/central_memories.
	// The LWW tiebreaker uses (updated_at, version, writer_id, MutationID) only;
	// see writeWins in reconcile.go.
	Seq        int64
	UpdatedAt  time.Time
	OccurredAt time.Time
	WriterID   string
	// Payload is the canonical JSON encoding used for MutationID derivation.
	Payload []byte
}

// Action is the low-level operation the adapter must execute.
type Action int

const (
	// NoOp — nothing to do; the stored state is already correct.
	NoOp Action = iota
	// ActionInsert — the record does not exist; insert it.
	ActionInsert
	// ActionUpdate — the record exists and the incoming write wins; update it.
	ActionUpdate
	// ActionWriteTombstone — incoming op is Delete; write tombstone + set deleted_at.
	ActionWriteTombstone
)

// Decision is the enriched result returned by Decide(). It carries the Action
// plus the additional context the adapter needs to execute it correctly.
//
// The bare Action alone is insufficient when:
//   - ActionUpdate is resolved via topic_key: the stored row's sync_id (TargetSyncID)
//     may differ from the incoming mutation's SyncID.  The adapter MUST use
//     TargetSyncID in the WHERE clause, not m.SyncID.
//   - A write supersedes a tombstone: the adapter MUST clear deleted_at on the
//     memories row AND delete the memory_tombstones entry (Undelete = true).
type Decision struct {
	// Action is the operation to execute.
	Action Action

	// TargetSyncID is the sync_id of the row the adapter must operate on.
	//
	//   ActionUpdate         → the resolved row's sync_id (may differ from m.SyncID when
	//                         resolved via FindByTopic, i.e. cross-writer topic convergence).
	//   ActionInsert         → m.SyncID (the incoming record's own identity).
	//   ActionWriteTombstone → the resolved row's sync_id (may differ from m.SyncID for
	//                         cross-writer deletes resolved via topic_key; adapter MUST use
	//                         TargetSyncID, not m.SyncID, in the tombstone write).
	//   NoOp                 → m.SyncID (informational; adapter uses m.SyncID directly).
	TargetSyncID string

	// Undelete signals that the adapter must reverse a prior soft-delete:
	//   • SET deleted_at = NULL on the memories row identified by TargetSyncID.
	//   • DELETE FROM memory_tombstones WHERE sync_id = TargetSyncID (and by
	//     topic_key/project/scope when applicable).
	//
	// True when:
	//   ActionUpdate — the resolved row (TargetSyncID) currently has deleted_at set.
	//   ActionInsert — a tombstone for the incoming sync_id (or topic) was superseded
	//                  and must be cleared before the record becomes live again.
	Undelete bool
}
