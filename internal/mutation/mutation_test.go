package mutation

import (
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

func baseM() domain.Mutation {
	ts := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	return domain.Mutation{
		Op:        domain.OpUpsert,
		SyncID:    "sync-abc",
		SessionID: "sess-1",
		EntityType: domain.EntityMemory,
		Type:      "manual",
		Title:     "Test memory",
		Content:   "Some content",
		Project:   "eng",
		Scope:     "project",
		Version:   1,
		Seq:       0,
		UpdatedAt: ts,
		OccurredAt: ts,
		WriterID:  "writer-A",
	}
}

func TestNewMutationID_Deterministic(t *testing.T) {
	m := baseM()
	p1 := CanonicalPayload(m)
	p2 := CanonicalPayload(m)

	id1 := NewMutationID(p1)
	id2 := NewMutationID(p2)

	if id1 != id2 {
		t.Errorf("same inputs must produce same ID: got %q vs %q", id1, id2)
	}
}

func TestNewMutationID_DifferentContent(t *testing.T) {
	m1 := baseM()
	m2 := baseM()
	m2.Content = "Different content"

	p1 := CanonicalPayload(m1)
	p2 := CanonicalPayload(m2)

	if NewMutationID(p1) == NewMutationID(p2) {
		t.Error("different content must produce different IDs")
	}
}

func TestNewMutationID_NotEmpty(t *testing.T) {
	m := baseM()
	p := CanonicalPayload(m)
	id := NewMutationID(p)
	if id == "" {
		t.Error("mutation ID must not be empty")
	}
}
