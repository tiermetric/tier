package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

const attributedLowerBound = "attributed (lower bound)"

// IssueReport is a live, developer-mode partition of one scores window. Every
// dollar appears in exactly one reconciliation row, including spend-only issues
// and unattributed reasons. Share is a fraction of captured spend, not a claim
// about capture completeness; zero total spend has a zero share.
type IssueReport struct {
	scoring.CorrectionRuleStamp
	Since               string                 `json:"since"`
	Until               string                 `json:"until,omitempty"`
	Repo                string                 `json:"repo,omitempty"`
	RepoScopeExcluded   *repoScopeExcludedJSON `json:"repo_scope_excluded,omitempty"`
	DataQuality         *dataQualityJSON       `json:"data_quality,omitempty"`
	Label               string                 `json:"label"`
	TotalCostUSD        float64                `json:"total_cost_usd"`
	AttributedCostUSD   float64                `json:"attributed_cost_usd"`
	UnattributedCostUSD float64                `json:"unattributed_cost_usd"`
	UnattributedShare   float64                `json:"unattributed_share"`
	Reconciliation      []IssueReportRow       `json:"reconciliation"`
}

// IssueReportRow identifies a stored (repo, issue_id), never a hierarchy or a
// feature. Unattributed rows retain the sentinel issue_id as their reason.
// TIER is null unless repository identity is unambiguous and the summed evidence
// clears the shared engine's floor.
type IssueReportRow struct {
	Kind              string   `json:"kind"` // issue or unattributed
	Repo              string   `json:"repo"`
	IssueID           string   `json:"issue_id"`
	Label             string   `json:"label"`
	UnattributedShare float64  `json:"unattributed_share"`
	CostUSD           float64  `json:"cost_usd"`
	WeightedPoints    float64  `json:"weighted_points"`
	OutcomeN          int      `json:"outcome_n"`
	SampleN           int      `json:"sample_n"`
	FlaggedOutcomes   int      `json:"flagged_outcomes"`
	TIER              *float64 `json:"tier"`
	ScoreStatus       string   `json:"score_status"`
}

func (h *Handler) handleGetIssueScores(w http.ResponseWriter, r *http.Request) {
	// Refuse before any data reads, including on a sealed anonymized install.
	if h.aggregation != scoring.AggregationDeveloper {
		writeError(w, http.StatusForbidden, "per-issue reports are available only in developer mode; team/division privacy design is pending")
		return
	}
	if !rejectUnknownQueryParams(w, r, "since", "until", "before", "repo") {
		return
	}
	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid since: "+err.Error())
		return
	}
	since = sinceUTC(since)
	until, ok := h.parseWindowUpperBound(w, r, since)
	if !ok {
		return
	}
	scope, ok := parseRepoScope(w, r)
	if !ok {
		return
	}
	var win windowScores
	var disclosure scopeDisclosure
	var horizon time.Time
	var hasHorizon bool
	var perSource map[string]time.Time
	err = h.store.ReadSnapshot(r.Context(), func(snap *store.Snapshot) error {
		var e error
		win, e = h.loadWindow(r.Context(), snap, since, until, scope)
		if e != nil {
			return e
		}
		// Match /scores: horizon failures are logged and omit the annotation;
		// a per-source failure retains the global horizon.
		horizon, hasHorizon, e = snap.CostCoverageStart(r.Context(), scope)
		if e != nil {
			h.logger.Error("query cost coverage start", "err", e)
			hasHorizon = false
		} else {
			perSource, e = snap.SourceCoverageStart(r.Context(), scope)
			if e != nil {
				h.logger.Error("query per-source coverage start", "err", e)
				perSource = nil
			}
		}
		disclosure, e = h.buildScopeDisclosure(r.Context(), snap, since, until, scope)
		return e
	})
	if err != nil {
		h.logger.Error("issue scores snapshot", "err", logSafeStr(err.Error()))
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	report := buildIssueReport(win)
	report.Repo = disclosure.Repo
	report.RepoScopeExcluded = disclosure.Excluded
	report.DataQuality = withCostHorizon(dataQualityBlock(h.aggregation, win.zeroTokenOutcomes, win.priceVersions, win.hasSpend()), since, horizon, hasHorizon, perSource)
	writeJSON(w, http.StatusOK, report)
}

