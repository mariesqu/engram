package localstore

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
)

// ── Float32 ↔ BLOB codec ─────────────────────────────────────────────────────
//
// Vectors are stored as contiguous little-endian IEEE 754 float32 values.
// No length prefix, no framing — raw bytes only (matches the vector-search spec).
// Vectors are stored L2-normalized so cosine similarity equals dot product,
// eliminating per-query magnitude division on the hot path.

// encodeVector encodes a float32 slice to a little-endian byte slice.
// Each float32 occupies exactly 4 bytes.
func encodeVector(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// decodeVector decodes a little-endian byte slice to a float32 slice.
// dims is the expected number of float32 values (cross-dim safety guard).
// Returns an error when:
//   - len(b) is not divisible by 4 (corrupt/incomplete data), or
//   - len(b)/4 != dims (dimension mismatch — treat as stale, never cosine-compare).
func decodeVector(b []byte, dims int) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("decodeVector: blob length %d is not a multiple of 4", len(b))
	}
	got := len(b) / 4
	if got != dims {
		return nil, fmt.Errorf("decodeVector: blob has %d floats, want %d (dimension mismatch)", got, dims)
	}
	v := make([]float32, dims)
	for i := range v {
		bits := binary.LittleEndian.Uint32(b[i*4:])
		v[i] = math.Float32frombits(bits)
	}
	return v, nil
}

// L2Normalize returns a new vector with unit L2 norm.
// A zero-magnitude vector is returned as-is (all zeros → cosine undefined).
//
// Exported so the embedding backfill loop (internal/embedding) can normalize
// raw provider output before writing it via UpdateEmbedding. The codec stores
// vectors L2-normalized so cosine similarity equals dot product (decision 4).
func L2Normalize(v []float32) []float32 {
	return l2Normalize(v)
}

// l2Normalize is the unexported implementation used within this package.
func l2Normalize(v []float32) []float32 {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	if sum == 0 {
		out := make([]float32, len(v))
		copy(out, v)
		return out
	}
	mag := math.Sqrt(sum)
	out := make([]float32, len(v))
	for i, f := range v {
		out[i] = float32(float64(f) / mag)
	}
	return out
}

// dot computes the dot product of two equal-length float32 slices.
// When vectors are L2-normalized this equals the cosine similarity.
func dot(a, b []float32) float32 {
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return float32(sum)
}

// ── vectorRow ────────────────────────────────────────────────────────────────

// vectorRow is a row returned by SelectVectors: the sync_id and the decoded
// (already L2-normalized) embedding vector.
type vectorRow struct {
	syncID string
	vec    []float32
}

// SyncID returns the sync_id of this vector row. Exported for callers outside
// this package (e.g. mem_similar in cmd/engram/tools.go).
func (v vectorRow) SyncID() string { return v.syncID }

// VectorRow is an exported alias for vectorRow so package-external callers
// (cmd/engram mem_similar) can receive slices from SelectVectors.
type VectorRow = vectorRow

// ── SelectVectors ────────────────────────────────────────────────────────────

// SelectVectors queries all live (non-deleted) rows that have a non-NULL
// embedding, decoding each BLOB with the length-guard. Rows whose BLOB length
// does not match dims are silently skipped (they are stale — the backfill loop
// will re-embed them on the next pass). NaN-producing rows are also excluded.
//
// project and filter.Type/Scope predicates mirror the FTS path so the cosine
// scan is scoped the same way as a keyword search.
//
// dims must match the configured provider's Dimensions(). Passing 0 skips all
// rows (returns nil, nil) — safe when NoopProvider is active.
func SelectVectors(db *sql.DB, project string, filter SearchFilter, dims int) ([]vectorRow, error) {
	if dims <= 0 {
		return nil, nil
	}

	q := `SELECT sync_id, embedding FROM memories WHERE embedding IS NOT NULL AND deleted_at IS NULL`
	args := []any{}

	if project != "" {
		q += "\n  AND LOWER(project) = ?"
		args = append(args, strings.ToLower(strings.TrimSpace(project)))
	}
	if filter.Type != "" {
		q += "\n  AND type = ?"
		args = append(args, filter.Type)
	}
	if filter.Scope != "" {
		q += "\n  AND scope = ?"
		args = append(args, strings.ToLower(strings.TrimSpace(filter.Scope)))
	}
	if filter.TopicKey != "" {
		q += " AND topic_key = ?"
		args = append(args, filter.TopicKey)
	}

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("SelectVectors: query: %w", err)
	}
	defer rows.Close()

	var result []vectorRow
	for rows.Next() {
		var syncID string
		var blob []byte
		if err := rows.Scan(&syncID, &blob); err != nil {
			return nil, fmt.Errorf("SelectVectors: scan: %w", err)
		}
		v, err := decodeVector(blob, dims)
		if err != nil {
			// Dimension mismatch or corrupt blob: skip this row.
			// The backfill loop will re-embed it when its model/dims change.
			continue
		}
		// Guard against NaN-producing rows (all-zeros were already normalized to
		// all-zeros by the write path; any NaN in the stored blob is corrupt).
		hasNaN := false
		for _, f := range v {
			if math.IsNaN(float64(f)) {
				hasNaN = true
				break
			}
		}
		if hasNaN {
			continue
		}
		result = append(result, vectorRow{syncID: syncID, vec: v})
	}
	return result, rows.Err()
}

