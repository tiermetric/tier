package config

import (
	"strings"
	"testing"
	"time"
)

// fixedNow is the clock validateSubscriptions is judged against in these tests,
// so the active_since bounds assert a FIXED relationship rather than drifting
// with the wall clock (a "2026-09" case that is future today and past next
// month is a test that stops testing anything).
var fixedNow = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

// TestLoad_SubscriptionsRoundTrip proves the block reaches the caller intact
// through the strict decoder — including active_since, whose absence and
// presence select two different reconciler behaviours (#155).
func TestLoad_SubscriptionsRoundTrip(t *testing.T) {
	path := writeYAML(t, `
subscriptions:
  - route_prefix: "glm-5.2@ollama.com"
    plan: "max"
    org: "acme"
    monthly_fee_usd: 100.00
    active_since: "2026-06"
  - route_prefix: "kimi-2@moonshot.example"
    monthly_fee_usd: 40
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Subscriptions) != 2 {
		t.Fatalf("got %d subscriptions, want 2", len(cfg.Subscriptions))
	}
	s := cfg.Subscriptions[0]
	if s.RoutePrefix != "glm-5.2@ollama.com" || s.Plan != "max" || s.Org != "acme" ||
		s.MonthlyFeeUSD != 100.00 || s.ActiveSince != "2026-06" {
		t.Errorf("subscriptions[0] = %+v, want the full round-trip", s)
	}
	if got := cfg.Subscriptions[1]; got.ActiveSince != "" {
		t.Errorf("absent active_since = %q, want empty (current-period-only is the default)", got.ActiveSince)
	}
}

// TestLoad_SubscriptionsRejectsUnknownKey pins that the block inherits the
// loader's strict KnownFields decoding. A typo'd `monthly_fee` would otherwise
// leave the fee at 0 — and a $0 fee is exactly the silent-nothing failure the
// positive-fee guard exists to prevent, arriving by a different door.
func TestLoad_SubscriptionsRejectsUnknownKey(t *testing.T) {
	path := writeYAML(t, `
subscriptions:
  - route_prefix: "glm-5.2@ollama.com"
    monthly_fee: 100.00
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted an unknown subscriptions key; want a strict-decode error")
	}
}

