package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The window every test in this file uses. Both bounds are UTC, which is what
// tsWindow normalizes to anyway.
var (
	digestSince = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	digestUntil = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
)

func newDigestDB(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "digest.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func seedDigestEvent(t *testing.T, db *DB, dev, issue string, costMicro int64, at time.Time, key string) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), TokenEvent{
		Developer: dev, IssueID: issue, Model: "claude-sonnet-4",
		InputTok: 1000, OutputTok: 500, CacheRead: 10,
		CacheWrite5m: 5, CacheWrite1h: 1,
		CostMicro: costMicro, Source: "jsonl", Fidelity: "realtime",
		Repo: "acme/tier", IdempotencyKey: key, Timestamp: at,
	}); err != nil {
		t.Fatalf("InsertTokenEvent(%s,%s): %v", dev, issue, err)
	}
}

func seedDigestOutcome(t *testing.T, db *DB, dev, issue string, weight, quality float64, at time.Time, sha string) {
	t.Helper()
	if _, err := db.InsertOutcome(context.Background(), Outcome{
		Developer: dev, IssueID: issue, PRNumber: 1,
		Weight: weight, Quality: quality, MergeCommitSHA: sha,
		Source: "api", WorkType: "feature", Repo: "acme/tier", Timestamp: at,
	}); err != nil {
		t.Fatalf("InsertOutcome(%s,%s): %v", dev, issue, err)
	}
}

// eventsDigest / outcomesDigest read the standard window under an EXPLICIT
// scope. The scope is a required argument rather than defaulted to FleetWide so
// that every call site states which population it measured — a scoped digest and
// a fleet-wide one are different values over different rows, and a helper that
// silently picked one would let a scoped assertion pass on a fleet-wide read.
func eventsDigest(t *testing.T, db *DB, scope RepoScope) Digest {
	t.Helper()
	d, err := db.EventsDigest(context.Background(), digestSince, digestUntil, scope)
	if err != nil {
		t.Fatalf("EventsDigest(%q): %v", scope, err)
	}
	return d
}

func outcomesDigest(t *testing.T, db *DB, scope RepoScope) Digest {
	t.Helper()
	d, err := db.OutcomesDigest(context.Background(), digestSince, digestUntil, scope)
	if err != nil {
		t.Fatalf("OutcomesDigest(%q): %v", scope, err)
	}
	return d
}

// seedDigestWindow fills [digestSince, digestUntil) with three events and two
// outcomes, and puts one event and one outcome on EACH side of the window so
// every test inherits an out-of-window population the digest must ignore.
func seedDigestWindow(t *testing.T, db *DB) {
	t.Helper()
	seedDigestEvent(t, db, "alice", "issue-1", 1_000, digestSince.AddDate(0, 0, 4), "k-a1")
	seedDigestEvent(t, db, "bob", "issue-2", 2_500, digestSince.AddDate(0, 0, 9), "k-b1")
	// Deliberately unkeyed: this row carries a NULL idempotency_key and is the
	// reason the digest is not built over that column. See eventsdigest.go's
	// header; TestDigestCoversRowsWithNullIdempotencyKey asserts it is covered.
	seedDigestEvent(t, db, "carol", "issue-3", 900, digestSince.AddDate(0, 0, 14), "")
	seedDigestOutcome(t, db, "alice", "issue-1", 3, 1, digestSince.AddDate(0, 0, 4), "sha-a1")
	seedDigestOutcome(t, db, "bob", "issue-2", 5, 0.8, digestSince.AddDate(0, 0, 9), "sha-b1")

	seedDigestEvent(t, db, "mallory", "issue-9", 77_777, digestSince.AddDate(0, 0, -17), "k-before")
	seedDigestEvent(t, db, "mallory", "issue-9", 88_888, digestUntil.AddDate(0, 0, 14), "k-after")
	seedDigestOutcome(t, db, "mallory", "issue-9", 9, 1, digestSince.AddDate(0, 0, -17), "sha-before")
	seedDigestOutcome(t, db, "mallory", "issue-9", 9, 1, digestUntil.AddDate(0, 0, 14), "sha-after")
}

// TestDigestSurvivesReopenAndVacuum is the POSITIVE arm: the digest must be a
// function of the window's CONTENT, not of the file's physical layout.
//
// The VACUUM INTO half is the one that actually earns something. Reopening the
// same file could pass on a digest that accidentally depended on page order or
// rowid placement, because nothing moved. VACUUM rebuilds the database from
// scratch into a fresh file — every page reallocated, every b-tree repacked —
// so a digest that survives it is reading rows, not storage.
func TestDigestSurvivesReopenAndVacuum(t *testing.T) {
	db, path := newDigestDB(t)
	seedDigestWindow(t, db)

	first := eventsDigest(t, db, FleetWide)
	firstOut := outcomesDigest(t, db, FleetWide)
	if first.Rows != 3 {
		t.Fatalf("events digest covers %d rows, want 3 — the fixture or the window predicate is wrong, and every comparison below would be over the wrong set", first.Rows)
	}
	if firstOut.Rows != 2 {
		t.Fatalf("outcomes digest covers %d rows, want 2", firstOut.Rows)
	}

	// Arm 1: a fresh Open of the same file.
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got := eventsDigest(t, reopened, FleetWide); got.Value != first.Value {
		t.Errorf("events digest changed across an Open cycle:\n  before %s\n  after  %s\nNothing was written between the two reads, so the digest depends on something other than the rows — a map walk, a driver-rendered timestamp, or the connection's state", first.Value, got.Value)
	}
	if got := outcomesDigest(t, reopened, FleetWide); got.Value != firstOut.Value {
		t.Errorf("outcomes digest changed across an Open cycle:\n  before %s\n  after  %s", firstOut.Value, got.Value)
	}

	// Arm 2: a VACUUM INTO copy — a physically different file holding the same
	// logical rows.
	dest := filepath.Join(t.TempDir(), "vacuumed.db")
	if err := Backup(context.Background(), path, dest); err != nil {
		t.Fatalf("Backup (VACUUM INTO): %v", err)
	}
	copied, err := Open(dest)
	if err != nil {
		t.Fatalf("open vacuumed copy: %v", err)
	}
	t.Cleanup(func() { _ = copied.Close() })

	got := eventsDigest(t, copied, FleetWide)
	if got.Value != first.Value {
		t.Errorf("events digest differs across a VACUUM INTO copy:\n  source %s\n  copy   %s\nVACUUM preserves every row and (for an INTEGER PRIMARY KEY) every id, so a difference means the digest is reading physical layout — page order, rowid placement, or a value the copy re-rendered", first.Value, got.Value)
	}
	if got.Rows != first.Rows {
		t.Errorf("vacuumed copy covers %d rows, source covered %d", got.Rows, first.Rows)
	}
	if got := outcomesDigest(t, copied, FleetWide); got.Value != firstOut.Value {
		t.Errorf("outcomes digest differs across a VACUUM INTO copy:\n  source %s\n  copy   %s", firstOut.Value, got.Value)
	}
}

// TestDigestNegativeControls is the pair the whole surface rests on.
//
// 🔴 BOTH ARMS ARE REQUIRED AND THEY FAIL DIFFERENT MUTANTS. Arm 1 alone is
// satisfied by a digest of the ENTIRE table — it would still change when a row
// lands. Only arm 2 proves the window bound is real. Arm 2 alone is satisfied by
// a constant. Together they say: exactly the in-window rows, and nothing else.
func TestDigestNegativeControls(t *testing.T) {
	db, _ := newDigestDB(t)
	seedDigestWindow(t, db)

	base := eventsDigest(t, db, FleetWide)
	baseOut := outcomesDigest(t, db, FleetWide)

	// 🔴 ARM 1: a late arrival INSIDE an already-published window.
	seedDigestEvent(t, db, "dave", "issue-4", 4_200, digestSince.AddDate(0, 0, 20), "k-late")
	late := eventsDigest(t, db, FleetWide)
	if late.Value == base.Value {
		t.Fatalf("a row inserted INSIDE the published window left the digest unchanged (%s) — the digest cannot detect a late arrival, which is the single thing it exists to make visible", base.Value)
	}
	if late.Rows != base.Rows+1 {
		t.Errorf("row count went %d -> %d after one in-window insert, want +1", base.Rows, late.Rows)
	}

	seedDigestOutcome(t, db, "dave", "issue-4", 2, 1, digestSince.AddDate(0, 0, 20), "sha-late")
	lateOut := outcomesDigest(t, db, FleetWide)
	if lateOut.Value == baseOut.Value {
		t.Fatalf("an outcome inserted INSIDE the published window left the outcomes digest unchanged (%s)", baseOut.Value)
	}

	// 🔴 ARM 2: rows OUTSIDE the window, on both sides. Neither may move it.
	//
	// ⚠️ A NON-CHANGE IS ONLY EVIDENCE IF THE STIMULUS WAS APPLIED, and
	// seedDigestEvent cannot tell you that it was: insertTokenEventSQL is
	// `ON CONFLICT(idempotency_key) DO UPDATE SET` over the token-count columns
	// only, so an insert whose key already exists returns a NIL ERROR and writes
	// nothing. Today's keys are unique, so this arm is honest — but nothing
	// asserted it, and a future fixture edit reusing a key would turn the file's
	// load-bearing control silently vacuous. Arm 1 has its denominator
	// (Rows == base+1); arm 2's "Rows unchanged" is ALSO what a failed insert
	// produces, so it needs a table-level count instead.
	totalBefore := countTokenEvents(t, db)
	seedDigestEvent(t, db, "eve", "issue-5", 5_000, digestUntil.AddDate(0, 0, 3), "k-after-2")
	if got := eventsDigest(t, db, FleetWide); got.Value != late.Value {
		t.Errorf("a row inserted AFTER the window's upper bound changed the digest:\n  before %s\n  after  %s\nThe window bound is not being applied — the digest is over the whole table, so it would flag untampered history every time a new event lands", late.Value, got.Value)
	}
	seedDigestEvent(t, db, "eve", "issue-6", 6_000, digestSince.AddDate(0, 0, -3), "k-before-2")
	if got := eventsDigest(t, db, FleetWide); got.Value != late.Value {
		t.Errorf("a row inserted BEFORE the window's lower bound changed the digest:\n  before %s\n  after  %s", late.Value, got.Value)
	}
	if totalAfter := countTokenEvents(t, db); totalAfter != totalBefore+2 {
		t.Fatalf("the two OUT-of-window inserts added %d rows, want 2 — they silently no-opped (a duplicate idempotency_key returns nil), so the two \"digest unchanged\" assertions above were vacuous", totalAfter-totalBefore)
	}
	if got := eventsDigest(t, db, FleetWide); got.Rows != late.Rows {
		t.Errorf("row count moved to %d after two OUT-of-window inserts, want %d", got.Rows, late.Rows)
	}

	seedDigestOutcome(t, db, "eve", "issue-5", 4, 1, digestUntil.AddDate(0, 0, 3), "sha-after-2")
	seedDigestOutcome(t, db, "eve", "issue-6", 4, 1, digestSince.AddDate(0, 0, -3), "sha-before-2")
	if got := outcomesDigest(t, db, FleetWide); got.Value != lateOut.Value {
		t.Errorf("out-of-window outcomes changed the outcomes digest:\n  before %s\n  after  %s", lateOut.Value, got.Value)
	}
}

