package topickey

import (
	"strings"
	"testing"
)

func TestSuggest_Deterministic(t *testing.T) {
	a := Suggest("decision", "Use cookie auth over JWT", "we chose cookies")
	b := Suggest("decision", "Use cookie auth over JWT", "we chose cookies")
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
}

func TestSuggest_FamilyFromType(t *testing.T) {
	cases := map[string]string{
		"bugfix":        "bug/",
		"architecture":  "architecture/",
		"decision":      "decision/",
		"pattern":       "pattern/",
		"config":        "config/",
		"discovery":     "discovery/",
		"learning":      "learning/",
		"session_summary": "session/",
	}
	for typ, wantPrefix := range cases {
		got := Suggest(typ, "some title here", "")
		if !strings.HasPrefix(got, wantPrefix) {
			t.Errorf("Suggest(type=%q) = %q; want prefix %q", typ, got, wantPrefix)
		}
	}
}

func TestSuggest_NormalizesTitle(t *testing.T) {
	got := Suggest("decision", "Use Cookie Auth!!! (over JWT)", "")
	if got != "decision/use-cookie-auth-over-jwt" {
		t.Errorf("got %q; want decision/use-cookie-auth-over-jwt", got)
	}
}

func TestSuggest_FamilyFromContentKeywords(t *testing.T) {
	// Type "manual" and a neutral title, but content mentions a crash → bug family.
	got := Suggest("manual", "user list page", "fixed a panic that caused a crash on load")
	if !strings.HasPrefix(got, "bug/") {
		t.Errorf("got %q; want bug/ prefix (content keyword inference)", got)
	}
}

func TestSuggest_EmptyFallsBackToTopicGeneral(t *testing.T) {
	if got := Suggest("", "", ""); got != "topic/general" {
		t.Errorf("got %q; want topic/general", got)
	}
}

func TestSuggest_SegmentFromContentWhenNoTitle(t *testing.T) {
	got := Suggest("decision", "", "we will use PostgreSQL not MSSQL")
	// family decision, segment from first content words.
	if !strings.HasPrefix(got, "decision/") || got == "decision/general" {
		t.Errorf("got %q; want a decision/<segment-from-content>", got)
	}
}
