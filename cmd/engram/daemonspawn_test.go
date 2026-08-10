package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/controlapi"
)

// TestBuildSpawnCmd_ArgsIncludeHTTPTransport verifies that the command built
// for auto-launching a resident daemon (used by both 'engram tray' and
// 'engram connect' via spawnDetachedDaemon) always includes
// --http --transport http — without --transport http the spawned daemon
// would never mount /mcp, breaking 'engram connect' clients. This asserts
// on the constructed *exec.Cmd only; it never starts a real process.
func TestBuildSpawnCmd_ArgsIncludeHTTPTransport(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	cmd := buildSpawnCmd("engram", dbPath)
	closeSpawnLog(t, cmd)

	got := strings.Join(cmd.Args, " ")
	for _, want := range []string{"daemon", "--db", dbPath, "--http", "--transport", "http"} {
		if !strings.Contains(got, want) {
			t.Errorf("spawn command args %v missing %q", cmd.Args, want)
		}
	}
	// --http and --transport http must both be present (not just one).
	if !strings.Contains(got, "--http --transport http") {
		t.Errorf("spawn command args %v: want \"--http --transport http\" together, got %q", cmd.Args, got)
	}
}

// TestBuildSpawnCmd_DetachedAndNoStdin verifies the spawned command carries
// platform-specific detach attributes (SysProcAttr, non-nil on every
// platform we build for — see spawn_windows.go / spawn_other.go) and never
// inherits the parent's stdin, so it survives the parent exiting. Stdout/
// Stderr are no longer simply "not inherited" — they are wired to the spawn
// log; see TestBuildSpawnCmd_LogFileOpensSuccessfully and
// TestBuildSpawnCmd_LogFileOpenFailure_FallsBackToDiscard below.
func TestBuildSpawnCmd_DetachedAndNoStdin(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	cmd := buildSpawnCmd("engram", dbPath)
	closeSpawnLog(t, cmd)

	if cmd.SysProcAttr == nil {
		t.Error("SysProcAttr is nil; spawned daemon would not be detached from the parent")
	}
	if cmd.Stdin != nil {
		t.Error("spawned daemon must not inherit the parent's stdin")
	}
}

// TestBuildSpawnCmd_LogFileOpensSuccessfully verifies that when the spawn
// log's directory exists, buildSpawnCmd wires the child's stdout AND stderr
// to the SAME daemon-spawn.log file next to dbPath, with a header line
// naming the spawn args already written before the child would ever start —
// so even a child that dies immediately after Start() leaves a diagnosable
// trace (see openSpawnLog).
func TestBuildSpawnCmd_LogFileOpensSuccessfully(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	cmd := buildSpawnCmd("engram", dbPath)
	closeSpawnLog(t, cmd)

	stdoutFile, ok := cmd.Stdout.(*os.File)
	if !ok {
		t.Fatalf("cmd.Stdout = %#v, want an *os.File (the spawn log)", cmd.Stdout)
	}
	if cmd.Stderr != cmd.Stdout {
		t.Errorf("cmd.Stderr must be the SAME file as cmd.Stdout (combined log); got %#v vs %#v", cmd.Stderr, cmd.Stdout)
	}

	wantPath := filepath.Join(dir, daemonSpawnLogName)
	if stdoutFile.Name() != wantPath {
		t.Errorf("log file path = %q, want %q", stdoutFile.Name(), wantPath)
	}

	contents, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read spawn log: %v", err)
	}
	if !strings.Contains(string(contents), "daemon") || !strings.Contains(string(contents), dbPath) {
		t.Errorf("spawn log header should name the spawn args (daemon --db %s ...); got %q", dbPath, contents)
	}
}

// TestBuildSpawnCmd_LogFileOpenFailure_FallsBackToDiscard verifies that when
// the spawn log cannot be opened (here: its directory does not exist),
// buildSpawnCmd falls back to nil (discard) instead of failing — a missing
// log must never block a spawn.
func TestBuildSpawnCmd_LogFileOpenFailure_FallsBackToDiscard(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "does-not-exist", "test.db")

	cmd := buildSpawnCmd("engram", dbPath)
	closeSpawnLog(t, cmd)

	if cmd.Stdout != nil {
		t.Errorf("cmd.Stdout = %#v, want nil (fall back to discard when the log cannot be opened)", cmd.Stdout)
	}
	if cmd.Stderr != nil {
		t.Errorf("cmd.Stderr = %#v, want nil (fall back to discard when the log cannot be opened)", cmd.Stderr)
	}
}

