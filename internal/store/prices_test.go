package store

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"math"
	"strings"
	"sync"
	"testing"
)

func TestNormalizeModel(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"claude-sonnet-4-20250514", "claude-sonnet-4"},
		{"claude-opus-4-6", "claude-opus-4-6"},
		{"gpt-4o-2024-05-13", "gpt-4o"},
		{"gemini-2.5-pro-preview", "gemini-2.5-pro"},
		{"CLAUDE-SONNET-4", "claude-sonnet-4"},
		{"gpt-4o-latest", "gpt-4o"},
		// A `-1m` context suffix is deliberately NOT stripped (#4): long-context
		// pricing is modelled as an input-size threshold on the base model, not a
		// suffix alias. An unrecognized suffix stays intact so it either matches an
		// explicit entry or fails loud (WARN + fallback) — never silently collapses.
		{"claude-sonnet-4-1m", "claude-sonnet-4-1m"},
	}
	for _, c := range cases {
		got := NormalizeModel(c.raw)
		if got != c.want {
			t.Errorf("NormalizeModel(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestComputeCost(t *testing.T) {
	// Existing assertions sometimes route an unknown model through the
	// fallback path; suppress its WARN log so test output stays clean.
	silenceUnknownModelLogger(t)

	// Claude Sonnet 4: $3/M input, $15/M output
	// 1M input + 1M output = $3 + $15 = $18
	cost := computeCostUSD("claude-sonnet-4", CostUsage{Input: 1_000_000, Output: 1_000_000})
	if math.Abs(cost-18.0) > 0.001 {
		t.Errorf("ComputeCost(claude-sonnet-4, 1M, 1M) = %.4f, want 18.00", cost)
	}

	// Anthropic cache read: 0.1x base input rate.
	// 500k input ($1.50) + 500k cache_read (500k * $3/M * 0.1 = $0.15) = $1.65
	cost = computeCostUSD("claude-sonnet-4", CostUsage{Input: 500_000, CacheRead: 500_000})
	if math.Abs(cost-1.65) > 0.001 {
		t.Errorf("ComputeCost with anthropic cache_read = %.4f, want 1.65 (input + 0.1x read)", cost)
	}

	// Self-hosted large (combined rate $2/M): 500k in + 500k out = 1M * $2/M = $2
	cost = computeCostUSD("self-hosted-large", CostUsage{Input: 500_000, Output: 500_000})
	if math.Abs(cost-2.0) > 0.001 {
		t.Errorf("ComputeCost(self-hosted-large) = %.4f, want 2.00", cost)
	}

	// Unknown model falls back to self-hosted-medium ($0.50/M combined).
	cost = computeCostUSD("unknown-model-xyz", CostUsage{Input: 1_000_000})
	if math.Abs(cost-0.50) > 0.001 {
		t.Errorf("ComputeCost(unknown) = %.4f, want 0.50", cost)
	}

	// Minor versions do NOT inherit the base-family rate (#80): Opus 4.0 is
	// $15/$75, but Opus 4.6/4.7/4.8 re-priced down to $5/$25. The old test
	// asserted opus-4 == opus-4-6; that equivalence is exactly the falsified
	// assumption a family-fallback would bake in, so assert they DIFFER.
	opus40 := computeCostUSD("claude-opus-4", CostUsage{Input: 1_000_000, Output: 1_000_000})
	if math.Abs(opus40-90.0) > 0.001 {
		t.Errorf("ComputeCost(claude-opus-4, 1M, 1M) = %.4f, want 90.00 ($15+$75)", opus40)
	}
	// Opus 4.6: $5/$25 → 1M + 1M = $30.
	opus46 := computeCostUSD("claude-opus-4-6", CostUsage{Input: 1_000_000, Output: 1_000_000})
	if math.Abs(opus46-30.0) > 0.001 {
		t.Errorf("ComputeCost(claude-opus-4-6, 1M, 1M) = %.4f, want 30.00 ($5+$25)", opus46)
	}
	if math.Abs(opus40-opus46) < 0.001 {
		t.Errorf("claude-opus-4 ($15/$75) and claude-opus-4-6 ($5/$25) must differ: %v vs %v", opus40, opus46)
	}

	// Opus 4.7: $5/$25 → 1M + 1M = $30 (must NOT fall through to self-hosted-medium $0.50).
	cost = computeCostUSD("claude-opus-4-7", CostUsage{Input: 1_000_000, Output: 1_000_000})
	if math.Abs(cost-30.0) > 0.001 {
		t.Errorf("ComputeCost(claude-opus-4-7, 1M, 1M) = %.4f, want 30.00 — likely fell through to self-hosted-medium fallback", cost)
	}

	// Opus 4.8: $5/$25 → 1M + 1M = $30. Pinned so a dropped entry can't silently
	// revert to the $0.50 self-hosted fallback (#80).
	opus48 := computeCostUSD("claude-opus-4-8", CostUsage{Input: 1_000_000, Output: 1_000_000})
	if math.Abs(opus48-30.0) > 0.001 {
		t.Errorf("ComputeCost(claude-opus-4-8, 1M, 1M) = %.4f, want 30.00 ($5+$25)", opus48)
	}

	// Fable 5: distinct family at $10/$50 → 1M + 1M = $60. A family-fallback could
	// never price this; only an explicit entry can (#80).
	fable5 := computeCostUSD("claude-fable-5", CostUsage{Input: 1_000_000, Output: 1_000_000})
	if math.Abs(fable5-60.0) > 0.001 {
		t.Errorf("ComputeCost(claude-fable-5, 1M, 1M) = %.4f, want 60.00 ($10+$50)", fable5)
	}

	// Haiku 4.5: $1/$5 → 1M + 1M = $6 (corrected from the stale $0.80/$4, #80).
	haiku45 := computeCostUSD("claude-haiku-4-5", CostUsage{Input: 1_000_000, Output: 1_000_000})
	if math.Abs(haiku45-6.0) > 0.001 {
		t.Errorf("ComputeCost(claude-haiku-4-5, 1M, 1M) = %.4f, want 6.00 ($1+$5)", haiku45)
	}

	// Sonnet 5: list $3/$15 → 1M + 1M = $18. Pinned so a dropped entry can't
	// silently revert to the $0.50 self-hosted guess (the model this refresh adds).
	sonnet5 := computeCostUSD("claude-sonnet-5", CostUsage{Input: 1_000_000, Output: 1_000_000})
	if math.Abs(sonnet5-18.0) > 0.001 {
		t.Errorf("ComputeCost(claude-sonnet-5, 1M, 1M) = %.4f, want 18.00 ($3+$15) — likely fell through to self-hosted-medium fallback", sonnet5)
	}

	// Mythos 5: same $10/$50 flagship rate as Fable 5 → 1M + 1M = $60. Its own
	// audited entry; a family-fallback could never price it (#80).
	mythos5 := computeCostUSD("claude-mythos-5", CostUsage{Input: 1_000_000, Output: 1_000_000})
	if math.Abs(mythos5-60.0) > 0.001 {
		t.Errorf("ComputeCost(claude-mythos-5, 1M, 1M) = %.4f, want 60.00 ($10+$50)", mythos5)
	}

	// Sonnet 4.7: 1M input + 1M output = $3 + $15 = $18 (matches Sonnet 4 / 4.6 pinned rate).
	cost = computeCostUSD("claude-sonnet-4-7", CostUsage{Input: 1_000_000, Output: 1_000_000})
	if math.Abs(cost-18.0) > 0.001 {
		t.Errorf("ComputeCost(claude-sonnet-4-7, 1M, 1M) = %.4f, want 18.00", cost)
	}
	sonnet4 := computeCostUSD("claude-sonnet-4", CostUsage{Input: 10_000, Output: 10_000})
	sonnet47 := computeCostUSD("claude-sonnet-4-7", CostUsage{Input: 10_000, Output: 10_000})
	if math.Abs(sonnet4-sonnet47) > 0.0001 {
		t.Errorf("claude-sonnet-4 and claude-sonnet-4-7 should cost the same: %v vs %v", sonnet4, sonnet47)
	}

	// Date-suffixed variant must normalize and resolve to the same rate as the
	// bare key — exercises the NormalizeModel + priceTable lookup as a unit.
	costDated := computeCostUSD("claude-opus-4-7-20260301", CostUsage{Input: 1_000_000, Output: 1_000_000})
	if math.Abs(costDated-30.0) > 0.001 {
		t.Errorf("ComputeCost(claude-opus-4-7-20260301) = %.4f, want 30.00 — normalizer + table did not resolve as a unit", costDated)
	}

	// Uppercase variant must lowercase via NormalizeModel and resolve correctly.
	costUpper := computeCostUSD("CLAUDE-SONNET-4-7", CostUsage{Input: 1_000_000, Output: 1_000_000})
	if math.Abs(costUpper-18.0) > 0.001 {
		t.Errorf("ComputeCost(CLAUDE-SONNET-4-7) = %.4f, want 18.00 — case-folding broken", costUpper)
	}

	// Anthropic 5-minute cache write: 1.25x base input rate.
	// 1M cache_write_5m at Opus 4.7 ($5/M input) = 1M * $5/M * 1.25 = $6.25
	cost5m := computeCostUSD("claude-opus-4-7", CostUsage{CacheWrite5m: 1_000_000})
	if math.Abs(cost5m-6.25) > 0.001 {
		t.Errorf("ComputeCost(claude-opus-4-7, cache_write_5m=1M) = %.4f, want 6.25 (1.25x input)", cost5m)
	}

	// Anthropic 1-hour cache write: 2x base input rate.
	// 1M cache_write_1h at Opus 4.7 = 1M * $5/M * 2.0 = $10
	cost1h := computeCostUSD("claude-opus-4-7", CostUsage{CacheWrite1h: 1_000_000})
	if math.Abs(cost1h-10.0) > 0.001 {
		t.Errorf("ComputeCost(claude-opus-4-7, cache_write_1h=1M) = %.4f, want 10.00 (2.0x input)", cost1h)
	}

	// Anthropic cache read: 0.1x base input rate on Opus 4.7.
	// 1M cache_read = 1M * $5/M * 0.1 = $0.50
	costReadOpus := computeCostUSD("claude-opus-4-7", CostUsage{CacheRead: 1_000_000})
	if math.Abs(costReadOpus-0.50) > 0.001 {
		t.Errorf("ComputeCost(claude-opus-4-7, cache_read=1M) = %.4f, want 0.50 (0.1x input)", costReadOpus)
	}

	// Mixed scenario: full TTL split + read + uncached input + output, all in one call.
	// Opus 4.7: $5/M input, $25/M output.
	//   input    100k    * $5/M            = $0.50
	//   read     200k    * $5/M * 0.1      = $0.10
	//   write5m  100k    * $5/M * 1.25     = $0.625
	//   write1h   50k    * $5/M * 2.0      = $0.50
	//   output    50k    * $25/M           = $1.25
	//   total = $2.975
	costMixed := computeCostUSD("claude-opus-4-7", CostUsage{
		Input:        100_000,
		Output:       50_000,
		CacheRead:    200_000,
		CacheWrite5m: 100_000,
		CacheWrite1h: 50_000,
	})
	if math.Abs(costMixed-2.975) > 0.001 {
		t.Errorf("ComputeCost mixed-scenario = %.4f, want 2.975 — multiplier table broken", costMixed)
	}

	// OpenAI cached_tokens: 0.5x input rate (NOT 1.0x — that was the pre-#55 bug).
	// gpt-4o: $2.50/M input. 1M cache_read = 1M * $2.50/M * 0.5 = $1.25.
	costOpenAICache := computeCostUSD("gpt-4o", CostUsage{CacheRead: 1_000_000})
	if math.Abs(costOpenAICache-1.25) > 0.001 {
		t.Errorf("ComputeCost(gpt-4o, cache_read=1M) = %.4f, want 1.25 (0.5x input)", costOpenAICache)
	}

	// OpenAI never populates cache writes; if a caller passes them anyway, the
	// 1.0x default multiplier applies — but only because they're not part of
	// any real OpenAI request. Contract sanity check: same 1M placed into
	// CacheWrite5m vs CacheRead on gpt-4o produces different numbers, proving
	// the provider switch isn't silently ignoring buckets.
	costOpenAIWriteDefault := computeCostUSD("gpt-4o", CostUsage{CacheWrite5m: 1_000_000})
	if math.Abs(costOpenAIWriteDefault-2.50) > 0.001 {
		t.Errorf("ComputeCost(gpt-4o, cache_write_5m=1M) = %.4f, want 2.50 (1.0x — no OpenAI write SKU)", costOpenAIWriteDefault)
	}

	// Self-hosted entries do NOT apply cache multipliers — combined rate is
	// total / $rate, no provider switch. 1M cache_read on self-hosted-large
	// ($2/M combined) = $2, not $0.20.
	costSelfHostedCache := computeCostUSD("self-hosted-large", CostUsage{CacheRead: 1_000_000})
	if math.Abs(costSelfHostedCache-2.0) > 0.001 {
		t.Errorf("ComputeCost(self-hosted-large, cache_read=1M) = %.4f, want 2.00 (combined rate, no discount)", costSelfHostedCache)
	}

	// Contrapositive: the new entries must NOT be priced at the self-hosted-medium
	// fallback rate. If a future refactor accidentally drops the new keys, a
	// straight equality test against $90.00 would still fail, but this assertion
	// makes the regression mode explicit.
	fallbackCost := computeCostUSD("definitely-not-a-real-model", CostUsage{Input: 1_000_000})
	opus47Cost := computeCostUSD("claude-opus-4-7", CostUsage{Input: 1_000_000})
	if math.Abs(opus47Cost-fallbackCost) < 0.001 {
		t.Errorf("claude-opus-4-7 must not price at the fallback rate: opus47=%v fallback=%v", opus47Cost, fallbackCost)
	}
}

// TestComputeCost_RPT2026Corrections pins the RPT-2026 §4 price corrections
// (#115): each corrected model must price at its verified March-2026 rate.
// These FAIL on the pre-correction table (o3 at $10/$40, deepseek-r1 swapped,
// etc.) and pass once prices.yaml carries the corrected values. Money is
// asserted in exact integer micro-dollars (#69).
//
// Token counts are deliberately ASYMMETRIC (1M input, 2M output) so the input
// and output rates are pinned INDEPENDENTLY — a symmetric 1M+1M call is
// invariant to swapping the two rates and so could not catch the deepseek-r1
// input/output swap this correction un-does. want = input_per_m×1 + output_per_m×2
// (in micro-dollars).
func TestComputeCost_RPT2026Corrections(t *testing.T) {
	silenceUnknownModelLogger(t)
	usage := CostUsage{Input: 1_000_000, Output: 2_000_000}
	cases := []struct {
		name  string
		model string
		want  int64 // micro-dollars: input_per_m + 2×output_per_m
	}{
		// o3: $2.00/$8.00 (RPT §1 line 56) — was $10/$40. 2.00 + 2×8.00 = $18.00.
		{"o3 corrected to 2.00/8.00", "o3", 18_000_000},
		// o3-mini: $0.55/$2.20 (RPT §1 line 83) — was $1.10/$4.40. 0.55 + 2×2.20 = $4.95.
		{"o3-mini corrected to 0.55/2.20", "o3-mini", 4_950_000},
		// gemini-2.5-flash: $0.30/$2.50 (RPT §4) — was $0.15/$0.60. 0.30 + 2×2.50 = $5.30.
		{"gemini-2.5-flash corrected to 0.30/2.50", "gemini-2.5-flash", 5_300_000},
		// deepseek-r1: input $0.55, output $2.19 (RPT §4 un-swap) — was $2.19/$8.19.
		// 0.55 + 2×2.19 = $4.93; a still-swapped 2.19/0.55 table would give $3.29.
		{"deepseek-r1 un-swapped to 0.55/2.19", "deepseek-r1", 4_930_000},
		// deepseek-v3: $0.28/$0.42 (RPT §4) — was $0.27/$1.10. 0.28 + 2×0.42 = $1.12.
		{"deepseek-v3 corrected to 0.28/0.42", "deepseek-v3", 1_120_000},
		// gpt-4.1-nano: $0.05/$0.20 (RPT §4) — was $0.10/$0.40. 0.05 + 2×0.20 = $0.45.
		{"gpt-4.1-nano corrected to 0.05/0.20", "gpt-4.1-nano", 450_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ComputeCost(c.model, usage)
			if got != c.want {
				t.Errorf("ComputeCost(%s, 1M in + 2M out) = %d micro, want %d", c.model, got, c.want)
			}
		})
	}
}

// TestComputeCost_GPT45FallsBackToUnknownModel pins that the retired gpt-4.5
// entry (RPT §4: "GPT-4.5 retired") is GONE from the table (#115): a gpt-4.5
// call now misses the table and takes the self-hosted-medium fallback
// ($0.50/M combined) rather than the stale $75/$150 entry. Fails on main where
// the entry still exists (1M input would price at $75 = 75_000_000 micro).
func TestComputeCost_GPT45FallsBackToUnknownModel(t *testing.T) {
	silenceUnknownModelLogger(t)
	if got := ComputeCost("gpt-4.5", CostUsage{Input: 1_000_000}); got != 500_000 {
		t.Errorf("ComputeCost(gpt-4.5, 1M in) = %d micro, want 500_000 (entry retired → self-hosted-medium fallback)", got)
	}
}

// computeCostUSD wraps ComputeCost (which since #69 returns integer
// micro-dollars) back into float dollars, so the value-based assertions in this
// file keep reading in the dollar units the price table is documented in.
func computeCostUSD(model string, u CostUsage) float64 {
	return MicroToDollars(ComputeCost(model, u))
}

// TestComputeCostLongContextTier pins the #4 threshold-aware pricing: a model
// with a context_threshold re-prices the whole request at its over-tier once the
// input-side context (input + all cache classes) exceeds the threshold. The
// boundary is strict (>threshold) and output bills at the premium rate too.
func TestComputeCostLongContextTier(t *testing.T) {
	silenceUnknownModelLogger(t)

	// gemini-2.5-pro: base $1.25/$10, over-200K $2.50/$15, no cache discount (1.0x).

	// At exactly the threshold, the base rate still applies (strict > boundary).
	if got := computeCostUSD("gemini-2.5-pro", CostUsage{Input: 200_000}); math.Abs(got-0.25) > 0.001 {
		t.Errorf("gemini-2.5-pro at 200K input = %.4f, want 0.25 (base rate AT the boundary)", got)
	}
	// One token over flips the request to the premium input rate.
	wantOver := 200_001.0 / 1_000_000 * 2.50
	if got := computeCostUSD("gemini-2.5-pro", CostUsage{Input: 200_001}); math.Abs(got-wantOver) > 0.001 {
		t.Errorf("gemini-2.5-pro at 200001 input = %.4f, want %.4f (premium input rate)", got, wantOver)
	}
	// Output also bills at the premium output rate when over-threshold: 1M in + 1M out.
	if got := computeCostUSD("gemini-2.5-pro", CostUsage{Input: 1_000_000, Output: 1_000_000}); math.Abs(got-17.50) > 0.001 {
		t.Errorf("gemini-2.5-pro 1M in + 1M out = %.4f, want 17.50 (premium in+out $2.50+$15)", got)
	}
	// Sub-threshold call uses base for both classes.
	wantUnder := 100_000.0/1_000_000*1.25 + 100_000.0/1_000_000*10.00
	if got := computeCostUSD("gemini-2.5-pro", CostUsage{Input: 100_000, Output: 100_000}); math.Abs(got-wantUnder) > 0.001 {
		t.Errorf("gemini-2.5-pro 100K in + 100K out = %.4f, want %.4f (base in+out)", got, wantUnder)
	}
	// Cache tokens count toward the input-context threshold: 150K input + 100K
	// cache_read = 250K > 200K, so BOTH the input and the read bill off the
	// premium input rate. The read additionally carries Gemini 2.5+ implicit
	// caching's 0.25x discount (#122), scaled off the SELECTED premium rate.
	wantCache := 150_000.0/1_000_000*2.50 + 100_000.0/1_000_000*2.50*0.25
	if got := computeCostUSD("gemini-2.5-pro", CostUsage{Input: 150_000, CacheRead: 100_000}); math.Abs(got-wantCache) > 0.001 {
		t.Errorf("gemini-2.5-pro 150K in + 100K cache_read = %.4f, want %.4f (cache counts toward threshold, read 0.25x off premium)", got, wantCache)
	}
	// The cache-WRITE classes count toward the threshold too (not just reads):
	// 150K input + 100K write5m and + 100K write1h each cross 200K. Google has
	// no cache-WRITE discount (write multipliers default 1.0x), so both bill at
	// the premium input rate.
	wantWrite5m := 150_000.0/1_000_000*2.50 + 100_000.0/1_000_000*2.50
	if got := computeCostUSD("gemini-2.5-pro", CostUsage{Input: 150_000, CacheWrite5m: 100_000}); math.Abs(got-wantWrite5m) > 0.001 {
		t.Errorf("gemini-2.5-pro 150K in + 100K cache_write_5m = %.4f, want %.4f (write5m counts toward threshold)", got, wantWrite5m)
	}
	wantWrite1h := 150_000.0/1_000_000*2.50 + 100_000.0/1_000_000*2.50
	if got := computeCostUSD("gemini-2.5-pro", CostUsage{Input: 150_000, CacheWrite1h: 100_000}); math.Abs(got-wantWrite1h) > 0.001 {
		t.Errorf("gemini-2.5-pro 150K in + 100K cache_write_1h = %.4f, want %.4f (write1h counts toward threshold)", got, wantWrite1h)
	}
	// Output tokens do NOT count toward the threshold: a huge output with tiny
	// input stays on the BASE tier (input-context = 0 ≤ 200K), billed at $10/M
	// output — NOT the $15/M premium.
	if got := computeCostUSD("gemini-2.5-pro", CostUsage{Output: 1_000_000}); math.Abs(got-10.00) > 0.001 {
		t.Errorf("gemini-2.5-pro 0 in + 1M out = %.4f, want 10.00 (output does not trigger the tier)", got)
	}

	// gemini-3.1-pro (base $2/$12, over $4/$18) — exercise the SECOND Google
	// threshold entry on the premium path so its distinct rates are pinned.
	if got := computeCostUSD("gemini-3.1-pro", CostUsage{Input: 300_000, Output: 10_000}); math.Abs(got-(300_000.0/1_000_000*4.00+10_000.0/1_000_000*18.00)) > 0.001 {
		t.Errorf("gemini-3.1-pro 300K in + 10K out = %.4f, want %.4f (premium $4/$18)", got, 300_000.0/1_000_000*4.00+10_000.0/1_000_000*18.00)
	}

	// claude-sonnet-4-5: 1M beta premium $6/$22.50 above 200K, with Anthropic cache
	// multipliers scaling off the SELECTED (premium) input rate. 300K in + 10K out.
	wantSonnet := 300_000.0/1_000_000*6.00 + 10_000.0/1_000_000*22.50
	if got := computeCostUSD("claude-sonnet-4-5", CostUsage{Input: 300_000, Output: 10_000}); math.Abs(got-wantSonnet) > 0.001 {
		t.Errorf("claude-sonnet-4-5 300K in + 10K out = %.4f, want %.4f (1M-beta premium)", got, wantSonnet)
	}
	// Anthropic 0.1x cache read scales off the premium $6 input rate above threshold.
	wantSC := 300_000.0/1_000_000*6.00 + 1_000_000.0/1_000_000*6.00*0.10
	if got := computeCostUSD("claude-sonnet-4-5", CostUsage{Input: 300_000, CacheRead: 1_000_000}); math.Abs(got-wantSC) > 0.001 {
		t.Errorf("claude-sonnet-4-5 300K in + 1M cache_read = %.4f, want %.4f (0.1x read on premium base)", got, wantSC)
	}

	// A flat model (no threshold) is unaffected even far above 200K.
	if got := computeCostUSD("claude-sonnet-4", CostUsage{Input: 1_000_000}); math.Abs(got-3.00) > 0.001 {
		t.Errorf("claude-sonnet-4 (no threshold) 1M input = %.4f, want 3.00 (flat, unaffected)", got)
	}
}

// installPriceTable swaps the package price table to a synthetic one parsed
// from yamlDoc for the duration of the calling test, restoring the default on
// cleanup. NOT for t.Parallel() tests: priceTable is a plain global
// (WRITE-ONCE-BEFORE-SERVE), so this swap-and-restore is only race-free because
// non-parallel tests run to completion before the parallel batch resumes.
func installPriceTable(t *testing.T, yamlDoc string) {
	t.Helper()
	tbl, info, err := parsePriceTable([]byte(yamlDoc))
	if err != nil {
		t.Fatalf("parsePriceTable(synthetic): %v", err)
	}
	prevTbl, prevInfo := priceTable, activePriceTableInfo
	priceTable = tbl
	activePriceTableInfo = info
	t.Cleanup(func() {
		priceTable = prevTbl
		activePriceTableInfo = prevInfo
	})
}

// TestComputeCost_ThresholdCountsNonOverlappingInputOnce documents the tier
// selector's contract after #114: callers pass NON-overlapping token classes,
// so Input+CacheRead equals the provider's true input-side context exactly
// once. A 150K-input + 120K-cache-read request (270K real prompt) crosses a
// 200K threshold; the pre-fix overlapping encoding — which stored the SAME 270K
// prompt as Input=270K plus a redundant CacheRead=120K (390K combined) — is no
// longer producible by the parsers, so the selector cannot over-count it.
func TestComputeCost_ThresholdCountsNonOverlappingInputOnce(t *testing.T) {
	silenceUnknownModelLogger(t)
	// Synthetic OpenAI model with a 200K long-context threshold (no OpenAI
	// model in the real table carries one) so we exercise the 0.5x cache-read
	// multiplier together with the tier selector. parsePriceTable requires the
	// three self-hosted fallback keys, so include them.
	installPriceTable(t, `
version: 99
effective_date: 2026-07-02
models:
  synthetic-openai-lc: { input_per_m: 2.50, output_per_m: 10.00, context_threshold: 200000, input_per_m_over: 5.00, output_per_m_over: 20.00, provider: openai }
  self-hosted-large: { input_per_m: 1.00, output_per_m: 2.00, provider: self-hosted }
  self-hosted-medium: { input_per_m: 0.50, output_per_m: 1.00, provider: self-hosted }
  self-hosted-small: { input_per_m: 0.20, output_per_m: 0.40, provider: self-hosted }
`)

	// 150K input + 120K cache_read = 270K > 200K → premium rates. OpenAI read
	// mult 0.5 scales off the premium $5.00 input rate.
	// in  = 150000/1e6 * 5.00           = 0.75
	// read= 120000/1e6 * 5.00 * 0.5     = 0.30      → total $1.05 = 1_050_000 micro
	over := ComputeCost("synthetic-openai-lc", CostUsage{Input: 150_000, CacheRead: 120_000})
	if want := DollarsToMicro(1.05); over != want {
		t.Errorf("270K non-overlapping input: cost = %d micro, want %d (premium tier, cached once)", over, want)
	}

	// Exactly 200K (150K input + 50K cache_read) does NOT cross (strict > ).
	// in  = 150000/1e6 * 2.50           = 0.375
	// read=  50000/1e6 * 2.50 * 0.5     = 0.0625    → total $0.4375 = 437_500 micro
	atBoundary := ComputeCost("synthetic-openai-lc", CostUsage{Input: 150_000, CacheRead: 50_000})
	if want := DollarsToMicro(0.4375); atBoundary != want {
		t.Errorf("200K combined at boundary: cost = %d micro, want %d (base tier)", atBoundary, want)
	}
}

// TestComputeCostReturnsExactMicro pins the #69 contract at micro precision: the
// other TestComputeCost assertions go through computeCostUSD (a 0.001-dollar
// tolerance, far coarser than the ≤0.5-micro rounding), so they cannot catch a
// regression in the round-once-at-the-micro-boundary behaviour. These assert the
// exact int64 micro return directly.
func TestComputeCostReturnsExactMicro(t *testing.T) {
	silenceUnknownModelLogger(t)
	cases := []struct {
		name  string
		model string
		usage CostUsage
		want  int64
	}{
		// Sonnet 4 ($3/$15 per M): 1M in + 1M out = $18 = 18_000_000 micro.
		{"sonnet 1M+1M", "claude-sonnet-4", CostUsage{Input: 1_000_000, Output: 1_000_000}, 18_000_000},
		// Opus 4.7 ($5/M) 1M cache_read at 0.1x = $0.50 = 500_000 micro.
		{"opus cache_read 0.1x", "claude-opus-4-7", CostUsage{CacheRead: 1_000_000}, 500_000},
		// Small Sonnet call: 100 in + 50 out = $0.0003 + $0.00075 = $0.00105 = 1_050 micro.
		{"sonnet small", "claude-sonnet-4", CostUsage{Input: 100, Output: 50}, 1_050},
		// Self-hosted-large combined $2/M: 500k+500k = $2 = 2_000_000 micro.
		{"self-hosted combined", "self-hosted-large", CostUsage{Input: 500_000, Output: 500_000}, 2_000_000},
		// Long-context over-tier (#4): gemini-2.5-pro >200K bills at $2.50/$15;
		// 1M in + 1M out = $17.50 = 17_500_000 micro (round-once on the premium path).
		{"gemini over-tier 1M+1M", "gemini-2.5-pro", CostUsage{Input: 1_000_000, Output: 1_000_000}, 17_500_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ComputeCost(c.model, c.usage); got != c.want {
				t.Errorf("ComputeCost(%s) = %d micro, want %d", c.model, got, c.want)
			}
		})
	}
}

