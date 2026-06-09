package main

import (
	"path/filepath"
	"testing"
	"time"
)

// TestDaemonTool_MemJudge_RejectsBadConfidence verifies mem_judge rejects a
// confidence outside 0..1 (the guard runs before JudgeRelation, so the judgment_id
// need not exist). A corrupt confidence would otherwise poison the agent heuristic
// ("confidence < 0.7 → surface to the user").
func TestDaemonTool_MemJudge_RejectsBadConfidence(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "engram.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	judgeTool := components.mcpServer.ListTools()["mem_judge"]
	for _, bad := range []float64{-0.5, 1.5, 2} {
		req := newToolRequest("mem_judge", map[string]any{
			"judgment_id": "rel-whatever",
			"relation":    "related",
			"confidence":  bad,
		})
		result, err := judgeTool.Handler(t.Context(), req)
		if err != nil {
			t.Fatalf("confidence=%v: transport error: %v", bad, err)
		}
		if !result.IsError {
			t.Errorf("confidence=%v: expected a tool error (out of 0..1 range), got success", bad)
		}
	}
}
