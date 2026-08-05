package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// OpenAI Responses-API parsing (#459 task 2) — JSON and SSE.
//
// 🔴 FIXTURE PROVENANCE: testdata/openai-responses-*.{json,sse} are SYNTHETIC,
// written from the documented Responses schema. Nothing here has round-tripped
// live traffic — no credential exists in this workspace — so these tests pin a
// PARSER CONTRACT, not a wire observation. The evidence for the one semantic
// judgment they encode (input_tokens is inclusive of cached_tokens) is stated in
// testdata/PROVENANCE.md and in the openAIResponsesUsage doc, and comes from
// internal/collector/codexrollout's containment checks over real captured Codex
// sessions. Live confirmation is #459 task 3, which is credential-blocked.

// gpt-5.3-codex is priced at $1.75/M input, $14.00/M output, cache_read_mult
// 0.10 (internal/store/prices.yaml). The fixtures bill 12 000 input tokens of
// which 10 000 are cached, plus 1 000 output.
const (
	fixtureInputTokens  = 12000
	fixtureCachedTokens = 10000
	fixtureOutputTokens = 1000

	// wantFixtureCostMicro is the INCLUSIVE reading: 2 000 fresh input tokens at
	// $1.75/M = 3 500 µ$, 10 000 cached at 0.10 × $1.75/M = 1 750 µ$, 1 000
	// output at $14/M = 14 000 µ$.
	wantFixtureCostMicro = 3500 + 1750 + 14000 // 19 250

	// wantAdditiveCostMicro is what the SAME payload costs under the WRONG
	// (additive) reading, where cached is charged on top of a full-price
	// input_tokens instead of carved out of it: 12 000 × $1.75/M = 21 000 µ$ +
	// 1 750 + 14 000. Named so the money test states the defect it excludes
	// rather than only the value it expects.
	wantAdditiveCostMicro = 21000 + 1750 + 14000 // 36 750
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// --- Shape discrimination ---

// TestOpenAIShape_DiscriminatesResponsesFromChatCompletions pins the
// discriminator itself, separately from either parser. Both shapes arrive on the
// same /openai/ route, so this function is the only thing standing between a
// Responses body and a Chat Completions parse (and vice versa) — and its DEFAULT
// is what guarantees the change is additive: anything not positively identified
// as Responses keeps its pre-#459 route.
func TestOpenAIShape_DiscriminatesResponsesFromChatCompletions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want openAIWireShape
	}{
		{
			name: "chat completions with object",
			body: `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o","usage":{"prompt_tokens":10,"completion_tokens":5}}`,
			want: shapeChatCompletions,
		},
		{
			name: "chat completions without object (compatible upstream)",
			body: `{"id":"chatcmpl-1","model":"llama-3.1-70b","usage":{"prompt_tokens":10,"completion_tokens":5}}`,
			want: shapeChatCompletions,
		},
		{
			name: "responses with object",
			body: `{"id":"resp_1","object":"response","model":"gpt-5.3-codex","usage":{"input_tokens":10,"output_tokens":5}}`,
			want: shapeResponses,
		},
		{
			name: "responses identified by usage keys alone",
			body: `{"id":"resp_1","model":"gpt-5.3-codex","usage":{"input_tokens":10,"output_tokens":5}}`,
			want: shapeResponses,
		},
		{
			// Key PRESENCE, not value: an all-zero Responses usage block is still
			// the Responses shape (the parser then declines it as zero-usage,
			// which is a different decision made in a different place).
			name: "responses with zero counts is still the responses shape",
			body: `{"id":"resp_1","model":"gpt-5.3-codex","usage":{"input_tokens":0,"output_tokens":0}}`,
			want: shapeResponses,
		},
		{
			// Isolates RULE 1's chat branch. The usage block carries ONLY
			// Responses keys, so rule 2 would say Responses — only the explicit
			// `object` stops it. Deleting `case "chat.completion"` from
			// openAIShape flips this row and nothing else in the suite, which is
			// the point: it is the sole guard against a decorated
			// chat.completion being re-routed out from under the incumbent path.
			name: "chat.completion object wins even when usage carries only responses keys",
			body: `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o","usage":{"input_tokens":10,"output_tokens":5}}`,
			want: shapeChatCompletions,
		},
		{
			// Rule 2's mutual exclusion, with the object present. Deliberately
			// NOT named for rule 1 — it carries prompt_tokens, so rule 2 alone
			// already routes it to chat.
			name: "hybrid usage stays on chat (object present)",
			body: `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o","usage":{"prompt_tokens":10,"completion_tokens":5,"input_tokens":10,"output_tokens":5}}`,
			want: shapeChatCompletions,
		},
		{
			// Same protection without an object field: the two key families are
			// mutually exclusive, so a hybrid stays on the path it already worked
			// on.
			name: "hybrid usage without object stays on chat",
			body: `{"id":"x","model":"gpt-4o","usage":{"prompt_tokens":10,"completion_tokens":5,"input_tokens":10}}`,
			want: shapeChatCompletions,
		},
		{
			// Pins the fast path's OUTCOME-NEUTRALITY. This body has no
			// "input_tokens" key, so screening on that key alone would route it
			// to Chat Completions and capture nothing — while rules 1 and 2 both
			// say Responses and parseOpenAIResponses prices it. Not a shape
			// OpenAI emits; the point is that the fast path must never be able to
			// decide something the rules decide differently.
			name: "responses with only output_tokens is not lost to the fast path",
			body: `{"id":"resp_o","object":"response","model":"gpt-5.3-codex","usage":{"output_tokens":500}}`,
			want: shapeResponses,
		},
		{
			// An Anthropic body names its counts input_tokens/output_tokens too,
			// so on the OPENAI route it reads as the Responses shape. That is
			// recorded here as a known, accepted consequence rather than papered
			// over: it is reachable only by pointing an Anthropic client at
			// /openai/ (a misconfiguration — the provider comes from the mount),
			// and the outcome is strictly better than before, where such a body
			// captured nothing at all. It is imperfect: Anthropic's
			// cache_read_input_tokens has no Responses equivalent and is dropped,
			// so a misrouted Anthropic call undercounts its cache class. Fix the
			// route, not the discriminator.
			name: "anthropic body on the openai route reads as responses",
			body: `{"id":"msg_1","model":"claude-sonnet-4","usage":{"input_tokens":10,"output_tokens":5}}`,
			want: shapeResponses,
		},
		{
			name: "malformed json defaults to chat",
			body: `{"input_tokens": `,
			want: shapeChatCompletions,
		},
		{
			name: "empty body defaults to chat",
			body: ``,
			want: shapeChatCompletions,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := openAIShape([]byte(tc.body)); got != tc.want {
				t.Errorf("openAIShape() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseOpenAI_ChatCompletionsUnchangedByResponsesDispatch is the
// don't-disturb-the-incumbent guard: parseOpenAI now dispatches on shape, and a
// Chat Completions body must come out the far side with exactly the numbers it
// produced before the dispatcher existed.
func TestParseOpenAI_ChatCompletionsUnchangedByResponsesDispatch(t *testing.T) {
	t.Parallel()
	body := []byte(`{"id":"chatcmpl-guard","object":"chat.completion","model":"gpt-4o","usage":{"prompt_tokens":500,"completion_tokens":300,"prompt_tokens_details":{"cached_tokens":50}}}`)
	ev := parseOpenAI(body, "bob", "issue-7", "")
	if ev == nil {
		t.Fatal("parseOpenAI returned nil for a Chat Completions body")
	}
	if ev.InputTok != 450 || ev.OutputTok != 300 || ev.CacheRead != 50 {
		t.Errorf("got input=%d output=%d cache=%d, want 450/300/50", ev.InputTok, ev.OutputTok, ev.CacheRead)
	}
	if want := collector.MessageIdempotencyKey(string(ProviderOpenAI), "chatcmpl-guard"); ev.IdempotencyKey != want {
		t.Errorf("IdempotencyKey = %q, want %q", ev.IdempotencyKey, want)
	}
}

// --- JSON path ---

// TestParseOpenAIResponses_CachedIsCarvedOutOfInput is the money test.
//
// usage.input_tokens is INCLUSIVE of input_tokens_details.cached_tokens, so the
// cached prefix must be carved OUT of the input class before pricing. Charging
// it in both classes overbills — here by 91% (36 750 µ$ vs 19 250 µ$), and by
// far more on a heavily-cached agentic session, which is exactly what Codex
// traffic is. Both numbers are named so the test states which reading it
// excludes, not merely which one it expects.
func TestParseOpenAIResponses_CachedIsCarvedOutOfInput(t *testing.T) {
	t.Parallel()
	ev := parseOpenAI(readFixture(t, "openai-responses-completed.json"), "alice", "issue-459", "")
	if ev == nil {
		t.Fatal("parseOpenAI returned nil for a Responses-API body")
	}
	if ev.Model != "gpt-5.3-codex" {
		t.Errorf("Model = %q, want gpt-5.3-codex", ev.Model)
	}
	if ev.InputTok != fixtureInputTokens-fixtureCachedTokens {
		t.Errorf("InputTok = %d, want %d (input_tokens %d - cached %d)",
			ev.InputTok, fixtureInputTokens-fixtureCachedTokens, fixtureInputTokens, fixtureCachedTokens)
	}
	if ev.CacheRead != fixtureCachedTokens {
		t.Errorf("CacheRead = %d, want %d", ev.CacheRead, fixtureCachedTokens)
	}
	// The containment identity, asserted rather than assumed: the two input
	// classes partition input_tokens exactly — no token counted twice, none lost.
	if ev.InputTok+ev.CacheRead != fixtureInputTokens {
		t.Errorf("InputTok+CacheRead = %d, want %d (the two classes must partition input_tokens)",
			ev.InputTok+ev.CacheRead, fixtureInputTokens)
	}
	if ev.CostMicro != wantFixtureCostMicro {
		t.Errorf("CostMicro = %d, want %d (inclusive reading). The additive misreading costs %d.",
			ev.CostMicro, wantFixtureCostMicro, wantAdditiveCostMicro)
	}
}

// TestParseOpenAIResponses_ReasoningTokensDoNotInflateOutput guards the OTHER
// direction of the same containment question. output_tokens_details.
// reasoning_tokens is a SUBSET of output_tokens, so folding it in would bill
// reasoning twice — the mirror image of the cached defect. This is the opposite
// of Gemini's thoughtsTokenCount, which IS excluded from its parent and so must
// be added (#122); the two look alike and must not be "made consistent".
func TestParseOpenAIResponses_ReasoningTokensDoNotInflateOutput(t *testing.T) {
	t.Parallel()
	// The fixture must PROVE its own premise first. Asserting only
	// OutputTok == 1000 would stay green if someone deleted output_tokens_details
	// from the fixture — the test would then guard nothing while its name still
	// claimed it did.
	var raw struct {
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			TotalTokens        int `json:"total_tokens"`
			InputTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	body := readFixture(t, "openai-responses-completed.json")
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	if raw.Usage.OutputTokensDetails == nil || raw.Usage.OutputTokensDetails.ReasoningTokens == 0 {
		t.Fatal("fixture carries no output_tokens_details.reasoning_tokens — this test would be vacuous")
	}
	if raw.Usage.OutputTokensDetails.ReasoningTokens >= raw.Usage.OutputTokens {
		t.Fatalf("fixture is not a subset case: reasoning_tokens=%d >= output_tokens=%d",
			raw.Usage.OutputTokensDetails.ReasoningTokens, raw.Usage.OutputTokens)
	}
	// The containment identities the whole inclusive-cached argument rests on
	// (see testdata/PROVENANCE.md), asserted on the fixture itself so it cannot
	// drift away from codexrollout's checkContainment.
	if raw.Usage.TotalTokens != raw.Usage.InputTokens+raw.Usage.OutputTokens {
		t.Errorf("fixture breaks containment: total_tokens=%d != input+output=%d",
			raw.Usage.TotalTokens, raw.Usage.InputTokens+raw.Usage.OutputTokens)
	}
	if raw.Usage.InputTokensDetails == nil || raw.Usage.InputTokensDetails.CachedTokens > raw.Usage.InputTokens {
		t.Error("fixture breaks containment: cached_tokens must be present and <= input_tokens")
	}

	ev := parseOpenAI(body, "alice", "issue-459", "")
	if ev == nil {
		t.Fatal("parseOpenAI returned nil")
	}
	if ev.OutputTok != fixtureOutputTokens {
		t.Errorf("OutputTok = %d, want %d (output_tokens verbatim; reasoning_tokens is inside it, not additional)",
			ev.OutputTok, fixtureOutputTokens)
	}
}

// TestParseOpenAIResponses_ZeroUsageReturnsNil pins the zero-usage guard on the
// JSON path. A terminal Responses body reporting 0/0 is "nothing billable",
// never "an event worth $0": storing it would add a row representing no work AND
// burn the resp_* idempotency key, so a later re-ingest carrying the real cost
// trips store.ErrCostConflict instead of deduping. The E2E arm below proves the
// uncaptured counter still fires for it.
func TestParseOpenAIResponses_ZeroUsageReturnsNil(t *testing.T) {
	t.Parallel()
	body := []byte(`{"id":"resp_zero","object":"response","status":"completed","model":"gpt-5.3-codex","usage":{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":0}}}`)
	if ev := parseOpenAIResponses(body, "alice", "issue-459", ""); ev != nil {
		t.Errorf("parseOpenAIResponses: expected nil for zero usage, got %+v", ev)
	}
	if ev := parseOpenAI(body, "alice", "issue-459", ""); ev != nil {
		t.Errorf("parseOpenAI: expected nil for zero usage, got %+v", ev)
	}
}

// TestParseOpenAIResponses_NoInputTokensDetails: Azure / LiteLLM / OpenRouter
// `/responses` shims omit the details object entirely. Absent details means no
// cached portion — the whole input is fresh, and nothing may be carved out.
func TestParseOpenAIResponses_NoInputTokensDetails(t *testing.T) {
	t.Parallel()
	body := []byte(`{"id":"resp_nodetails","object":"response","model":"gpt-5.3-codex","usage":{"input_tokens":900,"output_tokens":100,"total_tokens":1000}}`)
	ev := parseOpenAI(body, "alice", "issue-459", "")
	if ev == nil {
		t.Fatal("parseOpenAI returned nil for a Responses body without input_tokens_details")
	}
	if ev.InputTok != 900 || ev.CacheRead != 0 || ev.OutputTok != 100 {
		t.Errorf("got input=%d cache=%d output=%d, want 900/0/100", ev.InputTok, ev.CacheRead, ev.OutputTok)
	}
}

// TestParseOpenAIResponses_EmitsIdempotencyKeyFromResponseID pins the dedup
// anchor: Responses ids are resp_*, and they share the "openai" namespace with
// chatcmpl-* so a numeric collision across vendors stays namespaced away.
func TestParseOpenAIResponses_EmitsIdempotencyKeyFromResponseID(t *testing.T) {
	t.Parallel()
	ev := parseOpenAI(readFixture(t, "openai-responses-completed.json"), "alice", "issue-459", "")
	if ev == nil {
		t.Fatal("parseOpenAI returned nil")
	}
	want := collector.MessageIdempotencyKey(string(ProviderOpenAI), "resp_synthetic_json_0001")
	if ev.IdempotencyKey != want {
		t.Errorf("IdempotencyKey = %q, want %q", ev.IdempotencyKey, want)
	}
	if ev.Fidelity != collector.FidelityRealtime {
		t.Errorf("Fidelity = %q, want %q", ev.Fidelity, collector.FidelityRealtime)
	}
}

// TestParseOpenAIResponses_ContradictoryCachedClampsInputToZero: a payload
// claiming more cached tokens than input tokens is impossible, but it must not
// produce a NEGATIVE fresh-input count (and so a negative cost). Same policy as
// the Chat Completions path (#114) — clamp, never reject: the proxy sits on the
// request path.
func TestParseOpenAIResponses_ContradictoryCachedClampsInputToZero(t *testing.T) {
	t.Parallel()
	body := []byte(`{"id":"resp_bad","object":"response","model":"gpt-5.3-codex","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":9999},"output_tokens":10}}`)
	ev := parseOpenAI(body, "alice", "issue-459", "")
	if ev == nil {
		t.Fatal("parseOpenAI returned nil; a contradictory payload must still be captured, clamped")
	}
	if ev.InputTok != 0 {
		t.Errorf("InputTok = %d, want 0 (cached clamped to input, never negative)", ev.InputTok)
	}
	if ev.CacheRead != 100 {
		t.Errorf("CacheRead = %d, want 100 (clamped to input_tokens)", ev.CacheRead)
	}
	if ev.CostMicro < 0 {
		t.Errorf("CostMicro = %d, must never be negative", ev.CostMicro)
	}
}

// TestParseOpenAIResponses_NullUsageReturnsNil: the Responses object carries
// "usage": null until it reaches a terminal status. Null usage is "nothing to
// bill", not "zero tokens", and must not become a $0 event.
//
// It calls parseOpenAIResponses DIRECTLY on purpose. Routed through parseOpenAI
// this body never reaches the guard — with usage null there is no "input_tokens"
// key anywhere, so openAIShape's fast path sends it to the Chat Completions
// parser, which also returns nil. The dispatcher-level assertion below records
// that the OUTCOME is the same either way; the direct call is what actually pins
// the nil-usage guard, and dropping the guard is a nil dereference, not a
// wrong number.
func TestParseOpenAIResponses_NullUsageReturnsNil(t *testing.T) {
	t.Parallel()
	body := []byte(`{"id":"resp_pending","object":"response","status":"in_progress","model":"gpt-5.3-codex","usage":null}`)
	if ev := parseOpenAIResponses(body, "alice", "issue-459", ""); ev != nil {
		t.Errorf("parseOpenAIResponses: expected nil for a null-usage body, got %+v", ev)
	}
	if ev := parseOpenAI(body, "alice", "issue-459", ""); ev != nil {
		t.Errorf("parseOpenAI: expected nil for a null-usage body, got %+v", ev)
	}
}

// TestParseOpenAIResponses_StampsHostAndBillingMode: the Responses path must
// carry the SAME host/billing_mode treatment as Chat Completions (#300) — one
// convention, not a second one. Asserted by feeding both shapes the same counts
// on the same host and requiring identical Host, BillingMode and CostMicro.
//
// The model is a HOST-KEYED row from prices.yaml
// (meta-llama/llama-3.3-70b-instruct@openrouter.ai, billing_mode per_token), not
// an unpriced name: an unpriced model resolves through the size-class GUESS
// fallback, which prices both shapes identically for a reason that has nothing
// to do with #300 and logs a WARN on every run. Pinning the keyed lookup is what
// makes this a host-qualified-pricing test.
func TestParseOpenAIResponses_StampsHostAndBillingMode(t *testing.T) {
	t.Parallel()
	const host = "openrouter.ai"
	const model = "meta-llama/llama-3.3-70b-instruct"
	responses := parseOpenAI([]byte(`{"id":"resp_h","object":"response","model":"`+model+`","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":20},"output_tokens":50}}`), "bob", "issue-2", host)
	chat := parseOpenAI([]byte(`{"id":"chatcmpl-h","object":"chat.completion","model":"`+model+`","usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":20}}}`), "bob", "issue-2", host)
	if responses == nil || chat == nil {
		t.Fatal("both shapes must parse")
	}
	if responses.Host != host {
		t.Errorf("Host = %q, want %q", responses.Host, host)
	}
	if responses.BillingMode != store.BillingPerToken {
		t.Errorf("BillingMode = %q, want %q (the host-keyed price row sets it explicitly)", responses.BillingMode, store.BillingPerToken)
	}
	if responses.CostMicro == 0 {
		t.Error("CostMicro = 0; the host-keyed price row must produce a real cost or this test proves nothing")
	}
	if responses.BillingMode != chat.BillingMode || responses.Host != chat.Host || responses.CostMicro != chat.CostMicro {
		t.Errorf("Responses (host=%q mode=%q cost=%d) diverges from Chat Completions (host=%q mode=%q cost=%d) on identical counts",
			responses.Host, responses.BillingMode, responses.CostMicro, chat.Host, chat.BillingMode, chat.CostMicro)
	}
	if responses.InputTok != chat.InputTok || responses.CacheRead != chat.CacheRead || responses.OutputTok != chat.OutputTok {
		t.Errorf("token classes diverge: responses %d/%d/%d vs chat %d/%d/%d",
			responses.InputTok, responses.CacheRead, responses.OutputTok,
			chat.InputTok, chat.CacheRead, chat.OutputTok)
	}
}

// TestParseOpenAIResponses_NegativeTokensClamped: #121 applies to this shape too
// — a hostile or malformed body must not push a negative cost into the store.
func TestParseOpenAIResponses_NegativeTokensClamped(t *testing.T) {
	t.Parallel()
	body := []byte(`{"id":"resp_neg","object":"response","model":"gpt-5.3-codex","usage":{"input_tokens":-500,"input_tokens_details":{"cached_tokens":-10},"output_tokens":200}}`)
	ev := parseOpenAI(body, "alice", "issue-121", "")
	if ev == nil {
		t.Fatal("parseOpenAI returned nil; clamp must keep the event, not drop it")
	}
	if ev.InputTok < 0 || ev.CacheRead < 0 || ev.OutputTok < 0 || ev.CostMicro < 0 {
		t.Errorf("negative field survived the clamp: input=%d cache=%d output=%d cost=%d",
			ev.InputTok, ev.CacheRead, ev.OutputTok, ev.CostMicro)
	}
	if ev.OutputTok != 200 {
		t.Errorf("OutputTok = %d, want 200 (the valid field must survive)", ev.OutputTok)
	}
}

// --- SSE path ---

// TestOpenAIStreamRouter_ResponsesCompletedEventCarriesUsage runs the synthetic
// stream fixture through the real framer and the provider ROUTER — the same two
// pieces the proxy wires, minus streamCapture, which the end-to-end test further
// down covers. Named for the router because that is what it constructs: the
// leaf-only assertions live in the TestOpenAIResponsesStreamParser_* tests.
func TestOpenAIStreamRouter_ResponsesCompletedEventCarriesUsage(t *testing.T) {
	t.Parallel()
	p := &openAIStreamParser{}
	f := &sseFramer{onEvent: p.OnEvent}
	f.Feed(readFixture(t, "openai-responses-stream.sse"))
	f.Flush()

	ev := p.Finalise("alice", "issue-459", "")
	if ev == nil {
		t.Fatal("Finalise returned nil for a stream ending in response.completed")
	}
	if ev.InputTok != fixtureInputTokens-fixtureCachedTokens || ev.CacheRead != fixtureCachedTokens || ev.OutputTok != fixtureOutputTokens {
		t.Errorf("got input=%d cache=%d output=%d, want %d/%d/%d",
			ev.InputTok, ev.CacheRead, ev.OutputTok,
			fixtureInputTokens-fixtureCachedTokens, fixtureCachedTokens, fixtureOutputTokens)
	}
	if ev.CostMicro != wantFixtureCostMicro {
		t.Errorf("CostMicro = %d, want %d (inclusive reading; the additive misreading costs %d)",
			ev.CostMicro, wantFixtureCostMicro, wantAdditiveCostMicro)
	}
	if want := collector.MessageIdempotencyKey(string(ProviderOpenAI), "resp_synthetic_sse_0002"); ev.IdempotencyKey != want {
		t.Errorf("IdempotencyKey = %q, want %q", ev.IdempotencyKey, want)
	}
}

// TestOpenAIStreamParser_RoutesByEventNameWithoutDisturbingChat pins the SSE
// discriminator. The router claims only the `response.` event-name prefix;
// everything else — including the chat stream's empty event name — goes to the
// Chat Completions sub-parser it always went to.
func TestOpenAIStreamParser_RoutesByEventNameWithoutDisturbingChat(t *testing.T) {
	t.Parallel()

	t.Run("chat stream reaches the chat sub-parser", func(t *testing.T) {
		t.Parallel()
		p := &openAIStreamParser{}
		f := &sseFramer{onEvent: p.OnEvent}
		f.Feed([]byte(`data: {"id":"chatcmpl-r","model":"gpt-4o","usage":{"prompt_tokens":40,"completion_tokens":15,"prompt_tokens_details":{"cached_tokens":5}}}` + "\n\n"))
		f.Flush()
		if !p.chat.gotUsage {
			t.Error("chat sub-parser saw no usage — an eventless chunk must route to chat")
		}
		if p.responses.gotUsage {
			t.Error("responses sub-parser claimed an eventless chat chunk")
		}
		ev := p.Finalise("bob", "issue-7", "")
		if ev == nil || ev.InputTok != 35 || ev.OutputTok != 15 || ev.CacheRead != 5 {
			t.Errorf("chat result = %+v, want input=35 output=15 cache=5", ev)
		}
	})

	t.Run("responses stream reaches the responses sub-parser", func(t *testing.T) {
		t.Parallel()
		p := &openAIStreamParser{}
		f := &sseFramer{onEvent: p.OnEvent}
		f.Feed(readFixture(t, "openai-responses-stream.sse"))
		f.Flush()
		if !p.responses.gotUsage {
			t.Error("responses sub-parser saw no usage — a response.* event must route to it")
		}
		if p.chat.gotUsage {
			t.Error("chat sub-parser claimed a response.* event")
		}
	})
}

// feedResponsesLeaf drives the Responses sub-parser DIRECTLY through a real
// framer — no router. Tests named for the leaf must construct the leaf: routed
// through openAIStreamParser, a nil result is also what the Chat Completions
// sub-parser returns for the same bytes, so an assertion of nil would hold even
// with the routing broken.
func feedResponsesLeaf(t *testing.T, stream string) *openAIResponsesStreamParser {
	t.Helper()
	p := &openAIResponsesStreamParser{}
	f := &sseFramer{onEvent: p.OnEvent}
	f.Feed([]byte(stream))
	f.Flush()
	return p
}

// TestOpenAIResponsesStreamParser_AnyTerminalStatusIsBilled: response.completed
// is the usual terminal event but not the only one that carries usage — a
// response that hit max_output_tokens terminates as response.incomplete, and one
// that errored as response.failed. Those tokens were billed all the same, so the
// parser latches any usage-bearing event rather than matching one event name,
// and dropping either would take a deliberate change.
func TestOpenAIResponsesStreamParser_AnyTerminalStatusIsBilled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ event, status string }{
		{"response.completed", "completed"},
		{"response.incomplete", "incomplete"},
		{"response.failed", "failed"},
	} {
		t.Run(tc.event, func(t *testing.T) {
			t.Parallel()
			stream := strings.Join([]string{
				`event: response.created`,
				`data: {"type":"response.created","response":{"id":"resp_t","model":"gpt-5.3-codex","usage":null}}`,
				``,
				`event: ` + tc.event,
				`data: {"type":"` + tc.event + `","response":{"id":"resp_t","status":"` + tc.status + `","model":"gpt-5.3-codex","usage":{"input_tokens":300,"input_tokens_details":{"cached_tokens":100},"output_tokens":64}}}`,
				``,
			}, "\n")
			ev := feedResponsesLeaf(t, stream).Finalise("alice", "issue-459", "")
			if ev == nil {
				t.Fatalf("Finalise returned nil for a %s stream — those tokens were billed", tc.event)
			}
			if ev.InputTok != 200 || ev.CacheRead != 100 || ev.OutputTok != 64 {
				t.Errorf("got input=%d cache=%d output=%d, want 200/100/64", ev.InputTok, ev.CacheRead, ev.OutputTok)
			}
		})
	}
}

// TestOpenAIResponsesStreamParser_LastUsageWinsAndResetsCached pins the latch
// semantics, which no single-usage-event stream can reach.
//
// Two things at once, both money bugs if wrong: the second usage block REPLACES
// the first (accumulating would bill the prefix twice), and a terminal block
// that omits input_tokens_details resets cached to 0 rather than inheriting the
// earlier count — inheriting would carve tokens out of an input total that no
// longer contains them.
func TestOpenAIResponsesStreamParser_LastUsageWinsAndResetsCached(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`event: response.in_progress`,
		`data: {"type":"response.in_progress","response":{"id":"resp_two","model":"gpt-5.3-codex","usage":{"input_tokens":500,"input_tokens_details":{"cached_tokens":400},"output_tokens":10}}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_two","status":"completed","model":"gpt-5.3-codex","usage":{"input_tokens":900,"output_tokens":120}}}`,
		``,
	}, "\n")
	ev := feedResponsesLeaf(t, stream).Finalise("alice", "issue-459", "")
	if ev == nil {
		t.Fatal("Finalise returned nil")
	}
	if ev.InputTok != 900 {
		t.Errorf("InputTok = %d, want 900 (last usage REPLACES the first; 1400 would mean it accumulated, 500 that the first won)", ev.InputTok)
	}
	if ev.CacheRead != 0 {
		t.Errorf("CacheRead = %d, want 0 (the terminal block omits input_tokens_details; 400 would mean a stale cached count was inherited)", ev.CacheRead)
	}
	if ev.OutputTok != 120 {
		t.Errorf("OutputTok = %d, want 120", ev.OutputTok)
	}
}

// TestOpenAIResponsesStreamParser_ZeroedTrailerDoesNotDestroyCount: last-wins
// must not be UNCONDITIONAL. A trailing usage block of 0/0 after a real one
// would otherwise wipe a correct count to zero — the defect
// anthropicStreamParser guards with `if md.Usage.OutputTokens > 0`.
func TestOpenAIResponsesStreamParser_ZeroedTrailerDoesNotDestroyCount(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_z","status":"completed","model":"gpt-5.3-codex","usage":{"input_tokens":2000,"input_tokens_details":{"cached_tokens":0},"output_tokens":1000}}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_z","status":"completed","model":"gpt-5.3-codex","usage":{"input_tokens":0,"output_tokens":0}}}`,
		``,
	}, "\n")
	ev := feedResponsesLeaf(t, stream).Finalise("alice", "issue-459", "")
	if ev == nil {
		t.Fatal("Finalise returned nil; a zeroed trailer must not erase a real count")
	}
	if ev.InputTok != 2000 || ev.OutputTok != 1000 {
		t.Errorf("got input=%d output=%d, want 2000/1000 (the zeroed trailer must be ignored)", ev.InputTok, ev.OutputTok)
	}
}

// TestOpenAIResponsesStreamParser_NoTerminalUsageEmitsNothing: a stream cut
// before any terminal event must emit nothing rather than guess at counts. The
// caller turns that nil into reason=stream_no_usage, which the end-to-end test
// below pins. Constructed on the LEAF — routed through the router this would
// pass with routing entirely broken, since the chat sub-parser also returns nil.
func TestOpenAIResponsesStreamParser_NoTerminalUsageEmitsNothing(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_cut","model":"gpt-5.3-codex","usage":null}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		``,
	}, "\n")
	p := feedResponsesLeaf(t, stream)
	if !strings.Contains(p.id, "resp_cut") {
		t.Errorf("id = %q, want the created event's id latched — otherwise this stream never reached the leaf and the nil below proves nothing", p.id)
	}
	if ev := p.Finalise("alice", "issue-459", ""); ev != nil {
		t.Errorf("expected nil for a stream with no terminal usage, got %+v", ev)
	}
}

// TestOpenAIResponsesStreamParser_ZeroCountsNeverEmit pins Finalise's zero-count
// arm DIRECTLY, by constructing the state rather than feeding a stream.
//
// That is deliberate, and worth explaining: no stream can currently produce this
// state, because OnEvent's latch refuses a 0/0 usage block and leaves gotUsage
// false. The arm is a SECOND lock, unreachable while the first one stands — so
// no wire-level test can kill a mutant that deletes it, and an unpinned guard is
// one refactor away from being deleted as dead code with nobody noticing. If the
// latch condition is ever relaxed, this is what keeps a 0-token event (which
// would burn the resp_* idempotency key) out of the store.
func TestOpenAIResponsesStreamParser_ZeroCountsNeverEmit(t *testing.T) {
	t.Parallel()
	p := &openAIResponsesStreamParser{id: "resp_z", model: "gpt-5.3-codex", gotUsage: true}
	if ev := p.Finalise("alice", "issue-459", ""); ev != nil {
		t.Errorf("expected nil for latched-but-zero counts, got %+v", ev)
	}
}

// TestOpenAIResponsesStreamParser_ModellessTerminalEmitsNothing: a terminal
// event whose response object names no model must emit nothing. Pricing an
// unnamed model would send it through ComputeCost's self-hosted size-class
// fallback and produce a confident dollar figure derived from a guess — the JSON
// path refuses that (r.Model == "") and so must this one.
func TestOpenAIResponsesStreamParser_ModellessTerminalEmitsNothing(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_nomodel","status":"completed","usage":{"input_tokens":500,"output_tokens":20}}}`,
		``,
	}, "\n")
	p := feedResponsesLeaf(t, stream)
	if !p.gotUsage {
		t.Fatal("the leaf never latched usage — this test would then prove nothing about the model guard")
	}
	if ev := p.Finalise("alice", "issue-459", ""); ev != nil {
		t.Errorf("expected nil for a model-less terminal event, got %+v", ev)
	}
}

