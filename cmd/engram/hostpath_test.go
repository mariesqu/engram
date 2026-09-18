package main

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests cover hostpath.go — the normalization every caller-supplied
// directory goes through. Two of the three shapes only exist on Windows, and
// the ones that depend on filepath's platform rules are skipped elsewhere
// rather than faked: a test that passes by asking a Linux filepath whether a
// Windows path is absolute is a test that proves nothing.

// alwaysMounted / neverMounted stand in for the drive probe so the translation
// can be tested without depending on which letters this machine happens to have.
func alwaysMounted(byte) bool { return true }
func neverMounted(byte) bool  { return false }

// TestGitBashPathToWindows_TranslatesEveryShellShape is the pure core: what a
// Git Bash, WSL or Cygwin `pwd` prints, and what Windows calls the same place.
// It runs on every platform because the function does not consult filepath.
func TestGitBashPathToWindows_TranslatesEveryShellShape(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"git bash", "/c/GitLab/engram", `C:\GitLab\engram`},
		{"wsl", "/mnt/c/GitLab/engram", `C:\GitLab\engram`},
		{"cygwin", "/cygdrive/c/GitLab", `C:\GitLab`},
		{"lowercase drive is upcased", "/d/repos", `D:\repos`},
		{"bare drive", "/c", `C:\`},
		{"drive root", "/c/", `C:\`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := gitBashPathToWindows(tc.in, alwaysMounted)
			if !ok {
				t.Fatalf("gitBashPathToWindows(%q) refused a path it should translate", tc.in)
			}
			if got != tc.want {
				t.Errorf("gitBashPathToWindows(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestGitBashPathToWindows_RefusesWhatIsNotADrive covers the other half: the
// shapes that LOOK like a drive path and are not. Guessing here is how
// "/mnt/data" becomes "D:\ata" — a directory that does not exist, whose
// basename would become a project name nobody has.
func TestGitBashPathToWindows_RefusesWhatIsNotADrive(t *testing.T) {
	for _, in := range []string{
		"",
		"repo",                // relative, not POSIX at all
		`C:\GitLab`,           // already native
		"//server/share",      // UNC, which filepath.IsAbs already accepts
		"/mnt/data/archive",   // a Linux mount point, not a drive
		"/repos/engram",       // multi-letter first segment
		"/cygdrive/data/here", // same, behind the Cygwin prefix
		"/1/repos",            // not a letter
	} {
		if got, ok := gitBashPathToWindows(in, alwaysMounted); ok {
			t.Errorf("gitBashPathToWindows(%q) = %q, true — it is not a drive path", in, got)
		}
	}
}

// TestGitBashPathToWindows_RefusesAnUnmountedDrive pins the fail-closed half.
// "/d/repos" on a machine with no D: is a path nobody can check, and a
// translation nobody can check is a guess.
func TestGitBashPathToWindows_RefusesAnUnmountedDrive(t *testing.T) {
	if got, ok := gitBashPathToWindows("/d/repos", neverMounted); ok {
		t.Errorf("gitBashPathToWindows = %q, true — the drive is not mounted, so the path names nothing", got)
	}
}

// TestStripExtendedLengthPrefix covers the \\?\ form: stripped for a drive
// path, left alone for the UNC one (stripping that yields "UNC\server\share",
// which is not a path at all).
func TestStripExtendedLengthPrefix(t *testing.T) {
	if got, ok := stripExtendedLengthPrefix(`\\?\C:\GitLab\engram`); !ok || got != `C:\GitLab\engram` {
		t.Errorf("stripExtendedLengthPrefix = (%q, %v), want (%q, true)", got, ok, `C:\GitLab\engram`)
	}
	for _, in := range []string{`\\?\UNC\server\share`, `C:\GitLab`, `/c/GitLab`, ""} {
		if got, ok := stripExtendedLengthPrefix(in); ok {
			t.Errorf("stripExtendedLengthPrefix(%q) = %q, true — nothing to strip here", in, got)
		}
	}
}

// TestNormalizeHostPath_Windows is the wrapper on the platform it exists for:
// a Git Bash path becomes absolute and reports itself as translated, an
// extended-length path becomes a plain one, and a genuinely relative path stays
// relative so the caller can refuse it.
func TestNormalizeHostPath_Windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the shapes under test are Windows-only; elsewhere /c/x IS an absolute path")
	}
	cases := []struct {
		name       string
		in         string
		want       string
		absolute   bool
		translated bool
	}{
		{"git bash", "/c/GitLab/engram", `C:\GitLab\engram`, true, true},
		{"wsl", "/mnt/c/GitLab", `C:\GitLab`, true, true},
		{"extended length", `\\?\C:\GitLab\engram`, `C:\GitLab\engram`, true, false},
		{"native", `C:\GitLab`, `C:\GitLab`, true, false},
		{"dot", ".", ".", false, false},
		{"relative", `repo\sub`, `repo\sub`, false, false},
		{"unmountable posix path", "/repos/engram", "/repos/engram", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeHostPathWith(tc.in, alwaysMounted)
			if got.Path != tc.want || got.Absolute != tc.absolute || got.Translated != tc.translated {
				t.Errorf("normalizeHostPath(%q) = %+v, want {Path:%q Absolute:%v Translated:%v}",
					tc.in, got, tc.want, tc.absolute, tc.translated)
			}
			if got.Absolute && !filepath.IsAbs(got.Path) {
				t.Errorf("normalizeHostPath(%q) reported Absolute for %q, which filepath disagrees with", tc.in, got.Path)
			}
		})
	}
}

// TestNormalizeHostPath_LeavesPOSIXAlone is the other platform's contract:
// "/c/GitLab" is a perfectly good absolute path on Linux and macOS, and
// rewriting it there would break the only platform where it was already right.
func TestNormalizeHostPath_LeavesPOSIXAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX absolute paths are the Windows case under test above")
	}
	got := normalizeHostPathWith("/c/GitLab", alwaysMounted)
	if got.Path != "/c/GitLab" || !got.Absolute || got.Translated {
		t.Errorf("normalizeHostPath = %+v, want the path untouched and absolute", got)
	}
}

// TestGitBashDirectoryHint_NamesTheShapeAndTheFix pins the wording every
// surface shares. An agent that has read it once has to recognise it in the
// other place it appears, and "pass an absolute path" — the generic advice — is
// unactionable for someone who just passed what their shell calls one.
func TestGitBashDirectoryHint_NamesTheShapeAndTheFix(t *testing.T) {
	hint := gitBashDirectoryHint("/c/GitLab/engram")
	for _, want := range []string{`"/c/GitLab/engram"`, "Git Bash/MSYS path", `C:\`} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint must mention %q; got: %s", want, hint)
		}
	}
}
