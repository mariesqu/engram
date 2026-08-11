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
