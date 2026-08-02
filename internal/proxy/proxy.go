// Package proxy implements the TIER reverse proxy.
//
// Developers point ANTHROPIC_BASE_URL / OPENAI_BASE_URL at this proxy — the two
// providers cmd/tierd actually mounts, gated behind --anthropic-target /
// --openai-target. Every response is intercepted, the usage block extracted, and
// a TokenEvent is written to the store. The original response is forwarded
// unchanged.
//
// A Gemini response parser (parseGemini, ProviderGemini) is present and, since
// #300, stamps the serving Host — but cmd/tierd mounts NO /gemini/ route and has
// no --gemini-target flag, so no Gemini traffic reaches this proxy today.
// Pointing GEMINI_BASE_URL here would 404. Wiring a Gemini capture route is a
// separate product decision, deliberately not done here.
//
// Codex is likewise NOT captured, for a different reason. The Codex CLI speaks
// OpenAI's Responses API, which reports usage as input_tokens / output_tokens
// (nested under `response` in the streaming events). Both OpenAI code paths here
// read the Chat Completions shape instead — parseOpenAI on the JSON path and
// openAIStreamParser on the SSE path Codex actually uses — so a Codex response
// yields no TokenEvent. It is counted under tier_proxy_uncaptured_responses_total
// (reason=stream_no_usage for the default streaming case, reason=parse for a
// non-streaming one), so the gap is visible rather than silent. Codex writes its
// own rollout logs; no tierd collector reads them yet (#459, #463).
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/store"
)

// Provider identifies the AI API format used for response parsing.
type Provider string

const (
	ProviderAnthropic Provider = "anthropic"
	// ProviderOpenAI selects the OpenAI Chat Completions usage shape
	// (usage.prompt_tokens / completion_tokens) — OpenAI and any upstream that
	// returns that same shape. It does NOT cover the Codex CLI; see the package
	// doc for what this proxy cannot capture (#463).
	ProviderOpenAI Provider = "openai"
	ProviderGemini Provider = "gemini"
)

// WriteRecorder records the outcome of one store-write attempt, labelled by
// provider and outcome ("ok"|"error"), so a silent Ingest failure surfaces as a
// metric instead of only a log line (#70, pairs with #67).
// *metrics.CounterVec satisfies it; a nil recorder disables recording.
type WriteRecorder interface {
	Inc(labelValues ...string)
}

const (
	outcomeOK    = "ok"
	outcomeError = "error"
)

// Reasons for tier_proxy_uncaptured_responses_total — one increment per 2xx
// response the proxy intercepted but produced NO TokenEvent from.
const (
	reasonEncoding      = "encoding"        // non-identity Content-Encoding — capture skipped
	reasonParse         = "parse"           // JSON path: non-empty body, parser returned nil
	reasonStreamNoUsage = "stream_no_usage" // SSE path: Finalise returned nil at stream close
	reasonTooLarge      = "too_large"       // JSON path: body past the read cap — forwarded intact, uncaptured (#123)
)

// Internal attribution headers the client sets on the inbound request. They are
// TIER-internal and must never reach the upstream provider: they carry an
// employee identifier and an internal issue number, which are not the provider's
// business. Mirrors the api package's X-Tier-Token strip (ProxyAuth), which
// already removes the auth header before forwarding. Kept as named constants so
// the Rewrite strip and the modifyResponse fallback read cannot drift (#129).
const (
	headerDeveloper = "X-Tier-Developer"
	headerIssue     = "X-Tier-Issue"
	// headerRepo carries the canonical "owner/repo" of the work this request
	// belongs to (#231). The proxy has no other way to learn it: it sees an HTTP
	// request, not a git checkout. Absent -> repoid.Unqualified plus a counter, and
	// the tolerant cost<->outcome join still attaches the cost to the issue.
	//
	// Like the other two it is stripped before forwarding: the repository name is
	// internal information and not the upstream provider's business.
	headerRepo = "X-Tier-Repo"
)

