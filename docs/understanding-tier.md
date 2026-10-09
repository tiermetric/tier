# Understanding TIER

TIER (Token Impact & Efficiency Ratio) is explained here in three layers. Each
layer stands on its own, so read only as far as you need.

1. **[The short answer](#layer-1--the-short-answer)** — what TIER is and why a team would want it, in plain words.
2. **[How the meter works](#layer-2--how-the-meter-works)** — where the numbers come from. Technical terms are defined where they first appear.
3. **[The full method, page by page](#layer-3--the-full-method-page-by-page)** — how to run it on real data, then each rule in brief with a link to the page that defines it.

---

## Layer 1 — The short answer

The bill from an AI coding tool tells you how much you spent. It does not tell
you what the spending produced. TIER measures that second thing: **how much
finished work you got for each dollar of AI use.**

The number is **outcome points per $1,000**. Higher means more finished work
for the same money.

- **The top of the fraction is finished work.** A piece of work counts only
  once it has been merged, which means accepted into the team's main code.
  Bigger pieces earn more points. With GitHub's webhook connected, work that
  later fails its GitHub Actions run or has to be undone earns fewer; without
  the webhook, TIER sees neither.
- **The bottom is what the AI use costs at the AI vendors' published prices**,
  in US dollars, taken from a price table built into TIER. It is deliberately
  *not* your invoice. Discounts and credits differ between companies and change
  over time. A fixed price list means the same work always costs the same, so
  this month can be compared with last month.

Compare a team with its own history, not with other teams. The kind of work a
team takes on moves the number, and the number cannot tell the kind of work
apart from the skill of the people doing it.

**What it is for.** TIER is built to coach the practice, not the person. It
shows where AI spend turned into finished work and where it did not, so a team
can change how it works. It is not for pay, promotion or performance reviews.
That is a rule for the people who use TIER. The software cannot enforce it.
Points follow the size of the finished work, not how hard it was: someone who
spends a lot of AI effort on a small fix to a hard problem scores lower than
someone making routine changes of the same size. A low number describes the
work, not the developer's ability.

**Who sees what.** TIER has three reporting modes. Its server will not start
until someone picks one, because there is no default. Whichever mode you pick,
read [legal-and-privacy.md](legal-and-privacy.md) first. In some countries,
Germany, France and the Netherlands among them, even reporting by group can
require consulting employee representatives.

- `team` and `division` only ever show groups. A group with fewer than five
  counted people is merged into one catch-all row, so the reports name no
  individual. TIER counts people, not raw IDs: an ID counts only if, in the
  window, it is on the roster, is not a bot, and has activity a collector,
  the proxy or a merged pull request recorded (a hand-entered `/costs` row or
  a push-only commit does not count). A person who works under two names
  counts once after their IDs are mapped together
  ([more detail](#three-things-that-will-bite-you)). Five is the default. The operator can
  change it, but never to fewer than three. Whoever holds the server's
  administrator token (its password for changes) can still export one
  person's stored records, so that person's request to see their data can be
  answered.
- `developer` shows a named row for every person. It carries the heaviest
  legal obligations in those countries, so it should be a deliberate choice.
  Where a team uses it, the intended use is coaching: the developer and their
  manager look at the same page together. Today, anyone who can read the server
  sees every developer's row;
  [The limits, stated plainly](#the-limits-stated-plainly) says who that is.
  Access limited to each person's own row is planned but not built.

**One caveat.** The number is an estimate built from real records, not an exact
reading. Spend is recorded when it happens, but work only counts when it
merges, which comes later. So a period that ended very recently reads too low,
and a period that starts before TIER began recording spend reads too high.
Layer 2 explains both.

**A real example.** On TIER's own repository, over the 30 days to 2026-07-21,
TIER measured **221** outcome points per $1,000 across 152 merged pieces of
work. In the same period, **72.1%** of the spend could not be linked to a
specific piece of work. (These are the figures the project's
[README](../README.md) publishes.) "Could not be linked" is not the same as
"wasted". The unlinked spend is still counted in the bottom of the fraction.
What it lacks is a label saying which task it went to.

**Try it.** Install TIER with Go 1.26.9 or newer (or 1.27.2+; Go 1.27.0–1.27.1 carry the same advisories)
(`go install github.com/tiermetric/tier/cmd/tierd@latest`), or download a
release; [quickstart.md §1](quickstart.md#1-install) covers both. Then run
`tierd demo` and open `http://127.0.0.1:8080` in a browser. The demo fills the
dashboard with invented developers and spend, and picks the `developer` mode
for you. To measure your own work, see
[From install to a first real score](#from-install-to-a-first-real-score).

**Next:** [How the meter works](#layer-2--how-the-meter-works).

---

## Layer 2 — How the meter works

The score is a ratio:

```
TIER = outcome points ÷ (list-price cost of the AI use / 1,000)
```

Read it as **"outcome points per $1,000."** The *list-price cost* is what the
AI use would cost at the vendors' published per-token prices, not what you
paid. TIER computes the ratio per developer. A team's or division's score is
its members' points added together, divided by their costs added together. It
is not an average of individual scores. In developer mode the API covers the
last 90 days and the dashboard the last 30 by default, and both can be changed.
In team and division mode both read one whole sealed calendar month, the latest
by default; pick another in the dashboard's month picker or with `?period=YYYY-MM`.

Both halves come from records that already exist: AI session logs, and merged
pull requests. Neither comes from surveys or timesheets. The main human
judgment in the formula is the size label on a pull request, covered below (a
revert's reason is also read from keywords in its human-written message).

### The bottom half: dollars

TIER reads the session files your AI coding tool already writes to disk. For
Claude Code, these are JSONL files (one JSON record per line) under
`~/.claude/projects/`. Each AI reply in them records its token counts and the
model's name. A *token* is a fragment of text a few characters long, and it is
the unit AI vendors bill by. Codex CLI, Opencode and Meta's Muse Code have
their own local readers, each switched on by a flag. Other tools can be captured through an
optional proxy. [README § Capturing tokens](../README.md#capturing-tokens)
lists which tools are supported and how well tested each one is.

Tokens become dollars through a **reference price table** built into the
program. It holds the vendors' published prices in US dollars per million
tokens, under one version number for the whole table. That choice is
deliberate. An invoice bundles discounts, credits and negotiated rates, so
identical work would cost different amounts at different discount levels. A
list price makes the same work cost the same. The table changes only when you
install a new TIER release or supply your own table. On a flat-rate
subscription, the figure is what your usage would have cost at list price, not
what you paid. If you record what you actually paid, TIER reports the ratio
between the two as a separate number. See
[pricing-philosophy.md](pricing-philosophy.md) for why, and
[reference-price-table.md](reference-price-table.md) for the table itself.

Spend is linked to a piece of work through the branch the AI session was on.
TIER looks for the issue number (the ticket number in your tracker) in the
branch name, such as `feature/42-login` or `fix/TIER-99-crash`, or in a commit
on that branch made around the same time. Spend it cannot link lands in an
**unattributed** bucket. It stays in the bottom half of the fraction, so it
still lowers the number. It is reported as its own line alongside the score,
and withheld with the totals when the anonymity rule described below hides
them. The bucket's label says why the link failed:

| Bucket | What happened |
|---|---|
| `unattributed:main` | recorded on `main` or `master`, which carry no issue number |
| `unattributed:branch-without-issue` | recorded on a named branch with no issue number in its name |
| `unattributed:detached-head` | no branch was recorded (a detached HEAD, or none at all) |
| `unattributed` | from a source that never sees a branch: the proxy, or a vendor's billing feed |

`unattributed:main` can have a technical cause rather than a missing ticket.
Here is one. A developer starts an AI session in the main copy of the
repository, then has it work in a second working copy (a git "worktree")
checked out on a feature branch. In one installation's session files, examined
on 2026-09-26, Claude Code recorded the branch where the session started and
did not record a later `cd` into another worktree. Other installations and
Claude Code versions have not been checked. Helper agents that Claude Code runs
in worktrees of their own can land on `main` the same way;
[Attribution](#attribution--how-spend-is-tied-to-work) in Layer 3 explains how.

The remedy is to start the session inside the worktree, on a branch whose name
carries the issue number. A worktree on a branch without one moves the spend to
`unattributed:branch-without-issue` instead. The fix applies to future sessions
only: spend already recorded keeps the label it was given. `tierd doctor --repo .`
reports how much of your spend it could link, so you can check whether the fix
worked.

### The top half: outcome points

Only **merged** pull requests count, and only those that name an issue, either
in the branch name or in a line such as `closes #42` in the description. A
merged pull request with no issue reference is skipped, because its work cannot
be joined to its spend. "Merged" means the merge was recorded (by GitHub, or
through TIER's outcomes API for other trackers). It does not mean deployed.
Unmerged work adds cost and no points.

Each merged pull request earns points from its size label, on a fixed scale of
five sizes:

| Label | Weight |
|---|---|
| `xs` | 0.5 |
| `s` | 1 |
| `m` | 3 |
| `l` | 5 |
| `xl` | 8 |

A person, or your own automation, puts the label on the pull request in GitHub.
A pull request with no recognised label is sized from its diff instead, on the
same five weights.

The weights are **locked**. You can rename the labels in the config file, but
TIER refuses any weight not on this scale. The lock keeps your own history
comparable: a weight change would silently rescale every earlier period. Fixed
weights are also one precondition for ever comparing two installations, but
only one. Both would also need the same price-table version, the same version
of the scoring rules, and the same labelling habits. Until you have all of
that, compare a team with its own history.

Each pull request's weight is then multiplied by a **quality multiplier**
between 0.1 and 1.0 when it comes from GitHub (an outcome sent through the
outcomes API carries the quality its sender set, from 0 to 1). It is 1.0
unless something goes wrong after the merge and GitHub's webhook reports it;
without the webhook nothing lowers it after the merge.
A failing GitHub Actions run (the automated build and tests) on the merge commit within 48
hours lowers it to 0.7, unless a re-run of the same check soon passes. A revert
within 60 days lowers it to 0.8 when the revert was a business decision, or to
0.1 when it was a code problem. TIER tells the two apart from keywords in the
revert's human-written commit message, and treats a revert as a code problem
unless the message gives only business reasons. The worst of these wins. Failed checks *before* the merge do not count. A revert
can arrive weeks after the merge, so a past period's score can still move. The
numerator is `Σ(weight × quality)`.

### Three things that will bite you

**1. Task mix moves the number, and the number cannot separate it from
skill.** A quarter spent building new features and a quarter spent chasing
production bugs will not score the same. The number cannot say how much of
the gap comes from the work and how much from the people. Compare a team to its
own history, not to another team's number.

**2. Small groups are anonymized, by design.** In the `team` and `division`
modes, TIER groups developers by the team (or division) the operator assigns
each one to. A group with fewer *counted* people than the anonymity
floor (five by default, never below three) is folded into one catch-all row
named `other`, along with developers who have no team. Since #856 the count is
of people, not IDs: an ID counts only if it is on the roster during the window
(after its aliases are joined, see
[Attribution](#attribution--how-spend-is-tied-to-work)), is not a bot, and has
activity a collector, the proxy or a pull request captured in the window. Each
figure also needs its own floor: a group's cost is shown only if enough counted
people in it have captured spend, its points only if enough have merged work,
and its paid spend only if enough have paid spend; the fidelity split
(`coverage_pct`) needs enough people behind each part of it. [manager-faq.md](manager-faq.md) has the full rule and how to bring
uncounted people back. Folding drops nothing: the folded spend and points stay in `other` and
in the totals. If `other` is itself below the floor, it is withheld along with
the totals, because subtracting the named rows from the totals would reveal
it, and TIER says so rather than showing an empty page. If you are evaluating
TIER on your own, pick `developer` mode (`tierd serve --aggregation developer`):
in `team` or `division` mode a group of one is always folded away and you will
not see your own row. The full command is in
[quickstart.md §5](quickstart.md#5-the-full-tier-score).

**3. The two halves must cover the same stretch of work.** Both halves are
counted over the same calendar dates. TIER does not pair each merged pull
request with the exact spend that produced it. Spend is recorded when it
happens, and points arrive later, at merge. Two things follow:

- A window that ends too recently reads **low**. Its spend is in, but some of
  its work has not merged yet.
- A window that starts before TIER began recording spend reads **high**. That
  starting point is your installation's *cost horizon*. Work merged before it
  is counted, but its spend was never recorded, so it looks free.

On one real multi-repo installation, measured on 2026-07-26, the API's default
90-day window gave a number about twice as high as the trailing 30 days, which
roughly matched the period whose spend had been recorded. TIER warns about this
second case, with a dashboard banner and a `tierd doctor` check, but it still
computes the number. There is
no fixed safe length. In developer mode, the rule of thumb is a window several
times longer than your usual time from first AI use to merge, starting after the
cost horizon. In team and division mode the window is a sealed month: read
months that start on or after the cost horizon; reading an earlier one, the
dashboard banner names the first such month.
See [interpreting-the-number.md](interpreting-the-number.md#practical-guidance).

**Next:** [The full method, page by page](#layer-3--the-full-method-page-by-page).
Before you rely on any score, read
[interpreting-the-number.md](interpreting-the-number.md), which covers windows,
skew and when a score is too early to trust.

---

## Layer 3 — The full method, page by page

Layer 2 gave the formula, the weights and the multiplier values. For each step,
this layer says what the program does, gives each rule's key numbers in brief
and links the page that owns it, so each rule has one copy to keep correct. For a code-grounded account of what the tool does and does not do
today, read [how-it-works.md](how-it-works.md) alongside it.

### From install to a first real score

This is the single-developer path. Each step is in
[quickstart.md](quickstart.md) with its full output.

1. **Install.** Go 1.26.9 or newer (or 1.27.2+; Go 1.27.0–1.27.1 carry the same advisories), or a release download:
   [quickstart.md §1](quickstart.md#1-install).
2. **Check capture.** In your repository's folder, run `tierd doctor --repo .`.
   It reports what TIER can read on this machine and how much of your spend it
   could link to an issue.
3. **See the cost side.** `tierd score --repo .` prints cost per issue from
   your local session files. It needs no server and no GitHub access. It shows
   cost only, not a TIER score. It reads Claude Code sessions only; Codex,
   Opencode and Muse Code spend is picked up in steps 5 and 6, with the flags
   named in step 6.
4. **Load merged work.** `tierd backfill --repo <owner>/<name> --token @<file>`
   reads your merged pull requests from GitHub. `<owner>/<name>` is the
   repository as it appears in its GitHub URL. `<file>` is a file holding a
   GitHub token that can read the repository's pull requests;
   [README § What you need before you start](../README.md#what-you-need-before-you-start)
   shows how to create one.
5. **Serve.** `tierd serve --aggregation developer --watch-repo <path>`, where
   `<path>` is the folder of your local checkout. Leave it running. No token
   is needed because none was set; without one, the server accepts
   connections only from this machine, unless something forwards that port,
   such as an SSH tunnel or a reverse proxy.
6. **Load past spend.** In a second terminal, run
   `tierd ship --server http://127.0.0.1:8080 --repo <path>`.
   `--watch-repo` only picks up new activity, so without this step the score
   divides up to 90 days of merged work by only the spend in session files
   written to after step 5, and reads too high. `ship` sends the last 90 days of session files still on
   your disk. `--server` is the address the server from step 5 listens on,
   and `<path>` is the same checkout folder. It needs no `--api-token`,
   because the server has none set. `ship` reads Claude Code sessions only,
   unless you add `--codex-rollout` for Codex, `--opencode` for Opencode or
   `--muse` for Muse Code; add the same flag to the step 5 `serve` command.
   Without it, a run on a repository where you used only one of those tools
   finds nothing and stops with an error that blames the `--repo` path.
7. **Join your two names.** Spend from steps 5 and 6 is recorded under your
   computer's login name, which `echo $USER` prints. Merged work from step 4
   is recorded under your GitHub login, which your GitHub profile page shows,
   or which `gh api user -q .login` prints if you have the GitHub CLI. If the
   two are the same, skip this step. If they differ, you appear twice in the
   scores: once with spend and no merged work, once with merged work and no
   spend. Tell the server they are one person:

   ```sh
   curl -f -X POST http://127.0.0.1:8080/api/v1/developer_alias \
     -H "Content-Type: application/json" \
     -d '{"alias":"<login-name>","canonical":"<github-login>"}'
   ```

   `<login-name>` is what `echo $USER` printed and `<github-login>` is your
   GitHub login. No token header is needed, because the server has none set.
   The server answers `201 Created` with an empty body, so the command prints
   nothing when it works. The join also applies to spend and work already
   loaded. The team-server form is in
   [README § Identity mapping](../README.md#identity-mapping).
8. **Open the dashboard.** Open `http://127.0.0.1:8080`, or read the same
   scores as JSON with `curl -f http://127.0.0.1:8080/api/v1/scores`.

Everything is stored in one SQLite database file on the machine running the
server, `~/.tier/tier.db` by default. For a shared team server, with tokens,
the GitHub webhook, and spend sent in from each developer's laptop, see
[README § Run it for a team](../README.md#run-it-for-a-team).

### Capture — how spend gets measured

TIER is **JSONL-first**. The primary path reads the session files the tool
already writes locally. On that path no proxy sits in front of your model
provider and no request is intercepted. An optional reverse proxy exists for
tools that write no usable local log, and open-weights and self-hosted models
have their own capture path. On a team, each developer runs `tierd ship` on
their own machine. It reads the local session files and sends the cost
records to the team's server; this is the *laptop-shipper* setup.

- Capture paths and the laptop-shipper topology — [README § Capturing tokens](../README.md#capturing-tokens)
- Self-hosted and open-weights models — [open-weights-capture.md](open-weights-capture.md)
- What is read, stored, and never touched — [privacy.md](privacy.md)

### Pricing — how tokens become dollars

`ComputeCost`, the function that turns an event's token counts into dollars,
prices each event against the table in `internal/store/prices.yaml`. That file
is compiled into the program and is the single source of truth. It is
versioned as one unit: any price change bumps the table's single version
number. Each cost is fixed when it is recorded and stamped with the version
that priced it, so upgrading TIER does not reprice history. `tierd reprice` is
the explicit, audited way to do that, and a window whose costs span several
versions is flagged.

Before lookup, a model name is lowercased and a trailing date stamp (plus
`-preview` or `-latest`) is removed. Dated snapshots of one model therefore
share a price, while differently named versions keep their own. A model not in
the table is priced by a guess. If its name carries a parameter count (such as
`70b`), it gets a self-hosted size-class rate. Otherwise it gets the flat
self-hosted-medium rate of $0.50 per million tokens. Either way the server
writes a one-time warning per model name to its log and counts the event in the
`tier_unknown_model_events_total` metric. Open-weights models are priced by
model *and* by the host serving them, because the cost belongs to the host,
not the weights.

- Why list prices, not invoices — [pricing-philosophy.md](pricing-philosophy.md)
- The table, and §4's list of corrections to older copies of it — [reference-price-table.md](reference-price-table.md)
- Normalization rules and the target governance model — [cost-normalization-spec.md](cost-normalization-spec.md)

### Attribution — how spend is tied to work

Spend is joined to an issue through the issue reference carried by the
session's branch. Merged work is joined through the pull request's branch name
or a `closes #N` line in its body. Cost is attributed to your **shipping
identity**: the developer name stamped on your cost records, which is your
computer's login name unless you override it. Outcomes are attributed to the
**PR author's GitHub login**. When those differ, they must be mapped to one
developer (an alias, recorded through the API), or cost and outcomes land in
separate rows and cannot be paired. The mapping is applied when scores are
read, so it corrects past windows too. Developers who appear on only one side
are reported in the response's `unjoined_developers` note, by name in
`developer` mode and as a count otherwise. The anonymity floor counts the IDs
mapped to one person once, and only if that person also meets the rest of the
rule in [Three things that will bite you](#three-things-that-will-bite-you)
(item 2), so load the alias map before relying on it.

Claude Code can run helper agents in worktrees of their own, on branch names
it makes up (`worktree-agent-` followed by a code). Those names carry no issue
number, so TIER uses the last branch a person named earlier in the same session
file. If that branch was `main`, the helper's spend lands on `main` too.

- Branch, label, and issue-reference conventions — [conventions.md](conventions.md)
- Identity mapping — [README § Identity mapping](../README.md#identity-mapping). It carries both the team-server form and the tokenless laptop form (`http://127.0.0.1:8080`, no `Authorization` header). The tokenless form works only against a server started with no token, which then accepts connections from the same computer alone; anyone logged in to that computer can use it too, as can anything that forwards its port.

### Scoring — how outcomes become points

The canonical rubric defines the fixed `{0.5, 1, 3, 5, 8}` size scale, the
label taxonomy, the fallback for a pull request with no recognised label, and
the quality multiplier derived from quality events. The fallback sizes a pull
request from its diff: lines changed plus ten for each file changed, bucketed
onto the same five weights.

Two checks decide whether a score has enough behind it to read. A row is only
treated as sound once it has at least 3 outcomes and at least $5 of list-price
spend; below that it is still shown, marked as below the evidence floor. A
merged pull request is flagged when its developer recorded fewer than 1,000
tokens against its issue in the 14 days before the merge: its points still count, but its developer's row is
marked as not sound, because TIER cannot tell whether the work used AI it
never saw, was badly attributed, or was genuinely done without AI. Direct commits to the main branch, with no
pull request, count only if the operator turns on `--push-capture`, and then at
the smallest weight, 0.5.

- The canonical rubric, and why it defines no absolute good/bad band — [rubric.md](rubric.md)
- The quality multiplier's degradation rules — [quality-degradation-spec.md](quality-degradation-spec.md)
- Outcome ingest for teams not on GitHub — [outcomes-api.md](outcomes-api.md)
- GitHub webhook wiring — [webhook-setup.md](webhook-setup.md)

### Reading the result

There is no good or bad value. The rubric deliberately defines no absolute
band, so your first settled window is your baseline, and later windows are
read against it.

Two notes in the `data_quality` block of the `GET /api/v1/scores` response say
when a score needs care, and the dashboard shows each as a notice.
`window_predates_cost_capture` is `true` when the window starts before the cost
horizon. In `developer` mode, start it at `cost_coverage_safe_since` instead
(the dashboard's **From** date, or `?since=` on the API). In `team` and
`division` mode the server reports whole sealed months and refuses `?since=`,
so there is no `cost_coverage_safe_since`; pick a later sealed month that
begins at or after the cost horizon, in the dashboard's month picker or with
`?period=YYYY-MM` on the API. A month that is not sealed yet answers `404`;
when the month will become sealable, the answer gives the time it becomes
eligible (`sealable_at`), and a month before the earliest sealable one, or one
the sealer skipped, answers without it. `kanon_suppressed` says the anonymity
floor withheld the `other` row and the totals.

- Honest reading, windowing skew, and when a score is too early to trust — [interpreting-the-number.md](interpreting-the-number.md)
- Where TIER sits alongside DORA (the DevOps Research and Assessment delivery metrics), SPACE (a developer-productivity framework) and DX Core 4 (which folds DORA, SPACE and DevEx into four dimensions) — [how-tier-relates.md](how-tier-relates.md)
- Moving practices between people without ranking them — [peer-learning.md](peer-learning.md)

### The limits, stated plainly

TIER can be gamed. The known ways are documented: label inflation, splitting
pull requests, picking cheap work, and several failure modes that are
structural rather than deliberate. Read
[adversarial-analysis.md](adversarial-analysis.md) before the score is used
anywhere it could affect how people are treated.

Measuring developers individually is regulated in some jurisdictions. Two
separate kinds of rule apply. Works-council co-determination rules, such as
Germany's §87 BetrVG, can require the works council's agreement before a
monitoring tool is introduced. GDPR Article 22 restricts decisions about a
person made solely by automated processing. Read
[legal-and-privacy.md](legal-and-privacy.md) **before** you pick a reporting
mode, whichever you pick. `developer` aggregation carries the heaviest
obligations, but that page also lists works-council consultation and a data
protection impact assessment (DPIA) for `team` mode in several countries.

In `developer` mode, anyone who can read the server today sees every
developer's row. That means anyone
who holds one of its tokens (the administrator token or the read-only viewer
token), or, when no token is set, anyone on the same computer, plus anyone
reaching it through something that forwards its port, such as an SSH tunnel or
a reverse proxy.

### Interfaces

- `/api/v1` compatibility contract — what is pinned and what may change — [api-compatibility.md](api-compatibility.md)
- Operator security model — [security.md](security.md)
- Full documentation index — [docs/README.md](README.md)
