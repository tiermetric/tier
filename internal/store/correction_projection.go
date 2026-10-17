package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/tiermetric/tier/internal/event3"
)

var ErrCorrectionFrozen = errors.New("correction chain contains an erased party; allocation is frozen")

// ReconcileCorrectionChain persists a whole connected chain under the SQLite
// write lock, independent of report windows. U2/U4 wire the mutation/read paths;
// this core never enables scoring or mutates raw outcomes, quality or seals.
func (d *DB) ReconcileCorrectionChain(ctx context.Context, outcomeID int64) error {
	tx, err := beginImmediate(ctx, d.db)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := d.prepareCorrectionWriteTx(ctx, tx); err != nil {
		return err
	}
	if err = d.reconcileCorrectionChainTx(ctx, tx, outcomeID); err != nil {
		return err
	}
	if err = d.refreshEnabledCorrectionsTx(ctx, tx, outcomeID); err != nil {
		return err
	}
	return tx.Commit()
}

func correctionComponent(ctx context.Context, tx *sql.Tx, id int64) (map[int64]bool, []map[string]any, error) {
	rows, err := correctionRows(ctx, tx, `WITH RECURSIVE component(id) AS (
 VALUES(?) UNION SELECT r.parent_outcome_id FROM corrective_relations r JOIN component c ON r.child_outcome_id=c.id WHERE r.parent_outcome_id IS NOT NULL
 UNION SELECT r.child_outcome_id FROM corrective_relations r JOIN component c ON r.parent_outcome_id=c.id WHERE r.child_outcome_id IS NOT NULL
 ) SELECT id FROM component`, []any{id})
	if err != nil {
		return nil, nil, err
	}
	ids := map[int64]bool{}
	for _, row := range rows {
		ids[rowInt(row, "id")] = true
	}
	list := correctionComponentIDs(ids)
	relations, err := correctionRows(ctx, tx, `SELECT * FROM corrective_relations WHERE child_outcome_id IN (`+list+`) OR parent_outcome_id IN (`+list+`) ORDER BY id`, nil)
	return ids, relations, err
}
func correctionComponentIDs(ids map[int64]bool) string {
	list := []string{"NULL"}
	for id := range ids {
		list = append(list, strconv.FormatInt(id, 10))
	}
	return strings.Join(list, ",")
}
func rowInt(row map[string]any, key string) int64     { v, _ := row[key].(int64); return v }
func rowString(row map[string]any, key string) string { v, _ := row[key].(string); return v }
func rationalFloat(v float64) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(v, 'g', -1, 64))
	if !ok {
		return nil, errors.New("nonfinite chain input")
	}
	return r, nil
}
func nullableRat(v *big.Rat) (any, error) {
	if v == nil {
		return nil, nil
	}
	return event3.EncodeRat(v)
}
func reviewFromRow(row map[string]any) (*event3.Review, error) {
	v := &event3.Review{Revision: rowInt(row, "revision"), PureRepair: rowInt(row, "pure_repair") == 1, Scope: rowString(row, "scope"), Rationale: rowString(row, "rationale"), Reviewer: rowString(row, "reviewer"), ComparisonBase: rowString(row, "comparison_base"), ComparisonBefore: rowString(row, "comparison_before"), ComparisonAfter: rowString(row, "comparison_after")}
	var ok bool
	v.ReviewedAt, ok = row["reviewed_at"].(time.Time)
	if !ok {
		return nil, errors.New("missing review time")
	}
	for _, entry := range []struct {
		key    string
		target **big.Rat
	}{{"original_weight", &v.Original}, {"reviewed_own_weight", &v.Own}, {"before_weight", &v.Before}, {"after_weight", &v.After}} {
		if row[entry.key] != nil {
			r, err := event3.DecodeRat(rowString(row, entry.key))
			if err != nil {
				return nil, err
			}
			*entry.target = r
		}
	}
	// Reviewer erasure preserves the reviewed sizing. Supply only an in-memory
	// nonidentifying label to the resolver; carry-forward copies the nullable row.
	if row["reviewer"] == nil {
		v.Reviewer = "erased reviewer"
	}
	return v, v.Validate()
}