// TestDigestCoversARepriceThatMovesNoID is the second negative control, and it
// is the one a naive design fails.
//
// A digest over ids — or over idempotency_key, or over COUNT(*) — is stable
// under a reprice: no row appeared, no row left, no key changed, only
// cost_micro moved. That is precisely the mutation a published cost report must
// not be able to hide, so the digest has to cover MUTABLE columns. The test
// pins that the id set is byte-identical either side of the UPDATE, so the
// changed digest cannot be explained by anything but the value.
func TestDigestCoversARepriceThatMovesNoID(t *testing.T) {
	db, _ := newDigestDB(t)
	ctx := context.Background()
	seedDigestWindow(t, db)

	idsBefore := windowIDs(t, db)
	before := eventsDigest(t, db, FleetWide)

	res, err := db.db.ExecContext(ctx,
		`UPDATE token_events SET cost_micro = cost_micro + 1 WHERE idempotency_key = 'k-b1'`)
	if err != nil {
		t.Fatalf("reprice: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("reprice touched %d rows, want exactly 1 — with 0 the assertion below would be vacuous and with >1 it would prove less than it claims", n)
	}

	after := eventsDigest(t, db, FleetWide)
	if after.Value == before.Value {
		t.Fatalf("a reprice of an in-window row left the digest unchanged (%s) — the digest covers only immutable identity, so a silent restatement of cost is invisible to it", before.Value)
	}
	if after.Rows != before.Rows {
		t.Errorf("row count changed %d -> %d across an UPDATE; it must not", before.Rows, after.Rows)
	}
	// 🔴 THE CONTROL THAT GIVES THE ASSERTION ITS MEANING: nothing moved but the value.
	if idsAfter := windowIDs(t, db); idsAfter != idsBefore {
		t.Fatalf("the in-window id set changed across the UPDATE (%s -> %s), so the changed digest is explained by a moved row rather than by the reprice, and this test proves nothing about mutable-column coverage", idsBefore, idsAfter)
	}

	// The same property for outcomes' quality, which is a float and therefore
	// also exercises the shared canonPriceFloat renderer.
	beforeOut := outcomesDigest(t, db, FleetWide)
	if _, err := db.db.ExecContext(ctx,
		`UPDATE outcomes SET quality = 0.5 WHERE merge_commit_sha = 'sha-b1'`); err != nil {
		t.Fatalf("requality: %v", err)
	}
	if got := outcomesDigest(t, db, FleetWide); got.Value == beforeOut.Value {
		t.Fatalf("an in-window quality change left the outcomes digest unchanged (%s)", beforeOut.Value)
	}
}

// windowIDs returns the in-window token_events ids in (ts, id) order as one
// string, so a test can assert the row SET is unchanged independently of the
// digest it is checking.
func windowIDs(t *testing.T, db *DB) string {
	t.Helper()
	clause, args := tsWindow(digestSince, digestUntil)
	rows, err := db.db.QueryContext(context.Background(),
		`SELECT id FROM token_events WHERE `+clause+digestOrderSQL, args...)
	if err != nil {
		t.Fatalf("windowIDs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("windowIDs scan: %v", err)
		}
		ids = append(ids, fmt.Sprint(id))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("windowIDs rows: %v", err)
	}
	if len(ids) == 0 {
		t.Fatalf("windowIDs read no rows — the control is vacuous")
	}
	return strings.Join(ids, ",")
}

// countTokenEvents returns the WHOLE-TABLE row count, which is what proves an
// out-of-window insert actually happened (the in-window Rows figure cannot).
func countTokenEvents(t *testing.T, db *DB) int64 {
	t.Helper()
	var n int64
	if err := db.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM token_events`).Scan(&n); err != nil {
		t.Fatalf("countTokenEvents: %v", err)
	}
	return n
}

// TestDigestSeesADeletedRow completes the enumeration in the file header, which
// claims "a late-arriving row, a reprice, a DELETION or an edited column" all
// become visible. Insert, reprice and edit each have an arm; deletion did not.
//
// It follows from injectivity plus Rows, but a claim in a doc block that no test
// exercises is exactly the kind this file has already been wrong about once.
func TestDigestSeesADeletedRow(t *testing.T) {
	db, _ := newDigestDB(t)
	ctx := context.Background()
	seedDigestWindow(t, db)

	before := eventsDigest(t, db, FleetWide)
	res, err := db.db.ExecContext(ctx,
		`DELETE FROM token_events WHERE idempotency_key = 'k-a1'`)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("delete removed %d rows, want exactly 1 — the assertion below would be vacuous", n)
	}
	after := eventsDigest(t, db, FleetWide)
	if after.Value == before.Value {
		t.Errorf("deleting an in-window row left the digest unchanged (%s)", before.Value)
	}
	if after.Rows != before.Rows-1 {
		t.Errorf("row count went %d -> %d after one deletion, want -1", before.Rows, after.Rows)
	}
}

// TestDigestCoversRowsWithNullIdempotencyKey is the guard for the correction
// this whole file is built around, and it asserts the two facts DIRECTLY rather
// than trusting the schema comment.
func TestDigestCoversRowsWithNullIdempotencyKey(t *testing.T) {
	db, _ := newDigestDB(t)
	ctx := context.Background()
	seedDigestWindow(t, db)

	// FACT 1: unkeyed rows really do store SQL NULL, so they really are outside
	// the partial unique index.
	var nulls int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM token_events WHERE idempotency_key IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("count nulls: %v", err)
	}
	if nulls == 0 {
		t.Fatalf("no row in the fixture carries a NULL idempotency_key, so this test cannot fail and the premise of the whole file is untested here. The unkeyed seed row must store NULL (insertTokenEventSQL binds it through NULLIF(?, ''))")
	}

	// FACT 2: the index really is PARTIAL — read from sqlite_master, not from a
	// comment.
	var idxSQL string
	if err := db.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_token_events_idempotency'`).Scan(&idxSQL); err != nil {
		t.Fatalf("read index sql: %v", err)
	}
	if !strings.Contains(idxSQL, "WHERE idempotency_key IS NOT NULL") {
		t.Fatalf("idx_token_events_idempotency is no longer partial (%q). If it became total, revisit eventsdigest.go's header — but note a total index would also require the column to be NOT NULL, which is a schema change of its own", idxSQL)
	}

	// THE ASSERTION: mutating the NULL-keyed row must move the digest. A digest
	// built over idempotency_key would not see this row at all.
	before := eventsDigest(t, db, FleetWide)
	res, err := db.db.ExecContext(ctx,
		`UPDATE token_events SET cost_micro = cost_micro + 1 WHERE idempotency_key IS NULL AND developer = 'carol'`)
	if err != nil {
		t.Fatalf("mutate null-keyed row: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("mutation touched %d rows, want 1 — the assertion below would be vacuous", n)
	}
	if got := eventsDigest(t, db, FleetWide); got.Value == before.Value {
		t.Fatalf("changing a row whose idempotency_key is NULL left the digest unchanged (%s) — the digest is keyed on a column those rows do not have, and is silently hashing an INCOMPLETE set", before.Value)
	}
}

// TestDigestCoversOutcomeProvenance is the arm for the columns that move NO
// published number, and it is a different claim from every other test here.
//
// 🔴 THE DIGEST IS THE TAMPER-EVIDENCE HALF OF A PROVENANCE CLAIM, NOT ONLY AN
// ARITHMETIC ONE. Every other coverage arm in this file asks "would a changed
// figure be visible". These two columns change no figure at all: re-point
// merge_commit_sha at a different commit and every score, every total and every
// per-developer split is byte-identical. What changes is WHICH WORK the outcome
// claims to have come from. If that were outside the digest, a verify-report
// could return REPRODUCED while the outcome had been silently re-attributed to
// work it did not come from — telling the truth about the arithmetic and
// something false about the provenance.
//
// ⚠️ Measured: before these columns were covered, all four of the UPDATEs below
// left the digest byte-identical. Keep them covered — the "it moves no published
// number, so drop it" argument is the one that would take them back out, and it
// is answered in outcomeDigestRow's field comments.
func TestDigestCoversOutcomeProvenance(t *testing.T) {
	db, _ := newDigestDB(t)
	ctx := context.Background()
	seedDigestWindow(t, db)

	for _, c := range []struct {
		name   string
		update string
	}{
		{"merge_commit_sha re-pointed at another commit",
			`UPDATE outcomes SET merge_commit_sha = 'FORGED-sha' WHERE merge_commit_sha = 'sha-a1'`},
		{"pr_number re-pointed at another PR",
			`UPDATE outcomes SET pr_number = 9999 WHERE merge_commit_sha = 'sha-b1'`},
	} {
		before := outcomesDigest(t, db, FleetWide)
		res, err := db.db.ExecContext(ctx, c.update)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			t.Fatalf("%s touched %d rows, want exactly 1 — the assertion below would be vacuous", c.name, n)
		}
		after := outcomesDigest(t, db, FleetWide)
		if after.Value == before.Value {
			t.Errorf("%s left the outcomes digest unchanged (%s). The outcome now claims to come from different work, and a verify-report over this window would still say REPRODUCED", c.name, before.Value)
		}
		if after.Rows != before.Rows {
			t.Errorf("%s changed the row count %d -> %d; it must not", c.name, before.Rows, after.Rows)
		}
	}

	// 🔴 CONTROL: NULL and the COALESCEd rendering must not be confusable. A push
	// outcome stores NULL and reads back as ""/0; an outcome that genuinely
	// carries an empty SHA would be indistinguishable — which is ListOutcomes'
	// existing convention (#242) and is inherited deliberately, not by accident.
	// What must NOT happen is NULL colliding with a REAL sha.
	if _, err := db.db.ExecContext(ctx,
		`UPDATE outcomes SET merge_commit_sha = NULL WHERE merge_commit_sha = 'FORGED-sha'`); err != nil {
		t.Fatalf("null out a sha: %v", err)
	}
	nulled := outcomesDigest(t, db, FleetWide)
	if _, err := db.db.ExecContext(ctx,
		`UPDATE outcomes SET merge_commit_sha = 'restored-sha' WHERE merge_commit_sha IS NULL`); err != nil {
		t.Fatalf("restore a sha: %v", err)
	}
	if restored := outcomesDigest(t, db, FleetWide); restored.Value == nulled.Value {
		t.Error("control: nulling a merge_commit_sha and setting it to a real value produce the SAME digest — the provenance column is not reaching the frames after all")
	}
}

