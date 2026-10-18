package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

func excludedPR() Outcome {
	return Outcome{Developer: "alice", Repo: "acme/app", IssueID: "issue-42", PRNumber: 43,
		MergeCommitSHA: "revert-merge", Weight: 3, Quality: 0.8, Timestamp: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)}
}

func TestRevertExclusion_DeliveryOrderAndReplay(t *testing.T) {
	for _, order := range []string{"push-first", "PR-first", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			o := excludedPR()
			push := func() error {
				_, err := db.RecordPushCommit(ctx, o, "2026-07-10", o.MergeCommitSHA, o.Timestamp)
				return err
			}
			exclude := func() error { return db.ExcludePROutcome(ctx, o, "head.ref matches ^revert-[0-9]+-", "github-webhook") }
			funcs := []func() error{push, exclude}
			if order == "PR-first" {
				funcs = []func() error{exclude, push}
			}
			if order == "concurrent" {
				start := make(chan struct{})
				errs := make(chan error, 2)
				var wg sync.WaitGroup
				for _, f := range funcs {
					wg.Add(1)
					go func(f func() error) { defer wg.Done(); <-start; errs <- f() }(f)
				}
				close(start)
				wg.Wait()
				close(errs)
				for err := range errs {
					if err != nil {
						t.Fatal(err)
					}
				}
			} else {
				for _, f := range funcs {
					if err := f(); err != nil {
						t.Fatal(err)
					}
				}
			}
			assertCount(t, db, `SELECT COUNT(*) FROM outcomes`, 0)
			assertCount(t, db, `SELECT COUNT(*) FROM push_outcome_commits`, 0)
			assertCount(t, db, `SELECT COUNT(*) FROM outcome_exclusions`, 1)
			epoch, err := db.EraseEpoch(ctx)
			if err != nil {
				t.Fatal(err)
			}
			audit := pushAuditTrail(t, db)
			for i := 0; i < 3; i++ {
				if err := push(); err != nil {
					t.Fatal(err)
				}
				if err := exclude(); err != nil {
					t.Fatal(err)
				}
				for _, write := range []func(context.Context, Outcome) (bool, error){db.RecordPROutcome, db.InsertOutcome} {
					forged := o
					forged.Repo = "elsewhere/repo"
					forged.PRNumber = 99
					if ok, err := write(ctx, forged); err != nil || ok {
						t.Fatalf("reinsert excluded SHA=%v,%v", ok, err)
					}
				}
			}
			after, err := db.EraseEpoch(ctx)
			if err != nil || after != epoch {
				t.Fatalf("replay epoch %d -> %d: %v", epoch, after, err)
			}
			if !reflect.DeepEqual(audit, pushAuditTrail(t, db)) {
				t.Fatal("replay rewrote audit")
			}
			assertCount(t, db, `SELECT COUNT(*) FROM outcomes`, 0)
			if order == "push-first" {
				assertCount(t, db, `SELECT COUNT(*) FROM push_outcome_audit WHERE action='superseded'`, 1)
			}
		})
	}
}

