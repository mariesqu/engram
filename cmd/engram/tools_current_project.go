package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
	projectpkg "github.com/mariesqu/engram/internal/project"
	"github.com/mariesqu/engram/internal/syncer"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerCurrentProjectTools(srv *mcpserver.MCPServer, store *localstore.Store, loop *syncer.Loop, embedLoop *embedding.Loop, gated embedding.EmbeddingProvider, writerID string, activity *SessionActivity, daemonCwdIsWorkspace bool) {
	// ── mem_current_project ──────────────────────────────────────────────────
	// Registered first because it is meant to be CALLED first: agent protocols
	// (gentle-ai's ODD protocol among them) open a session by asking which
	// project this caller resolves to, before any read or write.
	srv.AddTool(
		mcp.NewTool("mem_current_project",
			mcp.WithDescription(`Detect the project Engram resolves for THIS caller, and how it got there. Returns project, project_source, project_path (the canonical directory of the project — repo root, config directory, or the resolved cwd), cwd (absolute, cleaned), cwd_input (what you passed, verbatim), directory_source, directory_exists, available_projects, fallback, writes_blocked and optional hints. NEVER errors — use it for discovery before writing. Recommended as the first call when starting a new session.

Three fields say "do not trust this name blindly":
  fallback=true         — the project name is a GUESS (a directory basename, or a lenient fallback after a resolution error). Pass an explicit project on later calls if that is not the name you want.
  writes_blocked=true   — mem_save/mem_save_prompt/mem_session_start/mem_session_summary will REFUSE this directory (ambiguous, misconfigured, missing, relative, no directory at all, or an omitted project) until you pass project explicitly — or, when error_hint says directory is not a string, until you send it as a string or omit it.
  directory_exists=false — the resolved directory does not exist, so any name here is invented from its basename. Pass a real directory or an explicit project.`),
			mcp.WithTitleAnnotation("Detect Current Project"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString("project",
				mcp.Description("Optional explicit project. When set it is echoed back verbatim with project_source=\"explicit_override\" and no detection runs — the way to confirm the exact name your later calls will use."),
			),
			mcp.WithString("directory",
				mcp.Description(directoryArgDescription),
			),
			mcp.WithString("cwd",
				mcp.Description(cwdArgDescription),
			),
		),
		handleCurrentProject(store, daemonCwdIsWorkspace),
	)
}

// handleCurrentProject returns the handler for mem_current_project — the
// session-bootstrap probe. It answers one question: which project will THIS
// caller's tool calls resolve to, and how was that name derived?
//
// It reports the LENIENT read resolution (the resolveReadProject chain:
// explicit project → detection from the resolved directory → basename), because
// that is what mem_search / mem_context / mem_review actually answer from. What
// it adds on top is the part an agent cannot otherwise see until something goes
// wrong — the ways that name is a GUESS rather than an identity:
//
//   - fallback=true: the project is only a directory basename, or a lenient
//     fallback after a resolution error. Nothing declared it (no
//     .engram/config.json, no git remote, no git root), so it changes the day
//     the folder is renamed.
//   - directory_source="daemon_cwd": no directory reached the daemon, so the
//     answer describes the DAEMON's working directory. The daemon is shared and
//     typically resident from wherever autostart or the tray launched it (on
//     Windows commonly C:\Windows\system32) — the original junk-project misfile.
//   - directory_source="cwd_alias": the directory came from the MODEL, not from
//     `engram connect`. It is an assertion about the workspace, not an
//     observation of it.
//   - directory_source="relative_path": the value was relative, so it was
//     resolved against the DAEMON's working directory rather than the caller's.
//     "." is the answer a model gives when asked for its cwd, and it resolves to
//     a real, existing, entirely unrelated project.
//   - directory_exists=false: the resolved directory is not there at all, so the
//     basename it yields names nothing that exists.
//
// Plus writes_blocked=true for the read/write asymmetry: an ambiguous
// directory, a broken .engram/config.json, a missing directory, or an omitted
// project all answer reads from the basename while making every write tool
// refuse. Without the flag an agent learns that only from a failed mem_save,
// halfway through a session.
//
// store is read ONLY for the project's sync policy (PolicyOmitted ⇒ writes are
// refused before any row is written — see handleSave). It may be nil, which
// skips that check; every other field is filesystem-derived.
//
// Like its upstream counterpart it NEVER returns a tool error: a discovery call
// that fails is a discovery call an agent stops making. That holds even for a
// non-string "directory", which every OTHER directory-aware tool rejects as a
// caller error — here it is reported in the envelope
// (directory_source="invalid_directory_argument") so the agent can still see
// what the daemon would resolve.
func handleCurrentProject(store *localstore.Store, daemonCwdIsWorkspace bool) mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		explicitProject, _ := args["project"].(string)
		dirArg := readDirectoryArg(args)

		envelope := currentProjectEnvelope(store, explicitProject, dirArg, daemonCwdIsWorkspace)

		out, err := json.Marshal(envelope)
		if err != nil {
			// Unreachable: the envelope holds only strings, bools and []string.
			// Degrade to the one field that matters rather than to an error — the
			// never-errors contract is the point of this tool.
			return mcp.NewToolResultText(fmt.Sprintf("project: %v", envelope["project"])), nil
		}
		return mcp.NewToolResultText(string(out)), nil
	}
}

