// Package proxy implements the TIER reverse proxy.
//
// Developers point ANTHROPIC_BASE_URL / OPENAI_BASE_URL at this proxy — three
// providers cmd/tierd mounts, gated behind --anthropic-target / --openai-target
// / --gemini-target. Gemini authenticates upstream with an `x-goog-api-key`
// header or a `?key=` query parameter rather than an env-var base-URL swap; the
// proxy forwards either unchanged, but the HEADER is the form to recommend —
// a `?key=` value rides in the request line and can be logged by anything
// sitting in front of tierd (see README's proxy section and
// docs/how-it-works.md). Every response is intercepted, the usage block
// extracted, and a TokenEvent is written to the store. The original response is
// forwarded unchanged.
//
// Gemini's route (#459 task 4) is structurally complete — mounted through the
// same registerProxy path as /anthropic/ and /openai/, onto the parseGemini /
// geminiStreamParser pair that has existed since v1 (#1) and gained
// thinking/cache-token handling in #122 and host stamping in #300 — but it has
// never carried live traffic: no request has round-tripped through it to a real
// generativelanguage.googleapis.com response. Treat it as proven-by-parser-unit-
// tests, not proven-by-wire, until a live run exercises it (see the
// TIER_LIVE_GEMINI_KEY-gated test in internal/integration).
//
// The OpenAI route carries TWO wire shapes and parses both (#459 task 2). Chat
// Completions reports usage as prompt_tokens / completion_tokens; the Responses
// API (/v1/responses — what the Codex CLI speaks) reports input_tokens /
// output_tokens with input_tokens_details.cached_tokens, nested under `response`
// in the streaming events. Discrimination is on the PAYLOAD, never the path:
// openAIShape on the JSON path, the `response.*` SSE event name on the streaming
// path. A body that is not positively identified as the Responses shape parses
// exactly as it did before, so nothing about the Chat Completions path moved.
//
// ⚠️ What that does and does NOT claim. The Responses parser is an ENHANCEMENT
// covering Responses-API traffic generally; it is not what makes Codex spend
// measurable. Codex is already captured — from its own local rollout logs, by
// internal/collector/codexrollout under `--codex-rollout` (#464), which is the
// path verified against real captured sessions and remains the supported one
// (it needs no base-URL change and works with ChatGPT-subscription auth, which
// never traverses this proxy). This parser has never round-tripped live
// Responses traffic; #459 task 3 (credential-blocked) is what would prove it on
// the wire. Anything it cannot parse still lands in
// tier_proxy_uncaptured_responses_total (reason=stream_no_usage on the streaming
// path, reason=parse on the JSON one), so a gap stays visible rather than silent.
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
	// ProviderOpenAI selects OpenAI's usage shapes — BOTH Chat Completions
	// (usage.prompt_tokens / completion_tokens), which every OpenAI-compatible
	// upstream also returns, and the Responses API (usage.input_tokens /
	// output_tokens, #459 task 2). One provider tag, two shapes, discriminated
	// per response body; see the package doc.
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

// isReservedIdentifier reports whether a client-supplied X-Tier-* attribution value
// looks like the server-assigned unattributed sentinel family — the bare sentinel or
// any "unattributed:<reason>" sub-bucket, compared case-INSENSITIVELY. Deliberately
// wider than the exact read-side matcher (store.IsUnattributed): at ingest the safe
// move is to refuse anything that resembles the sentinel, so a forged case variant
// never becomes a stored row.
//
// 🔴 IT DELEGATES — it does NOT restate the rule. Under #466 this was a hand-copied
// four-liner justified by "the proxy cannot import internal/api". That was a false
// dichotomy: the rule belongs to NEITHER consumer, it belongs beside the sentinel it
// is about, and internal/store imports only logsafe and repoid so it was already in
// this package's dependency graph — there was never a cycle to avoid. The #466
// postmortem is itself a story about a matcher silently drifting from its twin while
// every shared constant still matched, so a third copy was the one thing not to ship.
//
// It is NOT issue-specific (it was named isReservedIssueID under #466, when only
// X-Tier-Issue was guarded). The sentinel names two columns and modifyResponse now
// screens BOTH headers through it — X-Tier-Issue (#466) and X-Tier-Developer (#619).
func isReservedIdentifier(s string) bool {
	return store.ResemblesUnattributed(s)
}

