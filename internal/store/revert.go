package store

import (
	"context"
	"time"

	"github.com/tiermetric/tier/internal/repoid"
)

// RevertIssueCandidates resolves only qualified, same-repository outcomes in
// the inclusive occurrence window. The first query is bounded by LIMIT 2:
// one row is resolvable; two mean ambiguity. Only ambiguity loads the remaining
// candidates, so each receives the durable checked-but-unresolved marker.
// Legacy rows are diagnostics only and can never become penalty targets.
// Both reads share a snapshot so insertion order and concurrent writes cannot
// turn a multi-outcome issue into a supposedly unique target.
func (d *DB) RevertIssueCandidates(ctx context.Context, repo, issue string, since, until time.Time) (candidates []Outcome, legacyOnly bool, err error) {
	if canonical, ok := repoid.Canonical(repo); ok {
		repo = canonical
	} else {
		repo = repoid.Unqualified
	}
	if issue == "" {
		return nil, false, nil
	}
	tx, release, err := beginRead(ctx, d.db)
	if err != nil {
		return nil, false, err
	}
	defer release()
	const query = `SELECT id, developer, issue_id, weight, quality, ts, repo
 FROM outcomes WHERE repo = ? AND issue_id = ? AND ts >= ? AND ts <= ? ORDER BY id LIMIT ?`
	// A suffix-free lower bound includes both SQLite text and driver time.Time
	// encodings at equality, retaining nanoseconds without wrapping the indexed ts.
	read := func(repo string, limit int) ([]Outcome, error) {
		rows, err := tx.QueryContext(ctx, query, repo, issue, since.UTC().Format("2006-01-02 15:04:05.999999999"), normalizeTS(until), limit)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		var out []Outcome
		for rows.Next() {
			var o Outcome
			if err := rows.Scan(&o.ID, &o.Developer, &o.IssueID, &o.Weight, &o.Quality, &o.Timestamp, &o.Repo); err != nil {
				return nil, err
			}
			out = append(out, o)
		}
		return out, rows.Err()
	}
	if repo != repoid.Unqualified {
		candidates, err = read(repo, 2)
		if err != nil {
			return nil, false, err
		}
		if len(candidates) == 2 {
			candidates, err = read(repo, -1)
			if err != nil {
				return nil, false, err
			}
		}
	}
	if len(candidates) == 0 {
		legacy, err := read(repoid.Unqualified, 2)
		if err != nil {
			return nil, false, err
		}
		legacyOnly = len(legacy) > 0
	}
	return candidates, legacyOnly, nil
}

// Diagnostic only. Do not project this into the numeric quality or report digest.
const revertUnknownSQL = `(CASE WHEN EXISTS (SELECT 1 FROM quality_events q WHERE q.outcome_id=outcomes.id AND q.event_type NOT IN ('ci_pass','ci_fail','ci_fail_flaky','revert_quality','revert_strategic')) THEN 'unknown' ELSE '' END)`
