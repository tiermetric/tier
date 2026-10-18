package store

import (
	"context"
	"strings"
	"time"
)

// RepairSpend counts token_events.id once across all supporting repairs. A PR
// reference supports that PR; issue-only evidence remains a shared pool, never
// silently split across PRs. Recorded dollars are not the repair's causal cost.
type RepairSpend struct {
	Status                 string `json:"status"`
	TokenEvents            int64  `json:"token_events"`
	CostMicro              int64  `json:"cost_micro"`
	SupportedPRCostMicro   int64  `json:"supported_pr_cost_micro"`
	PooledIssueCostMicro   int64  `json:"pooled_issue_cost_micro"`
	SupportedPRTokenEvents int64  `json:"supported_pr_token_events"`
	PooledIssueTokenEvents int64  `json:"pooled_issue_token_events"`
	Attribution            string `json:"attribution"`
	ClaimLimit             string `json:"claim_limit"`
}

func (r reader) RepairSpendWindow(ctx context.Context, since, until time.Time, scope RepoScope) (RepairSpend, error) {
	s := RepairSpend{Status: "recorded", Attribution: "unavailable", ClaimLimit: "Supporting captured spend only; issue attribution is pooled, incomplete capture is retained, and neither cost causality nor complete repair history is established."}
	where, args := tsWindow(since, until)
	where = strings.ReplaceAll(where, "ts", "te.ts")
	query := `SELECT te.id,te.cost_micro,te.pr_number IS NOT NULL FROM token_events te
 WHERE ` + where + ` AND (?='' OR te.repo=?) AND EXISTS (
 SELECT 1 FROM corrective_relations r LEFT JOIN outcomes o ON o.id=r.child_outcome_id
 WHERE r.state NOT IN ('removed','unrelated','withdrawn') AND r.child_repo=te.repo
 AND COALESCE((SELECT canonical FROM developer_alias WHERE alias=r.child_developer),r.child_developer)=COALESCE((SELECT canonical FROM developer_alias WHERE alias=te.developer),te.developer)
 AND ((te.pr_number IS NOT NULL AND te.pr_number=r.child_pr_number)
 OR (te.pr_number IS NULL AND o.issue_id!='' AND te.issue_id=o.issue_id))) ORDER BY te.id`
	repo := ""
	if !scope.IsFleetWide() {
		repo = string(scope)
	}
	rows, err := r.queryEffectiveWindow(ctx, since, until, scope, query, append(args, repo, repo)...)
	if err != nil {
		return s, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, cost int64
		var supported bool
		if err = rows.Scan(&id, &cost, &supported); err != nil {
			return s, err
		}
		s.TokenEvents++
		s.CostMicro += cost
		if supported {
			s.SupportedPRCostMicro += cost
			s.SupportedPRTokenEvents++
		} else {
			s.PooledIssueCostMicro += cost
			s.PooledIssueTokenEvents++
		}
	}
	if s.TokenEvents > 0 {
		s.Attribution = "supported_pr"
		if s.PooledIssueTokenEvents > 0 {
			s.Attribution = "pooled"
			if s.SupportedPRTokenEvents > 0 {
				s.Attribution = "partial"
			}
		}
	}
	return s, rows.Err()
}
