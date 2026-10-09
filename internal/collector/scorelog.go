package collector

import (
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// ScoreLogCall is one priced-independent API call read from a single session
// log file — no git repo, no store, no attribution join. It is the shape
// `tierd score-log` (#465) needs: exactly the token classes
// store.ComputeCostHost prices, plus the model and timestamp that produced
// them, taken from the SAME parser `tierd score`/`tierd serve` use. One
// implementation, not a second one (the defect class #465 exists to close —
// see model-bench/score.py, which this retires).
type ScoreLogCall struct {
	Timestamp time.Time
	// Model is the RAW model string recorded in the log for this call,
	// unmodified. The caller decides whether to price at this value or an
	// operator-supplied override; ScoreLogCall never substitutes one itself,
	// so a caller that wants to know what the log actually said always can.
	Model string
	Usage store.CostUsage
}

// ScoreLogSession is what ONE session log file yields: its Calls in file
// order (already deduplicated exactly as live capture dedupes), plus
// identifying metadata for a report header. Two producers build this type —
// ParseClaudeSessionLog (this file, Claude Code JSONL) and
// codexrollout.ScoreLog (the Codex rollout-log twin) — so `tierd score-log`
// (#465) can treat both formats identically once parsed.
type ScoreLogSession struct {
	SessionID string
	StartTime time.Time
	EndTime   time.Time
	Calls     []ScoreLogCall
}

// ParseClaudeSessionLog reads a single Claude Code session JSONL file — the
// same file format `tierd score` scans under ~/.claude/projects/ — and
// returns one ScoreLogCall per assistant message, deduplicated by
// message.id exactly as parseSessionFile does for live capture (largest-total
// wins for a repeated id; see parseSessionFileFromOffset). No git repo is
// required and no commit join is performed: score-log prices a log in
// isolation, matching what a fully unattributed capture of the same file
// would cost.
//
// Returns a zero-Calls ScoreLogSession (not an error) when the file parses
// cleanly but has no assistant usage — a genuinely empty/aborted session.
// Callers that want a "no billable usage" refusal (mirroring
// model-bench/score.py's control arm) must check len(Calls) themselves; this
// function does not fail loud on that condition. It DOES fail loud (non-nil
// error) on a read or scan failure, matching parseSessionFile's contract.
func ParseClaudeSessionLog(path string) (ScoreLogSession, error) {
	s, err := parseSessionFile(path)
	if err != nil {
		return ScoreLogSession{}, err
	}
	if s == nil {
		// No assistant usage in the file at all — see parseSessionFileFromOffset's
		// "Only truly empty sessions ... are discarded" contract.
		return ScoreLogSession{}, nil
	}

	calls := make([]ScoreLogCall, 0, len(s.Messages))
	for _, m := range s.Messages {
		model := m.model
		if model == "" {
			// Defensive fallback to the session-level model, mirroring
			// joinSessionsToCommits — should not happen with current Claude
			// Code JSONL, but older formats may have lacked a per-message field.
			model = s.Model
		}
		ts := m.timestamp
		if ts.IsZero() {
			ts = s.StartTime
		}
		calls = append(calls, ScoreLogCall{
			Timestamp: ts.UTC(),
			Model:     model,
			Usage: store.CostUsage{
				Input:        m.input,
				Output:       m.output,
				CacheRead:    m.cacheRead,
				CacheWrite5m: m.cacheWrite5m,
				CacheWrite1h: m.cacheWrite1h,
			},
		})
	}
	return ScoreLogSession{
		SessionID: s.SessionID,
		StartTime: s.StartTime.UTC(),
		EndTime:   s.EndTime.UTC(),
		Calls:     calls,
	}, nil
}
