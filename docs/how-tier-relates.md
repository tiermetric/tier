# How TIER Relates to DORA, SPACE, and DX Core 4

**Audience:** anyone evaluating TIER who already runs (or is considering) an
engineering-metrics framework and asks the natural first question: *"How does
this sit alongside DORA / SPACE / DX Core 4 — does it replace them?"*

**Short answer:** it complements them. TIER measures one axis none of the three
frameworks covers — the **dollar efficiency of AI token spend** — and it is
deliberately narrow. It is not a delivery-throughput metric, not a
developer-experience or well-being metric, and not a holistic-productivity
score. Run TIER *next to* your existing framework, never fused into it.

---

## The one thing TIER measures

TIER is a single ratio (`internal/scoring/engine.go`):

```
TIER = Σ(outcome_weight × quality_multiplier) / (list-price AI cost / $1,000)
```

- **outcome_weight** — the size of a merged outcome, from a PR `size/*` label,
  or a lines/files git heuristic when no label is set.
- **quality_multiplier** — a within-formula discount on that outcome: `1.0` for
  a clean merge, reduced when the work is later reverted or fails CI (floors
  `0.7` ci-fail, `0.8` strategic revert, `0.1` quality revert).
- **list-price AI cost** — the AI spend attributed to that work, priced from a
  versioned reference price table (list price, not your negotiated invoice).

So TIER answers exactly one question: **"How much shipped outcome did we get per
$1,000 of AI compute?"** Two sidecars travel with it — **Coverage %** (the share
of that cost we actually measured vs. inferred, the trust line on the number)
and **Spend Leverage** (list price ÷ your actual invoice, a CFO discount signal).
Neither changes what TIER *is*: a trailing efficiency diagnostic on the AI-cost
axis.

### "Trailing" and "diagnostic" — chosen words

- **Trailing.** TIER is computed *after* work merges. Outcomes enter on a
  `pull_request` merge webhook; the quality multiplier is revised later when a
  revert or CI-failure event arrives. It reports on realized cost and realized
  outcomes — it does not predict, forecast, or steer work in flight.
- **Diagnostic, not a target.** TIER is a lagging indicator you read to
  investigate ("why did our AI spend per outcome jump this month?"), not a KPI
  to maximize. The ranking floor in the engine (`MinRankedOutcomes = 3`,
  `MinRankedCostUSD = $5.00`) exists precisely because tiny samples produce
  absurd ratios; below-floor rows are listed but never ranked. That is a
  diagnostic posture, not a scoreboard posture.

---

## What TIER does NOT claim

Stated plainly, so no adopter over-reads the number:

- **Not delivery throughput.** TIER says nothing about how often you deploy, how
  fast changes reach production, or how much you ship in absolute terms. A team
  that ships more is not "better TIER"; a team that ships the same for less AI
  spend is.
- **Not developer happiness or experience.** TIER has no signal for
  satisfaction, well-being, flow, cognitive load, or friction. It never surveys
  a human.
- **Not holistic productivity.** TIER is one ratio on one axis. It is not a
  productivity index and cannot stand in for one. "Good TIER" and "productive
  team" are different claims; treating them as the same is the misuse this
  document exists to prevent.
- **Not a semantic quality or complexity measure.** The `outcome_weight` is a
  size proxy (label or lines/files), and the `quality_multiplier` is a revert /
  CI-failure discount — **not** a change-failure *rate* and **not** an
  assessment of how hard or how good the code is. A subtle 5-line concurrency
  fix and a 5-line typo fix weigh the same today.

---

## Dimension-by-dimension mapping

For each dimension of each framework: does TIER measure it? The honest answer is
almost always **no — and that is the point.** TIER adds a column those
frameworks do not have.

### DORA — the four keys

DORA measures software *delivery* performance. Its four keys:

| DORA key | What it measures | Does TIER measure it? |
|---|---|---|
| Deployment Frequency | How often you release to production | No. TIER has no deploy signal. |
| Lead Time for Changes | Commit → production latency | No. TIER is not a latency metric. |
| Change Failure Rate | % of deployments causing a failure | No. TIER's revert/CI discount adjusts an outcome's *weight* inside the ratio; it is not reported as a failure rate and must not be read as one. |
| Time to Restore Service | How fast you recover from a failure | No. TIER has no incident or recovery signal. |

**Relationship:** orthogonal. DORA tells you how well you *deliver*; TIER tells
you how efficiently AI *dollars* convert into delivered outcomes. Neither
substitutes for the other.

### SPACE — the five dimensions

SPACE is a framework for reasoning about developer productivity across multiple
dimensions (deliberately resisting a single number). Its five:

| SPACE dimension | What it captures | Does TIER measure it? |
|---|---|---|
| **S**atisfaction & well-being | How developers feel about work, tools, team | No. TIER never measures humans. |
| **P**erformance | Outcomes of work (quality, reliability, impact) | Partially adjacent: TIER's numerator is a size-weighted outcome count discounted for reverts — a narrow slice, not a performance judgment. |
| **A**ctivity | Counts of actions (commits, PRs, reviews) | No, as an end. TIER counts merged outcomes only as the numerator of an efficiency ratio, never as an activity score. |
| **C**ommunication & collaboration | How work and information flow between people | No. TIER has no collaboration signal. |
| **E**fficiency & flow | Ability to do work with minimal interruption | Adjacent in name only. SPACE efficiency is about *human* flow; TIER efficiency is about *dollar* yield of AI compute. Do not conflate them. |

**Relationship:** SPACE's own thesis — never collapse the dimensions into one
number — is exactly TIER's posture. TIER is a specialized reading on a
cost-efficiency axis SPACE does not include; it belongs *beside* a SPACE
picture, not *inside* a fused score.

### DX Core 4 — the four top-level dimensions

DX Core 4 unifies DORA, SPACE, and DevEx into four dimensions with a
per-engineer normalization:

| DX Core 4 dimension | What it captures | Does TIER measure it? |
|---|---|---|
| Speed | Throughput / delivery velocity (e.g. diffs per engineer, lead time) | No. TIER is not a velocity metric. |
| Effectiveness | Developer experience — ability to do work well (DXI) | No. TIER has no experience signal. |
| Quality | Change failure rate, failed-deployment recovery, operational health | No. As with DORA, TIER's revert/CI discount is a weight adjustment inside the ratio, not a quality dimension. |
| Impact | Business value / share of time on valuable work | No. TIER measures cost efficiency of outcomes, not their downstream business impact. |

**Relationship:** DX Core 4 deliberately spans speed, experience, quality, and
impact — and deliberately does **not** include "AI-spend efficiency." That is
the gap TIER fills. If you run DX Core 4, add TIER as a companion cost lens; do
not try to graft it onto one of the four dimensions.

---

## Why none of the three covers TIER's axis

DORA, SPACE, and DX Core 4 all predate the era where a large and *variable*
fraction of engineering cost is metered AI token spend. They measure delivery,
experience, and outcomes — all valuable — but none of them asks *what the AI
compute cost to produce that delivery, and whether that cost is trending
efficiently.* That is a new axis, and it is the only axis TIER measures. That is
why TIER is a complement: it adds a column, it does not overwrite one.

---

## Anti-patterns (do not do these)

These are the misuse modes that turn a useful diagnostic into an actively
harmful one. The third is the specific Goodhart failure the project's
adversarial analysis warns about.

1. **TIER as a sole (or primary) performance input.** Do not put TIER in a
   performance review, a stack-rank, or a compensation formula. TIER measures
   dollar-efficiency of AI spend on merged work — it is blind to difficulty,
   collaboration, mentoring, on-call, design, and everything else that makes an
   engineer valuable. As a *sole* input it rewards exactly the wrong behaviors
   (cheap, high-label-weight, low-difficulty work).

2. **Cross-cohort leaderboards.** Do not rank developers, teams, or
   organizations against each other on TIER. Model mix, task mix, labeling
   discipline, and capture coverage differ across cohorts, so the comparison is
   apples-to-oranges. TIER is also **single-tenant with no org isolation today**
   (see the README status section), so cross-org comparison is not even
   technically meaningful. Compare a cohort *to its own past*, never to another
   cohort.

3. **Composite fusion into a single performance number (the Goodhart trap).** Do
   not blend TIER into a DORA/SPACE/DX Core 4 composite or any "one number"
   dashboard that purports to rate performance. *When a measure becomes a
   target, it ceases to be a good measure.* Fusing TIER into a performance score
   makes it a target, and the gaming paths are obvious and cheap: inflate PR
   `size/*` labels to raise the numerator; avoid hard-but-small work that weighs
   little; or — worst of all — move AI usage onto tools TIER cannot capture,
   which lowers measured cost, inflates the ratio, *and* silently craters
   Coverage %, destroying the very trust line that made the number worth reading.
   Keep TIER standalone and paired with its Coverage % so it stays diagnostic.

---

## The one-line takeaway

> TIER is a trailing efficiency diagnostic on the AI-cost axis — an axis DORA,
> SPACE, and DX Core 4 do not measure. Run it *beside* your framework as a
> complement. Never fuse it into a single performance number, never rank cohorts
> on it, and never make it a sole performance input.

See [How TIER Works Today](how-it-works.md) for the full measurement model and
its grounding in the code.
