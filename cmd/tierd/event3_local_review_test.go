package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

func TestCorrectionReview_R2ServeReadOnly(t *testing.T) {
	if args := os.Getenv("U4_SERVE_CHILD"); args != "" {
		os.Exit(dispatch(strings.Split(args, "\n"), os.Stdout, os.Stderr))
	}
	for _, match := range []bool{true, false} {
		t.Run(fmt.Sprint(match), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "serve.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err = db.ConfigureCorrectionScoring(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			before, err := db.CorrectionScoringPolicy(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			month := u4SealScoresFixture(t, path)
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := listener.Addr().String()
			_ = listener.Close()
			args := []string{"serve", "--db", path, "--addr", addr, "--aggregation", "team", "--read-only"}
			if match {
				args = append(args, "--event3-scoring")
			}
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(self, "-test.run=^TestCorrectionReview_R2ServeReadOnly$")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "TIER_") && !strings.HasPrefix(entry, "U4_SERVE_CHILD=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "U4_SERVE_CHILD="+strings.Join(args, "\n"))
			logPath := filepath.Join(t.TempDir(), "serve.log")
			log, err := os.Create(logPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = log.Close() }()
			cmd.Stderr = log
			cmd.Stdout = log
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			stopped := false
			defer func() {
				if !stopped {
					_ = cmd.Process.Kill()
					if err := <-done; err != nil {
						t.Logf("tierd exit after kill: %v", err)
					}
				}
			}()
			client := http.Client{Timeout: 100 * time.Millisecond}
			ready := false
			exited := false
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				select {
				case err := <-done:
					t.Logf("tierd exited before livez answered: %v", err)
					stopped = true
					exited = true
				default:
				}
				if exited {
					break
				}
				resp, e := client.Get("http://" + addr + "/api/v1/livez")
				if e == nil {
					_ = resp.Body.Close()
					ready = resp.StatusCode == http.StatusOK
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if ready {
				resp, err := client.Get("http://" + addr + "/api/v1/scores?period=" + month)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("score response %d, want 200", resp.StatusCode)
				}
			}
			logs, _ := os.ReadFile(logPath)
			if match && (!ready || exited) {
				t.Fatalf("matching read-only serve failed: %s", logs)
			}
			if !match && (!exited || !strings.Contains(string(logs), "disagrees")) {
				t.Errorf("read-only mismatch was not refused: ready=%v exited=%v %s", ready, exited, logs)
			}
			reopened, e := store.Open(path)
			if e != nil {
				t.Fatal(e)
			}
			defer func() { _ = reopened.Close() }()
			after, e := reopened.CorrectionScoringPolicy(context.Background())
			if e != nil || after != before {
				t.Fatalf("read-only changed policy: before %+v after %+v: %v", before, after, e)
			}
		})
	}
}
