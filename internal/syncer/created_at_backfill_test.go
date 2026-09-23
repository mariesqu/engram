package syncer_test

// Tests for syncer.BackfillCreatedAt (FUP-005c): the paging driver that
// fetches original creation times from central and applies them locally,
// exactly once per project.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/syncer"
)

// createdAtFakeCentral is a test-only Central implementing the OPTIONAL
// createdAtSource capability. pages[i] is returned for the i-th call
// (1-indexed); running past the configured pages returns an empty (drained)
// page, exactly like the real server does. errFrom, when > 0, makes every
// call from that call number onward return err instead.
type createdAtFakeCentral struct {
	mu sync.Mutex

	pages   [][]domain.CreatedAtEntry
	err     error
	errFrom int // 0 means never error

	calls []string // the `after` cursor passed on each call, in order
}

func (c *createdAtFakeCentral) Apply(context.Context, domain.Mutation) error { return nil }

func (c *createdAtFakeCentral) PullSince(context.Context, string, int64, int) ([]domain.Mutation, error) {
	return nil, nil
}

func (c *createdAtFakeCentral) OriginalCreatedAt(_ context.Context, _, after string, _ int) ([]domain.CreatedAtEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, after)
	callNum := len(c.calls)

	if c.errFrom > 0 && callNum >= c.errFrom {
		return nil, c.err
	}
	idx := callNum - 1
	if idx >= len(c.pages) {
		return nil, nil
	}
	return c.pages[idx], nil
}

func (c *createdAtFakeCentral) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// noCreatedAtCapability is a Central that does NOT implement createdAtSource
// — reuses multiProjectCentral (syncallprojects_test.go, same package),
// which only implements Apply/PullSince.
func noCreatedAtCapability() *multiProjectCentral {
	return newMultiProjectCentral(nil)
}

func seedLocalRow(t *testing.T, node *syncer.Node, syncID, project string, createdAt time.Time) {
	t.Helper()
	writeVersion(t, node, project, syncID, 1, createdAt)
}

// TestBackfillCreatedAt_PagesAppliesAndMarksDone covers the success path
// end to end: multiple pages are fetched with an advancing keyset cursor,
// applied to matching local rows, and the project is marked done.
func TestBackfillCreatedAt_PagesAppliesAndMarksDone(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "backfill-success")

	newer := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	seedLocalRow(t, node, "sync-bf-1", "bfproj", newer)
	seedLocalRow(t, node, "sync-bf-2", "bfproj", newer)

	older1 := time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC)
	older2 := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	central := &createdAtFakeCentral{
		pages: [][]domain.CreatedAtEntry{
			{{SyncID: "sync-bf-1", CreatedAt: older1}},
			{{SyncID: "sync-bf-2", CreatedAt: older2}},
		},
	}

	ran, err := syncer.BackfillCreatedAt(ctx, node, central, "bfproj")
	if err != nil {
		t.Fatalf("BackfillCreatedAt: %v", err)
	}
	if !ran {
		t.Error("BackfillCreatedAt returned ran=false, want true (completed this call)")
	}

	// Paging must have advanced: page 2's `after` is page 1's last sync_id.
	if got := central.calls; len(got) != 3 || got[0] != "" || got[1] != "sync-bf-1" || got[2] != "sync-bf-2" {
		t.Errorf("calls = %v, want [\"\" \"sync-bf-1\" \"sync-bf-2\"] (empty page 3 confirms the drain)", got)
	}

	done, err := node.Store.CreatedAtBackfillDone("bfproj")
	if err != nil {
		t.Fatalf("CreatedAtBackfillDone: %v", err)
	}
	if !done {
		t.Error("CreatedAtBackfillDone = false after a successful BackfillCreatedAt")
	}

	var createdAt1, createdAt2 string
	if err := node.Store.DB().QueryRow(`SELECT created_at FROM memories WHERE sync_id = 'sync-bf-1'`).Scan(&createdAt1); err != nil {
		t.Fatalf("query sync-bf-1: %v", err)
	}
	if err := node.Store.DB().QueryRow(`SELECT created_at FROM memories WHERE sync_id = 'sync-bf-2'`).Scan(&createdAt2); err != nil {
		t.Fatalf("query sync-bf-2: %v", err)
	}
	if !parseSQLiteTime(t, createdAt1).Equal(older1) {
		t.Errorf("sync-bf-1 created_at = %s, want %v", createdAt1, older1)
	}
	if !parseSQLiteTime(t, createdAt2).Equal(older2) {
		t.Errorf("sync-bf-2 created_at = %s, want %v", createdAt2, older2)
	}
}

// TestBackfillCreatedAt_AlreadyDoneMakesNoNetworkCalls proves the steady-state
// cost claim: once marked done, a later call touches OriginalCreatedAt zero
// times.
func TestBackfillCreatedAt_AlreadyDoneMakesNoNetworkCalls(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "backfill-already-done")

	if err := node.Store.MarkCreatedAtBackfillDone("bfproj"); err != nil {
		t.Fatalf("MarkCreatedAtBackfillDone: %v", err)
	}

	central := &createdAtFakeCentral{pages: [][]domain.CreatedAtEntry{{{SyncID: "sync-x", CreatedAt: time.Now()}}}}

	ran, err := syncer.BackfillCreatedAt(ctx, node, central, "bfproj")
	if err != nil {
		t.Fatalf("BackfillCreatedAt: %v", err)
	}
	if ran {
		t.Error("BackfillCreatedAt returned ran=true for an already-completed project")
	}
	if n := central.callCount(); n != 0 {
		t.Errorf("OriginalCreatedAt was called %d time(s), want 0 for an already-done project", n)
	}
}

