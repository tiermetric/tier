//go:build integration

package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Exercise runServe's real metric wiring, including unauthenticated headers.
func TestServeSmoke_WebhookOversizedCounter(t *testing.T) {
	base, _ := bootTeamServe(t, []string{"--webhook-secret", "bounds-test-secret"}, nil)
	for _, event := range []string{"push", "pull_request", "untrusted-1", "untrusted-2"} {
		limit := 1 << 20
		if event == "push" {
			limit = 25 << 20
		}
		req, err := http.NewRequest(http.MethodPost, base+"/webhook/github", strings.NewReader(strings.Repeat(" ", limit+1))) //nolint:noctx // loopback probe
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-GitHub-Event", event)
		resp, err := smokeClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 413 {
			t.Errorf("%s: status=%d, want 413", event, resp.StatusCode)
		}
	}
	code, body := scrapeMetrics(t, base, smokeAPITok)
	if code != 200 {
		t.Fatalf("metrics status=%d", code)
	}
	for _, series := range []string{`tier_webhook_oversized_total{event="push"} 1`, `tier_webhook_oversized_total{event="pull_request"} 1`, `tier_webhook_oversized_total{event="other"} 2`} {
		if !strings.Contains(body, "\n"+series+"\n") {
			t.Errorf("missing counter series %s", series)
		}
	}
	if strings.Contains(body, "untrusted-") {
		t.Fatal("unauthenticated event values became metric labels")
	}
}

func TestServeSmoke_WebhookStalledChunkedOverflowIsPrompt413(t *testing.T) {
	base, _ := bootTeamServe(t, []string{"--webhook-secret", "bounds-test-secret"}, nil)
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(base, "http://"), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// Leave the chunk unfinished beyond the overflow byte, with no final chunk.
	_, err = fmt.Fprintf(conn, "POST /webhook/github HTTP/1.1\r\nHost: localhost\r\nX-GitHub-Event: pull_request\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s", (1<<20)+2, strings.Repeat(" ", (1<<20)+1))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no prompt overflow response while chunked tail stalled: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 413 || !resp.Close {
		t.Fatalf("status=%d close=%v, want 413 and connection close", resp.StatusCode, resp.Close)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("overflow response did not complete promptly: %v", err)
	}
}
