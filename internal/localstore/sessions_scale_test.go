package localstore

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openTempBenchStore is openTempStore for a testing.TB — benchmarks need the
// same throwaway store tests get, and openTempStore's *testing.T signature
// cannot serve both.
func openTempBenchStore(tb testing.TB) *Store {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "scale.db")
	s, err := Open(path)
	if err != nil {
		tb.Fatalf("Open(%q): %v", path, err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	return s
}

// seedSessionScaleFixture writes sessionCount sessions and memoryCount memories
// (spread evenly across those sessions) with raw SQL inside ONE transaction.
//
// It deliberately bypasses CreateSessionWithProject/AddObservation: this fixture
// exists to measure the SHAPE of the RecentSessions query at a realistic corpus
// size, and routing five thousand rows through the write queue would measure the
// write path instead — slowly enough that the fixture, not the query, would be
// what the test times out on.
func seedSessionScaleFixture(tb testing.TB, s *Store, project string, sessionCount, memoryCount int) {
	tb.Helper()

	base := time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC)

	tx, err := s.DB().Begin()
	if err != nil {
		tb.Fatalf("begin scale fixture: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	sessStmt, err := tx.Prepare(
		`INSERT INTO sessions (id, project, directory, started_at) VALUES (?, ?, '', ?)`)
	if err != nil {
		tb.Fatalf("prepare session insert: %v", err)
	}
	defer sessStmt.Close()

	for i := 0; i < sessionCount; i++ {
		startedAt := base.Add(time.Duration(i) * time.Hour).Format(sqliteTimeLayout)
		if _, err := sessStmt.Exec(fmt.Sprintf("sess-%04d", i), project, startedAt); err != nil {
			tb.Fatalf("insert session %d: %v", i, err)
		}
	}

	memStmt, err := tx.Prepare(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content,
		   project, scope, writer_id, created_at, updated_at)
		VALUES (?, ?, 'memory', 'manual', ?, ?, ?, 'project', 'w', ?, ?)`)
	if err != nil {
		tb.Fatalf("prepare memory insert: %v", err)
	}
	defer memStmt.Close()

	for i := 0; i < memoryCount; i++ {
		sessionID := fmt.Sprintf("sess-%04d", i%sessionCount)
		createdAt := base.Add(time.Duration(i) * time.Minute).Format(sqliteTimeLayout)
		title := fmt.Sprintf("scale memory %d", i)
		if _, err := memStmt.Exec(
			fmt.Sprintf("scale-%06d", i), sessionID, title, "alpha content", project,
			createdAt, createdAt,
		); err != nil {
			tb.Fatalf("insert memory %d: %v", i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit scale fixture: %v", err)
	}
}

// explainRecentSessions returns SQLite's query plan for the EXACT statement
// RecentSessions ships, one detail line per plan row.
func explainRecentSessions(tb testing.TB, s *Store, project string, limit int) []string {
	tb.Helper()

	query, args := recentSessionsQuery(project, limit)
	rows, err := s.DB().Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		tb.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			tb.Fatalf("scan plan row: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		tb.Fatalf("plan rows: %v", err)
	}
	return plan
}

// TestRecentSessions_ScalesToThreeHundredSessions is the regression test for the
// correlated-subquery ordering the derived-table rewrite replaced. The old query
// evaluated a MAX(created_at) subquery over memories once PER SESSION ROW — an
// O(sessions × memories) scan that measured 1.39s on this fixture shape and
// dragged every mem_context call down with it (FormatContext calls RecentSessions
// first, then counts observations per returned session).
//
// The assertion that actually guards the fix is the QUERY PLAN, not the clock: a
// correlated subquery is a SHAPE, and SQLite names it in the plan
// ("CORRELATED SCALAR SUBQUERY"). Asserting on the shape fails for the right
// reason on any machine, where a wall-clock threshold tight enough to separate
// the two shapes is also tight enough to flake on a loaded CI runner. The 1s
// budget below is a deliberately generous BACKSTOP — six times the worst number
// the broken shape produced here is 1.58s, so a regression that somehow keeps
// the plan clean still cannot hide — not a performance target.
//
// The fixture is the one the slowdown was measured on (300 sessions, 20k
// memories) rather than the benchmark's 5k, and both halves of the hot path are
// timed: RecentSessions alone and FormatContext, which is what mem_context
// actually calls.
func TestRecentSessions_ScalesToThreeHundredSessions(t *testing.T) {
	s := openTempStore(t)
	const project = "scale"

	seedSessionScaleFixture(t, s, project, 300, 20000)

	// 1. Shape: no per-row correlated subquery, and the grouped derived table
	//    (aliased lm) is materialized once.
	plan := explainRecentSessions(t, s, project, 5)
	joined := strings.Join(plan, "\n")
	t.Logf("RecentSessions query plan:\n%s", joined)

	if strings.Contains(strings.ToUpper(joined), "CORRELATED") {
		t.Errorf("RecentSessions plan contains a correlated subquery — the per-session "+
			"MAX(created_at) is back:\n%s", joined)
	}
	derived := false
	for _, line := range plan {
		up := strings.ToUpper(line)
		// SQLite renders the grouped subquery as MATERIALIZE / CO-ROUTINE lm
		// depending on version; either is the one-pass shape this test wants.
		if strings.Contains(up, "MATERIALIZE") || strings.Contains(up, "CO-ROUTINE") {
			derived = true
			break
		}
		if strings.Contains(up, "SUBQUERY") {
			derived = true
			break
		}
	}
	if !derived {
		t.Errorf("RecentSessions plan shows no materialized derived table — the grouped "+
			"newest-memory-per-session pass is gone:\n%s", joined)
	}

	// 2. Backstop: the whole mem_context read path stays far away from the
	//    seconds the correlated form cost.
	const budget = time.Second

	start := time.Now()
	sessions, err := s.RecentSessions(project, 5)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RecentSessions: %v", err)
	}
	if len(sessions) != 5 {
		t.Fatalf("RecentSessions returned %d sessions, want 5", len(sessions))
	}
	t.Logf("RecentSessions over 300 sessions / 20000 memories took %s", elapsed)
	if elapsed > budget {
		t.Errorf("RecentSessions took %s over 300 sessions / 20000 memories, want < %s",
			elapsed, budget)
	}

	start = time.Now()
	blob, err := s.FormatContext(project, "")
	ctxElapsed := time.Since(start)
	if err != nil {
		t.Fatalf("FormatContext: %v", err)
	}
	if blob == "" {
		t.Fatal("FormatContext returned an empty blob over a seeded fixture")
	}
	t.Logf("FormatContext over 300 sessions / 20000 memories took %s", ctxElapsed)
	if ctxElapsed > budget {
		t.Errorf("FormatContext took %s over 300 sessions / 20000 memories, want < %s — "+
			"the per-session work RecentSessions feeds is unbounded again", ctxElapsed, budget)
	}
}

// BenchmarkRecentSessions_300Sessions5kMemories reports the per-call cost of the
// ordering query at the fixture size the test above guards, so a future change to
// the expression can be compared with a number rather than a hunch.
func BenchmarkRecentSessions_300Sessions5kMemories(b *testing.B) {
	s := openTempBenchStore(b)
	const project = "scale"

	seedSessionScaleFixture(b, s, project, 300, 5000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.RecentSessions(project, 5); err != nil {
			b.Fatalf("RecentSessions: %v", err)
		}
	}
}

// BenchmarkFormatContext_300Sessions5kMemories covers the surface an agent
// actually calls: FormatContext runs RecentSessions and then one observation
// COUNT per returned session.
func BenchmarkFormatContext_300Sessions5kMemories(b *testing.B) {
	s := openTempBenchStore(b)
	const project = "scale"

	seedSessionScaleFixture(b, s, project, 300, 5000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.FormatContext(project, ""); err != nil {
			b.Fatalf("FormatContext: %v", err)
		}
	}
}
