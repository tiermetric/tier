package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tiermetric/tier/internal/corrects"
	"github.com/tiermetric/tier/internal/repoid"
)

// ErrCorrectionDeclarationIgnored is the unchanged-ignored replay status: the
// canonical merge already has a different declaration, whose evidence is preserved.
var ErrCorrectionDeclarationIgnored = errors.New("correction declaration: unchanged-ignored (canonical declaration changed)")

// RecordCorrectionDeclaration stores fetch-time evidence and one edge per parent,
// without requiring issue attribution or an outcome. Replay preserves first evidence.
// A changed state or parent set returns the existing id and
// ErrCorrectionDeclarationIgnored; identical declarations remain successful no-ops.
// Uses the webhook-delivery write lock without the request-path 250ms cap.
// Text is rejected above 4096 bytes; callers choose a bounded excerpt, never a PR body.
func (d *DB) RecordCorrectionDeclaration(ctx context.Context, o Outcome, declaration corrects.Declaration, text, provenance string) (int64, error) {
	tx, err := beginImmediate(ctx, d.db)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := d.prepareCorrectionWriteTx(ctx, tx); err != nil {
		return 0, err
	}
	id, err := d.recordCorrectionDeclarationTx(ctx, tx, o, declaration, text, provenance)
	if err != nil {
		return id, err
	}
	if err = d.refreshCorrectionOutcomeTx(ctx, tx, o); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// recordCorrectionDeclarationTx leaves commit and rollback to the transaction owner.
func (d *DB) recordCorrectionDeclarationTx(ctx context.Context, tx *sql.Tx, o Outcome, declaration corrects.Declaration, text, provenance string) (int64, error) {
	repo, validRepo := repoid.Canonical(o.Repo)
	if o.PRNumber <= 0 || o.MergeCommitSHA == "" || !validRepo || len(text) > 4096 || len(provenance) > 128 {
		return 0, fmt.Errorf("correction declaration: canonical merge identity and bounded evidence required")
	}
	if (declaration.State == corrects.StateParents) != (len(declaration.Parents) > 0) || len(declaration.Parents) > corrects.MaxParents {
		return 0, fmt.Errorf("correction declaration: inconsistent parents")
	}
	for _, parent := range declaration.Parents {
		if parent <= 0 {
			return 0, fmt.Errorf("correction declaration: invalid parent")
		}
		if parent == o.PRNumber {
			declaration = corrects.Declaration{State: corrects.StateInvalid}
			break
		}
	}
	now := d.clock().UTC()
	a, b, f := o.DiffStats()
	inserted, err := tx.ExecContext(ctx, `INSERT INTO correction_declarations(outcome_id,developer,repo,pr_number,merge_commit_sha,merged_at,opened_at,additions,deletions,changed_files,declaration_state,supporting_text,provenance,recorded_at,rule_version)
 VALUES(NULL,NULLIF(?,''),?,?,?,?,?,?,?,?,?,?,?,?, 'event3-v1') ON CONFLICT DO NOTHING`, o.Developer, repo, o.PRNumber, o.MergeCommitSHA, nullableOutcomeTime(o.MergedAt), nullableOutcomeTime(o.CreatedAt), a, b, f, declaration.State, text, provenance, now)
	if err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM correction_declarations WHERE repo=? AND pr_number=? AND merge_commit_sha=?`, repo, o.PRNumber, o.MergeCommitSHA).Scan(&id); err != nil {
		return 0, err
	}
	n, err := inserted.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		var state corrects.State
		if err := tx.QueryRowContext(ctx, `SELECT declaration_state FROM correction_declarations WHERE id=?`, id).Scan(&state); err != nil {
			return 0, err
		}
		parents, err := correctionRows(ctx, tx, `SELECT parent_pr_number FROM corrective_relations WHERE child_declaration_id=?`, []any{id})
		if err != nil {
			return 0, err
		}
		parentSet := make(map[int64]bool, len(declaration.Parents))
		for _, parent := range declaration.Parents {
			parentSet[int64(parent)] = true
		}
		matches := state == declaration.State && len(parents) == len(parentSet)
		for _, parent := range parents {
			number, ok := parent["parent_pr_number"].(int64)
			matches = matches && ok && parentSet[number]
		}
		if !matches {
			return id, ErrCorrectionDeclarationIgnored
		}
		return id, attachCorrectionOutcomeTx(ctx, tx, o, now)
	}
	for _, parent := range declaration.Parents {
		_, err = tx.ExecContext(ctx, `INSERT INTO corrective_relations(child_declaration_id,child_outcome_id,
 child_developer,child_repo,parent_repo,child_pr_number,parent_pr_number,child_merge_commit_sha,child_merged_at,
 state,resolution_reason,revision,recorded_at,updated_at,rule_version,supporting_text)
 SELECT id,outcome_id,developer,repo,repo,pr_number,?,merge_commit_sha,merged_at,
 'pending','missing-parent',1,?,?,rule_version,supporting_text
 FROM correction_declarations WHERE id=? AND declaration_state='parents' ON CONFLICT DO NOTHING`, parent, now, now, id)
		if err != nil {
			return 0, err
		}
	}
	return id, attachCorrectionOutcomeTx(ctx, tx, o, now)
}

// Outcome links are resolved from canonical identities, never caller-supplied IDs.
// This runs inside RecordPROutcome even when its outcome insert was a replay.
func attachCorrectionOutcomeTx(ctx context.Context, tx *sql.Tx, o Outcome, now time.Time) error {
	repo, _ := repoid.Canonical(o.Repo)
	_, err := tx.ExecContext(ctx, `UPDATE correction_declarations SET outcome_id=(
 SELECT id FROM outcomes WHERE repo=? AND pr_number=? AND merge_commit_sha=?)
 WHERE repo=? AND pr_number=? AND merge_commit_sha=?`, repo, o.PRNumber, o.MergeCommitSHA,
		repo, o.PRNumber, o.MergeCommitSHA)
	if err != nil {
		return err
	}
	return reconcileCorrectionRelationsTx(ctx, tx, o, now)
}

// UNION keeps both endpoint lookups indexed even when the planner would prefer
// filtering all eligible states for an OR. It also deduplicates matching edges.
const reconcileCorrectionRelationsSQL = `WITH affected AS (
 SELECT id FROM corrective_relations AS child_edge
 WHERE child_repo=@repo AND child_pr_number=@pr AND child_merge_commit_sha IS NOT NULL
 AND parent_repo IS NOT NULL AND parent_pr_number IS NOT NULL
 UNION
 SELECT id FROM corrective_relations AS parent_edge WHERE parent_repo=@repo AND parent_pr_number=@pr
), endpoints AS (
 SELECT r.id, r.state AS old_state, r.parent_outcome_id AS old_pid, r.parent_developer AS old_pdev, r.parent_merge_commit_sha AS old_psha, r.parent_declaration_id AS old_pdid, r.parent_merged_at AS old_pmerged, c.outcome_id AS cid, COALESCE(c.merged_at,co.merged_at) AS cmerged, p.id AS pdid, COALESCE(p.outcome_id,po.id) AS pid,
 COALESCE(p.developer,po.developer) AS pdev, COALESCE(p.merge_commit_sha,po.merge_commit_sha) AS psha,
 COALESCE(p.merged_at,po.merged_at) AS pmerged,
 EXISTS(SELECT 1 FROM outcome_exclusions x WHERE x.exclusion_merge_commit_sha=r.child_merge_commit_sha) AS cx,
 EXISTS(SELECT 1 FROM outcome_exclusions x WHERE x.exclusion_repo=r.parent_repo AND x.exclusion_pr_number=r.parent_pr_number) AS px
 FROM affected a JOIN corrective_relations r ON r.id=a.id
 JOIN correction_declarations c ON c.id=r.child_declaration_id
 LEFT JOIN outcomes co ON co.id=c.outcome_id
 LEFT JOIN correction_declarations p ON p.id=(SELECT id FROM correction_declarations
 WHERE repo=r.parent_repo AND pr_number=r.parent_pr_number ORDER BY merged_at DESC,merge_commit_sha DESC LIMIT 1)
 LEFT JOIN outcomes po ON po.id=(SELECT id FROM outcomes WHERE repo=r.parent_repo AND pr_number=r.parent_pr_number
 AND (p.merge_commit_sha IS NULL OR merge_commit_sha=p.merge_commit_sha) ORDER BY merged_at DESC,merge_commit_sha DESC LIMIT 1)
 WHERE r.child_repo IS NOT NULL AND r.child_pr_number IS NOT NULL AND r.child_merge_commit_sha IS NOT NULL
 AND r.parent_repo IS NOT NULL AND r.parent_pr_number IS NOT NULL AND r.state IN ('pending','declared','excluded','disputed-declared','confirmed','disputed','withdrawn-confirmed')
), protected AS (
 SELECT *, old_state IN ('disputed-declared','confirmed','disputed','withdrawn-confirmed') AS retained,
 (old_pid IS NOT NULL AND pid IS NOT NULL AND old_pid != pid)
 OR (old_pdev IS NOT NULL AND pdev IS NOT NULL AND old_pdev != pdev)
 OR (old_psha IS NOT NULL AND psha IS NOT NULL AND old_psha != psha) AS conflict FROM endpoints
), resolved AS (
 SELECT *, CASE WHEN cx THEN NULL ELSE cid END AS child_id,
 CASE WHEN px THEN NULL WHEN retained AND conflict THEN old_pid WHEN retained THEN COALESCE(old_pid,pid) ELSE pid END AS parent_id,
 CASE WHEN retained AND conflict THEN old_pdid WHEN retained THEN COALESCE(old_pdid,pdid) ELSE pdid END AS parent_did,
 CASE WHEN retained AND conflict THEN old_pdev WHEN retained THEN COALESCE(old_pdev,pdev) ELSE pdev END AS parent_dev,
 CASE WHEN retained AND conflict THEN old_psha WHEN retained THEN COALESCE(old_psha,psha) ELSE psha END AS parent_sha,
 CASE WHEN retained AND conflict THEN old_pmerged WHEN retained THEN COALESCE(old_pmerged,pmerged) ELSE pmerged END AS parent_merged,
 CASE WHEN cx OR px THEN 'excluded' WHEN retained AND conflict THEN CASE WHEN old_state='disputed-declared' THEN old_state ELSE 'disputed' END
 WHEN retained THEN old_state WHEN pid IS NULL OR cid IS NULL OR cmerged IS NULL OR pmerged IS NULL THEN 'pending' ELSE 'declared' END AS new_state,
 CASE WHEN cx THEN 'excluded-child' WHEN px THEN 'excluded-parent' WHEN retained AND conflict THEN 'parent-identity-conflict' WHEN pid IS NULL THEN 'missing-parent'
 WHEN cid IS NULL THEN 'missing-child-outcome' WHEN cmerged IS NULL OR pmerged IS NULL THEN 'missing-occurrence' END AS reason FROM protected
)
 UPDATE corrective_relations AS r SET
 child_outcome_id=e.child_id,child_merged_at=e.cmerged,parent_declaration_id=e.parent_did,parent_outcome_id=e.parent_id,
 parent_developer=e.parent_dev,parent_merge_commit_sha=e.parent_sha,parent_merged_at=e.parent_merged,
 state=e.new_state,resolution_reason=e.reason,updated_at=@now,
 revision=r.revision+EXISTS(SELECT 1 FROM correction_audit a WHERE a.relation_id=r.id)
 FROM resolved e WHERE r.id=e.id AND
 (r.child_outcome_id,r.child_merged_at,r.parent_declaration_id,r.parent_outcome_id,r.parent_developer,r.parent_merge_commit_sha,r.parent_merged_at,r.state,r.resolution_reason)
 IS NOT (e.child_id,e.cmerged,e.parent_did,e.parent_id,e.parent_dev,e.parent_sha,e.parent_merged,e.new_state,e.reason)`

// Reconcile intact active edges without resetting audited lifecycle dispositions.
// Late evidence can fill confirmed/disputed endpoints; erasure is irreversible.
// Canonical occurrence/SHA ordering, rather than insertion IDs, selects parents.
func reconcileCorrectionRelationsTx(ctx context.Context, tx *sql.Tx, o Outcome, now time.Time) error {
	repo, _ := repoid.Canonical(o.Repo)
	args := []any{sql.Named("repo", repo), sql.Named("pr", o.PRNumber), sql.Named("now", now.UTC())}
	before, err := correctionRows(ctx, tx, `SELECT * FROM corrective_relations WHERE (child_repo=@repo AND child_pr_number=@pr) OR (parent_repo=@repo AND parent_pr_number=@pr)`, args[:2])
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, reconcileCorrectionRelationsSQL, args...)
	if err != nil {
		return err
	}
	// Reconciliation may fill missing endpoints but cannot change an already bound
	// retained endpoint. Carry reviewed sizing before any projection recomputation.
	recompute := map[int64]bool{}
	for _, prior := range before {
		rows, e := correctionRows(ctx, tx, `SELECT * FROM corrective_relations WHERE id=?`, []any{prior["id"]})
		if e != nil {
			return e
		}
		current := rows[0]
		if current["revision"] == prior["revision"] {
			continue
		}
		state := rowString(prior, "state")
		if state == "confirmed" || state == "disputed" || state == "withdrawn-confirmed" {
			if err := carryCorrectionReviewTx(ctx, tx, rowInt(current, "id"), rowInt(current, "revision")); err != nil {
				return err
			}
		}
		if current["child_outcome_id"] != prior["child_outcome_id"] || current["parent_outcome_id"] != prior["parent_outcome_id"] || rowString(current, "resolution_reason") == "parent-identity-conflict" {
			for _, row := range []map[string]any{prior, current} {
				for _, key := range []string{"child_outcome_id", "parent_outcome_id"} {
					if id := rowInt(row, key); id > 0 {
						recompute[id] = true
					}
				}
			}
		}
	}
	// Recompute both sides of a membership change, including a detached old parent.
	// Unprojected chains remain the responsibility of the scoring-policy resolver.
	projectionDB := &DB{now: func() time.Time { return now }}
	for id := range recompute {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM correction_allocations WHERE outcome_id=?`, id).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			continue
		}
		var live int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outcomes WHERE id=?`, id).Scan(&live); err != nil {
			return err
		}
		if live > 0 {
			if err := projectionDB.reconcileCorrectionChainTx(ctx, tx, id); err != nil {
				return err
			}
		}
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO correction_audit(relation_id,revision,action,actor,actor_role,recorded_at,rule_version,
 child_declaration_id,parent_declaration_id,child_outcome_id,parent_outcome_id,child_developer,parent_developer,child_repo,parent_repo,child_pr_number,parent_pr_number,child_merge_commit_sha,parent_merge_commit_sha,child_merged_at,parent_merged_at,reason_text)
 SELECT id,revision,CASE WHEN revision=1 THEN 'declared' ELSE 'reconciled' END,
 CASE WHEN revision=1 THEN child_developer END,CASE WHEN revision=1 THEN 'declarer' ELSE 'system' END,@now,rule_version,
 child_declaration_id,parent_declaration_id,child_outcome_id,parent_outcome_id,child_developer,parent_developer,child_repo,parent_repo,child_pr_number,parent_pr_number,child_merge_commit_sha,parent_merge_commit_sha,child_merged_at,parent_merged_at,
 CASE WHEN revision=1 THEN supporting_text ELSE resolution_reason END
 FROM corrective_relations WHERE ((child_repo=@repo AND child_pr_number=@pr) OR (parent_repo=@repo AND parent_pr_number=@pr))
 AND child_repo IS NOT NULL AND parent_repo IS NOT NULL AND state IN ('pending','declared','excluded','disputed-declared','confirmed','disputed','withdrawn-confirmed') ON CONFLICT DO NOTHING`, args...)
	return err
}

