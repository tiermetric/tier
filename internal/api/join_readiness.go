package api

import (
	"sort"

	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

// These receipts diagnose joins; they never repair identities or attribution.
// Anonymous modes must not construct or publish them, including aggregate counts.
type joinReadinessJSON struct {
	IncompatibleReposTotal         int              `json:"incompatible_repos_total"`
	AmbiguousRepoBlindMatchesTotal int              `json:"ambiguous_repo_blind_matches_total"`
	IssueSpendWithoutOutcomeTotal  int              `json:"issue_spend_without_same_developer_outcome_total"`
	OutcomeOnlyRepos               []repoOutcomeGap `json:"outcome_only_repos"`
	CostOnlyRepos                  []repoCostGap    `json:"cost_only_repos"`
	UnqualifiedCostShare           float64          `json:"unqualified_cost_share"`
	IncompatibleRepos              []issueRepoGap   `json:"incompatible_repos"`
	AmbiguousRepoBlindMatches      []issueRepoGap   `json:"ambiguous_repo_blind_matches"`
	IssueSpendWithoutOutcome       []issueRepoGap   `json:"issue_spend_without_same_developer_outcome"`
}
type repoOutcomeGap struct {
	Repo         string  `json:"repo"`
	OutcomeCount int     `json:"outcome_count"`
	Points       float64 `json:"points"`
}
type repoCostGap struct {
	Repo            string  `json:"repo"`
	CapturedCostUSD float64 `json:"captured_cost_usd"`
}
type issueRepoGap struct {
	Developer      string   `json:"developer"`
	IssueID        string   `json:"issue_id"`
	CostRepo       string   `json:"cost_repo,omitempty"`
	OutcomeRepos   []string `json:"outcome_repos,omitempty"`
	PlausibleRepos []string `json:"plausible_repos,omitempty"`
}

func buildJoinReadiness(w windowScores) *joinReadinessJSON {
	d := &joinReadinessJSON{OutcomeOnlyRepos: []repoOutcomeGap{}, CostOnlyRepos: []repoCostGap{}, IncompatibleRepos: []issueRepoGap{}, AmbiguousRepoBlindMatches: []issueRepoGap{}, IssueSpendWithoutOutcome: []issueRepoGap{}}
	type pair struct{ dev, issue string }
	costs := map[pair][]store.DevIssueCost{}
	outcomes := map[pair][]store.Outcome{}
	repoCosts := map[string]int64{}
	repoCostPresence := map[string]bool{}
	repoOutcomes := map[string]repoOutcomeGap{}
	var total, unqualified int64
	for _, c := range w.issueCosts {
		if c.TotalCostMicro <= 0 {
			continue
		}
		costs[pair{w.canon(c.Developer), c.IssueID}] = append(costs[pair{w.canon(c.Developer), c.IssueID}], c)
		captured := c.CapturedRealtimeMicro + c.CapturedNonRealtimeMicro
		total += captured
		if !repoid.IsReal(c.Repo) {
			unqualified += captured
		}
		repoCosts[c.Repo] += captured
		repoCostPresence[c.Repo] = true
	}
	for i, o := range w.outcomes {
		k := pair{w.canon(o.Developer), o.IssueID}
		outcomes[k] = append(outcomes[k], o)
		gap := repoOutcomes[o.Repo]
		gap.Repo = o.Repo
		gap.OutcomeCount++
		// Use the exact policy-projected points already used by scores.
		gap.Points += w.scoredOutcomes[i].Points()
		repoOutcomes[o.Repo] = gap
	}
	if total > 0 {
		d.UnqualifiedCostShare = float64(unqualified) / float64(total)
	}
	for repo, gap := range repoOutcomes {
		if repoid.IsReal(repo) && !repoCostPresence[repo] {
			d.OutcomeOnlyRepos = append(d.OutcomeOnlyRepos, gap)
		}
	}
	for repo, micro := range repoCosts {
		if _, matched := repoOutcomes[repo]; repoid.IsReal(repo) && !matched && micro > 0 {
			d.CostOnlyRepos = append(d.CostOnlyRepos, repoCostGap{repo, store.MicroToDollars(micro)})
		}
	}
	for k, cs := range costs {
		if store.IsUnattributed(k.issue) {
			continue
		}
		os := outcomes[k]
		realRepos := map[string]bool{}
		blind := false
		for _, c := range cs {
			if repoid.IsReal(c.Repo) {
				realRepos[c.Repo] = true
			} else {
				blind = true
			}
		}
		for _, o := range os {
			if repoid.IsReal(o.Repo) {
				realRepos[o.Repo] = true
			} else {
				blind = true
			}
		}
		if blind && len(realRepos) > 1 && len(os) > 0 {
			d.AmbiguousRepoBlindMatches = append(d.AmbiguousRepoBlindMatches, issueRepoGap{Developer: k.dev, IssueID: k.issue, PlausibleRepos: sortedRepoKeys(realRepos)})
		}

		// Reused issue numbers can be fully joined in several real repositories.
		// Diagnose incompatible pairs only when at least one side has no compatible
		// counterpart; the cross-product of two healthy joins is not a mismatch.
		matchedCostRepos, matchedOutcomeRepos := map[string]bool{}, map[string]bool{}
		for _, c := range cs {
			for _, o := range os {
				if store.RepoMatch(c.Repo, o.Repo) {
					matchedCostRepos[c.Repo] = true
					matchedOutcomeRepos[o.Repo] = true
				}
			}
		}
		// Aliases may collapse several raw cost rows; emit one receipt per repo.
		seen := map[string]bool{}
		for _, c := range cs {
			if seen[c.Repo] {
				continue
			}
			seen[c.Repo] = true
			matches := matchedCostRepos[c.Repo]
			incompatible := map[string]bool{}
			for _, o := range os {
				if !store.RepoMatch(c.Repo, o.Repo) && (!matches || !matchedOutcomeRepos[o.Repo]) {
					incompatible[o.Repo] = true
				}
			}
			if len(incompatible) > 0 {
				d.IncompatibleRepos = append(d.IncompatibleRepos, issueRepoGap{Developer: k.dev, IssueID: k.issue, CostRepo: c.Repo, OutcomeRepos: sortedRepoKeys(incompatible)})
			}
			if !matches {
				d.IssueSpendWithoutOutcome = append(d.IssueSpendWithoutOutcome, issueRepoGap{Developer: k.dev, IssueID: k.issue, CostRepo: c.Repo})
			}
		}
	}
	sort.Slice(d.OutcomeOnlyRepos, func(i, j int) bool { return d.OutcomeOnlyRepos[i].Repo < d.OutcomeOnlyRepos[j].Repo })
	sort.Slice(d.CostOnlyRepos, func(i, j int) bool { return d.CostOnlyRepos[i].Repo < d.CostOnlyRepos[j].Repo })
	for _, rows := range [][]issueRepoGap{d.IncompatibleRepos, d.AmbiguousRepoBlindMatches, d.IssueSpendWithoutOutcome} {
		sort.Slice(rows, func(i, j int) bool {
			a, b := rows[i], rows[j]
			if a.Developer != b.Developer {
				return a.Developer < b.Developer
			}
			if a.IssueID != b.IssueID {
				return a.IssueID < b.IssueID
			}
			return a.CostRepo < b.CostRepo
		})
	}
	d.IncompatibleReposTotal = len(d.IncompatibleRepos)
	d.AmbiguousRepoBlindMatchesTotal = len(d.AmbiguousRepoBlindMatches)
	d.IssueSpendWithoutOutcomeTotal = len(d.IssueSpendWithoutOutcome)
	// Sort before capping so repeated reads publish the same first 50 groups.
	if len(d.IncompatibleRepos) > 50 {
		d.IncompatibleRepos = d.IncompatibleRepos[:50]
	}
	if len(d.AmbiguousRepoBlindMatches) > 50 {
		d.AmbiguousRepoBlindMatches = d.AmbiguousRepoBlindMatches[:50]
	}
	if len(d.IssueSpendWithoutOutcome) > 50 {
		d.IssueSpendWithoutOutcome = d.IssueSpendWithoutOutcome[:50]
	}
	return d
}
func sortedRepoKeys(m map[string]bool) []string {
	s := make([]string, 0, len(m))
	for r := range m {
		s = append(s, r)
	}
	sort.Strings(s)
	return s
}
