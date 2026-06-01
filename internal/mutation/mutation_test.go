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

// TestNewMutationID_DifferentStatus verifies that two mutations identical in
// every other field but differing ONLY in Status produce different IDs.
// With the pre-fix canonicalFields (Status absent) they collide — this confirms
// the P2 bug and proves the fix.
func TestNewMutationID_DifferentStatus(t *testing.T) {
	done := "done"
	m1 := baseM()
	// m1 has nil Status (zero value from baseM)
	m2 := baseM()
	m2.Status = &done

	id1 := NewMutationID(CanonicalPayload(m1))
	id2 := NewMutationID(CanonicalPayload(m2))

	if id1 == id2 {
		t.Error("mutations differing only in Status must produce different IDs; got identical IDs (P2 bug: Status missing from canonicalFields)")
	}
}

// TestNewMutationID_DifferentParentSyncID verifies that two mutations identical
// in every other field but differing ONLY in ParentSyncID produce different IDs.
// With the pre-fix canonicalFields (ParentSyncID absent) they collide.
func TestNewMutationID_DifferentParentSyncID(t *testing.T) {
	parent := "parent-sync-abc"
	m1 := baseM()
	// m1 has nil ParentSyncID
	m2 := baseM()
	m2.ParentSyncID = &parent

	id1 := NewMutationID(CanonicalPayload(m1))
	id2 := NewMutationID(CanonicalPayload(m2))

	if id1 == id2 {
		t.Error("mutations differing only in ParentSyncID must produce different IDs; got identical IDs (P2 bug: ParentSyncID missing from canonicalFields)")
	}
}
