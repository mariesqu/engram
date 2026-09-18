package localstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// SearchFilter carries the optional filter parameters for SearchMemoriesFiltered.
// All fields are optional — a zero SearchFilter is equivalent to calling
// SearchMemories with no filters other than project and limit.
type SearchFilter struct {
	// Type filters by observation type, e.g. "decision", "bugfix", "architecture".
	// Empty means "any type".
	Type string

	// Scope filters by scope: "project" or "personal".
	// Empty means "any scope".
	Scope string

	// TopicKey filters to a specific topic_key value.
	// Empty means "any topic".
	TopicKey string

	// Mode selects the retrieval strategy. Zero value and "fts" use the existing
	// FTS5/BM25 path (byte-identical to today's behavior). "semantic" uses cosine
	// scan only. "hybrid" fuses FTS and cosine via RRF (k=60). An unknown value
	// falls back to "fts" (safe default).
	//
	// Mode is additive — all existing callers that do not set Mode get the
	// identical FTS results they received before this field was added.
	Mode string

	// CreatedFrom/CreatedTo optionally bound created_at (inclusive on both
	// ends). The zero time.Time disables the corresponding bound. Both fields
	// are additive — existing callers that leave them zero see byte-identical
	// results to before these fields were added.
	//
	// Honored by EVERY mode: the FTS predicate bounds m.created_at and
	// SelectVectors bounds created_at on the cosine candidate scan, so a row
	// outside the window can reach no page through either half of a hybrid
	// fusion.
	CreatedFrom time.Time
	CreatedTo   time.Time

	// Offset skips the first Offset matching rows before LIMIT is applied —
	// used for the web UI's paged "load more" browsing. Zero (the default)
	// changes nothing versus before this field was added. Negative values are
	// treated as zero by callers that construct SQL from this filter.
	//
	// Honored by every mode, but at different layers: "fts" pushes it into SQL
	// OFFSET, while "semantic" and "hybrid" apply it to the FINAL ranked list
	// (after cosine ranking / after RRF fusion) because rank order does not
	// exist until scoring has run. See SearchMemoriesFiltered's filter-semantics
	// doc block.
	Offset int
}

// ftsRankExpr is the ORDER BY expression shared by the FTS-only path and the FTS
// half of hybrid, so the two can never rank the same corpus differently. It
// requires the memories table to be aliased `m` and the FTS table `fts`.
//
// The 1.10 is the pinned boost: 10%, ported from the upstream composite rank. It
// MULTIPLIES rather than adds because bm25() returns a NEGATIVE score (more
// negative = better match) and results are ordered ASC — scaling by 1.10 moves a
// pinned row further from zero, i.e. earlier. It is deliberately small: pinning
// should break a near-tie in the pinned row's favour, not drag an irrelevant
// memory to the top of an unrelated search.
//
// Only the pinned term of upstream's composite rank is ported. Upstream also
// multiplies in a recency term (from last_seen_at) and a stability term (from
// revision_count + duplicate_count); this schema has none of those three
// columns, so there is nothing to compute them from. Adding a recency term off
// updated_at instead would silently reorder every existing search result, which
// is not something a pinning feature gets to do — it belongs in its own change
// with its own before/after evidence.
//
// It is a hand-written const, not a Sprintf'd package var: a var built at init is
// a mutable global holding a string that never changes, and a const cannot be
// reassigned by anything — including a test.
//
// Cost note: `ORDER BY fts.rank * <expr>` gives up FTS5's rank pushdown. A bare
// `ORDER BY fts.rank` lets FTS5 return rows in rank order directly; multiplying
// it makes the sort key an expression SQLite must materialize and sort in a temp
// b-tree. Measured at 20k rows the difference is a wash (the temp sort is over
// the matched rows only, not the whole table), which is what buys the pinned
// boost — but it is a real change in query plan, so a future widening of the
// expression should be measured rather than assumed free.
const ftsRankExpr = "fts.rank * (CASE WHEN m.pinned = 1 THEN 1.10 ELSE 1.0 END)"

