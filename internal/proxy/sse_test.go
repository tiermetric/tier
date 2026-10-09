package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tiermetric/tier/internal/collector"
)

// --- SSE framer tests ---

// TestSSEFramer_BoundaryAcrossFeeds verifies that the framer reassembles SSE
// events whose `\n\n` terminator straddles a chunk boundary — the most common
// failure mode in stream parsers. We feed the same logical stream split at
// every possible byte position and assert the parsed events are identical.
func TestSSEFramer_BoundaryAcrossFeeds(t *testing.T) {
	t.Parallel()
	stream := "event: a\ndata: {\"x\":1}\n\nevent: b\ndata: {\"x\":2}\n\n"

	for split := 0; split <= len(stream); split++ {
		split := split
		t.Run(fmt.Sprintf("split-at-%d", split), func(t *testing.T) {
			t.Parallel()
			var got []string
			f := &sseFramer{onEvent: func(eventType string, data []byte) {
				got = append(got, eventType+":"+string(data))
			}}
			f.Feed([]byte(stream[:split]))
			f.Feed([]byte(stream[split:]))
			f.Flush()

			want := []string{`a:{"x":1}`, `b:{"x":2}`}
			if !equalStrings(got, want) {
				t.Errorf("split=%d: got %v, want %v", split, got, want)
			}
		})
	}
}

// TestSSEFramer_FlushDeliversTrailingEvent covers truncated streams (client
// disconnect, upstream crash) where the final event lacks the `\n\n`
// terminator. Flush must still deliver it so partial state is preserved.
func TestSSEFramer_FlushDeliversTrailingEvent(t *testing.T) {
	t.Parallel()
	var got string
	f := &sseFramer{onEvent: func(eventType string, data []byte) {
		got = eventType + ":" + string(data)
	}}
	f.Feed([]byte("event: trailing\ndata: {\"final\":true}")) // no trailing \n\n
	f.Flush()
	if got != `trailing:{"final":true}` {
		t.Errorf("Flush dropped trailing event: %q", got)
	}
}

// TestSSEFramer_CRLFLineTerminators verifies the framer handles SSE streams
// with "\r\n" line endings — produced by some intermediary proxies even when
// the upstream emits "\n". A regression here would silently lose every event
// from a Cloudflare-fronted or Vertex-region-affected upstream.
func TestSSEFramer_CRLFLineTerminators(t *testing.T) {
	t.Parallel()
	want := []string{`a:{"x":1}`, `b:{"x":2}`}
	for name, stream := range map[string]string{
		"CRLF":        "event: a\r\ndata: {\"x\":1}\r\n\r\nevent: b\r\ndata: {\"x\":2}\r\n\r\n",
		"CR":          "event: a\rdata: {\"x\":1}\r\revent: b\rdata: {\"x\":2}\r\r",
		"LF":          "event: a\ndata: {\"x\":1}\n\nevent: b\ndata: {\"x\":2}\n\n",
		"mixed":       "event: a\r\ndata: {\"x\":1}\r\revent: b\ndata: {\"x\":2}\r\n\n",
		"CR-CRLF":     "event: a\rdata: {\"x\":1}\r\r\nevent: b\rdata: {\"x\":2}\r\r\n",
		"trailing-CR": "event: a\rdata: {\"x\":1}\r\revent: b\rdata: {\"x\":2}\r",
	} {
		t.Run(name, func(t *testing.T) {
			checkFeeds := func(t *testing.T, chunks []string) {
				t.Helper()
				var got []string
				f := &sseFramer{onEvent: func(eventType string, data []byte) {
					got = append(got, eventType+":"+string(data))
				}}
				for _, chunk := range chunks {
					f.Feed([]byte(chunk))
					f.Feed(nil) // An empty feed must preserve a pending CRLF pair.
				}
				if name == "trailing-CR" && len(got) != 1 {
					t.Fatalf("before Flush: got %v, want only the first event", got)
				}
				f.Flush()
				if !equalStrings(got, want) {
					t.Errorf("got %v, want %v", got, want)
				}
			}
			// Every split includes both CR CR | LF and CR | CR LF.
			for split := 0; split <= len(stream); split++ {
				t.Run(fmt.Sprintf("split-at-%d", split), func(t *testing.T) {
					checkFeeds(t, []string{stream[:split], stream[split:]})
				})
			}
			t.Run("bytewise", func(t *testing.T) {
				chunks := make([]string, len(stream))
				for i := range chunks {
					chunks[i] = stream[i : i+1]
				}
				checkFeeds(t, chunks)
			})
		})
	}
}

