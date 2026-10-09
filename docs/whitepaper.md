# TIER — Token Impact & Efficiency Ratio

## A white paper: accepted software changes per dollar of recorded AI usage

**Paper version:** 2.2 (2026-10-01). Describes TIER **v0.5.2**, the first public release of the v0.5 line, an open-source tool released under the Apache License 2.0. The paper is written for two readers with no background: a skeptical engineer and a chief financial officer (CFO). It says what TIER measures, what the number is worth, which decisions it may and may not inform, how complete its inputs are, and how to try it. The mechanics behind every statement here are in the companion [technical reference](technical-reference.md); Section 13 says what it covers. Where this paper and the reference disagree, the reference is correct.

**Two roles.** The **operator** is the project maintainer, who decides disputed design questions; the rulings this paper relies on are stated in full where they apply. Issue numbers (#826 and others) point to the maintainer's issue tracker, which may not be public; they are provenance only, and nothing here requires opening them. Whoever installs and runs TIER is the **administrator**, usually an engineer. A CFO does not install or run TIER; finance's part is posting what was paid (Section 8).

---

## 0. Summary for finance and leadership

**What the number is.** TIER counts accepted software changes, weighted by size and by what happened after merge, per $1,000 of AI usage priced at the providers' published list rates (a manual import path is the one exception; Section 2.1). `cost_per_point` is the same ratio in dollars per point. An accepted change is a merged pull request, a direct commit to the repository's default branch when the administrator turns on that option, or an outcome posted through TIER's application programming interface (API); a pull request or commit counts only if it names an issue number (Section 2).

**What a point is worth.** A small change that merged with no failure afterwards is one point; the standard sizes run from 0.5 to 8. A point has **no dollar value** (Section 3).

**What the headline can show.** On the dashboard the headline is a number only when the figure clears an evidence floor. Otherwise it shows a word: "NO SCORE" (nothing accepted), "FREE" (accepted work but no recorded cost), "NOT ENOUGH SPEND TO SCORE" or "NOT ENOUGH EVIDENCE TO SCORE". A number labelled "provisional" rests mostly on spend that could not be linked to a piece of work (Section 5).

**How a figure may be compared.** Mainly with the same team's or organisation's own history, for one kind of work (per kind only in the per-developer mode; Section 9), under the same price table and scoring rules. Two teams in one organisation compare only weakly. Two organisations never compare (Section 6).

**What it may inform.** How AI is used: which model or route handles which work, whether a tool trial went with cheaper accepted work, whether a team's change in practice moved its cost per point, and whether a flat-fee plan's recorded usage is worth its fee at list prices. A manager may look at an individual developer's numbers in order to coach them (Section 7).

**What it must not inform.** By the operator's ruling of 2026-09-26: pay, compensation, promotion, performance reviews, performance improvement plans, forced ranking, discipline or dismissal. This paper adds four more: comparisons between organisations, comparisons between kinds of work, headcount or hours-saved claims, and any return on investment stated in dollars. TIER cannot enforce any of this; the organisation's own policy must (Section 7).

**How complete the cost side is today.** TIER sees only the usage it captures (Section 4). By default it reads Claude Code's session logs. The administrator can turn on capture for Codex (OpenAI's command-line coding agent), for Opencode (an open-source coding agent) when it uses the Z.ai coding plan (Z.ai sells the GLM models; the coding plan is its flat-fee subscription), for Muse Code (Meta's command-line coding agent; its token mapping was measured on one real Muse Code 1.4.0 session), and for organisation-level API usage reported by Anthropic and OpenAI. A built-in proxy records any calls a client chooses to send through it. Cursor, GitHub Copilot and ChatGPT Team are not captured. Usage TIER does not capture looks free, so TIER reads high (when any cost is recorded at all). One doubt is open: a preliminary third-party measurement reports that Claude Code's own logs undercount tokens, output by 10–17× and input by 100–174×. If it holds, TIER's Claude Code cost is too low, so TIER reads too high, cost per point too low and Spend Leverage too low. The direction is known; the size is not. The project is measuring it (issue #837), and as of 2026-09-26 it is unsettled.

**Spend Leverage** is the list-price value of recorded usage divided by what finance posted as paid (Section 8). Above 1, the organisation paid less than list; near 1 is metered billing at list; below 1, it paid more than list, for example for an under-used flat plan. It is not a saving, and it never changes TIER.

---

## 1. The problem

AI bills report spending, not the software changes that spending produced. Two teams can spend the same amount and merge very different amounts of work, and some spend goes to work that never merges.

The common engineering frameworks do not cover this. DORA (DevOps Research and Assessment) measures delivery speed and stability; SPACE (satisfaction, performance, activity, communication, efficiency) and DX Core 4 (speed, effectiveness, quality, impact) are frameworks for developer productivity and experience. As the project reads their published definitions, none uses dollars of AI spend as a denominator. TIER adds a measure in dollars of AI spend, for use alongside them.

A useful measure has to deal with four things. Tokens are not comparable across models: the same token count costs very different amounts on an expensive and a cheap model. Invoices are not comparable across organisations: discounts, credits and subscriptions make identical workloads bill differently. Merged work is not comparable by count: a one-line typo fix and a multi-week subsystem are both one pull request (PR). And a number tied to a person's incentives can be gamed. Sections 2 to 6 deal with the first three; Sections 7 and 10 with the fourth.

---

## 2. What TIER measures

### 2.1 The formula

```
TIER = Σ(outcome_weight × quality_multiplier) / (total_AI_cost_USD / 1000)
```

**Unit:** weighted outcome points per $1,000 of list-price AI cost. A TIER of 100 means 100 weighted points for each $1,000 of AI usage. When the window has no recorded cost the formula would divide by zero, so TIER reports 0 instead (Section 2.4).

- **Accepted outcomes.** The numerator counts three kinds: a merged pull request; a direct commit to the repository's default branch, when the administrator turns on push capture; and an outcome posted through TIER's outcomes API, where the caller asserts the merge. Each merged PR is its own outcome, even when several share an issue. A PR that is closed without merging, abandoned or still open adds nothing to the numerator, but its AI cost stays in the denominator. A merged PR counts only if its branch name or description names an issue (for example a branch `feature/42-login`, or `closes #42` or `#42` in the description); one that names none records no outcome at all. A direct commit needs an issue number in its message and always weighs 0.5.
- **Weight** is the change's size: 0.5, 1, 3, 5 or 8, from a size label on the PR (`size/xs` or `xs`, through `size/xl` or `xl`; Section 3) or, with no label, from the number of lines and files changed. An outcome posted through the API may instead carry any weight above 0 up to 8.
- **Quality multiplier** starts at 1.0 and is lowered by what happens after merge: to 0.7 when continuous integration (CI) fails on the merge commit within 48 hours (a quick re-run that passes clears it), to 0.8 when the change is reverted within 60 days for a business reason, and to 0.1 when it is reverted within 60 days for a defect. TIER tells the two kinds of revert apart by keywords in the revert commit's message ("no longer needed" reads as business; "broke" or "bug" as defect, and a message with neither counts as a defect). When several apply, the lowest wins. These signals come only from GitHub, so outside GitHub an outcome keeps the quality it was posted with (1.0 unless the caller sent another value).
- **Cost** is all AI usage recorded in the window, priced at list rates by TIER itself, with one exception: a script posting cost through the manual cost API supplies its own dollar figure (Section 4.1). It covers accepted work, abandoned work, and spend that could not be linked to any issue.
- Team and organisation figures are **summed points over summed cost**, never an average of individual ratios. If A has 10 points on $10 (TIER 1,000) and B has 1 point on $100 (TIER 10), the average of the two ratios is 505, while the pooled figure is 11 ÷ $110 × 1,000 = 100.

The weights, quality values and time limits are the project's rubric choices; it publishes no study behind them. The **rubric version**, reported with every score, is the number that changes when those choices change.

### 2.2 A worked example (illustration)

Suppose a developer merges three pull requests in a window, and $35.00 of list-price AI spend is recorded under that developer in the same window.

| PR | Size label | Weight | What happened after merge | Quality | weight × quality |
|---|---|---|---|---|---|
| A | `size/m` | 3 | nothing | 1.0 | 3.0 |
| B | `size/s` | 1 | CI failed on the merge commit within 48 h | 0.7 | 0.7 |
| C | `size/l` | 5 | reverted 20 days later because the feature was "no longer needed" | 0.8 | 4.0 |

Weighted points = 3.0 + 0.7 + 4.0 = **7.7**. TIER = 7.7 / (35.00 / 1000) = 7.7 / 0.035 = **220** points per $1,000. `cost_per_point` = 35.00 / 7.7 = **$4.55 per point** (and 1,000 / 220 = 4.55, the same figure from the other direction).

### 2.3 How the pieces fit together

1. **Capture.** TIER reads the token counts that AI coding tools already record (Section 4.1): model, tokens by kind, the time, the git branch and the working directory. It never reads the text of prompts or responses from those logs.
2. **Price.** Each call's tokens are multiplied by the list rates for that model and summed to one dollar figure per call, rounded once to the millionth of a dollar.
3. **Link.** The call is linked to a piece of work through the issue number in its git branch (for example `feature/42-login` gives issue 42), or recorded as unlinked.
4. **Record outcomes.** Merged PRs arrive from GitHub as they merge, or from a one-time backfill of history, or from any other system through the API, each with its issue number, size and later quality.
5. **Score.** For a window, TIER adds up points and cost per developer, per team and overall, and divides.

### 2.4 The companion figures

- **`cost_per_point`** = cost ÷ points, in dollars per weighted point. With zero points it is empty (`null`), so "nothing accepted" never reads as the most efficient score. With points but zero recorded cost, TIER is 0 and `cost_per_point` is 0. Those two zeros mean "no recorded cost", not the least or the most efficient result; the dashboard shows the case as "FREE", and neither number should be read.
- **Spend Leverage** = list-price cost ÷ what was actually paid (Section 8).

### 2.5 Which figure a decision uses

The **pooled** figure divides the window's points by all of its recorded cost, including abandoned work and unlinked spend. TIER also reports a **segment** for each kind of work (`feature`, `bug`, `security`, `incident`, `tech-debt`, `research`, `compliance`, read from a PR label equal to that name or prefixed `type:` or `kind:`; `feature` when there is none). A segment divides that kind's points by the cost linked to that kind's outcomes only, so it leaves out abandoned work and unlinked spend, and it usually reads higher.

The segment answers "what did accepted work of this kind cost"; the pooled figure answers "what did we spend per accepted point". A decision about one kind of work compares its segment across two windows, and reads beside it how much spend went to work with no outcome and to spend with no issue (both are in the `segment_reconciliation` block of `GET /api/v1/scores`), because moving spend into abandoned work lowers the pooled figure and leaves the segment unchanged. Do not compare one kind of work with another: nothing shows that their points represent equivalent work. Segments are reported only in the per-developer mode; team and division reports leave them out (Section 9).

---

## 3. What a point is and is not worth

A weighted point is a unit of accepted change, sized by a label or by the size of the diff and reduced by a CI failure or a revert. "Clean" here means that no failure was recorded, not that success was verified.

A point has **no dollar value**. TIER does not measure revenue, customer value or engineering hours. It does not separate what the AI contributed from what the developer did: a change written entirely by hand still counts as a point. There is no good or bad level, and no benchmark: a TIER of 40 is neither good nor bad on its own. What the number can say is how many points of accepted work went with each $1,000 of recorded AI spend, and how that changed over time for the same people doing the same kind of work.

The size labels mean:

| Label | Weight | Meaning |
|---|---|---|
| `size/xs` | 0.5 | trivial: a one-line fix, a typo, a config flag, a dependency bump |
| `size/s` | 1 | small: a localised change to one function or file |
| `size/m` | 3 | medium: a self-contained feature or fix across a few files |
| `size/l` | 5 | large: a feature touching several components, or a refactor with migration |
| `size/xl` | 8 | extra-large: a subsystem or multi-day effort landed as one PR |

With no label, TIER computes effort = lines added + lines deleted + 10 × files changed, and maps it to 0.5 (up to 15), 1 (up to 60), 3 (up to 200), 5 (up to 1,000) or 8. A 50-line change across 3 files has effort 80 and weight 3. The diff heuristic cannot tell generated or vendored files from written code, so a label is the better input. TIER creates no labels; the team adds them in GitHub.

Because the weight comes from labels, a team that labels generously scores the same work higher than one that labels strictly, and nothing in the software can see the difference. A review of a sample of labels against the size meanings is the only check.

---

## 4. Where the cost comes from, and how complete it is

### 4.1 What is captured

TIER prices tokens with a **reference price table** built into the program: the providers' published list rates, including how they charge for cached input and very long prompts, but never the organisation's own invoice. Discounts and subscription fees are recorded separately, as paid spend (Section 8). Version 12 of the table, effective 2026-10-02, lists 91 models. A model the table does not list is priced at a rough rate the project chose ($0.10 to $2.00 per million tokens, by apparent model size), with a warning in the server's log.

Cost reaches TIER by these paths:

| Path | Default | What it sees |
|---|---|---|
| Claude Code session logs | on | Claude Code usage on the machine it runs on, for the repository being watched |
| Codex CLI logs | off | Codex usage, when the administrator turns it on |
| Opencode database | off | Opencode usage on the Z.ai coding plan only, when turned on |
| Muse Code session logs | off | Muse Code usage, when turned on; some subagent spend is left out, with a warning |
| Reverse proxy | listening | only the calls a client is configured to send through it; it stores usage counts, not request or response text |
| Organisation pollers | off | daily API usage from Anthropic's and OpenAI's organisation reports, for the part the paths above did not record |
| Manual cost API | available | whatever a script posts, at the dollar figure it gives |

A priced call, as an illustration using version 12's rates. `claude-sonnet-5` is listed at $2.00 per million input tokens and $10.00 per million output tokens; Anthropic charges cache reads at 0.10× the input rate and five-minute cache writes at 1.25×. A call with 200,000 input tokens, 1,500,000 cache-read tokens, 50,000 cache-write tokens and 5,000 output tokens costs 0.2 × $2.00 + 1.5 × $2.00 × 0.10 + 0.05 × $2.00 × 1.25 + 0.005 × $10.00 = $0.400 + $0.300 + $0.125 + $0.050 = **$0.875**. Cache reuse therefore matters: for the same tokens on the same model, less reuse costs more and scores lower, which is why the API reports the share of tokens that were cache reads beside the score.

A developer's laptop sends its captured cost to a central server with `tierd ship`. Cost is recorded under a **developer id**: the laptop's operating-system user name, unless `--developer` names another. Outcomes are recorded under the PR author's GitHub login. When the two differ, an alias joins them (Section 11), and every figure is then reported under the GitHub login.

### 4.2 What is missing, and in which direction it moves the number

- **Tools with no capture.** Cursor, GitHub Copilot and ChatGPT Team are not captured, nor is any other tool not listed above. Their usage looks free, so TIER reads high. If a window has no recorded cost at all, TIER is 0 instead.
- **Tools not turned on.** Codex, Opencode or Muse Code work done on a laptop whose `ship` does not include those tools records outcomes with no cost, so it too reads high.
- **The open doubt on Claude Code's logs (issue #837).** A third-party measurement (gille.ai, article dated 24 February 2026, labelled preliminary by its author) compared Claude Code's logged token counts with the totals Claude Code shows on screen and found the logs lower: output by 10–17×, input by 100–174×, even after duplicates were removed. If that holds for current Claude Code versions, every Claude Code dollar figure in TIER is too low. TIER then reads too high and Spend Leverage too low. The size is unknown, and TIER itself makes no adjustment for it. One mechanism narrows it for API-billed usage only: the organisation pollers, when turned on, add the gap they see between the provider's daily totals and what TIER recorded, as spend not linked to any issue; they cannot see usage on a subscription plan. Treat the doubt as open until a later version of this paper says otherwise.
- **Subscriptions.** A flat-fee plan's bill has no token counts, so it cannot be used to check TIER's token-based cost. Its fee is recorded as paid spend for Spend Leverage and never enters TIER.
- **Fast mode.** Anthropic's faster, dearer "fast mode" is not detected, so it is priced at the normal rate: cost reads low, TIER high.
- **Muse Code web search.** Meta charges for web searches per query, not per token, and TIER does not price them, so Muse Code cost reads low for sessions that searched.
- **Old logs.** Tools delete old session logs. The project's reading is that Claude Code keeps about 30 days by default, so a first run of TIER usually recovers about a month of cost. Work merged before that has outcomes but no recorded cost.
- **Scope.** The log readers take only sessions run inside the watched repository (or a git worktree of it). AI spend on planning or research in other folders is not captured by them; the proxy, pollers and manual API are not limited this way.

### 4.3 How to check before relying on a dollar figure

Confirm by hand that every AI tool the team uses is captured by some path. For API-billed usage, compare one month of TIER's cost with what the provider billed for the same usage. For subscription usage no such check exists. Prefer decisions that rest on a change over time within one capture path over decisions that rest on the absolute dollar level.

---

## 5. Reading a figure

### 5.1 The evidence floor

A figure clears the **evidence floor** when it has at least **3** outcomes, at least **$5.00** of recorded cost, and **no zero-token outcome**. An outcome is zero-token when its developer recorded fewer than 1,000 AI tokens on that issue in the 14 days up to the issue's latest merge in the window. That catches, without being able to tell them apart, AI work TIER never captured, cost and outcomes under two unjoined names, AI work done more than 14 days before the merge, work whose session logs are gone, and work written without AI. If Claude Code's logs undercount (Section 4.2), AI work that was done can also fall under the threshold. A team's figure applies the same test to its summed numbers, so **one zero-token outcome from any member takes the whole team's figure below the floor**. The thresholds are fixed in the code and are the project's judgement, with no published study behind them.

Below the floor the number is still computed and returned by the API; the floor changes what the number may claim, not the number. A developer figure that clears it gets a 95% confidence interval, found by resampling that developer's outcomes 1,000 times (in a rare case where every resample has zero cost, the interval reads 0 to 0). The API field that marks the floor is called `ranked`, a name kept for compatibility: nothing in TIER ranks anyone, and every list is shown in alphabetical order (a team view's `other` row last), never by TIER.

