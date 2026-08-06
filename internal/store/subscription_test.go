package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// assertSubscriptionNet fails unless the org_actual_spend net for (org, period)
// SCOPED TO THE ROUTE'S OWN SOURCE equals want micro-dollars. This is what the
// reconciler reads back, so asserting it is asserting the idempotency memory
// itself — not merely the money.
func assertSubscriptionNet(t *testing.T, db *DB, routePrefix, org, period string, want int64) {
	t.Helper()
	net, err := db.OrgActualSpendNet(context.Background(), org, period, SubscriptionSpendSource(routePrefix))
	if err != nil {
		t.Fatalf("OrgActualSpendNet(%s, %s, %s): %v", org, period, routePrefix, err)
	}
	if net != want {
		t.Errorf("subscription net(%s, %s, %s) = %d micro, want %d", routePrefix, org, period, net, want)
	}
}

// assertOrgNetAllSources fails unless the CROSS-SOURCE org_actual_spend total
// for (org, period) equals want. This is the figure the allocation read (and
// therefore Spend Leverage) actually consumes, so it is the one that proves the
// fee reached the metric rather than just the table.
func assertOrgNetAllSources(t *testing.T, db *DB, org, period string, want int64) {
	t.Helper()
	var got int64
	if err := db.db.QueryRow(`
		SELECT COALESCE(SUM(actual_paid_micro), 0) FROM org_actual_spend
		WHERE org = ? AND period = ?`, org, period).Scan(&got); err != nil {
		t.Fatalf("cross-source net(%s, %s): %v", org, period, err)
	}
	if got != want {
		t.Errorf("cross-source net(%s, %s) = %d micro, want %d", org, period, got, want)
	}
}

// countOrgSpendRows returns how many org_actual_spend ROWS exist for (org,
// period). Row count, not net: an idempotency bug that posted +100 then -100
// then +100 would leave the net correct and is only visible as extra rows.
func countOrgSpendRows(t *testing.T, db *DB, org, period string) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow(`
		SELECT COUNT(*) FROM org_actual_spend WHERE org = ? AND period = ?`, org, period).Scan(&n); err != nil {
		t.Fatalf("count rows(%s, %s): %v", org, period, err)
	}
	return n
}

// TestReconcileSubscriptionFee_IdempotentDeltaPost pins the #113 fee mechanics
// end to end: the first pass posts the whole fee, a re-run posts NOTHING
// (restart safety — the property #155's backfill leans on), a fee change posts
// exactly the difference in each direction, and a new period posts the full fee
// again without disturbing the previous one. Every assertion is in integer
// micro-dollars; no float ever touches the ledger.
func TestReconcileSubscriptionFee_IdempotentDeltaPost(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	const route = "glm-5.2@ollama.com"
	const fee = int64(100_000_000) // $100.00/month

	delta, err := db.ReconcileSubscriptionFee(ctx, route, "acme", "2026-07", fee)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if delta != fee {
		t.Errorf("first delta = %d, want %d (the full fee)", delta, fee)
	}
	assertSubscriptionNet(t, db, route, "acme", "2026-07", fee)
	if n := countOrgSpendRows(t, db, "acme", "2026-07"); n != 1 {
		t.Errorf("rows after first post = %d, want 1", n)
	}

	// Re-run: a no-op. A restart, a second startup pass, and an hourly tick all
	// land here, so this is THE idempotency assertion.
	delta, err = db.ReconcileSubscriptionFee(ctx, route, "acme", "2026-07", fee)
	if err != nil {
		t.Fatalf("reconcile rerun: %v", err)
	}
	if delta != 0 {
		t.Errorf("rerun delta = %d, want 0", delta)
	}
	assertSubscriptionNet(t, db, route, "acme", "2026-07", fee)
	if n := countOrgSpendRows(t, db, "acme", "2026-07"); n != 1 {
		t.Errorf("rows after rerun = %d, want 1 — a re-run must write no row at all", n)
	}

	// Fee raised to $120 mid-month: exactly the +$20 difference, never the full
	// new fee on top of the old one.
	delta, err = db.ReconcileSubscriptionFee(ctx, route, "acme", "2026-07", 120_000_000)
	if err != nil {
		t.Fatalf("reconcile raise: %v", err)
	}
	if delta != 20_000_000 {
		t.Errorf("raise delta = %d, want 20000000", delta)
	}
	assertSubscriptionNet(t, db, route, "acme", "2026-07", 120_000_000)

	// Fee cut to $110: a -$10 credit memo. Negative deltas are legal under the
	// #24 accumulation model and are how a downgrade corrects a posted month.
	delta, err = db.ReconcileSubscriptionFee(ctx, route, "acme", "2026-07", 110_000_000)
	if err != nil {
		t.Fatalf("reconcile cut: %v", err)
	}
	if delta != -10_000_000 {
		t.Errorf("cut delta = %d, want -10000000", delta)
	}
	assertSubscriptionNet(t, db, route, "acme", "2026-07", 110_000_000)

	// Month rollover: the full fee posts again for the new period and the prior
	// period's net is untouched.
	delta, err = db.ReconcileSubscriptionFee(ctx, route, "acme", "2026-08", fee)
	if err != nil {
		t.Fatalf("reconcile new period: %v", err)
	}
	if delta != fee {
		t.Errorf("new-period delta = %d, want %d", delta, fee)
	}
	assertSubscriptionNet(t, db, route, "acme", "2026-08", fee)
	assertSubscriptionNet(t, db, route, "acme", "2026-07", 110_000_000)
}

