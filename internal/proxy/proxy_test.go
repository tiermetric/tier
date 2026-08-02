package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
)

// memSink is a minimal in-memory collector.Ingester for tests — records every
// ingested event and enforces the same partial-uniqueness contract on
// IdempotencyKey that store.DB does, so we can assert dedup behavior end-to-end
// without a DB.
type memSink struct {
	mu     sync.Mutex
	events []collector.TokenEvent
	seen   map[string]struct{} // non-empty IdempotencyKey set
}

func newMemSink() *memSink {
	return &memSink{seen: make(map[string]struct{})}
}

func (m *memSink) Ingest(_ context.Context, e collector.TokenEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.IdempotencyKey != "" {
		if _, exists := m.seen[e.IdempotencyKey]; exists {
			return nil // mirrors ON CONFLICT DO NOTHING
		}
		m.seen[e.IdempotencyKey] = struct{}{}
	}
	m.events = append(m.events, e)
	return nil
}

// --- Parser unit tests ---

// TestProviderConstantsAlignWithCollector enforces the load-bearing invariant
// behind #19: the proxy's typed Provider constants and the collector's
// `ProviderAnthropic` (used by the JSONL emitter) must produce the same string
// when passed to MessageIdempotencyKey, otherwise cross-source dedup silently
// breaks. A future rename in either package would pass the unit tests that
// hardcode the literal "anthropic" — only a cross-package assertion catches it.
func TestProviderConstantsAlignWithCollector(t *testing.T) {
	if string(ProviderAnthropic) != collector.ProviderAnthropic {
		t.Errorf("proxy.ProviderAnthropic = %q, collector.ProviderAnthropic = %q — must match for cross-source dedup (#19)",
			ProviderAnthropic, collector.ProviderAnthropic)
	}
}

func TestParseAnthropic_EmitsIdempotencyKeyFromMessageID(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"id":"msg_01ABCxyz",
		"model":"claude-sonnet-4",
		"usage":{
			"input_tokens":1000,
			"output_tokens":500,
			"cache_creation_input_tokens":200,
			"cache_read_input_tokens":100
		}
	}`)
	ev := parseAnthropic(body, "alice", "issue-42", "")
	if ev == nil {
		t.Fatal("parseAnthropic returned nil for a valid body")
	}
	want := collector.MessageIdempotencyKey(string(ProviderAnthropic), "msg_01ABCxyz")
	if ev.IdempotencyKey != want {
		t.Errorf("IdempotencyKey = %q, want %q", ev.IdempotencyKey, want)
	}
	if ev.IdempotencyKey == "" || len(ev.IdempotencyKey) != 64 {
		t.Errorf("key should be a 64-char sha256 digest, got %d chars", len(ev.IdempotencyKey))
	}
	if ev.Model != "claude-sonnet-4" || ev.InputTok != 1000 || ev.OutputTok != 500 {
		t.Errorf("usage fields not preserved: %+v", ev)
	}
}

// TestParseAnthropic_ParsesNestedCacheCreationTTL verifies the proxy reads
// the nested `cache_creation` object that Anthropic added for the 5m vs 1h
// TTL split. Without this, both buckets stay at zero and ComputeCost can't
// apply the correct multiplier. Issue #55.
func TestParseAnthropic_ParsesNestedCacheCreationTTL(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"id":"msg_ttl_split",
		"model":"claude-opus-4-7",
		"usage":{
			"input_tokens":10,
			"output_tokens":5,
			"cache_creation_input_tokens":400,
			"cache_read_input_tokens":0,
			"cache_creation":{
				"ephemeral_5m_input_tokens":100,
				"ephemeral_1h_input_tokens":300
			}
		}
	}`)
	ev := parseAnthropic(body, "alice", "issue-55", "")
	if ev == nil {
		t.Fatal("parseAnthropic returned nil for a valid TTL-split body")
	}
	if ev.CacheWrite5m != 100 {
		t.Errorf("CacheWrite5m = %d, want 100 (from nested ephemeral_5m_input_tokens)", ev.CacheWrite5m)
	}
	if ev.CacheWrite1h != 300 {
		t.Errorf("CacheWrite1h = %d, want 300 (from nested ephemeral_1h_input_tokens)", ev.CacheWrite1h)
	}
	// Cost sanity check at the corrected $5/$25 Opus 4.7 rate (#80): 1h dominates
	// (300 * $5/M * 2.0 = $0.003) + 5m (100 * $5/M * 1.25 = $0.000625) + input
	// (10 * $5/M = $0.00005) + output (5 * $25/M = $0.000125). Total ~= $0.0038.
	if ev.CostMicro < 3_700 || ev.CostMicro > 3_900 {
		t.Errorf("CostMicro = %d, want ~3_800 ($0.0038) — TTL multipliers may not be wired through", ev.CostMicro)
	}
}

// TestParseAnthropic_NullCacheCreationBucketsAs5m exercises the JSON-null
// case: `cache_creation: null` vs. field-absent. Go's encoding/json sets a
// pointer field to nil for both inputs, so the legacy-fallback path fires
// and the rolled-up count buckets to 5m. Documented and pinned in a test
// so a future change (e.g. moving to json.RawMessage) doesn't silently
// flip the semantic.
func TestParseAnthropic_NullCacheCreationBucketsAs5m(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"id":"msg_null_cache",
		"model":"claude-sonnet-4",
		"usage":{
			"input_tokens":50,
			"output_tokens":25,
			"cache_creation_input_tokens":300,
			"cache_read_input_tokens":0,
			"cache_creation":null
		}
	}`)
	ev := parseAnthropic(body, "alice", "issue-55", "")
	if ev == nil {
		t.Fatal("parseAnthropic returned nil for cache_creation:null body")
	}
	if ev.CacheWrite5m != 300 {
		t.Errorf("CacheWrite5m = %d, want 300 (null == absent → legacy fallback)", ev.CacheWrite5m)
	}
	if ev.CacheWrite1h != 0 {
		t.Errorf("CacheWrite1h = %d, want 0", ev.CacheWrite1h)
	}
}

// TestParseAnthropic_LegacyResponseBucketsAs5m verifies that a response
// missing the nested cache_creation object (pre-1h-feature shape) buckets
// the rolled-up cache_creation_input_tokens into CacheWrite5m, matching
// Anthropic's pre-feature default per #55.
func TestParseAnthropic_LegacyResponseBucketsAs5m(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"id":"msg_legacy_shape",
		"model":"claude-sonnet-4",
		"usage":{
			"input_tokens":100,
			"output_tokens":50,
			"cache_creation_input_tokens":250,
			"cache_read_input_tokens":0
		}
	}`)
	ev := parseAnthropic(body, "alice", "issue-55", "")
	if ev == nil {
		t.Fatal("parseAnthropic returned nil for legacy body")
	}
	if ev.CacheWrite5m != 250 {
		t.Errorf("CacheWrite5m = %d, want 250 (legacy fallback)", ev.CacheWrite5m)
	}
	if ev.CacheWrite1h != 0 {
		t.Errorf("CacheWrite1h = %d, want 0 (no nested object means no 1h)", ev.CacheWrite1h)
	}
}

func TestParseAnthropic_MissingIDLeavesKeyEmpty(t *testing.T) {
	t.Parallel()
	// Older API versions or malformed bodies may lack `id`. The event must still
	// insert (partial unique index allows NULL) but won't dedup against repeats.
	body := []byte(`{"model":"claude-sonnet-4","usage":{"input_tokens":100,"output_tokens":50}}`)
	ev := parseAnthropic(body, "alice", "issue-42", "")
	if ev == nil {
		t.Fatal("parseAnthropic returned nil")
	}
	if ev.IdempotencyKey != "" {
		t.Errorf("IdempotencyKey should be empty when id is missing, got %q", ev.IdempotencyKey)
	}
}

func TestParseAnthropic_MalformedReturnsNil(t *testing.T) {
	t.Parallel()
	if ev := parseAnthropic([]byte(`{not json`), "alice", "issue-42", ""); ev != nil {
		t.Errorf("expected nil on malformed body, got %+v", ev)
	}
}

