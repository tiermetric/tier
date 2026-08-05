package store

import (
	"context"
	"testing"
	"time"
)

// costByKey reads one row's stored cost_micro and price_version by
// idempotency_key — the direct read the exclusion tests need, since
// onlyCostMicro aggregates and would hide WHICH row moved.
func costByKey(t *testing.T, db *DB, key string) (cost int64, version int) {
	t.Helper()
	if err := db.db.QueryRow(
		`SELECT cost_micro, price_version FROM token_events WHERE idempotency_key = ?`, key,
	).Scan(&cost, &version); err != nil {
		t.Fatalf("read row %s: %v", key, err)
	}
	return cost, version
}

// insertCollectorEvent writes a token_event as an automated collector would:
// an explicit (wrong) cost at an old price_version, under a NON-'api' source, so
// a reprice has something legitimate to correct. Keyed so the exclusion tests can
// read exactly this row back.
func insertCollectorEvent(t *testing.T, db *DB, key, issue string, inputTok int, storedCost int64) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), TokenEvent{
		Developer:      "alice",
		IssueID:        issue,
		Model:          repriceModel,
		InputTok:       inputTok,
		CostMicro:      storedCost,
		Source:         "jsonl",
		Fidelity:       "realtime",
		IdempotencyKey: key,
		PriceVersion:   1,
		Timestamp:      time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("InsertTokenEvent %s: %v", key, err)
	}
}

// TestReprice_NeverRepricesManualCostRows is the RED-1 fix at the source grain:
// a caller-authoritative POST /api/v1/costs row (source='api') carries the
// CALLER's figure — an invoice number, not a price the server derived — so
// recomputing it from token counts does not correct it, it destroys it.
// recomputeKnownSourceCosts has excluded source='api' since #55 for exactly this
// reason; Reprice simply never got the same treatment, and before this fix a
// sweep rewrote a $42.00 manual import to $0.00.
//
// The manual row here carries REAL token counts (1000 in / 500 out), so the
// zero-token guard cannot be what saves it — only the source filter can. The
// jsonl control row carries non-zero tokens and the same wrong cost, and MUST be
// repriced: that is what proves the exclusion keys on source rather than on
// something incidental to the setup. (The two fixtures are NOT identical — the
// control is input-only and pinned at v1, the manual row is stamped to the
// active version by stampPriceVersion. Neither difference is load-bearing: both
// rows are at or above the floor and both carry non-zero tokens, which is all
// the argument needs.)
func TestReprice_NeverRepricesManualCostRows(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	manual := manualCostEvent("finance-invoice-1", 42_000_000) // $42.00, 1000 in / 500 out
	if err := db.InsertManualCostEvent(ctx, manual); err != nil {
		t.Fatalf("insert manual cost row: %v", err)
	}
	// Control arm: identical token counts and an identically wrong cost, but
	// captured by a collector — this one MUST move.
	insertCollectorEvent(t, db, "jsonl-control-1", "issue-control", 1000, 42_000_000)

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}

	if got, _ := costByKey(t, db, "finance-invoice-1"); got != 42_000_000 {
		t.Errorf("manual-import cost_micro after reprice = %d, want 42000000 UNCHANGED "+
			"(a caller-authoritative figure must never be re-derived from token counts)", got)
	}
	if res.ExcludedRowCount != 1 {
		t.Errorf("ExcludedRowCount = %d, want 1 (the manual row matched the floor and was protected)", res.ExcludedRowCount)
	}

	// Control arm must have moved, or this test would pass with a Reprice that
	// simply does nothing at all.
	wantControl := ComputeCost(repriceModel, CostUsage{Input: 1000})
	if got, _ := costByKey(t, db, "jsonl-control-1"); got != wantControl {
		t.Errorf("jsonl control cost_micro = %d, want %d (a collector-captured row MUST still reprice)", got, wantControl)
	}
	if res.RowCount != 1 || res.ChangedRowCount != 1 {
		t.Errorf("RowCount=%d ChangedRowCount=%d, want 1/1 (the control row only)", res.RowCount, res.ChangedRowCount)
	}
}

