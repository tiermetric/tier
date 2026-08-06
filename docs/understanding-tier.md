# Understanding TIER

This page is written in three layers. Each one is a complete, honest answer —
stop wherever you have what you came for.

1. **[The short answer](#layer-1--the-short-answer)** — what TIER is and why a team would want it. No jargon.
2. **[How the meter works](#layer-2--how-the-meter-works)** — where the numbers come from.
3. **[The full method, page by page](#layer-3--the-full-method-page-by-page)** — where every number is defined.

---

## Layer 1 — The short answer

Your team is spending real money on AI coding tools. Most teams can tell you
**how much**. Almost none can tell you **what they got for it**.

TIER answers the second question: **how much shipped work you got per dollar of
AI spend**, not how many tokens you burned.

The number: **outcome points per $1,000.** Work that actually merged goes on
top. On the bottom goes what that AI usage costs at a standard published price
list — deliberately *not* your invoice, so two teams' numbers mean the same
thing. If you spent a lot and shipped little, you find out before the next
invoice.

TIER is built to grade **the work, not the worker.** Its anonymized reporting
modes fold small groups behind an anonymity floor so no individual is named.
There is no silent default here — the server refuses to start until someone
explicitly picks a reporting mode. Naming individuals is a
deliberate choice, legally weighty in some countries. Someone doing hard,
ambiguous work will score lower than someone landing routine changes: a
fact about the work, not the person.

One caveat: this is an estimate, not a meter reading — it reads low on short
windows by construction, and work mix moves it more than skill does.

Measured on TIER's own repository, trailing 30 days to 2026-07-21: most of that
window's spend joined no tracked outcome. Unattributed is not the same as wasted
— it means the spend did not join an outcome TIER could see, which is a coverage
statement about the measurement, not a verdict on the work.

**Next:** [How the meter works](#layer-2--how-the-meter-works), or see one now
with `tierd demo` ([README](../README.md#see-it-in-60-seconds)).

---

## Layer 2 — How the meter works

The score is a ratio:

```
TIER = outcome points ÷ (dollars spent / 1,000)
```

Read as **"outcome points per $1,000."** Both halves are measured from things
that actually happened, not from self-reports.

### The bottom half: dollars

TIER reads the session files your AI coding tool already writes to disk —
for Claude Code, the JSONL under `~/.claude/projects/`. Every request records
how many tokens were used and by which model.

Those tokens are converted to dollars using a **reference price table** shipped
inside the binary, not your invoice. That is deliberate. An invoice bundles
discounts, credits, and negotiated rates, so two teams doing identical work
would score differently. A shared list price makes the number comparable.
See [pricing-philosophy.md](pricing-philosophy.md) for why, and
[reference-price-table.md](reference-price-table.md) for the table itself.

Spend is attributed to a piece of work by reading the issue reference from your
branch name or PR. Spend it can't attribute lands in an **unattributed** bucket
— and that bucket is reported, not hidden. A large unattributed share is
usually exploration, spikes, and dead ends. That is real spend and real
information.

### The top half: outcome points

Only **merged** work counts. Not lines written, not PRs opened, not commits —
merged. Code that never shipped produced no outcome, however much it cost.

Each merged PR is worth points based on its size label, on a fixed five-point
scale:

| Label | Weight |
|---|---|
| `xs` | 0.5 |
| `s` | 1 |
| `m` | 3 |
| `l` | 5 |
| `xl` | 8 |

The scale is **locked**. Label *names* are configurable; the weights are not.
If every org could set its own weights, "points per $1,000" would mean
something different everywhere and cross-team comparison would be meaningless.

Each PR's weight is then scaled by a **quality multiplier** — work that broke
the build, failed CI, or got reverted is worth less than work that landed
cleanly. So the numerator is `Σ(weight × quality)`.

### Three things that will bite you

**1. Task mix shapes the number more than skill does.** A quarter of
greenfield feature work and a quarter of gnarly production debugging will not
score the same, and the difference is not effort or talent. Compare a team to
its own history, not to another team's number.

**2. Small groups are anonymized, by design.** In the `team` setting, a
group with fewer contributing developers than the anonymity floor (default
five, minimum three) collapses into one anonymous row. Their spend and
outcomes stay in the totals — nothing is dropped — but no individual is named. If even
that anonymous row would be too small to hide behind, it is withheld along with the
totals that would give it away, and the response tells you so rather than looking empty. If you are evaluating TIER solo, you must explicitly ask
for `developer` mode or you will not see your own row.

**3. The two halves must cover the same window.** Spend is booked up front;
the outcome lands later, at merge. If you measure spend over a period longer
than your outcomes, the number inflates. We inflated our own number this way
once, caught it, and corrected it. Always window-match. See
[interpreting-the-number.md](interpreting-the-number.md).

**Next:** [The full method, page by page](#layer-3--the-full-method-page-by-page), or
[interpreting-the-number.md](interpreting-the-number.md) — the honest-reading
guide, and the single most useful page here before you trust any score.

---

## Layer 3 — The full method, page by page

This layer says how each number is produced and **links to the authoritative
page for each formula rather than repeating it** — restating a formula in two
places is how the two copies drift apart.

For the code-grounded account of what the tool does and does not do today,
read [how-it-works.md](how-it-works.md) alongside this.

### Capture — how spend gets measured

TIER is **JSONL-first**. The primary path reads the session files the tool
already writes locally; no proxy sits in front of your model provider and no
request is intercepted. An optional reverse proxy exists for environments where
JSONL isn't available, and open-weights / self-hosted models have their own
capture path.

- Capture paths and the laptop-shipper topology — [README § Capturing tokens](../README.md#capturing-tokens)
- Self-hosted and open-weights models — [open-weights-capture.md](open-weights-capture.md)
- What is read, stored, and never touched — [privacy.md](privacy.md)

### Pricing — how tokens become dollars

`ComputeCost` prices each event against the embedded table in
`internal/store/prices.yaml`, which is the single source of truth and is
versioned as one atomic unit. Model names are normalized (date and version
suffixes stripped) before lookup; an unknown model falls back to a
self-hosted-medium rate and emits a warning rather than silently billing wrong.

- Why list prices, not invoices — [pricing-philosophy.md](pricing-philosophy.md)
- The table, and the erratum list — [reference-price-table.md](reference-price-table.md)
- Normalization rules and the target governance model — [cost-normalization-spec.md](cost-normalization-spec.md)

### Attribution — how spend is tied to work

Spend is joined to an issue via the issue reference carried by the branch name
or PR body. Cost is attributed to your **shipping identity** (OS username by
default); outcomes are attributed to the **PR author's GitHub login**. When
those differ they must be mapped to one developer, or cost and outcomes land in
separate rows and cannot be paired.

- Branch, label, and issue-reference conventions — [conventions.md](conventions.md)
- Identity mapping — [README § Identity mapping](../README.md#identity-mapping)

### Scoring — how outcomes become points

The canonical rubric defines the fixed `{0.5, 1, 3, 5, 8}` size scale, the
label taxonomy, the heuristic fallback when a PR carries no recognised label,
and the quality multiplier derived from quality events.

- The canonical rubric, and why it defines no absolute good/bad band — [rubric.md](rubric.md)
- The quality multiplier's degradation rules — [quality-degradation-spec.md](quality-degradation-spec.md)
- Outcome ingest for teams not on GitHub — [outcomes-api.md](outcomes-api.md)
- GitHub webhook wiring — [webhook-setup.md](webhook-setup.md)

### Reading the result

- Honest reading, windowing skew, and when a score is too provisional to trust — [interpreting-the-number.md](interpreting-the-number.md)
- Where TIER sits alongside DORA, SPACE, and DX Core 4 — [how-tier-relates.md](how-tier-relates.md)
- Moving practices between people without ranking them — [peer-learning.md](peer-learning.md)

### The limits, stated plainly

TIER can be gamed, and the ways it can be gamed are published rather than
hidden: label inflation, PR splitting, cheap-work farming, and several failure
modes that are structural rather than adversarial. Read
[adversarial-analysis.md](adversarial-analysis.md) before you put this number in
front of anyone whose incentives it touches.

Deploying per-developer measurement is regulated in some jurisdictions. If you
operate in the EU or anywhere with works-council or GDPR Art. 22
co-determination obligations, read
[legal-and-privacy.md](legal-and-privacy.md) **before** you enable
`developer` aggregation.

### Interfaces

- `/api/v1` compatibility contract — what is pinned and what may change — [api-compatibility.md](api-compatibility.md)
- Operator security model — [security.md](security.md)
- Full documentation index — [docs/README.md](README.md)
