// Package topickey derives a stable, DETERMINISTIC topic-key suggestion from a
// memory's type/title/content. Determinism is the whole point: independent
// sessions (or different agents) that describe the same topic similarly converge
// on the SAME key, so mem_save upserts land on one evolving chain instead of
// fragmenting into near-duplicate topics.
//
// The shape is always "family/segment", e.g. "architecture/auth-model".
package topickey

import (
	"regexp"
	"strings"
)

// nonAlphaNum collapses any run of non [a-z0-9] characters to a single space,
// which the normalizer then joins with hyphens.
var nonAlphaNum = regexp.MustCompile(`[^a-z0-9]+`)

// Suggest returns a "family/segment" topic key inferred from type/title/content.
// It never returns an empty string — the worst case is "topic/general".
func Suggest(typ, title, content string) string {
	family := inferFamily(typ, title, content)

	segment := normalizeSegment(title)
	if segment == "" {
		words := strings.Fields(strings.ToLower(content))
		if len(words) > 8 {
			words = words[:8]
		}
		segment = normalizeSegment(strings.Join(words, " "))
	}
	if segment == "" {
		segment = "general"
	}

	// Avoid "architecture/architecture-foo" style redundancy.
	if strings.HasPrefix(segment, family+"-") {
		segment = strings.TrimPrefix(segment, family+"-")
	}
	if segment == "" || segment == family {
		segment = "general"
	}

	return family + "/" + segment
}

// inferFamily maps an explicit type, then falling back to title/content keywords,
// to a stable topic family. Defaults to "topic" when nothing matches.
func inferFamily(typ, title, content string) string {
	t := strings.TrimSpace(strings.ToLower(typ))
	switch t {
	case "architecture", "design", "adr", "refactor":
		return "architecture"
	case "bug", "bugfix", "fix", "incident", "hotfix":
		return "bug"
	case "decision":
		return "decision"
	case "pattern", "convention", "guideline":
		return "pattern"
	case "config", "setup", "infra", "infrastructure", "ci":
		return "config"
	case "discovery", "investigation", "root_cause", "root-cause":
		return "discovery"
	case "learning", "learn":
		return "learning"
	case "session_summary":
		return "session"
	}

	text := strings.ToLower(title + " " + content)
	switch {
	case hasAny(text, "bug", "fix", "panic", "error", "crash", "regression", "incident", "hotfix"):
		return "bug"
	case hasAny(text, "architecture", "design", "adr", "boundary", "hexagonal", "refactor"):
		return "architecture"
	case hasAny(text, "decision", "tradeoff", "chose", "choose", "decide"):
		return "decision"
	case hasAny(text, "pattern", "convention", "naming", "guideline"):
		return "pattern"
	case hasAny(text, "config", "setup", "environment", "env", "docker", "pipeline"):
		return "config"
	case hasAny(text, "discovery", "investigate", "investigation", "found", "root cause"):
		return "discovery"
	case hasAny(text, "learned", "learning"):
		return "learning"
	}

	// A non-empty, non-default type becomes its own family.
	if t != "" && t != "manual" {
		return normalizeSegment(t)
	}
	return "topic"
}

func hasAny(text string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

// normalizeSegment lowercases, replaces non-alphanumeric runs with hyphens, and
// caps the length so a key segment is stable and filesystem/URL friendly.
func normalizeSegment(s string) string {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" {
		return ""
	}
	v = nonAlphaNum.ReplaceAllString(v, " ")
	v = strings.Join(strings.Fields(v), "-")
	if len(v) > 100 {
		v = v[:100]
	}
	return v
}
