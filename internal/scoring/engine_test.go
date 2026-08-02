package scoring

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestComputeDeveloper(t *testing.T) {
	outcomes := []Outcome{
		{Developer: "alice", IssueID: "issue-1", Weight: 8, Quality: 1.0},
		{Developer: "alice", IssueID: "issue-2", Weight: 3, Quality: 1.0},
	}
	// From tier-workflow.md worked example: Alice total cost $20.85
	// TIER = 11.0 / (20.85/1000) = 527.6
	s := ComputeDeveloper("alice", outcomes, 20.85, 20.85, 0)
	if math.Abs(s.WeightedPoints-11.0) > 0.001 {
		t.Errorf("WeightedPoints = %v, want 11.0", s.WeightedPoints)
	}
	expected := 11.0 / (20.85 / 1000.0)
	if math.Abs(s.TIER-expected) > 0.01 {
		t.Errorf("TIER = %v, want %.2f", s.TIER, expected)
	}
	if s.CoveragePercent != 100.0 {
		t.Errorf("coverage = %v, want 100.0", s.CoveragePercent)
	}
}

func TestComputeDeveloperReverted(t *testing.T) {
	outcomes := []Outcome{
		{Developer: "bob", IssueID: "issue-3", Weight: 5, Quality: 0.5},
	}
	s := ComputeDeveloper("bob", outcomes, 10.0, 10.0, 0)
	if math.Abs(s.WeightedPoints-2.5) > 0.001 {
		t.Errorf("WeightedPoints with revert = %v, want 2.5", s.WeightedPoints)
	}
}

func TestComputeDeveloperNoCost(t *testing.T) {
	// No cost → TIER is 0, no divide-by-zero.
	s := ComputeDeveloper("carol", nil, 0, 0, 0)
	if s.TIER != 0 {
		t.Errorf("TIER with zero cost = %v, want 0", s.TIER)
	}
}

func TestRollupTeam(t *testing.T) {
	// From tier-workflow.md: Alice TIER=118.3, Bob TIER=97.9
	// Team: 16.5 / (158.0/1000) = 104.4
	alice := DeveloperScore{Developer: "alice", WeightedPoints: 6.0, TotalCostUSD: 50.70, CoveragePercent: 100}
	alice.TIER = alice.WeightedPoints / (alice.TotalCostUSD / 1000.0)

	bob := DeveloperScore{Developer: "bob", WeightedPoints: 10.5, TotalCostUSD: 107.30, CoveragePercent: 100}
	bob.TIER = bob.WeightedPoints / (bob.TotalCostUSD / 1000.0)

	team := RollupTeam("backend", []DeveloperScore{alice, bob})
	if math.Abs(team.TotalCostUSD-158.0) > 0.001 {
		t.Errorf("team cost = %v, want 158.0", team.TotalCostUSD)
	}
	expectedTIER := 16.5 / (158.0 / 1000.0)
	if math.Abs(team.TIER-expectedTIER) > 0.1 {
		t.Errorf("team TIER = %v, want %.1f", team.TIER, expectedTIER)
	}
}

// TestComputeDeveloper_SpendLeverage covers the CFO-facing sidecar:
// SpendLeverage = TotalCostUSD / ActualPaidUSD. An enterprise contract that
// charges $400 for what would have been $1,000 at list price gives the
// developer a 2.5× leverage number — what gets pitched to the CFO.
func TestComputeDeveloper_SpendLeverage(t *testing.T) {
	outcomes := []Outcome{
		{Developer: "alice", IssueID: "issue-1", Weight: 8, Quality: 1.0},
	}
	s := ComputeDeveloper("alice", outcomes, 1000.0, 1000.0, 400.0)
	if math.Abs(s.SpendLeverage-2.5) > 0.001 {
		t.Errorf("SpendLeverage = %v, want 2.5", s.SpendLeverage)
	}
	if s.ActualPaidUSD != 400.0 {
		t.Errorf("ActualPaidUSD = %v, want 400.0", s.ActualPaidUSD)
	}
}

