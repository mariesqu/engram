package obsidian

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// writeStateWithWriter stages a .engram-sync-state.json fixture directly
// (bypassing Export()) with the given LastWriterID, so single-writer
// detection tests can set up "a state file last written by writer X" without
// first driving an entire prior cycle through the exporter.
func writeStateWithWriter(t *testing.T, vault, writerID string) {
	t.Helper()
	statePath := filepath.Join(vault, engramDir, stateFileName)
	st := &SyncState{
		Files:        map[int64]string{},
		Hubs:         map[string]string{},
		Narratives:   map[string]string{},
		LastWriterID: writerID,
	}
	if err := WriteState(statePath, st); err != nil {
		t.Fatalf("setup: WriteState() error = %v", err)
	}
}

// singleObservationStore returns a fresh mockStore carrying exactly one live
// observation, old enough that a second cycle over the same store (without
// --force) treats it as already-synced and skips it — these tests care about
// the single-writer warning, not the create/update/skip counters.
func singleObservationStore() *mockStore {
	return &mockStore{
		listProjects: []string{"proj1"},
		byProject: map[string][]*domain.Record{
			"proj1": {newTestRecord(1, "proj1", "architecture", "sess-1", "Body", time.Now().Add(-time.Hour))},
		},
	}
}

func countSubstring(lines []string, substr string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// TestSingleWriterMismatchWarnsAndDoesNotBlock covers REQ-NARR-08: a
// mismatched writer_id logs exactly one loud warning naming BOTH writer ids,
// and the cycle completes normally regardless — a legitimate machine
// migration must still export successfully, never be blocked by this check.
func TestSingleWriterMismatchWarnsAndDoesNotBlock(t *testing.T) {
	vault := t.TempDir()
	writeStateWithWriter(t, vault, "writer-a")

	exp, err := NewExporter(singleObservationStore(), ExportConfig{VaultPath: vault, LocalWriterID: "writer-b"})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	getLogs := captureLog(exp)

	result, err := exp.Export()
	if err != nil {
		t.Fatalf("Export() error = %v, want the cycle to complete despite the writer mismatch", err)
	}
	if result.Created != 1 {
		t.Errorf("Created = %d, want 1 — the cycle must still write the vault, not block", result.Created)
	}

	warnings := 0
	for _, l := range getLogs() {
		if strings.Contains(l, "writer-a") && strings.Contains(l, "writer-b") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("got %d warning(s) naming both writer-a and writer-b, want exactly 1: %v", warnings, getLogs())
	}

	got, err := ReadState(filepath.Join(vault, engramDir, stateFileName))
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	if got.LastWriterID != "writer-b" {
		t.Errorf("state.LastWriterID after the cycle = %q, want %q", got.LastWriterID, "writer-b")
	}
}

// TestSameWriterEmitsNoWarning is the negative counterpart: the same writer
// exporting again must never trigger the mismatch warning.
func TestSameWriterEmitsNoWarning(t *testing.T) {
	vault := t.TempDir()
	writeStateWithWriter(t, vault, "writer-a")

	exp, err := NewExporter(singleObservationStore(), ExportConfig{VaultPath: vault, LocalWriterID: "writer-a"})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	getLogs := captureLog(exp)

	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	if n := countSubstring(getLogs(), "WARNING"); n != 0 {
		t.Errorf("got %d WARNING log(s) with the same writer id, want 0: %v", n, getLogs())
	}
}

// TestFirstRunEmitsNoWarning is a GUARD: with no prior state file there is
// no recorded writer to differ from, so the mismatch check is trivially
// silent — nothing warns yet on a first run, full stop. It is a genuine (if
// easy) property of the production guard rather than a tautology of the
// test itself: mutation-prove by making the warning unconditional and
// confirming this fails.
func TestFirstRunEmitsNoWarning(t *testing.T) {
	vault := t.TempDir()

	exp, err := NewExporter(singleObservationStore(), ExportConfig{VaultPath: vault, LocalWriterID: "writer-a"})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	getLogs := captureLog(exp)

	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	if n := countSubstring(getLogs(), "WARNING"); n != 0 {
		t.Errorf("got %d WARNING log(s) on the very first run, want 0: %v", n, getLogs())
	}

	got, err := ReadState(filepath.Join(vault, engramDir, stateFileName))
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	if got.LastWriterID != "writer-a" {
		t.Errorf("state.LastWriterID = %q, want %q recorded for the first time", got.LastWriterID, "writer-a")
	}
}

// TestEmptyLocalWriterIDSkipsTheCheckEntirely is a GUARD: an unconfigured
// LocalWriterID (a bare manual CLI run) must never compare "" against a real
// recorded writer id and must never produce a false-alarm warning.
// Mutation-prove by dropping the `e.cfg.LocalWriterID != ""` guard in
// Export().
func TestEmptyLocalWriterIDSkipsTheCheckEntirely(t *testing.T) {
	vault := t.TempDir()
	writeStateWithWriter(t, vault, "writer-a")

	exp, err := NewExporter(singleObservationStore(), ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	getLogs := captureLog(exp)

	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	if n := countSubstring(getLogs(), "WARNING"); n != 0 {
		t.Errorf("got %d WARNING log(s) with an unconfigured LocalWriterID, want 0: %v", n, getLogs())
	}
}

// TestMigrationWarnsOnceNotForever covers the exact firing shape REQ-NARR-08
// asks for: a one-off machine migration warns on the FIRST cycle after it
// happens and then falls silent, because the end-of-cycle assignment
// (state.LastWriterID = e.cfg.LocalWriterID) moves the recorded writer
// forward before the next cycle's ReadState ever runs.
func TestMigrationWarnsOnceNotForever(t *testing.T) {
	vault := t.TempDir()
	writeStateWithWriter(t, vault, "writer-a")
	store := singleObservationStore()

	exp1, err := NewExporter(store, ExportConfig{VaultPath: vault, LocalWriterID: "writer-b"})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	getLogs1 := captureLog(exp1)
	if _, err := exp1.Export(); err != nil {
		t.Fatalf("cycle 1: Export() error = %v", err)
	}
	if n := countSubstring(getLogs1(), "WARNING"); n != 1 {
		t.Fatalf("cycle 1: got %d WARNING log(s), want exactly 1: %v", n, getLogs1())
	}

	exp2, err := NewExporter(store, ExportConfig{VaultPath: vault, LocalWriterID: "writer-b"})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	getLogs2 := captureLog(exp2)
	if _, err := exp2.Export(); err != nil {
		t.Fatalf("cycle 2: Export() error = %v", err)
	}
	if n := countSubstring(getLogs2(), "WARNING"); n != 0 {
		t.Fatalf("cycle 2 (same writer as cycle 1 recorded): got %d WARNING log(s), want 0: %v", n, getLogs2())
	}
}

// TestTwoAlternatingWritersWarnEveryCycle is the pathological counterpart to
// TestMigrationWarnsOnceNotForever: two machines genuinely alternating
// exports into the same vault deserve continuous noise, every cycle, never
// just once.
func TestTwoAlternatingWritersWarnEveryCycle(t *testing.T) {
	vault := t.TempDir()
	writeStateWithWriter(t, vault, "writer-a")
	store := singleObservationStore()

	writers := []string{"writer-b", "writer-a", "writer-b"}
	for i, w := range writers {
		exp, err := NewExporter(store, ExportConfig{VaultPath: vault, LocalWriterID: w})
		if err != nil {
			t.Fatalf("cycle %d: NewExporter() error = %v", i, err)
		}
		getLogs := captureLog(exp)
		if _, err := exp.Export(); err != nil {
			t.Fatalf("cycle %d: Export() error = %v", i, err)
		}
		if n := countSubstring(getLogs(), "WARNING"); n != 1 {
			t.Errorf("cycle %d (writer %q): got %d WARNING log(s), want exactly 1: %v", i, w, n, getLogs())
		}
	}
}

// TestProvenanceRewriteIsOneTimeThenStable pins the design's stated caveat
// (design section 6, "idempotency_analysis"): adding provenance frontmatter
// to ObservationToMarkdown changes the rendered bytes of every existing
// observation note, so the FIRST export a maintainer runs against a
// pre-Phase-B vault (via --force, the documented repair/upgrade lever —
// see Export()'s unparseable-state fallback) rewrites every already-tracked
// note exactly once. The SECOND run, with no --force and no further data
// change, must be the standard REQ-EXPORT-08 no-op:
// `created=0 updated=0 deleted=0 hubs=0`. If it is not, a wall-clock or
// filesystem dependency leaked into the frontmatter (task 7.10) — the
// rewrite must be caused by the one-time code upgrade, never by anything
// that recomputes on every cycle.
func TestProvenanceRewriteIsOneTimeThenStable(t *testing.T) {
	vault := t.TempDir()
	old := time.Now().Add(-720 * time.Hour)

	records := []*domain.Record{
		newTestRecord(1, "proj1", "architecture", "sess-1", "First observation body", old),
		newTestRecord(2, "proj1", "bugfix", "sess-1", "Second observation body", old),
		newTestRecord(3, "proj1", "decision", "sess-2", "Third observation body", old),
	}
	store := &mockStore{
		listProjects: []string{"proj1"},
		byProject:    map[string][]*domain.Record{"proj1": records},
	}

	// Simulate a pre-Phase-B state file: every observation already tracked
	// at exactly the path the CURRENT exporter also computes (the filename
	// scheme did not change in Phase 7 — only the frontmatter body did).
	statePath := filepath.Join(vault, engramDir, stateFileName)
	preState := &SyncState{
		Files:      map[int64]string{},
		Hubs:       map[string]string{},
		Narratives: map[string]string{},
	}
	for _, rec := range records {
		preState.Files[rec.ID] = vaultRelPath(rec)
	}
	if err := WriteState(statePath, preState); err != nil {
		t.Fatalf("setup: WriteState() error = %v", err)
	}

	exp1, err := NewExporter(store, ExportConfig{VaultPath: vault, Force: true})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	first, err := exp1.Export()
	if err != nil {
		t.Fatalf("first Export() error = %v", err)
	}
	if first.Updated != len(records) {
		t.Errorf("first Export(): Updated = %d, want %d — every pre-Phase-B note must be rewritten exactly once", first.Updated, len(records))
	}
	if first.Created != 0 {
		t.Errorf("first Export(): Created = %d, want 0 — these observations were already tracked, not new", first.Created)
	}

	// Second run: normal operation, no --force, nothing in the underlying
	// data changed between the two calls.
	exp2, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	second, err := exp2.Export()
	if err != nil {
		t.Fatalf("second Export() error = %v", err)
	}
	if got, want := second.Summary(), "sync: created=0 updated=0 deleted=0 skipped=3 hubs=0"; got != want {
		t.Errorf("second Export().Summary() = %q, want %q — a clean no-op", got, want)
	}
}

// narrativeFilesUnder returns every file under {vault}/engram/_narratives, or
// an empty slice when that directory does not exist at all (the OFF/nothing-
// rendered case — a missing directory is not a test failure by itself).
func narrativeFilesUnder(t *testing.T, vault string) []string {
	t.Helper()
	root := filepath.Join(vault, "engram", "_narratives")
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			out = append(out, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// TestNarrativeNotesRenderedUnderTheNarrativesSubtree covers REQ-NARR-07's
// vault-layout amendment: engram/_narratives/{safeFilename(project)}/
// {safeFilename(topic_prefix)}.md, resolved through the EXISTING
// resolveVaultPath containment guard.
func TestNarrativeNotesRenderedUnderTheNarrativesSubtree(t *testing.T) {
	vault := t.TempDir()
	reader := &fakeNarrativeReader{rows: map[string]Narrative{
		"proj1\x00sdd/change": {
			Body:          "Narrative body text.",
			Model:         "mistral-small-latest",
			GeneratedAt:   time.Now(),
			SourceCount:   3,
			SourceWriters: []string{"writer-a"},
		},
	}}
	exp, err := NewExporter(&mockStore{}, ExportConfig{VaultPath: vault, Narratives: reader})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}

	wantPath := filepath.Join(vault, "engram", "_narratives", "proj1", "sdd-change.md")
	content, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("expected narrative note at %s: %v", wantPath, err)
	}
	if !strings.Contains(string(content), "Narrative body text.") {
		t.Errorf("narrative note missing the model's body:\n%s", content)
	}
}

// TestHostileNarrativeKeyCannotEscapeTheVault covers the same containment
// discipline already proven for state.Files/state.Hubs entries, applied to
// the NarrativeReader's map keys: a key missing the "\x00" identity
// separator (so it cannot possibly be a (project, topic_prefix) pair) is
// refused, logged, and never written — never coerced into a plausible-but-
// wrong path.
func TestHostileNarrativeKeyCannotEscapeTheVault(t *testing.T) {
	vault := t.TempDir()
	hostileKeys := []string{
		"..",
		"../..",
		"/etc/passwd",
		"proj\x00topic\x00extra", // a SECOND NUL beyond the one expected delimiter
	}
	rows := map[string]Narrative{}
	for _, k := range hostileKeys {
		rows[k] = Narrative{Body: "hostile", Model: "m", GeneratedAt: time.Now()}
	}
	exp, err := NewExporter(&mockStore{}, ExportConfig{VaultPath: vault, Narratives: &fakeNarrativeReader{rows: rows}})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	getLogs := captureLog(exp)

	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v, want the cycle to complete despite hostile keys", err)
	}

	if files := narrativeFilesUnder(t, vault); len(files) != 0 {
		t.Errorf("hostile narrative keys produced %d file(s) under _narratives/: %v", len(files), files)
	}
	if n := countSubstring(getLogs(), "refusing"); n != len(hostileKeys) {
		t.Errorf("got %d 'refusing' log line(s), want exactly %d (one per hostile key): %v", n, len(hostileKeys), getLogs())
	}
}

// TestNarrativePathNeverEntersStateFiles pins the design's named trap:
// projectFromRelPath reads parts[1], so a narrative path tracked in
// state.Files would resolve to the phantom project "_narratives" and the
// observation deletion pass would try to os.Remove it.
func TestNarrativePathNeverEntersStateFiles(t *testing.T) {
	vault := t.TempDir()
	ts := time.Now().Add(-time.Hour)
	store := &mockStore{
		listProjects: []string{"proj1"},
		byProject: map[string][]*domain.Record{
			"proj1": {newTestRecord(1, "proj1", "architecture", "sess-1", "Observation body", ts)},
		},
	}
	reader := &fakeNarrativeReader{rows: map[string]Narrative{
		"proj1\x00sdd/change": {Body: "Narrative body.", Model: "m", GeneratedAt: time.Now()},
	}}
	exp, err := NewExporter(store, ExportConfig{VaultPath: vault, Narratives: reader})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}

	got, err := ReadState(filepath.Join(vault, engramDir, stateFileName))
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	for id, relPath := range got.Files {
		if strings.Contains(relPath, "_narratives") {
			t.Errorf("state.Files[%d] = %q references the narrative subtree — projectFromRelPath would "+
				"resolve this to the phantom project %q and try to os.Remove it during observation deletion",
				id, relPath, "_narratives")
		}
	}
	if _, ok := got.Narratives["proj1\x00sdd/change"]; !ok {
		t.Error("the narrative was not tracked in state.Narratives at all — test setup or the render pass is broken")
	}
}

