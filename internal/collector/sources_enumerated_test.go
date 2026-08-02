package collector

// The guard behind #492's shipping decision.
//
// /api/v1/events used to test `req.Source != "jsonl"` against a bare literal.
// That silently closed the central path to Codex — Codex outcomes arrived while
// its cost was rejected, so Codex work read as FREE and inflated TIER for
// everyone using it. The fix replaced the literal with ShippableSource, but a
// predicate over a switch has the same failure mode one step later: add a
// seventh collector, forget the switch, and its spend is rejected just as
// quietly.
//
// So the source list is made structural. allSources must name every Source*
// constant declared ANYWHERE in this package, and ShippableSource must answer
// for every entry in it. A new collector therefore cannot compile-and-pass
// without someone writing down whether it can ship — including one that follows
// the local convention of declaring itself in its own file.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// declaredSourceConstants returns the value of every `Source*` constant declared
// anywhere in this package, read from the AST rather than from allSources itself
// — the whole point is to compare the declarations against the list, so reading
// the list twice would prove nothing.
//
// THE WHOLE PACKAGE, not collector.go. Every other collector in this tree got
// its own file (jsonl.go, watcher.go, and the codexrollout/anthropicadmin/
// openaiusage subpackages); SourceCodexRollout landed in collector.go only
// because that const block happens to live there, and nothing makes the next
// contributor put theirs alongside it. A scan pinned to one file would miss a
// new source declared the most natural way there is, which is precisely the
// omission this guard claims to prevent.
//
// It also returns the number of files scanned, so callers can prove the scan
// was not pinned to one file after all.
func declaredSourceConstants(t *testing.T) (map[string]string, int) {
	t.Helper()
	// The directory is walked here rather than with parser.ParseDir (deprecated
	// since Go 1.25) — and deliberately WITHOUT build-tag filtering, since a
	// source constant behind any build tag still needs a ship decision.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	out := make(map[string]string)
	files := 0
	for _, entry := range entries {
		path := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files++
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			// VAR as well as CONST. `var SourceGeminiCLI = "gemini-cli"` is one
			// token away from the const form, works identically as a source, and
			// would otherwise be swept up by neither this scan nor the compiler.
			if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, ident := range vs.Names {
					if !strings.HasPrefix(ident.Name, "Source") {
						continue
					}
					// A `Source*` name this scan cannot READ must be loud, never
					// skipped. A silent `continue` here would drop the constant
					// from the comparison set, and the "nothing is missing"
					// loops below would then pass over a set the scan quietly
					// declined to populate — the project's signature false
					// green. Concatenations (`prefix + "-cli"`), aliases
					// (`= SourceJSONL`) and iota forms all land here.
					if i >= len(vs.Values) {
						t.Fatalf("%s: %s has no value expression this scan can read (iota or grouped const?). Give it a plain string literal, or teach declaredSourceConstants to read this form — do NOT leave it unscanned", path, ident.Name)
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s: %s = %T is not a plain string literal, so this scan cannot verify it is enumerated. Give it a plain string literal, or teach declaredSourceConstants to read this form — do NOT leave it unscanned", path, ident.Name, vs.Values[i])
					}
					val, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquote %s = %s: %v", ident.Name, lit.Value, err)
					}
					out[ident.Name] = val
				}
			}
		}
	}
	return out, files
}

