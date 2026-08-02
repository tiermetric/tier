package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// repriceModel is a model with a known, stable table rate used across the reprice
// tests. claude-sonnet-4 is $3/M input in the embedded default table, so a row of
// InputTok=1000 prices to 1000/1e6 * 3.0 = $0.003 = 3000 micro-dollars. The tests
// deliberately store a WRONG cost_micro at an old version and prove reprice
// recomputes to this audited figure.
const repriceModel = "claude-sonnet-4"

// insertMispricedEvent writes a token_event with an EXPLICIT (wrong) cost_micro at
// an explicit price_version, unkeyed so successive inserts never collide. The
// stored cost is intentionally not ComputeCost(model, usage) so a reprice has
// something to correct. Returns the correct (recomputed) cost for assertions.
func insertMispricedEvent(t *testing.T, db *DB, dev, issue string, version int, inputTok int, storedCost int64, ts time.Time) int64 {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), TokenEvent{
		Developer:    dev,
		IssueID:      issue,
		Model:        repriceModel,
		InputTok:     inputTok,
		CostMicro:    storedCost,
		Source:       "jsonl",
		Fidelity:     "realtime",
		PriceVersion: version,
		Timestamp:    ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent (v%d): %v", version, err)
	}
	return ComputeCost(repriceModel, CostUsage{Input: inputTok})
}

func repriceAuditRows(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM reprice_audit`).Scan(&n); err != nil {
		t.Fatalf("count reprice_audit: %v", err)
	}
	return n
}

func repriceRowAuditRows(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM reprice_row_audit`).Scan(&n); err != nil {
		t.Fatalf("count reprice_row_audit: %v", err)
	}
	return n
}

// rowID returns the token_events.id of the single row for an issue — the key the
// per-row before-image audit records against.
func rowID(t *testing.T, db *DB, issue string) int64 {
	t.Helper()
	var id int64
	if err := db.db.QueryRow(`SELECT id FROM token_events WHERE issue_id = ?`, issue).Scan(&id); err != nil {
		t.Fatalf("read id %s: %v", issue, err)
	}
	return id
}

func rowCost(t *testing.T, db *DB, issue string) (cost int64, version int) {
	t.Helper()
	if err := db.db.QueryRow(
		`SELECT cost_micro, price_version FROM token_events WHERE issue_id = ?`, issue,
	).Scan(&cost, &version); err != nil {
		t.Fatalf("read row %s: %v", issue, err)
	}
	return cost, version
}

func rowBilling(t *testing.T, db *DB, issue string) string {
	t.Helper()
	var mode string
	if err := db.db.QueryRow(
		`SELECT billing_mode FROM token_events WHERE issue_id = ?`, issue,
	).Scan(&mode); err != nil {
		t.Fatalf("read billing_mode %s: %v", issue, err)
	}
	return mode
}

// TestReprice_RequiresFromVersion proves --from-version < 1 is rejected: the
// store refuses to reprice the whole table by accident (safe-by-default).
func TestReprice_RequiresFromVersion(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	for _, v := range []int{0, -1} {
		if _, err := db.Reprice(context.Background(), RepriceOptions{FromVersion: v, Commit: true}); err == nil {
			t.Errorf("Reprice(FromVersion=%d) = nil error, want rejection", v)
		}
	}
}

// TestReprice_DryRunMutatesNothing is the core safe-by-default guarantee: a dry
// run (Commit false) reports what WOULD change but writes no cost, no version
// bump, and no audit row.
func TestReprice_DryRunMutatesNothing(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	want := insertMispricedEvent(t, db, "alice", "issue-dry", 3, 1000, 999_000, base)

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: false})
	if err != nil {
		t.Fatalf("Reprice dry-run: %v", err)
	}
	if res.Committed {
		t.Error("dry run reports Committed=true")
	}
	if res.RowCount != 1 || res.ChangedRowCount != 1 {
		t.Errorf("RowCount=%d ChangedRowCount=%d, want 1/1", res.RowCount, res.ChangedRowCount)
	}
	if res.OldCostMicroSum != 999_000 || res.NewCostMicroSum != want {
		t.Errorf("cost sums old=%d new=%d, want old=999000 new=%d", res.OldCostMicroSum, res.NewCostMicroSum, want)
	}
	if len(res.Developers) != 1 || res.Developers[0] != "alice" {
		t.Errorf("Developers=%v, want [alice]", res.Developers)
	}
	if res.RepriceID != "" {
		t.Errorf("dry run minted a RepriceID %q, want empty", res.RepriceID)
	}
	// Nothing mutated.
	if cost, ver := rowCost(t, db, "issue-dry"); cost != 999_000 || ver != 3 {
		t.Errorf("row after dry run cost=%d version=%d, want 999000/3 (unchanged)", cost, ver)
	}
	if n := repriceAuditRows(t, db); n != 0 {
		t.Errorf("dry run wrote %d audit rows, want 0", n)
	}
}

