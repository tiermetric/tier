package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/metrics"
)

// warnCapture returns a WARN-level text logger writing into a buffer, so a test
// can assert on the emitted WARN without a ticker or a sleep.
func warnCapture() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})), &buf
}

// TestUnknownShareCheck_WarnsAbove5Percent: an interval whose unknown-model
// fallback cost is a >5% share of total priced cost logs the WARN, carrying both
// dollar figures (#135). checkUnknownCostShare is called directly with synthetic
// counter deltas — no ticker, no clock.
func TestUnknownShareCheck_WarnsAbove5Percent(t *testing.T) {
	logger, buf := warnCapture()
	// 60_000 / 1_000_000 = 6% > 5% threshold.
	if !checkUnknownCostShare(60_000, 1_000_000, logger) {
		t.Fatal("checkUnknownCostShare(6% share) returned false, want true (should warn)")
	}
	out := buf.String()
	for _, want := range []string{
		"unknown-model fallback exceeds threshold",
		"add the missing models to prices.yaml",
		// $0.06 unknown micro-dollars → dollars. (total_usd is deliberately not
		// asserted on an exact string — slog's float formatting of a whole
		// dollar amount is a cosmetic detail, not behavior worth pinning.)
		"unknown_usd=0.06",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("WARN log missing %q:\n%s", want, out)
		}
	}
}

// TestUnknownShareCheck_SilentBelowThreshold: at or below 5%, and for degenerate
// zero-delta intervals, the check is silent (no WARN, returns false).
func TestUnknownShareCheck_SilentBelowThreshold(t *testing.T) {
	cases := []struct {
		name           string
		unknown, total int64
	}{
		{"below threshold (4%)", 40_000, 1_000_000},
		{"exactly at threshold (5%, strict >)", 50_000, 1_000_000},
		{"no fallback spend this interval", 0, 1_000_000},
		{"no priced spend this interval", 60_000, 0},
		{"negative unknown delta (counter reset guard)", -1, 1_000_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logger, buf := warnCapture()
			if checkUnknownCostShare(c.unknown, c.total, logger) {
				t.Errorf("checkUnknownCostShare(%d, %d) returned true, want false (silent)", c.unknown, c.total)
			}
			if buf.Len() != 0 {
				t.Errorf("expected no WARN for (%d, %d), got: %q", c.unknown, c.total, buf.String())
			}
		})
	}
}

// TestCostRecorder_AddAndTotal covers the costRecorder adapter that fronts the
// two #135 counters: Add must (a) record into the exported CounterVec under its
// exact metric name, so /metrics exposes it, and (b) accumulate the running
// int64 micro-dollar Total() the serve-path share check reads. This closes the
// gap the pure checkUnknownCostShare tests leave — the delta math in
// runUnknownCostShareMonitor is just Total()-minus-previous, so a correct
// accumulator is what makes it correct.
func TestCostRecorder_AddAndTotal(t *testing.T) {
	reg := metrics.NewRegistry()
	rec := &costRecorder{counter: reg.NewCounter("tier_priced_cost_micro_total", "test")}

	if rec.Total() != 0 {
		t.Fatalf("fresh costRecorder Total() = %d, want 0", rec.Total())
	}
	rec.Add(500_000) // one $0.50 fallback event, in micro-dollars
	rec.Add(390_135) // one priced event
	if got := rec.Total(); got != 890_135 {
		t.Errorf("costRecorder Total() = %d, want 890_135 (running micro-dollar sum)", got)
	}

	// The same values must be exposed on /metrics under the exact counter name.
	var sb strings.Builder
	reg.Render(&sb)
	if want := "tier_priced_cost_micro_total 890135"; !strings.Contains(sb.String(), want) {
		t.Errorf("rendered metrics missing %q:\n%s", want, sb.String())
	}
}

// TestNewServeMetrics_RegistersCostCounters pins that the two #135 cost counters
// are registered under their exact metric names (the wire-verification contract,
// hardened as a unit test). Their series render only once non-zero, so assert on
// the HELP header the registry always emits.
func TestNewServeMetrics_RegistersCostCounters(t *testing.T) {
	sm := newServeMetrics("v1.2.3")
	sm.unknownModelCost.Add(1)
	sm.pricedCost.Add(1)
	var sb strings.Builder
	sm.reg.Render(&sb)
	out := sb.String()
	for _, want := range []string{
		"tier_unknown_model_cost_micro_total 1",
		"tier_priced_cost_micro_total 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("serve metric set missing %q:\n%s", want, out)
		}
	}
}
