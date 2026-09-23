package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ── state files ─────────────────────────────────────────────────────────────

// The two per-session markers, named once so a typo cannot silently create a
// third kind that nothing ever reads.
const (
	// hookStateToolsLoaded marks that the first prompt of a session has been
	// seen; its modification time is also how the nudge measures session age.
	hookStateToolsLoaded = "tools-loaded"
	// hookStateLastNudge marks when the save reminder last fired.
	hookStateLastNudge = "last-nudge"
	// hookStateProject caches the project the session's workspace resolved to
	// (see hookCacheProject). Unlike the two markers above it has content.
	hookStateProject = "project"
)

// hookStateDir returns the directory the per-session markers live in:
// <UserCacheDir>/engram/hooks, created 0700.
//
// NOT os.TempDir. These files are scratch, but they are scratch that decides
// behaviour — whether the bootstrap has fired, when the session started
// talking, when the nudge last spoke — and the system temp directory is
// world-readable, world-WRITABLE on Unix, and swept by tools that have no idea
// what they are deleting. A marker another user can create is a marker another
// user can use to silence someone else's reminders, and the whole tree was a
// flat list of engram-hook-* files in a directory shared with every other
// program on the machine. The user cache directory is per-user, 0700 here, and
// the documented home for exactly this: regenerable state that must survive a
// crashed agent (so not memory) without living in the user's config.
//
// A failure to resolve or create it falls back to os.TempDir — the old home.
// Degrading a debounce is acceptable; refusing to run a hook is not.
//
// The MkdirAll runs ONCE per directory: a hook run calls this for every marker
// it touches (claim, age, clear — twice each for the two kinds), and a process
// that has already created the directory has nothing to learn from doing it
// again. Keyed on the resolved path rather than a bare sync.Once so the state
// directory can still move within one process, which is exactly what the tests
// do when they isolate it per test — and guarded by a mutex because the claim
// it feeds is the one thing here that races by design.
var hookStateDirCreated struct {
	sync.Mutex
	dir string
}

func hookStateDir() string {
	base, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(base) == "" {
		return os.TempDir()
	}
	dir := filepath.Join(base, "engram", "hooks")

	hookStateDirCreated.Lock()
	defer hookStateDirCreated.Unlock()
	if hookStateDirCreated.dir == dir {
		return dir
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "engram hook: cannot use %s (%v); falling back to the temp directory\n", dir, err)
		return os.TempDir()
	}
	hookStateDirCreated.dir = dir
	return dir
}

// hookStateFile returns the path of a per-session marker file. The session id
// is HASHED rather than embedded: it is host-supplied text that ends up in a
// filesystem path, and a hash is both traversal-proof and fixed-length on hosts
// with short path limits. The tradeoff — you cannot eyeball which session a
// file belongs to — costs nothing, since nothing reads these but this binary.
func hookStateFile(sessionID, kind string) string {
	return filepath.Join(hookStateDir(), "engram-hook-"+hookSessionHashHex(sessionID)+"-"+kind)
}

// hookSessionHashHex is the session-id hash hookStateFile keys every marker
// filename on. Shared (rather than inlined twice) so hookClearOccurrenceMarkers
// — which has to find a session's markers by NAME, not by a path it already
// has — can never compute a different prefix than hookStateFile does.
func hookSessionHashHex(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:8])
}

// hookClearState removes the fixed per-session files (both markers and the
// cached project), plus every per-occurrence dedup marker hookClaimOccurrence
// left behind for it.
//
// It runs at session-start/post-compaction and at session-end, for two
// different reasons that happen to want the same thing. A `--resume` (and a
// compaction, which fires the same hook) REUSES the session id: the model's
// context is new, so the bootstrap has to fire again, and the age clock the
// nudge reads has to start from now rather than from whenever this id first
// spoke — days ago, on a machine that has since been rebooted. At session-end
// it is plain hygiene: without it the directory grows one pair of files, plus
// one occurrence marker per distinct prompt/report, per session, forever.
func hookClearState(sessionID string) {
	if strings.TrimSpace(sessionID) == "" {
		return
	}
	for _, kind := range []string{hookStateToolsLoaded, hookStateLastNudge, hookStateProject} {
		if err := os.Remove(hookStateFile(sessionID, kind)); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "engram hook: could not clear session state: %v\n", err)
		}
	}
	hookClearOccurrenceMarkers(sessionID)
}

