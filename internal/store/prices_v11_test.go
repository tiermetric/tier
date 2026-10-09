package store

import (
	"strings"
	"testing"
)

// v11Rows is the price of 1M tokens of ONE category, in micro-dollars, for every
// row v11 (#895) added. Each figure is the per-million rate on Meta's pricing page
// (dev.meta.ai/docs/pricing-rate-limits, read 2026-09-27), not a product of the
// table's own multipliers, so a mistyped multiplier fails here. Meta publishes no
// cache-write rate, so both write classes bill at the input rate.
var v11Rows = map[string]v10PerCategoryMicro{
	"muse-spark-1.3":             {1_250_000, 4_250_000, 150_000, 1_250_000, 1_250_000},
	"muse-spark-1.2":             {1_250_000, 4_250_000, 150_000, 1_250_000, 1_250_000},
	"muse-spark-1.1":             {1_250_000, 4_250_000, 150_000, 1_250_000, 1_250_000},
	"muse-spark-1.3-contributor": {100_000, 200_000, 2_000, 100_000, 100_000},
	"muse-spark-1.2-contributor": {100_000, 200_000, 2_000, 100_000, 100_000},
}

// TestPriceTableV11_PerCategoryRates pins every v11 row's resolved rate for each
// token category separately, its per_token billing basis, and that it is audited.
func TestPriceTableV11_PerCategoryRates(t *testing.T) {
	loadDefaultPriceTable(t)
	const m = 1_000_000
	for model, want := range v11Rows {
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
			t.Errorf("%s: billing mode %q, want %q (operator ruling on #895: list price, not the subscription)",
				model, mode, BillingPerToken)
		}
	}
}

// TestPriceTableV11_MuseCallPricedExactly prices one mixed call (input, output and
// cached input together) on each tier, against a total worked by hand from the
// published rates. The token counts are chosen so each total is a whole number of
// micro-dollars, so rounding cannot hide an error.
func TestPriceTableV11_MuseCallPricedExactly(t *testing.T) {
	loadDefaultPriceTable(t)
	u := CostUsage{Input: 200_000, Output: 40_000, CacheRead: 1_000_000}
	cases := []struct {
		model string
		want  int64
	}{
		// 200k x $1.25 + 40k x $4.25 + 1M x $0.15 = $0.25 + $0.17 + $0.15.
		{"muse-spark-1.3", 570_000},
		// 200k x $0.10 + 40k x $0.20 + 1M x $0.002 = $0.02 + $0.008 + $0.002.
		{"muse-spark-1.3-contributor", 30_000},
	}
	for _, c := range cases {
		if got := ComputeCost(c.model, u); got != c.want {
			t.Errorf("ComputeCost(%s, %+v) = %d micro-dollars, want %d", c.model, u, got, c.want)
		}
	}
}

// TestPriceTableV11_ContributorIsNotTheStandardRow pins that the Contributor ids
// resolve to their OWN rows. A normalization that folded "-contributor" away, or a
// table that aliased the two, would bill Contributor usage 12.5x too high on input.
func TestPriceTableV11_ContributorIsNotTheStandardRow(t *testing.T) {
	loadDefaultPriceTable(t)
	u := CostUsage{Input: 1_000_000, Output: 1_000_000, CacheRead: 1_000_000}
	for _, pair := range [][2]string{
		{"muse-spark-1.3", "muse-spark-1.3-contributor"},
		{"muse-spark-1.2", "muse-spark-1.2-contributor"},
	} {
		std, contrib := pair[0], pair[1]
		if NormalizeModel(contrib) != contrib {
			t.Errorf("NormalizeModel(%q) = %q, want it unchanged", contrib, NormalizeModel(contrib))
		}
		if s, c := ComputeCost(std, u), ComputeCost(contrib, u); s == c {
			t.Errorf("%s and %s both price 1M/1M/1M at %d micro-dollars; the tiers must be distinct", std, contrib, s)
		}
	}
}

// TestPriceTableV11_RawIDsResolve pins the model strings Muse writes. Muse Code
// 1.4.0 logs "muse-spark-1.3-contributor" verbatim (#895, measured on a workstation);
// NormalizeModel lowercases, trims and strips a date suffix like any other id.
func TestPriceTableV11_RawIDsResolve(t *testing.T) {
	loadDefaultPriceTable(t)
	u := CostUsage{Input: 1_000_000, Output: 1_000_000, CacheRead: 1_000_000}
	cases := []struct{ raw, row string }{
		{"muse-spark-1.3-contributor", "muse-spark-1.3-contributor"},
		{"MUSE-Spark-1.3-Contributor", "muse-spark-1.3-contributor"},
		{"  muse-spark-1.2-contributor\n", "muse-spark-1.2-contributor"},
		{"muse-spark-1.3", "muse-spark-1.3"},
		{"muse-spark-1.3-20260901", "muse-spark-1.3"},
		{"muse-spark-1.1-latest", "muse-spark-1.1"},
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

// TestPriceTableV11_NoUnpublishedContributorTier pins that the table does not
// invent a rate Meta does not publish: there is no muse-spark-1.1 Contributor tier,
// so that id is NOT audited (it takes the visible guessed fallback) rather than
// inheriting either published row.
func TestPriceTableV11_NoUnpublishedContributorTier(t *testing.T) {
	loadDefaultPriceTable(t)
	if IsAuditedRate(HostUnknown, "muse-spark-1.1-contributor") {
		t.Error("muse-spark-1.1-contributor is priced as audited, but Meta publishes no rate for it")
	}
}

// TestParsePriceTable_MetaProvider pins the meta provider's parse: it is accepted,
// its defaults are 1.0x for reads and writes, it bills per_token, and near-miss
// spellings are rejected like any other unknown provider.
func TestParsePriceTable_MetaProvider(t *testing.T) {
	const fallbacks = "  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n" +
		"  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n" +
		"  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n"
	doc := func(provider string) []byte {
		return []byte("version: 1\neffective_date: \"2030-01-01\"\nmodels:\n" +
			"  m: {input_per_m: 1, output_per_m: 2, provider: " + provider + "}\n" + fallbacks)
	}

	tbl, _, err := parsePriceTable(doc("meta"))
	if err != nil {
		t.Fatalf("provider meta rejected: %v", err)
	}
	got := tbl["m"]
	if got.provider != providerMeta || got.cacheReadMult != 1.0 ||
		got.cacheWrite5mMult != 1.0 || got.cacheWrite1hMult != 1.0 || got.billingMode != BillingPerToken {
		t.Errorf("meta row resolved to %+v, want provider meta, read 1.0, writes 1.0, per_token", got)
	}

	for _, bad := range []string{"Meta", "meta-llama", "meta.ai", "facebook"} {
		if _, _, err := parsePriceTable(doc(bad)); err == nil || !strings.Contains(err.Error(), "invalid provider") {
			t.Errorf("provider %q: err = %v, want an invalid provider error", bad, err)
		}
	}
}