func buildIssueReport(win windowScores) IssueReport {
	type key struct{ repo, issue string }
	type group struct {
		cost, realtime int64
		outcomes       []scoring.Outcome
	}
	groups := map[key]*group{}
	// Use the same repo-blind predicate as store.RepoMatch. Identity cannot be
	// recovered by moving cost or outcomes across these disjoint stored keys.
	repoBlindIssues := map[string]bool{}
	get := func(k key) *group {
		g := groups[k]
		if g == nil {
			g = &group{}
			groups[k] = g
		}
		return g
	}
	var total, unattributed int64
	for _, c := range win.issueCosts {
		if !repoid.IsReal(c.Repo) {
			repoBlindIssues[c.IssueID] = true
		}
		g := get(key{c.Repo, c.IssueID})
		g.cost += c.TotalCostMicro
		g.realtime += c.RealtimeCostMicro
		total += c.TotalCostMicro
		if c.IssueID == "" || store.IsUnattributed(c.IssueID) {
			unattributed += c.TotalCostMicro
		}
	}
	// Scored outcomes are index-aligned with stored outcomes. Each projection is
	// already attached to its own outcome: a correction debit changes the original
	// outcome's points, so it stays on that original's issue, even across windows.
	for _, o := range win.scoredOutcomes {
		if !repoid.IsReal(o.Repo) {
			repoBlindIssues[o.IssueID] = true
		}
		g := get(key{o.Repo, o.IssueID})
		g.outcomes = append(g.outcomes, o)
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	isUnattributed := func(k key) bool { return k.issue == "" || store.IsUnattributed(k.issue) }
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if isUnattributed(a) != isUnattributed(b) {
			return !isUnattributed(a)
		}
		if a.repo != b.repo {
			return a.repo < b.repo
		}
		return a.issue < b.issue
	})
	stamp, _ := win.policy.Stamp() // loadWindow validated the policy in the snapshot.
	report := IssueReport{CorrectionRuleStamp: stamp, Since: win.since.Format("2006-01-02"), Label: attributedLowerBound,
		TotalCostUSD: store.MicroToDollars(total), AttributedCostUSD: store.MicroToDollars(total - unattributed), UnattributedCostUSD: store.MicroToDollars(unattributed), Reconciliation: make([]IssueReportRow, 0, len(keys))}
	if !win.until.IsZero() {
		report.Until = win.until.Format("2006-01-02")
	}
	if total > 0 {
		report.UnattributedShare = float64(unattributed) / float64(total)
	}
	for _, k := range keys {
		g := groups[k]
		score := scoring.ComputeDeveloper("", g.outcomes, store.MicroToDollars(g.cost), store.MicroToDollars(g.realtime), 0)
		row := IssueReportRow{Kind: "issue", Repo: k.repo, IssueID: k.issue, Label: attributedLowerBound, UnattributedShare: report.UnattributedShare,
			CostUSD: score.TotalCostUSD, WeightedPoints: score.WeightedPoints, OutcomeN: len(g.outcomes), SampleN: score.SampleN, FlaggedOutcomes: score.FlaggedOutcomes, ScoreStatus: "below evidence floor"}
		if isUnattributed(k) {
			row.Kind = "unattributed"
			row.Label = "unattributed"
		}
		switch {
		case !repoid.IsReal(k.repo) || repoBlindIssues[k.issue]:
			row.ScoreStatus = "repo-blind spend on this issue id"
		case row.Kind == "unattributed":
			row.ScoreStatus = "no score"
		case score.FlaggedOutcomes > 0:
			row.ScoreStatus = "zero-token outcome"
		case score.SampleN == 0:
			row.ScoreStatus = "no score"
		case row.Kind == "issue" && score.Ranked:
			row.TIER = &score.TIER
			row.ScoreStatus = "scored"
		}
		report.Reconciliation = append(report.Reconciliation, row)
	}
	return report
}
