// Package dashboard serves the embedded HTML dashboard and its client script.
package dashboard

import (
	_ "embed"
	"net/http"
)

// cspHeader is the Content-Security-Policy sent with every dashboard response
// (#59, hardened in #145).
//
// The page keeps the API token in sessionStorage, so this header is the last
// line of defence against token exfiltration if an HTML-injection bug ever
// regresses. The application script is now served as a separate embedded asset
// (/dashboard.js) rather than an inline <script>, so script-src can be 'self':
// an injected inline <script> is structurally blocked (there is no
// 'unsafe-inline' for scripts) and is dead on arrival — it cannot read the
// token. style-src keeps 'unsafe-inline' for the embedded <style> block; this
// change hardens script-src only, per finding E-Y5.
const cspHeader = "default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'"

const (
	// jsPath is the route (and the src referenced by index.html) for the
	// embedded client script.
	jsPath = "/dashboard.js"

	contentTypeHTML = "text/html; charset=utf-8"
	contentTypeJS   = "text/javascript; charset=utf-8"
)

// SECURITY INVARIANTS for indexHTML and dashboardJS (not style preferences —
// the API token lives in sessionStorage on this page):
//   - All user-supplied values are inserted via textContent (never
//     innerHTML), so no injected markup can read the token.
//   - The token travels ONLY in the Authorization header. Never add a
//     ?token= query-param fallback: query strings land in server logs,
//     browser history, and Referer headers.
//
// dashboardJS carries a header-comment pointer back to this block, because it
// is edited by people who never open this file.
//
//go:embed assets/index.html
var indexHTML []byte

//go:embed assets/dashboard.js
var dashboardJS []byte

// Handler serves the embedded HTML dashboard and its client script.
type Handler struct{}

// New returns a new dashboard Handler.
func New() *Handler { return &Handler{} }

// ServeHTTP implements http.Handler. It serves the dashboard page at "/" and
// the client script at "/dashboard.js"; every other path is a 404 (unchanged
// from the pre-#145 single-page handler). Both served responses carry
// cspHeader (so a direct navigation to the script asset also gets a locked-down
// context) and X-Content-Type-Options: nosniff — the HTML response is the one
// that actually holds the token in sessionStorage, so defence-in-depth against
// MIME confusion applies there too, not only on the executable asset.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/", "":
		w.Header().Set("Content-Security-Policy", cspHeader)
		w.Header().Set("Content-Type", contentTypeHTML)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(indexHTML)
	case jsPath:
		w.Header().Set("Content-Security-Policy", cspHeader)
		w.Header().Set("Content-Type", contentTypeJS)
		// nosniff so a browser can't be tricked into treating the script as a
		// different, executable-in-another-context MIME type.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(dashboardJS)
	default:
		http.NotFound(w, r)
	}
}
