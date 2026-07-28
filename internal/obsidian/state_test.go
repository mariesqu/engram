package obsidian

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSyncStateRoundTrip covers REQ-EXPORT-06 (incremental sync) and
// REQ-EXPORT-09 (deletion via state diff): SyncState must carry both the
// last-export timestamp AND an id -> vault-path map, since that map is the
// only deletion-detection mechanism available (RecentObservations hard-
// filters deleted_at IS NULL and localstore exposes no tombstone listing).
func TestSyncStateRoundTrip(t *testing.T) {
	t.Run("write then read round-trips timestamp and file map", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".engram-sync-state.json")

		want := &SyncState{
			LastExportAt: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
			Files: map[int64]string{
				1: "engram/engram/architecture/foo-1.md",
				2: "engram/engram/bugfix/bar-2.md",
			},
		}
		if err := WriteState(path, want); err != nil {
			t.Fatalf("WriteState() error = %v", err)
		}

		got, err := ReadState(path)
		if err != nil {
			t.Fatalf("ReadState() error = %v", err)
		}
		if !got.LastExportAt.Equal(want.LastExportAt) {
			t.Errorf("LastExportAt = %v, want %v", got.LastExportAt, want.LastExportAt)
		}
		if len(got.Files) != len(want.Files) {
			t.Fatalf("Files = %v, want %v", got.Files, want.Files)
		}
		for id, path := range want.Files {
			if got.Files[id] != path {
				t.Errorf("Files[%d] = %q, want %q", id, got.Files[id], path)
			}
		}
	})

	t.Run("reading a missing state file returns an empty state, not an error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "does-not-exist.json")

		got, err := ReadState(path)
		if err != nil {
			t.Fatalf("ReadState() on missing file error = %v, want nil", err)
		}
		if got == nil {
			t.Fatal("ReadState() on missing file returned nil state")
		}
		if !got.LastExportAt.IsZero() {
			t.Errorf("LastExportAt = %v, want zero value", got.LastExportAt)
		}
		if got.Files == nil {
			t.Error("Files map is nil, want an initialized empty map")
		}
	})

	t.Run("reading a corrupt state file returns an error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "corrupt.json")
		if err := os.WriteFile(path, []byte("{not valid json"), 0644); err != nil {
			t.Fatalf("setup: WriteFile() error = %v", err)
		}

		_, err := ReadState(path)
		if err == nil {
			t.Fatal("ReadState() on corrupt file error = nil, want an error")
		}
	})
}

