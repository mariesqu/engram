package obsidian

import (
	"path/filepath"
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
