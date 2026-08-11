package syncer_test

// Tests for Pull's batch-halving retry — the v1.5.2 pull-wedge fix.
//
// The wedge: Pull asks central for up to pullLimit (1000) mutations per batch,
// central accepts pushed mutations up to ~1 MiB each, and remote.Client refuses to
// buffer a response over 4 MiB. A big-enough batch therefore fails EVERY time with
// the same overflow, and because the cursor only advances on a successful batch,
// every following round re-requested the identical oversized window forever. The
// fix is a transport-level sentinel (transport.ErrResponseTooLarge) plus a halving
// retry here, which makes forward progress unconditional.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/syncer"
	"github.com/mariesqu/engram/internal/transport"
)

// halvingCentral is a test-only Central that models a transport with a response
// size cap: it serves any PullSince whose limit is <= maxBatch and rejects
// anything larger with an error wrapping transport.ErrResponseTooLarge — the exact
// error shape remote.Client produces on overflow. Every requested limit is
// recorded so tests can assert the halving SEQUENCE, not just the end state.
type halvingCentral struct {
	mu sync.Mutex
	// maxBatch is the largest limit this central will serve. 0 rejects every
	// limit, including 1 — the pathological "one mutation is over the cap" case
	// that must surface as an error instead of looping.
	maxBatch int
	// limits records, in call order, every limit Pull asked for.
	limits []int
	// muts is central's authoritative per-project mutation list (seq-ascending).
	muts map[string][]domain.Mutation
	// errFor, when non-nil and returning non-nil for a limit, overrides the
	// maxBatch logic for that call. It lets a test script a NON-overflow failure
	// partway through the halving walk (mirrors mockCentral.errFn in loop_test.go).
	errFor func(limit int) error
}

// maxHalvingCalls bounds how many PullSince calls the mock will answer with the
// overflow sentinel. A correct halving loop needs at most log2(pullLimit)+1 ≈ 11
// calls; beyond that the loop is not converging, so the mock returns a
// NON-sentinel error to break out. Without this guard a regression would hang the
// test binary until the package timeout instead of failing with a readable message.
const maxHalvingCalls = 64

func (c *halvingCentral) Apply(_ context.Context, _ domain.Mutation) error { return nil }

func (c *halvingCentral) PullSince(_ context.Context, project string, since int64, limit int) ([]domain.Mutation, error) {
	c.mu.Lock()
	c.limits = append(c.limits, limit)
	calls := len(c.limits)
	maxBatch := c.maxBatch
	muts := c.muts[project]
	errFor := c.errFor
	c.mu.Unlock()

	if calls > maxHalvingCalls {
		return nil, errors.New("halvingCentral: halving loop is not terminating")
	}
	if errFor != nil {
		if err := errFor(limit); err != nil {
			return nil, err
		}
	}
	if limit > maxBatch {
		// Byte-for-byte the shape remote.Client returns, sentinel included.
		return nil, fmt.Errorf("remote.PullSince: response body exceeds cap of %d bytes: %w",
			4<<20, transport.ErrResponseTooLarge)
	}

	out := make([]domain.Mutation, 0, len(muts))
	for _, m := range muts {
		if m.Seq > since {
			out = append(out, m)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (c *halvingCentral) recordedLimits() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]int, len(c.limits))
	copy(cp, c.limits)
	return cp
}

// assertLimits compares the recorded PullSince limit sequence against want. The
// SEQUENCE is the assertion that matters, not just the value Pull settled on: it is
// the only observable proof that Pull halves step by step rather than, say, dropping
// straight to 1, retrying the same limit, or continuing after a non-overflow error.
func assertLimits(t *testing.T, got, want []int) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("PullSince limits = %v; want %v", got, want)
	}
}