// silenceUnknownModelLogger replaces the package-level logger with one that
// discards output for the duration of the calling test. Restores on cleanup.
func silenceUnknownModelLogger(t *testing.T) {
	t.Helper()
	orig := unknownModelLogger.Load()
	unknownModelLogger.Store(log.New(io.Discard, "", 0))
	t.Cleanup(func() { unknownModelLogger.Store(orig) })
}

// captureUnknownModelLogger redirects the unknown-model logger into a buffer
// for the duration of the calling test and returns the buffer. Restores on
// cleanup. Use resetUnknownModelDedupe to ensure a clean dedupe-cache state.
func captureUnknownModelLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	orig := unknownModelLogger.Load()
	var buf bytes.Buffer
	unknownModelLogger.Store(log.New(&buf, "", 0))
	t.Cleanup(func() { unknownModelLogger.Store(orig) })
	return &buf
}

// resetUnknownModelDedupe fully clears the WARN dedupe cache so the calling
// test starts from a deterministic state regardless of prior tests in the same
// process. Safe to call from any test order, including under -shuffle.
func resetUnknownModelDedupe(t *testing.T) {
	t.Helper()
	unknownModelSeen.Range(func(k, _ any) bool {
		unknownModelSeen.Delete(k)
		return true
	})
	unknownModelSeenCount.Store(0)
	unknownModelWarnSuppressed.Store(false)
}

