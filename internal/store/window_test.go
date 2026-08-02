package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestWindowedReads_HalfOpen exercises the #276 half-open [since, until) bound
// on the scores-path store reads directly. Events sit at X-1s, X, Y-1s and Y;
// the bounded reads must return only the X and Y-1s rows (lower inclusive,
// upper exclusive), and the open-ended forms (zero until, or the pre-#276
// no-until methods) must include Y.
func TestWindowedReads_HalfOpen(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "tier-window.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	x := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	y := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	insert := func(issue string, ts time.Time) {
		if err := db.InsertTokenEvent(ctx, TokenEvent{
			Developer: "alice", IssueID: issue, Model: "claude-sonnet-4",
			InputTok: 2000, CostMicro: 10_000_000, Source: "jsonl",
			Fidelity: "realtime", Timestamp: ts,
		}); err != nil {
			t.Fatalf("insert %s: %v", issue, err)
		}
		if _, err := db.InsertOutcome(ctx, Outcome{
			Developer: "alice", IssueID: issue, PRNumber: 1,
			Weight: 1, Quality: 1, Timestamp: ts,
		}); err != nil {
			t.Fatalf("insert outcome %s: %v", issue, err)
		}
	}
	insert("i-xm1", x.Add(-time.Second)) // X-1s: excluded
	insert("i-x", x)                     // X:    included
	insert("i-ym1", y.Add(-time.Second)) // Y-1s: included
	insert("i-y", y)                     // Y:    excluded

	sumCosts := func(cs []DeveloperCost) int64 {
		var m int64
		for _, c := range cs {
			m += c.TotalCostMicro
		}
		return m
	}

	// Bounded [X, Y): exactly X and Y-1s → 20_000_000 micro.
	bounded, err := db.DeveloperCostsWindow(ctx, x, y)
	if err != nil {
		t.Fatalf("DeveloperCostsWindow: %v", err)
	}
	if got := sumCosts(bounded); got != 20_000_000 {
		t.Errorf("DeveloperCostsWindow [X,Y) = %d micro, want 20_000_000", got)
	}

	// Zero until is unbounded above: X, Y-1s and Y → 30_000_000.
	open, err := db.DeveloperCostsWindow(ctx, x, time.Time{})
	if err != nil {
		t.Fatalf("DeveloperCostsWindow open: %v", err)
	}
	if got := sumCosts(open); got != 30_000_000 {
		t.Errorf("DeveloperCostsWindow [X,inf) = %d micro, want 30_000_000", got)
	}

	// The pre-#276 method must be byte-for-byte equivalent to the zero-until form.
	legacy, err := db.DeveloperCosts(ctx, x)
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	if got := sumCosts(legacy); got != 30_000_000 {
		t.Errorf("DeveloperCosts (legacy open-ended) = %d micro, want 30_000_000", got)
	}

	// Outcomes read honors the same bound.
	outs, err := db.AllOutcomesWindow(ctx, x, y)
	if err != nil {
		t.Fatalf("AllOutcomesWindow: %v", err)
	}
	if len(outs) != 2 {
		t.Errorf("AllOutcomesWindow [X,Y) count = %d, want 2", len(outs))
	}

	// Issue-grain cost read honors the same bound.
	ic, err := db.DeveloperIssueCostsWindow(ctx, x, y)
	if err != nil {
		t.Fatalf("DeveloperIssueCostsWindow: %v", err)
	}
	var icMicro int64
	for _, c := range ic {
		icMicro += c.TotalCostMicro
	}
	if icMicro != 20_000_000 {
		t.Errorf("DeveloperIssueCostsWindow [X,Y) = %d micro, want 20_000_000", icMicro)
	}
}

