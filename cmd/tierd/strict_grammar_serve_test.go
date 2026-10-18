//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
	"github.com/tiermetric/tier/internal/store"
)

func TestTrackerKeysPrecedence(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"serve", "ship", "score", "backfill", "doctor"} {
		for _, tc := range []struct{ name, config, env, cli, want string }{
			{"config", "badCONF", "", "", "badCONF"},
			{"env-over-config", "badCONF", "badENV", "", "badENV"},
			{"CLI-over-env-and-config", "badCONF", "badENV", "badCLI", "badCLI"},
		} {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, []byte("attribution:\n  tracker_keys: "+tc.config+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				args := []string{command, "--config", path}
				if tc.cli != "" {
					args = append(args, "--tracker-keys", tc.cli)
				}
				cmd := exec.Command(self)
				cmd.Env = wtServeEnv(t.TempDir(), "TIER_TRACKER_KEYS="+tc.env, "TIERD_SMOKE_CHILD_ARGS="+strings.Join(args, "\n"))
				out, err := cmd.CombinedOutput()
				if err == nil || !strings.Contains(string(out), `invalid tracker key "`+tc.want+`"`) {
					t.Fatalf("dispatch %s err=%v out=%s", command, err, out)
				}
			})
		}
	}
}

func TestServeTrackerGrammarWiring(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	repo, home := initGitRepo(t), t.TempDir()
	claude := filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(claude, "projects", "p1"), 0755); err != nil {
		t.Fatal(err)
	}
	addr, dbPath := freeLoopbackPort(t), filepath.Join(t.TempDir(), "tier.db")
	args := []string{"serve", "--addr", addr, "--db", dbPath, "--aggregation", "team", "--log-format", "json", "--webhook-secret", "strict-secret", "--watch-repo", repo, "--tracker-keys", "PROJ"}
	cmd := exec.Command(self)
	cmd.Env = wtServeEnv(home, "TIER_TRACKER_KEYS=", "TIERD_SMOKE_CHILD_ARGS="+strings.Join(args, "\n"))
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	base := "http://" + addr
	waitUntilLive(t, base+"/api/v1/livez", stderr, exited)
	waitForWT(t, "watcher startup", stderr, func() bool { return strings.Contains(stderr.String(), `"msg":"watcher started"`) })
	body, err := json.Marshal(map[string]any{"action": "closed", "repository": map[string]string{"full_name": "acme/app"}, "pull_request": map[string]any{"number": 9, "merged": true, "merge_commit_sha": "strict-merge", "head": map[string]string{"ref": "fix/PROJ-9-x"}, "user": map[string]string{"login": "alice"}}})
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("strict-secret"))
	_, _ = mac.Write(body)
	req, err := http.NewRequest(http.MethodPost, base+"/webhook/github", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "strict-grammar")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("webhook: status=%d body=%s err=%v", resp.StatusCode, raw, err)
	}
	staging := t.TempDir()
	writeSessionFixture(t, staging, repo)
	stagedPath := filepath.Join(staging, "projects", "p1", "s1.jsonl")
	replaceStrictFixtureBranch(t, stagedPath, "feature/42-foo")
	raw, err = os.ReadFile(stagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claude, "projects", "p1", "s1.jsonl"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	waitForWT(t, "watcher event", stderr, func() bool {
		_, body := httpGet(t, base+"/api/v1/healthz")
		return strings.Contains(body, `"last_event_ts"`)
	})
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("serve: %v %s", err, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve shutdown timeout")
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	o, ok, err := db.OutcomeByMergeCommit(context.Background(), "strict-merge")
	if err != nil || !ok || o.IssueID != "PROJ-9" || o.IssueRule != issueref.RuleBranchConfiguredKey {
		t.Fatalf("webhook outcome=%+v found=%v err=%v", o, ok, err)
	}
	rows, _, err := db.ListTokenEvents(context.Background(), time.Time{}, time.Now().Add(time.Hour), store.PageCursor{}, 100)
	if err != nil || len(rows) != 1 || rows[0].IssueID != "PROJ-9" || rows[0].IssueRule != issueref.RuleBranchConfiguredKey {
		t.Fatalf("watcher rows=%+v err=%v", rows, err)
	}
}

func TestServeCodexMuseTrackerGrammarWiring(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, keys := range []string{"", "PROJ"} {
		t.Run("keys="+keys, func(t *testing.T) {
			repo, home := initGitRepo(t), t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0755); err != nil {
				t.Fatal(err)
			}
			codexDir := stageCodexRollout(t, repo)
			replaceStrictFixtureBranch(t, filepath.Join(codexDir, "2026", "07", "23", "rollout-clean-session.jsonl"), "feature/42-foo")
			museHome := stageMuseHome(t, repo)
			replaceStrictFixtureBranch(t, filepath.Join(museHome, "sessions", "2026", "09", "21", "ses-fixture-0001", "session.jsonl"), "feature/895-muse-fixture")
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			config := fmt.Sprintf("collectors:\n  codex_rollout:\n    sessions_dir: %q\n  muse:\n    home: %q\n", codexDir, museHome)
			if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			addr, dbPath := freeLoopbackPort(t), filepath.Join(t.TempDir(), "tier.db")
			args := []string{"serve", "--addr", addr, "--db", dbPath, "--aggregation", "team", "--log-format", "json",
				"--watch-repo", repo, "--tracker-keys", keys,
				"--codex-rollout", "--muse", "--config", configPath}
			cmd := exec.Command(self)
			cmd.Env = wtServeEnv(home, "TIER_TRACKER_KEYS=", "TIERD_SMOKE_CHILD_ARGS="+strings.Join(args, "\n"))
			stderr := &lockedBuffer{}
			cmd.Stderr = stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			waitUntilLive(t, "http://"+addr+"/api/v1/livez", stderr, exited)
			db, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			var rows []store.TokenEvent
			waitForWT(t, "Codex and Muse events", stderr, func() bool {
				rows, _, err = db.ListTokenEvents(context.Background(), time.Time{}, time.Now().Add(time.Hour), store.PageCursor{}, 100)
				return err == nil && len(rows) == cleanSessionBillableCalls+museFixtureEvents
			})
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-exited:
				if err != nil {
					t.Fatalf("serve: %v %s", err, stderr.String())
				}
			case <-time.After(15 * time.Second):
				t.Fatal("serve shutdown timeout")
			}
			for _, source := range []struct {
				name  string
				count int
			}{
				{"codex-rollout", cleanSessionBillableCalls}, {"muse", museFixtureEvents},
			} {
				t.Run(source.name, func(t *testing.T) {
					count := 0
					for _, row := range rows {
						if row.Source != source.name {
							continue
						}
						count++
						if keys == "" {
							if !store.IsUnattributed(row.IssueID) || row.IssueRule != "" {
								t.Errorf("unconfigured event %+v", row)
							}
						} else if row.IssueID != "PROJ-9" || row.IssueRule != issueref.RuleBranchConfiguredKey {
							t.Errorf("configured event %+v", row)
						}
					}
					if count != source.count {
						t.Fatalf("stored %d events, want %d; rows=%+v", count, source.count, rows)
					}
				})
			}
		})
	}
}
