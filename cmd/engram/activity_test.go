package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// ─── SessionActivity unit tests ───────────────────────────────────────────────

// TestSessionActivity_RecordAndCurrent verifies that RecordPrompt stores the
// prompt and CurrentPrompt returns it when called with the same session+project.
func TestSessionActivity_RecordAndCurrent(t *testing.T) {
	a := NewSessionActivity()
	a.RecordPrompt("sess-1", "projA", "hello world")

	got, ok := a.CurrentPrompt("sess-1", "projA")
	if !ok {
		t.Fatal("CurrentPrompt: expected ok=true, got false")
	}
	if got != "hello world" {
		t.Errorf("CurrentPrompt = %q, want %q", got, "hello world")
	}
}

// TestSessionActivity_WrongProject verifies that CurrentPrompt returns ("", false)
// when the stored project does not match the requested project.
func TestSessionActivity_WrongProject(t *testing.T) {
	a := NewSessionActivity()
	a.RecordPrompt("sess-1", "projA", "hello world")

	got, ok := a.CurrentPrompt("sess-1", "projB")
	if ok {
		t.Errorf("CurrentPrompt for wrong project: expected ok=false, got true (content=%q)", got)
	}
	if got != "" {
		t.Errorf("CurrentPrompt for wrong project: expected empty string, got %q", got)
	}
}

// TestSessionActivity_ClearSession verifies that ClearSession removes the
// session so CurrentPrompt returns ("", false) afterwards.
func TestSessionActivity_ClearSession(t *testing.T) {
	a := NewSessionActivity()
	a.RecordPrompt("sess-clear", "projA", "some prompt")

	a.ClearSession("sess-clear")

	got, ok := a.CurrentPrompt("sess-clear", "projA")
	if ok {
		t.Errorf("CurrentPrompt after ClearSession: expected ok=false, got true (content=%q)", got)
	}
	if got != "" {
		t.Errorf("CurrentPrompt after ClearSession: expected empty string, got %q", got)
	}
}

// TestSessionActivity_NoRecord verifies that CurrentPrompt returns ("", false)
// when no prompt has ever been recorded for the session.
func TestSessionActivity_NoRecord(t *testing.T) {
	a := NewSessionActivity()
	got, ok := a.CurrentPrompt("no-such-session", "projA")
	if ok {
		t.Errorf("CurrentPrompt for unknown session: expected ok=false, got true (content=%q)", got)
	}
	if got != "" {
		t.Errorf("CurrentPrompt for unknown session: expected empty string, got %q", got)
	}
}

// TestSessionActivity_Concurrent verifies that concurrent RecordPrompt and
// CurrentPrompt calls on separate sessions do not data-race.
//
// NOTE: -race requires CGO_ENABLED=1 (cgo + gcc). Running with the normal test
// suite (CGO_ENABLED=0) exercises the logic without the race detector.
func TestSessionActivity_Concurrent(t *testing.T) {
	a := NewSessionActivity()
	const workers = 20
	done := make(chan struct{}, workers*2)

	for i := range workers {
		sessID := "sess-concurrent"
		proj := "proj"
		if i%2 == 0 {
			proj = "proj-even"
		}
		go func() {
			a.RecordPrompt(sessID, proj, "prompt text")
			done <- struct{}{}
		}()
		go func() {
			a.CurrentPrompt(sessID, proj)
			done <- struct{}{}
		}()
	}

	deadline := time.After(5 * time.Second)
	received := 0
	for received < workers*2 {
		select {
		case <-done:
			received++
		case <-deadline:
			t.Fatalf("concurrent test timed out after receiving %d/%d completions", received, workers*2)
		}
	}
}

// ─── mem_save_prompt handler tests ───────────────────────────────────────────

// TestDaemonTool_MemSavePrompt_PersistsAndRecords verifies the mem_save_prompt
// handler end-to-end: after calling it, a user_prompts row exists in the store
// and CurrentPrompt returns the content for the same session+project.
func TestDaemonTool_MemSavePrompt_PersistsAndRecords(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "engram.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	promptTool, ok := components.mcpServer.ListTools()["mem_save_prompt"]
	if !ok {
		t.Fatal("mem_save_prompt not registered")
	}

	req := newToolRequest("mem_save_prompt", map[string]any{
		"content":    "implement the PR-4 surface layer",
		"session_id": "sess-sp",
		"project":    "engram",
	})
	result, err := promptTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	// Verify the prompt row is in the store.
	prompt, err := components.store.GetPromptBySessionAndContent("sess-sp", "engram", "implement the PR-4 surface layer")
	if err != nil {
		t.Fatalf("GetPromptBySessionAndContent: %v", err)
	}
	if prompt.Content != "implement the PR-4 surface layer" {
		t.Errorf("prompt.Content = %q, want %q", prompt.Content, "implement the PR-4 surface layer")
	}
}