// TestReprice_CommitUpdatesAndAudits proves --commit recomputes cost_micro, bumps
// price_version to the active table version, and writes the audit ledger.
func TestReprice_CommitUpdatesAndAudits(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 13, 0, 0, 0, time.UTC)
	active := ActivePriceTableInfo().Version

	want := insertMispricedEvent(t, db, "bob", "issue-commit", 2, 1000, 999_000, base)

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true, ToolVersion: "tierd-test"})
	if err != nil {
		t.Fatalf("Reprice commit: %v", err)
	}
	if !res.Committed || res.RepriceID == "" {
		t.Errorf("Committed=%v RepriceID=%q, want true/non-empty", res.Committed, res.RepriceID)
	}
	if res.ToVersion != active {
		t.Errorf("ToVersion=%d, want active %d", res.ToVersion, active)
	}
	// Row mutated: cost recomputed, version bumped to active.
	if cost, ver := rowCost(t, db, "issue-commit"); cost != want || ver != active {
		t.Errorf("row after commit cost=%d version=%d, want %d/%d", cost, ver, want, active)
	}
	// One audit row for the single old version (2).
	var (
		auditOld, auditNew, auditFrom int
		auditRows                     int64
		auditOldSum, auditNewSum      int64
		auditTool, auditEff, auditID  string
	)
	if err := db.db.QueryRow(
		`SELECT reprice_id, from_version, old_price_version, new_price_version,
		        row_count, old_cost_micro_sum, new_cost_micro_sum,
		        tool_version, price_effective_date
		 FROM reprice_audit`,
	).Scan(&auditID, &auditFrom, &auditOld, &auditNew, &auditRows, &auditOldSum, &auditNewSum, &auditTool, &auditEff); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if auditID != res.RepriceID {
		t.Errorf("audit reprice_id=%q, want %q", auditID, res.RepriceID)
	}
	if auditFrom != 1 || auditOld != 2 || auditNew != active {
		t.Errorf("audit from/old/new = %d/%d/%d, want 1/2/%d", auditFrom, auditOld, auditNew, active)
	}
	if auditRows != 1 || auditOldSum != 999_000 || auditNewSum != want {
		t.Errorf("audit rows/oldsum/newsum = %d/%d/%d, want 1/999000/%d", auditRows, auditOldSum, auditNewSum, want)
	}
	if auditTool != "tierd-test" || auditEff != ActivePriceTableInfo().EffectiveDate {
		t.Errorf("audit tool/effdate = %q/%q, want tierd-test/%q", auditTool, auditEff, ActivePriceTableInfo().EffectiveDate)
	}
}

// TestReprice_FromVersionFloorSelectsRows proves --from-version N selects only
// rows with price_version >= N: a row below the floor is left untouched.
func TestReprice_FromVersionFloorSelectsRows(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)

	wantBelow := insertMispricedEvent(t, db, "carol", "issue-v1", 1, 1000, 999_000, base)
	wantAbove := insertMispricedEvent(t, db, "carol", "issue-v5", 5, 1000, 888_000, base.Add(time.Minute))
	_ = wantBelow

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 5, Commit: true})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	if res.RowCount != 1 || res.ChangedRowCount != 1 {
		t.Errorf("RowCount=%d Changed=%d, want 1/1 (only the v5 row is >= floor)", res.RowCount, res.ChangedRowCount)
	}
	// The below-floor v1 row is untouched.
	if cost, ver := rowCost(t, db, "issue-v1"); cost != 999_000 || ver != 1 {
		t.Errorf("below-floor row cost=%d version=%d, want 999000/1 (untouched)", cost, ver)
	}
	// The v5 row is repriced.
	if cost, _ := rowCost(t, db, "issue-v5"); cost != wantAbove {
		t.Errorf("v5 row cost=%d, want %d (repriced)", cost, wantAbove)
	}
}

// TestReprice_SameTableNoOp proves repricing rows already priced under the active
// table with a correct cost is a genuine no-op: ChangedRowCount 0 and no audit.
func TestReprice_SameTableNoOp(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	active := ActivePriceTableInfo().Version

	// Store the row at the active version with its CORRECT audited cost.
	correct := ComputeCost(repriceModel, CostUsage{Input: 1000})
	insertMispricedEvent(t, db, "dave", "issue-noop", active, 1000, correct, base)

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	if res.RowCount != 1 {
		t.Errorf("RowCount=%d, want 1", res.RowCount)
	}
	if res.ChangedRowCount != 0 {
		t.Errorf("ChangedRowCount=%d, want 0 (already correctly priced under active table)", res.ChangedRowCount)
	}
	if n := repriceAuditRows(t, db); n != 0 {
		t.Errorf("no-op reprice wrote %d audit rows, want 0", n)
	}
	// The --commit was honored (Committed true), but with no mutation there is no
	// audit row and thus no correlation id — RepriceID is non-empty IFF the ledger
	// gained rows.
	if !res.Committed {
		t.Error("no-op commit reports Committed=false, want true (the commit ran)")
	}
	if res.RepriceID != "" {
		t.Errorf("no-op commit minted RepriceID %q, want empty (no audit row written)", res.RepriceID)
	}
}

// TestReprice_ReconcilesActiveVersionRow proves the placeholder-promotion edge
// (#233/#294): a row already at the active price_version but carrying a WRONG
// cost_micro is reconciled — cost recomputes, version stays active, and an audit
// row records old==new version with a cost delta.
func TestReprice_ReconcilesActiveVersionRow(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 16, 0, 0, 0, time.UTC)
	active := ActivePriceTableInfo().Version

	want := insertMispricedEvent(t, db, "erin", "issue-reconcile", active, 1000, 1, base) // stored cost 1 micro, wrong

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	if res.ChangedRowCount != 1 {
		t.Errorf("ChangedRowCount=%d, want 1 (active-version row reconciled)", res.ChangedRowCount)
	}
	if cost, ver := rowCost(t, db, "issue-reconcile"); cost != want || ver != active {
		t.Errorf("reconciled row cost=%d version=%d, want %d/%d", cost, ver, want, active)
	}
	// Audit row: old == new == active, delta recorded.
	var old, nw int
	if err := db.db.QueryRow(`SELECT old_price_version, new_price_version FROM reprice_audit`).Scan(&old, &nw); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if old != active || nw != active {
		t.Errorf("audit old/new = %d/%d, want %d/%d (same version, cost reconciled)", old, nw, active, active)
	}
}

