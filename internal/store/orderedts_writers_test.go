package store

// The structural half of #723's coverage arm.
//
// TestOrderedTSWriters_NormalizeToUTC (timestamp_zone_test.go) is a BEHAVIOURAL
// guard: it hands each writer a nonzero-offset timestamp and checks what lands on
// disk. That is the stronger check for the writers it names — but it is a
// hardcoded table, so it is blind to exactly the regression normalizeTS exists to
// prevent: a NEW store method that inserts into an ordered-ts table and forgets
// the normalization. Nothing would fail; the table simply would not mention it.
//
// The bug this whole issue is about was a writer nobody remembered to check. A
// doc comment saying "apply normalizeTS to every ordered-ts writer" is the same
// instrument that already failed once — cmd/tierd/demo.go sat un-normalized while
// every sibling writer normalized and a comment said they all did.
//
// So the writer set is made structural, following the pattern of
// computecost_callers_test.go: "does this INSERT normalize its ts?" is not
// reliably decidable from the AST, but "is there a NEW INSERT into an ordered-ts
// table at all?" IS decidable, and it routes every new one through a human who
// has to answer the normalization question.
//
// ⚠️ SCOPE, stated so nobody over-reads a green. This guard sees INSERT
// statements written as string literals inside internal/store. It does NOT see:
// a ts written by an UPDATE (there are none today); SQL assembled from fragments
// at runtime; or a write from outside this package (impossible today — all SQL in
// the tree lives here, and DB.db is unexported). A pass means "no unreviewed
// INSERT into an ordered-ts table appeared", NOT "every stored timestamp is UTC".
// The behavioural guard is what establishes the latter, for the writers it names.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// orderedTSTables are the tables whose timestamp column is ORDER-SENSITIVE —
// ordered by, keyset-paginated over, or range-filtered. See the ledger on
// normalizeTS in store.go for the full column list and for the two Go-written
// timestamp columns deliberately EXCLUDED (actual_spend, org_actual_spend),
// which are provenance stamps nothing sorts on.
//
// ⚠️ This list is scoped by WHAT IS ORDERED, deliberately NOT by what currently
// takes a Go value. quality_history.ts takes none today — both its INSERTs let
// SQLite's DEFAULT CURRENT_TIMESTAMP fill it, which is always UTC — so it is
// absent from normalizeTS's ledger and there is no live bug. But it IS
// keyset-exported (store.go, keysetWindowSQLFor("ts")), and this guard exists to
// catch a FUTURE writer. Scoping the list to today's Go-written columns would
// mean a session that starts stamping quality_history.ts from Go gets silence on
// a keyset-paginated column — precisely the class of miss that produced #723.
//
// #714 added the last two on exactly the reasoning in the paragraph above. Neither
// takes a Go time value today — price_table_registry.first_seen and
// price_forget_audit.ts are filled by DEFAULT CURRENT_TIMESTAMP, and
// price_table_registry.forgotten_at by the SQL literal CURRENT_TIMESTAMP in an
// UPDATE, all of which are always UTC — so there is no live bug. They are here
// because all three columns are ORDERED BY, and one of them decides an ANSWER
// rather than a display order: readForgottenPriceTableRegistry ends
// `ORDER BY forgotten_at DESC, id DESC LIMIT 1` to read back the row a retirement
// just stamped. A local-zone write there does not merely sort oddly — it returns
// THE WRONG ROW, and the value it returns is the evidence a #714 retirement is
// required to preserve.
var orderedTSTables = []string{
	"outcomes", "token_events", "quality_events", "quality_history",
	"price_table_registry", "price_forget_audit",
	// #849: the push ledger is ORDER BY ts (owner re-derivation), and the audit's
	// outcome_ts is range-filtered by verify-report.
	"push_outcome_commits", "push_outcome_audit",
}

// fileScopeDecl is the enclosing-name recorded for a string literal outside any
// function body — a package-level const/var. insertTokenEventSQL is one, so this
// is a real key, not a placeholder.
const fileScopeDecl = "<file-scope>"

// insertSite identifies one INSERT statement by the file and the enclosing
// declaration it appears in. Deliberately NOT by line number: a line number
// changes on every unrelated edit above it and the guard would be re-pinned so
// often that a real change would pass unnoticed inside the churn.
type insertSite struct {
	file string
	decl string
	tbl  string
}

