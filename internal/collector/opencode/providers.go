package opencode

import (
	"sort"
	"strings"
)

// providerDecision is the capture policy for one Opencode `providerID`.
type providerDecision int

const (
	// providerExcludedUnknown is the default for a providerID nobody has
	// classified. It is the FAIL-CLOSED arm: capture nothing, and say so loudly
	// once per provider. It fails closed because the alternative — capture it and
	// let ComputeCostHost guess — silently injects unaudited dollars into the
	// denominator of a cost-per-outcome metric.
	providerExcludedUnknown providerDecision = iota
	// providerPriced means this route has an audited per-token rate reachable
	// from the price table, so its rows are captured and priced.
	providerPriced
	// providerExcludedNoRate means the provider is KNOWN and deliberately not
	// captured, because no per-token rate for it can be substantiated.
	providerExcludedNoRate
)

// providerPolicy is the allowlist. Every entry is a decision someone made with a
// reason written down, and the default (a provider absent from this map) is
// providerExcludedUnknown.
//
// ⛔ DO NOT ADD A PROVIDER HERE WITHOUT A PRICE-TABLE ROW FOR IT. Adding a
// provider to the priced arm without a matching `<model>@<providerID>` row means
// its rows reach ComputeCostHost, miss, and are priced at the guessed
// self-hosted-medium fallback ($0.50/M) — which is not a small error in either
// direction and is exactly why ollama-cloud is excluded below.
//
// Third-party routes serving claude-*/gpt-* IDs (OpenRouter, Copilot, Bedrock)
// still need audited host-specific price rows before admission. For new poll
// days, store.CapturedTokensByDayModel filters by serving host and per-token
// billing, so known third-party usage cannot offset an Anthropic/OpenAI invoice.
var providerPolicy = map[string]struct {
	decision providerDecision
	// why is rendered into the startup INFO / the one-time WARN, so an operator
	// reads the REASON, not just a name.
	why string
}{
	// The Z.ai coding plan. The built-in table prices glm-5.3 per token at Z.ai
	// list price (#786) through its model-only row; an operator row for any other
	// model is keyed `<model>@zai-coding-plan` — i.e. keyed on THIS STRING. The
	// host TokenEvent carries for these rows is the providerID verbatim, not a
	// hostname, because Opencode records no hostname and a hostname could not
	// tell the metered API apart from the coding plan.
	"zai-coding-plan": {providerPriced, "audited Z.ai published per-token rates; glm-5.3/glm-5.3-flash price at the embedded model-only rows (#786), any other model needs a <model>@zai-coding-plan override row (#712)"},

	// 🔴 NAMED DELIBERATELY, NOT LEFT TO THE DEFAULT. A bare `zai` provider exists
	// on exactly one row carrying ZERO tokens — a failed configuration attempt,
	// confirmed first-hand on the maintainer's data 2026-08-28. It would fall
	// through to providerExcludedUnknown and behave identically, so this entry
	// buys no behaviour; it buys the DIAGNOSIS. Without it, the next operator to
	// see "unknown Opencode provider: zai" would reasonably read it as a route
	// this collector forgot to support and add it — which would price GLM rows at
	// the $0.50/M guessed fallback, since no `glm-5.3@zai` row exists or should.
	"zai": {providerExcludedNoRate, "not the coding plan and not the metered API — a failed config attempt (1 row, 0 tokens); no <model>@zai rate exists or should"},

	// 🔴 THE EXPLICIT ollama-cloud DECISION (#719). Ollama's cloud tier is a flat
	// subscription that publishes NO per-token rate for any of the models it
	// serves, so there is nothing to price these rows AGAINST. Capturing them
	// anyway would route ~342.3M tokens (measured 2026-08-28 across
	// kimi-k2.7-code, glm-5.2, kimi-k2.6, gpt-oss:120b, gpt-oss:20b, qwen3.5:397b
	// and kimi-k3) through the guessed self-hosted-medium fallback at $0.50/M and
	// inject ~$171 of invented spend into the TIER denominator.
	//
	// SILENCE IS NOT AN OPTION and the code enforces that: the exclusion is named
	// in an INFO at startup and its per-provider row COUNT is reported after every
	// scan, so an operator can see the size of what is being left out rather than
	// discovering a hole in the numbers later. If Ollama publishes per-token
	// rates, the fix is a price-table row plus flipping this entry — in that
	// order, never the reverse.
	"ollama-cloud": {providerExcludedNoRate, "Ollama's cloud tier publishes no per-token rate; capturing it would price ~342M tokens at the guessed $0.50/M fallback (~$171 of invented spend)"},
}

// classifyProvider returns the capture decision for a providerID.
func classifyProvider(providerID string) (providerDecision, string) {
	if p, ok := providerPolicy[providerID]; ok {
		return p.decision, p.why
	}
	return providerExcludedUnknown, "not in this collector's provider allowlist; no audited rate can be assumed for it"
}

// pricedProviders returns the providerIDs this collector captures, sorted, for
// the startup INFO. Sorted so the log line is stable across runs (Go map order is
// randomized) — an operator diffing two startups should see a diff only when the
// policy actually changed.
func pricedProviders() []string { return providersWithDecision(providerPriced) }

// namedExclusions returns the providerIDs deliberately excluded BY NAME (as
// opposed to by falling through the default), sorted.
func namedExclusions() []string { return providersWithDecision(providerExcludedNoRate) }

func providersWithDecision(d providerDecision) []string {
	out := make([]string, 0, len(providerPolicy))
	for id, p := range providerPolicy {
		if p.decision == d {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// exclusionReasons renders `<providerID>=<why>` for every provider excluded BY
// NAME, in the same sorted order namedExclusions uses.
//
// It exists so the startup INFO's reasons are DERIVED from the policy rather than
// hardcoded by key: a third named exclusion would otherwise be announced with no
// reason attached, silently defeating the disclosure contract that INFO exists to
// keep, and a mistyped key would render as an empty string rather than failing.
func exclusionReasons() string {
	ids := namedExclusions()
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id+"="+providerPolicy[id].why)
	}
	return strings.Join(parts, " | ")
}
