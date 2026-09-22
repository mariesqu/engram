package diagnostic

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mariesqu/engram/internal/localstore"
	projectpkg "github.com/mariesqu/engram/internal/project"
)

// Check codes. They are part of the tool's contract: an agent passes one to
// mem_doctor's check argument, and a report quotes them back.
const (
	CheckAmbiguousActiveSessions         = "ambiguous_active_sessions"
	CheckOrphanedObservationSession      = "orphaned_observation_session"
	CheckProjectPolicyUnknown            = "project_policy_unknown"
	CheckSessionProjectDirectoryMismatch = "session_project_directory_mismatch"
	CheckSQLiteLockContention            = "sqlite_lock_contention"
	CheckStaleReviewBacklog              = "stale_review_backlog"
	CheckSyncBacklog                     = "sync_backlog"
	CheckParkedMutations                 = "parked_mutations"
)

// ReasonUnregisteredSessionSaves is a REASON code, not a check code — it names
// a FINDING of the orphaned-session check, so it is deliberately not something
// mem_doctor's check argument accepts. It describes the same shape of data
// (live observations whose session id has no row in sessions) for the opposite
// reason: the store's own documented default rather than a session that
// vanished.
const ReasonUnregisteredSessionSaves = "unregistered_session_saves"

// Thresholds. They are constants rather than configuration because a threshold
// nobody tunes is a threshold nobody has to explain, and every one of these is
// about the shape of the problem, not about taste.
const (
	// staleReviewWarnRatio — the share of a project's live memories that may be
	// due for review before the backlog itself is the finding. Below it, review
	// is working as designed; above it, the lifecycle signal has stopped meaning
	// anything because everything is stale.
	staleReviewWarnRatio = 0.5
	// staleReviewWarnMin — a floor, so a project with 3 memories and 2 stale ones
	// does not trip a warning about a "backlog".
	staleReviewWarnMin = 20
	// syncBacklogWarnAge — how long the oldest unpushed mutation may sit before
	// the backlog stops looking like a sync that has not run yet and starts
	// looking like one that cannot.
	syncBacklogWarnAge = 6 * time.Hour
	// syncBacklogWarnCount — the same signal by volume, for a node that writes
	// faster than it syncs.
	syncBacklogWarnCount = 500
)

// ─── orphaned_observation_session ────────────────────────────────────────────

// OrphanedObservationSessionCheck finds live observations whose session id has
// no row in sessions.
//
// It reports TWO conditions that look identical in SQL and mean opposite
// things. An observation naming a session id nobody registered is a warning:
// something created it, and whatever that was is gone. An observation naming
// "manual-save-{project}" is the store's own DEFAULT for a save made outside a
// tracked session — documented, expected, and previously reported as a warning
// on every single store that had ever seen a plain mem_save. That one is an
// informational note and does not move the check out of "ok".
type OrphanedObservationSessionCheck struct{}

func (OrphanedObservationSessionCheck) Code() string { return CheckOrphanedObservationSession }

func (c OrphanedObservationSessionCheck) Run(_ context.Context, scope Scope) (CheckResult, error) {
	split, err := scope.Store.OrphanedSessions(scope.Project)
	if err != nil {
		return CheckResult{}, err
	}
	refs, unregistered := split.Orphans, split.Unregistered

	findings := make([]Finding, 0, len(refs))
	for _, ref := range refs {
		findings = append(findings, Finding{
			CheckID:    c.Code(),
			Severity:   SeverityWarning,
			ReasonCode: CheckOrphanedObservationSession,
			Message: fmt.Sprintf("%d observation(s) reference session %q, which has no row in sessions.",
				ref.ObservationCount, ref.SessionID),
			Why: "Those observations can no longer be grouped under the session that produced them: mem_context reports sessions, " +
				"and a session that does not exist contributes nothing. The memories themselves are intact and searchable.",
			Evidence: mustJSON(ref),
			SafeNextStep: "If the session id is one you recognise, re-register it with mem_session_start (same id, explicit project) " +
				"to restore the grouping. Doctor does not create sessions: it cannot know when the work actually happened.",
			RequiresConfirmation: true,
		})
	}

	notes := unregisteredSaveNotes(c.Code(), unregistered)
	evidence := map[string]any{
		"orphaned_session_ids":     len(refs),
		"unregistered_session_ids": len(unregistered),
	}
	if len(findings) == 0 {
		return okWithNotes(c.Code(), evidence, notes), nil
	}
	return resultFromFindings(c.Code(), evidence, append(findings, notes...)), nil
}

