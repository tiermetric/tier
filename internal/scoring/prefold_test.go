package scoring

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"sort"
	"testing"
)

// foldRelTol bounds how far a folded residual float may sit from
// AggregateLabeledKAnon's. The generator's addends are non-negative (credit
// memos have their own test). Two summation orders of n such addends differ by
// up to about 2(n-1)·2⁻⁵³ of the sum (n ≤ 75: ~1.6e-14), and a quotient adds
// both sums' errors (~3.3e-14); measured max 9.2e-16. The smallest dropped
// addend is about 2.7e-12 of the largest residual. Named rows are compared bit
// for bit.
const foldRelTol = 1e-13

const foldIterations = 3000

func foldKey(prefix string) func(string) string {
	return func(id string) string { return prefix + id }
}

func genFoldRow(rng *rand.Rand, dev string) DeveloperScore {
	d := DeveloperScore{Developer: dev}
	if rng.IntN(5) == 0 {
		d.ActualPaidUSD = microToDollars(rng.Int64N(9_000_000) + 1)
	}
	if rng.IntN(6) == 0 {
		return d // idle or paid-only seat
	}
	d.SampleN = rng.IntN(4)
	if d.SampleN > 0 {
		d.WeightedPoints = float64(rng.IntN(40)) / 4
		d.FlaggedOutcomes = rng.IntN(d.SampleN+1) * rng.IntN(2)
	}
	if rng.IntN(4) > 0 {
		// Mixed magnitudes, so re-association can round differently.
		d.TotalCostUSD = microToDollars(rng.Int64N(5_000_000)+1) * math.Pow(10, float64(rng.IntN(4)))
		realtime := [3]float64{0, d.TotalCostUSD, d.TotalCostUSD * rng.Float64()}[rng.IntN(3)]
		d.CoveragePercent = realtime / d.TotalCostUSD * 100
		if rng.IntN(4) > 0 {
			d.CapturedRealtimeUSD = realtime
		}
		if rng.IntN(4) > 0 {
			d.CapturedNonRealtimeUSD = d.TotalCostUSD - realtime
		}
	}
	return d
}

// genFoldWindow gives each person one to three rows under random labels, a
// person sometimes holding two labels, or two rows under one.
func genFoldWindow(rng *rand.Rand, people, labels []string) ([]LabeledScore, Census) {
	var rows []LabeledScore
	for _, p := range people {
		for n := rng.IntN(3) + 1; n > 0; n-- {
			rows = append(rows, LabeledScore{Label: labels[rng.IntN(len(labels))], Score: genFoldRow(rng, p)})
		}
	}
	rng.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
	uncounted, pseudo := map[string]bool{}, map[string]bool{}
	for _, p := range people {
		uncounted[p] = rng.IntN(7) == 0
		pseudo[p] = rng.IntN(10) == 0
	}
	return rows, Census{
		Uncounted: func(d string) bool { return uncounted[d] },
		UncapturedCost: func(r DeveloperScore, realtime bool) bool {
			if realtime {
				return r.CapturedRealtimeUSD <= 0
			}
			return r.CapturedNonRealtimeUSD <= 0
		},
		Pseudo: func(d string) bool { return pseudo[d] },
	}
}

func genFoldPools(rng *rand.Rand) (people, labels []string) {
	for i := rng.IntN(24) + 2; i > 0; i-- {
		people = append(people, fmt.Sprintf("dev-%02d", i))
	}
	all := []string{"", OtherCohort, "alpha", "beta", "gamma", "delta"}
	rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	return people, all[:rng.IntN(len(all))+1]
}

func shuffled(rng *rand.Rand, in []LabelInput) []LabelInput {
	rng.Shuffle(len(in), func(i, j int) { in[i], in[j] = in[j], in[i] })
	return in
}

type foldDrift struct {
	inexact, residuals int
	maxRel             float64
}

