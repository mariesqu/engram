package obsidian

import (
	"fmt"
	"strings"
)

// ObsRef is a lightweight reference to an exported observation note, used to
// render session and topic hub pages without re-touching the full
// domain.Record. VaultPath is the wikilink target relative to the vault
// root, WITHOUT the trailing ".md" extension (Obsidian resolves wikilinks by
// note name/path, not by file extension).
//
// VaultPath MUST use forward slashes on EVERY platform
// (bugfix/obsidian-wikilink-path-separator): Obsidian resolves wikilinks
// with forward slashes always, on every OS -- a backslash is an ordinary,
// literal filename character to Obsidian's resolver, not a separator.
// Construct VaultPath only via vaultJoin (exporter.go/path.go) or an
// already-normalized value; never via a bare filepath.Join, which emits
// OS-native separators and, on Windows, silently produces an unresolvable
// link.
type ObsRef struct {
	VaultPath string
	Title     string
}

// SessionHubMarkdown renders the "_sessions/{session-id}.md" hub note listing
// every observation in the session as a wikilink (REQ-EXPORT-04). Session
// hubs are UNCONDITIONAL — every unique session_id gets one, regardless of
// how many observations it has.
func SessionHubMarkdown(sessionID string, refs []ObsRef) string {
	refs = dedupeRefs(refs)
	var b strings.Builder
	fmt.Fprintf(&b, "# Session %s\n\n", sessionID)
	b.WriteString("## Observations\n\n")
	for _, r := range refs {
		fmt.Fprintf(&b, "- [[%s|%s]]\n", r.VaultPath, r.Title)
	}
	return b.String()
}

// TopicHubMarkdown renders the "_topics/{prefix}.md" hub note listing every
// observation sharing that topic_key prefix as a wikilink (REQ-EXPORT-05).
// Callers MUST check ShouldCreateTopicHub before writing this to disk — the
// hub itself does not enforce the "at least 2 observations" rule.
func TopicHubMarkdown(prefix string, refs []ObsRef) string {
	refs = dedupeRefs(refs)
	var b strings.Builder
	fmt.Fprintf(&b, "# Topic %s\n\n", prefix)
	b.WriteString("## Observations\n\n")
	for _, r := range refs {
		fmt.Fprintf(&b, "- [[%s|%s]]\n", r.VaultPath, r.Title)
	}
	return b.String()
}

// dedupeRefs collapses refs so each VaultPath appears at most once, keeping
// the FIRST occurrence and the ORIGINAL relative order of first appearances.
// This is defense in depth for hub note generation
// (bugfix/listprojects-case-variant-double-read): even after the exporter
// dedupes case-variant project names before ever reaching
// RecentObservations, a hub-generating caller must never be able to make the
// same observation appear twice in a hub note — the render step is the last,
// cheapest, most generic place to catch that, independent of how the
// caller's accumulation produced its input.
//
// Implemented as a single linear scan over refs IN ORDER, with a seen-set
// used only for membership testing (never ranged over to build the output).
// The vault is git-synced, so a hub note whose wikilink order shuffled
// between two otherwise-identical export runs would show up as a spurious
// diff every single time — determinism here is not cosmetic.
func dedupeRefs(refs []ObsRef) []ObsRef {
	if len(refs) == 0 {
		return refs
	}
	seen := make(map[string]bool, len(refs))
	out := make([]ObsRef, 0, len(refs))
	for _, r := range refs {
		if seen[r.VaultPath] {
			continue
		}
		seen[r.VaultPath] = true
		out = append(out, r)
	}
	return out
}

// ShouldCreateTopicHub reports whether a topic hub should be created for a
// prefix shared by count observations. REQ-EXPORT-05: a hub is only created
// when at least 2 exported observations share the prefix — a single-
// observation prefix gets no hub note.
func ShouldCreateTopicHub(count int) bool {
	return count >= 2
}

// TopicPrefix returns the hub grouping prefix for topicKey: everything
// before the LAST '/' (REQ-EXPORT-05, resolving the recovered spec/design
// contradiction in favor of the design's stated grouping — this is what
// makes "sdd/{change}/proposal", "sdd/{change}/spec", "sdd/{change}/design"
// etc. all group under one "sdd/{change}" hub). A topic_key with no '/' has
// no prefix at all; ok is false and the caller must skip hub grouping for it.
func TopicPrefix(topicKey string) (prefix string, ok bool) {
	i := strings.LastIndex(topicKey, "/")
	if i < 0 {
		return "", false
	}
	return topicKey[:i], true
}
