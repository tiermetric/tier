package scoring

// The COVERAGE ARM behind #722.
//
// #722 was three instances of one shape: a value accumulated inside a `range` over
// a MAP, where the accumulation order reaches a float sum. Go randomizes map
// iteration per range, float addition is not associative, so the published figure
// wobbles between runs over identical data — which is a direct counterexample to
// #710's promise that a re-run reproduces bit-identically.
//
// Three instances is the definition of a shape that needs a structural guard rather
// than a fourth fix. This test enumerates every such site in internal/scoring and
// internal/api and pins the set: a NEW one fails here until a human writes down why
// it is safe.
//
// 🔴 WHAT THIS GUARD DOES NOT DO — read this before treating a green as coverage.
//
//  1. It is SYNTACTIC. It has no type information, so "is this accumulation a
//     FLOAT?" is not decidable here. It flags integer accumulations too (which are
//     exact and order-independent) and relies on the allowlist verdict to say so.
//     An over-flagging tripwire that routes a human to the site is the trade; a
//     checker that tried to decide floatness would be the kind of clever check that
//     passes for the wrong reason.
//
//  2. The FLAGSHIP #722 SITE HAS NO ACCUMULATION IN THE LOOP AT ALL. AggregateTeamsKAnon
//     only APPENDS to otherDevs inside the map range; the float sum happens later, in
//     RollupTeam, over the slice that loop built. So a checker looking for `+=` inside
//     a map range — the literal wording of the #722 issue — would have missed the very
//     defect it was written for. That is why an append to a slice that outlives the
//     loop counts as an order-carrying site here, and why the semantic guards in
//     determinism_test.go and internal/api/spend_determinism_test.go are the primary
//     protection. This file is a tripwire, not a proof.
//
//  3. Its map inference is a best-effort subset (declared map types, make/composite
//     literals, function results declared to be maps, and index expressions peeled
//     through those). TWO blind-spot classes are known and MEASURED — listed as two
//     because knowing only one invites "so the rest is covered":
//
//     a. A map reached through a STRUCT FIELD. `for level, n := range s.FidelityCounts`
//     in api/fidelity.go is a real map range this detector does not see.
//
//     b. A NAMED map type, which is an *ast.Ident and not an *ast.MapType.
//     `for k, v := range q` over url.Values in api/handler.go (~:5038) is invisible
//     for this reason.
//
//     Both happen to be safe (int64 counts; and the url.Values site sorts its output
//     at ~:5063 with a comment naming this very hazard) — but they are safe by
//     inspection, NOT by anything this detector established.
//     TestMapAccumDetectorSeesKnownSites is the positive control that keeps the
//     inference honest for the routes #722 actually travelled; it does not make the
//     inference complete.
//
//  4. ⚠️ IT LIVES IN internal/scoring AND PARSES internal/api. That is deliberate —
//     #722 spanned both packages and one allowlist beats two that drift — but it has
//     a surprising consequence: an internal/api refactor reddens the internal/scoring
//     suite. If you are chasing an unrelated api failure and this fires, it is not
//     noise and deleting it is not the fix; add the new site to the allowlist with a
//     verdict.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// mapAccumAllowlist is every order-carrying map range currently in the two packages,
// keyed "<pkg>/<file>:<func>:<range expr>", with the verdict a human reached.
//
// ⚠️ Adding a key here is a REVIEW, not a formality. The question to answer is:
// does anything downstream of this loop add floats in the order this loop produced?
// If yes, sort before the loop (or sort what it built) — do not allowlist it.
var mapAccumAllowlist = map[string]string{
	"scoring/engine.go:AggregateLabeledKAnon:groups": "FIXED #722. `named` is sorted by team; `otherRows` is sorted by " +
		"(developer, label) (sortLabeledScores) before RollupTeam float-sums it.",
	"scoring/engine.go:CompareLabeledKAnon:teamSet": "FIXED #722. `named` is sorted by team; otherARows/otherBRows are each " +
		"sorted by (developer, label) before RollupTeam float-sums them.",

	"api/handler.go:loadWindow:actualSpend": "FIXED #722. The loop only COLLECTS raw keys; they are sort.Strings'd and the " +
		"float sum happens in the sorted loop below it.",
	"api/handler.go:handleGetDeveloperScore:spendAll": "FIXED #722. Same shape and same fix as loadWindow's canonSpend rebuild.",

	"api/handler.go:loadWindow:union": "SAFE: collects developer names only; sort.Strings'd immediately, and every " +
		"downstream sum iterates the sorted slice.",
	"api/handler.go:buildScoresResponse:orgBucketMicro": "SAFE: builds the unattributed-bucket rows, then sort.Slice by cost desc with " +
		"the (unique) bucket label as tie-break. Shares are per-entry divisions, not an accumulation.",
	"api/handler.go:buildWorkTypeSegments:segByDev": "SAFE: collects work-type labels only; sort.Strings'd before any segment is built.",
	"api/handler.go:buildWorkTypeSegments:devMap": "SAFE: collects developer names only; sort.Strings'd before the per-developer " +
		"cost sums (which are int64 micro-dollars anyway).",
	"api/handler.go:buildWorkTypeSegments:segIssues[wt][dev]": "SAFE: accumulates int64 micro-dollars. Integer addition is associative and exact, " +
		"so iteration order cannot change the total.",
	"api/handler.go:buildTeamRollups:byTeam": "SAFE: collects team labels only; sort.Strings'd before any row is built. " +
		"Each group's developers were appended by ranging the sorted devScores SLICE, so RollupTeam float-sums them in developer order.",
	"api/membership.go:newMembershipTimeline:seen": "SAFE: collects boundary instants only; sorted and de-duplicated " +
		"before use. No float is summed.",
	"api/membership.go:groupWindow:keySet": "SAFE: collects (developer, label) keys only; sorted before any row is " +
		"computed, so each row's inputs and the row order are fixed (#886).",
	"api/handler.go:buildSegmentReconciliation:byDev": "SAFE: collects developer names only; sort.Strings'd before the reconciliation rows.",
	"api/handler.go:loadWindow:tokenTotals": "SAFE: accumulates int64 token counts into a map keyed by canonical DevIssue. " +
		"Integer addition; and each target key's addends are independent of order.",
	"api/handler.go:handleGetDeveloperScore:tokenTotals": "SAFE: same int64 re-keying as loadWindow's.",
	"api/handler.go:handleEraseDeveloper:counts":         "SAFE: sums int64 per-table delete counts. Integer addition.",

	"api/fidelity.go:handleGetFidelity:merged": "SAFE: collects rows only; sort.Slice by developer before the response is written.",
	"api/compare.go:compareDevelopers:names":   "SAFE: collects developer names only; sort.Strings'd before any row is built.",
}

