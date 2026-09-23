package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	projectpkg "github.com/mariesqu/engram/internal/project"
	"github.com/mark3labs/mcp-go/mcp"
)

// directoryArgDescription is the shared doc string for the optional "directory"
// argument of every directory-aware tool. It is deliberately identical across
// tools — agents should leave "directory" to the transport — so it is worded to
// hold for all of them. Every directory-aware tool also accepts "project", which
// outranks it (see TestRegisterTools_DirectoryAwareToolsDeclareProject).
// TestRegisterTools_DirectoryAwareToolsDeclareDirectory pins that every
// directory-aware tool ships exactly this text.
const directoryArgDescription = "Directory to resolve the project from. Normally injected automatically by 'engram connect' " +
	"(the client's working directory); set it by hand only to target a different checkout. " +
	"Where a tool also accepts 'project', prefer that."

// cwdArgDescription is the shared doc string for the "cwd" alias of the
// "directory" argument. Every directory-aware tool declares it and every
// directory-aware handler reads it through readDirectoryArg, so the alias the
// injected agent protocol tells models to use ("call it with the workspace in
// cwd") means the same thing everywhere instead of on mem_current_project only.
// TestRegisterTools_DirectoryAwareToolsDeclareCwdAlias pins the text.
const cwdArgDescription = "Alias for \"directory\", read ONLY when \"directory\" is absent or blank — " +
	"'engram connect' injects the real client directory into \"directory\", and that value must keep " +
	"winning over a hand-written path. Pass an ABSOLUTE path: a relative one (\".\", \"./repo\") is " +
	"resolved with filepath.Abs against the DAEMON's working directory — a shared, resident process that " +
	"is almost never in your repo — so write tools refuse it."

// Directory-source labels. They answer the question an agent cannot otherwise
// ask — WHOSE idea was the directory this answer describes? — and are reported
// verbatim in mem_current_project's directory_source field.
const (
	// dirSourceArgument: the "directory" argument decided. Normally that is the
	// CLIENT's working directory, injected by `engram connect`.
	dirSourceArgument = "argument"
	// dirSourceCwdAlias: the "cwd" alias decided, i.e. a path the MODEL supplied.
	// Weaker evidence than dirSourceArgument by construction — see readDirectoryArg.
	dirSourceCwdAlias = "cwd_alias"
	// dirSourceDaemonCwd: nothing reached the daemon, so the answer describes the
	// SHARED daemon's own working directory — the original junk-project misfile.
	dirSourceDaemonCwd = "daemon_cwd"
	// dirSourceInvalid: "directory" was present but not a JSON string. Never
	// silently downgraded to the alias — see readDirectoryArg.
	dirSourceInvalid = "invalid_directory_argument"
	// dirSourceRelativePath: the value that decided (either key) was RELATIVE.
	// resolveProjectDir resolves it with filepath.Abs, i.e. against the DAEMON's
	// working directory — so it is neither the caller's directory nor an honest
	// daemon_cwd answer, and it gets a label of its own.
	dirSourceRelativePath = "relative_path"
	// dirSourceTranslatedPosix: the value was a Git Bash/MSYS path ("/c/GitLab/x")
	// and this is Windows, so it was rewritten to its native form before anything
	// resolved it. The answer is about the caller's directory — that is why it is
	// not dirSourceRelativePath — but the path in it is NOT the string they sent,
	// and a source label that hid that would make the one field that exists to
	// say "how did we get here?" lie. cwd_input still carries the original.
	dirSourceTranslatedPosix = "translated_posix_path"
)

// relativeDirectoryHint is the one sentence every surface uses for a relative
// directory — the tool errors write tools return and the hint
// mem_current_project reports. One wording, so an agent that has read it once
// recognises it wherever it turns up.
const relativeDirectoryHint = "a RELATIVE path was resolved against the daemon's working directory, not yours — " +
	"pass an absolute path or an explicit project"

