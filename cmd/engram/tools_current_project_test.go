package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	projectpkg "github.com/mariesqu/engram/internal/project"
)

// These tests cover mem_current_project, the session-bootstrap probe: agents are
// told to call it FIRST, so what it reports is what every later call is filed
// under. The contract has three halves — resolve like the read tools do, make a
// fallback visible instead of plausible, and NEVER error.

// callCurrentProject invokes the registered mem_current_project handler and
// decodes its JSON response. It fails the test on a transport error or a tool
// error: this tool is contractually incapable of either.
func callCurrentProject(t *testing.T, args map[string]any) map[string]any {
	t.Helper()

	components, err := buildDaemon(daemonCfg{
		db:           filepath.Join(t.TempDir(), "current_project.db"),
		syncInterval: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	tool, ok := components.mcpServer.ListTools()["mem_current_project"]
	if !ok {
		t.Fatal("mem_current_project is not registered")
	}
	result, err := tool.Handler(t.Context(), newToolRequest("mem_current_project", args))
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("mem_current_project must never return a tool error; got: %v", result.Content)
	}
	if len(result.Content) == 0 {
		t.Fatal("mem_current_project returned no content")
	}
	text, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want mcp.TextContent", result.Content[0])
	}

	var env map[string]any
	if err := json.Unmarshal([]byte(text.Text), &env); err != nil {
		t.Fatalf("response is not JSON (%v): %s", err, text.Text)
	}
	return env
}

// TestCurrentProject_ExplicitDirectory pins the normal `engram connect` path:
// the forwarded client directory is what gets detected, NOT the daemon's cwd.
// The daemon is chdir'd into the junk directory from the field incident, so a
// handler that ignored "directory" would answer "system32".
func TestCurrentProject_ExplicitDirectory(t *testing.T) {
	chdirToJunkDir(t)
	clientDir := pinnedProjectDir(t, "forwarded-repo")

	env := callCurrentProject(t, map[string]any{"directory": clientDir})

	if env["project"] != "forwarded-repo" {
		t.Errorf("project = %v, want %q (the forwarded directory was ignored)", env["project"], "forwarded-repo")
	}
	if env["project_source"] != projectpkg.SourceConfig {
		t.Errorf("project_source = %v, want %q", env["project_source"], projectpkg.SourceConfig)
	}
	if env["directory_source"] != "argument" {
		t.Errorf("directory_source = %v, want %q", env["directory_source"], "argument")
	}
	if env["fallback"] != false {
		t.Errorf("fallback = %v, want false: a pinned .engram/config.json is a declared identity", env["fallback"])
	}
	if env["writes_blocked"] != false {
		t.Errorf("writes_blocked = %v, want false", env["writes_blocked"])
	}
	if _, ok := env["hint"]; ok {
		t.Errorf("unexpected hint on a fully resolved project: %v", env["hint"])
	}
}

// TestCurrentProject_CwdAliasHonouredWhenDirectoryAbsent covers the alias the
// injected ODD protocol tells agents to use ("call mem_current_project with the
// workspace directory in the cwd parameter"). Without it that protocol resolves
// the daemon's own directory and reports a junk project with full confidence.
func TestCurrentProject_CwdAliasHonouredWhenDirectoryAbsent(t *testing.T) {
	chdirToJunkDir(t)
	clientDir := pinnedProjectDir(t, "cwd-alias-repo")

	env := callCurrentProject(t, map[string]any{"cwd": clientDir})

	if env["project"] != "cwd-alias-repo" {
		t.Errorf("project = %v, want %q", env["project"], "cwd-alias-repo")
	}
	if env["directory_source"] != "argument" {
		t.Errorf("directory_source = %v, want %q", env["directory_source"], "argument")
	}
}

// TestCurrentProject_DirectoryBeatsCwdAlias pins the precedence between the two.
// `engram connect` injects THIS caller's real working directory into
// "directory"; a model-supplied "cwd" is a guess about the same thing, so the
// injected value must win — otherwise a hallucinated path silently re-points the
// whole session.
func TestCurrentProject_DirectoryBeatsCwdAlias(t *testing.T) {
	injected := pinnedProjectDir(t, "injected-repo")
	guessed := pinnedProjectDir(t, "guessed-repo")

	env := callCurrentProject(t, map[string]any{"directory": injected, "cwd": guessed})

	if env["project"] != "injected-repo" {
		t.Errorf("project = %v, want %q — \"cwd\" outranked the injected \"directory\"", env["project"], "injected-repo")
	}
}

