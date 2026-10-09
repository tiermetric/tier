package opencode

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// THE RECONCILIATION AGAINST A REAL OPENCODE STORE
// ─────────────────────────────────────────────────────────────────────────────
//
// This is the ONLY test in the package that touches a real Opencode database, and
// it SKIPS unless TIER_OPENCODE_DB is set. It is written as a test rather than a
// throwaway script so the reconciliation is a runnable, reviewable command that
// lives with the code it verifies:
//
//	TIER_OPENCODE_DB="$HOME/.local/share/opencode/opencode.db" \
//	TIER_OPENCODE_REPO="$HOME,/private/tmp" \
//	go test ./internal/collector/opencode/ -run TestReconcileAgainstRealStore -v
//
// It is env-gated because `make check` must not depend on one machine's data. The
// synthetic fixtures elsewhere in this package carry every behavioural guard; what
// only a real store can establish is that the guards were pointed at the right
// shape in the first place.
//
// ⚠️ TIER_OPENCODE_REPO IS A COMMA-SEPARATED LIST, AND IT HAS TO BE. The collector
// DROPS any message whose cwd is outside every configured repo — cross-repo bleed
// would put another project's dollars on this project's issues (#15) — whereas the
// #712 population was counted from the database with no cwd filter at all. So a
// scope that misses even one working directory reconciles SHORT, correctly, and
// the shortfall is a scoping difference rather than a capture bug. Measured on the
// maintainer's store: `$HOME` alone reconciles 3,940 of the 3,943 rows, missing
// exactly the three whose cwd is `/private/tmp` (21,252 tokens). The test reports
// the foreign-repo skip count so a short reconciliation says WHY.

// The #712 measurement window. The maintainer's GLM-5.3 corpus was measured for
// the Z.ai price rows at a specific instant, and the store has kept growing since
// — so "all GLM-5.3 rows today" is a MOVING number and reconciling against it
// would be reconciling against nothing. This cutoff is the largest
// `time.completed` in that measured population; filtering on it reproduces the
// #712 population exactly (verified: 3,943 rows, and every one of the four
// per-class totals below matches internal/store/prices_glm53_test.go).
const reconcileCutoffMS = int64(1_787_939_641_519) // 2026-08-28T17:54:01.519Z

// The measured mix, copied from the constants #712 asserts its price rows
// against (internal/store/prices_glm53_test.go). Restated here rather than
// imported because those are unexported — and because a reconciliation that
// derived its expectation from the code under test would prove nothing.
const (
	reconcileRows      = 3_943
	reconcileInput     = 23_863_660
	reconcileCacheRead = 241_709_760
	reconcileOutput    = 788_691
	reconcileReasoning = 4_415_226
	reconcileTokens    = reconcileInput + reconcileCacheRead + reconcileOutput + reconcileReasoning // 270,777,337
)

// 🔴 TWO DOLLAR FIGURES, AND THE DIFFERENCE BETWEEN THEM IS REAL.
//
// #712 published $119.150896 (119_150_896 micro). That figure prices the mix as
// ONE aggregate: four class totals, one call to the rate arithmetic, one rounding.
//
// A per-message collector cannot reproduce it exactly, and should not pretend to.
// store.ComputeCostHost rounds EVERY event to an integer micro-dollar
// (DollarsToMicro -> math.RoundToEven), and the store sums integers — so the
// collector's total is the sum of 3,943 independent roundings. Measured, that is
// 119_150_906 micro: exactly 10 micro-dollars ($0.00001, 8.4e-8 relative) above
// the aggregate figure.
//
// Both numbers are correct for what they measure. The collector's is the one that
// will actually be in the database, so it is the one pinned here; the aggregate is
// pinned alongside it, together with the bound on their difference, so that a
// future divergence has to name which of the two moved.
const (
	reconcileCostMicroPerMessage = 119_150_906 // what SUM(cost_micro) will hold
	reconcileCostMicroAggregate  = 119_150_896 // what #712 published
	// One micro-dollar of rounding slack per message is the arithmetic ceiling on
	// the gap; the measured gap is 10 over 3,943 messages. The bound is stated as
	// the row count so it stays meaningful if the population is ever re-measured.
	reconcileRoundingSlackMicro = reconcileRows
)

