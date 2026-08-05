package store

// Tests for RepairRepo (#493).
//
// The measured reality on the workstation that motivated this issue is that
// ZERO unqualified rows remain in either token_events or outcomes, so there is
// no real data to exercise the repair against. Every test here therefore builds
// its own synthetic fixture — the issue body's reported event count and unqualified
// share are stale and must not be treated as a fixture source.
//
// Each guard below is paired with a control arm: the positive assertion proves
// the repair does its job, and the control proves the guard is what stops it
// doing more.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/repoid"
)

// repairSeed describes one synthetic token_events row for a repair fixture.
type repairSeed struct {
	developer string
	sessionID string
	repo      string // "" means "leave it to the insert path" -> the unqualified sentinel
	costMicro int64
}

// repairSeedSeq makes every seeded row's idempotency key globally unique.
//
// ⚠️ A per-call index is NOT enough and the first draft of these tests was wrong
// because of it: a second seedRepairRows call in the same test restarted at 0,
// its key collided with the first batch's, and InsertTokenEvent's UPSERT absorbed
// it — so the "new" row was silently the OLD row, and a control arm asserted
// against a row it never created.
var repairSeedSeq atomic.Int64

// seedRepairRows inserts the given rows and returns their ids in seed order, so
// a test can assert on EXACT rows rather than on aggregate counts alone. Each row
// gets a globally distinct idempotency key so nothing collapses on the upsert.
func seedRepairRows(t *testing.T, db *DB, seeds []repairSeed) []int64 {
	t.Helper()
	ctx := context.Background()
	ids := make([]int64, 0, len(seeds))
	for i, s := range seeds {
		ev := TokenEvent{
			Developer:      s.developer,
			IssueID:        "issue-493",
			Model:          "claude-sonnet-4",
			InputTok:       1000,
			CostMicro:      s.costMicro,
			Source:         "jsonl",
			Fidelity:       "realtime",
			Repo:           s.repo,
			SessionID:      s.sessionID,
			IdempotencyKey: "repair-seed-" + itoa(int(repairSeedSeq.Add(1))),
			Timestamp:      time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
		}
		if err := db.InsertTokenEvent(ctx, ev); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
		var id int64
		if err := db.db.QueryRowContext(ctx,
			`SELECT id FROM token_events WHERE idempotency_key = ?`, ev.IdempotencyKey,
		).Scan(&id); err != nil {
			t.Fatalf("seed row %d: read back id: %v", i, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// (itoa lives in repo_qualify_test.go — same package, reused here rather than
// duplicated.)

// repoOf reads one row's stored repo — the ground truth every assertion here is
// made against, read straight from the column rather than through an aggregate.
func repoOf(t *testing.T, db *DB, id int64) string {
	t.Helper()
	var repo string
	if err := db.db.QueryRowContext(context.Background(),
		`SELECT repo FROM token_events WHERE id = ?`, id).Scan(&repo); err != nil {
		t.Fatalf("read repo for row %d: %v", id, err)
	}
	return repo
}

// snapshotRepos reads every (id, repo) pair in the table, for the row-for-row
// "nothing changed" assertions the dry-run control arm needs.
func snapshotRepos(t *testing.T, db *DB) map[int64]string {
	t.Helper()
	rows, err := db.db.QueryContext(context.Background(), `SELECT id, repo FROM token_events ORDER BY id`)
	if err != nil {
		t.Fatalf("snapshot repos: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var repo string
		if err := rows.Scan(&id, &repo); err != nil {
			t.Fatalf("snapshot scan: %v", err)
		}
		out[id] = repo
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot iterate: %v", err)
	}
	return out
}

// countRows is a small helper for the audit-ledger assertions.
func countRows(t *testing.T, db *DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// ---------------------------------------------------------------------------
// Acceptance bullet 1: an all-unqualified DB repairs to real slugs, with an
// audit row per change.
// ---------------------------------------------------------------------------

// TestRepairRepoCommitRepairsAllUnqualifiedRows proves the primary path: a DB
// whose rows are ALL on the sentinel is repaired to the mapped slugs, and both
// audit ledgers gain the right rows.
func TestRepairRepoCommitRepairsAllUnqualifiedRows(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	ids := seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "sess-a", costMicro: 100},
		{developer: "alice", sessionID: "sess-a", costMicro: 200},
		{developer: "alice", sessionID: "sess-b", costMicro: 300},
	})
	// Control: the fixture really is 100% unqualified before the repair, so the
	// test cannot pass by asserting a state that was already true.
	for i, id := range ids {
		if got := repoOf(t, db, id); got != repoid.Unqualified {
			t.Fatalf("fixture row %d (id %d) repo = %q, want the %q sentinel", i, id, got, repoid.Unqualified)
		}
	}

	res, err := db.RepairRepo(ctx, RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app", "sess-b": "acme/lib"},
		Commit:        true,
		ToolVersion:   "tierd-test",
	})
	if err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}
	if !res.Committed || res.ChangedRowCount != 3 {
		t.Fatalf("Committed=%v ChangedRowCount=%d, want true/3", res.Committed, res.ChangedRowCount)
	}
	if res.ChangedCostMicroSum != 600 {
		t.Errorf("ChangedCostMicroSum = %d, want 600", res.ChangedCostMicroSum)
	}
	if res.RepairID == "" {
		t.Error("RepairID is empty on a committed run that changed rows")
	}

	want := []string{"acme/app", "acme/app", "acme/lib"}
	for i, id := range ids {
		if got := repoOf(t, db, id); got != want[i] {
			t.Errorf("row %d (id %d) repo = %q, want %q", i, id, got, want[i])
		}
	}

	// One before-image PER CHANGED ROW, and one aggregate row per target repo.
	if n := countRows(t, db, `SELECT COUNT(*) FROM repo_repair_row_audit WHERE repair_id = ?`, res.RepairID); n != 3 {
		t.Errorf("repo_repair_row_audit rows = %d, want 3 (one per changed row)", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM repo_repair_audit WHERE repair_id = ?`, res.RepairID); n != 2 {
		t.Errorf("repo_repair_audit rows = %d, want 2 (one per target repo)", n)
	}
	// The per-repo aggregate must carry the spend that actually moved, not a count.
	var rowCount int64
	var costSum int64
	if err := db.db.QueryRowContext(ctx,
		`SELECT row_count, cost_micro_sum FROM repo_repair_audit WHERE repair_id = ? AND to_repo = ?`,
		res.RepairID, "acme/app").Scan(&rowCount, &costSum); err != nil {
		t.Fatalf("read aggregate audit: %v", err)
	}
	if rowCount != 2 || costSum != 300 {
		t.Errorf("acme/app aggregate = %d rows / %d micro, want 2 / 300", rowCount, costSum)
	}
}

// TestRepairRepoIsTheOnlyPathBecauseAReshipCannotFixIt pins the PREMISE of #493
// rather than its implementation: it reproduces the operator's actual experience
// after upgrading to the #491 shipper.
//
// A row lands unqualified. The operator upgrades and re-ships the SAME message —
// same idempotency_key, now correctly carrying a real repo. The UPSERT absorbs it,
// takes MAX on the token counts, and leaves `repo` exactly as it was, because
// insertTokenEventSQL deliberately excludes `repo` from its conflict clause so a
// repo-blind producer can never downgrade a qualified row. Nothing changes, and
// the server still reports success.
//
// If this test ever starts failing because the re-ship DID repair the row, the
// conflict clause has been widened to include `repo` — which would reintroduce
// the downgrade bug that exclusion exists to prevent, and would make this whole
// command unnecessary. Either way it must be a deliberate decision, not a silent
// drift, so it is pinned here.
func TestRepairRepoIsTheOnlyPathBecauseAReshipCannotFixIt(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	const key = "reship-same-message"
	base := TokenEvent{
		Developer: "alice", IssueID: "issue-493", Model: "claude-sonnet-4",
		InputTok: 1000, CostMicro: 100, Source: "jsonl", Fidelity: "realtime",
		SessionID: "sess-a", IdempotencyKey: key,
		Timestamp: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
	}
	// 1. The pre-#491 shipper's write: repo-blind.
	if err := db.InsertTokenEvent(ctx, base); err != nil {
		t.Fatalf("initial insert: %v", err)
	}
	var id int64
	if err := db.db.QueryRowContext(ctx,
		`SELECT id FROM token_events WHERE idempotency_key = ?`, key).Scan(&id); err != nil {
		t.Fatalf("read id: %v", err)
	}
	if got := repoOf(t, db, id); got != repoid.Unqualified {
		t.Fatalf("fixture repo = %q, want %q", got, repoid.Unqualified)
	}

	// 2. The upgraded shipper re-ships the identical message, now WITH the repo.
	fixed := base
	fixed.Repo = "acme/app"
	if err := db.InsertTokenEvent(ctx, fixed); err != nil {
		t.Fatalf("re-ship: %v", err)
	}
	// 3. NOTHING CHANGED. This is the bug #493 exists to repair.
	if got := repoOf(t, db, id); got != repoid.Unqualified {
		t.Fatalf("re-ship repaired the row (repo = %q). The ON CONFLICT clause now writes `repo`, which reintroduces the downgrade bug that exclusion prevents — if that was deliberate, #493's premise is void and this command should be reconsidered", got)
	}
	// ...and it really was the same row, not a second one silently inserted.
	if n := countRows(t, db, `SELECT COUNT(*) FROM token_events WHERE idempotency_key = ?`, key); n != 1 {
		t.Fatalf("re-ship produced %d rows for one idempotency key, want 1", n)
	}

	// 4. RepairRepo IS the path that fixes it.
	if _, err := db.RepairRepo(ctx, RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app"},
		Commit:        true,
	}); err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}
	if got := repoOf(t, db, id); got != "acme/app" {
		t.Errorf("after repair repo = %q, want acme/app", got)
	}
}

// TestRepairRepoAuditHoldsCorrectBeforeImages proves the per-row ledger records
// the row's EXACT pre-repair repo, the slug it moved to, and the session that
// resolved it — the substrate a row-grain inverse-undo restores from. A ledger
// that does not match what was overwritten is worse than no ledger.
func TestRepairRepoAuditHoldsCorrectBeforeImages(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	ids := seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "sess-a", costMicro: 100},
	})
	res, err := db.RepairRepo(ctx, RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app"},
		Commit:        true,
	})
	if err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}

	var eventID int64
	var oldRepo, newRepo string
	if err := db.db.QueryRowContext(ctx,
		`SELECT token_event_id, old_repo, new_repo FROM repo_repair_row_audit WHERE repair_id = ?`,
		res.RepairID).Scan(&eventID, &oldRepo, &newRepo); err != nil {
		t.Fatalf("read before-image: %v", err)
	}
	if eventID != ids[0] {
		t.Errorf("before-image token_event_id = %d, want %d", eventID, ids[0])
	}
	if oldRepo != repoid.Unqualified {
		t.Errorf("before-image old_repo = %q, want %q — a before-image that does not match what was overwritten would restore a value that was never there", oldRepo, repoid.Unqualified)
	}
	if newRepo != "acme/app" {
		t.Errorf("before-image new_repo = %q, want acme/app", newRepo)
	}
}

// TestRepairRepoRowAuditStoresNoPersonalData pins the PRIVACY decision behind the
// row ledger's column list. This table is designed to OUTLIVE the row it
// describes, and it has no developer column, so anything personal recorded here
// would survive a GDPR Art. 17 erasure with no key for EraseDeveloper to reach it
// by. session_id in particular is classified as personal data in docs/privacy.md
// and was deliberately left out.
//
// GUARD COVERAGE: add a session_id (or developer) column back to
// repo_repair_row_audit and this test fails, forcing the compliance question to
// be answered again rather than re-opened by accident.
func TestRepairRepoRowAuditStoresNoPersonalData(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	rows, err := db.db.QueryContext(context.Background(), `PRAGMA table_info(repo_repair_row_audit)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		got[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}
	want := map[string]bool{
		"id": true, "repair_id": true, "token_event_id": true,
		"old_repo": true, "new_repo": true, "ts": true,
	}
	for name := range got {
		if !want[name] {
			t.Errorf("repo_repair_row_audit gained column %q. This ledger outlives the rows it describes and has no developer column, so anything identifying here is erasure-proof personal data — answer the compliance question before adding it (see the table's schema comment and developerPIITables)", name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("repo_repair_row_audit is missing expected column %q", name)
		}
	}
}

// TestRepairRepoCanonicalizesSuppliedSlug proves the repair writes the SAME byte
// string the collector would have written: a hand-typed "Acme/App.git" is
// canonicalized through repoid, so the repaired row still joins its outcomes
// (#231). Without this the repair would invent an identity nothing else in the
// pipeline produces.
func TestRepairRepoCanonicalizesSuppliedSlug(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	ids := seedRepairRows(t, db, []repairSeed{{developer: "alice", sessionID: "sess-a", costMicro: 1}})
	if _, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "Acme/App.git"},
		Commit:        true,
	}); err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}
	if got := repoOf(t, db, ids[0]); got != "acme/app" {
		t.Errorf("repo = %q, want the canonicalized %q", got, "acme/app")
	}
}

