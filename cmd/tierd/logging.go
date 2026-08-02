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
// TTY gets readable output. TTY detection is stdlib-only (os.ModeCharDevice),
// so no go-isatty dependency is pulled into the lean dependency set.
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

// isTerminal reports whether f is a character device (a TTY) using only the
// stdlib. A Stat error (e.g. a closed fd) is treated as "not a terminal", which
// safely selects JSON.
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
// Unwrap keeps the wrapper transparent to http.ResponseController — which the
// reverse proxy's SSE capture uses to Flush (and would use to Hijack on a
// protocol upgrade). The controller walks Unwrap to the real *http.response and
// reaches its FlushError, so flush errors are preserved. We deliberately do NOT
// implement Flush() ourselves: a plain Flush() would shadow the underlying
// FlushError and silently downgrade it to an error-discarding flush.
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
	r.status = code
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

// Unwrap exposes the underlying writer so http.ResponseController can reach its
// Flusher/Hijacker/FlushError/etc. — see the type doc for why we rely on this
// rather than re-implementing Flush.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// quietPaths are logged at Debug so routine liveness/readiness/scrape traffic
// doesn't flood the access log at the default Info level.
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
// API, webhook, dashboard, and proxy routes. Liveness/readiness/scrape paths
// log at Debug to avoid spam; everything else at Info.
func requestLogger(logger *slog.Logger, m *httpMetrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK // handler returned without writing
		}
		elapsed := time.Since(start)

		level := slog.LevelInfo
		if quietPaths[r.URL.Path] {
			level = slog.LevelDebug
		}
		logger.Log(r.Context(), level, "http request",
			"method", r.Method,
			// r.URL.Path is percent-decoded from the client's request target, so it
			// can carry embedded CR/LF and is CRLF-injectable. Route it through the
			// shared logsafe barrier — which strips CR/LF, the CodeQL-recognized
			// sanitizer — so a forged newline cannot spawn a standalone access-log
			// record (#321, go/log-injection). The value renders quoted as a result.
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
			m.requests.Inc(r.Method, route, statusClass)
			m.duration.Observe(elapsed.Seconds(), r.Method, route)
		}
	})
}

// routeLabel reduces a ServeMux pattern to a low-cardinality route label: the
// path template with any leading method/host token stripped (it's "GET /path"
// for the method-scoped API routes but bare "/path" for the proxy/dashboard
// routes — normalise to the path since method is already its own label).
//
// "other" is a defensive fallback for an empty pattern (no match). In the wired
// server it is effectively unreachable because the dashboard mounts a "/"
// catch-all, so every unmatched request resolves to pattern "/". It exists so
// that, if that catch-all is ever removed, a flood of unmatched paths still
// collapses to a single bounded series rather than exploding cardinality.
func routeLabel(pattern string) string {
	if pattern == "" {
		return "other"
	}
	if i := strings.LastIndexByte(pattern, ' '); i >= 0 {
		return pattern[i+1:]
	}
	return pattern
}
