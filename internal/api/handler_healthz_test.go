package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/health"
	"github.com/tiermetric/tier/internal/store"
)

// newHandlerWithState builds a Handler with a real on-disk SQLite store
// and the supplied watcher state. Used by the /healthz tests where state
// drives the response code/body.
func newHandlerWithState(t *testing.T, state *health.WatcherState) *Handler {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tier-healthz-test.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_ = os.Remove(path)
	})
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(db, quiet, "", state, "test", RateLimitConfig{})
}

// getHealthz dispatches a GET to /api/v1/healthz and decodes the body.
func getHealthz(t *testing.T, h *Handler) (int, healthzResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)
	var body healthzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v\nraw: %s", err, rec.Body.String())
	}
	return rec.Code, body
}

// stubSubsystem is a health.Snapshotter for exercising the extensible
// /healthz body (#48) without standing up a real collector.
type stubSubsystem struct {
	healthy bool
	detail  any
}

func (s stubSubsystem) HealthSnapshot() health.SubsystemSnapshot {
	return health.SubsystemSnapshot{Healthy: s.healthy, Detail: s.detail}
}

// TestHandleHealthz_RegisteredSubsystemAppears: a subsystem registered via
// RegisterSubsystem shows up under its name in the `subsystems` map alongside
// the watcher, and the top-level `healthy` mirrors the 200 status. This is the
// #48 extensibility contract: adding a subsystem needs no new top-level key.
func TestHandleHealthz_RegisteredSubsystemAppears(t *testing.T) {
	state := health.NewWatcherState()
	state.SetRunning()
	h := newHandlerWithState(t, state)
	h.RegisterSubsystem("anthropic_admin", stubSubsystem{healthy: true, detail: map[string]any{"status": "polling"}})

	code, body := getHealthz(t, h)
	if code != http.StatusOK {
		t.Errorf("status = %d, want 200 (all subsystems healthy)", code)
	}
	if !body.Healthy {
		t.Errorf("body.healthy = false, want true")
	}
	if _, ok := body.Subsystems["watcher"]; !ok {
		t.Errorf("subsystems missing watcher entry; got keys %v", keysOf(body.Subsystems))
	}
	sub, ok := body.Subsystems["anthropic_admin"]
	if !ok {
		t.Fatalf("subsystems missing anthropic_admin entry; got keys %v", keysOf(body.Subsystems))
	}
	if !sub.Healthy {
		t.Errorf("anthropic_admin.healthy = false, want true")
	}
}

// TestHandleHealthz_UnhealthySubsystemFlips503: a healthy watcher plus one
// unhealthy registered subsystem must drive /healthz to 503 and healthy=false,
// while the watcher entry still renders healthy. Proves the aggregate spans all
// subsystems, not just the watcher.
func TestHandleHealthz_UnhealthySubsystemFlips503(t *testing.T) {
	state := health.NewWatcherState()
	state.SetRunning()
	h := newHandlerWithState(t, state)
	h.RegisterSubsystem("anthropic_admin", stubSubsystem{healthy: false})

	code, body := getHealthz(t, h)
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (one subsystem unhealthy)", code)
	}
	if body.Healthy {
		t.Errorf("body.healthy = true, want false")
	}
	// The watcher itself is fine — the 503 comes solely from the collector.
	if body.Watcher.Status != health.StatusRunning {
		t.Errorf("watcher.status = %q, want %q", body.Watcher.Status, health.StatusRunning)
	}
	if !body.Subsystems["watcher"].Healthy {
		t.Errorf("watcher subsystem entry should stay healthy")
	}
}

// TestHandleHealthz_WatcherFieldsRenderInBothPlaces: the existing watcher
// fields must survive the #48 reshape — they appear both in the legacy
// top-level `watcher` block (backward compat) and under subsystems.watcher.
func TestHandleHealthz_WatcherFieldsRenderInBothPlaces(t *testing.T) {
	state := health.NewWatcherState()
	state.SetRunning()
	state.RecordWatchAddFailure(errors.New("too many open files"))
	h := newHandlerWithState(t, state)

	code, raw := getHealthzRaw(t, h)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	// Legacy top-level block preserved with watch_add_failures.
	legacy, ok := raw["watcher"].(map[string]any)
	if !ok {
		t.Fatalf("top-level watcher block missing or wrong type: %T", raw["watcher"])
	}
	if legacy["watch_add_failures"] != float64(1) {
		t.Errorf("legacy watcher.watch_add_failures = %v, want 1", legacy["watch_add_failures"])
	}
	if legacy["status"] != string(health.StatusRunning) {
		t.Errorf("legacy watcher.status = %v, want running", legacy["status"])
	}
	// Same fields available under subsystems.watcher.detail.
	subs, ok := raw["subsystems"].(map[string]any)
	if !ok {
		t.Fatalf("subsystems block missing: %T", raw["subsystems"])
	}
	watcherEntry, ok := subs["watcher"].(map[string]any)
	if !ok {
		t.Fatalf("subsystems.watcher missing: %T", subs["watcher"])
	}
	detail, ok := watcherEntry["detail"].(map[string]any)
	if !ok {
		t.Fatalf("subsystems.watcher.detail missing: %T", watcherEntry["detail"])
	}
	if detail["watch_add_failures"] != float64(1) {
		t.Errorf("subsystems.watcher.detail.watch_add_failures = %v, want 1", detail["watch_add_failures"])
	}
}

