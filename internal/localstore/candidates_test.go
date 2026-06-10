package localstore

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// openTestStore opens a fresh in-memory-backed Store in a temp directory.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// insertMemory inserts a minimal memories row directly — bypasses AddObservation
// so tests can control the exact title/project/scope/sync_id without the full
// domain.Mutation machinery.
func insertMemory(t *testing.T, s *Store, syncID, title, project, scope string) int64 {
	t.Helper()
	res, err := s.db.Exec(`
		INSERT INTO memories
			(sync_id, session_id, entity_type, type, title, content, project, scope,
			 version, writer_id, last_write_mutation_id, created_at, updated_at)
		VALUES (?, 'sess-test', 'memory', 'manual', ?, '', ?, ?,
			1, '', '', datetime('now'), datetime('now'))
	`, syncID, title, project, scope)
	if err != nil {
		t.Fatalf("insertMemory %q: %v", syncID, err)
	}
	id, _ := res.LastInsertId()
	return id
}

// conflictRelationsCount returns the number of conflict_relations rows.
func conflictRelationsCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM conflict_relations`).Scan(&n); err != nil {
		t.Fatalf("count conflict_relations: %v", err)
	}
	return n
}

// floatPtr returns a pointer to f. lenientFloor() is a floor of 0.0 — since FTS5
// bm25 scores are always negative and a candidate passes when score <= floor, a
// floor of 0.0 admits EVERY match. Tests that exercise exclusion/insert/limit (not
// the floor itself) use it so their assertions don't depend on the BM25 score
// magnitude, which varies with the (small) test corpus size.
func floatPtr(f float64) *float64 { return &f }
func lenientFloor() *float64      { return floatPtr(0.0) }

// ── sanitizeFTSCandidates ─────────────────────────────────────────────────────

func TestSanitizeFTSCandidates_Basic(t *testing.T) {
	got := sanitizeFTSCandidates("auth bug fix")
	want := `"auth" OR "bug" OR "fix"`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeFTSCandidates_SingleWord(t *testing.T) {
	got := sanitizeFTSCandidates("auth")
	want := `"auth"`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeFTSCandidates_StripQuotes(t *testing.T) {
	// Leading/trailing quotes on a token must be stripped before re-wrapping.
	got := sanitizeFTSCandidates(`"auth" bug`)
	want := `"auth" OR "bug"`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestSanitizeFTSCandidates_InteriorQuote guards the fix for the interior-quote
// crash: a token with an INTERIOR double-quote must have ALL quotes stripped, not
// just leading/trailing — otherwise it emits an unterminated FTS5 string literal.
func TestSanitizeFTSCandidates_InteriorQuote(t *testing.T) {
	got := sanitizeFTSCandidates(`Fixed JWT"auth bug`)
	want := `"Fixed" OR "JWTauth" OR "bug"`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// And the produced query must actually run against FTS5 without error.
	s := openTestStore(t)
	insertMemory(t, s, "obs-q", "JWTauth handling", "proj", "project")
	savedID := insertMemory(t, s, "obs-new", `Fixed JWT"auth bug`, "proj", "project")
	if _, err := s.FindCandidates(context.Background(), savedID, CandidateOptions{BM25Floor: lenientFloor()}); err != nil {
		t.Errorf("FindCandidates with an interior-quote title errored: %v", err)
	}
}

