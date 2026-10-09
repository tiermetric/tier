# RPT-2026: Complete Reference Price Table
## TIER Framework — Token Impact & Efficiency Ratio
**Narrative companion to `internal/store/prices.yaml`** — describing table `version: 12`, `effective_date: 2026-10-02`.

**Status:** Published reference. The authoritative, machine-readable table is `internal/store/prices.yaml`, which `tierd` embeds at build time and which this document describes. **Where the two disagree, the YAML is correct** — it is the single source of truth, and this page carries no independent version number precisely so the two cannot drift apart.

🔴 **Before you edit this table or a `--prices` override, read [§9, `version:` Is Binding](#9-version-is-binding--one-number-one-table-714).** A change to this table needs the next `version:` integer; a change to an override needs a `version:` this database has never recorded. Without that, one integer denotes two different price tables; §9 says exactly which changes count. A **`--prices` override** that does it is refused outright; an edit to **this** table is refused on every deployment that already served the previous version. There is no flag to accept it; the remedy is always to bump the version.

**Purpose:** Comprehensive, research-verified reference pricing for every major AI model used in software development, effective for all TIER score calculations during calendar year 2026.

**Data sourced:** Base rows captured March 26, 2026 from official provider pricing pages and verified third-party aggregators; subsequent revisions through `version: 12` (2026-10-02) added and re-verified individual rows — `version: 8` (#461) added the GPT-5.6 Codex family (`gpt-5.6-sol` $5/$30, `gpt-5.6-terra` $2.50/$15, `gpt-5.6-luna` $1/$6, OpenAI standard-tier list); `version: 9` added `claude-opus-5` at $5/$25, verified against the vendor catalog rather than inferred from the 4.x pattern; `version: 10` (#786) added `claude-fable-5-1`, `claude-mythos-5-1`, `claude-opus-5-5`, the GPT-6 family and the Z.ai GLM-5.3 rows under a new `zai` provider, and corrected `claude-sonnet-5` from $3/$15 to $2/$10; `version: 11` (#895) added Meta's Muse Spark models (`muse-spark-1.3`, `-1.2`, `-1.1` and the `-contributor` tier of 1.3 and 1.2) under a new `meta` provider, from Meta's own pricing page read 2026-09-27; `version: 12` (#1047) added `claude-sonnet-5-5` at $2/$10 with the standard Anthropic cache rates, from Anthropic's pricing page read 2026-10-02. Per the table's own versioning rule, `effective_date` stamps when the table was last revised **as a whole** — it is not a guarantee that every row was re-captured on that date.

---

## 1. Cloud API Models (Pay-Per-Token)

All prices are USD per million tokens ($/MTok) at standard list rates. Batch API discounts and enterprise negotiations are excluded per TIER specification (see cost normalization spec Section 2.1). **Prompt caching multipliers are applied** — see §7 below.

### 1.1 Frontier Tier

Models representing the highest capability level. Used for architecture review, complex reasoning, and high-stakes code generation.

| `model_family` | Provider | Input $/M | Output $/M | Context | Notes |
|----------------|----------|-----------|------------|---------|-------|
| `gpt-5.4-pro` | OpenAI | 30.00 | 180.00 | 1M | OpenAI ultra-premium tier. Highest cost model on the market. |
| `o1` | OpenAI | 15.00 | 60.00 | 200K | OpenAI reasoning model (legacy frontier). |
| `claude-opus-4` | Anthropic | 15.00 | 75.00 | 200K | Legacy Anthropic flagship. Still available. |
| `claude-opus-4.1` | Anthropic | 15.00 | 75.00 | 200K | Legacy Anthropic flagship. Still available. |
| `claude-opus-4.5` | Anthropic | 5.00 | 25.00 | 1M | Anthropic flagship (current gen). Major price reduction from Opus 4. |
| `claude-opus-4.6` | Anthropic | 5.00 | 25.00 | 1M | Anthropic Opus (current gen). Fast mode: $30/$150. |
| `claude-opus-4.7` | Anthropic | 5.00 | 25.00 | 1M | Anthropic Opus. |
| `claude-opus-4.8` | Anthropic | 5.00 | 25.00 | 1M | Anthropic Opus (previous generation). |
| `claude-opus-5` | Anthropic | 5.00 | 25.00 | 1M | Anthropic latest Opus flagship. Same $5/$25 as 4.8. Fast mode bills $10/$50 (not encoded — see the [fast-mode caveat](../internal/store/prices.yaml)). |
| `claude-fable-5` | Anthropic | 10.00 | 50.00 | 1M | Anthropic most-capable model (distinct family, above Opus tier). |
| `claude-mythos-5` | Anthropic | 10.00 | 50.00 | 1M | Same $10/$50 flagship rate as Fable 5 (Project Glasswing). Mid-year addendum (v7). |
| `claude-fable-5-1` | Anthropic | 10.00 | 50.00 | — | Fable 5.1. Cache read $0.25 (0.025×); writes $12.50 (5m) / $20 (1h). v10 (#786). |
| `claude-mythos-5-1` | Anthropic | 10.00 | 50.00 | — | Mythos 5.1. Same rates as Fable 5.1. v10 (#786). |
| `claude-opus-5-5` | Anthropic | 4.00 | 20.00 | — | Opus 5.5. Cache read $0.20 (0.05×); writes $5 (5m) / $8 (1h). Fast mode $8/$40 not encoded. v10 (#786). |
| `gpt-6-astra` | OpenAI | 10.00 | 50.00 | — | GPT-6 flagship. Cache read $1.00 (0.1×), cache write $12.50 (1.25×). v10 (#786). |
| `gpt-5.4` | OpenAI | 2.50 | 15.00 | 1M | OpenAI latest flagship (Mar 2026). |
| `grok-3-fast` | xAI | 5.00 | 25.00 | 131K | xAI premium speed tier. |
| `grok-4` | xAI | 3.00 | 15.00 | 131K | xAI latest flagship. |

**RPT-2026 Frontier reference model:** `claude-opus-4` at $15.00/$75.00 (set at RPT publication; not updated mid-year per spec Section 2.4).

**Important note on Opus pricing evolution:** Claude Opus 4 and 4.1 launched at $15/$75. Opus 4.5, 4.6, 4.7, 4.8, and 5 all dropped to $5/$25 -- a 67% price reduction. Note the reduction did NOT carry backwards: 4.1 remains $15/$75, so "a newer Opus is cheaper" is not a rule and each generation must be captured, not inferred. For RPT-2026, we retain the original `claude-opus-4` entry at $15/$75 as the frontier reference anchor (set January 2026). The Opus 4.5–4.8 and Opus 5 entries are mid-year addenda at their actual list prices. Note these minor versions do NOT share the base-family rate, so the code price table (`internal/store/prices.yaml`) lists each explicitly rather than inferring a family rate (#80). Claude Fable 5 is a distinct, more-capable family priced above Opus tier at $10/$50. Similarly, `gpt-4.5` at $75/$150 has been retired by OpenAI and replaced by the GPT-5.x series at dramatically lower prices.

---

### 1.2 Standard Tier

Primary workhorse models for day-to-day coding, code review, and feature implementation.

| `model_family` | Provider | Input $/M | Output $/M | Context | Notes |
|----------------|----------|-----------|------------|---------|-------|
| `claude-sonnet-4` | Anthropic | 3.00 | 15.00 | 200K | Anthropic primary coding model. |
| `claude-sonnet-4.5` | Anthropic | 3.00 | 15.00 | 200K | Anthropic updated Sonnet. 1M beta at $6/$22.50. |
| `claude-sonnet-4.6` | Anthropic | 3.00 | 15.00 | 1M | Anthropic latest Sonnet. Full 1M at standard rate. |
| `claude-sonnet-5` | Anthropic | 2.00 | 10.00 | 1M | Anthropic current Sonnet. v7–v9 carried $3/$15; Anthropic made the $2/$10 introductory price standard and cancelled the 2026-09-01 increase, so v10 (#786) corrects it. |
| `claude-sonnet-5-5` | Anthropic | 2.00 | 10.00 | 1M | Sonnet 5.5. Cache read $0.20 (0.1×); writes $2.50 (5m) / $4 (1h): the Anthropic defaults. Its own row: `NormalizeModel` does not collapse it onto `claude-sonnet-5`. v12 (#1047), Anthropic pricing page read 2026-10-02. |
| `gpt-6-sol` | OpenAI | 2.00 | 10.00 | — | GPT-6 standard. Cache read $0.20 (0.1×), cache write $2.50 (1.25×). v10 (#786). |
| `muse-spark-1.3`, `muse-spark-1.2`, `muse-spark-1.1` | Meta | 1.25 | 4.25 | — | Meta Muse Spark. Cached input $0.15 (0.12×); cache writes bill at the input rate (no published write rate); reasoning tokens are part of output. Priced per token at list, also on a Muse subscription (operator ruling, #895). Web search ($2.50 per 1,000 queries) is not a token charge and is not encoded. v11. |
| `glm-5.3` | Z.ai | 1.40 | 4.40 | — | Z.ai GLM-5.3. Cache read $0.26; cache writes bill at the input rate (no published write rate). Priced per token at list, also on the coding plan (operator ruling, #786). v10. |
| `gpt-4o` | OpenAI | 2.50 | 10.00 | 128K | OpenAI established workhorse. |
| `gpt-4.1` | OpenAI | 2.00 | 8.00 | 1M | OpenAI standard (Apr 2025). |
| `gpt-5` | OpenAI | 1.25 | 10.00 | 1M | OpenAI GPT-5 base. |
| `gpt-5.1` | OpenAI | 0.625 | 5.00 | 1M | OpenAI GPT-5.1. |
| `gpt-5.2` | OpenAI | 0.875 | 7.00 | 1M | OpenAI GPT-5.2. |
| `gpt-5.3-codex` | OpenAI | 1.75 | 14.00 | 1M | OpenAI Codex agentic coding model (Jan 2026). |
| `gpt-5.1-codex` | OpenAI | 1.25 | 10.00 | 1M | OpenAI Codex mid-tier coding model. |
| `o3` | OpenAI | 2.00 | 8.00 | 200K | OpenAI reasoning model. |
| `gemini-2.5-pro` | Google | 1.25 | 10.00 | 1M | Google primary. >200K input: $2.50/$15.00. |
| `gemini-3.1-pro` | Google | 2.00 | 12.00 | 1M | Google latest. >200K input: $4.00/$18.00. |
| `grok-3` | xAI | 3.00 | 15.00 | 131K | xAI standard model. |
| `deepseek-r1` | DeepSeek | 0.55 | 2.19 | 128K | DeepSeek reasoning. Cache hit: $0.028/M input. |
| `command-a` | Cohere | 2.50 | 10.00 | 256K | Cohere enterprise flagship. |
| `command-r-plus` | Cohere | 2.50 | 10.00 | 128K | Cohere enterprise model. |
| `mistral-large-3` | Mistral | 0.50 | 1.50 | 128K | Mistral flagship. Unusually affordable for capability. |

---

### 1.3 Efficient Tier

Cost-optimized models for high-volume tasks, simple code completions, test generation, and routine operations.

| `model_family` | Provider | Input $/M | Output $/M | Context | Notes |
|----------------|----------|-----------|------------|---------|-------|
| `claude-haiku-4.5` | Anthropic | 1.00 | 5.00 | 200K | Anthropic current small model. |
| `claude-haiku-3.5` | Anthropic | 0.80 | 4.00 | 200K | Anthropic legacy small. Still widely used. |
| `gpt-4o-mini` | OpenAI | 0.15 | 0.60 | 128K | OpenAI legacy cost-optimized. |
| `gpt-4.1-mini` | OpenAI | 0.40 | 1.60 | 1M | OpenAI mini (Apr 2025). |
| `gpt-4.1-nano` | OpenAI | 0.05 | 0.20 | 1M | OpenAI smallest legacy. |
| `gpt-5-mini` | OpenAI | 0.125 | 1.00 | 1M | OpenAI GPT-5 mini. |
| `gpt-5-nano` | OpenAI | 0.05 | 0.40 | 1M | OpenAI GPT-5 nano. |
| `gpt-5.4-mini` | OpenAI | 0.75 | 4.50 | 1M | OpenAI latest mini (Mar 2026). |
| `gpt-5.4-nano` | OpenAI | 0.20 | 1.25 | 1M | OpenAI latest nano (Mar 2026). |
| `gpt-5.1-codex-mini` | OpenAI | 0.25 | 2.00 | 1M | OpenAI cheapest Codex. |
| `gpt-6-luna` | OpenAI | 0.10 | 0.50 | — | GPT-6 small. Cache read $0.01 (0.1×), cache write $0.125 (1.25×). v10 (#786). |
| `muse-spark-1.3-contributor`, `muse-spark-1.2-contributor` | Meta | 0.10 | 0.20 | — | Meta Muse Spark Contributor tier: discounted in exchange for Meta training on prompts and completions. Cached input $0.002 (0.02×); cache writes bill at the input rate. Its own row, not an alias of the standard tier. There is no 1.1 Contributor tier. v11 (#895). |
| `glm-5.3-flash` | Z.ai | 0.15 | 0.50 | — | Z.ai GLM-5.3 Flash. Cache read $0.03 (0.2×); cache writes bill at the input rate (no published write rate). v10 (#786). |
| `o3-mini` | OpenAI | 0.55 | 2.20 | 200K | OpenAI reasoning mini (Jan 2025). |
| `o4-mini` | OpenAI | 1.10 | 4.40 | 200K | OpenAI reasoning mini (Apr 2025). |
| `gemini-2.5-flash` | Google | 0.30 | 2.50 | 1M | Google fast model. Thinking: additional cost. |
| `gemini-2.5-flash-lite` | Google | 0.10 | 0.40 | 1M | Google cheapest current model. |
| `gemini-2.0-flash` | Google | 0.10 | 0.40 | 1M | Google prior-gen. Sunset June 2026. |
| `gemini-3-flash` | Google | 0.50 | 3.00 | 1M | Google latest Flash. |
| `gemini-3.1-flash-lite` | Google | 0.25 | 1.50 | 1M | Google latest lite. |
| `grok-3-mini` | xAI | 0.30 | 0.50 | 131K | xAI cost-optimized. |
| `grok-4-fast` | xAI | 0.20 | 0.50 | 131K | xAI latest fast/efficient. |
| `grok-4.1-fast` | xAI | 0.20 | 0.50 | 131K | xAI latest fast variant. |
| `grok-code-fast-1` | xAI | 0.20 | 1.50 | 131K | xAI dedicated coding model. Higher output cost. |
| `deepseek-v3` | DeepSeek | 0.28 | 0.42 | 128K | DeepSeek-V3.2 chat. Cache hit: $0.028/M. |
| `command-r` | Cohere | 0.50 | 1.50 | 128K | Cohere standard. |
| `command-r7b` | Cohere | 0.0375 | 0.15 | 128K | Cohere ultra-budget. |
| `codestral` | Mistral | 0.30 | 0.90 | 256K | Mistral dedicated coding model. |
| `mistral-small-3.2` | Mistral | 0.075 | 0.20 | 128K | Mistral 24B small. |
| `devstral-small` | Mistral | 0.07 | 0.28 | 128K | Mistral coding agent model. |
| `amazon-nova-pro` | Amazon | 0.80 | 3.20 | 300K | AWS Bedrock native. |
| `amazon-nova-lite` | Amazon | 0.06 | 0.24 | 300K | AWS Bedrock budget. |

---

### 1.4 Embedding Tier

| `model_family` | Provider | Input $/M | Output $/M | Notes |
|----------------|----------|-----------|------------|-------|
| `text-embedding-3-large` | OpenAI | 0.13 | N/A | OpenAI 3072-dim embedding. |
| `text-embedding-3-small` | OpenAI | 0.02 | N/A | OpenAI 1536-dim embedding. |
| `voyage-3-large` | Voyage AI | 0.18 | N/A | Used in Anthropic ecosystem. |
| `gemini-embedding-2` | Google | 0.20 | N/A | Google text embedding. |

---

### 1.5 Open-Source Models (via Hosted APIs)

These models are open-weight and can be self-hosted (see Section 2) or accessed via inference providers (Together AI, Fireworks, DeepInfra, Groq, etc.). Prices below reflect typical hosted API rates, NOT self-hosted costs.

| `model_family` | Provider(s) | Params | Typical Hosted $/M (in+out avg) | Notes |
|----------------|-------------|--------|----------------------------------|-------|
| `llama-3.3-70b` | Meta (via providers) | 70B | $0.20-0.60 | Primary open-source workhorse. |
| `llama-3.1-405b` | Meta (via providers) | 405B | $1.00-3.00 | Largest open-source dense model. |
| `llama-3.2-8b` | Meta (via providers) | 8B | $0.03-0.10 | Efficient small model. |
| `llama-4-maverick` | Meta (via providers) | 400B MoE | $0.15-0.50 | MoE architecture, 17B active. |
| `qwen-2.5-72b` | Alibaba (via providers) | 72B | $0.20-0.50 | Strong multilingual model. |
| `qwen-2.5-coder-32b` | Alibaba (via providers) | 32B | $0.10-0.30 | Best open-source coding model for size. |
| `qwen-2.5-coder-7b` | Alibaba (via providers) | 7B | $0.03-0.10 | Small but capable coder. |
| `qwen-3-8b` | Alibaba (via providers) | 8B | $0.03-0.10 | Latest Qwen generation. |
| `qwen-3-32b` | Alibaba (via providers) | 32B | $0.10-0.30 | Latest Qwen mid-size. |
| `qwen-3-235b` | Alibaba (via providers) | 235B MoE | $0.30-0.80 | MoE, 22B active params. |
| `deepseek-coder-v2-236b` | DeepSeek | 236B MoE | $0.30-0.80 | MoE coding specialist. |
| `deepseek-v3` | DeepSeek | 671B MoE | $0.28-0.42 | Also available via DeepSeek API (see 1.3). |
| `codestral-22b` | Mistral | 22B | $0.10-0.30 | Open-weight coding model. |
| `starcoder2-15b` | BigCode | 15B | $0.05-0.15 | Open-source code completion. |
| `starcoder2-7b` | BigCode | 7B | $0.03-0.08 | Small code completion. |
| `codellama-70b` | Meta | 70B | $0.20-0.60 | Legacy coding model. Superseded by Llama 3.3 + Qwen Coder. |
| `codellama-34b` | Meta | 34B | $0.10-0.30 | Legacy mid-size coder. |
| `nvidia-nemotron-70b` | NVIDIA | 70B | $0.20-0.50 | NVIDIA's Llama-based model. |
| `gpt-oss-120b` | OpenAI | 120B | $0.039-0.10 | OpenAI's open-weight model. Extremely cheap. |
| `gpt-oss-20b` | OpenAI | 20B | $0.03-0.10 | OpenAI small open-weight. |

---

## 2. Self-Hosted Inference Cost Model

### 2.1 Reference Tier Pricing (For TIER Score Calculation)

Per the TIER cost normalization specification, self-hosted models use fixed reference tiers, NOT actual amortized costs. This ensures cross-organization comparability.

| Tier | Model Criteria | Reference $/M tokens | Rationale |
|------|---------------|---------------------|-----------|
| `self-hosted-frontier` | 70B+ params at Q8/FP16, or 100B+ MoE at Q4+ | **$2.00** | Fixed normalization rate. |
| `self-hosted-standard` | 7B-70B params at any quant, or 70B+ at Q4 | **$0.50** | Mid-tier GPU infrastructure. |
| `self-hosted-efficient` | Sub-7B params, embeddings, rerankers | **$0.10** | Modest hardware, small models. |

### 2.2 Estimated Actual Self-Hosted Costs (For Reference Only)

These are NOT used in TIER calculations. They are provided for organizations evaluating build-vs-buy decisions.

#### Cost Methodology

```
Annual cost per GPU node =
    (hardware_cost / amortization_years)
    + (power_draw_kw * 8760h * PUE * electricity_rate)
    + (hardware_cost / amortization_years * staffing_overhead)

Cost per million tokens =
    annual_cost * 1_000_000 / (tokens_per_second * 3600 * 8760 * utilization)
```

At 50% utilization, annual output is `tok/s * 3600 * 8760 * 0.5`, or
`tok/s * 15,768,000` tokens. Using the assumptions below:

| Hardware | Annual cost arithmetic (USD) | Annual cost |
|----------|------------------------------|-------------|
| DGX Spark | `4999 / 3 * 1.10 + 0.3 * 8760 * 1.1 * 0.12` | $2,179.862667 |
| Single H100 | `30000 / 3 * 1.10 + 0.7 * 8760 * 1.3 * 0.10` | $11,797.16 |
| DGX H100 | `300000 / 3 * 1.10 + 10.2 * 8760 * 1.3 * 0.10` | $121,615.76 |
| DGX H200 | `450000 / 3 * 1.10 + 10.2 * 8760 * 1.3 * 0.10` | $176,615.76 |

For example, Spark at 25-35 tok/s costs
`2179.862667 * 1,000,000 / (35 * 15,768,000)` to
`2179.862667 * 1,000,000 / (25 * 15,768,000)` = **$3.95-5.53/M**.
All ranges below use the same arithmetic, fastest throughput first, rounded to
cents. Power cost follows the stated full-year draw; utilization scales token
output only. These are estimates from the stated assumptions, not benchmarks.

#### DGX Spark (128GB Unified Memory, ~$4,999 MSRP)

| Model | Params | Quant | Est. tok/s | Est. $/M tokens | TIER Tier |
|-------|--------|-------|-----------|-----------------|-----------|
| Qwen 2.5 Coder 32B | 32B | Q8 | 25-35 | $3.95-5.53 | self-hosted-standard |
| Qwen 2.5 72B | 72B | Q4 | 12-18 | $7.68-11.52 | self-hosted-standard* |
| Llama 3.3 70B | 70B | Q4 | 12-18 | $7.68-11.52 | self-hosted-standard* |
| Qwen 3 8B | 8B | Q8 | 60-80 | $1.73-2.30 | self-hosted-efficient |
| Qwen3-Embedding 8B | 8B | FP16 | N/A | Not computable | self-hosted-efficient |

*70B at Q4 = self-hosted-standard per tier assignment rules (Section 3.3 of cost normalization spec).

**Assumptions:** $4,999 hardware, 3-year amortization, 300W power draw, $0.12/kWh, PUE 1.1 (small deployment), 50% utilization, 10% staffing overhead.

#### Single H100 80GB SXM (~$25,000-$35,000)

| Model | Params | Quant | Est. tok/s | Est. $/M tokens | TIER Tier |
|-------|--------|-------|-----------|-----------------|-----------|
| Qwen 2.5 Coder 32B | 32B | FP16 | 80-120 | $6.23-9.35 | self-hosted-standard |
| Llama 3.2 8B | 8B | FP16 | 200-300 | $2.49-3.74 | self-hosted-efficient |
| StarCoder2 15B | 15B | FP16 | 120-180 | $4.16-6.23 | self-hosted-standard |
| CodeLlama 34B | 34B | FP16 | 60-90 | $8.31-12.47 | self-hosted-standard |

**Assumptions:** $30,000 hardware, 3-year amortization, 700W TDP, $0.10/kWh, PUE 1.3, 50% utilization, 10% staffing.

#### 8x H100 DGX H100 Node (~$300,000)

| Model | Params | Quant | Est. tok/s | Est. $/M tokens | TIER Tier |
|-------|--------|-------|-----------|-----------------|-----------|
| Llama 3.3 70B | 70B | FP16 | 200-400 | $19.28-38.56 | self-hosted-frontier |
| Qwen 2.5 72B | 72B | FP16 | 200-400 | $19.28-38.56 | self-hosted-frontier |
| NVIDIA Nemotron 70B | 70B | FP16 | 200-400 | $19.28-38.56 | self-hosted-frontier |
| DeepSeek Coder V2 236B | 236B MoE | FP8 | 80-150 | $51.42-96.41 | self-hosted-frontier |
| Llama 3.1 405B | 405B | FP16 | 40-80 | $96.41-192.82 | self-hosted-frontier |

**Assumptions:** $300,000 hardware, 3-year amortization, 10.2kW TDP, $0.10/kWh, PUE 1.3, 50% utilization, 10% staffing.

#### 8x H200 DGX H200 Node (~$400,000-$500,000)

| Model | Params | Quant | Est. tok/s | Est. $/M tokens | TIER Tier |
|-------|--------|-------|-----------|-----------------|-----------|
| Llama 3.3 70B | 70B | FP16 | 300-500 | $22.40-37.34 | self-hosted-frontier |
| DeepSeek V3 671B | 671B MoE | FP8 | 50-100 | $112.01-224.02 | self-hosted-frontier |
| Llama 3.1 405B | 405B | FP16 | 60-120 | $93.34-186.68 | self-hosted-frontier |

**Assumptions:** $450,000 hardware, 3-year amortization, 10.2kW TDP, $0.10/kWh, PUE 1.3, 50% utilization, 10% staffing.

#### Consumer/Workstation GPUs

| GPU | VRAM | Model | Quant | Est. tok/s | Est. $/M tokens |
|-----|------|-------|-------|-----------|-----------------|
| RTX 4090 (24GB) ~$1,600 | 24GB | Qwen 2.5 Coder 7B | Q8 | 50-80 | Not computable |
| RTX 4090 (24GB) | 24GB | CodeLlama 13B | Q4 | 30-50 | Not computable |
| RTX 6000 Ada (48GB) ~$6,800 | 48GB | Qwen 2.5 Coder 32B | Q4 | 25-40 | Not computable |
| RTX 6000 Blackwell (96GB) ~TBD | 96GB | Llama 3.3 70B | Q4 | 15-25 | Not computable |
| Mac Studio M4 Ultra (192GB) ~$7,000 | 192GB | Qwen 2.5 72B | Q4 | 8-15 | Not computable |

No power, electricity, PUE, utilization or staffing assumptions are stated for
these consumer/workstation rows, so their $/M costs cannot be recomputed. The
Spark embedding row likewise lacks token throughput. No cost estimate is given
for those rows.

---

## 3. Tiered Summary for RPT-2026

### 3.1 API Models by Effective Cost (Output-Weighted)

For coding workloads, output tokens dominate cost (typically 60-70% of spend). This ranking uses an approximate "effective cost" = 0.35 * input + 0.65 * output per MTok.

#### TIER 1 — Frontier ($10+/M effective)

| Model | Provider | Input $/M | Output $/M | Effective $/M |
|-------|----------|-----------|------------|--------------|
| gpt-5.4-pro | OpenAI | 30.00 | 180.00 | 127.50 |
| claude-opus-4 / 4.1 | Anthropic | 15.00 | 75.00 | 54.00 |
| o1 | OpenAI | 15.00 | 60.00 | 44.25 |
| claude-fable-5 | Anthropic | 10.00 | 50.00 | 36.00 |
| claude-opus-4.5 / 4.6 / 4.7 / 4.8 / 5 | Anthropic | 5.00 | 25.00 | 18.00 |
| grok-3-fast | xAI | 5.00 | 25.00 | 18.00 |
| gpt-5.4 | OpenAI | 2.50 | 15.00 | 10.63 |
| claude-sonnet-4/4.5/4.6 | Anthropic | 3.00 | 15.00 | 10.80 |
| grok-3 / grok-4 | xAI | 3.00 | 15.00 | 10.80 |

#### TIER 2 — Standard ($1-10/M effective)

| Model | Provider | Input $/M | Output $/M | Effective $/M |
|-------|----------|-----------|------------|--------------|
| gpt-5.3-codex | OpenAI | 1.75 | 14.00 | 9.71 |
| gemini-3.1-pro | Google | 2.00 | 12.00 | 8.50 |
| command-a | Cohere | 2.50 | 10.00 | 7.38 |
| gpt-4o | OpenAI | 2.50 | 10.00 | 7.38 |
| gemini-2.5-pro | Google | 1.25 | 10.00 | 6.94 |
| gpt-5 | OpenAI | 1.25 | 10.00 | 6.94 |
| gpt-5.1-codex | OpenAI | 1.25 | 10.00 | 6.94 |
| gpt-4.1 / o3 | OpenAI | 2.00 | 8.00 | 5.90 |
| gpt-5.2 | OpenAI | 0.875 | 7.00 | 4.86 |
| claude-haiku-4.5 | Anthropic | 1.00 | 5.00 | 3.60 |
| gpt-5.1 | OpenAI | 0.625 | 5.00 | 3.47 |
| gpt-5.4-mini | OpenAI | 0.75 | 4.50 | 3.19 |
| o4-mini | OpenAI | 1.10 | 4.40 | 3.25 |
| claude-haiku-3.5 | Anthropic | 0.80 | 4.00 | 2.88 |
| gemini-2.5-flash | Google | 0.30 | 2.50 | 1.73 |
| gpt-5.1-codex-mini | OpenAI | 0.25 | 2.00 | 1.39 |
| o3-mini | OpenAI | 0.55 | 2.20 | 1.62 |
| deepseek-r1 | DeepSeek | 0.55 | 2.19 | 1.62 |
| gemini-3-flash | Google | 0.50 | 3.00 | 2.13 |
| gpt-4.1-mini | OpenAI | 0.40 | 1.60 | 1.18 |
| grok-code-fast-1 | xAI | 0.20 | 1.50 | 1.05 |
| mistral-large-3 | Mistral | 0.50 | 1.50 | 1.15 |
| command-r | Cohere | 0.50 | 1.50 | 1.15 |
| gemini-3.1-flash-lite | Google | 0.25 | 1.50 | 1.06 |
| amazon-nova-pro | Amazon | 0.80 | 3.20 | 2.36 |

#### TIER 3 — Efficient (<$1/M effective)

| Model | Provider | Input $/M | Output $/M | Effective $/M |
|-------|----------|-----------|------------|--------------|
| gpt-5.4-nano | OpenAI | 0.20 | 1.25 | 0.88 |
| gpt-5-mini | OpenAI | 0.125 | 1.00 | 0.69 |
| codestral | Mistral | 0.30 | 0.90 | 0.69 |
| gpt-4o-mini | OpenAI | 0.15 | 0.60 | 0.44 |
| grok-3-mini | xAI | 0.30 | 0.50 | 0.43 |
| grok-4-fast / 4.1-fast | xAI | 0.20 | 0.50 | 0.40 |
| deepseek-v3 | DeepSeek | 0.28 | 0.42 | 0.37 |
| gemini-2.5-flash-lite | Google | 0.10 | 0.40 | 0.30 |
| gemini-2.0-flash | Google | 0.10 | 0.40 | 0.30 |
| devstral-small | Mistral | 0.07 | 0.28 | 0.21 |
| amazon-nova-lite | Amazon | 0.06 | 0.24 | 0.18 |
| gpt-5-nano | OpenAI | 0.05 | 0.40 | 0.28 |
| gpt-4.1-nano | OpenAI | 0.05 | 0.20 | 0.15 |
| mistral-small-3.2 | Mistral | 0.075 | 0.20 | 0.16 |
| command-r7b | Cohere | 0.0375 | 0.15 | 0.11 |
| gpt-oss-120b | OpenAI | 0.039 | 0.10 | 0.08 |
| gpt-oss-20b | OpenAI | 0.03 | 0.10 | 0.08 |

---

## 4. Corrections to Existing RPT-2026 in Cost Normalization Spec

The following entries in the current `docs/cost-normalization-spec.md` Section 2.3 need updating based on verified March 2026 pricing:

### Entries to Update

| Entry | Current Value | Correct Value | Reason |
|-------|--------------|---------------|--------|
| `gpt-4.5` | $75.00 / $150.00 | **Remove** | GPT-4.5 retired. Replaced by GPT-5.x series. |
| `gemini-2.0-ultra` | $10.00 / $30.00 | **Remove** | Never launched at this price. Gemini 3.1 Pro is the current Google flagship at $2.00/$12.00. |
| `gemini-2.5-flash` | $0.15 / $0.60 | **$0.30 / $2.50** | Verified from Google's official pricing page. |
| `deepseek-r1` input | $2.19 | **$0.55** | $2.19 is the OUTPUT price. Input is $0.55. Prices were swapped. |
| `deepseek-v3` | $0.27 / $1.10 | **$0.28 / $0.42** | DeepSeek V3.2 updated pricing. Significant output price decrease. |
| `gpt-4.1-nano` | $0.10 / $0.40 | **$0.05 / $0.20** | Verified from OpenAI pricing page and aggregators. |

### Entries to Add (Mid-Year Addenda)

| `model_family` | Tier | Input $/M | Output $/M | Notes |
|----------------|------|-----------|------------|-------|
| `claude-opus-4.5` | frontier | 5.00 | 25.00 | New Opus generation, major price reduction. |
| `claude-opus-4.6` | frontier | 5.00 | 25.00 | Opus generation. Fast mode available at $30/$150. |
| `claude-opus-4.7` | frontier | 5.00 | 25.00 | Opus generation, same $5/$25 rate. |
| `claude-opus-4.8` | frontier | 5.00 | 25.00 | Opus generation, same $5/$25 rate. |
| `claude-opus-5` | frontier | 5.00 | 25.00 | Latest Opus flagship, same $5/$25 rate (v9). |
| `claude-fable-5` | frontier | 10.00 | 50.00 | Most-capable model, distinct family above Opus tier. |
| `claude-sonnet-4.5` | standard | 3.00 | 15.00 | Updated Sonnet, 1M context beta. |
| `claude-sonnet-4.6` | standard | 3.00 | 15.00 | Latest Sonnet, full 1M context at standard rate. |
| `claude-haiku-4.5` | efficient | 1.00 | 5.00 | Updated Haiku. |
| `gpt-5` | standard | 1.25 | 10.00 | OpenAI GPT-5 base. |
| `gpt-5.1` | standard | 0.625 | 5.00 | OpenAI GPT-5.1. |
| `gpt-5.2` | standard | 0.875 | 7.00 | OpenAI GPT-5.2. |
| `gpt-5.4` | frontier | 2.50 | 15.00 | OpenAI latest flagship (Mar 2026). |
| `gpt-5.4-pro` | frontier | 30.00 | 180.00 | OpenAI ultra-premium. |
| `gpt-5.4-mini` | efficient | 0.75 | 4.50 | OpenAI latest mini. |
| `gpt-5.4-nano` | efficient | 0.20 | 1.25 | OpenAI latest nano. |
| `gpt-5.3-codex` | standard | 1.75 | 14.00 | OpenAI Codex agentic model. |
| `gpt-5.1-codex` | standard | 1.25 | 10.00 | OpenAI Codex mid-tier. |
| `gpt-5.1-codex-mini` | efficient | 0.25 | 2.00 | OpenAI cheapest Codex. |
| `gpt-5-mini` | efficient | 0.125 | 1.00 | OpenAI GPT-5 mini. |
| `gpt-5-nano` | efficient | 0.05 | 0.40 | OpenAI GPT-5 nano. |
| `o4-mini` | efficient | 1.10 | 4.40 | OpenAI reasoning mini. |
| `gpt-oss-120b` | efficient | 0.039 | 0.10 | OpenAI open-source 120B. |
| `gpt-oss-20b` | efficient | 0.03 | 0.10 | OpenAI open-source 20B. |
| `gemini-3.1-pro` | standard | 2.00 | 12.00 | Google latest flagship. |
| `gemini-3-flash` | efficient | 0.50 | 3.00 | Google Flash gen 3. |
| `gemini-3.1-flash-lite` | efficient | 0.25 | 1.50 | Google latest lite. |
| `gemini-2.5-flash-lite` | efficient | 0.10 | 0.40 | Google budget model. |
| `grok-3-fast` | frontier | 5.00 | 25.00 | xAI premium speed. |
| `grok-4` | standard | 3.00 | 15.00 | xAI latest. |
| `grok-4-fast` | efficient | 0.20 | 0.50 | xAI efficient. |
| `grok-4.1-fast` | efficient | 0.20 | 0.50 | xAI latest efficient. |
| `grok-code-fast-1` | efficient | 0.20 | 1.50 | xAI dedicated coding. |
| `deepseek-v3.2` | efficient | 0.28 | 0.42 | DeepSeek V3.2 update. |
| `mistral-large-3` | standard | 0.50 | 1.50 | Mistral flagship. |
| `codestral` | efficient | 0.30 | 0.90 | Mistral coding model. |
| `devstral-small` | efficient | 0.07 | 0.28 | Mistral coding agent. |
| `mistral-small-3.2` | efficient | 0.075 | 0.20 | Mistral small. |
| `command-a` | standard | 2.50 | 10.00 | Cohere flagship. |
| `command-r` | efficient | 0.50 | 1.50 | Cohere standard. |
| `command-r7b` | efficient | 0.0375 | 0.15 | Cohere ultra-budget. |
| `amazon-nova-pro` | efficient | 0.80 | 3.20 | AWS native model. |
| `amazon-nova-lite` | efficient | 0.06 | 0.24 | AWS budget model. |

---

## 5. Cross-Provider Defaults (Design — NOT Implemented)

> ⚠️ **This tiered-default scheme is design intent, not shipped behavior.** The
> code has no `frontier-default` / `standard-default` / `efficient-default` tier
> and does no provider/tier inference for unknown models. The shipped fallback
> (`ComputeCost`, using `internal/store/prices.yaml`) prices an unmatched model at a
> **self-hosted reference rate**: a size-class heuristic (`self-hosted-large`
> $2.00/M, `self-hosted-medium` $0.50/M, `self-hosted-small` $0.10/M) when the
> model string carries a parameter count, else the flat `self-hosted-medium`
> $0.50/M combined rate. Both paths emit a one-time `WARN` and bump the
> unknown-model event/cost counters (`tier_unknown_model_events_total` /
> `tier_unknown_model_cost_micro_total`, #267/#135). The rows below are retained
> as a target design; see `docs/cost-normalization-spec.md` (implementation-status
> banner) for the same clarification.

| Tier | Input $/M | Output $/M | When Used |
|------|-----------|------------|-----------|
| `frontier-default` | 5.00 | 25.00 | Unknown model from known frontier-class provider. Updated from $15/$75 to reflect new Opus 4.5+ and GPT-5.4 pricing reality. |
| `standard-default` | 2.00 | 10.00 | Unknown model, unknown tier. Updated from $3/$12 to reflect market compression. |
| `efficient-default` | 0.30 | 1.50 | Unknown model tagged as small/mini/nano. Updated from $0.40/$1.60. |

---

## 6. Key Market Observations (March 2026)

### Price Compression Is Accelerating

1. **Anthropic's Opus 4.5/4.6 dropped 67%** from Opus 4 ($15/$75 to $5/$25), making frontier models dramatically more accessible.
2. **OpenAI's GPT-5.x series** replaced GPT-4.5 ($75/$150) with models ranging from $0.05/$0.40 (nano) to $2.50/$15.00 (standard), a 10-50x cost reduction.
3. **DeepSeek V3.2** reduced output pricing from $1.10/M to $0.42/M -- a 62% drop.
4. **Google's Gemini 2.5 Flash-Lite** at $0.10/$0.40 and **Amazon Nova Lite** at $0.06/$0.24 are approaching commodity pricing.
5. **xAI's Grok Fast variants** at $0.20/$0.50 represent aggressive undercutting by a new entrant.

### Implications for TIER

- The frontier-default fallback of $15/$75 (based on Opus 4) is now overly conservative. Most "frontier" models are $3-5 input, $15-25 output.
- The efficient tier is being compressed toward near-zero. Models like GPT-OSS-120B at $0.039/$0.10 offer 120B-parameter performance at embedding-tier prices.
- Self-hosted reference tiers ($2.00/$0.50/$0.10) are fixed normalization rates, not actual infrastructure costs. Every computable estimate in §2.2 exceeds its tier's rate; the smallest gap is 7.9x ($3.95/M vs $0.50/M for Qwen 2.5 Coder 32B on DGX Spark). These estimates are not used in TIER calculations.
- The RPT-2027 TPI will likely show significant deflation (TPI < 0.60) given current trends.

### Coding-Specific Model Recommendations

For organizations choosing models specifically for coding tasks in the TIER framework:

| Use Case | Recommended API Model | Recommended Self-Hosted | Rationale |
|----------|----------------------|------------------------|-----------|
| Architecture review | Claude Opus 4.6, GPT-5.4 | Llama 3.3 70B FP16 | Highest reasoning quality needed. |
| Day-to-day coding | Claude Sonnet 4.6, o3, GPT-4.1 | Qwen 2.5 Coder 32B Q8 | Best cost/quality balance for code gen. |
| Code completion | Codestral, GPT-5.1-Codex-Mini | Qwen 2.5 Coder 7B | Fast, cheap, good enough for completions. |
| Test generation | Gemini 2.5 Flash, GPT-4o-mini | StarCoder2 15B | High volume, lower quality threshold. |
| Code review | Claude Sonnet 4.6, o3 | Qwen 2.5 72B Q4 | Needs strong reasoning about code quality. |
| Embedding/search | text-embedding-3-large | Qwen3-Embedding 8B | Semantic code search. |

---

## 7. Prompt Cache Multipliers

Cache multipliers are **per-model**, stored in `prices.yaml` (`cache_read_mult` / `cache_write_5m_mult` / `cache_write_1h_mult`) and baked into the effective rate at parse time (#122). Each model omits them to inherit its **provider default** (below) or sets an explicit value to override that class. They are multipliers **of the model's selected input rate**, so under a long-context over-tier a cached read scales off the premium base too. `ComputeCost` reads the baked value directly — there is no per-provider branch anymore.

Provider defaults (used when a model sets no explicit multiplier):

| Provider | Cache read | 5-minute cache write | 1-hour cache write |
|---|---|---|---|
| Anthropic | **0.1×** | **1.25×** | **2.0×** |
| OpenAI | **0.5×** | 1.0× (no write SKU) | 1.0× (no write SKU) |
| Z.ai (`zai`, v10) | 1.0× (every row sets its own) | 1.0× (no published write rate) | 1.0× (no published write rate) |
| Meta (`meta`, v11) | 1.0× (every row sets its own) | 1.0× (no published write rate) | 1.0× (no published write rate) |
| Google, xAI, DeepSeek | 1.0× | 1.0× | 1.0× |
| Self-hosted | combined rate, no discount | — | — |

Per-model overrides in the current table:

| Model | Field | Multiplier on selected input rate | Note |
|---|---|---|---|
| gemini-2.5-pro | `cache_read_mult` | **0.25×** | Gemini 2.5+ implicit caching — 75% discount on cached input. |
| gemini-2.5-flash | `cache_read_mult` | **0.25×** | " |
| gemini-3.1-pro | `cache_read_mult` | **0.25×** | " |
| deepseek-v3 | `cache_read_mult` | **0.1×** | Absolute $0.028/M cache-hit ÷ $0.28/M input. |
| deepseek-r1 | `cache_read_mult` | **0.0509×** | Absolute $0.028/M cache-hit ÷ $0.55/M input. |
| claude-fable-5-1, claude-mythos-5-1 | `cache_read_mult` | **0.025×** | $0.25/M ÷ $10/M. Writes inherit the Anthropic defaults. |
| claude-opus-5-5 | `cache_read_mult` | **0.05×** | $0.20/M ÷ $4/M. Writes inherit the Anthropic defaults. |
| gpt-6-astra, gpt-6-sol, gpt-6-luna | `cache_read_mult` / `cache_write_5m_mult` / `cache_write_1h_mult` | **0.1×** / **1.25×** / **1.25×** | OpenAI publishes one write rate, so both write classes carry it. No OpenAI capture path fills a write bucket today. |
| glm-5.3 | `cache_read_mult` | **0.18571428571428572×** | $0.26/M ÷ $1.40/M, the nearest float64. |
| glm-5.3-flash | `cache_read_mult` | **0.2×** | $0.03/M ÷ $0.15/M. |
| muse-spark-1.3, muse-spark-1.2, muse-spark-1.1 | `cache_read_mult` | **0.12×** | $0.15/M ÷ $1.25/M. |
| muse-spark-1.3-contributor, muse-spark-1.2-contributor | `cache_read_mult` | **0.02×** | $0.002/M ÷ $0.10/M. |

Gemini's `cachedContentTokenCount` is a subset of `promptTokenCount` (carved out of Input, same shape as OpenAI's `cached_tokens`); `thoughtsTokenCount` is reasoning usage billed at the output rate and folded into Output (#122). The GPT-5.x entries **are in the shipped table** (`prices.yaml`, added in v5 per #135 and present through the current v12) and each carries an explicit `cache_read_mult: 0.10` — the 5-era discount differs from the 4-era 0.5× default (which the `gpt-4.x` / `o`-series rows inherit).

Sources:
- Anthropic: https://platform.claude.com/docs/en/about-claude/pricing#prompt-caching (5m=1.25×, 1h=2.0×, hit=0.1×).
- OpenAI: cache-hit input is 50% off standard rate on caching-enabled models.
- Google Gemini: implicit caching on 2.5+ models discounts cached input tokens 75% (0.25×).
- DeepSeek: published cache-hit rate $0.028/M (https://api-docs.deepseek.com/quick_start/pricing).
- Z.ai: no per-token cache-write rate is published. The "limited-time free" line on its pricing page is cache **storage**, not writes, so `zai` writes default to 1.0× input, the conservative choice. No real Opencode row has reported a cache write yet (0 of ~9,267 measured), so this has no numeric effect today.
- Meta: no cache-write rate is published. Meta's prompt-caching page (https://dev.meta.ai/docs/prompt-caching) says "`cached_tokens` is a subset of your input tokens … billed at a reduced rate compared to uncached input" and describes no write step, so `meta` writes default to 1.0× input. Meta's token-counting page (https://dev.meta.ai/docs/token-counting) says `reasoning_tokens` is "a subset of the output total", so reasoning is already billed inside output and needs no rate of its own.

Legacy entries (rows / JSONL responses pre-dating the nested `cache_creation` object) bucket all cache writes into the 5m TTL — this matches Anthropic's pre-1h-feature default and is the safest historical assumption per the TIER cost-correctness decision matrix (issue #55).

The 1h TTL premium pays off as a steady-state caching strategy when the cache is read more than twice within the hour (2 × 0.1× < 1.0× fresh-read break-even); the 5m TTL pays off after a single read. Pricing decisions about cache TTL belong upstream (in the application calling Claude) — TIER reports what was actually charged.

---

## 8. Data Sources

Base rows verified March 26, 2026, with later revisions through `version: 12` (2026-10-02), from the following authoritative sources:

- [Anthropic Official Pricing](https://platform.claude.com/docs/en/about-claude/pricing)
- [OpenAI Official Pricing](https://openai.com/api/pricing/) and [Developer Docs](https://developers.openai.com/api/docs/pricing)
- [Google Gemini API Pricing](https://ai.google.dev/gemini-api/docs/pricing)
- [xAI Grok API](https://x.ai/api)
- [DeepSeek API Pricing](https://api-docs.deepseek.com/quick_start/pricing)
- [Mistral AI Pricing](https://mistral.ai/pricing)
- [Cohere Pricing](https://cohere.com/pricing)
- [Amazon Nova Pricing](https://aws.amazon.com/nova/pricing/)
- [OpenAI Codex Pricing](https://developers.openai.com/codex/pricing)
- [PricePerToken.com](https://pricepertoken.com) (third-party aggregator, cross-verified)
- [OpenRouter](https://openrouter.ai) (provider marketplace pricing)
- [Z.ai model pages](https://docs.z.ai/guides/overview/pricing) (GLM-5.3 / GLM-5.3 Flash, v10)
- [Meta Muse Spark pricing](https://dev.meta.ai/docs/pricing-rate-limits) (Muse Spark 1.1–1.3 and Contributor tier, v11; the page is undated, read 2026-09-27)

---

## 9. `version:` Is Binding — One Number, One Table (#714)

🔴 **This section describes a deliberate behaviour change. An override that was
accepted before #714 can now fail at startup.** Read it before editing any price
table, embedded or override.

### The rule

Every priced row in TIER carries a `price_version` column: the `version:` integer
of the price table that produced its cost. That stamp is written once, at insert,
and is never rewritten on the normal path — it is the only record of *which rates*
a historical cost was computed with.

But `version:` is only a **number**. It resolves to actual rates through a *file*
— `internal/store/prices.yaml`, or whatever `--prices` points at, which may not
be version-controlled at all. So if you change the table and do **not** bump
`version:`, one integer now denotes two different price tables, and the rows
stamped under each are **indistinguishable forever**. No later query, backup, or
audit can separate them.

⇒ **Changing the table and bumping `version:` are the same action. Do both or neither.**
"The table" is the *resolved* table, which `table_hash` covers: for each row, its
model key, input and output rates, over-threshold rates, `context_threshold`,
`combined`, `provider`, effective cache multipliers and effective `billing_mode`.
So adding or removing a row, or flipping a `billing_mode` or `provider`, needs a
new version exactly as a rate edit does. `effective_date`, comments and row order
are not part of it and do not need one.

A restart keeps its number whenever the resolved table is unchanged. The same
file and the same `tierd` build guarantee that; a comment edit or a new path does
not change it.
A row that leaves a cache multiplier out inherits its provider's default, which
lives in `tierd`'s code, not in the file, and the hash covers the resolved value.
An upgrade that changes one of those defaults therefore changes the hash of an
unchanged file: if a refusal names your table after an upgrade, bump its version.

### What enforces it

TIER records the content identity of every price table it serves **from a
database**, in a `price_table_registry` row: the version, the `table_hash` and
`file_hash` from [the identity stamp](api-compatibility.md#price-table-content-identity-713),
the effective date, the model count, where the table was loaded from, which binary
first served it, and `first_seen`. (A store-less command such as `tierd score-log`
prices and stamps without recording anything.) Two guards then refuse a second,
*different* table claiming a version already in use:

| guard | where | catches |
|---|---|---|
| **load-time** | every price-table load, including `tierd score-log`, which opens no database at all | an override reusing **this binary's embedded** `version:` with a different table |
| **open-time** | `tierd serve` / `reprice` / any command that opens the store, before anything is priced or stamped | a **different override file** at the same version; an override edited between two boots; **and an embedded table that changed under a version some earlier binary already served into this database** |

Only `table_hash` — the identity of the *resolved* table — is compared.
A comment-only edit moves `file_hash` and is **not** a collision; the same table
loaded from a second path is **not** a collision either. A hash recorded under an
older *canonicalization scheme* (the `tierpt1:` tag) is treated as **not
comparable** rather than as a collision, so a future fix to the serialization
cannot brick every deployment at once.

⚠️ **One case is caught by neither, deliberately.** Two *different* override files
sharing a non-embedded version, used only through a store-less command
(`tierd score-log --prices A`, then `--prices B`, both `version: 1000`), is
invisible to the load-time guard (which compares only against the embedded table)
and unreachable by the open-time one (there is no database). Closing it would
require per-user state outside any database — a worse problem than the one it
solves.

⛔ **There is no flag that accepts a collision, and there will not be one.** A
`--accept-rehash` would silently rebind the meaning of every historical
`price_version` stamp, with a green startup log — the exact corruption these
guards exist to prevent. The remedy is always: **bump the version.**

### What the refusal looks like

There are **two** refusals, and they read differently.

The **open-time** one names the database, both hashes, where the recorded one came
from, and the ways out:

```
database "/var/lib/tier/tier.db": price table version 9 in this binary hashes
table_hash "tierpt1:086f4116…" but this database already recorded version 9 as
table_hash "tierpt1:7acfb82b…" (recorded "2026-08-29T00:01:23Z" from source
"/etc/tier/prices.yaml" by "tierd v0.4.1 go1.26.6 linux/amd64") — two different
price tables share one version number, and every row stamped price_version=9
under each is indistinguishable forever. Bump 'version:' in the price table (this
is the fix in almost every case), point --db at a different file, or — only if the
RECORDED row is the wrong one — run 'tierd prices forget-version --db
"/var/lib/tier/tier.db" --version 9 --commit' to drop it. There is deliberately no
flag that accepts the collision
```

The **load-time** one needs no database, names the override path, and does *not*
offer `forget-version` — there is no recorded row to forget:

```
price table "/etc/tier/prices.yaml" declares version 9, which is this binary's
EMBEDDED price table version, but its table_hash "tierpt1:086f4116…" does not
match the embedded table_hash "tierpt1:7acfb82b…" — two different price tables
would share one version number, and every row stamped price_version=9 under each
is indistinguishable forever. Bump 'version:' in "/etc/tier/prices.yaml" to a
number the embedded table does not use (overrides conventionally start at 1000).
There is deliberately no flag that accepts the collision
```

⚠️ **If BOTH tables are the embedded default**, the open-time message says so and
tells you *not* to run `forget-version`: you cannot bump a version compiled into
the binary you were shipped, so that is a tierd release bug to report, not an
operator misconfiguration to work around.

🔴 **The failure mode is a total service outage, not degraded pricing.** A refused
`store.Open` takes down the daemon — dashboard, API and ingestion — because the
guard runs before anything is served. That is the deliberate choice (integrity
over availability), but size your change control for a price edit accordingly.

### If you write a `--prices` override

Use a `version:` of **1000 or more** that this database has never recorded.
Overrides start at `1000` so they stay clear of the embedded table's current
`version:`, which counts upward. When to bump is [the rule](#the-rule). Copying
the embedded table, changing it, and leaving the embedded table's
current `version:` in place is refused when the table loads.

An override that gives a built-in per-token model a different `billing_mode` (its own
row, or a `<model>@<host>` row that outranks it) is loaded as written, and `serve` (and
every other command that loads `--prices`) logs one startup WARN per such model and host,
naming the model, the host and both modes.

### Inspecting what is recorded

```sh
tierd prices list  --db /var/lib/tier/tier.db          # every recorded identity
tierd prices audit --db /var/lib/tier/tier.db          # every retirement
```

Both are read-only, both accept `--json`, and both work on a database
`tierd serve` is currently refusing to open — which is the point. Run them
*before* an upgrade to see which versions a deployment has already bound.

### The escape hatch

Sometimes the **recorded** row is the wrong one — it was seeded by a staging or
test table, or by a bad edit that has since been reverted. For that, and only
that:

```sh
# DRY RUN — prints the recorded row, changes nothing
tierd prices forget-version --db /var/lib/tier/tier.db --version 9

# archive it first if you want the record in machine-readable form
tierd prices forget-version --db /var/lib/tier/tier.db --version 9 --json > v9.json

# then retire it
tierd prices forget-version --db /var/lib/tier/tier.db --version 9 --commit
```

🔴 **It retires the identity; it does not delete it.** The row is **kept** and
stamped with `forgotten_at` and `forgotten_by`, and a `price_forget_audit` ledger
entry records the act — the same ledger family `tierd reprice` and
`tierd repair-repo` already write into. Afterwards the database can still answer:

> *version 9 once meant `table_hash tierpt1:7acfb82b…`, and that record was
> retired at 2026-08-29T01:21:42Z by `sre-oncall`.*

That is the whole reason it works this way. **This program exists so provenance
is not lost; an escape hatch that erased the record of its own use would
contradict the thing it protects.** A retired identity stops *binding the guard*,
so the next start records the replacement — it does not stop *being evidence*.
(Uniqueness on `version` is a **partial** index, `WHERE forgotten_at IS NULL`, so
retiring keeps the evidence without blocking re-registration. Both properties
depend on that one predicate.)

The row is always printed **before** anything is written, and if that output
cannot be written — a closed stdout, a full disk, a truncated pipe — the
retirement is **aborted**. You cannot lose the record silently.

`forgotten_by` defaults to the OS username and can be set with `--actor`. ⚠️ It
is **self-asserted**: a local CLI has no authenticated principal to bind it to,
so read it as a claim, corroborated by the `tool_version` the ledger records
alongside it. Same honesty as the `actor` field on `cost_correction_audit`.

⚠️ **It is still not a bypass.** It does not let two real tables share one
version — the remedy for that is always to bump the version. What it does is make
using the hatch leave a mark instead of a hole. Do not automate it in a unit file
or an entrypoint: running it on every boot converts fail-closed into fail-open.

### What it does **not** claim

`table_hash` is an **unkeyed** digest computed by the same process that reports
it. Against an adversary who controls the install it proves nothing — such an
adversary changes the table and reports whatever hash they like. It detects
*accidental* drift: an operator who edited the YAML on a box whose binary is
trusted, or two installs that believe they are running the same rates and are
not. See [`docs/api-compatibility.md`](api-compatibility.md#price-table-content-identity-713)
for the full statement of what the two hashes do and do not establish.

🔴 **A recorded row is authoritative only for rows written at or after its
`first_seen`.** It does **not** establish anything about rows that already carried
that version number beforehand. This matters most on the first upgrade of an
existing database: the same startup that records the active version's identity
also runs the one-shot backfill that stamps pre-#233 rows with that version, so
those rows inherit a version integer that now has a recorded hash while having
been priced by whatever table was in effect at the time. `tierd prices list`
prints `first_seen` and says so.

⚠️ **Nothing consumes the registry as a verifier today.** Its readers are the
guard itself and the three `tierd prices` operator commands (`list`, `audit`,
`forget-version`). The registry **records identity and refuses collisions** — it
does not certify that any particular stored row was priced by the table its
version names.

A `price_version` with **no** registry row is not an error and is not "invalid".
It means no identity-recording binary ever served that table here — most commonly
rows that pre-date this feature, or a figure produced by a store-less command like
`tierd score-log`, which prices and stamps without touching a database. TIER
deliberately does **not** mint a hash for a table nobody possesses.
