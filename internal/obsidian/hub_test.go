package obsidian

import (
	"strings"
	"testing"
)

// TestSessionHub covers REQ-EXPORT-04: session hub notes are unconditional —
// every unique session_id gets a "_sessions/{id}.md" listing its exported
// observations as wikilinks, regardless of how many observations it has.
func TestSessionHub(t *testing.T) {
	t.Run("renders a wikilink for every observation in the session", func(t *testing.T) {
		refs := []ObsRef{
			{VaultPath: "engram/proj/architecture/foo-1", Title: "Foo"},
			{VaultPath: "engram/proj/bugfix/bar-2", Title: "Bar"},
			{VaultPath: "engram/proj/decision/baz-3", Title: "Baz"},
		}
		got := SessionHubMarkdown("sess-1", refs)

		if !strings.Contains(got, "sess-1") {
			t.Errorf("SessionHubMarkdown() missing session id in output:\n%s", got)
		}
		for _, r := range refs {
			want := "[[" + r.VaultPath + "|" + r.Title + "]]"
			if !strings.Contains(got, want) {
				t.Errorf("SessionHubMarkdown() missing wikilink %q:\n%s", want, got)
			}
		}
	})

	t.Run("a session with a single observation still gets a hub note", func(t *testing.T) {
		refs := []ObsRef{
			{VaultPath: "engram/proj/architecture/solo-9", Title: "Solo"},
		}
		got := SessionHubMarkdown("sess-solo", refs)

		want := "[[engram/proj/architecture/solo-9|Solo]]"
		if !strings.Contains(got, want) {
			t.Errorf("SessionHubMarkdown() with a single observation missing wikilink %q:\n%s", want, got)
		}
	})
}

// TestTopicHub covers REQ-EXPORT-05: the hub grouping prefix is the
// topic_key split on the LAST '/' (the recovered spec/design contradiction,
// resolved in favor of the design's stated grouping of SDD phases).
func TestTopicHub(t *testing.T) {
	t.Run("prefix is everything before the last slash", func(t *testing.T) {
		prefix, ok := TopicPrefix("sdd/obsidian-export-rebuild/spec")
		if !ok {
			t.Fatal("TopicPrefix() ok = false, want true")
		}
		if prefix != "sdd/obsidian-export-rebuild" {
			t.Errorf("TopicPrefix() = %q, want %q", prefix, "sdd/obsidian-export-rebuild")
		}

		refs := []ObsRef{
			{VaultPath: "engram/proj/architecture/a-1", Title: "A"},
			{VaultPath: "engram/proj/decision/b-2", Title: "B"},
		}
		got := TopicHubMarkdown(prefix, refs)
		for _, r := range refs {
			want := "[[" + r.VaultPath + "|" + r.Title + "]]"
			if !strings.Contains(got, want) {
				t.Errorf("TopicHubMarkdown() missing wikilink %q:\n%s", want, got)
			}
		}
	})

	t.Run("a single-segment prefix (one slash) is still a valid prefix", func(t *testing.T) {
		prefix, ok := TopicPrefix("discovery/foo")
		if !ok {
			t.Fatal("TopicPrefix() ok = false, want true")
		}
		if prefix != "discovery" {
			t.Errorf("TopicPrefix() = %q, want %q", prefix, "discovery")
		}
	})
}

// TestSessionHubDedupesRepeatedObsRef and TestTopicHubDedupesRepeatedObsRef
// cover defense-in-depth for bugfix/listprojects-case-variant-double-read
// (#4713): even if a caller's accumulation ever hands the render function
// the SAME observation twice (e.g. a future duplicate-producing path in
// Export()'s bySession/byTopicPrefix construction), the rendered hub note
// must list it exactly once.
func TestSessionHubDedupesRepeatedObsRef(t *testing.T) {
	ref := ObsRef{VaultPath: "engram/proj/architecture/foo-1", Title: "Foo"}
	got := SessionHubMarkdown("sess-1", []ObsRef{ref, ref})

	want := "[[engram/proj/architecture/foo-1|Foo]]"
	if n := strings.Count(got, want); n != 1 {
		t.Errorf("SessionHubMarkdown() rendered the wikilink %d times, want exactly 1:\n%s", n, got)
	}
}

func TestTopicHubDedupesRepeatedObsRef(t *testing.T) {
	ref := ObsRef{VaultPath: "engram/proj/architecture/foo-1", Title: "Foo"}
	got := TopicHubMarkdown("sdd/change", []ObsRef{ref, ref})

	want := "[[engram/proj/architecture/foo-1|Foo]]"
	if n := strings.Count(got, want); n != 1 {
		t.Errorf("TopicHubMarkdown() rendered the wikilink %d times, want exactly 1:\n%s", n, got)
	}
}

// TestHubMarkdownOrderIsStableAcrossRepeatedGeneration guards against Go's
// randomized map iteration leaking into rendered file content. The vault is
// git-synced, so nondeterministic ordering across otherwise-identical runs
// would produce a spurious diff every single time, even when nothing
// actually changed. This is a GUARD, not a caught regression: a correct
// dedup implementation (a linear scan over the input slice, never a map
// range for the OUTPUT order) already satisfies it — it exists to keep it
// that way.
func TestHubMarkdownOrderIsStableAcrossRepeatedGeneration(t *testing.T) {
	refs := []ObsRef{
		{VaultPath: "engram/proj/architecture/a-1", Title: "A"},
		{VaultPath: "engram/proj/bugfix/b-2", Title: "B"},
		{VaultPath: "engram/proj/decision/c-3", Title: "C"},
		{VaultPath: "engram/proj/architecture/a-1", Title: "A"}, // repeated
	}

	first := SessionHubMarkdown("sess-1", refs)
	for i := 0; i < 20; i++ {
		got := SessionHubMarkdown("sess-1", refs)
		if got != first {
			t.Fatalf("SessionHubMarkdown() is nondeterministic across repeated calls with identical input:\nrun 1:\n%s\nrun %d:\n%s", first, i+2, got)
		}
	}

	firstTopic := TopicHubMarkdown("sdd/change", refs)
	for i := 0; i < 20; i++ {
		got := TopicHubMarkdown("sdd/change", refs)
		if got != firstTopic {
			t.Fatalf("TopicHubMarkdown() is nondeterministic across repeated calls with identical input")
		}
	}
}

// TestTopicHubSkipped covers the two ways a topic hub is skipped: fewer than
// two observations share the prefix (REQ-EXPORT-05), or the topic_key has no
// '/' at all and therefore has no prefix.
func TestTopicHubSkipped(t *testing.T) {
	t.Run("a prefix shared by only one observation gets no hub", func(t *testing.T) {
		if ShouldCreateTopicHub(1) {
			t.Error("ShouldCreateTopicHub(1) = true, want false")
		}
	})

	t.Run("a topic_key with no slash has no prefix and produces no hub", func(t *testing.T) {
		_, ok := TopicPrefix("no-slash-topic")
		if ok {
			t.Error("TopicPrefix() ok = true for a slash-less topic_key, want false")
		}
	})
}
