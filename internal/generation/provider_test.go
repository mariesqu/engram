package generation

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/mariesqu/engram/internal/localstore"
)

// TestGenerationProviderPortShape pins the GenerationProvider interface
// shape: exactly Generate(ctx, project, prompt) (string, error) and
// ModelName() string. Mirrored deliberately from
// internal/embedding.EmbeddingProvider but WITHOUT Dimensions() (meaningless
// for text) and WITHOUT a batch signature (a chat completion is one request
// per unit — see provider.go's doc).
func TestGenerationProviderPortShape(t *testing.T) {
	var _ GenerationProvider = NoopProvider{}

	var p GenerationProvider = NoopProvider{}
	body, err := p.Generate(context.Background(), "proj", "prompt")
	if err != nil {
		t.Fatalf("NoopProvider.Generate: %v", err)
	}
	if body != "" {
		t.Errorf("NoopProvider.Generate body = %q, want empty", body)
	}
	if got := p.ModelName(); got != "noop" {
		t.Errorf("NoopProvider.ModelName() = %q, want %q", got, "noop")
	}
}

// TestErrGenerationGatedIsDistinctSentinel pins ErrGenerationGated as a
// distinct, errors.Is-comparable sentinel — the narrative loop (a later
// slice) must be able to distinguish "gated, silently skip" from every
// other error via errors.Is.
func TestErrGenerationGatedIsDistinctSentinel(t *testing.T) {
	if !errors.Is(ErrGenerationGated, ErrGenerationGated) {
		t.Error("errors.Is(ErrGenerationGated, ErrGenerationGated) = false, want true")
	}
	other := errors.New("some unrelated error")
	if errors.Is(ErrGenerationGated, other) {
		t.Error("errors.Is(ErrGenerationGated, other) = true, want false")
	}
	if errors.Is(other, ErrGenerationGated) {
		t.Error("errors.Is(other, ErrGenerationGated) = true, want false")
	}
}

// TestEligibleForGenerationMatrix pins the full policy matrix: only
// PolicySynced is eligible. Deliberately stricter than
// embedding.EligibleForEmbedding, which also permits PolicyLocalOnly given
// a local provider and consent.
func TestEligibleForGenerationMatrix(t *testing.T) {
	tests := []struct {
		name   string
		policy localstore.Policy
		want   bool
	}{
		{"synced", localstore.PolicySynced, true},
		{"local-only", localstore.PolicyLocalOnly, false},
		{"omitted", localstore.PolicyOmitted, false},
		{"zero value", localstore.Policy(""), false},
		{"unknown", localstore.Policy("bogus-policy"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EligibleForGeneration(tt.policy); got != tt.want {
				t.Errorf("EligibleForGeneration(%q) = %v, want %v", tt.policy, got, tt.want)
			}
		})
	}
}

// _ is a compile-time signature pin: if EligibleForGeneration ever grows a
// consent parameter (mirroring embedding's `remote bool, consent ...bool`),
// this package fails to compile. That is the intended failure mode —
// narrative generation's consent matrix must never silently widen without
// every call site being forced to be re-read.
var _ func(localstore.Policy) bool = EligibleForGeneration

// TestEligibleForGenerationTakesOnlyAPolicy exists so `go test -run` can
// target the signature pin above by name. The pin itself is the
// package-level compile-time assertion; if EligibleForGeneration's
// signature ever changes, the package fails to compile before this body
// ever runs.
func TestEligibleForGenerationTakesOnlyAPolicy(t *testing.T) {
	var f func(localstore.Policy) bool = EligibleForGeneration
	if f == nil {
		t.Fatal("EligibleForGeneration must not be nil")
	}
}

// TestGenerationNeverReferencesEmbeddingConsent is a GUARD: it parses every
// .go file in this package (production AND test files) with go/parser and
// fails if internal/embedding is imported, or if the identifier
// "embedding_local_consent", "EmbeddingLocalConsent", or
// "narrative_local_consent" appears anywhere as a Go identifier (NOT as a
// string literal or comment — go/ast's Ident nodes never match text inside
// BasicLit string values or comments, so this test's own forbidden-name
// table below is safe to scan).
//
// Mutation-prove by temporarily adding a `consent ...bool` variadic to
// EligibleForGeneration (fails TestEligibleForGenerationTakesOnlyAPolicy,
// the compile-time signature pin) plus a temporary `internal/embedding`
// import somewhere in the package (fails this test's import check) —
// confirming both fail independently.
func TestGenerationNeverReferencesEmbeddingConsent(t *testing.T) {
	const forbiddenImport = "github.com/mariesqu/engram/internal/embedding"
	forbiddenIdents := map[string]bool{
		"embedding_local_consent": true,
		"EmbeddingLocalConsent":   true,
		"narrative_local_consent": true,
	}

	files := goFilesInPackageDir(t)
	if len(files) == 0 {
		t.Fatal("no .go files found — the guard would pass vacuously")
	}

	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		for _, spec := range f.Imports {
			p, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil {
				t.Fatalf("%s: unquote import %s: %v", filepath.Base(path), spec.Path.Value, uerr)
			}
			if p == forbiddenImport {
				t.Errorf("%s imports %q — internal/generation must never import internal/embedding", filepath.Base(path), p)
			}
		}

		ast.Inspect(f, func(n ast.Node) bool {
			ident, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			if forbiddenIdents[ident.Name] {
				t.Errorf("%s: identifier %q must never appear in internal/generation — "+
					"embedding_local_consent is scoped by name and doc comment to embedding "+
					"only, and narrative generation must never silently inherit or reuse it",
					filepath.Base(path), ident.Name)
			}
			return true
		})
	}
}
