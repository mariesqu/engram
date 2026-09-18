package diagnostic

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/localstore"
)

// These tests cover the doctor's checks one condition at a time. Two properties
// matter beyond "does it find the thing":
//
//   - a check that CANNOT answer must not take the report down with it. The
//     whole point of running diagnostics is the run that happens when something
//     is already wrong.
//   - a check must stay quiet about conditions it cannot actually prove. A
//     warning that fires on healthy state is a warning people learn to skip,
//     and then the real one goes unread too.

// fakeStore returns exactly the state a test puts in it, including errors,
// which is the only way to exercise the can't-answer paths.
type fakeStore struct {
	sessions      []localstore.DiagnosticSession
	sessionsErr   error
	sessionsCalls int
	orphans       []localstore.OrphanedSessionRef
	orphansErr    error
	unregistered  []localstore.OrphanedSessionRef
	unregErr      error
	noPolicy      []string
	noPolicyErr   error
	review        localstore.ReviewCounts
	reviewErr     error
	backlog       localstore.SyncBacklog
	backlogErr    error
	lock          localstore.SQLiteLockSnapshot
	lockErr       error
	central       bool
	lastProjectIn string
}

func (f *fakeStore) DiagnosticSessions(project string) ([]localstore.DiagnosticSession, error) {
	f.lastProjectIn = project
	f.sessionsCalls++
	return f.sessions, f.sessionsErr
}

// OrphanedSessions keeps the fake's two fields — the check reports the two
// classes very differently (a warning and a note), and a test that sets only one
// of them is saying exactly that.
func (f *fakeStore) OrphanedSessions(project string) (localstore.OrphanedSessions, error) {
	f.lastProjectIn = project
	if f.orphansErr != nil {
		return localstore.OrphanedSessions{}, f.orphansErr
	}
	if f.unregErr != nil {
		return localstore.OrphanedSessions{}, f.unregErr
	}
	return localstore.OrphanedSessions{Orphans: f.orphans, Unregistered: f.unregistered}, nil
}

func (f *fakeStore) ProjectsWithoutPolicy(project string) ([]string, error) {
	f.lastProjectIn = project
	return f.noPolicy, f.noPolicyErr
}

func (f *fakeStore) CountByReviewStatus(project string) (localstore.ReviewCounts, error) {
	f.lastProjectIn = project
	return f.review, f.reviewErr
}

func (f *fakeStore) SyncBacklog() (localstore.SyncBacklog, error) { return f.backlog, f.backlogErr }

func (f *fakeStore) SQLiteLockSnapshot(context.Context) (localstore.SQLiteLockSnapshot, error) {
	return f.lock, f.lockErr
}

func (f *fakeStore) CentralConfigured() bool { return f.central }

// healthyStore is a store with nothing wrong with it — the baseline every
// "must stay quiet" assertion runs against.
func healthyStore() *fakeStore {
	return &fakeStore{
		review: localstore.ReviewCounts{Total: 40, Active: 39, NeedsReview: 1},
		lock:   localstore.SQLiteLockSnapshot{BusyTimeoutMS: 5000},
	}
}

// runOne runs a single check and returns its result.
func runOne(t *testing.T, check Check, scope Scope) CheckResult {
	t.Helper()
	report := NewRunnerWithRegistry(NewRegistry(check)).RunAll(context.Background(), scope)
	if len(report.Checks) != 1 {
		t.Fatalf("expected exactly one check result, got %d", len(report.Checks))
	}
	return report.Checks[0]
}

// evidenceOf decodes a result's evidence blob.
func evidenceOf(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("evidence is not a JSON object (%v): %s", err, raw)
	}
	return out
}

// ─── orphaned_observation_session ────────────────────────────────────────────

