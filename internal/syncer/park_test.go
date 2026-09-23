package syncer_test

// Tests for FUP-004's push-side outbox parking: syncer.Push must PARK a
// mutation central rejects permanently (400/413/422) instead of retrying it
// forever, stop the REST of that mutation's sync_id group (order matters
// within a version chain) without touching any OTHER group, and
// syncer.SyncAllProjects must let pull proceed after a push failure UNLESS
// that failure is a fatal auth problem (401/403).

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/syncer"
)

// parkCentral is a test-only Central whose Apply behavior is driven by a
// per-sync_id error map and which records every mutation it was actually
// asked to Apply, IN CALL ORDER — the thing the group-stop assertions need:
// a parked group's LATER entries must never even reach Apply.
type parkCentral struct {
	mu sync.Mutex

	// applyErrFor maps sync_id → the error Apply returns for EVERY mutation
	// carrying that sync_id. Absent/nil → success.
	applyErrFor map[string]error

	applied []domain.Mutation // every mutation Apply was called with, in order

	pullResults map[string][]domain.Mutation
	pullCalled  []string
}

func (c *parkCentral) Apply(_ context.Context, m domain.Mutation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applied = append(c.applied, m)
	if err := c.applyErrFor[m.SyncID]; err != nil {
		return err
	}
	return nil
}

func (c *parkCentral) PullSince(_ context.Context, project string, since int64, _ int) ([]domain.Mutation, error) {
	c.mu.Lock()
	c.pullCalled = append(c.pullCalled, project)
	muts := c.pullResults[project]
	c.mu.Unlock()

	var out []domain.Mutation
	for _, mut := range muts {
		if mut.Seq > since {
			out = append(out, mut)
		}
	}
	return out, nil
}

func (c *parkCentral) appliedSyncIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.applied))
	for i, m := range c.applied {
		out[i] = m.SyncID
	}
	return out
}

func (c *parkCentral) pullCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pullCalled)
}

// statusErr (see also syncer_discovery_test.go's identical-shaped statusErr in
// this same package) carries an HTTP status the way remote.StatusError does,
// without importing internal/remote.
type parkStatusErr struct {
	code int
	msg  string
}

func (e *parkStatusErr) Error() string   { return e.msg }
func (e *parkStatusErr) StatusCode() int { return e.code }

// writeVersion is a small helper: writes sync_id at the given version/time,
// under project, through node.Write (LocalWrite + outbox enqueue).
func writeVersion(t *testing.T, node *syncer.Node, project, syncID string, version int, at time.Time) {
	t.Helper()
	_, err := node.Write(domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "title",
		Content:    "content",
		Project:    project,
		Scope:      "project",
		Version:    version,
		WriterID:   "writer",
		UpdatedAt:  at,
	})
	if err != nil {
		t.Fatalf("Write(%s, v%d): %v", syncID, version, err)
	}
}

// ── Push: parking ────────────────────────────────────────────────────────────

// TestPush_ParksPermanentlyRejectedEntry is the core FUP-004 regression test:
// a 422 from central.Apply must park the outbox entry (visible via ListParked)
// rather than leaving it pending forever, and Push itself must not treat the
// park as a Push-level error — other, unrelated entries pushed fine.
func TestPush_ParksPermanentlyRejectedEntry(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "park-basic")

	writeVersion(t, node, "proj", "sync-rejected", 1, time.Date(2025, 1, 1, 0, 0, 1, 0, time.UTC))
	writeVersion(t, node, "proj", "sync-ok", 1, time.Date(2025, 1, 1, 0, 0, 2, 0, time.UTC))

	central := &parkCentral{
		applyErrFor: map[string]error{
			"sync-rejected": &parkStatusErr{code: 422, msg: "rejected by constraint"},
		},
	}

	pushed, err := syncer.Push(ctx, node, central)
	if err != nil {
		t.Fatalf("Push: %v (a park must not surface as a Push error)", err)
	}
	// seed ("testproject" from openNode) + sync-ok = 2 successfully pushed;
	// sync-rejected is parked, not counted as pushed.
	if pushed != 2 {
		t.Errorf("Push pushed=%d, want 2 (seed + sync-ok; sync-rejected parked, not counted)", pushed)
	}

	parked, err := node.Store.ListParked()
	if err != nil {
		t.Fatalf("ListParked: %v", err)
	}
	if len(parked) != 1 {
		t.Fatalf("ListParked returned %d entries, want 1", len(parked))
	}
	if parked[0].Project != "proj" {
		t.Errorf("parked entry project = %q, want %q", parked[0].Project, "proj")
	}
	if parked[0].Attempts != 1 {
		t.Errorf("parked entry attempts = %d, want 1", parked[0].Attempts)
	}

	// The un-rejected entries must have actually reached central.
	applied := central.appliedSyncIDs()
	found := map[string]bool{}
	for _, sid := range applied {
		found[sid] = true
	}
	if !found["sync-ok"] {
		t.Errorf("sync-ok was never applied; applied=%v", applied)
	}
}