// attribution carries the X-Tier-* values from the Rewrite hook — which reads
// the INBOUND request (r.In) and strips the headers from the OUTBOUND one
// (r.Out) — to modifyResponse, which sees only the outbound request (where the
// headers no longer exist). A context value is the only per-request channel that
// survives both the upstream round trip and the context.WithoutCancel detach
// used for the store write (WithoutCancel preserves values), so it works on the
// SSE path too, where the emit fires at stream Close on the detached context.
type attribution struct{ developer, issue, repo string }

// attrKey is the private context key for the attribution value. A struct{} type
// (not a string) guarantees no collision with any other package's context keys.
type attrKey struct{}

// Proxy wraps httputil.ReverseProxy and adds token usage extraction.
type Proxy struct {
	rp       *httputil.ReverseProxy
	provider Provider
	// host is the SERVING host this proxy forwards to, derived from the --target
	// URL (#300). It is the property that prices an open-weights model — the coarse
	// `provider` parse tag cannot tell OpenRouter from Together from Ollama — and is
	// threaded into every emitted event's Host so ComputeCostHost can key a per-host
	// rate. Empty (no target) stores as the HostUnknown sentinel at insert.
	host         string
	sink         collector.Ingester
	writes       WriteRecorder
	uncaptured   WriteRecorder
	unattributed WriteRecorder
	logger       *slog.Logger
}

// SetUnattributedRecorder installs the recorder for
// tier_proxy_unattributed_total{header}, incremented once per intercepted 2xx
// response per missing X-Tier-* attribution header (developer|issue). A setter
// (not a New parameter) keeps this optional concern out of New's signature; a
// nil recorder — the default — disables recording, matching the writes /
// uncaptured contract. Called at wiring time, before the proxy serves traffic,
// so no concurrent access with recordUnattributed occurs.
func (p *Proxy) SetUnattributedRecorder(rec WriteRecorder) {
	p.unattributed = rec
}

// recordUnattributed bumps tier_proxy_unattributed_total for one intercepted 2xx.
// header is one of "developer", "issue", or "repo" (#231).
// response missing the named X-Tier-* header (developer|issue). Nil-guarded like
// recordWrite / recordUncaptured so tests and `tierd score` (no metrics) are
// no-ops.
func (p *Proxy) recordUnattributed(header string) {
	if p.unattributed == nil {
		return
	}
	p.unattributed.Inc(header)
}