// currentProjectEnvelope builds the mem_current_project response body. It is
// separated from the MCP plumbing so the resolution contract can be tested
// directly; it reads the filesystem (detection), the store (policy), and — only
// when no directory reached the daemon — the process working directory.
//
// Field contract. Always present:
//
//	project, project_source, project_path (CANONICAL directory of the project:
//	the repo root for the git cases, the directory holding .engram/config.json
//	for a pinned one, else the resolved cwd), cwd (absolute and cleaned),
//	cwd_input (the caller's value verbatim, "" when none), directory_source,
//	directory_exists, available_projects (never null), fallback, writes_blocked.
//
// Advisory, present only when they have something to say: warning, error_hint,
// hints. hints is an ARRAY — the reasons a name is untrustworthy compose (a
// daemon-cwd answer for a missing directory under an omitted project is three
// separate problems), and a joined sentence makes an agent parse prose to tell
// them apart.
func currentProjectEnvelope(store *localstore.Store, explicitProject string, dirArg directoryArg, daemonCwdIsWorkspace bool) map[string]any {
	dir := resolveProjectDir(dirArg.Directory)

	env := map[string]any{
		"project":            "",
		"project_source":     "",
		"project_path":       "",
		"cwd":                dir,
		"cwd_input":          dirArg.Input,
		"directory_source":   dirArg.Source,
		"directory_exists":   false,
		"available_projects": []string{},
		"fallback":           false,
		"writes_blocked":     false,
	}

	var hints []string
	if dirArg.Err != nil {
		// The one caller error this tool reports instead of raising: the bridge
		// leaves a non-string "directory" alone (see injectClientDirectory), so
		// nothing else in the chain would ever mention it.
		// Writes are blocked even beside an explicit project: every write tool
		// raises this type error (readDirectoryArg) before it looks at project, so
		// the remedy here is to fix or drop "directory", not to name a project.
		env["error_hint"] = dirArg.Err.Error()
		env["writes_blocked"] = true
		hints = append(hints, "the \"directory\" argument was not a string and was IGNORED (the \"cwd\" alias is "+
			"deliberately not consulted for it) — this answer describes the daemon's own directory",
			"every write tool refuses a non-string \"directory\", even with an explicit project — send it as a string or omit it")
	}
	if dirArg.Warning != "" {
		// The alias's own malformed-argument case. It is not an error_hint: nothing
		// was refused, and the answer below is the one an absent alias would give.
		hints = append(hints, dirArg.Warning)
	}

	// Does the directory the answer is about actually exist? Detection happily
	// derives a basename from a path that is not there, which is how a typo'd
	// ENGRAM_CLIENT_DIR or a hallucinated "cwd" invents a brand-new project.
	info, statErr := os.Stat(dir)
	switch {
	case statErr == nil && info.IsDir():
		env["directory_exists"] = true
	case dir == "":
		hints = append(hints, "the daemon could not read its own working directory, so no directory could be resolved — pass directory or project explicitly")
	default:
		hints = append(hints, "the resolved directory does not exist (or is not a directory), so any project name here is invented from its basename — pass a real directory or an explicit project")
	}

	// An explicit project wins outright in every other tool (resolveReadProject /
	// resolveSaveProject), so no detection runs here either: reporting a detected
	// project beside an explicit one would only invite an agent to second-guess
	// the name it just supplied. The policy check below still applies — an
	// explicit name does not make an omitted project writable.
	if explicitProject = strings.TrimSpace(explicitProject); explicitProject != "" {
		env["project"] = explicitProject
		env["project_source"] = projectpkg.SourceExplicitOverride
		applyPolicyBlock(store, env, &hints)
		setHints(env, hints)
		return env
	}

	if dirArg.Relative {
		// Reported BEFORE the existence check so a relative path that resolves to
		// nothing still says WHY it resolved there. Writes are blocked for the same
		// reason resolveSaveProject refuses them: the path was resolved against the
		// daemon's cwd, so the project it names belongs to the daemon, not to the
		// caller. Kept out of the explicit-project branch above on purpose — naming
		// a project skips the directory entirely, so nothing is blocked.
		env["writes_blocked"] = true
		if hint, ok := dirArg.gitBashHint(); ok {
			// Same sentence the write tools return, so an agent that has read one
			// recognises the other.
			hints = append(hints, hint+" — every write tool refuses it")
		} else {
			hints = append(hints, fmt.Sprintf("the directory you passed (%q) is RELATIVE: %s — "+
				"every write tool refuses it", dirArg.Directory, relativeDirectoryHint))
		}
	}

	if !env["directory_exists"].(bool) {
		// Detection would answer from the basename of a path that is not there.
		// Report the name every read tool will use, but never as a normal answer:
		// missing_directory says the name describes nothing on this machine.
		env["project"] = projectpkg.DetectProject(dir)
		env["project_source"] = sourceMissingDirectory
		env["project_path"] = dir
		env["fallback"] = true
		env["writes_blocked"] = true
		hints = append(hints, "writes (mem_save, mem_session_start, mem_session_summary) should not file memories under a directory that does not exist — pass project explicitly")
		applyPolicyBlock(store, env, &hints)
		setHints(env, hints)
		return env
	}

	det := projectpkg.DetectProjectFull(dir)
	env["project"] = det.Project
	env["project_source"] = det.Source
	env["project_path"] = det.Path
	if len(det.AvailableProjects) > 0 {
		env["available_projects"] = det.AvailableProjects
	}
	if det.Warning != "" {
		env["warning"] = det.Warning
	}

	if det.Error != nil {
		// Lenient, exactly like resolveReadProject: reads answer from the basename
		// rather than erroring. project_source keeps det.Source — a broken
		// .engram/config.json is still a CONFIG answer, and relabelling it
		// "dir_basename" would send the agent looking for a missing config file
		// instead of the malformed one it has.
		env["project"] = projectpkg.DetectProject(dir)
		env["project_path"] = dir
		env["fallback"] = true
		env["error_hint"] = det.Error.Error()
		if errors.Is(det.Error, projectpkg.ErrInvalidConfig) || errors.Is(det.Error, projectpkg.ErrAmbiguousProject) {
			env["writes_blocked"] = true
			hints = append(hints, "reads fall back to the directory basename but writes (mem_save, mem_session_start, mem_session_summary) will REFUSE this directory — pass project explicitly")
		}
		if errors.Is(det.Error, projectpkg.ErrInvalidConfig) {
			hints = append(hints, "the .engram/config.json in this directory is present but unusable — fix its project_name, or pass project explicitly")
		}
	}
	if env["project_source"] == projectpkg.SourceDirBasename {
		env["fallback"] = true
		hints = append(hints, "the project name is only a directory basename (no .engram/config.json, git remote or git root) — pass project explicitly if that is not the name you want")
	}
	switch dirArg.Source {
	case dirSourceDaemonCwd:
		if daemonCwdIsWorkspace {
			// A per-client `engram daemon --transport stdio` (README.md's documented
			// setup): the MCP client spawned THIS daemon process in the project
			// directory, so its cwd genuinely is the caller's workspace — not the
			// SHARED resident daemon's own directory. Nothing is blocked; the label
			// still says where the answer came from, softened to say why it is
			// trusted here.
			hints = append(hints, "no directory reached the daemon, but this daemon is running in per-client stdio "+
				"mode (--transport stdio), so its own working directory IS your workspace — writes are not blocked")
		} else {
			env["writes_blocked"] = true
			hints = append(hints, "no directory reached the daemon, so this is the DAEMON's own working directory and typically NOT your repo — every write tool refuses it without an explicit project; restart the resident daemon on a current binary, set ENGRAM_CLIENT_DIR, or pass directory/project explicitly")
		}
	case dirSourceCwdAlias:
		hints = append(hints, "this directory came from the \"cwd\" alias you supplied, not from 'engram connect' — if it is not the workspace you are actually in, every later call is filed under the wrong project")
		// dirSourceRelativePath is deliberately absent: its hint is emitted above,
		// before the existence check, so it survives the missing-directory return.
	}

	applyPolicyBlock(store, env, &hints)
	setHints(env, hints)
	return env
}

