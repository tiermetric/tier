//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

func TestDemoPrivate_RefusesDB(t *testing.T) {
	for _, uri := range []bool{false, true} {
		t.Run(map[bool]string{false: "path", true: "URI_demo_suffix"}[uri], func(t *testing.T) {
			path := t.TempDir() // Bounds old startup without allowing a listener.
			if uri {
				path = "file:" + filepath.Join(path, "operator.db") + "?demo=1"
			}
			code, out := runSecretEnvChild(t, nil, "demo", "--db", path)
			if code != 1 || !strings.Contains(out, "demo: --db is refused") {
				t.Fatalf("exit=%d; want explicit --db refusal, got:\n%s", code, out)
			}
		})
	}
}

func privateDemoChild(t *testing.T, root, addr string, extraEnv ...string) (*exec.Cmd, *lockedBuffer, <-chan error) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "operator.yaml")
	if err := os.WriteFile(config, []byte("db: "+filepath.Join(root, "config.db")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(scrubbedEnv(os.Environ()), "TMPDIR="+root, "TIER_DB="+filepath.Join(root, "operator.db"), "TIER_CONFIG="+config,
		"TIERD_SMOKE_CHILD_ARGS="+strings.Join([]string{"demo", "--addr", addr}, "\n"))
	cmd.Env = append(cmd.Env, extraEnv...)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, stderr, exited
}

func stopPrivateDemo(t *testing.T, cmd *exec.Cmd, stderr *lockedBuffer, exited <-chan error, sig os.Signal) {
	t.Helper()
	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("shutdown: %v\n%s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("shutdown timed out\n%s", stderr.String())
	}
}

// Locate the private file through the test-owned temporary parent, never through
// a production log or operator-selectable database flag.
func privateDemoPaths(t *testing.T, root string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "tierd-demo-*", "demo.db"))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestDemoPrivate_ConcurrentPathsAndSignalCleanup(t *testing.T) {
	root := t.TempDir()
	a, b := freeLoopbackPort(t), freeLoopbackPort(t)
	c1, s1, e1 := privateDemoChild(t, root, a)
	waitUntilLive(t, "http://"+a+"/api/v1/livez", s1, e1)
	c2, s2, e2 := privateDemoChild(t, root, b)
	waitUntilLive(t, "http://"+b+"/api/v1/livez", s2, e2)
	paths := privateDemoPaths(t, root)
	if len(paths) != 2 {
		t.Errorf("two concurrent demos need different private paths; got %v", paths)
	}
	for _, path := range paths {
		info, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0700 {
			t.Errorf("private directory mode=%o, want 0700", info.Mode().Perm())
		}
		if strings.Contains(s1.String()+s2.String(), path) {
			t.Error("private database path was printed")
		}
	}
	stopPrivateDemo(t, c1, s1, e1, syscall.SIGINT)
	if len(privateDemoPaths(t, root)) != 1 {
		t.Error("SIGINT must remove only the first demo's directory")
	}
	stopPrivateDemo(t, c2, s2, e2, syscall.SIGTERM)
	for _, path := range paths {
		if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("private directory survives shutdown: %s (%v)", path, err)
		}
	}
}