### 5.2 The words on the dashboard

The dashboard headline shows a word instead of a number in four cases, checked in this order (the first that applies wins):

| Word | Meaning |
|---|---|
| NO SCORE | the window has no accepted points; nothing accepted is the absence of a score, not a score of zero |
| FREE | accepted points but zero recorded cost; often a capture gap rather than free work |
| NOT ENOUGH SPEND TO SCORE | below the floor, with under $5.00 of recorded cost |
| NOT ENOUGH EVIDENCE TO SCORE | below the floor for any other reason |

Separately, when less than half of the window's cost could be linked to an issue, the headline is dimmed and labelled "**provisional** — N% attribution coverage", where N is linked cost as a percentage of the window's cost. Team and division reports do not publish that share, so their headline reads "provisional — coverage not shown". Provisional and the evidence floor are independent: a figure can have either, both or neither.

### 5.3 Linked and unlinked spend

TIER links spend to a piece of work through the issue number in the git branch the session ran on. Claude Code records the branch a session started on, so work started in the main checkout and done in a git worktree (a second checkout of the repository, on its own branch) is booked as unlinked. An opt-in switch, `--worktree-attribution`, off by default, links that spend through the worktree the session's tool calls worked in. It was released in v0.5.2 as a preview because its measured accuracy fell short, and an issue it stores for a message is never changed afterwards; read the reference, Section 5.1, and `docs/how-it-works.md`, section "Measured accuracy: read this before turning it on", before turning it on. Spend it cannot link is kept in the denominator as unattributed spend. Linking does not move the pooled figure by itself, because the denominator includes linked and unlinked cost alike. The figure reads **low** in one linking case: accepted work whose PR named no issue, which records no outcome while its cost stays in. That case often shares a cause with a low linked share, branch names without issue numbers, so a provisional label is a reason to check how many merged PRs recorded no outcome (`tierd backfill` reports the count). A merged PR that did name its issue is counted even if its spend was not linked; what is lost then is the pairing, which weakens the per-kind segments and can trip the zero-token test.

