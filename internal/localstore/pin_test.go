package localstore

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestSetPinned_RoundTrip covers the basic contract: pin, read back, unpin, read
// back. Both directions are idempotent — re-pinning an already-pinned row is a
// successful no-op, not an error, because the tool is declared idempotent.
func TestSetPinned_RoundTrip(t *testing.T) {
	s := openTempStore(t)

	res, err := s.AddObservation(AddObservationParams{
		Title: "keep this", Content: "c", Project: "pin", Type: "decision",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	if pinned, err := s.IsPinned(res.ID); err != nil || pinned {
		t.Fatalf("a fresh memory must be unpinned; got pinned=%v err=%v", pinned, err)
	}

	for _, want := range []bool{true, true, false, false} {
		got, err := s.SetPinned(res.ID, want)
		if err != nil {
			t.Fatalf("SetPinned(%v): %v", want, err)
		}
		if got != want {
			t.Errorf("SetPinned(%v) returned %v", want, got)
		}
		readBack, err := s.IsPinned(res.ID)
		if err != nil {
			t.Fatalf("IsPinned: %v", err)
		}
		if readBack != want {
			t.Errorf("after SetPinned(%v), IsPinned = %v", want, readBack)
		}
	}
}

// TestSetPinned_UnknownAndDeletedIDsError pins the failure mode that matters:
// reporting success for a memory that can never surface would leave the caller
// waiting for a pinned row that does not exist.
func TestSetPinned_UnknownAndDeletedIDsError(t *testing.T) {
	s := openTempStore(t)

	if _, err := s.SetPinned(999999, true); !errors.Is(err, ErrObservationNotFound) {
		t.Errorf("SetPinned on an unknown id = %v, want ErrObservationNotFound", err)
	}

	res, err := s.AddObservation(AddObservationParams{
		Title: "gone", Content: "c", Project: "pin", Type: "decision",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if _, err := s.DB().Exec(
		`UPDATE memories SET deleted_at = datetime('now') WHERE id = ?`, res.ID,
	); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	if _, err := s.SetPinned(res.ID, true); !errors.Is(err, ErrObservationNotFound) {
		t.Errorf("SetPinned on a soft-deleted row = %v, want ErrObservationNotFound", err)
	}
}

// TestFormatContext_PinnedSection covers the context split end to end: pinned
// rows get their own section ahead of recents, and do NOT also appear under
// "Recent Observations" — repeating them would spend the caller's context window
// saying the same thing twice.
func TestFormatContext_PinnedSection(t *testing.T) {
	s := openTempStore(t)
	const project = "pin"

	pinned, err := s.AddObservation(AddObservationParams{
		Title: "always-relevant fact", Content: "postgres, never mssql", Project: project, Type: "decision",
	})
	if err != nil {
		t.Fatalf("AddObservation(pinned): %v", err)
	}
	if _, err := s.AddObservation(AddObservationParams{
		Title: "ordinary note", Content: "something else", Project: project, Type: "manual",
	}); err != nil {
		t.Fatalf("AddObservation(ordinary): %v", err)
	}

	// Before pinning: no section, and the row sits under recents.
	before, err := s.FormatContext(project, "")
	if err != nil {
		t.Fatalf("FormatContext (before): %v", err)
	}
	if strings.Contains(before, "### Pinned") {
		t.Error("a store with nothing pinned must not render a '### Pinned' section")
	}

	if _, err := s.SetPinned(pinned.ID, true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}

	got, err := s.FormatContext(project, "")
	if err != nil {
		t.Fatalf("FormatContext (after): %v", err)
	}

	pinnedIdx := strings.Index(got, "### Pinned")
	recentIdx := strings.Index(got, "### Recent Observations")
	if pinnedIdx < 0 {
		t.Fatalf("missing '### Pinned' section; got:\n%s", got)
	}
	if recentIdx < 0 {
		t.Fatalf("missing '### Recent Observations' section; got:\n%s", got)
	}
	if pinnedIdx > recentIdx {
		t.Errorf("'### Pinned' must come before '### Recent Observations'; got:\n%s", got)
	}

	pinnedBlock := got[pinnedIdx:recentIdx]
	recentBlock := got[recentIdx:]
	if !strings.Contains(pinnedBlock, "always-relevant fact") {
		t.Errorf("pinned memory is missing from the Pinned section:\n%s", pinnedBlock)
	}
	if strings.Contains(recentBlock, "always-relevant fact") {
		t.Errorf("pinned memory is repeated under Recent Observations:\n%s", recentBlock)
	}
	if !strings.Contains(recentBlock, "ordinary note") {
		t.Errorf("unpinned memory is missing from Recent Observations:\n%s", recentBlock)
	}
}

// TestSearch_PinnedRanksAboveEqualUnpinned pins the ranking boost using two rows
// that are IDENTICAL in every dimension the ranker sees — same title, same body,
// same type, same project — so the only thing that can separate them is the pin.
func TestSearch_PinnedRanksAboveEqualUnpinned(t *testing.T) {
	s := openTempStore(t)
	const project = "pin"

	first, err := s.AddObservation(AddObservationParams{
		Title: "alpha beta", Content: "alpha beta gamma", Project: project, Type: "manual",
	})
	if err != nil {
		t.Fatalf("AddObservation(first): %v", err)
	}
	second, err := s.AddObservation(AddObservationParams{
		Title: "alpha beta", Content: "alpha beta gamma", Project: project, Type: "manual",
	})
	if err != nil {
		t.Fatalf("AddObservation(second): %v", err)
	}

	// Pin the row that loses the tie-break today, so a pass cannot come from the
	// pre-existing ordering.
	baseline, _, err := s.SearchMemoriesFiltered("alpha", project, 10, SearchFilter{})
	if err != nil {
		t.Fatalf("baseline search: %v", err)
	}
	if len(baseline) != 2 {
		t.Fatalf("baseline search returned %d rows, want 2", len(baseline))
	}
	loser := second.ID
	if baseline[0].ID == second.ID {
		loser = first.ID
	}

	if _, err := s.SetPinned(loser, true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}

	got, _, err := s.SearchMemoriesFiltered("alpha", project, 10, SearchFilter{})
	if err != nil {
		t.Fatalf("search after pin: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("search returned %d rows, want 2", len(got))
	}
	if got[0].ID != loser {
		t.Errorf("pinned row #%d did not rank first (got #%d) — the pinned boost is not applied",
			loser, got[0].ID)
	}
}

// ── context budget ───────────────────────────────────────────────────────────

// TestCountPinned_ScopesLikePinnedObservations pins that the count and the list
// answer from the same set. A count that ignored project or scope would make the
// overflow line lie in exactly the situation it exists for: a user with pins in
// three projects would be told about pins that are not in the section above it.
func TestCountPinned_ScopesLikePinnedObservations(t *testing.T) {
	s := openTempStore(t)

	seed := func(project, scope string, pinned bool) {
		t.Helper()
		res, err := s.AddObservation(AddObservationParams{
			Title: "p", Content: "c", Project: project, Scope: scope, Type: "decision",
		})
		if err != nil {
			t.Fatalf("AddObservation(%s/%s): %v", project, scope, err)
		}
		if pinned {
			if _, err := s.SetPinned(res.ID, true); err != nil {
				t.Fatalf("SetPinned: %v", err)
			}
		}
	}

	seed("alpha", "project", true)
	seed("alpha", "project", true)
	seed("alpha", "personal", true)
	seed("beta", "project", true)
	seed("alpha", "project", false)

	for _, tc := range []struct {
		project, scope string
		want           int
	}{
		{"alpha", "project", 2},
		{"alpha", "personal", 1},
		{"alpha", "", 3},
		{"beta", "project", 1},
		{"", "", 4},
	} {
		got, err := s.CountPinned(tc.project, tc.scope)
		if err != nil {
			t.Fatalf("CountPinned(%q,%q): %v", tc.project, tc.scope, err)
		}
		if got != tc.want {
			t.Errorf("CountPinned(%q,%q) = %d, want %d", tc.project, tc.scope, got, tc.want)
		}
		list, err := s.PinnedObservations(tc.project, tc.scope, 100)
		if err != nil {
			t.Fatalf("PinnedObservations(%q,%q): %v", tc.project, tc.scope, err)
		}
		if len(list) != got {
			t.Errorf("CountPinned(%q,%q)=%d but PinnedObservations returned %d rows",
				tc.project, tc.scope, got, len(list))
		}
	}
}

// TestFormatContext_PinnedSectionIsBudgeted covers the whole budget in one pass:
// the pinned section stops at contextPinnedLimit, says how many it left out, and
// the two sections together stay inside the 30-bullet ceiling even when every
// memory in the store is pinned.
func TestFormatContext_PinnedSectionIsBudgeted(t *testing.T) {
	s := openTempStore(t)
	const project = "budget"
	const pinnedRows = contextPinnedLimit + 14

	for i := 0; i < pinnedRows; i++ {
		res, err := s.AddObservation(AddObservationParams{
			Title: fmt.Sprintf("pinned %d", i), Content: "c", Project: project, Type: "decision",
		})
		if err != nil {
			t.Fatalf("AddObservation: %v", err)
		}
		if _, err := s.SetPinned(res.ID, true); err != nil {
			t.Fatalf("SetPinned: %v", err)
		}
	}
	for i := 0; i < contextRecentLimit+5; i++ {
		if _, err := s.AddObservation(AddObservationParams{
			Title: fmt.Sprintf("recent %d", i), Content: "c", Project: project, Type: "manual",
		}); err != nil {
			t.Fatalf("AddObservation: %v", err)
		}
	}

	got, err := s.FormatContext(project, "")
	if err != nil {
		t.Fatalf("FormatContext: %v", err)
	}

	pinnedBullets := strings.Count(got, "- [decision] **pinned ")
	if pinnedBullets != contextPinnedLimit {
		t.Errorf("pinned section rendered %d bullets, want %d (the cap)", pinnedBullets, contextPinnedLimit)
	}
	recentBullets := strings.Count(got, "- [manual] **recent ")
	if recentBullets != contextRecentLimit {
		t.Errorf("recent section rendered %d bullets, want %d (the cap)", recentBullets, contextRecentLimit)
	}

	wantOverflow := fmt.Sprintf("- …and %d more pinned (use mem_search)", pinnedRows-contextPinnedLimit)
	if !strings.Contains(got, wantOverflow) {
		t.Errorf("missing the overflow line %q; got:\n%s", wantOverflow, got)
	}

	// The overflow line belongs to the pinned section, not the recents below it.
	if idx, recents := strings.Index(got, wantOverflow), strings.Index(got, "### Recent Observations"); idx > recents {
		t.Errorf("overflow line rendered after '### Recent Observations'; got:\n%s", got)
	}
}

// TestFormatContext_NoOverflowLineWhenPinsFitTheCap is the other half: the line
// is a warning about truncation, so a store whose pins all fit must not carry it.
// A store with EXACTLY the cap pinned is the boundary that decides whether the
// condition is "> cap" or "≥ cap".
func TestFormatContext_NoOverflowLineWhenPinsFitTheCap(t *testing.T) {
	s := openTempStore(t)
	const project = "budget"

	for i := 0; i < contextPinnedLimit; i++ {
		res, err := s.AddObservation(AddObservationParams{
			Title: fmt.Sprintf("pinned %d", i), Content: "c", Project: project, Type: "decision",
		})
		if err != nil {
			t.Fatalf("AddObservation: %v", err)
		}
		if _, err := s.SetPinned(res.ID, true); err != nil {
			t.Fatalf("SetPinned: %v", err)
		}
	}

	got, err := s.FormatContext(project, "")
	if err != nil {
		t.Fatalf("FormatContext: %v", err)
	}
	if strings.Contains(got, "more pinned") {
		t.Errorf("exactly %d pins fit the cap — no overflow line belongs here; got:\n%s",
			contextPinnedLimit, got)
	}
}

// ── the unpinned corpus is untouched ─────────────────────────────────────────

// TestSearchFiltered_UnpinnedOrderMatchesBareFTSRank is the guarantee the pinned
// boost owes every existing user: on a store where nothing is pinned, the FTS
// ORDER BY must return exactly what a bare `ORDER BY fts.rank` returns. The boost
// multiplies by 1.0 for an unpinned row, but it also changes the QUERY PLAN — a
// bare fts.rank is pushed down into FTS5, while the CASE expression is sorted in
// a temp b-tree — and "mathematically equivalent" is not the same claim as
// "produces the same order".
//
// 200 rows with strictly descending term frequency at a constant document length
// give 200 distinct bm25 scores, so the comparison is about ordering and not
// about how two sorts happen to break a tie.
func TestSearchFiltered_UnpinnedOrderMatchesBareFTSRank(t *testing.T) {
	s := openTempStore(t)
	const project = "rank"
	const rows = 200

	for i := 0; i < rows; i++ {
		var content strings.Builder
		for j := 0; j < rows; j++ {
			if j < rows-i {
				content.WriteString("alpha ")
			} else {
				fmt.Fprintf(&content, "pad%d ", j)
			}
		}
		if _, err := s.AddObservation(AddObservationParams{
			Title: fmt.Sprintf("doc %d", i), Content: content.String(),
			Project: project, Type: "decision",
		}); err != nil {
			t.Fatalf("AddObservation(%d): %v", i, err)
		}
	}

	got, _, err := s.SearchMemoriesFiltered("alpha", project, rows, SearchFilter{})
	if err != nil {
		t.Fatalf("SearchMemoriesFiltered: %v", err)
	}
	if len(got) != rows {
		t.Fatalf("search returned %d rows, want %d", len(got), rows)
	}

	// The reference: the identical query with the pinned CASE removed.
	reference, err := s.DB().Query(`
		SELECT m.id
		FROM memories_fts fts
		JOIN memories m ON m.id = fts.rowid
		WHERE memories_fts MATCH ?
		  AND m.deleted_at IS NULL
		  AND LOWER(m.project) = ?
		ORDER BY fts.rank
		LIMIT ?`, "\"alpha\"", project, rows)
	if err != nil {
		t.Fatalf("reference query: %v", err)
	}
	defer reference.Close()

	i := 0
	for reference.Next() {
		var id int64
		if err := reference.Scan(&id); err != nil {
			t.Fatalf("scan reference row: %v", err)
		}
		if i >= len(got) {
			t.Fatalf("reference has more rows than the search returned (%d)", len(got))
		}
		if got[i].ID != id {
			t.Fatalf("position %d: pinned-boost ORDER BY returned #%d, bare fts.rank returned #%d — "+
				"the boost reorders a corpus with nothing pinned", i, got[i].ID, id)
		}
		i++
	}
	if err := reference.Err(); err != nil {
		t.Fatalf("reference rows: %v", err)
	}
	if i != rows {
		t.Errorf("reference query returned %d rows, want %d", i, rows)
	}
}
