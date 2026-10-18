package api

import (
	"maps"
	"math/big"
	"slices"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

const intervalAttributionNote = "excludes uncertainty from unattributed spend"
const intervalWithheldNote = "withheld: row attributed-cost share below 50%"

// intervalNote uses the bootstrap's issue allocation, scaling and eligibility.
// Exact micro-dollar ratios keep shared-issue rounding from moving the 0.5 floor.
func intervalNote(s scoring.DeveloperScore, outcomes []scoring.Outcome, index store.JoinIndex) string {
	if !s.Ranked || s.TotalCostUSD <= 0 {
		return "withheld: below evidence floor"
	}
	type counts struct{ all, eligible int64 }
	perIssue := map[store.DevIssue]counts{}
	for _, o := range outcomes {
		key := store.DevIssue{Developer: s.Developer, Repo: o.Repo, IssueID: o.IssueID}
		n := perIssue[key]
		n.all++
		if o.RankingEligible() {
			n.eligible++
		}
		perIssue[key] = n
	}
	attributed, resampled := new(big.Rat), new(big.Rat)
	for key, n := range perIssue {
		cost := new(big.Rat).SetInt64(index.Sum(s.Developer, key.Repo, key.IssueID))
		attributed.Add(attributed, cost)
		share := new(big.Rat).SetFrac64(n.eligible, n.all)
		resampled.Add(resampled, new(big.Rat).Mul(cost, share))
	}
	denominator := new(big.Rat).SetInt64(store.DollarsToMicro(s.TotalCostUSD))
	// The bootstrap scales all allocated cost proportionally when tolerant joins exceed the row total.
	if attributed.Cmp(denominator) > 0 {
		denominator = attributed
	}
	if new(big.Rat).Mul(resampled, big.NewRat(2, 1)).Cmp(denominator) < 0 {
		return intervalWithheldNote
	}
	return intervalAttributionNote
}

type issueMultiplicityJSON struct {
	Outcomes int `json:"outcomes"`
	Issues   int `json:"issues"`
}

type weightBucketJSON struct {
	Weight   float64 `json:"weight"`
	Outcomes int     `json:"outcomes"`
	Points   float64 `json:"points"`
}

type partitionDiagnosticsJSON struct {
	OutcomesPerIssue    []issueMultiplicityJSON `json:"outcomes_per_issue"`
	WeightBuckets       []weightBucketJSON      `json:"weight_buckets"`
	OutcomeN            int                     `json:"outcome_n"`
	CappedPRCount       int                     `json:"capped_pr_count"`
	CappedPRCountShare  *float64                `json:"capped_pr_count_share"`
	CappedPRPoints      float64                 `json:"capped_pr_points"`
	CappedPRPointsShare *float64                `json:"capped_pr_points_share"`
	PointsPerPR         *float64                `json:"points_per_pr"`
	PointsPer1000USD    float64                 `json:"points_per_1000_usd"`
	Notice              string                  `json:"notice"`
}

func partitionDiagnostics(win windowScores) partitionDiagnosticsJSON {
	d := partitionDiagnosticsJSON{
		OutcomesPerIssue: []issueMultiplicityJSON{}, WeightBuckets: []weightBucketJSON{},
		Notice: "Points depend on PR partitioning: splitting the same work can change points on the same issue or across issues. Diagnostics apply no automatic penalties. Outcomes include PR and push outcomes; points per PR excludes direct push outcomes.",
	}
	issues := map[struct{ repo, issue string }]int{}
	buckets := map[float64]weightBucketJSON{}
	var points, prPoints, cost float64
	var prs int
	for i, o := range win.outcomes {
		p := win.scoredOutcomes[i].Points()
		points += p
		d.OutcomeN++
		issues[struct{ repo, issue string }{o.Repo, o.IssueID}]++
		b := buckets[o.Weight]
		b.Weight = o.Weight
		b.Outcomes++
		b.Points += p
		buckets[o.Weight] = b
		if o.WeightSource != store.WeightSourcePush {
			prs++
			prPoints += p
			if o.Weight == 8 {
				d.CappedPRCount++
				d.CappedPRPoints += p
			}
		}
	}
	counts := map[int]int{}
	for _, n := range issues {
		counts[n]++
	}
	for _, n := range slices.Sorted(maps.Keys(counts)) {
		d.OutcomesPerIssue = append(d.OutcomesPerIssue, issueMultiplicityJSON{n, counts[n]})
	}
	for _, weight := range slices.Sorted(maps.Keys(buckets)) {
		d.WeightBuckets = append(d.WeightBuckets, buckets[weight])
	}
	if prs > 0 {
		share, mean := float64(d.CappedPRCount)/float64(prs), prPoints/float64(prs)
		d.CappedPRCountShare, d.PointsPerPR = &share, &mean
	}
	if prPoints > 0 {
		share := d.CappedPRPoints / prPoints
		d.CappedPRPointsShare = &share
	}
	for _, s := range win.devScores {
		cost += s.TotalCostUSD
	}
	if cost > 0 {
		d.PointsPer1000USD = points / (cost / 1000)
	}
	return d
}
