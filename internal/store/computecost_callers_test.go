package store

// The guard behind #525.
//
// ComputeCost resolves a billing_mode and throws it away. That is harmless for a
// caller that only wants a number, and silently WRONG for a caller that persists
// a TokenEvent: the row takes normalizeBillingMode's per_token default and claims
// a billing basis it never earned. A model absent from the price table is priced
// at the guessed self-hosted-medium rate while reporting per_token, and the column
// is published by BOTH /export surfaces.
//
// It has now been the same bug twice — /events (#492) and then the JSONL collector
// plus both org pollers (#525) — which is the definition of a defect that needs a
// structural guard rather than a third fix. A doc comment saying "don't do this"
// did not stop it; the comment was already there when #525's three callers were
// written.
//
// So the caller set is made structural: ComputeCost has exactly ONE non-test
// reference in this repository, and this test names it and pins how many times it
// appears. Any other reference fails here, whether or not it persists anything.
// "Does this call persist a TokenEvent?" is not decidable from the AST, and a
// guard that tried to decide it would be the kind of clever check that passes for
// the wrong reason. "Is there a new reference at all?" IS decidable, and it routes
// every new one through a human who has to answer the persistence question.
//
// ⚠️ SCOPE, stated so nobody over-reads a green: this guard catches CALLERS OF
// ComputeCost. It does NOT catch a persisting producer that simply never sets
// BillingMode — internal/api/handler.go's /costs manual import and cmd/tierd's
// demo seeder both insert TokenEvents with no mode and are invisible to it. A pass
// here means "nobody reintroduced the ComputeCost shortcut", NOT "billing_mode is
// honest on every path".

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// computeCostName is the identifier under guard. Matching is EXACT: ComputeCostHost
// shares a prefix and is the correct API, so a prefix match would flag every correct
// caller in the tree and the guard would be switched off within a day.
const computeCostName = "ComputeCost"

// fileScope is the enclosing-name used for a reference that sits outside any
// function body — a package-level var/const initializer. It can never equal an
// allowlist key, which is the point: a file-scope call must not be able to inherit
// the exemption of whatever function happens to be declared above it.
const fileScope = "<file-scope>"

type allowEntry struct {
	// calls is the exact number of references permitted at this key. Pinning the
	// COUNT (not just the key) is what stops a SECOND ComputeCost call being added
	// inside an already-exempt function and riding its exemption in for free.
	calls int
	why   string
}

// computeCostAllowlist is the complete set of permitted non-test references,
// keyed by "<repo-relative file>:<enclosing func>".
//
// recomputeKnownSourceCosts is the one-shot #55 repricer at the top of Open().
// It is marker-guarded, runs before any insert, and only ever touches pre-#300
// rows whose host is the backfilled sentinel — for which the model-only rate is
// exactly correct. Critically it UPDATEs cost_micro on rows that already exist
// rather than persisting a new TokenEvent, so there is no mode for it to
// discard. That is why it is exempt, and the reasoning is the exemption: a
// caller that does not match it does not inherit it.
var computeCostAllowlist = map[string]allowEntry{
	"internal/store/store.go:recomputeKnownSourceCosts": {
		calls: 1,
		why: "one-shot #55 repricer: marker-guarded, pre-#300 rows only, UPDATEs cost on " +
			"existing rows and persists no TokenEvent",
	},
}

// callSite is one discovered reference to ComputeCost.
type callSite struct {
	key  string // "<repo-relative file>:<enclosing func>"
	line int
}

// scanResult is what a walk produced: the references found, and the set of files
// actually parsed so a caller can prove the walk was not vacuous.
type scanResult struct {
	sites   []callSite
	scanned map[string]bool
}

// referencesIn records every reference to ComputeCost under n, attributed to
// enclosing.
//
// It matches REFERENCES, not only direct calls. A func value (`f := ComputeCost`),
// a package-level alias (`var cc = ComputeCost`) and a parenthesized callee
// (`(ComputeCost)(…)`) are all callers the moment they are invoked, and the
// invocation site is not syntactically recoverable from the call expression. An
// injected `costFn` seam is an ordinary Go refactor, so a call-only matcher would
// be routine to slip past.
//
// Known and accepted false positive: a method named ComputeCost on an unrelated
// type also trips this. That is the right trade for a package-blind AST guard —
// fail loud, then allowlist with reasoning — but it is the first thing a new
// contributor will hit, so it is written down here rather than discovered.
func referencesIn(fset *token.FileSet, n ast.Node, rel, enclosing string, out *[]callSite) {
	record := func(pos token.Pos) {
		*out = append(*out, callSite{
			key:  rel + ":" + enclosing,
			line: fset.Position(pos).Line,
		})
	}
	ast.Inspect(n, func(nd ast.Node) bool {
		switch e := nd.(type) {
		case *ast.SelectorExpr:
			if e.Sel.Name == computeCostName {
				record(e.Pos())
				// Do not descend: e.Sel is an Ident with the same name and would
				// be counted a second time.
				return false
			}
		case *ast.Ident:
			if e.Name == computeCostName {
				record(e.Pos())
			}
		}
		return true
	})
}

