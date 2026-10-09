package store

import (
	"strings"
	"testing"
)

// TestComputeCostHost_DefaultBillingModes pins the billing_mode a model resolves
// to when the price table carries NO host-qualified entry (the state before #268
// seeds real per-host rates): a real metered model is per_token, a self-hosted
// reference rate is self_hosted_amortized, and the cost is byte-identical to the
// host-agnostic ComputeCost.
func TestComputeCostHost_DefaultBillingModes(t *testing.T) {
	silenceUnknownModelLogger(t)
	cost, mode := ComputeCostHost("api.openai.com", "gpt-4o", CostUsage{Input: 1_000_000})
	if mode != BillingPerToken {
		t.Errorf("gpt-4o billing_mode = %q, want %q", mode, BillingPerToken)
	}
	if want := ComputeCost("gpt-4o", CostUsage{Input: 1_000_000}); cost != want {
		t.Errorf("ComputeCostHost cost = %d, want %d (== host-agnostic ComputeCost)", cost, want)
	}
	// A self-hosted heuristic match (…70b…) is an APPROXIMATE amortized estimate.
	if _, m := ComputeCostHost("some.internal.host", "llama-3.1-70b", CostUsage{Input: 1_000_000}); m != BillingSelfHostedAmortized {
		t.Errorf("llama-3.1-70b billing_mode = %q, want %q", m, BillingSelfHostedAmortized)
	}
	// The flat unknown-model fallback is likewise amortized.
	if _, m := ComputeCostHost("some.internal.host", "totally-unknown-model", CostUsage{Input: 1_000_000}); m != BillingSelfHostedAmortized {
		t.Errorf("unknown-model billing_mode = %q, want %q", m, BillingSelfHostedAmortized)
	}
}

// TestComputeCostHost_HostQualifiedKeyWins is the #300 core: when a host-qualified
// (host, model) entry exists, it outranks the model-only rate — but ONLY for that
// host. A different host, or an unknown host, falls back to the model-only rate,
// preserving current behavior for every host #268 has not yet seeded.
func TestComputeCostHost_HostQualifiedKeyWins(t *testing.T) {
	silenceUnknownModelLogger(t)
	installPriceTable(t, `
version: 1
effective_date: 2026-07-13
models:
  self-hosted-large: { input_per_m: 2.00, combined: true, provider: self-hosted }
  self-hosted-medium: { input_per_m: 0.50, combined: true, provider: self-hosted }
  self-hosted-small: { input_per_m: 0.10, combined: true, provider: self-hosted }
  synthmodel: { input_per_m: 10.00, output_per_m: 20.00, provider: openai }
  "synthmodel@openrouter.ai": { input_per_m: 1.00, output_per_m: 2.00, provider: openai }
`)
	one := CostUsage{Input: 1_000_000}
	if got, _ := ComputeCostHost("openrouter.ai", "synthmodel", one); got != DollarsToMicro(1.00) {
		t.Errorf("host-qualified cost = %d, want %d ($1/M host rate)", got, DollarsToMicro(1.00))
	}
	if got, _ := ComputeCostHost("together.ai", "synthmodel", one); got != DollarsToMicro(10.00) {
		t.Errorf("other-host cost = %d, want %d (model-only $10/M)", got, DollarsToMicro(10.00))
	}
	if got, _ := ComputeCostHost("", "synthmodel", one); got != DollarsToMicro(10.00) {
		t.Errorf("unknown-host cost = %d, want %d (model-only fallback)", got, DollarsToMicro(10.00))
	}
	// Host match is case-insensitive (a URL host may arrive mixed-case).
	if got, _ := ComputeCostHost("OpenRouter.AI", "synthmodel", one); got != DollarsToMicro(1.00) {
		t.Errorf("mixed-case host cost = %d, want %d (host rate)", got, DollarsToMicro(1.00))
	}
}

// TestComputeCostHost_UpstreamModelCannotForgeHostRate is the security regression
// (#300 review): `model` is upstream-controlled, so a hostile response naming a
// model that embeds the host separator ("llama@openrouter.ai") must NOT hit the
// host-qualified entry via the model-only exact lookup — it routes to the guessed
// self-hosted path instead, priced at that rate and never at the (cheap) host rate.
func TestComputeCostHost_UpstreamModelCannotForgeHostRate(t *testing.T) {
	silenceUnknownModelLogger(t)
	installPriceTable(t, `
version: 1
effective_date: 2026-07-13
models:
  self-hosted-large: { input_per_m: 2.00, combined: true, provider: self-hosted }
  self-hosted-medium: { input_per_m: 0.50, combined: true, provider: self-hosted }
  self-hosted-small: { input_per_m: 0.10, combined: true, provider: self-hosted }
  "llama-3.1-70b@openrouter.ai": { input_per_m: 0.01, combined: true, provider: self-hosted, billing_mode: subscription }
`)
	// A hostile upstream body claims the host-qualified key AS its model, from a
	// different (or unknown) serving host. It must be priced at the guessed
	// self-hosted rate, not the $0.01/M host row.
	got, mode := ComputeCostHost("evil.com", "llama-3.1-70b@openrouter.ai", CostUsage{Input: 1_000_000})
	if got == DollarsToMicro(0.01) {
		t.Errorf("forged model hit the host-qualified rate ($0.01/M) — host guard bypassed")
	}
	// selfHostedClass sees "70b" -> self-hosted-large ($2/M combined), amortized.
	if want := DollarsToMicro(2.00); got != want {
		t.Errorf("forged model cost = %d, want %d (self-hosted-large heuristic)", got, want)
	}
	if mode != BillingSelfHostedAmortized {
		t.Errorf("forged model billing_mode = %q, want %q (guessed, not the forged subscription)", mode, BillingSelfHostedAmortized)
	}
}

