package api

import (
	"math"
	"testing"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// cppVal derefs a cost_per_point pointer, failing the test if it is nil. Post-#472 a
// row WITH accepted points always carries a value (including 0 for a FREE row); nil
// means zero points, which these value assertions never expect.
func cppVal(t *testing.T, p *float64) float64 {
	t.Helper()
	if p == nil {
		t.Fatal("cost_per_point is nil (expected a value for a row with points)")
	}
	return *p
}

// segmentFor returns the work-type segment for wt, or a zero value + false.
func segmentFor(resp scoresResponse, wt string) (workTypeSegmentJSON, bool) {
	for _, s := range resp.WorkTypes {
		if s.WorkType == wt {
			return s, true
		}
	}
	return workTypeSegmentJSON{}, false
}

// TestGetScores_RubricVersionStamped pins deliverable 2: the canonical rubric
// version is surfaced at the top level of /scores, exactly like price_table.
// Fails on main — the field does not exist.
func TestGetScores_RubricVersionStamped(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 2.0)
	seedOutcome(t, db, "alice", "issue-1", 3, 1)

	resp := decodeScores(t, h)
	if resp.Rubric.Version != scoring.RubricVersion {
		t.Errorf("rubric.version = %d, want %d (scoring.RubricVersion)", resp.Rubric.Version, scoring.RubricVersion)
	}
	// price_table is still stamped alongside it — the two version stamps that a
	// matched-comparison requires travel together.
	if resp.PriceTable.Version < 1 {
		t.Errorf("price_table.version = %d, want >= 1", resp.PriceTable.Version)
	}
}

// TestGetScores_CostPerPointDeveloperAndSegment pins deliverable 1: cost_per_point
// = total_cost_usd / weighted_points on both the per-developer row and the
// work_type segment, and its self-relative CI is the reciprocal of the TIER CI.
func TestGetScores_CostPerPointDeveloperAndSegment(t *testing.T) {
	h, db := newTestHandler(t)
	// 3 outcomes clear the #133 ranking floor; points 3+5+1 = 9, cost $9 → cpp 1.0.
	seedCosts(t, db, "alice", "issue-1", 3.0)
	seedCosts(t, db, "alice", "issue-2", 5.0)
	seedCosts(t, db, "alice", "issue-3", 1.0)
	seedOutcome(t, db, "alice", "issue-1", 3, 1)
	seedOutcome(t, db, "alice", "issue-2", 5, 1)
	seedOutcome(t, db, "alice", "issue-3", 1, 1)

	resp := decodeScores(t, h)
	d, ok := scoresDevsFrom(resp)["alice"]
	if !ok {
		t.Fatal("alice absent from /scores")
	}
	cpp := cppVal(t, d.CostPerPoint)
	if math.Abs(cpp-1.0) > 1e-9 {
		t.Errorf("cost_per_point = %v, want 1.0 ($9 / 9 points)", cpp)
	}
	// Exact inverse-unit dual of TIER.
	if math.Abs(cpp-1000.0/d.TIER) > 1e-6 {
		t.Errorf("cost_per_point %v != 1000/TIER %v", cpp, 1000.0/d.TIER)
	}
	// Ranked → self-relative CI populated as the reciprocal (ends swapped) of the
	// TIER CI (#239 item 3).
	if !d.Ranked {
		t.Fatalf("alice should be ranked (3 outcomes, $9); ranked=%v", d.Ranked)
	}
	if d.CostPerPointCILow <= 0 || d.CostPerPointCIHigh <= 0 {
		t.Fatalf("cost_per_point CI absent: [%v, %v]", d.CostPerPointCILow, d.CostPerPointCIHigh)
	}
	if math.Abs(d.CostPerPointCILow-1000.0/d.CIHigh) > 1e-9 ||
		math.Abs(d.CostPerPointCIHigh-1000.0/d.CILow) > 1e-9 {
		t.Errorf("cost_per_point CI [%v,%v] is not the reciprocal of TIER CI [%v,%v]",
			d.CostPerPointCILow, d.CostPerPointCIHigh, d.CILow, d.CIHigh)
	}

	// The work_type segment (default = feature) carries cost_per_point too.
	seg, ok := segmentFor(resp, store.WorkTypeFeature)
	if !ok {
		t.Fatalf("feature segment absent; got %d segments", len(resp.WorkTypes))
	}
	if seg.Total == nil || math.Abs(cppVal(t, seg.Total.CostPerPoint)-1.0) > 1e-9 {
		t.Errorf("feature segment total cost_per_point = %+v, want 1.0", seg.Total)
	}
	var segDev *developerScoreJSON
	for i := range seg.Developers {
		if seg.Developers[i].Developer == "alice" {
			segDev = &seg.Developers[i]
		}
	}
	if segDev == nil {
		t.Fatal("alice absent from feature segment")
	}
	if cpp := cppVal(t, segDev.CostPerPoint); math.Abs(cpp-1.0) > 1e-9 {
		t.Errorf("segment cost_per_point = %v, want 1.0", cpp)
	}
}

