# Legal & Privacy: Deploying TIER Where Per-Developer Measurement Is Regulated

> ⚠️ **This document is not legal advice.** It is deployment and positioning
> guidance written by engineers, not lawyers. Employment law, works-council
> obligations, and data-protection rules vary by country, by industry, and over
> time, and they turn on facts specific to your organization. **Before deploying
> TIER in a way that measures or ranks named individuals, consult your own legal
> counsel, your Data Protection Officer (DPO), and — where one exists — your
> works council or employee representative body.** Nothing here creates a
> compliance guarantee.

## Why this document exists

TIER attributes AI token spend to outcomes and, in its default modes, reports
that attribution **per named developer**. In several of TIER's target markets,
measuring or ranking the performance of named employees is legally
constrained — sometimes requiring prior co-determination or works-council
agreement, sometimes restricted outright under data-protection law. Adopters
need to understand that exposure *before* their legal and HR review, not after.

This guide covers: what TIER actually measures and stores; the EU labor-law
exposure of per-developer ranking; TIER's posture under GDPR Article 22
(automated profiling); when a Data Protection Impact Assessment (DPIA) is
indicated; the works-council consultation prerequisites; and a
jurisdiction-by-jurisdiction decision table for choosing a deployment mode.

## TIER's stance, in writing

**TIER is a diagnostic, not a performance-appraisal input.** It is designed to
answer an organizational question — "are we getting dollar-value outcomes for
our AI token spend?" — not to grade individuals. The metric is deliberately
aggregate-friendly (team-level rollups are first-class), and the project's
position is that TIER numbers **should not be fed into individual performance
reviews, compensation, promotion, ranking-and-yanking, or disciplinary
processes.**

Two independent reasons make this the right posture:

1. **Legal.** In co-determined jurisdictions (below), using a measurement system
   as an input to individual appraisal is precisely the use that triggers the
   heaviest obligations, and can be unlawful without prior agreement.
2. **Methodological.** Per-developer TIER is easily distorted — by task mix,
   pairing, seniority, review load, and the incentive to game any number that is
   tied to a person's evaluation. TIER measures *spend efficiency of a body of
   work*, which is a team-and-portfolio property, not a fair proxy for
   individual worth.

