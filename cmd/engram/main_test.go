package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// TestRun_NoArgs verifies that calling run with no arguments returns exit code 2
// (usage error) without panicking.
func TestRun_NoArgs(t *testing.T) {
	code := run([]string{})
	if code != 2 {
		t.Errorf("run([]): got exit code %d, want 2", code)
	}
}

// TestEnvOr_TrimsWhitespace verifies envOr strips surrounding whitespace (env vars
// from files/CI often carry a trailing newline) and treats a whitespace-only value
// as unset, falling back to the default.
func TestEnvOr_TrimsWhitespace(t *testing.T) {
	t.Setenv("ENGRAM_TEST_TRIM", "  value\n")
	if got := envOr("ENGRAM_TEST_TRIM", "def"); got != "value" {
		t.Errorf("envOr = %q, want %q (trimmed)", got, "value")
	}
	t.Setenv("ENGRAM_TEST_TRIM", " \n\t")
	if got := envOr("ENGRAM_TEST_TRIM", "def"); got != "def" {
		t.Errorf("envOr whitespace-only = %q, want default %q", got, "def")
	}
}

// TestRun_HelpFlag verifies that -h, --help, and "help" all return exit code 2.
func TestRun_HelpFlag(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		code := run([]string{arg})
		if code != 2 {
			t.Errorf("run([%q]): got exit code %d, want 2", arg, code)
		}
	}
}

// TestRun_UnknownSubcommand verifies that an unknown subcommand returns exit
// code 2 (usage), not 1 (runtime error) or 0.
func TestRun_UnknownSubcommand(t *testing.T) {
	code := run([]string{"bogus"})
	if code != 2 {
		t.Errorf("run([bogus]): got exit code %d, want 2", code)
	}
}

// TestRun_ServeMissingDSN verifies that 'serve' with no DSN and no ENGRAM_DSN
// env returns exit code 1 (the "dsn required" validation error).
func TestRun_ServeMissingDSN(t *testing.T) {
	t.Setenv("ENGRAM_DSN", "") // ensure env is unset for this test
	code := run([]string{"serve"})
	if code != 1 {
		t.Errorf("run([serve]) with no DSN: got exit code %d, want 1", code)
	}
}

// TestRun_KeysProvisionMissingDSN verifies that 'keys provision' with no DSN
// returns exit code 1.
func TestRun_KeysProvisionMissingDSN(t *testing.T) {
	t.Setenv("ENGRAM_DSN", "")
	code := run([]string{"keys", "provision", "writer-x"})
	if code != 1 {
		t.Errorf("run([keys provision writer-x]) with no DSN: got exit code %d, want 1", code)
	}
}

// TestRun_KeysRevokeMissingDSN verifies that 'keys revoke' with no DSN returns
// exit code 1.
func TestRun_KeysRevokeMissingDSN(t *testing.T) {
	t.Setenv("ENGRAM_DSN", "")
	code := run([]string{"keys", "revoke", "writer-x"})
	if code != 1 {
		t.Errorf("run([keys revoke writer-x]) with no DSN: got exit code %d, want 1", code)
	}
}

// TestRun_KeysProvisionMissingWriterID verifies that 'keys provision' with a
// DSN but no writer-id returns exit code 1.
func TestRun_KeysProvisionMissingWriterID(t *testing.T) {
	t.Setenv("ENGRAM_DSN", "")
	code := run([]string{"keys", "provision", "--dsn", "postgres://fake/db"})
	if code != 1 {
		t.Errorf("run([keys provision --dsn ...]): got exit code %d, want 1", code)
	}
}

// TestRun_KeysRevokeMissingWriterID verifies that 'keys revoke' with a DSN
// but no writer-id returns exit code 1.
func TestRun_KeysRevokeMissingWriterID(t *testing.T) {
	t.Setenv("ENGRAM_DSN", "")
	code := run([]string{"keys", "revoke", "--dsn", "postgres://fake/db"})
	if code != 1 {
		t.Errorf("run([keys revoke --dsn ...]): got exit code %d, want 1", code)
	}
}

// captureStderr runs f with os.Stderr redirected to a pipe and returns what was
// written. Tests are sequential, so swapping the global os.Stderr is safe here.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()

	f()

	_ = w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	_ = r.Close()
	return buf.String()
}

