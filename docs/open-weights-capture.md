# Capturing open-weights / self-hosted models

**Audience:** operators who run open-weights models (Llama, Qwen, DeepSeek, Mixtral)
through a hosted per-token API (OpenRouter, Together, Fireworks, Groq, DeepInfra) or a
self-hosted server (Ollama, vLLM) and want TIER to price that spend.

**Tone:** what it does today, what it doesn't. No aspiration.

---

## 1. There is no separate capture route — you retarget the OpenAI proxy

TIER captures open-weights traffic through the **existing** `/openai/` reverse proxy
(`cmd/tierd/main.go`), not a dedicated route. Any OpenAI-compatible endpoint works:
Ollama's `/v1`, vLLM's OpenAI server, a hosted gateway, or a per-token host's API.

Point `--openai-target` (the flag, or the `proxy.openai_target` config key; no
environment variable sets it) at the upstream, then send your OpenAI-SDK traffic
through `http://<tierd>/openai/` instead of the provider directly:

```sh
# Local Ollama
tierd serve --aggregation developer \
  --api-token @/run/secrets/tier-token \
  --openai-target http://localhost:11434/v1

# vLLM OpenAI server
tierd serve --aggregation developer \
  --openai-target http://vllm.internal:8000/v1

# A per-token host (host-qualified rates apply — see §4)
tierd serve --aggregation developer \
  --openai-target https://openrouter.ai/api/v1
```

