package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/metrics"
	"github.com/tiermetric/tier/internal/store"
)

// TestEvalZeroOutcome pins the pure tripwire predicate: cost accrued in the window
// AND zero outcomes recorded there (#189). Every other combination is clear.
func TestEvalZeroOutcome(t *testing.T) {
	cases := []struct {
		name string
		act  store.WindowActivity
		want bool
	}{
		{"cost and no outcomes -> tripped", store.WindowActivity{CostMicro: 500_000, Outcomes: 0}, true},
		{"cost and outcomes -> clear", store.WindowActivity{CostMicro: 500_000, Outcomes: 3}, false},
		{"no cost and no outcomes -> clear (fresh/idle)", store.WindowActivity{CostMicro: 0, Outcomes: 0}, false},
		{"no cost but outcomes -> clear", store.WindowActivity{CostMicro: 0, Outcomes: 2}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := evalZeroOutcome(c.act); got != c.want {
				t.Errorf("evalZeroOutcome(%+v) = %v, want %v", c.act, got, c.want)
			}
		})
	}
}

// fakeActivityStore returns a canned WindowActivity (or error) for the tripwire.
type fakeActivityStore struct {
	act store.WindowActivity
	err error
}

func (f fakeActivityStore) WindowActivity(context.Context, time.Time) (store.WindowActivity, error) {
	return f.act, f.err
}

// TestCheckZeroOutcome_Tripped: the tripwire WARNs and sets the gauge to 1 when
// cost accrued with no outcomes, carrying the window and the accrued dollar figure.
func TestCheckZeroOutcome_Tripped(t *testing.T) {
	logger, buf := warnCapture()
	reg := metrics.NewRegistry()
	gauge := reg.NewGauge("tier_zero_outcome_tripwire", "test")

	st := fakeActivityStore{act: store.WindowActivity{CostMicro: 1_500_000, Outcomes: 0}}
	if !checkZeroOutcome(context.Background(), 7*24*time.Hour, st, gauge, logger) {
		t.Fatal("checkZeroOutcome returned false, want true (cost accrued, zero outcomes)")
	}
	out := buf.String()
	for _, want := range []string{"zero-outcome tripwire", "window_days=7", "cost_usd=1.5"} {
		if !strings.Contains(out, want) {
			t.Errorf("WARN log missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(renderGauge(reg), "tier_zero_outcome_tripwire 1") {
		t.Errorf("gauge not set to 1 when tripped:\n%s", renderGauge(reg))
	}
}

// TestCheckZeroOutcome_Clear: when outcomes exist the check is silent and the gauge
// is 0.
func TestCheckZeroOutcome_Clear(t *testing.T) {
	logger, buf := warnCapture()
	reg := metrics.NewRegistry()
	gauge := reg.NewGauge("tier_zero_outcome_tripwire", "test")

	st := fakeActivityStore{act: store.WindowActivity{CostMicro: 1_000_000, Outcomes: 4}}
	if checkZeroOutcome(context.Background(), 7*24*time.Hour, st, gauge, logger) {
		t.Fatal("checkZeroOutcome returned true, want false (outcomes present)")
	}
	if buf.Len() != 0 {
		t.Errorf("expected no WARN when clear, got: %q", buf.String())
	}
	if !strings.Contains(renderGauge(reg), "tier_zero_outcome_tripwire 0") {
		t.Errorf("gauge not set to 0 when clear:\n%s", renderGauge(reg))
	}
}

// TestCheckZeroOutcome_QueryError: a store error is logged and treated as
// not-tripped, and it must NOT flap the gauge (last known value is kept).
func TestCheckZeroOutcome_QueryError(t *testing.T) {
	logger, buf := warnCapture()
	reg := metrics.NewRegistry()
	gauge := reg.NewGauge("tier_zero_outcome_tripwire", "test")
	gauge.Set(1) // pretend a prior tick tripped

	st := fakeActivityStore{err: errors.New("db down")}
	if checkZeroOutcome(context.Background(), 7*24*time.Hour, st, gauge, logger) {
		t.Fatal("checkZeroOutcome returned true on query error, want false")
	}
	if !strings.Contains(buf.String(), "query failed") {
		t.Errorf("expected a query-failure WARN, got: %q", buf.String())
	}
	if !strings.Contains(renderGauge(reg), "tier_zero_outcome_tripwire 1") {
		t.Errorf("gauge should keep its prior value on query error:\n%s", renderGauge(reg))
	}
}

// TestNewServeMetrics_RegistersTripwireGauge pins the gauge under its exact name so
// /metrics exposes it (the wire contract).
func TestNewServeMetrics_RegistersTripwireGauge(t *testing.T) {
	sm := newServeMetrics("v1.2.3")
	sm.zeroOutcomeTripwire.Set(0)
	var sb strings.Builder
	sm.reg.Render(&sb)
	if !strings.Contains(sb.String(), "tier_zero_outcome_tripwire 0") {
		t.Errorf("serve metric set missing tier_zero_outcome_tripwire:\n%s", sb.String())
	}
}

func renderGauge(reg *metrics.Registry) string {
	var sb strings.Builder
	reg.Render(&sb)
	return sb.String()
}