// S09-6: splitting CRLF between upstream reads must preserve captured usage.
func TestStreamCapture_CRLFReadBoundaries_S09_6(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_crlf","model":"claude-sonnet-4","usage":{"input_tokens":150,"output_tokens":1,"cache_creation_input_tokens":50,"cache_read_input_tokens":25}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":99}}`,
		``,
		``,
	}, "\r\n")
	capture := func(t *testing.T, upstream io.Reader) collector.TokenEvent {
		t.Helper()
		sink := newMemSink()
		sc := newStreamCapture(context.Background(), io.NopCloser(upstream), ProviderAnthropic,
			"alice", "issue-1095", "", "", sink, func(error) {}, func(string) {}, slog.Default())
		forwarded, err := io.ReadAll(sc)
		if err != nil {
			t.Fatal(err)
		}
		if string(forwarded) != stream {
			t.Fatal("forwarded stream changed")
		}
		if err := sc.Close(); err != nil {
			t.Fatal(err)
		}
		events := sink.snapshot()
		if len(events) != 1 {
			t.Fatalf("captured %d events, want 1", len(events))
		}
		return events[0]
	}
	want := capture(t, strings.NewReader(stream))
	if want.InputTok != 150 || want.OutputTok != 99 || want.CacheRead != 25 || want.CacheWrite5m != 50 {
		t.Fatalf("unexpected unsplit usage: %+v", want)
	}
	for split := 1; split < len(stream); split++ {
		if stream[split-1] != '\r' || stream[split] != '\n' {
			continue
		}
		t.Run(fmt.Sprintf("split-at-%d", split), func(t *testing.T) {
			// MultiReader returns each segment in separate Read calls.
			got := capture(t, io.MultiReader(strings.NewReader(stream[:split]), strings.NewReader(stream[split:])))
			got.Timestamp = want.Timestamp
			if !reflect.DeepEqual(got, want) {
				t.Errorf("split usage = %+v, unsplit = %+v", got, want)
			}
		})
	}
}

// TestSSEFramer_MultilineDataConcatenated verifies the framer joins multiple
// `data:` lines within a single event with '\n' between them, per the SSE
// spec. Real providers emit one data line today, but a wire-format evolution
// that splits a payload across lines must not silently lose bytes.
func TestSSEFramer_MultilineDataConcatenated(t *testing.T) {
	t.Parallel()
	stream := "event: chunk\ndata: line-one\ndata: line-two\n\n"
	var got string
	f := &sseFramer{onEvent: func(eventType string, data []byte) {
		got = string(data)
	}}
	f.Feed([]byte(stream))
	f.Flush()

	want := "line-one\nline-two"
	if got != want {
		t.Errorf("multi-line data: got %q, want %q", got, want)
	}
}

// TestSSEFramer_SplitInsideJSONPayload ensures a chunk boundary inside a JSON
// `data:` payload still produces correct parsing once the rest of the bytes
// arrive — defends against a parser that calls json.Unmarshal on a partial
// event.
func TestSSEFramer_SplitInsideJSONPayload(t *testing.T) {
	t.Parallel()
	full := "event: m\ndata: {\"output_tokens\":42}\n\n"
	// Split right in the middle of the JSON number.
	splitAt := strings.Index(full, "42") + 1
	var got string
	f := &sseFramer{onEvent: func(eventType string, data []byte) {
		got = string(data)
	}}
	f.Feed([]byte(full[:splitAt])) // up through "{\"output_tokens\":4"
	if got != "" {
		t.Fatalf("framer dispatched on partial event: %q", got)
	}
	f.Feed([]byte(full[splitAt:])) // "2}\n\n"
	if got != `{"output_tokens":42}` {
		t.Errorf("mid-JSON-split parse failed: got %q", got)
	}
}

// TestSSEFramer_BufferCapDropsOldest defends against an upstream that emits
// gigabytes between frame terminators. We feed slightly over the cap and
// assert subsequent valid events still parse.
func TestSSEFramer_BufferCapDropsOldest(t *testing.T) {
	t.Parallel()
	var events []string
	f := &sseFramer{onEvent: func(eventType string, data []byte) {
		events = append(events, eventType+":"+string(data))
	}}

	// Feed a chunk just over the cap that contains no terminator — should be
	// truncated, not OOM.
	junk := bytes.Repeat([]byte("x"), maxSSEBuffer+1024)
	junk[len(junk)-1] = 'y'
	f.Feed([]byte("old"))
	f.Feed(junk)
	if len(f.buf) > maxSSEBuffer {
		t.Fatalf("buffer length = %d, exceeds cap %d", len(f.buf), maxSSEBuffer)
	}
	if !bytes.Equal(f.buf, junk[len(junk)-maxSSEBuffer:]) {
		t.Fatal("buffer did not retain the newest bytes")
	}
	// Now feed a complete event. It must still be parsed.
	f.Feed([]byte("\n\nevent: ok\ndata: {\"ok\":1}\n\n"))
	if len(f.buf) > maxSSEBuffer {
		t.Fatalf("buffer length after recovery = %d, exceeds cap %d", len(f.buf), maxSSEBuffer)
	}

	found := false
	for _, e := range events {
		if e == `ok:{"ok":1}` {
			found = true
		}
	}
	if !found {
		t.Errorf("framer failed to recover after buffer-cap drop; events=%v", events)
	}
}

// A split CRLF's paired LF adds no buffered bytes, even at the buffer cap.
func TestSSEFramer_BufferCapPreservesUsageAfterSplitCRLF(t *testing.T) {
	t.Parallel()
	p := &anthropicStreamParser{}
	f := &sseFramer{onEvent: p.OnEvent}
	event := "event: message_start\ndata: " +
		`{"message":{"id":"msg_cap","model":"claude-sonnet-4","usage":{"input_tokens":150,"output_tokens":1}}}`
	stream := event + strings.Repeat(" ", maxSSEBuffer-len(event)-1) + "\r"
	f.Feed([]byte(stream))
	if len(f.buf) != maxSSEBuffer || !f.afterCR {
		t.Fatalf("setup: buffer length=%d, afterCR=%v", len(f.buf), f.afterCR)
	}
	wantBuf := bytes.Clone(f.buf)

	f.Feed([]byte("\n"))
	if !bytes.Equal(f.buf, wantBuf) {
		t.Fatalf("paired LF changed the full buffer: length=%d, want %d", len(f.buf), len(wantBuf))
	}
	if f.afterCR {
		t.Fatal("paired LF did not clear the pending CR")
	}
	f.Flush()
	ev := p.Finalise("alice", "issue-1095", "")
	if ev == nil {
		t.Fatal("paired LF at the buffer cap lost captured usage")
	}
	if ev.Model != "claude-sonnet-4" || ev.InputTok != 150 || ev.OutputTok != 1 {
		t.Fatalf("unexpected captured usage: %+v", ev)
	}
}

// --- Anthropic streaming parser ---

func TestAnthropicStreamParser_ExtractsIDAndCumulativeOutput(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_stream_01","model":"claude-sonnet-4","usage":{"input_tokens":150,"output_tokens":1,"cache_creation_input_tokens":50,"cache_read_input_tokens":25}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"text":"hello"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":42}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":99}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	p := &anthropicStreamParser{}
	f := &sseFramer{onEvent: p.OnEvent}
	f.Feed([]byte(stream))
	f.Flush()

	ev := p.Finalise("alice", "issue-42", "")
	if ev == nil {
		t.Fatal("Finalise returned nil for a complete stream")
	}
	if ev.Model != "claude-sonnet-4" {
		t.Errorf("Model = %q, want claude-sonnet-4", ev.Model)
	}
	if ev.InputTok != 150 {
		t.Errorf("InputTok = %d, want 150 (from message_start)", ev.InputTok)
	}
	if ev.OutputTok != 99 {
		t.Errorf("OutputTok = %d, want 99 (last message_delta is cumulative)", ev.OutputTok)
	}
	// Legacy stream — no nested cache_creation — the rolled-up 50 buckets to 5m.
	if ev.CacheRead != 25 || ev.CacheWrite5m != 50 || ev.CacheWrite1h != 0 {
		t.Errorf("cache fields: read=%d 5m=%d 1h=%d, want 25/50/0",
			ev.CacheRead, ev.CacheWrite5m, ev.CacheWrite1h)
	}
	wantKey := collector.MessageIdempotencyKey(string(ProviderAnthropic), "msg_stream_01")
	if ev.IdempotencyKey != wantKey {
		t.Errorf("IdempotencyKey = %q, want %q", ev.IdempotencyKey, wantKey)
	}
}

// TestAnthropicStreamParser_ParsesNestedCacheCreationTTL confirms the SSE
// path picks up the nested cache_creation object on message_start (the
// canonical streaming shape for Anthropic responses since the 1h-TTL
// feature). Without it, both buckets stay at zero and ComputeCost applies
// the wrong multiplier. Issue #55.
func TestAnthropicStreamParser_ParsesNestedCacheCreationTTL(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_stream_ttl","model":"claude-opus-4-7","usage":{"input_tokens":100,"output_tokens":1,"cache_creation_input_tokens":400,"cache_read_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":300}}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":42}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	p := &anthropicStreamParser{}
	f := &sseFramer{onEvent: p.OnEvent}
	f.Feed([]byte(stream))
	f.Flush()

	ev := p.Finalise("alice", "issue-55", "")
	if ev == nil {
		t.Fatal("Finalise returned nil")
	}
	if ev.CacheWrite5m != 100 {
		t.Errorf("CacheWrite5m = %d, want 100 (from nested ephemeral_5m_input_tokens)", ev.CacheWrite5m)
	}
	if ev.CacheWrite1h != 300 {
		t.Errorf("CacheWrite1h = %d, want 300 (from nested ephemeral_1h_input_tokens)", ev.CacheWrite1h)
	}
	if ev.CacheRead != 50 {
		t.Errorf("CacheRead = %d, want 50", ev.CacheRead)
	}
	// Cost assertion catches swapped 5m/1h multipliers — same arithmetic as
	// the non-streaming TestParseAnthropic_ParsesNestedCacheCreationTTL.
	// Opus 4.7 at the corrected $5/M input, $25/M output rate (#80):
	//   uncached input:    100 * $5/M            = $0.000500
	//   read (0.1x):        50 * $5/M * 0.1      = $0.0000250
	//   write5m (1.25x):   100 * $5/M * 1.25     = $0.000625
	//   write1h (2.0x):    300 * $5/M * 2.0      = $0.0030
	//   output:             42 * $25/M           = $0.00105
	//   total = $0.0052
	if ev.CostMicro < 5_100 || ev.CostMicro > 5_300 {
		t.Errorf("CostMicro = %d, want ~5_200 ($0.0052) — 5m/1h multipliers may be swapped or miswired", ev.CostMicro)
	}
}

func TestAnthropicStreamParser_CacheOnlyUsage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                        string
		usage                       string
		cacheRead, write5m, write1h int
	}{
		{"read", `{"cache_read_input_tokens":100}`, 100, 0, 0},
		{"legacy-write", `{"cache_creation_input_tokens":100}`, 0, 100, 0},
		{"write-5m", `{"cache_creation":{"ephemeral_5m_input_tokens":100}}`, 0, 100, 0},
		{"write-1h", `{"cache_creation":{"ephemeral_1h_input_tokens":100}}`, 0, 0, 100},
		{"all", `{"cache_read_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":100}}`, 100, 100, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &anthropicStreamParser{}
			p.OnEvent("message_start", []byte(`{"message":{"id":"msg_cache_only","model":"claude-sonnet-4","usage":`+tc.usage+`}}`))
			ev := p.Finalise("alice", "issue-1095", "")
			if ev == nil {
				t.Fatal("Finalise dropped billable cache-only usage")
			}
			if ev.InputTok != 0 || ev.OutputTok != 0 || ev.CacheRead != tc.cacheRead || ev.CacheWrite5m != tc.write5m || ev.CacheWrite1h != tc.write1h {
				t.Fatalf("unexpected cache-only usage: %+v", ev)
			}
			if ev.CostMicro <= 0 {
				t.Errorf("cache-only cost = %d, want positive spend", ev.CostMicro)
			}
			if want := collector.MessageIdempotencyKey(string(ProviderAnthropic), "msg_cache_only"); ev.IdempotencyKey != want {
				t.Errorf("IdempotencyKey = %q, want %q", ev.IdempotencyKey, want)
			}
		})
	}
}

func TestAnthropicStreamParser_NonPositiveUsageReturnsNil(t *testing.T) {
	t.Parallel()
	for name, usage := range map[string]string{
		"zero":         `{"input_tokens":0,"output_tokens":0}`,
		"input":        `{"input_tokens":-100}`,
		"output":       `{"output_tokens":-100}`,
		"cache-read":   `{"cache_read_input_tokens":-100}`,
		"legacy-write": `{"cache_creation_input_tokens":-100}`,
		"write-5m":     `{"cache_creation":{"ephemeral_5m_input_tokens":-100}}`,
		"write-1h":     `{"cache_creation":{"ephemeral_1h_input_tokens":-100}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := &anthropicStreamParser{}
			p.OnEvent("message_start", []byte(`{"message":{"id":"msg_no_usage","model":"claude-sonnet-4","usage":`+usage+`}}`))
			if ev := p.Finalise("alice", "issue-1095", ""); ev != nil {
				t.Fatalf("non-positive usage emitted an event: %+v", ev)
			}
			if p.input != 0 || p.output != 0 || p.cacheRead != 0 || p.cacheWrite5m != 0 || p.cacheWrite1h != 0 {
				t.Fatalf("counts were not clamped before the no-usage guard: %+v", p)
			}
		})
	}
}