func TestComputeCostWarnsOnUnknownModel(t *testing.T) {
	// NormalizeModel strips trailing 4/6/8-digit suffixes, so the test name
	// must use a suffix that survives normalization to assert the WARN
	// includes the post-normalization model string verbatim.
	const unknown = "acme-llm-x"
	resetUnknownModelDedupe(t)
	buf := captureUnknownModelLogger(t)

	cost := computeCostUSD(unknown, CostUsage{Input: 1_000_000})
	// Cost must still equal the self-hosted-medium fallback ($0.50/M).
	if math.Abs(cost-0.50) > 0.001 {
		t.Errorf("ComputeCost(%q) = %.4f, want 0.50 fallback", unknown, cost)
	}
	got := buf.String()
	if !strings.Contains(got, unknown) {
		t.Errorf("expected WARN log to mention %q, got: %q", unknown, got)
	}
	// Pin both the fallback class name AND the dollar rate to catch silent
	// changes to either the message format or the underlying rate constant.
	if !strings.Contains(got, "self-hosted-medium") {
		t.Errorf("expected WARN log to mention fallback class, got: %q", got)
	}
	if !strings.Contains(got, "$0.50/M") {
		t.Errorf("expected WARN log to pin the $0.50/M fallback rate, got: %q", got)
	}
	// #286: the self-hosted-medium price is a GUESS, not an audited rate. The WARN
	// must frame it that way (the data_quality story) rather than as a bare
	// "fallback" that reads like a normal, trusted code path.
	if !strings.Contains(strings.ToLower(got), "guess") {
		t.Errorf("expected WARN log to frame the price as a guess, got: %q", got)
	}
}