// TestStaleNarrativeIsStillRenderedAndFlagged covers REQ-NARR-06: a stale
// narrative renders its last successfully cached text WITH
// narrative_stale: true — staleness is not failure.
func TestStaleNarrativeIsStillRenderedAndFlagged(t *testing.T) {
	vault := t.TempDir()
	reader := &fakeNarrativeReader{rows: map[string]Narrative{
		"proj1\x00sdd/change": {Body: "Stale but still true as of last generation.", Model: "m", GeneratedAt: time.Now(), Stale: true},
	}}
	exp, err := NewExporter(&mockStore{}, ExportConfig{VaultPath: vault, Narratives: reader})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(vault, "engram", "_narratives", "proj1", "sdd-change.md"))
	if err != nil {
		t.Fatalf("expected the stale narrative to still be rendered: %v", err)
	}
	if !strings.Contains(string(content), "Stale but still true as of last generation.") {
		t.Errorf("stale narrative note missing its last successfully cached text:\n%s", content)
	}
	if !strings.Contains(string(content), "narrative_stale: true") {
		t.Errorf("stale narrative note missing narrative_stale: true:\n%s", content)
	}
}

// TestRegeneratedNarrativeClearsTheStaleFlag is the negative counterpart:
// once regenerated, the note carries narrative_stale: false.
func TestRegeneratedNarrativeClearsTheStaleFlag(t *testing.T) {
	vault := t.TempDir()
	reader := &fakeNarrativeReader{rows: map[string]Narrative{
		"proj1\x00sdd/change": {Body: "Freshly regenerated.", Model: "m", GeneratedAt: time.Now(), Stale: false},
	}}
	exp, err := NewExporter(&mockStore{}, ExportConfig{VaultPath: vault, Narratives: reader})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(vault, "engram", "_narratives", "proj1", "sdd-change.md"))
	if err != nil {
		t.Fatalf("expected the narrative to be rendered: %v", err)
	}
	if strings.Contains(string(content), "narrative_stale: true") {
		t.Errorf("regenerated (non-stale) narrative note still carries narrative_stale: true:\n%s", content)
	}
}