// ---------------------------------------------------------------------------
// Acceptance bullet 2: re-running is a no-op.
// ---------------------------------------------------------------------------

// TestRepairRepoRerunIsANoOp proves the second identical commit changes nothing
// and writes NO new audit rows — the acceptance bullet, and the thing that makes
// this command safe to put in a runbook.
func TestRepairRepoRerunIsANoOp(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "sess-a", costMicro: 100},
		{developer: "alice", sessionID: "sess-b", costMicro: 200},
	})
	opts := RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app", "sess-b": "acme/lib"},
		Commit:        true,
	}
	first, err := db.RepairRepo(ctx, opts)
	if err != nil {
		t.Fatalf("first repair: %v", err)
	}
	if first.ChangedRowCount != 2 {
		t.Fatalf("first run changed %d rows, want 2", first.ChangedRowCount)
	}
	after := snapshotRepos(t, db)
	auditRows := countRows(t, db, `SELECT COUNT(*) FROM repo_repair_audit`)
	rowAuditRows := countRows(t, db, `SELECT COUNT(*) FROM repo_repair_row_audit`)

	second, err := db.RepairRepo(ctx, opts)
	if err != nil {
		t.Fatalf("second repair: %v", err)
	}
	if second.ChangedRowCount != 0 {
		t.Errorf("second run changed %d rows, want 0 (re-running must be a no-op)", second.ChangedRowCount)
	}
	if second.RepairID != "" {
		t.Errorf("second run minted repair_id %q; a run that wrote no audit rows must not report one", second.RepairID)
	}
	if !second.Committed {
		t.Error("second run Committed = false; the --commit request was honored even though nothing changed")
	}
	// Row-for-row: nothing moved.
	for id, repo := range after {
		if got := repoOf(t, db, id); got != repo {
			t.Errorf("row %d repo changed on re-run: %q -> %q", id, repo, got)
		}
	}
	// And the ledgers did not grow.
	if n := countRows(t, db, `SELECT COUNT(*) FROM repo_repair_audit`); n != auditRows {
		t.Errorf("repo_repair_audit grew on a no-op re-run: %d -> %d", auditRows, n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM repo_repair_row_audit`); n != rowAuditRows {
		t.Errorf("repo_repair_row_audit grew on a no-op re-run: %d -> %d", rowAuditRows, n)
	}
}

// ---------------------------------------------------------------------------
// Acceptance bullet 3: an already-real repo is NEVER modified, even when the
// mapping disagrees. This is the invariant the insert-path conflict clause
// protects and the most important control arm in the set.
// ---------------------------------------------------------------------------

// TestRepairRepoRealRepoNeverModifiedEvenWhenMappingDisagrees is the control arm
// for the whole feature: a row that already names a real repository must survive
// a mapping that says otherwise, and the disagreement must be REPORTED rather
// than silently swallowed.
func TestRepairRepoRealRepoNeverModifiedEvenWhenMappingDisagrees(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	ids := seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "sess-real", repo: "acme/already-right", costMicro: 100},
		{developer: "alice", sessionID: "sess-blank", costMicro: 200},
	})

	res, err := db.RepairRepo(ctx, RepairRepoOptions{
		Developer: "alice",
		// The mapping disagrees with the stored, already-correct row.
		SlugBySession: map[string]string{"sess-real": "evil/hijack", "sess-blank": "acme/lib"},
		Commit:        true,
	})
	if err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}

	// The qualified row is untouched...
	if got := repoOf(t, db, ids[0]); got != "acme/already-right" {
		t.Errorf("already-qualified row repo = %q, want acme/already-right — a real repo must never be overwritten", got)
	}
	// ...and it is not in the ledger at all, because it never changed.
	if n := countRows(t, db, `SELECT COUNT(*) FROM repo_repair_row_audit WHERE token_event_id = ?`, ids[0]); n != 0 {
		t.Errorf("already-qualified row has %d before-image(s); an unchanged row must never enter the ledger", n)
	}
	// The unqualified row in the same run still gets repaired — proving the
	// refusal is targeted at the qualified row, not a blanket abort.
	if got := repoOf(t, db, ids[1]); got != "acme/lib" {
		t.Errorf("unqualified row repo = %q, want acme/lib", got)
	}
	if res.ChangedRowCount != 1 {
		t.Errorf("ChangedRowCount = %d, want 1", res.ChangedRowCount)
	}

	// REPORTED, not silently skipped.
	if res.ConflictRowCount != 1 {
		t.Fatalf("ConflictRowCount = %d, want 1", res.ConflictRowCount)
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("len(Conflicts) = %d, want 1", len(res.Conflicts))
	}
	c := res.Conflicts[0]
	if c.SessionID != "sess-real" || c.StoredRepo != "acme/already-right" || c.MappedRepo != "evil/hijack" || c.RowCount != 1 {
		t.Errorf("conflict = %+v, want session sess-real, stored acme/already-right, mapped evil/hijack, 1 row", c)
	}
}

// TestRepairRepoAgreeingMappingIsNotAConflict is the control arm for the conflict
// REPORT: a mapping that agrees with an already-real row must not be reported as
// a disagreement. Without this, the conflict counter could be a constant "every
// qualified row" and the test above would still pass.
func TestRepairRepoAgreeingMappingIsNotAConflict(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "sess-real", repo: "acme/app", costMicro: 100},
	})
	res, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-real": "acme/app"},
		Commit:        true,
	})
	if err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}
	if res.ConflictRowCount != 0 || len(res.Conflicts) != 0 {
		t.Errorf("ConflictRowCount = %d / %d conflicts, want 0 — a mapping that AGREES with the stored repo is not a disagreement", res.ConflictRowCount, len(res.Conflicts))
	}
	if res.AlreadyQualifiedRowCount != 1 {
		t.Errorf("AlreadyQualifiedRowCount = %d, want 1", res.AlreadyQualifiedRowCount)
	}
}

// TestRepairRepoUpdateSQLRefusesRealRepo drives the UPDATE statement DIRECTLY,
// bypassing the Go-side classification, to prove the `AND repo = ?`
// compare-and-swap guard in repairRepoUpdateSQL is load-bearing on its own.
//
// GUARD COVERAGE: delete `AND repo = ?` from repairRepoUpdateSQL and this test
// fails — RowsAffected becomes 1 and the qualified row is overwritten. Without
// this test that clause would be an untestable claim.
func TestRepairRepoUpdateSQLRefusesRealRepo(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	ids := seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "sess-real", repo: "acme/already-right", costMicro: 1},
	})

	// Ask the statement to do exactly what a classification bug would ask of it:
	// rewrite a row whose stored repo is NOT the sentinel we claim it holds.
	out, err := db.db.ExecContext(ctx, repairRepoUpdateSQL, "evil/hijack", ids[0], repoid.Unqualified)
	if err != nil {
		t.Fatalf("exec guarded update: %v", err)
	}
	n, err := out.RowsAffected()
	if err != nil {
		t.Fatalf("rows affected: %v", err)
	}
	if n != 0 {
		t.Errorf("RowsAffected = %d, want 0 — the compare-and-swap guard must block a write against a row that no longer holds the before-image value", n)
	}
	if got := repoOf(t, db, ids[0]); got != "acme/already-right" {
		t.Errorf("repo = %q, want acme/already-right (the guarded UPDATE must not have fired)", got)
	}

	// Control arm: the SAME statement DOES fire when the expected before-image
	// matches — proving the zero above came from the guard, not from a broken
	// statement or a wrong row id.
	unqualified := seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "sess-blank", costMicro: 1},
	})
	out, err = db.db.ExecContext(ctx, repairRepoUpdateSQL, "acme/app", unqualified[0], repoid.Unqualified)
	if err != nil {
		t.Fatalf("exec guarded update (control): %v", err)
	}
	if n, err = out.RowsAffected(); err != nil || n != 1 {
		t.Errorf("control RowsAffected = %d (err %v), want 1 — the statement itself must work when the before-image matches", n, err)
	}
}

// ---------------------------------------------------------------------------
// Acceptance bullet 4: unresolvable rows stay unqualified AND are reported.
// ---------------------------------------------------------------------------

// TestRepairRepoUnresolvedRowsStayUnqualifiedAndAreReported proves both halves:
// a row whose session is absent from the mapping, and a row with NO session at
// all (the proxy/poller shape), are left on the sentinel and named in the result.
func TestRepairRepoUnresolvedRowsStayUnqualifiedAndAreReported(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	ids := seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "sess-mapped", costMicro: 100},
		{developer: "alice", sessionID: "sess-unmapped", costMicro: 200},
		{developer: "alice", sessionID: "", costMicro: 300}, // proxy-shaped: no session at all
	})

	res, err := db.RepairRepo(ctx, RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-mapped": "acme/app"},
		Commit:        true,
	})
	if err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}

	if got := repoOf(t, db, ids[0]); got != "acme/app" {
		t.Errorf("mapped row repo = %q, want acme/app", got)
	}
	// LEFT unqualified — never guessed.
	if got := repoOf(t, db, ids[1]); got != repoid.Unqualified {
		t.Errorf("unmapped-session row repo = %q, want it left on %q — an unresolvable row must never be guessed", got, repoid.Unqualified)
	}
	if got := repoOf(t, db, ids[2]); got != repoid.Unqualified {
		t.Errorf("session-less row repo = %q, want it left on %q", got, repoid.Unqualified)
	}

	// REPORTED.
	if res.UnresolvedRowCount != 2 {
		t.Fatalf("UnresolvedRowCount = %d, want 2", res.UnresolvedRowCount)
	}
	if res.UnresolvedNoSessionRowCount != 1 {
		t.Errorf("UnresolvedNoSessionRowCount = %d, want 1 — the session-less rows must be distinguishable from a forgotten mapping entry", res.UnresolvedNoSessionRowCount)
	}
	byID := map[string]RepairRepoUnresolved{}
	for _, u := range res.Unresolved {
		byID[u.SessionID] = u
	}
	if u, ok := byID["sess-unmapped"]; !ok || u.RowCount != 1 || u.CostMicroSum != 200 {
		t.Errorf("unresolved bucket for sess-unmapped = %+v (present=%v), want 1 row / 200 micro", u, ok)
	}
	if u, ok := byID[""]; !ok || u.RowCount != 1 || u.CostMicroSum != 300 {
		t.Errorf("unresolved no-session bucket = %+v (present=%v), want 1 row / 300 micro", u, ok)
	}
	// Invariant the result type promises.
	if res.UnqualifiedRowCount != res.ChangedRowCount+res.UnresolvedRowCount {
		t.Errorf("UnqualifiedRowCount (%d) != Changed (%d) + Unresolved (%d)", res.UnqualifiedRowCount, res.ChangedRowCount, res.UnresolvedRowCount)
	}
}

// ---------------------------------------------------------------------------
// Dry run writes NOTHING.
// ---------------------------------------------------------------------------

// TestRepairRepoDryRunChangesNothing proves the default mode is inert: it reports
// exactly what a commit would do and mutates no row and no ledger. Asserted
// ROW-FOR-ROW against a pre-run snapshot, not by a count.
func TestRepairRepoDryRunChangesNothing(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "sess-a", costMicro: 100},
		{developer: "alice", sessionID: "sess-b", costMicro: 200},
		{developer: "alice", sessionID: "sess-real", repo: "acme/keep", costMicro: 300},
	})
	before := snapshotRepos(t, db)

	opts := RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app", "sess-b": "acme/lib"},
	}
	dry, err := db.RepairRepo(ctx, opts)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Committed {
		t.Error("dry run reported Committed = true")
	}
	if dry.RepairID != "" {
		t.Errorf("dry run minted repair_id %q; it must write no audit and report none", dry.RepairID)
	}
	if dry.ChangedRowCount != 2 {
		t.Errorf("dry run ChangedRowCount = %d, want 2 (it must report what a commit WOULD do)", dry.ChangedRowCount)
	}

	// Row-for-row: nothing moved.
	after := snapshotRepos(t, db)
	if len(before) != len(after) {
		t.Fatalf("row count changed during a dry run: %d -> %d", len(before), len(after))
	}
	for id, repo := range before {
		if after[id] != repo {
			t.Errorf("dry run mutated row %d: %q -> %q", id, repo, after[id])
		}
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM repo_repair_audit`); n != 0 {
		t.Errorf("dry run wrote %d repo_repair_audit row(s), want 0", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM repo_repair_row_audit`); n != 0 {
		t.Errorf("dry run wrote %d repo_repair_row_audit row(s), want 0", n)
	}

	// Control arm: the SAME options with Commit DO change those exact rows, so the
	// "nothing changed" above is the dry run's doing and not an inert fixture.
	opts.Commit = true
	if _, err := db.RepairRepo(ctx, opts); err != nil {
		t.Fatalf("control commit: %v", err)
	}
	changed := 0
	for id, repo := range snapshotRepos(t, db) {
		if repo != before[id] {
			changed++
		}
	}
	if changed != 2 {
		t.Errorf("control commit changed %d rows, want 2 — the dry-run fixture must be genuinely repairable", changed)
	}
}

// ---------------------------------------------------------------------------
// Scope guards.
// ---------------------------------------------------------------------------

// TestRepairRepoDeveloperScopeIsEnforced proves the required --developer selector
// actually bounds the mutation: another developer's rows are invisible to the
// repair even when they share a mapped session id. This is the guard that stops
// one laptop's mapping re-attributing someone else's spend.
//
// GUARD COVERAGE: drop the `WHERE developer = ?` filter from
// repairRepoSelectSQL and this test fails — bob's row gets repaired too.
func TestRepairRepoDeveloperScopeIsEnforced(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	ids := seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "sess-shared", costMicro: 100},
		{developer: "bob", sessionID: "sess-shared", costMicro: 200},
	})
	res, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-shared": "acme/app"},
		Commit:        true,
	})
	if err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}
	if res.ScannedRowCount != 1 || res.ChangedRowCount != 1 {
		t.Errorf("Scanned=%d Changed=%d, want 1/1 — the repair must only see the named developer's rows", res.ScannedRowCount, res.ChangedRowCount)
	}
	if got := repoOf(t, db, ids[0]); got != "acme/app" {
		t.Errorf("alice's row repo = %q, want acme/app", got)
	}
	if got := repoOf(t, db, ids[1]); got != repoid.Unqualified {
		t.Errorf("bob's row repo = %q, want it untouched on %q — a repair scoped to alice must never reach bob", got, repoid.Unqualified)
	}
}

// TestRepairRepoRequiresDeveloper proves the required-selector guard: an empty
// developer is refused before anything is read or written.
func TestRepairRepoRequiresDeveloper(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	seedRepairRows(t, db, []repairSeed{{developer: "alice", sessionID: "sess-a", costMicro: 1}})
	before := snapshotRepos(t, db)

	_, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		SlugBySession: map[string]string{"sess-a": "acme/app"},
		Commit:        true,
	})
	if err == nil {
		t.Fatal("RepairRepo with an empty Developer succeeded; it must refuse to repair every developer's rows at once")
	}
	if !strings.Contains(err.Error(), "developer is required") {
		t.Errorf("error = %v, want it to name the missing developer", err)
	}
	for id, repo := range snapshotRepos(t, db) {
		if repo != before[id] {
			t.Errorf("a rejected repair mutated row %d: %q -> %q", id, before[id], repo)
		}
	}
}

// TestRepairRepoRequiresNonEmptyMapping proves an empty mapping is refused rather
// than reported as a clean zero-change run. Without this guard a forgotten --map
// prints "0 rows repaired" — indistinguishable from "your history is fine".
func TestRepairRepoRequiresNonEmptyMapping(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	seedRepairRows(t, db, []repairSeed{{developer: "alice", sessionID: "sess-a", costMicro: 1}})
	_, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		Developer: "alice",
		Commit:    true,
	})
	if err == nil {
		t.Fatal("RepairRepo with an empty mapping succeeded; a run that can resolve nothing must not look like a successful repair")
	}
	if !strings.Contains(err.Error(), "mapping is empty") {
		t.Errorf("error = %v, want it to name the empty mapping", err)
	}
}

// TestRepairRepoRejectsNonCanonicalSlug proves the store re-validates every
// mapping value at the MUTATION boundary rather than trusting its caller, and
// that nothing is written when it refuses. The sentinel case is the security-ish
// one: a caller must not be able to forge 'unqualified' back into the column, the
// same discipline the ingest API applies to a client-supplied sentinel.
func TestRepairRepoRejectsNonCanonicalSlug(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	seedRepairRows(t, db, []repairSeed{{developer: "alice", sessionID: "sess-a", costMicro: 1}})
	before := snapshotRepos(t, db)

	for name, slug := range map[string]string{
		"single segment":  "app",
		"sentinel forged": repoid.Unqualified,
		"empty":           "",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := db.RepairRepo(context.Background(), RepairRepoOptions{
				Developer:     "alice",
				SlugBySession: map[string]string{"sess-a": slug},
				Commit:        true,
			})
			if err == nil {
				t.Fatalf("RepairRepo accepted %q as a repository slug", slug)
			}
			if !strings.Contains(err.Error(), "canonical owner/repo slug") {
				t.Errorf("error = %v, want it to name the canonicalization failure", err)
			}
			for id, repo := range snapshotRepos(t, db) {
				if repo != before[id] {
					t.Errorf("a rejected repair mutated row %d: %q -> %q", id, before[id], repo)
				}
			}
		})
	}

	// Control arm: a VALID slug through the very same path DOES commit, proving
	// the refusals above came from canonicalization and not from a broken fixture.
	if _, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app"},
		Commit:        true,
	}); err != nil {
		t.Fatalf("control: a valid slug must be accepted: %v", err)
	}
}

// TestRepairRepoRejectsEmptySessionKey proves a mapping keyed by "" is refused.
// It could never match a row (a NULL session reads back as "" and is
// unresolvable by definition), so accepting it would silently do nothing.
func TestRepairRepoRejectsEmptySessionKey(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	seedRepairRows(t, db, []repairSeed{{developer: "alice", sessionID: "", costMicro: 1}})
	_, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"": "acme/app"},
		Commit:        true,
	})
	if err == nil {
		t.Fatal("RepairRepo accepted an empty session key; a session-less row must stay unresolvable")
	}
	if !strings.Contains(err.Error(), "empty session id") {
		t.Errorf("error = %v, want it to name the empty session id", err)
	}
}

// TestRepairRepoUnknownDeveloperScansNothing proves a typo'd developer is
// reported as "examined nothing" rather than as a successful no-op — the signal
// the CLI turns into a "check --developer" message.
func TestRepairRepoUnknownDeveloperScansNothing(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	seedRepairRows(t, db, []repairSeed{{developer: "alice", sessionID: "sess-a", costMicro: 1}})
	res, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		Developer:     "alicia", // typo
		SlugBySession: map[string]string{"sess-a": "acme/app"},
		Commit:        true,
	})
	if err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}
	if res.ScannedRowCount != 0 || res.ChangedRowCount != 0 {
		t.Errorf("Scanned=%d Changed=%d, want 0/0 for an unknown developer", res.ScannedRowCount, res.ChangedRowCount)
	}
}

// ---------------------------------------------------------------------------
// Blast radius: what the repair must NOT touch.
// ---------------------------------------------------------------------------

// rowSnapshot reads EVERY column of one token_events row into a map, so a test
// can assert on the whole row rather than on the one column it remembered to
// check.
func rowSnapshot(t *testing.T, db *DB, id int64) map[string]any {
	t.Helper()
	rows, err := db.db.QueryContext(context.Background(), `SELECT * FROM token_events WHERE id = ?`, id)
	if err != nil {
		t.Fatalf("select row %d: %v", id, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("row %d not found", id)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatalf("scan row %d: %v", id, err)
	}
	out := make(map[string]any, len(cols))
	for i, c := range cols {
		out[c] = fmt.Sprintf("%v", vals[i])
	}
	return out
}

// TestRepairRepoTouchesOnlyTheRepoColumn is the control arm for this feature's
// central design claim — that a targeted UPDATE was chosen over a re-INSERT
// precisely so the repair cannot disturb anything else, above all cost_micro,
// which #233 holds IMMUTABLE on the replay path. Until this test existed, that
// claim appeared in the file comment, the commit message, and the issue, and was
// asserted nowhere.
//
// GUARD COVERAGE: add any other column to repairRepoUpdateSQL's SET clause (e.g.
// `, cost_micro = 0`) and this test fails, naming the column.
func TestRepairRepoTouchesOnlyTheRepoColumn(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	ids := seedRepairRows(t, db, []repairSeed{{developer: "alice", sessionID: "sess-a", costMicro: 4242}})
	before := rowSnapshot(t, db, ids[0])

	if _, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app"},
		Commit:        true,
	}); err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}
	after := rowSnapshot(t, db, ids[0])

	if len(before) != len(after) {
		t.Fatalf("column count changed: %d -> %d", len(before), len(after))
	}
	for col, was := range before {
		now := after[col]
		if col == "repo" {
			if was == now {
				t.Fatalf("repo did not change (%v) — the fixture was not repairable, so this test proves nothing", was)
			}
			continue
		}
		if was != now {
			t.Errorf("repair modified column %q: %v -> %v. Only `repo` may change; rewriting anything else (cost_micro above all) is the exact side effect a re-INSERT would have had and this design exists to avoid", col, was, now)
		}
	}
}

// TestRepairRepoDoesNotTouchOutcomes enforces the documented SCOPE boundary.
// `outcomes` also carries a `repo` column that can hold the same sentinel, and
// the command deliberately does not repair it — the outcome producer is the
// GitHub webhook, which always knows the repository, so an unqualified outcome
// means something different and needs a different diagnosis.
//
// GUARD COVERAGE: add any `UPDATE outcomes` to the repair transaction and this
// test fails.
func TestRepairRepoDoesNotTouchOutcomes(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	seedRepairRows(t, db, []repairSeed{{developer: "alice", sessionID: "sess-a", costMicro: 100}})
	if _, err := db.db.ExecContext(ctx,
		`INSERT INTO outcomes (developer, issue_id, weight, quality, repo, ts)
		 VALUES ('alice', 'issue-493', 1.0, 1.0, ?, '2026-07-11 12:00:00+00:00')`,
		repoid.Unqualified); err != nil {
		t.Fatalf("seed outcome: %v", err)
	}

	if _, err := db.RepairRepo(ctx, RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app"},
		Commit:        true,
	}); err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}

	var outcomeRepo string
	if err := db.db.QueryRowContext(ctx, `SELECT repo FROM outcomes WHERE developer = 'alice'`).Scan(&outcomeRepo); err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	if outcomeRepo != repoid.Unqualified {
		t.Errorf("outcomes.repo = %q, want it untouched on %q — repair-repo is scoped to token_events", outcomeRepo, repoid.Unqualified)
	}
	// Control arm: the token_events row in the SAME run WAS repaired, so the
	// untouched outcome is a boundary and not an inert fixture.
	if n := countRows(t, db, `SELECT COUNT(*) FROM token_events WHERE repo = 'acme/app'`); n != 1 {
		t.Errorf("token_events repaired rows = %d, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// The purpose the repair exists for.
// ---------------------------------------------------------------------------

// TestRepairRepoMakesSpendVisibleToARepoScopedRead asserts the OUTCOME rather
// than the column. The issue's actual complaint is that until the history is
// repaired "no per-repository TIER score can be computed from it, only a fleet
// aggregate" — so the strongest available control arm is a repo-scoped read that
// returns nothing before and the real spend after.
func TestRepairRepoMakesSpendVisibleToARepoScopedRead(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "sess-a", costMicro: 5000},
	})
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	before, err := db.DeveloperCostsWindow(ctx, since, until, RepoScope("acme/app"))
	if err != nil {
		t.Fatalf("scoped read before: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("scoped read returned %d row(s) BEFORE the repair, want 0 — the fixture must actually be invisible to a per-repository score", len(before))
	}
	// Control: the spend is not missing, it is merely unattributed — a fleet-wide
	// read sees it the whole time. Without this the test would also pass on an
	// empty database.
	fleet, err := db.DeveloperCostsWindow(ctx, since, until, FleetWide)
	if err != nil {
		t.Fatalf("fleet read: %v", err)
	}
	if len(fleet) != 1 {
		t.Fatalf("fleet-wide read returned %d row(s), want 1", len(fleet))
	}

	if _, err := db.RepairRepo(ctx, RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app"},
		Commit:        true,
	}); err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}

	after, err := db.DeveloperCostsWindow(ctx, since, until, RepoScope("acme/app"))
	if err != nil {
		t.Fatalf("scoped read after: %v", err)
	}
	if len(after) != 1 || after[0].Developer != "alice" || after[0].TotalCostMicro != 5000 {
		t.Errorf("scoped read after repair = %+v, want one alice row of 5000 micro — this is the whole point of the repair", after)
	}
}

// ---------------------------------------------------------------------------
// Derived and aggregate fields.
// ---------------------------------------------------------------------------

// TestRepairRepoAggregateLedgerRecordsRunIdentity covers the aggregate ledger's
// identity columns, which are the durable record of WHO was repaired, FROM what,
// and by WHICH binary. Only row_count and cost_micro_sum were asserted before, so
// developer/from_repo/tool_version could each have been written as a constant.
func TestRepairRepoAggregateLedgerRecordsRunIdentity(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	seedRepairRows(t, db, []repairSeed{{developer: "alice", sessionID: "sess-a", costMicro: 100}})
	res, err := db.RepairRepo(ctx, RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app"},
		Commit:        true,
		ToolVersion:   "tierd-under-test",
	})
	if err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}

	var developer, fromRepo, toRepo, toolVersion string
	if err := db.db.QueryRowContext(ctx,
		`SELECT developer, from_repo, to_repo, tool_version FROM repo_repair_audit WHERE repair_id = ?`,
		res.RepairID).Scan(&developer, &fromRepo, &toRepo, &toolVersion); err != nil {
		t.Fatalf("read aggregate audit: %v", err)
	}
	if developer != "alice" {
		t.Errorf("audit developer = %q, want alice — the ledger must record what the run was ALLOWED to touch", developer)
	}
	if fromRepo != repoid.Unqualified {
		t.Errorf("audit from_repo = %q, want %q (the value the rows actually held)", fromRepo, repoid.Unqualified)
	}
	if toRepo != "acme/app" {
		t.Errorf("audit to_repo = %q, want acme/app", toRepo)
	}
	if toolVersion != "tierd-under-test" {
		t.Errorf("audit tool_version = %q, want the caller's stamp — provenance for which binary rewrote this history", toolVersion)
	}
}

// TestRepairRepoFromRepoRecordsTheObservedValue proves from_repo is READ from the
// row rather than assumed to be the sentinel. The candidate test is
// !repoid.IsReal, which also admits the empty string; normalizeRepo makes ""
// unreachable through the insert path, so this test writes one directly.
//
// GUARD COVERAGE: hardcode repoid.Unqualified back into the audit INSERT and this
// test fails — the ledger would assert a before-value the per-row before-image
// contradicts.
func TestRepairRepoFromRepoRecordsTheObservedValue(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// A corrupt/legacy row holding "" rather than the sentinel. NOT reachable via
	// InsertTokenEvent, which normalizes — hence the raw INSERT.
	if _, err := db.db.ExecContext(ctx,
		`INSERT INTO token_events (developer, issue_id, model, cost_micro, source, fidelity, repo, session_id, ts)
		 VALUES ('alice', 'issue-493', 'claude-sonnet-4', 70, 'jsonl', 'realtime', '', 'sess-empty', '2026-07-11 12:00:00+00:00')`,
	); err != nil {
		t.Fatalf("seed empty-repo row: %v", err)
	}

	res, err := db.RepairRepo(ctx, RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-empty": "acme/app"},
		Commit:        true,
	})
	if err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}
	if len(res.ByRepo) != 1 || res.ByRepo[0].FromRepo != "" {
		t.Fatalf("ByRepo = %+v, want one group whose FromRepo is the observed empty string", res.ByRepo)
	}

	var fromRepo, oldRepo string
	if err := db.db.QueryRowContext(ctx,
		`SELECT from_repo FROM repo_repair_audit WHERE repair_id = ?`, res.RepairID).Scan(&fromRepo); err != nil {
		t.Fatalf("read aggregate: %v", err)
	}
	if err := db.db.QueryRowContext(ctx,
		`SELECT old_repo FROM repo_repair_row_audit WHERE repair_id = ?`, res.RepairID).Scan(&oldRepo); err != nil {
		t.Fatalf("read before-image: %v", err)
	}
	if fromRepo != oldRepo {
		t.Errorf("aggregate from_repo (%q) contradicts the per-row before-image (%q) — an audit that can disagree with itself is not an audit", fromRepo, oldRepo)
	}
	if fromRepo != "" {
		t.Errorf("from_repo = %q, want the observed %q", fromRepo, "")
	}
}

// TestRepairRepoSessionCountCountsDistinctSessions covers the per-repo
// SessionCount, which is documented as the operator's signal for "did this come
// from one runaway session or a broad history" and is printed in the report. No
// other fixture in this file maps two sessions to one repo, which is the only
// shape where the field is non-trivial.
func TestRepairRepoSessionCountCountsDistinctSessions(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "sess-1", costMicro: 10},
		{developer: "alice", sessionID: "sess-2", costMicro: 20},
		{developer: "alice", sessionID: "sess-2", costMicro: 30},
	})
	res, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-1": "acme/app", "sess-2": "acme/app"},
		Commit:        true,
	})
	if err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}
	if len(res.ByRepo) != 1 {
		t.Fatalf("ByRepo = %+v, want a single acme/app group", res.ByRepo)
	}
	got := res.ByRepo[0]
	if got.RowCount != 3 || got.CostMicroSum != 60 || got.SessionCount != 2 {
		t.Errorf("ByRepo[0] = %+v, want RowCount 3 / CostMicroSum 60 / SessionCount 2 (three rows across TWO distinct sessions)", got)
	}
}

// TestRepairRepoResultSlicesAreSorted pins the three documented orderings. Go
// randomizes map iteration, so without the sorts the report AND the order the
// aggregate audit rows are inserted in would differ run to run — and the fixtures
// here are deliberately seeded in reverse order so a missing sort cannot pass by
// luck.
func TestRepairRepoResultSlicesAreSorted(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "s-zeta", costMicro: 1},
		{developer: "alice", sessionID: "s-yank", costMicro: 1},
		{developer: "alice", sessionID: "s-alfa", costMicro: 1},
		{developer: "alice", sessionID: "u-zulu", costMicro: 1},
		{developer: "alice", sessionID: "u-mike", costMicro: 1},
		{developer: "alice", sessionID: "u-echo", costMicro: 1},
		{developer: "alice", sessionID: "", costMicro: 1},
		{developer: "alice", sessionID: "c-two", repo: "acme/two", costMicro: 1},
		{developer: "alice", sessionID: "c-one", repo: "acme/one", costMicro: 1},
	})
	res, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		Developer: "alice",
		SlugBySession: map[string]string{
			"s-zeta": "zz/zulu", "s-yank": "mm/mike", "s-alfa": "aa/alfa",
			"c-two": "other/x", "c-one": "other/x",
		},
	})
	if err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}

	if len(res.ByRepo) != 3 {
		t.Fatalf("ByRepo = %+v, want 3 groups", res.ByRepo)
	}
	for i := 1; i < len(res.ByRepo); i++ {
		if res.ByRepo[i-1].Repo >= res.ByRepo[i].Repo {
			t.Errorf("ByRepo is not ascending by slug: %q then %q", res.ByRepo[i-1].Repo, res.ByRepo[i].Repo)
		}
	}
	if len(res.Unresolved) != 4 {
		t.Fatalf("Unresolved = %+v, want 4 buckets (3 unmapped sessions + the no-session bucket)", res.Unresolved)
	}
	if res.Unresolved[0].SessionID != "" {
		t.Errorf("Unresolved[0].SessionID = %q, want the no-session bucket to sort first", res.Unresolved[0].SessionID)
	}
	for i := 1; i < len(res.Unresolved); i++ {
		if res.Unresolved[i-1].SessionID >= res.Unresolved[i].SessionID {
			t.Errorf("Unresolved is not ascending: %q then %q", res.Unresolved[i-1].SessionID, res.Unresolved[i].SessionID)
		}
	}
	if len(res.Conflicts) != 2 {
		t.Fatalf("Conflicts = %+v, want 2", res.Conflicts)
	}
	if res.Conflicts[0].SessionID >= res.Conflicts[1].SessionID {
		t.Errorf("Conflicts is not ascending by session: %q then %q", res.Conflicts[0].SessionID, res.Conflicts[1].SessionID)
	}
}

// TestRepairRepoConflictsSplitBySessionAndStoredRepo covers the composite
// conflict key. One session whose rows carry TWO different real repos must
// surface as two distinct conflicts; collapsing them would show the operator one
// line naming only whichever repo happened to be scanned first, hiding half the
// disagreement.
//
// GUARD COVERAGE: key the conflict map by session alone and this test fails.
func TestRepairRepoConflictsSplitBySessionAndStoredRepo(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "sess-x", repo: "acme/one", costMicro: 11},
		{developer: "alice", sessionID: "sess-x", repo: "acme/two", costMicro: 22},
	})
	res, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-x": "acme/three"},
	})
	if err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}
	if len(res.Conflicts) != 2 {
		t.Fatalf("Conflicts = %+v, want 2 (one session, two different stored repos)", res.Conflicts)
	}
	if res.Conflicts[0].StoredRepo != "acme/one" || res.Conflicts[0].CostMicroSum != 11 {
		t.Errorf("Conflicts[0] = %+v, want acme/one / 11 micro", res.Conflicts[0])
	}
	if res.Conflicts[1].StoredRepo != "acme/two" || res.Conflicts[1].CostMicroSum != 22 {
		t.Errorf("Conflicts[1] = %+v, want acme/two / 22 micro", res.Conflicts[1])
	}
}

// ---------------------------------------------------------------------------
// Failure atomicity — fault injection.
// ---------------------------------------------------------------------------

// TestRepairRepoAbortsWhenTheGuardedUpdateMatchesNothing reaches the fail-loud
// `RowsAffected != 1` branch, which is otherwise unreachable through the public
// API (one transaction, one writer). A BEFORE UPDATE trigger raising IGNORE
// abandons the row operation WITHOUT an error, which is exactly the shape the
// branch exists for: the write silently did not happen. Committing a ledger that
// describes a change that did not happen is the one outcome worse than failing.
//
// GUARD COVERAGE: replace the `n != 1` check with `_ = n` and this test fails —
// the run would report success while the rows stayed unqualified.
func TestRepairRepoAbortsWhenTheGuardedUpdateMatchesNothing(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	ids := seedRepairRows(t, db, []repairSeed{{developer: "alice", sessionID: "sess-a", costMicro: 100}})
	if _, err := db.db.ExecContext(ctx,
		`CREATE TRIGGER swallow_repair_update BEFORE UPDATE OF repo ON token_events
		 BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatalf("install fault-injection trigger: %v", err)
	}

	_, err := db.RepairRepo(ctx, RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app"},
		Commit:        true,
	})
	if err == nil {
		t.Fatal("RepairRepo reported success while its UPDATE changed nothing — it must fail loud rather than write a ledger describing a change that did not happen")
	}
	if !strings.Contains(err.Error(), "nothing has been committed") {
		t.Errorf("error = %v, want it to state that nothing was committed", err)
	}
	if got := repoOf(t, db, ids[0]); got != repoid.Unqualified {
		t.Errorf("row repo = %q, want it unchanged", got)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM repo_repair_row_audit`); n != 0 {
		t.Errorf("aborted run left %d orphan before-image(s), want 0", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM repo_repair_audit`); n != 0 {
		t.Errorf("aborted run left %d aggregate audit row(s), want 0", n)
	}
}

