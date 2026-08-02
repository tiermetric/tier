# Peer Learning with TIER: Move Practices, Not Scores

> **The one rule.** A team gets better together by sharing *what someone
> changed and what happened* -- a practice -- not by comparing frozen
> individual numbers. TIER already ships every surface this workflow needs.
> There is no new feature here: it is a way of using `/scores`, the dashboard,
> and `/scores/compare` that stays name-free and coercion-free by construction.

TIER grades the **work, not the worker** (see the
[project README](../README.md) and
[docs/legal-and-privacy.md](legal-and-privacy.md)). This page shows a team lead
how to run peer learning on that principle: read the name-free levers as
discussion prompts, agree one practice change, then measure honestly whether it
helped.

## Table of contents

- [What this is](#what-this-is)
- [The levers retro](#the-levers-retro)
- [The before/after practice experiment](#the-beforeafter-practice-experiment)
- [Capture the practice](#capture-the-practice)
- [Anti-coercion policy](#anti-coercion-policy)
- [What NOT to do](#what-not-to-do)

---

## What this is

Practice transfer, not number transfer. When one part of a team has learned to
get more yield per $1,000 -- higher cache reuse, less premium-model spend on
routine work, less undirected exploration -- the thing worth spreading is the
*habit that produced it*, described in words, not a leaderboard cell.

TIER supports this with three shipped surfaces and nothing else:

| Surface | What it gives the retro | Names anyone? |
|---|---|---|
| `GET /api/v1/scores` + the dashboard | The name-free **levers** and work-type segments | No -- whole-window, name-free |
| `GET /api/v1/scores/compare` (#277) | A before/after **delta** with a CI-honest `significant` flag | Not in team/division mode |
| Your team's own runbook | The **narrative**: what changed, why, what happened | Written by humans, not TIER |

> **Positioning guardrail.** TIER's served unit is the team/quarter. A
> per-developer number exists, but it is a **private self-diagnostic** for an
> individual to raise their *own* yield -- never a ranking input. Keep every
> step below name-free or self-only.

---

## The levers retro

Run this as a standing team retro (monthly or per-cycle). It uses only the
**name-free levers** on `GET /api/v1/scores` -- the same numbers the dashboard
renders in its cost-composition panel. None of them names an individual, so
they are safe to project on a shared screen in any aggregation mode.

**The levers, and the question each one prompts:**

| Lever (field on `/scores`) | Reads as | Discussion prompt |
|---|---|---|
| `cost_composition.cache_read_share` | Cache-hit share of input-side tokens | Are we re-reading context we could cache? Who has a workflow that keeps this high? |
| `cost_composition.premium_model_share` | Spend share on frontier/reasoning models | Are we reaching for a premium model on routine work a cheaper one handles? |
| `data_quality.exploratory_cost_share` | Share of spend that was exploratory (mainline, no issue) | How much is undirected exploration, and is that deliberate or drift? |
| `work_types[]` | Yield **within** one work category (#187) | Compared like-for-like (bugfix vs bugfix), where is the practice gap? |

**How to run it:**

1. Open the dashboard (served at `http://127.0.0.1:8080/` by default) or fetch
   `GET /api/v1/scores` for the window, and read the cost-composition panel plus
   the work-type segments.
2. Treat each lever as a **prompt, not a verdict**. A high
   `premium_model_share` is not "bad" -- it is a question: is the premium spend
   buying premium outcomes, or is it habit?
3. Compare **within a work type** (`work_types[]`), never across types. The
   pooled top-level `total` is back-compat only and is explicitly **not** a
   cross-type ranking.
4. Agree **one** practice change to try -- e.g. "route routine refactors to the
   cheaper model," or "start sessions from a cached project brief." One change,
   so the next step can actually attribute the effect.

> **Honesty caveat.** Yield reflects task mix and context, not raw talent. A
> senior on a legacy module can show a lower lever than a junior on greenfield.
> Read the levers to find *practices to copy*, never to rank people. There is
> deliberately no absolute good/bad band (see [docs/rubric.md](rubric.md)).

Because `cost_composition` and the `data_quality` shares are name-free, they
carry **identically in developer and anonymized (team/division) mode** -- they
do not defeat k-anonymity, so this retro works even when the deployment never
emits per-developer rows.

---

## The before/after practice experiment

You changed one practice. Did it help? Test it with
`GET /api/v1/scores/compare` (#277): two half-open windows -- **A = before**,
**B = after** -- and read the result honestly.

Every delta is **B - A**. The endpoint reuses the exact windowed computation as
`/scores`, so a compared number never diverges from what `/scores` reports for
the same window.

**Worked example (illustrative).** Say the team adopted "cheaper model for
routine refactors" on 2026-06-01. Compare the month before against the month
after:

```
GET /api/v1/scores/compare?since_a=2026-05-01&until_a=2026-06-01\
&since_b=2026-06-01&until_b=2026-07-01
```

```bash
# read-only viewer token (--read-token, #190) is enough for this GET
curl -sS -H "Authorization: Bearer $TIER_READ_TOKEN" \
  "http://127.0.0.1:8080/api/v1/scores/compare?since_a=2026-05-01&until_a=2026-06-01&since_b=2026-06-01&until_b=2026-07-01"
```

**Reading the result honestly** is the whole point:

- Each row carries `delta_tier`, `delta_weighted_points`,
  `delta_total_cost_usd`, and a `significant` flag.
- `significant` is `true` **only** when the two 95% bootstrap TIER confidence
  intervals do **not** overlap (and the row is `ranked` in both windows).
- **Overlapping CIs, or a window below the ranking floor, means
  `significant: false` -- the move is within sampling noise. Do not over-read
  it.** A positive `delta_tier` with `significant: false` is "promising, keep
  watching," not "it worked."
- Watch the honest windowing skew too: a recent window B reads artificially low
  because cost books up front and outcomes land at merge time. See
  [docs/interpreting-the-number.md](interpreting-the-number.md) before trusting
  a short after-window.

**It works in team/division mode too** -- the endpoint is an aggregate view, so
in anonymized mode it returns `200` with k-anonymized group deltas (k-anonymity
is a two-window **intersection**: a group must clear the floor in *both* windows
to appear as a named row). One honest caveat there:

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
- Signal watched: premium_model_share on /scores; delta_total_cost_usd on
  /scores/compare (May vs June).
- Result: premium share fell, delta_tier positive but significant:false after
  one month -- directional, re-check next cycle.
- Keep / drop: keep; revisit at the next levers retro.
```

That entry is what actually transfers between people. The TIER surfaces told you
*where to look* and *whether the move cleared noise*; your runbook says *what to
do*.

---

## Anti-coercion policy

TIER measures the **work, not the worker**. This policy is a hard line for any
team using this workflow, and it is consistent with
[docs/legal-and-privacy.md](legal-and-privacy.md), which states TIER numbers
**should not be fed into individual performance reviews, compensation,
promotion, ranking-and-yanking, or disciplinary processes**.

Concretely:

- **A manager may never request that an individual's number be shared or
  screenshotted.** Not in a standup, not in a review, not in a DM. The request
  itself is out of bounds, regardless of the answer.
- **Individual numbers are self-view / opt-in only.** A per-developer TIER
  number is a private self-diagnostic, surfaced for the individual to raise
  their *own* yield. Whether to look at it, and whether to discuss it, is the
  individual's choice.
- **Transparency is the safeguard, not exposure.** When a team does run in
  developer mode, both the individual and their manager see the *same*
  transparent number -- and its purpose is to **coach and optimize token
  spend, never to rank or appraise**.
- **The honest caveat travels with every number.** Yield reflects task mix and
  context, not raw talent. No pay, promotion, or ranking language attaches to a
  TIER number, ever.

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
| Tie a tier, or any lever, to pay, promotion, or a review | Use it to coach token-spend habits; keep it out of appraisal entirely |
| Ask for, share, or screenshot another developer's number | Keep individual numbers self-view / opt-in; project only name-free aggregates |
| Read a `significant: false` (or a team-mode delta) as proof a change worked | Treat it as directional; re-check next cycle before claiming an effect |
| Compare yield across different work types | Compare **within** a `work_type` segment (#187) |
| Trust a short, recent after-window at face value | Account for the windowing skew ([interpreting-the-number.md](interpreting-the-number.md)) |

Every example on this page is **name-free or self-only**. If a step would put
one developer's number in front of anyone but that developer, it is not part of
this workflow.

---

## Related

- [docs/interpreting-the-number.md](interpreting-the-number.md) -- read the number honestly; the windowing skew.
- [docs/legal-and-privacy.md](legal-and-privacy.md) -- the labor-law basis for the anti-coercion policy.
- [docs/api-compatibility.md](api-compatibility.md) -- the exact `/scores` and `/scores/compare` contract.
- [docs/rubric.md](rubric.md) -- why there is no absolute good/bad band.
</content>
</invoke>