// TestComputeDeveloper_SpendLeverageNoActualSpend confirms the dashboard-
// friendly "no leverage data yet" path: when finance has not posted an invoice
// for the period, SpendLeverage stays 0 (rendered as "—") rather than NaN or
// Inf, which would break JSON encoding (json.Marshal returns an error on NaN).
func TestComputeDeveloper_SpendLeverageNoActualSpend(t *testing.T) {
	s := ComputeDeveloper("alice", nil, 1000.0, 0, 0)
	if s.SpendLeverage != 0 {
		t.Errorf("SpendLeverage with zero actual_paid = %v, want 0", s.SpendLeverage)
	}
}

// TestComputeDeveloper_SpendLeverageOverCredited covers the #24 over-credit
// case: when credit memos exceed the invoice, ActualPaidUSD is negative.
// SpendLeverage stays 0 (dashboard renders "—") per the product decision
// that a negative leverage multiplier has no meaningful interpretation.
func TestComputeDeveloper_SpendLeverageOverCredited(t *testing.T) {
	s := ComputeDeveloper("alice", nil, 1000.0, 0, -50.0)
	if s.SpendLeverage != 0 {
		t.Errorf("SpendLeverage with negative actual_paid = %v, want 0 (per #24 product decision)", s.SpendLeverage)
	}
	// ActualPaidUSD itself is recorded truthfully — only the derived
	// leverage metric is suppressed.
	if s.ActualPaidUSD != -50.0 {
		t.Errorf("ActualPaidUSD = %v, want -50.0 (negative net is recorded)", s.ActualPaidUSD)
	}
}

// TestRollupTeam_SpendLeverage verifies the team aggregate uses summed values,
// not an average of individual leverage ratios — same principle as team TIER.
// Two developers at different leverage ratios produce a team ratio weighted by
// their absolute dollar amounts.
func TestRollupTeam_SpendLeverage(t *testing.T) {
	alice := DeveloperScore{Developer: "alice", TotalCostUSD: 1000, ActualPaidUSD: 400, SpendLeverage: 2.5}
	bob := DeveloperScore{Developer: "bob", TotalCostUSD: 500, ActualPaidUSD: 250, SpendLeverage: 2.0}
	team := RollupTeam("backend", []DeveloperScore{alice, bob})

	wantLeverage := 1500.0 / 650.0
	if math.Abs(team.SpendLeverage-wantLeverage) > 0.001 {
		t.Errorf("team SpendLeverage = %v, want %.4f (1500/650, not avg(2.5,2.0))",
			team.SpendLeverage, wantLeverage)
	}
	if team.ActualPaidUSD != 650.0 {
		t.Errorf("team ActualPaidUSD = %v, want 650.0", team.ActualPaidUSD)
	}
}

// --- #64: FormatReport coverage (was 0%) ---

func TestFormatReport_Empty(t *testing.T) {
	out := FormatReport(nil, "2026-01-01", AggregationDeveloper)
	if !strings.Contains(out, "No data found") {
		t.Errorf("empty scores: output = %q, want the no-data message", out)
	}
}

