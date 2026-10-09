package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// pastInvoicePeriod is a billing period strictly before any month these tests
// run in, so a membership opened "this month" can never cover it.
const pastInvoicePeriod = "2025-06"

// pastWindowAlloc returns every developer's allocation for pastInvoicePeriod
// alone — the historical seat split a re-enrolment must not rewrite.
func pastWindowAlloc(t *testing.T, db *DB) map[string]float64 {
	t.Helper()
	since := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	got, err := db.ActualSpendAllWindow(context.Background(), since, since.AddDate(0, 1, 0))
	if err != nil {
		t.Fatalf("ActualSpendAllWindow: %v", err)
	}
	return got
}

// openStart returns the period_start of developer's open membership in org.
func openStart(t *testing.T, db *DB, developer, org string) string {
	t.Helper()
	var start string
	if err := db.db.QueryRow(
		`SELECT period_start FROM period_membership
		 WHERE developer = ? AND org = ? AND period_end IS NULL`, developer, org,
	).Scan(&start); err != nil {
		t.Fatalf("open membership (%s, %s): %v", developer, org, err)
	}
	return start
}

// upsertAtMonth calls UpsertHierarchy and returns the UTC months read just
// before and just after it, so a caller can accept either side of a month
// boundary the call may straddle.
func upsertAtMonth(t *testing.T, db *DB, developer, org string) (before, after string) {
	t.Helper()
	before = time.Now().UTC().Format("2006-01")
	if err := db.UpsertHierarchy(context.Background(), developer, "platform", "", org, "test:fixture"); err != nil {
		t.Fatalf("UpsertHierarchy(%s, %q): %v", developer, org, err)
	}
	after = time.Now().UTC().Format("2006-01")
	return before, after
}

func assertAllocUnchanged(t *testing.T, before, after map[string]float64) {
	t.Helper()
	for dev, want := range before {
		if after[dev] != want {
			t.Errorf("%s's %s allocation = %v, want %v (unchanged by the re-enrolment)", dev, pastInvoicePeriod, after[dev], want)
		}
	}
	for dev, got := range after {
		if _, ok := before[dev]; !ok && got != 0 {
			t.Errorf("%s gained a %s allocation of %v from a re-enrolment", dev, pastInvoicePeriod, got)
		}
	}
}

// Pins #867: a developer whose org was cleared and who is later assigned a
// DIFFERENT org opens that membership in the assignment month, so the new
// org's past invoices keep their seat split; a genuine first enrolment still
// opens at the '0000-01' sentinel.
func TestUpsertHierarchy_ReenrolAfterClearOpensThisMonth(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	upsertAtMonth(t, db, "bob", "globex")
	if got := openStart(t, db, "bob", "globex"); got != "0000-01" {
		t.Fatalf("first enrolment opened at %q, want the '0000-01' sentinel", got)
	}
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "globex", Period: pastInvoicePeriod, ActualPaidMicro: 1000 * MicroPerUSD, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend: %v", err)
	}

	upsertAtMonth(t, db, "alice", "acme")
	upsertAtMonth(t, db, "alice", "") // clear: a departure
	baseline := pastWindowAlloc(t, db)
	if baseline["bob"] != 1000 {
		t.Fatalf("baseline bob = %v, want 1000 (sole globex seat)", baseline["bob"])
	}

	before, after := upsertAtMonth(t, db, "alice", "globex")
	if got := openStart(t, db, "alice", "globex"); got != before && got != after {
		t.Errorf("re-enrolment opened globex at %q, want the assignment month %q", got, before)
	}
	assertAllocUnchanged(t, baseline, pastWindowAlloc(t, db))
}