// hybridFullFusionFTSCap bounds the FTS candidate list on an OFFSET hybrid page.
//
// An offset page fuses the full candidate lists so the ranking does not depend on
// the offset (see SearchMemoriesFiltered's doc block). "Full" is literal for the
// cosine half — SelectVectors already scanned those rows — but an unbounded FTS
// half would let a one-word query pull every matching row in the store into
// memory to answer a ten-row page. 2000 is far past what any real paging session
// walks (200 pages of 10) while keeping the worst case a few megabytes, and a
// page that reaches beyond it returns the rows it can rather than erroring.
const hybridFullFusionFTSCap = 2000

// dateRangeSQL appends CreatedFrom/CreatedTo predicates (if set) to a WHERE
// clause being built for the given column expression (e.g. "m.created_at" or
// "created_at"), returning the updated query string and args. datetime(...)
// wraps both sides so the comparison is robust to the exact stored text
// format (SQLite's datetime() normalizes common ISO8601/SQL date-time forms).
func dateRangeSQL(q string, args []any, column string, f SearchFilter) (string, []any) {
	if !f.CreatedFrom.IsZero() {
		q += "\n  AND datetime(" + column + ") >= datetime(?)"
		args = append(args, f.CreatedFrom.UTC().Format(time.RFC3339))
	}
	if !f.CreatedTo.IsZero() {
		q += "\n  AND datetime(" + column + ") <= datetime(?)"
		args = append(args, f.CreatedTo.UTC().Format(time.RFC3339))
	}
	return q, args
}

// SearchDegradation records why a semantic/hybrid search fell back to FTS.
// It is returned alongside results so callers can emit an optional note.
type SearchDegradation struct {
	// Reason is a human-readable explanation for the fallback.
	// Empty string means no degradation occurred (semantic path worked).
	Reason string
}

