package obsidian

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// TestNewExporter covers construction validation: a missing vault path is a
// configuration error, and a store satisfying the narrow StoreReader
// interface (REQ-EXPORT-10) is all NewExporter needs — no mocking framework,
// just a hand-rolled fake (mockStore, testhelpers_test.go).
func TestNewExporter(t *testing.T) {
	t.Run("missing vault path is an error", func(t *testing.T) {
		store := &mockStore{}
		_, err := NewExporter(store, ExportConfig{})
		if err == nil {
			t.Fatal("NewExporter() with empty VaultPath error = nil, want an error")
		}
	})

	t.Run("a valid vault path constructs an exporter", func(t *testing.T) {
		store := &mockStore{}
		exp, err := NewExporter(store, ExportConfig{VaultPath: t.TempDir()})
		if err != nil {
			t.Fatalf("NewExporter() error = %v, want nil", err)
		}
		if exp == nil {
			t.Fatal("NewExporter() returned nil exporter with nil error")
		}
	})

	t.Run("mockStore satisfies StoreReader", func(t *testing.T) {
		var _ StoreReader = &mockStore{}
	})
}

// TestExportConfig exercises the config fields NewExporter validates.
func TestExportConfig(t *testing.T) {
	t.Run("empty vault path is rejected regardless of other fields", func(t *testing.T) {
		store := &mockStore{}
		_, err := NewExporter(store, ExportConfig{Project: "engram", Limit: 10})
		if err == nil {
			t.Fatal("NewExporter() with empty VaultPath error = nil, want an error")
		}
	})
}

// countFiles counts regular files under root (recursively), for a coarse
// vault-structure assertion without hand-walking every expected path.
func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return 0
	}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("countFiles(%s): %v", root, err)
	}
	return n
}

// TestIncrementalExport covers REQ-EXPORT-06: the state file gates which
// observations get written on a subsequent run — only observations whose
// UpdatedAt (falling back to CreatedAt) is after the cutoff are (re)written;
// everything else is counted Skipped with zero disk I/O.
func TestIncrementalExport(t *testing.T) {
	vault := t.TempDir()
	old := time.Now().Add(-24 * time.Hour)

	store := &mockStore{
		listProjects: []string{"proj1"},
		byProject: map[string][]*domain.Record{
			"proj1": {
				newTestRecord(1, "proj1", "architecture", "sess-1", "First observation body", old),
				newTestRecord(2, "proj1", "bugfix", "sess-1", "Second observation body", old),
			},
		},
	}

	exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}

	first, err := exp.Export()
	if err != nil {
		t.Fatalf("first Export() error = %v", err)
	}
	if first.Created != 2 {
		t.Errorf("first Export(): Created = %d, want 2", first.Created)
	}
	if first.Skipped != 0 {
		t.Errorf("first Export(): Skipped = %d, want 0", first.Skipped)
	}

	// Add a third, NEWER observation while leaving the first two untouched
	// (same UpdatedAt). Only the new one should be written on the second run.
	newer := time.Now().Add(1 * time.Hour)
	store.byProject["proj1"] = append(store.byProject["proj1"],
		newTestRecord(3, "proj1", "decision", "sess-1", "Third observation body", newer))

	second, err := exp.Export()
	if err != nil {
		t.Fatalf("second Export() error = %v", err)
	}
	if second.Created != 1 {
		t.Errorf("second Export(): Created = %d, want 1 (only the new observation)", second.Created)
	}
	if second.Skipped != 2 {
		t.Errorf("second Export(): Skipped = %d, want 2 (unchanged observations)", second.Skipped)
	}
	if second.Updated != 0 {
		t.Errorf("second Export(): Updated = %d, want 0", second.Updated)
	}
}

