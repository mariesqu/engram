package main

// tools_search_paging_test.go — handler tests for mem_search's offset and
// created_from / created_to arguments.

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mariesqu/engram/internal/localstore"
)

// searchPagingDaemon builds a daemon and seeds n memories that all match the
// query "haystack", each backdated to a distinct day so the date-bound
// assertions have something to cut on. Returns the components and the ids in
// creation order (oldest first).
func searchPagingDaemon(t *testing.T, n int) (*daemonComponents, []int64) {
	t.Helper()
	components, err := buildDaemon(daemonCfg{
		db:           filepath.Join(t.TempDir(), "paging.db"),
		syncInterval: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		res, err := components.store.AddObservation(localstore.AddObservationParams{
			Title:   fmt.Sprintf("haystack %d", i),
			Content: "haystack needle content",
			Project: "paging",
			Type:    "decision",
		})
		if err != nil {
			t.Fatalf("AddObservation(%d): %v", i, err)
		}
		// Day i of June 2024, so row 0 is the oldest.
		when := time.Date(2024, 6, i+1, 12, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")
		if _, err := components.store.DB().Exec(
			`UPDATE memories SET created_at = ? WHERE id = ?`, when, res.ID,
		); err != nil {
			t.Fatalf("backdate created_at(%d): %v", i, err)
		}
		ids[i] = res.ID
	}
	return components, ids
}

// runSearch calls the registered mem_search handler and returns its text,
// failing on a transport error. It does NOT fail on a tool error — several tests
// here are about the tool errors.
func runSearch(t *testing.T, c *daemonComponents, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	tool := c.mcpServer.ListTools()["mem_search"]
	result, err := tool.Handler(t.Context(), newToolRequest("mem_search", args))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	return result, result.Content[0].(mcp.TextContent).Text
}

// TestMemSearch_OffsetPagesThroughResults covers the happy path of the new
// argument: two pages of two cover four distinct rows, in the same order the
// unpaged call returns them.
func TestMemSearch_OffsetPagesThroughResults(t *testing.T) {
	c, ids := searchPagingDaemon(t, 5)

	_, unpaged := runSearch(t, c, map[string]any{
		"query": "haystack", "project": "paging", "limit": float64(4),
	})

	seen := map[string]bool{}
	for page := 0; page < 2; page++ {
		_, text := runSearch(t, c, map[string]any{
			"query": "haystack", "project": "paging",
			"limit": float64(2), "offset": float64(page * 2),
		})
		for _, id := range ids {
			marker := fmt.Sprintf("#%d ", id)
			if !strings.Contains(text, marker) {
				continue
			}
			if seen[marker] {
				t.Errorf("page %d repeats %s — offset is not being applied", page, marker)
			}
			seen[marker] = true
			if !strings.Contains(unpaged, marker) {
				t.Errorf("page %d returned %s, which the unpaged top-4 does not contain", page, marker)
			}
		}
	}
	if len(seen) != 4 {
		t.Errorf("two pages of 2 covered %d distinct rows, want 4", len(seen))
	}
}

// TestMemSearch_OffsetPastTheEndIsEmpty pins the behaviour that makes paging
// terminable: reaching past the last row answers "nothing", not page one.
func TestMemSearch_OffsetPastTheEndIsEmpty(t *testing.T) {
	c, _ := searchPagingDaemon(t, 3)

	result, text := runSearch(t, c, map[string]any{
		"query": "haystack", "project": "paging", "offset": float64(500),
	})
	if result.IsError {
		t.Fatalf("paging past the end must not be an error: %s", text)
	}
	if !strings.Contains(text, "No memories found") {
		t.Errorf("offset past the end returned results instead of an empty page:\n%s", text)
	}
}

// TestMemSearch_DateBoundsFilter covers created_from / created_to in both
// accepted formats. The date-only created_to case is the one that matters: a
// bare "2024-06-03" has to cover the whole day, or it silently drops everything
// saved on the day the caller explicitly asked to include.
func TestMemSearch_DateBoundsFilter(t *testing.T) {
	c, ids := searchPagingDaemon(t, 5) // 2024-06-01 … 2024-06-05

	marker := func(i int) string { return fmt.Sprintf("#%d ", ids[i]) }

	cases := []struct {
		name    string
		args    map[string]any
		want    []int
		exclude []int
	}{
		{
			name: "created_from date-only",
			args: map[string]any{"created_from": "2024-06-04"},
			want: []int{3, 4}, exclude: []int{0, 1, 2},
		},
		{
			name: "created_to date-only covers the whole day",
			args: map[string]any{"created_to": "2024-06-02"},
			want: []int{0, 1}, exclude: []int{2, 3, 4},
		},
		{
			name: "rfc3339 window",
			args: map[string]any{
				"created_from": "2024-06-02T00:00:00Z",
				"created_to":   "2024-06-03T23:59:59Z",
			},
			want: []int{1, 2}, exclude: []int{0, 3, 4},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"query": "haystack", "project": "paging", "limit": float64(20)}
			for k, v := range tc.args {
				args[k] = v
			}
			result, text := runSearch(t, c, args)
			if result.IsError {
				t.Fatalf("tool error: %s", text)
			}
			for _, i := range tc.want {
				if !strings.Contains(text, marker(i)) {
					t.Errorf("row %d (2024-06-%02d) is missing from the window:\n%s", i, i+1, text)
				}
			}
			for _, i := range tc.exclude {
				if strings.Contains(text, marker(i)) {
					t.Errorf("row %d (2024-06-%02d) leaked past the window:\n%s", i, i+1, text)
				}
			}
		})
	}
}