// unregisteredSaveNotes folds the manual-save rows into at most ONE
// informational finding. One note per session id would print a line per project
// for a condition that is the same condition everywhere: "you are saving
// without registering a session".
func unregisteredSaveNotes(checkID string, refs []localstore.OrphanedSessionRef) []Finding {
	if len(refs) == 0 {
		return nil
	}
	total := 0
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		total += ref.ObservationCount
		ids = append(ids, ref.SessionID)
	}
	sort.Strings(ids)
	return []Finding{{
		CheckID:    checkID,
		Severity:   SeverityInfo,
		ReasonCode: ReasonUnregisteredSessionSaves,
		Message: fmt.Sprintf("%d observation(s) were saved without a registered session; call mem_session_start at session start.",
			total),
		Why: "A save with no session_id is filed under the default \"" + localstore.ManualSaveSessionPrefix +
			"{project}\" id, which nothing registers. The memories are intact, searchable and synced — they simply " +
			"carry no session, so mem_context cannot group them with the work they came from.",
		Evidence: mustJSON(map[string]any{
			"observation_count": total,
			"session_ids":       ids,
		}),
		SafeNextStep: "Nothing is broken. To get the grouping, call mem_session_start at the beginning of a session " +
			"and pass its id to mem_save.",
	}}
}

// ─── session_project_directory_mismatch ──────────────────────────────────────

// SessionProjectDirectoryMismatchCheck compares each session's stored project
// against the project its directory resolves to today.
type SessionProjectDirectoryMismatchCheck struct{}

func (SessionProjectDirectoryMismatchCheck) Code() string {
	return CheckSessionProjectDirectoryMismatch
}

func (c SessionProjectDirectoryMismatchCheck) Run(_ context.Context, scope Scope) (CheckResult, error) {
	// scope.Sessions(), not Store.DiagnosticSessions: this check and
	// ambiguous_active_sessions both need every session row, and one report
	// should not read the sessions table twice.
	sessions, err := scope.Sessions()
	if err != nil {
		return CheckResult{}, err
	}

	cache := map[string]DetectedProject{}
	findings := make([]Finding, 0)
	evaluated := 0
	for _, sess := range sessions {
		detected, ok := detectSessionDirectoryProject(scope, cache, sess.Directory)
		if !ok {
			// A directory that is gone, or that resolves only to its own basename,
			// is not evidence of anything: the session may predate a rename, and a
			// basename is a guess in the first place. Skipping is the finding's
			// absence, not its suppression.
			continue
		}
		evaluated++
		stored := normalizeProject(sess.Project)
		if stored == "" || stored == detected.Project {
			continue
		}
		findings = append(findings, Finding{
			CheckID:    c.Code(),
			Severity:   SeverityWarning,
			ReasonCode: CheckSessionProjectDirectoryMismatch,
			Message: fmt.Sprintf("Session %q is filed under %q, but its directory resolves to %q.",
				sess.ID, stored, detected.Project),
			Why: "Reads and writes that resolve through this session land under a different project than the same directory resolves to now, " +
				"so half the work ends up in each — the drift that mem_merge_projects exists to clean up.",
			Evidence: mustJSON(map[string]any{
				"session_id":               sess.ID,
				"session_project":          sess.Project,
				"directory":                sess.Directory,
				"directory_project":        detected.Project,
				"directory_project_source": detected.Source,
				"directory_project_path":   detected.Path,
			}),
			SafeNextStep: "Decide which name is canonical, then re-run mem_session_start with the same id and an explicit project " +
				"(it rewrites the session row), and fold the existing memories with mem_merge_projects. Both are deliberate calls; doctor makes neither.",
			RequiresConfirmation: true,
		})
	}
	return resultFromFindings(c.Code(), map[string]any{
		"sessions_seen":      len(sessions),
		"sessions_evaluated": evaluated,
	}, findings), nil
}