// TestFormatReport_SortsAndTotals pins the three load-bearing behaviors:
// descending-TIER ordering, the recomputed team-total row (points and cost
// summed, TIER and coverage derived — NOT averaged), and the explanatory
// footer. Input is deliberately unsorted to prove the sort happens.
func TestFormatReport_SortsAndTotals(t *testing.T) {
	scores := []DeveloperScore{
		{Developer: "bob", TIER: 100, WeightedPoints: 1, TotalCostUSD: 10, CoveragePercent: 50},
		{Developer: "alice", TIER: 1200, WeightedPoints: 3, TotalCostUSD: 2.5, CoveragePercent: 100},
	}
	out := FormatReport(scores, "2026-01-01", AggregationDeveloper)

	if !strings.Contains(out, "TIER Report — since 2026-01-01") {
		t.Errorf("missing header; out=%q", out)
	}
	ai, bi := strings.Index(out, "alice"), strings.Index(out, "bob")
	if ai == -1 || bi == -1 || ai > bi {
		t.Errorf("rows not sorted by TIER desc (alice@%d, bob@%d)", ai, bi)
	}
	// Team totals: points 4, cost 12.5 → TIER = 4/(12.5/1000) = 320.
	// Coverage = (2.5×100% + 10×50%) / 12.5 = 60%.
	totalIdx := strings.Index(out, "TEAM TOTAL")
	if totalIdx == -1 {
		t.Fatalf("missing TEAM TOTAL row; out=%q", out)
	}
	totalLine := strings.SplitN(out[totalIdx:], "\n", 2)[0]
	// The four numeric columns are positional (TIER, cost, points, coverage),
	// the last four whitespace-separated fields. Matching by column keeps a
	// value like "4.0" from false-matching digits bleeding in from another field.
	fields := strings.Fields(totalLine)
	if len(fields) < 4 {
		t.Fatalf("TEAM TOTAL line malformed: %q", totalLine)
	}
	cols := fields[len(fields)-4:]
	for i, want := range []string{"320.0", "12.5000", "4.0", "60%"} {
		if cols[i] != want {
			t.Errorf("TEAM TOTAL column %d = %q, want %q (line %q)", i, cols[i], want, totalLine)
		}
	}
	if !strings.Contains(out, "Formula: TIER") || !strings.Contains(out, "Fidelity: %") {
		t.Errorf("missing explanatory footer; out=%q", out)
	}
}

// TestFormatReport_ZeroCostNoDivByZero pins the guard for the all-estimated /
// no-spend case: the team row renders 0.0 instead of NaN/Inf.
func TestFormatReport_ZeroCostNoDivByZero(t *testing.T) {
	out := FormatReport([]DeveloperScore{
		{Developer: "carol", WeightedPoints: 2},
	}, "2026-01-01", AggregationDeveloper)
	if strings.Contains(out, "NaN") || strings.Contains(out, "Inf") {
		t.Errorf("zero-cost report leaked NaN/Inf: %q", out)
	}
	if !strings.Contains(out, "TEAM TOTAL") {
		t.Errorf("missing TEAM TOTAL row; out=%q", out)
	}
}

// --- #133: ranking floor + bootstrap CI ---

// TestComputeDeveloper_OneLuckyPRIsUnranked is the C3 regression: a single
// 0.5-weight PR against $0.0004 of cost yields a stratospheric TIER that used
// to top the leaderboard. It must now be flagged unranked so it can't outrank a
// developer with real evidence behind their number.
func TestComputeDeveloper_OneLuckyPRIsUnranked(t *testing.T) {
	s := ComputeDeveloper("lucky",
		[]Outcome{{Developer: "lucky", IssueID: "1", Weight: 0.5, Quality: 1.0}},
		0.0004, 0.0004, 0)
	if s.Ranked {
		t.Errorf("one $0.0004 PR must be unranked, got Ranked=true (TIER=%v)", s.TIER)
	}
	if s.SampleN != 1 {
		t.Errorf("SampleN = %d, want 1", s.SampleN)
	}
	// The lucky TIER is still computed and carried (listed, not hidden).
	if s.TIER <= 0 {
		t.Errorf("unranked row should still carry its computed TIER, got %v", s.TIER)
	}
}

// TestComputeDeveloper_RankedRequiresBothFloors pins the two-threshold gate,
// including the >= boundaries on both dimensions.
func TestComputeDeveloper_RankedRequiresBothFloors(t *testing.T) {
	mkOutcomes := func(n int) []Outcome {
		out := make([]Outcome, n)
		for i := range out {
			out[i] = Outcome{Developer: "d", IssueID: "i", Weight: 1, Quality: 1.0}
		}
		return out
	}
	cases := []struct {
		name       string
		n          int
		costUSD    float64
		wantRanked bool
	}{
		{"n=3 and $5 exactly -> ranked (both boundaries)", 3, 5.00, true},
		{"n=2 and $500 -> unranked (too few outcomes)", 2, 500, false},
		{"n=50 and $4.99 -> unranked (below cost floor)", 50, 4.99, false},
		{"n=3 and $5.01 -> ranked", 3, 5.01, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := ComputeDeveloper("d", mkOutcomes(tc.n), tc.costUSD, tc.costUSD, 0)
			if s.Ranked != tc.wantRanked {
				t.Errorf("Ranked = %v, want %v (n=%d, cost=%.2f)",
					s.Ranked, tc.wantRanked, tc.n, tc.costUSD)
			}
			if s.SampleN != tc.n {
				t.Errorf("SampleN = %d, want %d", s.SampleN, tc.n)
			}
		})
	}
}

