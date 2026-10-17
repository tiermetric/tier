package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

func TestRepoJoin_OutcomeOnlyRepoListed(t *testing.T) {
	h, db := newTestHandler(t)
	seedRepoOutcome(t, db, repoAlpha, "alice", "issue-42", 3)
	resp, _ := getScoresRaw(t, h, "?since="+scoresSince())
	block, ok := dataQualityOf(t, resp)["join_readiness"].(map[string]any)
	if !ok {
		t.Fatal("missing join_readiness")
	}
	rows := block["outcome_only_repos"].([]any)
	if len(rows) != 1 {
		t.Fatalf("repos: %v", rows)
	}
	row := rows[0].(map[string]any)
	if row["repo"] != repoAlpha || row["outcome_count"] != float64(1) || row["points"] != float64(3) {
		t.Fatalf("row: %v", row)
	}
}

func TestRepoJoin_MismatchAndScope(t *testing.T) {
	h, db := newTestHandler(t)
	seedRepoCost(t, db, repoAlpha, "alice", "issue-42", 10)
	seedRepoOutcome(t, db, repoBeta, "alice", "issue-42", 3)
	resp, _ := getScoresRaw(t, h, "?since="+scoresSince())

	typed := getScores(t, h, "/api/v1/scores?since="+scoresSince())
	got, err := json.MarshalIndent(struct {
		Developers []developerScoreJSON `json:"developers"`
		Total      *teamScoreJSON       `json:"total"`
	}{typed.Developers, typed.Total}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// Recorded from unmodified origin/main 038dc40f, before #972/U97.
	baseline, err := os.ReadFile("testdata/join_readiness_score_fields.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(got, '\n'), baseline) {
		t.Fatalf("score fields differ from base bytes:\n%s", got)
	}
	dq := dataQualityOf(t, resp)
	if dq["unjoined_developers"] != nil {
		t.Fatal("identity itself should join")
	}
	block, ok := dq["join_readiness"].(map[string]any)
	if !ok {
		t.Fatal("missing join_readiness")
	}
	if len(block["incompatible_repos"].([]any)) != 1 {
		t.Fatalf("mismatch: %v", block)
	}
	scoped, _ := getScoresRaw(t, h, "?since="+scoresSince()+"&repo="+repoAlpha)
	b, _ := json.Marshal(dataQualityOf(t, scoped)["join_readiness"])
	if strings.Contains(string(b), repoBeta) {
		t.Fatalf("scope leaked: %s", b)
	}
}

func TestRepoJoin_AnonymousNoNewCounts(t *testing.T) {
	for _, mode := range []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision} {
		t.Run(mode.String(), func(t *testing.T) {
			h, db := newTestHandler(t)
			h.aggregation = mode
			h.unsealedRecompute = true
			seedRepoCost(t, db, repoAlpha, "alice", "issue-42", 10)
			seedRepoOutcome(t, db, repoBeta, "alice", "issue-42", 3)
			resp, _ := getScoresRaw(t, h, "?since="+scoresSince())
			dq := dataQualityOf(t, resp)
			for _, key := range []string{"join_readiness", "captured_dollar_diagnostics"} {
				if _, ok := dq[key]; ok {
					t.Fatalf("anonymous receipt: %s=%v", key, dq[key])
				}
			}
		})
	}
}

