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

// TestNewMutationID_NilVsEmptyStringStatus proves that nil Status and &"" Status
// hash to DIFFERENT IDs — they are persisted differently (NULL vs "") and the
// content-addressed ID must reflect that distinction.
//
// With `Status string json:"status,omitempty"` in canonicalFields both nil and &""
// dereference to "" which omitempty drops entirely — identical hash (Codex bug A).
// After the fix (`Status *string json:"status"` without omitempty) nil marshals to
// JSON null and &"" marshals to "" — different hashes.
func TestNewMutationID_NilVsEmptyStringStatus(t *testing.T) {
	emptyStr := ""
	m1 := baseM() // Status == nil
	m2 := baseM()
	m2.Status = &emptyStr // Status == &""

	id1 := NewMutationID(CanonicalPayload(m1))
	id2 := NewMutationID(CanonicalPayload(m2))

	if id1 == id2 {
		t.Error("nil Status and &\"\" Status must produce different IDs; omitempty conflates them (Codex bug A)")
	}
}

// TestNewMutationID_NilVsEmptyStringParentSyncID proves that nil ParentSyncID
// and &"" ParentSyncID hash to DIFFERENT IDs.
// "" passes the non-null hierarchy CHECK; NULL fails it — they are semantically
// and structurally distinct, so the hash must distinguish them.
//
// Same root cause as TestNewMutationID_NilVsEmptyStringStatus: omitempty on a
// string field conflates nil-pointer-deref ("") with explicit empty string ("").
func TestNewMutationID_NilVsEmptyStringParentSyncID(t *testing.T) {
	emptyStr := ""
	m1 := baseM() // ParentSyncID == nil
	m2 := baseM()
	m2.ParentSyncID = &emptyStr // ParentSyncID == &""

	id1 := NewMutationID(CanonicalPayload(m1))
	id2 := NewMutationID(CanonicalPayload(m2))

	if id1 == id2 {
		t.Error("nil ParentSyncID and &\"\" ParentSyncID must produce different IDs; omitempty conflates them (Codex bug A)")
	}
}

// TestNewMutationID_NilVsEmptyStringTopicKey proves that nil TopicKey and &""
// TopicKey hash to DIFFERENT IDs — same omitempty root cause, same fix applied
// for consistency across all three nullable fields.
func TestNewMutationID_NilVsEmptyStringTopicKey(t *testing.T) {
	emptyStr := ""
	m1 := baseM() // TopicKey == nil
	m2 := baseM()
	m2.TopicKey = &emptyStr // TopicKey == &""

	id1 := NewMutationID(CanonicalPayload(m1))
	id2 := NewMutationID(CanonicalPayload(m2))

	if id1 == id2 {
		t.Error("nil TopicKey and &\"\" TopicKey must produce different IDs; omitempty conflates them (Codex bug A)")
	}
}
