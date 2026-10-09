package scoring

import (
	"math"
	"math/rand/v2"
	"sort"
	"testing"
)

const cppEps = 1e-9

// TestComputeDeveloperCostPerPoint checks the core inverse-unit: cost_per_point
// = TotalCostUSD / WeightedPoints, deterministic, and the exact 1000/TIER dual.
func TestComputeDeveloperCostPerPoint(t *testing.T) {
	outcomes := []Outcome{
		{Developer: "alice", IssueID: "issue-1", Weight: 8, Quality: 1.0},
		{Developer: "alice", IssueID: "issue-2", Weight: 3, Quality: 1.0},
	}
	s := ComputeDeveloper("alice", outcomes, 20.85, 20.85, 0)

	wantPoints := 11.0
	wantCPP := 20.85 / wantPoints
	if math.Abs(s.CostPerPoint-wantCPP) > cppEps {
		t.Errorf("CostPerPoint = %v, want %v", s.CostPerPoint, wantCPP)
	}
	// cost_per_point is the exact inverse-unit dual of TIER: cpp == 1000/TIER.
	if math.Abs(s.CostPerPoint-1000.0/s.TIER) > 1e-6 {
		t.Errorf("CostPerPoint %v != 1000/TIER %v", s.CostPerPoint, 1000.0/s.TIER)
	}
	// Determinism: identical inputs → identical output.
	s2 := ComputeDeveloper("alice", outcomes, 20.85, 20.85, 0)
	if s.CostPerPoint != s2.CostPerPoint {
		t.Errorf("CostPerPoint not deterministic: %v vs %v", s.CostPerPoint, s2.CostPerPoint)
	}
}

// TestComputeDeveloperCostPerPointZeroPoints proves a zero-point row emits no
// misleading cost_per_point — the guard is on the points denominator, so the
// field stays 0 ("—") rather than dividing by zero.
func TestComputeDeveloperCostPerPointZeroPoints(t *testing.T) {
	// Points sum to 0 (a single quality-reverted-to-zero style outcome), but cost
	// was spent: cost_per_point must stay 0, not +Inf.
	outcomes := []Outcome{
		{Developer: "carol", IssueID: "issue-9", Weight: 5, Quality: 0.0},
	}
	s := ComputeDeveloper("carol", outcomes, 12.0, 12.0, 0)
	if s.WeightedPoints != 0 {
		t.Fatalf("precondition: WeightedPoints = %v, want 0", s.WeightedPoints)
	}
	if s.CostPerPoint != 0 {
		t.Errorf("CostPerPoint with zero points = %v, want 0 (no divide-by-zero)", s.CostPerPoint)
	}
	if math.IsInf(s.CostPerPoint, 0) || math.IsNaN(s.CostPerPoint) {
		t.Errorf("CostPerPoint = %v, want a finite 0", s.CostPerPoint)
	}
}

// TestComputeDeveloperCostPerPointZeroCost documents the intended behaviour of a
// $0-cost row that has points: a legitimate 0.0 (surfaced by the #136 tripwire),
// not a suppressed value — the guard is on points, not cost.
func TestComputeDeveloperCostPerPointZeroCost(t *testing.T) {
	outcomes := []Outcome{
		{Developer: "dave", IssueID: "issue-1", Weight: 3, Quality: 1.0},
	}
	s := ComputeDeveloper("dave", outcomes, 0, 0, 0)
	if s.CostPerPoint != 0 {
		t.Errorf("CostPerPoint with zero cost = %v, want 0.0", s.CostPerPoint)
	}
}

// TestRollupTeamCostPerPoint checks the team field is computed on summed totals,
// not an average of per-developer ratios.
func TestRollupTeamCostPerPoint(t *testing.T) {
	alice := DeveloperScore{Developer: "alice", WeightedPoints: 6.0, TotalCostUSD: 50.70}
	bob := DeveloperScore{Developer: "bob", WeightedPoints: 10.5, TotalCostUSD: 107.30}
	team := RollupTeam("backend", []DeveloperScore{alice, bob})

	wantCPP := (50.70 + 107.30) / (6.0 + 10.5)
	if math.Abs(team.CostPerPoint-wantCPP) > cppEps {
		t.Errorf("team CostPerPoint = %v, want %v", team.CostPerPoint, wantCPP)
	}
}

