//go:build unix

package collector

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Pins: a FIFO where git keeps a pointer file (here the admin dir's commondir)
// makes classify return Unresolvable at once instead of blocking in open(2).
func TestWorktreeIndex_FIFOPointerDoesNotBlock(t *testing.T) {
	base := canonPath(t, t.TempDir())
	repo := wtRepo(t, base, "repo")
	wt := addWorktree(t, repo, filepath.Join(base, "wt"), "b1")
	commondir := filepath.Join(repo, ".git", "worktrees", "wt", "commondir")
	if err := os.Remove(commondir); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(commondir, 0o644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	x := newWorktreeIndex([]string{repo}, nil)
	done := make(chan pathClassification, 1)
	go func() { done <- x.classify(filepath.Join(wt, "f")) }()
	select {
	case got := <-done:
		if got != wantUnresolvable {
			t.Errorf("classify with a FIFO commondir = %+v, want Unresolvable", got)
		}
	case <-time.After(5 * time.Second):
		// Release the blocked open so the goroutine ends with the test.
		if w, err := os.OpenFile(commondir, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		t.Fatal("classify blocked on a FIFO commondir for 5s")
	}
}