// TestExporterComputesStalenessNowhere is a go/parser AST guard: staleness
// is computed EXCLUSIVELY by the generation Loop (design #4754 §6's "sharp
// edge" — computing it needs the hashing code, which internal/obsidian may
// not import) and this package must only ever READ the stored boolean, never
// hash or compare against live records itself.
func TestExporterComputesStalenessNowhere(t *testing.T) {
	fset := token.NewFileSet()
	checked := 0
	for _, path := range goFilesInPackageDir(t) {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		checked++

		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				t.Fatalf("%s: unquote import %s: %v", filepath.Base(path), imp.Path.Value, uerr)
			}
			if p == "crypto/sha256" {
				t.Errorf("%s imports %q — internal/obsidian must never hash source content itself; "+
					"staleness is computed by the generation Loop and read here as a stored boolean (REQ-NARR-06)",
					filepath.Base(path), p)
			}
		}

		// Reading the DESCRIPTIVE SourceHash column (n.SourceHash, rendered
		// verbatim into frontmatter) is exactly the intended, required
		// usage — REQ-PROV-01/-02/-03/-06's render side. What REQ-NARR-06
		// actually forbids is this package COMPARING it against anything
		// (that would be a local re-implementation of the staleness
		// predicate the generation Loop already owns), so the guard here
		// targets ==/!= comparisons involving a .SourceHash selector, not
		// every read of the field.
		ast.Inspect(f, func(n ast.Node) bool {
			bin, ok := n.(*ast.BinaryExpr)
			if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
				return true
			}
			for _, side := range []ast.Expr{bin.X, bin.Y} {
				if sel, ok := side.(*ast.SelectorExpr); ok && sel.Sel.Name == "SourceHash" {
					t.Errorf("%s compares a .SourceHash selector with %s — internal/obsidian must never "+
						"recompute or compare a source hash itself; it only reads the Stale boolean the "+
						"generation Loop already computed and stored", filepath.Base(path), bin.Op)
				}
			}
			return true
		})
	}
	if checked < 10 {
		t.Errorf("only %d files parsed; the package has many more — the guard is not covering the package", checked)
	}
}

