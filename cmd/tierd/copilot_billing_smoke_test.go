//go:build integration

package main

import (
	"fmt"
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

	"github.com/tiermetric/tier/internal/store"
)

func TestServeSmoke_ReadOnlyNeutralizesCopilotBilling(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if _, err := fmt.Fprintf(w, `{"timePeriod":{"year":%s,"month":%s,"day":%s},"organization":"acme","usageItems":[{"model":"m","product":"copilot","grossAmount":1}]}`, r.URL.Query().Get("year"), r.URL.Query().Get("month"), r.URL.Query().Get("day")); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	cfgPath, dbPath := filepath.Join(dir, "tier.yaml"), filepath.Join(dir, "tier.db")
	body := fmt.Sprintf("collectors:\n  copilot_billing:\n    org: acme\n    token: smoke-secret\n    api_base_url: %s\n", upstream.URL)
	if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	addr := freeLoopbackPort(t)
	cmd := exec.Command(self)
	cmd.Env = append(scrubbedEnv(os.Environ()), "TIERD_SMOKE_CHILD_ARGS="+strings.Join([]string{"serve", "--addr", addr, "--db", dbPath, "--aggregation", "team", "--read-only", "--config", cfgPath}, "\n"))
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
		if strings.Contains(logs, "Copilot billing poller enabled") {
			t.Fatalf("read-only started Copilot poller: %s", logs)
		}
		if strings.Contains(logs, "Copilot billing poller disabled") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(stderr.String(), "Copilot billing poller disabled") {
		t.Fatalf("missing disabled log: %s", stderr.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("read-only called GitHub %d times", calls.Load())
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := <-exited; err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	rows, _, err := db.ListTokenEvents(t.Context(), time.Now().AddDate(0, 0, -31), time.Now(), store.PageCursor{}, 100)
	if err != nil || len(rows) != 0 {
		t.Fatalf("read-only stored events: %d, err=%v", len(rows), err)
	}
}
