package scoring

// Guards for #722 (part of #710): the k-anonymity RESIDUAL row must be
// bit-reproducible.
//
// AggregateTeamsKAnon and CompareTeamsKAnon build their "other" cohort by ranging
// a map, and Go randomizes map iteration order per range. RollupTeam then
// float-sums that slice, and float addition is not associative, so before #722 two
// runs over IDENTICAL data could publish an "other" row whose weighted_points /
// total_cost_usd / actual_paid_usd — and the tier, cost_per_point, coverage_pct and
// **ranked** derived from them — differed.
//
// 🔴 SCOPE — AND THE FIRST VERSION OF THIS PARAGRAPH WAS WRONG, SO READ THE
// CORRECTION. It said these were "ULP-class: team rows carry no confidence interval
// and newTeamDeltaJSON hardcodes Significant:false, so no published boolean can flip
// here." Both cited facts are TRUE and the conclusion is FALSE. RollupTeam also
// computes:
//
//	ts.Ranked = flagged == 0 && sampleN >= MinRankedOutcomes &&
//	            ts.TotalCostUSD >= MinRankedCostUSD    // a THRESHOLD at exactly $5.00
//
// A threshold turns a last-ulp difference into a flipped BOOLEAN, and ts.Ranked
// reaches the wire (newTeamScoreJSON, and compare.go's Ranked: a.Ranked && b.Ranked)
// where its own doc calls the false value "load-bearing" (#603). Costs arrive as
// store.MicroToDollars(m) = float64(m)/1e6, which makes an exact $5.000000 total a
// REPRESENTABLE landing point rather than a measure-zero accident — so the boundary
// is MORE reachable here than with arbitrary floats, not less. Measured over all 720
// orders of {4, 4, 4, 4, 9, 4999975} micro: 360 sum to 5.0 (ranked TRUE) and 360 to
// 4.999999999999999 (ranked FALSE).
//
// ⇒ #722 is the SAME CLASS as #711 — a published boolean flipping on row order —
// at far lower probability, not a different and safer class.
// TestAggregateTeamsKAnon_ResidualRankedIsStableOnTheCostFloor is the arm that pins
// it, and it exists because this repo does not accept a "cannot flip" claim without
// a test that fails when it goes false.
//
// ⚠️ The scoping DOES hold for the two internal/api spend sites in
// spend_determinism_test.go: actual_paid_usd feeds no threshold (SpendLeverage gates
// on > 0; ComputeDeveloper's Ranked reads totalCostUSD, not actual-paid). Those are
// genuinely ULP-only. The over-claim was specific to these two engine.go sites.
//
// 🔴 THE VACUITY RISK IS THE WHOLE DIFFICULTY, and it has two independent halves,
// each with its own control below:
//
//	(a) Go randomizes PER RANGE, so a single-shot comparison passes by luck.
//	    Answered by looping orderIterations times and asserting exactly ONE
//	    distinct result (plus asserting the loop actually ran).
//	(b) A fixture can be order-INSENSITIVE, in which case (a) passes no matter what
//	    the code does. Float addition is COMMUTATIVE — a+b == b+a exactly — so a
//	    two-element residual can never detect anything; only associativity fails,
//	    which needs three or more addends of differing magnitude. Answered by
//	    TestResidualFixtureIsOrderSensitive, which proves the fixture's own values
//	    produce more than one sum under real Go map iteration.
//
// Without (b) this file would be the "test that cannot fail" shape #711 already hit.

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// orderIterations is the sample size for every map-order loop here. Go's map
// iteration randomization is not uniform over permutations, so the minority order
// can be rare: the six-value fixture below was measured at ~13% minority, giving
// P(miss) ≈ 0.87^1000 ≈ 1e-60. A handful of iterations would not do.
const orderIterations = 1000

// residualValues are the fixture's per-developer magnitudes, chosen by MEASUREMENT
// rather than by eye: ranged as a six-entry map 5000 times they produce two
// distinct float64 sums (~4337/663), which is what makes the loops below able to
// fail. TestResidualFixtureIsOrderSensitive re-measures this on every run so the
// property cannot rot silently.
//
// The per-field scale factors are exact powers of two (1, 2, 1/512) so each field
// carries a different magnitude while keeping bit-for-bit the same order
// sensitivity the control arm measures — a power-of-two scale only shifts the
// exponent, it cannot change which additions round.
var residualValues = []float64{
	1000.000001,
	0.000001,
	3.333333,
	999999.999999,
	0.000007,
	12345.678901,
}

