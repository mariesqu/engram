package main

import (
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

// TestRun_KeysUnknownSubcommand verifies that 'keys <unknown>' returns exit
// code 1 (the dispatch returns an error, not usage).
func TestRun_KeysUnknownSubcommand(t *testing.T) {
	code := run([]string{"keys", "frobnicate"})
	if code != 1 {
		t.Errorf("run([keys frobnicate]): got exit code %d, want 1", code)
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
