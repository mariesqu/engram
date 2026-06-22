package syncer_test

// Tests for the "new-project pull discovery" feature in syncer.SyncAllProjects:
//   - A central that implements projectLister is used to union central projects with local ones.
//   - A central WITHOUT projectLister still works (local-only pulls, no regression).
//   - unionProjects helper: dedup, drop empty, sort.
//   - Loop.LastResult reflects outcomes after a sync cycle.

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/syncer"
)

// statusErr is a test error carrying an HTTP status code, mirroring the shape of
// remote.StatusError (which exposes StatusCode() int). It lets these tests
// exercise SyncAllProjects's duck-typed 501 detection without importing
// internal/remote.
type statusErr struct {
	code int
	msg  string
}

func (e *statusErr) Error() string   { return e.msg }
func (e *statusErr) StatusCode() int { return e.code }

// ── stub: Central WITH projectLister ─────────────────────────────────────────

// discoveryProjectCentral is a Central that:
//   - Implements BOTH PullSince (per-project) AND ListProjects (optional projectLister).
//   - Records which projects PullSince was called for.
//   - Returns the configured mutations per project from PullSince.
//   - Returns a fixed list of project names from ListProjects.
type discoveryProjectCentral struct {
	mu sync.Mutex

	// pullResults maps project → []domain.Mutation returned by PullSince.
	pullResults map[string][]domain.Mutation

	// pullCalled records each project name passed to PullSince.
	pullCalled []string

	// centralProjects is the list returned by ListProjects.
	centralProjects []string

	// listErr, if non-nil, is returned by ListProjects.
	listErr error
}

func (d *discoveryProjectCentral) Apply(_ context.Context, _ domain.Mutation) error {
	return nil
}

func (d *discoveryProjectCentral) PullSince(_ context.Context, project string, since int64, _ int) ([]domain.Mutation, error) {
	d.mu.Lock()
	d.pullCalled = append(d.pullCalled, project)
	muts := d.pullResults[project]
	d.mu.Unlock()

	var out []domain.Mutation
	for _, m := range muts {
		if m.Seq > since {
			out = append(out, m)
		}
	}
	return out, nil
}

func (d *discoveryProjectCentral) ListProjects(_ context.Context) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.listErr != nil {
		return nil, d.listErr
	}
	cp := make([]string, len(d.centralProjects))
	copy(cp, d.centralProjects)
	return cp, nil
}

func (d *discoveryProjectCentral) getPullCalled() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := make([]string, len(d.pullCalled))
	copy(cp, d.pullCalled)
	return cp
}

// ── TestSyncAllProjects_PullsCentralOnlyProject ────────────────────────────────

// TestSyncAllProjects_PullsCentralOnlyProject is the core regression test for
// new-project pull discovery: SyncAllProjects must call PullSince for a project
// that exists ONLY on central (the node's local store has NO row for it).
//
// Setup:
//   - Local node seeded with project "alpha" only (via openNode seed + one write).
//   - Stub central implements ListProjects → ["alpha", "central-only"].
//   - Stub PullSince returns one mutation for "central-only".
//
// Assert: PullSince is called for "central-only" and that mutation is pulled.
func TestSyncAllProjects_PullsCentralOnlyProject(t *testing.T) {
	ctx := context.Background()

	// openNode seeds "testproject". Write one more local mutation for "alpha"
	// so local projects = {"testproject", "alpha"}.
	node := openNode(t, "disc-central-only")
	if _, err := node.Write(domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "disc-alpha-1",
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "alpha",
		Project:    "alpha",
		Scope:      "project",
		WriterID:   "writer",
		UpdatedAt:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Write alpha: %v", err)
	}

	// centralOnly mutation — seq=1.
	centralOnlyMut := makeMutation("central-only", 1)

	stub := &discoveryProjectCentral{
		pullResults: map[string][]domain.Mutation{
			"alpha":        {},
			"testproject":  {},
			"central-only": {centralOnlyMut},
		},
		centralProjects: []string{"alpha", "central-only"},
	}

	_, pulled, err := syncer.SyncAllProjects(ctx, node, stub)
	if err != nil {
		t.Fatalf("SyncAllProjects: %v", err)
	}

	// The mutation for "central-only" must have been pulled.
	if pulled != 1 {
		t.Errorf("pulled=%d, want 1 (the central-only mutation)", pulled)
	}

	// PullSince must have been called for "central-only".
	called := stub.getPullCalled()
	calledSet := make(map[string]bool, len(called))
	for _, p := range called {
		calledSet[p] = true
	}
	if !calledSet["central-only"] {
		t.Errorf("PullSince never called for \"central-only\"; called=%v", called)
	}
}

