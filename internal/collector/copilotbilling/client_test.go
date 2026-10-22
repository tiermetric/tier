package copilotbilling

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

const secretToken = "PAT-SECRET-NEVER-LOG"

func rsaFixture(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}
func mustClient(t *testing.T, cfg ClientConfig) *Client {
	t.Helper()
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func report(day time.Time, items ...usageItem) usageReport {
	var r usageReport
	r.Organization = "acme"
	r.TimePeriod.Year = day.Year()
	r.TimePeriod.Month = int(day.Month())
	r.TimePeriod.Day = day.Day()
	r.UsageItems = append([]usageItem{}, items...)
	return r
}
func item(model, product string, gross float64) usageItem {
	return usageItem{Model: model, Product: product, GrossAmount: &gross}
}
func writeReport(t *testing.T, w http.ResponseWriter, r usageReport) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(r); err != nil {
		t.Error(err)
	}
}

func TestCopilotJWTClaimsSignatureAndTokenCacheRefresh(t *testing.T) {
	key, keyPEM := rsaFixture(t)
	now := fixedNow
	exchanges, usageCalls := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != apiVersion {
			t.Error("missing required API headers")
		}
		if r.URL.Path == "/app/installations/123/access_tokens" {
			exchanges++
			if r.Method != http.MethodPost {
				t.Error("token exchange must POST")
			}
			jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			parts := strings.Split(jwt, ".")
			if len(parts) != 3 {
				t.Error("JWT shape")
				w.WriteHeader(400)
				return
			}
			decode := func(s string) []byte {
				b, e := base64.RawURLEncoding.DecodeString(s)
				if e != nil {
					t.Error(e)
				}
				return b
			}
			var header map[string]string
			if err := json.Unmarshal(decode(parts[0]), &header); err != nil {
				t.Error(err)
			}
			if header["alg"] != "RS256" || header["typ"] != "JWT" {
				t.Error("JWT header", header)
			}
			var claims struct {
				Iss      string `json:"iss"`
				Iat, Exp int64
			}
			if err := json.Unmarshal(decode(parts[1]), &claims); err != nil {
				t.Error(err)
			}
			if claims.Iss != "42" || claims.Iat != now.Add(-time.Minute).Unix() || claims.Exp != now.Add(9*time.Minute).Unix() {
				t.Errorf("claims=%+v", claims)
			}
			hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, hash[:], decode(parts[2])); err != nil {
				t.Error("RS256 verification", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": fmt.Sprintf("installation-%d", exchanges), "expires_at": now.Add(time.Hour).Format(time.RFC3339)})
			return
		}
		usageCalls++
		if r.Header.Get("Authorization") != fmt.Sprintf("Bearer installation-%d", exchanges) {
			t.Error("wrong cached token")
		}
		if r.URL.Path != "/organizations/acme/settings/billing/ai_credit/usage" || r.Method != "GET" || r.URL.Query().Get("user") != "alice" || r.URL.Query().Get("year") != "2026" || r.URL.Query().Get("month") != "10" || r.URL.Query().Get("day") != "19" {
			t.Error("usage contract mismatch")
		}
		writeUserReport(t, w, r, report(fixedNow.AddDate(0, 0, -2)))
	}))
	defer srv.Close()
	c := mustClient(t, ClientConfig{Org: "acme", BaseURL: srv.URL, AppID: "42", InstallationID: "123", PrivateKey: keyPEM, Now: func() time.Time { return now }})
	fetch := func() {
		t.Helper()
		if _, _, err := c.usage(t.Context(), fixedNow.AddDate(0, 0, -2), "alice", 0); err != nil {
			t.Fatal(err)
		}
	}
	fetch()
	now = now.Add(54 * time.Minute)
	fetch()
	if exchanges != 1 {
		t.Fatalf("cache refreshed too early: %d", exchanges)
	}
	now = now.Add(time.Minute)
	fetch()
	if exchanges != 2 || usageCalls != 3 {
		t.Fatalf("exchanges=%d usage=%d", exchanges, usageCalls)
	}
}

