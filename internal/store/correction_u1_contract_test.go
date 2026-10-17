package store

import (
	"context"
	"testing"
)

// Runtime baseline receipts: these tests also compile on 11eba0a.
func TestCorrectionU1Schema_ReviewedAndSealedColumns(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	for table, cols := range map[string][]string{
		"correction_assessments": {"participant_outcome_id", "reviewed_own_weight", "before_weight", "after_weight", "reviewer", "reviewed_at", "comparison_base", "comparison_before", "comparison_after"},
		"correction_allocations": {"outcome_id", "share", "projected_quality", "allocated_points", "marginal_credit", "ranking_eligibility", "frozen", "chain_version"},
		"sealed_report":          {"scoring_policy", "correction_rule_version", "rubric_version", "correction_revision"},
		"sealed_rollup":          {"outcome_n"},
	} {
		for _, col := range cols {
			var n int
			if err := db.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, col).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("missing %s.%s", table, col)
			}
		}
	}
}
func TestCorrectionU1DSAR_NewRowsHaveExportAndErase(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	parent, _ := correctionFixture(t, db)
	// Numeric evidence is intentionally retained after erasure; every link and text
	// field in the subject's assessment must be scrubbed in the same transaction.
	_, err := db.db.Exec(`INSERT INTO correction_assessments(relation_id,revision,participant_outcome_id,basis,reviewer,rationale,scope,reviewed_at,rule_version,recorded_at) SELECT id,1,?,'reviewed','parent-login','private rationale','private scope',CURRENT_TIMESTAMP,'event3-v2',CURRENT_TIMESTAMP FROM corrective_relations`, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := db.ExportDeveloper(context.Background(), "parent-login")
	if err != nil {
		t.Fatal(err)
	}
	// Map via JSON so the baseline can compile before the additive export fields exist.
	if len(exp.CorrectionAssessments) != 1 {
		t.Fatal("new assessment omitted from subject export")
	}
	if _, err = db.EraseDeveloper(context.Background(), "parent-login"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.db.QueryRow(`SELECT COUNT(*) FROM correction_assessments WHERE participant_outcome_id IS NOT NULL OR relation_id IS NOT NULL OR reviewer IS NOT NULL OR rationale IS NOT NULL OR scope IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("assessment identifiers survived")
	}
}
