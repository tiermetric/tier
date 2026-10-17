# Score-series changes

This register identifies shipped changes that can alter a score series, its
attribution, eligibility or statistical meaning. Use it when interpreting a
before/after figure or re-deriving a historical period. It is a seed register,
not proof that an installation's complete definition history is known.
Eligibility-only changes must be recorded separately from scoring changes: a
ranking gate can withdraw ranking or an interval without changing the points or
headline. Attribution and inference changes are labelled separately below.

## Comparison rule

Compare figures under the same measurement definition. If a scoring, pricing, attribution or eligibility change affects either period, label the comparison 'definition changed' and name the change. Historical sealed bytes remain unchanged; a revised live derivation is a separate observation. A comparison across a break names the break's row; a published comparison either names its applicable entries or states that the definition history is unknown.

## Reading the register

All timestamps below identify **shipment to main**, not inferred deployment or
operator activation times. Non-price rows use the merge commit's committer time,
measured with `git log --format='%H %cI' -1 <commit>` and converted to UTC;
price rows use `gh pr view <PR> --json mergedAt`, retaining the file-change SHA
as a secondary citation. A table's `effective_date` is not its shipment timestamp.
Cited SHAs and PRs are in the private development history; public readers cannot
resolve them. Installation activation times are **unknown** unless separately
recorded; #823 is opt-in and Event 3 scoring is OFF by default. The four Event 3
rows record shipped capabilities, not four activated score breaks. Price v1 is
the baseline, with unchanged default values, included to account for every table
version.

Effect types describe when the change reaches observations:

- **prospective**: new insertion/capture, or a newly priced scan, uses the shipped
  rule after deployment/opt-in. It does not by itself rewrite stored history.
- **live-retroactive**: reading existing inputs under a new method, re-ingesting
  earlier outcomes, or a guarded fill of earlier unsealed usage can change a live
  historical derivation.
- **explicit-apply**: an operator deliberately applies a historical change.
  A dry run is not activation. Record the actual apply time, scope and receipt
  in its own row; the shipped tool or price-table date is not that receipt.

### Pricing surfaces

For each price row, **headline** and **segments** can move through cost;
**shares** weighted by dollars and **interval** can move with the affected cost
mix; **flags** about unknown/guessed rates can change when a model gains a row,
while the token-count tripwire is unchanged by rates alone; **eligibility** can
change at a cost floor. These are pricing changes, not changes to outcome
weights. A row addition leaves previously listed rate values unchanged but can
replace fallback pricing for matching usage. An existing-rate correction changes
the pricing definition for an already listed model.
At read time, premium-model share, fidelity unknown-model dollars and the
unknown-pricing token share classify historical inputs against the active table
without changing stored cost, so model coverage or premium-threshold changes can
revise those live historical surfaces; even a rate-only version change can alter
the unknown-pricing flag because the stored price version differs from the active
version.

Neither kind of table revision automatically updates stored `cost_micro`.
Ordinary re-shipping keeps the stored cost. Newly captured rows and fresh scans
use the active table; changing historical stored prices requires an audited
`tierd reprice --commit` operation with its own **explicit-apply** row. No such
installation apply date has been established for this seed. At fixed points,
lower total cost raises TIER and higher total cost lowers it; where token/model
mix does not establish a direction, the table says **unknown**. See the
[reference price table](reference-price-table.md) and
[reproducibility contract](reproducibility.md).

### Sealed-report treatment

Every row follows R-2026-09-29-11: **sealed report bytes never change**. Config
changes apply from the first unsealed period and cross-config sealed comparison is refused. Recomputed live history
is a separate observation, never a replacement of the sealed report. Earlier
rows predate sealing; the same preservation rule governs their use today.

## Shipped register