// detectSessionDirectoryProject resolves a session's directory to a project,
// memoized per run.
//
// Only DECLARED identities count: a config file, a git remote, or a git root.
// A dir_basename answer is exactly as much of a guess as the stored project it
// would be compared against, so pitting them against each other would report
// "mismatches" that are two guesses disagreeing.
func detectSessionDirectoryProject(scope Scope, cache map[string]DetectedProject, directory string) (DetectedProject, bool) {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		return DetectedProject{}, false
	}
	if cached, ok := cache[directory]; ok {
		return cached, cached.Project != ""
	}

	if scope.DetectProject != nil {
		detected, ok := scope.DetectProject(directory)
		cache[directory] = detected
		return detected, ok && detected.Project != ""
	}

	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		cache[directory] = DetectedProject{}
		return DetectedProject{}, false
	}
	res := projectpkg.DetectProjectFull(directory)
	detected := DetectedProject{}
	switch {
	case res.Error != nil:
	case res.Source == projectpkg.SourceConfig,
		res.Source == projectpkg.SourceGitRemote,
		res.Source == projectpkg.SourceGitRoot:
		detected = DetectedProject{Project: normalizeProject(res.Project), Source: res.Source, Path: res.Path}
	}
	cache[directory] = detected
	return detected, detected.Project != ""
}

// ─── ambiguous_active_sessions ───────────────────────────────────────────────

// AmbiguousActiveSessionsCheck finds two or more OPEN sessions sharing a
// project and directory.
type AmbiguousActiveSessionsCheck struct{}

func (AmbiguousActiveSessionsCheck) Code() string { return CheckAmbiguousActiveSessions }

func (c AmbiguousActiveSessionsCheck) Run(_ context.Context, scope Scope) (CheckResult, error) {
	sessions, err := scope.Sessions()
	if err != nil {
		return CheckResult{}, err
	}

	type key struct{ project, directory string }
	open := map[key][]string{}
	for _, sess := range sessions {
		if sess.EndedAt != nil {
			continue
		}
		project := normalizeProject(sess.Project)
		directory := strings.TrimSpace(sess.Directory)
		if project == "" || directory == "" {
			continue
		}
		k := key{project, directory}
		open[k] = append(open[k], sess.ID)
	}

	keys := make([]key, 0, len(open))
	for k := range open {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].project != keys[j].project {
			return keys[i].project < keys[j].project
		}
		return keys[i].directory < keys[j].directory
	})

	findings := make([]Finding, 0)
	for _, k := range keys {
		ids := open[k]
		if len(ids) < 2 {
			continue
		}
		sort.Strings(ids)
		findings = append(findings, Finding{
			CheckID:    c.Code(),
			Severity:   SeverityWarning,
			ReasonCode: CheckAmbiguousActiveSessions,
			Message: fmt.Sprintf("%d sessions are still open for project %q in the same directory.",
				len(ids), k.project),
			Why: "Open sessions are what mem_context reports as recent activity, and a session that never ended keeps claiming to be current. " +
				"Several at once usually means an agent host exited without firing its session-end hook.",
			Evidence: mustJSON(map[string]any{
				"project":     k.project,
				"directory":   k.directory,
				"session_ids": ids,
				"open_count":  len(ids),
			}),
			SafeNextStep: "Close the ones you recognise as finished with mem_session_end (ideally after mem_session_summary). " +
				"Doctor does not pick: it cannot tell which of them is the session you are in.",
			RequiresConfirmation: true,
		})
	}
	return resultFromFindings(c.Code(), map[string]any{"open_session_groups": len(open)}, findings), nil
}

// ─── project_policy_unknown ──────────────────────────────────────────────────

// ProjectPolicyUnknownCheck reports projects with memories but no explicit
// policy row, and only when central is configured.
type ProjectPolicyUnknownCheck struct{}

