package obsidian

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// forbiddenImport is the whole point of this file. internal/obsidian MUST NOT
// import internal/localstore — design constraint 14 / REQ-WATCH-10.
//
// localstore.Open is NOT a read-only open: it runs ApplySchema then
// runMigrations on EVERY invocation (internal/localstore/store.go). A repeating,
// unattended open from a second — possibly version-mismatched — binary would
// migrate the schema out from under a running daemon. That is verify-report
// #4710's MAJOR-10, and moving the scheduled export into the daemon process is
// what closes it: the loop uses the store the daemon ALREADY has open, reached
// through the narrow StoreReader interface and its behaviour-free adapter in
// cmd/engram.
//
// That guarantee is only as strong as this test. An `import
// "…/internal/localstore"` added to this package would compile fine, pass every
// other test, and silently reopen the door — which is exactly why the design
// specified a MECHANICAL check rather than a comment or review discipline.
const forbiddenImport = "github.com/mariesqu/engram/internal/localstore"

// TestObsidianPackageDoesNotImportLocalstore parses every .go file in this
// package (production AND test files) with go/parser and fails if any of them
// imports internal/localstore.
//
// Test files are included on purpose: a test that opened a real store would
// reintroduce the coupling this constraint forbids and would make the
// production-only version of this check a half-truth.
func TestObsidianPackageDoesNotImportLocalstore(t *testing.T) {
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
			if p == forbiddenImport {
				t.Errorf(
					"%s imports %q — internal/obsidian must NEVER import internal/localstore "+
						"(design constraint 14 / REQ-WATCH-10). localstore.Open runs schema "+
						"migrations on every call; the daemon is the single store owner and "+
						"hands this package a StoreReader.",
					filepath.Base(path), p)
			}
		}
	}
	if checked < 10 {
		t.Errorf("only %d files parsed; the package has many more — the guard is not covering the package", checked)
	}
}

// TestObsidianPackageImportsOnlyStdlibAndDomain is the triangulation partner:
// it pins the POSITIVE shape of the dependency set, not just the one forbidden
// edge. Today the only non-stdlib import in the whole package is
// internal/domain. A new internal/* dependency of any kind is a design event
// that should be a deliberate decision, so it fails here first.
func TestObsidianPackageImportsOnlyStdlibAndDomain(t *testing.T) {
	const allowed = "github.com/mariesqu/engram/internal/domain"

	fset := token.NewFileSet()
	for _, path := range goFilesInPackageDir(t) {
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
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
			if p != allowed {
				t.Errorf("%s imports %q — internal/obsidian may only depend on the standard "+
					"library and %q (design constraint 6: zero new module dependencies; "+
					"constraint 14: no localstore)", filepath.Base(path), p, allowed)
			}
		}
	}
}

// goFilesInPackageDir lists every .go file next to this test. Using the working
// directory is safe and deliberate: `go test` always runs a package's tests
// with the package directory as the working directory, so no build-tag-aware
// package loading (and no non-stdlib helper) is needed.
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