// scannedPackages are the two packages #722 covers, relative to the repo root.
var scannedPackages = []string{"internal/scoring", "internal/api"}

// knownSites are the four sites #722 actually fixed. The detector MUST still see all
// four. This is the positive control: without it, an inference regression that
// silently found nothing would leave every assertion below trivially satisfied — the
// "green because it looked at nothing" failure.
var knownSites = []string{
	"scoring/engine.go:AggregateLabeledKAnon:groups",
	"scoring/engine.go:CompareLabeledKAnon:teamSet",
	"api/handler.go:loadWindow:actualSpend",
	"api/handler.go:handleGetDeveloperScore:spendAll",
}

func TestMapAccumDetectorSeesKnownSites(t *testing.T) {
	found := scanMapAccumSites(t)
	for _, want := range knownSites {
		if _, ok := found[want]; !ok {
			t.Errorf("detector no longer sees %q.\n"+
				"Its map inference has regressed, and every assertion in this file is now "+
				"weaker than it reads. Fix the inference before touching the allowlist.", want)
		}
	}
	if len(found) < len(knownSites) {
		t.Fatalf("detector found %d sites across %v, want at least %d", len(found), scannedPackages, len(knownSites))
	}
	t.Logf("detector sees %d order-carrying map ranges across %v", len(found), scannedPackages)
}

