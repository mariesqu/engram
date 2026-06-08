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

// Push drains the node's pending outbox and applies each mutation to central in
// local order. Central assigns the authoritative seq and reconciles. On a
// successful central Apply the outbox entry is acked so it is never pushed again
// (INV5 at the transport layer; central_mutations.mutation_id UNIQUE is the
// deeper guard).
//
// Returns the number of mutations pushed (acked) this round.
func Push(ctx context.Context, n *Node, central Central) (int, error) {
	entries, err := n.Store.DrainOutbox(0)
	if err != nil {
		return 0, fmt.Errorf("push %s: drain outbox: %w", n.Name, err)
	}

	pushed := 0
	for _, e := range entries {
		if err := central.Apply(ctx, e.Mutation); err != nil {
			return pushed, fmt.Errorf("push %s: central.Apply(local_seq=%d, mutation_id=%s): %w",
				n.Name, e.LocalSeq, e.Mutation.MutationID, err)
		}
		if err := n.Store.AckMutation(e.LocalSeq); err != nil {
			return pushed, fmt.Errorf("push %s: ack(local_seq=%d): %w", n.Name, e.LocalSeq, err)
		}
		pushed++
	}
	return pushed, nil
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
//  1. Push: drain the outbox (project-agnostic) to central once.
//  2. ListProjects: discover all projects known to n's local store.
//  3. Pull each project using its own per-project cursor.
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
	for _, proj := range projects {
		n, err := Pull(ctx, n, central, proj)
		pulled += n
		if err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return pushed, pulled, &syncAllError{errs: errs}
	}
	return pushed, pulled, nil
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
	msg := fmt.Sprintf("%d project pull errors: %s", len(e.errs), e.errs[0].Error())
	for _, err := range e.errs[1:] {
		msg += "; " + err.Error()
	}
	return msg
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
