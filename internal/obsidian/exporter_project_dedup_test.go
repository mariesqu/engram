package obsidian

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// TestProjectsToExportDedupesListProjects covers the real-data smoke-test
// finding recorded in bugfix/listprojects-case-variant-double-read (#4713):
// Store.ListProjects() (internal/localstore/sync.go:413) does a
// case-SENSITIVE "SELECT DISTINCT project", so project-name drift ("gitlab"
// and "GitLab" both present in the store) makes it return both spellings as
// separate entries. projectsToExport() must collapse them into ONE project
// before the caller ever loops over them and calls RecentObservations, which
// matches case-insensitively (LOWER(project) — internal/localstore/search.go)
// and would otherwise hand back the SAME underlying rows for both spellings.
func TestProjectsToExportDedupesListProjects(t *testing.T) {
	store := &mockStore{listProjects: []string{"gitlab", "GitLab", "engram"}}
	exp, err := NewExporter(store, ExportConfig{VaultPath: t.TempDir()})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	projects, err := exp.projectsToExport()
	if err != nil {
		t.Fatalf("projectsToExport() error = %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("projectsToExport() = %v, want 2 entries (gitlab/GitLab collapsed into one)", projects)
	}
}

// TestExportVisitsCaseVariantProjectRecordsExactlyOnce is the end-to-end
// regression test for #4713's real-data symptom: on a first export into an
// empty vault, EVERY observation must be Created exactly once and Skipped
// must be zero. Before the fix, the exporter looped both "gitlab" and
// "GitLab" (ListProjects' raw, undeduped output), and RecentObservations
// resolves both spellings to the SAME underlying rows (case-insensitive
// match), so each record was visited twice within the same cycle: written
// once (Created) and revisited a second time with identical content already
// on disk (Skipped). A nonzero Skipped count on a first run into an empty
// vault is exactly that smoking gun.
func TestExportVisitsCaseVariantProjectRecordsExactlyOnce(t *testing.T) {
	vault := t.TempDir()
	ts := time.Now().Add(-time.Hour)

	store := &mockStore{
		listProjects: []string{"gitlab", "GitLab"},
		byProject: map[string][]*domain.Record{
			"gitlab": {
				newTestRecord(1, "gitlab", "decision", "sess-1", "First gitlab body", ts),
			},
			"GitLab": {
				newTestRecord(2, "GitLab", "decision", "sess-1", "Second gitlab body", ts),
			},
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
		t.Errorf("Created = %d, want 2 (one per distinct observation)", result.Created)
	}
	if result.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0 -- a nonzero value on a first export into an empty vault means a record was visited more than once (bugfix/listprojects-case-variant-double-read)", result.Skipped)
	}

	noteFiles := 0
	if err := filepath.Walk(filepath.Join(vault, "engram"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".md") &&
			!strings.Contains(filepath.ToSlash(path), "/_sessions/") &&
			!strings.Contains(filepath.ToSlash(path), "/_topics/") {
			noteFiles++
		}
		return nil
	}); err != nil {
		t.Fatalf("walk vault: %v", err)
	}
	if noteFiles != 2 {
		t.Errorf("note files on disk = %d, want 2 (one per distinct observation, not one per project-name variant)", noteFiles)
	}
}

// TestProjectFilterDeletionScopeIsCaseInsensitive covers a corollary of the
// same bug in the deletion pass. The deletion pass scopes itself to the
// projects processed THIS run via a "processed" set built from the resolved
// project list; a state entry's project segment is compared against that
// set before its file is eligible for removal. That segment is derived from
// the RECORD's own Project field (via vaultRelPath), which can carry
// different casing than whatever the --project filter string happened to
// be -- RecentObservations matches case-insensitively and can hand back
// records whose stored casing differs from the queried filter. A
// case-sensitive comparison here made the deletion pass silently refuse to
// ever clean up that project's files whenever the two happened to differ in
// case.
func TestProjectFilterDeletionScopeIsCaseInsensitive(t *testing.T) {
	vault := t.TempDir()
	ts := time.Now().Add(-time.Hour)

	store := &mockStore{
		listProjects: []string{"engram"},
		byProject: map[string][]*domain.Record{
			"engram": {
				newTestRecord(1, "engram", "decision", "sess-1", "Keep me", ts),
				newTestRecord(2, "engram", "decision", "sess-1", "Delete me", ts),
			},
		},
	}

	// First run is scoped with a DIFFERENT case ("Engram") than the records'
	// own stored Project field ("engram", lowercase) to establish state.
	exp, err := NewExporter(store, ExportConfig{VaultPath: vault, Project: "Engram"})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("first Export() error = %v", err)
	}

	before := countFiles(t, filepath.Join(vault, "engram", "engram"))
	if before != 2 {
		t.Fatalf("expected 2 observation files after first export, got %d", before)
	}

	// Observation 2 is soft-deleted: it no longer appears in the live listing.
	store.byProject["engram"] = []*domain.Record{
		newTestRecord(1, "engram", "decision", "sess-1", "Keep me", ts),
	}

	// Second run is scoped with YET ANOTHER case ("ENGRAM"). A
	// case-sensitive scope guard would never recognize this run as covering
	// the file's actual project segment ("engram", lowercase) and would
	// leave the deleted observation's note orphaned on disk forever.
	scoped, err := NewExporter(store, ExportConfig{VaultPath: vault, Project: "ENGRAM"})
	if err != nil {
		t.Fatalf("NewExporter() (scoped) error = %v", err)
	}
	result, err := scoped.Export()
	if err != nil {
		t.Fatalf("scoped Export() error = %v", err)
	}
	if result.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1 -- case-insensitive project scoping must still recognize this run covers the deleted file's project", result.Deleted)
	}

	after := countFiles(t, filepath.Join(vault, "engram", "engram"))
	if after != 1 {
		t.Errorf("expected 1 observation file remaining after deletion, got %d", after)
	}
}

