package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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
	cmd := buildSpawnCmd("engram", "/tmp/test.db")

	got := strings.Join(cmd.Args, " ")
	for _, want := range []string{"daemon", "--db", "/tmp/test.db", "--http", "--transport", "http"} {
		if !strings.Contains(got, want) {
			t.Errorf("spawn command args %v missing %q", cmd.Args, want)
		}
	}
	// --http and --transport http must both be present (not just one).
	if !strings.Contains(got, "--http --transport http") {
		t.Errorf("spawn command args %v: want \"--http --transport http\" together, got %q", cmd.Args, got)
	}
}

// TestBuildSpawnCmd_DetachedAndNoStdio verifies the spawned command carries
// platform-specific detach attributes (SysProcAttr, non-nil on every
// platform we build for — see spawn_windows.go / spawn_other.go) and does
// not inherit the parent's stdio, so it survives the parent exiting.
func TestBuildSpawnCmd_DetachedAndNoStdio(t *testing.T) {
	cmd := buildSpawnCmd("engram", "/tmp/test.db")

	if cmd.SysProcAttr == nil {
		t.Error("SysProcAttr is nil; spawned daemon would not be detached from the parent")
	}
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		t.Error("spawned daemon must not inherit the parent's stdio handles")
	}
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
// probeDaemonHTTP correctly distinguishes a healthy (200, correct token)
// response from an unhealthy one, using a real httptest server (no
// daemon.json involved — probeDaemonHTTP takes port/token directly).
func TestProbeDaemonHTTP_RealServer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
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
