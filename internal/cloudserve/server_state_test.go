package cloudserve_test

// Tests for Server.handleState (POST /v1/state).
// Mirrors the /v1/projects handler test patterns in server_projects_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mariesqu/engram/internal/cloudserve"
	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/syncwire"
)

// ── mock Central WITH writerPurgeEpoch capability ─────────────────────────────

// purgeEpochCentral is a test Central that implements BOTH the core transport
// interface AND the optional writerPurgeEpoch capability (structurally — the
// interface itself is unexported in package cloudserve, so this satisfies it
// implicitly via the WriterPurgeEpoch method signature).
type purgeEpochCentral struct {
	*mockCentral

	// epochByWriter maps writer_id → the epoch WriterPurgeEpoch should return.
	epochByWriter map[string]int64
	// epochErr, if non-nil, is returned by WriterPurgeEpoch for every writer.
	epochErr error
	// gotWriterID captures the writer_id WriterPurgeEpoch was called with.
	gotWriterID string
}

func (p *purgeEpochCentral) Apply(ctx context.Context, m domain.Mutation) error {
	return p.mockCentral.Apply(ctx, m)
}

func (p *purgeEpochCentral) PullSince(ctx context.Context, project string, sinceSeq int64, limit int) ([]domain.Mutation, error) {
	return p.mockCentral.PullSince(ctx, project, sinceSeq, limit)
}

func (p *purgeEpochCentral) WriterPurgeEpoch(_ context.Context, writerID string) (int64, error) {
	p.gotWriterID = writerID
	if p.epochErr != nil {
		return 0, p.epochErr
	}
	return p.epochByWriter[writerID], nil
}

// newStateServer constructs an httptest.Server whose Central satisfies the
// optional writerPurgeEpoch capability, using AllowAllVerifier (so authWriterID
// is always "" unless explicitly overridden by a custom verifier in a test).
func newStateServer(t *testing.T, central *purgeEpochCentral) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(cloudserve.New(central, cloudserve.AllowAllVerifier()).Handler())
	t.Cleanup(ts.Close)
	return ts
}

// stubVerifier is a minimal cloudserve.Verifier that always authenticates as a
// fixed writer_id, regardless of headers — used to exercise handleState's
// "authenticated writer" path without a real HMAC key exchange.
type stubVerifier struct {
	writerID string
}

func (v stubVerifier) Verify(_ context.Context, _, _, _ string, _ []byte, _ string) (string, error) {
	return v.writerID, nil
}

// newStateServerWithWriter constructs an httptest.Server that authenticates
// every request as writerID (via stubVerifier), backed by central.
func newStateServerWithWriter(t *testing.T, central *purgeEpochCentral, writerID string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(cloudserve.New(central, stubVerifier{writerID: writerID}).Handler())
	t.Cleanup(ts.Close)
	return ts
}

// ── TestHandleState_ReturnsAuthenticatedWritersEpoch ──────────────────────────

// TestHandleState_ReturnsAuthenticatedWritersEpoch verifies that /v1/state
// returns the epoch for the AUTHENTICATED writer — not any writer_id the
// client might try to smuggle in (the request carries no identity field at
// all, so there is nothing to forge, but this proves the epoch returned
// matches the identity the auth layer established).
func TestHandleState_ReturnsAuthenticatedWritersEpoch(t *testing.T) {
	central := &purgeEpochCentral{
		mockCentral: &mockCentral{},
		epochByWriter: map[string]int64{
			"writer-a": 3,
			"writer-b": 7,
		},
	}
	ts := newStateServerWithWriter(t, central, "writer-b")

	body, _ := json.Marshal(syncwire.StateRequest{})
	resp, err := http.Post(ts.URL+"/v1/state", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/state: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got syncwire.StateResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode StateResponse: %v", err)
	}
	if got.PurgeEpoch != 7 {
		t.Errorf("PurgeEpoch = %d; want 7 (writer-b's epoch, the authenticated writer)", got.PurgeEpoch)
	}
	if central.gotWriterID != "writer-b" {
		t.Errorf("WriterPurgeEpoch called with %q; want %q (the authenticated writer, not writer-a)", central.gotWriterID, "writer-b")
	}
}

// ── TestHandleState_AllowAllVerifier_NoAuthenticatedWriter_400 ────────────────