// verdictPrefixes are the only three conclusions an allowlist entry may reach. The
// prefix is asserted so an entry cannot be added with a shrug: every key must say
// whether the site was FIXED, is SAFE, or is a known FALSE POSITIVE of the
// detector's own over-approximation. An empty or hand-waved verdict fails here.
var verdictPrefixes = []string{"FIXED", "SAFE", "FALSE POSITIVE"}

func TestMapAccumAllowlistVerdictsAreStated(t *testing.T) {
	if len(mapAccumAllowlist) == 0 {
		t.Fatal("mapAccumAllowlist is empty — this test would pass vacuously")
	}
	for key, verdict := range mapAccumAllowlist {
		ok := false
		for _, p := range verdictPrefixes {
			if strings.HasPrefix(verdict, p) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("allowlist verdict for %q must start with one of %v, got %q.\n"+
				"State the conclusion, not a description — the prefix is what makes a "+
				"skimmed allowlist auditable.", key, verdictPrefixes, verdict)
		}
	}
}

func TestNoUnreviewedMapAccumulation(t *testing.T) {
	found := scanMapAccumSites(t)

	var added []string
	for key, detail := range found {
		if _, ok := mapAccumAllowlist[key]; !ok {
			added = append(added, fmt.Sprintf("%s\n        %s", key, detail))
		}
	}
	sort.Strings(added)
	if len(added) > 0 {
		t.Errorf("NEW order-carrying map range(s) in %v — each one accumulates or appends "+
			"inside a `range` over a map, and Go randomizes that order per range:\n\n    %s\n\n"+
			"For each: does anything downstream add FLOATS in the order this loop produced?\n"+
			"  yes -> sort first (see sortLabeledScores / the sort.Strings hops in handler.go)\n"+
			"  no  -> add it to mapAccumAllowlist with the reason it cannot matter.",
			scannedPackages, strings.Join(added, "\n    "))
	}

	var stale []string
	for key := range mapAccumAllowlist {
		if _, ok := found[key]; !ok {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("stale mapAccumAllowlist entries (the site is gone — delete them, or the "+
			"allowlist will silently pre-approve a future site that lands on the same key):\n    %s",
			strings.Join(stale, "\n    "))
	}
}

// --- the detector ---

// mapAccumRepoRoot resolves the repository root from this package's directory and
// proves it by requiring go.mod, so a moved test fails loudly instead of scanning
// an empty tree and passing.
func mapAccumRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}

// funcResultTypes maps an (unqualified) function or method name to its declared
// result types. Name-based, so a same-named function in another package can make a
// LOCAL variable look map-typed when it is not. That over-approximation is the safe
// direction for a tripwire — a false positive is allowlisted with the reason; a
// false negative is a defect that ships.
type funcResultTypes map[string][]ast.Expr

// typeEnv is the per-function mini type environment: variable name -> declared or
// inferred type expression, plus the repo-wide function result table.
type typeEnv struct {
	vars    map[string]ast.Expr
	results funcResultTypes
}

