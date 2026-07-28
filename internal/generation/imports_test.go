package generation

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// allowedGenerationImports is the exact set of non-stdlib import paths this
// package may depend on. internal/domain provides the pure Record type
// (used starting in a later slice, for prompt/cache-key assembly);
// internal/localstore provides Policy, Store (via PolicyChecker) and, in a
// later slice, the narratives cache reader/writer and Store.GetSession for
// generation-time path verification.
//
// internal/obsidian and internal/embedding are explicitly forbidden:
// internal/obsidian must never import internal/generation and
// internal/generation must not import internal/obsidian either (REQ-GEN-08
// — the import boundary is symmetric by construction, not just by
// convention); internal/embedding is a sibling privacy-gated subsystem
// whose consent semantics must not leak here — see
// TestGenerationNeverReferencesEmbeddingConsent in provider_test.go.
var allowedGenerationImports = map[string]bool{
	"github.com/mariesqu/engram/internal/domain":     true,
	"github.com/mariesqu/engram/internal/localstore": true,
}

// TestGenerationPackageImportsOnlyStdlibDomainAndLocalstore mirrors the
// doctrine of internal/obsidian/imports_test.go
// (TestObsidianPackageImportsOnlyStdlibAndDomain): it parses every .go file
// in this package (production AND test files) and fails a mechanical guard
// — not a review — on any non-stdlib import outside the allow-list.
func TestGenerationPackageImportsOnlyStdlibDomainAndLocalstore(t *testing.T) {
	files := goFilesInPackageDir(t)
	if len(files) == 0 {
		t.Fatal("no .go files found in the package directory — the guard would pass vacuously")
	}

	fset := token.NewFileSet()
	checked := 0
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		checked++
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote import %s: %v", filepath.Base(path), spec.Path.Value, err)
			}
			// Stdlib import paths never contain a dot in their first segment.
			first := p
			if i := strings.IndexByte(p, '/'); i >= 0 {
				first = p[:i]
			}
			if !strings.Contains(first, ".") {
				continue // stdlib
			}
			if allowedGenerationImports[p] {
				continue
			}
			reason := "not in the allow-list (internal/domain, internal/localstore only)"
			switch p {
			case "github.com/mariesqu/engram/internal/obsidian":
				reason = "internal/obsidian must never import internal/generation, and the " +
					"converse must also never happen (REQ-GEN-08) — internal/obsidian reaches " +
					"cached narrative data only through its own narrow reader interface"
			case "github.com/mariesqu/engram/internal/embedding":
				reason = "internal/generation must not inherit internal/embedding's consent " +
					"semantics — see TestGenerationNeverReferencesEmbeddingConsent"
			}
			t.Errorf("%s imports %q — %s", filepath.Base(path), p, reason)
		}
	}
	if checked == 0 {
		t.Errorf("no files were parsed; the guard is not covering the package")
	}
}

// goFilesInPackageDir lists every .go file next to this test. Using the
// working directory is safe and deliberate: `go test` always runs a
// package's tests with the package directory as the working directory
// (mirrors internal/obsidian/imports_test.go's identical helper).
func goFilesInPackageDir(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}