func TestOrphanedObservationSessionCheck(t *testing.T) {
	store := healthyStore()
	store.orphans = []localstore.OrphanedSessionRef{
		{SessionID: "ghost-1", ObservationCount: 7, Projects: []string{"engram"}},
	}

	got := runOne(t, OrphanedObservationSessionCheck{}, Scope{Store: store, Project: "engram"})

	if got.Result != StatusWarning {
		t.Errorf("result = %q, want %q", got.Result, StatusWarning)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(got.Findings))
	}
	if !strings.Contains(got.Findings[0].Message, "ghost-1") {
		t.Errorf("message = %q, want it to name the missing session", got.Findings[0].Message)
	}
	if !got.Findings[0].RequiresConfirmation {
		t.Error("recovering an orphaned session is a judgement call; the finding must say so")
	}
	if ev := evidenceOf(t, got.Findings[0].Evidence); ev["observation_count"] != float64(7) {
		t.Errorf("evidence lost the observation count: %v", ev)
	}
}

func TestOrphanedObservationSessionCheck_CleanStoreIsSilent(t *testing.T) {
	got := runOne(t, OrphanedObservationSessionCheck{}, Scope{Store: healthyStore()})
	if got.Result != StatusOK {
		t.Errorf("result = %q, want %q on a clean store", got.Result, StatusOK)
	}
	if len(got.Findings) != 0 {
		t.Errorf("findings = %v, want none", got.Findings)
	}
}

// ─── session_project_directory_mismatch ──────────────────────────────────────

func TestSessionProjectDirectoryMismatchCheck(t *testing.T) {
	store := healthyStore()
	store.sessions = []localstore.DiagnosticSession{
		{ID: "s-1", Project: "old-name", Directory: "/repos/app"},
		{ID: "s-2", Project: "app", Directory: "/repos/app"},
	}
	scope := Scope{Store: store, DetectProject: func(dir string) (DetectedProject, bool) {
		return DetectedProject{Project: "app", Source: "git_root", Path: dir}, true
	}}

	got := runOne(t, SessionProjectDirectoryMismatchCheck{}, scope)

	if got.Result != StatusWarning {
		t.Fatalf("result = %q, want %q", got.Result, StatusWarning)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("findings = %d, want 1 (only s-1 disagrees)", len(got.Findings))
	}
	ev := evidenceOf(t, got.Findings[0].Evidence)
	if ev["session_id"] != "s-1" || ev["directory_project"] != "app" {
		t.Errorf("evidence = %v, want it to name the session and both projects", ev)
	}
	if !strings.Contains(got.Findings[0].SafeNextStep, "mem_merge_projects") {
		t.Errorf("safe_next_step = %q, want it to name the repair tools", got.Findings[0].SafeNextStep)
	}
}

// TestSessionProjectDirectoryMismatchCheck_UndetectableDirectoriesAreSkipped is
// the "stay quiet" half. A directory that no longer exists, or that only
// resolves to its own basename, is not evidence: comparing a guess against a
// stored name produces mismatches that mean nothing, and this check would then
// fire on every machine that has ever renamed a folder.
func TestSessionProjectDirectoryMismatchCheck_UndetectableDirectoriesAreSkipped(t *testing.T) {
	store := healthyStore()
	store.sessions = []localstore.DiagnosticSession{
		{ID: "s-1", Project: "anything", Directory: "/gone"},
		{ID: "s-2", Project: "anything", Directory: ""},
	}
	scope := Scope{Store: store, DetectProject: func(string) (DetectedProject, bool) {
		return DetectedProject{}, false
	}}

	got := runOne(t, SessionProjectDirectoryMismatchCheck{}, scope)

	if got.Result != StatusOK {
		t.Errorf("result = %q, want %q — an unresolvable directory proves nothing", got.Result, StatusOK)
	}
	if ev := evidenceOf(t, got.Evidence); ev["sessions_evaluated"] != float64(0) {
		t.Errorf("evidence = %v, want it to report that nothing could be evaluated", ev)
	}
}

// TestSessionProjectDirectoryMismatchCheck_CaseOnlyDifferenceIsNotAMismatch —
// the store normalizes project names, so "App" and "app" are the same project.
func TestSessionProjectDirectoryMismatchCheck_CaseOnlyDifferenceIsNotAMismatch(t *testing.T) {
	store := healthyStore()
	store.sessions = []localstore.DiagnosticSession{{ID: "s-1", Project: "  App  ", Directory: "/repos/app"}}
	scope := Scope{Store: store, DetectProject: func(string) (DetectedProject, bool) {
		return DetectedProject{Project: "app", Source: "git_root"}, true
	}}

	if got := runOne(t, SessionProjectDirectoryMismatchCheck{}, scope); got.Result != StatusOK {
		t.Errorf("result = %q, want %q for a case-only difference", got.Result, StatusOK)
	}
}

