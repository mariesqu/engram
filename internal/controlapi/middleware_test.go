package controlapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
)

// TestOriginCheck_POST_WrongOrigin_Middleware verifies that a POST request
// with a wrong Origin header receives 403 Forbidden from withOrigin.
// We expose this via a thin wrapper that wires withOrigin on a test mux.
func TestOriginCheck_POST_WrongOrigin_Middleware(t *testing.T) {
	srv := controlapi.New("tok", 7700, &mockStore{}, &mockSyncCtrl{}, &mockCfgStore{}, "v1")

	// Build a test mux that applies withOrigin on a POST route.
	mux := http.NewServeMux()
	mux.HandleFunc("/test", srv.WithAuthAndOrigin(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/test", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Origin", "http://evil.example.com")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("wrong origin POST: got %d, want 403", resp.StatusCode)
	}
}

// TestOriginCheck_POST_CorrectOrigin verifies that a POST with the correct
// loopback Origin passes the origin check.
func TestOriginCheck_POST_CorrectOrigin(t *testing.T) {
	srv := controlapi.New("tok", 7700, &mockStore{}, &mockSyncCtrl{}, &mockCfgStore{}, "v1")

	mux := http.NewServeMux()
	mux.HandleFunc("/test", srv.WithAuthAndOrigin(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/test", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Origin", "http://127.0.0.1:7700")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("correct origin POST: got %d, want 200", resp.StatusCode)
	}
}

// TestOriginCheck_GET_NoOriginRequired verifies GET is exempt from origin check.
func TestOriginCheck_GET_NoOriginRequired(t *testing.T) {
	srv := controlapi.New("tok", 7700, &mockStore{}, &mockSyncCtrl{}, &mockCfgStore{}, "v1")

	mux := http.NewServeMux()
	mux.HandleFunc("/test", srv.WithAuthAndOrigin(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/test", nil)
	req.Header.Set("Authorization", "Bearer tok")
	// No Origin header.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET without Origin: got %d, want 200", resp.StatusCode)
	}
}