// directoryArg is the outcome of reading the directory a tool call resolves its
// project from. Source is one of the dirSource* labels; Err is non-nil only for
// dirSourceInvalid.
type directoryArg struct {
	Directory string
	// Input is what the caller actually sent, verbatim. It differs from
	// Directory only when a path was normalized (a Git Bash path translated, an
	// extended-length prefix stripped), and mem_current_project reports IT as
	// cwd_input: a caller shown only the rewritten value cannot tell what engram
	// did with what they typed, which is the question that field answers.
	Input  string
	Source string
	Err    error
	// Relative is true when Directory is a non-absolute path (Source is then
	// dirSourceRelativePath). Write tools REFUSE it: filepath.Abs would resolve
	// it against the daemon's cwd, filing the memory under whatever directory the
	// autostart or tray happened to launch from.
	Relative bool
	// Warning is an advisory mem_current_project turns into a hint. It has two
	// sources: a "cwd" alias that was present but not a string (NOT promoted to
	// Err — the alias is a courtesy, and a malformed one falls back to the same
	// daemon-cwd answer an absent one would — but not silence either, because the
	// caller believes they supplied a directory), and a Git Bash path that was
	// translated, where the answer is right but is about a path the caller never
	// wrote.
	Warning string
}

// readDirectoryArg extracts the directory a directory-aware tool call resolves
// its project from, applying one precedence for ALL of them:
//
//  1. "directory", when it is a non-blank string. `engram connect` injects the
//     CLIENT process's working directory here — the only value in the chain that
//     is OBSERVED rather than guessed.
//  2. "cwd", the alias the injected agent protocol tells models to fill in. It is
//     consulted ONLY when the "directory" KEY is absent, JSON null, or a blank
//     string, so a hallucinated path can never re-point a session that already
//     carries a real directory.
//  3. Neither — the caller gets "" and resolveProjectDir falls back to the
//     daemon's own cwd.
//
// A RELATIVE value in either key is kept (reads still answer from it) but
// labelled dirSourceRelativePath, because resolveProjectDir resolves it with
// filepath.Abs — against the DAEMON's working directory, not the caller's. "."
// is the value a model filling in the alias writes sooner or later, and left
// unlabelled it reads exactly like an observed directory while describing the
// resident daemon's own folder.
//
// A "directory" that is PRESENT but not a string is a caller error, reported as
// such (Err non-nil) rather than treated as absent. That case is the one the
// bridge cannot help with: injectClientDirectory deliberately suppresses
// injection for a non-string "directory" (hasNonEmptyStringArg in connect.go —
// "the caller's error to see, not ours to paper over"), so silently falling
// through to "cwd" here would resolve the session from a model-supplied path
// while the caller believes their own argument is in force.
func readDirectoryArg(args map[string]any) directoryArg {
	if raw, present := args["directory"]; present && raw != nil {
		dir, ok := raw.(string)
		if !ok {
			return directoryArg{
				Source: dirSourceInvalid,
				Err: fmt.Errorf("directory must be a string, got %T; "+
					"drop it and let 'engram connect' inject the client directory, or pass project explicitly", raw),
			}
		}
		if dir = strings.TrimSpace(dir); dir != "" {
			return newDirectoryArg(dir, dirSourceArgument)
		}
	}
	if raw, present := args["cwd"]; present && raw != nil {
		cwd, ok := raw.(string)
		if !ok {
			// The mirror of the non-string "directory" case, one notch softer: the
			// alias is optional, so a malformed one resolves like an absent one —
			// but the caller thinks they named a workspace, so it is reported.
			return directoryArg{
				Source: dirSourceDaemonCwd,
				Warning: fmt.Sprintf("the \"cwd\" alias was not a string (got %T) and was IGNORED — "+
					"this answer describes the daemon's own directory", raw),
			}
		}
		if cwd = strings.TrimSpace(cwd); cwd != "" {
			return newDirectoryArg(cwd, dirSourceCwdAlias)
		}
	}
	return directoryArg{Source: dirSourceDaemonCwd}
}

