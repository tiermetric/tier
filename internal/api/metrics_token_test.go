package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/metrics"
	"github.com/tiermetric/tier/internal/scoring"
)

// newTestHandlerWithMetricsScope arms all three credentials (#944) on a handler
// with a metrics registry wired, in the given aggregation mode.
func newTestHandlerWithMetricsScope(t *testing.T, mode scoring.AggregationMode) *Handler {
	t.Helper()
	h, _ := newTestHandlerWithScopes(t, scopeTestWrite, scopeTestRead)
	h.SetMetricsToken(scopeTestMetrics)
	h.SetAggregation(mode, scoring.DefaultKAnonymity)
	return h
}

// TestMetrics_MetricsTokenScrapesEveryMode pins #944's scrape matrix with the
// metrics token armed: in team and division mode the metrics and write tokens
// scrape and the read token is 403; in developer mode all three scrape.
func TestMetrics_MetricsTokenScrapesEveryMode(t *testing.T) {
	cases := []struct {
		mode     scoring.AggregationMode
		wantRead int
	}{
		{scoring.AggregationTeam, http.StatusForbidden},
		{scoring.AggregationDivision, http.StatusForbidden},
		{scoring.AggregationDeveloper, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.mode.String(), func(t *testing.T) {
			h := newTestHandlerWithMetricsScope(t, tc.mode)
			for _, c := range []struct {
				name, tok string
				want      int
			}{
				{"metrics", scopeTestMetrics, http.StatusOK},
				{"write", scopeTestWrite, http.StatusOK},
				{"read", scopeTestRead, tc.wantRead},
				{"none", "", http.StatusUnauthorized},
			} {
				code, body := doRequestWithHeader(t, h, http.MethodGet, "/metrics", nil, bearerHeader(c.tok))
				if code != c.want {
					t.Errorf("%s token GET /metrics = %d, want %d; body = %s", c.name, code, c.want, body)
				}
				if code == http.StatusOK && !strings.Contains(string(body), "tier_build_info") {
					t.Errorf("%s token GET /metrics 200 without the exposition; body = %s", c.name, body)
				}
			}
		})
	}
}

// recordingMux records every pattern Register and RegisterReadOnly mount (#944),
// so the walk below covers each registered route rather than a hand-kept list.
type recordingMux struct {
	patterns []string
}

func (m *recordingMux) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	m.patterns = append(m.patterns, pattern)
}

var wildcardRE = regexp.MustCompile(`\{[^}]*\}`)

// openRoutes are the routes that answer without any token. Every other
// registered route must 401 without one, so a route that silently loses its
// gate fails the walk instead of being mistaken for an open one.
var openRoutes = map[string]bool{
	"GET /api/v1/health":  true,
	"GET /api/v1/healthz": true,
	"GET /api/v1/livez":   true,
	"GET /api/v1/version": true,
}

// TestMetricsToken_OpensMetricsOnly walks EVERY route Register and
// RegisterReadOnly mount, in every aggregation mode, and pins that the
// metrics token opens GET /metrics and nothing else: 403 on every gated route
// (write, admin, read and export alike), and on an open route nothing beyond
// what an anonymous caller gets. The viewer token must get 403 on every route
// registered by registerWriteRoutes.
func TestMetricsToken_OpensMetricsOnly(t *testing.T) {
	type table struct {
		name     string
		register func(h *Handler, mux routeMux)
	}
	tables := []table{
		{"Register", func(h *Handler, mux routeMux) { h.Register(mux) }},
		{"RegisterReadOnly", func(h *Handler, mux routeMux) { h.RegisterReadOnly(mux) }},
	}
	modes := []scoring.AggregationMode{scoring.AggregationDeveloper, scoring.AggregationTeam, scoring.AggregationDivision}
	for _, tb := range tables {
		for _, mode := range modes {
			t.Run(tb.name+"/"+mode.String(), func(t *testing.T) {
				h := newTestHandlerWithMetricsScope(t, mode)
				rec := &recordingMux{}
				tb.register(h, rec)
				mux := http.NewServeMux()
				tb.register(h, mux)

				sort.Strings(rec.patterns)
				seen := map[string]bool{}
				for _, p := range rec.patterns {
					seen[p] = true
				}
				// Vacuity guards: the walk must include /metrics, and the full
				// table must include a write route.
				if !seen["GET /metrics"] {
					t.Fatalf("walk recorded no GET /metrics; patterns = %v", rec.patterns)
				}
				if tb.name == "Register" && !seen["POST /api/v1/events"] {
					t.Fatalf("Register walk recorded no POST /api/v1/events; patterns = %v", rec.patterns)
				}

				serve := func(pattern, tok string) (int, string) {
					method, path, _ := strings.Cut(pattern, " ")
					req := httptest.NewRequest(method, wildcardRE.ReplaceAllString(path, "x"), strings.NewReader("{}"))
					req.Header.Set("Content-Type", "application/json")
					if tok != "" {
						req.Header.Set("Authorization", "Bearer "+tok)
					}
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, req)
					return w.Code, w.Body.String()
				}
				for _, p := range rec.patterns {
					code, body := serve(p, scopeTestMetrics)
					anon, _ := serve(p, "")
					switch {
					case p == "GET /metrics":
						if code != http.StatusOK {
							t.Errorf("metrics token %s = %d, want 200; body = %s", p, code, body)
						}
					case openRoutes[p]:
						if code != anon {
							t.Errorf("metrics token %s = %d, anonymous = %d: the metrics token must add nothing on an open route", p, code, anon)
						}
					default:
						if anon != http.StatusUnauthorized {
							t.Errorf("anonymous %s = %d, want 401 (a gated route; add it to openRoutes only if it is meant to be open)", p, anon)
						}
						if code != http.StatusForbidden || !strings.Contains(body, "metrics token is not authorized") {
							t.Errorf("metrics token %s = %d, want 403 naming the metrics token; body = %s", p, code, body)
						}
					}
				}
				if tb.name == "Register" {
					writes := &recordingMux{}
					h.registerWriteRoutes(writes)
					if len(writes.patterns) == 0 {
						t.Fatal("registerWriteRoutes recorded no patterns")
					}
					for _, p := range writes.patterns {
						if code, body := serve(p, scopeTestRead); code != http.StatusForbidden || !strings.Contains(body, errReadTokenForbidden) {
							t.Errorf("viewer token %s = %d, want 403 naming the read-only token; body = %s", p, code, body)
						}
					}
				}
			})
		}
	}
}

