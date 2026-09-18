package localstore

import (
	"strings"
	"testing"
	"time"
)

// seedSessionAt registers a session and backdates its started_at, so a test can
// state the ordering it is asserting instead of racing the wall clock.
func seedSessionAt(t *testing.T, s *Store, id, project string, startedAt time.Time) {
	t.Helper()
	if err := s.CreateSessionWithProject(id, project, ""); err != nil {
		t.Fatalf("CreateSessionWithProject(%q): %v", id, err)
	}
	if _, err := s.DB().Exec(
		`UPDATE sessions SET started_at = ? WHERE id = ?`,
		startedAt.UTC().Format("2006-01-02 15:04:05"), id,
	); err != nil {
		t.Fatalf("backdate started_at for %q: %v", id, err)
	}
}

// seedMemoryAt adds an observation to a session and stamps its created_at.
// Returns the sync_id so a caller can soft-delete it.
func seedMemoryAt(t *testing.T, s *Store, sessionID, project, title string, createdAt time.Time) string {
	t.Helper()
	res, err := s.AddObservation(AddObservationParams{
		SessionID: sessionID,
		Title:     title,
		Content:   title,
		Project:   project,
	})
	if err != nil {
		t.Fatalf("AddObservation(%q): %v", title, err)
	}
	if _, err := s.DB().Exec(
		`UPDATE memories SET created_at = ? WHERE sync_id = ?`,
		createdAt.UTC().Format("2006-01-02 15:04:05"), res.SyncID,
	); err != nil {
		t.Fatalf("backdate created_at for %q: %v", title, err)
	}
	return res.SyncID
}

func sessionOrder(t *testing.T, s *Store, project string, limit int) []string {
	t.Helper()
	sessions, err := s.RecentSessions(project, limit)
	if err != nil {
		t.Fatalf("RecentSessions: %v", err)
	}
	ids := make([]string, len(sessions))
	for i, sess := range sessions {
		ids[i] = sess.ID
	}
	return ids
}

// TestRecentSessions_OrdersByLastActivityNotStartTime is the regression test for
// the ordering this commit fixes: a session registered an hour ago and never
// used outranked a session opened last week that has been saving memories all
// morning. "Recent" has to mean recently WORKED IN — the idle one carries no
// context worth restoring.
func TestRecentSessions_OrdersByLastActivityNotStartTime(t *testing.T) {
	s := openTempStore(t)
	const project = "activity"

	// Started first, but touched most recently.
	seedSessionAt(t, s, "old-busy", project, time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC))
	seedMemoryAt(t, s, "old-busy", project, "latest work", time.Date(2024, 6, 10, 14, 0, 0, 0, time.UTC))

	// Started later, then nothing ever happened in it.
	seedSessionAt(t, s, "new-idle", project, time.Date(2024, 5, 1, 9, 0, 0, 0, time.UTC))

	got := sessionOrder(t, s, project, 5)
	want := []string{"old-busy", "new-idle"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("RecentSessions order = %v, want %v (old-busy has a memory from 2024-06-10; "+
			"new-idle only started 2024-05-01)", got, want)
	}

	sessions, err := s.RecentSessions(project, 5)
	if err != nil {
		t.Fatalf("RecentSessions: %v", err)
	}
	if wantActivity := time.Date(2024, 6, 10, 14, 0, 0, 0, time.UTC); !sessions[0].LastActivityAt.Equal(wantActivity) {
		t.Errorf("old-busy LastActivityAt = %s, want %s",
			sessions[0].LastActivityAt.Format(time.RFC3339), wantActivity.Format(time.RFC3339))
	}
	if !sessions[1].LastActivityAt.Equal(sessions[1].StartedAt) {
		t.Errorf("new-idle LastActivityAt = %s, want it to fall back to started_at %s",
			sessions[1].LastActivityAt.Format(time.RFC3339), sessions[1].StartedAt.Format(time.RFC3339))
	}
}