// TestFormatReport_UnrankedSortAfterRanked proves the two-tier order and the
// below-floor separator: a low-evidence row with a huge TIER lands after every
// ranked developer, under the separator, never above them.
func TestFormatReport_UnrankedSortAfterRanked(t *testing.T) {
	// lucky: enormous TIER but only 1 outcome / $0.0004 → unranked.
	lucky := ComputeDeveloper("lucky",
		[]Outcome{{Weight: 0.5, Quality: 1.0}}, 0.0004, 0.0004, 0)
	// alice: modest TIER but 3 outcomes / $10 → ranked.
	alice := ComputeDeveloper("alice",
		[]Outcome{{Weight: 3, Quality: 1}, {Weight: 5, Quality: 1}, {Weight: 8, Quality: 1}},
		10, 10, 0)

	out := FormatReport([]DeveloperScore{lucky, alice}, "2026-01-01", AggregationDeveloper)

	sep := strings.Index(out, "below ranking floor")
	ai := strings.Index(out, "alice")
	li := strings.Index(out, "lucky")
	if sep == -1 {
		t.Fatalf("missing below-floor separator; out=%q", out)
	}
	if ai == -1 || li == -1 {
		t.Fatalf("missing a developer row; out=%q", out)
	}
	if ai >= sep || sep >= li {
		t.Errorf("expected ranked alice before separator before unranked lucky; alice@%d sep@%d lucky@%d",
			ai, sep, li)
	}
}

// fixedDenomCI runs BootstrapCI with the whole denominator in the FIXED term (no
// per-outcome cost) — i.e. the pre-#495 numerator-only behaviour, expressed through
// the joint signature. The #133 percentile-machinery goldens are pinned through it,
// and the coverage control arm uses it as the "denominator pinned" comparator.
func fixedDenomCI(contribs []float64, totalCostUSD float64, b int, rng *rand.Rand) (lo, hi float64) {
	return BootstrapCI(contribs, make([]float64, len(contribs)), totalCostUSD, b, rng)
}

// TestBootstrapCI_Deterministic pins the golden interval for a fixed seed and
// asserts the point TIER lies inside it. Golden values captured with rand.NewPCG(1,2)
// under a FIXED denominator (all cost in the fixed term) — the #133 percentile
// machinery is unchanged by #495, so these goldens must still hold.
func TestBootstrapCI_Deterministic(t *testing.T) {
	contribs := []float64{3, 5, 8}
	const costUSD = 10.0
	rng := rand.New(rand.NewPCG(1, 2))
	lo, hi := fixedDenomCI(contribs, costUSD, 1000, rng)

	const wantLo, wantHi = 900, 2400 // golden: index 25 and 974 of sorted resamples
	if lo != wantLo || hi != wantHi {
		t.Errorf("BootstrapCI = [%v, %v], want golden [%v, %v]", lo, hi, wantLo, wantHi)
	}
	// Point TIER = (3+5+8)/(10/1000) = 1600 must sit inside the interval.
	const point = 1600.0
	if lo > point || point > hi {
		t.Errorf("point TIER %v not within [%v, %v]", point, lo, hi)
	}
}

