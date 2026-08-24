package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/metrics"
	"github.com/tiermetric/tier/internal/store"
)

// newTestHandler returns a Handler with auth DISABLED (empty token), which
// matches the pre-#22 behavior most existing tests assume. The auth-specific
// tests use newTestHandlerWithToken to configure a bearer token explicitly.
// Backing store is a fresh on-disk SQLite database (not :memory:) so the same
// pool and locking semantics apply as in production. (This said
// "SetMaxOpenConns(1) semantics" until #669 raised the pool to maxOpenConns.
// It opens through store.Open, so it inherits whatever production uses and the
// claim stays true without naming a number that drifts.)
func newTestHandler(t *testing.T) (*Handler, *store.DB) {
	return newTestHandlerWithToken(t, "")
}

// newTestHandlerWithToken lets a test configure the bearer token. Quiet
// logger by default so the "TIER_API_TOKEN not set" startup warning
// doesn't pollute test output.
func newTestHandlerWithToken(t *testing.T, token string) (*Handler, *store.DB) {
	return newTestHandlerWithTokenAndLimit(t, token, RateLimitConfig{})
}

// newTestHandlerWithTokenAndLimit is the rate-limit-aware variant used by the
// #36 lockout tests. Everything else goes through newTestHandlerWithToken with
// the limiter disabled.
func newTestHandlerWithTokenAndLimit(t *testing.T, token string, rl RateLimitConfig) (*Handler, *store.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tier-api-test.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_ = os.Remove(path)
	})
	// io.Discard sink keeps the "TIER_API_TOKEN is not set" warning out of
	// test output for the token="" case. The auth tests don't care about
	// the warning's content — they exercise the runtime path.
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	// nil watcherState — these handler tests don't exercise /healthz.
	// The healthz-specific tests in handler_healthz_test.go inject a real
	// state instance.
	return New(db, quiet, token, nil, "test", rl), db
}

// seedCosts is a small helper that inserts list-price cost rows so /scores
// has something to compute against. The TIER field of the response is not
// exercised here — that's covered by scoring/engine_test.go.
//
// InputTok is set well above scoring.MinAttributableTokens so a cost-bearing
// issue also clears the #136 zero-token tripwire: in production a non-zero cost
// always derives from non-zero tokens, so seeding cost without tokens would be
// an unrealistic fixture that spuriously trips the flag. Tests that specifically
// exercise the tripwire seed a zero-token outcome directly (no seedCosts call).
func seedCosts(t *testing.T, db *store.DB, developer, issueID string, costUSD float64) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: developer,
		IssueID:   issueID,
		Model:     "claude-sonnet-4",
		InputTok:  2000, // > scoring.MinAttributableTokens (1000): not zero-token flagged
		CostMicro: store.DollarsToMicro(costUSD),
		Source:    "jsonl",
		Fidelity:  "realtime",
		Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}
}

// seedTokens inserts a token_event carrying tokens (input) but no cost, so a
// test can give an outcome's issue enough recorded tokens to clear the #136
// zero-token tripwire without perturbing the developer's cost-based TIER/floor
// assertions. ts is now, inside the 14-day attributable window.
func seedTokens(t *testing.T, db *store.DB, developer, issueID string, tokens int) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: developer,
		IssueID:   issueID,
		Model:     "claude-sonnet-4",
		InputTok:  tokens,
		Source:    "jsonl",
		Fidelity:  "realtime",
		Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertTokenEvent (tokens): %v", err)
	}
}

// doRequest is a thin wrapper that returns status + body for a request routed
// through the handler's full ServeMux. Going through the mux (not the handler
// method directly) exercises the method-and-path routing too.
func doRequest(t *testing.T, h *Handler, method, target string, body any) (int, []byte) {
	t.Helper()
	var b io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		b = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, target, b)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// --- #22 auth tests ---

// doRequestWithHeader is the auth-aware variant of doRequest. Used by the
// auth tests; the no-header doRequest is fine for everything that runs
// against an unauth'd handler.
func doRequestWithHeader(t *testing.T, h *Handler, method, target string, body any, header http.Header) (int, []byte) {
	t.Helper()
	var b io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		b = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, target, b)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// validCostPayload is a minimal POST /costs body that passes validation —
// used by auth tests where the focus is the auth gate, not the field-level
// validation already covered by TestPostCosts_*.
func validCostPayload() map[string]any {
	return map[string]any{
		"developer":     "alice",
		"issue_id":      "issue-42",
		"model":         "claude-sonnet-4",
		"cost_usd":      0.0105,
		"input_tokens":  100,
		"output_tokens": 50,
	}
}

// TestPostCosts_AuthRequired covers the four auth paths in one table.
func TestPostCosts_AuthRequired(t *testing.T) {
	const token = "s3cret-test-token-9f3a"

	cases := []struct {
		name       string
		token      string // handler config; "" disables auth
		header     string // exact Authorization header value; "" omits the header
		wantStatus int
	}{
		{"no token configured + no header -> open (201)", "", "", http.StatusCreated},
		{"token configured + no header -> 401", token, "", http.StatusUnauthorized},
		{"token configured + wrong scheme -> 401", token, "Basic " + token, http.StatusUnauthorized},
		{"token configured + wrong token -> 401", token, "Bearer wrong-token-of-similar-length", http.StatusUnauthorized},
		{"token configured + correct token -> 201", token, "Bearer " + token, http.StatusCreated},
		{"token configured + length-mismatch token -> 401", token, "Bearer short", http.StatusUnauthorized},
		{"token configured + empty bearer -> 401", token, "Bearer ", http.StatusUnauthorized},
		// RFC 7235 §2.1: auth-scheme is case-insensitive.
		{"token configured + lowercase scheme -> 201", token, "bearer " + token, http.StatusCreated},
		{"token configured + uppercase scheme -> 201", token, "BEARER " + token, http.StatusCreated},
		{"token configured + mixed case scheme -> 201", token, "BeArEr " + token, http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHandlerWithToken(t, tc.token)
			header := http.Header{}
			if tc.header != "" {
				header.Set("Authorization", tc.header)
			}
			code, body := doRequestWithHeader(t, h, http.MethodPost, "/api/v1/costs",
				validCostPayload(), header)
			if code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", code, tc.wantStatus, body)
			}
		})
	}
}

// TestPostActualSpend_AuthRequired confirms the auth gate applies to the
// finance-side endpoint too — the more dangerous of the two writes, since
// it upserts.
func TestPostActualSpend_AuthRequired(t *testing.T) {
	const token = "s3cret-test-token-9f3a"
	h, _ := newTestHandlerWithToken(t, token)

	// No header → 401.
	code, _ := doRequest(t, h, http.MethodPost, "/api/v1/actual_spend", map[string]any{
		"developer": "alice", "period": "2026-05", "actual_paid_usd": 400.0,
	})
	if code != http.StatusUnauthorized {
		t.Errorf("no header: status = %d, want 401", code)
	}

	// Correct token → 201.
	header := http.Header{"Authorization": []string{"Bearer " + token}}
	code, _ = doRequestWithHeader(t, h, http.MethodPost, "/api/v1/actual_spend",
		map[string]any{"developer": "alice", "period": "2026-05", "actual_paid_usd": 400.0},
		header)
	if code != http.StatusCreated {
		t.Errorf("with token: status = %d, want 201", code)
	}
}

// TestAuth_ScoreGETsRequireToken covers the #59 posture change: when a token
// is configured, the score GETs (per-developer spend + ranking) 401 without
// it; /health and /healthz stay open for probes.
func TestAuth_ScoreGETsRequireToken(t *testing.T) {
	const token = "s3cret-test-token-9f3a"
	h, _ := newTestHandlerWithToken(t, token)

	for _, target := range []string{"/api/v1/scores", "/api/v1/scores/alice"} {
		code, body := doRequest(t, h, http.MethodGet, target, nil)
		if code != http.StatusUnauthorized {
			t.Errorf("GET %s without token: status = %d, want 401; body = %s", target, code, body)
		}
		header := http.Header{"Authorization": []string{"Bearer " + token}}
		code, body = doRequestWithHeader(t, h, http.MethodGet, target, nil, header)
		if code != http.StatusOK {
			t.Errorf("GET %s with token: status = %d, want 200; body = %s", target, code, body)
		}
	}

	// Probe endpoints stay open even with a token configured.
	for _, target := range []string{"/api/v1/health", "/api/v1/healthz", "/api/v1/livez"} {
		code, body := doRequest(t, h, http.MethodGet, target, nil)
		if code != http.StatusOK {
			t.Errorf("GET %s without token: status = %d, want 200; body = %s", target, code, body)
		}
	}
}

// TestAuth_ScoreGETsOpenWithoutToken pins the laptop-mode contract: empty
// token means the GETs serve unauthenticated (the non-loopback bind is
// refused at startup in that mode, so this never faces a network).
func TestAuth_ScoreGETsOpenWithoutToken(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, target := range []string{"/api/v1/scores", "/api/v1/scores/alice", "/api/v1/health"} {
		code, body := doRequest(t, h, http.MethodGet, target, nil)
		if code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200; body = %s", target, code, body)
		}
	}
}

// TestLivez covers the #49 liveness probe: always 200, body carries
// status=alive, the injected version, and a non-negative uptime. It must stay
// 200 regardless of watcher state (the nil-watcher handler here stands in for
// "watcher restarting" — neither should ever fail liveness).
func TestLivez(t *testing.T) {
	h, _ := newTestHandler(t) // version "test", nil watcherState

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/livez", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /livez: status = %d, want 200; body = %s", code, body)
	}
	var resp livezResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode livez body %q: %v", body, err)
	}
	if resp.Status != "alive" {
		t.Errorf("status = %q, want %q", resp.Status, "alive")
	}
	if resp.Version != "test" {
		t.Errorf("version = %q, want %q (the injected build version)", resp.Version, "test")
	}
	// uptime_s is ~0 in a sub-millisecond unit test, so a `>= 0` check would be
	// vacuous (it's always true). Assert the field is actually present in the
	// JSON instead — a missing/renamed field would decode to the zero value and
	// silently pass a value check. omitempty is intentionally NOT set on the
	// field, so 0 must still serialize.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode livez raw body %q: %v", body, err)
	}
	if _, ok := raw["uptime_s"]; !ok {
		t.Errorf("uptime_s field missing from /livez body %q", body)
	}
}

// TestProxyAuth covers the #59 proxy gate: 401 on missing/wrong X-Tier-Token,
// pass-through with the header stripped on match, and transparency when no
// token is configured.
func TestProxyAuth(t *testing.T) {
	const token = "proxy-test-token-77ab"

	var sawHeader string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get(ProxyTokenHeader)
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		name       string
		token      string // gate config; "" disables
		header     string // X-Tier-Token value; "" omits
		wantStatus int
	}{
		{"no token configured -> open", "", "", http.StatusOK},
		{"token configured + no header -> 401", token, "", http.StatusUnauthorized},
		{"token configured + wrong token -> 401", token, "wrong-token-of-equal-len", http.StatusUnauthorized},
		{"token configured + correct token -> 200", token, token, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sawHeader = "unset"
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			if tc.header != "" {
				req.Header.Set(ProxyTokenHeader, tc.header)
			}
			rec := httptest.NewRecorder()
			h, _ := newTestHandlerWithToken(t, tc.token)
			h.ProxyAuth(upstream).ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			// On the authenticated pass-through, the tier token must not
			// leak to the upstream provider.
			if tc.wantStatus == http.StatusOK && tc.token != "" && sawHeader != "" {
				t.Errorf("upstream saw %s = %q, want stripped", ProxyTokenHeader, sawHeader)
			}
		})
	}
}

// newTestHandlerWithScopes configures both a write (admin) token and a
// read-only token on a fresh handler (#190). Either may be "" to leave that
// scope unarmed. A metrics registry is wired so the /metrics read route is
// mounted and exercisable by the scope matrix.
func newTestHandlerWithScopes(t *testing.T, writeToken, readToken string) (*Handler, *store.DB) {
	t.Helper()
	h, db := newTestHandlerWithToken(t, writeToken)
	h.SetReadToken(readToken)
	reg := metrics.NewRegistry()
	reg.NewGauge("tier_build_info", "bi", "version").Set(1, "test")
	h.SetMetricsRegistry(reg)
	return h, db
}