// TestPush_ParkStopsRestOfSameGroupOnly proves the ordering guarantee: a
// parked entry stops the REST of its OWN sync_id's version chain (order
// matters — a later version must not apply out of turn while an earlier one
// sits rejected), but a DIFFERENT sync_id's group is entirely unaffected.
func TestPush_ParkStopsRestOfSameGroupOnly(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "park-group")

	// Two versions of the SAME sync_id — both enqueued before any push, so
	// Push sees them as one group in local_seq order.
	writeVersion(t, node, "proj", "sync-group", 1, time.Date(2025, 1, 1, 0, 0, 1, 0, time.UTC))
	writeVersion(t, node, "proj", "sync-group", 2, time.Date(2025, 1, 1, 0, 0, 2, 0, time.UTC))
	// An unrelated group that must be entirely unaffected.
	writeVersion(t, node, "proj", "sync-other", 1, time.Date(2025, 1, 1, 0, 0, 3, 0, time.UTC))

	central := &parkCentral{
		applyErrFor: map[string]error{
			"sync-group": &parkStatusErr{code: 400, msg: "malformed"},
		},
	}

	if _, err := syncer.Push(ctx, node, central); err != nil {
		t.Fatalf("Push: %v", err)
	}

	applied := central.appliedSyncIDs()
	groupCalls := 0
	for _, sid := range applied {
		if sid == "sync-group" {
			groupCalls++
		}
	}
	// Exactly ONE call for sync-group: the first (rejected, parked) version.
	// The second version must NEVER have reached Apply at all.
	if groupCalls != 1 {
		t.Errorf("Apply was called %d time(s) for sync-group, want exactly 1 (the group must stop after the park); applied=%v",
			groupCalls, applied)
	}

	otherFound := false
	for _, sid := range applied {
		if sid == "sync-other" {
			otherFound = true
		}
	}
	if !otherFound {
		t.Errorf("sync-other (a different group) was never applied; applied=%v", applied)
	}

	// Both queued sync-group versions are gone from DrainOutbox: the first is
	// parked, and the second is BLOCKED behind it (withheld until the head is
	// retried or discarded) — but still unacked and counted on the parked head,
	// proving it is held rather than lost.
	entries, err := node.Store.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	for _, e := range entries {
		if e.Mutation.SyncID == "sync-group" {
			t.Errorf("DrainOutbox returned sync-group v%d after its head was parked; it must be withheld",
				e.Mutation.Version)
		}
	}
	parked, err := node.Store.ListParked()
	if err != nil {
		t.Fatalf("ListParked: %v", err)
	}
	if len(parked) != 1 || parked[0].BlockedBehind != 1 {
		t.Errorf("ListParked = %+v, want one parked head with BlockedBehind=1 (the held second version)", parked)
	}
}

// chainCentral rejects exactly ONE mutation (by version) of one sync_id with a
// permanent 422 while reject is set, and records every Apply in call order.
type chainCentral struct {
	parkCentral
	rejectSyncID  string
	rejectVersion int
	reject        bool
}

func (c *chainCentral) Apply(ctx context.Context, m domain.Mutation) error {
	c.mu.Lock()
	c.applied = append(c.applied, m)
	rejected := c.reject && m.SyncID == c.rejectSyncID && m.Version == c.rejectVersion
	c.mu.Unlock()
	if rejected {
		return &parkStatusErr{code: 422, msg: "rejected head"}
	}
	return nil
}

// appliedVersionsOf lists the versions Apply saw for syncID, in call order.
func (c *chainCentral) appliedVersionsOf(syncID string) []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []int
	for _, m := range c.applied {
		if m.SyncID == syncID {
			out = append(out, m.Version)
		}
	}
	return out
}

