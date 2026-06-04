// Package transport defines the port interfaces for the sync transport layer.
// These interfaces are the seam between the sync harness / autosync loop and
// any concrete central-side implementation — whether that is an in-process
// *centralstore.Store or a remote HTTP client.
package transport

import (
	"context"

	"github.com/mariesqu/engram/internal/domain"
)

// Central is the central-side port of the sync transport. Any type that
// implements Apply and PullSince can act as the remote peer in a sync round —
// an in-process *centralstore.Store satisfies this interface directly, and the
// future HTTP client will satisfy it over the wire.
//
//   - Apply assigns the authoritative BIGSERIAL seq and reconciles the pushed
//     mutation via the same domain.Decide the local store uses.
//   - PullSince returns mutations with seq > sinceSeq for the project, in strict
//     ascending seq order, each carrying its central seq.
type Central interface {
	Apply(ctx context.Context, m domain.Mutation) error
	PullSince(ctx context.Context, project string, sinceSeq int64, limit int) ([]domain.Mutation, error)
}
