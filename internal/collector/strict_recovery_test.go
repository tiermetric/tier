package collector_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/issueref"
	"github.com/tiermetric/tier/internal/store"
	"github.com/tiermetric/tier/internal/webhook"
)

func TestStrictRejectedBranchRecovery(t *testing.T) {
	for _, mode := range []string{"pr-number-at-insert", "branch-at-merge"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			repo := t.TempDir()
			branch := "docs/notes-2026-10-14"
			ts := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
			subject := "Use UTF-8"
			if mode == "pr-number-at-insert" {
				subject = "fix: widget (#733)"
			}
			for _, args := range [][]string{{"init", "-q"}, {"checkout", "-q", "-b", branch}, {"-c", "user.name=fixture", "-c", "user.email=fixture@test", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-q", "-m", subject}} {
				cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
				cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+ts.Format(time.RFC3339), "GIT_COMMITTER_DATE="+ts.Format(time.RFC3339))
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git: %v %s", err, out)
				}
			}
			dir := t.TempDir()
			projects := filepath.Join(dir, "projects")
			if err := os.MkdirAll(projects, 0700); err != nil {
				t.Fatal(err)
			}
			line, err := json.Marshal(map[string]any{"type": "assistant", "timestamp": ts.Format(time.RFC3339), "sessionId": "strict-recovery", "gitBranch": branch, "cwd": repo, "message": map[string]any{"id": "strict-recovery", "role": "assistant", "model": "claude-sonnet-4", "usage": map[string]int{"input_tokens": 100}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projects, "session.jsonl"), append(line, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			merge := func() {
				payload, err := json.Marshal(map[string]any{"action": "closed", "repository": map[string]any{"full_name": "owner/repo", "default_branch": "main"}, "pull_request": map[string]any{"number": 733, "body": "closes #42", "merged": true, "merge_commit_sha": "strict-merge", "created_at": ts.Add(-time.Hour).Format(time.RFC3339), "merged_at": ts.Add(time.Hour).Format(time.RFC3339), "head": map[string]any{"ref": branch, "repo": map[string]string{"full_name": "owner/repo"}}, "base": map[string]any{"repo": map[string]string{"full_name": "owner/repo", "default_branch": "main"}}, "user": map[string]string{"login": "alice"}}})
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(string(payload)))
				req.Header.Set("X-GitHub-Event", "pull_request")
				mac := hmac.New(sha256.New, []byte("secret"))
				_, _ = mac.Write(payload)
				req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
				h := webhook.New(db, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)), webhook.WithGrammar(issueref.Grammar{}))
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != 204 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			if mode == "pr-number-at-insert" {
				merge()
			}
			c := collector.JSONLCollector{RepoPath: repo, ClaudeDir: dir, DeveloperID: "alice", RepoSlug: "owner/repo", Grammar: issueref.Grammar{}}
			events, err := c.Collect(ctx, ts.Add(-time.Hour))
			if err != nil || len(events) != 1 || !collector.IsUnattributed(events[0].IssueID) || events[0].IssueRule != "" {
				t.Fatal(events, err)
			}
			if err := c.Run(ctx, ts.Add(-time.Hour), ingester.Store(db)); err != nil {
				t.Fatal(err)
			}
			if mode == "branch-at-merge" {
				merge()
			}
			exp, err := db.ExportDeveloper(ctx, "alice")
			if err != nil || len(exp.Outcomes) != 1 || len(exp.TokenEvents) != 1 {
				t.Fatal(exp, err)
			}
			if exp.Outcomes[0].IssueID != "issue-42" || (exp.Outcomes[0].IssueRule == nil || *exp.Outcomes[0].IssueRule != string(issueref.RuleBodyCloseDirective)) {
				t.Fatal(exp.Outcomes)
			}
			event := exp.TokenEvents[0]
			if event.IssueID != "issue-42" || (event.AttributionRule == nil || *event.AttributionRule != mode) || event.IssueRule != nil {
				t.Fatalf("event=%+v want issue-42/%s with no copied issue_rule", event, mode)
			}
			if err := c.Run(ctx, ts.Add(-time.Hour), ingester.Store(db)); err != nil {
				t.Fatal(err)
			}
			replay, err := db.ExportDeveloper(ctx, "alice")
			if err != nil || len(replay.TokenEvents) != 1 || replay.TokenEvents[0].IssueID != "issue-42" {
				t.Fatal(replay, err)
			}
		})
	}
}
