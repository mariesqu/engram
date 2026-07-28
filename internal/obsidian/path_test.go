package obsidian

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestContainedPath is the security guard for REQ-EXPORT-02 ("MUST NOT write
// outside {vault}/engram/"). containedPath is used on BOTH the write path
// (exporter-generated note and hub paths) and the delete path (paths read
// verbatim out of .engram-sync-state.json, which lives inside the vault and
// is therefore attacker-writable by anything that can write into the vault:
// Obsidian Sync, a shared/cloud vault, another plugin, an imported bundle).
//
// The vectors below are the ones an independent harness confirmed escape the
// pre-fix implementation.
func TestContainedPath(t *testing.T) {
	root := t.TempDir()

	t.Run("accepted paths resolve under the root", func(t *testing.T) {
		cases := []struct {
			name    string
			relPath string
			want    string
		}{
			{
				name:    "ordinary observation note",
				relPath: "proj1/decision/some-slug-1.md",
				want:    filepath.Join(root, "proj1", "decision", "some-slug-1.md"),
			},
			{
				name:    "backslash separators are separators too",
				relPath: `proj1\decision\some-slug-1.md`,
				want:    filepath.Join(root, "proj1", "decision", "some-slug-1.md"),
			},
			{
				name:    "dots inside a name are ordinary characters",
				relPath: "v1.0.0/architecture/note-2.md",
				want:    filepath.Join(root, "v1.0.0", "architecture", "note-2.md"),
			},
			{
				name:    "hub note",
				relPath: "_sessions/manual-save-engram.md",
				want:    filepath.Join(root, "_sessions", "manual-save-engram.md"),
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := containedPath(root, tc.relPath)
				if err != nil {
					t.Fatalf("containedPath(root, %q) error = %v, want nil", tc.relPath, err)
				}
				if got != tc.want {
					t.Errorf("containedPath(root, %q) = %q, want %q", tc.relPath, got, tc.want)
				}
			})
		}
	})

	t.Run("escaping paths are rejected with an error, never sanitized", func(t *testing.T) {
		cases := []struct {
			name    string
			relPath string
		}{
			{"empty", ""},
			{"bare parent reference", ".."},
			{"self reference is not strictly inside", "."},
			{"leading traversal", "../victim.md"},
			{"buried traversal keeps a real project name in segment 0", "proj1/../../../../victim.md"},
			{"traversal after the engram namespace", "engram/proj1/../../../../victim.md"},
			{"trailing traversal", "proj1/decision/.."},
			{"posix absolute", "/etc/passwd"},
			{"windows absolute with backslashes", `C:\victim.md`},
			{"windows absolute with forward slashes", "C:/victim.md"},
			{"drive-relative path", "c:victim.md"},
			{"drive letter buried in a later segment", "proj1/C:/victim.md"},
			{"unc share", `\\server\share\victim.md`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := containedPath(root, tc.relPath)
				if err == nil {
					t.Fatalf("containedPath(root, %q) = %q, want an error", tc.relPath, got)
				}
				if got != "" {
					t.Errorf("containedPath(root, %q) returned path %q alongside an error; it must return no path at all", tc.relPath, got)
				}
			})
		}
	})

	t.Run("an absolute path is rejected even when it points inside the root", func(t *testing.T) {
		inside := filepath.Join(root, "proj1", "decision", "a-1.md")
		got, err := containedPath(root, inside)
		if err == nil {
			t.Fatalf("containedPath(root, %q) = %q, want an error (relPath must be relative)", inside, got)
		}
	})

	t.Run("a sibling directory sharing the root's textual prefix is rejected", func(t *testing.T) {
		// Guards against a naive strings.HasPrefix(abs, root) check:
		// "{root}evil" has "{root}" as a string prefix but is NOT under it.
		got, err := containedPath(root, "../"+filepath.Base(root)+"evil/note.md")
		if err == nil {
			t.Fatalf("containedPath(root, sibling) = %q, want an error", got)
		}
	})
}

// TestSafeFilename pins the sanitizer that turns an uncontrolled project,
// type, session id or topic prefix into a single, safe path SEGMENT.
//
// Two properties matter and pull in opposite directions: path-significant
// names ("." / ".." / "...") must be neutralized because filepath.Join
// Cleans them away and they would walk out of {vault}/engram/, while a
// benign name that merely CONTAINS dots ("v1.0.0") must survive verbatim.
func TestSafeFilename(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ordinary name is untouched", "engram", "engram"},
		{"hyphenated session id is untouched", "manual-save-engram", "manual-save-engram"},
		{"dots inside a name survive", "v1.0.0", "v1.0.0"},
		{"current directory", ".", "_"},
		{"parent directory", "..", "__"},
		{"triple dot resolves to the parent on windows", "...", "___"},
		{"empty name would collapse a path segment", "", "_"},
		{"forward slash is not a separator inside one segment", "a/b", "a-b"},
		{"backslash is not a separator inside one segment", `a\b`, "a-b"},
		{"topic prefix", "sdd/obsidian-export-rebuild", "sdd-obsidian-export-rebuild"},
		{"traversal pair", "../..", "__-__"},
		{"reserved windows characters", `a:b*c?d"e<f>g|h`, "a-b-c-d-e-f-g-h"},
		{"trailing dot is stripped by windows", "foo.", "foo_"},
		{"trailing space is stripped by windows", "foo ", "foo_"},
		{"leading dot hides the entry from obsidian", ".hidden", "_hidden"},
		{"control characters", "line\nbreak", "line-break"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := safeFilename(tc.in)
			if got != tc.want {
				t.Errorf("safeFilename(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.ContainsAny(got, `/\`) {
				t.Errorf("safeFilename(%q) = %q, which still contains a path separator", tc.in, got)
			}
		})
	}
}
