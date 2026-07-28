package obsidian

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

func strPtr(s string) *string { return &s }

// TestObservationToMarkdown covers REQ-EXPORT-03: YAML frontmatter (type,
// project, scope, topic_key, session_id, created_at, tags) + an H1 title +
// content body + a "## Wikilinks" section. TopicKey and Status are pointers
// and may be nil — a nil pointer must never render as the literal "<nil>".
func TestObservationToMarkdown(t *testing.T) {
	created := time.Date(2026, 7, 27, 14, 50, 23, 0, time.UTC)

	t.Run("full record renders complete frontmatter and body", func(t *testing.T) {
		rec := &domain.Record{
			ID:        12,
			SessionID: "sess-1",
			Type:      "architecture",
			Title:     "Some Decision",
			Content:   "Decision body text.",
			Project:   "engram",
			Scope:     "project",
			TopicKey:  strPtr("sdd/obsidian-export-rebuild/spec"),
			Status:    strPtr("active"),
			CreatedAt: created,
		}

		got := ObservationToMarkdown(rec)

		wantContains := []string{
			"---\n",
			"type: \"architecture\"\n",
			"project: \"engram\"\n",
			"scope: \"project\"\n",
			"topic_key: \"sdd/obsidian-export-rebuild/spec\"\n",
			"session_id: \"sess-1\"\n",
			"created_at: 2026-07-27T14:50:23Z\n",
			"status: \"active\"\n",
			"tags: [\"architecture\"]\n",
			"# Some Decision\n",
			"Decision body text.",
			"## Wikilinks\n",
			"- [[_sessions/sess-1]]\n",
		}
		for _, want := range wantContains {
			if !strings.Contains(got, want) {
				t.Errorf("ObservationToMarkdown() missing %q in output:\n%s", want, got)
			}
		}
	})

	t.Run("nil TopicKey omits the field and never prints <nil>", func(t *testing.T) {
		rec := &domain.Record{
			ID:        1,
			SessionID: "sess-2",
			Type:      "bugfix",
			Title:     "No topic",
			Content:   "body",
			Project:   "engram",
			Scope:     "project",
			TopicKey:  nil,
			CreatedAt: created,
		}
		got := ObservationToMarkdown(rec)
		if strings.Contains(got, "<nil>") {
			t.Errorf("ObservationToMarkdown() rendered <nil> for a nil TopicKey:\n%s", got)
		}
		if strings.Contains(got, "topic_key:") {
			t.Errorf("ObservationToMarkdown() emitted topic_key for a nil TopicKey:\n%s", got)
		}
	})

	t.Run("nil Status omits the field and never prints <nil>", func(t *testing.T) {
		rec := &domain.Record{
			ID:        2,
			SessionID: "sess-3",
			Type:      "pattern",
			Title:     "No status",
			Content:   "body",
			Project:   "engram",
			Scope:     "project",
			Status:    nil,
			CreatedAt: created,
		}
		got := ObservationToMarkdown(rec)
		if strings.Contains(got, "<nil>") {
			t.Errorf("ObservationToMarkdown() rendered <nil> for a nil Status:\n%s", got)
		}
		if strings.Contains(got, "status:") {
			t.Errorf("ObservationToMarkdown() emitted status for a nil Status:\n%s", got)
		}
	})

	t.Run("special YAML characters in field values are escaped", func(t *testing.T) {
		rec := &domain.Record{
			ID:        3,
			SessionID: `sess "weird"`,
			Type:      "decision",
			Title:     "Title with: colon",
			Content:   "body",
			Project:   `my"project`,
			Scope:     "project",
			CreatedAt: created,
		}
		got := ObservationToMarkdown(rec)
		if !strings.Contains(got, `project: "my\"project"`) {
			t.Errorf("ObservationToMarkdown() did not escape embedded quote in project:\n%s", got)
		}
		if !strings.Contains(got, `session_id: "sess \"weird\""`) {
			t.Errorf("ObservationToMarkdown() did not escape embedded quotes in session_id:\n%s", got)
		}
	})
}

// lockedGraphTagQueries are the four tag-matching colour groups of the
// locked-in graph.json (design "Locked-In Visual Config", REQ-GRAPH-05).
// They are reproduced here because the synthesized `tags` frontmatter is the
// ONLY thing that makes them resolve: if the tag is not exactly the
// lowercased type, every one of these queries matches zero notes and the
// locked visual config renders a uniform grey graph.
var lockedGraphTagQueries = []string{
	"tag:#architecture",
	"tag:#bugfix",
	"tag:#decision",
	"tag:#pattern",
}

// lineWithPrefix returns the single rendered line starting with prefix.
func lineWithPrefix(t *testing.T, md, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no line starting with %q in rendered note:\n%s", prefix, md)
	return ""
}