// scanFile finds every non-test reference to ComputeCost in one parsed file.
//
// It iterates file.Decls EXPLICITLY rather than running a single flat ast.Inspect
// over the file. A flat walk has to carry the enclosing function in a variable
// that is only ever assigned on entering a FuncDecl and never reset, so any
// reference outside a function body — a package-level var initializer — silently
// inherits the name of the last function declared above it. In this very file's
// allowlist that would mean a `var x = ComputeCost(…)` placed anywhere below
// recomputeKnownSourceCosts in store.go keys to recomputeKnownSourceCosts and is
// pre-approved. store.go is exactly where a future repricer would grow, so that
// is not a hypothetical shape.
func scanFile(fset *token.FileSet, rel string, file *ast.File) []callSite {
	var out []callSite
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			// var/const initializers and everything else at package scope.
			referencesIn(fset, decl, rel, fileScope, &out)
			continue
		}
		name := fd.Name.Name
		if fd.Recv != nil && len(fd.Recv.List) > 0 {
			// Qualify methods so two types' same-named methods cannot collapse
			// onto one allowlist key.
			name = "(" + types.ExprString(fd.Recv.List[0].Type) + ")." + name
		}
		// Body ONLY. ComputeCost's own declaration in prices.go has an Ident named
		// ComputeCost for its name; inspecting the whole FuncDecl would make the
		// definition report itself as a caller.
		if fd.Body != nil {
			referencesIn(fset, fd.Body, rel, name, &out)
		}
	}
	return out
}

// collectComputeCostRefs walks a tree rooted at root and returns every non-test
// reference to ComputeCost, plus the set of files parsed.
//
// THE WHOLE REPOSITORY, not this package and not one file. #525's three callers
// lived in internal/collector and in two subpackages under it — none of them in
// internal/store. A guard scanned against its own package would have reported a
// clean tree while all three defects sat one directory away, which is exactly the
// failure recorded against the #492 guard (an AST guard that parsed one file while
// claiming to protect a package).
//
// Deliberately WITHOUT build-tag filtering: a persisting caller behind any build
// tag stores the same wrong column.
//
// root is a parameter rather than a constant so the control arm can drive this
// same walk over a planted defect. A guard whose walker is only ever exercised
// against a clean tree has no evidence it can see anything at all.
func collectComputeCostRefs(root string) (scanResult, error) {
	res := scanResult{scanned: map[string]bool{}}
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules":
				// Conventional, and safe to match by name anywhere.
				return fs.SkipDir
			}
			// Skip any NESTED MODULE, by reason rather than by name. tools/docgen
			// is its own module and .claude/worktrees/* are gitignored checkouts of
			// THIS repo at older commits — left in, the walk reads a pre-#300 copy
			// of proxy.go and reports violations that no longer exist on main, a
			// guard failing loudly for a defect nobody can fix. Matching on the
			// base name instead would silently skip a future real package that
			// happened to be called "docgen", which is the vacuity this guard
			// exists to prevent.
			if rel != "." {
				if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
					return fs.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// SkipObjectResolution: the guard is purely syntactic and needs no object
		// graph.
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		res.scanned[rel] = true
		res.sites = append(res.sites, scanFile(fset, rel, file)...)
		return nil
	})
	return res, err
}

// repoRoot resolves the repository root from this package's directory and proves
// it is really the root.
func repoRoot(t *testing.T) string {
	t.Helper()
	// This test lives in internal/store, so the repo root is two levels up.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	// os.Stat, NOT filepath.Abs. Abs performs no filesystem access at all — on an
	// already-absolute input it returns (Clean(path), nil) and can only fail when
	// os.Getwd() fails for a RELATIVE input. An Abs-based existence check here was
	// unreachable code wearing the costume of a non-vacuity guarantee: precisely
	// the "control that cannot fail for its stated reason" this file warns about.
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod — the walk is rooted wrong, so a clean "+
			"result would be vacuous: %v", root, err)
	}
	return root
}

