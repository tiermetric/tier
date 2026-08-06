//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// Subscription route under test (#113): Ollama Cloud bills a flat monthly fee,
// so its tokens have no per-token list price of their own. the maintainer's 2026-07-02
// ruling — the list-rate INVERSION — values them at a comparable peer's list
// rate in the TIER denominator and routes the FEE to actual-paid only.
const (
	glmHost  = "ollama.com"
	glmModel = "glm-5.2"
	glmOrg   = "acme"
)

// subscriptionPricesYAML uses ROUND comparable rates ($1/M in, $3/M out) so the
// end-to-end arithmetic below is exact rather than approximately right: 500M in
// + 500M out = $2,000 of list value. The recommended real-world rates are
// pinned at micro precision by the unit tests in internal/store; here the point
// is the pipeline, and a clean number makes a wrong one unmistakable.
const subscriptionPricesYAML = `
version: 1
effective_date: "2026-08-01"
models:
  "glm-5.2@ollama.com": { input_per_m: 1.00, output_per_m: 3.00, provider: self-hosted, billing_mode: subscription }
  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}
  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}
  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}
`

// loadSubscriptionPrices installs subscriptionPricesYAML as the process-wide
// active price table for the calling test and restores the embedded default
// afterwards. The table is a write-once-before-serve global, so this must not
// run in parallel with another test that prices events — none of the
// integration tests do.
func loadSubscriptionPrices(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prices.yaml")
	if err := os.WriteFile(path, []byte(subscriptionPricesYAML), 0o600); err != nil {
		t.Fatalf("write price table: %v", err)
	}
	if _, err := store.LoadPriceTable(path); err != nil {
		t.Fatalf("LoadPriceTable: %v", err)
	}
	t.Cleanup(func() {
		if _, err := store.LoadPriceTable(filepath.Join("..", "store", "prices.yaml")); err != nil {
			t.Fatalf("restore embedded price table: %v", err)
		}
	})
}

// insertGLMSpend prices one Ollama Cloud call through ComputeCostHost — the
// exact path the proxy uses at ingest — and persists it with the resolved
// billing_mode, so the stored row carries the honest basis rather than passing
// as a canonical $/M.
func insertGLMSpend(t *testing.T, db *store.DB, inputTok, outputTok int) {
	t.Helper()
	cost, mode := store.ComputeCostHost(glmHost, glmModel, store.CostUsage{Input: inputTok, Output: outputTok})
	if mode != store.BillingSubscription {
		t.Fatalf("ingest resolved billing_mode %q, want %q — the route is not being priced as a subscription", mode, store.BillingSubscription)
	}
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer:   "alice",
		IssueID:     "issue-9",
		Model:       glmModel,
		InputTok:    inputTok,
		OutputTok:   outputTok,
		CostMicro:   cost,
		Source:      collector.SourceProxy,
		Fidelity:    collector.FidelityRealtime,
		Host:        glmHost,
		BillingMode: mode,
		Timestamp:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}
}

// TestSubscriptionOnlyDeveloper_HasNonZeroTIER pins the property the inversion
// exists to produce, over real HTTP: a developer whose ONLY spend rides a
// flat-fee route still has a REAL denominator — comparable-priced dollars — so
// their TIER is computable and non-zero.
//
// Both rejected alternatives fail here. Pricing subscription tokens at $0 makes
// the denominator zero and TIER undefined; the superseded amortized-divisor
// design makes it depend on the reporting window and on colleagues' usage, so
// this same session would score differently for two different `?since=`.
func TestSubscriptionOnlyDeveloper_HasNonZeroTIER(t *testing.T) {
	loadSubscriptionPrices(t)
	srv, db := newServerWithStore(t)

	// 500M in ($500) + 500M out ($1,500) at the comparables = $2,000 list.
	insertGLMSpend(t, db, 500_000_000, 500_000_000)
	inserted, err := db.InsertOutcome(context.Background(), store.Outcome{
		Developer: "alice", IssueID: "issue-9", Weight: 3, Quality: 1.0,
		Timestamp: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}
	if !inserted {
		t.Fatal("outcome was deduped away — the TIER numerator would be 0 for the wrong reason")
	}

	a := alice(t, getScores(t, srv.URL))
	if !approxEq(a.TotalCostUSD, 2000.0) {
		t.Errorf("total cost = %.4f, want 2000.0000 (comparable-priced subscription tokens)", a.TotalCostUSD)
	}
	// TIER = 3 weighted points / ($2,000 / 1000) = 1.5 — non-zero BECAUSE the
	// denominator is a real list valuation rather than a $0 hack.
	if !approxEq(a.TIER, 1.5) {
		t.Errorf("TIER = %.4f, want 1.5000", a.TIER)
	}
}

// TestLeverage_SubscriptionFeeAsActualPaid is the fee leg end to end, and the
// CFO-facing point of the whole ruling: $2,000 of list value delivered for the
// $100 the org actually paid = 20x Spend Leverage.
//
// It also runs the reconcile TWICE. Idempotency is not a store-level curiosity
// here — a double-post would halve the leverage number an executive reads, so
// the second call is asserted on the WIRE, not just in SQL.
func TestLeverage_SubscriptionFeeAsActualPaid(t *testing.T) {
	loadSubscriptionPrices(t)
	srv, db := newServerWithStore(t)
	ctx := context.Background()

	insertGLMSpend(t, db, 500_000_000, 500_000_000) // $2,000 list

	// alice is the org's only seat, so the whole org-level fee allocates to her
	// under the existing two-tier actual-spend resolution (#23/#41).
	if err := db.UpsertHierarchy(ctx, "alice", "core", "", glmOrg); err != nil {
		t.Fatalf("UpsertHierarchy: %v", err)
	}

	period := store.CurrentPeriod(time.Now())
	const feeMicro = int64(100_000_000) // $100.00/month, the dogfood Max plan
	delta, err := db.ReconcileSubscriptionFee(ctx, "glm-5.2@ollama.com", glmOrg, period, feeMicro)
	if err != nil {
		t.Fatalf("ReconcileSubscriptionFee: %v", err)
	}
	if delta != feeMicro {
		t.Fatalf("first fee delta = %d micro, want %d", delta, feeMicro)
	}
	// A restart re-runs the startup pass. It must post nothing.
	if delta, err = db.ReconcileSubscriptionFee(ctx, "glm-5.2@ollama.com", glmOrg, period, feeMicro); err != nil || delta != 0 {
		t.Fatalf("second fee delta = (%d, %v), want (0, nil) — a restart must not re-bill the period", delta, err)
	}

	a := alice(t, getScores(t, srv.URL))
	if !approxEq(a.ActualPaidUSD, 100.0) {
		t.Errorf("actual paid = %.4f, want 100.0000 (the flat fee, posted exactly once)", a.ActualPaidUSD)
	}
	if !approxEq(a.SpendLeverage, 20.0) {
		t.Errorf("spend leverage = %.4f, want 20.0000 ($2,000 list / $100 paid)", a.SpendLeverage)
	}
	// The fee must NOT have leaked into the denominator: TIER's cost side is the
	// comparable-priced tokens alone. This is the inversion's central separation,
	// and the only place in the suite where both sides are visible at once.
	if !approxEq(a.TotalCostUSD, 2000.0) {
		t.Errorf("total cost = %.4f, want 2000.0000 — the flat fee must never enter the TIER denominator", a.TotalCostUSD)
	}
}
