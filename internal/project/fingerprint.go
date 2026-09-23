package project

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DetectionFingerprint summarises the filesystem state DetectProjectFull reads
// for dir, WITHOUT running git: two equal fingerprints mean detection would read
// the same inputs and so give the same answer. It exists for callers that cache
// a detection result (the prompt hook, which cannot afford the two git spawns
// inside its budget) and need a cheap way to tell whether the cache is stale.
//
// It mirrors DetectProjectFull's precedence input by input:
//
//   - the directory itself (a deleted workspace must not keep its answer);
//   - every level from dir up to the enclosing git root (or the filesystem root
//     when there is none): its `.git` entry, recorded ABSENT too, so a `git
//     init` or a new worktree above dir changes the fingerprint;
//   - every `.engram/config.json` detection could read — each level up to and
//     including the git root, or only dir's own outside git — again recording
//     absent files, so a config newly created closer to dir invalidates;
//   - inside git: the repository config holding remote.origin (the `.git`
//     directory's config, or for a worktree/submodule the `.git` file, the
//     gitdir's commondir and config.worktree, and the common dir's config);
//   - outside git: dir's own modification time (a child directory added or
//     removed) and the `.git` of every child scanChildren would inspect.
//
// Every entry is a stat, apart from reading a `.git` FILE or `commondir` file
// (a line each, worktrees and submodules only) and one ReadDir outside git.
// Regular files contribute size and modification time; directories contribute
// existence only, except dir outside git, whose listing is itself an input.
// Anything other than "does not exist" is returned as an error: a caller must
// treat that as "cannot tell", never as "unchanged".
//
// Detection inputs that live outside the workspace — the global git config,
// an includeIf'd file, GIT_DIR in the daemon's environment — are not covered;
// callers caching on this fingerprint should bound the cache's age as well.
func DetectionFingerprint(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("detection fingerprint: empty directory")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("detection fingerprint: %w", err)
	}
	absDir = canonicalizePath(absDir)

	var b strings.Builder
	add := func(path string, content bool) error {
		fp, err := statFingerprint(path, content)
		if err != nil {
			return fmt.Errorf("detection fingerprint: %w", err)
		}
		fmt.Fprintf(&b, "%s\x00%s\n", path, fp)
		return nil
	}

	if err := add(absDir, false); err != nil {
		return "", err
	}

	// Walk up exactly as findGitRootFS does, recording each level's .git.
	gitRoot := ""
	var levels []string
	for current := filepath.Clean(absDir); ; {
		levels = append(levels, current)
		gitEntry := filepath.Join(current, ".git")
		if err := add(gitEntry, false); err != nil {
			return "", err
		}
		if _, err := lstat(gitEntry); err == nil {
			gitRoot = current
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}

	// The config candidates detectFromConfig can read.
	configLevels := levels
	if gitRoot == "" {
		configLevels = levels[:1]
	}
	for _, level := range configLevels {
		if err := add(filepath.Join(level, ".engram", "config.json"), true); err != nil {
			return "", err
		}
	}

	if gitRoot != "" {
		if err := addGitConfigInputs(gitRoot, add); err != nil {
			return "", err
		}
		return b.String(), nil
	}

	// Outside git: scanChildren's inputs. The directory's own mtime moves when
	// a child is created, removed or renamed; a child gaining a .git does not
	// touch it, so each child's .git is recorded as well.
	if err := add(absDir, true); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return "", fmt.Errorf("detection fingerprint: %w", err)
	}
	scanned := 0
	for _, entry := range entries {
		if scanned >= 20 {
			break
		}
		name := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(name, ".") || noiseSet[name] {
			continue
		}
		scanned++
		if err := add(filepath.Join(absDir, name, ".git"), false); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

// addGitConfigInputs records the files `git remote get-url origin` reads for
// the repository rooted at gitRoot. A `.git` directory holds the config
// directly; a `.git` FILE (worktree, submodule) points at a gitdir whose
// `commondir` names the directory that holds the shared config.
func addGitConfigInputs(gitRoot string, add func(string, bool) error) error {
	gitEntry := filepath.Join(gitRoot, ".git")
	info, err := lstat(gitEntry)
	if err != nil {
		return fmt.Errorf("detection fingerprint: %w", err)
	}
	if info.IsDir() {
		return add(filepath.Join(gitEntry, "config"), true)
	}

	if err := add(gitEntry, true); err != nil {
		return err
	}
	body, err := os.ReadFile(gitEntry)
	if err != nil {
		return fmt.Errorf("detection fingerprint: %w", err)
	}
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(body)), "gitdir:")
	if !ok {
		return nil // not a gitdir pointer; git will refuse it, and so did detection
	}
	gitDir = resolveGitPath(gitRoot, strings.TrimSpace(gitDir))

	for _, name := range []string{"config", "config.worktree", "commondir"} {
		if err := add(filepath.Join(gitDir, name), true); err != nil {
			return err
		}
	}
	common, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil // a submodule: its gitdir's own config is the one
	}
	if err != nil {
		return fmt.Errorf("detection fingerprint: %w", err)
	}
	commonDir := resolveGitPath(gitDir, strings.TrimSpace(string(common)))
	return add(filepath.Join(commonDir, "config"), true)
}

// resolveGitPath resolves a path git stored relative to base.
func resolveGitPath(base, path string) string {
	path = filepath.FromSlash(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return filepath.Clean(path)
}

// lstat is os.Lstat, swappable so tests can inject a stat failure.
var lstat = os.Lstat

// statFingerprint describes one input: "-" when it does not exist, "d" for a
// directory whose contents are not an input, and size plus modification time
// otherwise.
func statFingerprint(path string, content bool) (string, error) {
	info, err := lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "-", nil
	}
	if err != nil {
		return "", err
	}
	if info.IsDir() && !content {
		return "d", nil
	}
	return fmt.Sprintf("%s:%d:%d", info.Mode().Type(), info.Size(), info.ModTime().UnixNano()), nil
}
