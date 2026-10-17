package store

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func seedExclusionIdentity(t *testing.T, db *DB) (Outcome, PRExclusionIdentity) {
	t.Helper()
	o := excludedPR()
	if _, err := db.RecordPROutcome(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := db.OutcomeByMergeCommit(context.Background(), o.MergeCommitSHA)
	if err != nil || !ok {
		t.Fatal(stored, ok, err)
	}
	return o, PRExclusionIdentity{o.Repo, o.PRNumber, stored.ID, o.MergeCommitSHA}
}

func TestApplyPRExclusion_StaleIdentityAndErase(t *testing.T) {
	for _, field := range []string{"id", "repo", "pr", "sha", "erased", "other-exclusion"} {
		t.Run(field, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			o, id := seedExclusionIdentity(t, db)
			switch field {
			case "id":
				id.OutcomeID++
			case "repo":
				id.Repo = "acme/different"
			case "pr":
				id.PR++
			case "sha":
				id.MergeSHA = "changed"
			case "erased":
				if _, err := db.EraseDeveloper(ctx, o.Developer); err != nil {
					t.Fatal(err)
				}
			case "other-exclusion":
				if err := db.ExcludePROutcome(ctx, o, "other evidence", "webhook"); err != nil {
					t.Fatal(err)
				}
			}
			before, err := db.ExportDeveloper(ctx, "alice")
			if err != nil {
				t.Fatal(err)
			}
			epoch, err := db.EraseEpoch(ctx)
			if err != nil {
				t.Fatal(err)
			}
			changed, err := db.ApplyPRExclusion(ctx, id, `{"plan_digest":"approved"}`, "operator")
			if err == nil || changed || !strings.Contains(err.Error(), "stale") {
				t.Fatalf("stale=%v %v", changed, err)
			}
			after, err := db.ExportDeveloper(ctx, "alice")
			if err != nil {
				t.Fatal(err)
			}
			endEpoch, err := db.EraseEpoch(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) || epoch != endEpoch {
				t.Fatal("stale plan mutated data")
			}
		})
	}
}

