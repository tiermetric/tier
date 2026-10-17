package scoring

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// PR 16 must couple correction-rule wiring to the rubric bump and stored flag
// state (R-2026-10-14-20). Until then, even a production reference is forbidden.
func TestCorrectionRuleUnreachableFromScoring(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" || path == filepath.Join(root, "internal", "qualityrule") {
				return filepath.SkipDir
			}
			// A nested module's sources are outside this module's scoring paths.
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				} else if !os.IsNotExist(err) {
					return err
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		imports := make(map[string]bool)
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			switch importPath {
			case "github.com/tiermetric/tier/internal/qualityrule", "github.com/tiermetric/tier/internal/quality":
				name := filepath.Base(importPath)
				if spec.Name != nil {
					name = spec.Name.Name
				}
				imports[name] = true
			}
		}
		forbidden := func(name string) bool {
			return name == "ResolveWithCorrection" || name == "FloorCleanCorrection" || name == "FloorCIFailCorrection"
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := node.X.(*ast.Ident); ok && pkg.Obj == nil && imports[pkg.Name] && forbidden(node.Sel.Name) {
					t.Errorf("%s: production reference to %s.%s; correction wiring requires PR 16's rubric bump", fset.Position(node.Pos()), pkg.Name, node.Sel.Name)
				}
			case *ast.Ident:
				if imports["."] && node.Obj == nil && forbidden(node.Name) {
					t.Errorf("%s: production reference to dot-imported %s; correction wiring requires PR 16's rubric bump", fset.Position(node.Pos()), node.Name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
