package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/corrects"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

type issueTestRow struct {
	Kind, Repo string
	IssueID    string `json:"issue_id"`
	Label      string
	Cost       float64  `json:"cost_usd"`
	Points     float64  `json:"weighted_points"`
	Share      float64  `json:"unattributed_share"`
	Outcomes   int      `json:"outcome_n"`
	Samples    int      `json:"sample_n"`
	TIER       *float64 `json:"tier"`
	Status     string   `json:"score_status"`
}
type issueTestReport struct {
	Label        string
	Total        float64 `json:"total_cost_usd"`
	Attributed   float64 `json:"attributed_cost_usd"`
	Unattributed float64 `json:"unattributed_cost_usd"`
	Share        float64 `json:"unattributed_share"`
	Until        string
	DataQuality  *dataQualityJSON `json:"data_quality"`
	Rubric       int              `json:"rubric_version"`
	Rows         []issueTestRow   `json:"reconciliation"`
}

func getIssues(t *testing.T, h *Handler, query string) issueTestReport {
	t.Helper()
	code, body := doRequest(t, h, http.MethodGet, "/api/v1/issues?since=2026"+query, nil)
	if code != 200 {
		t.Fatalf("issues status %d: %s", code, body)
	}
	var report issueTestReport
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatal(err)
	}
	return report
}
func issueCost(t *testing.T, db *store.DB, repo, dev, issue string, dollars float64, at time.Time) {
	t.Helper()
	if err := db.InsertTokenEvent(t.Context(), store.TokenEvent{Developer: dev, Repo: repo, IssueID: issue, CostMicro: store.DollarsToMicro(dollars), InputTok: 2000, Source: "jsonl", Fidelity: "realtime", Timestamp: at}); err != nil {
		t.Fatal(err)
	}
}
func issueOutcome(t *testing.T, db *store.DB, repo, dev, issue string, weight float64, n int, at time.Time) store.Outcome {
	t.Helper()
	o := store.Outcome{Developer: dev, Repo: repo, IssueID: issue, Weight: weight, Quality: 1, PRNumber: n, Timestamp: at, MergedAt: &at, MergeCommitSHA: fmt.Sprintf("%s-%d", repo, n)}
	a, b, c := 10, 0, 1
	o.SetDiffStats(&a, &b, &c)
	if _, err := db.InsertOutcome(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	return o
}
func TestIssuesHonesty(t *testing.T) {
	h, db := newTestHandler(t)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	// Each developer is below the floor. The issue passes only after summing.
	for i, dev := range []string{"alice", "bob", "carol"} {
		issueCost(t, db, "acme/a", dev, "issue-1", float64(i+1), at)
		issueOutcome(t, db, "acme/a", dev, "issue-1", float64(i+1), i+1, at)
	}
	// Spend without an outcome on the SAME issue must stay in its denominator.
	issueCost(t, db, "acme/a", "dave", "issue-1", 4, at)
	issueCost(t, db, "acme/a", "alice", "issue-2", 7, at)
	issueCost(t, db, "acme/b", "alice", "issue-1", 5, at)
	issueOutcome(t, db, "acme/b", "alice", "issue-1", 8, 10, at)
	issueCost(t, db, "acme/a", "alice", store.UnattributedMainBucket, 8, at)
	issueCost(t, db, "acme/a", "alice", "", 2, at)
	r := getIssues(t, h, "")
	t.Run("label_and_share_beside_every_figure", func(t *testing.T) {
		if r.Label != "attributed (lower bound)" || r.Share != 10.0/32 {
			t.Fatal(r)
		}
		for _, row := range r.Rows {
			if row.Share != r.Share || (row.Kind == "issue" && row.Label != r.Label) {
				t.Fatal(row)
			}
		}
	})
	t.Run("disjoint_reconciliation_sums_to_total", func(t *testing.T) {
		sum := 0.0
		seen := map[string]bool{}
		for _, row := range r.Rows {
			key := row.Kind + row.Repo + row.IssueID
			if seen[key] {
				t.Fatal("duplicate", row)
			}
			seen[key] = true
			sum += row.Cost
		}
		if len(r.Rows) != 5 || sum != 32 || r.Total != sum || r.Attributed != 22 || r.Unattributed != 10 {
			t.Fatal(r)
		}
	})
	t.Run("empty_issue_is_unattributed", func(t *testing.T) {
		for _, row := range r.Rows {
			if row.IssueID == "" {
				if row.Kind != "unattributed" || row.Cost != 2 || r.Unattributed != 10 {
					t.Fatal(row, r)
				}
				return
			}
		}
		t.Fatal("missing empty-issue row")
	})
	t.Run("spend_only_issue_no_score", func(t *testing.T) {
		if len(r.Rows) < 2 {
			t.Fatal("missing issue rows")
		}
		row := r.Rows[1]
		if row.IssueID != "issue-2" || row.Cost != 7 || row.Outcomes != 0 || row.Status != "no score" || row.TIER != nil {
			t.Fatal(row)
		}
	})
	t.Run("sum_then_divide_each_outcome_once_and_sum_then_floor", func(t *testing.T) {
		if len(r.Rows) < 1 {
			t.Fatal("missing issue rows")
		}
		row := r.Rows[0]
		if row.Points != 6 || row.Cost != 10 || row.Outcomes != 3 || row.Samples != 3 || row.TIER == nil || *row.TIER != 600 || row.Status != "scored" {
			t.Fatal(row)
		}
	})
	t.Run("never_sorted_by_tier_and_repo_is_part_of_key", func(t *testing.T) {
		if len(r.Rows) < 3 {
			t.Fatal("missing issue rows")
		}
		if r.Rows[0].Repo != "acme/a" || r.Rows[1].IssueID != "issue-2" || r.Rows[2].Repo != "acme/b" || r.Rows[2].Points != 8 || r.Rows[2].Status != "below evidence floor" || r.Rows[2].TIER != nil {
			t.Fatal(r)
		}
	})
	t.Run("open_ended_until_and_exclusive_bound", func(t *testing.T) {
		if r.Until != "" {
			t.Fatal(r.Until)
		}
		bounded := getIssues(t, h, "&until=2026-10-01")
		if bounded.Total != 0 || len(bounded.Rows) != 0 {
			t.Fatal(bounded)
		}
		scoped := getIssues(t, h, "&before=2026-11&repo=acme/b")
		if scoped.Total != 5 || len(scoped.Rows) != 1 {
			t.Fatal(scoped)
		}
	})
}

func TestIssuesRepoBlindWithholdsTIER(t *testing.T) {
	for _, blindRepo := range []string{repoid.Unqualified, ""} {
		for _, source := range []string{"spend", "outcome"} {
			t.Run(fmt.Sprintf("%q/%s", blindRepo, source), func(t *testing.T) {
				h, db := newTestHandler(t)
				at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
				issueCost(t, db, "acme/app", "alice", "1", 5, at)
				for i := 1; i <= 3; i++ {
					issueOutcome(t, db, "acme/app", "alice", "1", 1, i, at)
				}
				control := getIssues(t, h, "")
				if len(control.Rows) != 1 || control.Rows[0].TIER == nil || *control.Rows[0].TIER != 600 || control.Rows[0].Status != "scored" {
					t.Fatal("qualified control", control)
				}
				if source == "spend" {
					issueCost(t, db, blindRepo, "alice", "1", 45, at)
					// Blind spend on another issue must not poison a qualified row.
					issueCost(t, db, "acme/app", "bob", "2", 5, at)
					for i := 4; i <= 6; i++ {
						issueOutcome(t, db, "acme/app", "bob", "2", 1, i, at)
					}
				} else {
					issueOutcome(t, db, blindRepo, "alice", "1", 1, 4, at)
				}
				r := getIssues(t, h, "")
				sum := 0.0
				for _, row := range r.Rows {
					sum += row.Cost
					if row.IssueID == "2" {
						if row.Status != "scored" || row.TIER == nil {
							t.Fatal("unrelated issue", row)
						}
						continue
					}
					if row.TIER != nil || row.Status != "repo-blind spend on this issue id" {
						t.Fatalf("ambiguous row must withhold TIER: %+v", row)
					}
					if row.Repo == "acme/app" && (row.Cost != 5 || row.Points != 3 || row.Outcomes != 3) {
						t.Fatal("reassigned blind data", row)
					}
				}
				wantRows := 2
				if source == "spend" {
					wantRows = 3
				}
				if sum != r.Total || len(r.Rows) != wantRows {
					t.Fatal("partition changed", r)
				}
				if source == "spend" {
					scores := decodeScores(t, h)
					// Alice has 3 points over $50 in /scores, not 600.
					if scores.Developers[0].TIER != 60 {
						t.Fatal(scores.Developers[0])
					}
				}
			})
		}
	}
}

func TestIssuesHistoricalEmptyRepoStaysDisjoint(t *testing.T) {
	// Store inserts normalize an empty repo to unqualified. Exercise historical
	// empty keys directly so that normalization cannot hide a regression here.
	for _, source := range []string{"spend", "outcome"} {
		t.Run(source, func(t *testing.T) {
			win := windowScores{
				issueCosts: []store.DevIssueCost{{Repo: "acme/app", IssueID: "1", TotalCostMicro: 5000000}},
			}
			for range 3 {
				win.scoredOutcomes = append(win.scoredOutcomes, scoring.Outcome{Repo: "acme/app", IssueID: "1", Weight: 1, Quality: 1})
			}
			if source == "spend" {
				win.issueCosts = append(win.issueCosts, store.DevIssueCost{IssueID: "1", TotalCostMicro: 45000000})
			} else {
				win.scoredOutcomes = append(win.scoredOutcomes, scoring.Outcome{IssueID: "1", Weight: 1, Quality: 1})
			}
			r := buildIssueReport(win)
			if len(r.Reconciliation) != 2 || r.Reconciliation[0].Repo != "" || r.Reconciliation[1].Repo != "acme/app" || r.Reconciliation[1].CostUSD != 5 || r.Reconciliation[1].WeightedPoints != 3 {
				t.Fatal(r)
			}
			for _, row := range r.Reconciliation {
				if row.TIER != nil || row.ScoreStatus != "repo-blind spend on this issue id" {
					t.Fatal(row)
				}
			}
		})
	}
}
func TestIssuesEvidenceFloors(t *testing.T) {
	for _, tc := range []struct {
		n    int
		cost float64
		want string
	}{{0, 5, "no score"}, {2, 5, "below evidence floor"}, {3, 4.999999, "below evidence floor"}, {3, 5, "scored"}} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			h, db := newTestHandler(t)
			at := time.Now().UTC()
			issueCost(t, db, "acme/a", "alice", "issue-1", tc.cost, at)
			for i := 0; i < tc.n; i++ {
				issueOutcome(t, db, "acme/a", "alice", "issue-1", 1, i+1, at)
			}
			report := getIssues(t, h, "")
			if len(report.Rows) == 0 {
				t.Fatal("missing issue rows")
			}
			row := report.Rows[0]
			if row.Status != tc.want || (row.TIER != nil) != (tc.want == "scored") {
				t.Fatal(row)
			}
		})
	}
}

