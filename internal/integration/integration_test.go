//go:build integration

// Package integration holds the cross-package end-to-end tests that run only
// under `make check-full` (-tags integration). Before #64 the tag matched
// zero files, so check-full was silently equivalent to check.
//
// The test wires the REAL store (SQLite on disk), REST API, and webhook
// handler onto one mux — the same composition cmd/tierd builds — and drives
// the full pipeline over HTTP: cost capture → merged-PR outcome → TIER
// score → revert degradation → finance ledger → Spend Leverage.
package integration

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/health"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/store"
	"github.com/tiermetric/tier/internal/webhook"
)

const (
	apiToken      = "integration-api-token"
	webhookSecret = "integration-webhook-secret"
)

// newServer builds the tierd HTTP composition over a fresh on-disk SQLite
// store: API (bearer-gated) + webhook (HMAC-gated), exactly as runServe
// mounts them.
func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv, _ := newServerWithStore(t)
	return srv
}

// newServerWithStore is newServer's factored core: it returns the same HTTP
// composition plus the live *store.DB backing it, so tests that drive the
// store directly (e.g. the watcher wire test, which writes through the real
// collector and reads the result back over HTTP) share the exact wiring
// instead of duplicating it.
func newServerWithStore(t *testing.T) (*httptest.Server, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "integration.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	api.New(db, quiet, apiToken, nil, "integration", api.RateLimitConfig{}).Register(mux)
	mux.Handle("POST /webhook/github", webhook.New(db, webhookSecret, quiet))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, db
}