func TestParseOpenAI_EmitsIdempotencyKeyFromID(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"id":"chatcmpl-9XYZ",
		"model":"gpt-4o",
		"usage":{
			"prompt_tokens":500,
			"completion_tokens":300,
			"prompt_tokens_details":{"cached_tokens":50}
		}
	}`)
	ev := parseOpenAI(body, "bob", "issue-7", "")
	if ev == nil {
		t.Fatal("parseOpenAI returned nil for a valid body")
	}
	want := collector.MessageIdempotencyKey(string(ProviderOpenAI), "chatcmpl-9XYZ")
	if ev.IdempotencyKey != want {
		t.Errorf("IdempotencyKey = %q, want %q", ev.IdempotencyKey, want)
	}
	if ev.CacheRead != 50 {
		t.Errorf("CacheRead = %d, want 50 (from prompt_tokens_details.cached_tokens)", ev.CacheRead)
	}
	// prompt 500 includes 50 cached, so InputTok carves to 450 (#114).
	if ev.InputTok != 450 {
		t.Errorf("InputTok = %d, want 450 (prompt 500 - cached 50)", ev.InputTok)
	}
}

// TestParseOpenAI_CachedTokensPricedAtHalfRateOfCarvedOutInput owns the "0.5x
// applies to cached" contract for OpenAI (issue #55). Under #114 cached is a
// SUBSET of prompt_tokens, so it is carved OUT of Input before pricing: a
// fully-cached request (prompt == cached) leaves Input == 0 and prices every
// token once at the 0.5x cache-read rate.
func TestParseOpenAI_CachedTokensPricedAtHalfRateOfCarvedOutInput(t *testing.T) {
	t.Parallel()
	// prompt_tokens 1M, all of it cached (cached == prompt), no output. gpt-4o
	// input rate $2.50/M. Input carves to 0, so cost = 1M * $2.50/M * 0.5 = $1.25.
	body := []byte(`{
		"id":"chatcmpl-cache-half",
		"model":"gpt-4o",
		"usage":{
			"prompt_tokens":1000000,
			"completion_tokens":0,
			"prompt_tokens_details":{"cached_tokens":1000000}
		}
	}`)
	ev := parseOpenAI(body, "alice", "issue-55", "")
	if ev == nil {
		t.Fatal("parseOpenAI returned nil")
	}
	if ev.InputTok != 0 {
		t.Errorf("InputTok = %d, want 0 (cached carved out of prompt)", ev.InputTok)
	}
	if ev.CacheRead != 1_000_000 {
		t.Errorf("CacheRead = %d, want 1_000_000", ev.CacheRead)
	}
	// 1M cached at 0.5x $2.50/M = $1.25 = 1_250_000 micro-dollars, exactly.
	if ev.CostMicro != 1_250_000 {
		t.Errorf("CostMicro = %d, want 1_250_000 ($1.25, 0.5x OpenAI cached_tokens rate on carved-out input)", ev.CostMicro)
	}
}

// TestParseOpenAI_CachedTokensNotDoubleCharged pins #114: OpenAI's
// prompt_tokens INCLUDES cached_tokens, so cached must be carved out of Input
// before pricing (non-overlapping classes). Fails on main, which passes prompt
// and cached additively into ComputeCost and bills the overlap 1.5x.
func TestParseOpenAI_CachedTokensNotDoubleCharged(t *testing.T) {
	t.Parallel()
	// gpt-4o $2.50/$10.00. prompt 1000 (900 of it cached), completion 100.
	// Input carves to 100; cost = 100*2.50/1e6 + 900*2.50*0.5/1e6 + 100*10/1e6
	// = 250 + 1125 + 1000 = 2375 micro. Main computes 4625 (prompt billed full
	// PLUS cached billed at half).
	body := []byte(`{
		"id":"chatcmpl-nodouble",
		"model":"gpt-4o",
		"usage":{
			"prompt_tokens":1000,
			"completion_tokens":100,
			"prompt_tokens_details":{"cached_tokens":900}
		}
	}`)
	ev := parseOpenAI(body, "alice", "issue-114", "")
	if ev == nil {
		t.Fatal("parseOpenAI returned nil for a valid body")
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

// TestParseOpenAI_CachedExceedsPromptClamped pins the defensive upper bound: a
// contradictory payload where cached > prompt clamps cached to prompt so Input
// never goes negative. All tokens then price at the cache-read rate.
func TestParseOpenAI_CachedExceedsPromptClamped(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"id":"chatcmpl-clamp",
		"model":"gpt-4o",
		"usage":{
			"prompt_tokens":100,
			"completion_tokens":50,
			"prompt_tokens_details":{"cached_tokens":150}
		}
	}`)
	ev := parseOpenAI(body, "alice", "issue-114", "")
	if ev == nil {
		t.Fatal("parseOpenAI returned nil for a valid body")
	}
	if ev.InputTok != 0 {
		t.Errorf("InputTok = %d, want 0 (cached clamped to prompt, never negative)", ev.InputTok)
	}
	if ev.CacheRead != 100 {
		t.Errorf("CacheRead = %d, want 100 (cached clamped to prompt)", ev.CacheRead)
	}
	// Independent oracle (not ComputeCost, which would be tautological): Input 0
	// + 100 cache_read at 0.5x $2.50/M = 125 micro + 50 output at $10/M = 500
	// micro = 625 micro. Never negative.
	if ev.CostMicro != 625 {
		t.Errorf("CostMicro = %d, want 625 (0 input + 100 cache_read@0.5x + 50 output)", ev.CostMicro)
	}
}

func TestParseOpenAI_MissingIDLeavesKeyEmpty(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"gpt-4o","usage":{"prompt_tokens":100,"completion_tokens":50}}`)
	ev := parseOpenAI(body, "bob", "issue-7", "")
	if ev == nil {
		t.Fatal("parseOpenAI returned nil")
	}
	if ev.IdempotencyKey != "" {
		t.Errorf("IdempotencyKey should be empty when id missing, got %q", ev.IdempotencyKey)
	}
}

func TestParseGemini_EmitsIdempotencyKeyFromResponseID(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"responseId":"rsp-abc123",
		"modelVersion":"gemini-2.5-pro",
		"usageMetadata":{"promptTokenCount":400,"candidatesTokenCount":200,"totalTokenCount":600}
	}`)
	ev := parseGemini(body, "carol", "issue-99", "")
	if ev == nil {
		t.Fatal("parseGemini returned nil for a valid body")
	}
	want := collector.MessageIdempotencyKey(string(ProviderGemini), "rsp-abc123")
	if ev.IdempotencyKey != want {
		t.Errorf("IdempotencyKey = %q, want %q", ev.IdempotencyKey, want)
	}
}

func TestParseGemini_MissingResponseIDLeavesKeyEmpty(t *testing.T) {
	t.Parallel()
	body := []byte(`{"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`)
	ev := parseGemini(body, "carol", "issue-99", "")
	if ev == nil {
		t.Fatal("parseGemini returned nil")
	}
	if ev.IdempotencyKey != "" {
		t.Errorf("IdempotencyKey should be empty when responseId missing, got %q", ev.IdempotencyKey)
	}
}

