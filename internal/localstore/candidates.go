package localstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// ── Relation vocabulary ───────────────────────────────────────────────────────

// Relation verb constants — the locked set accepted by JudgeRelation.
// "pending" is the initial state, not a verdict.
const (
	RelationPending       = "pending"
	RelationRelated       = "related"
	RelationCompatible    = "compatible"
	RelationScoped        = "scoped"
	RelationConflictsWith = "conflicts_with"
	RelationSupersedes    = "supersedes"
	RelationNotConflict   = "not_conflict"
)

// Judgment status constants.
const (
	JudgmentStatusPending  = "pending"
	JudgmentStatusJudged   = "judged"
	JudgmentStatusOrphaned = "orphaned"
	JudgmentStatusIgnored  = "ignored"
)

// validRelationVerbs is the locked set of verbs that JudgeRelation accepts.
// "pending" is NOT in this set — it is the default state, not a verdict.
var validRelationVerbs = map[string]bool{
	RelationRelated:       true,
	RelationCompatible:    true,
	RelationScoped:        true,
	RelationConflictsWith: true,
	RelationSupersedes:    true,
	RelationNotConflict:   true,
}

func isValidRelationVerb(v string) bool { return validRelationVerbs[v] }

// ── Types ─────────────────────────────────────────────────────────────────────

// Candidate represents a potential conflict candidate surfaced by FindCandidates.
type Candidate struct {
	// ID is the integer primary key of the candidate observation.
	ID int64
	// SyncID is the TEXT sync_id of the candidate observation.
	SyncID string
	// Title is the candidate's title.
	Title string
	// Type is the candidate's observation type.
	Type string
	// TopicKey is the candidate's topic_key (may be nil).
	TopicKey *string
	// Score is the FTS5 BM25 rank (negative; MORE negative = better/stronger match).
	Score float64
	// JudgmentID is the sync_id of the pending conflict_relations row created for
	// this (source, candidate) pair. Empty when SkipInsert=true.
	JudgmentID string
}

// CandidateOptions controls the FindCandidates query.
type CandidateOptions struct {
	// BM25Floor is the MAXIMUM BM25 score to include (FTS5 bm25 is negative and more
	// negative = stronger; a candidate passes only when score <= floor). nil means
	// use the default (-2.0); a stricter (more negative) floor admits only stronger
	// matches, and 0.0 admits every match (no filtering).
	BM25Floor *float64
	// Limit caps the number of candidates returned. Default 3 when nil or <=0.
	Limit int
	// SkipInsert controls whether FindCandidates inserts pending conflict_relations
	// rows. When true, candidates are returned without any writes.
	SkipInsert bool
	// EmbedFn is an optional cosine-pass port. When non-nil, FindCandidates embeds
	// the saved observation's text, runs a cosine scan over stored vectors scoped
	// to the same project, and unions the cosine results with the FTS candidate set
	// by sync_id — surfacing paraphrases that share no keywords.
	// nil → FTS-only path unchanged (EmbedDims is ignored).
	EmbedFn EmbedQueryFn
	// EmbedDims is the expected embedding dimensionality. Required when EmbedFn is
	// non-nil; ignored otherwise. Passing 0 when EmbedFn is non-nil disables the
	// cosine pass (SelectVectors returns nil for dims ≤ 0).
	EmbedDims int
}