// TestAuth_ReadTokenScopeMatrix is the #190 scope-rejection matrix: with a
// distinct write token and read-only token both armed, the read token is
// accepted on the GET read routes (/scores, /scores/{dev}, /metrics) and
// rejected (403) on every POST/DELETE write route and the admin GETs, while the
// write token continues to work everywhere.
func TestAuth_ReadTokenScopeMatrix(t *testing.T) {
	const writeToken = "write-admin-token-of-len-32-aaaa"
	const readToken = "read-viewer-token-of-len-32-bbbb"

	bearer := func(tok string) http.Header {
		if tok == "" {
			return http.Header{}
		}
		return http.Header{"Authorization": []string{"Bearer " + tok}}
	}

	// Write (mutating) routes and admin GETs: the read token must be rejected
	// with 403 (authenticated, wrong scope); the write token succeeds; no token
	// is 401. wantWrite is the write-token success status per route.
	writeRoutes := []struct {
		method    string
		target    string
		body      any
		wantWrite int
	}{
		{http.MethodPost, "/api/v1/costs", validCostPayload(), http.StatusCreated},
		{http.MethodPost, "/api/v1/actual_spend", map[string]any{"developer": "alice", "period": "2026-05", "actual_paid_usd": 400.0}, http.StatusCreated},
		{http.MethodPost, "/api/v1/org_actual_spend", map[string]any{"org": "acme", "period": "2026-05", "actual_paid_usd": 2000.0}, http.StatusCreated},
		{http.MethodPost, "/api/v1/developer_alias", map[string]any{"alias": "al", "canonical": "alice"}, http.StatusCreated},
		{http.MethodDelete, "/api/v1/developer_alias/al", nil, http.StatusNotFound}, // route reached, alias absent → 404 (not an auth code)
		// GDPR endpoints (#184): write-scoped, so the read token is 403 here even
		// though the score GETs accept it. Route reached with no data → 404.
		{http.MethodDelete, "/api/v1/developer/nobody", nil, http.StatusNotFound},
		{http.MethodGet, "/api/v1/developer/nobody/export", nil, http.StatusNotFound},
		// Admin/finance GETs are NOT granted to the viewer scope (#190 grants
		// ONLY /scores, /scores/{dev}, /metrics + dashboard data).
		{http.MethodGet, "/api/v1/org_actual_spend", nil, http.StatusOK},
		{http.MethodGet, "/api/v1/developer_alias", nil, http.StatusOK},
	}
	for _, rt := range writeRoutes {
		t.Run("write-route "+rt.method+" "+rt.target, func(t *testing.T) {
			h, _ := newTestHandlerWithScopes(t, writeToken, readToken)
			// read token → 403 (authenticated but not authorized for this scope)
			if code, body := doRequestWithHeader(t, h, rt.method, rt.target, rt.body, bearer(readToken)); code != http.StatusForbidden {
				t.Errorf("read token on %s %s: status = %d, want 403; body = %s", rt.method, rt.target, code, body)
			}
			// no token → 401
			if code, _ := doRequestWithHeader(t, h, rt.method, rt.target, rt.body, bearer("")); code != http.StatusUnauthorized {
				t.Errorf("no token on %s %s: status = %d, want 401", rt.method, rt.target, code)
			}
			// write token → route's normal success
			if code, body := doRequestWithHeader(t, h, rt.method, rt.target, rt.body, bearer(writeToken)); code != rt.wantWrite {
				t.Errorf("write token on %s %s: status = %d, want %d; body = %s", rt.method, rt.target, code, rt.wantWrite, body)
			}
		})
	}

	// Read routes: read token AND write token both accepted; no token 401.
	readRoutes := []string{"/api/v1/scores", "/api/v1/scores/alice", "/metrics"}
	for _, target := range readRoutes {
		t.Run("read-route GET "+target, func(t *testing.T) {
			h, _ := newTestHandlerWithScopes(t, writeToken, readToken)
			if code, body := doRequestWithHeader(t, h, http.MethodGet, target, nil, bearer(readToken)); code != http.StatusOK {
				t.Errorf("read token on GET %s: status = %d, want 200; body = %s", target, code, body)
			}
			if code, body := doRequestWithHeader(t, h, http.MethodGet, target, nil, bearer(writeToken)); code != http.StatusOK {
				t.Errorf("write token on GET %s: status = %d, want 200; body = %s", target, code, body)
			}
			if code, _ := doRequestWithHeader(t, h, http.MethodGet, target, nil, bearer("")); code != http.StatusUnauthorized {
				t.Errorf("no token on GET %s: status = %d, want 401", target, code)
			}
		})
	}
}

// TestAuth_ReadTokenRejectedOnProxy confirms the read-only token cannot open the
// upstream provider relay (#190): the proxy carries real Anthropic/OpenAI
// credentials, so it requires the write scope. A read token → 403; write → pass.
func TestAuth_ReadTokenRejectedOnProxy(t *testing.T) {
	const writeToken = "write-admin-token-of-len-32-aaaa"
	const readToken = "read-viewer-token-of-len-32-bbbb"
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h, _ := newTestHandlerWithScopes(t, writeToken, readToken)

	do := func(tok string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set(ProxyTokenHeader, tok)
		rec := httptest.NewRecorder()
		h.ProxyAuth(upstream).ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do(readToken); code != http.StatusForbidden {
		t.Errorf("read token on proxy: status = %d, want 403", code)
	}
	if code := do(writeToken); code != http.StatusOK {
		t.Errorf("write token on proxy: status = %d, want 200", code)
	}
}

// TestAuth_ReadTokenParityWithWriteToken pins the parity contract (#190): with
// only a read token armed and no write token, an unauthenticated request to a
// read route is still 401 (the read scope gates reads), and the read token
// unlocks it — i.e. the read token behaves as a first-class credential.
func TestAuth_ReadTokenParityWithWriteToken(t *testing.T) {
	const writeToken = "write-admin-token-of-len-32-aaaa"
	const readToken = "read-viewer-token-of-len-32-bbbb"
	h, _ := newTestHandlerWithScopes(t, writeToken, readToken)

	// A read token that is wrong-but-length-matched is rejected on a read route.
	wrong := http.Header{"Authorization": []string{"Bearer read-viewer-token-of-len-32-XXXX"}}
	if code, _ := doRequestWithHeader(t, h, http.MethodGet, "/api/v1/scores", nil, wrong); code != http.StatusUnauthorized {
		t.Errorf("wrong token on GET /scores: status = %d, want 401", code)
	}
}

// TestPostCosts_IdempotencyKey_DedupsRepeats covers the #21 happy path: two
// identical POSTs with the same IdempotencyKey produce exactly one row in
// SQLite (the partial unique index absorbs the second), and the second POST
// still returns 201 — the endpoint is idempotent, not error-prone.
func TestPostCosts_IdempotencyKey_DedupsRepeats(t *testing.T) {
	h, db := newTestHandler(t)
	payload := map[string]any{
		"developer":       "alice",
		"issue_id":        "issue-42",
		"model":           "claude-sonnet-4",
		"cost_usd":        0.0105,
		"input_tokens":    1000,
		"output_tokens":   500,
		"idempotency_key": "client-stable-key-001",
	}

	for i := 0; i < 2; i++ {
		code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload)
		if code != http.StatusCreated {
			t.Fatalf("POST #%d: status = %d, want 201; body = %s", i, code, body)
		}
	}

	// Verify exactly one row landed in token_events.
	costs, err := db.DeveloperCosts(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	if len(costs) != 1 {
		t.Fatalf("expected 1 developer cost row, got %d", len(costs))
	}
	if costs[0].TotalCostMicro != 10_500 {
		t.Errorf("total cost = %v, want 10_500 micro ($0.0105, dedup must NOT sum the two posts)", costs[0].TotalCostMicro)
	}
}

// TestPostCosts_LegacyCacheWriteEmitsWarningHeader exercises the issue #55
// API back-compat path: clients still sending cache_write_tokens (deprecated)
// get a Warning: 299 header AND their value lands in cache_write_5m (the
// pre-1h-feature default per the pricing decision matrix).
func TestPostCosts_LegacyCacheWriteEmitsWarningHeader(t *testing.T) {
	h, db := newTestHandler(t)
	// cost_usd is set to a number that, if the routing transformation worked,
	// the row's stored value equals; DeveloperCosts then confirms persistence.
	// This is a smoke test for the handler→ingester→store chain on the
	// legacy code path. End-to-end bucket routing (legacy → cache_write_5m,
	// not 1h) is structurally guaranteed by handler.go's one-line
	// `req.CacheWrite5m = req.CacheWrite` and verified at the SQL level by
	// TestOpen_MigratesCacheWriteSplit in the store package; an additional
	// per-bucket assertion here would require exposing *store.DB's SQL
	// handle for read-back, which the package intentionally encapsulates.
	const wantCost = 0.5
	payload := map[string]any{
		"developer":          "alice",
		"issue_id":           "issue-55",
		"model":              "claude-opus-4-7",
		"input_tokens":       0,
		"output_tokens":      0,
		"cache_write_tokens": 500, // legacy field
		"cost_usd":           wantCost,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/costs", bytes.NewReader(buf))
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	warns := rec.Header().Values("Warning")
	var got string
	for _, w := range warns {
		if strings.Contains(w, "cache_write_tokens is deprecated") {
			got = w
			break
		}
	}
	if got == "" {
		t.Fatalf("no 299 deprecation Warning header found in %v", warns)
	}
	// Verify the RFC 7234 §5.5 grammar: warn-code SP warn-agent SP warn-text.
	if !strings.HasPrefix(got, "299 tierd \"") {
		t.Errorf("Warning header malformed (expected `299 tierd \"...\"` shape): %q", got)
	}

	// End-to-end persistence smoke test: the POST must have created exactly
	// one row reachable via the public DeveloperCosts API.
	costs, err := db.DeveloperCosts(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	if len(costs) != 1 || costs[0].Developer != "alice" {
		t.Fatalf("expected 1 row for alice, got %+v", costs)
	}
	if costs[0].TotalCostMicro != store.DollarsToMicro(wantCost) {
		t.Errorf("TotalCostMicro = %v, want %v (client-supplied cost must persist through the legacy path)", costs[0].TotalCostMicro, store.DollarsToMicro(wantCost))
	}
}

// TestPostCosts_AmbiguousCacheWriteRejects400 confirms a payload that mixes
// the legacy cache_write_tokens with either new TTL-split field is rejected
// up-front rather than silently double-counting or routing into the wrong
// bucket. Issue #55.
func TestPostCosts_AmbiguousCacheWriteRejects400(t *testing.T) {
	h, _ := newTestHandler(t)
	payload := map[string]any{
		"developer":             "alice",
		"issue_id":              "issue-55",
		"model":                 "claude-opus-4-7",
		"input_tokens":          0,
		"output_tokens":         0,
		"cache_write_tokens":    100, // legacy
		"cache_write_5m_tokens": 50,  // new — combining is ambiguous
		"cost_usd":              0.00,
	}
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", code, body)
	}
	if !strings.Contains(string(body), "cache_write_tokens cannot be combined") {
		t.Errorf("body = %s, want mention of ambiguous combination", body)
	}
}

// TestPostCosts_SourceAllowlist verifies #34: the manual REST endpoint may
// only attribute rows to source "api" (or omit it, which defaults to "api").
// Accepting client-supplied "jsonl"/"proxy" would let a caller fabricate rows
// that masquerade as automated capture and inflate the source-keyed Coverage %
// and Spend Leverage aggregates (#17).
func TestPostCosts_SourceAllowlist(t *testing.T) {
	cases := []struct {
		name     string
		source   any // omitted when nil
		wantCode int
	}{
		{"omitted defaults to api", nil, http.StatusCreated},
		{"explicit api allowed", "api", http.StatusCreated},
		{"empty string allowed", "", http.StatusCreated},
		{"jsonl rejected", "jsonl", http.StatusBadRequest},
		{"proxy rejected", "proxy", http.StatusBadRequest},
		{"arbitrary rejected", "totally-made-up", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHandler(t)
			payload := map[string]any{
				"developer":     "alice",
				"issue_id":      "issue-34",
				"model":         "claude-sonnet-4",
				"cost_usd":      0.01,
				"input_tokens":  100,
				"output_tokens": 50,
			}
			if tc.source != nil {
				payload["source"] = tc.source
			}
			code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload)
			if code != tc.wantCode {
				t.Fatalf("source=%v: status = %d, want %d; body = %s", tc.source, code, tc.wantCode, body)
			}
			if tc.wantCode == http.StatusBadRequest && !bytes.Contains(body, []byte(`source must be \"api\" or omitted`)) {
				t.Errorf("source=%v: 400 body should explain the allow-list; got %s", tc.source, body)
			}
		})
	}
}

// TestPostCosts_FidelityPolicy verifies #82: the manual REST endpoint may not
// attribute rows to "realtime" fidelity (reserved for the collector/proxy's
// per-request capture, and the key for the Coverage % / Spend Leverage
// metrics). An omitted fidelity defaults to "estimated"; "daily"/"estimated"
// are accepted; "realtime" and any other value are rejected with 400.
func TestPostCosts_FidelityPolicy(t *testing.T) {
	cases := []struct {
		name     string
		fidelity any // omitted when nil
		wantCode int
		wantMsg  string // substring expected in a 400 body
	}{
		{"omitted defaults to estimated", nil, http.StatusCreated, ""},
		{"daily allowed", "daily", http.StatusCreated, ""},
		{"estimated allowed", "estimated", http.StatusCreated, ""},
		{"empty string defaults", "", http.StatusCreated, ""},
		{"realtime rejected", "realtime", http.StatusBadRequest, "reserved for automated capture"},
		{"arbitrary rejected", "hourly", http.StatusBadRequest, "fidelity must be"},
		// Exact-match contract (mirrors the source switch): near-miss realtime
		// variants are still rejected, via the enum branch. Pinned so a future
		// normalize-then-match change can't silently re-route them.
		{"capitalized realtime rejected", "Realtime", http.StatusBadRequest, "fidelity must be"},
		{"whitespace realtime rejected", " realtime", http.StatusBadRequest, "fidelity must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHandler(t)
			payload := map[string]any{
				"developer":     "alice",
				"issue_id":      "issue-82",
				"model":         "claude-sonnet-4",
				"cost_usd":      0.01,
				"input_tokens":  100,
				"output_tokens": 50,
			}
			if tc.fidelity != nil {
				payload["fidelity"] = tc.fidelity
			}
			code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload)
			if code != tc.wantCode {
				t.Fatalf("fidelity=%v: status = %d, want %d; body = %s", tc.fidelity, code, tc.wantCode, body)
			}
			if tc.wantMsg != "" && !bytes.Contains(body, []byte(tc.wantMsg)) {
				t.Errorf("fidelity=%v: 400 body = %s, want substring %q", tc.fidelity, body, tc.wantMsg)
			}
		})
	}
}

