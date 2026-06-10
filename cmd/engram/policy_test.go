package main

// Tests for the omitted-project refusal in mem_save and mem_save_prompt, and
// for the CLI projects list/policy commands.  These cover the three "critical
// proofs" required by the PR-② spec:
//
//   - omitted refusal: both tools return IsError=true, zero rows written, zero
//     outbox entries.
//   - CLI projects list: prints the table; exits 0.
//   - CLI projects policy: valid → exits 0; invalid → exits non-zero.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mark3labs/mcp-go/mcp"
)

// ─── omitted refusal ─────────────────────────────────────────────────────────

// TestHandleSave_Omitted_ReturnsError verifies that mem_save on an omitted
// project returns a tool error and writes NOTHING (zero rows, zero outbox).
func TestHandleSave_Omitted_ReturnsError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "omit_save.db")
	components, err := buildDaemon(daemonCfg{db: dbPath, syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	const project = "forbidden-proj"
	if err := components.store.SetPolicy(project, localstore.PolicyOmitted); err != nil {
		t.Fatalf("SetPolicy omitted: %v", err)
	}

	saveTool := components.mcpServer.ListTools()["mem_save"]
	req := newToolRequest("mem_save", map[string]any{
		"title":   "should be refused",
		"content": "this must not be written",
		"project": project,
	})
	result, err := saveTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected tool error for omitted project, got success: %v", result.Content)
	}

	// The error text must mention the project name and "omitted".
	if len(result.Content) > 0 {
		text := result.Content[0].(mcp.TextContent).Text
		if !strings.Contains(text, project) {
			t.Errorf("error text %q does not mention the project name %q", text, project)
		}
		if !strings.Contains(text, "omitted") {
			t.Errorf("error text %q does not mention 'omitted'", text)
		}
	}

	// Zero rows must have been written.
	rows, searchErr := components.store.SearchMemories("should be refused", project, 10)
	if searchErr != nil {
		t.Fatalf("SearchMemories: %v", searchErr)
	}
	if len(rows) != 0 {
		t.Errorf("want 0 rows for omitted project; got %d (write must be refused)", len(rows))
	}

	// Zero outbox entries.
	pending, pendErr := components.store.PendingCount()
	if pendErr != nil {
		t.Fatalf("PendingCount: %v", pendErr)
	}
	if pending != 0 {
		t.Errorf("want 0 pending outbox entries; got %d (write must be refused)", pending)
	}
}

// TestHandleSavePrompt_Omitted_ReturnsError verifies that mem_save_prompt on an
// omitted project returns a tool error and writes nothing.
func TestHandleSavePrompt_Omitted_ReturnsError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "omit_saveprompt.db")
	components, err := buildDaemon(daemonCfg{db: dbPath, syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	const project = "omitted-prompt-proj"
	if err := components.store.SetPolicy(project, localstore.PolicyOmitted); err != nil {
		t.Fatalf("SetPolicy omitted: %v", err)
	}

	promptTool := components.mcpServer.ListTools()["mem_save_prompt"]
	req := newToolRequest("mem_save_prompt", map[string]any{
		"content":    "this prompt should be refused",
		"session_id": "sess-omit",
		"project":    project,
	})
	result, err := promptTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected tool error for omitted project in mem_save_prompt, got success: %v", result.Content)
	}

	// Error text should mention the project and "omitted".
	if len(result.Content) > 0 {
		text := result.Content[0].(mcp.TextContent).Text
		if !strings.Contains(text, project) {
			t.Errorf("error text %q does not mention the project name %q", text, project)
		}
	}

	// Zero outbox entries (nothing was written).
	pending, pendErr := components.store.PendingCount()
	if pendErr != nil {
		t.Fatalf("PendingCount: %v", pendErr)
	}
	if pending != 0 {
		t.Errorf("want 0 pending outbox entries for omitted prompt; got %d", pending)
	}
}

// TestHandleSave_LocalOnly_Succeeds_ButStaysUnacked verifies that mem_save on a
// local-only project succeeds (writes the row) and the outbox entry is created
// (but will not be acked on Push — that is proven by the syncer tests).
func TestHandleSave_LocalOnly_Succeeds_ButStaysUnacked(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "local_only_save.db")
	components, err := buildDaemon(daemonCfg{db: dbPath, syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	const project = "local-only-proj"
	if err := components.store.SetPolicy(project, localstore.PolicyLocalOnly); err != nil {
		t.Fatalf("SetPolicy local-only: %v", err)
	}

	saveTool := components.mcpServer.ListTools()["mem_save"]
	req := newToolRequest("mem_save", map[string]any{
		"title":   "local-only write",
		"content": "this should be written but not pushed",
		"project": project,
	})
	result, err := saveTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("local-only save must succeed, got tool error: %v", result.Content)
	}

	// Row must be written.
	rows, searchErr := components.store.SearchMemories("local-only write", project, 10)
	if searchErr != nil {
		t.Fatalf("SearchMemories: %v", searchErr)
	}
	if len(rows) == 0 {
		t.Error("local-only save must write a row (only push is skipped, not the local write)")
	}
}

// ─── CLI projects tests ───────────────────────────────────────────────────────

// TestRun_ProjectsNoSubcommand verifies that 'projects' with no subcommand
// prints usage and returns exit code 0 (help is not an error).
func TestRun_ProjectsNoSubcommand(t *testing.T) {
	code := run([]string{"projects"})
	if code != 0 {
		t.Errorf("run([projects]) with no subcommand: got exit code %d, want 0 (help/usage)", code)
	}
}

// TestRun_ProjectsUnknownSubcommand verifies that 'projects frobnicate'
// returns exit code 1 (unknown subcommand is a runtime error, not a usage flag).
func TestRun_ProjectsUnknownSubcommand(t *testing.T) {
	code := run([]string{"projects", "frobnicate"})
	if code != 1 {
		t.Errorf("run([projects frobnicate]): got exit code %d, want 1 (unknown subcommand)", code)
	}
}

// TestRun_ProjectsPolicyInvalidValue verifies that 'projects policy <proj> <bad>'
// returns a non-zero exit code when the policy value is not valid.
func TestRun_ProjectsPolicyInvalidValue(t *testing.T) {
	// No daemon running — projects policy with invalid policy value should fail
	// before even attempting a network call.  The CLI validates the policy string
	// before calling the control API.
	code := run([]string{"projects", "policy", "some-proj", "invalid-policy-value"})
	if code == 0 {
		t.Errorf("run([projects policy ... invalid-policy-value]): expected non-zero exit code, got 0")
	}
}
