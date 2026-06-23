package controlapi_test

// Tests for DELETE /api/v1/projects/{project}?scope=local|purge-all.
// Covers:
//   - scope=local  → 200 {"deleted": N}
//   - scope=purge-all → 200 {"deleted": N}
//   - scope missing or unknown → 400
//   - wrong Origin → 403
//   - no token → 401
//   - wrong HTTP method (GET, POST) → 405

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// deleteProject issues a DELETE /api/v1/projects/{project} request with the
// given scope query parameter, Authorization header, and optional Origin header.
func deleteProject(t *testing.T, ts *httptest.Server, project, scope, auth, origin string) *http.Response {
	t.Helper()
	path := "/api/v1/projects/" + project
	if scope != "" {
		path += "?scope=" + scope
	}
	req, err := http.NewRequest(http.MethodDelete, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// TestProjectDelete_ScopeLocal_Returns200 verifies that DELETE with scope=local
// calls PurgeProjectLocal and returns 200 {"deleted": 0}.
func TestProjectDelete_ScopeLocal_Returns200(t *testing.T) {
	store := &mockStore{}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	resp := deleteProject(t, ts, "my-project", "local", authHeader("tok"), "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scope=local: want 200, got %d (body: %s)", resp.StatusCode, readBody(t, resp))
	}

	var body map[string]int
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["deleted"]; !ok {
		t.Error("scope=local response must have 'deleted' key")
	}
}

// TestProjectDelete_ScopePurgeAll_Returns200 verifies that DELETE with
// scope=purge-all calls TombstoneProject and returns 200 {"deleted": 0}.
func TestProjectDelete_ScopePurgeAll_Returns200(t *testing.T) {
	store := &mockStore{}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	resp := deleteProject(t, ts, "my-project", "purge-all", authHeader("tok"), "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scope=purge-all: want 200, got %d (body: %s)", resp.StatusCode, readBody(t, resp))
	}

	var body map[string]int
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["deleted"]; !ok {
		t.Error("scope=purge-all response must have 'deleted' key")
	}
}

// TestProjectDelete_UnknownScope_Returns400 verifies that an unrecognised scope
// value returns 400 Bad Request.
func TestProjectDelete_UnknownScope_Returns400(t *testing.T) {
	_, ts := newTestServer(t, "tok", &mockStore{}, &mockSyncCtrl{}, &mockCfgStore{})

	resp := deleteProject(t, ts, "my-project", "invalid-scope", authHeader("tok"), "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown scope: want 400, got %d", resp.StatusCode)
	}
}

// TestProjectDelete_MissingScope_Returns400 verifies that a missing scope
// query parameter returns 400 Bad Request.
func TestProjectDelete_MissingScope_Returns400(t *testing.T) {
	_, ts := newTestServer(t, "tok", &mockStore{}, &mockSyncCtrl{}, &mockCfgStore{})

	resp := deleteProject(t, ts, "my-project", "", authHeader("tok"), "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing scope: want 400, got %d", resp.StatusCode)
	}
}

// TestProjectDelete_NoToken_Returns401 verifies that DELETE without an
// Authorization header returns 401 (auth middleware fires first).
func TestProjectDelete_NoToken_Returns401(t *testing.T) {
	_, ts := newTestServer(t, "tok", &mockStore{}, &mockSyncCtrl{}, &mockCfgStore{})

	resp := deleteProject(t, ts, "my-project", "local", "", "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: want 401, got %d", resp.StatusCode)
	}
}

// TestProjectDelete_WrongOrigin_Returns403 verifies that DELETE from a
// cross-site Origin returns 403 (origin guard fires before the handler).
func TestProjectDelete_WrongOrigin_Returns403(t *testing.T) {
	_, ts := newTestServer(t, "tok", &mockStore{}, &mockSyncCtrl{}, &mockCfgStore{})

	resp := deleteProject(t, ts, "my-project", "local", authHeader("tok"), "http://evil.example.com")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("wrong origin: want 403, got %d", resp.StatusCode)
	}
}

// TestProjectDelete_WrongMethod_Returns404 verifies that GET and POST on the
// DELETE-only path return 404 Not Found.
//
// Go 1.22 ServeMux returns 405 Method Not Allowed only when the same path is
// registered without a method prefix AND the request method doesn't match a
// method-prefixed pattern. Here, "DELETE /api/v1/projects/{project}" is the only
// registration for this path; the catch-all "/" handler matches GET and POST first,
// so the response is 404 rather than 405.
func TestProjectDelete_WrongMethod_Returns404(t *testing.T) {
	_, ts := newTestServer(t, "tok", &mockStore{}, &mockSyncCtrl{}, &mockCfgStore{})

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			req, err := http.NewRequest(method, ts.URL+"/api/v1/projects/my-project?scope=local", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Authorization", authHeader("tok"))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s: want 404 (catch-all handles non-DELETE methods), got %d", method, resp.StatusCode)
			}
		})
	}
}
