package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/tiermetric/tier/internal/repoid"
)

// The composite key starts with the trusted exclusion repo; a future tenant_id can lead it
// mechanically. id is the original outcomes.id, never a new surrogate.
// The exclusion key is separate from nullable original PR/SHA fields so a
// whole legacy row can be retained without rewriting its NULLs.
// Tombstones have NULL id and neutral outcome fields; moved rows retain every
// stored outcome column, including nullable values and provenance.
const schemaOutcomeExclusions = `
CREATE TABLE IF NOT EXISTS outcome_exclusions (
 repo TEXT NOT NULL,
 exclusion_repo TEXT NOT NULL,
 author_developer TEXT NOT NULL,
 exclusion_author_type TEXT,
 pr_number INTEGER,
 merge_commit_sha TEXT,
 exclusion_pr_number INTEGER NOT NULL,
 exclusion_merge_commit_sha TEXT NOT NULL,
 id INTEGER,
 developer TEXT NOT NULL,
 issue_id TEXT NOT NULL DEFAULT '',
 weight REAL NOT NULL DEFAULT 0,
 quality REAL NOT NULL DEFAULT 1,
 weight_source TEXT NOT NULL DEFAULT 'legacy',
 additions INTEGER,
 deletions INTEGER,
 changed_files INTEGER,
 source TEXT NOT NULL DEFAULT 'github-webhook',
 push_day TEXT,
 work_type TEXT NOT NULL DEFAULT 'feature',
 work_type_source TEXT NOT NULL DEFAULT 'default',
 ts DATETIME NOT NULL,
 author_type TEXT,
 head_branch_hash TEXT,
 created_at DATETIME,
 merged_at DATETIME,
 reason TEXT NOT NULL,
 evidence TEXT NOT NULL,
 actor TEXT NOT NULL,
 excluded_at DATETIME NOT NULL,
 PRIMARY KEY (exclusion_repo, exclusion_pr_number, exclusion_merge_commit_sha)
);
CREATE INDEX IF NOT EXISTS idx_outcome_exclusions_sha ON outcome_exclusions(exclusion_merge_commit_sha);
CREATE INDEX IF NOT EXISTS idx_outcome_exclusions_developer ON outcome_exclusions(developer);
CREATE INDEX IF NOT EXISTS idx_outcome_exclusions_author_developer ON outcome_exclusions(author_developer);
`

// ExcludePROutcome is the audited exclusion mechanism for trusted GitHub PR
// evidence. It moves any existing PR outcome whole, persists an author-owned
// tombstone even without issue attribution, and reconciles push credit under
// the same unbounded write lock as webhook ingestion. Replays change nothing.
func (d *DB) ExcludePROutcome(ctx context.Context, o Outcome, evidence, actor string) error {
	_, err := d.excludePROutcome(ctx, o, evidence, actor, nil)
	return err
}

// ApplyPRExclusion checks the approved identity under the exclusion write lock.
// Each row commits independently. A matching replay is a no-op; missing (including
// erased) or changed rows are stale and must never be recreated by ingestion.
// evidence includes the approved plan digest in the existing exclusion audit row.
func (d *DB) ApplyPRExclusion(ctx context.Context, expected PRExclusionIdentity, evidence, actor string) (bool, error) {
	return d.excludePROutcome(ctx, Outcome{PRNumber: expected.PR, MergeCommitSHA: expected.MergeSHA}, evidence, actor, &expected)
}

