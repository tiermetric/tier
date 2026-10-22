# Peer Learning with TIER: Move Practices, Not Scores

> **The one rule.** A team gets better together by sharing *what someone
> changed and what happened* -- a practice -- not by comparing frozen
> individual numbers. For real data in v0.6.2, use team or division reports
> over sealed months. The levers retro is a post-#826 practice: its figures
> can be previewed only in the synthetic demo (`tierd demo`), which invents
> its own data in a private temporary database and cannot load yours.

TIER coaches the **practice, not the person** (see the
[project README](../README.md) and
[docs/legal-and-privacy.md](legal-and-privacy.md)). This page shows a team lead
how to run peer learning on that principle: agree one practice change, then
measure honestly whether it helped. The name-free levers will be discussion
prompts when developer mode ships (#826); today they are a synthetic-demo preview.

## Table of contents

- [What this is](#what-this-is)
- [The levers retro](#the-levers-retro)
- [The before/after practice experiment](#the-beforeafter-practice-experiment)
- [Capture the practice](#capture-the-practice)
- [Coaching, not appraisal](#coaching-not-appraisal)
- [What NOT to do](#what-not-to-do)

---

## What this is

**v0.6.2 availability:** Per-developer views and the per-issue report are not available on real data until #826 ships; `tierd demo` shows them read-only using its own invented data in a private temporary database and cannot load yours.

Practice transfer, not number transfer. When one part of a team has learned to
get more yield per $1,000 -- higher cache reuse, less premium-model spend on
routine work, less spend that TIER could not link to an issue -- the thing
worth spreading is the
*habit that produced it*, described in words, not a leaderboard cell.

The method uses these three surfaces, with the levers available only in the
synthetic demo until developer mode ships (#826):

| Surface | What it gives the retro | Names anyone? |
|---|---|---|
| `GET /api/v1/scores` + the dashboard | Team/division rows today; **levers** and work-type segments only in the synthetic demo until #826 | Real-data reports name no individual; the synthetic demo names invented developers |
| `GET /api/v1/scores/compare` (#277) | A before/after **delta** with an unvalidated non-overlap field; inference not established | Not in team/division mode |
| Your team's own runbook | The **narrative**: what changed, why, what happened | Written by humans, not TIER |

> **Who sees what.** Real-data servers in v0.6.2 run `team` or `division`;
> `tierd serve --aggregation developer` exits 1 with no bypass. When developer
> mode ships (#826), named-viewer, audit and retention controls will govern
> individual rows. TIER is built for coaching, not for evaluating developers.
> Per-person figures are not accurate enough to judge an individual. The
> synthetic demo shows invented named rows beside the levers. See
> [Coaching, not appraisal](#coaching-not-appraisal).

---

## The levers retro

After developer mode ships (#826), run this as a standing team retro (monthly
or per-cycle). It uses the **name-free levers** on `GET /api/v1/scores` and the
dashboard's cost-composition panel. Today these figures are available only in
the synthetic demo, beside invented named rows. On real data today, compare
your team's sealed months and discuss with the team what changed in their
practice. When the levers become available, share the lever figures, not individual pages.

**The levers, and the question each one prompts:**

| Lever (field on `/scores`) | Reads as | Discussion prompt |
|---|---|---|
| `cost_composition.cache_read_share` | Cache-hit share of input-side tokens | Are we re-reading context we could cache? Who has a workflow that keeps this high? |
| `cost_composition.premium_model_share` | Spend share on frontier/reasoning models | Are we reaching for a premium model on routine work a cheaper one handles? |
| `data_quality.exploratory_cost_share` | Share of spend recorded on `main` or `master`, so TIER could not link it to an issue | What work are we doing on `main` without a branch, and should it have one? |
| `work_types[]` | Yield **within** one work category (#187) | Compared like-for-like (bugfix vs bugfix), where is the practice gap? |

The name `exploratory_cost_share` is misleading and kept for compatibility. It
does not measure exploration: it measures spend TIER could not link to an issue
because it was recorded on `main` or `master`.

**How to run it:**

1. In the synthetic demo today, or after developer mode ships (#826), open the
   dashboard (served at `http://127.0.0.1:8080/` by default) or fetch
   `GET /api/v1/scores` for the window, and read the cost-composition panel plus
   the work-type segments.
2. Treat each lever as a **prompt, not a verdict**. A high
   `premium_model_share` is not "bad" -- it is a question: is the premium spend
   buying premium outcomes, or is it habit?
3. Compare **within a work type** (`work_types[]`), never across types. The
   pooled top-level `total` is back-compat only and is explicitly **not** a
   cross-type ranking.
4. Treat a move to a cheaper model as a hypothesis to investigate. Compare similar work over finished periods, record other changes (task mix, acceptance criteria, pricing and capture), and inspect acceptance and repair evidence beside cost. A before/after change in TIER cannot show that the model change caused the difference, and TIER does not credit merged work to a model, so it cannot say whether the cheaper model did the work as well.

> **Honesty caveat.** Yield reflects task mix and context, not raw talent. A
> senior on a legacy module can show a lower lever than a junior on greenfield.
> Read the levers to find *practices to copy*, never to rank people. There is
> deliberately no absolute good/bad band (see [docs/rubric.md](rubric.md)).

A server in an anonymized (`team`/`division`) mode does **not** publish these
levers (#864). It shows one breakdown of the window, the team (or division) rows
and the total, because a second breakdown can be subtracted from the first: team
totals minus work-type totals recovered a 3-person group's spend in testing, and
a model only one developer used showed that developer's spend in the per-model
rows. Naming no one is not enough to be safe. In `team` or `division` mode, run
the retro on the team rows alone. The levers are a post-#826 practice for real
data; only the synthetic demo publishes them today. Real-data developer-mode
startup is refused. #937 tracks an audit that would let the levers come back
in anonymized modes.

---

## The before/after practice experiment

You changed one practice. Did it help? Test it with
`GET /api/v1/scores/compare` (#277): two half-open windows -- **A = before**,
**B = after** -- and read the result honestly.

Every delta is **B - A**. In **developer mode**, the endpoint reuses the exact
windowed computation as `/scores`, so a compared number never diverges from what
`/scores` reports for the same window; this is a synthetic-demo preview until
#826 ships. In **team/division mode**, it refolds both
sealed months with the intersection k floor: a group is named only if it clears
k in both months, and each month's residual must also reach k or the whole
comparison is withheld. Compared rows can therefore differ from `/scores`.

**Worked example (synthetic demo only; real-data developer mode planned with #826).**
Say the invented team adopted "cheaper model for routine refactors" on
`CHANGE_DATE`. Point the command at the synthetic demo and compare the 8 days
before against the 8 days after. The demo's merges sit 7 to 19 days before the
day it started, so with that day as `S`, replace `BEFORE_START` with `S` minus
22 days, `CHANGE_DATE` with `S` minus 14 days and `AFTER_END` with `S` minus 6
days, all as UTC dates:

```
GET /api/v1/scores/compare?since_a=BEFORE_START&until_a=CHANGE_DATE\
&since_b=CHANGE_DATE&until_b=AFTER_END
```

```bash
# Synthetic demo only; TIER_HOST points at the running tierd demo.
curl -sS "$TIER_HOST/api/v1/scores/compare?since_a=BEFORE_START&until_a=CHANGE_DATE&since_b=CHANGE_DATE&until_b=AFTER_END"
```

**Reading the result honestly** is the whole point:

- Each row carries `delta_tier`, `delta_weighted_points`,
  `delta_total_cost_usd`, and a `significant` flag.
- `significant` is a legacy raw non-overlap result, retained for compatibility.
  It is true only for a developer present and above the floor in both windows
  with disjoint bounds and, in v0.6.1 (#1233), each row's own attributed-cost share at least 50%; false for withheld bounds. Neither value establishes
  an effect or a noise threshold. The API labels it unvalidated and the dashboard
  says “inference not established”, without interval-derived colours.
- In v0.6.1 (#1233), intervals are withheld below 50% of the row's own cost paired with resampled outcomes; exactly 50% passes. When shown they are labelled "excludes uncertainty from unattributed spend". Team and division rows carry none and reveal no floor status.
- Nominal 95% bounds. Coverage is unvalidated: in constructed tests where several outcomes share one issue, they covered the true value less often than 95%. They are descriptive resampling bounds and exclude uncertainty from unattributed spend, incorrect attribution and uncaptured usage.
- Treat a small difference as no evidence either way, and a large one as a prompt
  to investigate, not a measured effect.
- Watch the honest windowing skew too: a recent window B reads artificially low
  because cost books up front and outcomes land at merge time. See
  [docs/interpreting-the-number.md](interpreting-the-number.md) before trusting
  a short after-window.

**In team/division mode, compare two closed, sealed months instead:**

```text
GET /api/v1/scores/compare?period_a=2026-05&period_b=2026-06
```

Give both `period_a` and `period_b` (A earlier than B), or omit both to select
the two latest sealed months. An unsealed month returns `404`; finish
[arming and sealing](quickstart.md#7-viewing-the-dashboard-from-another-machine)
first. The free-window example above returns `400`, with this exact `error`:

```text
?since_a=, ?until_a=, ?since_b=, ?until_b= not accepted for anonymised reports (#913): they are served per sealed calendar month, so two overlapping windows cannot be differenced to isolate one person's figures. Remove ?since_a=, ?until_a=, ?since_b=, ?until_b= and select two months with ?period_a=YYYY-MM and ?period_b=YYYY-MM (neither: the two latest sealed months)
```

A valid sealed comparison returns `200` with k-anonymized group deltas (k-anonymity
is a two-window **intersection**: a group must clear the floor in *both* windows
to appear as a named row). The whole comparison is withheld, and says so, when
either window's own `other` row does not reach *k* (#864), or the comparison's
intersection residual does not reach *k* on either side. One honest caveat there:

> **In team/division mode, `significant` is always `false`.** Group aggregates
> carry no bootstrap CI, so the significance test is simply *not computed* at
> group grain -- absence of a significant flag is **not** evidence of "no
> effect." In anonymized mode, read the delta as **directional only**: the sign
> and rough size of the move, not a pass/fail verdict.

---

## Capture the practice

TIER stores the numbers. **Humans store the narrative.** The learning does not
live in the database -- it lives in the sentence a teammate can read next
quarter and copy.

After the experiment, write the practice into the team's **own** shared doc or
runbook -- not into TIER, which has no field for it. A good entry is four lines:

```markdown
### Practice: cheaper model for routine refactors  (adopted 2026-06-01)
- Change: route bugfix/refactor sessions to the standard model; reserve the
  premium model for design and hard reasoning.
- Signal watched: premium_model_share on /scores (developer mode);
  delta_total_cost_usd on /scores/compare (May vs June).
- Result: premium share fell and delta_tier was positive with significant:false
  after one month. This does not establish that the model change caused the
  difference or that the cheaper model did the work as well. Investigate task
  mix, acceptance, repair evidence, pricing and capture before interpreting it.
- Keep / drop: keep; revisit at the next levers retro.
```

That entry is what actually transfers between people. The TIER surfaces told you
*where to look* and *whether the move cleared noise*; your runbook says *what to
do*.

---

## Coaching, not appraisal

TIER coaches the **practice, not the person**. When developer mode ships (#826),
a developer and their manager will see the same numbers, with the same caveats.
Today, coach from team rows, or preview the per-developer view in `tierd demo`.

- TIER is built for coaching, not for evaluating developers. Per-person figures are not accurate enough to judge an individual.
- **Talk about the habit, not the number.** A coaching conversation looks at
  the practice behind a number (cache reuse, which model for which job, spend
  with no issue attached), agrees one thing to try, and checks later whether
  it helped. The number is where the conversation starts, not a grade.
- **Keep a developer's numbers between the developer and their manager** (and
  a skip-level manager, if your organization uses one). The team retro uses
  only team numbers, which name nobody.
- **Compare within a work type, never rank.** Read a developer's numbers
  against their own trend within one work type, not against a teammate's.
- **The honest caveat travels with every number.** Yield reflects task mix and
  context, not raw talent.

**TIER cannot enforce this.** It shows numbers; it cannot stop someone from
copying one into a review. Only your own written policy, or a works agreement
where one applies, can. Write the purpose down before adopting the developer
mode planned with #826.

**What TIER does today, and what is planned.** Real-data servers in v0.6.2
publish only team or division rows. In the synthetic demo, anyone who can read
the API or dashboard sees every invented developer's numbers in identifier
order, never sorted by TIER. Developer mode is planned with #826: a separate
viewing token for each permitted viewer (the developer, their declared manager
and an optional skip-level manager); a log of who viewed each developer's page,
which that developer can read; a limit on how long per-developer data is kept
(90 days by default, at most 365 days, enough for a full annual coaching cycle);
and a list with no TIER order. These controls are planned, not yet built.
Every mode stores per-developer identifiers; score data is not automatically
deleted today and stays until an admin erases the person. Raw webhook payloads
have separate retention and are not rewritten by an erase (see
[privacy.md](privacy.md#data-subject-rights-access-and-erasure-gdpr-art-15--art-17)).

Deployments in co-determined jurisdictions have a *legal* reason for this too --
see [docs/legal-and-privacy.md](legal-and-privacy.md) for works-council, DPIA,
and GDPR Article 22 guidance. The anonymized `--aggregation team|division` modes
(k-anonymity floor default 5, hard minimum 3) exist precisely so an organization
can run this workflow without ever naming an individual.

---

## What NOT to do

| Do NOT | Do instead |
|---|---|
| Rank teammates against each other by TIER | Compare the **name-free levers** and copy the practice behind the better one |
| Evaluate developers on TIER | TIER is built for coaching, not for evaluating developers. Per-person figures are not accurate enough to judge an individual. |
| Put a developer's number in a review, or show it to anyone beyond the developer and their manager | Keep it between the developer and their manager (and a skip-level manager, if you use one); show the team only name-free numbers |
| Read a `significant: false` (or a team-mode delta) as proof a change worked | Treat it as an observation to investigate; inference is not established |
| Compare yield across different work types | Compare **within** a `work_type` segment (#187) |
| Trust a short, recent after-window at face value | Account for the windowing skew ([interpreting-the-number.md](interpreting-the-number.md)) |

---

## Related

- [docs/interpreting-the-number.md](interpreting-the-number.md) -- read the number honestly; the windowing skew.
- [docs/legal-and-privacy.md](legal-and-privacy.md) -- the labor-law basis for keeping TIER out of appraisal.
- [docs/api-compatibility.md](api-compatibility.md) -- the exact `/scores` and `/scores/compare` contract.
- [docs/rubric.md](rubric.md) -- why there is no absolute good/bad band.
