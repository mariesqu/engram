package obsidian

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// TestSessionHubMergesCaseVariantSessionIDs covers a THIRD finding from the
// real-data smoke test that verified the project-dedup fix
// (bugfix/listprojects-case-variant-double-read): the same project-name case
// drift that produced duplicate ListProjects() entries also produced
// case-variant SESSION IDs in the live data ("manual-save-gitlab" vs
// "manual-save-GitLab", one pair per drifted project — 5 pairs found).
//
// bySession is keyed by the raw, case-sensitive SessionID string, and the
// hub filename is derived from that key via safeFilename, which does NOT
// fold case. On a case-insensitive filesystem (Windows NTFS, macOS default
// HFS+/APFS -- i.e. most Obsidian vaults), two keys differing only by case
// resolve to the SAME physical file regardless of what the exporter
// intends. Left unmerged, EVERY cycle writes that one physical file twice
// with two different bodies (whichever key is processed second always sees
// the other's leftover content and treats it as changed) -- this is not a
// one-time fluke, it perpetually rewrites those hub files on every single
// run forever, which is exactly what the live smoke test observed: hubs=8
// on a run where NOTHING in the underlying data had changed.
func TestSessionHubMergesCaseVariantSessionIDs(t *testing.T) {
	vault := t.TempDir()
	ts := time.Now().Add(-time.Hour)

	store := &mockStore{
		listProjects: []string{"proj1"},
		byProject: map[string][]*domain.Record{
			"proj1": {
				newTestRecord(1, "proj1", "decision", "manual-save-gitlab", "First body", ts),
				newTestRecord(2, "proj1", "decision", "manual-save-GitLab", "Second body", ts),
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

	sessionsDir := filepath.Join(vault, "engram", "_sessions")
	files := allFiles(t, sessionsDir)
	if len(files) != 1 {
		t.Fatalf("expected exactly 1 session hub file for case-variant session ids, got %d: %v", len(files), files)
	}

	content, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", files[0], err)
	}
	if !strings.Contains(string(content), "first-body-1") {
		t.Errorf("merged session hub is missing the observation filed under session %q:\n%s", "manual-save-gitlab", content)
	}
	if !strings.Contains(string(content), "second-body-2") {
		t.Errorf("merged session hub is missing the observation filed under session %q:\n%s", "manual-save-GitLab", content)
	}

	// The critical assertion: a second cycle over UNCHANGED data must be a
	// true no-op for hub notes. Before the fix, this hub file ping-pongs
	// between the two case-variant contents on every single cycle.
	second, err := exp.Export()
	if err != nil {
		t.Fatalf("second Export() error = %v", err)
	}
	if second.Hubs != 0 {
		t.Errorf("Hubs = %d on an unchanged second run, want 0 -- case-variant session ids must not perpetually rewrite the same hub file", second.Hubs)
	}
}

// TestTopicHubMergesCaseVariantPrefixes covers the identical scenario for
// topic hubs: a topic_key prefix that differs only in case between two
// observations must not perpetually rewrite the same physical hub file.
func TestTopicHubMergesCaseVariantPrefixes(t *testing.T) {
	vault := t.TempDir()
	ts := time.Now().Add(-time.Hour)

	topicLower := "sdd/change/spec"
	topicUpper := "SDD/change/spec"
	rec1 := newTestRecord(1, "proj1", "decision", "sess-1", "First body", ts)
	rec1.TopicKey = &topicLower
	rec2 := newTestRecord(2, "proj1", "decision", "sess-1", "Second body", ts)
	rec2.TopicKey = &topicUpper

	store := &mockStore{
		listProjects: []string{"proj1"},
		byProject:    map[string][]*domain.Record{"proj1": {rec1, rec2}},
	}

	exp, err := NewExporter(store, ExportConfig{VaultPath: vault})
	if err != nil {
		t.Fatalf("NewExporter() error = %v", err)
	}
	if _, err := exp.Export(); err != nil {
		t.Fatalf("first Export() error = %v", err)
	}

	topicsDir := filepath.Join(vault, "engram", "_topics")
	files := allFiles(t, topicsDir)
	if len(files) != 1 {
		t.Fatalf("expected exactly 1 topic hub file for case-variant topic prefixes, got %d: %v", len(files), files)
	}

	second, err := exp.Export()
	if err != nil {
		t.Fatalf("second Export() error = %v", err)
	}
	if second.Hubs != 0 {
		t.Errorf("Hubs = %d on an unchanged second run, want 0 -- case-variant topic prefixes must not perpetually rewrite the same hub file", second.Hubs)
	}
}