// TestReconcileSubscriptionFee_PreservesForeignRows is the reason the reconcile
// is SOURCE-SCOPED (#138 review R1). Finance posts invoices under the same org
// by hand, and provider pollers post under their own tags; converging the (org,
// period) cross-source NET to the fee would silently offset all of them — the
// exact hazard the abandoned branch built a whole ledger table to dodge before
// org_actual_spend.source existed.
func TestReconcileSubscriptionFee_PreservesForeignRows(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	const route = "glm-5.2@ollama.com"
	const fee = int64(100_000_000)

	// A manual finance invoice for $50 under the same (org, period).
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "acme", Period: "2026-07", ActualPaidMicro: 50_000_000,
		Source: OrgSpendSourceManual, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend: %v", err)
	}

	delta, err := db.ReconcileSubscriptionFee(ctx, route, "acme", "2026-07", fee)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if delta != fee {
		t.Errorf("delta = %d, want %d — a foreign row must not shrink the fee post", delta, fee)
	}
	// The org's real total is both facts, not one netted against the other.
	assertOrgNetAllSources(t, db, "acme", "2026-07", 150_000_000)

	delta, err = db.ReconcileSubscriptionFee(ctx, route, "acme", "2026-07", fee)
	if err != nil {
		t.Fatalf("reconcile rerun: %v", err)
	}
	if delta != 0 {
		t.Errorf("rerun delta = %d, want 0", delta)
	}
	assertOrgNetAllSources(t, db, "acme", "2026-07", 150_000_000)
}

// TestReconcileSubscriptionFee_RoutesDoNotNetAgainstEachOther is the guard on
// SubscriptionSpendSource embedding the route prefix. Fold two routes into one
// flat 'subscription' tag and each reads the OTHER's posting as its own memory:
// route B would see route A's $100 already posted, post $0, and the org would be
// billed for one plan while paying for two.
func TestReconcileSubscriptionFee_RoutesDoNotNetAgainstEachOther(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := db.ReconcileSubscriptionFee(ctx, "glm-5.2@ollama.com", "acme", "2026-07", 100_000_000); err != nil {
		t.Fatalf("reconcile route A: %v", err)
	}
	delta, err := db.ReconcileSubscriptionFee(ctx, "kimi-2@moonshot.example", "acme", "2026-07", 40_000_000)
	if err != nil {
		t.Fatalf("reconcile route B: %v", err)
	}
	if delta != 40_000_000 {
		t.Errorf("route B delta = %d, want 40000000 — route A's posting must not be read as route B's", delta)
	}
	assertSubscriptionNet(t, db, "glm-5.2@ollama.com", "acme", "2026-07", 100_000_000)
	assertSubscriptionNet(t, db, "kimi-2@moonshot.example", "acme", "2026-07", 40_000_000)
	assertOrgNetAllSources(t, db, "acme", "2026-07", 140_000_000)
}

