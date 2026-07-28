package generation

import "context"

// gatedProvider is the ONLY edge at which narrative-worthy observation text
// may cross to a generation provider. It wraps an inner GenerationProvider
// and enforces per-project policy via PolicyChecker before every Generate
// call.
//
// Construction: NewGated returns a GenerationProvider (not *gatedProvider),
// so no caller ever holds a reference to the inner raw provider. This makes
// bypass structurally impossible: you cannot call inner.Generate without
// going through the gate. Mirrors internal/embedding/gated.go's
// gatedProvider on the one property that matters — VERIFIED there:
// NewGated returns EmbeddingProvider, never *gatedProvider.
//
// Unlike embedding's gatedProvider, this type carries no `remote` and no
// `consent` field: EligibleForGeneration branches on policy alone (see its
// doc in provider.go), so either field would be inert state that LIES about
// the decision space. NewGated therefore takes exactly two arguments.
type gatedProvider struct {
	inner   GenerationProvider // raw provider — NEVER exposed outside this package
	checker PolicyChecker
}

// NewGated constructs the single gated wrapper around inner. The returned
// GenerationProvider is what daemon wiring hands everywhere (a later
// slice) — no caller ever receives a reference to inner directly.
func NewGated(inner GenerationProvider, checker PolicyChecker) GenerationProvider {
	return &gatedProvider{
		inner:   inner,
		checker: checker,
	}
}

// Generate enforces the privacy gate before delegating to the inner
// provider.
//
// Gate logic:
//   - Fetches the project's policy via checker.GetPolicy.
//   - Returns ErrGenerationGated when EligibleForGeneration returns false —
//     BEFORE any call to inner.
//   - Delegates to inner.Generate only when eligible.
//
// Evaluated per call, so a policy flip is reflected on the very next unit
// (GetPolicy is a cached lookup — see internal/localstore/policy.go).
func (g *gatedProvider) Generate(ctx context.Context, project, prompt string) (string, error) {
	pol, err := g.checker.GetPolicy(project)
	if err != nil {
		return "", err
	}

	if !EligibleForGeneration(pol) {
		return "", ErrGenerationGated
	}

	return g.inner.Generate(ctx, project, prompt)
}

// ModelName delegates unconditionally to the inner provider. Used by the
// narrative cache (a later slice) to record the producing model and detect
// a model change that invalidates every cache entry.
func (g *gatedProvider) ModelName() string { return g.inner.ModelName() }

// ensure gatedProvider satisfies GenerationProvider at compile time.
var _ GenerationProvider = (*gatedProvider)(nil)
