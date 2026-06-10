package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
)

// TestControlClient_NoDaemonJSON verifies that NewControlClient returns
// ErrDaemonNotRunning when no daemon.json exists in the directory.
func TestControlClient_NoDaemonJSON(t *testing.T) {
	dir := t.TempDir()
	_, err := NewControlClient(dir)
	if err == nil {
		t.Fatal("want error for missing daemon.json, got nil")
	}
	if !errors.Is(err, ErrDaemonNotRunning) {
		t.Errorf("want ErrDaemonNotRunning, got %v", err)
	}
}

// TestControlClient_Stale401_Retries verifies that a 401 response triggers
// a re-read of daemon.json and a single retry with the refreshed token.
func TestControlClient_Stale401_Retries(t *testing.T) {
	dir := t.TempDir()

	// First token (stale) → 401. Second token (fresh) → 200.
	const staleToken = "stale-token"
	const freshToken = "fresh-token"

	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		auth := r.Header.Get("Authorization")
		if auth == "Bearer "+freshToken {
			st := controlapi.Status{DaemonVersion: "test"}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(st)
			return
		}
		// Any other token → 401
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid token"}`))
	}))
	defer ts.Close()

	// Extract host:port from the test server URL.
	// ts.URL is "http://127.0.0.1:<port>"
	var port int
	if _, err := parseTestServerURL(ts.URL, &port); err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}

	// Write daemon.json with the stale token initially.
	if err := controlapi.WriteDaemonJSON(dir, staleToken, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON (stale): %v", err)
	}

	client, err := NewControlClient(dir)
	if err != nil {
		t.Fatalf("NewControlClient: %v", err)
	}

	// Before the retry, update daemon.json with the fresh token so the
	// re-read finds the new token.
	if err := controlapi.WriteDaemonJSON(dir, freshToken, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON (fresh): %v", err)
	}

	var st controlapi.Status
	if err := client.Get("/api/v1/status", &st); err != nil {
		t.Fatalf("Get after stale token retry: %v", err)
	}

	if st.DaemonVersion != "test" {
		t.Errorf("DaemonVersion: got %q, want %q", st.DaemonVersion, "test")
	}
	// Must have made exactly 2 calls: first with stale token → 401, second with fresh token → 200.
	if callCount != 2 {
		t.Errorf("call count: got %d, want 2 (initial 401 + retry with fresh token)", callCount)
	}
}

// TestControlClient_DoubleStale401_FailsWithDaemonNotRunning verifies that
// two consecutive 401 responses (daemon restarted with yet another token)
// cause the client to return ErrDaemonNotRunning rather than looping.
func TestControlClient_DoubleStale401_FailsWithDaemonNotRunning(t *testing.T) {
	dir := t.TempDir()
	const badToken = "always-bad"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid token"}`))
	}))
	defer ts.Close()

	var port int
	if _, err := parseTestServerURL(ts.URL, &port); err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}

	if err := controlapi.WriteDaemonJSON(dir, badToken, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	client, err := NewControlClient(dir)
	if err != nil {
		t.Fatalf("NewControlClient: %v", err)
	}

	var st controlapi.Status
	err = client.Get("/api/v1/status", &st)
	if err == nil {
		t.Fatal("want error after double 401, got nil")
	}
	if !errors.Is(err, ErrDaemonNotRunning) {
		t.Errorf("want ErrDaemonNotRunning, got: %v", err)
	}
}

// parseTestServerURL extracts the port from an httptest.Server URL.
func parseTestServerURL(rawURL string, port *int) (string, error) {
	// rawURL is "http://127.0.0.1:<port>"
	// Simplest parse: find the last ':' and parse the rest as int.
	for i := len(rawURL) - 1; i >= 0; i-- {
		if rawURL[i] == ':' {
			var p int
			_, err := parsePort(rawURL[i+1:], &p)
			if err != nil {
				return "", err
			}
			*port = p
			return rawURL[:i], nil
		}
	}
	return "", errors.New("no port found in URL: " + rawURL)
}

// parsePort parses an integer from s into *p.
func parsePort(s string, p *int) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	*p = n
	return n, nil
}

// TestRunStatusCmd_NoDaemon verifies that `engram status` returns exit code 1
// when no daemon.json is present.
func TestCLI_Status_NoDaemon(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Ensure no daemon.json in that directory.
	err := runStatusCmd([]string{"--db", dbPath})
	if err == nil {
		t.Fatal("want error for no daemon, got nil")
	}
	if !errors.Is(err, ErrDaemonNotRunning) {
		t.Errorf("want ErrDaemonNotRunning, got: %v", err)
	}
}

// TestCLI_UI_NoDaemon_Errors verifies that `engram ui` returns an error
// when no daemon.json is present.
func TestCLI_UI_NoDaemon_Errors(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	err := runUICmd([]string{"--db", dbPath})
	if err == nil {
		t.Fatal("want error for no daemon, got nil")
	}
	if !errors.Is(err, ErrDaemonNotRunning) {
		t.Errorf("want ErrDaemonNotRunning, got: %v", err)
	}
}

// TestCLI_Status_PrintsOutput verifies that `engram status` fetches and
// prints the daemon status from an httptest server.
func TestCLI_Status_PrintsOutput(t *testing.T) {
	dir := t.TempDir()
	const token = "test-status-token"

	centralURL := "https://central.example.com"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		st := controlapi.Status{
			CentralConnected: true,
			CentralURL:       &centralURL,
			DaemonVersion:    "0.1.0-test",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	}))
	defer ts.Close()

	var port int
	if _, err := parseTestServerURL(ts.URL, &port); err != nil {
		t.Fatalf("parse URL: %v", err)
	}

	if err := controlapi.WriteDaemonJSON(dir, token, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	dbPath := filepath.Join(dir, "test.db")
	err := runStatusCmd([]string{"--db", dbPath})
	if err != nil {
		t.Fatalf("runStatusCmd: %v", err)
	}
}
