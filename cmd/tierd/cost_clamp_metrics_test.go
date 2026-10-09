package main

import (
	"context"
	"github.com/tiermetric/tier/internal/store"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func TestCostClampCounterWiring(t *testing.T) {
	sm := newServeMetrics("test")
	store.SetCostClampRecorder(sm.costClamps)
	defer store.SetCostClampRecorder(nil)
	db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for i, cost := range []int64{store.MaxTokenEventCostMicro + 1, 1} {
		if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{Developer: "alice", IssueID: "1082", Model: "m", Source: "proxy", Fidelity: "realtime", InputTok: i + 1, CostMicro: cost}); err != nil {
			t.Fatal(err)
		}
	}
	var out strings.Builder
	sm.reg.Render(&out)
	if !strings.Contains(out.String(), "tier_token_event_cost_clamps_total 1\n") {
		t.Fatalf("metrics: %s", out.String())
	}
}

// float64(MaxInt64) is 2^63. Bound both monitor inputs before converting them
// back to int64 so a saturated ComputeCost result cannot suppress the warning.
func TestCostRecorder_SaturatedCostKeepsFallbackWarning(t *testing.T) {
	sm := newServeMetrics("test")
	for _, rec := range []*costRecorder{sm.unknownModelCost, sm.pricedCost} {
		rec.Add(float64(math.MaxInt64))
		if got := rec.Total(); got != store.MaxTokenEventCostMicro {
			t.Fatalf("saturated total = %d, want %d", got, store.MaxTokenEventCostMicro)
		}
	}
	logger, out := warnCapture()
	if !checkUnknownCostShare(sm.unknownModelCost.Total(), sm.pricedCost.Total(), logger) || !strings.Contains(out.String(), "unknown-model fallback exceeds threshold") {
		t.Fatalf("saturated fallback must warn: %s", out.String())
	}
	var rendered strings.Builder
	sm.reg.Render(&rendered)
	for _, name := range []string{"tier_priced_cost_micro_total", "tier_unknown_model_cost_micro_total"} {
		if !strings.Contains(rendered.String(), name+" 10000000000\n") {
			t.Fatalf("bounded metric missing: %s", rendered.String())
		}
	}
}