| shipped (UTC) | PR + rule id | surfaces moved (headline, shares, interval, flags, segments, eligibility) | direction | effect type (prospective / live-retroactive / explicit-apply) | sealed-report treatment | compatible comparison conditions |
|---|---|---|---|---|---|---|
| 2026-06-25T19:52:39Z | #111; `price-table v1`; file change `75407d96` | [Pricing surfaces](#pricing-surfaces). Baseline: externalised the existing rates without changing their values. | Unchanged from the prior hardcoded table. | prospective | Sealed bytes never change (R-2026-09-29-11). | Same default rates; an override needs its own identity. Historical stored-cost changes require a separate explicit-apply receipt. |
| 2026-07-01T20:58:20Z | #112; `price-table v2`; file change `01e16c4b` | [Pricing surfaces](#pricing-surfaces). Existing-rate change: threshold pricing for Gemini 2.5 Pro; adds Sonnet 4.5 and Gemini 3.1 Pro rows. | Gemini 2.5 Pro cost rises above the context threshold, so headline falls at fixed points; new-row net direction unknown. | prospective (stored cost); live-retroactive (premium share and guessed/unknown-pricing flags) | Sealed bytes never change (R-2026-09-29-11). | Same context-tier logic and table content. Historical stored-cost changes require a separate explicit-apply receipt. |
| 2026-07-02T12:40:14Z | #158; `price-table v3`; file change `73db5ec9` | [Pricing surfaces](#pricing-surfaces). Existing-rate corrections: o3, o3-mini, GPT-4.1 nano and DeepSeek R1 fall; Gemini 2.5 Flash rises; DeepSeek V3 changes both ways; retires GPT-4.5. | Mixed cost directions; aggregate headline direction unknown. Retired model can fall back to a guessed rate. | prospective (stored cost); live-retroactive (premium share and guessed/unknown-pricing flags) | Sealed bytes never change (R-2026-09-29-11). | Same corrected rates, model resolution and fallback treatment. Historical stored-cost changes require a separate explicit-apply receipt. |
| 2026-07-02T15:01:43Z | #167; `price-table v4`; file change `e46abb37` | [Pricing surfaces](#pricing-surfaces). Existing-rate change: per-model cache reads for Gemini 2.5/3.1 and DeepSeek; also ships Gemini thought/cache capture. | Discounted cache-read cost falls; newly captured thought tokens can raise cost; net headline direction unknown. | prospective (stored cost); live-retroactive (unknown-pricing flags) | Sealed bytes never change (R-2026-09-29-11). | Same cache multipliers AND captured token classes. Historical stored-cost changes require a separate explicit-apply receipt. |
| 2026-07-07T03:41:45Z | #209; `price-table v5`; file change `148efb0f` | [Pricing surfaces](#pricing-surfaces). Adds missing RPT model rows: GPT-5.x, o1/o4-mini, Claude Opus 4.1 and 4.5 rows, Gemini lite/3.x, Grok variants and DeepSeek V3.2; existing rate rows unchanged. | Unknown in aggregate: explicit rows replace model fallback pricing for matching usage. | prospective (stored cost); live-retroactive (premium share and guessed/unknown-pricing flags) | Sealed bytes never change (R-2026-09-29-11). | Same new-row coverage and fallback/capture population. Historical stored-cost changes require a separate explicit-apply receipt. |
| 2026-07-09T15:58:27Z | #247 (issue #231); rule id: unknown | Attribution/scoring: headline can move through future push deduplication or revert quality; shares, interval, flags, segments and eligibility can move through repository-qualified joins. | Stops cross-repo issue fusion, wrong-repo revert penalties and dropped push outcomes. Numerical direction unknown. | prospective | Sealed bytes never change (R-2026-09-29-11). | Same repository qualifiers and tolerant join. Pre-#247 rows carry `unqualified` and read unchanged under the tolerant join. New capture is prospective; migration does not recover discarded outcomes or infer old repositories. |
| 2026-07-13T06:11:34Z | #308; `price-table v6`; file change `c93dc8e4` | [Pricing surfaces](#pricing-surfaces). Adds host-qualified open-weights per-token rows; existing model-only rates unchanged. | Unknown: host rates replace size-class/model-only pricing only on matching host/model keys. | prospective (stored cost); live-retroactive (premium share and guessed/unknown-pricing flags) | Sealed bytes never change (R-2026-09-29-11). | Same host-qualified keys and billing modes. Historical stored-cost changes require a separate explicit-apply receipt. |
| 2026-07-13T16:23:01Z | #358; `price-table v7`; file change `179e2452` | [Pricing surfaces](#pricing-surfaces). Adds Claude Sonnet 5 and Mythos 5 rows; existing rate rows unchanged. | Matching usage moves from guessed rates to explicit rates; aggregate direction unknown. | prospective (stored cost); live-retroactive (premium share and guessed/unknown-pricing flags) | Sealed bytes never change (R-2026-09-29-11). | Same row coverage and model resolution. Historical stored-cost changes require a separate explicit-apply receipt. |
| 2026-07-22T20:35:23Z | #461; `price-table v8`; file change `d3e41077` | [Pricing surfaces](#pricing-surfaces). Adds GPT-5.6 sol/terra/luna rows; existing rate rows unchanged. | Matching usage replaces fallback pricing; aggregate direction unknown. | prospective (stored cost); live-retroactive (premium share and guessed/unknown-pricing flags) | Sealed bytes never change (R-2026-09-29-11). | Same GPT-5.6 rows and cache-read multipliers. Historical stored-cost changes require a separate explicit-apply receipt. |
| 2026-07-24T14:47:02Z | #510 (issue #495); rule id: unknown | Inference only: interval (pooled, compare, segments and detail); headline, shares, flags, segment point scores and eligibility unchanged. | Joint contribution/cost resampling replaces a fixed denominator. Width may rise or fall with correlation; no universal direction. | live-retroactive | Sealed bytes never change (R-2026-09-29-11). | Same joint resampling, shared-issue allocation and fixed unmatched-cost remainder; compare statistical meaning only under the same method. This shipment establishes no general coverage guarantee. |
| 2026-07-26T20:12:09Z | #513; `price-table v9`; file change `6720959b` | [Pricing surfaces](#pricing-surfaces). Adds Claude Opus 5 row; existing rate rows unchanged. | Matching usage replaces guessed pricing; aggregate direction unknown. | prospective (stored cost); live-retroactive (premium share and guessed/unknown-pricing flags) | Sealed bytes never change (R-2026-09-29-11). | Same Opus 5 row and fast-mode capture limitation. Historical stored-cost changes require a separate explicit-apply receipt. |
| 2026-09-25T14:40:38Z | #787; `price-table v10`; file change `7b133529` | [Pricing surfaces](#pricing-surfaces). Existing-rate change: Sonnet 5 input/output $3/$15 → $2/$10; adds Opus 5.5, Fable/Mythos 5.1, GPT-6 and GLM rows. Premium input threshold also falls from $5 to $4. | Sonnet 5 cost falls, headline rises at fixed points; added-row aggregate direction unknown; Opus 5.5 counts as premium. | prospective (stored cost); live-retroactive (premium share and guessed/unknown-pricing flags) | Sealed bytes never change (R-2026-09-29-11). | Same v10 rates, Z.ai write treatment, premium threshold and model mix. Historical stored-cost changes require a separate explicit-apply receipt. |
| 2026-09-27T21:21:45Z | #897; `price-table v11`; file change `b63b5587` | [Pricing surfaces](#pricing-surfaces). Adds Meta Muse Spark standard and Contributor rows under the meta provider; existing rate rows unchanged. | Matching usage replaces fallback pricing; aggregate direction unknown. | prospective (stored cost); live-retroactive (premium share and guessed/unknown-pricing flags) | Sealed bytes never change (R-2026-09-29-11). | Same Muse rows, billing mode and capture; non-token search charges remain outside these rates. Historical stored-cost changes require a separate explicit-apply receipt. |
| 2026-09-30T20:55:49Z | #1014 (issue #823); R-2026-09-30-10 | Opt-in attribution: shares move; interval, flags, segments and eligibility can move. Headline unchanged for the same retained events and outcomes; routing/capture changes can also change that population. | Attribution moves among issues/repos, including the foreign-repo bucket; aggregate direction unknown. | prospective | Sealed bytes never change (R-2026-09-29-11). | Same worktree-attribution setting and capture/routing population. Default off; only newly stored messages use the opt-in rule; existing bookings keep their first issue/repo/rule. |
| 2026-10-03T01:45:43Z | #1056; `price-table v12`; file change `d68a9d62` | [Pricing surfaces](#pricing-surfaces). Adds Claude Sonnet 5.5 row; existing rate rows unchanged. | Matching usage replaces guessed pricing; aggregate direction unknown. | prospective (stored cost); live-retroactive (premium share and guessed/unknown-pricing flags) | Sealed bytes never change (R-2026-09-29-11). | Same Sonnet 5.5 row and cache-read rate. Historical stored-cost changes require a separate explicit-apply receipt. |
| 2026-10-11T08:25:56Z | #1183 (#1074 PR a); R-2026-10-11-2 / R-2026-10-11-3 | Attribution: shares, interval, flags, segments and eligibility can move; headline unchanged at fixed total cost and points. | Exactly one same-repo real-issue PR match can convert unattributed insertion to attributed usage; affected attribution share can rise and zero-token flags can clear. Other directions unknown. | prospective | Sealed bytes never change (R-2026-09-29-11). | Same PR-number evidence, insert-time resolution and server support; explicit attribution wins, ambiguous matches stay unattributed. No merge-time fill yet. |
| 2026-10-11T20:55:21Z | #1187 (#1074 PR b); R-2026-10-11-3 / R-2026-10-11-4 | Attribution fills: shares, interval, flags, segments and eligibility can move on earlier unsealed usage; headline unchanged at fixed total cost and points. | Eligible unattributed usage becomes attributed; attribution share can rise and zero-token flags can clear. Interval/segment direction unknown. | live-retroactive | Sealed bytes never change (R-2026-09-29-11). | Same PR-number-first fill and bounded branch evidence. Never overwrites attribution; protects sealed months and sealed token lookbacks; branch fill skips forks, PR-number rows, backfill and ambiguous windows. Record fill receipts and the accepted late-webhook residual. |
| 2026-10-12T14:16:52Z | #1203; `price-table v13`; file change `770afeaa` | [Pricing surfaces](#pricing-surfaces). Adds Claude Haiku 5.5 (including its context tier) and GPT-6.1 Sol rows; existing rate rows unchanged. | Unknown in aggregate; model, context and cache mix determine the cost shift. | prospective (stored cost); live-retroactive (premium share and guessed/unknown-pricing flags) | Sealed bytes never change (R-2026-09-29-11). | Same two rows, context tiers and cache multipliers. Historical stored-cost changes require a separate explicit-apply receipt. |
| 2026-10-13T07:44:03Z | #1207; `price-table v14`; file change `db7963e1` | [Pricing surfaces](#pricing-surfaces). Existing-rate change: Sonnet 5.5 cache reads $0.20/M → $0.10/M (0.1× → 0.05×); no added rows. | Cost falls for affected reads; headline rises at fixed points. Other token classes keep their rates. | prospective (stored cost); live-retroactive (unknown-pricing flags) | Sealed bytes never change (R-2026-09-29-11). | Same cache-read rate and captured cache classes. Historical stored-cost changes require a separate explicit-apply receipt. |
| 2026-10-14T19:21:38Z | #1239 (issue #1229); R-2026-10-14-4 | Scoring exclusion: headline points, interval and segments can fall/change; shares and flags can change with outcome population; eligibility can change. This is not merely an eligibility-only gate. | Recognised revert PRs earn no positive credit. Their credit is removed/refused; fixed-cost headline cannot rise from removing it. Original penalties still require separate recognised revert evidence. | prospective (insertion) + live-retroactive on re-ingest/backfill | Sealed bytes never change (R-2026-09-29-11). | Same revert classifier and exclusion state on insertion paths (webhook, backfill, API and push). Webhook redelivery and `tierd backfill` (default `--since`: 90 days ago) can remove an existing revert outcome by merge SHA in any month and reconcile push credit, with no sealed-month check: sealed report bytes stay unchanged, but live re-derivation of a sealed month can change. #1277 historical discovery/explicit-apply remains pending as a separate activation. |

