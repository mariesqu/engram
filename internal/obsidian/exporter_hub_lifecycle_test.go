package obsidian

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// TestStaleSessionHubIsDeleted covers REQ-EXPORT-09's NEW clause (closes
// deferred MAJOR-4): a session whose every observation has gone (soft
// deleted, or otherwise no longer live) must leave NO hub file and NO
// tracked state entry — not a hub full of dead links forever.
func TestStaleSessionHubIsDeleted(t *testing.T) {
	vault := t.TempDir()
	ts := time.Now().Add(-1 * time.Hour)

	store := &mockStore{
		listProjects: []string{"proj1"},
		byProject: map[string][]*domain.Record{
			"proj1": {
				newTestRecord(1, "proj1", "architecture", "sess-1", "Only observation in this session", ts),
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

	hubPath := filepath.Join(vault, "engram", "_sessions", "sess-1.md")
	if _, err := os.Stat(hubPath); err != nil {
		t.Fatalf("expected session hub after first export: %v", err)
	}

	// Simulate the observation vanishing from the live listing entirely:
	// the session has no live members left at all.
	store.byProject["proj1"] = nil

	result, err := exp.Export()
	if err != nil {
		t.Fatalf("second Export() error = %v", err)
	}
	if _, err := os.Stat(hubPath); !os.IsNotExist(err) {
		t.Errorf("stale session hub still exists after its last observation vanished, stat err = %v", err)
	}

	statePath := filepath.Join(vault, "engram", ".engram-sync-state.json")
	st, err := ReadState(statePath)
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	if _, ok := st.Hubs["_sessions/sess-1"]; ok {
		t.Error("state still tracks the stale session hub after it should have been swept")
	}
	if result.Deleted == 0 {
		t.Error("Deleted counter did not account for the removed session hub (or its observation)")
	}
}

// TestTopicHubDroppingBelowThresholdIsDeleted covers the same NEW clause for
// topic hubs: a prefix falling from 2 refs to 1 (ShouldCreateTopicHub's
// threshold) must remove the existing hub rather than leaving a stale
// 2-entry hub whose second member no longer exists.
func TestTopicHubDroppingBelowThresholdIsDeleted(t *testing.T) {
	vault := t.TempDir()
	ts := time.Now().Add(-1 * time.Hour)
	topic := "sdd/some-change/spec"

	rec1 := newTestRecord(1, "proj1", "architecture", "sess-1", "First topic member", ts)
	rec1.TopicKey = &topic
	rec2 := newTestRecord(2, "proj1", "bugfix", "sess-2", "Second topic member", ts)
	rec2.TopicKey = &topic

	store := &mockStore{
		listProjects: []string{"proj1"},
		byProject: map[string][]*domain.Record{
			"proj1": {rec1, rec2},
		},
	}
	exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("first Export() error = %v", err)
	}

	prefix, ok := TopicPrefix(topic)
	if !ok {
		t.Fatalf("TopicPrefix(%q) ok = false, want true", topic)
	}
	hubPath := filepath.Join(vault, "engram", "_topics", safeFilename(prefix)+".md")
	if _, err := os.Stat(hubPath); err != nil {
		t.Fatalf("expected topic hub after first export: %v", err)
	}

	// Drop to 1 remaining member: below ShouldCreateTopicHub's threshold.
	store.byProject["proj1"] = []*domain.Record{rec1}

	if _, err := exp.Export(); err != nil {
		t.Fatalf("second Export() error = %v", err)
	}
	if _, err := os.Stat(hubPath); !os.IsNotExist(err) {
		t.Errorf("topic hub still exists after dropping below the 2-ref threshold, stat err = %v", err)
	}
}

// TestHubSweepDoesNotRunUnderProjectFilter covers the MAJOR-3 mitigation: a
// hub is inherently cross-project, so a --project-scoped run must NEVER
// delete a hub belonging to another project, even if that hub would look
// "stale" from the filtered run's necessarily-truncated point of view.
func TestHubSweepDoesNotRunUnderProjectFilter(t *testing.T) {
	vault := t.TempDir()
	ts := time.Now().Add(-1 * time.Hour)

	store := &mockStore{
		listProjects: []string{"proj1", "proj2"},
		byProject: map[string][]*domain.Record{
			"proj1": {newTestRecord(1, "proj1", "architecture", "sess-1", "Proj1 obs", ts)},
			"proj2": {newTestRecord(2, "proj2", "architecture", "sess-2", "Proj2 obs", ts)},
		},
	}

	// First run: unfiltered, so both session hubs get rendered and tracked.
	exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("first Export() error = %v", err)
	}

	sess1Hub := filepath.Join(vault, "engram", "_sessions", "sess-1.md")
	if _, err := os.Stat(sess1Hub); err != nil {
		t.Fatalf("expected sess-1 hub after first export: %v", err)
	}

	// proj1's only observation vanishes from the live listing entirely —
	// from an UNFILTERED run's point of view sess-1's hub is now stale.
	// But the second run below is scoped to proj2, using a NEW exporter
	// instance (mirrors a fresh CLI/daemon invocation reading the same
	// state file). A project-scoped run has no visibility into proj1 at
	// all, so it must not decide sess-1's hub is stale.
	store.byProject["proj1"] = nil
	scoped, err := NewExporter(store, ExportConfig{VaultPath: vault, Project: "proj2"})
	if err != nil {
		t.Fatalf("NewExporter() (scoped) error = %v", err)
	}
	if _, err := scoped.Export(); err != nil {
		t.Fatalf("scoped Export() error = %v", err)
	}

	if _, err := os.Stat(sess1Hub); err != nil {
		t.Errorf("sess-1 hub was removed by a proj2-scoped run: %v", err)
	}

	statePath := filepath.Join(vault, "engram", ".engram-sync-state.json")
	st, err := ReadState(statePath)
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	if _, ok := st.Hubs["_sessions/sess-1"]; !ok {
		t.Error("state dropped the sess-1 hub entry despite the sweep being disabled under a project filter")
	}
}

// TestHubSweepIsInertOnFirstUpgradeCycle covers the backward-compatible
// upgrade path: a state file written before Hubs existed has no "hubs" key.
// The very first cycle after upgrading must not delete anything — there is
// nothing tracked yet to compare this cycle's rendered hubs against.
func TestHubSweepIsInertOnFirstUpgradeCycle(t *testing.T) {
	vault := t.TempDir()
	engramRoot := filepath.Join(vault, "engram")
	if err := os.MkdirAll(engramRoot, 0755); err != nil {
		t.Fatalf("setup: MkdirAll() error = %v", err)
	}
	// A legacy state file predating the Hubs field: only "files", no "hubs" key.
	legacy := `{"last_export_at":"2020-01-01T00:00:00Z","files":{}}`
	statePath := filepath.Join(engramRoot, ".engram-sync-state.json")
	if err := os.WriteFile(statePath, []byte(legacy), 0644); err != nil {
		t.Fatalf("setup: WriteFile() error = %v", err)
	}

	ts := time.Now().Add(-1 * time.Hour)
	store := &mockStore{
		listProjects: []string{"proj1"},
		byProject: map[string][]*domain.Record{
			"proj1": {newTestRecord(1, "proj1", "architecture", "sess-1", "First cycle after upgrade", ts)},
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
	if result.Deleted != 0 {
		t.Errorf("Deleted = %d on the first post-upgrade cycle, want 0 (nothing tracked to compare against)", result.Deleted)
	}

	hubPath := filepath.Join(engramRoot, "_sessions", "sess-1.md")
	if _, err := os.Stat(hubPath); err != nil {
		t.Errorf("expected the session hub to be created normally: %v", err)
	}
}

// TestProjectFilteredExportWarnsAboutHubTruncation covers the MAJOR-3
// mitigation: a --project-scoped run must emit exactly ONE loud warning per
// cycle naming the project and the consequence (hub membership truncated to
// this run's scope, stale-hub cleanup disabled). An unfiltered run must emit
// no such warning at all.
func TestProjectFilteredExportWarnsAboutHubTruncation(t *testing.T) {
	ts := time.Now().Add(-1 * time.Hour)

	t.Run("a project filter logs exactly one truncation warning", func(t *testing.T) {
		vault := t.TempDir()
		store := &mockStore{
			listProjects: []string{"proj1"},
			byProject: map[string][]*domain.Record{
				"proj1": {newTestRecord(1, "proj1", "architecture", "sess-1", "Proj1 obs", ts)},
			},
		}
		exp, err := NewExporter(store, ExportConfig{VaultPath: vault, Project: "proj1"})
		if err != nil {
			t.Fatalf("NewExporter() error = %v", err)
		}
		logs := captureLog(exp)

		if _, err := exp.Export(); err != nil {
			t.Fatalf("Export() error = %v", err)
		}

		var warnings []string
		for _, line := range logs() {
			if strings.Contains(line, "proj1") && strings.Contains(line, "hub") {
				warnings = append(warnings, line)
			}
		}
		if len(warnings) != 1 {
			t.Fatalf("hub-truncation warnings = %d (%v), want exactly 1", len(warnings), warnings)
		}
		if !strings.Contains(warnings[0], "sweep") && !strings.Contains(warnings[0], "disabled") {
			t.Errorf("warning %q does not name the consequence (sweep disabled)", warnings[0])
		}
	})

	t.Run("no project filter logs no truncation warning", func(t *testing.T) {
		vault := t.TempDir()
		store := &mockStore{
			listProjects: []string{"proj1"},
			byProject: map[string][]*domain.Record{
				"proj1": {newTestRecord(1, "proj1", "architecture", "sess-1", "Proj1 obs", ts)},
			},
		}
		exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
		if err != nil {
			t.Fatalf("NewExporter() error = %v", err)
		}
		logs := captureLog(exp)

		if _, err := exp.Export(); err != nil {
			t.Fatalf("Export() error = %v", err)
		}

		for _, line := range logs() {
			if strings.Contains(line, "hub") && strings.Contains(line, "disabled") {
				t.Errorf("unfiltered export logged a hub-truncation warning: %q", line)
			}
		}
	})
}
