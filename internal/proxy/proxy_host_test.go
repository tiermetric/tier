package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// TestParseAnthropic_StampsHostAndBillingMode asserts the JSON parser threads the
// serving host into the emitted event and derives the billing_mode from pricing
// (#300). A first-party Anthropic model bills per_token.
func TestParseAnthropic_StampsHostAndBillingMode(t *testing.T) {
	body := []byte(`{"id":"msg_1","model":"claude-opus-4-8","usage":{"input_tokens":100,"output_tokens":50}}`)
	ev := parseAnthropic(body, "alice", "issue-1", "api.anthropic.com")
	if ev == nil {
		t.Fatal("parseAnthropic returned nil")
	}
	if ev.Host != "api.anthropic.com" {
		t.Errorf("Host = %q, want api.anthropic.com", ev.Host)
	}
	if ev.BillingMode != store.BillingPerToken {
		t.Errorf("BillingMode = %q, want %q", ev.BillingMode, store.BillingPerToken)
	}
}

// TestParseOpenAI_SelfHostedModelBillsAmortized asserts a self-hosted open-weights
// model routed through a serving host resolves to the amortized billing mode.
func TestParseOpenAI_SelfHostedModelBillsAmortized(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-1","model":"llama-3.1-70b","usage":{"prompt_tokens":100,"completion_tokens":50}}`)
	ev := parseOpenAI(body, "bob", "issue-2", "openrouter.ai")
	if ev == nil {
		t.Fatal("parseOpenAI returned nil")
	}
	if ev.Host != "openrouter.ai" {
		t.Errorf("Host = %q, want openrouter.ai", ev.Host)
	}
	if ev.BillingMode != store.BillingSelfHostedAmortized {
		t.Errorf("BillingMode = %q, want %q", ev.BillingMode, store.BillingSelfHostedAmortized)
	}
}

// TestProxy_CapturesHostFromTarget is the end-to-end proof that a proxy stamps the
// host derived from its --target URL onto the stored event (SSE path).
func TestProxy_CapturesHostFromTarget(t *testing.T) {
	sink := newMemSink()
	target, _ := url.Parse("https://openrouter.ai")
	p := New(target, ProviderOpenAI, collector.SourceProxy, sink, nil, nil, slog.Default())
	if p.host != "openrouter.ai" {
		t.Fatalf("Proxy.host = %q, want openrouter.ai (from target)", p.host)
	}

	// Drive the SSE emit path directly through newStreamCapture, mirroring how
	// modifyResponse wires it, to confirm host flows into the stored event.
	stream := "data: {\"id\":\"chatcmpl-1\",\"model\":\"llama-3.1-70b\",\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50}}\n\n" +
		"data: [DONE]\n\n"
	sc := newStreamCapture(context.Background(), io.NopCloser(strings.NewReader(stream)),
		ProviderOpenAI, "bob", "issue-1", p.host, "", sink, func(error) {}, func(string) {}, slog.Default())
	if _, err := io.ReadAll(sc); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if err := sc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(sink.events) != 1 {
		t.Fatalf("captured %d events, want 1", len(sink.events))
	}
	if got := sink.events[0]; got.Host != "openrouter.ai" {
		t.Errorf("stored Host = %q, want openrouter.ai", got.Host)
	}
	if got := sink.events[0]; got.BillingMode != store.BillingSelfHostedAmortized {
		t.Errorf("stored BillingMode = %q, want %q", got.BillingMode, store.BillingSelfHostedAmortized)
	}
}

// TestProxy_SourceStampedByTagger locks the load-bearing invariant #46 created,
// now via the seam #337 hardened: the proxy parsers do not self-set ev.Source, so
// a proxy event is labeled by the source tagger — but that tagger is built
// INSIDE New (#337), not wrapped around the sink at the call site. The test
// passes New a BARE sink plus collector.SourceProxy, drives a capture through the
// proxy's own tagged sink (p.sink), and asserts the stored event carries
// Source == SourceProxy while Host/BillingMode still survive the decorator. That
// a bare sink through New alone yields a stamped source is the whole point: an
// untagged proxy sink is unconstructable.
func TestProxy_SourceStampedByTagger(t *testing.T) {
	sink := newMemSink()

	target, _ := url.Parse("https://openrouter.ai")
	// Bare sink in; New builds its own SourceTagger around it (#337). p.sink is
	// that internally-tagged sink — the exact one every capture path writes to.
	p := New(target, ProviderOpenAI, collector.SourceProxy, sink, nil, nil, slog.Default())

	stream := "data: {\"id\":\"chatcmpl-9\",\"model\":\"llama-3.1-70b\",\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	sc := newStreamCapture(context.Background(), io.NopCloser(strings.NewReader(stream)),
		ProviderOpenAI, "bob", "issue-1", p.host, "", p.sink, func(error) {}, func(string) {}, slog.Default())
	if _, err := io.ReadAll(sc); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if err := sc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(sink.events) != 1 {
		t.Fatalf("captured %d events, want 1", len(sink.events))
	}
	got := sink.events[0]
	if got.Source != collector.SourceProxy {
		t.Errorf("stored Source = %q, want %q — the SourceTagger wiring must label proxy events", got.Source, collector.SourceProxy)
	}
	// The tagger must not clobber the fields the parser set.
	if got.Host != "openrouter.ai" {
		t.Errorf("stored Host = %q, want openrouter.ai (Host must survive the tagger)", got.Host)
	}
	if got.BillingMode != store.BillingSelfHostedAmortized {
		t.Errorf("stored BillingMode = %q, want %q (BillingMode must survive the tagger)", got.BillingMode, store.BillingSelfHostedAmortized)
	}
}
