package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
	"github.com/tiermetric/tier/internal/issueref/issuereftest"
)

func strictCollectorFixture(t *testing.T, branch, text string) (*JSONLCollector, time.Time) {
	t.Helper()
	repo := t.TempDir()
	initEmptyGitRepo(t, repo)
	if text == "" {
		text = "chore: fixture"
	}
	for _, args := range [][]string{{"checkout", "-q", "-b", branch}, {"-c", "user.name=fixture", "-c", "user.email=fixture@test", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-q", "-m", text}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	g, err := issueref.NewGrammar(issuereftest.Keys)
	if err != nil {
		t.Fatal(err)
	}
	commits, err := gitLogWithGrammar(context.Background(), repo, time.Time{}, g)
	if err != nil || len(commits) != 1 {
		t.Fatalf("commits %+v %v", commits, err)
	}
	ts := commits[0].Timestamp
	dir := t.TempDir()
	projects := filepath.Join(dir, "projects")
	if err := os.MkdirAll(projects, 0700); err != nil {
		t.Fatal(err)
	}
	line := map[string]any{"type": "assistant", "timestamp": ts.Format(time.RFC3339), "sessionId": "strict", "gitBranch": branch, "cwd": repo, "message": map[string]any{"id": "strict", "role": "assistant", "model": "claude-sonnet-4", "usage": map[string]int{"input_tokens": 100}}}
	data, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, projects, "strict.jsonl", []string{string(data)})
	return &JSONLCollector{Grammar: g, RepoPath: repo, ClaudeDir: dir, DeveloperID: "alice", RepoSlug: "owner/repo"}, ts
}

func TestStrictGrammarSharedProducerFixtures(t *testing.T) {
	for _, tc := range issuereftest.Fixtures() {
		t.Run(tc.Name, func(t *testing.T) {
			c, ts := strictCollectorFixture(t, tc.Branch, tc.Text)
			events, err := c.Collect(context.Background(), ts.Add(-time.Hour))
			if err != nil || len(events) != 1 {
				t.Fatalf("events %+v %v", events, err)
			}
			want := tc.Spend
			if want.ID == "" {
				want.ID = UnattributedNoIssue
			}
			if got := (issueref.Ref{ID: events[0].IssueID, Rule: events[0].IssueRule}); got != want {
				t.Fatalf("got %+v want %+v", got, want)
			}
			resolver, err := NewIssueResolver(context.Background(), c.RepoPath, ts.Add(-time.Hour), c.Grammar)
			if err != nil {
				t.Fatal(err)
			}
			if got := resolver.ResolveRef(tc.Branch, ts); got != want {
				t.Fatalf("resolver %+v want %+v", got, want)
			}
			if tc.Revert && events[0].PRNumber != nil {
				t.Fatal("revert regained PR attribution")
			}
			if tc.Name == "bare-PR" && (events[0].PRNumber == nil || *events[0].PRNumber != 446) {
				t.Fatal("lost PR metadata")
			}
		})
	}
}

func TestRevertBranchInfoCount(t *testing.T) {
	c, ts := strictCollectorFixture(t, "revert-45-fix/123-x", "closes #42 (#733)")
	var logs bytes.Buffer
	c.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	events, err := c.Collect(context.Background(), ts.Add(-time.Hour))
	if err != nil || len(events) != 1 {
		t.Fatal(events, err)
	}
	if !IsUnattributed(events[0].IssueID) || events[0].IssueRule != "" || events[0].PRNumber != nil {
		t.Fatal(events)
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	found := 0
	for _, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["msg"] == "revert-branch events left unattributed" {
			found++
			if row["level"] != "INFO" || row["count"] != float64(1) {
				t.Fatal(row)
			}
		}
	}
	if found != 1 {
		t.Fatalf("count lines=%d logs=%s", found, &logs)
	}
}

func TestRejectedBranchDoesNotInheritOtherDecoration(t *testing.T) {
	for _, text := range []string{"Use UTF-8", "closes #42"} {
		t.Run(text, func(t *testing.T) {
			c, ts := strictCollectorFixture(t, "fix/435b-auth-50051-cutover", text)
			cmd := exec.Command("git", "-C", c.RepoPath, "branch", "feature/50051")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git branch: %v %s", err, out)
			}
			events, err := c.Collect(context.Background(), ts.Add(-time.Hour))
			if err != nil || len(events) != 1 {
				t.Fatal(events, err)
			}
			want := issueref.Ref{ID: UnattributedNoIssue}
			if text == "closes #42" {
				want = issueref.Ref{ID: "issue-42", Rule: issueref.RuleCommitCloseDirective}
			}
			if got := (issueref.Ref{ID: events[0].IssueID, Rule: events[0].IssueRule}); got != want {
				t.Fatalf("got %+v want %+v", got, want)
			}
		})
	}
}

func TestJSONLRevertCountQuietAtZero(t *testing.T) {
	c, ts := strictCollectorFixture(t, "feature/42-x", "closes #42")
	var logs bytes.Buffer
	c.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	for range 2 {
		if _, err := c.Collect(context.Background(), ts.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(logs.String(), "revert-branch events left unattributed") {
		t.Fatal(logs.String())
	}
}

func TestWatcherRevertCountPerPass(t *testing.T) {
	for _, branch := range []string{"feature/42-x", "revert-42-x"} {
		t.Run(branch, func(t *testing.T) {
			repo, claude := t.TempDir(), t.TempDir()
			path := filepath.Join(claude, "projects", "p1", "s.jsonl")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			writeJSONLLine(t, path, "revert-count", repo, branch, 100)
			rec := &recordingStore{}
			w := &Watcher{Ingester: rec, Checkpoints: rec}
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			var calls atomic.Int32
			cache := newCommitCache(time.Hour, time.Hour, time.Hour, fakeGitLog(&calls, nil, nil), nil)
			targets := resolveTargets([]string{repo})
			state, ok, _ := w.process(context.Background(), path, parseState{}, targets, "alice", cache, logger)
			if !ok || len(rec.snapshot()) != 1 {
				t.Fatal("first pass", ok, rec.snapshot())
			}
			want := 0
			if strings.HasPrefix(branch, "revert-") {
				want = 1
			}
			if got := strings.Count(logs.String(), `"msg":"revert-branch events left unattributed"`); got != want {
				t.Fatalf("before shutdown: got=%d want=%d logs=%s", got, want, &logs)
			}
			if _, ok, _ := w.process(context.Background(), path, state, targets, "alice", cache, logger); !ok {
				t.Fatal("resume failed")
			}
			if got := strings.Count(logs.String(), `"msg":"revert-branch events left unattributed"`); got != want {
				t.Fatalf("empty pass count=%d want=%d logs=%s", got, want, &logs)
			}
			if want == 1 && (!strings.Contains(logs.String(), `"count":1`) || !strings.Contains(logs.String(), `"level":"INFO"`)) {
				t.Fatal(logs.String())
			}
		})
	}
}