func sameTeamScore(t *testing.T, where string, got, want TeamScore, drift *foldDrift) {
	t.Helper()
	if got.Team != want.Team || got.Developers != nil {
		t.Fatalf("%s: got team=%q devs=%v, want team=%q", where, got.Team, got.Developers, want.Team)
	}
	// The residual's Ranked may differ only when both costs sit on the
	// MinRankedCostUSD floor (TestFolded_ResidualRankedDivergesAtCostFloor).
	onFloor := want.Team == OtherCohort &&
		math.Abs(got.TotalCostUSD-MinRankedCostUSD) <= foldRelTol*MinRankedCostUSD &&
		math.Abs(want.TotalCostUSD-MinRankedCostUSD) <= foldRelTol*MinRankedCostUSD
	if got.Ranked != want.Ranked && !onFloor {
		t.Fatalf("%s: team %q ranked=%v, want %v", where, want.Team, got.Ranked, want.Ranked)
	}
	g := [...]float64{got.TIER, got.WeightedPoints, got.TotalCostUSD, got.ActualPaidUSD, got.SpendLeverage, got.CoveragePercent, got.CostPerPoint}
	w := [...]float64{want.TIER, want.WeightedPoints, want.TotalCostUSD, want.ActualPaidUSD, want.SpendLeverage, want.CoveragePercent, want.CostPerPoint}
	exact := g == w
	if want.Team == OtherCohort {
		drift.residuals++
		if !exact {
			drift.inexact++
		}
		for i := range g {
			if g[i] != w[i] {
				drift.maxRel = math.Max(drift.maxRel, math.Abs(g[i]-w[i])/math.Max(math.Abs(g[i]), math.Abs(w[i])))
			}
			if math.Abs(g[i]-w[i]) > foldRelTol*math.Max(math.Abs(g[i]), math.Abs(w[i])) {
				t.Fatalf("%s: residual field %d = %v, want %v", where, i, g[i], w[i])
			}
		}
	} else if !exact {
		t.Fatalf("%s: named row %q figures %v, want bit-identical %v", where, want.Team, g, w)
	}
}

func TestFolded_MatchesLabeledKAnon(t *testing.T) {
	rng := rand.New(rand.NewPCG(913, 1))
	var drift foldDrift
	var namedSeen, withheld int
	for it := 0; it < foldIterations; it++ {
		people, labels := genFoldPools(rng)
		rows, census := genFoldWindow(rng, people, labels)
		k := rng.IntN(6) + 1
		want, wantSup := AggregateLabeledKAnon(rows, k, census)
		got, gotSup := AggregateFolded(shuffled(rng, PreFold(rows, census, foldKey("h:"))), k)
		where := fmt.Sprintf("iteration %d (k=%d)", it, k)
		if gotSup != wantSup || len(got) != len(want) {
			t.Fatalf("%s: got %d rows %+v, want %d rows %+v", where, len(got), gotSup, len(want), wantSup)
		}
		for i := range want {
			sameTeamScore(t, where, got[i], want[i], &drift)
			if want[i].Team != OtherCohort {
				namedSeen++
			}
		}
		if wantSup.Residual {
			withheld++
		}
	}
	if namedSeen < 100 || withheld < 100 || drift.residuals < 100 {
		t.Fatalf("generator too narrow: %d named rows, %d withheld, %d emitted residuals", namedSeen, withheld, drift.residuals)
	}
	t.Logf("%d emitted residuals, %d not bit-identical (max relative difference %g, tolerance %g); %d named rows bit-identical",
		drift.residuals, drift.inexact, drift.maxRel, foldRelTol, namedSeen)
}

