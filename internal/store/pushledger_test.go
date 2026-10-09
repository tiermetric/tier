package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pushAuditTrail renders push_outcome_audit in id order as
// "action developer outcome_ts commit".
func pushAuditTrail(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.db.Query(`SELECT action, developer, outcome_ts, commit_sha FROM push_outcome_audit ORDER BY id`)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var action, dev, sha string
		var ts time.Time
		if err := rows.Scan(&action, &dev, &ts, &sha); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		out = append(out, action+" "+dev+" "+ts.UTC().Format("15:04")+" "+sha)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	return out
}

func pushOwner(t *testing.T, db *DB) (string, string, int) {
	t.Helper()
	var dev string
	var ts time.Time
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM outcomes WHERE source='push'`).Scan(&n); err != nil {
		t.Fatalf("count push rows: %v", err)
	}
	if n == 0 {
		return "", "", 0
	}
	if err := db.db.QueryRow(`SELECT developer, ts FROM outcomes WHERE source='push'`).Scan(&dev, &ts); err != nil {
		t.Fatalf("read push owner: %v", err)
	}
	return dev, ts.UTC().Format("15:04"), n
}

// TestPushLedger_ReDerivationAndSupersedeAreAudited pins the audit trail #849
// owes: every owner re-derivation as a before/after pair, and a supersede as the
// deleted row's before-image.
func TestPushLedger_ReDerivationAndSupersedeAreAudited(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	day := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	// Each commit is pushed at its own commit time (#938 orders by push time).
	commit := func(dev, sha string, hour int) {
		t.Helper()
		at := day.Add(time.Duration(hour) * time.Hour)
		st, err := db.RecordPushCommit(ctx, Outcome{
			Developer: dev, IssueID: "issue-42", Weight: 0.5, Quality: 1,
			Repo: "acme/app", Timestamp: at,
		}, "2026-09-10", sha, at)
		if err != nil || st != PushCommitRecorded {
			t.Fatalf("RecordPushCommit(%s): status=%v err=%v", sha, st, err)
		}
	}
	pr := func(sha string) {
		t.Helper()
		if ok, err := db.RecordPROutcome(ctx, Outcome{
			Developer: "x", IssueID: "issue-42", PRNumber: 1, Weight: 1, Quality: 1,
			MergeCommitSHA: sha, Repo: "acme/app", Timestamp: day,
		}); err != nil || !ok {
			t.Fatalf("RecordPROutcome(%s): inserted=%v err=%v", sha, ok, err)
		}
	}

	commit("alice", "aaaa", 12)
	commit("bob", "bbbb", 9) // pushed earlier, delivered later: takes the row
	if dev, ts, _ := pushOwner(t, db); dev != "bob" || ts != "09:00" {
		t.Fatalf("owner after an earlier commit = %s@%s, want bob@09:00", dev, ts)
	}
	pr("bbbb") // bob's commit was a PR's merge commit: the row goes back to alice
	if dev, ts, n := pushOwner(t, db); n != 1 || dev != "alice" || ts != "12:00" {
		t.Fatalf("owner after bob's commit left = %s@%s (rows %d), want alice@12:00 (1 row)", dev, ts, n)
	}
	pr("aaaa") // the last commit leaves: the row is superseded
	if _, _, n := pushOwner(t, db); n != 0 {
		t.Fatalf("push rows after the last commit left = %d, want 0", n)
	}

	want := []string{
		"rederived_from alice 12:00 bbbb", "rederived_to bob 09:00 bbbb",
		"rederived_from bob 09:00 bbbb", "rederived_to alice 12:00 bbbb",
		"superseded alice 12:00 aaaa",
	}
	if got := pushAuditTrail(t, db); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit trail:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestEraseDeveloper_BlanksSupersededCommitAuthoredBySubject pins that erasing a
// commit's author blanks that commit's SHA in every surviving audit row, even
// after a PR by another developer superseded it: the ledger entry is gone and
// the merge commit belongs to someone else's outcome, so neither can identify it.
func TestEraseDeveloper_BlanksSupersededCommitAuthoredBySubject(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	day := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		dev, sha string
		hour     int
	}{{"alice", "aaaa", 12}, {"bob", "bbbb", 9}} {
		at := day.Add(time.Duration(c.hour) * time.Hour) // pushed at its commit time
		if st, err := db.RecordPushCommit(ctx, Outcome{
			Developer: c.dev, IssueID: "issue-42", Weight: 0.5, Quality: 1,
			Repo: "acme/app", Timestamp: at,
		}, "2026-09-10", c.sha, at); err != nil || st != PushCommitRecorded {
			t.Fatalf("RecordPushCommit(%s): status=%v err=%v", c.sha, st, err)
		}
	}
	if ok, err := db.RecordPROutcome(ctx, Outcome{
		Developer: "x", IssueID: "issue-42", PRNumber: 1, Weight: 1, Quality: 1,
		MergeCommitSHA: "bbbb", Repo: "acme/app", Timestamp: day,
	}); err != nil || !ok {
		t.Fatalf("RecordPROutcome: inserted=%v err=%v", ok, err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM push_outcome_audit WHERE developer = 'alice' AND commit_sha = 'bbbb'`, 2)

	if _, err := db.EraseDeveloper(ctx, "bob"); err != nil {
		t.Fatalf("EraseDeveloper(bob): %v", err)
	}
	if got := pushAuditTrail(t, db); strings.Join(got, "|") != "rederived_from alice 12:00 |rederived_to alice 12:00 " {
		t.Fatalf("audit trail after erasing bob still names his commit:\n  %s", strings.Join(got, "\n  "))
	}
}