// TestParseAnthropic_KeysAreProviderScoped guards against a future provider
// returning numeric ids that could otherwise collide across vendors. The
// provider-prefixed key construction must give different keys for the same
// response id seen on different providers.
func TestParseAnthropic_KeysAreProviderScoped(t *testing.T) {
	t.Parallel()
	body := []byte(`{"id":"42","model":"claude-sonnet-4","usage":{"input_tokens":1,"output_tokens":1}}`)
	evA := parseAnthropic(body, "x", "i", "")
	if evA == nil {
		t.Fatal("nil event")
	}
	body = []byte(`{"id":"42","model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	evO := parseOpenAI(body, "x", "i", "")
	if evO == nil {
		t.Fatal("nil event")
	}
	if evA.IdempotencyKey == evO.IdempotencyKey {
		t.Errorf("provider-scoped keys collided on shared id: %q", evA.IdempotencyKey)
	}
}

// --- End-to-end proxy test ---

// TestProxy_DedupsRepeatedAnthropicResponse routes the same synthetic
// Anthropic response through the proxy twice and asserts the sink only
// retains one row. This is the integration contract for the JSONL+proxy
// foundation laid by #6: two calls with the same provider response id
// produce one persisted event.
func TestProxy_DedupsRepeatedAnthropicResponse(t *testing.T) {
	// Upstream "Anthropic API" that returns a stable response on every call.
	const respID = "msg_e2e_dedup_01"
	respBody := `{
		"id":"` + respID + `",
		"model":"claude-sonnet-4",
		"usage":{"input_tokens":100,"output_tokens":50,
		         "cache_creation_input_tokens":0,"cache_read_input_tokens":0}
	}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	sink := newMemSink()
	p := New(target, ProviderAnthropic, collector.SourceProxy, sink, nil, nil, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	// Issue the same logical request twice — same response id both times.
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages",
			bytes.NewReader([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tier-Developer", "alice")
		req.Header.Set("X-Tier-Issue", "issue-42")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		// Drain so the connection returns to the pool before the next request.
		// ModifyResponse already ran synchronously before Do() returned, so the
		// sink write is observable on the next line — the drain is purely a
		// keep-alive courtesy.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
	}

	if got := len(sink.events); got != 1 {
		t.Fatalf("expected 1 event after duplicate insert, got %d: %+v", got, sink.events)
	}
	// Assert the *exact* key derived from the upstream response id. A weak
	// non-empty check would still pass if a regression caused parseAnthropic
	// to emit a key from the wrong field (or a literal placeholder) — only
	// equality to the derived key proves the dedup wiring is intact.
	wantKey := collector.MessageIdempotencyKey(string(ProviderAnthropic), respID)
	if sink.events[0].IdempotencyKey != wantKey {
		t.Errorf("IdempotencyKey = %q, want %q", sink.events[0].IdempotencyKey, wantKey)
	}
	if sink.events[0].Developer != "alice" {
		t.Errorf("developer = %q, want alice", sink.events[0].Developer)
	}
}

// TestProxy_SourceStampedWithoutExternalTagger is the #337 regression: it proves
// proxy.New ALONE stamps ev.Source, with NO external ingester.SourceTagger
// wrapping the sink. Before #337 a caller who passed a bare ingester.Store(db) to
// New — forgetting the wiring-time tagger — would silently persist token_events
// with an empty source (the column is TEXT NOT NULL, so "" satisfies it). New now
// builds its own tagger internally, so this end-to-end capture through a BARE
// memSink must land Source == SourceProxy. If this ever fails, the wiring-trap
// #46 relocated to the seam has reopened.
func TestProxy_SourceStampedWithoutExternalTagger(t *testing.T) {
	const respID = "msg_source_337"
	respBody := `{"id":"` + respID + `","model":"claude-sonnet-4","usage":{"input_tokens":100,"output_tokens":50}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	// BARE sink — deliberately NOT wrapped in ingester.SourceTagger. New must do
	// the stamping itself, which is the entire point of #337.
	sink := newMemSink()
	p := New(target, ProviderAnthropic, collector.SourceProxy, sink, nil, nil, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages",
		bytes.NewReader([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tier-Developer", "alice")
	req.Header.Set("X-Tier-Issue", "issue-42")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	if got := len(sink.events); got != 1 {
		t.Fatalf("captured %d events, want 1", got)
	}
	got := sink.events[0]
	if got.Source != collector.SourceProxy {
		t.Errorf("stored Source = %q, want %q — proxy.New must stamp source with no external tagger (#337 wiring-trap reopened)", got.Source, collector.SourceProxy)
	}
	// Confirm it is the REAL captured event that got stamped (not some empty
	// zero-value row) — the parser-set attribution/usage must survive the tagger.
	if got.Developer != "alice" {
		t.Errorf("stored Developer = %q, want alice — tagger must not clobber parser-set fields", got.Developer)
	}
	if got.InputTok != 100 || got.OutputTok != 50 {
		t.Errorf("stored tokens = (%d in, %d out), want (100, 50) — the stamped event must be the parsed one", got.InputTok, got.OutputTok)
	}
}

// TestProxy_NewPanicsOnEmptySource pins the #337 guard: an empty source is
// rejected at construction, so a proxy that would persist empty-source
// (unattributable) token_events cannot be built. Fail-loud at wiring time beats
// a silent TEXT-NOT-NULL "" row. This is what makes the "source-less proxy is
// unconstructable" claim on New literally true — not merely "the wrap can't be
// forgotten".
func TestProxy_NewPanicsOnEmptySource(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New with an empty source did not panic — an empty-source proxy must be unconstructable (#337)")
		}
	}()
	target, _ := url.Parse("https://openrouter.ai")
	_ = New(target, ProviderOpenAI, "", newMemSink(), nil, nil, nil)
}

// TestProxy_RewritesHostAndPreservesBasePath pins the #62 fix. The
// load-bearing assertion is the Host header: the Director variant forwarded
// the INBOUND host upstream, which virtual-hosted gateways reject; SetURL
// derives Host from the target. The base-path join (/base + /v1/messages →
// /base/v1/messages) was never broken — both forms share rewriteRequestURL —
// but is pinned here as a regression guard. Also pins that no X-Forwarded-*
// reaches the upstream (Rewrite mode strips them; we never call
// SetXForwarded — internal developer IPs must not leak to the provider).
func TestProxy_RewritesHostAndPreservesBasePath(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var gotPath, gotHost, gotXFF string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath, gotHost, gotXFF = r.URL.Path, r.Host, r.Header.Get("X-Forwarded-For")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_bp","model":"claude-sonnet-4","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL + "/base")
	if err != nil {
		t.Fatal(err)
	}
	p := New(target, ProviderAnthropic, collector.SourceProxy, newMemSink(), nil, nil, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/base/v1/messages" {
		t.Errorf("upstream path = %q, want /base/v1/messages (target base path dropped)", gotPath)
	}
	wantHost := target.Host
	if gotHost != wantHost {
		t.Errorf("upstream Host = %q, want %q (SetURL must derive Host from the target)", gotHost, wantHost)
	}
	if gotXFF != "" {
		t.Errorf("upstream saw X-Forwarded-For = %q, want none (internal IPs must not leak)", gotXFF)
	}
}

// TestProxy_ForwardsResponseBodyToClient guards against a regression where
// buffering the body for parsing accidentally short-changes the client.
// Asserts byte-identical forwarding — an unmarshal/remarshal round-trip would
// hide truncation or whitespace bugs since both sides would still parse OK.
func TestProxy_ForwardsResponseBodyToClient(t *testing.T) {
	t.Parallel()
	respBody := `{"id":"msg_x","model":"claude-sonnet-4","usage":{"input_tokens":1,"output_tokens":1}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
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
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	_ = resp.Body.Close()

	if !bytes.Equal(got, []byte(respBody)) {
		t.Errorf("client body differs from upstream\n got: %q\nwant: %q", got, respBody)
	}
	if resp.ContentLength != int64(len(respBody)) {
		t.Errorf("Content-Length = %d, want %d", resp.ContentLength, len(respBody))
	}
}

// --- Write-outcome counter (#70) ---

// errSink fails every Ingest, exercising the store-write-failure path that
// surfaces as a log line (always) plus a counter increment (#70). Before #70 no
// test drove a failing sink at all.
type errSink struct{ err error }

func (e errSink) Ingest(context.Context, collector.TokenEvent) error { return e.err }

// recRecorder captures WriteRecorder.Inc calls as "provider|outcome" strings so
// a test can assert exactly which label set was recorded. When ch is non-nil it
// receives one signal per Inc, letting an end-to-end test (where the SSE emit
// fires on the server goroutine at Close) block until the record actually lands
// instead of racing it.
type recRecorder struct {
	mu    sync.Mutex
	calls []string
	ch    chan struct{}
}

func (r *recRecorder) Inc(labelValues ...string) {
	r.mu.Lock()
	r.calls = append(r.calls, strings.Join(labelValues, "|"))
	r.mu.Unlock()
	if r.ch != nil {
		select {
		case r.ch <- struct{}{}:
		default:
		}
	}
}

func (r *recRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// jsonProviderFixtures maps each provider to a minimal successful response body
// that its JSON parser extracts a TokenEvent from — so the {provider} label can
// be pinned for all three providers, not just anthropic.
var jsonProviderFixtures = []struct {
	provider Provider
	label    string
	body     string
}{
	{ProviderAnthropic, "anthropic", `{"id":"msg_x","model":"claude-sonnet-4","usage":{"input_tokens":10,"output_tokens":5}}`},
	{ProviderOpenAI, "openai", `{"id":"chatcmpl-x","model":"gpt-4o","usage":{"prompt_tokens":10,"completion_tokens":5}}`},
	{ProviderGemini, "gemini", `{"responseId":"x","modelVersion":"gemini-1.5-pro","usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`},
}

// TestProxy_RecordsWriteOutcome_JSON drives the real Proxy end-to-end through
// httptest and asserts the JSON path records exactly one {provider,outcome}
// sample per attempted store write. The write is synchronous inside
// modifyResponse — it completes before the proxy streams the response — so the
// recorder is populated by the time the client finishes reading. Runs across
// all three providers so the {provider} label mapping is pinned for each, and
// across ok (memSink) / error (errSink, the #70 silent-failure path).
func TestProxy_RecordsWriteOutcome_JSON(t *testing.T) {
	t.Parallel()
	run := func(provider Provider, body string, sink collector.Ingester) []string {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		}))
		defer upstream.Close()
		target, _ := url.Parse(upstream.URL)
		rec := &recRecorder{}
		p := New(target, provider, collector.SourceProxy, sink, rec, nil, nil)
		proxySrv := httptest.NewServer(p)
		defer proxySrv.Close()

		req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return rec.snapshot()
	}

	for _, f := range jsonProviderFixtures {
		t.Run(f.label+"/ok", func(t *testing.T) {
			t.Parallel()
			if got := run(f.provider, f.body, newMemSink()); len(got) != 1 || got[0] != f.label+"|"+outcomeOK {
				t.Errorf("recorder calls = %v, want [%s|ok]", got, f.label)
			}
		})
		t.Run(f.label+"/error", func(t *testing.T) {
			t.Parallel()
			if got := run(f.provider, f.body, errSink{err: errors.New("boom")}); len(got) != 1 || got[0] != f.label+"|"+outcomeError {
				t.Errorf("recorder calls = %v, want [%s|error]", got, f.label)
			}
		})
	}
}

// TestProxy_RecordsWriteOutcome_SSE_EndToEnd covers the wiring the deterministic
// direct-Close test below cannot: that modifyResponse actually threads
// p.recordWrite into newStreamCapture. The streaming emit fires on the server
// goroutine at Close, so we sync on the recorder's channel rather than racing
// the client EOF (the reason the rest of the SSE assertions use direct Close).
func TestProxy_RecordsWriteOutcome_SSE_EndToEnd(t *testing.T) {
	t.Parallel()
	streamBody := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_e2e","model":"claude-sonnet-4","usage":{"input_tokens":20,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":10}}`,
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
	rec := &recRecorder{ch: make(chan struct{}, 1)}
	p := New(target, ProviderAnthropic, collector.SourceProxy, newMemSink(), rec, nil, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	select {
	case <-rec.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("streaming emit never recorded a write outcome")
	}
	if got := rec.snapshot(); len(got) != 1 || got[0] != "anthropic|"+outcomeOK {
		t.Errorf("recorder calls = %v, want [anthropic|ok]", got)
	}
}

// TestProxy_RecordsWriteOutcome_SSE is the streaming analogue. The streaming
// emit fires at streamCapture.Close, so — unlike the JSON path, which records
// synchronously inside modifyResponse before the client sees any bytes — driving
// it through httptest would race the server-side Close against the client EOF.
// We instead exercise the real wiring (a Proxy's recordWrite, carrying its
// provider label and nil-guard) by constructing newStreamCapture with
// p.recordWrite and calling Close directly, which runs emit synchronously in
// this goroutine — the same deterministic pattern as
// TestStreamCapture_CloseEmitsExactlyOnce.
func TestProxy_RecordsWriteOutcome_SSE(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_rec_sse","model":"claude-sonnet-4","usage":{"input_tokens":20,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":10}}`,
		``,
	}, "\n")
	target, _ := url.Parse("http://example.invalid")
	run := func(sink collector.Ingester) []string {
		rec := &recRecorder{}
		p := New(target, ProviderAnthropic, collector.SourceProxy, sink, rec, nil, nil)
		sc := newStreamCapture(context.Background(), io.NopCloser(strings.NewReader(stream)),
			p.provider, "alice", "issue-1", "", "", sink, p.recordWrite, p.recordUncaptured, slog.Default())
		if _, err := io.ReadAll(sc); err != nil {
			t.Fatalf("read: %v", err)
		}
		if err := sc.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		return rec.snapshot()
	}

	if got := run(newMemSink()); len(got) != 1 || got[0] != "anthropic|"+outcomeOK {
		t.Errorf("ok path: recorder calls = %v, want [anthropic|ok]", got)
	}
	if got := run(errSink{err: errors.New("boom")}); len(got) != 1 || got[0] != "anthropic|"+outcomeError {
		t.Errorf("error path: recorder calls = %v, want [anthropic|error]", got)
	}
}

