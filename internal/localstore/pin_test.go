package localstore

import (
	"errors"
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