// ─── ambiguous_active_sessions ───────────────────────────────────────────────

func TestAmbiguousActiveSessionsCheck(t *testing.T) {
	ended := time.Now().Add(-time.Hour)
	store := healthyStore()
	store.sessions = []localstore.DiagnosticSession{
		{ID: "open-a", Project: "engram", Directory: "/repos/engram"},
		{ID: "open-b", Project: "engram", Directory: "/repos/engram"},
		{ID: "closed", Project: "engram", Directory: "/repos/engram", EndedAt: &ended},
		{ID: "elsewhere", Project: "engram", Directory: "/repos/other"},
	}

	got := runOne(t, AmbiguousActiveSessionsCheck{}, Scope{Store: store})

	if got.Result != StatusWarning {
		t.Fatalf("result = %q, want %q", got.Result, StatusWarning)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(got.Findings))
	}
	ev := evidenceOf(t, got.Findings[0].Evidence)
	ids, _ := ev["session_ids"].([]any)
	if len(ids) != 2 {
		t.Fatalf("session_ids = %v, want the two OPEN sessions only", ev["session_ids"])
	}
	if ids[0] != "open-a" || ids[1] != "open-b" {
		t.Errorf("session_ids = %v, want them sorted and free of the ended session", ids)
	}
}

// TestAmbiguousActiveSessionsCheck_OneOpenSessionIsNormal — the everyday case.
func TestAmbiguousActiveSessionsCheck_OneOpenSessionIsNormal(t *testing.T) {
	store := healthyStore()
	store.sessions = []localstore.DiagnosticSession{
		{ID: "open-a", Project: "engram", Directory: "/repos/engram"},
		{ID: "open-b", Project: "engram", Directory: "/repos/other"},
	}

	if got := runOne(t, AmbiguousActiveSessionsCheck{}, Scope{Store: store}); got.Result != StatusOK {
		t.Errorf("result = %q, want %q: one open session per directory is the normal case", got.Result, StatusOK)
	}
}

// ─── project_policy_unknown ──────────────────────────────────────────────────

func TestProjectPolicyUnknownCheck_OnlyWhenCentralConfigured(t *testing.T) {
	store := healthyStore()
	store.noPolicy = []string{"secret-client-work"}

	// Local-only node: the computed default is local-only, nothing leaves the
	// machine, and an absent policy row is exactly what it should be.
	got := runOne(t, ProjectPolicyUnknownCheck{}, Scope{Store: store})
	if got.Result != StatusOK {
		t.Errorf("result = %q, want %q without central configured", got.Result, StatusOK)
	}

	store.central = true
	got = runOne(t, ProjectPolicyUnknownCheck{}, Scope{Store: store})
	if got.Result != StatusWarning {
		t.Fatalf("result = %q, want %q with central configured", got.Result, StatusWarning)
	}
	if got.Severity != SeverityInfo {
		t.Errorf("severity = %q, want %q — this is information, not a fault", got.Severity, SeverityInfo)
	}
	if len(got.Findings) != 1 || !strings.Contains(got.Findings[0].Message, "synced") {
		t.Errorf("finding must name the default that is in force: %+v", got.Findings)
	}
	if ev := evidenceOf(t, got.Findings[0].Evidence); ev["computed_default"] != "synced" {
		t.Errorf("evidence = %v, want the computed default spelled out", ev)
	}
}

// ─── sqlite_lock_contention ──────────────────────────────────────────────────

func TestSQLiteLockContentionCheck(t *testing.T) {
	store := healthyStore()
	store.lock = localstore.SQLiteLockSnapshot{BusyTimeoutMS: 5000, CheckpointBusy: 1, WALPages: 120}

	got := runOne(t, SQLiteLockContentionCheck{}, Scope{Store: store})

	if got.Result != StatusWarning {
		t.Fatalf("result = %q, want %q", got.Result, StatusWarning)
	}
	if got.Findings[0].ReasonCode != "sqlite_lock_contention_detected" {
		t.Errorf("reason_code = %q", got.Findings[0].ReasonCode)
	}
}

