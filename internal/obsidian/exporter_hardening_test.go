package obsidian

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// captureLog installs a diagnostic sink on exp and returns a func that yields
// everything logged so far. Diagnostics go to stderr in production
// (REQ-EXPORT-11 forbids interleaving them into the stdout summary line);
// tests capture them instead of polluting `go test` output.
func captureLog(exp *Exporter) func() []string {
	var lines []string
	exp.logf = func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	return func() []string { return lines }
}

// allFiles returns every regular file under root as a sorted slice of paths.
func allFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// TestResolveVaultPath covers the exporter-side half of C1/C2: a
// VAULT-relative path (the form stored in .engram-sync-state.json) must
// resolve strictly inside {vault}/engram/, or be refused outright.
func TestResolveVaultPath(t *testing.T) {
	vault := t.TempDir()
	exp, err := NewExporter(&mockStore{}, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}

	t.Run("a well-formed note path resolves", func(t *testing.T) {
		got, err := exp.resolveVaultPath("engram/proj1/decision/a-1.md")
		if err != nil {
			t.Fatalf("resolveVaultPath() error = %v, want nil", err)
		}
		want := filepath.Join(vault, "engram", "proj1", "decision", "a-1.md")
		if got != want {
			t.Errorf("resolveVaultPath() = %q, want %q", got, want)
		}
	})

	t.Run("hostile state entries are refused", func(t *testing.T) {
		// The first vector is the one an independent harness confirmed:
		// projectFromRelPath reads segment[1] == "proj1" and PASSES the
		// scope guard, while filepath.Join resolves the target to
		// "{vault}/../victim.md".
		cases := []string{
			"engram/proj1/../../../../victim.md",
			"engram/../victim.md",
			"engram/",
			"engram",
			"..",
			"../victim.md",
			"/etc/passwd",
			`C:\victim.md`,
			"C:/victim.md",
			`\\server\share\victim.md`,
			"decision/a-1.md",
			"ENGRAM/proj1/decision/a-1.md",
			"",
		}
		for _, relPath := range cases {
			t.Run(relPath, func(t *testing.T) {
				got, err := exp.resolveVaultPath(relPath)
				if err == nil {
					t.Fatalf("resolveVaultPath(%q) = %q, want an error", relPath, got)
				}
			})
		}
	})
}

// TestResolveVaultPathRefusesHostileEntryInBothSeparatorForms is a targeted
// regression test for bugfix/obsidian-wikilink-path-separator: normalising
// vault-relative paths to forward slashes MUST NOT weaken the C1/C2
// containment guard. The same escape vector --
// "engram/proj1/../../../../victim.md", which resolves outside the vault
// while the project-scope check still reads segment[1] as "proj1" -- must
// be refused identically whether it arrives in the forward-slash form this
// package now always emits, or in the OS-native backslash form a pre-fix
// build (or a foreign OS) could have left behind in
// .engram-sync-state.json.
func TestResolveVaultPathRefusesHostileEntryInBothSeparatorForms(t *testing.T) {
	vault := t.TempDir()
	exp, err := NewExporter(&mockStore{}, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}

	cases := []struct {
		name string
		path string
	}{
		{"forward-slash form", "engram/proj1/../../../../victim.md"},
		{"backslash form", `engram\proj1\..\..\..\..\victim.md`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := exp.resolveVaultPath(tc.path)
			if err == nil {
				t.Fatalf("resolveVaultPath(%q) = %q, want an error", tc.path, got)
			}
		})
	}
}

