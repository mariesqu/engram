// Package syncer is the production home of the local↔central sync orchestration:
// Node, NewNode, (*Node).Write, Push, Pull, Sync, SyncAllProjects, and SyncAll.
// These primitives drive one full push/pull round for a local SQLite node against
// any transport.Central peer — whether that is an in-process *centralstore.Store
// or a *remote.Client.
//
// Multi-project correctness: central_mutations.seq is a single global BIGSERIAL.
// The old global pull cursor (sync_state.last_pulled_seq) would skip interleaved
// projects: pull A advances the global cursor past A's seqs; B's lower interleaved
// seqs then fall below the cursor and are silently missed. Pull therefore uses
// per-project cursors (localstore.PullCursorFor / SetPullCursorFor), and
// SyncAllProjects drives one Push + one Pull-per-project cycle.
//
// The autosync Loop drives SyncAllProjects per tick.
//
// History: this logic lived in internal/spike (the in-process convergence proof
// harness) during early transport development. It was moved here in PR5a so that
// production code can import it without depending on a test-flavored "spike"
// package. internal/spike now re-exports every identifier as a thin compatibility
// shim, keeping the convergence-proof acceptance tests stable and unmodified.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/transport"
)

// Central is the central-store port this package drives. It is an alias of
// transport.Central so callers can reference syncer.Central without importing
// the transport package directly — while the canonical definition remains in
// transport, available to the HTTP client and any other concrete implementation
// without a circular import.
type Central = transport.Central

// Node is one sync participant: a local SQLite store plus its own outbox and
// pull cursor (both persisted inside the store's sync_mutations / sync_state
// tables). A node owns its SQLite temp file; two nodes = two independent files.
type Node struct {
	// Name is a human label used in error messages.
	Name string
	// Store is the node's local SQLite store (its own DB file).
	Store *localstore.Store
}

// NewNode wraps an already-open local store as a sync node.
func NewNode(name string, store *localstore.Store) *Node {
	return &Node{Name: name, Store: store}
}

// Write applies a new local write on this node (Decide→Apply locally) and
// enqueues it for push. Thin pass-through to localstore.LocalWrite so harness
// callers (tests) have one entry point per node.
func (n *Node) Write(m domain.Mutation) (domain.Mutation, error) {
	out, err := n.Store.LocalWrite(m)
	if err != nil {
		return out, fmt.Errorf("node %s: write: %w", n.Name, err)
	}
	return out, nil
}

// pullLimit bounds a single PullSince call. Large enough that the acceptance tests'
// mutation counts always come back in one round.
const pullLimit = 1000

