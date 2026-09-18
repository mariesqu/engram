package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// These tests cover `engram setup hooks`. The merge writes into a file that
// belongs to the USER — their other hooks, their permissions, keys this binary
// has never heard of — so the tests are mostly about what the merge must NOT
// do.

// TestSetupHooks_MergePreservesUnknownKeys is the first rule: a settings file
// is not ours to normalize. Every key outside "hooks", and every hook that is
// not engram's, must come back byte-identical in meaning.
func TestSetupHooks_MergePreservesUnknownKeys(t *testing.T) {
	existing := []byte(`{
  "model": "opus",
  "permissions": {"allow": ["Bash(git status)"]},
  "somethingEngramHasNeverHeardOf": {"nested": [1, 2, 3]},
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [{"type": "command", "command": "my-own-script.sh", "timeout": 3}]}
    ]
  }
}`)

	merged, added, err := mergeHookSettings(existing, engramHookPack("claude-code"))
	if err != nil {
		t.Fatalf("mergeHookSettings: %v", err)
	}
	if added == 0 {
		t.Fatal("no hooks were added to a file that had none of ours")
	}

	var doc map[string]any
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatalf("merged settings are not valid JSON: %v\n%s", err, merged)
	}
	if doc["model"] != "opus" {
		t.Errorf("model = %v, want it preserved", doc["model"])
	}
	if _, ok := doc["somethingEngramHasNeverHeardOf"]; !ok {
		t.Error("an unknown top-level key was dropped — that is someone's configuration")
	}
	if !strings.Contains(string(merged), "my-own-script.sh") {
		t.Error("the user's own SessionStart hook was dropped")
	}
	if !strings.Contains(string(merged), "engram hook session-start") {
		t.Error("engram's session-start hook was not installed")
	}
	// 2-space indent: what the agent hosts write, so the next rewrite by the host
	// is not a whole-file diff.
	if !strings.Contains(string(merged), "\n  \"hooks\"") && !strings.Contains(string(merged), "\n  \"model\"") {
		t.Errorf("merged settings are not 2-space indented:\n%s", merged)
	}
}

// TestSetupHooks_MergeIsIdempotent — running setup twice must change nothing
// the second time. Not "produce equivalent output": produce the same bytes and
// report zero additions, which is what makes the command safe to put in a
// README, a bootstrap script, or a loop.
func TestSetupHooks_MergeIsIdempotent(t *testing.T) {
	for _, agent := range []string{"claude-code", "codex"} {
		t.Run(agent, func(t *testing.T) {
			first, added, err := mergeHookSettings(nil, engramHookPack(agent))
			if err != nil {
				t.Fatalf("first merge: %v", err)
			}
			if added != 5 {
				t.Errorf("first merge added %d hooks, want 5", added)
			}

			second, added, err := mergeHookSettings(first, engramHookPack(agent))
			if err != nil {
				t.Fatalf("second merge: %v", err)
			}
			if added != 0 {
				t.Errorf("second merge added %d hooks, want 0 — setup is not idempotent", added)
			}
			if string(second) != string(first) {
				t.Errorf("second merge rewrote the file:\n--- first ---\n%s\n--- second ---\n%s", first, second)
			}
		})
	}
}

// TestSetupHooks_DedupIsByCommandOnly pins the dedup rule and the reason for
// it: once installed, the timeout, matcher and status message belong to the
// user. A second run must not "restore" them, and must not append a duplicate
// hook that would run the same command twice per event.
func TestSetupHooks_DedupIsByCommandOnly(t *testing.T) {
	existing := []byte(`{
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [{"type": "command", "command": "engram hook session-start", "timeout": 42}]}
    ]
  }
}`)

	merged, added, err := mergeHookSettings(existing, engramHookPack("claude-code"))
	if err != nil {
		t.Fatalf("mergeHookSettings: %v", err)
	}
	if added != 4 {
		t.Errorf("added = %d, want 4 (session-start was already present)", added)
	}
	if !strings.Contains(string(merged), `"timeout": 42`) {
		t.Errorf("the user's edited timeout was overwritten:\n%s", merged)
	}
	if strings.Count(string(merged), "engram hook session-start") != 1 {
		t.Errorf("session-start was installed twice:\n%s", merged)
	}
}

