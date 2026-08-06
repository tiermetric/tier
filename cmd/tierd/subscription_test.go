package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/config"
	"github.com/tiermetric/tier/internal/store"
)

// quietLogger discards output for tests that assert on the store, not the log.
func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// capturingLogger returns a logger at the given level plus the buffer it writes
// to, for the tests that assert on operator-facing output (the per-period
// backfill INFO line, the uncovered-route WARN).
func capturingLogger(level slog.Level) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})), &buf
}

// openTestDB opens a real store in a temp dir. The reconciler's whole contract
// is idempotency against SQL state, so a fake store cannot prove it — these
// tests run against the real schema and the real CHECK constraints.
func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// subscriptionRows returns (period → net micro-dollars) for one route's own
// source tag, plus the total number of org_actual_spend rows written under it.
// The ROW COUNT is what makes a double-post visible: a bug that posted +$100
// then -$100 then +$100 leaves a correct net behind three rows.
func subscriptionRows(t *testing.T, db *store.DB, routePrefix, org string, periods []string) (map[string]int64, int) {
	t.Helper()
	nets := make(map[string]int64, len(periods))
	rows := 0
	ctx := context.Background()
	for _, p := range periods {
		net, err := db.OrgActualSpendNet(ctx, org, p, store.SubscriptionSpendSource(routePrefix))
		if err != nil {
			t.Fatalf("OrgActualSpendNet(%s): %v", p, err)
		}
		if net != 0 {
			nets[p] = net
		}
	}
	// since is the epoch so the roll-up covers every period, including the
	// backfilled ones — a `since` anchored on now would hide exactly the rows
	// these tests exist to count.
	totals, err := db.OrgActualSpendTotals(ctx, time.Unix(0, 0), org)
	if err != nil {
		t.Fatalf("OrgActualSpendTotals: %v", err)
	}
	for _, tot := range totals {
		rows += tot.Entries
	}
	return nets, rows
}

// monthsBack returns the period n calendar months before the current UTC one,
// so a test states a RELATIONSHIP ("two months back") rather than a date that
// rots.
//
// 🔴 It steps from the FIRST of the month, not from today. time.AddDate
// normalizes an overflowing day-of-month, so `now.AddDate(0,-1,0)` on 31 March
// yields 31 February → 3 MARCH — the same period it started from. That collapse
// happens on 22 days of 2026–2027 and would make the backfill tests either red
// (a "previous" period equal to the current one contradicts itself) or, worse,
// VACUOUSLY GREEN (a three-period range silently covering two). Anchoring at
// day 1 makes the month arithmetic exact for every date.
func monthsBack(n int) string {
	now := time.Now().UTC()
	firstOfMonth := time.Date(now.Year(), now.Month(), 1, 12, 0, 0, 0, time.UTC)
	return store.CurrentPeriod(firstOfMonth.AddDate(0, -n, 0))
}

// requireDistinctPeriods fails unless the given periods are pairwise distinct.
// Every backfill test below is built on "N successive months are N different
// months"; if monthsBack ever regresses to the AddDate-overflow behaviour, this
// says so in one line instead of letting the tests degrade into passing while
// covering fewer periods than they claim.
func requireDistinctPeriods(t *testing.T, periods ...string) {
	t.Helper()
	seen := map[string]bool{}
	for _, p := range periods {
		if seen[p] {
			t.Fatalf("periods %v are not distinct — monthsBack collapsed (see its doc comment); every backfill assertion below would be measuring fewer months than it claims", periods)
		}
		seen[p] = true
	}
}

