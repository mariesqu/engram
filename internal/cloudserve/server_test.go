package cloudserve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/cloudserve"
	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/mutation"
	"github.com/mariesqu/engram/internal/syncwire"
	"github.com/mariesqu/engram/internal/transport"
)

// ── mock transport.Central ────────────────────────────────────────────────────

type mockCentral struct {
	applyErr   error
	applyCalls int
	pullResult []domain.Mutation
	pullErr    error

	// pullCalls records every (sinceSeq, limit) pair, in order. handlePull fetches
	// in chunks, so "what did the server ask for" is a SEQUENCE, not one value —
	// which is why the old single-call gotProject/gotSinceSeq/gotLimit captures are
	// gone rather than kept alongside this.
	pullCalls []mockPullCall

	// unshare (DeleteProject) — makes mockCentral satisfy the projectDeleter
	// capability so /v1/unshare can be exercised.
	deleteResult     int64
	deleteErr        error
	gotDeleteProject string
}

func (m *mockCentral) Apply(_ context.Context, _ domain.Mutation) error {
	m.applyCalls++
	return m.applyErr
}

func (m *mockCentral) DeleteProject(_ context.Context, project string) (int64, error) {
	m.gotDeleteProject = project
	return m.deleteResult, m.deleteErr
}

// mockPullCall is one recorded PullSince invocation.
type mockPullCall struct {
	sinceSeq int64
	limit    int
}

// PullSince serves pullResult the way a real Central must: only rows with
// seq > sinceSeq, in stored (seq-ascending) order, at most limit of them.
//
// Honoring sinceSeq is REQUIRED, not cosmetic. handlePull now fetches in chunks and
// advances a cursor between them, so a mock that returned its whole fixture on
// every call would hand back the same chunk forever and duplicate rows into the
// response — only the iteration backstop would end the request. Before chunking,
// this mock ignored both arguments; it was changed deliberately as part of that
// work.
func (m *mockCentral) PullSince(_ context.Context, _ string, sinceSeq int64, limit int) ([]domain.Mutation, error) {
	m.pullCalls = append(m.pullCalls, mockPullCall{sinceSeq: sinceSeq, limit: limit})
	if m.pullErr != nil {
		return nil, m.pullErr
	}

	out := make([]domain.Mutation, 0, min(limit, len(m.pullResult)))
	for _, mut := range m.pullResult {
		if mut.Seq <= sinceSeq {
			continue
		}
		if len(out) >= limit {
			break
		}
		out = append(out, mut)
	}
	return out, nil
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

// ── push 422 — Apply returns a permanent (transport.ErrPermanent) error ──────

// TestHandlePush_PermanentApplyError_Returns422 is the FUP-004 regression
// test: a mutation Apply rejects for a DETERMINISTIC reason (wrapped in
// transport.ErrPermanent — mirroring what centralstore.Apply actually returns
// for a Postgres data exception / constraint violation) must come back as 422,
// not 500, so the client parks the entry instead of retrying it forever. The
// response body must carry the safe detail text, not a generic "internal
// error" — the client reads it into last_error for mem_doctor/CLI visibility.
func TestHandlePush_PermanentApplyError_Returns422(t *testing.T) {
	central := &mockCentral{applyErr: fmt.Errorf("Apply: insert mutation: %w: rejected by constraint %q (SQLSTATE 23514)",
		transport.ErrPermanent, "memories_entity_type_check")}
	ts := newTestServer(t, central)

	body, _ := validPushBody(t)
	resp, err := http.Post(ts.URL+"/v1/push", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/push: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
	var got struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(got.Error, "memories_entity_type_check") {
		t.Errorf("error body = %q, want it to name the constraint", got.Error)
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

func TestHandlePush_NULPayload_ReturnsActionable400BeforeApply(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)
	m := domain.Mutation{
		Op: domain.OpUpsert, SyncID: "sync-nul", EntityType: domain.EntityMemory,
		Content: "bad\x00content", Project: "project", Scope: "project",
		Version: 1, WriterID: "writer", UpdatedAt: time.Now().UTC(), OccurredAt: time.Now().UTC(),
	}
	m.Payload = mutation.CanonicalPayload(m)
	m.MutationID = mutation.NewMutationID(m.Payload)
	body, err := json.Marshal(syncwire.PushRequest{Mutation: syncwire.ToWire(m)})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	resp, err := http.Post(ts.URL+"/v1/push", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/push: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if !strings.Contains(got["error"], "U+0000") || !strings.Contains(got["error"], "content") {
		t.Fatalf("error = %q, want actionable content/U+0000 message", got["error"])
	}
	if central.applyCalls != 0 {
		t.Fatalf("central Apply called %d times, want 0", central.applyCalls)
	}
}

func TestHandlePush_NULAnywhereInCanonicalJSON_Returns400BeforeApply(t *testing.T) {
	base := domain.Mutation{
		Op: domain.OpUpsert, SyncID: "sync-nul-hidden", EntityType: domain.EntityMemory,
		Content: "valid content", Project: "project", Scope: "project",
		Version: 1, WriterID: "writer", UpdatedAt: time.Now().UTC(), OccurredAt: time.Now().UTC(),
	}
	basePayload := mutation.CanonicalPayload(base)
	tests := []struct {
		name    string
		payload []byte
	}{
		{"unknown field value", appendPushJSONField(t, basePayload, `"unknown":"bad\u0000value"`)},
		{"object key", appendPushJSONField(t, basePayload, `"bad\u0000key":"value"`)},
		{"shadowed earlier duplicate field", prependPushJSONField(t, basePayload, `"content":"bad\u0000shadow"`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			central := &mockCentral{}
			ts := newTestServer(t, central)
			request := syncwire.PushRequest{Mutation: syncwire.WireMutation{
				MutationID: mutation.NewMutationID(tt.payload),
				OccurredAt: base.OccurredAt.UTC().Format(time.RFC3339Nano),
				Payload:    tt.payload,
			}}
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}

			resp, err := http.Post(ts.URL+"/v1/push", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST /v1/push: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var got map[string]string
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if !strings.Contains(got["error"], "U+0000") {
				t.Fatalf("error = %q, want actionable U+0000 message", got["error"])
			}
			if central.applyCalls != 0 {
				t.Fatalf("central Apply called %d times, want 0", central.applyCalls)
			}
		})
	}
}

func appendPushJSONField(t *testing.T, payload []byte, field string) []byte {
	t.Helper()
	if len(payload) == 0 || payload[len(payload)-1] != '}' {
		t.Fatalf("test payload is not a JSON object: %q", payload)
	}
	result := append([]byte(nil), payload[:len(payload)-1]...)
	result = append(result, ',')
	result = append(result, field...)
	return append(result, '}')
}

func prependPushJSONField(t *testing.T, payload []byte, field string) []byte {
	t.Helper()
	if len(payload) == 0 || payload[0] != '{' {
		t.Fatalf("test payload is not a JSON object: %q", payload)
	}
	result := []byte{'{'}
	result = append(result, field...)
	result = append(result, ',')
	return append(result, payload[1:]...)
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