// residualFixture builds a devScores slice and label map where every developer in
// residualValues sits in its OWN singleton group — sub-k at k=5, so all six fold
// into the OtherCohort residual — alongside one named group of five contributors
// that clears the floor. Six residual contributors is >= k, so the residual is
// EMITTED rather than suppressed by #593; a suppressed residual would make these
// tests assert nothing.
func residualFixture() ([]DeveloperScore, map[string]string) {
	var devs []DeveloperScore
	teamOf := map[string]string{}
	for i, v := range residualValues {
		d := DeveloperScore{
			Developer:      "resid-" + string(rune('a'+i)),
			WeightedPoints: v / 512,
			TotalCostUSD:   v,
			ActualPaidUSD:  v * 2,
			SampleN:        1,
		}
		devs = append(devs, d)
		teamOf[d.Developer] = "solo-" + string(rune('a'+i))
	}
	for i := 0; i < 5; i++ {
		d := DeveloperScore{
			Developer:      "named-" + string(rune('a'+i)),
			WeightedPoints: 2,
			TotalCostUSD:   10,
			ActualPaidUSD:  20,
			SampleN:        3,
		}
		devs = append(devs, d)
		teamOf[d.Developer] = "alpha"
	}
	return devs, teamOf
}

// sumBits is the identity under test: the three float fields RollupTeam
// accumulates, compared at BIT level. Comparing with == or a printed %.4f would
// hide exactly the last-ulp difference this guard exists to catch.
type sumBits struct {
	points, cost, paid uint64
}

func bitsOf(ts TeamScore) sumBits {
	return sumBits{
		points: math.Float64bits(ts.WeightedPoints),
		cost:   math.Float64bits(ts.TotalCostUSD),
		paid:   math.Float64bits(ts.ActualPaidUSD),
	}
}

func residualRow(t *testing.T, rows []TeamScore) TeamScore {
	t.Helper()
	for _, r := range rows {
		if r.Team == OtherCohort {
			return r
		}
	}
	t.Fatalf("no %q row in %d rows — the fixture stopped exercising the residual", OtherCohort, len(rows))
	return TeamScore{}
}

// TestResidualFixtureIsOrderSensitive is the VACUITY CONTROL for every other test
// in this file, and it is deliberately the first one.
//
// It reproduces the exact mechanism AggregateTeamsKAnon used before #722 — build
// the same map[label][]DeveloperScore, range it, concatenate, sum — and asserts
// that the fixture's own values yield MORE THAN ONE distinct sum. If this ever
// reports 1, the fixture has become order-insensitive and the determinism tests
// below are passing for free.
func TestResidualFixtureIsOrderSensitive(t *testing.T) {
	devs, teamOf := residualFixture()
	groups := map[string][]DeveloperScore{}
	for _, d := range devs {
		if teamOf[d.Developer] != "alpha" {
			groups[teamOf[d.Developer]] = append(groups[teamOf[d.Developer]], d)
		}
	}
	if len(groups) < 3 {
		t.Fatalf("residual groups = %d, want >= 3: float addition is commutative, so "+
			"two addends can NEVER expose an ordering difference", len(groups))
	}

	seen := map[uint64]int{}
	ran := 0
	for i := 0; i < orderIterations; i++ {
		var unsorted []DeveloperScore
		for _, g := range groups { // the pre-#722 shape: map iteration order
			unsorted = append(unsorted, g...)
		}
		seen[bitsOf(RollupTeam(OtherCohort, unsorted)).cost]++
		ran++
	}
	if ran != orderIterations {
		t.Fatalf("loop ran %d times, want %d", ran, orderIterations)
	}
	if len(seen) < 2 {
		t.Fatalf("the fixture produced %d distinct total_cost_usd over %d map ranges, want >= 2.\n"+
			"The determinism guards in this file are VACUOUS until residualValues is "+
			"replaced with values whose sum actually depends on addition order.", len(seen), ran)
	}
	t.Logf("fixture order-sensitivity: %d distinct total_cost_usd sums over %d map ranges (%v)", len(seen), ran, seen)
}

// TestAggregateTeamsKAnon_ResidualIsBitReproducible is the positive arm. Same
// input slice every call; the only thing that varies between iterations is Go's
// per-range map iteration order inside the function.
//
// NEGATIVE CONTROL: delete the sortLabeledScores(otherRows) call in
// AggregateTeamsKAnon and this must fail. Measured kill rate is recorded in the
// #722 PR — do not weaken the fixture without re-measuring it.
func TestAggregateTeamsKAnon_ResidualIsBitReproducible(t *testing.T) {
	devs, teamOf := residualFixture()

	seen := map[sumBits]int{}
	ran := 0
	for i := 0; i < orderIterations; i++ {
		rows, sup := AggregateTeamsKAnon(devs, teamOf, MinKAnonymity, store.ResemblesUnattributed)
		if sup.Residual {
			t.Fatalf("residual SUPPRESSED (%d < k=%d) — the fixture no longer publishes an "+
				"'other' row, so this test asserts nothing", sup.Developers, sup.K)
		}
		seen[bitsOf(residualRow(t, rows))]++
		ran++
	}
	if ran != orderIterations {
		t.Fatalf("loop ran %d times, want %d", ran, orderIterations)
	}
	if len(seen) != 1 {
		t.Errorf("residual row produced %d distinct (points,cost,paid) bit-triples over %d "+
			"identical calls, want exactly 1: %v", len(seen), ran, seen)
	}
}

