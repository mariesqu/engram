package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// setup.go implements `engram setup hooks --agent <claude-code|codex>`: it
// merges engram's lifecycle hooks into the agent host's own settings file.
//
// The merge is APPEND-ONLY and deduplicated by command. That is the whole
// design: this file belongs to the user, not to engram. It carries their other
// hooks, their permissions, their model preferences, and keys this binary has
// never heard of — a rewrite that "normalizes" it is a rewrite that silently
// deletes their configuration. So the file is decoded into json.RawMessage at
// every level except the one being touched, every unknown key is carried
// through byte-for-byte, and an entry whose command is already present is left
// exactly as it is (timeout edits, matcher edits and all).
//
// Two things the merge does NOT preserve, both of them stated on stdout when it
// writes: the ORDER of top-level keys (Go marshals a map sorted by key) and the
// original indentation (the output is re-indented with two spaces, which is
// what every agent host writes). The write itself goes through
// writeHookSettings — temp file, rename, one .bak — because a settings file is
// not a thing to truncate in place and hope.
const setupUsage = `Usage: engram setup hooks --agent <claude-code|codex> [--dry-run]

Install engram's lifecycle hooks into an agent host's settings.

The hooks call the "engram" binary from PATH (engram hook <event>), so make sure
this binary is on PATH — or install the plugin pack under plugin/<agent>/ if your
host supports plugins.

Targets:
  claude-code   $CLAUDE_CONFIG_DIR/settings.json (default: ~/.claude/settings.json)
  codex         $CODEX_HOME/hooks.json           (default: ~/.codex/hooks.json)

Flags:
  --agent      Which host to install into (required)
  --dry-run    Print the merged file to stdout instead of writing it

The merge is append-only: existing hooks, and any other setting in the file, are
preserved. An engram hook that is already installed is left untouched, so
re-running this command is a no-op.
`

// runSetupCmd is the entry point for `engram setup <target>`.
func runSetupCmd(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, setupUsage)
		return errors.New("setup: a target is required (currently: hooks)")
	}
	switch args[0] {
	case "hooks":
		return runSetupHooksCmd(args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, setupUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, setupUsage)
		return fmt.Errorf("setup: unknown target %q", args[0])
	}
}

func runSetupHooksCmd(args []string) error {
	fs := flag.NewFlagSet("setup hooks", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), setupUsage) }
	agent := fs.String("agent", "", "agent host to install into: claude-code | codex")
	dryRun := fs.Bool("dry-run", false, "print the merged settings instead of writing them")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("setup hooks takes no positional arguments; unexpected: %v", fs.Args())
	}

	path, err := hookSettingsPath(*agent)
	if err != nil {
		return err
	}

	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("setup hooks: read %s: %w", path, err)
	}

	merged, added, err := mergeHookSettings(existing, engramHookPack(*agent))
	if err != nil {
		return fmt.Errorf("setup hooks: %s: %w", path, err)
	}

	if *dryRun {
		fmt.Printf("# %s (dry run — nothing written; %d hook(s) would be added)\n", path, added)
		fmt.Println(string(merged))
		return nil
	}

	if added == 0 {
		fmt.Printf("engram hooks already installed in %s — nothing to do\n", path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("setup hooks: create %s: %w", filepath.Dir(path), err)
	}
	backup, err := writeHookSettings(path, merged, existing)
	if err != nil {
		return fmt.Errorf("setup hooks: write %s: %w", path, err)
	}
	fmt.Printf("installed %d engram hook(s) into %s\n", added, path)
	if backup != "" {
		fmt.Printf("Your previous settings were copied to %s.\n", backup)
	}
	fmt.Println("Note: the file was re-serialized, so top-level key ORDER may differ from before; " +
		"every key and value is preserved.")
	fmt.Printf("Note: %s.\n", setupHookBinaryHint())
	fmt.Println("Restart the agent for the hooks to take effect.")
	return nil
}

// hookSettingsFailAfterTemp is a TEST seam. When non-nil it is called after the
// temp file has been written and before the rename, and a non-nil return aborts
// the write. It exists because the whole point of the temp-and-rename dance is
// what happens when the write fails halfway — a full disk, a killed process, an
// antivirus scanner holding the file — and that is otherwise the one path no
// test can reach. Production leaves it nil.
var hookSettingsFailAfterTemp func() error

