package localstore

import "testing"

// TestAddObservation_CarriesWriterID verifies the writer id flows into the
// enqueued outbox mutation. Without it, the central server's per-writer HMAC
// forgery check (mutation.writer_id must equal the authenticated writer) 403s
// every push in central mode.
func TestAddObservation_CarriesWriterID(t *testing.T) {
	s := openTempStore(t)

	if _, err := s.AddObservation(AddObservationParams{
		Title: "t", Content: "c", Project: "proj", WriterID: "writer-x",
	}); err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	entries, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("outbox has %d entries, want 1", len(entries))
	}
	if got := entries[0].Mutation.WriterID; got != "writer-x" {
		t.Errorf("outbox mutation WriterID = %q, want %q (central push forgery check requires it)", got, "writer-x")
	}
}

// TestAddObservation_VersionProgresses verifies that a re-save to the same topic
// deterministically wins the LWW tiebreaker via version progression — even when
// the two writes share an UpdatedAt (coarse wall clock). Without progression the
// winner would fall to the arbitrary content-addressed mutation_id.
func TestAddObservation_VersionProgresses(t *testing.T) {
	s := openTempStore(t)
	const topic = "arch/decision"

	if _, err := s.AddObservation(AddObservationParams{Title: "t", Content: "v1", Project: "proj", TopicKey: topic}); err != nil {
		t.Fatalf("AddObservation v1: %v", err)
	}
	r2, err := s.AddObservation(AddObservationParams{Title: "t", Content: "v2", Project: "proj", TopicKey: topic})
	if err != nil {
		t.Fatalf("AddObservation v2: %v", err)
	}

	rec, err := s.GetObservation(r2.ID)
	if err != nil {
		t.Fatalf("GetObservation: %v", err)
	}
	if rec.Content != "v2" {
		t.Errorf("after re-save, content = %q, want %q (version progression must make the re-save win)", rec.Content, "v2")
	}
	if rec.Version < 2 {
		t.Errorf("version = %d, want >= 2 after a topic re-save", rec.Version)
	}
}