// ConflictRelation represents a row in conflict_relations.
type ConflictRelation struct {
	ID             int64    `json:"id"`
	SyncID         string   `json:"sync_id"`
	SourceID       string   `json:"source_id"`
	TargetID       string   `json:"target_id"`
	Relation       string   `json:"relation"`
	JudgmentStatus string   `json:"judgment_status"`
	Confidence     *float64 `json:"confidence,omitempty"`
	Reason         *string  `json:"reason,omitempty"`
	Evidence       *string  `json:"evidence,omitempty"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
}

// JudgeRelationParams holds the inputs for JudgeRelation.
type JudgeRelationParams struct {
	// JudgmentID is the sync_id of the conflict_relations row to update (required).
	JudgmentID string
	// Relation is the verdict verb (required); must be one of validRelationVerbs.
	Relation string
	// Reason is an optional free-text explanation.
	Reason *string
	// Evidence is optional free-form JSON or text evidence.
	Evidence *string
	// Confidence is an optional 0..1 confidence score.
	Confidence *float64
}

// ── sanitizeFTSCandidates ─────────────────────────────────────────────────────

// sanitizeFTSCandidates builds an OR-based FTS5 query from a title so that
// FindCandidates returns observations with ANY term overlap (broad recall).
//
// Unlike sanitizeFTS (which joins tokens with implicit AND for precise search),
// OR semantics ensure that a single overlapping word is enough to surface a
// candidate — BM25 ranking then orders by relevance. Port of old_code
// sanitizeFTSCandidates verbatim: split on whitespace, strip leading/trailing
// quotes from each token, wrap in "...", join with " OR ".
func sanitizeFTSCandidates(title string) string {
	words := strings.Fields(title)
	if len(words) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(words))
	for _, w := range words {
		// Strip ALL double-quotes (not just leading/trailing): an interior quote
		// (e.g. token JWT"auth) would otherwise produce "JWT"auth" — an unterminated
		// FTS5 string literal that errors the whole MATCH. Mirrors sanitizeFTS.
		w = strings.ReplaceAll(w, `"`, "")
		if w != "" {
			quoted = append(quoted, `"`+w+`"`)
		}
	}
	return strings.Join(quoted, " OR ")
}

// ── newRelSyncID ──────────────────────────────────────────────────────────────

// newRelSyncID generates a random sync_id for a conflict_relations row.
// Format: "rel-<16 hex chars>" (8 random bytes). Mirrors old_code newSyncID("rel").
func newRelSyncID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback: timestamp-based (practically unreachable on any modern OS).
		return fmt.Sprintf("rel-%d", time.Now().UTC().UnixNano())
	}
	return "rel-" + hex.EncodeToString(b)
}

// ── FindCandidates ────────────────────────────────────────────────────────────

