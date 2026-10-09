package scoring

// Test-only adapters (#886). The production folds take rows that each carry the
// label valid when their events happened (AggregateLabeledKAnon,
// CompareLabeledKAnon). Most fold tests exercise the k-floor, residual and
// determinism rules with a developer who holds one label for the whole window;
// these adapters label every row from a static map so those tests read as they
// did before dated membership. They live in a _test.go file on purpose: no
// production caller may label a window from a single current map.

func labelFromMap(devScores []DeveloperScore, teamOf map[string]string) []LabeledScore {
	rows := make([]LabeledScore, len(devScores))
	for i, d := range devScores {
		rows[i] = LabeledScore{Label: teamOf[d.Developer], Score: d}
	}
	return rows
}

func AggregateTeamsKAnon(devScores []DeveloperScore, teamOf map[string]string, k int, isPseudo func(developer string) bool) ([]TeamScore, KAnonSuppression) {
	return AggregateLabeledKAnon(labelFromMap(devScores, teamOf), k, Census{Uncounted: isPseudo, UncapturedCost: bothShares(isPseudo), Pseudo: isPseudo})
}

func CompareTeamsKAnon(aScores, bScores []DeveloperScore, teamOf map[string]string, k int, isPseudo func(developer string) bool) ([]TeamComparison, KAnonSuppression) {
	c := Census{Uncounted: isPseudo, UncapturedCost: bothShares(isPseudo), Pseudo: isPseudo}
	return CompareLabeledKAnon(labelFromMap(aScores, teamOf), labelFromMap(bScores, teamOf), k, c, c)
}

// bothShares is an UncapturedCost that judges both cost shares by one predicate.
func bothShares(uncaptured func(developer string) bool) func(DeveloperScore, bool) bool {
	return func(row DeveloperScore, _ bool) bool { return uncaptured(row.Developer) }
}
