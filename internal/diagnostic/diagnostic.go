// Package diagnostic runs read-only operational checks over a local engram
// store and reports them in one structured envelope (the `mem_doctor` tool and,
// in principle, any CLI or UI that wants the same answers).
//
// Two decisions shape everything here.
//
// It REPORTS, it never repairs. Every finding carries an evidence blob and a
// safe_next_step the operator performs deliberately; nothing in this package
// writes. The conditions worth diagnosing — an observation whose session is
// gone, two live sessions for one directory — are precisely the ones where the
// correct fix depends on context the store does not have, and an automatic
// "repair" would destroy the evidence on its way to guessing.
//
// A check never fails the run. A check that cannot answer returns a finding
// that says so (severity error), and the report's rollup carries it:
// errors > blocked > warning > ok. A doctor that refuses to print the other
// eight results because the ninth could not read a pragma is a doctor nobody
// runs twice.
package diagnostic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mariesqu/engram/internal/localstore"
)

// Result and severity vocabularies. They are separate on purpose: the RESULT is
// what the machine branches on, the SEVERITY is how loud a human should find it.
const (
	StatusOK      = "ok"
	StatusWarning = "warning"
	StatusBlocked = "blocked"
	StatusError   = "error"

	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityError    = "error"
	SeverityBlocking = "blocking"
)

// Store is the read-only surface the checks need. It is an interface, not
// *localstore.Store, so a check can be tested against a fixture that returns
// exactly the state under test — including states that are hard to create in a
// real database, like a store whose queries fail.
//
// *localstore.Store satisfies it (see internal/localstore/diagnostic.go).
type Store interface {
	DiagnosticSessions(project string) ([]localstore.DiagnosticSession, error)
	OrphanedObservationSessions(project string) ([]localstore.OrphanedSessionRef, error)
	UnregisteredSessionSaves(project string) ([]localstore.OrphanedSessionRef, error)
	ProjectsWithoutPolicy(project string) ([]string, error)
	CountByReviewStatus(project string) (localstore.ReviewCounts, error)
	SyncBacklog() (localstore.SyncBacklog, error)
	SQLiteLockSnapshot(ctx context.Context) (localstore.SQLiteLockSnapshot, error)
	CentralConfigured() bool
}

// Scope is one doctor run: which store, which project (empty = every project),
// and what "now" means. Now is injected so the age-based checks are testable
// without sleeping.
//
// DetectProject overrides directory→project detection, which is otherwise a
// filesystem walk. Tests use it; production leaves it nil.
type Scope struct {
	Store         Store
	Project       string
	Now           time.Time
	DetectProject func(directory string) (DetectedProject, bool)

	// sessions memoizes the session projection for ONE run. It is a pointer so
	// it survives the by-value copies a Scope makes on its way through the
	// runner into each check. Nil means "not memoized" — a Scope built by hand
	// (tests, or any caller running a single check directly) still works, it just
	// queries every time.
	sessions *sessionMemo
}

// sessionMemo holds one run's session projection.
type sessionMemo struct {
	once sync.Once
	rows []localstore.DiagnosticSession
	err  error
}

// memoized returns a copy of the scope with a fresh session memo attached. The
// runner calls it once per run: two checks (ambiguous_active_sessions and
// session_project_directory_mismatch) each need every session row, and on a
// long-lived store that is the most expensive read the doctor does — running it
// twice per report is pure duplication of a query whose answer cannot change
// mid-run in any way the report should reflect.
func (s Scope) memoized() Scope {
	s.sessions = &sessionMemo{}
	return s
}

// Sessions returns the scope's session projection, loading it at most once per
// run. A check must use this rather than Store.DiagnosticSessions directly.
func (s Scope) Sessions() ([]localstore.DiagnosticSession, error) {
	if s.sessions == nil {
		return s.Store.DiagnosticSessions(s.Project)
	}
	s.sessions.once.Do(func() {
		s.sessions.rows, s.sessions.err = s.Store.DiagnosticSessions(s.Project)
	})
	return s.sessions.rows, s.sessions.err
}

// now returns the scope's clock, defaulting to the wall clock.
func (s Scope) now() time.Time {
	if s.Now.IsZero() {
		return time.Now().UTC()
	}
	return s.Now.UTC()
}

// DetectedProject is what a directory resolves to, and how.
type DetectedProject struct {
	Project string `json:"project"`
	Source  string `json:"source"`
	Path    string `json:"path,omitempty"`
}

// Finding is one concrete instance of a problem: this session, this project,
// this session id. A check with three bad sessions reports three findings, not
// three checks.
type Finding struct {
	CheckID              string          `json:"check_id"`
	Severity             string          `json:"severity"`
	ReasonCode           string          `json:"reason_code"`
	Message              string          `json:"message"`
	Why                  string          `json:"why"`
	Evidence             json.RawMessage `json:"evidence"`
	SafeNextStep         string          `json:"safe_next_step"`
	RequiresConfirmation bool            `json:"requires_confirmation"`
}

