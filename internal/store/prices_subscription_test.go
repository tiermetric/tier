package store

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// priceTableWith wraps one model line in the minimal valid document shape (a
// version, an effective_date, and the three required self-hosted fallbacks) so a
// case can vary exactly one field and nothing else.
func priceTableWith(modelLine string) []byte {
	return []byte("version: 1\neffective_date: \"2026-08-01\"\nmodels:\n" + modelLine + "\n" +
		"  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n" +
		"  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n" +
		"  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n")
}

// TestParsePriceTable_RejectsNonFiniteAndAbsurdRates is the finiteness half of
// the money pair #113 makes mandatory. The FEE side (config's monthly_fee_usd)
// has always been checked finite and capped; the comparable RATE side — which is
// what actually lands in the TIER denominator, and which for a subscription route
// is ALWAYS hand-authored because no subscription row ships in the embedded table
// — was checked only for positivity.
//
// `.nan` is the dangerous one: NaN <= 0 is FALSE, so it walked straight through
// the guard whose own comment says it exists to stop the "$0 table" hazard, and
// every call on that route then cost $0. `.inf` / 1e300 are the other direction:
// cost_micro saturates to MaxInt64 and two such rows overflow any SUM.
//
// MUTATION: revert any one of the seven checks to its old `<= 0` / `< 0` form and
// the matching subtests here fail — measured, not assumed.
func TestParsePriceTable_RejectsNonFiniteAndAbsurdRates(t *testing.T) {
	// Every money-shaped field parsePriceTable accepts, with a template that sets
	// exactly that field to the hostile value and leaves the rest well-formed.
	fields := []struct{ name, tmpl, want string }{
		{"input_per_m", `  "glm-5.2@ollama.com": { input_per_m: %s, output_per_m: 7.00, provider: self-hosted, billing_mode: subscription }`, "input_per_m must be > 0"},
		{"output_per_m", `  "glm-5.2@ollama.com": { input_per_m: 0.875, output_per_m: %s, provider: self-hosted, billing_mode: subscription }`, "output_per_m must be > 0"},
		{"input_per_m_over", `  m: { input_per_m: 1, output_per_m: 2, context_threshold: 200000, input_per_m_over: %s, output_per_m_over: 4, provider: anthropic }`, "input_per_m_over must be > 0"},
		{"output_per_m_over", `  m: { input_per_m: 1, output_per_m: 2, context_threshold: 200000, input_per_m_over: 3, output_per_m_over: %s, provider: anthropic }`, "output_per_m_over must be > 0"},
		{"cache_read_mult", `  m: { input_per_m: 1, output_per_m: 2, cache_read_mult: %s, provider: anthropic }`, "cache_read_mult must be >= 0"},
		{"cache_write_5m_mult", `  m: { input_per_m: 1, output_per_m: 2, cache_write_5m_mult: %s, provider: anthropic }`, "cache_write_5m_mult must be >= 0"},
		{"cache_write_1h_mult", `  m: { input_per_m: 1, output_per_m: 2, cache_write_1h_mult: %s, provider: anthropic }`, "cache_write_1h_mult must be >= 0"},
	}
	// .nan is the guard-walker; .inf and 1e300 are the saturation direction. -.inf
	// is included so the multipliers (whose legal floor is 0, not >0) are covered
	// on their negative side by a NON-finite value too.
	values := []string{".nan", ".inf", "-.inf", "1e300"}

	for _, f := range fields {
		for _, v := range values {
			t.Run(f.name+"="+v, func(t *testing.T) {
				_, _, err := parsePriceTable(priceTableWith(fmt.Sprintf(f.tmpl, v)))
				if err == nil {
					t.Fatalf("parsePriceTable ACCEPTED %s: %s — a non-finite or absurd money value reached the price table", f.name, v)
				}
				if !strings.Contains(err.Error(), f.want) {
					t.Errorf("error = %q, want it to name the field: %q", err.Error(), f.want)
				}
			})
		}
	}

	// CONTROL ARM: the same seven fields at ORDINARY values must still parse.
	// Without it, a guard that rejected every table unconditionally would pass
	// every assertion above.
	t.Run("control: ordinary values still parse", func(t *testing.T) {
		ok := []string{
			`  "glm-5.2@ollama.com": { input_per_m: 0.875, output_per_m: 7.00, provider: self-hosted, billing_mode: subscription }`,
			`  m: { input_per_m: 1, output_per_m: 2, context_threshold: 200000, input_per_m_over: 3, output_per_m_over: 4, provider: anthropic }`,
			`  m: { input_per_m: 1, output_per_m: 2, cache_read_mult: 0.1, cache_write_5m_mult: 1.25, cache_write_1h_mult: 2, provider: anthropic }`,
			// 0 on a multiplier is legal and means "inherit the provider default";
			// the bound must not have quietly become "> 0".
			`  m: { input_per_m: 1, output_per_m: 2, cache_read_mult: 0, provider: anthropic }`,
			// The cap is inclusive: MaxCostUSD itself is a legal (if absurd) rate,
			// so the bound is a magnitude check and not an off-by-one.
			fmt.Sprintf(`  m: { input_per_m: %g, output_per_m: 2, provider: anthropic }`, MaxCostUSD),
		}
		for _, line := range ok {
			if _, _, err := parsePriceTable(priceTableWith(line)); err != nil {
				t.Errorf("parsePriceTable REJECTED a well-formed entry %s: %v", line, err)
			}
		}
	})

	// The CONSEQUENCE the guard exists for, pinned so the rationale above cannot
	// rot into a claim nobody checks: a NaN rate prices at $0 (DollarsToMicro pins
	// NaN to 0 deterministically), and an infinite one saturates to MaxInt64 where
	// a second row overflows the SUM.
	t.Run("consequence: why these values are not merely untidy", func(t *testing.T) {
		// Held in a slice so the arithmetic is evaluated at run time on a real
		// float64, the way a rate decoded from YAML reaches ComputeCost — a bare
		// math.NaN() literal here is constant-analysed by staticcheck (SA4012)
		// rather than measured.
		rates := []float64{math.NaN(), math.Inf(1)}
		const tokens = 1000
		if got := DollarsToMicro(rates[0] * tokens / 1e6); got != 0 {
			t.Errorf("a NaN rate prices at %d micro, want 0 — the $0-route claim above is stale", got)
		}
		if got := DollarsToMicro(rates[1] * tokens / 1e6); got != math.MaxInt64 {
			t.Errorf("an infinite rate prices at %d micro, want MaxInt64 — the saturation claim above is stale", got)
		}
	})
}

