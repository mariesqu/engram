package localstore

import (
	"fmt"
	"path/filepath"
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

// TestRecentSessions_ScalesToThreeHundredSessions is the regression test for the
// correlated-subquery ordering this commit replaces. The old query evaluated a
// MAX(created_at) subquery over memories(session_id) once PER SESSION ROW — an
// O(sessions × memories) scan that measured 1.39s on this fixture shape and
// dragged every mem_context call down with it (FormatContext calls RecentSessions
// first). The derived-table join computes the same value in one grouped pass.
//
// The fixture is the one the slowdown was measured on (300 sessions, 20k
// memories) rather than the benchmark's 5k, because at 5k the two query shapes
// are only ~100× apart in absolute terms and a budget that separates them would
// be tight enough to flake on a loaded runner. The 200ms budget here is two
// orders of magnitude above what the fixed query needs and an order of magnitude
// below what the correlated form costs: it fails on a REGRESSION of query shape,
// not on a slow CI runner.
func TestRecentSessions_ScalesToThreeHundredSessions(t *testing.T) {
	s := openTempStore(t)
	const project = "scale"

	seedSessionScaleFixture(t, s, project, 300, 20000)

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

	if budget := 200 * time.Millisecond; elapsed > budget {
		t.Errorf("RecentSessions took %s over 300 sessions / 20000 memories, want < %s — "+
			"the per-session correlated subquery is back", elapsed, budget)
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
