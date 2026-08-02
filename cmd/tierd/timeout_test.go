package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseStreamHandler writes `frames` `data: ping` SSE frames spaced `interval`
// apart, flushing after each so bytes reach the client as they are produced.
// The stream itself is the test clock: no test needs a sleep to synchronise.
func sseStreamHandler(frames int, interval time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for i := 0; i < frames; i++ {
			if i > 0 {
				select {
				case <-ticker.C:
				case <-r.Context().Done():
					return
				}
			}
			if _, err := io.WriteString(w, "data: ping\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	})
}

// startStreamServer runs h on a real *http.Server (not httptest, which does not
// arm WriteTimeout) bound to an ephemeral loopback port, and returns its base
// URL. WriteTimeout arms an absolute per-response deadline, exactly the
// behaviour under test.
func startStreamServer(t *testing.T, h http.Handler, writeTimeout time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h, WriteTimeout: writeTimeout}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return "http://" + ln.Addr().String()
}

// readFrames GETs url, drains the body to EOF, and reports how many `data: ping`
// frames arrived plus any read error (a server-side WriteTimeout severs the
// connection mid-body, surfacing as a read error and/or a truncated frame count).
func readFrames(t *testing.T, url string) (int, error) {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // loopback test client
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return strings.Count(string(body), "data: ping"), err
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

const (
	streamFrames   = 8
	streamInterval = 100 * time.Millisecond // ~800ms total stream
	cutWriteTO     = 200 * time.Millisecond // bites well before the stream ends
)

// TestServerWriteTimeout_StillCutsUnwrappedRoutes is the control: the same
// stream mounted WITHOUT noWriteTimeout must be severed by the server's
// WriteTimeout. It pins that the harness actually exercises WriteTimeout (so a
// silent-pass rewrite of the middleware test would be caught) and that normal
// routes keep the deadline.
func TestServerWriteTimeout_StillCutsUnwrappedRoutes(t *testing.T) {
	t.Parallel()
	url := startStreamServer(t, sseStreamHandler(streamFrames, streamInterval), cutWriteTO)
	n, err := readFrames(t, url)
	if err == nil && n >= streamFrames {
		t.Fatalf("expected WriteTimeout to cut the stream, got %d frames, err=%v", n, err)
	}
}

// TestNoWriteTimeout_StreamSurvivesServerWriteTimeout is the fix: wrapping the
// stream in noWriteTimeout clears the per-response deadline so all frames
// arrive even though the server's WriteTimeout is far shorter than the stream.
func TestNoWriteTimeout_StreamSurvivesServerWriteTimeout(t *testing.T) {
	t.Parallel()
	h := noWriteTimeout(discardLogger(), sseStreamHandler(streamFrames, streamInterval))
	url := startStreamServer(t, h, cutWriteTO)
	n, err := readFrames(t, url)
	if err != nil {
		t.Fatalf("stream read error: %v", err)
	}
	if n != streamFrames {
		t.Fatalf("got %d frames, want %d", n, streamFrames)
	}
}

// TestNoWriteTimeout_UnwrapChainReached pins that SetWriteDeadline penetrates
// the statusRecorder wrapper via Unwrap: the stream is mounted behind the
// production requestLogger composition (which wraps every response in a
// statusRecorder) plus noWriteTimeout, and all frames must still arrive. A
// future response wrapper without an Unwrap() would regress this.
func TestNoWriteTimeout_UnwrapChainReached(t *testing.T) {
	t.Parallel()
	logger := discardLogger()
	mux := http.NewServeMux()
	mux.Handle("/", noWriteTimeout(logger, sseStreamHandler(streamFrames, streamInterval)))
	h := requestLogger(logger, nil, mux)
	url := startStreamServer(t, h, cutWriteTO)
	n, err := readFrames(t, url)
	if err != nil {
		t.Fatalf("stream read error: %v", err)
	}
	if n != streamFrames {
		t.Fatalf("got %d frames, want %d", n, streamFrames)
	}
}

// TestNoWriteTimeout_SetDeadlineErrorFailsOpenAndWarns pins the fail-open
// posture: when SetWriteDeadline is unsupported (httptest.ResponseRecorder has
// no SetWriteDeadline and no Unwrap, so ResponseController returns
// ErrNotSupported), the request must still be served and a WARN must be logged
// — a regression to fail-closed, or a dropped warning, would be caught here.
func TestNoWriteTimeout_SetDeadlineErrorFailsOpenAndWarns(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	served := false
	h := noWriteTimeout(logger, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/anthropic/v1/messages", nil))
	if !served {
		t.Fatal("request was not served: fail-open posture violated")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(buf.String(), "clearing write deadline failed") {
		t.Fatalf("expected WARN log about clearing write deadline, got: %q", buf.String())
	}
}

// TestRegisterProxy_ExemptsRouteFromWriteTimeout guards the wiring: a stream
// mounted through registerProxy (the seam main.go uses for both proxy routes)
// survives a server WriteTimeout far shorter than the stream. Removing the
// noWriteTimeout wrap from registerProxy would fail this. The route is mounted
// behind the production requestLogger/statusRecorder composition and a
// pass-through auth (the tokenless ProxyAuth shape), and the prefix is stripped
// exactly as in production.
func TestRegisterProxy_ExemptsRouteFromWriteTimeout(t *testing.T) {
	t.Parallel()
	logger := discardLogger()
	mux := http.NewServeMux()
	passthrough := proxyAuthFunc(func(next http.Handler) http.Handler { return next })
	registerProxy(mux, "/anthropic", sseStreamHandler(streamFrames, streamInterval), passthrough, logger)
	url := startStreamServer(t, requestLogger(logger, nil, mux), cutWriteTO)
	n, err := readFrames(t, url+"/anthropic/v1/messages")
	if err != nil {
		t.Fatalf("stream read error: %v", err)
	}
	if n != streamFrames {
		t.Fatalf("got %d frames, want %d", n, streamFrames)
	}
}
