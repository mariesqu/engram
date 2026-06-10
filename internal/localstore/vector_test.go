package localstore

import (
	"database/sql"
	"math"
	"path/filepath"
	"testing"
)

// ── Task 2.1: Codec + math ────────────────────────────────────────────────────

// TestCodec_RoundTrip_Cosine1 encodes a vector, decodes it, then checks dot == 1.0.
// Uses an L2-normalized input so dot(v, decode(encode(v))) == cosine(v, v) == 1.0.
func TestCodec_RoundTrip_Cosine1(t *testing.T) {
	v := []float32{0.6, 0.8} // already unit: 0.36+0.64=1
	encoded := encodeVector(v)
	decoded, err := decodeVector(encoded, 2)
	if err != nil {
		t.Fatalf("decodeVector: %v", err)
	}
	result := dot(decoded, v)
	if math.Abs(float64(result)-1.0) > 1e-6 {
		t.Errorf("cosine round-trip = %v, want ~1.0", result)
	}
}

// TestCodec_DimMismatch_Error checks that mismatched dims return an error.
func TestCodec_DimMismatch_Error(t *testing.T) {
	v := []float32{1.0, 0.0, 0.0}
	encoded := encodeVector(v)
	// Request dims=4 but blob has 3 floats.
	_, err := decodeVector(encoded, 4)
	if err == nil {
		t.Fatal("expected error for dim mismatch, got nil")
	}
}

// TestCodec_NotMultipleOf4_Error checks that a malformed blob (len%4 != 0) errors.
func TestCodec_NotMultipleOf4_Error(t *testing.T) {
	blob := []byte{0x01, 0x02, 0x03} // 3 bytes — not a multiple of 4
	_, err := decodeVector(blob, 0)
	if err == nil {
		t.Fatal("expected error for non-multiple-of-4 blob, got nil")
	}
}

// TestCodec_LittleEndian_ByteLayout checks the exact little-endian byte layout
// for a known float32 value (1.0 = 0x3F800000 in IEEE 754).
func TestCodec_LittleEndian_ByteLayout(t *testing.T) {
	v := []float32{1.0}
	blob := encodeVector(v)
	if len(blob) != 4 {
		t.Fatalf("expected 4 bytes, got %d", len(blob))
	}
	// 1.0 in little-endian: 00 00 80 3F
	want := []byte{0x00, 0x00, 0x80, 0x3F}
	for i, b := range want {
		if blob[i] != b {
			t.Errorf("byte[%d] = 0x%02X, want 0x%02X", i, blob[i], b)
		}
	}
}

// TestDot_OrthogonalVectors_Zero checks that orthogonal vectors dot to 0.
func TestDot_OrthogonalVectors_Zero(t *testing.T) {
	a := []float32{1, 0}
	b := []float32{0, 1}
	result := dot(a, b)
	if math.Abs(float64(result)) > 1e-7 {
		t.Errorf("dot(orthogonal) = %v, want 0", result)
	}
}

// TestDot_ParallelVectors_One checks that a unit vector dotted with itself is 1.
func TestDot_ParallelVectors_One(t *testing.T) {
	a := []float32{0.6, 0.8}
	result := dot(a, a)
	if math.Abs(float64(result)-1.0) > 1e-6 {
		t.Errorf("dot(unit vec with itself) = %v, want 1.0", result)
	}
}

// ── Task 2.2: SelectVectors + cosineTopK ─────────────────────────────────────