// TestDigestEmptyWindowIsADistinguishableSentinel is the vacuity control.
//
// The failure it guards is specific: a digest that folded no rows and emitted
// sha256("") would be byte-identical to what a "we never ran this" path
// produces, so a manifest could publish a confident-looking identity for a
// measurement that never happened. The domain frame makes the empty value a
// SPECIFIC, table-scoped constant instead.
func TestDigestEmptyWindowIsADistinguishableSentinel(t *testing.T) {
	db, _ := newDigestDB(t)
	seedDigestWindow(t, db)
	ctx := context.Background()

	emptySince := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	emptyUntil := time.Date(2019, 2, 1, 0, 0, 0, 0, time.UTC)

	empty, err := db.EventsDigest(ctx, emptySince, emptyUntil, FleetWide)
	if err != nil {
		t.Fatalf("EventsDigest(empty): %v", err)
	}
	if empty.Rows != 0 {
		t.Fatalf("the 'empty' window covers %d rows — the fixture leaks into it and this test proves nothing", empty.Rows)
	}
	if empty.Value == "" {
		t.Fatal("an empty window produced an EMPTY digest value, which is exactly the 'we did not run' state it must be distinguishable from")
	}
	if !strings.HasPrefix(empty.Value, digestScheme+":") {
		t.Errorf("empty-window digest %q is not scheme-tagged", empty.Value)
	}

	// It must not be the hash of nothing.
	nothing := sha256.Sum256(nil)
	if empty.Value == digestScheme+":"+hex.EncodeToString(nothing[:]) {
		t.Error("the empty window's digest IS sha256(\"\") — the domain frame is not being written, so an unrun measurement and a genuinely empty window are indistinguishable")
	}

	// It must not collide with a populated window.
	populated := eventsDigest(t, db, FleetWide)
	if empty.Value == populated.Value {
		t.Errorf("the empty window and the populated window produced the same digest %s", empty.Value)
	}

	// 🔴 And the two TABLES' empty digests must differ, or "no events in W" and
	// "no outcomes in W" are the same published claim.
	emptyOut, err := db.OutcomesDigest(ctx, emptySince, emptyUntil, FleetWide)
	if err != nil {
		t.Fatalf("OutcomesDigest(empty): %v", err)
	}
	if emptyOut.Rows != 0 {
		t.Fatalf("the 'empty' outcomes window covers %d rows", emptyOut.Rows)
	}
	if emptyOut.Value == empty.Value {
		t.Errorf("an empty token_events window and an empty outcomes window produced the same digest %s — the domain separation frame is missing or shared", empty.Value)
	}

	// CONTROL: the empty value is a CONSTANT, not an accident of this store.
	other, _ := newDigestDB(t)
	againstEmptyStore, err := other.EventsDigest(ctx, emptySince, emptyUntil, FleetWide)
	if err != nil {
		t.Fatalf("EventsDigest(other): %v", err)
	}
	if againstEmptyStore.Value != empty.Value {
		t.Errorf("two empty windows produced different digests (%s vs %s) — the sentinel is not stable", empty.Value, againstEmptyStore.Value)
	}
}

// TestDigestFramingIsUniquelyDecodable pins the property the injectivity
// argument in appendTokenEventFrames rests on: the stream decodes into frames
// for arbitrary payloads, and each row contributes a FIXED number of them.
//
// ⚠️ Neither an equality test nor a golden vector notices when this breaks —
// wrapping a field in `if x != "" {…}` keeps every digest self-consistent while
// making distinct windows collidable. This is the assertion that sees it.
func TestDigestFramingIsUniquelyDecodable(t *testing.T) {
	var buf bytes.Buffer

	// Payloads chosen to break a concatenating serializer: separators, newlines,
	// quotes and an empty string all appear inside user-controlled columns.
	rows := []tokenEventDigestRow{
		{ID: 1, TS: "2026-08-01 10:00:00 +0000 UTC", Developer: "a|b", Repo: "", IssueID: "x\ny", Model: "m\"1", Host: "h", Source: "jsonl", BillingMode: "per_token"},
		{ID: 2, TS: "2026-08-02 10:00:00 +0000 UTC", Developer: "a", Repo: "b|c", IssueID: "", Model: "m", Host: "", Source: "", BillingMode: ""},
		{ID: 3, TS: "", Developer: "", Repo: "", IssueID: "", Model: "", Host: "", Source: "", BillingMode: ""},
	}
	for _, r := range rows {
		appendTokenEventFrames(&buf, r)
	}
	if got := countFrames(t, buf.Bytes()); got != len(rows)*digestFramesPerTokenEvent {
		t.Fatalf("%d rows decoded to %d frames, want %d (%d per row). Rows are no longer fixed-arity, so the frame stream cannot be regrouped into rows and two DISTINCT windows can hash identically", len(rows), got, len(rows)*digestFramesPerTokenEvent, digestFramesPerTokenEvent)
	}

	var obuf bytes.Buffer
	orows := []outcomeDigestRow{
		{ID: 1, TS: "t", Developer: "a|b", Repo: "", IssueID: "i", Weight: 1.5, Quality: 1, WorkType: "feature", Source: "api"},
		{ID: 2, TS: "", Developer: "", Repo: "", IssueID: "", Weight: 0, Quality: 0, WorkType: "", Source: ""},
	}
	for _, r := range orows {
		appendOutcomeFrames(&obuf, r)
	}
	if got := countFrames(t, obuf.Bytes()); got != len(orows)*digestFramesPerOutcome {
		t.Fatalf("%d outcome rows decoded to %d frames, want %d", len(orows), got, len(orows)*digestFramesPerOutcome)
	}

	// 🔴 THE FORGERY ARM. Under any delimiter-joining serializer these two rows
	// serialize identically ("a" + "b|c" vs "a|b" + "c" around a "|"). Framing is
	// what makes them distinct, and this is the assertion that says so.
	var l, r bytes.Buffer
	appendTokenEventFrames(&l, tokenEventDigestRow{ID: 1, Developer: "a", Repo: "b|c"})
	appendTokenEventFrames(&r, tokenEventDigestRow{ID: 1, Developer: "a|b", Repo: "c"})
	if l.String() == r.String() {
		t.Fatal("two rows that differ only in where a '|' falls between developer and repo serialized IDENTICALLY — the serializer is concatenating, so a caller who controls a developer or repo string can forge a matching digest and the tamper-evidence claim is false")
	}
}

// countFrames walks a length-prefixed stream and returns the frame count,
// failing if the stream is not exactly consumed.
func countFrames(t *testing.T, b []byte) int {
	t.Helper()
	n := 0
	for len(b) > 0 {
		if len(b) < 8 {
			t.Fatalf("truncated frame header: %d trailing bytes", len(b))
		}
		size := binary.BigEndian.Uint64(b[:8])
		b = b[8:]
		if uint64(len(b)) < size {
			t.Fatalf("frame claims %d bytes but only %d remain — the stream is not uniquely decodable", size, len(b))
		}
		b = b[size:]
		n++
	}
	return n
}

// TestDigestFramesCoverEveryTokenEventField mutates each field of the row struct
// in turn and requires the serialized bytes to change.
//
// ⚠️ This is deliberately stronger than "field count == frame count", which is
// the obvious pin and is satisfied by writing one field twice and omitting
// another. A field that is added to the struct and forgotten in the appender
// falls silently OUTSIDE the identity — the digest keeps working, and the column
// it no longer covers becomes freely editable without evidence.
func TestDigestFramesCoverEveryTokenEventField(t *testing.T) {
	assertEveryFieldIsFramed(t, tokenEventDigestRow{}, digestFramesPerTokenEvent,
		func(buf *bytes.Buffer, v any) { appendTokenEventFrames(buf, v.(tokenEventDigestRow)) })
}

func TestDigestFramesCoverEveryOutcomeField(t *testing.T) {
	assertEveryFieldIsFramed(t, outcomeDigestRow{}, digestFramesPerOutcome,
		func(buf *bytes.Buffer, v any) { appendOutcomeFrames(buf, v.(outcomeDigestRow)) })
}

func assertEveryFieldIsFramed(t *testing.T, zero any, wantFrames int, appendFn func(*bytes.Buffer, any)) {
	t.Helper()
	typ := reflect.TypeOf(zero)

	var base bytes.Buffer
	appendFn(&base, zero)
	if got := countFrames(t, base.Bytes()); got != wantFrames {
		t.Fatalf("%s emits %d frames, want %d", typ.Name(), got, wantFrames)
	}
	if typ.NumField() != wantFrames {
		t.Fatalf("%s has %d fields but emits %d frames — the two must agree, or a field is written twice while another is not written at all", typ.Name(), typ.NumField(), wantFrames)
	}

	checked := 0
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		mutated := reflect.New(typ).Elem()
		mutated.Set(reflect.ValueOf(zero))
		switch f.Type.Kind() {
		case reflect.String:
			mutated.Field(i).SetString("MUTANT")
		case reflect.Int64:
			mutated.Field(i).SetInt(987654321)
		case reflect.Float64:
			mutated.Field(i).SetFloat(1.2345)
		default:
			t.Fatalf("%s.%s has unhandled kind %s — this test cannot mutate it, so the field is UNCHECKED. Extend the switch rather than letting it pass", typ.Name(), f.Name, f.Type.Kind())
		}
		var got bytes.Buffer
		appendFn(&got, mutated.Interface())
		if got.String() == base.String() {
			t.Errorf("%s.%s is declared in the digest row but changing it does NOT change the serialized bytes — the column is outside the digest's identity and can be edited without evidence. Add it to the appender (and bump digestScheme, because doing so changes every previously published digest)", typ.Name(), f.Name)
		}
		checked++
	}
	// NOTE: a `checked != NumField()` assertion would be theatre — checked++ runs
	// on every iteration of a loop bounded by NumField(), so it cannot fail. The
	// real control is the `default:` arm above: a field whose kind this test does
	// not know how to mutate is UNCHECKED, and it fails loudly rather than being
	// silently counted as covered.
	_ = checked
}

// TestDigestNormalizesNegativeZeroWeight pins that the digest reuses #713's
// canonPriceFloat rather than a bare FormatFloat.
//
// FormatFloat(-0.0,'x',-1,64) is "-0x0p+00" and FormatFloat(0.0,…) is "0x0p+00",
// so without the normalization two arithmetically identical windows would report
// different identities — a false tamper alarm on untouched data, which is the
// worst failure this surface can have.
func TestDigestNormalizesNegativeZeroWeight(t *testing.T) {
	negZero := math.Copysign(0, -1)
	if !math.Signbit(negZero) {
		t.Fatal("the fixture value is not a negative zero; the test is vacuous")
	}

	var pos, neg bytes.Buffer
	appendOutcomeFrames(&pos, outcomeDigestRow{ID: 1, TS: "t", Weight: 0, Quality: 0})
	appendOutcomeFrames(&neg, outcomeDigestRow{ID: 1, TS: "t", Weight: negZero, Quality: negZero})
	if pos.String() != neg.String() {
		t.Errorf("+0.0 and -0.0 weights/qualities serialize differently — canonPriceFloat's normalization is not being applied, so an arithmetically identical window reports a changed identity")
	}

	// CONTROL: the renderer is not simply collapsing everything to one string.
	var other bytes.Buffer
	appendOutcomeFrames(&other, outcomeDigestRow{ID: 1, TS: "t", Weight: math.SmallestNonzeroFloat64, Quality: 0})
	if other.String() == pos.String() {
		t.Error("control: the smallest non-zero float serializes the same as zero — the float renderer is lossy and two genuinely different weights would digest identically")
	}
}