func isMapType(e ast.Expr) bool {
	_, ok := unparen(e).(*ast.MapType)
	return ok
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// typeOf resolves the type expression of e within the environment, or nil when the
// detector cannot model it. It peels index expressions through map/array value types
// so a nested `m[a][b]` is resolved rather than abandoned.
func (env typeEnv) typeOf(e ast.Expr) ast.Expr {
	switch t := unparen(e).(type) {
	case *ast.Ident:
		return env.vars[t.Name]
	case *ast.CompositeLit:
		return t.Type
	case *ast.IndexExpr:
		switch c := unparen(env.typeOf(t.X)).(type) {
		case *ast.MapType:
			return c.Value
		case *ast.ArrayType:
			return c.Elt
		}
	case *ast.CallExpr:
		if id, ok := unparen(t.Fun).(*ast.Ident); ok {
			if id.Name == "make" && len(t.Args) > 0 {
				return t.Args[0]
			}
			if rs := env.results[id.Name]; len(rs) == 1 {
				return rs[0]
			}
		}
		if sel, ok := unparen(t.Fun).(*ast.SelectorExpr); ok {
			if rs := env.results[sel.Sel.Name]; len(rs) == 1 {
				return rs[0]
			}
		}
	}
	return nil
}

// bind records the type of every LHS name in an assignment, resolving multi-value
// calls positionally so `spendAll, err := h.store.ActualSpendAllWindow(...)` binds
// spendAll to map[string]float64.
func (env typeEnv) bind(lhs []ast.Expr, rhs []ast.Expr) {
	if len(rhs) == 1 && len(lhs) > 1 {
		call, ok := unparen(rhs[0]).(*ast.CallExpr)
		if !ok {
			return
		}
		var rs []ast.Expr
		switch f := unparen(call.Fun).(type) {
		case *ast.Ident:
			rs = env.results[f.Name]
		case *ast.SelectorExpr:
			rs = env.results[f.Sel.Name]
		}
		for i, l := range lhs {
			if id, ok := unparen(l).(*ast.Ident); ok && i < len(rs) {
				env.vars[id.Name] = rs[i]
			}
		}
		return
	}
	for i, l := range lhs {
		if i >= len(rhs) {
			return
		}
		if id, ok := unparen(l).(*ast.Ident); ok {
			if ty := env.typeOf(rhs[i]); ty != nil {
				env.vars[id.Name] = ty
			}
		}
	}
}

func fieldTypes(fl *ast.FieldList) []ast.Expr {
	var out []ast.Expr
	if fl == nil {
		return out
	}
	for _, f := range fl.List {
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			out = append(out, f.Type)
		}
	}
	return out
}

// exprString renders a range expression compactly for the site key. Only the shapes
// the detector can resolve need a faithful rendering.
func exprString(e ast.Expr) string {
	switch t := unparen(e).(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	case *ast.IndexExpr:
		return exprString(t.X) + "[" + exprString(t.Index) + "]"
	case *ast.CallExpr:
		return exprString(t.Fun) + "()"
	}
	return "<expr>"
}

// goFilesIn lists a directory's non-test .go files. mustHaveFiles is set for the
// two packages UNDER GUARD, where an empty listing would make the whole scan pass
// vacuously; it is left false for the repo-wide result-type sweep, where a
// test-only package (internal/integration) is legitimately empty.
func goFilesIn(t *testing.T, dir string, mustHaveFiles bool) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	if mustHaveFiles && len(out) == 0 {
		t.Fatalf("no non-test .go files in %s — the scan would pass vacuously", dir)
	}
	return out
}