Treat any per-developer view as an engineering diagnostic (e.g. "which cost
attribution is missing an issue reference?"), and make organizational decisions
from **team-level** numbers.

## What TIER measures and stores (and what it never touches)

The single most important fact for a privacy review: **TIER never reads,
transmits, or stores prompt or completion content, source code, or file
contents.** Its parsers are field allowlists — token counts and metadata only.

Full, code-grounded detail is in **[docs/privacy.md](privacy.md)** (guarantee
verified against the code on 2026-07-04). In summary, TIER stores in a single
local SQLite file, with no external transmission by default:

- a **developer identifier** (an OS username, or a GitHub login for PR
  outcomes) plus any alias mapping you configure;
- an **issue id** derived from a branch name or PR/commit body;
- **model names**, **token counts**, and **list-price cost** in micro-dollars;
- **PR metadata** (number, author login, size-derived weight, quality, merge
  SHA); and
- two **conditional** stores that hold personal data and that an auditor should
  note specifically: **watcher tail-state** (only in live `--watch-repo` mode)
  and **raw GitHub webhook payloads** (only when the webhook path is enabled).
  The raw webhook payloads *do* contain contributor names, email addresses, PR
  titles, and commit messages — the same PR/commit metadata GitHub itself
  stores — bounded to roughly 90 days. See [docs/privacy.md](privacy.md) for the
  exact fields, retention bounds, and how to store none of it (run without the
  webhook path).

The privacy guarantee bounds *content*. It does **not** by itself resolve the
*labor-law* question, because the developer identifier plus cost attribution is
personal data about a named individual regardless of whether any prompt text is
stored. That is the subject of the rest of this document.

## Single-tenant reality (relevant to data-segregation claims)

TIER today is **single-tenant with no organizational isolation.** There is no
`tenant_id` / org column: developer identifiers are global, so two
organizations' `alice` rows would collide. Practical consequences for a legal
review:

- **Do not run TIER as a shared multi-org service.** Run one instance per team
  or per organization. TIER cannot enforce data segregation between cohorts,
  business units, or legal entities within a single database — that separation
  must be achieved by running **separate instances** with separate database
  files and separate access control.
- Do not represent to a works council or DPA that TIER provides
  cross-team/cross-entity data isolation. It does not. The only isolation
  boundary that exists is the process and its local SQLite file (mode `0600`).

## EU labor-law exposure of per-developer ranking

In much of the EU, systems that monitor employee performance or behavior are
subject to **co-determination** or **works-council consultation** before they
may be introduced. Per-developer TIER — which ranks named developers by an
efficiency number — is squarely the kind of system these rules govern. The
threshold is generally the *capability* to monitor individual performance, not
whether you subjectively intend to use it that way.

### Germany — Betriebsrat co-determination (§87 BetrVG)

Under **§87(1) No. 6 of the Betriebsverfassungsgesetz (BetrVG)**, the works
council (**Betriebsrat**) has a **mandatory co-determination right** over the
introduction and use of "technical devices designed to monitor the behavior or
performance of employees." German case law reads this broadly: a system that is
*objectively suitable* for monitoring individual performance is covered even if
that is not its stated purpose. A per-developer TIER deployment is very likely
in scope.

- **Consequence:** where a Betriebsrat exists, introducing per-developer TIER
  generally requires a **works agreement (Betriebsvereinbarung)** negotiated in
  advance. Deploying without one can render the measurement unlawful and its
  outputs unusable, and can expose the employer to injunctive relief.
- **Team-only aggregate** reporting that never surfaces individual performance
  substantially reduces (though a works council may still wish to be consulted
  about) this exposure.

### France — CSE works-council consultation

In France, introducing a tool capable of monitoring employee activity generally
requires **prior information-and-consultation of the CSE (Comité Social et
Économique)** (see, broadly, Art. L2312-38 of the Code du travail), and
employees must be **informed in advance** of any system used to monitor their
activity. The CNIL (the French DPA) has repeatedly emphasized proportionality
and transparency for workplace monitoring. Per-developer measurement without
prior CSE consultation and employee information is a live legal risk.

### Netherlands — works council consent (WOR Art. 27)

Under the **Wet op de ondernemingsraden (WOR)**, the works council
(**Ondernemingsraad**) has a **consent right (instemmingsrecht) under Article
27** over decisions to adopt, amend, or withdraw arrangements for personnel
monitoring or for processing personal data of employees, including systems that
monitor presence, behavior, or performance. Adopting per-developer TIER without
the OR's prior consent can make the introduction voidable.

### Other EU/EEA jurisdictions

Most EU/EEA member states have an analogous works-council consultation or
co-determination regime for employee-monitoring systems (e.g. Italy's Art. 4 of
the Statuto dei Lavoratori, which restricts remote monitoring of workers).
**Assume consultation is required and confirm locally.** The safe default in any
EU/EEA jurisdiction is team-only aggregate reporting plus advance consultation.

## GDPR Article 22 posture (automated profiling)

**GDPR Article 22** restricts decisions "based solely on automated processing,
including profiling," that produce **legal effects** or **similarly
significantly affect** an individual. TIER's designed posture keeps it clear of
Article 22:

- **TIER does not make decisions.** It produces a diagnostic number. It does not
  automatically discipline, rank-and-yank, deny promotion, or alter compensation.
- **Keep a human in the loop and keep TIER out of appraisal.** Article 22's
  strongest protections attach when an automated output *solely* drives a
  significant decision about a person. TIER's stance (above) — that its numbers
  are not an appraisal input — is what keeps a deployment out of that box. If an
  adopter *does* wire TIER outputs into individual HR decisions, they change the
  legal analysis and likely trigger Article 22 (and the co-determination regimes
  above) in full.

Independently of Article 22, per-developer cost attribution is **personal data**
under GDPR. The usual obligations apply: a **lawful basis** (legitimate interest
will require a balancing test and is weakened by any appraisal use),
**transparency** to the data subjects, **data minimization**, **purpose
limitation**, and **storage limitation**. Team-only aggregate reporting that
cannot single out an individual is the strongest data-minimization posture.

## When a DPIA is indicated

A **Data Protection Impact Assessment** is required under **GDPR Article 35**
when processing is "likely to result in a high risk" to individuals —
explicitly including "**systematic and extensive evaluation of personal
aspects ... based on automated processing, including profiling**" and
"**systematic monitoring**." Several EU DPAs list *employee monitoring* on their
mandatory-DPIA lists.

**Practical guidance:**

- **Per-developer deployment:** treat a DPIA as **expected**. Systematic
  evaluation of employees' work efficiency is a textbook high-risk case.
- **Team-only aggregate deployment:** a DPIA is far less likely to be required,
  because processing that cannot single out an individual is not "evaluation of
  personal aspects" of a data subject — but document that reasoning and confirm
  with your DPO.
- **Do the DPIA before deployment,** involve the DPO, and where consultation is
  required, sequence it with the works-council process (below). A DPIA is also
  the natural place to record the retention bounds and the conditional
  personal-data stores documented in [docs/privacy.md](privacy.md).

## Works-council consultation prerequisites (checklist)

Where a works council or equivalent body exists, treat the following as
prerequisites **before** per-developer TIER goes live. Adapt to local law with
counsel:

1. **Engage early.** Bring the works council in at the design/decision stage, not
   after deployment. In Germany the co-determination right precedes introduction.
2. **Document purpose and scope.** State plainly that TIER is a spend-efficiency
   diagnostic, name what it measures, and — critically — commit in writing to
   what it will **not** be used for (individual appraisal, discipline,
   compensation, ranking).
3. **Describe the data.** Provide the [docs/privacy.md](privacy.md) field list:
   what is stored, the no-content guarantee, retention bounds, and the
   conditional PII stores (watcher state, raw webhook payloads).
4. **Prefer the least-intrusive mode.** Offer team-only aggregate reporting as
   the default; justify any per-developer processing against necessity and
   proportionality.
5. **Agree access controls and retention.** Who can read scores; how long data
   is kept; how the single-tenant local database is protected.
6. **Reach the required instrument** — a Betriebsvereinbarung (DE), CSE
   information-and-consultation record (FR), or OR consent (NL) — before go-live.
7. **Provide transparency to employees** and a route to raise concerns.
8. **Complete the DPIA** and reconcile it with the consultation outcome.

## Deployment decision table: team-only vs per-developer, by jurisdiction

The recommended deployment mode depends on where your developers are employed
(the location of the *employees measured*, not the company HQ). "Team-only" here
means reporting that never surfaces an individual developer's performance.

> **Availability note (accurate as of 2026-07-09):** TIER now ships a dedicated
> **team-only aggregation mode** with a k-anonymity floor (issue **#185**). It is
> selected by a **REQUIRED** `--aggregation team|developer` setting on
> `tierd serve` (env `TIER_AGGREGATION`, config key `aggregation`): there is **no
> silent default** — `tierd serve` refuses to start until you choose, so an
> existing deployment can never change its reporting posture on upgrade by
> accident. In `team` mode `GET /api/v1/scores`, the dashboard, and
> `GET /api/v1/scores/{developer}` **never** return an individual developer name:
> named rows are replaced by team aggregates, and any team with fewer than the
> k-anonymity floor of **contributing** developers is collapsed into an aggregate
> **"other"** bucket (its spend and outcomes remain in the totals — never
> dropped — so team numbers stay honest). The floor defaults to **k = 5** with a
> **hard minimum of 3** (`--k-anonymity`, env `TIER_K_ANONYMITY`, config key
> `k_anonymity`); `tierd serve` refuses to start with a smaller value. The local
> `tierd score` CLI is a deliberate carve-out — it is a single-operator tool that
> reads the invoking user's own JSONL, not a served multi-viewer surface, so it
> is not gated. **Still restrict access and consult as required:** team mode
> removes the technical capability to surface individuals, but the underlying
> database still stores per-developer identifiers (see the storage note below),
> so run one instance per cohort and follow the works-council / DPIA steps below.

| Jurisdiction of measured employees | Recommended mode | Prerequisites before go-live | Rationale |
|---|---|---|---|
| **Germany (DE)** | **Team-only** | Betriebsrat co-determination; works agreement (§87 BetrVG) if any individual-level capability remains; DPIA | Per-developer monitoring is co-determined under §87(1) No. 6; suitability to monitor is enough to trigger it. |
| **France (FR)** | **Team-only** | Prior CSE information-and-consultation; advance employee notice; DPIA | Monitoring tools require CSE consultation and transparency (Code du travail L2312-38; CNIL guidance). |
| **Netherlands (NL)** | **Team-only** | OR consent under WOR Art. 27; DPIA | Personnel-monitoring arrangements need works-council consent before adoption. |
| **Other EU / EEA** | **Team-only (default)** | Local works-council consultation; DPIA; lawful basis + transparency | Most member states have analogous monitoring-consultation regimes; confirm locally. |
| **UK** | Team-only preferred; per-developer only with safeguards | UK GDPR lawful basis + DPIA; transparency; ICO employment-monitoring guidance | No works-council co-determination regime, but ICO expects a DPIA and proportionality for monitoring. |
| **US (at-will, non-union)** | Per-developer possible | Internal policy; transparency; still keep TIER out of appraisal | Fewer statutory constraints, but the diagnostic-not-appraisal stance still applies; state privacy laws may add duties. |
| **Anywhere with a collective agreement / union** | Follow the agreement | Whatever the agreement requires | A collective bargaining agreement can impose obligations beyond statute regardless of country. |

**How to read this table:** in every EU/EEA row the safe default is **team-only
aggregate reporting plus advance consultation**. Run `tierd serve` with
`--aggregation team` (the #185 mode) so named per-developer views are technically
suppressed — the API, dashboard, and per-developer endpoint never return an
individual name, and sub-k cohorts collapse into "other". Pair that with access
restriction (a read-only viewer token, #190) and separate instances per cohort.
Per-developer deployment in a co-determined jurisdiction should not go live until
both (a) the required works-council instrument is in place and (b) a DPIA has
been completed — and even then, only as an engineering diagnostic, never as an
appraisal input.

## Summary

- TIER stores **no prompt/completion content, no code, no file contents** (see
  [docs/privacy.md](privacy.md), verified 2026-07-04) — but per-developer cost
  attribution is still **personal data** and still triggers labor-law and
  data-protection duties.
- In **DE / FR / NL** (and most of the EU/EEA), per-developer ranking is
  **co-determined or consultation-restricted** — engage the works council and
  complete a DPIA before go-live.
- **Team-only aggregate** reporting is the low-exposure default and is now a
  built-in mode (#185): run `tierd serve --aggregation team` (k-anonymity floor
  `--k-anonymity`, default 5, hard minimum 3) so the API, dashboard, and
  per-developer endpoint never surface an individual name and sub-k cohorts
  collapse into "other" with totals preserved. `aggregation` is a required
  setting with no silent default; the local `tierd score` CLI is not gated.
- TIER is **single-tenant with no org isolation** — separate cohorts by running
  separate instances; do not claim built-in data segregation.
- TIER's position, in writing: **diagnostic, not a performance-appraisal input.**
- **None of the above is legal advice.** Consult your counsel, DPO, and works
  council.

## Related

- [docs/privacy.md](privacy.md) — exactly what TIER reads, stores, and never
  touches, grounded in the code.
- [docs/how-tier-relates.md](how-tier-relates.md) — why TIER complements DORA /
  SPACE / DX Core 4 rather than replacing them (reinforces the aggregate,
  non-appraisal framing).
