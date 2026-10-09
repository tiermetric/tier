//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// refusalDeadline bounds a child expected to exit on its own; one still running
// at the deadline did not refuse.
const refusalDeadline = 15 * time.Second

// runServeChild runs a real `tierd serve` child with args, env layered on a
// TIER_*-scrubbed environment, and returns its exit code and stderr. It is for
// runs that exit on their own (a refusal or -h). The child binds a free
// loopback port, never the default one, and is killed at refusalDeadline; a
// child still running then fails the test as "did not refuse".
func runServeChild(t *testing.T, args, env []string) (int, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), refusalDeadline)
	t.Cleanup(cancel)
	full := append([]string{"serve", "--addr", freeLoopbackPort(t)}, args...)
	cmd := exec.CommandContext(ctx, self)
	cmd.WaitDelay = time.Second
	cmd.Env = append(scrubbedEnv(os.Environ()), "TIERD_SMOKE_CHILD_ARGS="+strings.Join(full, "\n"))
	cmd.Env = append(cmd.Env, env...)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	err = cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("child did not refuse: still running after %s, killed; stderr:\n%s", refusalDeadline, stderr.String())
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, stderr.String()
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), stderr.String()
	default:
		t.Fatalf("run tierd child: %v", err)
		return 0, ""
	}
}

// bootServeChild boots a developer-mode loopback serve child and returns once
// /livez answers; a child that exits first fails the test with its stderr.
func bootServeChild(t *testing.T, args, env []string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	addr := freeLoopbackPort(t)
	full := append([]string{"serve", "--addr", addr, "--db", filepath.Join(t.TempDir(), "grace.db"), "--aggregation", "developer"}, args...)
	cmd := exec.Command(self)
	cmd.Env = append(scrubbedEnv(os.Environ()), "TIERD_SMOKE_CHILD_ARGS="+strings.Join(full, "\n"))
	cmd.Env = append(cmd.Env, env...)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tierd child: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGKILL) })
	waitUntilLive(t, "http://"+addr+"/api/v1/livez", stderr, exited)
}

// writeGraceConfig writes a config file whose only key is report_grace.
func writeGraceConfig(t *testing.T, value string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tier.yaml")
	if err := os.WriteFile(p, []byte("report_grace: \""+value+"\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// TestServeSmoke_ReportGraceWiring pins serve's own wiring of --report-grace:
// the flag's registered default, the TIER_REPORT_GRACE and report_grace sources,
// their precedence, and the startup refusals. A winner below 24h must refuse
// (the winner is read); a loser below 24h must not (the loser is ignored).
func TestServeSmoke_ReportGraceWiring(t *testing.T) {
	const refusal = "--report-grace must be >= 24h0m0s"

	t.Run("default is 336h in -h", func(t *testing.T) {
		code, stderr := runServeChild(t, []string{"-h"}, nil)
		if code != 0 || !strings.Contains(stderr, "-report-grace duration") || !strings.Contains(stderr, "(default 336h0m0s)") {
			t.Fatalf("serve -h = %d, want 0 listing -report-grace with (default 336h0m0s); stderr:\n%s", code, stderr)
		}
	})
	t.Run("env becomes the flag default", func(t *testing.T) {
		code, stderr := runServeChild(t, []string{"-h"}, []string{"TIER_REPORT_GRACE=48h"})
		if code != 0 || !strings.Contains(stderr, "(default 48h0m0s)") {
			t.Fatalf("TIER_REPORT_GRACE=48h serve -h = %d, want (default 48h0m0s); stderr:\n%s", code, stderr)
		}
	})

	refused := []struct {
		name string
		args []string
		env  []string
		want string
	}{
		{"cli below minimum", []string{"--report-grace", "1h"}, nil, refusal},
		{"env below minimum", nil, []string{"TIER_REPORT_GRACE=1h"}, refusal},
		{"config below minimum", []string{"--config", writeGraceConfig(t, "1h")}, nil, refusal},
		{"cli beats valid env", []string{"--report-grace", "1h"}, []string{"TIER_REPORT_GRACE=48h"}, refusal},
		{"env beats valid config", []string{"--config", writeGraceConfig(t, "48h")}, []string{"TIER_REPORT_GRACE=1h"}, refusal},
		{"invalid env", nil, []string{"TIER_REPORT_GRACE=14d"}, "TIER_REPORT_GRACE"},
		{"invalid env despite valid cli", []string{"--report-grace", "48h"}, []string{"TIER_REPORT_GRACE=14d"}, "TIER_REPORT_GRACE"},
		{"invalid config", []string{"--config", writeGraceConfig(t, "14d")}, nil, "config: report-grace: invalid duration"},
	}
	for _, tc := range refused {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			args := append([]string{"--db", filepath.Join(t.TempDir(), "grace.db"), "--aggregation", "developer"}, tc.args...)
			code, stderr := runServeChild(t, args, tc.env)
			if code != 1 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit = %d, want 1 with %q; stderr:\n%s", code, tc.want, stderr)
			}
		})
	}

	started := []struct {
		name string
		args []string
		env  []string
	}{
		{"valid cli beats env below minimum", []string{"--report-grace", "48h"}, []string{"TIER_REPORT_GRACE=1h"}},
		{"valid cli beats config below minimum", []string{"--report-grace", "48h", "--config", writeGraceConfig(t, "1h")}, nil},
		{"valid env beats config below minimum", []string{"--config", writeGraceConfig(t, "1h")}, []string{"TIER_REPORT_GRACE=48h"}},
	}
	for _, tc := range started {
		t.Run("starts: "+tc.name, func(t *testing.T) {
			bootServeChild(t, tc.args, tc.env)
		})
	}
}
