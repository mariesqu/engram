package generation

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestExtractPathsFromProse pins ExtractPaths' extraction rules: a POSIX
// path, a Windows path with backslashes, a path inside backticks, and a
// bare relative path all extract; a URL must NOT be extracted (it is not a
// filesystem path); and a sentence-ending period must NOT be captured into
// the extracted path text.
func TestExtractPathsFromProse(t *testing.T) {
	tests := []struct {
		name  string
		prose string
		want  []string
	}{
		{
			name:  "posix path",
			prose: "The file /home/user/file.go was read during generation.",
			want:  []string{"/home/user/file.go"},
		},
		{
			name:  "windows path with backslashes",
			prose: `See C:\Users\foo\bar.txt for the original source.`,
			want:  []string{`C:\Users\foo\bar.txt`},
		},
		{
			name:  "path inside backticks",
			prose: "Update `internal/generation/loop.go` before the next cycle.",
			want:  []string{"internal/generation/loop.go"},
		},
		{
			name:  "bare relative path",
			prose: "Changes are in pkg/file.go and nowhere else.",
			want:  []string{"pkg/file.go"},
		},
		{
			name:  "url must not be extracted",
			prose: "See https://example.com/path/to/thing for more detail.",
			want:  nil,
		},
		{
			name:  "sentence-ending period must not be captured",
			prose: "The fix is in internal/generation/loop.go.",
			want:  []string{"internal/generation/loop.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractPaths(tt.prose)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ExtractPaths(%q) = %#v, want %#v", tt.prose, got, tt.want)
			}
		})
	}
}

// TestVerifyPathsListsOnlyUnresolvable pins the core os.Stat check against a
// real temp directory: an existing file resolves, a missing one does not,
// and the results are sorted and de-duplicated.
func TestVerifyPathsListsOnlyUnresolvable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "exists.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}

	resolved, unverified := VerifyPaths(dir, []string{"exists.txt", "missing.txt", "exists.txt"})

	if want := []string{"exists.txt"}; !reflect.DeepEqual(resolved, want) {
		t.Errorf("resolved = %#v, want %#v (deduplicated)", resolved, want)
	}
	if want := []string{"missing.txt"}; !reflect.DeepEqual(unverified, want) {
		t.Errorf("unverified = %#v, want %#v", unverified, want)
	}
}

// TestVerifyPathsNeverDropsAPathSilently pins REQ-PROV-02's honesty
// property directly: a path that cannot be resolved must be LISTED, never
// silently dropped. len(resolved) + len(unverified) must always equal the
// number of DISTINCT input paths.
func TestVerifyPathsNeverDropsAPathSilently(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}

	input := []string{"a.go", "b.go", "c/d.go", "a.go", "e.go"}
	resolved, unverified := VerifyPaths(dir, input)

	distinct := map[string]bool{}
	for _, p := range input {
		distinct[p] = true
	}

	if got, want := len(resolved)+len(unverified), len(distinct); got != want {
		t.Errorf("len(resolved)+len(unverified) = %d, want %d (count of distinct input paths) -- "+
			"a path must never be silently dropped", got, want)
	}
}

// TestVerifyPathsWithNoSessionDirectoryMarksAllUnverified pins the
// behaviour when the caller has no known session directory to verify
// against (e.g. Store.GetSession found nothing for the project): every
// path must be reported unverified, never silently presented as confirmed.
func TestVerifyPathsWithNoSessionDirectoryMarksAllUnverified(t *testing.T) {
	resolved, unverified := VerifyPaths("", []string{"a.go", "b.go"})

	if resolved != nil {
		t.Errorf("resolved = %#v, want nil (no session directory to verify against)", resolved)
	}
	want := []string{"a.go", "b.go"}
	if !reflect.DeepEqual(unverified, want) {
		t.Errorf("unverified = %#v, want %#v", unverified, want)
	}
}