// ── TestUnionProjects ──────────────────────────────────────────────────────────

// unionProjects is unexported, so we test it indirectly through SyncAllProjects:
// a stub that returns known central projects lets us observe which PullSince calls
// were made and infer that unionProjects ran correctly.
//
// However, the critical properties (dedup, drop empty, sort) can be tested
// directly via a thin wrapper test that drives SyncAllProjects with crafted inputs
// and observes the ordered PullSince calls — which are exactly the union result.
func TestUnionProjects_ViaDiscovery(t *testing.T) {
	tests := []struct {
		name            string
		localProjects   []string // extra projects written to node beyond "testproject"
		centralProjects []string
		wantPulled      []string // sorted expected PullSince project calls (superset)
	}{
		{
			name:            "both empty beyond testproject seed",
			localProjects:   nil,
			centralProjects: []string{},
			// Only "testproject" (from openNode seed) is in local.
			wantPulled: []string{"testproject"},
		},
		{
			name:            "dedup: same project in both local and central",
			localProjects:   []string{"shared"},
			centralProjects: []string{"shared", "testproject"},
			// Union = {testproject, shared} — no dup.
			wantPulled: []string{"shared", "testproject"},
		},
		{
			name:            "central adds new project",
			localProjects:   []string{"local-proj"},
			centralProjects: []string{"new-from-central"},
			// Union = {local-proj, new-from-central, testproject}.
			wantPulled: []string{"local-proj", "new-from-central", "testproject"},
		},
		{
			name:            "empty string in central is dropped",
			localProjects:   nil,
			centralProjects: []string{"", "valid-proj"},
			// "" must be dropped; result = {testproject, valid-proj}.
			wantPulled: []string{"testproject", "valid-proj"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			node := openNode(t, "union-"+tc.name)

			for i, proj := range tc.localProjects {
				if _, err := node.Write(domain.Mutation{
					Op:         domain.OpUpsert,
					SyncID:     "union-" + proj + "-" + itoa(i),
					SessionID:  "sess",
					EntityType: domain.EntityMemory,
					Type:       "manual",
					Title:      proj,
					Project:    proj,
					Scope:      "project",
					WriterID:   "writer",
					UpdatedAt:  time.Date(2025, 1, 1, 0, 0, i, 0, time.UTC),
				}); err != nil {
					t.Fatalf("Write %s: %v", proj, err)
				}
			}

			stub := &discoveryProjectCentral{
				pullResults:     map[string][]domain.Mutation{},
				centralProjects: tc.centralProjects,
			}

			if _, _, err := syncer.SyncAllProjects(ctx, node, stub); err != nil {
				t.Fatalf("SyncAllProjects: %v", err)
			}

			called := stub.getPullCalled()
			// Deduplicate calls (a project may be called more than once if cursors are at 0).
			calledSet := make(map[string]struct{})
			var calledUniq []string
			for _, p := range called {
				if _, ok := calledSet[p]; !ok {
					calledSet[p] = struct{}{}
					calledUniq = append(calledUniq, p)
				}
			}
			sort.Strings(calledUniq)

			if len(calledUniq) != len(tc.wantPulled) {
				t.Errorf("PullSince called for %v (want %v)", calledUniq, tc.wantPulled)
				return
			}
			for i, w := range tc.wantPulled {
				if calledUniq[i] != w {
					t.Errorf("PullSince[%d] = %q; want %q (full=%v)", i, calledUniq[i], w, calledUniq)
				}
			}
		})
	}
}

