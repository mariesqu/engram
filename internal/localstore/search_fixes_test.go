package localstore

import "testing"

// TestSearchMemoriesFiltered_PopulatesID verifies search results carry the
// integer primary key, and that the surfaced id resolves via GetObservation —
// i.e. the mem_search → mem_get_observation(id) workflow actually composes.
func TestSearchMemoriesFiltered_PopulatesID(t *testing.T) {
	s := openTempStore(t)

	res, err := s.AddObservation(AddObservationParams{Title: "t", Content: "findme alpha token", Project: "p"})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	hits, _, err := s.SearchMemoriesFiltered("findme", "p", 10, SearchFilter{})
	if err != nil {
		t.Fatalf("SearchMemoriesFiltered: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no search hits")
	}
	if hits[0].ID == 0 {
		t.Fatal("search hit ID is 0 — the search→get workflow has no usable id")
	}
	if hits[0].ID != res.ID {
		t.Errorf("search hit ID = %d, want %d", hits[0].ID, res.ID)
	}
	// The surfaced id must round-trip through GetObservation.
	got, err := s.GetObservation(hits[0].ID)
	if err != nil {
		t.Fatalf("GetObservation(searchHit.ID=%d): %v — search→get workflow broken", hits[0].ID, err)
	}
	if got.Content != "findme alpha token" {
		t.Errorf("GetObservation content = %q, want the saved content", got.Content)
	}
}
