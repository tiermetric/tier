package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func captureOutcome(pr int, sha string) Outcome {
	merged := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return Outcome{Developer: "alice", Repo: "acme/app", PRNumber: pr, MergeCommitSHA: sha,
		IssueID: "issue-9", Weight: 3, Quality: 1, Timestamp: merged, MergedAt: &merged}
}

func TestCorrectionReplay_FillsDeclarationWithoutSecondOutcome(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	o := captureOutcome(22, "child-merge")
	if inserted, err := db.RecordPROutcome(ctx, o); err != nil || !inserted {
		t.Fatalf("initial outcome: %v %v", inserted, err)
	}
	for i := 0; i < 2; i++ {
		recordCorrection(t, db, o, "Corrects: #11", "bounded declaration")
		if inserted, err := db.RecordPROutcome(ctx, o); err != nil || inserted {
			t.Fatalf("replay outcome: %v %v", inserted, err)
		}
	}
	d, found, err := db.CorrectionDeclarationByPR(ctx, o.Repo, o.PRNumber, o.MergeCommitSHA)
	stored, _, _ := db.OutcomeByMergeCommit(ctx, o.MergeCommitSHA)
	if err != nil || !found || d["outcome_id"] != stored.ID {
		t.Fatalf("replay did not attach declaration: %+v %v", d, err)
	}
	for table, want := range map[string]int{"outcomes": 1, "correction_declarations": 1, "corrective_relations": 1, "correction_audit": 1} {
		var n int
		if err := db.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != want {
			t.Fatalf("%s count=%d want=%d err=%v", table, n, want, err)
		}
	}
}

func TestCorrectionParent_ExcludedIsNotMissingOrCredited(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(map[bool]string{false: "tombstone", true: "retained-outcome"}[retained], func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			p := captureOutcome(11, "parent-merge")
			if retained {
				if _, err := db.RecordPROutcome(ctx, p); err != nil {
					t.Fatal(err)
				}
			} else {
				p.IssueID = ""
			}
			if err := db.ExcludePROutcome(ctx, p, "revert evidence", "test"); err != nil {
				t.Fatal(err)
			}
			c := captureOutcome(22, "child-merge")
			recordCorrection(t, db, c, "Corrects: #11", "bounded declaration")
			rels, err := db.CorrectionRelationsByPR(ctx, c.Repo, c.PRNumber)
			if err != nil || len(rels) != 1 || rels[0]["state"] != "excluded" || rels[0]["resolution_reason"] != "excluded-parent" || rels[0]["parent_outcome_id"] != nil {
				t.Fatalf("excluded parent resolution: %+v %v", rels, err)
			}
			if _, found, err := db.OutcomeByMergeCommit(ctx, p.MergeCommitSHA); err != nil || found {
				t.Fatalf("excluded parent credited: %v %v", found, err)
			}
		})
	}
}

