package localstore

import (
	"database/sql"
	"testing"
	"time"
)

// reviewAfterOf reads the raw review_after column for a row, returning ok=false
// when it is SQL NULL — the distinction the decay map turns on.
func reviewAfterOf(t *testing.T, s *Store, id int64) (time.Time, bool) {
	t.Helper()
	var raw sql.NullString
	if err := s.DB().QueryRow(
		`SELECT review_after FROM memories WHERE id = ?`, id,
	).Scan(&raw); err != nil {
		t.Fatalf("read review_after of %d: %v", id, err)
	}
	if !raw.Valid || raw.String == "" {
		return time.Time{}, false
	}
	return parseTime(raw.String), true
}

// assertMonthsFromNow checks a due date lands within a day of now+months. The
// tolerance absorbs the calendar arithmetic and test runtime without weakening
// the assertion — a wrong map entry is off by months, not hours.
func assertMonthsFromNow(t *testing.T, label string, got time.Time, months int) {
	t.Helper()
	want := time.Now().UTC().AddDate(0, months, 0)
	if diff := got.Sub(want); diff > 24*time.Hour || diff < -24*time.Hour {
		t.Errorf("%s review_after = %s, want ≈ %s (+%d months)",
			label, got.Format(time.RFC3339), want.Format(time.RFC3339), months)
	}
}

// TestDecay_NewInsertStampsReviewAfterByType covers the map at the one place it
// is applied: a brand-new row. Types in the map get a months-out due date;
// everything else stays NULL and keeps falling back to the rolling window.
func TestDecay_NewInsertStampsReviewAfterByType(t *testing.T) {
	s := openTempStore(t)

	cases := []struct {
		typ    string
		months int
		want   bool
	}{
		{"decision", 6, true},
		{"policy", 12, true},
		{"preference", 3, true},
		{"bugfix", 0, false},
		{"manual", 0, false},
	}

	for _, tc := range cases {
		res, err := s.AddObservation(AddObservationParams{
			Title:   "t-" + tc.typ,
			Content: "c",
			Project: "decay",
			Type:    tc.typ,
		})
		if err != nil {
			t.Fatalf("AddObservation(%s): %v", tc.typ, err)
		}

		got, ok := reviewAfterOf(t, s, res.ID)
		if ok != tc.want {
			t.Errorf("type %q: review_after set = %v, want %v", tc.typ, ok, tc.want)
			continue
		}
		if tc.want {
			assertMonthsFromNow(t, "type "+tc.typ, got, tc.months)
		}
	}
}

// TestDecay_TopicKeyRevisionKeepsPriorReviewAfter is the reason the stamp lives
// in execInsert and not in the save handler. A topic_key re-save is an UPDATE of
// the same row; if it recomputed review_after, a memory could be kept
// permanently "fresh" by rewriting it, which is precisely the stale context the
// lifecycle feature exists to surface.
func TestDecay_TopicKeyRevisionKeepsPriorReviewAfter(t *testing.T) {
	s := openTempStore(t)

	first, err := s.AddObservation(AddObservationParams{
		Title: "auth model", Content: "v1", Project: "decay",
		Type: "decision", TopicKey: "architecture/auth-model",
	})
	if err != nil {
		t.Fatalf("AddObservation (first): %v", err)
	}
	if _, ok := reviewAfterOf(t, s, first.ID); !ok {
		t.Fatal("a new decision must carry a review_after")
	}

	// Backdate it so a recompute would be unmistakable.
	backdated := time.Now().UTC().AddDate(0, 0, 3).Format(sqliteTimeLayout)
	if _, err := s.DB().Exec(
		`UPDATE memories SET review_after = ? WHERE id = ?`, backdated, first.ID,
	); err != nil {
		t.Fatalf("backdate review_after: %v", err)
	}

	second, err := s.AddObservation(AddObservationParams{
		Title: "auth model", Content: "v2 — now with refresh tokens", Project: "decay",
		Type: "decision", TopicKey: "architecture/auth-model",
	})
	if err != nil {
		t.Fatalf("AddObservation (revision): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("topic_key revision landed on a new row (%d != %d) — fixture no longer tests an UPDATE",
			second.ID, first.ID)
	}

	after, ok := reviewAfterOf(t, s, first.ID)
	if !ok {
		t.Fatal("review_after was cleared by a topic_key revision")
	}
	if !after.Equal(parseTime(backdated)) {
		t.Errorf("topic_key revision moved review_after from %s to %s — a revision must not reset the clock",
			backdated, after.Format(time.RFC3339))
	}
}

