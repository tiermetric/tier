package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// orderFixtureRepo is the repository every seeded outcome carries. It is a real
// slug rather than the 'unqualified' sentinel so the SCOPED AllOutcomesWindow arm
// below binds a value a production scope would actually bind.
const orderFixtureRepo = "acme/tier"

// orderProbe is one seeded outcome plus the row identity the DB assigned it.
type orderProbe struct {
	issue string
	ts    time.Time
	id    int64
}

// seedOrderFixture writes n outcomes for one developer whose ts values are
// DELIBERATELY not in insertion order, with a run of rows sharing one identical
// ts, and returns them keyed by issue_id together with the rowid SQLite
// assigned.
//
// The fixture is the whole point of the test, so it is built to be a real trap:
// the natural rowid scan order (which is what an unordered `SELECT ... FROM
// outcomes` returns today, by accident) must NOT equal the (ts, id) order, and
// the identical-ts run must be long enough that ts alone cannot break the tie.
// assertFixtureIsATrap below refuses to let the test proceed if either property
// is lost.
func seedOrderFixture(t *testing.T, db *DB, dev string) map[string]orderProbe {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	// Insertion index -> ts offset in minutes. Insertion order is 0..7, so rowid
	// order is 0..7; the offsets below put ts in a different order entirely, and
	// four rows (indices 1, 3, 4, 7) share the SAME instant.
	offsets := []int{50, 10, 30, 10, 10, 0, 40, 10}
	for i, off := range offsets {
		issue := fmt.Sprintf("ISSUE-%d", i)
		if _, err := db.InsertOutcome(ctx, Outcome{
			Developer: dev,
			IssueID:   issue,
			// Weight/Quality are the REAL columns whose product scoring sums in
			// arrival order; distinct values per row so a permutation is visible.
			Weight:         float64(i+1) * 0.1,
			Quality:        0.7 + float64(i)*0.03,
			MergeCommitSHA: "sha-" + dev + "-" + issue,
			Repo:           orderFixtureRepo,
			Timestamp:      base.Add(time.Duration(off) * time.Minute),
		}); err != nil {
			t.Fatalf("InsertOutcome %s: %v", issue, err)
		}
	}

	// Read back id and ts from the DB rather than assuming rowid == insertion
	// index: the assertions below are only meaningful against the identity the
	// storage engine actually assigned.
	probes := map[string]orderProbe{}
	rows, err := db.db.QueryContext(ctx, `SELECT id, issue_id, ts FROM outcomes WHERE developer = ?`, dev)
	if err != nil {
		t.Fatalf("read back ids: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p orderProbe
		if err := rows.Scan(&p.id, &p.issue, &p.ts); err != nil {
			t.Fatalf("scan probe: %v", err)
		}
		probes[p.issue] = p
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("probe rows: %v", err)
	}
	if len(probes) != len(offsets) {
		t.Fatalf("seeded %d outcomes but read back %d — the dedup index collapsed rows and the fixture is broken", len(offsets), len(probes))
	}

	// 🔴 ANALYZE IS THE TRIGGER, NOT DRESSING. Measured on modernc.org/sqlite
	// v1.48.0 with this schema: with no table statistics the planner serves
	// `WHERE ts >= ? AND ts < ?` from idx_outcomes_ts_id, so an UNORDERED read
	// already comes back in ts order and a mutation that deletes the ORDER BY
	// still passes. After ANALYZE the same statement plans as `SCAN outcomes` —
	// rowid order — which is exactly the silent plan flip #711 is about. Without
	// this line the assertions below are vacuous for AllOutcomesWindow;
	// assertReaderIsNotAccidentallyOrdered proves the flip actually happened.
	//
	// ⚠️ THE MECHANISM HAS A NAME: SQLITE_ENABLE_STAT4. Measured here, single
	// variable — `pragma_compile_options` on modernc.org/sqlite v1.48.0 reports
	// ENABLE_STAT4; ANALYZE writes sqlite_stat1 AND sqlite_stat4 sample rows, and
	// the plan goes SEARCH -> SCAN; `DELETE FROM sqlite_stat4` plus a statistics
	// reload puts it back to SEARCH. Without stat4 a two-sided `ts` range falls
	// back to SQLite's default range selectivity estimate (~1/64) so the index
	// always looks cheap; with it the planner samples the real histogram, sees this
	// window covers most of the table, and prices non-covering access to a
	// 17-column row above a scan. Two consequences for whoever reads a red here:
	//
	//   - A SQLite BUILD WITHOUT STAT4 KEEPS THE INDEX PLAN, and the controls
	//     below will fire. That is the guard reporting "this configuration cannot
	//     demonstrate #711", not a regression in the fix. (The macOS system
	//     `sqlite3` 3.51.0 is such a build — probing with it instead of the pinned
	//     pure-Go driver is what made a reviewer's first repro "refute" this.)
	//   - A NARROW probe table makes idx_outcomes_ts_id a COVERING index, which
	//     wins unconditionally. The real outcomes table is 17 columns and is never
	//     covering for these readers, so a hand-built schema cannot reproduce the
	//     flip either.
	if _, err := db.db.ExecContext(ctx, `ANALYZE`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
	return probes
}

// assertReaderIsNotAccidentallyOrdered fails if the reader's OWN predicate,
// stripped of its ORDER BY, already yields (ts, id) order. When it does, deleting
// scoringOrderSQL changes nothing observable and the WINDOW-SHAPED assertions in
// this file are satisfied by the query planner rather than by the fix.
//
// 🔴 IT REPORTS WITH t.Errorf AND RETURNS false RATHER THAN CALLING t.Fatalf, and
// the caller runs it immediately before the arms it actually guards. It probes
// ONE shape — `ts >= ? AND ts < ?` — so it speaks only for AllOutcomesSince and
// AllOutcomesWindow. DeveloperOutcomes (`developer = ? AND ts >= ?`) is served by
// idx_outcomes_developer in every configuration measured, so its arm is
// unconditionally non-vacuous; a Fatalf here would kill it, and ListOutcomes,
// over a plan move that says nothing about either. Failing loud without taking
// two permanently-meaningful arms down with it is the whole difference.
func assertReaderIsNotAccidentallyOrdered(t *testing.T, db *DB, probes map[string]orderProbe, since, until time.Time) bool {
	t.Helper()
	where, args := tsWindow(since, until)
	rows, err := db.db.QueryContext(context.Background(),
		`SELECT issue_id FROM outcomes WHERE `+where, args...)
	if err != nil {
		t.Fatalf("unordered probe: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []orderProbe
	for rows.Next() {
		var issue string
		if err := rows.Scan(&issue); err != nil {
			t.Fatalf("scan unordered probe: %v", err)
		}
		p, ok := probes[issue]
		if !ok {
			t.Fatalf("unordered probe returned unknown issue %q", issue)
		}
		got = append(got, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("unordered probe rows: %v", err)
	}
	if len(got) != len(probes) {
		t.Fatalf("unordered probe returned %d rows, want %d — the window does not cover the fixture", len(got), len(probes))
	}
	ordered := true
	for i := 1; i < len(got); i++ {
		if !lessTSID(got[i-1], got[i]) {
			ordered = false
			break
		}
	}
	if ordered {
		t.Errorf("vacuity control: AllOutcomesWindow's WHERE clause WITHOUT an ORDER BY already returned rows in (ts, id) order (plan: %s).\nThe planner is doing the fix's job by accident, so removing scoringOrderSQL would not change any result and the window-shaped arms below cannot fail. Fix the fixture (ANALYZE, row count, window width — and see the STAT4 note in seedOrderFixture) — do not weaken the assertion.",
			queryPlan(t, db, `SELECT issue_id FROM outcomes WHERE `+where, args...))
		return false
	}
	return true
}

// assertFixtureIsATrap fails if the fixture could be satisfied by an unordered
// read, which would make every ordering assertion in this file vacuous.
func assertFixtureIsATrap(t *testing.T, probes map[string]orderProbe) {
	t.Helper()

	// (1) at least one ts must be shared by 3+ rows, so id is doing real work.
	byTS := map[int64]int{}
	maxRun := 0
	for _, p := range probes {
		byTS[p.ts.UTC().UnixNano()]++
	}
	for _, n := range byTS {
		if n > maxRun {
			maxRun = n
		}
	}
	if maxRun < 3 {
		t.Fatalf("fixture: longest identical-ts run is %d (< 3) — ts alone would be a total order here and the (ts, id) tiebreak is untested", maxRun)
	}

	// (2) rowid order must differ from (ts, id) order, or a plain rowid scan
	// already satisfies the assertion and the ORDER BY proves nothing.
	byID := make([]orderProbe, 0, len(probes))
	for _, p := range probes {
		byID = append(byID, p)
	}
	sortProbes(byID, func(a, b orderProbe) bool { return a.id < b.id })
	byTSID := append([]orderProbe(nil), byID...)
	sortProbes(byTSID, lessTSID)
	same := true
	for i := range byID {
		if byID[i].issue != byTSID[i].issue {
			same = false
			break
		}
	}
	if same {
		t.Fatal("fixture: rowid order and (ts, id) order are IDENTICAL — an unordered rowid scan would pass, so this test cannot fail")
	}
}

func lessTSID(a, b orderProbe) bool {
	if !a.ts.Equal(b.ts) {
		return a.ts.Before(b.ts)
	}
	return a.id < b.id
}

// sortProbes is an insertion sort — tiny n, and it keeps the test free of any
// dependency on the sort package's stability semantics.
func sortProbes(s []orderProbe, less func(a, b orderProbe) bool) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && less(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// assertTotallyOrdered fails unless got is strictly ascending on (ts, id).
func assertTotallyOrdered(t *testing.T, name string, got []Outcome, probes map[string]orderProbe) {
	t.Helper()
	if len(got) != len(probes) {
		t.Fatalf("%s: returned %d outcomes, want %d — the reader is not seeing the whole fixture, so an ordering assertion over it is not meaningful", name, len(got), len(probes))
	}
	var prev orderProbe
	var have bool
	seq := make([]string, 0, len(got))
	for _, o := range got {
		p, ok := probes[o.IssueID]
		if !ok {
			t.Fatalf("%s: returned unknown issue %q", name, o.IssueID)
		}
		seq = append(seq, fmt.Sprintf("%s(ts=+%dm,id=%d)", p.issue, int(p.ts.Sub(p.ts.Truncate(time.Hour)).Minutes()), p.id))
		if have && !lessTSID(prev, p) {
			t.Errorf("%s: rows are NOT strictly ascending on (ts, id): %s(ts=%s,id=%d) came before %s(ts=%s,id=%d)\nfull sequence: %s",
				name, prev.issue, prev.ts.UTC().Format(time.RFC3339), prev.id,
				p.issue, p.ts.UTC().Format(time.RFC3339), p.id, strings.Join(seq, " "))
			return
		}
		prev, have = p, true
	}
}

// TestOutcomeReads_AreTotallyOrdered pins #711: every []Outcome read that feeds
// scoring must return rows in the TOTAL order (ts, id).
//
// It is not a style assertion. weight and quality are REAL columns and
// scoring.ComputeDeveloper sums their product in ARRIVAL order (float addition
// is not associative), and the same slice becomes the index domain of the
// fixed-seed bootstrap CI — see scoringOrderSQL in store.go, and
// TestBootstrapCI_IsPermutationSensitive in internal/scoring for the hazard
// itself.
//
// ts alone is not enough: outcomes routinely share an instant (a webhook batch,
// a backfill), so the fixture deliberately contains a four-row identical-ts run
// and refuses to run if that property is ever lost.
func TestOutcomeReads_AreTotallyOrdered(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	probes := seedOrderFixture(t, db, "alice")
	assertFixtureIsATrap(t, probes)

	since := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	// ── ARMS THAT ARE MEANINGFUL IN EVERY MEASURED CONFIGURATION ────────────
	// These two do not depend on the window-shaped query plan, so they run first
	// and unconditionally: DeveloperOutcomes is served by idx_outcomes_developer
	// and pays a temp b-tree for the order (see
	// TestAllOutcomesWindow_OrderIsIndexServed's note), and ListOutcomes carries
	// its own keyset ORDER BY. Ordering them ahead of the control is deliberate —
	// a plan move in the window readers must not stop them from reporting.
	devOut, err := db.DeveloperOutcomes(ctx, "alice", since)
	if err != nil {
		t.Fatalf("DeveloperOutcomes: %v", err)
	}
	assertTotallyOrdered(t, "DeveloperOutcomes", devOut, probes)

	// ListOutcomes already carried ORDER BY ts, id via keysetWindowSQL before
	// #711. Asserted here anyway so the bulk-export reader is covered by the
	// same invariant rather than by a claim in a comment.
	listOut, _, err := db.ListOutcomes(ctx, since, until, PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListOutcomes: %v", err)
	}
	assertTotallyOrdered(t, "ListOutcomes", listOut, probes)

	// ── WINDOW-SHAPED ARMS, GATED ON THE CONTROL THAT SPEAKS FOR THEM ───────
	// The control probes `ts >= ? AND ts < ?` — these readers' own shape. It
	// reports with Errorf and returns false, so a plan move fails the test with a
	// diagnostic instead of silently proving nothing, and the arms it cannot
	// vouch for are skipped rather than passing on the planner's coincidence.
	if !assertReaderIsNotAccidentallyOrdered(t, db, probes, since, until) {
		return
	}

	sinceOut, err := db.AllOutcomesSince(ctx, since)
	if err != nil {
		t.Fatalf("AllOutcomesSince: %v", err)
	}
	assertTotallyOrdered(t, "AllOutcomesSince", sinceOut, probes)

	winOut, err := db.AllOutcomesWindow(ctx, since, until, FleetWide)
	if err != nil {
		t.Fatalf("AllOutcomesWindow: %v", err)
	}
	assertTotallyOrdered(t, "AllOutcomesWindow", winOut, probes)

	// The SCOPED path is a different SQL string: AllOutcomesWindow concatenates
	// where + scopeSQL + scoringOrderSQL, so the repo predicate lands BETWEEN the
	// window predicate and the ORDER BY. A fleet-wide arm alone would not notice
	// a scope fragment that broke that concatenation, and /scores?repo= runs this
	// exact string (#590).
	scopedOut, err := db.AllOutcomesWindow(ctx, since, until, RepoScope(orderFixtureRepo))
	if err != nil {
		t.Fatalf("AllOutcomesWindow (scoped): %v", err)
	}
	assertTotallyOrdered(t, "AllOutcomesWindow(repo="+orderFixtureRepo+")", scopedOut, probes)
}

// TestAllOutcomesWindow_OrderIsIndexServed checks that the #711 ORDER BY is paid
// for by idx_outcomes_ts_id rather than by a materialized sort — the claim made
// in the issue and in scoringOrderSQL's comment. A sort would still be CORRECT,
// so this is a performance pin, not a correctness one; it exists because "no
// sort cost" was asserted in prose and nothing measured it.
//
// ⚠️ SCOPE, STATED HONESTLY: EXPLAIN QUERY PLAN needs SQL text, and the reader's
// statement is assembled inside AllOutcomesWindow, so this test rebuilds the same
// shape from tsWindow + scoringOrderSQL. It therefore pins the CONSTANT and the
// SCHEMA — "(ts, id) is index-servable on the outcomes table" — not the call
// site. TestOutcomeReads_AreTotallyOrdered is what pins the call sites, and it is
// the test that reddens when the tail is removed from a reader.
//
// ⚠️ The claim does NOT extend to DeveloperOutcomes, and an earlier version of
// this comment had the condition BACKWARDS. It said the temp b-tree is paid "at
// small/unanalyzed scale". Re-measured on this schema (5 developers round-robin,
// one row per minute, `WHERE developer = ? AND ts >= ?` + scoringOrderSQL):
//
//	n =    8 · no stats -> TEMP B-TREE   · ANALYZEd -> TEMP B-TREE
//	n =   40 · no stats -> TEMP B-TREE   · ANALYZEd -> TEMP B-TREE
//	n =  400 · no stats -> TEMP B-TREE   · ANALYZEd -> TEMP B-TREE
//	n = 2000 · no stats -> TEMP B-TREE   · ANALYZEd -> SEARCH idx_outcomes_ts_id
//	                                                   (NO sort)
//
// So the sort is paid at every size measured, ANALYZE or not — and where it is
// NOT paid, ANALYZE is what REMOVED it, by moving the plan onto the (ts, id)
// index. (A reviewer reported that same "no sort" cell on an 8-row fixture, which
// is the same effect at a different crossover point — so the cell turns on the
// collected statistics and the data shape, NOT on "small scale".)
// That reader has no production caller, so the cost is not on any hot path — but
// "no sort cost" is a claim about THIS query, not about all three.
func TestAllOutcomesWindow_OrderIsIndexServed(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// Enough rows, and enough of them inside the window, that the planner has a
	// real choice to make. A window holding a small minority of the table would
	// bias the planner toward the index regardless, which would make the
	// assertion pass for the wrong reason.
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	const total = 400
	for i := 0; i < total; i++ {
		issue := fmt.Sprintf("W-%d", i)
		if _, err := db.InsertOutcome(ctx, Outcome{
			Developer:      fmt.Sprintf("dev%d", i%5),
			IssueID:        issue,
			Weight:         1.0,
			Quality:        1.0,
			MergeCommitSHA: "sha-" + issue,
			Timestamp:      base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("InsertOutcome %s: %v", issue, err)
		}
	}
	if _, err := db.db.ExecContext(ctx, `ANALYZE`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
	// ~79% of rows inside the window (see #711's review note): a window that
	// excludes almost everything keeps the seek for the wrong reason.
	since := base
	until := base.Add(time.Duration(total*79/100) * time.Minute)

	where, args := tsWindow(since, until)
	plan := queryPlan(t, db, `SELECT developer, issue_id, weight, quality, ts FROM outcomes WHERE `+where+scoringOrderSQL, args...)
	if strings.Contains(strings.ToUpper(plan), "TEMP B-TREE FOR ORDER BY") {
		t.Errorf("AllOutcomesWindow planned as %q — the ORDER BY is being paid for with a materialized sort, not idx_outcomes_ts_id", plan)
	}
	if !strings.Contains(plan, "idx_outcomes_ts_id") {
		t.Errorf("AllOutcomesWindow planned as %q — expected it to be served by idx_outcomes_ts_id", plan)
	}

	// 🔴 CONTROL: EXPLAIN QUERY PLAN must be capable of SAYING "TEMP B-TREE FOR
	// ORDER BY" in this environment, or the negative assertion above is
	// vacuously satisfied by an instrument that never speaks.
	control := queryPlan(t, db, `SELECT developer FROM outcomes ORDER BY quality, weight`)
	if !strings.Contains(strings.ToUpper(control), "TEMP B-TREE FOR ORDER BY") {
		t.Fatalf("control: an ORDER BY over two unindexed REAL columns planned as %q, which does not mention a temp b-tree — EXPLAIN QUERY PLAN is not reporting sorts here, so the assertion above cannot fail and proves nothing", control)
	}
}
