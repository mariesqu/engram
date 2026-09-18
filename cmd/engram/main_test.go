package main

import (
	"bytes"
	"io"
	"os"
	"regexp"
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

// captureStdout runs f with os.Stdout redirected to a pipe and returns what
// was written. Tests are sequential so swapping the global os.Stdout is safe.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	f()

	_ = w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	_ = r.Close()
	return buf.String()
}

// TestRun_Version_ExitZero verifies that 'engram version' returns exit code 0.
func TestRun_Version_ExitZero(t *testing.T) {
	code := run([]string{"version"})
	if code != 0 {
		t.Errorf("run([version]): got exit code %d, want 0", code)
	}
}

// TestRun_Version_Output verifies that 'engram version' prints exactly two
// whitespace-separated tokens — "engram" and the version — on a single line,
// and NOTHING else. The version var defaults to "dev" in test builds (no
// ldflags injection), so we assert the shape rather than a literal value; the
// semver-specific contract is pinned by TestRun_Version_BareSemverLine below.
func TestRun_Version_Output(t *testing.T) {
	out := captureStdout(t, func() {
		if code := run([]string{"version"}); code != 0 {
			t.Errorf("run([version]): exit code %d, want 0", code)
		}
	})

	// Must contain the word "engram".
	if !strings.Contains(out, "engram") {
		t.Errorf("version output missing 'engram': %q", out)
	}
	// Exactly two fields: "engram" and a non-empty version token. Anything more
	// (the GOOS/GOARCH pair and Go runtime version that used to live here) would
	// break the anchored version probe — see runVersionCmd.
	parts := strings.Fields(out)
	if len(parts) != 2 {
		t.Fatalf("version output has %d fields, want exactly 2 (\"engram <version>\"): %q", len(parts), out)
	}
	if parts[0] != "engram" {
		t.Errorf("first field = %q, want %q: %q", parts[0], "engram", out)
	}
	if parts[1] == "" {
		t.Errorf("version token is empty: %q", out)
	}
	// One line only — a second line fails the probe's end-of-TEXT anchor.
	if lines := strings.Split(strings.TrimRight(out, "\n"), "\n"); len(lines) != 1 {
		t.Errorf("version output spans %d lines, want 1: %q", len(lines), out)
	}
}

// TestRun_Version_BareSemverLine pins the compatibility contract itself: with a
// release-shaped version injected, the WHOLE trimmed stdout of `engram version`
// matches the anchored regexp integrators probe it with (gentle-ai's
// engramVersionPattern in internal/components/engram/protocol.go, applied to
// the trimmed full output — not just its first line).
// NOTE: MUTATES the package-level version var; see TestRun_Version_InjectedValue.
func TestRun_Version_BareSemverLine(t *testing.T) {
	original := version
	version = "v1.5.5"
	t.Cleanup(func() { version = original })

	probe := regexp.MustCompile(`^(?:engram\s+)?v?(\d+)\.(\d+)\.(\d+)$`)

	out := captureStdout(t, func() {
		if code := run([]string{"version"}); code != 0 {
			t.Errorf("run([version]): exit code %d, want 0", code)
		}
	})

	if got := strings.TrimSpace(out); got != "engram v1.5.5" {
		t.Errorf("version output = %q, want %q", got, "engram v1.5.5")
	}
	if !probe.MatchString(strings.TrimSpace(out)) {
		t.Errorf("version output %q does not match the integrator probe %s", out, probe)
	}
}

// TestRun_Version_Verbose verifies that the build details removed from the bare
// line are still reachable — on a SECOND line, behind --verbose — so nothing was
// lost, only moved out of the probe's way.
func TestRun_Version_Verbose(t *testing.T) {
	for _, flag := range []string{"--verbose", "-v"} {
		t.Run(flag, func(t *testing.T) {
			out := captureStdout(t, func() {
				if code := run([]string{"version", flag}); code != 0 {
					t.Errorf("run([version %s]): exit code %d, want 0", flag, code)
				}
			})

			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			if len(lines) != 2 {
				t.Fatalf("verbose output spans %d lines, want 2: %q", len(lines), out)
			}
			if fields := strings.Fields(lines[0]); len(fields) != 2 || fields[0] != "engram" {
				t.Errorf("verbose first line = %q, want the bare \"engram <version>\" line", lines[0])
			}
			// Second line: GOOS/GOARCH pair plus the "go" runtime prefix.
			if !strings.Contains(lines[1], "/") {
				t.Errorf("verbose output missing GOOS/GOARCH pair: %q", out)
			}
			if !strings.Contains(lines[1], "go") {
				t.Errorf("verbose output missing Go runtime version: %q", out)
			}
		})
	}
}

// TestRun_Version_InjectedValue verifies that the ldflags injection contract
// works: assigning version before calling runVersionCmd results in that value
// appearing in the output. This proves the -X main.version=... injection path
// without requiring a full cross-compiled binary.
// NOTE: this test MUTATES the package-level version var (restored via
// t.Cleanup). Do NOT add t.Parallel() to tests in this package — the CI race
// lane (-race on ubuntu) will catch a violation, but better not to write one.
func TestRun_Version_InjectedValue(t *testing.T) {
	original := version
	version = "v1.2.3-test"
	t.Cleanup(func() { version = original })

	out := captureStdout(t, func() {
		if err := runVersionCmd(nil); err != nil {
			t.Fatalf("runVersionCmd: %v", err)
		}
	})

	if !strings.Contains(out, "v1.2.3-test") {
		t.Errorf("version output does not contain injected version %q: %q", "v1.2.3-test", out)
	}
}