func TestCopilotClientAuthValidation(t *testing.T) {
	_, pemKey := rsaFixture(t)
	for name, cfg := range map[string]ClientConfig{
		"neither":     {Org: "acme"},
		"both":        {Org: "acme", Token: secretToken, AppID: "1", InstallationID: "2", PrivateKey: pemKey},
		"partial-app": {Org: "acme", AppID: "1"},
		"bad-key":     {Org: "acme", AppID: "1", InstallationID: "2", PrivateKey: "SECRET-BAD-PEM"},
		"bad-id":      {Org: "acme", AppID: "../secret", InstallationID: "2", PrivateKey: pemKey},
		"bad-origin":  {Org: "acme", Token: secretToken, BaseURL: "https://secret:password@example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewClient(cfg)
			if err == nil {
				t.Fatal("accepted invalid config")
			}
			if strings.Contains(err.Error(), secretToken) || strings.Contains(err.Error(), "SECRET-BAD-PEM") || strings.Contains(err.Error(), "password") {
				t.Fatal("secret in error")
			}
		})
	}
	// GitHub Apps also distribute PKCS#8 PEM.
	key, _ := rsaFixture(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	c := mustClient(t, ClientConfig{Org: "acme", AppID: "1", InstallationID: "2", PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))})
	if c.AuthMode() != "app" {
		t.Fatal("PKCS8 App auth not selected")
	}
}

func TestCopilotScopeWarnOnceAndAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name, scopes string
		warn         bool
	}{
		{"allowed", "read:org, admin:org, manage_billing:copilot", false},
		{"broad", "repo, read:org, delete_repo", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-OAuth-Scopes", tc.scopes)
				writeReport(t, w, report(fixedNow.AddDate(0, 0, -1)))
			}))
			defer srv.Close()
			c := mustClient(t, ClientConfig{Org: "acme", Token: secretToken, BaseURL: srv.URL, Now: func() time.Time { return fixedNow }, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			for range 2 {
				if res := c.Probe(t.Context()); res.Err != nil {
					t.Fatal(res.Err)
				}
			}
			count := strings.Count(logs.String(), "level=WARN")
			want := 0
			if tc.warn {
				want = 1
			}
			if count != want {
				t.Fatalf("warnings=%d want=%d logs=%s", count, want, logs.String())
			}
			if tc.warn && (!strings.Contains(logs.String(), "repo") || !strings.Contains(logs.String(), "delete_repo")) {
				t.Fatal("scope names absent")
			}
			if strings.Contains(logs.String(), secretToken) {
				t.Fatal("token leaked")
			}
		})
	}
}