// TestFailedUnitWithNoPriorRowRendersNothing covers REQ-NARR-05: a unit with
// no cache row (its most recent generation attempt failed, or it has never
// been generated) contributes NO narrative content — no note, no
// placeholder, no partial fragment.
func TestFailedUnitWithNoPriorRowRendersNothing(t *testing.T) {
	vault := t.TempDir()
	exp, err := NewExporter(&mockStore{}, ExportConfig{VaultPath: vault, Narratives: &fakeNarrativeReader{rows: map[string]Narrative{}}})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	if files := narrativeFilesUnder(t, vault); len(files) != 0 {
		t.Errorf("a unit with no cache row rendered %d file(s): %v", len(files), files)
	}
}

// TestNilNarrativeReaderRendersNothing covers REQ-GEN-04's rendering-side
// counterpart: the feature OFF (nil NarrativeReader) leaves an empty temp
// vault free of _narratives/, and zero log lines mention narrative
// generation.
func TestNilNarrativeReaderRendersNothing(t *testing.T) {
	vault := t.TempDir()
	store := &mockStore{
		listProjects: []string{"proj1"},
		byProject: map[string][]*domain.Record{
			"proj1": {newTestRecord(1, "proj1", "architecture", "sess-1", "Observation body", time.Now().Add(-time.Hour))},
		},
	}
	exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	getLogs := captureLog(exp)

	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(vault, "engram", "_narratives")); !os.IsNotExist(err) {
		t.Errorf("_narratives directory exists with a nil NarrativeReader, stat err = %v", err)
	}
	for _, l := range getLogs() {
		if strings.Contains(strings.ToLower(l), "narrative") {
			t.Errorf("log line mentions narrative generation with the feature OFF: %q", l)
		}
	}
}