// TestReconcileSubscriptionFee_RejectsBadArguments pins the fail-loud argument
// guards. A malformed period is caught HERE rather than by the
// org_actual_spend.period CHECK constraint, so the error names the caller's
// mistake; an empty org would write money under a key no allocation read
// queries.
func TestReconcileSubscriptionFee_RejectsBadArguments(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	cases := []struct {
		name                     string
		route, org, period, want string
	}{
		{"empty route", "", "acme", "2026-07", "route prefix is required"},
		{"empty org", "glm@h", "", "2026-07", "org is required"},
		{"period with day", "glm@h", "acme", "2026-07-01", "must be YYYY-MM"},
		{"single-digit month", "glm@h", "acme", "2026-7", "must be YYYY-MM"},
		{"month 13", "glm@h", "acme", "2026-13", "must be YYYY-MM"},
		{"empty period", "glm@h", "acme", "", "must be YYYY-MM"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ReconcileSubscriptionFee(ctx, tc.route, tc.org, tc.period, 1)
			if err == nil {
				t.Fatalf("ReconcileSubscriptionFee(%q, %q, %q): want error, got nil", tc.route, tc.org, tc.period)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestSubscriptionSpendSource pins the tag format both halves of the reconcile
// share. A drift between the read scope and the write tag would make every pass
// re-post the full fee forever, so the format is asserted rather than assumed.
func TestSubscriptionSpendSource(t *testing.T) {
	if got, want := SubscriptionSpendSource("glm-5.2@ollama.com"), "subscription:glm-5.2@ollama.com"; got != want {
		t.Errorf("SubscriptionSpendSource = %q, want %q", got, want)
	}
	if SubscriptionSpendSource("a") == SubscriptionSpendSource("b") {
		t.Error("two route prefixes produced the same source tag — they would net against each other")
	}
	if SubscriptionSpendSource("a") == OrgSpendSourceManual {
		t.Error("subscription source collides with the manual finance tag")
	}
}

// TestPostSubscriptionFeeIfUnposted_FillsOnceAndNeverRestates is the guard on
// #155's actual wording — "every period the plan was active and UNPOSTED".
//
// The distinction it pins is a money-correctness one, not a style one: an org
// that paid $100 in June paid $100 in June, and an August config edit must not
// silently rewrite that. The converging form (ReconcileSubscriptionFee) WOULD
// rewrite it, which is exactly why closed periods route here instead.
func TestPostSubscriptionFeeIfUnposted_FillsOnceAndNeverRestates(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	const route = "glm-5.2@ollama.com"

	posted, err := db.PostSubscriptionFeeIfUnposted(ctx, route, "acme", "2026-06", 100_000_000)
	if err != nil {
		t.Fatalf("first fill: %v", err)
	}
	if posted != 100_000_000 {
		t.Errorf("first fill posted %d, want 100000000 (the full fee — no proration)", posted)
	}

	// Re-run with the SAME fee: nothing.
	if posted, err = db.PostSubscriptionFeeIfUnposted(ctx, route, "acme", "2026-06", 100_000_000); err != nil || posted != 0 {
		t.Fatalf("re-fill = (%d, %v), want (0, nil)", posted, err)
	}

	// 🔴 The one that matters: the fee is later RAISED to $120 and the startup
	// catch-up runs again. June must still read $100.
	if posted, err = db.PostSubscriptionFeeIfUnposted(ctx, route, "acme", "2026-06", 120_000_000); err != nil || posted != 0 {
		t.Fatalf("fill after a fee raise = (%d, %v), want (0, nil)", posted, err)
	}
	assertSubscriptionNet(t, db, route, "acme", "2026-06", 100_000_000)
	if n := countOrgSpendRows(t, db, "acme", "2026-06"); n != 1 {
		t.Errorf("rows = %d, want 1 — a closed period must never be restated", n)
	}

	// CONTROL: the converging form on the SAME state DOES restate. Without this
	// the test above would pass against a store where neither method ever wrote
	// anything after the first call.
	delta, err := db.ReconcileSubscriptionFee(ctx, route, "acme", "2026-06", 120_000_000)
	if err != nil {
		t.Fatalf("converge control: %v", err)
	}
	if delta != 20_000_000 {
		t.Errorf("converge control delta = %d, want 20000000 — the two methods must differ, or the split is decorative", delta)
	}
}

// TestPostSubscriptionFeeIfUnposted_ZeroNetCountsAsPosted pins that "unposted"
// means NO ROW, not a zero net. A period whose fee was posted and then fully
// credited by hand has been DECIDED; re-posting the fee there would overrule a
// human correction on the next boot — the same class of error as restatement.
func TestPostSubscriptionFeeIfUnposted_ZeroNetCountsAsPosted(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	const route = "glm-5.2@ollama.com"

	if _, err := db.PostSubscriptionFeeIfUnposted(ctx, route, "acme", "2026-06", 100_000_000); err != nil {
		t.Fatalf("fill: %v", err)
	}
	// A human credits it back to zero (e.g. the plan was cancelled retroactively).
	if _, err := db.ReconcileSubscriptionFee(ctx, route, "acme", "2026-06", 0); err != nil {
		t.Fatalf("credit: %v", err)
	}
	assertSubscriptionNet(t, db, route, "acme", "2026-06", 0)

	if posted, err := db.PostSubscriptionFeeIfUnposted(ctx, route, "acme", "2026-06", 100_000_000); err != nil || posted != 0 {
		t.Fatalf("fill over a zeroed period = (%d, %v), want (0, nil) — a deliberate credit must not be undone", posted, err)
	}
	assertSubscriptionNet(t, db, route, "acme", "2026-06", 0)
}

// TestPostSubscriptionFeeIfUnposted_RejectsBadArguments mirrors the converging
// form's argument guards — the backfill path must not be the loose one.
func TestPostSubscriptionFeeIfUnposted_RejectsBadArguments(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cases := []struct{ name, route, org, period, want string }{
		{"empty route", "", "acme", "2026-07", "route prefix is required"},
		{"empty org", "glm@h", "", "2026-07", "org is required"},
		{"single-digit month", "glm@h", "acme", "2026-7", "must be YYYY-MM"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.PostSubscriptionFeeIfUnposted(ctx, tc.route, tc.org, tc.period, 1); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestPostSubscriptionFeeIfUnposted_RejectsNonPositiveFee pins the asymmetry
// between the two methods that is easiest to "tidy up" into a bug.
//
// FILLING a period with a zero fee writes a row, and because "unposted" means NO
// ROW, that row marks the period permanently covered — while the 0 return is
// indistinguishable from "already covered", so the caller logs nothing. No later
// correction can repair it. config.minMonthlyFeeUSD is the entry-point guard;
// this is the store-side belt for a caller that skips config.
//
// CONVERGING to zero is the opposite: a legitimate, deliberate act (a
// retroactively cancelled plan credited back to nil, pinned by
// TestPostSubscriptionFeeIfUnposted_ZeroNetCountsAsPosted). The control arm below
// exists so that mirroring this guard onto ReconcileSubscriptionFee — the obvious
// symmetry — fails loudly instead of quietly outlawing a real operation.
func TestPostSubscriptionFeeIfUnposted_RejectsNonPositiveFee(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	const route = "glm-5.2@ollama.com"

	for _, feeMicro := range []int64{0, -1, -100_000_000} {
		t.Run(fmt.Sprintf("fee=%d", feeMicro), func(t *testing.T) {
			posted, err := db.PostSubscriptionFeeIfUnposted(ctx, route, "acme", "2026-06", feeMicro)
			if err == nil {
				t.Fatalf("backfill accepted feeMicro=%d (posted %d) — it would mark 2026-06 permanently covered", feeMicro, posted)
			}
			if !strings.Contains(err.Error(), "fee must be > 0") {
				t.Errorf("error = %v, want it to name the fee bound", err)
			}
			// And it must have written NOTHING, or the rejection is cosmetic.
			if n := countOrgSpendRows(t, db, "acme", "2026-06"); n != 0 {
				t.Errorf("rejected backfill still wrote %d row(s)", n)
			}
		})
	}

	// CONTROL ARM: converging to zero remains legal on the OTHER method.
	if _, err := db.ReconcileSubscriptionFee(ctx, route, "acme", "2026-06", 100_000_000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if delta, err := db.ReconcileSubscriptionFee(ctx, route, "acme", "2026-06", 0); err != nil || delta != -100_000_000 {
		t.Errorf("converge-to-zero = (%d, %v), want (-100000000, nil) — cancelling a plan must stay legal", delta, err)
	}
	assertSubscriptionNet(t, db, route, "acme", "2026-06", 0)
}

// TestSubscriptionFee_FractionalDollarsSurviveTheRoundTrip pins the money
// boundary at the seam that actually stores it. Every other subscription test
// uses whole dollars, which cannot distinguish a correct micro-dollar
// conversion from one that truncates or rounds — a $19.99 plan would then read
// as $19 or $20 in a finance export, and re-post a delta forever because the
// stored value never equals the converted fee.
func TestSubscriptionFee_FractionalDollarsSurviveTheRoundTrip(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	const route = "glm-5.2@ollama.com"
	feeMicro := DollarsToMicro(19.99)
	if feeMicro != 19_990_000 {
		t.Fatalf("DollarsToMicro(19.99) = %d, want 19990000", feeMicro)
	}

	if delta, err := db.ReconcileSubscriptionFee(ctx, route, "acme", "2026-07", feeMicro); err != nil || delta != feeMicro {
		t.Fatalf("first post = (%d, %v), want (%d, nil)", delta, err, feeMicro)
	}
	assertSubscriptionNet(t, db, route, "acme", "2026-07", 19_990_000)
	// The re-run is the real assertion: it only deltas to 0 if what was STORED
	// equals what DollarsToMicro produces, exactly.
	if delta, err := db.ReconcileSubscriptionFee(ctx, route, "acme", "2026-07", feeMicro); err != nil || delta != 0 {
		t.Errorf("re-run = (%d, %v), want (0, nil) — a fractional fee must be idempotent too", delta, err)
	}
}

// TestCountActiveSeatsMirrorsAllocation pins the CLAIM in CountActiveSeats's
// doc: its predicate is the `active` CTE in actualSpendCTE, restated.
//
// The warning it feeds says "this fee will not reach Spend Leverage". That is a
// claim about a DIFFERENT query, so asserting it against a copy of its own SQL
// would prove nothing — the two are compared against each other here. Drift in
// either direction is a lie: a loose copy stays silent while money vanishes, a
// tight one accuses a correctly-configured operator.
//
// The membership windows are chosen so each arm of the CTE's predicate has a
// case that turns on it: joined-later, departed-earlier, still-open, a duplicate
// row for one developer (seats are DISTINCT developers), and a different org.
func TestCountActiveSeatsMirrorsAllocation(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	const period = "2026-06"

	seedMembership(t, db, "alice", "acme", "2026-01", "")        // open — active
	seedMembership(t, db, "alice", "acme", "2026-02", "2026-09") // DUPLICATE developer, still one seat
	seedMembership(t, db, "bob", "acme", "2026-01", "2026-05")   // departed BEFORE the period
	seedMembership(t, db, "carol", "acme", "2026-07", "")        // joined AFTER the period
	seedMembership(t, db, "dave", "acme", "2026-06", "2026-06")  // exactly this period — active
	seedMembership(t, db, "erin", "other", "2026-01", "")        // a different org entirely

	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "acme", Period: period, ActualPaidMicro: 100_000_000,
		Source: OrgSpendSourceManual, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed org spend: %v", err)
	}

	// The allocation read is the authority: a developer is a seat iff the org
	// fallback allocates them a slice of the org invoice.
	since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	allocated := map[string]bool{}
	for _, dev := range []string{"alice", "bob", "carol", "dave", "erin"} {
		got, err := db.ActualSpendForDeveloper(ctx, dev, since)
		if err != nil {
			t.Fatalf("ActualSpendForDeveloper(%s): %v", dev, err)
		}
		allocated[dev] = got > 0
	}
	wantSeats := 0
	for _, ok := range allocated {
		if ok {
			wantSeats++
		}
	}
	// CONTROL: the fixture must actually separate the two groups, or "they agree"
	// is vacuous.
	if wantSeats == 0 || wantSeats == len(allocated) {
		t.Fatalf("fixture allocates to %d of %d developers — it is not exercising the predicate: %v", wantSeats, len(allocated), allocated)
	}
	if !allocated["alice"] || !allocated["dave"] || allocated["bob"] || allocated["carol"] || allocated["erin"] {
		t.Fatalf("the allocation read itself changed shape: %v", allocated)
	}

	got, err := db.CountActiveSeats(ctx, "acme", period)
	if err != nil {
		t.Fatalf("CountActiveSeats: %v", err)
	}
	if got != wantSeats {
		t.Errorf("CountActiveSeats(acme, %s) = %d, but the allocation read reaches %d developer(s) (%v) — the two predicates have drifted", period, got, wantSeats, allocated)
	}

	// The zero case is the one the WARN fires on: an org key nobody is mapped to.
	if got, err := db.CountActiveSeats(ctx, "typo-org", period); err != nil || got != 0 {
		t.Errorf("CountActiveSeats(typo-org) = (%d, %v), want (0, nil)", got, err)
	}
	// And a malformed period is a caller error, not a silent 0.
	if _, err := db.CountActiveSeats(ctx, "acme", "2026-6"); err == nil {
		t.Error("CountActiveSeats accepted a malformed period; a silent 0 would read as 'no seats'")
	}
}

// TestPeriodRange covers the #155 backfill list: inclusive on both ends, across
// a year boundary, and fail-loud on the two shapes that would silently produce
// an EMPTY range — a malformed bound and an inverted one. An empty range is the
// dangerous failure here: it looks exactly like "nothing to back-fill".
func TestPeriodRange(t *testing.T) {
	cases := []struct {
		name, from, to string
		want           []string
		wantErr        string
	}{
		{name: "single period", from: "2026-08", to: "2026-08", want: []string{"2026-08"}},
		{name: "three periods", from: "2026-06", to: "2026-08", want: []string{"2026-06", "2026-07", "2026-08"}},
		{name: "year boundary", from: "2025-11", to: "2026-02", want: []string{"2025-11", "2025-12", "2026-01", "2026-02"}},
		{name: "inverted", from: "2026-08", to: "2026-06", wantErr: "inverted"},
		{name: "malformed from", from: "2026-6", to: "2026-08", wantErr: "must be YYYY-MM"},
		{name: "malformed to", from: "2026-06", to: "26-08", wantErr: "must be YYYY-MM"},
		{name: "month 00", from: "2026-00", to: "2026-08", wantErr: "must be YYYY-MM"},
		// Bounded HERE, not only in config: a caller that skips config validation
		// would otherwise materialise ~21,000 periods from a typo'd year and issue
		// one transaction for each.
		{name: "absurd range", from: "0226-06", to: "2026-08", wantErr: "exceeds 240 periods"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PeriodRange(tc.from, tc.to)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("PeriodRange(%q, %q) = %v, want error %q", tc.from, tc.to, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("PeriodRange(%q, %q): %v", tc.from, tc.to, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("PeriodRange(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// TestPeriodRange_BoundIsExactlyMaxPeriodRange pins the OFF-BY-ONE the range
// test above cannot see. Its "absurd range" case is ~21,000 periods, which every
// plausible spelling of the bound rejects — so `len(out) >= MaxPeriodRange`
// silently becoming `>` compiles, passes that case, and quietly permits 241.
//
// A bound that is only ever exercised far from its edge is not pinned, it is
// decorated. This asserts the edge itself, from both sides, and derives the
// dates from MaxPeriodRange rather than hard-coding 240 — so raising the
// constant moves the test with it instead of breaking it.
func TestPeriodRange_BoundIsExactlyMaxPeriodRange(t *testing.T) {
	// addMonths walks from a fixed epoch so the arithmetic is independent of the
	// wall clock and of PeriodRange itself.
	addMonths := func(n int) string {
		return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, n, 0).Format("2006-01")
	}
	const from = "2000-01"

	atLimit := addMonths(MaxPeriodRange - 1) // inclusive count == MaxPeriodRange
	got, err := PeriodRange(from, atLimit)
	if err != nil {
		t.Fatalf("PeriodRange(%s..%s) = %v, want a %d-period range accepted at the limit", from, atLimit, err, MaxPeriodRange)
	}
	if len(got) != MaxPeriodRange {
		t.Fatalf("PeriodRange(%s..%s) returned %d periods, want exactly %d", from, atLimit, len(got), MaxPeriodRange)
	}

	overLimit := addMonths(MaxPeriodRange) // one more
	if got, err := PeriodRange(from, overLimit); err == nil {
		t.Errorf("PeriodRange(%s..%s) accepted %d periods, want a refusal at %d+1", from, overLimit, len(got), MaxPeriodRange)
	}
}

// TestValidPeriodAndCurrentPeriod pins the period shape against the SQL CHECK
// constraint it exists to pre-empt, and pins CurrentPeriod to UTC. A
// local-zone month boundary would post one org's fee under two different
// periods depending on where the server runs.
func TestValidPeriodAndCurrentPeriod(t *testing.T) {
	for _, ok := range []string{"2026-01", "2026-12", "1999-07"} {
		if !ValidPeriod(ok) {
			t.Errorf("ValidPeriod(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "2026-1", "2026-13", "2026-00", "2026-07-01", "202-07", "abcd-07"} {
		if ValidPeriod(bad) {
			t.Errorf("ValidPeriod(%q) = true, want false", bad)
		}
	}
	// 2026-01-01T00:30 in a +05:30 zone is still 2025-12-31 in UTC.
	zone := time.FixedZone("IST", 5*3600+1800)
	if got, want := CurrentPeriod(time.Date(2026, 1, 1, 0, 30, 0, 0, zone)), "2025-12"; got != want {
		t.Errorf("CurrentPeriod = %q, want %q (periods are UTC months)", got, want)
	}
}

// subscriptionTestTableYAML is a minimal valid price table carrying one
// subscription route, one per-token route on the SAME model at a different
// host, and the three required self-hosted fallbacks.
const subscriptionTestTableYAML = `
version: 1
effective_date: "2026-08-01"
models:
  "glm-5.2@ollama.com": { input_per_m: 0.875, output_per_m: 7.00, provider: self-hosted, billing_mode: subscription }
  "glm-5.2@openrouter.ai": { input_per_m: 0.60, output_per_m: 2.20, provider: self-hosted, billing_mode: per_token }
  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}
  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}
  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}
`

// loadDefaultPriceTable pins the EMBEDDED default table active for the calling
// test and restores it on cleanup — the internal/store twin of cmd/tierd's
// loadEmbeddedPriceTable (#313). restoreDefaultPriceTable alone only registers
// the cleanup; a test that asserts about the default must also SET it, or it is
// really asserting about whatever ran before it.
func loadDefaultPriceTable(t *testing.T) {
	t.Helper()
	tbl, info, err := parsePriceTable(defaultPriceTableYAML)
	if err != nil {
		t.Fatalf("pin default price table: %v", err)
	}
	priceTable, activePriceTableInfo = tbl, info
	restoreDefaultPriceTable(t)
}

// loadSubscriptionTestTable installs subscriptionTestTableYAML as the active
// table for the calling test, restoring the embedded default afterwards.
func loadSubscriptionTestTable(t *testing.T) {
	t.Helper()
	restoreDefaultPriceTable(t)
	tbl, info, err := parsePriceTable([]byte(subscriptionTestTableYAML))
	if err != nil {
		t.Fatalf("parse subscription test table: %v", err)
	}
	priceTable = tbl
	activePriceTableInfo = info
}

// TestSubscriptionRoutes_OnlySubscriptionModeEntries is the TABLE half of the
// two-artifacts-one-truth gate. It must report subscription entries and ONLY
// subscription entries: a per-token row leaking in would make the coverage gate
// accept a fee for a metered route, which is exactly what the gate exists to
// refuse.
func TestSubscriptionRoutes_OnlySubscriptionModeEntries(t *testing.T) {
	loadSubscriptionTestTable(t)

	got := SubscriptionRoutes()
	want := []string{"glm-5.2@ollama.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SubscriptionRoutes() = %v, want %v (the per-token and self-hosted rows must not appear)", got, want)
	}
	// The operator-facing summary must carry the RATES (#154 item 3) — the only
	// part of a comparable valuation a reader can actually audit — and must say
	// plainly that they are not a metered price.
	summary := SubscriptionRouteSummary()
	if len(summary) != 1 {
		t.Fatalf("SubscriptionRouteSummary() = %v, want one entry", summary)
	}
	for _, want := range []string{"glm-5.2@ollama.com", "in $0.875/M", "out $7/M", "not a metered price"} {
		if !strings.Contains(summary[0], want) {
			t.Errorf("summary %q omits %q", summary[0], want)
		}
	}
	if len(SubscriptionRoutesWithPrefix("glm-5.2@ollama.com")) != 1 {
		t.Error("exact-key prefix matched nothing")
	}
	if len(SubscriptionRoutesWithPrefix("glm-")) != 1 {
		t.Error("family prefix matched nothing")
	}
	// A prefix that only matches the PER-TOKEN row must match nothing here.
	if got := SubscriptionRoutesWithPrefix("glm-5.2@openrouter"); len(got) != 0 {
		t.Errorf("SubscriptionRoutesWithPrefix(per-token host) = %v, want empty", got)
	}
	if got := SubscriptionRoutesWithPrefix("nope"); len(got) != 0 {
		t.Errorf("SubscriptionRoutesWithPrefix(unmatched) = %v, want empty", got)
	}
}

// TestSubscriptionRoutes_EmbeddedDefaultSeedsNone pins a DELIBERATE data
// decision: no `billing_mode: subscription` row ships in the embedded price
// table. A subscription rate is a comparable-peer VALUATION, not a published
// per-token price — shipping one would state a $/M figure for a real vendor
// that no vendor publishes, to every TIER install. Operators declare their own
// via --prices (docs/open-weights-capture.md §5).
//
// If you are here because you just seeded one: that is a decision, not a test
// fix. Verify the host's echoed model id and the comparable peer you priced it
// against on a live first-party page first, then update this test.
func TestSubscriptionRoutes_EmbeddedDefaultSeedsNone(t *testing.T) {
	// PIN the embedded default active rather than asserting on whatever the
	// package-global happens to hold. Every other test here that swaps the table
	// restores it — but this assertion's correctness would then depend on a
	// property no test enforces, and it is the one test in this file that reads
	// the global without setting it.
	loadDefaultPriceTable(t)
	if got := SubscriptionRoutes(); len(got) != 0 {
		t.Errorf("embedded price table ships subscription routes %v, want none", got)
	}
}
