// Subscription-fee posting (#113, list-rate inversion).
//
// A flat-fee route (Ollama Cloud MAX and friends) carries TWO money facts with
// TWO destinations, and #113's ruling is that they must never be mixed:
//
//   - its TOKENS are priced in the TIER denominator at a COMPARABLE LIST RATE,
//     carried by the route's price-table entry alongside
//     `billing_mode: subscription` (see prices.go). That pricing is local,
//     per-call, and deterministic at ingest — no window, no divisor, no
//     dependence on another developer's usage.
//   - its FEE — the dollars the org genuinely pays — enters ONLY the
//     actual-paid side of Spend Leverage, via the idempotent per-period
//     org_actual_spend post implemented here.
//
// WHY THERE IS NO LEDGER TABLE. The abandoned 2026-07 branch carried a
// `subscription_fee_postings` table keyed (route_prefix, period), because at
// that time OrgActualSpendNet was NOT source-scoped: converging the (org,
// period) NET to the fee would have silently offset finance's manual rows.
// #138 later added org_actual_spend.source and a source-SCOPED net for exactly
// that reason, so the isolation the ledger existed to provide is now a column.
// Reconciling against this route's OWN source net is therefore equivalent, one
// table lighter, and strictly better on one axis: it SELF-HEALS. A ledger that
// says "posted" while the money row has been deleted never repairs itself; a
// source-scoped net re-posts the difference on the next pass.
package store

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// subscriptionSourcePrefix tags org_actual_spend rows written by the #113
// subscription-fee reconciler. The full source is this prefix plus the route
// prefix (see SubscriptionSpendSource), so EACH configured route reconciles
// against its own rows only — two subscriptions under one org do not net
// against each other, and neither nets against 'manual' or a provider poller.
const subscriptionSourcePrefix = "subscription:"

// SubscriptionSpendSource returns the org_actual_spend `source` tag for a
// subscription route's flat-fee rows (#113/#138). It is the single definition
// of that tag: ReconcileSubscriptionFee uses it for BOTH the net read and the
// written row, so the two can never drift apart and start double-posting.
//
// The route prefix is embedded rather than folded into one flat 'subscription'
// tag on purpose: the net is the reconciler's memory of what it already posted,
// so two routes sharing a tag would each read the OTHER's postings as their own
// and post a compensating negative delta forever.
func SubscriptionSpendSource(routePrefix string) string {
	return subscriptionSourcePrefix + routePrefix
}