// TestStaleNarrativeNoteIsSweptWhenNoLongerRendered covers REQ-NARR-07's
// SyncState amendment: a narrative tracked in a prior cycle but absent from
// the current render is removed from disk AND from state.Narratives.
func TestStaleNarrativeNoteIsSweptWhenNoLongerRendered(t *testing.T) {
	vault := t.TempDir()
	reader := &fakeNarrativeReader{rows: map[string]Narrative{
		"proj1\x00sdd/change": {Body: "First body.", Model: "m", GeneratedAt: time.Now()},
	}}
	exp1, err := NewExporter(&mockStore{}, ExportConfig{VaultPath: vault, Narratives: reader})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp1.Export(); err != nil {
		t.Fatalf("first Export() error = %v", err)
	}
	notePath := filepath.Join(vault, "engram", "_narratives", "proj1", "sdd-change.md")
	if _, err := os.Stat(notePath); err != nil {
		t.Fatalf("expected the narrative note after the first cycle: %v", err)
	}

	reader.rows = map[string]Narrative{} // no longer rendered this cycle
	exp2, err := NewExporter(&mockStore{}, ExportConfig{VaultPath: vault, Narratives: reader})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp2.Export(); err != nil {
		t.Fatalf("second Export() error = %v", err)
	}
	if _, err := os.Stat(notePath); !os.IsNotExist(err) {
		t.Errorf("stale narrative note still exists after it stopped being rendered, stat err = %v", err)
	}

	got, err := ReadState(filepath.Join(vault, engramDir, stateFileName))
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	if _, ok := got.Narratives["proj1\x00sdd/change"]; ok {
		t.Error("state still tracks the swept narrative")
	}
}

