// Package syncer is the production home of the local↔central sync orchestration:
// Node, NewNode, (*Node).Write, Push, Pull, Sync, and SyncAll. These primitives drive one full
// push/pull round for a local SQLite node against any transport.Central peer —
// whether that is an in-process *centralstore.Store or a *remote.Client.
//
// The autosync Loop (continuous background syncing, PR5b) will be added to this
// package alongside the round-trip helpers defined here.
//
// History: this logic lived in internal/spike (the in-process convergence proof
// harness) during early transport development. It was moved here in PR5a so that
// production code can import it without depending on a test-flavored "spike"
// package. internal/spike now re-exports every identifier as a thin compatibility
// shim, keeping the convergence-proof acceptance tests stable and unmodified.
package syncer

import (
	"context"
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

// pullLimit bounds a single PullSince call. Large enough that the spike's small
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

// Pull fetches central mutations for project with seq > the node's cursor, in
// strict ascending seq order, applies each to the node's local store through the
// SAME domain.Decide→Apply path, and advances the cursor to the highest seq
// seen.
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
	cursor, err := n.Store.PullCursor()
	if err != nil {
		return 0, fmt.Errorf("pull %s: read cursor: %w", n.Name, err)
	}

	muts, err := central.PullSince(ctx, project, cursor, pullLimit)
	if err != nil {
		return 0, fmt.Errorf("pull %s: PullSince(since=%d): %w", n.Name, cursor, err)
	}

	maxSeq := cursor
	for _, m := range muts {
		if err := n.Store.ApplyPulled(m); err != nil {
			return 0, fmt.Errorf("pull %s: apply pulled (seq=%d, mutation_id=%s): %w",
				n.Name, m.Seq, m.MutationID, err)
		}
		if m.Seq > maxSeq {
			maxSeq = m.Seq
		}
	}

	if maxSeq > cursor {
		if err := n.Store.SetPullCursor(maxSeq); err != nil {
			return len(muts), fmt.Errorf("pull %s: advance cursor to %d: %w", n.Name, maxSeq, err)
		}
	}
	return len(muts), nil
}

// Sync is one full round for a node: push its local writes to central, then pull
// central's mutations back. Returns (pushed, pulled) counts.
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

// SyncAll runs `rounds` full bidirectional sync rounds across all nodes in
// order. Multiple rounds let writes propagate: round 1 pushes everyone's local
// writes to central; round 2 lets each node pull the others' writes that landed
// after its own round-1 pull. Two rounds settle any pair of writers; the
// acceptance tests use 2–3 to be safe.
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
