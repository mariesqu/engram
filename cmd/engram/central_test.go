package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
)

// TestCLI_CentralConnect_HappyPath verifies that `engram central connect`
// POSTs to /api/v1/central/connect with the exact expected JSON field names
// (central_url, writer_id, writer_key) and the key resolved from ENGRAM_WRITER_KEY.
func TestCLI_CentralConnect_HappyPath(t *testing.T) {
	dir := t.TempDir()
	const token = "test-connect-token"

	var gotBody controlapi.ConnectRequest
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/api/v1/central/connect" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		st := controlapi.Status{CentralConnected: true, CentralConfigured: true, DaemonVersion: "test"}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	}))
	defer ts.Close()

	port := mustParsePort(t, ts.URL)
	if err := controlapi.WriteDaemonJSON(dir, token, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	t.Setenv("ENGRAM_WRITER_KEY", "deadbeef")

	dbPath := filepath.Join(dir, "test.db")
	err := runCentralConnectCmdWithIO(
		[]string{"--url", "http://central.example.com", "--writer-id", "writer-1", "--db", dbPath},
		strings.NewReader(""),
		false,
	)
	if err != nil {
		t.Fatalf("runCentralConnectCmdWithIO: %v", err)
	}

	if gotAuth != "Bearer "+token {
		t.Errorf("Authorization header: got %q, want %q", gotAuth, "Bearer "+token)
	}
	if gotBody.CentralURL != "http://central.example.com" {
		t.Errorf("central_url: got %q, want %q", gotBody.CentralURL, "http://central.example.com")
	}
	if gotBody.WriterID != "writer-1" {
		t.Errorf("writer_id: got %q, want %q", gotBody.WriterID, "writer-1")
	}
	if gotBody.WriterKey != "deadbeef" {
		t.Errorf("writer_key: got %q, want %q", gotBody.WriterKey, "deadbeef")
	}
}

// TestCLI_CentralConnect_KeyFromEnv verifies ENGRAM_WRITER_KEY is read and
// stdin is never consulted when the env var is set.
func TestCLI_CentralConnect_KeyFromEnv(t *testing.T) {
	dir := t.TempDir()
	const token = "env-key-token"

	var gotKey string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body controlapi.ConnectRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotKey = body.WriterKey
		st := controlapi.Status{DaemonVersion: "test"}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	}))
	defer ts.Close()

	port := mustParsePort(t, ts.URL)
	if err := controlapi.WriteDaemonJSON(dir, token, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	t.Setenv("ENGRAM_WRITER_KEY", "cafef00d")

	dbPath := filepath.Join(dir, "test.db")
	// stdin deliberately provides a DIFFERENT value — env must win, stdin must
	// never be consumed when the env var is set.
	err := runCentralConnectCmdWithIO(
		[]string{"--url", "http://central.example.com", "--writer-id", "writer-1", "--db", dbPath},
		strings.NewReader("should-not-be-used\n"),
		false,
	)
	if err != nil {
		t.Fatalf("runCentralConnectCmdWithIO: %v", err)
	}
	if gotKey != "cafef00d" {
		t.Errorf("writer_key: got %q, want %q (from ENGRAM_WRITER_KEY)", gotKey, "cafef00d")
	}
}

// TestCLI_CentralConnect_KeyFromPipedStdin verifies that when ENGRAM_WRITER_KEY
// is unset, a single line piped on stdin supplies the key.
func TestCLI_CentralConnect_KeyFromPipedStdin(t *testing.T) {
	dir := t.TempDir()
	const token = "stdin-key-token"

	var gotKey string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body controlapi.ConnectRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotKey = body.WriterKey
		st := controlapi.Status{DaemonVersion: "test"}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	}))
	defer ts.Close()

	port := mustParsePort(t, ts.URL)
	if err := controlapi.WriteDaemonJSON(dir, token, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	t.Setenv("ENGRAM_WRITER_KEY", "")

	dbPath := filepath.Join(dir, "test.db")
	err := runCentralConnectCmdWithIO(
		[]string{"--url", "http://central.example.com", "--writer-id", "writer-1", "--db", dbPath},
		strings.NewReader("piped-hex-key\n"),
		false, // not a terminal — simulates a pipe
	)
	if err != nil {
		t.Fatalf("runCentralConnectCmdWithIO: %v", err)
	}
	if gotKey != "piped-hex-key" {
		t.Errorf("writer_key: got %q, want %q (from piped stdin)", gotKey, "piped-hex-key")
	}
}