// TestPull_ResponseTooLarge_HalvesLimitUntilBatchFits proves the recovery path:
// against a central that refuses batches over 250 rows, Pull must walk the limit
// down 1000 → 500 → 250, succeed on the third attempt, apply the mutations and
// advance the cursor. The cursor advancing is the whole point — that is what stops
// the next round from re-requesting the same oversized window.
func TestPull_ResponseTooLarge_HalvesLimitUntilBatchFits(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "pull-halving-recovers")

	const project = "halving"
	central := &halvingCentral{
		maxBatch: 250,
		muts: map[string][]domain.Mutation{
			project: {
				makeMutation(project, 1),
				makeMutation(project, 2),
				makeMutation(project, 3),
			},
		},
	}

	pulled, err := syncer.Pull(ctx, node, central, project)
	if err != nil {
		t.Fatalf("Pull: %v (halving retry must recover from an oversized batch)", err)
	}
	if pulled != 3 {
		t.Errorf("Pull returned %d; want 3 mutations", pulled)
	}

	// One halving step per overflow, then the settled limit again for the empty
	// batch that proves the project is drained. Pull terminates on an EMPTY batch,
	// never on a short one — a short batch is ambiguous (drained, or truncated by
	// central's response byte budget), so it always costs one extra round-trip.
	assertLimits(t, central.recordedLimits(), []int{1000, 500, 250, 250})

	// The mutations really landed — a successful retry must apply the batch, not
	// merely stop erroring.
	live, err := node.Store.CountLiveByProject(project)
	if err != nil {
		t.Fatalf("CountLiveByProject: %v", err)
	}
	if live != 3 {
		t.Errorf("live memories in %q = %d; want 3 (pulled batch was not applied)", project, live)
	}

	// Cursor advanced to the highest seq seen: the next round resumes AFTER this
	// batch instead of replaying it.
	cursor, err := node.Store.PullCursorFor(project)
	if err != nil {
		t.Fatalf("PullCursorFor: %v", err)
	}
	if cursor != 3 {
		t.Errorf("pull cursor = %d; want 3 (cursor must advance after a successful halved batch)", cursor)
	}
}

// TestPull_ResponseTooLarge_AtLimitOne_ReturnsError proves the halving loop has a
// floor. A central that rejects even a single-mutation batch is a genuine anomaly
// (that mutation could never have been pushed through the same transport), so Pull
// stops at limit=1 and surfaces the error — it does NOT spin forever, and it does
// NOT advance the cursor over data it never received.
func TestPull_ResponseTooLarge_AtLimitOne_ReturnsError(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "pull-halving-floor")

	const project = "halving-floor"
	// maxBatch 0 → every limit overflows, including 1.
	central := &halvingCentral{maxBatch: 0, muts: map[string][]domain.Mutation{}}

	pulled, err := syncer.Pull(ctx, node, central, project)
	if err == nil {
		t.Fatal("Pull returned nil against a central that rejects every batch size; want the overflow error")
	}
	if !errors.Is(err, transport.ErrResponseTooLarge) {
		t.Errorf("Pull error = %v; want it to preserve the transport.ErrResponseTooLarge chain", err)
	}
	if pulled != 0 {
		t.Errorf("Pull returned %d; want 0 on failure", pulled)
	}

	// The limits must halve all the way to 1 and then stop: 1000, 500, …, 1.
	assertLimits(t, central.recordedLimits(), []int{1000, 500, 250, 125, 62, 31, 15, 7, 3, 1})

	// A failed pull must leave the cursor untouched: nothing was applied, so
	// re-requesting the same window next round is the correct behaviour.
	cursor, err := node.Store.PullCursorFor(project)
	if err != nil {
		t.Fatalf("PullCursorFor: %v", err)
	}
	if cursor != 0 {
		t.Errorf("pull cursor = %d; want 0 (a failed pull must never advance the cursor)", cursor)
	}
}

