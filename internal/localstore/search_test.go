package localstore

import (
	"strings"
	"testing"
	"time"
)

// seedObservation is a helper that inserts a raw memories row for test setup.
// It bypasses AddObservation so tests can control project/type/scope/deleted_at
// precisely without going through the full mutation pipeline.
func seedObservation(t *testing.T, s *Store, syncID, title, content, project, typ, scope string) {
	t.Helper()
	_, err := s.db.Exec(
		`INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		 VALUES (?, 'seed-sess', 'memory', ?, ?, ?, ?, ?, 'w1')`,
		syncID, typ, title, content, project, scope,
	)
	if err != nil {
		t.Fatalf("seedObservation(%q): %v", syncID, err)
	}
}

// softDelete marks a memories row as deleted for testing exclusion.
func softDelete(t *testing.T, s *Store, syncID string) {
	t.Helper()
	_, err := s.db.Exec(
		`UPDATE memories SET deleted_at = ? WHERE sync_id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), syncID,
	)
	if err != nil {
		t.Fatalf("softDelete(%q): %v", syncID, err)
	}
}

// ── SearchMemoriesFiltered ────────────────────────────────────────────────────

// TestSearchMemoriesFiltered_TypeFilter verifies that the type filter narrows
// results to only the given type, regardless of scope or project.
func TestSearchMemoriesFiltered_TypeFilter(t *testing.T) {
	s := openTempStore(t)

	seedObservation(t, s, "sf-decision-1", "auth decision", "chose JWT over sessions", "proj-a", "decision", "project")
	seedObservation(t, s, "sf-bugfix-1", "auth bugfix", "fixed JWT expiry bug", "proj-a", "bugfix", "project")
	seedObservation(t, s, "sf-decision-2", "auth pattern", "established auth pattern", "proj-a", "decision", "project")

	// Search for "auth" with type=decision — should return 2, not 3.
	results, err := s.SearchMemoriesFiltered("auth", "proj-a", 10, SearchFilter{Type: "decision"})
	if err != nil {
		t.Fatalf("SearchMemoriesFiltered type filter: %v", err)
	}
	for _, r := range results {
		if r.Type != "decision" {
			t.Errorf("type filter returned type %q, want %q (sync_id=%q)", r.Type, "decision", r.SyncID)
		}
	}
	if len(results) < 1 {
		t.Errorf("expected at least 1 decision result, got 0")
	}
	for _, r := range results {
		if r.SyncID == "sf-bugfix-1" {
			t.Error("type filter did not exclude bugfix row")
		}
	}
}

// TestSearchMemoriesFiltered_ScopeFilter verifies that the scope filter narrows
// results to only the given scope.
func TestSearchMemoriesFiltered_ScopeFilter(t *testing.T) {
	s := openTempStore(t)

	seedObservation(t, s, "sf-proj-1", "project scoped memory", "project note", "myproject", "manual", "project")
	seedObservation(t, s, "sf-personal-1", "personal scoped memory", "personal note", "myproject", "manual", "personal")

	// Scope=project must not return the personal row.
	projResults, err := s.SearchMemoriesFiltered("scoped memory", "myproject", 10, SearchFilter{Scope: "project"})
	if err != nil {
		t.Fatalf("scope=project filter: %v", err)
	}
	for _, r := range projResults {
		if r.Scope != "project" {
			t.Errorf("scope filter returned scope %q, want %q", r.Scope, "project")
		}
	}

	// Scope=personal must not return the project row.
	personalResults, err := s.SearchMemoriesFiltered("scoped memory", "myproject", 10, SearchFilter{Scope: "personal"})
	if err != nil {
		t.Fatalf("scope=personal filter: %v", err)
	}
	for _, r := range personalResults {
		if r.Scope != "personal" {
			t.Errorf("scope filter returned scope %q, want %q", r.Scope, "personal")
		}
	}
}

// TestSearchMemoriesFiltered_ProjectFilter verifies that the project filter
// narrows results to only the given project.
func TestSearchMemoriesFiltered_ProjectFilter(t *testing.T) {
	s := openTempStore(t)

	seedObservation(t, s, "sf-pa-1", "cache architecture", "redis caching layer", "project-a", "architecture", "project")
	seedObservation(t, s, "sf-pb-1", "cache architecture", "memcache layer", "project-b", "architecture", "project")

	results, err := s.SearchMemoriesFiltered("cache architecture", "project-a", 10, SearchFilter{})
	if err != nil {
		t.Fatalf("project filter: %v", err)
	}
	for _, r := range results {
		if r.Project != "project-a" {
			t.Errorf("project filter returned project %q, want %q", r.Project, "project-a")
		}
	}
	if len(results) == 0 {
		t.Error("expected at least 1 result for project-a")
	}
}

// TestSearchMemoriesFiltered_LimitHonored verifies that the limit parameter
// is respected.
func TestSearchMemoriesFiltered_LimitHonored(t *testing.T) {
	s := openTempStore(t)

	for i := 0; i < 5; i++ {
		syncID := "sf-lim-" + string(rune('a'+i))
		seedObservation(t, s, syncID, "limit test memory", "content for limit test", "limitproj", "manual", "project")
	}

	results, err := s.SearchMemoriesFiltered("limit test", "limitproj", 3, SearchFilter{})
	if err != nil {
		t.Fatalf("limit filter: %v", err)
	}
	if len(results) > 3 {
		t.Errorf("limit=3 returned %d results", len(results))
	}
}

// TestSearchMemoriesFiltered_FTSMatchesTitleAndContent verifies that FTS search
// matches in both title and content fields.
func TestSearchMemoriesFiltered_FTSMatchesTitleAndContent(t *testing.T) {
	s := openTempStore(t)

	seedObservation(t, s, "sf-title", "hexagonal match in title", "other content", "fts-test", "manual", "project")
	seedObservation(t, s, "sf-content", "other title", "hexagonal match in content body", "fts-test", "manual", "project")

	results, err := s.SearchMemoriesFiltered("hexagonal", "fts-test", 10, SearchFilter{})
	if err != nil {
		t.Fatalf("FTS title+content: %v", err)
	}
	found := map[string]bool{}
	for _, r := range results {
		found[r.SyncID] = true
	}
	if !found["sf-title"] {
		t.Error("FTS did not match title row")
	}
	if !found["sf-content"] {
		t.Error("FTS did not match content row")
	}
}

// TestSearchMemoriesFiltered_ExcludesDeleted verifies that soft-deleted rows
// are excluded from search results.
func TestSearchMemoriesFiltered_ExcludesDeleted(t *testing.T) {
	s := openTempStore(t)

	seedObservation(t, s, "sf-del-live", "deleted test memory", "live content", "deltest", "manual", "project")
	seedObservation(t, s, "sf-del-gone", "deleted test memory", "gone content", "deltest", "manual", "project")
	softDelete(t, s, "sf-del-gone")

	results, err := s.SearchMemoriesFiltered("deleted test memory", "deltest", 10, SearchFilter{})
	if err != nil {
		t.Fatalf("deleted exclusion: %v", err)
	}
	for _, r := range results {
		if r.SyncID == "sf-del-gone" {
			t.Error("soft-deleted row appeared in search results")
		}
	}
}

// TestSearchMemoriesFiltered_CombinedFilters verifies that multiple filters
// (type + scope) are applied together as AND conditions.
func TestSearchMemoriesFiltered_CombinedFilters(t *testing.T) {
	s := openTempStore(t)

	// Only this one matches type=architecture AND scope=personal.
	seedObservation(t, s, "sf-combo-1", "combined filter match", "arch decision personal", "comboproject", "architecture", "personal")
	// Wrong type.
	seedObservation(t, s, "sf-combo-2", "combined filter match", "bugfix personal", "comboproject", "bugfix", "personal")
	// Wrong scope.
	seedObservation(t, s, "sf-combo-3", "combined filter match", "arch project scoped", "comboproject", "architecture", "project")

	results, err := s.SearchMemoriesFiltered("combined filter", "comboproject", 10, SearchFilter{
		Type:  "architecture",
		Scope: "personal",
	})
	if err != nil {
		t.Fatalf("combined filters: %v", err)
	}
	for _, r := range results {
		if r.SyncID != "sf-combo-1" {
			t.Errorf("combined filter returned unexpected row %q (type=%q scope=%q)", r.SyncID, r.Type, r.Scope)
		}
	}
	if len(results) != 1 {
		t.Errorf("combined filter: expected 1 result, got %d", len(results))
	}
}

// ── RecentObservations ────────────────────────────────────────────────────────

// TestRecentObservations_OrderNewestFirst verifies that results are returned
// newest-first (created_at DESC, id DESC).
func TestRecentObservations_OrderNewestFirst(t *testing.T) {
	s := openTempStore(t)

	// Insert in order; let created_at be auto-assigned (datetime('now')).
	// We test order by id DESC as a tie-breaker for same-second inserts.
	seedObservation(t, s, "ro-first", "first memory", "content", "ordproj", "manual", "project")
	seedObservation(t, s, "ro-second", "second memory", "content", "ordproj", "manual", "project")
	seedObservation(t, s, "ro-third", "third memory", "content", "ordproj", "manual", "project")

	results, err := s.RecentObservations("ordproj", "", 10)
	if err != nil {
		t.Fatalf("RecentObservations: %v", err)
	}
	if len(results) < 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	// Newest inserts get the highest id, so they appear first.
	if results[0].SyncID != "ro-third" {
		t.Errorf("first result should be newest (ro-third), got %q", results[0].SyncID)
	}
	if results[len(results)-1].SyncID == "ro-third" {
		t.Error("oldest should be last, not first")
	}
}

// TestRecentObservations_LimitRespected verifies the limit parameter caps the
// result set.
func TestRecentObservations_LimitRespected(t *testing.T) {
	s := openTempStore(t)

	for i := 0; i < 6; i++ {
		syncID := "ro-lim-" + string(rune('a'+i))
		seedObservation(t, s, syncID, "limit obs", "content", "limproj", "manual", "project")
	}

	results, err := s.RecentObservations("limproj", "", 4)
	if err != nil {
		t.Fatalf("RecentObservations limit: %v", err)
	}
	if len(results) > 4 {
		t.Errorf("limit=4 returned %d results", len(results))
	}
}

// TestRecentObservations_ProjectFilter verifies the project filter.
func TestRecentObservations_ProjectFilter(t *testing.T) {
	s := openTempStore(t)

	seedObservation(t, s, "ro-pa-1", "project-a obs", "content", "project-a", "manual", "project")
	seedObservation(t, s, "ro-pb-1", "project-b obs", "content", "project-b", "manual", "project")

	results, err := s.RecentObservations("project-a", "", 10)
	if err != nil {
		t.Fatalf("RecentObservations project filter: %v", err)
	}
	for _, r := range results {
		if r.Project != "project-a" {
			t.Errorf("project filter returned project %q, want %q", r.Project, "project-a")
		}
	}
	if len(results) == 0 {
		t.Error("expected at least 1 result for project-a")
	}
}

// TestRecentObservations_ExcludesDeleted verifies that soft-deleted rows are
// not returned.
func TestRecentObservations_ExcludesDeleted(t *testing.T) {
	s := openTempStore(t)

	seedObservation(t, s, "ro-del-live", "live obs", "content", "delproj2", "manual", "project")
	seedObservation(t, s, "ro-del-gone", "deleted obs", "content", "delproj2", "manual", "project")
	softDelete(t, s, "ro-del-gone")

	results, err := s.RecentObservations("delproj2", "", 10)
	if err != nil {
		t.Fatalf("RecentObservations deleted exclusion: %v", err)
	}
	for _, r := range results {
		if r.SyncID == "ro-del-gone" {
			t.Error("soft-deleted row appeared in RecentObservations")
		}
	}
	found := false
	for _, r := range results {
		if r.SyncID == "ro-del-live" {
			found = true
		}
	}
	if !found {
		t.Error("live row should appear in RecentObservations")
	}
}

// TestRecentObservations_DefaultLimit verifies that limit <= 0 defaults to a
// sensible non-zero value (i.e., does not return zero results when data exists).
func TestRecentObservations_DefaultLimit(t *testing.T) {
	s := openTempStore(t)
	seedObservation(t, s, "ro-def-1", "default limit test", "content", "deflimproj", "manual", "project")

	results, err := s.RecentObservations("deflimproj", "", 0)
	if err != nil {
		t.Fatalf("RecentObservations default limit: %v", err)
	}
	if len(results) == 0 {
		t.Error("default limit (0) should return results, not zero rows")
	}
}

// ── FormatContext ─────────────────────────────────────────────────────────────

// TestFormatContext_ContainsSectionsWhenDataExists verifies the top-level
// structure of the context blob when sessions and observations exist.
func TestFormatContext_ContainsSectionsWhenDataExists(t *testing.T) {
	s := openTempStore(t)

	// Seed a session.
	if err := s.CreateSession("fc-sess-1", "fc-project", "/path"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Seed an observation.
	seedObservation(t, s, "fc-obs-1", "format context test title", "format context test content", "fc-project", "decision", "project")

	ctx, err := s.FormatContext("fc-project", "")
	if err != nil {
		t.Fatalf("FormatContext: %v", err)
	}
	if ctx == "" {
		t.Fatal("FormatContext returned empty string — expected non-empty context")
	}
	if !strings.Contains(ctx, "## Memory from Previous Sessions") {
		t.Error("FormatContext missing header '## Memory from Previous Sessions'")
	}
	if !strings.Contains(ctx, "### Recent Sessions") {
		t.Error("FormatContext missing '### Recent Sessions' section")
	}
	if !strings.Contains(ctx, "### Recent Observations") {
		t.Error("FormatContext missing '### Recent Observations' section")
	}
	if !strings.Contains(ctx, "fc-project") {
		t.Error("FormatContext does not mention the project name in session list")
	}
}

// TestFormatContext_ContainsObservationTitle verifies that observation titles
// appear in the formatted output.
func TestFormatContext_ContainsObservationTitle(t *testing.T) {
	s := openTempStore(t)

	if err := s.CreateSession("fc-sess-2", "obscheck", "/path"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	seedObservation(t, s, "fc-obs-title-1", "unique observation title xyz", "some content", "obscheck", "bugfix", "project")

	ctx, err := s.FormatContext("obscheck", "")
	if err != nil {
		t.Fatalf("FormatContext: %v", err)
	}
	if !strings.Contains(ctx, "unique observation title xyz") {
		t.Errorf("FormatContext does not contain observation title; got:\n%s", ctx)
	}
}

// TestFormatContext_EmptyWhenNoData verifies that FormatContext returns an
// empty string rather than a partial header when there are no sessions and
// no observations.
func TestFormatContext_EmptyWhenNoData(t *testing.T) {
	s := openTempStore(t)

	ctx, err := s.FormatContext("does-not-exist", "")
	if err != nil {
		t.Fatalf("FormatContext empty: %v", err)
	}
	if ctx != "" {
		t.Errorf("FormatContext should return empty string for no-data project, got:\n%s", ctx)
	}
}

// TestFormatContext_ScopeFilter verifies that the scope parameter filters the
// observations section. A "personal" scope query should not surface project-
// scoped observations (when only personal ones exist).
func TestFormatContext_ScopeFilter(t *testing.T) {
	s := openTempStore(t)

	if err := s.CreateSession("fc-scope-sess", "scopeproj", "/path"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	seedObservation(t, s, "fc-scope-pers", "personal scoped obs", "personal note", "scopeproj", "manual", "personal")
	seedObservation(t, s, "fc-scope-proj", "project scoped obs", "project note", "scopeproj", "manual", "project")

	// Request personal scope — should see personal obs, not project obs.
	ctx, err := s.FormatContext("scopeproj", "personal")
	if err != nil {
		t.Fatalf("FormatContext scope=personal: %v", err)
	}
	if strings.Contains(ctx, "project scoped obs") {
		t.Error("FormatContext with scope=personal should not include project-scoped observation")
	}
	if !strings.Contains(ctx, "personal scoped obs") {
		t.Error("FormatContext with scope=personal should include personal-scoped observation")
	}
}