// CorrectionDeclarationByPR uses the canonical merge identity, never an issue number.
func (d *DB) CorrectionDeclarationByPR(ctx context.Context, repo string, pr int, sha string) (map[string]any, bool, error) {
	repo, _ = repoid.Canonical(repo)
	rows, err := correctionRows(ctx, d.db, `SELECT * FROM correction_declarations WHERE repo=? AND pr_number=? AND merge_commit_sha=?`, []any{repo, pr, sha})
	if err != nil || len(rows) == 0 {
		return nil, false, err
	}
	return rows[0], true, nil
}

// CorrectionRelationsByOutcome finds either endpoint, including multiple parents.
func (d *DB) CorrectionRelationsByOutcome(ctx context.Context, id int64) ([]map[string]any, error) {
	return correctionRows(ctx, d.db, `SELECT * FROM corrective_relations WHERE child_outcome_id=? OR parent_outcome_id=? ORDER BY id`, []any{id, id})
}

// CorrectionRelationsByPR also finds unresolved endpoints that have no outcome.
func (d *DB) CorrectionRelationsByPR(ctx context.Context, repo string, pr int) ([]map[string]any, error) {
	repo, _ = repoid.Canonical(repo)
	return correctionRows(ctx, d.db, `SELECT * FROM corrective_relations WHERE (child_repo=? AND child_pr_number=?) OR (parent_repo=? AND parent_pr_number=?) ORDER BY id`, []any{repo, pr, repo, pr})
}
