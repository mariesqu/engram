package localstore

// Integration tests for the local SYNC API (sync.go) against a real SQLite temp
// file. These prove the outbox + cursor mechanics in isolation, before the
// in-process convergence spike wires them to a real central store.

import (
	"fmt"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/mutation"
)

// upsertMut builds a topic-keyed upsert mutation with content-addressed identity
// left UNSET so LocalWrite derives Payload + MutationID itself.
func upsertMut(syncID, topic, content string, version int, at time.Time) domain.Mutation {
	tk := topic
	return domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "title",
		Content:    content,
		Project:    "engram",
		Scope:      "project",
		TopicKey:   &tk,
		Version:    version,
		UpdatedAt:  at,
		WriterID:   "writer-" + syncID,
	}
}

// TestLocalWrite_AppliesAndEnqueues verifies LocalWrite both materializes the
// row locally (visible to FindByTopic) AND enqueues a pending outbox entry whose
// MutationID is the content-addressed ID derived from the canonical payload.
func TestLocalWrite_AppliesAndEnqueues(t *testing.T) {
	s := openTempStore(t)
	topic := "sdd/test/localwrite"

	m := upsertMut("sync-lw-1", topic, "hello", 1, baseT.Add(10*time.Second))
	got, err := s.LocalWrite(m)
	if err != nil {
		t.Fatalf("LocalWrite: %v", err)
	}

	// MutationID must equal the content-addressed ID of the canonical payload.
	wantID := mutation.NewMutationID(mutation.CanonicalPayload(got))
	if got.MutationID != wantID {
		t.Errorf("LocalWrite MutationID=%q, want content-addressed %q", got.MutationID, wantID)
	}
	if len(got.Payload) == 0 {
		t.Error("LocalWrite: Payload not populated")
	}

	// Row is live and visible.
	rec, err := s.FindByTopic(topic, "engram", "project")
	if err != nil {
		t.Fatalf("FindByTopic: %v", err)
	}
	if rec == nil || rec.Content != "hello" {
		t.Fatalf("FindByTopic: got %+v, want live row with content 'hello'", rec)
	}

	// Exactly one pending outbox entry.
	n, err := s.PendingCount()
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if n != 1 {
		t.Errorf("PendingCount=%d, want 1", n)
	}
}

// TestDrainOutbox_OrderAndDecode verifies DrainOutbox returns pending entries in
// local_seq order with fully-decoded mutations (content round-trips from the
// canonical payload).
func TestDrainOutbox_OrderAndDecode(t *testing.T) {
	s := openTempStore(t)

	_, err := s.LocalWrite(upsertMut("sync-d-1", "sdd/test/d1", "first", 1, baseT.Add(1*time.Second)))
	if err != nil {
		t.Fatalf("LocalWrite 1: %v", err)
	}
	_, err = s.LocalWrite(upsertMut("sync-d-2", "sdd/test/d2", "second", 1, baseT.Add(2*time.Second)))
	if err != nil {
		t.Fatalf("LocalWrite 2: %v", err)
	}

	entries, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("DrainOutbox: got %d entries, want 2", len(entries))
	}
	if entries[0].LocalSeq >= entries[1].LocalSeq {
		t.Errorf("DrainOutbox not in local_seq order: %d then %d", entries[0].LocalSeq, entries[1].LocalSeq)
	}
	if entries[0].Mutation.Content != "first" || entries[1].Mutation.Content != "second" {
		t.Errorf("DrainOutbox decode mismatch: %q, %q", entries[0].Mutation.Content, entries[1].Mutation.Content)
	}
	if entries[0].Mutation.SyncID != "sync-d-1" {
		t.Errorf("DrainOutbox entry[0] sync_id=%q, want sync-d-1", entries[0].Mutation.SyncID)
	}
}