func TestRevertExclusion_MoveWholeRowAndDSAR(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	o := excludedPR()
	o.WeightSource = "label"
	o.Source = OutcomeSourceAPI
	o.WorkType = "security"
	o.WorkTypeSource = "api"
	o.AuthorType = "User"
	hash := "branch-hash"
	o.HeadBranchHash = &hash
	opened := o.Timestamp.Add(-time.Hour)
	o.CreatedAt = &opened
	o.MergedAt = &o.Timestamp
	o.Additions = 31
	o.Deletions = 17
	o.ChangedFiles = 4
	if ok, err := db.RecordPROutcome(ctx, o); err != nil || !ok {
		t.Fatalf("seed: %v %v", ok, err)
	}
	before, err := db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	id := before.Outcomes[0].ID
	if _, err := db.db.Exec(`INSERT INTO quality_events(outcome_id,developer,issue_id,event_type,source_ref,event_ts) VALUES (?,'alice','issue-42','ci_pass','check',?)`, id, normalizeTS(o.Timestamp)); err != nil {
		t.Fatal(err)
	}
	if err := db.ExcludePROutcome(ctx, o, "title matches ^Revert quoted", "backfill"); err != nil {
		t.Fatal(err)
	}
	after, err := db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Outcomes) != 0 || len(after.OutcomeExclusions) != 1 {
		t.Fatalf("export: %+v", after)
	}
	x := after.OutcomeExclusions[0]
	if x.ID == nil || *x.ID != id || x.Reason != "github-revert-pr" || x.Evidence == "" || x.Actor != "backfill" || x.ExcludedAt.IsZero() {
		t.Fatalf("exclusion: %+v", x)
	}
	// Compare every stored outcome column including NULLs with the before-image.
	v, w := reflect.ValueOf(x), reflect.ValueOf(before.Outcomes[0])
	for i := 0; i < w.NumField(); i++ {
		name := w.Type().Field(i).Name
		if name == "ID" {
			continue
		}
		if !reflect.DeepEqual(v.FieldByName(name).Interface(), w.Field(i).Interface()) {
			t.Errorf("moved column %s changed", name)
		}
	}
	assertCount(t, db, `SELECT COUNT(*) FROM quality_events q JOIN outcome_exclusions e ON e.id=q.outcome_id`, 1)
	if !slices.Contains(developerPIITables, "outcome_exclusions") {
		t.Fatal("exclusions escape DSAR registry")
	}
	counts, err := db.EraseDeveloper(ctx, "alice")
	if err != nil || counts["outcome_exclusions"] != 1 {
		t.Fatalf("erase: %v %v", counts, err)
	}
	exp, err := db.ExportDeveloper(ctx, "alice")
	if err != nil || exp.RowCount() != 0 {
		t.Fatalf("post-erase export: %+v %v", exp, err)
	}
}

func TestRevertExclusion_ReconcilesSharedPushOwner(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	o := excludedPR()
	if _, err := db.RecordPushCommit(ctx, o, "2026-07-10", o.MergeCommitSHA, o.Timestamp); err != nil {
		t.Fatal(err)
	}
	other := o
	other.Developer = "bob"
	other.Timestamp = o.Timestamp.Add(time.Hour)
	if _, err := db.RecordPushCommit(ctx, other, "2026-07-10", "ordinary", other.Timestamp); err != nil {
		t.Fatal(err)
	}
	if err := db.ExcludePROutcome(ctx, o, "head match", "github-webhook"); err != nil {
		t.Fatal(err)
	}
	dev, _, n := pushOwner(t, db)
	if n != 1 || dev != "bob" {
		t.Fatalf("remaining owner=%s rows=%d", dev, n)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM push_outcome_commits WHERE commit_sha='ordinary'`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM push_outcome_audit WHERE action='rederived_to' AND developer='bob'`, 1)
}