// TestPushLedger_UnusableCommitIDIsRefused: an empty id cannot be deduplicated,
// and an id shaped like a pre-ledger marker must never reach the ledger, where a
// PR with that "merge commit" could remove the marker and delete a legacy row.
func TestPushLedger_UnusableCommitIDIsRefused(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	for _, sha := range []string{"", preLedgerSHAPrefix + "1"} {
		if _, err := db.RecordPushCommit(context.Background(), Outcome{
			Developer: "alice", IssueID: "issue-42", Weight: 0.5, Quality: 1, Repo: "acme/app",
			Timestamp: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
		}, "2026-09-10", sha, time.Time{}); err == nil {
			t.Errorf("RecordPushCommit accepted commit id %q", sha)
		}
	}
	assertCount(t, db, `SELECT COUNT(*) FROM outcomes`, 0)
	assertCount(t, db, `SELECT COUNT(*) FROM push_outcome_commits`, 0)
}

// TestPushLedger_WebhookWritesWaitOutAHeldLock: GitHub does not redeliver a
// failed webhook, so both delivery transactions must wait for a writer holding
// the lock for ~1s — as main's single-statement writes did under the DSN's
// busy_timeout — rather than fail at the request-path cap.
func TestPushLedger_WebhookWritesWaitOutAHeldLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wait.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	ts := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		call func() error
	}{
		{"RecordPushCommit", func() error {
			_, err := db.RecordPushCommit(ctx, Outcome{Developer: "alice", IssueID: "issue-42",
				Weight: 0.5, Quality: 1, Repo: "acme/app", Timestamp: ts}, "2026-09-10", "aaaa", time.Time{})
			return err
		}},
		{"RecordPROutcome", func() error {
			_, err := db.RecordPROutcome(ctx, Outcome{Developer: "alice", IssueID: "issue-42",
				PRNumber: 7, Weight: 1, Quality: 1, MergeCommitSHA: "aaaa", Repo: "acme/app", Timestamp: ts})
			return err
		}},
	} {
		release := holdWriteLock(t, path)
		released := make(chan struct{})
		go func() { time.Sleep(time.Second); release(); close(released) }()
		err := c.call()
		<-released
		if err != nil {
			t.Errorf("%s failed while another writer held the lock for ~1s: %v", c.name, err)
		}
	}
	assertCount(t, db, `SELECT COUNT(*) FROM outcomes`, 1)
}

// TestPushLedger_SupersedeRunsWhenThePROutcomeAlreadyExisted: a PR outcome a
// pre-#849 writer stored first (InsertOutcome, no reconciliation) still removes
// its merge commit from the ledger when the reconciling path sees it again.
func TestPushLedger_SupersedeRunsWhenThePROutcomeAlreadyExisted(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ts := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	if _, err := db.RecordPushCommit(ctx, Outcome{Developer: "alice", IssueID: "issue-42",
		Weight: 0.5, Quality: 1, Repo: "acme/app", Timestamp: ts}, "2026-09-10", "aaaa", time.Time{}); err != nil {
		t.Fatalf("RecordPushCommit: %v", err)
	}
	pr := Outcome{Developer: "alice", IssueID: "issue-42", PRNumber: 7, Weight: 1, Quality: 1,
		MergeCommitSHA: "aaaa", Repo: "acme/app", Timestamp: ts}
	if _, err := db.InsertOutcome(ctx, pr); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}
	if inserted, err := db.RecordPROutcome(ctx, pr); err != nil || inserted {
		t.Fatalf("RecordPROutcome = inserted %v, err %v; want false, nil", inserted, err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM outcomes WHERE source='push'`, 0)
}

// TestPushLedger_SupersedeIsInstallWideLikeTheMergeCommitCheck: merge_commit_sha
// is unique install-wide and a PR outcome's repo can be unqualified (an API post
// without one), so a PR removes its merge commit from the ledger whatever repo
// spelling push capture recorded it under — the same key the push side's
// PR-captured check uses.
func TestPushLedger_SupersedeIsInstallWideLikeTheMergeCommitCheck(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ts := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	if _, err := db.RecordPushCommit(ctx, Outcome{Developer: "alice", IssueID: "issue-42",
		Weight: 0.5, Quality: 1, Repo: "acme/app", Timestamp: ts}, "2026-09-10", "aaaa", time.Time{}); err != nil {
		t.Fatalf("RecordPushCommit: %v", err)
	}
	if inserted, err := db.RecordPROutcome(ctx, Outcome{Developer: "alice", IssueID: "issue-42",
		PRNumber: 7, Weight: 1, Quality: 1, MergeCommitSHA: "aaaa", Timestamp: ts}); err != nil || !inserted {
		t.Fatalf("RecordPROutcome = inserted %v, err %v; want true, nil", inserted, err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM outcomes WHERE source='push'`, 0)
	assertCount(t, db, `SELECT COUNT(*) FROM push_outcome_commits`, 0)
}

