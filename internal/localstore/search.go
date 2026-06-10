package localstore

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

		q += "\nORDER BY fts.rank\nLIMIT ?"
		args = append(args, limit)

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

		topK := cosineTopK(queryVec, vrows, limit)
		if len(topK) == 0 {
			results, err := runFTS()
			return results, SearchDegradation{Reason: "semantic results not ready; showing keyword results"}, err
		}

		syncIDs := make([]string, len(topK))
		for i, c := range topK {
			syncIDs[i] = c.syncID
		}
		records, err := s.fetchBySyncIDs(syncIDs)
		return records, SearchDegradation{}, err
	}

	// ── Hybrid path: FTS + cosine → RRF ─────────────────────────────────────
	// Run FTS with 2× candidates.
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
		q += "\nORDER BY fts.rank\nLIMIT ?"
		args = append(args, limit*2)
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

	// Cosine candidates with 2× candidates.
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

	cosineCandidates := cosineTopK(queryVec, vrows, limit*2)
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

	// Fuse and return top-limit results.
	fusedIDs := rrfFuse(ftsRanks, cosineRanks, 60, limit)

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

// RecentObservations returns the most recent live (non-deleted) memories ordered
// by created_at DESC, id DESC. project and scope are optional filters; an empty
// string disables the filter for that dimension. limit <= 0 defaults to 20.
//
// Mirrors old_code store.RecentObservations (project+scope variant).
func (s *Store) RecentObservations(project, scope string, limit int) ([]*domain.Record, error) {
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

// truncateStr truncates s to at most n runes. Used by FormatContext to produce
// bounded preview text.
func truncateStr(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

// FormatContext assembles the agent-facing memory context blob from recent
// sessions and recent observations, mirroring old_code store.FormatContext.
//
// Format (faithful to old_code):
//
//	## Memory from Previous Sessions
//
//	### Recent Sessions
//	- **project** (started_at)[: summary] [N observations]
//
//	### Recent Observations
//	- [type] **title**: content_preview
//
// Returns an empty string when there are no sessions and no observations.
// project and scope are optional — empty string means "all".
func (s *Store) FormatContext(project, scope string) (string, error) {
	sessions, err := s.RecentSessions(project, 5)
	if err != nil {
		return "", fmt.Errorf("FormatContext: RecentSessions: %w", err)
	}

	observations, err := s.RecentObservations(project, scope, 20)
	if err != nil {
		return "", fmt.Errorf("FormatContext: RecentObservations: %w", err)
	}

	if len(sessions) == 0 && len(observations) == 0 {
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

			fmt.Fprintf(&b, "- **%s** (%s)%s [%d observations]\n",
				sess.Project,
				sess.StartedAt.UTC().Format("2006-01-02 15:04:05"),
				summary,
				obsCount,
			)
		}
		b.WriteString("\n")
	}

	if len(observations) > 0 {
		b.WriteString("### Recent Observations\n")
		for _, obs := range observations {
			fmt.Fprintf(&b, "- [%s] **%s**: %s\n",
				obs.Type, obs.Title, truncateStr(obs.Content, 300))
		}
		b.WriteString("\n")
	}

	return b.String(), nil
}