// TestDigestHashesRowsInTSIDOrder pins the ORDER BY, which every other test in
// this file is blind to.
//
// 🔴 WHY THE OTHER TESTS CANNOT SEE THIS. In the ordinary fixture, rows are
// inserted in ascending ts, so rowid order and (ts, id) order COINCIDE.
// Deleting the ORDER BY entirely — the #711 defect shape, an unordered read
// feeding a published number — leaves every stability and negative-control arm
// green, because SQLite happens to hand back rowid order and rowid order is the
// right answer for that fixture. Here the ids are assigned so rowid order is the
// exact REVERSE of ts order, which is the only arrangement that can tell them
// apart.
//
// The expectation is built by hand from the row values in (ts, id) order rather
// than by re-running the production query, so this is not the query checking
// itself.
func TestDigestHashesRowsInTSIDOrder(t *testing.T) {
	db, _ := newDigestDB(t)
	ctx := context.Background()

	early := digestSince.AddDate(0, 0, 3)
	late := digestSince.AddDate(0, 0, 20)

	// id 1 carries the LATE ts, id 2 the EARLY one: rowid order is (1,2), the
	// correct (ts, id) order is (2,1).
	for _, r := range []struct {
		id  int64
		dev string
		at  time.Time
	}{{1, "late-row", late}, {2, "early-row", early}} {
		if _, err := db.db.ExecContext(ctx, `
			INSERT INTO token_events (id, developer, issue_id, model, input_tok, output_tok,
			    cache_read, cache_write_5m, cache_write_1h, cost_micro, source, fidelity,
			    repo, price_version, host, billing_mode, ts)
			VALUES (?, ?, 'issue-1', 'claude-sonnet-4', 1000, 500, 10, 5, 1, 1234,
			        'jsonl', 'realtime', 'acme/tier', 0, 'unknown', 'per_token', ?)`,
			r.id, r.dev, r.at); err != nil {
			t.Fatalf("seed %s: %v", r.dev, err)
		}
	}

	// Read back the STORED ts bytes so the expectation uses the same values the
	// digest sees; only the ORDER below is asserted.
	tsOf := func(id int64) string {
		var s string
		if err := db.db.QueryRowContext(ctx,
			`SELECT CAST(ts AS TEXT) FROM token_events WHERE id = ?`, id).Scan(&s); err != nil {
			t.Fatalf("read ts of id %d: %v", id, err)
		}
		return s
	}
	row := func(id int64, dev string) tokenEventDigestRow {
		return tokenEventDigestRow{
			ID: id, TS: tsOf(id), Developer: dev, Repo: "acme/tier", IssueID: "issue-1",
			Model: "claude-sonnet-4", Host: "unknown", InputTok: 1000, OutputTok: 500,
			CacheRead: 10, CacheWrite5m: 5, CacheWrite1h: 1, CostMicro: 1234,
			PriceVersion: 0, BillingMode: "per_token", Source: "jsonl",
			Fidelity: "realtime",
		}
	}
	byTS := []tokenEventDigestRow{row(2, "early-row"), row(1, "late-row")}
	byRowID := []tokenEventDigestRow{row(1, "late-row"), row(2, "early-row")}

	got := eventsDigest(t, db, FleetWide)
	if got.Rows != 2 {
		t.Fatalf("digest covers %d rows, want 2", got.Rows)
	}
	if want := expectedEventsDigest(byTS); got.Value != want {
		t.Errorf("digest is %s, want %s — the rows are not being hashed in (ts, id) order. An unordered or rowid-ordered read makes the digest a function of INSERTION order, so the same rows can hash two ways and an untampered window reports as changed", got.Value, want)
	}

	// 🔴 CONTROL: the two orders must actually be distinguishable, or the
	// assertion above is satisfied by any ordering at all.
	if expectedEventsDigest(byTS) == expectedEventsDigest(byRowID) {
		t.Fatal("control: (ts, id) order and rowid order produce the SAME expected digest, so this fixture cannot tell an ordered read from an unordered one and the assertion above proves nothing")
	}
}