// TestNarrativeSweepRunsUnderProjectFilter covers the second dividend of
// decision #4751 decision 3: UNLIKE the hub sweep (disabled entirely under a
// --project filter, because a hub is inherently cross-project), the
// narrative sweep RUNS under a filter and touches only that project's
// narratives — the unit IS (project, topic_prefix), so per-project scoping
// is exact.
func TestNarrativeSweepRunsUnderProjectFilter(t *testing.T) {
	vault := t.TempDir()
	reader := &fakeNarrativeReader{rows: map[string]Narrative{
		"proj1\x00sdd/change": {Body: "Proj1 narrative.", Model: "m", GeneratedAt: time.Now()},
		"proj2\x00sdd/change": {Body: "Proj2 narrative.", Model: "m", GeneratedAt: time.Now()},
	}}
	exp1, err := NewExporter(&mockStore{}, ExportConfig{VaultPath: vault, Narratives: reader})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp1.Export(); err != nil {
		t.Fatalf("first (unfiltered) Export() error = %v", err)
	}
	proj1Path := filepath.Join(vault, "engram", "_narratives", "proj1", "sdd-change.md")
	proj2Path := filepath.Join(vault, "engram", "_narratives", "proj2", "sdd-change.md")
	if _, err := os.Stat(proj1Path); err != nil {
		t.Fatalf("expected proj1 narrative after first export: %v", err)
	}
	if _, err := os.Stat(proj2Path); err != nil {
		t.Fatalf("expected proj2 narrative after first export: %v", err)
	}

	// Both narratives vanish from the reader's point of view. A
	// proj2-scoped run must sweep ONLY proj2's narrative.
	reader.rows = map[string]Narrative{}
	scoped, err := NewExporter(&mockStore{}, ExportConfig{VaultPath: vault, Project: "proj2", Narratives: reader})
	if err != nil {
		t.Fatalf("NewExporter() (scoped) error = %v", err)
	}
	if _, err := scoped.Export(); err != nil {
		t.Fatalf("scoped Export() error = %v", err)
	}

	if _, err := os.Stat(proj1Path); err != nil {
		t.Errorf("proj1 narrative was removed by a proj2-scoped run: %v", err)
	}
	if _, err := os.Stat(proj2Path); !os.IsNotExist(err) {
		t.Errorf("proj2 narrative still exists after a proj2-scoped sweep, stat err = %v", err)
	}

	got, err := ReadState(filepath.Join(vault, engramDir, stateFileName))
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	if _, ok := got.Narratives["proj1\x00sdd/change"]; !ok {
		t.Error("state dropped proj1's narrative entry despite the sweep being scoped to proj2 only")
	}
	if _, ok := got.Narratives["proj2\x00sdd/change"]; ok {
		t.Error("state still tracks proj2's narrative after the scoped sweep")
	}
}

// TestHostileNarrativeStateEntryIsRefusedNotRemoved mirrors the identical
// property already proven for state.Files/state.Hubs entries: a ".."-bearing
// entry read from the untrusted in-vault JSON is skipped, logged, RETAINED
// in state, and never aborts the cycle.
func TestHostileNarrativeStateEntryIsRefusedNotRemoved(t *testing.T) {
	vault := t.TempDir()
	statePath := filepath.Join(vault, engramDir, stateFileName)
	preState := &SyncState{
		Files: map[int64]string{},
		Hubs:  map[string]string{},
		Narratives: map[string]string{
			"proj1\x00sdd/change": "engram/proj1/../../../../victim.md",
		},
	}
	if err := WriteState(statePath, preState); err != nil {
		t.Fatalf("setup: WriteState() error = %v", err)
	}

	exp, err := NewExporter(&mockStore{}, ExportConfig{VaultPath: vault, Narratives: &fakeNarrativeReader{rows: map[string]Narrative{}}})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	getLogs := captureLog(exp)

	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v, want the cycle to complete despite the hostile state entry", err)
	}

	if n := countSubstring(getLogs(), "refusing"); n == 0 {
		t.Errorf("expected a 'refusing' diagnostic for the hostile narrative state entry, got: %v", getLogs())
	}

	got, err := ReadState(statePath)
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	if _, ok := got.Narratives["proj1\x00sdd/change"]; !ok {
		t.Error("hostile narrative state entry was removed rather than retained")
	}
}

// TestNarrativeSweepIsInertOnFirstUpgradeCycle is a GUARD (nothing is
// tracked yet, so "delete nothing" is trivially true), matching Phase A task
// 7.5's precedent: a legacy state file with no "narratives" key at all must
// not crash or misbehave, and the first post-upgrade cycle renders normally.
func TestNarrativeSweepIsInertOnFirstUpgradeCycle(t *testing.T) {
	vault := t.TempDir()
	engramRoot := filepath.Join(vault, engramDir)
	if err := os.MkdirAll(engramRoot, 0755); err != nil {
		t.Fatalf("setup: MkdirAll() error = %v", err)
	}
	legacy := `{"last_export_at":"2020-01-01T00:00:00Z","files":{},"hubs":{}}`
	if err := os.WriteFile(filepath.Join(engramRoot, stateFileName), []byte(legacy), 0644); err != nil {
		t.Fatalf("setup: WriteFile() error = %v", err)
	}

	reader := &fakeNarrativeReader{rows: map[string]Narrative{
		"proj1\x00sdd/change": {Body: "First cycle after upgrade.", Model: "m", GeneratedAt: time.Now()},
	}}
	exp, err := NewExporter(&mockStore{}, ExportConfig{VaultPath: vault, Narratives: reader})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}

	notePath := filepath.Join(vault, "engram", "_narratives", "proj1", "sdd-change.md")
	if _, err := os.Stat(notePath); err != nil {
		t.Errorf("expected the narrative note to be created normally: %v", err)
	}
}

