package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPRNumberAtInsert(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ts := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	for _, o := range []Outcome{
		{Repo: "unqualified", PRNumber: 42, IssueID: "U-1", MergeCommitSHA: "u"},
		{Repo: "acme/a", PRNumber: 42, IssueID: "A-1", MergeCommitSHA: "a"},
		{Repo: "acme/b", PRNumber: 42, IssueID: "B-2", MergeCommitSHA: "b"},
		{Repo: "acme/a", PRNumber: 43, IssueID: "A-3", MergeCommitSHA: "c"},
		{Repo: "acme/a", PRNumber: 43, IssueID: "A-4", MergeCommitSHA: "d"},
		{Repo: "acme/a", PRNumber: 44, IssueID: UnattributedIssueID, MergeCommitSHA: "e"},
	} {
		o.Developer, o.Timestamp, o.Weight, o.Quality = "alice", ts, 1, 1
		if _, err := db.InsertOutcome(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, repo, issue, want string
		pr                      int
		batch                   bool
	}{
		{"known_single", "acme/a", UnattributedIssueID, "A-1", 42, false},
		{"same_number_other_repo", "acme/b", UnattributedNoIssueBucket, "B-2", 42, true},
		{"no_cross_repo", "acme/c", UnattributedIssueID, UnattributedIssueID, 42, false},
		{"unknown", "acme/a", UnattributedIssueID, UnattributedIssueID, 99, true},
		{"unqualified", "unqualified", UnattributedIssueID, UnattributedIssueID, 42, false},
		{"explicit_wins", "acme/a", "PROJ-9", "PROJ-9", 42, true},
		{"ambiguous", "acme/a", UnattributedIssueID, UnattributedIssueID, 43, false},
		{"unattributed_outcome", "acme/a", UnattributedIssueID, UnattributedIssueID, 44, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := ruleEvent(tc.name, AttributionRuleNone)
			e.Timestamp = ts
			e.Repo = tc.repo
			e.IssueID = tc.issue
			e.PRNumber = &tc.pr
			var err error
			if tc.batch {
				err = db.InsertTokenEvents(ctx, []TokenEvent{e})
			} else {
				err = db.InsertTokenEvent(ctx, e)
			}
			if err != nil {
				t.Fatal(err)
			}
			var issue string
			var pr int
			var rule sql.NullString
			if err := db.db.QueryRow(`SELECT issue_id, pr_number, attribution_rule FROM token_events WHERE idempotency_key=?`, tc.name).Scan(&issue, &pr, &rule); err != nil {
				t.Fatal(err)
			}
			wantRule := ""
			if tc.want != tc.issue {
				wantRule = string(AttributionRulePRNumberAtInsert)
			}
			if issue != tc.want || pr != tc.pr || rule.String != wantRule {
				t.Fatalf("got (%s,%d,%v), want (%s,%d,%s)", issue, pr, rule, tc.want, tc.pr, wantRule)
			}
		})
	}

}

