// Package remote provides an HTTP client that implements [transport.Central]
// by speaking to a [cloudserve] server over the wire.
//
// The Client translates Apply and PullSince calls into POST /v1/push and
// POST /v1/pull HTTP requests using the [syncwire] JSON envelope format.
// It is the companion of cloudserve (PR3) and together they form the
// push/pull transport layer tested end-to-end by the PR4 convergence proof.
//
// Authentication (HMAC shared secret) is deferred to PR6; the client
// currently sends unauthenticated requests, matching the server.
//
// Retry / backoff is deferred to PR5; the client surfaces errors as
// [StatusError] so the caller can distinguish retryable 5xx from
// non-retryable 4xx without inspecting strings.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/syncwire"
)

// defaultTimeout is used when the caller passes a nil *http.Client.
// It is intentionally generous — sync payloads are small and the network
// round-trip dominates, but 30 s leaves room for a slow embedded-postgres
// start in CI without masking genuine hangs.
const defaultTimeout = 30 * time.Second

// maxResponseBytes is the maximum response body the client accepts. A full
// PullResponse with the default 100-mutation limit is well under 1 MiB; the server
// already rejects oversized request bodies — this mirrors that discipline on the
// response side. A larger response is rejected with an explicit error: the client
// reads maxResponseBytes+1 and fails fast if the body exceeds the cap, rather than
// silently truncating it (which would also leave the connection undrained and
// unreusable).
const maxResponseBytes = 4 << 20 // 4 MiB

// StatusError is returned whenever the server responds with a non-2xx status.
// It carries the HTTP status code and the body text so callers can:
//   - Log the exact server message without swallowing it.
//   - Distinguish retryable 5xx from non-retryable 4xx via [StatusError.Retryable].
//   - PR5 can use Retryable to drive exponential backoff without pattern-matching
//     error strings.
type StatusError struct {
	// Code is the HTTP status code returned by the server (e.g. 400, 500).
	Code int
	// Body is the full response body text. It is never truncated: a response that
	// exceeds maxResponseBytes is rejected with a separate overflow error before any
	// StatusError is constructed, so Body always holds the complete (≤ cap) body.
	Body string
}

// Error implements the error interface.
func (e *StatusError) Error() string {
	return fmt.Sprintf("remote: server returned %d: %s", e.Code, e.Body)
}

// Retryable reports whether this error is likely transient.
// 5xx responses indicate a server-side failure that may resolve on retry
// (overload, DB hiccup, restart). 4xx responses indicate a client error
// that will not be fixed by retrying (malformed request, auth failure).
func (e *StatusError) Retryable() bool {
	return e.Code >= 500
}

// Client implements [transport.Central] over HTTP.
// Construct it with [New]; it is safe for concurrent use — the underlying
// *http.Client handles connection pooling.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// New constructs a Client targeting the cloudserve server at baseURL
// (e.g. "http://localhost:8080"). If httpClient is nil a default client
// with a 30-second timeout is used.
//
// baseURL may include or omit a trailing slash: New trims any trailing slashes so
// route paths (e.g. "/v1/push") are appended cleanly, never producing "//v1/push"
// (which some servers answer with a redirect that net/http would follow by
// downgrading POST to GET, silently breaking sync).
func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient}
}

// Apply pushes one mutation to the central server.
//
// It calls [syncwire.ToWire] on m, wraps it in a [syncwire.PushRequest],
// marshals to JSON, and POSTs to baseURL+"/v1/push". On any 2xx response the optional
// [syncwire.PushResponse] body is decoded but its contents are not used (the
// server echoes the mutation_id, which the client already knows). On any
// non-2xx status a [*StatusError] is returned with the server's status code and
// body text.
//
// Apply propagates ctx cancellation: if ctx is cancelled before the response
// arrives the underlying http.Client cancels the in-flight request and returns
// promptly with an error.
func (c *Client) Apply(ctx context.Context, m domain.Mutation) error {
	wire := syncwire.ToWire(m)
	body, err := json.Marshal(syncwire.PushRequest{Mutation: wire})
	if err != nil {
		return fmt.Errorf("remote.Apply: marshal PushRequest: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/push", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("remote.Apply: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("remote.Apply: do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("remote.Apply: read response body: %w", err)
	}
	if len(respBody) > maxResponseBytes {
		return fmt.Errorf("remote.Apply: response body exceeds cap of %d bytes", maxResponseBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &StatusError{Code: resp.StatusCode, Body: string(respBody)}
	}

	// Decode the PushResponse best-effort; ignore errors (the 200 status is
	// the authoritative success signal — the body is informational).
	var pr syncwire.PushResponse
	_ = json.Unmarshal(respBody, &pr)

	return nil
}

// PullSince fetches mutations from the central server with seq > sinceSeq
// for the given project, up to limit rows (0 means server default).
//
// It marshals a [syncwire.PullRequest] and POSTs to baseURL+"/v1/pull".
// On success it decodes the [syncwire.PullResponse] and calls
// [syncwire.FromWire] on each [syncwire.WireMutation], returning the
// reconstructed []domain.Mutation in seq-ascending order (the server
// guarantees this ordering).
//
// On any non-2xx status a [*StatusError] is returned. ctx cancellation is
// propagated just as in [Apply].
func (c *Client) PullSince(ctx context.Context, project string, sinceSeq int64, limit int) ([]domain.Mutation, error) {
	body, err := json.Marshal(syncwire.PullRequest{
		Project:  project,
		SinceSeq: sinceSeq,
		Limit:    limit,
	})
	if err != nil {
		return nil, fmt.Errorf("remote.PullSince: marshal PullRequest: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/pull", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("remote.PullSince: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("remote.PullSince: do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("remote.PullSince: read response body: %w", err)
	}
	if len(respBody) > maxResponseBytes {
		return nil, fmt.Errorf("remote.PullSince: response body exceeds cap of %d bytes", maxResponseBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &StatusError{Code: resp.StatusCode, Body: string(respBody)}
	}

	var pr syncwire.PullResponse
	if err := json.Unmarshal(respBody, &pr); err != nil {
		return nil, fmt.Errorf("remote.PullSince: decode PullResponse: %w", err)
	}

	mutations := make([]domain.Mutation, 0, len(pr.Mutations))
	for i, w := range pr.Mutations {
		m, err := syncwire.FromWire(w)
		if err != nil {
			return nil, fmt.Errorf("remote.PullSince: FromWire[%d]: %w", i, err)
		}
		mutations = append(mutations, m)
	}
	return mutations, nil
}