func TestAnthropicStreamParser_NoMessageStartReturnsNil(t *testing.T) {
	t.Parallel()
	// Stream that begins after message_start (e.g., upstream truncated).
	stream := strings.Join([]string{
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":5}}`,
		``,
	}, "\n")

	p := &anthropicStreamParser{}
	f := &sseFramer{onEvent: p.OnEvent}
	f.Feed([]byte(stream))
	f.Flush()

	if ev := p.Finalise("alice", "issue-42", ""); ev != nil {
		t.Errorf("expected nil event when message_start never arrived, got %+v", ev)
	}
}

// --- OpenAI streaming parser ---

func TestOpenAIStreamParser_FinalChunkUsage(t *testing.T) {
	t.Parallel()
	// Realistic shape: many chunks with usage:null, one final chunk with usage.
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-stream01","model":"gpt-4o","choices":[{"delta":{"content":"Hi"}}],"usage":null}`,
		``,
		`data: {"id":"chatcmpl-stream01","model":"gpt-4o","choices":[{"delta":{"content":" there"}}],"usage":null}`,
		``,
		`data: {"id":"chatcmpl-stream01","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":40,"completion_tokens":15,"prompt_tokens_details":{"cached_tokens":5}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	p := &openAIStreamParser{}
	f := &sseFramer{onEvent: p.OnEvent}
	f.Feed([]byte(stream))
	f.Flush()

	ev := p.Finalise("bob", "issue-7", "")
	if ev == nil {
		t.Fatal("Finalise returned nil with a populated final usage block")
	}
	// prompt_tokens 40 includes 5 cached, so InputTok carves to 35 (#114).
	if ev.InputTok != 35 || ev.OutputTok != 15 || ev.CacheRead != 5 {
		t.Errorf("got input=%d output=%d cache=%d, want 35/15/5",
			ev.InputTok, ev.OutputTok, ev.CacheRead)
	}
	wantKey := collector.MessageIdempotencyKey(string(ProviderOpenAI), "chatcmpl-stream01")
	if ev.IdempotencyKey != wantKey {
		t.Errorf("IdempotencyKey = %q, want %q", ev.IdempotencyKey, wantKey)
	}
}

// TestOpenAIStreamParser_CachedTokensNotDoubleCharged pins #114 for the SSE
// path: the final usage chunk carries prompt_tokens INCLUSIVE of cached_tokens,
// so Finalise must carve cached out of Input before pricing. Fails on main.
func TestOpenAIStreamParser_CachedTokensNotDoubleCharged(t *testing.T) {
	t.Parallel()
	// gpt-4o $2.50/$10.00. prompt 1000 (900 cached), completion 100. Input
	// carves to 100; cost = 250 + 1125 + 1000 = 2375 micro. Main computes 4625.
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-strnodouble","model":"gpt-4o","choices":[{"delta":{"content":"hi"}}],"usage":null}`,
		``,
		`data: {"id":"chatcmpl-strnodouble","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":100,"prompt_tokens_details":{"cached_tokens":900}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	p := &openAIStreamParser{}
	f := &sseFramer{onEvent: p.OnEvent}
	f.Feed([]byte(stream))
	f.Flush()

	ev := p.Finalise("dev", "issue", "")
	if ev == nil {
		t.Fatal("Finalise returned nil with a populated final usage block")
	}
	if ev.InputTok != 100 {
		t.Errorf("InputTok = %d, want 100 (prompt 1000 - cached 900)", ev.InputTok)
	}
	if ev.CacheRead != 900 {
		t.Errorf("CacheRead = %d, want 900", ev.CacheRead)
	}
	if ev.CostMicro != 2375 {
		t.Errorf("CostMicro = %d, want 2375 (cached not double-charged)", ev.CostMicro)
	}
}

func TestOpenAIStreamParser_UsageLatching(t *testing.T) {
	t.Parallel()
	const first = `{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":20}}`
	for _, tc := range []struct {
		name                  string
		usage                 []string
		input, output, cached int
		wantNil               bool
	}{
		{"latest-positive", []string{first, `{"prompt_tokens":200,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":50}}`}, 150, 30, 50, false},
		{"later-zero", []string{first, `{"prompt_tokens":0,"completion_tokens":0}`}, 80, 10, 20, false},
		{"later-zero-with-details", []string{first, `{"prompt_tokens":0,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":0}}`}, 80, 10, 20, false},
		{"zero-only", []string{`{"prompt_tokens":0,"completion_tokens":0}`}, 0, 0, 0, true},
		{"later-without-details", []string{first, `{"prompt_tokens":200,"completion_tokens":30}`}, 200, 30, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &openAIStreamParser{}
			f := &sseFramer{onEvent: p.OnEvent}
			for _, usage := range tc.usage {
				f.Feed([]byte(`data: {"id":"chatcmpl-latch","model":"gpt-4o","usage":` + usage + "}\n\n"))
			}
			f.Flush()
			ev := p.Finalise("bob", "issue-1095", "")
			if tc.wantNil {
				if ev != nil || p.chat.gotUsage {
					t.Fatalf("zero-only block latched usage: event=%+v, gotUsage=%v", ev, p.chat.gotUsage)
				}
				return
			}
			if ev == nil {
				t.Fatal("Finalise dropped populated usage")
			}
			if ev.InputTok != tc.input || ev.OutputTok != tc.output || ev.CacheRead != tc.cached {
				t.Errorf("got input=%d output=%d cache=%d, want %d/%d/%d", ev.InputTok, ev.OutputTok, ev.CacheRead, tc.input, tc.output, tc.cached)
			}
		})
	}
}

