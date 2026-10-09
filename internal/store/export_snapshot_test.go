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
// rows.Close() runs, so one connection was never a lock held across the eleven
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
// correct": with the subject's rows in ALL THIRTEEN exported tables written and
// deleted ATOMICALLY (one write transaction each way), every snapshot of the
// database has those thirteen tables either all populated or all empty. So an export
// reporting some populated and some empty read two different database states,
// and that is a tear by construction — no timing assumption, no sampling, no
// flake.
//
// ⚠️ ALL ELEVEN, not a representative pair, and that is not caution — it is
// MEASURED. This file's first draft asserted over token_events and outcomes
// alone; a mutant reverting exactly ONE other read (quality_history) to the pool
// SURVIVED it. See subjectSeedStatements.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
	// The session_id is what joins the watcher_checkpoint seed below (#919).
	{"token_events", `INSERT INTO token_events (developer, issue_id, model, cost_micro, source, fidelity, session_id)
		VALUES (?1, 'issue-673', 'claude-sonnet-4', 1000, 'proxy', 'realtime', 'sess-673-' || ?1)`},
	{"outcomes", `INSERT INTO outcomes (developer, issue_id, weight) VALUES (?, 'issue-673', 1.0)`},
	{"actual_spend", `INSERT INTO actual_spend (developer, period, actual_paid_micro) VALUES (?, '2026-08', 200000000)`},
	{"org_hierarchy", `INSERT INTO org_hierarchy (developer, team, division, org) VALUES (?, 'core', 'platform', 'acme')`},
	{"period_membership", `INSERT INTO period_membership (developer, org, period_start) VALUES (?, 'acme', '2026-08')`},
	{"hierarchy_membership", `INSERT INTO hierarchy_membership (developer, team, division, valid_from, written_by)
		VALUES (?, 'core', 'platform', '2026-08-01 00:00:00 +0000 UTC', 'test:fixture')`},
	{"quality_events", `INSERT INTO quality_events (outcome_id, developer, issue_id, event_type, source_ref, event_ts)
		VALUES (1, ?, 'issue-673', 'ci_pass', 'sha-673:1', CURRENT_TIMESTAMP)`},
	{"quality_history", `INSERT INTO quality_history (outcome_id, developer, issue_id, old_quality, new_quality, reason, source_ref)
		VALUES (1, ?, 'issue-673', 1.0, 0.5, 'ci_fail', 'sha-673:1')`},
	{"repo_repair_audit", `INSERT INTO repo_repair_audit (repair_id, developer, from_repo, to_repo, row_count, cost_micro_sum, tool_version)
		VALUES ('repair-673', ?, 'unqualified', 'acme/tier', 1, 1000, 'test')`},
	{"push_outcome_commits", `INSERT INTO push_outcome_commits (repo, commit_sha, outcome_id, developer, ts)
		VALUES ('acme/tier', 'sha-673', 1, ?, '2026-08-01 00:00:00 +0000 UTC')`},
	{"push_outcome_audit", `INSERT INTO push_outcome_audit (outcome_id, repo, issue_id, push_day, action, developer, outcome_ts, commit_sha)
		VALUES (1, 'acme/tier', 'issue-673', '2026-08-01', 'superseded', ?, '2026-08-01 00:00:00 +0000 UTC', 'sha-673')`},
	{"developer_alias", `INSERT INTO developer_alias (alias, canonical) VALUES ('alias-of-' || ?, ?)`},
	// #919: the subject's watcher checkpoint, joined through the session id of
	// the token_events row above. Its path does not exist, so an erase DELETES it
	// rather than tombstoning it — the all-or-nothing invariant this file asserts
	// (every table at its seeded count, or every table at zero) then holds for it.
	{"watcher_checkpoint", `INSERT INTO watcher_checkpoint (path, inode, byte_offset, head_crc, head_len, metadata)
		VALUES ('/nonexistent-919/' || ?1 || '.jsonl', 1, 10, 1, 10, json_object('SessionID', 'sess-673-' || ?1))`},
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
	"period_membership": 1, "hierarchy_membership": 1, "quality_events": 1, "quality_history": 1,
	"repo_repair_audit": 1, "push_outcome_commits": 1, "push_outcome_audit": 1,
	"developer_alias": 1, "watcher_checkpoint": 1,
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
		"token_events":         len(exp.TokenEvents),
		"outcomes":             len(exp.Outcomes),
		"actual_spend":         len(exp.ActualSpend),
		"org_hierarchy":        len(exp.OrgHierarchy),
		"period_membership":    len(exp.PeriodMembership),
		"hierarchy_membership": len(exp.HierarchyMembership),
		"quality_events":       len(exp.QualityEvents),
		"quality_history":      len(exp.QualityHistory),
		"repo_repair_audit":    len(exp.RepoRepairAudit),
		"push_outcome_commits": len(exp.PushOutcomeCommits),
		"push_outcome_audit":   len(exp.PushOutcomeAudit),
		"developer_alias":      len(exp.DeveloperAlias),
		"watcher_checkpoint":   len(exp.WatcherCheckpoint),
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
		if s.table == "watcher_checkpoint" {
			query = `SELECT COUNT(*) FROM watcher_checkpoint
				WHERE path = '/nonexistent-919/' || ? || '.jsonl' OR path = '/nonexistent-919/alias-of-' || ? || '.jsonl'`
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
	// sawPopulated / sawEmpty are the vacuity control. See the assertion after
	// wg.Wait() for why they replaced a cycle-count floor.
	sawPopulated, sawEmpty, alternations := 0, 0, 0
	prevState := 0 // 0 unknown, 1 populated, 2 empty
	for i := 0; i < iterations; i++ {
		exp, err := db.ExportDeveloper(ctx, developer)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("ExportDeveloper (iteration %d): %v", i, err)
		}
		// 🔑 THE INVARIANT, ACROSS ALL ELEVEN TABLES, AND AT EXACT COUNTS: every
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
		switch {
		case len(wrong) == 0 && len(zero) == 0:
			sawPopulated++ // a consistent snapshot of the seeded state
			if prevState == 2 {
				alternations++
			}
			prevState = 1
		case len(wrong) == 0 && present == 0:
			sawEmpty++ // a consistent snapshot taken between an erase and its re-seed
			if prevState == 1 {
				alternations++
			}
			prevState = 2
		}
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
	// fail — sitting inside the file whose own header warns about it.
	//
	// 🔴 IT ASSERTS A PROPERTY, NOT A RATE, AND THAT IS THE FIX FOR #726.
	//
	// It used to require `cycles >= 40`, "a tenth of what this machine measures
	// (432-455 cycles per 2000 exports)". That number was a SNAPSHOT OF ONE
	// MACHINE published as a property of the test. Re-measured over 22 runs on the
	// machine that now runs it: 111-362, median ~205 — half the recorded baseline,
	// so the real margin was ~3x rather than the intended 10x, and an occasional
	// collapse to 18 dipped under the floor. ~3 failures in 35 runs.
	// ⚠️ The collapses were not the hardware: they were 8+ concurrent agents
	// saturating the box. A first diagnosis ruled load OUT because 8 background
	// SPINNERS reproduced nothing — but spinners contend for CPU alone, while a
	// concurrent `go test` contends for CPU, disk AND the SQLite WAL. A negative
	// result from the wrong load shape is not a negative result.
	//
	// ⇒ Any absolute cycle floor is a number that must be re-measured on every
	// machine and under every load, and nothing makes anyone re-measure it. So the
	// control no longer counts cycles at all. What it needs to establish is that
	// THE READER RACED A LIVE WRITER, and the direct evidence for that is that the
	// reader observed the database in MORE THAN ONE consistent state: at least one
	// export saw the seeded rows, and at least one saw the window between an erase
	// and its re-seed. A writer that never writes cannot produce the second, at any
	// speed, on any hardware — so both mutants above still fail, and a slow or
	// loaded box no longer fails for being slow.
	//
	// The cycle count is still LOGGED, because it is useful when diagnosing a
	// failure, but nothing asserts on it. A number that is reported and not
	// asserted cannot rot into a false gate.
	n := atomic.LoadInt64(&cycles)
	t.Logf("writer completed %d erase/re-seed cycles during %d export iterations; "+
		"reader observed %d populated and %d empty consistent snapshots, %d alternations",
		n, iterations, sawPopulated, sawEmpty, alternations)
	if sawPopulated == 0 || sawEmpty == 0 {
		t.Fatalf("control: across %d exports the reader observed %d populated and %d empty "+
			"consistent snapshots (writer reported %d cycles) — it needs BOTH to have raced a "+
			"live writer at all, so the %d-tear result below is VACUOUS. Seeing only populated "+
			"states means the writer never took effect; seeing only empty ones means the "+
			"re-seed never did",
			iterations, sawPopulated, sawEmpty, n, tears)
	}
	// ⚠️ BOTH STATES SEEN IS NOT YET INTERLEAVING, and a negative control is what
	// proved it. A writer that erases ONCE and never re-seeds also produces both —
	// the seeded state before it, the empty state forever after. Measured with
	// exactly that mutant: 4 populated, 1996 empty, ONE alternation, and the
	// populated/empty control PASSED it. The retired cycle floor could not have
	// caught it either: `cycles` reported 4187, because it increments after a
	// re-seed that is free to be a no-op.
	//
	// ⭐ Requiring TWO alternations demands the reader watch the population COME
	// BACK — i.e. observe a complete erase->re-seed round from the outside, which
	// is the only thing that makes a torn read possible at all.
	// Measured on healthy code across 35 runs: 820-913.
	const minAlternations = 2
	if alternations < minAlternations {
		t.Fatalf("control: the reader observed only %d populated<->empty alternations across "+
			"%d exports (%d populated, %d empty, writer reported %d cycles) — fewer than %d "+
			"means it never watched a full erase and re-seed from the outside, so the %d-tear "+
			"result below is VACUOUS",
			alternations, iterations, sawPopulated, sawEmpty, n, minAlternations, tears)
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

// exportReadPlanExpectations records, for EVERY table ExportDeveloper reads, the
// measured plan of that read: whether it SEEKS or SCANS, WHICH index it uses, and
// whether it materialises a temp b-tree to satisfy the ORDER BY. Frozen here so a
// change in any of the three is visible.
//
// ⭐ EIGHT OF THE NINE SEEK. #679 added the four missing developer-leading
// indexes (idx_period_membership_developer, idx_quality_events_developer,
// idx_quality_history_developer, idx_repo_repair_audit_developer) and flipped the
// four `false` entries to `true` — exactly the failure this table was written to
// force, so the improvement could not land silently.
//
// 🔴 THE `sorts` COLUMN EXISTS BECAUSE #679 ADDED SORTS, AND THE FIRST DRAFT OF
// THIS TABLE COULD NOT SEE THEM. It planned `WHERE developer IN (?,?)` with NO
// ORDER BY — a query ExportDeveloper never runs. Measured on the real shape:
// quality_events, quality_history and repo_repair_audit previously planned as a
// bare `SCAN`, which walks in rowid order and so satisfies `ORDER BY id` FOR
// FREE; with a developer index they became `SEARCH … | USE TEMP B-TREE FOR ORDER
// BY`. ⇒ #679 traded a free ordered scan for a seek PLUS a sort. That is a win
// when the subject is a small fraction of the table and a LOSS when the subject
// IS the table — see beginRead's doc block, which carries both measurements.
// ⛔ Never drop the ORDER BY from these fixtures again: the cost it exposes lives
// inside the held read transaction that whole doc block reasons about.
//
// ⚠️ `orderBy` is source-parity-checked against ExportDeveloper (see
// TestExportPlanFixtureMatchesTheRealExportSQL), so it cannot drift from
// production. The column LIST is not duplicated here, and that is measured, not
// assumed: `SELECT *` and the real column list plan IDENTICALLY for all 13 reads
// (checked in the same test).
//
// 🔴 THE HISTORY IS KEPT BECAUSE IT IS THE REASON THE TABLE EXISTS. An earlier
// version of this test checked ONLY token_events — the one table where the seek
// held — while its name and doc claimed the general property, and beginRead's doc
// block cited it to justify not bounding the export. Measured consequence, with a
// NINE-ROW subject: 200,000 OTHER developers' quality_events rows took one export
// from 153µs to 11.68ms, a 76.3× slowdown driven entirely by rows the subject does
// not own. An index dropped in either direction now fails here before anyone
// re-reasons about the WAL.
//
// ⚠️ RE-MEASURED AFTER #679, AND IT IS A TRADE, NOT A CLEAN WIN — do not summarise
// it as "the indexes made the export faster". Same probe, shipped driver:
// 11.694ms -> 95.96µs when the subject is a slice of the table (122×), but
// 308.1ms -> 381.8ms (24% SLOWER) when the subject IS the table, because the sort
// this table now records is no longer free. beginRead's doc block carries both
// figures and the mechanism.
//
// ⚠️ developer_alias stays `seeks:false` FOREVER, and the reason is NOT that an
// index would be useless. developerIdentifierSet runs `SELECT alias, canonical
// FROM developer_alias` with NO WHERE clause, so it reads the whole map whatever
// indexes exist — that read is what the `false` records. ⛔ Do not read this row
// as "never index developer_alias": the predicate below is `canonical = ?`, which
// is EraseDeveloper's DELETE shape (a different path, under a WRITE lock), and an
// index on (canonical) would legitimately speed THAT up. If one is ever added,
// flip this row and say which path moved — do not revert the index.
var exportReadPlanExpectations = []struct {
	table     string
	seeks     bool
	predicate string
	// index is the index the plan must name. Asserted POSITIVELY — see the loop
	// for why the name and not merely the absence of "SCAN".
	index string
	// sorts records whether the plan materialises a temp b-tree for the ORDER BY.
	sorts bool
	// orderBy is production's ORDER BY for this read, source-parity-checked.
	orderBy string
}{
	{"token_events", true, `developer IN (?,?)`, "idx_token_events_scores", true, "id"},
	{"outcomes", true, `developer IN (?,?)`, "idx_outcomes_developer", true, "id"},
	{"actual_spend", true, `developer IN (?,?)`, "idx_actual_spend_dev_period_nu", true, "id"},
	// The only seek with NO sort: ORDER BY developer is already the autoindex's
	// leading column, so the walk emits in order.
	{"org_hierarchy", true, `developer IN (?,?)`, "sqlite_autoindex_org_hierarchy_1", false, "developer"},
	{"period_membership", true, `developer IN (?,?)`, "idx_period_membership_developer", true, "developer, period_start"},
	// #886: the dated team history, read in timeline order.
	{"hierarchy_membership", true, `developer IN (?,?)`, "idx_hierarchy_membership_developer", false, "developer, valid_from, id"},
	{"quality_events", true, `developer IN (?,?)`, "idx_quality_events_developer", true, "id"},
	{"quality_history", true, `developer IN (?,?)`, "idx_quality_history_developer", true, "id"},
	{"repo_repair_audit", true, `developer IN (?,?)`, "idx_repo_repair_audit_developer", true, "id"},
	{"push_outcome_commits", true, `developer IN (?,?)`, "idx_push_outcome_commits_developer", true, "id"},
	{"push_outcome_audit", true, `developer IN (?,?)`, "idx_push_outcome_audit_developer", true, "id"},
	// Whole-table BY DESIGN — see the ⚠️ above before "fixing" this.
	{"developer_alias", false, `canonical = ?`, "", false, "alias"},
	// #919: a scan of watcher_checkpoint (one row per tailed session file, in
	// path order off its primary-key autoindex, so no sort) with the subject's
	// session ids read by an indexed subquery on token_events.
	{"watcher_checkpoint", false, subjectCheckpointPredicate("?,?"), "", false, "path"},
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
		plan := queryPlan(t, db, `SELECT * FROM `+e.table+` WHERE `+e.predicate+` ORDER BY `+e.orderBy, args...)
		checked++
		scans := strings.Contains(plan, "SCAN "+e.table)
		switch {
		case e.seeks && scans:
			t.Errorf("%s: the export read now plans as a FULL SCAN (%q) but is recorded as a SEEK. An index that used to bound this read is gone, so the export transaction is now proportional to the table rather than the subject — and beginRead's doc block reasons about exactly that. Restore the index or update both the expectation AND that doc block", e.table, plan)
		case !e.seeks && !scans:
			t.Errorf("%s: the export read no longer plans as a full scan (%q) — that is an IMPROVEMENT, and it must be recorded rather than silently absorbed, because beginRead's doc block enumerates this table's plan by name and reasons about the export transaction's duration from it. Flip seeks to true here, fill in the index column, and update that block in the same commit. ⛔ EXCEPT for developer_alias, which is whole-table BY DESIGN — if that is the table named here, the plan text changed or an index was added to the alias map; do NOT flip it", e.table, plan)
		}

		// 🔴 THE NAMED-INDEX ASSERTION, AND IT IS NOT BELT-AND-BRACES OVER `scans`.
		// The switch above is a NEGATIVE match — it fires on the literal
		// "SCAN <table>". A change in the driver's plan WORDING (SQLite said
		// "SCAN TABLE x" before 3.36) makes that substring stop matching, at which
		// point every seeks==true row passes by silence. Measured with exactly that
		// mutant: with all four #679 indexes ALSO deleted, the loop reported four
		// real full-table scans as ZERO failures, and the only thing that fired was
		// developer_alias — via the improvement arm, whose message tells the reader
		// to flip it, which turns the suite green over four regressions.
		//
		// ⭐ A POSITIVE match on the index NAME cannot fail that way: it fires under
		// plan-text drift AND under a genuine scan, because in both worlds the name
		// is absent. It also pins the four index names beginRead's doc block
		// enumerates, which nothing else in the tree asserts — rename an index and
		// that block goes stale silently.
		if e.seeks && !strings.Contains(plan, e.index) {
			t.Errorf("%s: the export read does not use %s — plan was %q. Either the index is gone/renamed (the export transaction is no longer subject-bounded) or EXPLAIN QUERY PLAN's wording changed. If it is a rename, update this row AND the per-table plan list in beginRead's doc block, which names this index", e.table, e.index, plan)
		}

		// 🔴 THE SORT IS PART OF THE TRANSACTION'S DURATION, SO IT IS PINNED TOO.
		// A temp b-tree materialises the WHOLE result set before the first row is
		// returned, inside the read transaction beginRead's doc block reasons
		// about. #679 ADDED this cost to three tables that previously walked in
		// rowid order for free — recording it is what keeps the doc block's
		// win/loss framing honest instead of "indexes made it faster".
		sorts := strings.Contains(plan, "USE TEMP B-TREE")
		if sorts != e.sorts {
			t.Errorf("%s: plan %q materialises a temp b-tree = %v, recorded as %v. A sort appearing or disappearing changes what the export costs INSIDE the held read transaction — update this row and beginRead's doc block together, and re-measure the subject≈table arm before calling a new sort free", e.table, plan, sorts, e.sorts)
		}
	}

	// 🔴 CONTROL 1: every expectation must have been exercised. A loop that
	// planned nothing would report no failures and look identical to a pass.
	if checked != len(exportReadPlanExpectations) || checked != 13 {
		t.Fatalf("control: planned %d reads, expected 13 — the loop did not cover every export read, so a regression in an unplanned table is invisible", checked)
	}

	// 🔴 CONTROL 2: EXPLAIN QUERY PLAN must be capable of emitting the EXACT
	// string the loop matches on — "SCAN <table>", not merely "SCAN".
	//
	// ⚠️ IT ASSERTED THE LOOSER "SCAN" UNTIL #679, AND THE GAP WAS REAL. The loop
	// matches "SCAN "+e.table; a control that only proves the word "SCAN" exists
	// still passes when the table-qualified form stops matching, which is exactly
	// the drift the named-index assertion above was added for. Measured: this
	// control passed while the loop silently absorbed four full-table scans.
	// Pinning the qualified form makes the control defend the predicate in use.
	control := queryPlan(t, db, `SELECT COUNT(*) FROM token_events WHERE model = ?`, "claude-sonnet-4")
	if !strings.Contains(control, "SCAN token_events") {
		t.Fatalf("control: a query with no usable index planned as %q, which does not contain \"SCAN token_events\" — EXPLAIN QUERY PLAN is not reporting scans in the form this test matches on, so the seeks==true assertions above cannot fail by that route and prove nothing. If the driver's plan wording changed, update the loop's matcher and this control together", control)
	}

	// 🔴 CONTROL 3: and the planner must be capable of NOT scanning where an index
	// exists, or the developer_alias row — the ONE seeks==false expectation left
	// after #679 — is vacuously satisfied.
	//
	// ⚠️ It is now largely REDUNDANT and is kept for DIAGNOSIS, not coverage: any
	// environment that fails it also fails all eight seeks==true rows first. Its
	// value is that it fails with "the planner is scanning everywhere" rather than
	// letting eight simultaneous failures be misread as eight deleted indexes.
	seekControl := queryPlan(t, db, `SELECT COUNT(*) FROM token_events WHERE developer = ?`, "alice")
	if strings.Contains(seekControl, "SCAN token_events") {
		t.Fatalf("control: an indexed developer lookup planned as %q — the planner is scanning even where an index exists, so the developer_alias expectation above cannot fail either, and the eight seek failures you are also seeing are one planner problem, not eight missing indexes", seekControl)
	}
}

// TestExportPlanFixtureMatchesTheRealExportSQL is the anti-drift guard for
// exportReadPlanExpectations: it checks the fixture against ExportDeveloper's
// SOURCE, not against another hand-maintained list.
//
// 🔴 WHY IT EXISTS. The fixture planned `WHERE developer IN (?,?)` with NO ORDER
// BY for its whole life — a query ExportDeveloper has never run — and that single
// omission hid the temp-b-tree sort #679 introduced on three tables. A fixture
// that drifts from production does not fail; it quietly measures something else
// and reports it as the truth. This test makes that drift a failure.
//
// It asserts TWO things per table:
//
//	(1) the `ORDER BY <orderBy>` this fixture plans appears VERBATIM in
//	    store.go — so production's ordering and the fixture's cannot diverge; and
//	(2) the column LIST may be omitted, because `SELECT *` and production's real
//	    column list plan IDENTICALLY. That is measured here, per table, not
//	    assumed — it is what licenses the fixture to stay short.
func TestExportPlanFixtureMatchesTheRealExportSQL(t *testing.T) {
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	// Bound the search to ExportDeveloper's body so an ORDER BY belonging to some
	// unrelated method cannot satisfy the check.
	body := string(src)
	start := strings.Index(body, "func (d *DB) ExportDeveloper(")
	if start < 0 {
		t.Fatal("control: ExportDeveloper not found in store.go — the parity check cannot run, and a fixture that matches nothing would pass vacuously")
	}
	end := strings.Index(body[start:], "\n// queryRows runs one read")
	if end < 0 {
		t.Fatal("control: could not find the end of ExportDeveloper's body in store.go — an unbounded search would match ORDER BY clauses from other methods")
	}
	fnSrc := body[start : start+end]

	// 🔴 CONTROL: the slice must actually contain export reads. A zero-length or
	// wrong slice makes every Contains below vacuously false in the wrong
	// direction (it would fail loudly) — but a slice containing the WHOLE file
	// would make them vacuously true, which is the dangerous direction.
	if n := strings.Count(fnSrc, "FROM "); n < 13 {
		t.Fatalf("control: the ExportDeveloper source slice contains %d %q occurrences, want >= 13 — the slice is too small to hold all thirteen reads", n, "FROM ")
	}
	if strings.Contains(fnSrc, "func (d *DB) EraseDeveloper(") {
		t.Fatal("control: the ExportDeveloper source slice swallowed EraseDeveloper — the bound is wrong and an ORDER BY from another method could satisfy the parity check")
	}

	db, cleanup := newTestDB(t)
	defer cleanup()
	seedSubjectAtomically(t, db, "alice")

	// Pull each read's real column list straight out of the source, so the
	// SELECT-* equivalence is checked against production text rather than a copy.
	selectRe := regexp.MustCompile(`(?s)SELECT (.*?)\s+FROM (\w+) WHERE`)
	realCols := map[string]string{}
	for _, m := range selectRe.FindAllStringSubmatch(fnSrc, -1) {
		cols := strings.Join(strings.Fields(m[1]), " ")
		realCols[m[2]] = cols
	}
	if len(realCols) != len(exportReadPlanExpectations) {
		t.Fatalf("control: extracted %d SELECT column lists from ExportDeveloper but the fixture has %d rows — the regex is not matching every read, so the equivalence below covers an unknown subset", len(realCols), len(exportReadPlanExpectations))
	}

	// 🔴 THE ORDER BY MUST BE BOUND TO ITS TABLE, AND A MUTANT PROVED IT. A first
	// draft asked only whether "ORDER BY id" appeared ANYWHERE in the method.
	// Six of the nine reads order by id, so changing quality_events' ordering to
	// event_ts left the substring present and the test PASSED — production and
	// fixture silently describing different queries, which is the exact failure
	// this test exists to catch. Parse per table instead.
	realOrder := realExportOrderBys(t, fnSrc)
	if len(realOrder) != len(exportReadPlanExpectations) {
		t.Fatalf("control: extracted %d per-table ORDER BY clauses but the fixture has %d rows — a read whose clause was not found would be checked against nothing", len(realOrder), len(exportReadPlanExpectations))
	}

	for _, e := range exportReadPlanExpectations {
		if got := realOrder[e.table]; got != e.orderBy {
			t.Errorf("%s: the fixture plans `ORDER BY %s` but ExportDeveloper sorts this read by `%s` — the fixture has drifted from production, so its recorded plan (and the temp-b-tree verdict built on it) describes a query nobody runs. Fix the orderBy field to match the method", e.table, e.orderBy, got)
		}

		cols, ok := realCols[e.table]
		if !ok {
			t.Errorf("%s: no SELECT … FROM %s found in ExportDeveloper — is this table still exported?", e.table, e.table)
			continue
		}
		args := []any{"alice", "alias-of-alice"}
		if e.table == "developer_alias" {
			args = []any{"alice"}
		}
		tail := ` FROM ` + e.table + ` WHERE ` + e.predicate + ` ORDER BY ` + e.orderBy
		starPlan := queryPlan(t, db, `SELECT *`+tail, args...)
		realPlan := queryPlan(t, db, `SELECT `+cols+tail, args...)
		if starPlan != realPlan {
			t.Errorf("%s: `SELECT *` plans as %q but production's column list plans as %q — they are no longer equivalent, so this fixture's SELECT-* shortcut is measuring the wrong query. Put the real column list in the fixture", e.table, starPlan, realPlan)
		}
	}
}

// exportIndexes679 is the set #679 added, and the set an UPGRADED database must
// gain. Kept next to the upgrade test so the two cannot drift apart.
var exportIndexes679 = map[string]string{
	"idx_period_membership_developer": "period_membership",
	"idx_quality_events_developer":    "quality_events",
	"idx_quality_history_developer":   "quality_history",
	"idx_repo_repair_audit_developer": "repo_repair_audit",
}

// TestUpgradeAddsTheExportDeveloperIndexes proves the four #679 indexes appear on
// an EXISTING on-disk database, not merely on a fresh one.
//
// 🔴 EVERY OTHER PLAN TEST IN THIS FILE USES newTestDB, i.e. a database created
// from scratch by the current binary — so all of them would still pass if the
// CREATE INDEX statements were placed somewhere the upgrade path never reaches
// (the Phase-3 block guarded by a migration flag, say, or a fresh-DB-only branch).
// The whole point of #679 is bounding the export on deployments that ALREADY have
// history; a fix that only lands on new installs fixes nobody.
//
// ⚠️ THE FIXTURE IS "OPEN, THEN DROP THE FOUR", AND THAT IS EXACT, NOT AN
// APPROXIMATION — because #679 added exactly these four indexes and nothing else.
// Dropping precisely them reproduces a pre-#679 file byte-for-byte in every
// respect this test reads. It is preferred over hand-writing legacy CREATE TABLE
// statements, which would silently drift from the real schema and start testing a
// shape no deployment ever had.
func TestUpgradeAddsTheExportDeveloperIndexes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pre679.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for name := range exportIndexes679 {
		if _, err := db.db.Exec(`DROP INDEX IF EXISTS ` + name); err != nil {
			t.Fatalf("drop %s: %v", name, err)
		}
	}
	// 🔴 CONTROL: the fixture must really be in the pre-#679 state. Without this,
	// a DROP that silently did nothing would leave the indexes present and the
	// assertions below would pass while proving nothing about the upgrade.
	before := allIndexNames(t, db.db)
	for name := range exportIndexes679 {
		if before[name] {
			t.Fatalf("control: %s survived the DROP, so the fixture is not in the pre-#679 state and this test cannot observe an upgrade", name)
		}
	}
	// 🔴 CONTROL: and the read really does scan in that state, or "the index came
	// back" is not evidence that anything was ever bound.
	plan := queryPlan(t, db, `SELECT * FROM quality_events WHERE developer IN (?,?) ORDER BY id`, "alice", "alias-of-alice")
	if !strings.Contains(plan, "SCAN quality_events") {
		t.Fatalf("control: with idx_quality_events_developer dropped the export read planned as %q, not a SCAN — the fixture does not reproduce the pre-#679 defect", plan)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Re-open the SAME FILE with the current binary: this is the upgrade.
	up, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open (the upgrade): %v", err)
	}
	defer func() { _ = up.Close() }()

	after := allIndexNames(t, up.db)
	for name, table := range exportIndexes679 {
		if !after[name] {
			t.Errorf("upgrading an existing database did NOT create %s on %s — the export stays table-size-bounded on every deployment that already has history, which is the only kind #679 is about. The CREATE INDEX must run on the upgrade path, not just on a fresh DB", name, table)
		}
	}
	upPlan := queryPlan(t, up, `SELECT * FROM quality_events WHERE developer IN (?,?) ORDER BY id`, "alice", "alias-of-alice")
	if strings.Contains(upPlan, "SCAN quality_events") {
		t.Errorf("after upgrading an existing database the export read still plans as %q — the index exists but is not being used, so the bound was not actually restored", upPlan)
	}
}