// TestReconcileSubscriptionFeesOnce_BackfillsEachPeriodExactlyOnce is #155's
// acceptance criterion at the seam that implements it: a startup pass with
// active_since two months back posts THREE period rows, and a second pass posts
// NONE. The second pass is the whole point — a backfill that is not idempotent
// re-bills the org's actual-paid total at every boot.
func TestReconcileSubscriptionFeesOnce_BackfillsEachPeriodExactlyOnce(t *testing.T) {
	db := openTestDB(t)
	// FIXED periods, passed in as `current` — no wall clock anywhere in this test.
	// The seam exists precisely so the month arithmetic under test is stated, not
	// derived from time.Now() twice and hoped to agree.
	const prev2, prev1, current = "2026-06", "2026-07", "2026-08"
	subs := []config.Subscription{{
		RoutePrefix: "glm-5.2@ollama.com", Org: "acme", MonthlyFeeUSD: 100, ActiveSince: prev2,
	}}
	all := []string{prev2, prev1, current}

	logger, buf := capturingLogger(slog.LevelInfo)
	reconcileSubscriptionFeesOnce(context.Background(), db, subs, logger, current, true)

	nets, rows := subscriptionRows(t, db, "glm-5.2@ollama.com", "acme", all)
	if rows != 3 {
		t.Errorf("org_actual_spend rows after the first pass = %d, want 3 (one per active period)", rows)
	}
	for _, p := range all {
		if nets[p] != 100_000_000 {
			t.Errorf("net(%s) = %d micro, want 100000000 (the full fee — no proration)", p, nets[p])
		}
	}
	// One INFO line per period actually posted (#155 item 4).
	for _, p := range all {
		if !strings.Contains(buf.String(), "period="+p) {
			t.Errorf("startup log does not name the caught-up period %s; got:\n%s", p, buf.String())
		}
	}

	// SECOND PASS — the idempotency arm. Nothing may be posted and nothing logged.
	logger2, buf2 := capturingLogger(slog.LevelInfo)
	reconcileSubscriptionFeesOnce(context.Background(), db, subs, logger2, current, true)

	nets2, rows2 := subscriptionRows(t, db, "glm-5.2@ollama.com", "acme", all)
	if rows2 != 3 {
		t.Errorf("rows after the second pass = %d, want 3 — a re-run must post nothing", rows2)
	}
	for _, p := range all {
		if nets2[p] != 100_000_000 {
			t.Errorf("net(%s) after re-run = %d micro, want 100000000 (unchanged)", p, nets2[p])
		}
	}
	if strings.Contains(buf2.String(), "subscription fee posted") {
		t.Errorf("second pass logged a posting; want silence. got:\n%s", buf2.String())
	}
}

// TestReconcileSubscriptionFeesOnce_FeeChangeDoesNotRestateClosedPeriods is the
// money-correctness assertion behind the two-method split, at the seam that
// chooses between them.
//
// An org that paid $100 in June paid $100 in June. Raising monthly_fee_usd to
// $120 in August and restarting must move ONLY August; a converging backfill
// would silently rewrite June and July too, and every historical Spend Leverage
// figure with them, leaving no record that anything changed.
func TestReconcileSubscriptionFeesOnce_FeeChangeDoesNotRestateClosedPeriods(t *testing.T) {
	db := openTestDB(t)
	const prev2, prev1, current = "2026-06", "2026-07", "2026-08"
	all := []string{prev2, prev1, current}
	sub := config.Subscription{RoutePrefix: "glm-5.2@ollama.com", Org: "acme", MonthlyFeeUSD: 100, ActiveSince: prev2}

	reconcileSubscriptionFeesOnce(context.Background(), db, []config.Subscription{sub}, quietLogger(), current, true)
	nets, _ := subscriptionRows(t, db, "glm-5.2@ollama.com", "acme", all)
	for _, p := range all {
		if nets[p] != 100_000_000 {
			t.Fatalf("setup: net(%s) = %d, want 100000000", p, nets[p])
		}
	}

	// The plan is upgraded to $120 and tierd restarts — the startup pass runs again.
	sub.MonthlyFeeUSD = 120
	reconcileSubscriptionFeesOnce(context.Background(), db, []config.Subscription{sub}, quietLogger(), current, true)

	nets2, rows := subscriptionRows(t, db, "glm-5.2@ollama.com", "acme", all)
	for _, p := range []string{prev2, prev1} {
		if nets2[p] != 100_000_000 {
			t.Errorf("net(%s) = %d after the raise, want 100000000 — a CLOSED period must never be restated by a later config edit", p, nets2[p])
		}
	}
	// CONTROL ARM: the CURRENT period does converge, so this is a targeted split
	// and not simply "the second pass writes nothing".
	if nets2[current] != 120_000_000 {
		t.Errorf("net(%s) = %d after the raise, want 120000000 — the current period must follow the live config", current, nets2[current])
	}
	if rows != 4 {
		t.Errorf("rows = %d, want 4 (three fills + one current-period delta)", rows)
	}
}

