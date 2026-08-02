package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// restoreDefaultPriceTable reloads the embedded default into the package global
// on cleanup, so a test that swaps the table via LoadPriceTable / parsePriceTable
// doesn't leak its override into other tests in this package.
func restoreDefaultPriceTable(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		tbl, info, err := parsePriceTable(defaultPriceTableYAML)
		if err != nil {
			t.Fatalf("restore default price table: %v", err)
		}
		priceTable = tbl
		activePriceTableInfo = info
	})
}

// TestEmbeddedPriceTableLoads asserts the go:embed default parses at init and
// carries the expected shape — the externalized YAML (#68) is the source of
// truth and must stay loadable.
func TestEmbeddedPriceTableLoads(t *testing.T) {
	info := ActivePriceTableInfo()
	if info.Version < 1 {
		t.Errorf("embedded table version = %d, want >= 1", info.Version)
	}
	if info.EffectiveDate == "" {
		t.Error("embedded table effective_date is empty")
	}
	if info.ModelCount < 20 {
		t.Errorf("embedded table model count = %d, want >= 20 (full RPT)", info.ModelCount)
	}
	// Required fallback keys must be present (parsePriceTable enforces this, but
	// pin it against an accidental prices.yaml edit).
	for _, k := range requiredSelfHostedKeys {
		if _, ok := priceTable[k]; !ok {
			t.Errorf("embedded table missing required key %q", k)
		}
	}
}

// TestEmbeddedOpenWeightsHostRates_Resolve is the #268 end-to-end guard: it drives
// the EMBEDDED table through ComputeCostHost with the raw (host, model) pair the
// proxy would capture — the host it stamps from --openai-target and the exact,
// often mixed-case, model id the upstream echoes — and asserts EVERY seeded row
// resolves to its audited per-token rate with billing_mode per_token. This is the
// real contract, and it must cover ALL rows, not a sample: a seeded key is only
// useful if HostModelKey's normalization (lowercase + date-suffix strip) reproduces
// it from what the proxy actually sees. NoValueDrift (below) pins the rows as
// AUTHORED, but it cannot catch a key that no live request will ever hit — a
// consistent typo in both the YAML and its `want` entry passes NoValueDrift yet
// silently falls to the size-class heuristic in production. So each raw model string
// here is the id the host really echoes (NOT the normalized key), forcing the
// NormalizeModel round-trip. The Fireworks mixtral row (0.50/0.50) is the sharpest
// case: a miss would size-class-match "8x7b"→self-hosted-medium at the SAME $0.50, so
// the rate check alone can't see it — the billing_mode (per_token vs the fallback's
// self_hosted_amortized) and the shared cost-recorder assertion below are what catch it.
func TestEmbeddedOpenWeightsHostRates_Resolve(t *testing.T) {
	silenceUnknownModelLogger(t)
	resetUnknownModelDedupe(t)
	t.Cleanup(func() { SetUnknownModelCostRecorder(nil) })
	installPriceTable(t, string(defaultPriceTableYAML)) // the real shipped table

	// A host-qualified hit is an AUDITED rate, never a guess, so it must NOT bump the
	// unknown-model cost recorder. On a MISS every row would size-class-match its param
	// count and bump it, so rec.calls==0 after the loop is a second, rate-independent
	// proof that the host-qualified branch fired for all rows (covers the mixtral
	// same-cost collision). NB: the proxy strips the port (url.URL.Hostname()) before
	// calling ComputeCostHost — normalizeHost does not — so keys and these cases use
	// bare hosts; port handling is the proxy's contract, exercised in internal/proxy.
	rec := &fakeCostRecorder{}
	SetUnknownModelCostRecorder(rec)

	cases := []struct {
		name            string
		host, model     string // raw, exactly as the proxy captures them (host + echoed id)
		inRate, outRate float64
	}{
		// OpenRouter.
		{"openrouter-llama33", "openrouter.ai", "meta-llama/llama-3.3-70b-instruct", 0.10, 0.32},
		{"openrouter-llama31-8b", "openrouter.ai", "meta-llama/llama-3.1-8b-instruct", 0.02, 0.03},
		{"openrouter-qwen25-72b", "openrouter.ai", "qwen/qwen-2.5-72b-instruct", 0.36, 0.40},
		// DeepSeek's versioned slug: -0324 is stripped by NormalizeModel, so the key drops it.
		{"openrouter-deepseek-v3-0324", "openrouter.ai", "deepseek/deepseek-chat-v3-0324", 0.24, 0.90},
		// Together (mixed-case echo).
		{"together-llama33", "api.together.ai", "meta-llama/Llama-3.3-70B-Instruct-Turbo", 1.04, 1.04},
		// Fireworks (accounts/… slug; symmetric in=out tiers; mixtral collides at 0.50).
		{"fireworks-llama33", "api.fireworks.ai", "accounts/fireworks/models/llama-v3p3-70b-instruct", 0.90, 0.90},
		{"fireworks-llama31-8b", "api.fireworks.ai", "accounts/fireworks/models/llama-v3p1-8b-instruct", 0.20, 0.20},
		{"fireworks-qwen2p5-72b", "api.fireworks.ai", "accounts/fireworks/models/qwen2p5-72b-instruct", 0.90, 0.90},
		{"fireworks-mixtral", "api.fireworks.ai", "accounts/fireworks/models/mixtral-8x7b-instruct", 0.50, 0.50},
		// Groq.
		{"groq-llama33", "api.groq.com", "llama-3.3-70b-versatile", 0.59, 0.79},
		{"groq-llama31-8b", "api.groq.com", "llama-3.1-8b-instant", 0.05, 0.08},
		{"groq-qwen3-32b", "api.groq.com", "qwen/qwen3-32b", 0.29, 0.59},
		// DeepInfra (mixed-case echoes; doubled meta-llama prefix; -v0.1 must survive).
		{"deepinfra-llama33", "api.deepinfra.com", "meta-llama/Llama-3.3-70B-Instruct-Turbo", 0.10, 0.32},
		{"deepinfra-llama31-8b", "api.deepinfra.com", "meta-llama/Meta-Llama-3.1-8B-Instruct", 0.02, 0.05},
		{"deepinfra-qwen25-72b", "api.deepinfra.com", "Qwen/Qwen2.5-72B-Instruct", 0.36, 0.40},
		{"deepinfra-deepseek-v3", "api.deepinfra.com", "deepseek-ai/DeepSeek-V3", 0.32, 0.89},
		{"deepinfra-mixtral", "api.deepinfra.com", "mistralai/Mixtral-8x7B-Instruct-v0.1", 0.54, 0.54},
	}
	// Every seeded prices.yaml host row must have a case here (and vice versa), so this
	// guard cannot silently drift out of full coverage as rows are added or removed.
	if got := countEmbeddedHostQualifiedRows(t); got != len(cases) {
		t.Fatalf("embedded table has %d host-qualified rows but the Resolve suite covers %d — add/remove a case", got, len(cases))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotIn, mode := ComputeCostHost(c.host, c.model, CostUsage{Input: 1_000_000})
			if want := DollarsToMicro(c.inRate); gotIn != want {
				t.Errorf("input cost = %d, want %d ($%.2f/M audited host rate)", gotIn, want, c.inRate)
			}
			if mode != BillingPerToken {
				t.Errorf("billing_mode = %q, want %q (miss → size-class fallback, not an audited host rate)", mode, BillingPerToken)
			}
			gotOut, _ := ComputeCostHost(c.host, c.model, CostUsage{Output: 1_000_000})
			if want := DollarsToMicro(c.outRate); gotOut != want {
				t.Errorf("output cost = %d, want %d ($%.2f/M audited host rate)", gotOut, want, c.outRate)
			}
		})
	}
	if rec.calls != 0 {
		t.Errorf("a host-qualified hit bumped the unknown-model cost recorder %d times, want 0 — a row missed and fell to the size-class guess", rec.calls)
	}
}