// TestSQLiteLockContentionCheck_ZeroBusyTimeoutIsItsOwnFinding — a busy_timeout
// of 0 turns ordinary contention into a failed mem_save, which is a different
// problem from contention itself and gets its own reason code.
func TestSQLiteLockContentionCheck_ZeroBusyTimeoutIsItsOwnFinding(t *testing.T) {
	store := healthyStore()
	store.lock = localstore.SQLiteLockSnapshot{BusyTimeoutMS: 0}

	got := runOne(t, SQLiteLockContentionCheck{}, Scope{Store: store})

	if got.Result != StatusWarning {
		t.Fatalf("result = %q, want %q", got.Result, StatusWarning)
	}
	if len(got.Findings) != 1 || got.Findings[0].ReasonCode != "sqlite_busy_timeout_disabled" {
		t.Errorf("findings = %+v, want the disabled-timeout finding", got.Findings)
	}
}

// TestSQLiteLockContentionCheck_ProbeFailureIsReportedNotThrown — a probe that
// cannot run is the most interesting thing this check has to say, and it must
// say it inside the report rather than failing the run.
func TestSQLiteLockContentionCheck_ProbeFailureIsReportedNotThrown(t *testing.T) {
	store := healthyStore()
	store.lockErr = errors.New("database is locked")

	got := runOne(t, SQLiteLockContentionCheck{}, Scope{Store: store})

	if got.Result != StatusError {
		t.Fatalf("result = %q, want %q", got.Result, StatusError)
	}
	if got.Findings[0].ReasonCode != "sqlite_lock_probe_failed" {
		t.Errorf("reason_code = %q", got.Findings[0].ReasonCode)
	}
	if !strings.Contains(got.Findings[0].Message, "database is locked") {
		t.Errorf("message = %q, want the underlying error", got.Findings[0].Message)
	}
}

func TestSQLiteLockContentionCheck_HealthyIsSilent(t *testing.T) {
	if got := runOne(t, SQLiteLockContentionCheck{}, Scope{Store: healthyStore()}); got.Result != StatusOK {
		t.Errorf("result = %q, want %q", got.Result, StatusOK)
	}
}

// ─── stale_review_backlog ────────────────────────────────────────────────────

func TestStaleReviewBacklogCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		counts localstore.ReviewCounts
		want   string
	}{
		"mostly fresh":       {localstore.ReviewCounts{Total: 100, Active: 90, NeedsReview: 10}, StatusOK},
		"small project":      {localstore.ReviewCounts{Total: 4, Active: 1, NeedsReview: 3}, StatusOK},
		"half stale":         {localstore.ReviewCounts{Total: 40, Active: 15, NeedsReview: 20, Expired: 5}, StatusWarning},
		"everything expired": {localstore.ReviewCounts{Total: 60, Expired: 60}, StatusWarning},
		"empty project":      {localstore.ReviewCounts{}, StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			store := healthyStore()
			store.review = tc.counts

			got := runOne(t, StaleReviewBacklogCheck{}, Scope{Store: store})
			if got.Result != tc.want {
				t.Errorf("result = %q, want %q for %+v", got.Result, tc.want, tc.counts)
			}
			if tc.want == StatusWarning &&
				!strings.Contains(got.Findings[0].SafeNextStep, "mem_review") {
				t.Errorf("safe_next_step = %q, want it to name mem_review", got.Findings[0].SafeNextStep)
			}
		})
	}
}

// ─── sync_backlog ────────────────────────────────────────────────────────────