// TestPull_ResponseTooLarge_DrainsInOneCall is the guarantee the whole fix exists
// for: a backlog too big for ONE response still drains completely, and does so
// within a single Pull call. Pull keeps fetching until a batch comes back empty,
// committing the cursor after each batch, so a size-truncated response is just a
// smaller step — never a stall.
//
// This also pins why "short batch" cannot mean "drained": here a short batch means
// "the transport could not carry more", and against a byte-budgeting central it
// means the same thing. Only an EMPTY batch is unambiguous.
//
// maxBatch is 3, not 2, on purpose: halving from 1000 visits 1000, 500, …, 3, 1 —
// it never lands on 2, so a maxBatch of 2 would settle at limit=1 and test
// single-row batches instead of truncation.
func TestPull_ResponseTooLarge_DrainsInOneCall(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "pull-halving-drain")

	const project = "halving-drain"
	central := &halvingCentral{
		maxBatch: 3,
		muts: map[string][]domain.Mutation{
			project: {
				makeMutation(project, 1),
				makeMutation(project, 2),
				makeMutation(project, 3),
				makeMutation(project, 4),
				makeMutation(project, 5),
			},
		},
	}

	pulled, err := syncer.Pull(ctx, node, central, project)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if pulled != 5 {
		t.Errorf("pulled = %d; want 5 — ONE call must drain the whole backlog, not one batch of it", pulled)
	}

	cursor, err := node.Store.PullCursorFor(project)
	if err != nil {
		t.Fatalf("PullCursorFor: %v", err)
	}
	if cursor != 5 {
		t.Errorf("pull cursor = %d; want 5 (backlog fully drained)", cursor)
	}
	live, err := node.Store.CountLiveByProject(project)
	if err != nil {
		t.Fatalf("CountLiveByProject: %v", err)
	}
	if live != 5 {
		t.Errorf("live memories in %q = %d; want 5", project, live)
	}

	// The full conversation, and every step of it is deliberate:
	//   1000…3  — halving ladder until the batch fits (batch 1 → 3 rows);
	//   6       — AIMD growth: batch 1 came back COMPLETELY full, so try double;
	//   3       — the growth overflowed, halve straight back (batch 2 → 2 rows);
	//   3       — batch 2 was short, so ask once more; empty ⇒ drained.
	assertLimits(t, central.recordedLimits(),
		[]int{1000, 500, 250, 125, 62, 31, 15, 7, 3, 6, 3, 3})
}

// TestPull_StickyLimit_ReusedAcrossCalls proves the halving ladder is paid ONCE per
// project, not once per sync round. The limit Pull settles on is remembered on the
// Node, so the next call opens at that value instead of walking 1000 → … → 3 again.
//
// Without stickiness a node with fat rows burns ~9 futile round-trips per project
// on EVERY autosync round, forever — which is degraded pacing rather than a bug,
// and exactly the kind of quiet tax that never gets noticed.
func TestPull_StickyLimit_ReusedAcrossCalls(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "pull-sticky")

	const project = "sticky"
	central := &halvingCentral{
		maxBatch: 3,
		muts: map[string][]domain.Mutation{
			project: {makeMutation(project, 1), makeMutation(project, 2)},
		},
	}

	// First call: walks the ladder down to 3 and drains both rows.
	if _, err := syncer.Pull(ctx, node, central, project); err != nil {
		t.Fatalf("Pull (first): %v", err)
	}
	firstCalls := len(central.recordedLimits())

	// Second call against a drained project: one request, at the REMEMBERED limit.
	if _, err := syncer.Pull(ctx, node, central, project); err != nil {
		t.Fatalf("Pull (second): %v", err)
	}
	second := central.recordedLimits()[firstCalls:]
	assertLimits(t, second, []int{3})
}