// TestMemSearch_RejectsMalformedPagingArgs is the other half of the contract.
// Every one of these used to be silently ignorable, and each ignored value
// answers a DIFFERENT question than the caller asked — page one instead of page
// five, the whole corpus instead of last week — with no way for the caller to
// tell.
func TestMemSearch_RejectsMalformedPagingArgs(t *testing.T) {
	c, _ := searchPagingDaemon(t, 2)

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"offset string", map[string]any{"offset": "10"}, "offset must be a number"},
		{"offset negative", map[string]any{"offset": float64(-1)}, "offset must be a non-negative integer"},
		{"offset fractional", map[string]any{"offset": 1.5}, "offset must be a non-negative integer"},
		{"created_from garbage", map[string]any{"created_from": "last tuesday"}, "created_from must be RFC3339"},
		{"created_to garbage", map[string]any{"created_to": "06/30/2024"}, "created_to must be RFC3339"},
		{"created_from non-string", map[string]any{"created_from": float64(2024)}, "created_from must be a string"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"query": "haystack", "project": "paging"}
			for k, v := range tc.args {
				args[k] = v
			}
			result, text := runSearch(t, c, args)
			if !result.IsError {
				t.Fatalf("expected a tool error, got:\n%s", text)
			}
			if !strings.Contains(text, tc.want) {
				t.Errorf("error text %q does not contain %q", text, tc.want)
			}
		})
	}
}