// TestComputeCostHost_HostQualifiedHitIsSilent pins that an audited per-host rate
// is neither a guess nor a silent-estimate hazard: it must NOT bump the
// unknown-model cost recorder. The model here has no model-only entry and no param
// count, so a MISS would fall to the flat fallback and bump the recorder — making
// this a discriminating check that the host-qualified branch actually fired.
func TestComputeCostHost_HostQualifiedHitIsSilent(t *testing.T) {
	silenceUnknownModelLogger(t)
	resetUnknownModelDedupe(t)
	t.Cleanup(func() { SetUnknownModelCostRecorder(nil) })
	installPriceTable(t, `
version: 1
effective_date: 2026-07-13
models:
  self-hosted-large: { input_per_m: 2.00, combined: true, provider: self-hosted }
  self-hosted-medium: { input_per_m: 0.50, combined: true, provider: self-hosted }
  self-hosted-small: { input_per_m: 0.10, combined: true, provider: self-hosted }
  "synthmodel@openrouter.ai": { input_per_m: 1.00, output_per_m: 2.00, provider: openai }
`)
	rec := &fakeCostRecorder{}
	SetUnknownModelCostRecorder(rec)
	ComputeCostHost("openrouter.ai", "synthmodel", CostUsage{Input: 1_000_000})
	if rec.calls != 0 {
		t.Errorf("host-qualified hit bumped the unknown-model cost recorder %d times, want 0 (an audited host rate is not a guess)", rec.calls)
	}
}

// TestComputeCostHost_SubscriptionBillingMode confirms an explicit billing_mode in
// the table (the shape #113/#268 will seed for flat $/mo hosts like Ollama Cloud /
// GLM) is honored and surfaced — never silently rewritten to per_token.
func TestComputeCostHost_SubscriptionBillingMode(t *testing.T) {
	silenceUnknownModelLogger(t)
	installPriceTable(t, `
version: 1
effective_date: 2026-07-13
models:
  self-hosted-large: { input_per_m: 2.00, combined: true, provider: self-hosted }
  self-hosted-medium: { input_per_m: 0.50, combined: true, provider: self-hosted }
  self-hosted-small: { input_per_m: 0.10, combined: true, provider: self-hosted }
  "glm-4.6@ollama.cloud": { input_per_m: 0.50, combined: true, provider: self-hosted, billing_mode: subscription }
`)
	if _, mode := ComputeCostHost("ollama.cloud", "glm-4.6", CostUsage{Input: 1_000}); mode != BillingSubscription {
		t.Errorf("glm-4.6@ollama.cloud billing_mode = %q, want %q", mode, BillingSubscription)
	}
}

// TestComputeCost_BackwardCompatDelegatesToHostUnknown pins that the legacy
// host-agnostic ComputeCost is exactly ComputeCostHost with an unknown host — the
// contract the off-limits callers (collector, api/events) rely on staying stable.
func TestComputeCost_BackwardCompatDelegatesToHostUnknown(t *testing.T) {
	silenceUnknownModelLogger(t)
	u := CostUsage{Input: 500_000, Output: 250_000, CacheRead: 10_000}
	legacy := ComputeCost("claude-sonnet-4", u)
	hosted, mode := ComputeCostHost("", "claude-sonnet-4", u)
	if legacy != hosted {
		t.Errorf("ComputeCost=%d != ComputeCostHost(unknown host)=%d", legacy, hosted)
	}
	if mode != BillingPerToken {
		t.Errorf("first-party per-token model billing_mode = %q, want %q", mode, BillingPerToken)
	}
}

// TestParsePriceTable_RejectsInvalidBillingMode fails loud on an unrecognized
// billing_mode rather than silently defaulting — the same fail-loud discipline the
// provider allowlist already enforces.
func TestParsePriceTable_RejectsInvalidBillingMode(t *testing.T) {
	yamlDoc := `
version: 1
effective_date: 2026-07-13
models:
  self-hosted-large: { input_per_m: 2.00, combined: true, provider: self-hosted }
  self-hosted-medium: { input_per_m: 0.50, combined: true, provider: self-hosted }
  self-hosted-small: { input_per_m: 0.10, combined: true, provider: self-hosted }
  bad: { input_per_m: 1.00, output_per_m: 2.00, provider: openai, billing_mode: monthly }
`
	_, _, err := parsePriceTable([]byte(yamlDoc))
	if err == nil {
		t.Fatal("parsePriceTable accepted an invalid billing_mode, want error")
	}
	if !strings.Contains(err.Error(), "billing_mode") {
		t.Errorf("error = %v, want mention of billing_mode", err)
	}
}

// TestHostModelKey pins the host-qualified key convention #268 authors YAML keys
// against: NormalizeModel(model) + "@" + lowercased host.
func TestHostModelKey(t *testing.T) {
	if got := HostModelKey("OpenRouter.AI", "Llama-3.1-70B"); got != "llama-3.1-70b@openrouter.ai" {
		t.Errorf("HostModelKey = %q, want %q", got, "llama-3.1-70b@openrouter.ai")
	}
}