// TestActualSpendAllWindow_PeriodBound covers the monthly upper bound on the
// spend read (#276), which is the riskiest #276 path because it rewrites the
// shared allocation CTE's bound-parameter list (actualSpendCTEBounded). Three
// tier-1 invoices in Feb/Mar/Apr; a window ending at until=2026-04-01 must
// include Feb and Mar (period < "2026-04") and exclude Apr, while the
// open-ended form includes all three.
func TestActualSpendAllWindow_PeriodBound(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "tier-spend-window.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now().UTC()
	for _, a := range []ActualSpend{
		{Developer: "alice", Period: "2026-02", ActualPaidMicro: 100 * MicroPerUSD, Timestamp: now},
		{Developer: "alice", Period: "2026-03", ActualPaidMicro: 200 * MicroPerUSD, Timestamp: now},
		{Developer: "alice", Period: "2026-04", ActualPaidMicro: 400 * MicroPerUSD, Timestamp: now},
	} {
		if err := db.InsertActualSpend(ctx, a); err != nil {
			t.Fatalf("InsertActualSpend(%+v): %v", a, err)
		}
	}

	since := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	bounded, err := db.ActualSpendAllWindow(ctx, since, until)
	if err != nil {
		t.Fatalf("ActualSpendAllWindow: %v", err)
	}
	if got := bounded["alice"]; got != 300 { // Feb 100 + Mar 200; Apr excluded
		t.Errorf("ActualSpendAllWindow [Feb, Apr) alice = %v, want 300 (Apr must be excluded)", got)
	}

	open, err := db.ActualSpendAllWindow(ctx, since, time.Time{})
	if err != nil {
		t.Fatalf("ActualSpendAllWindow open: %v", err)
	}
	if got := open["alice"]; got != 700 { // all three months
		t.Errorf("ActualSpendAllWindow [Feb, inf) alice = %v, want 700", got)
	}
}

// TestActualSpendAllWindow_OrgFallbackPeriodBound exercises the riskiest #276
// path: the bounded org-fallback allocation (actualSpendCTEBounded over
// org_actual_spend + period_membership, Part B). The tier-1-only Part-B seed in
// the sibling test never returns a row, so it validates the bound-PARAM count but
// not that `AND period < ?` actually EXCLUDES an out-of-window period from the
// per-period remainder split. Here two seats (no tier-1) split a $2000 in-window
// org invoice and an $8000 out-of-window one: the bounded [Feb, Apr) read must
// give each seat only the Feb slice ($1000), proving the Apr org spend is dropped;
// the open-ended read must include Apr ($1000 + $4000 = $5000).
func TestActualSpendAllWindow_OrgFallbackPeriodBound(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	// Two seats in org "acme", both org-fallback (no tier-1 actual_spend). First
	// enrollment makes them active since "0000-01", so both count in every period.
	for _, dev := range []string{"alice", "bob"} {
		if err := db.UpsertHierarchy(ctx, dev, "platform", "", "acme"); err != nil {
			t.Fatalf("UpsertHierarchy(%s): %v", dev, err)
		}
	}
	for _, o := range []OrgActualSpend{
		{Org: "acme", Period: "2026-02", ActualPaidMicro: 2000 * MicroPerUSD, Timestamp: now}, // in [Feb, Apr)
		{Org: "acme", Period: "2026-04", ActualPaidMicro: 8000 * MicroPerUSD, Timestamp: now}, // at Apr: excluded
	} {
		if err := db.InsertOrgActualSpend(ctx, o); err != nil {
			t.Fatalf("InsertOrgActualSpend(%+v): %v", o, err)
		}
	}

	since := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	// Bounded [Feb, Apr): only the Feb invoice is in-window. $2000 / 2 seats =
	// $1000 each; the Apr org spend must be excluded by `AND period < ?`.
	bounded, err := db.ActualSpendAllWindow(ctx, since, until)
	if err != nil {
		t.Fatalf("ActualSpendAllWindow bounded: %v", err)
	}
	for _, dev := range []string{"alice", "bob"} {
		if got := bounded[dev]; got != 1000 {
			t.Errorf("bounded [Feb, Apr) %s org-fallback share = %v, want 1000 (Apr org spend must be excluded)", dev, got)
		}
	}

	// Open-ended: Apr re-enters, so each seat also gets $8000 / 2 = $4000 → $5000.
	open, err := db.ActualSpendAllWindow(ctx, since, time.Time{})
	if err != nil {
		t.Fatalf("ActualSpendAllWindow open: %v", err)
	}
	for _, dev := range []string{"alice", "bob"} {
		if got := open[dev]; got != 5000 {
			t.Errorf("open [Feb, inf) %s org-fallback share = %v, want 5000 (Feb $1000 + Apr $4000)", dev, got)
		}
	}
}
