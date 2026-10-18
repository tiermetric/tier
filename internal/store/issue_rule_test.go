package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
)

func TestIssueRuleMigration(t *testing.T) {
	for _, version := range []int{10, 11} {
		t.Run(fmt.Sprintf("v%d-to-v12", version), func(t *testing.T) {
			testIssueRuleMigration(t, version)
		})
	}
}

func testIssueRuleMigration(t *testing.T, version int) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	ctx := context.Background()
	var db *DB
	var err error
	if version == 11 {
		restoreMainV11Fixture(t, path)
	} else {
		db, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if db.UpgradeNotice() != "" {
			t.Fatal(db.UpgradeNotice())
		}
		if err = db.InsertTokenEvent(ctx, ruleEvent("old", "")); err != nil {
			t.Fatal(err)
		}
		if _, err = db.InsertOutcome(ctx, Outcome{Developer: "alice", IssueID: "issue-1", Weight: 1, Quality: 1, MergeCommitSHA: "old", Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if _, err = db.db.Exec(`INSERT INTO outcome_exclusions(repo,exclusion_repo,author_developer,exclusion_pr_number,exclusion_merge_commit_sha,developer,ts,reason,evidence,actor,excluded_at) VALUES ('acme/app','acme/app','alice',1,'old-archive','alice',CURRENT_TIMESTAMP,'github-revert-pr','evidence','operator',CURRENT_TIMESTAMP)`); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"outcomes", "token_events", "outcome_exclusions"} {
			if _, err = db.db.Exec(`ALTER TABLE ` + table + ` DROP COLUMN issue_rule`); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = db.db.Exec(`UPDATE outcomes SET id=7; UPDATE token_events SET id=12; DROP TABLE issue_rule_cutover; ALTER TABLE quality_events DROP COLUMN trigger_event; DELETE FROM tier_migrations WHERE name='quality-trigger-recording-v1'`); err != nil {
			t.Fatal(err)
		}
		if err = db.Close(); err != nil {
			t.Fatal(err)
		}
		setUserVersion(t, path, 10)
	}
	started := time.Now().UTC()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	wantNotice := UpgradeNoticeIssueRule
	if version == 10 {
		wantNotice = UpgradeNoticeQualityTriggers + "\n" + wantNotice
	}
	if readUserVersion(t, path) != 12 || schemaVersion != 12 || db.UpgradeNotice() != wantNotice {
		t.Fatalf("version/notice: %d %s", readUserVersion(t, path), db.UpgradeNotice())
	}
	var cutover string
	var outcomeID, eventID, count int64
	if err = db.db.QueryRow(`SELECT recorded_at,outcome_id,token_event_id FROM issue_rule_cutover WHERE id=1`).Scan(&cutover, &outcomeID, &eventID); err != nil {
		t.Fatal(err)
	}
	stamp, err := time.Parse(time.RFC3339Nano, cutover)
	if err != nil || stamp.Location() != time.UTC || stamp.Before(started.Truncate(time.Millisecond)) || stamp.After(time.Now().UTC()) || outcomeID != 7 || eventID != 12 {
		t.Fatalf("cutover=(%q,%d,%d), err=%v", cutover, outcomeID, eventID, err)
	}
	for _, table := range []string{"outcomes", "token_events", "outcome_exclusions"} {
		var rule sql.NullString
		if err = db.db.QueryRow(`SELECT issue_rule FROM ` + table).Scan(&rule); err != nil {
			t.Fatal(err)
		}
		if rule.Valid {
			t.Fatal("historical rule invented", rule)
		}
		var typ string
		var nn int
		var def any
		if err = db.db.QueryRow(`SELECT type, "notnull", dflt_value FROM pragma_table_info(?) WHERE name='issue_rule'`, table).Scan(&typ, &nn, &def); err != nil {
			t.Fatal(err)
		}
		if typ != "TEXT" || nn != 0 || def != nil {
			t.Fatal(typ, nn, def)
		}
	}
	exp, err := db.ExportDeveloper(ctx, "alice")
	if err != nil || len(exp.TokenEvents) != 1 || len(exp.Outcomes) != 1 || exp.TokenEvents[0].IssueRule != nil || exp.Outcomes[0].IssueRule != nil || len(exp.OutcomeExclusions) != 1 || exp.OutcomeExclusions[0].IssueRule != nil {
		t.Fatal(exp, err)
	}
	data, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	var exported map[string]json.RawMessage
	if err := json.Unmarshal(data, &exported); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"token_events", "outcomes", "outcome_exclusions"} {
		var rows []map[string]json.RawMessage
		if err := json.Unmarshal(exported[table], &rows); err != nil || len(rows) != 1 || string(rows[0]["issue_rule"]) != "null" {
			t.Fatalf("%s: %s, err=%v", table, exported[table], err)
		}
	}
	if err := db.InsertTokenEvent(ctx, ruleEvent("post-cutover", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertOutcome(ctx, Outcome{Developer: "alice", IssueID: "issue-2", Weight: 1, Quality: 1, MergeCommitSHA: "post-cutover", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	before := dumpSchema(t, path)
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if db.UpgradeNotice() != "" || dumpSchema(t, path) != before {
		t.Fatal("migration not idempotent")
	}
	var reopenedCutover string
	var reopenedOutcomeID, reopenedEventID int64
	if err := db.db.QueryRow(`SELECT recorded_at,outcome_id,token_event_id FROM issue_rule_cutover WHERE id=1`).Scan(&reopenedCutover, &reopenedOutcomeID, &reopenedEventID); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM issue_rule_cutover`).Scan(&count); err != nil || count != 1 || reopenedCutover != cutover || reopenedOutcomeID != outcomeID || reopenedEventID != eventID {
		t.Fatalf("cutover changed: (%q,%d,%d), count=%d, err=%v", reopenedCutover, reopenedOutcomeID, reopenedEventID, count, err)
	}
	// Version-10 and main-built version-11 binaries both refuse the new header.
	if readUserVersion(t, path) <= version {
		t.Fatal("downgrade not refused")
	}
}
func TestIssueRuleStoredFirstWriterAndDSAR(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ev := ruleEvent("rule", AttributionRuleBranch)
	ev.IssueRule = issueref.RuleBranchFirstNumeric
	if err := db.InsertTokenEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	ev.IssueRule = issueref.RuleBodyBareHash
	if err := db.InsertTokenEvents(ctx, []TokenEvent{ev}); err != nil {
		t.Fatal(err)
	}
	o := Outcome{Developer: "alice", IssueID: "issue-1", Weight: 1, Quality: 1, IssueRule: issueref.RuleBodyCloseDirective, MergeCommitSHA: "rule", Timestamp: ev.Timestamp}
	if _, err := db.InsertOutcome(ctx, o); err != nil {
		t.Fatal(err)
	}
	o.IssueRule = ""
	if _, err := db.InsertOutcome(ctx, o); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO outcome_exclusions(repo,exclusion_repo,author_developer,exclusion_pr_number,exclusion_merge_commit_sha,developer,ts,reason,evidence,actor,excluded_at,issue_rule) VALUES ('acme/app','acme/app','alice',1,'rule-archive','alice',CURRENT_TIMESTAMP,'github-revert-pr','evidence','operator',CURRENT_TIMESTAMP,?)`, string(issueref.RuleBodyBareHash)); err != nil {
		t.Fatal(err)
	}
	exp, err := db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.OutcomeExclusions) != 1 || exp.OutcomeExclusions[0].IssueRule == nil || *exp.OutcomeExclusions[0].IssueRule != "body_bare_hash" || exp.TokenEvents[0].IssueRule == nil || *exp.TokenEvents[0].IssueRule != "branch_first_numeric" || exp.Outcomes[0].IssueRule == nil || *exp.Outcomes[0].IssueRule != "body_close_directive" {
		t.Fatalf("%+v %+v", exp.TokenEvents, exp.Outcomes)
	}
	if _, err := db.EraseDeveloper(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"token_events", "outcomes"} {
		var n int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatal(table, n, err)
		}
	}
}

func TestIssueRuleReplayDoesNotUpgradeNull(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ev := ruleEvent("legacy", "")
	if err := db.InsertTokenEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	ev.IssueRule = issueref.RuleBranchFirstSlot
	if err := db.InsertTokenEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	o := Outcome{Developer: "alice", IssueID: "issue-1", Weight: 1, Quality: 1, MergeCommitSHA: "legacy", Timestamp: ev.Timestamp}
	if _, err := db.InsertOutcome(ctx, o); err != nil {
		t.Fatal(err)
	}
	o.IssueRule = issueref.RuleBodyConfiguredKey
	if _, err := db.InsertOutcome(ctx, o); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"token_events", "outcomes"} {
		var rule sql.NullString
		if err := db.db.QueryRow(`SELECT issue_rule FROM ` + table).Scan(&rule); err != nil {
			t.Fatal(err)
		}
		if rule.Valid {
			t.Fatalf("%s replay upgraded NULL to %q", table, rule.String)
		}
	}
}