// TestExportResultSummary pins REQ-EXPORT-11's wire format exactly. This is
// the single line an operator greps to tell whether a long-running watch
// daemon is alive and doing work, and it is the format tasks 3.2 and 8.6
// both depend on. It had zero test coverage: Summary() appeared exactly
// twice in the repository, at its definition and at its one call site.
func TestExportResultSummary(t *testing.T) {
	cases := []struct {
		name string
		in   ExportResult
		want string
	}{
		{
			name: "every counter distinct so no two fields can be transposed",
			in:   ExportResult{Created: 1, Updated: 2, Deleted: 3, Skipped: 4, Hubs: 5},
			want: "sync: created=1 updated=2 deleted=3 skipped=4 hubs=5",
		},
		{
			name: "zero value",
			in:   ExportResult{},
			want: "sync: created=0 updated=0 deleted=0 skipped=0 hubs=0",
		},
		{
			name: "a realistic incremental cycle",
			in:   ExportResult{Created: 3, Updated: 0, Deleted: 1, Skipped: 1738, Hubs: 42},
			want: "sync: created=3 updated=0 deleted=1 skipped=1738 hubs=42",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Summary(); got != tc.want {
				t.Errorf("Summary() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWriteStateIsAtomic covers the state file's durability contract. A bare
// os.WriteFile opens the destination with O_TRUNC and then streams bytes
// into it, so the file is observably EMPTY or PARTIAL for the duration of
// the write. Two consumers are exposed to that window: the Obsidian viewer
// polls this file every 60 s (REQ-PLUGIN-06), and the exporter's own next
// run parses it — where a truncated file is a hard, unrecoverable error.
//
// The state must therefore be staged in a temp file and moved into place
// with a single rename. A concurrent reader is how that is observable: it
// must never see anything but a complete, parseable state.
func TestWriteStateIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".engram-sync-state.json")

	const entries = 3000
	big := &SyncState{LastExportAt: time.Now().UTC(), Files: map[int64]string{}}
	for i := int64(1); i <= entries; i++ {
		big.Files[i] = fmt.Sprintf("engram/some-project/architecture/an-observation-slug-of-realistic-length-%d.md", i)
	}
	if err := WriteState(path, big); err != nil {
		t.Fatalf("setup: WriteState() error = %v", err)
	}

	const rounds = 40
	done := make(chan error, 1)
	go func() {
		for i := 0; i < rounds; i++ {
			if err := WriteState(path, big); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	// Three outcomes are counted separately, because they mean very
	// different things:
	//
	//   torn        - the file OPENED and its bytes were incomplete. This is
	//                 the defect: a torn state file is what bricks the next
	//                 export (an unparseable state is fatal) and what makes
	//                 the viewer render a healthy vault as absent. It must
	//                 be exactly zero.
	//   unavailable - the raw open failed transiently. On Windows the rename
	//                 briefly locks the destination, so this is expected and
	//                 harmless: no wrong data is ever observed.
	//   readState   - the production read path failed. Its bounded retry
	//                 exists to absorb `unavailable`, so this must also be
	//                 zero.
	var reads, torn, unavailable, readStateErrs int
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("WriteState() error = %v", err)
			}
			if reads == 0 {
				t.Fatal("the reader never observed the file; the test would prove nothing")
			}
			if torn > 0 {
				t.Errorf("%d of %d concurrent reads opened the state file and got INCOMPLETE bytes; the write is not atomic", torn, reads)
			}
			if readStateErrs > 0 {
				t.Errorf("ReadState failed on %d of %d concurrent reads (%d transient open failures); the read retry is not absorbing the rename window",
					readStateErrs, reads, unavailable)
			}
			t.Logf("reads=%d torn=%d transient-open-failures=%d", reads, torn, unavailable)
			return
		default:
			reads++
			if raw, err := os.ReadFile(path); err != nil {
				unavailable++
			} else {
				var st SyncState
				if json.Unmarshal(raw, &st) != nil || len(st.Files) != entries {
					torn++
				}
			}
			if _, err := ReadState(path); err != nil {
				readStateErrs++
			}
		}
	}
}

// TestWriteStateLeavesNoTemporaryFiles guards the other half of temp+rename:
// the staging file must not accumulate in the vault, where Obsidian would
// index it and the user would see it.
func TestWriteStateLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".engram-sync-state.json")

	for i := 0; i < 5; i++ {
		if err := WriteState(path, &SyncState{
			LastExportAt: time.Now().UTC(),
			Files:        map[int64]string{1: "engram/p/architecture/a-1.md"},
		}); err != nil {
			t.Fatalf("WriteState() error = %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != ".engram-sync-state.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("state directory contains %v, want exactly [.engram-sync-state.json]", names)
	}
}

// TestSyncStateRoundTripsHubs covers REQ-EXPORT-09's NEW clause (closes
// deferred MAJOR-4): SyncState must carry a Hubs map (hub identity ->
// vault-relative path) alongside Files, and it must round-trip through
// WriteState/ReadState exactly like Files does.
func TestSyncStateRoundTripsHubs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".engram-sync-state.json")

	want := &SyncState{
		LastExportAt: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
		Files:        map[int64]string{1: "engram/proj/architecture/foo-1.md"},
		Hubs: map[string]string{
			"_sessions/manual-save-engram": "engram/_sessions/manual-save-engram.md",
			"_topics/sdd/some-change":      "engram/_topics/sdd-some-change.md",
		},
	}
	if err := WriteState(path, want); err != nil {
		t.Fatalf("WriteState() error = %v", err)
	}

	got, err := ReadState(path)
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	if len(got.Hubs) != len(want.Hubs) {
		t.Fatalf("Hubs = %v, want %v", got.Hubs, want.Hubs)
	}
	for identity, relPath := range want.Hubs {
		if got.Hubs[identity] != relPath {
			t.Errorf("Hubs[%q] = %q, want %q", identity, got.Hubs[identity], relPath)
		}
	}
}