// TestMetricsToken_RefusedOnProxy pins that the metrics token cannot open the
// upstream provider relay through X-Tier-Token (#944).
func TestMetricsToken_RefusedOnProxy(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := newTestHandlerWithMetricsScope(t, scoring.AggregationDeveloper)
	do := func(tok string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set(ProxyTokenHeader, tok)
		rec := httptest.NewRecorder()
		h.ProxyAuth(upstream).ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do(scopeTestMetrics); code != http.StatusForbidden {
		t.Errorf("metrics token on proxy: status = %d, want 403", code)
	}
	if code := do(scopeTestWrite); code != http.StatusOK {
		t.Errorf("write token on proxy: status = %d, want 200 (control)", code)
	}
}

// TestMetricsToken_Classify pins that each configured secret classifies to its
// own scope and that a near-miss matches none.
func TestMetricsToken_Classify(t *testing.T) {
	h := newTestHandlerWithMetricsScope(t, scoring.AggregationTeam)
	cases := []struct {
		candidate string
		want      authScope
	}{
		{scopeTestWrite, scopeWrite},
		{scopeTestRead, scopeRead},
		{scopeTestMetrics, scopeMetrics},
		{scopeTestMetrics[:len(scopeTestMetrics)-1] + "X", scopeNone},
		{scopeTestMetrics[:10], scopeNone},
		{"", scopeNone},
	}
	for _, tc := range cases {
		if got := h.classify([]byte(tc.candidate)); got != tc.want {
			t.Errorf("classify(%q) = %d, want %d", tc.candidate, got, tc.want)
		}
	}
	// Unarmed: with no metrics token configured, the value matches nothing.
	h.SetMetricsToken("")
	if got := h.classify([]byte(scopeTestMetrics)); got != scopeNone {
		t.Errorf("unarmed classify(metrics token) = %d, want scopeNone", got)
	}
}

// TestMetricsToken_SharesLockout pins that /metrics sits behind the shared
// per-IP failed-auth lockout (#36): once the failure budget is spent, even the
// valid metrics token gets 429. A wrong-scope 403 is a valid credential, so it
// must neither count as a failure nor reset the count. Two wrong-scope 403s are
// pinned: the metrics token on a read route, and the read token on team-mode
// /metrics. Each case runs from its own client IP.
func TestMetricsToken_SharesLockout(t *testing.T) {
	h, _ := newTestHandlerWithTokenAndLimit(t, scopeTestWrite,
		RateLimitConfig{MaxFailures: 2, Window: time.Minute, Lockout: 15 * time.Minute})
	h.SetReadToken(scopeTestRead)
	h.SetMetricsToken(scopeTestMetrics)
	h.SetAggregation(scoring.AggregationTeam, scoring.DefaultKAnonymity)
	reg := metrics.NewRegistry()
	reg.NewGauge("tier_build_info", "bi", "version").Set(1, "test")
	h.SetMetricsRegistry(reg)
	mux := http.NewServeMux()
	h.Register(mux)
	get := func(ip, path, tok string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = ip + ":4000"
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	const bad = "wrong-token-of-length-32-zzzzzzz"
	cases := []struct {
		name, wrongPath, wrongTok string
		ipCount, ipReset          string
	}{
		{"metrics token on /scores", "/api/v1/scores", scopeTestMetrics, "203.0.113.7", "203.0.113.8"},
		{"read token on team-mode /metrics", "/metrics", scopeTestRead, "203.0.113.9", "203.0.113.10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Does not count: three wrong-scope 403s leave the budget untouched.
			for i := 0; i < 3; i++ {
				if c := get(tc.ipCount, tc.wrongPath, tc.wrongTok); c != http.StatusForbidden {
					t.Fatalf("wrong-scope request #%d = %d, want 403", i, c)
				}
			}
			if c := get(tc.ipCount, "/metrics", scopeTestMetrics); c != http.StatusOK {
				t.Fatalf("metrics token after wrong-scope 403s = %d, want 200 (a wrong scope is not a failure)", c)
			}
			// Does not reset: bad, wrong-scope 403, bad spends the budget of 2.
			if c := get(tc.ipReset, "/metrics", bad); c != http.StatusUnauthorized {
				t.Fatalf("bad token = %d, want 401", c)
			}
			if c := get(tc.ipReset, tc.wrongPath, tc.wrongTok); c != http.StatusForbidden {
				t.Fatalf("wrong-scope request = %d, want 403", c)
			}
			if c := get(tc.ipReset, "/metrics", bad); c != http.StatusUnauthorized {
				t.Fatalf("second bad token = %d, want 401", c)
			}
			if c := get(tc.ipReset, "/metrics", scopeTestMetrics); c != http.StatusTooManyRequests {
				t.Errorf("valid metrics token after the failure budget = %d, want 429 (a wrong-scope 403 must not reset the count)", c)
			}
		})
	}
}