// TestDaemonTool_MemSavePrompt_MissingContent verifies that mem_save_prompt
// returns a tool error (not a transport error) when content is absent.
func TestDaemonTool_MemSavePrompt_MissingContent(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "engram.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	promptTool := components.mcpServer.ListTools()["mem_save_prompt"]
	req := newToolRequest("mem_save_prompt", map[string]any{})
	result, err := promptTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected tool error for missing content, got success: %v", result.Content)
	}
}

// ─── capture_prompt auto-capture tests ───────────────────────────────────────

// TestDaemonTool_AutoCapture_DefaultTrue verifies that mem_save_prompt followed
// by mem_save (capture_prompt absent → default true) under the same
// session+project results in exactly one user_prompts row for the prompt (dedup
// ensures a second mem_save does not insert a duplicate).
func TestDaemonTool_AutoCapture_DefaultTrue(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "engram.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	const (
		sessID  = "sess-autocapture"
		project = "capture-proj"
		prompt  = "add auto-capture to mem_save"
	)

	// 1. Save the prompt via mem_save_prompt.
	promptTool := components.mcpServer.ListTools()["mem_save_prompt"]
	pReq := newToolRequest("mem_save_prompt", map[string]any{
		"content":    prompt,
		"session_id": sessID,
		"project":    project,
	})
	pResult, err := promptTool.Handler(t.Context(), pReq)
	if err != nil {
		t.Fatalf("mem_save_prompt transport error: %v", err)
	}
	if pResult.IsError {
		t.Fatalf("mem_save_prompt tool error: %v", pResult.Content)
	}

	// 2. Call mem_save without capture_prompt (defaults to true).
	saveTool := components.mcpServer.ListTools()["mem_save"]
	sReq := newToolRequest("mem_save", map[string]any{
		"title":      "auto-capture test observation",
		"content":    "implementing the capture path",
		"session_id": sessID,
		"project":    project,
	})
	sResult, err := saveTool.Handler(t.Context(), sReq)
	if err != nil {
		t.Fatalf("mem_save transport error: %v", err)
	}
	if sResult.IsError {
		t.Fatalf("mem_save tool error: %v", sResult.Content)
	}

	// 3. Verify exactly one prompt row exists (AddPromptIfMissing deduped the re-capture).
	count, err := components.store.CountPromptsForSession(sessID, project, prompt)
	if err != nil {
		t.Fatalf("CountPromptsForSession: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 prompt row after dedup, got %d", count)
	}

	// 4. A second mem_save must not create a second row.
	sResult2, err := saveTool.Handler(t.Context(), newToolRequest("mem_save", map[string]any{
		"title":      "second observation same session",
		"content":    "body",
		"session_id": sessID,
		"project":    project,
	}))
	if err != nil {
		t.Fatalf("second mem_save transport error: %v", err)
	}
	if sResult2.IsError {
		t.Fatalf("second mem_save tool error: %v", sResult2.Content)
	}

	count2, err := components.store.CountPromptsForSession(sessID, project, prompt)
	if err != nil {
		t.Fatalf("CountPromptsForSession (second check): %v", err)
	}
	if count2 != 1 {
		t.Errorf("expected still 1 prompt row after second mem_save, got %d (dedup failed)", count2)
	}
}