// SearchMemoriesFiltered is the canonical search entry point supporting FTS,
// semantic, and hybrid retrieval modes.
//
// Filter semantics (all ANDed, all optional):
//   - project: LOWER(project) = lower(project) — case-insensitive; empty = all
//   - type:    exact match on the type column
//   - scope:   exact match on the scope column (normalized to lower)
//   - limit:   defaults to 10; max is not enforced here (caller is responsible)
//   - mode:    "" or "fts" → FTS only (byte-identical to before); "semantic" →
//     cosine only; "hybrid" → FTS + cosine → RRF(k=60); unknown → fts
//
// Date bounds and paging across modes:
//   - f.CreatedFrom / f.CreatedTo are pushed into SQL on BOTH halves — the FTS
//     predicate and SelectVectors' cosine candidate scan — so no mode can
//     surface a row outside the window. Pushing them down (rather than
//     post-filtering the ranked page) is load-bearing for "hybrid": RRF fuses
//     the two candidate lists, so an out-of-range row surviving the cosine half
//     would be re-admitted into the fused page the FTS half had excluded.
//   - f.Offset skips the first N rows of the FINAL ranked page. "fts" pushes it
//     into SQL OFFSET; "semantic" applies it after cosine ranking and "hybrid"
//     after RRF fusion, because neither has a rank order until scoring has run.
//     The guarantee the two paths make when Offset > 0 is that the RANKING IS
//     NOT A FUNCTION OF THE OFFSET: every offset page is cut from the same
//     ranking page one is cut from, so consecutive pages are disjoint and their
//     union is the unpaged top-(offset+limit). Widening a truncated pool by the
//     offset — which is what this used to do — does NOT give that: RRF scores
//     depend on each row's rank WITHIN the candidate lists, so changing how far
//     those lists are truncated reorders the fused result, and two pages drawn
//     from two different rankings can repeat rows and skip others. So an offset
//     page ranks the FULL candidate lists (every vector row for cosine; FTS up
//     to hybridFullFusionFTSCap) and slices [offset:offset+limit] out of it.
//     Offset == 0 keeps the historic narrow pool byte-for-byte — page one was
//     never wrong, and widening it would silently reorder every existing
//     first-page hybrid result.
//   - Paging is stable only for a fixed corpus and a fixed query: a concurrent
//     write, or an embedding backfill that adds a vector mid-scan, can shift
//     rows across the page boundary exactly as it can on the FTS path.
//   - When a semantic/hybrid search DEGRADES to FTS (see below), the fallback
//     is runFTS, which applies Offset in SQL — so paging survives degradation.
//
// FTS injection prevention: the query string is passed through sanitizeFTS
// (wraps each token in double-quotes) before reaching the FTS5 engine.
//
// Degradation: when mode is "semantic" or "hybrid" but the semantic path is
// unavailable (no embed fn, gated project, provider error, or no vectors),
// the search degrades to FTS and the returned SearchDegradation.Reason explains
// why. mode="" and "fts" never set Reason (hard constraint: keyless byte-identical).
func (s *Store) SearchMemoriesFiltered(query, project string, limit int, f SearchFilter) ([]*domain.Record, SearchDegradation, error) {
	if limit <= 0 {
		limit = 10
	}

	mode := f.Mode
	switch mode {
	case "", "fts", "semantic", "hybrid":
		// valid
	default:
		mode = "fts" // unknown → safe default
	}

	// ── FTS path (byte-identical to pre-PR-1 behavior) ──────────────────────
	// Mode "" and "fts" always take this path. "semantic" and "hybrid" fall through
	// to the FTS path on degradation.
	runFTS := func() ([]*domain.Record, error) {
		ftsQ := sanitizeFTS(query)
		if ftsQ == "" {
			return nil, nil
		}

		q := `
			SELECT m.id, m.sync_id, m.session_id, m.entity_type, m.type, m.title, m.content,
			       m.project, m.scope, m.version, m.writer_id, m.last_write_mutation_id,
			       m.topic_key, m.status, m.parent_sync_id,
			       m.created_at, m.updated_at, m.deleted_at
			FROM memories_fts fts
			JOIN memories m ON m.id = fts.rowid
			WHERE memories_fts MATCH ?
			  AND m.deleted_at IS NULL`
		args := []any{ftsQ}

		if project != "" {
			q += "\n  AND LOWER(m.project) = ?"
			args = append(args, strings.ToLower(strings.TrimSpace(project)))
		}
		if f.Type != "" {
			q += "\n  AND m.type = ?"
			args = append(args, f.Type)
		}
		if f.Scope != "" {
			q += "\n  AND m.scope = ?"
			args = append(args, strings.ToLower(strings.TrimSpace(f.Scope)))
		}
		if f.TopicKey != "" {
			q += "\n  AND m.topic_key = ?"
			args = append(args, f.TopicKey)
		}
		q, args = dateRangeSQL(q, args, "m.created_at", f)

		offset := f.Offset
		if offset < 0 {
			offset = 0
		}
		q += "\nORDER BY " + ftsRankExpr + "\nLIMIT ? OFFSET ?"
		args = append(args, limit, offset)

		rows, err := s.db.Query(q, args...)
		if err != nil {
			return nil, fmt.Errorf("SearchMemoriesFiltered: %w", err)
		}
		defer rows.Close()

		var results []*domain.Record
		for rows.Next() {
			r, scanErr := scanRecordWithIDFromRows(rows)
			if scanErr != nil {
				return nil, fmt.Errorf("SearchMemoriesFiltered scan: %w", scanErr)
			}
			results = append(results, r)
		}
		return results, rows.Err()
	}

	// Zero-value and explicit "fts" take the historic FTS-only path — no note.
	if mode == "" || mode == "fts" {
		results, err := runFTS()
		return results, SearchDegradation{}, err
	}

	// ── Semantic / hybrid path ───────────────────────────────────────────────
	// Read the embed function and dimensions under the read lock.
	s.embedFnMu.RLock()
	embedFn := s.embedFn
	dims := s.embedDims
	s.embedFnMu.RUnlock()

	// Degradation: no embed fn configured (Noop provider or missing key).
	if embedFn == nil || dims <= 0 {
		results, err := runFTS()
		reason := "semantic search unavailable: not configured; showing keyword results"
		return results, SearchDegradation{Reason: reason}, err
	}

	// Embed the query. The embed fn already encapsulates the privacy gate:
	// if the project is omitted or local-only+remote, it returns ErrEmbeddingGated.
	vecs, embedErr := embedFn(context.Background(), project, []string{query})
	if embedErr != nil || len(vecs) == 0 || len(vecs[0]) == 0 {
		results, err := runFTS()
		var reason string
		switch {
		case errors.Is(embedErr, ErrEmbeddingGated):
			reason = "semantic search unavailable for this project's policy; showing keyword results"
		case embedErr != nil:
			// Transient provider failure (network, 5xx, timeout) — NOT a policy
			// denial; telling the user "policy" here would be a lie.
			reason = "semantic search unavailable: provider error; showing keyword results"
		default:
			reason = "semantic search unavailable: provider returned no vector; showing keyword results"
		}
		return results, SearchDegradation{Reason: reason}, err
	}

	queryVec := l2Normalize(vecs[0])

	// Offset is applied to the ranked page below, never to the candidate scans.
	// Negative values mean "page one" (same normalization runFTS applies).
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	// ── Cosine-only ("semantic") path ────────────────────────────────────────
	if mode == "semantic" {
		vrows, svErr := SelectVectors(s.db, project, f, dims)
		if svErr != nil {
			results, err := runFTS()
			return results, SearchDegradation{Reason: "semantic search unavailable: vector scan error; showing keyword results"}, err
		}
		if len(vrows) == 0 {
			results, err := runFTS()
			return results, SearchDegradation{Reason: "semantic results not ready; showing keyword results"}, err
		}

		// Rank offset+limit candidates, then drop the first `offset` — the page
		// boundary only exists once the cosine scores have ordered the rows.
		topK := cosineTopK(queryVec, vrows, offset+limit)
		if len(topK) == 0 {
			results, err := runFTS()
			return results, SearchDegradation{Reason: "semantic results not ready; showing keyword results"}, err
		}
		if offset >= len(topK) {
			// Paged past the end of the ranked list. This is an EMPTY page, not a
			// degradation — falling back to FTS here would answer a "give me rows
			// 50-59" request with rows 0-9 of a different ranking.
			return nil, SearchDegradation{}, nil
		}
		topK = topK[offset:]

		syncIDs := make([]string, len(topK))
		for i, c := range topK {
			syncIDs[i] = c.syncID
		}
		records, err := s.fetchBySyncIDs(syncIDs)
		return records, SearchDegradation{}, err
	}

	// ── Hybrid path: FTS + cosine → RRF ─────────────────────────────────────
	// Page one keeps the historic pool: 2× the requested rows from each half.
	// An OFFSET page instead fuses the full candidate lists, because an RRF score
	// is a property of a row's rank within the lists it was fused from — make the
	// pool a function of the offset and each page is cut from a DIFFERENT ranking,
	// which is how pages end up repeating rows and skipping others. Fusing the
	// same full lists for every offset makes the ranking offset-independent, so
	// [offset:offset+limit] is a genuine slice of one ordering.
	ftsPool := limit * 2
	if offset > 0 {
		ftsPool = hybridFullFusionFTSCap
	}

	// Run FTS for the candidate pool sized above.
	ftsCandidates, ftsErr := func() ([]*domain.Record, error) {
		ftsQ := sanitizeFTS(query)
		if ftsQ == "" {
			return nil, nil
		}
		q := `
			SELECT m.id, m.sync_id, m.session_id, m.entity_type, m.type, m.title, m.content,
			       m.project, m.scope, m.version, m.writer_id, m.last_write_mutation_id,
			       m.topic_key, m.status, m.parent_sync_id,
			       m.created_at, m.updated_at, m.deleted_at
			FROM memories_fts fts
			JOIN memories m ON m.id = fts.rowid
			WHERE memories_fts MATCH ?
			  AND m.deleted_at IS NULL`
		args := []any{ftsQ}
		if project != "" {
			q += "\n  AND LOWER(m.project) = ?"
			args = append(args, strings.ToLower(strings.TrimSpace(project)))
		}
		if f.Type != "" {
			q += "\n  AND m.type = ?"
			args = append(args, f.Type)
		}
		if f.Scope != "" {
			q += "\n  AND m.scope = ?"
			args = append(args, strings.ToLower(strings.TrimSpace(f.Scope)))
		}
		if f.TopicKey != "" {
			q += "\n  AND m.topic_key = ?"
			args = append(args, f.TopicKey)
		}
		q, args = dateRangeSQL(q, args, "m.created_at", f)
		q += "\nORDER BY " + ftsRankExpr + "\nLIMIT ?"
		args = append(args, ftsPool)
		rows, err := s.db.Query(q, args...)
		if err != nil {
			return nil, fmt.Errorf("hybrid FTS: %w", err)
		}
		defer rows.Close()
		var res []*domain.Record
		for rows.Next() {
			r, e := scanRecordWithIDFromRows(rows)
			if e != nil {
				return nil, fmt.Errorf("hybrid FTS scan: %w", e)
			}
			res = append(res, r)
		}
		return res, rows.Err()
	}()
	if ftsErr != nil {
		results, err := runFTS()
		return results, SearchDegradation{Reason: "semantic search unavailable: FTS error; showing keyword results"}, err
	}

	// Scan the vector rows; the pool is cut from them below (see cosineK).
	vrows, svErr := SelectVectors(s.db, project, f, dims)
	if svErr != nil {
		results, err := runFTS()
		return results, SearchDegradation{Reason: "semantic search unavailable: vector scan error; showing keyword results"}, err
	}

	// Build rank lists (sync_id).
	ftsRanks := make([]string, len(ftsCandidates))
	ftsRecordsByID := make(map[string]*domain.Record, len(ftsCandidates))
	for i, r := range ftsCandidates {
		ftsRanks[i] = r.SyncID
		ftsRecordsByID[r.SyncID] = r
	}

	// The cosine half mirrors the FTS half: 2×limit on page one, every scanned
	// vector row on an offset page (SelectVectors has already applied the same
	// project/type/scope/date predicates, so "all of them" is still a bounded,
	// correctly-scoped list).
	cosineK := limit * 2
	if offset > 0 {
		cosineK = len(vrows)
	}
	cosineCandidates := cosineTopK(queryVec, vrows, cosineK)
	cosineRanks := make([]string, len(cosineCandidates))
	for i, c := range cosineCandidates {
		cosineRanks[i] = c.syncID
	}

	if len(cosineRanks) == 0 {
		// No vectors yet — RRF degenerates to FTS list.
		results, err := runFTS()
		reason := fmt.Sprintf("semantic results not ready (%d pending); showing keyword results", s.countNullEmbeddings())
		return results, SearchDegradation{Reason: reason}, err
	}

	// Fuse, then cut the requested page out of the fused ranking. Fusing to
	// offset+limit and slicing is the only order that gives a correct page:
	// RRF scores are a property of the fused list, so there is nothing to skip
	// until it exists. rrfFuse sorts first and truncates after, so asking it for
	// offset+limit returns a genuine PREFIX of the full fused ranking — which is
	// what makes [offset:] the same rows page one would have skipped.
	fusedIDs := rrfFuse(ftsRanks, cosineRanks, 60, offset+limit)
	if offset >= len(fusedIDs) {
		// Paged past the end of the fused ranking — an empty page, not a
		// degradation (see the semantic path for why the distinction matters).
		return nil, SearchDegradation{}, nil
	}
	fusedIDs = fusedIDs[offset:]

	// Build the result set from fused IDs. Records may come from FTS cache or
	// need a fresh fetch for cosine-only entries.
	result := make([]*domain.Record, 0, len(fusedIDs))
	var missingIDs []string
	for _, id := range fusedIDs {
		if r, ok := ftsRecordsByID[id]; ok {
			result = append(result, r)
		} else {
			missingIDs = append(missingIDs, id)
		}
	}
	if len(missingIDs) > 0 {
		extra, err := s.fetchBySyncIDs(missingIDs)
		if err != nil {
			return result, SearchDegradation{}, err
		}
		// Insert extras in fused order.
		extraMap := make(map[string]*domain.Record, len(extra))
		for _, r := range extra {
			extraMap[r.SyncID] = r
		}
		// Rebuild in exact fused order.
		ordered := make([]*domain.Record, 0, len(fusedIDs))
		for _, id := range fusedIDs {
			if r, ok := ftsRecordsByID[id]; ok {
				ordered = append(ordered, r)
			} else if r, ok := extraMap[id]; ok {
				ordered = append(ordered, r)
			}
		}
		return ordered, SearchDegradation{}, nil
	}

	// Reorder result to match fused order (FTS map hits may not be in fused order).
	ordered := make([]*domain.Record, 0, len(fusedIDs))
	for _, id := range fusedIDs {
		if r, ok := ftsRecordsByID[id]; ok {
			ordered = append(ordered, r)
		}
	}
	return ordered, SearchDegradation{}, nil
}

