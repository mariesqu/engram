package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// hostpath.go normalizes the directory paths callers hand engram. It exists
// because on Windows "is this path absolute?" is not one question, and both of
// the shapes filepath.IsAbs gets wrong arrive from real hosts:
//
//   - A Git Bash / MSYS / Cygwin / WSL path ("/c/GitLab/engram",
//     "/mnt/c/GitLab/engram"). Every agent host launched from a Git Bash shell
//     eventually sends one, and so does every model that has ever read one.
//     filepath.IsAbs says false, so it was labelled RELATIVE and refused with a
//     sentence about the daemon's working directory that explains nothing; and
//     filepath.Abs turns it into "C:\c\GitLab\engram" — a directory that does
//     not exist, whose basename is a project nobody has. Translating it is the
//     difference between answering the caller's question and inventing a
//     project out of their shell's path syntax.
//   - The extended-length prefix ("\\?\C:\GitLab\engram"). filepath.IsAbs
//     already says true, so nothing refuses it — it simply travels on, into
//     project detection, into the directory column of session rows, and back
//     out to the user, where the same checkout now has two spellings that
//     compare unequal. Stripping it costs nothing and keeps one directory one
//     string.
//
// Everything here is a no-op off Windows: "/c/GitLab" IS an absolute path on
// Linux and macOS, and rewriting it there would break the only platform where
// it was already right.

// hostPath is the result of normalizing a caller-supplied directory.
//
// Path is the value the rest of the stack should resolve from; the caller keeps
// the original for reporting (mem_current_project's cwd_input), because a
// caller who is shown only the rewritten path cannot tell what engram did with
// what they sent.
type hostPath struct {
	// Path is the normalized directory. It equals the input unless a prefix was
	// stripped or a translation happened.
	Path string
	// Absolute reports whether Path can be resolved without a working directory.
	Absolute bool
	// Translated is true only for a Git Bash/MSYS path rewritten to its native
	// Windows form — the one case where Path names a location the caller never
	// typed, and therefore the one case worth saying out loud.
	Translated bool
}

// normalizeHostPath normalizes dir for this machine. See hostpath.go's header
// for what "normalize" covers and why.
func normalizeHostPath(dir string) hostPath {
	return normalizeHostPathWith(dir, windowsDriveExists)
}

// normalizeHostPathWith is normalizeHostPath with the drive probe injected, so
// the "that drive is not mounted" branch is testable without depending on which
// letters happen to exist on the machine running the tests.
func normalizeHostPathWith(dir string, driveExists func(letter byte) bool) hostPath {
	if dir == "" {
		return hostPath{}
	}
	if runtime.GOOS == "windows" {
		if stripped, ok := stripExtendedLengthPrefix(dir); ok {
			return hostPath{Path: filepath.Clean(stripped), Absolute: true}
		}
	}
	if filepath.IsAbs(dir) {
		return hostPath{Path: dir, Absolute: true}
	}
	if runtime.GOOS == "windows" && looksLikeGitBashPath(dir) {
		if native, ok := gitBashPathToWindows(dir, driveExists); ok {
			return hostPath{Path: filepath.Clean(native), Absolute: true, Translated: true}
		}
	}
	return hostPath{Path: dir}
}

// looksLikeGitBashPath reports whether dir has the shape of a POSIX absolute
// path. On Windows that is never a relative path and never a native one: it is
// a path from a shell, and saying so is what turns an unactionable "not
// absolute" refusal into one the user can fix.
//
// A double leading slash is excluded: "//server/share" is a UNC path, which
// filepath.IsAbs already accepts on Windows.
func looksLikeGitBashPath(dir string) bool {
	return strings.HasPrefix(dir, "/") && !strings.HasPrefix(dir, "//")
}

// gitBashDirectoryHint is the one sentence every surface uses for a POSIX-shaped
// path that could not be translated — the tool errors write tools return and the
// hints mem_current_project reports. One wording, so an agent (or a user) that
// has read it once recognises it wherever it turns up.
//
// It names the shape rather than repeating the generic relative-path sentence,
// because the two have different fixes: a relative path needs spelling out, and
// this one needs a different syntax entirely.
func gitBashDirectoryHint(dir string) string {
	return fmt.Sprintf("%q looks like a Git Bash/MSYS path; on Windows pass a native path such as C:\\...", dir)
}

// gitBashPathToWindows translates a POSIX-shaped absolute path, as Git Bash,
// MSYS2, Cygwin or WSL spell it, into its native Windows form:
//
//	/c/GitLab/engram       → C:\GitLab\engram
//	/mnt/c/GitLab/engram   → C:\GitLab\engram   (WSL)
//	/cygdrive/c/GitLab     → C:\GitLab          (Cygwin)
//
// ok is false when dir is not that shape, or when the drive it names is not
// mounted on this machine. A translation nobody can check is a guess, and a
// guessed directory is exactly how a junk project gets minted — the disease the
// whole directory-forwarding chain exists to cure. Failing here leaves the
// caller with the Git Bash hint, which tells them what to type instead.
//
// The drive segment must be a SINGLE letter: "/mnt/data" is a Linux mount
// point, not a drive, and turning it into "D:\ata" would be worse than refusing.
func gitBashPathToWindows(dir string, driveExists func(letter byte) bool) (string, bool) {
	rest, ok := strings.CutPrefix(dir, "/")
	if !ok || strings.HasPrefix(rest, "/") {
		return "", false
	}
	for _, prefix := range []string{"mnt/", "cygdrive/"} {
		if trimmed, found := strings.CutPrefix(rest, prefix); found {
			rest = trimmed
			break
		}
	}
	if rest == "" || !isASCIILetter(rest[0]) {
		return "", false
	}
	tail := rest[1:]
	if tail != "" && tail[0] != '/' {
		return "", false
	}
	letter := rest[0]
	if driveExists != nil && !driveExists(letter) {
		return "", false
	}
	// Built by hand rather than with filepath.FromSlash so the function stays
	// pure and testable on any platform: FromSlash is a no-op off Windows, which
	// would make a unit test of Windows behaviour pass by doing nothing.
	return strings.ToUpper(string(letter)) + `:\` + strings.ReplaceAll(strings.TrimPrefix(tail, "/"), "/", `\`), true
}

// stripExtendedLengthPrefix removes the "\\?\" extended-length prefix from a
// drive path, so one directory has one spelling.
//
// The UNC form ("\\?\UNC\server\share") is deliberately left alone: stripping
// its prefix yields "UNC\server\share", which is not a path at all, and
// rewriting it properly is guesswork nobody has asked for.
func stripExtendedLengthPrefix(dir string) (string, bool) {
	rest, ok := strings.CutPrefix(dir, `\\?\`)
	if !ok {
		return "", false
	}
	if len(rest) >= 2 && isASCIILetter(rest[0]) && rest[1] == ':' {
		return rest, true
	}
	return "", false
}

// windowsDriveExists reports whether the named drive letter is mounted. It is
// the production probe behind gitBashPathToWindows; a letter that is not there
// is how a plausible-looking translation ("/d/repos" on a machine with no D:)
// is caught before it becomes a project name.
func windowsDriveExists(letter byte) bool {
	info, err := os.Stat(strings.ToUpper(string(letter)) + `:\`)
	return err == nil && info.IsDir()
}

// isASCIILetter reports whether b is a-z or A-Z. Drive letters are ASCII by
// definition, so unicode.IsLetter would accept things no volume can be named.
func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
