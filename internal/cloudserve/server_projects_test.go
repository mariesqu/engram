package cloudserve_test

// Tests for Server.handleProjects (POST /v1/projects).
// Mirrors the push/pull handler test patterns in server_test.go.

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

// ── mock Central WITH projectLister ──────────────────────────────────────────

// projectListerCentral is a test Central that implements BOTH the core transport
// interface AND the optional projectLister capability. It is wired into a separate
// cloudserve.Server so the handleProjects capability check resolves correctly
// without touching the existing mockCentral (which does NOT implement projectLister).
type projectListerCentral struct {
	// Embed mockCentral so Apply + PullSince are covered by delegation.
	*mockCentral

	// listResult is returned by ListProjects.
	listResult []string
	// listErr, if non-nil, is returned by ListProjects.
	listErr error
}

func (p *projectListerCentral) Apply(ctx context.Context, m domain.Mutation) error {
	return p.mockCentral.Apply(ctx, m)
}

func (p *projectListerCentral) PullSince(ctx context.Context, project string, sinceSeq int64, limit int) ([]domain.Mutation, error) {
	return p.mockCentral.PullSince(ctx, project, sinceSeq, limit)
}

func (p *projectListerCentral) ListProjects(_ context.Context) ([]string, error) {
	if p.listErr != nil {
		return nil, p.listErr
	}
	return p.listResult, nil
}

// newProjectsServer constructs an httptest.Server whose Central satisfies the
// optional projectLister capability.
func newProjectsServer(t *testing.T, central *projectListerCentral) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(cloudserve.New(central, cloudserve.AllowAllVerifier()).Handler())
	t.Cleanup(ts.Close)
	return ts
}

// ── TestHandleProjects_Success ────────────────────────────────────────────────

// TestHandleProjects_Success verifies that POST /v1/projects with a Central that
// implements projectLister returns 200 and the expected ProjectsResponse JSON.
func TestHandleProjects_Success(t *testing.T) {
	wantProjects := []string{"alpha", "beta", "gamma"}

	central := &projectListerCentral{
		mockCentral: &mockCentral{},
		listResult:  wantProjects,
	}
	ts := newProjectsServer(t, central)

	body, _ := json.Marshal(syncwire.ProjectsRequest{})
	resp, err := http.Post(ts.URL+"/v1/projects", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/projects: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got syncwire.ProjectsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode ProjectsResponse: %v", err)
	}

	if len(got.Projects) != len(wantProjects) {
		t.Fatalf("Projects len=%d, want %d", len(got.Projects), len(wantProjects))
	}
	for i, w := range wantProjects {
		if got.Projects[i] != w {
			t.Errorf("Projects[%d] = %q; want %q", i, got.Projects[i], w)
		}
	}
}

// TestHandleProjects_EmptyList verifies that a Central returning an empty
// project list gives 200 with Projects: null or [].
func TestHandleProjects_EmptyList(t *testing.T) {
	central := &projectListerCentral{
		mockCentral: &mockCentral{},
		listResult:  nil,
	}
	ts := newProjectsServer(t, central)

	body, _ := json.Marshal(syncwire.ProjectsRequest{})
	resp, err := http.Post(ts.URL+"/v1/projects", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/projects: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var got syncwire.ProjectsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode ProjectsResponse: %v", err)
	}
	// nil or empty slice are both acceptable for an empty project set.
	if len(got.Projects) != 0 {
		t.Errorf("Projects = %v; want empty", got.Projects)
	}
}

// ── TestHandleProjects_501_WhenCentralDoesNotImplementProjectLister ───────────

// TestHandleProjects_501_WhenCentralLacksLister verifies that when the wrapped
// Central does NOT implement the optional projectLister interface, handleProjects
// returns 501 Not Implemented.
func TestHandleProjects_501_WhenCentralLacksLister(t *testing.T) {
	// Use the plain mockCentral (does NOT implement ListProjects).
	central := &mockCentral{}
	ts := newTestServer(t, central) // reuses the helper from server_test.go

	body, _ := json.Marshal(syncwire.ProjectsRequest{})
	resp, err := http.Post(ts.URL+"/v1/projects", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/projects: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 (Central lacks projectLister)", resp.StatusCode)
	}

	// Response must be JSON {"error":"..."}.
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

// ── TestHandleProjects_500_WhenListProjectsErrors ─────────────────────────────

// TestHandleProjects_500_WhenListProjectsErrors verifies that when ListProjects
// returns an error the server returns 500.
func TestHandleProjects_500_WhenListProjectsErrors(t *testing.T) {
	central := &projectListerCentral{
		mockCentral: &mockCentral{},
		listErr:     errors.New("DB down"),
	}
	ts := newProjectsServer(t, central)

	body, _ := json.Marshal(syncwire.ProjectsRequest{})
	resp, err := http.Post(ts.URL+"/v1/projects", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/projects: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// ── TestHandleProjects_405_WrongMethod ────────────────────────────────────────

// TestHandleProjects_405_WrongMethod verifies that GET on /v1/projects returns 405.
func TestHandleProjects_405_WrongMethod(t *testing.T) {
	central := &projectListerCentral{
		mockCentral: &mockCentral{},
		listResult:  []string{"proj"},
	}
	ts := newProjectsServer(t, central)

	resp, err := http.Get(ts.URL + "/v1/projects")
	if err != nil {
		t.Fatalf("GET /v1/projects: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/projects: status = %d, want 405", resp.StatusCode)
	}
}

// ── TestHandleProjects_AllowAllVerifier_NoHeaders ─────────────────────────────

// TestHandleProjects_AllowAllVerifier_NoHeaders proves AllowAllVerifier is a
// bypass for /v1/projects too: no auth headers → still 200. Mirrors the pattern
// in server_auth_test.go for push and pull.
func TestHandleProjects_AllowAllVerifier_NoHeaders(t *testing.T) {
	central := &projectListerCentral{
		mockCentral: &mockCentral{},
		listResult:  []string{"proj-a"},
	}
	ts := newProjectsServer(t, central)

	body, _ := json.Marshal(syncwire.ProjectsRequest{})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/projects", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Deliberately omit X-Writer-Id and X-Signature.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/projects: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("AllowAll + no auth headers: status = %d, want 200", resp.StatusCode)
	}
}
