// Command facadegen generates the root loom facade (aliases.go) from the
// exported top-level declarations of internal/engine.
//
// The facade re-exports every exported type (as an alias), const, var, and func
// so that applications import github.com/rhaqim/loom instead of the internal
// package. Declarations are grouped by their source file (a domain, after the
// Phase 0 split) and carry the doc comment copied from their source
// declaration, so the docs have a single home in internal/engine and the facade
// stays a readable, sectioned API reference.
//
// Usage: go run ./internal/tools/facadegen <engine-dir> <out-file>
package main

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// entry is one re-exported symbol.
type entry struct {
	name string
	kind string // "type", "const", "var", "func"
	doc  string // cleaned doc comment (no // markers), may be empty
	line int    // source line, for ordering within a group
}

// sectionOrder is the preferred reading order for the domain groups. Files not
// listed here are appended alphabetically, so a new domain file still shows up.
var sectionOrder = []string{
	"engine.go", "api_session.go", "api_agent.go", "api_prompt.go",
	"responseformat.go", "api_action.go", "api_step.go", "api_result.go",
	"api_generator.go", "api_modality.go", "flow.go", "flow_store.go",
	"hook.go", "schemahook.go", "judges.go", "api_cost.go", "api_budget.go",
	"cache.go", "bus.go", "pricing.go", "gc_service.go", "poller.go",
	"query.go", "api_errors.go",
}

// sectionTitle maps a source file to a human title. Unmapped files fall back to
// a title derived from the filename.
var sectionTitle = map[string]string{
	"engine.go":         "Engine, configuration & core types",
	"api_session.go":    "Sessions & state",
	"api_agent.go":      "Agents",
	"api_prompt.go":     "Prompts",
	"responseformat.go": "Response formats",
	"api_action.go":     "Actions",
	"api_step.go":       "Steps",
	"api_result.go":     "Results",
	"api_generator.go":  "Generators",
	"api_modality.go":   "Modality",
	"flow.go":           "Flows & turns",
	"flow_store.go":     "Flow persistence",
	"hook.go":           "Hooks",
	"schemahook.go":     "Schema validation",
	"judges.go":         "Judges",
	"api_cost.go":       "Cost tracking",
	"api_budget.go":     "Budgets",
	"cache.go":          "Caching",
	"bus.go":            "Event bus",
	"pricing.go":        "Pricing",
	"gc_service.go":     "Garbage collection",
	"poller.go":         "Async task poller",
	"query.go":          "Query helpers",
	"api_errors.go":     "Errors & control-flow sentinels",
}

