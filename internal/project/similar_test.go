package project

import "testing"

// ─── levenshtein unit tests ──────────────────────────────────────────────────

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{a: "", b: "", want: 0},
		{a: "abc", b: "", want: 3},
		{a: "", b: "abc", want: 3},
		{a: "abc", b: "abc", want: 0},
		{a: "kitten", b: "sitting", want: 3},
		{a: "saturday", b: "sunday", want: 3},
		{a: "engram", b: "engam", want: 1},  // single deletion
		{a: "engram", b: "engram", want: 0}, // identical
		{a: "a", b: "b", want: 1},
		{a: "abc", b: "ac", want: 1},   // one deletion
		{a: "abc", b: "axc", want: 1},  // one substitution
		{a: "abc", b: "abcd", want: 1}, // one insertion
	}

	for _, tc := range tests {
		got := levenshtein(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("levenshtein(%q, %q) = %d; want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestLevenshtein_Symmetry verifies that levenshtein(a,b) == levenshtein(b,a).
func TestLevenshtein_Symmetry(t *testing.T) {
	pairs := [][2]string{
		{"engram", "engam"},
		{"kitten", "sitting"},
		{"abc", "xyz"},
		{"", "hello"},
	}
	for _, p := range pairs {
		ab := levenshtein(p[0], p[1])
		ba := levenshtein(p[1], p[0])
		if ab != ba {
			t.Errorf("levenshtein(%q,%q)=%d != levenshtein(%q,%q)=%d (symmetry broken)",
				p[0], p[1], ab, p[1], p[0], ba)
		}
	}
}

// ─── FindSimilar unit tests ──────────────────────────────────────────────────

func TestFindSimilar_CaseInsensitiveAndSubstring(t *testing.T) {
	existing := []string{"Engram", "engram-memory", "totally-different"}
	matches := FindSimilar("engram", existing, 3)

	if len(matches) < 2 {
		t.Fatalf("expected at least 2 matches, got %d: %v", len(matches), matches)
	}

	if matches[0].Name != "Engram" || matches[0].MatchType != "case-insensitive" {
		t.Errorf("first match = %+v; want {Engram, case-insensitive}", matches[0])
	}

	if matches[1].Name != "engram-memory" || matches[1].MatchType != "substring" {
		t.Errorf("second match = %+v; want {engram-memory, substring}", matches[1])
	}
}

func TestFindSimilar_TiandaGroup(t *testing.T) {
	existing := []string{"tianda-for-woocommerce", "tianda-wc-plugin", "Tianda"}
	matches := FindSimilar("tianda", existing, 3)

	if len(matches) != 3 {
		t.Fatalf("expected 3 matches, got %d: %v", len(matches), matches)
	}

	foundCase := false
	foundSub1 := false
	foundSub2 := false
	for _, m := range matches {
		switch m.Name {
		case "Tianda":
			foundCase = true
			if m.MatchType != "case-insensitive" {
				t.Errorf("Tianda match type = %q; want case-insensitive", m.MatchType)
			}
		case "tianda-for-woocommerce":
			foundSub1 = true
			if m.MatchType != "substring" {
				t.Errorf("tianda-for-woocommerce match type = %q; want substring", m.MatchType)
			}
		case "tianda-wc-plugin":
			foundSub2 = true
			if m.MatchType != "substring" {
				t.Errorf("tianda-wc-plugin match type = %q; want substring", m.MatchType)
			}
		}
	}
	if !foundCase {
		t.Error("Tianda not found in results")
	}
	if !foundSub1 {
		t.Error("tianda-for-woocommerce not found in results")
	}
	if !foundSub2 {
		t.Error("tianda-wc-plugin not found in results")
	}
}

func TestFindSimilar_ExcludesExactMatch(t *testing.T) {
	existing := []string{"engram"}
	matches := FindSimilar("engram", existing, 3)

	if len(matches) != 0 {
		t.Errorf("expected empty results for exact match, got %v", matches)
	}
}

func TestFindSimilar_NothingSimilar(t *testing.T) {
	existing := []string{"xyz", "qrs", "totally-unrelated"}
	matches := FindSimilar("abc", existing, 1)

	if len(matches) != 0 {
		t.Errorf("expected no matches, got %v", matches)
	}
}

func TestFindSimilar_LevenshteinHit(t *testing.T) {
	existing := []string{"engam"} // distance 1 from "engram"
	matches := FindSimilar("engram", existing, 2)

	if len(matches) != 1 {
		t.Fatalf("expected 1 levenshtein match, got %d: %v", len(matches), matches)
	}
	if matches[0].Name != "engam" {
		t.Errorf("match name = %q; want engam", matches[0].Name)
	}
	if matches[0].MatchType != "levenshtein" {
		t.Errorf("match type = %q; want levenshtein", matches[0].MatchType)
	}
	if matches[0].Distance != 1 {
		t.Errorf("distance = %d; want 1", matches[0].Distance)
	}
}

func TestFindSimilar_LevenshteinBeyondMaxDistance(t *testing.T) {
	existing := []string{"completely-different-string"}
	matches := FindSimilar("engram", existing, 2)

	if len(matches) != 0 {
		t.Errorf("expected no matches beyond max distance, got %v", matches)
	}
}

func TestFindSimilar_OrderingCaseFirst(t *testing.T) {
	existing := []string{
		"engam",      // levenshtein distance 1
		"Engram",     // case-insensitive
		"engram-old", // substring
	}
	matches := FindSimilar("engram", existing, 2)

	if len(matches) < 3 {
		t.Fatalf("expected 3 matches, got %d: %v", len(matches), matches)
	}

	if matches[0].MatchType != "case-insensitive" {
		t.Errorf("matches[0].MatchType = %q; want case-insensitive", matches[0].MatchType)
	}
	if matches[1].MatchType != "substring" {
		t.Errorf("matches[1].MatchType = %q; want substring", matches[1].MatchType)
	}
	if matches[2].MatchType != "levenshtein" {
		t.Errorf("matches[2].MatchType = %q; want levenshtein", matches[2].MatchType)
	}
}

func TestFindSimilar_EmptyExisting(t *testing.T) {
	matches := FindSimilar("engram", []string{}, 3)
	if len(matches) != 0 {
		t.Errorf("expected no matches for empty existing list, got %v", matches)
	}
}

func TestFindSimilar_ZeroMaxDistance(t *testing.T) {
	// With maxDistance=0, levenshtein matches at distance > 0 must be excluded.
	existing := []string{"engam"} // distance 1
	matches := FindSimilar("engram", existing, 0)

	if len(matches) != 0 {
		t.Errorf("expected no matches with maxDistance=0, got %v", matches)
	}
}

// TestFindSimilar_NonASCIIRuneScaling verifies Fix 4: effectiveMax is derived
// from the rune count of the query, not its byte length. A 2-rune non-ASCII
// name ("éà") has runeLen=2 but byteLen=4; with maxDistance=3 the effective max
// must be 1 (max(1, 2/2)), not 2 (byte-based: 4/2). The candidate "éb" has
// Levenshtein distance 1, so it must still match; a candidate at distance 2
// must not match (it exceeds effectiveMax=1).
func TestFindSimilar_NonASCIIRuneScaling(t *testing.T) {
	// "éà" is 2 runes / 4 bytes. effectiveMax = max(1, 2/2) = 1.
	query := "éà"
	// distance("éà","éb") == 1  → should match
	// distance("éà","xy") == 2  → must NOT match (exceeds effectiveMax=1)
	existing := []string{"éb", "xy"}
	matches := FindSimilar(query, existing, 3)

	foundEb := false
	foundXy := false
	for _, m := range matches {
		switch m.Name {
		case "éb":
			foundEb = true
		case "xy":
			foundXy = true
		}
	}

	if !foundEb {
		t.Errorf("expected 'éb' (distance 1) to match with rune-based effectiveMax=1, but it did not")
	}
	if foundXy {
		t.Errorf("expected 'xy' (distance 2) to be excluded by rune-based effectiveMax=1, but it was included")
	}
}
