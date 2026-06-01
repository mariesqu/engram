package domain

// Reader is the read-only port that Decide() depends on.
// Both the local SQLite adapter and the central Postgres adapter implement it.
// Mock implementations are used in unit tests — no database required.
type Reader interface {
	// FindByTopic returns the live (non-deleted) record for the given
	// (topicKey, project, scope) triple, or nil if none exists.
	FindByTopic(topicKey, project, scope string) (*Record, error)

	// FindBySyncID returns the record for the given sync_id regardless of
	// topic_key, or nil if not found.
	FindBySyncID(syncID string) (*Record, error)

	// FindTombstone returns the tombstone for the given sync_id or topic_key
	// (whichever is non-empty and has an entry), or nil if no tombstone exists.
	FindTombstone(syncID string, topicKey *string, project, scope string) (*Tombstone, error)

	// MutationApplied reports whether a mutation with the given ID has already
	// been applied (idempotency guard — Invariant 5).
	MutationApplied(mutationID string) (bool, error)
}

// Writer is the write port executed by adapters after Decide() returns an Action.
// Local and central adapters each provide their own implementation.
type Writer interface {
	// Insert persists a new record derived from m.
	Insert(m Mutation) error

	// Update overwrites the existing record identified by syncID with data from m.
	Update(syncID string, m Mutation) error

	// WriteTombstone records a soft-delete atomically: sets deleted_at on the
	// memories row AND inserts a row in memory_tombstones.
	WriteTombstone(m Mutation) error

	// RecordApplied marks mutation_id as applied in applied_mutations (INV 5).
	RecordApplied(mutationID string) error
}