// New creates a new proxy that forwards requests to target and records usage to
// sink, stamping every emitted event's Source with source (callers pass
// collector.SourceProxy). writes may be nil (write outcomes are then not
// counted); uncaptured may be nil (2xx responses that yield no token event are
// then not counted).
//
// Source contract (#46, #337): the proxy's parsers deliberately leave ev.Source
// empty; New wraps the given sink in its OWN ingester.SourceTagger(source, sink)
// so every captured event — JSON and SSE alike — is labelled before it reaches
// the store. Callers pass the BARE backing ingester (e.g. ingester.Store(db))
// plus the source name; there is no separate call-site wrap to forget. Because
// token_events.source is TEXT NOT NULL with no default, an empty source would be
// persisted silently, so New PANICS on an empty source rather than construct a
// proxy that writes unattributable rows. Between the auto-wrap (you cannot forget
// to tag) and the empty-source guard (you cannot tag with nothing), a source-less
// proxy is unconstructable — closing the wiring-trap #337 relocated from the
// parsers to the wiring seam. (A wrong-but-non-empty source is still the caller's
// responsibility; every call site passes collector.SourceProxy.)
// TestProxy_SourceStampedWithoutExternalTagger and TestProxy_NewPanicsOnEmptySource pin it.
func New(target *url.URL, provider Provider, source string, sink collector.Ingester, writes, uncaptured WriteRecorder, logger *slog.Logger) *Proxy {
	if source == "" {
		// Fail loud at wiring time (before any traffic is served) rather than let a
		// mis-wired proxy persist empty-source token_events silently — the exact
		// silent-provenance-drop class #337 exists to eliminate. A programmer error,
		// signalled as a panic like the stdlib's Must* constructors; no call site
		// passes "".
		panic("proxy.New: source must be non-empty (pass collector.SourceProxy); an empty source would persist unattributable token_events (#337)")
	}
	if logger == nil {
		logger = slog.Default()
	}
	// Capture the serving host from the target now (#300). The proxy has no other
	// place to learn it — modifyResponse sees only the response — so it is stamped
	// on the struct once at construction and read on every capture. A nil target
	// (defensive; main.go only constructs New with a parsed target) leaves it empty,
	// which normalizes to the HostUnknown sentinel at insert.
	//
	// Hostname(), NOT Host: the PORT is deliberately dropped so the pricing key is
	// port-independent (`openrouter.ai:443` and `openrouter.ai` price identically,
	// and every self-hosted `localhost:PORT` collapses to one `localhost` basis).
	// Hostname() also strips IPv6 brackets correctly. This is the host-string shape
	// #268 must seed its per-host rate keys against (see store.HostModelKey).
	host := ""
	if target != nil {
		host = target.Hostname()
	}
	// Wrap the caller's bare sink in the proxy's own source tagger (#337). The
	// parsers leave ev.Source empty by design (a shared stamp, not six per-parser
	// assignments — #46); doing the wrap HERE, rather than trusting the call site
	// to pass an already-tagged sink, is what makes an untagged proxy sink
	// unconstructable. SourceTagger stamps a copy of each event (Ingest takes
	// TokenEvent by value), so the parser-set Host/BillingMode/etc. survive.
	p := &Proxy{provider: provider, host: host, sink: ingester.SourceTagger(source, sink), writes: writes, uncaptured: uncaptured, logger: logger}
	// Rewrite + SetURL (Go 1.20+) rather than NewSingleHostReverseProxy
	// (#62). The real defect was the Host header: the Director left the
	// outbound Host as the INBOUND host (e.g. localhost:8080), which
	// virtual-hosted / path-prefixed gateways reject — a failure that
	// presents as a routing problem. SetURL clears Out.Host so net/http
	// derives the Host header from the target URL. (Path joining was
	// never the bug: both forms route through the same rewriteRequestURL
	// and preserve a target base path like /base identically — the test
	// pins that too.) Rewrite mode also strips inbound X-Forwarded-* and,
	// since SetXForwarded is deliberately not called, nothing re-adds
	// them: the upstream provider has no business learning internal
	// developer IPs (the Director mode appended X-Forwarded-For).
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			// Capture attribution from the INBOUND request before anything mutates
			// the outbound one, then strip the internal X-Tier-* headers from the
			// OUTBOUND request so they never reach the provider (#129) — mirroring
			// the api package's X-Tier-Token strip. modifyResponse reads these
			// values from the context below, NOT from the (now-absent) headers, so
			// attribution survives on both the JSON and SSE paths. Stashing before
			// stripping is the whole point: reading resp.Request in modifyResponse
			// after a delete-only fix would see empty strings.
			a := attribution{
				developer: r.In.Header.Get(headerDeveloper),
				issue:     r.In.Header.Get(headerIssue),
				repo:      r.In.Header.Get(headerRepo),
			}
			r.SetURL(target)
			r.Out.Header.Del(headerDeveloper)
			r.Out.Header.Del(headerIssue)
			r.Out.Header.Del(headerRepo)
			r.Out = r.Out.WithContext(context.WithValue(r.Out.Context(), attrKey{}, a))
			// Strip the client's Accept-Encoding so the Go Transport negotiates
			// (and transparently decompresses) any gzip itself. Per the net/http
			// Transport doc, auto-gunzip plus Content-Encoding/Content-Length
			// header removal happen ONLY when the Transport added the
			// "Accept-Encoding: gzip" header — if the client set it, the
			// Transport leaves the compressed body untouched and modifyResponse
			// (and both parsers) see gzip bytes, which json.Unmarshal to nil:
			// silent $0 capture (#117). With the header removed, modifyResponse
			// always sees identity bytes on BOTH the JSON and SSE paths and tier
			// needs zero decompression code. The client then receives an
			// identity-encoded response, which is legal HTTP — a server may
			// always respond without the requested coding.
			r.Out.Header.Del("Accept-Encoding")
		},
	}
	rp.ModifyResponse = p.modifyResponse
	rp.ErrorHandler = p.errorHandler
	// Flush per write so SSE clients receive events as they arrive instead of
	// after the whole response buffers. Go 1.21+ auto-detects text/event-stream
	// and forces -1 internally, but explicit is safer across Go versions and
	// future refactors that might intercept ModifyResponse. The cost on
	// non-streaming JSON responses is negligible (small body, single flush).
	rp.FlushInterval = -1
	p.rp = rp
	return p
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.rp.ServeHTTP(w, r)
}