// TestDeletedObsRemoved covers REQ-EXPORT-09: an observation present in a
// prior run's state but absent from the current live listing (soft-deleted,
// so RecentObservations no longer returns it) has its vault file removed and
// is dropped from state. The diff MUST be scoped to the projects processed
// this run — a filtered run must never delete another project's files.
func TestDeletedObsRemoved(t *testing.T) {
	t.Run("an observation missing from the live listing is deleted from disk and state", func(t *testing.T) {
		vault := t.TempDir()
		ts := time.Now().Add(-1 * time.Hour)

		store := &mockStore{
			listProjects: []string{"proj1"},
			byProject: map[string][]*domain.Record{
				"proj1": {
					newTestRecord(1, "proj1", "architecture", "sess-1", "Keep me", ts),
					newTestRecord(2, "proj1", "bugfix", "sess-1", "Delete me", ts),
				},
			},
		}
		exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
		if err != nil {
			t.Fatalf("NewExporter() error = %v", err)
		}
		if _, err := exp.Export(); err != nil {
			t.Fatalf("first Export() error = %v", err)
		}

		before := countFiles(t, filepath.Join(vault, "engram", "proj1"))
		if before != 2 {
			t.Fatalf("expected 2 observation files after first export, got %d", before)
		}

		// Simulate observation id=2 being soft-deleted: it no longer appears
		// in RecentObservations' live listing.
		store.byProject["proj1"] = []*domain.Record{
			newTestRecord(1, "proj1", "architecture", "sess-1", "Keep me", ts),
		}

		result, err := exp.Export()
		if err != nil {
			t.Fatalf("second Export() error = %v", err)
		}
		if result.Deleted != 1 {
			t.Errorf("Deleted = %d, want 1", result.Deleted)
		}

		after := countFiles(t, filepath.Join(vault, "engram", "proj1"))
		if after != 1 {
			t.Errorf("expected 1 observation file remaining after deletion, got %d", after)
		}
	})

	t.Run("a --project scoped run never deletes another project's files", func(t *testing.T) {
		vault := t.TempDir()
		ts := time.Now().Add(-1 * time.Hour)

		store := &mockStore{
			listProjects: []string{"proj1", "proj2"},
			byProject: map[string][]*domain.Record{
				"proj1": {newTestRecord(1, "proj1", "architecture", "sess-1", "Proj1 obs", ts)},
				"proj2": {newTestRecord(2, "proj2", "architecture", "sess-2", "Proj2 obs", ts)},
			},
		}

		// First run: export everything (no project filter) so state tracks both.
		exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
		if err != nil {
			t.Fatalf("NewExporter() error = %v", err)
		}
		if _, err := exp.Export(); err != nil {
			t.Fatalf("first Export() error = %v", err)
		}

		// Second run: scope to proj1 only, using a NEW exporter instance
		// (mirrors a fresh CLI invocation reading the same state file).
		scoped, err := NewExporter(store, ExportConfig{VaultPath: vault, Project: "proj1"})
		if err != nil {
			t.Fatalf("NewExporter() (scoped) error = %v", err)
		}
		result, err := scoped.Export()
		if err != nil {
			t.Fatalf("scoped Export() error = %v", err)
		}
		if result.Deleted != 0 {
			t.Errorf("scoped Export(): Deleted = %d, want 0 (proj2 must be left alone)", result.Deleted)
		}

		proj2Count := countFiles(t, filepath.Join(vault, "engram", "proj2"))
		if proj2Count != 1 {
			t.Errorf("proj2 file count = %d, want 1 (untouched by the proj1-scoped run)", proj2Count)
		}
	})
}

