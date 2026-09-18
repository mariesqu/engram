package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mariesqu/engram/internal/localstore"
	projectpkg "github.com/mariesqu/engram/internal/project"
)

// These tests cover mem_current_project, the session-bootstrap probe: agents are
// told to call it FIRST, so what it reports is what every later call is filed
// under. The contract has three halves — resolve like the read tools do, make a
// fallback visible instead of plausible, and NEVER error.

// currentProjectDaemon boots a daemon whose store the test can mutate (policy
// rows) before probing. Most tests only need callCurrentProject, which wraps it.
func currentProjectDaemon(t *testing.T) *daemonComponents {
	t.Helper()

	components, err := buildDaemon(daemonCfg{
		db:           filepath.Join(t.TempDir(), "current_project.db"),
		syncInterval: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)
	return components
}

// callCurrentProjectOn invokes the registered mem_current_project handler of an
// already-built daemon and decodes its JSON response. It fails the test on a
// transport error or a tool error: this tool is contractually incapable of
// either, non-string arguments included.
func callCurrentProjectOn(t *testing.T, components *daemonComponents, args map[string]any) map[string]any {
	t.Helper()

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

// callCurrentProject is callCurrentProjectOn against a throwaway daemon.
func callCurrentProject(t *testing.T, args map[string]any) map[string]any {
	t.Helper()
	return callCurrentProjectOn(t, currentProjectDaemon(t), args)
}

// hintsOf returns the advisory hints array as a single searchable string. The
// field is an ARRAY (one entry per independent reason the name is a guess);
// tests assert on the text of the reasons, not on their order.
func hintsOf(t *testing.T, env map[string]any) string {
	t.Helper()
	raw, ok := env["hints"]
	if !ok {
		return ""
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("hints = %#v, want a JSON array", raw)
	}
	parts := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("hints entry is %T, want a string: %#v", item, item)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n")
}

// ambiguousProjectDir builds a directory that detection CANNOT resolve: a
// parent of two git repos, itself shielded from git's upward walk by a
// deliberately malformed .git file in its parent (git exits non-zero on
// "invalid gitfile format", so detectGitRootDir/detectFromGitRemote return ""
// no matter where TMPDIR lives — including inside a checkout, which is what
// used to make this fixture skip itself and take the writes_blocked assertion
// with it).
func ambiguousProjectDir(t *testing.T) string {
	t.Helper()

	shield := filepath.Join(t.TempDir(), "shield")
	parent := filepath.Join(shield, "workspace")
	for _, name := range []string{"repo-a", "repo-b"} {
		if err := os.MkdirAll(filepath.Join(parent, name, ".git"), 0o755); err != nil {
			t.Fatalf("mkdir %s/.git: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(shield, ".git"), []byte("not a gitfile\n"), 0o644); err != nil {
		t.Fatalf("write shield .git: %v", err)
	}

	det := projectpkg.DetectProjectFull(parent)
	if det.Error == nil {
		t.Fatalf("fixture did not detect as ambiguous (source=%q, project=%q) — the shield stopped working",
			det.Source, det.Project)
	}
	return parent
}

// invalidConfigDir returns a directory holding an .engram/config.json that
// exists but cannot be used (empty project_name). Deterministic by
// construction: config detection runs BEFORE any git probing, so nothing about
// the host's checkout layout can change the outcome.
func invalidConfigDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".engram"), 0o755); err != nil {
		t.Fatalf("mkdir .engram: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".engram", "config.json"), []byte(`{"project_name": "   "}`), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
	return dir
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
	if env["directory_source"] != dirSourceArgument {
		t.Errorf("directory_source = %v, want %q", env["directory_source"], dirSourceArgument)
	}
	if env["directory_exists"] != true {
		t.Errorf("directory_exists = %v, want true", env["directory_exists"])
	}
	if env["fallback"] != false {
		t.Errorf("fallback = %v, want false: a pinned .engram/config.json is a declared identity", env["fallback"])
	}
	if env["writes_blocked"] != false {
		t.Errorf("writes_blocked = %v, want false", env["writes_blocked"])
	}
	if _, ok := env["hints"]; ok {
		t.Errorf("unexpected hints on a fully resolved project: %v", env["hints"])
	}
}

// TestCurrentProject_CwdAliasHonouredWhenDirectoryAbsent covers the alias the
// injected ODD protocol tells agents to use ("call mem_current_project with the
// workspace directory in the cwd parameter"). Without it that protocol resolves
// the daemon's own directory and reports a junk project with full confidence.
// The reported directory_source says the path came from the MODEL, which is
// weaker evidence than an injected one and must not read like it.
func TestCurrentProject_CwdAliasHonouredWhenDirectoryAbsent(t *testing.T) {
	chdirToJunkDir(t)
	clientDir := pinnedProjectDir(t, "cwd-alias-repo")

	env := callCurrentProject(t, map[string]any{"cwd": clientDir})

	if env["project"] != "cwd-alias-repo" {
		t.Errorf("project = %v, want %q", env["project"], "cwd-alias-repo")
	}
	if env["directory_source"] != dirSourceCwdAlias {
		t.Errorf("directory_source = %v, want %q", env["directory_source"], dirSourceCwdAlias)
	}
	if hints := hintsOf(t, env); !strings.Contains(hints, "\"cwd\" alias") {
		t.Errorf("hints = %q, want them to disclose that the directory came from the caller's alias", hints)
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
	if env["directory_source"] != dirSourceArgument {
		t.Errorf("directory_source = %v, want %q", env["directory_source"], dirSourceArgument)
	}
}

// TestCurrentProject_NonStringDirectoryNeverFallsThroughToCwd is the regression
// test for the gap between the bridge and the daemon. `engram connect` SKIPS
// injection when "directory" is present but not a string (hasNonEmptyStringArg
// — "the caller's error to see, not ours to paper over"), so the daemon is the
// only place left that can notice. Treating that key as absent would consult
// the model-supplied "cwd" and resolve the whole session from a path nobody
// verified, while the caller believes their own argument is in force.
func TestCurrentProject_NonStringDirectoryNeverFallsThroughToCwd(t *testing.T) {
	guessed := pinnedProjectDir(t, "guessed-repo")
	junkProject := chdirToJunkDir(t)

	env := callCurrentProject(t, map[string]any{"directory": 42, "cwd": guessed})

	if env["project"] == "guessed-repo" {
		t.Fatalf("project = %v: a non-string \"directory\" was downgraded to the \"cwd\" alias", env["project"])
	}
	if env["project"] != junkProject {
		t.Errorf("project = %v, want the daemon cwd basename %q", env["project"], junkProject)
	}
	if env["directory_source"] != dirSourceInvalid {
		t.Errorf("directory_source = %v, want %q", env["directory_source"], dirSourceInvalid)
	}
	if hint, _ := env["error_hint"].(string); !strings.Contains(hint, "must be a string") {
		t.Errorf("error_hint = %q, want it to name the type error", hint)
	}
	if hints := hintsOf(t, env); !strings.Contains(hints, "IGNORED") {
		t.Errorf("hints = %q, want them to say the argument was ignored", hints)
	}
}

// TestDirectoryAwareTools_NonStringDirectoryIsACallerError is the other half:
// mem_current_project reports the bad argument because it never errors, but
// every other directory-aware tool must REFUSE it. Silently resolving from the
// daemon's cwd (or from a model-supplied "cwd") would file the write under a
// project the caller never named.
func TestDirectoryAwareTools_NonStringDirectoryIsACallerError(t *testing.T) {
	components := currentProjectDaemon(t)
	guessed := pinnedProjectDir(t, "guessed-repo")

	// Minimal valid arguments per tool, plus the offending "directory".
	cases := map[string]map[string]any{
		"mem_save":            {"title": "t"},
		"mem_save_prompt":     {"content": "c"},
		"mem_search":          {"query": "q"},
		"mem_context":         {},
		"mem_review":          {"action": "list"},
		"mem_session_start":   {"id": "s1"},
		"mem_session_summary": {"content": "## Goal\nx"},
	}
	registered := components.mcpServer.ListTools()
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			tool, ok := registered[name]
			if !ok {
				t.Fatalf("%s is not registered", name)
			}
			call := map[string]any{"directory": []any{"not", "a", "string"}, "cwd": guessed}
			for k, v := range args {
				call[k] = v
			}
			result, err := tool.Handler(t.Context(), newToolRequest(name, call))
			if err != nil {
				t.Fatalf("handler transport error: %v", err)
			}
			if !result.IsError {
				t.Fatalf("%s accepted a non-string directory; want a tool error: %v", name, result.Content)
			}
			text, _ := result.Content[0].(mcp.TextContent)
			if !strings.Contains(text.Text, "directory must be a string") {
				t.Errorf("%s error = %q, want it to name the type error", name, text.Text)
			}
		})
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
	if hints := hintsOf(t, env); !strings.Contains(hints, "pass project explicitly") {
		t.Errorf("hints = %q, want them to tell the agent to pass project explicitly", hints)
	}
	// Writes are only blocked by ambiguity/invalid config/omitted policy; a plain
	// folder saves fine under its basename.
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
	if env["directory_source"] != dirSourceDaemonCwd {
		t.Errorf("directory_source = %v, want %q", env["directory_source"], dirSourceDaemonCwd)
	}
	if env["cwd_input"] != "" {
		t.Errorf("cwd_input = %v, want \"\": nothing was supplied by the caller", env["cwd_input"])
	}
	if env["fallback"] != true {
		t.Errorf("fallback = %v, want true", env["fallback"])
	}
	if hints := hintsOf(t, env); !strings.Contains(hints, "DAEMON's own working directory") {
		t.Errorf("hints = %q, want them to disclose that this is the daemon's directory", hints)
	}
}

// TestCurrentProject_AmbiguousDirectoryNeverErrors pins the never-errors
// contract on the input that makes write tools hard-error: a multi-repo parent.
// The probe answers with the lenient read resolution AND flags writes_blocked,
// so the agent learns about the ambiguity here instead of from a failed
// mem_save halfway through the session.
func TestCurrentProject_AmbiguousDirectoryNeverErrors(t *testing.T) {
	parent := ambiguousProjectDir(t)

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
	if hints := hintsOf(t, env); !strings.Contains(hints, "will REFUSE this directory") {
		t.Errorf("hints = %q, want them to state that writes are refused", hints)
	}
}

// TestCurrentProject_InvalidConfigKeepsConfigSource covers the OTHER
// writes_blocked input, and the field an agent acts on. A broken
// .engram/config.json is still a CONFIG answer: relabelling it "dir_basename"
// would send the agent looking for a config file that is missing, when the real
// one is sitting right there, malformed. The basename advice must stay out of
// this branch for the same reason.
func TestCurrentProject_InvalidConfigKeepsConfigSource(t *testing.T) {
	dir := invalidConfigDir(t)

	env := callCurrentProject(t, map[string]any{"directory": dir})

	// The lenient read answer for a broken config is the literal project
	// "unknown" — DetectProject returns it whenever detection produced no name,
	// and the invalid-config result carries an empty Project (unlike the
	// ambiguous one, which keeps the basename). Asserted so the probe stays
	// pinned to what mem_search/mem_context actually query.
	if env["project"] != "unknown" {
		t.Errorf("project = %v, want the lenient %q", env["project"], "unknown")
	}
	if env["project_source"] != projectpkg.SourceConfig {
		t.Errorf("project_source = %v, want %q — a malformed config is still a config answer",
			env["project_source"], projectpkg.SourceConfig)
	}
	if env["writes_blocked"] != true {
		t.Errorf("writes_blocked = %v, want true: every write tool hard-errors on an invalid .engram/config.json", env["writes_blocked"])
	}
	if env["fallback"] != true {
		t.Errorf("fallback = %v, want true: the reported name is a basename guess", env["fallback"])
	}
	if hint, _ := env["error_hint"].(string); !strings.Contains(hint, "invalid .engram/config.json") {
		t.Errorf("error_hint = %q, want it to name the invalid config", hint)
	}
	hints := hintsOf(t, env)
	if !strings.Contains(hints, "present but unusable") {
		t.Errorf("hints = %q, want them to point at the malformed config", hints)
	}
	if strings.Contains(hints, "no .engram/config.json") {
		t.Errorf("hints = %q, must not claim the config file is missing — it exists and is broken", hints)
	}
}

// TestCurrentProject_MissingDirectoryIsFlagged covers the invented-project case:
// detection derives a basename from any string, including a path that does not
// exist (a typo'd ENGRAM_CLIENT_DIR, a hallucinated "cwd"). Reporting that name
// like a normal answer is how a brand-new junk project gets created; the probe
// must say the directory is not there and block writes.
func TestCurrentProject_MissingDirectoryIsFlagged(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-checkout")

	env := callCurrentProject(t, map[string]any{"directory": missing})

	if env["directory_exists"] != false {
		t.Errorf("directory_exists = %v, want false", env["directory_exists"])
	}
	if env["project_source"] != "missing_directory" {
		t.Errorf("project_source = %v, want %q", env["project_source"], "missing_directory")
	}
	if env["fallback"] != true {
		t.Errorf("fallback = %v, want true", env["fallback"])
	}
	if env["writes_blocked"] != true {
		t.Errorf("writes_blocked = %v, want true", env["writes_blocked"])
	}
	if hints := hintsOf(t, env); !strings.Contains(hints, "does not exist") {
		t.Errorf("hints = %q, want them to say the directory does not exist", hints)
	}
}

// TestCurrentProject_OmittedPolicyBlocksWrites covers the one write refusal no
// filesystem evidence can reveal: a project whose sync policy is "omitted" is
// refused by mem_save/mem_save_prompt/mem_session_summary BEFORE anything is
// written (GetPolicy → "capture refused"). Detection is perfectly happy with
// that directory, so without the policy check the probe would report a
// confident, fully-resolved project that cannot accept a single save.
func TestCurrentProject_OmittedPolicyBlocksWrites(t *testing.T) {
	components := currentProjectDaemon(t)
	clientDir := pinnedProjectDir(t, "omitted-repo")
	if err := components.store.SetPolicy("omitted-repo", localstore.PolicyOmitted); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	env := callCurrentProjectOn(t, components, map[string]any{"directory": clientDir})

	if env["project"] != "omitted-repo" {
		t.Fatalf("project = %v, want %q", env["project"], "omitted-repo")
	}
	if env["writes_blocked"] != true {
		t.Errorf("writes_blocked = %v, want true for an omitted project", env["writes_blocked"])
	}
	if hints := hintsOf(t, env); !strings.Contains(hints, "omitted") {
		t.Errorf("hints = %q, want them to name the omitted policy", hints)
	}

	// The claim is only worth making if it is true — prove mem_save refuses.
	saveTool := components.mcpServer.ListTools()["mem_save"]
	result, err := saveTool.Handler(t.Context(), newToolRequest("mem_save", map[string]any{
		"title": "should be refused", "directory": clientDir,
	}))
	if err != nil {
		t.Fatalf("mem_save transport error: %v", err)
	}
	if !result.IsError {
		t.Error("mem_save accepted a write for an omitted project; writes_blocked would be a lie")
	}
}

// TestCurrentProject_OmittedPolicyBlocksExplicitProjectToo — an explicit project
// skips detection, not the policy. mem_save refuses an omitted project however
// its name was resolved, so the probe must too.
func TestCurrentProject_OmittedPolicyBlocksExplicitProjectToo(t *testing.T) {
	components := currentProjectDaemon(t)
	if err := components.store.SetPolicy("named-and-omitted", localstore.PolicyOmitted); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	env := callCurrentProjectOn(t, components, map[string]any{"project": "named-and-omitted"})

	if env["project_source"] != projectpkg.SourceExplicitOverride {
		t.Errorf("project_source = %v, want %q", env["project_source"], projectpkg.SourceExplicitOverride)
	}
	if env["writes_blocked"] != true {
		t.Errorf("writes_blocked = %v, want true: naming an omitted project does not make it writable", env["writes_blocked"])
	}
}

// TestCurrentProject_RelativeDirectoryResolvesAbsolute pins the canonicalization
// added to resolveProjectDir. A relative path is exactly what an agent filling
// in the "cwd" alias writes, and filepath.Base(".") is "." — which detection
// turns into the project "unknown". The probe must resolve it the way every
// other tool now does, report the absolute path in cwd, and keep the caller's
// verbatim value in cwd_input so the two can be compared.
func TestCurrentProject_RelativeDirectoryResolvesAbsolute(t *testing.T) {
	repo := pinnedProjectDir(t, "relative-repo")
	chdirTo(t, repo)

	env := callCurrentProject(t, map[string]any{"cwd": "."})

	if env["project"] != "relative-repo" {
		t.Errorf("project = %v, want %q — a relative directory resolved to a phantom project", env["project"], "relative-repo")
	}
	if env["cwd_input"] != "." {
		t.Errorf("cwd_input = %v, want %q verbatim", env["cwd_input"], ".")
	}
	got, _ := env["cwd"].(string)
	if !filepath.IsAbs(got) {
		t.Errorf("cwd = %q, want an absolute path", got)
	}
	if canonicalDir(t, got) != canonicalDir(t, repo) {
		t.Errorf("cwd = %q, want the resolved %q", got, repo)
	}
}

// TestCurrentProject_ResponseSchema pins the keys a consumer may rely on:
// the always-present fields, and the advisory ones that appear only when they
// have something to say. A field that comes and goes without reason is a field
// no agent can branch on.
func TestCurrentProject_ResponseSchema(t *testing.T) {
	clientDir := pinnedProjectDir(t, "schema-repo")

	env := callCurrentProject(t, map[string]any{"directory": clientDir})

	for _, key := range []string{
		"project", "project_source", "project_path", "cwd", "cwd_input",
		"directory_source", "directory_exists", "available_projects", "fallback", "writes_blocked",
	} {
		if _, ok := env[key]; !ok {
			t.Errorf("response is missing always-present key %q: %v", key, env)
		}
	}
	for _, key := range []string{"warning", "error_hint", "hints"} {
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
	if got, _ := env["cwd_input"].(string); got != clientDir {
		t.Errorf("cwd_input = %q, want the supplied directory %q verbatim", got, clientDir)
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
// on the same inputs. It goes through the registered HANDLER, not through
// currentProjectEnvelope, because the argument reading (the "cwd" alias, the
// non-string rejection) is half of the resolution and a test that skipped it
// would let the two halves drift apart silently.
func TestCurrentProject_MatchesReadToolResolution(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "agreement-folder")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pinned := pinnedProjectDir(t, "agreement-repo")
	chdirTo(t, pinnedProjectDir(t, "daemon-cwd-repo"))

	for _, tc := range []struct {
		name string
		args map[string]any
		// directory is what resolveReadProject must be given to answer the same
		// question: the value the handler ends up resolving from.
		project, directory string
	}{
		{name: "forwarded directory", args: map[string]any{"directory": pinned}, directory: pinned},
		{name: "cwd alias", args: map[string]any{"cwd": pinned}, directory: pinned},
		{name: "bare folder", args: map[string]any{"directory": bare}, directory: bare},
		{name: "empty directory", args: map[string]any{"directory": "  "}, directory: ""},
		{name: "no directory at all", args: map[string]any{}, directory: ""},
		{name: "ambiguous", args: map[string]any{"directory": ambiguousProjectDir(t)}},
		{name: "invalid config", args: map[string]any{"directory": invalidConfigDir(t)}},
		{name: "explicit project", args: map[string]any{"project": "explicitly-named", "directory": bare},
			project: "explicitly-named", directory: bare},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := tc.directory
			if directory == "" {
				if d, ok := tc.args["directory"].(string); ok {
					directory = d
				}
			}
			env := callCurrentProject(t, tc.args)
			want := resolveReadProject(tc.project, directory)
			if env["project"] != want {
				t.Errorf("mem_current_project%v project = %v, but the read tools resolve %q",
					tc.args, env["project"], want)
			}
		})
	}
}