func TestCorrectionSettlement_ChildFirstThenParent(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	c, p := captureOutcome(22, "child-merge"), captureOutcome(11, "parent-merge")
	recordCorrection(t, db, c, "Corrects: #11", "bounded declaration")
	if inserted, err := db.RecordPROutcome(ctx, c); err != nil || !inserted {
		t.Fatalf("child: %v %v", inserted, err)
	}
	rels, err := db.CorrectionRelationsByPR(ctx, c.Repo, c.PRNumber)
	child, _, _ := db.OutcomeByMergeCommit(ctx, c.MergeCommitSHA)
	if err != nil || len(rels) != 1 || rels[0]["child_outcome_id"] != child.ID || rels[0]["resolution_reason"] != "missing-parent" {
		t.Fatalf("child-first pending: %+v %v", rels, err)
	}
	// Outcome arrival alone must settle a missing parent, including legacy parents
	// that have no declaration yet. A later declaration must fill that endpoint too.
	if inserted, err := db.RecordPROutcome(ctx, p); err != nil || !inserted {
		t.Fatalf("parent: %v %v", inserted, err)
	}
	parent, _, _ := db.OutcomeByMergeCommit(ctx, p.MergeCommitSHA)
	rels, err = db.CorrectionRelationsByPR(ctx, c.Repo, c.PRNumber)
	if err != nil || len(rels) != 1 || rels[0]["state"] != "declared" || rels[0]["parent_outcome_id"] != parent.ID {
		t.Fatalf("outcome-only parent arrival: %+v %v", rels, err)
	}
	for i := 0; i < 2; i++ {
		recordCorrection(t, db, p, "Corrects: none", "parent evidence")
		if inserted, err := db.RecordPROutcome(ctx, p); err != nil || inserted {
			t.Fatalf("parent replay: %v %v", inserted, err)
		}
	}
	rels, err = db.CorrectionRelationsByPR(ctx, c.Repo, c.PRNumber)
	if err != nil || len(rels) != 1 || rels[0]["state"] != "declared" || rels[0]["resolution_reason"] != nil || rels[0]["parent_outcome_id"] != parent.ID || rels[0]["parent_declaration_id"] == nil {
		t.Fatalf("settled: %+v %v", rels, err)
	}
	var n int
	var points float64
	if err := db.db.QueryRow(`SELECT count(*),sum(weight*quality) FROM outcomes`).Scan(&n, &points); err != nil || n != 2 || points != 6 {
		t.Fatalf("duplicate/changed credit: n=%d points=%v err=%v", n, points, err)
	}
}