func TestRevertExclusion_AtomicRollback(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	o := excludedPR()
	if _, err := db.RecordPushCommit(ctx, o, "2026-07-10", o.MergeCommitSHA, o.Timestamp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertOutcome(ctx, o); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`CREATE TRIGGER refuse_reconcile BEFORE DELETE ON push_outcome_commits BEGIN SELECT RAISE(ABORT,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.ExcludePROutcome(ctx, o, "head match", "github-webhook"); err == nil {
		t.Fatal("expected failure")
	}
	assertCount(t, db, `SELECT COUNT(*) FROM outcome_exclusions`, 0)
	assertCount(t, db, `SELECT COUNT(*) FROM outcomes`, 2)
	assertCount(t, db, `SELECT COUNT(*) FROM push_outcome_commits`, 1)
	epoch, err := db.EraseEpoch(ctx)
	if err != nil || epoch != 0 {
		t.Fatalf("rollback epoch=%d %v", epoch, err)
	}
}

func TestRevertExclusion_EraseEpochAndSealedBytes(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	o := excludedPR()
	if _, err := db.RecordPROutcome(ctx, o); err != nil {
		t.Fatal(err)
	}
	before, err := db.EraseEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	july := sealFixture("month", sealMonth(7), sealMonth(8), "cfg-a", "stored July")
	if err := db.ExcludePROutcome(ctx, o, "head match", "github-webhook"); err != nil {
		t.Fatal(err)
	}
	after, err := db.EraseEpoch(ctx)
	if err != nil || after != before+1 {
		t.Fatalf("exclusion epoch=%d %v", after, err)
	}
	if _, _, err := db.SealReport(ctx, july, nil, nil, SealCheck{EraseEpoch: before, Floor: testSealFloor}); !errors.Is(err, ErrSealEraseRaced) {
		t.Fatalf("stale seal=%v", err)
	}
	if _, _, err := db.SealReport(ctx, july, nil, nil, SealCheck{EraseEpoch: after, Floor: testSealFloor}); err != nil {
		t.Fatal(err)
	}
	// Erasure hard-deletes even a no-outcome tombstone owned by the author.
	if _, err := db.EraseDeveloper(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	erased, err := db.EraseEpoch(ctx)
	if err != nil || erased != after+1 {
		t.Fatalf("erase epoch=%d %v", erased, err)
	}
	aug := sealFixture("month", sealMonth(8), sealMonth(9), "cfg-a", "August")
	if _, _, err := db.SealReport(ctx, aug, nil, nil, SealCheck{EraseEpoch: after, Floor: testSealFloor}); !errors.Is(err, ErrSealEraseRaced) {
		t.Fatalf("seal after erase=%v", err)
	}
	sealed, err := db.SealedReport(ctx, "month", sealMonth(7))
	if err != nil || !bytes.Equal(sealed.Body, july.Body) {
		t.Fatalf("sealed bytes=%+v %v", sealed, err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM outcome_exclusions`, 0)
}