// TestExportUpgradesBackslashStateWithoutMassRewrite covers the backward-
// compatible upgrade path for bugfix/obsidian-wikilink-path-separator: a
// state file carrying OS-native-backslash values -- written by a pre-fix
// build of this package, or by a foreign OS across a git-synced vault --
// must still load, and loading it must NOT be mistaken for "everything
// moved": no mass deletion (the note the entry names is still live and on
// disk) and no mass rewrite (REQ-EXPORT-08's idempotency contract holds
// across the upgrade, exactly like an ordinary unchanged re-run).
func TestExportUpgradesBackslashStateWithoutMassRewrite(t *testing.T) {
	vault := t.TempDir()
	ts := time.Now().Add(-time.Hour)

	rec := newTestRecord(1, "proj1", "decision", "sess-1", "Body one", ts)
	store := &mockStore{
		listProjects: []string{"proj1"},
		byProject:    map[string][]*domain.Record{"proj1": {rec}},
	}

	exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("first Export() error = %v", err)
	}

	notePath := filepath.Join(vault, "engram", "proj1", "decision", "body-one-1.md")
	if _, err := os.Stat(notePath); err != nil {
		t.Fatalf("setup: expected %s: %v", notePath, err)
	}

	// Hand-write a PRE-FIX-shaped state file: backslash values, a cutoff far
	// enough in the future that the tracked record is never re-evaluated on
	// timestamp alone.
	statePath := filepath.Join(vault, "engram", stateFileName)
	cutoff := ts.Add(2 * time.Hour).UTC().Format(`2006-01-02T15:04:05.999999999Z07:00`)
	legacy := `{"last_export_at":"` + cutoff + `",` +
		`"files":{"1":"engram\\proj1\\decision\\body-one-1.md"},` +
		`"hubs":{"_sessions/sess-1":"engram\\_sessions\\sess-1.md"}}`
	if err := os.WriteFile(statePath, []byte(legacy), 0644); err != nil {
		t.Fatalf("setup: WriteFile() error = %v", err)
	}

	// Backdate every file on disk (except the state file, which legitimately
	// changes every cycle) so any rewrite is unmistakable.
	backdated := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	files := allFiles(t, vault)
	for _, f := range files {
		if f == statePath {
			continue
		}
		if err := os.Chtimes(f, backdated, backdated); err != nil {
			t.Fatalf("Chtimes(%s): %v", f, err)
		}
	}

	result, err := exp.Export()
	if err != nil {
		t.Fatalf("Export() with a legacy backslash state file error = %v", err)
	}
	if result.Created != 0 || result.Updated != 0 || result.Deleted != 0 || result.Hubs != 0 {
		t.Errorf("Export() with a legacy backslash state file = %+v, want created=updated=deleted=hubs=0 (a true no-op)", *result)
	}
	if result.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", result.Skipped)
	}

	for _, f := range files {
		if f == statePath {
			continue
		}
		info, err := os.Stat(f)
		if err != nil {
			t.Fatalf("Stat(%s): %v", f, err)
		}
		if !info.ModTime().Equal(backdated) {
			t.Errorf("%s was rewritten when loading a legacy backslash-format state file (mtime %v, want %v)", f, info.ModTime(), backdated)
		}
	}

	st, err := ReadState(statePath)
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	if want := "engram/proj1/decision/body-one-1.md"; st.Files[1] != want {
		t.Errorf("state Files[1] = %q, want %q (normalised to forward slashes)", st.Files[1], want)
	}
	if want := "engram/_sessions/sess-1.md"; st.Hubs["_sessions/sess-1"] != want {
		t.Errorf("state Hubs[%q] = %q, want %q", "_sessions/sess-1", st.Hubs["_sessions/sess-1"], want)
	}
}

// TestExportNeverWritesOutsideEngramNamespace covers C2 (REQ-EXPORT-02:
// "MUST NOT write outside {vault}/engram/") end to end, using the exact
// escape vectors confirmed against the pre-fix implementation:
//
//	project=".."               -> {vault}/decision/slug-1.md   (escapes engram/)
//	project=".." AND type=".." -> {vault}/../slug-1.md          (escapes the VAULT)
//
// Both are reachable today: normalizeProject only trims/lowercases/collapses
// and never touches '.' or '/', and mem_save(project:"..", type:"..") is
// accepted through the public MCP tool surface.
func TestExportNeverWritesOutsideEngramNamespace(t *testing.T) {
	base := t.TempDir()
	vault := filepath.Join(base, "vault")
	if err := os.MkdirAll(vault, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	ts := time.Now().Add(-time.Hour)

	dotdotTopic := "../../../victim/escape"
	rec3 := newTestRecord(3, "..", "..", "..", "Third hostile body", ts)
	rec3.TopicKey = &dotdotTopic
	rec4 := newTestRecord(4, "..", "..", "..", "Fourth hostile body", ts)
	rec4.TopicKey = &dotdotTopic

	store := &mockStore{
		listProjects: []string{".."},
		byProject: map[string][]*domain.Record{
			"..": {
				newTestRecord(1, "..", "decision", "sess-1", "First hostile body", ts),
				newTestRecord(2, "..", "..", "sess-1", "Second hostile body", ts),
				rec3,
				rec4,
			},
		},
	}

	exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}

	engramRoot := filepath.Join(vault, "engram") + string(filepath.Separator)
	files := allFiles(t, base)
	if len(files) == 0 {
		t.Fatal("Export() wrote no files at all; the test would prove nothing")
	}
	for _, f := range files {
		if !strings.HasPrefix(f, engramRoot) {
			t.Errorf("Export() wrote %q, which is outside the %q namespace", f, engramRoot)
		}
	}
}

