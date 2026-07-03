package main

// Tests for checkRemotePurgeOnStartup — the daemon-side remote-purge decision
// table (fail-open on error, no-op on unsupported, purge triggered exactly
// when remote > honored, idempotent on a second start).

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/remote"
)

// fakeStateFetcher is a stateFetcher test double whose State() call is fully
// scripted: it returns a fixed WriterState or a fixed error.
type fakeStateFetcher struct {
	calls int
	state remote.WriterState
	err   error
}

func (f *fakeStateFetcher) State(_ context.Context) (remote.WriterState, error) {
	f.calls++
	return f.state, f.err
}

// fakeStatusErr mirrors *remote.StatusError's StatusCode() method so
// isRemoteStateUnsupported can classify it without depending on the concrete
// remote package error type.
type fakeStatusErr struct {
	code int
}

func (e *fakeStatusErr) Error() string   { return "fake status error" }
func (e *fakeStatusErr) StatusCode() int { return e.code }

// openPurgeTestStore opens a fresh localstore.Store in a temp directory.
func openPurgeTestStore(t *testing.T) *localstore.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "purge-test.db")
	st, err := localstore.Open(path)
	if err != nil {
		t.Fatalf("localstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestCheckRemotePurgeOnStartup_NilCentral_NoOp verifies local-only mode
// (central == nil) never calls State or touches the store.
func TestCheckRemotePurgeOnStartup_NilCentral_NoOp(t *testing.T) {
	st := openPurgeTestStore(t)

	// Passing a nil *fakeStateFetcher via the interface would NOT be nil at the
	// interface level (a classic Go gotcha) — the production call sites pass
	// components.central directly, which is a genuinely nil remoteCentral
	// interface value when centralURL == "". Reproduce that here.
	var central stateFetcher
	checkRemotePurgeOnStartup(context.Background(), st, central)

	honored, err := st.HonoredPurgeEpoch()
	if err != nil {
		t.Fatalf("HonoredPurgeEpoch: %v", err)
	}
	if honored != 0 {
		t.Errorf("HonoredPurgeEpoch = %d; want 0 (nil central must be a pure no-op)", honored)
	}
}

// TestCheckRemotePurgeOnStartup_FailOpen_OnNetworkError verifies that a
// generic (non-status) error from State() — network failure, timeout, 5xx —
// is fail-open: the check is skipped, startup is not blocked, and the honored
// epoch is left unchanged.
func TestCheckRemotePurgeOnStartup_FailOpen_OnNetworkError(t *testing.T) {
	st := openPurgeTestStore(t)
	fake := &fakeStateFetcher{err: errors.New("dial tcp: connection refused")}

	checkRemotePurgeOnStartup(context.Background(), st, fake)

	if fake.calls != 1 {
		t.Errorf("State() called %d times; want 1", fake.calls)
	}
	honored, err := st.HonoredPurgeEpoch()
	if err != nil {
		t.Fatalf("HonoredPurgeEpoch: %v", err)
	}
	if honored != 0 {
		t.Errorf("HonoredPurgeEpoch = %d; want 0 (fail-open must not touch the store)", honored)
	}
}

// TestCheckRemotePurgeOnStartup_NoOp_On404And501 verifies that a
// capability-absent signal (404 from an older central's catch-all, or 501 from
// a Central lacking the writerPurgeEpoch capability) is treated as a no-op,
// not a failure — mirroring syncer.isDiscoveryUnsupported's classification.
func TestCheckRemotePurgeOnStartup_NoOp_On404And501(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusNotImplemented} {
		st := openPurgeTestStore(t)
		fake := &fakeStateFetcher{err: &fakeStatusErr{code: code}}

		checkRemotePurgeOnStartup(context.Background(), st, fake)

		honored, err := st.HonoredPurgeEpoch()
		if err != nil {
			t.Fatalf("HonoredPurgeEpoch (code=%d): %v", code, err)
		}
		if honored != 0 {
			t.Errorf("code=%d: HonoredPurgeEpoch = %d; want 0 (unsupported must be a no-op)", code, honored)
		}
	}
}

// TestCheckRemotePurgeOnStartup_PurgeTriggered_WhenRemoteAheadOfHonored
// verifies the core trigger condition: remote_epoch > honored_epoch runs
// PurgeForResync and records the new honored epoch.
func TestCheckRemotePurgeOnStartup_PurgeTriggered_WhenRemoteAheadOfHonored(t *testing.T) {
	st := openPurgeTestStore(t)

	// Seed a synced project with one memory so the purge has something to do.
	if err := st.SetPolicy("purge-trigger-proj", localstore.PolicySynced); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		 VALUES ('sync-trigger-1', 'sess', 'memory', 'manual', 'seed', 'seed', 'purge-trigger-proj', 'project', 'w')`,
	); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	fake := &fakeStateFetcher{state: remote.WriterState{PurgeEpoch: 5}}

	checkRemotePurgeOnStartup(context.Background(), st, fake)

	honored, err := st.HonoredPurgeEpoch()
	if err != nil {
		t.Fatalf("HonoredPurgeEpoch: %v", err)
	}
	if honored != 5 {
		t.Errorf("HonoredPurgeEpoch = %d; want 5 (purge must have run and recorded the new epoch)", honored)
	}

	var n int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE project = 'purge-trigger-proj'`,
	).Scan(&n); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if n != 0 {
		t.Errorf("memories remaining for purge-trigger-proj = %d; want 0 (purge must have run)", n)
	}
}