// TestReconcileSubscriptionFeesOnce_TickTouchesOnlyCurrentPeriod pins #155 item
// 3. The steady-state tick must NOT re-walk history: with the same active_since
// as the test above, a non-backfill pass may post the current period only.
//
// The distinction is load-bearing rather than cosmetic — the catch-up is a
// one-shot, and a tick that repeated it would issue N transactions an hour
// forever to rediscover that every past month is already posted.
func TestReconcileSubscriptionFeesOnce_TickTouchesOnlyCurrentPeriod(t *testing.T) {
	db := openTestDB(t)
	const prev2, prev1, current = "2026-06", "2026-07", "2026-08"
	subs := []config.Subscription{{
		RoutePrefix: "glm-5.2@ollama.com", Org: "acme", MonthlyFeeUSD: 100, ActiveSince: prev2,
	}}

	reconcileSubscriptionFeesOnce(context.Background(), db, subs, quietLogger(), current, false)

	nets, rows := subscriptionRows(t, db, "glm-5.2@ollama.com", "acme", []string{prev2, prev1, current})
	if rows != 1 {
		t.Errorf("rows after a tick pass = %d, want 1 (current period only)", rows)
	}
	if nets[current] != 100_000_000 {
		t.Errorf("net(current=%s) = %d, want 100000000", current, nets[current])
	}
	for _, p := range []string{prev1, prev2} {
		if nets[p] != 0 {
			t.Errorf("net(%s) = %d, want 0 — the hourly tick must not backfill", p, nets[p])
		}
	}
}

// TestReconcileSubscriptionFeesOnce_NoActiveSinceIsCurrentPeriodOnly pins the
// DEFAULT: without active_since even the startup pass touches only the current
// period, so upgrading to this build cannot silently start posting fees for
// months the operator never declared the plan active.
func TestReconcileSubscriptionFeesOnce_NoActiveSinceIsCurrentPeriodOnly(t *testing.T) {
	db := openTestDB(t)
	const prev1, current = "2026-07", "2026-08"
	subs := []config.Subscription{{RoutePrefix: "glm-5.2@ollama.com", Org: "acme", MonthlyFeeUSD: 100}}

	reconcileSubscriptionFeesOnce(context.Background(), db, subs, quietLogger(), current, true)

	nets, rows := subscriptionRows(t, db, "glm-5.2@ollama.com", "acme", []string{prev1, current})
	if rows != 1 {
		t.Errorf("rows = %d, want 1 — absent active_since must not backfill", rows)
	}
	if nets[current] != 100_000_000 || nets[prev1] != 0 {
		t.Errorf("nets = %v, want only the current period (%s) posted", nets, current)
	}
}

