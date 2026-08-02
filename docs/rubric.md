# The canonical TIER rubric (v1)

This document is the **versioned, normative rubric** for TIER's outcome weights
and work-type taxonomy. It is the weight-side analogue of the
[reference price table](./reference-price-table.md): the price table pins the
**dollars**, this rubric pins the **weighted points**, and a TIER number is only
comparable across two measurements when **both** stamps match.

- **Rubric version:** `1` (the `scoring.RubricVersion` constant).
- **Where it is stamped:** every `GET /api/v1/scores` response carries a top-level
  `rubric: {version}` block next to `price_table: {version, effective_date}`.
- **When it must change:** any edit to the weight scale or the work-type taxonomy
  below **must** bump `scoring.RubricVersion`. That is the point of the stamp, not
  a side effect — a silent edit would make yesterday's scores falsely comparable
  to today's.

> **There is no absolute "good" TIER.** This rubric deliberately defines **no**
> reference cohort and **no** good/ok/poor band. A weighted point is org-local by
> construction; a bare TIER of 40 is neither good nor bad on its own. What *is*
> defensible is a **self-relative** comparison — the same org over time, or two
> parties under a *matched* rubric + price version — expressed through
> `cost_per_point`. See [What you may and may not compare](#what-you-may-and-may-not-compare).

## Why the rubric is versioned (the exploit it closes)

TIER's numerator is `Σ(weight × quality)`. The weight comes from a size label (or
a heuristic fallback — see [conventions.md](./conventions.md)). If the label→weight
mapping is merely a "convention", two orgs can apply it with different generosity:
a generous-labeling org calls the same change `size/l` (5) that a strict org calls
`size/m` (3). At **identical real efficiency**, the generous org posts a higher
TIER. Without a stamped rubric, a cross-org comparison silently rewards labeling
culture, not engineering yield — and TIER drifts into people-ranking, the one
thing it must not do.

Two distinct mechanisms address this — do not conflate them:

- **Normativity** (this document's worked examples) is what makes a strict org and
  a generous org score the *same real work* the same way. Both are expected to
  calibrate against this canonical rubric rather than their house habits. This is
  the actual cross-org generosity guard.
- **The version stamp** (`rubric.version` on the wire) is *provenance*: it makes a
  change to the rubric **definition** detectable over time, so a self-over-time or
  matched-version comparison can confirm the weighting did not drift. It does
  **not**, by itself, detect a cross-org generosity difference — two orgs on the
  same binary stamp the same version however each labels — nor a local
  `size_labels` remap (see the caveat under [Size → weight](#size--weight)).

So: a `rubric.version` **mismatch** makes a comparison non-comparable and visibly
so; a `rubric.version` **match** is necessary but not sufficient — it must be
paired with shared normative calibration (and matched `size_labels`) before a
cross-org `cost_per_point` reading is defensible.

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
> against another party's until the calibrations are reconciled. A
> config-fingerprint stamp is a possible future refinement; today the honest rule
> is "matched `rubric.version` is necessary, not sufficient, across orgs with
> customized labels."

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
enormous ratio. This is exactly why raw TIER is **not** a rankable absolute, and
why the [ranking floor](./api-compatibility.md) (`#133`: ≥ 3 outcomes and ≥ $5
cost) exists. `cost_per_point` is the steadier unit: across the three examples it
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
window or (under a matched rubric + price version) another party. A pooled
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
- Paired with the `price_table.version` stamp, it is a **constant-dollar**
  self-comparison over time.
- It is guarded on the points denominator: a zero-point row reports `0`
  (rendered `—`), never a divide-by-zero. (A `$0`-cost row with points reports a
  legitimate `0.0`, already surfaced by the zero-token tripwire `#136`.)

### Self-relative interval (not an absolute rank)

`/scores` also returns `cost_per_point_ci_low/high`: the 95% percentile-bootstrap
interval for a ranked developer's `cost_per_point`, derived by reciprocal
transform from the same TIER bootstrap (`#133`) — it expresses `cost_per_point`
against **that developer's own resampled outcome history**, the honest thing a
single window supports. It is **not** a percentile rank against other developers
or other orgs. A cross-org benchmark distribution — the DORA-report analogue —
is a deliberate forward-compatible follow-up (`#239` item 4) that needs a second
org's opt-in data to exist first; TIER does not synthesize one from an absent
dataset.

## What you may and may not compare

| Comparison | Allowed? | Unit | Condition |
|------------|----------|------|-----------|
| Same developer/team over time | **Yes** | `cost_per_point` | Same `rubric.version` **and** same `price_table.version`. A `price_table` bump is a constant-dollar break; a `rubric` bump is a weighting break. |
| Same developer/team, within one `work_type`, over time | **Yes** | `cost_per_point` (per segment) | As above; compare a segment only to the same segment. |
| Two parties, matched versions | **Cautiously** | `cost_per_point` | Same `rubric.version` **and** `price_table.version`, **and** matched `outcomes.size_labels` (a remap diverges from canonical calibration — the stamp cannot see it), **and** comparable cache-share context (see [pricing-philosophy.md §4](./pricing-philosophy.md)). Different cache-hit rates change cost independent of yield. |
| Two parties, mismatched `rubric.version` or `price_table.version` | **No** | — | Non-comparable by construction. This is the guard, not a limitation. |
| Raw `tier` across parties | **No** | — | The numerator is org-local; use `cost_per_point` under matched versions instead. |
| Cross-type (`feature` vs `security`) | **No** | — | A category error (`#187`). Compare within a `work_type` only. |
| Against an absolute "good" value | **No** | — | No such value exists. TIER has no reference cohort baked in, by design. |

## Related

- [conventions.md](./conventions.md) — how labels and issue references are read
  (the operational input to this rubric).
- [reference-price-table.md](./reference-price-table.md) — the dollar side of the
  pair; its `version` co-stamps every comparison.
- [pricing-philosophy.md](./pricing-philosophy.md) — cache-share context (§4),
  required for any cross-party `cost_per_point` reading.
- [api-compatibility.md](./api-compatibility.md) — the `/scores` wire contract for
  `cost_per_point`, its CI, and the `rubric` stamp.
