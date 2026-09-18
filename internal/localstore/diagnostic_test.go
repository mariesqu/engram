package localstore

import (
	"context"
	"testing"
	"time"
)

// These tests cover the read-only queries behind mem_doctor. The checks that
// consume them are unit-tested against fakes (internal/diagnostic); what can
// only be tested here is that the SQL says what the check thinks it says.

// TestDiagnosticSessions_ReportsOpenAndClosed pins the one field every
// session-based check branches on: EndedAt is nil for an OPEN session, and a
// closed one carries its timestamp.
func TestDiagnosticSessions_ReportsOpenAndClosed(t *testing.T) {
	s := openTempStore(t)

	if err := s.CreateSession("open-1", "engram", "/repos/engram"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.CreateSession("closed-1", "engram", "/repos/engram"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.EndSession("closed-1", "done"); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if err := s.CreateSession("other", "elsewhere", "/repos/other"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	all, err := s.DiagnosticSessions("")
	if err != nil {
		t.Fatalf("DiagnosticSessions: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("sessions = %d, want 3 (empty project means every project)", len(all))
	}

	scoped, err := s.DiagnosticSessions("engram")
	if err != nil {
		t.Fatalf("DiagnosticSessions(engram): %v", err)
	}
	if len(scoped) != 2 {
		t.Fatalf("scoped sessions = %d, want 2", len(scoped))
	}
	byID := map[string]DiagnosticSession{}
	for _, sess := range scoped {
		byID[sess.ID] = sess
	}
	if byID["open-1"].EndedAt != nil {
		t.Errorf("open-1 reports ended_at %v, want nil", byID["open-1"].EndedAt)
	}
	if byID["closed-1"].EndedAt == nil {
		t.Error("closed-1 reports no ended_at; the ambiguity check would count it as still open")
	}
	if byID["open-1"].Directory != "/repos/engram" {
		t.Errorf("directory = %q, want it preserved", byID["open-1"].Directory)
	}
	if byID["open-1"].StartedAt.IsZero() {
		t.Error("started_at was not parsed")
	}
}

// TestOrphanedObservationSessions_FindsUnmatchedIDs is the query behind the
// orphan check. The FK was deliberately removed from this schema (an
// out-of-order sync pull can land an observation before its session), so
// nothing but this query can tell you it happened.
func TestOrphanedObservationSessions_FindsUnmatchedIDs(t *testing.T) {
	s := openTempStore(t)

	if err := s.CreateSession("real", "engram", "/repos/engram"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	for _, p := range []AddObservationParams{
		{SessionID: "real", Title: "anchored", Project: "engram"},
		{SessionID: "ghost", Title: "orphan one", Project: "engram"},
		{SessionID: "ghost", Title: "orphan two", Project: "engram"},
		{SessionID: "ghost-2", Title: "other project orphan", Project: "elsewhere"},
	} {
		if _, err := s.AddObservation(p); err != nil {
			t.Fatalf("AddObservation(%q): %v", p.Title, err)
		}
	}

	refs, err := s.OrphanedObservationSessions("")
	if err != nil {
		t.Fatalf("OrphanedObservationSessions: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("orphans = %+v, want 2 session ids", refs)
	}
	if refs[0].SessionID != "ghost" || refs[0].ObservationCount != 2 {
		t.Errorf("heaviest orphan = %+v, want ghost with 2 observations first", refs[0])
	}
	if len(refs[0].Projects) != 1 || refs[0].Projects[0] != "engram" {
		t.Errorf("projects = %v, want the owning project", refs[0].Projects)
	}

	scoped, err := s.OrphanedObservationSessions("engram")
	if err != nil {
		t.Fatalf("OrphanedObservationSessions(engram): %v", err)
	}
	if len(scoped) != 1 || scoped[0].SessionID != "ghost" {
		t.Errorf("scoped orphans = %+v, want only ghost", scoped)
	}
}

// TestOrphanedObservationSessions_IgnoresDeletedRows — a soft-deleted
// observation is not a live reference, and reporting one would send someone
// hunting for a session behind a memory that no longer exists.
func TestOrphanedObservationSessions_IgnoresDeletedRows(t *testing.T) {
	s := openTempStore(t)

	res, err := s.AddObservation(AddObservationParams{SessionID: "ghost", Title: "gone soon", Project: "engram"})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if err := s.DeleteMemory(res.ID, "writer-1"); err != nil {
		t.Fatalf("DeleteMemory: %v", err)
	}

	refs, err := s.OrphanedObservationSessions("")
	if err != nil {
		t.Fatalf("OrphanedObservationSessions: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("orphans = %+v, want none: the only referencing row is deleted", refs)
	}
}

// TestProjectsWithoutPolicy_ListsOnlyUnpinnedProjects backs the
// project_policy_unknown check: a project with an explicit row has been decided
// on, whatever the decision was.
func TestProjectsWithoutPolicy_ListsOnlyUnpinnedProjects(t *testing.T) {
	s := openTempStore(t)

	for _, project := range []string{"decided", "undecided-a", "undecided-b"} {
		if _, err := s.AddObservation(AddObservationParams{
			SessionID: "sess", Title: "note for " + project, Project: project,
		}); err != nil {
			t.Fatalf("AddObservation(%s): %v", project, err)
		}
	}
	if err := s.SetPolicy("decided", PolicyLocalOnly); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	got, err := s.ProjectsWithoutPolicy("")
	if err != nil {
		t.Fatalf("ProjectsWithoutPolicy: %v", err)
	}
	if len(got) != 2 || got[0] != "undecided-a" || got[1] != "undecided-b" {
		t.Errorf("projects = %v, want the two undecided ones, sorted", got)
	}

	scoped, err := s.ProjectsWithoutPolicy("undecided-a")
	if err != nil {
		t.Fatalf("ProjectsWithoutPolicy(undecided-a): %v", err)
	}
	if len(scoped) != 1 || scoped[0] != "undecided-a" {
		t.Errorf("scoped = %v, want only the scoped project", scoped)
	}
}

// TestCountByReviewStatus_MatchesListForReview is the anti-drift guard: the
// count and the listing must agree, because the status is computed in Go (from
// updated_at + the review window) rather than stored, and two implementations
// of one rule drift.
func TestCountByReviewStatus_MatchesListForReview(t *testing.T) {
	s := openTempStore(t)
	s.SetReviewWindowDays(30)

	for i, title := range []string{"fresh one", "fresh two", "stale one"} {
		res, err := s.AddObservation(AddObservationParams{
			SessionID: "sess", Title: title, Project: "engram",
		})
		if err != nil {
			t.Fatalf("AddObservation: %v", err)
		}
		if i == 2 {
			// Age it past the window. updated_at is the only input to the default
			// due date, so backdating it is what makes the row need review.
			old := time.Now().UTC().AddDate(0, 0, -60).Format("2006-01-02 15:04:05")
			if _, err := s.db.Exec(`UPDATE memories SET updated_at = ? WHERE id = ?`, old, res.ID); err != nil {
				t.Fatalf("backdate: %v", err)
			}
		}
	}

	counts, err := s.CountByReviewStatus("engram")
	if err != nil {
		t.Fatalf("CountByReviewStatus: %v", err)
	}
	if counts.Total != 3 {
		t.Errorf("total = %d, want 3", counts.Total)
	}
	if counts.NeedsReview != 1 || counts.Active != 2 {
		t.Errorf("counts = %+v, want 1 needs_review and 2 active", counts)
	}

	listed, err := s.ListForReview(ReviewStatusNeedsReview, "engram", 50)
	if err != nil {
		t.Fatalf("ListForReview: %v", err)
	}
	if len(listed) != counts.NeedsReview {
		t.Errorf("ListForReview returned %d rows but CountByReviewStatus counted %d — the two disagree",
			len(listed), counts.NeedsReview)
	}
}

// TestSyncBacklog_CountsUnackedMutations backs the sync_backlog check. Every
// local write enqueues an outbox row, so a fresh store with one observation has
// a backlog of exactly one.
func TestSyncBacklog_CountsUnackedMutations(t *testing.T) {
	s := openTempStore(t)

	if backlog, err := s.SyncBacklog(); err != nil {
		t.Fatalf("SyncBacklog: %v", err)
	} else if backlog.Pending != 0 || !backlog.Oldest.IsZero() {
		t.Errorf("empty store backlog = %+v, want zero", backlog)
	}

	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "sess", Title: "queued", Project: "engram",
	}); err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	backlog, err := s.SyncBacklog()
	if err != nil {
		t.Fatalf("SyncBacklog: %v", err)
	}
	if backlog.Pending != 1 {
		t.Errorf("pending = %d, want 1", backlog.Pending)
	}
	if backlog.Oldest.IsZero() {
		t.Error("oldest_occurred_at is zero; the age-based severity would never fire")
	}

	// Acking is what drains it — the check must see the drain, not just the fill.
	entries, err := s.DrainOutbox(10)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	for _, e := range entries {
		if err := s.AckMutation(e.LocalSeq); err != nil {
			t.Fatalf("AckMutation: %v", err)
		}
	}
	if backlog, err = s.SyncBacklog(); err != nil {
		t.Fatalf("SyncBacklog: %v", err)
	} else if backlog.Pending != 0 {
		t.Errorf("pending after ack = %d, want 0", backlog.Pending)
	}
}

// TestSQLiteLockSnapshot_ReadsPragmas covers the probe itself. The values are
// the store's own connection settings, so the assertion is that they were READ
// (a busy_timeout the check would flag as disabled must not come from a query
// that silently returned zero).
func TestSQLiteLockSnapshot_ReadsPragmas(t *testing.T) {
	s := openTempStore(t)

	snap, err := s.SQLiteLockSnapshot(context.Background())
	if err != nil {
		t.Fatalf("SQLiteLockSnapshot: %v", err)
	}
	if snap.BusyTimeoutMS <= 0 {
		t.Errorf("busy_timeout = %d, want the store's configured timeout — a writer that meets a lock would fail instantly",
			snap.BusyTimeoutMS)
	}
	if snap.CheckpointBusy != 0 {
		t.Errorf("checkpoint_busy = %d on an idle store, want 0", snap.CheckpointBusy)
	}
}

// TestCentralConfigured_FollowsTheInjectedFn — several checks are meaningful
// only on one side of this line (an unpushed outbox is a backlog with central
// configured and simply unused without it).
func TestCentralConfigured_FollowsTheInjectedFn(t *testing.T) {
	s := openTempStore(t)

	if s.CentralConfigured() {
		t.Error("a store with no injected fn must report not-configured")
	}
	s.SetCentralConfiguredFn(func() bool { return true })
	if !s.CentralConfigured() {
		t.Error("CentralConfigured ignored the injected fn")
	}
	s.SetCentralConfiguredFn(func() bool { return false })
	if s.CentralConfigured() {
		t.Error("CentralConfigured ignored a fn that reports false")
	}
}

// TestOrphanedObservationSessions_ExcludesTheManualSaveDefault splits the two
// conditions that used to look identical in SQL. A save with no session_id is
// filed under the store's OWN default ("manual-save-{project}"), which nothing
// ever registers — so it matched the orphan query and produced a permanent
// warning about documented behaviour.
func TestOrphanedObservationSessions_ExcludesTheManualSaveDefault(t *testing.T) {
	s := openTempStore(t)

	for _, p := range []AddObservationParams{
		{SessionID: "ghost", Title: "a real orphan", Project: "engram"},
		{SessionID: DefaultManualSessionID("engram"), Title: "a plain mem_save", Project: "engram"},
		{SessionID: DefaultManualSessionID("engram"), Title: "another plain mem_save", Project: "engram"},
	} {
		if _, err := s.AddObservation(p); err != nil {
			t.Fatalf("AddObservation(%q): %v", p.Title, err)
		}
	}

	refs, err := s.OrphanedObservationSessions("")
	if err != nil {
		t.Fatalf("OrphanedObservationSessions: %v", err)
	}
	if len(refs) != 1 || refs[0].SessionID != "ghost" {
		t.Fatalf("orphans = %+v, want only the genuinely orphaned session", refs)
	}

	manual, err := s.UnregisteredSessionSaves("")
	if err != nil {
		t.Fatalf("UnregisteredSessionSaves: %v", err)
	}
	if len(manual) != 1 {
		t.Fatalf("unregistered = %+v, want one session id", manual)
	}
	if manual[0].SessionID != DefaultManualSessionID("engram") || manual[0].ObservationCount != 2 {
		t.Errorf("unregistered = %+v, want the manual-save id with 2 observations", manual[0])
	}
}

// TestUnregisteredSessionSaves_IgnoresARegisteredManualSession — the split is
// by session id, but the \"has no session row\" condition still applies. Someone
// who registers "manual-save-engram" deliberately has a session like any other,
// and neither query should report it.
func TestUnregisteredSessionSaves_IgnoresARegisteredManualSession(t *testing.T) {
	s := openTempStore(t)

	id := DefaultManualSessionID("engram")
	if err := s.CreateSession(id, "engram", "/repos/engram"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.AddObservation(AddObservationParams{SessionID: id, Title: "anchored", Project: "engram"}); err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	manual, err := s.UnregisteredSessionSaves("")
	if err != nil {
		t.Fatalf("UnregisteredSessionSaves: %v", err)
	}
	if len(manual) != 0 {
		t.Errorf("unregistered = %+v, want none: that session exists", manual)
	}
}

// TestOrphanedObservationSessions_ProjectNamesWithCommasSurvive is the reason
// the projects column is json_group_array and not GROUP_CONCAT. GROUP_CONCAT
// joins on a comma and the caller split on one, so a project whose NAME
// contains a comma came back as two projects, neither of which exists —
// evidence that reads like a second problem to chase.
func TestOrphanedObservationSessions_ProjectNamesWithCommasSurvive(t *testing.T) {
	s := openTempStore(t)

	const project = "acme, inc"
	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "ghost", Title: "orphan", Project: project,
	}); err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	refs, err := s.OrphanedObservationSessions("")
	if err != nil {
		t.Fatalf("OrphanedObservationSessions: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("orphans = %+v, want 1", refs)
	}
	if len(refs[0].Projects) != 1 {
		t.Fatalf("projects = %q, want ONE project — the name was split on its own comma", refs[0].Projects)
	}
	if refs[0].Projects[0] != normalizeProject(project) {
		t.Errorf("project = %q, want %q", refs[0].Projects[0], normalizeProject(project))
	}
}