// TestReprice_NeverRepricesMoneyCarryingZeroTokenRows is the RED-1 fix at the
// second grain, and it is NOT redundant with the source filter: store.
// InsertTokenEvent is exported and takes CostMicro verbatim, so a row carrying
// money with no token counts is reachable under ANY source. Re-deriving such a
// row's cost from tokens that do not exist can only zero it, whoever wrote it.
//
// The guard is deliberately scoped to MONEY-CARRYING zero-token rows, and the
// second arm is what pins that scope: a zero-token row whose cost is legitimately
// 0 still needs its price_version reconciled, so an unqualified zero-token guard
// would over-exclude it. Repricing it is a no-op on the cost by construction.
func TestReprice_NeverRepricesMoneyCarryingZeroTokenRows(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// Money with no tokens to explain it — protected.
	insertCollectorEvent(t, db, "zero-tok-money", "issue-money", 0, 42_000_000)
	// No tokens, no money — NOT protected: it must still get its version bump.
	insertCollectorEvent(t, db, "zero-tok-free", "issue-free", 0, 0)

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}

	if got, ver := costByKey(t, db, "zero-tok-money"); got != 42_000_000 || ver != 1 {
		t.Errorf("money-carrying zero-token row = (cost %d, v%d), want (42000000, v1) UNTOUCHED "+
			"(a cost the token counts cannot explain must not be re-derived)", got, ver)
	}
	if res.ExcludedRowCount != 1 {
		t.Errorf("ExcludedRowCount = %d, want 1", res.ExcludedRowCount)
	}

	// The scope arm: zero tokens AND zero cost is repriceable, and was repriced.
	active := ActivePriceTableInfo().Version
	if got, ver := costByKey(t, db, "zero-tok-free"); got != 0 || ver != active {
		t.Errorf("zero-token zero-cost row = (cost %d, v%d), want (0, v%d) — the guard must not "+
			"over-exclude a row that legitimately needs its price_version reconciled", got, ver, active)
	}
	if res.RowCount != 1 || res.ChangedRowCount != 1 {
		t.Errorf("RowCount=%d ChangedRowCount=%d, want 1/1 (the zero-cost row only)", res.RowCount, res.ChangedRowCount)
	}
}

// TestReprice_SanctionedCorrectionSurvivesReprice is the whole RED-1 defect
// end to end, in the terms that made it RED rather than a wart: it is not only
// that a $42.00 sanctioned correction became $0.00, it is that
// cost_correction_audit went on asserting the old -> new pair as though it still
// held. A ledger that lies is not an audit trail.
//
// This asserts the ledger against the LIVE row, not against itself: after a
// committed reprice sweep, the row's cost_micro must still equal the
// new_cost_micro the ledger claims it was corrected to.
func TestReprice_SanctionedCorrectionSurvivesReprice(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	original := manualCostEvent("finance-k1", 42_000_000)
	if err := db.InsertManualCostEvent(ctx, original); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	corrected := manualCostEvent("finance-k1", 50_000_000)
	res, err := db.CorrectManualCostEvent(ctx, corrected, "finance-alice", "Q3 invoice reconciliation")
	if err != nil {
		t.Fatalf("CorrectManualCostEvent: %v", err)
	}
	if !res.Corrected {
		t.Fatal("Corrected = false, want true (setup precondition)")
	}

	if _, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true}); err != nil {
		t.Fatalf("Reprice: %v", err)
	}

	_, _, ledgerNew, _, _ := auditRow(t, db)
	stored, _ := costByKey(t, db, "finance-k1")
	if stored != 50_000_000 {
		t.Errorf("corrected cost_micro after a reprice sweep = %d, want 50000000 — "+
			"the sanctioned correction was destroyed", stored)
	}
	if stored != ledgerNew {
		t.Errorf("cost_correction_audit asserts new_cost_micro=%d but the row now holds %d — "+
			"the ledger is affirmatively false with no marker", ledgerNew, stored)
	}
}

