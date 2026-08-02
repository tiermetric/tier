// Package health tracks runtime state of long-running subsystems and exposes
// it to /healthz consumers. Today the only tracked subsystem is the
// fsnotify watcher; the supervisor lives here too because watcher
// resilience and the state it reports are tightly coupled.
package health

import (
	"sync"
	"time"
)

// WatcherStatus is the high-level state of the watcher subsystem.
//
//   - StatusNotConfigured: tierd serve was started without --watch-repo, so
//     no watcher was constructed. /healthz reports this but does not 503 —
//     it's a legitimate operating mode.
//   - StatusRunning: Watcher.Run is currently executing.
//   - StatusRestarting: Run returned with an error and the supervisor is
//     waiting out the backoff before retrying.
//   - StatusStopped: ctx was cancelled cleanly OR the supervisor terminated
//     after MaxFailures inside the failure window. The LastError field
//     distinguishes the two cases (empty for clean shutdown).
type WatcherStatus string

const (
	StatusNotConfigured WatcherStatus = "not_configured"
	StatusRunning       WatcherStatus = "running"
	StatusRestarting    WatcherStatus = "restarting"
	StatusStopped       WatcherStatus = "stopped"
)

// WatcherState is the thread-safe snapshot of watcher state exposed via
// /healthz. All public methods take the internal mutex; callers from any
// goroutine are safe.
//
// The struct is intentionally small — a Prometheus exporter mapping over
// this in the future can read these fields directly without further
// transformation.
type WatcherState struct {
	mu                sync.RWMutex
	status            WatcherStatus
	lastError         string
	lastEventTS       time.Time
	startedAt         time.Time
	restartCount      int
	nextRetryAt       time.Time
	watchAddFailures  int
	lastWatchAddError string
}

// NewWatcherState constructs a state in StatusNotConfigured. Callers that
// actually start a watcher should follow with SetRunning before the first
// /healthz hit.
func NewWatcherState() *WatcherState {
	return &WatcherState{status: StatusNotConfigured}
}

// SetRunning is called by the supervisor when Watcher.Run begins. Resets
// LastError because we're past whatever caused the prior failure (if any).
func (w *WatcherState) SetRunning() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = StatusRunning
	w.lastError = ""
	w.startedAt = time.Now().UTC()
	w.nextRetryAt = time.Time{}
}

// SetRestarting is called when Run returned with err and the supervisor is
// about to wait for backoff before retrying. nextRetryAt lets /healthz tell
// an operator when the next attempt will happen.
//
// Always overwrites lastError — passing nil clears it. The previous
// behaviour silently kept a stale error if the caller forgot to forward
// the trigger, which made /healthz look broken in a confusing way.
func (w *WatcherState) SetRestarting(err error, backoff time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = StatusRestarting
	if err != nil {
		w.lastError = err.Error()
	} else {
		w.lastError = ""
	}
	w.restartCount++
	w.nextRetryAt = time.Now().UTC().Add(backoff)
}

// SetStopped marks the watcher permanently stopped. err == nil indicates
// clean shutdown (ctx cancellation); a non-nil err indicates terminal
// failure after exhausting restarts.
func (w *WatcherState) SetStopped(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = StatusStopped
	if err != nil {
		w.lastError = err.Error()
	} else {
		w.lastError = ""
	}
	w.nextRetryAt = time.Time{}
}

// RecordEvent updates the last-event timestamp. Called by the supervisor
// (or directly by a wrapped Ingester) every time an event flows through —
// useful for diagnosing "watcher status=running but no data" cases.
func (w *WatcherState) RecordEvent() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastEventTS = time.Now().UTC()
}

// RecordWatchAddFailure records a single fsnotify watch registration that
// failed (EMFILE/ENOSPC/permission), captured from the collector via its
// OnWatchAddFailure callback (#142). This is the operator signal for silent
// capture degradation: a macOS laptop that has exhausted its fd limit keeps
// reporting status=running while events quietly stop arriving for some repos.
//
// The count is cumulative across the process lifetime and is deliberately NOT
// reset by SetRunning: a supervisor restart cannot recover an fd-limit
// condition, so zeroing on restart would erase the very degradation this exists
// to surface. Mirrors RecordEvent's locking discipline.
func (w *WatcherState) RecordWatchAddFailure(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.watchAddFailures++
	if err != nil {
		w.lastWatchAddError = err.Error()
	}
}

// Snapshot returns a value-copy of the current state, suitable for
// serialising to JSON. Holding the lock for the copy keeps the snapshot
// internally consistent (no partial update). Time fields are nil-pointers
// when unset so the JSON output omits them entirely.
func (w *WatcherState) Snapshot() WatcherSnapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := WatcherSnapshot{
		Status:            w.status,
		LastError:         w.lastError,
		RestartCount:      w.restartCount,
		WatchAddFailures:  w.watchAddFailures,
		LastWatchAddError: w.lastWatchAddError,
	}
	if !w.lastEventTS.IsZero() {
		t := w.lastEventTS
		out.LastEventTS = &t
	}
	if !w.startedAt.IsZero() {
		t := w.startedAt
		out.StartedAt = &t
	}
	if !w.nextRetryAt.IsZero() {
		t := w.nextRetryAt
		out.NextRetryAt = &t
	}
	return out
}

// HealthSnapshot adapts the watcher to the generic subsystem Snapshotter
// contract (#48) so a *WatcherState can register directly into a
// health.Registry. Healthy mirrors WatcherSnapshot.Healthy (not_configured
// and running are healthy); Detail carries the full WatcherSnapshot so the
// /healthz body keeps rendering every existing watcher field under the
// subsystem's entry.
func (w *WatcherState) HealthSnapshot() SubsystemSnapshot {
	snap := w.Snapshot()
	return SubsystemSnapshot{Healthy: snap.Healthy(), Detail: snap}
}

// WatcherSnapshot is the immutable view returned by Snapshot. JSON tags
// match the /healthz contract documented in docs/how-it-works.md §9.
//
// Time fields are *time.Time so the standard library's omitempty actually
// omits unset values — `omitempty` on a value `time.Time` doesn't omit (the
// reflection check fails because time.Time is a non-empty struct), which
// would otherwise emit `"last_event_ts":"0001-01-01T00:00:00Z"` for fresh
// state and confuse /healthz consumers.
type WatcherSnapshot struct {
	Status       WatcherStatus `json:"status"`
	LastError    string        `json:"last_error,omitempty"`
	LastEventTS  *time.Time    `json:"last_event_ts,omitempty"`
	StartedAt    *time.Time    `json:"started_at,omitempty"`
	RestartCount int           `json:"restart_count"`
	NextRetryAt  *time.Time    `json:"next_retry_at,omitempty"`
	// WatchAddFailures is the cumulative count of failed fsnotify watch
	// registrations (#142) — non-zero means capture may be silently degraded
	// for some directories even while Status stays "running". Always emitted
	// (no omitempty) so a dashboard can bind to it unconditionally.
	WatchAddFailures  int    `json:"watch_add_failures"`
	LastWatchAddError string `json:"last_watch_add_error,omitempty"`
}

// Healthy returns true when /healthz should reply 200. A NotConfigured
// state is healthy — running tierd serve without --watch-repo is a
// legitimate mode, not a fault.
func (s WatcherSnapshot) Healthy() bool {
	return s.Status == StatusRunning || s.Status == StatusNotConfigured
}