// TestPull_AIMD_GrowsBackTowardCeiling pins the "additive increase" half of the
// policy. Halving alone is a ratchet: one fat backlog would pin a project at a tiny
// batch size for the lifetime of the process even after its rows shrank back to
// normal. Growth on a completely-full batch is what lets throughput recover.
//
// Here central refuses anything over 3 rows until the limit first settles, then
// accepts any size — modelling a backlog whose fat rows are behind it. The limit
// must climb 3 → 6 → 12 → 24 → 48 → 96, doubling only after batches that come back
// full, and never past pullLimit.
func TestPull_AIMD_GrowsBackTowardCeiling(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "pull-aimd")

	const (
		project = "aimd"
		total   = 100
	)
	muts := make([]domain.Mutation, 0, total)
	for i := 1; i <= total; i++ {
		muts = append(muts, makeMutation(project, int64(i)))
	}

	settled := false
	central := &halvingCentral{
		maxBatch: pullLimitForTests, // the mock's own cap never binds; errFor decides
		muts:     map[string][]domain.Mutation{project: muts},
		errFor: func(limit int) error {
			if settled || limit <= 3 {
				settled = true // the fat rows are behind us; anything goes from here
				return nil
			}
			return fmt.Errorf("remote.PullSince: response body exceeds cap of %d bytes: %w",
				4<<20, transport.ErrResponseTooLarge)
		},
	}

	pulled, err := syncer.Pull(ctx, node, central, project)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if pulled != total {
		t.Errorf("pulled = %d; want %d", pulled, total)
	}

	// Skip the initial halving ladder and assert the growth curve that follows.
	got := central.recordedLimits()
	ladder := []int{1000, 500, 250, 125, 62, 31, 15, 7, 3}
	if len(got) < len(ladder) {
		t.Fatalf("PullSince limits = %v; want at least the halving ladder %v", got, ladder)
	}
	assertLimits(t, got[:len(ladder)], ladder)
	// 3 (3 rows) → 6 → 12 → 24 → 48 fill up 93 rows; the batch at 96 returns the
	// last 7 (short ⇒ no growth), and one more at 96 comes back empty ⇒ drained.
	assertLimits(t, got[len(ladder):], []int{6, 12, 24, 48, 96, 96})
}

// pullLimitForTests mirrors the package's pullLimit ceiling. The tests live in
// syncer_test (black box), so the unexported constant is not visible; duplicating
// it here keeps the mocks honest about the ceiling they are modelling.
const pullLimitForTests = 1000

// TestPull_NonSentinelErrorMidHalving_AbortsImmediately proves the halving loop is
// scoped to ONE condition. Overflow is the only error that means "ask for less";
// a 500, an auth failure or a dropped connection means the server or network is
// unhealthy, and hammering it 9 more times with shrinking limits would turn one
// failed round-trip into a burst of pointless load — while also delaying the
// Loop's backoff, which is the correct response to those errors.
func TestPull_NonSentinelErrorMidHalving_AbortsImmediately(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "pull-halving-abort")

	const project = "halving-abort"
	boom := errors.New("central exploded")
	central := &halvingCentral{
		maxBatch: 1000, // irrelevant: errFor decides both calls
		muts:     map[string][]domain.Mutation{},
		errFor: func(limit int) error {
			switch limit {
			case 1000:
				// Overflow → Pull is expected to halve.
				return fmt.Errorf("remote.PullSince: response body exceeds cap of %d bytes: %w",
					4<<20, transport.ErrResponseTooLarge)
			case 500:
				// A plain failure → Pull must give up here, not keep halving.
				return boom
			default:
				return nil
			}
		},
	}

	pulled, err := syncer.Pull(ctx, node, central, project)
	if err == nil {
		t.Fatal("Pull returned nil after a non-overflow error; want that error surfaced")
	}
	if !errors.Is(err, boom) {
		t.Errorf("Pull error = %v; want it to wrap the central's own error", err)
	}
	if errors.Is(err, transport.ErrResponseTooLarge) {
		t.Errorf("Pull error = %v; want the LAST error, not the earlier overflow", err)
	}
	if pulled != 0 {
		t.Errorf("Pull returned %d; want 0 on failure", pulled)
	}

	// Exactly two calls: the overflow at 1000, then the fatal one at 500. A third
	// entry would mean Pull kept halving through a non-overflow failure.
	assertLimits(t, central.recordedLimits(), []int{1000, 500})

	cursor, err := node.Store.PullCursorFor(project)
	if err != nil {
		t.Fatalf("PullCursorFor: %v", err)
	}
	if cursor != 0 {
		t.Errorf("pull cursor = %d; want 0 (a failed pull must never advance the cursor)", cursor)
	}
}