// TestCLI_CentralConnect_MissingKey verifies that an empty ENGRAM_WRITER_KEY
// and empty stdin produce a clear, non-nil error instead of sending an empty key.
func TestCLI_CentralConnect_MissingKey(t *testing.T) {
	dir := t.TempDir()
	const token = "missing-key-token"

	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	port := mustParsePort(t, ts.URL)
	if err := controlapi.WriteDaemonJSON(dir, token, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	t.Setenv("ENGRAM_WRITER_KEY", "")

	dbPath := filepath.Join(dir, "test.db")
	err := runCentralConnectCmdWithIO(
		[]string{"--url", "http://central.example.com", "--writer-id", "writer-1", "--db", dbPath},
		strings.NewReader(""), // EOF immediately — no key supplied
		false,
	)
	if err == nil {
		t.Fatal("want error for missing writer key, got nil")
	}
	if !strings.Contains(err.Error(), "writer key") {
		t.Errorf("error should mention writer key; got: %v", err)
	}
	if called {
		t.Error("server should never be called when the writer key is missing")
	}
}

// TestCLI_CentralDisconnect_HappyPath verifies that `engram central disconnect`
// POSTs to /api/v1/central/disconnect and prints a retention confirmation.
func TestCLI_CentralDisconnect_HappyPath(t *testing.T) {
	dir := t.TempDir()
	const token = "disconnect-token"

	var gotPath, gotMethod string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		st := controlapi.Status{CentralConnected: false, DaemonVersion: "test"}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	}))
	defer ts.Close()

	port := mustParsePort(t, ts.URL)
	if err := controlapi.WriteDaemonJSON(dir, token, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	dbPath := filepath.Join(dir, "test.db")
	err := runCentralDisconnectCmd([]string{"--db", dbPath})
	if err != nil {
		t.Fatalf("runCentralDisconnectCmd: %v", err)
	}

	if gotPath != "/api/v1/central/disconnect" {
		t.Errorf("path: got %q, want %q", gotPath, "/api/v1/central/disconnect")
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q, want POST", gotMethod)
	}
}

// TestCLI_CentralConnect_DaemonNotRunning verifies the ErrDaemonNotRunning
// sentinel is returned (with the actionable message style of other
// subcommands) when no daemon.json exists.
func TestCLI_CentralConnect_DaemonNotRunning(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	t.Setenv("ENGRAM_WRITER_KEY", "deadbeef")

	err := runCentralConnectCmdWithIO(
		[]string{"--url", "http://central.example.com", "--writer-id", "writer-1", "--db", dbPath},
		strings.NewReader(""),
		false,
	)
	if err == nil {
		t.Fatal("want error for no daemon, got nil")
	}
	if !errors.Is(err, ErrDaemonNotRunning) {
		t.Errorf("want ErrDaemonNotRunning, got: %v", err)
	}
}

// TestCLI_CentralDisconnect_DaemonNotRunning mirrors the connect case for disconnect.
func TestCLI_CentralDisconnect_DaemonNotRunning(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	err := runCentralDisconnectCmd([]string{"--db", dbPath})
	if err == nil {
		t.Fatal("want error for no daemon, got nil")
	}
	if !errors.Is(err, ErrDaemonNotRunning) {
		t.Errorf("want ErrDaemonNotRunning, got: %v", err)
	}
}

// TestCLI_CentralConnect_MissingDB verifies --db is required and ENGRAM_DB is
// consulted as the fallback, isolated from the developer's real environment.
func TestCLI_CentralConnect_MissingDB(t *testing.T) {
	t.Setenv("ENGRAM_DB", "")
	t.Setenv("ENGRAM_WRITER_KEY", "deadbeef")

	err := runCentralConnectCmdWithIO(
		[]string{"--url", "http://central.example.com", "--writer-id", "writer-1"},
		strings.NewReader(""),
		false,
	)
	if err == nil {
		t.Fatal("want error for missing --db, got nil")
	}
	if !strings.Contains(err.Error(), "--db") {
		t.Errorf("error should mention --db; got: %v", err)
	}
}

