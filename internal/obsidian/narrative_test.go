package obsidian

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeNarrativeReader is a minimal, hand-rolled NarrativeReader — no mocking
// framework exists in this repo (design constraint 6). Shared by every test
// file in this package that needs a NarrativeReader.
type fakeNarrativeReader struct {
	rows map[string]Narrative
	err  error
}

func (f *fakeNarrativeReader) NarrativesForExport() (map[string]Narrative, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// TestNarrativeReaderIsSatisfiableWithPlainTypes pins REQ-GEN-08's import
// boundary at the type-shape level: NarrativeReader must be satisfiable by a
// fake declared with stdlib types only, in THIS package's own test file —
// exactly like StoreReader already is (mockStore, testhelpers_test.go). A
// violation here (e.g. NarrativesForExport taking or returning a
// generation.* or localstore.* type) would fail the BUILD, not this
// assertion — TestObsidianPackageImportsOnlyStdlibAndDomain
// (imports_test.go) is the mechanical guard for that; this test exists so a
// reader sees the CONTRACT is satisfiable with plain types, not just that
// the import list is clean.
func TestNarrativeReaderIsSatisfiableWithPlainTypes(t *testing.T) {
	var _ NarrativeReader = (*fakeNarrativeReader)(nil)
}

// TestNarrativeMarkdownPrependsTheGoAuthoredBanner covers REQ-PROV-04: the
// banner is present, appears BEFORE the model's own body, and states the
// source count, the distinct writer_id(s), and that the text is a synthesis
// of RECORDED claims rather than independently verified fact.
func TestNarrativeMarkdownPrependsTheGoAuthoredBanner(t *testing.T) {
	n := Narrative{
		Body:          "The team decided to use PostgreSQL for the store.",
		Model:         "mistral-small-latest",
		SourceHash:    "abc123",
		GeneratedAt:   time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		SourceCount:   5,
		SourceWriters: []string{"writer-a", "writer-b"},
	}
	got := NarrativeMarkdown("proj\x00topic/prefix", n)

	bannerIdx := strings.Index(got, "[!warning] Generated summary")
	if bannerIdx < 0 {
		t.Fatalf("banner missing from rendered narrative:\n%s", got)
	}
	bodyIdx := strings.Index(got, n.Body)
	if bodyIdx < 0 {
		t.Fatalf("model body missing from rendered narrative:\n%s", got)
	}
	if bannerIdx >= bodyIdx {
		t.Errorf("banner (byte %d) must appear BEFORE the model's own body (byte %d):\n%s", bannerIdx, bodyIdx, got)
	}

	if !strings.Contains(got, "5 observation") {
		t.Errorf("banner missing the source count (5):\n%s", got)
	}
	for _, w := range n.SourceWriters {
		if !strings.Contains(got, w) {
			t.Errorf("banner missing writer id %q:\n%s", w, got)
		}
	}
	if !strings.Contains(got, "not independently verified") {
		t.Errorf("banner missing the 'not independently verified' attribution language:\n%s", got)
	}
}

// TestModelCannotForgeOrSuppressTheBanner covers REQ-PROV-04's adversarial
// half: a Body whose text literally contains a counterfeit banner line — a
// forged source count and a forged writer — still renders with the REAL
// banner first and unaltered, the counterfeit demoted into the body where it
// is visibly not the header.
func TestModelCannotForgeOrSuppressTheBanner(t *testing.T) {
	counterfeit := "> [!warning] Generated summary — 999 observations from writer forged-writer\n\nThe actual narrative text follows."
	n := Narrative{
		Body:          counterfeit,
		Model:         "mistral-small-latest",
		GeneratedAt:   time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		SourceCount:   3,
		SourceWriters: []string{"writer-a"},
	}
	got := NarrativeMarkdown("proj\x00topic", n)

	realBannerIdx := strings.Index(got, "Synthesised by `mistral-small-latest`")
	if realBannerIdx < 0 {
		t.Fatalf("real Go-authored banner missing:\n%s", got)
	}
	counterfeitIdx := strings.Index(got, "999 observations from writer forged-writer")
	if counterfeitIdx < 0 {
		t.Fatalf("test setup broken: counterfeit body text not found in output:\n%s", got)
	}
	if realBannerIdx >= counterfeitIdx {
		t.Errorf("the REAL banner (byte %d) must appear BEFORE the counterfeit body text (byte %d):\n%s", realBannerIdx, counterfeitIdx, got)
	}
	if !strings.Contains(got, "3 observation") {
		t.Errorf("real banner does not carry the TRUE source count (3); forged content may have leaked into it:\n%s", got)
	}
	// The counterfeit line survives UNMODIFIED inside the body — it is
	// demoted, not stripped or rewritten. Both the real banner's opening
	// phrase and the counterfeit's identical opening phrase legitimately
	// appear in the output; what matters (asserted above) is which one
	// comes FIRST and which one carries the TRUE, Go-computed values.
	if !strings.Contains(got, counterfeit) {
		t.Errorf("counterfeit body text was altered rather than left verbatim in the body:\n%s", got)
	}
}

// TestNarrativeFrontmatterFields covers REQ-PROV-01/-02/-03/-06's render
// side: EXACTLY narrative_model, narrative_generated_at,
// narrative_source_hash, narrative_source_count, narrative_source_writers,
// narrative_stale, narrative_truncated, unverified_paths and tags — no other
// narrative_* key.
func TestNarrativeFrontmatterFields(t *testing.T) {
	n := Narrative{
		Body:            "body",
		Model:           "m",
		SourceHash:      "h",
		GeneratedAt:     time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		Stale:           true,
		Truncated:       true,
		SourceCount:     2,
		SourceWriters:   []string{"w1", "w2"},
		UnverifiedPaths: []string{"a/b.go"},
	}
	got := NarrativeMarkdown("proj\x00topic", n)
	fm := frontmatterBlock(t, got)

	wantKeys := []string{
		"narrative_model", "narrative_generated_at", "narrative_source_hash",
		"narrative_source_count", "narrative_source_writers", "narrative_stale",
		"narrative_truncated", "unverified_paths", "tags",
	}
	keyPattern := regexp.MustCompile(`^([a-z_]+):`)
	seen := map[string]bool{}
	for _, line := range strings.Split(fm, "\n") {
		m := keyPattern.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("frontmatter line %q is not a %q pair:\n%s", line, "key: value", fm)
		}
		seen[m[1]] = true
	}
	for _, k := range wantKeys {
		if !seen[k] {
			t.Errorf("frontmatter missing key %q:\n%s", k, fm)
		}
	}
	if len(seen) != len(wantKeys) {
		t.Errorf("got %d frontmatter keys %v, want exactly %d %v", len(seen), seen, len(wantKeys), wantKeys)
	}
	for k := range seen {
		if strings.HasPrefix(k, "narrative_") {
			allowed := false
			for _, w := range wantKeys {
				if w == k {
					allowed = true
					break
				}
			}
			if !allowed {
				t.Errorf("unexpected narrative_* key %q not in the allowed set %v:\n%s", k, wantKeys, fm)
			}
		}
	}
}