// TestHubBytesAreUnchangedByNarrativePresence is a GUARD at write time
// (hub.go is untouched by Phase B) but a standing bar against a future
// change that would let a hub link back to its narrative — which would make
// every hub's bytes depend on narrative presence.
func TestHubBytesAreUnchangedByNarrativePresence(t *testing.T) {
	ts := time.Now().Add(-time.Hour)
	topic := "sdd/resolution-check/spec"
	newFixture := func() *mockStore {
		rec1 := newTestRecord(1, "proj-a", "architecture", "sess-1", "First topic member", ts)
		rec1.TopicKey = &topic
		rec2 := newTestRecord(2, "proj-a", "bugfix", "sess-2", "Second topic member", ts)
		rec2.TopicKey = &topic
		return &mockStore{
			listProjects: []string{"proj-a"},
			byProject:    map[string][]*domain.Record{"proj-a": {rec1, rec2}},
		}
	}

	withoutVault := t.TempDir()
	expWithout, err := NewExporter(newFixture(), ExportConfig{VaultPath: withoutVault})
	if err != nil {
		t.Fatalf("NewExporter() (without narratives) error = %v", err)
	}
	if _, err := expWithout.Export(); err != nil {
		t.Fatalf("Export() (without narratives) error = %v", err)
	}

	withVault := t.TempDir()
	reader := &fakeNarrativeReader{rows: map[string]Narrative{
		"proj-a\x00sdd/resolution-check": {Body: "A narrative for this exact topic.", Model: "m", GeneratedAt: time.Now(), SourceCount: 2},
	}}
	expWith, err := NewExporter(newFixture(), ExportConfig{VaultPath: withVault, Narratives: reader})
	if err != nil {
		t.Fatalf("NewExporter() (with narratives) error = %v", err)
	}
	if _, err := expWith.Export(); err != nil {
		t.Fatalf("Export() (with narratives) error = %v", err)
	}

	// Sanity: the narrative itself really did render, or the byte
	// comparison below would trivially pass for the wrong reason.
	if files := narrativeFilesUnder(t, withVault); len(files) == 0 {
		t.Fatal("no narrative files rendered; the fixture must produce at least one for this comparison to mean anything")
	}

	hubsWithout := hubFiles(t, withoutVault)
	hubsWith := hubFiles(t, withVault)
	if len(hubsWithout) == 0 || len(hubsWithout) != len(hubsWith) {
		t.Fatalf("hub file count differs: without=%d with=%d (want equal and nonzero)", len(hubsWithout), len(hubsWith))
	}

	for _, hubPath := range hubsWithout {
		rel, err := filepath.Rel(withoutVault, hubPath)
		if err != nil {
			t.Fatalf("Rel() error = %v", err)
		}
		wantContent, err := os.ReadFile(hubPath)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", hubPath, err)
		}
		gotContent, err := os.ReadFile(filepath.Join(withVault, rel))
		if err != nil {
			t.Fatalf("ReadFile counterpart of %s: %v", rel, err)
		}
		if string(wantContent) != string(gotContent) {
			t.Errorf("hub %s bytes differ depending on narrative presence:\nwithout:\n%s\nwith:\n%s", rel, wantContent, gotContent)
		}
	}
}

// TestSecondRunWithNarrativesWritesZeroBytes is one of Phase A's two
// standing regression bars, restated as an explicit regression task: a full
// export run twice over unchanged data WITH narratives present still yields
// a true no-op on the second run, and the narrative note's mtime is
// unchanged.
func TestSecondRunWithNarrativesWritesZeroBytes(t *testing.T) {
	vault := t.TempDir()
	old := time.Now().Add(-720 * time.Hour)
	store := &mockStore{
		listProjects: []string{"proj1"},
		byProject: map[string][]*domain.Record{
			"proj1": {newTestRecord(1, "proj1", "architecture", "sess-1", "Observation body", old)},
		},
	}
	reader := &fakeNarrativeReader{rows: map[string]Narrative{
		"proj1\x00sdd/change": {Body: "Stable narrative body.", Model: "m", GeneratedAt: time.Now(), SourceCount: 3},
	}}

	exp1, err := NewExporter(store, ExportConfig{VaultPath: vault, Narratives: reader})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp1.Export(); err != nil {
		t.Fatalf("first Export() error = %v", err)
	}

	notePath := filepath.Join(vault, "engram", "_narratives", "proj1", "sdd-change.md")
	firstInfo, err := os.Stat(notePath)
	if err != nil {
		t.Fatalf("expected the narrative note after the first cycle: %v", err)
	}

	exp2, err := NewExporter(store, ExportConfig{VaultPath: vault, Narratives: reader})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	second, err := exp2.Export()
	if err != nil {
		t.Fatalf("second Export() error = %v", err)
	}
	if got, want := second.Summary(), "sync: created=0 updated=0 deleted=0 skipped=1 hubs=0"; got != want {
		t.Errorf("second Export().Summary() = %q, want %q — a clean no-op even with narratives present", got, want)
	}

	secondInfo, err := os.Stat(notePath)
	if err != nil {
		t.Fatalf("narrative note vanished after the second cycle: %v", err)
	}
	if !secondInfo.ModTime().Equal(firstInfo.ModTime()) {
		t.Errorf("narrative note mtime changed on an unchanged second run: first=%v second=%v", firstInfo.ModTime(), secondInfo.ModTime())
	}
}

