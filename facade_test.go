package loom_test

// This is the facade drift guard. It fails when a top-level identifier is
// exported from internal/engine but not re-exported by the root loom facade
// (aliases.go). When it fails, run `go generate ./...` to regenerate the facade.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// exportedTopLevel returns the set of exported top-level identifiers declared in
// the non-test .go files of dir (funcs without receivers, types, consts, vars).
func exportedTopLevel(t *testing.T, dir string) map[string]bool {
	t.Helper()
	set := map[string]bool{}
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				if decl.Recv == nil && decl.Name.IsExported() {
					set[decl.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							set[s.Name.Name] = true
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								set[n.Name] = true
							}
						}
					}
				}
			}
		}
	}
	return set
}

func TestFacadeReExportsAllEngineSymbols(t *testing.T) {
	engineSyms := exportedTopLevel(t, filepath.Join("internal", "engine"))
	facadeSyms := exportedTopLevel(t, ".")

	var missing []string
	for name := range engineSyms {
		if !facadeSyms[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("facade is missing %d re-exports from internal/engine (run `go generate ./...`): %v",
			len(missing), missing)
	}
}