// TestUnknownModelWarnIsLengthBounded pins the #321-review finding on this sink,
// which is a DIFFERENT bound from the one #286 pins below.
//
// #286 caps how many DISTINCT models get a WARN. Nothing capped how BIG one WARN
// could be, and this sink has no fallback protection: prices.go's `import "log"`
// is the only one in any non-test file in the tree, so these two warnings never
// touch slog. An upstream model string of a megabyte produced a megabyte record.
//
// %q was never the missing piece — it does escape CR/LF, so this was a flood and
// not a forgery — which is exactly why it survived review for so long. The fix is
// logsafe.Str, whose cap bounds the RENDERED width.
//
// GUARD COVERAGE: revert either warnUnknownModel or warnHeuristicModel to
// `%q, norm` and this test fails on the corresponding subtest.
func TestUnknownModelWarnIsLengthBounded(t *testing.T) {
	cases := []struct {
		name string
		// model picks the warn path: the size-class heuristic fires when the
		// name embeds a parameter count, the flat fallback otherwise.
		model string
	}{
		{name: "flat GUESS fallback", model: "acme-llm-" + strings.Repeat("A", 100_000)},
		{name: "size-class heuristic", model: "acme-70b-" + strings.Repeat("A", 100_000)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetUnknownModelDedupe(t)
			buf := captureUnknownModelLogger(t)

			computeCostUSD(tc.model, CostUsage{Input: 1_000_000})

			got := buf.String()
			if got == "" {
				t.Fatalf("no WARN emitted for %q — the test is not exercising the path it claims", tc.name)
			}
			// The whole record, not just the interpolated field: the sentence
			// around it is a fixed ~250 bytes, so a generous ceiling still fails
			// hard on an uncapped 100 KB name.
			if len(got) > 1024 {
				t.Errorf("WARN record is %d bytes for a 100KB model name; the sink is not length-bounded.\n"+
					"prices.go imports plain \"log\", so slog's handling does not apply here — "+
					"route the model name through logsafe.Str.", len(got))
			}
			// Control: the barrier must still identify the model, or the WARN
			// cannot do the job it exists for (telling an operator what to add to
			// prices.yaml).
			if !strings.Contains(got, "acme-") {
				t.Errorf("the cap destroyed the diagnostic: %q", got)
			}
		})
	}
}