func signBody(body []byte) string {
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// getWatcherHealthz GETs /api/v1/healthz (an open probe — no auth) and returns
// the decoded watcher snapshot.
func getWatcherHealthz(t *testing.T, url string) health.WatcherSnapshot {
	t.Helper()
	resp, err := http.Get(url + "/api/v1/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Watcher health.WatcherSnapshot `json:"watcher"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	return body.Watcher
}

// TestPipeline_RecordingIngesterStampsHealthz is the #50 end-to-end proof, on
// the #46 seam: an event ingested through the ingester.RecordingIngester
// decorator — the exact wiring cmd/tierd uses for the live watcher
// (Ingester: RecordingIngester(state, SourceTagger(..., Store(db)))) — makes
// /healthz report a non-zero last_event_ts over HTTP. Without the decorator,
// last_event_ts stayed absent forever (the bug #50 fixes). The unit tests cover
// the decorator and WatcherState in isolation; this is the only test that proves
// the two are wired together end to end.
func TestPipeline_RecordingIngesterStampsHealthz(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "integration-healthz.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Watcher state in the running state, wrapped exactly as runServe wires it:
	// RecordingIngester over the shared ingester.Store adapter.
	ws := health.NewWatcherState()
	ws.SetRunning()
	recordingIngester := ingester.RecordingIngester(ws, ingester.Store(db))

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	api.New(db, quiet, apiToken, ws, "integration", api.RateLimitConfig{}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Before any event, /healthz omits last_event_ts (running, but no data yet).
	if snap := getWatcherHealthz(t, srv.URL); snap.LastEventTS != nil {
		t.Fatalf("last_event_ts present before any event: %v", snap.LastEventTS)
	}

	// Simulate the watcher landing one event through the decorator.
	if err := recordingIngester.Ingest(context.Background(), collector.TokenEvent{
		Developer: "alice",
		Model:     "claude-sonnet-4",
		Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// Now /healthz must report a real, non-zero last_event_ts.
	snap := getWatcherHealthz(t, srv.URL)
	if snap.LastEventTS == nil {
		t.Fatalf("last_event_ts still absent after an event flowed through RecordingIngester")
	}
	if snap.LastEventTS.IsZero() {
		t.Errorf("last_event_ts is the zero time; want a real timestamp")
	}
}

// postJSON sends body with the bearer token and asserts the wanted status.
func postJSON(t *testing.T, url string, body map[string]any, wantStatus int) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != wantStatus {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s: status %d, want %d; body=%s", url, resp.StatusCode, wantStatus, b)
	}
}

// postWebhook sends a signed GitHub webhook event with a unique delivery id.
func postWebhook(t *testing.T, url, event, deliveryID string, payload map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal webhook: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url+"/webhook/github", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	req.Header.Set("X-Hub-Signature-256", signBody(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("webhook POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("webhook %s: status %d, want 204; body=%s", event, resp.StatusCode, b)
	}
}

// scoresResponse mirrors the wire shape of GET /api/v1/scores (the subset
// this test asserts on). Field drift between this and the API is the kind of
// cross-package break this tier exists to catch.
type scoresResponse struct {
	Developers  []devScore `json:"developers"`
	DataQuality *struct {
		ZeroTokenOutcomes []struct {
			Developer string `json:"developer"`
			IssueID   string `json:"issue_id"`
			Tokens    int64  `json:"tokens"`
		} `json:"zero_token_outcomes"`
	} `json:"data_quality"`
}

// devScore is one element of scoresResponse.Developers — named so the wire
// shape is declared once and shared by the alice() lookup helper.
type devScore struct {
	Developer       string  `json:"developer"`
	TIER            float64 `json:"tier"`
	WeightedPoints  float64 `json:"weighted_points"`
	TotalCostUSD    float64 `json:"total_cost_usd"`
	SpendLeverage   float64 `json:"spend_leverage"`
	SampleN         int     `json:"sample_n"`
	CILow           float64 `json:"ci_low"`
	CIHigh          float64 `json:"ci_high"`
	Ranked          bool    `json:"ranked"`
	FlaggedOutcomes int     `json:"flagged_outcomes"`
}

func getScores(t *testing.T, url string) scoresResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url+"/api/v1/scores?since=2026-01-01", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// Score GETs require the bearer token when one is configured (#59).
	req.Header.Set("Authorization", "Bearer "+apiToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /scores: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /scores: status %d; body=%s", resp.StatusCode, b)
	}
	var sr scoresResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode scores: %v", err)
	}
	return sr
}

// alice is the common single-developer lookup, expressed via the shared
// devByName helper (declared in watcher_wire_test.go) so there is one lookup.
func alice(t *testing.T, sr scoresResponse) devScore {
	t.Helper()
	return devByName(t, sr, "alice")
}

// approxEq compares two floats within a tight tolerance. The values under test
// are exact-division results (e.g. 3/0.0025), so 1e-6 is generous headroom
// against float rounding while still catching any real formula regression.
func approxEq(a, b float64) bool {
	return math.Abs(a-b) < 1e-6
}

// TestPipeline_CostToScoreToRevertToLeverage is the full TIER lifecycle,
// exercising both event-derived quality signals through real HTTP + HMAC (#134):
//
//  1. POST /api/v1/costs             — alice spends $2.50 on issue-42
//  2. webhook pull_request (merged)  — size/M label → weight 3, quality 1.0
//     → TIER = 3 / (2.50/1000) = 1200
//  3. webhook workflow_run (failure) — CI fails on the merge commit → quality 0.7
//     → TIER = 2.1 / (2.50/1000) = 840
//  4. webhook push (revert, "broke") — quality revert floors to 0.1
//     → TIER = 0.3 / (2.50/1000) = 120
//  5. POST /api/v1/actual_spend      — finance posts $1.25 actual
//     → Spend Leverage = 2.50 / 1.25 = 2.0
func TestPipeline_CostToScoreToRevertToLeverage(t *testing.T) {
	srv := newServer(t)

	// 1. Capture cost.
	postJSON(t, srv.URL+"/api/v1/costs", map[string]any{
		"developer": "alice", "issue_id": "issue-42", "model": "claude-sonnet-4",
		"input_tokens": 1000, "output_tokens": 500, "cost_usd": 2.50,
	}, http.StatusCreated)

	// 2. Merged PR with a size label.
	const mergeSHA = "0123456789abcdef0123456789abcdef01234567"
	postWebhook(t, srv.URL, "pull_request", "delivery-pr-1", map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number": 7, "merged": true, "merge_commit_sha": mergeSHA,
			"head":   map[string]any{"ref": "feature/42-foo"},
			"user":   map[string]any{"login": "alice"},
			"labels": []map[string]any{{"name": "size/M"}},
		},
	})

	a := alice(t, getScores(t, srv.URL))
	if !approxEq(a.WeightedPoints, 3.0) || !approxEq(a.TIER, 1200.0) {
		t.Fatalf("after merge: points=%.2f TIER=%.2f, want 3.00/1200.00", a.WeightedPoints, a.TIER)
	}

	// 3. CI fails on the merge commit → quality 0.7 (#134 Events 1-2).
	postWebhook(t, srv.URL, "workflow_run", "delivery-wf-1", map[string]any{
		"action": "completed",
		"workflow_run": map[string]any{
			"head_sha": mergeSHA, "head_branch": "main",
			"conclusion": "failure", "run_attempt": 1,
			"updated_at": time.Now().UTC().Format(time.RFC3339),
		},
		"repository": map[string]any{"default_branch": "main"},
	})

	a = alice(t, getScores(t, srv.URL))
	if !approxEq(a.WeightedPoints, 2.1) || !approxEq(a.TIER, 840.0) {
		t.Fatalf("after CI failure: points=%.2f TIER=%.2f, want 2.10/840.00", a.WeightedPoints, a.TIER)
	}

	// 4. Revert push referencing the merge commit footer, with a quality keyword
	//    ("broke prod") → quality revert floors to 0.1 (#134 Event 4).
	postWebhook(t, srv.URL, "push", "delivery-push-1", map[string]any{
		"commits": []map[string]any{{
			"id":      "fedcba9876543210fedcba9876543210fedcba98",
			"message": "Revert \"feat: add foo\"\n\nThis reverts commit " + mergeSHA + ". broke prod",
			"author":  map[string]any{"username": "carol"},
		}},
	})

	a = alice(t, getScores(t, srv.URL))
	if !approxEq(a.WeightedPoints, 0.3) || !approxEq(a.TIER, 120.0) {
		t.Fatalf("after revert: points=%.2f TIER=%.2f, want 0.30/120.00", a.WeightedPoints, a.TIER)
	}

	// 5. Finance posts the actual invoice — half of list price.
	postJSON(t, srv.URL+"/api/v1/actual_spend", map[string]any{
		"developer": "alice", "period": "2026-06", "actual_paid_usd": 1.25,
	}, http.StatusCreated)

	a = alice(t, getScores(t, srv.URL))
	if !approxEq(a.SpendLeverage, 2.0) {
		t.Fatalf("spend leverage = %.2f, want 2.00", a.SpendLeverage)
	}
	if !approxEq(a.TotalCostUSD, 2.50) {
		t.Fatalf("total cost = %.4f, want 2.5000", a.TotalCostUSD)
	}
}

// TestPipeline_RevertResolvedByIssueID_RealStore drives the tier-2 revert path —
// a footerless, issue-named revert — end to end over HTTP into the REAL store: the
// surviving gap #149 names. The store SQL itself is already covered by
// internal/store/repo_qualify_test.go, but nothing drove it through the webhook
// wire; the only tier-2 test (webhook.TestHandlePush_RevertResolvedByIssueIDLookup)
// runs against the fake store and proves nothing about the real query behind HTTP.
//
// Two merged PRs land on issue-42 in the SAME repo (acme/app): alice first, then
// bob. A revert whose message NAMES the issue ("closes #42") but carries NO
// "This reverts commit <sha>" footer forces tier 1 (SHA lookup) to miss and tier 2
// (LatestOutcomeByIssue) to fire. The repo-qualified
// `ORDER BY (repo = ?) DESC, id DESC` must resolve to bob — the most-recent outcome
// — so the penalty lands on him and alice is untouched. If it lands on alice the
// recency semantics broke; if neither degrades the tier-2 wire broke — exactly the
// regressions the finding predicts.
//
// #231: keep ALL THREE payloads on one repository.full_name. The query is
// repo-scoped, so a mismatched or absent repo on the revert push would exercise the
// sentinel-tolerance branch instead of the intended exact-repo leg — a different
// assertion. No revert keyword → EventRevertQuality (the conservative default) →
// quality floors to 0.1, so the degraded developer lands at 3 × 0.1 = 0.3.
func TestPipeline_RevertResolvedByIssueID_RealStore(t *testing.T) {
	srv := newServer(t)
	const repo = "acme/app"

	// 1. Both developers spend on issue-42 so both appear in /scores.
	for _, dev := range []string{"alice", "bob"} {
		postJSON(t, srv.URL+"/api/v1/costs", map[string]any{
			"developer": dev, "issue_id": "issue-42", "model": "claude-sonnet-4",
			"input_tokens": 1000, "output_tokens": 500, "cost_usd": 2.50,
		}, http.StatusCreated)
	}

	// 2. Merged PR #1 — alice, feature/42-foo, size/M → weight 3, quality 1.0.
	postWebhook(t, srv.URL, "pull_request", "delivery-pr-issueid-1", map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number": 7, "merged": true,
			"merge_commit_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"head":             map[string]any{"ref": "feature/42-foo"},
			"user":             map[string]any{"login": "alice"},
			"labels":           []map[string]any{{"name": "size/M"}},
		},
		"repository": map[string]any{"full_name": repo},
	})

	// 3. Merged PR #2 — bob, feature/42-bar (SAME issue-42), distinct SHA + number.
	//    bob's outcome is inserted LAST, so it holds the higher id — the tier-2
	//    recency winner the revert must resolve to.
	postWebhook(t, srv.URL, "pull_request", "delivery-pr-issueid-2", map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number": 8, "merged": true,
			"merge_commit_sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"head":             map[string]any{"ref": "feature/42-bar"},
			"user":             map[string]any{"login": "bob"},
			"labels":           []map[string]any{{"name": "size/M"}},
		},
		"repository": map[string]any{"full_name": repo},
	})

	// Both start at a clean weight-3, quality-1.0 outcome.
	sr := getScores(t, srv.URL)
	if a := devByName(t, sr, "alice"); !approxEq(a.WeightedPoints, 3.0) {
		t.Fatalf("alice pre-revert points = %.2f, want 3.00", a.WeightedPoints)
	}
	if b := devByName(t, sr, "bob"); !approxEq(b.WeightedPoints, 3.0) {
		t.Fatalf("bob pre-revert points = %.2f, want 3.00", b.WeightedPoints)
	}

	// 4. Footerless, issue-named revert: no "This reverts commit <sha>" footer, so
	//    tier 1 misses and tier 2 resolves the target through the real SQL. The
	//    subject names issue #42 via a close directive so issueref recovers issue-42.
	postWebhook(t, srv.URL, "push", "delivery-push-issueid-1", map[string]any{
		"commits": []map[string]any{{
			"id":      "cccccccccccccccccccccccccccccccccccccccc",
			"message": `Revert "feat: add bar (closes #42)"`,
			"author":  map[string]any{"username": "dave"},
		}},
		"repository": map[string]any{"full_name": repo},
	})

	// 5. The penalty landed on bob (the most-recent outcome for issue-42), through
	//    the REAL `ORDER BY (repo = ?) DESC, id DESC` SQL; alice is untouched.
	sr = getScores(t, srv.URL)
	if b := devByName(t, sr, "bob"); !approxEq(b.WeightedPoints, 0.3) {
		t.Fatalf("bob post-revert points = %.2f, want 0.30 (3 × 0.1) — tier-2 wire or recency broke", b.WeightedPoints)
	}
	if a := devByName(t, sr, "alice"); !approxEq(a.WeightedPoints, 3.0) {
		t.Fatalf("alice post-revert points = %.2f, want 3.00 — penalty leaked onto the wrong developer", a.WeightedPoints)
	}
}