// TestSetupHooks_RefusesInvalidJSON — a settings file that does not parse is a
// file whose contents we cannot preserve. Overwriting it with a fresh document
// would delete everything in it, so the merge stops and says so.
func TestSetupHooks_RefusesInvalidJSON(t *testing.T) {
	_, _, err := mergeHookSettings([]byte("{ this is not json"), engramHookPack("claude-code"))
	if err == nil {
		t.Fatal("merging into an unparseable settings file must fail rather than overwrite it")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("error = %v, want it to explain why nothing was written", err)
	}
}

// TestSetupHooks_EmptyAndNullHooksAreAccepted covers the two shapes a settings
// file carries when hooks were never configured.
func TestSetupHooks_EmptyAndNullHooksAreAccepted(t *testing.T) {
	for name, existing := range map[string]string{
		"absent file": "",
		"empty doc":   `{}`,
		"null hooks":  `{"hooks": null}`,
		"empty hooks": `{"hooks": {}}`,
	} {
		t.Run(name, func(t *testing.T) {
			merged, added, err := mergeHookSettings([]byte(existing), engramHookPack("claude-code"))
			if err != nil {
				t.Fatalf("mergeHookSettings: %v", err)
			}
			if added != 5 {
				t.Errorf("added = %d, want 5", added)
			}
			if !strings.Contains(string(merged), "engram hook user-prompt-submit") {
				t.Errorf("hooks were not installed:\n%s", merged)
			}
		})
	}
}

// TestSetupHooksPath_HonoursHostEnvironment — both hosts let the user move
// their config directory, and a setup command that wrote to the default anyway
// would install hooks into a directory the agent never reads.
func TestSetupHooksPath_HonoursHostEnvironment(t *testing.T) {
	custom := t.TempDir()

	t.Setenv("CLAUDE_CONFIG_DIR", custom)
	got, err := hookSettingsPath("claude-code")
	if err != nil {
		t.Fatalf("hookSettingsPath: %v", err)
	}
	if want := filepath.Join(custom, "settings.json"); got != want {
		t.Errorf("claude-code path = %q, want %q", got, want)
	}

	t.Setenv("CODEX_HOME", custom)
	got, err = hookSettingsPath("codex")
	if err != nil {
		t.Fatalf("hookSettingsPath: %v", err)
	}
	if want := filepath.Join(custom, "hooks.json"); got != want {
		t.Errorf("codex path = %q, want %q", got, want)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory on this host: %v", err)
	}
	got, err = hookSettingsPath("claude-code")
	if err != nil {
		t.Fatalf("hookSettingsPath: %v", err)
	}
	if want := filepath.Join(home, ".claude", "settings.json"); got != want {
		t.Errorf("default claude-code path = %q, want %q", got, want)
	}
}

// TestSetupHooksPath_RejectsUnknownAgent — a typo'd agent must not silently
// write hooks somewhere nothing reads them.
func TestSetupHooksPath_RejectsUnknownAgent(t *testing.T) {
	for _, agent := range []string{"", "claude", "cursor"} {
		if _, err := hookSettingsPath(agent); err == nil {
			t.Errorf("hookSettingsPath(%q) returned no error", agent)
		}
	}
}

