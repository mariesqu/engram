package main

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

// TestDaemonTool_GetObservation_RejectsBadID verifies mem_get_observation rejects
// non-integer, non-positive, and out-of-int64-range float ids (MCP delivers JSON
// numbers as float64, which cannot represent every int64 above 2^53).
func TestDaemonTool_GetObservation_RejectsBadID(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "engram.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	getTool := components.mcpServer.ListTools()["mem_get_observation"]
	// float64(math.MaxInt64) == 2^63 is the exact boundary that int64() overflows.
	for _, badID := range []float64{1.5, -3, 0, float64(math.MaxInt64), 9.3e18} {
		req := newToolRequest("mem_get_observation", map[string]any{"id": badID})
		result, err := getTool.Handler(t.Context(), req)
		if err != nil {
			t.Fatalf("id=%v: transport error: %v", badID, err)
		}
		if !result.IsError {
			t.Errorf("id=%v: expected a tool error (non-integer/out-of-range), got success", badID)
		}
	}
}

// TestDaemonTool_SessionSummary_UsesSessionProject verifies mem_session_summary
// stores the summary under the SESSION's project (captured at session start),
// not the daemon process's cwd.
func TestDaemonTool_SessionSummary_UsesSessionProject(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "engram.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	if err := components.store.CreateSession("sess-sum", "session-project", "/some/client/dir"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	sumTool := components.mcpServer.ListTools()["mem_session_summary"]
	req := newToolRequest("mem_session_summary", map[string]any{"content": "did stuff this session", "session_id": "sess-sum"})
	result, err := sumTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	// The summary must be searchable under the session's project, not the daemon cwd.
	hits, err := components.store.SearchMemories("did stuff this session", "session-project", 10)
	if err != nil {
		t.Fatalf("SearchMemories: %v", err)
	}
	if len(hits) == 0 {
		t.Error("session summary not stored under the session's project 'session-project' (used the daemon cwd instead?)")
	}
}
