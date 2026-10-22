//go:build integration

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestServeSmoke_ReadOnlyNeutralizesCursorAdmin(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"totalUsageEventsCount":0,"pagination":{"currentPage":1,"numPages":1},"usageEvents":[]}`))
	}))
	defer server.Close()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "tier.yaml")
	if err := os.WriteFile(cfg, []byte("collectors:\n  cursor_admin:\n    api_key: smoke-key\n    api_base_url: "+server.URL+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	addr := freeLoopbackPort(t)
	cmd := exec.Command(self)
	args := []string{"serve", "--addr", addr, "--db", filepath.Join(dir, "cursor-ro.db"), "--aggregation", "team", "--read-only", "--config", cfg}
	cmd.Env = append(scrubbedEnv(os.Environ()), "TIERD_SMOKE_CHILD_ARGS="+strings.Join(args, "\n"))
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGKILL) })
	waitUntilLive(t, "http://"+addr+"/api/v1/livez", stderr, exited)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		logs := stderr.String()
		if calls.Load() > 0 || strings.Contains(logs, "Cursor Admin poller enabled") {
			t.Fatalf("read-only started Cursor poller: calls=%d logs=%s", calls.Load(), logs)
		}
		if strings.Contains(logs, "Cursor Admin poller disabled") && strings.Contains(logs, "Cursor Admin) are OFF") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("read-only disabled receipt missing: %s", stderr.String())
}
