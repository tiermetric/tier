package health

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Runnable is the contract a supervised subsystem implements. collector.
// Watcher satisfies this directly via its Run method. Decoupled here so
// tests can substitute a stub that simulates failure patterns.
type Runnable interface {
	Run(ctx context.Context) error
}

// StateReporter is the lifecycle sink the Supervisor writes transitions to.
// Extracting it from the concrete *WatcherState (#48) decouples the
// Supervisor from the watcher specifically: any subsystem that wants
// backoff-restart supervision can supply its own state sink implementing
// these three transitions, and the Supervisor no longer reaches into
// watcher-specific fields. *WatcherState satisfies it unchanged.
type StateReporter interface {
	// SetRunning marks the supervised Runnable as executing.
	SetRunning()
	// SetRestarting records that Run returned err and the supervisor is
	// waiting out backoff before the next attempt.
	SetRestarting(err error, backoff time.Duration)
	// SetStopped marks the Runnable permanently stopped: err == nil for a
	// clean ctx-cancellation shutdown, non-nil for terminal failure.
	SetStopped(err error)
}

// Supervisor wraps a Runnable in exponential-backoff restart logic, with a
// failure-window safety valve that gives up after MaxFailures restarts
// inside FailureWindow (#28).
//
// Concrete behaviours:
//
//   - Clean shutdown (ctx cancelled): Run returns nil, supervisor stops,
//     state.SetStopped(nil).
//
//   - Single failure followed by a long-lived run: backoff resets to
//     BackoffBase as soon as the next run lasts longer than
//     ResetThreshold. The failure-count slate is NOT wiped — a flapping
//     pattern (60s up, 5s fail, 60s up, 5s fail, ...) still progresses
//     toward the MaxFailures-in-window terminal condition. Resetting
//     counts on every "long" run would let a misbehaving subsystem
//     restart indefinitely.
//
//   - Sustained failure (immediate Run returns N times inside the
//     window): supervisor calls state.SetStopped(err) and exits with
//     that error. The caller (cmd/tierd serve) treats this as fatal and
//     SHUTS THE PROCESS DOWN: the error is sent on srvErr, which drives
//     the same graceful-shutdown path an HTTP listener crash takes
//     (cmd/tierd/main.go, the `sup.Supervise` goroutine). The HTTP
//     listener does NOT keep serving.
//
//     That is deliberate. A tierd that answered /scores while silently
//     ingesting nothing would report a falling TIER score as if it were
//     a real efficiency regression, which is worse than being down. It
//     is also why any deployment MUST set a restart policy
//     (Restart=always / restart: unless-stopped) — see deploy/README.md.
//
//   - Backoff: BackoffBase, BackoffBase*2, ..., capped at BackoffMax.
//
// Supervisor is not safe for concurrent or repeated calls. Supervise
// reads its configuration once at entry and uses local copies; constructing
// a fresh Supervisor per Supervise invocation is the documented pattern.
type Supervisor struct {
	Run Runnable
	// State receives the lifecycle transitions (running/restarting/stopped).
	// Typed as the StateReporter interface (#48) so the Supervisor is not
	// coupled to *WatcherState; the watcher passes its *WatcherState here.
	State  StateReporter
	Logger *slog.Logger

	// BackoffBase is the starting backoff (default 1s if zero).
	BackoffBase time.Duration
	// BackoffMax caps exponential growth (default 32s if zero).
	BackoffMax time.Duration
	// MaxFailures is the rolling-window threshold (default 5).
	MaxFailures int
	// FailureWindow is the rolling window (default 60s).
	FailureWindow time.Duration
	// ResetThreshold: a Run lasting at least this long resets the
	// backoff (NOT the failure window — see type comment for why).
	// Default = FailureWindow.
	ResetThreshold time.Duration
}

