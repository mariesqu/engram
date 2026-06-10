package embedding

import (
	"context"

	"github.com/mariesqu/engram/internal/localstore"
)

// gatedProvider is the ONLY edge at which memory text may cross to an
// embedding provider. It wraps an inner EmbeddingProvider and enforces
// per-project policy via PolicyChecker before every Embed call.
//
// Construction: NewGated returns an EmbeddingProvider (not *gatedProvider),
// so no caller ever holds a reference to the inner raw provider. This makes
// bypass structurally impossible: you cannot call inner.Embed without going
// through the gate.
//
// The 'remote' field signals whether 'inner' sends text off-node. Set true
// for RemoteOpenAIProvider, false for NoopProvider or any future local sidecar.
// The 'consent' field is reserved for PR-2 (local-sidecar consent). In PR-1
// it is always false — gatedProvider is constructed without a consent flag.
type gatedProvider struct {
	inner   EmbeddingProvider // raw provider — NEVER exposed outside this package
	checker PolicyChecker
	remote  bool // true → inner sends text off-node (OpenAI)
	consent bool // PR-2: separate sidecar-consent flag; always false in PR-1
}

// NewGated constructs the single gated wrapper around inner. The returned
// EmbeddingProvider is what the daemon wiring hands everywhere — no caller
// ever receives a reference to inner directly.
//
// remote should be true for RemoteOpenAIProvider (text leaves the node) and
// false for NoopProvider or a future local sidecar provider.
func NewGated(inner EmbeddingProvider, checker PolicyChecker, remote bool) EmbeddingProvider {
	return &gatedProvider{
		inner:   inner,
		checker: checker,
		remote:  remote,
		consent: false, // PR-2 will expose this parameter
	}
}

// Embed enforces the privacy gate before delegating to the inner provider.
//
// Gate logic (mirrors EligibleForEmbedding + PolicyChecker):
//   - Fetches the project's policy via checker.GetPolicy.
//   - Returns ErrEmbeddingGated when EligibleForEmbedding returns false.
//   - Delegates to inner.Embed only when eligible.
//
// This is the single choke-point mandated by the embedding-privacy spec.
// It is evaluated per-call so a policy flip mid-backfill is reflected
// immediately on the NEXT row processed (GetPolicy is a cached lookup,
// per localstore policy.go, so the cost is negligible).
func (g *gatedProvider) Embed(ctx context.Context, project string, texts []string) ([][]float32, error) {
	pol, err := g.checker.GetPolicy(project)
	if err != nil {
		return nil, err
	}

	if !EligibleForEmbedding(pol, g.remote) {
		return nil, ErrEmbeddingGated
	}

	return g.inner.Embed(ctx, project, texts)
}

// Dimensions delegates unconditionally to the inner provider.
// The gate does not constrain dimensionality — that is a property of the
// underlying model, not the policy.
func (g *gatedProvider) Dimensions() int { return g.inner.Dimensions() }

// ModelName delegates unconditionally to the inner provider.
// Used by the backfill loop to record the producing model in embedding_model.
func (g *gatedProvider) ModelName() string { return g.inner.ModelName() }

// NOTE (PR-2 seam): the consent flag on gatedProvider is wired but always
// false in PR-1 — there is deliberately NO constructor that sets it. PR-2
// adds the consent-aware constructor alongside the sidecar provider and the
// explicit consent setting.

// ensure gatedProvider satisfies EmbeddingProvider at compile time.
var _ EmbeddingProvider = (*gatedProvider)(nil)

// ensure NoopProvider satisfies EmbeddingProvider at compile time.
var _ EmbeddingProvider = NoopProvider{}

// satisfiesLocalStore verifies that *localstore.Store satisfies PolicyChecker.
// This is a compile-time check only; the variable is never used at runtime.
var _ PolicyChecker = (*localstore.Store)(nil)
