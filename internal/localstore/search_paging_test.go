package localstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// seedVectorRow adds an observation, backdates its created_at, and attaches an
// L2-normalized embedding built from the given sparse axis weights. It returns
// the sync_id so callers can assert on identity rather than title text.
func seedVectorRow(t *testing.T, s *Store, project, title, content string, createdAt time.Time, weights map[int]float32) string {
	t.Helper()

	res, err := s.AddObservation(AddObservationParams{
		Title:   title,
		Content: content,
		Project: project,
		Type:    "decision",
	})
	if err != nil {
		t.Fatalf("AddObservation(%q): %v", title, err)
	}

	if _, err := s.DB().Exec(
		`UPDATE memories SET created_at = ? WHERE sync_id = ?`,
		createdAt.UTC().Format("2006-01-02 15:04:05"), res.SyncID,
	); err != nil {
		t.Fatalf("backdate created_at for %q: %v", title, err)
	}

	if err := UpdateEmbedding(s.DB(), res.SyncID, testUnitVec(weights), "test-model",
		createdAt.UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("UpdateEmbedding(%q): %v", title, err)
	}
	return res.SyncID
}

// testUnitVec builds a testVecDims-wide L2-normalized vector from sparse axis
// weights, so a test can state "this row sits at cosine ≈ 0.99 from the query"
// by naming one or two axes instead of writing out 8 floats.
const testVecDims = 8

func testUnitVec(weights map[int]float32) []float32 {
	v := make([]float32, testVecDims)
	for axis, w := range weights {
		v[axis] = w
	}
	return l2Normalize(v)
}

// fixedQueryVec wires an embed fn that returns the same query vector for every
// text — the deterministic stand-in for a provider, so cosine scores in a test
// are a property of the seeded rows alone.
func fixedQueryVec(t *testing.T, s *Store, weights map[int]float32) {
	t.Helper()
	q := testUnitVec(weights)
	s.SetEmbedFn(func(_ context.Context, _ string, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			v := make([]float32, len(q))
			copy(v, q)
			out[i] = v
		}
		return out, nil
	}, testVecDims)
}

// containsSyncID reports whether a result set carries the given row.
func containsSyncID(records []*domain.Record, syncID string) bool {
	for _, r := range records {
		if r.SyncID == syncID {
			return true
		}
	}
	return false
}

// ── date bounds on the cosine half ───────────────────────────────────────────