func TestOpenAIStreamParser_NoUsageMeansNoEmit(t *testing.T) {
	t.Parallel()
	// Client did not opt in to stream_options.include_usage — every chunk has usage:null.
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-x","model":"gpt-4o","choices":[{"delta":{"content":"hi"}}],"usage":null}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	p := &openAIStreamParser{}
	f := &sseFramer{onEvent: p.OnEvent}
	f.Feed([]byte(stream))
	f.Flush()

	if ev := p.Finalise("bob", "issue-7", ""); ev != nil {
		t.Errorf("expected nil event when include_usage was not set, got %+v", ev)
	}
}

// TestStreamCapture_NoUsageStreamCounts pins reason "stream_no_usage": an SSE
// stream that closes without a usable usage block (Finalise returns nil) emits
// no event but must be counted through the full streamCapture emit closure — the
// streaming analogue of the JSON "parse" case. Fails on main: the counter does
// not exist and the emit closure returns silently when ev==nil.
func TestStreamCapture_NoUsageStreamCounts(t *testing.T) {
	t.Parallel()
	// OpenAI stream that never carries usage (client did not opt into
	// stream_options.include_usage), reusing the NoUsageMeansNoEmit frames.
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-x","model":"gpt-4o","choices":[{"delta":{"content":"hi"}}],"usage":null}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	sink := newMemSink()
	rec := &recRecorder{}
	p := New(mustParseURL(t, "http://example.invalid"), ProviderOpenAI, collector.SourceProxy, sink, nil, rec, nil)
	sc := newStreamCapture(context.Background(), io.NopCloser(strings.NewReader(stream)),
		p.provider, "bob", "issue-7", "", "", p.sink, p.recordWrite, p.recordUncaptured, slog.Default())
	if _, err := io.ReadAll(sc); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := sc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	events := sink.snapshot()
	if got := len(events); got != 0 {
		t.Fatalf("expected 0 events, got %d: %+v", got, events)
	}
	if calls := rec.snapshot(); len(calls) != 1 || calls[0] != "openai|"+reasonStreamNoUsage {
		t.Errorf("recorder calls = %v, want [openai|stream_no_usage]", calls)
	}
}