// TestSpawnLogNeedsRotation table-drives the pure daemonSpawnLogMaxBytes
// threshold decision (no filesystem access).
func TestSpawnLogNeedsRotation(t *testing.T) {
	tests := []struct {
		name string
		size int64
		want bool
	}{
		{name: "well under cap", size: 1024, want: false},
		{name: "just under cap", size: daemonSpawnLogMaxBytes - 1, want: false},
		{name: "exactly at cap", size: daemonSpawnLogMaxBytes, want: true},
		{name: "well over cap", size: daemonSpawnLogMaxBytes * 2, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := spawnLogNeedsRotation(tt.size); got != tt.want {
				t.Errorf("spawnLogNeedsRotation(%d) = %v, want %v", tt.size, got, tt.want)
			}
		})
	}
}

// TestRotateSpawnLogIfNeeded_RotatesWhenOversize verifies that an
// at-or-over-cap spawn log is renamed to its .old sibling — overwriting any
// previous .old — so a fresh log can be created afterward.
func TestRotateSpawnLogIfNeeded_RotatesWhenOversize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, daemonSpawnLogName)
	oldPath := path + ".old"

	if err := os.WriteFile(oldPath, []byte("stale .old content"), 0o600); err != nil {
		t.Fatalf("seed stale .old: %v", err)
	}
	oversize := []byte(strings.Repeat("x", daemonSpawnLogMaxBytes))
	if err := os.WriteFile(path, oversize, 0o600); err != nil {
		t.Fatalf("seed oversize log: %v", err)
	}

	rotateSpawnLogIfNeeded(path)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("original log should have been renamed away; Stat err = %v", err)
	}
	rotated, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatalf("read rotated .old: %v", err)
	}
	if len(rotated) != len(oversize) {
		t.Errorf(".old file size = %d, want %d (the oversize log's content, overwriting the stale .old)", len(rotated), len(oversize))
	}
}

// TestRotateSpawnLogIfNeeded_NoOpBelowCap verifies a log under the size cap
// is left untouched (no rotation, no .old file created).
func TestRotateSpawnLogIfNeeded_NoOpBelowCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, daemonSpawnLogName)
	if err := os.WriteFile(path, []byte("small"), 0o600); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	rotateSpawnLogIfNeeded(path)

	if _, err := os.Stat(path + ".old"); !os.IsNotExist(err) {
		t.Error(".old file should not exist when the log is under the size cap")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(contents) != "small" {
		t.Errorf("log contents changed unexpectedly: %q", contents)
	}
}

// TestRotateSpawnLogIfNeeded_NoExistingLog verifies a first-ever spawn (no
// log file yet) is a safe no-op — Stat's ErrNotExist must not be treated as
// a fatal condition.
func TestRotateSpawnLogIfNeeded_NoExistingLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, daemonSpawnLogName)

	rotateSpawnLogIfNeeded(path) // must not panic

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("no log should have been created by rotation alone")
	}
}

// closeSpawnLog closes the *os.File buildSpawnCmd wired as cmd.Stdout (if
// any — openSpawnLog can fall back to nil on failure) once the test is done
// with cmd, so a lingering open handle never blocks t.TempDir()'s cleanup
// from removing the directory (Windows disallows deleting a file that is
// still open).
func closeSpawnLog(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	f, ok := cmd.Stdout.(*os.File)
	if !ok {
		return
	}
	t.Cleanup(func() { _ = f.Close() })
}

