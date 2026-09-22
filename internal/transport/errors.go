package transport

import "errors"

// ErrResponseTooLarge is the sentinel a [Central] implementation returns —
// wrapped with %w — when a response body exceeds a transport-level size cap.
//
// Contract for implementations: if the transport refuses to buffer a response
// because it is larger than the implementation's cap, the returned error MUST
// wrap this sentinel so callers can match it with errors.Is. The wrapping error
// should still name the method and the cap for operators reading logs.
//
// Contract for callers: an ErrResponseTooLarge from PullSince is NOT a fatal
// condition and NOT a "the central is broken" signal — it means "this batch was
// too big for one response". The caller may retry the SAME cursor with a smaller
// limit. syncer.Pull does exactly that: it halves the limit and retries, which
// terminates because central's push path caps a single mutation's request body
// (cloudserve maxRequestBytes, 1 MiB) well below any sane response cap, so a
// batch of one always fits. Without that retry the pull cursor never advances
// and every subsequent round re-requests the identical oversized window —
// a permanent sync wedge (the v1.5.2 "response body exceeds cap" bug).
//
// PullSince is the ONLY operation with a shrink-and-retry path, because it is the
// only one whose response size the caller can influence (via limit). On the
// capability calls a transport may also expose — ListProjects, Unshare, State —
// the sentinel is classification only: there is no knob to turn, so an overflow
// there has no automatic remediation and surfaces to the operator.
//
// Scope note: match this sentinel at the Pull / PullSince call site. Above the
// per-project loop, syncer.SyncAllProjects joins failures into an aggregate error;
// that aggregate does implement Unwrap() []error, so errors.Is still traverses it
// (Go 1.20+ multi-error semantics) — but a hit there only tells you that AT LEAST
// ONE project overflowed, not which, and the remediation has already happened
// inside Pull. Use it for reporting, never to drive a retry decision.
var ErrResponseTooLarge = errors.New("transport: response exceeds size cap")

// ErrPermanent is the sentinel a [Central] implementation returns — wrapped
// with %w — when Apply rejects a mutation for a DETERMINISTIC reason that
// retrying will never fix: a validation failure this store enforces itself, or
// a Postgres data exception / integrity-constraint violation (SQLSTATE class
// 22 / 23). It is the counterpart to the plain (unwrapped) error Apply returns
// for everything else — a transient outage, a lost connection, a DB restart —
// which callers must keep retrying.
//
// Contract for implementations: wrap ErrPermanent ONLY when the mutation
// itself is at fault, never for infrastructure trouble, and never include the
// database's raw error text (e.g. a Postgres DETAIL clause can embed the
// actual offending row value) — the wrapped error's message reaches the pushing
// client verbatim over HTTP (see cloudserve's 422 mapping), so it must already
// be safe to show: derive it from structured identifiers (a constraint name, a
// SQLSTATE code, a field name), never from the driver's free-text message.
//
// Contract for callers: an ErrPermanent from Apply/push means THIS SPECIFIC
// mutation cannot ever succeed against this central and must not be retried —
// the caller should PARK it (and, for an ordered group such as one sync_id's
// version chain, everything queued behind it) rather than back off and resend
// it forever. cloudserve maps it to HTTP 422; a client's Retryable() must
// return false for it exactly as it already does for a 4xx StatusError.
var ErrPermanent = errors.New("transport: mutation permanently rejected")
