package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// ─── mem_search handler tests ─────────────────────────────────────────────────

// TestDaemonTool_MemSearch_FindsSavedMemory verifies end-to-end that a memory
// saved via mem_save is discoverable via mem_search.
func TestDaemonTool_MemSearch_FindsSavedMemory(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "search.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	// Create a session so resolveSaveProject has a project to attach to.
	if err := components.store.CreateSession("search-sess", "search-project", "/tmp/search"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	saveTool := components.mcpServer.ListTools()["mem_save"]
	saveReq := newToolRequest("mem_save", map[string]any{
		"title":      "hexagonal architecture decision",
		"content":    "chose hexagonal over layered for better testability",
		"type":       "decision",
		"project":    "search-project",
		"session_id": "search-sess",
	})
	saveResult, err := saveTool.Handler(t.Context(), saveReq)
	if err != nil {
		t.Fatalf("mem_save transport error: %v", err)
	}
	if saveResult.IsError {
		t.Fatalf("mem_save tool error: %v", saveResult.Content)
	}

	// Now search for it.
	searchTool, ok := components.mcpServer.ListTools()["mem_search"]
	if !ok {
		t.Fatal("mem_search not registered")
	}

	searchReq := newToolRequest("mem_search", map[string]any{
		"query":   "hexagonal",
		"project": "search-project",
	})
	result, err := searchTool.Handler(t.Context(), searchReq)
	if err != nil {
		t.Fatalf("mem_search transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("mem_search tool error: %v", result.Content)
	}

	if len(result.Content) == 0 {
		t.Fatal("mem_search returned empty content")
	}
	text, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(text.Text, "hexagonal") {
		t.Errorf("mem_search result does not contain the search term; got:\n%s", text.Text)
	}
}

// TestDaemonTool_MemSearch_TypeFilter verifies that passing type= narrows
// results — a "bugfix" query should not return "decision" rows.
func TestDaemonTool_MemSearch_TypeFilter(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "filter.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	if err := components.store.CreateSession("filter-sess", "filter-proj", "/tmp/filter"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	saveTool := components.mcpServer.ListTools()["mem_save"]

	// Save a decision.
	saveReq := newToolRequest("mem_save", map[string]any{
		"title":      "auth filter decision",
		"content":    "chose cookie auth over JWT",
		"type":       "decision",
		"project":    "filter-proj",
		"session_id": "filter-sess",
	})
	if r, err := saveTool.Handler(t.Context(), saveReq); err != nil || r.IsError {
		t.Fatalf("mem_save decision: err=%v isError=%v content=%v", err, r.IsError, r.Content)
	}

	// Save a bugfix.
	saveReq2 := newToolRequest("mem_save", map[string]any{
		"title":      "auth filter bugfix",
		"content":    "fixed cookie expiry edge case",
		"type":       "bugfix",
		"project":    "filter-proj",
		"session_id": "filter-sess",
	})
	if r, err := saveTool.Handler(t.Context(), saveReq2); err != nil || r.IsError {
		t.Fatalf("mem_save bugfix: err=%v isError=%v content=%v", err, r.IsError, r.Content)
	}

	searchTool := components.mcpServer.ListTools()["mem_search"]

	// Search for "auth" with type=decision — should surface the decision, not the bugfix.
	req := newToolRequest("mem_search", map[string]any{
		"query":   "auth filter",
		"project": "filter-proj",
		"type":    "decision",
	})
	result, err := searchTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("mem_search transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("mem_search tool error: %v", result.Content)
	}

	if len(result.Content) == 0 {
		t.Fatal("mem_search empty content")
	}
	text := result.Content[0].(mcp.TextContent)

	// The decision title must appear.
	if !strings.Contains(text.Text, "auth filter decision") {
		t.Errorf("type=decision filter did not surface the decision row; got:\n%s", text.Text)
	}
	// The bugfix must NOT appear.
	if strings.Contains(text.Text, "auth filter bugfix") {
		t.Errorf("type=decision filter leaked the bugfix row; got:\n%s", text.Text)
	}
}

// TestDaemonTool_MemSearch_ScopeFilter verifies that passing scope=personal
// narrows results to personal-scoped rows only.
func TestDaemonTool_MemSearch_ScopeFilter(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "scope.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	if err := components.store.CreateSession("scope-sess", "scope-proj", "/tmp/scope"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	saveTool := components.mcpServer.ListTools()["mem_save"]

	// Save a project-scoped memory.
	projectReq := newToolRequest("mem_save", map[string]any{
		"title":      "scope test project memory",
		"content":    "scoped to the project team",
		"scope":      "project",
		"project":    "scope-proj",
		"session_id": "scope-sess",
	})
	if r, err := saveTool.Handler(t.Context(), projectReq); err != nil || r.IsError {
		t.Fatalf("mem_save project: err=%v isError=%v", err, r.IsError)
	}

	// Save a personal-scoped memory.
	personalReq := newToolRequest("mem_save", map[string]any{
		"title":      "scope test personal memory",
		"content":    "personal preference note",
		"scope":      "personal",
		"project":    "scope-proj",
		"session_id": "scope-sess",
	})
	if r, err := saveTool.Handler(t.Context(), personalReq); err != nil || r.IsError {
		t.Fatalf("mem_save personal: err=%v isError=%v", err, r.IsError)
	}

	searchTool := components.mcpServer.ListTools()["mem_search"]

	req := newToolRequest("mem_search", map[string]any{
		"query":   "scope test",
		"project": "scope-proj",
		"scope":   "personal",
	})
	result, err := searchTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("mem_search transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("mem_search tool error: %v", result.Content)
	}

	if len(result.Content) == 0 {
		t.Fatal("mem_search empty content")
	}
	text := result.Content[0].(mcp.TextContent)

	if !strings.Contains(text.Text, "scope test personal memory") {
		t.Errorf("scope=personal filter did not surface the personal row; got:\n%s", text.Text)
	}
	if strings.Contains(text.Text, "scope test project memory") {
		t.Errorf("scope=personal filter leaked the project-scoped row; got:\n%s", text.Text)
	}
}

// TestDaemonTool_MemSearch_MissingQuery verifies that mem_search returns a tool
// error when query is absent rather than crashing or returning empty success.
func TestDaemonTool_MemSearch_MissingQuery(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "noquery.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	searchTool := components.mcpServer.ListTools()["mem_search"]
	req := newToolRequest("mem_search", map[string]any{}) // no query
	result, err := searchTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected tool error for missing query, got success: %v", result.Content)
	}
}

// TestDaemonTool_MemSearch_LenientOnNoCwd verifies that mem_search does NOT
// error on ambiguous or missing project detection — it should return empty
// results rather than a tool error (lenient read contract).
func TestDaemonTool_MemSearch_LenientOnNoCwd(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "lenient.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	// Query something with no project argument and nothing in the DB — lenient
	// read should not error even if project detection returns empty.
	searchTool := components.mcpServer.ListTools()["mem_search"]
	req := newToolRequest("mem_search", map[string]any{"query": "something that does not exist anywhere"})
	result, err := searchTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	// IsError is acceptable only if query was empty, which it is not here.
	// A missing project should not be fatal for read tools.
	if result.IsError {
		if len(result.Content) > 0 {
			if txt, ok := result.Content[0].(mcp.TextContent); ok {
				t.Logf("tool error text: %s", txt.Text)
			}
		}
		t.Error("mem_search should not return a tool error for a valid query with no project (lenient read contract)")
	}
}

// ─── mem_context handler tests ───────────────────────────────────────────────

// TestDaemonTool_MemContext_ReturnsSessionAndObservation verifies that
// mem_context assembles a non-empty blob that contains a recent session and a
// recent observation when both exist.
func TestDaemonTool_MemContext_ReturnsSessionAndObservation(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "context.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	// Seed a session.
	if err := components.store.CreateSession("ctx-sess-1", "ctx-project", "/path"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Save an observation via the handler to ensure the full write path is exercised.
	saveTool := components.mcpServer.ListTools()["mem_save"]
	saveReq := newToolRequest("mem_save", map[string]any{
		"title":      "context observation marker",
		"content":    "this is the context observation body",
		"type":       "discovery",
		"project":    "ctx-project",
		"session_id": "ctx-sess-1",
	})
	if r, err := saveTool.Handler(t.Context(), saveReq); err != nil || r.IsError {
		t.Fatalf("mem_save: err=%v isError=%v content=%v", err, r.IsError, r.Content)
	}

	contextTool, ok := components.mcpServer.ListTools()["mem_context"]
	if !ok {
		t.Fatal("mem_context not registered")
	}

	req := newToolRequest("mem_context", map[string]any{"project": "ctx-project"})
	result, err := contextTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("mem_context transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("mem_context tool error: %v", result.Content)
	}

	if len(result.Content) == 0 {
		t.Fatal("mem_context returned empty content")
	}
	text, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}

	if !strings.Contains(text.Text, "## Memory from Previous Sessions") {
		t.Errorf("mem_context result missing header; got:\n%s", text.Text)
	}
	if !strings.Contains(text.Text, "ctx-project") {
		t.Errorf("mem_context result does not contain the project name; got:\n%s", text.Text)
	}
	if !strings.Contains(text.Text, "context observation marker") {
		t.Errorf("mem_context result does not contain the observation title; got:\n%s", text.Text)
	}
}

// TestDaemonTool_MemContext_EmptyWhenNoData verifies that mem_context returns a
// graceful "no context" message (not a tool error) when the project has no data.
func TestDaemonTool_MemContext_EmptyWhenNoData(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "ctx_empty.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	contextTool := components.mcpServer.ListTools()["mem_context"]
	req := newToolRequest("mem_context", map[string]any{"project": "no-such-project"})
	result, err := contextTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("mem_context transport error: %v", err)
	}
	// Must NOT be a tool error for an empty project.
	if result.IsError {
		t.Errorf("mem_context should not error on empty project, got tool error: %v", result.Content)
	}
	// Result should still have content (e.g. "no context" message).
	if len(result.Content) == 0 {
		t.Error("mem_context should return at least one content item even for empty project")
	}
}

// TestDaemonTool_MemContext_LenientOnNoCwd verifies the lenient-read contract:
// mem_context does NOT error when called with no project argument, even if
// project detection from cwd is ambiguous or unavailable.
func TestDaemonTool_MemContext_LenientOnNoCwd(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "ctx_lenient.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	contextTool := components.mcpServer.ListTools()["mem_context"]
	req := newToolRequest("mem_context", map[string]any{}) // no project
	result, err := contextTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if result.IsError {
		if len(result.Content) > 0 {
			if txt, ok := result.Content[0].(mcp.TextContent); ok {
				t.Logf("tool error text: %s", txt.Text)
			}
		}
		t.Error("mem_context should not tool-error on missing project argument (lenient read contract)")
	}
}