// periodRE matches a billing period: YYYY-MM with a REAL month (01-12). It is
// deliberately stricter than time.Parse("2006-01", …), which accepts a
// single-digit month ("2026-7") that would then violate the
// GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]' CHECK on org_actual_spend.period and
// fail at INSERT time instead of at config-validation time.
var periodRE = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])$`)

// ValidPeriod reports whether s is a well-formed billing period (YYYY-MM, month
// 01-12) — the shape org_actual_spend.period's CHECK constraint enforces.
func ValidPeriod(s string) bool { return periodRE.MatchString(s) }

// CurrentPeriod returns the billing period containing t, in UTC. Every period
// in TIER is a UTC calendar month: a local-zone month boundary would post one
// org's fee under two different periods depending on where the server runs.
func CurrentPeriod(t time.Time) string { return t.UTC().Format("2006-01") }

// MaxPeriodRange bounds PeriodRange at 20 years of months. config's
// active_since validation applies the same bound earlier and with a better error
// message, but the store must not DEPEND on that: a caller that skips config
// (a future CLI, a test, an operator tool) would otherwise materialise an
// unbounded slice from a typo'd year like "0226-06" and issue ~21,000
// transactions. The bound lives on both sides on purpose.
const MaxPeriodRange = 240

// PeriodRange returns every billing period from `from` through `to` inclusive,
// in ascending order (#155's backfill list). It returns an error when either
// bound is malformed, when from > to, or when the range exceeds MaxPeriodRange —
// an empty or absurd range would silently skip (or grossly overshoot) the
// backfill the operator asked for, which is precisely the failure #155 exists
// to fix.
func PeriodRange(from, to string) ([]string, error) {
	if !ValidPeriod(from) {
		return nil, fmt.Errorf("period %q must be YYYY-MM with month 01-12", from)
	}
	if !ValidPeriod(to) {
		return nil, fmt.Errorf("period %q must be YYYY-MM with month 01-12", to)
	}
	if from > to { // lexicographic ordering is chronological for zero-padded YYYY-MM
		return nil, fmt.Errorf("period range %s..%s is inverted", from, to)
	}
	start, err := time.Parse("2006-01", from)
	if err != nil {
		return nil, fmt.Errorf("parse period %q: %w", from, err)
	}
	var out []string
	for p := start.UTC(); ; p = p.AddDate(0, 1, 0) {
		s := p.Format("2006-01")
		out = append(out, s)
		if s >= to {
			break
		}
		if len(out) >= MaxPeriodRange {
			return nil, fmt.Errorf("period range %s..%s exceeds %d periods — check for a typo in the year", from, to, MaxPeriodRange)
		}
	}
	return out, nil
}

// ReconcileSubscriptionFee brings the flat fee recorded for (routePrefix,
// period) up to feeMicro by posting the DIFFERENCE into org_actual_spend under
// org, tagged SubscriptionSpendSource(routePrefix) (#113). It returns the delta
// posted, in integer micro-dollars — 0 when nothing needed posting.
//
// IDEMPOTENT BY CONSTRUCTION, which is the property #155's backfill leans on: a
// second pass reads back the net it just wrote, computes delta 0, and writes no
// row. Restarts, hourly ticks, and a re-run of the startup catch-up all
// converge instead of accumulating.
//
// A fee CHANGE posts exactly the difference — a raise as a positive delta, a
// cut as a negative one (the #24 accumulation model admits credit memos). That
// convergence is right for the CURRENT period, where "what we pay this month"
// is the live fact. It is WRONG for a closed one: an org that paid $100 in June
// paid $100 in June, and no August config edit changes that. So the #155
// backfill uses PostSubscriptionFeeIfUnposted for historical periods and this
// converging form only for the current one. See that function.
//
// SOURCE SCOPING is load-bearing, not tidiness (#138 review R1): both the read
// and the write are scoped to this route's own source, so a finance row posted
// by hand under 'manual', an Anthropic/OpenAI poller row, or a SECOND
// subscription route sharing the same (org, period) is neither netted against
// nor offset. The allocation read sums across sources, so the org total stays
// complete.
//
// The read and the write run in ONE transaction — but be precise about what
// that buys, because it is less than it looks. modernc.org/sqlite ignores
// sql.TxOptions.Isolation entirely, so this BeginTx is DEFERRED (#598); the
// real in-process atomicity comes from SetMaxOpenConns(1), which gives the tx
// the process's only connection. Against another PROCESS (a concurrent `tierd
// reprice` / `backfill`) a DEFERRED read-then-write can hit
// SQLITE_BUSY_SNAPSHOT, which busy_timeout does NOT retry — so the INSERT
// errors, this call returns the error, and the caller retries on the next tick.
// That is the honest guarantee: no double-post is possible, but a cross-process
// race is a retried failure rather than a serialized success. Promoting to
// store.beginImmediate is deliberately left to #598's sweep rather than done
// unilaterally here.
//
// period must be a valid YYYY-MM (ValidPeriod); a malformed one is rejected
// here rather than left to the org_actual_spend CHECK constraint, so the error
// names the caller's mistake instead of a SQL constraint.
func (d *DB) ReconcileSubscriptionFee(ctx context.Context, routePrefix, org, period string, feeMicro int64) (int64, error) {
	if routePrefix == "" {
		return 0, fmt.Errorf("subscription fee reconcile: route prefix is required")
	}
	if org == "" {
		return 0, fmt.Errorf("subscription fee reconcile (%s): org is required", routePrefix)
	}
	if !ValidPeriod(period) {
		return 0, fmt.Errorf("subscription fee reconcile (%s): period %q must be YYYY-MM with month 01-12", routePrefix, period)
	}
	source := SubscriptionSpendSource(routePrefix)

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin subscription fee reconcile (%s, %s): %w", routePrefix, period, err)
	}
	defer func() { _ = tx.Rollback() }()

	// SUM over zero rows is SQL NULL; COALESCE maps that to a 0 net so an
	// unposted period reads as "nothing posted yet" rather than a scan error.
	var posted int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(actual_paid_micro), 0) FROM org_actual_spend
		WHERE org = ? AND period = ? AND source = ?`,
		org, period, source,
	).Scan(&posted); err != nil {
		return 0, fmt.Errorf("read subscription fee net (%s, %s): %w", routePrefix, period, err)
	}

	delta := feeMicro - posted
	if delta == 0 {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO org_actual_spend (org, period, actual_paid_micro, source, ts)
		VALUES (?, ?, ?, ?, ?)`,
		org, period, delta, source, time.Now().UTC(),
	); err != nil {
		return 0, fmt.Errorf("post subscription fee delta (%s, %s): %w", routePrefix, period, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit subscription fee reconcile (%s, %s): %w", routePrefix, period, err)
	}
	return delta, nil
}