// TestBootstrapCI_TrimsInteriorPercentiles proves the 2.5/97.5 index selection
// actually trims (unlike the [3,5,8] case, whose CI bounds ARE the support
// min/max, so any index in the tails would pass). With 10 contributions the
// all-min / all-max resamples occur with probability 1e-10, far rarer than
// 2.5%, so a correct index at 25/974 lands strictly inside the support: an
// off-by-one or 0/(b-1) index bug would push a bound to the support extreme and
// fail. Golden captured with rand.NewPCG(7,11), fixed denominator.
func TestBootstrapCI_TrimsInteriorPercentiles(t *testing.T) {
	contribs := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10} // n=10
	const costUSD = 10.0
	rng := rand.New(rand.NewPCG(7, 11))
	lo, hi := fixedDenomCI(contribs, costUSD, 1000, rng)

	// Support extremes: all-min sum=10 → TIER 1000; all-max sum=100 → TIER 10000.
	const supportLo, supportHi = 1000.0, 10000.0
	if lo <= supportLo || hi >= supportHi {
		t.Errorf("CI [%v, %v] not strictly inside support (%v, %v) — percentile trimming not applied",
			lo, hi, supportLo, supportHi)
	}
	const wantLo, wantHi = 3600, 7300 // golden at indices 25 and 974
	if lo != wantLo || hi != wantHi {
		t.Errorf("BootstrapCI = [%v, %v], want golden [%v, %v]", lo, hi, wantLo, wantHi)
	}
	// Point TIER = Σ(1..10)/(10/1000) = 55/0.01 = 5500, inside the interval.
	const point = 5500.0
	if lo > point || point > hi {
		t.Errorf("point TIER %v not within [%v, %v]", point, lo, hi)
	}
}

// TestBootstrapCI_DegenerateSingleOutcome: with one outcome every resample is
// that same outcome, so the interval collapses to the point estimate — and with a
// per-outcome cost the joint denominator collapses to that same single value too.
func TestBootstrapCI_DegenerateSingleOutcome(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	// The whole cost carried as this outcome's own cost (fixed term 0): every draw
	// is the single outcome, so num=4 and cost=8 every replicate → TIER 500.
	lo, hi := BootstrapCI([]float64{4}, []float64{8}, 0, 1000, rng)
	point := 4.0 / (8.0 / 1000.0) // 500
	if lo != point || hi != point {
		t.Errorf("single-outcome CI = [%v, %v], want lo==hi==%v", lo, hi, point)
	}
}

// TestBootstrapCI_ZeroCost: a non-positive TOTAL denominator (no per-outcome cost,
// no fixed cost) has no meaningful TIER, so the interval is (0,0) — no panic/Inf.
func TestBootstrapCI_ZeroCost(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	lo, hi := BootstrapCI([]float64{3, 5, 8}, []float64{0, 0, 0}, 0, 1000, rng)
	if lo != 0 || hi != 0 {
		t.Errorf("zero-cost CI = [%v, %v], want [0, 0]", lo, hi)
	}
}

// TestBootstrapCI_GuardsReturnZero pins the full "return (0,0), no panic" contract
// across every early-exit guard, including b<=0, a nil rng, a non-positive total
// cost, and the #495 mismatched-slice-length guard.
func TestBootstrapCI_GuardsReturnZero(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	z3 := []float64{0, 0, 0}
	cases := []struct {
		name      string
		contribs  []float64
		costs     []float64
		fixedCost float64
		b         int
		rng       *rand.Rand
	}{
		{"zero resamples", []float64{3, 5, 8}, z3, 10, 0, rng},
		{"negative resamples", []float64{3, 5, 8}, z3, 10, -5, rng},
		{"no contributions", nil, nil, 10, 1000, rng},
		{"non-positive total cost", []float64{3, 5, 8}, z3, 0, 1000, rng},
		{"negative total cost", []float64{3, 5, 8}, z3, -1, 1000, rng},
		{"mismatched slice lengths", []float64{3, 5, 8}, []float64{1, 2}, 10, 1000, rng},
		{"nil rng", []float64{3, 5, 8}, z3, 10, 1000, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lo, hi := BootstrapCI(tc.contribs, tc.costs, tc.fixedCost, tc.b, tc.rng)
			if lo != 0 || hi != 0 {
				t.Errorf("BootstrapCI = [%v, %v], want [0, 0]", lo, hi)
			}
		})
	}
}

// (Note: there is deliberately no "joint is always wider than pinned" test. With
// POSITIVE weight↔cost correlation the joint interval can be NARROWER — an
// expensive-large outcome partly cancels in the ratio — which is exactly the
// correlation #495 says pinning throws away. Coverage, not width, is the invariant.)