// TestReprice_MultipleVersionsBreakdown proves one operation spanning several old
// price_versions writes one audit row per version, each with its own totals.
func TestReprice_MultipleVersionsBreakdown(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 17, 0, 0, 0, time.UTC)

	insertMispricedEvent(t, db, "alice", "issue-a", 3, 1000, 100_000, base)
	insertMispricedEvent(t, db, "bob", "issue-b", 4, 1000, 200_000, base.Add(time.Minute))
	insertMispricedEvent(t, db, "bob", "issue-c", 4, 1000, 300_000, base.Add(2*time.Minute))

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	if len(res.ByOldVersion) != 2 {
		t.Fatalf("ByOldVersion len=%d, want 2 (versions 3 and 4)", len(res.ByOldVersion))
	}
	if res.ByOldVersion[0].OldPriceVersion != 3 || res.ByOldVersion[0].RowCount != 1 {
		t.Errorf("v3 delta = %+v, want version 3 count 1", res.ByOldVersion[0])
	}
	if res.ByOldVersion[1].OldPriceVersion != 4 || res.ByOldVersion[1].RowCount != 2 {
		t.Errorf("v4 delta = %+v, want version 4 count 2", res.ByOldVersion[1])
	}
	if n := repriceAuditRows(t, db); n != 2 {
		t.Errorf("audit rows=%d, want 2 (one per old version)", n)
	}
	// Two distinct developers among changed rows.
	if len(res.Developers) != 2 {
		t.Errorf("Developers=%v, want 2 distinct", res.Developers)
	}
}

// TestReprice_ImmutableInsertPathUnchanged proves the #233 guarantee still holds:
// the NORMAL insert path never reprices on replay — only Reprice mutates a stored
// cost. A higher-priced replay of the same idempotency key leaves cost frozen;
// Reprice is then the only thing that moves it.
func TestReprice_ImmutableInsertPathUnchanged(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 18, 0, 0, 0, time.UTC)
	const key = "msg-reprice-immutable"

	first := TokenEvent{
		Developer: "frank", IssueID: "issue-imm", Model: repriceModel,
		InputTok: 1000, CostMicro: 10_000, Source: "jsonl", Fidelity: "realtime",
		IdempotencyKey: key, PriceVersion: 2, Timestamp: base,
	}
	if err := db.InsertTokenEvent(ctx, first); err != nil {
		t.Fatalf("insert first: %v", err)
	}
	// Higher-priced replay: insert path must NOT reprice (immutable, #233).
	replay := first
	replay.CostMicro = 99_000
	replay.PriceVersion = 9
	if err := db.InsertTokenEvent(ctx, replay); err != nil {
		t.Fatalf("insert replay: %v", err)
	}
	if cost, ver := rowCost(t, db, "issue-imm"); cost != 10_000 || ver != 2 {
		t.Fatalf("after replay cost=%d version=%d, want 10000/2 (insert path immutable)", cost, ver)
	}
	// Reprice is the sanctioned mutator — it DOES move the cost.
	want := ComputeCost(repriceModel, CostUsage{Input: 1000})
	if _, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true}); err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	if cost, _ := rowCost(t, db, "issue-imm"); cost != want {
		t.Errorf("after reprice cost=%d, want %d (reprice is the only sanctioned mutator)", cost, want)
	}
}

// TestReprice_CancelledBeforeBeginWritesNothing proves the pre-begin cancellation
// path: a context already cancelled when Reprice is called fails at BeginTx and
// writes nothing. This is NOT the atomicity proof (no writes ever execute) — that
// is TestReprice_AuditFailureRollsBackAtomically below, which forces a failure
// AFTER row updates have run inside the transaction.
func TestReprice_CancelledBeforeBeginWritesNothing(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	base := time.Date(2026, 7, 10, 19, 0, 0, 0, time.UTC)

	insertMispricedEvent(t, db, "grace", "issue-cancel", 3, 1000, 999_000, base)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE the reprice runs — BeginTx returns ctx.Err()

	if _, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true}); err == nil {
		t.Error("Reprice with cancelled context = nil error, want failure")
	}
	if cost, ver := rowCost(t, db, "issue-cancel"); cost != 999_000 || ver != 3 {
		t.Errorf("row after failed reprice cost=%d version=%d, want 999000/3 (unchanged)", cost, ver)
	}
	if n := repriceAuditRows(t, db); n != 0 {
		t.Errorf("failed reprice wrote %d audit rows, want 0", n)
	}
}

// TestReprice_AuditFailureRollsBackAtomically is the real atomicity proof: it
// forces the audit INSERT to fail AFTER every row UPDATE has already executed
// inside the transaction (a BEFORE-INSERT trigger that aborts), then asserts EVERY
// updated row AND the audit ledger rolled back together — no partial reprice. This
// exercises the update-then-audit-then-commit path the context-cancel test cannot.
func TestReprice_AuditFailureRollsBackAtomically(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 20, 0, 0, 0, time.UTC)

	// Two changed rows, so the proof covers multi-row rollback (both UPDATEs run
	// before the first audit INSERT is attempted).
	insertMispricedEvent(t, db, "alice", "issue-a", 3, 1000, 999_000, base)
	insertMispricedEvent(t, db, "bob", "issue-b", 3, 1000, 888_000, base.Add(time.Minute))

	// Abort the audit write. RAISE(ABORT) aborts the statement; the transaction is
	// then rolled back by Reprice's deferred Rollback (committed stays false).
	if _, err := db.db.ExecContext(ctx,
		`CREATE TRIGGER trg_block_audit BEFORE INSERT ON reprice_audit BEGIN SELECT RAISE(ABORT, 'blocked'); END`,
	); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	defer func() { _, _ = db.db.ExecContext(ctx, `DROP TRIGGER trg_block_audit`) }()

	if _, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true}); err == nil {
		t.Fatal("Reprice with a blocked audit insert = nil error, want failure")
	}

	// Both rows' UPDATEs (which executed inside the tx before the audit INSERT) must
	// be rolled back to their original cost/version.
	if cost, ver := rowCost(t, db, "issue-a"); cost != 999_000 || ver != 3 {
		t.Errorf("issue-a after failed commit cost=%d version=%d, want 999000/3 (rolled back)", cost, ver)
	}
	if cost, ver := rowCost(t, db, "issue-b"); cost != 888_000 || ver != 3 {
		t.Errorf("issue-b after failed commit cost=%d version=%d, want 888000/3 (rolled back)", cost, ver)
	}
	if n := repriceAuditRows(t, db); n != 0 {
		t.Errorf("failed commit left %d audit rows, want 0 (rolled back atomically with the updates)", n)
	}
	// The per-row before-images (written earlier in the same tx) rolled back too.
	if n := repriceRowAuditRows(t, db); n != 0 {
		t.Errorf("failed commit left %d row-audit rows, want 0 (rolled back atomically)", n)
	}
}

