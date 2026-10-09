package docs

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// get drives the handler for one path and returns the response plus its body. It
// uses the real *Handler (no fakes) so the tests exercise the embedded pages and
// headers exactly as production would serve them — mirroring internal/dashboard.
func get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	New().ServeHTTP(rec, req)
	res := rec.Result()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = res.Body.Close()
	return res, string(body)
}

// assertServedHTML asserts a 200 HTML response carrying the docs CSP and nosniff.
func assertServedHTML(t *testing.T, res *http.Response) {
	t.Helper()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html...", ct)
	}
	if nosniff := res.Header.Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", nosniff)
	}
	csp := res.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy header")
	}
	// The docs must never be able to execute JavaScript: there is no script-src
	// directive, so scripts fall through to default-src 'none'. Guard BOTH halves —
	// default-src 'none' present AND script-src absent — so a later edit that adds a
	// script-src (even 'self') is caught.
	if !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q, want default-src 'none'", csp)
	}
	if strings.Contains(csp, "script-src") {
		t.Errorf("CSP = %q, must NOT contain a script-src directive (scripts must fall to default-src 'none')", csp)
	}
	// The layout carries an inline <style>, so style-src must keep 'unsafe-inline'.
	if !strings.Contains(csp, "style-src 'unsafe-inline'") {
		t.Errorf("CSP = %q, want style-src 'unsafe-inline'", csp)
	}
}

func TestDocs_Serve(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantHTML   bool   // expect a 200 HTML page with the security headers
		wantBody   string // substring the body must contain (200 cases only)
	}{
		{name: "index bare slash", path: "/docs/", wantStatus: http.StatusOK, wantHTML: true, wantBody: "TIER"},
		{name: "index no trailing slash", path: "/docs", wantStatus: http.StatusOK, wantHTML: true, wantBody: "TIER"},
		{name: "known page README", path: "/docs/README.html", wantStatus: http.StatusOK, wantHTML: true, wantBody: "<!doctype html>"},
		{name: "known page rubric", path: "/docs/rubric.html", wantStatus: http.StatusOK, wantHTML: true, wantBody: "<h1"},
		{name: "unknown page", path: "/docs/does-not-exist.html", wantStatus: http.StatusNotFound},
		{name: "non-html asset", path: "/docs/README.md", wantStatus: http.StatusNotFound},
		{name: "nested path rejected", path: "/docs/sub/README.html", wantStatus: http.StatusNotFound},
		{name: "traversal attempt rejected", path: "/docs/..%2fsecret", wantStatus: http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, body := get(t, tc.path)
			if tc.wantHTML {
				assertServedHTML(t, res)
				if tc.wantBody != "" && !strings.Contains(body, tc.wantBody) {
					t.Errorf("body of %q missing %q", tc.path, tc.wantBody)
				}
				return
			}
			if res.StatusCode != tc.wantStatus {
				t.Errorf("status for %q = %d, want %d", tc.path, res.StatusCode, tc.wantStatus)
			}
		})
	}
}

// TestDocs_NoLiveScriptInServedPages is the served-side backstop for the docgen
// XSS posture: no committed page may contain a live <script> tag. tools/docgen
// renders with raw-HTML off and a bluemonday pass, and the layout adds none, but
// this pins the invariant at the serving boundary too (defence in depth).
func TestDocs_NoLiveScriptInServedPages(t *testing.T) {
	_, body := get(t, "/docs/README.html")
	if strings.Contains(strings.ToLower(body), "<script") {
		t.Error("served docs page contains a live <script> tag")
	}
}
