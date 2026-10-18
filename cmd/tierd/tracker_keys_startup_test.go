package main

import (
	"bytes"
	"encoding/json"
	"io"
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

func TestTrackerKeysCommandChild(t *testing.T) {
	raw := os.Getenv("TIER_STRICT_COMMAND_ARGS")
	if raw == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatal(err)
	}
	os.Exit(dispatch(args, os.Stdout, os.Stderr))
}

func trackerKeysChild(t *testing.T, args []string, env string) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestTrackerKeysCommandChild$")
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "TIER_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "TIER_STRICT_COMMAND_ARGS="+string(data), "TIER_TRACKER_KEYS="+env)
	return cmd
}

func TestTrackerKeysInvalidStartupEveryCommand(t *testing.T) {
	for _, command := range []string{"serve", "ship", "score", "backfill", "doctor"} {
		for _, source := range []string{"CLI", "env", "config"} {
			t.Run(command+"/"+source, func(t *testing.T) {
				args := []string{command}
				env := ""
				switch source {
				case "CLI":
					args = append(args, "--tracker-keys=bad-entry")
				case "env":
					env = "bad-entry"
				case "config":
					path := filepath.Join(t.TempDir(), "config.yaml")
					if err := os.WriteFile(path, []byte("attribution:\n  tracker_keys: bad-entry\n"), 0600); err != nil {
						t.Fatal(err)
					}
					args = append(args, "--config", path)
				}
				cmd := trackerKeysChild(t, args, env)
				out, err := cmd.CombinedOutput()
				if err == nil || !strings.Contains(string(out), `invalid tracker key "bad-entry"`) {
					t.Fatalf("err=%v output=%s", err, out)
				}
			})
		}
	}
}

func TestServeTrackerKeysStartupLine(t *testing.T) {
	for _, keys := range []string{"", "PROJ"} {
		t.Run(keys, func(t *testing.T) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := l.Addr().String()
			_ = l.Close()
			path := filepath.Join(t.TempDir(), "tier.db")
			cmd := trackerKeysChild(t, []string{"serve", "--addr", addr, "--db", path, "--aggregation", "team", "--tracker-keys", keys, "--log-format", "json"}, "")
			var logs bytes.Buffer
			cmd.Stderr = &logs
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
			ready := false
			client := &http.Client{Timeout: time.Second}
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				select {
				case err := <-done:
					exited = true
					t.Fatalf("serve exited %v %s", err, &logs)
				default:
				}
				resp, err := client.Get("http://" + addr + "/api/v1/livez")
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					ready = true
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if !ready {
				t.Fatal("serve did not start")
			}
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				exited = true
				if err != nil {
					t.Fatal(err, &logs)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("shutdown timed out")
			}
			want := "tracker keys: none configured (OP built in); unconfigured prefixes (suggest only): none"
			if keys != "" {
				want = "tracker keys: PROJ (OP built in); unconfigured prefixes (suggest only): none"
			}
			found := 0
			for _, line := range strings.Split(logs.String(), "\n") {
				var row map[string]any
				if json.Unmarshal([]byte(line), &row) == nil && row["msg"] == want {
					found++
					if row["level"] != "INFO" {
						t.Fatal(row)
					}
				}
			}
			if found != 1 {
				t.Fatalf("startup line count=%d logs=%s", found, &logs)
			}
		})
	}
}
