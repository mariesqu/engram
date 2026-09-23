package project

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests pin DetectionFingerprint to DetectProjectFull's inputs: every
// change that can move detection's answer must move the fingerprint, and a
// workspace nobody touched must fingerprint identically.

func writeInput(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// Move the mtime strictly forward so a same-size rewrite is still visible
	// on a filesystem with coarse timestamps.
	when := time.Now().Add(time.Hour)
	if info, err := os.Stat(path); err == nil && !info.ModTime().Before(when) {
		when = info.ModTime().Add(time.Hour)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func mustFingerprint(t *testing.T, dir string) string {
	t.Helper()
	fp, err := DetectionFingerprint(dir)
	if err != nil {
		t.Fatalf("DetectionFingerprint(%s): %v", dir, err)
	}
	return fp
}

func TestDetectionFingerprint_TracksDetectionInputs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(t *testing.T, root string)
		changed bool
	}{
		{"nothing touched", func(*testing.T, string) {}, false},
		{"unrelated file", func(t *testing.T, root string) {
			writeInput(t, filepath.Join(root, "pkg", "inner", "main.go"), "package main")
		}, false},
		{"config edited", func(t *testing.T, root string) {
			writeInput(t, filepath.Join(root, ".engram", "config.json"), `{"project_name":"other"}`)
		}, true},
		{"config created closer to the cwd", func(t *testing.T, root string) {
			writeInput(t, filepath.Join(root, "pkg", ".engram", "config.json"), `{"project_name":"closer"}`)
		}, true},
		{"git remote changed", func(t *testing.T, root string) {
			writeInput(t, filepath.Join(root, ".git", "config"), "[remote \"origin\"]\n\turl = b\n")
		}, true},
		{"nested repo initialised between cwd and root", func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, "pkg", ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cwd := filepath.Join(root, "pkg", "inner")
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			writeInput(t, filepath.Join(root, ".git", "config"), "[remote \"origin\"]\n\turl = a\n")
			writeInput(t, filepath.Join(root, ".engram", "config.json"), `{"project_name":"root"}`)

			before := mustFingerprint(t, cwd)
			tc.mutate(t, root)
			after := mustFingerprint(t, cwd)
			if got := before != after; got != tc.changed {
				t.Errorf("fingerprint changed = %v, want %v\nbefore:\n%s\nafter:\n%s", got, tc.changed, before, after)
			}
		})
	}
}

// TestDetectionFingerprint_WorktreeFollowsCommonConfig — in a linked worktree
// `.git` is a file, and the remote lives in the MAIN repository's config,
// reached through the gitdir's commondir.
func TestDetectionFingerprint_WorktreeFollowsCommonConfig(t *testing.T) {
	base := t.TempDir()
	mainGit := filepath.Join(base, "main", ".git")
	gitDir := filepath.Join(mainGit, "worktrees", "wt")
	worktree := filepath.Join(base, "wt")
	writeInput(t, filepath.Join(mainGit, "config"), "[remote \"origin\"]\n\turl = a\n")
	writeInput(t, filepath.Join(gitDir, "commondir"), "../..\n")
	writeInput(t, filepath.Join(worktree, ".git"), "gitdir: "+filepath.ToSlash(gitDir)+"\n")

	before := mustFingerprint(t, worktree)
	if !strings.Contains(before, filepath.Join(mainGit, "config")) {
		t.Fatalf("worktree fingerprint does not cover the common config:\n%s", before)
	}
	writeInput(t, filepath.Join(mainGit, "config"), "[remote \"origin\"]\n\turl = b\n")
	if after := mustFingerprint(t, worktree); after == before {
		t.Error("changing the main repository's remote did not change the worktree's fingerprint")
	}
}

// TestDetectionFingerprint_OutsideGitTracksChildRepos — outside a repository
// detection scans the children, so a child becoming a repository moves it.
func TestDetectionFingerprint_OutsideGitTracksChildRepos(t *testing.T) {
	dir := t.TempDir()
	if findGitRootFS(dir) != "" {
		t.Skip("the temp directory is inside a git repository")
	}
	if err := os.MkdirAll(filepath.Join(dir, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := mustFingerprint(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "child", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if after := mustFingerprint(t, dir); after == before {
		t.Error("a child gaining a .git did not change the fingerprint")
	}
}

// TestDetectionFingerprint_StatErrorIsAnError — anything but "does not exist"
// means the inputs cannot be read, which a cache must treat as a miss.
func TestDetectionFingerprint_StatErrorIsAnError(t *testing.T) {
	dir := t.TempDir()
	old := lstat
	lstat = func(path string) (fs.FileInfo, error) {
		if filepath.Base(path) == "config.json" {
			return nil, fs.ErrPermission
		}
		return old(path)
	}
	t.Cleanup(func() { lstat = old })

	if _, err := DetectionFingerprint(dir); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("DetectionFingerprint error = %v, want the stat failure", err)
	}
}
