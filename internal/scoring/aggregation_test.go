package scoring

import (
	"math"
	"strings"
	"testing"
)

// dev is a compact DeveloperScore builder for the k-anonymity tests: a developer
// with the given weighted points, cost, and one outcome of sample (so it counts
// as contributing unless all three are zero).
func dev(name string, points, cost float64) DeveloperScore {
	s := DeveloperScore{Developer: name, WeightedPoints: points, TotalCostUSD: cost, SampleN: 1}
	if cost > 0 {
		s.TIER = points / (cost / 1000.0)
	}
	return s
}

// sumTotals returns the summed points/cost/paid across a set of team scores, used
// to prove the k-anonymity aggregation preserves totals (no data dropped).
func sumTotals(teams []TeamScore) (points, cost, paid float64) {
	for _, t := range teams {
		points += t.WeightedPoints
		cost += t.TotalCostUSD
		paid += t.ActualPaidUSD
	}
	return
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestAggregateTeamsKAnon_FloorAndOther pins the core #185 behavior: a team at or
// above the k floor gets a named row; a team below it collapses into "other" with
// its cost and outcomes preserved (never dropped), and no sub-k team name leaks.
func TestAggregateTeamsKAnon_FloorAndOther(t *testing.T) {
	devs := []DeveloperScore{
		dev("a1", 3, 10), dev("a2", 3, 10), dev("a3", 3, 10), // team alpha: 3 contributing
		dev("b1", 5, 20), dev("b2", 5, 20), // team beta: 2 contributing (< k=3)
	}
	teamOf := map[string]string{
		"a1": "alpha", "a2": "alpha", "a3": "alpha",
		"b1": "beta", "b2": "beta",
	}
	got, _ := AggregateTeamsKAnon(devs, teamOf, 3)

	names := map[string]TeamScore{}
	for _, ts := range got {
		names[ts.Team] = ts
	}
	if _, ok := names["alpha"]; !ok {
		t.Errorf("expected named team 'alpha' (3 contributing >= k=3); got teams %v", teamNames(got))
	}
	if _, ok := names["beta"]; ok {
		t.Errorf("team 'beta' (2 < k=3) must NOT get a named row — it should collapse into 'other'; got %v", teamNames(got))
	}
	// #593: the residual here holds only beta's 2 contributors, which is BELOW k=3, so
	// it is WITHHELD rather than emitted. Before #593 it was published — a 2-person
	// cohort's exact figures under a label that reads as anonymized.
	//
	// This test used to assert the opposite ("beta's contributions must be fully
	// preserved in 'other'"), encoding the "totals are preserved exactly" invariant.
	// Steve retired that invariant (option A, 2026-08-03): it is the property that
	// makes the disclosure recoverable by subtraction, so preserving it and preserving
	// k-anonymity are mutually exclusive.
	if _, ok := names[OtherCohort]; ok {
		t.Errorf("a residual holding only 2 contributors (< k=3) must be WITHHELD, not emitted; got %v",
			teamNames(got))
	}
	_, sup := AggregateTeamsKAnon(devs, teamOf, 3)
	if !sup.Any() {
		t.Error("suppression must be REPORTED — a caller that cannot tell suppression from an " +
			"empty window will publish an unfloored total beside the named rows")
	}
	if sup.Developers != 2 {
		t.Errorf("sup.Developers = %d, want 2 (beta's b1+b2)", sup.Developers)
	}
}

// TestAggregateTeamsKAnon_ResidualClearingFloorIsStillEmitted is the control arm for
// the suppression rule. Without it, an implementation that withheld the residual
// UNCONDITIONALLY would pass every suppression assertion in this file while silently
// destroying the normal sub-k fold that #185 depends on.
func TestAggregateTeamsKAnon_ResidualClearingFloorIsStillEmitted(t *testing.T) {
	// Three sub-k teams that POOL into a residual of 4 contributors, >= k=3.
	devs := []DeveloperScore{
		dev("a1", 3, 10), dev("a2", 3, 10), dev("a3", 3, 10), // alpha: named
		dev("b1", 5, 20), dev("b2", 5, 20), // beta:  2 -> other
		dev("c1", 5, 20), dev("c2", 5, 20), // gamma: 2 -> other
	}
	teamOf := map[string]string{
		"a1": "alpha", "a2": "alpha", "a3": "alpha",
		"b1": "beta", "b2": "beta",
		"c1": "gamma", "c2": "gamma",
	}
	got, sup := AggregateTeamsKAnon(devs, teamOf, 3)
	names := teamNameSet(got)
	if !names[OtherCohort] {
		t.Errorf("a residual with 4 contributors (>= k=3) hides its members and MUST be emitted; got %v",
			teamNames(got))
	}
	if sup.Any() {
		t.Errorf("nothing should be suppressed when the residual clears the floor; got %+v", sup)
	}
	if names["beta"] || names["gamma"] {
		t.Errorf("sub-k team names must not leak; got %v", teamNames(got))
	}
}

// TestAggregateTeamsKAnon_IdleResidualIsNotSuppressed pins the third arm of the rule:
// a residual of purely idle seats (#39, zero cost and zero outcomes) identifies nobody
// and carries no figures, so withholding it would suppress the caller's grand total
// for no privacy gain. Emitted, and NOT reported as a suppression.
func TestAggregateTeamsKAnon_IdleResidualIsNotSuppressed(t *testing.T) {
	// NOTE: dev() hardcodes SampleN: 1, so dev(x, 0, 0) still CONTRIBUTES. A genuine
	// #39 idle seat is the zero-valued struct — the same shape
	// TestAggregateTeamsKAnon_ZeroSeatDoesNotCount uses. Getting this wrong is how the
	// first draft of this test failed against correct code.
	devs := []DeveloperScore{
		dev("a1", 3, 10), dev("a2", 3, 10), dev("a3", 3, 10),
		{Developer: "z1", SampleN: 0}, // idle seat: contributes nothing
	}
	teamOf := map[string]string{"a1": "alpha", "a2": "alpha", "a3": "alpha", "z1": "ghost"}
	got, sup := AggregateTeamsKAnon(devs, teamOf, 3)
	if sup.Any() {
		t.Errorf("an all-idle residual carries no figures and must not trigger suppression; got %+v", sup)
	}
	if !teamNameSet(got)[OtherCohort] {
		t.Errorf("the idle residual should still be emitted (it is a harmless zero row); got %v", teamNames(got))
	}
}

// TestAggregateTeamsKAnon_TotalsPreserved proves the reconciliation property IN THE
// UNSUPPRESSED CASE ONLY (#593) — this fixture's residual clears the floor, so it is
// emitted and the rows still sum. The property is NOT global any more: a sub-k residual
// is withheld and the rows deliberately do not reconcile. Do not generalize this test's
// name into a contract.
//
// Original doc follows.
//
// TestAggregateTeamsKAnon_TotalsPreserved proves the honesty invariant: summing
// every returned team row (named + "other") reproduces the same grand total as a
// straight RollupTeam over all developers. Identity is suppressed, data is not.
func TestAggregateTeamsKAnon_TotalsPreserved(t *testing.T) {
	devs := []DeveloperScore{
		dev("a1", 3, 10), dev("a2", 3, 10), dev("a3", 3, 10),
		dev("b1", 5, 20), dev("b2", 5, 20), // sub-k → other
		dev("c1", 1, 4), // teamless, sub-k → other
	}
	devs[5].ActualPaidUSD = 2 // exercise paid preservation too
	teamOf := map[string]string{
		"a1": "alpha", "a2": "alpha", "a3": "alpha",
		"b1": "beta", "b2": "beta",
	}
	got, _ := AggregateTeamsKAnon(devs, teamOf, 3)

	grand := RollupTeam("", devs)
	gotPoints, gotCost, gotPaid := sumTotals(got)
	if !approx(gotPoints, grand.WeightedPoints) || !approx(gotCost, grand.TotalCostUSD) || !approx(gotPaid, grand.ActualPaidUSD) {
		t.Errorf("k-anon aggregation must preserve totals: got (points %v, cost %v, paid %v), want (%v, %v, %v)",
			gotPoints, gotCost, gotPaid, grand.WeightedPoints, grand.TotalCostUSD, grand.ActualPaidUSD)
	}
}

// TestAggregateTeamsKAnon_ExactBoundary pins the exact-k and k-1 edges: a team
// with exactly k contributing developers is named; a team with k-1 collapses.
func TestAggregateTeamsKAnon_ExactBoundary(t *testing.T) {
	// k=4: team "at" has exactly 4, team "under" has 3.
	devs := []DeveloperScore{
		dev("t1", 1, 1), dev("t2", 1, 1), dev("t3", 1, 1), dev("t4", 1, 1),
		dev("u1", 1, 1), dev("u2", 1, 1), dev("u3", 1, 1),
	}
	teamOf := map[string]string{
		"t1": "at", "t2": "at", "t3": "at", "t4": "at",
		"u1": "under", "u2": "under", "u3": "under",
	}
	got, _ := AggregateTeamsKAnon(devs, teamOf, 4)
	names := teamNameSet(got)
	if !names["at"] {
		t.Errorf("team 'at' with exactly k=4 contributing must be named; got %v", teamNames(got))
	}
	if names["under"] {
		t.Errorf("team 'under' with k-1=3 contributing must collapse to 'other'; got %v", teamNames(got))
	}
	// #593: the residual holds "under"'s 3 contributors, below k=4, so it is withheld.
	if names[OtherCohort] {
		t.Errorf("a k-1 residual must be WITHHELD, not emitted as 'other'; got %v", teamNames(got))
	}
}

// TestAggregateTeamsKAnon_ZeroSeatDoesNotCount proves that a pure zero-activity
// seat (#39 allocated-but-idle) does NOT count toward the k floor — otherwise a
// single real contributor could masquerade as a k-sized anonymity set.
func TestAggregateTeamsKAnon_ZeroSeatDoesNotCount(t *testing.T) {
	zero := DeveloperScore{Developer: "idle", SampleN: 0} // contributes nothing
	devs := []DeveloperScore{
		dev("d1", 2, 5), dev("d2", 2, 5), zero, // team delta: 2 contributing + 1 idle
	}
	teamOf := map[string]string{"d1": "delta", "d2": "delta", "idle": "delta"}
	got, _ := AggregateTeamsKAnon(devs, teamOf, 3)
	if teamNameSet(got)["delta"] {
		t.Errorf("team 'delta' has only 2 CONTRIBUTING developers (the idle seat must not pad it to k=3); it must collapse to 'other'; got %v", teamNames(got))
	}
	// Sanity on the underlying predicate the floor relies on.
	if zero.contributes() {
		t.Errorf("a zero-activity seat must not be counted as contributing")
	}
}

// TestAggregateTeamsKAnon_OtherNameCollision proves a real team literally named
// "other" merges into the reserved suppression bucket rather than becoming a
// named team, so the reserved label always reads as the residual aggregate.
func TestAggregateTeamsKAnon_OtherNameCollision(t *testing.T) {
	devs := []DeveloperScore{
		dev("o1", 1, 1), dev("o2", 1, 1), dev("o3", 1, 1), // real team "other", 3 >= k
	}
	teamOf := map[string]string{"o1": "other", "o2": "other", "o3": "other"}
	got, _ := AggregateTeamsKAnon(devs, teamOf, 3)
	if len(got) != 1 || got[0].Team != OtherCohort {
		t.Fatalf("a real team named 'other' must fold into the reserved bucket; got %v", teamNames(got))
	}
	if !approx(got[0].TotalCostUSD, 3) {
		t.Errorf("'other' cost = %v, want 3 (totals preserved through the collision)", got[0].TotalCostUSD)
	}
}

// TestAggregateTeamsKAnon_ClampsBelowMin proves a k below MinKAnonymity is clamped
// up to MinKAnonymity (defence-in-depth; cmd/tierd already rejects such values).
func TestAggregateTeamsKAnon_ClampsBelowMin(t *testing.T) {
	// With k clamped to 3, a 2-developer team collapses even though the caller
	// passed k=1 (which would otherwise have named it).
	devs := []DeveloperScore{dev("p1", 1, 1), dev("p2", 1, 1)}
	teamOf := map[string]string{"p1": "pair", "p2": "pair"}
	got, _ := AggregateTeamsKAnon(devs, teamOf, 1)
	if teamNameSet(got)["pair"] {
		t.Errorf("k=1 must clamp to MinKAnonymity=%d, so the 2-dev team collapses; got %v", MinKAnonymity, teamNames(got))
	}
}

// TestAggregateTeamsKAnon_NoNamesLeak proves the returned team scores never carry
// the underlying developer names out of the anonymity boundary.
func TestAggregateTeamsKAnon_NoNamesLeak(t *testing.T) {
	devs := []DeveloperScore{dev("a1", 1, 1), dev("a2", 1, 1), dev("a3", 1, 1)}
	teamOf := map[string]string{"a1": "alpha", "a2": "alpha", "a3": "alpha"}
	rows, _ := AggregateTeamsKAnon(devs, teamOf, 3)
	for _, ts := range rows {
		if ts.Developers != nil {
			t.Errorf("team %q leaked a Developers slice (%d entries) across the anonymity boundary", ts.Team, len(ts.Developers))
		}
	}
}

// TestFormatReport_TeamMode proves the plain-text report omits the per-developer
// leaderboard entirely in team mode and prints only the aggregate total.
func TestFormatReport_TeamMode(t *testing.T) {
	// SampleN is set because the TEAM TOTAL row is gated on the SUMMED ranking
	// evidence (#606) — the per-developer Ranked flags are not what RollupTeam reads.
	// A fixture that clears the spend floor but carries no outcomes is genuinely
	// unranked, and this test is about mode behaviour, not about the floor.
	scores := []DeveloperScore{
		{Developer: "alice", TIER: 100, WeightedPoints: 3, TotalCostUSD: 10, CoveragePercent: 100, SampleN: 2, Ranked: true},
		{Developer: "bob", TIER: 50, WeightedPoints: 1, TotalCostUSD: 10, CoveragePercent: 100, SampleN: 2, Ranked: true},
	}
	out := FormatReport(scores, "2026-01-01", AggregationTeam)
	if strings.Contains(out, "alice") || strings.Contains(out, "bob") {
		t.Errorf("team-mode report must NOT name any developer; got:\n%s", out)
	}
	if !strings.Contains(out, "TEAM TOTAL") {
		t.Errorf("team-mode report must still print the aggregate TEAM TOTAL; got:\n%s", out)
	}
	if !strings.Contains(out, "team aggregation") {
		t.Errorf("team-mode report should announce the mode; got:\n%s", out)
	}
	// The aggregate must still be correct: points 4, cost 20 → TIER 200.
	if !strings.Contains(out, "200.0") {
		t.Errorf("team-mode TEAM TOTAL TIER should be 200.0 (4 / (20/1000)); got:\n%s", out)
	}
}

func teamNames(ts []TeamScore) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Team
	}
	return out
}

