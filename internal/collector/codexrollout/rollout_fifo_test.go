//go:build unix

package codexrollout

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestFIFONamedLikeRolloutDoesNotHangTheCollector is the second half of the
// #464 Y-S3 regression, and the more dangerous one.
//
// os.Open on a FIFO with no writer BLOCKS until one appears. parseRollout has no
// timeout and checks no context, so a single FIFO named rollout-*.jsonl anywhere
// under ~/.codex/sessions would wedge the collector goroutine for the life of the
// process — Codex capture silently dead, with no error, no log line, and a
// perfectly healthy-looking server.
//
// It lives in its own unix-tagged file because mkfifo has no Windows equivalent.
// The test runs Collect on a goroutine and fails on a deadline rather than
// hanging the suite: if the IsRegular guard is ever removed, this reports the
// regression instead of timing the whole package out at ten minutes.
func TestFIFONamedLikeRolloutDoesNotHangTheCollector(t *testing.T) {
	repo := initGitRepo(t, t.TempDir())
	sessions := t.TempDir()
	dir := filepath.Join(sessions, "2026", "07", "23")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fifo := filepath.Join(dir, "rollout-fifo.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable on this filesystem: %v", err)
	}

	c := newTestCollector(t, sessions, RepoTarget{Path: repo})
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		evs, err := c.Collect(context.Background(), testSince)
		done <- result{len(evs), err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Collect: %v", got.err)
		}
		if got.n != 0 {
			t.Errorf("emitted %d events from a FIFO; want 0", got.n)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Collect blocked on a FIFO named like a rollout log — the collector goroutine is wedged for the life of the process; the non-regular-file guard in findRolloutFiles is missing")
	}
}