// TestMemSearch_AbsentPagingArgsChangeNothing is the additive guarantee: a call
// that names none of the new arguments must behave exactly as it did before they
// existed, including an empty-string date (which means "no bound", not "the zero
// year").
func TestMemSearch_AbsentPagingArgsChangeNothing(t *testing.T) {
	c, _ := searchPagingDaemon(t, 3)

	_, base := runSearch(t, c, map[string]any{"query": "haystack", "project": "paging"})
	_, zeroed := runSearch(t, c, map[string]any{
		"query": "haystack", "project": "paging",
		"offset": float64(0), "created_from": "", "created_to": "  ",
	})
	if base != zeroed {
		t.Errorf("zero-valued paging args changed the result.\nbase:\n%s\nzeroed:\n%s", base, zeroed)
	}
	if !strings.Contains(base, "Found 3 memories") {
		t.Errorf("baseline search did not return all three rows:\n%s", base)
	}
}
// TestMemSearch_RejectsInvertedDateWindow covers the one malformed window the
// parser cannot see: both bounds are valid dates, and the pair is nonsense. The
// store ANDs them, so the search can only return nothing — and "No memories
// found" is a true answer that hides the caller's swapped arguments behind a
// statement about the corpus.
func TestMemSearch_RejectsInvertedDateWindow(t *testing.T) {
	c, _ := searchPagingDaemon(t, 3)

	result, text := runSearch(t, c, map[string]any{
		"query": "haystack", "project": "paging",
		"created_from": "2024-06-05", "created_to": "2024-06-02",
	})
	if !result.IsError {
		t.Fatalf("an inverted window must be a tool error, got:\n%s", text)
	}
	if !strings.Contains(text, "created_from is after created_to") {
		t.Errorf("error text %q does not name the inverted window", text)
	}

	// The bound is AFTER, not "different": an instant-wide window is a legitimate
	// question, and a check that refused it would reject a correct call.
	result, text = runSearch(t, c, map[string]any{
		"query": "haystack", "project": "paging",
		"created_from": "2024-06-02T12:00:00Z", "created_to": "2024-06-02T12:00:00Z",
	})
	if result.IsError {
		t.Errorf("created_from == created_to must be allowed, got a tool error: %s", text)
	}
}

// TestMemSearch_RejectsOffsetPastTheCeiling pins that the ceiling says what it
// is. The value below is a well-formed non-negative integer, so the generic
// "must be a non-negative integer" answer described a problem the caller did not
// have and gave them nothing they could act on.
func TestMemSearch_RejectsOffsetPastTheCeiling(t *testing.T) {
	c, _ := searchPagingDaemon(t, 2)

	result, text := runSearch(t, c, map[string]any{
		"query": "haystack", "project": "paging",
		"offset": float64(math.MaxInt32) + 1,
	})
	if !result.IsError {
		t.Fatalf("an offset past the ceiling must be a tool error, got:\n%s", text)
	}
	if !strings.Contains(text, "offset is too large") {
		t.Errorf("error text %q does not say the offset is too large", text)
	}
}

// TestMemSearch_OffsetPageIsNumberedFromTheOffset covers what a paged RESULT
// says about itself. Page one renders exactly as it always has; every page past
// it names its row range and numbers its items from the offset, because
// "Found 2 memories" over [1] and [2] renders page one and page three
// identically and an agent walking pages cannot tell which one it is holding.
func TestMemSearch_OffsetPageIsNumberedFromTheOffset(t *testing.T) {
	c, _ := searchPagingDaemon(t, 5)

	_, page1 := runSearch(t, c, map[string]any{
		"query": "haystack", "project": "paging", "limit": float64(2),
	})
	if !strings.Contains(page1, "Found 2 memories:") {
		t.Errorf("page one header changed:\n%s", page1)
	}
	if !strings.Contains(page1, "[1] #") || !strings.Contains(page1, "[2] #") {
		t.Errorf("page one is not numbered [1],[2]:\n%s", page1)
	}

	result, page2 := runSearch(t, c, map[string]any{
		"query": "haystack", "project": "paging",
		"limit": float64(2), "offset": float64(2),
	})
	if result.IsError {
		t.Fatalf("tool error: %s", page2)
	}
	if !strings.Contains(page2, "Found 2 memories (rows 3–4):") {
		t.Errorf("page two does not name its row range:\n%s", page2)
	}
	if !strings.Contains(page2, "[3] #") || !strings.Contains(page2, "[4] #") {
		t.Errorf("page two is not numbered [3],[4]:\n%s", page2)
	}
	if strings.Contains(page2, "[1] #") {
		t.Errorf("page two restarts its numbering at [1]:\n%s", page2)
	}

	// A short final page reports the rows it actually holds, not the rows it
	// asked for: offset 4 over five rows is row 5 alone.
	_, lastPage := runSearch(t, c, map[string]any{
		"query": "haystack", "project": "paging",
		"limit": float64(2), "offset": float64(4),
	})
	if !strings.Contains(lastPage, "Found 1 memories (rows 5–5):") {
		t.Errorf("short final page does not report a 5–5 range:\n%s", lastPage)
	}
}
