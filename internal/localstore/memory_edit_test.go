package localstore

import (
	"testing"

	"github.com/mariesqu/engram/internal/domain"
)

// addTestObservation is a test helper that inserts a memory row and returns
// the ObservationResult, fataling the test on error.
func addTestObservation(t *testing.T, s *Store, title, content, project string) ObservationResult {
	t.Helper()
	res, err := s.AddObservation(AddObservationParams{
		Type:    "manual",
		Title:   title,
		Content: content,
		Project: project,
		Scope:   "project",
	})
	if err != nil {
		t.Fatalf("AddObservation(%q): %v", title, err)
	}
	return res
}

// TestUpdateMemory_RoundTrip verifies that UpdateMemory modifies the title,
// content, and optionally type, and returns the updated record.
func TestUpdateMemory_RoundTrip(t *testing.T) {
	s := openTempStore(t)

	res := addTestObservation(t, s, "old title", "old content", "testproj")

	rec, err := s.UpdateMemory(res.ID, "new title", "new content", "", "writer1")
	if err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	if rec == nil {
		t.Fatal("UpdateMemory returned nil record")
	}
	if rec.Title != "new title" {
		t.Errorf("Title = %q, want %q", rec.Title, "new title")
	}
	if rec.Content != "new content" {
		t.Errorf("Content = %q, want %q", rec.Content, "new content")
	}
	// Type should be preserved ("manual") since we passed "".
	if rec.Type != "manual" {
		t.Errorf("Type = %q, want %q", rec.Type, "manual")
	}
}

// TestUpdateMemory_TypeOverride verifies that when a non-empty typ is supplied,
// the record's type is updated to that value.
func TestUpdateMemory_TypeOverride(t *testing.T) {
	s := openTempStore(t)

	res := addTestObservation(t, s, "title", "content", "proj")

	rec, err := s.UpdateMemory(res.ID, "title", "content", "decision", "writer1")
	if err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	if rec.Type != "decision" {
		t.Errorf("Type = %q, want %q", rec.Type, "decision")
	}
}

// TestUpdateMemory_OutboxEnqueued verifies that UpdateMemory enqueues an upsert
// mutation in the outbox for push to central.
func TestUpdateMemory_OutboxEnqueued(t *testing.T) {
	s := openTempStore(t)

	res := addTestObservation(t, s, "title", "content", "proj")

	// Record the pending count before the update.
	before, err := s.PendingCount()
	if err != nil {
		t.Fatalf("PendingCount before: %v", err)
	}

	if _, err := s.UpdateMemory(res.ID, "updated", "updated content", "", "writer1"); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}

	after, err := s.PendingCount()
	if err != nil {
		t.Fatalf("PendingCount after: %v", err)
	}
	if after <= before {
		t.Errorf("PendingCount did not increase after UpdateMemory: before=%d after=%d", before, after)
	}

	// Also verify the outbox has an OpUpsert entry for this SyncID.
	entries, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Mutation.Op == domain.OpUpsert && e.Mutation.SyncID == res.SyncID {
			found = true
		}
	}
	if !found {
		t.Errorf("expected OpUpsert in outbox for SyncID %q; total entries: %d", res.SyncID, len(entries))
	}
}

// TestUpdateMemory_NotFound verifies that UpdateMemory returns ErrObservationNotFound
// for a non-existent id.
func TestUpdateMemory_NotFound(t *testing.T) {
	s := openTempStore(t)

	_, err := s.UpdateMemory(999999, "title", "content", "", "writer1")
	if err == nil {
		t.Fatal("expected error for non-existent id, got nil")
	}
	// The error must wrap ErrObservationNotFound.
	if err.Error() == "" {
		t.Error("expected non-empty error message")
	}
}

// TestDeleteMemory_SoftDeletes verifies that DeleteMemory marks the row deleted_at
// so GetObservation returns ErrObservationNotFound afterwards.
func TestDeleteMemory_SoftDeletes(t *testing.T) {
	s := openTempStore(t)

	res := addTestObservation(t, s, "to delete", "content", "proj")

	// Confirm it is live before deletion.
	rec, err := s.GetObservation(res.ID)
	if err != nil {
		t.Fatalf("GetObservation before delete: %v", err)
	}
	if rec == nil {
		t.Fatal("record should be live before delete")
	}

	if err := s.DeleteMemory(res.ID, "writer1"); err != nil {
		t.Fatalf("DeleteMemory: %v", err)
	}

	// After deletion GetObservation must return ErrObservationNotFound.
	_, err = s.GetObservation(res.ID)
	if err == nil {
		t.Fatal("expected ErrObservationNotFound after delete, got nil")
	}
}

// TestDeleteMemory_OutboxEnqueued verifies that DeleteMemory enqueues an OpDelete
// mutation in the outbox.
func TestDeleteMemory_OutboxEnqueued(t *testing.T) {
	s := openTempStore(t)

	res := addTestObservation(t, s, "to delete", "content", "proj")

	before, err := s.PendingCount()
	if err != nil {
		t.Fatalf("PendingCount before: %v", err)
	}

	if err := s.DeleteMemory(res.ID, "writer1"); err != nil {
		t.Fatalf("DeleteMemory: %v", err)
	}

	after, err := s.PendingCount()
	if err != nil {
		t.Fatalf("PendingCount after: %v", err)
	}
	if after <= before {
		t.Errorf("PendingCount did not increase after DeleteMemory: before=%d after=%d", before, after)
	}

	// Verify the outbox contains an OpDelete entry for this SyncID.
	entries, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Mutation.Op == domain.OpDelete && e.Mutation.SyncID == res.SyncID {
			found = true
		}
	}
	if !found {
		t.Errorf("expected OpDelete in outbox for SyncID %q; total entries: %d", res.SyncID, len(entries))
	}
}

// TestDeleteMemory_NotFound verifies that DeleteMemory returns an error when
// the id does not exist.
func TestDeleteMemory_NotFound(t *testing.T) {
	s := openTempStore(t)

	err := s.DeleteMemory(999999, "writer1")
	if err == nil {
		t.Fatal("expected error for non-existent id, got nil")
	}
}

// TestDeleteMemory_AlreadyDeleted verifies that calling DeleteMemory on an
// already-deleted row returns ErrObservationNotFound (not a silent no-op).
func TestDeleteMemory_AlreadyDeleted(t *testing.T) {
	s := openTempStore(t)

	res := addTestObservation(t, s, "delete twice", "content", "proj")

	if err := s.DeleteMemory(res.ID, "writer1"); err != nil {
		t.Fatalf("first DeleteMemory: %v", err)
	}
	// Second delete must return an error.
	err := s.DeleteMemory(res.ID, "writer1")
	if err == nil {
		t.Fatal("expected error on second delete, got nil")
	}
}