// modifyResponse intercepts each response, extracts usage, and stores it.
//
// Two paths:
//
//   - JSON (`Content-Type: application/json`) — buffer the body up to 10 MB,
//     parse synchronously, emit one TokenEvent before the response heads to
//     the client. This is the original non-streaming path.
//
//   - SSE (`Content-Type: text/event-stream`) — wrap resp.Body with a
//     streamCapture that forwards bytes to the client unchanged while feeding
//     an SSE framer. The TokenEvent is emitted at body Close (after the
//     client finishes reading or disconnects). This path is what captures
//     Claude Code, which defaults to streaming. (Codex also streams but speaks
//     the Responses API and is not captured; Gemini CLI also streams but no
//     /gemini/ route is mounted — see the package doc.)
func (p *Proxy) modifyResponse(resp *http.Response) error {
	// Only intercept successful API responses.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	// Guard non-identity Content-Encoding. After the Accept-Encoding strip in
	// Rewrite, the Transport decompresses any gzip itself and deletes this
	// header, so in practice this fires only for an upstream that ignored our
	// stripping and forced br/zstd. Those bytes are unparseable here (no
	// decompressor by design), so skip capture, leave the response untouched for
	// byte-identical pass-through, and record the blindness instead of dropping
	// it silently. Rare by construction — one WARN per such response is fine.
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		p.logger.Warn("proxy: skipping capture of response with unhandled Content-Encoding",
			"provider", p.provider, "content_encoding", ce)
		p.recordUncaptured(reasonEncoding)
		return nil
	}
	ct := resp.Header.Get("Content-Type")

	// Attribution comes from the context carrier set in Rewrite (authoritative),
	// which stashed the values before stripping the X-Tier-* headers from the
	// outbound request (#129). The header read is a defensive fallback only: it
	// keeps any direct-construction path (a test wiring modifyResponse without
	// going through Rewrite) working, and documents that context is primary. In
	// the normal proxy flow the headers are already gone here, so the fallback is
	// a no-op and the context value wins.
	a, _ := resp.Request.Context().Value(attrKey{}).(attribution)
	developer, issueID, repo := a.developer, a.issue, a.repo
	if developer == "" {
		developer = requestHeader(resp.Request, headerDeveloper)
	}
	if issueID == "" {
		issueID = requestHeader(resp.Request, headerIssue)
	}
	if repo == "" {
		repo = requestHeader(resp.Request, headerRepo)
	}
	if developer == "" {
		// A proxied event with no developer would otherwise store "" and vanish
		// from every per-developer aggregate. Default to the same "unattributed"
		// sentinel as the missing-issue case below (symmetric, #129) and surface
		// the misconfigured client via the counter instead of silently polluting.
		developer = collector.UnattributedIssueID
		p.recordUnattributed("developer")
	}
	if issueID == "" {
		// Shared with the JSONL join fallback so the two capture paths cannot
		// drift about the same dollar (spec section 4.4, #120).
		issueID = collector.UnattributedIssueID
		p.recordUnattributed("issue")
	}
	// #231: canonicalize, or fall back to the sentinel + counter. A client that never
	// sets X-Tier-Repo is not broken — the tolerant join still attaches its cost to
	// the issue — but a multi-repo org needs the counter to see WHY its issues sharing
	// a number still fuse. Never guess a repo the client did not send.
	if slug, ok := repoid.Canonical(repo); ok {
		repo = slug
	} else {
		if repo != "" {
			// The rejected value is NEVER logged. X-Tier-Repo is an unauthenticated,
			// attacker-controlled request header: echoing it into a structured log is
			// log injection (an embedded newline forges a log entry) and clear-text
			// logging of request-header content, both flagged by CodeQL. The length is
			// enough to tell a truncated slug from a wholly wrong one, and the counter
			// below is the signal an operator actually alerts on.
			p.logger.Warn("ignoring non-canonical X-Tier-Repo header",
				"value_len", len(repo),
				"hint", `expected a canonical "owner/repo" slug`)
		}
		repo = repoid.Unqualified
		p.recordUnattributed("repo")
	}
	// Detached context so DB writes survive a client disconnect mid-stream.
	storeCtx := context.WithoutCancel(resp.Request.Context())

	switch {
	case strings.Contains(ct, "text/event-stream"):
		// Replace the body with a teeing wrapper; emit fires at Close.
		resp.Body = newStreamCapture(storeCtx, resp.Body, p.provider, developer, issueID, p.host, repo, p.sink, p.recordWrite, p.recordUncaptured, p.logger)
		return nil

	case strings.Contains(ct, "application/json"):
		return p.handleJSON(resp, developer, issueID, repo, storeCtx)

	default:
		// Unknown content type — pass through untouched.
		return nil
	}
}