func TestRollupTeamCostPerPointZeroPoints(t *testing.T) {
	team := RollupTeam("empty", []DeveloperScore{{Developer: "x", TotalCostUSD: 5.0}})
	if team.CostPerPoint != 0 {
		t.Errorf("team CostPerPoint with zero points = %v, want 0", team.CostPerPoint)
	}
}

// TestCostPerPointCI checks the reciprocal-transform interval: the cost_per_point
// bounds are the reciprocals of the TIER bounds with the ends swapped, and a
// non-positive (unranked) bound yields (0, 0).
func TestCostPerPointCI(t *testing.T) {
	lo, hi := CostPerPointCI(100.0, 500.0)
	if math.Abs(lo-1000.0/500.0) > cppEps {
		t.Errorf("cpp CI low = %v, want %v", lo, 1000.0/500.0)
	}
	if math.Abs(hi-1000.0/100.0) > cppEps {
		t.Errorf("cpp CI high = %v, want %v", hi, 1000.0/100.0)
	}
	if lo >= hi {
		t.Errorf("cpp CI low %v must be < high %v", lo, hi)
	}
	// Unranked / degenerate bounds → (0, 0).
	if l, h := CostPerPointCI(0, 0); l != 0 || h != 0 {
		t.Errorf("CostPerPointCI(0,0) = (%v,%v), want (0,0)", l, h)
	}
	if l, h := CostPerPointCI(-1, 500); l != 0 || h != 0 {
		t.Errorf("CostPerPointCI(neg,pos) = (%v,%v), want (0,0)", l, h)
	}
}

// TestCostPerPointCIMatchesBootstrap proves the load-bearing design claim: the
// reciprocal transform CostPerPointCI(tierLo, tierHi) equals a DIRECT percentile
// bootstrap of cost_per_point — so skipping a second resample is exact, not an
// approximation. It runs an independent, identically-seeded resample of
// cost_per_point = cost/points_b, takes the SAME percentile indices BootstrapCI
// uses, and compares. This also guards the hidden dependency on BootstrapCI's
// index scheme being SYMMETRIC (hiIdx == b-1-loIdx): if that ever changes,
// cppLo (= scale/tierHi) stops equalling the direct 2.5th percentile and this
// test fails — which the old tautological assertion (re-checking the function's
// own body) did not catch.
func TestCostPerPointCIMatchesBootstrap(t *testing.T) {
	contribs := []float64{8, 3, 5, 1, 3, 5}
	const cost = 42.0
	const b = DefaultBootstrapSamples

	rng := rand.New(rand.NewPCG(1, 2))
	// Fixed denominator (all cost in the fixed term): the cost_per_point = 1000/tier
	// reciprocal identity this test checks holds per replicate regardless, and the
	// draw order is identical to the pre-#495 call so the golden comparison stands.
	tierLo, tierHi := BootstrapCI(contribs, make([]float64, len(contribs)), cost, b, rng)
	cppLo, cppHi := CostPerPointCI(tierLo, tierHi)

	// Independent direct bootstrap of cost_per_point, SAME seed/stream/draw order
	// as BootstrapCI, so each resample's point-sum matches.
	rng2 := rand.New(rand.NewPCG(1, 2))
	n := len(contribs)
	samples := make([]float64, b)
	for i := range samples {
		var sum float64
		for j := 0; j < n; j++ {
			sum += contribs[rng2.IntN(n)]
		}
		samples[i] = cost / sum // cost_per_point for this resample
	}
	sort.Float64s(samples)

	// The exact percentile indices BootstrapCI uses (bootstrap.go).
	loIdx := int(math.Floor(0.025 * float64(b)))
	hiIdx := int(math.Ceil(0.975*float64(b))) - 1
	wantLo, wantHi := samples[loIdx], samples[hiIdx]

	if math.Abs(cppLo-wantLo) > 1e-9 {
		t.Errorf("cost_per_point CI low: reciprocal=%v, direct bootstrap=%v", cppLo, wantLo)
	}
	if math.Abs(cppHi-wantHi) > 1e-9 {
		t.Errorf("cost_per_point CI high: reciprocal=%v, direct bootstrap=%v", cppHi, wantHi)
	}
}