// TestDecay_UpdateMemoryDoesNotRecompute is the same guarantee through the
// mem_update surface: editing a row's text is not a review of its contents.
func TestDecay_UpdateMemoryDoesNotRecompute(t *testing.T) {
	s := openTempStore(t)

	res, err := s.AddObservation(AddObservationParams{
		Title: "keep pg", Content: "v1", Project: "decay", Type: "decision",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	pinned := time.Now().UTC().AddDate(0, 0, 2).Format(sqliteTimeLayout)
	if _, err := s.DB().Exec(
		`UPDATE memories SET review_after = ? WHERE id = ?`, pinned, res.ID,
	); err != nil {
		t.Fatalf("pin review_after: %v", err)
	}

	if _, err := s.UpdateMemory(res.ID, "keep pg", "v2", "", "w1"); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}

	got, ok := reviewAfterOf(t, s, res.ID)
	if !ok {
		t.Fatal("UpdateMemory cleared review_after")
	}
	if !got.Equal(parseTime(pinned)) {
		t.Errorf("UpdateMemory moved review_after from %s to %s", pinned, got.Format(time.RFC3339))
	}
}

// TestMarkReviewed_RecomputesFromTypeDecay covers the reset path: a re-reviewed
// policy buys another 12 months, a decision another 6.
func TestMarkReviewed_RecomputesFromTypeDecay(t *testing.T) {
	s := openTempStore(t)

	policy, err := s.AddObservation(AddObservationParams{
		Title: "retention", Content: "c", Project: "decay", Type: "policy",
	})
	if err != nil {
		t.Fatalf("AddObservation(policy): %v", err)
	}
	decision, err := s.AddObservation(AddObservationParams{
		Title: "postgres", Content: "c", Project: "decay", Type: "decision",
	})
	if err != nil {
		t.Fatalf("AddObservation(decision): %v", err)
	}

	// Drive both due dates into the past so the reset is observable.
	stale := time.Now().UTC().AddDate(0, 0, -400).Format(sqliteTimeLayout)
	if _, err := s.DB().Exec(
		`UPDATE memories SET review_after = ? WHERE id IN (?, ?)`, stale, policy.ID, decision.ID,
	); err != nil {
		t.Fatalf("backdate review_after: %v", err)
	}

	n, err := s.MarkReviewed([]int64{policy.ID, decision.ID})
	if err != nil {
		t.Fatalf("MarkReviewed: %v", err)
	}
	if n != 2 {
		t.Errorf("MarkReviewed updated %d rows, want 2", n)
	}

	got, ok := reviewAfterOf(t, s, policy.ID)
	if !ok {
		t.Fatal("policy review_after is NULL after MarkReviewed")
	}
	assertMonthsFromNow(t, "policy after MarkReviewed", got, 12)

	got, ok = reviewAfterOf(t, s, decision.ID)
	if !ok {
		t.Fatal("decision review_after is NULL after MarkReviewed")
	}
	assertMonthsFromNow(t, "decision after MarkReviewed", got, 6)
}

// TestMarkReviewed_UnknownTypeFallsBackToWindow pins the documented DEVIATION
// from upstream. Upstream writes NULL for a type with no decay entry because
// NULL means "never due" there. Here NULL means "updated_at + window", and
// MarkReviewed may not touch updated_at (it is the LWW ordering field), so NULL
// would make marking a bugfix reviewed a no-op — it would come straight back as
// needs_review. The row gets now + window instead, and lands ACTIVE.
func TestMarkReviewed_UnknownTypeFallsBackToWindow(t *testing.T) {
	s := openTempStore(t)
	s.SetReviewWindowDays(30)

	res, err := s.AddObservation(AddObservationParams{
		Title: "fix N+1", Content: "c", Project: "decay", Type: "bugfix",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if _, ok := reviewAfterOf(t, s, res.ID); ok {
		t.Fatal("a bugfix has no decay entry — review_after must be NULL on insert")
	}

	// Age it past the rolling window so it is genuinely needs_review.
	stale := time.Now().UTC().AddDate(0, 0, -60).Format(sqliteTimeLayout)
	if _, err := s.DB().Exec(
		`UPDATE memories SET updated_at = ? WHERE id = ?`, stale, res.ID,
	); err != nil {
		t.Fatalf("age updated_at: %v", err)
	}
	if status, err := s.ReviewStatusForID(res.ID); err != nil || status != ReviewStatusNeedsReview {
		t.Fatalf("pre-condition: status = %q (err %v), want %q", status, err, ReviewStatusNeedsReview)
	}

	if _, err := s.MarkReviewed([]int64{res.ID}); err != nil {
		t.Fatalf("MarkReviewed: %v", err)
	}

	got, ok := reviewAfterOf(t, s, res.ID)
	if !ok {
		t.Fatal("MarkReviewed left review_after NULL — the row is still needs_review, so the mark did nothing")
	}
	if want := time.Now().UTC().AddDate(0, 0, 30); got.Sub(want) > 24*time.Hour || got.Sub(want) < -24*time.Hour {
		t.Errorf("review_after = %s, want ≈ %s (now + 30d window)",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if status, err := s.ReviewStatusForID(res.ID); err != nil || status != ReviewStatusActive {
		t.Errorf("post-MarkReviewed status = %q (err %v), want %q", status, err, ReviewStatusActive)
	}
}

// TestMarkReviewed_SkipsUnknownAndDeletedRows keeps the historical contract:
// an id that names nothing live is skipped, not an error.
func TestMarkReviewed_SkipsUnknownAndDeletedRows(t *testing.T) {
	s := openTempStore(t)

	live, err := s.AddObservation(AddObservationParams{
		Title: "live", Content: "c", Project: "decay", Type: "decision",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	gone, err := s.AddObservation(AddObservationParams{
		Title: "gone", Content: "c", Project: "decay", Type: "decision",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if _, err := s.DB().Exec(
		`UPDATE memories SET deleted_at = datetime('now') WHERE id = ?`, gone.ID,
	); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	n, err := s.MarkReviewed([]int64{live.ID, gone.ID, 999999})
	if err != nil {
		t.Fatalf("MarkReviewed: %v", err)
	}
	if n != 1 {
		t.Errorf("MarkReviewed updated %d rows, want 1 (deleted and unknown ids are skipped)", n)
	}
}