func TestSanitizeFTSCandidates_Empty(t *testing.T) {
	if got := sanitizeFTSCandidates(""); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestSanitizeFTSCandidates_OnlySpaces(t *testing.T) {
	if got := sanitizeFTSCandidates("   "); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

// ── FindCandidates ────────────────────────────────────────────────────────────

func TestFindCandidates_OverlappingTitles(t *testing.T) {
	s := openTestStore(t)

	insertMemory(t, s, "obs-001", "JWT authentication fix", "proj", "project")
	insertMemory(t, s, "obs-002", "auth middleware refactor", "proj", "project")
	insertMemory(t, s, "obs-003", "authentication session bug", "proj", "project")
	insertMemory(t, s, "obs-unrelated", "database migration plan", "proj", "project")

	savedID := insertMemory(t, s, "obs-new", "JWT authentication session bug", "proj", "project")

	candidates, err := s.FindCandidates(context.Background(), savedID, CandidateOptions{BM25Floor: lenientFloor()})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(candidates) == 0 {
		t.Fatal("expected candidates, got none")
	}
	for _, c := range candidates {
		if c.ID == savedID {
			t.Errorf("self (savedID=%d) must be excluded from candidates", savedID)
		}
		if c.SyncID == "obs-unrelated" {
			t.Error("unrelated observation (no title overlap) must not be a candidate")
		}
	}
	if len(candidates) > 3 {
		t.Errorf("expected at most 3 candidates, got %d", len(candidates))
	}
}

func TestFindCandidates_RelationRowsInserted(t *testing.T) {
	s := openTestStore(t)

	insertMemory(t, s, "obs-a", "JWT auth middleware", "proj", "project")
	insertMemory(t, s, "obs-b", "auth token refresh", "proj", "project")
	savedID := insertMemory(t, s, "obs-new", "JWT auth token", "proj", "project")

	candidates, err := s.FindCandidates(context.Background(), savedID, CandidateOptions{BM25Floor: lenientFloor()})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(candidates) == 0 {
		t.Fatal("expected candidates with a lenient floor, got none")
	}

	for _, c := range candidates {
		if c.JudgmentID == "" {
			t.Errorf("candidate %q has empty JudgmentID", c.SyncID)
		}
		if !strings.HasPrefix(c.JudgmentID, "rel-") {
			t.Errorf("candidate %q JudgmentID %q lacks 'rel-' prefix", c.SyncID, c.JudgmentID)
		}
	}

	if n := conflictRelationsCount(t, s); n != len(candidates) {
		t.Errorf("expected %d conflict_relations rows, got %d", len(candidates), n)
	}

	for _, c := range candidates {
		rel, err := s.GetRelation(c.JudgmentID)
		if err != nil {
			t.Errorf("GetRelation(%q): %v", c.JudgmentID, err)
			continue
		}
		if rel.JudgmentStatus != JudgmentStatusPending {
			t.Errorf("expected pending status, got %q", rel.JudgmentStatus)
		}
		if rel.Relation != RelationPending {
			t.Errorf("expected pending relation, got %q", rel.Relation)
		}
		if rel.SourceID != "obs-new" {
			t.Errorf("expected source_id obs-new, got %q", rel.SourceID)
		}
	}
}

func TestFindCandidates_SkipInsert(t *testing.T) {
	s := openTestStore(t)

	insertMemory(t, s, "obs-a", "JWT auth middleware", "proj", "project")
	savedID := insertMemory(t, s, "obs-new", "JWT auth token", "proj", "project")

	candidates, err := s.FindCandidates(context.Background(), savedID, CandidateOptions{SkipInsert: true, BM25Floor: lenientFloor()})
	if err != nil {
		t.Fatalf("FindCandidates(SkipInsert=true): %v", err)
	}
	for _, c := range candidates {
		if c.JudgmentID != "" {
			t.Errorf("SkipInsert=true: expected empty JudgmentID, got %q", c.JudgmentID)
		}
	}
	if n := conflictRelationsCount(t, s); n != 0 {
		t.Errorf("SkipInsert=true: expected 0 conflict_relations rows, got %d", n)
	}
}

func TestFindCandidates_SoftDeletedExcluded(t *testing.T) {
	s := openTestStore(t)

	idA := insertMemory(t, s, "obs-a", "JWT auth middleware", "proj", "project")
	if _, err := s.db.Exec(
		`UPDATE memories SET deleted_at = datetime('now') WHERE id = ?`, idA,
	); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	savedID := insertMemory(t, s, "obs-new", "JWT auth token", "proj", "project")

	// Lenient floor so the ONLY reason obs-a can be excluded is the deleted_at
	// filter (not the BM25 floor dropping everything in a tiny corpus).
	candidates, err := s.FindCandidates(context.Background(), savedID, CandidateOptions{BM25Floor: lenientFloor()})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	for _, c := range candidates {
		if c.SyncID == "obs-a" {
			t.Error("soft-deleted observation must not be a candidate")
		}
	}
}

func TestFindCandidates_CrossProjectExcluded(t *testing.T) {
	s := openTestStore(t)

	insertMemory(t, s, "obs-other-proj", "JWT auth middleware", "other-proj", "project")
	savedID := insertMemory(t, s, "obs-new", "JWT auth token", "proj", "project")

	candidates, err := s.FindCandidates(context.Background(), savedID, CandidateOptions{BM25Floor: lenientFloor()})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	for _, c := range candidates {
		if c.SyncID == "obs-other-proj" {
			t.Error("observation from a different project must not be a candidate")
		}
	}
}

// TestFindCandidates_CrossScopeExcluded verifies the scope filter: a personal
// observation is not a candidate for a project-scoped save.
func TestFindCandidates_CrossScopeExcluded(t *testing.T) {
	s := openTestStore(t)

	insertMemory(t, s, "obs-personal", "JWT auth middleware", "proj", "personal")
	savedID := insertMemory(t, s, "obs-new", "JWT auth token", "proj", "project")

	candidates, err := s.FindCandidates(context.Background(), savedID, CandidateOptions{BM25Floor: lenientFloor()})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	for _, c := range candidates {
		if c.SyncID == "obs-personal" {
			t.Error("observation from a different scope must not be a candidate")
		}
	}
}

func TestFindCandidates_LimitHonored(t *testing.T) {
	s := openTestStore(t)

	for i := 0; i < 10; i++ {
		insertMemory(t, s,
			fmt.Sprintf("obs-%02d", i),
			fmt.Sprintf("JWT auth session token refresh %d", i),
			"proj", "project",
		)
	}

	limit := 2
	savedID := insertMemory(t, s, "obs-new", "JWT auth session token", "proj", "project")

	candidates, err := s.FindCandidates(context.Background(), savedID, CandidateOptions{Limit: limit, BM25Floor: lenientFloor()})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(candidates) > limit {
		t.Errorf("expected at most %d candidates, got %d", limit, len(candidates))
	}
}

// TestFindCandidates_BM25FloorFilters proves the floor keeps STRONG (multi-word
// overlap, more-negative score) matches and drops WEAK (single shared word,
// closer-to-zero score) ones. Uses a realistic corpus so BM25 IDF spreads the
// scores around the default −2.0 floor (in a 2-row corpus all scores hug 0).
func TestFindCandidates_BM25FloorFilters(t *testing.T) {
	s := openTestStore(t)

	insertMemory(t, s, "obs-strong-1", "JWT auth login bug in middleware", "proj", "project")
	insertMemory(t, s, "obs-strong-2", "fix JWT auth login session bug", "proj", "project")
	insertMemory(t, s, "obs-weak", "user login redesign notes", "proj", "project") // only "login" overlaps
	for i := 0; i < 12; i++ {
		insertMemory(t, s,
			fmt.Sprintf("obs-noise-%02d", i),
			fmt.Sprintf("unrelated infrastructure topic number %d", i),
			"proj", "project",
		)
	}

	savedID := insertMemory(t, s, "obs-new", "JWT auth login bug", "proj", "project")

	// Default floor (−2.0): strong multi-word matches pass; the single-word weak
	// match (closer to 0) is excluded.
	candidates, err := s.FindCandidates(context.Background(), savedID, CandidateOptions{SkipInsert: true})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(candidates) == 0 {
		t.Fatal("expected the strong matches to pass the default floor")
	}

	var sawStrong, sawWeak bool
	for _, c := range candidates {
		if c.Score > -2.0 {
			t.Errorf("candidate %q score %.4f is weaker than floor −2.0 (should be excluded)", c.SyncID, c.Score)
		}
		if c.SyncID == "obs-strong-1" || c.SyncID == "obs-strong-2" {
			sawStrong = true
		}
		if c.SyncID == "obs-weak" {
			sawWeak = true
		}
	}
	if !sawStrong {
		t.Error("expected a strong multi-word match among candidates")
	}
	if sawWeak {
		t.Error("weak single-word match should be excluded by the default floor")
	}
}

func TestFindCandidates_ObservationNotFound(t *testing.T) {
	s := openTestStore(t)

	_, err := s.FindCandidates(context.Background(), 999999, CandidateOptions{})
	if err == nil {
		t.Error("expected error for non-existent savedID, got nil")
	}
}

// ── JudgeRelation / GetRelation round-trip ────────────────────────────────────

func TestJudgeRelation_RoundTrip(t *testing.T) {
	s := openTestStore(t)

	judgmentID := newRelSyncID()
	if _, err := s.db.Exec(`
		INSERT INTO conflict_relations
			(sync_id, source_id, target_id, relation, judgment_status, created_at, updated_at)
		VALUES (?, 'obs-src', 'obs-tgt', 'pending', 'pending', datetime('now'), datetime('now'))
	`, judgmentID); err != nil {
		t.Fatalf("insert pending relation: %v", err)
	}

	reason := "overlapping topic"
	confidence := 0.9

	rel, err := s.JudgeRelation(JudgeRelationParams{
		JudgmentID: judgmentID,
		Relation:   RelationConflictsWith,
		Reason:     &reason,
		Confidence: &confidence,
	})
	if err != nil {
		t.Fatalf("JudgeRelation: %v", err)
	}

	if rel.JudgmentStatus != JudgmentStatusJudged {
		t.Errorf("expected status %q, got %q", JudgmentStatusJudged, rel.JudgmentStatus)
	}
	if rel.Relation != RelationConflictsWith {
		t.Errorf("expected relation %q, got %q", RelationConflictsWith, rel.Relation)
	}
	if rel.Reason == nil || *rel.Reason != reason {
		t.Errorf("reason mismatch: %v", rel.Reason)
	}
	if rel.Confidence == nil || *rel.Confidence != confidence {
		t.Errorf("confidence mismatch: %v", rel.Confidence)
	}

	got, err := s.GetRelation(judgmentID)
	if err != nil {
		t.Fatalf("GetRelation: %v", err)
	}
	if got.JudgmentStatus != JudgmentStatusJudged {
		t.Errorf("GetRelation status: expected %q, got %q", JudgmentStatusJudged, got.JudgmentStatus)
	}
	if got.Relation != RelationConflictsWith {
		t.Errorf("GetRelation relation: expected %q, got %q", RelationConflictsWith, got.Relation)
	}
}

func TestJudgeRelation_InvalidVerb(t *testing.T) {
	s := openTestStore(t)

	_, err := s.JudgeRelation(JudgeRelationParams{
		JudgmentID: "rel-fake",
		Relation:   "invalid_verb",
	})
	if err == nil {
		t.Error("expected error for invalid verb, got nil")
	}
}

func TestJudgeRelation_NotFound(t *testing.T) {
	s := openTestStore(t)

	_, err := s.JudgeRelation(JudgeRelationParams{
		JudgmentID: "rel-does-not-exist",
		Relation:   RelationRelated,
	})
	if err == nil {
		t.Error("expected error for unknown JudgmentID, got nil")
	}
}

func TestJudgeRelation_AllVerbs(t *testing.T) {
	verbs := []string{
		RelationRelated, RelationCompatible, RelationScoped,
		RelationConflictsWith, RelationSupersedes, RelationNotConflict,
	}

	s := openTestStore(t)

	for _, verb := range verbs {
		verb := verb
		t.Run(verb, func(t *testing.T) {
			id := newRelSyncID()
			if _, err := s.db.Exec(`
				INSERT INTO conflict_relations
					(sync_id, source_id, target_id, relation, judgment_status, created_at, updated_at)
				VALUES (?, 'obs-src', 'obs-tgt', 'pending', 'pending', datetime('now'), datetime('now'))
			`, id); err != nil {
				t.Fatalf("insert: %v", err)
			}

			rel, err := s.JudgeRelation(JudgeRelationParams{
				JudgmentID: id,
				Relation:   verb,
			})
			if err != nil {
				t.Fatalf("JudgeRelation(%q): %v", verb, err)
			}
			if rel.Relation != verb {
				t.Errorf("expected %q, got %q", verb, rel.Relation)
			}
		})
	}
}

func TestGetRelation_NotFound(t *testing.T) {
	s := openTestStore(t)

	_, err := s.GetRelation("rel-does-not-exist")
	if err == nil {
		t.Error("expected error for unknown sync_id, got nil")
	}
}

// ── Migration tests ───────────────────────────────────────────────────────────

func TestMigrateV7ToV8_FreshDB(t *testing.T) {
	s := openTestStore(t)

	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if ver != currentSchemaVersion {
		t.Errorf("expected schema version %d, got %d", currentSchemaVersion, ver)
	}

	if _, err := s.db.Exec(`
		INSERT INTO conflict_relations
			(sync_id, source_id, target_id, relation, judgment_status, created_at, updated_at)
		VALUES ('rel-test', 'obs-a', 'obs-b', 'pending', 'pending', datetime('now'), datetime('now'))
	`); err != nil {
		t.Fatalf("insert into conflict_relations: %v", err)
	}
}

func TestMigrateV7ToV8_Idempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v8.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Re-run migration on an already-v8 DB — must be a no-op (CREATE TABLE/INDEX IF
	// NOT EXISTS), no error, version stays 8.
	if err := migrateV7ToV8(s.db); err != nil {
		t.Fatalf("migrateV7ToV8 (idempotent run): %v", err)
	}
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if ver != 8 {
		t.Errorf("expected user_version 8, got %d", ver)
	}
}

// ── Concurrency smoke test ────────────────────────────────────────────────────

// TestFindCandidates_ConcurrentNoDead confirms concurrent FindCandidates calls do
// not produce SQLITE_BUSY or deadlock — validating that (1) the FTS cursor is
// drained+closed BEFORE any write, and (2) s.mu serializes the write phase.
func TestFindCandidates_ConcurrentNoDead(t *testing.T) {
	s := openTestStore(t)

	for i := 0; i < 5; i++ {
		insertMemory(t, s,
			fmt.Sprintf("obs-seed-%d", i),
			fmt.Sprintf("JWT authentication session bug %d", i),
			"proj", "project",
		)
	}

	savedIDs := make([]int64, 5)
	for i := 0; i < 5; i++ {
		savedIDs[i] = insertMemory(t, s,
			fmt.Sprintf("obs-new-%d", i),
			fmt.Sprintf("JWT auth token refresh %d", i),
			"proj", "project",
		)
	}

	const workers = 5
	errs := make([]error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)

	deadline := time.After(10 * time.Second)
	done := make(chan struct{})

	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			// Lenient floor so the write phase (INSERT) is actually exercised.
			_, errs[i] = s.FindCandidates(context.Background(), savedIDs[i], CandidateOptions{BM25Floor: lenientFloor()})
		}()
	}

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-deadline:
		t.Fatal("TestFindCandidates_ConcurrentNoDead: timed out — likely deadlock")
	}

	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: FindCandidates error: %v", i, err)
		}
	}
}

