package scoring

import "testing"

func isPseudoNone(string) bool { return false }

// countAll counts every id, with every id's cost captured.
var countAll = Census{Uncounted: isPseudoNone, UncapturedCost: bothShares(isPseudoNone), Pseudo: isPseudoNone}

// TestAggregateLabeledKAnon_MovedDeveloperCountsOnceInResidual pins #886's
// distinct count: a developer who moved between two sub-k teams inside the
// window reaches the residual as TWO rows, and must fill ONE of the k seats.
// Counting rows would let a single person make a residual look k-sized.
func TestAggregateLabeledKAnon_MovedDeveloperCountsOnceInResidual(t *testing.T) {
	rows := []LabeledScore{
		{Label: "x", Score: DeveloperScore{Developer: "mover", SampleN: 1, WeightedPoints: 1, TotalCostUSD: 5}},
		{Label: "y", Score: DeveloperScore{Developer: "mover", SampleN: 1, WeightedPoints: 1, TotalCostUSD: 5}},
		{Label: "x", Score: DeveloperScore{Developer: "other1", SampleN: 1, WeightedPoints: 1, TotalCostUSD: 5}},
	}
	named, sup := AggregateLabeledKAnon(rows, MinKAnonymity, countAll)
	if len(named) != 0 {
		t.Fatalf("named rows = %+v, want none: two people cannot clear k=3", named)
	}
	if !sup.Residual || sup.Developers != 2 {
		t.Errorf("suppression = %+v, want the residual withheld with 2 developers (mover counted once)", sup)
	}

	// Control: a third DISTINCT person clears the floor, so the count above is the
	// dedupe's doing, not a residual that never clears.
	rows = append(rows, LabeledScore{Label: "y", Score: DeveloperScore{Developer: "other2", SampleN: 1, WeightedPoints: 1, TotalCostUSD: 5}})
	named, sup = AggregateLabeledKAnon(rows, MinKAnonymity, countAll)
	if sup.Any() || len(named) != 1 || named[0].Team != OtherCohort || named[0].TotalCostUSD != 20 {
		t.Errorf("named = %+v, sup = %+v, want one emitted residual carrying all four rows' $20", named, sup)
	}
}

// TestAggregateLabeledKAnon_MovedDeveloperCountsInEachTeam: a developer who
// contributed in two named teams counts toward BOTH floors, and each team carries
// only the share of their figures that happened while they were in it.
func TestAggregateLabeledKAnon_MovedDeveloperCountsInEachTeam(t *testing.T) {
	var rows []LabeledScore
	for _, d := range []string{"a1", "a2"} {
		rows = append(rows, LabeledScore{Label: "alpha", Score: DeveloperScore{Developer: d, TotalCostUSD: 10}})
	}
	for _, d := range []string{"b1", "b2"} {
		rows = append(rows, LabeledScore{Label: "beta", Score: DeveloperScore{Developer: d, TotalCostUSD: 20}})
	}
	rows = append(rows,
		LabeledScore{Label: "alpha", Score: DeveloperScore{Developer: "mover", TotalCostUSD: 1}},
		LabeledScore{Label: "beta", Score: DeveloperScore{Developer: "mover", TotalCostUSD: 2}})
	named, sup := AggregateLabeledKAnon(rows, MinKAnonymity, countAll)
	if sup.Any() || len(named) != 2 {
		t.Fatalf("named = %+v, sup = %+v, want alpha and beta both named", named, sup)
	}
	if named[0].Team != "alpha" || named[0].TotalCostUSD != 21 || named[1].Team != "beta" || named[1].TotalCostUSD != 42 {
		t.Errorf("rows = %+v, want alpha $21 and beta $42 (the mover's $1 and $2 each where they happened)", named)
	}
}

// TestCompareLabeledKAnon_MovedDeveloperCountsOnceInResidual is the compare
// twin of the residual dedupe.
func TestCompareLabeledKAnon_MovedDeveloperCountsOnceInResidual(t *testing.T) {
	side := []LabeledScore{
		{Label: "x", Score: DeveloperScore{Developer: "mover", TotalCostUSD: 5}},
		{Label: "y", Score: DeveloperScore{Developer: "mover", TotalCostUSD: 5}},
		{Label: "x", Score: DeveloperScore{Developer: "other1", TotalCostUSD: 5}},
	}
	_, sup := CompareLabeledKAnon(side, side, MinKAnonymity, countAll, countAll)
	if !sup.Residual || sup.Developers != 2 {
		t.Errorf("suppression = %+v, want the residual withheld with 2 developers", sup)
	}
}