func teamNameSet(ts []TeamScore) map[string]bool {
	out := map[string]bool{}
	for _, t := range ts {
		out[t.Team] = true
	}
	return out
}

// findTeam returns the paired comparison for a team name, if present.
func findTeam(cmps []TeamComparison, team string) (TeamComparison, bool) {
	for _, c := range cmps {
		if c.Team == team {
			return c, true
		}
	}
	return TeamComparison{}, false
}

// TestCompareTeamsKAnon_Intersection pins the #277 two-window intersection: a team
// named in BOTH windows stays named; a team sub-k in EITHER window folds to "other"
// in BOTH — its presence never differs across windows, so no sub-k aggregate can be
// recovered from a delta. Totals are preserved per side.
func TestCompareTeamsKAnon_Intersection(t *testing.T) {
	teamOf := map[string]string{
		"b1": "blue", "b2": "blue", "b3": "blue",
		"r1": "red", "r2": "red", "r3": "red",
	}
	// Window A: blue has 3, red has 3 -> both would clear k=3 alone.
	aScores := []DeveloperScore{
		dev("b1", 1, 4), dev("b2", 1, 4), dev("b3", 1, 4),
		dev("r1", 1, 10), dev("r2", 1, 10), dev("r3", 1, 10),
	}
	// Window B: blue still has 3; red has ONLY r1 -> red sub-k in B.
	bScores := []DeveloperScore{
		dev("b1", 1, 4), dev("b2", 1, 4), dev("b3", 1, 4),
		dev("r1", 1, 10),
	}

	cmps, _ := CompareTeamsKAnon(aScores, bScores, teamOf, 3)

	if _, ok := findTeam(cmps, "red"); ok {
		t.Fatal("red is sub-k in window B and must never be a named comparison row")
	}
	blue, ok := findTeam(cmps, "blue")
	if !ok {
		t.Fatal("blue clears k in both windows and must be named")
	}
	if blue.A.TotalCostUSD != 12 || blue.B.TotalCostUSD != 12 {
		t.Fatalf("blue cost a/b = %v/%v, want 12/12", blue.A.TotalCostUSD, blue.B.TotalCostUSD)
	}
	if blue.A.Developers != nil || blue.B.Developers != nil {
		t.Fatal("named comparison must not carry developer names past the k-anon boundary")
	}
	// #593: the residual is WITHHELD, and this fixture is the clearest illustration of
	// why the floor must be applied PER SIDE rather than to a collapsed count.
	//
	// The residual holds red: 3 contributors in window A (k-safe on its own) but only
	// r1 in window B. This test previously asserted `other.B.TotalCostUSD == 10` — r1's
	// spend, ALONE, published as an anonymized aggregate. A rule that suppressed only
	// when BOTH sides were sub-k would keep emitting exactly that.
	if _, ok := findTeam(cmps, OtherCohort); ok {
		t.Fatal("the residual is k-safe in window A but holds ONE developer in window B; " +
			"publishing it exposes r1's window-B figures under an anonymized label")
	}
	_, sup := CompareTeamsKAnon(aScores, bScores, teamOf, 3)
	if !sup.Any() {
		t.Error("a residual that is sub-k in EITHER window must be suppressed and reported")
	}
	if sup.Developers != 3 {
		t.Errorf("sup.Developers = %d, want 3 (the larger of the two sides' counts)", sup.Developers)
	}

	// ⚠️ The per-side totals assertion that used to close this test is gone. It summed
	// every row and required 42/22 — i.e. it required the suppressed cohort's figures to
	// be present. That is the reconciliation property #593 retired.
}