// writeHookSettings replaces path's contents with merged, atomically, and
// returns the path of the backup it wrote (empty when it wrote none).
//
// Atomic because this file belongs to the USER. os.WriteFile truncates first
// and writes second: a crash, a full disk or a killed process between the two
// leaves an agent host's settings.json empty or half-written, which for Claude
// Code means every permission, every model preference and every OTHER hook the
// user had is gone — to install a memory hook. A temp file in the SAME
// directory (so the rename stays on one filesystem, where it is atomic) and
// os.Rename over the original cannot produce that state: either the old file is
// there or the new one is.
//
// The file's mode is preserved when it already exists; a file created here is
// 0600, because an agent's settings routinely carry API keys and nothing else
// on the machine needs to read them.
//
// A <file>.bak copy of the ORIGINAL bytes is written on the FIRST modification
// only. Re-running the command must not overwrite the one pristine copy with a
// version engram has already edited — a backup that tracks the current file is
// not a backup.
func writeHookSettings(path string, merged, existing []byte) (string, error) {
	dir := filepath.Dir(path)

	perm := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}

	backup := ""
	if len(existing) > 0 {
		candidate := path + ".bak"
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(candidate, existing, perm); err != nil {
				// Not fatal: a backup that could not be written is a reason to warn,
				// not a reason to leave the hooks uninstalled.
				fmt.Fprintf(os.Stderr, "engram setup hooks: could not write %s: %v\n", candidate, err)
			} else {
				backup = candidate
			}
		}
	}

	tmp, err := os.CreateTemp(dir, ".engram-settings-*.json.tmp")
	if err != nil {
		return backup, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(merged); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return backup, fmt.Errorf("write temp file: %w", err)
	}
	// Sync BEFORE the rename, or the atomicity above is only atomic against a
	// crashed process. A rename is a metadata operation: the OS is free to
	// publish the new directory entry while the file's CONTENT is still in the
	// page cache, so a machine that loses power between the two leaves the user's
	// settings.json pointing at a zero-length file — the exact outcome the temp
	// file exists to prevent, arrived at the long way round. Not fatal on its
	// own: a filesystem that refuses fsync (some network mounts) is a reason to
	// warn, not a reason to abandon a write that is otherwise complete.
	if err := tmp.Sync(); err != nil {
		fmt.Fprintf(os.Stderr, "engram setup hooks: could not flush %s to disk (%v); "+
			"the write is still atomic against a crashed process, but not against a power loss\n", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return backup, fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		_ = os.Remove(tmpPath)
		return backup, fmt.Errorf("chmod temp file: %w", err)
	}
	if hookSettingsFailAfterTemp != nil {
		if err := hookSettingsFailAfterTemp(); err != nil {
			_ = os.Remove(tmpPath)
			return backup, err
		}
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return backup, fmt.Errorf("replace %s: %w", path, err)
	}
	return backup, nil
}

// hookSettingsPath returns the file to merge into for the named agent.
//
// Claude Code owns ~/.claude/settings.json and honours CLAUDE_CONFIG_DIR;
// Codex owns ~/.codex and honours CODEX_HOME. Upstream installs the Codex pack
// by shelling out to `codex plugin add` against a published marketplace, which
// this fork does not have — so the hooks file is written directly, in the same
// shape the shipped pack (plugin/codex/hooks/hooks.json) declares.
func hookSettingsPath(agent string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("setup hooks: resolve home directory: %w", err)
	}
	switch strings.TrimSpace(strings.ToLower(agent)) {
	case "claude-code":
		root := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
		if root == "" {
			root = filepath.Join(home, ".claude")
		}
		return filepath.Join(root, "settings.json"), nil
	case "codex":
		root := strings.TrimSpace(os.Getenv("CODEX_HOME"))
		if root == "" {
			root = filepath.Join(home, ".codex")
		}
		return filepath.Join(root, "hooks.json"), nil
	case "":
		return "", errors.New("setup hooks: --agent is required (claude-code | codex)")
	default:
		return "", fmt.Errorf("setup hooks: unknown agent %q (want claude-code or codex)", agent)
	}
}

// hookEntry is one command hook inside a matcher group.
type hookEntry struct {
	Type          string `json:"type"`
	Command       string `json:"command"`
	Timeout       int    `json:"timeout,omitempty"`
	StatusMessage string `json:"statusMessage,omitempty"`
	Async         bool   `json:"async,omitempty"`
}

// hookGroup is one matcher group of an event.
type hookGroup struct {
	Matcher string      `json:"matcher,omitempty"`
	Hooks   []hookEntry `json:"hooks"`
}

// hookPack is the set of groups engram installs, per event. The order of events
// is irrelevant to the host; the order WITHIN an event is preserved.
type hookPack struct {
	Events []struct {
		Event string
		Group hookGroup
	}
}

