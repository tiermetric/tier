package store

// Tests for #673 — ExportDeveloper's reads must share ONE snapshot, so a
// concurrent EraseDeveloper cannot tear a GDPR Art. 15 subject-access export.
//
// 🔴 THE POOL SIZE NEVER GATED THIS, AND THIS HEADER SAID THE OPPOSITE UNTIL
// REVIEW MEASURED IT. It claimed that at SetMaxOpenConns(1) the reads and the
// erase "queued on the one connection" so the tear "needed a second PROCESS".
// FALSE, and inverted. Against the fully unfixed code, 2000 iterations per run,
// varying ONLY maxOpenConns:
//
//	maxOpenConns = 4   ->  668 / 676 / 719 torn   (~34%)
//	maxOpenConns = 1   ->  1977 / 2000 torn       (~99%, first tear at iteration 0)
//
// ⭐ Each read is a separate QueryContext that RETURNS ITS CONNECTION when
// rows.Close() runs, so one connection was never a lock held across the nine
// reads — it was a single slot handed to the writer BETWEEN every read. Pool size
// changes the interleaving pattern, never the window. ⛔ Never "lower
// maxOpenConns to reduce tearing": at 1 it is near-total. This was a live
// single-process defect for as long as the method has existed; #669 prompted the
// audit, it did not open the hole.
//
// ⚠️ WHAT MAKES THE FAILURE WORTH A TEST RATHER THAN A COMMENT: it is SILENT.
// A torn export returns 200 with a well-formed body. Some tables reflect the
// pre-erase state, others the post-erase state, and nothing in the artifact says
// which. The subject receives a partial answer presented as complete.
//
// 🔑 THE INVARIANT THESE ARMS ASSERT, and it is deliberately not "the export is
// correct": with the subject's rows in ALL NINE exported tables written and
// deleted ATOMICALLY (one write transaction each way), every snapshot of the
// database has those nine tables either all populated or all empty. So an export
// reporting some populated and some empty read two different database states,
// and that is a tear by construction — no timing assumption, no sampling, no
// flake.
//
// ⚠️ ALL NINE, not a representative pair, and that is not caution — it is
// MEASURED. This file's first draft asserted over token_events and outcomes
// alone; a mutant reverting exactly ONE other read (quality_history) to the pool
// SURVIVED it. See subjectSeedStatements.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// subjectSeedStatements is one INSERT per table ExportDeveloper reads, each
// taking the developer identifier as its only bound argument.
//
// 🔴 IT MUST COVER EVERY EXPORTED TABLE, AND A MUTANT PROVED WHY. An earlier
// draft of this test seeded only token_events and outcomes. Reverting ALL NINE
// export reads to the pool was caught — but reverting exactly ONE (quality_history)
// SURVIVED, because no arm looked at that table. That is this project's
// "hardening applied to 3 of the 5 sites it scoped" shape, inside the very test
// written to catch it. ⇒ The seed list and the export's read list must be the
// SAME list, which TestExportedTablesAreAllSeeded asserts against the source.
//
// ⚠️ developer_alias is seeded with an alias POINTING AT the subject, which also
// widens the resolved identifier set to two ids — so the identifier resolution
// itself (the export's first read, and one that runs before the others) is inside
// the covered set rather than assumed.
var subjectSeedStatements = []struct{ table, stmt string }{
	{"token_events", `INSERT INTO token_events (developer, issue_id, model, cost_micro, source, fidelity)
		VALUES (?, 'issue-673', 'claude-sonnet-4', 1000, 'proxy', 'realtime')`},
	{"outcomes", `INSERT INTO outcomes (developer, issue_id, weight) VALUES (?, 'issue-673', 1.0)`},
	{"actual_spend", `INSERT INTO actual_spend (developer, period, actual_paid_micro) VALUES (?, '2026-08', 200000000)`},
	{"org_hierarchy", `INSERT INTO org_hierarchy (developer, team, division, org) VALUES (?, 'core', 'platform', 'acme')`},
	{"period_membership", `INSERT INTO period_membership (developer, org, period_start) VALUES (?, 'acme', '2026-08')`},
	{"quality_events", `INSERT INTO quality_events (outcome_id, developer, issue_id, event_type, source_ref, event_ts)
		VALUES (1, ?, 'issue-673', 'ci_pass', 'sha-673:1', CURRENT_TIMESTAMP)`},
	{"quality_history", `INSERT INTO quality_history (outcome_id, developer, issue_id, old_quality, new_quality, reason, source_ref)
		VALUES (1, ?, 'issue-673', 1.0, 0.5, 'ci_fail', 'sha-673:1')`},
	{"repo_repair_audit", `INSERT INTO repo_repair_audit (repair_id, developer, from_repo, to_repo, row_count, cost_micro_sum, tool_version)
		VALUES ('repair-673', ?, 'unqualified', 'acme/tier', 1, 1000, 'test')`},
	{"developer_alias", `INSERT INTO developer_alias (alias, canonical) VALUES ('alias-of-' || ?, ?)`},
	// 🔴 THE ALIAS-OWNED ROW, AND IT IS NOT A DUPLICATE OF THE FIRST ENTRY.
	// It is a SECOND token_events row stored under the ALIAS identifier rather
	// than the canonical one, written in the SAME transaction as everything else.
	//
	// WHY IT EXISTS: without it, reverting ExportDeveloper's
	// `developerIdentifierSet(ctx, tx, id)` back to `d.db` — leaving all nine
	// table reads on the transaction — PASSED EVERY ARM IN THIS FILE. Every row
	// was stored under the canonical id, so losing the alias never changed any
	// table's populated/empty state; the snapshot simply shifted to the first
	// queryRows. With this row present the mutant is caught: the resolved set
	// drops to [alice], its row goes undisclosed, and token_events reads 1 —
	// a count no consistent snapshot can produce.
	//
	// ⚠️ THAT FAILURE MODE IS THE WORST ONE THIS METHOD HAS: rows belonging to an
	// alias silently OMITTED from a subject-access response, with the alias also
	// missing from Identifiers, returned 200 and looking complete.
	{"token_events", `INSERT INTO token_events (developer, issue_id, model, cost_micro, source, fidelity)
		VALUES ('alias-of-' || ?, 'issue-673-alias', 'claude-sonnet-4', 1000, 'proxy', 'realtime')`},
}

