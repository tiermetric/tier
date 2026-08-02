package health

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWatcherState_TransitionsAndSnapshot covers the basic lifecycle:
// NotConfigured → Running → Restarting → Stopped, and that Snapshot
// returns a coherent point-in-time view.
func TestWatcherState_TransitionsAndSnapshot(t *testing.T) {
	s := NewWatcherState()
	if snap := s.Snapshot(); snap.Status != StatusNotConfigured {
		t.Errorf("initial status = %q, want %q", snap.Status, StatusNotConfigured)
	}
	if !s.Snapshot().Healthy() {
		t.Error("NotConfigured must be healthy — running tierd without --watch-repo is valid")
	}

	s.SetRunning()
	snap := s.Snapshot()
	if snap.Status != StatusRunning || snap.LastError != "" {
		t.Errorf("after SetRunning: %+v", snap)
	}
	if snap.StartedAt.IsZero() {
		t.Error("StartedAt should be set after SetRunning")
	}

	wantErr := errors.New("synthetic transient failure")
	s.SetRestarting(wantErr, 2*time.Second)
	snap = s.Snapshot()
	if snap.Status != StatusRestarting || snap.LastError != wantErr.Error() {
		t.Errorf("after SetRestarting: %+v", snap)
	}
	if snap.RestartCount != 1 {
		t.Errorf("RestartCount = %d, want 1", snap.RestartCount)
	}
	if snap.NextRetryAt.IsZero() {
		t.Error("NextRetryAt should be set after SetRestarting")
	}
	if snap.Healthy() {
		t.Error("Restarting must not be healthy")
	}

	s.SetStopped(nil)
	snap = s.Snapshot()
	if snap.Status != StatusStopped || snap.LastError != "" {
		t.Errorf("after clean SetStopped: %+v", snap)
	}

	s.SetStopped(errors.New("terminal"))
	snap = s.Snapshot()
	if snap.LastError != "terminal" {
		t.Errorf("terminal error not retained: %+v", snap)
	}
}

// TestWatcherState_RecordWatchAddFailure pins the #142 contract: the count
// accumulates, the last error string is retained, the JSON field names are
// exactly what /healthz consumers bind to, and — critically — a degraded
// watcher stays Healthy() (a laptop fd limit must not flap readiness and evict
// a pod for something a restart cannot fix).
func TestWatcherState_RecordWatchAddFailure(t *testing.T) {
	s := NewWatcherState()
	s.SetRunning()

	if snap := s.Snapshot(); snap.WatchAddFailures != 0 || snap.LastWatchAddError != "" {
		t.Fatalf("fresh state: %+v", snap)
	}

	s.RecordWatchAddFailure(errors.New("too many open files"))
	s.RecordWatchAddFailure(errors.New("no space left on device"))

	snap := s.Snapshot()
	if snap.WatchAddFailures != 2 {
		t.Errorf("WatchAddFailures = %d, want 2", snap.WatchAddFailures)
	}
	if snap.LastWatchAddError != "no space left on device" {
		t.Errorf("LastWatchAddError = %q, want last error", snap.LastWatchAddError)
	}
	if !snap.Healthy() {
		t.Error("a running-but-degraded watcher must stay Healthy() — failures are a signal, not a 503")
	}

	// A supervisor restart must NOT zero the count: an fd-limit condition a
	// restart cannot recover would otherwise erase the signal. SetRunning clears
	// lastError but must leave watch-add failure state intact.
	s.SetRunning()
	snap = s.Snapshot()
	if snap.WatchAddFailures != 2 || snap.LastWatchAddError != "no space left on device" {
		t.Errorf("SetRunning must not reset watch-add failure state: %+v", snap)
	}

	// A nil error increments the count without clobbering the last error string.
	s.RecordWatchAddFailure(nil)
	snap = s.Snapshot()
	if snap.WatchAddFailures != 3 {
		t.Errorf("WatchAddFailures after nil = %d, want 3", snap.WatchAddFailures)
	}
	if snap.LastWatchAddError != "no space left on device" {
		t.Errorf("nil error must not clear LastWatchAddError, got %q", snap.LastWatchAddError)
	}

	// JSON contract: watch_add_failures always present, last_watch_add_error
	// present when set. These key names are the /healthz wire contract.
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	if !strings.Contains(js, `"watch_add_failures":3`) {
		t.Errorf("JSON missing watch_add_failures field: %s", js)
	}
	if !strings.Contains(js, `"last_watch_add_error":"no space left on device"`) {
		t.Errorf("JSON missing last_watch_add_error field: %s", js)
	}
}

// TestWatcherState_WatchAddFailureOmitsEmptyError confirms last_watch_add_error
// is omitted when no failure has occurred, so a healthy /healthz body is not
// polluted with an empty string field.
func TestWatcherState_WatchAddFailureOmitsEmptyError(t *testing.T) {
	s := NewWatcherState()
	s.SetRunning()
	b, err := json.Marshal(s.Snapshot())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	if strings.Contains(js, "last_watch_add_error") {
		t.Errorf("last_watch_add_error should be omitted when empty: %s", js)
	}
	if !strings.Contains(js, `"watch_add_failures":0`) {
		t.Errorf("watch_add_failures must always be present (no omitempty): %s", js)
	}
}

// TestWatcherState_ThreadSafe spins up writers and readers across many
// goroutines under -race to confirm the mutex actually protects state.
func TestWatcherState_ThreadSafe(t *testing.T) {
	s := NewWatcherState()
	const N = 100
	var wg sync.WaitGroup

	// Writers: alternate SetRunning / SetRestarting / RecordEvent.
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				s.SetRunning()
			case 1:
				s.SetRestarting(errors.New("x"), 100*time.Millisecond)
			case 2:
				s.RecordEvent()
			case 3:
				s.RecordWatchAddFailure(errors.New("too many open files"))
			}
		}(i)
	}

	// Readers: Snapshot repeatedly. The race detector will catch any
	// unlocked field access.
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Snapshot()
		}()
	}

	wg.Wait()
}