func TestApplyPRExclusion_SealedBytesReplayAndDSAR(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	_, id := seedExclusionIdentity(t, db)
	before, err := db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	july := sealFixture("month", sealMonth(7), sealMonth(8), "cfg-a", "previously sealed bytes")
	if _, _, err := db.SealReport(ctx, july, nil, nil, SealCheck{Floor: testSealFloor}); err != nil {
		t.Fatal(err)
	}
	evidence := `{"plan_digest":"approved","operator_approved_digest":"approved"}`
	for i := 0; i < 2; i++ {
		applied, err := db.ApplyPRExclusion(ctx, id, evidence, "operator")
		if err != nil || applied != (i == 0) {
			t.Fatalf("apply=%v %v", applied, err)
		}
		epoch, err := db.EraseEpoch(ctx)
		if err != nil || epoch != 1 {
			t.Fatalf("epoch=%d %v", epoch, err)
		}
	}
	sealed, err := db.SealedReport(ctx, "month", sealMonth(7))
	if err != nil || !bytes.Equal(sealed.Body, july.Body) {
		t.Fatalf("sealed bytes changed: %+v %v", sealed, err)
	}
	after, err := db.ExportDeveloper(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Outcomes) != 0 || len(after.OutcomeExclusions) != 1 {
		t.Fatal(after)
	}
	x := after.OutcomeExclusions[0]
	if x.ID == nil || *x.ID != before.Outcomes[0].ID || x.Actor != "operator" || x.Evidence != evidence || x.ExcludedAt.IsZero() {
		t.Fatal(x)
	}
	// The historical apply must preserve the same whole-row and privacy contract
	// as insertion-time exclusion, including NULL-valued fields.
	v, w := reflect.ValueOf(x), reflect.ValueOf(before.Outcomes[0])
	for i := 0; i < w.NumField(); i++ {
		name := w.Type().Field(i).Name
		if name != "ID" && !reflect.DeepEqual(v.FieldByName(name).Interface(), w.Field(i).Interface()) {
			t.Errorf("moved column %s changed", name)
		}
	}
	if _, err := db.EraseDeveloper(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if applied, err := db.ApplyPRExclusion(ctx, id, evidence, "operator"); err == nil || applied {
		t.Fatal("replay resurrected erased exclusion")
	}
	assertCount(t, db, `SELECT COUNT(*) FROM outcome_exclusions`, 0)
}

func TestApplyPRExclusion_RollbackWithPushReconciliation(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	o := excludedPR()
	if _, err := db.RecordPushCommit(ctx, o, "2026-07-10", o.MergeCommitSHA, o.Timestamp); err != nil {
		t.Fatal(err)
	}
	// Seed legacy PR data without the normal ingestion-time supersession.
	if _, err := db.InsertOutcome(ctx, o); err != nil {
		t.Fatal(err)
	}
	stored, _, err := db.OutcomeByMergeCommit(ctx, o.MergeCommitSHA)
	if err != nil {
		t.Fatal(err)
	}
	id := PRExclusionIdentity{o.Repo, o.PRNumber, stored.ID, o.MergeCommitSHA}
	if _, err := db.db.Exec(`CREATE TRIGGER refuse_apply BEFORE DELETE ON push_outcome_commits BEGIN SELECT RAISE(ABORT,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if applied, err := db.ApplyPRExclusion(ctx, id, "approved evidence", "operator"); err == nil || applied {
		t.Fatal("failed reconciliation applied")
	}
	assertCount(t, db, `SELECT COUNT(*) FROM outcome_exclusions`, 0)
	assertCount(t, db, `SELECT COUNT(*) FROM outcomes`, 2)
	assertCount(t, db, `SELECT COUNT(*) FROM push_outcome_commits`, 1)
	if _, err := db.db.Exec(`DROP TRIGGER refuse_apply`); err != nil {
		t.Fatal(err)
	}
	if applied, err := db.ApplyPRExclusion(ctx, id, "approved evidence", "operator"); err != nil || !applied {
		t.Fatal(applied, err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM outcomes`, 0)
	assertCount(t, db, `SELECT COUNT(*) FROM outcome_exclusions`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM push_outcome_commits`, 0)
	assertCount(t, db, `SELECT COUNT(*) FROM push_outcome_audit WHERE action='superseded'`, 1)
}

func TestApplyPRExclusion_ReadersMatchInsertionTimeExclusion(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(fmt.Sprint("apply=", apply), func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			o, id := seedExclusionIdentity(t, db)
			keep := o
			keep.PRNumber++
			keep.MergeCommitSHA = "keep"
			keep.IssueID = "issue-control"
			keep.Weight = 1
			keep.Quality = 1
			if _, err := db.RecordPROutcome(ctx, keep); err != nil {
				t.Fatal(err)
			}
			since, until := o.Timestamp.Add(-time.Hour), o.Timestamp.Add(time.Hour)
			before, err := db.AllOutcomesWindow(ctx, since, until, FleetWide)
			if err != nil || len(before) != 2 {
				t.Fatal(before, err)
			}
			if apply {
				if _, err := db.ApplyPRExclusion(ctx, id, `{"plan_digest":"approved"}`, "operator"); err != nil {
					t.Fatal(err)
				}
			} else if err := db.ExcludePROutcome(ctx, o, "head match", "github-webhook"); err != nil {
				t.Fatal(err)
			}
			// These are the current-outcome inputs used by scoring/bootstrap, the
			// developer evidence-floor census, and exports. The ordinary control survives.
			all, err := db.AllOutcomesWindow(ctx, since, until, FleetWide)
			if err != nil || len(all) != 1 || all[0].IssueID != "issue-control" || all[0].Weight*all[0].Quality != 1 {
				t.Fatalf("scoring/bootstrap=%+v %v", all, err)
			}
			dev, err := db.DeveloperOutcomes(ctx, "alice", since)
			if err != nil || len(dev) != 1 || dev[0].IssueID != "issue-control" {
				t.Fatalf("developer floor input=%+v %v", dev, err)
			}
			page, more, err := db.ListOutcomes(ctx, since, until, PageCursor{}, 10)
			if err != nil || more || len(page) != 1 || page[0].MergeCommitSHA != "keep" {
				t.Fatalf("export=%+v %v %v", page, more, err)
			}
			exported, err := db.ExportDeveloper(ctx, "alice")
			if err != nil || len(exported.Outcomes) != 1 || len(exported.OutcomeExclusions) != 1 {
				t.Fatalf("DSAR export=%+v %v", exported, err)
			}
			ids, err := db.PRExclusionCandidates(ctx, "", time.Time{})
			if err != nil || len(ids) != 1 || ids[0].MergeSHA != "keep" {
				t.Fatalf("candidates=%+v %v", ids, err)
			}
		})
	}
}
