package store

import (
	"context"
	"math"
	"testing"
	"time"
)

// TestExportDeveloper_PushOutcomeCommitsCarryPushOrder (#938): the DSAR export
// discloses each ledger entry's ownership sort key — the push time for a keyed
// commit, the maximum for an unkeyed one, and 0 for a pre-ledger marker.
func TestExportDeveloper_PushOutcomeCommitsCarryPushOrder(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	commitTS := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	o := Outcome{Developer: "alice", IssueID: "issue-42", Weight: 0.5, WeightSource: WeightSourcePush,
		Quality: 1, Source: OutcomeSourcePush, Repo: "acme/app", Timestamp: commitTS}
	if _, err := db.UpsertPushOutcome(ctx, o, "2026-09-10"); err != nil {
		t.Fatalf("UpsertPushOutcome: %v", err)
	}
	pushedAt := commitTS.Add(5 * time.Minute)
	if st, err := db.RecordPushCommit(ctx, o, "2026-09-10", "aaaa", pushedAt); err != nil || st != PushCommitRecorded {
		t.Fatalf("RecordPushCommit(aaaa): status=%v err=%v", st, err)
	}
	if st, err := db.RecordPushCommit(ctx, o, "2026-09-10", "bbbb", time.Time{}); err != nil || st != PushCommitRecorded {
		t.Fatalf("RecordPushCommit(bbbb): status=%v err=%v", st, err)
	}
	exp, err := db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatalf("ExportDeveloper: %v", err)
	}
	got := map[string]int64{}
	for _, c := range exp.PushOutcomeCommits {
		got[c.CommitSHA] = c.PushOrder
	}
	want := map[string]int64{"aaaa": pushedAt.Unix(), "bbbb": math.MaxInt64}
	for sha, key := range want {
		if got[sha] != key {
			t.Errorf("export push_order[%s] = %d, want %d", sha, got[sha], key)
		}
	}
	if len(got) != 3 {
		t.Fatalf("export rows = %v, want the marker and two commits", got)
	}
	for sha, key := range got {
		if _, ok := want[sha]; !ok && key != 0 {
			t.Errorf("pre-ledger marker %s push_order = %d, want 0", sha, key)
		}
	}
}
