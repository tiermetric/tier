package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// serveVia routes one request through a mux built by the given register func and
// returns the status code. It mirrors doRequest but lets the caller pick
// Register vs RegisterReadOnly.
func serveVia(h *Handler, register func(*http.ServeMux), method, target string) int {
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	register(mux)
	mux.ServeHTTP(rec, req)
	return rec.Code
}

// TestRegisterReadOnly_OmitsEveryWriteRoute pins the #429 security invariant for
// the public demo instance: on a read-only mux EVERY write/ingest/admin route is
// STRUCTURALLY ABSENT — the request never reaches the (requireAuth) write handler.
// A structurally-absent route answers 404 (no pattern for the path) or 405 (a read
// twin exists for the path but not this method); the decisive point is that it is
// NEVER the 401 a MOUNTED-but-token-gated route returns without a header. The full
// Register mux is asserted to return that 401 for the SAME routes, proving they
// exist to be omitted (so a future write route dropped from registerWriteRoutes
// can't make this test pass vacuously).
func TestRegisterReadOnly_OmitsEveryWriteRoute(t *testing.T) {
	// Token set so a MOUNTED write route (full Register) 401s without an
	// Authorization header — the signal we distinguish "absent" (404/405) from.
	h, _ := newTestHandlerWithToken(t, "s3cret-test-token-9f3a")

	writeRoutes := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/costs"},
		{http.MethodPost, "/api/v1/events"},
		{http.MethodPost, "/api/v1/outcomes"},
		{http.MethodPost, "/api/v1/actual_spend"},
		{http.MethodPost, "/api/v1/org_actual_spend"},
		{http.MethodGet, "/api/v1/org_actual_spend"},
		{http.MethodPost, "/api/v1/developer_alias"},
		{http.MethodDelete, "/api/v1/developer_alias/somealias"},
		{http.MethodGet, "/api/v1/developer_alias"},
		{http.MethodPut, "/api/v1/org_hierarchy/somedev"},
		{http.MethodPost, "/api/v1/org_hierarchy"},
		{http.MethodGet, "/api/v1/org_hierarchy"},
		{http.MethodPost, "/api/v1/period_membership/somedev/end"},
		{http.MethodDelete, "/api/v1/developer/someid"},
		{http.MethodGet, "/api/v1/developer/someid/export"},
	}
	for _, r := range writeRoutes {
		ro := serveVia(h, h.RegisterReadOnly, r.method, r.path)
		if ro != http.StatusNotFound && ro != http.StatusMethodNotAllowed {
			t.Errorf("read-only %s %s = %d, want 404/405 (write route must be structurally absent, never reachable)", r.method, r.path, ro)
		}
		if ro == http.StatusUnauthorized || ro == http.StatusForbidden {
			t.Errorf("read-only %s %s = %d: route is MOUNTED (auth-gated), not structurally absent — a leaked token could reach it", r.method, r.path, ro)
		}
		if full := serveVia(h, h.Register, r.method, r.path); full != http.StatusUnauthorized {
			t.Errorf("full Register %s %s = %d, want 401 (route must be mounted+auth-gated so its read-only omission is meaningful)", r.method, r.path, full)
		}
	}

	// The read + health surface stays reachable on the read-only mux (never 404):
	// a mounted read route with a token set but no header returns 401 (present).
	readRoutes := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/scores"},
		{http.MethodGet, "/api/v1/scores/compare"},
		{http.MethodGet, "/api/v1/scores/somedev"},
		{http.MethodGet, "/api/v1/events"},
		{http.MethodGet, "/api/v1/outcomes"},
		{http.MethodGet, "/api/v1/quality_events"},
		{http.MethodGet, "/api/v1/quality_history"},
		{http.MethodGet, "/api/v1/fidelity"},
	}
	for _, r := range readRoutes {
		if ro := serveVia(h, h.RegisterReadOnly, r.method, r.path); ro == http.StatusNotFound {
			t.Errorf("read-only %s %s = 404, want the read route mounted (the demo dashboard needs it)", r.method, r.path)
		}
	}
	for _, p := range []string{"/api/v1/health", "/api/v1/healthz", "/api/v1/livez"} {
		if ro := serveVia(h, h.RegisterReadOnly, http.MethodGet, p); ro == http.StatusNotFound {
			t.Errorf("read-only GET %s = 404, want the health probe mounted", p)
		}
	}
}
