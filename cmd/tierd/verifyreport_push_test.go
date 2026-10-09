package main

import (
	"context"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// TestVerifyReport_PushReconciliationIsSeen pins the push_outcome_audit ledger
// (#849) end to end: a manifest taken while a push outcome is in the window,
// then a merged PR that supersedes it, must report the "push reconciliations"
// dimension CHANGED — and print no money figure, because none moved.
func TestVerifyReport_PushReconciliationIsSeen(t *testing.T) {
	f := seedVerifyDB(t)
	ctx := context.Background()
	db, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	inWindow := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	if st, err := db.RecordPushCommit(ctx, store.Outcome{
		Developer: "alice", IssueID: "issue-777", Weight: 0.5, Quality: 1,
		Repo: "acme/tier", Timestamp: inWindow,
	}, "2026-08-10", "sha-squash-777", time.Time{}); err != nil || st != store.PushCommitRecorded {
		t.Fatalf("seed push commit: status=%v err=%v", st, err)
	}
	_ = db.Close()

	f = serveFixtureManifest(t, f, "since="+verifyWindowSince+"&until="+verifyWindowUntil)
	m := readFixtureManifest(t, f.manifestPath)
	if pin := m.ledgers().PushOutcomeAuditCount; pin == nil || *pin != 0 {
		t.Fatalf("manifest push_outcome_audit_count = %v, want 0 — the pin is not on the wire", pin)
	}

	db, err = store.Open(f.dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	// The PR outcome lands AFTER the window, so the only in-window trace of the
	// supersede is the deleted push row and its audit row.
	if _, err := db.RecordPROutcome(ctx, store.Outcome{
		Developer: "alice", IssueID: "issue-777", PRNumber: 777, Weight: 1, Quality: 1,
		MergeCommitSHA: "sha-squash-777", Repo: "acme/tier",
		Timestamp: time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("RecordPROutcome: %v", err)
	}
	_ = db.Close()

	code, out, errb := runVerify(t, f.manifestPath, "--db", f.dbPath)
	if code != rcDiverged {
		t.Fatalf("exit = %d, want %d\nstdout=%s\nstderr=%s", code, rcDiverged, out, errb)
	}
	assertDimStatus(t, out, "push reconciliations", "CHANGED")
	assertDimDetail(t, out, "push reconciliations", "touched 1 row(s) in this window")
	assertDimDetailLacks(t, out, "push reconciliations", "USD")
}
