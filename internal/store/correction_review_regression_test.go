package store

import (
	"context"
	"math/big"
	"testing"
	"time"
)

func TestCorrectionAssessment_EraseBeforeFirstProjection(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, _, relation := projectionFixture(t, db, false)
	ctx := context.Background()
	if _, err := db.EraseDeveloper(ctx, "original"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordCorrectionAssessment(ctx, relation, 1, projectionReview()); err == nil {
		t.Fatal("accepted fresh reviewed identities on erased endpoint before allocations exist")
	}
}

func projectionThreeLinks(t *testing.T, db *DB) (Outcome, Outcome, Outcome, Outcome, int64) {
	t.Helper()
	o, b, r := projectionFixture(t, db, false)
	confirmFixture(t, db, r)
	c := correctionOutcome(t, db, "carol", "acme/app", "carol-sha", 30)
	d := correctionOutcome(t, db, "dave", "acme/app", "dave-sha", 40)
	if _, err := db.db.Exec(`UPDATE outcomes SET weight=1,quality=1,merged_at=?,additions=10,deletions=0,changed_files=1 WHERE id IN (?,?)`, b.MergedAt, c.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	recordCorrection(t, db, c, "Corrects: #20", "Carol repairs Bob")
	recordCorrection(t, db, d, "Corrects: #30", "Dave repairs Carol")
	if err := db.ReconcileCorrectionChain(context.Background(), d.ID); err != nil {
		t.Fatal(err)
	}
	return o, b, c, d, r
}

func TestCorrectionProjectionExport_DirectOnlyThreeLinks(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	o, _, _, _, r := projectionThreeLinks(t, db)
	exp, err := db.ExportDeveloper(context.Background(), "carol")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range exp.CorrectionAllocations {
		if rowInt(row, "outcome_id") == o.ID {
			t.Error("Carol exported Alice allocation")
		}
	}
	for _, row := range exp.CorrectionAssessments {
		if rowInt(row, "relation_id") == r {
			t.Error("Carol exported Bob to Alice review", row)
		}
	}
	if len(exp.CorrectionAllocations) != 2 || len(exp.CorrectionAssessments) != 2 {
		t.Fatalf("direct export allocations=%d assessments=%d", len(exp.CorrectionAllocations), len(exp.CorrectionAssessments))
	}
}

func TestCorrectionProjectionErase_DeletesOwnRowsFreezesThreeLinks(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	o, _, c, _, _ := projectionThreeLinks(t, db)
	before := projectionRows(t, db)
	ctx := context.Background()
	if _, err := db.EraseDeveloper(ctx, "carol"); err != nil {
		t.Fatal(err)
	}
	after := projectionRows(t, db)
	if len(after) != 3 {
		t.Fatalf("subject allocation retained: %d rows", len(after))
	}
	for _, row := range after {
		if rowInt(row, "outcome_id") == c.ID || row["outcome_id"] == nil {
			t.Fatal("subject allocation survives", row)
		}
		if rowInt(row, "frozen") != 1 {
			t.Fatal("transitive survivor not frozen", row)
		}
		for _, prior := range before {
			if prior["outcome_id"] == row["outcome_id"] && prior["allocated_points"] != row["allocated_points"] {
				t.Fatal("survivor points changed")
			}
		}
	}
	rows, err := correctionRows(ctx, db.db, `SELECT * FROM correction_assessments`, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row["participant_outcome_id"] == nil || rowInt(row, "participant_outcome_id") == c.ID {
			t.Fatal("subject assessment survives", row)
		}
	}
	var rationale string
	if err := db.db.QueryRow(`SELECT rationale FROM correction_assessments WHERE basis='reviewed'`).Scan(&rationale); err != nil || rationale != projectionReview().Rationale {
		t.Fatalf("unrelated review scrubbed: %q %v", rationale, err)
	}
	if err := db.ReconcileCorrectionChain(ctx, o.ID); err != nil {
		t.Fatalf("transitive survivor not frozen: %v", err)
	}
}

func TestCorrectionAssessment_ReplayUsesValuesAndInstant(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, _, relation := projectionFixture(t, db, false)
	ctx := context.Background()
	review := projectionReview()
	if err := db.RecordCorrectionAssessment(ctx, relation, 1, review); err != nil {
		t.Fatal(err)
	}
	review.Original = new(big.Rat).SetInt64(5)
	review.Own = new(big.Rat).SetInt64(1)
	review.ReviewedAt = review.ReviewedAt.In(time.FixedZone("review-zone", -7*60*60))
	if err := db.RecordCorrectionAssessment(ctx, relation, 1, review); err != nil {
		t.Fatal("equal sizing and instant rejected", err)
	}
}

func TestCorrectionProjection_ReviewMustMatchCurrentRevision(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, f, relation := projectionFixture(t, db, true)
	ctx := context.Background()
	if err := db.RecordCorrectionAssessment(ctx, relation, 1, projectionReview()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE corrective_relations SET revision=2; UPDATE outcomes SET weight=8 WHERE id=?`, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.ReconcileCorrectionChain(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	for _, row := range projectionRows(t, db) {
		if rowInt(row, "outcome_id") == f.ID && (row["rule_version"] != "event3-v1" || row["reviewed_own_weight"] != "8/1") {
			t.Fatal("stale review selected", row)
		}
	}
}

func TestCorrectionProjection_RelationMatchesSelectedParent(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	_, b, _ := projectionFixture(t, db, true)
	c := correctionOutcome(t, db, "original", "acme/app", "deepest-sha", 30)
	c.MergedAt = b.MergedAt
	if _, err := db.db.Exec(`UPDATE outcomes SET weight=1,quality=1,merged_at=?,additions=10,deletions=0,changed_files=1 WHERE id=?`, b.MergedAt, c.ID); err != nil {
		t.Fatal(err)
	}
	recordCorrection(t, db, c, "Corrects: #10, #20", "deepest parent")
	if err := db.ReconcileCorrectionChain(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}
	var relation int64
	if err := db.db.QueryRow(`SELECT id FROM corrective_relations WHERE child_outcome_id=? AND parent_outcome_id=?`, c.ID, b.ID).Scan(&relation); err != nil {
		t.Fatal(err)
	}
	for _, row := range projectionRows(t, db) {
		if rowInt(row, "outcome_id") == c.ID && (rowInt(row, "allocation_parent_outcome_id") != b.ID || rowInt(row, "relation_id") != relation) {
			t.Fatal("wrong selected relation", row)
		}
	}
}