func TestCapturedDollarLabels(t *testing.T) {
	h, db := newTestHandler(t)
	WithCaptureEngines(map[string]bool{"claude-code": true, "codex-rollout": false})(h)
	seedRepoCost(t, db, repoAlpha, "alice", "unattributed:main", 10)
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{Developer: "alice", Repo: repoAlpha, IssueID: "issue-1", Model: "claude-sonnet-4", Source: "api", Fidelity: "realtime", CostMicro: 10000000, Timestamp: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	seedRepoCost(t, db, repoBeta, "bob", "unattributed:foreign-only", 90)
	block := getCapturedDollars(t, h, "?since="+scoresSince()+"&repo="+repoAlpha)
	raw, _ := json.Marshal(block)
	if strings.Contains(string(raw), "foreign-only") || strings.Contains(string(raw), repoBeta) {
		t.Fatalf("foreign diagnostic leaked: %s", raw)
	}
	resp, _ := getScoresRaw(t, h, "?since="+scoresSince())
	if _, ok := dataQualityOf(t, resp)["captured_dollar_diagnostics"]; ok {
		t.Fatal("live diagnostic in reproducible scores")
	}
	enablement := block["engine_enablement"].(map[string]any)
	if enablement["claude-code"] != "enabled" || enablement["codex-rollout"] != "disabled" || enablement["opencode"] != "unknown" {
		t.Fatalf("engine enablement: %v", enablement)
	}
	buckets := block["buckets"].([]any)
	if len(buckets) != 1 {
		t.Fatalf("buckets: %v", buckets)
	}
	bucket := buckets[0].(map[string]any)
	if bucket["engine"] != "claude-code" || bucket["bucket"] != "unattributed:main" || bucket["subagent_origin"] != "unknown" || bucket["share"] != float64(1) {
		t.Fatalf("bucket: %v", bucket)
	}
	if block["share_label"] != "share of captured dollars" || block["scope"] != repoAlpha || block["sample_size"] != float64(1) || block["captured_cost_usd"] != float64(10) {
		t.Fatalf("receipt: %v", block)
	}
}

func TestRepoJoin_ControlsAndScoreBytes(t *testing.T) {
	h, db := newTestHandler(t)
	seedRepoCost(t, db, repoAlpha, "local-alice", "issue-42", 10)
	seedRepoOutcome(t, db, repoAlpha, "alice", "issue-42", 3)
	if err := db.UpsertDeveloperAlias(context.Background(), "local-alice", "alice", "test:fixture"); err != nil {
		t.Fatal(err)
	}
	since := time.Now().UTC().AddDate(0, 0, -7)
	q := scoresQuery{since: since}
	var data scoresWindowData
	if err := db.ReadSnapshot(context.Background(), func(s *store.Snapshot) error {
		var err error
		data, err = h.loadScoresWindow(context.Background(), s, q)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := h.buildScoresResponse(data, q, false)
	scoreBytes := func(s scoresResponse) []byte {
		b, err := json.Marshal(struct {
			Developers []developerScoreJSON `json:"developers"`
			Total      *teamScoreJSON       `json:"total"`
		}{s.Developers, s.Total})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	// Serialize the actual response score fields before and after constructing the
	// receipt: diagnostics must not mutate any score input, directly or by alias.
	baseline := scoreBytes(before)
	readiness := buildJoinReadiness(data.win)
	after, _ := h.buildScoresResponse(data, q, false)
	if string(baseline) != string(scoreBytes(after)) {
		t.Fatal("diagnostics changed score bytes")
	}
	if len(readiness.IncompatibleRepos)+len(readiness.OutcomeOnlyRepos)+len(readiness.CostOnlyRepos)+len(readiness.IssueSpendWithoutOutcome) != 0 {
		t.Fatalf("valid alias/same repo: %+v", readiness)
	}
	// A colleague's merge cannot satisfy the spender's join, even in the same repo.
	seedRepoCost(t, db, repoAlpha, "bob", "issue-42", 5)
	resp, _ := getScoresRaw(t, h, "?since="+scoresSince())
	gaps := dataQualityOf(t, resp)["join_readiness"].(map[string]any)["issue_spend_without_same_developer_outcome"].([]any)
	if len(gaps) != 1 || gaps[0].(map[string]any)["developer"] != "bob" {
		t.Fatalf("collaboration: %v", gaps)
	}
}

func TestRepoJoin_ParallelSameIssueJoinsAreHealthy(t *testing.T) {
	h, db := newTestHandler(t)
	for _, repo := range []string{repoAlpha, repoBeta} {
		seedRepoCost(t, db, repo, "alice", "issue-42", 10)
		seedRepoOutcome(t, db, repo, "alice", "issue-42", 3)
	}
	resp, _ := getScoresRaw(t, h, "?since="+scoresSince())
	d := dataQualityOf(t, resp)["join_readiness"].(map[string]any)
	for _, key := range []string{"outcome_only_repos", "cost_only_repos", "incompatible_repos", "ambiguous_repo_blind_matches", "issue_spend_without_same_developer_outcome"} {
		if len(d[key].([]any)) != 0 {
			t.Fatalf("healthy joins flagged: %s=%v", key, d[key])
		}
	}
}

func TestRepoJoin_RepoBlindAmbiguity(t *testing.T) {
	h, db := newTestHandler(t)
	seedRepoCost(t, db, "unqualified", "alice", "issue-42", 10)
	seedRepoOutcome(t, db, repoAlpha, "alice", "issue-42", 2)
	seedRepoOutcome(t, db, repoBeta, "alice", "issue-42", 3)
	resp, _ := getScoresRaw(t, h, "?since="+scoresSince())
	block := dataQualityOf(t, resp)["join_readiness"].(map[string]any)
	if block["unqualified_cost_share"] != float64(1) || len(block["ambiguous_repo_blind_matches"].([]any)) != 1 {
		t.Fatalf("ambiguity: %v", block)
	}
}

type insertDuringJoinRead struct {
	*store.Snapshot
	insert func()
}

func (r *insertDuringJoinRead) CostCompositionWindow(ctx context.Context, since, until time.Time, scope store.RepoScope) (store.CostComposition, error) {
	r.insert()
	return r.Snapshot.CostCompositionWindow(ctx, since, until, scope)
}

func TestRepoJoin_SameSnapshotAsScores(t *testing.T) {
	h, db := newTestHandler(t)
	seedRepoOutcome(t, db, repoAlpha, "alice", "issue-42", 3)
	q := scoresQuery{since: time.Now().UTC().AddDate(0, 0, -7)}
	var data scoresWindowData
	inserted := false
	if err := db.ReadSnapshot(context.Background(), func(s *store.Snapshot) error {
		r := &insertDuringJoinRead{Snapshot: s, insert: func() {
			inserted = true
			seedRepoCost(t, db, repoBeta, "bob", "issue-99", 20)
		}}
		var err error
		data, err = h.loadScoresWindow(context.Background(), r, q)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("concurrent write did not fire")
	}
	resp, err := h.buildScoresResponse(data, q, false)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := json.Marshal(resp.DataQuality.JoinReadiness)
	if strings.Contains(string(block), repoBeta) {
		t.Fatalf("post-snapshot cost leaked: %s", block)
	}
	live, _ := getScoresRaw(t, h, "?since="+scoresSince())
	if len(dataQualityOf(t, live)["join_readiness"].(map[string]any)["cost_only_repos"].([]any)) != 1 {
		t.Fatal("write not visible after snapshot")
	}
}

func TestRepoJoin_CorrectionPoints(t *testing.T) {
	d := buildJoinReadiness(windowScores{
		outcomes:       []store.Outcome{{Repo: repoAlpha}},
		scoredOutcomes: []scoring.Outcome{{Weight: 3, Quality: 1, Correction: &scoring.CorrectionValues{Points: 1}}},
		canon:          func(s string) string { return s },
	})
	if len(d.OutcomeOnlyRepos) != 1 || d.OutcomeOnlyRepos[0].Points != 1 {
		t.Fatalf("points: %+v", d.OutcomeOnlyRepos)
	}
}

func TestRepoJoin_RealRepoPresenceIgnoresUnqualified(t *testing.T) {
	h, db := newTestHandler(t)
	seedRepoOutcome(t, db, repoAlpha, "alice", "issue-1", 3)
	seedRepoOutcome(t, db, "unqualified", "bob", "issue-2", 2)
	seedRepoCost(t, db, repoBeta, "bob", "issue-3", 10)
	seedRepoCost(t, db, "unqualified", "alice", "issue-4", 5)
	resp, _ := getScoresRaw(t, h, "?since="+scoresSince())
	d := dataQualityOf(t, resp)["join_readiness"].(map[string]any)
	outcomes, costs := d["outcome_only_repos"].([]any), d["cost_only_repos"].([]any)
	if len(outcomes) != 1 || outcomes[0].(map[string]any)["repo"] != repoAlpha || len(costs) != 1 || costs[0].(map[string]any)["repo"] != repoBeta {
		t.Fatalf("real repo gaps lost: %v", d)
	}
	if d["unqualified_cost_share"].(float64) != 1.0/3 {
		t.Fatal("incorrect captured-dollar denominator")
	}
	scoped, _ := getScoresRaw(t, h, "?since="+scoresSince()+"&repo="+repoBeta)
	if dataQualityOf(t, scoped)["join_readiness"].(map[string]any)["unqualified_cost_share"] != float64(0) {
		t.Fatal("strict repo includes unqualified cost")
	}
}

func getCapturedDollars(t *testing.T, h *Handler, query string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.handleCapturedDollars(rec, httptest.NewRequest(http.MethodGet, "/api/v1/diagnostics/captured-dollars"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("diagnostics status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCapturedDollarEndpointReadScopeAndAnonymous(t *testing.T) {
	const readToken = "read-viewer-token-of-len-32-bbbb"
	h, _ := newTestHandlerWithScopes(t, "write-operator-token-of-len-32-aaaa", readToken)
	target := "/api/v1/diagnostics/captured-dollars"
	if code, body := doRequestWithHeader(t, h, http.MethodGet, target, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d %s", code, body)
	}
	if code, body := doRequestWithHeader(t, h, http.MethodGet, target, nil, http.Header{"Authorization": {"Bearer " + readToken}}); code != http.StatusOK {
		t.Fatalf("read scope: %d %s", code, body)
	}
	for _, mode := range []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision} {
		h.aggregation = mode
		if code, body := doRequestWithHeader(t, h, http.MethodGet, target, nil, http.Header{"Authorization": {"Bearer " + readToken}}); code != http.StatusForbidden || strings.Contains(string(body), "captured_cost_usd") {
			t.Fatalf("anonymous: %d %s", code, body)
		}
	}
}

// Pin the public route's 400 categories through Register's real mux and read auth.
// The wire error is a string; its prefix identifies the validation failure class.
func TestCapturedDollarEndpointBadRequest(t *testing.T) {
	const readToken = "read-viewer-token-of-len-32-bbbb"
	h, _ := newTestHandlerWithScopes(t, "write-operator-token-of-len-32-aaaa", readToken)
	for _, tc := range []struct {
		name      string
		query     string
		wantClass string
	}{
		{"bad_since", "?since=not-a-date", "invalid since:"},
		{"bad_until", "?since=2026-10-01&until=not-a-date", "invalid until:"},
		{"bad_before", "?since=2026-10-01&before=not-a-date", "invalid until:"},
		{"equal_window", "?since=2026-10-01&until=2026-10-01", "until must be after since"},
		{"reversed_window", "?since=2026-10-02&until=2026-10-01", "until must be after since"},
		{"unknown_parameter", "?since=2026-10-01&repos=acme/alpha", "unknown query parameter(s):"},
		{"disallowed_repo", "?since=2026-10-01&repo=unqualified", "invalid repo:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doRequestWithHeader(t, h, http.MethodGet,
				"/api/v1/diagnostics/captured-dollars"+tc.query, nil,
				http.Header{"Authorization": {"Bearer " + readToken}})
			if code != http.StatusBadRequest {
				t.Errorf("status=%d, want %d; body=%s", code, http.StatusBadRequest, body)
			}
			var wire struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(body, &wire); err != nil {
				t.Fatalf("decode error response: %v; body=%s", err, body)
			}
			if !strings.HasPrefix(wire.Error, tc.wantClass) {
				t.Errorf("error=%q, want class %q", wire.Error, tc.wantClass)
			}
		})
	}
}

func TestRepoJoin_ListsBoundedWithTotals(t *testing.T) {
	w := windowScores{canon: func(s string) string { return s }}
	for i := 79; i >= 0; i-- {
		issue := fmt.Sprintf("issue-%03d", i)
		w.issueCosts = append(w.issueCosts, store.DevIssueCost{Developer: "alice", IssueID: issue, Repo: repoAlpha, TotalCostMicro: 1, CapturedRealtimeMicro: 1})
		w.issueCosts = append(w.issueCosts, store.DevIssueCost{Developer: "bob", IssueID: issue, Repo: "unqualified", TotalCostMicro: 1, CapturedRealtimeMicro: 1})
		for _, o := range []store.Outcome{{Developer: "alice", IssueID: issue, Repo: repoBeta}, {Developer: "bob", IssueID: issue, Repo: repoAlpha}, {Developer: "bob", IssueID: issue, Repo: repoBeta}} {
			w.outcomes = append(w.outcomes, o)
			w.scoredOutcomes = append(w.scoredOutcomes, scoring.Outcome{})
		}
	}
	d := buildJoinReadiness(w)
	if len(d.IncompatibleRepos) != 50 || d.IncompatibleReposTotal != 80 || len(d.AmbiguousRepoBlindMatches) != 50 || d.AmbiguousRepoBlindMatchesTotal != 80 || len(d.IssueSpendWithoutOutcome) != 50 || d.IssueSpendWithoutOutcomeTotal != 80 {
		t.Fatalf("bounds/totals: %+v", d)
	}
	if d.IncompatibleRepos[0].IssueID != "issue-000" || d.IncompatibleRepos[49].IssueID != "issue-049" {
		t.Fatal("cap before stable sort")
	}
}