// --- Gemini streaming parser ---

func TestGeminiStreamParser_CumulativeUsage(t *testing.T) {
	t.Parallel()
	// Gemini sends running totals on every chunk; the last value wins.
	stream := strings.Join([]string{
		`data: {"responseId":"gem-stream-01","modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":10}}`,
		``,
		`data: {"responseId":"gem-stream-01","modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":35}}`,
		``,
		`data: {"responseId":"gem-stream-01","modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":60}}`,
		``,
	}, "\n")

	p := &geminiStreamParser{}
	f := &sseFramer{onEvent: p.OnEvent}
	f.Feed([]byte(stream))
	f.Flush()

	ev := p.Finalise("carol", "issue-99", "")
	if ev == nil {
		t.Fatal("Finalise returned nil")
	}
	if ev.InputTok != 100 || ev.OutputTok != 60 {
		t.Errorf("got input=%d output=%d, want 100/60 (cumulative final)", ev.InputTok, ev.OutputTok)
	}
	wantKey := collector.MessageIdempotencyKey(string(ProviderGemini), "gem-stream-01")
	if ev.IdempotencyKey != wantKey {
		t.Errorf("IdempotencyKey = %q, want %q", ev.IdempotencyKey, wantKey)
	}
}

// --- End-to-end streaming proxy tests ---