func TestIssuesFlaggedAndUnattributedStatuses(t *testing.T) {
	h, db := newTestHandler(t)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	// Spend exists, but not for the outcome's developer: the shared tripwire
	// flags that outcome even though the issue totals clear both evidence floors.
	issueCost(t, db, "acme/app", "bob", "1", 5, at)
	for i := 1; i <= 3; i++ {
		issueOutcome(t, db, "acme/app", "alice", "1", 1, i, at)
	}
	issueOutcome(t, db, "acme/app", "alice", store.UnattributedMainBucket, 1, 4, at)
	r := getIssues(t, h, "")
	if len(r.Rows) != 2 || r.Rows[0].Status != "zero-token outcome" || r.Rows[0].TIER != nil || r.Rows[1].Kind != "unattributed" || r.Rows[1].Status != "no score" || r.Rows[1].TIER != nil {
		t.Fatal(r)
	}
}

func TestIssuesDataQualityMatchesScores(t *testing.T) {
	h, db := newTestHandler(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	seedPricedCostAt(t, db, "alice", "1", 5, 1, at)
	seedPricedCostAt(t, db, "alice", "2", 5, 2, at.Add(24*time.Hour))
	for _, bounds := range []string{"", "&since=2026-10-02", "&until=2026-10-02", "&repo=acme/empty"} {
		t.Run(bounds, func(t *testing.T) {
			// Use exactly one since parameter, shared by both endpoints.
			query := "since=2026" + bounds
			if bounds == "&since=2026-10-02" {
				query = "since=2026-10-02"
			}
			var report issueTestReport
			code, body := doRequest(t, h, "GET", "/api/v1/issues?"+query, nil)
			if code != 200 || json.Unmarshal([]byte(body), &report) != nil {
				t.Fatal(code, body)
			}
			var scores scoresResponse
			code, body = doRequest(t, h, "GET", "/api/v1/scores?"+query, nil)
			if code != 200 || json.Unmarshal([]byte(body), &scores) != nil {
				t.Fatal(code, body)
			}
			got, want := report.DataQuality, scores.DataQuality
			// /scores also carries unrelated coverage signals. Compare only
			// the report's horizon and pricing signals, including omission.
			if got == nil {
				got = &dataQualityJSON{}
			}
			if want == nil {
				want = &dataQualityJSON{}
			}
			if got.CostCoverageStart != want.CostCoverageStart || got.CostCoverageSafeSince != want.CostCoverageSafeSince || !sameInts(got.MixedPriceVersions, want.MixedPriceVersions) || (got.WindowPredatesCostCapture == nil) != (want.WindowPredatesCostCapture == nil) {
				t.Fatalf("issues %+v; scores %+v", got, want)
			}
			if got.WindowPredatesCostCapture != nil && *got.WindowPredatesCostCapture != *want.WindowPredatesCostCapture {
				t.Fatalf("issues %+v; scores %+v", got, want)
			}
			if bounds == "" && (!*got.WindowPredatesCostCapture || !sameInts(got.MixedPriceVersions, []int{1, 2})) {
				t.Fatalf("missing horizon/mixed-price warning: %+v", got)
			}
			if bounds == "&since=2026-10-02" && *got.WindowPredatesCostCapture {
				t.Fatalf("covered window warns: %+v", got)
			}
		})
	}
}
func TestIssuesRefuseModesAndValidateWindow(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, mode := range []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision} {
		h.aggregation = mode
		code, body := doRequest(t, h, "GET", "/api/v1/issues?since=2026", nil)
		if code != 403 {
			t.Fatalf("mode %v: %d %s", mode, code, body)
		}
	}
	h.aggregation = scoring.AggregationDeveloper
	for _, query := range []string{"since=bad", "since=2026&until=bad", "since=2026&until=2025", "since=2026&sort=tier", "since=2026&repo=bad"} {
		code, body := doRequest(t, h, "GET", "/api/v1/issues?"+query, nil)
		if code != 400 {
			t.Fatalf("%s: %d %s", query, code, body)
		}
	}
}
func TestIssuesEvent3DebitOnOriginalIssueAndStoredPolicy(t *testing.T) {
	h, db := newTestHandler(t)
	h.storedScoringPolicy = true
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i, issue := range []string{"original", "repair"} {
		o := issueOutcome(t, db, "acme/app", "alice", issue, []float64{5, 1}[i], i+1, at.Add(time.Duration(i)*time.Hour))
		body := "Corrects: none"
		if i == 1 {
			body = "Corrects: #1"
		}
		if _, err := db.RecordCorrectionDeclaration(t.Context(), o, corrects.Parse(body), "bounded evidence", "test"); err != nil {
			t.Fatal(err)
		}
	}
	legacy := getIssues(t, h, "")
	if legacy.Rubric != 1 || legacy.Rows[0].Points != 5 || legacy.Rows[1].Points != 1 {
		t.Fatal(legacy)
	}
	if err := db.ConfigureCorrectionScoring(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	on := getIssues(t, h, "")
	if on.Rubric != 2 || on.Rows[0].IssueID != "original" || on.Rows[0].Points != 3.25 || on.Rows[1].Points != 1 || on.Rows[1].Samples != 0 {
		t.Fatal(on)
	}
	// /scores and the partition must consume exactly the same projected points.
	code, body := doRequest(t, h, "GET", "/api/v1/scores?since=2026", nil)
	if code != 200 {
		t.Fatal(code, body)
	}
	var scores struct {
		Developers []struct {
			Points float64 `json:"weighted_points"`
		}
	}
	if err := json.Unmarshal([]byte(body), &scores); err != nil {
		t.Fatal(err)
	}
	if scores.Developers[0].Points != on.Rows[0].Points+on.Rows[1].Points {
		t.Fatal(body, on)
	}
}

func TestIssuesReadScopeAndReadOnlyRegistration(t *testing.T) {
	h, _ := newTestHandlerWithScopes(t, scopeTestWrite, scopeTestRead)
	for _, tc := range []struct {
		token string
		want  int
	}{{"", 401}, {"wrong", 401}, {scopeTestRead, 200}, {scopeTestWrite, 200}} {
		code, body := doRequestWithHeader(t, h, "GET", "/api/v1/issues?since=2026", nil, bearerHeader(tc.token))
		if code != tc.want {
			t.Fatalf("status %d, want %d: %s", code, tc.want, body)
		}
	}
	if code := serveVia(h, h.RegisterReadOnly, "GET", "/api/v1/issues?since=2026"); code != 401 {
		t.Fatal(code)
	}
}

func TestIssuesOrderIsNeitherAscendingNorDescendingTIER(t *testing.T) {
	h, db := newTestHandler(t)
	at := time.Now().UTC()
	for i, weight := range []float64{3, 1, 2} {
		issue := fmt.Sprintf("issue-%d", i)
		issueCost(t, db, "acme/app", "alice", issue, 10, at)
		for j := 1; j <= 3; j++ {
			issueOutcome(t, db, "acme/app", "alice", issue, weight, 10*i+j, at)
		}
	}
	r := getIssues(t, h, "")
	if len(r.Rows) != 3 {
		t.Fatal(r)
	}
	for i, want := range []float64{900, 300, 600} {
		row := r.Rows[i]
		if row.IssueID != fmt.Sprintf("issue-%d", i) || row.TIER == nil || *row.TIER != want {
			t.Fatal(r)
		}
	}
}
