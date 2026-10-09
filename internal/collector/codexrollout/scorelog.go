package codexrollout

import (
	"log/slog"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// ScoreLog reads a single Codex rollout JSONL file — the same
// `~/.codex/sessions/**/rollout-*.jsonl` format the live collector scans —
// and returns one collector.ScoreLogCall per billable token_count delta, in
// file order. It is the one-shot, repo-free entry point `tierd score-log`
// (#465) needs: no scan window, no repo/issue attribution, no ingester.
//
// It delegates entirely to parseRollout, so it inherits that function's
// fail-loud contract byte for byte: a containment violation (cached > input,
// total != input+output), a read/scan failure, a session with billable calls
// but no session id, a session whose file never named ANY model, or a call
// with no timestamp and nothing to fall back on are all non-nil errors — the
// same "one implementation" guarantee ParseClaudeSessionLog gives the Claude
// path. A session with zero billable token_count events is NOT an error
// (see parseRollout's RETURN CONTRACT): it returns a zero-Calls session so
// the caller can decide what to do with a genuinely idle log.
//
// logger receives parseRollout's own diagnostics (skipped malformed lines,
// the nonzero cache_write_input_tokens notice); pass a discard logger to
// silence them. Host is always absent from the returned usage — a session
// file records the CLIENT's view and cannot know which host served the call,
// exactly as the live Codex collector's buildEvents leaves it (#300/#525).
func ScoreLog(path string, logger *slog.Logger) (collector.ScoreLogSession, error) {
	sess, err := parseRollout(path, logger)
	if err != nil {
		return collector.ScoreLogSession{}, err
	}
	// parseRollout's contract: a non-nil session is always returned on
	// success, even with zero Calls (that is how a genuinely idle log is told
	// apart from one damaged badly enough to lose every billable line — see
	// SkippedLines on the type). Defend against a nil session anyway rather
	// than panic on the dereferences below if that contract is ever broken.
	if sess == nil {
		return collector.ScoreLogSession{}, nil
	}

	calls := make([]collector.ScoreLogCall, 0, len(sess.Calls))
	for _, c := range sess.Calls {
		ts := c.Timestamp
		if ts.IsZero() {
			ts = sess.StartTime
		}
		calls = append(calls, collector.ScoreLogCall{
			Timestamp: ts.UTC(),
			Model:     c.Model,
			Usage: store.CostUsage{
				Input:     c.Input,
				Output:    c.Output,
				CacheRead: c.CacheRead,
				// OpenAI has no cache-write SKU, so both write buckets stay
				// zero, matching buildEvents' live pricing call exactly.
			},
		})
	}
	return collector.ScoreLogSession{
		SessionID: sess.SessionID,
		StartTime: sess.StartTime.UTC(),
		// rolloutSession carries no explicit end time; the last call's
		// timestamp is the closest equivalent and is cheap to derive here
		// rather than adding a field that only this caller would use.
		EndTime: lastCallTime(sess).UTC(),
		Calls:   calls,
	}, nil
}

// lastCallTime returns the timestamp of s's last call, falling back to
// s.StartTime for a zero-Calls session (mirrors ScoreLog's own per-call
// fallback, so a zero-Calls report's start and end times agree).
func lastCallTime(s *rolloutSession) time.Time {
	if len(s.Calls) == 0 {
		return s.StartTime
	}
	last := s.Calls[len(s.Calls)-1].Timestamp
	if last.IsZero() {
		return s.StartTime
	}
	return last
}
