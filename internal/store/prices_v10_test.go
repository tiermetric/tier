package store

import (
	"strings"
	"testing"
)

// v10PerCategoryMicro is the price of 1M tokens of ONE category, in micro-dollars,
// for every row v10 (#786) added or changed. Each figure is the per-million rate on
// the official pricing page the operator pasted on 2026-09-25, not a product of the
// table's own multipliers, so a mistyped multiplier fails here.
type v10PerCategoryMicro struct {
	input, output, cacheRead, write5m, write1h int64
}

var v10Rows = map[string]v10PerCategoryMicro{
	"claude-fable-5-1":  {10_000_000, 50_000_000, 250_000, 12_500_000, 20_000_000},
	"claude-mythos-5-1": {10_000_000, 50_000_000, 250_000, 12_500_000, 20_000_000},
	"claude-opus-5-5":   {4_000_000, 20_000_000, 200_000, 5_000_000, 8_000_000},
	// The fix: $2/$10, down from $3/$15. Cache classes are the anthropic defaults.
	"claude-sonnet-5": {2_000_000, 10_000_000, 200_000, 2_500_000, 4_000_000},
	"gpt-6-astra":     {10_000_000, 50_000_000, 1_000_000, 12_500_000, 12_500_000},
	"gpt-6-sol":       {2_000_000, 10_000_000, 200_000, 2_500_000, 2_500_000},
	"gpt-6-luna":      {100_000, 500_000, 10_000, 125_000, 125_000},
	// Z.ai publishes no cache-write rate, so writes bill at 1.0x input.
	"glm-5.3":       {1_400_000, 4_400_000, 260_000, 1_400_000, 1_400_000},
	"glm-5.3-flash": {150_000, 500_000, 30_000, 150_000, 150_000},
}

// TestPriceTableV10_PerCategoryRates pins every v10 row's resolved rate for each
// token category separately, so a correct total cannot hide two compensating errors.
func TestPriceTableV10_PerCategoryRates(t *testing.T) {
	loadDefaultPriceTable(t)
	const m = 1_000_000
	for model, want := range v10Rows {
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
	}
}

// TestPriceTableV10_RawIDsResolve pins the model strings the clients actually
// write. Claude Code logs claude-fable-5-1 / claude-opus-5-5 verbatim (measured in
// ~/.claude/projects on 2026-09-25), opencode writes "<providerID>/<model>", and
// Codex writes gpt-6-astra. Each must price exactly as its table row.
func TestPriceTableV10_RawIDsResolve(t *testing.T) {
	loadDefaultPriceTable(t)
	u := CostUsage{Input: 1_000_000, Output: 1_000_000, CacheRead: 1_000_000}
	cases := []struct{ raw, row string }{
		{"claude-fable-5-1", "claude-fable-5-1"},
		{"claude-opus-5-5", "claude-opus-5-5"},
		{"claude-opus-5-5-20260901", "claude-opus-5-5"},
		{"gpt-6-astra", "gpt-6-astra"},
		{"zai-coding-plan/glm-5.3", "glm-5.3"},
		{"ZAI-Coding-Plan/GLM-5.3-Flash", "glm-5.3-flash"},
	}
	for _, c := range cases {
		if got := NormalizeModel(c.raw); got != c.row {
			t.Errorf("NormalizeModel(%q) = %q, want %q", c.raw, got, c.row)
		}
		if got, want := ComputeCost(c.raw, u), ComputeCost(c.row, u); got != want {
			t.Errorf("ComputeCost(%q) = %d, want the %q row's %d", c.raw, got, c.row, want)
		}
	}
}

// TestNormalizeModel_StripsOnlyKnownProviderPrefixes pins that the prefix strip is
// an allowlist. A host-qualified open-weights key carries its own slash and must
// survive untouched, or every #268 row stops matching.
func TestNormalizeModel_StripsOnlyKnownProviderPrefixes(t *testing.T) {
	for _, raw := range []string{
		"meta-llama/llama-3.3-70b-instruct",
		"ollama-cloud/glm-5.2",
		"zai/glm-5.3",
	} {
		if got := NormalizeModel(raw); got != raw {
			t.Errorf("NormalizeModel(%q) = %q, want it unchanged", raw, got)
		}
	}
}

// TestPriceTableV10_GLMOnCodingPlanHostIsPerToken pins the operator ruling on #786:
// GLM usage on the coding plan is costed per token at list price. Opencode prices
// with host = its providerID; with no host-qualified row that falls to the
// model-only glm rows.
func TestPriceTableV10_GLMOnCodingPlanHostIsPerToken(t *testing.T) {
	loadDefaultPriceTable(t)
	for _, model := range []string{"glm-5.3", "glm-5.3-flash"} {
		u := CostUsage{Input: 1_000_000}
		got, mode := ComputeCostHost("zai-coding-plan", model, u)
		if got != v10Rows[model].input || mode != BillingPerToken {
			t.Errorf("ComputeCostHost(zai-coding-plan, %s) = (%d, %q), want (%d, %q)",
				model, got, mode, v10Rows[model].input, BillingPerToken)
		}
		if !IsAuditedRate("zai-coding-plan", model) {
			t.Errorf("%s on zai-coding-plan is not an audited rate", model)
		}
	}
}

// TestParsePriceTable_ZAIProvider pins the zai provider's parse: it is accepted,
// its defaults are 1.0x for reads and writes, and near-miss spellings are rejected
// like any other unknown provider.
func TestParsePriceTable_ZAIProvider(t *testing.T) {
	const fallbacks = "  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n" +
		"  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n" +
		"  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n"
	doc := func(provider string) []byte {
		return []byte("version: 1\neffective_date: \"2030-01-01\"\nmodels:\n" +
			"  m: {input_per_m: 1, output_per_m: 2, provider: " + provider + "}\n" + fallbacks)
	}

	tbl, _, err := parsePriceTable(doc("zai"))
	if err != nil {
		t.Fatalf("provider zai rejected: %v", err)
	}
	got := tbl["m"]
	if got.provider != providerZAI || got.cacheReadMult != 1.0 ||
		got.cacheWrite5mMult != 1.0 || got.cacheWrite1hMult != 1.0 || got.billingMode != BillingPerToken {
		t.Errorf("zai row resolved to %+v, want provider zai, read 1.0, writes 1.0, per_token", got)
	}

	for _, bad := range []string{"ZAI", "z.ai", "zai-coding-plan", "zhipu"} {
		if _, _, err := parsePriceTable(doc(bad)); err == nil || !strings.Contains(err.Error(), "invalid provider") {
			t.Errorf("provider %q: err = %v, want an invalid provider error", bad, err)
		}
	}
}
