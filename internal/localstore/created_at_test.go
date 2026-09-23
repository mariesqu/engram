package localstore

// Tests for FUP-005: created_at materializes from the mutation's OccurredAt
// (the ORIGINATING node's write time, carried on the sync wire) instead of
// always defaulting to whichever node happens to apply it. See execInsert's
// doc comment in apply.go.

import (
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// pulledMut builds a mutation shaped like one PullSince would return: SyncID,
// content, an explicit OccurredAt (the wire field FUP-005 now materializes),
// and UpdatedAt/Version/WriterID for Decide/LWW. MutationID/Payload are left
// unset — normalizeMutation (called inside ApplyPulled) derives them, exactly
// as it does for a real pulled mutation whose payload round-tripped through
// central's JSONB.
func pulledMut(syncID, typ, content string, version int, updatedAt, occurredAt time.Time) domain.Mutation {
	return domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       typ,
		Title:      "title",
		Content:    content,
		Project:    "engram",
		Scope:      "project",
		Version:    version,
		WriterID:   "writer-" + syncID,
		UpdatedAt:  updatedAt,
		OccurredAt: occurredAt,
	}
}

// TestExecInsert_CreatedAtFromOccurredAt is the core FUP-005 regression test:
// a pulled mutation carrying a historical OccurredAt must materialize with
// THAT date as created_at, not the arrival instant.
func TestExecInsert_CreatedAtFromOccurredAt(t *testing.T) {
	s := openTempStore(t)
	old := time.Date(2020, 3, 15, 9, 0, 0, 0, time.UTC)

	if err := s.ApplyPulled(pulledMut("sync-created-old", "manual", "old content", 1, old, old)); err != nil {
		t.Fatalf("ApplyPulled: %v", err)
	}

	var createdAt string
	if err := s.db.QueryRow(`SELECT created_at FROM memories WHERE sync_id = 'sync-created-old'`).
		Scan(&createdAt); err != nil {
		t.Fatalf("query created_at: %v", err)
	}
	got := parseTime(createdAt)
	if !got.Equal(old) {
		t.Errorf("created_at = %v, want %v (the mutation's OccurredAt, not now)", got, old)
	}
	// The stored TEXT must use the same layout every default-created row does,
	// so datetime()-wrapped comparisons and lexical ORDER BY stay consistent.
	if createdAt != old.Format(sqliteTimeLayout) {
		t.Errorf("created_at TEXT = %q, want SQLite layout %q", createdAt, old.Format(sqliteTimeLayout))
	}
}

// TestExecInsert_ZeroOccurredAtFallsBackToNow covers the pre-fix fallback: a
// mutation with no OccurredAt at all (predates the field, or a caller that
// never set it) still gets a sane created_at instead of a zero-time row.
func TestExecInsert_ZeroOccurredAtFallsBackToNow(t *testing.T) {
	s := openTempStore(t)
	before := time.Now().UTC()

	m := pulledMut("sync-created-zero", "manual", "content", 1, before, time.Time{})
	if err := s.ApplyPulled(m); err != nil {
		t.Fatalf("ApplyPulled: %v", err)
	}
	after := time.Now().UTC()

	var createdAt string
	if err := s.db.QueryRow(`SELECT created_at FROM memories WHERE sync_id = 'sync-created-zero'`).
		Scan(&createdAt); err != nil {
		t.Fatalf("query created_at: %v", err)
	}
	got := parseTime(createdAt)
	if got.Before(before.Add(-time.Second)) || got.After(after.Add(time.Second)) {
		t.Errorf("created_at = %v, want it within [%v, %v] (now-fallback)", got, before, after)
	}
}

// TestExecInsert_ReviewAfterDatesFromCreatedAt proves review_after for a
// decay type (e.g. "decision") is computed from the ROW's created_at, not
// time.Now(): a two-year-old pulled decision must be immediately due for
// review, not granted a fresh window by the act of applying it today.
func TestExecInsert_ReviewAfterDatesFromCreatedAt(t *testing.T) {
	s := openTempStore(t)
	// Truncated to whole seconds: sqliteTimeLayout has no fractional-second
	// component, so comparing against a sub-second time.Now() value would fail
	// on the lost precision alone, not on the thing this test actually checks.
	old := time.Now().UTC().Truncate(time.Second).AddDate(-2, 0, 0) // two years ago

	if err := s.ApplyPulled(pulledMut("sync-created-decay", "decision", "old decision", 1, old, old)); err != nil {
		t.Fatalf("ApplyPulled: %v", err)
	}

	var reviewAfter string
	if err := s.db.QueryRow(`SELECT review_after FROM memories WHERE sync_id = 'sync-created-decay'`).
		Scan(&reviewAfter); err != nil {
		t.Fatalf("query review_after: %v", err)
	}
	got := parseTime(reviewAfter)
	want := old.AddDate(0, 6, 0) // decision decays after 6 months
	if !got.Equal(want) {
		t.Errorf("review_after = %v, want %v (createdAt + 6mo, not now + 6mo)", got, want)
	}
	if !got.Before(time.Now().UTC()) {
		t.Error("review_after is not already in the past — a 2-year-old decision must be immediately due for review")
	}
}