// TestRepairRepoRollsBackEverythingOnAMidCommitFailure proves the all-or-nothing
// claim the file comment, the schema comments, and the CLI help all make. The
// failure is injected at the LAST write of the transaction — the aggregate audit
// insert — which happens after every row UPDATE and every before-image has
// already been issued. If the transaction were not atomic, this is precisely the
// shape that would leave a half-repaired table.
func TestRepairRepoRollsBackEverythingOnAMidCommitFailure(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	ids := seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "sess-a", costMicro: 100},
		{developer: "alice", sessionID: "sess-b", costMicro: 200},
	})
	opts := RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app", "sess-b": "acme/lib"},
		Commit:        true,
	}

	if _, err := db.db.ExecContext(ctx,
		`CREATE TRIGGER boom_on_audit BEFORE INSERT ON repo_repair_audit
		 BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END`); err != nil {
		t.Fatalf("install fault-injection trigger: %v", err)
	}

	if _, err := db.RepairRepo(ctx, opts); err == nil {
		t.Fatal("RepairRepo succeeded despite a failing audit write")
	}
	for i, id := range ids {
		if got := repoOf(t, db, id); got != repoid.Unqualified {
			t.Errorf("row %d repo = %q after a failed commit, want the transaction rolled it back to %q — a partial repair is worse than none", i, got, repoid.Unqualified)
		}
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM repo_repair_row_audit`); n != 0 {
		t.Errorf("failed commit left %d orphan before-image(s), want 0", n)
	}

	// Control arm: with the fault removed the SAME options commit cleanly, so the
	// rollback above was the transaction's doing and not a rejected input.
	if _, err := db.db.ExecContext(ctx, `DROP TRIGGER boom_on_audit`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if _, err := db.RepairRepo(ctx, opts); err != nil {
		t.Fatalf("control run after removing the fault: %v", err)
	}
	for i, id := range ids {
		if got := repoOf(t, db, id); got == repoid.Unqualified {
			t.Errorf("control: row %d still unqualified — the fixture was never repairable, so the rollback assertion proved nothing", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Degenerate shapes.
// ---------------------------------------------------------------------------

// TestRepairRepoOnAnEmptyDatabase pins the zero-everything shape: no rows at all
// is not an error, and it reports "examined nothing" so the caller can tell it
// apart from a completed repair.
func TestRepairRepoOnAnEmptyDatabase(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	res, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app"},
		Commit:        true,
	})
	if err != nil {
		t.Fatalf("RepairRepo on an empty DB: %v", err)
	}
	if res.ScannedRowCount != 0 || res.ChangedRowCount != 0 || res.RepairID != "" || len(res.ByRepo) != 0 {
		t.Errorf("result = %+v, want all-zero with no repair id", res)
	}
	if !res.Committed {
		t.Error("Committed = false; the --commit request was honored, there was simply nothing to do")
	}
}

// ---------------------------------------------------------------------------
// GDPR (#184) coverage of the new ledger.
// ---------------------------------------------------------------------------

// TestRepairRepoAuditIsErasedAndExported closes the compliance hole this feature
// would otherwise have opened. repo_repair_audit names a developer and the
// repositories their spend was moved into, so it is personal data: an Art. 17
// erasure must delete it and an Art. 15 export must disclose it.
//
// ⚠️ It deliberately does NOT iterate developerPIITables the way gdpr_test.go
// does — that shape is tautological and can never notice a table missing from the
// list, which is exactly how this table would have been missed. It names the
// table literally.
func TestRepairRepoAuditIsErasedAndExported(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	seedRepairRows(t, db, []repairSeed{{developer: "alice", sessionID: "sess-a", costMicro: 100}})
	if _, err := db.RepairRepo(ctx, RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app"},
		Commit:        true,
		ToolVersion:   "tierd-test",
	}); err != nil {
		t.Fatalf("RepairRepo: %v", err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM repo_repair_audit WHERE developer = 'alice'`); n != 1 {
		t.Fatalf("fixture has %d audit row(s), want 1", n)
	}

	// Art. 15: the DSAR must disclose it.
	exp, err := db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("ExportDeveloper: %v", err)
	}
	if len(exp.RepoRepairAudit) != 1 {
		t.Fatalf("export carries %d repo_repair_audit row(s), want 1 — the subject cannot see a retroactive change to their own attribution", len(exp.RepoRepairAudit))
	}
	if got := exp.RepoRepairAudit[0]; got.Developer != "alice" || got.ToRepo != "acme/app" || got.CostMicroSum != 100 {
		t.Errorf("exported audit row = %+v, want alice / acme/app / 100 micro", got)
	}

	// Art. 17: erasure must delete it, and report it by name.
	counts, err := db.EraseDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("EraseDeveloper: %v", err)
	}
	if counts["repo_repair_audit"] != 1 {
		t.Errorf("erase counts[repo_repair_audit] = %d, want 1 — a table missing from developerPIITables is a SILENT compliance hole, and the endpoint would still have reported success", counts["repo_repair_audit"])
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM repo_repair_audit WHERE developer = 'alice'`); n != 0 {
		t.Errorf("%d repo_repair_audit row(s) survived erasure, want 0", n)
	}
	// And the per-row ledger, which holds no personal data, is allowed to survive
	// — it is now a dangling row id and two repository slugs, exactly like
	// reprice_row_audit. Asserted so the reasoning is visible rather than assumed.
	if n := countRows(t, db, `SELECT COUNT(*) FROM repo_repair_row_audit`); n != 1 {
		t.Errorf("repo_repair_row_audit rows = %d, want 1 (retained: it carries no personal data)", n)
	}
}

// TestRepairRepoCommitTakesTheWriteLockBeforeItReads pins the transaction-mode
// fix, and it is the only test here that needs two independent connections.
//
// THE BUG IT PREVENTS: BeginTx opens a DEFERRED transaction, and the commit path
// reads the whole candidate set and only then writes. Under WAL, if any other
// connection commits in between, SQLite fails that upgrade with
// SQLITE_BUSY_SNAPSHOT — which busy_timeout does NOT retry; it returns
// immediately. On the 123k-row history this command exists for, one concurrent
// event insert would discard the entire scan with an opaque "database is locked".
//
// The fix issues a no-op UPDATE first, so a commit contends for the write lock up
// front where failing is cheap, and reports it in terms an operator can act on.
//
// GUARD COVERAGE: delete the lock-acquisition block and this test fails — the
// error arrives later and says "update row N" instead of "acquire write lock".
// Deleting only the `if opts.Commit` gate fails it too, via the dry-run arm.
func TestRepairRepoCommitTakesTheWriteLockBeforeItReads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock.db")

	writer, err := Open(path)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer func() { _ = writer.Close() }()
	seedRepairRows(t, writer, []repairSeed{{developer: "alice", sessionID: "sess-a", costMicro: 100}})

	repairer, err := Open(path)
	if err != nil {
		t.Fatalf("open repairer: %v", err)
	}
	defer func() { _ = repairer.Close() }()

	ctx := context.Background()
	opts := RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app"},
	}

	// Hold the write lock on the OTHER connection, exactly as a live
	// `tierd serve` ingest would.
	conn, err := writer.db.Conn(ctx)
	if err != nil {
		t.Fatalf("grab conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		_ = conn.Close()
		t.Fatalf("begin immediate: %v", err)
	}

	// A DRY RUN must still work: it never writes, so it must never contend.
	if _, err := repairer.RepairRepo(ctx, opts); err != nil {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		_ = conn.Close()
		t.Fatalf("dry run failed while another connection held the write lock: %v — a read-only run must not take a write lock", err)
	}

	// A COMMIT must fail AT LOCK ACQUISITION, not after the scan.
	commitOpts := opts
	commitOpts.Commit = true
	_, err = repairer.RepairRepo(ctx, commitOpts)
	if err == nil {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		_ = conn.Close()
		t.Fatal("commit succeeded while another connection held the write lock")
	}
	if !errors.Is(err, ErrWriteLockUnavailable) {
		t.Errorf("error = %v, want it to wrap ErrWriteLockUnavailable. Arriving later (as \"update row N: database is locked\") means the transaction is still upgrading mid-run, which is the SQLITE_BUSY_SNAPSHOT hazard this guard removes", err)
	}
	// And the operator-facing hint really is attached — it is gated on the
	// sentinel, so a regression that stopped wrapping would silently drop it.
	if !strings.Contains(err.Error(), "quiesced database") {
		t.Errorf("error = %v, want the contention hint telling the operator what to DO about it", err)
	}

	// Release, then prove the SAME commit works — so the failure above was the
	// lock and not a broken fixture.
	if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatalf("rollback holder: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close holder conn: %v", err)
	}
	if _, err := repairer.RepairRepo(ctx, commitOpts); err != nil {
		t.Fatalf("control: commit after releasing the lock: %v", err)
	}
	if got := repoOf(t, repairer, ids0(t, repairer)); got != "acme/app" {
		t.Errorf("repo = %q after the control commit, want acme/app", got)
	}
}

// ids0 returns the only token_events id in a single-row fixture.
func ids0(t *testing.T, db *DB) int64 {
	t.Helper()
	var id int64
	if err := db.db.QueryRowContext(context.Background(), `SELECT id FROM token_events`).Scan(&id); err != nil {
		t.Fatalf("read single row id: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// Review finding: developer ALIASES silently produce a partial repair (#493).
// ---------------------------------------------------------------------------

// TestRepairRepoReportsUnqualifiedRowsUnderAliasIdentities reproduces the exact
// measured failure. token_events.developer stores the RAW producer id — the OS
// username for a JSONL capture, the GitHub login for a webhook — and the two are
// unified only at READ time by developer_alias. So one human's rows sit under two
// names, `WHERE developer = ?` reaches one of them, and the run reports success.
//
// Measured: 7 rows for one person on ONE session, 3 under "devlead" and 4 under
// "dl" with an alias joining them. Repairing "devlead" fixed 3 and left 4 —
// with no UNRESOLVED bucket (the mapping DID resolve the session it saw) and no
// conflict. It was byte-for-byte indistinguishable from a complete repair.
//
// The fix is to REPORT, not to widen: the mapping is derived from one machine's
// session history, so applying it to rows stored under a different identifier is
// the cross-person re-attribution RepairRepoOptions.Developer exists to prevent.
// This test pins both halves — the gap is named, AND the sibling's rows are
// untouched.
func TestRepairRepoReportsUnqualifiedRowsUnderAliasIdentities(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	primary := seedRepairRows(t, db, []repairSeed{
		{developer: "devlead", sessionID: "sess-a", costMicro: 10},
		{developer: "devlead", sessionID: "sess-a", costMicro: 20},
		{developer: "devlead", sessionID: "sess-a", costMicro: 30},
	})
	sibling := seedRepairRows(t, db, []repairSeed{
		{developer: "dl", sessionID: "sess-a", costMicro: 40},
		{developer: "dl", sessionID: "sess-a", costMicro: 50},
		{developer: "dl", sessionID: "sess-a", costMicro: 60},
		{developer: "dl", sessionID: "sess-a", costMicro: 70},
	})
	if err := db.UpsertDeveloperAlias(ctx, "dl", "devlead"); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	res, err := db.RepairRepo(ctx, RepairRepoOptions{
		Developer:     "devlead",
		SlugBySession: map[string]string{"sess-a": "acme/app"},
		Commit:        true,
	})
	if err != nil {
		t.Fatalf("repair: %v", err)
	}

	// The repair itself is unchanged: exactly the named developer's rows.
	if res.ChangedRowCount != 3 {
		t.Errorf("ChangedRowCount = %d, want 3 (only the rows stored under the named identity)", res.ChangedRowCount)
	}
	if res.ScannedRowCount != 3 {
		t.Errorf("ScannedRowCount = %d, want 3 — the scope must stay EXACT; widening it to the alias set is the bug, not the fix", res.ScannedRowCount)
	}

	// 🔴 The gap is NAMED.
	if got, want := res.AliasIdentities, []string{"dl"}; len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
		t.Errorf("AliasIdentities = %v, want %v", got, want)
	}
	if res.AliasUnqualifiedRowCount != 4 {
		t.Errorf("AliasUnqualifiedRowCount = %d, want 4 — without this number the operator sees a clean report over a half-done repair", res.AliasUnqualifiedRowCount)
	}

	// And the sibling's rows really are untouched.
	for i, id := range sibling {
		if got := repoOf(t, db, id); got != repoid.Unqualified {
			t.Errorf("sibling row %d (id %d) repo = %q, want it left on the sentinel — the repair must never write rows it was not scoped to", i, id, got)
		}
	}
	for i, id := range primary {
		if got := repoOf(t, db, id); got != "acme/app" {
			t.Errorf("control: primary row %d (id %d) repo = %q, want acme/app", i, id, got)
		}
	}

	// The count TRACKS REALITY rather than being a constant: repairing the alias
	// identity too must drive it to zero, from the other direction.
	res2, err := db.RepairRepo(ctx, RepairRepoOptions{
		Developer:     "dl",
		SlugBySession: map[string]string{"sess-a": "acme/app"},
		Commit:        true,
	})
	if err != nil {
		t.Fatalf("repair alias identity: %v", err)
	}
	if got, want := res2.AliasIdentities, []string{"devlead"}; len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
		t.Errorf("second run AliasIdentities = %v, want %v (resolution is symmetric — naming either identity finds the other)", got, want)
	}
	if res2.AliasUnqualifiedRowCount != 0 {
		t.Errorf("second run AliasUnqualifiedRowCount = %d, want 0 — the first run repaired those rows, so the NOTE must stop firing", res2.AliasUnqualifiedRowCount)
	}
}

// TestRepairRepoAliasFieldsAreZeroWithoutAliases is the control arm for the test
// above: a developer with no alias rows must report NO sibling identities and NO
// unexamined rows. Without it, a bug that populated the fields unconditionally
// (or reported the developer as its own sibling) would still pass, and the
// command would print a NOTE telling every operator to re-run for an identity
// that does not exist.
func TestRepairRepoAliasFieldsAreZeroWithoutAliases(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "sess-a", costMicro: 10},
		{developer: "bob", sessionID: "sess-a", costMicro: 20},
	})

	res, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app"},
	})
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if len(res.AliasIdentities) != 0 {
		t.Errorf("AliasIdentities = %v, want none — bob is a DIFFERENT PERSON, not an alias, and naming him would be a privacy leak as well as wrong", res.AliasIdentities)
	}
	if res.AliasUnqualifiedRowCount != 0 {
		t.Errorf("AliasUnqualifiedRowCount = %d, want 0", res.AliasUnqualifiedRowCount)
	}
	// 🔴 ANCHOR. Every assertion above is "this field is zero", and the zero VALUE
	// of RepairRepoResult satisfies all of them — so a RepairRepo that returned
	// RepairRepoResult{}, nil would pass. These two pin that the run really
	// happened AND that it stayed scoped to alice (bob's row is not scanned),
	// which makes this a developer-scope arm as well as an alias one.
	if res.ScannedRowCount != 1 {
		t.Errorf("ScannedRowCount = %d, want 1 — alice owns one row and bob's must not be scanned", res.ScannedRowCount)
	}
	if res.ChangedRowCount != 1 {
		t.Errorf("ChangedRowCount = %d, want 1", res.ChangedRowCount)
	}
}

// ---------------------------------------------------------------------------
// Review finding: the report never said how many MAPPING ENTRIES matched nothing.
// ---------------------------------------------------------------------------

// TestRepairRepoReportsMappingEntriesThatMatchedNoRow covers the command's most
// likely failure mode, which was entirely invisible: every other number in the
// result describes the DATABASE, so an operator whose 400-entry mapping was a
// stale export, or carried a typo'd UUID, or had a UTF-8 BOM on line 1, got the
// same clean "0 row(s) repaired" as an operator whose history was already fine.
//
// The BOM case is in the fixture deliberately. strings.TrimSpace does NOT strip
// U+FEFF, so the first entry of a BOM'd --map-file silently carries three extra
// bytes and can never match — and it is the failure an operator is least able to
// see, because the two session ids look identical on screen.
func TestRepairRepoReportsMappingEntriesThatMatchedNoRow(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "sess-a", costMicro: 10},
		{developer: "alice", sessionID: "sess-real", repo: "acme/kept", costMicro: 20},
		{developer: "bob", sessionID: "sess-bob", costMicro: 30},
	})

	// A real U+FEFF, spelled as an escape: a literal BOM in Go source is a
	// compile error, which is itself a hint at how invisible this byte is.
	const bomSessA = "\ufeffsess-a"
	res, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		Developer: "alice",
		SlugBySession: map[string]string{
			"sess-a":    "acme/app",  // matches, and is repaired
			"sess-real": "acme/kept", // matches an ALREADY-QUALIFIED row
			"sess-typo": "acme/app",  // matches nothing: a typo or stale export
			bomSessA:    "acme/app",  // matches nothing: the BOM case
			"sess-bob":  "acme/app",  // matches nothing: another developer's session
		},
	})
	if err != nil {
		t.Fatalf("repair: %v", err)
	}

	if res.MappedSessionCount != 5 {
		t.Errorf("MappedSessionCount = %d, want 5 (what the operator supplied, so the report can say 'N of your M')", res.MappedSessionCount)
	}
	want := []string{"sess-bob", "sess-typo", bomSessA} // ascending; U+FEFF sorts after ASCII
	if len(res.UnmatchedSessions) != len(want) {
		t.Fatalf("UnmatchedSessions = %q, want %q", res.UnmatchedSessions, want)
	}
	for i := range want {
		if res.UnmatchedSessions[i] != want[i] {
			t.Errorf("UnmatchedSessions[%d] = %q, want %q (ascending order — a report whose lines reorder run to run cannot be diffed)", i, res.UnmatchedSessions[i], want[i])
		}
	}

	// 🔴 CONTROL: "sess-real" matched an already-qualified row, so it is NOT
	// unmatched. The question the field answers is "did your entry correspond to
	// anything at all", not "did it repair something" — conflating the two would
	// tell an operator to go fix an entry that is already correct.
	for _, got := range res.UnmatchedSessions {
		if got == "sess-real" {
			t.Error("an entry that matched an already-qualified row was reported as unmatched; it did correspond to a row, and telling the operator otherwise sends them hunting for a non-problem")
		}
	}
}

// TestRepairRepoAllMappingEntriesMatchedReportsNoGap is the control arm: a
// mapping in which every entry lands must report an EMPTY unmatched set. Without
// it, a bug that reported every entry as unmatched would pass the test above.
func TestRepairRepoAllMappingEntriesMatchedReportsNoGap(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	seedRepairRows(t, db, []repairSeed{
		{developer: "alice", sessionID: "sess-a", costMicro: 10},
		{developer: "alice", sessionID: "sess-b", costMicro: 20},
	})

	res, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app", "sess-b": "acme/lib"},
	})
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if len(res.UnmatchedSessions) != 0 {
		t.Errorf("UnmatchedSessions = %q, want none — every entry matched a row", res.UnmatchedSessions)
	}
	if res.MappedSessionCount != 2 {
		t.Errorf("MappedSessionCount = %d, want 2", res.MappedSessionCount)
	}
}

// TestRepairRepoMappingGapsAreReportedOnTheDryRunToo pins the gap reporting to
// the DRY RUN, which is the only place it is useful: an operator discovers a
// broken mapping BEFORE committing, or the dry run has not earned its keep.
func TestRepairRepoMappingGapsAreReportedOnTheDryRunToo(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	seedRepairRows(t, db, []repairSeed{{developer: "alice", sessionID: "sess-a", costMicro: 10}})

	res, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app", "sess-gone": "acme/app"},
		Commit:        false,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if res.Committed {
		t.Fatal("fixture: this must be a dry run")
	}
	if len(res.UnmatchedSessions) != 1 || res.UnmatchedSessions[0] != "sess-gone" {
		t.Errorf("dry-run UnmatchedSessions = %q, want [sess-gone] — the dry run is where a broken mapping is still fixable", res.UnmatchedSessions)
	}
}

// TestRepairRepoCommitAppliesEveryRowAcrossManyRows exercises the commit loop at
// a size where its prepared statements matter, and guards the refactor that
// hoisted the PREPARE out of the loop: every row must still get its own
// before-image and its own compare-and-swap UPDATE. A prepare-once refactor that
// reused a bound argument set, or that skipped rows, would show up here and
// nowhere in the small fixtures above.
//
// (The refactor's motivation is speed, not behaviour — 752ms -> 317ms at 123k
// rows. BenchmarkRepairRepoCommit is where that is measured; this test is the
// correctness half.)
func TestRepairRepoCommitAppliesEveryRowAcrossManyRows(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	const perSession = 40
	seeds := make([]repairSeed, 0, perSession*2)
	for i := 0; i < perSession; i++ {
		seeds = append(seeds,
			repairSeed{developer: "alice", sessionID: "sess-a", costMicro: int64(i + 1)},
			repairSeed{developer: "alice", sessionID: "sess-b", costMicro: int64(100 + i)},
		)
	}
	ids := seedRepairRows(t, db, seeds)

	res, err := db.RepairRepo(context.Background(), RepairRepoOptions{
		Developer:     "alice",
		SlugBySession: map[string]string{"sess-a": "acme/app", "sess-b": "acme/lib"},
		Commit:        true,
	})
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if res.ChangedRowCount != int64(len(seeds)) {
		t.Fatalf("ChangedRowCount = %d, want %d", res.ChangedRowCount, len(seeds))
	}
	// Row-for-row, not by aggregate: an aggregate count can be right while
	// individual rows are wrong.
	for i, id := range ids {
		want := "acme/app"
		if i%2 == 1 {
			want = "acme/lib"
		}
		if got := repoOf(t, db, id); got != want {
			t.Fatalf("row %d (id %d) repo = %q, want %q", i, id, got, want)
		}
	}
	// 🔴 THE LEDGER IS READ ROW-FOR-ROW, NOT COUNTED. A COUNT cannot see the
	// failure this test exists to catch: a prepared-statement hoist that binds a
	// CONSTANT old_repo/new_repo (say, changes[0]'s) while the UPDATE loop still
	// binds correctly writes the right NUMBER of before-images, with the right
	// token_event_ids, and the wrong slug on half of them. UNIQUE(repair_id,
	// token_event_id) does not catch it either — the ids are all distinct. And
	// this is the ONLY fixture with two target repos; every other before-image
	// test uses one row and one repo, where a constant binding is indistinguishable
	// from a correct one. The ledger is the substrate a future --undo restores
	// from, so a ledger that is wrong at row grain is worse than no ledger.
	type beforeImage struct{ oldRepo, newRepo string }
	ledger := map[int64]beforeImage{}
	auditRows, err := db.db.QueryContext(context.Background(),
		`SELECT token_event_id, old_repo, new_repo FROM repo_repair_row_audit WHERE repair_id = ?`, res.RepairID)
	if err != nil {
		t.Fatalf("read before-images: %v", err)
	}
	for auditRows.Next() {
		var id int64
		var bi beforeImage
		if err := auditRows.Scan(&id, &bi.oldRepo, &bi.newRepo); err != nil {
			_ = auditRows.Close()
			t.Fatalf("scan before-image: %v", err)
		}
		ledger[id] = bi
	}
	if err := auditRows.Err(); err != nil {
		_ = auditRows.Close()
		t.Fatalf("iterate before-images: %v", err)
	}
	if err := auditRows.Close(); err != nil {
		t.Fatalf("close before-images: %v", err)
	}
	if len(ledger) != len(seeds) {
		t.Fatalf("before-images = %d, want %d — one per changed row, always", len(ledger), len(seeds))
	}
	for i, id := range ids {
		wantNew := "acme/app"
		if i%2 == 1 {
			wantNew = "acme/lib"
		}
		got, ok := ledger[id]
		if !ok {
			t.Fatalf("row %d (id %d) has no before-image", i, id)
		}
		if got.oldRepo != repoid.Unqualified || got.newRepo != wantNew {
			t.Fatalf("row %d (id %d) before-image = (%q -> %q), want (%q -> %q) — an inverse-undo from this ledger would restore a value that was never there",
				i, id, got.oldRepo, got.newRepo, repoid.Unqualified, wantNew)
		}
	}
	if n := countRows(t, db, `SELECT COUNT(1) FROM repo_repair_audit WHERE repair_id = ?`, res.RepairID); n != 2 {
		t.Errorf("aggregate audit rows = %d, want 2 (one per target repo)", n)
	}
}

// repairRepoBenchRows is the fixture size BOTH benchmarks below use, and the
// number RepairRepo's prepare-block comment names. It is a const rather than a
// literal in each function so the two arms can never be compared at different
// scales — a ratio taken across two sizes is not a ratio.
const repairRepoBenchRows = 5000

// benchmarkRepairRepoCommit is the shared body of the two arms. Only `unprepared`
// differs between them, so the ratio they yield is attributable to the
// prepared-statement hoist and to nothing else about the harness.
//
// ⚠️ WHAT IS INSIDE THE TIMED REGION: the WHOLE RepairRepo call — the candidate
// scan, the classification, the commit loop, the aggregate audit insert and the
// COMMIT. Only the seeding is excluded. The hoist affects the commit loop alone,
// so the ratio these two arms yield is DILUTED by everything else in the call and
// is a lower bound on the loop's own speedup, never an estimate of it. (An earlier
// version of this comment said the benchmark "measures the commit loop", which
// invited exactly the wrong reading of its output.)
//
// ⚠️ ALWAYS pass an explicit small -benchtime; the figures in RepairRepo's prepare
// block are at -benchtime=3x. Each iteration rebuilds its own fixture from
// scratch, and that seeding is NOT timed — so the default -benchtime lets b.N
// climb into the dozens and the wall time into the tens of seconds while the
// reported ns/op barely moves. The number is per-repair, not per-row.
func benchmarkRepairRepoCommit(b *testing.B, unprepared bool) {
	b.Helper()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		dir := b.TempDir()
		db, err := Open(filepath.Join(dir, fmt.Sprintf("bench-%d.db", i)))
		if err != nil {
			b.Fatalf("open: %v", err)
		}
		ctx := context.Background()
		for r := 0; r < repairRepoBenchRows; r++ {
			if err := db.InsertTokenEvent(ctx, TokenEvent{
				Developer: "alice", IssueID: "issue-493", Model: "claude-sonnet-4",
				InputTok: 1000, CostMicro: int64(r), Source: "jsonl", Fidelity: "realtime",
				SessionID:      "sess-a",
				IdempotencyKey: fmt.Sprintf("bench-%d-%d", i, r),
				Timestamp:      time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
			}); err != nil {
				b.Fatalf("seed: %v", err)
			}
		}
		b.StartTimer()
		res, err := db.RepairRepo(ctx, RepairRepoOptions{
			Developer:     "alice",
			SlugBySession: map[string]string{"sess-a": "acme/app"},
			Commit:        true,
			unprepared:    unprepared,
		})
		if err != nil {
			b.Fatalf("repair: %v", err)
		}
		b.StopTimer()
		// Assert the arm did the work it is being timed for. A benchmark that
		// silently repaired zero rows would report a beautiful ns/op and a
		// meaningless ratio.
		if res.ChangedRowCount != repairRepoBenchRows {
			b.Fatalf("changed %d rows, want %d — the arm is not doing the work it is timed for", res.ChangedRowCount, repairRepoBenchRows)
		}
		_ = db.Close()
		b.StartTimer()
	}
}

// BenchmarkRepairRepoCommit is the PREPARED arm — the shipping path.
func BenchmarkRepairRepoCommit(b *testing.B) { benchmarkRepairRepoCommit(b, false) }

// BenchmarkRepairRepoCommitUnprepared is the CONTROL ARM, and it is the reason
// RepairRepo's prepare-block claim is checkable at all. It drives the identical
// loop with the pre-hoist executor (full SQL text re-sent per row), so the ratio
// the comment states can be reproduced from this tree with two commands instead of
// by hand-reverting the optimization. Without it the claim was a number no reader
// could obtain — which is how the previous figures drifted 5x from reality
// unnoticed.
func BenchmarkRepairRepoCommitUnprepared(b *testing.B) { benchmarkRepairRepoCommit(b, true) }

// TestRepairRepoUnpreparedPathIsIdentical pins the half of the control arm that a
// benchmark cannot: that the unprepared executor does the SAME WORK as the
// prepared one. A control arm that skipped the before-image write, or wrote a
// different one, would report a flattering ratio for the hoist while measuring
// less work — the exact failure mode that makes a benchmark lie.
//
// GUARD COVERAGE: point repairTxExecer at a different statement, or drop the
// before-image exec from the unprepared branch, and this test fails.
func TestRepairRepoUnpreparedPathIsIdentical(t *testing.T) {
	seeds := []repairSeed{
		{developer: "alice", sessionID: "sess-a", costMicro: 100},
		{developer: "alice", sessionID: "sess-b", costMicro: 200},
		{developer: "alice", sessionID: "sess-real", repo: "acme/keep", costMicro: 300},
	}
	mapping := map[string]string{"sess-a": "acme/app", "sess-b": "acme/lib"}

	type outcome struct {
		res      RepairRepoResult
		repos    map[int]string
		auditRow int
	}
	run := func(t *testing.T, unprepared bool) outcome {
		t.Helper()
		db, cleanup := newTestDB(t)
		defer cleanup()
		ids := seedRepairRows(t, db, seeds)
		res, err := db.RepairRepo(context.Background(), RepairRepoOptions{
			Developer:     "alice",
			SlugBySession: mapping,
			Commit:        true,
			ToolVersion:   "test",
			unprepared:    unprepared,
		})
		if err != nil {
			t.Fatalf("repair (unprepared=%v): %v", unprepared, err)
		}
		repos := map[int]string{}
		for i, id := range ids {
			// Keyed by SEED INDEX, not row id: the two arms build separate
			// databases, so their autoincrement ids differ by construction and a
			// map keyed on them could never compare equal.
			repos[i] = repoOf(t, db, id)
		}
		n := countRows(t, db, `SELECT COUNT(1) FROM repo_repair_row_audit WHERE repair_id = ?`, res.RepairID)
		// RepairID is a fresh random id per run, so it can never match across the
		// two arms — blank it rather than comparing it.
		res.RepairID = ""
		return outcome{res: res, repos: repos, auditRow: n}
	}

	prepared := run(t, false)
	control := run(t, true)

	if !reflect.DeepEqual(prepared.res, control.res) {
		t.Errorf("result differs between the prepared and unprepared paths:\n prepared = %+v\n control  = %+v", prepared.res, control.res)
	}
	if !reflect.DeepEqual(prepared.repos, control.repos) {
		t.Errorf("token_events.repo differs between the paths:\n prepared = %v\n control  = %v", prepared.repos, control.repos)
	}
	if prepared.auditRow != control.auditRow {
		t.Errorf("before-image row count: prepared = %d, control = %d — the control arm is not writing the same ledger, so any ratio it produces is measuring less work", prepared.auditRow, control.auditRow)
	}
	// Control on the control: the shared fixture must actually have exercised the
	// loop. Two arms that both changed nothing would compare equal and prove
	// nothing.
	if prepared.auditRow != 2 {
		t.Fatalf("before-image rows = %d, want 2 — the fixture did not exercise the commit loop, so the equality above is vacuous", prepared.auditRow)
	}
}

// TestNormalizeRepairMappingErrorsGoThroughTheLogsafeBarrier pins the barrier
// aliasUnqualifiedRows articulates ("an error message is not exempt from the
// barrier just because it is an error") on the three validation errors two
// functions away, which had stayed on bare %q.
//
// THE ASSERTION DISTINGUISHES THE TWO. Both %q and logsafe.Str produce a
// single-line result at runtime, so "no raw newline" would pass either way and
// prove nothing. logsafe.Str STRIPS CR/LF before quoting, so the literal
// two-character escape `\n` is absent from its output and present in %q's. That is
// the difference this checks — revert any of the three sites to %q and it fails.
func TestNormalizeRepairMappingErrorsGoThroughTheLogsafeBarrier(t *testing.T) {
	// A CRLF-bearing session id and slug, of the shape a hand-edited or
	// tool-generated --map file can carry.
	const dirty = "sess\r\ntime=2026-08-03 level=ERROR msg=\"forged\""

	// The two REACHABLE errors, each with the CRLF in a different argument so both
	// interpolation points are exercised. The third site — the fixed-point guard —
	// is deliberately absent: it fires only if repoid.Canonical stops being
	// idempotent, so no input can reach it and asserting on it is not possible. See
	// the note on normalizeRepairMapping.
	cases := map[string]map[string]string{
		// Empty session id -> only the SLUG is interpolated.
		"dirty slug, empty session id": {"": dirty},
		// Non-canonical slug -> the SESSION is interpolated (slug is clean here).
		"dirty session id": {dirty: "not-a-slug"},
		// ...and the slug argument of the same error.
		"dirty session id and slug": {dirty: "also/not\r\nvalid/deep/enough.git/"},
	}
	for name, mapping := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeRepairMapping(mapping)
			if err == nil {
				t.Fatalf("normalizeRepairMapping(%v) succeeded; the fixture must FAIL or this test asserts nothing", mapping)
			}
			msg := err.Error()
			if strings.Contains(msg, "\n") || strings.Contains(msg, "\r") {
				t.Errorf("error message carries a RAW CR/LF: %q", msg)
			}
			if strings.Contains(msg, `\n`) || strings.Contains(msg, `\r`) {
				t.Errorf("error message = %s\nwant the CR/LF STRIPPED (logsafe.Str), not escaped (%%q) — %%q is not a barrier CodeQL's go/log-injection recognizes, and this file's own rule is that an error is not exempt from it", msg)
			}
		})
	}

	// CONTROL: the barrier must not eat the diagnostic. An operator has to be able
	// to see which entry was rejected, so a clean value still appears in the text.
	_, err := normalizeRepairMapping(map[string]string{"sess-clean": "not-a-slug"})
	if err == nil {
		t.Fatal("control: a non-canonical slug must be rejected")
	}
	if !strings.Contains(err.Error(), "sess-clean") || !strings.Contains(err.Error(), "not-a-slug") {
		t.Errorf("control: error = %q, want it to still NAME the offending entry — a barrier that removed the diagnostic would pass the assertions above for the wrong reason", err.Error())
	}
}
