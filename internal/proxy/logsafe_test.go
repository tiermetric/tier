package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/collector"
)

// TestProxy_ContentEncodingNotForgeable pins the #321 barrier on the ONE
// upstream-controlled free-form string this package logs.
//
// docs/security.md §8 licenses exactly five classes of bare log field (issue
// refs, hex SHAs, the webhook event allowlist, validated periods, numerics).
// Content-Encoding is in none of them, so leaving it bare was a gap rather than
// a style choice. Note the X-Tier-Repo reject path further down modifyResponse
// is NOT the precedent for this fix: it drops the value entirely and logs only
// its length. Sanitize-and-keep is the right answer here only because the codec
// name is the diagnostic an operator needs; see that call site for the contrast.
//
// The two subtests drive two deliberately different reachability stories: the
// flood is reachable through a real HTTP hop; the CR/LF is not.
//
// ⚠️ DELIBERATELY NOT t.Parallel(), unlike its neighbours in this package.
// httptest.Server.Close() calls http.DefaultTransport.CloseIdleConnections()
// on the PROCESS-GLOBAL pool, not just its own client's
// (net/http/httptest/server.go: "assume most users ... will be using the
// standard transport, so help them out"). Every test here talks to its server
// through the shared http.DefaultClient, so one test's Close can break a
// connection another test is reusing — observed once as
// "transport connection broken: http: CloseIdleConnections called" in
// TestProxy_EmptyJSONBodyDoesNotCount during a loaded `go test ./...` run, and
// NOT reproducible in 75 targeted runs with or without this file. Running
// sequentially puts these two servers' Close calls in the sequential phase,
// before the parallel batch resumes, so this test cannot add to that hazard.
// The pre-existing exposure across the package's other ~30 tests is untouched
// and wants its own issue.
func TestProxy_ContentEncodingNotForgeable(t *testing.T) {
	// Reachable end-to-end: net/http's header parser bounds a value's BYTES only
	// by the whole-header limit, so a hostile or broken upstream can push
	// kilobytes into one log line. Driven through a real upstream and a real
	// proxy hop, so this exercises the sink as deployed.
	t.Run("oversized value is capped through a real hop", func(t *testing.T) {
		flood := strings.Repeat("z", 4096)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", flood)
			_, _ = w.Write([]byte{0x00, 0x01})
		}))
		defer upstream.Close()

		var buf bytes.Buffer
		target, _ := url.Parse(upstream.URL)
		p := New(target, ProviderAnthropic, collector.SourceProxy, newMemSink(), nil, nil,
			slog.New(slog.NewTextHandler(&buf, nil)))
		proxySrv := httptest.NewServer(p)
		defer proxySrv.Close()

		req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		got := buf.String()
		if !strings.Contains(got, "unhandled Content-Encoding") {
			t.Fatalf("the guard never fired, so this test proves nothing about its sink; log=%q", got)
		}
		if strings.Contains(got, flood) {
			t.Errorf("the whole %d-byte upstream header value reached the log unbounded; "+
				"record was %d bytes: %.200q...", len(flood), len(got), got)
		}
		if !strings.Contains(got, "...(truncated)") {
			t.Errorf("expected logsafe's truncation marker in the record, got: %q", got)
		}
	})

	// NOT reachable through net/http — the server's header writer maps CR/LF to
	// spaces and the client's parser cannot produce a value spanning lines — so
	// this drives modifyResponse directly. It is a barrier test, not a
	// vulnerability repro: it pins that the field stays single-line for any other
	// producer of this *http.Response (a cache, a test double, a non-net/http
	// transport), which is the property CodeQL's go/log-injection asks for.
	t.Run("CRLF cannot open a second record", func(t *testing.T) {
		var buf bytes.Buffer
		target, _ := url.Parse("http://upstream.invalid")
		p := New(target, ProviderAnthropic, collector.SourceProxy, newMemSink(), nil, nil,
			slog.New(slog.NewTextHandler(&buf, nil)))

		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("{}")),
			// Non-nil so that if the Content-Encoding guard ever stops returning
			// early, this fails as an assertion rather than nil-derefing on
			// resp.Request.Context() further down modifyResponse.
			Request: httptest.NewRequest(http.MethodPost, "/v1/messages", nil),
		}
		resp.Header["Content-Encoding"] = []string{
			"br\r\ntime=2026-08-04T00:00:00Z level=ERROR msg=\"auth bypassed\"",
		}

		if err := p.modifyResponse(resp); err != nil {
			t.Fatalf("modifyResponse: %v", err)
		}
		got := buf.String()
		if !strings.Contains(got, "unhandled Content-Encoding") {
			t.Fatalf("the guard never fired; log=%q", got)
		}
		// The join is THE detector. Do not add a "no extra newlines" assertion
		// here: slog's TextHandler quotes any value needing quoting, so the
		// emitted record is structurally one physical line whether or not
		// logsafe ran. Measured — such an assertion did not fire against the
		// unwrapped mutant, i.e. it could not fail. This one did.
		if !strings.Contains(got, `brtime=`) {
			t.Errorf("CR/LF survived into the log field: expected logsafe to strip them so the "+
				"halves join as \"brtime=\", got: %q", got)
		}
	})
}
