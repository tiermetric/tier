package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/proxy"
	"github.com/tiermetric/tier/internal/store"
)

// TestProxyWiring_TierTokenNeverReachesUpstream pins #865 through the composition
// runServe mounts: api.Handler.ProxyAuth around proxy.New, via registerProxy. The
// upstream must never see X-Tier-Token, with --api-token set AND with it empty —
// the tokenless arm is the one that leaked, because ProxyAuth is then a
// pass-through — while the provider's own auth headers still arrive.
func TestProxyWiring_TierTokenNeverReachesUpstream(t *testing.T) {
	const apiToken = "wiring-test-api-token-865"
	for _, tc := range []struct {
		name     string
		apiToken string // tierd --api-token; "" is no-auth mode
	}{
		{"auth on", apiToken},
		{"auth off", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var hits int
			var got http.Header
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				hits++
				got = r.Header.Clone()
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_865","model":"claude-sonnet-4","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer upstream.Close()

			db, err := store.Open(filepath.Join(t.TempDir(), "proxy-token-865.db"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			logger := discardLogger()
			h := api.New(db, logger, tc.apiToken, nil, "test", api.RateLimitConfig{})
			target, _ := url.Parse(upstream.URL)
			p := proxy.New(target, proxy.ProviderAnthropic, collector.SourceProxy, ingester.Store(db), nil, nil, logger)
			mux := http.NewServeMux()
			registerProxy(mux, "/anthropic", p, h.ProxyAuth, logger)
			srv := httptest.NewServer(mux)
			defer srv.Close()

			// With auth off the client may send any value here; it is still the
			// header a misconfigured or shared client carries, and must not leak.
			sent := tc.apiToken
			if sent == "" {
				sent = "client-sent-token-in-no-auth-mode"
			}
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(`{}`)) //nolint:noctx // loopback test POST
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(api.ProxyTokenHeader, sent)
			req.Header.Set("X-Api-Key", "provider-key")
			req.Header.Set("Authorization", "Bearer provider-bearer")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST through proxy: %v", err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}

			mu.Lock()
			defer mu.Unlock()
			if hits != 1 {
				t.Fatalf("upstream hits = %d, want 1", hits)
			}
			if v, ok := got[http.CanonicalHeaderKey(api.ProxyTokenHeader)]; ok {
				t.Errorf("upstream saw %s = %q, want it stripped", api.ProxyTokenHeader, v)
			}
			if g := got.Get("X-Api-Key"); g != "provider-key" {
				t.Errorf("upstream X-Api-Key = %q, want provider-key", g)
			}
			if g := got.Get("Authorization"); g != "Bearer provider-bearer" {
				t.Errorf("upstream Authorization = %q, want the provider bearer", g)
			}
		})
	}
}