// openTestDB opens a temporary SQLite database with the full schema applied.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		t.Fatalf("WAL pragma: %v", err)
	}
	if err := ApplySchema(db); err != nil {
		t.Fatalf("ApplySchema: %v", err)
	}
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// insertMemoryWithEmbedding inserts a row into memories with the given embedding blob.
// If blob is nil the row is inserted with NULL embedding.
func insertMemoryWithEmbedding(t *testing.T, db *sql.DB, syncID, project string, blob []byte) {
	t.Helper()
	const q = `
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content, project, scope,
		   version, writer_id, last_write_mutation_id, created_at, updated_at, embedding, embedding_model)
		VALUES (?, 'sess1', 'memory', 'manual', 'title', 'content', ?, 'project',
		        1, 'w1', 'mut1', datetime('now'), datetime('now'), ?, 'test-model')`
	_, err := db.Exec(q, syncID, project, blob)
	if err != nil {
		t.Fatalf("insert memory %q: %v", syncID, err)
	}
}

// TestSelectVectors_FiltersNullEmbedding checks that rows with NULL embedding
// are excluded from SelectVectors results.
func TestSelectVectors_FiltersNullEmbedding(t *testing.T) {
	db := openTestDB(t)
	// Row with embedding
	v := l2Normalize([]float32{1, 0})
	insertMemoryWithEmbedding(t, db, "has-embed", "proj", encodeVector(v))
	// Row without embedding
	insertMemoryWithEmbedding(t, db, "no-embed", "proj", nil)

	rows, err := SelectVectors(db, "proj", SearchFilter{}, 2)
	if err != nil {
		t.Fatalf("SelectVectors: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].syncID != "has-embed" {
		t.Errorf("got syncID %q, want has-embed", rows[0].syncID)
	}
}

// TestCosineTopK_MostSimilarFirst checks that cosineTopK returns the most
// similar row first.
func TestCosineTopK_MostSimilarFirst(t *testing.T) {
	query := []float32{1, 0} // unit vector pointing along first axis

	rows := []vectorRow{
		{syncID: "a", vec: l2Normalize([]float32{0, 1})},   // orthogonal → score=0
		{syncID: "b", vec: l2Normalize([]float32{1, 0})},   // parallel → score=1
		{syncID: "c", vec: l2Normalize([]float32{1, 0.1})}, // close → score~0.995
	}

	// Normalize query too.
	qNorm := l2Normalize(query)
	candidates := cosineTopK(qNorm, rows, 3)

	if len(candidates) == 0 {
		t.Fatal("expected at least 1 candidate")
	}
	// "b" should be first (score=1.0)
	if candidates[0].syncID != "b" {
		t.Errorf("top-1 = %q, want b", candidates[0].syncID)
	}
}

// TestCosineTopK_ZeroMagnitude_Excluded checks that zero-magnitude query
// returns nil (zero-magnitude stored vectors get score 0.0 and are excluded).
func TestCosineTopK_ZeroMagnitude_Excluded(t *testing.T) {
	query := []float32{0, 0} // zero-magnitude — all-zeros after normalization
	rows := []vectorRow{
		{syncID: "a", vec: l2Normalize([]float32{1, 0})},
	}
	result := cosineTopK(query, rows, 3)
	if len(result) != 0 {
		t.Errorf("expected 0 candidates for zero-magnitude query, got %d", len(result))
	}
}

// TestCosineTopK_TieBreak_SyncIDOrder checks that ties are broken by sync_id ascending.
func TestCosineTopK_TieBreak_SyncIDOrder(t *testing.T) {
	query := l2Normalize([]float32{1, 0})
	// Two rows with identical scores (identical vectors).
	rows := []vectorRow{
		{syncID: "z-row", vec: l2Normalize([]float32{1, 0})},
		{syncID: "a-row", vec: l2Normalize([]float32{1, 0})},
	}
	candidates := cosineTopK(query, rows, 2)
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	if candidates[0].syncID != "a-row" {
		t.Errorf("tie-break: top-1 = %q, want a-row", candidates[0].syncID)
	}
}

// ── Task 2.3: RRF fusion ──────────────────────────────────────────────────────

// TestRRF_DocInBothLists_ScoresHigher checks the spec scenario:
// doc-2 (rank 2 in FTS, rank 1 in cosine) beats doc-1 (rank 1 in FTS, rank 3 in cosine).
func TestRRF_DocInBothLists_ScoresHigher(t *testing.T) {
	// list A (FTS): [doc-1, doc-2, doc-3]
	// list B (cosine): [doc-2, doc-4, doc-1]
	fts := []string{"doc-1", "doc-2", "doc-3"}
	cos := []string{"doc-2", "doc-4", "doc-1"}

	result := rrfFuse(fts, cos, 60, 4)

	// doc-2: 1/(60+2) + 1/(60+1) = 1/62 + 1/61 ≈ 0.03217
	// doc-1: 1/(60+1) + 1/(60+3) = 1/61 + 1/63 ≈ 0.02421
	// So doc-2 must beat doc-1.
	if len(result) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(result))
	}
	if result[0] != "doc-2" {
		t.Errorf("top-1 = %q, want doc-2 (appears in both lists)", result[0])
	}
}