// TestCurrentProject_ExplicitProjectWins mirrors the precedence every other
// tool follows: an explicit project is echoed back verbatim, no detection runs,
// and the source says so. The daemon sits in the junk directory to prove the
// detection really was skipped rather than merely agreeing.
func TestCurrentProject_ExplicitProjectWins(t *testing.T) {
	chdirToJunkDir(t)

	env := callCurrentProject(t, map[string]any{"project": "  named-by-the-agent  "})

	if env["project"] != "named-by-the-agent" {
		t.Errorf("project = %v, want %q (trimmed)", env["project"], "named-by-the-agent")
	}
	if env["project_source"] != projectpkg.SourceExplicitOverride {
		t.Errorf("project_source = %v, want %q", env["project_source"], projectpkg.SourceExplicitOverride)
	}
	if env["fallback"] != false {
		t.Errorf("fallback = %v, want false: the caller named the project", env["fallback"])
	}
	if env["writes_blocked"] != false {
		t.Errorf("writes_blocked = %v, want false", env["writes_blocked"])
	}
}

// TestCurrentProject_BasenameFallbackIsFlagged is the reason this tool exists.
// A bare directory declares nothing, so the project name is just its basename —
// plausible, unstable, and indistinguishable from a real one in every other
// tool's output. Here it must be labelled.
func TestCurrentProject_BasenameFallbackIsFlagged(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "bare-folder")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	env := callCurrentProject(t, map[string]any{"directory": bare})

	if env["project"] != "bare-folder" {
		t.Errorf("project = %v, want %q", env["project"], "bare-folder")
	}
	if env["project_source"] != projectpkg.SourceDirBasename {
		t.Errorf("project_source = %v, want %q", env["project_source"], projectpkg.SourceDirBasename)
	}
	if env["fallback"] != true {
		t.Errorf("fallback = %v, want true: a basename is a guess, and the response must say so", env["fallback"])
	}
	hint, _ := env["hint"].(string)
	if !strings.Contains(hint, "pass project explicitly") {
		t.Errorf("hint = %q, want it to tell the agent to pass project explicitly", hint)
	}
	// Writes are only blocked by ambiguity/invalid config; a plain folder saves
	// fine under its basename.
	if env["writes_blocked"] != false {
		t.Errorf("writes_blocked = %v, want false", env["writes_blocked"])
	}
}

// TestCurrentProject_DaemonCwdFallbackIsFlagged covers the other fallback: no
// directory reached the daemon at all (an old `engram connect`, ENGRAM_CLIENT_DIR=none,
// a host that spawns servers in $HOME). The answer then describes the SHARED
// daemon's own directory, which is exactly how every repo's memories once ended
// up in one "system32" project — so the response has to disclose it.
func TestCurrentProject_DaemonCwdFallbackIsFlagged(t *testing.T) {
	chdirToJunkDir(t)

	env := callCurrentProject(t, map[string]any{})

	if env["project"] != "system32" {
		t.Errorf("project = %v, want %q (the daemon's cwd basename)", env["project"], "system32")
	}
	if env["directory_source"] != "daemon_cwd" {
		t.Errorf("directory_source = %v, want %q", env["directory_source"], "daemon_cwd")
	}
	if env["fallback"] != true {
		t.Errorf("fallback = %v, want true", env["fallback"])
	}
	hint, _ := env["hint"].(string)
	if !strings.Contains(hint, "DAEMON's own working directory") {
		t.Errorf("hint = %q, want it to disclose that this is the daemon's directory", hint)
	}
}