// handleJSON is the original buffer-and-parse path, factored out so
// modifyResponse can branch on Content-Type cleanly.
func (p *Proxy) handleJSON(resp *http.Response, developer, issueID, repo string, storeCtx context.Context) error {
	// Limit response buffering to 10 MB — large enough for any usage JSON, small
	// enough to avoid OOM on malformed or adversarial responses. Read one byte
	// PAST the cap so an over-cap body is distinguishable from one that lands
	// exactly at it (io.LimitReader would otherwise return maxBody bytes with a
	// nil error either way, hiding the truncation).
	const maxBody = 10 << 20 // 10 MB
	origBody := resp.Body
	body, err := io.ReadAll(io.LimitReader(origBody, maxBody+1))

	switch {
	case err != nil:
		// Upstream read failed mid-body (e.g. connection reset). The buffered
		// prefix is NOT the whole response, so forwarding it under a 200 would be
		// silent corruption of a pass-through callers must be able to trust.
		// Close the upstream body and return a proxy error: ReverseProxy routes
		// it to errorHandler, which emits a 502 (this runs before any client
		// write, so the 502 is always cleanly writable). ReverseProxy also closes
		// resp.Body (still origBody here) when ModifyResponse errors, so this is a
		// deliberate, idempotent double-close: net/http response bodies guard Close
		// with a closed flag, and closing here keeps the branch self-evidently
		// correct in isolation.
		_ = origBody.Close()
		return fmt.Errorf("proxy: read upstream response body: %w", err)

	case len(body) > maxBody:
		// Response is larger than the capture cap: too big to parse for usage,
		// but the proxy still owes the client the EXACT upstream bytes. Reconstruct
		// the stream losslessly by re-attaching the buffered prefix ahead of the
		// still-unread remainder via io.MultiReader, and keep the ORIGINAL Closer:
		// origBody is not closed in this branch, so the upstream connection is
		// released only when ReverseProxy finishes copying (closing a NopCloser
		// would leave the real body's connection dangling). Headers and
		// ContentLength are left untouched so the wire bytes match the upstream
		// byte-for-byte. Skip parsing, count the miss, and pass through.
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(body), origBody), origBody}
		p.recordUncaptured(reasonTooLarge)
		return nil
	}

	// Normal path: the full body is buffered. Close the upstream and restore the
	// buffered bytes for the client. Setting ContentLength here is a no-op for a
	// well-formed Content-Length upstream (len(body) already equals it) and
	// repairs chunked responses where ContentLength was -1.
	_ = origBody.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))

	var ev *collector.TokenEvent
	switch p.provider {
	case ProviderAnthropic:
		ev = parseAnthropic(body, developer, issueID, p.host)
	case ProviderOpenAI:
		ev = parseOpenAI(body, developer, issueID, p.host)
	case ProviderGemini:
		ev = parseGemini(body, developer, issueID, p.host)
	}
	if ev == nil {
		// A non-empty 2xx JSON body the parser rejected is a capture miss worth
		// surfacing (#117) — otherwise it is indistinguishable from "no AI work".
		// An empty body (len 0) carried no usage to begin with, so it is not a
		// parse failure and is excluded.
		if len(body) > 0 {
			p.recordUncaptured(reasonParse)
		}
		return nil
	}
	// Stamped here, at the single emit point, rather than threaded through each
	// provider parser: a parser that forgot it would silently write the zero value
	// and normalizeRepo would quietly turn that into the sentinel (#231).
	ev.Repo = repo
	writeErr := p.sink.Ingest(storeCtx, *ev)
	if writeErr != nil {
		p.logger.Error("store token event", "err", writeErr)
	}
	p.recordWrite(writeErr)
	return nil
}

