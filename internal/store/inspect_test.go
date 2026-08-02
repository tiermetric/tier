package store

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestInspectIdentities_ReadsAcrossTables pins that InspectIdentities surfaces a
// developer or org from ANY identity-bearing table, so `tierd demo`'s #475 guard
// cannot miss real data that lives in only one of them.
func TestInspectIdentities_ReadsAcrossTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()

	// A developer that exists ONLY via a token event (no hierarchy row) — the
	// union must still find it.
	if err := db.InsertTokenEvents(ctx, []TokenEvent{{
		Developer: "cost-only-dev", IssueID: "ISS-1", Model: "m",
		CostMicro: 1, Source: "jsonl", Fidelity: "realtime", IdempotencyKey: "k1",
	}}); err != nil {
		t.Fatalf("insert token event: %v", err)
	}
	// A developer + org that exist only via the hierarchy.
	if err := db.UpsertHierarchy(ctx, "hier-dev", "Team", "Div", "RealCorp"); err != nil {
		t.Fatalf("upsert hierarchy: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ids, err := InspectIdentities(ctx, path)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	sort.Strings(ids.Developers)
	if !contains(ids.Developers, "cost-only-dev") || !contains(ids.Developers, "hier-dev") {
		t.Errorf("developers = %v, want both cost-only-dev and hier-dev", ids.Developers)
	}
	if !contains(ids.Orgs, "RealCorp") {
		t.Errorf("orgs = %v, want RealCorp", ids.Orgs)
	}
}

// TestInspectIdentities_EmptyDB pins that a freshly-migrated, empty database
// yields empty slices and no error (the recreatable-demo path treats that as
// "nothing real to lose").
func TestInspectIdentities_EmptyDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	ids, err := InspectIdentities(context.Background(), path)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if len(ids.Developers) != 0 || len(ids.Orgs) != 0 {
		t.Errorf("empty db returned identities: %+v", ids)
	}
}

// TestInspectIdentities_NonSQLiteFileErrors pins fail-closed behavior: a file
// that is not a readable SQLite database is an error, never a silent empty
// result that would let the demo guard mistake it for recreatable.
func TestInspectIdentities_NonSQLiteFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.db")
	if err := os.WriteFile(path, []byte("definitely not sqlite"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := InspectIdentities(context.Background(), path); err == nil {
		t.Fatal("InspectIdentities returned nil error for a non-SQLite file")
	}
}

// TestInspectIdentities_DoesNotMutate pins the #475 safety guarantee at the store
// layer: inspecting a real database must not change its bytes. Reading a WAL
// database may (re)build the ephemeral -shm, which is expected and not part of
// the database content, so the assertion is on the main DB file only.
func TestInspectIdentities_DoesNotMutate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	if err := db.UpsertHierarchy(ctx, "dev", "Team", "Div", "RealCorp"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	if _, err := InspectIdentities(ctx, path); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(before) != string(after) {
		t.Error("InspectIdentities mutated the database file")
	}
}

// TestPopulatedTablesOutside_CatchesNonIdentityTables is the control arm for the
// #475 fail-closed guard: a database whose ONLY rows live in a table with no
// developer/org column (webhook_payloads — the GitHub audit trail a
// webhook-configured deployment holds before any capture or outcome) must be
// reported as populated, so `tierd demo`'s guard refuses to delete it. Before
// this the identity-only scan saw it as empty and the DB was recreatable.
func TestPopulatedTablesOutside_CatchesNonIdentityTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webhook-only.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	// A real webhook audit row, no developer/org anywhere in the DB.
	if err := db.InsertWebhookPayload(ctx, "push", "delivery-1", []byte(`{"ref":"refs/heads/main"}`)); err != nil {
		t.Fatalf("insert webhook payload: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Identity scan sees nothing — this is exactly the gap.
	ids, err := InspectIdentities(ctx, path)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if len(ids.Developers) != 0 || len(ids.Orgs) != 0 {
		t.Fatalf("expected no identities from a webhook-only DB, got %+v", ids)
	}

	// The table scan MUST catch it (allow-list is the demo seeder's own tables).
	demoTables := []string{"token_events", "outcomes", "actual_spend", "org_hierarchy", "period_membership"}
	extra, err := PopulatedTablesOutside(ctx, path, demoTables)
	if err != nil {
		t.Fatalf("PopulatedTablesOutside: %v", err)
	}
	if !contains(extra, "webhook_payloads") {
		t.Errorf("PopulatedTablesOutside = %v, want it to include webhook_payloads", extra)
	}
}

// TestPopulatedTablesOutside_DemoDBIsClean pins the other direction: a DB with
// rows ONLY in the demo seeder's tables reports nothing outside the allow-list,
// so a genuine prior demo database stays recreatable.
func TestPopulatedTablesOutside_DemoDBIsClean(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	if err := db.InsertTokenEvents(ctx, []TokenEvent{{
		Developer: "demo-ada", IssueID: "DEMO-1", Model: "m",
		CostMicro: 1, Source: "jsonl", Fidelity: "realtime", IdempotencyKey: "k1",
	}}); err != nil {
		t.Fatalf("insert token event: %v", err)
	}
	if err := db.UpsertHierarchy(ctx, "demo-ada", "Team", "Div", "ACME (DEMO)"); err != nil {
		t.Fatalf("upsert hierarchy: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	demoTables := []string{"token_events", "outcomes", "actual_spend", "org_hierarchy", "period_membership"}
	extra, err := PopulatedTablesOutside(ctx, path, demoTables)
	if err != nil {
		t.Fatalf("PopulatedTablesOutside: %v", err)
	}
	if len(extra) != 0 {
		t.Errorf("PopulatedTablesOutside = %v, want empty for a demo-only DB", extra)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