// TestPostCosts_NoKey_DuplicatesAllowed covers the back-compat path: when no
// IdempotencyKey is supplied, the row stores NULL via NULLIF and the partial
// unique index doesn't enforce uniqueness over NULLs — so duplicate POSTs
// land as separate rows. Matches the pre-#21 behavior.
func TestPostCosts_NoKey_DuplicatesAllowed(t *testing.T) {
	h, db := newTestHandler(t)
	payload := map[string]any{
		"developer":     "alice",
		"issue_id":      "issue-42",
		"model":         "claude-sonnet-4",
		"cost_usd":      0.0105,
		"input_tokens":  1000,
		"output_tokens": 500,
		// no idempotency_key
	}
	for i := 0; i < 2; i++ {
		code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload)
		if code != http.StatusCreated {
			t.Fatalf("POST #%d: status = %d, want 201; body = %s", i, code, body)
		}
	}

	costs, _ := db.DeveloperCosts(context.Background(), time.Now().Add(-time.Hour))
	if len(costs) != 1 {
		t.Fatalf("expected 1 aggregated developer row, got %d", len(costs))
	}
	if costs[0].TotalCostMicro != 21_000 {
		t.Errorf("total cost = %v, want 21_000 micro ($0.0210, unkeyed posts must NOT dedup)", costs[0].TotalCostMicro)
	}
}

// TestPostCosts_DifferentKeys_NoDedup covers the explicit-non-dedup path:
// two POSTs with DIFFERENT IdempotencyKeys land as two rows even when the
// rest of the payload matches. This proves the dedup is keyed, not
// content-addressed.
func TestPostCosts_DifferentKeys_NoDedup(t *testing.T) {
	h, db := newTestHandler(t)
	base := map[string]any{
		"developer":     "alice",
		"issue_id":      "issue-42",
		"model":         "claude-sonnet-4",
		"cost_usd":      0.0105,
		"input_tokens":  1000,
		"output_tokens": 500,
	}
	for _, key := range []string{"key-A", "key-B"} {
		p := map[string]any{}
		for k, v := range base {
			p[k] = v
		}
		p["idempotency_key"] = key
		code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", p)
		if code != http.StatusCreated {
			t.Fatalf("POST with key %q: status = %d, want 201; body = %s", key, code, body)
		}
	}

	costs, _ := db.DeveloperCosts(context.Background(), time.Now().Add(-time.Hour))
	if len(costs) != 1 {
		t.Fatalf("expected 1 developer in aggregate, got %d", len(costs))
	}
	if costs[0].TotalCostMicro != 21_000 {
		t.Errorf("total cost = %v, want 21_000 micro ($0.0210, different keys must NOT dedup)", costs[0].TotalCostMicro)
	}
}

// TestPostCosts_DisallowsUnknownFields catches typo'd field names that would
// otherwise be silently dropped. Matches the contract on /actual_spend. The
// assertion checks that the error body explicitly names the offending field,
// so a regression that loosens DisallowUnknownFields would fail the test
// even if some other unknown key were silently accepted.
func TestPostCosts_DisallowsUnknownFields(t *testing.T) {
	h, _ := newTestHandler(t)
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", map[string]any{
		"developer":      "alice",
		"issue_id":       "issue-42",
		"model":          "claude-sonnet-4",
		"IdempotencyKey": "key-1", // wrong case — should be rejected
		"input_tokens":   100,
		"output_tokens":  50,
	})
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", code, body)
	}
	if !bytes.Contains(body, []byte("IdempotencyKey")) {
		t.Errorf("400 body should name the offending field; got %s", body)
	}
}

// TestPostCosts_SameKeyDivergentCost_Returns409 pins the #295 (ruling A) cost
// semantics on top of #233's immutability. When the same IdempotencyKey is reused
// with a DIFFERENT cost_usd, the write now FAILS LOUD with 409 instead of the old
// silent first-writer-wins 201 — a finance correction is never silently dropped.
// cost_micro stays IMMUTABLE (#233) at the first writer's value: a 409 rejects, it
// does not overwrite. cost_micro is shared by /costs and /events through
// insertTokenEventSQL, but only the manual /costs surface uses the divergence
// guard (InsertManualCostEvent); the automated /events path keeps replaying
// cost-identical per-message rows through InsertTokenEvent. This test replaces the
// pre-#295 first-writer-wins-201 contract; a future refactor must change it
// deliberately (ruling C — a sanctioned audited override — is a separate follow-up).
func TestPostCosts_SameKeyDivergentCost_Returns409(t *testing.T) {
	h, db := newTestHandler(t)
	post := func(cost float64) (int, []byte) {
		return doRequest(t, h, http.MethodPost, "/api/v1/costs", map[string]any{
			"developer":       "alice",
			"issue_id":        "issue-42",
			"model":           "claude-sonnet-4",
			"cost_usd":        cost,
			"input_tokens":    100,
			"output_tokens":   50,
			"idempotency_key": "shared-key",
		})
	}

	code, body := post(0.0100)
	if code != http.StatusCreated {
		t.Fatalf("first POST: status = %d, want 201; body = %s", code, body)
	}
	code, body = post(0.0500)
	if code != http.StatusConflict {
		t.Fatalf("divergent second POST: status = %d, want 409; body = %s", code, body)
	}
	// The 409 carries a JSON {"error": ...} body naming the correction path — a
	// client parses this to know its post was rejected. Guard the shape so a
	// swap to an empty body (or a non-JSON string) is a test failure.
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("409 body is not a JSON object: %v (body = %s)", err, body)
	}
	if !strings.Contains(errResp["error"], "idempotency_key") {
		t.Errorf("409 error message should name idempotency_key and the correction path; got %q", errResp["error"])
	}

	costs, _ := db.DeveloperCosts(context.Background(), time.Now().Add(-time.Hour))
	if len(costs) != 1 {
		t.Fatalf("expected 1 row (409 must not add a row), got %d", len(costs))
	}
	if costs[0].TotalCostMicro != 10_000 {
		t.Errorf("cost = %v, want 10_000 micro ($0.0100, first-writer-wins; a 409 must not re-price the immutable row per #233)",
			costs[0].TotalCostMicro)
	}
}

// TestPostCosts_RejectsNegatives covers the new >= 0 validation: a client
// posting negative tokens or negative cost_usd is rejected at 400.
func TestPostCosts_RejectsNegatives(t *testing.T) {
	h, _ := newTestHandler(t)
	cases := []struct {
		name    string
		payload map[string]any
	}{
		{"negative input_tokens", map[string]any{
			"developer": "alice", "issue_id": "issue-42", "model": "claude-sonnet-4",
			"input_tokens": -1, "output_tokens": 50, "cost_usd": 0.01,
		}},
		{"negative output_tokens", map[string]any{
			"developer": "alice", "issue_id": "issue-42", "model": "claude-sonnet-4",
			"input_tokens": 100, "output_tokens": -1, "cost_usd": 0.01,
		}},
		{"negative cache_read_tokens", map[string]any{
			"developer": "alice", "issue_id": "issue-42", "model": "claude-sonnet-4",
			"input_tokens": 100, "output_tokens": 50, "cache_read_tokens": -1, "cost_usd": 0.01,
		}},
		{"negative cost_usd", map[string]any{
			"developer": "alice", "issue_id": "issue-42", "model": "claude-sonnet-4",
			"input_tokens": 100, "output_tokens": 50, "cost_usd": -0.01,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", tc.payload)
			if code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body = %s", code, body)
			}
		})
	}
}

// TestPostCosts_RejectsNonFiniteCost guards against NaN/Inf leaking into the
// store, which would break json.Marshal on the next /scores call.
func TestPostCosts_RejectsNonFiniteCost(t *testing.T) {
	h, _ := newTestHandler(t)
	// JSON encoders typically reject NaN/Inf in numbers, so send as raw text
	// to exercise the server-side guard rather than the encoder's.
	cases := []string{
		`{"developer":"alice","issue_id":"issue-42","model":"claude-sonnet-4","cost_usd":1e400,"input_tokens":1,"output_tokens":1}`,
		`{"developer":"alice","issue_id":"issue-42","model":"claude-sonnet-4","cost_usd":NaN,"input_tokens":1,"output_tokens":1}`,
	}
	for _, body := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/costs",
			bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		mux := http.NewServeMux()
		h.Register(mux)
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
}

// TestPostCosts_IdempotencyKeyTooLongRejected pins the #144 trust-boundary cap:
// a client-supplied idempotency_key over maxIdentifierLen is rejected with a
// field-named 400 (so a ~1 MiB key can't be persisted into the partial unique
// index), while a key at the cap and a collector-shaped 64-hex key are accepted.
// The 257-char case FAILS on main (uncapped → 201).
func TestPostCosts_IdempotencyKeyTooLongRejected(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		want    int
		wantMsg string // exact error body when want == 400
	}{
		{"over cap by one", strings.Repeat("x", maxIdentifierLen+1), http.StatusBadRequest, "idempotency_key must be <= 256 chars"},
		{"exactly at cap", strings.Repeat("x", maxIdentifierLen), http.StatusCreated, ""},
		{"collector 64-hex key", strings.Repeat("a", 64), http.StatusCreated, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Fresh handler per case so an accepted key doesn't dedup a later one.
			h, _ := newTestHandler(t)
			payload := validCostPayload()
			payload["idempotency_key"] = tc.key
			code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload)
			if code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", code, tc.want, body)
			}
			if tc.want == http.StatusBadRequest {
				var got map[string]string
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("unmarshal error body %q: %v", body, err)
				}
				if got["error"] != tc.wantMsg {
					t.Errorf("error = %q, want %q", got["error"], tc.wantMsg)
				}
			}
		})
	}
}

// TestPostOrgActualSpend_Success covers the #23 happy path: an org-level
// invoice posts cleanly and the resulting allocation is visible via the
// store's ActualSpendForDeveloper. The endpoint exists to support
// enterprise contracts that bill one org for N seats.
func TestPostOrgActualSpend_Success(t *testing.T) {
	h, db := newTestHandler(t)
	// Seed two developers in the org so we can read back the allocation.
	for _, dev := range []string{"alice", "bob"} {
		if err := db.UpsertHierarchy(context.Background(), dev, "platform", "", "acme"); err != nil {
			t.Fatalf("UpsertHierarchy: %v", err)
		}
	}
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/org_actual_spend", map[string]any{
		"org":             "acme",
		"period":          "2026-05",
		"actual_paid_usd": 2000.0,
	})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", code, body)
	}
	got, err := db.ActualSpendForDeveloper(context.Background(),
		"alice", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ActualSpendForDeveloper: %v", err)
	}
	if got != 1000 {
		t.Errorf("alice slice = %v, want 1000 ($2000 / 2 seats)", got)
	}
}

// TestPostOrgActualSpend_ValidationErrors exercises the same validation
// surface as /actual_spend, minus the developer field and with org swapped
// in. We rely on shared helpers (validatePeriod, MaxBytesReader, etc.) so
// the table here is intentionally a sampler — broad coverage already lives
// in TestPostActualSpend_ValidationErrors.
func TestPostOrgActualSpend_ValidationErrors(t *testing.T) {
	h, _ := newTestHandler(t)
	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing org", map[string]any{"period": "2026-05", "actual_paid_usd": 100.0}},
		{"missing period", map[string]any{"org": "acme", "actual_paid_usd": 100.0}},
		{"malformed period", map[string]any{"org": "acme", "period": "2026-13", "actual_paid_usd": 100.0}},
		// Negative amounts ARE allowed under #24 (credit memos / refunds).
		// The rejection tests for negative values were removed at that point.
		{"unknown field", map[string]any{"org": "acme", "period": "2026-05", "actual_paid_usd": 100.0, "extra": "junk"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doRequest(t, h, http.MethodPost, "/api/v1/org_actual_spend", tc.body)
			if code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body = %s", code, body)
			}
		})
	}
}

// TestPostOrgActualSpend_AuthRequired confirms the auth gate from #22 applies
// to the new endpoint too. Same Bearer-token contract as /actual_spend and
// /costs.
func TestPostOrgActualSpend_AuthRequired(t *testing.T) {
	const token = "s3cret-test-token-9f3a"
	h, _ := newTestHandlerWithToken(t, token)

	// No header → 401.
	code, _ := doRequest(t, h, http.MethodPost, "/api/v1/org_actual_spend", map[string]any{
		"org": "acme", "period": "2026-05", "actual_paid_usd": 2000.0,
	})
	if code != http.StatusUnauthorized {
		t.Errorf("no header: status = %d, want 401", code)
	}

	// Correct token → 201.
	header := http.Header{"Authorization": []string{"Bearer " + token}}
	code, _ = doRequestWithHeader(t, h, http.MethodPost, "/api/v1/org_actual_spend",
		map[string]any{"org": "acme", "period": "2026-05", "actual_paid_usd": 2000.0},
		header)
	if code != http.StatusCreated {
		t.Errorf("with token: status = %d, want 201", code)
	}
}