// seededRowsPerTable is the number of rows each EXPORTED table holds after one
// seedSubject, keyed by the export's own table names.
//
// 🔑 token_events holds TWO — one under the canonical id and one under the alias
// — which is what makes the identifier-resolution read observable. Asserting the
// EXACT count rather than "non-empty" is what turns a torn identifier set into a
// failure instead of a shrug.
var seededRowsPerTable = map[string]int{
	"token_events": 2, "outcomes": 1, "actual_spend": 1, "org_hierarchy": 1,
	"period_membership": 1, "quality_events": 1, "quality_history": 1,
	"repo_repair_audit": 1, "developer_alias": 1,
}

// seedSubjectAtomically writes one row in EVERY table ExportDeveloper reads,
// inside a SINGLE write transaction.
//
// 🔴 THE ATOMICITY IS LOAD-BEARING, NOT TIDINESS. If the inserts committed
// separately, an export could legitimately observe some tables populated and
// others empty — a real intermediate state of the database — and the tear
// assertion below would fire on correct code. One transaction removes those
// states from existence, so the ONLY way to observe a mismatch is to read two
// different snapshots.
func seedSubjectAtomically(t *testing.T, db *DB, developer string) {
	t.Helper()
	if err := seedSubject(db, developer); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// seedSubject is seedSubjectAtomically's error-returning core, so the concurrent
// writer goroutine below can use it without calling t.Fatalf off the test
// goroutine (which is a vet error and, worse, does not stop the test).
func seedSubject(db *DB, developer string) error {
	ctx := context.Background()
	tx, err := beginImmediate(ctx, db.db)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	for _, s := range subjectSeedStatements {
		args := []any{developer}
		if s.table == "developer_alias" {
			args = append(args, developer)
		}
		if _, err := tx.ExecContext(ctx, s.stmt, args...); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insert %s: %w", s.table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// exportedTableCounts reports how many rows each export table returned, in the
// SAME table order as subjectSeedStatements, so a tear can be named by table.
func exportedTableCounts(exp DeveloperExport) map[string]int {
	return map[string]int{
		"token_events":      len(exp.TokenEvents),
		"outcomes":          len(exp.Outcomes),
		"actual_spend":      len(exp.ActualSpend),
		"org_hierarchy":     len(exp.OrgHierarchy),
		"period_membership": len(exp.PeriodMembership),
		"quality_events":    len(exp.QualityEvents),
		"quality_history":   len(exp.QualityHistory),
		"repo_repair_audit": len(exp.RepoRepairAudit),
		"developer_alias":   len(exp.DeveloperAlias),
	}
}

// TestExportedTablesAreAllSeeded pins the seed list against the export STRUCT BY
// REFLECTION, so a table added to DeveloperExport without a seed statement fails
// here rather than silently shipping with no tear coverage.
//
// 🔴 THE HAND-LIST VERSION OF THIS TEST DID NOT WORK, AND ITS DOC COMMENT SAID IT
// DID. It compared subjectSeedStatements against exportedTableCounts — a SECOND
// hand-maintained list thirty lines away — plus a literal `len == 9`. Measured:
// adding a tenth slice field to DeveloperExport and changing nothing else left it
// GREEN, because `counted` IS the stale list and the tripwire counted that map
// rather than the struct. ⇒ A GUARD WRITTEN TO CATCH SET-DRIFT WAS BLIND IN THE
// EXACT DIRECTION IT NAMED. The struct is now the single source.
//
// ⚠️ It matters beyond this file: DeveloperExport.RowCount() hand-lists the same
// slices, and the API maps RowCount()==0 to 404 — so a tenth PII table would be
// read and serialised but excluded from the count, and a subject with data ONLY
// in that table would get "not found" for an Art. 15 request. That is filed as
// its own issue; this test is what would have surfaced it.
func TestExportedTablesAreAllSeeded(t *testing.T) {
	t.Parallel()

	seeded := map[string]bool{}
	for _, s := range subjectSeedStatements {
		seeded[s.table] = true
	}
	counted := exportedTableCounts(DeveloperExport{})

	// Reflect over the STRUCT — the authority — rather than over either list.
	rt := reflect.TypeOf(DeveloperExport{})
	found := 0
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		// Identifiers is the resolved id set, not a stored table.
		if f.Type.Kind() != reflect.Slice || f.Name == "Identifiers" {
			continue
		}
		found++
		table := f.Tag.Get("json")
		if !seeded[table] {
			t.Errorf("DeveloperExport.%s (json %q) is an exported table with NO subjectSeedStatements entry — the tear test cannot observe it, so a read left on the pool there would survive every arm in this file. Add a seed statement in the same commit that adds the field", f.Name, table)
		}
		if _, ok := counted[table]; !ok {
			t.Errorf("DeveloperExport.%s (json %q) is not in exportedTableCounts — the tear assertion reads that map, so this table's rows are invisible to it", f.Name, table)
		}
	}

	// 🔴 CONTROL: reflection must actually find the fields. A walk that matched
	// nothing makes every check above vacuous in the "missing" direction — the
	// precise failure this test was rewritten to remove.
	if found == 0 {
		t.Fatal("control: reflection over DeveloperExport found ZERO table slices — the walk is not matching, so every assertion above proves nothing")
	}
	if found != len(counted) {
		t.Errorf("reflection found %d exported table slices but exportedTableCounts covers %d — the map has drifted from the struct", found, len(counted))
	}
	for table := range seeded {
		if _, ok := counted[table]; !ok {
			t.Errorf("subjectSeedStatements seeds %q but exportedTableCounts does not count it — the seed is doing work no assertion reads", table)
		}
	}
}

// countSeededTables reports the quiescent row count for the subject in EVERY
// seeded table, so the control arms prove the fixture across the same set the
// tear assertion reads — not a subset of it.
//
// ⚠️ TWO TABLES NEED THEIR OWN PREDICATE, for different reasons. developer_alias
// is keyed by `canonical`, not `developer`. And token_events holds rows under
// BOTH the canonical id and the alias, so counting only `developer = ?` would
// miss the alias-owned row — the one row that makes the identifier-resolution
// read observable at all.
func countSeededTables(t *testing.T, db *DB, developer string) map[string]int {
	t.Helper()
	ctx := context.Background()
	out := map[string]int{}
	for _, s := range subjectSeedStatements {
		if _, done := out[s.table]; done {
			continue // token_events has two seed statements; count it once
		}
		query := `SELECT COUNT(*) FROM ` + s.table + ` WHERE developer = ? OR developer = 'alias-of-' || ?`
		if s.table == "developer_alias" {
			query = `SELECT COUNT(*) FROM developer_alias WHERE canonical = ? OR canonical = ?`
		}
		var n int
		if err := db.db.QueryRowContext(ctx, query, developer, developer).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", s.table, err)
		}
		out[s.table] = n
	}
	return out
}

// assertAllSeededTablesHold fails unless every seeded table holds its EXACT
// expected row count — seededRowsPerTable when seeded, zero when erased.
//
// 🔑 EXACT, NOT "NON-EMPTY". token_events holds two rows (canonical + alias), and
// a count of ONE there is the signature of a torn identifier set: the alias was
// resolved from a different snapshot than the rows. "Non-empty" cannot see it.
func assertAllSeededTablesHold(t *testing.T, db *DB, developer string, seeded bool, why string) {
	t.Helper()
	for table, n := range countSeededTables(t, db, developer) {
		want := 0
		if seeded {
			want = seededRowsPerTable[table]
		}
		if n != want {
			t.Fatalf("control (%s): %s holds %d rows for %q, want %d — the fixture does not establish the all-or-nothing invariant across every exported table, so a mismatch below would be an artifact rather than a finding", why, table, n, developer, want)
		}
	}
}

// TestExportDeveloperIsNotTornByAConcurrentErase hammers ExportDeveloper against
// a writer that alternates atomic erase and atomic re-seed, and fails on any
// export that reports one table populated and the other empty.
//
// 🔬 MEASURED AGAINST THE UNFIXED IMPLEMENTATION (reads on d.db, no enclosing
// transaction): tears within the first few hundred iterations, every run of a
// 5× repeat. With the read transaction in place: 0 tears.
func TestExportDeveloperIsNotTornByAConcurrentErase(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	const developer = "alice"
	seedSubjectAtomically(t, db, developer)

	// 🔴 CONTROL: the invariant this test rests on must actually hold in the
	// QUIESCENT state, in EVERY seeded table. If the seed left any table out of
	// step, every "tear" below would be an artifact of the fixture rather than a
	// finding.
	assertAllSeededTablesHold(t, db, developer, true, "after an atomic seed")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	// cycles counts COMPLETED erase+re-seed rounds. It is the control that the
	// adversary actually ran — see the assertion after wg.Wait().
	var cycles int64

	// Writer: erase and re-seed, each atomically, as fast as it can.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := db.EraseDeveloper(ctx, developer); err != nil {
				// Contention is an expected, retryable outcome on this path;
				// anything else is a real failure and must not be swallowed.
				if !isWriteLockUnavailable(err) {
					t.Errorf("writer: EraseDeveloper: %v", err)
					return
				}
				continue
			}
			seedSubjectAtomicallyNoFatal(t, db, developer)
			atomic.AddInt64(&cycles, 1)
		}
	}()

	// Reader: export repeatedly and check the both-or-neither invariant.
	const iterations = 2000
	tears := 0
	var firstTear string
	for i := 0; i < iterations; i++ {
		exp, err := db.ExportDeveloper(ctx, developer)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("ExportDeveloper (iteration %d): %v", i, err)
		}
		// 🔑 THE INVARIANT, ACROSS ALL NINE TABLES, AND AT EXACT COUNTS: every
		// row is written and deleted in the same transaction as the others, so a
		// consistent snapshot shows every table at its seeded count OR every
		// table at zero. Anything else read two states.
		//
		// ⚠️ EXACT COUNTS, NOT "POPULATED vs EMPTY", and that is not fussiness.
		// token_events holds TWO rows — one canonical, one alias-owned. A count
		// of ONE there means the identifier set was resolved from a different
		// snapshot than the rows, which a populated/empty test reads as
		// perfectly healthy. That mutant (identifier resolution left on the
		// pool) SURVIVED the populated/empty version of this arm.
		counts := exportedTableCounts(exp)
		var wrong, zero []string
		for table, n := range counts {
			switch n {
			case seededRowsPerTable[table]:
				// fully present
			case 0:
				zero = append(zero, table)
			default:
				wrong = append(wrong, fmt.Sprintf("%s=%d(want %d)", table, n, seededRowsPerTable[table]))
			}
		}
		present := len(counts) - len(zero) - len(wrong)
		if len(wrong) > 0 || (len(zero) > 0 && present > 0) {
			tears++
			if firstTear == "" {
				sort.Strings(wrong)
				sort.Strings(zero)
				firstTear = fmt.Sprintf("iteration %d: partial-count=%v empty=%v fully-present=%d", i, wrong, zero, present)
			}
		}
	}
	close(stop)
	wg.Wait()

	// 🔴 THE CONTROL THIS ARM SHIPPED WITHOUT, AND IT IS THE WHOLE REASON THE
	// RESULT MEANS ANYTHING. Without it the test passes when the writer never
	// writes — PROVEN by two mutants: (a) a writer goroutine that returns before
	// its first cycle, and (b) beginImmediateBounded always returning the
	// contention sentinel, so every erase loses the race, the loop swallows it
	// and spins forever writing nothing. BOTH PASSED IN 0.45s.
	//
	// ⭐ That is this project's signature false-green — a control arm that cannot
	// fail — sitting inside the file whose own header warns about it. And it was
	// reachable without any mutant: a loaded or single-core machine that drove the
	// cycle count toward zero would produce an identical green.
	//
	// The floor is a tenth of what this machine measures (432–455 cycles per 2000
	// exports across 5 runs), so it tolerates a slow box while still failing a
	// writer that is not writing.
	n := atomic.LoadInt64(&cycles)
	t.Logf("writer completed %d erase/re-seed cycles during %d export iterations", n, iterations)
	if n < 40 {
		t.Fatalf("control: the writer completed only %d erase/re-seed cycles during %d export iterations — with no concurrent writer there is nothing to tear, so the %d-tear result below is VACUOUS (measured on healthy code: 432-455)", n, iterations, tears)
	}

	if tears > 0 {
		t.Errorf("ExportDeveloper produced %d TORN exports in %d iterations (first: %s) — the subject's rows are written and deleted atomically, so no single database state has some tables at their seeded count and others empty or partial. A torn export is two different snapshots stitched into one Art. 15 artifact, returned 200 and looking complete", tears, iterations, firstTear)
	}
}