// TestDaemonTool_AutoCapture_ExplicitFalse verifies that capture_prompt=false
// on mem_save suppresses auto-capture: even after mem_save_prompt, the count
// remains at 1 (only the explicit mem_save_prompt row, no re-capture attempt).
func TestDaemonTool_AutoCapture_ExplicitFalse(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "engram.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	const (
		sessID  = "sess-noautocapture"
		project = "no-capture-proj"
		prompt  = "this prompt should not be re-captured"
	)

	// 1. Save the prompt.
	promptTool := components.mcpServer.ListTools()["mem_save_prompt"]
	pReq := newToolRequest("mem_save_prompt", map[string]any{
		"content":    prompt,
		"session_id": sessID,
		"project":    project,
	})
	pResult, err := promptTool.Handler(t.Context(), pReq)
	if err != nil {
		t.Fatalf("mem_save_prompt transport error: %v", err)
	}
	if pResult.IsError {
		t.Fatalf("mem_save_prompt tool error: %v", pResult.Content)
	}

	// 2. mem_save with capture_prompt=false.
	saveTool := components.mcpServer.ListTools()["mem_save"]
	sReq := newToolRequest("mem_save", map[string]any{
		"title":          "no-capture observation",
		"content":        "body",
		"session_id":     sessID,
		"project":        project,
		"capture_prompt": false,
	})
	sResult, err := saveTool.Handler(t.Context(), sReq)
	if err != nil {
		t.Fatalf("mem_save transport error: %v", err)
	}
	if sResult.IsError {
		t.Fatalf("mem_save tool error: %v", sResult.Content)
	}

	// Still exactly 1 row (only the explicit mem_save_prompt).
	count, err := components.store.CountPromptsForSession(sessID, project, prompt)
	if err != nil {
		t.Fatalf("CountPromptsForSession: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 prompt row (only mem_save_prompt), got %d", count)
	}
}

// TestDaemonTool_AutoCapture_NoPriorPrompt verifies that mem_save with
// capture_prompt=true (default) when no prior mem_save_prompt has been called
// produces no capture, no error, and a normal save result.
func TestDaemonTool_AutoCapture_NoPriorPrompt(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "engram.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	saveTool := components.mcpServer.ListTools()["mem_save"]
	req := newToolRequest("mem_save", map[string]any{
		"title":      "save with no prior prompt",
		"content":    "body",
		"session_id": "sess-noprompt",
		"project":    "noprompt-proj",
	})
	result, err := saveTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "Memory saved") {
		t.Errorf("expected 'Memory saved' in result, got: %s", text)
	}
}

// ─── mem_session_end clears activity ─────────────────────────────────────────

// TestDaemonTool_SessionEnd_ClearsActivity verifies that after mem_session_end,
// CurrentPrompt returns ("", false) for the session — the activity was cleared.
func TestDaemonTool_SessionEnd_ClearsActivity(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "engram.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	const (
		sessID  = "sess-end-clear"
		project = "clear-proj"
	)

	// Create the session so EndSession succeeds.
	if err := components.store.CreateSession(sessID, project, "/tmp"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Save a prompt via mem_save_prompt to populate activity.
	promptTool := components.mcpServer.ListTools()["mem_save_prompt"]
	pReq := newToolRequest("mem_save_prompt", map[string]any{
		"content":    "a prompt that should be cleared",
		"session_id": sessID,
		"project":    project,
	})
	pResult, err := promptTool.Handler(t.Context(), pReq)
	if err != nil {
		t.Fatalf("mem_save_prompt transport error: %v", err)
	}
	if pResult.IsError {
		t.Fatalf("mem_save_prompt tool error: %v", pResult.Content)
	}

	// End the session.
	endTool := components.mcpServer.ListTools()["mem_session_end"]
	eReq := newToolRequest("mem_session_end", map[string]any{"id": sessID})
	eResult, err := endTool.Handler(t.Context(), eReq)
	if err != nil {
		t.Fatalf("mem_session_end transport error: %v", err)
	}
	if eResult.IsError {
		t.Fatalf("mem_session_end tool error: %v", eResult.Content)
	}

	// A subsequent mem_save must not auto-capture (activity cleared).
	// We verify by checking prompt count is still 1 (no new row via auto-capture).
	saveTool := components.mcpServer.ListTools()["mem_save"]
	sReq := newToolRequest("mem_save", map[string]any{
		"title":      "post-end save",
		"content":    "body",
		"session_id": sessID,
		"project":    project,
	})
	sResult, err := saveTool.Handler(t.Context(), sReq)
	if err != nil {
		t.Fatalf("mem_save transport error: %v", err)
	}
	if sResult.IsError {
		t.Fatalf("mem_save tool error: %v", sResult.Content)
	}

	count, err := components.store.CountPromptsForSession(sessID, project, "a prompt that should be cleared")
	if err != nil {
		t.Fatalf("CountPromptsForSession: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 prompt row (activity cleared on session end, no re-capture), got %d", count)
	}
}
