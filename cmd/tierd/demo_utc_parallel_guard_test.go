package main

// Enforces the t.Parallel() prohibition that demo_utc_test.go states in prose.
//
// TestSeedDemo_StampsUTCTimestamps and TestDemoThenWebhook_OrderByTSIsInstantOrder
// assign the PROCESS-GLOBAL time.Local. That is safe only while they are
// sequential: Go resumes parallel top-level tests after the sequential pass
// completes, so a non-parallel test never overlaps with them. Add t.Parallel() to
// either one and it would leak a fake zone into every test running beside it —
// and have its own zone yanked away mid-run. The failure would show up as a
// confusing, intermittent, zone-dependent flake in an unrelated test.
//
// #723's whole argument is that a comment saying "don't do this" is not a guard:
// cmd/tierd/demo.go sat un-normalized while a comment asserted every writer
// normalized. So this prohibition is enforced structurally, the same way
// internal/store/orderedts_writers_test.go enforces the writer set.
//
// ⚠️ SCOPE, so nobody over-reads a green: this checks the files listed below, by
// AST. It does NOT stop a NEW file from mutating time.Local inside a parallel
// test. That would want a package-wide scan for `time.Local =`, worth adding if a
// third such file ever appears; today the honest claim is that it pins the ones
// that exist, and the staleness check below fails if a listed file stops
// qualifying.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// timeLocalMutatingFiles are the files whose tests assign time.Local and must
// therefore stay sequential.
var timeLocalMutatingFiles = []string{"demo_utc_test.go"}

func TestTimeLocalMutatingTests_AreNotParallel(t *testing.T) {
	for _, name := range timeLocalMutatingFiles {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		// Staleness / vacuity control: if the file no longer assigns time.Local,
		// this entry is pinning nothing and should be removed rather than left to
		// read as protection it is not providing.
		mutates := false
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range as.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Local" {
					continue
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "time" {
					mutates = true
				}
			}
			return true
		})
		if !mutates {
			t.Errorf("%s no longer assigns time.Local — drop it from "+
				"timeLocalMutatingFiles; a stale entry makes this guard read as "+
				"protection it is not providing", name)
			continue
		}

		checked := 0
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || !strings.HasPrefix(fd.Name.Name, "Test") {
				continue
			}
			checked++
			ast.Inspect(fd, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Parallel" {
					return true
				}
				t.Errorf("%s: %s calls t.Parallel(), but this file assigns the "+
					"PROCESS-GLOBAL time.Local.\n"+
					"A parallel test would leak its substituted zone into every test "+
					"running beside it, and have its own yanked away mid-run — an "+
					"intermittent, zone-dependent flake in an unrelated test.\n"+
					"Keep these tests sequential (#723).", name, fd.Name.Name)
				return false
			})
		}
		if checked == 0 {
			t.Errorf("%s: found no Test functions to check — this guard is VACUOUS", name)
		}
	}
}
