package controlapi_test

// Tests for PUT /api/v1/projects/{project}/policy.
// Covers the three axes required by the PR-② spec:
//   - 200 on a valid policy value
//   - 400 on an invalid policy value
//   - 401 on missing/wrong token (auth middleware)
//   - 403 on cross-site origin (origin guard)

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
)

// putPolicy issues a PUT /api/v1/projects/{project}/policy request.
func putPolicy(t *testing.T, ts *httptest.Server, project, policyVal, auth, origin string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"policy": policyVal})
	req, err := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/projects/"+project+"/policy", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
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

// TestPolicyUpdate_Valid_Returns200 verifies that PUT /api/v1/projects/{project}/policy
// with a valid policy value returns 200 and the updated project+policy in the body.
func TestPolicyUpdate_Valid_Returns200(t *testing.T) {
	store := &mockStore{}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	resp := putPolicy(t, ts, "my-project", "local-only", authHeader("tok"), "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d (body: %s)", resp.StatusCode, readBody(t, resp))
	}

	// Body must contain the project name and policy.
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["project"] != "my-project" {
		t.Errorf("response project = %q; want %q", body["project"], "my-project")
	}
	if body["policy"] != "local-only" {
		t.Errorf("response policy = %q; want %q", body["policy"], "local-only")
	}

	// The store must have been called with the correct values.
	if got, ok := store.policies["my-project"]; !ok {
		t.Error("store.SetPolicy was not called for 'my-project'")
	} else if got != controlapi.PolicyLocalOnly {
		t.Errorf("store policy = %q; want %q", got, controlapi.PolicyLocalOnly)
	}
}

// TestPolicyUpdate_InvalidValue_Returns400 verifies that an unrecognised policy
// string returns 400 Bad Request without calling SetPolicy.
func TestPolicyUpdate_InvalidValue_Returns400(t *testing.T) {
	store := &mockStore{}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	resp := putPolicy(t, ts, "my-project", "not-a-real-policy", authHeader("tok"), "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid policy: want 400, got %d", resp.StatusCode)
	}

	// SetPolicy must NOT have been called (bad input rejected before store call).
	if len(store.policies) != 0 {
		t.Errorf("store.SetPolicy must not be called for invalid policy value; got: %v", store.policies)
	}
}

// TestPolicyUpdate_NoToken_Returns401 verifies that PUT without an Authorization
// header returns 401 (auth middleware fires before the handler).
func TestPolicyUpdate_NoToken_Returns401(t *testing.T) {
	_, ts := newTestServer(t, "tok", &mockStore{}, &mockSyncCtrl{}, &mockCfgStore{})

	resp := putPolicy(t, ts, "my-project", "synced", "", "") // no auth
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: want 401, got %d", resp.StatusCode)
	}
}

// TestPolicyUpdate_WrongOrigin_Returns403 verifies that PUT from a cross-site
// Origin returns 403 (origin guard fires before the handler).
func TestPolicyUpdate_WrongOrigin_Returns403(t *testing.T) {
	_, ts := newTestServer(t, "tok", &mockStore{}, &mockSyncCtrl{}, &mockCfgStore{})

	resp := putPolicy(t, ts, "my-project", "synced", authHeader("tok"), "http://evil.example.com")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("wrong origin: want 403, got %d", resp.StatusCode)
	}
}

// TestPolicyUpdate_AllThreePolicies verifies that each of the three valid policy
// strings is accepted with 200 by the endpoint.
func TestPolicyUpdate_AllThreePolicies(t *testing.T) {
	cases := []struct {
		policyStr string
		wantEnum  controlapi.Policy
	}{
		{"synced", controlapi.PolicySynced},
		{"local-only", controlapi.PolicyLocalOnly},
		{"omitted", controlapi.PolicyOmitted},
	}

	for _, tc := range cases {
		t.Run(tc.policyStr, func(t *testing.T) {
			store := &mockStore{}
			_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

			resp := putPolicy(t, ts, "proj-"+tc.policyStr, tc.policyStr, authHeader("tok"), "")
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("%q: want 200, got %d", tc.policyStr, resp.StatusCode)
			}
			if got, ok := store.policies["proj-"+tc.policyStr]; !ok {
				t.Errorf("%q: SetPolicy not called", tc.policyStr)
			} else if got != tc.wantEnum {
				t.Errorf("%q: stored policy = %q; want %q", tc.policyStr, got, tc.wantEnum)
			}
		})
	}
}