// allIndexNames returns every index in the database, across all tables.
// (index_test.go's indexNames is hardcoded to token_events.)
func allIndexNames(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='index'`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan index name: %v", err)
		}
		out[n] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("index rows: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("control: sqlite_master reported ZERO indexes — the query is wrong, so every presence check built on it is vacuously false")
	}
	return out
}

// realExportOrderBys maps each exported table to the ORDER BY clause
// ExportDeveloper actually sorts that read by, parsed out of the method's source.
//
// ⚠️ THE CLAUSE IS IN A DIFFERENT STRING LITERAL FROM THE TABLE NAME, which is
// why this is a scan rather than one regex: the reads are written as
//
//	`… FROM quality_events WHERE developer IN (` + placeholders + `) ORDER BY id`
//
// so the backtick literal ENDS at `IN (` and the ORDER BY begins a new one. The
// scan therefore finds "FROM <table> WHERE", then takes the next "ORDER BY" and
// reads to the closing backtick.
func realExportOrderBys(t *testing.T, fnSrc string) map[string]string {
	t.Helper()
	out := map[string]string{}
	fromRe := regexp.MustCompile(`FROM (\w+) WHERE`)
	locs := fromRe.FindAllStringSubmatchIndex(fnSrc, -1)
	for i, loc := range locs {
		table := fnSrc[loc[2]:loc[3]]
		// Bound the forward search at the NEXT read, so a clause belonging to the
		// following statement can never be attributed to this table.
		end := len(fnSrc)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		seg := fnSrc[loc[1]:end]
		ob := strings.Index(seg, "ORDER BY ")
		if ob < 0 {
			t.Errorf("control: no ORDER BY found for the %s read in ExportDeveloper's source — if that read genuinely has no ordering, the fixture must record that rather than a clause", table)
			continue
		}
		rest := seg[ob+len("ORDER BY "):]
		if q := strings.IndexByte(rest, '`'); q >= 0 {
			rest = rest[:q]
		}
		out[table] = strings.TrimSpace(rest)
	}
	return out
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
// ⚠️ Reachable since #846: serve's daily prune loop runs while webhook
// deliveries are being written, so a delivery can land between the two passes.
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
	// the two DELETEs share a transaction or not, because this test is
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

