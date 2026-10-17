package store

import "testing"

// Pin independent dollar examples for the v13 additions (#1202), including
// Haiku's strict 100K boundary and caches contributing to prompt length.
func TestPriceTableV13_CostExamples(t *testing.T) {
	loadDefaultPriceTable(t)
	cases := []struct {
		name  string
		model string
		usage CostUsage
		want  int64
	}{
		{"Haiku input/output", "claude-haiku-5-5", CostUsage{Input: 10_000, Output: 2_000}, 2_000}, // $0.001 + $0.001
		{"Sol input/output", "gpt-6.1-sol", CostUsage{Input: 10_000, Output: 2_000}, 40_000},       // $0.02 + $0.02
		{"Haiku at 100K", "claude-haiku-5-5", CostUsage{Input: 100_000, Output: 2_000}, 11_000},
		{"Haiku above 100K", "claude-haiku-5-5", CostUsage{Input: 100_002, Output: 2_000}, 55_001},
		{"Haiku caches at 100K", "claude-haiku-5-5", CostUsage{Input: 70_000, CacheRead: 10_000, CacheWrite5m: 10_000, CacheWrite1h: 10_000, Output: 2_000}, 11_350},
		{"Haiku caches above 100K", "claude-haiku-5-5", CostUsage{Input: 70_002, CacheRead: 10_000, CacheWrite5m: 10_000, CacheWrite1h: 10_000, Output: 2_000}, 56_751},
		{"Haiku cache read", "claude-haiku-5-5", CostUsage{CacheRead: 100_000}, 1_000},
		{"Haiku long cache read", "claude-haiku-5-5", CostUsage{CacheRead: 200_000}, 10_000},
		{"Sol cache read", "gpt-6.1-sol", CostUsage{CacheRead: 1_000_000}, 100_000},
		{"Sol cache write 5m", "gpt-6.1-sol", CostUsage{CacheWrite5m: 1_000_000}, 2_500_000},
		{"Sol cache write 1h", "gpt-6.1-sol", CostUsage{CacheWrite1h: 1_000_000}, 2_500_000},
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
