// Package remote provides an HTTP client that implements [transport.Central]
// by speaking to a [cloudserve] server over the wire.
//
// The Client translates Apply and PullSince calls into POST /v1/push and
// POST /v1/pull HTTP requests using the [syncwire] JSON envelope format.
// It is the companion of cloudserve (PR3) and together they form the
// push/pull transport layer tested end-to-end by the PR4 convergence proof.
//
// Authentication: every request is HMAC-signed with the writer's key (PR6b-2).
// The server authenticates via cloudserve's Verifier (NewKeyVerifier). Pass a
// non-empty writerID and a non-nil key to enable signing. The client signs the
// URL path ("/v1/push" or "/v1/pull"), not the full URL, matching exactly what
// the server's withAuth middleware receives in r.URL.Path.
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
	"github.com/mariesqu/engram/internal/wireauth"
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

// StatusCode returns the HTTP status code the server responded with. It lets
// callers in OTHER packages classify a specific status — e.g. a 501 meaning a
// capability is absent — WITHOUT importing this package. syncer.SyncAllProjects
// uses it via a duck-typed interface to detect an older central that does not
// implement project discovery, so that a 501 there is treated as "capability
// absent" rather than a retryable sync failure.
func (e *StatusError) StatusCode() int {
	return e.Code
}

// Client implements [transport.Central] over HTTP.
// Construct it with [New]; it is safe for concurrent use — the underlying
// *http.Client handles connection pooling.
type Client struct {
	baseURL    string
	httpClient *http.Client
	writerID   string
	key        []byte
}

// New constructs a Client targeting the cloudserve server at baseURL
// (e.g. "http://localhost:8080") with per-writer HMAC signing.
//
// writerID and key are used to sign every request:
//   - X-Writer-Id header is set to writerID.
//   - X-Signature header is set to wireauth.Sign(key, method, path, body).
//
// The path signed is the URL path only (e.g. "/v1/push"), never the full URL,
// matching exactly what the server's withAuth middleware receives in r.URL.Path.
//
// If httpClient is nil a default client with a 30-second timeout is used.
//
// baseURL may include or omit a trailing slash: New trims any trailing slashes so
// route paths (e.g. "/v1/push") are appended cleanly, never producing "//v1/push"
// (which some servers answer with a redirect that net/http would follow by
// downgrading POST to GET, silently breaking sync).
func New(baseURL string, httpClient *http.Client, writerID string, key []byte) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
		writerID:   writerID,
		key:        key,
	}
}

// buildRequest constructs a signed POST request for the given path and body.
//
// It sets Content-Type: application/json and then signs the request:
//   - X-Writer-Id: c.writerID
//   - X-Signature: wireauth.Sign(c.key, method, path, body)
//
// The path (e.g. "/v1/push") is signed directly — NOT the full URL — so the
// signature matches what the server's withAuth middleware computes from r.URL.Path.
func (c *Client) buildRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	if c.writerID != "" && len(c.key) > 0 {
		sig := wireauth.Sign(c.key, method, path, body)
		req.Header.Set(wireauth.HeaderWriterID, c.writerID)
		req.Header.Set(wireauth.HeaderSignature, sig)
	}

	return req, nil
}

// Apply pushes one mutation to the central server.
//
// It calls [syncwire.ToWire] on m, wraps it in a [syncwire.PushRequest],
// marshals to JSON, and POSTs to baseURL+"/v1/push". The request is signed with
// the writer's HMAC key via [wireauth.Sign]. On any 2xx response the optional
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

	req, err := c.buildRequest(ctx, http.MethodPost, "/v1/push", body)
	if err != nil {
		return fmt.Errorf("remote.Apply: build request: %w", err)
	}

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
// The request is signed with the writer's HMAC key via [wireauth.Sign].
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

	req, err := c.buildRequest(ctx, http.MethodPost, "/v1/pull", body)
	if err != nil {
		return nil, fmt.Errorf("remote.PullSince: build request: %w", err)
	}

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

// ListProjects fetches the distinct set of projects central knows (POST
// /v1/projects, HMAC-signed like push/pull). It is the client side of
// new-project pull discovery: [syncer.SyncAllProjects] unions these with the
// node's locally-known projects so the node pulls projects that originated on
// OTHER writers and were never written locally.
//
// Satisfies the optional projectLister capability used by SyncAllProjects. On
// any non-2xx status a [*StatusError] is returned (a 501 means the server's
// Central does not support discovery — the caller falls back to local-only
// projects). ctx cancellation is propagated as in [Apply] and [PullSince].
func (c *Client) ListProjects(ctx context.Context) ([]string, error) {
	body, err := json.Marshal(syncwire.ProjectsRequest{})
	if err != nil {
		return nil, fmt.Errorf("remote.ListProjects: marshal ProjectsRequest: %w", err)
	}

	req, err := c.buildRequest(ctx, http.MethodPost, "/v1/projects", body)
	if err != nil {
		return nil, fmt.Errorf("remote.ListProjects: build request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("remote.ListProjects: do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("remote.ListProjects: read response body: %w", err)
	}
	if len(respBody) > maxResponseBytes {
		return nil, fmt.Errorf("remote.ListProjects: response body exceeds cap of %d bytes", maxResponseBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &StatusError{Code: resp.StatusCode, Body: string(respBody)}
	}

	var pr syncwire.ProjectsResponse
	if err := json.Unmarshal(respBody, &pr); err != nil {
		return nil, fmt.Errorf("remote.ListProjects: decode ProjectsResponse: %w", err)
	}
	return pr.Projects, nil
}

// Unshare hard-deletes a project's data from central WITHOUT tombstones (POST
// /v1/unshare, HMAC-signed like push/pull). The deletion does NOT propagate to
// other nodes — they keep their copies. Returns the number of central rows
// deleted. It is the authenticated-wire equivalent of `--remote=unshare`, so the
// daemon never needs the central Postgres DSN. A 501 means the server's Central
// does not support unshare; any non-2xx status returns a *StatusError.
func (c *Client) Unshare(ctx context.Context, project string) (int, error) {
	body, err := json.Marshal(syncwire.UnshareRequest{Project: project})
	if err != nil {
		return 0, fmt.Errorf("remote.Unshare: marshal UnshareRequest: %w", err)
	}

	req, err := c.buildRequest(ctx, http.MethodPost, "/v1/unshare", body)
	if err != nil {
		return 0, fmt.Errorf("remote.Unshare: build request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("remote.Unshare: do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return 0, fmt.Errorf("remote.Unshare: read response body: %w", err)
	}
	if len(respBody) > maxResponseBytes {
		return 0, fmt.Errorf("remote.Unshare: response body exceeds cap of %d bytes", maxResponseBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, &StatusError{Code: resp.StatusCode, Body: string(respBody)}
	}

	var ur syncwire.UnshareResponse
	if err := json.Unmarshal(respBody, &ur); err != nil {
		return 0, fmt.Errorf("remote.Unshare: decode UnshareResponse: %w", err)
	}
	return int(ur.Deleted), nil
}
