package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

// Both original and trusted exclusion authors identify a subject's PR. Include
// original identities and trusted exclusion keys, even for outcome-less tombstones.
func correctionIdentities(marks string) string {
	outcome := `SELECT id FROM outcomes WHERE developer IN (` + marks + `)`
	declaration := `developer IN (` + marks + `) OR outcome_id IN (` + outcome + `)`
	exclusion := `developer IN (` + marks + `) OR author_developer IN (` + marks + `)`
	return `SELECT repo,pr_number FROM outcomes WHERE developer IN (` + marks + `) UNION SELECT repo,pr_number FROM correction_declarations WHERE ` + declaration + ` UNION SELECT repo,pr_number FROM outcome_exclusions WHERE ` + exclusion + ` UNION SELECT exclusion_repo,exclusion_pr_number FROM outcome_exclusions WHERE ` + exclusion
}

// Match canonical PR identities too: a pending endpoint can predate its author's
// outcome or declaration and therefore have neither a developer nor a linked id.
func correctionEndpoint(prefix, marks string) string {
	outcome := `SELECT id FROM outcomes WHERE developer IN (` + marks + `)`
	declaration := correctionSubject("correction_declarations", marks)
	return prefix + `developer IN (` + marks + `) OR ` + prefix + `outcome_id IN (` + outcome + `) OR ` + prefix + `declaration_id IN (SELECT id FROM correction_declarations WHERE ` + declaration + `) OR (` + prefix + `repo,` + prefix + `pr_number) IN (` + correctionIdentities(marks) + `)`
}

func correctionSubject(table, marks string) string {
	if table == "correction_declarations" {
		return `developer IN (` + marks + `) OR outcome_id IN (SELECT id FROM outcomes WHERE developer IN (` + marks + `)) OR (repo,pr_number) IN (` + correctionIdentities(marks) + `)`
	}
	predicate := correctionEndpoint("child_", marks) + ` OR ` + correctionEndpoint("parent_", marks)
	if table == "correction_audit" {
		predicate += ` OR actor IN (` + marks + `)`
	}
	return predicate
}

// Export and erasure use the same shared evidence set, including sibling copies
// of a multi-parent declaration's excerpt for endpoint parties.
func correctionPredicates(marks string) map[string]string {
	relations := correctionSubject("corrective_relations", marks)
	// Late endpoint declarations may not have a copied declaration id either.
	shared := correctionSubject("correction_declarations", marks) + ` OR EXISTS (SELECT 1 FROM corrective_relations r WHERE (` + relations + `) AND (
 r.child_declaration_id=correction_declarations.id OR r.parent_declaration_id=correction_declarations.id
 OR (r.child_repo,r.child_pr_number)=(correction_declarations.repo,correction_declarations.pr_number)
 OR (r.parent_repo,r.parent_pr_number)=(correction_declarations.repo,correction_declarations.pr_number)))`
	relations = `(` + relations + `) OR child_declaration_id IN (SELECT id FROM correction_declarations WHERE ` + shared + `)`
	audit := correctionEndpoint("child_", marks) + ` OR ` + correctionEndpoint("parent_", marks) + ` OR relation_id IN (SELECT id FROM corrective_relations WHERE ` + relations + `) OR child_declaration_id IN (SELECT id FROM correction_declarations WHERE ` + shared + `)`
	return map[string]string{"correction_declarations": shared, "corrective_relations": relations, "correction_audit": audit}
}

func correctionArgs(predicate, marks string, args []any) []any {
	if len(args) == 0 || marks == "" {
		return nil
	}
	var bound []any
	for range strings.Count(predicate, marks) {
		bound = append(bound, args...)
	}
	return bound
}

// Resolve row ids before mutating any identifying links or audit actors. Only
// database integer ids enter these SQL literals; subject identifiers stay bound.
func correctionIDs(ctx context.Context, tx *sql.Tx, table, predicate, marks string, args []any) (string, error) {
	rows, err := correctionRows(ctx, tx, "SELECT id FROM "+table+" WHERE "+predicate, correctionArgs(predicate, marks, args))
	if err != nil {
		return "", err
	}
	ids := []string{"NULL"}
	for _, row := range rows {
		ids = append(ids, strconv.FormatInt(row["id"].(int64), 10))
	}
	return strings.Join(ids, ","), nil
}