// TestExportSkipsHostileStateEntryWithoutDeleting covers C1: relPath on the
// deletion path comes verbatim out of .engram-sync-state.json, a file that
// lives INSIDE the vault and is therefore writable by anything that can
// write to the vault. A malformed or hostile entry must be skipped with a
// logged diagnostic — never removed, and never fatal to the whole export.
func TestExportSkipsHostileStateEntryWithoutDeleting(t *testing.T) {
	base := t.TempDir()
	vault := filepath.Join(base, "vault")
	if err := os.MkdirAll(filepath.Join(vault, "engram"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	victim := filepath.Join(base, "victim.md")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// "engram/proj1/../../../victim.md" resolves to "{base}/victim.md" while
	// projectFromRelPath still reads segment[1] as "proj1", so the existing
	// project scope guard passes it straight through to os.Remove.
	hostile := "engram/proj1/../../../victim.md"
	statePath := filepath.Join(vault, "engram", stateFileName)
	if err := WriteState(statePath, &SyncState{
		Files: map[int64]string{1: hostile},
	}); err != nil {
		t.Fatalf("setup: WriteState() error = %v", err)
	}

	ts := time.Now().Add(-time.Hour)
	store := &mockStore{
		listProjects: []string{"proj1"},
		byProject: map[string][]*domain.Record{
			"proj1": {newTestRecord(2, "proj1", "decision", "sess-1", "A live observation", ts)},
		},
	}
	exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	logged := captureLog(exp)

	result, err := exp.Export()
	if err != nil {
		t.Fatalf("Export() error = %v; a hostile state entry must not abort the export", err)
	}

	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("the file outside the vault was deleted by a hostile state entry: %v", err)
	}
	if result.Deleted != 0 {
		t.Errorf("Deleted = %d, want 0 (nothing was legitimately deleted)", result.Deleted)
	}
	if result.Created != 1 {
		t.Errorf("Created = %d, want 1 (the export must carry on past the bad entry)", result.Created)
	}

	lines := logged()
	found := false
	for _, l := range lines {
		if strings.Contains(l, "victim.md") {
			found = true
		}
	}
	if !found {
		t.Errorf("no diagnostic logged for the refused state entry; got %v", lines)
	}

	st, err := ReadState(statePath)
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	if st.Files[1] != hostile {
		t.Errorf("state Files[1] = %q, want the refused entry retained (%q) so the alarm persists", st.Files[1], hostile)
	}
}

// TestChangedContentRemovesOrphanedNote covers C3. The filename embeds
// Slugify(rec.Content), so when content changes the exporter computes a NEW
// path and overwrites state.Files[rec.ID] with it — leaving the old file on
// disk, untracked and therefore undeletable by anything, --force included.
//
// The trigger is this repo's PRIMARY write pattern: a topic-keyed mem_save
// performs an in-place UPDATE that keeps the same integer id
// (internal/localstore/apply.go execUpdate).
func TestChangedContentRemovesOrphanedNote(t *testing.T) {
	vault := t.TempDir()
	ts := time.Now().Add(-time.Hour)

	rec := newTestRecord(1, "proj1", "decision", "sess-1", "Original content body", ts)
	store := &mockStore{
		listProjects: []string{"proj1"},
		byProject:    map[string][]*domain.Record{"proj1": {rec}},
	}

	exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("first Export() error = %v", err)
	}

	typeDir := filepath.Join(vault, "engram", "proj1", "decision")
	oldPath := filepath.Join(typeDir, "original-content-body-1.md")
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("expected the first export to write %s: %v", oldPath, err)
	}

	// Same observation id, new content -> new slug -> new filename.
	updated := newTestRecord(1, "proj1", "decision", "sess-1", "Rewritten content body", time.Now())
	store.byProject["proj1"] = []*domain.Record{updated}

	second, err := exp.Export()
	if err != nil {
		t.Fatalf("second Export() error = %v", err)
	}

	newPath := filepath.Join(typeDir, "rewritten-content-body-1.md")
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("expected the second export to write %s: %v", newPath, err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("the note for the old slug still exists at %s (stat err = %v); it is orphaned and untrackable", oldPath, err)
	}

	notes := allFiles(t, typeDir)
	if len(notes) != 1 {
		t.Errorf("observation id=1 has %d files on disk (%v), want exactly 1", len(notes), notes)
	}
	if second.Updated != 1 {
		t.Errorf("Updated = %d, want 1", second.Updated)
	}
	if second.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1 (the orphaned note must be counted)", second.Deleted)
	}

	st, err := ReadState(filepath.Join(vault, "engram", stateFileName))
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	if len(st.Files) != 1 {
		t.Errorf("state Files = %v, want exactly one entry for id=1", st.Files)
	}
}