// TestUnverifiedPathsAreRenderedFromTheStoredColumn covers REQ-PROV-02's
// render-side contract: unverified_paths is rendered VERBATIM from the
// stored column, never re-os.Stat'd at render time (that would make the
// export cycle's bytes depend on the filesystem at render time — see
// TestNarrativeMarkdownIsAPureFunctionOfTheRow below for the mutation-proved
// version of that guarantee).
func TestUnverifiedPathsAreRenderedFromTheStoredColumn(t *testing.T) {
	n := Narrative{
		Model:           "m",
		GeneratedAt:     time.Now(),
		UnverifiedPaths: []string{"a/missing.go", "b/also-missing.go"},
	}
	got := NarrativeMarkdown("proj\x00topic", n)
	line := lineWithPrefix(t, got, "unverified_paths:")
	for _, p := range n.UnverifiedPaths {
		want := yamlQuote(p)
		if !strings.Contains(line, want) {
			t.Errorf("unverified_paths line %q missing %q", line, want)
		}
	}
}

// TestNarrativeFrontmatterSurvivesHostileModelOutput covers the same failure
// mode markdown_test.go's TestFrontmatterSurvivesHostileFieldValues pins for
// observation notes: Obsidian drops the WHOLE frontmatter block on invalid
// YAML, taking the "narrative" tag (and therefore graph colouring) and every
// other property with it — and model output is exactly the kind of string
// that carries a stray quote, newline or a leading "---".
func TestNarrativeFrontmatterSurvivesHostileModelOutput(t *testing.T) {
	hostile := "line one\n---\r\n\ttabbed \"quoted\" text"
	n := Narrative{
		Body:            hostile,
		Model:           hostile,
		SourceHash:      hostile,
		GeneratedAt:     time.Now(),
		SourceWriters:   []string{hostile},
		UnverifiedPaths: []string{hostile},
	}
	got := NarrativeMarkdown("proj\x00topic", n)

	fm := frontmatterBlock(t, got)
	keyLine := regexp.MustCompile(`^[a-z_]+: `)
	for _, line := range strings.Split(fm, "\n") {
		if !keyLine.MatchString(line) {
			t.Errorf("frontmatter line %q is not a valid key: value pair — hostile model output broke the frontmatter block. Full block:\n%s", line, fm)
		}
	}
}