// TestReprice_VersionOnlyBump proves the "correct cost, old version" quadrant: a
// row whose cost already recomputes identically but sits below the active version
// is still CHANGED (a provenance bump), gets its version moved to active, and is
// audited with a zero cost delta.
func TestReprice_VersionOnlyBump(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 21, 0, 0, 0, time.UTC)
	active := ActivePriceTableInfo().Version

	// Correct cost, but stamped one version below active.
	correct := ComputeCost(repriceModel, CostUsage{Input: 1000})
	insertMispricedEvent(t, db, "heidi", "issue-vbump", active-1, 1000, correct, base)

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	if res.ChangedRowCount != 1 {
		t.Errorf("ChangedRowCount=%d, want 1 (version-only bump is still a change)", res.ChangedRowCount)
	}
	if cost, ver := rowCost(t, db, "issue-vbump"); cost != correct || ver != active {
		t.Errorf("row cost=%d version=%d, want %d/%d (cost unchanged, version bumped)", cost, ver, correct, active)
	}
	// Audit records a zero cost delta.
	if len(res.ByOldVersion) != 1 {
		t.Fatalf("ByOldVersion len=%d, want 1", len(res.ByOldVersion))
	}
	if d := res.ByOldVersion[0]; d.OldCostMicroSum != d.NewCostMicroSum {
		t.Errorf("audit cost sums old=%d new=%d, want equal (version-only bump)", d.OldCostMicroSum, d.NewCostMicroSum)
	}
}

// TestReprice_BillingModeReconciled proves billing_mode is part of the change
// predicate AND is rewritten on commit: a row already at the active version with a
// correct cost but a WRONG billing_mode is detected as changed and its billing_mode
// is re-resolved from the active table.
func TestReprice_BillingModeReconciled(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 22, 0, 0, 0, time.UTC)
	active := ActivePriceTableInfo().Version

	correct := ComputeCost(repriceModel, CostUsage{Input: 1000})
	// Correct cost, active version, but a bogus billing_mode ("subscription").
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer: "ivan", IssueID: "issue-billing", Model: repriceModel,
		InputTok: 1000, CostMicro: correct, Source: "jsonl", Fidelity: "realtime",
		PriceVersion: active, BillingMode: BillingSubscription, Timestamp: base,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got := rowBilling(t, db, "issue-billing"); got != BillingSubscription {
		t.Fatalf("seed billing_mode=%q, want %q", got, BillingSubscription)
	}

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	if res.ChangedRowCount != 1 {
		t.Errorf("ChangedRowCount=%d, want 1 (billing_mode differs even though cost/version match)", res.ChangedRowCount)
	}
	// claude-sonnet-4 resolves to per_token, so the bogus subscription mode is fixed.
	if got := rowBilling(t, db, "issue-billing"); got != BillingPerToken {
		t.Errorf("billing_mode after reprice=%q, want %q", got, BillingPerToken)
	}
}

// TestReprice_OutputAndCacheTokens proves the recompute feeds ALL token components
// (output + cache), not just input, to the pricing function.
func TestReprice_OutputAndCacheTokens(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 23, 0, 0, 0, time.UTC)

	usage := CostUsage{Input: 1000, Output: 500, CacheRead: 2000, CacheWrite5m: 300, CacheWrite1h: 100}
	want := ComputeCost(repriceModel, usage)
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer: "judy", IssueID: "issue-multi", Model: repriceModel,
		InputTok: usage.Input, OutputTok: usage.Output, CacheRead: usage.CacheRead,
		CacheWrite5m: usage.CacheWrite5m, CacheWrite1h: usage.CacheWrite1h,
		CostMicro: 1, Source: "jsonl", Fidelity: "realtime", PriceVersion: 3, Timestamp: base,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	if res.NewCostMicroSum != want {
		t.Errorf("NewCostMicroSum=%d, want %d (all token components priced)", res.NewCostMicroSum, want)
	}
	if cost, _ := rowCost(t, db, "issue-multi"); cost != want {
		t.Errorf("row cost after reprice=%d, want %d", cost, want)
	}
}

// TestReprice_GuessedModelFlagged proves a changed row whose model has no exact
// active-table entry is counted in GuessedRowCount, so the caller can warn that
// audited history is being rewritten to an estimate.
func TestReprice_GuessedModelFlagged(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 11, 1, 0, 0, 0, time.UTC)

	// A model not in the price table and with no param-count heuristic → the flat
	// self-hosted-medium GUESS fallback.
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer: "ken", IssueID: "issue-guess", Model: "totally-unknown-model-xyz",
		InputTok: 1000, CostMicro: 999_000, Source: "jsonl", Fidelity: "realtime",
		PriceVersion: 3, Timestamp: base,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: false})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	if res.ChangedRowCount != 1 || res.GuessedRowCount != 1 {
		t.Errorf("ChangedRowCount=%d GuessedRowCount=%d, want 1/1 (unknown model priced by guess)", res.ChangedRowCount, res.GuessedRowCount)
	}
}

