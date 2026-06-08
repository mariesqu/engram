package localstore

import (
	"path/filepath"
	"testing"
)

// TestAddObservation_RoundTrip verifies the core AddObservation contract:
//  1. The returned id is positive and sync_id is non-empty.
//  2. GetObservation(id) returns the saved row with the correct fields.
//  3. The outbox is non-empty after the write (LocalWrite enqueued the mutation).
func TestAddObservation_RoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Seed a session so the session_id FK is satisfied.
	if err := store.CreateSession("sess-1", "testproject", "/src"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	params := AddObservationParams{
		SessionID: "sess-1",
		Type:      "decision",
		Title:     "Use LocalWrite for all writes",
		Content:   "LocalWrite runs domain.Decide inside the tx.",
		Project:   "testproject",
		Scope:     "project",
	}

	result, err := store.AddObservation(params)
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if result.ID <= 0 {
		t.Errorf("ID = %d, want > 0", result.ID)
	}
	if result.SyncID == "" {
		t.Error("SyncID is empty")
	}

	// GetObservation must return the saved row with the correct fields.
	rec, err := store.GetObservation(result.ID)
	if err != nil {
		t.Fatalf("GetObservation: %v", err)
	}
	if rec.Title != params.Title {
		t.Errorf("Title = %q, want %q", rec.Title, params.Title)
	}
	if rec.Content != params.Content {
		t.Errorf("Content = %q, want %q", rec.Content, params.Content)
	}
	if rec.Type != params.Type {
		t.Errorf("Type = %q, want %q", rec.Type, params.Type)
	}
	if rec.Project != "testproject" {
		t.Errorf("Project = %q, want %q", rec.Project, "testproject")
	}
	if rec.Scope != "project" {
		t.Errorf("Scope = %q, want %q", rec.Scope, "project")
	}
	if rec.SyncID != result.SyncID {
		t.Errorf("SyncID mismatch: GetObservation %q vs AddObservation result %q", rec.SyncID, result.SyncID)
	}

	// The outbox must have at least one pending entry (LocalWrite enqueued).
	count, err := store.PendingCount()
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if count == 0 {
		t.Error("outbox is empty after AddObservation — LocalWrite must enqueue")
	}
}

// TestAddObservation_OutboxEnqueued verifies the outbox entry count increases
// after a write. DrainOutbox should return the enqueued mutation with a matching
// SyncID.
func TestAddObservation_OutboxEnqueued(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "obs_outbox.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	result, err := store.AddObservation(AddObservationParams{
		SessionID: "sess-outbox",
		Title:     "outbox test",
		Content:   "content",
		Project:   "proj",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	entries, err := store.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("DrainOutbox returned 0 entries after AddObservation")
	}

	// At least one entry should match the returned sync_id.
	found := false
	for _, e := range entries {
		if e.Mutation.SyncID == result.SyncID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no outbox entry with SyncID=%q; entries: %v", result.SyncID, entries)
	}
}

// TestGetObservation_NotFound verifies ErrObservationNotFound for a missing id.
func TestGetObservation_NotFound(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "notfound.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	_, err = store.GetObservation(99999)
	if err != ErrObservationNotFound {
		t.Errorf("GetObservation(99999): got %v, want ErrObservationNotFound", err)
	}
}

// TestAddObservation_TopicKeyUpsert verifies that writing twice with the same
// topic_key updates in-place rather than inserting a second row.
func TestAddObservation_TopicKeyUpsert(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "topic.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	params := AddObservationParams{
		SessionID: "sess-topic",
		Type:      "architecture",
		Title:     "auth model",
		Content:   "v1 content",
		Project:   "myproject",
		Scope:     "project",
		TopicKey:  "architecture/auth-model",
	}

	r1, err := store.AddObservation(params)
	if err != nil {
		t.Fatalf("first AddObservation: %v", err)
	}

	params.Content = "v2 content"
	r2, err := store.AddObservation(params)
	if err != nil {
		t.Fatalf("second AddObservation: %v", err)
	}

	// Both calls should resolve to the SAME row (upsert, not insert).
	if r1.ID != r2.ID {
		t.Errorf("topic upsert created a new row: r1.ID=%d r2.ID=%d", r1.ID, r2.ID)
	}

	rec, err := store.GetObservation(r2.ID)
	if err != nil {
		t.Fatalf("GetObservation: %v", err)
	}
	if rec.Content != "v2 content" {
		t.Errorf("Content = %q, want %q (upsert should update content)", rec.Content, "v2 content")
	}
}

// TestAddObservation_DefaultType verifies that an empty Type defaults to "manual".
func TestAddObservation_DefaultType(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "deftype.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	result, err := store.AddObservation(AddObservationParams{
		Title:   "no type",
		Content: "body",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	rec, err := store.GetObservation(result.ID)
	if err != nil {
		t.Fatalf("GetObservation: %v", err)
	}
	if rec.Type != "manual" {
		t.Errorf("Type = %q, want %q (default)", rec.Type, "manual")
	}
}
