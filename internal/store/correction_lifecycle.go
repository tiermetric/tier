package store

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/tiermetric/tier/internal/event3"
)

// CorrectionCommand identifies an optimistic revision. Actor and Role are
// self-asserted local operator input, not authenticated identities.
type CorrectionCommand struct {
	RelationID, Revision            int64
	Actor, Role, Reason, Settlement string
	Review                          *event3.Review
}

func (d *DB) ConfirmCorrection(ctx context.Context, c CorrectionCommand) error {
	return d.mutateCorrection(ctx, c, "confirm")
}
func (d *DB) DisputeCorrection(ctx context.Context, c CorrectionCommand) error {
	return d.mutateCorrection(ctx, c, "dispute")
}
func (d *DB) WithdrawCorrection(ctx context.Context, c CorrectionCommand) error {
	return d.mutateCorrection(ctx, c, "withdraw")
}
func (d *DB) SettleCorrection(ctx context.Context, c CorrectionCommand) error {
	return d.mutateCorrection(ctx, c, "settle-"+c.Settlement)
}

func (d *DB) mutateCorrection(ctx context.Context, c CorrectionCommand, action string) error {
	if c.RelationID <= 0 || c.Revision <= 0 || strings.TrimSpace(c.Actor) == "" || strings.TrimSpace(c.Role) == "" || len(c.Actor) > 128 || len(c.Role) > 128 || len(c.Reason) > 4096 {
		return errors.New("relation, revision and bounded self-asserted actor/role required")
	}
	if action != "confirm" && strings.TrimSpace(c.Reason) == "" {
		return errors.New("dispute, withdrawal and settlement require a reason")
	}
	if action == "confirm" || action == "settle-re-assess" {
		if c.Review == nil {
			return errors.New("reviewed sizing required")
		}
		if err := c.Review.Validate(); err != nil {
			return err
		}
		if c.Review.Revision != c.Revision || c.Review.Reviewer != c.Actor {
			return errors.New("review revision/reviewer must match command")
		}
	}
	if c.Review != nil && action != "confirm" && action != "settle-re-assess" {
		return errors.New("reviewed sizing is accepted only for confirmation or re-assessment")
	}
	tx, err := beginImmediate(ctx, d.db)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := d.prepareCorrectionWriteTx(ctx, tx); err != nil {
		return err
	}
	rows, err := correctionRows(ctx, tx, `SELECT * FROM corrective_relations WHERE id=?`, []any{c.RelationID})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return sql.ErrNoRows
	}
	row := rows[0]
	// A retry is accepted only when the complete audit/evidence input matches.
	audit, err := correctionRows(ctx, tx, `SELECT * FROM correction_audit WHERE relation_id=? AND revision=? AND action=?`, []any{c.RelationID, c.Revision + 1, action})
	if err != nil {
		return err
	}
	if len(audit) > 0 {
		if rowString(audit[0], "actor") != c.Actor || rowString(audit[0], "actor_role") != c.Role || rowString(audit[0], "reason_text") != c.Reason {
			return errors.New("conflicting correction replay")
		}

		if c.Review != nil {
			reviews, e := correctionRows(ctx, tx, `SELECT * FROM correction_assessments WHERE relation_id=? AND revision=? AND basis='reviewed'`, []any{c.RelationID, c.Revision + 1})
			if e != nil {
				return e
			}
			if len(reviews) != 1 {
				return errors.New("review replay unavailable")
			}
			previous, e := reviewFromRow(reviews[0])
			if e != nil {
				return e
			}
			previous.Revision = c.Revision
			if !reviewsEqual(*previous, *c.Review) {
				return errors.New("conflicting review replay")
			}
		}
		return tx.Commit()
	}
	if rowInt(row, "revision") != c.Revision || row["child_developer"] == nil || row["parent_repo"] == nil {
		return errors.New("stale or erased correction relation")
	}
	if (action == "confirm" || action == "settle-re-assess") && row["child_outcome_id"] == nil {
		return errors.New("child outcome not captured yet; confirm after the fix PR's outcome is recorded")
	}
	state := rowString(row, "state")
	next := ""
	retained := state == "confirmed" || state == "disputed" || state == "withdrawn-confirmed"
	switch action {
	case "confirm":
		if c.Role != "operator" {
			return errors.New("confirmation requires operator role")
		}
		if retained || state == "disputed-declared" || state == "removed" || state == "unrelated" || state == "withdrawn" {
			return errors.New("relation requires operator settlement")
		}
		next = "confirmed"
	case "dispute":
		if state == "removed" || state == "withdrawn" || state == "unrelated" {
			return errors.New("inactive relation")
		}
		next = "disputed-declared"
		if retained {
			next = "disputed"
		}
	case "withdraw":
		// D3 retains confirmed allocations through dispute and withdrawal until settlement.
		if state == "disputed-declared" || state == "removed" || state == "unrelated" || state == "withdrawn" || state == "withdrawn-confirmed" {
			return errors.New("withdrawal requires active relation without unconfirmed dispute")
		}
		next = "withdrawn"
		if retained {
			next = "withdrawn-confirmed"
		}
	case "settle-uphold", "settle-re-assess", "settle-remove":
		if c.Role != "operator" {
			return errors.New("settlement requires operator role")
		}
		if state != "disputed" && state != "withdrawn-confirmed" && state != "disputed-declared" {
			return errors.New("settlement requires disputed or withdrawn-confirmed relation")
		}
		next = "confirmed"
		if action == "settle-remove" {
			next = "removed"
		}
		if action == "settle-uphold" && !retained {
			next = "declared"
		}
	default:
		return errors.New("settlement must be uphold, re-assess or remove")
	}
	if next == "confirmed" {
		actor, e := canonicalCorrectionIdentityTx(ctx, tx, c.Actor)
		if e != nil {
			return e
		}
		child, e := canonicalCorrectionIdentityTx(ctx, tx, rowString(row, "child_developer"))
		if e != nil {
			return e
		}
		if strings.EqualFold(actor, child) {
			return errors.New("confirmer must differ from declarer")
		}
	}
	review := c.Review
	_, err = tx.ExecContext(ctx, `UPDATE corrective_relations SET state=?,revision=revision+1,disposition_text=?,updated_at=? WHERE id=?`, next, c.Reason, d.clock().UTC(), c.RelationID)
	if err != nil {
		return err
	}
	if review != nil {
		current := *review
		current.Revision = c.Revision + 1
		current.ReviewedAt = current.ReviewedAt.UTC()
		if err = d.recordCorrectionAssessmentTx(ctx, tx, c.RelationID, current.Revision, current); err != nil {
			return err
		}
	} else if retained && next != "removed" {
		if err = carryCorrectionReviewTx(ctx, tx, c.RelationID, c.Revision+1); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO correction_audit(relation_id,revision,action,actor,actor_role,recorded_at,rule_version,child_declaration_id,parent_declaration_id,child_outcome_id,parent_outcome_id,child_developer,parent_developer,child_repo,parent_repo,child_pr_number,parent_pr_number,child_merge_commit_sha,parent_merge_commit_sha,child_merged_at,parent_merged_at,reason_text)
 SELECT id,revision,?,?,?,?, 'event3-v2',child_declaration_id,parent_declaration_id,child_outcome_id,parent_outcome_id,child_developer,parent_developer,child_repo,parent_repo,child_pr_number,parent_pr_number,child_merge_commit_sha,parent_merge_commit_sha,child_merged_at,parent_merged_at,? FROM corrective_relations WHERE id=?`, action, c.Actor, c.Role, d.clock().UTC(), c.Reason, c.RelationID)
	if err != nil {
		return err
	}
	if id := rowInt(row, "child_outcome_id"); id > 0 {
		if err = d.reconcileCorrectionChainTx(ctx, tx, id); err != nil {
			return err
		}
	}
	if err = d.refreshCorrectionRelationTx(ctx, tx, c.RelationID); err != nil {
		return err
	}
	return tx.Commit()
}

// Alias storage preserves spelling. Resolve a case variant to the stored alias
// before using the same canonical resolver as the rest of the store.
func canonicalCorrectionIdentityTx(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	var alias string
	err := tx.QueryRowContext(ctx, `SELECT alias FROM developer_alias WHERE alias=? COLLATE NOCASE ORDER BY alias LIMIT 1`, id).Scan(&alias)
	if err == nil {
		id = alias
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return canonicalOfTx(ctx, tx, id)
}

func reviewsEqual(a, b event3.Review) bool {
	for i, size := range []*big.Rat{a.Original, a.Own, a.Before, a.After} {
		other := []*big.Rat{b.Original, b.Own, b.Before, b.After}[i]
		if (size == nil) != (other == nil) || size != nil && size.Cmp(other) != 0 {
			return false
		}
	}
	if !a.ReviewedAt.Equal(b.ReviewedAt) {
		return false
	}
	a.Original, a.Own, a.Before, a.After = nil, nil, nil, nil
	b.Original, b.Own, b.Before, b.After = nil, nil, nil, nil
	a.ReviewedAt, b.ReviewedAt = time.Time{}, time.Time{}
	return a == b
}
