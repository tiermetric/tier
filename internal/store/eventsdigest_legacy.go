package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

// ReportDigestsForScheme preserves tierdig1's exact domains, columns, ordering
// and framing. Old reports must never be compared against tierdig2 bytes.
func (d *DB) ReportDigestsForScheme(ctx context.Context, since, until time.Time, scope RepoScope, scheme string) (events, outcomes Digest, err error) {
	if scheme == digestScheme {
		return d.ReportDigests(ctx, since, until, scope)
	}
	if scheme != "tierdig1" {
		return Digest{}, Digest{}, fmt.Errorf("unsupported digest scheme %q", scheme)
	}
	err = d.ReadSnapshot(ctx, func(s *Snapshot) error {
		// Token framing did not change; only its domain and scheme tag did.
		events, err = legacyDigestFrom(ctx, s.q, since.Add(-AttributableWindow), until, scope, false)
		if err != nil {
			return err
		}
		outcomes, err = legacyDigestFrom(ctx, s.q, since, until, scope, true)
		return err
	})
	return
}

func legacyDigestFrom(ctx context.Context, q digestQueryer, since, until time.Time, scope RepoScope, outcomes bool) (Digest, error) {
	if err := checkDigestWindow(since, until); err != nil {
		return Digest{}, err
	}
	clause, args := digestPredicate(since, until, scope)
	query, domain := tokenEventDigestQuery(clause), "tierdig1/token_events"
	if outcomes {
		query = `SELECT id,CAST(ts AS TEXT),developer,COALESCE(repo,'unqualified'),issue_id,weight,quality,COALESCE(work_type,'feature'),COALESCE(source,'github-webhook'),COALESCE(pr_number,0),COALESCE(merge_commit_sha,'') FROM outcomes INDEXED BY idx_outcomes_ts_id WHERE ` + clause + digestOrderSQL
		domain = "tierdig1/outcomes"
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return Digest{}, err
	}
	defer func() { _ = rows.Close() }()
	h := sha256.New()
	var buf bytes.Buffer
	writeLengthPrefixed(&buf, digestDomainFor(domain, scope))
	foldFrames(h, &buf)
	var n int64
	for rows.Next() {
		if outcomes {
			var r outcomeDigestRow
			if err = rows.Scan(&r.ID, &r.TS, &r.Developer, &r.Repo, &r.IssueID, &r.Weight, &r.Quality, &r.WorkType, &r.Source, &r.PRNumber, &r.MergeCommitSHA); err != nil {
				return Digest{}, err
			}
			appendLegacyOutcomeFrames(&buf, r)
			foldFrames(h, &buf)
		} else {
			var r tokenEventDigestRow
			if err = rows.Scan(&r.ID, &r.TS, &r.Developer, &r.Repo, &r.IssueID, &r.Model, &r.Host, &r.InputTok, &r.OutputTok, &r.CacheRead, &r.CacheWrite5m, &r.CacheWrite1h, &r.CostMicro, &r.PriceVersion, &r.BillingMode, &r.Source, &r.Fidelity); err != nil {
				return Digest{}, err
			}
			appendTokenEventFrames(&buf, r)
			foldFrames(h, &buf)
		}
		n++
	}
	if err = rows.Err(); err != nil {
		return Digest{}, err
	}
	return Digest{Value: "tierdig1:" + hex.EncodeToString(h.Sum(nil)), Rows: n}, nil
}

// Freeze the original eleven frames independently of tierdig2's extended row.
func appendLegacyOutcomeFrames(buf *bytes.Buffer, r outcomeDigestRow) {
	for _, v := range []string{strconv.FormatInt(r.ID, 10), r.TS, r.Developer, r.Repo, r.IssueID,
		canonPriceFloat(r.Weight), canonPriceFloat(r.Quality), r.WorkType, r.Source,
		strconv.FormatInt(r.PRNumber, 10), r.MergeCommitSHA} {
		writeLengthPrefixed(buf, v)
	}
}
