package localstore

import (
	"testing"
)

// seedReviewable inserts a live memory row with explicit updated_at / review_after
// / expires_at so review-status transitions can be exercised deterministically.
// updatedAt/reviewAfter/expiresAt are SQLite datetime strings (e.g.
// datetime('now','-40 days')); pass "" to leave review_after / expires_at NULL.
func seedReviewable(t *testing.T, s *Store, syncID, title, project, updatedExpr, reviewAfterExpr, expiresExpr string) int64 {
	t.Helper()
	// Build the column list dynamically so NULL columns stay NULL.
	q := `INSERT INTO memories
		(sync_id, session_id, entity_type, type, title, content, project, scope, writer_id,
		 created_at, updated_at`
	vals := `VALUES (?, 'sess', 'memory', 'decision', ?, 'body', ?, 'project', 'w1',
		` + updatedExpr + `, ` + updatedExpr
	if reviewAfterExpr != "" {
		q += `, review_after`
		vals += `, ` + reviewAfterExpr
	}
	if expiresExpr != "" {
		q += `, expires_at`
		vals += `, ` + expiresExpr
	}
	q += `) ` + vals + `)`

	res, err := s.db.Exec(q, syncID, title, project)
	if err != nil {
		t.Fatalf("seedReviewable(%q): %v", syncID, err)
	}
	id, _ := res.LastInsertId()
	return id
}

// TestReviewStatus_WindowBoundary verifies the active → needs_review transition
// across the default 30-day window: a fresh row is active, an old row crosses to
// needs_review once updated_at + window is in the past.
func TestReviewStatus_WindowBoundary(t *testing.T) {
	s := openTempStore(t) // default window = 30

	freshID := seedReviewable(t, s, "rev-fresh", "fresh", "p", "datetime('now','-1 days')", "", "")
	staleID := seedReviewable(t, s, "rev-stale", "stale", "p", "datetime('now','-40 days')", "", "")

	rows, err := s.ListForReview("all", "p", 50)
	if err != nil {
		t.Fatalf("ListForReview: %v", err)
	}
	got := map[int64]string{}
	for _, r := range rows {
		got[r.ID] = r.Status
	}
	if got[freshID] != ReviewStatusActive {
		t.Errorf("fresh row status = %q, want active", got[freshID])
	}
	if got[staleID] != ReviewStatusNeedsReview {
		t.Errorf("stale row status = %q, want needs_review", got[staleID])
	}
}

// TestReviewStatus_CustomWindow verifies SetReviewWindowDays changes the boundary:
// a 10-day-old row is active under a 30-day window but needs_review under a 7-day
// window.
func TestReviewStatus_CustomWindow(t *testing.T) {
	s := openTempStore(t)
	id := seedReviewable(t, s, "rev-cw", "cw", "p", "datetime('now','-10 days')", "", "")

	// Default 30-day window: active.
	rows, _ := s.ListForReview("all", "p", 50)
	if rows[0].Status != ReviewStatusActive {
		t.Fatalf("with 30d window: status = %q, want active", rows[0].Status)
	}
	_ = id

	// Shrink window to 7 days: the same row is now needs_review.
	s.SetReviewWindowDays(7)
	rows, _ = s.ListForReview("all", "p", 50)
	if rows[0].Status != ReviewStatusNeedsReview {
		t.Fatalf("with 7d window: status = %q, want needs_review", rows[0].Status)
	}
}

// TestReviewStatus_Expired verifies the expired status takes precedence: a row
// with expires_at in the past is expired regardless of review_after.
func TestReviewStatus_Expired(t *testing.T) {
	s := openTempStore(t)
	seedReviewable(t, s, "rev-exp", "exp", "p",
		"datetime('now','-1 days')",  // updated recently (would be active)
		"datetime('now','+30 days')", // review_after in the future
		"datetime('now','-1 days')")  // but expired yesterday

	rows, err := s.ListForReview("all", "p", 50)
	if err != nil {
		t.Fatalf("ListForReview: %v", err)
	}
	if rows[0].Status != ReviewStatusExpired {
		t.Errorf("status = %q, want expired", rows[0].Status)
	}
}

