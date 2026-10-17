package store

import "testing"

// TestPriceTableV14_Sonnet55CacheReadReceipt pins Anthropic's pricing page read
// 2026-10-12 (#1206): Sonnet 5.5 cache hits cost $0.10/M (0.05x of $2 input),
// down from the $0.20/M published on 2026-10-02 and encoded in v12/v13.
// Dollar expectations are independent of the table's multipliers.
func TestPriceTableV14_Sonnet55CacheReadReceipt(t *testing.T) {
	loadDefaultPriceTable(t)
	cases := []struct {
		name  string
		model string
		usage CostUsage
		want  int64
	}{
		{"Sonnet cache read", "claude-sonnet-5-5", CostUsage{CacheRead: 1_000_000}, 100_000},
		{"Sonnet dated cache read", "claude-sonnet-5-5-20261001", CostUsage{CacheRead: 1_000_000}, 100_000},
		{"Sonnet input", "claude-sonnet-5-5", CostUsage{Input: 1_000_000}, 2_000_000},
		{"Sonnet output", "claude-sonnet-5-5", CostUsage{Output: 1_000_000}, 10_000_000},
		{"Sonnet cache write 5m", "claude-sonnet-5-5", CostUsage{CacheWrite5m: 1_000_000}, 2_500_000},
		{"Sonnet cache write 1h", "claude-sonnet-5-5", CostUsage{CacheWrite1h: 1_000_000}, 4_000_000},
		{"Sonnet mixed usage", "claude-sonnet-5-5", CostUsage{Input: 10_000, Output: 2_000, CacheRead: 100_000, CacheWrite5m: 10_000, CacheWrite1h: 10_000}, 115_000}, // $0.02 + $0.02 + $0.01 + $0.025 + $0.04
		{"Sonnet 5 unchanged", "claude-sonnet-5", CostUsage{CacheRead: 1_000_000}, 200_000},
		{"Fable 5.1 unchanged", "claude-fable-5-1", CostUsage{CacheRead: 1_000_000}, 250_000},
		{"Opus 5.5 unchanged", "claude-opus-5-5", CostUsage{CacheRead: 1_000_000}, 200_000},
		{"Haiku 5.5 unchanged", "claude-haiku-5-5", CostUsage{CacheRead: 100_000}, 1_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, mode := ComputeCostHost(HostUnknown, c.model, c.usage); got != c.want || mode != BillingPerToken {
				t.Errorf("ComputeCostHost(%q, %+v) = (%d, %q), want (%d, %q)", c.model, c.usage, got, mode, c.want, BillingPerToken)
			}
			if !IsAuditedRate(HostUnknown, c.model) {
				t.Errorf("%s: priced at the guessed fallback", c.model)
			}
		})
	}
}