// TestPipeline_AuthBoundaries pins the #59/#60 exposure model end-to-end:
// every mutating or data-bearing route refuses unauthenticated callers.
func TestPipeline_AuthBoundaries(t *testing.T) {
	srv := newServer(t)

	// Unauthenticated cost write → 401.
	body, _ := json.Marshal(map[string]any{
		"developer": "mallory", "issue_id": "issue-1", "model": "m", "cost_usd": 1.0,
	})
	resp, err := http.Post(srv.URL+"/api/v1/costs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST /costs: %d, want 401", resp.StatusCode)
	}

	// Unauthenticated score read → 401 (#59).
	resp, err = http.Get(srv.URL + "/api/v1/scores")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /scores: %d, want 401", resp.StatusCode)
	}

	// Unsigned webhook → 403.
	prBody, _ := json.Marshal(map[string]any{"action": "closed"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhook/github", bytes.NewReader(prBody))
	req.Header.Set("X-GitHub-Event", "pull_request")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("unsigned webhook: %d, want 403", resp.StatusCode)
	}

	// Probes stay open without credentials.
	for _, path := range []string{"/api/v1/health", "/api/v1/healthz", "/api/v1/livez"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: %d, want 200", path, resp.StatusCode)
		}
	}
}

// TestAliasJoin_EndToEnd is the #125 cross-package proof: a cost row captured
// under the OS username and an outcome recorded under the GitHub login collapse
// into ONE scored developer once an alias maps the two, exercised over the real
// store + REST wiring (not a fake). Without the alias the two identities would
// score separately (cost with TIER 0, outcome vanished pre-#125); with it, one
// canonical row carries both the spend and the points.
func TestAliasJoin_EndToEnd(t *testing.T) {
	srv, db := newServerWithStore(t)

	// Map the GitHub login (outcome side) to the OS username (cost side).
	postJSON(t, srv.URL+"/api/v1/developer_alias", map[string]any{
		"alias":     "asmith-gh",
		"canonical": "alice.smith",
	}, http.StatusCreated)

	// Cost captured under the OS username via the manual REST endpoint (source api).
	postJSON(t, srv.URL+"/api/v1/costs", map[string]any{
		"developer":     "alice.smith",
		"issue_id":      "issue-1",
		"model":         "claude-sonnet-4-5",
		"input_tokens":  1000,
		"output_tokens": 500,
		"cost_usd":      2.5,
	}, http.StatusCreated)

	// Outcome recorded under the GitHub login, inserted directly through the
	// store (the webhook path stamps the raw PR author login the same way).
	if _, err := db.InsertOutcome(context.Background(), store.Outcome{
		Developer: "asmith-gh",
		IssueID:   "issue-1",
		Weight:    3,
		Quality:   1,
		Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}

	sr := getScores(t, srv.URL)
	if len(sr.Developers) != 1 {
		t.Fatalf("got %d developer rows, want 1 merged canonical row; resp=%+v", len(sr.Developers), sr)
	}
	d := sr.Developers[0]
	if d.Developer != "alice.smith" {
		t.Errorf("developer = %q, want alice.smith (canonical)", d.Developer)
	}
	if d.WeightedPoints != 3 {
		t.Errorf("weighted_points = %v, want 3 (outcome joined through alias)", d.WeightedPoints)
	}
	if d.TotalCostUSD != 2.5 {
		t.Errorf("total_cost_usd = %v, want 2.5", d.TotalCostUSD)
	}
	// TIER = 3 / (2.5/1000) = 1200.
	if !approxEq(d.TIER, 1200) {
		t.Errorf("tier = %v, want 1200", d.TIER)
	}
}

// findDev returns the scored row for developer name, failing the test if absent.
func findDev(t *testing.T, sr scoresResponse, name string) devScore {
	t.Helper()
	for _, d := range sr.Developers {
		if d.Developer == name {
			return d
		}
	}
	t.Fatalf("%s not in scores response: %+v", name, sr)
	return devScore{}
}

// TestPipeline_RankingFloorAndBootstrapCI is the #133 end-to-end proof over the
// real store + REST wiring: a developer with 3 merged PRs and $10 of cost clears
// both ranking floors (ranked, non-zero bootstrap CI bracketing TIER), while a
// developer with a single $0.0004 PR — the C3 lucky-PR case whose bare TIER
// would otherwise top the board — is listed but flagged unranked with zero CI.
func TestPipeline_RankingFloorAndBootstrapCI(t *testing.T) {
	srv, db := newServerWithStore(t)

	// ranked-dev: $10 list-price cost + 3 merged-PR outcomes.
	postJSON(t, srv.URL+"/api/v1/costs", map[string]any{
		"developer": "ranked-dev", "issue_id": "issue-1", "model": "claude-sonnet-4",
		"input_tokens": 1000, "output_tokens": 500, "cost_usd": 10.0,
	}, http.StatusCreated)
	for i, w := range []float64{3, 5, 8} {
		issueID := "issue-" + string(rune('1'+i))
		// Every outcome's issue needs recorded tokens or the #136 zero-token
		// tripwire flags it and unranks ranked-dev. issue-1 already has tokens
		// from the POST /costs above; give issue-2 and issue-3 their own.
		if i > 0 {
			if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
				Developer: "ranked-dev", IssueID: issueID, Model: "claude-sonnet-4",
				InputTok: 5000, Source: "jsonl", Fidelity: "realtime",
				Timestamp: time.Now().UTC(),
			}); err != nil {
				t.Fatalf("InsertTokenEvent: %v", err)
			}
		}
		if _, err := db.InsertOutcome(context.Background(), store.Outcome{
			Developer: "ranked-dev",
			IssueID:   issueID,
			Weight:    w, Quality: 1, Timestamp: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("InsertOutcome: %v", err)
		}
	}

	// lucky: one micro-cost PR — below both floors.
	postJSON(t, srv.URL+"/api/v1/costs", map[string]any{
		"developer": "lucky", "issue_id": "issue-9", "model": "claude-sonnet-4",
		"input_tokens": 1, "output_tokens": 1, "cost_usd": 0.0004,
	}, http.StatusCreated)
	if _, err := db.InsertOutcome(context.Background(), store.Outcome{
		Developer: "lucky", IssueID: "issue-9",
		Weight: 0.5, Quality: 1, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}

	sr := getScores(t, srv.URL)

	r := findDev(t, sr, "ranked-dev")
	if !r.Ranked {
		t.Errorf("ranked-dev Ranked = false, want true")
	}
	if r.SampleN != 3 {
		t.Errorf("ranked-dev sample_n = %d, want 3", r.SampleN)
	}
	if r.CILow <= 0 || r.CIHigh <= 0 {
		t.Errorf("ranked-dev CIs = [%v, %v], want both > 0", r.CILow, r.CIHigh)
	}
	if r.CILow > r.TIER || r.TIER > r.CIHigh {
		t.Errorf("ranked-dev TIER %v not within CI [%v, %v]", r.TIER, r.CILow, r.CIHigh)
	}

	l := findDev(t, sr, "lucky")
	if l.Ranked {
		t.Errorf("lucky Ranked = true, want false (below both floors)")
	}
	if l.CILow != 0 || l.CIHigh != 0 {
		t.Errorf("unranked lucky CIs = [%v, %v], want [0, 0]", l.CILow, l.CIHigh)
	}
}

// findDevOpt returns (row, true) for developer name, or (_, false) if absent —
// unlike findDev it does not fail the test, so a caller can assert absence.
func findDevOpt(sr scoresResponse, name string) (devScore, bool) {
	for _, d := range sr.Developers {
		if d.Developer == name {
			return d, true
		}
	}
	return devScore{}, false
}

// TestPipeline_ZeroTokenOutcomeTripwire is the #136 end-to-end proof over real
// HTTP + HMAC: a merged PR whose issue has NO recorded tokens is flagged and
// unranks its developer, surfaces in the data_quality block — and once tokens
// are recorded for that issue inside the pre-merge window, the flag clears on a
// re-query. This is the G-02 off-books gaming vector and its remedy.
func TestPipeline_ZeroTokenOutcomeTripwire(t *testing.T) {
	srv, db := newServerWithStore(t)

	// Merged PR by "wiretest" on issue-999 — but NO cost/token events posted.
	postWebhook(t, srv.URL, "pull_request", "delivery-136-1", map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number": 999, "merged": true,
			"merge_commit_sha": "aaaa1111bbbb2222cccc3333dddd4444eeee5555",
			"head":             map[string]any{"ref": "feature/999-offbooks"},
			"user":             map[string]any{"login": "wiretest"},
			"labels":           []map[string]any{{"name": "size/M"}},
		},
	})

	sr := getScores(t, srv.URL)
	d, ok := findDevOpt(sr, "wiretest")
	if !ok {
		t.Fatalf("wiretest absent from /scores; got %+v", sr.Developers)
	}
	if d.Ranked {
		t.Errorf("wiretest Ranked = true, want false (zero-token outcome)")
	}
	if d.FlaggedOutcomes != 1 {
		t.Errorf("wiretest flagged_outcomes = %d, want 1", d.FlaggedOutcomes)
	}
	if sr.DataQuality == nil {
		t.Fatalf("data_quality block absent before tokens recorded")
	}
	foundFlag := false
	for _, z := range sr.DataQuality.ZeroTokenOutcomes {
		if z.Developer == "wiretest" && z.IssueID == "issue-999" {
			foundFlag = true
		}
	}
	if !foundFlag {
		t.Errorf("data_quality missing {wiretest, issue-999}; got %+v", sr.DataQuality)
	}

	// Now record tokens for that issue INSIDE the pre-merge window (ts an hour
	// before merge — capture normally lands before the PR merges). The flag must
	// clear on re-query.
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: "wiretest", IssueID: "issue-999", Model: "claude-sonnet-4",
		InputTok: 5000, Source: "jsonl", Fidelity: "realtime",
		Timestamp: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}

	sr = getScores(t, srv.URL)
	d, ok = findDevOpt(sr, "wiretest")
	if !ok {
		t.Fatalf("wiretest absent from /scores after tokens; got %+v", sr.Developers)
	}
	if d.FlaggedOutcomes != 0 {
		t.Errorf("wiretest flagged_outcomes = %d after 5K tokens, want 0 (flag cleared)", d.FlaggedOutcomes)
	}
	if _, present := findDevOpt(sr, "wiretest"); present {
		if sr.DataQuality != nil {
			for _, z := range sr.DataQuality.ZeroTokenOutcomes {
				if z.Developer == "wiretest" && z.IssueID == "issue-999" {
					t.Errorf("data_quality still lists {wiretest, issue-999} after tokens recorded")
				}
			}
		}
	}
}
