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

// TestFromCanonicalPayload_RoundTrip verifies that a Mutation can be encoded
// with CanonicalPayload and faithfully reconstructed by FromCanonicalPayload.
//
// The round-trip covers:
//   - All content fields present in canonicalFields (Op, SyncID, SessionID,
//     EntityType, Type, Title, Content, Project, Scope, Version, UpdatedAt,
//     WriterID)
//   - Nil nullable pointer fields (TopicKey, Status, ParentSyncID → nil → nil)
//   - Non-nil pointer fields (&"nonEmpty" → &"nonEmpty")
//   - Empty-string pointer fields (&"" → &"")
//   - UpdatedAt nanosecond precision preserved through the canonical string format
//
// Fields NOT in the canonical payload (MutationID, Seq, OccurredAt, Payload)
// are not checked here — those are the caller's responsibility to fill in.
func TestFromCanonicalPayload_RoundTrip(t *testing.T) {
	t.Run("nil nullable fields", func(t *testing.T) {
		m := baseM()
		// TopicKey, Status, ParentSyncID are nil in baseM (zero value).
		payload := CanonicalPayload(m)
		got, err := FromCanonicalPayload(payload)
		if err != nil {
			t.Fatalf("FromCanonicalPayload: %v", err)
		}
		assertContentFields(t, m, got)
		if got.TopicKey != nil {
			t.Errorf("TopicKey: got %v, want nil", got.TopicKey)
		}
		if got.Status != nil {
			t.Errorf("Status: got %v, want nil", got.Status)
		}
		if got.ParentSyncID != nil {
			t.Errorf("ParentSyncID: got %v, want nil", got.ParentSyncID)
		}
	})

	t.Run("non-nil non-empty nullable fields", func(t *testing.T) {
		tk := "sdd/test/roundtrip"
		st := "done"
		ps := "parent-sync-xyz"
		m := baseM()
		m.TopicKey = &tk
		m.Status = &st
		m.ParentSyncID = &ps
		payload := CanonicalPayload(m)
		got, err := FromCanonicalPayload(payload)
		if err != nil {
			t.Fatalf("FromCanonicalPayload: %v", err)
		}
		assertContentFields(t, m, got)
		if got.TopicKey == nil || *got.TopicKey != tk {
			t.Errorf("TopicKey: got %v, want %q", got.TopicKey, tk)
		}
		if got.Status == nil || *got.Status != st {
			t.Errorf("Status: got %v, want %q", got.Status, st)
		}
		if got.ParentSyncID == nil || *got.ParentSyncID != ps {
			t.Errorf("ParentSyncID: got %v, want %q", got.ParentSyncID, ps)
		}
	})

	t.Run("empty-string nullable fields distinct from nil", func(t *testing.T) {
		emptyStr := ""
		m := baseM()
		m.TopicKey = &emptyStr
		m.Status = &emptyStr
		m.ParentSyncID = &emptyStr
		payload := CanonicalPayload(m)
		got, err := FromCanonicalPayload(payload)
		if err != nil {
			t.Fatalf("FromCanonicalPayload: %v", err)
		}
		// Must NOT be nil — &"" round-trips as &"", not as nil.
		if got.TopicKey == nil {
			t.Error("TopicKey: got nil, want &\"\" (empty-string must not collapse to nil)")
		} else if *got.TopicKey != "" {
			t.Errorf("TopicKey: got %q, want \"\"", *got.TopicKey)
		}
		if got.Status == nil {
			t.Error("Status: got nil, want &\"\"")
		} else if *got.Status != "" {
			t.Errorf("Status: got %q, want \"\"", *got.Status)
		}
		if got.ParentSyncID == nil {
			t.Error("ParentSyncID: got nil, want &\"\"")
		} else if *got.ParentSyncID != "" {
			t.Errorf("ParentSyncID: got %q, want \"\"", *got.ParentSyncID)
		}
	})

	t.Run("nil TopicKey and &empty TopicKey round-trip distinctly", func(t *testing.T) {
		mNil := baseM()
		emptyStr := ""
		mEmpty := baseM()
		mEmpty.TopicKey = &emptyStr

		gotNil, err := FromCanonicalPayload(CanonicalPayload(mNil))
		if err != nil {
			t.Fatalf("FromCanonicalPayload nil: %v", err)
		}
		gotEmpty, err := FromCanonicalPayload(CanonicalPayload(mEmpty))
		if err != nil {
			t.Fatalf("FromCanonicalPayload empty: %v", err)
		}
		if gotNil.TopicKey != nil {
			t.Errorf("nil TopicKey round-trip: got %v, want nil", gotNil.TopicKey)
		}
		if gotEmpty.TopicKey == nil {
			t.Error("&\"\" TopicKey round-trip: got nil, want &\"\"")
		}
	})

	t.Run("UpdatedAt nanosecond precision preserved", func(t *testing.T) {
		m := baseM()
		// Use a time with nanoseconds to verify sub-second precision is preserved.
		m.UpdatedAt = time.Date(2024, 6, 15, 12, 34, 56, 789012345, time.UTC)
		payload := CanonicalPayload(m)
		got, err := FromCanonicalPayload(payload)
		if err != nil {
			t.Fatalf("FromCanonicalPayload: %v", err)
		}
		if !got.UpdatedAt.Equal(m.UpdatedAt) {
			t.Errorf("UpdatedAt: got %v, want %v", got.UpdatedAt, m.UpdatedAt)
		}
	})
}

// assertContentFields verifies that all scalar content fields present in the
// canonical payload round-trip correctly from want to got. It does NOT check
// nullable pointer fields or non-canonical fields (MutationID, Seq, etc.).
func assertContentFields(t *testing.T, want, got domain.Mutation) {
	t.Helper()
	if got.Op != want.Op {
		t.Errorf("Op: got %q, want %q", got.Op, want.Op)
	}
	if got.SyncID != want.SyncID {
		t.Errorf("SyncID: got %q, want %q", got.SyncID, want.SyncID)
	}
	if got.SessionID != want.SessionID {
		t.Errorf("SessionID: got %q, want %q", got.SessionID, want.SessionID)
	}
	if got.EntityType != want.EntityType {
		t.Errorf("EntityType: got %q, want %q", got.EntityType, want.EntityType)
	}
	if got.Type != want.Type {
		t.Errorf("Type: got %q, want %q", got.Type, want.Type)
	}
	if got.Title != want.Title {
		t.Errorf("Title: got %q, want %q", got.Title, want.Title)
	}
	if got.Content != want.Content {
		t.Errorf("Content: got %q, want %q", got.Content, want.Content)
	}
	if got.Project != want.Project {
		t.Errorf("Project: got %q, want %q", got.Project, want.Project)
	}
	if got.Scope != want.Scope {
		t.Errorf("Scope: got %q, want %q", got.Scope, want.Scope)
	}
	if got.Version != want.Version {
		t.Errorf("Version: got %d, want %d", got.Version, want.Version)
	}
	if !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("UpdatedAt: got %v, want %v", got.UpdatedAt, want.UpdatedAt)
	}
	if got.WriterID != want.WriterID {
		t.Errorf("WriterID: got %q, want %q", got.WriterID, want.WriterID)
	}
}