// TestProxy_DoesNotRecordWhenNoEvent pins the metric's denominator integrity:
// when no TokenEvent is extracted (zero/absent usage, unparseable body) there is
// no store write, so recordWrite must NOT fire. A regression that moved the
// counter above the ev==nil guard would silently inflate the "ok" count with
// phantom writes — and the success-outcome tests above would still pass, so this
// negative case is the one that catches it. Covered on both paths.
func TestProxy_DoesNotRecordWhenNoEvent(t *testing.T) {
	t.Parallel()

	t.Run("json/zero-usage", func(t *testing.T) {
		t.Parallel()
		// Valid anthropic JSON but input==output==0 → parseAnthropic returns nil.
		const body = `{"id":"msg_z","model":"claude-sonnet-4","usage":{"input_tokens":0,"output_tokens":0}}`
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		}))
		defer upstream.Close()
		target, _ := url.Parse(upstream.URL)
		rec := &recRecorder{}
		p := New(target, ProviderAnthropic, collector.SourceProxy, newMemSink(), rec, nil, nil)
		proxySrv := httptest.NewServer(p)
		defer proxySrv.Close()

		resp, err := http.Post(proxySrv.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if got := rec.snapshot(); len(got) != 0 {
			t.Errorf("recorder calls = %v, want none (no event extracted → no write)", got)
		}
	})

	t.Run("sse/no-usage", func(t *testing.T) {
		t.Parallel()
		// message_start with a model but zero usage and no message_delta →
		// Finalise returns nil (input==output==0).
		stream := strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_z","model":"claude-sonnet-4","usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		}, "\n")
		rec := &recRecorder{}
		p := New(mustParseURL(t, "http://example.invalid"), ProviderAnthropic, collector.SourceProxy, newMemSink(), rec, nil, nil)
		sc := newStreamCapture(context.Background(), io.NopCloser(strings.NewReader(stream)),
			p.provider, "alice", "issue-1", "", "", p.sink, p.recordWrite, p.recordUncaptured, slog.Default())
		if _, err := io.ReadAll(sc); err != nil {
			t.Fatalf("read: %v", err)
		}
		if err := sc.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if got := rec.snapshot(); len(got) != 0 {
			t.Errorf("recorder calls = %v, want none (no event extracted → no write)", got)
		}
	})
}

// --- Content-Encoding capture + uncaptured counter (#117) ---