func TestPRNumberAtInsertSkipsSealedMonth(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ts := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	_, err := db.db.Exec(`INSERT INTO sealed_report (level,period_size,period_start,period_end,k,config_digest,sealed_at,body,body_digest,tool_version,tool_commit) VALUES ('team','month','2026-05-01T00:00:00Z','2026-06-01T00:00:00Z',2,'x','2026-06-02T00:00:00Z','{}','x','dev','x')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertOutcome(ctx, Outcome{Developer: "alice", Repo: "acme/a", PRNumber: 42, IssueID: "A-1", MergeCommitSHA: "sealed", Timestamp: ts, Weight: 1, Quality: 1}); err != nil {
		t.Fatal(err)
	}
	n := 42
	e := ruleEvent("sealed", AttributionRuleNone)
	e.Timestamp = ts
	e.Repo = "acme/a"
	e.PRNumber = &n
	e.IssueID = UnattributedIssueID
	if err := db.InsertTokenEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	var issue string
	if err := db.db.QueryRow(`SELECT issue_id FROM token_events WHERE idempotency_key='sealed'`).Scan(&issue); err != nil {
		t.Fatal(err)
	}
	if issue != UnattributedIssueID || attributionRuleOf(t, db, "sealed").Valid {
		t.Fatalf("sealed month resolved: %s", issue)
	}
}

// Insert-time PR fill protects the sealed attribution band, including the exact lower
// edge, 1ns before it, and a sub-second offset inside it.
func TestPRNumberAtInsertProtectsSealedLookback(t *testing.T) {
	start, end := sealMonth(6), sealMonth(7)
	low := start.Add(-AttributableWindow)
	for _, batch := range []bool{false, true} {
		mode := "single"
		if batch {
			mode = "bulk"
		}
		for _, tc := range []struct {
			name string
			ts   time.Time
			fill bool
		}{
			{"late_may", time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC), false},
			{"before_band", low.Add(-time.Nanosecond), true},
			{"band_start", low, false},
			{"band_start_frac", low.Add(500 * time.Millisecond), false},
			{"before_end", end.Add(-time.Nanosecond), false},
			{"band_end", end, true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				db, cleanup := newTestDB(t)
				defer cleanup()
				ctx := context.Background()
				if _, err := db.InsertOutcome(ctx, fillOutcome("lookback", start, start.Add(time.Hour))); err != nil {
					t.Fatal(err)
				}
				if _, err := db.db.Exec(`INSERT INTO sealed_report (level,period_size,period_start,period_end,k,config_digest,sealed_at,body,body_digest,tool_version,tool_commit) VALUES ('team','month','2026-06-01T00:00:00Z','2026-07-01T00:00:00Z',2,'x','2026-07-02T00:00:00Z','{}','x','dev','x')`); err != nil {
					t.Fatal(err)
				}
				pr := 42
				e := ruleEvent("lookback", AttributionRuleNone)
				e.Timestamp, e.Repo, e.IssueID, e.PRNumber = tc.ts, "acme/a", UnattributedNoIssueBucket, &pr
				var err error
				if batch {
					err = db.InsertTokenEvents(ctx, []TokenEvent{e})
				} else {
					err = db.InsertTokenEvent(ctx, e)
				}
				if err != nil {
					t.Fatal(err)
				}
				want, rule := e.IssueID, AttributionRuleNone
				if tc.fill {
					want, rule = "issue-42", AttributionRulePRNumberAtInsert
				}
				assertFill(t, db, e.IdempotencyKey, want, rule)
			})
		}
	}
}

// PR evidence on a late insert must preserve the digest of the same unattributed row.
func TestPRNumberAtInsertSealedLookbackDigest(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	start, end := sealMonth(6), sealMonth(7)
	event := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	if _, err := db.InsertOutcome(ctx, fillOutcome("lookback", start, start.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO sealed_report (level,period_size,period_start,period_end,k,config_digest,sealed_at,body,body_digest,tool_version,tool_commit) VALUES ('team','month','2026-06-01T00:00:00Z','2026-07-01T00:00:00Z',2,'x','2026-07-02T00:00:00Z','{}','x','dev','x')`); err != nil {
		t.Fatal(err)
	}
	e := ruleEvent("lookback", AttributionRuleNone)
	e.Timestamp, e.Repo, e.IssueID = event, "acme/a", UnattributedIssueID
	args, err := insertTokenEventArgs(e)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, insertTokenEventSQL, args...); err != nil {
		t.Fatal(err)
	}
	before, _, err := (reader{tx}).ReportDigests(ctx, start, end, FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	pr := 42
	e.PRNumber = &pr
	if err := db.InsertTokenEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	after, _, err := db.ReportDigests(ctx, start, end, FleetWide)
	if err != nil || before != after || after.Rows != 1 {
		t.Fatalf("sealed events digest changed: %v -> %v (%v)", before, after, err)
	}
	assertFill(t, db, "lookback", UnattributedIssueID, AttributionRuleNone)
}

func TestPRNumberMigrationIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tier.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertTokenEvent(context.Background(), ruleEvent("legacy", AttributionRuleNone)); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP INDEX idx_token_events_repo_branch_ts; ALTER TABLE token_events DROP COLUMN pr_number; ALTER TABLE token_events DROP COLUMN branch_hash; DROP INDEX idx_outcomes_repo_pr_number`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	for i := 0; i < 2; i++ {
		db, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var typ string
		var notnull int
		var def, pr sql.NullString
		if err := db.db.QueryRow(`SELECT type,"notnull",dflt_value FROM pragma_table_info('token_events') WHERE name='pr_number'`).Scan(&typ, &notnull, &def); err != nil {
			t.Fatal(err)
		}
		if err := db.db.QueryRow(`SELECT pr_number FROM token_events WHERE idempotency_key='legacy'`).Scan(&pr); err != nil {
			t.Fatal(err)
		}
		if typ != "INTEGER" || notnull != 0 || def.Valid || pr.Valid {
			t.Fatalf("migration: %s %d %v %v", typ, notnull, def, pr)
		}
		var hash sql.NullString
		if err := db.db.QueryRow(`SELECT branch_hash FROM token_events WHERE idempotency_key='legacy'`).Scan(&hash); err != nil || hash.Valid {
			t.Fatalf("branch migration: %v %v", hash, err)
		}
		if err := db.db.QueryRow(`SELECT type,"notnull",dflt_value FROM pragma_table_info('token_events') WHERE name='branch_hash'`).Scan(&typ, &notnull, &def); err != nil || typ != "TEXT" || notnull != 0 || def.Valid {
			t.Fatalf("branch schema: %s %d %v %v", typ, notnull, def, err)
		}
		_ = db.Close()
	}
}

func TestPRResolutionUsesIndex(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	n := 42
	e := ruleEvent("plan", AttributionRuleNone)
	e.PRNumber = &n
	e.Repo = "acme/a"
	e.IssueID = UnattributedIssueID
	args, err := insertTokenEventArgs(e)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.db.Query("EXPLAIN QUERY PLAN "+insertTokenEventSQL, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		found = found || strings.Contains(detail, "USING INDEX idx_outcomes_repo_pr_number")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("resolution did not use repo/PR index")
	}
}
