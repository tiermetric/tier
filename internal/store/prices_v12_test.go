package store

import "testing"

// v12Rows is the price of 1M tokens of ONE category, in micro-dollars, for the row
// v12 (#1047) added. Each figure is the per-million rate on Anthropic's pricing page
// (platform.claude.com/docs/en/about-claude/pricing, read 2026-10-02), not a product
// of the table's own multipliers, so a wrong default or a stray override fails here.
var v12Rows = map[string]v10PerCategoryMicro{
	"claude-sonnet-5-5": {2_000_000, 10_000_000, 200_000, 2_500_000, 4_000_000},
}

// TestPriceTableV12_PerCategoryRates pins the v12 row's resolved rate for each token
// category separately, its per_token billing basis, and that it is audited rather
// than the guessed self-hosted-medium fallback.
func TestPriceTableV12_PerCategoryRates(t *testing.T) {
	loadDefaultPriceTable(t)
	const m = 1_000_000
	for model, want := range v12Rows {
		cases := []struct {
			name string
			u    CostUsage
			want int64
		}{
			{"input", CostUsage{Input: m}, want.input},
			{"output", CostUsage{Output: m}, want.output},
			{"cache read", CostUsage{CacheRead: m}, want.cacheRead},
			{"cache write 5m", CostUsage{CacheWrite5m: m}, want.write5m},
			{"cache write 1h", CostUsage{CacheWrite1h: m}, want.write1h},
		}
		for _, c := range cases {
			if got := ComputeCost(model, c.u); got != c.want {
				t.Errorf("%s %s: 1M tokens priced at %d micro-dollars, want %d", model, c.name, got, c.want)
			}
		}
		if !IsAuditedRate(HostUnknown, model) {
			t.Errorf("%s: not an audited rate; it would price at the guessed fallback", model)
		}
		if _, mode := ComputeCostHost(HostUnknown, model, CostUsage{Input: m}); mode != BillingPerToken {
			t.Errorf("%s: billing mode %q, want %q", model, mode, BillingPerToken)
		}
	}
}

// TestPriceTableV12_RawIDsResolve pins the model strings that must reach the
// claude-sonnet-5-5 row: the id Claude Code writes (measured in ~/.claude/projects on
// 2026-10-02), a dated snapshot, and case/whitespace variants. None may collapse
// onto claude-sonnet-5; its rates equal this row's, so NormalizeModel is what pins that.
func TestPriceTableV12_RawIDsResolve(t *testing.T) {
	loadDefaultPriceTable(t)
	u := CostUsage{Input: 1_000_000, Output: 1_000_000, CacheRead: 1_000_000}
	for _, raw := range []string{
		"claude-sonnet-5-5",
		"claude-sonnet-5-5-20261001",
		"  Claude-Sonnet-5-5\n",
	} {
		if got := NormalizeModel(raw); got != "claude-sonnet-5-5" {
			t.Errorf("NormalizeModel(%q) = %q, want %q", raw, got, "claude-sonnet-5-5")
		}
		if got, want := ComputeCost(raw, u), ComputeCost("claude-sonnet-5-5", u); got != want {
			t.Errorf("ComputeCost(%q) = %d, want the claude-sonnet-5-5 row's %d", raw, got, want)
		}
	}
}