// TestOpenAIStreamRouter_MixedShapeStreamKeepsResponsesAndSignals pins the one
// case where the router must CHOOSE. No real upstream emits both shapes in one
// stream, but the type does not enforce that, and silently discarding one of two
// live money totals is exactly what the uncaptured counter exists to prevent —
// so the discard is logged. Pinning the log is the point: without it the drop is
// invisible.
func TestOpenAIStreamRouter_MixedShapeStreamKeepsResponsesAndSignals(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	p := &openAIStreamParser{logger: slog.New(slog.NewTextHandler(&logs, nil))}
	f := &sseFramer{onEvent: p.OnEvent}
	f.Feed([]byte(strings.Join([]string{
		`data: {"id":"chatcmpl-mixed","model":"gpt-4o","usage":{"prompt_tokens":9000,"completion_tokens":900}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_mixed","status":"completed","model":"gpt-5.3-codex","usage":{"input_tokens":10,"output_tokens":5}}}`,
		``,
	}, "\n")))
	f.Flush()

	if !p.chat.gotUsage || !p.responses.gotUsage {
		t.Fatalf("setup failed: both sub-parsers must have latched (chat=%v responses=%v)", p.chat.gotUsage, p.responses.gotUsage)
	}
	ev := p.Finalise("alice", "issue-459", "")
	if ev == nil {
		t.Fatal("Finalise returned nil")
	}
	if want := collector.MessageIdempotencyKey(string(ProviderOpenAI), "resp_mixed"); ev.IdempotencyKey != want {
		t.Errorf("IdempotencyKey = %q, want the Responses one (%q) — Responses wins the tie", ev.IdempotencyKey, want)
	}
	if got := logs.String(); !strings.Contains(got, "BOTH OpenAI usage shapes") || !strings.Contains(got, "9000") {
		t.Errorf("discarding the chat counts must be logged with the numbers dropped; got: %s", got)
	}
}