// TestSetupHooks_WritesAndReports drives the command end to end against a
// redirected config directory, including the --dry-run contract: print the
// merged result, write nothing.
func TestSetupHooks_WritesAndReports(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	settings := filepath.Join(dir, "settings.json")

	dryRun := captureStdout(t, func() {
		if err := runSetupCmd([]string{"hooks", "--agent", "claude-code", "--dry-run"}); err != nil {
			t.Fatalf("dry run: %v", err)
		}
	})
	if !strings.Contains(dryRun, "engram hook session-start") {
		t.Errorf("dry run did not print the merged settings:\n%s", dryRun)
	}
	if _, err := os.Stat(settings); !os.IsNotExist(err) {
		t.Errorf("--dry-run wrote %s; it must not touch the filesystem", settings)
	}

	if err := runSetupCmd([]string{"hooks", "--agent", "claude-code"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	written, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("settings were not written: %v", err)
	}
	for _, event := range []string{"session-start", "post-compaction", "user-prompt-submit", "subagent-stop", "session-end"} {
		if !strings.Contains(string(written), "engram hook "+event) {
			t.Errorf("settings are missing the %s hook:\n%s", event, written)
		}
	}

	// Second run: same bytes, and it says so instead of pretending to work.
	out := captureStdout(t, func() {
		if err := runSetupCmd([]string{"hooks", "--agent", "claude-code"}); err != nil {
			t.Fatalf("second install: %v", err)
		}
	})
	if !strings.Contains(out, "already installed") {
		t.Errorf("second run output = %q, want it to report a no-op", out)
	}
	after, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if string(after) != string(written) {
		t.Error("the second run rewrote the settings file")
	}
}

// TestSetupHooks_CodexDifferences pins the three places Codex is not Claude
// Code: no "fork" session source, no async hooks, and a 2s ceiling on
// UserPromptSubmit. Shipping Claude's pack to Codex means hooks that never fire
// or settings it rejects.
func TestSetupHooks_CodexDifferences(t *testing.T) {
	claude := hookPackByEvent(t, engramHookPack("claude-code"))
	codex := hookPackByEvent(t, engramHookPack("codex"))

	if !strings.Contains(claude["SessionStart"][0].Matcher, "fork") {
		t.Errorf("claude-code SessionStart matcher = %q, want it to include fork", claude["SessionStart"][0].Matcher)
	}
	if strings.Contains(codex["SessionStart"][0].Matcher, "fork") {
		t.Errorf("codex SessionStart matcher = %q, but Codex has no fork source", codex["SessionStart"][0].Matcher)
	}
	if got := codex["UserPromptSubmit"][0].Hooks[0].Timeout; got != 2 {
		t.Errorf("codex UserPromptSubmit timeout = %d, want 2", got)
	}
	if !claude["SubagentStop"][0].Hooks[0].Async {
		t.Error("claude-code SubagentStop should be async — the report can be filed while the host moves on")
	}
	if codex["SubagentStop"][0].Hooks[0].Async {
		t.Error("codex SubagentStop must not declare async; Codex has no such flag")
	}
	if got := codex["SessionEnd"][0].Hooks[0].Timeout; got != 3 {
		t.Errorf("codex SessionEnd timeout = %d, want 3", got)
	}
}

// TestEngramHookPack_MatchesShippedPack keeps the two ways of installing the
// same hooks in step: the plugin packs under plugin/<agent>/hooks/hooks.json
// (for hosts that install plugins) and the settings merge (for everyone else).
// A user who installs one way and reads the docs for the other must get the
// same behaviour.
func TestEngramHookPack_MatchesShippedPack(t *testing.T) {
	for agent, path := range map[string]string{
		"claude-code": filepath.Join("..", "..", "plugin", "claude-code", "hooks", "hooks.json"),
		"codex":       filepath.Join("..", "..", "plugin", "codex", "hooks", "hooks.json"),
	} {
		t.Run(agent, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read shipped pack: %v", err)
			}
			var shipped struct {
				Hooks map[string][]hookGroup `json:"hooks"`
			}
			if err := json.Unmarshal(raw, &shipped); err != nil {
				t.Fatalf("shipped pack is not valid JSON: %v", err)
			}

			generated := hookPackByEvent(t, engramHookPack(agent))
			if len(shipped.Hooks) != len(generated) {
				t.Errorf("shipped pack has %d events, generated has %d", len(shipped.Hooks), len(generated))
			}
			for event, want := range generated {
				got, ok := shipped.Hooks[event]
				if !ok {
					t.Errorf("shipped pack is missing event %q", event)
					continue
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("event %q differs between the shipped pack and `engram setup hooks`:\nshipped:   %+v\ngenerated: %+v",
						event, got, want)
				}
			}
		})
	}
}

// hookPackByEvent regroups a hookPack by event name, preserving order.
func hookPackByEvent(t *testing.T, pack hookPack) map[string][]hookGroup {
	t.Helper()
	out := map[string][]hookGroup{}
	for _, item := range pack.Events {
		out[item.Event] = append(out[item.Event], item.Group)
	}
	return out
}

// TestSetupCmd_UnknownTarget — `engram setup` is a namespace; an unknown target
// is a usage error, not a silent success.
func TestSetupCmd_UnknownTarget(t *testing.T) {
	if err := runSetupCmd([]string{"everything"}); err == nil {
		t.Error("unknown setup target must be reported")
	}
	if err := runSetupCmd(nil); err == nil {
		t.Error("missing setup target must be reported")
	}
	if err := runSetupCmd([]string{"hooks"}); err == nil {
		t.Error("setup hooks without --agent must be reported")
	}
}

// ─── the settings file survives a failed write ──────────────────────────────