// PostSubscriptionFeeIfUnposted posts the full monthly fee for (routePrefix,
// period) if and ONLY if this route has never posted a row for that period, and
// returns what it posted (0 when the period was already covered). It is the
// backfill half of #155, and it exists because "catch up a missed month" and
// "correct this month's fee" are different operations that must not share code.
//
// WHY NOT ReconcileSubscriptionFee. That one CONVERGES the period's net to the
// configured fee, which for a CLOSED period means an August config edit silently
// restates what the org paid in June — every historical Spend Leverage figure
// moves, and the operator is never told. #155 asks only for periods that are
// "active and UNPOSTED"; this is that, exactly.
//
// "Unposted" is the ABSENCE OF ANY ROW under this route's source, not a zero
// net. A period whose postings deliberately net to zero (a fee posted, then
// fully credited) has been decided; re-posting the fee there would overrule a
// human correction, which is the same class of error as the restatement above.
//
// Same transaction and same DEFERRED-BeginTx caveat as ReconcileSubscriptionFee.
func (d *DB) PostSubscriptionFeeIfUnposted(ctx context.Context, routePrefix, org, period string, feeMicro int64) (int64, error) {
	if routePrefix == "" {
		return 0, fmt.Errorf("subscription fee backfill: route prefix is required")
	}
	if org == "" {
		return 0, fmt.Errorf("subscription fee backfill (%s): org is required", routePrefix)
	}
	if !ValidPeriod(period) {
		return 0, fmt.Errorf("subscription fee backfill (%s): period %q must be YYYY-MM with month 01-12", routePrefix, period)
	}
	// A non-positive fee here would write a row whose value is zero (or negative)
	// and — because "unposted" means NO ROW — permanently mark the period covered,
	// with a 0 return the caller cannot distinguish from "already covered". No
	// later config correction can repair it. config.minMonthlyFeeUSD stops that at
	// the entry point; this is the store-side belt for a caller that skips config.
	//
	// It is deliberately NOT mirrored onto ReconcileSubscriptionFee: converging a
	// period to zero is a LEGITIMATE operation there (a retroactively cancelled
	// plan, pinned by TestPostSubscriptionFeeIfUnposted_ZeroNetCountsAsPosted),
	// and the two methods differ precisely because filling and converging are not
	// the same act.
	if feeMicro <= 0 {
		return 0, fmt.Errorf("subscription fee backfill (%s, %s): fee must be > 0 micro-dollars, got %d — filling a period with a zero or negative fee would mark it permanently posted", routePrefix, period, feeMicro)
	}
	source := SubscriptionSpendSource(routePrefix)

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin subscription fee backfill (%s, %s): %w", routePrefix, period, err)
	}
	defer func() { _ = tx.Rollback() }()

	var rows int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM org_actual_spend
		WHERE org = ? AND period = ? AND source = ?`,
		org, period, source,
	).Scan(&rows); err != nil {
		return 0, fmt.Errorf("read subscription fee postings (%s, %s): %w", routePrefix, period, err)
	}
	if rows > 0 {
		return 0, nil // already covered — the idempotent case
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO org_actual_spend (org, period, actual_paid_micro, source, ts)
		VALUES (?, ?, ?, ?, ?)`,
		org, period, feeMicro, source, time.Now().UTC(),
	); err != nil {
		return 0, fmt.Errorf("post subscription fee backfill (%s, %s): %w", routePrefix, period, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit subscription fee backfill (%s, %s): %w", routePrefix, period, err)
	}
	return feeMicro, nil
}