// TestListForReview_StatusFilter verifies the status filter selects the right
// subset and that "needs_review" is the implicit default.
func TestListForReview_StatusFilter(t *testing.T) {
	s := openTempStore(t)
	seedReviewable(t, s, "f-active", "active one", "p", "datetime('now','-1 days')", "", "")
	seedReviewable(t, s, "f-stale", "stale one", "p", "datetime('now','-40 days')", "", "")
	seedReviewable(t, s, "f-exp", "expired one", "p", "datetime('now','-1 days')", "", "datetime('now','-1 days')")

	// Default (empty status) → needs_review only.
	rows, err := s.ListForReview("", "p", 50)
	if err != nil {
		t.Fatalf("ListForReview default: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != ReviewStatusNeedsReview {
		t.Fatalf("default filter: got %d rows %+v, want 1 needs_review", len(rows), rows)
	}

	// active filter → 1.
	rows, _ = s.ListForReview("active", "p", 50)
	if len(rows) != 1 || rows[0].Title != "active one" {
		t.Errorf("active filter: got %+v, want [active one]", rows)
	}

	// expired filter → 1.
	rows, _ = s.ListForReview("expired", "p", 50)
	if len(rows) != 1 || rows[0].Title != "expired one" {
		t.Errorf("expired filter: got %+v, want [expired one]", rows)
	}

	// all → 3.
	rows, _ = s.ListForReview("all", "p", 50)
	if len(rows) != 3 {
		t.Errorf("all filter: got %d rows, want 3", len(rows))
	}
}

// TestListForReview_InvalidStatus verifies an unknown status is rejected.
func TestListForReview_InvalidStatus(t *testing.T) {
	s := openTempStore(t)
	if _, err := s.ListForReview("bogus", "", 50); err == nil {
		t.Fatal("expected error for invalid status, got nil")
	}
}

// TestMarkReviewed_ResetsClock verifies mark_reviewed stamps review_after into the
// future so a stale row becomes active again.
func TestMarkReviewed_ResetsClock(t *testing.T) {
	s := openTempStore(t)
	id := seedReviewable(t, s, "mr-1", "stale", "p", "datetime('now','-40 days')", "", "")

	// Pre: needs_review.
	rows, _ := s.ListForReview("all", "p", 50)
	if rows[0].Status != ReviewStatusNeedsReview {
		t.Fatalf("pre-mark status = %q, want needs_review", rows[0].Status)
	}

	n, err := s.MarkReviewed([]int64{id})
	if err != nil {
		t.Fatalf("MarkReviewed: %v", err)
	}
	if n != 1 {
		t.Fatalf("MarkReviewed rows = %d, want 1", n)
	}

	// Post: active (review_after now in the future) with review_after set.
	rows, _ = s.ListForReview("all", "p", 50)
	if rows[0].Status != ReviewStatusActive {
		t.Errorf("post-mark status = %q, want active", rows[0].Status)
	}
	if rows[0].ReviewAfter == nil {
		t.Error("review_after should be set after MarkReviewed")
	}
}

// TestMarkReviewed_SkipsDeletedAndUnknown verifies deleted/unknown ids do not
// inflate the affected-row count.
func TestMarkReviewed_SkipsDeletedAndUnknown(t *testing.T) {
	s := openTempStore(t)
	liveID := seedReviewable(t, s, "mr-live", "live", "p", "datetime('now','-40 days')", "", "")
	delID := seedReviewable(t, s, "mr-del", "deleted", "p", "datetime('now','-40 days')", "", "")
	if _, err := s.db.Exec(`UPDATE memories SET deleted_at = datetime('now') WHERE id = ?`, delID); err != nil {
		t.Fatal(err)
	}

	n, err := s.MarkReviewed([]int64{liveID, delID, 999999})
	if err != nil {
		t.Fatalf("MarkReviewed: %v", err)
	}
	if n != 1 {
		t.Errorf("affected = %d, want 1 (only the live row)", n)
	}
}

// TestMarkReviewed_Empty verifies an empty id slice is a no-op.
func TestMarkReviewed_Empty(t *testing.T) {
	s := openTempStore(t)
	n, err := s.MarkReviewed(nil)
	if err != nil || n != 0 {
		t.Fatalf("MarkReviewed(nil) = (%d, %v), want (0, nil)", n, err)
	}
}

// TestReviewStatusForID verifies the per-id status helper used by mem_get_observation.
func TestReviewStatusForID(t *testing.T) {
	s := openTempStore(t)
	id := seedReviewable(t, s, "rsid", "x", "p", "datetime('now','-40 days')", "", "")

	st, err := s.ReviewStatusForID(id)
	if err != nil {
		t.Fatalf("ReviewStatusForID: %v", err)
	}
	if st != ReviewStatusNeedsReview {
		t.Errorf("status = %q, want needs_review", st)
	}

	// Missing id → ("", nil).
	st, err = s.ReviewStatusForID(987654)
	if err != nil || st != "" {
		t.Errorf("ReviewStatusForID(missing) = (%q, %v), want (\"\", nil)", st, err)
	}
}

// TestIDByTopicKey verifies topic_key → id resolution for mark_reviewed.
func TestIDByTopicKey(t *testing.T) {
	s := openTempStore(t)
	res, err := s.AddObservation(AddObservationParams{
		Type: "decision", Title: "t", Content: "c",
		Project: "p", Scope: "project", TopicKey: "arch/auth",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	id, err := s.IDByTopicKey("arch/auth", "p", "project")
	if err != nil {
		t.Fatalf("IDByTopicKey: %v", err)
	}
	if id != res.ID {
		t.Errorf("id = %d, want %d", id, res.ID)
	}

	if _, err := s.IDByTopicKey("nope/missing", "p", "project"); err == nil {
		t.Error("expected ErrObservationNotFound for unknown topic_key")
	}
}

// TestDistinctProjects verifies the helper returns only non-empty, live projects.
func TestDistinctProjects(t *testing.T) {
	s := openTempStore(t)
	seedReviewable(t, s, "dp-1", "a", "alpha", "datetime('now')", "", "")
	seedReviewable(t, s, "dp-2", "b", "beta", "datetime('now')", "", "")
	seedReviewable(t, s, "dp-3", "c", "alpha", "datetime('now')", "", "") // dup project
	// empty-project row must be excluded.
	if _, err := s.db.Exec(
		`INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		 VALUES ('dp-empty', 'sess', 'memory', 'manual', 't', 'c', '', 'project', 'w1')`,
	); err != nil {
		t.Fatal(err)
	}

	projs, err := s.DistinctProjects()
	if err != nil {
		t.Fatalf("DistinctProjects: %v", err)
	}
	set := map[string]bool{}
	for _, p := range projs {
		set[p] = true
	}
	if !set["alpha"] || !set["beta"] {
		t.Errorf("missing expected projects in %v", projs)
	}
	if set[""] {
		t.Errorf("empty project should be excluded, got %v", projs)
	}
	if len(projs) != 2 {
		t.Errorf("distinct count = %d, want 2", len(projs))
	}
}
