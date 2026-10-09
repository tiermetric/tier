package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// seedCostCorrectionAuditRow appends one row to the money-rewrite ledger and
// returns its id. Written directly rather than through CorrectManualCostEvent
// because these tests are about what the SCHEMA permits, not about the
// correction path.
func seedCostCorrectionAuditRow(t *testing.T, db *DB) int64 {
	t.Helper()
	res, err := db.db.Exec(
		`INSERT INTO cost_correction_audit
		    (token_event_id, old_cost_micro, new_cost_micro, actor, reason)
		 VALUES (?, ?, ?, ?, ?)`,
		int64(1), int64(42_000_000), int64(50_000_000), "finance-alice", "Q3 invoice")
	if err != nil {
		t.Fatalf("seed cost_correction_audit: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	return id
}

// TestCostCorrectionAuditRefusesUpdate is the #604 ruling (Option A) at the
// schema level: an audit row's content is a historical fact, so UPDATE on
// cost_correction_audit is refused by trg_cost_correction_audit_no_update. The
// UPDATE is attempted against a REAL database, not a mock.
//
// The CONTROL ARM is the part that makes the assertion mean anything. A probe
// that cannot detect any failure would report "UPDATE refused" against a
// database with no trigger at all, so this first proves the harness enforces
// constraints: an INSERT violating a NOT NULL column MUST fail. Only then is a
// refused UPDATE evidence of the trigger rather than evidence of a broken test.
//
// 🔴 This is NOT tamper-proofing. DROP TRIGGER is one line for anyone holding
// the DB file. What it pins is that no FUTURE code path can silently mutate the
// ledger.
func TestCostCorrectionAuditRefusesUpdate(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	// CONTROL ARM: prove this database enforces constraints at all.
	if _, err := db.db.Exec(
		`INSERT INTO cost_correction_audit (token_event_id, old_cost_micro, new_cost_micro, actor, reason)
		 VALUES (?, ?, ?, NULL, ?)`,
		int64(1), int64(1), int64(2), "reason"); err == nil {
		t.Fatal("control arm FAILED: an INSERT violating NOT NULL on actor succeeded — " +
			"this harness does not enforce constraints, so a 'refused UPDATE' below would prove nothing")
	}

	id := seedCostCorrectionAuditRow(t, db)

	// A valid append must still succeed — the trigger must block UPDATE, not
	// writes in general.
	seedCostCorrectionAuditRow(t, db)

	// THE ASSERTION: UPDATE is refused.
	_, err := db.db.Exec(`UPDATE cost_correction_audit SET new_cost_micro = ? WHERE id = ?`, int64(1), id)
	if err == nil {
		t.Fatal("UPDATE on cost_correction_audit succeeded — the append-only guard is not in force")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("UPDATE error = %v, want the trigger's RAISE(ABORT) message naming append-only", err)
	}

	// The row is untouched: RAISE(ABORT) undoes the statement.
	var got int64
	if err := db.db.QueryRow(`SELECT new_cost_micro FROM cost_correction_audit WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatalf("read back row %d: %v", id, err)
	}
	if got != 50_000_000 {
		t.Errorf("new_cost_micro = %d, want 50000000 unchanged after the refused UPDATE", got)
	}
}

// TestCostCorrectionAuditAllowsDelete pins the OTHER half of the #604 ruling,
// which is the half most likely to be "helpfully" undone later: DELETE is NOT
// blocked, deliberately. Erasure (GDPR Art. 17) and ledger retention/GC are
// lawful reasons to remove an audit row, and no trigger can distinguish them
// from tampering. UPDATE is never legitimate on an audit row; DELETE is. That
// asymmetry is the ruling.
func TestCostCorrectionAuditAllowsDelete(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	id := seedCostCorrectionAuditRow(t, db)
	if _, err := db.db.Exec(`DELETE FROM cost_correction_audit WHERE id = ?`, id); err != nil {
		t.Fatalf("DELETE on cost_correction_audit = %v, want success — #604 ruled NO BEFORE DELETE "+
			"trigger on any table, because erasure and retention are lawful deletes", err)
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM cost_correction_audit WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if n != 0 {
		t.Errorf("row count after DELETE = %d, want 0", n)
	}
}

// TestAuditLedgerUpdateGuardScopedToCostCorrection pins the SCOPE of the guard,
// which is what keeps the schema comments on the other FOUR ledgers honest. Each
// of them claims to be "append-only by construction -- no code path mutates it;
// the table itself is not protected against direct DB access". That claim is only
// true while the table carries no trigger, so this asserts the difference
// directly, for every sibling rather than one representative: a raw UPDATE
// succeeds on all four, and is refused on cost_correction_audit.
//
// If someone later adds a trigger to any sibling, the matching subtest fails and
// names the table whose comment has become stale — the wording and the schema
// cannot drift apart silently.
func TestAuditLedgerUpdateGuardScopedToCostCorrection(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	// One minimal seed per sibling ledger, then a raw UPDATE that must succeed.
	siblings := []struct {
		table  string
		insert string
		args   []any
		update string
	}{
		{
			"reprice_audit",
			`INSERT INTO reprice_audit (reprice_id, from_version, old_price_version,
			    new_price_version, row_count, old_cost_micro_sum, new_cost_micro_sum,
			    price_effective_date, tool_version) VALUES (?,?,?,?,?,?,?,?,?)`,
			[]any{"run-1", 1, 1, 9, int64(1), int64(1000), int64(2000), "2026-08-01", "tierd-test"},
			`UPDATE reprice_audit SET row_count = 99`,
		},
		{
			"reprice_row_audit",
			`INSERT INTO reprice_row_audit (reprice_id, token_event_id, old_cost_micro,
			    old_price_version, old_billing_mode) VALUES (?,?,?,?,?)`,
			[]any{"run-1", int64(1), int64(1000), 1, "per_token"},
			`UPDATE reprice_row_audit SET old_cost_micro = 99`,
		},
		{
			"repo_repair_audit",
			`INSERT INTO repo_repair_audit (repair_id, developer, from_repo, to_repo,
			    row_count, cost_micro_sum, tool_version) VALUES (?,?,?,?,?,?,?)`,
			[]any{"rep-1", "alice", "unqualified", "acme/tier", int64(1), int64(1000), "tierd-test"},
			`UPDATE repo_repair_audit SET row_count = 99`,
		},
		{
			"repo_repair_row_audit",
			`INSERT INTO repo_repair_row_audit (repair_id, token_event_id, old_repo, new_repo)
			 VALUES (?,?,?,?)`,
			[]any{"rep-1", int64(1), "unqualified", "acme/tier"},
			`UPDATE repo_repair_row_audit SET new_repo = 'other/repo'`,
		},
	}
	for _, s := range siblings {
		t.Run(s.table, func(t *testing.T) {
			if _, err := db.db.Exec(s.insert, s.args...); err != nil {
				t.Fatalf("seed %s: %v", s.table, err)
			}
			if _, err := db.db.Exec(s.update); err != nil {
				t.Errorf("UPDATE on %s = %v, want success — its schema comment claims it is "+
					"NOT protected against direct DB access, so a refusal here means that "+
					"comment (and docs/privacy.md) are now false", s.table, err)
			}
		})
	}

	// And the one table that IS protected.
	t.Run("cost_correction_audit", func(t *testing.T) {
		id := seedCostCorrectionAuditRow(t, db)
		if _, err := db.db.Exec(`UPDATE cost_correction_audit SET actor = 'mallory' WHERE id = ?`, id); err == nil {
			t.Error("UPDATE on cost_correction_audit succeeded — the money-rewrite ledger IS supposed to refuse it")
		}
	})
}

// TestEraseDeveloper_StillDeletesAuditRows exists so criterion 2 of the #604
// ruling cannot be re-litigated by accident. A BEFORE DELETE trigger added to
// repo_repair_audit — the audit ledger that IS in developerPIITables — would
// break GDPR Art. 17 erasure, and it would break it silently in production
// rather than loudly here. This runs the erasure end to end and asserts the
// ledger row is actually gone.
//
// It deliberately duplicates coverage TestEraseDeveloper_RemovesAllPIITables
// already has: that test is about the completeness of the PII registry, this one
// is about the ABSENCE of a delete guard. A future change would have to defeat
// both, and this one names the reason.
func TestEraseDeveloper_StillDeletesAuditRows(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	seedDeveloperPII(t, db, "alice")
	if got := countDeveloperRows(t, db, "repo_repair_audit", "alice"); got != 1 {
		t.Fatalf("setup: repo_repair_audit rows for alice = %d, want 1", got)
	}

	counts, err := db.EraseDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("EraseDeveloper = %v — if this is a trigger abort, a BEFORE DELETE trigger has been "+
			"added to an audit ledger and #604 criterion 2 has been violated", err)
	}
	if got := countDeveloperRows(t, db, "repo_repair_audit", "alice"); got != 0 {
		t.Errorf("repo_repair_audit rows for alice after erase = %d, want 0 — an audit ledger in "+
			"developerPIITables MUST remain hard-deletable for Art. 17", got)
	}
	if counts["repo_repair_audit"] != 1 {
		t.Errorf("counts[repo_repair_audit] = %d, want 1", counts["repo_repair_audit"])
	}
}

// TestCostCorrectionAuditTriggerAppliesOnUpgrade covers the path the fresh-DB
// tests above cannot reach: a database created by a PRE-#604 binary, already
// populated, opened by a binary that has the trigger. `CREATE TRIGGER IF NOT
// EXISTS` lives in schemaPostMigration precisely so it reaches such a database,
// and "it works on a fresh DB" is not evidence that it does — every existing
// deployment takes the upgrade path and only the upgrade path.
//
// It also pins idempotency across boots: a third Open must leave exactly one
// trigger and must not error.
func TestCostCorrectionAuditTriggerAppliesOnUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("initial Open: %v", err)
	}
	// Simulate a pre-#604 database: no trigger, and a ledger row already in it.
	if _, err := db.db.Exec(`DROP TRIGGER trg_cost_correction_audit_no_update`); err != nil {
		t.Fatalf("simulate pre-#604 schema: %v", err)
	}
	seedCostCorrectionAuditRow(t, db)
	// Control arm: unprotected, the UPDATE a pre-#604 binary could do succeeds.
	// Without this, a trigger that never installed would be indistinguishable
	// from one that did.
	if _, err := db.db.Exec(`UPDATE cost_correction_audit SET actor = 'pre-604'`); err != nil {
		t.Fatalf("control arm FAILED: UPDATE on the unprotected table = %v, want success", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade Open: %v", err)
	}
	defer func() { _ = db2.Close() }()
	if n := triggerCount(t, db2, "trg_cost_correction_audit_no_update"); n != 1 {
		t.Fatalf("trigger count after the upgrade Open = %d, want 1 — the guard does not reach "+
			"an existing database, so no deployed install would ever get it", n)
	}
	if _, err := db2.db.Exec(`UPDATE cost_correction_audit SET actor = 'post-604'`); err == nil {
		t.Error("UPDATE succeeded after the upgrade Open — the trigger was not installed on the existing DB")
	}

	// Idempotency across boots: IF NOT EXISTS must no-op, never duplicate or error.
	if err := db2.Close(); err != nil {
		t.Fatalf("close before third Open: %v", err)
	}
	db3, err := Open(path)
	if err != nil {
		t.Fatalf("third Open: %v", err)
	}
	defer func() { _ = db3.Close() }()
	if n := triggerCount(t, db3, "trg_cost_correction_audit_no_update"); n != 1 {
		t.Errorf("trigger count after a third Open = %d, want exactly 1", n)
	}
}

// triggerCount reports how many triggers of that name exist in the schema.
func triggerCount(t *testing.T, db *DB, name string) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?`, name,
	).Scan(&n); err != nil {
		t.Fatalf("count trigger %s: %v", name, err)
	}
	return n
}