// Push drains the node's pending outbox and applies each synced mutation to
// central. Central assigns the authoritative seq and reconciles BY VERSION
// (domain.Decide is last-write-wins), so applies are safe to run out of order —
// a higher-version mutation never loses to a lower-version one. On a successful
// central Apply the outbox entry is acked so it is never pushed again (INV5 at
// the transport layer; central_mutations.mutation_id UNIQUE is the deeper guard).
//
// Concurrency: the network Apply is the bottleneck — one round-trip per mutation
// to a possibly-remote central. Push applies up to pushConcurrency() DISTINCT
// memories (sync_ids) in parallel (default 8; override with ENGRAM_PUSH_CONCURRENCY),
// turning a large first-sync backlog from O(N·RTT) wall time into roughly
// O(N·RTT / C). Multiple versions of the SAME memory are applied sequentially so
// central's read-then-write reconcile can't lose a higher version to a stale one.
// The local store is single-connection (SetMaxOpenConns(1)), so the concurrent
// AckMutation calls serialize safely without SQLITE_BUSY; only network applies overlap.
//
// Policy filter (PR-②): each outbox entry is checked against the project's
// effective policy (via a small per-project cache) before being applied. Entries
// whose project has policy local-only or omitted are SKIPPED — left UNACKED in
// the outbox so they remain eligible for drain on the next push cycle. This is
// the mechanism behind the "flip local-only→synced drains the queue" guarantee.
//
// Returns the number of mutations pushed (acked) this round and the first error
// encountered (if any). On error, already-acked mutations stay acked; the rest
// are retried next cycle (idempotent via mutation_id).
func Push(ctx context.Context, n *Node, central Central) (int, error) {
	entries, err := n.Store.DrainOutbox(0)
	if err != nil {
		return 0, fmt.Errorf("push %s: drain outbox: %w", n.Name, err)
	}

	// Phase 1 — policy filter (sequential, cheap local reads). Build the list of
	// synced entries to apply and accumulate per-project skip counts. The cache
	// avoids a GetPolicy round-trip per entry when many share a project.
	type pushJob struct {
		localSeq int64
		mutation domain.Mutation
	}
	// Group synced entries by sync_id, preserving outbox (local_seq) order within
	// each group. Skipped (non-synced) entries are left unacked.
	groups := make(map[string][]pushJob)
	var order []string // first-seen sync_id order, for deterministic scheduling
	skipped := map[string]int{}
	polCache := map[string]localstore.Policy{}
	for _, e := range entries {
		pol, cached := polCache[e.Mutation.Project]
		if !cached {
			p, polErr := n.Store.GetPolicy(e.Mutation.Project)
			if polErr != nil {
				return 0, fmt.Errorf("push %s: get policy for project %q (local_seq=%d): %w",
					n.Name, e.Mutation.Project, e.LocalSeq, polErr)
			}
			pol = p
			polCache[e.Mutation.Project] = p
		}
		if pol != localstore.PolicySynced {
			skipped[e.Mutation.Project]++ // leave unacked — eligible again when policy flips to synced
			continue
		}
		sid := e.Mutation.SyncID
		if _, seen := groups[sid]; !seen {
			order = append(order, sid)
		}
		groups[sid] = append(groups[sid], pushJob{localSeq: e.LocalSeq, mutation: e.Mutation})
	}

	// Phase 2 — apply with bounded concurrency, ONE goroutine per sync_id.
	//
	// Central reconciles each mutation by reading current state (domain.Decide)
	// then writing, in SEPARATE transactions — so two concurrent applies of the
	// SAME identity can interleave and a stale lower version can clobber a higher
	// one (a lost update the old serial path could never produce). Serializing per
	// sync_id (entries applied in local_seq = version order within one goroutine)
	// preserves each identity's apply order, while DISTINCT identities — the
	// overwhelming majority of a backlog — still run in parallel. SetLimit throttles
	// to pushConcurrency() groups; the first error cancels gctx so the rest stop.
	var pushed atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(pushConcurrency())
	for _, sid := range order {
		if gctx.Err() != nil {
			break // a prior apply failed (or ctx cancelled) — stop scheduling more
		}
		jobs := groups[sid]
		g.Go(func() error {
			for _, j := range jobs {
				if gctx.Err() != nil {
					return gctx.Err() // a sibling failed or ctx cancelled — stop this identity
				}
				if err := central.Apply(gctx, j.mutation); err != nil {
					return fmt.Errorf("push %s: central.Apply(local_seq=%d, mutation_id=%s): %w",
						n.Name, j.localSeq, j.mutation.MutationID, err)
				}
				if err := n.Store.AckMutation(j.localSeq); err != nil {
					return fmt.Errorf("push %s: ack(local_seq=%d): %w", n.Name, j.localSeq, err)
				}
				pushed.Add(1)
			}
			return nil
		})
	}
	applyErr := g.Wait()

	// One debug line per drain cycle summarises all skipped projects — not per entry.
	for proj, count := range skipped {
		slog.Debug("syncer.Push: skipped project (non-synced policy)",
			"node", n.Name,
			"project", proj,
			"policy", string(polCache[proj]),
			"skipped_entries", count,
		)
	}
	return int(pushed.Load()), applyErr
}

// pushConcurrency returns how many DISTINCT memories (sync_ids) Push applies in
// parallel. Default 8; override with ENGRAM_PUSH_CONCURRENCY (clamped to [1, 64]).
// The network Apply dominates, so parallelism cuts first-sync backlog wall time
// roughly linearly until central's connection pool saturates — that pool is sized
// to at least 16 (centralstore.Open), so values above 16 only help when the DSN
// raises pool_max_conns to match.
func pushConcurrency() int {
	const def = 8
	raw := strings.TrimSpace(os.Getenv("ENGRAM_PUSH_CONCURRENCY"))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return def
	}
	if n > 64 {
		return 64
	}
	return n
}