// TestHandleState_AllowAllVerifier_NoAuthenticatedWriter_400 verifies that when
// no auth is established (AllowAllVerifier, which stashes "" as the
// authenticated writer_id), /v1/state returns 400 — there is no identity to
// report an epoch for.
func TestHandleState_AllowAllVerifier_NoAuthenticatedWriter_400(t *testing.T) {
	central := &purgeEpochCentral{
		mockCentral:   &mockCentral{},
		epochByWriter: map[string]int64{},
	}
	ts := newStateServer(t, central) // AllowAllVerifier

	body, _ := json.Marshal(syncwire.StateRequest{})
	resp, err := http.Post(ts.URL+"/v1/state", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/state: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (no authenticated writer identity)", resp.StatusCode)
	}
}

// ── TestHandleState_MissingAuthHeaders_Unauthorized ───────────────────────────

// TestHandleState_MissingAuthHeaders_Unauthorized proves that /v1/state sits
// behind the SAME HMAC auth middleware as push/pull: an unsigned request
// against a REAL (non-AllowAll, non-stub) verifier is rejected with 401 before
// ever reaching handleState. This uses a verifier that always fails, mirroring
// server_auth_test.go's "wrong signature" pattern without needing a real store.
func TestHandleState_MissingAuthHeaders_Unauthorized(t *testing.T) {
	central := &purgeEpochCentral{mockCentral: &mockCentral{}}
	ts := httptest.NewServer(cloudserve.New(central, alwaysFailVerifier{}).Handler())
	t.Cleanup(ts.Close)

	body, _ := json.Marshal(syncwire.StateRequest{})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/state", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Deliberately omit X-Writer-Id and X-Signature.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/state: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (auth required on /v1/state)", resp.StatusCode)
	}
}

// alwaysFailVerifier is a cloudserve.Verifier that always rejects — used to
// prove /v1/state is behind the same auth gate as the other authenticated
// routes, without depending on a real HMAC key/store.
type alwaysFailVerifier struct{}

func (alwaysFailVerifier) Verify(_ context.Context, _, _, _ string, _ []byte, _ string) (string, error) {
	return "", errors.New("denied")
}

// ── TestHandleState_501_WhenCentralLacksCapability ────────────────────────────

// TestHandleState_501_WhenCentralLacksCapability verifies that when the
// wrapped Central does NOT implement the optional writerPurgeEpoch capability,
// handleState returns 501 Not Implemented — mirroring handleProjects'
// capability-gating behavior.
func TestHandleState_501_WhenCentralLacksCapability(t *testing.T) {
	// Plain mockCentral does NOT implement WriterPurgeEpoch.
	central := &mockCentral{}
	ts := newTestServer(t, central) // reuses the helper from server_test.go

	body, _ := json.Marshal(syncwire.StateRequest{})
	resp, err := http.Post(ts.URL+"/v1/state", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/state: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 (Central lacks writerPurgeEpoch)", resp.StatusCode)
	}

	var errBody struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody.Error == "" {
		t.Error("error field is empty, want a non-empty message")
	}
}

// ── TestHandleState_500_WhenLookupErrors ──────────────────────────────────────

// TestHandleState_500_WhenLookupErrors verifies that a WriterPurgeEpoch lookup
// error surfaces as 500.
func TestHandleState_500_WhenLookupErrors(t *testing.T) {
	central := &purgeEpochCentral{
		mockCentral: &mockCentral{},
		epochErr:    errors.New("DB down"),
	}
	ts := newStateServerWithWriter(t, central, "writer-err")

	body, _ := json.Marshal(syncwire.StateRequest{})
	resp, err := http.Post(ts.URL+"/v1/state", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/state: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// ── TestHandleState_405_WrongMethod ────────────────────────────────────────────

// TestHandleState_405_WrongMethod verifies that GET on /v1/state returns 405.
func TestHandleState_405_WrongMethod(t *testing.T) {
	central := &purgeEpochCentral{mockCentral: &mockCentral{}}
	ts := newStateServerWithWriter(t, central, "writer-405")

	resp, err := http.Get(ts.URL + "/v1/state")
	if err != nil {
		t.Fatalf("GET /v1/state: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/state: status = %d, want 405", resp.StatusCode)
	}
}