// ── FindCandidates cosine pass (task 2.8) ────────────────────────────────────

// TestFindCandidates_NilGate_FTSOnly verifies that passing EmbedFn=nil leaves
// the existing FTS-only path completely unchanged — the cosine pass is skipped.
func TestFindCandidates_NilGate_FTSOnly(t *testing.T) {
	s := openTestStore(t)

	// Insert a candidate that shares keywords with the source.
	insertMemory(t, s, "cand-fts", "JWT auth login bug", "proj", "project")
	savedID := insertMemory(t, s, "src", "JWT auth login bug fix", "proj", "project")

	// EmbedFn is nil — cosine pass must not run.
	candidates, err := s.FindCandidates(context.Background(), savedID, CandidateOptions{
		BM25Floor:  lenientFloor(),
		SkipInsert: true,
		EmbedFn:    nil, // explicit nil
	})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	// FTS should still surface the keyword match.
	found := false
	for _, c := range candidates {
		if c.SyncID == "cand-fts" {
			found = true
		}
	}
	if !found {
		t.Error("FTS candidate not found with nil EmbedFn")
	}
}

// TestFindCandidates_CosineSurfaces_Paraphrase proves the cosine pass surfaces a
// paraphrase that shares NO keywords with the source (FTS returns 0 results),
// provided it has a similar embedding vector.
//
// Setup:
//   - Source:    "authentication failure event" (stored + distinct embedding)
//   - Paraphrase: "login error occurrence" (no shared words → FTS miss)
//   - Noise:      "completely unrelated infrastructure note"
//
// The paraphrase gets an embedding nearly identical to the source (high cosine).
// The noise row gets an orthogonal embedding.
// The injected EmbedFn returns the source's own stored vector for any input,
// simulating a semantic model that would embed the paraphrase similarly.
func TestFindCandidates_CosineSurfaces_Paraphrase(t *testing.T) {
	s := openTestStore(t)

	dims := 4

	// Insert rows.
	paraphraseID := insertMemory(t, s, "cand-paraphrase", "login error occurrence", "proj", "project")
	noiseID := insertMemory(t, s, "cand-noise", "completely unrelated infrastructure note", "proj", "project")
	savedID := insertMemory(t, s, "src", "authentication failure event", "proj", "project")

	// Build L2-normalized embeddings. Source and paraphrase are nearly identical;
	// noise is orthogonal.
	srcVec := l2Normalize([]float32{0.9, 0.1, 0.1, 0.1})
	paraVec := l2Normalize([]float32{0.85, 0.12, 0.09, 0.08}) // high cosine with src
	noiseVec := l2Normalize([]float32{0.0, 0.0, 0.0, 1.0})    // orthogonal

	now := "2025-01-01T00:00:00Z"

	// Write embeddings directly (bypass the loop — tests only need the stored BLOB).
	for _, row := range []struct {
		id  int64
		vec []float32
	}{
		{savedID, srcVec},
		{paraphraseID, paraVec},
		{noiseID, noiseVec},
	} {
		blob := encodeVector(row.vec)
		if _, err := s.db.Exec(
			`UPDATE memories SET embedding=?, embedding_model='test', embedding_created_at=? WHERE id=?`,
			blob, now, row.id,
		); err != nil {
			t.Fatalf("store embedding for id=%d: %v", row.id, err)
		}
	}

	// The FTS query for "authentication failure event" should NOT match
	// "login error occurrence" (no shared words). Verify FTS finds nothing first.
	ftsOnlyCands, err := s.FindCandidates(context.Background(), savedID, CandidateOptions{
		BM25Floor:  lenientFloor(),
		SkipInsert: true,
		EmbedFn:    nil,
	})
	if err != nil {
		t.Fatalf("FTS-only FindCandidates: %v", err)
	}
	for _, c := range ftsOnlyCands {
		if c.SyncID == "cand-paraphrase" {
			t.Error("FTS should NOT surface the paraphrase (no keyword overlap)")
		}
	}

	// Now inject an EmbedFn that returns srcVec for any text (simulates the
	// semantic model embedding the source title into a similar vector space).
	embedFn := EmbedQueryFn(func(_ context.Context, _ string, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			cp := make([]float32, dims)
			copy(cp, srcVec)
			out[i] = cp
		}
		return out, nil
	})

	cosineCands, err := s.FindCandidates(context.Background(), savedID, CandidateOptions{
		BM25Floor:  lenientFloor(),
		SkipInsert: true,
		EmbedFn:    embedFn,
		EmbedDims:  dims,
	})
	if err != nil {
		t.Fatalf("cosine-pass FindCandidates: %v", err)
	}

	// The paraphrase must be in the result set.
	foundParaphrase := false
	for _, c := range cosineCands {
		if c.SyncID == "cand-paraphrase" {
			foundParaphrase = true
		}
	}
	if !foundParaphrase {
		t.Error("cosine pass should surface cand-paraphrase (high cosine, no keyword overlap)")
	}
}
