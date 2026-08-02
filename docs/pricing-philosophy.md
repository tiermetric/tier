# TIER Pricing Philosophy — What the Denominator Measures

> **Status:** Resolution memo, 2026-07. Formalizes the decision already shipped in
> #55 (see `docs/reference-price-table.md` §7) as written policy, and **supersedes**
> `docs/cost-normalization-spec.md` §2.5's "cached tokens priced at full input rate"
> paragraph. This is a documentation of an existing decision surfaced under the
> spec-contradiction rule, not a new choice. Flagged for maintainer review in the
> PR for #140 as overturnable.

## Why this memo exists

The TIER denominator is `total_effective_cost` — the dollar value of AI inference
consumed (`docs/cost-normalization-spec.md` §1). Two authorities disagreed on how that
dollar value is computed:

- **The shipped code** applies real cache-mechanics multipliers. `ComputeCost`
  (`internal/store/prices.go:624-629`) prices cache reads at `anthropicReadMult = 0.10`,
  5-minute writes at `1.25`, 1-hour writes at `2.00`, and OpenAI cache reads at
  `openAIReadMult = 0.50` (`prices.go:163-166`), scaled off the model's selected input
  rate.
- **`docs/cost-normalization-spec.md` §2.5** said the opposite: "Cached tokens are
  priced at the model's standard **input** rate, not the discounted cache-hit rate"
  because "cache efficiency is an infrastructure optimization, not a developer
  productivity signal."

Nobody adjudicated the conflict; each artifact claimed authority. This memo resolves it.

## 1. The rule

**TIER prices what the provider would charge at published list rates — "actual-charge
mechanics at RPT list prices."**

Token-class mechanics that change what a call *costs* — cache reads/writes
(`prices.go:624-627`), long-context over-tier re-pricing (`prices.go:610-622`, #4),
thinking tokens where surfaced (priced at the output rate) — are applied exactly as the
provider's public price sheet defines them. They live **in the denominator**.

Commercial arrangements that change what *you pay* for the same mechanics — enterprise
discounts, batch-window discounts, subscription flat fees — are **not** applied to the
denominator. They belong to `actual_spend` and surface through Spend Leverage (§5).

The dividing question is: *does this change the list price of the compute, or does it
change our procurement of that compute?* The former is mechanics; the latter is
procurement.

## 2. Mechanics vs. procurement

| Cost factor | Class | Where it lands | Authority |
|---|---|---|---|
| Cache read / write multipliers | mechanics | denominator | `prices.go:624-627`; RPT §7 |
| Long-context over-tier (>threshold re-price) | mechanics | denominator | `prices.go:610-622` (#4) |
| Thinking / reasoning tokens | mechanics (output rate) | denominator | cost-norm §2.5; `prices.go` output rate |
| Enterprise / negotiated discount | procurement | `actual_spend` → Spend Leverage | cost-norm §2.1 |
| **Batch API discount** | **procurement** | **`actual_spend` → Spend Leverage** | see below |
| Subscription flat fee | procurement | pending #113 / P2-09 ruling | cross-ref only — not decided here |

**Batch API is priced at list, and this is a rule, not an accident.** The batch
discount is a latency-tolerance discount on *identical* compute — same model, same
tokens, same capability consumed — so it is procurement, not mechanics. The pipeline
carries **no batch-token field** (grep `batch` across `internal/` yields zero pricing
hits), so batch traffic is already priced at list. That outcome is now the intended
rule, matching the original §2.5 batch rationale ("a billing discount for latency
tolerance ... does not reflect a different level of AI capability consumed"), not an
oversight to be closed.

## 3. What this supersedes

`docs/cost-normalization-spec.md` §2.5, "Cached input tokens":

> Cached tokens are priced at the model's standard **input** rate, not the discounted
> cache-hit rate. [...] Cache efficiency is an infrastructure optimization, not a
> developer productivity signal.

This paragraph is **superseded**. Cache multipliers are real list-price mechanics and
are applied per RPT §7. The §2.5 batch and thinking-token paragraphs stand (batch now
explicitly grounded as procurement; thinking tokens unchanged at the output rate). The
cost-normalization spec carries a dated `Superseded 2026-07` changelog line pointing
here rather than a silent rewrite.

## 4. Known consequence, accepted

Two orgs with identical developer behavior but different cache-hit rates get different
TIER scores. §2.5's original rationale said this must not happen — we accept it anyway,
deliberately:

- Cache hits are **real cost mechanics the developer's workflow controls** (loop
  structure, context reuse, prompt stability), not an opaque infrastructure setting.
- Refusing them would misstate spend by up to **10×** on cache-heavy agentic traffic
  (a 0.10× read billed at 1.0×), violating the "dollar value, not token count" mandate
  (`docs/cost-normalization-spec.md` §1).

**Cross-org comparability guidance:** read a team's cache-read share alongside its TIER
score — the per-request data is in `token_events` (cache_read / cache_write columns) —
so a low-TIER, high-cache-reuse team is not mistaken for an inefficient one.

## 5. Spend Leverage is counterfactual

Spend Leverage (`internal/scoring/engine.go:172,199`) is
`TotalCostUSD / ActualPaidUSD`: the list-price cost of observed usage over what finance
actually paid. On pay-as-you-go billing it sits near **1×** (you pay list); it climbs
only on flat or subsidized plans, so it is a diagnostic of your billing plan, not a
company-typical figure. It is routinely 20–30× on subsidized traffic, which invites a
misreading — that the org "saved" 30×.

> Spend Leverage compares what your usage would have cost at published list rates
> against what you actually paid. It is a *counterfactual* -- at list prices you would
> likely have used fewer tokens, so leverage above ~5x describes the value of your
> procurement, not a savings figure you could have banked by switching to list-rate
> billing.

Nobody would actually buy 20–30× the tokens at list; the number is a leverage
indicator, not a bankable saving. The dashboard tile itself is labeled plan-neutrally
(`internal/dashboard/`, "metered cost / paid spend", #453) so the figure is never
presented as output value or a bankable saving; this counterfactual caveat is documented
here and in the public docs rather than in a tile tooltip.

## 6. Quality-penalty scoping (resolved in #134 / P2-03)

Historical note, for readers of older review findings (§C9): the revert quality penalty
was **previously issue-wide**. `DB.UpdateQuality` degraded every outcome sharing a
`(developer, issue_id)` (`internal/store/store.go`, `WHERE developer = ? AND issue_id =
?`), so multiple PRs on one issue all took the revert penalty.

This **was fixed in #134 / P2-03**. The event-derived path now targets the specific
reverted / CI-failed outcome row: `DB.UpdateQualityForOutcome` writes `WHERE id = ?`
(`internal/store/store.go`), and the revert detector routes through it
(`internal/webhook/handler.go`). The issue-wide `UpdateQuality` is retained only for
back-compat callers and is documented as superseded for event-driven degradation. No
action remains here — this section records the resolution, it does not open work.