func TestIssueRuleCutoverFresh(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	var recordedAt string
	var outcomeID, eventID int64
	if err := db.db.QueryRow(`SELECT recorded_at,outcome_id,token_event_id FROM issue_rule_cutover WHERE id=1`).Scan(&recordedAt, &outcomeID, &eventID); err != nil {
		t.Fatal(err)
	}
	if stamp, err := time.Parse(time.RFC3339Nano, recordedAt); err != nil || stamp.Location() != time.UTC || outcomeID != 0 || eventID != 0 {
		t.Fatalf("fresh cutover=(%q,%d,%d), err=%v", recordedAt, outcomeID, eventID, err)
	}
}

func TestIssueRuleServerAssignments(t *testing.T) {
	for _, path := range []string{"insert", "pr_fill", "branch_fill"} {
		t.Run(path, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			o := fillOutcome("server", fillMerge.Add(-time.Hour), fillMerge)
			e := ruleEvent("client", AttributionRuleBranch)
			e.Repo, e.Timestamp = o.Repo, fillMerge
			e.IssueID, e.IssueRule = UnattributedIssueID, issueref.RuleBranchFirstNumeric
			if path == "branch_fill" {
				e.BranchHash = o.HeadBranchHash
			} else {
				e.PRNumber = &o.PRNumber
			}
			if path == "insert" {
				if _, err := db.RecordPROutcome(ctx, o); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.InsertTokenEvent(ctx, e); err != nil {
				t.Fatal(err)
			}
			if path != "insert" {
				// Retained client assertions can predate the unattributed insert guard.
				if _, err := db.db.Exec(`UPDATE token_events SET issue_rule=? WHERE idempotency_key=?`, string(e.IssueRule), e.IdempotencyKey); err != nil {
					t.Fatal(err)
				}
				if _, err := db.RecordPROutcome(ctx, o); err != nil {
					t.Fatal(err)
				}
			}
			want := AttributionRulePRNumberAtInsert
			if path == "branch_fill" {
				want = AttributionRuleBranchAtMerge
			}
			assertFill(t, db, e.IdempotencyKey, o.IssueID, want)
			var rule sql.NullString
			if err := db.db.QueryRow(`SELECT issue_rule FROM token_events WHERE idempotency_key=?`, e.IdempotencyKey).Scan(&rule); err != nil || rule.Valid {
				t.Fatalf("server assignment retained rule=%v, err=%v", rule, err)
			}
		})
	}
}