// TestGetScores_OrgAllocationFlowsThrough is the end-to-end #23/#39 test: a
// seeded org-level invoice + org_hierarchy + token_events for one developer.
// alice (with usage) gets her per-seat allocation; the other 3 seats are
// surfaced as zero-cost rows (#39) so the TOTAL ActualPaidUSD reflects all 4
// seats — otherwise team Spend Leverage would inflate by seats/active.
// TestGetScores_StampsPriceTableVersion proves every /scores response carries the
// top-level price_table provenance (#233) — the version + effective_date of the
// table that produced its cost figures, so a CFO can tell a real trend from a
// table-bump artifact and an org can verify it priced against a known table.
func TestGetScores_StampsPriceTableVersion(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 1000.0)

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores: status = %d, body = %s", code, body)
	}
	var resp struct {
		PriceTable struct {
			Version       int    `json:"version"`
			EffectiveDate string `json:"effective_date"`
		} `json:"price_table"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	info := store.ActivePriceTableInfo()
	if resp.PriceTable.Version != info.Version {
		t.Errorf("price_table.version = %d, want active %d", resp.PriceTable.Version, info.Version)
	}
	if resp.PriceTable.EffectiveDate != info.EffectiveDate {
		t.Errorf("price_table.effective_date = %q, want %q", resp.PriceTable.EffectiveDate, info.EffectiveDate)
	}
}

func TestGetScores_OrgAllocationFlowsThrough(t *testing.T) {
	h, db := newTestHandler(t)
	// 4 developers in org "acme", but only alice has token events this period.
	for _, dev := range []string{"alice", "bob", "carol", "dave"} {
		if err := db.UpsertHierarchy(context.Background(), dev, "platform", "", "acme"); err != nil {
			t.Fatalf("UpsertHierarchy: %v", err)
		}
	}
	seedCosts(t, db, "alice", "issue-1", 1000.0)

	// Org paid $4000 → per-seat allocation = $1000 each (4 active seats).
	// Period is pinned to current month explicitly (rather than computed at
	// test runtime); a month-boundary tick mid-test would otherwise have the
	// POST write one period and the subsequent GET read /scores with a since
	// in the next month — flaky on 23:59:59 UTC.
	period := time.Now().UTC().Format("2006-01")
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/org_actual_spend", map[string]any{
		"org":             "acme",
		"period":          period,
		"actual_paid_usd": 4000.0,
	})
	if code != http.StatusCreated {
		t.Fatalf("seed org spend: status = %d, body = %s", code, body)
	}

	code, body = doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores: status = %d, body = %s", code, body)
	}
	var resp struct {
		Developers []struct {
			Developer     string  `json:"developer"`
			TotalCostUSD  float64 `json:"total_cost_usd"`
			ActualPaidUSD float64 `json:"actual_paid_usd"`
			SpendLeverage float64 `json:"spend_leverage"`
		} `json:"developers"`
		Total struct {
			TotalCostUSD  float64 `json:"total_cost_usd"`
			ActualPaidUSD float64 `json:"actual_paid_usd"`
			SpendLeverage float64 `json:"spend_leverage"`
		} `json:"total"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// #39: all 4 seats surfaced (alice with usage + 3 zero-cost members).
	if len(resp.Developers) != 4 {
		t.Fatalf("got %d devs, want 4 (alice + 3 zero-cost active seats)", len(resp.Developers))
	}
	byDev := map[string]struct {
		cost, paid, lev float64
	}{}
	for _, d := range resp.Developers {
		byDev[d.Developer] = struct{ cost, paid, lev float64 }{d.TotalCostUSD, d.ActualPaidUSD, d.SpendLeverage}
	}
	if a := byDev["alice"]; a.paid != 1000.0 || a.lev != 1.0 || a.cost != 1000.0 {
		t.Errorf("alice = %+v, want cost=1000 paid=1000 leverage=1.0", a)
	}
	for _, dev := range []string{"bob", "carol", "dave"} {
		z := byDev[dev]
		if z.cost != 0 || z.paid != 1000.0 || z.lev != 0 {
			t.Errorf("%s = %+v, want cost=0 paid=1000 leverage=0 (zero-cost seat)", dev, z)
		}
	}
	// TOTAL reflects all 4 seats: list $1000 / paid $4000 = 0.25 (not 1.0,
	// which is the pre-#39 inflated value from counting only alice's seat).
	if resp.Total.ActualPaidUSD != 4000.0 {
		t.Errorf("total actual_paid_usd = %v, want 4000 (all 4 seats)", resp.Total.ActualPaidUSD)
	}
	if resp.Total.SpendLeverage != 0.25 {
		t.Errorf("total spend_leverage = %v, want 0.25 (list 1000 / paid 4000)", resp.Total.SpendLeverage)
	}
}

func TestPostActualSpend_Success(t *testing.T) {
	h, db := newTestHandler(t)
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/actual_spend", map[string]any{
		"developer":       "alice",
		"period":          "2026-05",
		"actual_paid_usd": 400.0,
	})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", code, body)
	}

	// Verify it round-trips: ActualSpendForDeveloper should now return 400.
	got, err := db.ActualSpendForDeveloper(context.Background(),
		"alice", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ActualSpendForDeveloper: %v", err)
	}
	if got != 400.0 {
		t.Errorf("round-trip total = %v, want 400.0", got)
	}
}

func TestPostActualSpend_ValidationErrors(t *testing.T) {
	h, _ := newTestHandler(t)
	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing developer", map[string]any{"period": "2026-05", "actual_paid_usd": 100.0}},
		{"missing period", map[string]any{"developer": "alice", "actual_paid_usd": 100.0}},
		{"malformed period — wrong format", map[string]any{"developer": "alice", "period": "2026/05", "actual_paid_usd": 100.0}},
		{"malformed period — bad month", map[string]any{"developer": "alice", "period": "2026-13", "actual_paid_usd": 100.0}},
		{"malformed period — extra characters", map[string]any{"developer": "alice", "period": "2026-05-01", "actual_paid_usd": 100.0}},
		// Negative actual_paid_usd is allowed under #24 (credit memos / refunds).
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doRequest(t, h, http.MethodPost, "/api/v1/actual_spend", tc.body)
			if code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body = %s", code, body)
			}
		})
	}
}

// TestPostActualSpend_RejectsNonFinite covers the math.IsNaN / math.IsInf
// gate. Without it, a client encoding +Inf (e.g. via a JSON encoder that
// allows non-finite floats — most do not, but custom payloads can) would land
// in SQLite as Inf, then break json.Marshal on the next /scores call.
func TestPostActualSpend_RejectsNonFinite(t *testing.T) {
	h, _ := newTestHandler(t)
	cases := []string{
		// json.Decoder rejects bare Inf / NaN per spec, so we have to send
		// them as raw JSON text. The decoder will fail with "invalid character"
		// before our finite-check runs — which is fine: either path is a 400.
		`{"developer":"alice","period":"2026-05","actual_paid_usd":1e400}`,
		`{"developer":"alice","period":"2026-05","actual_paid_usd":NaN}`,
		`{"developer":"alice","period":"2026-05","actual_paid_usd":Infinity}`,
	}
	for _, body := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/actual_spend",
			bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		mux := http.NewServeMux()
		h.Register(mux)
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400; body = %s", body, rec.Code, rec.Body)
		}
	}
}

// TestPostActualSpend_YearRange covers the year sanity bounds. The previous
// regex-only implementation accepted 0000-05 and 9999-12, both of which would
// corrupt the lexicographic ordering invariant on which ActualSpendForDeveloper
// depends.
func TestPostActualSpend_YearRange(t *testing.T) {
	h, _ := newTestHandler(t)
	bad := []string{"0000-05", "1999-12", "2051-01", "9999-12"}
	for _, period := range bad {
		t.Run(period, func(t *testing.T) {
			code, body := doRequest(t, h, http.MethodPost, "/api/v1/actual_spend", map[string]any{
				"developer": "alice", "period": period, "actual_paid_usd": 100.0,
			})
			if code != http.StatusBadRequest {
				t.Errorf("period %q: status = %d, want 400; body = %s", period, code, body)
			}
		})
	}
}

// TestPostActualSpend_AccumulatesViaHTTP exercises the full accumulation
// path through the REST layer (#24): finance posts an invoice + a credit
// memo, both via HTTP, and the net is the sum. Replaces the pre-#24 upsert
// test.
func TestPostActualSpend_AccumulatesViaHTTP(t *testing.T) {
	h, db := newTestHandler(t)
	post := func(amount float64) {
		t.Helper()
		code, body := doRequest(t, h, http.MethodPost, "/api/v1/actual_spend", map[string]any{
			"developer": "alice", "period": "2026-05", "actual_paid_usd": amount,
		})
		if code != http.StatusCreated {
			t.Fatalf("POST %v: status = %d; body = %s", amount, code, body)
		}
	}
	post(500.0)  // invoice
	post(-100.0) // credit memo
	got, err := db.ActualSpendForDeveloper(context.Background(),
		"alice", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ActualSpendForDeveloper: %v", err)
	}
	if got != 400.0 {
		t.Errorf("net = %v, want 400 ($500 invoice + $-100 credit memo)", got)
	}
}

// TestPostActualSpend_AcceptsNegative confirms the >= 0 guard is gone:
// finance can post a credit memo as a single negative-amount row.
func TestPostActualSpend_AcceptsNegative(t *testing.T) {
	h, _ := newTestHandler(t)
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/actual_spend", map[string]any{
		"developer": "alice", "period": "2026-05", "actual_paid_usd": -50.0,
	})
	if code != http.StatusCreated {
		t.Errorf("status = %d, want 201; body = %s", code, body)
	}
}

// TestPostActualSpend_DisallowsUnknownFields catches typos like "actualPaidUSD"
// (camelCase) that would otherwise be silently dropped, leaving the inserted
// row at $0. The misnamed field is a real foot-gun for finance integrations.
func TestPostActualSpend_DisallowsUnknownFields(t *testing.T) {
	h, _ := newTestHandler(t)
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/actual_spend", map[string]any{
		"developer":     "alice",
		"period":        "2026-05",
		"actualPaidUSD": 400.0, // wrong case — should be rejected, not silently $0
	})
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", code, body)
	}
}

func TestPostActualSpend_InvalidJSON(t *testing.T) {
	h, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/actual_spend",
		bytes.NewReader([]byte("{not json")))
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestGetScores_TotalPresentByDefault covers the #25 happy path: /scores
// always returns a "total" block computed server-side via
// scoring.RollupTeam, independent of any ?team= filter. The dashboard reads
// it directly instead of recomputing client-side.
func TestGetScores_TotalPresentByDefault(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 100.0)
	seedCosts(t, db, "bob", "issue-2", 200.0)

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", code, body)
	}
	var resp struct {
		Total *struct {
			TotalCostUSD    float64 `json:"total_cost_usd"`
			ActualPaidUSD   float64 `json:"actual_paid_usd"`
			SpendLeverage   float64 `json:"spend_leverage"`
			CoveragePercent float64 `json:"coverage_pct"`
		} `json:"total"`
		Developers []struct {
			TotalCostUSD float64 `json:"total_cost_usd"`
		} `json:"developers"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Total == nil {
		t.Fatalf("total block missing from /scores response: %s", body)
	}
	// Absolute checks (not just internal consistency). $100 + $200 = $300;
	// seedCosts emits realtime-fidelity rows so CoveragePercent must be 100.
	// SpendLeverage stays 0 because we haven't seeded actual_spend.
	if resp.Total.TotalCostUSD != 300.0 {
		t.Errorf("total.total_cost_usd = %v, want 300.0 ($100 alice + $200 bob)",
			resp.Total.TotalCostUSD)
	}
	if resp.Total.CoveragePercent != 100.0 {
		t.Errorf("total.coverage_pct = %v, want 100.0 (seedCosts uses realtime fidelity)",
			resp.Total.CoveragePercent)
	}
	if resp.Total.SpendLeverage != 0 {
		t.Errorf("total.spend_leverage = %v, want 0 (no actual_spend seeded)",
			resp.Total.SpendLeverage)
	}
	// Sum of dev rows must equal total — the invariant the dashboard relies
	// on. If RollupTeam ever diverged from sum(devs), this catches it.
	var manualCost float64
	for _, d := range resp.Developers {
		manualCost += d.TotalCostUSD
	}
	if resp.Total.TotalCostUSD != manualCost {
		t.Errorf("total.total_cost_usd = %v != Σ(devs) = %v",
			resp.Total.TotalCostUSD, manualCost)
	}
}

// TestGetScores_TotalAbsentWhenEmpty: when no developer has any cost in the
// window, the response includes no developers and no total — the dashboard
// shows "No data for this period." instead of a misleading $0 row.
func TestGetScores_TotalAbsentWhenEmpty(t *testing.T) {
	h, _ := newTestHandler(t)
	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", code, body)
	}
	// Use a permissive map decode so absent `total` is distinguishable
	// from present-but-zero.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := raw["total"]; present {
		t.Errorf("expected total to be absent when no developers, got: %s", body)
	}
}

// TestGetScores_TeamFilterStillSets covers backwards compatibility (#25):
// ?team= continues to populate the legacy "team" block. The total block
// also remains present (rolled across ALL developers, not just the filtered
// team). Both fields coexist.
func TestGetScores_TeamFilterStillSets(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	seedCosts(t, db, "alice", "issue-1", 100.0)
	seedCosts(t, db, "bob", "issue-2", 200.0)
	if err := db.UpsertHierarchy(ctx, "alice", "platform", "", "acme"); err != nil {
		t.Fatalf("UpsertHierarchy alice: %v", err)
	}
	if err := db.UpsertHierarchy(ctx, "bob", "growth", "", "acme"); err != nil {
		t.Fatalf("UpsertHierarchy bob: %v", err)
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?team=platform", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", code, body)
	}
	var resp struct {
		Total *struct {
			TotalCostUSD float64 `json:"total_cost_usd"`
		} `json:"total"`
		Team *struct {
			Team         string  `json:"team"`
			TotalCostUSD float64 `json:"total_cost_usd"`
		} `json:"team"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Total == nil || resp.Total.TotalCostUSD != 300.0 {
		t.Errorf("total = %+v, want non-nil with cost 300 (all devs)", resp.Total)
	}
	if resp.Team == nil || resp.Team.Team != "platform" || resp.Team.TotalCostUSD != 100.0 {
		t.Errorf("team = %+v, want {platform, 100} (alice only)", resp.Team)
	}
}