// TestReprice_EmptyDBCommit proves a commit against a DB with no candidate rows is
// a clean no-op: no error, nothing examined/changed, no audit, no RepriceID.
func TestReprice_EmptyDBCommit(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	res, err := db.Reprice(context.Background(), RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatalf("Reprice on empty DB: %v", err)
	}
	if res.RowCount != 0 || res.ChangedRowCount != 0 {
		t.Errorf("RowCount=%d ChangedRowCount=%d, want 0/0", res.RowCount, res.ChangedRowCount)
	}
	if !res.Committed || res.RepriceID != "" {
		t.Errorf("Committed=%v RepriceID=%q, want true/empty", res.Committed, res.RepriceID)
	}
	if n := repriceAuditRows(t, db); n != 0 {
		t.Errorf("empty-DB commit wrote %d audit rows, want 0", n)
	}
}

// readRowAudit reads the single reprice_row_audit before-image for a token_event
// id, returning its captured old cost/version/billing and the owning reprice_id.
func readRowAudit(t *testing.T, db *DB, tokenEventID int64) (repriceID string, oldCost int64, oldVersion int, oldBilling string) {
	t.Helper()
	if err := db.db.QueryRow(
		`SELECT reprice_id, old_cost_micro, old_price_version, old_billing_mode
		 FROM reprice_row_audit WHERE token_event_id = ?`, tokenEventID,
	).Scan(&repriceID, &oldCost, &oldVersion, &oldBilling); err != nil {
		t.Fatalf("read reprice_row_audit for token_event %d: %v", tokenEventID, err)
	}
	return repriceID, oldCost, oldVersion, oldBilling
}

// TestReprice_CommitWritesPerRowBeforeImage proves --commit captures, for EACH
// changed row, that row's EXACT pre-reprice cost/version/billing_mode in
// reprice_row_audit — keyed by token_event_id and the run's reprice_id — in the
// same transaction as the updates. This is the durable record a surgical
// inverse-undo restores from.
func TestReprice_CommitWritesPerRowBeforeImage(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 11, 3, 0, 0, 0, time.UTC)

	insertMispricedEvent(t, db, "alice", "issue-a", 3, 1000, 999_000, base)
	insertMispricedEvent(t, db, "bob", "issue-b", 4, 1000, 888_000, base.Add(time.Minute))

	// Snapshot the exact pre-reprice values so the assertion is independent of
	// insert-path defaults.
	idA, idB := rowID(t, db, "issue-a"), rowID(t, db, "issue-b")
	preCostA, preVerA := rowCost(t, db, "issue-a")
	preBillA := rowBilling(t, db, "issue-a")
	preCostB, preVerB := rowCost(t, db, "issue-b")
	preBillB := rowBilling(t, db, "issue-b")

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatalf("Reprice commit: %v", err)
	}
	// One before-image per CHANGED row.
	if n := repriceRowAuditRows(t, db); int64(n) != res.ChangedRowCount || n != 2 {
		t.Fatalf("reprice_row_audit rows=%d, want 2 (== ChangedRowCount %d)", n, res.ChangedRowCount)
	}
	// Each before-image holds the row's EXACT old values, keyed to this run.
	ridA, oldCostA, oldVerA, oldBillA := readRowAudit(t, db, idA)
	if ridA != res.RepriceID || oldCostA != preCostA || oldVerA != preVerA || oldBillA != preBillA {
		t.Errorf("issue-a before-image = (rid %q, cost %d, ver %d, bill %q), want (%q, %d, %d, %q)",
			ridA, oldCostA, oldVerA, oldBillA, res.RepriceID, preCostA, preVerA, preBillA)
	}
	ridB, oldCostB, oldVerB, oldBillB := readRowAudit(t, db, idB)
	if ridB != res.RepriceID || oldCostB != preCostB || oldVerB != preVerB || oldBillB != preBillB {
		t.Errorf("issue-b before-image = (rid %q, cost %d, ver %d, bill %q), want (%q, %d, %d, %q)",
			ridB, oldCostB, oldVerB, oldBillB, res.RepriceID, preCostB, preVerB, preBillB)
	}
	// The rows themselves were repriced away from those old values (sanity: the
	// before-image is a genuine BEFORE, not the current state).
	if cost, _ := rowCost(t, db, "issue-a"); cost == preCostA {
		t.Errorf("issue-a cost unchanged at %d — before-image would be meaningless", cost)
	}
}