// TestPublishedErasureTableListMatchesTheCode pins the GDPR erasure table list
// that docs/how-it-works.md PUBLISHES against developerPIITables, the slice
// EraseDeveloper actually iterates.
//
// 🔴 WHY, AND IT IS NOT HYPOTHETICAL. Found while verifying #679: the published
// list named SEVEN tables and the code erases EIGHT — `repo_repair_audit` was
// missing from the doc. The code was right; the document understated the
// coverage of a RIGHT-TO-ERASURE endpoint. A reader auditing tier's Art. 17
// compliance would have concluded a table of personal data was never cleared.
//
// ⭐ It is the #642 / #724 defect class one surface over: a list published in
// prose, sourced from one Go value, with no gate between them. The direction of
// the error matters — a doc that UNDERSTATES erasure is the safe direction for a
// data subject and the dangerous one for an auditor, and neither is acceptable
// in a compliance-facing document.
//
// The reverse direction is guarded too: a table added to the doc but not to the
// code would be a doc CLAIMING an erasure that does not happen, which is the
// unsafe direction. Both arms are asserted.
func TestPublishedErasureTableListMatchesTheCode(t *testing.T) {
	raw, err := os.ReadFile("../../docs/how-it-works.md")
	if err != nil {
		t.Fatalf("cannot read docs/how-it-works.md: %v", err)
	}
	// The erasure paragraph is the one naming the endpoint; scope to it so an
	// unrelated table name elsewhere in a 1000-line document cannot satisfy or
	// break this. A file-wide scan is how the sibling guards in this repo went
	// wrong.
	// 🔴 ANCHORED TO THE TABLE LIST ITSELF, not to the paragraph. A first version
	// bounded on the erasure bullet and split on a blank line — but the bullet is
	// one long line inside a list, so the scope ran on and swept a dozen FIELD
	// names (coverage_pct, spend_leverage, k_anonymity…) into the comparison. The
	// over-claim arm then failed on a CORRECT document. A guard that reddens a
	// correct doc is a guard somebody deletes.
	const lead = "across **all** developer-PII tables — "
	const tail = " — plus the `developer_alias` rows themselves"
	i := strings.Index(string(raw), lead)
	if i < 0 {
		t.Fatalf("cannot find the erasure table list (%q) — this guard is not reading "+
			"what it thinks it is", lead)
	}
	rest := string(raw)[i+len(lead):]
	j := strings.Index(rest, tail)
	if j < 0 {
		t.Fatalf("the erasure table list has no %q terminator — the paragraph was "+
			"restructured and this guard's scope is now wrong", tail)
	}
	para := rest[:j]

	published := map[string]bool{}
	for _, m := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(para, -1) {
		published[m[1]] = true
	}

	var missing []string
	for _, table := range developerPIITables {
		if !published[table] {
			missing = append(missing, table)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("docs/how-it-works.md's erasure list omits %v, which EraseDeveloper "+
			"does clear (developerPIITables). A right-to-erasure endpoint's published "+
			"coverage must not understate what the code does", missing)
	}

	inCode := map[string]bool{}
	for _, table := range developerPIITables {
		inCode[table] = true
	}
	var overclaimed []string
	for table := range published {
		// Only judge names that are plausibly tables: the paragraph also contains
		// field names and JSON keys in backticks.
		if !inCode[table] {
			overclaimed = append(overclaimed, table)
		}
	}
	sort.Strings(overclaimed)
	if len(overclaimed) > 0 {
		t.Errorf("docs/how-it-works.md's erasure paragraph names %v, which "+
			"developerPIITables does NOT contain — a document claiming an erasure "+
			"that does not happen is the unsafe direction", overclaimed)
	}

	if len(developerPIITables) == 0 {
		t.Fatal("developerPIITables is empty: this guard compared nothing")
	}
	t.Logf("published erasure list matches developerPIITables (%d tables)",
		len(developerPIITables))
}