// TestDigestCoversRowsWithAnUnparseableTimestamp pins that a #723-shaped row is
// DIGESTIBLE AT ALL.
//
// ⚠️ IT DOES NOT PIN THE CAST, AND AN EARLIER VERSION OF THIS COMMENT CLAIMED IT
// DID. Measured: dropping `CAST(ts AS TEXT)` from both projections is killed by
// TestDigestHashesRowsInTSIDOrder and TestDigestGoldenVectorMatchesTheLiveRead —
// not by this test, which passes either way because an unrecognised layout is
// passed through verbatim by both routes. The claim below is the narrower, true
// one; the CAST's guard lives in those two tests.
//
// The ts column holds at least three textual encodings side by side (see
// eventsdigest.go's header). One of them — a Go time.Time written in a non-UTC
// zone, the #723 shape — is a layout modernc.org/sqlite does NOT recognise.
// Measured: scanning such a value into a time.Time HARD-ERRORS ("unsupported
// Scan, storing driver.Value type string into type *time.Time"), so a single
// such row anywhere in a window would make the whole window undigestable and the
// manifest would report an error instead of an identity.
//
// This test seeds exactly that row and requires the digest to cover it.
func TestDigestCoversRowsWithAnUnparseableTimestamp(t *testing.T) {
	db, _ := newDigestDB(t)
	ctx := context.Background()
	seedDigestWindow(t, db)

	before := eventsDigest(t, db, FleetWide)

	const rawTS = "2026-08-05 12:00:00 +0100 X"
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO token_events (developer, issue_id, model, cost_micro, source, fidelity,
		    repo, price_version, host, billing_mode, ts)
		VALUES ('zoe', 'issue-7', 'm', 42, 'proxy', 'realtime', 'acme/tier', 0, 'unknown',
		        'per_token', ?)`, rawTS); err != nil {
		t.Fatalf("seed unparseable-ts row: %v", err)
	}

	// CONTROL: the row must really be inside the window, or the assertions are vacuous.
	after, err := db.EventsDigest(ctx, digestSince, digestUntil, FleetWide)
	if err != nil {
		t.Fatalf("EventsDigest over a window containing a driver-unparseable ts: %v\nThe read is converting ts through the driver's DATETIME handling instead of taking the stored bytes, so one non-UTC-zone row (the #723 shape) makes the whole window undigestable", err)
	}
	if after.Rows != before.Rows+1 {
		t.Fatalf("row count went %d -> %d; the unparseable-ts row is not inside the window, so this test proves nothing", before.Rows, after.Rows)
	}
	if after.Value == before.Value {
		t.Error("adding a row with a driver-unparseable ts left the digest unchanged")
	}

	// And the RAW bytes are what got hashed: the driver would have rendered a
	// recognised layout as RFC3339Nano, which this value is not.
	var stored string
	if err := db.db.QueryRowContext(ctx,
		`SELECT CAST(ts AS TEXT) FROM token_events WHERE developer = 'zoe'`).Scan(&stored); err != nil {
		t.Fatalf("read back stored ts: %v", err)
	}
	if stored != rawTS {
		t.Errorf("stored ts is %q, want the raw %q — the write path normalized it and this fixture no longer represents the #723 shape", stored, rawTS)
	}
}

// expectedEventsDigest recomputes a token_events digest from rows supplied in an
// explicit order — the test-side reference for the ordering assertion.
func expectedEventsDigest(rows []tokenEventDigestRow) string {
	h := sha256.New()
	var buf bytes.Buffer
	writeLengthPrefixed(&buf, digestDomainTokenEvents)
	foldFrames(h, &buf)
	for _, r := range rows {
		appendTokenEventFrames(&buf, r)
		foldFrames(h, &buf)
	}
	return finishDigest(h)
}

// TestDigestFailsClosedOnAFailedRead pins the claim EventsDigest's body makes in
// a comment: a read that does not complete must produce an ERROR, never a
// digest.
//
// This is the failure mode that matters most for a published identity. A digest
// is a fixed-width hex string; there is nothing in its SHAPE that distinguishes
// one computed over the whole window from one computed over the first three rows
// before the read died. If a truncated walk returned a well-formed value, the
// manifest would publish a confident identity for a set nobody measured — and
// every later comparison against it would report tampering.
func TestDigestFailsClosedOnAFailedRead(t *testing.T) {
	db, _ := newDigestDB(t)
	seedDigestWindow(t, db)

	// CONTROL: the same call on a live context succeeds, so a failure below is
	// attributable to the cancellation and not to a broken fixture.
	if got := eventsDigest(t, db, FleetWide); got.Rows == 0 {
		t.Fatal("control: the digest read no rows on a live context, so the cancelled arm below proves nothing")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, c := range []struct {
		name string
		call func(context.Context) (Digest, error)
	}{
		{"EventsDigest", func(ctx context.Context) (Digest, error) {
			return db.EventsDigest(ctx, digestSince, digestUntil, FleetWide)
		}},
		{"OutcomesDigest", func(ctx context.Context) (Digest, error) {
			return db.OutcomesDigest(ctx, digestSince, digestUntil, FleetWide)
		}},
	} {
		got, err := c.call(cancelled)
		if err == nil {
			t.Errorf("%s returned a digest (%s, rows=%d) on a CANCELLED context instead of an error — a read that never completed is being published as an identity", c.name, got.Value, got.Rows)
			continue
		}
		if got.Value != "" || got.Rows != 0 {
			t.Errorf("%s returned err=%v but ALSO a non-zero Digest (%q, rows=%d); a caller that logs the error and carries on would publish it", c.name, err, got.Value, got.Rows)
		}
	}
}

// TestDigestReadLoopsCheckRowsErr is an AST arm over this file's own source, and
// like TestDigestSQLRequestsItsOrderExplicitly it is source-level because the
// behaviour is not reachable from a test.
//
// 🔴 WHAT IT GUARDS. `rows.Next()` returns false for two completely different
// reasons: the walk finished, or it FAILED partway. Only rows.Err() tells them
// apart. Drop that check and a truncated walk returns a perfectly well-formed
// digest over a PREFIX of the window — the silent-omission failure this whole
// file is built to avoid, wearing the shape of a valid answer.
//
// ⚠️ Measured by mutation: deleting the rows.Err() check leaves the entire
// behavioural suite green, TestDigestFailsClosedOnAFailedRead included. That
// test cancels the context BEFORE the call, so QueryContext fails and the loop
// is never entered — it covers the sibling error path, not this one. Provoking a
// genuine mid-scan row error needs the context cancelled between QueryContext
// returning and the last Next(), which is a window no exported API can aim at
// and which a timer-based test would hit only flakily. A flaky guard is worse
// than an honest source-level one, so this is the honest source-level one.
//
// ⚠️ READS THE SOURCE FROM DISK, cwd-relative. That is correct under `go test`
// (which runs in the package directory) but means this arm is INVISIBLE to
// `go test -overlay`: an overlay mutant of eventsdigest.go is not what
// parser.ParseFile sees, so this test reports on the on-disk file regardless.
// Anyone mutation-testing this file must patch the real bytes, not an overlay,
// or these source-level arms will appear to survive everything.
func TestDigestReadLoopsCheckRowsErr(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "eventsdigest.go", nil, 0)
	if err != nil {
		t.Fatalf("parse eventsdigest.go: %v", err)
	}

	checked := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !callsMethod(fn, "Next") {
			continue
		}
		checked++
		if !returnsOnRowsErr(fn) {
			t.Errorf("%s iterates rows.Next() but does not `return` on rows.Err(). rows.Next() returns false both when the walk FINISHED and when it FAILED; without the Err() check a truncated read yields a well-formed digest over a prefix of the window, which is indistinguishable from the real one", fn.Name.Name)
		}
	}
	if checked != 2 {
		t.Fatalf("control: found %d functions iterating rows.Next(), want 2 (EventsDigest, OutcomesDigest) — the census missed a read loop, so a loop with no Err() check could sit here unexamined", checked)
	}

	// 🔴 CONTROLS: the detector must be CAPABLE of saying "no", and must say it
	// for BOTH realistic degradations — the guard deleted outright, and the guard
	// present but not fail-closed. The second is the one the first version of this
	// predicate missed.
	for _, c := range []struct {
		name string
		src  string
	}{
		{"no Err() check at all", `package p
func leaky(rows *R) error {
	for rows.Next() {
		_ = rows.Scan()
	}
	return nil
}`},
		{"Err() checked but only logged", `package p
func logged(rows *R) error {
	for rows.Next() {
		_ = rows.Scan()
	}
	if err := rows.Err(); err != nil {
		log(err)
	}
	return nil
}`},
		{"Err() checked but returns the truncated result", `package p
func truncated(rows *R) (Digest, error) {
	for rows.Next() {
		_ = rows.Scan()
	}
	if err := rows.Err(); err != nil {
		return Digest{Value: "partial"}, nil
	}
	return Digest{}, nil
}`},
	} {
		bad, err := parser.ParseFile(token.NewFileSet(), "x.go", c.src, 0)
		if err != nil {
			t.Fatalf("parse control %q: %v", c.name, err)
		}
		fn := bad.Decls[0].(*ast.FuncDecl)
		if !callsMethod(fn, "Next") {
			t.Fatalf("control %q: the detector does not even see the rows.Next() loop it was handed", c.name)
		}
		if returnsOnRowsErr(fn) {
			t.Fatalf("control %q: the detector accepted it. That shape publishes a digest over a TRUNCATED walk, which is the exact failure this guard is named after — so the assertions above are satisfied by an instrument that cannot see the defect", c.name)
		}
	}

	// 🔴 POSITIVE CONTROL: it must also say YES to a correct guard, or it would
	// reject everything and the assertions above would pass for the wrong reason.
	good, err := parser.ParseFile(token.NewFileSet(), "y.go", `package p
func correct(rows *R) (Digest, error) {
	for rows.Next() {
		_ = rows.Scan()
	}
	if err := rows.Err(); err != nil {
		return Digest{}, err
	}
	return Digest{}, nil
}`, 0)
	if err != nil {
		t.Fatalf("parse positive control: %v", err)
	}
	if !returnsOnRowsErr(good.Decls[0].(*ast.FuncDecl)) {
		t.Fatal("positive control: the detector rejected a correctly fail-closed loop, so it would reject every real implementation and says nothing about the ones above")
	}
}

// callsMethod reports whether n contains a call to a selector method of the
// given name (e.g. rows.Next()).
func callsMethod(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// returnsOnRowsErr reports whether n contains an `if` whose condition or init
// calls rows.Err() and whose body returns a NON-NIL error.
//
// 🔴 THE "NON-NIL" PART IS THE WHOLE GUARD, AND THE FIRST VERSION OF THIS
// PREDICATE DID NOT HAVE IT. It asked only "does the branch contain a return",
// which is satisfied by the exact defect it was written to catch:
//
//	if err := rows.Err(); err != nil {
//	    return Digest{Value: finishDigest(h), Rows: n}, nil   // <- truncated!
//	}
//
// Measured against three variants of EventsDigest — baseline true, guard deleted
// false (caught), guard returning the truncated digest TRUE (not caught). A guard
// that passes the failure it is named after is worse than no guard, because it
// reads as coverage.
func returnsOnRowsErr(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		stmt, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		guardsErr := (stmt.Init != nil && callsMethod(stmt.Init, "Err")) || callsMethod(stmt.Cond, "Err")
		if guardsErr && returnsNonNilError(stmt.Body) {
			found = true
		}
		return !found
	})
	return found
}

// returnsNonNilError reports whether n contains a return whose LAST result (the
// error position, by Go convention) is something other than the literal nil.
func returnsNonNilError(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		ret, ok := node.(*ast.ReturnStmt)
		if !ok || len(ret.Results) == 0 {
			return true
		}
		last := ret.Results[len(ret.Results)-1]
		if id, isIdent := last.(*ast.Ident); isIdent && id.Name == "nil" {
			return true // a `return …, nil` inside the Err() branch is the defect
		}
		found = true
		return false
	})
	return found
}

// TestDigestDocCitationsResolve checks that every Test/Benchmark name cited in
// eventsdigest.go's comments actually exists.
//
// 🔴 THIS IS NOT PEDANTRY — IT CAUGHT A REAL ONE. The doc block over the SELECT
// constants cited "TestDigestReadsAreOneSeek", which never existed (the test is
// TestDigestReadsAreOneWindowSeek). A comment that names a guard is a CLAIM that
// the guard exists; a reader who greps for it and finds nothing cannot tell
// "renamed" from "never written" from "deleted when it started failing", and the
// most reassuring reading is the wrong one. The comments in this file carry a
// lot of that weight, so the claims are checked rather than trusted.
//
// ⚠️ READS THE SOURCE FROM DISK, cwd-relative. That is correct under `go test`
// (which runs in the package directory) but means this arm is INVISIBLE to
// `go test -overlay`: an overlay mutant of eventsdigest.go is not what
// parser.ParseFile sees, so this test reports on the on-disk file regardless.
// Anyone mutation-testing this file must patch the real bytes, not an overlay,
// or these source-level arms will appear to survive everything.
func TestDigestDocCitationsResolve(t *testing.T) {
	declared := map[string]bool{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				declared[fn.Name.Name] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("control: found no declared functions in any _test.go, so every citation below would be reported missing")
	}

	src, err := os.ReadFile("eventsdigest.go")
	if err != nil {
		t.Fatalf("read eventsdigest.go: %v", err)
	}
	cited := regexp.MustCompile(`\b(?:Test|Benchmark)[A-Za-z0-9_]+`).FindAllString(string(src), -1)
	seen := map[string]bool{}
	checked := 0
	for _, name := range cited {
		if seen[name] {
			continue
		}
		seen[name] = true
		checked++
		if !declared[name] {
			t.Errorf("eventsdigest.go cites %q, but no such Test/Benchmark is declared in this package. Either the guard was renamed and the comment was not, or the comment claims a guard that does not exist — and a comment naming a guard IS a claim that it exists", name)
		}
	}
	if checked == 0 {
		t.Fatal("control: found no Test/Benchmark citations in eventsdigest.go at all, so this test asserted nothing. Either the comments stopped naming their guards (which is the thing worth keeping) or the regex no longer matches")
	}

	// 🔴 CONTROL: a fabricated name must be reported missing, or the check above
	// is an instrument that never speaks.
	if declared["TestDigestThisNameIsDeliberatelyFabricated"] {
		t.Fatal("control: the declared-function set claims to contain a name that was never written, so it cannot distinguish a real citation from a broken one")
	}
}

// digestPlanCase pairs a digest read with the index it must use.
var digestPlanCases = []struct {
	name  string
	sel   string
	index string
	table string
}{
	{"token_events", tokenEventDigestSelect, "idx_token_events_ts_id", "token_events"},
	{"outcomes", outcomeDigestSelect, "idx_outcomes_ts_id", "outcomes"},
}

// TestDigestReadsAreOneWindowSeek is the BUDGET PIN.
//
// This is a FIFTH full-window scan on top of the four /scores already pays
// (measured context: ~629ms over 172k rows), which is why it lives on the
// manifest surface and not on a scoring path. What that budget assumes is that
// each read is an index SEEK to the window's lower bound followed by an ordered
// walk — no full table scan and, because ORDER BY (ts, id) matches the index,
// no sort. If the planner ever stops using idx_{token_events,outcomes}_ts_id the
// cost becomes proportional to the TABLE rather than the window, and the
// placement argument silently stops holding.
//
// The zero-rows control comes from queryPlan (export_snapshot_test.go), which
// t.Fatalf's when EXPLAIN QUERY PLAN yields no rows — without it every Contains
// check below would be vacuously false and every negative check vacuously true.
func TestDigestReadsAreOneWindowSeek(t *testing.T) {
	db, _ := newDigestDB(t)
	seedDigestWindow(t, db)

	clause, args := tsWindow(digestSince, digestUntil)
	checked := 0
	for _, c := range digestPlanCases {
		plan := queryPlan(t, db, c.sel+clause+digestOrderSQL, args...)
		checked++
		if strings.Contains(plan, "SCAN "+c.table) {
			t.Errorf("%s: the digest read plans as a FULL SCAN (%q). The budget argument in EventsDigest's doc block assumes a window seek, so this read is now proportional to the table rather than the window", c.name, plan)
		}
		if !strings.Contains(plan, c.index) {
			t.Errorf("%s: the digest read does not use %s (%q). That index is (ts, id) — exactly the digest's window predicate and its ORDER BY — so losing it costs both the seek and the sort", c.name, c.index, plan)
		}
		if strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("%s: the digest read materializes a sort (%q). ORDER BY (ts, id) is supposed to be satisfied by index order; a temp b-tree buffers the whole window and defeats the streaming hash's O(1) memory", c.name, plan)
		}
	}
	if checked != len(digestPlanCases) || checked != 2 {
		t.Fatalf("control: planned %d reads, expected 2 — the loop did not cover both digest reads", checked)
	}

	// 🔴 CONTROL: EXPLAIN QUERY PLAN must be CAPABLE of saying SCAN here, or the
	// three assertions above are satisfied by an instrument that never speaks.
	control := queryPlan(t, db, `SELECT COUNT(*) FROM token_events WHERE model = ?`, "claude-sonnet-4")
	if !strings.Contains(control, "SCAN") {
		t.Fatalf("control: a query with no usable index planned as %q, which contains no \"SCAN\" — the planner is not reporting scans in this environment, so the assertions above cannot fail", control)
	}
}

// TestDigestSQLRequestsItsOrderExplicitly is a SOURCE-LEVEL arm, and the reason
// it has to be one is measured rather than assumed.
//
// Deleting the ORDER BY outright — the #711 defect shape — leaves EVERY
// behavioural test in this file green, TestDigestHashesRowsInTSIDOrder included.
// Measured by mutation: with `digestOrderSQL` emptied, the whole suite still
// passes. The reason is that the window predicate is itself on ts, so SQLite
// picks idx_{token_events,outcomes}_ts_id anyway and hands back rows in (ts, id)
// order by coincidence of the plan. The read is then correct only for as long as
// the planner keeps choosing that path — a different index, a table rebuild, or
// a version with different statistics silently reorders a PUBLISHED identity, and
// no fixture reachable from here can distinguish the two states today.
//
// ⚠️ So this asserts the SQL asks for the order rather than that the rows arrive
// in it. That is weaker than a behavioural arm and is not a substitute for one —
// it is what remains when the behaviour is unobservable. The EQP test above is
// the other half: it pins that the requested order is satisfied by the index
// rather than by a sort.
func TestDigestSQLRequestsItsOrderExplicitly(t *testing.T) {
	if !strings.Contains(digestOrderSQL, "ORDER BY ts, id") {
		t.Fatalf("digestOrderSQL is %q and no longer requests ORDER BY (ts, id). The digest would then depend on whatever order the planner happens to return, which is exactly the unordered-read defect #711 fixed on the scoring path — and no behavioural test in this file can see it", digestOrderSQL)
	}
	// CONTROL: both digest reads must actually use the constant, or asserting on
	// it says nothing about the queries that run.
	for _, c := range digestPlanCases {
		if strings.Contains(c.sel, "ORDER BY") {
			t.Errorf("%s: the SELECT constant carries its own ORDER BY, so digestOrderSQL is not the single place the order is decided and the assertion above can be bypassed", c.name)
		}
	}
}

// BenchmarkEventsDigest records what the fifth scan actually costs, so the
// placement decision rests on a measured number rather than an estimate.
//
// MEASURED on this fixture (20,000 in-window rows, darwin/arm64, -benchtime 5x):
// 24.96 ms/op, 619,332 allocs/op — about 1.25 µs and 31 allocations per row.
// Extrapolated to the 172k-row store the /scores budget was measured on, that is
// roughly 215 ms, against ~629 ms for the FOUR comparable scans /scores already
// pays (~157 ms each). ⇒ this scan is ~1.4× one of those, which is the whole
// reason it is emitted on the manifest surface and not on /scores.
//
// ⚠️ WHERE THAT COST ACTUALLY IS, measured by splitting the loop rather than
// assumed: scanning the identical query with NO framing or hashing costs
// 20.47 ms and 539,522 allocs — 82% of the time and 87% of the allocations.
// Framing plus hashing 20,000 rows with no SQL at all costs 3.87 ms and 79,806
// allocs, i.e. ~4 allocations per row. The dominant cost is database/sql and the
// driver materializing 16 columns per row, not this file. Tightening the framing
// (say, strconv.AppendInt into a scratch buffer instead of FormatInt) could
// therefore win at most ~15% of the total, which is not worth trading the
// shared-helper reuse for. Re-measure before concluding otherwise.
func BenchmarkEventsDigest(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.db")
	db, err := Open(path)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	const rows = 20000
	events := make([]TokenEvent, 0, rows)
	for i := 0; i < rows; i++ {
		events = append(events, TokenEvent{
			Developer: fmt.Sprintf("dev-%d", i%50), IssueID: fmt.Sprintf("issue-%d", i%500),
			Model: "claude-sonnet-4", InputTok: 1000, OutputTok: 500,
			CostMicro: int64(i), Source: "jsonl", Fidelity: "realtime", Repo: "acme/tier",
			IdempotencyKey: fmt.Sprintf("bench-%d", i),
			Timestamp:      digestSince.Add(time.Duration(i) * time.Minute),
		})
	}
	if err := db.InsertTokenEvents(ctx, events); err != nil {
		b.Fatalf("seed: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d, err := db.EventsDigest(ctx, digestSince, digestUntil, FleetWide)
		if err != nil {
			b.Fatalf("EventsDigest: %v", err)
		}
		if d.Rows == 0 {
			b.Fatal("benchmark digested ZERO rows — it is measuring an empty window, not a scan")
		}
	}
}

// goldenTokenEventRows / goldenOutcomeRows are FROZEN inputs for the golden
// vectors. They are Go literals, not database reads, so the vector is
// independent of SQLite, the driver, the schema and the host.
var goldenTokenEventRows = []tokenEventDigestRow{
	{
		ID: 1, TS: "2026-08-05 00:00:00 +0000 UTC", Developer: "alice", Repo: "acme/tier",
		IssueID: "issue-1", Model: "claude-sonnet-4", Host: "unknown",
		InputTok: 1000, OutputTok: 500, CacheRead: 10, CacheWrite5m: 5, CacheWrite1h: 1,
		CostMicro: 1234, PriceVersion: 7, BillingMode: "per_token", Source: "jsonl",
		Fidelity: "realtime",
	},
	{
		ID: 2, TS: "2026-08-06 00:00:00 +0000 UTC", Developer: "bob|x", Repo: "",
		IssueID: "issue-2", Model: "gpt-5", Host: "openrouter",
		InputTok: 7, OutputTok: 8, CacheRead: 9, CacheWrite5m: 10, CacheWrite1h: 11,
		CostMicro: -5, PriceVersion: 0, BillingMode: "subscription", Source: "proxy",
		Fidelity: "daily",
	},
}

var goldenOutcomeRows = []outcomeDigestRow{
	{
		ID: 1, TS: "2026-08-05 00:00:00 +0000 UTC", Developer: "alice", Repo: "acme/tier",
		IssueID: "issue-1", Weight: 3.5, Quality: 0.875, WorkType: "feature", Source: "api",
		PRNumber: 42, MergeCommitSHA: "abc123def456",
	},
	{
		// The zero PRNumber / empty SHA are what a push-captured outcome reads back
		// as: both columns are NULL in storage and the projection COALESCEs them.
		// The live-read arm seeds this row with real SQL NULLs, so the pair also
		// pins that rendering.
		ID: 2, TS: "2026-08-06 00:00:00 +0000 UTC", Developer: "bob", Repo: "",
		IssueID: "issue-2", Weight: 0, Quality: 1, WorkType: "bugfix", Source: "github-webhook",
		PRNumber: 0, MergeCommitSHA: "",
	},
}

// The four frozen digests. See TestDigestGoldenVector for what they mean and
// what to do when one of them fails.
const (
	goldenTokenEventDigest = "tierdig1:f7860b950da365026dc3ddf4ad31e80ccfc0cf23eb1493d29b7acae5079a6180"
	goldenOutcomeDigest    = "tierdig1:e5ca09dc60fa2fd7688688a2b9068fb1bd30a2cc714ef675e665482c641366a3"
	goldenEmptyEventDigest = "tierdig1:471f175a8135c504950c9e9f9e92aba4450fd5efa718db0684abe3f5857d1f52"
	goldenEmptyOutcomeDig  = "tierdig1:958df6619e6881d1bf974a9d410d0471f0ee8c9eb38011e1e681cce205f1d758"
)

// TestDigestGoldenVector is the ONLY test in this file that would notice a
// silent change to the wire format, and it is the anchor #713 built for the
// price-table hash (prices_hash_test.go, TestPriceTableHash_GoldenVector).
//
// 🔴 WHY EVERY OTHER TEST HERE IS BLIND TO THIS. They all compare one run of the
// hasher against another run of the SAME hasher — digest-vs-digest — so any
// change to the serializer moves both sides together and they stay green.
// `expectedEventsDigest` is no exception: it reuses six production primitives
// (writeLengthPrefixed, foldFrames, finishDigest, appendTokenEventFrames, the
// domain constant, digestScheme), so it independently pins only the SELECT→struct
// column mapping and the row ORDER. Everything downstream of the struct is
// tautological without a frozen constant.
//
// Measured before this test existed — every one of these left the whole suite
// green while re-identifying every digest the store would ever publish:
//   - swapping the InputTok / OutputTok frame lines
//   - swapping the Developer / Repo frame lines
//   - swapping the Weight / Quality frame lines
//   - digestScheme "tierdig1" -> "tierdig2"
//   - a typo in digestDomainTokenEvents ("/token_event")
//
// The first three are exactly what tokenEventDigestRow's own doc block forbids
// ("the field ORDER is the frame order and is part of the wire format"). Nothing
// held that sentence until this test.
//
// ⛔ WHEN THIS FAILS, DO NOT PASTE IN THE NEW VALUE. A failure means the
// canonicalization changed. Decide whether that was deliberate; if it was, bump
// digestScheme in the SAME commit (and treat every published digest as
// re-identified), then update these constants. If it was not, you have found the
// bug this test exists for.
//
// 📌 THE ONE DELIBERATE REGENERATION SO FAR, RECORDED SO THE PROCEDURE IS SEEN TO
// HAVE BEEN FOLLOWED RATHER THAN ASSERTED. Adding pr_number and merge_commit_sha
// to the outcomes digest reddened goldenOutcomeDigest; the change was ruled
// intentional (see outcomeDigestRow's field comments for the provenance
// argument), so the constant was updated by hand, once, with the diff inspected:
//
//	goldenOutcomeDigest  f4013d09… -> e5ca09dc…
//
// ⭐ THE OTHER THREE CONSTANTS DID NOT MOVE, and that is the check that says the
// edit was scoped. goldenTokenEventDigest is untouched because token_events was
// not touched; BOTH empty sentinels are untouched because a per-row frame change
// cannot reach a digest that folds no rows. A regeneration that moved all four
// would have meant something wider had changed than intended — a domain frame or
// the scheme — and would have needed re-diagnosing, not re-pasting.
//
// digestScheme was deliberately NOT bumped: nothing has published a digest yet
// (#715's manifest surface does not exist), so there is no prior identity to
// invalidate. ⚠️ That reasoning expires the moment #715 ships — after it, a
// change of this shape REQUIRES the bump.
func TestDigestGoldenVector(t *testing.T) {
	if got := expectedEventsDigest(goldenTokenEventRows); got != goldenTokenEventDigest {
		t.Errorf("token_events golden vector changed:\n  got  %s\n  want %s\nThe serialization of a FIXED row set moved, so every digest this store has ever published now means something different. Read this test's doc block before touching the constant", got, goldenTokenEventDigest)
	}
	if got := expectedOutcomesDigest(goldenOutcomeRows); got != goldenOutcomeDigest {
		t.Errorf("outcomes golden vector changed:\n  got  %s\n  want %s", got, goldenOutcomeDigest)
	}
	// The empty-window sentinels are part of the wire format too: they are what a
	// manifest publishes for a window with no rows, so a change to the domain
	// frame silently re-identifies those just as surely.
	if got := expectedEventsDigest(nil); got != goldenEmptyEventDigest {
		t.Errorf("empty token_events sentinel changed:\n  got  %s\n  want %s", got, goldenEmptyEventDigest)
	}
	if got := expectedOutcomesDigest(nil); got != goldenEmptyOutcomeDig {
		t.Errorf("empty outcomes sentinel changed:\n  got  %s\n  want %s", got, goldenEmptyOutcomeDig)
	}

	// 🔴 CONTROL: the vector must be sensitive to the inputs, or four equal
	// constants would satisfy it.
	mutated := append([]tokenEventDigestRow(nil), goldenTokenEventRows...)
	mutated[0].CostMicro++
	if expectedEventsDigest(mutated) == goldenTokenEventDigest {
		t.Fatal("control: changing a row's cost_micro did not change the golden digest — the vector is not a function of its inputs and pins nothing")
	}
}

// TestDigestGoldenVectorMatchesTheLiveRead ties the frozen constants to the
// actual database read, which is what makes the vector guard the PRODUCTION path
// and not just the in-memory serializer.
//
// Without this arm the golden vector pins appendTokenEventFrames while the SELECT
// could drift underneath it. Measured: swapping `weight, quality` in
// outcomeDigestSelect, and swapping `developer` and `issue_id`, both left the
// entire suite green — a projection that records weight AS quality across every
// published outcomes digest, invisible.
func TestDigestGoldenVectorMatchesTheLiveRead(t *testing.T) {
	db, _ := newDigestDB(t)
	ctx := context.Background()

	// Seed rows whose STORED values equal the golden literals exactly. Every
	// column carries a DISTINCT value so a transposed pair of columns cannot
	// coincide.
	for _, r := range goldenTokenEventRows {
		if _, err := db.db.ExecContext(ctx, `
			INSERT INTO token_events (id, developer, issue_id, model, input_tok, output_tok,
			    cache_read, cache_write_5m, cache_write_1h, cost_micro, source, fidelity,
			    repo, price_version, host, billing_mode, ts)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.ID, r.Developer, r.IssueID, r.Model, r.InputTok, r.OutputTok,
			r.CacheRead, r.CacheWrite5m, r.CacheWrite1h, r.CostMicro, r.Source, r.Fidelity,
			r.Repo, r.PriceVersion, r.Host, r.BillingMode, r.TS); err != nil {
			t.Fatalf("seed token_event %d: %v", r.ID, err)
		}
	}
	for _, r := range goldenOutcomeRows {
		// NULLIF reproduces InsertOutcome's real behaviour: an absent PR number and
		// an absent merge commit are stored as SQL NULL, not as 0 / "". That makes
		// the second golden row exercise the projection's COALESCE rather than
		// merely round-tripping a zero value.
		if _, err := db.db.ExecContext(ctx, `
			INSERT INTO outcomes (id, developer, issue_id, weight, quality, work_type,
			    source, repo, pr_number, merge_commit_sha, ts)
			VALUES (?,?,?,?,?,?,?,?,NULLIF(?,0),NULLIF(?,''),?)`,
			r.ID, r.Developer, r.IssueID, r.Weight, r.Quality, r.WorkType,
			r.Source, r.Repo, r.PRNumber, r.MergeCommitSHA, r.TS); err != nil {
			t.Fatalf("seed outcome %d: %v", r.ID, err)
		}
	}

	// CONTROL: row 2's provenance columns must really be NULL, or the COALESCE
	// path this fixture is built to exercise is never taken.
	var nullPR, nullSHA int
	if err := db.db.QueryRowContext(ctx, `SELECT
		SUM(CASE WHEN pr_number IS NULL THEN 1 ELSE 0 END),
		SUM(CASE WHEN merge_commit_sha IS NULL THEN 1 ELSE 0 END)
		FROM outcomes`).Scan(&nullPR, &nullSHA); err != nil {
		t.Fatalf("count NULL provenance columns: %v", err)
	}
	if nullPR != 1 || nullSHA != 1 {
		t.Fatalf("expected exactly one NULL pr_number and one NULL merge_commit_sha, got %d and %d — the COALESCE arm of the projection is not exercised", nullPR, nullSHA)
	}

	// CONTROL: the seeded ts bytes must round-trip, or the comparison below is
	// against a row set that is not the golden one.
	for _, r := range goldenTokenEventRows {
		var stored string
		if err := db.db.QueryRowContext(ctx,
			`SELECT CAST(ts AS TEXT) FROM token_events WHERE id = ?`, r.ID).Scan(&stored); err != nil {
			t.Fatalf("read back ts: %v", err)
		}
		if stored != r.TS {
			t.Fatalf("stored ts for id %d is %q, want %q — the fixture does not reproduce the golden row, so the assertion below compares the wrong thing", r.ID, stored, r.TS)
		}
	}

	// `repo` is stored as '' on the second golden row; the projection COALESCEs
	// only NULL, so '' survives and the live read must match the literal.
	got := eventsDigest(t, db, FleetWide)
	if got.Value != goldenTokenEventDigest {
		t.Errorf("the LIVE token_events read does not reproduce the golden vector:\n  live   %s\n  golden %s\nThe serializer and the SELECT disagree — most likely two columns are transposed in tokenEventDigestSelect, which no other test in this file can see", got.Value, goldenTokenEventDigest)
	}
	if got.Rows != int64(len(goldenTokenEventRows)) {
		t.Errorf("live read covered %d rows, want %d", got.Rows, len(goldenTokenEventRows))
	}
	if gotOut := outcomesDigest(t, db, FleetWide); gotOut.Value != goldenOutcomeDigest {
		t.Errorf("the LIVE outcomes read does not reproduce the golden vector:\n  live   %s\n  golden %s\nMost likely two columns are transposed in outcomeDigestSelect — e.g. weight recorded as quality", gotOut.Value, goldenOutcomeDigest)
	}
}

