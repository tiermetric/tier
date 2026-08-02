package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// maxSSEBuffer caps the per-stream pending buffer for incomplete events. An
// adversarial or buggy upstream could otherwise send unbounded bytes between
// frame boundaries. Mirrors the proxy's JSON maxBody and the JSONL collector's
// maxJSONLLine — 10 MB is comfortably above any real SSE event size.
const maxSSEBuffer = 10 << 20

// streamCapture wraps the upstream response body, forwards every byte to the
// client unchanged, and feeds those same bytes to an SSE framer so the proxy
// can extract the final usage block without buffering the whole response.
//
// On Close (called by httputil.ReverseProxy after the client finishes reading
// the body, or after a client disconnect tears down the response), the
// accumulated parser state is finalised and a TokenEvent is emitted to the
// sink — same code path the non-streaming JSON parser uses, just deferred
// until end-of-stream.
//
// Concurrency: net/http guarantees Read and Close are not called concurrently
// from the proxy machinery, so streamCapture needs no internal mutex. A future
// refactor that introduces a goroutine reading from this type must add one.
type streamCapture struct {
	upstream io.ReadCloser
	framer   *sseFramer
	parser   streamParser

	// emit is invoked exactly once, from Close, with the parser's final state.
	emit func(p streamParser)
}

// Read implements io.Reader. Bytes flow through unchanged; the side-channel
// Feed call is synchronous but does only fast string scanning and JSON parsing
// of small SSE events, so it does not measurably delay client byte forwarding.
func (s *streamCapture) Read(p []byte) (int, error) {
	n, err := s.upstream.Read(p)
	if n > 0 {
		s.framer.Feed(p[:n])
	}
	return n, err
}

// Close finalises the framer (flushes any trailing bytes that lack the SSE
// terminator) and invokes emit with the parser's final state.
func (s *streamCapture) Close() error {
	err := s.upstream.Close()
	s.framer.Flush()
	if s.emit != nil {
		s.emit(s.parser)
		s.emit = nil // defensive: ensure emit runs at most once
	}
	return err
}

// sseFramer turns a stream of bytes into a sequence of SSE events. Events are
// delimited by a blank line (`\n\n`); within an event, `field: value` lines
// describe the event type and data. We only care about `event:` and `data:`;
// other fields (id:, retry:) are ignored.
//
// Bytes accumulate in buf until a `\n\n` boundary is observed. The framer is
// resilient to chunk-split SSE events — the boundary may arrive in any read.
type sseFramer struct {
	buf     []byte
	onEvent func(eventType string, data []byte)
}

// Feed appends bytes to the pending buffer and dispatches any complete events.
// Bounded by maxSSEBuffer to defend against an upstream that never sends a
// frame terminator.
//
// Per the SSE spec (HTML Living Standard §9.2.6), line terminators may be
// "\r\n", "\r", or "\n". Real-world providers emit "\n" today but intermediary
// proxies (Cloudflare, some Vertex regions) normalise to "\r\n". Normalising
// to "\n" on ingest costs one linear scan but keeps the rest of the framer
// simple and correct across all three line-ending variants.
func (f *sseFramer) Feed(p []byte) {
	if len(f.buf)+len(p) > maxSSEBuffer {
		// Drop the oldest bytes; we'd rather miss one corrupted event than
		// accumulate unbounded memory. Subsequent valid events still parse.
		excess := len(f.buf) + len(p) - maxSSEBuffer
		if excess >= len(f.buf) {
			f.buf = f.buf[:0]
		} else {
			f.buf = f.buf[excess:]
		}
	}
	// Normalise line terminators so the boundary scan below only has to look
	// for "\n\n". ReplaceAll is O(n) and fine for our throughput; using a
	// staging slice keeps the original `p` untouched (callers may reuse it).
	normalised := bytes.ReplaceAll(p, []byte("\r\n"), []byte("\n"))
	normalised = bytes.ReplaceAll(normalised, []byte("\r"), []byte("\n"))
	f.buf = append(f.buf, normalised...)
	for {
		idx := bytes.Index(f.buf, []byte("\n\n"))
		if idx < 0 {
			return
		}
		event := f.buf[:idx]
		f.buf = f.buf[idx+2:]
		f.parseEvent(event)
	}
}

// Flush parses any trailing event that lacks the `\n\n` terminator. Real SSE
// streams end with one, but a truncated stream (client disconnect, upstream
// crash) can leave bytes behind that still carry useful state.
func (f *sseFramer) Flush() {
	if len(f.buf) > 0 {
		f.parseEvent(f.buf)
		f.buf = f.buf[:0]
	}
}

