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
// promise. The wiring needs a live tempdb connection, so this reads the package source
// instead: every SetKiller / SetVictimKiller call must be made on a sampler that was not
// built from a tempdbProbe. Variable names do not matter; the probe does.
//
// ponytail: it follows one assignment, so a sampler passed through a helper or a struct
// field would escape it. Extend it if the wiring ever moves that way.
func TestTempdbSamplerIsNeverArmedWithKillers(t *testing.T) {
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	tempdbSamplers := map[string]bool{} // variables assigned from NewServerSampler(tempdbProbe{...}, ...)
	type armCall struct {
		recv string
		pos  token.Pos
	}
	var arms []armCall
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for i, rhs := range n.Rhs {
					call, ok := rhs.(*ast.CallExpr)
					if !ok || !isSelector(call.Fun, "NewServerSampler") || len(call.Args) == 0 || i >= len(n.Lhs) {
						continue
					}
					if lit, ok := call.Args[0].(*ast.CompositeLit); ok {
						if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "tempdbProbe" {
							if lhs, ok := n.Lhs[i].(*ast.Ident); ok {
								tempdbSamplers[lhs.Name] = true
							}
						}
					}
				}
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "SetKiller" && sel.Sel.Name != "SetVictimKiller") {
					return true
				}
				if recv, ok := sel.X.(*ast.Ident); ok {
					arms = append(arms, armCall{recv.Name, n.Pos()})
				}
			}
			return true
		})
	}
	if len(tempdbSamplers) == 0 {
		t.Fatal("found no sampler built from tempdbProbe: the wiring moved, so this test no longer checks anything; update it")
	}
	if len(arms) == 0 {
		t.Fatal("found no SetKiller/SetVictimKiller call at all: the wiring moved, so this test no longer checks anything; update it")
	}
	for _, a := range arms {
		if tempdbSamplers[a.recv] {
			t.Errorf("%s: %s is the tempdb sampler and must never be armed with a killer (docs/shrink.md: a tempdb shrink never kills a blocker)", fset.Position(a.pos), a.recv)
		}
	}
}

func isSelector(e ast.Expr, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == name
}
