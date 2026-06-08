//go:build acceptance

// Package syncer_test — multi-project convergence acceptance proof.
//
// This test is the KEYSTONE proof for the multi-project autosync refactor
// (per-project pull cursors, PR #33).
//
// THE PROBLEM BEING PROVED:
// central_mutations.seq is a single global BIGSERIAL. If two projects A and B
// are pushed to central such that their seqs are INTERLEAVED (e.g. A@1, B@2,
// A@3, B@4), a global pull cursor would miss the lower-seq project's mutations
// after advancing past them:
//   - Pull A: cursor = 0 → sees A@1, A@3 (seqs 1 and 3); cursor advances to 3.
//   - Pull B: cursor = 3 → sees B@4 only; B@2 (seq 2, below cursor=3) is MISSED.
//
// THE FIX:
// Per-project cursors (pull_cursors table). Pull A with cursor=0 → A@1, A@3;
// set A's cursor to 3. Pull B with cursor=0 → B@2, B@4; set B's cursor to 4.
// Both projects' mutations land locally. Neither skips the other's interleaved seqs.
//
// THIS TEST PROVES IT:
//  1. Two writers push mutations for project A and project B to central such that
//     their central seqs are interleaved: A@seq1, B@seq2, A@seq3, B@seq4.
//  2. A fresh node runs SyncAllProjects (via the Loop or directly).
//  3. Assert ALL of A's mutations AND ALL of B's mutations are present locally.
//     If a global cursor were used, B@seq2 would be missed.
//
// Infrastructure mirrors autosync_convergence_acceptance_test.go:
//   - One embedded-postgres instance per package (TestMain in autosync_convergence_acceptance_test.go).
//   - Fresh schema per test.
//   - Fresh SQLite per node (t.TempDir).
//   - cloudserve httptest.Server with real per-writer HMAC auth.
package syncer_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/centralstore"
	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/syncer"
)