// TestProjectsToExportDropsEmptyStringEntry covers a SECOND, independent
// finding from the real-data smoke test that verified the fix above: on the
// live store, ListProjects() returns an empty-string ("") entry alongside 92
// real project names (a UNION over memories/memory_tombstones/user_prompts/
// prompt_tombstones — at least one row across those tables carries
// project = "", legacy or never-set data). RecentObservations treats an
// empty project string as "no filter at all" (internal/localstore/search.go:
// the WHERE clause is only added "if project != \"\""), so looping that
// entry as if it were a normal per-project scope calls
// RecentObservations("", ...) and gets back EVERY record in the entire
// store -- on the live data this dominated the double-read symptom far more
// than the case-variant drift the bug was originally filed against: the
// blank entry alone returned as many rows as every other project combined
// (~4715 of ~9505 total rows summed across the raw project list).
func TestProjectsToExportDropsEmptyStringEntry(t *testing.T) {
	store := &mockStore{listProjects: []string{"", "proj1", "proj2"}}
	exp, err := NewExporter(store, ExportConfig{VaultPath: t.TempDir()})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	projects, err := exp.projectsToExport()
	if err != nil {
		t.Fatalf("projectsToExport() error = %v", err)
	}
	for _, p := range projects {
		if strings.TrimSpace(p) == "" {
			t.Fatalf("projectsToExport() = %v, included an empty-string entry", projects)
		}
	}
	if len(projects) != 2 {
		t.Errorf("projectsToExport() = %v, want exactly [proj1 proj2]", projects)
	}
}

// TestExportIgnoresEmptyStringProjectEntry is the end-to-end regression test
// for the empty-string finding: an empty-string entry anywhere in
// ListProjects()' output must never be looped as a per-project scope, since
// RecentObservations("", ...) matches EVERY record rather than none.
func TestExportIgnoresEmptyStringProjectEntry(t *testing.T) {
	vault := t.TempDir()
	ts := time.Now().Add(-time.Hour)

	store := &mockStore{
		listProjects: []string{"", "proj1", "proj2"},
		byProject: map[string][]*domain.Record{
			"proj1": {newTestRecord(1, "proj1", "decision", "sess-1", "Proj1 body", ts)},
			"proj2": {newTestRecord(2, "proj2", "decision", "sess-2", "Proj2 body", ts)},
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
		t.Errorf("Created = %d, want 2 (one per distinct observation)", result.Created)
	}
	if result.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0 -- an empty-string project entry must never be queried as if it were a real per-project scope", result.Skipped)
	}
}

// TestDedupeProjectsCaseInsensitive covers the pure dedup helper directly.
// This is triangulation on an already-fixed function (the behavioral RED was
// observed via TestProjectsToExportDedupesListProjects and
// TestExportVisitsCaseVariantProjectRecordsExactlyOnce above, against the
// method that calls it) — labelled honestly as a coverage addition, not a
// second RED/GREEN cycle.
func TestDedupeProjectsCaseInsensitive(t *testing.T) {
	t.Run("case-variant names collapse to one entry, tiebroken by sorting the variants", func(t *testing.T) {
		got := dedupeProjectsCaseInsensitive([]string{"gitlab", "GitLab", "engram"})
		// "GitLab" sorts before "gitlab" byte-wise (ASCII 'G'=0x47 < 'g'=0x67),
		// so it is the documented deterministic tiebreak winner.
		want := []string{"GitLab", "engram"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("dedupeProjectsCaseInsensitive() = %v, want %v", got, want)
		}
	})

	t.Run("no drift is a no-op", func(t *testing.T) {
		got := dedupeProjectsCaseInsensitive([]string{"proj1", "proj2"})
		want := []string{"proj1", "proj2"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("dedupeProjectsCaseInsensitive() = %v, want %v", got, want)
		}
	})

	t.Run("the tiebreak winner is deterministic regardless of input order", func(t *testing.T) {
		got1 := dedupeProjectsCaseInsensitive([]string{"gitlab", "GitLab"})
		got2 := dedupeProjectsCaseInsensitive([]string{"GitLab", "gitlab"})
		if !reflect.DeepEqual(got1, got2) {
			t.Errorf("dedupeProjectsCaseInsensitive() is order-dependent: %v vs %v", got1, got2)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		got := dedupeProjectsCaseInsensitive(nil)
		if len(got) != 0 {
			t.Errorf("dedupeProjectsCaseInsensitive(nil) = %v, want empty", got)
		}
	})
}
