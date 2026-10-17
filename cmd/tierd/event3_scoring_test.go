package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestServe_Event3ScoringDefaultsOff(t *testing.T) {
	// The production serve entry point must register the default-off flag.
	found := false
	ast.Inspect(serveBody(t), func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok || len(c.Args) < 2 {
			return true
		}
		lit, ok := c.Args[0].(*ast.BasicLit)
		if ok && lit.Value == `"event3-scoring"` {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("serve has no default-off event3-scoring flag")
	}
	path := filepath.Join(t.TempDir(), "tier.db")
	if got := u4ServeRubric(t, path, "", "", "--event3-scoring"); got != 2 {
		t.Fatal(got)
	}
	// A restart with no enabling setting restores legacy even after an on run.
	if got := u4ServeRubric(t, path, "", ""); got != 1 {
		t.Fatal("restart auto-enabled scoring", got)
	}

}
func TestServe_Event3FlagPrecedence(t *testing.T) {
	found := false
	ast.Inspect(serveBody(t), func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok || exprName(c.Fun) != "applyBoolFromConfig" || len(c.Args) != 5 {
			return true
		}
		lit, ok := c.Args[3].(*ast.BasicLit)
		if ok && lit.Value == `"event3-scoring"` {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("serve does not apply Event 3 CLI > env > config > default precedence")
	}
	for _, tc := range []struct {
		name, env, cfg string
		args           []string
		want           int
	}{
		{"config", "", "true", nil, 2},
		{"env false over config true", "false", "true", nil, 1},
		{"env true over config false", "true", "false", nil, 2},
		{"CLI false over env/config true", "true", "true", []string{"--event3-scoring=false"}, 1},
		{"CLI true over env/config false", "false", "false", []string{"--event3-scoring"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := u4ServeRubric(t, filepath.Join(t.TempDir(), "tier.db"), tc.env, tc.cfg, tc.args...); got != tc.want {
				t.Fatal(got, tc.want)
			}
		})
	}

}

// This subprocess enters the actual serve parser, config resolver, store flip,
// mux and signal shutdown. It runs in the default (non-integration) test gate.
func TestServe_Event3Child(t *testing.T) {
	if args := os.Getenv("TIER_U4_CHILD_ARGS"); args != "" {
		os.Exit(runServe(strings.Split(args, "\n")))
	}
}
func u4ServeRubric(t *testing.T, dbPath, envValue, cfgValue string, args ...string) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	text := "aggregation: developer\n"
	if cfgValue != "" {
		text += "outcomes:\n  event3_scoring: " + cfgValue + "\n"
	}
	if err := os.WriteFile(cfg, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	childArgs := append([]string{"--addr", addr, "--db", dbPath, "--config", cfg, "--aggregation", "developer"}, args...)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestServe_Event3Child$")
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "TIER_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "TIER_U4_CHILD_ARGS="+strings.Join(childArgs, "\n"))
	if envValue != "" {
		cmd.Env = append(cmd.Env, "TIER_EVENT3_SCORING="+envValue)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	exited := false
	defer func() {
		if !exited {
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			exited = true
			t.Fatalf("serve exited: %v %s", err, stderr.String())
		default:
		}
		resp, err := client.Get("http://" + addr + "/api/v1/scores?since=2026-10-01&until=2026-11-01")
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		var b struct {
			Rubric struct {
				Version int `json:"version"`
			} `json:"rubric"`
		}
		err = json.NewDecoder(resp.Body).Decode(&b)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("score response %d: %v", resp.StatusCode, err)
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			exited = true
			if err != nil {
				t.Fatal(err, stderr.String())
			}
		case <-time.After(15 * time.Second):
			t.Fatal("shutdown timeout")
		}
		return b.Rubric.Version
	}
	t.Fatal("serve did not start")
	return 0
}
