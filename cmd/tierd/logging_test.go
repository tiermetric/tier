package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestResolveFormat_CharacterDevice(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for _, format := range []string{"auto", ""} {
		if got := resolveFormat(format, f); got != "text" {
			t.Errorf("resolveFormat(%q, character device) = %q, want text", format, got)
		}
	}
}

// httptest.ResponseRecorder treats informational headers as final responses.
type finalResponseRecorder struct{ *httptest.ResponseRecorder }

func (r finalResponseRecorder) WriteHeader(code int) {
	if code == http.StatusSwitchingProtocols || code >= 200 {
		r.ResponseRecorder.WriteHeader(code)
	}
}

func TestRequestLogger_FirstFinalStatusAndPanic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []int
		body    string
		panics  any
		want    int
	}{
		{"informational_then_final", []int{103, 200}, "", nil, 200},
		{"informational_then_body", []int{103}, "ok", nil, 200},
		{"informational_only", []int{103}, "", nil, 200},
		{"first_final", []int{201, 500}, "", nil, 201},
		{"switching_protocols", []int{101}, "", nil, 101},
		{"switching_protocols_first_final", []int{103, 101, 500}, "", nil, 101},
		{"panic_after_final", []int{418}, "tea", "handler panic", 418},
		{"panic_before_write", nil, "", "handler panic", 500},
		{"panic_after_informational", []int{103}, "", "handler panic", 500},
		{"abort_after_final", []int{202}, "partial", http.ErrAbortHandler, 202},
		{"abort_after_body", nil, "partial", http.ErrAbortHandler, 200},
		{"abort_before_write", nil, "", http.ErrAbortHandler, 200},
		{"abort_after_informational", []int{103}, "", http.ErrAbortHandler, 200},
		{"wrapped_abort_is_panic", nil, "", fmt.Errorf("wrapped: %w", http.ErrAbortHandler), 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger, _ := newLogger(&buf, "json", "info")
			reg := metrics.NewRegistry()
			m := &httpMetrics{
				requests: reg.NewCounter("tier_http_requests_total", "h", "method", "route", "status"),
				duration: reg.NewHistogram("tier_http_request_duration_seconds", "h", []float64{1}, "method", "route"),
			}
			mux := http.NewServeMux()
			mux.HandleFunc("GET /test", func(w http.ResponseWriter, _ *http.Request) {
				for _, code := range tc.headers {
					w.WriteHeader(code)
				}
				if tc.body != "" {
					_, _ = w.Write([]byte(tc.body))
				}
				if tc.panics != nil {
					panic(tc.panics)
				}
			})
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				requestLogger(logger, m, mux).ServeHTTP(finalResponseRecorder{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/test", nil))
			}()
			if recovered != tc.panics {
				t.Errorf("recovered = %v, panics = %v", recovered, tc.panics)
			}
			var fields map[string]any
			if err := json.Unmarshal(buf.Bytes(), &fields); err != nil {
				t.Errorf("expected one JSON access line: %v (%q)", err, buf.String())
			} else if fields["status"] != float64(tc.want) || fields["bytes"] != float64(len(tc.body)) {
				t.Errorf("log fields = %v, want status %d and bytes %d", fields, tc.want, len(tc.body))
			}
			if tc.panics == http.ErrAbortHandler {
				if fields["aborted"] != true || fields["panicked"] != nil || fields["level"] != "WARN" {
					t.Errorf("abort log fields = %v, want aborted=true, no panicked, and level=WARN", fields)
				}
			} else if tc.panics != nil && (fields["panicked"] != true || fields["level"] != "ERROR" || fields["aborted"] != nil) {
				t.Errorf("panic log fields = %v, want panicked=true and level=ERROR", fields)
			}
			var sb strings.Builder
			reg.Render(&sb)
			for _, want := range []string{
				fmt.Sprintf(`tier_http_requests_total{method="GET",route="/test",status="%dxx"} 1`, tc.want/100),
				`tier_http_request_duration_seconds_count{method="GET",route="/test"} 1`,
			} {
				if !strings.Contains(sb.String(), want) {
					t.Errorf("missing metric %q:\n%s", want, sb.String())
				}
			}
		})
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

func TestRequestLogger_QuietPathStatusLevel(t *testing.T) {
	for _, path := range []string{"/api/v1/health", "/api/v1/healthz", "/api/v1/livez", "/metrics"} {
		for _, status := range []int{200, 302, 404, 503} {
			t.Run(fmt.Sprintf("%s/%d", path, status), func(t *testing.T) {
				out := serveOne(t, "debug", http.MethodGet, path, func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(status)
				})
				var fields map[string]any
				if err := json.Unmarshal([]byte(out), &fields); err != nil {
					t.Fatal(err)
				}
				want := "DEBUG"
				if status >= 400 {
					want = "INFO"
				}
				if fields["level"] != want {
					t.Errorf("level = %v, want %s for status %d", fields["level"], want, status)
				}
			})
		}
	}
}

func TestRequestLogger_FlusherTransparent(t *testing.T) {
	// The reverse proxy streams SSE via http.ResponseController.
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
		t.Error("http.ResponseController(w).Flush() must reach the underlying flusher")
	}
}

