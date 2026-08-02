//go:build integration

// End-to-end wire test for the #139 OpenAI Usage/Costs poller leg: an httptest
// OpenAI Usage/Costs API → the REAL openaiusage.Poller → the real ingester →
// real SQLite → GET /api/v1/org_actual_spend + GET /api/v1/scores. The twin of
// anthropic_admin_test.go. It proves the OpenAI-specific correctness points
// across the true package boundary: (1) input_tokens' cached subset is carved
// out (never double-counted); (2) a per-request event already captured by the
// proxy is NOT re-counted — only the REMAINDER lands under "unattributed"; (3)
// the cost report feeds org_actual_spend via a source-scoped idempotent delta
// that does NOT cannibalize a seeded anthropic-admin or manual row; (4) the #42
// GET reads back the SUMMED cross-source org total.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/collector/openaiusage"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/store"
)

// openaiFixedNow pins the poller's settlement window: 2026-06-15 (ending
// 2026-06-16T00:00Z) is >24h before this, so it is settled.
var openaiFixedNow = time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)

// newOpenAIAPIServer serves fixed usage + cost report JSON for the settled day
// 2026-06-15 with the REAL OpenAI wire shape: unix-second bucket bounds, an
// input_tokens INCLUSIVE of a 200-token cached subset, and a nested
// dollars-denominated cost amount ($100).
func newOpenAIAPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	dayStart := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC).Unix()
	dayEnd := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC).Unix()
	// input_tokens=1000 INCLUDES input_cached_tokens=200 (the carve-out target).
	usageJSON := fmt.Sprintf(`{"data":[{"start_time":%d,"end_time":%d,"results":[{"model":"gpt-4o","input_tokens":1000,"input_cached_tokens":200,"output_tokens":500}]}],"has_more":false,"next_page":null}`, dayStart, dayEnd)
	// amount.value is in DOLLARS ($100), not cents.
	costJSON := fmt.Sprintf(`{"data":[{"start_time":%d,"end_time":%d,"results":[{"amount":{"value":100.0,"currency":"usd"}}]}],"has_more":false,"next_page":null}`, dayStart, dayEnd)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-openai-admin-test" {
			t.Errorf("Authorization header = %q, want Bearer sk-openai-admin-test", got)
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/organization/usage/completions"):
			_, _ = w.Write([]byte(usageJSON))
		case strings.HasPrefix(r.URL.Path, "/v1/organization/costs"):
			_, _ = w.Write([]byte(costJSON))
		default:
			t.Errorf("unexpected OpenAI API path %s", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
}