func titleFor(file string) string {
	if t, ok := sectionTitle[file]; ok {
		return t
	}
	name := strings.TrimSuffix(file, ".go")
	for _, p := range []string{"api_", "store_", "service_"} {
		name = strings.TrimPrefix(name, p)
	}
	name = strings.ReplaceAll(name, "_", " ")
	if name == "" {
		return file
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func docText(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	return strings.TrimRight(cg.Text(), "\n")
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: facadegen <engine-dir> <out-file>")
		os.Exit(2)
	}
	dir, out := os.Args[1], os.Args[2]
	fset := token.NewFileSet()
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))

	groups := map[string][]entry{} // file basename -> entries
	seen := map[string]bool{}
	nTypes, nConsts, nVars := 0, 0, 0

	add := func(file string, e entry) {
		if e.name == "" || !ast.IsExported(e.name) || seen[e.name] {
			return
		}
		seen[e.name] = true
		groups[file] = append(groups[file], e)
		switch e.kind {
		case "type":
			nTypes++
		case "const":
			nConsts++
		default:
			nVars++
		}
	}

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			fmt.Fprintln(os.Stderr, "parse:", err)
			os.Exit(1)
		}
		base := filepath.Base(f)
		for _, d := range af.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				if decl.Recv != nil || !decl.Name.IsExported() {
					continue
				}
				if decl.Type.TypeParams != nil {
					fmt.Fprintln(os.Stderr, "SKIP generic func", decl.Name.Name)
					continue
				}
				add(base, entry{decl.Name.Name, "func", docText(decl.Doc), fset.Position(decl.Pos()).Line})
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if !s.Name.IsExported() {
							continue
						}
						if s.TypeParams != nil {
							fmt.Fprintln(os.Stderr, "SKIP generic type", s.Name.Name)
							continue
						}
						doc := docText(s.Doc)
						if doc == "" && len(decl.Specs) == 1 {
							doc = docText(decl.Doc)
						}
						add(base, entry{s.Name.Name, "type", doc, fset.Position(s.Pos()).Line})
					case *ast.ValueSpec:
						kind := "var"
						if decl.Tok == token.CONST {
							kind = "const"
						}
						doc := docText(s.Doc)
						if doc == "" && len(decl.Specs) == 1 {
							doc = docText(decl.Doc)
						}
						for _, n := range s.Names {
							if !n.IsExported() {
								continue
							}
							add(base, entry{n.Name, kind, doc, fset.Position(n.Pos()).Line})
						}
					}
				}
			}
		}
	}

	// Order the sections: preferred files first, then the rest alphabetically.
	rank := map[string]int{}
	for i, f := range sectionOrder {
		rank[f] = i
	}
	var ordered []string
	for f := range groups {
		ordered = append(ordered, f)
	}
	sort.Slice(ordered, func(i, j int) bool {
		ri, oki := rank[ordered[i]]
		rj, okj := rank[ordered[j]]
		switch {
		case oki && okj:
			return ri < rj
		case oki != okj:
			return oki // ranked files come before unranked
		default:
			return ordered[i] < ordered[j]
		}
	})

	var b strings.Builder
	b.WriteString("// Code generated by internal/tools/facadegen; DO NOT EDIT.\n")
	b.WriteString("// Regenerate with: go generate ./...\n")
	b.WriteString("//\n")
	b.WriteString("// This is the public API of github.com/rhaqim/loom. Every declaration is a\n")
	b.WriteString("// re-export of internal/engine: types are aliases (identical types across the\n")
	b.WriteString("// boundary), and consts/vars/funcs are value re-exports. Docs are copied from\n")
	b.WriteString("// the source declarations in internal/engine, which remain the single source of\n")
	b.WriteString("// truth. Sections group the surface by domain.\n\n")
	b.WriteString("package loom\n\n")
	b.WriteString("import engine \"github.com/rhaqim/loom/internal/engine\"\n\n")

	const rule = "////////////////////////////////////////////////////////////////////////////////"
	for _, file := range ordered {
		es := groups[file]
		sort.SliceStable(es, func(i, j int) bool { return es[i].line < es[j].line })
		title := titleFor(file)
		fmt.Fprintf(&b, "%s\n// %s\n%s\n\n", rule, title, rule)
		for _, e := range es {
			if e.doc != "" {
				for _, line := range strings.Split(e.doc, "\n") {
					if line == "" {
						b.WriteString("//\n")
					} else {
						fmt.Fprintf(&b, "// %s\n", line)
					}
				}
			}
			switch e.kind {
			case "type":
				fmt.Fprintf(&b, "type %s = engine.%s\n\n", e.name, e.name)
			case "const":
				fmt.Fprintf(&b, "const %s = engine.%s\n\n", e.name, e.name)
			default: // var, func
				fmt.Fprintf(&b, "var %s = engine.%s\n\n", e.name, e.name)
			}
		}
	}

	// Format the output so `go generate` alone produces gofmt-canonical bytes;
	// otherwise the CI facade-sync check would diff the generated file against
	// the (gofmt'd) committed one even when the surface is unchanged.
	formatted, err := format.Source([]byte(b.String()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "format:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(out, formatted, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("facade: %d types, %d consts, %d vars across %d sections\n", nTypes, nConsts, nVars, len(ordered))
}