// TestRun_KeysProvisionHelp_DoesNotLeakDSN proves `keys provision --help` never
// prints the ENGRAM_DSN secret (a Postgres DSN carries DB credentials). Regression
// guard for the credential-leak: --dsn must default to "" with ENGRAM_DSN resolved
// AFTER Parse, so PrintDefaults has no secret default value to print.
func TestRun_KeysProvisionHelp_DoesNotLeakDSN(t *testing.T) {
	const secret = "postgres://user:topsecret@db.internal:5432/engram"
	t.Setenv("ENGRAM_DSN", secret)

	out := captureStderr(t, func() {
		if code := run([]string{"keys", "provision", "--help"}); code != 0 {
			t.Errorf("keys provision --help: exit code %d, want 0", code)
		}
	})

	if strings.Contains(out, "topsecret") || strings.Contains(out, secret) {
		t.Errorf("keys provision --help leaked the ENGRAM_DSN secret:\n%s", out)
	}
	// Sanity: the dsn flag is still listed (the point of PrintDefaults).
	if !strings.Contains(out, "dsn") {
		t.Errorf("keys provision --help should still list the dsn flag; got:\n%s", out)
	}
}

// TestRun_KeysRevokeHelp_DoesNotLeakDSN is the revoke counterpart of the provision
// no-leak guard: `keys revoke --help` uses the same --dsn + PrintDefaults pattern,
// so it must equally never print the ENGRAM_DSN secret.
func TestRun_KeysRevokeHelp_DoesNotLeakDSN(t *testing.T) {
	const secret = "postgres://user:topsecret@db.internal:5432/engram"
	t.Setenv("ENGRAM_DSN", secret)

	out := captureStderr(t, func() {
		if code := run([]string{"keys", "revoke", "--help"}); code != 0 {
			t.Errorf("keys revoke --help: exit code %d, want 0", code)
		}
	})

	if strings.Contains(out, "topsecret") || strings.Contains(out, secret) {
		t.Errorf("keys revoke --help leaked the ENGRAM_DSN secret:\n%s", out)
	}
	// Sanity: the dsn flag is still listed (the point of PrintDefaults).
	if !strings.Contains(out, "dsn") {
		t.Errorf("keys revoke --help should still list the dsn flag; got:\n%s", out)
	}
}

// TestRun_KeysUnknownSubcommand verifies that 'keys <unknown>' returns exit
// code 1 (the dispatch returns an error, not usage).
func TestRun_KeysUnknownSubcommand(t *testing.T) {
	code := run([]string{"keys", "frobnicate"})
	if code != 1 {
		t.Errorf("run([keys frobnicate]): got exit code %d, want 1", code)
	}
}

// TestRun_ServeExtraPositional verifies that 'serve' with an unexpected positional
// argument returns exit code 1 (rejected by the NArg check before opening the store).
func TestRun_ServeExtraPositional(t *testing.T) {
	t.Setenv("ENGRAM_DSN", "")
	code := run([]string{"serve", "--dsn", "postgres://fake/db", "unexpected"})
	if code != 1 {
		t.Errorf("run([serve ... unexpected]): got exit code %d, want 1", code)
	}
}

// TestRun_KeysProvisionExtraPositional verifies that 'keys provision' with more than
// one positional (two writer-ids) returns exit code 1 rather than silently
// provisioning the first — guards against operator typos.
func TestRun_KeysProvisionExtraPositional(t *testing.T) {
	t.Setenv("ENGRAM_DSN", "")
	code := run([]string{"keys", "provision", "--dsn", "postgres://fake/db", "writer-a", "writer-b"})
	if code != 1 {
		t.Errorf("run([keys provision a b]): got exit code %d, want 1", code)
	}
}

// TestEnvOr_EnvSet verifies that envOr returns the env value when set.
func TestEnvOr_EnvSet(t *testing.T) {
	t.Setenv("ENGRAM_TEST_VAR", "from-env")
	got := envOr("ENGRAM_TEST_VAR", "default")
	if got != "from-env" {
		t.Errorf("envOr with env set: got %q, want %q", got, "from-env")
	}
}

// TestEnvOr_EnvUnset verifies that envOr returns the default when the env var
// is unset or empty.
func TestEnvOr_EnvUnset(t *testing.T) {
	t.Setenv("ENGRAM_TEST_VAR", "")
	got := envOr("ENGRAM_TEST_VAR", "default-val")
	if got != "default-val" {
		t.Errorf("envOr with empty env: got %q, want %q", got, "default-val")
	}
}
