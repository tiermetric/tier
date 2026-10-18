package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
)

// CapturedDollarRow is a diagnostic input, never a wire type. Paths come only
// from stored checkpoint metadata; reading a session file is forbidden (#826).
type CapturedDollarRow struct {
	Engine, Bucket, Origin, CWD string
	CostMicro, SampleSize       int64
}

func (d *DB) CapturedDollarRowsWindow(ctx context.Context, since, until time.Time, scope RepoScope) ([]CapturedDollarRow, error) {
	var out []CapturedDollarRow
	err := d.ReadSnapshot(ctx, func(s *Snapshot) error {
		var err error
		out, err = s.CapturedDollarRowsWindow(ctx, since, until, scope)
		return err
	})
	return out, err
}

func (r reader) CapturedDollarRowsWindow(ctx context.Context, since, until time.Time, scope RepoScope) ([]CapturedDollarRow, error) {
	where, args := tsWindow(since, until)
	clause, scopeArgs := scope.clause()
	args = append(args, scopeArgs...)
	// Restrict metadata to sessions in this exact window and repository. Origin
	// is session-level: parent and subagent events cannot be split by checkpoint.
	cps, err := r.q.QueryContext(ctx, `SELECT path, metadata FROM watcher_checkpoint WHERE json_valid(metadata) AND json_extract(metadata,'$.`+CheckpointSessionIDKey+`') IN (SELECT session_id FROM token_events WHERE `+where+clause+` AND source <> 'api' AND session_id IS NOT NULL)`, args...)
	if err != nil {
		return nil, err
	}
	type evidence struct {
		cwd, parentCWD            string
		count, parents, subagents int
		agrees                    bool
	}
	meta := map[string]evidence{}
	for cps.Next() {
		var path, raw string
		if err := cps.Scan(&path, &raw); err != nil {
			_ = cps.Close()
			return nil, err
		}
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &m) != nil || !IsFileCheckpoint(path) {
			continue
		}
		var session, cwd string
		if json.Unmarshal(m[CheckpointSessionIDKey], &session) != nil || session == "" {
			continue
		}
		_ = json.Unmarshal(m[CheckpointCWDKey], &cwd)
		e := meta[session]
		if e.count == 0 {
			e.cwd, e.agrees = cwd, true
		} else if e.cwd != cwd {
			e.agrees = false
		}
		e.count++
		if strings.Contains(filepath.ToSlash(path), "/subagents/") {
			e.subagents++
		} else {
			e.parents++
			e.parentCWD = cwd
		}
		meta[session] = e
	}
	err = cps.Err()
	_ = cps.Close()
	if err != nil {
		return nil, err
	}
	rows, err := r.queryEffectiveWindow(ctx, since, until, scope, `SELECT source, issue_id, COALESCE(session_id,''), SUM(cost_micro), COUNT(*) FROM token_events WHERE `+where+clause+` AND source <> 'api' GROUP BY source, issue_id, session_id ORDER BY source, issue_id, session_id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CapturedDollarRow
	for rows.Next() {
		var row CapturedDollarRow
		var session string
		if err := rows.Scan(&row.Engine, &row.Bucket, &session, &row.CostMicro, &row.SampleSize); err != nil {
			return nil, err
		}
		if row.Engine == "jsonl" {
			row.Engine = "claude-code"
		}
		row.Origin = "unknown"
		if row.Engine == "claude-code" {
			if e, ok := meta[session]; ok {
				if e.agrees {
					row.CWD = e.cwd
				} else if e.parents == 1 {
					row.CWD = e.parentCWD
				}
				switch {
				case e.parents > 0 && e.subagents > 0:
					row.Origin = "mixed"
				case e.parents > 0:
					row.Origin = "main_session"
				case e.subagents > 0:
					row.Origin = "subagent"
				}
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
