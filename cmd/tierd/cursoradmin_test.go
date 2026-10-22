package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector/cursoradmin"
	"github.com/tiermetric/tier/internal/config"
)

func TestResolveCursorAdminConfig(t *testing.T) {
	str := func(s string) *string { return &s }
	integer := func(n int) *int { return &n }
	t.Run("absent", func(t *testing.T) {
		s, err := resolveCursorAdminConfig(nil)
		if err != nil || s != nil {
			t.Fatalf("s=%+v err=%v", s, err)
		}
	})
	t.Run("defaults", func(t *testing.T) {
		s, err := resolveCursorAdminConfig(&config.CursorAdminConfig{APIKey: str("key")})
		if err != nil || s == nil {
			t.Fatal(err)
		}
		if s.interval != time.Hour || s.settleDays != 1 || s.lookbackDays != 7 || s.baseURL != "" {
			t.Fatalf("settings=%+v", s)
		}
	})
	for _, tc := range []struct {
		name   string
		change func(*config.CursorAdminConfig)
	}{
		{"missing-key", func(c *config.CursorAdminConfig) { c.APIKey = nil }},
		{"empty-key", func(c *config.CursorAdminConfig) { c.APIKey = str("") }},
		{"whitespace-key", func(c *config.CursorAdminConfig) { c.APIKey = str("  ") }},
		{"short-interval", func(c *config.CursorAdminConfig) { c.PollInterval = str("59m") }},
		{"bad-interval", func(c *config.CursorAdminConfig) { c.PollInterval = str("yesterday") }},
		{"empty-interval", func(c *config.CursorAdminConfig) { c.PollInterval = str("") }},
		{"zero-settle", func(c *config.CursorAdminConfig) { c.SettleDays = integer(0) }},
		{"negative-lookback", func(c *config.CursorAdminConfig) { c.LookbackDays = integer(-1) }},
		{"inverted-days", func(c *config.CursorAdminConfig) { c.LookbackDays = integer(1); c.SettleDays = integer(2) }},
		{"relative-url", func(c *config.CursorAdminConfig) { c.APIBaseURL = str("/teams") }},
		{"credential-url", func(c *config.CursorAdminConfig) { c.APIBaseURL = str("https://user:pass@example.test") }},
		{"query-url", func(c *config.CursorAdminConfig) { c.APIBaseURL = str("https://example.test?secret=x") }},
		{"path-url", func(c *config.CursorAdminConfig) { c.APIBaseURL = str("https://example.test/teams") }},
		{"scheme-url", func(c *config.CursorAdminConfig) { c.APIBaseURL = str("file://example.test") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &config.CursorAdminConfig{APIKey: str("key")}
			tc.change(c)
			if _, err := resolveCursorAdminConfig(c); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	t.Run("file-key-and-overrides", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(path, []byte("secret-from-file\n"), 0600); err != nil {
			t.Fatal(err)
		}
		c := &config.CursorAdminConfig{APIKey: str("@" + path), APIBaseURL: str("http://127.0.0.1:1234/"), PollInterval: str("2h"), SettleDays: integer(2), LookbackDays: integer(10)}
		s, err := resolveCursorAdminConfig(c)
		if err != nil {
			t.Fatal(err)
		}
		if s.apiKey != "secret-from-file" || s.baseURL != "http://127.0.0.1:1234" || s.interval != 2*time.Hour || s.settleDays != 2 || s.lookbackDays != 10 {
			t.Fatal("overrides or secret file not resolved")
		}
	})
}

func TestCursorAdminDoctorStatusAndCountOnly(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   checkStatus
	}{{200, statusOK}, {401, statusFail}, {403, statusFail}, {429, statusWarn}, {500, statusWarn}, {418, statusWarn}, {0, statusFail}} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			status := tc.status
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				user, pass, ok := r.BasicAuth()
				if !ok || user != "secret" || pass != "" || r.URL.Path != "/teams/filtered-usage-events" || r.Method != "POST" {
					t.Error("wrong probe request")
				}
				var body struct {
					Start, End     int64
					Page, PageSize int
				}
				var raw map[string]int64
				if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
					t.Error(err)
				}
				body.Start = raw["startDate"]
				body.End = raw["endDate"]
				body.Page = int(raw["page"])
				body.PageSize = int(raw["pageSize"])
				today := time.Now().UTC().Truncate(24 * time.Hour)
				if body.Page != 1 || body.PageSize != 1 || body.Start != today.AddDate(0, 0, -1).UnixMilli() || body.End != today.UnixMilli()-1 {
					t.Errorf("probe body=%+v", body)
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"totalUsageEventsCount":42,"usageEvents":[{"userEmail":"private@example.test","conversationId":"private-conversation"}],"message":"secret"}`))
			}))
			defer srv.Close()
			key, base := "secret", srv.URL
			if status == 0 {
				// A closed loopback listener produces a transport error without retries.
				srv.Close()
			}
			cfg := &config.Config{Collectors: config.CollectorsConfig{CursorAdmin: &config.CursorAdminConfig{APIKey: &key, APIBaseURL: &base}}}
			results := checkCollectorConfigsAt(t.Context(), cfg, "", "")
			r := findCheck(t, results, "cursor_admin credential")
			wantCount := 0
			if status == 200 {
				wantCount = 42
			}
			wantCalls := 1
			if status == 0 {
				wantCalls = 0
			}
			if r.detail != fmt.Sprintf("HTTP %d; event count %d", status, wantCount) || calls != wantCalls {
				t.Fatalf("result=%+v calls=%d", r, calls)
			}
			if r.status != tc.want {
				t.Fatalf("status=%+v", r)
			}
			if (status == 401 || status == 403) && r.hint != cursoradmin.AuthHint {
				t.Fatalf("missing auth hint: %+v", r)
			}
			if tc.want == statusWarn && r.hint != "re-run doctor shortly; if it persists, check the provider's status page" {
				t.Fatalf("missing transient failure hint: %+v", r)
			}
			rendered := fmt.Sprint(r)
			for _, s := range []string{"private@example.test", "private-conversation", "secret"} {
				if strings.Contains(rendered, s) {
					t.Fatalf("doctor leaked %q", s)
				}
			}
		})
	}
}

func TestCursorAdminStartupWarnsManualDoubleCount(t *testing.T) {
	for _, name := range append(serveBoolEnvNames, "TIER_TRACKER_KEYS", "TIER_API_TOKEN", "TIER_READ_TOKEN", "TIER_METRICS_TOKEN", "TIER_INGEST_TOKEN", "TIER_WEBHOOK_SECRET", "TIER_PRICES") {
		t.Setenv(name, "")
	}
	restoreEmbeddedPriceTable(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "tier.yaml")
	if err := os.WriteFile(cfg, []byte("collectors:\n  cursor_admin:\n    api_key: test-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	logs := captureOSStderr(t)
	// Stop after API setup, before any poller starts or listener opens.
	code := runServeWithOptions([]string{
		"--db", filepath.Join(dir, "tier.db"), "--addr", "127.0.0.1:0",
		"--aggregation", "team", "--config", cfg, "--log-format", "json",
		"--watch-repo", filepath.Join(dir, "not-a-repo"),
	}, serveOptions{})
	out := logs()
	if code != 1 || !strings.Contains(out, "watch-repo:") {
		t.Fatalf("serve did not reach watch-repo validation: code=%d logs=%s", code, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `"level":"WARN"`) && strings.Contains(line, "collectors.cursor_admin is enabled") && strings.Contains(line, "POST /api/v1/costs") && strings.Contains(line, "usage made through Cursor") && strings.Contains(line, "TWICE") {
			return
		}
	}
	t.Fatalf("missing Cursor manual-cost overlap WARN: %s", out)
}

func TestCursorAdminMetricsReceipts(t *testing.T) {
	sm := newServeMetrics("test")
	m := sm.cursor
	m.PollComplete(true)
	m.PollComplete(false)
	m.EventsIngested(2)
	m.AuthFailure(403)
	m.Counts(cursoradmin.Counts{Fallback: 3, Uncosted: 1, BYO: 2, NonChargeable: 4, Clamped: 5})
	var output strings.Builder
	sm.reg.Render(&output)
	for _, want := range []string{
		`tier_cursor_admin_polls_total{outcome="ok"} 1`,
		`tier_cursor_admin_polls_total{outcome="error"} 1`,
		`tier_cursor_admin_delta_events_total 2`,
		`tier_cursor_admin_auth_failures_total{status="403"} 1`,
		`tier_cursor_admin_events_total{reason="fallback"} 3`,
		`tier_cursor_admin_events_total{reason="uncosted"} 1`,
		`tier_cursor_admin_events_total{reason="byo"} 2`,
		`tier_cursor_admin_events_total{reason="nonchargeable"} 4`,
		`tier_cursor_admin_events_total{reason="clamped"} 5`,
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing metric %s in %s", want, output.String())
		}
	}
}

func TestCursorAdminHTTPOnlyLoopback(t *testing.T) {
	for _, base := range []string{"http://example.test", "http://127.0.0.1:1234", "http://localhost:1234", "http://[::1]:1234"} {
		t.Run(base, func(t *testing.T) {
			key := "key"
			_, err := resolveCursorAdminConfig(&config.CursorAdminConfig{APIKey: &key, APIBaseURL: &base})
			if (err != nil) != (base == "http://example.test") {
				t.Fatalf("base=%s err=%v", base, err)
			}
		})
	}
}

func TestCursorAdminAuthMetricLabelsBounded(t *testing.T) {
	sm := newServeMetrics("test")
	for _, status := range []int{401, 403, 0, 429, 599, -1} {
		sm.cursor.AuthFailure(status)
	}
	var out strings.Builder
	sm.reg.Render(&out)
	for _, want := range []string{`tier_cursor_admin_auth_failures_total{status="401"} 1`, `tier_cursor_admin_auth_failures_total{status="403"} 1`, `tier_cursor_admin_auth_failures_total{status="other"} 4`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %s in %s", want, out.String())
		}
	}
}