// TestUnknownModelWarnSetIsBounded pins #286: the distinct-model WARN dedupe set
// is capped, so an adversarial or noisy stream of never-before-seen model strings
// cannot grow it without bound (a memory-leak / WARN-flood vector). Past the cap,
// new models are still priced and still bump the per-event counters — only the
// per-model WARN is silenced, replaced by a single one-time suppression notice.
func TestUnknownModelWarnSetIsBounded(t *testing.T) {
	resetUnknownModelDedupe(t)
	buf := captureUnknownModelLogger(t)
	t.Cleanup(func() { SetUnknownModelRecorder(nil) })

	events := &countingRecorder{}
	SetUnknownModelRecorder(events)

	// Phase 1: warn about exactly maxUnknownModelWarn distinct unknown models. Each
	// string normalizes to a distinct key matching neither an exact table entry nor
	// selfHostedClass (no NNNb param count, no trailing date suffix NormalizeModel
	// strips), so each takes the flat guess path and its first sighting logs once.
	for i := 0; i < maxUnknownModelWarn; i++ {
		ComputeCost(fmt.Sprintf("acme-unknown-%d-z", i), CostUsage{Input: 1})
	}
	if got := unknownModelSeenCount.Load(); got != int64(maxUnknownModelWarn) {
		t.Fatalf("seen-set size = %d after %d distinct models, want %d", got, maxUnknownModelWarn, maxUnknownModelWarn)
	}
	if strings.Contains(buf.String(), "capped") {
		t.Fatalf("suppression notice logged before the cap was exceeded")
	}

	// Phase 2: further distinct models must NOT grow the set and must NOT emit a
	// per-model WARN — only a single one-time suppression notice, ever.
	buf.Reset()
	const extra = 50
	for i := 0; i < extra; i++ {
		ComputeCost(fmt.Sprintf("acme-overflow-%d-z", i), CostUsage{Input: 1})
	}
	if got := unknownModelSeenCount.Load(); got != int64(maxUnknownModelWarn) {
		t.Errorf("seen-set size = %d after %d overflow models, want still %d (bounded)", got, extra, maxUnknownModelWarn)
	}
	out := buf.String()
	if n := strings.Count(out, "capped"); n != 1 {
		t.Errorf("suppression notice appeared %d times, want exactly 1", n)
	}
	for i := 0; i < extra; i++ {
		if m := fmt.Sprintf("acme-overflow-%d-z", i); strings.Contains(out, m) {
			t.Errorf("overflow model %q logged a per-model WARN past the cap", m)
		}
	}
	// Pricing and per-event counting are UNAFFECTED by the WARN cap: every one of
	// the maxUnknownModelWarn + extra guessed events bumped the event recorder.
	if want := maxUnknownModelWarn + extra; events.n != want {
		t.Errorf("event recorder = %d, want %d (every guessed event counts; the cap only silences the WARN)", events.n, want)
	}
}

// TestUnknownModelWarnBoundHoldsUnderConcurrency exercises the claimUnknownModelWarn
// gate under REAL concurrency (#325 review): the other bound tests drive the cap
// serially, so `-race` never actually races the count-check / LoadOrStore /
// CompareAndSwap steps. This test fires G goroutines, each pushing M distinct
// never-before-seen models through ComputeCost concurrently, with G*M far past
// maxUnknownModelWarn, then pins the two invariants the gate documents:
//
//  1. the seen-set overshoots by AT MOST the goroutine count -- the documented
//     "maxUnknownModelWarn + G" bound (the count-check and LoadOrStore are not one
//     atomic step, so up to G goroutines can each pass the check before any stores);
//  2. the one-time "capped" suppression notice fires EXACTLY ONCE, because the
//     unknownModelWarnSuppressed atomic.Bool CAS admits a single winner even when
//     many goroutines observe the full set simultaneously.
//
// It asserts a BOUND (<= maxUnknownModelWarn + G) and an exactly-once notice, never
// an exact count, so it is deterministic and not flaky regardless of interleaving.
func TestUnknownModelWarnBoundHoldsUnderConcurrency(t *testing.T) {
	resetUnknownModelDedupe(t)
	buf := captureUnknownModelLogger(t)

	// G distinct first-sightings can each slip past the count-check before storing,
	// so the documented overshoot ceiling is maxUnknownModelWarn + G. G*perG must
	// blow well past the cap so the gate is genuinely exercised, not merely filled.
	const (
		goroutines = 16
		perG       = 256 // 16*256 = 4096 distinct models, ~4x the 1024 cap
	)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				// Globally distinct across goroutines; trailing "-z" survives
				// NormalizeModel's digit-suffix stripping, matching the serial
				// bound tests, so each is a genuine flat-fallback first sighting.
				ComputeCost(fmt.Sprintf("acme-conc-%d-%d-z", g, i), CostUsage{Input: 1})
			}
		}(g)
	}
	wg.Wait()

	// Invariant 1: the set reached the cap (proving the gate was exercised) and
	// overshot by at most the goroutine count.
	got := unknownModelSeenCount.Load()
	if got < int64(maxUnknownModelWarn) {
		t.Fatalf("seen-set size = %d, want >= %d -- the cap gate was never reached", got, maxUnknownModelWarn)
	}
	if max := int64(maxUnknownModelWarn + goroutines); got > max {
		t.Errorf("seen-set size = %d, want <= %d (maxUnknownModelWarn + G overshoot bound)", got, max)
	}

	// Invariant 2: the one-time suppression notice fired exactly once, and the
	// CAS-guarded flag is set. The log.Logger serializes writes into buf, so the
	// count is safe to read after the goroutines join.
	if n := strings.Count(buf.String(), "capped"); n != 1 {
		t.Errorf("suppression notice appeared %d times, want exactly 1 (atomic.Bool CAS must admit a single winner)", n)
	}
	if !unknownModelWarnSuppressed.Load() {
		t.Errorf("unknownModelWarnSuppressed = false, want true after exceeding the cap")
	}
}

// TestMarkerModelsDoNotConsumeCapSlot pins the #286 adversarial vector: synthetic
// placeholders (empty / <>-bearing) are guarded BEFORE the cap gate, so a stream
// of thousands of DISTINCT marker strings can neither warn nor occupy a slot in
// the bounded set — otherwise an attacker could exhaust the cap with junk and
// silence WARNs for genuinely unknown real models.
func TestMarkerModelsDoNotConsumeCapSlot(t *testing.T) {
	resetUnknownModelDedupe(t)
	silenceUnknownModelLogger(t)

	for i := 0; i < maxUnknownModelWarn+100; i++ {
		ComputeCost(fmt.Sprintf("<synthetic-%d>", i), CostUsage{Input: 1})
	}
	if got := unknownModelSeenCount.Load(); got != 0 {
		t.Errorf("marker strings consumed %d cap slots, want 0 (guarded before the cap gate)", got)
	}
}

// TestUnknownModelWarnCapIsSharedAcrossGuessPaths proves the flat-fallback and
// size-class-heuristic WARN paths draw from ONE shared distinct-model cap (#286):
// filling the cap entirely through the flat path suppresses a never-before-seen
// heuristic model's per-model WARN, yet that model is still priced by its class.
// The sharing is symmetric (both paths route through claimUnknownModelWarn), so
// one direction pins the contract.
func TestUnknownModelWarnCapIsSharedAcrossGuessPaths(t *testing.T) {
	resetUnknownModelDedupe(t)
	buf := captureUnknownModelLogger(t)

	// Fill the whole cap through the FLAT path (no param count → self-hosted-medium).
	for i := 0; i < maxUnknownModelWarn; i++ {
		ComputeCost(fmt.Sprintf("acme-flat-%d-z", i), CostUsage{Input: 1})
	}
	if got := unknownModelSeenCount.Load(); got != int64(maxUnknownModelWarn) {
		t.Fatalf("flat fill left set at %d, want %d", got, maxUnknownModelWarn)
	}

	// A fresh HEURISTIC model arriving past the shared cap: priced by its class,
	// but no per-model WARN and no set growth.
	buf.Reset()
	const heuristic = "llama-3.1-70b" // "70b" → self-hosted-large ($2/M combined)
	got := ComputeCost(heuristic, CostUsage{Input: 1_000_000})
	if got != 2_000_000 {
		t.Fatalf("ComputeCost(%q) = %d, want 2_000_000 (still priced past the cap)", heuristic, got)
	}
	if strings.Contains(buf.String(), heuristic) {
		t.Errorf("heuristic model logged a per-model WARN past the shared cap: %q", buf.String())
	}
	if got := unknownModelSeenCount.Load(); got != int64(maxUnknownModelWarn) {
		t.Errorf("heuristic model past the cap grew the set to %d, want still %d", got, maxUnknownModelWarn)
	}
}