// TestSetupHooks_FailedWriteLeavesTheOriginalIntact is the reason the write is
// a temp-file-and-rename instead of an os.WriteFile.
//
// os.WriteFile truncates first and writes second. A crash, a full disk, an
// antivirus scanner or a killed process between those two steps leaves the
// user's settings.json empty or half-written — and for Claude Code that file
// carries every permission, every model preference and every OTHER hook they
// have. Losing all of it to install a memory hook is not a trade anybody
// agreed to.
//
// The failure is injected where it actually happens: after the temp file is
// written, before the rename.
func TestSetupHooks_FailedWriteLeavesTheOriginalIntact(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, "settings.json")

	original := []byte(`{"permissions":{"allow":["Bash(git status)"]},"model":"opus"}` + "\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	hookSettingsFailAfterTemp = func() error { return errors.New("simulated disk failure") }
	t.Cleanup(func() { hookSettingsFailAfterTemp = nil })

	err := runSetupCmd([]string{"hooks", "--agent", "claude-code"})
	if err == nil {
		t.Fatal("a failed write must be reported, not swallowed")
	}
	if !strings.Contains(err.Error(), "simulated disk failure") {
		t.Errorf("error = %v, want it to carry the underlying failure", err)
	}

	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("the settings file is gone after a failed write: %v", readErr)
	}
	if string(after) != string(original) {
		t.Errorf("the settings file changed despite the failed write:\n got: %s\nwant: %s", after, original)
	}

	// And no debris: a temp file left behind in the user's config directory is
	// litter they have to recognise before they dare delete it.
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("a failed write left %q behind", entry.Name())
		}
	}
}

// TestSetupHooks_BacksUpTheOriginalOnce covers the .bak contract. The backup is
// the user's undo, so it is written on the FIRST modification and never again:
// a backup that tracks the current file is not a backup, it is a second copy of
// whatever engram last wrote.
func TestSetupHooks_BacksUpTheOriginalOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, "settings.json")
	backup := path + ".bak"

	original := []byte(`{"model":"opus"}` + "\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runSetupCmd([]string{"hooks", "--agent", "claude-code"}); err != nil {
			t.Fatalf("install: %v", err)
		}
	})
	if !strings.Contains(out, "copied to") {
		t.Errorf("the command did not mention the backup it wrote:\n%s", out)
	}
	if !strings.Contains(out, "re-serialized") {
		t.Errorf("the command did not disclose that top-level key order may change:\n%s", out)
	}

	saved, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("no backup was written: %v", err)
	}
	if string(saved) != string(original) {
		t.Errorf("backup = %s, want the ORIGINAL bytes %s", saved, original)
	}

	// A later run that changes the file again must not overwrite that copy.
	// (Remove one hook so there is something to add, and therefore a write.)
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	trimmed := strings.Replace(string(current), `"engram hook session-end"`, `"somebody elses hook"`, 1)
	if err := os.WriteFile(path, []byte(trimmed), 0o600); err != nil {
		t.Fatalf("rewrite settings: %v", err)
	}
	if err := runSetupCmd([]string{"hooks", "--agent", "claude-code"}); err != nil {
		t.Fatalf("second install: %v", err)
	}
	saved2, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(saved2) != string(original) {
		t.Errorf("the second run overwrote the pristine backup:\n got: %s\nwant: %s", saved2, original)
	}
}

// TestSetupHooks_NewFileIsPrivate — an agent's settings routinely carry API
// keys, and a file engram CREATES has no prior mode to inherit. 0600 is the
// only defensible default; 0644 would publish it to every account on the box.
// (Skipped on Windows, where the Unix permission bits are not the ACL.)
func TestSetupHooks_NewFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not the access control on Windows")
	}
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	if err := runSetupCmd([]string{"hooks", "--agent", "claude-code"}); err != nil {
		t.Fatalf("install: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("settings were not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("new settings file mode = %04o, want 0600", perm)
	}
}

// TestSetupHooks_PreservesAnExistingFileMode — the other half: a file the user
// already owns keeps the mode they chose. Tightening it silently is still
// changing their configuration.
func TestSetupHooks_PreservesAnExistingFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not the access control on Windows")
	}
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"model":"opus"}`), 0o644); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	if err := runSetupCmd([]string{"hooks", "--agent", "claude-code"}); err != nil {
		t.Fatalf("install: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("settings file mode = %04o, want the original 0644", perm)
	}
}
