package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	projectpkg "github.com/mariesqu/engram/internal/project"
)

// ── project resolution ──────────────────────────────────────────────────────

// hookResolveProject asks the daemon which project this directory resolves to,
// through the very tool agents are told to call first (mem_current_project), so
// a hook and the agent it is wrapping can never disagree about the name.
//
// It returns "" — meaning "do not claim a project" — whenever the answer is not
// trustworthy: a directory that does not exist, a resolution error, or a
// project every write tool would refuse anyway. A hook that guessed here would
// register sessions and file observations under a name nothing else uses, which
// is the exact failure the probe was built to expose.
//
// An EMPTY cwd is refused without asking. The host is the only thing that knows
// where the session is, so a payload without one leaves nothing to resolve —
// and the daemon's answer in that case describes the DAEMON's own working
// directory, which is a real, existing, confidently-reported project that has
// nothing to do with the session. Filing a subagent report or a prompt there is
// worse than not filing it: it is indistinguishable from real work. The
// directory_source check below catches the same answer arriving the long way
// round (a blank-after-trim cwd, or a relative one resolved against the daemon).
func hookResolveProject(ctx context.Context, client *mcpBridge, cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		fmt.Fprintf(os.Stderr, "engram hook: the hook payload carried no cwd, so there is no workspace to resolve\n")
		return ""
	}
	out, err := client.callTool(ctx, "mem_current_project", map[string]any{"directory": cwd})
	if err != nil {
		fmt.Fprintf(os.Stderr, "engram hook: resolve project: %v\n", err)
		return ""
	}
	var env struct {
		Project         string `json:"project"`
		Source          string `json:"project_source"`
		DirectorySource string `json:"directory_source"`
		ErrorHint       string `json:"error_hint"`
		WritesBlocked   bool   `json:"writes_blocked"`
		DirExists       bool   `json:"directory_exists"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		fmt.Fprintf(os.Stderr, "engram hook: mem_current_project returned non-JSON: %v\n", err)
		return ""
	}
	// Deliberately REDUNDANT with the writes_blocked check beside it, and worth
	// keeping anyway. mem_current_project blocks writes for both of these
	// sources, so neither term can decide the outcome alone — a mutation that
	// deletes either one leaves every test green. What they buy is independence:
	// writes_blocked is a POLICY answer that composes several conditions (an
	// omitted project, a missing directory, a relative path, no directory at
	// all) and could reasonably stop covering one of them, while this names the
	// two answers a hook must never act on no matter what policy says — the
	// ones that describe the DAEMON's directory rather than the session's. A
	// hook files memories nobody reviews; the cheap belt beside the braces is
	// the right trade.
	answersAboutTheDaemon := env.DirectorySource == dirSourceDaemonCwd || env.DirectorySource == dirSourceRelativePath
	if strings.TrimSpace(env.Project) == "" || env.ErrorHint != "" || env.WritesBlocked || !env.DirExists ||
		answersAboutTheDaemon {
		fmt.Fprintf(os.Stderr, "engram hook: no usable project for %q (source=%q, directory_source=%q, writes_blocked=%v)\n",
			cwd, env.Source, env.DirectorySource, env.WritesBlocked)
		return ""
	}
	return env.Project
}

// hookProject is the project a hook's work belongs to: the payload's cwd first,
// and the SESSION's own registration as the fallback — but ONLY when the
// payload carried NO cwd at all.
//
// The fallback is not a second guess at the same question — it is the same
// observation arriving by another road. session-start registered this id WITH
// the directory the host reported, so the session row holds a cwd that came
// from the host, for this very session, at a moment when the host did supply
// one. A later event from the same session that arrives with an EMPTY cwd
// (Codex omits it for some subagent shapes, and a wrapper script can drop it
// for any of them) is not a session we cannot place: it is one we already
// placed.
//
// A cwd that IS present but REFUSED — relative, an ambiguous monorepo parent,
// a directory that no longer exists — is a different answer, and the fallback
// must NOT catch it: the host told us exactly where the session is and the
// daemon said no, so falling back to the session's registration would silently
// override that refusal with a guess. Only strings.TrimSpace(in.CWD) == ""
// reaches hookProjectFromSession; hookResolveProject returning "" for a
// non-empty cwd stops here with no save.
func hookProject(ctx context.Context, client *mcpBridge, in hookInput) string {
	if project := hookResolveProject(ctx, client, in.CWD); project != "" {
		return project
	}
	if strings.TrimSpace(in.CWD) != "" {
		return ""
	}
	return hookProjectFromSession(ctx, client.dir, in.SessionID)
}

// hookProjectFromSession asks the daemon which project a session id was
// registered under (GET /api/v1/sessions/{id}).
//
// The control API, not an MCP tool, for the same reason hookLastSaveAge reads
// it there: no tool answers "which project is session X filed under?" in a
// machine-readable shape, and the session row is exactly the kind of local
// bookkeeping the control plane exists for. Every failure — no daemon, an
// unregistered id, a store with no session support (501), an empty project —
// returns "", which leaves the caller exactly where it was: not saving.
func hookProjectFromSession(ctx context.Context, dir, sessionID string) string {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return ""
	}
	client, err := NewControlClient(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "engram hook: session lookup: %v\n", err)
		return ""
	}
	// Same bound hookLastSaveAge applies, for the same reason: the client's 5s
	// default would outlive a 200ms prompt budget many times over, and the
	// context is what actually enforces the deadline across the 401 retry.
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ""
		}
		client.http.Timeout = remaining
	}

	var session struct {
		Project string `json:"project"`
	}
	// urlQueryEscape over-escapes for a path segment (it percent-encodes "/"),
	// which is the safe direction: a session id is host-supplied text, and the
	// worst case of over-escaping is a 404 that leaves the caller where it was.
	if err := client.GetContext(ctx, "/api/v1/sessions/"+urlQueryEscape(id), &session); err != nil {
		fmt.Fprintf(os.Stderr, "engram hook: session %q is not registered under a project (%v); "+
			"nothing to fall back to\n", id, err)
		return ""
	}
	project := strings.TrimSpace(session.Project)
	if project != "" {
		fmt.Fprintf(os.Stderr, "engram hook: the payload carried no usable cwd; using project %q "+
			"from the session's own registration\n", project)
	}
	return project
}

// ── per-session project cache ───────────────────────────────────────────────
//
// Resolving a workspace is the one expensive step on the prompt hook's hot
// path: mem_current_project makes the daemon run `git` twice (remote, then
// toplevel — internal/project/detect.go), and on Windows two process spawns
// measured 65–120ms of a 200ms budget on an idle machine, and blew it outright
// under load, so the prompt was silently not captured. session-start (8s
// budget) has already paid for that answer once, so it is cached per session
// and user-prompt-submit reads it back instead of asking again.
//
// The answer is NOT fixed for the session, though: editing or creating a
// .engram/config.json, checking out a branch that carries a different one, or
// changing the git remote all move the same cwd to a different project — and
// mem_save_prompt receives the cached name as an EXPLICIT project, which skips
// detection, so a stale hit misfiles the prompt silently. Every hit is
// therefore revalidated against project.DetectionFingerprint, the stat-level
// summary of every input detection reads (no git spawn), and bounded by
// hookProjectCacheTTL for the inputs a stat cannot see.

// hookProjectCacheTTL is the defence-in-depth backstop: whatever the
// fingerprint misses (a global git config, an includeIf'd file) is re-asked
// at least this often.
const hookProjectCacheTTL = 10 * time.Minute

// hookNow and hookDetectionFingerprint are the cache's clock and validator,
// swappable so tests can age a cache and inject a stat failure.
var (
	hookNow                  = time.Now
	hookDetectionFingerprint = projectpkg.DetectionFingerprint
)

// hookCachedProjectFile is the cache's on-disk shape. The cwd it was resolved
// FOR is stored beside the answer: a payload whose cwd differs (the user cd'd,
// or a host reports a subdirectory) is a different question, and gets a live
// resolution rather than a stale answer.
type hookCachedProjectFile struct {
	CWD         string    `json:"cwd"`
	Project     string    `json:"project"`
	Fingerprint string    `json:"fingerprint"`
	ResolvedAt  time.Time `json:"resolved_at"`
}

// hookProjectFingerprint is the detection fingerprint of cwd, or "" when it
// cannot be computed — which makes the answer uncacheable, never "unchanged".
func hookProjectFingerprint(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return ""
	}
	fp, err := hookDetectionFingerprint(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "engram hook: project cache: %v\n", err)
		return ""
	}
	return fp
}

// hookCacheProject records that cwd resolved to project for sessionID, with
// the fingerprint the caller took BEFORE resolving: an input that changes
// while the daemon is answering then mismatches on the next read, rather than
// being blessed by a fingerprint taken afterwards.
//
// Only a USABLE answer with a fingerprint is cached — an empty project means
// the workspace was refused, and caching the refusal would outlive whatever
// made it. Failures are ignored: a missing cache costs the next prompt one
// live resolution, nothing more.
//
// Written to a temp file and renamed, so a concurrent reader (the prompt hook
// of a double install) sees the old content or the new, never half a file.
func hookCacheProject(sessionID, cwd, project, fingerprint string) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(cwd) == "" ||
		strings.TrimSpace(project) == "" || fingerprint == "" {
		return
	}
	body, err := json.Marshal(hookCachedProjectFile{
		CWD: cwd, Project: project, Fingerprint: fingerprint, ResolvedAt: hookNow(),
	})
	if err != nil {
		return
	}
	path := hookStateFile(sessionID, hookStateProject)
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return
	}
	_, writeErr := tmp.Write(body)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(tmp.Name())
		return
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
	}
}

// hookCachedProject returns the project cached for sessionID when it was
// resolved for exactly this cwd, within hookProjectCacheTTL, and every
// detection input still fingerprints the same; "" otherwise — a miss, which
// the caller answers with a live resolution.
//
// fingerprint is the CURRENT fingerprint when this call computed one, so a
// caller that misses can cache its live answer without re-statting.
func hookCachedProject(sessionID, cwd string) (project, fingerprint string) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(cwd) == "" {
		return "", ""
	}
	body, err := os.ReadFile(hookStateFile(sessionID, hookStateProject))
	if err != nil {
		return "", ""
	}
	var cached hookCachedProjectFile
	if err := json.Unmarshal(body, &cached); err != nil || cached.CWD != cwd || cached.Fingerprint == "" {
		return "", ""
	}
	if age := hookNow().Sub(cached.ResolvedAt); age < 0 || age >= hookProjectCacheTTL {
		return "", ""
	}
	fingerprint = hookProjectFingerprint(cwd)
	if fingerprint == "" || fingerprint != cached.Fingerprint {
		return "", fingerprint
	}
	return strings.TrimSpace(cached.Project), fingerprint
}
