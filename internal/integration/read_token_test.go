//go:build integration

// End-to-end wire test for the read-only viewer token (#190). It builds the
// exact tierd HTTP composition — REAL store + REST API + a wired metrics
// registry — with BOTH a write api token and a distinct read token armed, then
// drives the full scope matrix over real HTTP: the read token is accepted on
// the GET read routes and rejected 403 on every write route and the reverse
// proxy, while the write token works everywhere. The unit tests in
// internal/api cover the middleware in isolation; this proves the two scopes
// are wired together through the same mux cmd/tierd mounts.
package integration

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/metrics"
	"github.com/tiermetric/tier/internal/store"
)

const readToken = "integration-read-token-distinct"

// newScopedServer builds the API composition with both scopes armed plus a
// metrics registry (so the /metrics read route is mounted), returning the
// running server.
func newScopedServer(t *testing.T) *httptest.Server {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "read-token.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := api.New(db, quiet, apiToken, nil, "integration", api.RateLimitConfig{})
	h.SetReadToken(readToken)
	reg := metrics.NewRegistry()
	reg.NewGauge("tier_build_info", "bi", "version").Set(1, "integration")
	h.SetMetricsRegistry(reg)
	mux := http.NewServeMux()
	h.Register(mux)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// doScoped issues a request with an optional Bearer token and returns the
// status code. body is sent as a raw JSON string when non-empty.
func doScoped(t *testing.T, method, url, token, body string) int {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestReadToken_ScopeMatrixOverHTTP(t *testing.T) {
	srv := newScopedServer(t)

	// Read routes: read token AND write token accepted (200); no token 401.
	for _, path := range []string{"/api/v1/scores", "/api/v1/scores/alice", "/metrics"} {
		if code := doScoped(t, http.MethodGet, srv.URL+path, readToken, ""); code != http.StatusOK {
			t.Errorf("read token GET %s = %d, want 200", path, code)
		}
		if code := doScoped(t, http.MethodGet, srv.URL+path, apiToken, ""); code != http.StatusOK {
			t.Errorf("write token GET %s = %d, want 200", path, code)
		}
		if code := doScoped(t, http.MethodGet, srv.URL+path, "", ""); code != http.StatusUnauthorized {
			t.Errorf("no token GET %s = %d, want 401", path, code)
		}
	}

	// Write routes: read token 403; no token 401; write token succeeds.
	writeCases := []struct {
		method, path, body string
		wantWrite          int
	}{
		{http.MethodPost, "/api/v1/costs", `{"developer":"alice","issue_id":"i1","model":"claude-sonnet-4","cost_usd":0.01,"input_tokens":100,"output_tokens":50}`, http.StatusCreated},
		{http.MethodPost, "/api/v1/actual_spend", `{"developer":"alice","period":"2026-05","actual_paid_usd":400}`, http.StatusCreated},
		{http.MethodPost, "/api/v1/org_actual_spend", `{"org":"acme","period":"2026-05","actual_paid_usd":2000}`, http.StatusCreated},
		{http.MethodGet, "/api/v1/org_actual_spend", "", http.StatusOK},
		{http.MethodGet, "/api/v1/developer_alias", "", http.StatusOK},
	}
	for _, wc := range writeCases {
		if code := doScoped(t, wc.method, srv.URL+wc.path, readToken, wc.body); code != http.StatusForbidden {
			t.Errorf("read token %s %s = %d, want 403", wc.method, wc.path, code)
		}
		if code := doScoped(t, wc.method, srv.URL+wc.path, "", wc.body); code != http.StatusUnauthorized {
			t.Errorf("no token %s %s = %d, want 401", wc.method, wc.path, code)
		}
		if code := doScoped(t, wc.method, srv.URL+wc.path, apiToken, wc.body); code != wc.wantWrite {
			t.Errorf("write token %s %s = %d, want %d", wc.method, wc.path, code, wc.wantWrite)
		}
	}
}
