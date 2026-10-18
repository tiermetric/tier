# The canonical TIER rubric (v1 and opt-in v2)

This document is the **versioned, normative rubric** for TIER's outcome weights
and work-type taxonomy. It is the weight-side analogue of the
[reference price table](./reference-price-table.md): the price table pins the
**dollars**, this rubric pins the **weighted points**, and a TIER number is only
comparable across two measurements when **both** stamps match.

- **Rubric version:** disabled Event 3 policy uses `1` (the legacy `scoring.RubricVersion` constant); enabled, fully resolved stored policy uses `2`.
- **Where it is stamped:** every `GET /api/v1/scores` response carries a top-level
  `rubric: {version}` block next to `price_table: {version, effective_date}`.
- **When it must change:** any edit to the weight scale or the work-type taxonomy
  below **must** bump `scoring.RubricVersion`. That is the point of the stamp, not
  a side effect — a silent edit would make yesterday's scores falsely comparable
  to today's.

> **There is no absolute "good" TIER.** This rubric deliberately defines **no**
> reference cohort and **no** good/ok/poor band. A weighted point is org-local by
> construction; a bare TIER of 40 is neither good nor bad on its own. What *is*
> defensible is a **self-relative** comparison — the same org over time, subject to matched measurement definitions — expressed through
> `cost_per_point`. See [What you may and may not compare](#what-you-may-and-may-not-compare).

## Why the rubric is versioned (the exploit it closes)

With Event 3 scoring disabled, TIER's numerator is `Σ(weight × quality)`. Enabled scoring uses the allocated points in [rubric 2](#rubric-2-corrective-work-operator-opt-in). The weight comes from a size label (or
a heuristic fallback — see [conventions.md](./conventions.md)). If the label→weight
mapping is merely a "convention", two orgs can apply it with different generosity:
a generous-labeling org calls the same change `size/l` (5) that a strict org calls
`size/m` (3). At **identical real efficiency**, the generous org posts a higher
TIER. Without a stamped rubric, a cross-org comparison silently rewards labeling
culture, not engineering yield — and TIER drifts into people-ranking, the one
thing it must not do.

Two distinct mechanisms address this — do not conflate them:

- **Normativity** (this document's worked examples) provides common definitions.
  It does not establish that two organisations calibrate labels equally, and
  TIER neither validates nor enforces inter-rater equivalence. Two organisations
  do not compare, even when their version stamps match.
- **The version stamp** (`rubric.version` on the wire) is *provenance*: it makes a
  change to the rubric **definition** detectable over time, so a self-over-time or
  matched-version comparison can confirm the weighting did not drift. It does
  **not**, by itself, detect a cross-org generosity difference — two orgs on the
  same binary stamp the same version however each labels — nor a local
  `size_labels` remap (see the caveat under [Size → weight](#size--weight)).

A `rubric.version` mismatch identifies a definition change. Matching versions do not establish shared calibration or comparable capture and task mix; a cross-organisation `cost_per_point` reading remains unsupported.

## Size → weight

The weight of a merged PR is its size, read from a GitHub label (case-insensitive,
`size/` prefix optional). The scale is fixed — these five values and no others:

| Label            | Weight | What it means (canonical calibration) |
|------------------|--------|----------------------------------------|
| `size/xs` / `xs` | 0.5    | Trivial: a one-line fix, a typo, a config flag, a dependency bump. |
| `size/s`  / `s`  | 1      | Small: a localized change to one function/file; a focused bug fix with a test. |
| `size/m`  / `m`  | 3      | Medium: a self-contained feature or fix spanning a few files; the everyday unit of work. |
| `size/l`  / `l`  | 5      | Large: a feature touching several components, or a non-trivial refactor with migration. |
| `size/xl` / `xl` | 8      | Extra-large: a subsystem, a cross-cutting change, or a multi-day effort landed as one PR. |

With no recognized label, TIER falls back to the lines/files heuristic (see
[conventions.md](./conventions.md#pr-size-labels--outcome-weight)); explicit labels
are preferred because they let a reviewer correct the heuristic (a two-line change
to a critical path is not a two-line typo fix).

> **What `rubric.version` does and does not assert.** The stamp names the
> **canonical** rubric compiled into the binary — this scale and taxonomy. It is
> **not** a fingerprint of an operator's local configuration. The `weights`
> themselves are locked to the `{0.5, 1, 3, 5, 8}` scale (`#244`, enforced at
> startup), but an operator MAY rename the labels that map onto them via
> `outcomes.size_labels` (e.g. map a house label `epic` to `8`). Renaming which
> label triggers a weight does not change the meaning of a weight, so the stamp
> stays honest for that case. A local convention that maps a size to a
> *different* weight than this document's calibration (e.g. treating everyday
> work as `l`/5 rather than `m`/3) is a **divergence the operator owns**: the
> stamp cannot detect it, so such an org must not read its `cost_per_point`
> against another party's. A config-fingerprint stamp is a possible future
> refinement; cross-organisation comparison remains unsupported even with
> matched versions and reconciled labels.

### Worked examples (per size)

Assume a clean outcome (`quality = 1.0`; see [Quality multiplier](#quality-multiplier)).

1. **A `size/s` (weight 1) bug fix that cost $2.00 of AI compute.**
   - `weighted_points = 1 × 1.0 = 1`
   - `cost_per_point = 2.00 / 1 = $2.00 per point`
   - `tier = 1 / (2.00/1000) = 500`
2. **A `size/l` (weight 5) feature that cost $12.00.**
   - `weighted_points = 5 × 1.0 = 5`
   - `cost_per_point = 12.00 / 5 = $2.40 per point`
   - `tier = 5 / (12.00/1000) ≈ 417`
3. **A `size/xs` (weight 0.5) typo fix that cost $0.40.**
   - `weighted_points = 0.5`
   - `cost_per_point = 0.40 / 0.5 = $0.80 per point`
   - `tier = 0.5 / (0.40/1000) = 1250`

Note example 3's TIER of 1250 — a tiny outcome against tiny cost produces an
enormous ratio. This is why one raw TIER value can't be read on its own, and
why the [evidence floor](./api-compatibility.md) (`#133`: at least 3 outcomes and
at least $5 of cost) exists. `cost_per_point` is the steadier unit: across the three examples it
stays in the $0.80–$2.40 band even as TIER swings from 417 to 1250.

## Work-type taxonomy

Outcomes are partitioned by `work_type` so a category is only ever compared to the
same category (a security outcome against other security work, never against
feature work). The canonical taxonomy is these seven categories, in order:

| `work_type`  | Scope |
|--------------|-------|
| `feature`    | New user-facing or internal capability. The default when unlabeled. |
| `bug`        | Correcting incorrect behavior in shipped code. |
| `security`   | Vulnerability remediation, hardening, auth/crypto work. |
| `incident`   | Production incident response and remediation. |
| `tech-debt`  | Refactoring, cleanup, dependency and platform maintenance. |
| `research`   | Spikes, investigations, prototypes — exploratory work. |
| `compliance` | Regulatory, audit, privacy, and policy-driven work. |

The same size→weight scale applies **within** every work type; the taxonomy
segments *what is compared to what*, it does not change how a point is earned.

### Worked example (per work_type)

A developer merges, in one window: a `size/m` `feature` ($9 cost), a `size/s`
`bug` ($1.50), and a `size/l` `security` fix ($20).

| Segment    | weight | cost   | cost_per_point |
|------------|--------|--------|----------------|
| `feature`  | 3      | $9.00  | $3.00 / point  |
| `bug`      | 1      | $1.50  | $1.50 / point  |
| `security` | 5      | $20.00 | $4.00 / point  |

Each `cost_per_point` is meaningful **only against the same segment** in another
window. A pooled
`cost_per_point` across the three is **not** a supported comparison — the
`/scores` `work_types` array is the authoritative surface, and the pooled
top-level rows are a back-compat summary, not a cross-type ranking (`#187`).

## Quality multiplier

Weight is scaled by a quality multiplier derived from quality events (`#134`),
so `weighted_points = Σ(weight × quality)`:

| Quality floor | Multiplier |
|---------------|------------|
| Clean         | 1.0        |
| CI failure    | 0.7        |
| Strategic revert | 0.8     |
| Quality revert   | 0.1     |

The quality floors are part of the rubric: changing them also bumps
`rubric.version`.

## `cost_per_point`: the benchmarking unit

`cost_per_point = total_cost_usd / weighted_points` — USD per weighted outcome
point. It is the **inverse-unit dual** of TIER (numerically `1000 / tier` when
both are defined) and is the unit TIER should be *benchmarked and trended* on:

- It reads as money a CFO already understands ("$2.40 per weighted point").
- Constant-price self-comparison requires matching resolved price fingerprints,
  the original pricing basis of stored rows (or an explicit common reprice),
  rubric, release and population. A loaded `price_table.version` alone does not
  establish that older rows used the same rates; see
  [technical reference §2.1](technical-reference.md#21-the-formula).
- It is guarded on the points denominator: a zero-point row reports `null`
  (rendered `—`), never a divide-by-zero and never a `0` that would read as
  the most efficient row (`#472`). (A `$0`-cost row with points reports a
  legitimate `0.0`, already surfaced by the zero-token tripwire `#136`.)

### Self-relative interval (not an absolute rank)

In v0.6.1, #1233 withholds intervals when the row's own attributed-cost share
is below 50%; exactly 50% passes. When shown they are labelled "excludes
uncertainty from unattributed spend". Team and division rows carry none.

`/scores` also returns `cost_per_point_ci_low/high`: the nominal 95% percentile-bootstrap bounds (coverage unvalidated)
for a ranked developer's `cost_per_point`, derived by reciprocal
transform from the same TIER bootstrap (`#133`) — it expresses `cost_per_point`
against **that developer's own resampled outcome history**, the honest thing a
single window supports. It is **not** a percentile rank against other developers
or other orgs. TIER does not publish a cross-org distribution or percentile.
Compare a team with its own past.

## What you may and may not compare

| Comparison | Allowed? | Unit | Condition |
|------------|----------|------|-----------|
| Same developer/team over time | **Conditional** | `cost_per_point` | Match resolved price fingerprints, historical row pricing or an explicit common reprice, rubric, release, population and capture. Matching loaded version numbers alone is insufficient. |
| Same developer/team, within one `work_type`, over time | **Conditional** | `cost_per_point` (per segment) | As above; compare a segment only to the same segment. |
| Two organisations | **No cross-organisation comparison** | `cost_per_point` | Matching stamps do not establish shared calibration, capture or task mix. Within one organisation, a comparison remains a question to investigate under the technical reference §2.1 conditions. |
| Two parties, mismatched `rubric.version` or `price_table.version` | **No** | — | Non-comparable by construction. This is the guard, not a limitation. |
| Raw `tier` across parties | **No cross-organisation comparison** | — | Neither `tier` nor `cost_per_point` makes differently calibrated organisations comparable; matched stamps do not establish equivalence. |
| Cross-type (`feature` vs `security`) | **No** | — | A category error (`#187`). Compare within a `work_type` only. |
| Against an absolute "good" value | **No** | — | No such value exists. TIER has no reference cohort baked in, by design. |

## Related

- [conventions.md](./conventions.md) — how labels and issue references are read
  (the operational input to this rubric).
- [reference-price-table.md](./reference-price-table.md) — the dollar side of the
  pair; its `version` co-stamps every comparison.
- [pricing-philosophy.md](./pricing-philosophy.md) — cache-share context (§4),
  required when interpreting `cost_per_point` over time.
- [api-compatibility.md](./api-compatibility.md) — the `/scores` wire contract for
  `cost_per_point`, its CI, and the `rubric` stamp.

Nominal 95% bounds. Coverage is unvalidated: in constructed tests where several outcomes share one issue, they covered the true value less often than 95%. They are descriptive resampling bounds and exclude uncertainty from unattributed spend, incorrect attribution and uncaptured usage. Inference is not established.


## Rubric 2: corrective work (operator opt-in)

The size scale and work-type taxonomy above are unchanged. The default-off
`outcomes.event3_scoring` switch selects the correction rule and rubric together
from persisted state. One transaction resolves every outcome's complete chain
and pins rubric 2; unresolved enabled state refuses scores and new seals.
Disabling restores legacy scoring and rubric 1. Lifecycle confirmation alone
never enables scoring.

Rubric 2 retains the quality floors above and adds this step when an enabled
correction occurs within the inclusive 14-day window after its parent merged:

| Parent's own quality before correction | Corrected quality |
|---|---|
| Clean: 1 | 17/20 (0.85) |
| CI failure: 0.7 | 11/20 (0.55) |
| Other quality values | Unchanged |

This step applies once to the parent's own quality, including when the parent
is itself a follow-up. An unconfirmed cross-author declaration does not enable it.

Under rubric 2, an original receives
`max(0.1 * W_original, q_original_corrected * W_original + sum(credit) - sum(revised_weight))`.
Here `q_original_corrected` is the original's own quality after the correction step above; it never reads a follow-up's quality.
A follow-up receives its allocated share multiplied by **its own** quality, never
its parent's quality. Repairing a repair runs the allocation on the parent's
share and replaces it. Stored rational inputs use exact `math/big.Rat` values in
lowest terms. Raw activity (`outcome_n`) remains separate from eligible growth
(`sample_n`); repair-only activity cannot inflate the ranking evidence floor.
Same-author declarations are provisional until operator review. Cross-author
credit requires confirmation; multi-root foreign relations remain pending. An
own-root multi-root declaration takes the least-favourable own-root result.
Disputes and withdrawals after confirmation retain credit until operator
settlement. Team/division diagnostics are `not_shown`.

Rubrics 1 and 2 each verify under their own rule; this change does not move the
weight scale. Unknown rubric versions remain a changed-definition signal.
Sealed comparisons reject differing rubric versions, and existing seals retain
their original bytes and digest. A shared rubric still does not establish
comparable calibration, capture completeness, or task mix.

See [Enable Event 3 scoring](technical-reference.md#enable-event-3-scoring) for configuration, rollback and startup behavior.

### Reviewed allocation and review limits

Reviewed cumulative scope before and after the correction determines marginal
growth; an explicit pure repair has zero marginal growth. This differs from the
follow-up's reviewed own size and its individual allocated share. The original
is charged first, retaining its floor; the remaining budget goes to follow-ups
in canonical order, capped at their reviewed own sizes. Their shares then take
their own quality. For a clean original of weight 5, corrected quality 0.85 and a
clean pure repair of size 1, the original receives 3.25 points and the fixer
receives 1: total 4.25, with zero added growth and no repair-only SampleN credit.

Growth is read on the same 0.5–8 scale, so it stops at the top bucket. When
the chain's cumulative scope before a follow-up already sizes at 8, whether
reviewed or bucketed from churn, the follow-up adds zero growth however much new
scope it delivers. For a confirmed or same-author pair, its share is then charged
against the original; either way it adds no SampleN credit. New scope merged as
its own PR that declares no parent (`Corrects: none`) is sized and credited on
its own.

Unconfirmed eligible same-author pairs use recorded churn and file counts as a
provisional approximation, not reviewed delivered growth. Missing sizing can
remain pending; unconfirmed foreign roots cannot move another author's points.
Reviewed per-root shares for multiple originals are planned, not shipped.
Actor/role are self-asserted local CLI input, not
authentication; single-operator installations confirm their own pairs. The
required distinct actor label is advisory and does not prove independent review.
Disputed status appears in developer/operator views, exports, the corrective
digest and new seals, while team/division views return `not_shown`. Disputes and
withdrawals after confirmation retain prior treatment until operator settlement.
Existing sealed reports never change.

See [Event 3 coverage](technical-reference.md#event-3-coverage) for recorded declarations and limits.
