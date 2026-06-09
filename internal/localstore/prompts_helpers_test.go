package localstore

import "testing"

// TestPromptReadHelpers_NormalizeProject guards the fix: the read helpers must
// normalize the project the same way AddPrompt does, so a mixed-case query still
// finds the row (AddPrompt lowercases the project before storing).
func TestPromptReadHelpers_NormalizeProject(t *testing.T) {
	s := openTempStore(t)

	if _, err := s.AddPrompt(AddPromptParams{
		SessionID: "s1", Content: "hello there", Project: "MixedCaseProj",
	}); err != nil {
		t.Fatalf("AddPrompt: %v", err)
	}

	// Query with the ORIGINAL mixed-case spelling — the helper must normalize it.
	n, err := s.CountPromptsForSession("s1", "MixedCaseProj", "hello there")
	if err != nil {
		t.Fatalf("CountPromptsForSession: %v", err)
	}
	if n != 1 {
		t.Errorf("CountPromptsForSession(mixed-case project) = %d, want 1 (project not normalized)", n)
	}

	if _, err := s.GetPromptBySessionAndContent("s1", "MixedCaseProj", "hello there"); err != nil {
		t.Errorf("GetPromptBySessionAndContent(mixed-case project): %v (project not normalized)", err)
	}
}
