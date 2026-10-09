// Bootstrap confidence intervals for TIER (#133, review C3).
//
// A TIER number is a ratio of a noisy numerator (Σ weight×quality over a handful
// of outcomes) to a denominator that is ALSO sampled: the cost attributed to those
// same outcomes. With only a few outcomes behind it, both carry real sampling
// noise, and a bare point estimate hides it. The percentile bootstrap makes the
// uncertainty visible: it resamples the outcomes with replacement, recomputes TIER
// for each resample, and reports the 2.5th/97.5th percentiles as a 95% interval.
//
// The resampling unit is one outcome's (weight×quality, cost) PAIR, drawn JOINTLY
// (#495) so the numerator and the outcome-attributed denominator move together —
// pinning the denominator understated the interval ~1.8×. Only cost tied to no
// outcome (unattributed/exploratory spend) stays fixed. The PRNG is injected so
// callers can make tests deterministic; production callers pass a freshly seeded
// generator.
package scoring

import (
	"math"
	"math/rand/v2"
	"sort"
)

// DefaultBootstrapSamples is the resample count (B) used for TIER confidence
// intervals. 1000 gives stable 2.5/97.5 percentile estimates at negligible cost
// for the outcome counts we see in practice.
const DefaultBootstrapSamples = 1000

// BootstrapCI returns the [2.5, 97.5] percentile bootstrap interval of TIER,
// resampling each outcome's (contribution, cost) PAIR JOINTLY (#495).
//
// TIER is a ratio of two SAMPLED quantities: the numerator (Σ weight×quality over
// outcomes) and the outcome-attributed part of the denominator (Σ cost over the
// same outcomes). Resampling the numerator alone while pinning cost — the pre-#495
// behaviour — held constant the noisier of the two (per-PR spend is heavy-tailed;
// weights live on a bounded 0.5–8 scale), producing intervals ~1.8× too narrow,
// worst at small N. Drawing ONE set of outcome indices per replicate and
// accumulating both sums from it preserves each outcome's weight↔cost correlation.
//
// contribs[i] and outcomeCostUSD[i] are outcome i's weight×quality contribution
// and its allocated list-price cost (its issue's cost, split across that issue's
// outcomes). fixedCostUSD is the window cost NOT tied to any resampled outcome —
// unattributed/exploratory spend plus the cost of issues with no outcome in the
// set — held constant so the interval's centre stays on the published TIER, whose
// denominator is the developer's TOTAL window cost. b is the resample count; rng
// is injected for deterministic tests: rand.New(rand.NewPCG(seed1, seed2)).
//
// Procedure: for each of b replicates, draw len(contribs) indices uniformly WITH
// replacement; num = Σ contribs[drawn]; denom = (Σ outcomeCostUSD[drawn] +
// fixedCostUSD) / 1000; tier_b = num / denom. The resampled TIERs are sorted
// ascending; lo is at index floor(0.025·m) and hi at ceil(0.975·m)-1, over the m
// finite replicates.
//
// Edge cases return (0, 0) with no panic: b <= 0, no contributions, mismatched
// slice lengths, nil rng, or a non-positive total cost (Σ outcomeCostUSD +
// fixedCostUSD) — a TIER of ∞/NaN has no meaningful interval. A replicate whose
// drawn denominator is <= 0 is skipped (reachable only when fixedCostUSD is 0 AND
// every drawn outcome cost is 0 — a zero-cost degenerate that #502's min-cost
// floor addresses); with any positive fixedCostUSD it never triggers and m == b.
//
// Complexity: O(b·n) float ops and O(b) space; the caller runs this per ranked
// developer per request. Each call owns its rng, so distinct calls are race-free.
func BootstrapCI(contribs, outcomeCostUSD []float64, fixedCostUSD float64, b int, rng *rand.Rand) (lo, hi float64) {
	n := len(contribs)
	if b <= 0 || n == 0 || n != len(outcomeCostUSD) || rng == nil {
		return 0, 0
	}
	// The point-estimate denominator must be positive for a ratio to exist.
	var totalOutcomeCost float64
	for _, c := range outcomeCostUSD {
		totalOutcomeCost += c
	}
	if totalOutcomeCost+fixedCostUSD <= 0 {
		return 0, 0
	}

	tiers := make([]float64, 0, b)
	for i := 0; i < b; i++ {
		var num, cost float64
		for j := 0; j < n; j++ {
			k := rng.IntN(n)
			num += contribs[k]
			cost += outcomeCostUSD[k]
		}
		denom := (cost + fixedCostUSD) / tierCostScaleUSD
		if denom <= 0 {
			// Degenerate replicate: no finite ratio. Skipping it (rather than
			// emitting an Inf/NaN that corrupts the percentiles) is safe because it
			// is unreachable whenever fixedCostUSD > 0, the normal case.
			continue
		}
		tiers = append(tiers, num/denom)
	}
	if len(tiers) == 0 {
		return 0, 0
	}
	sort.Float64s(tiers)

	// Percentile indices per the review C3 spec: floor(0.025·m) and ceil(0.975·m)-1
	// over the m finite replicates. For m = 1000 these are 25 and 974. Clamp to keep
	// them in range for any m.
	m := len(tiers)
	loIdx := int(math.Floor(0.025 * float64(m)))
	hiIdx := int(math.Ceil(0.975*float64(m))) - 1
	if loIdx < 0 {
		loIdx = 0
	}
	if hiIdx >= m {
		hiIdx = m - 1
	}
	if hiIdx < 0 {
		hiIdx = 0
	}
	return tiers[loIdx], tiers[hiIdx]
}

// CostPerPointCI transforms a TIER bootstrap interval [tierLo, tierHi] into the
// corresponding 95% percentile interval for cost_per_point (#239) WITHOUT a
// second resample. Within each bootstrap replicate cost_per_point_b = cost_b /
// points_b = 1000/tier_b EXACTLY — both are computed from the same drawn set, so
// the identity holds even though #495 now resamples cost jointly; the percentile
// bootstrap is invariant under a monotone transform, and 1/x is monotone
// DECREASING, so the cost_per_point percentiles are the reciprocals of the TIER
// percentiles with the bounds SWAPPED — a higher TIER is a lower cost_per_point.
// The result is therefore exact (~1 ULP), not an approximation, and reuses the
// existing #133 machinery.
//
// This is the SELF-relative interval: cost_per_point against the developer's own
// resampled outcome history, the honest thing a single window supports. A
// cross-org or historical PERCENTILE RANK is deliberately NOT synthesized here —
// it needs a second org's opt-in data to exist first (#239 item 4), and no
// absolute good/ok/poor band is invented from an absent dataset.
//
// Returns (0, 0) when either input bound is not a positive finite number: an
// unranked row already carries a (0, 0) TIER interval, and a non-positive (or NaN)
// bound has no meaningful reciprocal. The `!(x > 0)` form also rejects NaN, which
// a bare `<= 0` would let through.
func CostPerPointCI(tierLo, tierHi float64) (lo, hi float64) {
	if !(tierLo > 0) || !(tierHi > 0) {
		return 0, 0
	}
	return tierCostScaleUSD / tierHi, tierCostScaleUSD / tierLo
}
