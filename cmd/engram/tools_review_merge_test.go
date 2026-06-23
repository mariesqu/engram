package main

// tools_review_merge_test.go — handler tests for mem_review, mem_merge_projects,
// the save-time name-drift note (mem_save), and the Status line (mem_get_observation).

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mariesqu/engram/internal/localstore"
)

func newReviewMergeDaemon(t *testing.T) *daemonComponents {
	t.Helper()
	components, err := buildDaemon(daemonCfg{
		db:           filepath.Join(t.TempDir(), "rm.db"),
		syncInterval: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)
	return components
}

// markStale forces a row's updated_at into the past so it computes as needs_review
// under the default 30-day window.
func markStale(t *testing.T, c *daemonComponents, id int64) {
	t.Helper()
	if _, err := c.store.DB().Exec(
		`UPDATE memories SET updated_at = datetime('now','-40 days') WHERE id = ?`, id,
	); err != nil {
		t.Fatalf("markStale: %v", err)
	}
}

// TestMemReview_List verifies action="list" returns stale rows under needs_review.
func TestMemReview_List(t *testing.T) {
	c := newReviewMergeDaemon(t)
	res, err := c.store.AddObservation(localstore.AddObservationParams{
		Type: "decision", Title: "stale decision", Content: "c", Project: "p", Scope: "project",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	markStale(t, c, res.ID)

	tool := handleReview(c.store)
	result, err := tool(t.Context(), newToolRequest("mem_review", map[string]any{
		"action":  "list",
		"status":  "needs_review",
		"project": "p",
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].(mcp.TextContent).Text)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "stale decision") {
		t.Errorf("list result does not contain the stale memory:\n%s", text)
	}
	if !strings.Contains(text, "needs_review") {
		t.Errorf("list result missing status:\n%s", text)
	}
}

// TestMemReview_MarkReviewedByID verifies mark_reviewed via ids resets the clock.
func TestMemReview_MarkReviewedByID(t *testing.T) {
	c := newReviewMergeDaemon(t)
	res, err := c.store.AddObservation(localstore.AddObservationParams{
		Type: "decision", Title: "t", Content: "c", Project: "p", Scope: "project",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	markStale(t, c, res.ID)

	tool := handleReview(c.store)
	result, err := tool(t.Context(), newToolRequest("mem_review", map[string]any{
		"action": "mark_reviewed",
		"ids":    []any{float64(res.ID)},
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].(mcp.TextContent).Text)
	}
	if !strings.Contains(result.Content[0].(mcp.TextContent).Text, "Marked 1") {
		t.Errorf("unexpected mark_reviewed result: %s", result.Content[0].(mcp.TextContent).Text)
	}

	// The row is now active.
	st, _ := c.store.ReviewStatusForID(res.ID)
	if st != localstore.ReviewStatusActive {
		t.Errorf("post mark status = %q, want active", st)
	}
}

// TestMemReview_MarkReviewedByTopicKey verifies topic_key resolution.
func TestMemReview_MarkReviewedByTopicKey(t *testing.T) {
	c := newReviewMergeDaemon(t)
	res, err := c.store.AddObservation(localstore.AddObservationParams{
		Type: "architecture", Title: "auth", Content: "c", Project: "p", Scope: "project",
		TopicKey: "arch/auth",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	markStale(t, c, res.ID)

	tool := handleReview(c.store)
	result, err := tool(t.Context(), newToolRequest("mem_review", map[string]any{
		"action":    "mark_reviewed",
		"topic_key": "arch/auth",
		"project":   "p",
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].(mcp.TextContent).Text)
	}
	st, _ := c.store.ReviewStatusForID(res.ID)
	if st != localstore.ReviewStatusActive {
		t.Errorf("post mark status = %q, want active", st)
	}
}

// TestMemReview_MissingAction verifies a missing/invalid action errors.
func TestMemReview_MissingAction(t *testing.T) {
	c := newReviewMergeDaemon(t)
	tool := handleReview(c.store)
	result, err := tool(t.Context(), newToolRequest("mem_review", map[string]any{}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for missing action")
	}
}

// TestMemReview_MarkReviewedNoTarget verifies mark_reviewed with neither ids nor
// topic_key errors.
func TestMemReview_MarkReviewedNoTarget(t *testing.T) {
	c := newReviewMergeDaemon(t)
	tool := handleReview(c.store)
	result, err := tool(t.Context(), newToolRequest("mem_review", map[string]any{
		"action": "mark_reviewed",
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error when no ids or topic_key supplied")
	}
}

// TestMemReview_BadID verifies a non-integer id is rejected (mem_update rigor).
func TestMemReview_BadID(t *testing.T) {
	c := newReviewMergeDaemon(t)
	tool := handleReview(c.store)
	result, err := tool(t.Context(), newToolRequest("mem_review", map[string]any{
		"action": "mark_reviewed",
		"ids":    []any{float64(1.5)},
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for non-integer id")
	}
}

// TestMemMergeProjects_Moves verifies the merge handler renames source memories.
func TestMemMergeProjects_Moves(t *testing.T) {
	c := newReviewMergeDaemon(t)
	if _, err := c.store.AddObservation(localstore.AddObservationParams{
		Type: "manual", Title: "t", Content: "c", Project: "myapp", Scope: "project",
	}); err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	tool := handleMergeProjects(c.store)
	result, err := tool(t.Context(), newToolRequest("mem_merge_projects", map[string]any{
		"from": "myapp",
		"to":   "my-app",
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].(mcp.TextContent).Text)
	}
	if n, _ := c.store.CountLiveByProject("my-app"); n != 1 {
		t.Errorf("target count = %d, want 1", n)
	}
	if n, _ := c.store.CountLiveByProject("myapp"); n != 0 {
		t.Errorf("source count = %d, want 0", n)
	}
}

// TestMemMergeProjects_RequiredArgs verifies from/to are required.
func TestMemMergeProjects_RequiredArgs(t *testing.T) {
	c := newReviewMergeDaemon(t)
	tool := handleMergeProjects(c.store)

	r1, _ := tool(t.Context(), newToolRequest("mem_merge_projects", map[string]any{"to": "x"}))
	if !r1.IsError {
		t.Error("expected error for missing from")
	}
	r2, _ := tool(t.Context(), newToolRequest("mem_merge_projects", map[string]any{"from": "x"}))
	if !r2.IsError {
		t.Error("expected error for missing to")
	}
}

// TestMemSave_NameDriftNote verifies a save under a near-variant project surfaces
// the drift note, while an exact/distinct project does not.
func TestMemSave_NameDriftNote(t *testing.T) {
	c := newReviewMergeDaemon(t)

	// Seed an existing project "my-app".
	if _, err := c.store.AddObservation(localstore.AddObservationParams{
		Type: "manual", Title: "seed", Content: "c", Project: "my-app", Scope: "project",
	}); err != nil {
		t.Fatalf("seed AddObservation: %v", err)
	}

	saveTool := c.mcpServer.ListTools()["mem_save"]

	// Near-variant "myapp" → note expected.
	res, err := saveTool.Handler(t.Context(), newToolRequest("mem_save", map[string]any{
		"title": "drift", "content": "x", "project": "myapp",
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if res.IsError {
		t.Fatalf("save errored: %v", res.Content)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "note: project") || !strings.Contains(text, "my-app") {
		t.Errorf("expected drift note referencing my-app, got:\n%s", text)
	}

	// Exact match "my-app" → no note.
	res2, err := saveTool.Handler(t.Context(), newToolRequest("mem_save", map[string]any{
		"title": "exact", "content": "x", "project": "my-app",
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if strings.Contains(res2.Content[0].(mcp.TextContent).Text, "note: project") {
		t.Errorf("exact-match save should NOT carry a drift note:\n%s", res2.Content[0].(mcp.TextContent).Text)
	}

	// Clearly-distinct project → no note.
	res3, err := saveTool.Handler(t.Context(), newToolRequest("mem_save", map[string]any{
		"title": "distinct", "content": "x", "project": "totally-different-name",
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if strings.Contains(res3.Content[0].(mcp.TextContent).Text, "note: project") {
		t.Errorf("distinct save should NOT carry a drift note:\n%s", res3.Content[0].(mcp.TextContent).Text)
	}
}

// TestMemGetObservation_StatusLine verifies mem_get_observation surfaces a Status
// line reflecting the lifecycle status.
func TestMemGetObservation_StatusLine(t *testing.T) {
	c := newReviewMergeDaemon(t)
	res, err := c.store.AddObservation(localstore.AddObservationParams{
		Type: "decision", Title: "t", Content: "c", Project: "p", Scope: "project",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	markStale(t, c, res.ID)

	getTool := c.mcpServer.ListTools()["mem_get_observation"]
	result, err := getTool.Handler(t.Context(), newToolRequest("mem_get_observation", map[string]any{
		"id": float64(res.ID),
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "Status: needs_review") {
		t.Errorf("expected 'Status: needs_review' line, got:\n%s", text)
	}
}