// A separate process writes real data AFTER the demo binds, including under a
// demo identity. This is the measured race the previous inspect/delete guard
// could not prevent: read-only HTTP routes do not isolate the SQLite writer.
func TestDemoPrivate_OutsideWriter(t *testing.T) {
	if root := os.Getenv("TIER_DEMO_OUTSIDE_WRITER"); root != "" {
		for _, name := range []string{"tier-demo.db", "operator.db", "config.db"} {
			db, err := store.Open(filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			event := demoEvent("demo-ada", "REAL-SECRET", "claude-sonnet-5", 999, "real-outside-writer")
			event.Timestamp = time.Now().AddDate(0, 0, -3)
			if err := db.InsertTokenEvents(context.Background(), []store.TokenEvent{event}); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertHierarchy(context.Background(), "real-secret-person", "real-team", "real-division", "real-org", "test"); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	root := t.TempDir()
	addr := freeLoopbackPort(t)
	cmd, stderr, exited := privateDemoChild(t, root, addr)
	waitUntilLive(t, "http://"+addr+"/api/v1/livez", stderr, exited)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writer := exec.Command(self, "-test.run=^TestDemoPrivate_OutsideWriter$")
	writer.Env = append(scrubbedEnv(os.Environ()), "TIER_DEMO_OUTSIDE_WRITER="+root)
	if out, err := writer.CombinedOutput(); err != nil {
		t.Fatalf("outside writer: %v\n%s", err, out)
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/v1/scores", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "demo.tiermetric.org" // Tunnel Host remains supported.
	res, err := smokeClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var board struct {
		Developers []struct {
			Developer    string  `json:"developer"`
			TotalCostUSD float64 `json:"total_cost_usd"`
		} `json:"developers"`
	}
	if res.StatusCode != 200 {
		t.Fatalf("scores status=%d", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(&board); err != nil {
		t.Fatal(err)
	}
	if len(board.Developers) != len(demoData()) {
		t.Errorf("served %d developers, want synthetic cast %d", len(board.Developers), len(demoData()))
	}
	ada := false
	for _, d := range board.Developers {
		if !strings.HasPrefix(d.Developer, "demo-") {
			t.Errorf("real outside data served: %s", d.Developer)
		}
		if d.Developer == "demo-ada" {
			ada = true
			if math.Abs(d.TotalCostUSD-25.70) > .001 {
				t.Errorf("outside writer reached demo scores: ada=$%.2f, want $25.70", d.TotalCostUSD)
			}
		}
	}
	if !ada {
		t.Error("synthetic ada missing")
	}
	paths := privateDemoPaths(t, root)
	if len(paths) != 1 {
		t.Errorf("served path must be private, not operator/env/config selected: %v", paths)
	}
	stopPrivateDemo(t, cmd, stderr, exited, syscall.SIGTERM)
}

func TestDemoPrivate_StartupFailureCleanup(t *testing.T) {
	for _, failure := range []string{"occupied_address", "invalid_logger", "missing_secret_file", "invalid_capture_env", "invalid_event3_env", "invalid_k_env", "invalid_grace_env", "invalid_prices"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			var extraEnv []string
			switch failure {
			case "invalid_logger":
				extraEnv = []string{"TIER_LOG_FORMAT=invalid"}
			case "missing_secret_file":
				extraEnv = []string{"TIER_API_TOKEN=@" + filepath.Join(root, "missing-secret")}
			case "invalid_capture_env":
				extraEnv = []string{"TIER_CODEX_ROLLOUT=invalid"}
			case "invalid_event3_env":
				extraEnv = []string{"TIER_EVENT3_SCORING=invalid"}
			case "invalid_k_env":
				extraEnv = []string{"TIER_K_ANONYMITY=invalid"}
			case "invalid_grace_env":
				extraEnv = []string{"TIER_REPORT_GRACE=invalid"}
			case "invalid_prices":
				extraEnv = []string{"TIER_PRICES=" + filepath.Join(root, "missing-prices.yaml")}

			}
			_, stderr, exited := privateDemoChild(t, root, listener.Addr().String(), extraEnv...)
			select {
			case err := <-exited:
				var ee *exec.ExitError
				if !errors.As(err, &ee) || ee.ExitCode() != 1 {
					t.Fatalf("startup must exit 1, got %v\n%s", err, stderr.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("startup failure timeout\n%s", stderr.String())
			}
			dirs, err := filepath.Glob(filepath.Join(root, "tierd-demo-*"))
			if err != nil {
				t.Fatal(err)
			}
			if len(dirs) != 0 {
				t.Errorf("startup failure leaked private directory: %v", dirs)
			}
			if _, err := os.Stat(filepath.Join(root, "tier-demo.db")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("startup created operator-known fixed path (%v)", err)
			}
		})
	}
}

func TestDemoPrivate_BoolEnvPreflightTracksServeNames(t *testing.T) {
	const childEnv = "TIER_DEMO_BOOL_ENV_CHILD"
	const futureEnv = "TIER_FUTURE_SERVE_BOOL"
	if os.Getenv(childEnv) == "1" {
		serveBoolEnvNames[0] = futureEnv
		os.Exit(runDemo([]string{"--addr", "invalid"}, os.Stdout, os.Stderr))
	}
	root := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestDemoPrivate_BoolEnvPreflightTracksServeNames$")
	cmd.Env = append(scrubbedEnv(os.Environ()), "TMPDIR="+root, childEnv+"=1", futureEnv+"=invalid")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 || !strings.Contains(string(out), futureEnv) {
		t.Fatalf("malformed serve env must exit 1 naming %s: %v\n%s", futureEnv, err, out)
	}
	dirs, err := filepath.Glob(filepath.Join(root, "tierd-demo-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 0 {
		t.Errorf("new serve env name bypassed demo preflight and leaked private directory: %v", dirs)
	}
}