func TestSyncBacklogCheck(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	for name, tc := range map[string]struct {
		central  bool
		backlog  localstore.SyncBacklog
		want     string
		severity string
	}{
		"local only": {
			central: false,
			backlog: localstore.SyncBacklog{Pending: 900, Oldest: now.Add(-48 * time.Hour)},
			want:    StatusOK,
		},
		"drained": {
			central: true,
			want:    StatusOK,
		},
		"between cycles": {
			central:  true,
			backlog:  localstore.SyncBacklog{Pending: 3, Oldest: now.Add(-30 * time.Second)},
			want:     StatusWarning,
			severity: SeverityInfo,
		},
		"stuck for a day": {
			central:  true,
			backlog:  localstore.SyncBacklog{Pending: 12, Oldest: now.Add(-24 * time.Hour)},
			want:     StatusWarning,
			severity: SeverityWarning,
		},
		"huge": {
			central:  true,
			backlog:  localstore.SyncBacklog{Pending: 900, Oldest: now.Add(-time.Minute)},
			want:     StatusWarning,
			severity: SeverityWarning,
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := healthyStore()
			store.central = tc.central
			store.backlog = tc.backlog

			got := runOne(t, SyncBacklogCheck{}, Scope{Store: store, Now: now})
			if got.Result != tc.want {
				t.Fatalf("result = %q, want %q", got.Result, tc.want)
			}
			if tc.want == StatusOK {
				return
			}
			if got.Severity != tc.severity {
				t.Errorf("severity = %q, want %q", got.Severity, tc.severity)
			}
			if tc.severity == SeverityWarning &&
				!strings.Contains(got.Findings[0].SafeNextStep, "engram sync now") {
				t.Errorf("safe_next_step = %q, want an actionable command", got.Findings[0].SafeNextStep)
			}
		})
	}
}

// ─── runner and report shape ─────────────────────────────────────────────────

// TestRunAll_HealthyStore is the shape contract every consumer branches on.
func TestRunAll_HealthyStore(t *testing.T) {
	report := NewRunner().RunAll(context.Background(), Scope{Store: healthyStore(), Project: "engram"})

	if report.Status != StatusOK {
		t.Errorf("status = %q, want %q; checks: %+v", report.Status, StatusOK, report.Checks)
	}
	if report.Project != "engram" {
		t.Errorf("project = %q, want it echoed back", report.Project)
	}
	if report.Summary.Total != len(RegisteredCodes()) || report.Summary.Total != len(report.Checks) {
		t.Errorf("summary.total = %d, checks = %d, registered = %d",
			report.Summary.Total, len(report.Checks), len(RegisteredCodes()))
	}
	if report.Summary.OK != report.Summary.Total {
		t.Errorf("summary = %+v, want every check ok", report.Summary)
	}
	for _, check := range report.Checks {
		if check.CheckID == "" || check.Result == "" || check.Severity == "" ||
			check.ReasonCode == "" || check.Evidence == nil || check.SafeNextStep == "" {
			t.Errorf("check %q has empty contract fields: %+v", check.CheckID, check)
		}
	}
}

// TestRunAll_RollsUpWorstFirst pins the precedence error > blocked > warning > ok.
func TestRunAll_RollsUpWorstFirst(t *testing.T) {
	store := healthyStore()
	store.orphans = []localstore.OrphanedSessionRef{{SessionID: "ghost", ObservationCount: 1}}

	report := NewRunner().RunAll(context.Background(), Scope{Store: store})
	if report.Status != StatusWarning {
		t.Errorf("status = %q, want %q", report.Status, StatusWarning)
	}
	if report.Summary.Warnings != 1 {
		t.Errorf("summary = %+v, want exactly one warning", report.Summary)
	}

	store.lockErr = errors.New("probe failed")
	report = NewRunner().RunAll(context.Background(), Scope{Store: store})
	if report.Status != StatusError {
		t.Errorf("status = %q, want %q — an error outranks a warning", report.Status, StatusError)
	}
}

// TestRunAll_OneFailingCheckDoesNotSinkTheReport is the reason diagnostics get
// run at all: they are run when something is ALREADY wrong, and a store that
// cannot answer one query must still answer the rest.
func TestRunAll_OneFailingCheckDoesNotSinkTheReport(t *testing.T) {
	store := healthyStore()
	store.sessionsErr = errors.New("no such table: sessions")

	report := NewRunner().RunAll(context.Background(), Scope{Store: store})

	if report.Summary.Total != len(RegisteredCodes()) {
		t.Errorf("total = %d, want every check to have run", report.Summary.Total)
	}
	if report.Summary.Errors != 2 {
		t.Errorf("errors = %d, want 2 (both session-based checks)", report.Summary.Errors)
	}
	if report.Summary.OK == 0 {
		t.Error("every check was dragged down by the failing one")
	}
	for _, check := range report.Checks {
		if check.Result == StatusError && !strings.Contains(check.Message, "no such table") {
			t.Errorf("check %q lost the underlying error: %q", check.CheckID, check.Message)
		}
	}
}

