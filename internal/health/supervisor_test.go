package health

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// runnableFunc adapts a closure to Runnable so tests can dial in failure
// patterns without standing up a real fsnotify watcher.
type runnableFunc func(ctx context.Context) error

func (r runnableFunc) Run(ctx context.Context) error { return r(ctx) }

// quietLogger keeps test output clean — the supervisor's WARN/ERROR logs
// would otherwise flood test output across the failure-pattern subtests.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestSupervisor_HappyPath: the Runnable returns nil promptly. Supervisor
// treats this as terminal (Runnable exited cleanly without cancellation —
// for an event loop that's "the loop closed", not a transient failure).
// State ends as Stopped with no error.
func TestSupervisor_HappyPath(t *testing.T) {
	state := NewWatcherState()
	sup := &Supervisor{
		Run:    runnableFunc(func(_ context.Context) error { return nil }),
		State:  state,
		Logger: quietLogger(),
	}
	if err := sup.Supervise(context.Background()); err != nil {
		t.Fatalf("Supervise: %v", err)
	}
	if snap := state.Snapshot(); snap.Status != StatusStopped || snap.LastError != "" {
		t.Errorf("state = %+v, want Stopped with no error", snap)
	}
}

// TestSupervisor_CleanShutdownOnCtxCancel: cancelling the context while
// the Runnable is blocked should bring Supervise to a clean stop.
func TestSupervisor_CleanShutdownOnCtxCancel(t *testing.T) {
	state := NewWatcherState()
	sup := &Supervisor{
		Run: runnableFunc(func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		}),
		State:  state,
		Logger: quietLogger(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Supervise(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Supervise returned %v on ctx cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Supervise did not return within 2s of ctx cancel")
	}
	if snap := state.Snapshot(); snap.Status != StatusStopped || snap.LastError != "" {
		t.Errorf("state = %+v, want Stopped clean", snap)
	}
}

// TestSupervisor_TransientFailureThenRecovery: the Runnable fails once,
// then succeeds (blocks until ctx cancel). The supervisor restarts after
// the backoff, the second run survives long enough to reset backoff, and
// the final state on cancel is clean.
func TestSupervisor_TransientFailureThenRecovery(t *testing.T) {
	var calls atomic.Int32
	state := NewWatcherState()
	sup := &Supervisor{
		Run: runnableFunc(func(ctx context.Context) error {
			n := calls.Add(1)
			if n == 1 {
				return errors.New("synthetic transient")
			}
			// Second call: block until cancelled.
			<-ctx.Done()
			return nil
		}),
		State:  state,
		Logger: quietLogger(),
		// Tight constants so the test runs fast.
		BackoffBase:    10 * time.Millisecond,
		BackoffMax:     50 * time.Millisecond,
		MaxFailures:    5,
		FailureWindow:  time.Second,
		ResetThreshold: 100 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Supervise(ctx) }()

	// Wait for both runs to start.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() < 2 {
		t.Fatalf("expected at least 2 Runnable calls, got %d", calls.Load())
	}

	// Let the second run survive past ResetThreshold so backoff resets.
	time.Sleep(150 * time.Millisecond)
	cancel()

	if err := <-done; err != nil {
		t.Errorf("Supervise: %v", err)
	}
	if snap := state.Snapshot(); snap.Status != StatusStopped || snap.LastError != "" {
		t.Errorf("state after recovery + cancel: %+v", snap)
	}
}

// TestSupervisor_SustainedFailureTerminates: the Runnable fails immediately
// every call. After MaxFailures within FailureWindow, Supervise returns the
// terminal error and state is Stopped with the error retained.
func TestSupervisor_SustainedFailureTerminates(t *testing.T) {
	state := NewWatcherState()
	var calls atomic.Int32
	sentinel := errors.New("sustained-failure-sentinel")
	sup := &Supervisor{
		Run: runnableFunc(func(_ context.Context) error {
			calls.Add(1)
			return sentinel
		}),
		State:  state,
		Logger: quietLogger(),
		// Use tiny tunables so the sustained-failure path fires quickly.
		BackoffBase:    time.Millisecond,
		BackoffMax:     time.Millisecond,
		MaxFailures:    3,
		FailureWindow:  time.Second,
		ResetThreshold: time.Hour, // make sure runs are NEVER long-lived
	}
	err := sup.Supervise(context.Background())
	if err == nil {
		t.Fatal("expected terminal error from sustained-failure path")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("terminal error doesn't wrap sentinel: got %v", err)
	}
	if got := calls.Load(); got < 3 {
		t.Errorf("Runnable called %d times, want >= 3 (MaxFailures)", got)
	}
	snap := state.Snapshot()
	if snap.Status != StatusStopped {
		t.Errorf("state = %q, want %q", snap.Status, StatusStopped)
	}
	if !strings.Contains(snap.LastError, sentinel.Error()) {
		t.Errorf("LastError doesn't mention sentinel; got %q", snap.LastError)
	}
}

// TestSupervisor_RejectsNilRun + Nil state — guards against misconfiguration.
func TestSupervisor_RejectsNil(t *testing.T) {
	if err := (&Supervisor{State: NewWatcherState()}).Supervise(context.Background()); err == nil {
		t.Error("nil Run should fail")
	}
	if err := (&Supervisor{Run: runnableFunc(func(_ context.Context) error { return nil })}).Supervise(context.Background()); err == nil {
		t.Error("nil State should fail")
	}
}

// TestSupervisor_BackoffEscalates exercises the exponential growth: a
// Runnable that fails repeatedly should see the supervisor wait
// progressively longer (capped at BackoffMax).
//
// Wider tolerances than a naive `gap2 > gap1` because a busy CI runner can
// stall a 20ms sleep by 30-50ms — we assert the gap grew by AT LEAST half
// the base step, which scheduling jitter shouldn't reverse.
func TestSupervisor_BackoffEscalates(t *testing.T) {
	state := NewWatcherState()
	var mu sync.Mutex
	starts := []time.Time{}
	const base = 100 * time.Millisecond
	sup := &Supervisor{
		Run: runnableFunc(func(_ context.Context) error {
			mu.Lock()
			starts = append(starts, time.Now())
			n := len(starts)
			mu.Unlock()
			if n >= 4 {
				return nil // exit cleanly after enough samples
			}
			return errors.New("x")
		}),
		State:          state,
		Logger:         quietLogger(),
		BackoffBase:    base,
		BackoffMax:     time.Second,
		MaxFailures:    100,
		FailureWindow:  time.Minute,
		ResetThreshold: time.Hour,
	}
	_ = sup.Supervise(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(starts) < 3 {
		t.Fatalf("expected at least 3 starts, got %d", len(starts))
	}
	// Gaps approximate 100ms, 200ms, 400ms. Assert gap2 exceeds gap1 by
	// at least half the base step — survives 50ms scheduling jitter
	// while still catching a "backoff never grew" regression.
	gap1 := starts[1].Sub(starts[0])
	gap2 := starts[2].Sub(starts[1])
	if gap2 < gap1+base/2 {
		t.Errorf("backoff didn't escalate (or jitter ate it): gap1=%v, gap2=%v, need gap2 >= gap1+%v",
			gap1, gap2, base/2)
	}
}