// Pins #867's same-org arm: a developer who left an org months ago (org
// cleared, membership closed) and returns to it opens a new membership in the
// return month, not at '0000-01' — which would re-seat them for the gap.
func TestUpsertHierarchy_ReturnToSameOrgAfterGapOpensThisMonth(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	seedMembership(t, db, "bob", "acme", "0000-01", "")
	seedMembership(t, db, "alice", "acme", "0000-01", "2025-03")
	if _, err := db.db.Exec(
		`INSERT INTO org_hierarchy (developer, team, division, org) VALUES
		 ('bob', 'platform', '', 'acme'), ('alice', 'platform', '', '')`,
	); err != nil {
		t.Fatalf("seed org_hierarchy: %v", err)
	}
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "acme", Period: pastInvoicePeriod, ActualPaidMicro: 1000 * MicroPerUSD, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend: %v", err)
	}
	baseline := pastWindowAlloc(t, db)
	if baseline["bob"] != 1000 {
		t.Fatalf("baseline bob = %v, want 1000 (alice left in 2025-03)", baseline["bob"])
	}

	before, after := upsertAtMonth(t, db, "alice", "acme")
	if got := openStart(t, db, "alice", "acme"); got != before && got != after {
		t.Errorf("return to acme opened at %q, want the return month %q", got, before)
	}
	assertAllocUnchanged(t, baseline, pastWindowAlloc(t, db))

	var closedEnd sql.NullString
	if err := db.db.QueryRow(
		`SELECT period_end FROM period_membership
		 WHERE developer = 'alice' AND org = 'acme' AND period_start = '0000-01'`,
	).Scan(&closedEnd); err != nil {
		t.Fatalf("closed membership: %v", err)
	}
	if closedEnd.String != "2025-03" {
		t.Errorf("alice's earlier membership period_end = %v, want 2025-03 (history untouched)", closedEnd)
	}
}

// Pins #867's ended-membership arm: EndMembership leaves org_hierarchy.org set,
// so an explicit rejoin opens this month, not at '0000-01'.
func TestUpsertHierarchy_ReturnAfterEndMembershipOpensThisMonth(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	upsertAtMonth(t, db, "bob", "acme")
	upsertAtMonth(t, db, "alice", "acme")
	if err := db.EndMembership(ctx, "alice", "acme", "2025-03"); err != nil {
		t.Fatalf("EndMembership: %v", err)
	}
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "acme", Period: pastInvoicePeriod, ActualPaidMicro: 1000 * MicroPerUSD, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend: %v", err)
	}
	baseline := pastWindowAlloc(t, db)
	if baseline["bob"] != 1000 {
		t.Fatalf("baseline bob = %v, want 1000 (alice ended in 2025-03)", baseline["bob"])
	}

	before := time.Now().UTC().Format("2006-01")
	if err := db.UpsertHierarchies(ctx, []HierarchyRow{{Developer: "alice", Team: "platform", Org: "acme", Rejoin: true}}, "test:fixture"); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC().Format("2006-01")
	if got := openStart(t, db, "alice", "acme"); got != before && got != after {
		t.Errorf("return after EndMembership opened acme at %q, want the return month %q", got, before)
	}
	assertAllocUnchanged(t, baseline, pastWindowAlloc(t, db))
}

// Pins that a team-only placement (org "") is not membership history: the
// developer's first org still opens at the '0000-01' sentinel.
func TestUpsertHierarchy_FirstOrgAfterTeamOnlyOpensAtSentinel(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	upsertAtMonth(t, db, "zoe", "")
	upsertAtMonth(t, db, "zoe", "acme")
	if got := openStart(t, db, "zoe", "acme"); got != "0000-01" {
		t.Errorf("first org after a team-only placement opened at %q, want the '0000-01' sentinel", got)
	}
}

// Pins that membership history is the person's, not the raw id's: history
// recorded under an id later aliased to the canonical makes the canonical's
// next org a re-enrolment.
func TestUpsertHierarchy_ReenrolAcrossAliasOpensThisMonth(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	upsertAtMonth(t, db, "bob", "globex")
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "globex", Period: pastInvoicePeriod, ActualPaidMicro: 1000 * MicroPerUSD, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend: %v", err)
	}
	upsertAtMonth(t, db, "alice-laptop", "acme")
	upsertAtMonth(t, db, "alice-laptop", "")
	if err := db.UpsertDeveloperAlias(ctx, "alice-laptop", "alice", "test:fixture"); err != nil {
		t.Fatalf("UpsertDeveloperAlias: %v", err)
	}
	baseline := pastWindowAlloc(t, db)
	if baseline["bob"] != 1000 {
		t.Fatalf("baseline bob = %v, want 1000 (sole globex seat)", baseline["bob"])
	}

	before, after := upsertAtMonth(t, db, "alice", "globex")
	if got := openStart(t, db, "alice", "globex"); got != before && got != after {
		t.Errorf("re-enrolment across an alias opened globex at %q, want the assignment month %q", got, before)
	}
	assertAllocUnchanged(t, baseline, pastWindowAlloc(t, db))
}
