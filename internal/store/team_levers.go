package store

import (
	"context"
	"time"
)

// DeveloperComposition keeps raw ids until the API places costs by dated
// membership, just like DeveloperIssueCostsWindow. It never leaves the server.
type DeveloperComposition struct {
	Developer string
	ModelClassCost
}

func (d *DB) DeveloperCompositionWindow(ctx context.Context, since, until time.Time, scope RepoScope) ([]DeveloperComposition, error) {
	return reader{d.db}.DeveloperCompositionWindow(ctx, since, until, scope)
}

func (r reader) DeveloperCompositionWindow(ctx context.Context, since, until time.Time, scope RepoScope) ([]DeveloperComposition, error) {
	where, args := tsWindow(since, until)
	scopeSQL, scopeArgs := scope.clause()
	args = append([]any{UnattributedIssueID, unattributedGlobPattern}, args...)
	args = append(args, scopeArgs...)
	rows, err := r.queryEffectiveWindow(ctx, since, until, scope, `
 SELECT developer, host, model, SUM(cost_micro),
 SUM(CASE WHEN `+unattributedFamilyPredicate+` THEN cost_micro ELSE 0 END),
 SUM(input_tok), SUM(output_tok), SUM(cache_read), SUM(cache_write_5m), SUM(cache_write_1h)
 FROM token_events INDEXED BY idx_token_events_ts_id
 WHERE `+where+scopeSQL+`
 GROUP BY developer, host, model ORDER BY developer, host, model`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DeveloperComposition
	for rows.Next() {
		var c DeveloperComposition
		if err := rows.Scan(&c.Developer, &c.Host, &c.Model, &c.CostMicro, &c.UnattributedCostMicro,
			&c.InputTok, &c.OutputTok, &c.CacheRead, &c.CacheWrite5m, &c.CacheWrite1h); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