// newDirectoryArg labels a non-blank directory, downgrading absoluteSource to
// dirSourceRelativePath when the path is not absolute. The value is kept either
// way: reads answer from it (leniently, via the daemon-cwd resolution), and
// mem_current_project reports the ORIGINAL verbatim in cwd_input so the caller
// can see what the daemon did with what they sent.
//
// "Not absolute" is decided by normalizeHostPath, not by filepath.IsAbs alone,
// because on Windows IsAbs gets two real shapes wrong (see hostpath.go):
//
//   - "/c/GitLab/engram" — a Git Bash path, which IsAbs calls relative. It is
//     nothing of the sort: it names a specific directory on this machine, and
//     labelling it relative both refused every write and explained the refusal
//     with a sentence about the daemon's working directory that had nothing to
//     do with what went wrong. It is translated when the drive exists, and when
//     it cannot be it stays refused — with the hint that names the shape.
//   - "\\?\C:\GitLab\engram" — an extended-length path, which IsAbs correctly
//     calls absolute and which then travels on in a spelling that compares
//     unequal to every other reference to the same directory. The prefix is
//     stripped here, once, at the door.
func newDirectoryArg(dir, absoluteSource string) directoryArg {
	resolved := normalizeHostPath(dir)
	if !resolved.Absolute {
		return directoryArg{Directory: dir, Input: dir, Source: dirSourceRelativePath, Relative: true}
	}
	arg := directoryArg{Directory: resolved.Path, Input: dir, Source: absoluteSource}
	if resolved.Translated {
		arg.Source = dirSourceTranslatedPosix
		arg.Warning = fmt.Sprintf("%q is a Git Bash/MSYS path and was translated to %q — "+
			"this answer is about that directory", dir, resolved.Path)
	}
	return arg
}

// toolError renders a dirSourceInvalid directoryArg as the MCP tool error the
// calling tool returns. mem_current_project is the one caller that does NOT use
// it: it never errors, and reports the same condition in its envelope instead.
func (d directoryArg) toolError(tool string) *mcp.CallToolResult {
	return mcp.NewToolResultError(tool + ": " + d.Err.Error())
}

// relativeError renders a relative directory as the refusal a WRITE tool
// returns. Reads stay lenient (they answer from whatever the daemon's cwd makes
// of it); a write would file a memory under a project nobody chose, and the one
// thing the caller can do about it is spell the path out or name the project.
func (d directoryArg) relativeError(tool string) *mcp.CallToolResult {
	if hint, ok := d.gitBashHint(); ok {
		return mcp.NewToolResultError(fmt.Sprintf("%s: directory %s", tool, hint))
	}
	return mcp.NewToolResultError(fmt.Sprintf("%s: directory %q is not absolute — %s", tool, d.Directory, relativeDirectoryHint))
}

// gitBashHint returns the Git Bash wording when that is what this directory is,
// and ok=false when the generic relative-path sentence is the right one.
//
// The distinction is the whole point: "pass an absolute path" is unactionable
// advice for someone who just passed what their shell calls an absolute path.
// Only reachable for a path that could NOT be translated — an unmounted drive,
// or a leading slash that names no drive at all ("/repos/x") — since a
// translated one is absolute and never refused.
func (d directoryArg) gitBashHint() (string, bool) {
	if runtime.GOOS != "windows" || !looksLikeGitBashPath(d.Directory) {
		return "", false
	}
	return gitBashDirectoryHint(d.Directory), true
}

// daemonCwdError is the write-tool refusal for dirSourceDaemonCwd: no
// "directory"/"cwd" argument reached the daemon, so the project would be
// detected from the SHARED daemon's own working directory rather than the
// caller's — the resident daemon now starts in its config directory
// (spawnWorkingDir), so this used to file the memory under project "engram"
// for every caller that forgot to send one. The remedy is the same one
// missingDirectoryError and relativeError already teach: pass directory (or
// "cwd") explicitly, or name the project.
func (d directoryArg) daemonCwdError(tool string) *mcp.CallToolResult {
	return mcp.NewToolResultError(fmt.Sprintf(
		"%s: no directory reached the daemon, so this write would be filed under the DAEMON's own working "+
			"directory rather than yours — pass directory (or \"cwd\") explicitly, or pass project explicitly", tool))
}

// missingDirectoryError is the write-tool refusal for a directory that is not
// on this machine. Detection derives a basename from any string, so without it
// a typo'd path silently CREATES a project — the flag mem_current_project
// reports as writes_blocked with project_source="missing_directory".
func missingDirectoryError(tool, dir string) *mcp.CallToolResult {
	return mcp.NewToolResultError(fmt.Sprintf(
		"%s: the resolved directory %q does not exist (or is not a directory), so any project name would be "+
			"invented from its basename — pass a real directory or an explicit project", tool, dir))
}

