package generation

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mariesqu/engram/internal/localstore"
)

// fakeGenerationProvider is a stdlib-only GenerationProvider double used by
// the gate tests. It never makes a network call.
type fakeGenerationProvider struct {
	calls int32
	model string
	body  string
	err   error
}

func (f *fakeGenerationProvider) Generate(_ context.Context, _, _ string) (string, error) {
	atomic.AddInt32(&f.calls, 1)
	return f.body, f.err
}

func (f *fakeGenerationProvider) ModelName() string {
	if f.model == "" {
		return "fake-model"
	}
	return f.model
}

// fakePolicyChecker is a stdlib-only PolicyChecker double.
type fakePolicyChecker struct {
	policy localstore.Policy
	err    error
	calls  int32
}

func (f *fakePolicyChecker) GetPolicy(_ string) (localstore.Policy, error) {
	atomic.AddInt32(&f.calls, 1)
	return f.policy, f.err
}

// TestNewGatedReturnsTheInterfaceNotTheConcreteType pins REQ-GEN-01: the
// gate constructor MUST return the port interface, never the concrete
// gated-struct type, so no caller can ever hold a reference to the raw
// (ungated) provider.
func TestNewGatedReturnsTheInterfaceNotTheConcreteType(t *testing.T) {
	fake := &fakeGenerationProvider{}
	checker := &fakePolicyChecker{policy: localstore.PolicySynced}

	var p GenerationProvider = NewGated(fake, checker)

	dynType := reflect.TypeOf(p)
	if dynType.Kind() == reflect.Ptr {
		dynType = dynType.Elem()
	}
	typeName := dynType.Name()
	if typeName == "" {
		t.Fatal("NewGated returned a value with no named dynamic type")
	}
	if r := []rune(typeName)[0]; r < 'a' || r > 'z' {
		t.Errorf("NewGated's returned dynamic type %q is exported — it must be unexported "+
			"so no caller outside this package can name it, let alone construct it directly",
			typeName)
	}
}

