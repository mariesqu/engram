package main

// tools_update_test.go — mem_update MCP tool handler tests.
//   - Partial edit: omitted fields keep their current value; version bumps.
//   - Missing id → tool error (not a transport error).
//   - No changeable field provided → tool error.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mariesqu/engram/internal/localstore"
)

func newUpdateTestDaemon(t *testing.T) *daemonComponents {
	t.Helper()
	components, err := buildDaemon(daemonCfg{
		db:           filepath.Join(t.TempDir(), "update.db"),
		syncInterval: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)
	return components
}

// TestMemUpdate_PartialEditPreservesOmittedFields edits only the content and
// confirms the title is preserved and the version is bumped. loop/embedLoop are
// nil here (no central, Noop provider) — exercising the nil-safe trigger path.
func TestMemUpdate_PartialEditPreservesOmittedFields(t *testing.T) {
	components := newUpdateTestDaemon(t)

	res, err := components.store.AddObservation(localstore.AddObservationParams{
		SessionID: "sess1",
		Type:      "manual",
		Title:     "original title",
		Content:   "original content",
		Project:   "proj",
		Scope:     "project",
		WriterID:  "w1",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	tool := handleUpdate(components.store, components.loop, components.embedLoop, "w1")
	req := newToolRequest("mem_update", map[string]any{
		"id":      float64(res.ID),
		"content": "revised content",
	})
	result, err := tool(t.Context(), req)
	if err != nil {
		t.Fatalf("mem_update transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("mem_update tool error: %s", result.Content[0].(mcp.TextContent).Text)
	}

	rec, err := components.store.GetObservation(res.ID)
	if err != nil {
		t.Fatalf("GetObservation: %v", err)
	}
	if rec.Content != "revised content" {
		t.Errorf("content = %q; want %q", rec.Content, "revised content")
	}
	if rec.Title != "original title" {
		t.Errorf("title = %q; want preserved %q", rec.Title, "original title")
	}
	if rec.Version != 2 {
		t.Errorf("version = %d; want 2 (bumped)", rec.Version)
	}
}

// TestMemUpdate_NotFound confirms a missing id yields a tool error, not a crash.
func TestMemUpdate_NotFound(t *testing.T) {
	components := newUpdateTestDaemon(t)
	tool := handleUpdate(components.store, components.loop, components.embedLoop, "w1")
	req := newToolRequest("mem_update", map[string]any{
		"id":    float64(987654),
		"title": "does not matter",
	})
	result, err := tool(t.Context(), req)
	if err != nil {
		t.Fatalf("mem_update transport error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected tool error for missing id; got success")
	}
}

// TestMemUpdate_NoChangeableField confirms an id with no title/content/type is
// rejected with a clear tool error rather than a pointless no-op version bump.
func TestMemUpdate_NoChangeableField(t *testing.T) {
	components := newUpdateTestDaemon(t)
	res, err := components.store.AddObservation(localstore.AddObservationParams{
		SessionID: "sess1", Type: "manual", Title: "t", Content: "c",
		Project: "proj", Scope: "project", WriterID: "w1",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	tool := handleUpdate(components.store, components.loop, components.embedLoop, "w1")
	req := newToolRequest("mem_update", map[string]any{"id": float64(res.ID)})
	result, err := tool(t.Context(), req)
	if err != nil {
		t.Fatalf("mem_update transport error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected tool error when no title/content/type provided")
	}
}

// TestMemSuggestTopicKey covers the deterministic suggestion + the required-title
// guard. No daemon needed — the handler is a pure function.
func TestMemSuggestTopicKey(t *testing.T) {
	tool := handleSuggestTopicKey()

	res, err := tool(t.Context(), newToolRequest("mem_suggest_topic_key", map[string]any{
		"title": "Use cookie auth over JWT",
		"type":  "decision",
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool error: %s", res.Content[0].(mcp.TextContent).Text)
	}
	if got := res.Content[0].(mcp.TextContent).Text; got != "decision/use-cookie-auth-over-jwt" {
		t.Errorf("suggested key = %q; want decision/use-cookie-auth-over-jwt", got)
	}

	missing, err := tool(t.Context(), newToolRequest("mem_suggest_topic_key", map[string]any{}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !missing.IsError {
		t.Fatalf("expected tool error for missing title")
	}
}
