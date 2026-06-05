// Package spike is a compatibility shim for the convergence-proof acceptance
// tests. The sync orchestration (Node, NewNode, (*Node).Write, Push, Pull, Sync, SyncAll,
// and the Central type alias) has moved to internal/syncer — its production home,
// where the PR5b autosync Loop will also live. This file re-exports every
// identifier so the existing acceptance tests in internal/spike and
// internal/remote continue to compile and pass unchanged.
//
// Do not add new production logic here. New callers should import
// internal/syncer directly.
package spike

import (
	"context"

	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/syncer"
)

// Central is the central-store port the convergence harness drives. Re-exported
// from syncer.Central (= transport.Central) so test files that reference
// spike.Central keep compiling without change.
type Central = syncer.Central

// Node is one sync participant. Re-exported from syncer.Node.
type Node = syncer.Node

// NewNode wraps an already-open local store as a sync node.
func NewNode(name string, store *localstore.Store) *Node {
	return syncer.NewNode(name, store)
}

// Push drains the node's pending outbox and applies each mutation to central.
// See syncer.Push for full documentation.
func Push(ctx context.Context, n *Node, central Central) (int, error) {
	return syncer.Push(ctx, n, central)
}

// Pull fetches central mutations for project and applies them to the node's
// local store. See syncer.Pull for full documentation.
func Pull(ctx context.Context, n *Node, central Central, project string) (int, error) {
	return syncer.Pull(ctx, n, central, project)
}

// Sync is one full round for a node: push then pull.
// See syncer.Sync for full documentation.
func Sync(ctx context.Context, n *Node, central Central, project string) (pushed, pulled int, err error) {
	return syncer.Sync(ctx, n, central, project)
}

// SyncAll runs `rounds` full bidirectional sync rounds across all nodes.
// See syncer.SyncAll for full documentation.
func SyncAll(ctx context.Context, nodes []*Node, central Central, project string, rounds int) error {
	return syncer.SyncAll(ctx, nodes, central, project, rounds)
}