// TestAggregateTeamsKAnon_ResidualInvariantToInputOrder is the stronger arm the
// by-DEVELOPER sort buys over merely iterating the group map in sorted label
// order: the residual is the same bits however the CALLER ordered its slice.
//
// It is asserted only of the residual. A NAMED row's developers come from
// groups[label], which preserves the caller's slice order by construction, so a
// permuted input legitimately re-orders a named row's summation — asserting
// invariance there would be asserting something untrue.
func TestAggregateTeamsKAnon_ResidualInvariantToInputOrder(t *testing.T) {
	devs, teamOf := residualFixture()
	canonical, _ := AggregateTeamsKAnon(devs, teamOf, MinKAnonymity, store.ResemblesUnattributed)
	want := bitsOf(residualRow(t, canonical))

	rng := rand.New(rand.NewPCG(0x722, 0x710)) // fixed seed: a failure reproduces
	shuffled := make([]DeveloperScore, len(devs))
	copy(shuffled, devs)

	ran := 0
	for i := 0; i < orderIterations; i++ {
		rng.Shuffle(len(shuffled), func(a, b int) {
			shuffled[a], shuffled[b] = shuffled[b], shuffled[a]
		})
		rows, _ := AggregateTeamsKAnon(shuffled, teamOf, MinKAnonymity, store.ResemblesUnattributed)
		if got := bitsOf(residualRow(t, rows)); got != want {
			t.Fatalf("iteration %d: residual bits = %v, want %v (input permutation changed the sum)", i, got, want)
		}
		ran++
	}
	if ran != orderIterations {
		t.Fatalf("loop ran %d times, want %d", ran, orderIterations)
	}
}

// rankedFloorMicro is a residual cost set, in integer micro-dollars, that sums to
// EXACTLY MinRankedCostUSD ($5.00) in some addition orders and one ulp below it in
// others. Six values because a residual needs >= MinKAnonymity (5) contributing
// developers to be EMITTED rather than suppressed by #593 — a suppressed residual
// would make this test assert nothing.
//
// Measured over all 720 permutations: 360 sum to 5.0 (ranked TRUE), 360 to
// 4.999999999999999 (ranked FALSE). A clean 50/50 split is deliberate — it is the
// most detectable boundary available, and it also means the assertion below cannot
// be satisfied by a lucky bias in Go's (non-uniform) map iteration.
var rankedFloorMicro = []int64{4, 4, 4, 4, 9, 4_999_975}

// microToDollars mirrors store.MicroToDollars EXACTLY. It is duplicated rather than
// imported because internal/scoring deliberately depends on nothing (see the package
// doc); if store's conversion ever changes, this constant-folded copy is what makes
// the divergence show up as a failing boundary test rather than as silence.
func microToDollars(m int64) float64 { return float64(m) / 1_000_000 }