func TestCopilotRetriesProbeAndRedirectHygiene(t *testing.T) {
	calls := 0
	var waits []time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "9999")
			w.WriteHeader(429)
			return
		}
		if calls == 2 {
			w.WriteHeader(503)
			return
		}
		writeReport(t, w, report(fixedNow.AddDate(0, 0, -1), item("model", "copilot", 2)))
	}))
	defer srv.Close()
	c := mustClient(t, ClientConfig{Org: "acme", Token: secretToken, BaseURL: srv.URL, Now: func() time.Time { return fixedNow }, Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }})
	if _, status, err := c.usage(t.Context(), fixedNow.AddDate(0, 0, -1), "", 3); err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if calls != 3 || len(waits) != 2 || waits[0] != 30*time.Second || waits[1] != time.Second {
		t.Fatalf("calls=%d waits=%v", calls, waits)
	}
	calls = 0
	res := c.Probe(t.Context())
	if res.StatusCode != 429 || res.Err == nil || calls != 1 || res.AuthMode != "token" {
		t.Fatalf("probe retried or misclassified: %+v calls=%d", res, calls)
	}
	reached := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++; w.WriteHeader(200) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer redirect.Close()
	c = mustClient(t, ClientConfig{Org: "acme", Token: secretToken, BaseURL: redirect.URL, HTTPClient: redirect.Client()})
	res = c.Probe(t.Context())
	if res.StatusCode != 302 || res.Err == nil || reached != 0 {
		t.Fatalf("redirect followed: %+v reached=%d", res, reached)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCopilotErrorsNeverExposeQueriesOrResponseBodies(t *testing.T) {
	c := mustClient(t, ClientConfig{Org: "acme", Token: secretToken, HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("unsafe transport %s %s", r.URL.String(), r.Header.Get("Authorization"))
	})}})
	_, _, err := c.usage(t.Context(), fixedNow, "roster-login", 0)
	if err == nil || strings.Contains(err.Error(), "roster-login") || strings.Contains(err.Error(), secretToken) || strings.Contains(err.Error(), "user=") {
		t.Fatalf("unsafe error: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user":"outsider-secret","usageItems":BROKEN-` + secretToken))
	}))
	defer srv.Close()
	c = mustClient(t, ClientConfig{Org: "acme", Token: secretToken, BaseURL: srv.URL})
	_, _, err = c.usage(t.Context(), fixedNow, "roster-login", 0)
	if err == nil || strings.Contains(err.Error(), secretToken) || strings.Contains(err.Error(), "outsider-secret") {
		t.Fatalf("unsafe JSON error: %v", err)
	}
}

func writeUserReport(t *testing.T, w http.ResponseWriter, req *http.Request, rep usageReport) {
	if rep.User == "" {
		rep.User = req.URL.Query().Get("user")
	}
	writeReport(t, w, rep)
}

func TestCopilotAPIBaseURLLoopbackHTTPOnly(t *testing.T) {
	for _, raw := range []string{"http://example.test", "https://example.test/path", "https://user:pass@example.test", "https://example.test?q=x", "https://example.test?", "https://example.test/#fragment"} {
		if _, err := NewClient(ClientConfig{Org: "acme", Token: secretToken, BaseURL: raw}); err == nil {
			t.Errorf("accepted unsafe base URL %s", raw)
		}
	}
	for _, raw := range []string{"http://localhost:1234", "http://127.0.0.1:1234", "http://127.1.2.3", "http://[::1]:1234", "https://example.test", "https://example.test/"} {
		if _, err := NewClient(ClientConfig{Org: "acme", Token: secretToken, BaseURL: raw}); err != nil {
			t.Errorf("refused safe base URL %s: %v", raw, err)
		}
	}
}

func TestCopilotRateLimit403RetriesResetAndNoPermissionHint(t *testing.T) {
	for _, seconds := range []int64{12, 9999, -1} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			calls := 0
			var waits []time.Duration
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls != 2 {
					w.Header().Set("X-RateLimit-Remaining", "0")
					w.Header().Set("X-RateLimit-Reset", fmt.Sprint(fixedNow.Unix()+seconds))
					w.WriteHeader(http.StatusForbidden)
					return
				}
				writeReport(t, w, report(fixedNow.AddDate(0, 0, -1)))
			}))
			defer srv.Close()
			c := mustClient(t, ClientConfig{Org: "acme", Token: secretToken, BaseURL: srv.URL, Now: func() time.Time { return fixedNow }, Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }})
			if _, _, err := c.usage(t.Context(), fixedNow.AddDate(0, 0, -1), "", 1); err != nil {
				t.Fatal(err)
			}
			want := min(max(time.Duration(seconds)*time.Second, 500*time.Millisecond), maxRetryBackoff)
			if calls != 2 || len(waits) != 1 || waits[0] != want {
				t.Fatalf("calls=%d waits=%v want=%v", calls, waits, want)
			}
			res := c.Probe(t.Context())
			if res.StatusCode != 403 || res.Err == nil || !res.RateLimited || strings.Contains(res.Err.Error(), permissionHint) || calls != 3 {
				t.Fatalf("rate limit probe: %+v calls=%d", res, calls)
			}
		})
	}
}
