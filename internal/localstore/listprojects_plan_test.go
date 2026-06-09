package localstore

import (
	"strings"
	"testing"
)

// TestListProjects_UsesProjectIndexes asserts the ListProjects query is served by
// idx_mem_project / idx_tomb_project (an index scan, not a full table scan) —
// guarding the per-tick autosync cost. The plan is logged so the index-distinct
// behaviour (whether the per-arm DISTINCT needs a temp b-tree) is visible.
func TestListProjects_UsesProjectIndexes(t *testing.T) {
	s := openTempStore(t)

	// Must mirror ListProjects()'s actual 4-arm UNION so the plan is representative.
	const q = `EXPLAIN QUERY PLAN
		SELECT DISTINCT project FROM memories
		UNION
		SELECT DISTINCT project FROM memory_tombstones
		UNION
		SELECT DISTINCT project FROM user_prompts
		UNION
		SELECT DISTINCT project FROM prompt_tombstones
		ORDER BY project`

	rows, err := s.db.Query(q)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		lines = append(lines, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	plan := strings.Join(lines, "\n")
	t.Logf("ListProjects query plan:\n%s", plan)

	if !strings.Contains(plan, "memories USING") {
		t.Errorf("memories arm not using an index (full scan?):\n%s", plan)
	}
	if !strings.Contains(plan, "memory_tombstones USING") {
		t.Errorf("tombstones arm not using an index (full scan?):\n%s", plan)
	}
	if !strings.Contains(plan, "user_prompts USING") {
		t.Errorf("user_prompts arm not using an index (full scan?):\n%s", plan)
	}
	if !strings.Contains(plan, "prompt_tombstones USING") {
		t.Errorf("prompt_tombstones arm not using an index (full scan?):\n%s", plan)
	}
}
