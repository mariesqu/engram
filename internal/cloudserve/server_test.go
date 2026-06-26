package cloudserve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/cloudserve"
	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/mutation"
	"github.com/mariesqu/engram/internal/syncwire"
)

// ── mock transport.Central ────────────────────────────────────────────────────

type mockCentral struct {
	applyErr   error
	pullResult []domain.Mutation
	pullErr    error

	// Captured arguments from the last PullSince call (for clamp assertions).
	gotProject  string
	gotSinceSeq int64
	gotLimit    int

	// unshare (DeleteProject) — makes mockCentral satisfy the projectDeleter
	// capability so /v1/unshare can be exercised.
	deleteResult     int64
	deleteErr        error
	gotDeleteProject string
}

func (m *mockCentral) Apply(_ context.Context, _ domain.Mutation) error {
	return m.applyErr
}

func (m *mockCentral) DeleteProject(_ context.Context, project string) (int64, error) {
	m.gotDeleteProject = project
	return m.deleteResult, m.deleteErr
}

func (m *mockCentral) PullSince(_ context.Context, project string, sinceSeq int64, limit int) ([]domain.Mutation, error) {
	m.gotProject = project
	m.gotSinceSeq = sinceSeq
	m.gotLimit = limit
	return m.pullResult, m.pullErr
}

// ── helpers ───────────────────────────────────────────────────────────────────

// newTestServer returns an httptest.Server backed by a mock central.
func newTestServer(t *testing.T, central *mockCentral) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(cloudserve.New(central, cloudserve.AllowAllVerifier()).Handler())
	t.Cleanup(ts.Close)
	return ts
}

// validPushBody constructs a valid PushRequest body for a simple upsert mutation.
// The returned bytes are ready to POST.
func validPushBody(t *testing.T) ([]byte, string) {
	t.Helper()
	m := domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "sync-unit-1",
		SessionID:  "sess-unit-1",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Unit test memory",
		Content:    "unit content",
		Project:    "test-project",
		Scope:      "project",
		Version:    1,
		WriterID:   "writer-unit",
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
	}
	payload := mutation.CanonicalPayload(m)
	m.Payload = payload
	m.MutationID = mutation.NewMutationID(payload)

	wire := syncwire.ToWire(m)
	req := syncwire.PushRequest{Mutation: wire}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("validPushBody: marshal: %v", err)
	}
	return b, m.MutationID
}

// ── push 200 ─────────────────────────────────────────────────────────────────