// TestValidateSubscriptions covers every fail-loud arm of the block. Each case
// is a misconfiguration whose SILENT form would be a wrong number on a
// CFO-facing metric, so each must be a startup error rather than a default.
func TestValidateSubscriptions(t *testing.T) {
	valid := Subscription{RoutePrefix: "glm-5.2@ollama.com", Org: "acme", MonthlyFeeUSD: 100}

	cases := []struct {
		name string
		subs []Subscription
		want string // "" = must validate cleanly
	}{
		{name: "nil block", subs: nil},
		{name: "empty block", subs: []Subscription{}},
		{name: "minimal valid", subs: []Subscription{valid}},
		{name: "valid with active_since", subs: []Subscription{{RoutePrefix: "a@h", Org: "o", MonthlyFeeUSD: 1, ActiveSince: "2026-06"}}},
		{name: "active_since equal to current period", subs: []Subscription{{RoutePrefix: "a@h", Org: "o", MonthlyFeeUSD: 1, ActiveSince: "2026-08"}}},

		{name: "missing route_prefix", subs: []Subscription{{Org: "o", MonthlyFeeUSD: 1}}, want: "route_prefix is required"},
		{name: "uppercase route_prefix", subs: []Subscription{{RoutePrefix: "GLM-5.2@Ollama.com", Org: "o", MonthlyFeeUSD: 1}}, want: "must be lowercase"},
		{name: "over-long route_prefix", subs: []Subscription{{RoutePrefix: strings.Repeat("a", 257), Org: "o", MonthlyFeeUSD: 1}}, want: "route_prefix must be <="},
		{name: "over-long org", subs: []Subscription{{RoutePrefix: "a@h", Org: strings.Repeat("o", 257), MonthlyFeeUSD: 1}}, want: "org must be <="},

		{name: "zero fee", subs: []Subscription{{RoutePrefix: "a@h", Org: "o"}}, want: "monthly_fee_usd must be > 0"},
		{name: "negative fee", subs: []Subscription{{RoutePrefix: "a@h", Org: "o", MonthlyFeeUSD: -1}}, want: "monthly_fee_usd must be > 0"},
		// MAGNITUDE, not just sign. store.DollarsToMicro SATURATES to MaxInt64
		// instead of erroring, and one saturated org_actual_spend row makes the
		// allocation read's SUM fail with "integer overflow" the moment any second
		// row shares its (org, period) — so an unbounded fee does not report a wrong
		// number, it takes /scores down.
		{name: "fee above the magnitude cap", subs: []Subscription{{RoutePrefix: "a@h", Org: "o", MonthlyFeeUSD: 1e13}}, want: "monthly_fee_usd must be <="},
		{name: "fee at the magnitude cap", subs: []Subscription{{RoutePrefix: "a@h", Org: "o", MonthlyFeeUSD: 1e12}}},

		{name: "empty derived org key", subs: []Subscription{{RoutePrefix: "/", MonthlyFeeUSD: 1}}, want: "empty org key"},

		{name: "malformed active_since", subs: []Subscription{{RoutePrefix: "a@h", Org: "o", MonthlyFeeUSD: 1, ActiveSince: "2026-6"}}, want: "must be YYYY-MM"},
		{name: "day-bearing active_since", subs: []Subscription{{RoutePrefix: "a@h", Org: "o", MonthlyFeeUSD: 1, ActiveSince: "2026-06-01"}}, want: "must be YYYY-MM"},
		{name: "month 13 active_since", subs: []Subscription{{RoutePrefix: "a@h", Org: "o", MonthlyFeeUSD: 1, ActiveSince: "2026-13"}}, want: "must be YYYY-MM"},
		{name: "future active_since", subs: []Subscription{{RoutePrefix: "a@h", Org: "o", MonthlyFeeUSD: 1, ActiveSince: "2026-09"}}, want: "is in the future"},
		{name: "typo'd year active_since", subs: []Subscription{{RoutePrefix: "a@h", Org: "o", MonthlyFeeUSD: 1, ActiveSince: "0226-06"}}, want: "would backfill"},

		{name: "duplicate prefix", subs: []Subscription{valid, valid}, want: "overlaps"},
		{name: "shadowing prefix", subs: []Subscription{{RoutePrefix: "glm-", Org: "o", MonthlyFeeUSD: 1}, valid}, want: "overlaps"},
		{name: "distinct prefixes", subs: []Subscription{valid, {RoutePrefix: "kimi-2@moonshot.example", Org: "o", MonthlyFeeUSD: 40}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSubscriptions(tc.subs, fixedNow)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validateSubscriptions: unexpected error %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateSubscriptions accepted %+v; want an error mentioning %q", tc.subs, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestValidateSubscriptions_RejectsNonFiniteFee covers the YAML shapes that
// decode into a float64 the ordinary "> 0" test would ACCEPT. `.inf` is greater
// than zero; store.DollarsToMicro would turn it into MaxInt64 micro-dollars and
// write invoice-grade garbage into org_actual_spend.
func TestValidateSubscriptions_RejectsNonFiniteFee(t *testing.T) {
	for _, body := range []string{
		"subscriptions:\n  - route_prefix: \"a@h\"\n    org: o\n    monthly_fee_usd: .inf\n",
		"subscriptions:\n  - route_prefix: \"a@h\"\n    org: o\n    monthly_fee_usd: .nan\n",
	} {
		if _, err := Load(writeYAML(t, body)); err == nil {
			t.Errorf("Load accepted a non-finite monthly_fee_usd in:\n%s", body)
		}
	}
}

// TestValidateSubscriptions_RejectsSubMicroFee covers the gap BETWEEN the two
// bounds either side of it: a fee that is strictly positive and finite (so
// positiveFinite passes) and far under the magnitude cap, yet rounds to ZERO
// micro-dollars in the store.
//
// The consequence is not a small error, it is a permanent one. #155's backfill
// treats "this period has any row" as posted, so a zero-valued row marks the
// month covered forever; the 0 return is indistinguishable from "already
// covered", so the reconciler logs nothing at all; and a later correction to
// monthly_fee_usd can never repair those months.
//
// MUTATION: delete the minMonthlyFeeUSD check in validateSubscriptions and the
// rejected cases here fail.
func TestValidateSubscriptions_RejectsSubMicroFee(t *testing.T) {
	cases := []struct {
		name string
		fee  string
		want bool // should Load accept it?
	}{
		{"the measured zero-rounding case", "4e-7", false},
		{"a hair under one micro-dollar", "9.99e-7", false},
		{"absurdly small but positive", "1e-30", false},
		// CONTROL ARM: exactly one micro-dollar, and an ordinary plan, must still
		// load. Without these a guard that rejected every fee would pass above.
		{"exactly one micro-dollar", "1e-6", true},
		{"an ordinary plan", "100.00", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "subscriptions:\n  - route_prefix: \"a@h\"\n    org: o\n    monthly_fee_usd: " + tc.fee + "\n"
			_, err := Load(writeYAML(t, body))
			if accepted := err == nil; accepted != tc.want {
				t.Errorf("monthly_fee_usd %s: Load accepted=%v, want %v (err=%v)", tc.fee, accepted, tc.want, err)
			}
		})
	}
}

// TestSubscriptionOrgKey pins the fee's destination key. Getting it wrong posts
// real money under an org no allocation read queries, where it is invisible
// rather than wrong — the worst shape of failure for a finance figure.
func TestSubscriptionOrgKey(t *testing.T) {
	cases := []struct {
		sub  Subscription
		want string
	}{
		{Subscription{RoutePrefix: "glm-5.2@ollama.com", Org: "acme"}, "acme"},
		{Subscription{RoutePrefix: "glm-5.2@ollama.com"}, "glm-5.2@ollama.com"},
		{Subscription{RoutePrefix: "ollama-cloud/"}, "ollama-cloud"},
	}
	for _, tc := range cases {
		if got := tc.sub.OrgKey(); got != tc.want {
			t.Errorf("OrgKey(%+v) = %q, want %q", tc.sub, got, tc.want)
		}
	}
}

// TestMonthsBetween pins the arithmetic behind the typo bound, including the
// year-boundary case an off-by-twelve would silently pass.
func TestMonthsBetween(t *testing.T) {
	cases := []struct {
		from, to string
		want     int
	}{
		{"2026-08", "2026-08", 1},
		{"2026-06", "2026-08", 3},
		{"2025-12", "2026-01", 2},
		{"2025-08", "2026-08", 13},
	}
	for _, tc := range cases {
		if got := monthsBetween(tc.from, tc.to); got != tc.want {
			t.Errorf("monthsBetween(%q, %q) = %d, want %d", tc.from, tc.to, got, tc.want)
		}
	}
}
