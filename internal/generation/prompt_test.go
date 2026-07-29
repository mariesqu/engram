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

// verificationWords are the five words REQ-PROV-05 forbids applying to a
// completion or behavioural claim (the spec's own list). BuildPrompt's
// attribution-voice text legitimately contains some of them as part of a
// DENIAL ("never independently verified") -- the only usage REQ-PROV-05
// permits -- so assertNoAffirmedVerification only flags an occurrence NOT
// preceded by a nearby negation marker. Declared independently of
// internal/obsidian's identically-named helper (narrative_test.go): the two
// packages may not share test code across the REQ-GEN-08 import boundary,
// and ~15 duplicated lines beats inventing a third package for one helper --
// the same tradeoff design #4754 §7 already accepts for chatCompletionsURL.
var verificationWords = []string{"verified", "confirmed", "validated", "proven", "checked"}

var negationMarkers = []string{"not ", "never ", "no ", "n't "}

func assertNoAffirmedVerification(t *testing.T, label, text string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, word := range verificationWords {
		searchFrom := 0
		for {
			idx := strings.Index(lower[searchFrom:], word)
			if idx < 0 {
				break
			}
			pos := searchFrom + idx
			windowStart := pos - 50
			if windowStart < 0 {
				windowStart = 0
			}
			window := lower[windowStart:pos]
			negated := false
			for _, marker := range negationMarkers {
				if strings.Contains(window, marker) {
					negated = true
					break
				}
			}
			if !negated {
				t.Errorf("%s: %q at byte %d is not preceded by a negation (window %q) — REQ-PROV-05 forbids applying a verification word to a completion/behavioural claim; the only permitted usage is a denial (\"never verified\", \"NOT checked\"):\n%s",
					label, word, pos, window, text)
			}
			searchFrom = pos + len(word)
		}
	}
}

// TestProvenanceClaimsNoVerificationItCannotPerform is the generation half
// of task 8.23 (the obsidian half lives in
// internal/obsidian/narrative_test.go). It is a GUARD in the literal sense
// -- it asserts an ABSENCE -- and it is the ONLY mechanical form REQ-PROV-05
// can take: no filesystem, process or test-runner oracle exists in this
// change's scope to check a completion or behavioural claim, so the prompt
// template itself must never assert one of the banned words about such a
// claim in an affirmative (non-negated) way.
func TestProvenanceClaimsNoVerificationItCannotPerform(t *testing.T) {
	u := Unit{
		ProjectKey:     "engram",
		TopicPrefixKey: "sdd/obsidian-narrative",
		Records: []*domain.Record{
			{SyncID: "s1", Title: "Fixed cache key", Content: "Length-prefixed SHA-256 over sorted records."},
		},
	}
	assertNoAffirmedVerification(t, "prompt template", BuildPrompt(u))
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