// Pull fetches central mutations for project with seq > the node's per-project
// cursor, in strict ascending seq order, applies each to the node's local store
// through the SAME domain.Decide→Apply path, and advances the per-project cursor
// to the highest seq seen.
//
// Per-project cursor correctness: central_mutations.seq is a single global
// BIGSERIAL. Using a global cursor would skip interleaved projects: pulling A
// advances the global cursor past A's seqs; B's lower interleaved seqs then fall
// below the cursor and are silently missed. Pull uses PullCursorFor(project) /
// SetPullCursorFor(project, seq) so each project's cursor advances independently.
// This makes Pull the degenerate (single-project) case of SyncAllProjects.
//
// Replay is in seq order (INV2) so the local store observes central's
// authoritative ordering. Each applied mutation carries its central seq;
// localstore.ApplyPulled runs Decide(localReader, m) → Apply, with INV5 making a
// re-pulled mutation a no-op.
//
// LWW tiebreaker: at the exact (updated_at, version) tie, writeWins resolves by
// (writer_id, then the winning mutation's content-addressed mutation_id carried
// by last_write_mutation_id) — replica-identical payload-derived fields so every
// store computes the same winner (no central seq back-channel required). The INV4
// acceptance tests use DISTINCT UpdatedAt values so wall-clock decides convergence;
// the identity tiebreaker is the final authority only when updated_at and version
// are equal (probed explicitly in tsseq_probe_acceptance_test.go).
//
// Returns the number of mutations pulled (applied or no-op'd) this round.
func Pull(ctx context.Context, n *Node, central Central, project string) (int, error) {
	cursor, err := n.Store.PullCursorFor(project)
	if err != nil {
		return 0, fmt.Errorf("pull %s[%s]: read cursor: %w", n.Name, project, err)
	}

	muts, err := central.PullSince(ctx, project, cursor, pullLimit)
	if err != nil {
		return 0, fmt.Errorf("pull %s[%s]: PullSince(since=%d): %w", n.Name, project, cursor, err)
	}

	maxSeq := cursor
	for _, m := range muts {
		if err := n.Store.ApplyPulled(m); err != nil {
			return 0, fmt.Errorf("pull %s[%s]: apply pulled (seq=%d, mutation_id=%s): %w",
				n.Name, project, m.Seq, m.MutationID, err)
		}
		if m.Seq > maxSeq {
			maxSeq = m.Seq
		}
	}

	if maxSeq > cursor {
		if err := n.Store.SetPullCursorFor(project, maxSeq); err != nil {
			return len(muts), fmt.Errorf("pull %s[%s]: advance cursor to %d: %w", n.Name, project, maxSeq, err)
		}
	}
	return len(muts), nil
}

// Sync is one full round for a node: push its local writes to central, then pull
// central's mutations back for the given project. Returns (pushed, pulled) counts.
//
// For multi-project workloads use SyncAllProjects instead; Sync handles the
// single-project (degenerate) case and is kept for backward compatibility with
// the existing convergence acceptance tests.
func Sync(ctx context.Context, n *Node, central Central, project string) (pushed, pulled int, err error) {
	pushed, err = Push(ctx, n, central)
	if err != nil {
		return pushed, 0, err
	}
	pulled, err = Pull(ctx, n, central, project)
	if err != nil {
		return pushed, pulled, err
	}
	return pushed, pulled, nil
}

