//go:build acceptance

// Package centralstore_test — acceptance coverage for Store.PullSince against a
// REAL Postgres instance (embedded-postgres, started once per package in TestMain
// defined in store_acceptance_test.go).
//
// Each test uses newIsolatedStore for a hermetic per-test schema and drives
// Store.Apply or Store.InsertMutation to populate central_mutations, then calls
// Store.PullSince and asserts the returned slice.
//
// Invariant coverage:
//
//	TestPullSince_AllInOrder           — N inserts; PullSince(0,100) returns all N in ascending seq
//	TestPullSince_Cursor               — PullSince(k,100) returns only mutations with seq > k
//	TestPullSince_ProjectIsolation     — mutations in project B are NOT returned for project A
//	TestPullSince_Limit                — PullSince(0,2) returns at most 2 (the lowest-seq 2)
//	TestPullSince_NilVsEmptyTopicKey   — nil TopicKey and &"" TopicKey round-trip distinctly
package centralstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/mutation"
)

// ── TestPullSince_AllInOrder ──────────────────────────────────────────────────

// TestPullSince_AllInOrder inserts N mutations for a project via Store.Apply,
// then calls PullSince(project, 0, 100) and asserts:
//   - the returned slice has exactly N mutations,
//   - seq values are strictly ascending,
//   - each reconstructed Mutation matches what was stored (mutation_id, sync_id,
//     op, topic_key, version, updated_at, writer_id).
func TestPullSince_AllInOrder(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	const proj = "proj-pull-order"
	const n = 5
	base := time.Now().UTC().Truncate(time.Microsecond) // Postgres TIMESTAMPTZ has µs precision

	// Build and apply N mutations; capture what we expect to get back.
	type expected struct {
		mutationID string
		syncID     string
		op         domain.Op
		topicKey   *string
		version    int
		updatedAt  time.Time
		writerID   string
	}
	wants := make([]expected, n)

	for i := range n {
		syncID := "sync-order-" + itoa(i)
		mutID := "mut-order-" + itoa(i)
		tk := "sdd/test/order-" + itoa(i)
		updatedAt := base.Add(time.Duration(i) * time.Second)

		m := domain.Mutation{
			MutationID: mutID,
			Op:         domain.OpUpsert,
			SyncID:     syncID,
			SessionID:  "sess-order",
			EntityType: domain.EntityMemory,
			Type:       "manual",
			Title:      "Order test " + itoa(i),
			Content:    "content " + itoa(i),
			Project:    proj,
			Scope:      "project",
			TopicKey:   &tk,
			Version:    i + 1,
			WriterID:   "writer-pull",
			UpdatedAt:  updatedAt,
			OccurredAt: updatedAt,
		}
		m.Payload = mutation.CanonicalPayload(m)

		if err := store.Apply(ctx, m); err != nil {
			t.Fatalf("Apply mutation %d: %v", i, err)
		}
		wants[i] = expected{
			mutationID: mutID,
			syncID:     syncID,
			op:         domain.OpUpsert,
			topicKey:   &tk,
			version:    i + 1,
			updatedAt:  updatedAt,
			writerID:   "writer-pull",
		}
	}

	// Pull since the beginning.
	got, err := store.PullSince(ctx, proj, 0, 100)
	if err != nil {
		t.Fatalf("PullSince: %v", err)
	}
	if len(got) != n {
		t.Fatalf("PullSince: got %d mutations, want %d", len(got), n)
	}

	// Assert strictly ascending seq.
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Errorf("seq not strictly ascending at index %d: seq[%d]=%d seq[%d]=%d",
				i, i-1, got[i-1].Seq, i, got[i].Seq)
		}
	}

	// Assert content fields match what was stored.
	for i, g := range got {
		w := wants[i]
		if g.MutationID != w.mutationID {
			t.Errorf("[%d] MutationID: got %q, want %q", i, g.MutationID, w.mutationID)
		}
		if g.SyncID != w.syncID {
			t.Errorf("[%d] SyncID: got %q, want %q", i, g.SyncID, w.syncID)
		}
		if g.Op != w.op {
			t.Errorf("[%d] Op: got %q, want %q", i, g.Op, w.op)
		}
		if g.TopicKey == nil || *g.TopicKey != *w.topicKey {
			t.Errorf("[%d] TopicKey: got %v, want %q", i, g.TopicKey, *w.topicKey)
		}
		if g.Version != w.version {
			t.Errorf("[%d] Version: got %d, want %d", i, g.Version, w.version)
		}
		if !g.UpdatedAt.Equal(w.updatedAt) {
			t.Errorf("[%d] UpdatedAt: got %v, want %v", i, g.UpdatedAt, w.updatedAt)
		}
		if g.WriterID != w.writerID {
			t.Errorf("[%d] WriterID: got %q, want %q", i, g.WriterID, w.writerID)
		}
	}
}

// ── TestPullSince_Cursor ──────────────────────────────────────────────────────