`@/run/secrets/tier-token` is a file holding the API token you chose — see
[What you need before you start](../README.md#what-you-need-before-you-start).

Client change — swap the base URL and add the attribution headers:

```python
import os
from openai import OpenAI
client = OpenAI(
    base_url="http://localhost:8080/openai",      # tierd, not the provider; no /v1 here
    api_key=os.environ["PROVIDER_API_KEY"],      # your provider's key; tierd forwards it unchanged
    default_headers={
        "X-Tier-Token":     "<your --api-token value>",   # gate; required when auth is on
        "X-Tier-Developer": "alice",   # the same name your outcomes carry, or an alias you mapped
        "X-Tier-Issue":     "issue-42",                    # optional; attribution
        "X-Tier-Repo":      "acme/api",                    # optional; repo attribution
    },
)
```

Three things to get right:

- **The base URL ends at `/openai`.** tierd removes `/openai` from the request path
  and adds what is left to the end of the target. The target above already ends in
  `/v1`, and the SDK adds `/chat/completions`, so the provider receives
  `/v1/chat/completions`. A base URL of `.../openai/v1` would send
  `/v1/v1/chat/completions`: the provider answers 404 and nothing is captured. If
  your target has no path of its own (the default `https://api.openai.com`, for
  example), use `.../openai/v1` instead.
- **The provider key comes from your client, not from tierd.** The proxy adds no
  provider key of its own. It passes your `Authorization` header to the provider as
  it is, so `api_key` must be the real key that provider issued you (set it in
  `PROVIDER_API_KEY`, or whatever variable you use). `X-Tier-Token` is a separate
  key that lets you into tierd, and tierd never forwards it to the provider (see
  [security.md §7](security.md#7-proxy-header-hygiene)). For a local server that
  checks no key (Ollama, or vLLM started without `--api-key`), any placeholder
  string works.
- **Off your own machine, put TLS in front of tierd.** tierd serves plain HTTP,
  and every request through the proxy carries both your provider key and your
  `X-Tier-Token`. If clients reach tierd over a network rather than on
  `localhost`, send them through a TLS terminator (a reverse proxy such as nginx or
  Caddy) and point `base_url` at its `https://` address.

A request with no `X-Tier-Developer` is stored as `unattributed`, not dropped.

A dedicated capture route is **not** added: the OpenAI-compatible proxy *is* the route.
A separate route would need its own driving requirement first.

---

## 2. What the upstream response must contain

TIER prices from the token counts in the response body. The upstream must return an
OpenAI-compatible `usage` block:

```json
{
  "model": "llama-3.3-70b-versatile",
  "usage": { "prompt_tokens": 1234, "completion_tokens": 567 }
}
```

- `model` — the string the host echoes back. **This is host-specific** (see §4): Groq
  returns `llama-3.3-70b-versatile`, Together returns
  `meta-llama/Llama-3.3-70B-Instruct-Turbo`, OpenRouter returns
  `meta-llama/llama-3.3-70b-instruct`. TIER normalizes it (lowercase, date-suffix strip)
  but does **not** canonicalize across hosts.
- `usage.prompt_tokens` / `usage.completion_tokens` — required. A response with no usage
  block yields no token event (counted under the "uncaptured" metric, not priced at
  zero). A silently-absent usage block is the most common "why is my spend $0" cause —
  confirm yours emits one before trusting the numbers.

---

## 3. How the serving host is captured

The host is **not** read from the response — the response only carries `model`. It is
stamped once, at proxy construction, from the `--openai-target` URL's **hostname**
(`url.URL.Hostname()`, `internal/proxy/proxy.go`):

- the **port is dropped** — `openrouter.ai:443` and `openrouter.ai` are the same host,
  and every self-hosted `localhost:11434` / `localhost:8000` collapses to `localhost`;
- IPv6 brackets are stripped correctly;
- a target with no host leaves it empty, which stores as the `unknown` sentinel and
  prices at the model-only rate (exactly the pre-host behavior).

**Consequence for gateways (LiteLLM etc.):** the stamped host is whatever
`--openai-target` points at. Front five providers behind one LiteLLM gateway and every
event is stamped with the **gateway's** hostname, not the underlying provider — so the
per-host rates in §4 will **not** match, and those events price at the model-only /
size-class path instead. To get host-qualified rates, point `--openai-target` at the
provider directly (one target per host), or seed a rate row for your gateway host if it
bills flat.

---

## 4. How pricing resolves (exact host rate → model-only → size-class → fallback)

`ComputeCostHost` (`internal/store/prices.go`) resolves in this order:

1. **Host-qualified rate** — `NormalizeModel(model) + "@" + host`, e.g.
   `llama-3.3-70b-versatile@api.groq.com`. If `internal/store/prices.yaml` seeds a row
   for that exact `(model, host)` pair, its audited per-token rate is used and the event
   is priced **silently** (it is not a guess). These are the rows #268 adds.
2. **Model-only exact entry** — the pre-host path. Used when the host is unknown, or is a
   host with no seeded row for that model.
3. **Size-class heuristic** — a parameter count in the model string (`…70b…`, `…7b…`)
   maps to `self-hosted-large/medium/small`. A GUESS: it emits a one-time WARN and bumps
   the unknown-model cost counters (#267).
4. **Flat fallback** — `self-hosted-medium` ($0.50/M combined) when nothing else matches.
   Also a guessed, WARNed, counted estimate.

Only steps 1–2 are silent audited rates. Steps 3–4 are approximate — visible in the
`tier_unknown_model_*` metrics so you can see what share of spend is estimated.

### Seeded host-qualified rates

List prices in USD per million tokens, captured **2026-07-13**. Each row in
`prices.yaml` carries a provenance comment. Hosted open-weights pricing is the most
volatile market segment, so this set is deliberately small and backfilled on demand — if
a rate has drifted, override it with `tierd serve --prices /path/to/prices.yaml` (a full
copy with your corrections; the same flag exists on `score`, `ship` and `score-log`, and
flags go after the subcommand) rather than waiting on a release; a bad override fails
startup loudly.

The **host** column is the bare `--openai-target` hostname. The **model id** column is
the exact string each host echoes (what you key against, after normalization).

| Host (`--openai-target` hostname) | Model id (echoed) | Input $/M | Output $/M | Source |
|---|---|---|---|---|
| `openrouter.ai` | `meta-llama/llama-3.3-70b-instruct` | 0.10 | 0.32 | openrouter.ai model page + `/api/v1/models` |
| `openrouter.ai` | `meta-llama/llama-3.1-8b-instruct` | 0.02 | 0.03 | openrouter.ai model page + `/api/v1/models` |
| `openrouter.ai` | `qwen/qwen-2.5-72b-instruct` | 0.36 | 0.40 | openrouter.ai model page + `/api/v1/models` |
| `openrouter.ai` | `deepseek/deepseek-chat-v3-0324` † | 0.24 | 0.90 | openrouter.ai model page + `/api/v1/models` |
| `api.together.ai` | `meta-llama/Llama-3.3-70B-Instruct-Turbo` | 1.04 | 1.04 | together.ai/models/llama-3-3-70b |
| `api.fireworks.ai` | `accounts/fireworks/models/llama-v3p3-70b-instruct` | 0.90 | 0.90 | docs.fireworks.ai/serverless/pricing (dense >16B) |
| `api.fireworks.ai` | `accounts/fireworks/models/llama-v3p1-8b-instruct` | 0.20 | 0.20 | docs.fireworks.ai/serverless/pricing (dense 4–16B) |
| `api.fireworks.ai` | `accounts/fireworks/models/qwen2p5-72b-instruct` | 0.90 | 0.90 | docs.fireworks.ai/serverless/pricing (dense >16B) |
| `api.fireworks.ai` | `accounts/fireworks/models/mixtral-8x7b-instruct` | 0.50 | 0.50 | docs.fireworks.ai/serverless/pricing (MoE ≤56B) |
| `api.groq.com` | `llama-3.3-70b-versatile` | 0.59 | 0.79 | groq.com/pricing |
| `api.groq.com` | `llama-3.1-8b-instant` | 0.05 | 0.08 | groq.com/pricing |
| `api.groq.com` | `qwen/qwen3-32b` | 0.29 | 0.59 | groq.com/pricing |
| `api.deepinfra.com` | `meta-llama/Llama-3.3-70B-Instruct-Turbo` | 0.10 | 0.32 | deepinfra.com model page + `/models/list` |
| `api.deepinfra.com` | `meta-llama/Meta-Llama-3.1-8B-Instruct` | 0.02 | 0.05 | deepinfra.com model page + `/models/list` |
| `api.deepinfra.com` | `Qwen/Qwen2.5-72B-Instruct` | 0.36 | 0.40 | deepinfra.com model page + `/models/list` |
| `api.deepinfra.com` | `deepseek-ai/DeepSeek-V3` | 0.32 | 0.89 | deepinfra.com model page + `/models/list` |
| `api.deepinfra.com` | `mistralai/Mixtral-8x7B-Instruct-v0.1` | 0.54 | 0.54 | api.deepinfra.com/models/list |

† `deepseek/deepseek-chat-v3-0324` normalizes to `deepseek/deepseek-chat-v3` —
`NormalizeModel` strips the trailing 4-digit `-0324` — so the `prices.yaml` key is
`deepseek/deepseek-chat-v3@openrouter.ai`. The moving `deepseek/deepseek-chat` alias is
intentionally not keyed.

**Notes on host coverage.** Fireworks prices these older open-weights models by its
published parameter-size *tier*, not a per-model line item (input = output within a
tier); that tier table is its authoritative mechanism, so the rows above are exact.
Groq does not serve DeepSeek V3 or Mixtral 8x7B (deprecated) and its nearest Qwen is
`qwen/qwen3-32b`, not Qwen 2.5 72B — those are absent by design, not omission.

**Deliberately deferred (unverified — do not assume the default rate applies):**

- **Together** Llama 3.1 8B / Qwen 2.5 72B / DeepSeek V3 / Mixtral — prices are
  first-party but the echoed **id strings** were not confirmed on a live page; confirm
  via an authenticated `GET https://api.together.ai/v1/models` before seeding.
- **OpenRouter / Groq** Mixtral 8x7B — deprecated / no longer served at a published
  price.
- **Fireworks / DeepInfra** Qwen3 235B and **Fireworks** DeepSeek V3 — per-model price
  unverified or conflicting across sources.

Until re-verified against a live first-party page, these route through the model-only /
size-class path (a WARNed, counted estimate), not a seeded rate.

**To add a host/model:** key the row `<host's echoed model id, normalized>@<api-hostname>`,
set `input_per_m` / `output_per_m` to the published list rate, set
`billing_mode: per_token`, and add a source comment. Verify the exact model-id string
against a real response from that host — a mismatched key silently misses and falls to
the size-class path.

---

## 5. Subscription / flat-rate hosts (#113) — configure them yourself

Some hosts do not meter per token. Ollama Cloud and GLM bill a flat monthly
subscription, where the marginal cost of one more token is effectively zero. That
breaks the arithmetic every other price row assumes, so TIER splits the money in two
and gives each artifact exactly one truth.

### The two artifacts

| artifact | what it says | where it lands |
|---|---|---|
| **price table** (`prices.yaml` / `--prices`) | this route is subscription-billed, and its tokens are worth *this* comparable list rate | the TIER **denominator**, per call, at ingest |
| **config** (`subscriptions:`) | and here is the **fee** we actually pay for it | the **actual-paid** side of Spend Leverage, only |

The maintainer's ruling of 2026-07-02 (#113) chose this **list-rate inversion** over amortizing
the fee across the window's tokens. The amortized form is non-local: divide a flat fee
by the window's token count and the same session costs different dollars depending on
how much a *colleague* used the route, and on which `?since=` you asked for. The
inversion keeps every cost a local property of the call, which is what every other cost
in TIER already is.

The fee is *never* in the denominator, and the comparable rate is *never* claimed as a
paid price. `billing_mode: subscription` rides with the row through `/events` (#304, now
landed) so a consumer can always tell a valuation from a metered rate.

### Price-table half

```yaml
# in your --prices override
models:
  "glm-5.2@ollama.com":
    input_per_m: 0.875      # a comparable PEER model's published list rate
    output_per_m: 7.00      # — see the honesty note below
    provider: self-hosted
    billing_mode: subscription
```

The comparable rates are ordinary, required, positive rates: the parser's non-positive-rate
guard applies to a subscription row exactly as it does to any other, so a rate-less
subscription entry is a **startup error**, never a silent $0.

**🔴 No subscription row ships in the embedded default table, and this is deliberate.**
A comparable rate is a judgement — "GLM-5.2 is worth about what gpt-5.2 costs" — not a
figure any vendor publishes. Shipping one would state a $/M price for a real vendor, to
every TIER install, that nobody could verify against a first-party page. That is the same
bar §4 sets for per-token hosts ("verify the exact model-id string against a real
response from that host"), applied to a number that *has* no first-party source. So you
declare it, against a peer you can name, in your own override.

### Config half

```yaml
subscriptions:
  - route_prefix: "glm-5.2@ollama.com"  # a prefix of the PRICE-TABLE KEY, lowercase
    plan: "max"                          # informational; appears in logs
    org: "acme"                          # a name you choose for your organisation; use the same one in org_hierarchy and org_actual_spend
    monthly_fee_usd: 100.00              # the real flat fee, per calendar month
    active_since: "2026-06"              # optional; see "missed periods" below
```

`tierd serve` **refuses to start** when a configured `route_prefix` matches no
`billing_mode: subscription` entry in the active table. A fee posted for a route TIER
prices as an ordinary metered model would add real dollars to actual-paid with no
matching list value, quietly deflating Spend Leverage — a wrong number on a CFO-facing
metric, produced by a typo.

The reverse — a subscription row in the table with no fee configured — is a **WARN**, not
a refusal: leverage reads high (the fee is missing), but nothing is fabricated, and
`tierd score`, `tierd demo`, and read-only deployments legitimately run such a table with
no `subscriptions:` block at all.

### Missed billing periods (#155)

The reconciler posts each fee into `org_actual_spend` under the source
`subscription:<route_prefix>`. That source scoping is what makes it idempotent *and*
what keeps it from ever touching another feed's rows — a manual finance invoice, an
Anthropic/OpenAI poller delta, or a second subscription plan under the same org are
neither netted against nor offset. There is no separate ledger table; the `source`
column is the ledger.

Without `active_since` only the **current** period is reconciled — correct for a server
that never stops, and a hole for every month tierd was down. Set `active_since` to the
month the plan started and the **startup** pass fills in every month that has no
posting yet, logging one line per month actually posted. The hourly tick still touches
the current period only; the catch-up is a one-shot, not an hourly re-walk.

**The two periods are treated differently, on purpose.** The **current** month
*converges* to the configured fee, so a mid-month price change posts the difference. A
**closed** month is only ever *filled in at the configured fee* — never restated. Once
a month has a posting, raising `monthly_fee_usd` in August leaves June alone, so a
config edit cannot silently move every historical Spend Leverage figure.

> ⚠️ **A month that has NO posting is filled in at TODAY'S fee.** Config carries one
> fee and no per-period history, so first-run backfill cannot know what the plan cost
> back then. If your plan cost $100 until June and $200 now, setting
> `active_since: "2026-01"` posts **$200 for every one of those months** and understates
> Jan–Jun Spend Leverage by roughly 2×. tierd logs a **WARN** naming each month it
> backfills, precisely so this is visible; correct any affected month with
> `POST /api/v1/org_actual_spend`. Months that already have a posting are untouched.

> 🔴 **`route_prefix` and `org` are the posting identity.** A re-run knows a month is
> already covered by looking for rows under `subscription:<route_prefix>` for that org.
> Rename either and the next startup finds nothing under the new key and posts the whole
> history again, while the old rows remain — the org total then double-counts. If you
> rename one, correct or delete the old `org_actual_spend` rows in the same change.

### Metering caveat

Ollama Cloud **streaming** responses carry no token-usage stats (ollama#15169), so only
non-streamed calls meter reliably through the proxy. A streamed call is not
under-priced — it is not captured at all.

### Z.ai GLM-5.3 on the coding plan: no override needed

GLM-5.3 is **not** a subscription route in TIER. The embedded price table prices it
per token at Z.ai's published list price, including when it runs on the coding plan
(#786): its model-only `glm-5.3` row (`provider: zai`) prices every `glm-5.3` event,
whatever host the capture source stamps. Unlike the `glm-5.2@ollama.com` sketch above,
these rates are **not** a comparable-peer judgement: Z.ai publishes them. TIER ships
no `glm-5.2` row.

**Rates**: [docs.z.ai/guides/overview/pricing](https://docs.z.ai/guides/overview/pricing),
as of price table v10 (#786), read 2026-09-25.

| | input | cached input | output |
|---|---|---|---|
| GLM-5.3 | **$1.40 /M** | **$0.26 /M** | **$4.40 /M** |

🔴 **`cache_read_mult` is the whole number.** `provider: zai` defaults the cache-read
multiplier to **1.0×**, which bills a cached read at the full input rate. In a 32-day
measurement of this route, taken 2026-08-28, cache reads were
**241,709,760 of 270,777,337 tokens (89.3%)**. That mix prices at **$119.15** with the
row's `cache_read_mult: 0.18571428571428572` and at **$394.70** without it, **3.31×**
overstated. The value is the correctly-rounded `float64` quotient of `$0.26 ÷ $1.40`,
which multiplied by $1.40 lands bitwise on `0.26`; a tidier `0.186` prices at
$0.260400/M. `internal/store/prices_glm53_test.go` asserts the resulting **rate** (a 1M
cache-read event costs exactly 260,000 micro-dollars) and reintroduces `0.186` to prove
the assertion catches it.

⚠️ **Z.ai publishes no cache-write *token* rate.** Its pricing page lists a
`Cached Input Storage` SKU ("Limited-time Free"), which prices keeping a cache alive and
has no class in `CostUsage`. So the row sets no cache-write multiplier and both write
classes inherit the `zai` default of **1.0×**, the full input rate (§7 of
[reference-price-table.md](reference-price-table.md)). A free write class cannot be
expressed today: `cache_write_5m_mult: 0` means "inherit the provider default", not zero.

⚠️ **Whether Z.ai bills reasoning tokens at the output rate is UNVERIFIED.** `CostUsage`
has no reasoning class, so the producer decides by where it puts them. On the mix above,
folding them into output (what the Opencode collector does) and leaving them out differ
by **$19.43, 16% of the total**.

⛔ **Do not add a `glm-5.3@zai-coding-plan` row to a `--prices` override.** A
host-qualified row wins over the model-only one, so it would replace the embedded
per-token rate for this route. If an override does it anyway and that row sets a
`billing_mode` other than `per_token`, `serve` (and every other command that loads
`--prices`) logs a startup WARN naming the model, the host and both billing modes; it
still loads the override.

✅ **Any `--prices` override needs a fresh `version:`, and that IS enforced (#714).** An
override that reuses the embedded table's `version:` with a different table is refused — at load time by any
command that reads a price table (`tierd score-log` included), and at startup by any
command that opens the database, which also refuses a *second, different* override
reusing a version this database has already recorded. There is deliberately no flag
that accepts a collision; the remedy is always to bump the version. See
[§9 of the reference price table](reference-price-table.md#9-version-is-binding--one-number-one-table-714)
for the full rule, the refusal message, and the narrow escape hatch.

---

## 6. Verifying the walk-through against a local Ollama

(`secret` below is a throwaway literal for this loopback-only walk-through; a real
deployment uses `@file`.)

```sh
ollama serve &                     # :11434
ollama pull llama3.1:8b
tierd serve --aggregation developer --api-token secret \
  --openai-target http://localhost:11434/v1 &

curl -s http://localhost:8080/openai/chat/completions \
  -H "X-Tier-Token: secret" -H "X-Tier-Developer: alice" -H "X-Tier-Issue: issue-1" \
  -H "Content-Type: application/json" \
  -d '{"model":"llama3.1:8b","messages":[{"role":"user","content":"hi"}]}' | jq .usage
```

Then confirm the event landed:

```sh
curl -s http://localhost:8080/api/v1/events -H "Authorization: Bearer secret" | tail
```

`localhost` has no seeded host row, so `llama3.1:8b` prices via the size-class heuristic
(`8b` → `self-hosted-medium`) with a one-time WARN in tierd's log — expected, and visible
in `tier_unknown_model_events_total`. Point `--openai-target` at a seeded per-token host
(§4) to see step-1 silent pricing instead.