// TestReprice_RowAuditRollsBackOnMidTxFailure proves the per-row before-image
// participates in the SAME atomic transaction as the row UPDATEs, INCLUDING an
// already-fully-applied earlier row. The commit loop writes each row's before-image
// then its UPDATE, in id order; the trigger here aborts only the SECOND row's
// before-image, so the FIRST row's before-image AND UPDATE have already executed
// inside the tx when the failure hits. Asserting the first row rolled back proves
// the atomic unit spans committed-then-failed rows, not just the failing statement.
func TestReprice_RowAuditRollsBackOnMidTxFailure(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 11, 4, 0, 0, 0, time.UTC)

	insertMispricedEvent(t, db, "alice", "issue-a", 3, 1000, 999_000, base)
	insertMispricedEvent(t, db, "bob", "issue-b", 3, 1000, 888_000, base.Add(time.Minute))
	// issue-a has the lower id, so the commit loop (ORDER BY id) fully applies it
	// — before-image + UPDATE — BEFORE it reaches issue-b, whose before-image aborts.
	idB := rowID(t, db, "issue-b")

	// SQLite trigger bodies cannot bind ? parameters, so the id is inlined. idB is a
	// trusted int64 from our own INSERT (no injection surface).
	if _, err := db.db.ExecContext(ctx, fmt.Sprintf(
		`CREATE TRIGGER trg_block_row_audit BEFORE INSERT ON reprice_row_audit
		 WHEN NEW.token_event_id = %d BEGIN SELECT RAISE(ABORT, 'blocked'); END`, idB,
	)); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	defer func() { _, _ = db.db.ExecContext(ctx, `DROP TRIGGER trg_block_row_audit`) }()

	if _, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true}); err == nil {
		t.Fatal("Reprice with a blocked before-image insert = nil error, want failure")
	}
	// The FIRST row was fully applied (before-image + UPDATE) inside the tx before
	// the second row aborted — it must still roll back to its stored cost/version.
	if cost, ver := rowCost(t, db, "issue-a"); cost != 999_000 || ver != 3 {
		t.Errorf("issue-a (fully applied then rolled back) cost=%d version=%d, want 999000/3", cost, ver)
	}
	if cost, ver := rowCost(t, db, "issue-b"); cost != 888_000 || ver != 3 {
		t.Errorf("issue-b after failed commit cost=%d version=%d, want 888000/3 (rolled back)", cost, ver)
	}
	if n := repriceRowAuditRows(t, db); n != 0 {
		t.Errorf("failed commit left %d row-audit rows, want 0 (the first row's before-image rolled back too)", n)
	}
	if n := repriceAuditRows(t, db); n != 0 {
		t.Errorf("failed commit left %d aggregate audit rows, want 0 (rolled back)", n)
	}
}

// TestReprice_SurgicalInverseFromRowAudit proves the reprice_row_audit before-images
// enable a SURGICAL inverse-undo that survives continued ingestion: after a commit
// and a fresh ingest, restoring each changed row to its recorded old values (by
// token_event_id) recovers the exact pre-reprice cost/version/billing WITHOUT
// touching the newly-ingested row.
func TestReprice_SurgicalInverseFromRowAudit(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 11, 5, 0, 0, 0, time.UTC)

	insertMispricedEvent(t, db, "alice", "issue-a", 3, 1000, 999_000, base)
	preCostA, preVerA := rowCost(t, db, "issue-a")
	preBillA := rowBilling(t, db, "issue-a")

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatalf("Reprice commit: %v", err)
	}
	if cost, _ := rowCost(t, db, "issue-a"); cost == preCostA {
		t.Fatalf("row not repriced — nothing to invert")
	}

	// Continued ingestion AFTER the reprice: a new row a whole-file snapshot restore
	// would clobber, but a surgical row-scoped inverse must leave alone.
	insertMispricedEvent(t, db, "carol", "issue-new", 3, 1000, 777_000, base.Add(time.Hour))
	newCost, newVer := rowCost(t, db, "issue-new")

	// Apply the surgical inverse straight from the before-image ledger, scoped to
	// this run's reprice_id and keyed by token_event_id.
	if _, err := db.db.ExecContext(ctx, `
		UPDATE token_events
		SET cost_micro = (SELECT old_cost_micro FROM reprice_row_audit r WHERE r.token_event_id = token_events.id AND r.reprice_id = ?),
		    price_version = (SELECT old_price_version FROM reprice_row_audit r WHERE r.token_event_id = token_events.id AND r.reprice_id = ?),
		    billing_mode = (SELECT old_billing_mode FROM reprice_row_audit r WHERE r.token_event_id = token_events.id AND r.reprice_id = ?)
		WHERE id IN (SELECT token_event_id FROM reprice_row_audit WHERE reprice_id = ?)`,
		res.RepriceID, res.RepriceID, res.RepriceID, res.RepriceID,
	); err != nil {
		t.Fatalf("apply surgical inverse: %v", err)
	}

	// The repriced row is back to its EXACT pre-reprice state.
	if cost, ver := rowCost(t, db, "issue-a"); cost != preCostA || ver != preVerA {
		t.Errorf("issue-a after inverse cost=%d version=%d, want %d/%d (exact old values restored)", cost, ver, preCostA, preVerA)
	}
	if bill := rowBilling(t, db, "issue-a"); bill != preBillA {
		t.Errorf("issue-a after inverse billing=%q, want %q", bill, preBillA)
	}
	// The row ingested after the reprice is untouched by the surgical inverse.
	if cost, ver := rowCost(t, db, "issue-new"); cost != newCost || ver != newVer {
		t.Errorf("issue-new after inverse cost=%d version=%d, want %d/%d (untouched — survives ingestion)", cost, ver, newCost, newVer)
	}
}