// TestRunOne_UnknownCheckIsAnErrorReport — an unknown code gets the same
// envelope as everything else, listing what the caller may ask for instead.
func TestRunOne_UnknownCheckIsAnErrorReport(t *testing.T) {
	report := NewRunner().RunOne(context.Background(), Scope{Store: healthyStore()}, "does_not_exist")

	if report.Status != StatusError {
		t.Fatalf("status = %q, want %q", report.Status, StatusError)
	}
	if len(report.Checks) != 1 || report.Checks[0].CheckID != "invalid_check" {
		t.Fatalf("checks = %+v, want one invalid_check entry", report.Checks)
	}
	if !strings.Contains(report.Checks[0].Message, "sqlite_lock_contention") {
		t.Errorf("message = %q, want it to list the registered codes", report.Checks[0].Message)
	}
}

func TestRunOne_RunsExactlyOneCheck(t *testing.T) {
	report := NewRunner().RunOne(context.Background(), Scope{Store: healthyStore()}, CheckSyncBacklog)

	if len(report.Checks) != 1 || report.Checks[0].CheckID != CheckSyncBacklog {
		t.Fatalf("checks = %+v, want only %q", report.Checks, CheckSyncBacklog)
	}
	if report.Summary.Total != 1 {
		t.Errorf("summary.total = %d, want 1", report.Summary.Total)
	}
}

// TestRegistry_IsDeterministicAndComplete — the registry order is what makes
// two reports diffable, and RegisteredCodes is quoted in the tool description
// and in every invalid-check error.
func TestRegistry_IsDeterministicAndComplete(t *testing.T) {
	codes := RegisteredCodes()
	if len(codes) != 7 {
		t.Errorf("registered %d checks %v, want 7", len(codes), codes)
	}
	for i := 1; i < len(codes); i++ {
		if codes[i-1] >= codes[i] {
			t.Errorf("registry order is not sorted: %v", codes)
			break
		}
	}
	for _, code := range codes {
		check, err := DefaultRegistry().Lookup(code)
		if err != nil {
			t.Errorf("Lookup(%q): %v", code, err)
			continue
		}
		if check.Code() != code {
			t.Errorf("registry key %q holds a check whose code is %q", code, check.Code())
		}
	}
	if _, err := DefaultRegistry().Lookup("nope"); !errors.Is(err, ErrInvalidCheck) {
		t.Errorf("Lookup of an unknown code: %v, want ErrInvalidCheck", err)
	}
}

// TestReport_MarshalsToTheDocumentedShape pins the JSON keys the tool's
// description promises: consumers read these names, not the Go field names.
func TestReport_MarshalsToTheDocumentedShape(t *testing.T) {
	store := healthyStore()
	store.orphans = []localstore.OrphanedSessionRef{{SessionID: "ghost", ObservationCount: 2}}

	report := NewRunner().RunAll(context.Background(), Scope{Store: store, Project: "engram"})
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"status", "project", "summary", "checks"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("report is missing %q: %s", key, raw)
		}
	}
	summary, _ := decoded["summary"].(map[string]any)
	for _, key := range []string{"total", "ok", "warnings", "blocked", "errors"} {
		if _, ok := summary[key]; !ok {
			t.Errorf("summary is missing %q: %v", key, summary)
		}
	}
	checks, _ := decoded["checks"].([]any)
	if len(checks) == 0 {
		t.Fatal("report carries no checks")
	}
	first, _ := checks[0].(map[string]any)
	for _, key := range []string{
		"check_id", "result", "severity", "reason_code", "message", "why",
		"evidence", "safe_next_step", "requires_confirmation",
	} {
		if _, ok := first[key]; !ok {
			t.Errorf("check is missing %q: %v", key, first)
		}
	}
}