func TestWire_OpenAIUsagePollerRemainderAndReadBack(t *testing.T) {
	srv, db := newServerWithStore(t)
	ctx := context.Background()

	// Seed the ALREADY-CAPTURED baseline: a proxy event for alice on the settled
	// day (gpt-4o, 400 input / 100 output / 50 cache-read, in CARVED classes —
	// how the proxy parser stores OpenAI events). This is spend the poller must
	// NOT re-count.
	capturedUsage := store.CostUsage{Input: 400, Output: 100, CacheRead: 50}
	capturedCost := store.ComputeCost("gpt-4o", capturedUsage)
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer: "alice", IssueID: "issue-1", Model: "gpt-4o",
		InputTok: capturedUsage.Input, OutputTok: capturedUsage.Output, CacheRead: capturedUsage.CacheRead,
		CostMicro: capturedCost, Source: collector.SourceProxy, Fidelity: collector.FidelityRealtime,
		Timestamp: time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed captured event: %v", err)
	}

	// Seed TWO other-source org_actual_spend rows for the same (org, period) that
	// the OpenAI poller must NOT cannibalize (R1): an anthropic-admin $30 and a
	// manual $40.
	if err := db.InsertOrgActualSpend(ctx, store.OrgActualSpend{
		Org: "acme", Period: "2026-06", Source: collector.SourceAnthropicAdmin,
		ActualPaidMicro: store.DollarsToMicro(30), Timestamp: openaiFixedNow,
	}); err != nil {
		t.Fatalf("seed anthropic-admin org spend: %v", err)
	}
	if err := db.InsertOrgActualSpend(ctx, store.OrgActualSpend{
		Org: "acme", Period: "2026-06", Source: store.OrgSpendSourceManual,
		ActualPaidMicro: store.DollarsToMicro(40), Timestamp: openaiFixedNow,
	}); err != nil {
		t.Fatalf("seed manual org spend: %v", err)
	}

	// Expected remainder: admin carved (Input 1000−200=800, CacheRead 200, Output
	// 500) − captured (400/100/cr50) = Input 400, Output 400, CacheRead 150.
	remainderUsage := store.CostUsage{Input: 400, Output: 400, CacheRead: 150}
	remainderCost := store.ComputeCost("gpt-4o", remainderUsage)
	adminFullCost := store.ComputeCost("gpt-4o", store.CostUsage{Input: 800, CacheRead: 200, Output: 500})
	if remainderCost >= adminFullCost {
		t.Fatalf("test arithmetic broken: remainder cost %d should be < full admin cost %d", remainderCost, adminFullCost)
	}

	openaiAPI := newOpenAIAPIServer(t)
	defer openaiAPI.Close()
	client := openaiusage.NewClient(openaiusage.ClientConfig{
		APIKey:  "sk-openai-admin-test",
		BaseURL: openaiAPI.URL,
		Sleep:   func(c context.Context, _ time.Duration) error { return c.Err() },
		Logger:  slog.New(slog.NewTextHandler(quietWriter{}, nil)),
	})
	poller := openaiusage.NewPoller(openaiusage.PollerConfig{
		Client:   client,
		Store:    db,
		Org:      "acme",
		Interval: time.Hour, // long: only the immediate first pass runs in-test
		Logger:   slog.New(slog.NewTextHandler(quietWriter{}, nil)),
		Now:      func() time.Time { return openaiFixedNow },
	})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = poller.Run(runCtx, time.Time{}, ingester.Store(db)) }()

	// Await the unattributed remainder row landing in the real store.
	if !waitFor(func() bool {
		c, ok := developerCostMicro(t, db, collector.UnattributedIssueID)
		return ok && c > 0
	}, 5*time.Second, 10*time.Millisecond) {
		t.Fatalf("no unattributed remainder row within 5s — poller did not flow through the wire")
	}
	cancel()

	// Double-count proof #1: unattributed carries the carved REMAINDER cost, not
	// the full org aggregate — this also proves the cached subset was carved out.
	gotUnattributed, _ := developerCostMicro(t, db, collector.UnattributedIssueID)
	if gotUnattributed != remainderCost {
		t.Fatalf("unattributed cost = %d micro, want %d (carved remainder). Full-admin would be %d — a mismatch means double-counting or a bad cache carve-out",
			gotUnattributed, remainderCost, adminFullCost)
	}

	// Double-count proof #2: alice's captured event is untouched by the poll.
	gotAlice, _ := developerCostMicro(t, db, "alice")
	if gotAlice != capturedCost {
		t.Fatalf("alice captured cost = %d micro, want %d (poll must not alter captured rows)", gotAlice, capturedCost)
	}

	// Cost-report reconciliation, source-scoped (R1): $100 reported, $0 recorded
	// for openai-usage → one +$100 delta under that source. The seeded
	// anthropic-admin ($30) and manual ($40) rows must be untouched.
	openaiNet, err := db.OrgActualSpendNet(ctx, "acme", "2026-06", collector.SourceOpenAIUsage)
	if err != nil {
		t.Fatalf("OrgActualSpendNet(openai-usage): %v", err)
	}
	if openaiNet != store.DollarsToMicro(100) {
		t.Fatalf("openai-usage net = %d micro, want %d ($100 from the cost report)", openaiNet, store.DollarsToMicro(100))
	}
	adminNet, _ := db.OrgActualSpendNet(ctx, "acme", "2026-06", collector.SourceAnthropicAdmin)
	if adminNet != store.DollarsToMicro(30) {
		t.Fatalf("anthropic-admin net = %d micro, want %d (must be untouched — R1)", adminNet, store.DollarsToMicro(30))
	}
	manualNet, _ := db.OrgActualSpendNet(ctx, "acme", "2026-06", store.OrgSpendSourceManual)
	if manualNet != store.DollarsToMicro(40) {
		t.Fatalf("manual net = %d micro, want %d (must be untouched — R1)", manualNet, store.DollarsToMicro(40))
	}

	// #42 read-back over HTTP: the GET SUMs across ALL sources → $100 + $30 + $40
	// = $170 net, entries=3 (one row per source).
	got := getOrgActualSpend(t, srv.URL, "acme")
	if len(got.Orgs) != 1 {
		t.Fatalf("GET org_actual_spend returned %d orgs, want 1", len(got.Orgs))
	}
	if got.Orgs[0].ActualPaidUSD != 170.0 {
		t.Fatalf("GET summed total = %v, want 170 ($100 openai + $30 anthropic + $40 manual)", got.Orgs[0].ActualPaidUSD)
	}
	if got.Orgs[0].Entries != 3 {
		t.Fatalf("GET entries = %d, want 3 (one row per source)", got.Orgs[0].Entries)
	}
}

// getOrgActualSpend GETs the #42 read-back with the bearer token and decodes it.
func getOrgActualSpend(t *testing.T, url, org string) orgActualSpendWire {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url+"/api/v1/org_actual_spend?org="+org, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /org_actual_spend: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /org_actual_spend: status %d; body=%s", resp.StatusCode, b)
	}
	var out orgActualSpendWire
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}
