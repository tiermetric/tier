package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tiermetric/tier/internal/diagnostics"
)

type doctorJoinReadiness struct {
	IncompatibleReposTotal         int `json:"incompatible_repos_total"`
	AmbiguousRepoBlindMatchesTotal int `json:"ambiguous_repo_blind_matches_total"`
	IssueSpendWithoutOutcomeTotal  int `json:"issue_spend_without_same_developer_outcome_total"`
	OutcomeOnlyRepos               []struct {
		Repo         string  `json:"repo"`
		OutcomeCount int     `json:"outcome_count"`
		Points       float64 `json:"points"`
	} `json:"outcome_only_repos"`
	CostOnlyRepos []struct {
		Repo            string  `json:"repo"`
		CapturedCostUSD float64 `json:"captured_cost_usd"`
	} `json:"cost_only_repos"`
	UnqualifiedCostShare      float64 `json:"unqualified_cost_share"`
	IncompatibleRepos         []any   `json:"incompatible_repos"`
	AmbiguousRepoBlindMatches []any   `json:"ambiguous_repo_blind_matches"`
	IssueSpendWithoutOutcome  []any   `json:"issue_spend_without_same_developer_outcome"`
}

func checkJoinReadiness(s scoresResponse) []checkResult {
	d := s.DataQuality.JoinReadiness
	if d == nil {
		return nil
	} // Older servers and anonymous reports supply no receipt.
	status := statusOK
	if len(d.OutcomeOnlyRepos)+len(d.IncompatibleRepos)+len(d.AmbiguousRepoBlindMatches) > 0 {
		status = statusWarn
	}
	names := []string{}
	var count int
	var points float64
	for _, r := range d.OutcomeOnlyRepos {
		names = append(names, r.Repo)
		count += r.OutcomeCount
		points += r.Points
	}
	for _, r := range d.CostOnlyRepos {
		names = append(names, r.Repo)
	}
	detail := fmt.Sprintf("%d outcome-only repositories, %d incompatible issue/repository joins, %d ambiguous repo-blind joins", len(d.OutcomeOnlyRepos), max(d.IncompatibleReposTotal, len(d.IncompatibleRepos)), max(d.AmbiguousRepoBlindMatchesTotal, len(d.AmbiguousRepoBlindMatches)))
	detail += fmt.Sprintf("; repository gaps contain %d outcomes and %.2f points", count, points)
	if len(names) > 0 {
		detail += " (" + joinSafe(names) + ")"
	}
	return []checkResult{{name: "join readiness", status: status, detail: detail, hint: "check repository and issue evidence, capture coverage and window overlap. An alias joins identities; it does not verify the issue or repository match. Collaborators' captured spend stays under their own identities, even when someone else merges the work"},
		{name: "join coverage", status: statusInfo, detail: fmt.Sprintf("%d cost-only repositories ($%.2f captured cost); %d issue-cost groups without a same-developer outcome; %s repository-unqualified (share of captured dollars)", len(d.CostOnlyRepos), costOnlyUSD(d), max(d.IssueSpendWithoutOutcomeTotal, len(d.IssueSpendWithoutOutcome)), pctStr(d.UnqualifiedCostShare))}}
}

func checkCapturedDollars(s scoresResponse) []checkResult {
	d := s.DataQuality.CapturedDollarDiagnostics
	if d == nil || d.CapturedCostUSD <= 0 {
		return nil
	}
	window := safeDateStr(doctorDay(d.Since))
	if d.Until != "" {
		window += " to " + safeDateStr(doctorDay(d.Until))
	}
	r := checkResult{name: "captured-dollar diagnostics", status: statusOK, detail: fmt.Sprintf("share of captured dollars: %s in unattributed:main with stored main-checkout cwd and linked worktrees present now (%d captured events; window %s; scope %s)", pctStr(d.MainCheckoutWorktreeShare), d.SampleSize, window, joinSafe([]string{d.Scope}))}
	r.detail += fmt.Sprintf("; stored cwd evidence covers %s of captured dollars (%d events); missing paths or metadata can prevent detection", pctStr(d.StoredCWDCostShare), d.StoredCWDSampleSize)
	if d.MainCheckoutWorktreeShare >= 0.5 {
		r.status = statusWarn
		r.hint = diagnostics.WorktreeExplanation + " This is a likely cause, not proof of the historical working directory or missing real expenditure. See " + diagnostics.WorktreeExplanationURL
	}
	return []checkResult{r}
}

// The server reads checkpoint and git metadata; doctor only formats its receipt.
func readCapturedDollars(ctx context.Context, client *http.Client, server, token string) []checkResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(server, "/")+"/api/v1/diagnostics/captured-dollars", nil)
	if err != nil {
		return []checkResult{{name: "captured-dollar diagnostics", status: statusWarn, detail: "could not construct diagnostics request"}}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return []checkResult{{name: "captured-dollar diagnostics", status: statusWarn, detail: "diagnostics unavailable"}}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	var d diagnostics.CapturedDollars
	if err != nil || len(body) > 1<<20 || resp.StatusCode != http.StatusOK || json.Unmarshal(body, &d) != nil {
		return []checkResult{{name: "captured-dollar diagnostics", status: statusWarn, detail: "diagnostics unavailable or invalid response"}}
	}
	var s scoresResponse
	s.DataQuality.CapturedDollarDiagnostics = &d
	return checkCapturedDollars(s)
}

func costOnlyUSD(d *doctorJoinReadiness) float64 {
	var total float64
	for _, r := range d.CostOnlyRepos {
		total += r.CapturedCostUSD
	}
	return total
}