// seedSubjectAtomicallyNoFatal is seedSubject for use OFF the test goroutine:
// t.Fatalf from a non-test goroutine is a vet error and, worse, does not stop
// the test, so failures are reported with t.Errorf and the goroutine returns.
// Losing the write-lock race is an expected outcome on this path and is retried,
// not reported.
func seedSubjectAtomicallyNoFatal(t *testing.T, db *DB, developer string) {
	t.Helper()
	if err := seedSubject(db, developer); err != nil && !isWriteLockUnavailable(err) {
		t.Errorf("writer: seed: %v", err)
	}
}

// TestAReadTransactionHoldsOneSnapshotAndAPooledReadDoesNot is the MECHANISM
// arm: it demonstrates, at the SQLite level and with no reference to
// ExportDeveloper, that the fix's premise is true and that the unfixed shape
// really does observe two states.
//
// 🔴 THIS IS THE CONTROL THE TEST ABOVE CANNOT BE. The hammer test's pass
// depends on the fix being present; if a future change silently made the read
// transaction a no-op (a driver that ignores it, a helper that hands back the
// pool), the hammer would simply stop tearing for the WRONG reason and still
// pass. This arm fails in that world, because its second half asserts the
// pooled read DOES tear.
func TestAReadTransactionHoldsOneSnapshotAndAPooledReadDoesNot(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	const developer = "bob"
	seedSubjectAtomically(t, db, developer)

	// ARM 1 — inside a read transaction: read token_events, let a concurrent
	// writer delete everything and COMMIT, then read outcomes. Both reads must
	// reflect the pre-delete snapshot.
	tx, release, err := beginRead(ctx, db.db)
	if err != nil {
		t.Fatalf("beginRead: %v", err)
	}
	firstInTx := countRowsVia(t, tx, `SELECT COUNT(*) FROM token_events WHERE developer = ?`, developer)
	if firstInTx != 1 {
		release()
		t.Fatalf("arm 1 setup: token_events inside the read tx = %d, want 1", firstInTx)
	}
	if _, err := db.EraseDeveloper(ctx, developer); err != nil {
		release()
		t.Fatalf("arm 1: EraseDeveloper: %v", err)
	}
	secondInTx := countRowsVia(t, tx, `SELECT COUNT(*) FROM outcomes WHERE developer = ?`, developer)
	release()
	if secondInTx != 1 {
		t.Errorf("arm 1: outcomes read INSIDE the read transaction = %d, want 1 — the transaction did not hold its snapshot across the concurrent commit, so wrapping ExportDeveloper's reads in it buys nothing", secondInTx)
	}

	// 🔴 CONTROL: the erase really did land, in every table. Without this, arm 1
	// could pass because nothing was ever deleted.
	assertAllSeededTablesHold(t, db, developer, false, "after EraseDeveloper")

	// ARM 2 — the UNFIXED shape, on the pool: the same two reads straddling the
	// same concurrent commit MUST observe different states. This is the arm that
	// must fail if the defect ever stops being real.
	seedSubjectAtomically(t, db, developer)
	firstPooled := countRowsVia(t, db.db, `SELECT COUNT(*) FROM token_events WHERE developer = ?`, developer)
	if _, err := db.EraseDeveloper(ctx, developer); err != nil {
		t.Fatalf("arm 2: EraseDeveloper: %v", err)
	}
	secondPooled := countRowsVia(t, db.db, `SELECT COUNT(*) FROM outcomes WHERE developer = ?`, developer)
	if firstPooled == secondPooled {
		t.Errorf("arm 2: two POOLED reads straddling a concurrent erase returned %d and %d — identical, so the non-transactional shape is not observably torn in this environment and arm 1 is proving nothing by contrast", firstPooled, secondPooled)
	}
}