// TestRecentSessions_EndedAtCountsAsActivity covers the close-out case: a
// session whose only late event is mem_session_end. ended_at is written
// together with the summary, so it is the timestamp that says "this session was
// wrapped up at T" and it has to count.
func TestRecentSessions_EndedAtCountsAsActivity(t *testing.T) {
	s := openTempStore(t)
	const project = "activity"

	seedSessionAt(t, s, "closed-late", project, time.Date(2024, 1, 2, 9, 0, 0, 0, time.UTC))
	if err := s.EndSessionAt("closed-late", "wrapped up", time.Date(2024, 7, 1, 18, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("EndSessionAt: %v", err)
	}

	seedSessionAt(t, s, "started-later", project, time.Date(2024, 3, 1, 9, 0, 0, 0, time.UTC))

	got := sessionOrder(t, s, project, 5)
	if len(got) != 2 || got[0] != "closed-late" {
		t.Fatalf("RecentSessions order = %v, want closed-late first (ended 2024-07-01)", got)
	}
}

// TestRecentSessions_IgnoresDeletedMemories pins the deleted_at guard: a
// soft-deleted memory must not keep a session artificially "recent". The row is
// gone from every read surface; it cannot be the thing that ranks a session.
func TestRecentSessions_IgnoresDeletedMemories(t *testing.T) {
	s := openTempStore(t)
	const project = "activity"

	seedSessionAt(t, s, "ghost", project, time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC))
	syncID := seedMemoryAt(t, s, "ghost", project, "deleted work", time.Date(2024, 6, 10, 14, 0, 0, 0, time.UTC))
	if _, err := s.DB().Exec(
		`UPDATE memories SET deleted_at = datetime('now') WHERE sync_id = ?`, syncID,
	); err != nil {
		t.Fatalf("soft-delete memory: %v", err)
	}

	seedSessionAt(t, s, "live", project, time.Date(2024, 5, 1, 9, 0, 0, 0, time.UTC))

	got := sessionOrder(t, s, project, 5)
	if len(got) != 2 || got[0] != "live" {
		t.Fatalf("RecentSessions order = %v, want live first — a soft-deleted memory must not count as activity", got)
	}
}

// TestFormatContext_ShowsLastActivityTimestamp asserts the rendered bullet
// carries the derived stamp. Without it the ordering looks like a sorting bug:
// the reader sees an older start date at the top and has no way to tell why.
func TestFormatContext_ShowsLastActivityTimestamp(t *testing.T) {
	s := openTempStore(t)
	const project = "activity"

	seedSessionAt(t, s, "busy", project, time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC))
	seedMemoryAt(t, s, "busy", project, "latest work", time.Date(2024, 6, 10, 14, 0, 0, 0, time.UTC))

	ctx, err := s.FormatContext(project, "")
	if err != nil {
		t.Fatalf("FormatContext: %v", err)
	}
	if !strings.Contains(ctx, "2024-01-01 09:00:00 → 2024-06-10 14:00:00") {
		t.Errorf("FormatContext session bullet does not show the started → last-activity span; got:\n%s", ctx)
	}

	// An idle session renders exactly as it always did: one timestamp, no arrow.
	s2 := openTempStore(t)
	seedSessionAt(t, s2, "idle", project, time.Date(2024, 2, 2, 10, 0, 0, 0, time.UTC))
	seedMemoryAt(t, s2, "other", project, "unrelated", time.Date(2024, 2, 2, 10, 0, 0, 0, time.UTC))

	ctx2, err := s2.FormatContext(project, "")
	if err != nil {
		t.Fatalf("FormatContext (idle): %v", err)
	}
	for _, line := range strings.Split(ctx2, "\n") {
		if strings.HasPrefix(line, "- **"+project+"**") && strings.Contains(line, "→") {
			t.Errorf("idle session bullet should carry a single timestamp, got: %s", line)
		}
	}
}