func TestHandlePush_Success(t *testing.T) {
	central := &mockCentral{applyErr: nil}
	ts := newTestServer(t, central)

	body, mutID := validPushBody(t)
	resp, err := http.Post(ts.URL+"/v1/push", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/push: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var got syncwire.PushResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Status != "ok" {
		t.Errorf("status = %q, want %q", got.Status, "ok")
	}
	if got.MutationID != mutID {
		t.Errorf("mutation_id = %q, want %q", got.MutationID, mutID)
	}
	if !got.Applied {
		t.Error("applied = false, want true")
	}
}

// ── push 500 — Apply returns error ───────────────────────────────────────────

func TestHandlePush_ApplyError_Returns500(t *testing.T) {
	central := &mockCentral{applyErr: errors.New("DB down")}
	ts := newTestServer(t, central)

	body, _ := validPushBody(t)
	resp, err := http.Post(ts.URL+"/v1/push", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/push: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// ── push 400 — malformed body ─────────────────────────────────────────────────

func TestHandlePush_MalformedBody_Returns400(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)

	resp, err := http.Post(ts.URL+"/v1/push", "application/json", bytes.NewReader([]byte("not json")))
	if err != nil {
		t.Fatalf("POST /v1/push: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// ── push 400 — empty body ─────────────────────────────────────────────────────

func TestHandlePush_EmptyBody_Returns400(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)

	resp, err := http.Post(ts.URL+"/v1/push", "application/json", bytes.NewReader([]byte{}))
	if err != nil {
		t.Fatalf("POST /v1/push: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// ── push 400 — VerifyMutationID fails (tampered payload) ─────────────────────

func TestHandlePush_TamperedMutationID_Returns400(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)

	body, _ := validPushBody(t)

	// Unmarshal, tamper the mutation_id, re-marshal.
	var req syncwire.PushRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	req.Mutation.MutationID = "tampered-mutation-id-000000000000000000000"
	tampered, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}

	resp, err := http.Post(ts.URL+"/v1/push", "application/json", bytes.NewReader(tampered))
	if err != nil {
		t.Fatalf("POST /v1/push: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for tampered mutation_id", resp.StatusCode)
	}
}

// ── push 400 — FromWire fails (non-UTC occurred_at) ──────────────────────────

func TestHandlePush_NonUTCOccurredAt_Returns400(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)

	body, _ := validPushBody(t)

	// Unmarshal, set a non-UTC occurred_at (explicit offset), re-marshal.
	var req syncwire.PushRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	req.Mutation.OccurredAt = "2024-01-15T10:00:00+05:00" // non-UTC — must be rejected

	// OccurredAt is a sibling field OUTSIDE the canonical payload, so changing it
	// leaves the payload bytes (and thus mutation_id) untouched — VerifyMutationID
	// still passes, so the request reaches FromWire, which rejects the non-UTC
	// timestamp (→ 400). No mutation_id recompute or tamper needed.
	tampered, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}

	resp, err := http.Post(ts.URL+"/v1/push", "application/json", bytes.NewReader(tampered))
	if err != nil {
		t.Fatalf("POST /v1/push: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for non-UTC occurred_at", resp.StatusCode)
	}
}

// ── pull 200 — success ────────────────────────────────────────────────────────

func TestHandlePull_Success(t *testing.T) {
	m := domain.Mutation{
		MutationID: mutation.NewMutationID([]byte(`{}`)),
		Op:         domain.OpUpsert,
		SyncID:     "sync-pull-1",
		SessionID:  "sess-pull-1",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Pull test",
		Content:    "pull content",
		Project:    "test-project",
		Scope:      "project",
		Version:    1,
		WriterID:   "writer-pull",
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
		Seq:        42,
		Payload:    mutation.CanonicalPayload(domain.Mutation{}),
	}
	central := &mockCentral{pullResult: []domain.Mutation{m}}
	ts := newTestServer(t, central)

	req := syncwire.PullRequest{Project: "test-project", SinceSeq: 0}
	body, _ := json.Marshal(req)

	resp, err := http.Post(ts.URL+"/v1/pull", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/pull: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var got syncwire.PullResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Mutations) != 1 {
		t.Fatalf("len(mutations) = %d, want 1", len(got.Mutations))
	}
	if got.Mutations[0].Seq != 42 {
		t.Errorf("mutations[0].seq = %d, want 42", got.Mutations[0].Seq)
	}
}

// ── pull 400 — empty project ──────────────────────────────────────────────────

func TestHandlePull_EmptyProject_Returns400(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)

	req := syncwire.PullRequest{Project: "", SinceSeq: 0}
	body, _ := json.Marshal(req)

	resp, err := http.Post(ts.URL+"/v1/pull", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/pull: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for empty project", resp.StatusCode)
	}
}

// ── pull 400 — malformed body ─────────────────────────────────────────────────

func TestHandlePull_MalformedBody_Returns400(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)

	resp, err := http.Post(ts.URL+"/v1/pull", "application/json", bytes.NewReader([]byte("bad json")))
	if err != nil {
		t.Fatalf("POST /v1/pull: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// ── pull 500 — PullSince returns error ───────────────────────────────────────

func TestHandlePull_PullSinceError_Returns500(t *testing.T) {
	central := &mockCentral{pullErr: errors.New("DB timeout")}
	ts := newTestServer(t, central)

	req := syncwire.PullRequest{Project: "test-project", SinceSeq: 0}
	body, _ := json.Marshal(req)

	resp, err := http.Post(ts.URL+"/v1/pull", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/pull: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// ── method guard — 405 ────────────────────────────────────────────────────────

func TestMethodGuard_WrongMethod_Returns405(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)

	for _, path := range []string{"/v1/push", "/v1/pull"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: status = %d, want 405", path, resp.StatusCode)
		}
	}
}

// ── unknown path — 404 ────────────────────────────────────────────────────────

func TestUnknownPath_Returns404(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)

	resp, err := http.Get(ts.URL + "/does-not-exist")
	if err != nil {
		t.Fatalf("GET /does-not-exist: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode 404 JSON body: %v", err)
	}
	if body.Error == "" {
		t.Error("404 body: error field is empty, want a message")
	}
}

// ── error body shape ──────────────────────────────────────────────────────────

// TestErrorBody_Shape verifies that error responses carry a JSON {"error":"..."}
// body, not an empty body or a plain text string.
func TestErrorBody_Shape(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)

	resp, err := http.Post(ts.URL+"/v1/push", "application/json", bytes.NewReader([]byte("bad")))
	if err != nil {
		t.Fatalf("POST /v1/push: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if _, ok := body["error"]; !ok {
		t.Errorf("error body has no 'error' key: %v", body)
	}
}
