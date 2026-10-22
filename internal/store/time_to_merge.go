package store

import (
	"context"
	"time"
)

// TimeToMergeOutcomesWindow reads timestamp evidence without changing the hot
// scoring read or its projections. The window, scope and order are exactly
// AllOutcomesWindow's; callers join by ID inside the same read snapshot.
func (d *DB) TimeToMergeOutcomesWindow(ctx context.Context, since, until time.Time, scope RepoScope) ([]Outcome, error) {
	tx, release, err := beginRead(ctx, d.db)
	if err != nil {
		return nil, err
	}
	defer release()
	return (reader{tx}).TimeToMergeOutcomesWindow(ctx, since, until, scope)
}

func (r reader) TimeToMergeOutcomesWindow(ctx context.Context, since, until time.Time, scope RepoScope) ([]Outcome, error) {
	where, args := tsWindow(since, until)
	scopeSQL, scopeArgs := scope.clause()
	args = append(args, scopeArgs...)
	rows, err := r.q.QueryContext(ctx, `SELECT id, created_at, merged_at
		FROM outcomes WHERE `+where+scopeSQL+scoringOrderSQL, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Outcome
	for rows.Next() {
		var o Outcome
		if err := rows.Scan(&o.ID, &o.CreatedAt, &o.MergedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