// TestSubscriptionPeriods covers the range selection itself, including the
// inclusive current-period end that an off-by-one would drop — leaving the
// CURRENT month unposted, which is the one month everything else assumes is
// covered.
func TestSubscriptionPeriods(t *testing.T) {
	cases := []struct {
		name        string
		activeSince string
		current     string
		want        []string
		wantErr     bool
	}{
		{name: "absent", current: "2026-08", want: []string{"2026-08"}},
		{name: "same month", activeSince: "2026-08", current: "2026-08", want: []string{"2026-08"}},
		{name: "three months", activeSince: "2026-06", current: "2026-08", want: []string{"2026-06", "2026-07", "2026-08"}},
		{name: "across a year", activeSince: "2025-12", current: "2026-01", want: []string{"2025-12", "2026-01"}},
		{name: "inverted", activeSince: "2026-09", current: "2026-08", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := subscriptionPeriods(config.Subscription{ActiveSince: tc.activeSince}, tc.current)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("subscriptionPeriods = %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("subscriptionPeriods: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("subscriptionPeriods = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReconcileSubscriptionFeesOnce_SurvivesAStoreError pins the DELIBERATE
// ASYMMETRY the pollers established: a failed post is logged and the pass moves
// on to the next route. A fee-bookkeeping failure that aborted the pass would
// let one bad route silently stop every other route's fee from being posted.
func TestReconcileSubscriptionFeesOnce_SurvivesAStoreError(t *testing.T) {
	fake := &fakeSubscriptionStore{failOn: "bad@host"}
	subs := []config.Subscription{
		{RoutePrefix: "bad@host", Org: "acme", MonthlyFeeUSD: 10},
		{RoutePrefix: "good@host", Org: "acme", MonthlyFeeUSD: 20},
	}
	logger, buf := capturingLogger(slog.LevelInfo)

	reconcileSubscriptionFeesOnce(context.Background(), fake, subs, logger, "2026-08", false)

	if got := fake.calls(); len(got) != 2 {
		t.Errorf("reconcile calls = %v, want both routes attempted", got)
	}
	if !strings.Contains(buf.String(), "subscription fee reconcile failed") {
		t.Errorf("failure not logged; got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "good@host") {
		t.Errorf("the healthy route did not post after the failing one; got:\n%s", buf.String())
	}
}

// TestReconcileSubscriptionFeesOnce_StopsOnCancelledContext pins the shutdown
// behaviour of a LONG catch-up. Without the ctx check the loop would run every
// remaining period against a cancelled context, where each call fails fast and
// logs an ERROR — up to maxBackfillPeriods of them, which reads like a fault at
// exactly the moment an operator is looking for one.
func TestReconcileSubscriptionFeesOnce_StopsOnCancelledContext(t *testing.T) {
	subs := []config.Subscription{{
		RoutePrefix: "glm@h", Org: "acme", MonthlyFeeUSD: 1, ActiveSince: "2026-05",
	}}
	const current = "2026-08" // 4 periods: 2026-05..2026-08

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	stopped := &fakeSubscriptionStore{}
	logger, buf := capturingLogger(slog.LevelError)
	reconcileSubscriptionFeesOnce(cancelled, stopped, subs, logger, current, true)
	if got := stopped.calls(); len(got) != 0 {
		t.Errorf("cancelled pass made %d reconcile calls %v, want 0", len(got), got)
	}
	if buf.Len() != 0 {
		t.Errorf("cancelled pass logged errors instead of stopping quietly:\n%s", buf.String())
	}

	// CONTROL ARM: with a live context the SAME configuration walks all four
	// periods. Without this, a reconciler that never did anything would pass the
	// assertion above.
	live := &fakeSubscriptionStore{}
	reconcileSubscriptionFeesOnce(context.Background(), live, subs, quietLogger(), current, true)
	if got := live.calls(); len(got) != 4 {
		t.Errorf("live pass made %d reconcile calls %v, want 4 (active_since is 3 months back)", len(got), got)
	}
}

// TestReconcileSubscriptionFeesOnce_BackfillWarnsItUsedTodaysFee pins #113
// review Y3. First-run backfill fills a closed month at the CURRENTLY configured
// fee, because config carries one fee and no per-period history — so an operator
// adopting TIER with active_since set to the month their plan started gets every
// pre-raise month posted at today's price. That is the adoption path, not an edge
// case, and it silently moves those months' Spend Leverage.
//
// It cannot be computed correctly from the data available, so the requirement is
// that it be VISIBLE: WARN for a filled-in closed month, INFO for the current
// one. Two docs used to assert the opposite ("a closed month keeps what the org
// actually paid at the time"); they were corrected alongside this.
//
// MUTATION: collapse the WARN branch back into the single INFO line and this
// fails — both on the missing severity and on the missing correction path.
func TestReconcileSubscriptionFeesOnce_BackfillWarnsItUsedTodaysFee(t *testing.T) {
	fake := &fakeSubscriptionStore{seats: 1} // seats>0 so the Y4 warning stays out of the way
	subs := []config.Subscription{{
		RoutePrefix: "glm@h", Org: "acme", MonthlyFeeUSD: 200, ActiveSince: "2026-05",
	}}
	logger, buf := capturingLogger(slog.LevelInfo)

	reconcileSubscriptionFeesOnce(context.Background(), fake, subs, logger, "2026-08", true)
	out := buf.String()

	// Three closed months (2026-05..07) are BACKFILLED; each must warn.
	for _, closed := range []string{"2026-05", "2026-06", "2026-07"} {
		if !strings.Contains(out, `level=WARN`) || !strings.Contains(out, `period=`+closed) {
			t.Errorf("no line for backfilled period %s; got:\n%s", closed, out)
		}
	}
	if !strings.Contains(out, "BACKFILLED into a closed period at the CURRENTLY configured fee") {
		t.Errorf("the backfill line does not name the caveat; got:\n%s", out)
	}
	if !strings.Contains(out, "POST /api/v1/org_actual_spend") {
		t.Errorf("the backfill line does not say how to correct it; got:\n%s", out)
	}
	// The WARN must be a WARN. Re-running at WARN level must still show the
	// closed months — an INFO-level line would vanish here.
	warnOnly, warnBuf := capturingLogger(slog.LevelWarn)
	reconcileSubscriptionFeesOnce(context.Background(), &fakeSubscriptionStore{seats: 1}, subs, warnOnly, "2026-08", true)
	for _, closed := range []string{"2026-05", "2026-06", "2026-07"} {
		if !strings.Contains(warnBuf.String(), "period="+closed) {
			t.Errorf("backfilled period %s is not visible at WARN level — it is still INFO; got:\n%s", closed, warnBuf.String())
		}
	}
	// CONTROL ARM: the CURRENT period is a convergence, not a backfill, and must
	// stay at INFO. Without this a change that warned on everything would pass.
	if strings.Contains(warnBuf.String(), "period=2026-08") {
		t.Errorf("the CURRENT period warned; it converges to the live fee and must stay INFO:\n%s", warnBuf.String())
	}
	if !strings.Contains(out, "subscription fee posted to org_actual_spend") {
		t.Errorf("the current period lost its INFO line; got:\n%s", out)
	}
}

// TestReconcileSubscriptionFeesOnce_WarnsWhenTheOrgHasNoSeats pins #113 review
// Y4: a fee posted under an org key with no period_membership seats is recorded
// but unreachable. The allocation read reaches a developer only through
// `JOIN period_membership pm ON pm.org = inv.org`, so ActualPaidUSD stays 0 and
// Spend Leverage renders "—" while this reconciler logs a successful post every
// month, with nothing connecting the two.
//
// The startup coverage gate structurally cannot catch it (it runs before
// store.Open), which is why the check lives here, where there is a DB.
//
// MUTATION: delete the CountActiveSeats branch and the zero-seat subtest fails.
func TestReconcileSubscriptionFeesOnce_WarnsWhenTheOrgHasNoSeats(t *testing.T) {
	subs := []config.Subscription{{RoutePrefix: "glm@h", Org: "typo-org", MonthlyFeeUSD: 100}}
	const warning = "NO active developer seats"

	t.Run("no seats: warns and names the org key", func(t *testing.T) {
		fake := &fakeSubscriptionStore{seats: 0}
		logger, buf := capturingLogger(slog.LevelWarn)
		reconcileSubscriptionFeesOnce(context.Background(), fake, subs, logger, "2026-08", false)
		if !strings.Contains(buf.String(), warning) {
			t.Errorf("no unreachable-org WARN; got:\n%s", buf.String())
		}
		if !strings.Contains(buf.String(), "typo-org") {
			t.Errorf("the WARN does not name the org key an operator must fix; got:\n%s", buf.String())
		}
	})

	// CONTROL ARM: a healthy org must be silent, or the warning is noise an
	// operator learns to ignore — and the test above would pass on a reconciler
	// that warned unconditionally.
	t.Run("seats present: silent", func(t *testing.T) {
		fake := &fakeSubscriptionStore{seats: 3}
		logger, buf := capturingLogger(slog.LevelWarn)
		reconcileSubscriptionFeesOnce(context.Background(), fake, subs, logger, "2026-08", false)
		if strings.Contains(buf.String(), warning) {
			t.Errorf("warned about an org that HAS seats:\n%s", buf.String())
		}
	})

	// A seat lookup that ERRORS must not be read as "no seats" — that would turn
	// a transient DB error into a false accusation about the operator's config.
	t.Run("lookup error: reports the error, does not accuse", func(t *testing.T) {
		fake := &fakeSubscriptionStore{seatErr: fmt.Errorf("boom")}
		logger, buf := capturingLogger(slog.LevelWarn)
		reconcileSubscriptionFeesOnce(context.Background(), fake, subs, logger, "2026-08", false)
		if strings.Contains(buf.String(), warning) {
			t.Errorf("a failed seat lookup was reported as zero seats:\n%s", buf.String())
		}
		if !strings.Contains(buf.String(), "seat check failed") {
			t.Errorf("a failed seat lookup was swallowed:\n%s", buf.String())
		}
	})

	// The COST claim in the code comment: one extra SELECT per configured route
	// per pass, NOT one per period. A 240-period catch-up must not issue 240.
	//
	// It also pins WHICH period is probed, which is not cosmetic. ⚠️ NOT because
	// old months have no seats — a first enrollment is backdated to "0000-01", so
	// a founding developer does hold a seat in 2020. The false positives come from
	// org SWITCHES (new org's period_start is the switch month) and DEPARTURES
	// (period_end set), which give legitimate zero-seat months inside a long
	// catch-up and would train operators to ignore this warning. The probe must be
	// the CURRENT period, where a correctly-configured org always has seats and a
	// typo'd org key never does.
	t.Run("a long catch-up costs ONE seat lookup, against the CURRENT period", func(t *testing.T) {
		fake := &fakeSubscriptionStore{seats: 0}
		const current = "2026-08"
		long := []config.Subscription{{RoutePrefix: "glm@h", Org: "typo-org", MonthlyFeeUSD: 100, ActiveSince: "2020-01"}}
		reconcileSubscriptionFeesOnce(context.Background(), fake, long, quietLogger(), current, true)
		if posted := len(fake.calls()); posted < 50 {
			t.Fatalf("catch-up posted only %d periods; the cost claim is not being exercised", posted)
		}
		if got := fake.seatLookups(); got != 1 {
			t.Errorf("seat lookups = %d over %d posted periods, want exactly 1 per route per pass", got, len(fake.calls()))
		}
		if got := fake.seatPeriodsAsked(); len(got) != 1 || got[0] != current {
			t.Errorf("seat lookup asked about %v, want [%s] — probing a backfilled month would warn on every legitimate catch-up", got, current)
		}
	})
}

// TestRunSubscriptionFeeReconciler_StartupPassIsBackfillThenStops exercises the
// LOOP: its first act is a backfill pass, and it returns when ctx is cancelled
// rather than leaking a goroutine past shutdown.
//
// The ticker interval is set long enough that no tick can fire during the test,
// so an observed second pass would be a real bug and not a timing artifact.
func TestRunSubscriptionFeeReconciler_StartupPassIsBackfillThenStops(t *testing.T) {
	fake := &fakeSubscriptionStore{}
	// This test drives the LOOP, which reads the real clock, so the periods must
	// be derived from it. requireDistinctPeriods guards the one way that can go
	// wrong (see monthsBack's doc comment).
	current, prev1 := monthsBack(0), monthsBack(1)
	requireDistinctPeriods(t, prev1, current)
	subs := []config.Subscription{{RoutePrefix: "glm@h", Org: "acme", MonthlyFeeUSD: 1, ActiveSince: prev1}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // also runs if an assertion below t.Fatal's, so the 1h ticker never outlives the test
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSubscriptionFeeReconciler(ctx, fake, subs, quietLogger(), time.Hour)
	}()

	// The startup pass runs before the loop, so waiting for its two calls also
	// proves it happened FIRST — no tick can have produced them at a 1h interval.
	waitFor(t, func() bool { return len(fake.calls()) == 2 },
		"startup pass made %v, want 2 calls (%s then %s)", fake.calls, prev1, current)
	if got, want := fake.methodCalls(), []string{"ifunposted:" + prev1, "converge:" + current}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("startup pass = %v, want %v (a backfill range, with the closed period FILLED and the current one CONVERGED)", got, want)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runSubscriptionFeeReconciler did not return after ctx cancel — it would leak past shutdown")
	}
}

// waitFor polls cond until it holds or the test times out. Used instead of a
// sleep so the test is neither flaky nor slower than it needs to be.
//
// The msg/args tail is not decoration: without it a timeout reports only
// "condition not met", which after a five-second stall tells the reader nothing
// about WHAT was being waited for or what the observed state was. Pass a format
// that renders the actual value.
func waitFor(t *testing.T, cond func() bool, msg string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within 5s: "+msg, args...)
}

// fakeSubscriptionStore records the periods it was asked to post, TAGGED with
// which of the two store methods was used, and can be told to fail for one
// route. It exists for the tests that are about CONTROL FLOW (error tolerance,
// loop shape, which method a period routes to); everything about idempotency is
// tested against the real store, where it actually lives.
type fakeSubscriptionStore struct {
	mu     sync.Mutex
	seen   []string // periods, in call order
	tagged []string // "converge:<period>" / "ifunposted:<period>"
	failOn string
	// seats is what CountActiveSeats reports, and seatCalls counts how often it
	// was asked — the reconciler promises ONE seat lookup per route per pass, and
	// a 240-period catch-up issuing 240 of them is a real regression.
	seats       int
	seatErr     error
	seatCalls   int
	seatPeriods []string // the periods it was asked about, in call order
}

func (f *fakeSubscriptionStore) CountActiveSeats(_ context.Context, _, period string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seatCalls++
	f.seatPeriods = append(f.seatPeriods, period)
	return f.seats, f.seatErr
}

func (f *fakeSubscriptionStore) seatPeriodsAsked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seatPeriods...)
}

func (f *fakeSubscriptionStore) seatLookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seatCalls
}

func (f *fakeSubscriptionStore) ReconcileSubscriptionFee(_ context.Context, routePrefix, _, period string, feeMicro int64) (int64, error) {
	return f.record("converge", routePrefix, period, feeMicro)
}

func (f *fakeSubscriptionStore) PostSubscriptionFeeIfUnposted(_ context.Context, routePrefix, _, period string, feeMicro int64) (int64, error) {
	return f.record("ifunposted", routePrefix, period, feeMicro)
}

func (f *fakeSubscriptionStore) record(method, routePrefix, period string, feeMicro int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, period)
	f.tagged = append(f.tagged, method+":"+period)
	if routePrefix == f.failOn {
		return 0, fmt.Errorf("simulated store failure for %s", routePrefix)
	}
	return feeMicro, nil
}

func (f *fakeSubscriptionStore) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (f *fakeSubscriptionStore) methodCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tagged...)
}