// TestAckMutation_RemovesFromPendingAndAdvancesCursor verifies that acking an
// outbox entry removes it from the pending set and advances last_acked_seq.
func TestAckMutation_RemovesFromPendingAndAdvancesCursor(t *testing.T) {
	s := openTempStore(t)

	_, err := s.LocalWrite(upsertMut("sync-a-1", "sdd/test/a1", "one", 1, baseT.Add(1*time.Second)))
	if err != nil {
		t.Fatalf("LocalWrite: %v", err)
	}

	entries, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 pending, got %d", len(entries))
	}

	if err := s.AckMutation(entries[0].LocalSeq); err != nil {
		t.Fatalf("AckMutation: %v", err)
	}

	n, err := s.PendingCount()
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if n != 0 {
		t.Errorf("PendingCount after ack=%d, want 0", n)
	}

	// last_acked_seq advanced to the entry's local_seq.
	var acked int64
	if err := s.db.QueryRow(
		`SELECT last_acked_seq FROM sync_state WHERE target_key = 'central'`,
	).Scan(&acked); err != nil {
		t.Fatalf("read last_acked_seq: %v", err)
	}
	if acked != entries[0].LocalSeq {
		t.Errorf("last_acked_seq=%d, want %d", acked, entries[0].LocalSeq)
	}
}

// TestPullCursor_DefaultsZeroAndAdvancesMonotonically verifies the pull cursor
// starts at 0, advances forward, and refuses to rewind.
func TestPullCursor_DefaultsZeroAndAdvancesMonotonically(t *testing.T) {
	s := openTempStore(t)

	cur, err := s.PullCursor()
	if err != nil {
		t.Fatalf("PullCursor: %v", err)
	}
	if cur != 0 {
		t.Errorf("fresh PullCursor=%d, want 0", cur)
	}

	if err := s.SetPullCursor(5); err != nil {
		t.Fatalf("SetPullCursor(5): %v", err)
	}
	if cur, _ = s.PullCursor(); cur != 5 {
		t.Errorf("PullCursor after set 5=%d, want 5", cur)
	}

	// Advancing forward works.
	if err := s.SetPullCursor(9); err != nil {
		t.Fatalf("SetPullCursor(9): %v", err)
	}
	if cur, _ = s.PullCursor(); cur != 9 {
		t.Errorf("PullCursor after set 9=%d, want 9", cur)
	}

	// Rewinding is ignored (monotonic).
	if err := s.SetPullCursor(3); err != nil {
		t.Fatalf("SetPullCursor(3): %v", err)
	}
	if cur, _ = s.PullCursor(); cur != 9 {
		t.Errorf("PullCursor after rewind attempt=%d, want 9 (monotonic)", cur)
	}
}

// TestLocalWrite_Atomic verifies that LocalWrite leaves BOTH a live memory row
// (visible to FindByTopic) AND a pending outbox entry in the same SQLite
// database state after a single call. Both results commit together — the
// atomicity guarantee introduced by the single-tx refactor.
func TestLocalWrite_Atomic(t *testing.T) {
	s := openTempStore(t)
	topic := "sdd/test/atomic"

	m := upsertMut("sync-atom-1", topic, "atomic-content", 1, baseT.Add(5*time.Second))
	got, err := s.LocalWrite(m)
	if err != nil {
		t.Fatalf("LocalWrite: %v", err)
	}

	// Memory row must be live.
	rec, err := s.FindByTopic(topic, "engram", "project")
	if err != nil {
		t.Fatalf("FindByTopic: %v", err)
	}
	if rec == nil {
		t.Fatal("FindByTopic: got nil; want live memory row")
	}
	if rec.Content != "atomic-content" {
		t.Errorf("FindByTopic content=%q, want %q", rec.Content, "atomic-content")
	}

	// Outbox must also have exactly one pending entry for the same mutation.
	entries, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("DrainOutbox: got %d entries, want 1 (atomic commit)", len(entries))
	}
	if entries[0].Mutation.MutationID != got.MutationID {
		t.Errorf("outbox entry MutationID=%q, want %q", entries[0].Mutation.MutationID, got.MutationID)
	}
}