func (ProjectPolicyUnknownCheck) Code() string { return CheckProjectPolicyUnknown }

func (c ProjectPolicyUnknownCheck) Run(_ context.Context, scope Scope) (CheckResult, error) {
	if !scope.Store.CentralConfigured() {
		// Local-only node: the computed default IS local-only, nothing leaves the
		// machine, and an absent policy row means exactly what the operator wants.
		return okResult(c.Code(), map[string]any{"central_configured": false}), nil
	}

	projects, err := scope.Store.ProjectsWithoutPolicy(scope.Project)
	if err != nil {
		return CheckResult{}, err
	}
	if len(projects) == 0 {
		return okResult(c.Code(), map[string]any{"central_configured": true, "projects_without_policy": 0}), nil
	}

	finding := Finding{
		CheckID:    c.Code(),
		Severity:   SeverityInfo,
		ReasonCode: CheckProjectPolicyUnknown,
		Message: fmt.Sprintf("%d project(s) have memories but no explicit sync policy; with central configured they default to \"synced\".",
			len(projects)),
		Why: "The default is computed at read time, not stored, so these projects are being pushed to central on the strength of a default " +
			"nobody chose. That is correct for most projects and wrong for exactly the ones you would mind.",
		Evidence: mustJSON(map[string]any{"projects": projects, "computed_default": "synced"}),
		SafeNextStep: "Review the list and pin the ones that should stay here: " +
			"`engram projects policy <project> local-only` (or `omitted` to refuse capture entirely).",
	}
	return resultFromFindings(c.Code(), map[string]any{"projects_without_policy": len(projects)}, []Finding{finding}), nil
}

// ─── sqlite_lock_contention ──────────────────────────────────────────────────

// SQLiteLockContentionCheck probes the database for lock contention.
type SQLiteLockContentionCheck struct{}

func (SQLiteLockContentionCheck) Code() string { return CheckSQLiteLockContention }

func (c SQLiteLockContentionCheck) Run(ctx context.Context, scope Scope) (CheckResult, error) {
	snapshot, err := scope.Store.SQLiteLockSnapshot(ctx)
	if err != nil {
		// Reported as a finding rather than an error return: a probe that cannot
		// run is itself the most interesting thing this check has to say.
		return resultFromFindings(c.Code(), map[string]any{"probe": "failed"}, []Finding{{
			CheckID:      c.Code(),
			Severity:     SeverityError,
			ReasonCode:   "sqlite_lock_probe_failed",
			Message:      err.Error(),
			Why:          "Doctor could not read SQLite lock state, so contention can be neither confirmed nor ruled out.",
			Evidence:     mustJSON(map[string]any{"error": err.Error()}),
			SafeNextStep: "Close other engram processes (a second daemon, an open `engram memories` command) and re-run this check.",
		}}), nil
	}

	findings := make([]Finding, 0)
	if snapshot.CheckpointBusy != 0 {
		findings = append(findings, Finding{
			CheckID:    c.Code(),
			Severity:   SeverityWarning,
			ReasonCode: "sqlite_lock_contention_detected",
			Message:    "A WAL checkpoint could not complete: another connection is holding the database.",
			Why: "Writes queue behind that holder and fail once busy_timeout expires. The usual cause is a SECOND process on the same " +
				"database file — the single-owner rule the daemon enforces at startup only covers daemons.",
			Evidence:     mustJSON(snapshot),
			SafeNextStep: "Find the other holder (a stray `engram daemon`, a DB browser) and close it, then re-run this check.",
		})
	}
	if snapshot.BusyTimeoutMS <= 0 {
		findings = append(findings, Finding{
			CheckID:    c.Code(),
			Severity:   SeverityWarning,
			ReasonCode: "sqlite_busy_timeout_disabled",
			Message:    "busy_timeout is 0: a writer that meets a held lock fails immediately instead of waiting.",
			Why: "With no timeout, ordinary contention surfaces to the agent as a failed mem_save rather than as a call that took " +
				"a moment longer.",
			Evidence:     mustJSON(snapshot),
			SafeNextStep: "Restart the daemon so the store re-applies its connection pragmas; if it persists, the DSN is overriding them.",
		})
	}
	return resultFromFindings(c.Code(), snapshot, findings), nil
}