// TestProxy_StreamingAnthropicDedupes routes a synthetic Anthropic SSE response
// through the proxy twice and asserts exactly one event lands in the sink with
// the correct usage and idempotency key. This is the integration contract for
// the streaming path: same message id → same key → ON CONFLICT DO NOTHING in
// production.
func TestProxy_StreamingAnthropicDedupes(t *testing.T) {
	t.Parallel()
	const respID = "msg_e2e_stream_01"
	streamBody := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"` + respID + `","model":"claude-sonnet-4","usage":{"input_tokens":75,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":50}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streamBody)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	sink := newMemSink()
	p := New(target, ProviderAnthropic, collector.SourceProxy, sink, nil, nil, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages",
			strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tier-Developer", "alice")
		req.Header.Set("X-Tier-Issue", "issue-42")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		// MUST fully drain the body — the streaming emit fires at Close, and
		// Close is called only after the response body is fully read by the
		// httputil.ReverseProxy machinery.
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body %d: %v", i, err)
		}
		_ = resp.Body.Close()

		// Sanity: client received the bytes verbatim (streaming forward unchanged).
		if !bytes.Equal(body, []byte(streamBody)) {
			t.Errorf("request %d: client body differs from upstream", i)
		}
	}

	proxySrv.Close() // Wait for server-side stream Close and ingestion.
	events := sink.snapshot()
	if got := len(events); got != 1 {
		t.Fatalf("expected 1 event after duplicate streaming insert, got %d: %+v", got, events)
	}
	wantKey := collector.MessageIdempotencyKey(string(ProviderAnthropic), respID)
	if events[0].IdempotencyKey != wantKey {
		t.Errorf("IdempotencyKey = %q, want %q", events[0].IdempotencyKey, wantKey)
	}
	if events[0].InputTok != 75 || events[0].OutputTok != 50 {
		t.Errorf("usage: input=%d output=%d, want 75/50", events[0].InputTok, events[0].OutputTok)
	}
	if events[0].Developer != "alice" {
		t.Errorf("developer = %q, want alice", events[0].Developer)
	}
}

type ingestCountingSink struct {
	*memSink
	calls atomic.Int32
}

func (s *ingestCountingSink) Ingest(ctx context.Context, event collector.TokenEvent) error {
	s.calls.Add(1)
	return s.memSink.Ingest(ctx, event)
}