// TestGetScores_TeamFilterIncludesZeroCostSeat is the #39 team-rollup case: a
// developer who is an active org member with an allocated spend slice but zero
// token events (no costs) must still surface in the ?team= rollup — its
// ActualPaidUSD flows into the team's SpendLeverage denominator even though its
// TIER contribution is 0. Without the zero-cost surfacing this seat vanishes
// from the team total and the leverage denominator silently under-counts.
func TestGetScores_TeamFilterIncludesZeroCostSeat(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	// alice: real work + cost on platform.
	seedCosts(t, db, "alice", "issue-1", 100.0)
	if err := db.UpsertHierarchy(ctx, "alice", "platform", "", "acme"); err != nil {
		t.Fatalf("UpsertHierarchy alice: %v", err)
	}
	// zoe: active platform seat with $400 allocated spend but NO token events.
	if err := db.UpsertHierarchy(ctx, "zoe", "platform", "", "acme"); err != nil {
		t.Fatalf("UpsertHierarchy zoe: %v", err)
	}
	if err := db.InsertActualSpend(ctx, store.ActualSpend{
		Developer: "zoe", Period: time.Now().UTC().Format("2006-01"),
		ActualPaidMicro: 400.0 * store.MicroPerUSD, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertActualSpend zoe: %v", err)
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?team=platform", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", code, body)
	}
	var resp struct {
		Developers []struct {
			Developer     string  `json:"developer"`
			ActualPaidUSD float64 `json:"actual_paid_usd"`
		} `json:"developers"`
		Team *struct {
			Team          string  `json:"team"`
			TotalCostUSD  float64 `json:"total_cost_usd"`
			ActualPaidUSD float64 `json:"actual_paid_usd"`
		} `json:"team"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// zoe must appear as a surfaced developer row with her allocated spend.
	var foundZoe bool
	for _, d := range resp.Developers {
		if d.Developer == "zoe" {
			foundZoe = true
			if d.ActualPaidUSD != 400.0 {
				t.Errorf("zoe actual_paid_usd = %v, want 400.0", d.ActualPaidUSD)
			}
		}
	}
	if !foundZoe {
		t.Error("zero-cost active seat 'zoe' missing from surfaced developers (#39 regression)")
	}
	// Team rollup must fold zoe's $400 into the denominator alongside alice.
	if resp.Team == nil || resp.Team.ActualPaidUSD != 400.0 {
		t.Errorf("team = %+v, want ActualPaidUSD 400 (zero-cost seat folded in)", resp.Team)
	}
}

// TestGetScores_TeamFilterExcludesForeignTeamZeroCostSeat is the #39/team-filter
// cross-product: a zero-cost active seat on a DIFFERENT team must surface
// globally yet stay OUT of a ?team= rollup it doesn't belong to — otherwise the
// filtered team's SpendLeverage denominator over-counts a foreign seat's spend.
func TestGetScores_TeamFilterExcludesForeignTeamZeroCostSeat(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	period := time.Now().UTC().Format("2006-01")
	// alice: platform, real cost + $200 allocated spend.
	seedCosts(t, db, "alice", "issue-1", 100.0)
	if err := db.UpsertHierarchy(ctx, "alice", "platform", "", "acme"); err != nil {
		t.Fatalf("UpsertHierarchy alice: %v", err)
	}
	if err := db.InsertActualSpend(ctx, store.ActualSpend{
		Developer: "alice", Period: period, ActualPaidMicro: 200.0 * store.MicroPerUSD, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertActualSpend alice: %v", err)
	}
	// zoe: GROWTH team, zero token events but $400 allocated spend.
	if err := db.UpsertHierarchy(ctx, "zoe", "growth", "", "acme"); err != nil {
		t.Fatalf("UpsertHierarchy zoe: %v", err)
	}
	if err := db.InsertActualSpend(ctx, store.ActualSpend{
		Developer: "zoe", Period: period, ActualPaidMicro: 400.0 * store.MicroPerUSD, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertActualSpend zoe: %v", err)
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?team=platform", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", code, body)
	}
	var resp struct {
		Developers []struct {
			Developer string `json:"developer"`
		} `json:"developers"`
		Team *struct {
			ActualPaidUSD float64 `json:"actual_paid_usd"`
		} `json:"team"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// zoe still surfaces globally (zero-cost seat is not dropped)...
	var foundZoe bool
	for _, d := range resp.Developers {
		if d.Developer == "zoe" {
			foundZoe = true
		}
	}
	if !foundZoe {
		t.Error("foreign-team zero-cost seat 'zoe' missing from global developers (#39 regression)")
	}
	// ...but the platform rollup denominator is alice-only ($200), NOT $600.
	if resp.Team == nil || resp.Team.ActualPaidUSD != 200.0 {
		t.Errorf("team = %+v, want ActualPaidUSD 200 (zoe's growth-team spend excluded)", resp.Team)
	}
}

// TestGetScores_SurfacesSpendLeverage seeds a developer with $1000 of list-
// price cost and $400 of actual_spend, then asserts the /scores response
// surfaces both new fields and that SpendLeverage = 1000/400 = 2.5.
func TestGetScores_SurfacesSpendLeverage(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 1000.0)
	if err := db.InsertActualSpend(context.Background(), store.ActualSpend{
		Developer: "alice", Period: time.Now().UTC().Format("2006-01"),
		ActualPaidMicro: 400.0 * store.MicroPerUSD, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertActualSpend: %v", err)
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", code, body)
	}

	var resp struct {
		Developers []struct {
			Developer     string  `json:"developer"`
			TotalCostUSD  float64 `json:"total_cost_usd"`
			ActualPaidUSD float64 `json:"actual_paid_usd"`
			SpendLeverage float64 `json:"spend_leverage"`
		} `json:"developers"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Developers) != 1 {
		t.Fatalf("got %d developers, want 1", len(resp.Developers))
	}
	d := resp.Developers[0]
	if d.ActualPaidUSD != 400.0 {
		t.Errorf("actual_paid_usd = %v, want 400.0", d.ActualPaidUSD)
	}
	if d.SpendLeverage != 2.5 {
		t.Errorf("spend_leverage = %v, want 2.5", d.SpendLeverage)
	}
}

// TestGetScores_ZeroLeverageWhenNoActualSpend covers the dashboard-friendly
// "no leverage data yet" path: the field is present in the response but 0,
// not NaN (which json.Marshal would reject).
func TestGetScores_ZeroLeverageWhenNoActualSpend(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 1000.0)

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", code, body)
	}
	var resp struct {
		Developers []struct {
			SpendLeverage float64 `json:"spend_leverage"`
			ActualPaidUSD float64 `json:"actual_paid_usd"`
		} `json:"developers"`
	}
	_ = json.Unmarshal(body, &resp)
	if resp.Developers[0].SpendLeverage != 0 || resp.Developers[0].ActualPaidUSD != 0 {
		t.Errorf("expected zeros when no actual_spend recorded, got leverage=%v paid=%v",
			resp.Developers[0].SpendLeverage, resp.Developers[0].ActualPaidUSD)
	}
}

func TestGetDeveloperScore_SurfacesSpendLeverage(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 1000.0)
	if err := db.InsertActualSpend(context.Background(), store.ActualSpend{
		Developer: "alice", Period: time.Now().UTC().Format("2006-01"),
		ActualPaidMicro: 250.0 * store.MicroPerUSD, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertActualSpend: %v", err)
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores/alice", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", code, body)
	}
	var resp struct {
		SpendLeverage float64 `json:"spend_leverage"`
		ActualPaidUSD float64 `json:"actual_paid_usd"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ActualPaidUSD != 250.0 {
		t.Errorf("actual_paid_usd = %v, want 250.0", resp.ActualPaidUSD)
	}
	if resp.SpendLeverage != 4.0 {
		t.Errorf("spend_leverage = %v, want 4.0", resp.SpendLeverage)
	}
}

// teamsErrStore wraps a real *store.DB but forces TeamsForDevelopers to fail.
// Embedding satisfies the rest of the api.Store interface, so only the team
// lookup errors — letting us exercise the #94 item-3 handler error branch.
type teamsErrStore struct {
	*store.DB
}

func (teamsErrStore) TeamsForDevelopers(context.Context) (map[string]string, error) {
	return nil, errors.New("forced team lookup failure")
}

// TestGetScores_TeamFilterDBError pins the new fail-loud behavior (#94 item 3):
// when the bulk team lookup errors, the ?team= filter returns 500 rather than
// silently emitting an empty or partial team rollup. The old per-developer loop
// swallowed this error; nothing else exercises the new branch.
func TestGetScores_TeamFilterDBError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tier-api-test.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(teamsErrStore{db}, quiet, "", nil, "test", RateLimitConfig{})

	code, _ := doRequest(t, h, http.MethodGet, "/api/v1/scores?team=platform", nil)
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when TeamsForDevelopers errors", code)
	}
}

