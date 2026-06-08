package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mark3labs/mcp-go/mcp"
)

// TestDaemonTool_Search_SurfacesRealID verifies mem_search prints the real
// integer id (not the literal "#?") so an agent can follow up with
// mem_get_observation(id).
func TestDaemonTool_Search_SurfacesRealID(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "engram.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	res, err := components.store.AddObservation(localstore.AddObservationParams{
		Title: "searchable title", Content: "unique-haystack-token here", Project: "proj", Type: "decision",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	searchTool := components.mcpServer.ListTools()["mem_search"]
	req := newToolRequest("mem_search", map[string]any{"query": "unique-haystack-token", "project": "proj"})
	result, err := searchTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}
	text := result.Content[0].(mcp.TextContent).Text

	if strings.Contains(text, "#?") {
		t.Errorf("mem_search still prints #? (no usable id):\n%s", text)
	}
	if want := fmt.Sprintf("#%d", res.ID); !strings.Contains(text, want) {
		t.Errorf("mem_search output missing the real id %q:\n%s", want, text)
	}
}

// TestDaemonTool_Search_PersonalScopeCrossProject verifies that mem_search with
// scope=personal and no explicit project searches ACROSS projects (REQ-391), so
// a personal memory saved under a different project than the daemon cwd is found.
func TestDaemonTool_Search_PersonalScopeCrossProject(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "engram.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	if _, err := components.store.AddObservation(localstore.AddObservationParams{
		Title: "personal note", Content: "personal-cross-project-token", Project: "some-other-project", Scope: "personal",
	}); err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	searchTool := components.mcpServer.ListTools()["mem_search"]
	// No explicit project; scope=personal → must search all projects, not the cwd one.
	req := newToolRequest("mem_search", map[string]any{"query": "personal-cross-project-token", "scope": "personal"})
	result, err := searchTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "personal note") {
		t.Errorf("scope=personal search did not find the cross-project personal memory (project-scoped instead of cross-project):\n%s", text)
	}
}
