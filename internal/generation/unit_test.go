package generation

import (
	"testing"

	"github.com/mariesqu/engram/internal/domain"
)

func topicKeyPtr(s string) *string { return &s }

func newRecord(project, topicKey, syncID string) *domain.Record {
	return &domain.Record{
		SyncID:   syncID,
		Project:  project,
		TopicKey: topicKeyPtr(topicKey),
		Title:    "title-" + syncID,
		Content:  "content-" + syncID,
		WriterID: "writer-1",
	}
}

// TestUnitKeyIsCaseFolded pins UnitKey's case-folding: the unit lookup key
// must collapse casing differences in both project and prefix, mirroring
// localstore.narrativeFoldKey and obsidian's own NarrativeReader key so all
// three layers agree on identity for the same logical unit.
func TestUnitKeyIsCaseFolded(t *testing.T) {
	a := UnitKey("Engram", "sdd/obsidian-narrative")
	b := UnitKey("engram", "SDD/Obsidian-Narrative")
	if a != b {
		t.Errorf("UnitKey not case-folded: %q != %q", a, b)
	}
	if a != "engram\x00sdd/obsidian-narrative" {
		t.Errorf("UnitKey(%q) = %q, want the NUL-joined lower-cased form", "Engram, sdd/obsidian-narrative", a)
	}
}

// TestUnitsAreScopedByProjectAndTopicPrefix pins REQ-GEN-03: a topic prefix
// carrying records from project A AND project B must yield EXACTLY TWO
// units, (A,prefix) and (B,prefix), never one merged unit. Phase A's topic
// hubs are inherently cross-project (byTopicPrefix is accumulated inside
// the per-project loop but iterated outside it, exporter.go:223-239,405),
// so a bare per-topic unit would hand the gate one project's policy while
// the prompt contained another project's text -- a privacy-gate bypass.
func TestUnitsAreScopedByProjectAndTopicPrefix(t *testing.T) {
	recs := []*domain.Record{
		newRecord("A", "sdd/prefix/leaf", "a-1"),
		newRecord("A", "sdd/prefix/leaf", "a-2"),
		newRecord("B", "sdd/prefix/leaf", "b-1"),
	}

	units := AssembleUnits(recs)
	if len(units) != 2 {
		t.Fatalf("AssembleUnits returned %d units, want exactly 2 (one per project): %+v", len(units), units)
	}

	byProject := map[string]Unit{}
	for _, u := range units {
		byProject[u.ProjectKey] = u
	}
	ua, ok := byProject["A"]
	if !ok {
		t.Fatalf("no unit found for project A: %+v", units)
	}
	ub, ok := byProject["B"]
	if !ok {
		t.Fatalf("no unit found for project B: %+v", units)
	}
	if len(ua.Records) != 2 {
		t.Errorf("unit A has %d records, want 2", len(ua.Records))
	}
	if len(ub.Records) != 1 {
		t.Errorf("unit B has %d records, want 1", len(ub.Records))
	}
	if ua.TopicPrefixKey != "sdd/prefix" || ub.TopicPrefixKey != "sdd/prefix" {
		t.Errorf("unexpected TopicPrefixKey: A=%q B=%q, want %q both (split on the LAST \"/\" of %q)",
			ua.TopicPrefixKey, ub.TopicPrefixKey, "sdd/prefix", "sdd/prefix/leaf")
	}
}

// TestNoForeignProjectRecordEntersAUnit triangulates the same guarantee at
// the member level: for unit (A,prefix) every member's Project folds to
// "a", and the converse holds for (B,prefix)'s unit. Explicit per the
// proposal's own success criterion.
func TestNoForeignProjectRecordEntersAUnit(t *testing.T) {
	recs := []*domain.Record{
		newRecord("A", "sdd/prefix/leaf", "a-1"),
		newRecord("B", "sdd/prefix/leaf", "b-1"),
		newRecord("B", "sdd/prefix/leaf", "b-2"),
	}

	units := AssembleUnits(recs)
	for _, u := range units {
		for _, r := range u.Records {
			if !sameFold(r.Project, u.ProjectKey) {
				t.Errorf("unit %s/%s contains a record from project %q -- foreign-project leak",
					u.ProjectKey, u.TopicPrefixKey, r.Project)
			}
		}
	}
}

func sameFold(a, b string) bool {
	return UnitKey(a, "x") == UnitKey(b, "x")
}

// TestUnitEligibilityThreshold pins REQ-NARR-04's boundary-inclusive
// threshold on BOTH axes: a unit needs at least 3 observations AND at
// least 1,500 combined content characters.
func TestUnitEligibilityThreshold(t *testing.T) {
	tests := []struct {
		name       string
		numObs     int
		inputChars int
		want       bool
	}{
		{"2 obs / 5000 chars -> ineligible (obs axis)", 2, 5000, false},
		{"3 obs / 1499 chars -> ineligible (chars axis)", 3, 1499, false},
		{"3 obs / 1500 chars -> eligible (both floors met)", 3, 1500, true},
		{"10 obs / 20000 chars -> eligible", 10, 20000, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recs := make([]*domain.Record, tt.numObs)
			for i := range recs {
				recs[i] = &domain.Record{SyncID: "s"}
			}
			u := Unit{Records: recs, InputChars: tt.inputChars}
			if got := u.Eligible(3, 1500); got != tt.want {
				t.Errorf("Unit{%d records, %d chars}.Eligible(3,1500) = %v, want %v", tt.numObs, tt.inputChars, got, tt.want)
			}
		})
	}
}

// TestIneligibleUnitOccupiesNoCacheRow documents the contract at the
// boundary this package owns: Eligible is a pure predicate, and the loop
// (a later slice) is responsible for never calling UpsertNarrative for a
// unit where Eligible returns false. Pinned here as a compile-time/behaviour
// check on the predicate itself, since the cache-row-skipping behaviour
// lives in loop.go (Phase 6, out of this slice's scope).
func TestIneligibleUnitOccupiesNoCacheRow(t *testing.T) {
	u := Unit{Records: []*domain.Record{{SyncID: "only-one"}}, InputChars: 50}
	if u.Eligible(3, 1500) {
		t.Fatalf("a 1-record/50-char unit must be ineligible")
	}
}
