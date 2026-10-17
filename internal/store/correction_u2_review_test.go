package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/event3"
)

func confirmU2Fixture(t *testing.T, db *DB, rel int64) {
	t.Helper()
	if err := db.ConfirmCorrection(context.Background(), CorrectionCommand{RelationID: rel, Revision: 1, Actor: "operator-login", Role: "operator", Reason: "operator scope", Review: ptrReview(projectionReview())}); err != nil {
		t.Fatal(err)
	}
}

func TestCorrectionU2_R1RetainedParentCannotRebind(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	original, child, rel := projectionFixture(t, db, false)
	confirmU2Fixture(t, db, rel)
	replacement := correctionOutcome(t, db, "mallory", "acme/app", "replacement-sha", 10)
	at := child.MergedAt.Add(-time.Minute)
	replacement.MergedAt = &at
	recordCorrection(t, db, replacement, "Corrects: none", "replacement parent")
	rows, err := db.CorrectionRelationsByOutcome(context.Background(), child.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := rows[0]
	if rowInt(r, "parent_outcome_id") != original.ID || r["parent_developer"] != "original" || r["parent_merge_commit_sha"] != original.MergeCommitSHA || r["state"] != "disputed" || r["resolution_reason"] != "parent-identity-conflict" {
		t.Fatal("retained parent rebound or conflict hidden", r)
	}
	for _, a := range projectionRows(t, db) {
		if rowInt(a, "outcome_id") == original.ID && a["allocated_points"] != "13/4" {
			t.Fatal("original allocation changed", a)
		}
	}
	assertCount(t, db, `SELECT COUNT(*) FROM correction_audit WHERE action='reconciled' AND reason_text='parent-identity-conflict' AND actor_role='system'`, 1)
}

func TestCorrectionU2_R1RecomputesDetachedParent(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	parent, child, _ := projectionFixture(t, db, true)
	ctx := context.Background()
	if err := db.ReconcileCorrectionChain(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	replacement := correctionOutcome(t, db, "original", "acme/app", "new-original", 10)
	at := child.MergedAt.Add(-time.Minute)
	replacement.MergedAt = &at
	recordCorrection(t, db, replacement, "Corrects: none", "later canonical parent")
	for _, a := range projectionRows(t, db) {
		if rowInt(a, "outcome_id") == parent.ID && a["allocated_points"] != "5/1" {
			t.Fatal("detached parent allocation stranded", a)
		}
	}
}

func TestCorrectionU2_R2LateDeclarationKeepsReview(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	parent, child, rel := projectionFixture(t, db, false)
	// Simulate a parent first captured without its declaration.
	if _, err := db.db.Exec(`UPDATE corrective_relations SET parent_declaration_id=NULL; DELETE FROM correction_declarations WHERE id NOT IN (SELECT child_declaration_id FROM corrective_relations)`); err != nil {
		t.Fatal(err)
	}
	confirmU2Fixture(t, db, rel)
	recordCorrection(t, db, parent, "Corrects: none", "late declaration")
	if err := db.ReconcileCorrectionChain(context.Background(), child.ID); err != nil {
		t.Fatal(err)
	}
	for _, r := range projectionRows(t, db) {
		if rowInt(r, "outcome_id") == parent.ID && r["allocated_points"] != "13/4" {
			t.Fatal("reviewed parent points lost", r)
		}
		if rowInt(r, "outcome_id") == child.ID && (r["rule_version"] != event3.ReviewedRule || rowInt(r, "provisional") != 0) {
			t.Fatal("review vanished", r)
		}
	}
}

func TestCorrectionU2_R2LatestReviewCarryForward(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, child, rel := projectionFixture(t, db, false)
	confirmU2Fixture(t, db, rel)
	// An older reconciliation could advance the relation without copying its review.
	if _, err := db.db.Exec(`UPDATE corrective_relations SET revision=3 WHERE id=?`, rel); err != nil {
		t.Fatal(err)
	}
	if err := db.DisputeCorrection(context.Background(), CorrectionCommand{RelationID: rel, Revision: 3, Actor: "fixer", Role: "declarer", Reason: "appeal"}); err != nil {
		t.Fatal(err)
	}
	for _, r := range projectionRows(t, db) {
		if rowInt(r, "outcome_id") == child.ID && r["rule_version"] != event3.ReviewedRule {
			t.Fatal("latest review not carried", r)
		}
	}
}

func TestCorrectionU2_R3SettlementCannotSelfConfirm(t *testing.T) {
	for _, settlement := range []string{"re-assess", "uphold"} {
		t.Run(settlement, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			_, _, rel := projectionFixture(t, db, false)
			rev := int64(1)
			if settlement == "uphold" {
				confirmU2Fixture(t, db, rel)
				rev++
			}
			ctx := context.Background()
			if err := db.UpsertDeveloperAlias(ctx, "fixer-alias", "fixer", "test"); err != nil {
				t.Fatal(err)
			}
			if err := db.DisputeCorrection(ctx, CorrectionCommand{RelationID: rel, Revision: rev, Actor: "fixer", Role: "declarer", Reason: "appeal"}); err != nil {
				t.Fatal(err)
			}
			c := CorrectionCommand{RelationID: rel, Revision: rev + 1, Actor: "fixer-alias", Role: "operator", Reason: "settle", Settlement: settlement}
			if settlement == "re-assess" {
				r := projectionReview()
				r.Revision = c.Revision
				r.Reviewer = c.Actor
				c.Review = &r
			}
			if err := db.SettleCorrection(ctx, c); err == nil {
				t.Fatal("declarer self-confirmed through settlement")
			}
		})
	}
}

func TestCorrectionU2_R4AliasCannotConfirm(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, _, rel := projectionFixture(t, db, false)
	ctx := context.Background()
	if err := db.UpsertDeveloperAlias(ctx, "fixer-alias", "fixer", "test"); err != nil {
		t.Fatal(err)
	}
	r := projectionReview()
	r.Reviewer = "fixer-alias"
	if err := db.ConfirmCorrection(ctx, CorrectionCommand{RelationID: rel, Revision: 1, Actor: r.Reviewer, Role: "operator", Review: &r}); err == nil {
		t.Fatal("alias self-confirmed")
	}
}
func TestCorrectionU2_R4ConfirmRequiresOperator(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, _, rel := projectionFixture(t, db, false)
	if err := db.ConfirmCorrection(context.Background(), CorrectionCommand{RelationID: rel, Revision: 1, Actor: "operator-login", Role: "declarer", Review: ptrReview(projectionReview())}); err == nil {
		t.Fatal("non-operator confirmed")
	}
}
func TestCorrectionU2_R4SettleRequiresOperator(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, _, rel := projectionFixture(t, db, false)
	ctx := context.Background()
	if err := db.DisputeCorrection(ctx, CorrectionCommand{RelationID: rel, Revision: 1, Actor: "fixer", Role: "declarer", Reason: "appeal"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SettleCorrection(ctx, CorrectionCommand{RelationID: rel, Revision: 2, Actor: "operator-login", Role: "declarer", Reason: "settle", Settlement: "remove"}); err == nil {
		t.Fatal("non-operator settled")
	}
}
func TestCorrectionU2_R4UpholdDeclaredDoesNotConfirm(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, child, rel := projectionFixture(t, db, false)
	ctx := context.Background()
	if err := db.DisputeCorrection(ctx, CorrectionCommand{RelationID: rel, Revision: 1, Actor: "fixer", Role: "declarer", Reason: "appeal"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SettleCorrection(ctx, CorrectionCommand{RelationID: rel, Revision: 2, Actor: "operator-login", Role: "operator", Reason: "uphold", Settlement: "uphold"}); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.CorrectionRelationsByOutcome(ctx, child.ID)
	if rows[0]["state"] != "declared" {
		t.Fatal(rows)
	}
}
func TestCorrectionU2_R5WithdrawDisputesRequiresPriorConfirmation(t *testing.T) {
	for _, settlement := range []string{"unconfirmed", "uphold", "remove"} {
		t.Run(settlement, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			_, child, rel := projectionFixture(t, db, true)
			ctx := context.Background()
			rev := int64(1)
			confirmed := settlement != "unconfirmed"
			if confirmed {
				confirmU2Fixture(t, db, rel)
				rev++
			}
			if err := db.DisputeCorrection(ctx, CorrectionCommand{RelationID: rel, Revision: rev, Actor: "original", Role: "declarer", Reason: "appeal"}); err != nil {
				t.Fatal(err)
			}
			before := projectionRows(t, db)
			err := db.WithdrawCorrection(ctx, CorrectionCommand{RelationID: rel, Revision: rev + 1, Actor: "original", Role: "declarer", Reason: "withdraw"})
			if !confirmed {
				if err == nil {
					t.Fatal("withdrew disputed-declared relation")
				}
				if !reflect.DeepEqual(before, projectionRows(t, db)) {
					t.Fatal("withdraw changed disputed provisional allocation")
				}
				rows, err := db.CorrectionRelationsByOutcome(ctx, child.ID)
				if err != nil || len(rows) != 1 || rows[0]["state"] != "disputed-declared" || rowInt(rows[0], "revision") != rev+1 {
					t.Fatal("refused withdrawal changed relation", rows, err)
				}
				return
			}
			if err != nil {
				t.Fatal("withdraw after confirmation", err)
			}
			rows, err := db.CorrectionRelationsByOutcome(ctx, child.ID)
			if err != nil || len(rows) != 1 || rows[0]["state"] != "withdrawn-confirmed" || rowInt(rows[0], "revision") != rev+2 {
				t.Fatal("withdrawal not recorded for settlement", rows, err)
			}
			after := projectionRows(t, db)
			if len(after) != 2 || after[0]["allocated_points"] != "13/4" || after[1]["allocated_points"] != "1/1" {
				t.Fatal("withdraw changed confirmed allocation", after)
			}
			for i := range before {
				for _, key := range []string{"allocated_points", "share", "projected_quality", "ranking_eligibility", "rule_version", "provisional"} {
					if !reflect.DeepEqual(before[i][key], after[i][key]) {
						t.Fatalf("withdraw changed confirmed %s", key)
					}
				}
			}
			cmd := CorrectionCommand{RelationID: rel, Revision: rev + 2, Actor: "original", Role: "declarer", Reason: "settle withdrawal", Settlement: settlement}
			if err := db.SettleCorrection(ctx, cmd); err == nil {
				t.Fatal("settlement accepted without operator")
			}
			cmd.Actor, cmd.Role = "operator-login", "operator"
			if err := db.SettleCorrection(ctx, cmd); err != nil {
				t.Fatal(err)
			}
			want := "13/4"
			if settlement == "remove" {
				want = "5/1"
			}
			settled := projectionRows(t, db)
			if len(settled) != 2 || settled[0]["allocated_points"] != want || settled[1]["allocated_points"] != "1/1" {
				t.Fatal("incorrect settled allocation", settled)
			}
		})
	}
}
func TestCorrectionU2_R5WithdrawRefusesInactive(t *testing.T) {
	for _, state := range []string{"removed", "unrelated", "withdrawn", "withdrawn-confirmed"} {
		t.Run(state, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			_, child, rel := projectionFixture(t, db, false)
			if _, err := db.db.Exec(`UPDATE corrective_relations SET state=? WHERE id=?`, state, rel); err != nil {
				t.Fatal(err)
			}
			if err := db.WithdrawCorrection(context.Background(), CorrectionCommand{RelationID: rel, Revision: 1, Actor: "fixer", Role: "declarer", Reason: "withdraw"}); err == nil {
				t.Fatal("withdrew inactive relation")
			}
			rows, _ := db.CorrectionRelationsByOutcome(context.Background(), child.ID)
			if rows[0]["state"] != state || rows[0]["revision"] != int64(1) {
				t.Fatal(rows)
			}
		})
	}
}
func TestCorrectionU2_R7ReviewerExportIsOwnEvidenceOnly(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, _, rel := projectionFixture(t, db, false)
	confirmU2Fixture(t, db, rel)
	exp, err := db.ExportDeveloper(context.Background(), "operator-login")
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.CorrectionAllocations) != 0 || len(exp.CorrectiveRelations) != 0 || len(exp.CorrectionDeclarations) != 0 {
		t.Fatal("reviewer exported parties' evidence", exp)
	}
	if len(exp.CorrectionAudit) != 1 || exp.CorrectionAudit[0]["actor"] != "operator-login" {
		t.Fatal(exp.CorrectionAudit)
	}
	if len(exp.CorrectionAssessments) != 1 || exp.CorrectionAssessments[0]["reviewer"] != "operator-login" {
		t.Fatal(exp.CorrectionAssessments)
	}
}
func TestCorrectionU2_R7ReviewerErasureDoesNotFreezeParties(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, child, rel := projectionFixture(t, db, false)
	confirmU2Fixture(t, db, rel)
	ctx := context.Background()
	before := projectionRows(t, db)
	if _, err := db.EraseDeveloper(ctx, "operator-login"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, projectionRows(t, db)) {
		t.Fatal("reviewer erasure froze or changed allocations", projectionRows(t, db))
	}
	rows, _ := db.CorrectionRelationsByOutcome(ctx, child.ID)
	if rows[0]["supporting_text"] != "repair scope" {
		t.Fatal("party text scrubbed", rows)
	}
	audit, err := correctionRows(ctx, db.db, `SELECT * FROM correction_audit ORDER BY id`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if audit[0]["actor"] != "fixer" || audit[0]["reason_text"] != "repair scope" || audit[1]["actor"] != nil || audit[1]["actor_role"] != "operator" {
		t.Fatal("other party audit or reviewer nonidentity scrubbed", audit)
	}
	reviews, err := correctionRows(ctx, db.db, `SELECT * FROM correction_assessments WHERE basis='reviewed'`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reviews[0]["reviewer"] != nil || reviews[0]["rationale"] != projectionReview().Rationale {
		t.Fatal("review text scrubbed", reviews)
	}
	if err := db.DisputeCorrection(ctx, CorrectionCommand{RelationID: rel, Revision: 2, Actor: "fixer", Role: "declarer", Reason: "appeal"}); err != nil {
		t.Fatal("reviewer erasure blocked dispute", err)
	}
	if err := db.ReconcileCorrectionChain(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
}
func TestCorrectionU2_Y1CaseVariantCannotConfirm(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, _, rel := projectionFixture(t, db, false)
	ctx := context.Background()
	if err := db.UpsertDeveloperAlias(ctx, "fixer-alias", "fixer", "test"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertDeveloperAlias(ctx, "MixedAlias", "fixer", "test"); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"Fixer", "FIXER-ALIAS", "MixedAlias", "MIXEDALIAS"} {
		r := projectionReview()
		r.Reviewer = actor
		if err := db.ConfirmCorrection(ctx, CorrectionCommand{RelationID: rel, Revision: 1, Actor: actor, Role: "operator", Review: &r}); err == nil {
			t.Fatal("case variant self-confirmed", actor)
		}
	}
}
func TestCorrectionU2_Y4ExcludedParentPrecedesRetainedState(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	parent, child, rel := projectionFixture(t, db, false)
	confirmU2Fixture(t, db, rel)
	if err := db.ExcludePROutcome(context.Background(), parent, "revert", "test"); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.CorrectionRelationsByOutcome(context.Background(), child.ID)
	if rows[0]["state"] != "excluded" || rows[0]["resolution_reason"] != "excluded-parent" {
		t.Fatal(rows)
	}
}