// expectedOutcomesDigest is expectedEventsDigest's counterpart.
func expectedOutcomesDigest(rows []outcomeDigestRow) string {
	h := sha256.New()
	var buf bytes.Buffer
	writeLengthPrefixed(&buf, digestDomainOutcomes)
	foldFrames(h, &buf)
	for _, r := range rows {
		appendOutcomeFrames(&buf, r)
		foldFrames(h, &buf)
	}
	return finishDigest(h)
}

// TestDigestWindowBoundsAreHalfOpen pins WHERE the window bound sits, which the
// negative-control test does not.
//
// 🔴 WHY IT IS NEEDED. seedDigestWindow places out-of-window rows days from the
// bounds, so the negative control proves only that a bound exists SOMEWHERE in a
// six-day gap. Measured: flipping tsWindow from `ts >= ? AND ts < ?` to
// `ts > ? AND ts <= ?` left the digest suite entirely green.
//
// ⛔ AND IT CANNOT BE DELEGATED TO window_test.go, WHICH IS THE INTERESTING PART.
// Running that same flip against TestWindowedReads_HalfOpen and
// TestListTokenEvents_WindowHalfOpen: BOTH STILL PASS. Their fixtures put
// equal-cost rows on both edges and assert a SUM, so the flip drops one row and
// gains another and the total is unchanged — a compensating error. A digest is
// strictly more sensitive than any SUM-based read: swapping which row sits at the
// edge leaves a total identical and changes the hash completely. That sensitivity
// is exactly why this surface needs its own boundary fixture.
func TestDigestWindowBoundsAreHalfOpen(t *testing.T) {
	db, _ := newDigestDB(t)
	ctx := context.Background()

	base, err := db.EventsDigest(ctx, digestSince, digestUntil, FleetWide)
	if err != nil {
		t.Fatalf("EventsDigest(empty): %v", err)
	}
	if base.Rows != 0 {
		t.Fatalf("control: the store is not empty (%d rows), so the counts below are not attributable to the boundary rows", base.Rows)
	}

	// A Go-written row at exactly `until` must be EXCLUDED.
	seedDigestEvent(t, db, "on-until", "issue-u", 1, digestUntil, "k-on-until")
	if got := eventsDigest(t, db, FleetWide); got.Rows != 0 {
		t.Errorf("a row at exactly `until` is INCLUDED (rows=%d, want 0) — the upper bound is inclusive, but the window is documented half-open [since, until)", got.Rows)
	}

	// A Go-written row at exactly `since` must be INCLUDED.
	seedDigestEvent(t, db, "on-since", "issue-s", 1, digestSince, "k-on-since")
	if got := eventsDigest(t, db, FleetWide); got.Rows != 1 {
		t.Errorf("a row at exactly `since` is EXCLUDED (rows=%d, want 1) — the lower bound is exclusive, but the window is documented half-open [since, until)", got.Rows)
	}

	// 🔴 THE BARE-ENCODING ARM. A CURRENT_TIMESTAMP row stores no zone suffix, so
	// it is a SHORTER string and sorts before the zone-suffixed bound. Its
	// membership is therefore INVERTED. This pins today's real behaviour — it is
	// tsWindow's, shared with every report, so the digest is CONSISTENT with what
	// gets published. ⚠️ If a future migration normalizes ts encodings, THIS
	// TEST IS THE ONE THAT SHOULD CHANGE, and its failure is the signal that every
	// previously published window may now cover a different row set.
	bare := func(dev, at string) {
		if _, err := db.db.ExecContext(ctx, `
			INSERT INTO token_events (developer, issue_id, model, cost_micro, source,
			    fidelity, repo, price_version, host, billing_mode, ts)
			VALUES (?, 'issue-b', 'm', 1, 'api', 'daily', 'acme/tier', 0, 'unknown',
			        'per_token', ?)`, dev, at); err != nil {
			t.Fatalf("seed bare-encoded row %s: %v", dev, err)
		}
	}
	bare("bare-on-since", "2026-08-01 00:00:00")
	if got := eventsDigest(t, db, FleetWide); got.Rows != 1 {
		t.Errorf("a BARE-encoded row at exactly `since` was included (rows=%d, want 1 — i.e. excluded). Measured behaviour is that it sorts before the zone-suffixed bound and is dropped; if that changed, EventsDigest's doc block and every published window's membership changed with it", got.Rows)
	}
	bare("bare-on-until", "2026-09-01 00:00:00")
	if got := eventsDigest(t, db, FleetWide); got.Rows != 2 {
		t.Errorf("a BARE-encoded row at exactly `until` was excluded (rows=%d, want 2 — i.e. included). Measured behaviour is that it sorts before the zone-suffixed upper bound and is KEPT, making the window effectively (since, until] for that encoding", got.Rows)
	}
}