// recordWrite bumps the write-outcome counter for one Ingest attempt.
// Called once per attempted store write on both the JSON and SSE paths; a nil
// recorder (e.g. in tests) is a no-op. Note: proxy store writes are synchronous
// within the request lifecycle — handleJSON runs inside modifyResponse and the
// SSE emit runs inside streamCapture.Close — so srv.Shutdown's in-flight-request
// drain already flushes them on SIGTERM; no separate write registry is needed.
// A future refactor that moves writes onto a goroutine MUST add an explicit
// drain (WaitGroup + Shutdown hook), or events in flight at shutdown are lost.
func (p *Proxy) recordWrite(err error) {
	if p.writes == nil {
		return
	}
	outcome := outcomeOK
	if err != nil {
		outcome = outcomeError
	}
	p.writes.Inc(string(p.provider), outcome)
}

// recordUncaptured bumps tier_proxy_uncaptured_responses_total for one 2xx
// response the proxy intercepted but produced no TokenEvent from, labelled by
// provider and reason (encoding|parse|stream_no_usage|too_large). Kept parallel to
// recordWrite so the nil-recorder tolerance (tests, `tierd score`) lives in one
// place and callers — including the SSE emit closure — pass p.recordUncaptured
// as a method value.
func (p *Proxy) recordUncaptured(reason string) {
	if p.uncaptured == nil {
		return
	}
	p.uncaptured.Inc(string(p.provider), reason)
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	// r.URL.Path is percent-decoded from the client's request target, so it can
	// carry embedded CR/LF (e.g. "/v1/messages%0a...") — an attacker-controlled,
	// CRLF-injectable field. Route it through the shared logsafe barrier so a
	// forged newline cannot spawn a standalone log record (#321, go/log-injection).
	// err is the transport error, not attacker-controlled, and is logged verbatim.
	p.logger.Error("proxy error", "err", err, "path", logsafe.Str(r.URL.Path))
	http.Error(w, "proxy error", http.StatusBadGateway)
}

// requestHeader reads a value from a request header, returning "" if absent.
func requestHeader(r *http.Request, header string) string {
	return r.Header.Get(header)
}

// idempotencyKeyForProxy computes the cross-source dedup key for a proxy event.
//
// When responseID is empty (older API versions, malformed bodies, or Vertex AI
// non-streaming Gemini in some regions) the key is empty too. The event still
// inserts: store.InsertTokenEvent rewrites the empty string to SQL NULL via
// NULLIF, and the partial unique index on idempotency_key does not enforce
// uniqueness over NULL rows. Those rows won't dedup against repeats, which is
// acceptable because no other source produces a matching key either.
//
// As of #19 this delegates to collector.MessageIdempotencyKey — the same
// helper the JSONL collector calls when extracting `message.id` from a
// session file. Result: a Claude call captured by both paths produces an
// identical key and dedupes at the SQLite partial unique index.
//
// The provider arg is still kept so a numeric id collision across vendors
// (msg_42 from Anthropic vs. chatcmpl-42 from OpenAI — improbable but
// structurally possible) is namespaced away.
func idempotencyKeyForProxy(provider Provider, responseID string) string {
	return collector.MessageIdempotencyKey(string(provider), responseID)
}

// --- Anthropic response parser ---