// TestCheckRemotePurgeOnStartup_NoOp_WhenRemoteEqualsHonored verifies
// remote_epoch == honored_epoch does nothing (the steady state, including the
// initial 0 == 0 case).
func TestCheckRemotePurgeOnStartup_NoOp_WhenRemoteEqualsHonored(t *testing.T) {
	st := openPurgeTestStore(t)

	if err := st.SetPolicy("purge-steady-proj", localstore.PolicySynced); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		 VALUES ('sync-steady-1', 'sess', 'memory', 'manual', 'seed', 'seed', 'purge-steady-proj', 'project', 'w')`,
	); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	fake := &fakeStateFetcher{state: remote.WriterState{PurgeEpoch: 0}}
	checkRemotePurgeOnStartup(context.Background(), st, fake)

	var n int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE project = 'purge-steady-proj'`,
	).Scan(&n); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if n != 1 {
		t.Errorf("memories remaining for purge-steady-proj = %d; want 1 (0 == 0 must be a no-op)", n)
	}
}

// TestCheckRemotePurgeOnStartup_Idempotent_OnSecondStart verifies that once a
// purge has run and the honored epoch has been recorded, a SECOND call with
// the SAME remote epoch does not re-purge (idempotent across daemon restarts).
func TestCheckRemotePurgeOnStartup_Idempotent_OnSecondStart(t *testing.T) {
	st := openPurgeTestStore(t)

	if err := st.SetPolicy("purge-idempotent-proj", localstore.PolicySynced); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	fake := &fakeStateFetcher{state: remote.WriterState{PurgeEpoch: 2}}

	// First "start": purge triggered.
	checkRemotePurgeOnStartup(context.Background(), st, fake)
	honored, err := st.HonoredPurgeEpoch()
	if err != nil {
		t.Fatalf("HonoredPurgeEpoch after 1st check: %v", err)
	}
	if honored != 2 {
		t.Fatalf("HonoredPurgeEpoch after 1st check = %d; want 2", honored)
	}

	// Re-seed data as if the autosync loop re-pulled it from central.
	if _, err := st.DB().Exec(
		`INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		 VALUES ('sync-idempotent-1', 'sess', 'memory', 'manual', 'repulled', 'repulled', 'purge-idempotent-proj', 'project', 'w')`,
	); err != nil {
		t.Fatalf("seed memory after purge: %v", err)
	}

	// Second "start": same remote epoch (2) — must NOT purge again.
	checkRemotePurgeOnStartup(context.Background(), st, fake)

	var n int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE project = 'purge-idempotent-proj'`,
	).Scan(&n); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if n != 1 {
		t.Errorf("memories remaining after 2nd check = %d; want 1 (re-pulled data must survive a no-op 2nd check)", n)
	}
	if fake.calls != 2 {
		t.Errorf("State() called %d times; want 2 (once per checkRemotePurgeOnStartup call)", fake.calls)
	}
}