// SyncAllProjects is the multi-project autosync driver used by the Loop.
//
// It performs one full round for node n:
//  1. Push: drain the outbox (project-agnostic, policy-filtered) to central once.
//  2. ListProjects: discover all projects known to n's local store.
//  3. Pull each project using its own per-project cursor.
//
// Policy filter on pull (PR-②): projects with policy local-only or omitted are
// excluded from the pull loop entirely.  Their per-project cursors remain
// unchanged, so a future flip to synced resumes pulling from where it left off
// (pull is idempotent via per-project cursors + INV5).
//
// Error policy: Push errors short-circuit immediately (outbox integrity matters).
// For Pull, every project is attempted even if earlier ones fail; all errors are
// collected into a single joined error so the Loop can classify retryability.
// The Loop backs off if ANY underlying error is retryable (any project's pull
// failure is transient until proven otherwise). A project pull failure does NOT
// rewind that project's cursor — per-project cursors + INV5 (applied_mutations)
// make re-pulls idempotent.
//
// Returns (pushed, totalPulled, error).
func SyncAllProjects(ctx context.Context, n *Node, central Central) (pushed, pulled int, err error) {
	pushed, err = Push(ctx, n, central)
	if err != nil {
		return pushed, 0, fmt.Errorf("SyncAllProjects %s: push: %w", n.Name, err)
	}

	projects, err := n.Store.ListProjects()
	if err != nil {
		return pushed, 0, fmt.Errorf("SyncAllProjects %s: list projects: %w", n.Name, err)
	}

	var errs []error

	// New-project pull discovery: ListProjects above reads only the LOCAL store,
	// so a node would never pull a project it has not written to itself. To honor
	// the "mirror every project" contract, also discover projects that exist on
	// central — written by OTHER writers and never seen locally — and union them
	// in, so this node pulls them too. The capability is OPTIONAL: a Central that
	// does not implement projectLister (e.g. a lightweight test stub) yields no
	// central projects and pull stays purely local-driven (older behaviour).
	if lister, ok := central.(projectLister); ok {
		centralProjects, derr := lister.ListProjects(ctx)
		switch {
		case derr == nil:
			projects = unionProjects(projects, centralProjects)
		case isDiscoveryUnsupported(derr):
			// The central server does not implement project discovery — either an
			// OLDER central whose catch-all returns 404 for the unregistered
			// /v1/projects route (the real mixed-version case), or a 501 from a
			// capability-gated handler. Neither is a sync failure: this node simply
			// pulls its locally-known projects this cycle. Crucially we do NOT append
			// to errs — otherwise every cycle would report a spurious failed sync
			// (and a 501, being >= 500, would wedge the Loop into permanent backoff).
		default:
			// A genuine discovery failure (network, 5xx, auth) is recorded so the
			// Loop can classify retryability and back off, but we still pull the
			// locally-known projects this cycle rather than aborting the round.
			errs = append(errs, fmt.Errorf("SyncAllProjects %s: discover central projects: %w", n.Name, derr))
		}
	}

	for _, proj := range projects {
		// Pull filter: skip projects that are not synced.
		pol, polErr := n.Store.GetPolicy(proj)
		if polErr != nil {
			errs = append(errs, fmt.Errorf("SyncAllProjects %s: get policy %q: %w", n.Name, proj, polErr))
			continue
		}
		if pol != localstore.PolicySynced {
			// One debug line per skipped project per sync cycle — not per entry.
			slog.Debug("syncer.SyncAllProjects: skipping pull for non-synced project",
				"node", n.Name,
				"project", proj,
				"policy", string(pol),
			)
			continue // local-only or omitted: no pull for this project
		}

		cnt, perr := Pull(ctx, n, central, proj)
		pulled += cnt
		if perr != nil {
			errs = append(errs, perr)
		}
	}

	if len(errs) > 0 {
		return pushed, pulled, &syncAllError{errs: errs}
	}
	return pushed, pulled, nil
}

// projectLister is the OPTIONAL capability a [Central] may implement to
// enumerate the projects central knows (across all writers). When the transport
// supports it, SyncAllProjects unions central's projects with the node's local
// projects so a node pulls projects that originated elsewhere and were never
// written locally. *remote.Client and *centralstore.Store both satisfy it;
// lightweight test doubles need not. Mirrors the structural-typing pattern used
// by retryabler — an additive capability without widening the core Central port.
type projectLister interface {
	ListProjects(ctx context.Context) ([]string, error)
}

// statusCoder is the OPTIONAL HTTP-status accessor implemented by
// remote.StatusError. SyncAllProjects uses it to detect a 501 (an older central
// without project discovery) WITHOUT importing internal/remote — the same
// structural-typing approach as retryabler and projectLister.
type statusCoder interface {
	StatusCode() int
}