// TestReprice_CommitBlocksGuessedWithoutAllowGuessed proves the GUESS gate: a
// --commit that would rewrite real historical cost to a size-class GUESS estimate
// (a model not in the active table) FAILS unless AllowGuessed is set — and the gate
// protects the WHOLE batch, so a co-changed KNOWN-model row is left unmutated too
// (the gate fires before any write). The error names the offending model so the
// operator knows exactly what to add to the price table.
func TestReprice_CommitBlocksGuessedWithoutAllowGuessed(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 11, 6, 0, 0, 0, time.UTC)

	// One GUESSED-model row AND one normal (known-model) mispriced row in the batch.
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer: "ken", IssueID: "issue-guess", Model: "totally-unknown-model-xyz",
		InputTok: 1000, CostMicro: 999_000, Source: "jsonl", Fidelity: "realtime",
		PriceVersion: 3, Timestamp: base,
	}); err != nil {
		t.Fatalf("insert guessed: %v", err)
	}
	insertMispricedEvent(t, db, "alice", "issue-known", 3, 1000, 555_000, base.Add(time.Minute))

	_, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true})
	if err == nil {
		t.Fatal("Reprice --commit with a guessed row and no AllowGuessed = nil error, want failure")
	}
	if !strings.Contains(err.Error(), "allow-guessed") {
		t.Errorf("error = %q, want it to mention --allow-guessed", err.Error())
	}
	if !strings.Contains(err.Error(), "totally-unknown-model-xyz") {
		t.Errorf("error = %q, want it to NAME the offending model (actionable)", err.Error())
	}
	// The guessed row is unmutated...
	if cost, ver := rowCost(t, db, "issue-guess"); cost != 999_000 || ver != 3 {
		t.Errorf("guessed row after blocked commit cost=%d version=%d, want 999000/3 (unmutated)", cost, ver)
	}
	// ...AND the co-changed KNOWN-model row is unmutated (whole-batch protection).
	if cost, ver := rowCost(t, db, "issue-known"); cost != 555_000 || ver != 3 {
		t.Errorf("known-model row after blocked commit cost=%d version=%d, want 555000/3 (gate protected the whole batch)", cost, ver)
	}
	if n := repriceAuditRows(t, db); n != 0 {
		t.Errorf("blocked commit wrote %d aggregate audit rows, want 0", n)
	}
	if n := repriceRowAuditRows(t, db); n != 0 {
		t.Errorf("blocked commit wrote %d row-audit rows, want 0", n)
	}
}

// TestReprice_CommitProceedsWithAllowGuessed proves AllowGuessed lets the same
// guessed commit through: the row is repriced, both ledgers are written, and
// GuessedRowCount still surfaces (the warning stands).
func TestReprice_CommitProceedsWithAllowGuessed(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 11, 7, 0, 0, 0, time.UTC)

	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer: "ken", IssueID: "issue-guess", Model: "totally-unknown-model-xyz",
		InputTok: 1000, CostMicro: 999_000, Source: "jsonl", Fidelity: "realtime",
		PriceVersion: 3, Timestamp: base,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	id := rowID(t, db, "issue-guess")
	wantGuess, _ := ComputeCostHost("", "totally-unknown-model-xyz", CostUsage{Input: 1000})

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true, AllowGuessed: true})
	if err != nil {
		t.Fatalf("Reprice --commit --allow-guessed: %v", err)
	}
	if !res.Committed || res.GuessedRowCount != 1 {
		t.Errorf("Committed=%v GuessedRowCount=%d, want true/1 (proceeded, still flagged)", res.Committed, res.GuessedRowCount)
	}
	if len(res.GuessedModels) != 1 || res.GuessedModels[0] != "totally-unknown-model-xyz" {
		t.Errorf("GuessedModels=%v, want [totally-unknown-model-xyz]", res.GuessedModels)
	}
	// Row repriced to the EXACT guess estimate (not merely "changed").
	if cost, _ := rowCost(t, db, "issue-guess"); cost != wantGuess {
		t.Errorf("row cost after allow-guessed commit=%d, want %d (the guess estimate)", cost, wantGuess)
	}
	// And the before-image closed the loop: it holds the original 999000/v3.
	if _, oldCost, oldVer, _ := readRowAudit(t, db, id); oldCost != 999_000 || oldVer != 3 {
		t.Errorf("before-image cost=%d ver=%d, want 999000/3", oldCost, oldVer)
	}
	if n := repriceAuditRows(t, db); n != 1 {
		t.Errorf("allow-guessed commit wrote %d aggregate audit rows, want 1", n)
	}
	if n := repriceRowAuditRows(t, db); n != 1 {
		t.Errorf("allow-guessed commit wrote %d row-audit rows, want 1", n)
	}
}

// TestReprice_BeforeImageCapturesOldBillingMode proves the before-image records
// the row's PRE-reprice billing_mode, not the post value. Uses the billing-only
// change quadrant: correct cost + active version + a WRONG billing_mode. Since
// claude-sonnet-4 recomputes to per_token, an impl that mistakenly stored the NEW
// billing_mode would record "per_token" and this test would fail — closing the
// tautology where before==after hides the bug.
func TestReprice_BeforeImageCapturesOldBillingMode(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	active := ActivePriceTableInfo().Version

	correct := ComputeCost(repriceModel, CostUsage{Input: 1000})
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer: "ivan", IssueID: "issue-bill", Model: repriceModel,
		InputTok: 1000, CostMicro: correct, Source: "jsonl", Fidelity: "realtime",
		PriceVersion: active, BillingMode: BillingSubscription, Timestamp: base,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	id := rowID(t, db, "issue-bill")

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	// Row's billing_mode was reconciled to per_token...
	if got := rowBilling(t, db, "issue-bill"); got != BillingPerToken {
		t.Fatalf("row billing_mode after reprice=%q, want %q", got, BillingPerToken)
	}
	// ...but the before-image preserves the OLD subscription mode (a genuine BEFORE).
	rid, oldCost, oldVer, oldBill := readRowAudit(t, db, id)
	if oldBill != BillingSubscription {
		t.Errorf("before-image old_billing_mode=%q, want %q (the PRE-reprice value, not post)", oldBill, BillingSubscription)
	}
	if rid != res.RepriceID || oldCost != correct || oldVer != active {
		t.Errorf("before-image (rid %q, cost %d, ver %d), want (%q, %d, %d)", rid, oldCost, oldVer, res.RepriceID, correct, active)
	}
}