// TestOpenAIStreamRouter_MixedShapeWarningDoesNotCryWolf is the other half of
// the signal: a `response.*` event that carries NO usage (usage:null on
// response.created) must not make the router claim it discarded anything. A
// warning that fires when nothing was dropped is worse than none — operators
// learn to ignore it, and the one time it matters it reads as noise.
//
// Concretely this pins that the Responses sub-parser latches gotUsage only for a
// usage block with counts. Setting that flag unconditionally still produces the
// right EVENT (the zero-count arm in Finalise backstops it), so the emitted
// event alone cannot detect the regression — only the log can.
func TestOpenAIStreamRouter_MixedShapeWarningDoesNotCryWolf(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	p := &openAIStreamParser{logger: slog.New(slog.NewTextHandler(&logs, nil))}
	f := &sseFramer{onEvent: p.OnEvent}
	f.Feed([]byte(strings.Join([]string{
		`data: {"id":"chatcmpl-solo","model":"gpt-4o","usage":{"prompt_tokens":40,"completion_tokens":15}}`,
		``,
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_novalue","model":"gpt-5.3-codex","usage":null}}`,
		``,
	}, "\n")))
	f.Flush()

	ev := p.Finalise("bob", "issue-7", "")
	if ev == nil {
		t.Fatal("Finalise returned nil; the chat counts are the only real usage here and must survive")
	}
	if ev.InputTok != 40 || ev.OutputTok != 15 {
		t.Errorf("got input=%d output=%d, want 40/15 (the chat event, unharmed)", ev.InputTok, ev.OutputTok)
	}
	if got := logs.String(); strings.Contains(got, "BOTH OpenAI usage shapes") {
		t.Errorf("no counts were discarded, so nothing may be warned about; got: %s", got)
	}
}

