package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/metrics"
)

func TestResolveFormat(t *testing.T) {
	var buf bytes.Buffer // not an *os.File, so "auto" -> json
	cases := []struct{ in, want string }{
		{"json", "json"},
		{"text", "text"},
		{"auto", "json"}, // non-terminal writer
		{"", "json"},
		{"bogus", ""},
	}
	for _, c := range cases {
		if got := resolveFormat(c.in, &buf); got != c.want {
			t.Errorf("resolveFormat(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"debug", slog.LevelDebug, false},
		{"info", slog.LevelInfo, false},
		{"", slog.LevelInfo, false},
		{"WARN", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"bogus", 0, true},
	}
	for _, c := range cases {
		got, err := parseLevel(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseLevel(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
		}
		if err == nil && got != c.want {
			t.Errorf("parseLevel(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestNewLogger_JSONEmitsStructured(t *testing.T) {
	var buf bytes.Buffer
	logger, err := newLogger(&buf, "json", "info")
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}
	logger.Info("hello", "k", "v")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, buf.String())
	}
	if m["msg"] != "hello" || m["k"] != "v" {
		t.Errorf("unexpected JSON fields: %v", m)
	}
}

func TestNewLogger_InvalidFormatAndLevel(t *testing.T) {
	var buf bytes.Buffer
	if _, err := newLogger(&buf, "bogus", "info"); err == nil {
		t.Error("expected error for invalid format")
	}
	if _, err := newLogger(&buf, "json", "bogus"); err == nil {
		t.Error("expected error for invalid level")
	}
}

func TestNewLogger_LevelFiltersDebug(t *testing.T) {
	var buf bytes.Buffer
	logger, err := newLogger(&buf, "json", "info")
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}
	logger.Debug("suppressed")
	if buf.Len() != 0 {
		t.Errorf("debug line should be filtered at info level, got %q", buf.String())
	}
}

// serveOne drives a single request through requestLogger and returns the
// captured log output.
func serveOne(t *testing.T, level, method, path string, h http.HandlerFunc) string {
	t.Helper()
	var buf bytes.Buffer
	logger, err := newLogger(&buf, "json", level)
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}
	rr := httptest.NewRecorder()
	requestLogger(logger, nil, h).ServeHTTP(rr, httptest.NewRequest(method, path, nil))
	return buf.String()
}

func TestRequestLogger_LogsFields(t *testing.T) {
	out := serveOne(t, "info", http.MethodGet, "/api/v1/scores", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
		t.Fatalf("log line not JSON: %v (%q)", err, out)
	}
	// path is routed through logsafe.Str (#321, go/log-injection), which %q-quotes the
	// value, so the structured field carries the quoted form. r.URL.Path is client-
	// controlled (percent-decoded) and CRLF-injectable, so the sanitizer is mandatory
	// even though the JSON handler would also escape control bytes.
	if m["msg"] != "http request" || m["method"] != "GET" || m["path"] != `"/api/v1/scores"` {
		t.Errorf("unexpected fields: %v", m)
	}
	if m["status"].(float64) != http.StatusTeapot {
		t.Errorf("status = %v, want 418", m["status"])
	}
	if _, ok := m["duration_ms"]; !ok {
		t.Error("missing duration_ms")
	}
}

func TestRequestLogger_ImplicitStatusOK(t *testing.T) {
	// A handler that writes a body without WriteHeader is an implicit 200.
	out := serveOne(t, "info", http.MethodGet, "/api/v1/scores", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
		t.Fatalf("log line not JSON: %v (%q)", err, out)
	}
	if m["status"].(float64) != http.StatusOK {
		t.Errorf("status = %v, want implicit 200", m["status"])
	}
}

func TestRequestLogger_BytesAccumulate(t *testing.T) {
	// Two writes must accumulate (r.bytes += n), not last-write-wins.
	out := serveOne(t, "info", http.MethodGet, "/api/v1/scores", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ab"))
		_, _ = w.Write([]byte("cde"))
	})
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
		t.Fatalf("log line not JSON: %v (%q)", err, out)
	}
	if m["bytes"].(float64) != 5 {
		t.Errorf("bytes = %v, want 5 (2 + 3 accumulated)", m["bytes"])
	}
}

func TestRequestLogger_HealthPathAtDebug(t *testing.T) {
	noop := func(w http.ResponseWriter, _ *http.Request) {}
	// At info level, a health-check path must NOT produce an access-log line.
	if out := serveOne(t, "info", http.MethodGet, "/api/v1/healthz", noop); out != "" {
		t.Errorf("healthz should be silent at info level, got %q", out)
	}
	// A non-health path at info level DOES log.
	if out := serveOne(t, "info", http.MethodGet, "/api/v1/scores", noop); out == "" {
		t.Error("non-health path should log at info level")
	}
	// At debug level, the health path DOES log.
	if out := serveOne(t, "debug", http.MethodGet, "/api/v1/healthz", noop); out == "" {
		t.Error("healthz should log at debug level")
	}
}