// TestBackfillCreatedAt_CapabilityAbsentIsANoOp covers a Central that does not
// implement createdAtSource at all (an older/lightweight test double) — must
// not error, must not mark the project done (so a LATER call against a
// capable Central still runs).
func TestBackfillCreatedAt_CapabilityAbsentIsANoOp(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "backfill-no-capability")

	ran, err := syncer.BackfillCreatedAt(ctx, node, noCreatedAtCapability(), "bfproj")
	if err != nil {
		t.Fatalf("BackfillCreatedAt: %v", err)
	}
	if ran {
		t.Error("BackfillCreatedAt returned ran=true against a Central with no createdAtSource capability")
	}
	done, err := node.Store.CreatedAtBackfillDone("bfproj")
	if err != nil {
		t.Fatalf("CreatedAtBackfillDone: %v", err)
	}
	if done {
		t.Error("project marked done despite the capability being absent")
	}
}

// TestBackfillCreatedAt_404SkipsQuietlyAndStaysRetryable is the compatibility
// contract: an OLDER server's 404 must not error, and must NOT mark the
// project done, so the backfill is retried the next time it is attempted
// (e.g. after the server is upgraded).
func TestBackfillCreatedAt_404SkipsQuietlyAndStaysRetryable(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "backfill-404")

	central := &createdAtFakeCentral{errFrom: 1, err: &parkStatusErr{code: 404, msg: "not found"}}

	ran, err := syncer.BackfillCreatedAt(ctx, node, central, "bfproj")
	if err != nil {
		t.Fatalf("BackfillCreatedAt: %v (a 404 must be treated as unsupported, not a failure)", err)
	}
	if ran {
		t.Error("BackfillCreatedAt returned ran=true for a 404 response")
	}
	done, err := node.Store.CreatedAtBackfillDone("bfproj")
	if err != nil {
		t.Fatalf("CreatedAtBackfillDone: %v", err)
	}
	if done {
		t.Error("project marked done despite a 404 (unsupported) response — it must stay retryable")
	}
}

// TestBackfillCreatedAt_GenericErrorPropagatesAndStaysRetryable covers a
// genuine failure (not a compatibility signal): the error must propagate, and
// the project must NOT be marked done.
func TestBackfillCreatedAt_GenericErrorPropagatesAndStaysRetryable(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "backfill-generic-error")

	central := &createdAtFakeCentral{errFrom: 1, err: errors.New("connection reset")}

	_, err := syncer.BackfillCreatedAt(ctx, node, central, "bfproj")
	if err == nil {
		t.Fatal("BackfillCreatedAt: want a non-nil error for a generic failure")
	}
	done, doneErr := node.Store.CreatedAtBackfillDone("bfproj")
	if doneErr != nil {
		t.Fatalf("CreatedAtBackfillDone: %v", doneErr)
	}
	if done {
		t.Error("project marked done despite a genuine fetch error")
	}
}

// TestBackfillCreatedAt_ResumesFromScratchAfterInterruption covers the
// idempotent/resumable contract: a run interrupted mid-paging leaves the
// project not-done, and a LATER call (even restarting from page one) still
// converges to the same correct end state.
func TestBackfillCreatedAt_ResumesFromScratchAfterInterruption(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "backfill-resume")

	older := time.Date(2017, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	seedLocalRow(t, node, "sync-bf-resume", "bfproj", newer)

	// First attempt: page 1 succeeds, page 2 fails.
	failing := &createdAtFakeCentral{
		pages: [][]domain.CreatedAtEntry{
			{{SyncID: "sync-bf-resume", CreatedAt: older}},
		},
		errFrom: 2,
		err:     errors.New("dropped connection"),
	}
	if _, err := syncer.BackfillCreatedAt(ctx, node, failing, "bfproj"); err == nil {
		t.Fatal("first BackfillCreatedAt call: want an error (interrupted mid-page)")
	}
	if done, _ := node.Store.CreatedAtBackfillDone("bfproj"); done {
		t.Fatal("project marked done after an interrupted run")
	}
	// The first page's effect DID apply (BackfillCreatedAt writes are
	// per-page, not all-or-nothing across the whole project) — confirming
	// "resumable" means safe to retry, not that partial progress is lost.
	var createdAt string
	if err := node.Store.DB().QueryRow(`SELECT created_at FROM memories WHERE sync_id = 'sync-bf-resume'`).Scan(&createdAt); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !parseSQLiteTime(t, createdAt).Equal(older) {
		t.Fatalf("created_at after the interrupted run = %s, want the FIRST page's value %v already applied", createdAt, older)
	}

	// Second attempt: a fully successful Central, restarting from page one.
	succeeding := &createdAtFakeCentral{
		pages: [][]domain.CreatedAtEntry{
			{{SyncID: "sync-bf-resume", CreatedAt: older}},
		},
	}
	ran, err := syncer.BackfillCreatedAt(ctx, node, succeeding, "bfproj")
	if err != nil {
		t.Fatalf("second BackfillCreatedAt call: %v", err)
	}
	if !ran {
		t.Error("second BackfillCreatedAt call: want ran=true")
	}
	if done, _ := node.Store.CreatedAtBackfillDone("bfproj"); !done {
		t.Error("project not marked done after the SECOND (successful) run")
	}
}

// parseSQLiteTime parses a stored created_at value using the same layout
// sessions.go's sqliteTimeLayout constant is documented to use, without this
// package needing to import localstore's unexported constant.
func parseSQLiteTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		t.Fatalf("parseSQLiteTime(%q): %v", s, err)
	}
	return parsed.UTC()
}