// TestAckMutation_ErrorOnNonExistentSeq verifies that AckMutation returns an
// error when the supplied local_seq does not exist or has already been acked,
// and that last_acked_seq is NOT advanced in either case.
func TestAckMutation_ErrorOnNonExistentSeq(t *testing.T) {
	s := openTempStore(t)

	readCursor := func() int64 {
		t.Helper()
		var v int64
		if err := s.db.QueryRow(
			`SELECT last_acked_seq FROM sync_state WHERE target_key = 'central'`,
		).Scan(&v); err != nil {
			t.Fatalf("read last_acked_seq: %v", err)
		}
		return v
	}

	// 1. Ack a local_seq that was never inserted — must error.
	if err := s.AckMutation(999); err == nil {
		t.Error("AckMutation(non-existent): want error, got nil")
	}
	if got := readCursor(); got != 0 {
		t.Errorf("cursor after non-existent ack = %d; want 0 (must not advance)", got)
	}

	// 2. Write one mutation, ack it successfully, then try to ack it again (already-acked).
	_, err := s.LocalWrite(upsertMut("sync-ack-err", "sdd/test/ack-err", "v1", 1, baseT.Add(1*time.Second)))
	if err != nil {
		t.Fatalf("LocalWrite: %v", err)
	}
	entries, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 pending entry, got %d", len(entries))
	}
	localSeq := entries[0].LocalSeq

	// First ack succeeds and advances the cursor.
	if err := s.AckMutation(localSeq); err != nil {
		t.Fatalf("first AckMutation: %v", err)
	}
	if got := readCursor(); got != localSeq {
		t.Errorf("cursor after first ack = %d; want %d", got, localSeq)
	}

	// Second ack of the same (already-acked) local_seq must error.
	if err := s.AckMutation(localSeq); err == nil {
		t.Error("AckMutation(already-acked): want error, got nil")
	}
	// Cursor must remain at localSeq (not double-advance or reset).
	if got := readCursor(); got != localSeq {
		t.Errorf("cursor after double-ack attempt = %d; want %d (must not change)", got, localSeq)
	}
}

// TestLocalWrite_ConcurrentWrites_NoError launches several goroutines that each
// perform LocalWrite and ApplyPulled calls concurrently against the same Store
// (distinct topics so Decide sees no conflicts). With db.SetMaxOpenConns(1) the
// single SQLite connection serializes the transactions — the whole
// decide+apply+enqueue sequence is atomic per call, so there are no race
// conditions, constraint errors, or stale decisions.
//
// After all goroutines finish the test asserts:
//   - No errors occurred.
//   - The expected number of live rows is present (one per topic).
//   - The outbox has the expected number of pending entries (one per LocalWrite).
func TestLocalWrite_ConcurrentWrites_NoError(t *testing.T) {
	s := openTempStore(t)

	const nWriters = 8 // goroutines
	errCh := make(chan error, nWriters*2)

	for i := 0; i < nWriters; i++ {
		i := i
		go func() {
			topic := fmt.Sprintf("sdd/test/concurrent-%d", i)
			syncID := fmt.Sprintf("sync-c%d", i)
			m := upsertMut(syncID, topic, fmt.Sprintf("content-%d", i), 1, baseT.Add(time.Duration(i+1)*time.Second))
			_, err := s.LocalWrite(m)
			errCh <- err
		}()
	}

	// Also run ApplyPulled concurrently for distinct topics (simulating pull-apply
	// interleaved with local writes). These are distinct topics so no conflict.
	for i := 0; i < nWriters; i++ {
		i := i
		go func() {
			topic := fmt.Sprintf("sdd/test/concurrent-pull-%d", i)
			syncID := fmt.Sprintf("sync-p%d", i)
			m := upsertMut(syncID, topic, fmt.Sprintf("pulled-%d", i), 1, baseT.Add(time.Duration(i+1)*time.Second))
			// normalizeMutation so MutationID is set (ApplyPulled preserves it).
			m = normalizeMutation(m)
			err := s.ApplyPulled(m)
			errCh <- err
		}()
	}

	for i := 0; i < nWriters*2; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("concurrent write/pull error: %v", err)
		}
	}

	// Each LocalWrite topic must be live.
	for i := 0; i < nWriters; i++ {
		topic := fmt.Sprintf("sdd/test/concurrent-%d", i)
		rec, err := s.FindByTopic(topic, "engram", "project")
		if err != nil {
			t.Errorf("FindByTopic(%s): %v", topic, err)
			continue
		}
		if rec == nil {
			t.Errorf("topic %s: missing live row after concurrent LocalWrite", topic)
		}
	}

	// Each ApplyPulled topic must also be live.
	for i := 0; i < nWriters; i++ {
		topic := fmt.Sprintf("sdd/test/concurrent-pull-%d", i)
		rec, err := s.FindByTopic(topic, "engram", "project")
		if err != nil {
			t.Errorf("FindByTopic(%s): %v", topic, err)
			continue
		}
		if rec == nil {
			t.Errorf("topic %s: missing live row after concurrent ApplyPulled", topic)
		}
	}

	// Outbox must have exactly nWriters pending entries (one per LocalWrite).
	// ApplyPulled does NOT enqueue, so pulled rows don't appear here.
	n, err := s.PendingCount()
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if n != nWriters {
		t.Errorf("PendingCount=%d, want %d (one outbox entry per LocalWrite)", n, nWriters)
	}
}