type anthropicResponse struct {
	ID    string `json:"id"` // msg_<...> — the cross-source dedup anchor
	Model string `json:"model"`
	Usage struct {
		InputTokens int `json:"input_tokens"`
		// CacheCreationInputTokens is the rolled-up cache-write count. The
		// nested CacheCreation object splits it by TTL bucket when present.
		// Legacy responses (pre-1h-feature) omit CacheCreation; we bucket
		// the rolled-up number as 5m in that case.
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreation            *struct {
			Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
			Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
	} `json:"usage"`
}

func parseAnthropic(body []byte, developer, issueID, host string) *collector.TokenEvent {
	var r anthropicResponse
	if err := json.Unmarshal(body, &r); err != nil || r.Model == "" {
		return nil
	}
	if r.Usage.InputTokens == 0 && r.Usage.OutputTokens == 0 {
		return nil
	}
	// TTL split: nested object wins; otherwise bucket the rolled-up legacy
	// field as 5m (matches Anthropic's pre-1h-feature default).
	var w5m, w1h int
	if r.Usage.CacheCreation != nil {
		w5m = r.Usage.CacheCreation.Ephemeral5m
		w1h = r.Usage.CacheCreation.Ephemeral1h
	} else {
		w5m = r.Usage.CacheCreationInputTokens
	}
	// Clamp negative usage counts at the wire boundary (#121) — after the TTL
	// split, before ComputeCost — so a hostile/malformed body can't push a
	// negative cost into the store. Count once per event, not per field.
	if collector.ClampNegativeTokens(&r.Usage.InputTokens, &r.Usage.OutputTokens, &r.Usage.CacheReadInputTokens, &w5m, &w1h) {
		collector.WarnClamp(collector.SourceProxy, r.Model)
		collector.RecordClamp(collector.SourceProxy)
	}
	cost, billingMode := store.ComputeCostHost(host, r.Model, store.CostUsage{
		Input:        r.Usage.InputTokens,
		Output:       r.Usage.OutputTokens,
		CacheRead:    r.Usage.CacheReadInputTokens,
		CacheWrite5m: w5m,
		CacheWrite1h: w1h,
	})
	return &collector.TokenEvent{
		Developer:      developer,
		IssueID:        issueID,
		Model:          r.Model,
		InputTok:       r.Usage.InputTokens,
		OutputTok:      r.Usage.OutputTokens,
		CacheRead:      r.Usage.CacheReadInputTokens,
		CacheWrite5m:   w5m,
		CacheWrite1h:   w1h,
		CostMicro:      cost,
		Fidelity:       collector.FidelityRealtime,
		IdempotencyKey: idempotencyKeyForProxy(ProviderAnthropic, r.ID),
		Host:           host,
		BillingMode:    billingMode,
		Timestamp:      time.Now().UTC(),
	}
}

// --- OpenAI-compatible response parser (OpenAI, and any upstream returning the same Chat Completions usage shape) ---

type openAIResponse struct {
	ID    string `json:"id"` // chatcmpl-<...> — the cross-source dedup anchor
	Model string `json:"model"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// parseOpenAI builds a TokenEvent from an OpenAI-compatible (OpenAI, and any
// upstream returning the same shape) Chat Completions response. It does NOT
// handle the Responses API, which the Codex CLI speaks — see the package doc. OpenAI reports prompt_tokens
// INCLUSIVE of prompt_tokens_details.cached_tokens, so cached is carved out of
// the input class before pricing: the emitted InputTok is prompt_tokens −
// cached_tokens and CacheRead is cached_tokens, two non-overlapping classes.
func parseOpenAI(body []byte, developer, issueID, host string) *collector.TokenEvent {
	var r openAIResponse
	if err := json.Unmarshal(body, &r); err != nil || r.Model == "" {
		return nil
	}
	if r.Usage.PromptTokens == 0 && r.Usage.CompletionTokens == 0 {
		return nil
	}
	cached := 0
	if r.Usage.PromptTokensDetails != nil {
		cached = r.Usage.PromptTokensDetails.CachedTokens
	}
	// Clamp negative usage counts on the RAW wire values first (#121), before
	// the carve-out below. The P0-01 cached=min(cached,prompt) subtraction
	// (#114) layers AFTER this clamp so it only ever has to reason about
	// cached>prompt, never negatives. Count once per event.
	if collector.ClampNegativeTokens(&r.Usage.PromptTokens, &r.Usage.CompletionTokens, &cached) {
		collector.WarnClamp(collector.SourceProxy, r.Model)
		collector.RecordClamp(collector.SourceProxy)
	}
	// OpenAI's prompt_tokens INCLUDES cached_tokens — cached is a subset of
	// prompt, not an additional class (#114). Carve it out so the two classes
	// never overlap: Input is the fresh (non-cached) prompt, CacheRead is the
	// cached remainder. Clamp cached to [0, prompt] (negatives already handled
	// above) so a contradictory cached>prompt payload leaves Input at 0 rather
	// than going negative. This matches Anthropic, whose input_tokens already
	// excludes cache classes.
	if cached > r.Usage.PromptTokens {
		cached = r.Usage.PromptTokens
	}
	inputTok := r.Usage.PromptTokens - cached
	// OpenAI has no cache-write SKU — both 5m and 1h buckets stay zero, and
	// ComputeCost applies the OpenAI 0.5× multiplier to `cached`.
	cost, billingMode := store.ComputeCostHost(host, r.Model, store.CostUsage{
		Input:     inputTok,
		Output:    r.Usage.CompletionTokens,
		CacheRead: cached,
	})
	return &collector.TokenEvent{
		Developer:      developer,
		IssueID:        issueID,
		Model:          r.Model,
		InputTok:       inputTok,
		OutputTok:      r.Usage.CompletionTokens,
		CacheRead:      cached,
		CostMicro:      cost,
		Fidelity:       collector.FidelityRealtime,
		IdempotencyKey: idempotencyKeyForProxy(ProviderOpenAI, r.ID),
		Host:           host,
		BillingMode:    billingMode,
		Timestamp:      time.Now().UTC(),
	}
}

// --- Gemini response parser ---

type geminiResponse struct {
	// ResponseID is present on the public Gemini v1 API
	// (generativelanguage.googleapis.com:generateContent). Vertex AI's
	// equivalent (aiplatform.googleapis.com) may omit it on non-streaming
	// responses in some regions/versions — an empty value falls through to the
	// NULL idempotency-key path (no dedup, but the row still lands).
	ResponseID    string `json:"responseId"`
	ModelVersion  string `json:"modelVersion"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		// ThoughtsTokenCount is reasoning ("thinking") usage. Gemini bills it at
		// the OUTPUT rate and EXCLUDES it from CandidatesTokenCount, so it must be
		// folded into Output or all reasoning spend is dropped (#122).
		ThoughtsTokenCount int `json:"thoughtsTokenCount"`
		// CachedContentTokenCount is a SUBSET of PromptTokenCount (same shape as
		// OpenAI's cached_tokens): carve it out of Input and discount it at the
		// model's cache_read_mult (#122).
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
		TotalTokenCount         int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

func parseGemini(body []byte, developer, issueID, host string) *collector.TokenEvent {
	var r geminiResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil
	}
	um := &r.UsageMetadata
	// Zero-usage guard includes thoughts (#122): a thoughts-only response is real
	// billable spend and must not be dropped as empty.
	if um.PromptTokenCount == 0 && um.CandidatesTokenCount == 0 && um.ThoughtsTokenCount == 0 {
		return nil
	}
	model := r.ModelVersion
	if model == "" {
		model = "gemini-unknown"
	}
	// Clamp negative usage counts on the RAW wire values first (#121), including
	// the two new counters, before the cache carve-out below. Count once per event.
	if collector.ClampNegativeTokens(&um.PromptTokenCount, &um.CandidatesTokenCount, &um.ThoughtsTokenCount, &um.CachedContentTokenCount) {
		collector.WarnClamp(collector.SourceProxy, model)
		collector.RecordClamp(collector.SourceProxy)
	}
	// cachedContentTokenCount is INCLUDED in promptTokenCount, so carve it out so
	// Input and CacheRead never overlap (mirrors the OpenAI #114 pattern). Clamp
	// cached to prompt (negatives already handled) so a contradictory payload
	// leaves Input at 0 rather than negative.
	cached := um.CachedContentTokenCount
	if cached > um.PromptTokenCount {
		cached = um.PromptTokenCount
	}
	inputTok := um.PromptTokenCount - cached
	// thoughtsTokenCount is billed at the output rate and excluded from
	// candidatesTokenCount, so OutputTok stores billing-equivalent output tokens.
	outputTok := um.CandidatesTokenCount + um.ThoughtsTokenCount
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
		IdempotencyKey: idempotencyKeyForProxy(ProviderGemini, r.ResponseID),
		Host:           host,
		BillingMode:    billingMode,
		Timestamp:      time.Now().UTC(),
	}
}
