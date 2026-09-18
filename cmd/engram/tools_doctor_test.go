package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
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

	// A save with an explicit session_id that was never registered — exactly what
	// a manual mem_save or an interrupted session leaves behind.
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