// TestCompareTeamsKAnon_PresentOneWindowOnly: a team that exists only in window A
// (no developers in window B) is sub-k in B by definition and folds to "other" in
// both — never a named row that would betray it vanished.
func TestCompareTeamsKAnon_PresentOneWindowOnly(t *testing.T) {
	teamOf := map[string]string{"g1": "green", "g2": "green", "g3": "green"}
	aScores := []DeveloperScore{dev("g1", 1, 5), dev("g2", 1, 5), dev("g3", 1, 5)}
	var bScores []DeveloperScore // green absent entirely in window B

	cmps, _ := CompareTeamsKAnon(aScores, bScores, teamOf, 3)
	if _, ok := findTeam(cmps, "green"); ok {
		t.Fatal("green is absent in window B (sub-k) and must not be named")
	}
	other, ok := findTeam(cmps, OtherCohort)
	if !ok {
		t.Fatal("green's window-A data must fold into 'other'")
	}
	if other.A.TotalCostUSD != 15 || other.B.TotalCostUSD != 0 {
		t.Fatalf("other cost a/b = %v/%v, want 15/0", other.A.TotalCostUSD, other.B.TotalCostUSD)
	}
}

// TestCompareTeamsKAnon_EmptyLabelFoldsToOther pins the current-main "" guard the
// reference lacked (#270): the unnamed group (a developer with no team/division
// label) is the residual and must fold to "other" even when it clears k in both
// windows — never a spurious blank-named row.
func TestCompareTeamsKAnon_EmptyLabelFoldsToOther(t *testing.T) {
	// No entries in teamOf -> every developer maps to the empty "" label.
	teamOf := map[string]string{}
	aScores := []DeveloperScore{dev("u1", 1, 5), dev("u2", 1, 5), dev("u3", 1, 5)}
	bScores := []DeveloperScore{dev("u1", 1, 5), dev("u2", 1, 5), dev("u3", 1, 5)}

	cmps, _ := CompareTeamsKAnon(aScores, bScores, teamOf, 3)
	if _, ok := findTeam(cmps, ""); ok {
		t.Fatal("the empty '' label must never earn a named row")
	}
	other, ok := findTeam(cmps, OtherCohort)
	if !ok {
		t.Fatal("the unnamed group must fold into 'other'")
	}
	if other.A.TotalCostUSD != 15 || other.B.TotalCostUSD != 15 {
		t.Fatalf("other cost a/b = %v/%v, want 15/15", other.A.TotalCostUSD, other.B.TotalCostUSD)
	}
	if len(cmps) != 1 {
		t.Fatalf("expected exactly the 'other' residual row, got %d rows", len(cmps))
	}
}