// knownOrderedTSInserts pins every INSERT into an ordered-ts table, with the
// COUNT at each site. Pinning the count (not just the key) stops a SECOND insert
// being added inside an already-listed function and riding that entry in free.
//
// 🔴 ADDING AN ENTRY HERE IS NOT THE FIX. If this test fails because you added a
// writer, first route its timestamp through normalizeTS and add it to
// TestOrderedTSWriters_NormalizeToUTC — THEN record it here.
var knownOrderedTSInserts = map[insertSite]int{
	{file: "store.go", decl: fileScopeDecl, tbl: "token_events"}:          1, // insertTokenEventSQL — shared by every token-event exec path
	{file: "store.go", decl: "insertOutcomeRow", tbl: "outcomes"}:         1, // InsertOutcome + RecordPROutcome
	{file: "store.go", decl: "upsertPushOutcomeRow", tbl: "outcomes"}:     1, // UpsertPushOutcome + RecordPushCommit
	{file: "audit.go", decl: "AppendQualityEvent", tbl: "quality_events"}: 1,
	// quality_history: ts is filled by DEFAULT CURRENT_TIMESTAMP at both sites —
	// neither INSERT names the column. Pinned so a THIRD one, or a ts added to
	// either of these, has to be looked at.
	{file: "store.go", decl: "UpdateQuality", tbl: "quality_history"}:           1,
	{file: "store.go", decl: "UpdateQualityForOutcome", tbl: "quality_history"}: 1,
	// #714. Same shape as the quality_history pair: neither INSERT names its
	// timestamp column, so both are filled by DEFAULT CURRENT_TIMESTAMP (UTC).
	// Pinned so that naming one from Go — or adding a third writer — has to be
	// looked at. price_forget_audit DOES bind a first_seen value, but it is a
	// STRING copied straight out of the row being retired, never a Go time.Time.
	{file: "pricetableregistry.go", decl: "recordPriceTableIdentity", tbl: "price_table_registry"}: 1,
	{file: "pricetableregistry.go", decl: "ForgetPriceTableVersion", tbl: "price_forget_audit"}:    1,
	// #849: the pre-ledger marker and the commit entry, both ts via normalizeTS;
	// the audit's outcome_ts via normalizeTS (its ts is DEFAULT CURRENT_TIMESTAMP).
	{file: "pushledger.go", decl: "RecordPushCommit", tbl: "push_outcome_commits"}: 2,
	{file: "pushledger.go", decl: "insertPushAudit", tbl: "push_outcome_audit"}:    1,
}

func TestOrderedTSInserts_AreAllKnown(t *testing.T) {
	found := map[insertSite]int{}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		// Attribute each literal to the TOP-LEVEL declaration that encloses it.
		//
		// ⚠️ Walk f.Decls rather than tracking a "last FuncDecl seen" variable
		// while inspecting the whole file. ast.Inspect visits in source order and
		// such a variable is never cleared on leaving a body, so a package-level
		// const declared BELOW a function inherits that function's name. The first
		// draft did exactly that and misattributed insertTokenEventSQL to
		// recomputeKnownSourceCosts — the guard caught its own defect only because
		// the pinned entry then failed to match.
		countLits := func(root ast.Node, decl string) {
			ast.Inspect(root, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				for _, tbl := range orderedTSTables {
					// Count occurrences, not presence: one literal could carry two
					// INSERTs, and a bare presence check would hide the second.
					if c := strings.Count(lit.Value, "INSERT INTO "+tbl); c > 0 {
						found[insertSite{file: name, decl: decl, tbl: tbl}] += c
					}
				}
				return true
			})
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				countLits(fd, fd.Name.Name)
				continue
			}
			countLits(d, fileScopeDecl)
		}
	}

	// Vacuity control. If the scan walked nothing — a renamed package dir, a
	// changed working directory under a future test runner — every comparison
	// below is trivially satisfied and the guard reports a meaningless green.
	if scanned == 0 {
		t.Fatal("scanned 0 non-test .go files in internal/store — this guard is VACUOUS")
	}
	if len(found) == 0 {
		t.Fatalf("found 0 INSERT statements into %v across %d files — the matcher is "+
			"broken (the SQL was reformatted?) and this guard is VACUOUS",
			orderedTSTables, scanned)
	}

	for site, n := range found {
		want, ok := knownOrderedTSInserts[site]
		if !ok {
			t.Errorf("NEW INSERT into ordered-ts table %q at %s:%s (x%d).\n"+
				"Its timestamp must be routed through normalizeTS — `ORDER BY` on that "+
				"column is a binary string sort over an offset-bearing rendering, so an "+
				"un-normalized write silently breaks #711's (ts, id) total order (#723).\n"+
				"Then add it to TestOrderedTSWriters_NormalizeToUTC and to "+
				"knownOrderedTSInserts. Do NOT just add it here.",
				site.tbl, site.file, site.decl, n)
			continue
		}
		if n != want {
			t.Errorf("%s:%s now has %d INSERTs into %q, pinned at %d.\n"+
				"A new insert inside an already-known writer does not inherit its "+
				"normalization — check the new one routes its ts through normalizeTS, "+
				"then update knownOrderedTSInserts.",
				site.file, site.decl, n, site.tbl, want)
		}
	}
	for site, want := range knownOrderedTSInserts {
		if _, ok := found[site]; !ok {
			t.Errorf("pinned INSERT site %s:%s (%q, x%d) no longer exists. If it was "+
				"renamed or removed, drop it from knownOrderedTSInserts — a stale entry "+
				"makes this guard weaker than it reads.",
				site.file, site.decl, site.tbl, want)
		}
	}
}
