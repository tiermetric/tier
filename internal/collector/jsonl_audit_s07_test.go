package collector

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAuditS07_5_NoUsageWindowKeepsLastRealBranch(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign=%v", foreign), func(t *testing.T) {
			base := t.TempDir()
			repo := filepath.Join(base, "repo")
			if err := os.Mkdir(repo, 0o755); err != nil {
				t.Fatal(err)
			}
			h := newHeldRig(t, base, repo)
			if foreign {
				h.targets = nil
			}
			at := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
			line := func(id, branch string, out int) string {
				return wtJSONLLine(id, at, repo, branch, wtText, out)
			}
			first := h.read(t, &Watcher{}, line("first", "main", 100))
			if foreign {
				wantEvents(t, first)
			} else {
				wantEvents(t, first, "100 unattributed:main ")
			}
			seq := h.state.metadata.NextParseSeq
			wantEvents(t, h.read(t, &Watcher{},
				`{"type":"user","sessionId":"sess-823","gitBranch":"fix/77-capture"}`))
			if h.state.offset != fileSize(t, h.path) {
				t.Fatalf("no-usage window did not advance: offset %d", h.state.offset)
			}
			if h.state.metadata.LastRealBranch != "fix/77-capture" {
				t.Errorf("no-usage window lost last real branch: %+v", h.state.metadata)
			}
			if h.state.metadata.NextParseSeq != seq {
				t.Errorf("no-usage window changed parse sequence: got %d, want %d", h.state.metadata.NextParseSeq, seq)
			}
			h.restart(t)
			h.targets = resolveTargets([]string{repo})
			wantEvents(t, h.read(t, &Watcher{}, line("agent", "worktree-agent-abc123", 200)),
				"200 issue-77 ")
		})
	}
	t.Run("attribution-on", func(t *testing.T) {
		f := newWTFixture(t)
		h := newHeldRig(t, f.base, f.repo)
		w := &Watcher{worktrees: f.x}
		at := f.wtAt(10)
		line := func(id, branch string, out int) string {
			return wtJSONLLine(id, f.wtAt(1), f.repo, branch, wtText, out)
		}
		// Release the initial message so the next window has nothing held open.
		wantEvents(t, h.idleRead(t, w, at, at.Add(idleN), line("first", "main", 100)),
			"100 unattributed:main branch")
		wantHeld(t, h, fileSize(t, h.path), false)
		seq := h.state.metadata.NextParseSeq
		at = at.Add(2 * idleN)
		wantEvents(t, h.idleRead(t, w, at, at,
			`{"type":"user","sessionId":"sess-823","gitBranch":"fix/77-capture"}`))
		wantHeld(t, h, fileSize(t, h.path), false)
		h.restart(t)
		if h.state.metadata.LastRealBranch != "fix/77-capture" {
			t.Errorf("no-usage window lost checkpointed last real branch: got %q, want %q",
				h.state.metadata.LastRealBranch, "fix/77-capture")
		}
		if h.state.metadata.NextParseSeq != seq {
			t.Errorf("no-usage window changed checkpointed parse sequence: got %d, want %d",
				h.state.metadata.NextParseSeq, seq)
		}
		wantEvents(t, h.idleRead(t, w, at, at.Add(idleN), line("agent", "worktree-agent-abc123", 200)),
			"200 issue-77 branch")
	})
}

func TestAuditS07_7_MessageBranchBreaksMultiIssueBranchCommitTie(t *testing.T) {
	at := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	commits := []gitCommit{{
		Hash: "shared", Timestamp: at,
		Branches: []string{"fix/11-first", "fix/77-capture", "scratch"},
		Subject:  "fix: capture (refs #33)", IssueID: "issue-11",
	}}
	for _, tc := range []struct {
		branch string
		want   string
	}{
		{"fix/11-first", "issue-11"},
		{"fix/77-capture", "issue-77"},
		{"feature/77-capture", "issue-11"}, // Same leaf, but not a decorated branch.
		{"fix/77/scratch", "issue-11"},     // Own issue is absent from the decoration.
		{"scratch", "issue-11"},
		{"main", UnattributedMain},
	} {
		t.Run(tc.branch, func(t *testing.T) {
			s := sessionSummary{
				SessionID: "session", GitBranch: "fix/11-first", StartTime: at,
				Messages: []messageUsage{{messageID: "message", timestamp: at, gitBranch: tc.branch, output: 100}},
			}
			events := joinSessionsToCommits([]sessionSummary{s}, commits, "dev", "owner/repo")
			if len(events) != 1 || events[0].IssueID != tc.want {
				t.Fatalf("branch %q: events %+v, want issue %q", tc.branch, events, tc.want)
			}
		})
	}
}

func TestAuditS07_7_SingleBranchCommitWinsOverMessageBranch(t *testing.T) {
	at := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	commits := []gitCommit{{
		Hash: "single", Timestamp: at,
		Branches: []string{"fix/11/capture"}, IssueID: "issue-11",
	}}
	// Matching still uses the leaf name, even when the message's branch names
	// a different issue; only a multi-issue-branch commit permits a tie-break.
	s := sessionSummary{
		SessionID: "session", GitBranch: "fix/77/capture", StartTime: at,
		Messages: []messageUsage{{messageID: "message", timestamp: at, gitBranch: "fix/77/capture", output: 100}},
	}
	events := joinSessionsToCommits([]sessionSummary{s}, commits, "dev", "owner/repo")
	if len(events) != 1 || events[0].IssueID != "issue-11" {
		t.Fatalf("events %+v, want commit's issue-11 over message branch's issue-77", events)
	}
}