// engramHookPack returns the hooks engram installs for the named agent. It is
// the same content as the shipped packs under plugin/<agent>/hooks/hooks.json,
// with one difference that matters: the shipped packs invoke the plugin's own
// copy of the binary path the host provides, while a settings merge has no
// plugin root and calls "engram" from PATH.
//
// TestEngramHookPack_MatchesShippedPack keeps the two in step.
func engramHookPack(agent string) hookPack {
	codex := strings.EqualFold(strings.TrimSpace(agent), "codex")

	// Codex's SessionStart has no "fork" source, does not support async hooks,
	// and gives UserPromptSubmit a 2s ceiling (ours finishes in 200ms).
	startMatcher := "startup|resume|clear|fork"
	promptTimeout := 10
	if codex {
		startMatcher = "startup|resume|clear"
		promptTimeout = 2
	}
	sessionEndTimeout := 5
	if codex {
		sessionEndTimeout = 3
	}

	pack := hookPack{}
	add := func(event string, group hookGroup) {
		pack.Events = append(pack.Events, struct {
			Event string
			Group hookGroup
		}{event, group})
	}

	add("SessionStart", hookGroup{
		Matcher: startMatcher,
		Hooks: []hookEntry{{
			Type:          "command",
			Command:       "engram hook session-start",
			Timeout:       10,
			StatusMessage: "Loading engram memory...",
		}},
	})
	add("SessionStart", hookGroup{
		Matcher: "compact",
		Hooks: []hookEntry{{
			Type:          "command",
			Command:       "engram hook post-compaction",
			Timeout:       10,
			StatusMessage: "Recovering engram context after compaction...",
		}},
	})
	add("UserPromptSubmit", hookGroup{
		Hooks: []hookEntry{{
			Type:    "command",
			Command: "engram hook user-prompt-submit",
			Timeout: promptTimeout,
		}},
	})
	subagent := hookEntry{
		Type:    "command",
		Command: "engram hook subagent-stop",
		Timeout: 10,
		// Async lets the host carry on while the report is filed. Codex has no
		// such flag, and an unknown key in its settings is a risk not worth a
		// few hundred milliseconds.
		Async: !codex,
	}
	subagentGroup := hookGroup{Hooks: []hookEntry{subagent}}
	if codex {
		subagentGroup.Matcher = ".*"
	}
	add("SubagentStop", subagentGroup)
	add("SessionEnd", hookGroup{
		Hooks: []hookEntry{{
			Type:    "command",
			Command: "engram hook session-end",
			Timeout: sessionEndTimeout,
		}},
	})
	return pack
}

// mergeHookSettings merges pack into the settings document in existing and
// returns the re-encoded file plus how many groups were added.
//
// Everything outside the groups being appended is preserved verbatim: the
// document is decoded as map[string]json.RawMessage, the "hooks" object as
// map[string][]json.RawMessage, and each existing group stays the raw bytes it
// arrived as. Only the events engram touches are re-encoded, and only by
// appending to their list.
//
// Deduplication is by COMMAND, not by deep equality: once an engram hook is
// installed, the user owns its timeout, its matcher and its status message. A
// second run must not "restore" them.
func mergeHookSettings(existing []byte, pack hookPack) ([]byte, int, error) {
	doc := map[string]json.RawMessage{}
	if len(strings.TrimSpace(string(existing))) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, 0, fmt.Errorf("parse settings: %w "+
				"(refusing to overwrite a file that is not valid JSON — fix or move it first)", err)
		}
	}

	hooks := map[string][]json.RawMessage{}
	if raw, ok := doc["hooks"]; ok && len(strings.TrimSpace(string(raw))) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return nil, 0, fmt.Errorf("parse hooks: %w", err)
		}
	}

	installed := installedHookCommands(hooks)

	added := 0
	for _, item := range pack.Events {
		command := ""
		if len(item.Group.Hooks) > 0 {
			command = item.Group.Hooks[0].Command
		}
		if command == "" || installed[command] {
			continue
		}
		encoded, err := json.Marshal(item.Group)
		if err != nil {
			return nil, 0, fmt.Errorf("encode %s hook: %w", item.Event, err)
		}
		hooks[item.Event] = append(hooks[item.Event], encoded)
		installed[command] = true
		added++
	}

	if added > 0 {
		encoded, err := json.Marshal(hooks)
		if err != nil {
			return nil, 0, fmt.Errorf("encode hooks: %w", err)
		}
		doc["hooks"] = encoded
	}

	// 2-space indent: what every agent host writes, so a merged file does not
	// show up as a whole-file diff the next time the host rewrites it.
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, 0, fmt.Errorf("encode settings: %w", err)
	}
	return append(out, '\n'), added, nil
}

// installedHookCommands collects every command string already present anywhere
// in the settings' hooks, so a hook installed under a different event or
// matcher than the one this version ships is still recognised as installed.
func installedHookCommands(hooks map[string][]json.RawMessage) map[string]bool {
	found := map[string]bool{}
	for _, groups := range hooks {
		for _, raw := range groups {
			var group struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			}
			if err := json.Unmarshal(raw, &group); err != nil {
				continue // a group we cannot read is a group we must not touch
			}
			for _, h := range group.Hooks {
				if cmd := strings.TrimSpace(h.Command); cmd != "" {
					found[cmd] = true
				}
			}
		}
	}
	return found
}

// setupHookBinaryHint reports how the hooks will invoke engram, for the CLI's
// closing advice. Kept separate so the platform wording lives in one place.
func setupHookBinaryHint() string {
	if runtime.GOOS == "windows" {
		return `the hooks run "engram hook <event>", so engram.exe must be on PATH`
	}
	return `the hooks run "engram hook <event>", so engram must be on PATH`
}