// ── duplicate-install dedup (FUP-003) ───────────────────────────────────────
//
// Installing BOTH the Claude Code plugin (plugin/claude-code/hooks/hooks.json)
// and `engram setup hooks` registers the SAME commands twice, so the host fires
// every event through both — two independent `engram hook <event>` processes,
// each carrying the identical payload. Without a guard that means a duplicate
// mem_save_prompt for every prompt and a duplicate mem_save for every subagent
// report. The functions below make the SECOND of those two processes a no-op
// for its save, using the exact O_CREATE|O_EXCL claim hookClaimState already
// makes for the first-prompt bootstrap marker — just keyed on the occurrence
// (session, event, payload hash) instead of on the session alone.

// hookOccurrenceMarkerFile is hookClaimOccurrence's marker path: the same
// session hash hookStateFile always uses, plus a short hash of event+payload so
// two DIFFERENT prompts (or reports) in one session claim different markers —
// only the SAME occurrence arriving twice must collide.
func hookOccurrenceMarkerFile(sessionID, event, payload string) string {
	sum := sha256.Sum256([]byte(event + "\x00" + payload))
	return hookStateFile(sessionID, "occ-"+hex.EncodeToString(sum[:8]))
}

// hookOccurrenceDedupWindow bounds how long an occurrence marker actually
// dedups its own (session, event, payload) key. A double hook install (the
// plugin AND `engram setup hooks` both active) fires both processes within
// milliseconds of each other — that is the delivery this guards against. A
// user who submits the IDENTICAL text again later in the same session (a
// retyped "continue", a re-sent report) is a new occurrence that happens to
// hash the same, and must still be saved; without a window it would stay
// blocked for the full hookOccurrenceMarkerTTL (24h) or until session-end.
// 30s comfortably covers the double-install race — both processes launch and
// finish well under a second — while treating anything a human could
// plausibly retype as intentional.
const hookOccurrenceDedupWindow = 30 * time.Second

// hookClaimOccurrence reports whether THIS call should proceed with the save
// for event's payload in session id — true either because no marker exists yet
// (a genuinely new occurrence) or because the existing one is older than
// hookOccurrenceDedupWindow (not a near-simultaneous duplicate; see above).
// Call it immediately before the save it guards — not earlier — so a call that
// never reaches the save (an unresolvable project, a closed budget) never
// burns or refreshes the claim for an occurrence nothing actually saved.
//
// An empty session id claims unconditionally (no dedup, not a failure): the
// marker's session component is a hash, so an unnamed session would share ONE
// marker across every hook on the machine, and an unrelated second occurrence
// would then find the first one's marker and be wrongly skipped — worse than
// the duplicate this exists to prevent.
//
// Every OTHER failure to create the marker (an unwritable state directory) is
// fail-open by construction: it is hookClaimState's own contract, reused as-is,
// and it is the right one here too — a save that is not deduped is strictly
// better than one silently dropped.
func hookClaimOccurrence(sessionID, event, payload string) bool {
	if strings.TrimSpace(sessionID) == "" {
		return true
	}
	hookSweepStaleOccurrenceMarkers()

	path := hookOccurrenceMarkerFile(sessionID, event, payload)
	if hookClaimState(path) {
		return true
	}
	// A marker already exists. Outside the dedup window this is not a
	// near-simultaneous duplicate delivery — refresh it (so the window slides
	// with each genuine resave, the same way hookTouchState refreshes the nudge
	// cooldown) and let the save through. !ok (the marker vanished between the
	// two calls above) is the same "nothing here to collide with" case.
	if age, ok := hookStateAge(path); !ok || age >= hookOccurrenceDedupWindow {
		hookTouchState(path)
		return true
	}
	return false
}