// TestDigestRejectsAnInvalidWindow pins that a malformed window is an ERROR and
// not the empty-window sentinel.
//
// Measured before the guard existed: `until` before `since` returned
// tierdig1:471f175a… with rows=0 and err=nil — byte-identical to a genuinely
// empty window. A manifest would have published "window W contained no rows" for
// a window that could not contain anything.
func TestDigestRejectsAnInvalidWindow(t *testing.T) {
	db, _ := newDigestDB(t)
	seedDigestWindow(t, db)
	ctx := context.Background()

	for _, c := range []struct {
		name         string
		since, until time.Time
	}{
		{"inverted", digestUntil, digestSince},
		{"zero-width", digestSince, digestSince},
	} {
		got, err := db.EventsDigest(ctx, c.since, c.until, FleetWide)
		if err == nil {
			t.Errorf("EventsDigest(%s) returned %s rows=%d instead of an error — indistinguishable from a genuinely empty window", c.name, got.Value, got.Rows)
		} else if !errors.Is(err, ErrDigestWindowInvalid) {
			t.Errorf("EventsDigest(%s) failed with %v, which does not wrap ErrDigestWindowInvalid; a caller cannot tell a malformed window from a read failure", c.name, err)
		}
		if _, err := db.OutcomesDigest(ctx, c.since, c.until, FleetWide); !errors.Is(err, ErrDigestWindowInvalid) {
			t.Errorf("OutcomesDigest(%s) failed with %v, want ErrDigestWindowInvalid", c.name, err)
		}
	}

	// 🔴 CONTROL: the open-ended form (zero `until`) is tsWindow's documented
	// contract and must still be ACCEPTED, or this guard has broken a real caller.
	open, err := db.EventsDigest(ctx, digestSince, time.Time{}, FleetWide)
	if err != nil {
		t.Fatalf("EventsDigest with a zero `until` (open-ended) was rejected: %v — that is tsWindow's documented open-ended form, not a malformed window", err)
	}
	if open.Rows == 0 {
		t.Error("control: the open-ended window covered no rows, so it did not exercise the accepting path")
	}
}