func TestRevertExclusion_MigrationV6(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tier.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`DROP TABLE outcome_exclusions; PRAGMA user_version=6`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if readUserVersion(t, path) != schemaVersion || db.UpgradeNotice() != UpgradeNoticeOutcomeExclusions+"\n"+UpgradeNoticeCorrections+"\n"+UpgradeNoticePricingObservations+"\n"+UpgradeNoticeCorrectionProjection+"\n"+UpgradeNoticeQualityTriggers+"\n"+UpgradeNoticeIssueRule {
		t.Fatal("v6 upgrade missing notice/version")
	}
	ctx := context.Background()
	o := excludedPR()
	if err := db.ExcludePROutcome(ctx, o, "head match", "backfill"); err != nil {
		t.Fatal(err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM outcome_exclusions`, 1)
	var cols string
	if err := db.db.QueryRow(`SELECT group_concat(name,',') FROM (SELECT name FROM pragma_table_info('outcome_exclusions') WHERE pk>0 ORDER BY pk)`).Scan(&cols); err != nil || cols != "exclusion_repo,exclusion_pr_number,exclusion_merge_commit_sha" {
		t.Fatalf("tenant-ready key=%s %v", cols, err)
	}
}

func TestRevertExclusion_EraseBlanksOtherAuthorsAuditSHA(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	o := excludedPR()
	pushed := o
	pushed.Developer = "bob"
	if _, err := db.RecordPushCommit(ctx, pushed, "2026-07-10", o.MergeCommitSHA, o.Timestamp); err != nil {
		t.Fatal(err)
	}
	if err := db.ExcludePROutcome(ctx, o, "head match", "github-webhook"); err != nil {
		t.Fatal(err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM push_outcome_audit WHERE developer='bob' AND commit_sha='revert-merge'`, 1)
	if _, err := db.EraseDeveloper(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM push_outcome_audit WHERE developer='bob' AND commit_sha='' AND commit_developer=''`, 1)
}

func TestRevertExclusion_PreservesLegacyNulls(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	o := excludedPR()
	// A legacy row can lack PR metadata. Its original NULLs must survive even
	// though the trusted exclusion has a complete PR/SHA key.
	if _, err := db.db.Exec(`INSERT INTO outcomes(developer,issue_id,weight,quality,repo,merge_commit_sha,ts) VALUES ('alice','issue-42',3,1,'acme/app',?,?)`, o.MergeCommitSHA, normalizeTS(o.Timestamp)); err != nil {
		t.Fatal(err)
	}
	if err := db.ExcludePROutcome(ctx, o, "head match", "backfill"); err != nil {
		t.Fatal(err)
	}
	exp, err := db.ExportDeveloper(ctx, "alice")
	if err != nil || len(exp.OutcomeExclusions) != 1 {
		t.Fatalf("export=%+v %v", exp, err)
	}
	x := exp.OutcomeExclusions[0]
	if x.PRNumber != nil || x.Additions != nil || x.Deletions != nil || x.ChangedFiles != nil || x.CreatedAt != nil || x.MergedAt != nil || x.ID == nil || x.ExclusionPRNumber != 43 || x.ExclusionMergeCommitSHA != o.MergeCommitSHA {
		t.Fatalf("legacy columns rewritten: %+v", x)
	}
}

func TestRevertExclusion_SealedReportRemainsUnchanged(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	o := excludedPR()
	if _, err := db.RecordPROutcome(ctx, o); err != nil {
		t.Fatal(err)
	}
	july := sealFixture("month", sealMonth(7), sealMonth(8), "cfg-a", "already sealed")
	if _, _, err := db.SealReport(ctx, july, nil, nil, SealCheck{Floor: testSealFloor}); err != nil {
		t.Fatal(err)
	}
	if err := db.ExcludePROutcome(ctx, o, "head match", "github-webhook"); err != nil {
		t.Fatal(err)
	}
	sealed, err := db.SealedReport(ctx, "month", sealMonth(7))
	if err != nil || !bytes.Equal(sealed.Body, july.Body) {
		t.Fatalf("excluded sealed bytes changed: %+v %v", sealed, err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM outcomes`, 0)
}

func TestRevertExclusion_RequiresAuditIdentity(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	for _, tc := range []struct {
		number          int
		evidence, actor string
	}{{0, "head match", "backfill"}, {43, "", "backfill"}, {43, "head match", ""}} {
		o := excludedPR()
		o.PRNumber = tc.number
		if err := db.ExcludePROutcome(ctx, o, tc.evidence, tc.actor); err == nil {
			t.Fatal("accepted unaudited exclusion")
		}
	}
	o := excludedPR()
	o.MergeCommitSHA = ""
	if err := db.ExcludePROutcome(ctx, o, "head match", "backfill"); err == nil {
		t.Fatal("accepted exclusion without a merge identity")
	}
	assertCount(t, db, `SELECT COUNT(*) FROM outcome_exclusions`, 0)
}

func TestRevertExclusion_MismatchedAuthorOwnership(t *testing.T) {
	for _, repo := range []string{"acme/app", "unqualified"} {
		for _, eraseDev := range []string{"alice", "bob"} {
			t.Run(repo+"/erase-"+eraseDev, func(t *testing.T) {
				db, cleanup := newTestDB(t)
				defer cleanup()
				ctx := context.Background()
				trusted := excludedPR()
				trusted.AuthorType = AuthorTypeBot
				trusted.Repo = "ACME/APP"
				stored := trusted
				stored.Developer, stored.Repo, stored.Source, stored.AuthorType = "bob", repo, OutcomeSourceAPI, AuthorTypeUser
				if _, err := db.RecordPROutcome(ctx, stored); err != nil {
					t.Fatal(err)
				}
				// The trusted author's merge must also disappear from a co-author's push audit.
				if _, err := db.db.Exec(`INSERT INTO push_outcome_audit(outcome_id,outcome_ts,repo,issue_id,push_day,developer,commit_sha,action,ts) VALUES (0,?2,'acme/app','issue-42','2026-07-10','carol',?1,'superseded',?2)`, trusted.MergeCommitSHA, normalizeTS(trusted.Timestamp)); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					if i == 1 {
						trusted.Repo = "acme/app"
					}
					if err := db.ExcludePROutcome(ctx, trusted, `title matches Revert "original"`, "github-webhook"); err != nil {
						t.Fatal(err)
					}
				}
				for _, dev := range []string{"alice", "bob"} {
					exp, err := db.ExportDeveloper(ctx, dev)
					if err != nil {
						t.Fatal(err)
					}
					if len(exp.OutcomeExclusions) != 1 {
						t.Fatalf("%s export has %d exclusions, want 1", dev, len(exp.OutcomeExclusions))
					}
					x := exp.OutcomeExclusions[0]
					if x.ID == nil || x.Developer != "bob" || x.AuthorDeveloper != "alice" || x.Repo != repo || x.ExclusionRepo != "acme/app" || x.AuthorType == nil || *x.AuthorType != AuthorTypeUser || x.ExclusionAuthorType == nil || *x.ExclusionAuthorType != AuthorTypeBot {
						t.Fatalf("ownership/provenance lost: %+v", x)
					}
				}
				bots, err := db.BotDevelopers(ctx, time.Time{}, time.Time{})
				if err != nil || !reflect.DeepEqual(bots, []string{"alice"}) {
					t.Fatalf("trusted bot=%v %v", bots, err)
				}
				assertCount(t, db, `SELECT COUNT(*) FROM outcome_exclusions`, 1)
				counts, err := db.EraseDeveloper(ctx, eraseDev)
				if err != nil || counts["outcome_exclusions"] != 1 {
					t.Fatalf("erase %s: %v %v", eraseDev, counts, err)
				}
				assertCount(t, db, `SELECT COUNT(*) FROM outcome_exclusions`, 0)
				assertCount(t, db, `SELECT COUNT(*) FROM push_outcome_audit WHERE developer='carol' AND commit_sha=''`, 1)
				for _, dev := range []string{"alice", "bob"} {
					exp, err := db.ExportDeveloper(ctx, dev)
					if err != nil || len(exp.OutcomeExclusions) != 0 {
						t.Fatalf("post-erase %s: %+v %v", dev, exp, err)
					}
				}
			})
		}
	}
}

