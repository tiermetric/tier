package main

import (
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/tiermetric/tier/internal/health"
	"github.com/tiermetric/tier/internal/metrics"
)

// TestWatcherEventRecorder_BumpsBoth covers #50 + #67 together: each event must
// stamp the health state's last-event timestamp AND increment the metric.
func TestWatcherEventRecorder_BumpsBoth(t *testing.T) {
	reg := metrics.NewRegistry()
	counter := reg.NewCounter("tier_watcher_events_total", "events")
	state := health.NewWatcherState()
	rec := watcherEventRecorder{state: state, counter: counter}

	rec.RecordEvent()
	rec.RecordEvent()

	var sb strings.Builder
	reg.Render(&sb)
	if !strings.Contains(sb.String(), "tier_watcher_events_total 2") {
		t.Errorf("counter should be 2:\n%s", sb.String())
	}
	if state.Snapshot().LastEventTS == nil {
		t.Error("watcher state last_event_ts should be set after RecordEvent")
	}
}

// TestWatchAddErrno classifies fsnotify Add failures into the low-cardinality
// errno label. EMFILE (macOS/kqueue fd exhaustion) and ENOSPC (Linux inotify
// limit) are the two greppable limit classes; everything else is "other".
func TestWatchAddErrno(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{syscall.EMFILE, "emfile"},
		{syscall.ENOSPC, "enospc"},
		{errors.New("permission denied"), "other"},
		{syscall.EACCES, "other"},
		// Wrapped errors must still classify (errors.Is unwraps).
		{errors.Join(errors.New("watch add"), syscall.EMFILE), "emfile"},
	}
	for _, c := range cases {
		if got := watchAddErrno(c.err); got != c.want {
			t.Errorf("watchAddErrno(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// TestWatchAddFailureWire simulates the OnWatchAddFailure closure wired in
// runServe: an injected EMFILE both increments the errno-labelled counter (so a
// series appears in /metrics) AND lands in the health state that backs /healthz.
// This exercises the full observability path without exhausting real fds (#142).
func TestWatchAddFailureWire(t *testing.T) {
	sm := newServeMetrics("v1.2.3")
	state := health.NewWatcherState()
	state.SetRunning()

	// The exact closure body from runServe.
	onFailure := func(_ string, err error) {
		state.RecordWatchAddFailure(err)
		sm.watcherWatchAddFail.Inc(watchAddErrno(err))
	}
	onFailure("/home/x/.claude/projects/-repo", syscall.EMFILE)
	onFailure("/home/x/.claude/projects/-repo/sub", syscall.EMFILE)

	var sb strings.Builder
	sm.reg.Render(&sb)
	const want = `tier_watcher_watch_add_failures_total{errno="emfile"} 2`
	if !strings.Contains(sb.String(), want) {
		t.Errorf("rendered metrics missing %q:\n%s", want, sb.String())
	}

	snap := state.Snapshot()
	if snap.WatchAddFailures != 2 {
		t.Errorf("health watch_add_failures = %d, want 2", snap.WatchAddFailures)
	}
	if !snap.Healthy() {
		t.Error("a degraded-but-running watcher must stay Healthy() (200)")
	}
}

// TestNewServeMetrics_RegistersSet pins the fixed metric set (and catches a
// duplicate-name panic regression in the set itself).
func TestNewServeMetrics_RegistersSet(t *testing.T) {
	sm := newServeMetrics("v1.2.3")
	var sb strings.Builder
	sm.reg.Render(&sb)
	out := sb.String()
	for _, want := range []string{
		"tier_http_requests_total",
		"tier_http_request_duration_seconds",
		"tier_watcher_events_total",
		"tier_proxy_writes_total",
		`tier_build_info{version="v1.2.3"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metric set missing %q:\n%s", want, out)
		}
	}
}

// TestProxyWrites_LabelOrder pins the {provider,outcome} label order of the
// production counter. recordWrite calls Inc(provider, outcome) positionally
// (internal/proxy), so if newServeMetrics ever declared the labels reversed,
// every proxy-side test would still pass while the rendered exposition would be
// mislabeled (provider="ok"). Render the real CounterVec and assert the exact
// labelled series.
func TestProxyWrites_LabelOrder(t *testing.T) {
	sm := newServeMetrics("v1.2.3")
	sm.proxyWrites.Inc("anthropic", "ok")
	var sb strings.Builder
	sm.reg.Render(&sb)
	const want = `tier_proxy_writes_total{provider="anthropic",outcome="ok"} 1`
	if !strings.Contains(sb.String(), want) {
		t.Errorf("rendered metrics missing %q:\n%s", want, sb.String())
	}
}