// TestAllSourcesEnumerated: every Source* constant declared anywhere in this
// package appears in AllSources(), and nothing else does.
//
// The scan is NOT recursive, so the convention that every source constant lives
// in this package (not a collector subpackage) is load-bearing. A subpackage
// declaration can still only reach allSources as a literal, which the reverse
// direction below catches.
func TestAllSourcesEnumerated(t *testing.T) {
	declared, filesScanned := declaredSourceConstants(t)
	all := AllSources()

	// CONTROL ARM. Every assertion below is of the form "nothing is missing",
	// which passes vacuously when the match set is empty — the exact
	// false-green that has bitten this project repeatedly. If the AST scan
	// found no constants (renamed file, changed declaration shape, a parser
	// that silently returned nothing), say THAT rather than reporting a clean
	// run over zero facts.
	if len(declared) < 2 {
		t.Fatalf("AST scan found %d Source* constants in this package — the SCAN is broken, not the code; every check below would pass vacuously. Found: %v", len(declared), declared)
	}
	// ...and prove it really swept the package rather than one file. A
	// single-file scan passes every assertion below while missing a source
	// declared in a sibling file, which is the omission this guard exists for.
	if filesScanned < 2 {
		t.Fatalf("AST scan visited %d file(s) — it is not sweeping the package, so a Source* constant in a sibling file would go unchecked", filesScanned)
	}
	if got := declared["SourceJSONL"]; got != SourceJSONL {
		t.Fatalf("AST scan read SourceJSONL as %q, want %q — the scan is not reading the real declarations", got, SourceJSONL)
	}

	for name, value := range declared {
		if !slices.Contains(all, value) {
			t.Errorf("this package declares %s = %q but allSources omits it. Add it, and answer for it in ShippableSource: can a laptop ship this source to a central tierd? (#492)", name, value)
		}
	}

	// The reverse direction: AllSources must not name a source that no longer
	// exists, or ShippableSources() would advertise a dead value in the error
	// message a rejected client reads.
	declaredValues := make([]string, 0, len(declared))
	for _, value := range declared {
		declaredValues = append(declaredValues, value)
	}
	for _, value := range all {
		if !slices.Contains(declaredValues, value) {
			t.Errorf("allSources contains %q, which is not declared as a Source* constant anywhere in this package", value)
		}
	}
}

// TestAllSourcesEnumerated_CatchesAnOmission is the positive control for the
// test above: it runs the SAME containment check against a deliberately
// truncated list and requires it to fail. Without this, a check that could
// never fail would report success forever.
func TestAllSourcesEnumerated_CatchesAnOmission(t *testing.T) {
	declared, _ := declaredSourceConstants(t)
	truncated := []string{SourceJSONL} // a plausible "forgot to add the new one"

	var missing []string
	for _, value := range declared {
		if !slices.Contains(truncated, value) {
			missing = append(missing, value)
		}
	}
	if len(missing) == 0 {
		t.Fatal("the omission check found nothing missing from a one-entry AllSources — the check cannot fail, so TestAllSourcesEnumerated proves nothing")
	}
	if slices.Contains(missing, SourceJSONL) {
		t.Errorf("the omission check flagged %q, which IS present in the truncated list — it is not testing membership", SourceJSONL)
	}
}

// TestShippableSourceAnswersForEveryKnownSource: ShippableSource must return a
// deliberate answer for every enumerated source, and the two families that must
// never ship are asserted by name. A switch defaulting to false means a
// forgotten source fails CLOSED — safe for the store, but invisible, which is
// how #492 stayed hidden. Naming them here makes the omission loud instead.
func TestShippableSourceAnswersForEveryKnownSource(t *testing.T) {
	want := map[string]bool{
		SourceJSONL:          true,  // Claude Code local session files
		SourceCodexRollout:   true,  // Codex local rollout logs (#464) — the #492 fix
		SourceProxy:          false, // captured server-side; a client cannot assert it
		SourceCopilotAPI:     false, // org poller: daily aggregates, not per-call events
		SourceAnthropicAdmin: false, // org poller
		SourceOpenAIUsage:    false, // org poller
	}
	all := AllSources()
	if len(want) != len(all) {
		t.Fatalf("this table covers %d sources but AllSources() has %d — a new source was added without a ship decision (#492). AllSources(): %v", len(want), len(all), all)
	}
	for source, shippable := range want {
		if got := ShippableSource(source); got != shippable {
			t.Errorf("ShippableSource(%q) = %v, want %v", source, got, shippable)
		}
	}

	// ShippableSources() feeds the error message a rejected client reads, so it
	// must list exactly the accepted set, in declaration order.
	gotList := ShippableSources()
	wantList := []string{SourceJSONL, SourceCodexRollout}
	if !slices.Equal(gotList, wantList) {
		t.Errorf("ShippableSources() = %v, want %v", gotList, wantList)
	}

	// An unknown source is not shippable — the default arm, asserted rather
	// than assumed.
	if ShippableSource("some-future-collector") {
		t.Error(`ShippableSource("some-future-collector") = true; an unenumerated source must never ship`)
	}
}
