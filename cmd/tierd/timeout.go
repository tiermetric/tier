package main

import (
	"log/slog"
	"net/http"
	"time"
)

// noWriteTimeout clears the server WriteTimeout for streaming proxy routes.
//
// The one http.Server that hosts the API, dashboard, webhook, and reverse
// proxies sets WriteTimeout: 60s, which arms an absolute per-response deadline
// at request-read time — not a per-write idle timeout. Proxied SSE generations
// (Claude Code, Codex, Gemini CLI all stream by default) legitimately run for
// minutes, so a fixed response deadline kills long streams mid-flight and
// finalizes their token event with a truncated count. (Streaming is the point
// here, not capture: of those three only Claude Code is captured today — see
// the proxy package doc.)
//
// http.NewResponseController(w).SetWriteDeadline(time.Time{}) clears the
// deadline for this request only (net/http re-arms WriteTimeout at the start of
// each request on a kept-alive connection, so a later request on the same
// connection still gets the 60s cap); it walks the response writer's Unwrap()
// chain (statusRecorder cooperates) to reach the real connection.
//
// Bounds after clearing: a client that disconnects tears the stream down via
// the request context, and the reverse proxy's transport still bounds the
// upstream leg. What is NOT bounded on these routes is a client that stalls its
// read without closing the socket — no write deadline reaps it. That is an
// accepted trade-off for a single-tenant, typically-loopback developer proxy;
// if it is ever exposed to untrusted clients, the stricter fix is a rolling
// per-flush write deadline in internal/proxy (its own issue), not a fixed cap
// here that would re-truncate legitimate long streams.
func noWriteTimeout(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
			// Fail loud in logs, then continue: a proxy that dies at 60s is
			// still better than refusing the request outright.
			logger.Warn("proxy: clearing write deadline failed; streams >60s may be cut", "err", err)
		}
		next.ServeHTTP(w, r)
	})
}

// proxyAuthFunc is the ProxyAuth middleware shape (satisfied by the method value
// (*api.Handler).ProxyAuth) — declared so registerProxy can be exercised in a
// unit test without constructing a full api.Handler.
type proxyAuthFunc func(http.Handler) http.Handler

// registerProxy mounts reverse-proxy handler h under prefix (e.g. "/anthropic")
// on mux, gated by auth and exempted from the server WriteTimeout via
// noWriteTimeout so long SSE streams survive (#116). prefix is stripped before h
// sees the request. noWriteTimeout sits INSIDE auth so an unauthenticated
// request is rejected before the deadline is cleared and keeps the 60s cap.
// Centralizing the wrap here keeps the two provider mounts from drifting and
// gives the exemption a single unit-testable seam.
func registerProxy(mux *http.ServeMux, prefix string, h http.Handler, auth proxyAuthFunc, logger *slog.Logger) {
	mux.Handle(prefix+"/", auth(noWriteTimeout(logger, http.StripPrefix(prefix, h))))
}