// TestNoExportedSymbolExposesTheRawProvider is a GUARD: it parses every
// non-test .go file in this package with go/parser and fails if any
// EXPORTED func or struct field has a type referencing gatedProvider, or if
// any EXPORTED type declares a field named "inner" — the raw provider must
// never be reachable from outside this package through any exported path.
func TestNoExportedSymbolExposesTheRawProvider(t *testing.T) {
	fset := token.NewFileSet()
	checked := 0
	for _, path := range goFilesInPackageDir(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue // test doubles are allowed to be shaped however tests need
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		checked++

		ast.Inspect(f, func(n ast.Node) bool {
			switch decl := n.(type) {
			case *ast.FuncDecl:
				if !decl.Name.IsExported() {
					return true
				}
				if decl.Type.Results != nil {
					for _, field := range decl.Type.Results.List {
						if exprReferencesGatedProvider(field.Type) {
							t.Errorf("%s: exported func %s returns a type referencing gatedProvider",
								filepath.Base(path), decl.Name.Name)
						}
					}
				}
			case *ast.TypeSpec:
				if !decl.Name.IsExported() {
					return true
				}
				st, ok := decl.Type.(*ast.StructType)
				if !ok || st.Fields == nil {
					return true
				}
				for _, field := range st.Fields.List {
					for _, name := range field.Names {
						if name.Name == "inner" {
							t.Errorf("%s: exported type %s has a field named %q — the raw "+
								"provider must never be reachable from an exported type",
								filepath.Base(path), decl.Name.Name, name.Name)
						}
					}
					if exprReferencesGatedProvider(field.Type) {
						t.Errorf("%s: exported type %s has a field referencing gatedProvider",
							filepath.Base(path), decl.Name.Name)
					}
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no production files were checked — the guard would pass vacuously")
	}
}

func exprReferencesGatedProvider(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return exprReferencesGatedProvider(t.X)
	case *ast.Ident:
		return t.Name == "gatedProvider"
	case *ast.SelectorExpr:
		return t.Sel.Name == "gatedProvider"
	}
	return false
}

// TestGateRefusesIneligibleProjects tables the full refusal matrix: synced
// delegates, local-only and omitted are refused with ErrGenerationGated
// BEFORE any call to inner, and a GetPolicy error is propagated verbatim —
// never swallowed into a refusal.
func TestGateRefusesIneligibleProjects(t *testing.T) {
	sentinel := errors.New("boom: policy lookup failed")
	tests := []struct {
		name         string
		policy       localstore.Policy
		policyErr    error
		wantErr      error
		wantDelegate bool
	}{
		{"synced delegates", localstore.PolicySynced, nil, nil, true},
		{"local-only refused", localstore.PolicyLocalOnly, nil, ErrGenerationGated, false},
		{"omitted refused", localstore.PolicyOmitted, nil, ErrGenerationGated, false},
		{"GetPolicy error propagated verbatim", localstore.PolicySynced, sentinel, sentinel, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeGenerationProvider{body: "narrative body"}
			checker := &fakePolicyChecker{policy: tt.policy, err: tt.policyErr}
			g := NewGated(fake, checker)

			body, err := g.Generate(context.Background(), "proj", "prompt")

			switch {
			case tt.wantErr == nil && err != nil:
				t.Fatalf("Generate error = %v, want nil", err)
			case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
				t.Fatalf("Generate error = %v, want errors.Is match for %v", err, tt.wantErr)
			}

			if tt.wantDelegate {
				if body != "narrative body" {
					t.Errorf("Generate body = %q, want delegated body %q", body, "narrative body")
				}
				if got := atomic.LoadInt32(&fake.calls); got != 1 {
					t.Errorf("inner.Generate calls = %d, want 1 (delegated)", got)
				}
			} else if got := atomic.LoadInt32(&fake.calls); got != 0 {
				t.Errorf("inner.Generate calls = %d, want 0 (refused before delegation)", got)
			}
		})
	}
}

// TestGatedProjectIssuesZeroProviderCalls is the proposal's own named
// success criterion: a local-only or omitted project must produce ZERO
// requests to the underlying provider.
func TestGatedProjectIssuesZeroProviderCalls(t *testing.T) {
	for _, pol := range []localstore.Policy{localstore.PolicyLocalOnly, localstore.PolicyOmitted} {
		t.Run(string(pol), func(t *testing.T) {
			fake := &fakeGenerationProvider{}
			checker := &fakePolicyChecker{policy: pol}
			g := NewGated(fake, checker)

			if _, err := g.Generate(context.Background(), "proj", "prompt"); !errors.Is(err, ErrGenerationGated) {
				t.Fatalf("Generate error = %v, want ErrGenerationGated", err)
			}
			if got := atomic.LoadInt32(&fake.calls); got != 0 {
				t.Errorf("inner.Generate calls = %d, want 0", got)
			}
		})
	}
}

// TestGateCallsGetPolicyExactlyOnce mirrors internal/embedding/gated.go's
// per-call GetPolicy discipline: one lookup per Generate call, no more.
func TestGateCallsGetPolicyExactlyOnce(t *testing.T) {
	fake := &fakeGenerationProvider{body: "ok"}
	checker := &fakePolicyChecker{policy: localstore.PolicySynced}
	g := NewGated(fake, checker)

	if _, err := g.Generate(context.Background(), "proj", "prompt"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got := atomic.LoadInt32(&checker.calls); got != 1 {
		t.Errorf("GetPolicy calls = %d, want exactly 1", got)
	}
}

// TestNoCallSiteConstructsARawProviderOutsideTheGate is a GUARD: it parses
// every .go file under internal/ and cmd/ (excluding this package's own
// directory) looking for a call of the form `generation.NewXxx(...)` where
// Xxx != "Gated" — i.e. any raw generation-provider constructor invoked
// somewhere other than through NewGated. The one documented exception is
// cmd/engram/narrative_probe.go (a later slice): the probe has no project,
// so there is no policy to gate against.
//
// This CANNOT fail until a raw constructor exists (Phase 10, a later
// slice) — its mutation proof is deferred to that slice's own task, which
// temporarily adds a raw constructor call to cmd/engram/daemon.go, confirms
// this test names the offending file, then reverts.
func TestNoCallSiteConstructsARawProviderOutsideTheGate(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	const genPkgPath = "github.com/mariesqu/engram/internal/generation"
	genPkgDir := filepath.Join(repoRoot, "internal", "generation")
	allowedFile := filepath.ToSlash(filepath.Join("cmd", "engram", "narrative_probe.go"))

	fset := token.NewFileSet()
	checked := 0

	for _, root := range []string{filepath.Join(repoRoot, "internal"), filepath.Join(repoRoot, "cmd")} {
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path == genPkgDir {
					return fs.SkipDir // the gate's own constructor and doubles live here legitimately
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}

			rel, relErr := filepath.Rel(repoRoot, path)
			if relErr != nil {
				return relErr
			}
			rel = filepath.ToSlash(rel)

			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			checked++

			if rel == allowedFile {
				return nil // documented exception: no project, no policy to gate
			}

			pkgIdent := ""
			for _, imp := range f.Imports {
				p, uerr := strconv.Unquote(imp.Path.Value)
				if uerr != nil {
					return uerr
				}
				if p != genPkgPath {
					continue
				}
				if imp.Name != nil {
					pkgIdent = imp.Name.Name
				} else {
					pkgIdent = "generation"
				}
			}
			if pkgIdent == "" {
				return nil
			}

			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok || ident.Name != pkgIdent {
					return true
				}
				if sel.Sel.Name == "NewGated" {
					return true // the gate itself — the one allowed constructor
				}
				if strings.HasPrefix(sel.Sel.Name, "New") {
					t.Errorf("%s: calls %s.%s — every raw generation-provider constructor "+
						"must be reached only through generation.NewGated, never called "+
						"directly outside internal/generation (the only documented "+
						"exception is %s, which has no project and therefore no policy "+
						"to gate against)", rel, pkgIdent, sel.Sel.Name, allowedFile)
				}
				return true
			})
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", root, walkErr)
		}
	}
	if checked == 0 {
		t.Fatal("no files were checked — the guard would pass vacuously")
	}
}