// Supervise blocks until either the context is cancelled or the supervisor
// terminates after MaxFailures inside FailureWindow. Returns the
// terminating error (or nil for clean ctx-cancellation shutdown).
//
// State updates happen synchronously with the lifecycle transitions so a
// /healthz call interleaved with a restart sees a coherent state snapshot.
//
// Configuration is read once at entry into locals; the Supervisor struct
// is not mutated. Reusing the same struct across calls is not supported
// (the failureWindow history would survive), so callers should construct
// a fresh Supervisor per Supervise invocation.
func (s *Supervisor) Supervise(ctx context.Context) error {
	if s.Run == nil {
		return fmt.Errorf("supervisor: Run is required")
	}
	if s.State == nil {
		return fmt.Errorf("supervisor: State is required")
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Capture configuration in locals so the Supervisor struct stays
	// unmodified across the lifetime of Supervise. Callers that snapshot
	// the struct for metrics or for a /healthz response see the same
	// values they passed in.
	backoffBase := s.BackoffBase
	if backoffBase <= 0 {
		backoffBase = time.Second
	}
	backoffMax := s.BackoffMax
	if backoffMax <= 0 {
		backoffMax = 32 * time.Second
	}
	maxFailures := s.MaxFailures
	if maxFailures <= 0 {
		maxFailures = 5
	}
	failureWindowDur := s.FailureWindow
	if failureWindowDur <= 0 {
		failureWindowDur = time.Minute
	}
	resetThreshold := s.ResetThreshold
	if resetThreshold <= 0 {
		resetThreshold = failureWindowDur
	}

	backoff := backoffBase
	var failureWindow []time.Time

	for {
		s.State.SetRunning()
		runStart := time.Now()
		err := s.Run.Run(ctx)
		runDuration := time.Since(runStart)

		// Clean shutdown: ctx cancelled. Watcher.Run returns nil in this
		// case (see watcher.go's select on ctx.Done), so we hit either
		// nil err or ctx.Err() — both clean.
		if ctx.Err() != nil {
			s.State.SetStopped(nil)
			return nil
		}
		if err == nil {
			// Runnable returned cleanly without cancellation. For an
			// event loop that's unusual — treat as terminal so the
			// caller knows something exited deliberately (e.g.
			// fsnotify channel closed). Log so an operator who sees
			// /healthz reporting status=stopped with no LastError
			// understands the cause.
			logger.Warn("supervisor: Runnable returned nil without context cancellation; treating as terminal exit")
			s.State.SetStopped(nil)
			return nil
		}

		// Real failure. Reset backoff if this run survived past the
		// reset threshold — a recovered subsystem shouldn't pay
		// escalated backoff for an unrelated future failure. Failure
		// counts are NOT reset: a flapping subsystem still progresses
		// toward MaxFailures (see type comment).
		if runDuration >= resetThreshold {
			backoff = backoffBase
		}

		now := time.Now()
		failureWindow = append(failureWindow, now)
		// Trim expired entries by copy-compaction. The previous form
		// `failureWindow = failureWindow[1:]` advanced the slice header
		// without releasing the popped entries' references; copying
		// keeps the backing array bounded by MaxFailures.
		cutoff := now.Add(-failureWindowDur)
		drop := 0
		for drop < len(failureWindow) && failureWindow[drop].Before(cutoff) {
			drop++
		}
		if drop > 0 {
			failureWindow = failureWindow[:copy(failureWindow, failureWindow[drop:])]
		}

		if len(failureWindow) >= maxFailures {
			terminal := fmt.Errorf("supervisor: %d failures in %v, last: %w",
				maxFailures, failureWindowDur, err)
			s.State.SetStopped(terminal)
			logger.Error("supervisor terminating", "failures", len(failureWindow), "window", failureWindowDur, "err", err)
			return terminal
		}

		s.State.SetRestarting(err, backoff)
		logger.Warn("supervisor restarting", "err", err, "backoff", backoff, "failures_in_window", len(failureWindow))

		// Wait for backoff or cancellation.
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			s.State.SetStopped(nil)
			return nil
		case <-t.C:
		}
		// Re-check ctx in case it cancelled exactly as the timer fired
		// (both select cases ready; Go picks randomly). Without this we'd
		// waste one Runnable invocation that immediately sees ctx done.
		if ctx.Err() != nil {
			s.State.SetStopped(nil)
			return nil
		}

		backoff *= 2
		if backoff > backoffMax {
			backoff = backoffMax
		}
	}
}