func TestCorrectionCapture_OutcomeAndAttachmentRollbackTogether(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	c := captureOutcome(22, "child-merge")
	recordCorrection(t, db, c, "Corrects: #11", "bounded declaration")
	if _, err := db.db.Exec(`CREATE TRIGGER fail_correction_attachment BEFORE UPDATE OF outcome_id ON correction_declarations
 WHEN NEW.outcome_id IS NOT NULL BEGIN SELECT RAISE(ABORT,'injected correction failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordPROutcome(ctx, c); err == nil {
		t.Fatal("expected attachment failure")
	}
	assertCount(t, db, `SELECT count(*) FROM outcomes`, 0)
	assertCount(t, db, `SELECT count(*) FROM correction_declarations WHERE outcome_id IS NOT NULL`, 0)
	if _, err := db.db.Exec(`DROP TRIGGER fail_correction_attachment`); err != nil {
		t.Fatal(err)
	}
	if inserted, err := db.RecordPROutcome(ctx, c); err != nil || !inserted {
		t.Fatalf("retry: %v %v", inserted, err)
	}
	assertCount(t, db, `SELECT count(*) FROM outcomes`, 1)
	assertCount(t, db, `SELECT count(*) FROM correction_declarations WHERE outcome_id IS NOT NULL`, 1)
}

func TestCorrectionCapture_MultipleParentsAndLateExclusion(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	c, p, other := captureOutcome(22, "child-merge"), captureOutcome(11, "parent-merge"), captureOutcome(12, "other-merge")
	recordCorrection(t, db, c, "Corrects: #11, #12", "bounded declaration")
	for _, o := range []Outcome{c, p, other} {
		if inserted, err := db.RecordPROutcome(ctx, o); err != nil || !inserted {
			t.Fatalf("arrival: %v %v", inserted, err)
		}
	}
	assertCount(t, db, `SELECT count(*) FROM corrective_relations WHERE state='declared'`, 2)
	assertCount(t, db, `SELECT count(*) FROM outcomes`, 3)
	if err := db.ExcludePROutcome(ctx, p, "late recognized revert", "test"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if inserted, err := db.RecordPROutcome(ctx, p); err != nil || inserted {
			t.Fatalf("excluded replay: %v %v", inserted, err)
		}
	}
	assertCount(t, db, `SELECT count(*) FROM corrective_relations WHERE state='excluded' AND resolution_reason='excluded-parent' AND parent_outcome_id IS NULL`, 1)
	assertCount(t, db, `SELECT count(*) FROM corrective_relations WHERE state='declared'`, 1)
	assertCount(t, db, `SELECT count(*) FROM outcomes`, 2)
}

func TestCorrectionCapture_MissingOccurrenceStaysPending(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	c, p := captureOutcome(22, "child-merge"), captureOutcome(11, "parent-merge")
	c.MergedAt = nil // Timestamp still exists, but is not merge occurrence evidence.
	recordCorrection(t, db, c, "Corrects: #11", "bounded declaration")
	for _, o := range []Outcome{c, p} {
		if _, err := db.RecordPROutcome(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	assertCount(t, db, `SELECT count(*) FROM corrective_relations WHERE state='pending' AND resolution_reason='missing-occurrence' AND child_merged_at IS NULL`, 1)
}

func TestCorrectionCapture_MixedCaseAttachmentAndAudit(t *testing.T) {
	for _, mode := range []string{"insert", "declaration-replay", "outcome-replay", "parent-arrival"} {
		t.Run(mode, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			c, p := captureOutcome(22, "child-merge"), captureOutcome(11, "parent-merge")
			if mode != "parent-arrival" {
				if _, err := db.InsertOutcome(ctx, p); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "insert" {
				recordCorrection(t, db, c, "Corrects: #11", "first evidence")
			}
			if _, err := db.InsertOutcome(ctx, c); err != nil {
				t.Fatal(err)
			}
			c.Repo = "ACME/App"
			switch mode {
			case "insert", "declaration-replay":
				recordCorrection(t, db, c, "Corrects: #11", "first evidence")
			case "outcome-replay":
				if inserted, err := db.RecordPROutcome(ctx, c); err != nil || inserted {
					t.Fatalf("replay: %v %v", inserted, err)
				}
			case "parent-arrival":
				recordCorrection(t, db, c, "Corrects: #11", "first evidence")
				if _, err := db.InsertOutcome(ctx, p); err != nil {
					t.Fatal(err)
				}
				p.Repo = "AcMe/aPP"
				if _, err := db.RecordPROutcome(ctx, p); err != nil {
					t.Fatal(err)
				}
			}
			child, _, _ := db.OutcomeByMergeCommit(ctx, c.MergeCommitSHA)
			d, found, err := db.CorrectionDeclarationByPR(ctx, "acme/app", c.PRNumber, c.MergeCommitSHA)
			if err != nil || !found || d["outcome_id"] != child.ID {
				t.Fatalf("mixed-case attachment: %+v %v", d, err)
			}
			rels, err := db.CorrectionRelationsByPR(ctx, "acme/app", c.PRNumber)
			if err != nil || len(rels) != 1 || rels[0]["state"] != "declared" || rels[0]["child_outcome_id"] != child.ID || rels[0]["parent_outcome_id"] == nil {
				t.Fatalf("mixed-case reconciliation: %+v %v", rels, err)
			}
			var audits int
			if err := db.db.QueryRow(`SELECT count(*) FROM correction_audit WHERE relation_id=? AND child_repo='acme/app' AND parent_repo='acme/app' AND child_outcome_id=? AND parent_outcome_id IS NOT NULL AND revision=?`, rels[0]["id"], child.ID, rels[0]["revision"]).Scan(&audits); err != nil || audits != 1 {
				t.Fatalf("audit=%d err=%v", audits, err)
			}
			var points float64
			if err := db.db.QueryRow(`SELECT sum(weight*quality) FROM outcomes`).Scan(&points); err != nil || points != 6 {
				t.Fatalf("credit=%v err=%v", points, err)
			}
		})
	}
}

func TestCorrectionReplay_IdenticalDeclarationAttachesLateOutcome(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	c := captureOutcome(22, "child-merge")
	id := recordCorrection(t, db, c, "Corrects: #11", "first evidence")
	// Legacy insertion leaves attachment to the identical declaration replay.
	if _, err := db.InsertOutcome(ctx, c); err != nil {
		t.Fatal(err)
	}
	if got := recordCorrection(t, db, c, "Corrects: #11", "later evidence"); got != id {
		t.Fatalf("id=%d want=%d", got, id)
	}
	child, _, _ := db.OutcomeByMergeCommit(ctx, c.MergeCommitSHA)
	d, found, err := db.CorrectionDeclarationByPR(ctx, c.Repo, c.PRNumber, c.MergeCommitSHA)
	if err != nil || !found || d["outcome_id"] != child.ID || d["supporting_text"] != "first evidence" {
		t.Fatalf("replay attachment: %+v %v", d, err)
	}
	assertCount(t, db, `SELECT count(*) FROM correction_audit WHERE child_outcome_id IS NOT NULL`, 1)
}

func TestApplyPRExclusion_MultipleParentsAndLateExclusion(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint("event3-v2=", enabled), func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			c, p, other := captureOutcome(22, "child-merge"), captureOutcome(11, "parent-merge"), captureOutcome(12, "other-merge")
			recordCorrection(t, db, c, "Corrects: #11, #12", "bounded declaration")
			for _, o := range []Outcome{c, p, other} {
				if inserted, err := db.RecordPROutcome(ctx, o); err != nil || !inserted {
					t.Fatalf("arrival: %v %v", inserted, err)
				}
			}
			assertCount(t, db, `SELECT count(*) FROM corrective_relations WHERE state='declared'`, 2)
			assertCount(t, db, `SELECT count(*) FROM outcomes`, 3)
			if err := db.ConfigureCorrectionScoring(ctx, enabled); err != nil {
				t.Fatal(err)
			}
			stored, found, err := db.OutcomeByMergeCommit(ctx, p.MergeCommitSHA)
			if err != nil || !found {
				t.Fatalf("parent: %v %v", found, err)
			}
			id := PRExclusionIdentity{Repo: p.Repo, PR: p.PRNumber, OutcomeID: stored.ID, MergeSHA: p.MergeCommitSHA}
			for pass := 0; pass < 2; pass++ {
				if changed, err := db.ApplyPRExclusion(ctx, id, "late recognized revert", "operator"); err != nil || changed != (pass == 0) {
					t.Fatalf("apply pass %d: %v %v", pass, changed, err)
				}
			}
			for i := 0; i < 2; i++ {
				if inserted, err := db.RecordPROutcome(ctx, p); err != nil || inserted {
					t.Fatalf("excluded replay: %v %v", inserted, err)
				}
			}
			assertCount(t, db, `SELECT count(*) FROM corrective_relations WHERE state='excluded' AND resolution_reason='excluded-parent' AND parent_outcome_id IS NULL`, 1)
			assertCount(t, db, `SELECT count(*) FROM corrective_relations WHERE state='declared'`, 1)
			assertCount(t, db, `SELECT count(*) FROM outcomes`, 2)

			policy, err := db.CorrectionScoringPolicy(ctx)
			if err != nil || policy.Enabled != enabled {
				t.Fatalf("policy: %+v %v", policy, err)
			}
			if enabled {
				if policy.RubricVersion != 2 || policy.RuleVersion != "event3-v2" {
					t.Fatalf("wrong policy: %+v", policy)
				}
				// Historical allocation evidence can survive exclusion; current
				// scoring must read only the two live outcomes and their projections.
				rows, err := db.AllOutcomesWindow(ctx, time.Time{}, c.Timestamp.Add(time.Hour), FleetWide)
				if err != nil || len(rows) != 2 {
					t.Fatalf("current scoring rows: %+v %v", rows, err)
				}
				for _, row := range rows {
					if row.ID == stored.ID || row.Projection == nil || (row.Projection.RuleVersion != "event3-v1" && row.Projection.RuleVersion != "event3-v2") {
						t.Fatalf("excluded or unresolved scoring row: %+v", row)
					}
				}
			}
		})
	}
}
