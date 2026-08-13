package main

import (
	"path/filepath"
	"testing"
	"time"
)

// ─── mem_save: session_id default ──────────────────────────────────────────

// TestDaemonTool_MemSave_SessionIDDefault verifies that an omitted session_id
// is stored as "manual-save-{project}" using the FINAL resolved project name.
func TestDaemonTool_MemSave_SessionIDDefault(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "engram.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	saveTool := components.mcpServer.ListTools()["mem_save"]
	req := newToolRequest("mem_save", map[string]any{
		"title":   "session default test",
		"content": "body",
		"project": "default-proj",
	})
	result, err := saveTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	rec, err := components.store.GetObservation(1)
	if err != nil {
		t.Fatalf("GetObservation(1): %v", err)
	}
	const want = "manual-save-default-proj"
	if rec.SessionID != want {
		t.Errorf("SessionID = %q, want %q", rec.SessionID, want)
	}
}

// TestDaemonTool_MemSave_SessionIDExplicit verifies that an explicit
// session_id is stored verbatim (not overridden by the default).
func TestDaemonTool_MemSave_SessionIDExplicit(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "engram.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	saveTool := components.mcpServer.ListTools()["mem_save"]
	req := newToolRequest("mem_save", map[string]any{
		"title":      "explicit session test",
		"content":    "body",
		"project":    "default-proj",
		"session_id": "sess-explicit",
	})
	result, err := saveTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	rec, err := components.store.GetObservation(1)
	if err != nil {
		t.Fatalf("GetObservation(1): %v", err)
	}
	if rec.SessionID != "sess-explicit" {
		t.Errorf("SessionID = %q, want %q", rec.SessionID, "sess-explicit")
	}
}

// ─── mem_save_prompt: session_id default ───────────────────────────────────

// TestDaemonTool_MemSavePrompt_SessionIDDefault verifies that an omitted
// session_id is stored as "manual-save-{project}" using the FINAL resolved
// project name.
func TestDaemonTool_MemSavePrompt_SessionIDDefault(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "engram.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	promptTool := components.mcpServer.ListTools()["mem_save_prompt"]
	req := newToolRequest("mem_save_prompt", map[string]any{
		"content": "prompt body",
		"project": "prompt-proj",
	})
	result, err := promptTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	const want = "manual-save-prompt-proj"
	prompt, err := components.store.GetPromptBySessionAndContent(want, "prompt-proj", "prompt body")
	if err != nil {
		t.Fatalf("GetPromptBySessionAndContent(%q): %v", want, err)
	}
	if prompt.SessionID != want {
		t.Errorf("SessionID = %q, want %q", prompt.SessionID, want)
	}
}

// TestDaemonTool_MemSavePrompt_SessionIDExplicit verifies that an explicit
// session_id is stored verbatim.
func TestDaemonTool_MemSavePrompt_SessionIDExplicit(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "engram.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	promptTool := components.mcpServer.ListTools()["mem_save_prompt"]
	req := newToolRequest("mem_save_prompt", map[string]any{
		"content":    "prompt body explicit",
		"project":    "prompt-proj",
		"session_id": "sess-explicit-prompt",
	})
	result, err := promptTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	prompt, err := components.store.GetPromptBySessionAndContent("sess-explicit-prompt", "prompt-proj", "prompt body explicit")
	if err != nil {
		t.Fatalf("GetPromptBySessionAndContent: %v", err)
	}
	if prompt.SessionID != "sess-explicit-prompt" {
		t.Errorf("SessionID = %q, want %q", prompt.SessionID, "sess-explicit-prompt")
	}
}

// ─── mem_session_summary: session_id default ───────────────────────────────

// TestDaemonTool_MemSessionSummary_SessionIDDefault verifies that an omitted
// session_id is stored as "manual-save-{project}" using the FINAL resolved
// project name (cwd-detected, since this call passes no explicit project and no
// prior session row exists for an empty session_id).
func TestDaemonTool_MemSessionSummary_SessionIDDefault(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "engram.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	sumTool := components.mcpServer.ListTools()["mem_session_summary"]
	req := newToolRequest("mem_session_summary", map[string]any{
		"content": "## Goal\nverify session_id default",
	})
	result, err := sumTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	rec, err := components.store.GetObservation(1)
	if err != nil {
		t.Fatalf("GetObservation(1): %v", err)
	}
	want := "manual-save-" + rec.Project
	if rec.SessionID != want {
		t.Errorf("SessionID = %q, want %q (project=%q)", rec.SessionID, want, rec.Project)
	}
}

// TestDaemonTool_MemSessionSummary_SessionIDExplicit verifies that an
// explicit session_id is stored verbatim.
func TestDaemonTool_MemSessionSummary_SessionIDExplicit(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "engram.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	if err := components.store.CreateSession("sess-sum-explicit", "session-project", "/some/client/dir"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	sumTool := components.mcpServer.ListTools()["mem_session_summary"]
	req := newToolRequest("mem_session_summary", map[string]any{
		"content":    "## Goal\nverify explicit session_id is preserved",
		"session_id": "sess-sum-explicit",
	})
	result, err := sumTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	rec, err := components.store.GetObservation(1)
	if err != nil {
		t.Fatalf("GetObservation(1): %v", err)
	}
	if rec.SessionID != "sess-sum-explicit" {
		t.Errorf("SessionID = %q, want %q", rec.SessionID, "sess-sum-explicit")
	}
	if rec.Project != "session-project" {
		t.Errorf("Project = %q, want %q (session lookup should still take priority)", rec.Project, "session-project")
	}
}