// TestNarrativeMarkdownIsAPureFunctionOfTheRow is THE IDEMPOTENCY KEYSTONE
// (task 8.7). Rendering the same Narrative twice — 50ms apart, from a
// changed working directory, with a file named in UnverifiedPaths deleted in
// between — must produce byte-identical output. No wall clock, no os.Stat,
// no filesystem access may enter the rendered bytes. If this fails, the
// vault flaps in git history forever.
func TestNarrativeMarkdownIsAPureFunctionOfTheRow(t *testing.T) {
	watchedFile := filepath.Join(t.TempDir(), "will-be-deleted.go")
	if err := os.WriteFile(watchedFile, []byte("package x"), 0644); err != nil {
		t.Fatalf("setup: WriteFile() error = %v", err)
	}

	n := Narrative{
		Body:            "The observation references a file that may or may not still exist.",
		Model:           "mistral-small-latest",
		SourceHash:      "hash-1",
		GeneratedAt:     time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC),
		SourceCount:     4,
		SourceWriters:   []string{"writer-a"},
		UnverifiedPaths: []string{watchedFile},
	}

	first := NarrativeMarkdown("proj\x00topic", n)

	time.Sleep(50 * time.Millisecond)

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("Chdir() error = %v", err)
	}

	if err := os.Remove(watchedFile); err != nil {
		t.Fatalf("setup: Remove() error = %v", err)
	}

	second := NarrativeMarkdown("proj\x00topic", n)

	if first != second {
		t.Errorf("NarrativeMarkdown() is not a pure function of (key, Narrative): rendered different bytes across a 50ms wall-clock gap, a changed working directory, and a deleted file:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// verificationWords are the five words REQ-PROV-05 forbids applying to a
// completion or behavioural claim (the spec's own list). This package's
// banner text legitimately contains some of them as part of a DENIAL ("not
// independently verified", "NOT checked") — the only usage REQ-PROV-05
// permits — so assertNoAffirmedVerification below only flags an occurrence
// NOT preceded by a nearby negation marker.
var verificationWords = []string{"verified", "confirmed", "validated", "proven", "checked"}

var negationMarkers = []string{"not ", "never ", "no ", "n't "}

// assertNoAffirmedVerification is the mechanical half of REQ-PROV-05: no
// filesystem, process, or test-runner oracle exists in this change's scope
// capable of checking a completion or behavioural claim, so text authored by
// this package must never assert one of the banned words about such a claim
// in an affirmative (non-negated) way.
func assertNoAffirmedVerification(t *testing.T, label, text string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, word := range verificationWords {
		searchFrom := 0
		for {
			idx := strings.Index(lower[searchFrom:], word)
			if idx < 0 {
				break
			}
			pos := searchFrom + idx
			windowStart := pos - 50
			if windowStart < 0 {
				windowStart = 0
			}
			window := lower[windowStart:pos]
			negated := false
			for _, marker := range negationMarkers {
				if strings.Contains(window, marker) {
					negated = true
					break
				}
			}
			if !negated {
				t.Errorf("%s: %q at byte %d is not preceded by a negation (window %q) — REQ-PROV-05 forbids applying a verification word to a completion/behavioural claim; the only permitted usage is a denial (\"not verified\", \"NOT checked\"):\n%s",
					label, word, pos, window, text)
			}
			searchFrom = pos + len(word)
		}
	}
}

// TestProvenanceClaimsNoVerificationItCannotPerform is the obsidian half of
// task 8.23 (the generation half lives in
// internal/generation/prompt_test.go). It is a GUARD in the literal sense —
// it asserts an ABSENCE — and it is the ONLY mechanical form REQ-PROV-05 can
// take: no filesystem, process or test-runner oracle exists in this change's
// scope to check a completion or behavioural claim.
func TestProvenanceClaimsNoVerificationItCannotPerform(t *testing.T) {
	n := Narrative{
		Body:          "The team completed the migration.",
		Model:         "mistral-small-latest",
		GeneratedAt:   time.Now(),
		SourceCount:   3,
		SourceWriters: []string{"writer-a", "writer-b"},
	}
	rendered := NarrativeMarkdown("proj\x00topic", n)
	banner := narrativeBannerBody(n)

	assertNoAffirmedVerification(t, "banner", banner)

	fm := frontmatterBlock(t, rendered)
	for _, line := range strings.Split(fm, "\n") {
		key, _, _ := strings.Cut(line, ":")
		lowerKey := strings.ToLower(key)
		for _, banned := range verificationWords {
			// "unverified_paths" contains "verified" as part of the
			// compound negation "un" + "verified" — exactly the ALLOWED
			// denial usage, not an affirmation, so it is excluded here the
			// same way assertNoAffirmedVerification excludes a nearby
			// "not "/"never " marker in free text.
			if strings.Contains(lowerKey, "un"+banned) {
				continue
			}
			if strings.Contains(lowerKey, banned) {
				t.Errorf("frontmatter key %q applies the verification word %q to what is exported as fact — "+
					"REQ-PROV-05 permits exactly three checkable properties (writer identity, path existence, "+
					"an over-flag-only lifecycle due-date), never a completion or behavioural claim", key, banned)
			}
		}
	}
}