// TestAggregateLabeledKAnon_PaidOnlyRowsFillNoSeat pins #886's invoice rule: a
// row whose only figure is paid spend (an invoice placed at the start of a month
// that began before its seat was assigned) fills no k seat, so two active people
// plus any number of paid-only seats is still a sub-k residual and is withheld.
func TestAggregateLabeledKAnon_PaidOnlyRowsFillNoSeat(t *testing.T) {
	rows := []LabeledScore{
		{Label: "beta", Score: DeveloperScore{Developer: "b1", SampleN: 1, WeightedPoints: 1, TotalCostUSD: 5}},
		{Label: "beta", Score: DeveloperScore{Developer: "b2", SampleN: 1, WeightedPoints: 1, TotalCostUSD: 5}},
	}
	for _, d := range []string{"s1", "s2", "s3", "s4"} {
		rows = append(rows, LabeledScore{Label: "", Score: DeveloperScore{Developer: d, ActualPaidUSD: 100}})
	}
	named, sup := AggregateLabeledKAnon(rows, MinKAnonymity, countAll)
	if len(named) != 0 || !sup.Residual || sup.Developers != 2 {
		t.Errorf("named = %+v, sup = %+v, want the residual withheld with 2 developers (paid-only seats counted as none)", named, sup)
	}
	_, csup := CompareLabeledKAnon(rows, rows, MinKAnonymity, countAll, countAll)
	if !csup.Residual || csup.Developers != 2 {
		t.Errorf("compare suppression = %+v, want the residual withheld with 2 developers", csup)
	}

	// Control: the same seats with activity do count. Each gets both measures, so
	// the per-measure floor (#856) is met too: 6 people with cost and with points.
	for i := 2; i < len(rows); i++ {
		rows[i].Score.TotalCostUSD = 1
		rows[i].Score.SampleN = 1
		rows[i].Score.WeightedPoints = 1
	}
	named, sup = AggregateLabeledKAnon(rows, MinKAnonymity, countAll)
	if sup.Any() || len(named) != 1 || named[0].Team != OtherCohort {
		t.Errorf("named = %+v, sup = %+v, want the residual emitted once the seats have activity", named, sup)
	}
}

// TestAggregateLabeledKAnon_PerMeasureFloor pins #856's per-measure floor (E1)
// in the fold itself: five counted people in a group, of whom only one carries
// cost, is not a k-sized cost population, so the group is not named and the
// residual it falls into is withheld. The same holds for points and for each
// side of a comparison.
func TestAggregateLabeledKAnon_PerMeasureFloor(t *testing.T) {
	build := func(measured func(i int, d *DeveloperScore)) []LabeledScore {
		var rows []LabeledScore
		for i := 0; i < 5; i++ {
			d := DeveloperScore{Developer: string(rune('a' + i)), SampleN: 1, WeightedPoints: 1}
			measured(i, &d)
			rows = append(rows, LabeledScore{Label: "eng", Score: d})
		}
		return rows
	}
	cases := map[string][]LabeledScore{
		"cost carried by one": build(func(i int, d *DeveloperScore) {
			if i == 0 {
				d.TotalCostUSD = 123.45
			}
		}),
		"points carried by one": build(func(i int, d *DeveloperScore) {
			d.TotalCostUSD = 10
			if i != 0 {
				d.SampleN, d.WeightedPoints = 0, 0
			}
		}),
	}
	for name, rows := range cases {
		t.Run(name, func(t *testing.T) {
			named, sup := AggregateLabeledKAnon(rows, 5, countAll)
			if len(named) != 0 || !sup.Residual || sup.Developers != 5 {
				t.Errorf("named = %+v, sup = %+v, want nothing named and the 5-person residual withheld", named, sup)
			}
			_, csup := CompareLabeledKAnon(rows, rows, 5, countAll, countAll)
			if !csup.Residual {
				t.Errorf("compare suppression = %+v, want the residual withheld", csup)
			}
		})
	}

	// Control: every person carrying both measures clears the floor.
	full := build(func(_ int, d *DeveloperScore) { d.TotalCostUSD = 10 })
	named, sup := AggregateLabeledKAnon(full, 5, countAll)
	if sup.Any() || len(named) != 1 || named[0].Team != "eng" {
		t.Errorf("named = %+v, sup = %+v, want eng named once both measures have 5 people", named, sup)
	}
}

