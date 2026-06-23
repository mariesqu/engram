package localstore

import (
	"fmt"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// UpdateMemory applies an in-place edit to an existing live memory row identified
// by its integer primary key. The existing row is fetched (confirming it is live),
// then a new OpUpsert mutation is written through LocalWrite so the change is
// both materialized and enqueued in the outbox for push to central.
//
// title and content are required (non-empty) — the caller is responsible for
// pre-validating them. typ is optional; when empty the existing record's type is
// preserved. writerID identifies the writer (used for LWW tiebreaking on central).
//
// Returns the updated record as read back from the store via GetObservation.
// Returns ErrObservationNotFound when id refers to a missing or deleted row.
func (s *Store) UpdateMemory(id int64, title, content, typ, writerID string) (*domain.Record, error) {
	rec, err := s.GetObservation(id)
	if err != nil {
		return nil, fmt.Errorf("UpdateMemory: fetch record %d: %w", id, err)
	}

	effectiveType := rec.Type
	if typ != "" {
		effectiveType = typ
	}

	m := domain.Mutation{
		Op:           domain.OpUpsert,
		SyncID:       rec.SyncID,
		SessionID:    rec.SessionID,
		EntityType:   rec.EntityType,
		Type:         effectiveType,
		Title:        title,
		Content:      content,
		Project:      rec.Project,
		Scope:        rec.Scope,
		TopicKey:     rec.TopicKey,
		ParentSyncID: rec.ParentSyncID,
		Status:       rec.Status,
		Version:      rec.Version + 1,
		WriterID:     writerID,
		UpdatedAt:    time.Now().UTC(),
	}

	if _, err := s.LocalWrite(m); err != nil {
		return nil, fmt.Errorf("UpdateMemory: write: %w", err)
	}

	// Re-fetch the updated record so the caller gets the post-write state.
	return s.GetObservation(id)
}

// DeleteMemory soft-deletes the memory row identified by its integer primary key.
// A tombstone mutation (OpDelete) is written through LocalWrite so the deletion
// is materialized (deleted_at set) and enqueued in the outbox for push to central.
//
// Returns ErrObservationNotFound when id refers to a missing or already-deleted row.
func (s *Store) DeleteMemory(id int64, writerID string) error {
	rec, err := s.GetObservation(id)
	if err != nil {
		return fmt.Errorf("DeleteMemory: fetch record %d: %w", id, err)
	}

	m := domain.Mutation{
		Op:        domain.OpDelete,
		SyncID:    rec.SyncID,
		SessionID: rec.SessionID,
		EntityType: rec.EntityType,
		Type:      rec.Type,
		Title:     rec.Title,
		Content:   rec.Content,
		Project:   rec.Project,
		Scope:     rec.Scope,
		TopicKey:  rec.TopicKey,
		Status:    rec.Status,
		Version:   rec.Version + 1,
		WriterID:  writerID,
		UpdatedAt: time.Now().UTC(),
	}

	if _, err := s.LocalWrite(m); err != nil {
		return fmt.Errorf("DeleteMemory: write: %w", err)
	}
	return nil
}