// TestComputeCost_NoNewPersistingCallers is the guard named in ComputeCost's doc
// comment. If it fails, switch the offending caller to ComputeCostHost and store
// the returned mode — do not add it to the allowlist unless it genuinely matches
// the recomputeKnownSourceCosts exemption.
func TestComputeCost_NoNewPersistingCallers(t *testing.T) {
	root := repoRoot(t)
	res, err := collectComputeCostRefs(root)
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	// NON-VACUITY. A negative assertion ("no unexpected callers") is only
	// trustworthy once the match set is proven non-empty — the ledger records a
	// guard that printed "none" because its pattern matched nothing at all.
	//
	// This asserts the walk reached the specific files that HOSTED #525's defects
	// rather than counting files. A bare count floor is the weaker check: with 64
	// files in scope, a floor of 50 still passes with internal/api (9 files) or
	// cmd/tierd (9 files) entirely skipped, so a whole package could vanish and
	// the guard would stay green while blind to it. It also fails for the wrong
	// reason after a routine consolidation, and a guard that cries wolf gets
	// deleted.
	mustScan := []string{
		"internal/collector/jsonl.go",
		"internal/collector/openaiusage/poller.go",
		"internal/collector/anthropicadmin/poller.go",
		"internal/store/store.go",
		"internal/store/prices.go",
	}
	for _, f := range mustScan {
		if !res.scanned[f] {
			t.Fatalf("the walk never parsed %s (scanned %d files). It is not covering the "+
				"packages where #525's defects lived, so a clean result here would be vacuous", f, len(res.scanned))
		}
	}
	if len(res.sites) == 0 {
		t.Fatal("found ZERO references to ComputeCost anywhere in the repo. The known " +
			"caller in recomputeKnownSourceCosts exists, so this means the detector " +
			"is broken, not that the tree is clean")
	}

	counts := map[string]int{}
	for _, site := range res.sites {
		counts[site.key]++
		if _, ok := computeCostAllowlist[site.key]; !ok {
			t.Errorf("NEW ComputeCost reference at %s (line %d).\n"+
				"ComputeCost DISCARDS the resolved billing_mode. If this call persists a "+
				"TokenEvent, the row silently takes the per_token default and claims a "+
				"billing basis it has not earned (#492, #525) — use ComputeCostHost and "+
				"store the second return value.\n"+
				"If it genuinely cannot persist anything, add it to computeCostAllowlist "+
				"WITH the reasoning, the way recomputeKnownSourceCosts carries its own.",
				site.key, site.line)
		}
	}

	for key, entry := range computeCostAllowlist {
		got := counts[key]
		switch {
		case got == 0:
			// A stale exemption pre-approves a caller nobody reviewed.
			t.Errorf("stale entry in computeCostAllowlist: %q no longer references "+
				"ComputeCost. Remove it — a stale exemption silently blesses the next "+
				"call written at that location.", key)
		case got != entry.calls:
			t.Errorf("%q has %d ComputeCost references, allowlist permits %d.\n"+
				"The exemption covers specific reviewed calls (%s), not the function as a "+
				"blanket. A new call added inside an exempt function is still a new call — "+
				"switch it to ComputeCostHost, or raise the count only if it genuinely "+
				"matches the same reasoning.", key, got, entry.calls, entry.why)
		}
	}
}