// hookClearOccurrenceMarkers removes every occurrence marker hookClaimOccurrence
// left for sessionID. Unlike the two fixed markers hookClearState also removes,
// an occurrence marker has no fixed name — one is minted per distinct
// prompt/report — so they are found by LISTING the state directory for the
// session's hash prefix rather than removed by a path this function already
// knows.
func hookClearOccurrenceMarkers(sessionID string) {
	prefix := "engram-hook-" + hookSessionHashHex(sessionID) + "-occ-"

	entries, err := os.ReadDir(hookStateDir())
	if err != nil {
		return // best-effort — the TTL sweep below is the backstop
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		if err := os.Remove(filepath.Join(hookStateDir(), entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "engram hook: could not clear occurrence marker: %v\n", err)
		}
	}
}

// hookOccurrenceMarkerTTL bounds how long an occurrence marker survives a
// session that never reaches session-end (a crashed agent, a host that skips
// the event) — hookClearOccurrenceMarkers' own cleanup then never runs.
const hookOccurrenceMarkerTTL = 24 * time.Hour

// hookOccurrenceSweepCooldown bounds how often hookSweepStaleOccurrenceMarkers
// actually walks the state directory, so the common case (nothing due) costs
// one Stat on the cooldown marker, not a ReadDir on every hook run.
const hookOccurrenceSweepCooldown = time.Hour

// hookOccurrenceSweepMarkerFile is the machine-wide (not per-session) cooldown
// clock for the sweep — there is exactly one sweep for the whole machine, so it
// reuses hookStateFile with an empty session id rather than inventing a second
// naming scheme.
func hookOccurrenceSweepMarkerFile() string {
	return hookStateFile("", "occurrence-sweep")
}

// hookSweepStaleOccurrenceMarkers removes occurrence markers older than
// hookOccurrenceMarkerTTL — the backstop for a session whose hookClearState
// cleanup never runs. Best-effort throughout: a failed sweep just retries at
// the next cooldown, and it must never be the reason a hook fails to save.
func hookSweepStaleOccurrenceMarkers() {
	cooldown := hookOccurrenceSweepMarkerFile()
	if age, ok := hookStateAge(cooldown); ok && age < hookOccurrenceSweepCooldown {
		return
	}
	hookTouchState(cooldown)

	entries, err := os.ReadDir(hookStateDir())
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		if entry.IsDir() || !strings.Contains(entry.Name(), "-occ-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) >= hookOccurrenceMarkerTTL {
			_ = os.Remove(filepath.Join(hookStateDir(), entry.Name()))
		}
	}
}

// hookClaimState creates the marker and reports whether THIS call created it —
// the atomic form of "is this the first prompt of the session?".
//
// O_EXCL is what makes it atomic. The old exists-then-create pair let two
// prompts submitted in quick succession both read "absent" and both claim to be
// the first, which injects the bootstrap twice and resets the session's age
// clock underneath the nudge.
//
// A creation failure that is NOT "already exists" (an unwritable state
// directory) reports true: with no marker to read, the choice is between
// injecting the bootstrap on every prompt and never injecting it at all, and
// the first at least leaves a working session.
func hookClaimState(path string) bool {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		_ = f.Close()
		return true
	}
	if errors.Is(err, os.ErrExist) {
		return false
	}
	fmt.Fprintf(os.Stderr, "engram hook: could not record session state at %s: %v\n", path, err)
	return true
}

// hookStateAge returns how long ago the marker was last written. A missing or
// unreadable marker reports ok=false, which every caller treats as "do not act".
func hookStateAge(path string) (time.Duration, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return time.Since(info.ModTime()), true
}

// hookTouchState creates or refreshes the marker. Failures are ignored: an
// unwritable state directory degrades the nudge debounce, it does not break the
// hook.
//
// The create is O_EXCL and an "already exists" is retried as a Chtimes, so two
// hooks racing on the same marker cannot have one TRUNCATE the file the other
// is using as a clock.
func hookTouchState(path string) {
	now := time.Now()
	if err := os.Chtimes(path, now, now); err == nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		_ = f.Close()
		return
	}
	if errors.Is(err, os.ErrExist) {
		_ = os.Chtimes(path, now, now)
	}
}
