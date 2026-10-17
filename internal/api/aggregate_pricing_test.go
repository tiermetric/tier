package api

import (
	"encoding/json"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAggregatePricingDisclosureScoresCompareExport(t *testing.T) {
	h, db := newTestHandler(t)
	ts := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	cost, mode := store.ComputeAggregateCostHost("", "claude-haiku-5-5", store.CostUsage{Input: 120000})
	if err := db.InsertTokenEvent(t.Context(), store.TokenEvent{Developer: "alice", Repo: "acme/app", Model: "claude-haiku-5-5", Source: "anthropic-admin", InputTok: 120000, CostMicro: cost, BillingMode: mode, Timestamp: ts, PricingBasis: store.PricingAggregateApprox, TokenDerived: true}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/api/v1/scores?since=2026-06-01&until=2026-07-01", "/api/v1/scores/compare?since_a=2026-06-01&until_a=2026-07-01&since_b=2026-07-01&until_b=2026-08-01"} {
		rec := doExport(t, h, target, nil)
		var body map[string]any
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", target, rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if window, ok := body["window_a"].(map[string]any); ok {
			body = window
		}
		dq, _ := body["data_quality"].(map[string]any)
		ap, _ := dq["aggregate_pricing"].(map[string]any)
		if ap["approximate_rows"] != float64(1) || ap["possible_increase_micro"] != float64(48000) {
			t.Errorf("%s missing approximation and original-rate ceiling: %s", target, rec.Body.String())
		}
	}
	rec := doExport(t, h, "/api/v1/events?since=2026-06-01&until=2026-07-01", nil)
	var body struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 1 || body.Events[0]["pricing_basis"] != store.PricingAggregateApprox || body.Events[0]["rate_snapshot"] == "" || body.Events[0]["rate_snapshot"] == nil {
		t.Fatalf("export lacks resolvable basis: %s", rec.Body.String())
	}
	h.aggregation = scoring.AggregationTeam
	rec = doExport(t, h, "/api/v1/scores?since=2026-06-01&until=2026-07-01", nil)
	var anonymous map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &anonymous)
	dq, _ := anonymous["data_quality"].(map[string]any)
	ap, _ := dq["aggregate_pricing"].(map[string]any)
	if ap["approximate_rows"] != nil || ap["possible_increase_micro"] != nil || ap["ceiling_status"] != "not_shown" {
		t.Fatalf("anonymous disclosure leaked population or omitted static notice: %s", rec.Body.String())
	}
}

func TestAggregatePricingPolicyCoversLegacyBodiesAndOtherReadViews(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, target := range []string{"/api/v1/events?since=2026-06-01", "/api/v1/scores?since=2026-06-01"} {
		rec := doExport(t, h, target, nil)
		if !strings.Contains(rec.Header().Get("X-TIER-Aggregate-Pricing"), "may understate") {
			t.Errorf("%s lacks a policy disclosure", target)
		}
	}
	// The shared read wrapper must disclose policy without rewriting the bytes of
	// a previously sealed response that predates aggregate_pricing metadata.
	legacy := `{"data_quality":{"window_predates_cost_capture":false},"total":{"total_cost_usd":1}}`
	rec := httptest.NewRecorder()
	h.requireRead(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(legacy)) })(rec, httptest.NewRequest(http.MethodGet, "/api/v1/scores?period=2026-06", nil))
	if rec.Body.String() != legacy || !strings.Contains(rec.Header().Get("X-TIER-Aggregate-Pricing"), "Historical provider aggregates") {
		t.Fatalf("legacy bytes or disclosure lost: %s header=%q", rec.Body.String(), rec.Header().Get("X-TIER-Aggregate-Pricing"))
	}
}
