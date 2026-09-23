package main

// tools_pin_test.go — handler tests for mem_pin / mem_unpin and their effect on
// the mem_context surface.

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mariesqu/engram/internal/localstore"
)

func newPinDaemon(t *testing.T) *daemonComponents {
	t.Helper()
	components, err := buildDaemon(daemonCfg{
		db:           filepath.Join(t.TempDir(), "pin.db"),
		syncInterval: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)
	return components
}

// callPin invokes a registered tool and decodes its JSON body.
func callPin(t *testing.T, c *daemonComponents, tool string, id int64) map[string]any {
	t.Helper()
	registered, ok := c.mcpServer.ListTools()[tool]
	if !ok {
		t.Fatalf("%s is not registered", tool)
	}
	result, err := registered.Handler(t.Context(), newToolRequest(tool, map[string]any{
		"id": float64(id),
	}))
	if err != nil {
		t.Fatalf("%s transport error: %v", tool, err)
	}
	if result.IsError {
		t.Fatalf("%s tool error: %v", tool, result.Content)
	}
	var body map[string]any
	text := result.Content[0].(mcp.TextContent).Text
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatalf("%s response is not JSON (%s): %v", tool, text, err)
	}
	return body
}

// TestMemPin_RoundTrip drives pin → unpin through the registered tools and
// checks the response envelope: the caller needs the id, the sync_id and the
// resulting state, not just a prose acknowledgement.
func TestMemPin_RoundTrip(t *testing.T) {
	c := newPinDaemon(t)

	res, err := c.store.AddObservation(localstore.AddObservationParams{
		Type: "decision", Title: "postgres only", Content: "never mssql",
		Project: "p", Scope: "project",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	body := callPin(t, c, "mem_pin", res.ID)
	if body["pinned"] != true {
		t.Errorf("mem_pin response pinned = %v, want true", body["pinned"])
	}
	if got, want := body["sync_id"], res.SyncID; got != want {
		t.Errorf("mem_pin response sync_id = %v, want %v", got, want)
	}
	if got, want := body["id"], float64(res.ID); got != want {
		t.Errorf("mem_pin response id = %v, want %v", got, want)
	}
	if pinned, err := c.store.IsPinned(res.ID); err != nil || !pinned {
		t.Errorf("row is not pinned in the store after mem_pin (pinned=%v err=%v)", pinned, err)
	}

	body = callPin(t, c, "mem_unpin", res.ID)
	if body["pinned"] != false {
		t.Errorf("mem_unpin response pinned = %v, want false", body["pinned"])
	}
	if pinned, err := c.store.IsPinned(res.ID); err != nil || pinned {
		t.Errorf("row is still pinned after mem_unpin (pinned=%v err=%v)", pinned, err)
	}
}

// TestMemPin_RejectsBadIDs covers the argument contract. An unknown id is a tool
// ERROR, not a quiet success: a caller told "pinned" about a memory that does
// not exist would wait forever for it to show up in context.
func TestMemPin_RejectsBadIDs(t *testing.T) {
	c := newPinDaemon(t)
	tool := c.mcpServer.ListTools()["mem_pin"]

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing id", map[string]any{}, "id is required"},
		{"non-numeric id", map[string]any{"id": "42"}, "id must be a number"},
		{"fractional id", map[string]any{"id": 4.5}, "id must be a positive integer"},
		{"zero id", map[string]any{"id": float64(0)}, "id must be a positive integer"},
		{"unknown id", map[string]any{"id": float64(999999)}, "not found"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tool.Handler(t.Context(), newToolRequest("mem_pin", tc.args))
			if err != nil {
				t.Fatalf("transport error: %v", err)
			}
			if !result.IsError {
				t.Fatalf("expected a tool error, got: %v", result.Content)
			}
			if text := result.Content[0].(mcp.TextContent).Text; !strings.Contains(text, tc.want) {
				t.Errorf("error %q does not mention %q", text, tc.want)
			}
		})
	}
}

// TestMemPin_SurfacesInMemContext is the reason the feature exists: a pinned
// memory has to lead the context blob the agent actually reads.
func TestMemPin_SurfacesInMemContext(t *testing.T) {
	c := newPinDaemon(t)

	pinned, err := c.store.AddObservation(localstore.AddObservationParams{
		Type: "decision", Title: "pinned decision", Content: "the one that matters",
		Project: "p", Scope: "project",
	})
	if err != nil {
		t.Fatalf("AddObservation(pinned): %v", err)
	}
	if _, err := c.store.AddObservation(localstore.AddObservationParams{
		Type: "manual", Title: "ordinary note", Content: "background",
		Project: "p", Scope: "project",
	}); err != nil {
		t.Fatalf("AddObservation(ordinary): %v", err)
	}

	callPin(t, c, "mem_pin", pinned.ID)

	ctxTool := c.mcpServer.ListTools()["mem_context"]
	result, err := ctxTool.Handler(t.Context(), newToolRequest("mem_context", map[string]any{
		"project": "p",
	}))
	if err != nil {
		t.Fatalf("mem_context transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("mem_context tool error: %v", result.Content)
	}
	text := result.Content[0].(mcp.TextContent).Text

	pinnedIdx := strings.Index(text, "### Pinned")
	recentIdx := strings.Index(text, "### Recent Observations")
	if pinnedIdx < 0 || recentIdx < 0 || pinnedIdx > recentIdx {
		t.Fatalf("mem_context must render '### Pinned' before '### Recent Observations'; got:\n%s", text)
	}
	if !strings.Contains(text[pinnedIdx:recentIdx], "pinned decision") {
		t.Errorf("pinned memory missing from the Pinned section:\n%s", text)
	}
	if strings.Contains(text[recentIdx:], "pinned decision") {
		t.Errorf("pinned memory repeated under Recent Observations:\n%s", text)
	}
}

// TestRegisterTools_PinToolsAreRegistered keeps the tool surface honest: the
// server must advertise exactly the 18 tools the docs and the MCP instructions
// text promise, with mem_pin/mem_unpin among them.
func TestRegisterTools_PinToolsAreRegistered(t *testing.T) {
	c := newPinDaemon(t)
	tools := c.mcpServer.ListTools()

	want := []string{
		"mem_current_project", "mem_session_start", "mem_session_end", "mem_save",
		"mem_save_prompt", "mem_get_observation", "mem_update", "mem_suggest_topic_key",
		"mem_search", "mem_context", "mem_pin", "mem_unpin", "mem_judge", "mem_similar",
		"mem_review", "mem_merge_projects", "mem_session_summary", "mem_doctor",
	}
	if len(tools) != len(want) {
		names := make([]string, 0, len(tools))
		for name := range tools {
			names = append(names, name)
		}
		t.Errorf("registered %d tools %v, want %d: %v", len(tools), names, len(want), want)
	}
	for _, name := range want {
		if _, ok := tools[name]; !ok {
			t.Errorf("tool %q is not registered", name)
		}
	}
}