func TestRequestLogger_FlusherTransparent(t *testing.T) {
	// The reverse proxy streams SSE via http.ResponseController, which walks
	// Unwrap to the underlying flusher. That path must work through the wrapper.
	var buf bytes.Buffer
	logger, _ := newLogger(&buf, "json", "info")
	var controllerOK bool
	h := requestLogger(logger, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).Flush(); err == nil {
			controllerOK = true
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/anthropic/v1/messages", nil))
	if !controllerOK {
		t.Error("http.ResponseController(w).Flush() must reach the underlying flusher via Unwrap")
	}
}

// TestRequestLogger_PathNotForgeable is a security regression guard (#321, CodeQL
// go/log-injection; sibling of the webhook forge tests and TestProxy_PathNotForgeable).
//
// r.URL.Path is percent-decoded from the client's request target, so a path like
// "/evil%0atime=..." lands in the access log carrying a real newline. Logged raw
// (and under a text handler), that newline forges a second, standalone record an
// operator or a line-oriented SIEM reads as genuine. logsafe.Str must strip it so
// the diagnostic survives on one line and no forged record ever stands alone.
func TestRequestLogger_PathNotForgeable(t *testing.T) {
	const forgedMarker = `level=ERROR msg="tier: auth bypassed"`

	var buf bytes.Buffer
	// A TEXT handler is the adversarial case: unlike JSONHandler it does not escape
	// control bytes on its own, so it exposes whether OUR barrier ran.
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	noop := func(w http.ResponseWriter, _ *http.Request) {}
	req := httptest.NewRequest(http.MethodGet, "/clean", nil)
	// A percent-decoded target ("/evil%0a...") lands in r.URL.Path with a real CR/LF;
	// set it directly so the test does not depend on the target parser's charset rules.
	req.URL.Path = "/evil\ntime=2026-07-12T00:00:00Z " + forgedMarker
	requestLogger(logger, nil, http.HandlerFunc(noop)).
		ServeHTTP(httptest.NewRecorder(), req)

	logs := buf.String()
	// The diagnostic must survive.
	if !strings.Contains(logs, "http request") {
		t.Fatalf("access-log line was not emitted; diagnostic lost:\n%s", logs)
	}
	// The forged record must never begin its own line.
	for _, line := range strings.Split(logs, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), forgedMarker) {
			t.Fatalf("request path forged a standalone access-log record - log injection:\n%s", logs)
		}
	}
	// MECHANISM PIN + STRIP PROOF: the CR/LF is stripped and the value %q-quoted, so
	// the halves join as `path="\"/eviltime=...`. Removing logsafe.Str makes this fail.
	if !strings.Contains(logs, `path="\"/eviltime=`) {
		t.Errorf("path not stripped+rendered via logsafe.Str (expected `path=\"\\\"/eviltime=...`):\n%s", logs)
	}
}

func TestRouteLabel(t *testing.T) {
	cases := map[string]string{
		"GET /api/v1/health":             "/api/v1/health",
		"POST /api/v1/costs":             "/api/v1/costs",
		"GET /api/v1/scores/{developer}": "/api/v1/scores/{developer}",
		"/anthropic/":                    "/anthropic/",
		"/":                              "/",
		"":                               "other",
	}
	for in, want := range cases {
		if got := routeLabel(in); got != want {
			t.Errorf("routeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequestLogger_RecordsMetrics(t *testing.T) {
	reg := metrics.NewRegistry()
	reqs := reg.NewCounter("tier_http_requests_total", "h", "method", "route", "status")
	dur := reg.NewHistogram("tier_http_request_duration_seconds", "h", []float64{1}, "method", "route")
	m := &httpMetrics{requests: reqs, duration: dur}
	logger, _ := newLogger(&bytes.Buffer{}, "json", "error") // suppress access log

	// Route through a real ServeMux so r.Pattern is populated with the template.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/scores/{developer}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := requestLogger(logger, m, mux)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/scores/alice", nil))

	var sb strings.Builder
	reg.Render(&sb)
	out := sb.String()
	// The label must be the TEMPLATE, not the concrete developer "alice".
	if !strings.Contains(out, `tier_http_requests_total{method="GET",route="/api/v1/scores/{developer}",status="2xx"} 1`) {
		t.Errorf("request counter wrong/missing:\n%s", out)
	}
	if !strings.Contains(out, `tier_http_request_duration_seconds_count{method="GET",route="/api/v1/scores/{developer}"} 1`) {
		t.Errorf("duration count wrong/missing:\n%s", out)
	}
	if strings.Contains(out, "alice") {
		t.Errorf("raw developer leaked into a metric label:\n%s", out)
	}
}

func TestRequestLogger_MetricStatusAndRoute(t *testing.T) {
	reg := metrics.NewRegistry()
	reqs := reg.NewCounter("tier_http_requests_total", "h", "method", "route", "status")
	dur := reg.NewHistogram("tier_http_request_duration_seconds", "h", []float64{1}, "method", "route")
	m := &httpMetrics{requests: reqs, duration: dur}
	logger, _ := newLogger(&bytes.Buffer{}, "json", "error")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /bad", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("GET /err", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	h := requestLogger(logger, m, mux)
	for _, p := range []string{"/ok", "/bad", "/err", "/unregistered"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
	}

	var sb strings.Builder
	reg.Render(&sb)
	out := sb.String()
	for _, want := range []string{
		`tier_http_requests_total{method="GET",route="/ok",status="2xx"} 1`,
		`tier_http_requests_total{method="GET",route="/bad",status="4xx"} 1`,
		`tier_http_requests_total{method="GET",route="/err",status="5xx"} 1`,
		// unmatched path: bare mux 404 + empty r.Pattern -> route "other"
		`tier_http_requests_total{method="GET",route="other",status="4xx"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}
