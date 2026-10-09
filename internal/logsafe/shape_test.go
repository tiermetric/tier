package logsafe

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// quoteVerb is CodeQL's own safe-directive pattern (safeFormatArgument in
// LogInjectionCustomizations.qll): a %q-family verb with no '#' flag. Matching
// what the scanner matches is the point — a directive this rejects is one the
// scanner would not credit.
var quoteVerb = regexp.MustCompile(`%[^%#]*q`)

// TestClean_KeepsTheCodeQLRecognizedShape pins the barrier's SHAPE, not just its
// behaviour — because the two are separable here and only the behaviour has a
// test otherwise.
//
// CodeQL's go/log-injection credits a barrier by matching syntax, not semantics:
// ReplaceSanitizer in LogInjectionCustomizations.qll is
//
//	class ReplaceSanitizer extends StringOps::ReplaceAll, Sanitizer {
//	  ReplaceSanitizer() { this.getReplacedString() = ["\r", "\n"] }
//	}
//
// so the credit attaches to a strings.ReplaceAll call whose REPLACED literal is
// exactly "\r" or "\n". Rewriting clean() to strings.Map, strings.NewReplacer, a
// regexp, or a hand-rolled loop would keep every behavioural test in this package
// green while silently deleting the static-analysis barrier the package doc
// claims. That is the exact failure mode this test exists to catch: a claim that
// goes false without any behaviour changing.
//
// It reads the source rather than the runtime because there is nothing at runtime
// to observe — the shape IS the property.
func TestClean_KeepsTheCodeQLRecognizedShape(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "logsafe.go", nil, 0)
	if err != nil {
		t.Fatalf("parse logsafe.go: %v", err)
	}

	clean := findFunc(f, "clean")
	if clean == nil {
		t.Fatal("func clean not found in logsafe.go — if the barrier was renamed, retarget this test rather than deleting it")
	}

	// Collect the replaced-string literal of every strings.ReplaceAll call in clean.
	replaced := map[string]bool{}
	quoteFormats := map[string]bool{}
	ast.Inspect(clean, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch {
		case pkg.Name == "strings" && sel.Sel.Name == "ReplaceAll" && len(call.Args) == 3:
			if s, ok := stringLit(call.Args[1]); ok {
				replaced[s] = true
			}
		case pkg.Name == "fmt" && sel.Sel.Name == "Sprintf" && len(call.Args) >= 1:
			// Record the DIRECTIVES present, not the whole format literal:
			// keying on the literal would report "the barrier is gone" for a
			// harmless reshaping like "[%q]", and would miss a sharp verb
			// embedded in a longer format like "x=%#q".
			if s, ok := stringLit(call.Args[0]); ok {
				if strings.Contains(s, "%#q") {
					quoteFormats["%#q"] = true
				}
				if quoteVerb.MatchString(s) {
					quoteFormats["%q"] = true
				}
			}
		}
		return true
	})

	for _, want := range []string{"\r", "\n"} {
		if !replaced[want] {
			t.Errorf("clean() has no strings.ReplaceAll removing %q — CodeQL's ReplaceSanitizer "+
				"matches the replaced literal, so an equivalent rewrite loses the barrier credit "+
				"even though behaviour is unchanged", want)
		}
	}

	// %q is the secondary barrier and is ALSO shape-sensitive: CodeQL's
	// safeFormatArgument matches the directive regexp `%[^%#]*q`, which
	// deliberately excludes `%#q` because Go's alternate quoting preserves raw
	// newlines. Pin that we never drift to the sharp form.
	if quoteFormats["%#q"] {
		t.Errorf("clean() formats with the SHARP quote verb: Go's alternate quoting preserves raw "+
			"control bytes and CodeQL's safeFormatArgument regexp %s excludes it — use the plain one",
			strconv.Quote("%[^%#]*q"))
	}
	if !quoteFormats["%q"] {
		t.Error("clean() no longer formats with the plain quote verb — the secondary barrier is gone")
	}
}

func findFunc(f *ast.File, name string) *ast.FuncDecl {
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}