func (f *sseFramer) parseEvent(event []byte) {
	var eventType string
	var data []byte
	// Manual line iteration (no bytes.Split allocation): walk the slice and
	// dispatch on each '\n' boundary. Cheaper per-event GC, identical logic.
	for len(event) > 0 {
		end := bytes.IndexByte(event, '\n')
		var line []byte
		if end < 0 {
			line = event
			event = nil
		} else {
			line = event[:end]
			event = event[end+1:]
		}
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			eventType = string(bytes.TrimSpace(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			// Per spec, multiple data: lines within one event are concatenated
			// with '\n' between them. Real providers emit one today but the
			// spec compliance defends against silent loss when wire formats
			// evolve or intermediaries fold long payloads.
			payload := bytes.TrimSpace(line[len("data:"):])
			if data == nil {
				data = payload
			} else {
				data = append(append(data, '\n'), payload...)
			}
		}
	}
	if len(data) > 0 {
		f.onEvent(eventType, data)
	}
}

// streamParser is the provider-specific state machine consuming framed events.
// Each implementation accumulates final usage and exposes it via Finalise().
type streamParser interface {
	OnEvent(eventType string, data []byte)
	// Finalise builds the event from accumulated state. host (#300) is the serving
	// host the enclosing proxy forwards to; Finalise threads it into ComputeCostHost
	// and stamps the resolved host + billing_mode onto the event, so a host-qualified
	// rate applies on the streaming path exactly as on the JSON path.
	Finalise(developer, issueID, host string) *collector.TokenEvent
}

// --- Anthropic streaming parser ---
//
// Anthropic SSE shape:
//
//	event: message_start
//	data: {"type":"message_start","message":{"id":"msg_...","model":"...","usage":{"input_tokens":N,"output_tokens":1,...}}}
//
//	event: content_block_delta ...
//
//	event: message_delta
//	data: {"type":"message_delta","delta":{...},"usage":{"output_tokens":N}}   // cumulative
//
//	event: message_stop
//
// message_start carries the authoritative id, model, and input/cache counts.
// message_delta carries the cumulative output_tokens — take the LAST one.
type anthropicStreamParser struct {
	id, model                                            string
	input, output, cacheRead, cacheWrite5m, cacheWrite1h int
}

func (a *anthropicStreamParser) OnEvent(eventType string, data []byte) {
	switch eventType {
	case "message_start":
		var ms struct {
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					OutputTokens             int `json:"output_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					CacheCreation            *struct {
						Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
						Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
					} `json:"cache_creation"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(data, &ms); err != nil {
			return
		}
		a.id = ms.Message.ID
		a.model = ms.Message.Model
		a.input = ms.Message.Usage.InputTokens
		a.output = ms.Message.Usage.OutputTokens
		a.cacheRead = ms.Message.Usage.CacheReadInputTokens
		// TTL split: nested object wins; legacy stream (no nested object)
		// buckets the rolled-up field as 5m, matching the JSONL parser's
		// fallback policy.
		if ms.Message.Usage.CacheCreation != nil {
			a.cacheWrite5m = ms.Message.Usage.CacheCreation.Ephemeral5m
			a.cacheWrite1h = ms.Message.Usage.CacheCreation.Ephemeral1h
		} else {
			a.cacheWrite5m = ms.Message.Usage.CacheCreationInputTokens
		}
	case "message_delta":
		var md struct {
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &md); err != nil {
			return
		}
		// message_delta's output_tokens is cumulative — last one wins.
		if md.Usage.OutputTokens > 0 {
			a.output = md.Usage.OutputTokens
		}
	}
}

func (a *anthropicStreamParser) Finalise(developer, issueID, host string) *collector.TokenEvent {
	if a.model == "" || (a.input == 0 && a.output == 0) {
		// Stream ended before message_start or with no usable counts.
		return nil
	}
	// Clamp negative usage counts accumulated from the stream (#121) before
	// ComputeCost. Count once per event, not per field.
	if collector.ClampNegativeTokens(&a.input, &a.output, &a.cacheRead, &a.cacheWrite5m, &a.cacheWrite1h) {
		collector.WarnClamp(collector.SourceProxy, a.model)
		collector.RecordClamp(collector.SourceProxy)
	}
	cost, billingMode := store.ComputeCostHost(host, a.model, store.CostUsage{
		Input:        a.input,
		Output:       a.output,
		CacheRead:    a.cacheRead,
		CacheWrite5m: a.cacheWrite5m,
		CacheWrite1h: a.cacheWrite1h,
	})
	return &collector.TokenEvent{
		Developer:      developer,
		IssueID:        issueID,
		Model:          a.model,
		InputTok:       a.input,
		OutputTok:      a.output,
		CacheRead:      a.cacheRead,
		CacheWrite5m:   a.cacheWrite5m,
		CacheWrite1h:   a.cacheWrite1h,
		CostMicro:      cost,
		Fidelity:       collector.FidelityRealtime,
		IdempotencyKey: idempotencyKeyForProxy(ProviderAnthropic, a.id),
		Host:           host,
		BillingMode:    billingMode,
		Timestamp:      time.Now().UTC(),
	}
}

// --- OpenAI streaming parser ---
//
// OpenAI SSE chunks all carry the same `id`. The `usage` field is non-null
// only on the final chunk, and ONLY when the client opted in via
// stream_options.include_usage. Without that opt-in there is no usage block
// in the stream at all — we emit nothing rather than guess.
type openAIStreamParser struct {
	id, model    string
	prompt, comp int
	cached       int
	gotUsage     bool
}

func (o *openAIStreamParser) OnEvent(_ string, data []byte) {
	// OpenAI doesn't use `event:` fields — events are bare `data: {...}`.
	// The literal "[DONE]" terminator carries no JSON.
	if bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	var chunk struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return
	}
	if chunk.ID != "" {
		o.id = chunk.ID
	}
	if chunk.Model != "" {
		o.model = chunk.Model
	}
	if chunk.Usage != nil {
		o.prompt = chunk.Usage.PromptTokens
		o.comp = chunk.Usage.CompletionTokens
		if chunk.Usage.PromptTokensDetails != nil {
			o.cached = chunk.Usage.PromptTokensDetails.CachedTokens
		}
		o.gotUsage = true
	}
}

func (o *openAIStreamParser) Finalise(developer, issueID, host string) *collector.TokenEvent {
	if !o.gotUsage || o.model == "" {
		// stream_options.include_usage was not set, or stream malformed —
		// emit nothing rather than guess at counts.
		return nil
	}
	// Clamp negative usage counts on the RAW accumulated values first (#121),
	// before the carve-out below. The P0-01 cached=min(cached,prompt)
	// subtraction (#114) layers AFTER this clamp so it only handles
	// cached>prompt, never negatives. Count once per event.
	if collector.ClampNegativeTokens(&o.prompt, &o.comp, &o.cached) {
		collector.WarnClamp(collector.SourceProxy, o.model)
		collector.RecordClamp(collector.SourceProxy)
	}
	// OpenAI's prompt_tokens INCLUDES cached_tokens — cached is a subset of
	// prompt, not an additional class (#114). Carve it out here (once, at
	// emission) so Input and CacheRead never overlap; OnEvent keeps accumulating
	// the raw wire values. Clamp cached to prompt (negatives already handled)
	// so a contradictory payload leaves Input at 0 rather than negative.
	cached := o.cached
	if cached > o.prompt {
		cached = o.prompt
	}
	inputTok := o.prompt - cached
	cost, billingMode := store.ComputeCostHost(host, o.model, store.CostUsage{
		Input:     inputTok,
		Output:    o.comp,
		CacheRead: cached,
	})
	return &collector.TokenEvent{
		Developer:      developer,
		IssueID:        issueID,
		Model:          o.model,
		InputTok:       inputTok,
		OutputTok:      o.comp,
		CacheRead:      cached,
		CostMicro:      cost,
		Fidelity:       collector.FidelityRealtime,
		IdempotencyKey: idempotencyKeyForProxy(ProviderOpenAI, o.id),
		Host:           host,
		BillingMode:    billingMode,
		Timestamp:      time.Now().UTC(),
	}
}

// --- Gemini streaming parser ---
//
// Gemini :streamGenerateContent?alt=sse emits a sequence of JSON objects, each
// carrying the same `responseId` and a `usageMetadata` that grows monotonically
// with stream progress. The final chunk carries the cumulative count.
type geminiStreamParser struct {
	responseID, model  string
	prompt, candidates int
	// thoughts (reasoning, billed at output rate) and cached (subset of prompt)
	// accumulate cumulative last-wins like prompt/candidates (#122).
	thoughts, cached int
}

func (g *geminiStreamParser) OnEvent(_ string, data []byte) {
	var chunk struct {
		ResponseID    string `json:"responseId"`
		ModelVersion  string `json:"modelVersion"`
		UsageMetadata *struct {
			PromptTokenCount        int `json:"promptTokenCount"`
			CandidatesTokenCount    int `json:"candidatesTokenCount"`
			ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
			CachedContentTokenCount int `json:"cachedContentTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return
	}
	if chunk.ResponseID != "" {
		g.responseID = chunk.ResponseID
	}
	if chunk.ModelVersion != "" {
		g.model = chunk.ModelVersion
	}
	if chunk.UsageMetadata != nil {
		// Cumulative — last value wins.
		g.prompt = chunk.UsageMetadata.PromptTokenCount
		g.candidates = chunk.UsageMetadata.CandidatesTokenCount
		g.thoughts = chunk.UsageMetadata.ThoughtsTokenCount
		g.cached = chunk.UsageMetadata.CachedContentTokenCount
	}
}

func (g *geminiStreamParser) Finalise(developer, issueID, host string) *collector.TokenEvent {
	// Zero-usage guard includes thoughts (#122): a thoughts-only stream is real
	// billable spend and must not be dropped.
	if g.prompt == 0 && g.candidates == 0 && g.thoughts == 0 {
		return nil
	}
	model := g.model
	if model == "" {
		model = "gemini-unknown"
	}
	// Clamp negative usage counts accumulated from the stream (#121), including
	// the two new counters, before the cache carve-out. Count once per event.
	if collector.ClampNegativeTokens(&g.prompt, &g.candidates, &g.thoughts, &g.cached) {
		collector.WarnClamp(collector.SourceProxy, model)
		collector.RecordClamp(collector.SourceProxy)
	}
	// cachedContentTokenCount is a SUBSET of promptTokenCount — carve it out so
	// Input and CacheRead never overlap (mirrors the OpenAI #114 path). Clamp
	// cached to prompt (negatives already handled) so a contradictory payload
	// leaves Input at 0 rather than negative.
	cached := g.cached
	if cached > g.prompt {
		cached = g.prompt
	}
	inputTok := g.prompt - cached
	// thoughtsTokenCount is billed at the output rate and excluded from
	// candidatesTokenCount, so fold it into Output.
	outputTok := g.candidates + g.thoughts
	cost, billingMode := store.ComputeCostHost(host, model, store.CostUsage{
		Input:     inputTok,
		Output:    outputTok,
		CacheRead: cached,
	})
	return &collector.TokenEvent{
		Developer:      developer,
		IssueID:        issueID,
		Model:          model,
		InputTok:       inputTok,
		OutputTok:      outputTok,
		CacheRead:      cached,
		CostMicro:      cost,
		Fidelity:       collector.FidelityRealtime,
		IdempotencyKey: idempotencyKeyForProxy(ProviderGemini, g.responseID),
		Host:           host,
		BillingMode:    billingMode,
		Timestamp:      time.Now().UTC(),
	}
}

// newStreamCapture wires a fresh streaming intercept for the given provider.
// The returned ReadCloser replaces resp.Body; the proxy's modifyResponse
// hands ownership of upstream-Body to it.
//
// ctx must already be detached from the request context (the caller uses
// context.WithoutCancel) so the deferred sink write survives a mid-stream
// client disconnect.
//
// recordWrite must be non-nil; emit calls it once per attempted store write.
// recordUncaptured must be non-nil; emit calls it with reasonStreamNoUsage when
// the stream closes without usable usage (Finalise returns nil). Both production
// callers pass Proxy methods that carry the nil-recorder tolerance internally —
// so nil-handling lives there, not here.
func newStreamCapture(
	ctx context.Context,
	upstream io.ReadCloser,
	provider Provider,
	developer, issueID, host, repo string,
	sink collector.Ingester,
	recordWrite func(error),
	recordUncaptured func(reason string),
	logger *slog.Logger,
) *streamCapture {
	var parser streamParser
	switch provider {
	case ProviderAnthropic:
		parser = &anthropicStreamParser{}
	case ProviderOpenAI:
		parser = &openAIStreamParser{}
	case ProviderGemini:
		parser = &geminiStreamParser{}
	default:
		// Fail loud rather than silently degrade. The only way to reach here
		// is constructing a Proxy with a Provider value outside the three
		// constants — a programmer error, not a runtime condition.
		panic(fmt.Sprintf("proxy: unknown provider %q for SSE stream capture", provider))
	}
	framer := &sseFramer{onEvent: parser.OnEvent}
	return &streamCapture{
		upstream: upstream,
		framer:   framer,
		parser:   parser,
		emit: func(p streamParser) {
			ev := p.Finalise(developer, issueID, host)
			if ev == nil {
				// Stream closed without usable usage (e.g. OpenAI without
				// stream_options.include_usage) — a 2xx capture miss worth
				// surfacing (#117) rather than a silent nil.
				recordUncaptured(reasonStreamNoUsage)
				return
			}
			// Stamped at the single emit point rather than inside each provider's
			// Finalise: a parser that forgot it would write the zero value, which
			// normalizeRepo silently turns into the sentinel (#231).
			ev.Repo = repo
			err := sink.Ingest(ctx, *ev)
			if err != nil {
				logger.Error("store streaming token event", "err", err)
			}
			recordWrite(err)
		},
	}
}