// TestProxy_GzipUpstreamResponseIsCaptured pins the Accept-Encoding strip
// end-to-end against a real gzip-compressing upstream (the Test-plan variant).
// The upstream gzip-compresses and sets Content-Encoding: gzip whenever the
// request carries Accept-Encoding: gzip — exactly how a provider SDK's origin
// behaves. On main the client's Accept-Encoding travels verbatim, so the
// Transport (which only auto-gunzips a coding IT injected) hands modifyResponse
// gzip bytes, the parser returns nil, and usage is silently dropped: 0 events.
// On the branch the Rewrite hook strips the client header, the Transport
// negotiates its OWN gzip and transparently decompresses, so modifyResponse sees
// identity bytes and the event is captured — making the capture assertion
// load-bearing, not tautological. The compress/gzip use is confined to this test
// fixture; tier's production path adds no decompression code by design.
//
// gzipBytes is a nolint helper kept local so the prohibition on gzip in
// production code is visibly scoped to test fixtures only.
func TestProxy_GzipUpstreamResponseIsCaptured(t *testing.T) {
	t.Parallel()
	const body = `{"id":"msg_gz","model":"claude-sonnet-4","usage":{"input_tokens":500,"output_tokens":50}}`
	gzBody := gzipBytes(t, []byte(body))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(gzBody)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	sink := newMemSink()
	p := New(target, ProviderAnthropic, collector.SourceProxy, sink, nil, nil, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()

	if len(sink.events) != 1 {
		t.Fatalf("expected 1 token event (gzip upstream body must be captured), got %d: %+v", len(sink.events), sink.events)
	}
	if sink.events[0].InputTok != 500 {
		t.Errorf("InputTok = %d, want 500", sink.events[0].InputTok)
	}
	// The client (Go http, which set no gzip Accept-Encoding of its own here
	// beyond ours) receives an identity body: the Transport decompressed and the
	// proxy forwarded identity bytes, so the client sees the original JSON.
	if !bytes.Equal(got, []byte(body)) {
		t.Errorf("client body = %q, want identity JSON %q", got, body)
	}
}

// gzipBytes gzip-compresses b for a test upstream fixture. Confined to test code:
// tier's production proxy adds no gzip/decompression code — it relies on the Go
// Transport's transparent gzip handling (#117).
func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// TestProxy_UnhandledContentEncodingSkipsCaptureAndCounts pins the defense for a
// non-identity Content-Encoding the proxy cannot parse (br/zstd from an upstream
// that ignored the Accept-Encoding strip). The response must pass through
// byte-identical (tier has no decompressor by design), emit no event, and record
// reason "encoding". Fails on main: no guard exists, so the opaque body flows
// into the parser, returns nil, and nothing is counted.
func TestProxy_UnhandledContentEncodingSkipsCaptureAndCounts(t *testing.T) {
	t.Parallel()
	// Opaque, non-JSON bytes labelled Content-Encoding: br.
	opaque := []byte{0x1b, 0x2c, 0x00, 0x00, 0xff, 0x01, 0x02}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br")
		_, _ = w.Write(opaque)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	sink := newMemSink()
	rec := &recRecorder{}
	p := New(target, ProviderAnthropic, collector.SourceProxy, sink, nil, rec, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()

	if len(sink.events) != 0 {
		t.Fatalf("expected 0 events, got %d: %+v", len(sink.events), sink.events)
	}
	if !bytes.Equal(got, opaque) {
		t.Errorf("client body = %x, want %x (unhandled encoding must pass through byte-identical)", got, opaque)
	}
	if calls := rec.snapshot(); len(calls) != 1 || calls[0] != "anthropic|"+reasonEncoding {
		t.Errorf("recorder calls = %v, want [anthropic|encoding]", calls)
	}
}

// TestProxy_UnparseableJSONBodyCounts pins reason "parse": a 2xx JSON response
// whose body the parser rejects (here, truncated JSON) emits no event but must
// be counted so systematic $0 capture is observable rather than silent. Fails on
// main: the counter does not exist and ev==nil returns without recording.
func TestProxy_UnparseableJSONBodyCounts(t *testing.T) {
	t.Parallel()
	const body = `{"garbage`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	sink := newMemSink()
	rec := &recRecorder{}
	p := New(target, ProviderAnthropic, collector.SourceProxy, sink, nil, rec, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()

	if len(sink.events) != 0 {
		t.Fatalf("expected 0 events, got %d: %+v", len(sink.events), sink.events)
	}
	if !bytes.Equal(got, []byte(body)) {
		t.Errorf("client body = %q, want %q (unparseable body still forwarded unchanged)", got, body)
	}
	if calls := rec.snapshot(); len(calls) != 1 || calls[0] != "anthropic|"+reasonParse {
		t.Errorf("recorder calls = %v, want [anthropic|parse]", calls)
	}
}

// TestProxy_EmptyJSONBodyDoesNotCount guards the len==0 carve-out: an empty 2xx
// body is not a parse failure worth flagging (no bytes to parse), so the
// uncaptured counter must stay untouched. Without the guard, every empty-bodied
// 2xx would inflate the "parse" reason and drown the real signal.
func TestProxy_EmptyJSONBodyDoesNotCount(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// No body written.
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	sink := newMemSink()
	rec := &recRecorder{}
	p := New(target, ProviderAnthropic, collector.SourceProxy, sink, nil, rec, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if len(sink.events) != 0 {
		t.Fatalf("expected 0 events, got %d: %+v", len(sink.events), sink.events)
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Errorf("recorder calls = %v, want none (empty body must not count)", calls)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// --- Negative-usage clamp (#121) ---

// TestParseAnthropic_NegativeTokensClamped pins the JSON-boundary clamp: a
// hostile/malformed Anthropic body with a negative input_tokens must clamp that
// field to zero, keep the valid output, and never emit a negative cost. Fails
// on main, where r.Usage.InputTokens is copied unchecked into ComputeCost.
func TestParseAnthropic_NegativeTokensClamped(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"id":"msg_neg",
		"model":"claude-sonnet-4",
		"usage":{"input_tokens":-1000,"output_tokens":300,
			"cache_creation_input_tokens":0,"cache_read_input_tokens":-5}
	}`)
	ev := parseAnthropic(body, "alice", "issue-121", "")
	if ev == nil {
		t.Fatal("parseAnthropic returned nil; clamp must keep the event, not drop it")
	}
	if ev.InputTok != 0 {
		t.Errorf("InputTok = %d, want 0 (negative input clamped)", ev.InputTok)
	}
	if ev.CacheRead != 0 {
		t.Errorf("CacheRead = %d, want 0 (negative cache_read clamped)", ev.CacheRead)
	}
	if ev.OutputTok != 300 {
		t.Errorf("OutputTok = %d, want 300 (valid field untouched)", ev.OutputTok)
	}
	if ev.CostMicro < 0 {
		t.Errorf("CostMicro = %d, want >= 0 after clamp", ev.CostMicro)
	}
}

// TestParseOpenAI_NegativeTokensClamped pins the clamp for the OpenAI-compatible
// parser. Fails on main, where negative prompt_tokens flow straight into
// ComputeCost.
//
// This is a clamp regression + forward-compat guard, NOT an ordering pin: with
// both prompt and cached negative, clamp-first and subtract-first collapse to
// the same 0, so it cannot detect a mis-ordered subtraction. A genuine
// clamp-before-min(cached,prompt) ordering pin is impossible until P0-01 (#114)
// lands the subtraction; the assertion CacheRead==0 here stays true afterward
// (a clamped cached=0 gives min(0,prompt)=0).
func TestParseOpenAI_NegativeTokensClamped(t *testing.T) {
	t.Parallel()
	// prompt negative and cached negative: both must clamp to 0.
	body := []byte(`{
		"id":"chatcmpl-neg",
		"model":"gpt-4o",
		"usage":{"prompt_tokens":-100,"completion_tokens":50,
			"prompt_tokens_details":{"cached_tokens":-30}}
	}`)
	ev := parseOpenAI(body, "bob", "issue-121", "")
	if ev == nil {
		t.Fatal("parseOpenAI returned nil; clamp must keep the event, not drop it")
	}
	if ev.InputTok != 0 {
		t.Errorf("InputTok = %d, want 0 (negative prompt clamped before subtraction)", ev.InputTok)
	}
	if ev.CacheRead != 0 {
		t.Errorf("CacheRead = %d, want 0 (negative cached clamped)", ev.CacheRead)
	}
	if ev.OutputTok != 50 {
		t.Errorf("OutputTok = %d, want 50 (valid field untouched)", ev.OutputTok)
	}
	if ev.CostMicro < 0 {
		t.Errorf("CostMicro = %d, want >= 0 after clamp", ev.CostMicro)
	}
}

// TestParseGemini_NegativeTokensClamped pins the clamp for the Gemini parser:
// a negative candidatesTokenCount must clamp while the valid prompt survives.
// Fails on main.
func TestParseGemini_NegativeTokensClamped(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"responseId":"gem-neg",
		"modelVersion":"gemini-2.5-pro",
		"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":-40}
	}`)
	ev := parseGemini(body, "carol", "issue-121", "")
	if ev == nil {
		t.Fatal("parseGemini returned nil; clamp must keep the event, not drop it")
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

// clampCounter is a minimal collector.ClampRecorder for asserting per-event
// clamp counting on the proxy path.
type clampCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *clampCounter) Inc(labelValues ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = make(map[string]int)
	}
	src := ""
	if len(labelValues) > 0 {
		src = labelValues[0]
	}
	c.counts[src]++
}

func (c *clampCounter) get(src string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[src]
}

// TestParseAnthropic_ClampCounterIncrementsOncePerEvent proves the proxy-side
// counter semantic (qa gap): a single body with TWO negative usage fields bumps
// tier_negative_tokens_clamped_total{source="proxy"} exactly once — per event,
// not per field. Not parallel: it swaps the global recorder (restored via
// t.Cleanup), so it serializes ahead of the parallel parser tests.
func TestParseAnthropic_ClampCounterIncrementsOncePerEvent(t *testing.T) {
	rec := &clampCounter{}
	collector.SetClampRecorder(rec)
	t.Cleanup(func() { collector.SetClampRecorder(nil) })

	body := []byte(`{
		"id":"msg_neg2",
		"model":"claude-sonnet-4",
		"usage":{"input_tokens":-1000,"output_tokens":300,"cache_read_input_tokens":-5}
	}`)
	if ev := parseAnthropic(body, "alice", "issue-121", ""); ev == nil {
		t.Fatal("parseAnthropic returned nil; clamp must keep the event")
	}
	if got := rec.get(string(collector.SourceProxy)); got != 1 {
		t.Errorf("proxy clamp count = %d, want 1 (one event, two negative fields)", got)
	}
	if got := rec.get(string(collector.SourceJSONL)); got != 0 {
		t.Errorf("jsonl clamp count = %d, want 0", got)
	}
}

// --- Truncated-body / read-error faithfulness (#123) ---

// proxyMaxBody mirrors the handleJSON capture cap. Tests below pin behavior at
// and past this boundary; if the production constant changes, update here.
const proxyMaxBody = 10 << 20 // 10 MB

// TestProxy_OversizeJSONBodyForwardedIntact pins that a 2xx JSON body larger
// than the capture cap is forwarded byte-identical (headers untouched),
// uncaptured, and counted with reason "too_large". Fails on main, which cuts the
// body to exactly maxBody via LimitReader and forwards it under a 200 while the
// upstream Content-Length still advertises the true (larger) size — a short read
// on the wire. The oversize buffer is transient (one bytes.Repeat, no fixture).
func TestProxy_OversizeJSONBodyForwardedIntact(t *testing.T) {
	t.Parallel()
	// Two over-cap sizes exercise both reconstruction shapes:
	//   maxBody+1    — the tightest boundary; ReadAll drains the whole body so
	//                  origBody is at EOF and MultiReader serves only the prefix.
	//   maxBody+1KiB — a clear margin; 1023 bytes remain unread in origBody, so
	//                  MultiReader must stitch prefix + remainder in order.
	cases := []struct {
		name string
		size int
	}{
		{"exactly_over_cap_by_one", proxyMaxBody + 1},
		{"over_cap_with_remainder", proxyMaxBody + 1024},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Deterministic filler. Content-Type says JSON but the too-large branch
			// never parses it.
			big := bytes.Repeat([]byte("x"), tc.size)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", strconv.Itoa(len(big)))
				_, _ = w.Write(big)
			}))
			defer upstream.Close()

			target, _ := url.Parse(upstream.URL)
			sink := newMemSink()
			rec := &recRecorder{}
			p := New(target, ProviderAnthropic, collector.SourceProxy, sink, nil, rec, nil)
			proxySrv := httptest.NewServer(p)
			defer proxySrv.Close()

			req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v (a truncated body with a stale Content-Length surfaces here)", err)
			}
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (oversize body passes through, not an error)", resp.StatusCode)
			}
			if len(got) != len(big) {
				t.Fatalf("received %d bytes, want %d (body truncated at the cap)", len(got), len(big))
			}
			if sha256.Sum256(got) != sha256.Sum256(big) {
				t.Error("client body sha256 differs from upstream — bytes not forwarded intact")
			}
			if resp.ContentLength != int64(len(big)) {
				t.Errorf("Content-Length = %d, want %d (stale header advertises the wrong size)", resp.ContentLength, len(big))
			}
			if len(sink.events) != 0 {
				t.Fatalf("expected 0 events (oversize body is uncaptured), got %d", len(sink.events))
			}
			if calls := rec.snapshot(); len(calls) != 1 || calls[0] != "anthropic|too_large" {
				t.Errorf("recorder calls = %v, want [anthropic|too_large]", calls)
			}
		})
	}
}