func TestRequestLogger_FlushRecordsImplicitOK(t *testing.T) {
	for _, direct := range []bool{false, true} {
		for _, panics := range []bool{false, true} {
			t.Run(fmt.Sprintf("direct=%v/panic=%v", direct, panics), func(t *testing.T) {
				var buf bytes.Buffer
				logger, _ := newLogger(&buf, "json", "info")
				rr := httptest.NewRecorder()
				h := requestLogger(logger, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if direct {
						flusher, ok := w.(http.Flusher)
						if !ok {
							t.Fatal("writer does not implement http.Flusher")
						}
						flusher.Flush()
					} else if err := http.NewResponseController(w).Flush(); err != nil {
						t.Fatal(err)
					}
					if panics {
						panic("handler panic")
					}
					w.WriteHeader(http.StatusInternalServerError)
				}))
				var recovered any
				func() {
					defer func() { recovered = recover() }()
					h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/test", nil))
				}()
				if (panics && recovered != "handler panic") || (!panics && recovered != nil) {
					t.Errorf("recovered = %v, panics = %v", recovered, panics)
				}
				if !rr.Flushed || rr.Code != http.StatusOK {
					t.Errorf("underlying writer: flushed=%v, status=%d, want true, 200", rr.Flushed, rr.Code)
				}
				var fields map[string]any
				if err := json.Unmarshal(buf.Bytes(), &fields); err != nil {
					t.Fatal(err)
				}
				if fields["status"] != float64(http.StatusOK) {
					t.Errorf("logged status = %v, want 200", fields["status"])
				}
				if panics && (fields["panicked"] != true || fields["level"] != "ERROR") {
					t.Errorf("panic log fields = %v, want panicked=true and level=ERROR", fields)
				}
			})
		}
	}
}

type flushErrorRecorder struct {
	*httptest.ResponseRecorder
	err error
}

func (r *flushErrorRecorder) FlushError() error {
	r.Flush()
	return r.err
}

func TestStatusRecorder_FlushError(t *testing.T) {
	wantErr := errors.New("flush failed")
	for _, status := range []int{0, http.StatusCreated} {
		t.Run(fmt.Sprintf("status=%d", status), func(t *testing.T) {
			rr := &flushErrorRecorder{ResponseRecorder: httptest.NewRecorder(), err: wantErr}
			rec := &statusRecorder{ResponseWriter: rr}
			wantStatus := http.StatusOK
			if status != 0 {
				rec.WriteHeader(status)
				wantStatus = status
			}
			if err := http.NewResponseController(rec).Flush(); err != wantErr {
				t.Errorf("Flush() error = %v, want %v", err, wantErr)
			}
			if !rr.Flushed || rr.Code != wantStatus || rec.status != wantStatus {
				t.Errorf("flushed=%v, underlying status=%d, recorded status=%d, want true, %d, %d", rr.Flushed, rr.Code, rec.status, wantStatus, wantStatus)
			}
		})
	}
}

// TestRequestLogger_PathNotForgeable is a security regression guard (#321, CodeQL
// go/log-injection; sibling of the webhook forge tests and TestProxy_PathNotForgeable).
//
// r.URL.Path is percent-decoded from the client's request target, so a path like
// "/evil%0atime=..." contains a real newline. Both slog handlers escape control
// bytes; logsafe.Str strips CR/LF as defence in depth and provides the
// CodeQL-recognized sanitizer.
func TestRequestLogger_PathNotForgeable(t *testing.T) {
	const forgedMarker = `level=ERROR msg="tier: auth bypassed"`

	var buf bytes.Buffer
	// TextHandler also escapes control bytes; the assertions below additionally
	// pin logsafe's stripping and quoting.
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
		"GET example.com/path":           "example.com/path",
		"example.com/path":               "example.com/path",
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

func TestRequestLogger_MetricLabelsBounded(t *testing.T) {
	deny := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux := http.NewServeMux()
	mux.Handle("/items/{id}", deny)
	for _, tc := range []struct {
		name, path, route, status string
		handler                   http.Handler
	}{
		{"matched", "/items/", "/items/{id}", "4xx", mux},
		{"unmatched", "/missing/", "other", "4xx", mux},
		{"before_mux", "/items/", "other", "4xx", deny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := metrics.NewRegistry()
			m := &httpMetrics{
				requests: reg.NewCounter("tier_http_requests_total", "h", "method", "route", "status"),
				duration: reg.NewHistogram("tier_http_request_duration_seconds", "h", []float64{1}, "method", "route"),
			}
			logger, _ := newLogger(&bytes.Buffer{}, "json", "error")
			h := requestLogger(logger, m, tc.handler)
			methods := []string{
				http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
				http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace,
			}
			for i := 0; i < 1000; i++ {
				methods = append(methods, fmt.Sprintf("INVENTED%d", i))
			}
			for i, method := range methods {
				req := httptest.NewRequest(method, fmt.Sprintf("%s%d?q=%d", tc.path, i, i), nil)
				req.Host = fmt.Sprintf("host%d.example", i)
				h.ServeHTTP(httptest.NewRecorder(), req)
			}
			var sb strings.Builder
			reg.Render(&sb)
			out := sb.String()
			for _, metric := range []string{"tier_http_requests_total", "tier_http_request_duration_seconds_count"} {
				if got := strings.Count(out, metric+"{"); got != 10 {
					t.Errorf("%s series = %d, want 10 (nine standard methods + OTHER)", metric, got)
				}
				for _, method := range append(methods[:9:9], "OTHER") {
					labels := fmt.Sprintf(`method=%q,route=%q`, method, tc.route)
					if metric == "tier_http_requests_total" {
						labels += fmt.Sprintf(`,status=%q`, tc.status)
					}
					count := 1
					if method == "OTHER" {
						count = 1000
					}
					want := fmt.Sprintf("%s{%s} %d\n", metric, labels, count)
					if !strings.Contains(out, want) {
						t.Errorf("missing metric %q", want)
					}
				}
			}
		})
	}
}
