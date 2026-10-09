package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for dated, append-only hierarchy membership (#886).

var (
	memT1 = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	memT2 = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	memT3 = time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
)

func setClock(db *DB, t time.Time) { db.now = func() time.Time { return t } }

// membershipOf returns dev's rows as the score paths read them.
func membershipOf(t *testing.T, db *DB, dev string) []MembershipRow {
	t.Helper()
	all, err := db.HierarchyMembership(context.Background())
	if err != nil {
		t.Fatalf("HierarchyMembership: %v", err)
	}
	var out []MembershipRow
	for _, r := range all {
		if r.Developer == dev {
			out = append(out, r)
		}
	}
	return out
}

// TestHierarchyMembership_MoveClosesAndAppends pins the append-only write: a move
// closes the open row at the server clock and opens a new one from the same
// instant; the old row's team and valid_from are untouched.
func TestHierarchyMembership_MoveClosesAndAppends(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	setClock(db, memT1)
	if err := db.UpsertHierarchy(ctx, "alice", "alpha", "div-a", "acme", "fp-1"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	setClock(db, memT2)
	if err := db.UpsertHierarchy(ctx, "alice", "beta", "div-a", "acme", "fp-2"); err != nil {
		t.Fatalf("move: %v", err)
	}

	got := membershipOf(t, db, "alice")
	if len(got) != 2 {
		t.Fatalf("rows = %+v, want the closed alpha row and the open beta row", got)
	}
	if got[0].Team != "alpha" || !got[0].ValidFrom.Equal(memT1) || !got[0].ValidTo.Equal(memT2) {
		t.Errorf("old row = %+v, want alpha [%s, %s)", got[0], memT1, memT2)
	}
	if got[1].Team != "beta" || !got[1].ValidFrom.Equal(memT2) || !got[1].ValidTo.IsZero() {
		t.Errorf("new row = %+v, want beta open from %s", got[1], memT2)
	}
	var w1, w2 string
	if err := db.db.QueryRow(`SELECT written_by FROM hierarchy_membership WHERE developer='alice' AND team='alpha'`).Scan(&w1); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT written_by FROM hierarchy_membership WHERE developer='alice' AND team='beta'`).Scan(&w2); err != nil {
		t.Fatal(err)
	}
	if w1 != "fp-1" || w2 != "fp-2" {
		t.Errorf("written_by = %q / %q, want each row to keep the writer that created it (fp-1 / fp-2)", w1, w2)
	}
}

// TestHierarchyMembership_UnchangedPlacementIsNotAMove: re-importing the same
// team and division appends nothing, so a nightly re-import does not fill the
// timeline with boundaries.
func TestHierarchyMembership_UnchangedPlacementIsNotAMove(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	setClock(db, memT1)
	if err := db.UpsertHierarchy(ctx, "alice", "alpha", "div-a", "acme", "fp"); err != nil {
		t.Fatal(err)
	}
	setClock(db, memT2)
	if err := db.UpsertHierarchies(ctx, []HierarchyRow{{Developer: "alice", Team: "alpha", Division: "div-a", Org: "acme"}}, "fp"); err != nil {
		t.Fatal(err)
	}
	if got := membershipOf(t, db, "alice"); len(got) != 1 || !got[0].ValidFrom.Equal(memT1) {
		t.Fatalf("rows = %+v, want the single original row", got)
	}
	// Control: a division change IS a move (division mode groups by it).
	setClock(db, memT3)
	if err := db.UpsertHierarchy(ctx, "alice", "alpha", "div-b", "acme", "fp"); err != nil {
		t.Fatal(err)
	}
	if got := membershipOf(t, db, "alice"); len(got) != 2 || got[1].Division != "div-b" {
		t.Fatalf("rows = %+v, want a division change to append a row", got)
	}
}

// TestHierarchyMembership_BatchIsOneInstant: every row of one import carries the
// same valid_from.
func TestHierarchyMembership_BatchIsOneInstant(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	calls := 0
	db.now = func() time.Time { calls++; return memT1.Add(time.Duration(calls) * time.Second) }
	if err := db.UpsertHierarchies(context.Background(), []HierarchyRow{
		{Developer: "a", Team: "x"}, {Developer: "b", Team: "y"}, {Developer: "c", Team: "z"},
	}, "fp"); err != nil {
		t.Fatal(err)
	}
	all, err := db.HierarchyMembership(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("rows = %+v", all)
	}
	for _, r := range all[1:] {
		if !r.ValidFrom.Equal(all[0].ValidFrom) {
			t.Errorf("batch rows start at %s and %s, want one instant", all[0].ValidFrom, r.ValidFrom)
		}
	}
}

// TestHierarchyMembership_WrittenByRequired: a write without a writer
// fingerprint is refused and writes nothing at all.
func TestHierarchyMembership_WrittenByRequired(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	if err := db.UpsertHierarchy(context.Background(), "alice", "alpha", "", "acme", ""); err == nil {
		t.Fatal("UpsertHierarchy with empty writtenBy succeeded, want refusal")
	}
	if got := membershipOf(t, db, "alice"); len(got) != 0 {
		t.Errorf("membership rows = %+v after a refused write", got)
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM org_hierarchy`).Scan(&n); err != nil || n != 0 {
		t.Errorf("org_hierarchy rows = %d (err %v) after a refused write, want 0 — the transaction must roll back whole", n, err)
	}
}

// TestHierarchyMembership_ClockBehindIsRefused: a server clock earlier than the
// open row's start cannot close it (the new row would start inside time already
// attributed to the old team). Nothing changes.
func TestHierarchyMembership_ClockBehindIsRefused(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	setClock(db, memT2)
	if err := db.UpsertHierarchy(ctx, "alice", "alpha", "", "acme", "fp"); err != nil {
		t.Fatal(err)
	}
	setClock(db, memT1)
	err := db.UpsertHierarchy(ctx, "alice", "beta", "", "acme", "fp")
	if !errors.Is(err, ErrMembershipClockBehind) {
		t.Fatalf("err = %v, want ErrMembershipClockBehind", err)
	}
	got := membershipOf(t, db, "alice")
	if len(got) != 1 || got[0].Team != "alpha" || !got[0].ValidTo.IsZero() {
		t.Errorf("rows = %+v, want the untouched open alpha row", got)
	}
}

// TestHierarchyMembership_SchemaRefusesRewrites pins the triggers: no statement
// can rewrite a row's team, division, valid_from or written_by, re-open or
// re-date a closed row, insert a closed or overlapping row, or open a second
// row. Each arm runs raw SQL, so it holds for every code path, not only the
// store's own.
func TestHierarchyMembership_SchemaRefusesRewrites(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	setClock(db, memT1)
	if err := db.UpsertHierarchy(ctx, "alice", "alpha", "div", "acme", "fp"); err != nil {
		t.Fatal(err)
	}
	setClock(db, memT2)
	if err := db.UpsertHierarchy(ctx, "alice", "beta", "div", "acme", "fp"); err != nil {
		t.Fatal(err)
	}

	refused := []struct {
		name string
		sql  string
		args []any
	}{
		{"rewrite a closed row's team", `UPDATE hierarchy_membership SET team='beta' WHERE developer='alice' AND team='alpha'`, nil},
		{"rewrite the open row's team", `UPDATE hierarchy_membership SET team='gamma' WHERE developer='alice' AND valid_to IS NULL`, nil},
		{"rewrite a division", `UPDATE hierarchy_membership SET division='x' WHERE developer='alice' AND team='alpha'`, nil},
		{"backdate valid_from", `UPDATE hierarchy_membership SET valid_from=? WHERE developer='alice' AND team='beta'`, []any{memT1}},
		{"rewrite written_by", `UPDATE hierarchy_membership SET written_by='someone-else' WHERE developer='alice' AND team='beta'`, nil},
		{"re-open a closed row", `UPDATE hierarchy_membership SET valid_to=NULL WHERE developer='alice' AND team='alpha'`, nil},
		{"re-date a closed row", `UPDATE hierarchy_membership SET valid_to=? WHERE developer='alice' AND team='alpha'`, []any{memT3}},
		{"insert a closed (backdated) row", `INSERT INTO hierarchy_membership (developer, team, division, valid_from, valid_to, written_by) VALUES ('alice','gamma','',?,?,'fp')`, []any{memT1, memT2}},
		{"insert a row reaching back over closed history", `INSERT INTO hierarchy_membership (developer, team, division, valid_from, written_by) VALUES ('carol','gamma','',?,'fp')`, []any{memT2}},
		{"insert a second open row", `INSERT INTO hierarchy_membership (developer, team, division, valid_from, written_by) VALUES ('alice','gamma','',?,'fp')`, []any{memT3}},
	}
	// carol has only CLOSED history, [T1, T3): no open row, so the open-row unique
	// index cannot be what refuses her backdated insert — only the overlap trigger.
	setClock(db, memT1)
	if err := db.UpsertHierarchy(ctx, "carol", "alpha", "", "acme", "fp"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE hierarchy_membership SET valid_to=? WHERE developer='carol'`, memT3); err != nil {
		t.Fatalf("close carol's row: %v", err)
	}

	for _, c := range refused {
		if _, err := db.db.ExecContext(ctx, c.sql, c.args...); err == nil {
			t.Errorf("%s: statement succeeded, want the schema to refuse it", c.name)
		}
	}
	if got := membershipOf(t, db, "alice"); len(got) != 2 ||
		got[0].Team != "alpha" || !got[0].ValidFrom.Equal(memT1) || !got[0].ValidTo.Equal(memT2) ||
		got[1].Team != "beta" || !got[1].ValidFrom.Equal(memT2) || !got[1].ValidTo.IsZero() {
		t.Errorf("alice's rows changed: %+v", got)
	}

	// Control arm: the one permitted transition still works, so the refusals
	// above are the triggers' doing, not a table that refuses everything.
	if _, err := db.db.ExecContext(ctx, `UPDATE hierarchy_membership SET valid_to=? WHERE developer='alice' AND valid_to IS NULL`, memT3); err != nil {
		t.Fatalf("control: closing the open row failed: %v", err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO hierarchy_membership (developer, team, division, valid_from, written_by) VALUES ('alice','gamma','',?,'fp')`, memT3); err != nil {
		t.Fatalf("control: appending an open row at the close instant failed: %v", err)
	}
}

// TestBackfillHierarchyMembership_BaselinesAnUpgradedInstall models an install
// upgrading to #886: org_hierarchy holds rows and no dated table exists. Every
// row is baselined open from MembershipBaselineFrom, once; an assignment made
// after the upgrade is stamped with the server clock, not the sentinel.
func TestBackfillHierarchyMembership_BaselinesAnUpgradedInstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Rewind to the pre-#886 shape.
	for _, stmt := range []string{
		`DROP TABLE hierarchy_membership`,
		`DELETE FROM tier_migrations WHERE name = 'hierarchy_membership_baseline_v886'`,
		`INSERT INTO org_hierarchy (developer, team, division, org) VALUES ('alice','alpha',NULL,'acme'), ('bob','beta','div-b','acme')`,
	} {
		if _, err := db.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	_ = db.Close()

	for boot := 1; boot <= 2; boot++ {
		db, err = Open(path)
		if err != nil {
			t.Fatalf("boot %d: %v", boot, err)
		}
		all, err := db.HierarchyMembership(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 2 {
			t.Fatalf("boot %d: rows = %+v, want one baseline row per org_hierarchy row (idempotent)", boot, all)
		}
		for _, r := range all {
			if !r.ValidFrom.Equal(MembershipBaselineFrom) || !r.ValidTo.IsZero() {
				t.Errorf("boot %d: %s = %+v, want open from the baseline sentinel", boot, r.Developer, r)
			}
		}
		if all[0].Team != "alpha" || all[0].Division != "" || all[1].Team != "beta" || all[1].Division != "div-b" {
			t.Errorf("boot %d: baseline did not copy the placement: %+v", boot, all)
		}
		var by string
		if err := db.db.QueryRow(`SELECT written_by FROM hierarchy_membership WHERE developer='alice'`).Scan(&by); err != nil || by != MembershipWriterBaseline {
			t.Errorf("boot %d: written_by = %q (err %v), want %q", boot, by, err, MembershipWriterBaseline)
		}
		if boot == 1 {
			_ = db.Close()
		}
	}
	defer func() { _ = db.Close() }()

	// Post-upgrade assignment: server clock, never the sentinel.
	setClock(db, memT3)
	if err := db.UpsertHierarchy(context.Background(), "carol", "alpha", "", "acme", "fp"); err != nil {
		t.Fatal(err)
	}
	if got := membershipOf(t, db, "carol"); len(got) != 1 || !got[0].ValidFrom.Equal(memT3) {
		t.Errorf("carol = %+v, want one row from %s — a post-upgrade assignment must not inherit the baseline", got, memT3)
	}
}

// TestEraseDeveloper_RemovesEveryMembershipRow: erasure deletes a person's whole
// dated history — closed rows, the open row, and rows under an alias — and
// nobody else's.
func TestEraseDeveloper_RemovesEveryMembershipRow(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	for i, team := range []string{"alpha", "beta", "gamma"} {
		setClock(db, memT1.AddDate(0, i, 0))
		for _, dev := range []string{"alice", "bob"} {
			if err := db.UpsertHierarchy(ctx, dev, team, "", "acme", "fp"); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The alias write appends alice-gh's own row at the clock (#914).
	if err := db.UpsertDeveloperAlias(ctx, "alice-gh", "alice", "test:fixture"); err != nil {
		t.Fatal(err)
	}

	counts, err := db.EraseDeveloper(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if counts["hierarchy_membership"] != 4 {
		t.Errorf("deleted hierarchy_membership rows = %d, want 4 (3 under alice, 1 under her alias)", counts["hierarchy_membership"])
	}
	for _, dev := range []string{"alice", "alice-gh"} {
		if got := membershipOf(t, db, dev); len(got) != 0 {
			t.Errorf("%s still has membership rows after erase: %+v", dev, got)
		}
	}
	if got := membershipOf(t, db, "bob"); len(got) != 3 {
		t.Errorf("bob has %d membership rows after alice's erase, want 3 (untouched)", len(got))
	}
}

// TestExportDeveloper_DisclosesMembershipWithoutWriter: the DSAR export carries
// the subject's dated history and never the operator credential fingerprint.
func TestExportDeveloper_DisclosesMembershipWithoutWriter(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	const fp = "sha256:0123456789abcdef"
	setClock(db, memT1)
	if err := db.UpsertHierarchy(ctx, "alice", "alpha", "", "acme", fp); err != nil {
		t.Fatal(err)
	}
	setClock(db, memT2)
	if err := db.UpsertHierarchy(ctx, "alice", "beta", "", "acme", fp); err != nil {
		t.Fatal(err)
	}
	exp, err := db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.HierarchyMembership) != 2 || exp.HierarchyMembership[0].ValidTo == nil || exp.HierarchyMembership[1].ValidTo != nil {
		t.Fatalf("exported membership = %+v, want the closed alpha row then the open beta row", exp.HierarchyMembership)
	}
	raw, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), fp) || strings.Contains(string(raw), "written_by") {
		t.Errorf("DSAR export carries the writer fingerprint: %s", raw)
	}
}

// TestActualSpendByPeriodWindow_ReconcilesWithTheDeveloperTotal: the per-period
// read is the same allocation as ActualSpendAllWindow, split by period — tier-1
// invoices and the org-fallback remainder both.
func TestActualSpendByPeriodWindow_ReconcilesWithTheDeveloperTotal(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	setClock(db, memT1)
	for _, dev := range []string{"alice", "bob", "carol"} {
		if err := db.UpsertHierarchy(ctx, dev, "t", "", "acme", "fp"); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []ActualSpend{
		{Developer: "alice", Period: "2026-03", ActualPaidMicro: 100_000_000, Timestamp: memT1},
		{Developer: "alice", Period: "2026-04", ActualPaidMicro: 30_000_000, Timestamp: memT1},
		{Developer: "bob", Period: "2026-04", ActualPaidMicro: 5_000_000, Timestamp: memT1},
	} {
		if err := db.InsertActualSpend(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{Org: "acme", Period: "2026-03", ActualPaidMicro: 400_000_000, Timestamp: memT1}); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	total, err := db.ActualSpendAllWindow(ctx, since, until)
	if err != nil {
		t.Fatal(err)
	}
	periods, err := db.ActualSpendByPeriodWindow(ctx, since, until)
	if err != nil {
		t.Fatal(err)
	}
	sum := map[string]float64{}
	perDevPeriods := map[string]int{}
	for _, p := range periods {
		sum[p.Developer] += p.USD
		perDevPeriods[p.Developer]++
	}
	if len(sum) != len(total) {
		t.Fatalf("developers: by-period %v, total %v", sum, total)
	}
	for dev, want := range total {
		if diff := sum[dev] - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s: by-period sum %v, total %v", dev, sum[dev], want)
		}
	}
	// alice: tier-1 in both periods; bob: org fallback in 03 + tier-1 in 04.
	if perDevPeriods["alice"] != 2 || perDevPeriods["bob"] != 2 {
		t.Errorf("periods per developer = %v, want alice and bob split across 2026-03 and 2026-04", perDevPeriods)
	}
}
