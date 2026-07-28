// Package obsidian renders engram observations into an Obsidian-compatible
// markdown vault. It is purely additive: no changes to internal/localstore,
// no new Go module dependencies (stdlib only).
package obsidian

import "strings"

// maxSlugChars is the number of source characters considered before
// hyphenation, per REQ-EXPORT-07: "lowercase hyphenated first 40 chars of
// content". The cut happens on the RAW input, before collapsing separators,
// so "first 40 chars of content" is honored literally rather than producing
// 40 characters of already-hyphenated output.
const maxSlugChars = 40

// Slugify converts s into a lowercase, hyphen-separated slug suitable for use
// as a filename stem. Any run of non-alphanumeric (ASCII) runes collapses to
// a single hyphen; leading/trailing hyphens are trimmed. Non-ASCII runes
// (accents, CJK, emoji, ...) are treated as separators rather than dropped
// silently, keeping the function simple and dependency-free (no Unicode
// normalization package is introduced).
//
// The caller is responsible for appending the numeric observation id
// (REQ-EXPORT-07 collision-safe naming) — Slugify only produces the human
// -readable portion of the filename, and may return an empty string for
// input with no ASCII alphanumeric characters at all.
func Slugify(s string) string {
	runes := []rune(s)
	if len(runes) > maxSlugChars {
		runes = runes[:maxSlugChars]
	}

	var b strings.Builder
	prevHyphen := false
	for _, r := range runes {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r - 'A' + 'a')
			prevHyphen = false
		default:
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}

	return strings.TrimRight(b.String(), "-")
}
