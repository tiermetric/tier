package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/corrects"
)

func TestCorrectionReview_R4ScopedCost(t *testing.T) {
	var costs []time.Duration
	for _, size := range []int{200, 2000} {
		db, cleanup := newTestDB(t)
		ctx := context.Background()
		_, err := db.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?) INSERT INTO outcomes(developer,issue_id,repo,weight,quality,ts) SELECT 'seed',CAST(x AS TEXT),'acme/seed',1,1,'2026-10-01 00:00:00+00:00' FROM n`, size)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.ConfigureCorrectionScoring(ctx, true); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		for i := 0; i < 16; i++ {
			if _, err = db.InsertOutcome(ctx, Outcome{Developer: "new", IssueID: fmt.Sprint(i), Repo: "acme/new", Weight: 1, Quality: 1, Timestamp: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}); err != nil {
				t.Fatal(err)
			}
		}
		costs = append(costs, time.Since(start))
		cleanup()
	}
	t.Logf("200 outcomes: %v; 2000 outcomes: %v; ratio %.2f", costs[0], costs[1], float64(costs[1])/float64(costs[0]))
	if costs[1] > 4*costs[0] {
		t.Fatal("per-write cost grows with unrelated database size")
	}
}

func TestCorrectionReview_R5FrozenIngest(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	_, child, rel := projectionFixture(t, db, false)
	c := CorrectionCommand{RelationID: rel, Revision: 1, Actor: "operator-login", Role: "operator", Review: ptrReview(projectionReview())}
	if err := db.ConfirmCorrection(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := db.ConfigureCorrectionScoring(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EraseDeveloper(ctx, "original"); err != nil {
		t.Fatal(err)
	}
	var frozenPoints string
	if err := db.db.QueryRow(`SELECT allocated_points FROM correction_allocations WHERE outcome_id=?`, child.ID).Scan(&frozenPoints); err != nil {
		t.Fatal(err)
	}
	for _, declarationFirst := range []bool{false, true} {
		pr := 30
		if declarationFirst {
			pr = 40
		}
		at := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
		o := Outcome{Developer: "carol", Repo: "acme/app", PRNumber: pr, MergeCommitSHA: fmt.Sprint(pr), IssueID: fmt.Sprint(pr), Weight: 3, Quality: 1, Timestamp: at, MergedAt: &at}
		declare := func() {
			t.Helper()
			if _, err := db.RecordCorrectionDeclaration(ctx, o, corrects.Parse("Corrects: #20"), "repair frozen survivor", "test"); err != nil {
				t.Fatal("declaration failed", err)
			}
		}
		if declarationFirst {
			declare()
		}
		if _, err := db.RecordPROutcome(ctx, o); err != nil {
			t.Fatal("ingest failed", err)
		}
		if !declarationFirst {
			declare()
		}
		if _, err := db.CorrectionScoringPolicy(ctx); err != nil {
			t.Fatal("scores refused", err)
		}
		var status, reason, eligible, points string
		if err := db.db.QueryRow(`SELECT a.status,a.pending_reason,a.ranking_eligibility,a.allocated_points FROM correction_allocations a JOIN outcomes o ON o.id=a.outcome_id WHERE o.pr_number=?`, pr).Scan(&status, &reason, &eligible, &points); err != nil {
			t.Fatal(err)
		}
		if status != "pending" || reason != "frozen-chain" || eligible != "ineligible" || points != "3/1" {
			t.Fatal(status, reason, eligible, points)
		}
	}
	var after string
	if err := db.db.QueryRow(`SELECT allocated_points FROM correction_allocations WHERE outcome_id=?`, child.ID).Scan(&after); err != nil || after != frozenPoints {
		t.Fatal("frozen survivor moved", frozenPoints, after, err)
	}
}

func TestCorrectionReview_R7RefreshingWriters(t *testing.T) {
	for _, name := range []string{"InsertOutcome", "UpsertPushOutcome", "RecordPROutcome", "RecordPushCommit", "RecordPushCommitEarlier", "RecordCorrectionDeclaration", "UpdateQuality", "UpdateQualityForOutcome", "UpsertDeveloperAlias", "DeleteDeveloperAlias", "ExcludePROutcome", "ConfirmCorrection", "RecordCorrectionAssessment", "ReconcileCorrectionChain"} {
		t.Run(name, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			original, child, rel := projectionFixture(t, db, true)
			if name == "DeleteDeveloperAlias" {
				if err := db.UpsertDeveloperAlias(ctx, "original", "canonical", "test"); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.ConfigureCorrectionScoring(ctx, true); err != nil {
				t.Fatal(err)
			}
			at := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
			o := Outcome{Developer: "carol", Repo: "acme/app", PRNumber: 30, MergeCommitSHA: "new-outcome", IssueID: "new", Weight: 3, Quality: 1, Timestamp: at, MergedAt: &at}
			var err error
			if name == "RecordCorrectionDeclaration" {
				if _, err = db.RecordPROutcome(ctx, o); err != nil {
					t.Fatal(err)
				}
			}
			switch name {
			case "InsertOutcome":
				_, err = db.InsertOutcome(ctx, o)
			case "UpsertPushOutcome":
				_, err = db.UpsertPushOutcome(ctx, o, "2026-10-04")
			case "RecordPROutcome":
				_, err = db.RecordPROutcome(ctx, o)
			case "RecordPushCommit", "RecordPushCommitEarlier":
				_, err = db.RecordPushCommit(ctx, o, "2026-10-04", "push-new", at)
				if err == nil && name == "RecordPushCommitEarlier" {
					o.Developer = "dave"
					_, err = db.RecordPushCommit(ctx, o, "2026-10-04", "push-other", at.Add(time.Hour))
					if err == nil {
						_, err = db.RecordPushCommit(ctx, o, "2026-10-04", "push-other", at.Add(-time.Hour))
					}
				}
			case "RecordCorrectionDeclaration":
				_, err = db.RecordCorrectionDeclaration(ctx, o, corrects.Parse("Corrects: #10"), "new evidence", "test")
			case "UpdateQuality":
				err = db.UpdateQuality(ctx, "original", original.IssueID, 0.7)
			case "UpdateQualityForOutcome":
				err = db.UpdateQualityForOutcome(ctx, child.ID, 0.7, "ci_fail", "head")
			case "UpsertDeveloperAlias":
				err = db.UpsertDeveloperAlias(ctx, "original", "canonical", "test")
			case "DeleteDeveloperAlias":
				_, err = db.DeleteDeveloperAlias(ctx, "original", "test")
			case "ExcludePROutcome":
				err = db.ExcludePROutcome(ctx, original, "excluded", "operator")
			case "ConfirmCorrection":
				err = db.ConfirmCorrection(ctx, CorrectionCommand{RelationID: rel, Revision: 1, Actor: "operator-login", Role: "operator", Review: ptrReview(projectionReview())})
			case "RecordCorrectionAssessment":
				err = db.RecordCorrectionAssessment(ctx, rel, 1, projectionReview())
			case "ReconcileCorrectionChain":
				if _, err = db.db.Exec(`UPDATE outcomes SET quality=0.7 WHERE id=?`, child.ID); err == nil {
					err = db.ReconcileCorrectionChain(ctx, child.ID)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.CorrectionScoringPolicy(ctx); err != nil {
				t.Fatal("writer left policy unresolved", err)
			}
			var missing int
			if err = db.db.QueryRow(`SELECT COUNT(*) FROM outcomes o LEFT JOIN correction_allocations a ON a.outcome_id=o.id WHERE a.id IS NULL`).Scan(&missing); err != nil || missing != 0 {
				t.Fatal("writer left outcomes unallocated", missing, err)
			}
		})
	}
}