// TestReprice_BeforeImageAcrossChangeQuadrants proves the before-image captures the
// correct pre-values for the reconcile quadrant (active version, WRONG cost) and the
// version-only-bump quadrant (correct cost, OLD version) — not just the plain
// cost+version-change quadrant the main before-image test covers.
func TestReprice_BeforeImageAcrossChangeQuadrants(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	active := ActivePriceTableInfo().Version
	correct := ComputeCost(repriceModel, CostUsage{Input: 1000})

	// Reconcile quadrant: active version, wrong cost (1 micro).
	insertMispricedEvent(t, db, "erin", "issue-reconcile", active, 1000, 1, base)
	// Version-only-bump quadrant: correct cost, one version below active.
	insertMispricedEvent(t, db, "heidi", "issue-vbump", active-1, 1000, correct, base.Add(time.Minute))
	idReconcile := rowID(t, db, "issue-reconcile")
	idVbump := rowID(t, db, "issue-vbump")

	if _, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true}); err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	// Reconcile before-image: old cost 1, old version == active.
	if _, oldCost, oldVer, _ := readRowAudit(t, db, idReconcile); oldCost != 1 || oldVer != active {
		t.Errorf("reconcile before-image cost=%d ver=%d, want 1/%d", oldCost, oldVer, active)
	}
	// Version-only-bump before-image: old cost == correct, old version == active-1.
	if _, oldCost, oldVer, _ := readRowAudit(t, db, idVbump); oldCost != correct || oldVer != active-1 {
		t.Errorf("vbump before-image cost=%d ver=%d, want %d/%d", oldCost, oldVer, correct, active-1)
	}
}

// TestReprice_SecondCommitIsCleanNoOpNoNewBeforeImage proves reprice is idempotent:
// a second --commit over the same (now correctly-priced) rows is a no-op that writes
// NO new before-image, so re-running never double-writes reprice_row_audit.
func TestReprice_SecondCommitIsCleanNoOpNoNewBeforeImage(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)

	insertMispricedEvent(t, db, "alice", "issue-a", 3, 1000, 999_000, base)
	if _, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true}); err != nil {
		t.Fatalf("first Reprice: %v", err)
	}
	if n := repriceRowAuditRows(t, db); n != 1 {
		t.Fatalf("after first commit row-audit rows=%d, want 1", n)
	}

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatalf("second Reprice: %v", err)
	}
	if res.ChangedRowCount != 0 || res.RepriceID != "" {
		t.Errorf("second commit ChangedRowCount=%d RepriceID=%q, want 0/empty (clean no-op)", res.ChangedRowCount, res.RepriceID)
	}
	if n := repriceRowAuditRows(t, db); n != 1 {
		t.Errorf("second commit changed row-audit count to %d, want 1 (no new before-image)", n)
	}
}

// TestReprice_SecondGenuineRepriceWritesSecondBeforeImage proves UNIQUE(reprice_id,
// token_event_id) is per-RUN, not per-row: a second genuine reprice of the same row
// (drift re-introduced) mints a fresh reprice_id and writes a SECOND before-image for
// the same token_event_id — so an inverse chain across successive reprices is possible.
func TestReprice_SecondGenuineRepriceWritesSecondBeforeImage(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 11, 11, 0, 0, 0, time.UTC)

	insertMispricedEvent(t, db, "alice", "issue-a", 3, 1000, 999_000, base)
	id := rowID(t, db, "issue-a")

	res1, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatalf("first Reprice: %v", err)
	}
	// Re-introduce drift on the SAME row (as if a bad ingest re-underpriced it), so a
	// second reprice has a genuine change to make.
	if _, err := db.db.ExecContext(ctx,
		`UPDATE token_events SET cost_micro = 42, price_version = 1 WHERE id = ?`, id,
	); err != nil {
		t.Fatalf("re-introduce drift: %v", err)
	}
	res2, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatalf("second Reprice: %v", err)
	}
	if res2.RepriceID == res1.RepriceID || res2.RepriceID == "" {
		t.Fatalf("second run reprice_id=%q, want a fresh non-empty id distinct from %q", res2.RepriceID, res1.RepriceID)
	}
	// Two before-images now exist for the one token_event_id — one per run.
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM reprice_row_audit WHERE token_event_id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count before-images: %v", err)
	}
	if n != 2 {
		t.Errorf("before-images for token_event %d = %d, want 2 (one per run — UNIQUE is per-run)", id, n)
	}
	// The second run's before-image captured the drift value (42), proving it is that
	// run's genuine BEFORE, not a stale copy of the first.
	var second int64
	if err := db.db.QueryRow(
		`SELECT old_cost_micro FROM reprice_row_audit WHERE token_event_id = ? AND reprice_id = ?`,
		id, res2.RepriceID,
	).Scan(&second); err != nil {
		t.Fatalf("read second before-image: %v", err)
	}
	if second != 42 {
		t.Errorf("second before-image old_cost_micro=%d, want 42 (the drift value it repriced away)", second)
	}
}

// TestReprice_ByOldVersionSorted proves the per-version breakdown is sorted
// ascending regardless of insertion/id order (inserts v5 before v2).
func TestReprice_ByOldVersionSorted(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 7, 11, 2, 0, 0, 0, time.UTC)

	// Insert the HIGHER version first so a non-sorting implementation would return
	// [5, 2] in id order.
	insertMispricedEvent(t, db, "alice", "issue-hi", 5, 1000, 111_000, base)
	insertMispricedEvent(t, db, "bob", "issue-lo", 2, 1000, 222_000, base.Add(time.Minute))

	res, err := db.Reprice(ctx, RepriceOptions{FromVersion: 1, Commit: false})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	if len(res.ByOldVersion) != 2 {
		t.Fatalf("ByOldVersion len=%d, want 2", len(res.ByOldVersion))
	}
	if res.ByOldVersion[0].OldPriceVersion != 2 || res.ByOldVersion[1].OldPriceVersion != 5 {
		t.Errorf("ByOldVersion order = [%d, %d], want [2, 5] (ascending, not id order)",
			res.ByOldVersion[0].OldPriceVersion, res.ByOldVersion[1].OldPriceVersion)
	}
}