// TestStreamCapture_CloseEmitsExactlyOnce locks in the emit-on-Close contract:
// emit fires from Close, and a double-Close (which httputil.ReverseProxy
// generally doesn't do, but defensive middleware can) must not fire emit twice.
func TestStreamCapture_CloseEmitsExactlyOnce(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_close_test","model":"claude-sonnet-4","usage":{"input_tokens":10,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":5}}`,
		``,
	}, "\n")

	sink := &ingestCountingSink{memSink: newMemSink()}
	ctx := context.Background()
	sc := newStreamCapture(ctx, io.NopCloser(strings.NewReader(stream)), ProviderAnthropic, "alice", "issue-1", "", "", sink, func(error) {}, func(string) {}, slog.Default())

	// Drain the stream as the proxy machinery would.
	if _, err := io.ReadAll(sc); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := sc.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := sc.Close(); err != nil {
		// Closing a NopCloser twice is fine; just ensure we don't panic.
		t.Fatalf("second Close: %v", err)
	}
	events := sink.snapshot()
	if got := len(events); got != 1 {
		t.Fatalf("expected exactly 1 event after double-Close, got %d: %+v", got, events)
	}
	if got := sink.calls.Load(); got != 1 {
		t.Fatalf("Ingest calls after double-Close = %d, want 1", got)
	}
}

// TestProxy_StreamingNon200BypassesParsing locks in the StatusCode<200 || >=300
// guard for the streaming path: even billable usage under 429 must not emit
// a token event. An error-only stream cannot discriminate a missing guard.
func TestProxy_StreamingNon200BypassesParsing(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"message\":{\"id\":\"msg_429\",\"model\":\"claude-sonnet-4\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n")
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	sink := newMemSink()
	p := New(target, ProviderAnthropic, collector.SourceProxy, sink, nil, nil, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	proxySrv.Close() // Wait for server-side stream Close and ingestion.
	events := sink.snapshot()
	if len(events) != 0 {
		t.Errorf("non-2xx streaming response must not emit token events, got %d", len(events))
	}
}

// equalStrings is a local helper avoiding an extra slices import dependency.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- Negative-usage clamp on the SSE Finalise path (#121) ---

// TestAnthropicStreamParser_NegativeTokensClamped drives a negative input on
// message_start and asserts Finalise clamps it while keeping the streamed
// output. Fails on main. The SSE boundary is the deferred-emit twin of the
// non-streaming parseAnthropic clamp.
func TestAnthropicStreamParser_NegativeTokensClamped(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_neg","model":"claude-sonnet-4","usage":{"input_tokens":-1000,"output_tokens":1,"cache_read_input_tokens":-5}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":300}}`,
		``,
		`event: message_stop`,
		``,
	}, "\n")

	p := &anthropicStreamParser{}
	f := &sseFramer{onEvent: p.OnEvent}
	f.Feed([]byte(stream))
	f.Flush()

	ev := p.Finalise("alice", "issue-121", "")
	if ev == nil {
		t.Fatal("Finalise returned nil; clamp must keep the event, not drop it")
	}
	if ev.InputTok != 0 {
		t.Errorf("InputTok = %d, want 0 (negative input clamped)", ev.InputTok)
	}
	if ev.CacheRead != 0 {
		t.Errorf("CacheRead = %d, want 0 (negative cache_read clamped)", ev.CacheRead)
	}
	if ev.OutputTok != 300 {
		t.Errorf("OutputTok = %d, want 300 (cumulative delta untouched)", ev.OutputTok)
	}
	if ev.CostMicro < 0 {
		t.Errorf("CostMicro = %d, want >= 0 after clamp", ev.CostMicro)
	}
}

// TestOpenAIStreamParser_NegativeTokensClamped drives a negative prompt and a
// negative cached count on the final usage chunk; both clamp on Finalise. The
// block's total stays positive so it passes the populated-usage latch guard.
// This is a clamp regression + forward-compat guard, not an ordering pin (both
// negatives collapse to 0 under either order); the CacheRead==0 assertion stays
// true after P0-01's (#114) cached=min(cached,prompt) subtraction lands. Fails
// on main.
func TestOpenAIStreamParser_NegativeTokensClamped(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-neg","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":-100,"completion_tokens":150,"prompt_tokens_details":{"cached_tokens":-30}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	p := &openAIStreamParser{}
	f := &sseFramer{onEvent: p.OnEvent}
	f.Feed([]byte(stream))
	f.Flush()

	ev := p.Finalise("bob", "issue-121", "")
	if ev == nil {
		t.Fatal("Finalise returned nil; clamp must keep the event, not drop it")
	}
	if ev.InputTok != 0 {
		t.Errorf("InputTok = %d, want 0 (negative prompt clamped before subtraction)", ev.InputTok)
	}
	if ev.CacheRead != 0 {
		t.Errorf("CacheRead = %d, want 0 (negative cached clamped)", ev.CacheRead)
	}
	if ev.OutputTok != 150 {
		t.Errorf("OutputTok = %d, want 150 (valid field untouched)", ev.OutputTok)
	}
	if ev.CostMicro < 0 {
		t.Errorf("CostMicro = %d, want >= 0 after clamp", ev.CostMicro)
	}
}

// TestGeminiStreamParser_NegativeTokensClamped drives a negative cumulative
// candidatesTokenCount and asserts Finalise clamps it. Fails on main.
func TestGeminiStreamParser_NegativeTokensClamped(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`data: {"responseId":"gem-neg","modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":-40}}`,
		``,
	}, "\n")

	p := &geminiStreamParser{}
	f := &sseFramer{onEvent: p.OnEvent}
	f.Feed([]byte(stream))
	f.Flush()

	ev := p.Finalise("carol", "issue-121", "")
	if ev == nil {
		t.Fatal("Finalise returned nil; clamp must keep the event, not drop it")
	}
	if ev.InputTok != 100 {
		t.Errorf("InputTok = %d, want 100 (valid field untouched)", ev.InputTok)
	}
	if ev.OutputTok != 0 {
		t.Errorf("OutputTok = %d, want 0 (negative candidates clamped)", ev.OutputTok)
	}
	if ev.CostMicro < 0 {
		t.Errorf("CostMicro = %d, want >= 0 after clamp", ev.CostMicro)
	}
}