// TestPushLedger_RepushLowersOnlyToAStrictlyEarlierPushTime pins the #938
// re-push rule: a commit the ledger holds keeps its EARLIEST push time. A
// strictly earlier usable time lowers the stored key and re-derives the row as
// an audited pair; an identical, later or unusable time, or a stored key 0
// (recorded before #938), writes nothing. Every arm returns PushCommitDuplicate.
func TestPushLedger_RepushLowersOnlyToAStrictlyEarlierPushTime(t *testing.T) {
	day := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	hour := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }
	record := func(t *testing.T, db *DB, dev, sha string, commitHour int, pushedAt time.Time) PushCommitStatus {
		t.Helper()
		st, err := db.RecordPushCommit(context.Background(), Outcome{
			Developer: dev, IssueID: "issue-42", Weight: 0.5, Quality: 1,
			Repo: "acme/app", Timestamp: hour(commitHour),
		}, "2026-09-10", sha, pushedAt)
		if err != nil {
			t.Fatalf("RecordPushCommit(%s): %v", sha, err)
		}
		return st
	}
	snapshot := func(t *testing.T, db *DB) string {
		t.Helper()
		var order int64
		if err := db.db.QueryRow(`SELECT push_order FROM push_outcome_commits WHERE commit_sha = 'aaaa'`).Scan(&order); err != nil {
			t.Fatalf("read push_order: %v", err)
		}
		dev, ts, n := pushOwner(t, db)
		return fmt.Sprintf("aaaa=%d owner=%s@%s rows=%d audit=%q", order, dev, ts, n, pushAuditTrail(t, db))
	}

	t.Run("earlier lowers and re-derives", func(t *testing.T) {
		db, cleanup := newTestDB(t)
		defer cleanup()
		record(t, db, "alice", "aaaa", 14, hour(16))
		record(t, db, "bob", "bbbb", 12, hour(15))
		if dev, _, _ := pushOwner(t, db); dev != "bob" {
			t.Fatalf("setup: owner = %s, want bob", dev)
		}
		if st := record(t, db, "alice", "aaaa", 14, hour(14)); st != PushCommitDuplicate {
			t.Fatalf("status = %v, want PushCommitDuplicate", st)
		}
		got := snapshot(t, db)
		want := fmt.Sprintf("aaaa=%d owner=alice@14:00 rows=1 audit=%q", hour(14).Unix(), []string{
			"rederived_from alice 14:00 bbbb", "rederived_to bob 12:00 bbbb",
			"rederived_from bob 12:00 aaaa", "rederived_to alice 14:00 aaaa",
		})
		if got != want {
			t.Fatalf("after an earlier re-push:\n got  %s\n want %s", got, want)
		}
	})

	for _, tc := range []struct {
		name     string
		pushedAt time.Time
		legacy   bool
	}{
		{"identical", hour(14), false},
		{"later", hour(16), false},
		{"unusable", time.Time{}, false},
		{"legacy key 0", hour(13), true},
	} {
		t.Run(tc.name+" writes nothing", func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			record(t, db, "alice", "aaaa", 14, hour(14))
			record(t, db, "bob", "bbbb", 12, hour(15))
			if tc.legacy {
				if _, err := db.db.Exec(`UPDATE push_outcome_commits SET push_order = 0 WHERE commit_sha = 'aaaa'`); err != nil {
					t.Fatalf("rewind to key 0: %v", err)
				}
			}
			before := snapshot(t, db)
			if st := record(t, db, "alice", "aaaa", 14, tc.pushedAt); st != PushCommitDuplicate {
				t.Fatalf("status = %v, want PushCommitDuplicate", st)
			}
			if after := snapshot(t, db); after != before {
				t.Fatalf("re-push changed the store:\n before %s\n after  %s", before, after)
			}
		})
	}
}
