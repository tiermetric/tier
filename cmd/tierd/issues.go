package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/logsafe"
)

func runIssues(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("issues", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "central tierd base URL (required; developer mode)")
	tokenFlag := fs.String("api-token", "", "read or admin token; defaults to TIER_API_TOKEN; supports @/path/to/file")
	since := fs.String("since", "", "window lower bound: YYYY-MM-DD, YYYY-MM or YYYY; default 90 days")
	until := fs.String("until", "", "exclusive upper bound; omitted means open-ended")
	repo := fs.String("repo", "", "optional exact owner/repo scope")
	asJSON := fs.Bool("json", false, "print the server report as JSON")
	if err := fs.Parse(args); err != nil {
		return flagParseExit(err)
	}
	fail := func(err error) int { _, _ = fmt.Fprintf(stderr, "issues: %s\n", logsafe.Str(err.Error())); return 1 }
	if fs.NArg() != 0 {
		return fail(fmt.Errorf("unexpected positional arguments"))
	}
	if *server == "" {
		return fail(fmt.Errorf("--server is required"))
	}
	secretEnvFallback(fs, tokenFlag, "api-token", "TIER_API_TOKEN")
	token, err := resolveSecretFlag("--api-token", *tokenFlag)
	if err != nil {
		return fail(err)
	}
	base, err := doctorServerBase(*server, token)
	if err != nil {
		return fail(err)
	}
	query := url.Values{}
	for k, v := range map[string]string{"since": *since, "until": *until, "repo": *repo} {
		if v != "" {
			query.Set(k, v)
		}
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, strings.TrimRight(base, "/")+"/api/v1/issues?"+query.Encode(), nil)
	if err != nil {
		return fail(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := newServerCheckClient().Do(req)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("GET /api/v1/issues returned %d (requires read scope and developer mode)", resp.StatusCode))
	}
	// Bound memory and refuse a truncated response rather than printing a partial
	// reconciliation. Decode the complete object before writing any figures.
	const maxReportBytes = 32 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReportBytes+1))
	if err != nil {
		return fail(err)
	}
	if len(body) > maxReportBytes {
		return fail(fmt.Errorf("report exceeds 32 MiB; narrow the window or repo"))
	}
	var report api.IssueReport
	if err := json.Unmarshal(body, &report); err != nil {
		return fail(err)
	}
	if report.Label != "attributed (lower bound)" || report.Reconciliation == nil || report.RubricVersion < 1 {
		return fail(fmt.Errorf("server did not return a per-issue report; upgrade tierd on the server"))
	}
	if *asJSON {
		if _, err := stdout.Write(body); err != nil {
			return fail(err)
		}
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	end := report.Until
	if end == "" {
		end = "open-ended"
	}
	_, _ = fmt.Fprintf(tw, "Per-issue report [%s, %s), rubric %d (%s)\n", logsafe.Str(report.Since), logsafe.Str(end), report.RubricVersion, logsafe.Str(report.Policy))
	_, _ = fmt.Fprintf(tw, "%s: $%.6f; unattributed %.2f%% ($%.6f); total captured spend $%.6f\n", report.Label, report.AttributedCostUSD, 100*report.UnattributedShare, report.UnattributedCostUSD, report.TotalCostUSD)
	if report.RepoScopeExcluded != nil {
		_, _ = fmt.Fprintln(tw, "Strict repo scope excludes unqualified records; see --json for excluded counts and cost.")
	}
	if dq := report.DataQuality; dq != nil {
		if dq.WindowPredatesCostCapture != nil {
			_, _ = fmt.Fprintf(tw, "Cost capture began %s; window predates cost capture: %t; fully covered --since %s.\n", logsafe.Str(dq.CostCoverageStart), *dq.WindowPredatesCostCapture, logsafe.Str(dq.CostCoverageSafeSince))
		}
		if len(dq.MixedPriceVersions) > 1 {
			_, _ = fmt.Fprintf(tw, "Mixed price versions: %v; captured costs retain their original prices.\n", dq.MixedPriceVersions)
		}
	}
	_, _ = fmt.Fprintln(tw, "Reconciliation (disjoint rows; identifier order)")
	_, _ = fmt.Fprintln(tw, "REPO\tISSUE / REASON\tBASIS\tCOST USD\tPOINTS\tOUTCOMES / ELIGIBLE\tTIER / STATUS\tUNATTRIBUTED SHARE")
	for _, row := range report.Reconciliation {
		tier := row.ScoreStatus
		if row.TIER != nil {
			tier = fmt.Sprintf("%.2f", *row.TIER)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t$%.6f\t%.6f\t%d / %d\t%s\t%.2f%%\n", logsafe.Str(row.Repo), logsafe.Str(row.IssueID), logsafe.Str(row.Label), row.CostUSD, row.WeightedPoints, row.OutcomeN, row.SampleN, logsafe.Str(tier), 100*row.UnattributedShare)
	}
	if err := tw.Flush(); err != nil {
		return fail(err)
	}
	return 0
}