// TestWindowDigestsReadsBothUnderOneTransaction pins the paired read.
func TestWindowDigestsReadsBothUnderOneTransaction(t *testing.T) {
	db, _ := newDigestDB(t)
	seedDigestWindow(t, db)
	ctx := context.Background()

	ev, out, err := db.WindowDigests(ctx, digestSince, digestUntil, FleetWide)
	if err != nil {
		t.Fatalf("WindowDigests: %v", err)
	}
	// It must agree with the single-table methods on a quiescent store — the pair
	// is about ATOMICITY, not a different computation.
	if want := eventsDigest(t, db, FleetWide); ev.Value != want.Value || ev.Rows != want.Rows {
		t.Errorf("WindowDigests' events digest (%s rows=%d) differs from EventsDigest (%s rows=%d)", ev.Value, ev.Rows, want.Value, want.Rows)
	}
	if want := outcomesDigest(t, db, FleetWide); out.Value != want.Value || out.Rows != want.Rows {
		t.Errorf("WindowDigests' outcomes digest (%s rows=%d) differs from OutcomesDigest (%s rows=%d)", out.Value, out.Rows, want.Value, want.Rows)
	}
	if ev.Value == out.Value {
		t.Error("the two digests are identical, so the domain separation is not applied on this path")
	}

	// The window guard must apply here too, or the paired call is a way around it.
	if _, _, err := db.WindowDigests(ctx, digestUntil, digestSince, FleetWide); !errors.Is(err, ErrDigestWindowInvalid) {
		t.Errorf("WindowDigests with an inverted window returned %v, want ErrDigestWindowInvalid", err)
	}

	// 🔴 CONTROL: the pooled read must survive being called repeatedly. beginRead
	// holds one of maxOpenConns connections and its release func must run exactly
	// once; a leak here exhausts the pool and later calls block rather than fail.
	for i := 0; i < 8; i++ {
		if _, _, err := db.WindowDigests(ctx, digestSince, digestUntil, FleetWide); err != nil {
			t.Fatalf("WindowDigests call %d failed: %v — the read transaction is not being released, so the pool is exhausted", i, err)
		}
	}
}

// TestDigestStreamsRatherThanAccumulating pins the O(1)-memory claim in
// EventsDigest's doc block.
//
// Measured: deleting `buf.Reset()` from foldFrames leaves every digest-vs-digest
// comparison green, because the accumulation is deterministic — expectedEventsDigest
// folds through the same helper, so even the ordering test moves with it. What
// actually changes is that every row re-hashes the whole prefix: on the 172k-row
// window BenchmarkEventsDigest is sized against, that is tens of GB of buffer
// growth for a read documented as O(1) in the window size.
func TestDigestStreamsRatherThanAccumulating(t *testing.T) {
	h := sha256.New()
	buf := bytes.NewBuffer(make([]byte, 0, digestRowBytesHint))
	row := goldenTokenEventRows[0]

	var maxLen int
	for i := 0; i < 500; i++ {
		appendTokenEventFrames(buf, row)
		if buf.Len() > maxLen {
			maxLen = buf.Len()
		}
		foldFrames(h, buf)
		if buf.Len() != 0 {
			t.Fatalf("after folding row %d the buffer still holds %d bytes; foldFrames must empty it, or each row re-hashes every row before it and memory grows with the window", i, buf.Len())
		}
	}
	// One row's frames are ~300 bytes; anything near 500 rows' worth means
	// accumulation. The bound is deliberately loose — this is a shape assertion,
	// not a byte budget.
	if maxLen > 4*digestRowBytesHint {
		t.Errorf("the frame buffer peaked at %d bytes while folding one row at a time; it should never exceed a single row's frames (~%d)", maxLen, digestRowBytesHint)
	}
}

// TestDigestCoversEveryCostBearingColumn is the TABLE→STRUCT census, the
// direction assertEveryFieldIsFramed cannot see.
//
// 🔴 assertEveryFieldIsFramed starts from tokenEventDigestRow and proves every
// field of it reaches the frames. It says nothing about a column that exists in
// SQLite and never made it into the struct — which is the same silent-omission
// class this file rejects idempotency_key for, arriving by a different door. A
// new NOT NULL cost-bearing column added to token_events and forgotten here
// falls outside the digest with the whole suite green.
//
// The exclusion list is the tripwire: adding a column forces a deliberate choice
// between covering it (a wire-format change — bump digestScheme once anything is
// published) and naming it here with a reason.
func TestDigestCoversEveryCostBearingColumn(t *testing.T) {
	db, _ := newDigestDB(t)

	// Columns deliberately outside the digest. Each reason is in eventsdigest.go.
	excluded := map[string]map[string]string{
		"token_events": {
			"idempotency_key":  "nullable + PARTIAL unique index; the whole point of the file header",
			"session_id":       "opaque grouping key; no published figure reads it",
			"billed_to":        "a /costs admission declaration (#854); no spend query reads it",
			"cost_clamped":     "diagnostic clamp history; no spend or scoring query reads it",
			"poller_baseline":  "baseline cutover provenance; no spend or scoring query reads it",
			"attribution_rule": "no published figure or spend query reads it as of #823 S1; the first report that reads it must move it into the digest with a digestScheme bump",
		},
		"outcomes": {
			"weight_source":    "records HOW weight was derived, not the value",
			"work_type_source": "records HOW work_type was derived, not the value",
			"additions":        "raw diff stat retained for future recalibration; read by nothing today",
			"deletions":        "raw diff stat retained for future recalibration; read by nothing today",
			"changed_files":    "raw diff stat retained for future recalibration; read by nothing today",
			"push_day":         "push-dedup key, not part of the outcome's value",
			"author_type":      "decides only whether the author counts toward the k-anonymity floor (#856); moves no figure",
		},
	}
	framed := map[string]map[string]bool{
		"token_events": columnSetFor(tokenEventDigestSelect),
		"outcomes":     columnSetFor(outcomeDigestSelect),
	}

	checked := 0
	for _, table := range []string{"token_events", "outcomes"} {
		rows, err := db.db.QueryContext(context.Background(),
			`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatalf("pragma_table_info(%s): %v", table, err)
		}
		cols := []string{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("scan column: %v", err)
			}
			cols = append(cols, name)
		}
		_ = rows.Close()
		if len(cols) == 0 {
			t.Fatalf("control: pragma_table_info(%s) returned no columns, so this census asserts nothing", table)
		}
		for _, c := range cols {
			checked++
			if framed[table][c] || excluded[table][c] != "" {
				continue
			}
			t.Errorf("%s.%s is neither covered by the digest nor on its exclusion list. A column that is silently outside the identity can be edited without evidence — the exact failure this file rejects idempotency_key for. Either add it to the digest (a WIRE-FORMAT change: extend the row struct and appender, bump the frame count, and bump digestScheme if anything has been published) or add it to `excluded` above with the reason it carries no published meaning", table, c)
		}
	}
	if checked == 0 {
		t.Fatal("control: censused no columns at all")
	}

	// 🔴 CONTROL: the column extractor must actually find columns, or every
	// `framed[...]` lookup is false and the loop would report everything as
	// excluded-or-missing rather than covered.
	if !framed["token_events"]["cost_micro"] || !framed["outcomes"]["weight"] {
		t.Fatal("control: the projection parser did not find cost_micro / weight, so it cannot distinguish a covered column from an omitted one")
	}
	if framed["token_events"]["idempotency_key"] {
		t.Fatal("control: the parser reports idempotency_key as covered, which it must never be")
	}
}

// columnSetFor extracts the bare column names a digest projection reads. It is
// deliberately crude — it just reports which identifiers appear — because its job
// is to answer "is this column mentioned at all", not to parse SQL.
func columnSetFor(sel string) map[string]bool {
	out := map[string]bool{}
	for _, tok := range regexp.MustCompile(`[a-z_][a-z0-9_]*`).FindAllString(sel, -1) {
		out[tok] = true
	}
	// Words that are SQL, not columns, and could mask a real omission.
	for _, kw := range []string{"select", "cast", "as", "text", "coalesce", "from", "where", "unqualified", "feature"} {
		delete(out, kw)
	}
	return out
}

// TestDigestQueriesAreBuiltByTheSharedBuilder closes the gap that asserting on
// digestOrderSQL alone left open.
//
// 🔴 MEASURED: dropping `+digestOrderSQL` from the call site left the ENTIRE
// suite green, TestDigestSQLRequestsItsOrderExplicitly and
// TestDigestReadsAreOneWindowSeek included. The order test pinned the CONSTANT;
// nothing pinned that the constant was still concatenated into the statement that
// runs. And the EQP test cannot see it either — the plans are byte-identical with
// and without the ORDER BY, because the ts predicate selects the same index
// regardless:
//
//	with ORDER BY:    SEARCH token_events USING INDEX idx_token_events_ts_id (ts>? AND ts<?)
//	without ORDER BY: SEARCH token_events USING INDEX idx_token_events_ts_id (ts>? AND ts<?)
//
// So this asserts, in the AST, that every QueryContext in the file is handed a
// CALL to a *DigestQuery builder rather than an inline concatenation.
//
// ⚠️ READS THE SOURCE FROM DISK, cwd-relative. That is correct under `go test`
// (which runs in the package directory) but means this arm is INVISIBLE to
// `go test -overlay`: an overlay mutant of eventsdigest.go is not what
// parser.ParseFile sees, so this test reports on the on-disk file regardless.
// Anyone mutation-testing this file must patch the real bytes, not an overlay,
// or these source-level arms will appear to survive everything.
func TestDigestQueriesAreBuiltByTheSharedBuilder(t *testing.T) {
	// Both builders must carry the order, or routing through them proves nothing.
	for name, q := range map[string]string{
		"tokenEventDigestQuery": tokenEventDigestQuery("ts >= ?"),
		"outcomeDigestQuery":    outcomeDigestQuery("ts >= ?"),
	} {
		if !strings.Contains(q, "ORDER BY ts, id") {
			t.Errorf("%s produced %q, which does not request ORDER BY (ts, id)", name, q)
		}
	}

	file, err := parser.ParseFile(token.NewFileSet(), "eventsdigest.go", nil, 0)
	if err != nil {
		t.Fatalf("parse eventsdigest.go: %v", err)
	}
	found := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "QueryContext" || len(call.Args) < 2 {
			return true
		}
		found++
		inner, ok := call.Args[1].(*ast.CallExpr)
		if !ok {
			t.Errorf("a QueryContext call is passed a query expression that is not a builder call (%T) — most likely an inline concatenation, which can silently drop digestOrderSQL while every test stays green", call.Args[1])
			return true
		}
		id, ok := inner.Fun.(*ast.Ident)
		if !ok || !strings.HasSuffix(id.Name, "DigestQuery") {
			t.Errorf("a QueryContext call is built by %v rather than a *DigestQuery builder", inner.Fun)
		}
		return true
	})
	if found != 2 {
		t.Fatalf("control: found %d QueryContext calls in eventsdigest.go, want 2 — the census missed a read, so a hand-built query could sit here unexamined", found)
	}
}