func TestRevertExclusion_QualityBulkReadsOmitMovedRows(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	o := excludedPR()
	if _, err := db.RecordPROutcome(ctx, o); err != nil {
		t.Fatal(err)
	}
	exp, err := db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	id := exp.Outcomes[0].ID
	for _, n := range []int64{id, id + 100} {
		if _, err := db.db.Exec(`INSERT INTO quality_events(outcome_id,developer,issue_id,event_type,source_ref,event_ts) VALUES (?,'alice','issue-42','ci_pass','check',?)`, n, normalizeTS(o.Timestamp)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.Exec(`INSERT INTO quality_history(outcome_id,developer,issue_id,old_quality,new_quality,reason,ts) VALUES (?,'alice','issue-42',1,0.8,'ci_fail',?)`, n, o.Timestamp.Format(qualityHistoryTSLayout)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.ExcludePROutcome(ctx, o, "head match", "github-webhook"); err != nil {
		t.Fatal(err)
	}
	// Include a NULL-id tombstone to catch NOT IN's NULL trap.
	tombstone := o
	tombstone.PRNumber++
	tombstone.MergeCommitSHA = "other"
	if err := db.ExcludePROutcome(ctx, tombstone, "head match", "github-webhook"); err != nil {
		t.Fatal(err)
	}
	since, until := o.Timestamp.Add(-time.Hour), o.Timestamp.Add(time.Hour)
	events, more, err := db.ListQualityEvents(ctx, since, until, PageCursor{}, 1)
	if err != nil || more || len(events) != 1 || events[0].OutcomeID != id+100 {
		t.Errorf("events=%+v more=%v err=%v", events, more, err)
	}
	history, more, err := db.ListQualityHistory(ctx, since, until, PageCursor{}, 1)
	if err != nil || more || len(history) != 1 || history[0].OutcomeID != id+100 {
		t.Errorf("history=%+v more=%v err=%v", history, more, err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM quality_events`, 2)
	assertCount(t, db, `SELECT COUNT(*) FROM quality_history`, 2)
}

func TestRevertExclusion_MoveConflictRollsBackDeletion(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	o := excludedPR()
	o.IssueID = "" // No-issue exclusion remains an author-owned tombstone.
	if err := db.ExcludePROutcome(ctx, o, "head match", "github-webhook"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO outcomes(developer,issue_id,repo,pr_number,merge_commit_sha,weight,quality,ts) VALUES ('alice','issue-42','acme/app',43,?,1,1,?)`, o.MergeCommitSHA, normalizeTS(o.Timestamp)); err != nil {
		t.Fatal(err)
	}
	if err := db.ExcludePROutcome(ctx, o, "head match", "github-webhook"); err == nil {
		t.Error("move conflict silently deleted outcome")
	}
	assertCount(t, db, `SELECT COUNT(*) FROM outcomes`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM outcome_exclusions WHERE id IS NULL`, 1)
}

func TestRevertExclusion_TokenFillArrivalOrders(t *testing.T) {
	for _, issue := range []string{"issue-42", ""} {
		for _, order := range []string{"outcome-first", "exclusion-first"} {
			t.Run(issue+"/"+order, func(t *testing.T) {
				db, cleanup := newTestDB(t)
				defer cleanup()
				ctx := context.Background()
				opened := fillMerge.Add(-time.Hour)
				o := fillOutcome("revert-merge", opened, fillMerge)
				o.IssueID = issue
				seedFillEvent(t, db, "branch", o.Repo, "feature/topic", UnattributedNoIssueBucket, opened, nil)
				seedFillEvent(t, db, "pr", o.Repo, "feature/topic", UnattributedIssueID, opened, &o.PRNumber)
				// With no derivable issue, the normal ingestion path never inserts.
				if order == "outcome-first" && issue != "" {
					if inserted, err := db.RecordPROutcome(ctx, o); err != nil || !inserted {
						t.Fatalf("record outcome: inserted=%v err=%v", inserted, err)
					}
				}
				if err := db.ExcludePROutcome(ctx, o, "head match", "github-webhook"); err != nil {
					t.Fatal(err)
				}
				if order == "exclusion-first" && issue != "" {
					if inserted, err := db.RecordPROutcome(ctx, o); err != nil || inserted {
						t.Fatalf("excluded outcome reinserted: inserted=%v err=%v", inserted, err)
					}
				}
				if issue == "" {
					assertFill(t, db, "branch", UnattributedNoIssueBucket, AttributionRuleNone)
					assertFill(t, db, "pr", UnattributedIssueID, AttributionRuleNone)
					assertCount(t, db, `SELECT COUNT(*) FROM outcome_exclusions WHERE id IS NULL`, 1)
				} else {
					assertFill(t, db, "branch", issue, AttributionRuleBranchAtMerge)
					assertFill(t, db, "pr", issue, AttributionRulePRNumberAtInsert)
				}
				assertCount(t, db, `SELECT COUNT(*) FROM outcomes`, 0)
				assertCount(t, db, `SELECT COUNT(*) FROM outcome_exclusions`, 1)
			})
		}
	}
}
