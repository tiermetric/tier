//go:build integration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// wtServeEnv is the child's environment: HOME is the watcher's Claude home, and
// every capture switch the parent may export is cleared.
func wtServeEnv(home string, extra ...string) []string {
	env := append(os.Environ(), "HOME="+home, "TIER_CODEX_ROLLOUT=", "TIER_OPENCODE=", "TIER_MUSE=",
		"TIER_API_TOKEN=", "TIER_READ_TOKEN=", "TIER_METRICS_TOKEN=", "TIER_PRICES=", worktreeAttrEnv+"=")
	return append(env, extra...)
}

// runWTServe boots a real serve child watching the wtShipFixture repo through a
// RELATIVE --watch-repo, writes the fixture transcript once the watcher is up,
// waits for the watcher to land an event, stops serve, and returns the stored
// "issue rule" rows and serve's stderr.
func runWTServe(t *testing.T, extra ...string) ([]string, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	repo, _, fixtureClaude := wtShipFixture(t)
	transcript, err := os.ReadFile(filepath.Join(fixtureClaude, "projects", "p-wt", "s.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	projectDir := filepath.Join(home, ".claude", "projects", "p-wt")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	addr := freeLoopbackPort(t)
	dbPath := filepath.Join(t.TempDir(), "wt.db")
	args := append([]string{"serve", "--addr", addr, "--db", dbPath, "--aggregation", "developer",
		"--log-format", "json", "--watch-repo", filepath.Base(repo)}, extra...)
	cmd := exec.Command(self)
	cmd.Dir = filepath.Dir(repo)
	cmd.Env = wtServeEnv(home, "TIERD_SMOKE_CHILD_ARGS="+strings.Join(args, "\n"))
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGKILL) })

	base := "http://" + addr
	waitUntilLive(t, base+"/api/v1/livez", stderr, exited)
	waitForWT(t, "the watcher to start", stderr, func() bool { return strings.Contains(stderr.String(), `"msg":"watcher started"`) })
	// With attribution on, the watcher holds a transcript's last message until
	// the file has been idle for collector.idleRelease (600s); an mtime set
	// before the debounced read makes the file read as idle.
	path := filepath.Join(projectDir, "s.jsonl")
	if err := os.WriteFile(path, transcript, 0o644); err != nil {
		t.Fatal(err)
	}
	idle := time.Now().Add(-20 * time.Minute)
	if err := os.Chtimes(path, idle, idle); err != nil {
		t.Fatal(err)
	}
	waitForWT(t, "the watcher to land the event", stderr, func() bool {
		_, body := httpGet(t, base+"/api/v1/healthz")
		return strings.Contains(body, `"last_event_ts"`)
	})

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("serve exited non-zero on SIGTERM: %v\n%s", err, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("serve did not stop within 15s of SIGTERM\n%s", stderr.String())
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	return storedIssueRules(t, db), stderr.String()
}

func waitForWT(t *testing.T, what string, stderr *lockedBuffer, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s; stderr:\n%s", what, stderr.String())
}

// Pins serve's wiring of --worktree-attribution into its watcher: on from the
// flag, and on from the config key alone, the watcher stores the worktree
// branch's issue with its rule; off stores today's rule with none. The watched
// repo is given relative, so the index must be built from the resolved paths
// the watcher matches on.
func TestServeSmoke_WorktreeAttributionReachesWatcher(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "tier.yaml")
	if err := os.WriteFile(cfgPath, []byte("watch:\n  worktree_attribution: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		extra    []string
		want     string
		wantFrom string
	}{
		{"off", nil, "unattributed:main", "off (from default)"},
		{"flag", []string{"--worktree-attribution"}, "issue-44 worktree-toolpath", "on (from flag --worktree-attribution)"},
		{"config", []string{"--config", cfgPath}, "issue-44 worktree-toolpath", "on (from config watch.worktree_attribution)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, logs := runWTServe(t, tc.extra...)
			if !reflect.DeepEqual(got, []string{tc.want}) {
				t.Errorf("stored %q, want [%q]; stderr:\n%s", got, tc.want, logs)
			}
			if !strings.Contains(logs, tc.wantFrom) {
				t.Errorf("startup line %q missing; stderr:\n%s", tc.wantFrom, logs)
			}
		})
	}
}

// Pins that ship, score and serve each refuse a malformed
// TIER_WORKTREE_ATTRIBUTION with exit 1 even when --worktree-attribution is
// given, as the envBool siblings do.
func TestSmoke_MalformedWorktreeAttributionEnvFailsEveryCommand(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"ship", "--server", "http://127.0.0.1:1", "--repo", dir, "--claude-dir", dir, "--worktree-attribution"},
		{"score", "--repo", dir, "--claude-dir", dir, "--worktree-attribution"},
		{"serve", "--addr", freeLoopbackPort(t), "--db", filepath.Join(dir, "x.db"), "--aggregation", "developer", "--worktree-attribution"},
	} {
		t.Run(args[0], func(t *testing.T) {
			cmd := exec.Command(self)
			cmd.Env = wtServeEnv(t.TempDir(), worktreeAttrEnv+"=bogus", "TIERD_SMOKE_CHILD_ARGS="+strings.Join(args, "\n"))
			stderr := &lockedBuffer{}
			cmd.Stderr = stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGKILL) })
			select {
			case err := <-exited:
				if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
					t.Errorf("exit = %v, want exit status 1; stderr:\n%s", err, stderr.String())
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("still running after 20s; a malformed env must refuse to start. stderr:\n%s", stderr.String())
			}
			if want := worktreeAttrEnv + ` must be a boolean (true|false|1|0), got "bogus"`; !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr missing %q:\n%s", want, stderr.String())
			}
		})
	}
}