// directoryExists reports whether dir is present and is a directory. The empty
// string (the daemon could not read its own cwd) is NOT treated as missing:
// DetectProjectFull reads it as ".", which is the historic daemon-cwd answer.
// In practice this branch is now unreachable from the write-tool callers below
// — resolveSaveProject and handleSessionStart both refuse dirSourceDaemonCwd
// (the only source that resolves to "") before calling this — but it stays
// lenient here too, since a read tool reaching an empty dir must still answer
// rather than error.
func directoryExists(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return true
	}
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// directoryAwareTools names the MCP tools whose handlers resolve the project
// from a directory (resolveProjectDir → resolveReadProject / resolveSaveProject
// / handleSessionStart). It is the single source of truth shared by the daemon
// (which declares the optional "directory" argument on exactly these tools'
// schemas, see registerTools) and by the `engram connect` bridge running in the
// CLIENT process (which injects the client's working directory into exactly
// these tools/call frames, see injectClientDirectory in connect.go).
//
// Tools absent from this map never see a "directory" argument on the wire —
// their project is either irrelevant (mem_get_observation, mem_update,
// mem_judge, …) or derived from the data itself (mem_similar reads the source
// row's project).
var directoryAwareTools = map[string]bool{
	"mem_current_project": true,
	"mem_doctor":          true,
	"mem_session_start":   true,
	"mem_session_summary": true,
	"mem_save":            true,
	"mem_save_prompt":     true,
	"mem_search":          true,
	"mem_context":         true,
	"mem_review":          true,
}

// resolveProjectDir returns the directory that project detection must run
// against, applying the precedence every tool shares:
//
//  1. The caller-supplied "directory" argument, when non-empty. `engram connect`
//     injects the CLIENT process's working directory here (see connect.go) —
//     the daemon is SHARED and typically resident, so its own cwd is whatever
//     directory the autostart/tray happened to run from (on Windows commonly
//     C:\Windows\system32), never the repo the agent is working in.
//  2. The daemon's own working directory. This is the correct answer for a
//     per-client `engram daemon --transport stdio`, which the MCP client spawns
//     in the project directory itself.
//
// The result is ABSOLUTE and Clean: detection's own basename fallback reads
// filepath.Base(dir) verbatim, so a relative "." or "./repo" — which an agent
// filling in the "cwd" alias will write sooner or later — would otherwise
// resolve to the project "unknown" or "repo" while every neighbouring tool
// answered from a different name. Abs failing (an unreadable cwd) leaves the
// input untouched rather than inventing a path.
//
// A "" return (cwd unavailable) is passed through to DetectProjectFull, which
// treats it as ".".
func resolveProjectDir(directory string) string {
	dir := strings.TrimSpace(directory)
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if dir == "" {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs // filepath.Abs Cleans its result
	}
	return filepath.Clean(dir)
}

// resolveReadProject resolves the project for a READ tool call. Unlike write
// tools, read tools are LENIENT: no hard errors on ambiguous or invalid config.
// The policy mirrors the legacy predecessor's handleSearch/handleContext
// (REQ-310 lenient path):
//
//  1. Explicit project argument wins (normalized, used as-is — no store lookup).
//  2. Detect from the resolved directory (forwarded "directory" argument, else
//     the daemon's cwd — see resolveProjectDir) via DetectProjectFull.
//  3. On ANY detection error (ambiguous, invalid config, no .git, etc.) fall
//     back to the dir basename via DetectProject. Read tools never return a
//     project-resolution error to the agent.
//
// This contrasts with write tools (resolveSaveProject) which hard-error on
// ErrInvalidConfig and ErrAmbiguousProject.
func resolveReadProject(explicitProject, directory string) string {
	if strings.TrimSpace(explicitProject) != "" {
		return strings.TrimSpace(explicitProject)
	}
	dir := resolveProjectDir(directory)
	det := projectpkg.DetectProjectFull(dir)
	if det.Error != nil {
		// Lenient: fall back to basename, never error.
		return projectpkg.DetectProject(dir)
	}
	return det.Project
}