// TestRRF_DocInOneList_OneContribution checks that a doc in only one list
// contributes one RRF term.
func TestRRF_DocInOneList_OneContribution(t *testing.T) {
	fts := []string{"doc-a"}
	cos := []string{}

	result := rrfFuse(fts, cos, 60, 5)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	if result[0] != "doc-a" {
		t.Errorf("got %q, want doc-a", result[0])
	}
}

// TestRRF_TieBreak_Deterministic checks that equal RRF scores produce a
// deterministic sync_id ascending order.
func TestRRF_TieBreak_Deterministic(t *testing.T) {
	// Two docs, each in exactly one list at rank 1 — identical RRF score.
	fts := []string{"z-doc"}
	cos := []string{"a-doc"}

	r1 := rrfFuse(fts, cos, 60, 2)
	r2 := rrfFuse(fts, cos, 60, 2)

	if len(r1) != 2 || len(r2) != 2 {
		t.Fatalf("expected 2 results each, got %d, %d", len(r1), len(r2))
	}
	// Both calls must return the same order.
	if r1[0] != r2[0] || r1[1] != r2[1] {
		t.Errorf("non-deterministic: %v vs %v", r1, r2)
	}
	// a-doc < z-doc lexicographically → a-doc first.
	if r1[0] != "a-doc" {
		t.Errorf("tie-break: top-1 = %q, want a-doc", r1[0])
	}
}

// TestRRF_EmptyList_DegradesToOther checks that an empty cosine list degrades
// to the FTS list unchanged (by rank).
func TestRRF_EmptyList_DegradesToOther(t *testing.T) {
	fts := []string{"x", "y", "z"}
	cos := []string{}

	result := rrfFuse(fts, cos, 60, 3)
	if len(result) != 3 {
		t.Fatalf("expected 3 results, got %d", len(result))
	}
	// FTS rank order preserved: x→y→z (all same-source, sorted by rank).
	if result[0] != "x" || result[1] != "y" || result[2] != "z" {
		t.Errorf("expected [x y z], got %v", result)
	}
}

// ── Task 2.4: SelectEmbeddable + UpdateEmbedding ─────────────────────────────