// recordUnattributed bumps tier_proxy_unattributed_total for one intercepted 2xx
// response missing (or forging) the named X-Tier-* attribution header.
//
// header is one of "developer", "developer-forged" (#619), "issue", "issue-forged"
// (#466) or "repo" (#231) — a fixed, low-cardinality set; no client-supplied value
// ever becomes a label. Each "-forged" variant is deliberately DISTINCT from its
// plain form: the stored row is identical either way, so without separate labels an
// operator cannot tell a misconfigured client from one asserting the server-assigned
// sentinel, and the guard would make forging exactly as effective as omission.
// "developer-forged" is the one to alert on — a forged developer is the only one of
// the four that RAISES the sender's own score. Nil-guarded like
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
//     Claude Code, which defaults to streaming. (The Responses API streams too,
//     and is parsed here since #459 task 2 — though never yet against live
//     traffic, see the package doc; Gemini CLI also streams, and its
//     /gemini/ route + geminiStreamParser are mounted since #459 task 4, but
//     unlike Anthropic/OpenAI this path has never carried live traffic — see
//     the package doc.)
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
		// ce is a free-form UPSTREAM-controlled response header. It is not one of
		// the provably-constrained classes docs/security.md §8 licenses to be
		// logged bare (issue refs, hex SHAs, the webhook event allowlist,
		// validated periods, numerics), so it goes through the shared logsafe
		// barrier: an upstream returning a multi-kilobyte header value would
		// otherwise flood one log line, and the length cap is the live win here
		// (slog's handlers already escape CR/LF in an attribute value — see
		// docs/security.md §8 — so the strip is defense in depth for a
		// non-slog consumer, not the thing stopping a forged record today).
		//
		// ⚠️ This is DELIBERATELY NOT the posture the X-Tier-Repo reject path
		// takes ~45 lines below in this same function. That one logs only
		// len(repo) and never the value, because the rejected header is
		// attacker-supplied and carries no diagnostic worth the risk. Here the
		// value IS the diagnostic — an operator needs to see "br" or "zstd" to
		// know which codec defeated the Accept-Encoding strip — so it is
		// sanitized and kept rather than dropped. Same threat, different
		// cost/benefit, and the two must not be "made consistent" by wrapping
		// one or dropping the other (#321, go/log-injection).
		p.logger.Warn("proxy: skipping capture of response with unhandled Content-Encoding",
			"provider", p.provider, "content_encoding", logsafe.Str(ce))
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
	// A client may not FORGE the sentinel via X-Tier-Developer (#619) any more than via
	// X-Tier-Issue (#466 — see below). This header is the worse of the two to leave
	// open: a forged issue moves a dollar between buckets inside the sender's own
	// denominator, while a forged developer moves it out of that denominator entirely
	// and RAISES the sender's score.
	//
	// Treated exactly as a MISSING header, never as an error, for the same reason the
	// issue half is: the proxy sits on the request path and must never fail a provider
	// call over attribution metadata. The stored value is identical either way — the
	// difference is that the counter records it as unresolved, which is the truth. The
	// rejected value is never logged (same rule as X-Tier-Repo below).
	forgedDeveloper := developer != "" && isReservedIdentifier(developer)
	if forgedDeveloper {
		developer = ""
	}
	if developer == "" {
		// A proxied event with no developer would otherwise store "" and vanish
		// from every per-developer aggregate. Default to the same "unattributed"
		// sentinel as the missing-issue case below (symmetric, #129) and surface
		// the misconfigured client via the counter instead of silently polluting.
		developer = collector.UnattributedIssueID
		// Counted under a DISTINCT label when the header was forged rather than
		// absent (#619), mirroring issue-forged (#466): the stored row is identical
		// either way, so without separate labels an operator cannot tell a
		// misconfigured client from one asserting the server-assigned sentinel, and
		// the guard would make forging exactly as effective as omission.
		if forgedDeveloper {
			p.recordUnattributed("developer-forged")
		} else {
			p.recordUnattributed("developer")
		}
	}
	// A client may not FORGE the sentinel via X-Tier-Issue (#466). The header is
	// unauthenticated and taken verbatim, so without this a caller could assert
	// "unattributed" (or a labeled sub-bucket, or a case variant of either) about
	// spend it is otherwise attributing to itself, moving its own dollars out of the
	// #466 no-outcome gap and out of the attributed side of the #234 coverage split.
	// Treated exactly as a MISSING issue rather than as an error: the proxy sits on
	// the request path and must never fail a provider call over attribution metadata,
	// and the outcome is the same value the server would have assigned anyway — the
	// difference is that the counter now records it as unresolved, which is the truth.
	// The rejected value is never logged (same rule as X-Tier-Repo below).
	forgedIssue := issueID != "" && isReservedIdentifier(issueID)
	if forgedIssue {
		issueID = ""
	}
	if issueID == "" {
		// Shared with the JSONL join fallback so the two capture paths cannot
		// drift about the same dollar (spec section 4.4, #120).
		issueID = collector.UnattributedIssueID
		// Counted under a DISTINCT label when the header was forged rather than
		// absent (#466). The stored row is identical either way, so without separate
		// labels an operator cannot tell "client is misconfigured" from "client is
		// asserting the sentinel", and the guard would make forging exactly as
		// effective as omission instead of less.
		if forgedIssue {
			p.recordUnattributed("issue-forged")
		} else {
			p.recordUnattributed("issue")
		}
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

// --- OpenAI-compatible response parsers (OpenAI, and any upstream returning one of its two usage shapes) ---

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

// openAIWireShape names which of OpenAI's two usage shapes a body carries. Both
// arrive on the SAME route (a client points OPENAI_BASE_URL at /openai/ and then
// calls whichever endpoint it likes), so the shape has to come from the payload,
// never from the path or the provider tag.
type openAIWireShape int

const (
	// shapeChatCompletions is the DEFAULT: /v1/chat/completions and every
	// OpenAI-compatible upstream that mimics it (xAI, DeepSeek, OpenRouter,
	// Together, Ollama). Anything not positively identified as the Responses
	// shape lands here, which is what keeps this change additive — no body that
	// parsed before is routed anywhere new.
	shapeChatCompletions openAIWireShape = iota
	// shapeResponses is /v1/responses — the endpoint the Codex CLI speaks.
	shapeResponses
)

// openAIShape discriminates the two shapes from the body alone (#459 task 2).
//
// Three rules, in order:
//
//  1. `"object"` is authoritative when present: "response" -> Responses,
//     "chat.completion" -> Chat Completions. OpenAI itself always sets it.
//  2. Otherwise the usage KEYS decide: input_tokens/output_tokens present AND
//     prompt_tokens/completion_tokens absent -> Responses. Key PRESENCE (via
//     *int), not value, so a genuine zero count is not read as "absent" — and
//     the mutual exclusion means a hybrid body from some compatible upstream
//     stays on the Chat path it already worked on.
//  3. Anything else -> Chat Completions, the pre-#459 behaviour.
//
// The leading bytes.Contains pair is a fast path that keeps the common Chat
// Completions response at exactly one unmarshal, as before. It is outcome-
// neutral, but only because it screens for BOTH count keys: a body carrying
// neither cannot satisfy rule 2, and cannot satisfy rule 1's "response" case
// either in any way that matters, because a Responses object with no
// input_tokens AND no output_tokens has no usage worth parsing — both parsers
// return nil for it.
//
// ⚠️ Screening on "input_tokens" ALONE would not be neutral, and an earlier
// draft of this function had exactly that bug:
// {"object":"response","model":"m","usage":{"output_tokens":500}} would have
// skipped the probe, routed to Chat Completions, and captured nothing, while
// rules 1-3 say Responses and parseOpenAIResponses prices it. Not a shape OpenAI
// emits — but the fast path must not be able to decide anything the rules would
// decide differently, or it is a fourth rule pretending to be an optimisation.
func openAIShape(body []byte) openAIWireShape {
	if !bytes.Contains(body, []byte(`"input_tokens"`)) && !bytes.Contains(body, []byte(`"output_tokens"`)) {
		return shapeChatCompletions
	}
	var probe struct {
		Object string `json:"object"`
		Usage  *struct {
			InputTokens      *int `json:"input_tokens"`
			OutputTokens     *int `json:"output_tokens"`
			PromptTokens     *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return shapeChatCompletions
	}
	switch probe.Object {
	case "response":
		return shapeResponses
	case "chat.completion":
		return shapeChatCompletions
	}
	if probe.Usage == nil {
		return shapeChatCompletions
	}
	if (probe.Usage.InputTokens != nil || probe.Usage.OutputTokens != nil) &&
		probe.Usage.PromptTokens == nil && probe.Usage.CompletionTokens == nil {
		return shapeResponses
	}
	return shapeChatCompletions
}

// parseOpenAI builds a TokenEvent from an OpenAI-compatible JSON response of
// EITHER shape, dispatching on openAIShape. It is the single JSON entry point
// for ProviderOpenAI; handleJSON does not know there are two shapes.
func parseOpenAI(body []byte, developer, issueID, host string) *collector.TokenEvent {
	if openAIShape(body) == shapeResponses {
		return parseOpenAIResponses(body, developer, issueID, host)
	}
	return parseOpenAIChat(body, developer, issueID, host)
}

// parseOpenAIChat builds a TokenEvent from a Chat Completions response (OpenAI,
// and any upstream returning the same shape). OpenAI reports prompt_tokens
// INCLUSIVE of prompt_tokens_details.cached_tokens, so cached is carved out of
// the input class before pricing: the emitted InputTok is prompt_tokens −
// cached_tokens and CacheRead is cached_tokens, two non-overlapping classes.
func parseOpenAIChat(body []byte, developer, issueID, host string) *collector.TokenEvent {
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
	return openAITokenEvent(developer, issueID, host, r.ID, r.Model,
		r.Usage.PromptTokens, r.Usage.CompletionTokens, cached)
}

// --- OpenAI Responses API parser (/v1/responses — the endpoint the Codex CLI speaks) ---

// openAIResponsesUsage is the Responses-API usage block.
//
// 🔴 THE MONEY QUESTION — input_tokens is INCLUSIVE of
// input_tokens_details.cached_tokens, not additive. Reading it the other way
// would bill the cached prefix at BOTH the input rate and the cache-read rate
// (an ~11x overcharge on a fully-cached gpt-5.x call, whose cache_read_mult is
// 0.10). The evidence, strongest first:
//
//   - In-tree, from REAL data: internal/collector/codexrollout parses the same
//     usage numbers out of Codex's own rollout logs, and its checkContainment
//     asserts total_tokens == input_tokens + output_tokens AND
//     cached_input_tokens <= input_tokens on every event of every captured
//     session, without ever tripping. Take the first event of
//     testdata/rollout-duplicate-token-count.jsonl: input=17011, cached=11008,
//     output=118, total=17129 — and 17011+118 == 17129 exactly, with cached
//     nonzero. That EXCLUDES the additive reading in which total_tokens counts
//     every billable token, which would have to report 28137 here.
//
//     Be precise about what that does NOT exclude: a reading where cached sits
//     outside input AND outside total would satisfy both identities too. It is
//     ruled out by the field's name rather than by the data, so this is strong
//     evidence, not a proof — exactly the hedge parse.go already makes about its
//     sibling field ("total == input + output is CONSISTENT with that (it does
//     not prove it)"). What closes the residual is that the rest of the tree
//     already PRICES Codex on the inclusive reading (parse.go's deltaFrom) and
//     the Chat Completions precedent below; this parser agreeing with that
//     collector is what keeps identical traffic priced identically.
//
//   - Shape symmetry: `input_tokens_details` is a BREAKDOWN of its parent, the
//     same relationship Chat Completions' prompt_tokens_details.cached_tokens
//     has to prompt_tokens (#114) and Gemini's cachedContentTokenCount has to
//     promptTokenCount (#122). All three vendors' "details" objects decompose;
//     none of them extend.
//
// ⚠️ Not verified against a live /v1/responses round trip — no key here. #459
// task 3 (credential-blocked) is what would close that.
//
// output_tokens_details.reasoning_tokens is deliberately NOT a field here. It is
// a SUBSET of output_tokens (checkContainment asserts reasoning <= output on the
// same captured data), so reading it and adding it would bill reasoning twice.
// This is the OPPOSITE of Gemini's thoughtsTokenCount, which is EXCLUDED from
// candidatesTokenCount and so must be folded in (#122) — the two look alike and
// must not be "made consistent".
type openAIResponsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

type openAIResponsesBody struct {
	ID    string `json:"id"` // resp_<...> — the cross-source dedup anchor
	Model string `json:"model"`
	// Usage is a POINTER: the Responses object carries `"usage": null` until the
	// response reaches a terminal status, and a null usage is "nothing to bill",
	// not "zero tokens".
	Usage *openAIResponsesUsage `json:"usage"`
}

// parseOpenAIResponses builds a TokenEvent from a non-streaming Responses-API
// body. Same host/billing-mode treatment as the Chat Completions path — both go
// through openAITokenEvent, which is the only place either shape's numbers reach
// ComputeCostHost (#300).
//
// 🔴 DOUBLE-COUNT HAZARD with internal/collector/codexrollout. Codex writes its
// rollout logs whether or not it is proxied, so a Codex call routed through
// /openai/ while --codex-rollout is on is captured by BOTH, and the two rows
// cannot dedup: this path keys on the response id (idempotencyKeyForProxy ->
// IdempotencyKey("msg", "openai", "resp_...")) while the collector keys on
// (session id, ordinal), because a rollout log carries no response id at all.
// Different hashes by construction, and the store dedups on the key alone — so
// the spend DOUBLES rather than collides. Before #459 task 2 this was
// impossible, because this parser did not exist. cmd/tierd warns at startup
// when both are enabled; #459 task 3's live run must disable one of them.
func parseOpenAIResponses(body []byte, developer, issueID, host string) *collector.TokenEvent {
	var r openAIResponsesBody
	if err := json.Unmarshal(body, &r); err != nil || r.Model == "" || r.Usage == nil {
		return nil
	}
	if r.Usage.InputTokens == 0 && r.Usage.OutputTokens == 0 {
		return nil
	}
	cached := 0
	if r.Usage.InputTokensDetails != nil {
		cached = r.Usage.InputTokensDetails.CachedTokens
	}
	return openAITokenEvent(developer, issueID, host, r.ID, r.Model,
		r.Usage.InputTokens, r.Usage.OutputTokens, cached)
}

// openAITokenEvent builds the TokenEvent for BOTH OpenAI wire shapes and BOTH
// transports (JSON and SSE) from one set of raw wire counts. Four call sites,
// one convention: clamping, the cached carve-out, host-qualified pricing (#300)
// and the idempotency namespace cannot drift between Chat Completions and
// Responses, because neither shape has its own copy of them.
//
// input is INCLUSIVE of cached in BOTH shapes — chat's prompt_tokens contains
// prompt_tokens_details.cached_tokens (#114), Responses' input_tokens contains
// input_tokens_details.cached_tokens (evidence in openAIResponsesUsage) — so the
// carve-out is unconditional here rather than a per-shape decision.
func openAITokenEvent(developer, issueID, host, id, model string, input, output, cached int) *collector.TokenEvent {
	// Clamp negative usage counts on the RAW wire values first (#121), before
	// the carve-out below. The P0-01 cached=min(cached,input) subtraction (#114)
	// layers AFTER this clamp so it only ever has to reason about cached>input,
	// never negatives. Count once per event.
	if collector.ClampNegativeTokens(&input, &output, &cached) {
		collector.WarnClamp(collector.SourceProxy, model)
		collector.RecordClamp(collector.SourceProxy)
	}
	// Carve cached out of input so the two classes never overlap: Input is the
	// fresh (non-cached) input, CacheRead is the cached remainder. Clamp cached
	// to [0, input] (negatives already handled above) so a contradictory
	// cached>input payload leaves Input at 0 rather than going negative. This
	// matches Anthropic, whose input_tokens already excludes cache classes.
	if cached > input {
		cached = input
	}
	fresh := input - cached
	// OpenAI has no cache-write SKU — both 5m and 1h buckets stay zero, and
	// ComputeCost applies the model's cache-read multiplier to `cached`.
	cost, billingMode := store.ComputeCostHost(host, model, store.CostUsage{
		Input:     fresh,
		Output:    output,
		CacheRead: cached,
	})
	return &collector.TokenEvent{
		Developer:      developer,
		IssueID:        issueID,
		Model:          model,
		InputTok:       fresh,
		OutputTok:      output,
		CacheRead:      cached,
		CostMicro:      cost,
		Fidelity:       collector.FidelityRealtime,
		IdempotencyKey: idempotencyKeyForProxy(ProviderOpenAI, id),
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