// CheckResult is one check's verdict plus its findings.
type CheckResult struct {
	CheckID              string          `json:"check_id"`
	Result               string          `json:"result"`
	Severity             string          `json:"severity"`
	ReasonCode           string          `json:"reason_code"`
	Message              string          `json:"message"`
	Why                  string          `json:"why"`
	Evidence             json.RawMessage `json:"evidence"`
	SafeNextStep         string          `json:"safe_next_step"`
	RequiresConfirmation bool            `json:"requires_confirmation"`
	Findings             []Finding       `json:"findings,omitempty"`
}

// Summary is the count-by-result rollup.
type Summary struct {
	Total    int `json:"total"`
	OK       int `json:"ok"`
	Warnings int `json:"warnings"`
	Blocked  int `json:"blocked"`
	Errors   int `json:"errors"`
}

// Report is the whole envelope: one status, one summary, every check.
type Report struct {
	Status  string        `json:"status"`
	Project string        `json:"project,omitempty"`
	Summary Summary       `json:"summary"`
	Checks  []CheckResult `json:"checks"`
}

// Check is one diagnostic. Run returns a result; it returns an error only for
// something that made the check impossible to run, and the Runner turns that
// into an error RESULT rather than failing the report.
type Check interface {
	Code() string
	Run(ctx context.Context, scope Scope) (CheckResult, error)
}

// Runner executes a registry's checks.
type Runner struct {
	registry Registry
}

// NewRunner returns a Runner over the default registry.
func NewRunner() Runner { return Runner{registry: DefaultRegistry()} }

// NewRunnerWithRegistry returns a Runner over a custom registry (tests).
func NewRunnerWithRegistry(r Registry) Runner { return Runner{registry: r} }

// RunAll runs every registered check, in registry order.
func (r Runner) RunAll(ctx context.Context, scope Scope) Report {
	scope = scope.memoized()
	checks := r.registry.Checks()
	results := make([]CheckResult, 0, len(checks))
	for _, check := range checks {
		results = append(results, runCheck(ctx, scope, check))
	}
	return buildReport(scope.Project, results)
}

// RunOne runs a single check by code. An unknown code is an error report, not a
// panic and not an empty one: the caller asked a question, and "that check does
// not exist" is the answer.
func (r Runner) RunOne(ctx context.Context, scope Scope, code string) Report {
	check, err := r.registry.Lookup(code)
	if err != nil {
		return ErrorReport(scope.Project, err)
	}
	return buildReport(scope.Project, []CheckResult{runCheck(ctx, scope.memoized(), check)})
}

// runCheck executes one check and normalizes its result: an error becomes an
// error RESULT (the run continues), and every unset field gets a sane default
// so a consumer never has to branch on emptiness.
func runCheck(ctx context.Context, scope Scope, check Check) CheckResult {
	result, err := check.Run(ctx, scope)
	if err != nil {
		return CheckResult{
			CheckID:      check.Code(),
			Result:       StatusError,
			Severity:     SeverityError,
			ReasonCode:   check.Code() + "_failed",
			Message:      err.Error(),
			Why:          "The check could not read the state it needed, so this condition is neither confirmed nor ruled out.",
			Evidence:     mustJSON(map[string]any{"error": err.Error()}),
			SafeNextStep: "Re-run `mem_doctor` with check=" + check.Code() + " once the daemon is idle; if it keeps failing, the store itself needs attention.",
		}
	}
	if result.CheckID = strings.TrimSpace(result.CheckID); result.CheckID == "" {
		result.CheckID = check.Code()
	}
	if result.Result == "" {
		result.Result = StatusOK
	}
	if result.Severity == "" {
		result.Severity = SeverityInfo
	}
	if result.ReasonCode == "" {
		result.ReasonCode = result.CheckID + "_ok"
	}
	if result.Evidence == nil {
		result.Evidence = mustJSON(map[string]any{"evaluated": true})
	}
	return result
}

// buildReport counts the results and rolls them up worst-first.
func buildReport(project string, checks []CheckResult) Report {
	report := Report{Status: StatusOK, Project: project, Checks: checks}
	report.Summary.Total = len(checks)
	for _, check := range checks {
		switch check.Result {
		case StatusError:
			report.Summary.Errors++
		case StatusBlocked:
			report.Summary.Blocked++
		case StatusWarning:
			report.Summary.Warnings++
		default:
			report.Summary.OK++
		}
	}
	switch {
	case report.Summary.Errors > 0:
		report.Status = StatusError
	case report.Summary.Blocked > 0:
		report.Status = StatusBlocked
	case report.Summary.Warnings > 0:
		report.Status = StatusWarning
	}
	return report
}