// TestSelectEmbeddable_PicksNullAndStale checks that both NULL embedding rows
// and rows with a different embedding_model are picked.
func TestSelectEmbeddable_PicksNullAndStale(t *testing.T) {
	db := openTestDB(t)

	// Row with NULL embedding.
	insertMemoryWithEmbedding(t, db, "null-row", "proj", nil)
	// Row with the current model (should NOT be picked).
	v := encodeVector(l2Normalize([]float32{1, 0}))
	_, err := db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content, project, scope,
		   version, writer_id, last_write_mutation_id, created_at, updated_at, embedding, embedding_model)
		VALUES ('current-row', 'sess1', 'memory', 'manual', 'title', 'content', 'proj', 'project',
		        1, 'w1', 'mut1', datetime('now'), datetime('now'), ?, 'new-model')`, v)
	if err != nil {
		t.Fatalf("insert current row: %v", err)
	}
	// Row with stale model.
	_, err = db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content, project, scope,
		   version, writer_id, last_write_mutation_id, created_at, updated_at, embedding, embedding_model)
		VALUES ('stale-row', 'sess1', 'memory', 'manual', 'title', 'content', 'proj', 'project',
		        1, 'w1', 'mut2', datetime('now'), datetime('now'), ?, 'old-model')`, v)
	if err != nil {
		t.Fatalf("insert stale row: %v", err)
	}

	rows, err := SelectEmbeddable(db, "new-model", 100, 0)
	if err != nil {
		t.Fatalf("SelectEmbeddable: %v", err)
	}

	ids := make(map[string]bool)
	for _, r := range rows {
		ids[r.SyncID] = true
	}

	if !ids["null-row"] {
		t.Error("null-row should be in SelectEmbeddable results")
	}
	if !ids["stale-row"] {
		t.Error("stale-row should be in SelectEmbeddable results")
	}
	if ids["current-row"] {
		t.Error("current-row should NOT be in SelectEmbeddable results")
	}
}

// TestSelectEmbeddable_SkipsAlreadyEmbedded checks that up-to-date rows are excluded.
func TestSelectEmbeddable_SkipsAlreadyEmbedded(t *testing.T) {
	db := openTestDB(t)
	v := encodeVector(l2Normalize([]float32{1, 0}))
	_, err := db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content, project, scope,
		   version, writer_id, last_write_mutation_id, created_at, updated_at, embedding, embedding_model)
		VALUES ('up-to-date', 'sess1', 'memory', 'manual', 'title', 'content', 'proj', 'project',
		        1, 'w1', 'mut1', datetime('now'), datetime('now'), ?, 'cur-model')`, v)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, err := SelectEmbeddable(db, "cur-model", 100, 0)
	if err != nil {
		t.Fatalf("SelectEmbeddable: %v", err)
	}
	for _, r := range rows {
		if r.SyncID == "up-to-date" {
			t.Errorf("up-to-date row should be excluded, but was returned")
		}
	}
}

// TestUpdateEmbedding_Idempotent_NullGuard checks that UpdateEmbedding is a
// no-op when embedding is already set (AND embedding IS NULL guard).
func TestUpdateEmbedding_Idempotent_NullGuard(t *testing.T) {
	db := openTestDB(t)
	// Insert row with embedding=NULL.
	insertMemoryWithEmbedding(t, db, "target", "proj", nil)

	vec := l2Normalize([]float32{1, 0, 0})

	// First call should set embedding.
	if err := UpdateEmbedding(db, "target", vec, "model-1", "2024-01-01T00:00:00Z"); err != nil {
		t.Fatalf("first UpdateEmbedding: %v", err)
	}
	// Verify embedding is now set.
	var blob []byte
	if err := db.QueryRow(`SELECT embedding FROM memories WHERE sync_id='target'`).Scan(&blob); err != nil {
		t.Fatalf("scan after first update: %v", err)
	}
	if blob == nil {
		t.Fatal("embedding should be set after first UpdateEmbedding")
	}

	// Second call with different model — should be a no-op (embedding != NULL).
	vec2 := l2Normalize([]float32{0, 1, 0})
	if err := UpdateEmbedding(db, "target", vec2, "model-2", "2024-01-02T00:00:00Z"); err != nil {
		t.Fatalf("second UpdateEmbedding: %v", err)
	}
	// Embedding model should still be "model-1".
	var model string
	if err := db.QueryRow(`SELECT embedding_model FROM memories WHERE sync_id='target'`).Scan(&model); err != nil {
		t.Fatalf("scan model after second update: %v", err)
	}
	if model != "model-1" {
		t.Errorf("embedding_model = %q after idempotent update, want model-1", model)
	}
}