func TestFolded_MatchesCompareLabeledKAnon(t *testing.T) {
	rng := rand.New(rand.NewPCG(913, 2))
	var drift foldDrift
	var namedSeen, withheld int
	for it := 0; it < foldIterations; it++ {
		people, labels := genFoldPools(rng)
		aRows, aCensus := genFoldWindow(rng, people, labels)
		bRows, bCensus := genFoldWindow(rng, people, labels)
		k := rng.IntN(6) + 1
		want, wantSup := CompareLabeledKAnon(aRows, bRows, k, aCensus, bCensus)
		got, gotSup := CompareFolded(shuffled(rng, PreFold(aRows, aCensus, foldKey("a:"))),
			shuffled(rng, PreFold(bRows, bCensus, foldKey("b:"))), k)
		where := fmt.Sprintf("iteration %d (k=%d)", it, k)
		if gotSup != wantSup || len(got) != len(want) {
			t.Fatalf("%s: got %d rows %+v, want %d rows %+v", where, len(got), gotSup, len(want), wantSup)
		}
		for i := range want {
			if got[i].Team != want[i].Team {
				t.Fatalf("%s: row %d team %q, want %q", where, i, got[i].Team, want[i].Team)
			}
			sameTeamScore(t, where+" A", got[i].A, want[i].A, &drift)
			sameTeamScore(t, where+" B", got[i].B, want[i].B, &drift)
			if want[i].Team != OtherCohort {
				namedSeen++
			}
		}
		if wantSup.Residual {
			withheld++
		}
	}
	if namedSeen < 50 || withheld < 100 || drift.residuals < 100 {
		t.Fatalf("generator too narrow: %d named rows, %d withheld, %d emitted residual sides", namedSeen, withheld, drift.residuals)
	}
	t.Logf("%d emitted residual sides, %d not bit-identical (max relative difference %g, tolerance %g); %d named rows bit-identical",
		drift.residuals, drift.inexact, drift.maxRel, foldRelTol, namedSeen)
}

// oneSeatRows puts x in two sub-k labels beside one other person: two people,
// three (person, label) pairs, so k=3 withholds the residual only when x fills one seat.
func oneSeatRows() []LabeledScore {
	return []LabeledScore{
		{Label: "s1", Score: dev("x", 1, 2)},
		{Label: "s2", Score: dev("x", 1, 2)},
		{Label: "s2", Score: dev("y", 1, 2)},
	}
}

func TestFolded_PersonInTwoLabelsOneResidualSeat(t *testing.T) {
	want, wantSup := AggregateLabeledKAnon(oneSeatRows(), 3, countAll)
	got, gotSup := AggregateFolded(PreFold(oneSeatRows(), countAll, foldKey("h:")), 3)
	if !gotSup.Residual || gotSup.Developers != 2 || len(got) != 0 {
		t.Fatalf("got %v %+v, want the residual withheld with 2 people (x once)", got, gotSup)
	}
	if gotSup != wantSup || len(want) != 0 {
		t.Fatalf("folded %+v, labeled %v %+v", gotSup, want, wantSup)
	}
}

func TestFolded_TombstonedKeyStillOneSeat(t *testing.T) {
	inputs := PreFold(oneSeatRows(), countAll, foldKey("h:"))
	const tomb = "\x00tomb:5f1d0c9e7a2b"
	swaps := 0
	for i := range inputs {
		swap := func(keys []string) []string {
			for j := range keys {
				if keys[j] == "h:x" {
					keys[j] = tomb
					swaps++
				}
			}
			sort.Strings(keys)
			return keys
		}
		inputs[i].People = swap(inputs[i].People)
		for m := range inputs[i].Carriers {
			inputs[i].Carriers[m] = swap(inputs[i].Carriers[m])
		}
	}
	if swaps == 0 {
		t.Fatal("no key was tombstoned; the test would assert nothing")
	}
	got, sup := AggregateFolded(inputs, 3)
	if !sup.Residual || sup.Developers != 2 || len(got) != 0 {
		t.Fatalf("got %v %+v, want the residual withheld with 2 people (the tombstone once)", got, sup)
	}
}

func TestFolded_ResidualRankedStableOnCostFloor(t *testing.T) {
	inputs := PreFold(floorRows([]int{0, 1, 2, 3, 4, 5}), countAll, foldKey("h:"))
	rng := rand.New(rand.NewPCG(0x722, 913))
	seenRanked, seenCost := map[bool]int{}, map[uint64]int{}
	for i := 0; i < orderIterations; i++ {
		rng.Shuffle(len(inputs), func(a, b int) { inputs[a], inputs[b] = inputs[b], inputs[a] })
		got, sup := AggregateFolded(inputs, MinKAnonymity)
		if sup.Residual {
			t.Fatalf("residual withheld (%+v); the fixture must publish it", sup)
		}
		r := residualRow(t, got)
		seenRanked[r.Ranked]++
		seenCost[math.Float64bits(r.TotalCostUSD)]++
	}
	if len(seenRanked) != 1 || len(seenCost) != 1 {
		t.Fatalf("input order changed the residual: ranked %v, cost bits %v", seenRanked, seenCost)
	}
}