// TestCLI_CentralDisconnect_MissingDB mirrors the connect case for disconnect.
func TestCLI_CentralDisconnect_MissingDB(t *testing.T) {
	t.Setenv("ENGRAM_DB", "")

	err := runCentralDisconnectCmd([]string{})
	if err == nil {
		t.Fatal("want error for missing --db, got nil")
	}
	if !strings.Contains(err.Error(), "--db") {
		t.Errorf("error should mention --db; got: %v", err)
	}
}

// TestCLI_Central_NoArgs_PrintsUsage verifies `engram central` with no
// subcommand prints usage and returns nil (matches config/sync dispatch style).
func TestCLI_Central_NoArgs_PrintsUsage(t *testing.T) {
	if err := runCentralCmd([]string{}); err != nil {
		t.Errorf("no-args central: unexpected error: %v", err)
	}
}

// TestCLI_Central_UnknownSubcommand verifies an unknown subcommand errors.
func TestCLI_Central_UnknownSubcommand(t *testing.T) {
	if err := runCentralCmd([]string{"bogus"}); err == nil {
		t.Error("unknown central subcommand should return error")
	}
}

// TestCLI_CentralConnect_MissingURL verifies --url is required.
func TestCLI_CentralConnect_MissingURL(t *testing.T) {
	err := runCentralConnectCmdWithIO(
		[]string{"--writer-id", "writer-1", "--db", "irrelevant.db"},
		strings.NewReader(""),
		false,
	)
	if err == nil {
		t.Fatal("want error for missing --url, got nil")
	}
	if !strings.Contains(err.Error(), "--url") {
		t.Errorf("error should mention --url; got: %v", err)
	}
}

// TestCLI_CentralConnect_MissingWriterID verifies --writer-id is required.
func TestCLI_CentralConnect_MissingWriterID(t *testing.T) {
	err := runCentralConnectCmdWithIO(
		[]string{"--url", "http://central.example.com", "--db", "irrelevant.db"},
		strings.NewReader(""),
		false,
	)
	if err == nil {
		t.Fatal("want error for missing --writer-id, got nil")
	}
	if !strings.Contains(err.Error(), "--writer-id") {
		t.Errorf("error should mention --writer-id; got: %v", err)
	}
}

// TestReadWriterKey_TerminalPromptGoesToPromptWriter locks the security
// contract of the interactive path: the "Writer key (hex): " prompt is
// written to the injected prompt writer (os.Stderr in production) and the
// key is read from stdin. With the prompt destination injected, the
// function has no access to stdout at all — a regression that redirected
// the prompt to stdout would have to change this call site and fail here.
func TestReadWriterKey_TerminalPromptGoesToPromptWriter(t *testing.T) {
	t.Setenv("ENGRAM_WRITER_KEY", "")

	var prompt strings.Builder
	key, err := readWriterKey(strings.NewReader("deadbeef\n"), true, &prompt)
	if err != nil {
		t.Fatalf("readWriterKey: %v", err)
	}
	if key != "deadbeef" {
		t.Errorf("key = %q, want %q", key, "deadbeef")
	}
	if prompt.String() != "Writer key (hex): " {
		t.Errorf("prompt writer got %q, want %q", prompt.String(), "Writer key (hex): ")
	}
}

// TestReadWriterKey_NonTerminalNoPrompt verifies the piped-stdin path stays
// silent: no prompt bytes are emitted when stdin is not a terminal.
func TestReadWriterKey_NonTerminalNoPrompt(t *testing.T) {
	t.Setenv("ENGRAM_WRITER_KEY", "")

	var prompt strings.Builder
	key, err := readWriterKey(strings.NewReader("cafef00d\n"), false, &prompt)
	if err != nil {
		t.Fatalf("readWriterKey: %v", err)
	}
	if key != "cafef00d" {
		t.Errorf("key = %q, want %q", key, "cafef00d")
	}
	if prompt.Len() != 0 {
		t.Errorf("prompt writer got %q, want no output on the non-terminal path", prompt.String())
	}
}