// TestProxy_UpstreamMidBodyErrorReturns502 pins that a mid-body upstream read
// error surfaces as a 502 proxy error, never as a 200 carrying the truncated
// prefix. The upstream advertises a large Content-Length, flushes a partial
// body, then aborts the connection so the proxy's ReadAll errors mid-stream.
// Fails on main, which swallows the error (return nil) and forwards the prefix
// under a fabricated 200.
func TestProxy_UpstreamMidBodyErrorReturns502(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Advertise far more than we send, then abort — the proxy's read of the
		// declared-length body fails with an unexpected EOF.
		w.Header().Set("Content-Length", strconv.Itoa(1<<20))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"msg_trunc","model":"claude`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler) // aborts the conn without server log noise
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	sink := newMemSink()
	rec := &recRecorder{}
	p := New(target, ProviderAnthropic, collector.SourceProxy, sink, nil, rec, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (upstream read error must not become a truncated 200)", resp.StatusCode)
	}
	if !strings.Contains(string(got), "proxy error") {
		t.Errorf("body = %q, want it to contain %q", got, "proxy error")
	}
	if len(sink.events) != 0 {
		t.Fatalf("expected 0 events on a failed read, got %d", len(sink.events))
	}
	// A read error is a proxy failure, not an uncaptured 2xx — it must NOT be
	// miscounted (as too_large or any other reason). Locks the branch ordering:
	// the error branch returns before any recordUncaptured.
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Errorf("recorder calls = %v, want none (read error is a 502, not an uncaptured response)", calls)
	}
}