// TestGeminiStreamParser_ThoughtsAndCachedCaptured pins P0-09 on the SSE path:
// the cumulative last-wins accumulation captures thoughtsTokenCount (folded into
// Output) and cachedContentTokenCount (carved out of Input, discounted). Combines
// the JSON tests 1+2 on the streaming parser. Fails on main (fields not read).
func TestGeminiStreamParser_ThoughtsAndCachedCaptured(t *testing.T) {
	t.Parallel()
	// Running totals grow each chunk; the final chunk carries the full counts:
	// prompt 1000 (900 cached), candidates 100, thoughts 800.
	// Input = 1000 - 900 = 100; CacheRead = 900; Output = 100 + 800 = 900.
	// cost = 100*1.25/1e6 + 900*1.25*0.25/1e6 + 900*10.00/1e6
	//      = 0.000125 + 0.00028125 + 0.009 = 0.00940625 dollars
	//      * 1e6 = 9406.25 → RoundToEven → 9406 micro.
	stream := strings.Join([]string{
		`data: {"responseId":"gem-tc-01","modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":40,"thoughtsTokenCount":300,"cachedContentTokenCount":900}}`,
		``,
		`data: {"responseId":"gem-tc-01","modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":100,"thoughtsTokenCount":800,"cachedContentTokenCount":900}}`,
		``,
	}, "\n")

	p := &geminiStreamParser{}
	f := &sseFramer{onEvent: p.OnEvent}
	f.Feed([]byte(stream))
	f.Flush()

	ev := p.Finalise("carol", "issue-122", "")
	if ev == nil {
		t.Fatal("Finalise returned nil")
	}
	if ev.InputTok != 100 {
		t.Errorf("InputTok = %d, want 100 (prompt 1000 - cached 900)", ev.InputTok)
	}
	if ev.CacheRead != 900 {
		t.Errorf("CacheRead = %d, want 900", ev.CacheRead)
	}
	if ev.OutputTok != 900 {
		t.Errorf("OutputTok = %d, want 900 (candidates 100 + thoughts 800)", ev.OutputTok)
	}
	if ev.CostMicro != 9_406 {
		t.Errorf("CostMicro = %d, want 9_406 (thoughts output-rate + cached carved/discounted)", ev.CostMicro)
	}
}

// TestGeminiStreamParser_ThoughtsOnlyNotDropped pins the widened zero-usage
// guard (#122) on the SSE path: a stream whose only usage is thoughtsTokenCount
// is billable spend and must not be dropped by Finalise.
func TestGeminiStreamParser_ThoughtsOnlyNotDropped(t *testing.T) {
	t.Parallel()
	// gemini-2.5-pro. Final chunk: prompt 0, candidates 0, thoughts 500.
	// Output 500. cost = 500*10.00/1e6 = 0.005 = 5_000 micro.
	stream := strings.Join([]string{
		`data: {"responseId":"gem-thoughts-only-01","modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0,"thoughtsTokenCount":500}}`,
		``,
	}, "\n")

	p := &geminiStreamParser{}
	f := &sseFramer{onEvent: p.OnEvent}
	f.Feed([]byte(stream))
	f.Flush()

	ev := p.Finalise("carol", "issue-122", "")
	if ev == nil {
		t.Fatal("Finalise dropped a thoughts-only stream; it is billable spend")
	}
	if ev.InputTok != 0 || ev.OutputTok != 500 {
		t.Errorf("got input=%d output=%d, want 0/500 (thoughts-only)", ev.InputTok, ev.OutputTok)
	}
	if ev.CostMicro != 5_000 {
		t.Errorf("CostMicro = %d, want 5_000 (500 thoughts at $10/M output)", ev.CostMicro)
	}
}

// TestGeminiStreamParser_NegativeCachedClamped pins that the #121 clamp covers
// the new cached counter on the SSE path: a negative cachedContentTokenCount must
// not inflate Input or push CacheRead below 0.
func TestGeminiStreamParser_NegativeCachedClamped(t *testing.T) {
	t.Parallel()
	// Final chunk: prompt 100, candidates 20, cached -50. After clamp cached 0.
	// Input 100, CacheRead 0, Output 20. cost = 100*1.25/1e6 + 20*10.00/1e6
	// = 0.000125 + 0.0002 = 325 micro.
	stream := strings.Join([]string{
		`data: {"responseId":"gem-neg-cache-01","modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"cachedContentTokenCount":-50}}`,
		``,
	}, "\n")

	p := &geminiStreamParser{}
	f := &sseFramer{onEvent: p.OnEvent}
	f.Feed([]byte(stream))
	f.Flush()

	ev := p.Finalise("carol", "issue-121", "")
	if ev == nil {
		t.Fatal("Finalise returned nil; clamp must keep the event, not drop it")
	}
	if ev.InputTok != 100 {
		t.Errorf("InputTok = %d, want 100 (negative cached must not inflate input)", ev.InputTok)
	}
	if ev.CacheRead != 0 {
		t.Errorf("CacheRead = %d, want 0 (negative cached clamped)", ev.CacheRead)
	}
	if ev.CostMicro != 325 {
		t.Errorf("CostMicro = %d, want 325 (clamped, never negative)", ev.CostMicro)
	}
}