// TestOrphanCleanupIgnoresPathsThatAliasTheSameFile guards the orphan
// cleanup against deleting the note it has just written.
//
// state.Files stores OS-native separators, so a vault synced between a
// Windows machine and a POSIX one carries entries like
// "engram\proj1\decision\a-1.md" that the other OS renders as
// "engram/proj1/decision/a-1.md". Those are DIFFERENT strings that resolve
// to the SAME file. A raw string comparison would classify the note as
// superseded and remove it immediately after writing it — turning a
// cross-machine vault into silent data loss on the first export.
func TestOrphanCleanupIgnoresPathsThatAliasTheSameFile(t *testing.T) {
	vault := t.TempDir()
	ts := time.Now().Add(-time.Hour)

	store := &mockStore{
		listProjects: []string{"proj1"},
		byProject: map[string][]*domain.Record{
			"proj1": {newTestRecord(1, "proj1", "decision", "sess-1", "Original content body", ts)},
		},
	}
	exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("first Export() error = %v", err)
	}

	notePath := filepath.Join(vault, "engram", "proj1", "decision", "original-content-body-1.md")
	if _, err := os.Stat(notePath); err != nil {
		t.Fatalf("setup: expected %s: %v", notePath, err)
	}

	// Rewrite the state entry in the OTHER platform's separator style, as a
	// vault synced from another machine would carry it, and clear the cutoff
	// so the record is re-evaluated rather than skipped.
	statePath := filepath.Join(vault, "engram", stateFileName)
	st, err := ReadState(statePath)
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	native := st.Files[1]
	var foreign string
	if strings.Contains(native, `\`) {
		foreign = strings.ReplaceAll(native, `\`, "/")
	} else {
		foreign = strings.ReplaceAll(native, "/", `\`)
	}
	if foreign == native {
		t.Fatalf("setup: could not build a foreign-separator form of %q", native)
	}
	st.Files[1] = foreign
	st.LastExportAt = time.Time{}
	if err := WriteState(statePath, st); err != nil {
		t.Fatalf("WriteState() error = %v", err)
	}

	if _, err := exp.Export(); err != nil {
		t.Fatalf("second Export() error = %v", err)
	}

	if _, err := os.Stat(notePath); err != nil {
		t.Fatalf("the note was deleted by the orphan cleanup even though both state paths name the SAME file: %v", err)
	}
}

// TestUntrackedObservationIsNeverSkipped covers the second half of C4. The
// recovered design (#1220) specified the skip condition as "updated_at <=
// cutoff AND the ID is already in state.Files"; the rebuild dropped the
// second clause. Without it, an observation older than the cutoff but never
// actually written is skipped forever — and the skip branch even recorded it
// in state.Files as exported when no file was ever written.
func TestUntrackedObservationIsNeverSkipped(t *testing.T) {
	vault := t.TempDir()

	// A state file that claims a very recent export but tracks NO files at
	// all — exactly the shape left behind by the TOCTOU window, by a failed
	// cycle, or by a user deleting notes out of the vault by hand.
	statePath := filepath.Join(vault, "engram", stateFileName)
	if err := WriteState(statePath, &SyncState{
		LastExportAt: time.Now().Add(time.Hour),
		Files:        map[int64]string{},
	}); err != nil {
		t.Fatalf("setup: WriteState() error = %v", err)
	}

	ts := time.Now().Add(-time.Hour) // older than the cutoff
	store := &mockStore{
		listProjects: []string{"proj1"},
		byProject: map[string][]*domain.Record{
			"proj1": {newTestRecord(1, "proj1", "decision", "sess-1", "Never written body", ts)},
		},
	}
	exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}

	result, err := exp.Export()
	if err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	if result.Created != 1 {
		t.Errorf("Created = %d, want 1 (an untracked observation must never be skipped on timestamp alone)", result.Created)
	}
	if result.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0", result.Skipped)
	}

	notePath := filepath.Join(vault, "engram", "proj1", "decision", "never-written-body-1.md")
	if _, err := os.Stat(notePath); err != nil {
		t.Fatalf("expected %s on disk: %v", notePath, err)
	}
}

// injectingStore simulates the C4 TOCTOU window: an observation is saved to
// the store AFTER the exporter has already read it, i.e. inside the
// read -> write gap of a single export cycle. With the cutoff stamped after
// the work, that observation's updated_at lands before the persisted cutoff
// and it is skipped forever, silently, with no error.
type injectingStore struct {
	*mockStore
	injected bool
	project  string
}

func (s *injectingStore) RecentObservations(project, scope string, limit int) ([]*domain.Record, error) {
	recs, err := s.mockStore.RecentObservations(project, scope, limit)
	if err != nil {
		return nil, err
	}
	if !s.injected && project == s.project {
		// The store's clock must advance past the exporter's cycle start,
		// otherwise the fixture would not reproduce the race at all.
		time.Sleep(2 * time.Millisecond)
		now := time.Now()
		s.mockStore.byProject[project] = append(s.mockStore.byProject[project],
			newTestRecord(99, project, "decision", "sess-1", "Saved mid-export body", now))
		s.injected = true
	}
	return recs, nil
}

// TestObservationSavedDuringExportIsExportedNextCycle covers the first half
// of C4 (bugfix/obsidian-exporter-cutoff-toctou): the incremental cutoff must
// be captured BEFORE the first read, not after all the work.
func TestObservationSavedDuringExportIsExportedNextCycle(t *testing.T) {
	vault := t.TempDir()
	ts := time.Now().Add(-time.Hour)

	base := &mockStore{
		listProjects: []string{"proj1"},
		byProject: map[string][]*domain.Record{
			"proj1": {newTestRecord(1, "proj1", "decision", "sess-1", "Pre-existing body", ts)},
		},
	}
	store := &injectingStore{mockStore: base, project: "proj1"}

	exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}

	first, err := exp.Export()
	if err != nil {
		t.Fatalf("first Export() error = %v", err)
	}
	if first.Created != 1 {
		t.Fatalf("first Export(): Created = %d, want 1 (only the pre-existing observation was readable)", first.Created)
	}

	second, err := exp.Export()
	if err != nil {
		t.Fatalf("second Export() error = %v", err)
	}
	if second.Created != 1 {
		t.Errorf("second Export(): Created = %d, want 1 — the observation saved during the first cycle must still be exported, not skipped forever", second.Created)
	}

	notePath := filepath.Join(vault, "engram", "proj1", "decision", "saved-mid-export-body-99.md")
	if _, err := os.Stat(notePath); err != nil {
		t.Fatalf("the observation saved during the first export was never written: %v", err)
	}
}

// TestCorruptStateFileIsRecoverableWithForce covers the recovery half of the
// state file's durability story. A state file that cannot be parsed used to
// be terminal: Export propagated the parse error and --force did NOT bypass
// it, so there was no way back from inside the CLI — the user had to know
// the file existed, know where it lived, and delete a dot-file by hand.
//
// --force already means "ignore the persisted cutoff and re-evaluate every
// live observation", which is precisely what rebuilding from an empty state
// requires, so it is the natural repair lever.
func TestCorruptStateFileIsRecoverableWithForce(t *testing.T) {
	setup := func(t *testing.T) (string, *mockStore) {
		t.Helper()
		vault := t.TempDir()
		if err := os.MkdirAll(filepath.Join(vault, "engram"), 0o755); err != nil {
			t.Fatalf("setup: %v", err)
		}
		// A half-written file is exactly what a non-atomic write produced
		// when it was interrupted.
		if err := os.WriteFile(filepath.Join(vault, "engram", stateFileName),
			[]byte(`{"last_export_at":"2026-07-27T12:00:00Z","fi`), 0o644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		ts := time.Now().Add(-time.Hour)
		return vault, &mockStore{
			listProjects: []string{"proj1"},
			byProject: map[string][]*domain.Record{
				"proj1": {
					newTestRecord(1, "proj1", "decision", "sess-1", "First body", ts),
					newTestRecord(2, "proj1", "bugfix", "sess-1", "Second body", ts),
				},
			},
		}
	}

	t.Run("without --force a corrupt state file is still a hard error", func(t *testing.T) {
		vault, store := setup(t)
		exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
		if err != nil {
			t.Fatalf("NewExporter() error = %v", err)
		}
		captureLog(exp)
		if _, err := exp.Export(); err == nil {
			t.Fatal("Export() error = nil; a corrupt state file must not be silently discarded without --force")
		}
	})

	t.Run("--force rebuilds from an empty state and repairs the file", func(t *testing.T) {
		vault, store := setup(t)
		exp, err := NewExporter(store, ExportConfig{VaultPath: vault, Force: true})
		if err != nil {
			t.Fatalf("NewExporter() error = %v", err)
		}
		logged := captureLog(exp)

		result, err := exp.Export()
		if err != nil {
			t.Fatalf("Export() with --force error = %v; a corrupt state file must be recoverable", err)
		}
		if result.Created != 2 {
			t.Errorf("Created = %d, want 2 (everything is re-exported from an empty state)", result.Created)
		}

		statePath := filepath.Join(vault, "engram", stateFileName)
		st, err := ReadState(statePath)
		if err != nil {
			t.Fatalf("state file is still unparseable after a --force run: %v", err)
		}
		if len(st.Files) != 2 {
			t.Errorf("state Files = %v, want 2 entries", st.Files)
		}

		var mentioned bool
		for _, l := range logged() {
			if strings.Contains(l, stateFileName) {
				mentioned = true
			}
		}
		if !mentioned {
			t.Errorf("discarding a corrupt state file must be reported; logged = %v", logged())
		}
	})
}

