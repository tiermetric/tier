package codexrollout

import (
	"testing"
)

// TestScoreLog_DifferencesCumulativeSnapshots is #465's Codex-path core
// regression: ScoreLog must return one call PER DELTA (not per raw snapshot,
// and not one lump sum), matching exactly what live capture (buildEvents)
// would ingest. Two cumulative snapshots synthesize one real delta call.
func TestScoreLog_DifferencesCumulativeSnapshots(t *testing.T) {
	sessions := t.TempDir()
	body := synthetic("/repo", "", "gpt-5.6-terra", []tokenUsage{
		usage(1000, 400, 100, 40),
		usage(2500, 1200, 260, 90),
	})
	path := writeRollout(t, sessions, "clean.jsonl", body)

	sess, err := ScoreLog(path, quietLogger())
	if err != nil {
		t.Fatalf("ScoreLog: %v", err)
	}
	if sess.SessionID != "synthetic-session-0001" {
		t.Errorf("SessionID = %q, want synthetic-session-0001", sess.SessionID)
	}
	if len(sess.Calls) != 2 {
		t.Fatalf("len(Calls) = %d, want 2 (one per cumulative snapshot)", len(sess.Calls))
	}

	// First call: the raw delta from a zero baseline. Second call is the
	// DIFFERENCE between the two snapshots, not the second snapshot's raw
	// (cumulative) numbers — the whole point of differencing (see
	// TestDuplicateTokenCountEventIsNotDoubleCounted in rollout_test.go).
	c0, c1 := sess.Calls[0], sess.Calls[1]
	if c0.Model != "gpt-5.6-terra" || c1.Model != "gpt-5.6-terra" {
		t.Errorf("Model = %q / %q, want gpt-5.6-terra for both", c0.Model, c1.Model)
	}
	if c0.Usage.Input != 1000-400 || c0.Usage.CacheRead != 400 || c0.Usage.Output != 100 {
		t.Errorf("call0 usage = %+v, want input=600 cacheRead=400 output=100", c0.Usage)
	}
	wantDeltaInput := (2500 - 1200) - (1000 - 400)
	wantDeltaCache := 1200 - 400
	wantDeltaOutput := 260 - 100
	if c1.Usage.Input != wantDeltaInput || c1.Usage.CacheRead != wantDeltaCache || c1.Usage.Output != wantDeltaOutput {
		t.Errorf("call1 (DELTA) usage = %+v, want input=%d cacheRead=%d output=%d — a lump-sum implementation would report the raw cumulative totals instead",
			c1.Usage, wantDeltaInput, wantDeltaCache, wantDeltaOutput)
	}
}

// TestScoreLog_ContainmentViolationIsFatal is the control pair proving
// ScoreLog does not silently swallow parseRollout's fail-loud containment
// checks: a well-formed log (control arm) must succeed, and the identical
// shape with cached > input (negative control) must error — so the
// error-propagation assertion below can actually fail.
func TestScoreLog_ContainmentViolationIsFatal(t *testing.T) {
	t.Run("control/well-formed", func(t *testing.T) {
		sessions := t.TempDir()
		body := synthetic("/repo", "", "gpt-5.6-terra", []tokenUsage{usage(1000, 400, 100, 40)})
		path := writeRollout(t, sessions, "ok.jsonl", body)
		sess, err := ScoreLog(path, quietLogger())
		if err != nil {
			t.Fatalf("control arm must PASS, got error: %v", err)
		}
		if len(sess.Calls) == 0 {
			t.Fatal("control arm produced zero calls — the corrupted case below would prove nothing")
		}
	})
	t.Run("cached_exceeds_input", func(t *testing.T) {
		sessions := t.TempDir()
		body := synthetic("/repo", "", "gpt-5.6-terra", []tokenUsage{
			{InputTokens: 1000, CachedInputTokens: 1001, OutputTokens: 100, TotalTokens: 1100},
		})
		path := writeRollout(t, sessions, "corrupt.jsonl", body)
		if _, err := ScoreLog(path, quietLogger()); err == nil {
			t.Fatal("expected an error for cached_input_tokens > input_tokens; ScoreLog must not silently price a corrupted log")
		}
	})
}

// TestScoreLog_ZeroCallsIsNotAnError covers a session_meta-only rollout (no
// token_count events at all): a genuinely idle session must return zero
// Calls with a NIL error, not be conflated with a parse failure.
func TestScoreLog_ZeroCallsIsNotAnError(t *testing.T) {
	sessions := t.TempDir()
	body := synthetic("/repo", "", "gpt-5.6-terra", nil)
	path := writeRollout(t, sessions, "idle.jsonl", body)

	sess, err := ScoreLog(path, quietLogger())
	if err != nil {
		t.Fatalf("ScoreLog(idle session): unexpected error: %v", err)
	}
	if len(sess.Calls) != 0 {
		t.Errorf("len(Calls) = %d, want 0", len(sess.Calls))
	}
}

// TestScoreLog_ReadError covers a missing file: the error must propagate.
func TestScoreLog_ReadError(t *testing.T) {
	if _, err := ScoreLog("/nonexistent/path/does-not-exist.jsonl", quietLogger()); err == nil {
		t.Fatal("expected an error for a nonexistent file, got nil")
	}
}
