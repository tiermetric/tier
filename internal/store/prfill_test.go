package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
)

var fillMerge = time.Date(2026, 6, 10, 12, 0, 0, 123456789, time.UTC)

func fillOutcome(sha string, opened, merged time.Time) Outcome {
	return Outcome{Developer: "alice", Repo: "acme/a", IssueID: "issue-42", PRNumber: 42,
		MergeCommitSHA: sha, Weight: 1, Quality: 1, Timestamp: merged,
		HeadBranchHash: issueref.BranchHash("acme/a", "feature/topic"), CreatedAt: &opened, MergedAt: &merged}
}

func seedFillEvent(t *testing.T, db *DB, key, repo, branch, issue string, ts time.Time, pr *int) {
	t.Helper()
	e := ruleEvent(key, AttributionRuleNone)
	e.Repo, e.IssueID, e.Timestamp, e.PRNumber = repo, issue, ts, pr
	e.BranchHash = issueref.BranchHash(repo, branch)
	if err := db.InsertTokenEvent(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func assertFill(t *testing.T, db *DB, key, issue string, rule AttributionRule) {
	t.Helper()
	var got string
	var r sql.NullString
	if err := db.db.QueryRow(`SELECT issue_id, attribution_rule FROM token_events WHERE idempotency_key=?`, key).Scan(&got, &r); err != nil {
		t.Fatal(err)
	}
	if got != issue || r.String != string(rule) {
		t.Fatalf("%s: got (%s,%v), want (%s,%s)", key, got, r, issue, rule)
	}
}

func TestBranchFillWindowAndExclusions(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	opened := fillMerge.Add(-24 * time.Hour)
	// Literal seven days and nanosecond edges catch changes to the lead-in.
	start := opened.Add(-7 * 24 * time.Hour)
	for _, tc := range []struct {
		key, repo, branch, issue string
		ts                       time.Time
	}{
		{"in", "acme/a", "feature/topic", UnattributedNoIssueBucket, opened},
		{"lead_in", "acme/a", "feature/topic", UnattributedIssueID, start},
		{"before", "acme/a", "feature/topic", UnattributedIssueID, start.Add(-time.Nanosecond)},
		{"merge", "acme/a", "feature/topic", UnattributedIssueID, fillMerge},
		{"after", "acme/a", "feature/topic", UnattributedIssueID, fillMerge.Add(time.Nanosecond)},
		{"repo", "acme/b", "feature/topic", UnattributedIssueID, opened},
		{"branch", "acme/a", "feature/else", UnattributedIssueID, opened},
		{"main", "acme/a", "main", UnattributedIssueID, opened},
		{"owned", "acme/a", "feature/topic", "issue-9", opened},
		{"sealed", "acme/a", "feature/topic", UnattributedIssueID, start},
	} {
		seedFillEvent(t, db, tc.key, tc.repo, tc.branch, tc.issue, tc.ts, nil)
		if tc.key == "sealed" {
			// A seal intersecting only part of June protects the whole month,
			// exactly like insert-time PR resolution.
			if _, err := db.db.Exec(`INSERT INTO sealed_report (level,period_size,period_start,period_end,k,config_digest,sealed_at,body,body_digest,tool_version,tool_commit) VALUES ('team','month','2026-06-01T00:00:00Z','2026-06-02T00:00:00Z',2,'x','2026-06-03T00:00:00Z','{}','x','dev','x')`); err != nil {
				t.Fatal(err)
			}
			// Keep the exclusions test independent of the other June rows.
			if _, err := db.RecordPROutcome(context.Background(), fillOutcome("sealed", opened, fillMerge)); err != nil {
				t.Fatal(err)
			}
			assertFill(t, db, tc.key, tc.issue, AttributionRuleNone)
			if _, err := db.db.Exec(`DELETE FROM sealed_report; DELETE FROM outcomes`); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.RecordPROutcome(context.Background(), fillOutcome("window", opened, fillMerge)); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"in", "lead_in", "merge", "sealed"} {
		assertFill(t, db, key, "issue-42", AttributionRuleBranchAtMerge)
	}
	for _, key := range []string{"before", "after", "repo", "branch", "main"} {
		assertFill(t, db, key, UnattributedIssueID, AttributionRuleNone)
	}
	assertFill(t, db, "owned", "issue-9", AttributionRuleNone)
}

func TestBranchFillPreviousMergeAndAmbiguity(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	first := fillOutcome("first", fillMerge.Add(-time.Hour), fillMerge)
	if _, err := db.RecordPROutcome(ctx, first); err != nil {
		t.Fatal(err)
	}
	// Late events inside the first PR's window must not be taken by PR two.
	seedFillEvent(t, db, "first_window", "acme/a", "feature/topic", UnattributedIssueID, fillMerge.Add(-time.Second), nil)
	// Both inclusive windows cover the previous merge instant: skip it.
	seedFillEvent(t, db, "ambiguous", "acme/a", "feature/topic", UnattributedIssueID, fillMerge, nil)
	seedFillEvent(t, db, "second_window", "acme/a", "feature/topic", UnattributedIssueID, fillMerge.Add(time.Nanosecond), nil)
	second := fillOutcome("second", fillMerge, fillMerge.Add(time.Hour))
	second.PRNumber, second.IssueID = 43, "issue-43"
	if _, err := db.RecordPROutcome(ctx, second); err != nil {
		t.Fatal(err)
	}
	assertFill(t, db, "first_window", UnattributedIssueID, AttributionRuleNone)
	assertFill(t, db, "ambiguous", UnattributedIssueID, AttributionRuleNone)
	assertFill(t, db, "second_window", "issue-43", AttributionRuleBranchAtMerge)
}

func TestPRNumberMergeFillRepoScoped(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	pr := 42
	for _, repo := range []string{"acme/a", "acme/b"} {
		seedFillEvent(t, db, repo, repo, "", UnattributedIssueID, fillMerge.Add(-30*24*time.Hour), &pr)
	}
	seedFillEvent(t, db, "owned", "acme/a", "", "issue-9", fillMerge, &pr)
	o := fillOutcome("pr", fillMerge, fillMerge)
	o.HeadBranchHash, o.CreatedAt, o.MergedAt = nil, nil, nil // API-style evidence
	if _, err := db.RecordPROutcome(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	assertFill(t, db, "acme/a", "issue-42", AttributionRulePRNumberAtInsert)
	assertFill(t, db, "acme/b", UnattributedIssueID, AttributionRuleNone)
	assertFill(t, db, "owned", "issue-9", AttributionRuleNone)
	// A sealed month also excludes the retained PR-number fill.
	if _, err := db.db.Exec(`DELETE FROM outcomes; UPDATE token_events SET issue_id='unattributed', attribution_rule=NULL WHERE idempotency_key='acme/a'; INSERT INTO sealed_report (level,period_size,period_start,period_end,k,config_digest,sealed_at,body,body_digest,tool_version,tool_commit) VALUES ('team','month','2026-05-01T00:00:00Z','2026-06-01T00:00:00Z',2,'x','2026-06-02T00:00:00Z','{}','x','dev','x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordPROutcome(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	assertFill(t, db, "acme/a", UnattributedIssueID, AttributionRuleNone)
}

func TestPRFillTransactionRollback(t *testing.T) {
	for _, mode := range []string{"branch", "pr"} {
		t.Run(mode, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			var pr *int
			if mode == "pr" {
				n := 42
				pr = &n
			}
			seedFillEvent(t, db, "prior", "acme/a", "feature/topic", UnattributedIssueID, fillMerge.Add(-time.Nanosecond), pr)
			seedFillEvent(t, db, "rollback", "acme/a", "feature/topic", UnattributedIssueID, fillMerge, pr)
			// Fail AFTER the fill UPDATE, ensuring both the outcome and UPDATE roll back.
			if _, err := db.db.Exec(`CREATE TRIGGER fail_fill AFTER UPDATE OF issue_id ON token_events WHEN NEW.idempotency_key='rollback' BEGIN SELECT RAISE(ABORT, 'forced fill failure'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.RecordPROutcome(context.Background(), fillOutcome("rollback", fillMerge, fillMerge)); err == nil {
				t.Fatal("expected forced failure")
			}
			var count int
			if err := db.db.QueryRow(`SELECT COUNT(*) FROM outcomes`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("outcomes=%d err=%v", count, err)
			}
			assertFill(t, db, "prior", UnattributedIssueID, AttributionRuleNone)
			assertFill(t, db, "rollback", UnattributedIssueID, AttributionRuleNone)
		})
	}
}

func TestOutcomeBranchMigrationIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tier.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertOutcome(context.Background(), fillOutcome("legacy", fillMerge, fillMerge)); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP INDEX idx_outcomes_repo_branch_merge; DROP INDEX idx_token_events_repo_branch_ts; ALTER TABLE outcomes DROP COLUMN head_branch_hash; ALTER TABLE outcomes DROP COLUMN created_at; ALTER TABLE outcomes DROP COLUMN merged_at`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	for i := 0; i < 2; i++ {
		db, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var hash, opened, merged sql.NullString
		if err := db.db.QueryRow(`SELECT head_branch_hash, created_at, merged_at FROM outcomes`).Scan(&hash, &opened, &merged); err != nil || hash.Valid || opened.Valid || merged.Valid {
			t.Fatalf("legacy metadata: %v %v %v %v", hash, opened, merged, err)
		}
		for _, index := range []string{"idx_outcomes_repo_branch_merge", "idx_token_events_repo_branch_ts"} {
			var columns string
			if err := db.db.QueryRow(`SELECT group_concat(name, ',') FROM (SELECT name FROM pragma_index_info(?) ORDER BY seqno)`, index).Scan(&columns); err != nil {
				t.Fatal(err)
			}
			want := "repo,branch_hash,ts"
			if index == "idx_outcomes_repo_branch_merge" {
				want = "repo,head_branch_hash,merged_at"
			}
			if columns != want {
				t.Fatalf("index %s columns=%s want=%s", index, columns, want)
			}
			var unique int
			if err := db.db.QueryRow(`SELECT "unique" FROM pragma_index_list(?) WHERE name=?`, map[string]string{"idx_outcomes_repo_branch_merge": "outcomes", "idx_token_events_repo_branch_ts": "token_events"}[index], index).Scan(&unique); err != nil || unique != 0 {
				t.Fatalf("index %s: unique=%d err=%v", index, unique, err)
			}
		}
		_ = db.Close()
	}
}

func TestBranchFillEqualMergeWindowsAmbiguous(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	first := fillOutcome("first", fillMerge.Add(-time.Hour), fillMerge)
	if _, err := db.InsertOutcome(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	seedFillEvent(t, db, "overlap", "acme/a", "feature/topic", UnattributedIssueID, fillMerge.Add(-time.Minute), nil)
	second := fillOutcome("second", fillMerge.Add(-time.Hour), fillMerge)
	second.PRNumber, second.IssueID = 43, "issue-43"
	if _, err := db.RecordPROutcome(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	assertFill(t, db, "overlap", UnattributedIssueID, AttributionRuleNone)
}

func TestBranchFillSkipsPRNumberEvidence(t *testing.T) {
	for _, mode := range []string{"another_pr", "ambiguous_pr"} {
		t.Run(mode, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			pr := 99
			if mode == "ambiguous_pr" {
				pr = 42
			}
			seedFillEvent(t, db, "numbered", "acme/a", "feature/topic", UnattributedIssueID, fillMerge.Add(-time.Hour), &pr)
			seedFillEvent(t, db, "branch_only", "acme/a", "feature/topic", UnattributedIssueID, fillMerge.Add(-time.Hour), nil)
			if mode == "ambiguous_pr" {
				conflict := fillOutcome("conflict", fillMerge, fillMerge)
				conflict.IssueID, conflict.HeadBranchHash = "issue-99", nil
				if _, err := db.InsertOutcome(context.Background(), conflict); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.RecordPROutcome(context.Background(), fillOutcome("numbered", fillMerge, fillMerge)); err != nil {
				t.Fatal(err)
			}
			assertFill(t, db, "numbered", UnattributedIssueID, AttributionRuleNone)
			assertFill(t, db, "branch_only", "issue-42", AttributionRuleBranchAtMerge)
		})
	}
}

func TestPRFillProtectsSealedLookback(t *testing.T) {
	for _, mode := range []string{"branch", "pr_number"} {
		t.Run(mode, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			event := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
			var pr *int
			if mode == "pr_number" {
				n := 42
				pr = &n
			}
			seedFillEvent(t, db, "lookback", "acme/a", "feature/topic", UnattributedIssueID, event, pr)
			if pr != nil {
				start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).Add(-AttributableWindow)
				seedFillEvent(t, db, "lookback_start", "acme/a", "feature/topic", UnattributedIssueID, start, pr)
				seedFillEvent(t, db, "before_lookback", "acme/a", "feature/topic", UnattributedIssueID, start.Add(-time.Nanosecond), pr)
				seedFillEvent(t, db, "report_end", "acme/a", "feature/topic", UnattributedIssueID, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), pr)
			}
			if _, err := db.db.Exec(`INSERT INTO sealed_report (level,period_size,period_start,period_end,k,config_digest,sealed_at,body,body_digest,tool_version,tool_commit) VALUES ('team','month','2026-06-01T00:00:00Z','2026-07-01T00:00:00Z',2,'x','2026-07-02T00:00:00Z','{}','x','dev','x')`); err != nil {
				t.Fatal(err)
			}
			before, _, err := db.ReportDigests(context.Background(), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), RepoScope(""))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.RecordPROutcome(context.Background(), fillOutcome("lookback", event, event.Add(time.Hour))); err != nil {
				t.Fatal(err)
			}
			assertFill(t, db, "lookback", UnattributedIssueID, AttributionRuleNone)
			if pr != nil {
				assertFill(t, db, "lookback_start", UnattributedIssueID, AttributionRuleNone)
				assertFill(t, db, "before_lookback", "issue-42", AttributionRulePRNumberAtInsert)
				assertFill(t, db, "report_end", "issue-42", AttributionRulePRNumberAtInsert)
			}
			after, _, err := db.ReportDigests(context.Background(), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), RepoScope(""))
			if err != nil || before != after {
				t.Fatalf("sealed events digest changed: %v -> %v (%v)", before, after, err)
			}
		})
	}
}

func TestBranchFillMissingEvidence(t *testing.T) {
	for _, mode := range []string{"created_at_null", "merged_at_null", "complete"} {
		t.Run(mode, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			seedFillEvent(t, db, "lead_in", "acme/a", "feature/topic", UnattributedIssueID, fillMerge.Add(-time.Hour), nil)
			seedFillEvent(t, db, "merge", "acme/a", "feature/topic", UnattributedIssueID, fillMerge, nil)
			o := fillOutcome("missing", fillMerge, fillMerge)
			switch mode {
			case "created_at_null":
				o.CreatedAt = nil
			case "merged_at_null":
				o.MergedAt = nil
			}
			if _, err := db.RecordPROutcome(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			want, rule := UnattributedIssueID, AttributionRuleNone
			if mode == "complete" {
				want, rule = "issue-42", AttributionRuleBranchAtMerge
			}
			assertFill(t, db, "lead_in", want, rule)
			assertFill(t, db, "merge", want, rule)
		})
	}
}