### 5.4 Windows

A figure covers a half-open window `[since, until)` in Coordinated Universal Time (UTC). The API defaults to the 90 days from the start of that UTC day, the dashboard to the last 30 days (its "From" date can be changed), both with no end. Cost is timestamped when it is spent (for log-captured cost, the time in the tool's log) and outcomes when they merge, so:

- **Recent, open-ended windows tend to read low**: work in progress has cost but no outcome yet.
- **Windows that start before TIER began recording cost read high** (when some cost is recorded): they count outcomes whose cost was never captured. The API reports `window_predates_cost_capture` and `cost_coverage_safe_since`, the UTC date of the earliest cost TIER holds for the whole installation. Starting a window on or after that date avoids this effect; it cannot show a developer or tool whose capture started later. Section 11 shows where to read it.

A past window can still move: a late `ship`, a backfill, a revert within 60 days of merge, or an administrator's correction. Compare two windows that have stopped moving, never a mature quarter with the current open one. Team and division reports have no free window: each is a sealed calendar month that never moves (Section 9).

### 5.5 Did it change?

For one developer's pooled figure, TIER marks a change between two windows as `significant` only when both windows clear the floor and their 95% intervals do not overlap. This is a fixed rule, not a validated statistical test: the project has not studied how often the intervals are right, and nothing corrects for making many comparisons at once. Team figures carry no interval, so a change in them is directional only. A developer's per-kind segment carries its own interval, but the two-window comparison cannot filter by kind, so a per-kind change has no `significant` test either.

---

## 6. The comparison rule

1. **The main comparison is with your own past**: one team or organisation against its own history, for one kind of work (per-developer mode only: team and division reports publish no per-kind figures).
2. **Same prices, scoring rules and release.** The two figures must report the same price-table fingerprint (`price_table.table_hash`) and the same rubric version (`rubric.version`), both in every `GET /api/v1/scores` response; come from the same TIER release (`tierd version`, or `GET /api/v1/livez`); and neither may report a mix of price versions (`data_quality.mixed_price_versions` in the same response, which only the per-developer mode reports). The fingerprint describes the price table loaded now, not necessarily the one that priced older rows, so after a price-table change the reference (Section 2.1) explains how to confirm older rows.
3. **Two teams in one organisation compare weakly**: only within one kind of work (so only in the per-developer mode) and one window, after a review of a sample of each team's size labels, and still subject to their different task mix. Treat the difference as a question to ask, not a finding.
4. **Two organisations never compare**, even on the same price table. List prices put their cost on one price basis, but their labelling, task mix and capture coverage differ.
5. **One kind of work never compares with another.**

---

## 7. What the number may and must never inform

### 7.1 Coaching, not evaluation

TIER is meant for coaching how AI is used: which model for which work, how context and cache are reused, and whether spend starts from an issue. By the operator's ruling of 2026-09-26 (issue #826), a manager **may** look at an individual developer's numbers, in the per-developer mode (Section 9), in order to coach: to discuss that practice with the developer. The ruling also prohibits using the numbers to evaluate a developer. They **must never be used to evaluate the developer: not as an input to pay, compensation, promotion, individual performance reviews, performance improvement plans, forced ranking, discipline or dismissal.** TIER cannot enforce that line; only an organisation's own policy or works agreement can.

There are two reasons. The first is the project's reading of the law, not legal advice: in Germany, per-developer measurement is subject to works-council co-determination; in France and the Netherlands it requires the works council's consultation or consent; the project reads most of the European Union and European Economic Area (EU/EEA) as similar; and using the output alone to drive a human-resources decision would likely engage Article 22 of the General Data Protection Regulation (GDPR). The project expects a data protection impact assessment (DPIA) for any per-developer deployment, and for measured employees in the EU/EEA recommends team-only reporting plus advance consultation. The second reason is method: a per-developer figure moves with task mix, seniority, pairing and review load, and a number tied to a person's evaluation invites gaming.

**Who can see a developer's numbers today.** In v0.5.2, access is not per viewer. In the per-developer mode, anyone with the server's read token (or, on a server run without tokens, anyone who can reach it) sees every developer's figures. In team and division modes the read token shows only groups, but the admin token can still export one named developer's records, and anyone who can restart the server in the per-developer mode or read its database file sees everything (Section 9). Planned, and not in v0.5.2, with no release date published: separate tokens for the developer, their declared manager and an optional skip-level viewer; a log of who viewed each developer's page, readable by that developer; and in the per-developer mode, deletion after 90 days by default with a 365-day ceiling (the operator's stated reason: a full annual coaching cycle, not annual reviews). Until then, treat the read token in the per-developer mode as access to everyone's data.