// --- coverage gate (#113 two-artifacts-one-truth) ---

// subscriptionCoverageTable is a price table with ONE subscription route and
// one per-token row for the same model on a different host — the shape that
// makes "covers" a real question rather than a tautology.
const subscriptionCoverageTable = `
version: 1
effective_date: "2026-08-01"
models:
  "glm-5.2@ollama.com": { input_per_m: 0.875, output_per_m: 7.00, provider: self-hosted, billing_mode: subscription }
  "glm-5.2@openrouter.ai": { input_per_m: 0.60, output_per_m: 2.20, provider: self-hosted, billing_mode: per_token }
  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}
  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}
  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}
`

// loadCoverageTable installs subscriptionCoverageTable as the active price
// table for the calling test and restores the embedded default afterwards.
func loadCoverageTable(t *testing.T) {
	t.Helper()
	restoreEmbeddedPriceTable(t)
	path := filepath.Join(t.TempDir(), "prices.yaml")
	if err := os.WriteFile(path, []byte(subscriptionCoverageTable), 0o600); err != nil {
		t.Fatalf("write coverage price table: %v", err)
	}
	if _, err := store.LoadPriceTable(path); err != nil {
		t.Fatalf("load coverage price table: %v", err)
	}
}

// TestCheckSubscriptionCoverage_FatalOnUnmatchedPrefix is the gate's whole
// reason to exist: a fee configured for a route the table does NOT price as a
// subscription would post real dollars into actual-paid with no matching
// list-value tokens, quietly deflating Spend Leverage. It must refuse to start.
//
// The per-token host is the sharp case — the route exists in the table and
// prices fine, it just is not subscription-billed. A gate that only checked
// "is the key present" would wave it through.
func TestCheckSubscriptionCoverage_FatalOnUnmatchedPrefix(t *testing.T) {
	loadCoverageTable(t)

	cases := []struct {
		name, prefix string
	}{
		{"absent from the table", "kimi-2@moonshot.example"},
		{"present but billed per token", "glm-5.2@openrouter.ai"},
		{"misspelled host", "glm-5.2@olama.com"},
		{"misspelled model", "glm-52@ollama.com"},
		// An EMPTY prefix matches every key, so without an explicit guard it would
		// pass this gate unconditionally AND silence every uncovered-route WARN —
		// a config that claims to cover everything by saying nothing.
		// config.validateSubscriptions rejects it too, but this gate is called
		// directly and must not rely on someone else's validation.
		{"empty prefix", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSubscriptionCoverage(
				[]config.Subscription{{RoutePrefix: tc.prefix, Org: "acme", MonthlyFeeUSD: 100}}, quietLogger())
			if err == nil {
				t.Fatalf("checkSubscriptionCoverage(%q) = nil, want a fail-loud error", tc.prefix)
			}
			// The empty prefix fails for a DIFFERENT reason (it would match
			// everything), so it gets its own message; the rest must name the fix.
			want := "billing_mode: subscription"
			if tc.prefix == "" {
				want = "route_prefix is empty"
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to mention %q", err, want)
			}
		})
	}

	// CONTROL ARM: the covered prefix — and a family prefix of it — must pass.
	// Without this the test above would also pass against a gate that rejected
	// every configuration unconditionally.
	//
	// KNOWN AND ACCEPTED: because matching is a PREFIX match, a TRUNCATING typo
	// ("…@ollama.co") is byte-for-byte indistinguishable from a deliberate family
	// prefix and is accepted here on purpose. The looseness buys the family form
	// ("glm-"), which is what lets one plan cover several model rows on one host;
	// no gate can tell the two apart without a convention this schema does not
	// have. Substituting or transposing a character — the far commoner typo — is
	// still caught above.
	for _, prefix := range []string{"glm-5.2@ollama.com", "glm-5.2@ollama", "glm-"} {
		if err := checkSubscriptionCoverage(
			[]config.Subscription{{RoutePrefix: prefix, Org: "acme", MonthlyFeeUSD: 100}}, quietLogger()); err != nil {
			t.Errorf("checkSubscriptionCoverage(%q) = %v, want nil", prefix, err)
		}
	}
}