// TestCurrentProject_AmbiguousDirectoryNeverErrors pins the never-errors
// contract on the input that makes write tools hard-error: a multi-repo parent.
// The probe answers with the lenient read resolution AND flags writes_blocked,
// so the agent learns about the ambiguity here instead of from a failed
// mem_save halfway through the session.
func TestCurrentProject_AmbiguousDirectoryNeverErrors(t *testing.T) {
	parent := t.TempDir()
	for _, name := range []string{"repo-a", "repo-b"} {
		if err := os.MkdirAll(filepath.Join(parent, name, ".git"), 0o755); err != nil {
			t.Fatalf("mkdir %s/.git: %v", name, err)
		}
	}
	if det := projectpkg.DetectProjectFull(parent); det.Error == nil {
		t.Skipf("directory did not detect as ambiguous (source=%q); nothing to assert", det.Source)
	}

	env := callCurrentProject(t, map[string]any{"directory": parent})

	if env["project"] != filepath.Base(parent) {
		t.Errorf("project = %v, want the lenient basename %q", env["project"], filepath.Base(parent))
	}
	if env["writes_blocked"] != true {
		t.Errorf("writes_blocked = %v, want true: mem_save hard-errors on an ambiguous directory", env["writes_blocked"])
	}
	if env["fallback"] != true {
		t.Errorf("fallback = %v, want true", env["fallback"])
	}
	if hint, _ := env["error_hint"].(string); !strings.Contains(hint, "ambiguous") {
		t.Errorf("error_hint = %q, want it to name the ambiguity", hint)
	}
	available, ok := env["available_projects"].([]any)
	if !ok || len(available) != 2 {
		t.Errorf("available_projects = %v, want the two candidate repos", env["available_projects"])
	}
}

// TestCurrentProject_ResponseSchema pins the keys a consumer may rely on:
// the seven always-present fields, and the three advisory ones that appear only
// when they have something to say. A field that comes and goes without reason
// is a field no agent can branch on.
func TestCurrentProject_ResponseSchema(t *testing.T) {
	clientDir := pinnedProjectDir(t, "schema-repo")

	env := callCurrentProject(t, map[string]any{"directory": clientDir})

	for _, key := range []string{
		"project", "project_source", "project_path", "cwd",
		"directory_source", "available_projects", "fallback", "writes_blocked",
	} {
		if _, ok := env[key]; !ok {
			t.Errorf("response is missing always-present key %q: %v", key, env)
		}
	}
	for _, key := range []string{"warning", "error_hint", "hint"} {
		if _, ok := env[key]; ok {
			t.Errorf("advisory key %q must be omitted when there is nothing to say: %v", key, env[key])
		}
	}
	if _, ok := env["available_projects"].([]any); !ok {
		t.Errorf("available_projects = %#v, want a JSON array (never null)", env["available_projects"])
	}
	// Compared canonically: detection resolves the directory, so on Windows the
	// reported path is the long form of a t.TempDir() that may arrive in 8.3
	// short form (TMP=C:\Users\MTL~1.MES\...).
	gotPath, _ := env["project_path"].(string)
	if canonicalDir(t, gotPath) != canonicalDir(t, clientDir) {
		t.Errorf("project_path = %q, want %q", gotPath, clientDir)
	}
	if got, _ := env["cwd"].(string); got != clientDir {
		t.Errorf("cwd = %q, want the resolved directory %q verbatim", got, clientDir)
	}
}

// canonicalDir resolves dir to its canonical form for comparison, falling back
// to the input when it cannot be resolved.
func canonicalDir(t *testing.T, dir string) string {
	t.Helper()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

// TestCurrentProject_MatchesReadToolResolution is the anti-drift guard: the
// probe promises to report what the READ tools resolve, so the two must agree
// on the same inputs. If resolveReadProject ever changes, this fails rather
// than letting mem_current_project quietly describe a resolution nobody uses.
func TestCurrentProject_MatchesReadToolResolution(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "agreement-folder")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pinned := pinnedProjectDir(t, "agreement-repo")

	for _, tc := range []struct{ project, directory string }{
		{"", bare},
		{"", pinned},
		{"explicitly-named", bare},
	} {
		env := currentProjectEnvelope(tc.project, tc.directory)
		want := resolveReadProject(tc.project, tc.directory)
		if env["project"] != want {
			t.Errorf("currentProjectEnvelope(%q, %q) project = %v, but the read tools resolve %q",
				tc.project, tc.directory, env["project"], want)
		}
	}
}