// sourceMissingDirectory is the project_source reported when the resolved
// directory does not exist. It is deliberately NOT one of the projectpkg
// Source* constants: those all describe evidence found on disk, and there is
// none here.
const sourceMissingDirectory = "missing_directory"

// applyPolicyBlock sets writes_blocked when the resolved project's sync policy
// is "omitted", the one reason a write is refused that no amount of filesystem
// evidence can reveal: mem_save, mem_save_prompt, mem_session_start and
// mem_session_summary all check GetPolicy and return "capture refused" BEFORE
// writing anything, whether the project was detected or named explicitly. A nil store (direct unit tests)
// skips the check; a failed lookup is reported as a hint rather than swallowed.
func applyPolicyBlock(store *localstore.Store, env map[string]any, hints *[]string) {
	project, _ := env["project"].(string)
	if store == nil || strings.TrimSpace(project) == "" {
		return
	}
	pol, err := store.GetPolicy(project)
	if err != nil {
		*hints = append(*hints, fmt.Sprintf("could not read the sync policy for project %q (%v) — writes may still be refused", project, err))
		return
	}
	if pol == localstore.PolicyOmitted {
		env["writes_blocked"] = true
		*hints = append(*hints, fmt.Sprintf("project %q has policy \"omitted\": every write tool refuses it (capture refused) — "+
			"change it with 'engram projects policy %s local-only' or save under a different project", project, project))
	}
}

// setHints attaches the advisory hints array, omitting the key entirely when
// there is nothing to say (the shape the response-schema contract pins).
func setHints(env map[string]any, hints []string) {
	if len(hints) > 0 {
		env["hints"] = hints
	}
}