// TestComputeCost_GuardDetectsANewCaller is the CONTROL ARM.
//
// The ledger's most reliable bug in this project is a green that means "never
// ran". A guard is code and fails the same way — so this drives the REAL WALKER
// (collectComputeCostRefs, the same function the guard above uses) over planted
// trees on disk, not just the matcher over an in-memory snippet.
//
// That distinction is the whole point. An earlier version of this control arm
// re-implemented the inspection inline and exercised only the identifier match, so
// it was blind to every way the walk itself can break: wrong root, over-broad skip
// list, an inverted _test.go filter, enclosing-name misattribution. Those are the
// failures that make the guard silently pass on a dirty tree, and they were the
// ones no test touched.
func TestComputeCost_GuardDetectsANewCaller(t *testing.T) {
	// plant writes a one-file module and returns its root.
	plant := func(t *testing.T, src string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module planted\n"), 0o600); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "defect.go"), []byte(src), 0o600); err != nil {
			t.Fatalf("write defect.go: %v", err)
		}
		return dir
	}

	tests := []struct {
		name     string
		src      string
		wantKeys []string
		why      string
	}{
		{
			name: "the #525 defect: resolve a cost, drop the mode, persist it",
			src: `package p

func collect() []int {
	cost := store.ComputeCost(model, usage)
	return []int{cost}
}
`,
			wantKeys: []string{"defect.go:collect"},
			why:      "the exact shape the guard exists to catch",
		},
		{
			name: "the CORRECT form is not flagged",
			src: `package p

func collect() []int {
	cost, mode := store.ComputeCostHost("", model, usage)
	_ = mode
	return []int{cost}
}
`,
			wantKeys: nil,
			why: "ComputeCostHost shares a prefix; a prefix match would condemn every " +
				"correct caller and the guard would be disabled within a day",
		},
		{
			name: "a package-level var must NOT inherit the function above it",
			src: `package p

func recomputeKnownSourceCosts() {}

var seedCost = store.ComputeCost(model, usage)
`,
			wantKeys: []string{"defect.go:" + fileScope},
			why: "under a flat walk this keys to recomputeKnownSourceCosts and is silently " +
				"blessed by the allowlist — a guard that passes with the defect present",
		},
		{
			name: "a func VALUE is a reference, not a call",
			src: `package p

func wire() {
	f := store.ComputeCost
	_ = f
}
`,
			wantKeys: []string{"defect.go:wire"},
			why:      "an injected costFn seam is an ordinary refactor and must not slip past",
		},
		{
			name: "methods are qualified by receiver, not collapsed",
			src: `package p

type A struct{}
type B struct{}

func (a A) run() { _ = store.ComputeCost(m, u) }
func (b B) run() { _ = store.ComputeCost(m, u) }
`,
			wantKeys: []string{"defect.go:(A).run", "defect.go:(B).run"},
			why:      "two types' same-named methods must not collapse onto one allowlist key",
		},
		{
			name: "the definition itself is not a caller",
			src: `package p

func ComputeCost(model string, u int) int {
	cost, _ := ComputeCostHost("", model, u)
	return cost
}
`,
			wantKeys: nil,
			why: "prices.go declares ComputeCost; if the declaration reported itself the " +
				"guard would be permanently red on correct code",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := collectComputeCostRefs(plant(t, tc.src))
			if err != nil {
				t.Fatalf("walk planted tree: %v", err)
			}
			// Prove the walk actually parsed the planted file. Without this, every
			// wantKeys==nil case would pass just as well against a walk that read
			// nothing at all.
			if !res.scanned["defect.go"] {
				t.Fatalf("the walk never parsed defect.go — this case proves nothing")
			}

			var got []string
			for _, s := range res.sites {
				got = append(got, s.key)
			}
			if !sameKeys(got, tc.wantKeys) {
				t.Errorf("keys = %v, want %v\nwhy this case exists: %s", got, tc.wantKeys, tc.why)
			}
		})
	}
}

// TestComputeCost_GuardSkipsNestedModules pins the skip rule, which is otherwise
// only exercised incidentally by the repo walk.
//
// A nested module is skipped by REASON (it has its own go.mod) rather than by
// directory name, so tools/docgen and the gitignored .claude/worktrees/* checkouts
// are excluded without also blinding the guard to a future real package that
// happens to share one of those names.
func TestComputeCost_GuardSkipsNestedModules(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module outer\n"), 0o600); err != nil {
		t.Fatalf("write outer go.mod: %v", err)
	}
	// A real package in the outer module — MUST be seen.
	if err := os.WriteFile(filepath.Join(root, "real.go"),
		[]byte("package p\n\nfunc real1() { _ = store.ComputeCost(m, u) }\n"), 0o600); err != nil {
		t.Fatalf("write real.go: %v", err)
	}
	// A nested module — must NOT be seen.
	nested := filepath.Join(root, "docgen")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "go.mod"), []byte("module nested\n"), 0o600); err != nil {
		t.Fatalf("write nested go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "vendored.go"),
		[]byte("package q\n\nfunc nested1() { _ = store.ComputeCost(m, u) }\n"), 0o600); err != nil {
		t.Fatalf("write nested source: %v", err)
	}

	res, err := collectComputeCostRefs(root)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// Positive arm FIRST: if the outer file were also missed, the negative arm
	// below would pass vacuously.
	if !res.scanned["real.go"] {
		t.Fatal("the walk missed real.go in the outer module — the negative arm below " +
			"would then pass for the wrong reason")
	}
	if res.scanned["docgen/vendored.go"] {
		t.Error("the walk parsed a file inside a NESTED MODULE; a stale worktree copy " +
			"would report violations that do not exist on main")
	}
	var got []string
	for _, s := range res.sites {
		got = append(got, s.key)
	}
	if !sameKeys(got, []string{"real.go:real1"}) {
		t.Errorf("keys = %v, want exactly [real.go:real1]", got)
	}
}

// sameKeys compares two key sets order-insensitively.
func sameKeys(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		seen[w]--
		if seen[w] < 0 {
			return false
		}
	}
	return true
}
