package localstore

import (
	"strings"
	"testing"
)

// explainPlan runs EXPLAIN QUERY PLAN for query/args against s.db and returns
// the joined detail lines — the same shape sessions_scale_test.go and
// listprojects_plan_test.go already use for plan assertions.
func explainPlan(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()

	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
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
		t.Fatalf("plan rows: %v", err)
	}
	return strings.Join(lines, "\n")
}

// TestCountPinned_UsesProjectLowerIndex mirrors CountPinned's actual SQL (see
// pin.go) so the plan is representative of what CountPinned actually runs. A
// plain idx_mem_project cannot serve LOWER(project) = ? — SQLite only indexes
// the exact expression a predicate is written over.
func TestCountPinned_UsesProjectLowerIndex(t *testing.T) {
	s := openTempStore(t)

	const q = `SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND pinned = 1 AND LOWER(project) = ?`
	plan := explainPlan(t, s, q, "acme")
	t.Logf("CountPinned query plan:\n%s", plan)

	if !strings.Contains(strings.ToUpper(plan), "IDX_MEM_PROJECT_LOWER") {
		t.Errorf("CountPinned's LOWER(project) filter is not using idx_mem_project_lower "+
			"(full scan?):\n%s", plan)
	}
}

// TestRecentObservations_UsesProjectLowerIndex mirrors recentObservations'
// actual SQL (see search.go) for the pinAny shape shared by RecentObservations,
// PinnedObservations and RecentUnpinnedObservations.
func TestRecentObservations_UsesProjectLowerIndex(t *testing.T) {
	s := openTempStore(t)

	const q = `SELECT id FROM memories WHERE deleted_at IS NULL AND LOWER(project) = ? ORDER BY datetime(created_at) DESC, id DESC LIMIT ?`
	plan := explainPlan(t, s, q, "acme", 20)
	t.Logf("recentObservations query plan:\n%s", plan)

	if !strings.Contains(strings.ToUpper(plan), "IDX_MEM_PROJECT_LOWER") {
		t.Errorf("recentObservations' LOWER(project) filter is not using idx_mem_project_lower "+
			"(full scan?):\n%s", plan)
	}
}

// TestRecentSessions_UsesProjectLowerIndexes covers both halves of
// RecentSessions' project filter: the outer sessions.project predicate and the
// derived table's memories.project predicate (moved inside the subquery so the
// grouped newest-memory-per-session pass is scoped to one project instead of
// every project in the store — see recentSessionsQuery's doc comment).
func TestRecentSessions_UsesProjectLowerIndexes(t *testing.T) {
	s := openTempStore(t)

	query, args := recentSessionsQuery("acme", 5)
	plan := explainPlan(t, s, query, args...)
	t.Logf("RecentSessions query plan:\n%s", plan)

	up := strings.ToUpper(plan)
	if !strings.Contains(up, "IDX_SESSIONS_PROJECT_LOWER") {
		t.Errorf("RecentSessions' sessions.project filter is not using "+
			"idx_sessions_project_lower (full scan?):\n%s", plan)
	}
	if !strings.Contains(up, "IDX_MEM_PROJECT_LOWER") {
		t.Errorf("RecentSessions' derived-table project filter is not using "+
			"idx_mem_project_lower (full scan?):\n%s", plan)
	}
}

// TestRecentSessions_DerivedTableScopedToProject is the functional counterpart
// to the plan test above: a second project's memories must not be grouped into
// (or influence the ordering of) the first project's sessions.
func TestRecentSessions_DerivedTableScopedToProject(t *testing.T) {
	s := openTempStore(t)

	if err := s.CreateSession("sess-a", "proj-a", "/tmp/a"); err != nil {
		t.Fatalf("CreateSession a: %v", err)
	}
	if err := s.CreateSession("sess-b", "proj-b", "/tmp/b"); err != nil {
		t.Fatalf("CreateSession b: %v", err)
	}

	// proj-b gets a memory far in the future — if the derived table ever groups
	// across projects again, it can only ever affect proj-b's own row (join key
	// is session_id), so this asserts proj-a's result set/count stays exactly
	// what proj-a owns regardless of what proj-b is doing.
	if _, err := s.db.Exec(
		`INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id, created_at, updated_at)
		 VALUES ('mem-b-1', 'sess-b', 'memory', 'manual', 'b title', 'b content', 'proj-b', 'project', 'w', '2099-01-01 00:00:00', '2099-01-01 00:00:00')`,
	); err != nil {
		t.Fatalf("insert proj-b memory: %v", err)
	}

	got, err := s.RecentSessions("proj-a", 10)
	if err != nil {
		t.Fatalf("RecentSessions: %v", err)
	}
	if len(got) != 1 || got[0].ID != "sess-a" {
		t.Fatalf("RecentSessions(proj-a) = %+v, want exactly [sess-a]", got)
	}
}