// TestLocalWrite_IdempotentReenqueue verifies that re-running the SAME logical
// write (same content → same content-addressed MutationID) does not create a
// second outbox row.
func TestLocalWrite_IdempotentReenqueue(t *testing.T) {
	s := openTempStore(t)

	m := upsertMut("sync-idem-1", "sdd/test/idem", "v1", 1, baseT.Add(1*time.Second))
	first, err := s.LocalWrite(m)
	if err != nil {
		t.Fatalf("LocalWrite first: %v", err)
	}
	// Re-apply the SAME normalized mutation (carry the derived ID/payload).
	if _, err := s.LocalWrite(first); err != nil {
		t.Fatalf("LocalWrite re-apply: %v", err)
	}

	n, err := s.PendingCount()
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if n != 1 {
		t.Errorf("PendingCount after duplicate LocalWrite=%d, want 1 (idempotent enqueue)", n)
	}
}

// ── Normalize empty-topic tests ───────────────────────────────────────────────

// noTopicMut builds a no-topic upsert mutation. topicVal is passed as a *string
// so the test can supply nil, &"" or a non-empty pointer to explore all cases.
func noTopicMut(syncID string, topicKey *string, version int, at time.Time) domain.Mutation {
	return domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess-notopic",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "no-topic title",
		Content:    "no-topic content",
		Project:    "engram",
		Scope:      "project",
		TopicKey:   topicKey,
		Version:    version,
		UpdatedAt:  at,
		WriterID:   "writer-notopic",
	}
}

// noTopicDeleteMut builds a no-topic delete mutation.
func noTopicDeleteMut(syncID string, topicKey *string, version int, at time.Time) domain.Mutation {
	return domain.Mutation{
		Op:         domain.OpDelete,
		SyncID:     syncID,
		SessionID:  "sess-notopic-del",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "no-topic title",
		Content:    "no-topic content",
		Project:    "engram",
		Scope:      "project",
		TopicKey:   topicKey,
		Version:    version,
		UpdatedAt:  at,
		WriterID:   "writer-notopic",
	}
}

// TestNormalizeTopicKey_MutationID_NilAndEmptyConverge proves that a no-topic
// mutation built with TopicKey=&"" and the same one with TopicKey=nil produce
// the SAME MutationID after normalizeMutation, confirming that the
// normalisation runs BEFORE CanonicalPayload/NewMutationID.
func TestNormalizeTopicKey_MutationID_NilAndEmptyConverge(t *testing.T) {
	at := baseT.Add(1 * time.Second)
	empty := ""

	// Confirm &"" is folded to nil after normalisation.
	mEmpty := normalizeMutation(noTopicMut("sync-empty", &empty, 1, at))
	if mEmpty.TopicKey != nil {
		t.Errorf("normalizeMutation(&\"\"): TopicKey not nil after normalisation, got %q", *mEmpty.TopicKey)
	}

	// The content-addressed IDs must be equal: &"" and nil converge.
	// Use the SAME SyncID so the only difference is the TopicKey input value.
	mNil2 := normalizeMutation(noTopicMut("sync-same", nil, 1, at))
	mEmpty2 := normalizeMutation(noTopicMut("sync-same", &empty, 1, at))

	if mNil2.MutationID != mEmpty2.MutationID {
		t.Errorf("mutation_id mismatch: nil=%q, &\"\"=%q — normalize must run before NewMutationID",
			mNil2.MutationID, mEmpty2.MutationID)
	}
	// Payload must also be identical (the canonical JSON has null not "").
	if string(mNil2.Payload) != string(mEmpty2.Payload) {
		t.Errorf("payload mismatch: nil=%s, &\"\"=%s", mNil2.Payload, mEmpty2.Payload)
	}
}