// floorRows puts the #722 fixture's i-th developer under label solo-<perm[i]>.
func floorRows(perm []int) []LabeledScore {
	var rows []LabeledScore
	for i, m := range rankedFloorMicro {
		d := DeveloperScore{Developer: "floor-" + string(rune('a'+i)), TotalCostUSD: microToDollars(m), SampleN: 1}
		rows = append(rows, LabeledScore{Label: "solo-" + string(rune('a'+perm[i])), Score: d})
	}
	return rows
}

// TestFolded_ResidualRankedDivergesAtCostFloor pins the measured divergence the
// AggregateFolded doc states: over every labeling of the #722 fixture, the
// folded residual's Ranked differs from the labeled fold's in exactly 360 of 720.
func TestFolded_ResidualRankedDivergesAtCostFloor(t *testing.T) {
	perm := []int{0, 1, 2, 3, 4, 5}
	diff, total := 0, 0
	var walk func(int)
	walk = func(i int) {
		if i == len(perm) {
			rows := floorRows(perm)
			want, _ := AggregateLabeledKAnon(rows, MinKAnonymity, countAll)
			got, _ := AggregateFolded(PreFold(rows, countAll, foldKey("h:")), MinKAnonymity)
			if residualRow(t, got).Ranked != residualRow(t, want).Ranked {
				diff++
			}
			total++
			return
		}
		for j := i; j < len(perm); j++ {
			perm[i], perm[j] = perm[j], perm[i]
			walk(i + 1)
			perm[i], perm[j] = perm[j], perm[i]
		}
	}
	walk(0)
	if total != 720 || diff != 360 {
		t.Fatalf("%d of %d labelings flip the residual's Ranked, want 360 of 720", diff, total)
	}
}

// TestFolded_CreditMemoResidualPaidBound documents the one bound that holds when
// negative paid (credit memos) cancels: the two residual paid sums differ by at
// most 2(n-1)·2⁻⁵³·Σ|paid|, not by a fraction of the (near-zero) sum, so
// spend_leverage has no relative bound.
func TestFolded_CreditMemoResidualPaidBound(t *testing.T) {
	paidRow := func(name, label string, paid float64) LabeledScore {
		d := dev(name, 1, 2)
		d.ActualPaidUSD = paid
		return LabeledScore{Label: label, Score: d}
	}
	rows := []LabeledScore{paidRow("a", "l1", 0.1), paidRow("b", "l3", 0.2), paidRow("c", "l2", -0.3)}
	want, wantSup := AggregateLabeledKAnon(rows, 3, countAll)
	got, gotSup := AggregateFolded(PreFold(rows, countAll, foldKey("h:")), 3)
	if wantSup.Residual || gotSup.Residual {
		t.Fatalf("residual withheld (%+v, %+v); the fixture must publish it", wantSup, gotSup)
	}
	g, w := residualRow(t, got), residualRow(t, want)
	bound := 2 * 2 * math.Pow(2, -53) * 0.6
	if g.ActualPaidUSD == w.ActualPaidUSD || math.Abs(g.ActualPaidUSD-w.ActualPaidUSD) > bound {
		t.Fatalf("paid %v vs %v: want different but within %v", g.ActualPaidUSD, w.ActualPaidUSD, bound)
	}
	t.Logf("paid %v vs %v, spend_leverage %v vs %v", g.ActualPaidUSD, w.ActualPaidUSD, g.SpendLeverage, w.SpendLeverage)
}

