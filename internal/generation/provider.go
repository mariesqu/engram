// Package generation provides the GenerationProvider port, the gated
// wrapper that enforces per-project privacy policy for LLM narrative
// synthesis, and (in a later slice) concrete provider implementations.
//
// Architecture invariant: raw providers are NEVER handed to callers outside
// this package. The daemon wiring layer (a later slice) constructs a
// gatedProvider via NewGated and hands that everywhere. There is exactly
// one place narrative-worthy observation text can leave the node — inside
// the gate — making bypass STRUCTURALLY impossible. This mirrors
// internal/embedding's identical invariant (provider.go:1-12 there),
// applied to narrative generation.
//
// EligibleForGeneration is deliberately its OWN predicate, never a reuse of
// internal/embedding.EligibleForEmbedding: narrative synthesis ships full
// observation text (every member of a unit's title and content) in one
// prompt, strictly more context per call than an embedding call ships. Its
// consent matrix is therefore stricter — see EligibleForGeneration's doc.
//
// Zero new Go module dependencies: the provider implementation added in a
// later slice uses stdlib net/http + encoding/json only, exactly like
// internal/embedding's OpenAI-compatible provider.
package generation

import (
	"context"
	"errors"

	"github.com/mariesqu/engram/internal/localstore"
)

// GenerationProvider is the port for narrative synthesis. Implementations:
// RemoteMistralProvider (a later slice), NoopProvider, gatedProvider
// (wrapper, gated.go).
//
// Generate produces narrative prose for one (project, prompt) unit. Unlike
// EmbeddingProvider.Embed, there is deliberately no batch form: a chat
// completion is one request per unit, and the per-cycle unit budget (a
// later slice) caps a cycle at a small number of calls. A batch signature
// would also make partial failure ambiguous — with one string in, one
// string out, there is no "which of N failed" bookkeeping to get wrong.
//
// Callers must never invoke Generate on a raw (ungated) provider directly;
// always use a value returned by NewGated.
type GenerationProvider interface {
	// Generate returns narrative prose for the given project and prompt.
	// The project parameter is used by gatedProvider to enforce per-project
	// policy; concrete providers receive it but may ignore it.
	Generate(ctx context.Context, project, prompt string) (string, error)

	// ModelName returns a stable identifier stored in the narrative cache's
	// model column and folded into the source-hash cache key (a later
	// slice), so a model change invalidates every cache entry.
	ModelName() string
}

// PolicyChecker is the port for per-project policy lookup. It is satisfied
// by *localstore.Store, so internal/generation does NOT need localstore to
// import it upward (localstore already imports nothing from generation).
type PolicyChecker interface {
	GetPolicy(project string) (localstore.Policy, error)
}

// ErrGenerationGated is returned by gatedProvider.Generate when the
// project's policy forbids narrative synthesis. Callers (the narrative
// loop, a later slice) must treat this as a silent skip, NOT as a
// transient error to be retried.
//
// Unlike embedding.ErrEmbeddingGated, this sentinel is declared LOCALLY
// with errors.New rather than aliased from localstore: nothing in
// localstore calls a generation provider, so there is no import-cycle risk
// forcing the alias, and declaring it here means this change requires zero
// changes to localstore's error surface.
var ErrGenerationGated = errors.New("generation: project policy forbids narrative synthesis")

// EligibleForGeneration reports whether narrative synthesis may run for a
// project with the given policy. Only localstore.PolicySynced is eligible —
// this is STRICTER than embedding.EligibleForEmbedding, which also permits
// PolicyLocalOnly given a local provider and explicit consent.
//
// Narrative synthesis ships full observation text (every unit member's
// title and content) in one prompt; an embedding call ships one string's
// vector representation. That is strictly more context per call, so the
// generation gate does not inherit embedding's local-only carve-out: a
// local-only project is denied generation unconditionally, even with a
// local provider configured, even with consent.
//
// There is deliberately NO narrative_local_consent config key, and this
// function deliberately does NOT read embedding_local_consent:
// embedding_local_consent is scoped by name and doc comment to embedding
// only (internal/config/config.go), and reusing it here would silently
// widen a consent the user granted for a narrower operation.
func EligibleForGeneration(policy localstore.Policy) bool {
	return policy == localstore.PolicySynced
}

// NoopProvider is a GenerationProvider that never produces narrative text.
// It satisfies the interface so every call site can be unconditional — no
// caller needs an "if provider != nil" guard.
type NoopProvider struct{}

// Generate always returns ("", nil) — no text, no error.
func (NoopProvider) Generate(_ context.Context, _, _ string) (string, error) {
	return "", nil
}

// ModelName returns "noop".
func (NoopProvider) ModelName() string { return "noop" }

// ensure NoopProvider satisfies GenerationProvider at compile time.
var _ GenerationProvider = NoopProvider{}

// satisfiesLocalStore verifies that *localstore.Store satisfies
// PolicyChecker. Compile-time check only; the variable is never used at
// runtime.
var _ PolicyChecker = (*localstore.Store)(nil)