// TestBootstrapCI_Coverage is the #495 coverage control arm: on a synthetic
// population whose denominator genuinely varies, the JOINT interval achieves ~nominal
// 95% coverage of the true TIER, while the pre-#495 pinned-denominator interval
// UNDERcovers by a clear margin. A regression to pinning the denominator collapses
// the joint coverage toward the pinned number and fails this test.
func TestBootstrapCI_Coverage(t *testing.T) {
	if testing.Short() {
		t.Skip("coverage simulation is skipped under -short")
	}
	// Population of (contribution, cost) pairs: positive correlation (bigger work
	// costs more) plus heavy-tailed lognormal cost — the regime where pinning the
	// denominator understates the interval.
	type oc struct{ contrib, cost float64 }
	pgen := rand.New(rand.NewPCG(101, 202))
	pop := make([]oc, 200)
	var popC, popCost float64
	for i := range pop {
		w := 0.5 + 7.5*pgen.Float64()                    // contribution in [0.5, 8]
		c := 0.05 * w * math.Exp(1.1*pgen.NormFloat64()) // cost correlated with w, lognormal
		pop[i] = oc{w, c}
		popC += w
		popCost += c
	}
	trueTIER := popC / (popCost / 1000.0)

	const trials, n, b = 400, 25, 300
	rng := rand.New(rand.NewPCG(303, 404))
	var jointHits, pinnedHits int
	for tr := 0; tr < trials; tr++ {
		contribs := make([]float64, n)
		costs := make([]float64, n)
		var sampleCost float64
		for i := 0; i < n; i++ {
			o := pop[rng.IntN(len(pop))]
			contribs[i], costs[i] = o.contrib, o.cost
			sampleCost += o.cost
		}
		if jLo, jHi := BootstrapCI(contribs, costs, 0, b, rng); jLo <= trueTIER && trueTIER <= jHi {
			jointHits++
		}
		if pLo, pHi := fixedDenomCI(contribs, sampleCost, b, rng); pLo <= trueTIER && trueTIER <= pHi {
			pinnedHits++
		}
	}
	jointCov := float64(jointHits) / trials
	pinnedCov := float64(pinnedHits) / trials
	t.Logf("coverage: joint=%.3f pinned=%.3f (nominal 0.95, trueTIER=%.1f)", jointCov, pinnedCov, trueTIER)
	// Joint must be reasonably near nominal (bootstrap ratio CIs undercover a little
	// at n=25; allow Monte-Carlo slack).
	if jointCov < 0.85 {
		t.Errorf("joint coverage %.3f far below nominal 0.95 — denominator not resampled (#495)", jointCov)
	}
	// Control arm: pinning the denominator must undercover by a clear margin.
	if pinnedCov > jointCov-0.06 {
		t.Errorf("pinned-denominator coverage %.3f is not materially worse than joint %.3f — #495 fix has no measurable effect", pinnedCov, jointCov)
	}
}

// TestBootstrapCI_JointGolden pins the JOINT numeric path (#495) with a fixed seed on
// multi-outcome PAIRED data: both the numerator and the denominator are accumulated
// from ONE index set per replicate. A regression that draws separate indices for the
// two sums (breaking the weight↔cost pairing), or reverts to a fixed denominator,
// changes these goldens — a case the coverage aggregate alone might not catch if the
// broken interval still brackets the point TIER.
func TestBootstrapCI_JointGolden(t *testing.T) {
	contribs := []float64{3, 5, 8, 2}
	costs := []float64{0.4, 0.7, 1.2, 0.3}
	rng := rand.New(rand.NewPCG(3, 4))
	lo, hi := BootstrapCI(contribs, costs, 0, 1000, rng)

	const wantLo, wantHi = 6666.666666666667, 7272.727272727272 // golden captured with rand.NewPCG(3,4)
	if lo != wantLo || hi != wantHi {
		t.Errorf("joint BootstrapCI = [%v, %v], want golden [%v, %v]", lo, hi, wantLo, wantHi)
	}
	// Strict positive width — a joint interval on varied paired data must not collapse.
	if !(lo < hi) {
		t.Errorf("joint interval must have strict positive width, got [%v, %v]", lo, hi)
	}
	// Point TIER = Σcontrib / (Σcost/1000) must sit inside the interval.
	point := (3.0 + 5 + 8 + 2) / ((0.4 + 0.7 + 1.2 + 0.3) / 1000)
	if lo > point || point > hi {
		t.Errorf("point TIER %.1f not within [%v, %v]", point, lo, hi)
	}
}