func TestPreFold_AppliesKeyAndSortsSets(t *testing.T) {
	key := func(id string) string { return "k" + hex.EncodeToString([]byte(id)) }
	rng := rand.New(rand.NewPCG(913, 3))
	checked := 0
	for it := 0; it < 200; it++ {
		people, labels := genFoldPools(rng)
		rows, census := genFoldWindow(rng, people, labels)
		keyed, raw := map[string]bool{}, map[string]bool{}
		for _, p := range people {
			keyed[key(p)], raw[p] = true, true
		}
		for _, in := range PreFold(rows, census, key) {
			for m, set := range append([][]string{in.People}, in.Carriers[:]...) {
				for i, id := range set {
					if !keyed[id] || raw[id] {
						t.Fatalf("iteration %d label %q set %d: %q is not key(dev)", it, in.Label, m, id)
					}
					if i > 0 && set[i-1] >= id {
						t.Fatalf("iteration %d label %q set %d not sorted and distinct: %v", it, in.Label, m, set)
					}
					checked++
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no key was checked")
	}
}

func TestFolded_PaidOnlyResidualWithheldIdleEmitted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		row      DeveloperScore
		withheld bool
	}{
		{"paid-only", DeveloperScore{Developer: "p", ActualPaidUSD: 1234.56}, true},
		{"idle", DeveloperScore{Developer: "i"}, false},
	} {
		rows := []LabeledScore{{Label: "solo", Score: tc.row}}
		in := PreFold(rows, countAll, foldKey("h:"))
		want, wantSup := AggregateLabeledKAnon(rows, 5, countAll)
		got, gotSup := AggregateFolded(in, 5)
		if gotSup.Residual != tc.withheld || !reflect.DeepEqual(got, want) || gotSup != wantSup {
			t.Errorf("%s: folded %v %+v, labeled %v %+v, want withheld=%v", tc.name, got, gotSup, want, wantSup, tc.withheld)
		}
		wantC, wantCSup := CompareLabeledKAnon(rows, rows, 5, countAll, countAll)
		gotC, gotCSup := CompareFolded(in, in, 5)
		if gotCSup.Residual != tc.withheld || !reflect.DeepEqual(gotC, wantC) || gotCSup != wantCSup {
			t.Errorf("%s compare: folded %v %+v, labeled %v %+v, want withheld=%v", tc.name, gotC, gotCSup, wantC, wantCSup, tc.withheld)
		}
	}
}

func TestFolded_DuplicateLabelPanics(t *testing.T) {
	dup := []LabelInput{{Label: "a"}, {Label: "a"}}
	for name, fold := range map[string]func(){
		"AggregateFolded": func() { AggregateFolded(dup, 3) },
		"CompareFolded":   func() { CompareFolded(nil, dup, 3) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s accepted a duplicate label", name)
				}
			}()
			fold()
		}()
	}
}

// TestMeasureOrdinals pins the index order of LabelInput.Carriers and Has; a
// stored LabelInput depends on it.
func TestMeasureOrdinals(t *testing.T) {
	got := []measure{measurePoints, measureCost, measureRealtime, measureNonRealtime, measurePaid, numMeasures}
	for i, m := range got {
		if int(m) != i {
			t.Fatalf("measure ordinals %v, want 0..5", got)
		}
	}
}

// TestMeasureNames_StoredNames pins each measure's stored name: a sealed person
// row names its measure by it, so a renamed or reassigned name misreads every
// period sealed before the change.
func TestMeasureNames_StoredNames(t *testing.T) {
	want := [numMeasures]string{"points", "cost", "realtime", "non_realtime", "paid"}
	if MeasureNames != want {
		t.Fatalf("MeasureNames = %q, want %q", MeasureNames, want)
	}
	for _, n := range MeasureNames {
		if n == PeopleSetName {
			t.Fatalf("measure name %q collides with PeopleSetName", n)
		}
	}
}

// TestFoldInputs_NeverMarshal: LabelInput and RollupSums are never served, so
// every field marshals to nothing on any response path.
func TestFoldInputs_NeverMarshal(t *testing.T) {
	sums := RollupSums{WeightedPoints: 1, TotalCostUSD: 2, ActualPaidUSD: 3, RealtimeUSD: 4, SampleN: 5, FlaggedOutcomes: 6}
	in := LabelInput{Label: "a", Sums: sums, People: []string{"p"}, Contributes: true}
	in.Carriers[measureCost] = []string{"p"}
	in.Has[measureCost] = true
	for name, v := range map[string]any{"RollupSums": sums, "LabelInput": in, "[]LabelInput": []LabelInput{in}} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(b); got != "{}" && got != "[{}]" {
			t.Errorf("%s marshals to %s, want no fields", name, got)
		}
	}
}