// scanMapAccumSites returns every order-carrying map range in scannedPackages,
// keyed "<pkg>/<file>:<func>:<range expr>" with a human-readable detail string.
func scanMapAccumSites(t *testing.T) map[string]string {
	t.Helper()
	root := mapAccumRepoRoot(t)
	fset := token.NewFileSet()

	// Result-type table over every internal package, so a map arriving from
	// internal/store (ActualSpendAllWindow, OutcomeTokenTotals, ...) is recognised.
	results := funcResultTypes{}
	// RECURSIVE on purpose: internal/ has nested packages (internal/collector/*,
	// internal/docs/html). A top-level-only sweep would silently miss a map-returning
	// function there, and the miss would look exactly like "this site is not a map".
	internalDir := filepath.Join(root, "internal")
	var pkgDirs []string
	err := filepath.WalkDir(internalDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() != "testdata" {
			pkgDirs = append(pkgDirs, path)
		}
		if d.IsDir() && d.Name() == "testdata" {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", internalDir, err)
	}
	for _, pd := range pkgDirs {
		for _, path := range goFilesIn(t, pd, false) {
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok {
					results[fd.Name.Name] = fieldTypes(fd.Type.Results)
				}
			}
		}
	}

	found := map[string]string{}
	for _, pkg := range scannedPackages {
		pkgName := filepath.Base(pkg)
		for _, path := range goFilesIn(t, filepath.Join(root, pkg), true) {
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				env := typeEnv{vars: map[string]ast.Expr{}, results: results}
				// Params and named results of the enclosing function, plus every
				// binding anywhere in it (position-insensitive: over-approximation
				// is the safe direction).
				for _, fl := range []*ast.FieldList{fd.Type.Params, fd.Type.Results} {
					if fl == nil {
						continue
					}
					for _, fld := range fl.List {
						for _, nm := range fld.Names {
							env.vars[nm.Name] = fld.Type
						}
					}
				}
				ast.Inspect(fd, func(n ast.Node) bool {
					switch s := n.(type) {
					case *ast.FuncLit:
						for _, fld := range fieldListOf(s.Type.Params) {
							for _, nm := range fld.Names {
								env.vars[nm.Name] = fld.Type
							}
						}
					case *ast.ValueSpec:
						for i, nm := range s.Names {
							if s.Type != nil {
								env.vars[nm.Name] = s.Type
								continue
							}
							if i < len(s.Values) {
								if ty := env.typeOf(s.Values[i]); ty != nil {
									env.vars[nm.Name] = ty
								}
							}
						}
					case *ast.AssignStmt:
						env.bind(s.Lhs, s.Rhs)
					case *ast.RangeStmt:
						// `for _, v := range m` where m is map[K]V binds v to V, so a
						// nested map reached through a range is still modelled.
						if mt, ok := unparen(env.typeOf(s.X)).(*ast.MapType); ok && s.Value != nil {
							if id, ok := s.Value.(*ast.Ident); ok {
								env.vars[id.Name] = mt.Value
							}
						}
					}
					return true
				})

				ast.Inspect(fd, func(n ast.Node) bool {
					rs, ok := n.(*ast.RangeStmt)
					if !ok || !isMapType(env.typeOf(rs.X)) {
						return true
					}
					var notes []string
					ast.Inspect(rs.Body, func(b ast.Node) bool {
						as, ok := b.(*ast.AssignStmt)
						if !ok {
							return true
						}
						switch as.Tok {
						case token.ADD_ASSIGN, token.SUB_ASSIGN, token.MUL_ASSIGN, token.QUO_ASSIGN:
							notes = append(notes, fmt.Sprintf("%s %s (line %d)",
								exprString(as.Lhs[0]), as.Tok, fset.Position(as.Pos()).Line))
						case token.ASSIGN, token.DEFINE:
							for _, r := range as.Rhs {
								c, ok := unparen(r).(*ast.CallExpr)
								if !ok {
									continue
								}
								if id, ok := unparen(c.Fun).(*ast.Ident); ok && id.Name == "append" {
									notes = append(notes, fmt.Sprintf("append -> %s (line %d)",
										exprString(as.Lhs[0]), fset.Position(as.Pos()).Line))
								}
							}
						}
						return true
					})
					if len(notes) == 0 {
						return true
					}
					key := fmt.Sprintf("%s/%s:%s:%s", pkgName, filepath.Base(path), fd.Name.Name, exprString(rs.X))
					found[key] = strings.Join(notes, "; ")
					return true
				})
			}
		}
	}
	return found
}

func fieldListOf(fl *ast.FieldList) []*ast.Field {
	if fl == nil {
		return nil
	}
	return fl.List
}