// getHealthzRaw dispatches GET /healthz and decodes into a generic map so a
// test can assert on the exact wire shape (not just the typed view).
func getHealthzRaw(t *testing.T, h *Handler) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v\nraw: %s", err, rec.Body.String())
	}
	return rec.Code, body
}

func keysOf(m map[string]health.SubsystemSnapshot) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestLivez_StaysAliveRegardlessOfWatcherState is the core #49 contract:
// /livez NEVER 503s on watcher state, because a liveness failure means "kill
// the pod" and a restarting watcher is exactly what the in-process supervisor
// handles. /healthz (readiness) DOES 503 on restarting/stopped — these are the
// states that would wrongly kill a pod if liveness were wired to /healthz.
// nil-watcher alone (the TestLivez case) doesn't prove this: it takes a
// different code path (not_configured) than a non-nil restarting/stopped state.
func TestLivez_StaysAliveRegardlessOfWatcherState(t *testing.T) {
	cases := []struct {
		name  string
		setup func() *health.WatcherState
	}{
		{"nil", func() *health.WatcherState { return nil }},
		{"running", func() *health.WatcherState {
			s := health.NewWatcherState()
			s.SetRunning()
			return s
		}},
		{"restarting", func() *health.WatcherState {
			s := health.NewWatcherState()
			s.SetRunning()
			s.SetRestarting(errors.New("transient"), 2*time.Second)
			return s
		}},
		{"stopped", func() *health.WatcherState {
			s := health.NewWatcherState()
			s.SetStopped(errors.New("terminal"))
			return s
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandlerWithState(t, tc.setup())
			req := httptest.NewRequest(http.MethodGet, "/api/v1/livez", nil)
			rec := httptest.NewRecorder()
			mux := http.NewServeMux()
			h.Register(mux)
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("GET /livez with watcher=%s: status = %d, want 200; body = %s",
					tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestHandleHealthz_NoWatcher: tierd serve without --watch-repo is a valid
// mode. /healthz returns 200 with status=not_configured.
func TestHandleHealthz_NoWatcher(t *testing.T) {
	h := newHandlerWithState(t, nil)
	code, body := getHealthz(t, h)
	if code != http.StatusOK {
		t.Errorf("status = %d, want 200 (NotConfigured is healthy)", code)
	}
	if body.Watcher.Status != health.StatusNotConfigured {
		t.Errorf("watcher.status = %q, want %q", body.Watcher.Status, health.StatusNotConfigured)
	}
}

// TestHandleHealthz_Running: the supervised path normal-running case.
// 200 + status=running.
func TestHandleHealthz_Running(t *testing.T) {
	state := health.NewWatcherState()
	state.SetRunning()
	h := newHandlerWithState(t, state)
	code, body := getHealthz(t, h)
	if code != http.StatusOK {
		t.Errorf("status = %d, want 200", code)
	}
	if body.Watcher.Status != health.StatusRunning {
		t.Errorf("watcher.status = %q, want %q", body.Watcher.Status, health.StatusRunning)
	}
}

// TestHandleHealthz_Restarting: supervisor is between attempts after a
// transient failure. 503 (Service Unavailable, transient) so a kube probe
// would not promote the pod to ready, but the dashboard can still render
// the JSON.
func TestHandleHealthz_Restarting(t *testing.T) {
	state := health.NewWatcherState()
	state.SetRunning()
	state.SetRestarting(errors.New("transient"), 2*time.Second)
	h := newHandlerWithState(t, state)
	code, body := getHealthz(t, h)
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
	if body.Watcher.Status != health.StatusRestarting {
		t.Errorf("watcher.status = %q, want %q", body.Watcher.Status, health.StatusRestarting)
	}
	if body.Watcher.LastError == "" {
		t.Errorf("LastError empty; want the synthetic transient message")
	}
	// The new aggregate fields must agree with the 503: the watcher subsystem
	// reports unhealthy and the top-level healthy bool is false.
	if body.Healthy {
		t.Errorf("body.healthy = true, want false (watcher restarting)")
	}
	if body.Subsystems["watcher"].Healthy {
		t.Errorf("subsystems.watcher.healthy = true, want false (watcher restarting)")
	}
}

// TestHandleHealthz_StoppedWithError: supervisor terminated after sustained
// failures. 503 with the terminal error.
func TestHandleHealthz_StoppedWithError(t *testing.T) {
	state := health.NewWatcherState()
	state.SetStopped(errors.New("terminal failure"))
	h := newHandlerWithState(t, state)
	code, body := getHealthz(t, h)
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
	if body.Watcher.Status != health.StatusStopped {
		t.Errorf("watcher.status = %q, want %q", body.Watcher.Status, health.StatusStopped)
	}
	if body.Watcher.LastError != "terminal failure" {
		t.Errorf("LastError = %q, want %q", body.Watcher.LastError, "terminal failure")
	}
	if body.Healthy {
		t.Errorf("body.healthy = true, want false (watcher stopped)")
	}
	if body.Subsystems["watcher"].Healthy {
		t.Errorf("subsystems.watcher.healthy = true, want false (watcher stopped)")
	}
}
