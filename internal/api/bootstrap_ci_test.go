package api

import (
	"testing"

	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// usdMicro is one dollar in micro-dollars, for readable cost-index literals.
const usdMicro = int64(1_000_000)

func devIssueKey(dev, repo, issue string) store.DevIssue {
	return store.DevIssue{Developer: dev, Repo: repo, IssueID: issue}
}

// TestJointCIInputs pins the #495 caller glue that the coverage control arm does
// NOT reach (its fixtures use one outcome per issue with clean per-outcome cost):
// the per-issue even cost-split (n>1), per-(developer, repo, issue) keying via the
// new Outcome.Repo carry, the fixed non-outcome remainder, and the tolerant-overlap
// proportional scale added after code review. A cross-repo same-issue-id cost
// collision or a broken split would pass every other test; these pin it.
func TestJointCIInputs(t *testing.T) {
	const dev = "alice"
	approx := func(got, want float64) bool { d := got - want; return d < 1e-9 && d > -1e-9 }
	sum := func(xs []float64) float64 {
		var s float64
		for _, x := range xs {
			s += x
		}
		return s
	}

	t.Run("shared issue splits its cost evenly across outcomes (n>1)", func(t *testing.T) {
		// Two outcomes on the SAME (dev, repo, issue): the issue's $10 must split $5/$5,
		// not be counted twice.
		out := []scoring.Outcome{
			{Developer: dev, Repo: "o/r", IssueID: "1", Weight: 5, Quality: 1.0},
			{Developer: dev, Repo: "o/r", IssueID: "1", Weight: 3, Quality: 1.0},
		}
		idx := store.BuildJoinIndex(map[store.DevIssue]int64{devIssueKey(dev, "o/r", "1"): 10 * usdMicro})
		contribs, costs, fixed := jointCIInputs(dev, out, idx, 10.0)
		if !approx(contribs[0], 5) || !approx(contribs[1], 3) {
			t.Errorf("contribs = %v, want [5 3] (weight×quality)", contribs)
		}
		if !approx(costs[0], 5) || !approx(costs[1], 5) {
			t.Errorf("shared-issue cost not split evenly: costs = %v, want [5 5]", costs)
		}
		if !approx(fixed, 0) {
			t.Errorf("fixed = %v, want 0 (all $10 attributed)", fixed)
		}
	})

	t.Run("same issue id in different repos is keyed separately, not pooled", func(t *testing.T) {
		// Repo is part of the key (#231): repo o/a's issue 42 and repo o/b's issue 42
		// are different issues, each with its own cost — pooling them was the bug #231
		// fixed on the cost side, and it must not reappear in the CI denominator.
		out := []scoring.Outcome{
			{Developer: dev, Repo: "o/a", IssueID: "42", Weight: 5, Quality: 1.0},
			{Developer: dev, Repo: "o/b", IssueID: "42", Weight: 5, Quality: 1.0},
		}
		idx := store.BuildJoinIndex(map[store.DevIssue]int64{
			devIssueKey(dev, "o/a", "42"): 6 * usdMicro,
			devIssueKey(dev, "o/b", "42"): 4 * usdMicro,
		})
		_, costs, fixed := jointCIInputs(dev, out, idx, 10.0)
		if !approx(costs[0], 6) || !approx(costs[1], 4) {
			t.Errorf("per-repo keying wrong: costs = %v, want [6 4] (not pooled to 5/5)", costs)
		}
		if !approx(fixed, 0) {
			t.Errorf("fixed = %v, want 0", fixed)
		}
	})

	t.Run("unattributed remainder becomes the fixed denominator term", func(t *testing.T) {
		// The developer's total is $10 but only a $3 issue has an outcome; the other $7
		// (exploratory/unattributed) has no outcome to pair with and must stay fixed.
		out := []scoring.Outcome{{Developer: dev, Repo: "o/r", IssueID: "1", Weight: 3, Quality: 1.0}}
		idx := store.BuildJoinIndex(map[store.DevIssue]int64{devIssueKey(dev, "o/r", "1"): 3 * usdMicro})
		_, costs, fixed := jointCIInputs(dev, out, idx, 10.0)
		if !approx(costs[0], 3) {
			t.Errorf("costs = %v, want [3]", costs)
		}
		if !approx(fixed, 7) {
			t.Errorf("fixed = %v, want 7 ($10 total − $3 attributed)", fixed)
		}
	})

	t.Run("tolerant-overlap inflation is scaled back so attributed == total", func(t *testing.T) {
		// #495 code-review case: issue 42 carries a real-repo bucket ($10) AND a
		// repo-blind (unqualified) bucket ($5); the clean pooled total is $15. A
		// repo-qualified outcome pulls exact[o/a]+exact[unqualified]=15, and a repo-blind
		// outcome pulls anyRepo=15, so raw attributed = $30 > $15. The proportional scale
		// must bring it back to exactly $15 (fixed 0) while preserving the distribution.
		out := []scoring.Outcome{
			{Developer: dev, Repo: "o/a", IssueID: "42", Weight: 5, Quality: 1.0},
			{Developer: dev, Repo: repoid.Unqualified, IssueID: "42", Weight: 5, Quality: 1.0},
		}
		idx := store.BuildJoinIndex(map[store.DevIssue]int64{
			devIssueKey(dev, "o/a", "42"):              10 * usdMicro,
			devIssueKey(dev, repoid.Unqualified, "42"): 5 * usdMicro,
		})
		_, costs, fixed := jointCIInputs(dev, out, idx, 15.0)
		if got := sum(costs); !approx(got, 15) {
			t.Errorf("Σcosts = %v, want 15 (capped at the exact-partition total, not the inflated 30)", got)
		}
		if !approx(fixed, 0) {
			t.Errorf("fixed = %v, want 0", fixed)
		}
		// Raw [15,15] scaled by 15/30 = 0.5 → [7.5, 7.5]: distribution preserved.
		if !approx(costs[0], 7.5) || !approx(costs[1], 7.5) {
			t.Errorf("proportional scale wrong: costs = %v, want [7.5 7.5]", costs)
		}
	})
}