// TestSearchFiltered_DateWindowExcludesStrongCosineMatch is the regression test
// for the bug this filter push-down fixes: SelectVectors used to ignore
// CreatedFrom/CreatedTo entirely, so the cosine half of a hybrid fusion
// re-admitted rows the FTS half had correctly excluded — a date-bounded search
// answered with out-of-range rows.
//
// The fixture makes the excluded row the STRONGEST possible cosine hit (its
// vector IS the query vector) so a leak cannot hide at the bottom of the page:
// if the bound is not applied, that row comes back first.
func TestSearchFiltered_DateWindowExcludesStrongCosineMatch(t *testing.T) {
	s := openTempStore(t)
	const project = "paging"

	old := time.Date(2020, 3, 1, 12, 0, 0, 0, time.UTC)
	recent := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	oldStrong := seedVectorRow(t, s, project, "alpha ledger", "alpha alpha alpha", old,
		map[int]float32{0: 1.0}) // cosine 1.0 with the query
	newWeak := seedVectorRow(t, s, project, "alpha sketch", "alpha filler filler", recent,
		map[int]float32{0: 0.3, 1: 1.0}) // same axis, much weaker

	fixedQueryVec(t, s, map[int]float32{0: 1.0})

	// Control: with no date bound the excluded row wins on cosine. Without this
	// the exclusion assertions below could pass for the wrong reason (e.g. the
	// row never being a candidate at all).
	for _, mode := range []string{"semantic", "hybrid"} {
		got, deg, err := s.SearchMemoriesFiltered("alpha", project, 10, SearchFilter{Mode: mode})
		if err != nil {
			t.Fatalf("%s control search: %v", mode, err)
		}
		if deg.Reason != "" {
			t.Fatalf("%s control search degraded: %s", mode, deg.Reason)
		}
		if !containsSyncID(got, oldStrong) {
			t.Fatalf("%s control: expected the unbounded search to return the strong old match", mode)
		}
	}

	// A window that starts after the strong match was created.
	window := SearchFilter{CreatedFrom: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}

	for _, mode := range []string{"semantic", "hybrid"} {
		window.Mode = mode
		got, deg, err := s.SearchMemoriesFiltered("alpha", project, 10, window)
		if err != nil {
			t.Fatalf("%s windowed search: %v", mode, err)
		}
		if deg.Reason != "" {
			t.Fatalf("%s windowed search degraded: %s", mode, deg.Reason)
		}
		if containsSyncID(got, oldStrong) {
			t.Errorf("%s: row created %s leaked past CreatedFrom=%s via the cosine candidate set",
				mode, old.Format("2006-01-02"), window.CreatedFrom.Format("2006-01-02"))
		}
		if !containsSyncID(got, newWeak) {
			t.Errorf("%s: in-window row is missing — the bound excluded too much", mode)
		}
	}

	// The upper bound is the mirror case: a window that ENDS before the strong
	// match would also have to drop it.
	for _, mode := range []string{"semantic", "hybrid"} {
		got, _, err := s.SearchMemoriesFiltered("alpha", project, 10, SearchFilter{
			Mode:      mode,
			CreatedTo: time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("%s CreatedTo search: %v", mode, err)
		}
		if containsSyncID(got, newWeak) {
			t.Errorf("%s: row created %s leaked past CreatedTo=2022-01-01",
				mode, recent.Format("2006-01-02"))
		}
	}
}

// ── offset paging on the fused path ──────────────────────────────────────────

// TestSearchFiltered_HybridOffsetPagesAreDisjoint pins the second half of the
// fix: Offset used to be an FTS-only feature, so a hybrid "load more" request
// silently re-served page one forever.
//
// The fixture makes the FTS and cosine rankings AGREE (term frequency descends
// A→F at a constant document length; cosine descends on the same axis), which
// makes the fused ranking prefix-stable and the page boundaries exact — the
// easy case. TestSearchFiltered_HybridOffsetPagesAreDisjoint_AntiCorrelated
// below covers the hard one, where the two halves rank the corpus in opposite
// orders and the pool width genuinely decides the answer.
func TestSearchFiltered_HybridOffsetPagesAreDisjoint(t *testing.T) {
	s := openTempStore(t)
	const project = "paging"
	const created = "2024-06-01"

	at := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	// Six rows of identical token length with descending "alpha" frequency, and
	// embeddings whose cosine with the query descends in the same order.
	want := make([]string, 6)
	for i := 0; i < 6; i++ {
		content := ""
		for j := 0; j < 6; j++ {
			if j < 6-i {
				content += "alpha "
			} else {
				content += fmt.Sprintf("pad%d ", j)
			}
		}
		want[i] = seedVectorRow(t, s, project,
			fmt.Sprintf("doc %d", i), content, at,
			map[int]float32{0: 1.0, 1: float32(i) * 0.25},
		)
	}

	fixedQueryVec(t, s, map[int]float32{0: 1.0})

	// The unpaged ranking is the reference every page is cut from.
	full, deg, err := s.SearchMemoriesFiltered("alpha", project, 6, SearchFilter{Mode: "hybrid"})
	if err != nil {
		t.Fatalf("unpaged hybrid search: %v", err)
	}
	if deg.Reason != "" {
		t.Fatalf("unpaged hybrid search degraded: %s", deg.Reason)
	}
	if len(full) != 6 {
		t.Fatalf("unpaged hybrid returned %d rows, want 6 (created %s)", len(full), created)
	}

	seen := map[string]int{}
	for page := 0; page < 3; page++ {
		offset := page * 2
		got, deg, err := s.SearchMemoriesFiltered("alpha", project, 2, SearchFilter{
			Mode:   "hybrid",
			Offset: offset,
		})
		if err != nil {
			t.Fatalf("hybrid page %d: %v", page, err)
		}
		if deg.Reason != "" {
			t.Fatalf("hybrid page %d degraded: %s", page, deg.Reason)
		}
		if len(got) != 2 {
			t.Fatalf("hybrid page %d returned %d rows, want 2", page, len(got))
		}
		for i, r := range got {
			if prev, dup := seen[r.SyncID]; dup {
				t.Errorf("hybrid page %d row %d (%s) already appeared on page %d — pages are not disjoint",
					page, i, r.SyncID, prev)
			}
			seen[r.SyncID] = page
			if wantID := full[offset+i].SyncID; r.SyncID != wantID {
				t.Errorf("hybrid page %d row %d = %s, want %s (position %d of the unpaged ranking)",
					page, i, r.SyncID, wantID, offset+i)
			}
		}
	}
	if len(seen) != 6 {
		t.Errorf("three pages of 2 covered %d distinct rows, want 6", len(seen))
	}

	// Paging past the end is an EMPTY page, not a silent fall back to page one —
	// answering "rows 100-109" with rows 0-9 is the bug this replaces.
	past, _, err := s.SearchMemoriesFiltered("alpha", project, 2, SearchFilter{Mode: "hybrid", Offset: 100})
	if err != nil {
		t.Fatalf("hybrid past-the-end page: %v", err)
	}
	if len(past) != 0 {
		t.Errorf("hybrid Offset=100 returned %d rows, want 0", len(past))
	}
}

// TestSearchFiltered_SemanticOffsetPagesAreDisjoint is the cosine-only mirror of
// the hybrid paging test. Ordering here is fully determined by the seeded
// vectors, so the expectation is exact with no fixture caveats.
func TestSearchFiltered_SemanticOffsetPagesAreDisjoint(t *testing.T) {
	s := openTempStore(t)
	const project = "paging"

	at := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	want := make([]string, 5)
	for i := 0; i < 5; i++ {
		want[i] = seedVectorRow(t, s, project,
			fmt.Sprintf("doc %d", i), "alpha filler", at,
			map[int]float32{0: 1.0, 1: float32(i) * 0.3},
		)
	}
	fixedQueryVec(t, s, map[int]float32{0: 1.0})

	seen := map[string]bool{}
	for page := 0; page < 2; page++ {
		offset := page * 2
		got, deg, err := s.SearchMemoriesFiltered("alpha", project, 2, SearchFilter{
			Mode:   "semantic",
			Offset: offset,
		})
		if err != nil {
			t.Fatalf("semantic page %d: %v", page, err)
		}
		if deg.Reason != "" {
			t.Fatalf("semantic page %d degraded: %s", page, deg.Reason)
		}
		if len(got) != 2 {
			t.Fatalf("semantic page %d returned %d rows, want 2", page, len(got))
		}
		for i, r := range got {
			if seen[r.SyncID] {
				t.Errorf("semantic page %d row %d (%s) already appeared on an earlier page", page, i, r.SyncID)
			}
			seen[r.SyncID] = true
			if r.SyncID != want[offset+i] {
				t.Errorf("semantic page %d row %d = %s, want %s", page, i, r.SyncID, want[offset+i])
			}
		}
	}

	past, _, err := s.SearchMemoriesFiltered("alpha", project, 2, SearchFilter{Mode: "semantic", Offset: 50})
	if err != nil {
		t.Fatalf("semantic past-the-end page: %v", err)
	}
	if len(past) != 0 {
		t.Errorf("semantic Offset=50 returned %d rows, want 0", len(past))
	}
}

// TestSearchFiltered_HybridOffsetPagesAreDisjoint_AntiCorrelated is the paging
// test with the training wheels off: the FTS ranking and the cosine ranking are
// exact REVERSES of each other, which is the fixture that exposes an
// offset-dependent candidate pool.
//
// Why anti-correlation is the discriminating case: an RRF score is a function of
// a row's rank WITHIN each candidate list. Truncate the lists at 2×(offset+limit)
// — the old sizing — and every page fuses a different pair of lists, so every
// page is a slice of a DIFFERENT ranking. When the two halves agree, the
// disagreement is invisible (both lists put the same rows on top whatever the
// cut). When they disagree, widening the pool admits rows that outrank what page
// one already returned, and the pages repeat rows and skip others. Here page 1
// under the old sizing re-served two rows page 0 had already shown.
//
// The assertions are the guarantee itself: pages are disjoint, and concatenated
// they equal the unpaged top-(pages × pageSize) ranking, row for row.
func TestSearchFiltered_HybridOffsetPagesAreDisjoint_AntiCorrelated(t *testing.T) {
	s := openTempStore(t)
	const project = "paging"
	const (
		rows     = 20
		pageSize = 3
		pages    = 4
	)

	at := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	// Row i carries (rows-i) "alpha" tokens padded to a constant length, so bm25
	// ranks doc 0 first and doc 19 last. Its vector leans on axis 1 by (rows-1-i)
	// steps away from the query's axis 0, so cosine ranks doc 19 first and doc 0
	// last: the exact reverse.
	for i := 0; i < rows; i++ {
		content := ""
		for j := 0; j < rows; j++ {
			if j < rows-i {
				content += "alpha "
			} else {
				content += fmt.Sprintf("pad%d ", j)
			}
		}
		seedVectorRow(t, s, project,
			fmt.Sprintf("doc %d", i), content, at,
			map[int]float32{0: 1.0, 1: float32(rows-1-i) * 0.25},
		)
	}

	fixedQueryVec(t, s, map[int]float32{0: 1.0})

	// Guard the fixture before trusting anything built on it: if bm25 or the
	// cosine scan ever stops ordering these rows as designed, the paging
	// assertions below would be measuring a corpus nobody intended.
	ftsOrder := searchSyncIDs(t, s, project, rows, SearchFilter{Mode: "fts"})
	cosineOrder := searchSyncIDs(t, s, project, rows, SearchFilter{Mode: "semantic"})
	if len(ftsOrder) != rows || len(cosineOrder) != rows {
		t.Fatalf("fixture: fts returned %d rows and cosine %d, want %d each",
			len(ftsOrder), len(cosineOrder), rows)
	}
	for i, id := range ftsOrder {
		if mirrored := cosineOrder[rows-1-i]; mirrored != id {
			t.Fatalf("fixture is not anti-correlated: fts[%d]=%s but cosine[%d]=%s",
				i, id, rows-1-i, mirrored)
		}
	}

	// The reference ranking: one unpaged call wide enough to cover every page.
	full := searchSyncIDs(t, s, project, pages*pageSize, SearchFilter{Mode: "hybrid"})
	if len(full) != pages*pageSize {
		t.Fatalf("unpaged hybrid returned %d rows, want %d", len(full), pages*pageSize)
	}

	// A page-0 request on its own must be the TOP of that same ranking. It is a
	// separate call with a smaller limit, and hybrid used to size its candidate
	// pool from that limit — so this call fused 2*pageSize rows per half while
	// `full` fused 2*pages*pageSize, and the two answered from different
	// rankings.
	top := searchSyncIDs(t, s, project, pageSize, SearchFilter{Mode: "hybrid"})
	if len(top) != pageSize {
		t.Fatalf("hybrid top-%d returned %d rows", pageSize, len(top))
	}
	for i := range top {
		if top[i] != full[i] {
			t.Errorf("hybrid top-%d row %d = %s, want %s — asking for fewer rows changed the ranking",
				pageSize, i, top[i], full[i])
		}
	}

	seen := map[string]int{}
	for page := 0; page < pages; page++ {
		offset := page * pageSize
		got := searchSyncIDs(t, s, project, pageSize, SearchFilter{Mode: "hybrid", Offset: offset})
		if len(got) != pageSize {
			t.Fatalf("hybrid page %d returned %d rows, want %d", page, len(got), pageSize)
		}
		for i, id := range got {
			if prev, dup := seen[id]; dup {
				t.Errorf("hybrid page %d row %d (%s) already appeared on page %d — "+
					"the fused ranking still depends on the offset", page, i, id, prev)
			}
			seen[id] = page
			if want := full[offset+i]; id != want {
				t.Errorf("hybrid page %d row %d = %s, want %s (position %d of the unpaged ranking)",
					page, i, id, want, offset+i)
			}
		}
	}
	if len(seen) != pages*pageSize {
		t.Errorf("%d pages of %d covered %d distinct rows, want %d",
			pages, pageSize, len(seen), pages*pageSize)
	}
}

// searchSyncIDs runs a search and projects the result to sync_ids, failing the
// test on an error or an unexpected degradation — the three things every paging
// assertion in this file wants and none of them want to spell out.
func searchSyncIDs(t *testing.T, s *Store, project string, limit int, f SearchFilter) []string {
	t.Helper()
	records, deg, err := s.SearchMemoriesFiltered("alpha", project, limit, f)
	if err != nil {
		t.Fatalf("SearchMemoriesFiltered(mode=%q, offset=%d): %v", f.Mode, f.Offset, err)
	}
	if deg.Reason != "" && f.Mode != "fts" {
		t.Fatalf("SearchMemoriesFiltered(mode=%q, offset=%d) degraded: %s", f.Mode, f.Offset, deg.Reason)
	}
	ids := make([]string, len(records))
	for i, r := range records {
		ids[i] = r.SyncID
	}
	return ids
}

// TestSearchFiltered_HybridPageZeroIsCutFromTheSameRankingAsPageOne is the
// test for the seam that used to sit between page 0 and page 1.
//
// Hybrid once fused a NARROW pool (2×limit from each half) when offset == 0 and
// the FULL lists when offset > 0, to keep page one byte-identical to the
// pre-paging release. Two pools mean two rankings, and an RRF score is a
// property of a row's rank within the lists it was fused from — so page 0 and
// page 1 were slices of different orderings, which is the one thing paging may
// not do.
//
// The fixture is built so the two pools genuinely disagree. 21 rows; FTS ranks
// them 0..20 by descending term frequency; cosine ranks them by a permutation
// that puts the FTS top six LAST, the FTS bottom six FIRST, and leaves the
// middle nine where they are. Row 6 — the "bridge" — therefore sits at rank 6
// in BOTH halves: outside a 6-wide pool entirely, but the single highest fused
// score once the full lists are ranked (2/(60+7) beats 1/(60+1) + 1/(60+16)).
//
// So: page 0 must contain the bridge. Under the narrow pool it could not, since
// the row was in neither candidate list.
func TestSearchFiltered_HybridPageZeroIsCutFromTheSameRankingAsPageOne(t *testing.T) {
	s := openTempStore(t)
	const project = "paging"
	const (
		rows     = 21
		pageSize = 3 // narrow pool would have been 2*3 = 6 per half
		bridge   = 6
	)

	at := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	// cosineRank maps an FTS position to its cosine position. A bijection on
	// 0..20 by construction: 0..5 → 15..20, 6..14 → 6..14, 15..20 → 0..5.
	cosineRank := func(p int) int {
		switch {
		case p < 6:
			return p + 15
		case p < 15:
			return p
		default:
			return p - 15
		}
	}

	ids := make([]string, rows)
	for p := 0; p < rows; p++ {
		content := ""
		for j := 0; j < rows; j++ {
			if j < rows-p {
				content += "alpha "
			} else {
				content += fmt.Sprintf("pad%d ", j)
			}
		}
		// Leaning further off the query's axis 0 means a lower cosine, so the
		// cosine position IS the lean.
		ids[p] = seedVectorRow(t, s, project,
			fmt.Sprintf("doc %d", p), content, at,
			map[int]float32{0: 1.0, 1: float32(cosineRank(p)) * 0.25},
		)
	}

	fixedQueryVec(t, s, map[int]float32{0: 1.0})

	// Guard the fixture: the whole argument rests on where the bridge row sits
	// in each half's ranking.
	ftsOrder := searchSyncIDs(t, s, project, rows, SearchFilter{Mode: "fts"})
	cosineOrder := searchSyncIDs(t, s, project, rows, SearchFilter{Mode: "semantic"})
	if len(ftsOrder) != rows || len(cosineOrder) != rows {
		t.Fatalf("fixture: fts returned %d rows and cosine %d, want %d each", len(ftsOrder), len(cosineOrder), rows)
	}
	for p := 0; p < rows; p++ {
		if ftsOrder[p] != ids[p] {
			t.Fatalf("fixture: fts rank %d is %s, want doc %d", p, ftsOrder[p], p)
		}
		if want := cosineRank(p); cosineOrder[want] != ids[p] {
			t.Fatalf("fixture: cosine rank %d is %s, want doc %d", want, cosineOrder[want], p)
		}
	}
	// The narrow pool was 2*pageSize per half. If the bridge were inside it, the
	// fixture would prove nothing.
	if pool := 2 * pageSize; bridge < pool {
		t.Fatalf("fixture: the bridge row is inside the historic %d-wide pool", pool)
	}

	page0 := searchSyncIDs(t, s, project, pageSize, SearchFilter{Mode: "hybrid"})
	if len(page0) != pageSize {
		t.Fatalf("page 0 returned %d rows, want %d", len(page0), pageSize)
	}
	if !containsID(page0, ids[bridge]) {
		t.Errorf("page 0 = %v does not contain the bridge row %s — page one is still cut from a narrower pool "+
			"than the pages after it", page0, ids[bridge])
	}

	// And the pages remain a partition of one ranking across the 0/1 boundary.
	unpaged := searchSyncIDs(t, s, project, 2*pageSize, SearchFilter{Mode: "hybrid"})
	if len(unpaged) != 2*pageSize {
		t.Fatalf("unpaged hybrid returned %d rows, want %d", len(unpaged), 2*pageSize)
	}
	page1 := searchSyncIDs(t, s, project, pageSize, SearchFilter{Mode: "hybrid", Offset: pageSize})
	joined := append(append([]string{}, page0...), page1...)
	if len(joined) != len(unpaged) {
		t.Fatalf("two pages of %d returned %d rows, want %d", pageSize, len(joined), len(unpaged))
	}
	for i := range unpaged {
		if joined[i] != unpaged[i] {
			t.Errorf("row %d of page 0+1 = %s, want %s (the unpaged ranking)", i, joined[i], unpaged[i])
		}
	}
	seen := map[string]bool{}
	for _, id := range joined {
		if seen[id] {
			t.Errorf("row %s appears on both pages", id)
		}
		seen[id] = true
	}
}

// containsID reports whether ids holds want.
func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