// TestGetScores_CostPerPointZeroPointsNoMisleadingValue pins the #472 guard: a
// developer whose weighted points sum to 0 (a fully quality-reverted outcome) but who
// spent real money must report cost_per_point == NULL — never 0 (which would sort as
// the MOST efficient row), never +Inf, never a tiny "best in class" figure.
func TestGetScores_CostPerPointZeroPointsNoMisleadingValue(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "zero", "issue-1", 4.0)
	seedOutcome(t, db, "zero", "issue-1", 5, 0) // weight 5, quality 0 → 0 points

	d, ok := scoresDevsFrom(decodeScores(t, h))["zero"]
	if !ok {
		t.Fatal("developer zero absent from /scores")
	}
	if d.WeightedPoints != 0 {
		t.Fatalf("precondition: weighted_points = %v, want 0", d.WeightedPoints)
	}
	if d.CostPerPoint != nil {
		t.Errorf("cost_per_point with zero points = %v, want null (#472: 'no accepted outcome' must not encode as the most-efficient 0)", *d.CostPerPoint)
	}
	// This row is also unranked (1 outcome, zero points): its cost_per_point CI —
	// the reciprocal transform of the (0,0) TIER CI — must be (0,0), never a
	// spurious interval.
	if d.Ranked {
		t.Errorf("precondition: expected unranked row, got ranked=%v", d.Ranked)
	}
	if d.CostPerPointCILow != 0 || d.CostPerPointCIHigh != 0 {
		t.Errorf("unranked cost_per_point CI = [%v, %v], want [0, 0]", d.CostPerPointCILow, d.CostPerPointCIHigh)
	}
}

// TestGetScores_DivergentTIERWithLabelCulture is the wire-level face of the #239
// acceptance test: two developers with IDENTICAL cost and identical real work but
// different label generosity produce DIVERGENT TIER — the exploit cost_per_point +
// the versioned rubric exist to neutralize. (The convergence of cost_per_point once
// both are re-scored under the canonical rubric is proven deterministically in
// scoring.TestLabelCultureDivergentTIERConvergentCostPerPoint.)
func TestGetScores_DivergentTIERWithLabelCulture(t *testing.T) {
	h, db := newTestHandler(t)
	// Identical spend per unit of work for both developers.
	for _, iss := range []string{"issue-1", "issue-2", "issue-3"} {
		seedCosts(t, db, "generous", iss, 3.0)
		seedCosts(t, db, "strict", iss, 3.0)
	}
	// generous labels each unit a size up; strict a size down. Same real work.
	seedOutcome(t, db, "generous", "issue-1", 5, 1)
	seedOutcome(t, db, "generous", "issue-2", 8, 1)
	seedOutcome(t, db, "generous", "issue-3", 3, 1)
	seedOutcome(t, db, "strict", "issue-1", 1, 1)
	seedOutcome(t, db, "strict", "issue-2", 3, 1)
	seedOutcome(t, db, "strict", "issue-3", 0.5, 1)

	devs := scoresDevsFrom(decodeScores(t, h))
	g, s := devs["generous"], devs["strict"]
	if !(g.TIER > s.TIER) {
		t.Fatalf("generous TIER %v should exceed strict TIER %v (label-culture exploit)", g.TIER, s.TIER)
	}
	if g.TIER/s.TIER < 2.0 {
		t.Errorf("expected wide TIER divergence from labeling; ratio = %v", g.TIER/s.TIER)
	}
}

// TestGetScores_CostPerPointDisambiguatesFreeVsZeroOutcome is the #472 control arm:
// the value 0 is OVERLOADED, so the encoding must disambiguate the two rows that both
// produced it. A FREE row (real accepted points, zero captured cost) is genuinely the
// MOST efficient and keeps cost_per_point = 0; a zero-outcome row (real spend, no
// accepted points) is pure waste and must be NULL — never 0, which would sort it as
// the most efficient row for any consumer ranking by the column.
func TestGetScores_CostPerPointDisambiguatesFreeVsZeroOutcome(t *testing.T) {
	h, db := newTestHandler(t)
	// FREE: accepted work, no cost captured (e.g. a bot with $0 metered spend, #499).
	seedOutcome(t, db, "free", "issue-1", 5, 1) // 5 weighted points, quality 1
	// WASTE: real spend, a fully quality-reverted outcome (#472's own repro).
	seedCosts(t, db, "waste", "issue-2", 4.0)
	seedOutcome(t, db, "waste", "issue-2", 5, 0) // weight 5, quality 0 -> 0 points

	devs := scoresDevsFrom(decodeScores(t, h))

	free, ok := devs["free"]
	if !ok {
		t.Fatal("free developer absent from /scores")
	}
	if free.WeightedPoints <= 0 {
		t.Fatalf("precondition: FREE row must have accepted points, got %v", free.WeightedPoints)
	}
	if free.CostPerPoint == nil {
		t.Fatal("FREE row (points, zero cost) must carry cost_per_point = 0, not null — it is genuinely the most efficient")
	}
	if *free.CostPerPoint != 0 {
		t.Errorf("FREE cost_per_point = %v, want 0", *free.CostPerPoint)
	}

	waste, ok := devs["waste"]
	if !ok {
		t.Fatal("waste developer absent from /scores")
	}
	if waste.WeightedPoints != 0 {
		t.Fatalf("precondition: WASTE row must have 0 points, got %v", waste.WeightedPoints)
	}
	if waste.CostPerPoint != nil {
		t.Errorf("zero-outcome cost_per_point = %v, want null (must not encode as the most-efficient 0)", *waste.CostPerPoint)
	}
}