// TestPush_ParkedHeadBlocksChainAcrossCycles is the ordered-chain invariant
// ACROSS push cycles: with version 1 of a sync_id parked, versions 2 and 3 must
// never reach central on any later cycle (they used to, out of order, on the
// very next DrainOutbox) — and must go out, in order, once the head is either
// retried or discarded.
func TestPush_ParkedHeadBlocksChainAcrossCycles(t *testing.T) {
	for _, release := range []string{"retry", "discard"} {
		t.Run(release, func(t *testing.T) {
			ctx := context.Background()
			node := openNode(t, "park-chain-"+release)

			for v := 1; v <= 3; v++ {
				writeVersion(t, node, "proj", "sync-chain", v, time.Date(2025, 1, 1, 0, 0, v, 0, time.UTC))
			}
			central := &chainCentral{rejectSyncID: "sync-chain", rejectVersion: 1, reject: true}

			for cycle := 1; cycle <= 3; cycle++ {
				if _, err := syncer.Push(ctx, node, central); err != nil {
					t.Fatalf("Push cycle %d: %v", cycle, err)
				}
			}
			if got := central.appliedVersionsOf("sync-chain"); !slices.Equal(got, []int{1}) {
				t.Fatalf("after 3 cycles with v1 parked, Apply saw versions %v, want [1] — later versions leaked out of order", got)
			}

			parked, err := node.Store.ListParked()
			if err != nil || len(parked) != 1 {
				t.Fatalf("ListParked = %+v, %v; want exactly the parked v1", parked, err)
			}
			if parked[0].BlockedBehind != 2 {
				t.Errorf("BlockedBehind = %d, want 2 (v2 and v3 held behind the parked head)", parked[0].BlockedBehind)
			}
			backlog, err := node.Store.SyncBacklog()
			if err != nil {
				t.Fatalf("SyncBacklog: %v", err)
			}
			if backlog.Pending != 0 {
				t.Errorf("SyncBacklog.Pending = %d, want 0 — blocked rows are not waiting for the next tick", backlog.Pending)
			}

			central.mu.Lock()
			central.reject = false
			central.mu.Unlock()
			want := []int{1, 1, 2, 3} // the rejected attempt, then the retried head and its chain in order
			switch release {
			case "retry":
				err = node.Store.UnparkMutation(parked[0].LocalSeq)
			case "discard":
				err = node.Store.DiscardMutation(parked[0].LocalSeq)
				want = []int{1, 2, 3} // the head is dropped, never resent
			}
			if err != nil {
				t.Fatalf("%s head: %v", release, err)
			}

			if _, err := syncer.Push(ctx, node, central); err != nil {
				t.Fatalf("Push after %s: %v", release, err)
			}
			if got := central.appliedVersionsOf("sync-chain"); !slices.Equal(got, want) {
				t.Errorf("after %s, Apply saw versions %v, want %v (in order)", release, got, want)
			}
			if n, err := node.Store.PendingCount(); err != nil || n != 0 {
				t.Errorf("PendingCount after %s = %d, %v; want 0", release, n, err)
			}
		})
	}
}

// ── SyncAllProjects: push failures no longer skip pull, except 401/403 ──────

// TestSyncAllProjects_FatalPushErrorSkipsPull pins the one case that STILL
// stops the whole cycle: an auth failure. Pulling under the same broken
// credential would fail identically, so PullSince must never even be called.
func TestSyncAllProjects_FatalPushErrorSkipsPull(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "park-fatal")

	writeVersion(t, node, "proj", "sync-auth", 1, time.Date(2025, 1, 1, 0, 0, 1, 0, time.UTC))

	central := &parkCentral{
		applyErrFor: map[string]error{
			"sync-auth": &parkStatusErr{code: 403, msg: "forbidden"},
		},
		pullResults: map[string][]domain.Mutation{"proj": {makeMutation("proj", 1)}},
	}

	_, pulled, err := syncer.SyncAllProjects(ctx, node, central)
	if err == nil {
		t.Fatal("SyncAllProjects: want a non-nil error for a 403 push failure")
	}
	if pulled != 0 {
		t.Errorf("SyncAllProjects pulled=%d, want 0 — a fatal push error must skip pull entirely", pulled)
	}
	if n := central.pullCallCount(); n != 0 {
		t.Errorf("PullSince was called %d time(s), want 0 — a fatal push error must skip pull entirely", n)
	}
}

