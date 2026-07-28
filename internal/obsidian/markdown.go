package obsidian

import (
	"fmt"
	"strings"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// ObservationToMarkdown renders rec as a full Obsidian note: YAML
// frontmatter, an H1 title, the content body, and a "## Wikilinks" section
// (REQ-EXPORT-03).
//
// Frontmatter fields: type, project, scope, topic_key, session_id,
// created_at, status, tags. TopicKey and Status are pointers on
// domain.Record and may be nil; a nil pointer is OMITTED from the
// frontmatter rather than rendered as the literal "<nil>".
//
// tags is SYNTHESIZED from the lowercased rec.Type — domain.Record has no
// Tags column (see internal/domain/memory.go), but four of the six locked
// graph.json colour groups match `tag:#<type>` queries, so a note's single
// tag is what makes the locked visual config resolve to anything but a
// uniform grey graph (design constraint 10 / REQ-GRAPH-05).
//
// The Wikilinks section always links back to the note's session hub
// (REQ-EXPORT-04's hub is unconditional). A topic hub link is NOT rendered
// here: whether a topic hub exists at all depends on whether at least one
// OTHER observation shares its topic_key prefix (REQ-EXPORT-05), which is
// only known once the full export batch has been scanned. The exporter
// (Phase 2) is responsible for topic-hub-to-observation edges; those edges
// are rendered from the hub side (TopicHubMarkdown), which is sufficient to
// connect the graph (design constraint 2) without a second rendering pass
// over every observation note.
func ObservationToMarkdown(rec *domain.Record) string {
	var b strings.Builder

	b.WriteString("---\n")
	fmt.Fprintf(&b, "type: %s\n", yamlQuote(rec.Type))
	fmt.Fprintf(&b, "project: %s\n", yamlQuote(rec.Project))
	fmt.Fprintf(&b, "scope: %s\n", yamlQuote(rec.Scope))
	if rec.TopicKey != nil && *rec.TopicKey != "" {
		fmt.Fprintf(&b, "topic_key: %s\n", yamlQuote(*rec.TopicKey))
	}
	fmt.Fprintf(&b, "session_id: %s\n", yamlQuote(rec.SessionID))
	fmt.Fprintf(&b, "created_at: %s\n", rec.CreatedAt.UTC().Format(time.RFC3339))
	if rec.Status != nil && *rec.Status != "" {
		fmt.Fprintf(&b, "status: %s\n", yamlQuote(*rec.Status))
	}
	fmt.Fprintf(&b, "tags: [%s]\n", yamlQuote(strings.ToLower(rec.Type)))
	b.WriteString("---\n\n")

	fmt.Fprintf(&b, "# %s\n\n", rec.Title)
	b.WriteString(rec.Content)
	b.WriteString("\n\n")

	b.WriteString("## Wikilinks\n\n")
	fmt.Fprintf(&b, "- [[_sessions/%s]]\n", rec.SessionID)

	return b.String()
}

// yamlQuote renders s as a YAML double-quoted scalar, escaping backslashes,
// embedded double quotes and the line-structure characters, so arbitrary
// observation/session/project/type content can never break the frontmatter
// block it is embedded in.
//
// The line breaks matter as much as the quotes: a raw newline inside a
// double-quoted scalar continues the value on the next line, and a
// continuation that starts at column 0 — which is exactly what an
// unescaped value produces here — is invalid YAML. Obsidian's response to
// invalid frontmatter is to drop the WHOLE block, taking the note's tag
// (and therefore its graph colour) and every other property with it. That
// is why every field goes through this function, `tags` included.
//
// Escapes must be applied backslash-first, or the escapes would escape each
// other.
func yamlQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return `"` + s + `"`
}
