package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/api"
)

// guardReq sends one request through browserGuard over a stub and reports the
// status and whether the stub ran.
func guardReq(t *testing.T, checkHost bool, method, target, host string, header http.Header) (int, bool) {
	t.Helper()
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusTeapot)
	})
	req := httptest.NewRequest(method, target, strings.NewReader(`{}`))
	req.Host = host
	for k, vs := range header {
		req.Header[k] = vs
	}
	rec := httptest.NewRecorder()
	browserGuard(next, checkHost).ServeHTTP(rec, req)
	return rec.Code, reached
}

// TestBrowserGuard_CrossSiteOriginRefused pins #893's measured request: a POST
// carrying Origin: https://evil.example, and the other origins that are not
// this server's own (another loopback port, the opaque "null" origin). /api/
// is refused on every method; the provider proxies on a write (#903), where a
// cross-site text/plain POST would otherwise be relayed and metered as spend.
func TestBrowserGuard_CrossSiteOriginRefused(t *testing.T) {
	requests := []struct{ method, target string }{
		{http.MethodPost, "/api/v1/developer_alias"},
		{http.MethodGet, "/api/v1/developer_alias"},
		{http.MethodPost, "/openai/v1/chat/completions"},
		{http.MethodPost, "/gemini/v1beta/models/x:generateContent"},
	}
	for _, origin := range []string{"https://evil.example", "http://127.0.0.1:3000", "null"} {
		for _, rq := range requests {
			code, reached := guardReq(t, false, rq.method, rq.target, "127.0.0.1:8080",
				http.Header{"Origin": {origin}})
			if code != http.StatusForbidden || reached {
				t.Errorf("%s %s Origin %q: status = %d reached = %v, want 403 unreached", rq.method, rq.target, origin, code, reached)
			}
		}
	}
}

// TestBrowserGuard_SecFetchSiteRefused pins that cross-site and same-site are
// both refused: a page on another localhost port is same-site.
func TestBrowserGuard_SecFetchSiteRefused(t *testing.T) {
	for _, site := range []string{"cross-site", "same-site"} {
		code, reached := guardReq(t, false, http.MethodPost, "/api/v1/costs", "127.0.0.1:8080",
			http.Header{"Sec-Fetch-Site": {site}})
		if code != http.StatusForbidden || reached {
			t.Errorf("Sec-Fetch-Site %q: status = %d reached = %v, want 403 unreached", site, code, reached)
		}
	}
}

// TestBrowserGuard_SameOriginPasses pins the dashboard's own requests: a
// matching Origin, and Sec-Fetch-Site same-origin or none, which wins over an
// Origin whose host a Host-rewriting proxy made differ.
func TestBrowserGuard_SameOriginPasses(t *testing.T) {
	cases := []struct {
		name   string
		host   string
		header http.Header
	}{
		{"matching Origin", "127.0.0.1:8080", http.Header{"Origin": {"http://127.0.0.1:8080"}}},
		{"matching Origin behind TLS proxy", "tier.example.com", http.Header{"Origin": {"https://tier.example.com"}}},
		{"same-origin despite rewritten Host", "127.0.0.1:8080", http.Header{
			"Sec-Fetch-Site": {"same-origin"}, "Origin": {"https://tier.example.com"}}},
		{"typed URL", "127.0.0.1:8080", http.Header{"Sec-Fetch-Site": {"none"}}},
	}
	for _, tc := range cases {
		code, reached := guardReq(t, false, http.MethodPost, "/api/v1/developer_alias", tc.host, tc.header)
		if code != http.StatusTeapot || !reached {
			t.Errorf("%s: status = %d reached = %v, want the handler", tc.name, code, reached)
		}
	}
}

// TestBrowserGuard_NoBrowserHeadersPasses pins the shipper/curl shape: no
// Origin, no Sec-Fetch-Site, on a loopback Host with the Host check armed.
func TestBrowserGuard_NoBrowserHeadersPasses(t *testing.T) {
	code, reached := guardReq(t, true, http.MethodPost, "/api/v1/events", "127.0.0.1:8080", nil)
	if code != http.StatusTeapot || !reached {
		t.Fatalf("status = %d reached = %v, want the handler", code, reached)
	}
}

