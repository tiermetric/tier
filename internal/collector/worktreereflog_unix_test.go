//go:build unix

package collector

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Pins: a FIFO at logs/HEAD is refused at once, even when its pipe already
// holds a healthy reflog and no writer, so a read would succeed and resolve:
// only the regular-file check stands between it and an answer.
func TestBranchAt_FIFOReflogIsRefused(t *testing.T) {
	x, c, admin := reflogAdmin(t, historyHead, historyLog)
	logHead := filepath.Join(admin, "logs", "HEAD")
	if err := os.Remove(logHead); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(logHead, 0o644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	// Hold a reader open so the written bytes stay buffered after the writer
	// closes; the pipe then reads as the healthy reflog followed by EOF.
	holder, err := os.OpenFile(logHead, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	w, err := os.OpenFile(logHead, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(historyLog); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	type result struct {
		branch string
		ok     bool
	}
	done := make(chan result, 1)
	go func() {
		b, ok := x.branchAt(c, sec(1025), reflogNow)
		done <- result{b, ok}
	}()
	select {
	case r := <-done:
		if r.ok {
			t.Errorf("branchAt with a FIFO logs/HEAD = %q, want unresolvable", r.branch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("branchAt blocked on a FIFO logs/HEAD for 5s")
	}
}
