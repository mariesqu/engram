package controlapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
)

// mockMCPHandler is a minimal http.Handler that records calls for MountMCP tests.
type mockMCPHandler struct {
	called bool
	status int
}

func (m *mockMCPHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	m.called = true
	if m.status != 0 {
		w.WriteHeader(m.status)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"mock":"mcp"}`))
}

// TestMCPMount_TokenMissing_401 verifies that MountMCP enforces bearer-token auth:
// a POST to /mcp without an Authorization header returns 401.
func TestMCPMount_TokenMissing_401(t *testing.T) {
	const token = "abc123"
	mux := http.NewServeMux()
	handler := &mockMCPHandler{}
	controlapi.MountMCP(mux, token, handler)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("missing token: got %d, want 401", w.Code)
	}
	if handler.called {
		t.Error("underlying handler must not be called when token is missing")
	}
}

// TestMCPMount_WrongToken_401 verifies that MountMCP rejects a wrong token with 401.
func TestMCPMount_WrongToken_401(t *testing.T) {
	const token = "correct-token"
	mux := http.NewServeMux()
	handler := &mockMCPHandler{}
	controlapi.MountMCP(mux, token, handler)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", w.Code)
	}
	if handler.called {
		t.Error("underlying handler must not be called when token is wrong")
	}
}

// TestMCPMount_ValidToken_ForwardsToHandler verifies that MountMCP forwards
// requests with the correct bearer token to the underlying MCPHandler.
func TestMCPMount_ValidToken_ForwardsToHandler(t *testing.T) {
	const token = "valid-token-xyz"
	mux := http.NewServeMux()
	handler := &mockMCPHandler{}
	controlapi.MountMCP(mux, token, handler)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("valid token: got %d, want 200", w.Code)
	}
	if !handler.called {
		t.Error("underlying handler must be called when token is correct")
	}
}

// TestMCPMount_AbsentWhenNotRegistered verifies that a ServeMux WITHOUT a
// MountMCP call returns 404 for /mcp (not a MCP response).
// This is the "absent-when-not-opted-in" proof per the spec.
func TestMCPMount_AbsentWhenNotRegistered(t *testing.T) {
	mux := http.NewServeMux()
	// Deliberately do NOT call MountMCP — simulates stdio mode.

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer any-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Errorf("/mcp returned 200 on a mux without MountMCP — route must be absent")
	}
	// Standard ServeMux returns 404 for unmatched paths.
	if w.Code != http.StatusNotFound {
		t.Logf("/mcp on unmounted mux: got %d (404 expected, other non-200 also acceptable)", w.Code)
	}
}

// TestMCPMount_TrailingSlash_ForwardsToHandler verifies that /mcp/ (trailing
// slash) is also routed to the MCPHandler when the token is correct.
// The Streamable HTTP server uses both the bare path and path with trailing
// slash for GET/DELETE streaming endpoints.
func TestMCPMount_TrailingSlash_ForwardsToHandler(t *testing.T) {
	const token = "trail-slash-token"
	mux := http.NewServeMux()
	handler := &mockMCPHandler{}
	controlapi.MountMCP(mux, token, handler)

	req := httptest.NewRequest(http.MethodGet, "/mcp/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusUnauthorized || w.Code == http.StatusNotFound {
		t.Errorf("/mcp/ with valid token: got %d, handler should have been called", w.Code)
	}
	if !handler.called {
		t.Error("underlying handler must be called for /mcp/ with correct token")
	}
}

// TestMCPMount_EmptyToken_NeverAuthenticates: a zero-value/misconfigured
// server (token == "") must reject EVERY request — without the guard,
// "Authorization: Bearer " (empty credential) would equal "Bearer " + "".
func TestMCPMount_EmptyToken_NeverAuthenticates(t *testing.T) {
	mux := http.NewServeMux()
	controlapi.MountMCP(mux, "", &mockMCPHandler{})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	for _, auth := range []string{"", "Bearer ", "Bearer x"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("empty-token server, auth %q: got %d, want 401", auth, resp.StatusCode)
		}
	}
}
