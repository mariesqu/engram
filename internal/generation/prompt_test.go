package generation

import (
	"strconv"
	"strings"
	"testing"

	"github.com/mariesqu/engram/internal/domain"
)

// TestBuildPromptCarriesAttributionVoice pins REQ-PROV-05's only available
// mitigation at the prompt layer: the model has no oracle for completion or
// behavioural claims, so the prompt must instruct it to write every claim
// as RECORDED rather than as independently verified fact. It must also
// name the source count and contain every member's title and content, so
// the model has the material to synthesise from.
func TestBuildPromptCarriesAttributionVoice(t *testing.T) {
	u := Unit{
		ProjectKey:     "engram",
		TopicPrefixKey: "sdd/obsidian-narrative",
		Records: []*domain.Record{
			{SyncID: "s1", Title: "Fixed cache key", Content: "Length-prefixed SHA-256 over sorted records."},
			{SyncID: "s2", Title: "Added path verification", Content: "os.Stat at generation time, never render time."},
		},
	}

	prompt := BuildPrompt(u)

	lower := strings.ToLower(prompt)
	if !strings.Contains(lower, "recorded") {
		t.Errorf("BuildPrompt output does not instruct the model to write claims as RECORDED: %q", prompt)
	}
	if !strings.Contains(prompt, strconv.Itoa(len(u.Records))) {
		t.Errorf("BuildPrompt output does not name the source count (%d): %q", len(u.Records), prompt)
	}
	for _, r := range u.Records {
		if !strings.Contains(prompt, r.Title) {
			t.Errorf("BuildPrompt output missing member title %q", r.Title)
		}
		if !strings.Contains(prompt, r.Content) {
			t.Errorf("BuildPrompt output missing member content %q", r.Content)
		}
	}
}

// TestBuildPromptContainsNoForeignProjectText triangulates REQ-GEN-03 at
// the prompt layer: BuildPrompt is a pure function of u.Records, so a unit
// scoped to project A never leaks project B's text into the prompt --
// there is no store access, no network, and no data source beyond what the
// caller placed in the Unit.
func TestBuildPromptContainsNoForeignProjectText(t *testing.T) {
	unitA := Unit{
		ProjectKey:     "project-a",
		TopicPrefixKey: "shared/prefix",
		Records: []*domain.Record{
			{SyncID: "a-1", Title: "A-only title", Content: "A-only content, never seen by B's prompt"},
		},
	}
	unitB := Unit{
		ProjectKey:     "project-b",
		TopicPrefixKey: "shared/prefix",
		Records: []*domain.Record{
			{SyncID: "b-1", Title: "B-only title, distinctive marker XYZZY", Content: "B-only content QWERTY"},
		},
	}

	promptA := BuildPrompt(unitA)
	if strings.Contains(promptA, "XYZZY") || strings.Contains(promptA, "QWERTY") {
		t.Errorf("project A's prompt leaked project B's text: %q", promptA)
	}

	promptB := BuildPrompt(unitB)
	if strings.Contains(promptB, "A-only") {
		t.Errorf("project B's prompt leaked project A's text: %q", promptB)
	}
}

// TestPromptTemplateVersionIsPinned guards the const identity BuildPrompt's
// callers (the cache key, a later slice) fold into SourceHash -- see
// REQ-NARR-01: bumping the template text without bumping this const would
// leave stale cache entries pointing at a prompt that no longer matches
// what they were generated from.
func TestPromptTemplateVersionIsPinned(t *testing.T) {
	if promptTemplateVersion == "" {
		t.Fatal("promptTemplateVersion must not be empty")
	}
}