// TestExecUpdate_NeverChangesCreatedAt pins the documented invariant (already
// true before FUP-005; this is the regression test the requirement asked
// for): a REVISION of an existing row must never move its created_at, no
// matter what OccurredAt the revision itself carries.
func TestExecUpdate_NeverChangesCreatedAt(t *testing.T) {
	s := openTempStore(t)
	original := time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)
	revision := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	if err := s.ApplyPulled(pulledMut("sync-created-update", "manual", "v1", 1, original, original)); err != nil {
		t.Fatalf("ApplyPulled v1: %v", err)
	}
	if err := s.ApplyPulled(pulledMut("sync-created-update", "manual", "v2", 2, revision, revision)); err != nil {
		t.Fatalf("ApplyPulled v2: %v", err)
	}

	var content, createdAt string
	if err := s.db.QueryRow(`SELECT content, created_at FROM memories WHERE sync_id = 'sync-created-update'`).
		Scan(&content, &createdAt); err != nil {
		t.Fatalf("query: %v", err)
	}
	if content != "v2" {
		t.Fatalf("content = %q, want the revision to have landed (v2)", content)
	}
	got := parseTime(createdAt)
	if !got.Equal(original) {
		t.Errorf("created_at after revision = %v, want it UNCHANGED at %v", got, original)
	}
}

// TestRecentObservations_OrdersMixedOldAndNewCreatedAt covers ordering across
// a mix of pulled (historical OccurredAt) and freshly-local (now) rows —
// RecentObservations orders by created_at DESC, and that must reflect the
// TRUE creation time, not arrival order.
func TestRecentObservations_OrdersMixedOldAndNewCreatedAt(t *testing.T) {
	s := openTempStore(t)

	veryOld := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	old := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)

	// Applied in a DELIBERATELY scrambled arrival order — if ordering were by
	// arrival/rowid instead of created_at this would sort wrong.
	if err := s.ApplyPulled(pulledMut("sync-order-old", "manual", "old", 1, old, old)); err != nil {
		t.Fatalf("ApplyPulled old: %v", err)
	}
	if err := s.ApplyPulled(pulledMut("sync-order-veryold", "manual", "very old", 1, veryOld, veryOld)); err != nil {
		t.Fatalf("ApplyPulled veryold: %v", err)
	}
	if _, err := s.LocalWrite(domain.Mutation{
		Op: domain.OpUpsert, SyncID: "sync-order-new", SessionID: "sess",
		EntityType: domain.EntityMemory, Type: "manual", Title: "t", Content: "newest",
		Project: "engram", Scope: "project", WriterID: "writer-new",
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("LocalWrite new: %v", err)
	}

	got, err := s.RecentObservations("engram", "", 10)
	if err != nil {
		t.Fatalf("RecentObservations: %v", err)
	}
	var order []string
	for _, r := range got {
		order = append(order, r.SyncID)
	}
	want := []string{"sync-order-new", "sync-order-old", "sync-order-veryold"}
	if len(order) != len(want) {
		t.Fatalf("RecentObservations order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("RecentObservations order = %v, want %v (created_at DESC across mixed old/new rows)", order, want)
			break
		}
	}
}

// TestSearchFiltered_CreatedFromTo_MixedOldAndNewRows covers the other
// consumer named in FUP-005: SearchFilter.CreatedFrom/CreatedTo must bound
// results by the TRUE created_at, correctly separating an old pulled row from
// a fresh local one.
func TestSearchFiltered_CreatedFromTo_MixedOldAndNewRows(t *testing.T) {
	s := openTempStore(t)

	old := time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC)
	if err := s.ApplyPulled(pulledMut("sync-window-old", "manual", "old windowed content", 1, old, old)); err != nil {
		t.Fatalf("ApplyPulled old: %v", err)
	}
	now := time.Now().UTC()
	if _, err := s.LocalWrite(domain.Mutation{
		Op: domain.OpUpsert, SyncID: "sync-window-new", SessionID: "sess",
		EntityType: domain.EntityMemory, Type: "manual", Title: "t", Content: "new windowed content",
		Project: "engram", Scope: "project", WriterID: "writer-window-new",
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("LocalWrite new: %v", err)
	}

	f := SearchFilter{
		CreatedFrom: time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC),
		CreatedTo:   time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	results, _, err := s.SearchMemoriesFiltered("windowed", "engram", 10, f)
	if err != nil {
		t.Fatalf("SearchMemoriesFiltered: %v", err)
	}
	if len(results) != 1 || results[0].SyncID != "sync-window-old" {
		t.Fatalf("SearchMemoriesFiltered(2019-2021 window) = %+v, want exactly [sync-window-old]", results)
	}

	f2 := SearchFilter{CreatedFrom: now.Add(-time.Hour)}
	results2, _, err := s.SearchMemoriesFiltered("windowed", "engram", 10, f2)
	if err != nil {
		t.Fatalf("SearchMemoriesFiltered (recent window): %v", err)
	}
	if len(results2) != 1 || results2[0].SyncID != "sync-window-new" {
		t.Fatalf("SearchMemoriesFiltered(recent window) = %+v, want exactly [sync-window-new]", results2)
	}
}
