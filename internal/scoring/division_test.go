package scoring

import "testing"

// TestAggregationMode_StringAndAnonymized pins the level-parameter contract that
// makes the aggregation level a clean enum (#270): String() names each level for
// the API discriminator and reports, and Anonymized() is the single predicate
// every k-anon/403 suppression guard keys on. Division joins team as an
// anonymized grouped level; developer is never anonymized.
func TestAggregationMode_StringAndAnonymized(t *testing.T) {
	cases := []struct {
		mode       AggregationMode
		str        string
		anonymized bool
	}{
		{AggregationDeveloper, "developer", false},
		{AggregationTeam, "team", true},
		{AggregationDivision, "division", true},
	}
	for _, c := range cases {
		if got := c.mode.String(); got != c.str {
			t.Errorf("%v.String() = %q, want %q", c.mode, got, c.str)
		}
		if got := c.mode.Anonymized(); got != c.anonymized {
			t.Errorf("%q.Anonymized() = %v, want %v", c.str, got, c.anonymized)
		}
	}
}

// TestAggregateKAnon_LevelAgnostic_DivisionMap proves AggregateTeamsKAnon is
// reused UNCHANGED for the division level (#270): given a developer->division
// label map it folds exactly as it does for teams -- a division with >= k
// CONTRIBUTING developers is named, a sub-k division collapses into "other", and
// the division rollup preserves the grand total. The fold never inspects what the
// label MEANS, so adding org/department later is the same call with another map.
func TestAggregateKAnon_LevelAgnostic_DivisionMap(t *testing.T) {
	// "engineering": 3 contributors (>= k=3, named); "research": 2 (< k, -> other).
	devs := []DeveloperScore{
		dev("e1", 30, 10), dev("e2", 30, 10), dev("e3", 30, 10),
		dev("r1", 20, 7), dev("r2", 20, 7),
	}
	divisionOf := map[string]string{
		"e1": "engineering", "e2": "engineering", "e3": "engineering",
		"r1": "research", "r2": "research",
	}

	got := AggregateTeamsKAnon(devs, divisionOf, 3)

	names := map[string]TeamScore{}
	for _, ts := range got {
		names[ts.Team] = ts
	}
	if _, ok := names["research"]; ok {
		t.Errorf("sub-k division 'research' must NOT be named; got rows %v", teamNames(got))
	}
	eng, ok := names["engineering"]
	if !ok {
		t.Fatalf("expected a named 'engineering' division; got %v", teamNames(got))
	}
	if eng.WeightedPoints != 90 || eng.TotalCostUSD != 30 {
		t.Errorf("engineering rollup = points %v cost %v, want 90 / 30", eng.WeightedPoints, eng.TotalCostUSD)
	}
	other, ok := names["other"]
	if !ok {
		t.Fatalf("sub-k 'research' must fold into 'other'; got %v", teamNames(got))
	}
	if other.WeightedPoints != 40 || other.TotalCostUSD != 14 {
		t.Errorf("other (research preserved) = points %v cost %v, want 40 / 14", other.WeightedPoints, other.TotalCostUSD)
	}
	// Named rows must never carry developer identities across the boundary.
	if eng.Developers != nil || other.Developers != nil {
		t.Errorf("division rows must clear Developers; eng=%v other=%v", eng.Developers, other.Developers)
	}

	// Totals preserved: sum of visible division rows == straight rollup over all.
	full := RollupTeam("", devs)
	var sumPoints, sumCost float64
	for _, ts := range got {
		sumPoints += ts.WeightedPoints
		sumCost += ts.TotalCostUSD
	}
	if sumPoints != full.WeightedPoints || sumCost != full.TotalCostUSD {
		t.Errorf("division rollup lost data: sum(points %v, cost %v) != total(points %v, cost %v)",
			sumPoints, sumCost, full.WeightedPoints, full.TotalCostUSD)
	}
}

// TestAggregateKAnon_EmptyDivisionFoldsToOther documents the empty-division
// contract (#270): a developer whose division is "" (unset/NULL in the schema,
// COALESCEd to "" on read) belongs to the unnamed group and folds into "other"
// rather than forming a spurious blank-named division row.
func TestAggregateKAnon_EmptyDivisionFoldsToOther(t *testing.T) {
	devs := []DeveloperScore{
		dev("e1", 30, 10), dev("e2", 30, 10), dev("e3", 30, 10),
		dev("x1", 5, 2), // no division mapping at all
		dev("x2", 5, 2), // explicit empty division
	}
	divisionOf := map[string]string{
		"e1": "engineering", "e2": "engineering", "e3": "engineering",
		"x2": "",
	}
	got := AggregateTeamsKAnon(devs, divisionOf, 3)
	for _, ts := range got {
		if ts.Team == "" {
			t.Errorf("empty division must not produce a blank-named row; got %v", teamNames(got))
		}
	}
	var other *TeamScore
	for i := range got {
		if got[i].Team == "other" {
			other = &got[i]
		}
	}
	if other == nil {
		t.Fatalf("empty-division developers must land in 'other'; got %v", teamNames(got))
	}
	// dev(name, points, cost): x1+x2 = points 10, cost 4.
	if other.WeightedPoints != 10 || other.TotalCostUSD != 4 {
		t.Errorf("other (x1+x2 preserved) = points %v cost %v, want 10 / 4", other.WeightedPoints, other.TotalCostUSD)
	}
}

// TestAggregateKAnon_EmptyLabelNeverNamedEvenAboveK is the compliance-contract
// guard (#270): the unnamed group ("") folds into "other" EVEN WHEN it has >= k
// contributors, so it never emits a spurious blank-named (`team`-less) row. This
// is the realistic division-mode state where teams are populated but every
// division is NULL/empty -- all developers land in "" and must roll into "other",
// not one unlabeled aggregate masquerading as a division.
func TestAggregateKAnon_EmptyLabelNeverNamedEvenAboveK(t *testing.T) {
	// FIVE contributors, all with empty division (well above k=3).
	devs := []DeveloperScore{
		dev("a", 10, 3), dev("b", 10, 3), dev("c", 10, 3),
		dev("d", 10, 3), dev("e", 10, 3),
	}
	divisionOf := map[string]string{} // nobody mapped -> all in the "" group

	got := AggregateTeamsKAnon(devs, divisionOf, 3)

	if len(got) != 1 || got[0].Team != "other" {
		t.Fatalf("a >= k empty-label group must fold into a single 'other' row, never a blank-named one; got %v", teamNames(got))
	}
	if got[0].WeightedPoints != 50 || got[0].TotalCostUSD != 15 {
		t.Errorf("other = points %v cost %v, want 50 / 15 (all five preserved)", got[0].WeightedPoints, got[0].TotalCostUSD)
	}
}
