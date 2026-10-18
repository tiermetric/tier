package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
	"github.com/tiermetric/tier/internal/store"
)

func replaceStrictFixtureBranch(t *testing.T, path, old string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), old) {
		t.Fatal("missing fixture branch", old)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(raw), old, "fix/PROJ-9-x")), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestScoreTrackerGrammarWiring(t *testing.T) {
	t.Setenv("TIER_TRACKER_KEYS", "")
	loadEmbeddedPriceTable(t)
	repo, claude := initGitRepo(t), t.TempDir()
	writeSessionFixture(t, claude, repo)
	replaceStrictFixtureBranch(t, filepath.Join(claude, "projects", "p1", "s1.jsonl"), "feature/42-foo")
	for _, keys := range []string{"", "PROJ"} {
		out := captureStdout(t, func() {
			if code := dispatch([]string{"score", "--repo", repo, "--claude-dir", claude, "--since", "2026-01-01", "--tracker-keys", keys}, os.Stdout, os.Stderr); code != 0 {
				t.Fatal(code)
			}
		})
		if strings.Contains(out, "PROJ-9") != (keys != "") {
			t.Fatalf("keys=%q report=%s", keys, out)
		}
		if keys == "" && !strings.Contains(out, "unattributed") {
			t.Fatal(out)
		}
	}
}

func TestShipTrackerGrammarWiring(t *testing.T) {
	t.Setenv("TIER_TRACKER_KEYS", "")
	t.Setenv("TIER_CODEX_ROLLOUT", "")
	t.Setenv("TIER_OPENCODE", "")
	t.Setenv("TIER_MUSE", "")
	loadDeterministicPrices(t)
	for _, source := range []string{"jsonl", "codex", "muse"} {
		for _, keys := range []string{"", "PROJ"} {
			t.Run(source+"/keys="+keys, func(t *testing.T) {
				repo, claude := initGitRepo(t), t.TempDir()
				srv, db := newShipTestServer(t, "")
				args := []string{"ship", "--server", srv.URL, "--repo", repo, "--claude-dir", claude, "--since", "2026-01-01", "--tracker-keys", keys}
				wantCount := 1
				switch source {
				case "jsonl":
					writeSessionFixture(t, claude, repo)
					replaceStrictFixtureBranch(t, filepath.Join(claude, "projects", "p1", "s1.jsonl"), "feature/42-foo")
				case "codex":
					dir := stageCodexRollout(t, repo)
					replaceStrictFixtureBranch(t, filepath.Join(dir, "2026", "07", "23", "rollout-clean-session.jsonl"), "feature/42-foo")
					args = append(args, "--codex-rollout", "--codex-sessions-dir", dir)
					wantCount = cleanSessionBillableCalls
				case "muse":
					home := stageMuseHome(t, repo)
					replaceStrictFixtureBranch(t, filepath.Join(home, "sessions", "2026", "09", "21", "ses-fixture-0001", "session.jsonl"), "feature/895-muse-fixture")
					args = append(args, "--muse", "--muse-home", home)
					wantCount = museFixtureEvents
				}
				_ = captureStdout(t, func() {
					if code := dispatch(args, os.Stdout, os.Stderr); code != 0 {
						t.Fatal(code)
					}
				})
				rows, _, err := db.ListTokenEvents(context.Background(), time.Time{}, time.Now().Add(time.Hour), store.PageCursor{}, 100)
				if err != nil || len(rows) != wantCount {
					t.Fatalf("rows=%+v err=%v want=%d", rows, err, wantCount)
				}
				for _, row := range rows {
					if keys == "" {
						if !store.IsUnattributed(row.IssueID) || row.IssueRule != "" {
							t.Fatalf("unconfigured event %+v", row)
						}
					} else if row.IssueID != "PROJ-9" || row.IssueRule != issueref.RuleBranchConfiguredKey {
						t.Fatalf("configured event %+v", row)
					}
				}
			})
		}
	}
}

func TestDoctorUnreadableDBContinuesCaptureChecks(t *testing.T) {
	t.Setenv("TIER_TRACKER_KEYS", "")
	t.Setenv("HOME", t.TempDir())
	path := defaultDBPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := dispatch([]string{"doctor", "--repo", initGitRepo(t), "--claude-dir", t.TempDir()}, &out, &errOut)
	if !strings.Contains(errOut.String(), "WARNING: tracker keys diagnostic:") {
		t.Fatal(errOut.String())
	}
	if !strings.Contains(out.String(), "Claude") || !strings.Contains(out.String(), "git") {
		t.Fatalf("capture checks skipped: code=%d out=%s err=%s", code, &out, &errOut)
	}
}