// countRowsVia runs a single-row COUNT against any read surface (*sql.DB or
// *sql.Tx) so both arms above use the identical query path and differ ONLY in
// whether a transaction encloses them.
func countRowsVia(t *testing.T, q rowQuerier, query, arg string) int {
	t.Helper()
	rows, err := q.QueryContext(context.Background(), query, arg)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("count query returned no row")
	}
	var n int
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("scan count: %v", err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// exportReadPlanExpectations records, for EVERY table ExportDeveloper reads,
// whether its `developer IN (…)` read currently SEEKS an index or SCANS the whole
// table — measured, then frozen here so a change in either direction is visible.
//
// 🔴 FOUR OF THE NINE ARE FULL SCANS, AND THAT IS NOT A BUG IN THIS TEST — IT IS
// THE FINDING. quality_events, quality_history and repo_repair_audit have NO
// developer-leading index at all; period_membership has only an org-leading one.
//
// ⚠️ AN EARLIER VERSION OF THIS TEST CHECKED ONLY token_events — the ONE table
// where the seek holds — while its name and doc claimed the general property, and
// beginRead's doc block cited it to justify not bounding the export. Measured
// consequence, with a NINE-ROW subject: 200,000 OTHER developers' quality_events
// rows took one export from 153µs to 11.68ms, a 76.3× slowdown driven entirely by
// rows the subject does not own. ⇒ THE TRANSACTION IS TABLE-SIZE-BOUNDED, and the
// WAL argument in beginRead had to be rewritten around it.
//
// ⭐ The scans are recorded as EXPECTED so this test pins today's truth rather
// than an aspiration. Adding the missing indexes (#679) flips entries to true and
// fails here — deliberately, because that changes beginRead's bound argument and
// its doc block must be updated in the same commit.
var exportReadPlanExpectations = []struct {
	table     string
	seeks     bool
	predicate string
}{
	{"token_events", true, `developer IN (?,?)`},
	{"outcomes", true, `developer IN (?,?)`},
	{"actual_spend", true, `developer IN (?,?)`},
	{"org_hierarchy", true, `developer IN (?,?)`},
	{"period_membership", false, `developer IN (?,?)`},
	{"quality_events", false, `developer IN (?,?)`},
	{"quality_history", false, `developer IN (?,?)`},
	{"repo_repair_audit", false, `developer IN (?,?)`},
	// Whole-table BY DESIGN: developerIdentifierSet reads the entire alias map.
	{"developer_alias", false, `canonical = ?`},
}

// TestExportReadPlansMatchTheRecordedBound pins the query plan of EVERY export
// read against exportReadPlanExpectations, because beginRead's doc block reasons
// about the transaction's duration and those plans ARE the duration.
//
// ⚠️ TWO PLACEHOLDERS, NOT ONE. The real export builds its IN-clause from the
// resolved identifier set, and this file's fixture deliberately seeds an alias, so
// production runs `IN (?,?)`. The earlier test planned `IN (?)` — a shape the
// method does not execute when an alias exists, which is the case the fixture was
// built to create.
func TestExportReadPlansMatchTheRecordedBound(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	checked := 0
	for _, e := range exportReadPlanExpectations {
		args := []any{"alice", "alias-of-alice"}
		if e.table == "developer_alias" {
			args = []any{"alice"}
		}
		plan := queryPlan(t, db, `SELECT * FROM `+e.table+` WHERE `+e.predicate, args...)
		checked++
		scans := strings.Contains(plan, "SCAN "+e.table)
		switch {
		case e.seeks && scans:
			t.Errorf("%s: the export read now plans as a FULL SCAN (%q) but is recorded as a SEEK. An index that used to bound this read is gone, so the export transaction is now proportional to the table rather than the subject — and beginRead's doc block reasons about exactly that. Restore the index or update both the expectation AND that doc block", e.table, plan)
		case !e.seeks && !scans:
			t.Errorf("%s: the export read no longer plans as a full scan (%q) — that is an IMPROVEMENT, and it must be recorded rather than silently absorbed, because beginRead's doc block states this table is table-size-bounded. Flip seeks to true here and update that block in the same commit", e.table, plan)
		}
	}

	// 🔴 CONTROL 1: every expectation must have been exercised. A loop that
	// planned nothing would report no failures and look identical to a pass.
	if checked != len(exportReadPlanExpectations) || checked != 9 {
		t.Fatalf("control: planned %d reads, expected 9 — the loop did not cover every export read, so a regression in an unplanned table is invisible", checked)
	}

	// 🔴 CONTROL 2: EXPLAIN QUERY PLAN must be capable of SAYING "SCAN", or every
	// seeks==true assertion above is satisfied by an instrument that never speaks.
	control := queryPlan(t, db, `SELECT COUNT(*) FROM token_events WHERE model = ?`, "claude-sonnet-4")
	if !strings.Contains(control, "SCAN") {
		t.Fatalf("control: a query with no usable index planned as %q, which does not contain \"SCAN\" — EXPLAIN QUERY PLAN is not reporting scans in this environment, so the seek assertions above cannot fail and prove nothing", control)
	}

	// 🔴 CONTROL 3: and it must be capable of NOT saying SCAN, or every
	// seeks==false assertion is vacuously satisfied.
	seekControl := queryPlan(t, db, `SELECT COUNT(*) FROM token_events WHERE developer = ?`, "alice")
	if strings.Contains(seekControl, "SCAN token_events") {
		t.Fatalf("control: an indexed developer lookup planned as %q — the planner is scanning even where an index exists, so the seeks==false expectations above cannot fail either", seekControl)
	}
}

// queryPlan returns the concatenated EXPLAIN QUERY PLAN detail rows for query.
func queryPlan(t *testing.T, db *DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("EXPLAIN QUERY PLAN returned NO rows for %q — an empty plan makes every Contains check below vacuously false and every negative check vacuously true", query)
	}
	return strings.Join(out, " | ")
}

