// Package embedding provides the EmbeddingProvider port, the gated wrapper
// that enforces per-project privacy policy, and concrete provider implementations.
//
// Architecture invariant: raw providers (RemoteOpenAIProvider, NoopProvider)
// are NEVER handed to callers outside this package. The daemon wiring layer
// constructs gatedProvider via NewGated and hands that everywhere. There is
// exactly one place text can leave the node — inside the gate — making bypass
// STRUCTURALLY impossible.
//
// Zero new Go module dependencies: OpenAI provider uses stdlib net/http +
// encoding/json only. Cosine and RRF math is hand-written.
package embedding

import (
	"context"

	"github.com/mariesqu/engram/internal/localstore"
)

// EmbeddingProvider is the port for all embedding operations.
// Implementations: RemoteOpenAIProvider, NoopProvider, gatedProvider (wrapper).
//
// Embed returns one []float32 per input text in the same order.
// Callers must never invoke Embed on a raw (ungated) provider directly;
// always use a value returned by NewGated.
type EmbeddingProvider interface {
	// Embed embeds a batch of texts for the given project. Returns one []float32
	// per input in the same order. The project parameter is used by gatedProvider
	// to enforce per-project policy; concrete providers receive it but may ignore it.
	Embed(ctx context.Context, project string, texts []string) ([][]float32, error)

	// Dimensions returns the fixed dimensionality of all vectors this provider
	// produces. NoopProvider returns 0. RemoteOpenAIProvider returns 256.
	Dimensions() int

	// ModelName returns a stable identifier stored in embedding_model per row.
	ModelName() string
}

// PolicyChecker is the port for per-project policy lookup. It is satisfied
// by *localstore.Store so internal/embedding does NOT need to import localstore
// upward (localstore already imports nothing from embedding).
type PolicyChecker interface {
	GetPolicy(project string) (localstore.Policy, error)
}

// ErrEmbeddingGated is returned by gatedProvider.Embed when the project's
// policy forbids embedding (omitted, or local-only with a remote provider).
// Callers (the backfill loop, the search path) must treat this as a silent
// skip, NOT as a transient error to be retried.
//
// The canonical sentinel lives in localstore next to the EmbedQueryFn
// contract (this package imports localstore; the reverse import would cycle).
// This alias keeps the natural name available to embedding callers.
var ErrEmbeddingGated = localstore.ErrEmbeddingGated

// EligibleForEmbedding is a pure, zero-IO function that reports whether a
// memory with the given project policy may be embedded by the configured
// provider.
//
// Consent matrix (embedding-privacy spec, decision 5 + PR-2 sidecar consent):
//
//	omitted    + any provider             → false (never embed)
//	local-only + remote=true              → false (text must not leave the node)
//	local-only + remote=false, consent=false → false (explicit opt-in required)
//	local-only + remote=false, consent=true  → true  (user granted local consent)
//	synced     + any provider             → true
//
// The 'remote' parameter is true for RemoteOpenAIProvider (text leaves the node)
// and false for NoopProvider or OllamaSidecarProvider (local sidecar).
// 'consent' is the embedding_local_consent config flag; it is only checked when
// remote=false and policy=local-only.
func EligibleForEmbedding(policy localstore.Policy, remote bool, consent ...bool) bool {
	localConsent := len(consent) > 0 && consent[0]
	switch policy {
	case localstore.PolicySynced:
		return true
	case localstore.PolicyLocalOnly:
		if remote {
			return false // text must not leave the node
		}
		return localConsent // local sidecar requires explicit consent
	default: // PolicyOmitted or unknown
		return false
	}
}

// ── NoopProvider ─────────────────────────────────────────────────────────────

// NoopProvider is the default EmbeddingProvider when no API key or provider is
// configured. It satisfies the interface so all embedding call-sites are
// unconditional (no "if provider != nil" guards needed). Rows remain with
// embedding IS NULL and search degrades transparently to FTS.
type NoopProvider struct{}

// Embed returns (nil, nil) — no vectors, no error.
func (NoopProvider) Embed(_ context.Context, _ string, _ []string) ([][]float32, error) {
	return nil, nil
}

// Dimensions returns 0 for the noop provider.
func (NoopProvider) Dimensions() int { return 0 }

// ModelName returns "noop".
func (NoopProvider) ModelName() string { return "noop" }