// TestProjectFilter covers REQ-EXPORT-01/-09: --project limits which
// projects are exported; the absence of a filter exports every known
// project (triangulated against ListProjects()).
func TestProjectFilter(t *testing.T) {
	ts := time.Now().Add(-1 * time.Hour)

	t.Run("a project filter exports only that project", func(t *testing.T) {
		vault := t.TempDir()
		store := &mockStore{
			listProjects: []string{"proj1", "proj2"},
			byProject: map[string][]*domain.Record{
				"proj1": {newTestRecord(1, "proj1", "architecture", "sess-1", "Proj1 obs", ts)},
				"proj2": {newTestRecord(2, "proj2", "architecture", "sess-2", "Proj2 obs", ts)},
			},
		}
		exp, err := NewExporter(store, ExportConfig{VaultPath: vault, Project: "proj1"})
		if err != nil {
			t.Fatalf("NewExporter() error = %v", err)
		}
		result, err := exp.Export()
		if err != nil {
			t.Fatalf("Export() error = %v", err)
		}
		if result.Created != 1 {
			t.Errorf("Created = %d, want 1", result.Created)
		}
		if _, err := os.Stat(filepath.Join(vault, "engram", "proj2")); !os.IsNotExist(err) {
			t.Errorf("proj2 directory should not exist when --project=proj1, stat err = %v", err)
		}
	})

	t.Run("no project filter exports every project", func(t *testing.T) {
		vault := t.TempDir()
		store := &mockStore{
			listProjects: []string{"proj1", "proj2"},
			byProject: map[string][]*domain.Record{
				"proj1": {newTestRecord(1, "proj1", "architecture", "sess-1", "Proj1 obs", ts)},
				"proj2": {newTestRecord(2, "proj2", "architecture", "sess-2", "Proj2 obs", ts)},
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
		if result.Created != 2 {
			t.Errorf("Created = %d, want 2 (both projects)", result.Created)
		}
	})
}

// TestFullExportPipeline is the Phase 2 integration test: 3 projects, 5
// sessions, 20 observations (one of which is later removed from the live
// listing to simulate a soft-delete), asserting directory structure, state
// file persistence, and an idempotent/incremental second run.
func TestFullExportPipeline(t *testing.T) {
	vault := t.TempDir()
	ts := time.Now().Add(-1 * time.Hour)

	byProject := map[string][]*domain.Record{
		"proj-a": {},
		"proj-b": {},
		"proj-c": {},
	}
	sessions := []string{"sess-1", "sess-2", "sess-3", "sess-4", "sess-5"}
	projects := []string{"proj-a", "proj-b", "proj-c"}
	types := []string{"architecture", "bugfix", "decision", "pattern", "discovery"}

	var id int64
	for i := 0; i < 20; i++ {
		id++
		proj := projects[i%len(projects)]
		sess := sessions[i%len(sessions)]
		typ := types[i%len(types)]
		rec := newTestRecord(id, proj, typ, sess, fmt.Sprintf("Observation body number %d", id), ts)
		if id == 7 {
			topic := "sdd/full-pipeline/spec"
			rec.TopicKey = &topic
		}
		if id == 8 {
			topic := "sdd/full-pipeline/design"
			rec.TopicKey = &topic
		}
		byProject[proj] = append(byProject[proj], rec)
	}

	store := &mockStore{
		listProjects: projects,
		byProject:    byProject,
	}

	exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}

	first, err := exp.Export()
	if err != nil {
		t.Fatalf("first Export() error = %v", err)
	}
	if first.Created != 20 {
		t.Errorf("first Export(): Created = %d, want 20", first.Created)
	}
	// Hubs: 5 session hubs (unconditional) + 1 topic hub (obs 7 and 8 share
	// prefix "sdd/full-pipeline") = 6.
	if first.Hubs != 6 {
		t.Errorf("first Export(): Hubs = %d, want 6", first.Hubs)
	}

	for _, proj := range projects {
		if _, err := os.Stat(filepath.Join(vault, "engram", proj)); err != nil {
			t.Errorf("expected project dir for %q: %v", proj, err)
		}
	}
	if _, err := os.Stat(filepath.Join(vault, "engram", "_sessions")); err != nil {
		t.Errorf("expected _sessions dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(vault, "engram", "_topics")); err != nil {
		t.Errorf("expected _topics dir: %v", err)
	}
	statePath := filepath.Join(vault, "engram", ".engram-sync-state.json")
	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("expected state file: %v", err)
	}

	st, err := ReadState(statePath)
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	if len(st.Files) != 20 {
		t.Errorf("state Files count = %d, want 20", len(st.Files))
	}

	// Second run: remove one observation (id=20) from the live listing to
	// simulate a deletion, and re-export with everything else unchanged.
	for proj, recs := range byProject {
		var kept []*domain.Record
		for _, r := range recs {
			if r.ID == 20 {
				continue
			}
			kept = append(kept, r)
		}
		byProject[proj] = kept
	}
	store.byProject = byProject

	second, err := exp.Export()
	if err != nil {
		t.Fatalf("second Export() error = %v", err)
	}
	if second.Created != 0 {
		t.Errorf("second Export(): Created = %d, want 0", second.Created)
	}
	if second.Updated != 0 {
		t.Errorf("second Export(): Updated = %d, want 0", second.Updated)
	}
	if second.Deleted != 1 {
		t.Errorf("second Export(): Deleted = %d, want 1", second.Deleted)
	}
	if second.Skipped != 19 {
		t.Errorf("second Export(): Skipped = %d, want 19", second.Skipped)
	}

	st2, err := ReadState(statePath)
	if err != nil {
		t.Fatalf("ReadState() (second) error = %v", err)
	}
	if len(st2.Files) != 19 {
		t.Errorf("state Files count after deletion = %d, want 19", len(st2.Files))
	}
}

// TestExportWritesGraphConfigWhenModeIsPreserve covers REQ-GRAPH-02 wired
// into a full Export() cycle: preserve mode writes the embedded template
// when {vault}/.obsidian/graph.json is absent.
func TestExportWritesGraphConfigWhenModeIsPreserve(t *testing.T) {
	vault := t.TempDir()
	store := &mockStore{}
	exp, err := NewExporter(store, ExportConfig{VaultPath: vault, GraphConfig: GraphConfigPreserve})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(vault, ".obsidian", "graph.json"))
	if err != nil {
		t.Fatalf("expected .obsidian/graph.json to exist: %v", err)
	}
	if string(got) != string(defaultGraphTemplate) {
		t.Error("Export() with GraphConfigPreserve did not write the embedded template verbatim")
	}
}