// ── cosineTopK ───────────────────────────────────────────────────────────────

// cosineCandidate pairs a sync_id with its cosine similarity score.
type cosineCandidate struct {
	syncID string
	score  float32
}

// SyncID returns the candidate's sync_id.
func (c cosineCandidate) SyncID() string { return c.syncID }

// Score returns the candidate's cosine similarity score.
func (c cosineCandidate) Score() float32 { return c.score }

// CosineCandidate is an exported alias for cosineCandidate so package-external
// callers (cmd/engram mem_similar) can receive slices from CosineTopK.
type CosineCandidate = cosineCandidate

// CosineTopK is the exported variant of cosineTopK for use by package-external
// callers such as mem_similar in cmd/engram/tools.go.
func CosineTopK(queryVec []float32, rows []VectorRow, k int) []CosineCandidate {
	return cosineTopK(queryVec, rows, k)
}

// cosineTopK computes the dot product between queryVec (must be L2-normalized)
// and each stored vector (pre-normalized at write time), sorts descending by
// score, and returns the top k results.
//
// Tie-break: equal scores → sync_id ascending (lexicographic, deterministic).
// Zero-magnitude query vector → all scores 0.0; rows with zero score are excluded.
func cosineTopK(queryVec []float32, rows []vectorRow, k int) []cosineCandidate {
	if len(queryVec) == 0 || len(rows) == 0 || k <= 0 {
		return nil
	}

	// Check for zero-magnitude query (all-zeros after normalization).
	allZero := true
	for _, f := range queryVec {
		if f != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return nil
	}

	candidates := make([]cosineCandidate, 0, len(rows))
	for _, r := range rows {
		score := dot(queryVec, r.vec)
		if score <= 0 {
			continue // zero-magnitude stored vector or negative similarity → skip
		}
		candidates = append(candidates, cosineCandidate{syncID: r.syncID, score: score})
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].syncID < candidates[j].syncID
	})

	if k < len(candidates) {
		return candidates[:k]
	}
	return candidates
}

// ── RRF fusion ───────────────────────────────────────────────────────────────

// rrfFuse fuses two ranked sync_id lists using Reciprocal Rank Fusion (k=60).
//
// Formula: score(d) = Σ 1/(k + rank_i(d)) over both lists (ranks are 1-based).
// A document appearing in only one list contributes one term.
// Tie-break: equal fused scores → sync_id ascending.
//
// Returns the top limit sync_ids by fused score.
//
// Design rationale (decision 8): RRF operates on ranks, so the unbounded BM25
// score and the bounded [0,1] cosine score combine without normalization or
// weight tuning. k=60 is the standard recommendation.
func rrfFuse(ftsRanks []string, cosineRanks []string, k, limit int) []string {
	if k <= 0 {
		k = 60
	}

	type entry struct {
		syncID string
		score  float64
	}

	scores := make(map[string]float64)

	for i, id := range ftsRanks {
		rank := i + 1 // 1-based
		scores[id] += 1.0 / float64(k+rank)
	}
	for i, id := range cosineRanks {
		rank := i + 1 // 1-based
		scores[id] += 1.0 / float64(k+rank)
	}

	// Collect and sort by fused score desc, sync_id asc for tie-break.
	entries := make([]entry, 0, len(scores))
	for id, sc := range scores {
		entries = append(entries, entry{syncID: id, score: sc})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].score != entries[j].score {
			return entries[i].score > entries[j].score
		}
		return entries[i].syncID < entries[j].syncID
	})

	// Return top limit sync_ids.
	if limit <= 0 || limit > len(entries) {
		limit = len(entries)
	}
	out := make([]string, limit)
	for i := range out {
		out[i] = entries[i].syncID
	}
	return out
}

// ── SelectEmbeddable ─────────────────────────────────────────────────────────

// EmbeddableRow represents a row eligible for embedding, returned by SelectEmbeddable.
// Fields are exported so the embedding backfill loop (internal/embedding) can read them.
type EmbeddableRow struct {
	ID      int64 // rowid keyset cursor — the backfill pages with id > afterID
	SyncID  string
	Project string
	Text    string // title + " " + content for embedding
}