### Shipped, inactive

Dormant shipments record capability availability. Activation adds a second row
under the maintenance rules below.

| shipped (UTC) | PR + rule id | surfaces moved (headline, shares, interval, flags, segments, eligibility) | direction | effect type (prospective / live-retroactive / explicit-apply) | sealed-report treatment | compatible comparison conditions |
|---|---|---|---|---|---|---|
| 2026-10-15T15:43:33Z | #1266 (Event 3); R-2026-10-14-19 / R-2026-10-14-20 | Shipped schema, chain assessment, reviewed sizing and allocation core. With scoring OFF by default: headline, shares, interval, flags, scoring segments and eligibility retain legacy scoring. | No scoring change while inactive; active numerical direction unknown. | prospective | Sealed bytes never change (R-2026-09-29-11). | **shipped, inactive; activation adds its row**. No installation activation is established here. Compare under the same stored policy/rule/rubric; off is rubric 1, explicit on is rubric 2. Cross-rubric sealed comparisons are refused. |
| 2026-10-15T19:50:01Z | #1268 (Event 3); R-2026-10-14-19 / R-2026-10-14-20 | Shipped operator lifecycle and projection readers. With scoring OFF by default: headline, shares, interval, flags, scoring segments and eligibility retain legacy scoring. | No scoring change while inactive; active numerical direction unknown. | prospective | Sealed bytes never change (R-2026-09-29-11). | **shipped, inactive; activation adds its row**. No installation activation is established here. Compare under the same stored policy/rule/rubric; off is rubric 1, explicit on is rubric 2. Cross-rubric sealed comparisons are refused. |
| 2026-10-15T22:03:40Z | #1267 (Event 3); R-2026-10-14-19 / R-2026-10-14-20 | Shipped tierdig2, policy/rubric pinning and verification. With scoring OFF by default: headline, shares, interval, flags, scoring segments and eligibility retain legacy scoring. | No scoring change while inactive; active numerical direction unknown. | prospective | Sealed bytes never change (R-2026-09-29-11). | **shipped, inactive; activation adds its row**. No installation activation is established here. Compare under the same stored policy/rule/rubric; off is rubric 1, explicit on is rubric 2. Cross-rubric sealed comparisons are refused. |
| 2026-10-15T23:41:55Z | #1269 (Event 3); R-2026-10-14-19 / R-2026-10-14-20 | Shipped repair diagnostics, scoring switch and seal preservation. With scoring OFF by default: headline, shares, interval, flags, scoring segments and eligibility retain legacy scoring. | No scoring change while inactive; active numerical direction unknown. | prospective | Sealed bytes never change (R-2026-09-29-11). | **shipped, inactive; activation adds its row**. No installation activation is established here. Compare under the same stored policy/rule/rubric; off is rubric 1, explicit on is rubric 2. Cross-rubric sealed comparisons are refused. |

