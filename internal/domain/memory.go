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
	Seq       int64      `db:"seq"`
	WriterID  string     `db:"writer_id"`
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
	DeletedBy string    `db:"deleted_by"` // writer_id
	Version   int       `db:"version"`
	// Seq is the central seq of the delete mutation that created this tombstone.
	// 0 means not yet server-assigned (own pre-push write, same convention as
	// memories.seq for local-only writes). Populated from the persistent store
	// (local memory_tombstones.seq or central central_tombstones.seq) so that
	// domain.Decide can pass the real tombstone seq to writeWins as the
	// spec-authoritative tiebreaker (spec.md:89-97).
	Seq int64 `db:"seq"`
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