// TestReadStateNormalisesMissingHubsMap covers the backward-compatible
// upgrade path: a JSON document written before the Hubs field existed has
// "files" but no "hubs" key at all. json.Unmarshal leaves the field nil;
// ReadState must normalise that to a non-nil empty map, exactly as it
// already does for Files, so callers never hit a nil-map panic on the next
// WriteState and so the stale-hub sweep has an (empty) map to diff against
// rather than nothing.
func TestReadStateNormalisesMissingHubsMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".engram-sync-state.json")
	legacy := `{"last_export_at":"2026-07-27T12:00:00Z","files":{"1":"engram/proj/architecture/foo-1.md"}}`
	if err := os.WriteFile(path, []byte(legacy), 0644); err != nil {
		t.Fatalf("setup: WriteFile() error = %v", err)
	}

	got, err := ReadState(path)
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	if got.Hubs == nil {
		t.Fatal("Hubs map is nil, want an initialized empty map")
	}
	if len(got.Hubs) != 0 {
		t.Errorf("Hubs = %v, want empty", got.Hubs)
	}

	// A subsequent write must not panic assigning into a nil map.
	got.Hubs["_sessions/new"] = "engram/_sessions/new.md"
	if err := WriteState(path, got); err != nil {
		t.Fatalf("WriteState() after normalising Hubs error = %v", err)
	}
}

// TestReadStateNormalisesBackslashPaths covers
// bugfix/obsidian-wikilink-path-separator's SyncState half: a state file
// written by a pre-fix build (or by a Windows process later read on a
// synced macOS/Linux checkout) can carry OS-native-backslash values in both
// "files" and "hubs". Obsidian wikilinks and this package's own containment
// guard both require forward slashes, so ReadState must normalise every
// value the instant it is loaded into memory -- callers must never see a
// backslash in a SyncState value, regardless of what the on-disk JSON
// contains.
//
// filepath.ToSlash is deliberately NOT the mechanism under test here (see
// toVaultSlash's doc comment): it is a no-op on any OS whose own separator
// is already '/', so it would leave a Windows-authored backslash value
// untouched when this process runs on macOS or Linux -- exactly the
// cross-machine, git-synced-vault scenario this fix exists for. This test
// asserts the OBSERVABLE behavior (values are forward-slash after
// ReadState), not which OS the test happens to run on.
func TestReadStateNormalisesBackslashPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".engram-sync-state.json")
	legacy := `{"last_export_at":"2026-07-27T12:00:00Z",` +
		`"files":{"1":"engram\\proj1\\architecture\\foo-1.md"},` +
		`"hubs":{"_sessions/sess-1":"engram\\_sessions\\sess-1.md",` +
		`"_topics/sdd/change":"engram\\_topics\\sdd-change.md"}}`
	if err := os.WriteFile(path, []byte(legacy), 0644); err != nil {
		t.Fatalf("setup: WriteFile() error = %v", err)
	}

	got, err := ReadState(path)
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}

	if want := "engram/proj1/architecture/foo-1.md"; got.Files[1] != want {
		t.Errorf("Files[1] = %q, want %q (backslashes must be normalised on read)", got.Files[1], want)
	}
	if want := "engram/_sessions/sess-1.md"; got.Hubs["_sessions/sess-1"] != want {
		t.Errorf("Hubs[%q] = %q, want %q", "_sessions/sess-1", got.Hubs["_sessions/sess-1"], want)
	}
	if want := "engram/_topics/sdd-change.md"; got.Hubs["_topics/sdd/change"] != want {
		t.Errorf("Hubs[%q] = %q, want %q", "_topics/sdd/change", got.Hubs["_topics/sdd/change"], want)
	}

	for id, p := range got.Files {
		if strings.Contains(p, `\`) {
			t.Errorf("Files[%d] = %q still contains a backslash after ReadState", id, p)
		}
	}
	for k, p := range got.Hubs {
		if strings.Contains(p, `\`) {
			t.Errorf("Hubs[%q] = %q still contains a backslash after ReadState", k, p)
		}
	}
}