// carryCorrectionReviewTx retains the latest review, including a scrubbed reviewer,
// at the resulting revision without rewriting its provenance or reviewed time.
func carryCorrectionReviewTx(ctx context.Context, tx *sql.Tx, relation, revision int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO correction_assessments(relation_id,revision,chain_version,participant_outcome_id,basis,original_weight,original_quality,reviewed_own_weight,before_weight,after_weight,marginal_credit,pure_repair,comparison_base,comparison_before,comparison_after,scope,rationale,reviewer,reviewed_at,recorded_at,rule_version)
 SELECT relation_id,?,chain_version,participant_outcome_id,basis,original_weight,original_quality,reviewed_own_weight,before_weight,after_weight,marginal_credit,pure_repair,comparison_base,comparison_before,comparison_after,scope,rationale,reviewer,reviewed_at,recorded_at,rule_version FROM correction_assessments
 WHERE relation_id=? AND revision<? AND basis='reviewed' AND participant_outcome_id=(SELECT child_outcome_id FROM corrective_relations WHERE id=?) ORDER BY revision DESC,id DESC LIMIT 1 ON CONFLICT DO NOTHING`, revision, relation, revision, relation)
	return err
}

func (d *DB) reconcileCorrectionChainTx(ctx context.Context, tx *sql.Tx, id int64) error {
	ids, relations, err := correctionComponent(ctx, tx, id)
	if err != nil {
		return err
	}
	list := correctionComponentIDs(ids)
	oldRows, err := correctionRows(ctx, tx, `SELECT * FROM correction_allocations WHERE outcome_id IN (`+list+`)`, nil)
	if err != nil {
		return err
	}
	frozen := false
	old := map[int64]map[string]any{}
	for _, row := range oldRows {
		oid := rowInt(row, "outcome_id")
		if ids[oid] {
			if rowInt(row, "frozen") == 1 {
				frozen = true
			}
			old[oid] = row
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,repo,COALESCE(pr_number,0),COALESCE(merge_commit_sha,''),developer,merged_at,weight,quality,`+outcomeDiffStatsSQL+` 1 FROM outcomes WHERE id IN (`+list+`) ORDER BY id`)
	if err != nil {
		return err
	}
	var nodes []event3.Node
	for rows.Next() {
		var n event3.Node
		var merged sql.NullTime
		var w, q float64
		var stats [3]sql.NullInt64
		var legacy bool
		var ignored int
		if err = rows.Scan(&n.ID, &n.Repo, &n.PR, &n.SHA, &n.Developer, &merged, &w, &q, &stats[0], &stats[1], &stats[2], &legacy, &ignored); err != nil {
			_ = rows.Close()
			return err
		}
		if !ids[n.ID] {
			continue
		}
		n.MergedAt = merged.Time
		n.Weight, err = rationalFloat(w)
		if err != nil {
			_ = rows.Close()
			return err
		}
		n.Quality, err = rationalFloat(q)
		if err != nil {
			_ = rows.Close()
			return err
		}
		if stats[0].Valid && stats[1].Valid && stats[2].Valid && !legacy {
			n.Effort = &event3.Effort{Additions: stats[0].Int64, Deletions: stats[1].Int64, Files: stats[2].Int64}
		}
		for _, rel := range relations {
			if rowInt(rel, "child_outcome_id") == n.ID {
				pid := rowInt(rel, "parent_outcome_id")
				if pid == 0 {
					pid = -rowInt(rel, "id")
				}
				n.Parents = append(n.Parents, event3.Parent{ID: pid, State: rowString(rel, "state")})
			}
		}
		nodes = append(nodes, n)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if len(nodes) == 0 {
		return sql.ErrNoRows
	}
	for i := range nodes {
		nodes[i].Developer, err = canonicalOfTx(ctx, tx, nodes[i].Developer)
		if err != nil {
			return err
		}
		if frozen {
			nodes[i].Parents = nil
			continue
		}
		for j := range nodes[i].Parents {
			parent := &nodes[i].Parents[j]
			reviewed, err := correctionRows(ctx, tx, `SELECT a.* FROM correction_assessments a JOIN corrective_relations r ON r.id=a.relation_id WHERE r.child_outcome_id=? AND r.parent_outcome_id=? AND a.participant_outcome_id=r.child_outcome_id AND a.basis='reviewed' AND a.revision=r.revision AND r.state NOT IN ('removed','unrelated','withdrawn') ORDER BY a.revision DESC,a.id DESC LIMIT 1`, []any{nodes[i].ID, parent.ID})
			if err != nil {
				return err
			}
			if len(reviewed) > 0 {
				parent.Review, err = reviewFromRow(reviewed[0])
				if err != nil {
					return err
				}
			}
		}
	}
	allocations, err := event3.ResolveChain(nodes)
	if err != nil {
		return err
	}
	var version int64
	if err = tx.QueryRowContext(ctx, `SELECT projection_revision+1 FROM correction_policy WHERE id=1`).Scan(&version); err != nil {
		return err
	}
	changed := false
	for _, a := range allocations {
		if rowInt(old[a.OutcomeID], "frozen") == 1 {
			continue
		}
		if frozen {
			a.Role = "follow-up"
			a.Status = "pending"
			a.Reason = "frozen-chain"
			a.Eligible = "ineligible"
			for _, rel := range relations {
				if rowInt(rel, "child_outcome_id") == a.OutcomeID {
					a.ParentID = rowInt(rel, "parent_outcome_id")
					state := rowString(rel, "state")
					a.Disputed = a.Disputed || state == "disputed" || state == "disputed-declared" || state == "withdrawn-confirmed"
				}
			}
		}

		var relation any
		var revision int64 = 1
		for _, rel := range relations {
			if rowInt(rel, "child_outcome_id") == a.OutcomeID && rowInt(rel, "parent_outcome_id") == a.ParentID && rowString(rel, "state") != "removed" && rowString(rel, "state") != "unrelated" && rowString(rel, "state") != "withdrawn" {
				relation = rel["id"]
				revision = rowInt(rel, "revision")
				break
			}
		}
		var parent any
		if a.ParentID > 0 {
			parent = a.ParentID
		}
		values := map[string]any{"allocation_parent_outcome_id": parent, "relation_id": relation, "role": a.Role, "ranking_eligibility": a.Eligible, "status": a.Status, "pending_reason": a.Reason, "provisional": int64(0), "disputed": int64(0), "rule_version": a.RuleVersion}
		if a.Provisional {
			values["provisional"] = int64(1)
		}
		if a.Disputed {
			values["disputed"] = int64(1)
		}
		for _, entry := range []struct {
			key string
			r   *big.Rat
		}{{"original_weight", a.SourceWeight}, {"original_quality", a.SourceQuality}, {"raw_weight", a.RawWeight}, {"original_term", a.OriginalTerm}, {"reviewed_own_weight", a.Own}, {"before_weight", a.Before}, {"after_weight", a.After}, {"share", a.Share}, {"projected_quality", a.Quality}, {"allocated_points", a.Points}, {"marginal_credit", a.Credit}} {
			encoded, err := event3.EncodeRat(entry.r)
			if err != nil {
				return err
			}
			values[entry.key] = encoded
		}
		same := old[a.OutcomeID] != nil
		for key, value := range values {
			same = same && reflect.DeepEqual(old[a.OutcomeID][key], value)
		}
		if same {
			continue
		}
		changed = true
		// Explicit columns preserve separate shares, quality and final points. Never
		// reconstruct one by dividing/multiplying another.
		_, err = tx.ExecContext(ctx, `INSERT INTO correction_allocations(chain_version,relation_id,outcome_id,allocation_parent_outcome_id,original_weight,original_quality,role,raw_weight,original_term,reviewed_own_weight,before_weight,after_weight,share,projected_quality,allocated_points,marginal_credit,ranking_eligibility,status,pending_reason,provisional,disputed,recorded_at,rule_version)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(outcome_id) WHERE outcome_id IS NOT NULL DO UPDATE SET chain_version=excluded.chain_version,relation_id=excluded.relation_id,allocation_parent_outcome_id=excluded.allocation_parent_outcome_id,original_weight=excluded.original_weight,original_quality=excluded.original_quality,role=excluded.role,raw_weight=excluded.raw_weight,original_term=excluded.original_term,reviewed_own_weight=excluded.reviewed_own_weight,before_weight=excluded.before_weight,after_weight=excluded.after_weight,share=excluded.share,projected_quality=excluded.projected_quality,allocated_points=excluded.allocated_points,marginal_credit=excluded.marginal_credit,ranking_eligibility=excluded.ranking_eligibility,status=excluded.status,pending_reason=excluded.pending_reason,provisional=excluded.provisional,disputed=excluded.disputed,recorded_at=excluded.recorded_at,rule_version=excluded.rule_version`, version, relation, a.OutcomeID, parent, values["original_weight"], values["original_quality"], a.Role, values["raw_weight"], values["original_term"], values["reviewed_own_weight"], values["before_weight"], values["after_weight"], values["share"], values["projected_quality"], values["allocated_points"], values["marginal_credit"], a.Eligible, a.Status, a.Reason, values["provisional"], values["disputed"], d.clock().UTC(), a.RuleVersion)
		if err != nil {
			return err
		}
		if relation != nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO correction_assessments(relation_id,revision,chain_version,participant_outcome_id,basis,original_weight,original_quality,reviewed_own_weight,before_weight,after_weight,marginal_credit,recorded_at,rule_version)
 VALUES(?,?,?,?,'provisional',?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, relation, revision, version, a.OutcomeID, values["original_weight"], values["original_quality"], values["reviewed_own_weight"], values["before_weight"], values["after_weight"], values["marginal_credit"], d.clock().UTC(), event3.ProvisionalRule)
			if err != nil {
				return err
			}
		}
	}
	if changed {
		_, err = tx.ExecContext(ctx, `UPDATE correction_policy SET projection_revision=? WHERE id=1`, version)
	}
	return err
}

