package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/metrics"
)

// newLogger builds the process logger for `tierd serve` (#67). format is one of
// "auto", "json", or "text"; level is debug|info|warn|error. w is normally
// os.Stderr.
//
// "auto" emits JSON when w is not a terminal (a redirected/piped stream, i.e.
// production) and human-readable text when it is — so a container or systemd
// service gets structured logs without configuration, while a developer at a
// TTY gets readable output. Terminal detection uses the character-device
// approximation documented in isTerminal.
func newLogger(w io.Writer, format, level string) (*slog.Logger, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch resolveFormat(format, w) {
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("invalid --log-format %q (want auto|json|text)", format)
	}
}

// resolveFormat collapses "auto" to "json" or "text" based on whether w is a
// terminal. An explicit "json"/"text" passes through; anything else returns ""
// so newLogger reports the error.
func resolveFormat(format string, w io.Writer) string {
	switch format {
	case "json", "text":
		return format
	case "auto", "":
		if f, ok := w.(*os.File); ok && isTerminal(f) {
			return "text"
		}
		return "json"
	default:
		return ""
	}
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid --log-level %q (want debug|info|warn|error)", s)
	}
}

// isTerminal approximates a terminal with a character-device check. The stdlib
// has no portable TTY check and golang.org/x/term is not a dependency, so this
// also selects text for non-TTY character devices such as /dev/null. A Stat
// error (e.g. a closed fd) selects JSON.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// statusRecorder wraps http.ResponseWriter so requestLogger can report the
// response status and byte count, which are otherwise unobservable after the
// handler writes.
//
// FlushError records the implicit status while preserving underlying flush
// errors. Unwrap keeps other http.ResponseController operations transparent.
//
// io.ReaderFrom (sendfile) is intentionally NOT forwarded — routing the
// dashboard's static bytes through Write is what keeps the byte count accurate,
// and the page is tiny so the lost zero-copy path is irrelevant.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 && (code == http.StatusSwitchingProtocols || code >= 200) {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK // implicit 200 on first write without WriteHeader
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *statusRecorder) Flush() {
	_ = r.FlushError()
}

func (r *statusRecorder) FlushError() error {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return http.NewResponseController(r.ResponseWriter).Flush()
}

// Unwrap exposes the underlying writer for other http.ResponseController operations.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// quietPaths log 2xx/3xx responses at Debug so routine liveness/readiness/scrape
// traffic doesn't flood the access log at the default Info level.
var quietPaths = map[string]bool{
	"/api/v1/health":  true,
	"/api/v1/healthz": true,
	"/api/v1/livez":   true,
	"/metrics":        true,
}

// httpMetrics is the subset of the metrics registry requestLogger records into
// (#67). Nil disables metric recording, so the middleware works with or without
// metrics configured.
type httpMetrics struct {
	requests *metrics.CounterVec   // labels: method, route, status (class)
	duration *metrics.HistogramVec // labels: method, route
}

// requestLogger logs one structured line per HTTP request (method, path,
// status, duration, response bytes, remote addr) and, when m != nil, records
// request-count and duration metrics. It wraps the whole mux, so it covers the
// API, webhook, dashboard, and proxy routes. Liveness/readiness/scrape 2xx/3xx
// responses log at Debug; other responses at Info, aborts at Warn, panics at Error.
// On a panic, the defer logs and records metrics before the panic propagates.
func requestLogger(logger *slog.Logger, m *httpMetrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		completed := false
		defer func() {
			panicValue := recover()
			aborted := panicValue == http.ErrAbortHandler
			if rec.status == 0 {
				rec.status = http.StatusOK // no final status written
				if !completed && !aborted {
					rec.status = http.StatusInternalServerError
				}
			}
			elapsed := time.Since(start)

			level := slog.LevelInfo
			if quietPaths[r.URL.Path] && rec.status >= 200 && rec.status < 400 {
				level = slog.LevelDebug
			}
			requestLog := logger
			if aborted {
				level = slog.LevelWarn
				requestLog = logger.With("aborted", true)
			} else if !completed {
				level = slog.LevelError
				requestLog = logger.With("panicked", true)
			}
			requestLog.Log(r.Context(), level, "http request",
				"method", r.Method,
				// Both slog handlers escape control bytes. logsafe additionally strips
				// CR/LF from the percent-decoded path as defence in depth and a
				// CodeQL-recognized sanitizer (#321, go/log-injection).
				"path", logsafe.Str(r.URL.Path),
				"status", rec.status,
				"duration_ms", elapsed.Milliseconds(),
				"bytes", rec.bytes,
				"remote", r.RemoteAddr,
			)

			if m != nil {
				// Label by the matched ROUTE PATTERN, not the raw path, to keep
				// label cardinality bounded (e.g. /api/v1/scores/{developer}, not
				// one series per developer). r.Pattern is set by the ServeMux during
				// the wrapped ServeHTTP.
				route := routeLabel(r.Pattern)
				statusClass := strconv.Itoa(rec.status/100) + "xx"
				method := r.Method
				switch method {
				case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
					http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
				default:
					method = "OTHER"
				}
				m.requests.Inc(method, route, statusClass)
				m.duration.Observe(elapsed.Seconds(), method, route)
			}
			if panicValue != nil {
				panic(panicValue)
			}
		}()
		next.ServeHTTP(rec, r)
		completed = true
	})
}

// routeLabel reduces a ServeMux pattern to a low-cardinality route label: the
// pattern with any leading space-separated method stripped (it's "GET /path"
// for the method-scoped API routes but bare "/path" for the proxy/dashboard
// routes — normalise to the path since method is already its own label).
//
// "other" is the label for an empty pattern. In the wired server that is a
// request browserGuard refused before the mux ran (#893); an unmatched path
// never gets there, because the dashboard mounts a "/" catch-all. It also keeps
// a flood of unmatched paths to a single bounded series if that catch-all is
// ever removed.
func routeLabel(pattern string) string {
	if pattern == "" {
		return "other"
	}
	if i := strings.LastIndexByte(pattern, ' '); i >= 0 {
		return pattern[i+1:]
	}
	return pattern
}
