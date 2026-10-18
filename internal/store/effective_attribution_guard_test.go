package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestIssueReadersUseEffectiveProjection(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	globals := map[string][]ast.Node{}
	type function struct {
		path string
		decl *ast.FuncDecl
	}
	var functions []function
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if v, ok := spec.(*ast.ValueSpec); ok {
						for _, name := range v.Names {
							for _, value := range v.Values {
								globals[name.Name] = append(globals[name.Name], value)
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Body != nil {
					functions = append(functions, function{path, d})
					if d.Recv == nil {
						globals[d.Name.Name] = []ast.Node{d.Body}
					}
				}
			}
		}
	}
	allowed := map[string]bool{
		"effective_attribution.go:queryEffective": true,
		"eventsdigest.go:eventsDigestFrom":        true,
		"eventsdigest_legacy.go:legacyDigestFrom": true,
		"store.go:ListTokenEvents":                true,
		"store.go:ExportDeveloper":                true,
		"prfill.go:fillPROutcome":                 true,
		// Write identity is the stored first-writer identity.
		"store.go:lookupKeyedCostRow": true,
	}
	seen := map[string]bool{}
	// Pin SQL at call sites, including known wrappers that forward a string
	// parameter to QueryContext; their bodies cannot resolve the caller's SQL.
	querySQLArgument := map[string]int{
		"QueryContext":         1,
		"QueryRowContext":      1,
		"Query":                1,
		"QueryRow":             1,
		"queryRows":            2,
		"forEachRow":           1,
		"correctionRows":       2,
		"distinctStrings":      2,
		"queryInt64s":          2,
		"readSpans":            2,
		"queryEffective":       1,
		"queryEffectiveWindow": 4,
	}
	for _, f := range functions {
		locals := map[string][]ast.Node{}
		ast.Inspect(f.decl.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "WriteString" {
					if id, ok := sel.X.(*ast.Ident); ok {
						for _, arg := range c.Args {
							locals[id.Name] = append(locals[id.Name], arg)
						}
					}
				}
			}
			if a, ok := n.(*ast.AssignStmt); ok {
				for _, lhs := range a.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						for _, rhs := range a.Rhs {
							locals[id.Name] = append(locals[id.Name], rhs)
						}
					}
				}
			}
			if v, ok := n.(*ast.ValueSpec); ok {
				for _, name := range v.Names {
					for _, rhs := range v.Values {
						locals[name.Name] = append(locals[name.Name], rhs)
					}
				}
			}
			return true
		})
		ast.Inspect(f.decl.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			case *ast.Ident:
				name = fn.Name
			}
			index, ok := querySQLArgument[name]
			if !ok {
				return true
			}
			if len(call.Args) <= index {
				return true
			}
			// Pin identifier matching to SQLite's case-insensitive table names.
			text := strings.ToLower(sqlSource(call.Args[index], locals, globals, map[string]bool{}))
			if !strings.Contains(text, "token_events") || (!strings.Contains(text, "issue_id") && !selectAllColumns.MatchString(text)) {
				return true
			}
			key := f.path + ":" + f.decl.Name.Name
			seen[key] = true
			if name != "queryEffective" && name != "queryEffectiveWindow" && !allowed[key] {
				t.Errorf("%s uses %s to read token_events.issue_id outside the projection", key, name)
			}
			return true
		})
	}
	for key := range seen {
		t.Log(key)
	}
	for key := range allowed {
		if !seen[key] {
			t.Errorf("allowlisted reader %s was not enumerated", key)
		}
	}
	for _, name := range []string{"DeveloperIssueCostsWindow", "CostCompositionWindow", "UnattributedBucketCostsWindow", "OutcomeTokenTotals"} {
		if !seen["store.go:"+name] {
			t.Errorf("issue reader %s was not enumerated", name)
		}
	}
	if len(seen) < 12 {
		t.Fatalf("enumerated only %d readers", len(seen))
	}
}

var selectAllColumns = regexp.MustCompile(`(?i)\bSELECT\s+(?:\w+\.)?\*`)

func sqlSource(n ast.Node, locals, globals map[string][]ast.Node, active map[string]bool) string {
	var parts []string
	ast.Inspect(n, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.SelectorExpr:
			if v.Sel.Name == "String" {
				parts = append(parts, sqlSource(v.X, locals, globals, active))
			}
			return false
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				if s, err := strconv.Unquote(v.Value); err == nil {
					parts = append(parts, s)
				}
			}
		case *ast.Ident:
			if active[v.Name] {
				return false
			}
			active[v.Name] = true
			nodes, ok := locals[v.Name]
			if !ok {
				nodes = globals[v.Name]
			}
			for _, value := range nodes {
				scope := locals
				if _, ok := value.(*ast.BlockStmt); ok {
					scope = nil
				}
				parts = append(parts, sqlSource(value, scope, globals, active))
			}
			delete(active, v.Name)
		}
		return true
	})
	return strings.Join(parts, " ")
}