// TestProxy_ExactlyMaxBodyStillCaptured pins the boundary: a valid JSON body of
// exactly maxBody bytes is captured normally (not treated as too-large). This
// exercises the `> maxBody` (not `>=`) comparison — a body at the cap must parse.
func TestProxy_ExactlyMaxBodyStillCaptured(t *testing.T) {
	t.Parallel()
	// Valid anthropic JSON padded to EXACTLY maxBody bytes via a long string field.
	prefix := `{"id":"msg_max","model":"claude-sonnet-4","usage":{"input_tokens":10,"output_tokens":5},"pad":"`
	suffix := `"}`
	padLen := proxyMaxBody - len(prefix) - len(suffix)
	if padLen < 0 {
		t.Fatalf("prefix+suffix (%d) already exceed maxBody (%d)", len(prefix)+len(suffix), proxyMaxBody)
	}
	body := prefix + strings.Repeat("x", padLen) + suffix
	if len(body) != proxyMaxBody {
		t.Fatalf("test body length = %d, want exactly %d", len(body), proxyMaxBody)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	sink := newMemSink()
	rec := &recRecorder{}
	p := New(target, ProviderAnthropic, collector.SourceProxy, sink, nil, rec, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(got) != len(body) || sha256.Sum256(got) != sha256.Sum256([]byte(body)) {
		t.Errorf("body not forwarded intact at the cap boundary (got %d bytes, want %d)", len(got), len(body))
	}
	if len(sink.events) != 1 {
		t.Fatalf("expected 1 event (exactly-at-cap body is captured), got %d", len(sink.events))
	}
	if sink.events[0].InputTok != 10 || sink.events[0].OutputTok != 5 {
		t.Errorf("usage = (in %d, out %d), want (10, 5)", sink.events[0].InputTok, sink.events[0].OutputTok)
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Errorf("recorder calls = %v, want none (at-cap body is captured, not counted too_large)", calls)
	}
}

// TestProxy_NormalJSONPathUnchanged is the regression control: a standard small
// JSON body still yields a token event AND is forwarded byte-identical. Guards
// against the three-branch rewrite altering the common path.
func TestProxy_NormalJSONPathUnchanged(t *testing.T) {
	t.Parallel()
	const respBody = `{"id":"msg_norm","model":"claude-sonnet-4","usage":{"input_tokens":100,"output_tokens":50}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	sink := newMemSink()
	rec := &recRecorder{}
	p := New(target, ProviderAnthropic, collector.SourceProxy, sink, nil, rec, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(got, []byte(respBody)) {
		t.Errorf("client body differs from upstream\n got: %q\nwant: %q", got, respBody)
	}
	if resp.ContentLength != int64(len(respBody)) {
		t.Errorf("Content-Length = %d, want %d", resp.ContentLength, len(respBody))
	}
	if len(sink.events) != 1 {
		t.Fatalf("expected 1 event on the normal path, got %d", len(sink.events))
	}
	if sink.events[0].InputTok != 100 || sink.events[0].OutputTok != 50 {
		t.Errorf("usage = (in %d, out %d), want (100, 50)", sink.events[0].InputTok, sink.events[0].OutputTok)
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Errorf("recorder calls = %v, want none (successful capture)", calls)
	}
}

// TestParseGemini_ThoughtsTokensBilledAtOutputRate pins P0-09: Gemini bills
// reasoning ("thinking") tokens at the OUTPUT rate via thoughtsTokenCount, a
// field that is NOT included in candidatesTokenCount. parseGemini must fold
// thoughts into OutputTok. Fails on main, which reads only candidatesTokenCount
// (OutputTok would be 200, cost 3_250).
func TestParseGemini_ThoughtsTokensBilledAtOutputRate(t *testing.T) {
	t.Parallel()
	// gemini-2.5-pro $1.25/$10.00. prompt 1000 (no cache), candidates 200,
	// thoughts 800. Output folds thoughts: 200 + 800 = 1000.
	// cost = 1000*1.25/1e6 (input) + 1000*10.00/1e6 (output)
	//      = 0.00125 + 0.01000 = 0.01125 dollars = 11_250 micro exactly.
	body := []byte(`{
		"responseId":"gem-thoughts",
		"modelVersion":"gemini-2.5-pro",
		"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":200,"thoughtsTokenCount":800}
	}`)
	ev := parseGemini(body, "carol", "issue-122", "")
	if ev == nil {
		t.Fatal("parseGemini returned nil for a valid body")
	}
	if ev.InputTok != 1000 {
		t.Errorf("InputTok = %d, want 1000 (prompt, no cache)", ev.InputTok)
	}
	if ev.OutputTok != 1000 {
		t.Errorf("OutputTok = %d, want 1000 (candidates 200 + thoughts 800, both output-rate)", ev.OutputTok)
	}
	if ev.CostMicro != 11_250 {
		t.Errorf("CostMicro = %d, want 11_250 (thoughts billed at output rate)", ev.CostMicro)
	}
}

// TestParseGemini_CachedContentDiscountedAndSubtracted pins P0-09: Gemini's
// cachedContentTokenCount is a SUBSET of promptTokenCount (like OpenAI's
// cached_tokens), so it is carved out of Input and discounted at the model's
// cache_read_mult. Fails on main, which neither subtracts nor discounts cached.
func TestParseGemini_CachedContentDiscountedAndSubtracted(t *testing.T) {
	t.Parallel()
	// gemini-2.5-pro $1.25/$10.00, cache_read_mult 0.25 (implicit-cache 75% off).
	// prompt 1000 (900 cached), candidates 100, no thoughts.
	// Input = 1000 - 900 = 100; CacheRead = 900; Output = 100.
	// cost = 100*1.25/1e6 + 900*1.25*0.25/1e6 + 100*10.00/1e6
	//      = 0.000125 + 0.00028125 + 0.001 = 0.00140625 dollars
	//      * 1e6 = 1406.25 → RoundToEven → 1406 micro.
	body := []byte(`{
		"responseId":"gem-cached",
		"modelVersion":"gemini-2.5-pro",
		"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":100,"cachedContentTokenCount":900}
	}`)
	ev := parseGemini(body, "carol", "issue-122", "")
	if ev == nil {
		t.Fatal("parseGemini returned nil for a valid body")
	}
	if ev.InputTok != 100 {
		t.Errorf("InputTok = %d, want 100 (prompt 1000 - cached 900)", ev.InputTok)
	}
	if ev.CacheRead != 900 {
		t.Errorf("CacheRead = %d, want 900", ev.CacheRead)
	}
	if ev.CostMicro != 1_406 {
		t.Errorf("CostMicro = %d, want 1_406 (cached carved out + discounted 0.25x)", ev.CostMicro)
	}
}

// TestParseGemini_CachedExceedsPromptClamped mirrors the OpenAI #114 edge for
// Gemini: a contradictory payload where cachedContentTokenCount > promptTokenCount
// clamps cached to prompt so Input never goes negative.
func TestParseGemini_CachedExceedsPromptClamped(t *testing.T) {
	t.Parallel()
	// prompt 100, cached 150 → cached clamps to 100, Input 0, CacheRead 100.
	// candidates 50, no thoughts. gemini-2.5-pro read mult 0.25.
	// cost = 0 (input) + 100*1.25*0.25/1e6 + 50*10.00/1e6
	//      = 0.00003125 + 0.0005 = 0.00053125 dollars * 1e6 = 531.25 → 531 micro.
	body := []byte(`{
		"responseId":"gem-clamp",
		"modelVersion":"gemini-2.5-pro",
		"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":50,"cachedContentTokenCount":150}
	}`)
	ev := parseGemini(body, "carol", "issue-122", "")
	if ev == nil {
		t.Fatal("parseGemini returned nil for a valid body")
	}
	if ev.InputTok != 0 {
		t.Errorf("InputTok = %d, want 0 (cached clamped to prompt, never negative)", ev.InputTok)
	}
	if ev.CacheRead != 100 {
		t.Errorf("CacheRead = %d, want 100 (cached clamped to prompt)", ev.CacheRead)
	}
	if ev.CostMicro != 531 {
		t.Errorf("CostMicro = %d, want 531 (0 input + 100 cache_read@0.25x + 50 output)", ev.CostMicro)
	}
}

// TestParseGemini_ThoughtsOnlyResponseNotDropped pins the widened zero-usage
// guard (#122): a response with only thoughtsTokenCount is real billable spend
// and must NOT be dropped as empty. Regression guard for reverting the guard to
// the pre-#122 two-field form.
func TestParseGemini_ThoughtsOnlyResponseNotDropped(t *testing.T) {
	t.Parallel()
	// gemini-2.5-pro $1.25/$10.00. prompt 0, candidates 0, thoughts 800.
	// Input 0, CacheRead 0, Output 800. cost = 800*10.00/1e6 = 0.008 = 8_000 micro.
	body := []byte(`{
		"responseId":"gem-thoughts-only",
		"modelVersion":"gemini-2.5-pro",
		"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0,"thoughtsTokenCount":800}
	}`)
	ev := parseGemini(body, "carol", "issue-122", "")
	if ev == nil {
		t.Fatal("parseGemini dropped a thoughts-only response; it is billable spend")
	}
	if ev.InputTok != 0 {
		t.Errorf("InputTok = %d, want 0", ev.InputTok)
	}
	if ev.OutputTok != 800 {
		t.Errorf("OutputTok = %d, want 800 (thoughts-only, output rate)", ev.OutputTok)
	}
	if ev.CostMicro != 8_000 {
		t.Errorf("CostMicro = %d, want 8_000 (800 thoughts at $10/M output)", ev.CostMicro)
	}
}

// TestParseGemini_NegativeCachedAndThoughtsClamped pins that the #121 clamp
// covers the two new fields: a negative cachedContentTokenCount must not inflate
// Input (via prompt - negative) or push CacheRead below 0, and negative thoughts
// must not reduce Output below candidates.
func TestParseGemini_NegativeCachedAndThoughtsClamped(t *testing.T) {
	t.Parallel()
	// prompt 100, candidates 20, cached -50, thoughts -10. After clamp: cached 0,
	// thoughts 0. Input = 100 - 0 = 100; CacheRead = 0; Output = 20 + 0 = 20.
	// cost = 100*1.25/1e6 + 20*10.00/1e6 = 0.000125 + 0.0002 = 0.000325 = 325 micro.
	body := []byte(`{
		"responseId":"gem-neg-cache",
		"modelVersion":"gemini-2.5-pro",
		"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"cachedContentTokenCount":-50,"thoughtsTokenCount":-10}
	}`)
	ev := parseGemini(body, "carol", "issue-121", "")
	if ev == nil {
		t.Fatal("parseGemini returned nil; clamp must keep the event, not drop it")
	}
	if ev.InputTok != 100 {
		t.Errorf("InputTok = %d, want 100 (negative cached must not inflate input)", ev.InputTok)
	}
	if ev.CacheRead != 0 {
		t.Errorf("CacheRead = %d, want 0 (negative cached clamped)", ev.CacheRead)
	}
	if ev.OutputTok != 20 {
		t.Errorf("OutputTok = %d, want 20 (negative thoughts clamped)", ev.OutputTok)
	}
	if ev.CostMicro != 325 {
		t.Errorf("CostMicro = %d, want 325 (clamped, never negative)", ev.CostMicro)
	}
}

// --- Header hygiene: strip X-Tier-* upstream, default missing developer (#129) ---

// TestProxy_StripsTierAttributionHeadersUpstream pins review B5: the internal
// attribution headers X-Tier-Developer / X-Tier-Issue must NOT reach the
// upstream provider (they leak employee identities + issue numbers to
// Anthropic/OpenAI), mirroring the existing X-Tier-Token strip. The second half
// — asserting the STORED event still carries the correct developer/issue — pins
// the read-then-delete trap: a naive delete-only fix passes the strip assertion
// but destroys attribution because modifyResponse reads resp.Request (the
// OUTBOUND request) after the headers are gone. Fails on main (headers arrive
// upstream verbatim).
func TestProxy_StripsTierAttributionHeadersUpstream(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var gotDev, gotIssue string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotDev = r.Header.Get("X-Tier-Developer")
		gotIssue = r.Header.Get("X-Tier-Issue")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_hdr","model":"claude-sonnet-4","usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	sink := newMemSink()
	p := New(target, ProviderAnthropic, collector.SourceProxy, sink, nil, nil, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tier-Developer", "alice")
	req.Header.Set("X-Tier-Issue", "issue-7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if gotDev != "" {
		t.Errorf("upstream saw X-Tier-Developer = %q, want empty (internal identity must not leak)", gotDev)
	}
	if gotIssue != "" {
		t.Errorf("upstream saw X-Tier-Issue = %q, want empty (internal issue must not leak)", gotIssue)
	}
	// Read-then-delete trap: attribution must still land on the stored event.
	if len(sink.events) != 1 {
		t.Fatalf("expected 1 stored event, got %d", len(sink.events))
	}
	if sink.events[0].Developer != "alice" {
		t.Errorf("stored developer = %q, want alice (attribution destroyed by header strip)", sink.events[0].Developer)
	}
	if sink.events[0].IssueID != "issue-7" {
		t.Errorf("stored issue = %q, want issue-7 (attribution destroyed by header strip)", sink.events[0].IssueID)
	}
}

// TestProxy_MissingDeveloperDefaultsUnattributed pins review B6: a proxied
// request with no X-Tier-Developer must store developer="unattributed",
// symmetric with the existing missing-issue fallback — otherwise it stores ""
// and vanishes from every per-developer aggregate with no signal. Fails on main
// (stores empty string).
func TestProxy_MissingDeveloperDefaultsUnattributed(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_unattr","model":"claude-sonnet-4","usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	sink := newMemSink()
	p := New(target, ProviderAnthropic, collector.SourceProxy, sink, nil, nil, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	// X-Tier-Issue set, X-Tier-Developer deliberately absent.
	req.Header.Set("X-Tier-Issue", "issue-7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if len(sink.events) != 1 {
		t.Fatalf("expected 1 stored event, got %d", len(sink.events))
	}
	if sink.events[0].Developer != collector.UnattributedIssueID {
		t.Errorf("stored developer = %q, want %q (missing developer must default, symmetric with issue)",
			sink.events[0].Developer, collector.UnattributedIssueID)
	}
	if sink.events[0].IssueID != "issue-7" {
		t.Errorf("stored issue = %q, want issue-7 (present issue must be preserved)", sink.events[0].IssueID)
	}
}

// TestProxy_SSE_AttributionSurvivesHeaderStrip covers the streaming leg of the
// read-then-delete trap: the SSE emit fires at stream Close on the
// context.WithoutCancel-detached context, so it must read attribution from the
// context carrier set in Rewrite — not from the (now-stripped) request headers.
// Asserts the emitted event carries the request's developer/issue AND the
// upstream saw neither header. Deterministic: drains the whole body so Close
// runs before the assertions.
func TestProxy_SSE_AttributionSurvivesHeaderStrip(t *testing.T) {
	t.Parallel()
	const respID = "msg_sse_hdr"
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
	var mu sync.Mutex
	var gotDev, gotIssue string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotDev = r.Header.Get("X-Tier-Developer")
		gotIssue = r.Header.Get("X-Tier-Issue")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streamBody)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	sink := newMemSink()
	// The SSE emit fires on the server goroutine at stream Close — AFTER the client
	// already received EOF — so, unlike the JSON path, reading sink right after the
	// client drain would race the store write. Sync on the write recorder's channel
	// (recordWrite runs immediately after InsertTokenEvent returns) rather than on
	// timing, matching TestProxy_RecordsWriteOutcome_SSE_EndToEnd.
	rec := &recRecorder{ch: make(chan struct{}, 1)}
	p := New(target, ProviderAnthropic, collector.SourceProxy, sink, rec, nil, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tier-Developer", "alice")
	req.Header.Set("X-Tier-Issue", "issue-7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	// Fully drain — the streaming emit fires at Close, called only after the body
	// is fully read by the ReverseProxy machinery.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case <-rec.ch: // store write completed on the server goroutine
	case <-time.After(2 * time.Second):
		t.Fatal("streaming emit never recorded a write outcome")
	}

	mu.Lock()
	if gotDev != "" || gotIssue != "" {
		t.Errorf("upstream saw X-Tier-Developer=%q X-Tier-Issue=%q, want both empty (leak on SSE path)", gotDev, gotIssue)
	}
	mu.Unlock()

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 1 {
		t.Fatalf("expected 1 stored event, got %d", len(sink.events))
	}
	if sink.events[0].Developer != "alice" {
		t.Errorf("stored developer = %q, want alice (SSE attribution lost to WithoutCancel detach)", sink.events[0].Developer)
	}
	if sink.events[0].IssueID != "issue-7" {
		t.Errorf("stored issue = %q, want issue-7 (SSE attribution lost to WithoutCancel detach)", sink.events[0].IssueID)
	}
	// End-to-end proof (#337) that the streaming path is source-stamped through the
	// REAL ServeHTTP wiring, not just the streamCapture unit: the sink handed to New
	// is bare, so New's internal tagger is the only thing that can label this event.
	if sink.events[0].Source != collector.SourceProxy {
		t.Errorf("stored Source = %q, want %q (SSE path must be source-stamped by New's internal tagger, #337)", sink.events[0].Source, collector.SourceProxy)
	}
}

// TestProxy_UnattributedCounter pins the tier_proxy_unattributed_total{header}
// contract: exactly one Inc("developer") + one Inc("issue") for a 2xx missing
// both headers; zero for a fully-attributed request; and a nil recorder never
// panics. Drives the real Proxy end-to-end through the JSON path (resolution and
// counting both happen synchronously in modifyResponse before the client EOF).
func TestProxy_UnattributedCounter(t *testing.T) {
	t.Parallel()
	newUpstream := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg_ctr","model":"claude-sonnet-4","usage":{"input_tokens":10,"output_tokens":5}}`)
		}))
	}
	// send fires one request, optionally with the given headers set, and returns
	// the recorder's captured Inc label values.
	send := func(t *testing.T, setRecorder bool, headers map[string]string) []string {
		t.Helper()
		upstream := newUpstream()
		defer upstream.Close()
		target, _ := url.Parse(upstream.URL)
		rec := &recRecorder{}
		p := New(target, ProviderAnthropic, collector.SourceProxy, newMemSink(), nil, nil, nil)
		if setRecorder {
			p.SetUnattributedRecorder(rec)
		}
		proxySrv := httptest.NewServer(p)
		defer proxySrv.Close()

		req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return rec.snapshot()
	}

	t.Run("missing all three increments each once", func(t *testing.T) {
		t.Parallel()
		got := send(t, true, nil)
		if len(got) != 3 {
			t.Fatalf("recorder calls = %v, want 3 (developer + issue + repo)", got)
		}
		var dev, iss, repo int
		for _, c := range got {
			switch c {
			case "developer":
				dev++
			case "issue":
				iss++
			case "repo":
				repo++
			default:
				t.Errorf("unexpected label %q, want developer|issue|repo", c)
			}
		}
		if dev != 1 || iss != 1 || repo != 1 {
			t.Errorf("developer=%d issue=%d repo=%d, want 1 each", dev, iss, repo)
		}
	})

	// #231: a client that sets developer + issue but NOT X-Tier-Repo is genuinely
	// repo-blind. Its cost still joins tolerantly, but the counter must say so —
	// this is the signal a multi-repo org reads to learn why its issues sharing a
	// number still fuse. Silence here would be the bug.
	t.Run("missing only repo increments repo", func(t *testing.T) {
		t.Parallel()
		got := send(t, true, map[string]string{"X-Tier-Developer": "alice", "X-Tier-Issue": "issue-7"})
		if len(got) != 1 || got[0] != "repo" {
			t.Errorf("recorder calls = %v, want exactly [repo]", got)
		}
	})

	t.Run("fully attributed increments nothing", func(t *testing.T) {
		t.Parallel()
		got := send(t, true, map[string]string{
			"X-Tier-Developer": "alice",
			"X-Tier-Issue":     "issue-7",
			"X-Tier-Repo":      "acme/app",
		})
		if len(got) != 0 {
			t.Errorf("recorder calls = %v, want none (fully attributed)", got)
		}
	})

	t.Run("nil recorder does not panic", func(t *testing.T) {
		t.Parallel()
		// setRecorder=false leaves the unattributed recorder nil; a header-less
		// request must resolve to "unattributed" without panicking.
		if got := send(t, false, nil); len(got) != 0 {
			t.Errorf("recorder calls = %v, want none (nil recorder)", got)
		}
	})
}