// SelectEmbeddable returns rows that need (re-)embedding:
//   - embedding IS NULL (never embedded), OR
//   - embedding_model != currentModel (stale — model changed).
//
// Deleted rows are excluded. limit caps the batch size.
//
// afterID is a KEYSET CURSOR: only rows with id > afterID are returned, in id
// order. The backfill loop threads the last-seen id between pages so the tick
// always ADVANCES past permanently-gated rows — without it, 100+ gated rows
// with the lowest ids would fill every page and STARVE all eligible rows
// behind them forever. Pass 0 to start from the beginning.
func SelectEmbeddable(db *sql.DB, currentModel string, limit int, afterID int64) ([]EmbeddableRow, error) {
	if limit <= 0 {
		limit = 100
	}
	const q = `
		SELECT id, sync_id, project, title, content
		FROM memories
		WHERE deleted_at IS NULL
		  AND id > ?
		  AND (embedding IS NULL OR embedding_model IS NULL OR embedding_model != ?)
		ORDER BY id ASC
		LIMIT ?`

	rows, err := db.Query(q, afterID, currentModel, limit)
	if err != nil {
		return nil, fmt.Errorf("SelectEmbeddable: %w", err)
	}
	defer rows.Close()

	var result []EmbeddableRow
	for rows.Next() {
		var id int64
		var syncID, project, title, content string
		if err := rows.Scan(&id, &syncID, &project, &title, &content); err != nil {
			return nil, fmt.Errorf("SelectEmbeddable scan: %w", err)
		}
		text := title
		if content != "" {
			text = title + " " + content
		}
		result = append(result, EmbeddableRow{ID: id, SyncID: syncID, Project: project, Text: text})
	}
	return result, rows.Err()
}

// UpdateEmbedding updates the embedding for a single row identified by syncID
// only when the row has no existing embedding (embedding IS NULL).
//
// The UPDATE is intentionally a SINGLE STATEMENT and does NOT take s.mu.
// Rationale (design decision 3): AddObservation holds s.mu across the
// multi-statement read-modify-write (version pre-read → localWriteLocked →
// PK resolve) because an interleaved ApplyPulled would corrupt the LWW version.
// UpdateEmbedding is a single-statement UPDATE on derived columns
// (embedding, embedding_model, embedding_created_at) that no reconciliation
// path reads. SQLite WAL + SetMaxOpenConns(1) serialize the physical write.
// The AND embedding IS NULL guard makes this idempotent: if a concurrent write
// already embedded this row, this UPDATE is a safe no-op (0 rows affected).
//
// For model-change re-embedding use UpdateEmbeddingStale, which also overwrites
// rows whose embedding_model no longer matches the active model.
//
// vec must be L2-normalized before calling UpdateEmbedding.
func UpdateEmbedding(db *sql.DB, syncID string, vec []float32, model, ts string) error {
	blob := encodeVector(vec)
	_, err := db.Exec(
		`UPDATE memories
		    SET embedding = ?,
		        embedding_model = ?,
		        embedding_created_at = ?
		  WHERE sync_id = ?
		    AND embedding IS NULL`,
		blob, model, ts, syncID,
	)
	if err != nil {
		return fmt.Errorf("UpdateEmbedding %q: %w", syncID, err)
	}
	return nil
}

// UpdateEmbeddingStale is identical to UpdateEmbedding but also overwrites rows
// whose embedding_model differs from model (i.e. stale — model changed).
//
// Used exclusively by the backfill loop, which selects rows where embedding IS
// NULL OR embedding_model != currentModel and must be able to re-embed both.
//
// A concurrent write that already stored the new model causes 0 rows affected,
// which is a safe no-op.
//
// vec must be L2-normalized before calling UpdateEmbeddingStale.
func UpdateEmbeddingStale(db *sql.DB, syncID string, vec []float32, model, ts string) error {
	blob := encodeVector(vec)
	_, err := db.Exec(
		`UPDATE memories
		    SET embedding = ?,
		        embedding_model = ?,
		        embedding_created_at = ?
		  WHERE sync_id = ?
		    AND (embedding IS NULL OR embedding_model IS NULL OR embedding_model != ?)`,
		blob, model, ts, syncID, model,
	)
	if err != nil {
		return fmt.Errorf("UpdateEmbeddingStale %q: %w", syncID, err)
	}
	return nil
}

// ErrNoEmbedding is returned by GetEmbeddingBySyncID when the row exists but
// has no embedding stored (embedding IS NULL).
var ErrNoEmbedding = fmt.Errorf("observation has no embedding vector")

// GetEmbeddingBySyncID returns the stored (L2-normalized) embedding vector for
// the row with the given sync_id. dims is the expected dimensionality; a
// mismatched BLOB returns an error.
//
// Returns ErrNoEmbedding when the row exists but has embedding = NULL.
// Returns sql.ErrNoRows (wrapped) when the sync_id is not found.
func GetEmbeddingBySyncID(db *sql.DB, syncID string, dims int) ([]float32, error) {
	var blob []byte
	err := db.QueryRow(
		`SELECT embedding FROM memories WHERE sync_id = ? AND deleted_at IS NULL`,
		syncID,
	).Scan(&blob)
	if err != nil {
		return nil, fmt.Errorf("GetEmbeddingBySyncID %q: %w", syncID, err)
	}
	if blob == nil {
		return nil, ErrNoEmbedding
	}
	v, decErr := decodeVector(blob, dims)
	if decErr != nil {
		return nil, fmt.Errorf("GetEmbeddingBySyncID %q: %w", syncID, decErr)
	}
	return v, nil
}