// TestExportIsIdempotent asserts REQ-EXPORT-08's core claim directly:
// "Unchanged observations produce no disk I/O." The returned counters are
// exactly what is in doubt, so this checks the filesystem instead — every
// exported file is backdated to a known timestamp after the first run, and a
// second run must leave every one of those timestamps untouched.
func TestExportIsIdempotent(t *testing.T) {
	vault := t.TempDir()
	ts := time.Now().Add(-time.Hour)

	byProject := map[string][]*domain.Record{"proj-a": {}, "proj-b": {}}
	projects := []string{"proj-a", "proj-b"}
	types := []string{"architecture", "bugfix", "decision"}
	for i := 0; i < 10; i++ {
		id := int64(i + 1)
		proj := projects[i%len(projects)]
		rec := newTestRecord(id, proj, types[i%len(types)], fmt.Sprintf("sess-%d", i%3),
			fmt.Sprintf("Observation body number %d", id), ts)
		if id == 1 || id == 2 {
			topic := "sdd/idempotency/spec"
			rec.TopicKey = &topic
		}
		byProject[proj] = append(byProject[proj], rec)
	}
	store := &mockStore{listProjects: projects, byProject: byProject}

	exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	first, err := exp.Export()
	if err != nil {
		t.Fatalf("first Export() error = %v", err)
	}
	if first.Created != 10 {
		t.Fatalf("first Export(): Created = %d, want 10", first.Created)
	}
	if first.Hubs == 0 {
		t.Fatal("first Export(): Hubs = 0; the fixture must produce hub notes for this test to cover them")
	}

	// Backdate every exported file to a whole second in the past so that any
	// rewrite by the second run is unmistakable, without sleeping and
	// without depending on filesystem timestamp granularity.
	statePath := filepath.Join(vault, "engram", stateFileName)
	backdated := time.Now().Add(-time.Hour).Truncate(time.Second)
	files := allFiles(t, vault)
	if len(files) < 12 {
		t.Fatalf("expected at least 12 files after the first export, got %d: %v", len(files), files)
	}
	for _, f := range files {
		if err := os.Chtimes(f, backdated, backdated); err != nil {
			t.Fatalf("Chtimes(%s): %v", f, err)
		}
	}

	second, err := exp.Export()
	if err != nil {
		t.Fatalf("second Export() error = %v", err)
	}
	if second.Created != 0 || second.Updated != 0 || second.Deleted != 0 {
		t.Errorf("second Export(): Created=%d Updated=%d Deleted=%d, want all 0",
			second.Created, second.Updated, second.Deleted)
	}
	if second.Skipped != 10 {
		t.Errorf("second Export(): Skipped = %d, want 10", second.Skipped)
	}

	after := allFiles(t, vault)
	if len(after) != len(files) {
		t.Errorf("file count changed across an unchanged re-run: %d -> %d", len(files), len(after))
	}
	for _, f := range after {
		if f == statePath {
			// The state file legitimately changes every cycle: it carries
			// the new last-export timestamp.
			continue
		}
		info, err := os.Stat(f)
		if err != nil {
			t.Fatalf("Stat(%s): %v", f, err)
		}
		if !info.ModTime().Equal(backdated) {
			t.Errorf("%s was rewritten by an unchanged re-run (mtime %v, want %v)", f, info.ModTime(), backdated)
		}
	}
}
