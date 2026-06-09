package localstore

import (
	"path/filepath"
	"testing"

	"github.com/mariesqu/engram/internal/domain"
)

// TestApplyTx_RejectsPromptEntity verifies the defense-in-depth guard: a prompt
// mutation that wrongly reaches the memories apply path is rejected with a clear
// domain error rather than a cryptic entity_type CHECK violation at the INSERT.
// Prompts are materialized into user_prompts, never memories.
func TestApplyTx_RejectsPromptEntity(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "guard.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := applyTx(tx,
		domain.Decision{Action: domain.ActionInsert, TargetSyncID: "prompt-x"},
		domain.Mutation{EntityType: domain.EntityPrompt, SyncID: "prompt-x"},
	); err == nil {
		t.Error("applyTx must reject an EntityPrompt mutation (it belongs in user_prompts, not memories)")
	}
}
