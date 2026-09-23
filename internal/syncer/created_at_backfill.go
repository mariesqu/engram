package syncer

// created_at_backfill.go implements FUP-005c's driver: the ONE-TIME, per-
// project fetch of central's original creation times (FUP-005b's
// /v1/created-at) and their application to this node's local rows.

import (
	"context"
	"errors"
	"fmt"

	"github.com/mariesqu/engram/internal/domain"
)

// createdAtSource is the OPTIONAL capability a [Central] may implement to
// serve FUP-005's created_at backfill. *remote.Client and *centralstore.Store
// both satisfy it structurally; a lightweight test double need not — the same
// projectLister/retryabler/statusCoder structural-typing pattern used
// throughout this file.
type createdAtSource interface {
	OriginalCreatedAt(ctx context.Context, project, after string, limit int) ([]domain.CreatedAtEntry, error)
}

// createdAtBackfillPageLimit bounds each page BackfillCreatedAt requests —
// smaller than central's own ceiling (2000) so a slow or interrupted backfill
// commits progress in small increments rather than betting the whole project
// on one huge round-trip.
const createdAtBackfillPageLimit = 500

// httpStatusMethodNotAllowed mirrors httpStatusNotFound/httpStatusNotImplemented
// above (mirrored here without importing net/http, same discipline) — the
// FUP-005 brief names 405 explicitly alongside 404 as "server does not
// support this".
const httpStatusMethodNotAllowed = 405

// BackfillCreatedAt runs the ONE-TIME (per project) created_at backfill
// against central: pages through OriginalCreatedAt via a keyset cursor,
// applies each page to the local store (n.Store.BackfillCreatedAt — "the
// older of the two" per entry), and marks the project done so it is never
// re-attempted.
//
// Idempotent and resumable: completion is recorded ONLY after every page for
// the project has been applied. An interrupted run (crash, network loss
// mid-page) leaves the project NOT marked done, so the next call starts over
// from the first page — safe, because BackfillCreatedAt's local UPDATE
// converges to the same end state no matter how many times a page is
// re-applied (it only ever moves created_at older, never back).
//
// Compatibility: if central does not support this — an OLDER server (404),
// a method mismatch (405), or a Central lacking the createdAtLister
// capability (501, cloudserve's mapping) — this returns (false, nil): "not
// this round". The project is left NOT marked done, so a LATER call (e.g.
// after the server is upgraded) retries automatically; see
// isCreatedAtUnsupported. Any OTHER error from the fetch or the local apply
// is returned as a genuine failure.
//
// Returns whether the backfill actually ran to completion THIS call (true)
// or was skipped/already-done/deferred (false, nil error) — callers use this
// only for logging, never to gate other behavior.
func BackfillCreatedAt(ctx context.Context, n *Node, central Central, project string) (bool, error) {
	if project == "" {
		return false, nil
	}
	source, ok := central.(createdAtSource)
	if !ok {
		return false, nil // capability absent — nothing this Central will ever answer
	}

	done, err := n.Store.CreatedAtBackfillDone(project)
	if err != nil {
		return false, fmt.Errorf("BackfillCreatedAt %s[%s]: check done: %w", n.Name, project, err)
	}
	if done {
		return false, nil // the common case on every call after the first success
	}

	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("BackfillCreatedAt %s[%s]: cancelled: %w", n.Name, project, err)
		}

		page, err := source.OriginalCreatedAt(ctx, project, after, createdAtBackfillPageLimit)
		if err != nil {
			if isCreatedAtUnsupported(err) {
				return false, nil // server does not support this yet — retry a later call
			}
			return false, fmt.Errorf("BackfillCreatedAt %s[%s]: fetch page (after=%q): %w",
				n.Name, project, after, err)
		}
		if len(page) == 0 {
			break // drained — mirrors PullSince's own empty-batch contract
		}

		if _, err := n.Store.BackfillCreatedAt(page); err != nil {
			return false, fmt.Errorf("BackfillCreatedAt %s[%s]: apply page (after=%q): %w",
				n.Name, project, after, err)
		}
		after = page[len(page)-1].SyncID
	}

	if err := n.Store.MarkCreatedAtBackfillDone(project); err != nil {
		return false, fmt.Errorf("BackfillCreatedAt %s[%s]: mark done: %w", n.Name, project, err)
	}
	return true, nil
}

// isCreatedAtUnsupported reports whether err means the server does not (yet)
// support the created_at backfill:
//   - 404 — an OLDER central predating this route, matching
//     isDiscoveryUnsupported's real mixed-version case.
//   - 405 — a method mismatch, named explicitly in FUP-005's brief.
//   - 501 — the wrapped Central lacks the createdAtLister capability
//     (cloudserve's own capability-gating response).
func isCreatedAtUnsupported(err error) bool {
	var sc statusCoder
	if !errors.As(err, &sc) {
		return false
	}
	switch sc.StatusCode() {
	case httpStatusNotFound, httpStatusMethodNotAllowed, httpStatusNotImplemented:
		return true
	default:
		return false
	}
}