// TestSyncAllProjects_ParkedPushStillPulls is the FUP-004 headline behavior
// change: a permanently-rejected (parked) push entry must NOT skip pull —
// central may still have mutations worth applying even while this node's own
// push is stuck on one bad entry.
func TestSyncAllProjects_ParkedPushStillPulls(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "park-still-pulls")

	writeVersion(t, node, "proj", "sync-rejected-2", 1, time.Date(2025, 1, 1, 0, 0, 1, 0, time.UTC))

	central := &parkCentral{
		applyErrFor: map[string]error{
			"sync-rejected-2": &parkStatusErr{code: 422, msg: "rejected"},
		},
		pullResults: map[string][]domain.Mutation{"proj": {makeMutation("proj", 1)}},
	}

	_, pulled, err := syncer.SyncAllProjects(ctx, node, central)
	if err != nil {
		t.Fatalf("SyncAllProjects: %v (a parked entry must not surface as a sync error)", err)
	}
	if pulled == 0 {
		t.Error("SyncAllProjects pulled=0, want pull to have proceeded despite the parked push entry")
	}
	if n := central.pullCallCount(); n == 0 {
		t.Error("PullSince was never called — a parked push entry must not skip pull")
	}
}

// TestSyncAllProjects_RetryablePushStillPulls covers the other non-fatal
// case: a 5xx/network push failure is still reported as an error (the Loop
// must back off), but pull still runs.
func TestSyncAllProjects_RetryablePushStillPulls(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "park-retryable-still-pulls")

	writeVersion(t, node, "proj", "sync-flaky-2", 1, time.Date(2025, 1, 1, 0, 0, 1, 0, time.UTC))

	central := &parkCentral{
		applyErrFor: map[string]error{
			"sync-flaky-2": errors.New("dial tcp: connection refused"), // no status at all
		},
		pullResults: map[string][]domain.Mutation{"proj": {makeMutation("proj", 1)}},
	}

	_, pulled, err := syncer.SyncAllProjects(ctx, node, central)
	if err == nil {
		t.Fatal("SyncAllProjects: want a non-nil error for a retryable push failure (the Loop must back off)")
	}
	if pulled == 0 {
		t.Error("SyncAllProjects pulled=0, want pull to have proceeded despite the retryable push failure")
	}
	if n := central.pullCallCount(); n == 0 {
		t.Error("PullSince was never called — a retryable push failure must not skip pull")
	}
}

// TestLoop_ParkedEntryNotRetriedEachTick is TestLoop_NonRetryableNoHotLoop's
// intent under FUP-004's mechanism. A permanent Apply rejection no longer
// surfaces as a cycle error at all: Push parks the entry and returns nil, so
// the Loop sees a successful round and keeps its normal Interval cadence. What
// keeps that from being a hot loop against central is that DrainOutbox
// excludes parked rows — the rejected mutation must reach Apply exactly ONCE,
// however many ticks run afterwards.
func TestLoop_ParkedEntryNotRetriedEachTick(t *testing.T) {
	node := openNode(t, "park-loop")
	writeVersion(t, node, "proj", "sync-rejected-loop", 1, time.Date(2025, 1, 1, 0, 0, 1, 0, time.UTC))

	central := &parkCentral{
		applyErrFor: map[string]error{
			"sync-rejected-loop": &parkStatusErr{code: 422, msg: "rejected"},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := syncer.NewLoop(node, central, fastCfg())
	l.Start(ctx)

	// Every round pulls both projects (testproject + proj), so 20 pull calls is
	// ~10 rounds after the one that parked the entry.
	deadline := time.Now().Add(5 * time.Second)
	for central.pullCallCount() < 20 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	l.Stop()

	if n := central.pullCallCount(); n < 20 {
		t.Fatalf("loop made only %d pull calls in 5s — too few rounds to prove anything", n)
	}
	rejectedApplies := 0
	for _, sid := range central.appliedSyncIDs() {
		if sid == "sync-rejected-loop" {
			rejectedApplies++
		}
	}
	if rejectedApplies != 1 {
		t.Errorf("parked entry reached Apply %d times over %d pull calls, want exactly 1 — "+
			"a parked entry is being retried every tick", rejectedApplies, central.pullCallCount())
	}
	parked, err := node.Store.ListParked()
	if err != nil {
		t.Fatalf("ListParked: %v", err)
	}
	if len(parked) != 1 {
		t.Errorf("ListParked returned %d entries, want 1", len(parked))
	}
}