// TestPullSince_Cursor inserts several mutations, then verifies that
// PullSince(project, k, 100) returns only mutations with seq > k, not all.
func TestPullSince_Cursor(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	const proj = "proj-pull-cursor"

	// Insert 4 mutations. We capture the seq of the 2nd one to use as the cursor.
	mutations := []domain.Mutation{
		pullMutation("mut-cursor-0", "sync-cursor-0", proj),
		pullMutation("mut-cursor-1", "sync-cursor-1", proj),
		pullMutation("mut-cursor-2", "sync-cursor-2", proj),
		pullMutation("mut-cursor-3", "sync-cursor-3", proj),
	}
	for _, m := range mutations {
		if err := store.Apply(ctx, m); err != nil {
			t.Fatalf("Apply %s: %v", m.MutationID, err)
		}
	}

	// Pull all to discover the actual seq values.
	all, err := store.PullSince(ctx, proj, 0, 100)
	if err != nil {
		t.Fatalf("PullSince(0): %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("expected 4 mutations, got %d", len(all))
	}

	// Use the seq of the 2nd row as the cursor.
	cursor := all[1].Seq

	// Pull since cursor — must return only mutations 2 and 3.
	after, err := store.PullSince(ctx, proj, cursor, 100)
	if err != nil {
		t.Fatalf("PullSince(cursor): %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("PullSince(cursor=%d): got %d mutations, want 2", cursor, len(after))
	}
	for _, g := range after {
		if g.Seq <= cursor {
			t.Errorf("PullSince(cursor=%d): returned mutation with seq=%d (<= cursor)", cursor, g.Seq)
		}
	}

	// The mutation IDs must match the 3rd and 4th inserted mutations.
	if after[0].MutationID != all[2].MutationID {
		t.Errorf("cursor[0].MutationID=%q, want %q", after[0].MutationID, all[2].MutationID)
	}
	if after[1].MutationID != all[3].MutationID {
		t.Errorf("cursor[1].MutationID=%q, want %q", after[1].MutationID, all[3].MutationID)
	}
}

// ── TestPullSince_ProjectIsolation ────────────────────────────────────────────

// TestPullSince_ProjectIsolation inserts mutations under two distinct projects
// and verifies that PullSince for project A does NOT return project B mutations.
func TestPullSince_ProjectIsolation(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	const projA = "proj-iso-A"
	const projB = "proj-iso-B"

	mA := pullMutation("mut-iso-a", "sync-iso-a", projA)
	mB1 := pullMutation("mut-iso-b1", "sync-iso-b1", projB)
	mB2 := pullMutation("mut-iso-b2", "sync-iso-b2", projB)

	for _, m := range []domain.Mutation{mA, mB1, mB2} {
		if err := store.Apply(ctx, m); err != nil {
			t.Fatalf("Apply %s: %v", m.MutationID, err)
		}
	}

	// Pull for project A — must return exactly 1 mutation.
	gotA, err := store.PullSince(ctx, projA, 0, 100)
	if err != nil {
		t.Fatalf("PullSince projA: %v", err)
	}
	if len(gotA) != 1 {
		t.Fatalf("PullSince projA: got %d mutations, want 1", len(gotA))
	}
	if gotA[0].MutationID != mA.MutationID {
		t.Errorf("PullSince projA: got mutation_id=%q, want %q", gotA[0].MutationID, mA.MutationID)
	}
	if gotA[0].Project != projA {
		t.Errorf("PullSince projA: project=%q, want %q", gotA[0].Project, projA)
	}

	// Pull for project B — must return exactly 2 mutations, none from A.
	gotB, err := store.PullSince(ctx, projB, 0, 100)
	if err != nil {
		t.Fatalf("PullSince projB: %v", err)
	}
	if len(gotB) != 2 {
		t.Fatalf("PullSince projB: got %d mutations, want 2", len(gotB))
	}
	for _, g := range gotB {
		if g.Project != projB {
			t.Errorf("PullSince projB: returned mutation with project=%q (not %q)", g.Project, projB)
		}
		if g.MutationID == mA.MutationID {
			t.Errorf("PullSince projB: returned projA mutation %q", mA.MutationID)
		}
	}
}

// ── TestPullSince_Limit ───────────────────────────────────────────────────────

// TestPullSince_Limit inserts more mutations than the limit and verifies that
// PullSince(project, 0, 2) returns only the 2 with the lowest seq values.
func TestPullSince_Limit(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	const proj = "proj-pull-limit"

	// Insert 5 mutations.
	ids := make([]string, 5)
	for i := range 5 {
		mutID := "mut-lim-" + itoa(i)
		ids[i] = mutID
		m := pullMutation(mutID, "sync-lim-"+itoa(i), proj)
		if err := store.Apply(ctx, m); err != nil {
			t.Fatalf("Apply %s: %v", mutID, err)
		}
	}

	// Pull with limit=2 — must return exactly 2 results.
	got, err := store.PullSince(ctx, proj, 0, 2)
	if err != nil {
		t.Fatalf("PullSince(limit=2): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("PullSince(limit=2): got %d, want 2", len(got))
	}

	// The 2 returned must be the lowest-seq 2 (insertion order for a fresh schema).
	// Pull all 5 to find which seqs they got.
	all, err := store.PullSince(ctx, proj, 0, 100)
	if err != nil {
		t.Fatalf("PullSince(all): %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("PullSince(all): got %d, want 5", len(all))
	}

	// The first 2 in the limited pull must match the first 2 in the full pull.
	if got[0].Seq != all[0].Seq || got[1].Seq != all[1].Seq {
		t.Errorf("limit=2 must return the 2 lowest-seq mutations: got seqs %d,%d want %d,%d",
			got[0].Seq, got[1].Seq, all[0].Seq, all[1].Seq)
	}
}

// ── TestPullSince_NilVsEmptyTopicKey ─────────────────────────────────────────

// TestPullSince_NilVsEmptyTopicKey inserts two mutations through the wire — one
// with TopicKey=nil and one with TopicKey=&"" — and verifies that PullSince
// reconstructs them with the correct distinct pointer semantics.
//
// This is the nil-vs-empty round-trip through the actual Postgres storage path
// (CanonicalPayload → central_mutations.payload → PullSince → FromCanonicalPayload).
func TestPullSince_NilVsEmptyTopicKey(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	const proj = "proj-pull-nilvsempty"

	emptyStr := ""

	// Mutation A: TopicKey = nil
	mNil := domain.Mutation{
		MutationID: "mut-nil-tk",
		Op:         domain.OpUpsert,
		SyncID:     "sync-nil-tk",
		SessionID:  "sess-test",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Nil topic key",
		Content:    "nil topic content",
		Project:    proj,
		Scope:      "project",
		TopicKey:   nil, // explicitly nil
		Version:    1,
		WriterID:   "writer-test",
		UpdatedAt:  time.Now().Add(-10 * time.Second).UTC(),
		OccurredAt: time.Now().Add(-10 * time.Second).UTC(),
	}
	mNil.Payload = mutation.CanonicalPayload(mNil)

	// Mutation B: TopicKey = &"" (empty string pointer — NOT nil)
	mEmpty := domain.Mutation{
		MutationID: "mut-empty-tk",
		Op:         domain.OpUpsert,
		SyncID:     "sync-empty-tk",
		SessionID:  "sess-test",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Empty topic key",
		Content:    "empty topic content",
		Project:    proj,
		Scope:      "project",
		TopicKey:   &emptyStr, // &"" — distinct from nil
		Version:    1,
		WriterID:   "writer-test",
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
	}
	mEmpty.Payload = mutation.CanonicalPayload(mEmpty)

	if err := store.Apply(ctx, mNil); err != nil {
		t.Fatalf("Apply nil TopicKey: %v", err)
	}
	if err := store.Apply(ctx, mEmpty); err != nil {
		t.Fatalf("Apply &\"\" TopicKey: %v", err)
	}

	// Pull both.
	got, err := store.PullSince(ctx, proj, 0, 100)
	if err != nil {
		t.Fatalf("PullSince: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 mutations, got %d", len(got))
	}

	// Find each by MutationID.
	var gotNil, gotEmpty *domain.Mutation
	for i := range got {
		switch got[i].MutationID {
		case "mut-nil-tk":
			gotNil = &got[i]
		case "mut-empty-tk":
			gotEmpty = &got[i]
		}
	}
	if gotNil == nil {
		t.Fatal("nil-TopicKey mutation not found in PullSince results")
	}
	if gotEmpty == nil {
		t.Fatal("empty-TopicKey mutation not found in PullSince results")
	}

	// nil TopicKey must round-trip as nil.
	if gotNil.TopicKey != nil {
		t.Errorf("nil TopicKey round-trip: got %v, want nil", gotNil.TopicKey)
	}

	// &"" TopicKey must round-trip as &"" — NOT nil.
	if gotEmpty.TopicKey == nil {
		t.Error("&\"\" TopicKey round-trip: got nil, want &\"\" (empty-string must not collapse to nil)")
	} else if *gotEmpty.TopicKey != "" {
		t.Errorf("&\"\" TopicKey round-trip: got %q, want \"\"", *gotEmpty.TopicKey)
	}

	// Sanity: the two mutations must produce distinct canonical IDs.
	if gotNil.MutationID == gotEmpty.MutationID {
		t.Error("nil and &\"\" TopicKey must produce distinct MutationIDs")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// pullMutation constructs a minimal domain.Mutation for PullSince tests with
// the Payload already set (via CanonicalPayload) so Store.Apply accepts it.
func pullMutation(mutID, syncID, project string) domain.Mutation {
	m := domain.Mutation{
		MutationID: mutID,
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess-pull",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Pull test " + syncID,
		Content:    "content for " + syncID,
		Project:    project,
		Scope:      "project",
		Version:    1,
		WriterID:   "writer-pull",
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
	}
	m.Payload = mutation.CanonicalPayload(m)
	return m
}

// itoa is a tiny helper — avoids importing strconv in the test file.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
