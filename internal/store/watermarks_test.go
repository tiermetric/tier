package store

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// wmSeedEvent inserts one token event at `at`. Kept local so this file's
// fixtures never drift with another test file's helper.
func wmSeedEvent(t *testing.T, db *DB, issue string, at time.Time) {
	t.Helper()
	wmSeedEventRepo(t, db, issue, "acme/widgets", at)
}

func wmSeedEventRepo(t *testing.T, db *DB, issue, repo string, at time.Time) {
	t.Helper()
	err := db.InsertTokenEvent(context.Background(), TokenEvent{
		Developer: "alice",
		IssueID:   issue,
		Model:     "claude-sonnet-4",
		InputTok:  1000,
		OutputTok: 500,
		CostMicro: 10_500,
		Source:    "jsonl",
		Fidelity:  "realtime",
		Repo:      repo,
		Timestamp: at,
	})
	if err != nil {
		t.Fatalf("InsertTokenEvent(%s): %v", issue, err)
	}
}

// wmSeedOutcome inserts one outcome at `at` and returns its id, read back
// through the public path the production quality-revision caller uses.
func wmSeedOutcome(t *testing.T, db *DB, issue, sha string, quality float64, at time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	inserted, err := db.InsertOutcome(ctx, Outcome{
		Developer:      "alice",
		IssueID:        issue,
		PRNumber:       1,
		Weight:         3,
		Quality:        quality,
		MergeCommitSHA: sha,
		Repo:           "acme/widgets",
		Timestamp:      at,
	})
	if err != nil {
		t.Fatalf("InsertOutcome(%s): %v", issue, err)
	}
	if !inserted {
		t.Fatalf("InsertOutcome(%s): not inserted — fixture SHA %q collided", issue, sha)
	}
	o, ok, err := db.OutcomeByMergeCommit(ctx, sha)
	if err != nil || !ok {
		t.Fatalf("OutcomeByMergeCommit(%s): ok=%v err=%v", sha, ok, err)
	}
	return o.ID
}

