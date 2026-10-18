package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const RecoveryNotice = "scores are as of %s and may be revised as branches merge"

// Merge bounds use the strictly previous merge, including outcomes without a
// usable issue. Equal-time claimants share that predecessor. Date shifting
// preserves the stored subsecond suffix rather than rounding through datetime().
func recoveryAttributionCTE(ctx context.Context, index string) string {
	where := "1=1"
	if window, ok := ctx.Value(effectiveWindowKey{}).(effectiveWindow); ok {
		where = window.where
	}
	return fmt.Sprintf(`WITH
 event_population AS MATERIALIZED (SELECT * FROM token_events%s WHERE %s),
 branch_history AS (
 SELECT outcomes.*, MAX(merged_at) OVER (
 PARTITION BY repo,head_branch_hash ORDER BY merged_at
 GROUPS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS previous_merge
 FROM outcomes WHERE merged_at IS NOT NULL AND pr_number>0 AND source IN ('github-webhook','backfill')
 ),
 recovery_windows AS (
 SELECT *, MAX(COALESCE(previous_merge,''),date(substr(created_at,1,10),'-%d days')||substr(created_at,11)) AS window_start
 FROM branch_history WHERE created_at IS NOT NULL AND created_at<=merged_at
 ),
 eligible_population AS (
 SELECT * FROM event_population WHERE repo!='unqualified' AND instr(repo,'/')>0
 AND issue_id!='`+UnattributedMainBucket+`' AND issue_id!='`+UnattributedForeignRepoBucket+`'
 AND (`+unattributedIssueSQL+`)
 ),
 recovery_matches AS (
 SELECT e.id AS event_id,o.id,o.issue_id,o.source,e.pr_number IS NOT NULL AS pr_match
 FROM eligible_population e JOIN recovery_windows o
 ON o.repo=e.repo AND o.head_branch_hash=e.branch_hash
 AND (e.pr_number IS NULL OR o.pr_number=e.pr_number)
 AND e.ts>=o.window_start AND e.ts<=o.merged_at
 WHERE e.branch_hash IS NOT NULL
 UNION ALL
 SELECT e.id AS event_id,o.id,o.issue_id,o.source,1 AS pr_match
 FROM eligible_population e JOIN recovery_windows o
 ON o.repo=e.repo AND o.pr_number=e.pr_number AND o.head_branch_hash IS NOT NULL
 AND e.ts>=o.window_start AND e.ts<=o.merged_at
 WHERE e.branch_hash IS NULL AND e.pr_number IS NOT NULL
 ),
 recovery_claims AS (
 SELECT event_id,COUNT(*) AS claimants,MIN(issue_id) AS recovered_issue,
 MAX(source='backfill') AS backfill,MAX(pr_match) AS pr_match
 FROM recovery_matches GROUP BY event_id
 ),
 protected_events AS (
 SELECT e.id FROM event_population e JOIN sealed_report s
 ON (s.period_start < strftime('%%Y-%%m-%%dT00:00:00Z',substr(e.ts,1,7)||'-01','+1 month')
 AND s.period_end > substr(e.ts,1,7)||'-01T00:00:00Z')
 OR (e.ts >= date(substr(s.period_start,1,10),'-%d days')||' '||substr(s.period_start,12,8)||' +0000 UTC' AND e.ts<replace(replace(s.period_end,'T',' '),'Z',' +0000 UTC'))
 GROUP BY e.id
 ),
 effective_events AS (
 SELECT %s, e.issue_id AS stored_issue_id,
 CASE WHEN p.id IS NULL AND c.claimants=1 AND NOT (`+strings.ReplaceAll(unattributedIssueSQL, "issue_id", "c.recovered_issue")+`) THEN c.recovered_issue ELSE e.issue_id END AS issue_id,
 CASE WHEN NOT (`+strings.ReplaceAll(unattributedIssueSQL, "issue_id", "e.issue_id")+`)
 THEN CASE WHEN e.attribution_rule IN (`+fillRulesSQL+`) THEN 'existing_fill' ELSE 'stored_assignment' END
 WHEN p.id IS NULL AND c.claimants=1 AND NOT (`+strings.ReplaceAll(unattributedIssueSQL, "issue_id", "c.recovered_issue")+`) THEN 'read_time_recovery' ELSE 'unresolved' END AS attribution_layer,
 COALESCE(c.claimants,0) AS recovery_claimants, COALESCE(c.backfill,0) AS recovery_backfill,
 COALESCE(c.pr_match,0) AS recovery_pr_match, p.id IS NOT NULL AS recovery_protected,
 COALESCE(c.claimants=1 AND (`+strings.ReplaceAll(unattributedIssueSQL, "issue_id", "c.recovered_issue")+`),0) AS recovery_claimant_without_issue
 FROM event_population e LEFT JOIN recovery_claims c ON c.event_id=e.id LEFT JOIN protected_events p ON p.id=e.id
 ) `, index, where, int(BranchFillLeadIn.Hours()/24), int(AttributableWindow.Hours()/24), "e."+strings.ReplaceAll(effectiveEventColumns, ", ", ", e."))
}

func RecoveryDisclosure(ctx context.Context, asOf time.Time) string {
	if !recoveryEnabled(ctx) {
		return ""
	}
	return fmt.Sprintf(RecoveryNotice, asOf.UTC().Format(time.RFC3339))
}