// ─── stale_review_backlog ────────────────────────────────────────────────────

// StaleReviewBacklogCheck reports when most of a project's memories are due for
// review.
type StaleReviewBacklogCheck struct{}

func (StaleReviewBacklogCheck) Code() string { return CheckStaleReviewBacklog }

func (c StaleReviewBacklogCheck) Run(_ context.Context, scope Scope) (CheckResult, error) {
	counts, err := scope.Store.CountByReviewStatus(scope.Project)
	if err != nil {
		return CheckResult{}, err
	}

	evidence := map[string]any{
		"total":        counts.Total,
		"active":       counts.Active,
		"needs_review": counts.NeedsReview,
		"expired":      counts.Expired,
	}
	stale := counts.NeedsReview + counts.Expired
	if stale < staleReviewWarnMin || counts.Total == 0 ||
		float64(stale)/float64(counts.Total) < staleReviewWarnRatio {
		return okResult(c.Code(), evidence), nil
	}

	finding := Finding{
		CheckID:    c.Code(),
		Severity:   SeverityWarning,
		ReasonCode: CheckStaleReviewBacklog,
		Message: fmt.Sprintf("%d of %d live memories are due for review or expired (%.0f%%).",
			stale, counts.Total, 100*float64(stale)/float64(counts.Total)),
		Why: "The lifecycle flag exists so an agent treats an old memory as something to verify rather than a fact. " +
			"Once most of the project is flagged, the flag stops distinguishing anything and gets ignored.",
		Evidence: mustJSON(evidence),
		SafeNextStep: "Work through them with mem_review(action=\"list\", status=\"needs_review\"), then mark_reviewed the ones you have " +
			"CONFIRMED against current code. Never mark in bulk: that is how a stale memory becomes a trusted one.",
		RequiresConfirmation: true,
	}
	return resultFromFindings(c.Code(), evidence, []Finding{finding}), nil
}

// ─── sync_backlog ────────────────────────────────────────────────────────────

// SyncBacklogCheck reports mutations waiting in the outbound push journal.
type SyncBacklogCheck struct{}

func (SyncBacklogCheck) Code() string { return CheckSyncBacklog }

func (c SyncBacklogCheck) Run(_ context.Context, scope Scope) (CheckResult, error) {
	if !scope.Store.CentralConfigured() {
		// Local-only: nothing is expected to drain, so a backlog is not one.
		return okResult(c.Code(), map[string]any{"central_configured": false}), nil
	}

	backlog, err := scope.Store.SyncBacklog()
	if err != nil {
		return CheckResult{}, err
	}
	evidence := map[string]any{
		"central_configured": true,
		"pending":            backlog.Pending,
	}
	if backlog.Pending == 0 {
		return okResult(c.Code(), evidence), nil
	}

	age := time.Duration(0)
	if !backlog.Oldest.IsZero() {
		age = scope.now().Sub(backlog.Oldest.UTC())
		evidence["oldest_occurred_at"] = backlog.Oldest.UTC().Format(time.RFC3339)
		evidence["oldest_age_seconds"] = int(age.Seconds())
	}

	severity := SeverityInfo
	message := fmt.Sprintf("%d mutation(s) are waiting to be pushed to central.", backlog.Pending)
	why := "A non-empty outbox between sync cycles is normal — the autosync loop drains it on its next tick."
	next := "Nothing, unless it keeps growing. `engram sync now` triggers a cycle immediately."
	if age >= syncBacklogWarnAge || backlog.Pending >= syncBacklogWarnCount {
		severity = SeverityWarning
		if age > 0 {
			message = fmt.Sprintf("%d mutation(s) are waiting to be pushed; the oldest has been queued for %s.",
				backlog.Pending, age.Round(time.Minute))
		}
		why = "A backlog this old is a sync that cannot complete, not one that has not run: these memories exist on this machine only, " +
			"and a lost disk loses them."
		next = "Run `engram sync now` and read the error it reports (`engram status` shows the last cycle's result). " +
			"Usual causes: an unreachable central URL, a revoked writer key."
	}

	finding := Finding{
		CheckID:      c.Code(),
		Severity:     severity,
		ReasonCode:   CheckSyncBacklog,
		Message:      message,
		Why:          why,
		Evidence:     mustJSON(evidence),
		SafeNextStep: next,
	}
	return resultFromFindings(c.Code(), evidence, []Finding{finding}), nil
}

