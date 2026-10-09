package store

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestSeedDemoMembershipBaseline_EmptyStore pins the demo baseline (#886): on an
// empty store each developer gets exactly one OPEN row valid from the upgrade
// baseline instant, and a following UpsertHierarchies with the same placements
// appends nothing.
func TestSeedDemoMembershipBaseline_EmptyStore(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	rows := []HierarchyRow{
		{Developer: "demo-a", Team: "Payments", Division: "Product", Org: "ACME"},
		{Developer: "demo-b", Team: "Search", Division: "Product", Org: "ACME"},
	}
	if err := db.SeedDemoMembershipBaseline(ctx, rows); err != nil {
		t.Fatalf("SeedDemoMembershipBaseline: %v", err)
	}
	setClock(db, time.Now().UTC())
	if err := db.UpsertHierarchies(ctx, rows, "demo:seed"); err != nil {
		t.Fatalf("UpsertHierarchies: %v", err)
	}
	for _, r := range rows {
		got := membershipOf(t, db, r.Developer)
		if len(got) != 1 || got[0].Team != r.Team || got[0].Division != r.Division ||
			!got[0].ValidFrom.Equal(MembershipBaselineFrom) || !got[0].ValidTo.IsZero() {
			t.Errorf("%s rows = %+v, want one open %s row valid from the baseline instant", r.Developer, got, r.Team)
		}
	}
}

// TestSeedDemoMembershipBaseline_RefusesANonEmptyStore: any event, outcome,
// invoice or membership row already in the store refuses the baseline and
// writes nothing — a row dated at the baseline instant would otherwise place
// history that exists.
func TestSeedDemoMembershipBaseline_RefusesANonEmptyStore(t *testing.T) {
	past := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	seeds := map[string]func(*DB) error{
		"token_events": func(db *DB) error {
			return db.InsertTokenEvent(context.Background(), TokenEvent{Developer: "x", IssueID: "i", Model: "claude-sonnet-4",
				InputTok: 1, CostMicro: 1, Source: "jsonl", Fidelity: "realtime", Timestamp: past})
		},
		"outcomes": func(db *DB) error {
			_, err := db.InsertOutcome(context.Background(), Outcome{Developer: "x", IssueID: "i", Weight: 1, Quality: 1,
				MergeCommitSHA: "sha-x", Timestamp: past})
			return err
		},
		"actual_spend": func(db *DB) error {
			return db.InsertActualSpend(context.Background(), ActualSpend{Developer: "x", Period: "2026-05", ActualPaidMicro: 1, Timestamp: past})
		},
		"hierarchy_membership": func(db *DB) error {
			return db.UpsertHierarchy(context.Background(), "x", "t", "", "", "fp")
		},
	}
	for table, seed := range seeds {
		t.Run(table, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			if err := seed(db); err != nil {
				t.Fatalf("seed %s: %v", table, err)
			}
			before, err := db.HierarchyMembership(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			err = db.SeedDemoMembershipBaseline(context.Background(), []HierarchyRow{{Developer: "demo-a", Team: "Payments"}})
			if !errors.Is(err, ErrDemoBaselineStoreNotEmpty) {
				t.Fatalf("err = %v, want ErrDemoBaselineStoreNotEmpty", err)
			}
			after, err := db.HierarchyMembership(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Errorf("membership changed on a refusal: %+v -> %+v", before, after)
			}
		})
	}
}

// TestSeedDemoMembershipBaseline_OnlyCallerIsTheDemo scans every non-test Go
// file of the module and pins cmd/tierd/demo.go as the ONLY reference to
// SeedDemoMembershipBaseline: it is the one store path that writes a dated
// membership row not stamped by the server clock, and no request, CLI or
// import path may reach it.
func TestSeedDemoMembershipBaseline_OnlyCallerIsTheDemo(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", root, err)
	}
	var callers []string
	scanned, sawStore := 0, false
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir // another module
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		scanned++
		if filepath.Base(path) == "store.go" && filepath.Base(filepath.Dir(path)) == "store" {
			sawStore = true
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "SeedDemoMembershipBaseline" {
				rel, _ := filepath.Rel(root, path)
				callers = append(callers, filepath.ToSlash(rel))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 || !sawStore {
		t.Fatalf("scanned %d Go files (internal/store/store.go seen: %v), want the whole module", scanned, sawStore)
	}
	if !reflect.DeepEqual(callers, []string{"cmd/tierd/demo.go"}) {
		t.Errorf("SeedDemoMembershipBaseline referenced from %v, want only cmd/tierd/demo.go", callers)
	}
}
