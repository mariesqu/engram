package obsidian

import (
	"fmt"
	"strings"
	"time"
)

// Narrative is this package's own narrow, PLAIN-TYPED view onto one cached
// narrative row (REQ-GEN-08's import boundary): internal/obsidian may not
// import internal/generation or internal/localstore
// (TestObsidianPackageImportsOnlyStdlibAndDomain, imports_test.go, fails the
// BUILD on a violation, not merely a review), so this type is declared here,
// independently, using only stdlib types — exactly the same idiom StoreReader
// already establishes for domain.Record (exporter.go:16-36).
//
// Every field here is a STORED column, never something this package
// recomputes: Stale is written by the generation Loop (source_hash mismatch
// detected there, internal/generation's own package), UnverifiedPaths is
// os.Stat-checked once at GENERATION time and persisted (internal/generation
// paths.go), and Truncated/SourceCount/SourceWriters/GeneratedAt/SourceHash
// are all descriptive columns the loop writes alongside Body. This package
// reads every one of them verbatim and computes NOTHING from live data —
// see NarrativeMarkdown's purity guarantee below and
// TestExporterComputesStalenessNowhere (exporter_narrative_test.go).
type Narrative struct {
	Body            string
	Model           string
	SourceHash      string
	GeneratedAt     time.Time
	Stale           bool
	Truncated       bool
	SourceCount     int
	SourceWriters   []string
	UnverifiedPaths []string
}

// NarrativeReader is the narrow read surface Export() needs to render
// narrative notes (REQ-NARR-07 amends REQ-EXPORT-10). Satisfied by
// *localstore.Store through a thin, behaviour-free type-translation adapter
// living in cmd/engram (a later phase, beside the already-shipped
// obsidianStoreAdapter) — mirroring StoreReader's own precedent exactly.
//
// A nil NarrativeReader on ExportConfig means the feature is OFF: presence
// of the coordinate is the switch, the same idiom GraphConfig's zero value
// and StoreReader's own construction already use elsewhere in this package.
type NarrativeReader interface {
	// NarrativesForExport returns every currently cached narrative row,
	// keyed "lower(project)\x00lower(topic_prefix)" — the SAME fold
	// internal/generation.UnitKey and internal/localstore's
	// narrativeFoldKey both produce, so all three layers agree on unit
	// identity. ONE query per cycle, mirroring StoreReader's own
	// per-cycle-not-per-note shape.
	NarrativesForExport() (map[string]Narrative, error)
}

// narrativeBannerBody is the REQ-PROV-04 attribution banner, composed
// ENTIRELY from n's own stored columns — never from n.Body, which is the
// model's own text. NarrativeMarkdown prepends this UNCONDITIONALLY, after
// the model's response is already in hand, at a fixed position the model has
// no path to write into: the model cannot alter, omit, or forge this banner
// because it never produces it (see TestModelCannotForgeOrSuppressTheBanner).
//
// A pure function of n: no wall clock, no filesystem access, so it cannot
// break NarrativeMarkdown's own purity guarantee (TestNarrativeMarkdownIsAPureFunctionOfTheRow).
func narrativeBannerBody(n Narrative) string {
	writers := "no writer recorded"
	if len(n.SourceWriters) > 0 {
		writers = strings.Join(n.SourceWriters, ", ")
	}

	var b strings.Builder
	b.WriteString("> [!warning] Generated summary — claims are as RECORDED, not independently verified\n")
	fmt.Fprintf(&b, "> Synthesised by `%s` on %s over %d observation(s) from writer(s) %s.\n",
		n.Model, n.GeneratedAt.UTC().Format(time.RFC3339), n.SourceCount, writers)
	b.WriteString("> Completion and behavioural claims (\"tests pass\", \"27/27 done\") have no oracle here and are NOT checked.\n")
	return b.String()
}