// TestScope_ProjectReachesEveryPerProjectQuery — a scoped run must not quietly
// report on the whole node.
func TestScope_ProjectReachesEveryPerProjectQuery(t *testing.T) {
	for name, check := range map[string]Check{
		CheckOrphanedObservationSession:      OrphanedObservationSessionCheck{},
		CheckSessionProjectDirectoryMismatch: SessionProjectDirectoryMismatchCheck{},
		CheckAmbiguousActiveSessions:         AmbiguousActiveSessionsCheck{},
		CheckStaleReviewBacklog:              StaleReviewBacklogCheck{},
	} {
		t.Run(name, func(t *testing.T) {
			store := healthyStore()
			runOne(t, check, Scope{Store: store, Project: "scoped-project"})
			if store.lastProjectIn != "scoped-project" {
				t.Errorf("check queried project %q, want %q", store.lastProjectIn, "scoped-project")
			}
		})
	}
}

// ─── the store's own default is not a fault ─────────────────────────────────

// TestOrphanedObservationSessionCheck_ManualSavesAreAnInfoNote is the fix for a
// warning that fired on every healthy store. A mem_save with no session_id is
// filed under "manual-save-{project}" — the documented default — and nothing
// ever registers such a session, so the orphan check reported it as a problem
// forever. A doctor that warns about its own defaults is a doctor whose
// warnings get skipped, and the real ones go unread with them.
func TestOrphanedObservationSessionCheck_ManualSavesAreAnInfoNote(t *testing.T) {
	store := healthyStore()
	store.unregistered = []localstore.OrphanedSessionRef{
		{SessionID: localstore.ManualSaveSessionPrefix + "engram", ObservationCount: 12, Projects: []string{"engram"}},
	}

	got := runOne(t, OrphanedObservationSessionCheck{}, Scope{Store: store, Project: "engram"})

	if got.Result != StatusOK {
		t.Errorf("result = %q, want %q: saving without a session is the documented default, not a fault",
			got.Result, StatusOK)
	}
	if got.Severity != SeverityInfo {
		t.Errorf("severity = %q, want %q", got.Severity, SeverityInfo)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("findings = %+v, want exactly one informational note", got.Findings)
	}
	note := got.Findings[0]
	if note.ReasonCode != ReasonUnregisteredSessionSaves {
		t.Errorf("reason_code = %q, want %q", note.ReasonCode, ReasonUnregisteredSessionSaves)
	}
	if note.Severity != SeverityInfo {
		t.Errorf("finding severity = %q, want %q", note.Severity, SeverityInfo)
	}
	if !strings.Contains(note.Message, "12") || !strings.Contains(note.Message, "mem_session_start") {
		t.Errorf("message = %q, want the count and the thing to do about it", note.Message)
	}
	if note.RequiresConfirmation {
		t.Error("an informational note must not ask for confirmation; there is nothing to confirm")
	}
}

// TestOrphanedObservationSessionCheck_ReportsBothKinds — a store with a genuine
// orphan AND manual saves is a warning about the orphan, with the note still
// attached. The two must not be collapsed: one is a session that vanished, the
// other is a session that was never meant to exist.
func TestOrphanedObservationSessionCheck_ReportsBothKinds(t *testing.T) {
	store := healthyStore()
	store.orphans = []localstore.OrphanedSessionRef{
		{SessionID: "ghost-1", ObservationCount: 2, Projects: []string{"engram"}},
	}
	store.unregistered = []localstore.OrphanedSessionRef{
		{SessionID: localstore.ManualSaveSessionPrefix + "engram", ObservationCount: 5, Projects: []string{"engram"}},
	}

	got := runOne(t, OrphanedObservationSessionCheck{}, Scope{Store: store, Project: "engram"})

	if got.Result != StatusWarning {
		t.Errorf("result = %q, want %q — the real orphan still warns", got.Result, StatusWarning)
	}
	if len(got.Findings) != 2 {
		t.Fatalf("findings = %d, want the orphan warning and the manual-save note", len(got.Findings))
	}
	reasons := map[string]string{}
	for _, f := range got.Findings {
		reasons[f.ReasonCode] = f.Severity
	}
	if reasons[CheckOrphanedObservationSession] != SeverityWarning {
		t.Errorf("orphan finding severity = %q, want %q", reasons[CheckOrphanedObservationSession], SeverityWarning)
	}
	if reasons[ReasonUnregisteredSessionSaves] != SeverityInfo {
		t.Errorf("manual-save finding severity = %q, want %q", reasons[ReasonUnregisteredSessionSaves], SeverityInfo)
	}
}

