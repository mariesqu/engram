package localstore

import "testing"

// TestMergeProject_MovesMemories verifies the source project's memories are
// renamed to the target and the count is reported.
func TestMergeProject_MovesMemories(t *testing.T) {
	s := openTempStore(t)
	addTestObservation(t, s, "m1", "c1", "myapp")
	addTestObservation(t, s, "m2", "c2", "myapp")
	addTestObservation(t, s, "other", "c3", "unrelated")

	mem, _, _, err := s.MergeProject("myapp", "my-app")
	if err != nil {
		t.Fatalf("MergeProject: %v", err)
	}
	if mem != 2 {
		t.Errorf("moved memories = %d, want 2", mem)
	}

	// Source has no live rows; target has 2.
	if n, _ := s.CountLiveByProject("myapp"); n != 0 {
		t.Errorf("source live count = %d, want 0", n)
	}
	if n, _ := s.CountLiveByProject("my-app"); n != 2 {
		t.Errorf("target live count = %d, want 2", n)
	}
	// Untouched project survives.
	if n, _ := s.CountLiveByProject("unrelated"); n != 1 {
		t.Errorf("unrelated count = %d, want 1", n)
	}
}

// TestMergeProject_TopicCollision_TargetWins verifies that when source and target
// each hold a LIVE memory under the SAME (topic_key, scope), the merge soft-deletes
// the SOURCE row (target wins) so only ONE live row remains for that topic — the
// one-live-row topic invariant is preserved (no silent duplicate).
func TestMergeProject_TopicCollision_TargetWins(t *testing.T) {
	s := openTempStore(t)
	// Target already has a live memory under topic_key "arch/auth".
	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "s", Type: "decision", Title: "target auth", Content: "target",
		Project: "my-app", Scope: "project", TopicKey: "arch/auth", WriterID: "w",
	}); err != nil {
		t.Fatalf("target AddObservation: %v", err)
	}
	// Source has its OWN live memory under the same topic_key.
	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "s", Type: "decision", Title: "source auth", Content: "source",
		Project: "myapp", Scope: "project", TopicKey: "arch/auth", WriterID: "w",
	}); err != nil {
		t.Fatalf("source AddObservation: %v", err)
	}

	if _, _, _, err := s.MergeProject("myapp", "my-app"); err != nil {
		t.Fatalf("MergeProject: %v", err)
	}

	// Exactly ONE live row for (arch/auth, my-app, project) — the target's.
	id, err := s.IDByTopicKey("arch/auth", "my-app", "project")
	if err != nil {
		t.Fatalf("IDByTopicKey after merge: %v", err)
	}
	rec, err := s.GetObservation(id)
	if err != nil {
		t.Fatalf("GetObservation: %v", err)
	}
	if rec.Title != "target auth" {
		t.Errorf("surviving title = %q, want %q (target wins)", rec.Title, "target auth")
	}
	if n, _ := s.CountLiveByProject("myapp"); n != 0 {
		t.Errorf("source live count = %d, want 0", n)
	}
	if n, _ := s.CountLiveByProject("my-app"); n != 1 {
		t.Errorf("target live count = %d, want 1 (no duplicate)", n)
	}
}

// TestMergeProject_DedupsPolicyAndCursor verifies that when BOTH source and
// target already have a policy row (and a pull cursor), the target row wins and
// the source row is removed — no UNIQUE/PK conflict.
func TestMergeProject_DedupsPolicyAndCursor(t *testing.T) {
	s := openTempStore(t)
	addTestObservation(t, s, "m1", "c1", "myapp")

	// Explicit policy rows for both names.
	if err := s.SetPolicy("myapp", PolicyLocalOnly); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPolicy("my-app", PolicySynced); err != nil {
		t.Fatal(err)
	}
	// Pull cursors for both names under the default central target_key.
	if err := s.SetPullCursorFor("myapp", 5); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPullCursorFor("my-app", 9); err != nil {
		t.Fatal(err)
	}

	_, pol, cur, err := s.MergeProject("myapp", "my-app")
	if err != nil {
		t.Fatalf("MergeProject: %v", err)
	}
	if pol != 1 {
		t.Errorf("policy rows moved = %d, want 1 (source deduped)", pol)
	}
	if cur != 1 {
		t.Errorf("cursor rows moved = %d, want 1 (source deduped)", cur)
	}

	// Target policy must be the ORIGINAL target value (synced), not the source's.
	got, err := s.GetPolicy("my-app")
	if err != nil {
		t.Fatal(err)
	}
	if got != PolicySynced {
		t.Errorf("target policy = %q, want synced (target wins)", got)
	}

	// Target cursor must be the original target value (9), not the source's (5).
	seq, err := s.PullCursorFor("my-app")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 9 {
		t.Errorf("target cursor = %d, want 9 (target wins)", seq)
	}
}

// TestMergeProject_RenamesPolicyAndCursorWhenNoTargetRow verifies that when the
// target has NO policy/cursor row, the source rows are renamed in place.
func TestMergeProject_RenamesPolicyAndCursorWhenNoTargetRow(t *testing.T) {
	s := openTempStore(t)
	addTestObservation(t, s, "m1", "c1", "myapp")
	if err := s.SetPolicy("myapp", PolicyLocalOnly); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPullCursorFor("myapp", 7); err != nil {
		t.Fatal(err)
	}

	_, pol, cur, err := s.MergeProject("myapp", "my-app")
	if err != nil {
		t.Fatalf("MergeProject: %v", err)
	}
	if pol != 1 || cur != 1 {
		t.Errorf("moved policy=%d cursor=%d, want 1/1 (renamed)", pol, cur)
	}

	got, _ := s.GetPolicy("my-app")
	if got != PolicyLocalOnly {
		t.Errorf("renamed policy = %q, want local-only", got)
	}
	seq, _ := s.PullCursorFor("my-app")
	if seq != 7 {
		t.Errorf("renamed cursor = %d, want 7", seq)
	}
}

// TestMergeProject_RejectsBadArgs verifies empty/identical from/to are rejected.
func TestMergeProject_RejectsBadArgs(t *testing.T) {
	s := openTempStore(t)
	if _, _, _, err := s.MergeProject("", "x"); err == nil {
		t.Error("empty from should error")
	}
	if _, _, _, err := s.MergeProject("x", ""); err == nil {
		t.Error("empty to should error")
	}
	// from == to after normalization (case difference) → reject.
	if _, _, _, err := s.MergeProject("MyApp", "myapp"); err == nil {
		t.Error("from == to (after normalization) should error")
	}
}