func TestComputeCostDedupesUnknownModelWarning(t *testing.T) {
	const unknown = "acme-llm-dedupe-x"
	resetUnknownModelDedupe(t)
	buf := captureUnknownModelLogger(t)

	_ = computeCostUSD(unknown, CostUsage{Input: 1, Output: 1})
	if buf.Len() == 0 {
		t.Fatalf("expected WARN log on first unknown call")
	}

	// Second call for the same model must NOT re-log.
	buf.Reset()
	_ = computeCostUSD(unknown, CostUsage{Input: 1, Output: 1})
	if buf.Len() != 0 {
		t.Errorf("expected no WARN on repeat call for %q (deduped via sync.Map), got: %q", unknown, buf.String())
	}
}

func TestComputeCostSkipsWarnForSyntheticAndEmptyMarkers(t *testing.T) {
	// Claude Code emits "<synthetic>" for internal events that have no real
	// token cost. These must hit the fallback silently — emitting WARN for
	// every internal event would flood operators with non-actionable noise.
	cases := []string{"<synthetic>", "", "<unknown>"}
	for _, m := range cases {
		resetUnknownModelDedupe(t)
		buf := captureUnknownModelLogger(t)
		_ = computeCostUSD(m, CostUsage{Input: 1_000_000})
		if buf.Len() != 0 {
			t.Errorf("expected no WARN for marker %q, got: %q", m, buf.String())
		}
	}
}

// TestParsePriceTable_CacheMultValidation pins P0-09's per-model cache-multiplier
// schema: negative multipliers and multipliers on a combined entry are rejected
// fail-loud; omitted multipliers resolve to the provider default; an explicit
// value overrides only that class. Fails on main (the fields don't exist, so
// KnownFields rejects them before these checks run).
func TestParsePriceTable_CacheMultValidation(t *testing.T) {
	silenceUnknownModelLogger(t)
	// parsePriceTable requires the three self-hosted fallback keys; include them.
	const fallbacks = "  self-hosted-large: { input_per_m: 2, combined: true, provider: self-hosted }\n" +
		"  self-hosted-medium: { input_per_m: 0.5, combined: true, provider: self-hosted }\n" +
		"  self-hosted-small: { input_per_m: 0.1, combined: true, provider: self-hosted }\n"

	t.Run("rejects", func(t *testing.T) {
		cases := []struct{ name, yaml, want string }{
			{
				name: "negative cache_read_mult names the model",
				yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  m: { input_per_m: 1, output_per_m: 2, cache_read_mult: -0.1, provider: openai }\n" + fallbacks,
				want: `model "m": cache_read_mult must be >= 0`,
			},
			{
				name: "negative cache_write_5m_mult",
				yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  m: { input_per_m: 1, output_per_m: 2, cache_write_5m_mult: -1, provider: anthropic }\n" + fallbacks,
				want: "cache_write_5m_mult must be >= 0",
			},
			{
				name: "negative cache_write_1h_mult",
				yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  m: { input_per_m: 1, output_per_m: 2, cache_write_1h_mult: -1, provider: anthropic }\n" + fallbacks,
				want: "cache_write_1h_mult must be >= 0",
			},
			{
				name: "multiplier on a combined entry is rejected",
				yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  m: { input_per_m: 1, combined: true, cache_read_mult: 0.5, provider: self-hosted }\n" + fallbacks,
				want: "combined",
			},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				_, _, err := parsePriceTable([]byte(c.yaml))
				if err == nil {
					t.Fatalf("parsePriceTable accepted invalid table, want error containing %q", c.want)
				}
				if !strings.Contains(err.Error(), c.want) {
					t.Errorf("error = %q, want substring %q", err.Error(), c.want)
				}
			})
		}
	})

	t.Run("resolves provider defaults and explicit overrides", func(t *testing.T) {
		// anthropic with NO explicit multipliers → effective read 0.10, write5m
		// 1.25, write1h 2.00 (provider defaults). openai with an explicit
		// cache_read_mult 0.1 → effective read 0.1, writes still default 1.0.
		installPriceTable(t, "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n"+
			"  anth: { input_per_m: 10, output_per_m: 20, provider: anthropic }\n"+
			"  oai: { input_per_m: 2, output_per_m: 4, cache_read_mult: 0.1, provider: openai }\n"+fallbacks)

		// Anthropic defaults, each driven off the $10 base input rate.
		if got := ComputeCost("anth", CostUsage{CacheRead: 1_000_000}); got != DollarsToMicro(10*0.10) {
			t.Errorf("anth 1M cache_read = %d micro, want %d (default read 0.10x)", got, DollarsToMicro(10*0.10))
		}
		if got := ComputeCost("anth", CostUsage{CacheWrite5m: 1_000_000}); got != DollarsToMicro(10*1.25) {
			t.Errorf("anth 1M cache_write_5m = %d micro, want %d (default 5m 1.25x)", got, DollarsToMicro(10*1.25))
		}
		if got := ComputeCost("anth", CostUsage{CacheWrite1h: 1_000_000}); got != DollarsToMicro(10*2.00) {
			t.Errorf("anth 1M cache_write_1h = %d micro, want %d (default 1h 2.00x)", got, DollarsToMicro(10*2.00))
		}
		// OpenAI explicit read override + default (1.0) writes, off the $2 rate.
		if got := ComputeCost("oai", CostUsage{CacheRead: 1_000_000}); got != DollarsToMicro(2*0.1) {
			t.Errorf("oai 1M cache_read = %d micro, want %d (explicit read 0.1x)", got, DollarsToMicro(2*0.1))
		}
		if got := ComputeCost("oai", CostUsage{CacheWrite5m: 1_000_000}); got != DollarsToMicro(2*1.0) {
			t.Errorf("oai 1M cache_write_5m = %d micro, want %d (default write 1.0x)", got, DollarsToMicro(2*1.0))
		}
	})
}

// TestComputeCost_PerModelCacheMultOverridesProviderDefault pins that a per-model
// cache_read_mult overrides the provider default. Fails on main, where the openai
// provider constant (0.5x) is hardcoded in ComputeCost's switch.
func TestComputeCost_PerModelCacheMultOverridesProviderDefault(t *testing.T) {
	silenceUnknownModelLogger(t)
	// An openai (GPT-5-era shaped) model overriding the 0.5x default with 0.1x.
	// 1M cache-read at $2.00 input → 1M/1e6 * 2.00 * 0.1 = $0.20 = 200_000 micro.
	// Main's openai 0.5x constant would give 1_000_000 micro.
	installPriceTable(t, `
version: 1
effective_date: "2030-01-01"
models:
  gpt-5-era: { input_per_m: 2.00, output_per_m: 8.00, cache_read_mult: 0.1, provider: openai }
  self-hosted-large: { input_per_m: 2.00, combined: true, provider: self-hosted }
  self-hosted-medium: { input_per_m: 0.50, combined: true, provider: self-hosted }
  self-hosted-small: { input_per_m: 0.10, combined: true, provider: self-hosted }
`)
	if got := ComputeCost("gpt-5-era", CostUsage{CacheRead: 1_000_000}); got != 200_000 {
		t.Errorf("gpt-5-era 1M cache_read = %d micro, want 200_000 (per-model 0.1x overrides openai 0.5x default)", got)
	}
}

// TestComputeCost_DeepSeekCacheHitRate pins DeepSeek's absolute $0.028/M cache-hit
// rate, encoded in v4 as a per-model cache_read_mult on the EMBEDDED table. Fails
// on main, where DeepSeek falls through to the 1.0x provider default.
func TestComputeCost_DeepSeekCacheHitRate(t *testing.T) {
	silenceUnknownModelLogger(t)
	// deepseek-v3 input $0.28, cache_read_mult 0.1: 1M cache_read
	// = 1M/1e6 * 0.28 * 0.1 = $0.028 = 28_000 micro.
	if got := ComputeCost("deepseek-v3", CostUsage{CacheRead: 1_000_000}); got != 28_000 {
		t.Errorf("deepseek-v3 1M cache_read = %d micro, want 28_000 ($0.028/M hit = 0.1x of $0.28 input)", got)
	}
	// deepseek-r1 input $0.55, cache_read_mult 0.0509: 0.55 * 0.0509 = $0.0279950/M
	// → 1M cache_read = 0.0279950 dollars * 1e6 = 27_995 micro (exact, no half).
	if got := ComputeCost("deepseek-r1", CostUsage{CacheRead: 1_000_000}); got != 27_995 {
		t.Errorf("deepseek-r1 1M cache_read = %d micro, want 27_995 (0.55*0.0509 = $0.0279950/M)", got)
	}
}

