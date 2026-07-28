package obsidian

import (
	"fmt"
	"path/filepath"
	"strings"
)

// toVaultSlash converts every backslash in s to a forward slash,
// UNCONDITIONALLY, regardless of which OS this process is running on.
//
// A vault-relative path this package renders as a wikilink target
// (ObsRef.VaultPath) or persists in SyncState.Files/Hubs MUST use forward
// slashes on every platform: Obsidian resolves wikilinks that way
// everywhere, and the vault is git-synced across machines, so a backslash
// reaching either surface can only ever be a foreign-OS artifact (or,
// before bugfix/obsidian-wikilink-path-separator, this package's own former
// bug) -- never a genuine separator this process needs to preserve.
//
// filepath.ToSlash is deliberately NOT used here: it is a no-op on any OS
// whose own path separator is already '/', so it would leave a
// Windows-authored backslash path completely UNTOUCHED when read back on
// macOS or Linux -- exactly the cross-machine, git-synced-vault scenario
// this function exists to fix. A backslash is never a legitimate part of a
// vault-relative path this package produces or reads on ANY platform (see
// safeFilename, which already maps a literal backslash in any uncontrolled
// input to '-' before it ever reaches a path segment), so replacing it
// unconditionally is always safe.
//
// Every vault-relative path this package emits as a rendered wikilink
// target or a persisted SyncState value MUST pass through this function
// exactly once: at construction (vaultJoin) or at read (ReadState). Callers
// must never hand a bare filepath.Join result to either surface directly.
func toVaultSlash(s string) string {
	return strings.ReplaceAll(s, `\`, "/")
}

// vaultJoin joins path elements exactly like filepath.Join (producing a
// single, cleaned path) and then normalizes the result to forward slashes
// via toVaultSlash. Use this instead of a bare filepath.Join for ANY path
// that will become a rendered wikilink target (ObsRef.VaultPath) or a
// persisted SyncState.Files/Hubs value.
//
// Actual filesystem operations -- os.MkdirAll, os.WriteFile,
// resolveVaultPath's own internal filepath.Join against the vault root --
// are UNAFFECTED by this and keep using bare filepath.Join / OS-native
// separators exactly as before: only the VAULT-RELATIVE STRING that becomes
// a link or travels in the JSON state file is normalized here, never the
// path actually handed to the filesystem.
func vaultJoin(elem ...string) string {
	return toVaultSlash(filepath.Join(elem...))
}

// containedPath resolves relPath against root and returns the cleaned
// ABSOLUTE path, refusing — with an error, never with a sanitized guess —
// any input whose resolved location is not STRICTLY inside root.
//
// This is the single guard behind REQ-EXPORT-02 ("MUST NOT write outside
// {vault}/engram/") and it is deliberately used on both sides of the
// exporter:
//
//   - the WRITE path, where project/type/session-id/topic-prefix are
//     uncontrolled user input reaching the filesystem through a filename;
//   - the DELETE path, where relPath comes verbatim out of
//     .engram-sync-state.json. That file lives INSIDE the vault, so anything
//     able to write to the vault (Obsidian Sync, a shared/cloud vault,
//     another plugin, an imported note bundle) can steer an os.Remove.
//
// Rules, in order:
//
//  1. relPath must be non-empty and relative. Rooted paths ("/x", "\x"),
//     UNC paths ("//server/share") and drive-qualified paths ("C:\x",
//     "C:/x", "c:x") are refused on EVERY platform, not just the one whose
//     syntax they use — a state file written on Windows can be read on
//     Linux through a synced vault and vice versa.
//  2. Both '/' and '\' are treated as separators on every platform, for the
//     same cross-platform reason: "a\..\..\x" must not degrade into a
//     benign single-segment filename on Linux.
//  3. No segment may be "..". filepath.Clean would silently apply it.
//  4. The resolved path is compared to the resolved root with filepath.Rel,
//     NOT with strings.HasPrefix on the raw strings — "C:\vaultevil" has
//     "C:\vault" as a string prefix but is not inside it. Rel also compares
//     volumes and path elements case-insensitively on Windows, which a byte
//     comparison would not.
//
// Rule 3 makes rule 4 redundant and vice versa; both are kept on purpose,
// because this is the function standing between a hostile string and
// os.Remove.
func containedPath(root, relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("obsidian: refusing an empty vault path")
	}

	norm := toVaultSlash(relPath)
	if strings.HasPrefix(norm, "/") {
		return "", fmt.Errorf("obsidian: refusing rooted path %q: vault paths must be relative", relPath)
	}
	if filepath.IsAbs(relPath) || filepath.VolumeName(relPath) != "" {
		return "", fmt.Errorf("obsidian: refusing absolute path %q: vault paths must be relative", relPath)
	}
	for _, seg := range strings.Split(norm, "/") {
		if seg == ".." {
			return "", fmt.Errorf("obsidian: refusing path %q: %q segment escapes the export root", relPath, "..")
		}
		if hasDriveLetter(seg) {
			return "", fmt.Errorf("obsidian: refusing path %q: embedded drive letter in segment %q", relPath, seg)
		}
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("obsidian: resolve export root %q: %w", root, err)
	}
	absRoot = filepath.Clean(absRoot)

	abs := filepath.Clean(filepath.Join(absRoot, filepath.FromSlash(norm)))

	rel, err := filepath.Rel(absRoot, abs)
	if err != nil {
		return "", fmt.Errorf("obsidian: refusing path %q: not resolvable under %q: %w", relPath, absRoot, err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("obsidian: refusing path %q: resolves to %q, outside %q", relPath, abs, absRoot)
	}

	return abs, nil
}

// safeFilename turns an uncontrolled string (project, type, session id,
// topic prefix — the exporter controls none of them) into a single, safe
// path SEGMENT.
//
// Three classes of hazard are neutralized:
//
//  1. Separators and the reserved Windows filename characters become '-',
//     so the value can never split into more than one segment.
//  2. Control characters become '-'; they are illegal in Windows filenames
//     and invisible in a listing on POSIX.
//  3. Leading and trailing '.' / ' ' become '_'. This is the C2 fix and the
//     subtle one: filepath.Join CLEANS "." and ".." away, so a project
//     named ".." escaped {vault}/engram/ entirely, and Windows silently
//     strips trailing dots and spaces from a path component — which makes
//     "..." resolve to the PARENT directory and "foo." collide with "foo".
//     A leading '.' additionally hides the entry from Obsidian, which
//     ignores dot-prefixed files and folders.
//
// Only LEADING and TRAILING dots are touched. A name that merely contains
// dots is a perfectly ordinary filename and must survive verbatim: "v1.0.0"
// stays "v1.0.0".
func safeFilename(s string) string {
	mapped := strings.Map(func(r rune) rune {
		switch r {
		case '\\', '/', ':', '*', '?', '"', '<', '>', '|':
			return '-'
		}
		if r < 0x20 || r == 0x7f {
			return '-'
		}
		return r
	}, s)

	runes := []rune(mapped)
	for i := 0; i < len(runes) && isPathSignificantPad(runes[i]); i++ {
		runes[i] = '_'
	}
	for i := len(runes) - 1; i >= 0 && isPathSignificantPad(runes[i]); i-- {
		runes[i] = '_'
	}
	if len(runes) == 0 {
		// An empty segment would collapse the path ("engram//decision/x.md"
		// Cleans to "engram/decision/x.md"), shifting every later segment
		// and breaking the project scoping of the deletion diff.
		return "_"
	}
	return string(runes)
}

// isPathSignificantPad reports whether r is a character the filesystem gives
// positional meaning to at the edge of a path segment.
func isPathSignificantPad(r rune) bool {
	return r == '.' || r == ' '
}

// hasDriveLetter reports whether s starts with a Windows drive qualifier
// ("C:", "c:foo"). Implemented by hand rather than via filepath.VolumeName
// because VolumeName returns "" on non-Windows, and a hostile state entry
// must be rejected identically on every platform.
func hasDriveLetter(s string) bool {
	if len(s) < 2 || s[1] != ':' {
		return false
	}
	c := s[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
