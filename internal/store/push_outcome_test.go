package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// mkPush builds a source='push' Outcome for the given issue/developer.
func mkPush(issue, dev string, ts time.Time) Outcome {
	return Outcome{
		Developer:    dev,
		IssueID:      issue,
		Weight:       GitHeuristic(0, 0), // 0.5 degraded floor
		WeightSource: WeightSourcePush,
		Quality:      1.0,
		Source:       OutcomeSourcePush,
		Timestamp:    ts,
	}
}

// TestUpsertPushOutcome_IdempotentPerIssuePerDay pins RULING B at the store
// boundary: the FIRST upsert for an (issue, UTC-day) inserts; every later upsert
// for the same (issue, day) is a DO-NOTHING no-op (inserted=false). The issue earns
// exactly ONE 0.5 outcome that day — never 0.5×N — and the row round-trips with the
// 'push' weight_source + source provenance.
func TestUpsertPushOutcome_IdempotentPerIssuePerDay(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ts := time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)
	day := "2026-07-08"

	inserted, err := db.UpsertPushOutcome(ctx, mkPush("issue-42", "alice", ts), day)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if !inserted {
		t.Fatalf("first upsert reported inserted=false, want true")
	}
	// A second commit same issue/day (different author, later time) is a no-op.
	inserted, err = db.UpsertPushOutcome(ctx, mkPush("issue-42", "bob", ts.Add(4*time.Hour)), day)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if inserted {
		t.Fatalf("second same-(issue,day) upsert reported inserted=true, want false (aggregated, not summed)")
	}

	got, found, err := db.LatestOutcomeByIssue(ctx, "", "issue-42")
	if err != nil {
		t.Fatalf("LatestOutcomeByIssue: %v", err)
	}
	if !found {
		t.Fatal("push outcome not found")
	}
	if got.Weight != 0.5 {
		t.Errorf("Weight = %v, want 0.5", got.Weight)
	}
	if got.WeightSource != WeightSourcePush {
		t.Errorf("WeightSource = %q, want %q", got.WeightSource, WeightSourcePush)
	}
	if got.Source != OutcomeSourcePush {
		t.Errorf("Source = %q, want %q", got.Source, OutcomeSourcePush)
	}
	if got.Developer != "alice" {
		t.Errorf("Developer = %q, want alice (first writer wins the aggregated day)", got.Developer)
	}
	if got.MergeCommitSHA != "" {
		t.Errorf("MergeCommitSHA = %q, want empty (push outcomes carry none)", got.MergeCommitSHA)
	}
}

// TestUpsertPushOutcome_DistinctKeysInsert pins that a DIFFERENT issue or a
// DIFFERENT UTC day each produce their own outcome — the aggregation collapses only
// within one (issue, day).
func TestUpsertPushOutcome_DistinctKeysInsert(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "push2.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	base := time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)
	mustUpsert := func(issue, day string, ts time.Time, wantInserted bool) {
		t.Helper()
		ins, err := db.UpsertPushOutcome(ctx, mkPush(issue, "alice", ts), day)
		if err != nil {
			t.Fatalf("upsert %s/%s: %v", issue, day, err)
		}
		if ins != wantInserted {
			t.Fatalf("upsert %s/%s inserted=%v, want %v", issue, day, ins, wantInserted)
		}
	}
	mustUpsert("issue-42", "2026-07-08", base, true)
	mustUpsert("issue-42", "2026-07-09", base.Add(24*time.Hour), true) // next day → new
	mustUpsert("issue-99", "2026-07-08", base, true)                   // other issue → new
	mustUpsert("issue-42", "2026-07-08", base, false)                  // dup → no-op
}

// TestUpsertPushOutcome_CoexistsWithMergeCommitUnique pins the coexistence choice:
// a PR outcome (source='github-webhook', non-null merge_commit_sha) and a push
// outcome (source='push', null merge_commit_sha) for the SAME issue live side by
// side. The two partial unique indexes are disjoint, so neither constrains the
// other.
func TestUpsertPushOutcome_CoexistsWithMergeCommitUnique(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "coexist.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ts := time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)
	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "bob", IssueID: "issue-7", Weight: 3.0,
		WeightSource: WeightSourceLabel, Quality: 1.0,
		MergeCommitSHA: "abc123", Source: OutcomeSourceGitHubWebhook, Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertOutcome (PR path): %v", err)
	}
	inserted, err := db.UpsertPushOutcome(ctx, mkPush("issue-7", "alice", ts), "2026-07-08")
	if err != nil {
		t.Fatalf("UpsertPushOutcome for same issue: %v", err)
	}
	if !inserted {
		t.Fatalf("push outcome for an issue that also has a PR outcome was not inserted")
	}
	// The PR outcome is still findable by its SHA; the push one carries no SHA.
	if _, found, err := db.OutcomeByMergeCommit(ctx, "abc123"); err != nil || !found {
		t.Fatalf("PR outcome lookup by SHA: found=%v err=%v", found, err)
	}
}

// TestUpsertPushOutcome_EmptyDayFails pins the fail-loud guard: an empty UTC day is
// rejected rather than silently defeating the one-per-day unique index (SQLite NULLs
// compare unequal).
func TestUpsertPushOutcome_EmptyDayFails(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "emptyday.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.UpsertPushOutcome(ctx, mkPush("issue-1", "alice", time.Now().UTC()), ""); err == nil {
		t.Fatal("UpsertPushOutcome with empty day returned nil error, want failure")
	}
}

// TestUpsertPushOutcome_CountedByTripwire pins constraint #8: a push-captured
// outcome is a real outcomes row, so the #189 zero-outcome tripwire's
// WindowActivity counts it — no false "zero outcomes recorded" warning once push
// outcomes exist.
func TestUpsertPushOutcome_CountedByTripwire(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "tripwire.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	since := time.Now().UTC().Add(-24 * time.Hour)
	before, err := db.WindowActivity(ctx, since)
	if err != nil {
		t.Fatalf("WindowActivity (before): %v", err)
	}
	if before.Outcomes != 0 {
		t.Fatalf("precondition: Outcomes = %d, want 0", before.Outcomes)
	}
	now := time.Now().UTC()
	if _, err := db.UpsertPushOutcome(ctx, mkPush("issue-42", "alice", now), now.Format("2006-01-02")); err != nil {
		t.Fatalf("UpsertPushOutcome: %v", err)
	}
	after, err := db.WindowActivity(ctx, since)
	if err != nil {
		t.Fatalf("WindowActivity (after): %v", err)
	}
	if after.Outcomes != 1 {
		t.Fatalf("Outcomes = %d, want 1 (push outcome must count toward the tripwire)", after.Outcomes)
	}
}
