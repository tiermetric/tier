# TIER Cost Normalization Specification
## Version 1.0 — issued March 26, 2026; last revised 2026-07-13

**Status:** Published specification. It describes the **target** design; `tierd` implements a subset, itemized in the implementation-status banner below.

**Scope:** Defines how the TIER denominator converts raw token consumption into cost-normalized dollars, ensuring comparability across API providers, self-hosted infrastructure, hybrid environments, and time periods.

> ⚠️ **Implementation status (as of 2026-07-13, `main`).** This document specifies
> the **full, target** cost-normalization design. `tierd` implements a **subset**,
> and two areas below describe behavior that **no code enforces today**. Ground
> truth is `internal/store/prices.go` (`ComputeCost`) + `internal/store/prices.yaml`
> (`version: 9`, `effective_date: 2026-07-26`).
>
> **Unknown-model fallback — shipped reality (NOT the tiered design in §2.2 / §2.3 / §7.1).**
> There is **no** `frontier-default` / `standard-default` / `efficient-default`
> tier, **no** `standard-default 3.00/12.00` "ultimate fallback", **no**
> `infer_tier_from_model_name`, and **no** `price_confidence` column. When a
> normalized model string has **no exact `prices.yaml` entry**, `ComputeCost`
> prices it at a **self-hosted reference rate**:
> - **Size-class heuristic** — if the string carries a parameter count,
>   `selfHostedClass` maps it to `self-hosted-large` **$2.00/M**, `self-hosted-medium`
>   **$0.50/M**, or `self-hosted-small` **$0.10/M** (e.g. `...70b...` matches large).
> - **Flat fallback** — nothing matched -> the single `self-hosted-medium`
>   **$0.50/M** combined rate.
>
> Both paths emit a **one-time-per-model `WARN`** and bump the unknown-model
> **event + cost counters** (`tier_unknown_model_events_total` /
> `tier_unknown_model_cost_micro_total`, #267/#135), so mispriced spend is
> **observable** rather than silent. Only an **exact** table hit is silent.
>
> **Governance / RPT publication is aspirational.** No official RPT has ever been
> published, and no release has been cut. The "published annually, pinned
> to January 1 / community review Dec 15-31" language in §2.2, §2.4, and §8 is
> **target governance, not current fact**. The shipped table is the embedded
> `internal/store/prices.yaml` (v7, effective **2026-07-13**).
>
> **RPT-2026 table errata.** Several rows in §2.3 below carry **known-wrong**
> prices (retired / never-launched models, swapped in/out rates). They are **kept
> for provenance** and each is annotated with a pointer to the corrected
> authority: `docs/reference-price-table.md` **§4**. Do not price against the
> uncorrected rows.

---

## 1. Governing Principle

The TIER denominator must answer one question: **"How many dollars of AI inference did it cost to produce this outcome?"**

Raw token counts fail this test because 1M tokens of Claude Opus ($75 output) is not equivalent to 1M tokens of GPT-4o Mini ($0.60 output). A developer who uses an expensive model judiciously should not be penalized relative to one who burns cheap tokens wastefully. Cost normalization makes the denominator unit-consistent: dollars spent, regardless of how those dollars were spent.

### The Formula

```
TIER = Sigma(outcome_weight * quality_multiplier) / (total_effective_cost / $1,000)
```

- **Numerator:** Weighted, quality-adjusted outcomes (unchanged from prior specification)
- **Denominator:** Total effective cost in dollars, divided by $1,000 for readability
- **Unit:** Outcome points per $1,000 of AI inference spend

---

## 2. The Reference Price Table (RPT)

### 2.1 Why Reference Prices, Not Actual Prices

TIER uses a **published Reference Price Table**, not the actual billed amount. This is a deliberate design choice for three reasons:

**Reason 1: Enterprise discount distortion.** Company A negotiates a 40% discount on Claude API. Company B pays list price. If TIER uses actual billed amounts, Company A's developers appear 67% more efficient than Company B's developers for identical work. This is not a productivity signal -- it is a procurement signal.

**Reason 2: Internal transfer pricing distortion.** Self-hosted inference has no invoice. The "cost" depends on how the organization amortizes CapEx, allocates shared clusters, and accounts for electricity. These are accounting decisions, not engineering productivity signals.

**Reason 3: Temporal comparability.** If TIER uses actual prices and prices drop 50% year-over-year, a developer's TIER score doubles without any change in behavior. The metric must isolate the developer's efficiency from the market's price dynamics.

### 2.2 The Reference Price Table Structure

> **Target governance, not current fact.** The annual-publication / January-1
> pinning described here is the intended governance model. No RPT has been
> published to date; the shipped reference table is the embedded
> `internal/store/prices.yaml` (v7, `effective_date: 2026-07-13`). See the
> implementation-status banner at the top of this document.

The TIER project intends to publish an official RPT annually, pinned to January 1 of each year. The RPT for a given year is called **RPT-YYYY** (e.g., RPT-2026).

Each RPT entry contains:

| Field | Description |
|-------|-------------|
| `model_family` | Canonical family name (e.g., `claude-opus-4`, `gpt-4o`) |
| `tier` | Capability tier: `frontier`, `standard`, `efficient`, `embedding`, `image-gen` |
| `input_per_million` | Reference price per 1M input tokens, in USD |
| `output_per_million` | Reference price per 1M output tokens, in USD |
| `effective_date` | Date this entry takes effect (always January 1) |
| `source` | Where the price was derived from (provider list price page URL + date accessed) |

**Model matching rules:**
- Token events carry the model string from the API response (e.g., `claude-opus-4-20250514`).
- The cost engine strips date suffixes to match against `model_family`.
- If no match is found, the event is priced at the `standard` tier default for that provider.
- If no provider match is found, the event is priced at the cross-provider `standard` tier median.
- All fallback matches are flagged with `price_confidence: "estimated"` in the database.

> ⚠️ **The three fallback bullets above are TARGET design — not shipped.** There
> is no per-provider `standard` tier default, no cross-provider median, and no
> `price_confidence` column. The shipped fallback (`internal/store/prices.go`
> `ComputeCost`) prices an unmatched model at a **self-hosted reference rate** —
> a size-class heuristic (`self-hosted-large` $2.00/M, `self-hosted-medium`
> $0.50/M, `self-hosted-small` $0.10/M) when the string carries a parameter
> count, else the flat `self-hosted-medium` $0.50/M — with a one-time `WARN` and
> the unknown-model event/cost counters (#267/#135). See the implementation-status
> banner at the top of this document. `NormalizeModel` strips date suffixes only;
> it does **not** strip a single trailing minor-version digit, so each minor
> version needs its own `prices.yaml` row.

### 2.3 RPT-2026: Reference Price Table (Effective January 1, 2026)

These prices are derived from publicly listed API prices as of March 2026, representing the price landscape at the start of the TIER measurement year.

#### Frontier Tier (Most capable models, highest cost)

| Model Family | Input $/M | Output $/M | Notes |
|-------------|-----------|------------|-------|
| `claude-opus-4` | 15.00 | 75.00 | Anthropic flagship. Extended thinking billable separately (see Section 2.5). |
| `gpt-4.5` | 75.00 | 150.00 | ⚠️ **ERRATA (see reference-price-table.md §4): RETIRED.** Removed from the shipped table; OpenAI replaced it with the GPT-5.x series. Not in `prices.yaml` v7. |
| `gemini-2.0-ultra` | 10.00 | 30.00 | ⚠️ **ERRATA (see reference-price-table.md §4): NEVER LAUNCHED at this price.** Google's current flagship is `gemini-3.1-pro` at $2.00/$12.00. Not in `prices.yaml` v7. |
| `deepseek-r1` | 2.19 | 8.19 | ⚠️ **ERRATA (see reference-price-table.md §4): input/output SWAPPED.** Correct is **$0.55 in / $2.19 out** ($2.19 is the output rate); the $8.19 output shown here is wrong. `prices.yaml` v7: 0.55 / 2.19. |

#### Standard Tier (Primary workhorse models)

| Model Family | Input $/M | Output $/M | Notes |
|-------------|-----------|------------|-------|
| `claude-sonnet-4` | 3.00 | 15.00 | Anthropic primary coding model. |
| `gpt-4o` | 2.50 | 10.00 | OpenAI primary model. |
| `gpt-4.1` | 2.00 | 8.00 | OpenAI newer standard model. |
| `gemini-2.5-pro` | 1.25 | 10.00 | Google primary model. Thinking tokens: $3.50 in / $10 out. |
| `gemini-2.5-flash` | 0.15 | 0.60 | ⚠️ **ERRATA (see reference-price-table.md §4).** Correct is **$0.30 in / $2.50 out** (verified from Google's pricing page). `prices.yaml` v7: 0.30 / 2.50. |
| `deepseek-v3` | 0.27 | 1.10 | ⚠️ **ERRATA (see reference-price-table.md §4).** Correct is **$0.28 in / $0.42 out** (DeepSeek V3.2 updated pricing). `prices.yaml` v7: 0.28 / 0.42. |
| `claude-sonnet-3.5` | 3.00 | 15.00 | Prior-gen Anthropic, still widely used. |

#### Efficient Tier (Cost-optimized, smaller models)

| Model Family | Input $/M | Output $/M | Notes |
|-------------|-----------|------------|-------|
| `claude-haiku-3.5` | 0.80 | 4.00 | Anthropic cost-optimized. |
| `gpt-4o-mini` | 0.15 | 0.60 | OpenAI cost-optimized. |
| `gpt-4.1-mini` | 0.40 | 1.60 | OpenAI newer mini. |
| `gpt-4.1-nano` | 0.10 | 0.40 | ⚠️ **ERRATA (see reference-price-table.md §4).** Correct is **$0.05 in / $0.20 out** (verified from OpenAI pricing page). `prices.yaml` v7: 0.05 / 0.20. |

#### Embedding Tier

| Model Family | Input $/M | Output $/M | Notes |
|-------------|-----------|------------|-------|
| `text-embedding-3-large` | 0.13 | N/A | OpenAI embedding. |
| `text-embedding-3-small` | 0.02 | N/A | OpenAI small embedding. |
| `voyage-3-large` | 0.18 | N/A | Voyage AI (used by Anthropic ecosystem). |

#### Self-Hosted Tier

| Model Family | Cost $/M tokens | Notes |
|-------------|----------------|-------|
| `self-hosted-frontier` | 2.00 | 70B+ parameter models on dedicated GPU infrastructure. |
| `self-hosted-standard` | 0.50 | 7B-70B parameter models on dedicated or shared GPU infrastructure. |
| `self-hosted-efficient` | 0.10 | Sub-7B models, embeddings, rerankers on any infrastructure. |

Self-hosted tiers are explained in detail in Section 3.

#### Cross-Provider Defaults (Fallbacks)

> ⚠️ **NOT IMPLEMENTED — target design only.** These tiered defaults (and the
> `standard-default 3.00/12.00` "ultimate fallback") do **not** exist in the code.
> The shipped fallback (`internal/store/prices.go` `ComputeCost`) prices an
> unmatched model at a **self-hosted reference rate**: the size-class heuristic
> (`self-hosted-large` $2.00/M, `self-hosted-medium` $0.50/M, `self-hosted-small`
> $0.10/M) or the flat `self-hosted-medium` $0.50/M, each with a one-time `WARN`
> and the unknown-model counters (#267/#135). See the implementation-status banner
> at the top and `docs/reference-price-table.md` §5.

| Tier | Input $/M | Output $/M | When Used |
|------|-----------|------------|-----------|
| `frontier-default` | 15.00 | 75.00 | Unknown model from known frontier-class provider. |
| `standard-default` | 3.00 | 12.00 | Unknown model, unknown tier. **This is the ultimate fallback.** |
| `efficient-default` | 0.40 | 1.60 | Unknown model explicitly tagged as small/mini/nano. |

### 2.4 RPT Update Cadence

> ⚠️ **Target cadence, not current practice.** No RPT has been published on this
> cadence to date. Today the reference table ships embedded in the binary
> (`internal/store/prices.yaml`, v7, effective 2026-07-13) and is updated by
> editing that file, not by an annual January-1 publication.

- **Annual publication:** A new RPT is intended to be published each January 1. It is the sole reference for all TIER scores computed during that calendar year.
- **Mid-year addenda:** When a major new model launches mid-year (e.g., a new frontier model from a new provider), a supplemental entry is published as an addendum to the current RPT. Addenda only ADD entries; they never modify existing prices.
- **No retroactive changes:** Once a score is computed against RPT-2026, it is never recomputed against RPT-2027. Year-over-year comparisons are explicitly acknowledged as being against different price baselines (see Section 5).

### 2.5 Special Token Types

**Extended thinking / chain-of-thought tokens (Claude, Gemini):**
Some models bill "thinking" tokens at different rates than standard output tokens. For TIER RPT purposes:
- Thinking tokens are priced at the model's standard **output** rate.
- Rationale: Thinking tokens are compute the model performs to improve quality. They are functionally output tokens that the developer chose to enable. Pricing them differently would penalize developers who use reasoning modes, even when reasoning produces better outcomes (which the quality multiplier already rewards).

**Cached input tokens:**
> **Superseded 2026-07** by `docs/pricing-philosophy.md` (formalizes #55; see `docs/reference-price-table.md` §7). The rule below priced cached tokens at the full standard input rate; the shipped cost engine instead applies the provider's real cache-mechanics multipliers (Anthropic 0.1x read / 1.25x-2.0x writes, OpenAI 0.5x read), because those multipliers are published list-price mechanics, not a procurement discount. The original text is kept struck-through for provenance.
- ~~Cached tokens are priced at the model's standard **input** rate, not the discounted cache-hit rate.~~
- Cached tokens are priced at the provider's published cache-mechanics rate (read/write multipliers applied off the selected input rate) — see `docs/pricing-philosophy.md` §1-2 and `docs/reference-price-table.md` §7. Only *procurement* discounts (enterprise, batch, subscription) are excluded; cache multipliers are mechanics that change the list cost of the call, so they stay in the denominator.

**Batch API tokens (Anthropic, OpenAI):**
- Priced at the standard (non-batch) rate — a **procurement discount** on identical compute, excluded from the denominator by rule (not by pipeline omission). See `docs/pricing-philosophy.md` §2.
- Rationale: Batch pricing is a billing discount for latency tolerance. It does not reflect a different level of AI capability consumed.

**Image generation tokens:**
- Image generation models (DALL-E, FLUX) are priced per image, not per token. Convert to effective cost directly: one image generation = the model's list price per image.
- RPT carries a `cost_per_image` field for image models rather than per-million-token pricing.

---

## 3. Self-Hosted Inference Cost Model

### 3.1 The Problem

Self-hosted inference has no per-token invoice. The cost is buried in hardware amortization, electricity, cooling, networking, staffing, and utilization rates. TIER must define a standard method to convert these into an effective cost-per-token that is comparable to API pricing.

### 3.2 Design Decision: Reference Tiers, Not Actual Amortization

TIER does **not** require organizations to calculate their actual amortized cost-per-token. This would be:
- Extremely complex (shared clusters, variable utilization, multi-tenant scheduling)
- Non-comparable (one org's 3-year amortization vs. another's 5-year)
- Gameable (claim high amortized cost to lower your TIER denominator)

Instead, TIER assigns self-hosted inference to one of three **reference tiers** based on the model's parameter count and quantization:

| Tier | Model Criteria | Reference Cost | Rationale |
|------|---------------|----------------|-----------|
| `self-hosted-frontier` | 70B+ params at Q8/FP16, or 100B+ MoE at Q4+ | $2.00 / M tokens | Approximates the cost of running Llama 3.3 70B or Nemotron 120B on DGX-class hardware, amortized over 3 years at 50% utilization. Deliberately set below API frontier pricing to reflect the capital investment advantage, but high enough that self-hosted is not "free" in the metric. |
| `self-hosted-standard` | 7B-70B params at any quantization, or 70B+ at Q4 | $0.50 / M tokens | Approximates mid-tier models on workstation or cloud GPU infrastructure. |
| `self-hosted-efficient` | Sub-7B params, embeddings, rerankers, classifiers | $0.10 / M tokens | Small models that run on modest hardware. Deliberately non-zero: even cheap inference has a cost. |

### 3.3 Self-Hosted Tier Assignment

The TIER token collector must tag each self-hosted token event with the model name. The cost engine maps the model to a tier using this logic:

```
function assign_self_hosted_tier(model_name, model_metadata):
    params = model_metadata.parameter_count  # from model card or config
    quant  = model_metadata.quantization      # e.g., "Q4_K_M", "FP16", "Q8"
    arch   = model_metadata.architecture      # "dense" or "moe"

    if arch == "moe" and params >= 100_000_000_000:
        return "self-hosted-frontier"
    elif params >= 70_000_000_000 and quant in ["FP16", "Q8", "FP8"]:
        return "self-hosted-frontier"
    elif params >= 7_000_000_000:
        return "self-hosted-standard"
    else:
        return "self-hosted-efficient"
```

### 3.4 Why Not Actual Amortization?

Consider two organizations running identical Llama 3.3 70B workloads:

- **Org A:** DGX Spark cluster, purchased 2025, 3-year amortization, 60% utilization, includes electricity at $0.12/kWh. Calculated cost: $0.85/M tokens.
- **Org B:** Leased cloud GPUs (A100 spot instances), variable pricing, 30% utilization due to bursty workload. Calculated cost: $3.20/M tokens.

If TIER used actual costs, Org B's developers would appear 3.8x less efficient than Org A's developers for identical work. This is an infrastructure procurement signal, not a developer efficiency signal.

The reference tier approach treats both as `self-hosted-frontier` at $2.00/M tokens, isolating the developer productivity signal from infrastructure economics.

### 3.5 Organizational Override (Optional, Flagged)

Organizations MAY override the reference tier cost with their actual calculated cost-per-token. When they do:
- The override is recorded in the TIER database alongside the reference tier cost.
- Dashboard displays show both: "Reference TIER" (using RPT) and "Actual-Cost TIER" (using org-specific pricing).
- **Only Reference TIER is used for cross-organization benchmarking.** Actual-Cost TIER is for internal cost management only.
- Overrides are flagged with `cost_basis: "org_override"` so they are never silently mixed with reference-priced scores.

---

## 4. Hybrid Environment Cost Rollup

### 4.1 The Scenario

A developer working on Issue TIER-42 uses three AI tools:
1. **Local Llama 3.3 70B** on DGX Spark for exploratory prompting (self-hosted-frontier)
2. **Claude Sonnet 4** via API for code generation (standard API)
3. **Claude Opus 4** via API for architecture review (frontier API)

### 4.2 Cost Computation

Each token event is priced independently using the RPT for the current year:

```
Event 1: Local Llama exploration
    150,000 input + 50,000 output = 200,000 total tokens
    Cost = 200,000 * ($2.00 / 1,000,000) = $0.40

Event 2: Claude Sonnet code generation
    80,000 input + 120,000 output tokens
    Cost = (80,000 * $3.00/M) + (120,000 * $15.00/M) = $0.24 + $1.80 = $2.04

Event 3: Claude Opus architecture review
    60,000 input + 15,000 output tokens
    Cost = (60,000 * $15.00/M) + (15,000 * $75.00/M) = $0.90 + $1.125 = $2.025

Total effective cost for TIER-42: $0.40 + $2.04 + $2.025 = $4.465
```

### 4.3 The TIER Score

If TIER-42 is a weight-8 issue shipped at quality 1.0:

```
TIER for this issue = (8 * 1.0) / ($4.465 / $1,000) = 8.0 / 0.004465 = 1,791.6
```

This tells us: the developer produced 1,791.6 outcome points per $1,000 of AI inference. Alternatively: **it cost $4.47 in AI to ship this feature.**

### 4.4 Key Properties of Hybrid Rollup

1. **Additive across tools:** Costs from different tools simply sum. No special weighting or normalization between API and self-hosted.
2. **Model choice is visible:** The per-event cost breakdown shows that 45% of the cost came from the Opus review. This is actionable intelligence -- was the Opus review worth 45% of the budget?
3. **Self-hosted is not free:** The local Llama exploration cost $0.40 at reference rates. This prevents the gaming vector where developers route all exploration to "free" self-hosted and only use the API for the final answer.
4. **Unattributed tokens:** Any token events not attributed to an issue are summed into the developer's total cost denominator at the developer level. They inflate the denominator without adding to any issue's numerator, naturally penalizing unfocused usage.

   - **Per-message attribution:** attribution is resolved for each assistant message by the branch it *actually happened on*, not one branch latched for the whole session. A session that opens on `main` and later checks out `feature/<N>` has its main messages bucketed as exploratory and its feature messages attributed to `#N` — the single latched branch no longer routes an entire mixed session to one verdict.
   - **No mis-attribution of exploratory `main`:** a message on a mainline branch (or a detached HEAD) never *inherits* a nearby merge commit's `closes #N`. That cost is real exploratory/planning work, and a false attribution is worse than an honest one, so it stays unattributed.
   - **Labeled buckets (honest split):** unattributed spend is NOT excluded from the denominator — it stays in as overhead — but the single `unattributed` mass is split into labeled buckets so the overhead is *visible* rather than a silent hole: `unattributed:main` (exploratory on a mainline branch), `unattributed:detached-head`, and `unattributed:branch-without-issue`. Producers that see no branch (the proxy, org-level pollers) keep the base `unattributed` label. Every read path that splits attributed vs. unattributed treats the whole family as unattributed; the buckets surface on `/scores` (`data_quality.unattributed_buckets`, `data_quality.exploratory_cost_share`, and a per-developer `exploratory_cost_share`), in `tierd doctor`'s attribution line, and in `tierd score`'s cost-by-issue report. The split changes only the *labeling* of the remainder — total spend, and therefore TIER, is unchanged.
   - **Trunk-based development:** work committed directly to `main` (rather than through a feature branch) is bucketed as `unattributed:main`, not attributed via its merge commit — the deliberate consequence of the no-mis-attribution rule above. Trunk-based teams should expect a higher exploratory share and can raise attribution by naming branches `feature/<issue-number>-slug`.
   - **Attribution reflects commit state at ingest time:** a stored token event's `issue_id` is pinned when it is first ingested (the store's idempotency upsert updates token counts, never `issue_id`). A commit that lands *after* a message was ingested does not retroactively re-attribute the stored row. Consequently, a fresh parse (`tierd score` / `tierd doctor`, which re-resolve against the current commit set) can report a different bucket split than `/scores` and the dashboard (which read the stored, ingest-time attribution) when commits arrive between ingests — a staleness gap, not a bug.

---

## 5. The Temporal Problem: Year-Over-Year Comparability

### 5.1 The Problem

Token prices drop approximately 30-60% year-over-year. Claude Sonnet launched at $3/$15 per M tokens; a successor might launch at $1.50/$7.50. If TIER scores are computed against current prices, a TIER score of 500 in 2026 is not comparable to 500 in 2027 -- the 2027 score was computed against cheaper tokens.

### 5.2 Solution: RPT-Year Tagging

Every TIER score carries an RPT version tag:

```
{
    "developer": "alice",
    "tier_score": 1791.6,
    "rpt_version": "RPT-2026",
    "period": "2026-W13",
    "cost_basis": "reference"
}
```

**Rules for comparison:**
- Scores computed against the same RPT version are directly comparable.
- Scores computed against different RPT versions are **not directly comparable** without adjustment.
- The TIER dashboard displays the RPT version alongside every score.
- Cross-year trend charts include a disclaimer: "Scores across RPT boundaries reflect both developer efficiency changes and reference price changes."

### 5.3 The TIER Price Index (TPI)

To enable approximate cross-year comparison, TIER publishes a **TIER Price Index** alongside each RPT. The TPI is analogous to CPI -- it measures how the average cost of a "basket of AI inference" has changed relative to a base year.

**Base year:** RPT-2026 = TPI 1.000

**TPI computation:**

The "basket" is a weighted mix of token consumption representing typical AI-augmented development:

| Component | Weight | Rationale |
|-----------|--------|-----------|
| Standard-tier output tokens | 40% | Most development AI spend is on workhorse model output. |
| Standard-tier input tokens | 25% | Context loading is a major cost driver. |
| Frontier-tier output tokens | 15% | Architecture, review, and complex reasoning tasks. |
| Frontier-tier input tokens | 10% | Large context for frontier models. |
| Efficient-tier output tokens | 5% | Cheap models for simple tasks. |
| Self-hosted-standard tokens | 5% | Local inference component. |

```
TPI_YYYY = Sigma(weight_i * RPT_YYYY_price_i) / Sigma(weight_i * RPT_2026_price_i)
```

**Example:** If standard-tier output tokens drop from $15.00/M (RPT-2026) to $7.50/M (RPT-2027) and all other prices halve similarly:

```
TPI_2027 ~ 0.50
```

**Adjusted comparison:**

```
TIER_adjusted = TIER_raw * TPI_current / TPI_target

# "What would Alice's 2027 score be at 2026 prices?"
TIER_2026_equivalent = 1791.6 * (0.50 / 1.00) = 895.8
```

This means: if Alice's raw TIER in 2027 is 1791.6 (computed against cheaper RPT-2027 prices), her efficiency in 2026-equivalent terms is 895.8. She is actually less efficient than someone who scored 1791.6 in 2026.

### 5.4 What TPI Does NOT Do

- TPI does not retroactively change historical scores. Alice's 2026 score stays 1791.6 forever.
- TPI is an approximation for trend analysis, not a precise conversion. The basket weights are stylized, not measured per-organization.
- TPI is published by the TIER project, not by individual organizations. Organizations may publish their own internal price indices, but cross-org benchmarking uses the official TPI.

---

## 6. The $1,000 Normalization Constant

### 6.1 Why $1,000?

The denominator divides total effective cost by $1,000 to produce human-readable scores. The choice of $1,000 is calibrated to produce scores in a range where:

1. **Individual developers land in the hundreds to low thousands.** A typical developer shipping 2-3 medium issues per week at ~$5-15 in AI cost per issue produces a weekly TIER of roughly 300-2,000.
2. **The number is large enough to feel meaningful** but not so large that differences are invisible.
3. **$1,000 has intuitive meaning:** "How many outcome points does this developer produce per $1,000 of AI spend?" is a natural question for a CFO or VP of Engineering.

### 6.2 Calibration Analysis

Using the worked examples from the project overview (re-priced with RPT-2026):

| Developer | Issues | Weighted Outcome | Estimated Weekly AI Cost | TIER (per $1K) |
|-----------|--------|------------------|--------------------------|----------------|
| Bob | 2 | 21.0 | ~$4.50 | 4,667 |
| Eve | 2 | 13.0 | ~$3.80 | 3,421 |
| Alice | 2 | 12.8 | ~$6.20 | 2,065 |
| Charlie | 2 | 12.5 | ~$9.50 | 1,316 |
| Diana | 2 | 6.5 | ~$18.00 | 361 |
| Frank | 2 | 7.0 | ~$25.00 | 280 |

**Score range: 280 to 4,667.** This is a clean, readable range with clear separation between high and low performers.

### 6.3 Alternative Constants Considered and Rejected

| Constant | Score Range | Verdict |
|----------|-------------|---------|
| $1 | 280,000 - 4,667,000 | Scores in the millions. Unreadable. Rejected. |
| $100 | 2,800 - 46,670 | Reasonable but less intuitive. "$100 of AI" is too small for executive framing. |
| **$1,000** | **280 - 4,667** | **Clean range. Intuitive unit. Selected.** |
| $10,000 | 28 - 467 | Compresses the low end. A score of 28 vs 35 is hard to reason about. |
| $100,000 | 2.8 - 46.7 | Too small. Feels like a percentage, not a productivity metric. |

### 6.4 The Normalization Constant Is Fixed

$1,000 is part of the TIER specification. It does not change between RPT versions. This ensures that a TIER score of 1,500 always means "1,500 outcome points per $1,000 of AI inference" regardless of measurement year.

---

## 7. Cost Engine Implementation

### 7.1 Event Processing Pipeline

> ⚠️ **Illustrative pseudocode — the fallback branch is target design.** The
> `infer_tier_from_model_name` / `RPT[tier + "-default"]` lookup and the
> `price_confidence` enrichment below are **not** what ships. The real
> `ComputeCost` (`internal/store/prices.go`) resolves an unmatched model to a
> self-hosted reference rate (size-class heuristic or flat `self-hosted-medium`
> $0.50/M) with a one-time `WARN` and the unknown-model event/cost counters
> (#267/#135); there is no `-default` tier and no `price_confidence` column. See
> the implementation-status banner at the top.

```
For each token_event arriving at the cost engine:

1. EXTRACT model string from event
   model_raw = event.model  # e.g., "claude-sonnet-4-20250514"

2. NORMALIZE model string
   model_family = strip_date_suffix(strip_version_patch(model_raw))
   # "claude-sonnet-4-20250514" -> "claude-sonnet-4"

3. DETERMINE source type
   if event.source in ["self_hosted", "vllm", "llama_server", "ollama", "tgi"]:
       source_type = "self_hosted"
   else:
       source_type = "api"

4. LOOK UP reference price
   if source_type == "api":
       if model_family in RPT:
           input_price  = RPT[model_family].input_per_million
           output_price = RPT[model_family].output_per_million
           confidence   = "exact"
       else:
           tier = infer_tier_from_model_name(model_family)
           input_price  = RPT[tier + "-default"].input_per_million
           output_price = RPT[tier + "-default"].output_per_million
           confidence   = "estimated"
   else:  # self_hosted
       sh_tier    = assign_self_hosted_tier(model_family, event.model_metadata)
       cost_rate  = RPT[sh_tier].cost_per_million
       confidence = "reference_tier"

5. COMPUTE effective cost
   if source_type == "api":
       cost = (event.input_tokens * input_price / 1_000_000)
            + (event.output_tokens * output_price / 1_000_000)
   else:
       total_tokens = event.input_tokens + event.output_tokens
       cost = total_tokens * cost_rate / 1_000_000

6. STORE enriched event
   event.effective_cost    = cost
   event.rpt_version       = "RPT-2026"
   event.price_confidence  = confidence
   event.model_family      = model_family
   event.self_hosted_tier  = sh_tier if self_hosted else null
   store(event)
```

### 7.2 TIER Score Computation

```
function compute_tier(entity, period, rpt_version):
    # entity = developer | team | division | org
    # period = week | month | quarter

    events = get_events(entity, period, rpt_version)

    total_cost = SUM(event.effective_cost for event in events)

    outcomes = get_outcomes(entity, period)
    weighted_outcome = SUM(o.weight * o.quality_multiplier for o in outcomes)

    if total_cost == 0:
        return null  # No AI usage in period; TIER is undefined, not infinity

    tier_score = weighted_outcome / (total_cost / 1000.0)

    return {
        "score": tier_score,
        "total_cost": total_cost,
        "weighted_outcome": weighted_outcome,
        "rpt_version": rpt_version,
        "period": period,
        "event_count": len(events),
        "price_confidence": min_confidence(events)  # lowest confidence in the set
    }
```

### 7.3 Edge Cases

| Scenario | Handling |
|----------|----------|
| Zero tokens in period | TIER = null (undefined). Not zero, not infinity. Displayed as "No data." |
| Zero outcomes in period | TIER = 0.0. The developer consumed AI but shipped nothing. This is a legitimate and informative score. |
| All events have `confidence: "estimated"` | TIER is computed but flagged with a warning: "Score based on estimated pricing -- model identification may be incomplete." |
| Self-hosted with no model metadata | Default to `self-hosted-standard` ($0.50/M). Log a warning to improve model tagging. |
| Image generation events | Use `cost_per_image` from RPT instead of per-token pricing. Sum into total_cost normally. |
| Token events with only total_tokens (no input/output split) | For API models: assume a 60/40 input/output split (industry average). For self-hosted: use total tokens at the combined rate. Flag as `confidence: "estimated"`. |

---

## 8. Governance and Maintenance

### 8.1 RPT Publication Process

> ⚠️ **Proposed process, not yet operating.** No RPT has been published through
> this process, and no release has been cut; the community-review and
> January-1 publication steps below are the intended governance model, not a
> record of anything that has happened. Today the table ships embedded
> (`internal/store/prices.yaml`, v7, effective 2026-07-13).

1. **Data collection (December):** The TIER project scrapes current list prices from all major providers' public pricing pages.
2. **Self-hosted tier calibration (December):** Review GPU pricing trends, update the three self-hosted tier rates if hardware costs have shifted materially (>20% change from prior year).
3. **Community review (December 15-31):** Proposed RPT is published as a GitHub PR for community comment.
4. **Publication (January 1):** RPT-YYYY is merged and tagged. It becomes the sole reference for the new year.
5. **Mid-year addenda:** New model entries are proposed via PR and merged after 7-day review. Existing entries are never modified mid-year.

### 8.2 Self-Hosted Tier Calibration Methodology

The self-hosted reference costs are derived from a reference configuration:

**Reference hardware:** NVIDIA DGX-class system (H200 or equivalent generation)
**Amortization:** 3 years, straight-line
**Utilization:** 50% (accounts for idle time, maintenance, burst headroom)
**Electricity:** $0.10/kWh (US average commercial rate)
**Staffing overhead:** 10% of hardware cost (1 part-time ML engineer per 5 GPU nodes)
**PUE (Power Usage Effectiveness):** 1.3 (modern data center)

```
Annual cost per GPU node =
    (hardware_cost / 3)           # amortization
    + (power_draw_kw * 8760 * PUE * electricity_rate)  # electricity
    + (hardware_cost / 3 * 0.10)  # staffing

Cost per token =
    annual_cost / (tokens_per_second * 3600 * 8760 * utilization)
```

This calculation is published alongside the RPT for transparency. Organizations can see exactly how the self-hosted tiers were derived and raise objections if the assumptions are materially wrong.

### 8.3 What Happens When a Provider Changes Pricing Mid-Year

Nothing. The RPT-2026 prices remain in effect for all of 2026. If Anthropic halves Claude Sonnet pricing in June 2026, TIER scores for H2 2026 will still use the January 2026 reference prices. The actual billing savings are captured in the organization's finance systems, not in TIER.

This is intentional. TIER measures developer efficiency against a stable benchmark. If a developer's TIER improves in H2 2026, it is because they became more efficient, not because their provider dropped prices.

---

## 9. Frequently Anticipated Objections

### "Reference prices penalize organizations that invest in self-hosted infrastructure."

No. Self-hosted tiers are deliberately priced below API equivalents. Running Llama 70B locally costs $2.00/M tokens in TIER vs $15.00/M for Claude Sonnet output via API. A developer who achieves the same outcome with self-hosted inference gets a higher TIER score. The investment in infrastructure is rewarded, not penalized.

### "Our enterprise discount means we actually pay $5/M for Opus output, not $75/M. TIER makes us look expensive."

TIER is not measuring your procurement efficiency. It is measuring your developer efficiency at a standardized price point. Your $5/M actual cost is relevant to your CFO's ROI analysis, not to your VP of Engineering's productivity analysis. Use the "Actual-Cost TIER" organizational override for internal cost management.

### "Self-hosted at $2.00/M is too high. Our actual cost is $0.30/M after amortization."

The reference tier is designed for cross-organization comparability, not internal cost accounting. $2.00/M represents the amortized cost at 50% utilization for organizations that have not yet fully optimized their GPU utilization. If your utilization is 90% and your amortization is complete, your actual cost is lower -- and you can track that via the organizational override for internal dashboards.

### "Why not just use actual costs? Every other financial metric uses actual costs."

Because TIER is not a financial metric. It is a productivity metric. Financial metrics answer "how much did we spend?" Productivity metrics answer "what did we get per unit of effort?" Using actual costs conflates procurement skill with engineering skill. Standardized pricing isolates the developer signal. This is the same reason DORA does not adjust deployment frequency by the cost of the CI/CD platform.

### "The $1,000 constant is arbitrary."

Yes, in the same way that "per million tokens" in the original formula was arbitrary. The constant exists solely for readability. It could be $500 or $2,000 and the relative rankings would be identical. $1,000 was chosen because it produces scores in the hundreds-to-thousands range and has intuitive meaning in business conversations about AI spend.

---

## 10. Summary of Design Decisions

| Question | Decision | Rationale |
|----------|----------|-----------|
| List price or actual price? | **Reference price (RPT)** | Isolates developer efficiency from procurement. |
| Price at time of consumption or fixed? | **Fixed per calendar year (RPT-YYYY)** | Enables within-year comparability. |
| Self-hosted cost calculation? | **Reference tiers, not actual amortization** | Comparable across orgs. Not gameable. |
| Amortization period? | **3 years at 50% utilization (for tier calibration only)** | Used only to derive reference tier prices. |
| Shared cluster allocation? | **Irrelevant -- reference tiers eliminate this** | No per-org amortization math needed. |
| Utilization rate impact? | **No -- reference tier absorbs this** | 50% utilization baked into tier pricing. |
| Hybrid cost rollup? | **Simple addition across all sources** | Each event priced independently, then summed. |
| Year-over-year comparability? | **TPI (TIER Price Index) for adjustment** | Analogous to CPI. Base year = 2026. |
| Normalization constant? | **$1,000 (fixed, never changes)** | Produces readable scores in 280-4,667 range. |
| Mid-year price changes? | **Ignored until next RPT** | Stability over accuracy for productivity measurement. |