// Scrub shared evidence and the subject's endpoints before deleting their own
// declarations and outcomes. Surviving counterpart endpoints remain available.
func eraseSubjectCorrections(ctx context.Context, tx *sql.Tx, marks string, args []any, counts map[string]int64) error {
	if err := eraseSubjectCorrectionProjections(ctx, tx, marks, args, counts); err != nil {
		return err
	}
	predicates := correctionPredicates(marks)
	ids := make(map[string]string, len(predicates))
	for _, table := range []string{"correction_declarations", "corrective_relations", "correction_audit"} {
		var err error
		ids[table], err = correctionIDs(ctx, tx, table, predicates[table], marks, args)
		if err != nil {
			return err
		}
	}
	for _, table := range []string{"correction_audit", "corrective_relations", "correction_declarations"} {
		var sets []string
		if table == "correction_declarations" {
			sets = []string{"supporting_text=NULL", "provenance=NULL"}
		} else {
			for _, prefix := range []string{"child_", "parent_"} {
				endpoint := correctionEndpoint(prefix, marks)
				for _, col := range []string{"declaration_id", "outcome_id", "developer", "repo", "pr_number", "merge_commit_sha", "merged_at"} {
					sets = append(sets, prefix+col+"=CASE WHEN "+endpoint+" THEN NULL ELSE "+prefix+col+" END")
				}
			}
			textCols := []string{"supporting_text", "disposition_text", "appeal_text", "resolution_reason"}
			if table == "correction_audit" {
				textCols = []string{"reason_text", "actor", "actor_role"}
			}
			for _, col := range textCols {
				sets = append(sets, col+"=NULL")
			}
		}
		update := strings.Join(sets, ",") + " WHERE id IN (" + ids[table] + ")"
		res, err := tx.ExecContext(ctx, "UPDATE "+table+" SET "+update, correctionArgs(update, marks, args)...)
		if err != nil {
			return err
		}
		counts[table], err = res.RowsAffected()
		if err != nil {
			return err
		}
	}
	// A non-party reviewer owns only their identity on these rows. Preserve the
	// other parties' evidence and this reviewer's nonidentifying role and text.
	query := `UPDATE correction_audit SET actor=NULL WHERE actor IN (` + marks + `)`
	res, err := tx.ExecContext(ctx, query, correctionArgs(query, marks, args)...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	counts["correction_audit"] += n

	// Stats and recorded_at can re-identify the erased author through GitHub.
	own := correctionSubject("correction_declarations", marks)
	_, err = tx.ExecContext(ctx, "DELETE FROM correction_declarations WHERE "+own, correctionArgs(own, marks, args)...)
	return err
}

func exportSubjectCorrections(ctx context.Context, tx *sql.Tx, marks string, args []any, exp *DeveloperExport) error {
	if err := exportSubjectCorrectionProjections(ctx, tx, marks, args, exp); err != nil {
		return err
	}
	predicates := correctionPredicates(marks)
	for _, entry := range []struct {
		table  string
		target *[]map[string]any
	}{
		{"correction_declarations", &exp.CorrectionDeclarations}, {"corrective_relations", &exp.CorrectiveRelations}, {"correction_audit", &exp.CorrectionAudit},
	} {
		predicate := predicates[entry.table]
		if entry.table == "correction_audit" {
			predicate += ` OR actor IN (` + marks + `)`
		}
		rows, err := correctionRows(ctx, tx, "SELECT * FROM "+entry.table+" WHERE "+predicate+" ORDER BY id", correctionArgs(predicate, marks, args))
		if err != nil {
			return err
		}
		*entry.target = rows
	}
	return nil
}

// Maps disclose every stored column, including nullable identifiers and all text.
func correctionRows(ctx context.Context, q rowQuerier, query string, args []any) ([]map[string]any, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		values := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			row[col] = values[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