// TestOrphanedObservationSessionCheck_UnregisteredQueryFailureIsAnError — the
// note's query is not optional decoration: a check that silently swallowed its
// failure would report "ok" about a condition it never looked at.
func TestOrphanedObservationSessionCheck_UnregisteredQueryFailureIsAnError(t *testing.T) {
	store := healthyStore()
	store.unregErr = errors.New("database is locked")

	got := runOne(t, OrphanedObservationSessionCheck{}, Scope{Store: store, Project: "engram"})

	if got.Result != StatusError {
		t.Errorf("result = %q, want %q", got.Result, StatusError)
	}
	if !strings.Contains(got.Message, "database is locked") {
		t.Errorf("message = %q, want the underlying failure", got.Message)
	}
}

// ─── one run, one read of the sessions table ────────────────────────────────

// TestRunAll_ReadsSessionsOnce pins the memoization. Two checks
// (ambiguous_active_sessions and session_project_directory_mismatch) each need
// every session row, and on a long-lived store that is the most expensive read
// the doctor does. Running it twice per report is duplication of a query whose
// answer cannot change mid-run in any way the report should reflect.
func TestRunAll_ReadsSessionsOnce(t *testing.T) {
	store := healthyStore()
	store.sessions = []localstore.DiagnosticSession{
		{ID: "s1", Project: "engram", Directory: t.TempDir()},
	}

	NewRunnerWithRegistry(NewRegistry(
		AmbiguousActiveSessionsCheck{},
		SessionProjectDirectoryMismatchCheck{},
	)).RunAll(context.Background(), Scope{Store: store, Project: "engram"})

	if store.sessionsCalls != 1 {
		t.Errorf("DiagnosticSessions was called %d times in one run, want 1", store.sessionsCalls)
	}
}

// TestScope_SessionsWithoutMemoStillWorks — a Scope built by hand (a test, or a
// caller running a single check directly) has no memo attached. It must still
// answer, just without the caching.
func TestScope_SessionsWithoutMemoStillWorks(t *testing.T) {
	store := healthyStore()
	store.sessions = []localstore.DiagnosticSession{{ID: "s1", Project: "engram"}}

	scope := Scope{Store: store, Project: "engram"}
	for i := 0; i < 2; i++ {
		sessions, err := scope.Sessions()
		if err != nil {
			t.Fatalf("Sessions: %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("sessions = %d, want 1", len(sessions))
		}
	}
	if store.sessionsCalls != 2 {
		t.Errorf("DiagnosticSessions was called %d times without a memo, want 2", store.sessionsCalls)
	}
}

// ─── run-level failure vs a check that could not answer ─────────────────────

// TestReport_IsRunLevelFailure draws the line the mem_doctor tool marks IsError
// on. A probe that cannot read its own pragma rolls the report status up to
// "error" while every other check answered — flagging THAT as a failed tool
// call is how an agent learns not to run diagnostics.
func TestReport_IsRunLevelFailure(t *testing.T) {
	unknown := NewRunner().RunOne(context.Background(), Scope{Store: healthyStore()}, "fix_everything")
	if !unknown.IsRunLevelFailure() {
		t.Error("an unknown check code is a run-level failure: nothing was diagnosed")
	}

	store := healthyStore()
	store.lockErr = errors.New("unable to open database file")
	probeFailed := NewRunner().RunAll(context.Background(), Scope{Store: store, Project: "engram"})
	if probeFailed.Status != StatusError {
		t.Fatalf("status = %q, want %q for a failed probe", probeFailed.Status, StatusError)
	}
	if probeFailed.IsRunLevelFailure() {
		t.Error("a failed PROBE is a finding, not a failed run: the other checks answered")
	}
}
