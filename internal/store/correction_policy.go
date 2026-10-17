package store

import (
	"context"
	"database/sql"
	"errors"
)

var ErrCorrectionPolicyUnresolved = errors.New("event 3 policy unresolved")

// CorrectionPolicyState is the persisted policy, pinned with its projections.
// Reading an unresolved enabled policy refuses the report rather than labelling
// legacy values with rubric 2. No migration or lifecycle command enables it.
type CorrectionPolicyState struct {
	Enabled       bool
	RubricVersion int
	RuleVersion   string
	Revision      int64
}

func (d *DB) CorrectionScoringPolicy(ctx context.Context) (CorrectionPolicyState, error) {
	var p CorrectionPolicyState
	err := d.ReadSnapshot(ctx, func(s *Snapshot) error { var e error; p, e = s.CorrectionScoringPolicy(ctx); return e })
	return p, err
}
func (r reader) CorrectionScoringPolicy(ctx context.Context) (CorrectionPolicyState, error) {
	var p CorrectionPolicyState
	if err := r.q.QueryRowContext(ctx, `SELECT scoring_enabled,rubric_version,rule_version,projection_revision FROM correction_policy WHERE id=1`).Scan(&p.Enabled, &p.RubricVersion, &p.RuleVersion, &p.Revision); err != nil {
		return p, err
	}
	if !p.Enabled {
		if p.RubricVersion != 1 || p.RuleVersion != "legacy-v1" {
			return p, errors.New("inconsistent legacy correction policy")
		}
		return p, nil
	}
	if p.RubricVersion != 2 || p.RuleVersion != "event3-v2" {
		return p, ErrCorrectionPolicyUnresolved
	}
	var missing bool
	if err := r.q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM outcomes o LEFT JOIN correction_allocations a ON a.outcome_id=o.id WHERE a.id IS NULL OR a.rule_version NOT IN ('event3-v1','event3-v2') LIMIT 1)`).Scan(&missing); err != nil {
		return p, err
	}
	if missing {
		return p, ErrCorrectionPolicyUnresolved
	}
	return p, nil
}

// ConfigureCorrectionScoring is the only policy flip. The write lock covers the
// complete-chain resolution AND the stamp: readers see all legacy or all Event 3.
func (d *DB) ConfigureCorrectionScoring(ctx context.Context, enabled bool) error {
	tx, err := beginImmediate(ctx, d.db)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if enabled {
		if err = d.resolveAllCorrectionsTx(ctx, tx); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE correction_policy SET scoring_enabled=1,rubric_version=2,rule_version='event3-v2' WHERE id=1`)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE correction_policy SET scoring_enabled=0,rubric_version=1,rule_version='legacy-v1' WHERE id=1`)
	}
	if err != nil {
		return err
	}
	if _, err = (reader{tx}).CorrectionScoringPolicy(ctx); err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) resolveAllCorrectionsTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM outcomes ORDER BY id`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	return d.resolveCorrectionIDsTx(ctx, tx, ids)
}

func (d *DB) resolveCorrectionIDsTx(ctx context.Context, tx *sql.Tx, ids []int64) error {
	visited := map[int64]bool{}
	for _, id := range ids {
		if visited[id] {
			continue
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM outcomes WHERE id=?)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			continue
		}
		component, _, err := correctionComponent(ctx, tx, id)
		if err != nil {
			return err
		}
		if err = d.reconcileCorrectionChainTx(ctx, tx, id); err != nil {
			return err
		}
		for member := range component {
			visited[member] = true
		}
	}
	return nil
}

// prepareCorrectionWriteTx runs under the write lock BEFORE a scoped mutation.
// A pre-existing global invalidation may belong to an unrelated component, so
// resolve all of it before this transaction can publish a scoped refresh. This
// also distinguishes old invalidation from triggers fired by our own mutation.
func (d *DB) prepareCorrectionWriteTx(ctx context.Context, tx *sql.Tx) error {
	var enabled bool
	var rubric int
	var rule string
	if err := tx.QueryRowContext(ctx, `SELECT scoring_enabled,rubric_version,rule_version FROM correction_policy WHERE id=1`).Scan(&enabled, &rubric, &rule); err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	if rubric == 2 && rule == "event3-v2" {
		return nil
	}
	if err := d.resolveAllCorrectionsTx(ctx, tx); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE correction_policy SET rubric_version=2,rule_version='event3-v2' WHERE id=1`)
	return err
}

// Enabled scoped writers first call prepareCorrectionWriteTx before changing
// any evidence, then repair their own projections before publishing. Older v10
// writers leave the trigger's unresolved marker; reports refuse it.
func (d *DB) refreshEnabledCorrectionsTx(ctx context.Context, tx *sql.Tx, ids ...int64) error {
	var enabled bool
	if err := tx.QueryRowContext(ctx, `SELECT scoring_enabled FROM correction_policy WHERE id=1`).Scan(&enabled); err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	var err error
	if ids == nil {
		err = d.resolveAllCorrectionsTx(ctx, tx)
	} else {
		err = d.resolveCorrectionIDsTx(ctx, tx, ids)
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE correction_policy SET rubric_version=2,rule_version='event3-v2' WHERE id=1`)
	return err
}

// Outcome identity and relation endpoints bound an ingest refresh. An empty
// result is deliberately non-nil: a declaration without outcomes needs no resolve.
func correctionOutcomeIDsTx(ctx context.Context, tx *sql.Tx, o Outcome) ([]int64, error) {
	ids, err := queryInt64s(ctx, tx, `SELECT id FROM outcomes WHERE merge_commit_sha=?
 UNION SELECT id FROM outcomes WHERE repo=? AND issue_id=? AND merge_commit_sha IS NULL AND (push_day=? OR (developer=? AND ts=?))
 UNION SELECT child_outcome_id FROM corrective_relations WHERE child_outcome_id IS NOT NULL AND ((child_repo=? AND child_pr_number=?) OR (parent_repo=? AND parent_pr_number=?))
 UNION SELECT parent_outcome_id FROM corrective_relations WHERE parent_outcome_id IS NOT NULL AND ((child_repo=? AND child_pr_number=?) OR (parent_repo=? AND parent_pr_number=?))`, o.MergeCommitSHA, normalizeRepo(o.Repo), o.IssueID, o.PushDay, o.Developer, normalizeTS(o.Timestamp), normalizeRepo(o.Repo), o.PRNumber, normalizeRepo(o.Repo), o.PRNumber, normalizeRepo(o.Repo), o.PRNumber, normalizeRepo(o.Repo), o.PRNumber)
	return append([]int64{}, ids...), err
}
func (d *DB) refreshCorrectionOutcomeTx(ctx context.Context, tx *sql.Tx, o Outcome) error {
	ids, err := correctionOutcomeIDsTx(ctx, tx, o)
	if err != nil {
		return err
	}
	return d.refreshEnabledCorrectionsTx(ctx, tx, ids...)
}
func (d *DB) refreshCorrectionRelationTx(ctx context.Context, tx *sql.Tx, relation int64) error {
	ids, err := queryInt64s(ctx, tx, `SELECT child_outcome_id FROM corrective_relations WHERE id=? AND child_outcome_id IS NOT NULL UNION SELECT parent_outcome_id FROM corrective_relations WHERE id=? AND parent_outcome_id IS NOT NULL`, relation, relation)
	if err != nil {
		return err
	}
	return d.refreshEnabledCorrectionsTx(ctx, tx, append([]int64{}, ids...)...)
}