// --- #136: zero-token-outcome tripwire ---

// TestComputeDeveloper_ZeroTokenOutcomeUnranks is the C6/G-02 unit regression:
// a developer who clears BOTH #133 floors but has a single zero-token outcome is
// forced unranked, the flag is counted, and — critically — the outcome's points
// are unchanged (visibility, not score surgery).
func TestComputeDeveloper_ZeroTokenOutcomeUnranks(t *testing.T) {
	// 3 outcomes / $10 cost clears both #133 floors; one carries ZeroToken.
	outcomes := []Outcome{
		{Developer: "d", IssueID: "1", Weight: 3, Quality: 1},
		{Developer: "d", IssueID: "2", Weight: 5, Quality: 1},
		{Developer: "d", IssueID: "3", Weight: 8, Quality: 1, ZeroToken: true},
	}
	s := ComputeDeveloper("d", outcomes, 10, 10, 0)

	if s.Ranked {
		t.Errorf("Ranked = true, want false (one zero-token outcome must unrank)")
	}
	if s.FlaggedOutcomes != 1 {
		t.Errorf("FlaggedOutcomes = %d, want 1", s.FlaggedOutcomes)
	}
	// Points are 3+5+8 = 16 regardless of the flag — the number is not altered.
	if s.WeightedPoints != 16 {
		t.Errorf("WeightedPoints = %v, want 16 (flag must not touch points)", s.WeightedPoints)
	}
	if s.SampleN != 3 {
		t.Errorf("SampleN = %d, want 3", s.SampleN)
	}
}

// TestComputeDeveloper_NoZeroTokenStaysRanked is the negative control: the same
// floors-clearing developer with all outcomes above the token floor stays ranked
// with zero flags. Pins that the tripwire only fires on ZeroToken outcomes.
func TestComputeDeveloper_NoZeroTokenStaysRanked(t *testing.T) {
	outcomes := []Outcome{
		{Developer: "d", IssueID: "1", Weight: 3, Quality: 1},
		{Developer: "d", IssueID: "2", Weight: 5, Quality: 1},
		{Developer: "d", IssueID: "3", Weight: 8, Quality: 1},
	}
	s := ComputeDeveloper("d", outcomes, 10, 10, 0)
	if !s.Ranked {
		t.Errorf("Ranked = false, want true (no flags, both floors cleared)")
	}
	if s.FlaggedOutcomes != 0 {
		t.Errorf("FlaggedOutcomes = %d, want 0", s.FlaggedOutcomes)
	}
}

// TestFormatReport_FidelityCopy pins the #136 relabel of the text report: the
// column header reads "Fidelity" (not "Coverage") and the footer states the
// of-CAPTURED-spend scoping plus the completeness caveat.
func TestFormatReport_FidelityCopy(t *testing.T) {
	out := FormatReport([]DeveloperScore{
		{Developer: "alice", TIER: 100, WeightedPoints: 3, TotalCostUSD: 10, CoveragePercent: 100, Ranked: true},
	}, "2026-01-01", AggregationDeveloper)

	if !strings.Contains(out, "Fidelity") {
		t.Errorf("report header missing 'Fidelity'; out=%q", out)
	}
	if strings.Contains(out, "Coverage") {
		t.Errorf("report still says 'Coverage'; out=%q", out)
	}
	// Footer scoping: "CAPTURED" and the not-measured completeness caveat.
	if !strings.Contains(out, "CAPTURED") {
		t.Errorf("footer missing of-CAPTURED-spend scoping; out=%q", out)
	}
	if !strings.Contains(out, "completeness of capture is NOT measured") {
		t.Errorf("footer missing completeness caveat; out=%q", out)
	}
}