// TestComputeCost_GeminiImplicitCacheRead pins Gemini 2.5+ implicit caching (75%
// discount = 0.25x) on the EMBEDDED table, AND that the multiplier scales off the
// SELECTED (over-tier) input rate. Fails on main (google → 1.0x default).
func TestComputeCost_GeminiImplicitCacheRead(t *testing.T) {
	silenceUnknownModelLogger(t)
	// Base-rate 0.25x on a FLAT Gemini entry (no over-tier threshold to interact
	// with): gemini-2.5-flash input $0.30, cache_read_mult 0.25.
	// 1M cache_read = 1M/1e6 * 0.30 * 0.25 = $0.075 = 75_000 micro.
	if got := ComputeCost("gemini-2.5-flash", CostUsage{CacheRead: 1_000_000}); got != 75_000 {
		t.Errorf("gemini-2.5-flash 1M cache_read = %d micro, want 75_000 (0.25x of $0.30 base)", got)
	}
	// Sub-threshold gemini-2.5-pro (base $1.25): 100K cache_read = 100K < 200K, so
	// the base rate applies. 100K/1e6 * 1.25 * 0.25 = $0.03125 = 31_250 micro.
	if got := ComputeCost("gemini-2.5-pro", CostUsage{CacheRead: 100_000}); got != 31_250 {
		t.Errorf("gemini-2.5-pro 100K cache_read = %d micro, want 31_250 (0.25x of $1.25 base, sub-threshold)", got)
	}
	// Over-tier: 1.5M Input + 1M CacheRead = 2.5M input-context > 200K threshold,
	// so the request re-prices at the premium input rate $2.50 and the 0.25x read
	// multiplier scales off THAT selected rate (NOT the base $1.25) — the reason
	// #122 keeps multipliers as multipliers-of-input, not absolute $/M.
	// in   = 1_500_000/1e6 * 2.50         = 3.75
	// read = 1_000_000/1e6 * 2.50 * 0.25  = 0.625   → total $4.375 = 4_375_000 micro.
	if got := ComputeCost("gemini-2.5-pro", CostUsage{Input: 1_500_000, CacheRead: 1_000_000}); got != 4_375_000 {
		t.Errorf("gemini-2.5-pro 1.5M in + 1M cache_read (over-tier) = %d micro, want 4_375_000 (0.25x scales off premium $2.50)", got)
	}
}

// TestComputeCost_O1PricedAtRPTNotFallback pins that o1 (RPT §1.1 $15/$60) is in
// the table and prices at its real rate — NOT the self-hosted-medium fallback
// ($0.50/M combined). The raw model string carries a date suffix to exercise
// NormalizeModel + table lookup as a unit. FAILS on main (no o1 entry): 1M in +
// 1M out would fall back to 2M × $0.50/M = $1.00 = 1_000_000 micro (#135).
func TestComputeCost_O1PricedAtRPTNotFallback(t *testing.T) {
	silenceUnknownModelLogger(t)
	// 1M in + 1M out at $15/$60 = $75.00 = 75_000_000 micro.
	if got := ComputeCost("o1-2026-01-15", CostUsage{Input: 1_000_000, Output: 1_000_000}); got != DollarsToMicro(15+60) {
		t.Errorf("ComputeCost(o1-2026-01-15, 1M+1M) = %d micro, want %d ($15+$60; not the $1.00 fallback)", got, DollarsToMicro(15+60))
	}
}

// TestComputeCost_Gpt5FamilyPriced pins every gpt-5.x entry at its exact RPT
// rate. Token counts are ASYMMETRIC (1M in + 2M out) so input and output rates
// are pinned INDEPENDENTLY: want = input_per_m + 2×output_per_m (micro-dollars).
// FAILS on main (none of these exist → self-hosted-medium fallback = 3M × $0.50
// = $1.50 for every row) (#135).
func TestComputeCost_Gpt5FamilyPriced(t *testing.T) {
	silenceUnknownModelLogger(t)
	usage := CostUsage{Input: 1_000_000, Output: 2_000_000}
	cases := []struct {
		model string
		want  int64 // input_per_m + 2×output_per_m, in micro-dollars
	}{
		{"gpt-5", 21_250_000},             // 1.25 + 2×10.00 = 21.25
		{"gpt-5.1", 10_625_000},           // 0.625 + 2×5.00 = 10.625
		{"gpt-5.2", 14_875_000},           // 0.875 + 2×7.00 = 14.875
		{"gpt-5.4", 32_500_000},           // 2.50 + 2×15.00 = 32.50
		{"gpt-5.4-pro", 390_000_000},      // 30.00 + 2×180.00 = 390.00
		{"gpt-5-mini", 2_125_000},         // 0.125 + 2×1.00 = 2.125
		{"gpt-5-nano", 850_000},           // 0.05 + 2×0.40 = 0.85
		{"gpt-5.4-mini", 9_750_000},       // 0.75 + 2×4.50 = 9.75
		{"gpt-5.4-nano", 2_700_000},       // 0.20 + 2×1.25 = 2.70
		{"gpt-5.3-codex", 29_750_000},     // 1.75 + 2×14.00 = 29.75
		{"gpt-5.1-codex", 21_250_000},     // 1.25 + 2×10.00 = 21.25
		{"gpt-5.1-codex-mini", 4_250_000}, // 0.25 + 2×2.00 = 4.25
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			if got := ComputeCost(c.model, usage); got != c.want {
				t.Errorf("ComputeCost(%s, 1M in + 2M out) = %d micro, want %d", c.model, got, c.want)
			}
		})
	}
}

// TestComputeCost_Gpt5CacheReadMult pins the deliberate #135 decision that the
// gpt-5.x entries carry cache_read_mult 0.10 (the 5-era prompt-cache discount,
// RPT §7) rather than inheriting the openai 4-era 0.50 default. Without the
// explicit multiplier the cache read would bill at 0.50× (625_000 micro).
func TestComputeCost_Gpt5CacheReadMult(t *testing.T) {
	silenceUnknownModelLogger(t)
	// gpt-5 input $1.25, cache_read_mult 0.10: 1M cache_read
	// = 1M/1e6 × 1.25 × 0.10 = $0.125 = 125_000 micro.
	if got := ComputeCost("gpt-5", CostUsage{CacheRead: 1_000_000}); got != 125_000 {
		t.Errorf("gpt-5 1M cache_read = %d micro, want 125_000 (0.10x of $1.25 input; NOT the openai 0.50 default)", got)
	}
}

// TestComputeCost_Grok4FastPriced pins the new xAI entries at their RPT rates,
// asymmetric (1M in + 2M out) to pin input and output independently. FAILS on
// main (no grok-4/4-fast/code-fast entries → fallback) (#135).
func TestComputeCost_Grok4FastPriced(t *testing.T) {
	silenceUnknownModelLogger(t)
	usage := CostUsage{Input: 1_000_000, Output: 2_000_000}
	cases := []struct {
		model string
		want  int64
	}{
		{"grok-4", 33_000_000},          // 3.00 + 2×15.00 = 33.00
		{"grok-3-fast", 55_000_000},     // 5.00 + 2×25.00 = 55.00
		{"grok-4-fast", 1_200_000},      // 0.20 + 2×0.50 = 1.20
		{"grok-4.1-fast", 1_200_000},    // 0.20 + 2×0.50 = 1.20
		{"grok-code-fast-1", 3_200_000}, // 0.20 + 2×1.50 = 3.20
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			if got := ComputeCost(c.model, usage); got != c.want {
				t.Errorf("ComputeCost(%s, 1M in + 2M out) = %d micro, want %d", c.model, got, c.want)
			}
		})
	}
}

// TestComputeCost_Opus41StaysLegacyRate pins the RPT-vs-review discrepancy
// decision (#135): claude-opus-4-1 keeps Opus-4-generation pricing $15/$75 (RPT
// §1.1), NOT the $5/$25 the review's C5 shorthand implied (that rate applies to
// 4.5+ only). 1M in + 1M out must be $90.00, not $30.00.
func TestComputeCost_Opus41StaysLegacyRate(t *testing.T) {
	silenceUnknownModelLogger(t)
	got := ComputeCost("claude-opus-4-1", CostUsage{Input: 1_000_000, Output: 1_000_000})
	if got != DollarsToMicro(15+75) {
		t.Errorf("ComputeCost(claude-opus-4-1, 1M+1M) = %d micro, want %d ($15+$75, legacy Opus-4 rate)", got, DollarsToMicro(15+75))
	}
	// Contrast: 4-5 IS the reduced $5/$25 rate — proves the two are distinct.
	if opus45 := ComputeCost("claude-opus-4-5", CostUsage{Input: 1_000_000, Output: 1_000_000}); opus45 != DollarsToMicro(5+25) {
		t.Errorf("ComputeCost(claude-opus-4-5, 1M+1M) = %d micro, want %d ($5+$25)", opus45, DollarsToMicro(5+25))
	}
	if got == ComputeCost("claude-opus-4-5", CostUsage{Input: 1_000_000, Output: 1_000_000}) {
		t.Error("claude-opus-4-1 ($15/$75) and claude-opus-4-5 ($5/$25) must price differently")
	}
}