// ─── parked_mutations ────────────────────────────────────────────────────────

// mutationIDPrefixLen bounds how much of a mutation_id (a 64-character SHA-256
// hex digest) a finding's evidence shows — enough to correlate with
// `engram sync retry`/`discard`'s own output and with sync_mutations directly,
// without printing the full identifier for what is, after all, an operator-
// facing report.
const mutationIDPrefixLen = 12

// ParkedMutationsCheck reports outbox entries the syncer has PARKED (see
// FUP-004b): central rejected them permanently, so DrainOutbox has stopped
// resending them — but they are not gone, and a project's writes behind them
// in the same sync_id's version chain stay stuck until an operator retries or
// discards each one (`engram sync retry`/`discard`). SyncBacklogCheck
// deliberately excludes these rows (see SyncBacklog's own doc comment): a
// parked entry is not "waiting for the next tick" the way a pending one is,
// and conflating the two would make a real backlog look like it is draining
// when it is actually stuck.
type ParkedMutationsCheck struct{}

func (ParkedMutationsCheck) Code() string { return CheckParkedMutations }

func (c ParkedMutationsCheck) Run(_ context.Context, scope Scope) (CheckResult, error) {
	if !scope.Store.CentralConfigured() {
		// Local-only: nothing is ever pushed, so nothing can be parked.
		return okResult(c.Code(), map[string]any{"central_configured": false}), nil
	}

	parked, err := scope.Store.ListParked()
	if err != nil {
		return CheckResult{}, err
	}

	evidence := map[string]any{"central_configured": true, "parked_count": len(parked)}
	if len(parked) == 0 {
		return okResult(c.Code(), evidence), nil
	}

	findings := make([]Finding, 0, len(parked))
	for _, p := range parked {
		idPrefix := p.MutationID
		if len(idPrefix) > mutationIDPrefixLen {
			idPrefix = idPrefix[:mutationIDPrefixLen]
		}
		findings = append(findings, Finding{
			CheckID:    c.Code(),
			Severity:   SeverityWarning,
			ReasonCode: CheckParkedMutations,
			Message: fmt.Sprintf("mutation %s… (local_seq=%d, project %q) was permanently rejected and is parked (%d attempt(s)): %s",
				idPrefix, p.LocalSeq, p.Project, p.Attempts, p.LastError),
			Why: "Central rejected this exact mutation and will keep rejecting it unmodified — retrying it automatically forever would " +
				"just repeat the same failed push. It also blocks every LATER write to the same memory queued behind it.",
			Evidence: mustJSON(map[string]any{
				"local_seq":       p.LocalSeq,
				"mutation_id":     idPrefix,
				"project":         p.Project,
				"entity":          p.Entity,
				"attempts":        p.Attempts,
				"last_error":      p.LastError,
				"last_attempt_at": p.LastAttemptAt.UTC().Format(time.RFC3339),
				"parked_at":       p.ParkedAt.UTC().Format(time.RFC3339),
			}),
			SafeNextStep: fmt.Sprintf("Read last_error, then `engram sync retry %d` if the underlying problem is fixed, "+
				"or `engram sync discard %d` if this mutation's effect should never reach central.", p.LocalSeq, p.LocalSeq),
			RequiresConfirmation: true,
		})
	}
	return resultFromFindings(c.Code(), evidence, findings), nil
}

// normalizeProject applies the same lowercase/trim rule the store applies to
// project names, so a comparison here cannot report a "mismatch" that is only a
// difference in case.
func normalizeProject(value string) string {
	return strings.TrimSpace(strings.ToLower(strings.TrimSpace(value)))
}