func (d *DB) excludePROutcome(ctx context.Context, o Outcome, evidence, actor string, expected *PRExclusionIdentity) (applied bool, err error) {
	if o.PRNumber <= 0 || o.MergeCommitSHA == "" || evidence == "" || actor == "" {
		return false, errors.New("exclude PR outcome: PR number, merge SHA, evidence and actor required")
	}
	tx, err := beginImmediate(ctx, d.db)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if expected != nil {
		err := tx.QueryRowContext(ctx, `SELECT id, repo, pr_number, merge_commit_sha, developer, issue_id, ts,
 COALESCE(author_type, '') FROM outcomes WHERE id=? AND repo=? AND pr_number=? AND merge_commit_sha=?`,
			expected.OutcomeID, expected.Repo, expected.PR, expected.MergeSHA).Scan(
			&o.ID, &o.Repo, &o.PRNumber, &o.MergeCommitSHA, &o.Developer, &o.IssueID, &o.Timestamp, &o.AuthorType)
		if errors.Is(err, sql.ErrNoRows) {
			var replay bool
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM outcome_exclusions
 WHERE id=? AND repo=? AND pr_number=? AND merge_commit_sha=? AND evidence=?)`,
				expected.OutcomeID, expected.Repo, expected.PR, expected.MergeSHA, evidence).Scan(&replay)
			if err == nil && !replay {
				err = fmt.Errorf("stale exclusion plan: outcome %d (%s PR %d) changed, was excluded by another plan, or was erased; regenerate and approve a dry run", expected.OutcomeID, expected.Repo, expected.PR)
			}
			return false, err
		}
		if err != nil {
			return false, err
		}
	}
	if err := d.prepareCorrectionWriteTx(ctx, tx); err != nil {
		return false, err
	}
	// Reuse normal PR insertion/fill before the move so arrival order cannot
	// change token attribution. Without an issue, ingestion would not insert.
	if expected == nil && o.IssueID != "" && !IsUnattributed(o.IssueID) {
		inserted, err := insertOutcomeRow(ctx, tx, o)
		if err != nil {
			return false, fmt.Errorf("exclude PR outcome: insert: %w", err)
		}
		if inserted {
			if err := fillPROutcome(ctx, tx, o); err != nil {
				return false, fmt.Errorf("exclude PR outcome: fill: %w", err)
			}
		}
	}
	touched, err := correctionOutcomeIDsTx(ctx, tx, o)
	if err != nil {
		return false, err
	}
	now := normalizeTS(d.clock())
	exclusionRepo := normalizeRepo(o.Repo)
	if canonical, ok := repoid.Canonical(o.Repo); ok {
		exclusionRepo = canonical
	}
	// SHA identity is install-wide, matching the existing PR unique key and push
	// reconciliation. A missing SHA cannot identify a merge and is refused.
	res, err := tx.ExecContext(ctx, `
 INSERT INTO outcome_exclusions
 (id, developer, issue_id, pr_number, weight, quality, merge_commit_sha,
 weight_source, additions, deletions, changed_files, source, push_day,
 work_type, work_type_source, repo, ts, author_type, head_branch_hash, created_at, merged_at,
 reason, evidence, actor, excluded_at, exclusion_pr_number, exclusion_merge_commit_sha, exclusion_repo, author_developer, exclusion_author_type)
 SELECT id, developer, issue_id, pr_number, weight, quality, merge_commit_sha,
 weight_source, additions, deletions, changed_files, source, push_day,
 work_type, work_type_source, repo, ts, author_type, head_branch_hash, created_at, merged_at,
 'github-revert-pr', ?, ?, ?, ?, ?, ?, ?, NULLIF(?, '') FROM outcomes
 WHERE merge_commit_sha = ?
 ON CONFLICT DO NOTHING`, evidence, actor, now, o.PRNumber, o.MergeCommitSHA,
		exclusionRepo, o.Developer, normalizeAuthorType(o.AuthorType), o.MergeCommitSHA)
	if err != nil {
		return false, fmt.Errorf("exclude PR outcome: move: %w", err)
	}
	moved, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	res, err = tx.ExecContext(ctx, `DELETE FROM outcomes
 WHERE merge_commit_sha = ?`, o.MergeCommitSHA)
	if err != nil {
		return false, err
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if moved != deleted {
		return false, fmt.Errorf("exclude PR outcome: moved %d rows but deleted %d", moved, deleted)
	}
	res, err = tx.ExecContext(ctx, `INSERT INTO outcome_exclusions
 (repo, pr_number, merge_commit_sha, developer, ts, author_type, source, reason, evidence, actor, excluded_at, exclusion_pr_number, exclusion_merge_commit_sha, exclusion_repo, author_developer, exclusion_author_type)
 VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), ?, 'github-revert-pr', ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''))
 ON CONFLICT DO NOTHING`, exclusionRepo, o.PRNumber, o.MergeCommitSHA, o.Developer,
		normalizeTS(o.Timestamp), normalizeAuthorType(o.AuthorType), actor, evidence, actor, now, o.PRNumber, o.MergeCommitSHA,
		exclusionRepo, o.Developer, normalizeAuthorType(o.AuthorType))
	if err != nil {
		return false, fmt.Errorf("exclude PR outcome: tombstone: %w", err)
	}
	added, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if ValidPushCommitID(o.MergeCommitSHA) {
		if err := supersedePushCommit(ctx, tx, o.MergeCommitSHA); err != nil {
			return false, err
		}
	}
	// A seal computed from credit before the move must fail the same snapshot
	// epoch check as an erase. Later DSAR erasure bumps it again atomically.
	if moved+added > 0 {
		if err := bumpEraseEpoch(ctx, tx); err != nil {
			return false, err
		}
	}
	if err := attachCorrectionOutcomeTx(ctx, tx, o, d.clock()); err != nil {
		return false, fmt.Errorf("exclude PR outcome: corrections: %w", err)
	}
	if err := d.refreshEnabledCorrectionsTx(ctx, tx, touched...); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
