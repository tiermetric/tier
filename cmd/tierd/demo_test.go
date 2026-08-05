package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/store"
)

var hex40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

// TestSeedDemo_PopulatesAndIsIdempotent pins that the synthetic seed inserts
// cleanly and that a second run is safe — `tierd demo` recreates the DB each run
// but the idempotency keys (token events) and merge-SHA dedup (outcomes) must
// also make a re-seed into an existing DB a no-op rather than an error.
func TestSeedDemo_PopulatesAndIsIdempotent(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "demo.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	// Count rows over a window wide enough to cover every seeded timestamp so a
	// re-seed that failed to dedup (a new event missing its idempotency key, or
	// outcome dedup regressing) would double the counts and be caught here — not
	// just silently pass because no error was returned.
	countRows := func() (outcomes, events int, paid float64) {
		since := time.Now().AddDate(-1, 0, 0)
		until := time.Now().AddDate(0, 0, 1)
		outs, err := db.AllOutcomesWindow(ctx, since, until, store.FleetWide)
		if err != nil {
			t.Fatalf("read outcomes: %v", err)
		}
		evs, _, err := db.ListTokenEvents(ctx, since, until, store.PageCursor{}, 100000)
		if err != nil {
			t.Fatalf("read events: %v", err)
		}
		// actual_spend has no unique constraint (it's an append-and-sum ledger),
		// so summing per-developer paid catches a re-seed that double-inserts.
		// Read with an OPEN until (as /scores does): this window filters by
		// period (YYYY-MM), and a bounded until in the same calendar month as the
		// seeded row would exclude it via `period < untilPeriod`.
		spend, err := db.ActualSpendAllWindow(ctx, since, time.Time{})
		if err != nil {
			t.Fatalf("read actual spend: %v", err)
		}
		for _, v := range spend {
			paid += v
		}
		return len(outs), len(evs), paid
	}

	if err := seedDemo(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	o1, e1, p1 := countRows()
	if o1 == 0 || e1 == 0 || p1 == 0 {
		t.Fatalf("first seed populated nothing: outcomes=%d events=%d paid=%.2f", o1, e1, p1)
	}
	if err := seedDemo(ctx, db); err != nil {
		t.Fatalf("re-seed must be safe: %v", err)
	}
	o2, e2, p2 := countRows()
	if o1 != o2 || e1 != e2 || p1 != p2 {
		t.Errorf("re-seed changed totals (dedup regressed): outcomes %d->%d, events %d->%d, paid %.2f->%.2f", o1, o2, e1, e2, p1, p2)
	}
}

// TestDemo_SeedsRankedBoard is the end-to-end guard for the feature's whole
// point: seeding then serving must yield a POPULATED, RANKED board. It seeds a
// real DB, mounts the same API composition runServe does, and asserts /scores
// returns every demo developer ranked with zero flagged outcomes and the
// intended spread. This is the test that fails if the cost-vs-merge timestamp
// attribution ever regresses (cost stamped outside the 14-day window → every
// outcome zero-token → unranked → an empty-looking demo that the unit tests,
// which only inspect the static struct, would not catch).
func TestDemo_SeedsRankedBoard(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "demo.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := seedDemo(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	api.New(db, quiet, "", nil, "demo-test", api.RateLimitConfig{}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// NOTE (#590): this request used to carry "?aggregation=developer", which the
	// server has NEVER read — aggregation is a serve-time flag (--aggregation /
	// TIER_AGGREGATION / config key), not a query parameter. The test passed anyway
	// because api.New here already defaults to developer mode, so the no-op parameter
	// looked like it was doing the work. The strict unknown-parameter allowlist added
	// in #590 rejected it, which is exactly what that gate is for.
	//
	// Worth stating plainly, because the removed line was worse than dead weight: it
	// read as though a CLIENT could select the aggregation mode per request. If that
	// were ever true it would be a k-anonymity bypass — any caller could ask a
	// team-mode server for named per-developer rows. It is not true, and the
	// allowlist now makes any future attempt to send it a loud 400 rather than a
	// silent no-op that a reader might mistake for a working control.
	resp, err := http.Get(srv.URL + "/api/v1/scores")
	if err != nil {
		t.Fatalf("GET scores: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scores status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Developers []struct {
			Developer       string  `json:"developer"`
			TIER            float64 `json:"tier"`
			Ranked          bool    `json:"ranked"`
			FlaggedOutcomes int     `json:"flagged_outcomes"`
			SpendLeverage   float64 `json:"spend_leverage"`
		} `json:"developers"`
		// The dashboard's Spend Leverage tile renders the pooled top-level value,
		// not a per-developer one — assert the exact field the UI shows.
		Total struct {
			SpendLeverage float64 `json:"spend_leverage"`
		} `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode scores: %v", err)
	}

	if len(out.Developers) == 0 {
		t.Fatal("demo served an empty board — no developers scored")
	}
	tier := make(map[string]float64, len(out.Developers))
	ranked := 0
	for _, d := range out.Developers {
		tier[d.Developer] = d.TIER
		if d.Ranked {
			ranked++
		}
		if d.FlaggedOutcomes != 0 {
			t.Errorf("%s has %d flagged outcomes — cost was stamped outside its outcome's attributable window", d.Developer, d.FlaggedOutcomes)
		}
		// The demo seeds a flat-subscription invoice per developer so the Spend
		// Leverage CFO tile renders instead of "—"; a zero here means the
		// actual_spend seed didn't land in the scored window.
		if d.SpendLeverage <= 0 {
			t.Errorf("%s has spend_leverage %.2f — actual_spend was not seeded into the scored window", d.Developer, d.SpendLeverage)
		}
	}
	if ranked != len(out.Developers) {
		t.Errorf("want all %d demo developers ranked, got %d ranked", len(out.Developers), ranked)
	}
	// The pooled tile is what the dashboard renders — a zero here means it would
	// show "—" even if the per-developer proxies above passed.
	if out.Total.SpendLeverage <= 0 {
		t.Errorf("Spend Leverage tile would render '—': total.spend_leverage = %.2f", out.Total.SpendLeverage)
	}
	// The spread is the demo's teaching value: the efficient developer must
	// outrank the high-spend/reverted one.
	if tier["demo-ada"] <= tier["demo-margaret"] {
		t.Errorf("want demo-ada (%.1f) to outrank demo-margaret (%.1f)", tier["demo-ada"], tier["demo-margaret"])
	}
}

// TestDemoData_Integrity guards the synthetic dataset: every developer needs
// BOTH cost and outcomes to score, SHAs are unique 40-hex (the merge-SHA dedup
// key), and idempotency keys are unique. It also pins every weight to TIER's
// fixed 0.5/1/3/5/8 scale: the store does NOT enforce that (the outcomes.weight
// column has no CHECK constraint — the scale is validated upstream at the
// webhook/API layer, which the demo bypasses), so the demo data must self-police
// to stay believable and honestly represent the scale.
func TestDemoData_Integrity(t *testing.T) {
	seenSHA := map[string]bool{}
	seenKey := map[string]bool{}
	devs := demoData()
	if len(devs) < 3 {
		t.Fatalf("want a varied cast for a believable demo, got %d developers", len(devs))
	}
	for _, d := range devs {
		if len(d.events) == 0 || len(d.outcomes) == 0 {
			t.Errorf("%s: needs both cost events and outcomes to produce a TIER", d.name)
		}
		for _, e := range d.events {
			if e.CostMicro <= 0 {
				t.Errorf("%s: event %q has non-positive cost", d.name, e.IssueID)
			}
			if seenKey[e.IdempotencyKey] {
				t.Errorf("duplicate idempotency key %q (would silently drop a demo row)", e.IdempotencyKey)
			}
			seenKey[e.IdempotencyKey] = true
		}
		for _, o := range d.outcomes {
			if !hex40.MatchString(o.MergeCommitSHA) {
				t.Errorf("%s: outcome SHA %q is not 40-hex", d.name, o.MergeCommitSHA)
			}
			if seenSHA[o.MergeCommitSHA] {
				t.Errorf("duplicate merge SHA %q (outcome dedup would drop it)", o.MergeCommitSHA)
			}
			seenSHA[o.MergeCommitSHA] = true
			switch o.Weight {
			case 0.5, 1, 3, 5, 8:
			default:
				t.Errorf("%s: off-scale weight %v (store does not enforce the scale — demo data must)", d.name, o.Weight)
			}
		}
	}
}

// TestGuardDemoDBPath_RefusesRealDB is the #475 control arm: a real capture
// database — here one real developer, under the demo org so ONLY the developer
// check can catch it — must be refused, and the file must survive untouched.
func TestGuardDemoDBPath_RefusesRealDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "real.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// A real developer NOT in the demo cast, deliberately under the demo org so a
	// guard that only checked the org would wave it through — the developer check
	// must be what refuses it.
	if err := db.UpsertHierarchy(context.Background(), "realdev", "Payments", "Product", demoOrg); err != nil {
		t.Fatalf("seed real developer: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := guardDemoDBPath(path, io.Discard); err == nil {
		t.Fatal("guard allowed a real database — demo would have deleted it")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("guard deleted or damaged the real database: %v", err)
	}
}

// TestGuardDemoDBPath_RefusesWebhookOnlyDB is the end-to-end control arm for the
// #475 fail-closed guard through the function that gates os.Remove. A real
// deployment can hold data ONLY in webhook_payloads — the GitHub audit trail,
// written before any Claude capture or merged-PR outcome — a table with no
// developer/org column that the identity scan cannot see. This test drives the
// actual deletion gate (guardDemoDBPath, not just the store primitive) and
// asserts it REFUSES and the file SURVIVES. It fails if PopulatedTablesOutside is
// removed from isRecreatableDemoDB — the regression guard the store-level test
// alone did not provide.
func TestGuardDemoDBPath_RefusesWebhookOnlyDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webhook-only.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// A real webhook audit row and NOTHING else — zero developers, zero orgs.
	if err := db.InsertWebhookPayload(context.Background(), "push", "delivery-1", []byte(`{"ref":"refs/heads/main"}`)); err != nil {
		t.Fatalf("seed webhook payload: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := guardDemoDBPath(path, io.Discard); err == nil {
		t.Fatal("guard allowed a webhook-only real database — demo would have deleted it (#475)")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("guard deleted or damaged the real database: %v", err)
	}
}

// TestGuardDemoDBPath_AllowsPriorDemoDB pins that a database holding only the
// synthetic cast is recognised as recreatable.
func TestGuardDemoDBPath_AllowsPriorDemoDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := seedDemo(context.Background(), db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := guardDemoDBPath(path, io.Discard); err != nil {
		t.Fatalf("guard refused a prior demo database: %v", err)
	}
}

// TestGuardDemoDBPath_AllowsNonexistentAndEmpty pins the two zero-friction paths:
// a path that does not exist yet (the default temp path on a first run) and an
// empty tier database (nothing real to lose).
func TestGuardDemoDBPath_AllowsNonexistentAndEmpty(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "does-not-exist.db")
	if err := guardDemoDBPath(missing, io.Discard); err != nil {
		t.Fatalf("guard refused a nonexistent path: %v", err)
	}

	empty := filepath.Join(dir, "empty.db")
	db, err := store.Open(empty)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := guardDemoDBPath(empty, io.Discard); err != nil {
		t.Fatalf("guard refused an empty database: %v", err)
	}
}

// TestGuardDemoDBPath_RefusesNonSQLiteFile pins fail-closed behavior on a file
// that is not a readable SQLite database: it must be refused, not deleted.
func TestGuardDemoDBPath_RefusesNonSQLiteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("this is not a sqlite database\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := guardDemoDBPath(path, io.Discard); err == nil {
		t.Fatal("guard allowed a non-SQLite file")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("guard deleted the non-SQLite file: %v", err)
	}
}

// TestSyntheticDemoBitUnreachableFromServeFlags is the #476 security control: the
// only code allowed to SET serveOptions.syntheticDemo to true is runDemo (in
// demo.go). If any other source file sets it, a `serve` invocation could relax
// the non-loopback bind guard on real data — so this test fails loudly. Paired
// with TestValidateBind's control arm (bit unset + non-loopback → refused), it
// pins that the exemption is demo-only and flag-unreachable.
//
// It parses the AST rather than grepping so comments and strings that merely
// MENTION the field (main.go documents the invariant in prose) are ignored — only
// a real assignment of true counts.
func TestSyntheticDemoBitUnreachableFromServeFlags(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	// isTrue reports whether e is the untyped boolean constant true.
	isTrue := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && id.Name == "true"
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			var setsTrue bool
			switch node := n.(type) {
			case *ast.KeyValueExpr: // serveOptions{syntheticDemo: true}
				if k, ok := node.Key.(*ast.Ident); ok && k.Name == "syntheticDemo" && isTrue(node.Value) {
					setsTrue = true
				}
			case *ast.AssignStmt: // opts.syntheticDemo = true
				for i, lhs := range node.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if ok && sel.Sel.Name == "syntheticDemo" && i < len(node.Rhs) && isTrue(node.Rhs[i]) {
						setsTrue = true
					}
				}
			}
			if setsTrue && name != "demo.go" {
				t.Errorf("%s sets syntheticDemo=true; only demo.go (runDemo) may — the bit must be unreachable from the serve path (#476)", name)
			}
			return true
		})
	}
}

func TestDemoSHA_Is40Hex(t *testing.T) {
	for _, n := range []int{0, 1, 9, 255, 1 << 30} {
		if got := demoSHA(n); !hex40.MatchString(got) {
			t.Errorf("demoSHA(%d) = %q, want 40-hex", n, got)
		}
	}
}