// TestReconcileAgainstRealStore ingests a real Opencode database through the
// collector and reconciles the #712 population to the token and to the
// micro-dollar.
func TestReconcileAgainstRealStore(t *testing.T) {
	dbPath := os.Getenv("TIER_OPENCODE_DB")
	if dbPath == "" {
		t.Skip("set TIER_OPENCODE_DB to a real Opencode database to run the reconciliation (see the comment above this test)")
	}
	roots := os.Getenv("TIER_OPENCODE_REPO")
	if roots == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("resolve home dir: %v", err)
		}
		roots = home
	}
	var targets []RepoTarget
	for _, p := range strings.Split(roots, ",") {
		if p = strings.TrimSpace(p); p != "" {
			targets = append(targets, RepoTarget{Path: p, Slug: "tiermetric/tier"})
		}
	}
	t.Logf("scoping to %d repo target(s): %s", len(targets), roots)

	withZaiPrices(t)
	g := installGuessRecorder(t)

	logger, cap := newTestLogger()
	c, err := New(Config{
		DBPath:      dbPath,
		Repos:       targets,
		DeveloperID: "maintainer",
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	start := time.Now()
	events, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	t.Logf("collected %d events from %s in %s", len(events), dbPath, time.Since(start))

	// ── the #712 population ──────────────────────────────────────────────────
	var (
		rows      int
		input     int64
		output    int64 // already output+reasoning, as this collector maps it
		cacheRead int64
		costMicro int64
	)
	// Everything OUTSIDE the window, reported so the reader can see what the
	// cutoff excluded rather than having to trust that it excluded the right thing.
	var afterRows int
	var afterTokens, afterCostMicro int64

	for _, ev := range events {
		if ev.Source != collector.SourceOpencode {
			t.Fatalf("event carries source %q", ev.Source)
		}
		if ev.Model != "glm-5.3" || ev.Host != "zai-coding-plan" {
			continue
		}
		if ev.Timestamp.UnixMilli() > reconcileCutoffMS {
			afterRows++
			afterTokens += int64(ev.InputTok + ev.OutputTok + ev.CacheRead)
			afterCostMicro += ev.CostMicro
			continue
		}
		rows++
		input += int64(ev.InputTok)
		output += int64(ev.OutputTok)
		cacheRead += int64(ev.CacheRead)
		costMicro += ev.CostMicro
	}
	tokens := input + output + cacheRead

	t.Logf("── #712 window (time.completed <= %s) ──", time.UnixMilli(reconcileCutoffMS).UTC().Format(time.RFC3339Nano))
	t.Logf("  messages   %d", rows)
	t.Logf("  input      %d", input)
	t.Logf("  cache read %d", cacheRead)
	t.Logf("  output     %d  (output + reasoning)", output)
	t.Logf("  TOKENS     %d", tokens)
	t.Logf("  COST       %d micro = $%.6f", costMicro, store.MicroToDollars(costMicro))
	t.Logf("── after the window (the store kept growing) ──")
	t.Logf("  messages %d  tokens %d  cost $%.2f", afterRows, afterTokens, store.MicroToDollars(afterCostMicro))

	if rows != reconcileRows {
		t.Errorf("messages = %d, want %d. If this is SHORT, check skipped_foreign_repo below: the collector drops any cwd outside TIER_OPENCODE_REPO, and the #712 population was counted with no cwd filter",
			rows, reconcileRows)
	}
	if input != reconcileInput {
		t.Errorf("input = %d, want %d", input, reconcileInput)
	}
	if cacheRead != reconcileCacheRead {
		t.Errorf("cache read = %d, want %d", cacheRead, reconcileCacheRead)
	}
	if want := int64(reconcileOutput + reconcileReasoning); output != want {
		t.Errorf("output = %d, want %d (output %d + reasoning %d). %d alone would mean reasoning was dropped",
			output, want, reconcileOutput, reconcileReasoning, reconcileOutput)
	}
	if tokens != reconcileTokens {
		t.Errorf("TOKENS = %d, want %d", tokens, reconcileTokens)
	}
	if costMicro != reconcileCostMicroPerMessage {
		t.Errorf("COST = %d micro ($%.6f), want %d ($%.6f)",
			costMicro, store.MicroToDollars(costMicro), int64(reconcileCostMicroPerMessage), store.MicroToDollars(reconcileCostMicroPerMessage))
	}
	// The relationship to #712's published aggregate, asserted rather than
	// asserted-away: same figure to the cent, and the gap bounded by the
	// per-message rounding it comes from.
	gap := costMicro - reconcileCostMicroAggregate
	if gap < 0 {
		gap = -gap
	}
	if gap > reconcileRoundingSlackMicro {
		t.Errorf("the per-message total is %d micro from #712's aggregate figure %d; that is beyond the %d micro that %d independent roundings can explain, so this is a pricing difference, not a rounding one",
			gap, int64(reconcileCostMicroAggregate), int64(reconcileRoundingSlackMicro), rows)
	}
	if a, b := round2(store.MicroToDollars(costMicro)), round2(store.MicroToDollars(reconcileCostMicroAggregate)); a != b {
		t.Errorf("the per-message total (%d cents) and #712's aggregate (%d cents) do not agree at cent precision", a, b)
	}

	// ── zero guessed-cost share ──────────────────────────────────────────────
	//
	// The done-when condition. installGuessRecorder's own ability to redden is
	// proven by TestNegativeControl_AdmittedProviderServingUnpricedModelFiresTheGuessedCostCounter,
	// so a zero here is an earned zero and not an unwired counter.
	if n, micro := g.snapshot(); n != 0 || micro != 0 {
		t.Errorf("guessed-cost share is not zero: %d events / %.0f micro-dollars priced at an unaudited fallback rate", n, micro)
	}

	// ── the exclusions, reported ─────────────────────────────────────────────
	if summary := cap.find(slog.LevelInfo, "opencode scan complete"); len(summary) == 1 {
		t.Logf("excluded_by_provider: %s", attrOf(summary[0], "excluded_by_provider"))
		t.Logf("skipped_incomplete=%s skipped_zero_token=%s skipped_identity_violation=%s skipped_unverifiable=%s skipped_foreign_repo=%s",
			attrOf(summary[0], "skipped_incomplete"), attrOf(summary[0], "skipped_zero_token"),
			attrOf(summary[0], "skipped_identity_violation"), attrOf(summary[0], "skipped_unverifiable"),
			attrOf(summary[0], "skipped_foreign_repo"))
	}
	// Not one row of the real corpus may fail the additive identity. If any does,
	// the identity this collector is built on is not the one the data satisfies.
	for _, r := range cap.find(slog.LevelWarn, "additive identity") {
		t.Errorf("a REAL row failed the additive identity: %s", attrOf(r, "err"))
	}
}

func round2(v float64) int64 {
	if v < 0 {
		return -round2(-v)
	}
	return int64(v*100 + 0.5)
}