func TestShipCollectorRevertInfoVisible(t *testing.T) {
	t.Setenv("TIER_LOG_LEVEL", "")
	var out bytes.Buffer
	logger, err := newShipCollectorLogger(&out)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("routine successful scan")
	if out.Len() != 0 {
		t.Fatal(out.String())
	}
	logger.Info("revert-branch events left unattributed", "count", 2)
	if !strings.Contains(out.String(), "revert-branch events left unattributed") || !strings.Contains(out.String(), "2") {
		t.Fatal(out.String())
	}
}

func captureStrictStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old; _ = w.Close(); _ = r.Close() }()
	done := make(chan []byte, 1)
	go func() { raw, _ := io.ReadAll(r); done <- raw }()
	fn()
	_ = w.Close()
	return string(<-done)
}

func TestShipRevertCountWiring(t *testing.T) {
	t.Setenv("TIER_TRACKER_KEYS", "")
	t.Setenv("TIER_LOG_LEVEL", "")
	t.Setenv("TIER_CODEX_ROLLOUT", "")
	t.Setenv("TIER_OPENCODE", "")
	t.Setenv("TIER_MUSE", "")
	loadDeterministicPrices(t)
	for _, source := range []string{"jsonl", "codex", "muse"} {
		t.Run(source, func(t *testing.T) {
			repo, claude := initGitRepo(t), t.TempDir()
			srv, _ := newShipTestServer(t, "")
			args := []string{"ship", "--server", srv.URL, "--repo", repo, "--claude-dir", claude, "--since", "2026-01-01"}
			var path, oldBranch string
			want := "1"
			switch source {
			case "jsonl":
				writeSessionFixture(t, claude, repo)
				path, oldBranch = filepath.Join(claude, "projects", "p1", "s1.jsonl"), "feature/42-foo"
			case "codex":
				dir := stageCodexRollout(t, repo)
				path, oldBranch = filepath.Join(dir, "2026", "07", "23", "rollout-clean-session.jsonl"), "feature/42-foo"
				args = append(args, "--codex-rollout", "--codex-sessions-dir", dir)
				want = "4"
			case "muse":
				home := stageMuseHome(t, repo)
				path, oldBranch = filepath.Join(home, "sessions", "2026", "09", "21", "ses-fixture-0001", "session.jsonl"), "feature/895-muse-fixture"
				args = append(args, "--muse", "--muse-home", home)
				want = "4"
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(raw), oldBranch, "revert-9-x")), 0600); err != nil {
				t.Fatal(err)
			}
			logs := captureStrictStderr(t, func() {
				_ = captureStdout(t, func() {
					if code := dispatch(args, os.Stdout, os.Stderr); code != 0 {
						t.Fatal(code)
					}
				})
			})
			if strings.Count(logs, "revert-branch events left unattributed") != 1 || !strings.Contains(logs, `"level":"INFO"`) || !strings.Contains(logs, `"count":`+want) {
				t.Fatalf("ship %s logs=%s", source, logs)
			}
		})
	}
}

func TestServeTrackerDiagnosticFailureContinues(t *testing.T) {
	t.Setenv("TIER_TRACKER_KEYS", "")
	restoreEmbeddedPriceTable(t)
	logs := captureOSStderr(t)
	called := false
	code := runServeWithOptions([]string{
		"--db", filepath.Join(t.TempDir(), "tier.db"),
		"--addr", "127.0.0.1:0", "--aggregation", "team",
		"--watch-repo", filepath.Join(t.TempDir(), "not-a-repo"),
	}, serveOptions{issueIDCounts: func(context.Context) (map[string]int, error) {
		called = true
		return nil, fmt.Errorf("counts unavailable")
	}})
	out := logs()
	if !called || !strings.Contains(out, `"level":"WARN","msg":"tracker keys diagnostic","err":"counts unavailable"`) {
		t.Fatalf("called=%v logs=%s", called, out)
	}
	if code != 1 || !strings.Contains(out, "watch-repo:") {
		t.Fatalf("serve did not continue to watch-repo validation: code=%d logs=%s", code, out)
	}
}