// TestLocalWrite_EmptyTopicKey_StoresNULL verifies that LocalWrite (upsert) with
// TopicKey=&"" stores the memories.topic_key column as SQL NULL, not as ''.
func TestLocalWrite_EmptyTopicKey_StoresNULL(t *testing.T) {
	s := openTempStore(t)
	empty := ""
	m := noTopicMut("sync-null-upsert", &empty, 1, baseT.Add(1*time.Second))

	if _, err := s.LocalWrite(m); err != nil {
		t.Fatalf("LocalWrite: %v", err)
	}

	var topicKey *string
	err := s.db.QueryRow(
		`SELECT topic_key FROM memories WHERE sync_id = 'sync-null-upsert'`,
	).Scan(&topicKey)
	if err != nil {
		t.Fatalf("SELECT topic_key: %v", err)
	}
	if topicKey != nil {
		t.Errorf("topic_key = %q; want SQL NULL (not empty string)", *topicKey)
	}
}

// TestLocalWrite_EmptyTopicKeyDelete_StoresTombstoneNULL verifies that
// LocalWrite (delete) with TopicKey=&"" stores memory_tombstones.topic_key as
// SQL NULL, not as ''.
func TestLocalWrite_EmptyTopicKeyDelete_StoresTombstoneNULL(t *testing.T) {
	s := openTempStore(t)
	at := baseT.Add(1 * time.Second)
	empty := ""

	// Seed a live row first.
	if _, err := s.LocalWrite(noTopicMut("sync-null-del", &empty, 1, at)); err != nil {
		t.Fatalf("LocalWrite upsert: %v", err)
	}
	// Now delete it.
	del := noTopicDeleteMut("sync-null-del", &empty, 2, at.Add(1*time.Second))
	if _, err := s.LocalWrite(del); err != nil {
		t.Fatalf("LocalWrite delete: %v", err)
	}

	var topicKey *string
	err := s.db.QueryRow(
		`SELECT topic_key FROM memory_tombstones WHERE sync_id = 'sync-null-del'`,
	).Scan(&topicKey)
	if err != nil {
		t.Fatalf("SELECT tombstone topic_key: %v", err)
	}
	if topicKey != nil {
		t.Errorf("tombstone topic_key = %q; want SQL NULL (not empty string)", *topicKey)
	}
}

// TestLocalWrite_TwoNoTopicWrites_NoCollision verifies that two independent
// no-topic upserts (different sync_ids, &"" TopicKey) coexist without a
// UNIQUE violation on idx_mem_topic or any other index.
func TestLocalWrite_TwoNoTopicWrites_NoCollision(t *testing.T) {
	s := openTempStore(t)
	empty := ""
	at := baseT.Add(1 * time.Second)

	m1 := noTopicMut("sync-nt-1", &empty, 1, at)
	m2 := noTopicMut("sync-nt-2", &empty, 1, at.Add(1*time.Second))

	if _, err := s.LocalWrite(m1); err != nil {
		t.Fatalf("LocalWrite m1: %v", err)
	}
	if _, err := s.LocalWrite(m2); err != nil {
		t.Fatalf("LocalWrite m2 (second no-topic write): %v", err)
	}

	// Both rows must be live.
	var count int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memories WHERE topic_key IS NULL AND project = 'engram' AND scope = 'project' AND deleted_at IS NULL`,
	).Scan(&count); err != nil {
		t.Fatalf("count no-topic live rows: %v", err)
	}
	if count != 2 {
		t.Errorf("live no-topic rows = %d; want 2 (no UNIQUE collision)", count)
	}
}

// TestLocalWrite_TwoNoTopicDeletes_NoCollision verifies that two independent
// no-topic deletes (different sync_ids, &"" TopicKey) produce two tombstone
// rows without a UNIQUE violation on memory_tombstones_topic_uidx or idx_mem_topic.
func TestLocalWrite_TwoNoTopicDeletes_NoCollision(t *testing.T) {
	s := openTempStore(t)
	empty := ""
	at := baseT.Add(1 * time.Second)

	// Seed two live rows, then delete both.
	for i, syncID := range []string{"sync-nd-1", "sync-nd-2"} {
		m := noTopicMut(syncID, &empty, 1, at.Add(time.Duration(i)*time.Second))
		if _, err := s.LocalWrite(m); err != nil {
			t.Fatalf("LocalWrite upsert %s: %v", syncID, err)
		}
	}
	for i, syncID := range []string{"sync-nd-1", "sync-nd-2"} {
		del := noTopicDeleteMut(syncID, &empty, 2, at.Add(time.Duration(2+i)*time.Second))
		if _, err := s.LocalWrite(del); err != nil {
			t.Fatalf("LocalWrite delete %s: %v", syncID, err)
		}
	}

	// Both tombstones must exist with NULL topic_key.
	var count int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_tombstones WHERE topic_key IS NULL AND project = 'engram' AND scope = 'project'`,
	).Scan(&count); err != nil {
		t.Fatalf("count no-topic tombstones: %v", err)
	}
	if count != 2 {
		t.Errorf("no-topic tombstone rows = %d; want 2 (no UNIQUE collision)", count)
	}
}