## Pending — added by their own PRs on activation

No activation dates are assigned here:

- Fix 1 `exclude-reverts` historical discovery/apply (#1277).
- Fix 2 `trigger_event` cutover.
- Fix 5 attribution floor (record eligibility/withholding separately from scoring).
- Event 3 RubricVersion activation (the default-off shipments above are not activation).
- U86 tripwire and its propagated scoring/sealing inputs.
- U84 inference method selected after the study.
- #489 read-time recovery.
- U93 correction overlays.
- Cost promotion and poller cutovers.

## Maintaining this register

A PR that changes scoring, pricing, attribution or eligibility adds its own row in `docs/score-series-changes.md`.

This is an acceptance condition for every fix-plan PR, including an
eligibility-only change. Dormant shipments get a **shipped, inactive** row;
activation adds a second row. A default flip adds a row by PR. A per-install
enablement is recorded with that install's time and receipt, never as a
product-wide row here. Do not substitute a planned date for measured evidence.
Retain measured shipment evidence; record actual activation/apply timestamps and
receipts in the appropriate product-wide row or installation record when known.
Record a rule id (or **unknown**), the six surfaces, direction, effect type, sealed treatment and
comparison conditions. Keep prospective insertion, revised live derivation and
explicit historical application distinct. Record each timestamp's measurement
command in the commit message. A published comparison names its applicable rows
or states that the definition history is unknown.
