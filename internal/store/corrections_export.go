package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// CorrectionEvidenceColumns fixes the export order and table allowlist.
func CorrectionEvidenceColumns(kind string) ([]string, error) {
	switch kind {
	case "relations":
		return []string{"id", "child_declaration_id", "parent_declaration_id", "child_outcome_id", "parent_outcome_id", "child_developer", "parent_developer", "child_repo", "parent_repo", "child_pr_number", "parent_pr_number", "child_merge_commit_sha", "parent_merge_commit_sha", "child_merged_at", "parent_merged_at", "state", "resolution_reason", "revision", "recorded_at", "updated_at", "rule_version", "supporting_text", "disposition_text", "appeal_text"}, nil
	case "assessments":
		return []string{"id", "relation_id", "revision", "chain_version", "participant_outcome_id", "basis", "original_weight", "original_quality", "reviewed_own_weight", "before_weight", "after_weight", "marginal_credit", "pure_repair", "comparison_base", "comparison_before", "comparison_after", "scope", "rationale", "reviewer", "reviewed_at", "recorded_at", "rule_version"}, nil
	case "allocations":
		return []string{"id", "chain_version", "relation_id", "outcome_id", "allocation_parent_outcome_id", "original_weight", "original_quality", "role", "raw_weight", "original_term", "reviewed_own_weight", "before_weight", "after_weight", "share", "projected_quality", "allocated_points", "marginal_credit", "ranking_eligibility", "status", "pending_reason", "provisional", "disputed", "frozen", "recorded_at", "rule_version"}, nil
	case "audit":
		return []string{"id", "relation_id", "revision", "action", "actor", "actor_role", "recorded_at", "rule_version", "child_declaration_id", "parent_declaration_id", "child_outcome_id", "parent_outcome_id", "child_developer", "parent_developer", "child_repo", "parent_repo", "child_pr_number", "parent_pr_number", "child_merge_commit_sha", "parent_merge_commit_sha", "child_merged_at", "parent_merged_at", "reason_text"}, nil
	}
	return nil, errors.New("unknown correction evidence kind")
}
func correctionEvidenceTable(kind string) (string, error) {
	switch kind {
	case "relations":
		return "corrective_relations", nil
	case "assessments":
		return "correction_assessments", nil
	case "allocations":
		return "correction_allocations", nil
	case "audit":
		return "correction_audit", nil
	}
	return "", errors.New("unknown correction evidence kind")
}

// ListCorrectionEvidence is bounded and keyset paginated by recording time/id.
func (d *DB) ListCorrectionEvidence(ctx context.Context, kind string, since, until time.Time, after PageCursor, limit int) ([]map[string]any, bool, error) {
	table, err := correctionEvidenceTable(kind)
	if err != nil {
		return nil, false, err
	}
	n := clampExportLimit(limit)
	rows, err := correctionRows(ctx, d.db, `SELECT * FROM `+table+keysetWindowSQLFor("recorded_at"), []any{since, until, after.TS, after.TS, after.ID, n + 1})
	if err != nil {
		return nil, false, err
	}
	if len(rows) > n {
		return rows[:n], true, nil
	}
	return rows, false, nil
}

// CorrectionDetail exposes bounded local operator evidence, including the audit.
func (d *DB) CorrectionDetail(ctx context.Context, id int64) (map[string]any, error) {
	tx, release, err := beginRead(ctx, d.db)
	if err != nil {
		return nil, err
	}
	defer release()
	out := map[string]any{"actor_identity": "self-asserted; local CLI input is not authentication"}
	for _, kind := range []string{"relations", "assessments", "allocations", "audit"} {
		table, err := correctionEvidenceTable(kind)
		if err != nil {
			return nil, err
		}
		key := "relation_id"
		if kind == "relations" {
			key = "id"
		}
		query := `SELECT * FROM ` + table + ` WHERE ` + key + `=? ORDER BY id LIMIT 1001`
		args := []any{id}
		if kind == "allocations" {
			query = `WITH RECURSIVE parties(id) AS (
    SELECT child_outcome_id FROM corrective_relations WHERE id=?
    UNION SELECT parent_outcome_id FROM corrective_relations WHERE id=?
    UNION SELECT r.child_outcome_id FROM corrective_relations r JOIN parties p ON r.parent_outcome_id=p.id
    UNION SELECT r.parent_outcome_id FROM corrective_relations r JOIN parties p ON r.child_outcome_id=p.id
   ) SELECT * FROM correction_allocations WHERE outcome_id IN (SELECT id FROM parties WHERE id IS NOT NULL) OR relation_id=? ORDER BY id LIMIT 1001`
			args = []any{id, id, id}
		}
		rows, err := correctionRows(ctx, tx, query, args)
		if err != nil {
			return nil, err
		}
		if len(rows) > 1000 {
			return nil, errors.New("relation evidence exceeds show limit; use paginated exports")
		}
		if kind == "relations" && len(rows) == 0 {
			return nil, sql.ErrNoRows
		}
		out[kind] = rows
	}
	return out, nil
}