// TestPostActualSpend_OverBudgetWarns exercises the #94 item-2 ingestion-time
// data-quality signal end-to-end: posting a tier-1 invoice that pushes an org's
// active-member tier-1 sum past its contract total logs a WARN; a post that
// leaves it under budget does not.
func TestPostActualSpend_OverBudgetWarns(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	path := filepath.Join(t.TempDir(), "tier-api-test.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := New(db, logger, "", nil, "test", RateLimitConfig{})
	ctx := context.Background()
	// Current month so UpsertHierarchy's at-now membership covers the period.
	period := time.Now().UTC().Format("2006-01")

	// acme: $2000 org contract, two active members.
	for _, dev := range []string{"alice", "bob"} {
		if err := db.UpsertHierarchy(ctx, dev, "platform", "", "acme"); err != nil {
			t.Fatalf("UpsertHierarchy %s: %v", dev, err)
		}
	}
	if err := db.InsertOrgActualSpend(ctx, store.OrgActualSpend{
		Org: "acme", Period: period, ActualPaidMicro: 2000 * store.MicroPerUSD, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend: %v", err)
	}

	// First invoice keeps the org UNDER budget ($1500 < $2000) — no warn.
	code, _ := doRequest(t, h, http.MethodPost, "/api/v1/actual_spend",
		map[string]any{"developer": "alice", "period": period, "actual_paid_usd": 1500})
	if code != http.StatusCreated {
		t.Fatalf("alice POST status %d, want 201", code)
	}
	// Assert on the structured attr `overage_usd` (unique to this WARN) rather
	// than the free-text message, so a rephrase can't silently break the guard.
	if strings.Contains(logBuf.String(), "overage_usd") {
		t.Fatalf("unexpected over-budget warn while under budget: %s", logBuf.String())
	}

	// Second invoice pushes tier-1 sum to $2500 > $2000 — warn fires, names acme.
	code, _ = doRequest(t, h, http.MethodPost, "/api/v1/actual_spend",
		map[string]any{"developer": "bob", "period": period, "actual_paid_usd": 1000})
	if code != http.StatusCreated {
		t.Fatalf("bob POST status %d, want 201", code)
	}
	out := logBuf.String()
	// org name routed through logSafeStr (#321), so it renders %q-quoted even when
	// benign: `org="\"acme\""`.
	if !strings.Contains(out, `org="\"acme\""`) || !strings.Contains(out, "overage_usd=500") {
		t.Errorf("expected over-budget WARN (org=\"acme\" overage_usd=500), got: %s", out)
	}
}

// TestPostOrgActualSpend_OverBudgetWarns covers the SECOND warnOverBudget call
// site (handlePostOrgActualSpend): posting an org contract below the active
// members' existing tier-1 sum must fire the over-budget WARN. Without this,
// dropping that call site would pass every other test.
func TestPostOrgActualSpend_OverBudgetWarns(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	path := filepath.Join(t.TempDir(), "tier-api-test.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := New(db, logger, "", nil, "test", RateLimitConfig{})
	ctx := context.Background()
	period := time.Now().UTC().Format("2006-01")

	// acme: two active members with tier-1 invoices summing to $2500.
	for _, dev := range []string{"alice", "bob"} {
		if err := db.UpsertHierarchy(ctx, dev, "platform", "", "acme"); err != nil {
			t.Fatalf("UpsertHierarchy %s: %v", dev, err)
		}
	}
	for _, s := range []struct {
		dev string
		usd float64
	}{{"alice", 1500}, {"bob", 1000}} {
		if err := db.InsertActualSpend(ctx, store.ActualSpend{
			Developer: s.dev, Period: period, ActualPaidMicro: store.DollarsToMicro(s.usd), Timestamp: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("InsertActualSpend %s: %v", s.dev, err)
		}
	}

	// POST the org contract at $2000 — below the $2500 tier-1 sum — so the WARN
	// must fire via the org_actual_spend ingestion path.
	code, _ := doRequest(t, h, http.MethodPost, "/api/v1/org_actual_spend",
		map[string]any{"org": "acme", "period": period, "actual_paid_usd": 2000})
	if code != http.StatusCreated {
		t.Fatalf("org POST status %d, want 201", code)
	}
	out := logBuf.String()
	// The org name is routed through logSafeStr (#321 second-order log-injection
	// guard), so even a benign value renders %q-quoted: `org="\"acme\""`.
	if !strings.Contains(out, `org="\"acme\""`) || !strings.Contains(out, "overage_usd=500") {
		t.Errorf("expected over-budget WARN via org_actual_spend path (org=\"acme\" overage_usd=500), got: %s", out)
	}
}

// TestMetricsEndpoint_AuthGated covers #67: GET /metrics is bearer-gated (it
// exposes internal counters / spend-activity signal) and renders the registry.
func TestMetricsEndpoint_AuthGated(t *testing.T) {
	h, _ := newTestHandlerWithToken(t, "secret")
	reg := metrics.NewRegistry()
	reg.NewGauge("tier_build_info", "bi", "version").Set(1, "test")
	h.SetMetricsRegistry(reg)

	// No token -> 401.
	if code, _ := doRequest(t, h, http.MethodGet, "/metrics", nil); code != http.StatusUnauthorized {
		t.Fatalf("no-token /metrics = %d, want 401", code)
	}
	// With token -> 200 + exposition body.
	code, body := doRequestWithHeader(t, h, http.MethodGet, "/metrics", nil,
		http.Header{"Authorization": {"Bearer secret"}})
	if code != http.StatusOK {
		t.Fatalf("token /metrics = %d, want 200", code)
	}
	if !strings.Contains(string(body), "tier_build_info") {
		t.Errorf("metrics body missing tier_build_info:\n%s", body)
	}
}

// TestMetricsEndpoint_UnmountedWithoutRegistry confirms the route is absent when
// no registry is wired (e.g. tests, or a build that doesn't enable metrics).
func TestMetricsEndpoint_UnmountedWithoutRegistry(t *testing.T) {
	h, _ := newTestHandlerWithToken(t, "secret")
	code, _ := doRequestWithHeader(t, h, http.MethodGet, "/metrics", nil,
		http.Header{"Authorization": {"Bearer secret"}})
	if code != http.StatusNotFound {
		t.Errorf("/metrics without registry = %d, want 404", code)
	}
}

// TestMetricsEndpoint_ContentType pins the Prometheus v0.0.4 text Content-Type
// so scrapers parse the body without content negotiation.
func TestMetricsEndpoint_ContentType(t *testing.T) {
	h, _ := newTestHandlerWithToken(t, "secret")
	reg := metrics.NewRegistry()
	reg.NewGauge("tier_build_info", "bi", "version").Set(1, "test")
	h.SetMetricsRegistry(reg)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q, want text/plain; version=0.0.4...", ct)
	}
}

// TestPostCosts_MagnitudeCap verifies the #118 upper-bound gate on cost_usd,
// including the exact boundary: store.MaxCostUSD (1e12) is accepted, the first
// float above it is rejected, and a value an order of magnitude above is
// rejected with NO row inserted. Without the gate the bare DollarsToMicro of
// 1e13 dollars overflows int64 (implementation-defined), and on amd64 it lands
// a hugely NEGATIVE cost_micro that poisons every SUM aggregate forever. The
// handler uses a strict > comparison, so the boundary value 1e12 itself is legal.
func TestPostCosts_MagnitudeCap(t *testing.T) {
	cases := []struct {
		name      string
		cost      float64
		wantCode  int
		wantNoRow bool // for reject cases, assert the store stayed empty
	}{
		{"large but sane accepted", 999999.99, http.StatusCreated, false},
		{"exactly at cap accepted", store.MaxCostUSD, http.StatusCreated, false},
		{"just above cap rejected", math.Nextafter(store.MaxCostUSD, math.Inf(1)), http.StatusBadRequest, true},
		{"order of magnitude above cap rejected", 1e13, http.StatusBadRequest, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, db := newTestHandler(t)
			payload := map[string]any{
				"developer":     "alice",
				"issue_id":      "issue-118",
				"model":         "claude-sonnet-4",
				"cost_usd":      tc.cost,
				"input_tokens":  100,
				"output_tokens": 50,
			}
			code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload)
			if code != tc.wantCode {
				t.Fatalf("cost=%v: status = %d, want %d; body = %s", tc.cost, code, tc.wantCode, body)
			}
			if tc.wantCode == http.StatusBadRequest {
				// "<=" in the message is HTML-escaped by encoding/json to
				// <; assert on the escaping-independent parts.
				if !bytes.Contains(body, []byte("cost_usd")) || !bytes.Contains(body, []byte("1e12")) {
					t.Errorf("cost=%v: body = %s, want the cost_usd 1e12-cap message", tc.cost, body)
				}
			}
			if tc.wantNoRow {
				// No row may have been inserted; the reject must precede the
				// store write. A rejected POST leaves token_events empty, so
				// DeveloperCosts (SUM grouped by developer over ts>=since)
				// returns no rows.
				costs, err := db.DeveloperCosts(context.Background(), time.Now().Add(-time.Hour))
				if err != nil {
					t.Fatalf("DeveloperCosts: %v", err)
				}
				if len(costs) != 0 {
					t.Errorf("cost=%v: expected 0 developer rows after a rejected POST, got %d (%+v)", tc.cost, len(costs), costs)
				}
			}
		})
	}
}

// spendMagnitudeCapCases is the shared boundary table for the two actual-spend
// endpoints (#118). Negatives are legal there (credit memos, #24), so the gate
// is on |actual_paid_usd|: the -1e12..+1e12 band is accepted and anything of
// larger magnitude — including the -1e13 amd64 SUM-poison vector — is rejected.
// Boundaries are pinned on both signs because the cap edge is exactly where a
// >-vs->= regression would hide.
var spendMagnitudeCapCases = []struct {
	name     string
	amount   float64
	wantCode int
}{
	{"positive exactly at cap accepted", store.MaxCostUSD, http.StatusCreated},
	{"negative exactly at cap accepted", -store.MaxCostUSD, http.StatusCreated},
	{"just above cap rejected", math.Nextafter(store.MaxCostUSD, math.Inf(1)), http.StatusBadRequest},
	{"just below negative cap rejected", -math.Nextafter(store.MaxCostUSD, math.Inf(1)), http.StatusBadRequest},
	{"positive overflow rejected", 1e13, http.StatusBadRequest},
	{"negative overflow rejected (amd64 SUM-poison vector)", -1e13, http.StatusBadRequest},
}

// TestPostActualSpend_MagnitudeCap verifies the #118 magnitude gate on the
// developer actual-spend endpoint across the exact boundary.
func TestPostActualSpend_MagnitudeCap(t *testing.T) {
	for _, tc := range spendMagnitudeCapCases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHandler(t)
			payload := map[string]any{
				"developer":       "alice",
				"period":          "2026-07",
				"actual_paid_usd": tc.amount,
			}
			code, body := doRequest(t, h, http.MethodPost, "/api/v1/actual_spend", payload)
			if code != tc.wantCode {
				t.Fatalf("amount=%v: status = %d, want %d; body = %s", tc.amount, code, tc.wantCode, body)
			}
			if tc.wantCode == http.StatusBadRequest {
				// "<=" is HTML-escaped by encoding/json; assert escaping-independent.
				if !bytes.Contains(body, []byte("actual_paid_usd magnitude")) || !bytes.Contains(body, []byte("1e12")) {
					t.Errorf("amount=%v: body = %s, want the magnitude 1e12-cap message", tc.amount, body)
				}
			}
		})
	}
}

// TestPostOrgActualSpend_MagnitudeCap mirrors TestPostActualSpend_MagnitudeCap
// for the org-level endpoint (#118).
func TestPostOrgActualSpend_MagnitudeCap(t *testing.T) {
	for _, tc := range spendMagnitudeCapCases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHandler(t)
			payload := map[string]any{
				"org":             "acme",
				"period":          "2026-07",
				"actual_paid_usd": tc.amount,
			}
			code, body := doRequest(t, h, http.MethodPost, "/api/v1/org_actual_spend", payload)
			if code != tc.wantCode {
				t.Fatalf("amount=%v: status = %d, want %d; body = %s", tc.amount, code, tc.wantCode, body)
			}
			if tc.wantCode == http.StatusBadRequest {
				// "<=" is HTML-escaped by encoding/json; assert escaping-independent.
				if !bytes.Contains(body, []byte("actual_paid_usd magnitude")) || !bytes.Contains(body, []byte("1e12")) {
					t.Errorf("amount=%v: body = %s, want the magnitude 1e12-cap message", tc.amount, body)
				}
			}
		})
	}
}

// seedOutcome inserts a merged-PR outcome directly through the store so /scores
// has an outcome row to join against. Mirrors seedCosts.
func seedOutcome(t *testing.T, db *store.DB, developer, issueID string, weight, quality float64) {
	t.Helper()
	if _, err := db.InsertOutcome(context.Background(), store.Outcome{
		Developer: developer,
		IssueID:   issueID,
		Weight:    weight,
		Quality:   quality,
		Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}
}

// scoresDevs GETs /api/v1/scores and returns the decoded developer rows keyed
// by developer name.
func scoresDevs(t *testing.T, h *Handler) map[string]developerScoreJSON {
	t.Helper()
	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores: status = %d, body = %s", code, body)
	}
	var resp scoresResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal scores: %v", err)
	}
	out := map[string]developerScoreJSON{}
	for _, d := range resp.Developers {
		out[d.Developer] = d
	}
	return out
}

// TestGetScores_OutcomeOnlyDeveloperAppears is the #125 failing-first regression
// test. A developer who has outcomes but ZERO cost rows previously vanished from
// /scores entirely (the loop built rows only from cost rows + actual_spend keys),
// silently dropping their weighted points. It FAILS on main (row absent) and
// passes once handleGetScores builds rows over the union that includes outcome
// developers. Kept in handler_test.go (no branch-only symbols) so it compiles
// and demonstrably fails against main.
func TestGetScores_OutcomeOnlyDeveloperAppears(t *testing.T) {
	h, db := newTestHandler(t)
	seedOutcome(t, db, "gh-login", "issue-1", 3, 1)

	devs := scoresDevs(t, h)
	got, ok := devs["gh-login"]
	if !ok {
		t.Fatalf("developer gh-login absent from /scores; got %v", devs)
	}
	if got.WeightedPoints != 3 {
		t.Errorf("gh-login weighted_points = %v, want 3", got.WeightedPoints)
	}
	if got.TIER != 0 {
		t.Errorf("gh-login tier = %v, want 0 (no cost rows means TIER renders 0)", got.TIER)
	}
}

// TestGetScores_RankedFieldsInJSON pins the #133 wire contract: the four new
// fields ship on every developer row, a ranked developer carries a non-zero
// bootstrap CI bracketing their TIER, and an unranked (below-floor) developer
// carries ranked=false with both CI bounds at 0.
func TestGetScores_RankedFieldsInJSON(t *testing.T) {
	h, db := newTestHandler(t)
	// ranked: 3 outcomes + $10 cost clears both floors. Each outcome's issue
	// carries tokens (seedCosts on issue-1, seedTokens on 2 and 3) so none trips
	// the #136 zero-token tripwire, which would otherwise unrank the developer.
	seedCosts(t, db, "ranked-dev", "issue-1", 10.0)
	seedTokens(t, db, "ranked-dev", "issue-2", 5000)
	seedTokens(t, db, "ranked-dev", "issue-3", 5000)
	seedOutcome(t, db, "ranked-dev", "issue-1", 3, 1)
	seedOutcome(t, db, "ranked-dev", "issue-2", 5, 1)
	seedOutcome(t, db, "ranked-dev", "issue-3", 8, 1)
	// unranked: one lucky micro-cost PR — huge TIER, below both floors.
	seedCosts(t, db, "lucky", "issue-9", 0.0004)
	seedOutcome(t, db, "lucky", "issue-9", 0.5, 1)

	devs := scoresDevs(t, h)

	r, ok := devs["ranked-dev"]
	if !ok {
		t.Fatalf("ranked-dev absent; got %v", devs)
	}
	if !r.Ranked {
		t.Errorf("ranked-dev Ranked = false, want true")
	}
	if r.SampleN != 3 {
		t.Errorf("ranked-dev sample_n = %d, want 3", r.SampleN)
	}
	if r.CILow <= 0 || r.CIHigh <= 0 {
		t.Errorf("ranked-dev CIs = [%v, %v], want both > 0", r.CILow, r.CIHigh)
	}
	if r.CILow > r.TIER || r.TIER > r.CIHigh {
		t.Errorf("ranked-dev TIER %v not within CI [%v, %v]", r.TIER, r.CILow, r.CIHigh)
	}

	l, ok := devs["lucky"]
	if !ok {
		t.Fatalf("lucky absent; got %v", devs)
	}
	if l.Ranked {
		t.Errorf("lucky Ranked = true, want false")
	}
	if l.CILow != 0 || l.CIHigh != 0 {
		t.Errorf("unranked lucky CIs = [%v, %v], want [0, 0]", l.CILow, l.CIHigh)
	}
	if l.SampleN != 1 {
		t.Errorf("lucky sample_n = %d, want 1", l.SampleN)
	}
}