// TestApplyPulled_EmptyTopicKey_StoresNULL verifies that ApplyPulled with
// TopicKey=&"" also stores topic_key as SQL NULL (defensive normalisation path).
func TestApplyPulled_EmptyTopicKey_StoresNULL(t *testing.T) {
	s := openTempStore(t)
	empty := ""
	m := noTopicMut("sync-pulled-null", &empty, 1, baseT.Add(1*time.Second))
	m = normalizeMutation(m) // simulate: a central store already normalised, but caller passed &""

	// Overwrite TopicKey back to &"" to simulate an imperfect caller.
	m.TopicKey = &empty

	if err := s.ApplyPulled(m); err != nil {
		t.Fatalf("ApplyPulled: %v", err)
	}

	var topicKey *string
	err := s.db.QueryRow(
		`SELECT topic_key FROM memories WHERE sync_id = 'sync-pulled-null'`,
	).Scan(&topicKey)
	if err != nil {
		t.Fatalf("SELECT topic_key: %v", err)
	}
	if topicKey != nil {
		t.Errorf("ApplyPulled topic_key = %q; want SQL NULL", *topicKey)
	}
}

// ── Per-project pull cursor tests ─────────────────────────────────────────────

// TestListProjects returns the union of distinct project names from memories and
// memory_tombstones, sorted alphabetically, with no duplicates.
func TestListProjects(t *testing.T) {
	s := openTempStore(t)

	at := baseT.Add(1 * time.Second)

	// Write memories for projects "alpha" and "bravo".
	writeMut := func(syncID, project, scope string) domain.Mutation {
		return domain.Mutation{
			Op:         domain.OpUpsert,
			SyncID:     syncID,
			SessionID:  "sess",
			EntityType: domain.EntityMemory,
			Type:       "manual",
			Title:      "title",
			Content:    "content",
			Project:    project,
			Scope:      scope,
			WriterID:   "w",
			UpdatedAt:  at,
		}
	}
	deleteMut := func(syncID, project, scope string) domain.Mutation {
		return domain.Mutation{
			Op:         domain.OpDelete,
			SyncID:     syncID,
			SessionID:  "sess",
			EntityType: domain.EntityMemory,
			Type:       "manual",
			Title:      "title",
			Project:    project,
			Scope:      scope,
			WriterID:   "w",
			UpdatedAt:  at.Add(time.Second),
			Version:    2,
		}
	}

	// alpha appears in memories.
	if _, err := s.LocalWrite(writeMut("lp-a1", "alpha", "project")); err != nil {
		t.Fatalf("LocalWrite alpha: %v", err)
	}
	// bravo appears in memories.
	if _, err := s.LocalWrite(writeMut("lp-b1", "bravo", "project")); err != nil {
		t.Fatalf("LocalWrite bravo: %v", err)
	}
	// charlie: write then delete — appears in memory_tombstones only (tombstone-only project).
	if _, err := s.LocalWrite(writeMut("lp-c1", "charlie", "project")); err != nil {
		t.Fatalf("LocalWrite charlie: %v", err)
	}
	if _, err := s.LocalWrite(deleteMut("lp-c1", "charlie", "project")); err != nil {
		t.Fatalf("delete charlie: %v", err)
	}

	projects, err := s.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}

	want := []string{"alpha", "bravo", "charlie"}
	if len(projects) != len(want) {
		t.Fatalf("ListProjects: got %v, want %v", projects, want)
	}
	for i, p := range want {
		if projects[i] != p {
			t.Errorf("ListProjects[%d] = %q, want %q", i, projects[i], p)
		}
	}

	// A second write for alpha (same project) must not duplicate it.
	if _, err := s.LocalWrite(writeMut("lp-a2", "alpha", "project")); err != nil {
		t.Fatalf("LocalWrite alpha2: %v", err)
	}
	projects2, err := s.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects after second alpha write: %v", err)
	}
	if len(projects2) != 3 {
		t.Errorf("ListProjects after dup write: got %v, want {alpha, bravo, charlie}", projects2)
	}
}

