package store

import (
	"context"
	"database/sql"
)

// Expand to the complete connected evidence set before erasing any links. This
// includes endpoint subjects and indirect ancestor/sibling assessments.
// Allocations with ANY erased party are frozen together, never redistributed.
func projectionSubjectOutcomeIDs(marks string) string {
	return `SELECT id FROM outcomes WHERE developer IN (` + marks + `) UNION SELECT id FROM outcome_exclusions WHERE developer IN (` + marks + `) OR author_developer IN (` + marks + `)`
}
func projectionPrivacyPredicates(marks string) map[string]string {
	seed := correctionPredicates(marks)["corrective_relations"] + ` OR id IN (SELECT relation_id FROM correction_assessments WHERE participant_outcome_id IN (` + projectionSubjectOutcomeIDs(marks) + `))`
	edges := `WITH RECURSIVE connected(id) AS (
 SELECT id FROM corrective_relations WHERE ` + seed + `
 UNION SELECT r.id FROM connected c JOIN corrective_relations prior ON prior.id=c.id JOIN corrective_relations r
 ON r.child_outcome_id IN (prior.child_outcome_id,prior.parent_outcome_id)
 OR r.parent_outcome_id IN (prior.child_outcome_id,prior.parent_outcome_id)
 ) SELECT id FROM connected`
	outcomes := `SELECT child_outcome_id FROM corrective_relations WHERE id IN (` + edges + `) UNION SELECT parent_outcome_id FROM corrective_relations WHERE id IN (` + edges + `)`
	own := projectionSubjectOutcomeIDs(marks)
	return map[string]string{
		"correction_assessments": `relation_id IN (` + edges + `) OR participant_outcome_id IN (` + outcomes + `) OR participant_outcome_id IN (` + own + `) OR reviewer IN (` + marks + `)`,
		"correction_allocations": `relation_id IN (` + edges + `) OR outcome_id IN (` + outcomes + `) OR outcome_id IN (` + own + `)`,
	}
}

// Export discloses direct subject evidence; erasure alone traverses the chain.
func projectionExportPredicates(marks string) map[string]string {
	direct := `SELECT id FROM corrective_relations WHERE ` + correctionSubject("corrective_relations", marks)
	own := projectionSubjectOutcomeIDs(marks)
	return map[string]string{
		"correction_assessments": `relation_id IN (` + direct + `) OR participant_outcome_id IN (` + own + `) OR reviewer IN (` + marks + `)`,
		"correction_allocations": `relation_id IN (` + direct + `) OR outcome_id IN (` + own + `) OR allocation_parent_outcome_id IN (` + own + `)`,
	}
}
func exportSubjectCorrectionProjections(ctx context.Context, tx *sql.Tx, marks string, args []any, exp *DeveloperExport) error {
	predicates := projectionExportPredicates(marks)
	for _, entry := range []struct {
		table  string
		target *[]map[string]any
	}{{"correction_assessments", &exp.CorrectionAssessments}, {"correction_allocations", &exp.CorrectionAllocations}} {
		predicate := predicates[entry.table]
		rows, err := correctionRows(ctx, tx, "SELECT * FROM "+entry.table+" WHERE "+predicate+" ORDER BY id", correctionArgs(predicate, marks, args))
		if err != nil {
			return err
		}
		*entry.target = rows
	}
	return nil
}
func eraseSubjectCorrectionProjections(ctx context.Context, tx *sql.Tx, marks string, args []any, counts map[string]int64) error {
	predicates := projectionPrivacyPredicates(marks)
	allocationIDs, err := correctionIDs(ctx, tx, "correction_allocations", predicates["correction_allocations"], marks, args)
	if err != nil {
		return err
	}
	own := projectionSubjectOutcomeIDs(marks)
	for _, entry := range []struct{ table, column string }{
		{"correction_assessments", "participant_outcome_id"},
		{"correction_allocations", "outcome_id"},
	} {
		query := "DELETE FROM " + entry.table + " WHERE " + entry.column + " IN (" + own + ")"
		res, err := tx.ExecContext(ctx, query, correctionArgs(query, marks, args)...)
		if err != nil {
			return err
		}
		counts[entry.table], err = res.RowsAffected()
		if err != nil {
			return err
		}
	}
	// Preserve reviews of unrelated survivors; scrub only direct subject evidence.
	direct := `relation_id IN (SELECT id FROM corrective_relations WHERE ` + correctionSubject("corrective_relations", marks) + `) OR participant_outcome_id IN (` + own + `)`
	scrub, err := correctionIDs(ctx, tx, "correction_assessments", direct, marks, args)
	if err != nil {
		return err
	}
	for _, entry := range []struct{ table, sets, ids string }{
		{"correction_assessments", `relation_id=NULL,comparison_base=NULL,comparison_before=NULL,comparison_after=NULL,scope=NULL,rationale=NULL,reviewer=NULL,reviewed_at=NULL`, scrub},
		{"correction_allocations", `relation_id=NULL,allocation_parent_outcome_id=NULL,frozen=1`, allocationIDs},
	} {
		query := "UPDATE " + entry.table + " SET " + entry.sets + " WHERE id IN (" + entry.ids + ")"
		res, err := tx.ExecContext(ctx, query)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		counts[entry.table] += n
	}
	query := `UPDATE correction_assessments SET reviewer=NULL WHERE reviewer IN (` + marks + `)`
	res, err := tx.ExecContext(ctx, query, correctionArgs(query, marks, args)...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	counts["correction_assessments"] += n

	return nil
}