// TestCheckSubscriptionCoverage_CaseMismatchIsFatalNotSilent pins the CLAIM in
// store.SubscriptionRoutesWithPrefix's doc comment.
//
// That comment used to justify its plain byte-prefix match by saying the key is
// "already-lowercased". It is not: parsePriceTable neither lowercases nor
// case-validates model keys, so lowercase is a convention of the SHIPPED table,
// and a `--prices` override may carry mixed case. The comment now states the
// property that actually holds — a case mismatch cannot silently post an
// unmatched fee, because it is fatal at startup — and this pins that, so the
// justification cannot rot back into an invariant nobody enforces.
func TestCheckSubscriptionCoverage_CaseMismatchIsFatalNotSilent(t *testing.T) {
	restoreEmbeddedPriceTable(t)
	// A table whose subscription key is NOT lowercase. parsePriceTable accepts it
	// — which is the premise; if this ever fails, the invariant became real and
	// the comment should say so again.
	mixed := filepath.Join(t.TempDir(), "prices.yaml")
	if err := os.WriteFile(mixed, []byte(strings.Replace(
		subscriptionCoverageTable, `"glm-5.2@ollama.com"`, `"GLM-5.2@Ollama.com"`, 1)), 0o600); err != nil {
		t.Fatalf("write mixed-case price table: %v", err)
	}
	if _, err := store.LoadPriceTable(mixed); err != nil {
		t.Fatalf("parsePriceTable REJECTED a mixed-case model key: %v — the comment's premise has changed", err)
	}

	// config forces route_prefix lowercase, so this is the only prefix an operator
	// can legally write for that row — and it does not match.
	err := checkSubscriptionCoverage(
		[]config.Subscription{{RoutePrefix: "glm-5.2@ollama.com", Org: "acme", MonthlyFeeUSD: 100}}, quietLogger())
	if err == nil {
		t.Fatal("a case-mismatched route_prefix was accepted — the fee would post against a route nothing prices as a subscription")
	}
	if !strings.Contains(err.Error(), "billing_mode: subscription") {
		t.Errorf("error = %v, want the ordinary unmatched-prefix refusal", err)
	}
}

