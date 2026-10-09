//go:build integration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestServeSmoke_MuseWithoutWatchRepoRefusesToStart proves `serve --muse` is
// parsed and wired to museWatchCheck (#895): with no repo to attribute to, the
// child must exit non-zero naming the remedy, never start and drop Muse spend.
// Same shape as TestServeSmoke_CodexRolloutWithoutWatchRepoRefusesToStart.
func TestServeSmoke_MuseWithoutWatchRepoRefusesToStart(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	childArgs := strings.Join([]string{
		"serve",
		"--addr", freeLoopbackPort(t),
		"--db", filepath.Join(t.TempDir(), "muse-smoke.db"),
		"--aggregation", "developer",
		"--muse",
		// deliberately NO --watch-repo
	}, "\n")
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), "TIERD_SMOKE_CHILD_ARGS="+childArgs)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tierd child: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGKILL) })

	select {
	case err := <-exited:
		if err == nil {
			t.Fatalf("child exited ZERO with --muse and no --watch-repo; stderr:\n%s", stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("child still running after 10s; it must refuse to start. stderr:\n%s", stderr.String())
	}
	if logs := stderr.String(); !strings.Contains(logs, "--muse needs a repository") || !strings.Contains(logs, "--watch-repo") {
		t.Errorf("stderr must name the remedy; got:\n%s", logs)
	}
}