// HTTP statuses that mean "this central does not implement project discovery,"
// mirrored here without importing net/http into this transport-agnostic package:
//   - 404: an OLDER central predating /v1/projects — its catch-all returns a JSON
//     404 for the unregistered route. This is the REAL mixed-version case (a real
//     central always has its store implement ListProjects, so it never 501s).
//   - 501: the route exists but the wrapped Central does not implement the
//     capability (handleProjects' explicit "not supported").
const (
	httpStatusNotFound       = 404
	httpStatusNotImplemented = 501
)

// isDiscoveryUnsupported reports whether err is central signalling that it does
// not implement project discovery — a 404 from an older central's catch-all for
// the unregistered /v1/projects route, or a 501 from a capability-gated handler.
// Such an error must NOT be treated as a sync failure: otherwise every cycle
// against an older central would report a spurious error (and a 501, being >= 500,
// would wedge the Loop into permanent backoff). See SyncAllProjects.
//
// Scoping note: this predicate is applied ONLY to the discovery (ListProjects)
// error. A wrong base URL or a down server fails push/pull FIRST (push
// short-circuits the cycle), so reaching discovery means the transport works and
// a 404 there unambiguously means the route is absent — not a misroute.
func isDiscoveryUnsupported(err error) bool {
	var sc statusCoder
	if errors.As(err, &sc) {
		code := sc.StatusCode()
		return code == httpStatusNotFound || code == httpStatusNotImplemented
	}
	return false
}

// unionProjects merges two project-name slices into a sorted, de-duplicated set,
// dropping the empty string. Sorting keeps pull order stable across cycles and
// makes tests reproducible.
func unionProjects(local, central []string) []string {
	seen := make(map[string]struct{}, len(local)+len(central))
	out := make([]string, 0, len(local)+len(central))
	for _, group := range [][]string{local, central} {
		for _, p := range group {
			if p == "" {
				continue
			}
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// syncAllError is a multi-error returned by SyncAllProjects when one or more
// project pulls fail. It implements the retryabler interface so the Loop can
// classify retryability correctly: if ANY underlying error is retryable, the
// whole batch is considered retryable (back off and retry — re-pulls are
// idempotent via per-project cursors + INV5).
type syncAllError struct {
	errs []error
}

func (e *syncAllError) Error() string {
	if len(e.errs) == 1 {
		return e.errs[0].Error()
	}
	buf := fmt.Appendf(nil, "%d project pull errors: %s", len(e.errs), e.errs[0].Error())
	for _, err := range e.errs[1:] {
		buf = fmt.Appendf(buf, "; %s", err.Error())
	}
	return string(buf)
}

// Retryable returns true if ANY wrapped error is retryable. The Loop backs off
// on the whole batch — individual project pulls are idempotent so re-running
// previously-succeeded pulls is harmless.
func (e *syncAllError) Retryable() bool {
	for _, err := range e.errs {
		var r retryabler
		if errors.As(err, &r) {
			if r.Retryable() {
				return true
			}
		} else {
			// Unknown transport/local error → treat as retryable (transient default).
			return true
		}
	}
	return false
}

// Unwrap exposes the per-project errors so callers can use errors.Is / errors.As
// to inspect or match specific underlying failures (Go 1.20+ multi-error
// semantics). The Loop still classifies retryability via Retryable() above.
func (e *syncAllError) Unwrap() []error {
	return e.errs
}

// SyncAll runs `rounds` full bidirectional sync rounds across all nodes in
// order for the given project. Multiple rounds let writes propagate: round 1
// pushes everyone's local writes to central; round 2 lets each node pull the
// others' writes that landed after its own round-1 pull. Two rounds settle any
// pair of writers; the acceptance tests use 2–3 to be safe.
//
// SyncAll uses the per-project cursor path (Pull→PullCursorFor) introduced in
// the multi-project autosync refactor. Single-project callers are the degenerate
// case of per-project cursors and remain correct.
func SyncAll(ctx context.Context, nodes []*Node, central Central, project string, rounds int) error {
	for r := 0; r < rounds; r++ {
		for _, n := range nodes {
			if _, _, err := Sync(ctx, n, central, project); err != nil {
				return fmt.Errorf("SyncAll round %d, node %s: %w", r+1, n.Name, err)
			}
		}
	}
	return nil
}
