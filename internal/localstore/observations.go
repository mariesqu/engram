package localstore

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// ErrObservationNotFound is returned by GetObservation when no live row exists
// for the given integer primary key.
var ErrObservationNotFound = errors.New("observation not found")

// AddObservationParams carries the caller-supplied fields for a new memory write.
// All fields except Title are optional (zero values produce sensible defaults).
type AddObservationParams struct {
	// SessionID is the MCP session that originated this write. Required by the
	// memories schema (session_id NOT NULL); callers SHOULD supply a valid session
	// id — an empty string will produce a row that cannot be attributed to any
	// session but is otherwise valid.
	SessionID string

	// Type is the observation category (e.g. "decision", "bugfix", "architecture").
	// Defaults to "manual" when empty.
	Type string

	// Title is the short, searchable label. Required.
	Title string

	// Content is the full observation body.
	Content string

	// Project is the normalized project name (lowercased, trimmed). When empty
	// the row is stored with project="" which is valid but unsearchable by project.
	Project string

	// Scope is "project" (default) or "personal".
	Scope string

	// TopicKey is the optional stable upsert key. When non-empty and a live row
	// with the same (topic_key, project, scope) already exists, LocalWrite calls
	// domain.Decide which produces ActionUpdate — the existing row is updated
	// in-place rather than a new row being inserted.
	TopicKey string

	// WriterID is the node's writer identity — the daemon's --writer-id in central
	// mode, "" in local-only. It is baked into the mutation's canonical payload
	// (and thus the mutation_id) and is the LWW writer tiebreaker. The central
	// server's per-writer HMAC forgery check rejects a pushed mutation whose
	// writer_id does not match the authenticated writer, so a central-mode write
	// MUST carry the daemon's writer id or every push is a 403.
	WriterID string
}

// ObservationResult is the minimal result returned by AddObservation to avoid
// exposing a full domain.Record to callers that only need the IDs.
type ObservationResult struct {
	// ID is the autoincrement integer primary key of the memories row.
	ID int64
	// SyncID is the content-addressed sync identifier assigned at write time.
	SyncID string
}

// AddObservation materializes a new memory write through the reconciliation-
// correct LocalWrite path. It MUST NOT bypass LocalWrite — that function is the
// single correct entry point for local writes: it runs domain.Decide inside the
// transaction (INV5 idempotency guard) and atomically enqueues the outbox row
// (so the write is never lost from the push journal).
//
// Mutation construction:
//   - Op = OpUpsert — observations are always upserts (Decide determines insert vs update).
//   - EntityType = EntityMemory.
//   - SyncID = newObsSyncID() — a random "obs-<8-bytes-hex>" prefix, matching old_code.
//   - Version = 1 — first version; Decide will apply the LWW tiebreaker on conflict.
//   - UpdatedAt = time.Now().UTC() — local wall clock.
//   - WriterID = "" — local writes carry no writer identity until the daemon is
//     configured with a central URL and ENGRAM_WRITER_ID. An empty WriterID is
//     valid at the local layer; the push path fills it from cfg.writerID.
//   - TopicKey: normalized via domain.NormalizeTopicKey (nil when empty string).
//   - Scope: defaults to "project" when empty.
//
// Returns the integer PK of the materialized row (looked up by sync_id after
// LocalWrite commits) and the sync_id. Returns ErrObservationNotFound if the
// row cannot be found after write (should not happen in practice).
func (s *Store) AddObservation(p AddObservationParams) (ObservationResult, error) {
	if p.Type == "" {
		p.Type = "manual"
	}
	if p.Scope == "" {
		p.Scope = "project"
	}
	p.Project = normalizeProject(p.Project)

	syncID := newObsSyncID()
	now := time.Now().UTC()

	var topicKey *string
	if p.TopicKey != "" {
		tk := p.TopicKey
		topicKey = &tk
	}

	// Version progression for topic-keyed upserts: a re-save to the same topic must
	// deterministically win the LWW tiebreaker (updated_at, version, writer_id,
	// mutation_id). On a coarse wall clock two rapid saves can share the same
	// UpdatedAt; without a higher version the winner would fall to the arbitrary
	// content-addressed mutation_id. We read the current version for the topic and
	// write existing+1. (Single-writer daemon: the only concurrent writer is the
	// autosync pull; a rare interleave is acceptable until write-queue serialization.)
	version := 1
	if topicKey != nil {
		if rec, ferr := s.FindByTopic(*topicKey, p.Project, p.Scope); ferr == nil && rec != nil {
			version = rec.Version + 1
		}
	}

	m := domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  p.SessionID,
		EntityType: domain.EntityMemory,
		Type:       p.Type,
		Title:      p.Title,
		Content:    p.Content,
		Project:    p.Project,
		Scope:      p.Scope,
		TopicKey:   topicKey,
		Version:    version,
		UpdatedAt:  now,
		WriterID:   p.WriterID,
	}

	written, err := s.LocalWrite(m)
	if err != nil {
		return ObservationResult{}, fmt.Errorf("AddObservation: LocalWrite: %w", err)
	}

	// Resolve the integer PK by looking up the row's sync_id. When Decide
	// produced ActionUpdate (topic_key upsert), the stored row may carry a
	// different sync_id (TargetSyncID); the outbox row still carries written.SyncID
	// (the incoming mutation's SyncID). We resolve the integer PK through the
	// memories row that is now live for the given (topic_key, project, scope) when
	// a topic_key was supplied, or by written.SyncID otherwise.
	var id int64
	var resolvedSyncID string

	if topicKey != nil {
		// Topic-keyed upsert: find the live row for this (topic_key, project, scope).
		rec, err := s.FindByTopic(*topicKey, p.Project, p.Scope)
		if err != nil {
			return ObservationResult{}, fmt.Errorf("AddObservation: FindByTopic: %w", err)
		}
		if rec != nil {
			resolvedSyncID = rec.SyncID
		}
	}
	if resolvedSyncID == "" {
		resolvedSyncID = written.SyncID
	}

	err = s.db.QueryRow(
		`SELECT id FROM memories WHERE sync_id = ? AND deleted_at IS NULL`,
		resolvedSyncID,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return ObservationResult{}, ErrObservationNotFound
	}
	if err != nil {
		return ObservationResult{}, fmt.Errorf("AddObservation: resolve id: %w", err)
	}

	return ObservationResult{ID: id, SyncID: resolvedSyncID}, nil
}

// GetObservation fetches the full live (non-deleted) memory row for the given
// integer primary key. Returns ErrObservationNotFound when no live row exists.
func (s *Store) GetObservation(id int64) (*domain.Record, error) {
	const query = `
		SELECT sync_id, session_id, entity_type, type, title, content,
		       project, scope, version, writer_id, last_write_mutation_id,
		       topic_key, status, parent_sync_id,
		       created_at, updated_at, deleted_at
		FROM memories
		WHERE id = ? AND deleted_at IS NULL
		LIMIT 1`
	rec, err := scanRecord(s.db.QueryRow(query, id))
	if err != nil {
		return nil, fmt.Errorf("GetObservation(%d): %w", id, err)
	}
	if rec == nil {
		return nil, ErrObservationNotFound
	}
	rec.ID = id
	return rec, nil
}

// newObsSyncID generates a random sync_id for a new observation, using the
// same format as old_code: "obs-<16 hex chars>".
func newObsSyncID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback: timestamp-based (practically unreachable on any modern OS).
		return fmt.Sprintf("obs-%d", time.Now().UTC().UnixNano())
	}
	return "obs-" + hex.EncodeToString(b)
}