// TestPruneWebhookPayloadsIsOneTransaction pins #673's second half: the two
// retention DELETEs must not be independently visible.
//
// ⚠️ The only in-tree caller is Open(), before the process serves anything, so
// this is not reachable today. It is pinned because the method is EXPORTED — a
// future caller inherits the gap, and nothing else in the tree would tell them.
func TestPruneWebhookPayloadsIsOneTransaction(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// A row old enough for the age pass to delete.
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO webhook_payloads (event, delivery_id, body_gz, body_sha256, received_at)
		VALUES ('push', 'old-1', X'00', 'deadbeef', datetime('now', '-999 days'))`); err != nil {
		t.Fatalf("seed old payload: %v", err)
	}
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO webhook_payloads (event, delivery_id, body_gz, body_sha256)
		VALUES ('push', 'fresh-1', X'00', 'cafebabe')`); err != nil {
		t.Fatalf("seed fresh payload: %v", err)
	}

	n, err := db.PruneWebhookPayloads(ctx)
	if err != nil {
		t.Fatalf("PruneWebhookPayloads: %v", err)
	}
	if n != 1 {
		t.Errorf("PruneWebhookPayloads deleted %d rows, want 1 (the aged row only) — the counts are gathered inside the transaction, so a wrong total means the wrong rows moved", n)
	}

	var remaining int
	if err := db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM webhook_payloads`).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 1 {
		t.Errorf("webhook_payloads holds %d rows after the prune, want 1 — the fresh row must survive both passes", remaining)
	}

	// 🔴 SOURCE-LEVEL ARM: the count assertions above pass identically whether
	// the two DELETEs share a transaction or not, because Open()-time is
	// uncontended. What actually needs pinning is the ENCLOSURE, and the census
	// in deferred_conversion_test.go is what sees it — PruneWebhookPayloads is
	// listed in its wantUnbounded set, so removing the transaction fails there.
	// This note exists so a reader does not mistake the counts for that proof.
}

// TestBeginReadAlwaysReturnsItsConnection pins the release contract: a read
// transaction that is never rolled back holds a pool slot forever, and at
// maxOpenConns=4 that is four exports from a dead pool.
//
// 🔬 MUTANT: delete the tx.Rollback() from beginRead's release func. The loop
// below then exhausts the pool at i=maxOpenConns and the WATCHDOG fires with a
// named diagnosis in ~10s.
//
// 🔴 THE WATCHDOG IS NOT BELT-AND-BRACES; IT IS THE ONLY THING THAT MAKES THIS
// TEST LEGIBLE, AND THE OBVIOUS ALTERNATIVE IS WORSE THAN USELESS.
//
//   - WITHOUT it (the first draft): the leaked pool blocks the 5th beginRead
//     INSIDE the loop on an unbounded context. Execution never reaches any
//     assertion. The failure is `go test`'s PACKAGE-WIDE timeout panic — which
//     kills every other test in internal/store and reports no diagnosis. The
//     comment here claimed it "blocks until its deadline and fails"; measured, it
//     hangs the binary for the full -timeout instead.
//   - ⛔ WITH the obvious fix — bounding the ACQUISITION via
//     beginRead(context.WithTimeout(...), ...) — THE MUTANT PASSES. Cancelling
//     the context a transaction was begun with makes database/sql's awaitDone
//     roll it back and hand the connection back, so the leak repairs itself and
//     the guard sees a healthy pool. A fix that makes the test green against the
//     bug it exists to catch is the false-green this file is about.
//
// ⇒ The parent context stays UNCANCELLED and a separate goroutine watches the
// whole loop. MEASURED: leak mutant -> FAIL at 10.01s naming release(); healthy
// code -> ok in 0.21s.
func TestBeginReadAlwaysReturnsItsConnection(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// Open and release more read transactions than the pool has connections. If
	// release leaks, the (maxOpenConns+1)-th acquisition never returns.
	looped := make(chan struct{})
	go func() {
		defer close(looped)
		for i := 0; i < maxOpenConns*3; i++ {
			tx, release, err := beginRead(ctx, db.db)
			if err != nil {
				t.Errorf("beginRead %d: %v", i, err)
				return
			}
			// Touch the transaction so the snapshot is actually taken — an unused
			// tx could be released by a driver that never dialled.
			if _, err := tx.QueryContext(ctx, `SELECT 1`); err != nil {
				release()
				t.Errorf("beginRead %d: probe read: %v", i, err)
				return
			}
			release()
		}
	}()

	select {
	case <-looped:
	case <-time.After(10 * time.Second):
		t.Fatalf("3×maxOpenConns (%d) beginRead/release cycles did not complete within 10s — release() is leaking its pool connection, and at maxOpenConns=%d that many exports kill the pool for every other request", maxOpenConns*3, maxOpenConns)
	}

	// 🔴 CONTROL: prove the pool is genuinely usable afterwards. A leak that
	// somehow survived the loop shows up here as a timeout rather than an error.
	done := make(chan error, 1)
	go func() {
		var n int
		done <- db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM token_events`).Scan(&n)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("post-release read: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a plain read did not complete within 10s after 3×maxOpenConns beginRead/release cycles — release is leaking connections")
	}
}

// isWriteLockUnavailable reports whether err is the retryable contention
// sentinel, so the concurrent writer above can distinguish an expected loss of a
// race from a real failure it must not swallow. errors.Is, not a string match:
// the sentinel is wrapped with %w at every site that returns it.
func isWriteLockUnavailable(err error) bool {
	return errors.Is(err, ErrWriteLockUnavailable)
}