// TestListProjects_Empty returns nil (not an error) when the store has no rows.
func TestListProjects_Empty(t *testing.T) {
	s := openTempStore(t)
	projects, err := s.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects on empty store: %v", err)
	}
	if len(projects) != 0 {
		t.Errorf("ListProjects on empty store: got %v, want []", projects)
	}
}

// TestPullCursorFor_PerProjectIndependent proves that PullCursorFor and
// SetPullCursorFor track each project's cursor independently:
//   - A fresh store returns 0 for any project.
//   - Advancing project A does not affect project B.
//   - Each project's cursor is monotonic: a lower seq is silently ignored.
func TestPullCursorFor_PerProjectIndependent(t *testing.T) {
	s := openTempStore(t)

	// Fresh store: both projects return 0.
	got, err := s.PullCursorFor("projectA")
	if err != nil {
		t.Fatalf("PullCursorFor A (fresh): %v", err)
	}
	if got != 0 {
		t.Errorf("fresh PullCursorFor A = %d, want 0", got)
	}
	got, err = s.PullCursorFor("projectB")
	if err != nil {
		t.Fatalf("PullCursorFor B (fresh): %v", err)
	}
	if got != 0 {
		t.Errorf("fresh PullCursorFor B = %d, want 0", got)
	}

	// Advance A to 10 — B must remain at 0.
	if err := s.SetPullCursorFor("projectA", 10); err != nil {
		t.Fatalf("SetPullCursorFor A=10: %v", err)
	}
	gotA, err := s.PullCursorFor("projectA")
	if err != nil {
		t.Fatalf("PullCursorFor A after set: %v", err)
	}
	if gotA != 10 {
		t.Errorf("PullCursorFor A = %d, want 10", gotA)
	}
	gotB, err := s.PullCursorFor("projectB")
	if err != nil {
		t.Fatalf("PullCursorFor B after A set: %v", err)
	}
	if gotB != 0 {
		t.Errorf("PullCursorFor B = %d, want 0 (must not be affected by A)", gotB)
	}

	// Advance B to 5 — A must remain at 10.
	if err := s.SetPullCursorFor("projectB", 5); err != nil {
		t.Fatalf("SetPullCursorFor B=5: %v", err)
	}
	gotA, err = s.PullCursorFor("projectA")
	if err != nil {
		t.Fatalf("PullCursorFor A after B set: %v", err)
	}
	if gotA != 10 {
		t.Errorf("PullCursorFor A = %d after B set, want 10 (must not be affected by B)", gotA)
	}
	gotB, err = s.PullCursorFor("projectB")
	if err != nil {
		t.Fatalf("PullCursorFor B after set: %v", err)
	}
	if gotB != 5 {
		t.Errorf("PullCursorFor B = %d, want 5", gotB)
	}

	// Monotonic: lower seq must NOT rewind A's cursor.
	if err := s.SetPullCursorFor("projectA", 3); err != nil {
		t.Fatalf("SetPullCursorFor A=3 (lower than 10): %v", err)
	}
	gotA, err = s.PullCursorFor("projectA")
	if err != nil {
		t.Fatalf("PullCursorFor A after lower set: %v", err)
	}
	if gotA != 10 {
		t.Errorf("PullCursorFor A = %d after lower set, want 10 (cursor must not rewind)", gotA)
	}

	// Advance A further — confirms forward progression works after a rewind attempt.
	if err := s.SetPullCursorFor("projectA", 20); err != nil {
		t.Fatalf("SetPullCursorFor A=20: %v", err)
	}
	gotA, err = s.PullCursorFor("projectA")
	if err != nil {
		t.Fatalf("PullCursorFor A final: %v", err)
	}
	if gotA != 20 {
		t.Errorf("PullCursorFor A = %d, want 20", gotA)
	}
}