// --- End-to-end through the proxy, including the uncaptured counter ---

// TestProxy_ResponsesJSONCapturedAndNotCounted is direction 1 of the counter
// contract: a Responses 2xx that is now parsed produces an event and must NOT
// bump tier_proxy_uncaptured_responses_total. Before this change the same
// exchange produced zero events and one openai|parse increment.
func TestProxy_ResponsesJSONCapturedAndNotCounted(t *testing.T) {
	t.Parallel()
	body := readFixture(t, "openai-responses-completed.json")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	sink := newMemSink()
	rec := &recRecorder{}
	p := New(target, ProviderOpenAI, collector.SourceProxy, sink, nil, rec, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.3-codex","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tier-Developer", "alice")
	req.Header.Set("X-Tier-Issue", "issue-459")
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
		t.Fatalf("expected 1 token event from a Responses 2xx, got %d: %+v", len(sink.events), sink.events)
	}
	ev := sink.events[0]
	if ev.InputTok != fixtureInputTokens-fixtureCachedTokens || ev.CacheRead != fixtureCachedTokens || ev.OutputTok != fixtureOutputTokens {
		t.Errorf("got input=%d cache=%d output=%d, want %d/%d/%d", ev.InputTok, ev.CacheRead, ev.OutputTok,
			fixtureInputTokens-fixtureCachedTokens, fixtureCachedTokens, fixtureOutputTokens)
	}
	if ev.Developer != "alice" || ev.IssueID != "issue-459" {
		t.Errorf("attribution = %q/%q, want alice/issue-459", ev.Developer, ev.IssueID)
	}
	if ev.Source != collector.SourceProxy {
		t.Errorf("Source = %q, want %q", ev.Source, collector.SourceProxy)
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Errorf("uncaptured recorder calls = %v, want none (the response WAS captured)", calls)
	}
	if !bytes.Equal(got, body) {
		t.Error("client body differs from upstream: the proxy must forward the response unchanged")
	}
}