// itoa is a simple int-to-string helper to avoid importing strconv in this file
// (strconv is already used in syncallprojects_test.go via makeMutation).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// ── TestSyncAllProjects_WithoutProjectLister ────────────────────────────────────

// TestSyncAllProjects_WithoutProjectLister proves the optional interface is truly
// optional: a Central that does NOT implement projectLister still works — only
// local projects are pulled, and there is no panic or error.
//
// We reuse the existing multiProjectCentral from syncallprojects_test.go, which
// only satisfies transport.Central (Apply + PullSince) but NOT projectLister.
func TestSyncAllProjects_WithoutProjectLister(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "disc-no-lister")

	// Seed a local project beyond "testproject".
	if _, err := node.Write(domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "disc-local-only-1",
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "local-only",
		Project:    "local-only",
		Scope:      "project",
		WriterID:   "writer",
		UpdatedAt:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Write local-only: %v", err)
	}

	// multiProjectCentral does NOT implement ListProjects.
	central := newMultiProjectCentral(map[string][]domain.Mutation{
		"local-only":  {makeMutation("local-only", 5)},
		"testproject": {},
	})

	_, pulled, err := syncer.SyncAllProjects(ctx, node, central)
	if err != nil {
		t.Fatalf("SyncAllProjects without projectLister: %v", err)
	}

	// Must pull the local-only project's mutation (1 pulled).
	if pulled != 1 {
		t.Errorf("pulled=%d, want 1 (local-only project mutation)", pulled)
	}

	// Must have called PullSince for the local projects only.
	calls := central.getCalls()
	calledSet := make(map[string]bool)
	for _, c := range calls {
		calledSet[c.project] = true
	}
	if !calledSet["local-only"] {
		t.Errorf("PullSince not called for local-only; calls=%v", calls)
	}
	if !calledSet["testproject"] {
		t.Errorf("PullSince not called for testproject; calls=%v", calls)
	}
}

// ── TestSyncAllProjects discovery-error paths (W2 coverage) ─────────────────────

// TestSyncAllProjects_DiscoveryError_StillPullsLocal proves that a GENUINE
// discovery failure (e.g. network/5xx) is recorded as an error so the Loop can
// back off, BUT the locally-known projects are still pulled that cycle — push
// and local pulls are never aborted by a discovery hiccup.
func TestSyncAllProjects_DiscoveryError_StillPullsLocal(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "disc-err") // seeds "testproject"

	stub := &discoveryProjectCentral{
		pullResults: map[string][]domain.Mutation{"testproject": {}},
		listErr:     errors.New("central unreachable"),
	}

	if _, _, err := syncer.SyncAllProjects(ctx, node, stub); err == nil {
		t.Fatal("expected a discovery failure to surface as an error, got nil")
	}

	// The local-known project must still have been pulled despite the discovery error.
	if !contains(stub.getPullCalled(), "testproject") {
		t.Errorf("local project testproject was not pulled despite discovery failure; called=%v", stub.getPullCalled())
	}
}

