package store

import (
	"context"
	"fmt"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/event3"
)

func TestCorrectionLifecycle_AuditedIdempotentTransitions(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, child, rel := projectionFixture(t, db, false)
	ctx := context.Background()
	cmd := CorrectionCommand{RelationID: rel, Revision: 1, Actor: "operator-login", Role: "operator", Reason: "reviewed scope", Review: ptrReview(projectionReview())}
	if err := db.ConfirmCorrection(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	if err := db.ConfirmCorrection(ctx, cmd); err != nil {
		t.Fatal("replay", err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM correction_audit WHERE action='confirm'`, 1)
	before := projectionRows(t, db)
	cmd.Revision = 2
	cmd.Review = nil
	cmd.Actor = "fixer"
	cmd.Role = "declarer"
	cmd.Reason = "appeal"
	if err := db.DisputeCorrection(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	after := projectionRows(t, db)
	for i := range before {
		for _, key := range []string{"allocated_points", "share", "projected_quality", "ranking_eligibility"} {
			if !reflect.DeepEqual(before[i][key], after[i][key]) {
				t.Fatalf("dispute moved %s", key)
			}
		}
	}
	cmd.Revision = 3
	cmd.Actor = "operator-login"
	cmd.Role = "operator"
	cmd.Settlement = "remove"
	if err := db.SettleCorrection(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	rows := projectionRows(t, db)
	if rows[0]["allocated_points"] != "5/1" || rows[1]["allocated_points"] != "1/1" {
		t.Fatal(rows)
	}
	if err := db.ReconcileCorrectionChain(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
}
func TestCorrectionLifecycle_ValidationAndStaleRevision(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, _, rel := projectionFixture(t, db, false)
	cmd := CorrectionCommand{RelationID: rel, Revision: 1, Actor: "fixer", Role: "operator", Review: ptrReview(projectionReview())}
	if err := db.ConfirmCorrection(context.Background(), cmd); err == nil {
		t.Fatal("self confirmation")
	}
	cmd.Actor = "operator-login"
	cmd.Review = nil
	if err := db.ConfirmCorrection(context.Background(), cmd); err == nil {
		t.Fatal("unreviewed confirmation")
	}
	cmd.Review = ptrReview(projectionReview())
	cmd.Revision = 9
	if err := db.ConfirmCorrection(context.Background(), cmd); err == nil {
		t.Fatal("stale confirmation")
	}
	assertCount(t, db, `SELECT COUNT(*) FROM correction_assessments WHERE basis='reviewed'`, 0)
	cmd.Revision = 1
	cmd.Review = nil
	cmd.Reason = ""
	if err := db.WithdrawCorrection(context.Background(), cmd); err == nil {
		t.Fatal("reasonless withdrawal")
	}
}

func ptrReview(r event3.Review) *event3.Review { return &r }

func TestCorrectionLifecycle_UnconfirmedDisputeRetainsProvisional(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, child, rel := projectionFixture(t, db, true)
	ctx := context.Background()
	if err := db.ReconcileCorrectionChain(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	before := projectionRows(t, db)
	if err := db.DisputeCorrection(ctx, CorrectionCommand{RelationID: rel, Revision: 1, Actor: "original", Role: "declarer", Reason: "scope appeal"}); err != nil {
		t.Fatal(err)
	}
	after := projectionRows(t, db)
	if after[1]["disputed"] != int64(1) || before[0]["allocated_points"] != after[0]["allocated_points"] || before[1]["allocated_points"] != after[1]["allocated_points"] {
		t.Fatal(after)
	}
}
func TestCorrectionWithdraw_RecomputesOnlyBeforeConfirmation(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprint(confirmed), func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			_, child, rel := projectionFixture(t, db, true)
			ctx := context.Background()
			revision := int64(1)
			if confirmed {
				c := CorrectionCommand{RelationID: rel, Revision: revision, Actor: "operator-login", Role: "operator", Review: ptrReview(projectionReview())}
				if err := db.ConfirmCorrection(ctx, c); err != nil {
					t.Fatal(err)
				}
				revision++
			} else {
				if err := db.ReconcileCorrectionChain(ctx, child.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.WithdrawCorrection(ctx, CorrectionCommand{RelationID: rel, Revision: revision, Actor: "original", Role: "declarer", Reason: "withdraw declaration"}); err != nil {
				t.Fatal(err)
			}
			rows := projectionRows(t, db)
			want := "5/1"
			if confirmed {
				want = "13/4"
			}
			if rows[0]["allocated_points"] != want {
				t.Fatal(rows)
			}
		})
	}
}
func TestCorrectionSettlement_RejectsStaleRevisionAndReassesses(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, _, rel := projectionFixture(t, db, false)
	ctx := context.Background()
	c := CorrectionCommand{RelationID: rel, Revision: 1, Actor: "operator-login", Role: "operator", Review: ptrReview(projectionReview())}
	if err := db.ConfirmCorrection(ctx, c); err != nil {
		t.Fatal(err)
	}
	c.Review = nil
	c.Revision = 2
	c.Reason = "appeal"
	if err := db.DisputeCorrection(ctx, c); err != nil {
		t.Fatal(err)
	}
	c.Settlement = "remove"
	if err := db.SettleCorrection(ctx, c); err == nil {
		t.Fatal("stale settlement accepted")
	}
	c.Revision = 3
	c.Settlement = "re-assess"
	review := projectionReview()
	review.Revision = 3
	review.Own = projectionRat("3")
	c.Review = &review
	if err := db.SettleCorrection(ctx, c); err != nil {
		t.Fatal(err)
	}
	rows := projectionRows(t, db)
	if rows[0]["allocated_points"] != "5/4" || rows[1]["allocated_points"] != "3/1" {
		t.Fatal(rows)
	}
	if err := db.SettleCorrection(ctx, c); err != nil {
		t.Fatal("replay", err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM correction_assessments WHERE basis='reviewed'`, 3)
	var enabled int
	if err := db.db.QueryRow(`SELECT scoring_enabled FROM correction_policy`).Scan(&enabled); err != nil || enabled != 0 {
		t.Fatal("command enabled scoring", enabled, err)
	}
}
func TestCorrectionReaders_StoreWindowAndFrozenSurvivor(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, child, _ := projectionFixture(t, db, true)
	ctx := context.Background()
	if err := db.ReconcileCorrectionChain(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := db.DeveloperOutcomes(ctx, "original", time.Time{})
	if err != nil || len(rows) != 2 || rows[1].Projection == nil || rows[1].Projection.RankingEligibility != "ineligible" {
		t.Fatal(rows, err)
	}
	// The same projection must be read through a snapshot and bulk export.
	if err := db.ReadSnapshot(ctx, func(s *Snapshot) error {
		out, e := s.AllOutcomesWindow(ctx, time.Time{}, time.Time{}, FleetWide)
		if e != nil {
			return e
		}
		if len(out) != 2 || out[0].Projection.AllocatedPoints != 3.25 {
			t.Fatal(out)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out, _, err := db.ListOutcomes(ctx, time.Time{}, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), PageCursor{}, 100)
	if err != nil || len(out) != 2 || out[1].Projection.AllocatedPoints != 1 {
		t.Fatal(out, err)
	}
}
func TestCorrectionLifecycle_DisputedPendingParentStillResolvesEndpoints(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	child := correctionOutcome(t, db, "fixer", "acme/app", "child-sha", 20)
	recordCorrection(t, db, child, "Corrects: #10", "missing parent scope")
	rels, err := db.CorrectionRelationsByOutcome(ctx, child.ID)
	if err != nil || len(rels) != 1 {
		t.Fatal(rels, err)
	}
	id := rowInt(rels[0], "id")
	revision := rowInt(rels[0], "revision")
	if err := db.DisputeCorrection(ctx, CorrectionCommand{RelationID: id, Revision: revision, Actor: "fixer", Role: "declarer", Reason: "review parent"}); err != nil {
		t.Fatal(err)
	}
	pending := projectionRows(t, db)
	if len(pending) != 1 || pending[0]["disputed"] != int64(1) {
		t.Fatal("pending dispute hidden", pending)
	}
	parent := correctionOutcome(t, db, "original", "acme/app", "parent-sha", 10)
	if _, err := db.RecordPROutcome(ctx, parent); err != nil {
		t.Fatal(err)
	}
	rels, err = db.CorrectionRelationsByOutcome(ctx, child.ID)
	if err != nil || rowInt(rels[0], "parent_outcome_id") != parent.ID || rowString(rels[0], "state") != "disputed-declared" {
		t.Fatal(rels, err)
	}
}

func TestCorrectionReaders_FrozenSurvivorKeepsStoredAllocation(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, _, rel := projectionFixture(t, db, false)
	ctx := context.Background()
	c := CorrectionCommand{RelationID: rel, Revision: 1, Actor: "operator-login", Role: "operator", Review: ptrReview(projectionReview())}
	if err := db.ConfirmCorrection(ctx, c); err != nil {
		t.Fatal(err)
	}
	before, err := db.DeveloperOutcomes(ctx, "fixer", time.Time{})
	if err != nil || len(before) != 1 {
		t.Fatal(before, err)
	}
	if _, err := db.EraseDeveloper(ctx, "original"); err != nil {
		t.Fatal(err)
	}
	after, err := db.DeveloperOutcomes(ctx, "fixer", time.Time{})
	if err != nil || len(after) != 1 {
		t.Fatal(after, err)
	}
	a, b := before[0].Projection, after[0].Projection
	if !b.Frozen || a.AllocatedPoints != b.AllocatedPoints || a.CreditedWeight != b.CreditedWeight || a.ProjectedQuality != b.ProjectedQuality || a.RankingEligibility != b.RankingEligibility {
		t.Fatal(a, b)
	}
	c.Revision = 2
	c.Review = nil
	c.Reason = "withdraw"
	if err := db.WithdrawCorrection(ctx, c); err == nil {
		t.Fatal("reallocated erased chain")
	}
}
func TestCorrectionLifecycle_ConcurrentExactReplayIsOneAudit(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, _, rel := projectionFixture(t, db, false)
	c := CorrectionCommand{RelationID: rel, Revision: 1, Actor: "operator-login", Role: "operator", Review: ptrReview(projectionReview())}
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() { errs <- db.ConfirmCorrection(context.Background(), c) }()
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	assertCount(t, db, `SELECT COUNT(*) FROM correction_audit WHERE action='confirm'`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM correction_assessments WHERE basis='reviewed'`, 1)
}
func TestCorrectionLifecycle_ConfirmedPendingParentKeepsDisposition(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	child := correctionOutcome(t, db, "fixer", "acme/app", "pending-child", 20)
	recordCorrection(t, db, child, "Corrects: #10", "reviewed missing parent")
	rels, err := db.CorrectionRelationsByOutcome(ctx, child.ID)
	if err != nil || len(rels) != 1 {
		t.Fatal(rels, err)
	}
	c := CorrectionCommand{RelationID: rowInt(rels[0], "id"), Revision: 1, Actor: "operator-login", Role: "operator", Review: ptrReview(projectionReview())}
	if err := db.ConfirmCorrection(ctx, c); err != nil {
		t.Fatal(err)
	}
	parent := correctionOutcome(t, db, "original", "acme/app", "late-reviewed-parent", 10)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	parent.MergedAt = &at
	if _, err := db.db.Exec(`UPDATE outcomes SET merged_at=?,weight=5,additions=10,deletions=0,changed_files=1 WHERE id=?`, at, parent.ID); err != nil {
		t.Fatal(err)
	}
	childAt := at.Add(time.Hour)
	if _, err := db.db.Exec(`UPDATE outcomes SET merged_at=?,additions=10,deletions=0,changed_files=1 WHERE id=?`, childAt, child.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE correction_declarations SET merged_at=? WHERE outcome_id=?`, childAt, child.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordPROutcome(ctx, parent); err != nil {
		t.Fatal(err)
	}
	rels, err = db.CorrectionRelationsByOutcome(ctx, child.ID)
	if err != nil || rowString(rels[0], "state") != "confirmed" || rowInt(rels[0], "parent_outcome_id") != parent.ID {
		t.Fatal(rels, err)
	}
	if err := db.ReconcileCorrectionChain(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	for _, a := range projectionRows(t, db) {
		if rowInt(a, "outcome_id") == child.ID && (a["rule_version"] != event3.ReviewedRule || rowInt(a, "provisional") != 0) {
			t.Fatal("late parent lost reviewed allocation", a)
		}
	}
}
func TestCorrectionLifecycle_DisputeRequiresExplicitSettlement(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, _, rel := projectionFixture(t, db, false)
	ctx := context.Background()
	if err := db.DisputeCorrection(ctx, CorrectionCommand{RelationID: rel, Revision: 1, Actor: "fixer", Role: "declarer", Reason: "scope disputed"}); err != nil {
		t.Fatal(err)
	}
	review := projectionReview()
	review.Revision = 2
	c := CorrectionCommand{RelationID: rel, Revision: 2, Actor: "operator-login", Role: "operator", Review: &review}
	if err := db.ConfirmCorrection(ctx, c); err == nil {
		t.Fatal("confirmation bypassed explicit settlement")
	}
	c.Settlement = "re-assess"
	c.Reason = "reviewed appeal"
	if err := db.SettleCorrection(ctx, c); err != nil {
		t.Fatal(err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM correction_audit WHERE action='settle-re-assess'`, 1)
}

func TestCorrectionLifecycle_RetainsReviewedSizingAcrossRevisions(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, child, rel := projectionFixture(t, db, false)
	if _, err := db.db.Exec(`UPDATE outcomes SET weight=8 WHERE id=?`, child.ID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := CorrectionCommand{RelationID: rel, Revision: 1, Actor: "operator-login", Role: "operator", Review: ptrReview(projectionReview())}
	if err := db.ConfirmCorrection(ctx, c); err != nil {
		t.Fatal(err)
	}
	// Equivalent rationals and time instants must also replay through the CLI path.
	c.Review.Original = new(big.Rat).SetInt64(5)
	c.Review.Own = new(big.Rat).SetInt64(1)
	c.Review.ReviewedAt = c.Review.ReviewedAt.In(time.FixedZone("review-zone", -7*60*60))
	if err := db.ConfirmCorrection(ctx, c); err != nil {
		t.Fatal("equivalent replay", err)
	}
	assertReviewed := func() {
		t.Helper()
		for _, row := range projectionRows(t, db) {
			if rowInt(row, "outcome_id") == child.ID && (row["raw_weight"] != "8/1" || row["reviewed_own_weight"] != "1/1" || row["allocated_points"] != "1/1" || rowInt(row, "provisional") != 0 || row["rule_version"] != event3.ReviewedRule) {
				t.Fatal("lost current reviewed sizing", row)
			}
		}
	}
	assertReviewed()
	c.Review = nil
	c.Reason = "retain reviewed scope"
	c.Revision = 2
	if err := db.DisputeCorrection(ctx, c); err != nil {
		t.Fatal(err)
	}
	assertReviewed()
	c.Revision = 3
	c.Settlement = "uphold"
	if err := db.SettleCorrection(ctx, c); err != nil {
		t.Fatal(err)
	}
	assertReviewed()
	c.Revision = 4
	if err := db.WithdrawCorrection(ctx, c); err != nil {
		t.Fatal(err)
	}
	assertReviewed()
}

func TestCorrectionLifecycle_ChildOutcomeNotCaptured(t *testing.T) {
	for _, action := range []string{"confirm", "re-assess"} {
		t.Run(action, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			parent, child := captureOutcome(11, "parent-merge"), captureOutcome(22, "child-merge")
			parent.Developer = "original"
			child.Developer = "fixer"
			if _, err := db.RecordPROutcome(ctx, parent); err != nil {
				t.Fatal(err)
			}
			recordCorrection(t, db, child, "Corrects: #11", "merged PR without captured outcome")
			rels, err := db.CorrectionRelationsByPR(ctx, child.Repo, child.PRNumber)
			if err != nil || len(rels) != 1 || rels[0]["child_outcome_id"] != nil {
				t.Fatal(rels, err)
			}
			id := rowInt(rels[0], "id")
			revision := rowInt(rels[0], "revision")
			if action == "re-assess" {
				if err := db.DisputeCorrection(ctx, CorrectionCommand{RelationID: id, Revision: revision, Actor: "fixer", Role: "declarer", Reason: "review scope"}); err != nil {
					t.Fatal(err)
				}
				revision++
			}
			before, err := db.CorrectionDetail(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			review := projectionReview()
			review.Revision = revision
			c := CorrectionCommand{RelationID: id, Revision: revision, Actor: "operator-login", Role: "operator", Reason: "reviewed", Review: &review, Settlement: "re-assess"}
			if action == "confirm" {
				err = db.ConfirmCorrection(ctx, c)
			} else {
				err = db.SettleCorrection(ctx, c)
			}
			const want = "child outcome not captured yet; confirm after the fix PR's outcome is recorded"
			if err == nil || err.Error() != want {
				t.Errorf("got %v, want %q", err, want)
			}
			after, err := db.CorrectionDetail(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("failed command changed relation/evidence/allocation:\nbefore=%v\nafter=%v", before, after)
			}
			assertCount(t, db, `SELECT COUNT(*) FROM correction_assessments WHERE basis='reviewed'`, 0)
			assertCount(t, db, `SELECT COUNT(*) FROM outcomes`, 1)
		})
	}
}
