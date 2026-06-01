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
// participates in the content-addressed ID. Pointer fields are dereferenced so
// nil vs absent does not create accidental uniqueness.
type canonicalFields struct {
	Op         string `json:"op"`
	SyncID     string `json:"sync_id"`
	SessionID  string `json:"session_id"`
	EntityType string `json:"entity_type"`
	Type       string `json:"type"`
	Title      string `json:"title"`
	Content    string `json:"content"`
	Project    string `json:"project"`
	Scope      string `json:"scope"`
	TopicKey   string `json:"topic_key,omitempty"`
	Version    int    `json:"version"`
	UpdatedAt  string `json:"updated_at"` // RFC3339Nano for precision
	WriterID   string `json:"writer_id"`
}

// CanonicalPayload returns the deterministic JSON encoding of the fields that
// define mutation identity. The same logical write always produces the same
// payload regardless of call order or go-map iteration.
func CanonicalPayload(m domain.Mutation) []byte {
	cf := canonicalFields{
		Op:         string(m.Op),
		SyncID:     m.SyncID,
		SessionID:  m.SessionID,
		EntityType: string(m.EntityType),
		Type:       m.Type,
		Title:      m.Title,
		Content:    m.Content,
		Project:    m.Project,
		Scope:      m.Scope,
		Version:    m.Version,
		UpdatedAt:  m.UpdatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z"),
		WriterID:   m.WriterID,
	}
	if m.TopicKey != nil {
		cf.TopicKey = *m.TopicKey
	}

	b, err := json.Marshal(cf)
	if err != nil {
		// canonicalFields contains only string/int fields — this should never fail.
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
