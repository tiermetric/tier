package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/tiermetric/tier/internal/repoid"
)

// BranchFillLeadIn is the single lead-in for merge-time branch attribution
// (R-2026-10-11-3, amending R-2026-10-03-18).
const BranchFillLeadIn = 7 * 24 * time.Hour

func nullableOutcomeTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return normalizeTS(*t)
}

// Match insert-time resolution: any sealed period overlapping the event's UTC
// calendar month protects the whole month. Timestamp prefixes are UTC (#723).
const unsealedFillMonth = `NOT EXISTS (
 SELECT 1 FROM sealed_report
 WHERE period_start < strftime('%Y-%m-%dT00:00:00Z', substr(token_events.ts,1,7) || '-01', '+1 month')
   AND period_end > substr(token_events.ts,1,7) || '-01T00:00:00Z'
)`

// Protect the exact token band ReportDigests reads, as well as the existing
// calendar-month guard. Bind Go timestamps to preserve nanosecond comparisons.
func unsealedFillPredicate(ctx context.Context, tx *sql.Tx) (string, []any, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+sealedReportCols+` FROM sealed_report`)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = rows.Close() }()
	predicate := unsealedFillMonth
	var args []any
	for rows.Next() {
		r, err := scanSealedReport(rows)
		if err != nil {
			return "", nil, err
		}
		predicate += ` AND NOT (token_events.ts>=? AND token_events.ts<?)`
		args = append(args, normalizeTS(r.PeriodStart.Add(-AttributableWindow)), normalizeTS(r.PeriodEnd))
	}
	return predicate, args, rows.Err()
}

func fillPROutcome(ctx context.Context, tx *sql.Tx, o Outcome) error {
	if !repoid.IsReal(o.Repo) || o.IssueID == "" || IsUnattributed(o.IssueID) {
		return nil
	}
	unsealed, sealArgs, err := unsealedFillPredicate(ctx, tx)
	if err != nil {
		return err
	}
	// PR-number evidence wins, using the same server-owned rule as PR a.
	if o.PRNumber > 0 {
		_, err := tx.ExecContext(ctx, `UPDATE token_events SET issue_id=?, attribution_rule=?
 WHERE repo=? AND pr_number=? AND (`+unattributedFamilyPredicate+`)
 AND (SELECT COUNT(*) FROM outcomes WHERE repo=? AND pr_number=? AND issue_id != ''
      AND NOT (`+unattributedFamilyPredicate+`)) = 1 AND `+unsealed,
			append([]any{o.IssueID, AttributionRulePRNumberAtInsert, o.Repo, o.PRNumber, UnattributedIssueID, unattributedGlobPattern,
				o.Repo, o.PRNumber, UnattributedIssueID, unattributedGlobPattern}, sealArgs...)...)
		if err != nil {
			return err
		}
	}
	// Backfill is newest-first: retain its branch evidence for later webhooks,
	// but never credit events before predecessors are known.
	if o.Source == "backfill" || o.HeadBranchHash == nil || o.CreatedAt == nil || o.MergedAt == nil || o.CreatedAt.After(*o.MergedAt) {
		return nil
	}
	// Read all recorded windows under the outcome's write lock. Compute bounds
	// in Go to preserve nanosecond boundaries; SQLite date arithmetic rounds.
	rows, err := tx.QueryContext(ctx, `SELECT created_at, merged_at FROM outcomes
 WHERE repo=? AND head_branch_hash=? AND merged_at IS NOT NULL ORDER BY merged_at`, o.Repo, *o.HeadBranchHash)
	if err != nil {
		return err
	}
	type window struct{ start, end time.Time }
	var windows []window
	var previous, groupMerge, groupPrevious time.Time
	start := o.CreatedAt.Add(-BranchFillLeadIn)
	for rows.Next() {
		var opened sql.NullTime
		var merged time.Time
		if err := rows.Scan(&opened, &merged); err != nil {
			_ = rows.Close()
			return err
		}
		// Equal merge times share the same strictly previous merge bound.
		if !merged.Equal(groupMerge) {
			groupPrevious, groupMerge = previous, merged
		}
		if merged.Before(*o.MergedAt) && merged.After(start) {
			start = merged
		}
		if opened.Valid && !opened.Time.After(merged) {
			lower := opened.Time.Add(-BranchFillLeadIn)
			if groupPrevious.After(lower) {
				lower = groupPrevious
			}
			windows = append(windows, window{lower, merged})
		}
		previous = merged
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT id, ts FROM token_events
 WHERE repo=? AND branch_hash=? AND pr_number IS NULL AND ts>=? AND ts<=? AND (`+unattributedFamilyPredicate+`)`,
		o.Repo, *o.HeadBranchHash, normalizeTS(start), normalizeTS(*o.MergedAt), UnattributedIssueID, unattributedGlobPattern)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var ts time.Time
		if err := rows.Scan(&id, &ts); err != nil {
			_ = rows.Close()
			return err
		}
		claims := 0
		for _, w := range windows {
			if !ts.Before(w.start) && !ts.After(w.end) {
				claims++
			}
		}
		if claims == 1 {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE token_events SET issue_id=?, attribution_rule=?
 WHERE id=? AND pr_number IS NULL AND (`+unattributedFamilyPredicate+`) AND `+unsealed,
			append([]any{o.IssueID, AttributionRuleBranchAtMerge, id, UnattributedIssueID, unattributedGlobPattern}, sealArgs...)...); err != nil {
			return err
		}
	}
	return nil
}
