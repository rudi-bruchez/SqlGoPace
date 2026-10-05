package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// docs/shrink.md promises, under a heading that says it in as many words, that a tempdb
// shrink never kills a blocker. The opposite wiring shipped in 0.13.0 (d81f143, presented
// as a fix) and lasted twenty-nine versions, because nothing but a comment held the
// promise. The wiring needs a live tempdb connection, so this reads the package source.
//
// It is an allowlist, not a search for the tempdb sampler: a first version followed the
// sampler from its constructor and three edits walked past it (a type assertion back to
// *run.ServerSampler, the same through a struct field, a widened return type). So:
//   - every SetKiller / SetVictimKiller call is made on the variable `sampler`, by name;
//   - every assignment to `sampler` is a NewServerSampler call on a probe other than
//     tempdbProbe;
//   - newTempdbSampler returns run.Sampler, which has neither method.
func TestTempdbSamplerIsNeverArmedWithKillers(t *testing.T) {
	const armed = "sampler" // the one variable that may carry killers
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var arms int
	var sawTempdbCtor bool
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "SetKiller" && sel.Sel.Name != "SetVictimKiller") {
					return true
				}
				arms++
				if id, ok := sel.X.(*ast.Ident); !ok || id.Name != armed {
					t.Errorf("%s: %s is called on something other than %q; only the main sampler may carry a killer (docs/shrink.md: a tempdb shrink never kills a blocker)",
						fset.Position(n.Pos()), sel.Sel.Name, armed)
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == armed && (i >= len(n.Rhs) || !isMainSamplerCtor(n.Rhs[i])) {
						t.Errorf("%s: %q must be assigned from NewServerSampler on a probe other than tempdbProbe, and nothing else", fset.Position(n.Pos()), armed)
					}
				}
			case *ast.ValueSpec:
				for _, id := range n.Names {
					if id.Name == armed {
						t.Errorf("%s: declare %q with := from NewServerSampler; a var declaration escapes this check", fset.Position(n.Pos()), armed)
					}
				}
			case *ast.FuncDecl:
				if n.Name.Name != "newTempdbSampler" {
					return true
				}
				sawTempdbCtor = true
				res := n.Type.Results
				ok := res != nil && len(res.List) == 1 && len(res.List[0].Names) <= 1 && isRunSampler(res.List[0].Type)
				if !ok {
					t.Errorf("%s: newTempdbSampler must return run.Sampler, which cannot be armed", fset.Position(n.Pos()))
				}
			}
			return true
		})
	}
	if !sawTempdbCtor {
		t.Fatal("found no newTempdbSampler: the wiring moved, so this test no longer checks anything; update it")
	}
	if arms == 0 {
		t.Fatal("found no SetKiller/SetVictimKiller call at all: the wiring moved, so this test no longer checks anything; update it")
	}
}

// isMainSamplerCtor reports whether e is run.NewServerSampler(probe, ...) with a probe that
// is not a tempdbProbe literal.
func isMainSamplerCtor(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return false
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "NewServerSampler" {
		return false
	}
	if lit, ok := call.Args[0].(*ast.CompositeLit); ok {
		if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "tempdbProbe" {
			return false
		}
	}
	return true
}

// isRunSampler reports whether e is the type expression run.Sampler.
func isRunSampler(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "run" && sel.Sel.Name == "Sampler"
}