// TestComputeCost_DeepseekV32Priced pins that deepseek-v3.2 has its OWN entry and
// does not normalize to deepseek-v3 (the ".2" is not a date suffix), nor fall
// back. Asymmetric 1M in + 2M out: 0.28 + 2×0.42 = $1.12 (fallback would be 3M ×
// $0.50 = $1.50) (#135).
func TestComputeCost_DeepseekV32Priced(t *testing.T) {
	silenceUnknownModelLogger(t)
	if norm := NormalizeModel("deepseek-v3.2"); norm != "deepseek-v3.2" {
		t.Fatalf("NormalizeModel(deepseek-v3.2) = %q, want deepseek-v3.2 (must not collapse to deepseek-v3)", norm)
	}
	if got := ComputeCost("deepseek-v3.2", CostUsage{Input: 1_000_000, Output: 2_000_000}); got != 1_120_000 {
		t.Errorf("ComputeCost(deepseek-v3.2, 1M in + 2M out) = %d micro, want 1_120_000 (0.28 + 2×0.42; not the $1.50 fallback)", got)
	}
}

// TestComputeCost_RecordsFallbackCost: a fallback (unknown-model) event feeds the
// unknown-model COST recorder exactly the fallback cost in micro-dollars (as a
// float64, matching the Add(v float64, ...) counter contract, #135).
func TestComputeCost_RecordsFallbackCost(t *testing.T) {
	silenceUnknownModelLogger(t)
	resetUnknownModelDedupe(t)
	t.Cleanup(func() { SetUnknownModelCostRecorder(nil) })

	rec := &fakeCostRecorder{}
	SetUnknownModelCostRecorder(rec)

	// Unknown model, 1M input → self-hosted-medium $0.50/M combined = $0.50 =
	// 500_000 micro.
	got := ComputeCost("totally-unknown-x", CostUsage{Input: 1_000_000})
	if got != 500_000 {
		t.Fatalf("ComputeCost(unknown, 1M in) = %d micro, want 500_000 (fallback)", got)
	}
	if rec.calls != 1 || rec.sum != 500_000 {
		t.Errorf("unknown-model cost recorder = {calls:%d sum:%v}, want {1 500000}", rec.calls, rec.sum)
	}

	// A KNOWN model must NOT touch the unknown-model cost recorder.
	ComputeCost("claude-sonnet-4", CostUsage{Input: 1_000_000})
	if rec.calls != 1 {
		t.Errorf("unknown-model cost recorder calls = %d after a known model, want still 1", rec.calls)
	}
}

// TestComputeCost_RecordsPricedCostAlways: EVERY ComputeCost result (known and
// fallback alike) bumps the priced-cost recorder by its own micro-dollar cost,
// so it is the total-spend denominator for the fallback-share alert (#135).
func TestComputeCost_RecordsPricedCostAlways(t *testing.T) {
	silenceUnknownModelLogger(t)
	resetUnknownModelDedupe(t)
	t.Cleanup(func() { SetPricedCostRecorder(nil) })

	rec := &fakeCostRecorder{}
	SetPricedCostRecorder(rec)

	// Known model: claude-sonnet-4 1M in + 1M out = $18.00 = 18_000_000 micro.
	known := ComputeCost("claude-sonnet-4", CostUsage{Input: 1_000_000, Output: 1_000_000})
	// Fallback: unknown 1M in = $0.50 = 500_000 micro.
	fallback := ComputeCost("totally-unknown-priced", CostUsage{Input: 1_000_000})
	if want := float64(known + fallback); rec.sum != want {
		t.Errorf("priced-cost recorder sum = %v, want %v (known %d + fallback %d)", rec.sum, want, known, fallback)
	}
	if rec.calls != 2 {
		t.Errorf("priced-cost recorder calls = %d, want 2 (fires on every ComputeCost)", rec.calls)
	}
}

// fakeCostRecorder is a test UnknownModelCostRecorder capturing call count and
// the running micro-dollar sum passed to Add.
type fakeCostRecorder struct {
	calls int
	sum   float64
}

func (f *fakeCostRecorder) Add(v float64, _ ...string) {
	f.calls++
	f.sum += v
}

// TestComputeCost_HeuristicPriceIsObservable pins #267: a model priced by the
// size-class heuristic (a parameter count in the string, no exact table entry)
// is NOT silent — it emits a one-time WARN naming the model and the resolved
// self-hosted class, and it bumps BOTH the unknown-model event counter and the
// guessed-cost recorder (the same seams the flat fallback uses). Before the fix
// llama-3.1-70b matched "70b" → self-hosted-large, priced correctly, but fired
// no WARN and no counter, so an org on open-weights saw a chunk of estimated
// spend with zero observability.
func TestComputeCost_HeuristicPriceIsObservable(t *testing.T) {
	resetUnknownModelDedupe(t)
	buf := captureUnknownModelLogger(t)
	t.Cleanup(func() {
		SetUnknownModelRecorder(nil)
		SetUnknownModelCostRecorder(nil)
	})

	events := &countingRecorder{}
	cost := &fakeCostRecorder{}
	SetUnknownModelRecorder(events)
	SetUnknownModelCostRecorder(cost)

	// llama-3.1-70b → "70b" → self-hosted-large ($2.00/M combined). 1M input = $2
	// = 2_000_000 micro. Cost must be unchanged by the added observability.
	const heuristic = "llama-3.1-70b"
	got := ComputeCost(heuristic, CostUsage{Input: 1_000_000})
	if got != 2_000_000 {
		t.Fatalf("ComputeCost(%q, 1M in) = %d micro, want 2_000_000 (self-hosted-large)", heuristic, got)
	}

	// One-time WARN names the upstream model string and the class it landed at.
	out := buf.String()
	if !strings.Contains(out, heuristic) {
		t.Errorf("expected WARN to name the model %q, got: %q", heuristic, out)
	}
	if !strings.Contains(out, "self-hosted-large") {
		t.Errorf("expected WARN to name the resolved class self-hosted-large, got: %q", out)
	}

	// Both observability seams fired exactly once with the heuristic cost.
	if events.n != 1 {
		t.Errorf("event recorder = %d, want 1 (heuristic pricing is a counted guess)", events.n)
	}
	if cost.calls != 1 || cost.sum != 2_000_000 {
		t.Errorf("guessed-cost recorder = {calls:%d sum:%v}, want {1 2000000}", cost.calls, cost.sum)
	}
}

// TestComputeCost_HeuristicWarnDedupesButCounterPerEvent: like the flat-fallback
// pattern, the heuristic WARN dedupes to one per model, but the event counter
// fires on EVERY call (spend volume, not distinct models) (#267).
func TestComputeCost_HeuristicWarnDedupesButCounterPerEvent(t *testing.T) {
	resetUnknownModelDedupe(t)
	buf := captureUnknownModelLogger(t)
	t.Cleanup(func() { SetUnknownModelRecorder(nil) })

	events := &countingRecorder{}
	SetUnknownModelRecorder(events)

	const heuristic = "qwen-2.5-72b"
	ComputeCost(heuristic, CostUsage{Input: 1_000_000})
	if buf.Len() == 0 {
		t.Fatal("expected a WARN on the first heuristic-priced call")
	}
	buf.Reset()
	ComputeCost(heuristic, CostUsage{Input: 500_000})
	if buf.Len() != 0 {
		t.Errorf("expected no WARN on the repeat heuristic call for %q, got: %q", heuristic, buf.String())
	}
	if events.n != 2 {
		t.Errorf("event recorder = %d, want 2 (every heuristic event counts)", events.n)
	}
}

// TestComputeCost_ExactSelfHostedKeyIsSilent: an operator pricing directly at an
// explicit self-hosted-* key (or any real audited model) is an EXACT table hit,
// not a guess — it must NOT warn and must NOT touch the unknown-model
// event/cost recorders (#267: only non-exact-table pricing is observed).
func TestComputeCost_ExactSelfHostedKeyIsSilent(t *testing.T) {
	resetUnknownModelDedupe(t)
	buf := captureUnknownModelLogger(t)
	t.Cleanup(func() {
		SetUnknownModelRecorder(nil)
		SetUnknownModelCostRecorder(nil)
	})

	events := &countingRecorder{}
	cost := &fakeCostRecorder{}
	SetUnknownModelRecorder(events)
	SetUnknownModelCostRecorder(cost)

	for _, exact := range []string{"self-hosted-large", "self-hosted-medium", "self-hosted-small", "claude-sonnet-4"} {
		ComputeCost(exact, CostUsage{Input: 1_000_000})
	}
	if buf.Len() != 0 {
		t.Errorf("expected no WARN for exact table keys, got: %q", buf.String())
	}
	if events.n != 0 {
		t.Errorf("event recorder = %d after only exact keys, want 0", events.n)
	}
	if cost.calls != 0 {
		t.Errorf("guessed-cost recorder calls = %d after only exact keys, want 0", cost.calls)
	}
}