// TestExportOverwritesGraphConfigWhenModeIsForce covers REQ-GRAPH-03 wired
// into a full Export() cycle: force mode overwrites an existing
// .obsidian/graph.json unconditionally.
func TestExportOverwritesGraphConfigWhenModeIsForce(t *testing.T) {
	vault := t.TempDir()
	dir := filepath.Join(vault, ".obsidian")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("setup: MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "graph.json"), []byte(`{"stale":true}`), 0644); err != nil {
		t.Fatalf("setup: WriteFile() error = %v", err)
	}

	store := &mockStore{}
	exp, err := NewExporter(store, ExportConfig{VaultPath: vault, GraphConfig: GraphConfigForce})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "graph.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != string(defaultGraphTemplate) {
		t.Error("Export() with GraphConfigForce did not overwrite the stale graph.json")
	}
}

// TestExportNeverTouchesDotObsidianWhenModeIsSkip covers REQ-GRAPH-04 wired
// into a full Export() cycle: skip mode must not create .obsidian/ at all.
func TestExportNeverTouchesDotObsidianWhenModeIsSkip(t *testing.T) {
	vault := t.TempDir()
	store := &mockStore{}
	exp, err := NewExporter(store, ExportConfig{VaultPath: vault, GraphConfig: GraphConfigSkip})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(vault, ".obsidian")); !os.IsNotExist(err) {
		t.Errorf(".obsidian directory exists after a GraphConfigSkip export, stat err = %v", err)
	}
}

// TestExportDefaultsToSkipWhenGraphConfigUnset covers the design decision
// "GraphConfig empty-string default": an ExportConfig that never mentions
// graph config (the GraphConfigMode zero value, "") must be INERT — Export()
// must not write .obsidian/graph.json just because a caller never set the
// field.
func TestExportDefaultsToSkipWhenGraphConfigUnset(t *testing.T) {
	vault := t.TempDir()
	store := &mockStore{}
	exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("Export() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(vault, ".obsidian")); !os.IsNotExist(err) {
		t.Errorf(".obsidian directory exists after an export with GraphConfig unset, stat err = %v", err)
	}
}