// TestMultiProject_InterleavedSeqs_BothProjectsConverge is the multi-project
// convergence acceptance proof.
//
// It proves that with per-project pull cursors, interleaved central seqs from
// two different projects are BOTH correctly pulled — the lower-seq project's
// mutations are not skipped after the higher-seq project advances its cursor.
//
// Setup:
//   - writerAB pushes mutations for BOTH projects, using Apply in seq order so
//     central assigns monotonically increasing global seqs in the interleaved order.
//   - writerNode is the node that will pull; it writes nothing, just pulls.
//
// Interleaving:
//
//	The test pushes mutations via separate Apply calls in the following order:
//	  projectA mutation 1 → gets seq N
//	  projectB mutation 1 → gets seq N+1
//	  projectA mutation 2 → gets seq N+2
//	  projectB mutation 2 → gets seq N+3
//	Central's BIGSERIAL assigns contiguous seqs in Apply order, so A and B
//	seqs are interleaved: A@N, B@N+1, A@N+2, B@N+3.
//
// With a GLOBAL cursor: pulling A advances cursor to N+2; pulling B with
// cursor=N+2 sees only B@N+3 — B@N+1 is silently missed.
// With PER-PROJECT cursors: pulling A with cursor=0 sees A@N and A@N+2
// (cursor → N+2). Pulling B with cursor=0 sees B@N+1 and B@N+3 (cursor → N+3).
// Both projects' mutations land locally.
func TestMultiProject_InterleavedSeqs_BothProjectsConverge(t *testing.T) {
	ctx := context.Background()

	// ── Central with real embedded-postgres ──────────────────────────────────
	newClient, store := autoHTTPCentral(t)

	// Provision HMAC keys for the writer that pushes and the node that pulls.
	writerAB := "writer-AB" // pushes both projects
	writerNode := "writer-node"
	if err := store.UpsertWriterKey(ctx, writerAB, autoTestKey(writerAB)); err != nil {
		t.Fatalf("UpsertWriterKey %s: %v", writerAB, err)
	}
	if err := store.UpsertWriterKey(ctx, writerNode, autoTestKey(writerNode)); err != nil {
		t.Fatalf("UpsertWriterKey %s: %v", writerNode, err)
	}

	clientAB := newClient(writerAB)
	clientNode := newClient(writerNode)

	// ── Push mutations to central in interleaved project order ───────────────
	//
	// We push directly via clientAB.Apply so central assigns contiguous global
	// seqs. The seq assignment order is:
	//   Apply(A1) → seq S
	//   Apply(B1) → seq S+1
	//   Apply(A2) → seq S+2
	//   Apply(B2) → seq S+3
	//
	// A and B seqs are interleaved. A node pulling with a global cursor would miss
	// B1 (seq S+1) after pulling A's seqs (which advances cursor to S+2).

	const (
		projectA = "proj-interleave-A"
		projectB = "proj-interleave-B"
		scope    = "project"
	)

	// Build mutations. Use distinct sync_ids and a unique topic per mutation so
	// they are always distinct and can be looked up individually.
	topicA1 := "multiproject/test/A1"
	topicA2 := "multiproject/test/A2"
	topicB1 := "multiproject/test/B1"
	topicB2 := "multiproject/test/B2"

	makeM := func(project, syncID, topic string, at time.Time) domain.Mutation {
		tk := topic
		return domain.Mutation{
			Op:         domain.OpUpsert,
			SyncID:     syncID,
			SessionID:  "sess-mp",
			EntityType: domain.EntityMemory,
			Type:       "manual",
			Title:      "title-" + syncID,
			Content:    "content-" + syncID,
			Project:    project,
			Scope:      scope,
			TopicKey:   &tk,
			Version:    1,
			UpdatedAt:  at,
			WriterID:   writerAB,
		}
	}

	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	mutA1 := makeM(projectA, "mp-A1", topicA1, base.Add(1*time.Second))
	mutB1 := makeM(projectB, "mp-B1", topicB1, base.Add(2*time.Second))
	mutA2 := makeM(projectA, "mp-A2", topicA2, base.Add(3*time.Second))
	mutB2 := makeM(projectB, "mp-B2", topicB2, base.Add(4*time.Second))

	// Apply in interleaved order: A, B, A, B — so central assigns seqs:
	//   mutA1 → seq N
	//   mutB1 → seq N+1  (interleaved — a global cursor would skip this)
	//   mutA2 → seq N+2
	//   mutB2 → seq N+3
	for _, m := range []domain.Mutation{mutA1, mutB1, mutA2, mutB2} {
		if err := clientAB.Apply(ctx, m); err != nil {
			t.Fatalf("clientAB.Apply(%s/%s): %v", m.Project, m.SyncID, err)
		}
	}

	// ── Verify the interleaving in central's journal ──────────────────────────
	// Pull all mutations for each project from central to observe their seqs.
	// We use a seq=0 cursor and no limit to get all.
	allA, err := clientAB.PullSince(ctx, projectA, 0, 1000)
	if err != nil {
		t.Fatalf("PullSince projectA to verify: %v", err)
	}
	allB, err := clientAB.PullSince(ctx, projectB, 0, 1000)
	if err != nil {
		t.Fatalf("PullSince projectB to verify: %v", err)
	}
	if len(allA) != 2 {
		t.Fatalf("central has %d A mutations, want 2 (interleaving pre-condition)", len(allA))
	}
	if len(allB) != 2 {
		t.Fatalf("central has %d B mutations, want 2 (interleaving pre-condition)", len(allB))
	}
	// Interleaving condition: B's lower seq must fall BETWEEN A's two seqs.
	// allA[0].Seq < allB[0].Seq < allA[1].Seq  (i.e. A@N, B@N+1, A@N+2, B@N+3
	// where allB[0].Seq = N+1 which is between allA[0].Seq=N and allA[1].Seq=N+2).
	if !(allA[0].Seq < allB[0].Seq && allB[0].Seq < allA[1].Seq) {
		t.Fatalf(
			"interleaving pre-condition NOT met: A seqs %d,%d, B seqs %d,%d — "+
				"need A[0].seq < B[0].seq < A[1].seq for the global-cursor skip to manifest",
			allA[0].Seq, allA[1].Seq, allB[0].Seq, allB[1].Seq,
		)
	}
	t.Logf("interleaving confirmed: A@%d, B@%d, A@%d, B@%d",
		allA[0].Seq, allB[0].Seq, allA[1].Seq, allB[1].Seq)

	// ── Fresh node pulls via SyncAllProjects ──────────────────────────────────
	// The node starts empty — it has no local memories, so ListProjects returns
	// nothing on the first SyncAllProjects call. To pull the interleaved projects,
	// we call syncer.Pull directly for each project (which is exactly what
	// SyncAllProjects does for each project in ListProjects). This accurately
	// tests the Pull-with-per-project-cursor path that is the crux of the fix.
	//
	// Alternatively, we write local seed mutations for each project first so
	// ListProjects discovers them, then call SyncAllProjects. We use the latter
	// approach to test the full production code path.

	dir := t.TempDir()
	st, err := localstore.Open(filepath.Join(dir, "node.db"))
	if err != nil {
		t.Fatalf("localstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node := syncer.NewNode("pull-node", st)

	// Seed local writes for both projects so ListProjects returns them.
	// These writes are pushed to central (harmless mock-idempotent via Apply).
	seedMut := func(project, syncID string) {
		t.Helper()
		_, err := node.Write(domain.Mutation{
			Op:         domain.OpUpsert,
			SyncID:     syncID,
			SessionID:  "sess-seed",
			EntityType: domain.EntityMemory,
			Type:       "manual",
			Title:      "seed",
			Project:    project,
			Scope:      scope,
			WriterID:   writerNode,
			UpdatedAt:  base,
		})
		if err != nil {
			t.Fatalf("seed Write %s: %v", syncID, err)
		}
	}
	seedMut(projectA, "seed-A")
	seedMut(projectB, "seed-B")

	// SyncAllProjects: Push (sends seed writes to central) + Pull per project.
	_, pulled, err := syncer.SyncAllProjects(ctx, node, clientNode)
	if err != nil {
		t.Fatalf("SyncAllProjects: %v", err)
	}
	t.Logf("SyncAllProjects pulled=%d", pulled)

	// ── THE KEYSTONE ASSERTION ────────────────────────────────────────────────
	// ALL four mutations must be present locally. If a global cursor were used:
	//   - After pulling A (cursor advanced to allA[1].Seq = N+2)
	//   - Pulling B with cursor=N+2 would only see B@N+3, missing B@N+1.
	// Per-project cursors: pulling B with cursor=0 sees B@N+1 AND B@N+3.

	assertPresent := func(topic, project string) {
		t.Helper()
		rec, err := node.Store.FindByTopic(topic, project, scope)
		if err != nil {
			t.Errorf("FindByTopic(%q, %q): %v", topic, project, err)
			return
		}
		if rec == nil {
			t.Errorf("MISSING: topic=%q project=%q — per-project cursor failed to pull this mutation", topic, project)
		} else {
			t.Logf("present: topic=%q project=%q content=%q", topic, project, rec.Content)
		}
	}

	// All of A's mutations must be present.
	assertPresent(topicA1, projectA)
	assertPresent(topicA2, projectA)
	// All of B's mutations must be present — B1 is the critical one (interleaved seq).
	assertPresent(topicB1, projectB)
	assertPresent(topicB2, projectB)

	// ── Per-project cursor correctness ────────────────────────────────────────
	// Each project's cursor must be ≥ the highest seq seen for that project.
	// It may be higher if other tests pushed to the same central before us
	// (each test uses an isolated Postgres schema so seqs are per-schema and
	// start at 1, but seed writes from autoNodeForProjects also advance seqs).
	curA, err := node.Store.PullCursorFor(projectA)
	if err != nil {
		t.Fatalf("PullCursorFor A: %v", err)
	}
	curB, err := node.Store.PullCursorFor(projectB)
	if err != nil {
		t.Fatalf("PullCursorFor B: %v", err)
	}
	if curA < allA[1].Seq {
		t.Errorf("A pull cursor = %d, want ≥ %d (highest A seq)", curA, allA[1].Seq)
	}
	if curB < allB[1].Seq {
		t.Errorf("B pull cursor = %d, want ≥ %d (highest B seq)", curB, allB[1].Seq)
	}
	// Crucially: A's cursor must not equal B's (independent per-project cursors).
	// (They can be equal only if A's highest seq == B's highest seq, which is
	// impossible for interleaved seqs from the same schema).
	if curA == curB && allA[1].Seq != allB[1].Seq {
		t.Errorf("A cursor (%d) == B cursor (%d): cursors are not independent (expected different highest seqs)",
			curA, curB)
	}
	t.Logf("per-project cursors: A=%d (highest A seq=%d), B=%d (highest B seq=%d)",
		curA, allA[1].Seq, curB, allB[1].Seq)

	// ── Central state ─────────────────────────────────────────────────────────
	// Cross-check: central must also have all four mutations.
	assertCentral := func(topic, project string) {
		t.Helper()
		rec, err := store.FindByTopic(topic, project, scope)
		if err != nil {
			t.Errorf("central.FindByTopic(%q, %q): %v", topic, project, err)
			return
		}
		if rec == nil {
			t.Errorf("central missing: topic=%q project=%q", topic, project)
		}
	}
	assertCentral(topicA1, projectA)
	assertCentral(topicA2, projectA)
	assertCentral(topicB1, projectB)
	assertCentral(topicB2, projectB)

	t.Logf("multi-project convergence: A@%d+%d, B@%d+%d — all four mutations present locally",
		allA[0].Seq, allA[1].Seq, allB[0].Seq, allB[1].Seq)
}

// TestMultiProject_LoopConvergesBothProjects proves that the autosync Loop
// (which drives SyncAllProjects per tick) converges BOTH projects. Node A writes
// for project P and project Q; node B (with loops running) must see all writes
// for both projects.
func TestMultiProject_LoopConvergesBothProjects(t *testing.T) {
	ctx := context.Background()

	newClient, store := autoHTTPCentral(t)

	writerA := "mp-loop-writer-A"
	writerB := "mp-loop-writer-B"
	for _, w := range []string{writerA, writerB} {
		if err := store.UpsertWriterKey(ctx, w, autoTestKey(w)); err != nil {
			t.Fatalf("UpsertWriterKey %s: %v", w, err)
		}
	}

	clientA := newClient(writerA)
	clientB := newClient(writerB)

	const (
		projectP = "loop-mp-P"
		projectQ = "loop-mp-Q"
		scope    = "project"
	)

	nodeA := autoNode(t, "mp-loop-A")
	// nodeB must be seeded with both projects so SyncAllProjects calls PullSince
	// for each. A fresh empty node has no projects in ListProjects, so the Loop
	// would never pull from central for either project. The writerID must match
	// the HMAC-authenticated writer (writerB) so Push doesn't get a 403.
	nodeB := autoNodeForProjects(t, "mp-loop-B", writerB, []string{projectP, projectQ})

	loopCfg := syncer.Config{
		Interval: 50 * time.Millisecond,
		Debounce: 10 * time.Millisecond,
	}

	loopA := syncer.NewLoop(nodeA, clientA, loopCfg)
	loopB := syncer.NewLoop(nodeB, clientB, loopCfg)
	loopA.Start(ctx)
	loopB.Start(ctx)
	defer func() {
		loopA.Stop()
		loopB.Stop()
	}()

	base := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	topicP := "multiproject/loop/P"
	topicQ := "multiproject/loop/Q"

	writeOnA := func(project, syncID, topic string, at time.Time) {
		t.Helper()
		tk := topic
		if _, err := nodeA.Write(domain.Mutation{
			Op:         domain.OpUpsert,
			SyncID:     syncID,
			SessionID:  "sess-ml",
			EntityType: domain.EntityMemory,
			Type:       "manual",
			Title:      "title-" + syncID,
			Content:    "content-" + syncID,
			Project:    project,
			Scope:      scope,
			TopicKey:   &tk,
			Version:    1,
			UpdatedAt:  at,
			WriterID:   writerA,
		}); err != nil {
			t.Fatalf("nodeA.Write %s: %v", syncID, err)
		}
	}

	// Write one mutation per project on node A.
	writeOnA(projectP, "ml-P1", topicP, base.Add(1*time.Second))
	writeOnA(projectQ, "ml-Q1", topicQ, base.Add(2*time.Second))
	loopA.Trigger()

	// Poll until node B has both topics, bounded by 5 seconds.
	deadline := time.Now().Add(5 * time.Second)
	const poll = 20 * time.Millisecond

	var recP, recQ *domain.Record
	for time.Now().Before(deadline) {
		if recP == nil {
			r, _ := nodeB.Store.FindByTopic(topicP, projectP, scope)
			recP = r
		}
		if recQ == nil {
			r, _ := nodeB.Store.FindByTopic(topicQ, projectQ, scope)
			recQ = r
		}
		if recP != nil && recQ != nil {
			break
		}
		time.Sleep(poll)
	}

	if recP == nil {
		t.Errorf("Loop: project=%q topic=%q never reached node B", projectP, topicP)
	} else {
		t.Logf("Loop convergence: project=%q topic=%q content=%q", projectP, topicP, recP.Content)
	}
	if recQ == nil {
		t.Errorf("Loop: project=%q topic=%q never reached node B", projectQ, topicQ)
	} else {
		t.Logf("Loop convergence: project=%q topic=%q content=%q", projectQ, topicQ, recQ.Content)
	}
}

// needsCentralStore is a compile-time guard ensuring centralstore.Store's
// FindByTopic method is accessible in this test file. If the type changes
// incompatibly the test will fail to compile rather than silently missing the
// assertion.
var _ = (*centralstore.Store)(nil)