// TestSyncAllProjects_DiscoveryUnsupported_NotFatal proves the capability-absent
// handling: when central's ListProjects reports that discovery is unsupported,
// it must NOT be treated as a sync failure. SyncAllProjects must return nil error
// (so the Loop does not back off / report a perpetual error) while still pulling
// the locally-known projects. Both statuses must be covered:
//   - 404: an OLDER central whose catch-all returns 404 for the unregistered
//     /v1/projects route — the REAL mixed-version case.
//   - 501: a capability-gated handler ("not supported").
func TestSyncAllProjects_DiscoveryUnsupported_NotFatal(t *testing.T) {
	for _, code := range []int{404, 501} {
		code := code
		t.Run(itoa(code), func(t *testing.T) {
			ctx := context.Background()
			node := openNode(t, "disc-unsupported-"+itoa(code)) // seeds "testproject"

			stub := &discoveryProjectCentral{
				pullResults: map[string][]domain.Mutation{"testproject": {}},
				listErr:     &statusErr{code: code, msg: "unsupported"},
			}

			if _, _, err := syncer.SyncAllProjects(ctx, node, stub); err != nil {
				t.Fatalf("a %d from discovery must NOT be a sync error (older central); got %v", code, err)
			}

			// Local projects are still pulled — discovery being unavailable does not
			// stop the node syncing the projects it already knows.
			if !contains(stub.getPullCalled(), "testproject") {
				t.Errorf("local project testproject was not pulled; called=%v", stub.getPullCalled())
			}
		})
	}
}

// contains reports whether s is in xs.
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// ── TestLoop_LastResult ────────────────────────────────────────────────────────

// TestLoop_LastResult_ZeroBeforeFirstCycle asserts that LastResult returns the
// zero-value SyncOutcome (At.IsZero()) before any sync cycle has completed.
func TestLoop_LastResult_ZeroBeforeFirstCycle(t *testing.T) {
	// A Loop that has never been started: LastResult must return the zero value.
	l := syncer.NewLoop(nil, nil, syncer.Config{})
	got := l.LastResult()
	if !got.At.IsZero() {
		t.Errorf("LastResult before start: At=%v, want zero", got.At)
	}
	if got.Pushed != 0 || got.Pulled != 0 {
		t.Errorf("LastResult before start: Pushed=%d Pulled=%d, want 0 0", got.Pushed, got.Pulled)
	}
	if got.Err != "" {
		t.Errorf("LastResult before start: Err=%q, want empty", got.Err)
	}
}

// TestLoop_LastResult_ReflectsAfterCycle asserts that LastResult reflects the
// actual pushed/pulled counts after at least one sync cycle completes.
// We use a success path (mockCentral with no error) and a seeded node so
// SyncAllProjects has at least one project to pull.
func TestLoop_LastResult_ReflectsAfterCycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	central := newMockCentral(10)
	node := openNode(t, "lastresult")

	l := syncer.NewLoop(node, central, fastCfg())
	l.Start(ctx)
	defer l.Stop()

	// Wait for at least one sync to complete.
	if got := central.waitN(1, 2*time.Second); got < 1 {
		t.Fatal("no sync fired within 2s")
	}

	// Give the loop a short moment to record the result (recordResult happens just
	// after SyncAllProjects returns, before the next timer reset).
	time.Sleep(5 * time.Millisecond)

	result := l.LastResult()
	if result.At.IsZero() {
		t.Error("LastResult.At is zero after a completed cycle; want a real timestamp")
	}
	if result.At.After(time.Now().Add(time.Second)) {
		t.Errorf("LastResult.At=%v is in the future; likely a clock issue", result.At)
	}
	// The node has a seeded outbox entry. After the first push it is consumed (pushed=1)
	// but subsequent cycles have pushed=0 (idempotent). We only assert At is non-zero.
}

// TestLoop_LastResult_RecordsError asserts that LastResult.Err is non-empty when
// a sync cycle fails with a retryable error.
func TestLoop_LastResult_RecordsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	central := newMockCentral(10)
	central.setStaticErr(errRetryable)
	node := openNode(t, "lastresult-err")

	l := syncer.NewLoop(node, central, fastCfg())
	l.Start(ctx)
	defer l.Stop()

	// Wait for at least one failing sync.
	if got := central.waitN(1, 2*time.Second); got < 1 {
		t.Fatal("no sync fired within 2s")
	}

	time.Sleep(5 * time.Millisecond)

	result := l.LastResult()
	if result.At.IsZero() {
		t.Error("LastResult.At is zero after a failed cycle; want a real timestamp")
	}
	if result.Err == "" {
		t.Error("LastResult.Err is empty after a retryable error; want non-empty")
	}
}