// qualityHistoryRows counts quality_history directly, NOT through Watermarks.
// The vacuity control below has to establish that a history row really was
// written before it is allowed to conclude anything from the watermark; asking
// the watermark would make that check circular.
func qualityHistoryRows(t *testing.T, db *DB) int64 {
	t.Helper()
	var n int64
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM quality_history`).Scan(&n); err != nil {
		t.Fatalf("count quality_history: %v", err)
	}
	return n
}

// TestReportWatermarks_EmptyStoreIsZeroNotError pins the COALESCE: MAX(id) over
// an empty window is SQL NULL, which fails a scan into int64. An empty window's
// watermark is the real answer 0, not a 500.
func TestReportWatermarks_EmptyStoreIsZeroNotError(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	w, err := db.ReportWatermarks(context.Background(), time.Now().UTC().Add(-24*time.Hour), time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("ReportWatermarks on empty store: %v", err)
	}
	if w != (Watermarks{}) {
		t.Errorf("empty store watermarks = %+v, want the zero value", w)
	}
}

// TestReportWatermarks_BothSequencesAdvanceOnInsert is the positive arm. It
// asserts BOTH sequences move, because token_events.id and outcomes.id are
// separate AUTOINCREMENT counters — a singular "max event id" cannot bound
// outcomes, and a manifest carrying only one would look healthy while missing
// half the inputs.
//
// 🔴 THE TWO SEQUENCES ARE DELIBERATELY DESYNCHRONISED (three events, one
// outcome), AND THAT IS THE POINT OF THE FIXTURE. An earlier version seeded one
// of each, which made the two maxes — and both counts — numerically IDENTICAL at
// every step. A mutant that simply copied the token_events figures into the
// outcome fields SURVIVED that fixture: it could not distinguish "reads outcomes"
// from "reproduces token_events", so the test passed by arithmetic coincidence.
// With unequal populations the copy is immediately wrong, so the assertions are
// on EXACT expected values, not merely on "it went up".
func TestReportWatermarks_BothSequencesAdvanceOnInsert(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	since := now.Add(-24 * time.Hour)

	wmSeedEvent(t, db, "issue-1", now.Add(-3*time.Hour))
	wmSeedEvent(t, db, "issue-2", now.Add(-2*time.Hour))
	wmSeedEvent(t, db, "issue-3", now.Add(-time.Hour))
	wmSeedOutcome(t, db, "issue-1", "sha-1", 1.0, now.Add(-time.Hour))

	before, err := db.ReportWatermarks(ctx, since, time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("ReportWatermarks before: %v", err)
	}
	// The desynchronisation is asserted, not assumed: if a future fixture edit
	// re-aligned the two sequences, this test would quietly go back to being
	// unable to tell them apart.
	if before.Window.MaxTokenEventID == before.Window.MaxOutcomeID ||
		before.Window.TokenEventCount == before.Window.OutcomeCount {
		t.Fatalf("fixture precondition: the two sequences must differ, got events %d/%d outcomes %d/%d — "+
			"aligned sequences make this test blind to a field that merely copies the other",
			before.Window.MaxTokenEventID, before.Window.TokenEventCount,
			before.Window.MaxOutcomeID, before.Window.OutcomeCount)
	}
	if before.Window.MaxTokenEventID != 3 || before.Window.TokenEventCount != 3 {
		t.Fatalf("events before = %d/%d, want 3/3", before.Window.MaxTokenEventID, before.Window.TokenEventCount)
	}
	if before.Window.MaxOutcomeID != 1 || before.Window.OutcomeCount != 1 {
		t.Fatalf("outcomes before = %d/%d, want 1/1", before.Window.MaxOutcomeID, before.Window.OutcomeCount)
	}

	wmSeedEvent(t, db, "issue-4", now.Add(-30*time.Minute))
	wmSeedOutcome(t, db, "issue-2", "sha-2", 1.0, now.Add(-30*time.Minute))

	after, err := db.ReportWatermarks(ctx, since, time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("ReportWatermarks after: %v", err)
	}
	// Exact values. "Advanced" alone is satisfied by the wrong sequence advancing.
	if after.Window.MaxTokenEventID != 4 || after.Window.TokenEventCount != 4 {
		t.Errorf("events after = %d/%d, want 4/4", after.Window.MaxTokenEventID, after.Window.TokenEventCount)
	}
	if after.Window.MaxOutcomeID != 2 || after.Window.OutcomeCount != 2 {
		t.Errorf("outcomes after = %d/%d, want 2/2 — an outcome field carrying a token_events "+
			"figure would read 4/4 here", after.Window.MaxOutcomeID, after.Window.OutcomeCount)
	}
}

// TestReportWatermarks_TokenSideCoversTheAttributionBand is the arm that a
// comment claiming "the same predicate as the scoring reads" had instead of a
// test, and it was WRONG.
//
// 🔴 THE SCORING PATH DOES NOT READ token_events ONLY INSIDE [since, until).
// OutcomeTokenTotals — the #136 zero-token tripwire — builds a per-outcome window
// [merge - AttributableWindow, merge], so an outcome near the lower edge of the
// report window is funded by token_events up to 14 days BEFORE `since`. A
// late-ingested row in that band can lift a (developer, issue) total past
// scoring.MinAttributableTokens, clear the tripwire and change /scores — so a
// watermark bounded at `since` would hold still across two different reports.
//
// The two sides therefore have DIFFERENT lower bounds, and this test pins the
// difference in both directions: a row inside the band counts on the token side,
// and an outcome at the same instant does NOT count on the outcome side.
func TestReportWatermarks_TokenSideCoversTheAttributionBand(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	since := now.Add(-24 * time.Hour)
	// Squarely inside the 14-day look-back band and clearly before `since`.
	inBand := since.Add(-7 * 24 * time.Hour)
	// Outside even the band.
	beyondBand := since.Add(-AttributableWindow - 48*time.Hour)

	wmSeedEvent(t, db, "issue-inwindow", now.Add(-time.Hour))
	base, err := db.ReportWatermarks(ctx, since, time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("ReportWatermarks base: %v", err)
	}
	if base.Window.TokenEventCount != 1 {
		t.Fatalf("base token_event_count = %d, want 1", base.Window.TokenEventCount)
	}

	// A token event in the band must COUNT — this is the whole point.
	wmSeedEvent(t, db, "issue-inband", inBand)
	withBand, err := db.ReportWatermarks(ctx, since, time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("ReportWatermarks withBand: %v", err)
	}
	if withBand.Window.TokenEventCount != 2 {
		t.Errorf("token_event_count = %d, want 2: a token event %v before `since` is inside the "+
			"AttributableWindow band the scoring tripwire reads, so it MUST move the watermark",
			withBand.Window.TokenEventCount, since.Sub(inBand))
	}

	// A token event BEYOND the band must not count — the band is bounded, not
	// unbounded. Without this arm, simply dropping the lower bound entirely would
	// satisfy the assertion above.
	wmSeedEvent(t, db, "issue-beyond", beyondBand)
	beyond, err := db.ReportWatermarks(ctx, since, time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("ReportWatermarks beyond: %v", err)
	}
	if beyond.Window.TokenEventCount != 2 {
		t.Errorf("token_event_count = %d, want 2: an event beyond since-AttributableWindow is "+
			"outside everything the report reads and must NOT count", beyond.Window.TokenEventCount)
	}

	// 🔴 The asymmetry: the OUTCOME side is NOT widened. An outcome at the same
	// in-band instant is outside the report's window and must not count.
	wmSeedOutcome(t, db, "issue-inband", "sha-inband", 1.0, inBand)
	outcomes, err := db.ReportWatermarks(ctx, since, time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("ReportWatermarks outcomes: %v", err)
	}
	if outcomes.Window.OutcomeCount != 0 {
		t.Errorf("outcome_count = %d, want 0: the outcome side is [since, until) and is NOT "+
			"widened by AttributableWindow — widening it would over-count the report",
			outcomes.Window.OutcomeCount)
	}
}

// TestReportWatermarks_UpperBoundIsHalfOpen pins the `until` side. Without it,
// dropping the upper bound entirely passes every other test in this file,
// because no other fixture puts a row past `until`.
func TestReportWatermarks_UpperBoundIsHalfOpen(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	since := now.Add(-24 * time.Hour)
	until := now.Add(-6 * time.Hour)

	wmSeedEvent(t, db, "issue-inside", until.Add(-time.Hour))
	// EXACTLY at `until` — the half-open boundary. [since, until) EXCLUDES it.
	wmSeedEvent(t, db, "issue-at-until", until)
	wmSeedEvent(t, db, "issue-after", until.Add(time.Hour))
	wmSeedOutcome(t, db, "issue-inside", "sha-inside", 1.0, until.Add(-time.Hour))
	wmSeedOutcome(t, db, "issue-at-until", "sha-at-until", 1.0, until)

	w, err := db.ReportWatermarks(ctx, since, until, FleetWide)
	if err != nil {
		t.Fatalf("ReportWatermarks: %v", err)
	}
	if w.Window.TokenEventCount != 1 {
		t.Errorf("token_event_count = %d, want 1: the window is HALF-OPEN, so the row at exactly "+
			"`until` and the row after it are both excluded", w.Window.TokenEventCount)
	}
	if w.Window.OutcomeCount != 1 {
		t.Errorf("outcome_count = %d, want 1: same half-open rule on the outcome side", w.Window.OutcomeCount)
	}

	// Control: with the bound lifted, everything shows up. Without this, a read
	// that returned 1 for an unrelated reason would pass above.
	open, err := db.ReportWatermarks(ctx, since, time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("ReportWatermarks open: %v", err)
	}
	if open.Window.TokenEventCount != 3 || open.Window.OutcomeCount != 2 {
		t.Errorf("control: open-ended = %d events / %d outcomes, want 3/2 — the rows the bounded "+
			"read excluded do exist", open.Window.TokenEventCount, open.Window.OutcomeCount)
	}
}

// TestReportWatermarks_QualityRevisionIsInvisibleToTheObviousSequences IS THE
// GUARD THIS WHOLE CHANGE EXISTS FOR (#715).
//
// `outcomes.quality` is mutated IN PLACE. A revision therefore creates NO new
// row in token_events and NO new row in outcomes, while changing a direct
// multiplier on every weighted point the report publishes. A manifest
// watermarking only the two obvious sequences would pass the "advanced" half of
// a naive positive test and silently miss the revision entirely.
//
// Both halves are asserted, and the negative half is the load-bearing one:
//
//	(a) the two windowed sequences are UNCHANGED — correct, no new rows;
//	(b) max_quality_history_id ADVANCED — the revision is visible after all.
//
// Delete MaxQualityHistoryID and (b) fails. Window the quality_history read by
// the report's own [since, until) and (b) fails too, because the fixture's
// outcome is a month old while its revision is now.
func TestReportWatermarks_QualityRevisionIsInvisibleToTheObviousSequences(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	// A DELIBERATELY OLD window. The outcome sits a month back; the revision
	// below happens NOW. quality_history.ts is the instant of the MUTATION, not
	// of the row it mutated, so a window-filtered ledger read would find nothing.
	old := now.AddDate(0, -1, 0)
	since := old.Add(-24 * time.Hour)
	until := old.Add(24 * time.Hour)

	wmSeedEvent(t, db, "issue-9", old)
	outcomeID := wmSeedOutcome(t, db, "issue-9", "sha-9", 1.0, old)

	before, err := db.ReportWatermarks(ctx, since, until, FleetWide)
	if err != nil {
		t.Fatalf("ReportWatermarks before: %v", err)
	}

	// 🔴 VACUITY CONTROL, PART 1. UpdateQualityForOutcome is a NO-OP when the
	// value is unchanged (`if old == quality { return tx.Commit() }`) — it
	// commits and writes no history row. A fixture that "revised" 1.0 to 1.0
	// would exercise nothing and this test would pass for the wrong reason.
	const revised = 0.5
	historyBefore := qualityHistoryRows(t, db)

	if err := db.UpdateQualityForOutcome(ctx, outcomeID, revised, "ci_fail", "run-1"); err != nil {
		t.Fatalf("UpdateQualityForOutcome: %v", err)
	}

	// 🔴 VACUITY CONTROL, PART 2. Establish — independently of Watermarks — that
	// a history row was actually written. Only then does the watermark assertion
	// below mean anything.
	if got := qualityHistoryRows(t, db); got != historyBefore+1 {
		t.Fatalf("quality_history rows = %d, want %d: the revision was a NO-OP, so this "+
			"test would have proven nothing about the watermark", got, historyBefore+1)
	}

	after, err := db.ReportWatermarks(ctx, since, until, FleetWide)
	if err != nil {
		t.Fatalf("ReportWatermarks after: %v", err)
	}

	// (a) The negative half. No new rows, so the whole window block MUST hold
	// still. Compared as a struct so a field added later is covered automatically.
	if after.Window != before.Window {
		t.Errorf("the windowed sequences moved on an in-place quality revision: %+v -> %+v",
			before.Window, after.Window)
	}

	// (b) The positive half. The revision IS recoverable — but only here.
	if after.Ledgers.MaxQualityHistoryID <= before.Ledgers.MaxQualityHistoryID {
		t.Errorf("max_quality_history_id did not advance on a real quality revision: %d -> %d — "+
			"the manifest cannot see an in-place mutation",
			before.Ledgers.MaxQualityHistoryID, after.Ledgers.MaxQualityHistoryID)
	}
	if after.Ledgers.QualityHistoryCount != before.Ledgers.QualityHistoryCount+1 {
		t.Errorf("quality_history_count = %d, want %d",
			after.Ledgers.QualityHistoryCount, before.Ledgers.QualityHistoryCount+1)
	}
	// (c) And it is the ONLY ledger that moved. Without this, all four ledger
	// pairs could be reading quality_history and nothing would notice.
	if after.Ledgers.MaxRepriceRowAuditID != before.Ledgers.MaxRepriceRowAuditID ||
		after.Ledgers.MaxCostCorrectionAuditID != before.Ledgers.MaxCostCorrectionAuditID ||
		after.Ledgers.MaxRepoRepairRowAuditID != before.Ledgers.MaxRepoRepairRowAuditID {
		t.Errorf("a quality revision moved an unrelated ledger — the four reads are not "+
			"addressing four distinct tables: %+v -> %+v", before.Ledgers, after.Ledgers)
	}
}

// TestReportWatermarks_EachLedgerIsReadFromItsOwnTable is the arm that was
// missing, and its absence was structural rather than cosmetic.
//
// 🔴 THREE OF THE FOUR LEDGERS WERE ZERO IN EVERY FIXTURE. Only quality_history
// was ever exercised, so repointing reprice_row_audit, cost_correction_audit and
// repo_repair_row_audit at any other table — or at each other — left both full
// suites green. A transposed Scan target, a swapped max/count pair or a
// copy-pasted table name was invisible. `0 == 0` is agreement by coincidence,
// not by measurement.
//
// This writes ONE row to ONE ledger at a time and asserts that ledger's pair
// advanced AND that every other ledger held still. The cross-check is what makes
// it a test of ADDRESSING rather than of counting.
func TestReportWatermarks_EachLedgerIsReadFromItsOwnTable(t *testing.T) {
	// Direct SQL, matching the pattern in audit_append_only_test.go: these three
	// ledgers are written by long multi-step operations (Reprice, RepairRepo, a
	// /costs override) whose full fixtures would dominate this test without
	// making the addressing claim any stronger.
	ledgers := []struct {
		name   string
		insert string
		args   []any
		max    func(LedgerWatermarks) int64
		count  func(LedgerWatermarks) int64
	}{
		{
			"quality_history",
			`INSERT INTO quality_history (outcome_id, developer, issue_id, old_quality,
			    new_quality, reason, source_ref) VALUES (?,?,?,?,?,?,?)`,
			[]any{int64(1), "alice", "issue-1", 1.0, 0.5, "ci_fail", "sha-1"},
			func(l LedgerWatermarks) int64 { return l.MaxQualityHistoryID },
			func(l LedgerWatermarks) int64 { return l.QualityHistoryCount },
		},
		{
			"reprice_row_audit",
			`INSERT INTO reprice_row_audit (reprice_id, token_event_id, old_cost_micro,
			    old_price_version, old_billing_mode) VALUES (?,?,?,?,?)`,
			[]any{"run-1", int64(1), int64(1000), 1, "per_token"},
			func(l LedgerWatermarks) int64 { return l.MaxRepriceRowAuditID },
			func(l LedgerWatermarks) int64 { return l.RepriceRowAuditCount },
		},
		{
			"cost_correction_audit",
			`INSERT INTO cost_correction_audit (token_event_id, old_cost_micro,
			    new_cost_micro, actor, reason) VALUES (?,?,?,?,?)`,
			[]any{int64(1), int64(1000), int64(2000), "alice", "invoice reconciliation"},
			func(l LedgerWatermarks) int64 { return l.MaxCostCorrectionAuditID },
			func(l LedgerWatermarks) int64 { return l.CostCorrectionAuditCount },
		},
		{
			"repo_repair_row_audit",
			`INSERT INTO repo_repair_row_audit (repair_id, token_event_id, old_repo, new_repo)
			 VALUES (?,?,?,?)`,
			[]any{"rep-1", int64(1), "unqualified", "acme/tier"},
			func(l LedgerWatermarks) int64 { return l.MaxRepoRepairRowAuditID },
			func(l LedgerWatermarks) int64 { return l.RepoRepairRowAuditCount },
		},
	}

	for _, led := range ledgers {
		t.Run(led.name, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			since := time.Now().UTC().Add(-24 * time.Hour)

			before, err := db.ReportWatermarks(ctx, since, time.Time{}, FleetWide)
			if err != nil {
				t.Fatalf("ReportWatermarks before: %v", err)
			}
			if led.max(before.Ledgers) != 0 || led.count(before.Ledgers) != 0 {
				t.Fatalf("fixture precondition: %s starts at %d/%d, want 0/0",
					led.name, led.max(before.Ledgers), led.count(before.Ledgers))
			}

			res, err := db.db.ExecContext(ctx, led.insert, led.args...)
			if err != nil {
				t.Fatalf("insert into %s: %v", led.name, err)
			}
			// Control: an insert that wrote nothing would leave every watermark
			// still and make the assertions below vacuous.
			if n, err := res.RowsAffected(); err != nil || n != 1 {
				t.Fatalf("insert into %s affected %d rows (err %v), want 1", led.name, n, err)
			}

			after, err := db.ReportWatermarks(ctx, since, time.Time{}, FleetWide)
			if err != nil {
				t.Fatalf("ReportWatermarks after: %v", err)
			}
			if led.max(after.Ledgers) != 1 || led.count(after.Ledgers) != 1 {
				t.Errorf("%s = %d/%d after one insert, want 1/1 — this ledger's watermark is not "+
					"reading this ledger's table", led.name, led.max(after.Ledgers), led.count(after.Ledgers))
			}

			// 🔴 THE CROSS-CHECK. Every OTHER ledger must be untouched. This is what
			// makes the test prove ADDRESSING: if two fields read the same table,
			// writing to it moves both, and this arm reddens.
			for _, other := range ledgers {
				if other.name == led.name {
					continue
				}
				if other.max(after.Ledgers) != 0 || other.count(after.Ledgers) != 0 {
					t.Errorf("writing one row to %s also moved %s (%d/%d) — the two watermarks "+
						"are reading the same table", led.name, other.name,
						other.max(after.Ledgers), other.count(after.Ledgers))
				}
			}
			// The windowed block must be untouched too: a ledger write is not a
			// report row.
			if after.Window != before.Window {
				t.Errorf("writing to %s moved the windowed block: %+v -> %+v",
					led.name, before.Window, after.Window)
			}
		})
	}
}

// TestReportWatermarks_WindowAndScopeNarrowTheWindowedSequences pins that the two
// windowed reads apply BOTH the window and the repo scope. It does NOT claim to
// compare against a scoring read — an earlier comment here said it "pins that the
// two windowed reads use the SAME predicate the scoring reads use", which it
// never did, and which was false in any case (see
// TestReportWatermarks_TokenSideCoversTheAttributionBand).
func TestReportWatermarks_WindowAndScopeNarrowTheWindowedSequences(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	since := now.Add(-24 * time.Hour)

	wmSeedEvent(t, db, "issue-in", now.Add(-time.Hour))
	wmSeedEventRepo(t, db, "issue-other", "acme/other", now.Add(-time.Hour))
	// Beyond the attribution band, so it is outside the token side too.
	wmSeedEvent(t, db, "issue-old", since.Add(-AttributableWindow-48*time.Hour))
	// Outcomes in two repos, so the scope is exercised on BOTH sides — the
	// outcome side previously had no scoped assertion at all.
	wmSeedOutcome(t, db, "issue-in", "sha-in", 1.0, now.Add(-time.Hour))
	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "bob", IssueID: "issue-other", PRNumber: 2, Weight: 3, Quality: 1.0,
		MergeCommitSHA: "sha-other", Repo: "acme/other", Timestamp: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("InsertOutcome(other repo): %v", err)
	}

	fleet, err := db.ReportWatermarks(ctx, since, time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("ReportWatermarks fleet: %v", err)
	}
	if fleet.Window.TokenEventCount != 2 {
		t.Errorf("fleet-wide token_event_count = %d, want 2 (the pre-band row must be excluded)",
			fleet.Window.TokenEventCount)
	}
	if fleet.Window.OutcomeCount != 2 {
		t.Errorf("fleet-wide outcome_count = %d, want 2", fleet.Window.OutcomeCount)
	}

	scoped, err := db.ReportWatermarks(ctx, since, time.Time{}, RepoScope("acme/widgets"))
	if err != nil {
		t.Fatalf("ReportWatermarks scoped: %v", err)
	}
	if scoped.Window.TokenEventCount != 1 {
		t.Errorf("scoped token_event_count = %d, want 1 (the other repo must be excluded)",
			scoped.Window.TokenEventCount)
	}
	if scoped.Window.OutcomeCount != 1 {
		t.Errorf("scoped outcome_count = %d, want 1 — the scope must apply to the OUTCOME side too",
			scoped.Window.OutcomeCount)
	}
}

// TestReportWatermarks_LedgersIgnoreWindowAndScope establishes POSITIVELY that
// the mutation ledgers are neither windowed nor scoped.
//
// 🔴 THE EARLIER VERSION OF THIS ASSERTION WAS `0 == 0`. It compared a
// fleet-wide and a scoped read of quality_history on a fixture that never
// revised a quality, so both sides were zero and the comparison could not fail.
// Here a revision is made against an outcome in acme/other, and a read scoped to
// acme/widgets over a window that EXCLUDES the revision must still see it.
func TestReportWatermarks_LedgersIgnoreWindowAndScope(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	old := now.AddDate(0, -2, 0)

	// The outcome lives in acme/other and two months back.
	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "bob", IssueID: "issue-other", PRNumber: 2, Weight: 3, Quality: 1.0,
		MergeCommitSHA: "sha-other", Repo: "acme/other", Timestamp: old,
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}
	o, ok, err := db.OutcomeByMergeCommit(ctx, "sha-other")
	if err != nil || !ok {
		t.Fatalf("OutcomeByMergeCommit: ok=%v err=%v", ok, err)
	}
	// A row in the scoped repo so the scoped read is not trivially empty.
	wmSeedEvent(t, db, "issue-in", now.Add(-time.Hour))

	if err := db.UpdateQualityForOutcome(ctx, o.ID, 0.5, "ci_fail", "run-x"); err != nil {
		t.Fatalf("UpdateQualityForOutcome: %v", err)
	}
	// Vacuity control: the ledger really has a row, so a later zero would be a
	// failure rather than an empty fixture.
	if n := qualityHistoryRows(t, db); n != 1 {
		t.Fatalf("quality_history rows = %d, want 1 — the revision did not land", n)
	}

	// A window that starts AFTER the outcome, scoped to a DIFFERENT repo. Both
	// narrowings would hide the revision if they applied to the ledgers.
	scoped, err := db.ReportWatermarks(ctx, now.Add(-24*time.Hour), time.Time{}, RepoScope("acme/widgets"))
	if err != nil {
		t.Fatalf("ReportWatermarks scoped: %v", err)
	}
	if scoped.Ledgers.MaxQualityHistoryID != 1 || scoped.Ledgers.QualityHistoryCount != 1 {
		t.Errorf("scoped/narrow-window ledger read = %d/%d, want 1/1: the mutation ledgers must be "+
			"UNWINDOWED and UNSCOPED — a revision made today against another repo's June outcome "+
			"is exactly what this manifest exists to expose",
			scoped.Ledgers.MaxQualityHistoryID, scoped.Ledgers.QualityHistoryCount)
	}
	// And the windowed block genuinely IS narrowed on the same read — otherwise
	// the assertion above would be satisfied by nothing being narrowed at all.
	if scoped.Window.OutcomeCount != 0 {
		t.Errorf("control: scoped outcome_count = %d, want 0 — if the window/scope are not "+
			"narrowing anything, the ledger assertion above proves nothing", scoped.Window.OutcomeCount)
	}
}

// TestReportWatermarks_CountCatchesADeleteThatMaxCannot pins why BOTH signals
// are published. EraseDeveloper hard-deletes rows, and ids are never reissued,
// so deleting anything other than the highest-id row leaves MAX(id) exactly
// where it was. Only COUNT(*) moves.
//
// ⚠️ The fixture deletes the row with the LOWER id on purpose, and the precise
// claim matters: MAX is blind to the deletion of a NON-max row. Deleting the max
// row does lower MAX — a first draft of this test did exactly that and failed,
// which is the useful way to learn that "MAX cannot see a delete" is too strong.
func TestReportWatermarks_CountCatchesADeleteThatMaxCannot(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	since := now.Add(-24 * time.Hour)

	wmSeedEvent(t, db, "issue-drop", now.Add(-2*time.Hour)) // lower id — deleted
	wmSeedEvent(t, db, "issue-keep", now.Add(-time.Hour))   // MAX id — survives

	before, err := db.ReportWatermarks(ctx, since, time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("ReportWatermarks before: %v", err)
	}

	res, err := db.db.ExecContext(ctx, `DELETE FROM token_events WHERE issue_id = ?`, "issue-drop")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("delete removed %d rows (err %v), want 1 — the fixture deleted nothing", n, err)
	}

	after, err := db.ReportWatermarks(ctx, since, time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("ReportWatermarks after: %v", err)
	}
	if after.Window.MaxTokenEventID != before.Window.MaxTokenEventID {
		t.Errorf("max_token_event_id = %d, want %d unchanged — this test's premise is that MAX "+
			"cannot see the deletion of a non-max row",
			after.Window.MaxTokenEventID, before.Window.MaxTokenEventID)
	}
	if after.Window.TokenEventCount != before.Window.TokenEventCount-1 {
		t.Errorf("token_event_count = %d, want %d: COUNT is the only signal that sees a delete",
			after.Window.TokenEventCount, before.Window.TokenEventCount-1)
	}
}

// TestReportWatermarks_ReadsInOneSnapshot is a STRUCTURAL pin on the claim that
// all six ledgers are read in one transaction.
//
// The doc calls that "a CORRECTNESS REQUIREMENT, NOT TIDINESS", and the Store
// interface repeats it for alternate implementations — but nothing enforced it:
// moving the ledger statement off the transaction onto `d.db` passed both full
// suites, because a torn read only shows up under a concurrent commit that no
// unit test schedules. A race test here would be flaky; an AST pin is not.
//
// It asserts that ReportWatermarks' body issues NO query against `d.db`. Every
// read must go through the `tx` handle, which is what makes the six figures
// describe one instant.
func TestReportWatermarks_ReadsInOneSnapshot(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "watermarks.go", nil, 0)
	if err != nil {
		t.Fatalf("parse watermarks.go: %v", err)
	}

	var fn *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		if d, ok := n.(*ast.FuncDecl); ok && d.Name.Name == "ReportWatermarks" {
			fn = d
			return false
		}
		return true
	})
	// Control: the AST walk must actually find the function. A parser that
	// matched nothing would make every check below vacuously true.
	if fn == nil {
		t.Fatal("control arm: ReportWatermarks not found in watermarks.go — this pin is blind")
	}

	var offSnapshot []string
	var txCalls int
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !strings.HasPrefix(sel.Sel.Name, "Query") && !strings.HasPrefix(sel.Sel.Name, "Exec") {
			return true
		}
		// Receiver spelled `d.db` (the pool) rather than `tx` (the snapshot).
		if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "db" {
			offSnapshot = append(offSnapshot, sel.Sel.Name)
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "tx" {
			txCalls++
		}
		return true
	})

	// Control: the walk must find the transaction-bound reads it is measuring
	// against, or "no off-snapshot reads" is a statement about nothing.
	if txCalls == 0 {
		t.Fatal("control arm: found no tx-bound query in ReportWatermarks — the matcher is not " +
			"recognising the calls, so the off-snapshot check below proves nothing")
	}
	if len(offSnapshot) > 0 {
		t.Errorf("ReportWatermarks issues %v directly on the connection pool instead of the "+
			"transaction. Every ledger must be read inside ONE snapshot: read across connections, "+
			"a revision committing between two of them yields a manifest whose halves describe "+
			"database states that never coexisted.", offSnapshot)
	}
}

// TestWatermarks_CoverEveryLedgerInTheLiveSchema is the coverage pin, and its
// design is the point: one side is REFLECTED off the struct and the other is read
// from the LIVE sqlite_master schema. A hand-list on both sides would be a
// tautology that a newly added ledger could never redden.
//
// A new ledger table therefore fails this test until its author either
// watermarks it or adds it to deliberatelyUncovered with a reason — which is
// exactly the decision that must not be made silently.
func TestWatermarks_CoverEveryLedgerInTheLiveSchema(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	// classify runs the SAME switch used on the real struct below, so the control
	// arms exercise the actual matcher rather than a copy of it.
	classify := func(tag string) (prefix string, kind string) {
		name, _, _ := strings.Cut(tag, ",")
		switch {
		case strings.HasPrefix(name, "max_") && strings.HasSuffix(name, "_id"):
			return strings.TrimSuffix(strings.TrimPrefix(name, "max_"), "_id"), "max"
		// The `!HasPrefix("max_")` term is not decoration: without it
		// "max_reprice_row_audit_count" — a plausible typo that is neither a valid
		// max nor a valid count — falls through to the count branch and is
		// silently accepted under the prefix "max_reprice_row_audit", inventing a
		// ledger. The control arm below caught exactly that.
		case strings.HasSuffix(name, "_count") && !strings.HasPrefix(name, "max_"):
			return strings.TrimSuffix(name, "_count"), "count"
		}
		return "", ""
	}

	// --- found == 0 control, with teeth ---
	// 🔴 The earlier control was `haveMax["definitely_not_a_ledger"]` on a
	// map[string]bool. That returns false by LANGUAGE GUARANTEE whatever the
	// matcher does, so it could not fail and proved nothing. These feed
	// known-shaped-but-wrong tags through the real classifier and require it to
	// reject them.
	for _, bad := range []string{"max_reprice_row_audit_count", "reprice_row_audit_id", "developer", ""} {
		if _, kind := classify(bad); kind != "" {
			t.Fatalf("control arm: classifier accepted %q as a %s — it cannot distinguish, so "+
				"every classification below is untrustworthy", bad, kind)
		}
	}
	if _, kind := classify("max_x_id"); kind != "max" {
		t.Fatal("control arm: classifier rejected a well-formed max tag — it matches nothing")
	}

	// --- side 1: what the struct actually publishes, by reflection ---
	// A ledger counts as covered only when BOTH signals are present: a max
	// without a count cannot see a delete, and a count without a max is a weaker
	// insert signal.
	haveMax := map[string]bool{}
	haveCount := map[string]bool{}
	for _, rt := range []reflect.Type{
		reflect.TypeOf(WindowWatermarks{}),
		reflect.TypeOf(LedgerWatermarks{}),
	} {
		for i := range rt.NumField() {
			tag := rt.Field(i).Tag.Get("json")
			prefix, kind := classify(tag)
			switch kind {
			case "max":
				haveMax[prefix] = true
			case "count":
				haveCount[prefix] = true
			default:
				t.Errorf("%s field %s has json tag %q, which is neither a max_*_id nor a *_count — "+
					"this pin cannot classify it, so it would go unchecked", rt.Name(), rt.Field(i).Name, tag)
			}
		}
	}
	if len(haveMax) == 0 || len(haveCount) == 0 {
		t.Fatal("control arm: reflection found no max/count fields at all — the json tags moved " +
			"and this pin is blind")
	}

	// --- side 2: what the LIVE schema contains ---
	// GLOB, not LIKE: SQLite's LIKE is case-INSENSITIVE, so a future
	// `Quality_History` would match a lowercase LIKE pattern and quietly change
	// what this set means. GLOB is case-sensitive and mirrors Go's own matching.
	//
	// ⚠️ THE SUFFIX LIST IS THE PIN'S WEAK POINT AND IS DELIBERATELY WIDE. It
	// began as `*_audit` / `*_history` only, and `quality_events` — an
	// append-only signal log that the schema itself describes as the thing
	// quality is DERIVED from — slipped through unadjudicated. A ledger named
	// `*_log` or `*_journal` would have too. Widening the net cannot make the pin
	// wrong; it can only force an explicit decision on one more table.
	//
	// `*_registry` was added on exactly that reasoning when #714 landed
	// price_table_registry. The `*_audit` term already caught its ledger sibling
	// price_forget_audit and forced that decision; the registry itself would have
	// slipped through on a SUFFIX ACCIDENT while being the more interesting of the
	// two — it is mutated IN PLACE by the soft delete, which is the class this pin
	// is worst at seeing. Measured before widening: `*_registry` matches that one
	// table and nothing else, so this cost one adjudication and no churn.
	rows, err := db.db.Query(`
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND (
		      name GLOB '*_audit'    OR name GLOB '*_history' OR name GLOB '*_events'
		   OR name GLOB '*_log'      OR name GLOB '*_journal' OR name GLOB '*_membership'
		   OR name GLOB '*_registry')
		ORDER BY name`)
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var ledgers []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ledgers = append(ledgers, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	// --- found == 0 control for the schema query ---
	if len(ledgers) < len(watermarkLedgers) {
		t.Fatalf("control arm: sqlite_master returned only %d ledger tables (%v), fewer than the "+
			"%d this struct claims to cover — the schema query is not finding the ledgers, so the "+
			"coverage loop below is vacuous", len(ledgers), ledgers, len(watermarkLedgers))
	}
	var bogus int
	if err := db.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name GLOB '*_notaledger'`,
	).Scan(&bogus); err != nil {
		t.Fatalf("control query: %v", err)
	}
	if bogus != 0 {
		t.Fatalf("control arm: a deliberately bogus GLOB matched %d tables — the pattern is not "+
			"discriminating", bogus)
	}

	// Ledgers that are consciously NOT watermarked. Each needs a reason, because
	// the whole value of this pin is that skipping a ledger is a decision someone
	// had to make on purpose.
	deliberatelyUncovered := map[string]string{
		"reprice_audit": "aggregate per-RUN ledger; reprice_row_audit is the per-ROW twin, written " +
			"in the same transaction as every UPDATE, and it is covered",
		"repo_repair_audit": "aggregate per-RUN ledger; repo_repair_row_audit is the per-ROW twin and is covered",
		"quality_events": "the append-only SIGNAL log that quality is derived FROM, not a record of a " +
			"mutation. Every quality change it causes lands in quality_history, which IS covered, so " +
			"watermarking it would add a second signal for the same event",
		"period_membership": "UPDATEd in place (period_end) with no transition log, and a MAX(id)/COUNT(*) " +
			"pair CANNOT see an in-place UPDATE — a watermark here would be a guard that looks like a " +
			"guard and is not. Documented as an uncovered input on store.Watermarks instead",
		// --- #886 ---
		"hierarchy_membership": "the dated team/division ledger. Not watermarked yet: adding it changes the " +
			"tiermanifest1 ledger block, a wire change of its own. What it leaves uncovered is narrower than " +
			"org_hierarchy's gap: every row starts at its server-clock write time, so a membership write can " +
			"only re-group events at or after the write — it cannot move a window that ended before it, but " +
			"it can move a report whose window was still open when the write landed",
		// --- #714, adjudicated when price_table_registry / price_forget_audit landed ---
		"price_forget_audit": "NOT A REPORT INPUT. Retiring a price-table identity changes no cost, " +
			"score or outcome — only which table the startup guard will accept next, and the registry's " +
			"record of what a version meant. No scoring, aggregation or export path reads either #714 " +
			"table. The change it ENABLES (a different table registering under the reused version) is " +
			"already covered twice: the manifest stamps the ACTIVE price table's version and table_hash " +
			"(#713), and any actual reprice of stored rows lands in reprice_row_audit, which IS covered",
		"price_table_registry": "same non-input reason as price_forget_audit, plus the period_membership " +
			"reason: the #714 soft delete is an in-place UPDATE (forgotten_at/forgotten_by) on a row that " +
			"stays, so a MAX(id)/COUNT(*) pair cannot see a retirement at all. It is an identity table, " +
			"not a mutation ledger — the ledger for its mutations is price_forget_audit, above",
		// --- #975 ---
		"canonical_id_history": "NOT A REPORT INPUT. Only EraseDeveloper reads it, to find the sealed " +
			"person keys of a canonical id an alias edit retired; no scoring, aggregation or export path " +
			"reads it. The alias edits it records reach reports through developer_alias, not through it",
	}
	// The exclusion list must describe the LIVE schema too: a stale entry for a
	// renamed table would silently persist and quietly excuse its successor.
	for name := range deliberatelyUncovered {
		var n int
		if err := db.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?`, name,
		).Scan(&n); err != nil {
			t.Fatalf("lookup %s: %v", name, err)
		}
		if n != 1 {
			t.Errorf("deliberatelyUncovered names %q but the live schema has %d such table(s) — "+
				"a stale exclusion excuses nothing and hides its successor", name, n)
		}
	}

	for _, name := range ledgers {
		if reason, ok := deliberatelyUncovered[name]; ok {
			if reason == "" {
				t.Errorf("ledger %q is excluded with no reason", name)
			}
			continue
		}
		prefix, named := watermarkLedgers[name]
		if !named || !haveMax[prefix] || !haveCount[prefix] {
			t.Errorf("mutation ledger %q exists in the schema but Watermarks publishes max=%v count=%v. "+
				"Either watermark it (a report can change with no new row in the obvious sequences — that is "+
				"the whole finding behind #715), or add it to deliberatelyUncovered with a reason.",
				name, haveMax[prefix], haveCount[prefix])
		}
	}

	// The reverse direction: every ledger the struct claims must be a real table.
	// A renamed table would otherwise leave a phantom field publishing zeros
	// forever, which reads as "nothing has changed".
	for name, prefix := range watermarkLedgers {
		var n int
		if err := db.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?`, name,
		).Scan(&n); err != nil {
			t.Fatalf("lookup %s: %v", name, err)
		}
		if n != 1 {
			t.Errorf("Watermarks names ledger %q but the live schema has %d such table(s)", name, n)
		}
		if !haveMax[prefix] || !haveCount[prefix] {
			t.Errorf("watermarkLedgers maps %q -> %q but the struct publishes max=%v count=%v — "+
				"the map and the struct have drifted", name, prefix, haveMax[prefix], haveCount[prefix])
		}
	}
	// BOTH directions of the arity check. Only the max side was checked before,
	// so a stray count-only field (an "orphan_ledger_count" with no matching max)
	// reddened nothing — while the comment above claimed the pair requirement
	// stopped exactly that.
	if len(haveMax) != len(watermarkLedgers) {
		t.Errorf("Watermarks publishes %d max_*_id fields but watermarkLedgers names %d — "+
			"a field was added without updating the map the manifest documents",
			len(haveMax), len(watermarkLedgers))
	}
	if len(haveCount) != len(watermarkLedgers) {
		t.Errorf("Watermarks publishes %d *_count fields but watermarkLedgers names %d — "+
			"a count without a matching max is a half-guard", len(haveCount), len(watermarkLedgers))
	}
}

