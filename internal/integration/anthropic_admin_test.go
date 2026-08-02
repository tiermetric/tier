//go:build integration

// End-to-end wire test for the #138 Anthropic Admin poller leg: an httptest
// Admin API (usage + cost reports) → the REAL anthropicadmin.Poller → the real
// ingester → real SQLite → GET /api/v1/scores. It is the only test that exercises
// the poller's double-count-safety contract across the true package boundary: a
// per-request event already captured by the proxy path must NOT be re-counted when
// the org-level aggregate reports the same day — only the REMAINDER lands, under
// the "unattributed" sentinel.
package integration

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/collector/anthropicadmin"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/store"
)

// adminFixedNow pins the poller's settlement window: 2026-06-15 (ending
// 2026-06-16T00:00Z) is >24h before this, so it is settled.
var adminFixedNow = time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)

// newAdminAPIServer serves fixed usage + cost report JSON for the settled day
// 2026-06-15. Usage reports 1000 input / 500 output for claude-sonnet-4; cost
// reports 10000 cents ($100).
func newAdminAPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	const usageJSON = `{"data":[{"starting_at":"2026-06-15T00:00:00Z","ending_at":"2026-06-16T00:00:00Z","results":[{"model":"claude-sonnet-4","uncached_input_tokens":1000,"output_tokens":500,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}}]}],"has_more":false,"next_page":""}`
	const costJSON = `{"data":[{"starting_at":"2026-06-15T00:00:00Z","ending_at":"2026-06-16T00:00:00Z","results":[{"amount":"10000","currency":"USD"}]}],"has_more":false,"next_page":""}`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/organizations/usage_report/messages"):
			_, _ = w.Write([]byte(usageJSON))
		case strings.HasPrefix(r.URL.Path, "/v1/organizations/cost_report"):
			_, _ = w.Write([]byte(costJSON))
		default:
			t.Errorf("unexpected admin API path %s", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
}

// TestWire_AnthropicAdminPollerRemainderNoDoubleCount drives the real poller Run
// loop (one immediate pass) and proves: (1) a settled-day remainder lands as an
// "unattributed" developer priced at the REMAINDER, not the full org aggregate;
// (2) the already-captured proxy event is untouched; (3) the cost report feeds
// org_actual_spend via an idempotent delta.
func TestWire_AnthropicAdminPollerRemainderNoDoubleCount(t *testing.T) {
	srv, db := newServerWithStore(t)
	ctx := context.Background()

	// Seed the ALREADY-CAPTURED baseline: a proxy event for alice on the settled
	// day (claude-sonnet-4, 400 input / 100 output). This is the spend the poller
	// must NOT re-count.
	capturedUsage := store.CostUsage{Input: 400, Output: 100}
	capturedCost := store.ComputeCost("claude-sonnet-4", capturedUsage)
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer: "alice", IssueID: "issue-1", Model: "claude-sonnet-4",
		InputTok: capturedUsage.Input, OutputTok: capturedUsage.Output,
		CostMicro: capturedCost, Source: collector.SourceProxy, Fidelity: collector.FidelityRealtime,
		Timestamp: time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed captured event: %v", err)
	}

	// Seed an OTHER-provider (manual) org_actual_spend row for the same (org,
	// period): $40 that the Anthropic poller must NOT cannibalize (#138 R1).
	if err := db.InsertOrgActualSpend(ctx, store.OrgActualSpend{
		Org: "acme", Period: "2026-06", Source: store.OrgSpendSourceManual,
		ActualPaidMicro: store.DollarsToMicro(40), Timestamp: adminFixedNow,
	}); err != nil {
		t.Fatalf("seed manual org spend: %v", err)
	}

	// Expected remainder = admin(1000/500) − captured(400/100) = 600/400.
	remainderUsage := store.CostUsage{Input: 600, Output: 400}
	remainderCost := store.ComputeCost("claude-sonnet-4", remainderUsage)
	adminFullCost := store.ComputeCost("claude-sonnet-4", store.CostUsage{Input: 1000, Output: 500})
	if remainderCost >= adminFullCost {
		t.Fatalf("test arithmetic broken: remainder cost %d should be < full admin cost %d", remainderCost, adminFullCost)
	}

	// Real poller against the httptest Admin API, fixed clock so 2026-06-15 settles.
	adminAPI := newAdminAPIServer(t)
	defer adminAPI.Close()
	client := anthropicadmin.NewClient(anthropicadmin.ClientConfig{
		APIKey:  "sk-ant-admin-test",
		BaseURL: adminAPI.URL,
		Sleep:   func(c context.Context, _ time.Duration) error { return c.Err() },
		Logger:  slog.New(slog.NewTextHandler(quietWriter{}, nil)),
	})
	poller := anthropicadmin.NewPoller(anthropicadmin.PollerConfig{
		Client:   client,
		Store:    db,
		Org:      "acme",
		Interval: time.Hour, // long: only the immediate first pass runs in-test
		Logger:   slog.New(slog.NewTextHandler(quietWriter{}, nil)),
		Now:      func() time.Time { return adminFixedNow },
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

	// Double-count proof #1: unattributed carries the REMAINDER cost, not the full
	// org aggregate.
	gotUnattributed, _ := developerCostMicro(t, db, collector.UnattributedIssueID)
	if gotUnattributed != remainderCost {
		t.Fatalf("unattributed cost = %d micro, want %d (remainder). Full-admin would be %d — a mismatch there means the poller double-counted captured spend",
			gotUnattributed, remainderCost, adminFullCost)
	}

	// Double-count proof #2: alice's captured event is untouched by the poll.
	gotAlice, _ := developerCostMicro(t, db, "alice")
	if gotAlice != capturedCost {
		t.Fatalf("alice captured cost = %d micro, want %d (poll must not alter captured rows)", gotAlice, capturedCost)
	}

	// Assert over HTTP — the public end of the wire.
	sr := getScores(t, srv.URL)
	unattributed := devByName(t, sr, collector.UnattributedIssueID)
	if want := store.MicroToDollars(remainderCost); unattributed.TotalCostUSD != want {
		t.Fatalf("scores unattributed total_cost_usd = %v, want %v (remainder)", unattributed.TotalCostUSD, want)
	}

	// Cost-report reconciliation, source-scoped (#138 R1): $100 reported, $0 recorded
	// for anthropic-admin → one +$100 delta under that source. The pre-seeded $40
	// other-provider (manual) row for the same (org, period) must be untouched.
	adminNet, err := db.OrgActualSpendNet(ctx, "acme", "2026-06", collector.SourceAnthropicAdmin)
	if err != nil {
		t.Fatalf("OrgActualSpendNet(anthropic-admin): %v", err)
	}
	if adminNet != store.DollarsToMicro(100) {
		t.Fatalf("anthropic-admin net = %d micro, want %d ($100 from the cost report)", adminNet, store.DollarsToMicro(100))
	}
	manualNet, err := db.OrgActualSpendNet(ctx, "acme", "2026-06", store.OrgSpendSourceManual)
	if err != nil {
		t.Fatalf("OrgActualSpendNet(manual): %v", err)
	}
	if manualNet != store.DollarsToMicro(40) {
		t.Fatalf("manual net = %d micro, want %d (other-provider row must be untouched — R1)", manualNet, store.DollarsToMicro(40))
	}
}

// quietWriter discards poller log output so a passing run stays clean.
type quietWriter struct{}

func (quietWriter) Write(p []byte) (int, error) { return len(p), nil }