// TestParsePriceTable_RejectsRatelessSubscriptionEntry is THE fail-loud guard
// behind #113's pricing half, and the reason `tierd score` cannot panic on a
// subscription route (#154).
//
// The abandoned 2026-07 branch carved subscription entries OUT of the
// non-positive-rate check (they were rate-less by design, with comparables
// injected from config at serve startup) and then had to panic in ComputeCost
// if anything ever priced one before the injection — a landmine `tierd score`,
// which never loads a config, could step on. Steve's list-rate inversion puts
// the comparable rate IN the table, so the ordinary guard covers subscription
// entries like any other row and the un-injected state is unrepresentable.
//
// MUTATION: delete the `e.InputPerM <= 0` check in parsePriceTable and this
// test fails — a rate-less subscription entry parses and prices its tokens at
// $0, which is precisely the "$0 hack" the ruling refused.
func TestParsePriceTable_RejectsRatelessSubscriptionEntry(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{
			name: "no rates at all",
			yaml: `  "glm-5.2@ollama.com": { provider: self-hosted, billing_mode: subscription }`,
			want: "input_per_m must be > 0",
		},
		{
			name: "explicit zero input rate",
			yaml: `  "glm-5.2@ollama.com": { input_per_m: 0, output_per_m: 7.00, provider: self-hosted, billing_mode: subscription }`,
			want: "input_per_m must be > 0",
		},
		{
			name: "missing output rate on a non-combined entry",
			yaml: `  "glm-5.2@ollama.com": { input_per_m: 0.875, provider: self-hosted, billing_mode: subscription }`,
			want: "output_per_m must be > 0",
		},
		{
			name: "negative comparable rate",
			yaml: `  "glm-5.2@ollama.com": { input_per_m: -0.875, output_per_m: 7.00, provider: self-hosted, billing_mode: subscription }`,
			want: "input_per_m must be > 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := "version: 1\neffective_date: \"2026-08-01\"\nmodels:\n" + tc.yaml + "\n" +
				"  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n" +
				"  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n" +
				"  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n"
			_, _, err := parsePriceTable([]byte(doc))
			if err == nil {
				t.Fatal("parsePriceTable accepted a subscription entry with no usable comparable rate; want a fail-loud error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestParsePriceTable_AcceptsSubscriptionWithComparableRates is the CONTROL ARM
// for the test above: the same shape WITH positive comparable rates must parse.
// Without it, a guard that rejected every subscription entry unconditionally
// would pass the rejection test and still be broken.
func TestParsePriceTable_AcceptsSubscriptionWithComparableRates(t *testing.T) {
	tbl, _, err := parsePriceTable([]byte(subscriptionTestTableYAML))
	if err != nil {
		t.Fatalf("parsePriceTable rejected a well-formed subscription entry: %v", err)
	}
	p, ok := tbl["glm-5.2@ollama.com"]
	if !ok {
		t.Fatal("subscription entry missing from the parsed table")
	}
	if p.billingMode != BillingSubscription {
		t.Errorf("billing_mode = %q, want %q", p.billingMode, BillingSubscription)
	}
	if p.inputPerM != 0.875 || p.outputPerM != 7.00 {
		t.Errorf("comparable rates = %v/%v, want 0.875/7", p.inputPerM, p.outputPerM)
	}
}

// TestComputeCostHost_SubscriptionRoutePricesAtComparableRate is acceptance
// criterion 1 of #113 at the unit level: a subscription route PRICES, at its
// comparable list rate, and reports billing_mode=subscription so the stored
// event carries an honest basis rather than passing as a canonical $/M.
//
// The exact micro-dollar figure is the point — a "cost > 0" assertion would
// pass against the $0.50/M unknown-model fallback, which is one of the two
// outcomes the ruling forbids.
func TestComputeCostHost_SubscriptionRoutePricesAtComparableRate(t *testing.T) {
	loadSubscriptionTestTable(t)

	// 1000 input × $0.875/M + 500 output × $7/M = $0.000875 + $0.0035 = $0.004375.
	cost, mode := ComputeCostHost("ollama.com", "glm-5.2", CostUsage{Input: 1000, Output: 500})
	if want := int64(4375); cost != want {
		t.Errorf("cost = %d micro, want %d (the comparable list rate, not a fallback)", cost, want)
	}
	if mode != BillingSubscription {
		t.Errorf("billing_mode = %q, want %q", mode, BillingSubscription)
	}

	// CONTROL: the SAME model on a per-token host prices at THAT host's metered
	// rate and reports per_token. Cost is a property of the host, not the weights
	// (#300) — if this returned the subscription figure, the host-qualified
	// lookup would be doing nothing and criterion 1 would be meaningless.
	// 1000 × $0.60/M + 500 × $2.20/M = $0.0006 + $0.0011 = $0.0017.
	cost, mode = ComputeCostHost("openrouter.ai", "glm-5.2", CostUsage{Input: 1000, Output: 500})
	if want := int64(1700); cost != want {
		t.Errorf("per-token host cost = %d micro, want %d", cost, want)
	}
	if mode != BillingPerToken {
		t.Errorf("per-token host billing_mode = %q, want %q", mode, BillingPerToken)
	}
}

// TestComputeCost_SubscriptionRouteNeverPanics is acceptance criterion 2 of the
// batch (#154) at the unit level, exercised on EVERY path that can reach a
// subscription entry with the route's comparables never "injected" — because in
// this design there is no injection step to skip.
//
// It covers the host-blind ComputeCost that `tierd score` reaches through the
// collector, the host-aware form, and the hostile "model@host" string that the
// #300 separator guard routes to the guessed path. A panic in any of them is a
// test failure, not a crash report from an operator.
func TestComputeCost_SubscriptionRouteNeverPanics(t *testing.T) {
	loadSubscriptionTestTable(t)

	u := CostUsage{Input: 1000, Output: 500}
	// Host-blind: no host-qualified key can match, so this falls to the
	// size-class/flat guess and WARNs — priced, honestly, and never a panic.
	if got := ComputeCost("glm-5.2", u); got <= 0 {
		t.Errorf("host-blind ComputeCost = %d, want a positive fallback price", got)
	}
	// Host-aware on the subscription host: the comparable rate.
	if got, _ := ComputeCostHost("ollama.com", "glm-5.2", u); got != 4375 {
		t.Errorf("host-aware ComputeCostHost = %d, want 4375", got)
	}
	// A forged "model@host" model string must not reach the subscription row via
	// the model-only lookup (#300 security guard) — and must not panic either.
	if got, mode := ComputeCostHost("", "glm-5.2@ollama.com", u); got == 4375 {
		t.Error("a forged model@host string resolved the subscription row directly — the separator guard is not holding")
	} else if got <= 0 || mode != BillingSelfHostedAmortized {
		t.Errorf("forged model@host = (%d, %q), want a positive guessed self-hosted price", got, mode)
	}
	// Zero usage on a subscription route is $0 by arithmetic, not by fallback.
	if got, mode := ComputeCostHost("ollama.com", "glm-5.2", CostUsage{}); got != 0 || mode != BillingSubscription {
		t.Errorf("zero-usage subscription call = (%d, %q), want (0, %q)", got, mode, BillingSubscription)
	}
}