// TestGetScores_CostOnlyDeveloperUnrankedZeroSample is the exact wire-verify
// case (#133): a developer with cost rows but ZERO outcomes reaches /scores via
// the #125 union path and must serialize ranked=false, sample_n=0, and zero CIs
// — BootstrapCI must never be invoked for them (n==0 guard).
func TestGetScores_CostOnlyDeveloperUnrankedZeroSample(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "costonly", "issue-1", 500.0) // well over the $5 cost floor

	devs := scoresDevs(t, h)
	d, ok := devs["costonly"]
	if !ok {
		t.Fatalf("costonly absent from /scores; got %v", devs)
	}
	if d.Ranked {
		t.Errorf("costonly Ranked = true, want false (no outcomes)")
	}
	if d.SampleN != 0 {
		t.Errorf("costonly sample_n = %d, want 0", d.SampleN)
	}
	if d.CILow != 0 || d.CIHigh != 0 {
		t.Errorf("costonly CIs = [%v, %v], want [0, 0]", d.CILow, d.CIHigh)
	}
}

// TestAuthLockout_IndependentBucketsBehindTrustedProxy is the E-Y3 scenario
// (#131): behind a TLS terminator every request shares the terminator's IP, so
// keying the lockout on RemoteAddr lets one attacker lock out everyone. With a
// trusted CIDR configured, the lockout instead keys on the rightmost-untrusted
// X-Forwarded-For hop, so failures from client A never lock out client B —
// even though both arrive from the same trusted peer. Fails until the feature
// lands.
func TestAuthLockout_IndependentBucketsBehindTrustedProxy(t *testing.T) {
	const token = "s3cret-test-token-9f3a"
	h, _ := newTestHandlerWithTokenAndLimit(t, token, RateLimitConfig{
		MaxFailures:    3,
		Window:         time.Minute,
		Lockout:        15 * time.Minute,
		TrustedProxies: mustPrefixes(t, "10.0.0.0/8"),
	})
	mux := http.NewServeMux()
	h.Register(mux)

	body, _ := json.Marshal(validCostPayload())
	// All requests arrive from the same trusted peer (the terminator); only the
	// forged client identity in X-Forwarded-For differs.
	post := func(client string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/costs", bytes.NewReader(body))
		req.RemoteAddr = "10.0.0.5:443"
		req.Header.Set("X-Forwarded-For", client)
		req.Header.Set("Authorization", "Bearer wrong-token-value")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}

	const clientA = "203.0.113.7"
	const clientB = "198.51.100.9"
	// 3 bad auths from client A trip A's bucket.
	for i := 1; i <= 3; i++ {
		if c := post(clientA); c != http.StatusUnauthorized {
			t.Fatalf("client A attempt %d: status = %d, want 401", i, c)
		}
	}
	if c := post(clientA); c != http.StatusTooManyRequests {
		t.Fatalf("client A after threshold: status = %d, want 429 (locked)", c)
	}
	// Client B shares the peer but has its own bucket — still just a 401.
	if c := post(clientB); c != http.StatusUnauthorized {
		t.Errorf("client B (same trusted peer, different XFF): status = %d, want 401 — buckets must be independent", c)
	}
}

// TestAuthLockout_SharedBucketWithoutTrustFlag pins the DEFAULT behavior: with
// no trusted CIDR configured, X-Forwarded-For is never consulted, so two
// "clients" arriving from the same peer share one bucket. This is the
// intentional pre-#131 behavior (an attacker can't mint fresh buckets by
// varying a header they control); the trusted-proxy path is strictly opt-in.
func TestAuthLockout_SharedBucketWithoutTrustFlag(t *testing.T) {
	const token = "s3cret-test-token-9f3a"
	h, _ := newTestHandlerWithTokenAndLimit(t, token, RateLimitConfig{
		MaxFailures: 3,
		Window:      time.Minute,
		Lockout:     15 * time.Minute,
		// No TrustedProxies — XFF ignored entirely.
	})
	mux := http.NewServeMux()
	h.Register(mux)

	body, _ := json.Marshal(validCostPayload())
	post := func(client string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/costs", bytes.NewReader(body))
		req.RemoteAddr = "10.0.0.5:443"
		req.Header.Set("X-Forwarded-For", client)
		req.Header.Set("Authorization", "Bearer wrong-token-value")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}

	// 3 bad auths from "client A" trip the shared (peer-keyed) bucket.
	for i := 1; i <= 3; i++ {
		if c := post("203.0.113.7"); c != http.StatusUnauthorized {
			t.Fatalf("client A attempt %d: status = %d, want 401", i, c)
		}
	}
	// "client B" from the same peer is ALSO locked — shared bucket, by design.
	if c := post("198.51.100.9"); c != http.StatusTooManyRequests {
		t.Errorf("client B without trust flag: status = %d, want 429 — default shares one bucket per peer", c)
	}
}

// --- #136: zero-token-outcome tripwire ---

// decodeScores unmarshals a GET /api/v1/scores response body in full (including
// the #136 data_quality block), failing the test on a non-200 or bad JSON.
func decodeScores(t *testing.T, h *Handler) scoresResponse {
	t.Helper()
	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores: status = %d, body = %s", code, body)
	}
	var resp scoresResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal scores: %v", err)
	}
	return resp
}

// TestGetScores_ZeroTokenOutcomeFlagged is the C6/G-02 regression. A developer
// who clears BOTH #133 ranking floors (3 token-backed outcomes, $12 cost) but
// has one additional outcome whose issue recorded ZERO tokens must be forced
// unranked, report flagged_outcomes=1, and surface in the top-level data_quality
// block. Fails on main (fields/flag absent).
func TestGetScores_ZeroTokenOutcomeFlagged(t *testing.T) {
	h, db := newTestHandler(t)
	// Three token-backed outcomes + $12 cost → would be ranked on #133 alone.
	seedCosts(t, db, "offbooks", "issue-1", 4.0)
	seedCosts(t, db, "offbooks", "issue-2", 4.0)
	seedCosts(t, db, "offbooks", "issue-3", 4.0)
	seedOutcome(t, db, "offbooks", "issue-1", 3, 1)
	seedOutcome(t, db, "offbooks", "issue-2", 3, 1)
	seedOutcome(t, db, "offbooks", "issue-3", 3, 1)
	// The off-books outcome: merged, but its issue has NO recorded tokens.
	seedOutcome(t, db, "offbooks", "issue-off", 5, 1)

	resp := decodeScores(t, h)

	var d *developerScoreJSON
	for i := range resp.Developers {
		if resp.Developers[i].Developer == "offbooks" {
			d = &resp.Developers[i]
		}
	}
	if d == nil {
		t.Fatalf("offbooks absent from /scores; got %+v", resp.Developers)
	}
	if d.Ranked {
		t.Errorf("offbooks Ranked = true, want false (zero-token outcome must unrank)")
	}
	if d.FlaggedOutcomes != 1 {
		t.Errorf("offbooks flagged_outcomes = %d, want 1", d.FlaggedOutcomes)
	}
	// data_quality names the (developer, issue) pair.
	if resp.DataQuality == nil {
		t.Fatalf("data_quality block absent, want the flagged outcome")
	}
	found := false
	for _, z := range resp.DataQuality.ZeroTokenOutcomes {
		if z.Developer == "offbooks" && z.IssueID == "issue-off" {
			found = true
			if z.Tokens != 0 {
				t.Errorf("flagged tokens = %d, want 0", z.Tokens)
			}
		}
	}
	if !found {
		t.Errorf("data_quality.zero_token_outcomes missing {offbooks, issue-off}; got %+v",
			resp.DataQuality.ZeroTokenOutcomes)
	}
}

// TestGetScores_TokensAboveFloorNotFlagged is the negative control: an outcome
// whose issue recorded 1,001 tokens (just over MinAttributableTokens) in-window
// is NOT flagged, so the zero-token signal is absent. The data_quality block itself
// still ships the always-present honest-coverage fields (#351); only the zero-token
// signal is omit-when-clean.
func TestGetScores_TokensAboveFloorNotFlagged(t *testing.T) {
	h, db := newTestHandler(t)
	seedTokens(t, db, "clean", "issue-1", 1001) // 1 over the 1000 floor
	seedOutcome(t, db, "clean", "issue-1", 3, 1)

	resp := decodeScores(t, h)

	var d *developerScoreJSON
	for i := range resp.Developers {
		if resp.Developers[i].Developer == "clean" {
			d = &resp.Developers[i]
		}
	}
	if d == nil {
		t.Fatalf("clean absent from /scores; got %+v", resp.Developers)
	}
	if d.FlaggedOutcomes != 0 {
		t.Errorf("clean flagged_outcomes = %d, want 0 (1001 > 1000 floor)", d.FlaggedOutcomes)
	}
	if resp.DataQuality != nil &&
		(len(resp.DataQuality.ZeroTokenOutcomes) > 0 || resp.DataQuality.ZeroTokenOutcomeCount > 0) {
		t.Errorf("zero-token signal present with no flagged outcomes; got %+v", resp.DataQuality)
	}
}

// TestGetDeveloperScore_IssueZeroTokenField pins the per-issue zero_token flag on
// the detail endpoint: the token-backed issue reads false, the tokenless issue
// reads true, and the developer carries flagged_outcomes=1 / ranked=false.
func TestGetDeveloperScore_IssueZeroTokenField(t *testing.T) {
	h, db := newTestHandler(t)
	seedTokens(t, db, "dev", "issue-1", 5000)
	seedOutcome(t, db, "dev", "issue-1", 3, 1)
	seedOutcome(t, db, "dev", "issue-2", 5, 1) // no tokens → zero_token

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores/dev", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores/dev: status = %d, body = %s", code, body)
	}
	var resp developerDetailResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}

	byIssue := map[string]issueDetailJSON{}
	for _, iss := range resp.Issues {
		byIssue[iss.IssueID] = iss
	}
	if byIssue["issue-1"].ZeroToken {
		t.Errorf("issue-1 zero_token = true, want false (5000 tokens)")
	}
	if !byIssue["issue-2"].ZeroToken {
		t.Errorf("issue-2 zero_token = false, want true (no tokens)")
	}
	if resp.FlaggedOutcomes != 1 {
		t.Errorf("flagged_outcomes = %d, want 1", resp.FlaggedOutcomes)
	}
	if resp.Ranked {
		t.Errorf("Ranked = true, want false (a zero-token outcome must unrank)")
	}
}

// TestGetScores_TokenFloorBoundary pins the exact < MinAttributableTokens edge:
// exactly 1000 tokens is NOT flagged (the floor is `< 1000`, so 1000 clears it),
// while 999 IS flagged. This is the off-by-one a boundary test exists to catch.
func TestGetScores_TokenFloorBoundary(t *testing.T) {
	cases := []struct {
		name        string
		tokens      int
		wantFlagged int
	}{
		{"exactly-1000-not-flagged", 1000, 0},
		{"999-flagged", 999, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, db := newTestHandler(t)
			seedTokens(t, db, "dev", "issue-1", tc.tokens)
			seedOutcome(t, db, "dev", "issue-1", 3, 1)

			resp := decodeScores(t, h)
			var d *developerScoreJSON
			for i := range resp.Developers {
				if resp.Developers[i].Developer == "dev" {
					d = &resp.Developers[i]
				}
			}
			if d == nil {
				t.Fatalf("dev absent; got %+v", resp.Developers)
			}
			if d.FlaggedOutcomes != tc.wantFlagged {
				t.Errorf("flagged_outcomes = %d, want %d (%d tokens vs floor 1000)",
					d.FlaggedOutcomes, tc.wantFlagged, tc.tokens)
			}
		})
	}
}

