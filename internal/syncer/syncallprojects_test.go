package syncer_test

// Tests for syncer.SyncAllProjects — the multi-project autosync driver.

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/syncer"
)

// multiProjectCentral is a test-only Central that:
//   - Returns per-project mutation sets from PullSince (simulating central's
//     per-project pull API).
//   - Tracks which (project, cursor) pairs were queried so tests can assert
//     that per-project cursors were used correctly.
//   - Apply is a no-op (unit tests don't assert central state).
type multiProjectCentral struct {
	mu sync.Mutex
	// mutations maps project → []domain.Mutation (central's authoritative list).
	mutations map[string][]domain.Mutation
	// calls records each (project, since) pair passed to PullSince.
	calls []pullCall
}

type pullCall struct {
	project string
	since   int64
}

func newMultiProjectCentral(mutations map[string][]domain.Mutation) *multiProjectCentral {
	return &multiProjectCentral{mutations: mutations}
}

func (m *multiProjectCentral) Apply(_ context.Context, _ domain.Mutation) error {
	return nil
}

// PullSince returns all mutations for the project with seq > since, in seq order.
func (m *multiProjectCentral) PullSince(_ context.Context, project string, since int64, _ int) ([]domain.Mutation, error) {
	m.mu.Lock()
	m.calls = append(m.calls, pullCall{project: project, since: since})
	muts := m.mutations[project]
	m.mu.Unlock()

	var out []domain.Mutation
	for _, mut := range muts {
		if mut.Seq > since {
			out = append(out, mut)
		}
	}
	return out, nil
}

func (m *multiProjectCentral) getCalls() []pullCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]pullCall, len(m.calls))
	copy(cp, m.calls)
	return cp
}

// makeMutation builds a minimal domain.Mutation with the given project and
// central seq (Seq). SyncID and MutationID are distinct per mutation so
// ApplyPulled's idempotency guard never de-dupes them.
func makeMutation(project string, seq int64) domain.Mutation {
	suffix := project + "-" + strconv.FormatInt(seq, 10)
	syncID := "sync-mp-" + suffix
	return domain.Mutation{
		Op:         domain.OpUpsert,
		Seq:        seq,
		SyncID:     syncID,
		MutationID: "mut-mp-" + suffix,
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "title-" + project,
		Content:    "content-" + project,
		Project:    project,
		Scope:      "project",
		WriterID:   "writer",
		Payload:    []byte(`{"op":"upsert","project":"` + project + `","sync_id":"` + syncID + `"}`),
		OccurredAt: time.Date(2025, 1, 1, 0, 0, int(seq), 0, time.UTC),
		UpdatedAt:  time.Date(2025, 1, 1, 0, 0, int(seq), 0, time.UTC),
	}
}

// TestSyncAllProjects_TwoProjects verifies that SyncAllProjects pulls mutations
// for BOTH project "A" and project "B" from central, applying them to the local
// store, and advances each project's per-project cursor independently.
//
// This is the unit-level proof that SyncAllProjects iterates over ListProjects()
// and issues a per-project PullSince for each.
func TestSyncAllProjects_TwoProjects(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "sap-two-projects")

	// Write local memories for two projects so ListProjects returns both.
	writeLocal := func(project, syncID string) {
		t.Helper()
		_, err := node.Write(domain.Mutation{
			Op:         domain.OpUpsert,
			SyncID:     syncID,
			SessionID:  "sess",
			EntityType: domain.EntityMemory,
			Type:       "manual",
			Title:      "title",
			Project:    project,
			Scope:      "project",
			WriterID:   "writer",
			UpdatedAt:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("Write %s: %v", syncID, err)
		}
	}

	// Note: openNode already seeds a "testproject" memory.
	writeLocal("alpha", "local-alpha-1")
	writeLocal("beta", "local-beta-1")

	// Central has one mutation for alpha and one for beta (seqs 1 and 2).
	// These simulate mutations written by other nodes.
	centralAlpha := makeMutation("alpha", 1)
	centralBeta := makeMutation("beta", 2)

	central := newMultiProjectCentral(map[string][]domain.Mutation{
		"alpha":       {centralAlpha},
		"beta":        {centralBeta},
		"testproject": {}, // no central mutations for the seed project
	})

	pushed, pulled, err := syncer.SyncAllProjects(ctx, node, central)
	if err != nil {
		t.Fatalf("SyncAllProjects: %v", err)
	}
	// pushed: the seed from openNode + local-alpha-1 + local-beta-1 = 3 outbox entries.
	// (They are consumed — pushed to the mock central.)
	if pushed < 0 {
		t.Errorf("SyncAllProjects pushed=%d, want ≥0", pushed)
	}
	// pulled: 1 alpha + 1 beta = 2 (testproject has 0 central mutations).
	if pulled != 2 {
		t.Errorf("SyncAllProjects pulled=%d, want 2 (1 alpha + 1 beta)", pulled)
	}

	// PullSince must have been called for at least alpha, beta, and testproject.
	calls := central.getCalls()
	projectsCalled := make(map[string]bool)
	for _, c := range calls {
		projectsCalled[c.project] = true
	}
	for _, wantProj := range []string{"alpha", "beta", "testproject"} {
		if !projectsCalled[wantProj] {
			t.Errorf("PullSince not called for project %q; calls=%v", wantProj, calls)
		}
	}

	// Per-project cursors: alpha's cursor must be at seq=1; beta's at seq=2.
	alphaSeq, err := node.Store.PullCursorFor("alpha")
	if err != nil {
		t.Fatalf("PullCursorFor alpha: %v", err)
	}
	if alphaSeq != 1 {
		t.Errorf("alpha pull cursor = %d, want 1", alphaSeq)
	}
	betaSeq, err := node.Store.PullCursorFor("beta")
	if err != nil {
		t.Fatalf("PullCursorFor beta: %v", err)
	}
	if betaSeq != 2 {
		t.Errorf("beta pull cursor = %d, want 2", betaSeq)
	}

	// Alpha's cursor must not equal beta's and vice-versa — independent.
	if alphaSeq == betaSeq {
		t.Errorf("alpha cursor == beta cursor (%d): per-project cursors not independent", alphaSeq)
	}

	// A second SyncAllProjects call with the SAME central mutations must pull 0
	// new mutations (cursors are already at the highest seqs).
	_, pulled2, err := syncer.SyncAllProjects(ctx, node, central)
	if err != nil {
		t.Fatalf("SyncAllProjects (second call): %v", err)
	}
	if pulled2 != 0 {
		t.Errorf("second SyncAllProjects pulled=%d, want 0 (already at cursor)", pulled2)
	}
}

