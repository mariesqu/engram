package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mariesqu/engram/internal/diagnostic"
	"github.com/mariesqu/engram/internal/localstore"
)

// These tests cover mem_doctor at the TOOL boundary: the per-check logic is
// unit-tested in internal/diagnostic against fakes, so what is left to prove
// here is the wiring — that the tool scopes to the project the read tools would
// resolve, that a report full of warnings is still a successful call, and that
// an unknown check is answered rather than thrown.

// callDoctor invokes the registered mem_doctor handler and decodes its report.
func callDoctor(t *testing.T, components *daemonComponents, args map[string]any) (diagnostic.Report, bool) {
	t.Helper()

	tool, ok := components.mcpServer.ListTools()["mem_doctor"]
	if !ok {
		t.Fatal("mem_doctor is not registered")
	}
	result, err := tool.Handler(t.Context(), newToolRequest("mem_doctor", args))
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if len(result.Content) == 0 {
		t.Fatal("mem_doctor returned no content")
	}
	text, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want mcp.TextContent", result.Content[0])
	}
	var report diagnostic.Report
	if err := json.Unmarshal([]byte(text.Text), &report); err != nil {
		t.Fatalf("response is not a diagnostic report (%v): %s", err, text.Text)
	}
	return report, result.IsError
}

// doctorDaemon boots a daemon for the doctor tests.
func doctorDaemon(t *testing.T) *daemonComponents {
	t.Helper()
	components, err := buildDaemon(daemonCfg{
		db:           filepath.Join(t.TempDir(), "doctor.db"),
		syncInterval: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)
	return components
}

// TestDoctor_CleanStoreReportsOK is the baseline: a fresh store must come back
// green on every check. A doctor that warns about a store nobody has used yet
// is a doctor whose warnings mean nothing.
func TestDoctor_CleanStoreReportsOK(t *testing.T) {
	components := doctorDaemon(t)

	report, isError := callDoctor(t, components, map[string]any{"project": "engram"})

	if isError {
		t.Error("a healthy report must not be marked as a tool error")
	}
	if report.Status != diagnostic.StatusOK {
		t.Errorf("status = %q, want %q; checks: %+v", report.Status, diagnostic.StatusOK, report.Checks)
	}
	if report.Summary.Total != len(diagnostic.RegisteredCodes()) {
		t.Errorf("summary.total = %d, want every registered check (%d)",
			report.Summary.Total, len(diagnostic.RegisteredCodes()))
	}
	if report.Project != "engram" {
		t.Errorf("project = %q, want the scope echoed back", report.Project)
	}
}

// TestDoctor_FindsOrphanedObservations drives one real condition end to end
// through the tool, against a real store.
func TestDoctor_FindsOrphanedObservations(t *testing.T) {
	components := doctorDaemon(t)

	// A save naming a session id that was never registered — what an interrupted
	// session, or a sync pull that landed an observation before its session,
	// leaves behind. NOT what a plain mem_save leaves behind: that one is filed
	// under the store's own "manual-save-{project}" default and is reported as an
	// informational note instead (TestDoctor_PlainSaveStaysOKWithAnInfoNote).
	if _, err := components.store.AddObservation(localstore.AddObservationParams{
		SessionID: "never-registered",
		Title:     "an orphaned note",
		Project:   "engram",
	}); err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	report, isError := callDoctor(t, components, map[string]any{"project": "engram"})

	if isError {
		t.Error("warnings are a successful diagnosis, not a tool error")
	}
	if report.Status != diagnostic.StatusWarning {
		t.Fatalf("status = %q, want %q", report.Status, diagnostic.StatusWarning)
	}
	var found *diagnostic.CheckResult
	for i := range report.Checks {
		if report.Checks[i].CheckID == diagnostic.CheckOrphanedObservationSession {
			found = &report.Checks[i]
		}
	}
	if found == nil {
		t.Fatalf("report has no %q check: %+v", diagnostic.CheckOrphanedObservationSession, report.Checks)
	}
	if len(found.Findings) != 1 || !strings.Contains(found.Findings[0].Message, "never-registered") {
		t.Errorf("findings = %+v, want the orphaned session named", found.Findings)
	}
	if found.Findings[0].SafeNextStep == "" {
		t.Error("a finding with no safe_next_step tells the agent nothing it can act on")
	}
}

// TestDoctor_SingleCheck runs one check by code, which is what an agent does
// after a first full report.
func TestDoctor_SingleCheck(t *testing.T) {
	components := doctorDaemon(t)

	report, isError := callDoctor(t, components, map[string]any{
		"project": "engram",
		"check":   diagnostic.CheckSQLiteLockContention,
	})

	if isError {
		t.Error("a healthy single check must not be a tool error")
	}
	if len(report.Checks) != 1 || report.Checks[0].CheckID != diagnostic.CheckSQLiteLockContention {
		t.Fatalf("checks = %+v, want only the requested one", report.Checks)
	}
	if report.Summary.Total != 1 {
		t.Errorf("summary.total = %d, want 1", report.Summary.Total)
	}
}

// TestDoctor_UnknownCheckIsAnErrorReport — the caller asked a question, and
// "that check does not exist, here are the ones that do" is the answer. It is
// the one case the tool result is flagged as an error, because nothing was
// diagnosed.
func TestDoctor_UnknownCheckIsAnErrorReport(t *testing.T) {
	components := doctorDaemon(t)

	report, isError := callDoctor(t, components, map[string]any{"check": "fix_everything"})

	if !isError {
		t.Error("an unrunnable check must be flagged as a tool error")
	}
	if report.Status != diagnostic.StatusError {
		t.Errorf("status = %q, want %q", report.Status, diagnostic.StatusError)
	}
	if len(report.Checks) != 1 || report.Checks[0].CheckID != "invalid_check" {
		t.Fatalf("checks = %+v, want one invalid_check entry", report.Checks)
	}
	if !strings.Contains(report.Checks[0].Message, diagnostic.CheckSyncBacklog) {
		t.Errorf("message = %q, want it to list the valid codes", report.Checks[0].Message)
	}
}

// TestDoctor_ScopesToTheResolvedProject pins the wiring that makes the report
// about the caller: with no explicit project, the scope comes from the same
// resolution mem_search and mem_context use — the forwarded directory.
func TestDoctor_ScopesToTheResolvedProject(t *testing.T) {
	components := doctorDaemon(t)
	repo := pinnedProjectDir(t, "doctor-repo")
	chdirToJunkDir(t)

	// An orphan in a DIFFERENT project: a correctly scoped run must not see it.
	if _, err := components.store.AddObservation(localstore.AddObservationParams{
		SessionID: "never-registered", Title: "elsewhere", Project: "other-project",
	}); err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	report, _ := callDoctor(t, components, map[string]any{"directory": repo})

	if report.Project != "doctor-repo" {
		t.Errorf("project = %q, want the directory's project %q", report.Project, "doctor-repo")
	}
	if report.Status != diagnostic.StatusOK {
		t.Errorf("status = %q, want %q — the only problem belongs to another project: %+v",
			report.Status, diagnostic.StatusOK, report.Checks)
	}
}

// TestDoctor_HonoursTheCwdAlias — mem_doctor is directory-aware, so it reads the
// alias through the same shared resolver as every other directory-aware tool.
func TestDoctor_HonoursTheCwdAlias(t *testing.T) {
	components := doctorDaemon(t)
	repo := pinnedProjectDir(t, "alias-doctor-repo")

	report, _ := callDoctor(t, components, map[string]any{"cwd": repo})

	if report.Project != "alias-doctor-repo" {
		t.Errorf("project = %q, want the alias to be honoured", report.Project)
	}
}

// ─── the store's own default must not be a permanent warning ────────────────

// TestDoctor_PlainSaveStaysOKWithAnInfoNote is the end-to-end version of the
// fix: a fresh store, one ordinary mem_save with no session_id, and a doctor
// that says everything is fine.
//
// Before, that save was filed under "manual-save-{project}" — the documented
// default — which has no session row, so the orphan check reported it as a
// warning. Every store that had ever taken a manual save was permanently
// "warning", which is the state in which people stop reading warnings.
func TestDoctor_PlainSaveStaysOKWithAnInfoNote(t *testing.T) {
	components := doctorDaemon(t)
	repo := pinnedProjectDir(t, "plain-save-repo")

	saveTool := components.mcpServer.ListTools()["mem_save"]
	result, err := saveTool.Handler(t.Context(), newToolRequest("mem_save", map[string]any{
		"title": "a decision nobody registered a session for", "directory": repo,
	}))
	if err != nil {
		t.Fatalf("mem_save transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("mem_save failed: %v", result.Content)
	}

	report, isError := callDoctor(t, components, map[string]any{"project": "plain-save-repo"})

	if isError {
		t.Error("a healthy report must not be marked as a tool error")
	}
	if report.Status != diagnostic.StatusOK {
		t.Fatalf("status = %q, want %q — a plain mem_save is not a fault; checks: %+v",
			report.Status, diagnostic.StatusOK, report.Checks)
	}

	var orphan *diagnostic.CheckResult
	for i := range report.Checks {
		if report.Checks[i].CheckID == diagnostic.CheckOrphanedObservationSession {
			orphan = &report.Checks[i]
		}
	}
	if orphan == nil {
		t.Fatalf("report has no %q check", diagnostic.CheckOrphanedObservationSession)
	}
	if len(orphan.Findings) != 1 {
		t.Fatalf("findings = %+v, want the single informational note", orphan.Findings)
	}
	note := orphan.Findings[0]
	if note.ReasonCode != diagnostic.ReasonUnregisteredSessionSaves {
		t.Errorf("reason_code = %q, want %q", note.ReasonCode, diagnostic.ReasonUnregisteredSessionSaves)
	}
	if note.Severity != diagnostic.SeverityInfo {
		t.Errorf("severity = %q, want %q", note.Severity, diagnostic.SeverityInfo)
	}
	if !strings.Contains(note.Message, "mem_session_start") {
		t.Errorf("message = %q, want it to name the thing that would fix the grouping", note.Message)
	}
}

// ─── dispatch: one check means ONE check ────────────────────────────────────

// countingCheck records how many times it was run.
type countingCheck struct {
	code string
	runs *int32
}

func (c countingCheck) Code() string { return c.code }

func (c countingCheck) Run(context.Context, diagnostic.Scope) (diagnostic.CheckResult, error) {
	atomic.AddInt32(c.runs, 1)
	return diagnostic.CheckResult{CheckID: c.code}, nil
}

// TestDoctor_RunsOnlyTheRequestedCheck pins the dispatch. The handler used to
// run RunAll and then OVERWRITE the report with RunOne whenever a check
// argument was present, so asking for one check quietly cost a full diagnostic
// pass — including the WAL checkpoint probe and two scans of the sessions
// table. With the real registry that is invisible: every check answers ok on a
// clean store, so both versions produce the same response.
func TestDoctor_RunsOnlyTheRequestedCheck(t *testing.T) {
	components := doctorDaemon(t)

	var wantedRuns, otherRuns int32
	registry := diagnostic.NewRegistry(
		countingCheck{code: "wanted", runs: &wantedRuns},
		countingCheck{code: "other", runs: &otherRuns},
	)
	handler := handleDoctorWithRunner(components.store, diagnostic.NewRunnerWithRegistry(registry))

	if _, err := handler(t.Context(), newToolRequest("mem_doctor", map[string]any{
		"project": "engram", "check": "wanted",
	})); err != nil {
		t.Fatalf("handler transport error: %v", err)
	}

	if got := atomic.LoadInt32(&wantedRuns); got != 1 {
		t.Errorf("the requested check ran %d time(s), want 1", got)
	}
	if got := atomic.LoadInt32(&otherRuns); got != 0 {
		t.Errorf("the OTHER check ran %d time(s) for a single-check request — RunAll is still being called first", got)
	}

	// And with no check argument, everything runs exactly once.
	atomic.StoreInt32(&wantedRuns, 0)
	if _, err := handler(t.Context(), newToolRequest("mem_doctor", map[string]any{"project": "engram"})); err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if got := atomic.LoadInt32(&wantedRuns); got != 1 {
		t.Errorf("full run: check ran %d time(s), want 1", got)
	}
	if got := atomic.LoadInt32(&otherRuns); got != 1 {
		t.Errorf("full run: second check ran %d time(s), want 1", got)
	}
}

// ─── a check that could not answer is not a failed call ─────────────────────

// failingCheck reports a finding with severity=error, the way the SQLite lock
// probe does when it cannot read its own pragma.
type failingCheck struct{}

func (failingCheck) Code() string { return "probe_that_cannot_answer" }

func (c failingCheck) Run(context.Context, diagnostic.Scope) (diagnostic.CheckResult, error) {
	return diagnostic.CheckResult{
		CheckID:  c.Code(),
		Result:   diagnostic.StatusError,
		Severity: diagnostic.SeverityError,
		Message:  "could not read the pragma",
	}, nil
}

// TestDoctor_AProbeThatCannotAnswerIsNotAToolError draws the line IsError marks.
// A single probe failing rolls the report status up to "error" while every
// other check answered perfectly — and a tool result flagged as an error is
// what teaches an agent that running mem_doctor is something that fails. It
// then stops running it, on exactly the store that needed it.
func TestDoctor_AProbeThatCannotAnswerIsNotAToolError(t *testing.T) {
	components := doctorDaemon(t)
	handler := handleDoctorWithRunner(components.store,
		diagnostic.NewRunnerWithRegistry(diagnostic.NewRegistry(failingCheck{})))

	result, err := handler(t.Context(), newToolRequest("mem_doctor", map[string]any{"project": "engram"}))
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Error("a failed PROBE was reported as a failed CALL; the report itself is the answer")
	}

	text, _ := result.Content[0].(mcp.TextContent)
	var report diagnostic.Report
	if err := json.Unmarshal([]byte(text.Text), &report); err != nil {
		t.Fatalf("response is not a report (%v): %s", err, text.Text)
	}
	if report.Status != diagnostic.StatusError {
		t.Errorf("status = %q, want %q — the report must still SAY the probe failed", report.Status, diagnostic.StatusError)
	}
}