// TestGetScores_MultipleFlaggedOutcomes covers the count/entry behavior with
// more than one flag: two flagged developers plus one developer with two
// distinct flagged issues. Asserts per-developer FlaggedOutcomes counts and that
// data_quality lists every (developer, issue) pair in sorted order.
func TestGetScores_MultipleFlaggedOutcomes(t *testing.T) {
	h, db := newTestHandler(t)
	// alice: two distinct zero-token issues → FlaggedOutcomes 2, two entries.
	seedOutcome(t, db, "alice", "issue-2", 3, 1)
	seedOutcome(t, db, "alice", "issue-1", 3, 1)
	// bob: one zero-token issue → FlaggedOutcomes 1, one entry.
	seedOutcome(t, db, "bob", "issue-9", 3, 1)

	resp := decodeScores(t, h)
	byDev := map[string]developerScoreJSON{}
	for _, d := range resp.Developers {
		byDev[d.Developer] = d
	}
	if byDev["alice"].FlaggedOutcomes != 2 {
		t.Errorf("alice flagged_outcomes = %d, want 2", byDev["alice"].FlaggedOutcomes)
	}
	if byDev["bob"].FlaggedOutcomes != 1 {
		t.Errorf("bob flagged_outcomes = %d, want 1", byDev["bob"].FlaggedOutcomes)
	}
	if resp.DataQuality == nil {
		t.Fatalf("data_quality absent, want 3 flagged pairs")
	}
	got := resp.DataQuality.ZeroTokenOutcomes
	want := []zeroTokenOutcomeJSON{
		{Developer: "alice", IssueID: "issue-1", Tokens: 0},
		{Developer: "alice", IssueID: "issue-2", Tokens: 0},
		{Developer: "bob", IssueID: "issue-9", Tokens: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("data_quality entries = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("data_quality[%d] = %+v, want %+v (sorted by developer, issue)", i, got[i], want[i])
		}
	}
}

// --- #42 GET /api/v1/org_actual_spend ---

// TestGetOrgActualSpend_RequiresAuth confirms the read-back is auth-gated like
// the score GETs (#59): spend data, not a public listing.
func TestGetOrgActualSpend_RequiresAuth(t *testing.T) {
	const token = "s3cret-test-token-9f3a"
	h, _ := newTestHandlerWithToken(t, token)

	// No header → 401.
	code, _ := doRequest(t, h, http.MethodGet, "/api/v1/org_actual_spend", nil)
	if code != http.StatusUnauthorized {
		t.Errorf("no header: status = %d, want 401", code)
	}

	// Correct token → 200.
	header := http.Header{"Authorization": []string{"Bearer " + token}}
	code, _ = doRequestWithHeader(t, h, http.MethodGet, "/api/v1/org_actual_spend", nil, header)
	if code != http.StatusOK {
		t.Errorf("with token: status = %d, want 200", code)
	}
}

// TestGetOrgActualSpend_ReturnsNetTotals seeds a POST invoice + credit memo,
// then reads back the net with the entry count, and confirms the JSON shape.
func TestGetOrgActualSpend_ReturnsNetTotals(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	now := time.Now().UTC()
	period := now.Format("2006-01")

	if err := db.InsertOrgActualSpend(ctx, store.OrgActualSpend{
		Org: "acme", Period: period, ActualPaidMicro: 500 * store.MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend(+500): %v", err)
	}
	if err := db.InsertOrgActualSpend(ctx, store.OrgActualSpend{
		Org: "acme", Period: period, ActualPaidMicro: -100 * store.MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend(-100): %v", err)
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/org_actual_spend", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", code, body)
	}
	var resp orgActualSpendResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v; body = %s", err, body)
	}
	if len(resp.Orgs) != 1 {
		t.Fatalf("got %d orgs, want 1; body = %s", len(resp.Orgs), body)
	}
	e := resp.Orgs[0]
	if e.Org != "acme" || e.Period != period {
		t.Errorf("entry key = %s/%s, want acme/%s", e.Org, e.Period, period)
	}
	if e.ActualPaidUSD != 400.0 {
		t.Errorf("actual_paid_usd = %v, want 400 (net of the credit memo)", e.ActualPaidUSD)
	}
	if e.Entries != 2 {
		t.Errorf("entries = %d, want 2 (invoice + credit memo)", e.Entries)
	}
}

// TestGetOrgActualSpend_EmptyIsEmptySlice pins the "orgs": [] (not null)
// contract on an empty result.
func TestGetOrgActualSpend_EmptyIsEmptySlice(t *testing.T) {
	h, _ := newTestHandler(t)
	code, body := doRequest(t, h, http.MethodGet, "/api/v1/org_actual_spend", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(string(body), `"orgs":[]`) {
		t.Errorf("body = %s, want an empty orgs array (not null)", body)
	}
}

// TestGetOrgActualSpend_OrgFilter confirms the org query param filters to an
// exact match.
func TestGetOrgActualSpend_OrgFilter(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	now := time.Now().UTC()
	period := now.Format("2006-01")

	for _, org := range []string{"acme", "globex"} {
		if err := db.InsertOrgActualSpend(ctx, store.OrgActualSpend{
			Org: org, Period: period, ActualPaidMicro: 100 * store.MicroPerUSD, Timestamp: now,
		}); err != nil {
			t.Fatalf("InsertOrgActualSpend(%s): %v", org, err)
		}
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/org_actual_spend?org=acme", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", code, body)
	}
	var resp orgActualSpendResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Orgs) != 1 || resp.Orgs[0].Org != "acme" {
		t.Errorf("org filter returned %+v, want only acme", resp.Orgs)
	}
}

// TestGetOrgActualSpend_InvalidSince400 confirms a malformed since is a 400.
func TestGetOrgActualSpend_InvalidSince400(t *testing.T) {
	h, _ := newTestHandler(t)
	code, _ := doRequest(t, h, http.MethodGet, "/api/v1/org_actual_spend?since=not-a-date", nil)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a malformed since", code)
	}
}

// --- #321: developer path-value log-injection guards ---
//
// GET /api/v1/scores/{developer} logs the requested developer (`target`, a
// percent-decoded path segment, so client-controlled and never charset-validated)
// on two db-error paths. These fakes force exactly one of those store calls to
// fail so the sink is reached deterministically: each embeds a real *store.DB (so
// every OTHER Store method behaves normally) and overrides just one method.

type spendFaultStore struct{ *store.DB }

func (spendFaultStore) ActualSpendAllWindow(context.Context, time.Time, time.Time) (map[string]float64, error) {
	return nil, errors.New("boom: actual_spend query failed")
}

type tokenFaultStore struct{ *store.DB }

func (tokenFaultStore) OutcomeTokenTotals(context.Context, []store.Outcome, store.RepoScope) (map[store.DevIssue]int64, error) {
	return nil, errors.New("boom: token totals query failed")
}

// TestGetDeveloperScore_TargetNotForgeable is a security regression guard (#321,
// CodeQL go/log-injection; sibling of TestPostOutcome_SHANotForgeable). A forged
// {developer} path value carrying a newline must not forge a standalone log record
// on either db-error sink. Both sinks route `target` through logSafeStr, so the
// newline is stripped and the value quoted.
func TestGetDeveloperScore_TargetNotForgeable(t *testing.T) {
	const forgedMarker = `level=ERROR msg="tier: auth bypassed"`
	forgedDev := "evildev\ntime=2026-07-12T00:00:00Z " + forgedMarker

	run := func(t *testing.T, mkStore func(*store.DB) Store, wantDiag string) {
		t.Helper()
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		db, err := store.Open(filepath.Join(t.TempDir(), "tier-api-test.db"))
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		h := New(mkStore(db), logger, "", nil, "test", RateLimitConfig{})

		// Call the handler directly so the forged value lands in PathValue without
		// depending on the mux's path-segment decoding rules.
		req := httptest.NewRequest(http.MethodGet, "/api/v1/scores/x", nil)
		req.SetPathValue("developer", forgedDev)
		h.handleGetDeveloperScore(httptest.NewRecorder(), req)

		logs := buf.String()
		if !strings.Contains(logs, wantDiag) {
			t.Fatalf("expected diagnostic %q; sink not reached:\n%s", wantDiag, logs)
		}
		for _, line := range strings.Split(logs, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), forgedMarker) {
				t.Fatalf("developer path value forged a standalone log record - log injection:\n%s", logs)
			}
		}
		// MECHANISM PIN + STRIP PROOF: CR/LF stripped and value %q-quoted, so the
		// halves join as `developer="\"evildevtime=`. Removing logSafeStr makes this fail.
		if !strings.Contains(logs, `developer="\"evildevtime=`) {
			t.Errorf("target not stripped+rendered via logSafeStr (expected `developer=\"\\\"evildevtime=...`):\n%s", logs)
		}
	}

	t.Run("actual_spend sink", func(t *testing.T) {
		run(t, func(db *store.DB) Store { return spendFaultStore{db} }, "query actual_spend for developer")
	})
	t.Run("token_totals sink", func(t *testing.T) {
		run(t, func(db *store.DB) Store { return tokenFaultStore{db} }, "query token totals for developer")
	})
}

// --- #321: SECOND-ORDER (stored) client-controlled log-injection guards ---
//
// Unlike the /outcomes SHA and /scores/{developer} path-value sinks (first-order,
// straight off the request), these two sinks log values that were STORED earlier
// and read back on a later request. The identifier is never re-validated on the
// read path — canon() does not strip CR/LF and org names are length-capped but not
// charset-validated — so a newline stored once forges a log record on every later
// read unless the sink sanitizes. Both sinks route the stored value through
// logSafeStr; removing the wrap makes each of these fail.

// TestWarnUnjoined_DeveloperNotForgeable is a stored-injection regression guard:
// a developer id carrying a newline, INSERTED as a token event, then read back by
// GET /scores where it lands cost-only, must not forge a standalone log record on
// the warnUnjoined WARN sink. The value flows store -> read -> log with no
// intervening charset validation, so the barrier at the sink is the only defense.
func TestWarnUnjoined_DeveloperNotForgeable(t *testing.T) {
	const forgedMarker = `level=ERROR msg="tier: auth bypassed"`
	forgedDev := "evildev\ntime=2026-07-12T00:00:00Z " + forgedMarker

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	db, err := store.Open(filepath.Join(t.TempDir(), "tier-api-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := New(db, logger, "", nil, "test", RateLimitConfig{})

	// Store a cost row for the forged developer and NO matching outcome, so the
	// scores join marks it cost-only and warnUnjoined(dev, "cost") fires.
	seedCosts(t, db, forgedDev, "issue-unjoined", 1.0)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/scores", nil)
	h.handleGetScores(httptest.NewRecorder(), req)

	logs := buf.String()
	if !strings.Contains(logs, "developer identity has no join partner") {
		t.Fatalf("expected unjoined-identity WARN; sink not reached:\n%s", logs)
	}
	// Secondary guard, for a hypothetical non-slog concatenation sink: slog's
	// TextHandler already %q-escapes an embedded newline, so this loop alone would
	// pass even if logSafeStr were removed. The STRIP PROOF below is the load-bearing
	// assertion — it is the one that fails on an unwrap. Keep both.
	for _, line := range strings.Split(logs, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), forgedMarker) {
			t.Fatalf("stored developer id forged a standalone log record - log injection:\n%s", logs)
		}
	}
	// MECHANISM PIN + STRIP PROOF: CR/LF stripped and value %q-quoted, so the
	// halves join as `developer="\"evildevtime=`. Removing logSafeStr makes this fail.
	if !strings.Contains(logs, `developer="\"evildevtime=`) {
		t.Errorf("developer not stripped+rendered via logSafeStr (expected `developer=\"\\\"evildevtime=...`):\n%s", logs)
	}
}

// overBudgetFaultStore embeds a real *store.DB (so InsertOrgActualSpend and every
// other method behave normally) and overrides just OverBudgetPeriods to return a
// stored, forged org name — modeling a row whose org was persisted earlier and is
// read back by warnOverBudget without charset validation.
type overBudgetFaultStore struct {
	*store.DB
	forged []store.OverBudgetPeriod
}

func (s overBudgetFaultStore) OverBudgetPeriods(context.Context, time.Time) ([]store.OverBudgetPeriod, error) {
	return s.forged, nil
}

// TestWarnOverBudget_OrgNotForgeable is a stored-injection regression guard: a
// persisted org name carrying a newline, read back by the ingestion-time
// warnOverBudget WARN sink, must not forge a standalone log record. The request
// org is benign; the forged value comes from the store (OverBudgetPeriods), so
// this exercises the second-order path the sink actually faces. p.Org routes
// through logSafeStr; removing the wrap makes this fail.
func TestWarnOverBudget_OrgNotForgeable(t *testing.T) {
	const forgedMarker = `level=ERROR msg="tier: auth bypassed"`
	forgedOrg := "evilorg\ntime=2026-07-12T00:00:00Z " + forgedMarker
	const period = "2026-07"

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	db, err := store.Open(filepath.Join(t.TempDir(), "tier-api-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fs := overBudgetFaultStore{DB: db, forged: []store.OverBudgetPeriod{{
		Org: forgedOrg, Period: period,
		OrgTotal: 100, Tier1Sum: 200, Overage: 100,
	}}}
	h := New(fs, logger, "", nil, "test", RateLimitConfig{})

	// Benign, validation-passing request body; the forged org is injected via the
	// store, not the request, so the sink sees the stored value on read-back.
	body := `{"org":"acme","period":"` + period + `","actual_paid_usd":50}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/org_actual_spend", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.handlePostOrgActualSpend(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}

	logs := buf.String()
	if !strings.Contains(logs, "org over budget:") {
		t.Fatalf("expected over-budget WARN; sink not reached:\n%s", logs)
	}
	// Secondary guard, for a hypothetical non-slog concatenation sink: slog's
	// TextHandler already %q-escapes an embedded newline, so this loop alone would
	// pass even if logSafeStr were removed. The STRIP PROOF below is the load-bearing
	// assertion — it is the one that fails on an unwrap. Keep both.
	for _, line := range strings.Split(logs, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), forgedMarker) {
			t.Fatalf("stored org name forged a standalone log record - log injection:\n%s", logs)
		}
	}
	// MECHANISM PIN + STRIP PROOF: CR/LF stripped and value %q-quoted, so the
	// halves join as `org="\"evilorgtime=`. Removing logSafeStr makes this fail.
	if !strings.Contains(logs, `org="\"evilorgtime=`) {
		t.Errorf("org not stripped+rendered via logSafeStr (expected `org=\"\\\"evilorgtime=...`):\n%s", logs)
	}
}