func TestIssueRuleUnattributed(t *testing.T) {
	for _, issue := range []string{UnattributedIssueID, UnattributedNoIssueBucket, "unattributed:future", "Unattributed", "unattributedly"} {
		t.Run(issue, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			e := ruleEvent("bucket", AttributionRuleBranch)
			e.IssueID, e.IssueRule = issue, issueref.RuleBranchFirstNumeric
			if err := db.InsertTokenEvents(ctx, []TokenEvent{e}); err != nil {
				t.Fatal(err)
			}
			o := Outcome{Developer: "alice", IssueID: issue, Weight: 1, Quality: 1, Timestamp: e.Timestamp, IssueRule: e.IssueRule}
			if _, err := db.InsertOutcome(ctx, o); err != nil {
				t.Fatal(err)
			}
			if _, err := db.UpsertPushOutcome(ctx, o, "2026-10-01"); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"token_events", "outcomes"} {
				rows, err := db.db.Query(`SELECT issue_id,issue_rule FROM ` + table)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = rows.Close() }()
				for rows.Next() {
					var got string
					var rule sql.NullString
					if err := rows.Scan(&got, &rule); err != nil || got != issue || rule.Valid == IsUnattributed(issue) || (rule.Valid && rule.String != string(e.IssueRule)) {
						t.Fatalf("%s: issue=%q, rule=%v, err=%v", table, got, rule, err)
					}
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
