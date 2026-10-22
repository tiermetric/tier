package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/config"
)

func TestCopilotConfigValidationAndSecretFiles(t *testing.T) {
	if s, err := resolveCopilotBillingConfig(nil, quietLogger()); s != nil || err != nil {
		t.Fatalf("absent block enabled: %v %v", s, err)
	}
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("SECRET-COPILOT-TOKEN\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	keyPath := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer SECRET-COPILOT-TOKEN" {
			t.Error("resolved token file not used")
		}
		day := time.Now().UTC().AddDate(0, 0, -1)
		_ = json.NewEncoder(w).Encode(map[string]any{"timePeriod": map[string]int{"year": day.Year(), "month": int(day.Month()), "day": day.Day()}, "organization": "acme", "usageItems": []any{}})
	}))
	defer srv.Close()
	valid := func() *config.CopilotBillingConfig {
		return &config.CopilotBillingConfig{Org: strPtr("acme"), Token: strPtr("@" + tokenPath), APIBaseURL: strPtr(srv.URL)}
	}
	s, err := resolveCopilotBillingConfig(valid(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if s.client.AuthMode() != "token" || s.interval != 6*time.Hour || s.settleDays != 2 || s.lookbackDays != 7 {
		t.Fatalf("wrong resolved defaults: interval=%v settle=%d lookback=%d", s.interval, s.settleDays, s.lookbackDays)
	}
	if res := s.client.Probe(t.Context()); res.Err != nil {
		t.Fatal(res.Err)
	}
	app := &config.CopilotBillingConfig{Org: strPtr("acme"), AppID: strPtr("42"), InstallationID: strPtr("123"), PrivateKey: strPtr("@" + keyPath)}
	s, err = resolveCopilotBillingConfig(app, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if s.client.AuthMode() != "app" {
		t.Fatalf("App secret resolution failed: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*config.CopilotBillingConfig)
	}{
		{"neither", func(c *config.CopilotBillingConfig) { c.Token = nil }},
		{"both", func(c *config.CopilotBillingConfig) {
			c.AppID = app.AppID
			c.InstallationID = app.InstallationID
			c.PrivateKey = app.PrivateKey
		}},
		{"partial-app", func(c *config.CopilotBillingConfig) { c.Token = nil; c.AppID = app.AppID }},
		{"missing-org", func(c *config.CopilotBillingConfig) { c.Org = nil }},
		{"invalid-url", func(c *config.CopilotBillingConfig) { c.APIBaseURL = strPtr("https://secret:password@example.com") }},
		{"cleartext-remote", func(c *config.CopilotBillingConfig) { c.APIBaseURL = strPtr("http://example.test") }},
		{"base-path", func(c *config.CopilotBillingConfig) { c.APIBaseURL = strPtr("https://example.test/api") }},
		{"bad-interval", func(c *config.CopilotBillingConfig) { c.PollInterval = strPtr("bad") }},
		{"too-fast", func(c *config.CopilotBillingConfig) { c.PollInterval = strPtr("14m") }},
		{"unsettled", func(c *config.CopilotBillingConfig) { n := 0; c.SettleDays = &n }},
		{"negative-lookback", func(c *config.CopilotBillingConfig) { n := -1; c.LookbackDays = &n }},
		{"inverted-window", func(c *config.CopilotBillingConfig) { n := 8; c.SettleDays = &n }},
		{"too-long-lookback", func(c *config.CopilotBillingConfig) { n := 32; c.LookbackDays = &n }},
		{"missing-secret-file", func(c *config.CopilotBillingConfig) { c.Token = strPtr("@/missing/PRIVATE-TOKEN-PATH") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.mutate(c)
			_, err := resolveCopilotBillingConfig(c, quietLogger())
			if err == nil {
				t.Fatal("accepted invalid config")
			}
			for _, secret := range []string{"SECRET-COPILOT-TOKEN", "BEGIN RSA PRIVATE KEY", "password", "PRIVATE-TOKEN-PATH"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("secret in validation error")
				}
			}
		})
	}
	c := valid()
	c.PollInterval = strPtr("15m")
	n := 1
	c.SettleDays = &n
	c.LookbackDays = &n
	if _, err := resolveCopilotBillingConfig(c, quietLogger()); err != nil {
		t.Fatalf("minimum valid window: %v", err)
	}
	n = 31
	c.LookbackDays = &n
	if _, err := resolveCopilotBillingConfig(c, quietLogger()); err != nil {
		t.Fatalf("31-day window refused: %v", err)
	}
}

func TestCopilotDoctorProbeWire(t *testing.T) {
	for _, status := range []int{200, 401, 403, 404, 429, 503, 0} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			// The handler runs in a separate goroutine, including on transport failure.
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if status == 0 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				if r.URL.Query().Has("user") || r.URL.Path != "/organizations/acme/settings/billing/ai_credit/usage" || r.Header.Get("Authorization") != "Bearer DOCTOR-SECRET" {
					t.Error("wrong probe request")
				}
				day := time.Now().UTC().AddDate(0, 0, -1)
				if r.URL.Query().Get("day") != fmt.Sprint(day.Day()) || r.URL.Query().Get("month") != fmt.Sprint(int(day.Month())) || r.URL.Query().Get("year") != fmt.Sprint(day.Year()) {
					t.Error("probe not yesterday")
				}
				w.WriteHeader(status)
				if status == 200 {
					_ = json.NewEncoder(w).Encode(map[string]any{"timePeriod": map[string]int{"year": day.Year(), "month": int(day.Month()), "day": day.Day()}, "organization": "acme", "usageItems": []map[string]any{{"model": "SECRET-MODEL", "product": "copilot", "grossAmount": 2}}})
				} else {
					_, _ = w.Write([]byte("DOCTOR-SECRET outsider-private-login"))
				}
			}))
			defer srv.Close()
			cfg := &config.Config{}
			cfg.Collectors.CopilotBilling = &config.CopilotBillingConfig{Org: strPtr("acme"), Token: strPtr("DOCTOR-SECRET"), APIBaseURL: strPtr(srv.URL)}
			results := checkCollectorConfigsAt(t.Context(), cfg, "", "")
			if calls := calls.Load(); len(results) != 1 || calls != 1 {
				t.Fatalf("doctor probe missing/retried: results=%v calls=%d", results, calls)
			}
			r := results[0]
			want := statusWarn
			if status == 0 || status == 401 || status == 403 || status == 404 {
				want = statusFail
			}
			if status == 200 {
				want = statusOK
			}
			if r.status != want || !strings.Contains(r.detail, fmt.Sprintf("HTTP %d", status)) || !strings.Contains(r.detail, "auth mode token") {
				t.Fatalf("doctor result %+v", r)
			}
			if (status == 401 || status == 403 || status == 404) != strings.Contains(r.hint, "Administration: read") {
				t.Fatalf("wrong permission hint: %+v", r)
			}
			if status == 200 && !strings.Contains(r.detail, "items 1") {
				t.Fatalf("item count omitted: %+v", r)
			}
			for _, secret := range []string{"DOCTOR-SECRET", "outsider-private-login", "SECRET-MODEL"} {
				if strings.Contains(r.detail+r.hint, secret) {
					t.Fatal("doctor leaked vendor data or secret")
				}
			}
		})
	}
	cfg := &config.Config{}
	cfg.Collectors.CopilotBilling = &config.CopilotBillingConfig{}
	r := checkCollectorConfigsAt(t.Context(), cfg, "", "")
	if len(r) != 1 || r[0].status != statusFail {
		t.Fatal("doctor accepted empty config")
	}
}

func TestCopilotDoctorTransportFailureFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	results := checkCopilotBillingConfig(t.Context(), &config.CopilotBillingConfig{Org: strPtr("acme"), Token: strPtr("secret"), APIBaseURL: strPtr(srv.URL)})
	if len(results) != 1 || results[0].status != statusFail || !strings.Contains(results[0].detail, "endpoint unreachable") || !strings.Contains(results[0].hint, "check network access") {
		t.Fatalf("transport failure misclassified: %+v", results)
	}
}

func TestCopilotMetricsWire(t *testing.T) {
	sm := newServeMetrics("copilot-test")
	m := sm.copilot
	m.PollComplete(true)
	m.PollComplete(false)
	m.EventsIngested(3)
	m.Clamps(2)
	m.RemainderSkipped()
	for _, status := range []int{401, 403, 404, 429, 503, 599, 302} {
		m.HTTPError(status)
	}
	var out strings.Builder
	sm.reg.Render(&out)
	for _, want := range []string{
		`tier_copilot_billing_polls_total{outcome="ok"} 1`,
		`tier_copilot_billing_polls_total{outcome="error"} 1`,
		`tier_copilot_billing_delta_events_total 3`,
		`tier_copilot_billing_clamps_total 2`,
		`Copilot spend clamp observations per pass (not unique keys).`,
		`tier_copilot_billing_remainder_skipped_total 1`,
		`tier_copilot_billing_http_errors_total{status="401"} 1`,
		`tier_copilot_billing_http_errors_total{status="403"} 1`,
		`tier_copilot_billing_http_errors_total{status="404"} 1`,
		`tier_copilot_billing_http_errors_total{status="429"} 1`,
		`tier_copilot_billing_http_errors_total{status="5xx"} 2`,
		`tier_copilot_billing_http_errors_total{status="other"} 1`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("metric not wired: %s", want)
		}
	}
}

func TestCopilotServeCaptureEngineAndValidatedClient(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	capture, reused := false, false
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runServeWithOptions" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if ok && exprName(sel.X) == "copilotbilling" && sel.Sel.Name == "NewClient" {
					t.Error("serve reconstructs the client after startup validation")
				}
				if !ok || sel.Sel.Name != "WithCaptureEngines" || len(n.Args) != 1 {
					return true
				}
				lit, ok := n.Args[0].(*ast.CompositeLit)
				if !ok {
					return true
				}
				for _, e := range lit.Elts {
					kv, ok := e.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.BasicLit)
					if !ok || key.Value != `"copilot-billing"` {
						continue
					}
					var out bytes.Buffer
					if err := format.Node(&out, fset, kv.Value); err != nil {
						t.Fatal(err)
					}
					capture = out.String() == "copilotSettings != nil && !*readOnly"
				}
			case *ast.AssignStmt:
				if len(n.Rhs) == 1 {
					if sel, ok := n.Rhs[0].(*ast.SelectorExpr); ok && exprName(sel.X) == "copilotSettings" && sel.Sel.Name == "client" {
						reused = true
					}
				}
			}
			return true
		})
	}
	if !capture || !reused {
		t.Fatalf("capture engine configured=%v validated client reused=%v", capture, reused)
	}
}

func TestCopilotDoctorRateLimit403Warns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	results := checkCopilotBillingConfig(t.Context(), &config.CopilotBillingConfig{Org: strPtr("acme"), Token: strPtr("secret"), APIBaseURL: strPtr(srv.URL)})
	if len(results) != 1 || results[0].status != statusWarn || strings.Contains(results[0].hint, "Administration: read") {
		t.Fatalf("rate limit misclassified: %+v", results)
	}
}

func TestCopilotDoctorConfigHelpNamesCollector(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := runDoctor([]string{"--help"}, &out, &stderr); code != 0 || !strings.Contains(stderr.String(), "collectors.copilot_billing") {
		t.Fatalf("doctor help code=%d: %s", code, stderr.String())
	}
}