// TestCheckSubscriptionCoverage_WarnsButStartsOnUncoveredRoute pins the OTHER
// direction's deliberately softer severity. A subscription route with no
// configured fee understates actual-paid (leverage reads HIGH) — worth a WARN,
// never a refusal: `tierd score`, `tierd demo`, and every read-only deployment
// legitimately run a table with subscription rows and no subscriptions block.
func TestCheckSubscriptionCoverage_WarnsButStartsOnUncoveredRoute(t *testing.T) {
	loadCoverageTable(t)
	logger, buf := capturingLogger(slog.LevelWarn)

	if err := checkSubscriptionCoverage(nil, logger); err != nil {
		t.Fatalf("an uncovered subscription route must not block startup: %v", err)
	}
	if !strings.Contains(buf.String(), "glm-5.2@ollama.com") {
		t.Errorf("no WARN naming the uncovered route; got:\n%s", buf.String())
	}

	// CONTROL ARM: once the route IS covered, the WARN must stop. A warning that
	// fires unconditionally is noise an operator learns to ignore.
	logger2, buf2 := capturingLogger(slog.LevelWarn)
	if err := checkSubscriptionCoverage(
		[]config.Subscription{{RoutePrefix: "glm-5.2@ollama.com", Org: "acme", MonthlyFeeUSD: 100}}, logger2); err != nil {
		t.Fatalf("covered route: %v", err)
	}
	if strings.Contains(buf2.String(), "no monthly fee is configured") {
		t.Errorf("WARN still fired for a covered route; got:\n%s", buf2.String())
	}
}

// TestCheckSubscriptionCoverage_EmbeddedDefaultIsSilent pins that the gate is
// inert out of the box: the embedded price table seeds no subscription row, so
// a stock deployment neither fails nor warns.
func TestCheckSubscriptionCoverage_EmbeddedDefaultIsSilent(t *testing.T) {
	loadEmbeddedPriceTable(t)
	logger, buf := capturingLogger(slog.LevelWarn)
	if err := checkSubscriptionCoverage(nil, logger); err != nil {
		t.Fatalf("embedded default: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("embedded default produced warnings:\n%s", buf.String())
	}
}
