package localstore

import (
	"testing"
	"time"
)

func sampleNarrativeRow(project, topicPrefix string) NarrativeRow {
	return NarrativeRow{
		Project:         project,
		TopicPrefix:     topicPrefix,
		Body:            "Synthesised narrative body for " + project + "/" + topicPrefix,
		SourceHash:      "hash-" + project + "-" + topicPrefix,
		Model:           "mistral-large-latest",
		TemplateVersion: "pt-1",
		RendererVersion: "rv-1",
		SourceCount:     5,
		SourceWriters:   "writer-a,writer-b",
		UnverifiedPaths: "some/path.go\nother/path.go",
		Truncated:       false,
		Stale:           false,
		GeneratedAt:     time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC),
	}
}

func keysOf(m map[string]NarrativeRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestNarrativeUpsertRoundTrip verifies every column reads back through
// NarrativesForExport, and that a second upsert on the same (project,
// topic_prefix) OVERWRITES the row rather than creating a duplicate.
func TestNarrativeUpsertRoundTrip(t *testing.T) {
	st := openTempStore(t)

	row := sampleNarrativeRow("engram", "architecture/generation")
	if err := st.UpsertNarrative(row); err != nil {
		t.Fatalf("UpsertNarrative: %v", err)
	}

	exported, err := st.NarrativesForExport()
	if err != nil {
		t.Fatalf("NarrativesForExport: %v", err)
	}
	key := narrativeFoldKey(row.Project, row.TopicPrefix)
	got, ok := exported[key]
	if !ok {
		t.Fatalf("NarrativesForExport missing key %q; got keys %v", key, keysOf(exported))
	}
	if got.Project != "engram" || got.TopicPrefix != "architecture/generation" {
		t.Errorf("Project/TopicPrefix = %q/%q, want %q/%q", got.Project, got.TopicPrefix, "engram", "architecture/generation")
	}
	if got.Body != row.Body || got.SourceHash != row.SourceHash || got.Model != row.Model ||
		got.TemplateVersion != row.TemplateVersion || got.RendererVersion != row.RendererVersion ||
		got.SourceCount != row.SourceCount || got.SourceWriters != row.SourceWriters ||
		got.UnverifiedPaths != row.UnverifiedPaths || got.Truncated != row.Truncated || got.Stale != row.Stale {
		t.Errorf("round-tripped row = %+v, want fields matching %+v", got, row)
	}
	if !got.GeneratedAt.Equal(row.GeneratedAt) {
		t.Errorf("GeneratedAt = %v, want %v", got.GeneratedAt, row.GeneratedAt)
	}

	// Second upsert on the SAME key overwrites, never duplicates.
	updated := row
	updated.Body = "a completely different, regenerated body"
	updated.SourceHash = "hash-changed"
	if err := st.UpsertNarrative(updated); err != nil {
		t.Fatalf("second UpsertNarrative: %v", err)
	}

	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM narratives`).Scan(&count); err != nil {
		t.Fatalf("count narratives: %v", err)
	}
	if count != 1 {
		t.Errorf("narratives row count = %d after two upserts on the same key, want 1 (overwrite, not duplicate)", count)
	}

	exported2, err := st.NarrativesForExport()
	if err != nil {
		t.Fatalf("NarrativesForExport after overwrite: %v", err)
	}
	if exported2[key].Body != updated.Body {
		t.Errorf("Body after overwrite = %q, want %q", exported2[key].Body, updated.Body)
	}
	if exported2[key].SourceHash != updated.SourceHash {
		t.Errorf("SourceHash after overwrite = %q, want %q", exported2[key].SourceHash, updated.SourceHash)
	}
}

// TestNarrativeCacheKeysReturnsFoldedKeyToHash verifies the bulk cache-key
// scan the narrative loop's cache-miss test relies on.
func TestNarrativeCacheKeysReturnsFoldedKeyToHash(t *testing.T) {
	st := openTempStore(t)

	rowA := sampleNarrativeRow("ProjA", "topic/one")
	rowA.SourceHash = "hash-a"
	rowB := sampleNarrativeRow("projb", "topic/two")
	rowB.SourceHash = "hash-b"
	for _, r := range []NarrativeRow{rowA, rowB} {
		if err := st.UpsertNarrative(r); err != nil {
			t.Fatalf("UpsertNarrative: %v", err)
		}
	}

	keys, err := st.NarrativeCacheKeys()
	if err != nil {
		t.Fatalf("NarrativeCacheKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("NarrativeCacheKeys returned %d entries, want 2: %v", len(keys), keys)
	}
	if got := keys[narrativeFoldKey("ProjA", "topic/one")]; got != "hash-a" {
		t.Errorf("hash for ProjA/topic/one = %q, want %q", got, "hash-a")
	}
	if got := keys[narrativeFoldKey("projb", "topic/two")]; got != "hash-b" {
		t.Errorf("hash for projb/topic/two = %q, want %q", got, "hash-b")
	}
}

// TestNarrativesForExportReturnsEveryRenderField exercises a multi-row scan
// and confirms nothing is dropped or scrambled between rows.
func TestNarrativesForExportReturnsEveryRenderField(t *testing.T) {
	st := openTempStore(t)

	rows := []NarrativeRow{
		sampleNarrativeRow("alpha", "topic/one"),
		sampleNarrativeRow("beta", "topic/two"),
	}
	rows[0].Truncated = true
	rows[1].Stale = true
	for _, r := range rows {
		if err := st.UpsertNarrative(r); err != nil {
			t.Fatalf("UpsertNarrative: %v", err)
		}
	}

	exported, err := st.NarrativesForExport()
	if err != nil {
		t.Fatalf("NarrativesForExport: %v", err)
	}
	if len(exported) != 2 {
		t.Fatalf("NarrativesForExport returned %d rows, want 2", len(exported))
	}
	a := exported[narrativeFoldKey("alpha", "topic/one")]
	if !a.Truncated {
		t.Error("alpha/topic/one: Truncated = false, want true")
	}
	if a.Stale {
		t.Error("alpha/topic/one: Stale = true, want false")
	}
	b := exported[narrativeFoldKey("beta", "topic/two")]
	if !b.Stale {
		t.Error("beta/topic/two: Stale = false, want true")
	}
	if b.Truncated {
		t.Error("beta/topic/two: Truncated = true, want false")
	}
}

// TestMarkNarrativesStaleIsBatchedAndScoped verifies MarkNarrativesStale
// marks exactly the named keys and leaves every other row untouched.
func TestMarkNarrativesStaleIsBatchedAndScoped(t *testing.T) {
	st := openTempStore(t)

	for _, r := range []NarrativeRow{
		sampleNarrativeRow("p1", "t1"),
		sampleNarrativeRow("p2", "t2"),
		sampleNarrativeRow("p3", "t3"),
	} {
		if err := st.UpsertNarrative(r); err != nil {
			t.Fatalf("UpsertNarrative: %v", err)
		}
	}

	if err := st.MarkNarrativesStale([]string{
		narrativeFoldKey("p1", "t1"),
		narrativeFoldKey("p3", "t3"),
	}); err != nil {
		t.Fatalf("MarkNarrativesStale: %v", err)
	}

	exported, err := st.NarrativesForExport()
	if err != nil {
		t.Fatalf("NarrativesForExport: %v", err)
	}
	if !exported[narrativeFoldKey("p1", "t1")].Stale {
		t.Error("p1/t1 should be marked stale")
	}
	if exported[narrativeFoldKey("p2", "t2")].Stale {
		t.Error("p2/t2 must remain stale=false — it was not named")
	}
	if !exported[narrativeFoldKey("p3", "t3")].Stale {
		t.Error("p3/t3 should be marked stale")
	}
}

// TestMarkNarrativesStaleEmptySliceIssuesNoStatement proves an empty slice
// short-circuits BEFORE touching the database at all: the underlying
// *sql.DB is closed first, so any attempted Exec would return an error — a
// nil return here is only possible if no statement was ever issued.
func TestMarkNarrativesStaleEmptySliceIssuesNoStatement(t *testing.T) {
	st := openTempStore(t)
	if err := st.DB().Close(); err != nil {
		t.Fatalf("close underlying db: %v", err)
	}

	if err := st.MarkNarrativesStale(nil); err != nil {
		t.Errorf("MarkNarrativesStale(nil) on a closed DB = %v, want nil — an empty slice must issue no statement at all", err)
	}
	if err := st.MarkNarrativesStale([]string{}); err != nil {
		t.Errorf("MarkNarrativesStale([]string{}) on a closed DB = %v, want nil", err)
	}
}

// TestNarrativeKeysAreCaseFolded guards bugfix/listprojects-case-variant-double-read:
// "Engram" and "engram" (and their topic_prefix counterparts) must collapse
// to ONE row, never two.
func TestNarrativeKeysAreCaseFolded(t *testing.T) {
	st := openTempStore(t)

	if err := st.UpsertNarrative(sampleNarrativeRow("Engram", "Architecture/Generation")); err != nil {
		t.Fatalf("UpsertNarrative (mixed case): %v", err)
	}
	if err := st.UpsertNarrative(sampleNarrativeRow("engram", "architecture/generation")); err != nil {
		t.Fatalf("UpsertNarrative (lower case): %v", err)
	}

	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM narratives`).Scan(&count); err != nil {
		t.Fatalf("count narratives: %v", err)
	}
	if count != 1 {
		t.Errorf("narratives row count = %d for case-variant project/topic pairs, want 1 (case-folded collapse)", count)
	}
}
