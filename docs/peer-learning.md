# Peer Learning with TIER: Move Practices, Not Scores

> **The one rule.** A team gets better together by sharing *what someone
> changed and what happened* -- a practice -- not by comparing frozen
> individual numbers. Everything this page uses already ships: `/scores`, the
> dashboard and `/scores/compare`. The levers name nobody, but only a
> `developer`-mode server publishes them, and that server names every developer.

TIER coaches the **practice, not the person** (see the
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
- [Coaching, not appraisal](#coaching-not-appraisal)
- [What NOT to do](#what-not-to-do)

---

## What this is

Practice transfer, not number transfer. When one part of a team has learned to
get more yield per $1,000 -- higher cache reuse, less premium-model spend on
routine work, less spend that TIER could not link to an issue -- the thing
worth spreading is the
*habit that produced it*, described in words, not a leaderboard cell.

TIER supports this with three shipped surfaces and nothing else:

| Surface | What it gives the retro | Names anyone? |
|---|---|---|
| `GET /api/v1/scores` + the dashboard | The **levers** and work-type segments (`developer` mode only) | The levers do not; the same response and dashboard name every developer |
| `GET /api/v1/scores/compare` (#277) | A before/after **delta** with a CI-honest `significant` flag | Not in team/division mode |
| Your team's own runbook | The **narrative**: what changed, why, what happened | Written by humans, not TIER |

> **Who sees what.** `developer` mode is meant for a developer and their
> manager, looking at the same numbers so the manager can coach. Today anyone
> with read or dashboard access sees every developer's numbers; per-viewer
> access is planned (#826). Those numbers must never be used for pay,
> promotion, performance reviews, performance improvement plans, discipline or
> dismissal. The levers below name nobody, but they are published only beside
> every developer's named row. See [Coaching, not appraisal](#coaching-not-appraisal).

---

## The levers retro

Run this as a standing team retro (monthly or per-cycle). It uses only the
**name-free levers** on `GET /api/v1/scores` -- the same numbers the dashboard
renders in its cost-composition panel. None of them names an individual, but
they are published in `developer` aggregation mode only, where the same response
and dashboard name every developer (see the note below the steps). Share the
lever figures, not the dashboard.

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

A server in an anonymized (`team`/`division`) mode does **not** publish these
levers (#864). It shows one breakdown of the window, the team (or division) rows
and the total, because a second breakdown can be subtracted from the first: team
totals minus work-type totals recovered a 3-person group's spend in testing, and
a model only one developer used showed that developer's spend in the per-model
rows. Naming no one is not enough to be safe. In `team` or `division` mode, run
the retro on the team rows alone. The levers exist only in `developer` mode, where
the same response and dashboard name every developer, so never change an
install's mode to get them. #937 tracks an audit that would let the levers come
back in anonymized modes.

---

## The before/after practice experiment

You changed one practice. Did it help? Test it with
`GET /api/v1/scores/compare` (#277): two half-open windows -- **A = before**,
**B = after** -- and read the result honestly.

Every delta is **B - A**. In **developer mode**, the endpoint reuses the exact
windowed computation as `/scores`, so a compared number never diverges from what
`/scores` reports for the same window. In **team/division mode**, it refolds both
sealed months with the intersection k floor: a group is named only if it clears
k in both months, and each month's residual must also reach k or the whole
comparison is withheld. Compared rows can therefore differ from `/scores`.

**Worked example (illustrative, developer-mode only).** Say the team adopted "cheaper model for
routine refactors" on 2026-06-01. Compare the month before against the month
after:

```
GET /api/v1/scores/compare?since_a=2026-05-01&until_a=2026-06-01\
&since_b=2026-06-01&until_b=2026-07-01
```

```bash
# the read-only viewer token's VALUE (a password you generated for --read-token;
# see README → What you need before you start) is enough for this GET
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
- Result: premium share fell, delta_tier positive but significant:false after
  one month -- directional, re-check next cycle.
- Keep / drop: keep; revisit at the next levers retro.
```

That entry is what actually transfers between people. The TIER surfaces told you
*where to look* and *whether the move cleared noise*; your runbook says *what to
do*.

---

## Coaching, not appraisal

TIER coaches the **practice, not the person**. In `developer` mode a developer
and their manager see the same numbers, with the same caveats. Use it to coach
practice -- which model for which job, how to reuse context, starting work
from an issue -- never to appraise. The rules below match
[docs/legal-and-privacy.md](legal-and-privacy.md).

- **Never an input to a people decision.** No TIER number and no lever may be
  used for pay, promotion, performance reviews, performance improvement plans,
  discipline or dismissal -- not as the only input, and not as one of several.
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
where one applies, can. Write the purpose down before you turn `developer` mode
on.

**What TIER does today, and what is planned.** Today, in `developer` mode,
anyone who can read the API or the dashboard can see every developer's
numbers. The dashboard lists developers in identifier order and never sorts
them by TIER (v0.4.1 and earlier listed ranked rows first, highest TIER first),
but everyone's figures still sit on one page, so limit who can see it. Issue #826 plans the controls that make "only the
developer and their manager" a property of the deployment rather than a habit
(**planned, not yet built**): a separate viewing token for each permitted
viewer (the developer, their declared manager and an optional skip-level
manager); a log of who viewed each developer's page, which that developer can
read; a limit on how long per-developer data is kept in `developer` mode
(90 days by default, at most 365 days, enough for a full annual coaching
cycle); and a list with no TIER order.

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
| Tie a TIER number, or any lever, to pay, promotion, a review, a performance improvement plan, discipline or dismissal | Use it to coach AI habits; keep it out of appraisal entirely |
| Put a developer's number in a review, or show it to anyone beyond the developer and their manager | Keep it between the developer and their manager (and a skip-level manager, if you use one); show the team only name-free numbers |
| Read a `significant: false` (or a team-mode delta) as proof a change worked | Treat it as directional; re-check next cycle before claiming an effect |
| Compare yield across different work types | Compare **within** a `work_type` segment (#187) |
| Trust a short, recent after-window at face value | Account for the windowing skew ([interpreting-the-number.md](interpreting-the-number.md)) |

---

## Related

- [docs/interpreting-the-number.md](interpreting-the-number.md) -- read the number honestly; the windowing skew.
- [docs/legal-and-privacy.md](legal-and-privacy.md) -- the labor-law basis for keeping TIER out of appraisal.
- [docs/api-compatibility.md](api-compatibility.md) -- the exact `/scores` and `/scores/compare` contract.
- [docs/rubric.md](rubric.md) -- why there is no absolute good/bad band.