// CountActiveSeats returns how many DISTINCT developers held an active
// period_membership seat in org during the given billing period.
//
// It exists for ONE caller: the subscription-fee reconciler, which posts real
// money under a config-supplied org key that nothing else validates. An omitted
// or typo'd `org:` (the key falls back to the route prefix when absent) posts
// into an org that has no seats — and the allocation read reaches a developer
// ONLY through `JOIN period_membership pm ON pm.org = inv.org`, so those dollars
// reach nobody: ActualPaidUSD stays 0 and Spend Leverage renders "—" while the
// reconciler cheerfully logs a successful post every month. That is the exact
// mirror of the failure the startup coverage gate treats as FATAL (a bad
// route_prefix INFLATES actual-paid; a bad org UNDERSTATES it), and the startup
// gate structurally cannot catch it: it runs before store.Open, so it has no DB.
//
// THE PREDICATE MIRRORS the `active` CTE in actualSpendCTE exactly — same org
// equality, same non-empty guard, same half-open period window, same DISTINCT
// developer. If the two ever drift, the warning starts lying in one direction or
// the other, so TestCountActiveSeatsMirrorsAllocation pins them against each
// other rather than against a restated copy of this SQL.
func (d *DB) CountActiveSeats(ctx context.Context, org, period string) (int, error) {
	if !ValidPeriod(period) {
		return 0, fmt.Errorf("count active seats (%s): period %q must be YYYY-MM with month 01-12", org, period)
	}
	var n int
	if err := d.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT developer) FROM period_membership
		WHERE org = ? AND org IS NOT NULL AND org != ''
		  AND period_start <= ?
		  AND (period_end IS NULL OR period_end >= ?)`,
		org, period, period,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count active seats (%s, %s): %w", org, period, err)
	}
	return n, nil
}

// SubscriptionRoutes returns every key in the ACTIVE price table whose resolved
// billing_mode is `subscription` (#113), sorted. These are the routes whose
// cost_micro is a comparable-list-rate valuation of subscription-billed tokens
// rather than a metered per-token price.
//
// It reads priceTable under the same write-once-before-serve contract as
// ComputeCost (see priceTable's doc): callers run it at startup, after any
// --prices override has settled and before serving.
func SubscriptionRoutes() []string { return subscriptionRoutesMatching("") }

// SubscriptionRoutesWithPrefix returns the subscription-mode price-table keys
// that begin with prefix, sorted. It is the TABLE half of #113's
// two-artifacts-one-truth rule — the price table says "this route is
// subscription-billed, and here is its comparable rate"; the `subscriptions:`
// config block says "and here is the fee we pay for it". A configured route
// prefix that matches NOTHING here means the two artifacts disagree, and the
// caller refuses to start.
//
// The prefix is matched against the price-table KEY, which is either a
// normalized model (`glm-5.2`) or a host-qualified `model@host`
// (`glm-5.2@ollama.com`, see HostModelKey) — so a prefix can name one model,
// one family, or (via the "@host" tail) nothing at all.
//
// Matching is a plain, case-SENSITIVE byte prefix. Be precise about why that is
// safe, because the obvious justification is wrong: parsePriceTable neither
// lowercases nor case-validates model keys, so "the key is already lowercase" is
// a convention of the shipped table, not an enforced invariant — a --prices
// override may carry `GLM-5.2@Ollama.com`. What actually holds is the FAILURE
// MODE: config rejects a non-lowercase route_prefix, and a prefix that matches
// nothing is fatal at startup (checkSubscriptionCoverage), so a case mismatch
// cannot silently post a fee against an unmatched route — it refuses to start.
func SubscriptionRoutesWithPrefix(prefix string) []string { return subscriptionRoutesMatching(prefix) }

// SubscriptionRouteSummary renders each subscription route in the active table
// as "key (in $X/M, out $Y/M — comparable list rate)", sorted, for the operator
// provenance line `tierd score` prints (#154 item 3).
//
// It carries the RATES, not just the names, because the rate is the auditable
// part: a reader can check "is $0.875/$7.00 a fair peer for this model?" but can
// do nothing at all with a bare route name. A combined entry renders one rate,
// matching how it bills.
func SubscriptionRouteSummary() []string {
	routes := SubscriptionRoutes()
	out := make([]string, 0, len(routes))
	for _, k := range routes {
		p := priceTable[k]
		if p.combined {
			out = append(out, fmt.Sprintf("%s ($%g/M combined — comparable list rate, not a metered price)", k, p.inputPerM))
			continue
		}
		out = append(out, fmt.Sprintf("%s (in $%g/M, out $%g/M — comparable list rate, not a metered price)", k, p.inputPerM, p.outputPerM))
	}
	return out
}

// subscriptionRoutesMatching is the shared scan behind the two exported forms:
// an empty prefix matches every key. Kept private so the "what counts as a
// subscription route" test lives in exactly one place.
func subscriptionRoutesMatching(prefix string) []string {
	var out []string
	for k, p := range priceTable {
		if p.billingMode != BillingSubscription {
			continue
		}
		if prefix != "" && !strings.HasPrefix(k, prefix) {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