// FindCandidates runs a post-save FTS5 candidate query for the observation
// identified by savedID and returns at most opts.Limit candidates that score
// above the BM25 floor.
//
// For each candidate (unless SkipInsert=true), a pending conflict_relations row
// is inserted and its sync_id is exposed as Candidate.JudgmentID.
//
// CRITICAL — SetMaxOpenConns(1) cursor safety: the FTS SELECT query opens a
// cursor that holds the single DB connection. Any INSERT or QueryRow while the
// cursor is still open would attempt a second connection → SQLITE_BUSY or
// deadlock. This function therefore drains ALL SELECT rows into a local slice
// and explicitly calls rows.Close() BEFORE issuing any write. Mirrors the fix
// in old_code FindCandidates.
//
// Errors from FindCandidates should be logged and swallowed by callers —
// candidate detection failure must never fail the originating save.
func (s *Store) FindCandidates(ctx context.Context, savedID int64, opts CandidateOptions) ([]Candidate, error) {
	// Apply defaults.
	limit := opts.Limit
	if limit <= 0 {
		limit = 3
	}
	floor := -2.0
	if opts.BM25Floor != nil {
		floor = *opts.BM25Floor
	}

	// Read the saved observation's title, project, and scope — needed to build the
	// FTS query and to scope candidates to the same project+scope.
	var title, project, scope string
	if err := s.db.QueryRow(
		`SELECT title, ifnull(project,''), scope FROM memories WHERE id = ?`, savedID,
	).Scan(&title, &project, &scope); err == sql.ErrNoRows {
		return nil, fmt.Errorf("FindCandidates: observation %d not found", savedID)
	} else if err != nil {
		return nil, fmt.Errorf("FindCandidates: get saved observation: %w", err)
	}

	ftsQuery := sanitizeFTSCandidates(title)
	if ftsQuery == "" {
		return nil, nil
	}

	// ── Phase 1: drain the FTS SELECT into a local slice ─────────────────────
	//
	// SetMaxOpenConns(1): the open rows cursor holds the single connection.
	// We MUST drain + close the cursor before any INSERT/QueryRow, or the
	// follow-up write will deadlock (SQLITE_BUSY on the single connection).
	// Fetch limit*3 rows so the Go-side BM25 floor filter still has enough
	// candidates after exclusions.
	rows, err := s.db.Query(`
		SELECT m.id, ifnull(m.sync_id,'') AS sync_id, m.title, m.type, m.topic_key,
		       fts.rank
		FROM memories_fts fts
		JOIN memories m ON m.id = fts.rowid
		WHERE memories_fts MATCH ?
		  AND m.id != ?
		  AND m.deleted_at IS NULL
		  AND ifnull(m.project,'') = ?
		  AND m.scope = ?
		ORDER BY fts.rank
		LIMIT ?
	`, ftsQuery, savedID, project, scope, limit*3)
	if err != nil {
		return nil, fmt.Errorf("FindCandidates: FTS5 query: %w", err)
	}

	type rawCandidate struct {
		id       int64
		syncID   string
		title    string
		obsType  string
		topicKey *string
		score    float64
	}

	var raw []rawCandidate
	for rows.Next() {
		var rc rawCandidate
		if err := rows.Scan(&rc.id, &rc.syncID, &rc.title, &rc.obsType, &rc.topicKey, &rc.score); err != nil {
			rows.Close() //nolint:errcheck
			return nil, fmt.Errorf("FindCandidates: scan: %w", err)
		}
		// Apply BM25 floor. FTS5 bm25 scores are NEGATIVE and MORE NEGATIVE = a
		// BETTER (stronger) match — verified empirically: a 4-word-overlap title
		// scores ~-5.9, a 1-word overlap ~-1.5. The floor is therefore a MAXIMUM
		// score: a candidate must be at least as relevant as the floor (score <=
		// floor). Drop anything weaker (score > floor, i.e. closer to 0).
		if rc.score > floor {
			continue
		}
		raw = append(raw, rc)
		if len(raw) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		return nil, fmt.Errorf("FindCandidates: rows error: %w", err)
	}
	// EXPLICIT close before any write — releases the single connection.
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("FindCandidates: close rows: %w", err)
	}

	// ── Phase 1b: optional cosine pass — union with FTS results ──────────────
	//
	// When EmbedFn is set, embed the source observation's text and cosine-scan
	// all stored vectors in the same project/scope. Candidates already in 'raw'
	// (from FTS) are preserved; cosine-only hits are fetched by sync_id and
	// appended up to 'limit'. This surfaces paraphrases that share no keywords.
	//
	// All SELECTs here must complete and close before Phase 3 (INSERTs) to
	// respect the SetMaxOpenConns(1) cursor discipline.
	// TRUE union: the cosine pass runs whenever it is wired — NOT only when FTS
	// came up short. A paraphrase sharing no keywords must surface even when FTS
	// already filled the limit with weaker lexical matches; the combined set is
	// truncated to the limit after dedup.
	if opts.EmbedFn != nil && opts.EmbedDims > 0 {
		// Build the source text: title + content (same as SelectEmbeddable).
		var srcContent string
		_ = s.db.QueryRow(`SELECT ifnull(content,'') FROM memories WHERE id = ?`, savedID).Scan(&srcContent)
		srcText := title
		if srcContent != "" {
			srcText = title + " " + srcContent
		}

		// Build a set of sync_ids already in the FTS result so we skip duplicates.
		inFTS := make(map[string]bool, len(raw))
		for _, rc := range raw {
			inFTS[rc.syncID] = true
		}

		// Embed the source text via the injected gated function. A gated or noop
		// response is treated as "cosine unavailable" — fall through silently.
		vecs, embedErr := opts.EmbedFn(ctx, project, []string{srcText})
		if embedErr == nil && len(vecs) > 0 && len(vecs[0]) == opts.EmbedDims {
			queryVec := l2Normalize(vecs[0])
			filter := SearchFilter{} // scope the scan to the same project only

			// SelectVectors opens and closes its own cursor before returning.
			storedVecs, _ := SelectVectors(s.db, project, filter, opts.EmbedDims)

			// Remove the source row itself from the cosine scan.
			var sourceSyncIDForCosine string
			_ = s.db.QueryRow(`SELECT ifnull(sync_id,'') FROM memories WHERE id = ?`, savedID).Scan(&sourceSyncIDForCosine)
			filtered := storedVecs[:0]
			for _, vr := range storedVecs {
				if vr.syncID != sourceSyncIDForCosine {
					filtered = append(filtered, vr)
				}
			}

			// Top-K cosine candidates (fetch up to limit*3 so we have enough after
			// dedup with the FTS set).
			cosineHits := cosineTopK(queryVec, filtered, limit*3)

			// Collect sync_ids that are new (not in FTS set).
			var newSyncIDs []string
			for _, cc := range cosineHits {
				if !inFTS[cc.syncID] {
					newSyncIDs = append(newSyncIDs, cc.syncID)
					if len(newSyncIDs) >= limit-len(raw) {
						break
					}
				}
			}

			// Fetch metadata for the cosine-only additions.  Build placeholders.
			if len(newSyncIDs) > 0 {
				placeholders := make([]string, len(newSyncIDs))
				args := make([]any, len(newSyncIDs))
				for i, sid := range newSyncIDs {
					placeholders[i] = "?"
					args[i] = sid
				}
				q := `SELECT id, sync_id, title, type, topic_key FROM memories
				       WHERE sync_id IN (` + strings.Join(placeholders, ",") + `)
				         AND deleted_at IS NULL`
				metaRows, metaErr := s.db.Query(q, args...)
				if metaErr == nil {
					for metaRows.Next() && len(raw) < limit {
						var rc rawCandidate
						if scanErr := metaRows.Scan(&rc.id, &rc.syncID, &rc.title, &rc.obsType, &rc.topicKey); scanErr == nil {
							// Assign a synthetic BM25 score of 0 so the cosine-only
							// candidates sort after FTS hits (BM25 is negative; 0 = weakest).
							rc.score = 0
							raw = append(raw, rc)
						}
					}
					_ = metaRows.Close()
				}
			}
		}
	}

	if len(raw) == 0 {
		return nil, nil
	}

	// ── Phase 2: return without writes when SkipInsert=true ──────────────────
	if opts.SkipInsert {
		candidates := make([]Candidate, 0, len(raw))
		for _, rc := range raw {
			candidates = append(candidates, Candidate{
				ID:       rc.id,
				SyncID:   rc.syncID,
				Title:    rc.title,
				Type:     rc.obsType,
				TopicKey: rc.topicKey,
				Score:    rc.score,
				// JudgmentID intentionally empty — no row written.
			})
		}
		return candidates, nil
	}

	// ── Phase 3: acquire write lock + insert pending conflict_relations rows ──
	//
	// FindCandidates writes to the DB (INSERTs conflict_relations rows), so it
	// MUST hold s.mu exactly like AddObservation/ApplyPulled/etc. The FTS SELECT
	// above runs lock-free (read-only); only the write phase needs the lock.
	s.mu.Lock()
	defer s.mu.Unlock()

	// Fetch the source observation's sync_id — needed as source_id in relation rows.
	var sourceSyncID string
	if err := s.db.QueryRow(
		`SELECT ifnull(sync_id,'') FROM memories WHERE id = ?`, savedID,
	).Scan(&sourceSyncID); err != nil {
		return nil, fmt.Errorf("FindCandidates: get source sync_id: %w", err)
	}
	if sourceSyncID == "" {
		// savedID vanished (or had its sync_id cleared) between the FTS phase and
		// here — never write relation rows with an empty source_id.
		return nil, fmt.Errorf("FindCandidates: source observation %d has no sync_id", savedID)
	}

	candidates := make([]Candidate, 0, len(raw))
	for _, rc := range raw {
		judgmentID := newRelSyncID()
		if _, err := s.db.Exec(`
			INSERT INTO conflict_relations
				(sync_id, source_id, target_id, relation, judgment_status, created_at, updated_at)
			VALUES (?, ?, ?, 'pending', 'pending', datetime('now'), datetime('now'))
		`, judgmentID, sourceSyncID, rc.syncID); err != nil {
			// Log-and-skip: a duplicate or constraint error on one candidate must
			// not abort detection for the remaining candidates.
			continue
		}
		candidates = append(candidates, Candidate{
			ID:         rc.id,
			SyncID:     rc.syncID,
			Title:      rc.title,
			Type:       rc.obsType,
			TopicKey:   rc.topicKey,
			Score:      rc.score,
			JudgmentID: judgmentID,
		})
	}

	return candidates, nil
}