// TestAggregateTeamsKAnon_ResidualRankedIsStableOnTheCostFloor pins the claim that
// #722 cannot flip a published boolean — the claim this file's header originally
// made WITHOUT a test, and which was wrong in exactly the way an untested "cannot"
// usually is.
//
// ts.Ranked gates on ts.TotalCostUSD >= MinRankedCostUSD. That is a threshold, and a
// threshold converts the last-ulp wobble the other tests measure into a boolean
// visible on the wire. This fixture parks the residual exactly on the $5.00 floor so
// the flip is reachable, then asserts it never happens.
//
// NEGATIVE CONTROL: delete sortLabeledScores(otherRows) in AggregateTeamsKAnon
// and this must fail with BOTH true and false observed. Measured kill rate is
// recorded in the #722 PR.
func TestAggregateTeamsKAnon_ResidualRankedIsStableOnTheCostFloor(t *testing.T) {
	var devs []DeveloperScore
	teamOf := map[string]string{}
	var totalMicro int64
	for i, m := range rankedFloorMicro {
		d := DeveloperScore{
			Developer:    "floor-" + string(rune('a'+i)),
			TotalCostUSD: microToDollars(m),
			SampleN:      1, // Σ SampleN = 6 >= MinRankedOutcomes, so cost is the only live gate
		}
		devs = append(devs, d)
		teamOf[d.Developer] = "solo-" + string(rune('a'+i))
		totalMicro += m
	}

	// Fixture preconditions, asserted rather than assumed. If the floor constant or
	// the micro scale ever moves, this fails HERE with a clear reason instead of
	// silently drifting off the boundary and passing forever.
	if len(devs) < MinKAnonymity {
		t.Fatalf("fixture has %d residual developers, want >= MinKAnonymity (%d): a sub-k "+
			"residual is SUPPRESSED and this test would assert nothing", len(devs), MinKAnonymity)
	}
	if got := microToDollars(totalMicro); got != MinRankedCostUSD {
		t.Fatalf("fixture sums to $%v, want exactly MinRankedCostUSD ($%v). The fixture is "+
			"no longer parked on the ranking floor, so a flipped boolean is unreachable "+
			"and this test is VACUOUS.", got, MinRankedCostUSD)
	}

	seenRanked := map[bool]int{}
	seenCost := map[uint64]int{}
	ran := 0
	for i := 0; i < orderIterations; i++ {
		rows, sup := AggregateTeamsKAnon(devs, teamOf, MinKAnonymity, store.ResemblesUnattributed)
		if sup.Residual {
			t.Fatalf("residual SUPPRESSED (%d < k=%d) — fixture no longer publishes an 'other' row",
				sup.Developers, sup.K)
		}
		r := residualRow(t, rows)
		seenRanked[r.Ranked]++
		seenCost[math.Float64bits(r.TotalCostUSD)]++
		ran++
	}
	if ran != orderIterations {
		t.Fatalf("loop ran %d times, want %d", ran, orderIterations)
	}
	if len(seenRanked) != 1 {
		t.Errorf("PUBLISHED BOOLEAN FLIPPED: residual `ranked` took %d distinct values over %d "+
			"identical calls, want exactly 1: %v (cost bits seen: %v).\n"+
			"This is #711's failure class, not a ulp-class cosmetic difference.",
			len(seenRanked), ran, seenRanked, seenCost)
	}
	if len(seenCost) != 1 {
		t.Errorf("residual total_cost_usd took %d distinct bit patterns over %d identical calls, "+
			"want 1: %v", len(seenCost), ran, seenCost)
	}
	// The boundary must be LIVE: a fixture that lands safely above the floor in every
	// order would pass the two assertions above while proving nothing.
	if seenRanked[true] == 0 {
		t.Errorf("residual is ranked=false in the sorted order — the fixture fell BELOW the " +
			"floor rather than landing on it, so the flip this test guards is unreachable")
	}
}

// TestCompareTeamsKAnon_ResidualIsBitReproducible is the same guard for the
// two-window compare path (#277), whose otherA/otherB slices were assembled by
// ranging teamSet. Both SIDES are asserted: they are sorted independently, and a
// fix that sorted only one would publish a reproducible "before" against a
// wobbling "after".
//
// NEGATIVE CONTROL: delete either sortLabeledScores call in
// CompareTeamsKAnon and this must fail.
func TestCompareTeamsKAnon_ResidualIsBitReproducible(t *testing.T) {
	devs, teamOf := residualFixture()
	// Window B differs from A by an exact power of two per developer, so B is a
	// genuinely different set of addends that keeps A's measured order sensitivity.
	bDevs := make([]DeveloperScore, len(devs))
	for i, d := range devs {
		d.WeightedPoints *= 4
		d.TotalCostUSD *= 4
		d.ActualPaidUSD *= 4
		bDevs[i] = d
	}

	seenA := map[sumBits]int{}
	seenB := map[sumBits]int{}
	ran := 0
	for i := 0; i < orderIterations; i++ {
		rows, sup := CompareTeamsKAnon(devs, bDevs, teamOf, MinKAnonymity, store.ResemblesUnattributed)
		if sup.Residual {
			t.Fatalf("residual SUPPRESSED (%d < k=%d) — fixture no longer exercises the paired residual",
				sup.Developers, sup.K)
		}
		var found bool
		for _, r := range rows {
			if r.Team == OtherCohort {
				seenA[bitsOf(r.A)]++
				seenB[bitsOf(r.B)]++
				found = true
			}
		}
		if !found {
			t.Fatalf("iteration %d: no %q comparison row in %d rows", i, OtherCohort, len(rows))
		}
		ran++
	}
	if ran != orderIterations {
		t.Fatalf("loop ran %d times, want %d", ran, orderIterations)
	}
	if len(seenA) != 1 {
		t.Errorf("side A residual produced %d distinct bit-triples over %d calls, want 1: %v", len(seenA), ran, seenA)
	}
	if len(seenB) != 1 {
		t.Errorf("side B residual produced %d distinct bit-triples over %d calls, want 1: %v", len(seenB), ran, seenB)
	}
}
