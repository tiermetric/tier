package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// wantCompositionPlan is the plan the #333 DECISION rests on: a SEEK of the ts
// window on
// idx_token_events_ts_id, so the work is bounded by the WINDOW rather than by the
// table. (#333 is still OPEN pending the evidence being posted — this pins the
// measurement the decision was taken on, not a closure.) The index is NAMED, not just "some SEARCH": a seek on a different index
// is a different cost model (a skip-scan on idx_token_events_source_ts was
// observed at other seed spans), and naming it also catches a NEW index quietly
// taking over the query — including the 10-column ts-leading covering index #333
// rejected on size and insert cost, which keeps the SEEK and would otherwise slip
// through a shape-only assertion.
const wantCompositionPlan = "SEARCH token_events USING INDEX idx_token_events_ts_id"

// TestCostCompositionWindow_PlanStaysAWindowSeek pins the #333 decision in the
// only place it can be pinned deterministically: the query plan.
//
// #333 was decided WITHOUT an index — and is still OPEN, pending the evidence
// being posted — on the measurement that this read's cost is WINDOW-proportional (5.6 ms over a 1-day window, 133 ms over 30 days on the
// 172k-row dogfood snapshot) and that every index option traded that away. The
// tempting one — an index leading with (host, model), which removes the GROUP BY
// sort and looked 2x faster on a 30-day window — silently converts the ts SEEK
// into a full-table SCAN, making the query 3.3x SLOWER on a narrow window and
// unboundedly slower as token_events grows. token_events is append-only and
// retention is deferred (#141), so that trade is backwards.
//
// A latency assertion cannot be committed (it needs the dogfood DB), but the PLAN
// is exact and cheap. It EXPLAINs costCompositionStmt — the whole statement
// CostCompositionWindow actually executes, predicates included — and NOT a copy
// of it: a re-typed query cannot degrade when the original does, so a hand-copied
// plan test is a false green by construction. Measured, not assumed; see
// costCompositionStmt's doc for the mutant that proved it.
func TestCostCompositionWindow_PlanStaysAWindowSeek(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// Seed shape: a 38-day span against a 30-day window puts ~78% of rows inside
	// it, matching production (135,218/172,240 = 78.5%).
	//
	// That match is for readability, NOT load-bearing — measured, because an
	// earlier revision of this comment asserted a planner "cliff" that does not
	// exist here. Across the full grid {50, 300, 3000, 30000} rows x window
	// coverage {24%, 48%, 78%, 96%, 99%}, all 20 combinations plan SEARCH on the
	// clean schema, and all 20 plan SCAN once a (host, model, ts) index exists.
	// So the assertion holds and the control arm fires everywhere in that range.
	//
	// ⚠️ The cliff DOES appear if you add ANALYZE — with stats, 500 and 30000 rows
	// both flip to SCAN and this test fails for a reason that has nothing to do
	// with #333. Production never runs ANALYZE (nothing in this package or
	// cmd/tierd issues one), so the faithful case is also the stable one. Do not
	// "help" this test by adding it.
	const seedRows = 500
	const seedSpan = 38 * 24 * time.Hour
	const window = 30 * 24 * time.Hour

	base := time.Now().UTC().Add(-seedSpan)
	step := seedSpan / seedRows
	for i := 0; i < seedRows; i++ {
		ev := TokenEvent{
			Developer: "alice",
			IssueID:   "issue-1",
			Model:     "claude-opus-5",
			Host:      HostUnknown,
			InputTok:  10,
			OutputTok: 5,
			CostMicro: 100,
			Source:    "proxy",
			Fidelity:  "exact",
			Repo:      "acme/widgets",
			Timestamp: base.Add(time.Duration(i) * step),
		}
		if err := db.InsertTokenEvent(ctx, ev); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
	since := time.Now().UTC().Add(-window)

	// Sanity-check the premise rather than the selectivity: a window that caught
	// NOTHING (or everything, with no rows outside) would make a "seeks the
	// window" assertion vacuous no matter what the planner did.
	var inWindow, total int
	if err := db.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM token_events").Scan(&total); err != nil {
		t.Fatalf("count total: %v", err)
	}
	if err := db.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM token_events WHERE ts >= ?", since).Scan(&inWindow); err != nil {
		t.Fatalf("count in window: %v", err)
	}
	if inWindow == 0 || inWindow == total {
		t.Fatalf("seed does not straddle the window (%d of %d rows inside); a window "+
			"predicate that selects nothing or everything makes this test vacuous",
			inWindow, total)
	}

	for _, tc := range []struct {
		name  string
		scope RepoScope
	}{
		{"fleet-wide", FleetWide},
		// #590 scoped read: a different statement (trailing "AND repo = ?"), so it
		// gets its own plan. Nothing else in the suite plans this variant.
		{"repo-scoped", RepoScope("acme/widgets")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The WHOLE statement production will run, predicates included —
			// see costCompositionStmt's doc for why the seam must sit there.
			stmt, args := costCompositionStmt(since, time.Time{}, tc.scope)
			plan := explainPlan(t, db, stmt, args...)
			if !strings.Contains(plan, wantCompositionPlan) {
				t.Errorf("cost-composition no longer seeks the ts window on the expected index.\n"+
					"  got:  %s\n  want: a plan containing %q\n"+
					"the #333 decision rests on the measurement that this read's cost stays "+
					"window-proportional. A plan that SCANs token_events makes it "+
					"table-proportional on an append-only table with deferred retention "+
					"(#141) — 3.3x slower on a 1-day window in the measurement, and worse "+
					"every month. If an index was added to speed up the 30-day window, "+
					"re-measure the NARROW window before keeping it.", plan, wantCompositionPlan)
			}
			if strings.Contains(plan, "SCAN token_events") {
				t.Errorf("cost-composition degraded to a full table/index SCAN; plan = %s", plan)
			}
		})
	}
}

// explainPlan returns the EXPLAIN QUERY PLAN rows for stmt, joined for assertion.
func explainPlan(t *testing.T, db *DB, stmt string, args ...any) string {
	t.Helper()
	rows, err := db.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+stmt, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	if len(plan) == 0 {
		t.Fatal("EXPLAIN QUERY PLAN returned no rows — the test cannot conclude anything")
	}
	return strings.Join(plan, " | ")
}