// ── JudgeRelation ─────────────────────────────────────────────────────────────

// JudgeRelation records a verdict on an existing pending conflict_relations row.
// Re-judge is allowed (OVERWRITE semantics). Returns the updated row on success.
//
// Errors: returns an error if the JudgmentID is unknown or the relation verb is
// invalid.
func (s *Store) JudgeRelation(p JudgeRelationParams) (*ConflictRelation, error) {
	if !isValidRelationVerb(p.Relation) {
		return nil, fmt.Errorf("JudgeRelation: invalid relation verb %q — must be one of: related, compatible, scoped, conflicts_with, supersedes, not_conflict", p.Relation)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Update + check RowsAffected atomically under the lock — no separate pre-lock
	// existence check (which would be a TOCTOU window). 0 rows affected = not found.
	res, err := s.db.Exec(`
		UPDATE conflict_relations
		SET relation        = ?,
		    reason          = ?,
		    evidence        = ?,
		    confidence      = ?,
		    judgment_status = 'judged',
		    updated_at      = datetime('now')
		WHERE sync_id = ?
	`, p.Relation, p.Reason, p.Evidence, p.Confidence, p.JudgmentID)
	if err != nil {
		return nil, fmt.Errorf("JudgeRelation: update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("JudgeRelation: rows affected: %w", err)
	}
	if n == 0 {
		return nil, fmt.Errorf("JudgeRelation: relation %q not found", p.JudgmentID)
	}

	return s.GetRelation(p.JudgmentID)
}

// ── GetRelation ───────────────────────────────────────────────────────────────

// GetRelation retrieves a single conflict_relations row by its sync_id.
// This is a read-only operation — it does NOT acquire s.mu.
func (s *Store) GetRelation(syncID string) (*ConflictRelation, error) {
	row := s.db.QueryRow(`
		SELECT id, sync_id, source_id, target_id,
		       relation, judgment_status, confidence, reason, evidence,
		       created_at, updated_at
		FROM conflict_relations
		WHERE sync_id = ?
	`, syncID)

	var r ConflictRelation
	if err := row.Scan(
		&r.ID, &r.SyncID, &r.SourceID, &r.TargetID,
		&r.Relation, &r.JudgmentStatus, &r.Confidence, &r.Reason, &r.Evidence,
		&r.CreatedAt, &r.UpdatedAt,
	); err == sql.ErrNoRows {
		return nil, fmt.Errorf("GetRelation: relation %q not found", syncID)
	} else if err != nil {
		return nil, fmt.Errorf("GetRelation: %w", err)
	}
	return &r, nil
}