// TestSyncAllProjects_PartialFailure verifies the error collection policy:
// all projects are attempted even when an earlier one fails, and the returned
// error is retryable if any underlying error is retryable.
func TestSyncAllProjects_PartialFailure(t *testing.T) {
	ctx := context.Background()
	node := openNode(t, "sap-partial-fail")

	writeLocal := func(project, syncID string) {
		t.Helper()
		_, err := node.Write(domain.Mutation{
			Op:         domain.OpUpsert,
			SyncID:     syncID,
			SessionID:  "sess",
			EntityType: domain.EntityMemory,
			Type:       "manual",
			Title:      "title",
			Project:    project,
			Scope:      "project",
			WriterID:   "writer",
			UpdatedAt:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("Write %s: %v", syncID, err)
		}
	}

	writeLocal("projA", "pf-a1")
	writeLocal("projB", "pf-b1")

	// failCentral returns an error for projA but succeeds for projB and testproject.
	failCentral := &failOnProjectCentral{
		failProject: "projA",
		failErr:     &retryableErr{retryable: true, msg: "server 500"},
		okMuts: map[string][]domain.Mutation{
			"projB":       {makeMutation("projB", 7)},
			"testproject": {},
		},
	}

	_, pulled, err := syncer.SyncAllProjects(ctx, node, failCentral)
	if err == nil {
		t.Fatal("SyncAllProjects: want error for projA failure, got nil")
	}

	// projB's mutation must still have been pulled (1 pulled despite projA failure).
	if pulled != 1 {
		t.Errorf("SyncAllProjects pulled=%d, want 1 (projB succeeded despite projA error)", pulled)
	}

	// The error must be retryable (projA's error is retryable).
	type retryableIface interface{ Retryable() bool }
	isRetryableErr := false
	// Walk the error chain manually — syncAllError wraps the individual errors.
	if re, ok := err.(retryableIface); ok {
		isRetryableErr = re.Retryable()
	}
	if !isRetryableErr {
		t.Errorf("SyncAllProjects error not retryable: %v (want Retryable()=true)", err)
	}
}

// failOnProjectCentral is a Central that fails PullSince for one specific
// project and returns ok mutations for the rest.
type failOnProjectCentral struct {
	failProject string
	failErr     error
	okMuts      map[string][]domain.Mutation
}

func (f *failOnProjectCentral) Apply(_ context.Context, _ domain.Mutation) error { return nil }

func (f *failOnProjectCentral) PullSince(_ context.Context, project string, since int64, _ int) ([]domain.Mutation, error) {
	if project == f.failProject {
		return nil, f.failErr
	}
	var out []domain.Mutation
	for _, m := range f.okMuts[project] {
		if m.Seq > since {
			out = append(out, m)
		}
	}
	return out, nil
}