// TestProxy_RejectedRepoHeaderIsNotLogged is a security regression guard (#231,
// CodeQL go/log-injection + go/clear-text-logging).
//
// X-Tier-Repo is an unauthenticated, attacker-controlled request header. Echoing a
// rejected value into a structured log lets a caller forge log entries and dumps
// request-header content into logs. The rejection must be observable via the
// counter and the value's LENGTH — never the value itself.
func TestProxy_RejectedRepoHeaderIsNotLogged(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_canary","model":"claude-sonnet-4","usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := New(target, ProviderAnthropic, collector.SourceProxy, newMemSink(), nil, nil, logger)

	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	const marker = "CANARY-SHOULD-NOT-APPEAR-IN-LOGS"
	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tier-Developer", "alice")
	req.Header.Set("X-Tier-Issue", "issue-7")
	req.Header.Set("X-Tier-Repo", marker) // not a canonical owner/repo slug
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	logs := logBuf.String()
	if strings.Contains(logs, marker) {
		t.Fatalf("the rejected X-Tier-Repo value was echoed into the log — log injection / clear-text logging of a request header:\n%s", logs)
	}
	// It must still be observable that something was rejected.
	if !strings.Contains(logs, "non-canonical X-Tier-Repo") {
		t.Errorf("rejection was silent; expected a warning without the value. logs:\n%s", logs)
	}
}

// TestProxy_PathNotForgeable is a security regression guard (#321, CodeQL
// go/log-injection; sibling of TestProxy_RejectedRepoHeaderIsNotLogged).
//
// errorHandler logs r.URL.Path, which is percent-decoded from the client's request
// target — so a path like "/v1/messages%0a..." carries a real newline. Logged raw
// under a text handler, that newline forges a second, standalone log record an
// operator or a line-oriented SIEM reads as genuine. logsafe.Str must strip it.
func TestProxy_PathNotForgeable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := &Proxy{logger: logger}

	const forgedMarker = `level=ERROR msg="tier: auth bypassed"`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	// A percent-decoded target lands in r.URL.Path with a real CR/LF; set it directly
	// so the test does not depend on the client's target-parsing quirks.
	req.URL.Path = "evilpath\ntime=2026-07-12T00:00:00Z " + forgedMarker

	p.errorHandler(httptest.NewRecorder(), req, errors.New("dial tcp: connection refused"))

	logs := buf.String()
	if !strings.Contains(logs, "proxy error") {
		t.Fatalf("proxy-error diagnostic was not logged; lost:\n%s", logs)
	}
	// The forged record must never begin its own line.
	for _, line := range strings.Split(logs, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), forgedMarker) {
			t.Fatalf("request path forged a standalone log record - log injection:\n%s", logs)
		}
	}
	// MECHANISM PIN + STRIP PROOF: CR/LF stripped and value %q-quoted, so the halves
	// join as `path="\"evilpathtime=...`. Removing logsafe.Str makes this fail.
	if !strings.Contains(logs, `path="\"evilpathtime=`) {
		t.Errorf("path not stripped+rendered via logsafe.Str (expected `path=\"\\\"evilpathtime=...`):\n%s", logs)
	}
}

// TestProxy_UpstreamDown_502NoEventNoHang drives errorHandler from a DIFFERENT
// failure than TestProxy_UpstreamMidBodyErrorReturns502 (which triggers a
// mid-body read error): here the upstream is unreachable (connection refused),
// the other common production failure. It pins the whole developer-visible
// contract when Claude Code's proxied call cannot reach the provider:
//   - status 502 (not a masked 200/500);
//   - body is EXACTLY "proxy error\n" — the generic sentinel, NOT the underlying
//     dial error. A future handler that echoed err.Error() would leak the
//     internal target host/port into the developer's CLI; this exact-body pin
//     makes that regression fail the test;
//   - zero token events reach the sink (an upstream failure must never fabricate
//     a $-charged event);
//   - the request returns well within a hard client timeout (no hang).
func TestProxy_UpstreamDown_502NoEventNoHang(t *testing.T) {
	t.Parallel()

	// Reserve a loopback port, then release it so nothing is listening — the
	// address is guaranteed connection-refused, the ErrorHandler trigger.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	deadURL := "http://" + ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}
	target, err := url.Parse(deadURL)
	if err != nil {
		t.Fatalf("parse dead target %q: %v", deadURL, err)
	}

	sink := newMemSink()
	// Quiet logger: errorHandler logs at Error level; discard to keep test
	// output clean. slog.New(discard) rather than nil so we don't depend on the
	// process-global slog.Default().
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New(target, ProviderAnthropic, collector.SourceProxy, sink, nil, nil, quiet)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	// Hard client timeout is the no-hang enforcement: if the proxy ever blocked,
	// client.Do returns a timeout error and the type assertion below fails loudly.
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		// A timeout here means the handler hung; any other error is also a
		// contract failure (the proxy must always answer 502, never drop).
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			t.Fatalf("request timed out — errorHandler hung instead of returning 502: %v", err)
		}
		t.Fatalf("client.Do: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (unreachable upstream must surface as Bad Gateway)", resp.StatusCode)
	}
	// http.Error appends a trailing newline; the exact match is the anti-leak pin.
	if string(got) != "proxy error\n" {
		t.Errorf("body = %q, want exactly %q (must not echo the underlying dial error to the CLI)", got, "proxy error\n")
	}
	if len(sink.events) != 0 {
		t.Errorf("sink recorded %d events, want 0 (an upstream failure must not fabricate a token event)", len(sink.events))
	}
}