// TestProxy_ResponsesSSECapturedAndNotCounted is the streaming half of direction
// 1 — the transport Codex actually uses.
func TestProxy_ResponsesSSECapturedAndNotCounted(t *testing.T) {
	t.Parallel()
	stream := readFixture(t, "openai-responses-stream.sse")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(stream)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	sink := newMemSink()
	rec := &recRecorder{}
	p := New(target, ProviderOpenAI, collector.SourceProxy, sink, nil, rec, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.3-codex","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tier-Developer", "alice")
	req.Header.Set("X-Tier-Issue", "issue-459")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	// MUST fully drain: the streaming emit fires at body Close.
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()

	if len(sink.events) != 1 {
		t.Fatalf("expected 1 token event from a Responses SSE 2xx, got %d: %+v", len(sink.events), sink.events)
	}
	ev := sink.events[0]
	if ev.InputTok != fixtureInputTokens-fixtureCachedTokens || ev.CacheRead != fixtureCachedTokens || ev.OutputTok != fixtureOutputTokens {
		t.Errorf("got input=%d cache=%d output=%d, want %d/%d/%d", ev.InputTok, ev.CacheRead, ev.OutputTok,
			fixtureInputTokens-fixtureCachedTokens, fixtureCachedTokens, fixtureOutputTokens)
	}
	if ev.CostMicro != wantFixtureCostMicro {
		t.Errorf("CostMicro = %d, want %d", ev.CostMicro, wantFixtureCostMicro)
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Errorf("uncaptured recorder calls = %v, want none (the stream WAS captured)", calls)
	}
	if !bytes.Equal(got, stream) {
		t.Error("client body differs from upstream: the proxy must forward the stream unchanged")
	}
}

// TestProxy_UnparseableOpenAIBodyStillCountsUncaptured is direction 2: adding a
// second shape must not make the counter blind. A 2xx JSON body that is neither
// shape still emits nothing and still counts openai|parse — otherwise a real
// capture gap would go silent, which is the failure mode the counter exists for.
func TestProxy_UnparseableOpenAIBodyStillCountsUncaptured(t *testing.T) {
	t.Parallel()
	// Deliberately Responses-flavoured — it carries the "input_tokens" key that
	// trips openAIShape's fast path — but names no model, so neither parser can
	// price it. This is the body most likely to be silently swallowed by a
	// careless dispatcher.
	const body = `{"id":"resp_nomodel","object":"response","usage":{"input_tokens":10,"output_tokens":5}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	sink := newMemSink()
	rec := &recRecorder{}
	p := New(target, ProviderOpenAI, collector.SourceProxy, sink, nil, rec, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if len(sink.events) != 0 {
		t.Fatalf("expected 0 events for an unpriceable body, got %d: %+v", len(sink.events), sink.events)
	}
	if calls := rec.snapshot(); len(calls) != 1 || calls[0] != "openai|"+reasonParse {
		t.Errorf("recorder calls = %v, want [openai|parse]", calls)
	}
}

// TestProxy_ZeroUsageResponsesJSONCountsUncaptured is the third direction-2 arm
// on the JSON path: a well-formed, fully-parseable Responses 2xx that reports
// 0/0 must produce NO event and still bump openai|parse. Without the parser's
// zero-usage guard this stores a $0 row AND the counter goes silent — capture
// blindness that looks exactly like success.
func TestProxy_ZeroUsageResponsesJSONCountsUncaptured(t *testing.T) {
	t.Parallel()
	const body = `{"id":"resp_zero_e2e","object":"response","status":"completed","model":"gpt-5.3-codex","usage":{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":0}}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	sink := newMemSink()
	rec := &recRecorder{}
	p := New(target, ProviderOpenAI, collector.SourceProxy, sink, nil, rec, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if len(sink.events) != 0 {
		t.Fatalf("expected 0 events for a zero-usage Responses body, got %d: %+v", len(sink.events), sink.events)
	}
	if calls := rec.snapshot(); len(calls) != 1 || calls[0] != "openai|"+reasonParse {
		t.Errorf("recorder calls = %v, want [openai|parse]", calls)
	}
}

// TestProxy_ResponsesSSEWithoutTerminalUsageCountsStreamNoUsage is the streaming
// half of direction 2. Table-driven over the three streams that must all end in
// silence-plus-a-counter rather than a bogus event: no terminal event at all, a
// terminal event reporting 0/0, and a terminal event naming no model.
func TestProxy_ResponsesSSEWithoutTerminalUsageCountsStreamNoUsage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, stream string }{
		{
			name: "no terminal event",
			stream: `event: response.created` + "\n" +
				`data: {"type":"response.created","response":{"id":"resp_cut","model":"gpt-5.3-codex","usage":null}}` + "\n\n",
		},
		{
			name: "terminal event with zero usage",
			stream: `event: response.completed` + "\n" +
				`data: {"type":"response.completed","response":{"id":"resp_zero","status":"completed","model":"gpt-5.3-codex","usage":{"input_tokens":0,"output_tokens":0}}}` + "\n\n",
		},
		{
			name: "terminal event with no model",
			stream: `event: response.completed` + "\n" +
				`data: {"type":"response.completed","response":{"id":"resp_nomodel","status":"completed","usage":{"input_tokens":500,"output_tokens":20}}}` + "\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sink := newMemSink()
			rec := &recRecorder{}
			p := New(mustParseURL(t, "http://example.invalid"), ProviderOpenAI, collector.SourceProxy, sink, nil, rec, nil)
			sc := newStreamCapture(context.Background(), io.NopCloser(strings.NewReader(tc.stream)),
				p.provider, "alice", "issue-459", "", "", p.sink, p.recordWrite, p.recordUncaptured, slog.Default())
			if _, err := io.ReadAll(sc); err != nil {
				t.Fatalf("read: %v", err)
			}
			if err := sc.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			if got := len(sink.events); got != 0 {
				t.Fatalf("expected 0 events, got %d: %+v", got, sink.events)
			}
			if calls := rec.snapshot(); len(calls) != 1 || calls[0] != "openai|"+reasonStreamNoUsage {
				t.Errorf("recorder calls = %v, want [openai|stream_no_usage]", calls)
			}
		})
	}
}