// frontmatterBlock returns the text BETWEEN the opening and closing "---"
// fences. A note whose frontmatter block is malformed loses every property
// at once in Obsidian — including the tag that drives graph colouring — so
// the block's structural integrity is what these tests assert.
func frontmatterBlock(t *testing.T, md string) string {
	t.Helper()
	if !strings.HasPrefix(md, "---\n") {
		t.Fatalf("rendered note does not open with a frontmatter fence:\n%s", md)
	}
	rest := md[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatalf("rendered note has no closing frontmatter fence:\n%s", md)
	}
	return rest[:end]
}

// TestObservationTagsFromType is task 1.9: domain.Record has no Tags column,
// so the frontmatter `tags` field is synthesized from the LOWERCASED
// Record.Type. Assertions compare the whole rendered line exactly, so
// deleting the strings.ToLower in markdown.go breaks this test — the
// previous version used only already-lowercase fixtures plus a tautology
// (TrimPrefix("#"+x,"#") != x, which can never be true) and would have
// passed either way.
func TestObservationTagsFromType(t *testing.T) {
	cases := []struct {
		name    string
		typ     string
		wantTag string
	}{
		{"architecture", "architecture", "architecture"},
		{"bugfix", "bugfix", "bugfix"},
		{"decision", "decision", "decision"},
		{"pattern", "pattern", "pattern"},
		{"discovery", "discovery", "discovery"},
		{"mixed case is lowercased", "Architecture", "architecture"},
		{"upper case is lowercased", "BUGFIX", "bugfix"},
		{"mixed case with digits", "Decision2", "decision2"},
	}
	created := time.Date(2026, 7, 27, 14, 50, 23, 0, time.UTC)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &domain.Record{
				ID:        1,
				SessionID: "sess-1",
				Type:      tc.typ,
				Title:     "T",
				Content:   "C",
				Project:   "engram",
				Scope:     "project",
				CreatedAt: created,
			}
			got := ObservationToMarkdown(rec)

			gotLine := lineWithPrefix(t, got, "tags:")
			wantLine := fmt.Sprintf("tags: [%q]", tc.wantTag)
			if gotLine != wantLine {
				t.Errorf("ObservationToMarkdown() for type %q rendered %q, want %q", tc.typ, gotLine, wantLine)
			}

			// The tag Obsidian derives from that line must be exactly the
			// query the locked graph.json colours on.
			query := "tag:#" + tc.wantTag
			for _, coloured := range lockedGraphTagQueries {
				if coloured == query {
					return
				}
			}
			if tc.wantTag == "architecture" || tc.wantTag == "bugfix" || tc.wantTag == "decision" || tc.wantTag == "pattern" {
				t.Errorf("type %q produces graph query %q, which is not one of the locked colour groups %v", tc.typ, query, lockedGraphTagQueries)
			}
		})
	}
}

// TestFrontmatterSurvivesHostileFieldValues covers the one frontmatter field
// that bypassed yamlQuote: `tags` interpolated Record.Type raw, so a type of
// `a]` produced `tags: [a]]` — invalid YAML. Obsidian's response to invalid
// frontmatter is to drop the ENTIRE block, which kills graph colouring and
// every other property on that note at once. Record.Type is unvalidated user
// input (AddObservation only defaults an empty type and lowercases project).
func TestFrontmatterSurvivesHostileFieldValues(t *testing.T) {
	created := time.Date(2026, 7, 27, 14, 50, 23, 0, time.UTC)
	keyLine := regexp.MustCompile(`^[a-z_]+: `)

	cases := []struct {
		name    string
		typ     string
		wantTag string
	}{
		{"closing bracket would terminate the flow sequence early", "a]", `tags: ["a]"]`},
		{"double quote would terminate the scalar early", `a"b`, `tags: ["a\"b"]`},
		{"newline would break the block into a stray line", "a\nb", `tags: ["a\nb"]`},
		{"carriage return", "a\rb", `tags: ["a\rb"]`},
		{"tab", "a\tb", `tags: ["a\tb"]`},
		{"backslash would escape the closing quote", `a\`, `tags: ["a\\"]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &domain.Record{
				ID:        1,
				SessionID: "sess-1",
				Type:      tc.typ,
				Title:     "T",
				Content:   "C",
				Project:   "engram",
				Scope:     "project",
				CreatedAt: created,
			}
			got := ObservationToMarkdown(rec)

			fm := frontmatterBlock(t, got)
			for _, line := range strings.Split(fm, "\n") {
				if !keyLine.MatchString(line) {
					t.Errorf("frontmatter line %q is not a `key: value` pair — the block is malformed and Obsidian will drop every property on the note. Full block:\n%s", line, fm)
				}
			}
			if !strings.Contains(fm, tc.wantTag) {
				t.Errorf("frontmatter is missing %q; got:\n%s", tc.wantTag, fm)
			}
		})
	}
}