// TestNarrativeCountersAreNotFoldedIntoTheFourCounters is the other genuine
// catch: ExportResult.Narratives moves while Created/Updated/Deleted/Hubs
// stay at 0, in a cycle where ONLY narrative activity changed (the corpus
// was already fully tracked and stable).
func TestNarrativeCountersAreNotFoldedIntoTheFourCounters(t *testing.T) {
	vault := t.TempDir()
	old := time.Now().Add(-720 * time.Hour)
	store := &mockStore{
		listProjects: []string{"proj1"},
		byProject: map[string][]*domain.Record{
			"proj1": {newTestRecord(1, "proj1", "architecture", "sess-1", "Observation body", old)},
		},
	}

	exp1, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp1.Export(); err != nil {
		t.Fatalf("first Export() error = %v", err)
	}

	reader := &fakeNarrativeReader{rows: map[string]Narrative{
		"proj1\x00sdd/change": {Body: "First narrative.", Model: "m", GeneratedAt: time.Now(), SourceCount: 1},
	}}
	exp2, err := NewExporter(store, ExportConfig{VaultPath: vault, Narratives: reader})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	result, err := exp2.Export()
	if err != nil {
		t.Fatalf("second Export() error = %v", err)
	}

	if result.Created != 0 || result.Updated != 0 || result.Deleted != 0 || result.Hubs != 0 {
		t.Errorf("structural counters moved when only narrative activity changed: Created=%d Updated=%d Deleted=%d Hubs=%d",
			result.Created, result.Updated, result.Deleted, result.Hubs)
	}
	if result.Narratives == 0 {
		t.Error("Narratives counter did not move despite a brand-new narrative being rendered this cycle")
	}
}

// TestEveryHubWikilinkStillResolves is the third of Phase A's two standing
// bars restated together: over a vault rendered WITH narratives, every
// hub-emitted wikilink target still resolves to a real note and none
// contains a backslash — mirrors wikilink_resolution_test.go's
// TestHubWikilinksResolveToExistingFiles / TestHubWikilinksUseForwardSlashes
// exactly, with a NarrativeReader now attached.
func TestEveryHubWikilinkStillResolves(t *testing.T) {
	vault := t.TempDir()
	store := buildLinkResolutionFixture()
	reader := &fakeNarrativeReader{rows: map[string]Narrative{
		"proj-a\x00sdd/resolution-check": {Body: "A narrative.", Model: "m", GeneratedAt: time.Now(), SourceCount: 2},
	}}
	exp, err := NewExporter(store, ExportConfig{VaultPath: vault, Narratives: reader})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}

	if files := narrativeFilesUnder(t, vault); len(files) == 0 {
		t.Fatal("no narrative files rendered; the fixture must produce at least one for this test to cover anything")
	}

	hubs := hubFiles(t, vault)
	if len(hubs) == 0 {
		t.Fatal("no hub files were written; the fixture must produce at least one for this test to cover anything")
	}

	var checked int
	var missing []string
	var backslashes []string
	for _, hub := range hubs {
		content, err := os.ReadFile(hub)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", hub, err)
		}
		for _, target := range extractWikilinkTargets(string(content)) {
			checked++
			if strings.Contains(target, `\`) {
				backslashes = append(backslashes, hub+" -> "+target)
			}
			if resolved, ok := resolveObsidianWikilink(vault, target); !ok {
				missing = append(missing, fmt.Sprintf("%q -> %q", target, resolved))
			}
		}
	}
	if checked == 0 {
		t.Fatal("no wikilinks found in any hub file; the fixture must produce at least one for this test to cover anything")
	}
	if len(missing) != 0 {
		t.Errorf("%d of %d rendered wikilink targets do not resolve to an existing file (narratives present):\n%s",
			len(missing), checked, strings.Join(missing, "\n"))
	}
	if len(backslashes) != 0 {
		t.Errorf("%d of %d rendered wikilinks contain a backslash (narratives present):\n%s",
			len(backslashes), checked, strings.Join(backslashes, "\n"))
	}
}