// TestWatermarks_EveryLedgerIsAutoincrement pins the schema property the
// verify-report removal checks rest on (#1032): every table in watermarkLedgers
// declares `id INTEGER PRIMARY KEY AUTOINCREMENT`, so no auto-assigned id can
// fall at or below a pinned MAX(id) — not even the id of a deleted maximal row.
// Without AUTOINCREMENT SQLite reuses MAX(id)+1, a new row can re-enter the
// pinned population, and counting `id <= watermark` would hide a removal.
func TestWatermarks_EveryLedgerIsAutoincrement(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	autoinc := regexp.MustCompile(`(?is)\bid\s+INTEGER\s+PRIMARY\s+KEY\s+AUTOINCREMENT\b`)
	// Control: the matcher must reject the near-miss it exists to catch.
	if autoinc.MatchString("CREATE TABLE x (id INTEGER PRIMARY KEY, v TEXT)") {
		t.Fatal("control arm: matcher accepted a plain INTEGER PRIMARY KEY")
	}
	for table := range watermarkLedgers {
		var ddl string
		if err := db.db.QueryRowContext(t.Context(),
			`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&ddl); err != nil {
			t.Fatalf("read %s DDL: %v", table, err)
		}
		if !autoinc.MatchString(stripSQLComments(ddl)) {
			t.Errorf("%s does not declare `id INTEGER PRIMARY KEY AUTOINCREMENT`; a new row could reuse a "+
				"pinned id and mask a removal.\nDDL:\n%s", table, ddl)
		}
	}
}