// Run-level failure codes. A report carrying one of them as its ONLY check is a
// report about the DOCTOR (it could not run), not about the store — the
// distinction IsRunLevelFailure exists to express.
const (
	CodeDiagnosticError = "diagnostic_error"
	CodeInvalidCheck    = "invalid_check"
)

// IsRunLevelFailure reports whether r describes a failure of the doctor itself
// rather than a finding about the store.
//
// It exists because "status == error" is NOT that question. A single probe that
// cannot read its own pragma (sqlite_lock_probe_failed) rolls the report status
// up to error while the other six checks answered perfectly — and a caller that
// treated status alone as failure would tell the agent its mem_doctor CALL
// failed, which is how a diagnostic tool teaches people not to run it.
func (r Report) IsRunLevelFailure() bool {
	if len(r.Checks) != 1 {
		return false
	}
	switch r.Checks[0].CheckID {
	case CodeDiagnosticError, CodeInvalidCheck:
		return true
	default:
		return false
	}
}

// ErrorReport wraps a run-level failure (an unknown check code, a scope with no
// store) in the same envelope every other answer uses, so a consumer parses one
// shape and never a bare error string.
func ErrorReport(project string, err error) Report {
	code := CodeDiagnosticError
	next := "Run mem_doctor without a check argument to see every registered diagnostic."
	if errors.Is(err, ErrInvalidCheck) {
		code = CodeInvalidCheck
		next = "Call mem_doctor without a check argument; the response lists every check id."
	}
	return Report{
		Status:  StatusError,
		Project: project,
		Summary: Summary{Total: 1, Errors: 1},
		Checks: []CheckResult{{
			CheckID:      code,
			Result:       StatusError,
			Severity:     SeverityError,
			ReasonCode:   code,
			Message:      err.Error(),
			Why:          "Doctor could not run the requested diagnostic safely.",
			Evidence:     mustJSON(map[string]any{"error": err.Error()}),
			SafeNextStep: next,
		}},
	}
}

// mustJSON marshals evidence, degrading to an empty object rather than failing
// a diagnostic over its own formatting.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// okResult is the "nothing to report" shape, with the evidence that says what
// was actually examined — an ok with no evidence is indistinguishable from a
// check that did nothing.
func okResult(checkID string, evidence any) CheckResult {
	return CheckResult{
		CheckID:      checkID,
		Result:       StatusOK,
		Severity:     SeverityInfo,
		ReasonCode:   checkID + "_ok",
		Message:      "No issues detected.",
		Why:          "The evidence examined matches the expected operational invariants.",
		Evidence:     mustJSON(evidence),
		SafeNextStep: "No action required.",
	}
}

// okWithNotes is an OK result that still carries informational findings: a
// condition the operator may want to know about, which is not a problem.
//
// The distinction is the difference between a doctor people run and one they
// learn to ignore. "N memories were saved without a registered session" is
// true, useful, and describes the documented default behaviour of mem_save —
// reported as a WARNING it fires forever, on every healthy store, and takes the
// credibility of every other warning with it.
func okWithNotes(checkID string, evidence any, notes []Finding) CheckResult {
	result := okResult(checkID, evidence)
	if len(notes) == 0 {
		return result
	}
	result.Findings = notes
	result.Message = notes[0].Message
	result.SafeNextStep = notes[0].SafeNextStep
	return result
}

// resultFromFindings folds findings into a check result, taking the WORST
// severity present: one blocking finding blocks the check even if nine others
// are informational.
func resultFromFindings(checkID string, okEvidence any, findings []Finding) CheckResult {
	if len(findings) == 0 {
		return okResult(checkID, okEvidence)
	}
	result := CheckResult{
		CheckID:      checkID,
		Result:       StatusWarning,
		Severity:     SeverityWarning,
		ReasonCode:   findings[0].ReasonCode,
		Message:      fmt.Sprintf("%d finding(s) detected.", len(findings)),
		Why:          findings[0].Why,
		Evidence:     mustJSON(map[string]any{"finding_count": len(findings)}),
		SafeNextStep: findings[0].SafeNextStep,
		Findings:     findings,
	}
	// Informational findings must not drag a check out of "ok" severity into a
	// warning it does not deserve; a check whose findings are ALL info reports
	// info severity with a warning result (something to look at, nothing wrong).
	worst := SeverityInfo
	for _, f := range findings {
		switch f.Severity {
		case SeverityBlocking:
			result.Result = StatusBlocked
			result.Severity = SeverityBlocking
			return result
		case SeverityError:
			worst = SeverityError
		case SeverityWarning:
			if worst != SeverityError {
				worst = SeverityWarning
			}
		}
	}
	switch worst {
	case SeverityError:
		result.Result = StatusError
		result.Severity = SeverityError
	case SeverityInfo:
		result.Severity = SeverityInfo
	}
	return result
}
