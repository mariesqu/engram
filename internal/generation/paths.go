package generation

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// pathToken matches either a backtick-delimited token (capture group 1) or
// a whitespace-delimited token (capture group 2). Backticks are tried
// first so a path wrapped in backticks is captured verbatim, without its
// delimiters, even though it is otherwise indistinguishable from prose.
var pathToken = regexp.MustCompile("`([^`]+)`|(\\S+)")

// pathTrimSet is the set of trailing punctuation characters stripped from a
// bare (non-backtick) token before it is judged to be a path. Prose ends
// sentences and clauses with these; none of them is ever a legitimate
// trailing character of a real filesystem path.
const pathTrimSet = ".,;:!?)]}'\""

// ExtractPaths scans prose for filesystem-path-like tokens. It is
// deliberately conservative: a token qualifies only when it contains a path
// separator ('/' or '\\') and is not a URL (contains "://"). Backtick-quoted
// tokens are unwrapped and used verbatim -- the backticks already delimit
// the token exactly, so no trailing-punctuation trim is applied to them.
// Bare tokens have trailing sentence/clause punctuation stripped first, so
// a sentence-ending period after a path is never captured into the path
// text.
//
// Duplicates are NOT removed here -- VerifyPaths does that once each token
// has been resolved against the filesystem.
func ExtractPaths(prose string) []string {
	var out []string
	for _, m := range pathToken.FindAllStringSubmatch(prose, -1) {
		var tok string
		if m[1] != "" {
			tok = m[1] // backtick-quoted: verbatim
		} else {
			tok = strings.TrimRight(m[2], pathTrimSet)
		}
		if looksLikePath(tok) {
			out = append(out, tok)
		}
	}
	return out
}

// looksLikePath reports whether tok is plausibly a filesystem path: it must
// contain a path separator and must not be a URL.
func looksLikePath(tok string) bool {
	if tok == "" {
		return false
	}
	if strings.Contains(tok, "://") {
		return false // URL, not a path
	}
	return strings.ContainsAny(tok, "/\\")
}

// VerifyPaths checks each entry in paths against the filesystem, relative
// to sessionDir -- the narrated project's most recently known sessions
// directory. Resolving sessionDir itself (via Store.GetSession) is the
// caller's responsibility; this function has no store access, by design
// (REQ-GEN-08's import boundary is not at issue here, but keeping this
// package's path-verification logic free of a store dependency keeps it
// trivially testable with a plain temp directory).
//
// This MUST run at GENERATION time and its result MUST be PERSISTED by the
// caller. Re-os.Stat-ing at render time would make the export cycle's
// rendered bytes depend on the filesystem AT RENDER TIME, so a file deleted
// between two cycles would rewrite the note with no underlying data
// change -- a non-determinism the export layer must not acquire (see
// internal/obsidian's writeIfChanged idempotency guarantee).
//
// A path that cannot be resolved is listed in unverified -- NEVER silently
// dropped, and NEVER silently presented as confirmed (REQ-PROV-02's honesty
// property): len(resolved) + len(unverified) always equals the number of
// distinct entries in paths. An empty sessionDir (no known session
// directory for the project) marks every path unverified.
func VerifyPaths(sessionDir string, paths []string) (resolved, unverified []string) {
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true

		if sessionDir == "" {
			unverified = append(unverified, p)
			continue
		}

		full := p
		if !filepath.IsAbs(p) {
			full = filepath.Join(sessionDir, p)
		}
		if _, err := os.Stat(full); err == nil {
			resolved = append(resolved, p)
		} else {
			unverified = append(unverified, p)
		}
	}
	sort.Strings(resolved)
	sort.Strings(unverified)
	return resolved, unverified
}