// RecordCorrectionAssessment records reviewed evidence with an optimistic edge
// revision check. It does not confirm a relation. Replay must be byte-identical
// to the first evidence, including the reviewer and review timestamp.
func (d *DB) RecordCorrectionAssessment(ctx context.Context, relationID, expectedRevision int64, review event3.Review) error {
	review.ReviewedAt = review.ReviewedAt.UTC().Round(0)
	if err := review.Validate(); err != nil {
		return err
	}
	if review.Revision != expectedRevision {
		return errors.New("review revision differs from relation revision")
	}
	tx, err := beginImmediate(ctx, d.db)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := d.prepareCorrectionWriteTx(ctx, tx); err != nil {
		return err
	}
	if err = d.recordCorrectionAssessmentTx(ctx, tx, relationID, expectedRevision, review); err != nil {
		return err
	}
	if err = d.refreshCorrectionRelationTx(ctx, tx, relationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) recordCorrectionAssessmentTx(ctx context.Context, tx *sql.Tx, relationID, expectedRevision int64, review event3.Review) error {
	var child sql.NullInt64
	var revision int64
	var childRepo, parentRepo sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT child_outcome_id,revision,child_repo,parent_repo FROM corrective_relations WHERE id=?`, relationID).Scan(&child, &revision, &childRepo, &parentRepo); err != nil {
		return err
	}
	if revision != expectedRevision || !child.Valid || !childRepo.Valid || !parentRepo.Valid {
		return errors.New("stale or erased correction relation")
	}
	ids, _, err := correctionComponent(ctx, tx, child.Int64)
	if err != nil {
		return err
	}
	frozen, err := correctionRows(ctx, tx, `SELECT outcome_id FROM correction_allocations WHERE frozen=1`, nil)
	if err != nil {
		return err
	}
	for _, row := range frozen {
		if ids[rowInt(row, "outcome_id")] {
			return ErrCorrectionFrozen
		}
	}
	existing, err := correctionRows(ctx, tx, `SELECT * FROM correction_assessments WHERE relation_id=? AND revision=? AND participant_outcome_id=? AND basis='reviewed'`, []any{relationID, revision, child.Int64})
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		previous, err := reviewFromRow(existing[0])
		if err != nil {
			return err
		}
		previousSizes := []*big.Rat{previous.Original, previous.Own, previous.Before, previous.After}
		for i, size := range []*big.Rat{review.Original, review.Own, review.Before, review.After} {
			want, _ := nullableRat(size)
			got, _ := nullableRat(previousSizes[i])
			if want != got {
				return errors.New("assessment replay conflicts with preserved review sizing")
			}
		}
		sameTime := previous.ReviewedAt.Equal(review.ReviewedAt)
		previous.Original, previous.Own, previous.Before, previous.After = nil, nil, nil, nil
		review.Original, review.Own, review.Before, review.After = nil, nil, nil, nil
		previous.ReviewedAt, review.ReviewedAt = time.Time{}, time.Time{}
		if !sameTime || *previous != review {
			return errors.New("assessment replay conflicts with preserved review provenance")
		}
		return nil
	}
	values := make([]any, 0, 4)
	for _, v := range []*big.Rat{review.Original, review.Own, review.Before, review.After} {
		x, err := nullableRat(v)
		if err != nil {
			return err
		}
		values = append(values, x)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO correction_assessments(relation_id,revision,participant_outcome_id,basis,original_weight,reviewed_own_weight,before_weight,after_weight,pure_repair,comparison_base,comparison_before,comparison_after,scope,rationale,reviewer,reviewed_at,recorded_at,rule_version)
 VALUES(?,?,?,'reviewed',?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, relationID, revision, child.Int64, values[0], values[1], values[2], values[3], review.PureRepair, review.ComparisonBase, review.ComparisonBefore, review.ComparisonAfter, review.Scope, review.Rationale, review.Reviewer, review.ReviewedAt.UTC(), d.clock().UTC(), event3.ReviewedRule)
	if err != nil {
		return fmt.Errorf("record correction assessment: %w", err)
	}
	return nil
}
