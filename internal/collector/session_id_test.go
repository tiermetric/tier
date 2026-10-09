package collector

import (
	"testing"
	"time"
)

// TestJoinSessionsToCommits_PopulatesSessionID proves the JSONL path stamps the
// parsed session identity onto every emitted TokenEvent (#238). The parser already
// captures sessionId; before this it survived only inside the idempotency hash and
// was discarded at persistence, destroying the per-session grouping grain the
// context-bloat / rework-loop diagnostics need.
func TestJoinSessionsToCommits_PopulatesSessionID(t *testing.T) {
	sessions := []sessionSummary{
		{
			SessionID: "sess-aaa",
			GitBranch: "feature/42-foo",
			Model:     "claude-sonnet-4",
			StartTime: time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC),
			EndTime:   time.Date(2026, 7, 9, 10, 5, 0, 0, time.UTC),
			Messages: []messageUsage{
				{messageID: "msg_aaa_1", timestamp: time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC), model: "claude-sonnet-4", input: 100, output: 50},
				{messageID: "msg_aaa_2", timestamp: time.Date(2026, 7, 9, 10, 1, 0, 0, time.UTC), model: "claude-sonnet-4", input: 200, output: 80},
			},
		},
		{
			SessionID: "sess-bbb",
			GitBranch: "feature/99-bar",
			Model:     "claude-sonnet-4",
			StartTime: time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC),
			EndTime:   time.Date(2026, 7, 9, 11, 5, 0, 0, time.UTC),
			Messages: []messageUsage{
				{messageID: "msg_bbb_1", timestamp: time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC), model: "claude-sonnet-4", input: 300, output: 120},
			},
		},
	}
	events := joinSessionsToCommits(sessions, nil, "alice", "")
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	want := map[string]string{
		"msg_aaa_1": "sess-aaa",
		"msg_aaa_2": "sess-aaa",
		"msg_bbb_1": "sess-bbb",
	}
	for _, ev := range events {
		if ev.SessionID == "" {
			t.Errorf("event for issue %q has empty SessionID; want it stamped from the session", ev.IssueID)
		}
	}
	// Map each event back to its source session via the per-message id embedded in
	// the idempotency key and assert the stamped SessionID matches.
	for wantMsg, wantSess := range want {
		found := false
		for _, ev := range events {
			if ev.IdempotencyKey == MessageIdempotencyKey(ProviderAnthropic, wantMsg) {
				found = true
				if ev.SessionID != wantSess {
					t.Errorf("event %s: SessionID = %q, want %q", wantMsg, ev.SessionID, wantSess)
				}
			}
		}
		if !found {
			t.Errorf("no event emitted for message %s", wantMsg)
		}
	}
}
