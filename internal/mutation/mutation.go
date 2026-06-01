// Package mutation provides content-addressed mutation ID derivation for engram.
// IDs are SHA-256 hashes of the canonical JSON payload, making them
// deterministic and collision-resistant across writers and sessions.
package mutation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/mariesqu/engram/internal/domain"
)

// canonicalFields is the stable, ordered subset of Mutation fields that
// participates in the content-addressed ID.
//
// Nullable fields (TopicKey, Status, ParentSyncID) use *string WITHOUT omitempty
// so that nil marshals to JSON null and &"" marshals to "" — distinct hashes.
// Using plain string + omitempty would conflate nil and &"" (both yield "" which
// omitempty drops), making transitions between SQL NULL and empty-string invisible
// to the applied_mutations idempotency guard (INV5).  This matters especially for
// ParentSyncID: "" passes the non-null hierarchy CHECK, NULL fails it.
//
// Every field that is persisted to the memories table and that can change
// independently of version/updated_at MUST appear here — otherwise two
// mutations differing only in that field hash to the same ID and the second
// one is incorrectly skipped by the applied_mutations idempotency guard (INV5).
type canonicalFields struct {
	Op           string  `json:"op"`
	SyncID       string  `json:"sync_id"`
	SessionID    string  `json:"session_id"`
	EntityType   string  `json:"entity_type"`
	Type         string  `json:"type"`
	Title        string  `json:"title"`
	Content      string  `json:"content"`
	Project      string  `json:"project"`
	Scope        string  `json:"scope"`
	TopicKey     *string `json:"topic_key"`
	Status       *string `json:"status"`
	ParentSyncID *string `json:"parent_sync_id"`
	Version      int     `json:"version"`
	UpdatedAt    string  `json:"updated_at"` // RFC3339Nano for precision
	WriterID     string  `json:"writer_id"`
}

// CanonicalPayload returns the deterministic JSON encoding of the fields that
// define mutation identity. The same logical write always produces the same
// payload regardless of call order or go-map iteration.
//
// Nullable pointer fields (TopicKey, Status, ParentSyncID) are assigned directly
// so the JSON encoding preserves the nil-vs-non-nil distinction:
//   - nil  → JSON null  (SQL NULL in the database)
//   - &""  → JSON ""    (empty string in the database)
// This is critical for correctness: omitting the field via omitempty would make
// these two states hash-identical, causing the idempotency guard (INV5) to skip
// a mutation that transitions a column between NULL and "".
func CanonicalPayload(m domain.Mutation) []byte {
	cf := canonicalFields{
		Op:           string(m.Op),
		SyncID:       m.SyncID,
		SessionID:    m.SessionID,
		EntityType:   string(m.EntityType),
		Type:         m.Type,
		Title:        m.Title,
		Content:      m.Content,
		Project:      m.Project,
		Scope:        m.Scope,
		TopicKey:     m.TopicKey,
		Status:       m.Status,
		ParentSyncID: m.ParentSyncID,
		Version:      m.Version,
		UpdatedAt:    m.UpdatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z"),
		WriterID:     m.WriterID,
	}

	b, err := json.Marshal(cf)
	if err != nil {
		// canonicalFields contains only string/int/pointer fields — this should never fail.
		panic(fmt.Sprintf("mutation: CanonicalPayload marshal failed: %v", err))
	}
	return b
}

// NewMutationID returns the SHA-256 hex string of the given canonical payload.
// Calling CanonicalPayload then NewMutationID on the same logical mutation
// always yields the same ID.
func NewMutationID(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