// TestPull_NoOverflow_FullLimitThenEmptyConfirmation pins the happy path — the case
// that runs constantly and that a regression here would silently tax.
//
// When the transport accepts the full batch, Pull asks at pullLimit with no
// speculative shrinking and no probe call, and then asks EXACTLY ONCE MORE to see
// an empty batch. That second request is the price of having no has_more flag on
// the wire: one empty round-trip per active project per round, in exchange for a
// protocol where old and new clients and servers all agree on what "done" means.
// It must stay at exactly one — a regression that keeps re-asking would multiply
// traffic by the number of projects on every tick.
//
// No growth step appears here: the first batch (2 rows) was SHORT, so there is no
// evidence the row limit was the binding constraint.
func TestPull_NoOverflow_FullLimitThenEmptyConfirmation(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "pull-halving-happy")

	const project = "halving-happy"
	central := &halvingCentral{
		maxBatch: pullLimitForTests,
		muts: map[string][]domain.Mutation{
			project: {makeMutation(project, 1), makeMutation(project, 2)},
		},
	}

	pulled, err := syncer.Pull(ctx, node, central, project)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if pulled != 2 {
		t.Errorf("Pull returned %d; want 2", pulled)
	}
	assertLimits(t, central.recordedLimits(), []int{1000, 1000})

	cursor, err := node.Store.PullCursorFor(project)
	if err != nil {
		t.Fatalf("PullCursorFor: %v", err)
	}
	if cursor != 2 {
		t.Errorf("pull cursor = %d; want 2", cursor)
	}
}

// endlessCentral hands back exactly one fresh mutation per call, forever, always
// advancing the cursor. It models a central whose backlog grows at least as fast as
// the node drains it — the only thing that stops Pull is its own batch backstop.
type endlessCentral struct {
	mu      sync.Mutex
	project string
	calls   int
}

func (c *endlessCentral) Apply(_ context.Context, _ domain.Mutation) error { return nil }

func (c *endlessCentral) PullSince(_ context.Context, _ string, since int64, _ int) ([]domain.Mutation, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	// One row per call: enough to keep the drain loop alive, cheap enough that the
	// backstop is reached in 100 SQLite applies rather than 100 000.
	return []domain.Mutation{makeMutation(c.project, since+1)}, nil
}

func (c *endlessCentral) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestPull_DrainBackstop_StopsWithoutError pins the drain loop's pacing backstop.
//
// Draining "until empty" needs a floor under the pathological case where empty
// never comes — a firehose project, or a central bug. Pull stops at
// maxPullBatchesPerCall and returns NIL, not an error, deliberately: every batch it
// applied was committed and the cursor advanced, so this is not a failure, it is a
// yield. Returning an error would be actively harmful — the Loop would classify the
// round as failed and back off, slowing down the very node that has the most work
// to do. The next round resumes from the advanced cursor.
func TestPull_DrainBackstop_StopsWithoutError(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "pull-backstop")

	const project = "backstop"
	central := &endlessCentral{project: project}

	pulled, err := syncer.Pull(ctx, node, central, project)
	if err != nil {
		t.Fatalf("Pull: %v — hitting the backstop is a yield, not a failure", err)
	}
	if pulled != maxPullBatchesPerCallForTests {
		t.Errorf("pulled = %d; want %d (one row per batch, stopped by the backstop)",
			pulled, maxPullBatchesPerCallForTests)
	}
	if got := central.callCount(); got != maxPullBatchesPerCallForTests {
		t.Errorf("PullSince calls = %d; want %d — the backstop must cap the drain loop exactly",
			got, maxPullBatchesPerCallForTests)
	}

	// Progress was real and committed: the next round picks up from here.
	cursor, err := node.Store.PullCursorFor(project)
	if err != nil {
		t.Fatalf("PullCursorFor: %v", err)
	}
	if cursor != int64(maxPullBatchesPerCallForTests) {
		t.Errorf("pull cursor = %d; want %d (progress is kept when the backstop fires)",
			cursor, maxPullBatchesPerCallForTests)
	}
}

// maxPullBatchesPerCallForTests mirrors the package's maxPullBatchesPerCall for the
// black-box test package, like pullLimitForTests.
const maxPullBatchesPerCallForTests = 100
