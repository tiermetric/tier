package opencode

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// collectFrom runs one Collect pass over a fixture database scoped to repoRoot.
func collectFrom(t *testing.T, dbPath, repoRoot string) ([]collector.TokenEvent, *capturingLogger) {
	t.Helper()
	logger, cap := newTestLogger()
	c, err := New(Config{
		DBPath:      dbPath,
		Repos:       []RepoTarget{{Path: repoRoot, Slug: "tiermetric/tier"}},
		DeveloperID: "alice",
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return events, cap
}

// ─────────────────────────────────────────────────────────────────────────────
// THE GOLDEN ROW
// ─────────────────────────────────────────────────────────────────────────────

// Golden-row constants. One message with NON-ZERO cache reads AND non-zero
// reasoning — the only combination that can distinguish a correct implementation
// from the two errors no counter can see.
//
// ⭐ WHY THIS TEST EXISTS AND WHY IT IS THE ONLY GUARD FOR ITS TWO FAILURES.
// The cache-multiplier error and the reasoning-mapping error both produce a
// perfectly well-formed event: right model, right provider, right token counts,
// an exact (non-guessed) price-table hit, a non-zero cost, no WARN, no counter
// increment anywhere. Every other guard in this package passes on both of them.
// Only an assertion against a cost computed BY HAND, from the published rates,
// outside the code under test, can tell them apart.
//
// THE HAND COMPUTATION, at the built-in glm-5.3 row's rates (internal/store/prices.yaml, #786)
// ($1.40/M input, $0.26/M cached input — i.e. input x 0.18571428571428572 —
// $4.40/M output), for input=1,234 output=567 reasoning=8,901 cache.read=234,567:
//
//	total (Opencode's own)  = 1,234 + 567 + 8,901 + 234,567 =    245,269 ✓ additive
//
//	input       1,234 x $1.40/M                            = $  0.001727600000
//	cache read  234,567 x $1.40/M x 0.18571428571428572    = $  0.060987420000
//	            (1.40 x 0.18571428571428572 is bitwise 0.26, so this is exactly
//	             234,567 x $0.26/M — the rate the row cites)
//	output      (567 + 8,901) = 9,468 x $4.40/M            = $  0.041659200000
//	                                                         ─────────────────
//	                                                         $  0.104374220000
//
//	x 1e6 = 104,374.22 micro-dollars -> round-half-to-even -> 104,374
//
// The fractional part is .22, so RoundToEven's tie rule is not exercised and
// plain rounding gives the same integer — do not read this constant as depending
// on the tie rule.
const (
	goldenInput      = 1_234
	goldenOutput     = 567
	goldenReasoning  = 8_901
	goldenCacheRead  = 234_567
	goldenTotal      = goldenInput + goldenOutput + goldenReasoning + goldenCacheRead
	goldenCostMicro  = 104_374
	goldenOutputTok  = goldenOutput + goldenReasoning
	goldenCompleted  = int64(1_787_939_641_519)
	goldenSessionID  = "ses_golden"
	goldenMessageID  = "msg_golden"
	goldenClientCost = 0.0 // what Opencode itself writes on every subscription row
)

// The two errors this row exists to catch, each computed by the SAME hand method
// so a reader can check the arithmetic rather than trust it:
//
//	reasoning dropped (the Codex convention, wrong here):
//	  $0.001727600000 + $0.060987420000 + 567 x $4.40/M ($0.0024948) = $0.065209820000
//	  -> 65,210 micro
//
//	cache_read_mult left at the self-hosted 1.0x default:
//	  $0.001727600000 + 234,567 x $1.40/M ($0.3283938) + $0.041659200000 = $0.371780600000
//	  -> 371,781 micro
const (
	goldenCostIfReasoningDropped = 65_210
	goldenCostIfCacheMultDefault = 371_781
)

// TestGoldenRow_HandComputedCost prices one real-shaped message through the whole
// collector and compares it to the hand computation above.
func TestGoldenRow_HandComputedCost(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	cwd := filepath.Join(repo, "src")

	completed := goldenCompleted
	dbPath := newFixtureDB(t, dbSpec{
		Migrations: 3,
		Rows: []msgRow{buildRow(t, msgSpec{
			ID:          goldenMessageID,
			SessionID:   goldenSessionID,
			Cwd:         cwd,
			Created:     goldenCompleted - 3_000,
			Completed:   &completed,
			TimeUpdated: goldenCompleted,
			ClientCost:  goldenClientCost,
			Tokens:      autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
		})},
	})

	events, _ := collectFrom(t, dbPath, repo)
	if len(events) != 1 {
		t.Fatalf("want exactly 1 event, got %d", len(events))
	}
	ev := events[0]

	// The additive identity the fixture asserts about ITSELF, so a future edit to
	// the constants that breaks the shape fails here and not somewhere subtle.
	if goldenInput+goldenOutput+goldenReasoning+goldenCacheRead != goldenTotal {
		t.Fatalf("the golden fixture is not additive; the constants are wrong, not the code")
	}

	if ev.CostMicro != goldenCostMicro {
		t.Errorf("CostMicro = %d, want %d (hand-computed from the published Z.ai rates; see the comment above this test).\n"+
			"  %d would mean reasoning was DROPPED from the output class (the Codex convention, wrong here)\n"+
			"  %d would mean cache_read_mult fell back to the self-hosted 1.0x default",
			ev.CostMicro, goldenCostMicro, goldenCostIfReasoningDropped, goldenCostIfCacheMultDefault)
	}
	// Naming the two wrong answers as EXPLICIT rejections, not just as prose in
	// the message above: if a refactor ever made either of them the right answer,
	// this test would say which one landed.
	if ev.CostMicro == goldenCostIfReasoningDropped {
		t.Errorf("CostMicro = %d: reasoning tokens were dropped. Opencode reports reasoning BESIDE output; OutputTok must be output+reasoning", ev.CostMicro)
	}
	if ev.CostMicro == goldenCostIfCacheMultDefault {
		t.Errorf("CostMicro = %d: cache reads were billed at the FULL input rate. The Z.ai row's cache_read_mult is not being applied", ev.CostMicro)
	}

	if ev.OutputTok != goldenOutputTok {
		t.Errorf("OutputTok = %d, want %d (output %d + reasoning %d)", ev.OutputTok, goldenOutputTok, goldenOutput, goldenReasoning)
	}
	if ev.InputTok != goldenInput {
		t.Errorf("InputTok = %d, want %d", ev.InputTok, goldenInput)
	}
	if ev.CacheRead != goldenCacheRead {
		t.Errorf("CacheRead = %d, want %d", ev.CacheRead, goldenCacheRead)
	}
	if ev.Host != "zai-coding-plan" {
		t.Errorf("Host = %q, want %q — a blank host stores the row as unauditable spend (#236)", ev.Host, "zai-coding-plan")
	}
	if ev.BillingMode != store.BillingPerToken {
		t.Errorf("BillingMode = %q, want %q — GLM on the coding plan is costed per token at list price (#786)", ev.BillingMode, store.BillingPerToken)
	}
	if ev.Source != collector.SourceOpencode {
		t.Errorf("Source = %q, want %q", ev.Source, collector.SourceOpencode)
	}
	if ev.Fidelity != collector.FidelityRealtime {
		t.Errorf("Fidelity = %q, want %q", ev.Fidelity, collector.FidelityRealtime)
	}
	if ev.SessionID != goldenSessionID {
		t.Errorf("SessionID = %q, want %q", ev.SessionID, goldenSessionID)
	}
	if ev.Timestamp.UnixMilli() != goldenCompleted {
		t.Errorf("Timestamp = %s, want the message's time.completed (%d)", ev.Timestamp, goldenCompleted)
	}
	if ev.Timestamp.Location() != time.UTC {
		t.Errorf("Timestamp location = %v, want UTC — modernc.org/sqlite compares DATETIME strings lexically, so an offset-bearing timestamp windows incorrectly (#199)", ev.Timestamp.Location())
	}
}

// TestGoldenRow_PricingPathIsComputeCostHost proves the collector routes through
// the SAME entry point every other persisting producer uses, rather than
// reimplementing the arithmetic.
//
// It is a separate assertion from the hand-computed cost on purpose: a collector
// that hard-coded 104374 would pass the golden row and fail here, and a collector
// that called ComputeCostHost with the WRONG usage would pass here and fail the
// golden row. Neither test subsumes the other.
func TestGoldenRow_PricingPathIsComputeCostHost(t *testing.T) {
	withZaiPrices(t)
	want, wantMode := store.ComputeCostHost("zai-coding-plan", "glm-5.3", store.CostUsage{
		Input:     goldenInput,
		Output:    goldenOutputTok,
		CacheRead: goldenCacheRead,
	})
	if want != goldenCostMicro {
		t.Fatalf("store.ComputeCostHost returned %d for the golden usage but the hand computation says %d — one of them is wrong, and this test cannot say which; check the rates in %s against https://docs.z.ai/guides/overview/pricing",
			want, goldenCostMicro, embeddedPricesPath)
	}

	repo := repoDir(t)
	completed := goldenCompleted
	dbPath := newFixtureDB(t, dbSpec{
		Migrations: 3,
		Rows: []msgRow{buildRow(t, msgSpec{
			ID: goldenMessageID, SessionID: goldenSessionID,
			Cwd: filepath.Join(repo, "src"), Created: completed - 1000,
			Completed: &completed, TimeUpdated: completed,
			Tokens: autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
		})},
	})
	events, _ := collectFrom(t, dbPath, repo)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if events[0].CostMicro != want || events[0].BillingMode != wantMode {
		t.Errorf("event = (%d micro, %q); store.ComputeCostHost for the same usage = (%d micro, %q). The collector is not pricing through ComputeCostHost",
			events[0].CostMicro, events[0].BillingMode, want, wantMode)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NEVER TRUST THE CLIENT'S COST
// ─────────────────────────────────────────────────────────────────────────────

// TestNeverTrustsTheClientsCost feeds a message whose `cost` field is a large
// NON-ZERO number and requires the emitted cost to be the one TIER derived.
//
// The real store writes `cost: 0` on every row, and a test that only used zero
// would pass under an implementation that imported the field — zero is what the
// broken and the correct implementation both produce when the client says zero.
// A conspicuous 99.5 makes the two answers unmistakably different: importing it
// would yield 99,500,000 micro against the correct 104,374.
func TestNeverTrustsTheClientsCost(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	completed := goldenCompleted
	dbPath := newFixtureDB(t, dbSpec{
		Migrations: 3,
		Rows: []msgRow{buildRow(t, msgSpec{
			ID: "msg_cost", SessionID: "ses_cost",
			Cwd: filepath.Join(repo, "src"), Created: completed - 1000,
			Completed: &completed, TimeUpdated: completed,
			ClientCost: 99.5,
			Tokens:     autoTotal(goldenInput, goldenOutput, goldenReasoning, goldenCacheRead, 0),
		})},
	})
	events, _ := collectFrom(t, dbPath, repo)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if got := events[0].CostMicro; got != goldenCostMicro {
		t.Errorf("CostMicro = %d, want %d. %d would mean the client's `cost` field was imported",
			got, goldenCostMicro, store.DollarsToMicro(99.5))
	}
}

// TestZeroCostWithRealTokensIsFlagged: an event with real tokens that prices to
// nothing is the exact shape that makes work read as FREE and inflates TIER, so
// it must be LOUD.
//
// It is reachable in practice — not by importing the client's zero (structurally
// impossible here) but from OUR side, when the price table carries no row for the
// (host, model) and the fallback rounds a small event to zero micro-dollars. The
// fixture reproduces that with a model priced at the guessed self-hosted-medium
// rate ($0.50/M) and a single input token: 1 x $0.50/M = 0.5 micro, which
// RoundToEven takes to 0.
func TestZeroCostWithRealTokensIsFlagged(t *testing.T) {
	withZaiPrices(t)
	repo := repoDir(t)
	completed := goldenCompleted
	dbPath := newFixtureDB(t, dbSpec{
		Migrations: 3,
		Rows: []msgRow{buildRow(t, msgSpec{
			ID: "msg_zero", SessionID: "ses_zero",
			Model: "glm-9.9-not-in-the-table",
			Cwd:   filepath.Join(repo, "src"), Created: completed - 1000,
			Completed: &completed, TimeUpdated: completed,
			Tokens: autoTotal(1, 0, 0, 0, 0),
		})},
	})
	events, cap := collectFrom(t, dbPath, repo)
	if len(events) != 1 {
		t.Fatalf("want 1 event (a flagged event is still emitted; dropping it would lose the row AND the signal), got %d", len(events))
	}
	if events[0].CostMicro != 0 {
		t.Fatalf("the fixture no longer produces a zero-cost event (cost = %d); it cannot exercise the flag it exists for", events[0].CostMicro)
	}
	if got := cap.find(slog.LevelWarn, "priced to ZERO"); len(got) != 1 {
		t.Errorf("want exactly one WARN naming the zero-cost event, got %d. Records:%s", len(got), cap.dump())
	}
}