### 7.2 Decisions it can inform

1. **Model or routing choice.** Change the default model or route for one kind of work, then compare that kind's cost per point across two matched windows (per-developer mode only).
2. **A tool trial.** Give a group a new AI tool for a window and compare its cost per point with its own previous window. This works only if the new tool is captured; if it is not, its spend reads as zero and the trial looks free.
3. **A team's trend.** Whether one team's cost per point moves after a change in practice, such as linking branches to issues or reusing context.
4. **Whether a flat plan's usage is worth its fee.** Spend Leverage below 1 for several months means the fee was more than the list-price value of the usage TIER recorded. It does not say what metered billing would have cost, because usage might have differed under it. **This is the decision most exposed to TIER's blind spots.** On a subscription, TIER has no bill to check its token counts against (Section 4.2), and if the Claude Code undercount doubt (#837) holds, the recorded usage, and so leverage, is too low: a plan could look under-used when it is not. Do not cancel or downgrade a plan on this figure alone; set it beside the provider's own usage reports.

Before reading a change as an efficiency gain, rule out four ordinary causes: a shift in the mix of work, a change in how generously labels are applied, a change in which tools are captured, and a window edge.

### 7.3 Decisions it must never inform

Any evaluation of a person (the ruling above); comparisons between organisations or between kinds of work (Section 6); headcount or "hours saved" claims, because TIER measures no human effort; and any return on investment stated in dollars, because a point has no dollar value.

---

## 8. Spend Leverage

**Spend Leverage = list-price cost ÷ actual paid.** Finance's input is what was paid: a US-dollar amount per calendar month, posted either per developer (`POST /api/v1/actual_spend`) or for the whole organisation (`POST /api/v1/org_actual_spend`), by the administrator or with a token the administrator provides. An organisation-level amount, less any per-developer amounts for the same month, is split equally among the other developers enrolled with that organisation in TIER's team map (Section 9). The interface, credits and corrections are in the reference, Section 8.1.

A window takes the full paid amount of each month from the month containing its start up to, but not including, the month containing its end; with no end, every month from the start onward. So only windows aligned to whole months line up the two halves; a window inside a single month has no paid amount at all, and the dashboard's default 30-day window, which has no end, does not line up.

- **Above 1**: the organisation paid less than list for the recorded usage (a discount, a commitment, a subscription). As an illustration, usage worth $3,280 at list on a plan that cost $200 for the month is 3,280 / 200 = 16.4×.
- **Near 1**: metered billing at list; discounts, capture gaps and how seats are counted can each move it.
- **Below 1**: the organisation paid more than list, for example for an under-used flat plan.
- **Shown as "—"**: no payment posted, or a net of exactly zero; when credits exceed payments the dashboard tile reads "(credit)".

A high figure on a flat plan says what the plan bought at list prices; it does not say what metered billing would have cost, so it is not a saving. Leverage also inherits every capture gap: unrecorded usage lowers it, and a bill that covers tools TIER does not capture lowers it too. It never changes TIER. 

---

## 9. Privacy and reporting modes

The server must be started in one of three **reporting modes**, chosen with `--aggregation`, with no default:

- **`developer`**: one row per developer id. This is the coaching mode of Section 7.
- **`team`** and **`division`**: rows per team, or per division (a group of teams one level up), from TIER's **team map**: a table the administrator loads that gives each developer id a team, a division and an organisation. A group is named only when at least **k** people count toward it (**k-anonymity**; k is 5 by default and never below 3). A person counts when they are in the team map, are not a bot, and have AI spend that TIER captured or a merged pull request in the period; one person's several ids count once when an alias joins them (Section 4.1). Each figure also needs its own k: a group's cost is shown only if k counted people in it have captured spend, and its points only if k have merged work. Smaller groups are folded into a row called `other`, and when `other` itself is too small the whole report is withheld, named rows included, and the response says so. These reports show only the group rows and the total, with no per-kind segments, no spend by model and no attribution share, because a second breakdown of the same data can be subtracted from the first to reveal a smaller group. Spend that belongs to no person, such as the organisation pollers' remainder, is left out of their figures.

**Sealed monthly reports.** In `team` and `division` modes a report covers one calendar month, because two overlapping date windows can be subtracted to expose a group smaller than k. Each month is computed once, after it has closed, a grace period (14 days by default) has passed and every configured cost source has reported it; it is then stored and served unchanged, and data that arrives later never alters it. Nothing is reported until the administrator arms sealing, once, after loading any history. Sealing is one-way: a sealed month is never recomputed. The reference, Section 6.2, has the commands. The `developer` mode is unchanged and still serves any window.

The mode changes only what the server **shows**, not what it **stores**: every mode stores per-developer rows, and restarting in `developer` mode shows them all, including history collected under `team` mode. Anyone who controls the server's startup, or can read its database file, can see individual rows whatever the mode.

TIER's log readers read only metadata and token counts, never prompt or response text. Two paths differ: the proxy forwards complete requests and responses to the provider but stores only usage counts, and the GitHub webhook stores each raw delivery, which can include commit messages and author names and emails. Those deliveries are cut to the last 90 days when the database is opened and every 24 hours while the server runs. Cost and outcome records are never deleted by age. An erasure endpoint removes one developer's records, with limits the reference lists (Section 6.4). TIER sends no telemetry to the project or anyone else. It is built for one organisation per installation and must not be run as a shared multi-organisation service.

One known defect remains in what the k-anonymity floor protects (Section 10). The floor also cannot protect against a reader who already knows the other members' figures.

---

## 10. Known limits

Known limits, and whether each is filed for a fix:

- **The Claude Code undercount doubt (#837).** Direction known, size unknown, unsettled (Section 4.2).
- **Integration branches count twice.** A PR merged into an integration branch that is merged again later records two outcomes, one per merge. (A squash merge that push capture counted twice before v0.5.2, #849, stays counted twice; from v0.5.2 it counts once.)
- **The write token can change who counts toward k (#908).** Whoever holds the write (admin) token can edit the team map and the aliases that decide who counts, so the k-anonymity floor protects only what a read-token holder sees. Filed for a fix.
- **Manual cost and the pollers (#854).** A server running an organisation poller refuses a manual cost row for that provider's models unless the caller declares it billed elsewhere. The declaration is trusted, and rows posted before v0.5.2 that a poller also counts stay counted twice.
- **Labels are trusted.** Nothing detects a generously sized PR, and splitting large work into several PRs earns more points (one `xl` is 8; two `l` are 10).
- **The first recorded cost stands.** When the same call reaches TIER twice, the stored token counts rise to the larger of the two, but the stored cost stays at the first value, so the two can disagree until `tierd reprice` recomputes the cost from the counts.
- **One flagged outcome takes a whole team below the floor.** The more outcomes a team has, the likelier one of them is flagged, so a large team's headline can stay a word for long periods.
- **Quality is partial.** Follow-up fixes, partial reverts, incidents and hotfixes are specified but not enforced; CI and revert signals come only from GitHub; a backfill of history does not reconstruct them.
- **No per-viewer access yet** (Section 7.1) and **no tested deployment size** for the single-file database.
- **Gaming.** If TIER becomes a target, people can raise the number without improving the work: by inflating labels, splitting PRs, avoiding hard problems, doing AI work on tools TIER cannot see, or posting cost under another name with the shared write token. The reference lists each route (Section 11).

---

## 11. Quick install and first run, for one person

This trial is for an engineer: it runs on one laptop, in the per-developer mode, with Claude Code as the AI tool and a GitHub repository. It needs Go 1.26.9 or newer (plus `git` and `make` only to build from a clone), or a downloaded release (the download and checksum steps are in the repository's `docs/quickstart.md`). Everything below uses TIER's default database file, `~/.tier/tier.db`, and its default address, `127.0.0.1:8080`, which only this computer can reach.

**1. Install.**

```sh
go install github.com/tiermetric/tier/cmd/tierd@v0.5.2
tierd version
```

`go install` puts `tierd` in Go's binary directory (`$(go env GOPATH)/bin` unless `GOBIN` is set); add that to `PATH` if `tierd version` is not found.

**2. See it with invented data.**

```sh
tierd demo          # then open http://127.0.0.1:8080
```

The demo keeps running in that terminal and uses its own throwaway database. Stop it with Ctrl-C before step 5, because the server in step 5 uses the same address.

**3. See where your own Claude Code spend went** (no server, no outcomes). It prints the price-table version, cost per developer id, and cost per issue, including an `unattributed` row:

```sh
tierd score --repo ~/src/your-repo          # --repo here is a folder on this computer
```

**4. Create a read-only GitHub token** so TIER can read your merged PRs. On GitHub: Settings → Developer settings → Personal access tokens → Fine-grained tokens → Generate new token. Give it access to that one repository and "Pull requests: Read-only". Copy the value, run the line below, paste the value (nothing is shown) and press Enter:

```sh
mkdir -m 700 -p ~/.tier && chmod 700 ~/.tier && read -rs T && (umask 077; printf '%s' "$T" > ~/.tier/github-token) && chmod 600 ~/.tier/github-token; unset T
```

**5. Load outcomes, start the server, load cost.**

```sh
export TIER_GITHUB_TOKEN=@$HOME/.tier/github-token   # "@path" means: read the value from this file
tierd backfill --repo your-org/your-repo              # --repo here is the GitHub owner/name
tierd serve --aggregation developer --watch-repo ~/src/your-repo
# in a second terminal:
tierd ship --server http://127.0.0.1:8080 --repo ~/src/your-repo
```

- **Why backfill first.** `backfill` and `serve` write the same database file, and SQLite lets one writer in at a time; a backfill while the server is busy can fail with "database is busy". To load new merges later, stop `serve`, run `backfill` again, and restart.
- **Why both `serve --watch-repo` and `ship`.** The watcher records Claude Code sessions as they change while the server runs. `ship` runs once and loads the history already on disk (the last 90 days, or as much as the logs keep). A session both see is stored once.
- **No token is needed here.** A server bound to `127.0.0.1` and started without `--api-token` asks for none, which is why the commands above and the `curl` below carry none. Any server reachable from other machines must have a token (the reference, Section 9.5).

Open `http://127.0.0.1:8080`. If your computer's user name differs from your GitHub login, join them:

```sh
curl -X POST http://127.0.0.1:8080/api/v1/developer_alias \
  -H "Content-Type: application/json" -d '{"alias":"<your-os-user>","canonical":"<your-github-login>"}'
```

**6. Choose the window.** Read the earliest safe start date:

```sh
curl -s http://127.0.0.1:8080/api/v1/scores | grep -o '"cost_coverage_safe_since":"[^"]*"'
```

Set the dashboard's "From" date to that date, or add `?since=<that date>` to the API call. `tierd doctor --repo ~/src/your-repo --server http://127.0.0.1:8080` reports the same as its "cost horizon" check.

**What to expect.** With fewer than 3 merged PRs that name an issue, or under $5 of recorded cost, the headline shows a word, not a number (Section 5.2). Backfill loads 90 days of PRs while the logs may cover about 30, so the older PRs are flagged zero-token: the headline shows a "NOT ENOUGH …" word, and the number the API still returns for the 90-day window reads high. Step 6 fixes both. With no GitHub webhook in this setup, no CI or revert signal arrives, so every outcome keeps quality 1.0. Deploying for a team (a central server, laptops sending cost on a schedule, the webhook, encryption in transit, tokens) is for the administrator and is in the reference, Section 9.5.

---

## 12. Glossary

- **Accepted outcome**: a merged PR, an opt-in direct commit to the default branch, or an outcome posted through the API (§2.1).
- **Weighted point**: one outcome's weight × quality (§2.1).
- **TIER**: weighted points per $1,000 of list-price AI cost (§2.1).
- **cost_per_point**: dollars of list-price AI cost per weighted point (§2.4).
- **List price**: the provider's published per-token rate, from TIER's built-in price table (§4.1).
- **Unattributed (unlinked) spend**: cost that could not be linked to an issue; it stays in the denominator (§5.3).
- **Evidence floor (`ranked`)**: 3 outcomes, $5.00 of cost, no zero-token outcome (§5.1).
- **Zero-token outcome**: an outcome whose developer recorded under 1,000 tokens on its issue in the 14 days to the issue's latest merge (§5.1).
- **Provisional**: the dashboard's label when less than half of the cost was linked to an issue (§5.2).
- **Window**: the time span a figure covers, `[since, until)` in UTC (§5.4).
- **Cost horizon**: the earliest cost TIER has recorded; windows starting before it read high (§5.4).
- **Segment**: the figure for one kind of work, using only that kind's linked cost (§2.5).
- **Spend Leverage**: list-price cost ÷ actual paid (§8).
- **Developer id**: the name cost and outcomes are recorded under; an operating-system user name or a GitHub login, joined by an alias (§4.1).
- **Team map**: the administrator's table giving each developer id a team, a division (a group of teams) and an organisation (§9).
- **Rubric version**: the number that changes when the weights, work types or quality rules change (§2.1).
- **Reporting mode**: `developer`, `team` or `division`; changes what is shown, not what is stored (§9).
- **k-anonymity**: a group is named only when at least k people count toward it (§9).
- **Sealed month**: a team or division report for one calendar month, computed once after the month closes and never recomputed (§9).
- **Operator / administrator**: the project maintainer who rules on design questions / whoever runs an installation (front matter).
- **Codex, Opencode, Muse Code, Z.ai**: OpenAI's command-line coding agent; an open-source coding agent; Meta's command-line coding agent; the company selling the GLM models and a flat-fee coding plan for them (§0).

---

## 13. Where the detail lives

The [technical reference](technical-reference.md) covers, in more detail:

| Reference section | What it covers |
|---|---|
| 2 | the formula's edge cases, the comparison rule's price-version check, which figure a decision uses |
| 3 | every capture path, what each reads, identity, double counting, the organisation pollers, pricing and cache arithmetic, unknown-model guesses, price-table versions and overrides |
| 4 | the outcome producers, size labels and the size heuristic, kinds of work, quality floors and revert rules, push capture, the outcomes API, backfill |
| 5 | issue linking, opt-in worktree attribution, identity aliases, unattributed buckets, the two clocks, the cost horizon |
| 6 | reporting modes, k-anonymity and suppression, sealed monthly reports, the coaching position, what is stored, erasure and retention |
| 7 | the evidence floor, confidence intervals, `significant` |
| 8 | Spend Leverage accounting (months, seats, credits, subscription fees) and how to use TIER for a spending decision |
| 9 | install, demo, the full single-user score, team deployment, the other commands, upgrades |
| 10 | the API: routes, token scopes, response fields |
| 11 | known limits and gaming routes |
| 12, Appendix A | glossary and a machine-readable summary |

Administration procedures that neither document reproduces (full schemas, configuration keys, restore, hardening) are named in the reference with the repository file that holds each.