// TestLabelCultureDivergentTIERConvergentCostPerPoint is the issue #239 acceptance
// test, made falsifiable: two orgs with IDENTICAL activity (same real work, same
// cost) but DIFFERENT label cultures show divergent TIER under their own local
// labels, yet CONVERGENT cost_per_point once both are scored under the shared
// canonical rubric.
//
// The convergence comes from the CANONICAL RUBRIC, not from arithmetic: raw
// cost_per_point under each org's local labels diverges exactly as raw TIER does
// (it is the reciprocal). The methodology point the test pins is that
// cost_per_point is only cross-culture comparable within a matched rubric_version
// — which is precisely what closes the generous-vs-strict exploit.
func TestLabelCultureDivergentTIERConvergentCostPerPoint(t *testing.T) {
	// Three units of identical real work; both orgs spent the same $30.
	const cost = 30.0

	// Canonical rubric weights for that real work (what the size actually is).
	canonicalWeights := []float64{3, 5, 1} // m, l, xs → 9 points

	// Org A labels GENEROUSLY: every unit bumped up one size.
	orgALocal := []Outcome{
		{Developer: "a", IssueID: "1", Weight: 5, Quality: 1}, // l  (was m)
		{Developer: "a", IssueID: "2", Weight: 8, Quality: 1}, // xl (was l)
		{Developer: "a", IssueID: "3", Weight: 3, Quality: 1}, // m  (was xs)
	}
	// Org B labels STRICTLY: every unit bumped down one size.
	orgBLocal := []Outcome{
		{Developer: "b", IssueID: "1", Weight: 1, Quality: 1},   // s  (was m)
		{Developer: "b", IssueID: "2", Weight: 3, Quality: 1},   // m  (was l)
		{Developer: "b", IssueID: "3", Weight: 0.5, Quality: 1}, // xs (was xs, floor)
	}

	aLocal := ComputeDeveloper("a", orgALocal, cost, cost, 0)
	bLocal := ComputeDeveloper("b", orgBLocal, cost, cost, 0)

	// 1) DIVERGENT TIER under local labels: the generous org "wins" the naive
	// comparison at identical real efficiency — the exploit the issue describes.
	if !(aLocal.TIER > bLocal.TIER) {
		t.Fatalf("expected generous org TIER > strict org TIER, got %v vs %v", aLocal.TIER, bLocal.TIER)
	}
	tierRatio := aLocal.TIER / bLocal.TIER
	if tierRatio < 2.0 {
		t.Fatalf("local TIER should diverge widely; ratio = %v, want > 2", tierRatio)
	}

	// Guard the honesty claim: raw cost_per_point under LOCAL labels diverges too
	// (it is 1000/TIER). Convergence therefore CANNOT come from the unit alone.
	if math.Abs(aLocal.CostPerPoint-bLocal.CostPerPoint) < 0.5 {
		t.Fatalf("local cost_per_point should also diverge (%v vs %v); convergence must come from the rubric, not the unit",
			aLocal.CostPerPoint, bLocal.CostPerPoint)
	}

	// 2) Re-score BOTH orgs under the shared canonical rubric (same weights for the
	// same real work), holding cost fixed.
	canon := func(dev string) []Outcome {
		out := make([]Outcome, len(canonicalWeights))
		for i, w := range canonicalWeights {
			out[i] = Outcome{Developer: dev, IssueID: string(rune('1' + i)), Weight: w, Quality: 1}
		}
		return out
	}
	aCanon := ComputeDeveloper("a", canon("a"), cost, cost, 0)
	bCanon := ComputeDeveloper("b", canon("b"), cost, cost, 0)

	// CONVERGENT cost_per_point under the canonical rubric: identical activity →
	// identical cost_per_point once the weight rubric is matched.
	if math.Abs(aCanon.CostPerPoint-bCanon.CostPerPoint) > cppEps {
		t.Errorf("canonical cost_per_point diverged: %v vs %v (want convergent)",
			aCanon.CostPerPoint, bCanon.CostPerPoint)
	}
	// Pin the actual value (cost / Σ canonical weights), so a wrong formula (e.g.
	// cost/points²) can't pass on equality-to-itself alone.
	var canonPoints float64
	for _, w := range canonicalWeights {
		canonPoints += w
	}
	wantCanonCPP := cost / canonPoints // 30 / 9
	if math.Abs(aCanon.CostPerPoint-wantCanonCPP) > cppEps {
		t.Errorf("canonical cost_per_point = %v, want %v (cost/Σweights)", aCanon.CostPerPoint, wantCanonCPP)
	}
}