// TestBrowserGuard_LoopbackBindRefusesForeignHost pins the DNS-rebinding
// guard: a page at evil.example rebound to 127.0.0.1 sends Host evil.example,
// on the API and on every other path (dashboard, proxies).
func TestBrowserGuard_LoopbackBindRefusesForeignHost(t *testing.T) {
	for _, target := range []string{"/api/v1/scores", "/", "/anthropic/v1/messages"} {
		for _, host := range []string{"evil.example", "evil.example:8080", "localhost.evil.example:8080", ""} {
			code, reached := guardReq(t, true, http.MethodGet, target, host, nil)
			if code != http.StatusForbidden || reached {
				t.Errorf("%s Host %q: status = %d reached = %v, want 403 unreached", target, host, code, reached)
			}
		}
	}
}

// TestBrowserGuard_LoopbackBindAcceptsLoopbackHosts pins every name a local
// client dials a loopback listener by.
func TestBrowserGuard_LoopbackBindAcceptsLoopbackHosts(t *testing.T) {
	for _, host := range []string{"127.0.0.1:8080", "localhost:8080", "LOCALHOST:8080", "[::1]:8080", "127.0.0.1", "localhost", "[::1]", "127.1.2.3:8080"} {
		code, reached := guardReq(t, true, http.MethodGet, "/api/v1/scores", host, nil)
		if code != http.StatusTeapot || !reached {
			t.Errorf("Host %q: status = %d reached = %v, want the handler", host, code, reached)
		}
	}
}

// TestBrowserGuard_NonLoopbackBindAnyHost pins that the Host check is off when
// not armed: an operator's real hostname or proxy Host passes.
func TestBrowserGuard_NonLoopbackBindAnyHost(t *testing.T) {
	code, reached := guardReq(t, false, http.MethodGet, "/api/v1/scores", "tier.example.com", nil)
	if code != http.StatusTeapot || !reached {
		t.Fatalf("status = %d reached = %v, want the handler", code, reached)
	}
}

// TestBrowserGuard_CrossOriginReadsScopedToAPI pins that a cross-origin read
// is refused only on /api/: a link from another site to the dashboard or the
// docs (the demo is linked from tiermetric.org) still opens.
func TestBrowserGuard_CrossOriginReadsScopedToAPI(t *testing.T) {
	for _, target := range []string{"/", "/docs/quickstart"} {
		code, reached := guardReq(t, false, http.MethodGet, target, "demo.tiermetric.org",
			http.Header{"Sec-Fetch-Site": {"cross-site"}, "Origin": {"https://tiermetric.org"}})
		if code != http.StatusTeapot || !reached {
			t.Errorf("%s: status = %d reached = %v, want the handler", target, code, reached)
		}
	}
}

// TestHostCheckArmed pins when runServe arms the Host check: tokenless on a
// loopback address only. A token-mode tierd on 127.0.0.1 behind a proxy that
// forwards a public Host, and the synthetic demo behind its tunnel, stay off.
func TestHostCheckArmed(t *testing.T) {
	cases := []struct {
		addr, token string
		demo, want  bool
	}{
		{"127.0.0.1:8080", "", false, true},
		{"localhost:8080", "", false, true},
		{"[::1]:8080", "", false, true},
		{"127.0.0.1:8080", "tok", false, false},
		{"127.0.0.1:8080", "", true, false},
		{"0.0.0.0:8080", "", false, false},
		{"0.0.0.0:8080", "tok", false, false},
		{":8080", "tok", false, false},
	}
	for _, tc := range cases {
		if got := hostCheckArmed(tc.addr, tc.token, tc.demo); got != tc.want {
			t.Errorf("hostCheckArmed(%q, token=%v, demo=%v) = %v, want %v", tc.addr, tc.token != "", tc.demo, got, tc.want)
		}
	}
}

// TestBrowserGuard_WebhookUnaffected pins that the webhook keeps working on a
// tokenless loopback tierd reached through a tunnel: GitHub may deliver
// form-encoded, from no browser, with the tunnel's public Host. Its trust is
// the HMAC signature, so the Host check skips it. The stub is mounted directly
// on the mux beside the real API routes, as runServe mounts webhook.New, so
// requireJSON (inside the API's requireAuth) never sees it.
func TestBrowserGuard_WebhookUnaffected(t *testing.T) {
	h := api.New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil, "test", api.RateLimitConfig{})
	mux := http.NewServeMux()
	h.Register(mux)
	reached := false
	mux.HandleFunc("POST "+webhookPath, func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusAccepted)
	})
	req := httptest.NewRequest(http.MethodPost, webhookPath, strings.NewReader("payload=%7B%7D"))
	req.Host = "hooks.tunnel.example"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	browserGuard(mux, true).ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || !reached {
		t.Fatalf("status = %d reached = %v, want the webhook handler", rec.Code, reached)
	}
}