// countEmbeddedHostQualifiedRows returns how many rows in the shipped table are
// host-qualified (their normalized key carries the "@" host separator), so the
// Resolve suite can assert it covers every one of them.
func countEmbeddedHostQualifiedRows(t *testing.T) int {
	t.Helper()
	tbl, _, err := parsePriceTable(defaultPriceTableYAML)
	if err != nil {
		t.Fatalf("parse embedded table: %v", err)
	}
	n := 0
	for k := range tbl {
		if strings.Contains(k, hostKeySep) {
			n++
		}
	}
	return n
}

// TestEmbeddedPriceTable_NoValueDrift is the drift guard (#68): it pins the
// EXACT (input, output, combined, provider, long-context over-tier) of every
// entry in the embedded prices.yaml against a maintained expected set (seeded
// from the pre-externalization Go map, extended as models are added — e.g. the
// #4 long-context tiers on gemini-*-pro and claude-sonnet-4-5). TestComputeCost
// pins the #80-risky entries via dollar assertions, but the long tail (gpt-4.1,
// gemini-*, grok-*, deepseek-*, etc.) has no dollar check — a fat-finger in the
// YAML would slip through. This parses the embedded YAML directly (independent
// of any global swap). Update `want` in the same commit as any prices.yaml edit.
func TestEmbeddedPriceTable_NoValueDrift(t *testing.T) {
	tbl, _, err := parsePriceTable(defaultPriceTableYAML)
	if err != nil {
		t.Fatalf("parse embedded table: %v", err)
	}
	// Every entry pins its EFFECTIVE cache multiplier triple (read, write5m,
	// write1h) — the complete per-model pricing contract after #122. Provider
	// defaults: anthropic 0.10/1.25/2.00, openai 0.50/1.0/1.0, google/xai/deepseek
	// 1.0/1.0/1.0. Entries with an explicit cache_read_mult override only the read
	// class: gemini-2.5-* / 3.1-pro 0.25; deepseek-v3 0.1; deepseek-r1 0.0509; and
	// every gpt-5.x entry 0.10 (the 5-era discount, RPT §7, #135). Combined
	// self-hosted entries keep the 1.0 defaults (unused).
	want := map[string]modelPrice{
		"claude-opus-4":   {inputPerM: 15.00, outputPerM: 75.00, provider: providerAnthropic, cacheReadMult: 0.10, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2.00},
		"claude-opus-4-6": {inputPerM: 5.00, outputPerM: 25.00, provider: providerAnthropic, cacheReadMult: 0.10, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2.00},
		"claude-opus-4-7": {inputPerM: 5.00, outputPerM: 25.00, provider: providerAnthropic, cacheReadMult: 0.10, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2.00},
		"claude-opus-4-8": {inputPerM: 5.00, outputPerM: 25.00, provider: providerAnthropic, cacheReadMult: 0.10, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2.00},
		// Opus 5 carries the SAME $5/$25 as Opus 4.8 — verified against the vendor
		// catalog on 2026-07-26, not inferred (Opus 4.1 is $15/$75, so "newer Opus
		// is cheaper" is not a rule). Cache multipliers are the Anthropic family
		// defaults: read 0.10x, 5m write 1.25x, 1h write 2.00x.
		"claude-opus-5":     {inputPerM: 5.00, outputPerM: 25.00, provider: providerAnthropic, cacheReadMult: 0.10, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2.00},
		"claude-fable-5":    {inputPerM: 10.00, outputPerM: 50.00, provider: providerAnthropic, cacheReadMult: 0.10, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2.00},
		"claude-mythos-5":   {inputPerM: 10.00, outputPerM: 50.00, provider: providerAnthropic, cacheReadMult: 0.10, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2.00}, // v7 (#268 refresh)
		"claude-sonnet-4":   {inputPerM: 3.00, outputPerM: 15.00, provider: providerAnthropic, cacheReadMult: 0.10, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2.00},
		"claude-sonnet-4-5": {inputPerM: 3.00, outputPerM: 15.00, provider: providerAnthropic, cacheReadMult: 0.10, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2.00, contextThreshold: 200000, inputPerMOver: 6.00, outputPerMOver: 22.50},
		"claude-sonnet-4-6": {inputPerM: 3.00, outputPerM: 15.00, provider: providerAnthropic, cacheReadMult: 0.10, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2.00},
		"claude-sonnet-4-7": {inputPerM: 3.00, outputPerM: 15.00, provider: providerAnthropic, cacheReadMult: 0.10, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2.00},
		"claude-sonnet-5":   {inputPerM: 3.00, outputPerM: 15.00, provider: providerAnthropic, cacheReadMult: 0.10, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2.00}, // v7 (#268 refresh); list price, intro promo not encoded
		"claude-haiku-3.5":  {inputPerM: 0.80, outputPerM: 4.00, provider: providerAnthropic, cacheReadMult: 0.10, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2.00},
		"claude-haiku-4-5":  {inputPerM: 1.00, outputPerM: 5.00, provider: providerAnthropic, cacheReadMult: 0.10, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2.00},
		"gpt-4o":            {inputPerM: 2.50, outputPerM: 10.00, provider: providerOpenAI, cacheReadMult: 0.50, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-4.1":           {inputPerM: 2.00, outputPerM: 8.00, provider: providerOpenAI, cacheReadMult: 0.50, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-4.1-mini":      {inputPerM: 0.40, outputPerM: 1.60, provider: providerOpenAI, cacheReadMult: 0.50, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-4.1-nano":      {inputPerM: 0.05, outputPerM: 0.20, provider: providerOpenAI, cacheReadMult: 0.50, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-4o-mini":       {inputPerM: 0.15, outputPerM: 0.60, provider: providerOpenAI, cacheReadMult: 0.50, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"o3":                {inputPerM: 2.00, outputPerM: 8.00, provider: providerOpenAI, cacheReadMult: 0.50, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"o3-mini":           {inputPerM: 0.55, outputPerM: 2.20, provider: providerOpenAI, cacheReadMult: 0.50, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gemini-2.5-pro":    {inputPerM: 1.25, outputPerM: 10.00, provider: providerGoogle, cacheReadMult: 0.25, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, contextThreshold: 200000, inputPerMOver: 2.50, outputPerMOver: 15.00},
		"gemini-3.1-pro":    {inputPerM: 2.00, outputPerM: 12.00, provider: providerGoogle, cacheReadMult: 0.25, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, contextThreshold: 200000, inputPerMOver: 4.00, outputPerMOver: 18.00},
		"gemini-2.5-flash":  {inputPerM: 0.30, outputPerM: 2.50, provider: providerGoogle, cacheReadMult: 0.25, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gemini-2.0-flash":  {inputPerM: 0.10, outputPerM: 0.40, provider: providerGoogle, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"grok-3":            {inputPerM: 3.00, outputPerM: 15.00, provider: providerXAI, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"grok-3-mini":       {inputPerM: 0.30, outputPerM: 0.50, provider: providerXAI, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"deepseek-r1":       {inputPerM: 0.55, outputPerM: 2.19, provider: providerDeepSeek, cacheReadMult: 0.0509, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"deepseek-v3":       {inputPerM: 0.28, outputPerM: 0.42, provider: providerDeepSeek, cacheReadMult: 0.1, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		// --- v5 additions (#135): RPT-2026 §1/§4 models. gpt-5.x carry an
		// explicit cache_read_mult 0.10 (effective read 0.10); o1/o4-mini keep the
		// openai 0.50 default; grok-*/gemini-*/deepseek-v3.2 keep the 1.0 default.
		"claude-opus-4-1":       {inputPerM: 15.00, outputPerM: 75.00, provider: providerAnthropic, cacheReadMult: 0.10, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2.00},
		"claude-opus-4-5":       {inputPerM: 5.00, outputPerM: 25.00, provider: providerAnthropic, cacheReadMult: 0.10, cacheWrite5mMult: 1.25, cacheWrite1hMult: 2.00},
		"o1":                    {inputPerM: 15.00, outputPerM: 60.00, provider: providerOpenAI, cacheReadMult: 0.50, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"o4-mini":               {inputPerM: 1.10, outputPerM: 4.40, provider: providerOpenAI, cacheReadMult: 0.50, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-5":                 {inputPerM: 1.25, outputPerM: 10.00, provider: providerOpenAI, cacheReadMult: 0.10, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-5.1":               {inputPerM: 0.625, outputPerM: 5.00, provider: providerOpenAI, cacheReadMult: 0.10, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-5.2":               {inputPerM: 0.875, outputPerM: 7.00, provider: providerOpenAI, cacheReadMult: 0.10, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-5.4":               {inputPerM: 2.50, outputPerM: 15.00, provider: providerOpenAI, cacheReadMult: 0.10, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-5.4-pro":           {inputPerM: 30.00, outputPerM: 180.00, provider: providerOpenAI, cacheReadMult: 0.10, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-5-mini":            {inputPerM: 0.125, outputPerM: 1.00, provider: providerOpenAI, cacheReadMult: 0.10, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-5-nano":            {inputPerM: 0.05, outputPerM: 0.40, provider: providerOpenAI, cacheReadMult: 0.10, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-5.4-mini":          {inputPerM: 0.75, outputPerM: 4.50, provider: providerOpenAI, cacheReadMult: 0.10, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-5.4-nano":          {inputPerM: 0.20, outputPerM: 1.25, provider: providerOpenAI, cacheReadMult: 0.10, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-5.3-codex":         {inputPerM: 1.75, outputPerM: 14.00, provider: providerOpenAI, cacheReadMult: 0.10, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-5.1-codex":         {inputPerM: 1.25, outputPerM: 10.00, provider: providerOpenAI, cacheReadMult: 0.10, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-5.1-codex-mini":    {inputPerM: 0.25, outputPerM: 2.00, provider: providerOpenAI, cacheReadMult: 0.10, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-5.6-sol":           {inputPerM: 5.00, outputPerM: 30.00, provider: providerOpenAI, cacheReadMult: 0.10, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-5.6-terra":         {inputPerM: 2.50, outputPerM: 15.00, provider: providerOpenAI, cacheReadMult: 0.10, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gpt-5.6-luna":          {inputPerM: 1.00, outputPerM: 6.00, provider: providerOpenAI, cacheReadMult: 0.10, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"grok-4":                {inputPerM: 3.00, outputPerM: 15.00, provider: providerXAI, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"grok-3-fast":           {inputPerM: 5.00, outputPerM: 25.00, provider: providerXAI, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"grok-4-fast":           {inputPerM: 0.20, outputPerM: 0.50, provider: providerXAI, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"grok-4.1-fast":         {inputPerM: 0.20, outputPerM: 0.50, provider: providerXAI, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"grok-code-fast-1":      {inputPerM: 0.20, outputPerM: 1.50, provider: providerXAI, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gemini-2.5-flash-lite": {inputPerM: 0.10, outputPerM: 0.40, provider: providerGoogle, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gemini-3-flash":        {inputPerM: 0.50, outputPerM: 3.00, provider: providerGoogle, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"gemini-3.1-flash-lite": {inputPerM: 0.25, outputPerM: 1.50, provider: providerGoogle, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"deepseek-v3.2":         {inputPerM: 0.28, outputPerM: 0.42, provider: providerDeepSeek, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		// --- end v5 additions ---
		"self-hosted-large":  {inputPerM: 2.00, combined: true, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"self-hosted-medium": {inputPerM: 0.50, combined: true, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		"self-hosted-small":  {inputPerM: 0.10, combined: true, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0},
		// --- v6 additions (#268): host-qualified open-weights per-token rates. Each
		// pins billingMode: BillingPerToken EXPLICITLY — provider is self-hosted (1.0x
		// cache) but the mode overrides the self-hosted→amortized default because these
		// hosts meter per token. non-combined (distinct in/out). Keys are the exact
		// per-host echoed model id, NormalizeModel'd, "@" the bare --openai-target host.
		"meta-llama/llama-3.3-70b-instruct@openrouter.ai":                    {inputPerM: 0.10, outputPerM: 0.32, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, billingMode: BillingPerToken},
		"meta-llama/llama-3.1-8b-instruct@openrouter.ai":                     {inputPerM: 0.02, outputPerM: 0.03, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, billingMode: BillingPerToken},
		"qwen/qwen-2.5-72b-instruct@openrouter.ai":                           {inputPerM: 0.36, outputPerM: 0.40, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, billingMode: BillingPerToken},
		"deepseek/deepseek-chat-v3@openrouter.ai":                            {inputPerM: 0.24, outputPerM: 0.90, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, billingMode: BillingPerToken},
		"meta-llama/llama-3.3-70b-instruct-turbo@api.together.ai":            {inputPerM: 1.04, outputPerM: 1.04, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, billingMode: BillingPerToken},
		"accounts/fireworks/models/llama-v3p3-70b-instruct@api.fireworks.ai": {inputPerM: 0.90, outputPerM: 0.90, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, billingMode: BillingPerToken},
		"accounts/fireworks/models/llama-v3p1-8b-instruct@api.fireworks.ai":  {inputPerM: 0.20, outputPerM: 0.20, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, billingMode: BillingPerToken},
		"accounts/fireworks/models/qwen2p5-72b-instruct@api.fireworks.ai":    {inputPerM: 0.90, outputPerM: 0.90, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, billingMode: BillingPerToken},
		"accounts/fireworks/models/mixtral-8x7b-instruct@api.fireworks.ai":   {inputPerM: 0.50, outputPerM: 0.50, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, billingMode: BillingPerToken},
		"llama-3.3-70b-versatile@api.groq.com":                               {inputPerM: 0.59, outputPerM: 0.79, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, billingMode: BillingPerToken},
		"llama-3.1-8b-instant@api.groq.com":                                  {inputPerM: 0.05, outputPerM: 0.08, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, billingMode: BillingPerToken},
		"qwen/qwen3-32b@api.groq.com":                                        {inputPerM: 0.29, outputPerM: 0.59, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, billingMode: BillingPerToken},
		"meta-llama/llama-3.3-70b-instruct-turbo@api.deepinfra.com":          {inputPerM: 0.10, outputPerM: 0.32, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, billingMode: BillingPerToken},
		"meta-llama/meta-llama-3.1-8b-instruct@api.deepinfra.com":            {inputPerM: 0.02, outputPerM: 0.05, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, billingMode: BillingPerToken},
		"qwen/qwen2.5-72b-instruct@api.deepinfra.com":                        {inputPerM: 0.36, outputPerM: 0.40, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, billingMode: BillingPerToken},
		"deepseek-ai/deepseek-v3@api.deepinfra.com":                          {inputPerM: 0.32, outputPerM: 0.89, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, billingMode: BillingPerToken},
		"mistralai/mixtral-8x7b-instruct-v0.1@api.deepinfra.com":             {inputPerM: 0.54, outputPerM: 0.54, provider: providerSelfHosted, cacheReadMult: 1.0, cacheWrite5mMult: 1.0, cacheWrite1hMult: 1.0, billingMode: BillingPerToken},
	}
	if len(tbl) != len(want) {
		t.Errorf("embedded table has %d models, want %d (an entry was added or dropped)", len(tbl), len(want))
	}
	for name, w := range want {
		got, ok := tbl[name]
		if !ok {
			t.Errorf("embedded table missing model %q", name)
			continue
		}
		// billing_mode is DERIVED (#300) for the model-only rows, not authored
		// per-entry: self_hosted_amortized for a self-hosted entry, per_token
		// otherwise. Fill that derived value here so this drift guard stays focused on
		// the priced fields instead of restating the mode on every row. #268's
		// host-qualified rows carry an EXPLICIT mode (a per-token host serving an
		// open-weights model overrides the self-hosted→amortized default to
		// per_token), so any row that pins billingMode in `want` keeps it — only an
		// unset (zero-value) mode is derived here.
		if w.billingMode == "" {
			if w.provider == providerSelfHosted {
				w.billingMode = BillingSelfHostedAmortized
			} else {
				w.billingMode = BillingPerToken
			}
		}
		if got != w {
			t.Errorf("model %q = %+v, want %+v (value drift in the map→YAML port)", name, got, w)
		}
	}
	for name := range tbl {
		if _, ok := want[name]; !ok {
			t.Errorf("embedded table has unexpected model %q (add it to want, with any long-context over-tier fields)", name)
		}
	}
}

const validOverrideYAML = `
version: 7
effective_date: "2030-06-01"
models:
  custom-model: { input_per_m: 1.00, output_per_m: 2.00, provider: openai }
  self-hosted-large: { input_per_m: 2.00, combined: true, provider: self-hosted }
  self-hosted-medium: { input_per_m: 0.50, combined: true, provider: self-hosted }
  self-hosted-small: { input_per_m: 0.10, combined: true, provider: self-hosted }
`

// TestParsePriceTable_Valid covers a well-formed override: it parses, reports the
// right metadata, and its models price correctly.
func TestParsePriceTable_Valid(t *testing.T) {
	tbl, info, err := parsePriceTable([]byte(validOverrideYAML))
	if err != nil {
		t.Fatalf("parsePriceTable: %v", err)
	}
	if info.Version != 7 || info.EffectiveDate != "2030-06-01" || info.ModelCount != 4 {
		t.Errorf("info = %+v, want version=7 date=2030-06-01 models=4", info)
	}
	got := tbl["custom-model"]
	if got.inputPerM != 1.00 || got.outputPerM != 2.00 || got.provider != providerOpenAI {
		t.Errorf("custom-model = %+v, want 1.00/2.00 openai", got)
	}
}

// TestParsePriceTable_Rejects pins every validation gate — each must fail at
// parse time so a malformed or incomplete table can never reach ComputeCost.
func TestParsePriceTable_Rejects(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string // substring of the error
	}{
		{
			name: "version below 1",
			yaml: "version: 0\neffective_date: \"2030-01-01\"\nmodels:\n  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: "version must be >= 1",
		},
		{
			name: "missing effective_date",
			yaml: "version: 1\nmodels:\n  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: "must be YYYY-MM-DD",
		},
		{
			name: "malformed effective_date",
			yaml: "version: 1\neffective_date: \"Jan 2030\"\nmodels:\n  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: "must be YYYY-MM-DD",
		},
		{
			name: "no models",
			yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels: {}\n",
			want: "no models",
		},
		{
			name: "invalid provider",
			yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  m: {input_per_m: 1, output_per_m: 2, provider: bogus}\n  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: "invalid provider",
		},
		{
			name: "negative input rate",
			yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  m: {input_per_m: -1, output_per_m: 2, provider: openai}\n  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: "input_per_m must be > 0",
		},
		{
			name: "zero input rate (silent $0 hazard)",
			yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  m: {input_per_m: 0, output_per_m: 2, provider: openai}\n  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: "input_per_m must be > 0",
		},
		{
			name: "zero output rate on non-combined (silent $0 hazard)",
			yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  m: {input_per_m: 1, output_per_m: 0, provider: openai}\n  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: "output_per_m must be > 0",
		},
		{
			name: "zero rate on self-hosted-medium fallback (free unknown models)",
			yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n  self-hosted-medium: {input_per_m: 0, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: "input_per_m must be > 0",
		},
		{
			name: "missing required self-hosted key",
			yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: `missing required fallback entry "self-hosted-medium"`,
		},
		{
			name: "unknown top-level key (strict)",
			yaml: "version: 1\neffective_date: \"2030-01-01\"\nbogus_key: true\nmodels:\n  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: "decode price table",
		},
		{
			name: "unknown model field (strict)",
			yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  m: {input_per_m: 1, output_per_m: 2, provider: openai, typo_field: 3}\n  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: "decode price table",
		},
		{
			// Duplicate model key must be a decode ERROR, not silent last-wins:
			// a table with the same model listed twice at different rates would
			// otherwise price that model at whichever entry parsed last, a quiet
			// mispricing hazard. yaml.v3 v3.0.1 rejects duplicate mapping keys;
			// this case pins that the #52 replacement parser preserves it.
			name: "duplicate model key (strict, no silent last-wins)",
			yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  m: {input_per_m: 1, output_per_m: 2, provider: openai}\n  m: {input_per_m: 9, output_per_m: 9, provider: openai}\n  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: "decode price table",
		},
		{
			name: "over-tier rate without a threshold (#4)",
			yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  m: {input_per_m: 1, output_per_m: 2, input_per_m_over: 3, output_per_m_over: 4, provider: openai}\n  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: "context_threshold must be > 0",
		},
		{
			name: "threshold without over-tier input rate (#4)",
			yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  m: {input_per_m: 1, output_per_m: 2, context_threshold: 200000, output_per_m_over: 4, provider: openai}\n  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: "input_per_m_over must be > 0",
		},
		{
			name: "threshold without over-tier output rate (#4)",
			yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  m: {input_per_m: 1, output_per_m: 2, context_threshold: 200000, input_per_m_over: 3, provider: openai}\n  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: "output_per_m_over must be > 0",
		},
		{
			name: "over-tier on a combined self-hosted entry (#4)",
			yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  self-hosted-large: {input_per_m: 2, combined: true, context_threshold: 200000, input_per_m_over: 3, output_per_m_over: 4, provider: self-hosted}\n  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: "not supported on a combined entry",
		},
		{
			name: "negative over-tier input rate (#4)",
			yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  m: {input_per_m: 1, output_per_m: 2, context_threshold: 200000, input_per_m_over: -3, output_per_m_over: 4, provider: openai}\n  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: "input_per_m_over must be > 0",
		},
		{
			name: "negative context_threshold (#4)",
			yaml: "version: 1\neffective_date: \"2030-01-01\"\nmodels:\n  m: {input_per_m: 1, output_per_m: 2, context_threshold: -1, input_per_m_over: 3, output_per_m_over: 4, provider: openai}\n  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}\n  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}\n  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}\n",
			want: "context_threshold must be > 0",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := parsePriceTable([]byte(c.yaml))
			if err == nil {
				t.Fatalf("parsePriceTable accepted invalid table, want error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), c.want)
			}
		})
	}
}

// TestLoadPriceTable_OverridesActiveTable proves a --prices file fully REPLACES
// the active table (not merges): the override's model prices, and the default's
// models fall through to the unknown-model fallback.
func TestLoadPriceTable_OverridesActiveTable(t *testing.T) {
	silenceUnknownModelLogger(t)
	resetUnknownModelDedupe(t)
	restoreDefaultPriceTable(t)

	path := filepath.Join(t.TempDir(), "prices.yaml")
	if err := os.WriteFile(path, []byte(validOverrideYAML), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := LoadPriceTable(path)
	if err != nil {
		t.Fatalf("LoadPriceTable: %v", err)
	}
	if info.Version != 7 || ActivePriceTableInfo().Version != 7 {
		t.Errorf("active version = %d / %d, want 7", info.Version, ActivePriceTableInfo().Version)
	}
	// Override's custom-model prices at $1/M input.
	if got := ComputeCost("custom-model", CostUsage{Input: 1_000_000}); got != 1_000_000 {
		t.Errorf("ComputeCost(custom-model, 1M in) = %d micro, want 1_000_000 ($1.00)", got)
	}
	// A default-table model is gone → unknown-model fallback ($0.50/M combined).
	if got := ComputeCost("claude-sonnet-4", CostUsage{Input: 1_000_000}); got != 500_000 {
		t.Errorf("ComputeCost(claude-sonnet-4 after override) = %d micro, want 500_000 (fallback; override replaces, not merges)", got)
	}
}

// TestLoadPriceTable_BadFileLeavesTableUntouched: a parse error must NOT swap the
// active table (no silent fallback / no partial load).
func TestLoadPriceTable_BadFileLeavesTableUntouched(t *testing.T) {
	restoreDefaultPriceTable(t)
	before := ActivePriceTableInfo()

	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("version: 0\nmodels: {}\n"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadPriceTable(path); err == nil {
		t.Fatal("LoadPriceTable accepted an invalid file, want error")
	}
	if ActivePriceTableInfo() != before {
		t.Errorf("active table changed after a failed load: %+v -> %+v", before, ActivePriceTableInfo())
	}
	// Missing file also errors (and doesn't swap).
	if _, err := LoadPriceTable(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("LoadPriceTable on a missing file returned nil error")
	}
}

// countingRecorder is a test UnknownModelRecorder. It counts increments and, for
// #326, captures the label values passed on each Inc so a test can assert the
// guess_path label carries the correct bounded value per branch.
type countingRecorder struct {
	n      int
	labels [][]string
}

func (c *countingRecorder) Inc(labelValues ...string) {
	c.n++
	c.labels = append(c.labels, append([]string(nil), labelValues...))
}

// TestUnknownModelRecorder_CountsFallbacks: the counter fires on EVERY
// unknown-model fallback event (not deduped like the WARN), and a nil recorder
// is a safe no-op.
func TestUnknownModelRecorder_CountsFallbacks(t *testing.T) {
	silenceUnknownModelLogger(t)
	resetUnknownModelDedupe(t)
	t.Cleanup(func() { SetUnknownModelRecorder(nil) })

	rec := &countingRecorder{}
	SetUnknownModelRecorder(rec)

	// Two events for the same unknown model: the WARN dedupes to one, but the
	// counter must count both (spend volume, not distinct models).
	ComputeCost("totally-unknown-model", CostUsage{Input: 1_000_000})
	ComputeCost("totally-unknown-model", CostUsage{Input: 500_000})
	if rec.n != 2 {
		t.Errorf("recorder count = %d, want 2 (every fallback event counts)", rec.n)
	}

	// A KNOWN model must not touch the counter.
	ComputeCost("claude-sonnet-4", CostUsage{Input: 1_000_000})
	if rec.n != 2 {
		t.Errorf("recorder count = %d after a known model, want still 2", rec.n)
	}

	// Clearing the recorder makes subsequent fallbacks a no-op (no panic).
	SetUnknownModelRecorder(nil)
	ComputeCost("another-unknown-model", CostUsage{Input: 1_000_000})
	if rec.n != 2 {
		t.Errorf("recorder count = %d after clear, want still 2", rec.n)
	}
}

// TestUnknownModelRecorder_GuessPathLabel pins #326: each guessed-price event
// tags the counter with a guess_path label naming WHICH branch guessed —
// GuessPathSizeClass for a size-class heuristic hit, GuessPathFlat for the flat
// self-hosted-medium fallback. Critically, the label value is drawn from a small
// FIXED set and never carries the (unbounded, upstream-controlled) model string,
// so the counter's cardinality stays bounded.
func TestUnknownModelRecorder_GuessPathLabel(t *testing.T) {
	silenceUnknownModelLogger(t)
	resetUnknownModelDedupe(t)
	t.Cleanup(func() { SetUnknownModelRecorder(nil) })

	rec := &countingRecorder{}
	SetUnknownModelRecorder(rec)

	// Each guessed model routes down exactly one branch and must record that
	// branch's bounded token — never a fragment of the (upstream-controlled) model
	// string. The 7b case is the discriminator the label exists for: it prices at
	// self-hosted-medium (identical rate to the flat fallback) yet is size_class,
	// not flat. The "@host" case is upstream-controlled (the #300 forgery guard
	// routes it to the guess path) and must still yield a bounded label, not a leak.
	cases := []struct {
		model    string
		wantPath string
	}{
		{"llama-3.1-70b", GuessPathSizeClass},               // "70b" -> self-hosted-large
		{"mystery-7b", GuessPathSizeClass},                  // paramCountRE -> self-hosted-medium, but still size_class
		{"llama-3.1-70b@openrouter.ai", GuessPathSizeClass}, // "@" forgery guard -> guess path; "70b" -> size_class
		{"acme-mystery-z", GuessPathFlat},                   // nothing matches any class detector
	}
	for _, c := range cases {
		ComputeCost(c.model, CostUsage{Input: 1_000_000})
	}

	if rec.n != len(cases) {
		t.Fatalf("recorder count = %d, want %d (one per guessed event)", rec.n, len(cases))
	}
	for i, c := range cases {
		lv := rec.labels[i]
		// Arity + exact value per branch.
		if len(lv) != 1 {
			t.Fatalf("model %q recorded %d label values, want exactly 1 (guess_path)", c.model, len(lv))
		}
		if lv[0] != c.wantPath {
			t.Errorf("model %q guess_path = %q, want %q", c.model, lv[0], c.wantPath)
		}
		// Bounded cardinality: the value is one of exactly two fixed tokens and
		// never carries a fragment of the model string.
		if lv[0] != GuessPathSizeClass && lv[0] != GuessPathFlat {
			t.Errorf("model %q guess_path = %q, outside the bounded set {%q,%q}", c.model, lv[0], GuessPathSizeClass, GuessPathFlat)
		}
		if strings.ContainsAny(lv[0], "@") || strings.Contains(lv[0], "llama") || strings.Contains(lv[0], "mystery") || strings.Contains(lv[0], "acme") {
			t.Errorf("model %q guess_path %q leaked the model string (cardinality explosion)", c.model, lv[0])
		}
	}

	// A KNOWN (exact) model must not record any guess_path event.
	ComputeCost("claude-sonnet-4", CostUsage{Input: 1_000_000})
	if rec.n != len(cases) {
		t.Errorf("recorder count = %d after an exact-hit model, want still %d", rec.n, len(cases))
	}
}