// NarrativeMarkdown renders one narrative note: YAML frontmatter
// (REQ-PROV-01/-02/-03/-06 fields, all stored columns), the Go-authored
// attribution banner (REQ-PROV-04), then the model's own body, then a
// "## Wikilinks" section linking back to the topic hub it narrates
// (REQ-EXPORT-02's amendment: "reachable from the structural notes/hubs it
// narrates" — design #4754 §6 Option A: the narrative links TO the hub, the
// hub is never modified, so hub bytes stay fully independent of narrative
// presence, REQ-NARR-09/TestHubBytesAreUnchangedByNarrativePresence).
//
// key is the SAME folded identity NarrativeReader keys its map by
// ("lower(project)\x00lower(topic_prefix)") — split here purely for display
// and the hub wikilink target, never re-validated or re-derived from
// anything outside (key, n).
//
// PURE FUNCTION OF (key, n) — THE IDEMPOTENCY KEYSTONE
// (TestNarrativeMarkdownIsAPureFunctionOfTheRow, task 8.7). No wall clock
// (GeneratedAt comes from n, never time.Now()), no os.Stat, no filesystem
// access, no store or network call enters these rendered bytes. Every input
// this function reads is already a value the caller holds. If a future
// change adds ANY of those, the vault flaps in git history on every export
// cycle forever — remove the dependency rather than relax the test.
func NarrativeMarkdown(key string, n Narrative) string {
	project, topicPrefix, _ := strings.Cut(key, "\x00")

	var b strings.Builder

	b.WriteString("---\n")
	fmt.Fprintf(&b, "narrative_model: %s\n", yamlQuote(n.Model))
	fmt.Fprintf(&b, "narrative_generated_at: %s\n", n.GeneratedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "narrative_source_hash: %s\n", yamlQuote(n.SourceHash))
	fmt.Fprintf(&b, "narrative_source_count: %d\n", n.SourceCount)
	fmt.Fprintf(&b, "narrative_source_writers: [%s]\n", yamlQuoteList(n.SourceWriters))
	fmt.Fprintf(&b, "narrative_stale: %t\n", n.Stale)
	fmt.Fprintf(&b, "narrative_truncated: %t\n", n.Truncated)
	fmt.Fprintf(&b, "unverified_paths: [%s]\n", yamlQuoteList(n.UnverifiedPaths))
	b.WriteString("tags: [\"narrative\"]\n")
	b.WriteString("---\n\n")

	// The banner is prepended AFTER n.Body is already in hand (it is a
	// field read, not a computation over live data) and BEFORE n.Body is
	// ever written out — a fixed position the model's own text (which
	// follows immediately below) has no way to reach or precede.
	b.WriteString(narrativeBannerBody(n))
	b.WriteString("\n")

	fmt.Fprintf(&b, "# Narrative: %s (%s)\n\n", topicPrefix, project)
	b.WriteString(n.Body)
	b.WriteString("\n\n")

	b.WriteString("## Wikilinks\n\n")
	fmt.Fprintf(&b, "- [[_topics/%s]]\n", topicPrefix)

	return b.String()
}

// yamlQuoteList renders items as a YAML flow-style sequence body (the part
// between "[" and "]"), each element individually yamlQuote-escaped. Used
// for narrative_source_writers and unverified_paths — both are
// model-adjacent data (writer ids the model never sees, but path text
// EXTRACTED from the model's own prose) and must survive the same hostile
// characters yamlQuote already defends every other frontmatter field
// against (TestNarrativeFrontmatterSurvivesHostileModelOutput). Flow style
// (not YAML block-sequence "- item" lines) keeps every frontmatter line
// matching the "key: value" shape the rest of this package's frontmatter
// already guarantees (see markdown_test.go's frontmatterBlock helper).
func yamlQuoteList(items []string) string {
	quoted := make([]string, len(items))
	for i, it := range items {
		quoted[i] = yamlQuote(it)
	}
	return strings.Join(quoted, ", ")
}