// TestReprice_ExaminedAndExcludedPartitionTheFloor pins the claim that the
// candidate SELECT and the excluded COUNT share one predicate: whatever the
// predicate says, the two sets must PARTITION the rows at or above the floor —
// every such row is either examined or reported as protected, never both and
// never neither. If the two queries ever drift apart, this fails, and the CLI's
// "protected N row(s)" line stops being trustworthy.
//
// It spans all four categories at once so the invariant is exercised, not just
// asserted on a degenerate table.
func TestReprice_ExaminedAndExcludedPartitionTheFloor(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if err := db.InsertManualCostEvent(ctx, manualCostEvent("api-row", 42_000_000)); err != nil {
		t.Fatalf("insert manual: %v", err)
	}
	insertCollectorEvent(t, db, "collector-priced", "issue-priced", 1000, 999_000)
	insertCollectorEvent(t, db, "collector-zero-money", "issue-zero-money", 0, 7_000_000)
	insertCollectorEvent(t, db, "collector-zero-free", "issue-zero-free", 0, 0)

	var total int64
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM token_events WHERE price_version >= 1`).Scan(&total); err != nil {
		t.Fatalf("count rows at floor: %v", err)
	}
	if total != 4 {
		t.Fatalf("setup: %d rows at or above the floor, want 4", total)
	}

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: false})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	if res.RowCount+res.ExcludedRowCount != total {
		t.Errorf("examined(%d) + excluded(%d) = %d, want %d — the candidate SELECT and the "+
			"excluded COUNT no longer describe complementary sets",
			res.RowCount, res.ExcludedRowCount, res.RowCount+res.ExcludedRowCount, total)
	}
	// Both halves must be non-empty, or the partition above would hold trivially
	// for a predicate that excludes everything (or nothing).
	if res.RowCount != 2 || res.ExcludedRowCount != 2 {
		t.Errorf("examined=%d excluded=%d, want 2/2 (api + zero-token-money protected; "+
			"priced + zero-token-free examined)", res.RowCount, res.ExcludedRowCount)
	}
	// Counts alone are not enough: swapping WHICH two rows are excluded still
	// yields 2/2. Commit and assert the identities, so this test stands on its
	// own rather than leaning on its neighbours.
	if _, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true}); err != nil {
		t.Fatalf("Reprice commit: %v", err)
	}
	if got, _ := costByKey(t, db, "api-row"); got != 42_000_000 {
		t.Errorf("api-row cost = %d, want 42000000 (protected)", got)
	}
	if got, _ := costByKey(t, db, "collector-zero-money"); got != 7_000_000 {
		t.Errorf("collector-zero-money cost = %d, want 7000000 (protected)", got)
	}
	if got, _ := costByKey(t, db, "collector-priced"); got != ComputeCost(repriceModel, CostUsage{Input: 1000}) {
		t.Errorf("collector-priced cost = %d, want it REPRICED", got)
	}
}

// TestReprice_ExcludedCountReportedOnDryRunAndCommit proves the protection is
// visible BEFORE an operator commits, not only after. A dry run that hid the
// protected rows would let an operator commit believing the sweep covered
// everything at the floor.
func TestReprice_ExcludedCountReportedOnDryRunAndCommit(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if err := db.InsertManualCostEvent(ctx, manualCostEvent("api-row", 42_000_000)); err != nil {
		t.Fatalf("insert manual: %v", err)
	}
	dry, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: false})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	commit, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if dry.ExcludedRowCount != 1 || commit.ExcludedRowCount != 1 {
		t.Errorf("ExcludedRowCount dry=%d commit=%d, want 1/1 (identical in both modes)",
			dry.ExcludedRowCount, commit.ExcludedRowCount)
	}
	// And the floor genuinely matched nothing repriceable, which is the case the
	// CLI must NOT report as "check --from-version".
	if dry.RowCount != 0 {
		t.Errorf("RowCount = %d, want 0 (the only row at the floor is protected)", dry.RowCount)
	}
}

// TestReprice_ExcludedRowNeverTripsTheGuessGate closes an operator-facing claim
// that the exclusions create and nothing else checks. --allow-guessed exists to
// BLOCK a commit that would rewrite audited history to a size-class estimate, and
// it keys on GuessedRowCount. An excluded row never enters the candidate scan, so
// it can neither be repriced nor counted as guessed — meaning a manual import
// naming a model that is not in the price table must NOT block an otherwise clean
// commit.
//
// Without this, the safe-by-default gate could silently become "no reprice ever
// commits again" for any install holding one manual import of a retired model.
func TestReprice_ExcludedRowNeverTripsTheGuessGate(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// A manual import naming a model with NO entry in the active price table.
	unpriceable := manualCostEvent("finance-retired-model", 42_000_000)
	unpriceable.Model = "totally-retired-model-xyz"
	if err := db.InsertManualCostEvent(ctx, unpriceable); err != nil {
		t.Fatalf("seed manual row: %v", err)
	}
	// Plus one genuinely repriceable row, so the commit has real work to do.
	insertCollectorEvent(t, db, "jsonl-known", "issue-known", 1000, 999_000)

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true}) // NO AllowGuessed
	if err != nil {
		t.Fatalf("Reprice commit: %v — an EXCLUDED row's unpriceable model must not gate the commit", err)
	}
	if res.GuessedRowCount != 0 {
		t.Errorf("GuessedRowCount = %d, want 0 (the unpriceable model is on an excluded row, "+
			"so it is never priced at the GUESS fallback in the first place)", res.GuessedRowCount)
	}
	if len(res.GuessedModels) != 0 {
		t.Errorf("GuessedModels = %v, want empty", res.GuessedModels)
	}
	if got, _ := costByKey(t, db, "finance-retired-model"); got != 42_000_000 {
		t.Errorf("manual row cost = %d, want 42000000 untouched", got)
	}
	if got, _ := costByKey(t, db, "jsonl-known"); got != ComputeCost(repriceModel, CostUsage{Input: 1000}) {
		t.Errorf("collector row cost = %d, want repriced", got)
	}
}