// TestCompareLabeledKAnon_EachWindowUsesItsOwnPredicate pins #856's per-window
// evidence: an id counted in window B but not in window A must not fill a seat
// in A. With a single predicate for both windows it would.
func TestCompareLabeledKAnon_EachWindowUsesItsOwnPredicate(t *testing.T) {
	var rows []LabeledScore
	for _, d := range []string{"a", "b", "c"} {
		rows = append(rows, LabeledScore{Label: "eng", Score: DeveloperScore{Developer: d, SampleN: 1, WeightedPoints: 1, TotalCostUSD: 5}})
	}
	notC := func(dev string) bool { return dev == "c" }
	named, sup := CompareLabeledKAnon(rows, rows, MinKAnonymity, Census{Uncounted: notC, UncapturedCost: bothShares(isPseudoNone), Pseudo: isPseudoNone}, countAll)
	if len(named) != 0 || !sup.Residual {
		t.Errorf("named = %+v, sup = %+v, want eng withheld: window A has 2 counted people", named, sup)
	}
	named, sup = CompareLabeledKAnon(rows, rows, MinKAnonymity, countAll, countAll)
	if sup.Any() || len(named) != 1 || named[0].Team != "eng" {
		t.Errorf("control: named = %+v, sup = %+v, want eng named when both windows count 3", named, sup)
	}
}

// TestCompareLabeledKAnon_WindowBUsesItsOwnPredicate is the mirror of the test
// above: the id is uncounted in window B only. A fold that judged B with A's
// census would count it there.
func TestCompareLabeledKAnon_WindowBUsesItsOwnPredicate(t *testing.T) {
	var rows []LabeledScore
	for _, d := range []string{"a", "b", "c"} {
		rows = append(rows, LabeledScore{Label: "eng", Score: DeveloperScore{Developer: d, SampleN: 1, WeightedPoints: 1, TotalCostUSD: 5}})
	}
	notC := Census{Uncounted: func(dev string) bool { return dev == "c" }, UncapturedCost: bothShares(isPseudoNone), Pseudo: isPseudoNone}
	named, sup := CompareLabeledKAnon(rows, rows, MinKAnonymity, countAll, notC)
	if len(named) != 0 || !sup.Residual {
		t.Errorf("named = %+v, sup = %+v, want eng withheld: window B has 2 counted people", named, sup)
	}
}

// TestAggregateLabeledKAnon_CoverageShares pins the coverage part of #856's
// per-measure floor: coverage_pct splits cost into a realtime and a non-realtime
// share, and each non-zero share needs k counted carriers. A share carried by
// pseudo-developers alone identifies nobody and is exempt; one person beside
// them is not.
func TestAggregateLabeledKAnon_CoverageShares(t *testing.T) {
	isPseudo := func(dev string) bool { return dev == "pseudo" }
	census := Census{Uncounted: isPseudo, UncapturedCost: bothShares(isPseudo), Pseudo: isPseudo}
	build := func(extra ...DeveloperScore) []LabeledScore {
		var rows []LabeledScore
		for _, d := range []string{"a", "b", "c", "d", "e"} {
			rows = append(rows, LabeledScore{Label: "eng", Score: DeveloperScore{Developer: d, SampleN: 1, WeightedPoints: 1, TotalCostUSD: 10, CoveragePercent: 100}})
		}
		for _, x := range extra {
			rows = append(rows, LabeledScore{Label: "eng", Score: x})
		}
		return rows
	}
	pseudoDaily := DeveloperScore{Developer: "pseudo", TotalCostUSD: 12.34}
	halfDaily := func(rows []LabeledScore) []LabeledScore {
		rows[0].Score.CoveragePercent = 50
		return rows
	}
	cases := []struct {
		name  string
		rows  []LabeledScore
		named bool
	}{
		{"pseudo-developer alone carries the non-realtime share", build(pseudoDaily), true},
		{"one person carries it beside the pseudo-developer", halfDaily(build(pseudoDaily)), false},
		{"one person carries it", halfDaily(build()), false},
		{"every share has five carriers", build(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			named, sup := AggregateLabeledKAnon(tc.rows, 5, census)
			if got := len(named) == 1 && named[0].Team == "eng" && !sup.Any(); got != tc.named {
				t.Errorf("named = %+v, sup = %+v, want eng named = %v", named, sup, tc.named)
			}
			if !tc.named && (len(named) != 0 || !sup.Residual) {
				t.Errorf("named = %+v, sup = %+v, want nothing named and the residual withheld", named, sup)
			}
		})
	}
}
