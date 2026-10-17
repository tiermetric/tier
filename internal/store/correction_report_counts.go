package store

import (
	"context"
	"fmt"
	"time"
)

// CorrectionReportCounts distinguishes raw coverage/activity from eligible
// ranking samples without changing any existing digest's row population.
type CorrectionReportCounts struct {
	OutcomeN int64 `json:"outcome_n"`
	SampleN  int64 `json:"sample_n"`
}

func (r reader) CorrectionReportCounts(ctx context.Context, since, until time.Time, scope RepoScope, event3 bool) (CorrectionReportCounts, error) {
	where, args := digestPredicate(since, until, scope)
	// The subquery keeps the common scope predicate unambiguous. Flag-on
	// reads refuse missing projections, matching EffectiveCorrectionValues.
	query := `SELECT COUNT(*),COALESCE(SUM(CASE WHEN ?=0 OR a.outcome_id IS NULL OR a.ranking_eligibility='eligible' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN a.outcome_id IS NULL OR a.rule_version NOT IN ('event3-v1','event3-v2') THEN 1 ELSE 0 END),0) FROM (SELECT id FROM outcomes WHERE ` + where + `) o LEFT JOIN correction_allocations a ON a.outcome_id=o.id`
	args = append([]any{event3}, args...)
	var counts CorrectionReportCounts
	var unresolved int64
	err := r.q.QueryRowContext(ctx, query, args...).Scan(&counts.OutcomeN, &counts.SampleN, &unresolved)
	if err == nil && event3 && unresolved > 0 {
		return CorrectionReportCounts{}, fmt.Errorf("event 3 projection unresolved for %d outcomes", unresolved)
	}
	return counts, err
}