// fetchBySyncIDs retrieves records by sync_id, preserving the given order.
func (s *Store) fetchBySyncIDs(syncIDs []string) ([]*domain.Record, error) {
	if len(syncIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(syncIDs))
	placeholders = placeholders[:len(placeholders)-1]
	q := fmt.Sprintf(`
		SELECT id, sync_id, session_id, entity_type, type, title, content,
		       project, scope, version, writer_id, last_write_mutation_id,
		       topic_key, status, parent_sync_id,
		       created_at, updated_at, deleted_at
		FROM memories
		WHERE sync_id IN (%s) AND deleted_at IS NULL`, placeholders)
	args := make([]any, len(syncIDs))
	for i, id := range syncIDs {
		args[i] = id
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("fetchBySyncIDs: %w", err)
	}
	defer rows.Close()

	byID := make(map[string]*domain.Record, len(syncIDs))
	for rows.Next() {
		r, scanErr := scanRecordWithIDFromRows(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("fetchBySyncIDs scan: %w", scanErr)
		}
		byID[r.SyncID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fetchBySyncIDs rows: %w", err)
	}

	// Return in the caller's requested order.
	result := make([]*domain.Record, 0, len(syncIDs))
	for _, id := range syncIDs {
		if r, ok := byID[id]; ok {
			result = append(result, r)
		}
	}
	return result, nil
}

// countNullEmbeddings returns the count of live rows with NULL embedding.
// Used for the degradation message when no cosine candidates exist yet.
func (s *Store) countNullEmbeddings() int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE embedding IS NULL AND deleted_at IS NULL`).Scan(&n)
	return n
}

// pinFilter selects which side of the pinned flag a recent-observations query
// covers. It exists so the three variants share one query body instead of three
// near-identical copies that can drift.
type pinFilter int

const (
	pinAny      pinFilter = iota // no pinned predicate — every live row
	pinOnly                      // pinned = 1
	pinExcluded                  // pinned = 0
)

// RecentObservations returns the most recent live (non-deleted) memories ordered
// by created_at DESC, id DESC. project and scope are optional filters; an empty
// string disables the filter for that dimension. limit <= 0 defaults to 20.
//
// It ignores the pinned flag entirely — pinning changes where a memory is
// SURFACED, not whether it exists. Callers that want the split use
// PinnedObservations / RecentUnpinnedObservations.
func (s *Store) RecentObservations(project, scope string, limit int) ([]*domain.Record, error) {
	return s.recentObservations(project, scope, limit, pinAny)
}

// PinnedObservations returns the live PINNED memories for project/scope, newest
// first. Pinning is an explicit, hand-bounded act, but the caller still caps it
// (FormatContext at 20) so one over-enthusiastic pinning session cannot crowd
// every recent observation out of the context blob.
func (s *Store) PinnedObservations(project, scope string, limit int) ([]*domain.Record, error) {
	return s.recentObservations(project, scope, limit, pinOnly)
}

// RecentUnpinnedObservations is RecentObservations minus the pinned rows. It is
// the second half of the context split: pinned memories are rendered in their
// own section, so repeating them under "Recent Observations" would spend the
// caller's context window saying the same thing twice.
func (s *Store) RecentUnpinnedObservations(project, scope string, limit int) ([]*domain.Record, error) {
	return s.recentObservations(project, scope, limit, pinExcluded)
}

func (s *Store) recentObservations(project, scope string, limit int, pin pinFilter) ([]*domain.Record, error) {
	if limit <= 0 {
		limit = 20
	}
	project = normalizeProject(project)

	q := `
		SELECT id, sync_id, session_id, entity_type, type, title, content,
		       project, scope, version, writer_id, last_write_mutation_id,
		       topic_key, status, parent_sync_id,
		       created_at, updated_at, deleted_at
		FROM memories
		WHERE deleted_at IS NULL`
	args := []any{}

	switch pin {
	case pinOnly:
		q += "\n  AND pinned = 1"
	case pinExcluded:
		q += "\n  AND pinned = 0"
	case pinAny:
		// no predicate
	}
	if project != "" {
		q += "\n  AND LOWER(project) = ?"
		args = append(args, project)
	}
	if scope != "" {
		q += "\n  AND scope = ?"
		args = append(args, strings.ToLower(strings.TrimSpace(scope)))
	}

	q += "\nORDER BY datetime(created_at) DESC, id DESC\nLIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("RecentObservations: %w", err)
	}
	defer rows.Close()

	var results []*domain.Record
	for rows.Next() {
		r, err := scanRecordWithIDFromRows(rows)
		if err != nil {
			return nil, fmt.Errorf("RecentObservations scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// BrowseMemories returns live (non-deleted) memories ordered by
// created_at DESC, id DESC — the "no query" browse path used by the web UI's
// project drill-in modal. Unlike RecentObservations (project+scope only, no
// pagination), BrowseMemories additionally filters on f.Type, f.Scope,
// f.TopicKey, f.CreatedFrom/CreatedTo and supports offset-based paging via
// f.Offset, so the modal can lazily load pages of a project's memories under
// arbitrary filter combinations. limit <= 0 defaults to 20; negative offset is
// treated as 0.
//
// The project filter matches by LOWER(TRIM(project)) ONLY — mirroring the FTS
// path in SearchMemoriesFiltered — NOT the stricter normalizeProject (which
// also collapses repeated "--"/"__"). Using normalizeProject here would
// compare a collapsed query value against the UN-collapsed value stored in
// memories.project, silently dropping matches for any project name that
// happens to contain doubled separators.
func (s *Store) BrowseMemories(project string, f SearchFilter, limit int) ([]*domain.Record, error) {
	if limit <= 0 {
		limit = 20
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	project = strings.ToLower(strings.TrimSpace(project))

	q := `
		SELECT id, sync_id, session_id, entity_type, type, title, content,
		       project, scope, version, writer_id, last_write_mutation_id,
		       topic_key, status, parent_sync_id,
		       created_at, updated_at, deleted_at
		FROM memories
		WHERE deleted_at IS NULL`
	args := []any{}

	if project != "" {
		q += "\n  AND LOWER(project) = ?"
		args = append(args, project)
	}
	if f.Type != "" {
		q += "\n  AND type = ?"
		args = append(args, f.Type)
	}
	if f.Scope != "" {
		q += "\n  AND scope = ?"
		args = append(args, strings.ToLower(strings.TrimSpace(f.Scope)))
	}
	if f.TopicKey != "" {
		q += "\n  AND topic_key = ?"
		args = append(args, f.TopicKey)
	}
	q, args = dateRangeSQL(q, args, "created_at", f)

	q += "\nORDER BY datetime(created_at) DESC, id DESC\nLIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("BrowseMemories: %w", err)
	}
	defer rows.Close()

	var results []*domain.Record
	for rows.Next() {
		r, err := scanRecordWithIDFromRows(rows)
		if err != nil {
			return nil, fmt.Errorf("BrowseMemories scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// truncateStr truncates s to at most n runes. Used by FormatContext to produce
// bounded preview text.
func truncateStr(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

// contextPinnedLimit and contextRecentLimit are the two halves of FormatContext's
// observation budget, and they are stated together because what matters is the
// SUM: 10 + 20 = at most 30 bullets, each up to 300 characters, which is the most
// of a caller's context window mem_context is willing to spend on observations.
//
// Pinned is the smaller half deliberately. It was 20 — equal to recents — which
// meant a store with 20 pins rendered a context blob that was HALF pins, and
// pinning enough memories could push recent work down past where an agent
// actually reads. Ten is a shortlist; twenty is a second feed. The rows past the
// cap are not hidden: FormatContext prints how many were left out and where to
// find them (see CountPinned).
const (
	contextPinnedLimit = 10
	contextRecentLimit = 20
)

// writeObservationBullet renders one observation line. Shared by the "### Pinned"
// and "### Recent Observations" sections so the two can never drift into
// different shapes for the same data.
func writeObservationBullet(b *strings.Builder, obs *domain.Record) {
	fmt.Fprintf(b, "- [%s] **%s**: %s\n", obs.Type, obs.Title, truncateStr(obs.Content, 300))
}

// FormatContext assembles the agent-facing memory context blob from recent
// sessions and recent observations, mirroring the legacy predecessor's
// store.FormatContext.
//
// Format:
//
//	## Memory from Previous Sessions
//
//	### Recent Sessions
//	- **project** (started_at → last_activity_at)[: summary] [N observations]
//
//	### Pinned
//	- [type] **title**: content_preview
//
//	### Recent Observations
//	- [type] **title**: content_preview
//
// Pinned comes FIRST and Recent Observations excludes pinned rows. Both halves
// of that matter: a pin is the user saying "this one, always", so burying it in
// recency order defeats the point — and repeating it below would spend the
// caller's context window saying the same thing twice. The section is capped
// (contextPinnedLimit) so an over-enthusiastic pinning session cannot crowd out
// every recent observation, and when the cap bites it says so — an "…and N more
// pinned" line, because a silently truncated section leaves the caller believing
// they are looking at everything they pinned. It is omitted entirely when nothing
// is pinned, so a store that has never used mem_pin renders exactly as it always
// did.
//
// The session bullet carries BOTH timestamps because the list is ordered by
// last activity (see RecentSessions): showing only started_at would leave the
// ordering looking arbitrary — an older-started session correctly ranked first
// for having been worked in five minutes ago would read as a sorting bug. The
// arrow is omitted when the two are equal, so an idle session renders exactly
// as it always did.
//
// Returns an empty string when there are no sessions and no observations.
// project and scope are optional — empty string means "all".
func (s *Store) FormatContext(project, scope string) (string, error) {
	sessions, err := s.RecentSessions(project, 5)
	if err != nil {
		return "", fmt.Errorf("FormatContext: RecentSessions: %w", err)
	}

	pinned, err := s.PinnedObservations(project, scope, contextPinnedLimit)
	if err != nil {
		return "", fmt.Errorf("FormatContext: PinnedObservations: %w", err)
	}

	observations, err := s.RecentUnpinnedObservations(project, scope, contextRecentLimit)
	if err != nil {
		return "", fmt.Errorf("FormatContext: RecentUnpinnedObservations: %w", err)
	}

	if len(sessions) == 0 && len(pinned) == 0 && len(observations) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("## Memory from Previous Sessions\n\n")

	if len(sessions) > 0 {
		b.WriteString("### Recent Sessions\n")
		for _, sess := range sessions {
			summary := ""
			if sess.Summary != nil && *sess.Summary != "" {
				summary = fmt.Sprintf(": %s", truncateStr(*sess.Summary, 200))
			}
			// Count observations for this session. We use a best-effort query;
			// errors are silently ignored to avoid failing FormatContext on a
			// non-critical count.
			var obsCount int
			_ = s.db.QueryRow(
				`SELECT count(*) FROM memories WHERE session_id = ? AND deleted_at IS NULL`,
				sess.ID,
			).Scan(&obsCount)

			// Show the span the session actually covers. When nothing happened
			// after registration the two stamps coincide and only one is printed.
			started := sess.StartedAt.UTC().Format("2006-01-02 15:04:05")
			when := started
			if last := sess.LastActivityAt.UTC().Format("2006-01-02 15:04:05"); last != started && last != "" {
				when = started + " → " + last
			}

			fmt.Fprintf(&b, "- **%s** (%s)%s [%d observations]\n",
				sess.Project,
				when,
				summary,
				obsCount,
			)
		}
		b.WriteString("\n")
	}

	if len(pinned) > 0 {
		b.WriteString("### Pinned\n")
		for _, obs := range pinned {
			writeObservationBullet(&b, obs)
		}
		// Only count when the section is full — a short section IS the whole set,
		// and the extra query would answer a question nobody asked. A count error
		// is swallowed for the same reason the per-session COUNT above is: an
		// overflow footnote is not worth failing the whole context blob over.
		if len(pinned) >= contextPinnedLimit {
			if total, err := s.CountPinned(project, scope); err == nil && total > len(pinned) {
				fmt.Fprintf(&b, "- …and %d more pinned (use mem_search)\n", total-len(pinned))
			}
		}
		b.WriteString("\n")
	}

	if len(observations) > 0 {
		b.WriteString("### Recent Observations\n")
		for _, obs := range observations {
			writeObservationBullet(&b, obs)
		}
		b.WriteString("\n")
	}

	return b.String(), nil
}