// TestWaitForHealthyDaemon_SucceedsOnceProbePasses verifies that
// waitForHealthyDaemon polls until daemon.json exists AND probeFn passes,
// returning the DaemonJSON it read. sleepFn is a no-op so the test runs
// instantly despite exercising several "attempts".
func TestWaitForHealthyDaemon_SucceedsOnceProbePasses(t *testing.T) {
	dir := t.TempDir()

	var sleeps int
	sleepFn := func(time.Duration) {
		sleeps++
		// Only write daemon.json (simulate the daemon coming up) after a
		// couple of poll attempts, to prove the loop actually retries.
		if sleeps == 3 {
			if err := controlapi.WriteDaemonJSON(dir, "tok", 12345, os.Getpid()); err != nil {
				t.Fatalf("WriteDaemonJSON: %v", err)
			}
		}
	}

	var probes int
	probeFn := func(d controlapi.DaemonJSON) error {
		probes++
		if d.Token != "tok" {
			return errors.New("not yet healthy")
		}
		return nil
	}

	got, err := waitForHealthyDaemon(context.Background(), dir, 10, time.Millisecond, sleepFn, probeFn)
	if err != nil {
		t.Fatalf("waitForHealthyDaemon: %v", err)
	}
	if got.Token != "tok" || got.Port != 12345 {
		t.Errorf("got %+v, want token=tok port=12345", got)
	}
	if sleeps < 3 {
		t.Errorf("sleeps=%d, want at least 3 (the loop must retry until daemon.json appears)", sleeps)
	}
	if probes == 0 {
		t.Error("probeFn was never called")
	}
}

// TestWaitForHealthyDaemon_TimesOutWhenNeverHealthy verifies that
// waitForHealthyDaemon gives up after the configured attempt budget with a
// clear error, rather than blocking forever, when the daemon never becomes
// healthy (e.g. it crashed immediately after spawning).
func TestWaitForHealthyDaemon_TimesOutWhenNeverHealthy(t *testing.T) {
	dir := t.TempDir()

	var sleeps int
	sleepFn := func(time.Duration) { sleeps++ }
	probeFn := func(controlapi.DaemonJSON) error { return errors.New("never healthy") }

	_, err := waitForHealthyDaemon(context.Background(), dir, 5, time.Millisecond, sleepFn, probeFn)
	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "did not become healthy") {
		t.Errorf("error should be a clear timeout message; got: %v", err)
	}
	if sleeps != 5 {
		t.Errorf("sleeps=%d, want exactly 5 (the configured attempt budget)", sleeps)
	}
}

// TestWaitForHealthyDaemon_ContextCancelled verifies that waitForHealthyDaemon
// stops early and returns the context error when ctx is cancelled mid-poll,
// instead of running out the full attempt budget.
func TestWaitForHealthyDaemon_ContextCancelled(t *testing.T) {
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	var sleeps int
	sleepFn := func(time.Duration) {
		sleeps++
		if sleeps == 2 {
			cancel()
		}
	}
	probeFn := func(controlapi.DaemonJSON) error { return errors.New("never healthy") }

	_, err := waitForHealthyDaemon(ctx, dir, 100, time.Millisecond, sleepFn, probeFn)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got: %v", err)
	}
	if sleeps > 3 {
		t.Errorf("sleeps=%d, want the loop to stop shortly after cancellation, not run out the full budget", sleeps)
	}
}

// TestProbeDaemonHTTP_RealServer is a light integration check that
// probeDaemonHTTP correctly distinguishes a healthy (200, correct token,
// valid engram status body) response from an unhealthy one, using a real
// httptest server (no daemon.json involved — probeDaemonHTTP takes
// port/token directly).
func TestProbeDaemonHTTP_RealServer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"daemon_version":"test"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	port := mustParsePort(t, ts.URL)

	if err := probeDaemonHTTP(port, "good-token"); err != nil {
		t.Errorf("probeDaemonHTTP with correct token: want nil, got %v", err)
	}
	if err := probeDaemonHTTP(port, "wrong-token"); err == nil {
		t.Error("probeDaemonHTTP with wrong token: want error, got nil")
	}
}

// TestProbeDaemonHTTP_RejectsNonEngramBody verifies that a 200 response
// alone is not trusted: if the port has been reused by an unrelated
// loopback service that happens to answer 200 with a body that isn't an
// engram controlapi.Status (missing/empty daemon_version), probeDaemonHTTP
// must return an error instead of treating it as a healthy engram daemon.
func TestProbeDaemonHTTP_RejectsNonEngramBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty JSON object", body: `{}`},
		{name: "unrelated HTML body", body: `<html><body>not engram</body></html>`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/status", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tt.body))
			})
			ts := httptest.NewServer(mux)
			defer ts.Close()

			port := mustParsePort(t, ts.URL)

			if err := probeDaemonHTTP(port, "any-token"); err == nil {
				t.Error("probeDaemonHTTP with 200 but non-engram body: want error, got nil")
			}
		})
	}
}
