package project

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExtractRepoName_TrailingSlash verifies a remote URL with a trailing slash
// (common for Azure DevOps / Gitea / user-configured remotes) still yields the
// bare repo name rather than "repo.git".
func TestExtractRepoName_TrailingSlash(t *testing.T) {
	cases := map[string]string{
		"https://github.com/user/repo.git/": "repo",
		"git@github.com:user/repo.git/":     "repo",
		"https://github.com/user/repo/":     "repo",
		"https://github.com/user/repo.git":  "repo", // baseline, no trailing slash
	}
	for url, want := range cases {
		if got := extractRepoName(url); got != want {
			t.Errorf("extractRepoName(%q) = %q, want %q", url, got, want)
		}
	}
}

// TestDetectProjectFull_FilesystemRoot verifies the dir-basename fallback never
// returns a bare path separator ("/" or "\") as the project name for a
// filesystem root — which would corrupt the store as a project key.
func TestDetectProjectFull_FilesystemRoot(t *testing.T) {
	root := filepath.VolumeName(os.TempDir()) + string(os.PathSeparator)
	res := DetectProjectFull(root)
	if res.Project == "" || res.Project == "/" || res.Project == "\\" {
		t.Errorf("DetectProjectFull(%q).Project = %q, want a real name (e.g. %q)", root, res.Project, "unknown")
	}
}

// TestFindSimilar_EmptyQuery verifies an empty query returns no matches rather
// than matching every short candidate (effectiveMax would otherwise stay at the
// full maxDistance for a zero-length query).
func TestFindSimilar_EmptyQuery(t *testing.T) {
	got := FindSimilar("", []string{"a", "ab", "abc", "project-x"}, 3)
	if len(got) != 0 {
		t.Errorf("FindSimilar(%q, ...) returned %d matches, want 0", "", len(got))
	}
	// Whitespace-only collapses to empty after trim → also no matches.
	if got := FindSimilar("   ", []string{"a", "ab"}, 3); len(got) != 0 {
		t.Errorf("FindSimilar(whitespace, ...) returned %d matches, want 0", len(got))
	}
}
