package collector

import (
	"testing"
)

// TestParseClaudeSessionLog_TwoModelsIndependentCalls is the core #465
// regression: score-log must price EACH message at ITS OWN model, not one
// model for the whole session — the exact defect model-bench/score.py's
// single --model-key design has and score-log exists to retire. Two
// assistant messages, two different models, one dedup pair (msg_A gets a
// placeholder + a definitive entry, mirroring
// TestParseSessionFile_DedupsByMessageID) so this also proves ScoreLogCall
// rides the SAME dedup path, not a second summation.
func TestParseClaudeSessionLog_TwoModelsIndependentCalls(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		// msg_A on model X — placeholder then definitive (largest-total wins).
		`{"type":"assistant","timestamp":"2026-05-18T10:00:00Z","sessionId":"sess-1","message":{"id":"msg_A","model":"model-x","usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"assistant","timestamp":"2026-05-18T10:00:05Z","sessionId":"sess-1","message":{"id":"msg_A","model":"model-x","usage":{"input_tokens":1000,"output_tokens":500,"cache_read_input_tokens":100}}}`,
		// msg_B on a DIFFERENT model Y.
		`{"type":"assistant","timestamp":"2026-05-18T10:01:00Z","sessionId":"sess-1","message":{"id":"msg_B","model":"model-y","usage":{"input_tokens":300,"output_tokens":150}}}`,
	}
	path := writeJSONL(t, dir, "session1.jsonl", lines)

	sess, err := ParseClaudeSessionLog(path)
	if err != nil {
		t.Fatalf("ParseClaudeSessionLog: %v", err)
	}
	if sess.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", sess.SessionID)
	}
	if len(sess.Calls) != 2 {
		t.Fatalf("len(Calls) = %d, want 2 (dedup must collapse msg_A's placeholder, not drop msg_B)", len(sess.Calls))
	}

	byModel := map[string]ScoreLogCall{}
	for _, c := range sess.Calls {
		byModel[c.Model] = c
	}
	a, ok := byModel["model-x"]
	if !ok {
		t.Fatal("no call priced at model-x")
	}
	if a.Usage.Input != 1000 || a.Usage.Output != 500 || a.Usage.CacheRead != 100 {
		t.Errorf("model-x usage = %+v, want input=1000 output=500 cacheRead=100 (dedup should keep the DEFINITIVE entry, not the placeholder)", a.Usage)
	}
	b, ok := byModel["model-y"]
	if !ok {
		t.Fatal("no call priced at model-y")
	}
	if b.Usage.Input != 300 || b.Usage.Output != 150 {
		t.Errorf("model-y usage = %+v, want input=300 output=150", b.Usage)
	}
}

// TestParseClaudeSessionLog_NoAssistantUsage is the control pair for the
// "genuinely empty session" contract: a file with lines but no assistant
// usage returns ZERO calls and NO error (positive case), while the SAME file
// plus one real assistant message returns exactly one call (negative
// control) — proving the zero-calls assertion above can actually fail, not
// just always pass on an under-specified check.
func TestParseClaudeSessionLog_NoAssistantUsage(t *testing.T) {
	dir := t.TempDir()
	empty := writeJSONL(t, dir, "empty.jsonl", []string{
		`{"type":"user","sessionId":"sess-2","timestamp":"2026-05-18T10:00:00Z"}`,
		`{"type":"attachment","sessionId":"sess-2","timestamp":"2026-05-18T10:00:01Z"}`,
	})
	sess, err := ParseClaudeSessionLog(empty)
	if err != nil {
		t.Fatalf("ParseClaudeSessionLog(empty): unexpected error: %v", err)
	}
	if len(sess.Calls) != 0 {
		t.Fatalf("len(Calls) = %d, want 0 for a session with no assistant usage", len(sess.Calls))
	}

	withUsage := writeJSONL(t, dir, "withusage.jsonl", []string{
		`{"type":"user","sessionId":"sess-2","timestamp":"2026-05-18T10:00:00Z"}`,
		`{"type":"assistant","timestamp":"2026-05-18T10:00:01Z","sessionId":"sess-2","message":{"id":"msg_A","model":"model-x","usage":{"input_tokens":10,"output_tokens":5}}}`,
	})
	sess2, err := ParseClaudeSessionLog(withUsage)
	if err != nil {
		t.Fatalf("ParseClaudeSessionLog(withUsage): unexpected error: %v", err)
	}
	if len(sess2.Calls) != 1 {
		t.Fatalf("len(Calls) = %d, want 1 — the zero-Calls check above would pass vacuously if this fixture also produced 0", len(sess2.Calls))
	}
}

// TestParseClaudeSessionLog_ReadError covers a missing file: the error must
// propagate, not be swallowed into a silent empty session.
func TestParseClaudeSessionLog_ReadError(t *testing.T) {
	_, err := ParseClaudeSessionLog("/nonexistent/path/does-not-exist.jsonl")
	if err == nil {
		t.Fatal("expected an error for a nonexistent file, got nil")
	}
}
