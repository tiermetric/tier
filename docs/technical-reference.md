# TIER — technical reference

## How TIER v0.5.2 captures cost, records outcomes and computes the number

**Reference version:** 1.2 (2026-10-01). Describes TIER **v0.5.2**, the first public release of the v0.5 line (embedded price table version 12, effective 2026-10-02, 91 models; rubric version 1, which versions the size scale, work types and quality floors; Go 1.26.9). Earlier releases lack parts of what follows: v0.4.1, for example, has no Opencode collector, carries price table version 9 and has no `tierd prices forget-version`. TIER is released under the Apache License 2.0.

**What this document is.** The companion to the [TIER white paper](whitepaper.md), which says what the number means and which decisions it may and may not inform. This reference holds the mechanics: capture paths, pricing, outcome producers, attribution and windows, aggregation and privacy, statistics, Spend Leverage accounting, team deployment, the API, and the known ways the number misleads. Where the two overlap they say the same thing; where they seem to differ, this reference is the more detailed statement and the white paper is the defect.

**How to read it.** Every behaviour the reference describes is stated in it. Operator procedures it does not reproduce are named with the repository file that holds them each time: request and response schemas, the full configuration schema, backup and restore, the binary download and checksum commands, proxy and TLS hardening, each command's failure output and full flag list (`tierd <command> -h`). All file names refer to the public repository, `tiermetric/tier`. Where a behaviour is configurable, the reference names the flag or setting. "Planned" marks designed work that has an operator ruling behind it; "specified but not enforced" marks design text with no build commitment. Version 1.1 was read against the source of the time during the maintainer's review; version 1.2 checks each change made since then against the source prepared for v0.5.2. Measured figures carry their date and source.

**Two roles.** The **operator** is the project maintainer, who decides disputed design questions. Each ruling is recorded with its date in the maintainer's private rulings ledger and on the issue it answers; this reference states in full every ruling it relies on. Issue numbers (#826 and others) refer to the maintainer's issue tracker, which may not be public; they are provenance only. Whoever installs and runs TIER is the **administrator**.

---

## 1. The problem

AI bills report spending, not the software changes that spending produced. Two teams can spend the same amount and merge very different amounts of work, and some spend goes to work that never merges.

The common engineering frameworks do not cover this. DORA (DevOps Research and Assessment) measures delivery speed and stability. SPACE and DX Core 4 are frameworks for developer productivity and experience. As the project reads their published definitions (`docs/how-tier-relates.md` sets out that reading), none uses dollars of AI spend as a denominator. TIER adds a measure in dollars of AI spend, for use alongside them.

A useful measure has to deal with four things. Tokens are not comparable across models: the same token count costs very different amounts on an expensive and a cheap model. Invoices are not comparable across organisations: discounts, credits and subscriptions make identical workloads bill differently. Merged work is not comparable by count: a one-line typo fix and a multi-week subsystem are both "one pull request" (PR). And a number tied to a person's incentives can be gamed. Sections 2 to 5 address the first three; Sections 6.3 and 11 address the fourth.

---

## 2. Definition: the formula, units and a worked example

### 2.1 The formula

```
TIER = Σ(outcome_weight × quality_multiplier) / (total_AI_cost_USD / 1000)
```

The numerator is the sum, over accepted outcomes inside the measurement window, of each outcome's **weight** (its size) multiplied by its **quality multiplier** (1.0 for a clean merge, reduced by a continuous integration (CI) failure or a revert). The denominator is the list-price cost of all AI usage recorded in the same window, in US dollars, divided by 1,000. Spend that could not be linked to an issue is included (Section 5.3).

An **accepted outcome** is one of three things: a merged pull request (Section 4.1), a direct commit to the default branch when the administrator turns on push capture (Section 4.5), or an outcome posted through the outcomes API, where the caller asserts the merge (Section 4.6). Nothing else adds to the numerator.

**Unit:** weighted outcome points per $1,000 of list-price AI cost. A TIER of 100 means 100 weighted points for each $1,000 of AI usage. The scale constant 1,000 is fixed in the scoring engine.

The report also carries two related figures:

- **`cost_per_point`** = `total_cost_usd / weighted_points`, in US dollars per weighted point; `1000 / TIER` whenever both are non-zero. With zero points it is `null`, so "no accepted outcome" never reads as the most efficient score. With points at zero recorded cost, TIER is 0 and `cost_per_point` is 0; the dashboard shows that case as "FREE" (Section 7.1), because a `tier` of 0 alone cannot tell it from nothing accepted.
- **Spend Leverage** = `total_cost_usd / actual_paid_usd`, the list-price value of the usage divided by what finance actually paid (Section 8).

**Which figure a decision uses.** The pooled figure divides the window's points by **all** of its recorded cost: accepted work, abandoned and unfinished work, and spend not linked to any issue. Each per-work-type segment (Section 4.3) divides that type's points by the cost linked to that type's outcomes **only**, so it leaves out abandoned work and unlinked spend, and it usually reads higher (it can read lower when its own mix of outcomes differs). The segment answers "what did accepted work of this kind cost"; the pooled figure answers "what did we spend per accepted point". A decision about one kind of work compares its segment across two windows, and reads the pooled `no_outcome` and unattributed amounts from `segment_reconciliation` beside it (Section 5.3), because a change that moves spend into abandoned work lowers the pooled figure and leaves the segment unchanged. Segments and `segment_reconciliation` are served only in `developer` mode (Section 6.2).

**When two figures are comparable.**

1. **No absolute scale.** There is no good/ok/poor band and no reference cohort. A bare figure is neither good nor bad.
2. **The main comparison is with your own past.** An organisation, or a team, against its own history, for one work type (per-work-type figures are served only in `developer` mode; Section 6.2). Two teams in the same organisation compare weakly: only within one work type and one window, after a review of a sample of each team's size labels against Section 4.2, and still subject to their different task mix; treat the difference as a question to ask, not a finding. Two organisations do not compare, even on the same price table (Section 6.3).
3. **Same prices, rubric and release.** The two responses must carry the same `price_table.table_hash` and `rubric.version`, and come from the same TIER release (`GET /api/v1/livez` reports it). The release matters because some scoring rules, such as the heuristic's thresholds and the revert keyword lists, can change without a new rubric number (Section 4.2). That block describes the table the server has loaded **now**; each stored cost keeps the `price_version` it was stamped with (for every path except `POST /api/v1/costs`, the table that priced it; Section 3.1), and `data_quality.mixed_price_versions` appears only when one window holds several (in `developer` mode only; team and division reports do not carry it, Section 6.2). So after a table change, two windows can show the same hash yet have been priced by different tables. Confirm with the `price_version` column of `GET /api/v1/events`, or reprice both windows under the loaded table (Section 3.3). The hash, not the version number, is the check: an override can reuse a number with other rates (Section 3.4). No stamp can see labelling habits, so a change in how generously labels are applied moves the number with no stamp moving.

### 2.2 The rules inside the formula

- Each stored outcome contributes its full `weight × quality` once. One accepted change can be stored twice: a PR merged into an integration branch whose merge is then merged again (Section 4.1), and a squash merge that push capture stored twice before v0.5.2 (Section 4.5). Work that is never accepted (a pull request that is closed unmerged, abandoned or still open) contributes nothing to the numerator, however much it cost; that cost stays in the pooled denominator.
- Team and organisation figures are summed points over summed cost, never an average of individual ratios. Example: A has 10 points on $10 (TIER 1,000) and B has 1 point on $100 (TIER 10). The average of the two ratios is 505; the pooled figure is 11 / $110 × 1,000 = 100, which reflects where the money went.
- When the window's cost is zero the engine reports TIER 0 rather than dividing by zero. So uncaptured usage makes TIER read high only when some cost is recorded; with none recorded, TIER is 0.
- The window is half-open: `[since, until)`. A row timestamped exactly at `since` is inside; one at `until` is outside.

### 2.3 Worked example (illustration, with the arithmetic shown)

Suppose a developer merges three pull requests in a window, and $35.00 of list-price AI spend is recorded under that developer in the same window (all of it, whether or not it was linked to an issue).

| PR | Size label | Weight | What happened after merge | Quality | weight × quality |
|---|---|---|---|---|---|
| A | `size/m` | 3 | clean | 1.0 | 3.0 |
| B | `size/s` | 1 | CI failed on the merge commit within 48 h | 0.7 | 0.7 |
| C | `size/l` | 5 | reverted 20 days later by a commit whose first line is `Revert "Add export"` and whose body says the feature was "no longer needed" (strategic) | 0.8 | 4.0 |

Weighted points = 3.0 + 0.7 + 4.0 = **7.7**.

TIER = 7.7 / (35.00 / 1000) = 7.7 / 0.035 = **220** weighted points per $1,000.

`cost_per_point` = 35.00 / 7.7 = **$4.55 per weighted point** (and 1000 / 220 = 4.55, the same figure from the other direction).

This row clears the evidence floor (Section 7.1): it has 3 outcomes (the floor is 3), $35.00 of cost (the floor is $5.00), and, in this illustration, no outcome flagged as zero-token. So `ranked: true`, and it carries a confidence interval.

A second illustration, on the cost side, shows how one API call becomes dollars. Take `claude-sonnet-5`, which the version-12 price table lists at $2.00 per million input tokens and $10.00 per million output tokens, with Anthropic's default cache multipliers (reads 0.10×, 5-minute cache writes 1.25×, 1-hour cache writes 2.00× of the input rate). A call with 200,000 input tokens, 1,500,000 cache-read tokens, 50,000 5-minute cache-write tokens and 5,000 output tokens prices as:

```
input      200,000 / 1e6 × $2.00              = $0.400
cache read 1,500,000 / 1e6 × $2.00 × 0.10     = $0.300
cache w5m  50,000 / 1e6 × $2.00 × 1.25        = $0.125
output     5,000 / 1e6 × $10.00               = $0.050
total                                          $0.875  (stored as 875,000 micro-dollars)
```

The pricing function sums the per-class amounts for the call and rounds once, to the nearest micro-dollar (1 USD = 1,000,000; an exact half rounds to the even neighbour), and stores that integer.

---

## 3. Where the cost comes from: capture paths, pricing and cache, price-table versions

### 3.1 Capture paths

TIER's default path reads the session logs Claude Code already writes, so a Claude Code user installs nothing and sets nothing; the other paths need a flag or, for the proxy, a base-URL change. Every cost event lands in one table, `token_events`. Eight producers write to it: four log readers (Claude Code, Codex CLI, Opencode, Muse Code), the reverse proxy, `POST /api/v1/costs`, and two org-level pollers. `tierd ship` is a carrier, not a ninth producer: it runs the log readers on a laptop and sends their events to a server.

Cost is recorded under a developer identifier. The log readers use the operating-system (OS) username (`$USER`, then `$LOGNAME`, then the system account name). `tierd score` and `tierd ship` accept `--developer <id>` to override it; `tierd serve` has no such flag. Outcomes carry the GitHub login, so the two often need joining (Section 5.2).

**Claude Code session files (on by default).** Claude Code writes one JSONL (JSON Lines) file per session under `~/.claude/projects/`. TIER's parser reads only `type`, `timestamp`, `gitBranch`, `cwd`, `sessionId` and `message.{id, model, role, usage.*}`, so prompt and completion text never become a stored value. With worktree attribution on (Section 5.1; off by default), a second decoder also reads each tool call's name and the file paths it names, never file contents or shell commands; `docs/privacy.md` lists exactly what it reads and keeps. In Claude Code's logs as TIER's parser meets them, one call appears as several entries with the same `message.id`: one per streaming chunk plus a final entry. Within one read, TIER keeps the entry with the largest token total (input, output and all cache classes summed), which is the final one. A session counts only when its `cwd` is the target repository or a directory under it, or a git worktree of that repository; other sessions are dropped. Each message becomes one event keyed on `("anthropic", message_id)`, the same key the proxy derives for the same call. `tierd score` scans once and exits; `tierd serve --watch-repo <path>` follows the directory with a 1-second debounce.

When a second event arrives with a key already stored, from a later read or from the proxy, the store keeps the larger of each token count and keeps the **first** row's cost. It does not re-price. So a row first stored from a partial read keeps its partial cost, and a fuller count arriving later through the proxy does not raise the stored cost. `tierd reprice` (Section 3.3) recomputes cost from the stored counts. How often a partial first read happens in practice has not been measured.

**An open question about those counts.** A third-party measurement (gille.ai, 24 February 2026, labelled preliminary by its author) compared Claude Code's JSONL token counts with the totals Claude Code shows in its status bar. After removing duplicate entries by request id, it still found output tokens undercounted 10–17× and input tokens 100–174×. It gave two causes for the output gap: placeholder counts that are never updated, and thinking tokens the log leaves out. TIER's deduplication (issue #6) stops one call being counted twice; it does not close that gap. Whether the gap holds for current Claude Code versions, and how large it is, is being measured under issue #837 and is unsettled; treat it as open until a later version of this reference says otherwise. If it holds, Claude Code cost read from JSONL is understated, so TIER reads high and `cost_per_point` reads low. The white paper and Section 8.2 say what that means for a spending decision.

**Codex CLI rollout logs (off by default, `--codex-rollout`).** Codex writes per-session logs at `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`. TIER reads the line type and timestamp and, under `payload`, the session `id`, `cwd`, `git.branch`, `model` and the cumulative `total_token_usage` counters of each `token_count` event. Prompts, reasoning traces, tool output and patches in the same file are never decoded. Per-call usage is the **difference between consecutive cumulative snapshots**; the first snapshot counts in full, and a repeated snapshot adds nothing. Summing the per-call field instead can double-count, because Codex was seen to re-emit a `token_count` event: on one session captured for the collector's tests in July 2026, summing gave 145,165 tokens where the cumulative counters gave 124,386. Four checks are fatal for a file: `total == input + output` when `total_tokens` is non-zero, cached tokens within input, reasoning tokens within output (never added on top), and no counter going down. A file that fails emits no events on that pass, and the error names it; events stored from it on an earlier pass stay. This reference names no supported tool versions: a change in a tool's log format can stop its capture. The key is `("codex-rollout", "openai", session_id, ordinal)`. Under `serve` the collector rescans every `scan_interval` (config `collectors.codex_rollout.scan_interval`; default 5 minutes, floor 30 seconds). Under `ship` it makes one pass.

**Opencode session store (off by default, `--opencode`).** Opencode keeps sessions in a SQLite database at `~/.local/share/opencode/opencode.db` (`ship --opencode-db`, or `collectors.opencode.db_path` in the `serve` config, moves it). TIER opens it read-only, reads the `message` table and, from each message's JSON, the role, model id, provider id, working directory, completion time and token counts. It skips messages that have not completed. The key is `("opencode", provider id, message id)`. The spend is recorded against the watched repository when the message's working directory is inside it, and dropped otherwise.

Opencode reports `reasoning` tokens **beside** `output`, so TIER prices `output + reasoning` at the output rate. Z.ai's pricing page, as the project read it on 2026-08-28, does not say whether reasoning bills at that rate: the token arithmetic is checked, the rate convention is not. On the maintainer's own Opencode GLM-5.3 sessions, measured on 2026-08-28, reasoning exceeded output on 77% of 3,943 messages. Opencode's own `cost` field is not read.

The collector captures one provider route: the Z.ai coding plan (provider id `zai-coding-plan`). It skips every other provider and logs how many messages it skipped. Ollama's cloud tier is named in that log, because it publishes no per-token rate to price against; a provider TIER does not know is skipped with a warning. The embedded table prices `glm-5.3` ($1.40 / $4.40 per million, cache read $0.26) and `glm-5.3-flash` ($0.15 / $0.50, cache read $0.03) at Z.ai's published per-token API rates, which the operator ruled on 2026-09-25 are the value of coding-plan tokens even though the plan itself is a flat subscription (Section 8.1).

Any other GLM model on that route (for example `glm-5.2`) has no embedded row, so its events price at the $0.50 per million unknown-model guess, with a warning (Section 3.3). To give one a rate, build a `--prices` table from the embedded `internal/store/prices.yaml` with a `version` this database has never recorded, such as `1000`, because `--prices` replaces the whole table (Section 3.4). Do not add a `glm-5.3@zai-coding-plan` row: a key with `@<route>` is looked up before the bare model name, so that row would replace the embedded `glm-5.3` row for coding-plan traffic, and one marked `billing_mode: subscription` relabels that spend. Every command that loads an override changing a built-in per-token model's `billing_mode` logs a WARN naming both modes, and the override still loads (#921). The label never changes a stored cost or TIER: it appears in the `/events` export and the data-protection export, `tierd reprice` rewrites it, and a `subscriptions:` block (Section 8.1) accepts a route only if the price table marks that route's row `billing_mode: subscription`. `serve` warns at startup if a `--prices` table drops the embedded `glm-5.3` rows.

Opencode records no git branch, so its spend lands in the `unattributed:detached-head` bucket and never joins an issue. The spend still counts in the denominator, and the outcome it produced still counts in the numerator, so the pooled figure is not biased by it; only the pairing of that cost with its issue is lost. The zero-token check (Section 7.1) looks for tokens per (developer, repository, issue), so an outcome whose AI work was done only in Opencode records no tokens against its issue, is flagged, and takes its developer (and, in team or division mode, its group and the organisation headline) below the evidence floor while keeping its points. No branch-naming convention can fix this, because the branch is never recorded. Like Codex, Opencode capture is off by default on `serve` and `ship`, and `serve` requires `--watch-repo` with it.

**Muse Code session logs (off by default, `--muse`).** Meta's Muse Code CLI writes one session log per session at `~/.local/share/muse/sessions/YYYY/MM/DD/<session-id>/session.jsonl` (`ship --muse-home`, or `home` in the `collectors.muse` config block, moves it). TIER reads only token counts, model names, the session id, each run's recorded branch and the workspace root, never prompt, reply or tool text. Its token mapping was measured on one real Muse Code 1.4.0 session (`docs/how-it-works.md`, section 3d). Two record kinds are model calls Meta bills, and each becomes one event: an agent model call, and an automatic model review of a pending tool call. Muse writes a run's branch after the run's last model call, so a call is recorded only once its run has settled: the run's branch record is present, or the run has ended and Muse wrote past it, or the session log has gone 6 hours without a write. The 6-hour rule is a guess: a live run that waits longer at a tool-approval prompt is recorded as `unattributed:detached-head`, and that stays. Otherwise attribution follows the same branch and ±30-minute commit rule as Claude Code (Section 5.1), and a session counts only when its recorded workspace root is in a watched repository. A subagent's calls take the repository and branch of the parent run that spawned it; subagent spend that cannot be tied to a single parent run is excluded with a warning, so a Muse Code user's recorded spend can be lower than their real spend (`docs/how-it-works.md`, section 3d, lists the cases). The key is `("muse", "meta", record id)`. Calls are priced at the `muse-spark-*` per-token list rates even when the developer pays for Muse by subscription (an operator ruling on #895, 2026-09-27), and Meta's per-query web-search charge is not priced, so Muse Code cost is a floor for any session that searched. The proxy has no Meta route. Under `serve` the collector rescans every `scan_interval` (config `collectors.muse.scan_interval`; default 5 minutes, floor 30 seconds), its first pass reads every log on disk, and `serve` requires `--watch-repo` with it.

**Reverse proxy (mounted by default).** `tierd serve` mounts `/anthropic/`, `/openai/` and `/gemini/`, forwarding to `--anthropic-target`, `--openai-target` and `--gemini-target` (defaults: the three providers' public API hosts). Setting a target to the empty string unmounts that route; `--read-only` unmounts all three. A client points its base URL at the proxy (for example `ANTHROPIC_BASE_URL=http://<tier-host>:8080/anthropic`) and sends the **admin (write) token** in an `X-Tier-Token` header, so every developer who uses the proxy holds the organisation-wide write token (the same exposure as `ship`, Section 9.5). It is a separate header because `Authorization` carries the client's own provider credentials, which the proxy passes through. Forwarding a call on the client's provider credentials spends money, so the read-only token gets `403`; a missing or wrong token gets `401` and counts toward the per-IP lockout. Every `X-Tier-*` header, this one included, is removed before the request goes upstream, whether or not an admin token is configured (#865). With no admin token configured, which `serve` allows only on a loopback address, the proxy takes any request. The proxy passes the response through (it may decompress a gzip response in transit), and records `id`, `model` and `usage`, including from server-sent-event streams. The `/openai/` route reads both the Chat Completions and the Responses API shapes. The Gemini route is tested against synthetic bodies and has not been exercised by live Gemini traffic.

The client may name the spend with three headers: `X-Tier-Developer`, `X-Tier-Issue` and `X-Tier-Repo`. Whether a coding tool can add custom headers is a property of that tool, not of TIER. The proxy trusts the headers as sent. `X-Tier-Issue` is stored verbatim and joins an outcome only if it is written the way TIER stores issue ids (`issue-42`, `TIER-99`; Section 5.1). The one value the proxy rejects is the reserved sentinel `unattributed` (or any `unattributed:` value), which counts as forged. A missing or forged developer header stores the spend under the developer `unattributed`; a missing or forged issue header stores it under the issue `unattributed` on the named developer. A counter records each case. Any other name is accepted as sent, so a token holder can put spend on anyone's row (Section 11). Attribution never fails a provider request.

**Double counting, in one list.**

- Claude Code JSONL **and** the proxy for the same call: one row, because both use the Anthropic message id. The row keeps the larger token counts and the first writer's cost.
- Re-running `tierd score`, `tierd ship` or a watcher re-read: one row per key.
- Codex through `/openai/` **and** `--codex-rollout`: **counted twice.** The proxy keys on the response id, the collector on session and ordinal.
- Opencode through the proxy **and** `--opencode`: **counted twice**, for the same reason.
- `POST /api/v1/costs` without an `idempotency_key`: every re-post is a new row.
- `POST /api/v1/costs` for usage a provider poller also reports: refused on a server running that provider's poller, unless the row declares `"billed_to": "other"` (Manual REST, below; #854). A wrong declaration, and a row stored before v0.5.2, are counted twice.

`tierd serve` warns at startup in the two double-counting cases: Codex capture with `/openai/` mounted, and Opencode capture with any proxy route mounted.

**Manual REST.** `POST /api/v1/costs` stores one event, for scripts and imports. Its `source` must be `"api"` or omitted (omitted means `api`); any other value is a `400`. Every cost event carries a `fidelity`: `realtime` (captured per call, by the log readers and the proxy), `daily` (a daily total) or `estimated`. On `/costs` it may be `daily` or `estimated` (default `estimated`); `realtime` is refused. Unlike every other path, `/costs` stores the caller's dollar figure (`cost_usd`) as given, so an import must apply list pricing itself. With an `idempotency_key`, a repeat of the same cost is ignored and a different cost is a `409` unless the request sets `override` (an audited correction); a keyed post whose developer, issue, model, source or fidelity differs from the stored row is also a `409` and writes nothing (#871). On a server running the Anthropic Admin or OpenAI Usage poller, a row for that provider's models is refused with `400` unless it declares `"billed_to": "other"`, for spend billed where the poller cannot see it (Claude Max seats, Bedrock or Vertex, another organisation); the declaration is stored and exported, and `serve` warns at startup with the count and dollar sum of undeclared manual rows on days a poller covered (#854). `model`, `developer` and `issue_id` are required; `issue_id` is stored as sent, so it joins only if written the way TIER stores issue ids. A negative cost is a `400`. The body has no timestamp field: the server stamps the time it arrives, so an import of past spend lands in the current window. The row is stamped with the loaded table's `price_version` although that table did not price it, and `tierd reprice` never rewrites it.

**Org-level pollers (off by default).** Two pollers read an organisation's usage and cost from the Anthropic Admin API and the OpenAI Usage API, one day at a time, once that day ended at least 24 hours ago. They write two things. First, a remainder event per model and day: for each token class, the provider's total minus what the Claude Code, Codex, Opencode and proxy paths recorded for that model and day, never below zero, priced with the table. It is stored under developer `unattributed`, issue `unattributed` and fidelity `daily`, with no repository. These reach the denominator and cannot be split per person. Each poll re-reads every day from the first of the previous month. Because a repeated key keeps the larger token counts and the first cost (above), a remainder that later shrinks, because laptop events arrived late, keeps its larger counts; one that later grows gains tokens but keeps its first cost. Days older than that window are never revisited. Second, delta rows in `org_actual_spend` under the source `anthropic-admin` or `openai-usage`, which feed Spend Leverage (Section 8). They are enabled by the `collectors.anthropic_admin` and `collectors.openai_usage` config blocks, each taking `api_key` (prefer the `@/path/to/file` form), `org` and an optional `poll_interval` (default 1h, minimum 5m). Do not also post the same provider's invoice to `org_actual_spend` by hand for that organisation.

**The laptop shipper, `tierd ship`.** When developers work on laptops and the server runs centrally, `tierd ship --server <url> --repo <path>` sends locally captured events to `POST /api/v1/events`. The server prices each event again with its own loaded table. `ship` keeps no state (the server deduplicates on keys), reads the last 90 days by default, and exits 1 when it finds nothing unless `--allow-empty` is passed. Every run resends everything in its window, in batches of 500 events, and the server keeps one row per key; a scheduled job can pass a shorter `--since` to send less. A batch that fails after three retries ends the run with exit 1; batches already sent stay stored. The project has not measured the server load of frequent full resends. `--codex-rollout`, `--opencode` and `--muse` are off by default on `ship` too. Without them, a central server records a laptop's Codex, Opencode or Muse Code outcomes and none of their cost, so that work reads as free and TIER reads high for exactly the developers who moved to those tools.

### 3.2 Pricing: list prices, and why

Captured tokens are converted to dollars with a **reference price table** embedded in the binary (`internal/store/prices.yaml`), never with the organisation's invoice. The aim: **price what the provider would charge at its published list rates, including its charging mechanics.** Four exceptions are stated where they apply: an unknown model is priced by a guess (Section 3.3); Opencode reasoning tokens are priced at the output rate by assumption (Section 3.1); `/costs` stores the caller's figure (Section 3.1); and fast mode, below.

- **Mechanics count in the denominator:** cache-read and cache-write multipliers, long-context re-pricing above a model's threshold (Section 3.3), and thinking or reasoning tokens where a provider reports them (priced at the output rate).
- **Procurement does not:** enterprise discounts, batch discounts and subscription fees leave the denominator unchanged. They are recorded as paid spend and show up in Spend Leverage (Section 8).
- **One known gap:** Anthropic offers a faster, dearer "fast mode" for some models (Opus 5 at $10 / $50 per million, Opus 5.5 at $8 / $40, as recorded in the price table's comments and `docs/reference-price-table.md`). TIER's parsers read no speed or fast-mode field, so fast-mode calls are priced at the base rate. Cost is understated and TIER reads high, with no warning.

List pricing puts every installation's cost on the same price basis: the same tokens on the same model cost the same dollars everywhere. That does **not** make TIER comparable across organisations (Section 6.3). Cache reuse stays in the cost because it depends partly on how the work is run, for example how much context is reused between calls. Two teams with identical work but different cache reuse get different scores: the team with less reuse pays more for the same tokens and scores lower. Read `cache_read_share` (the share of tokens that were cache reads) beside a score before concluding that a team is less efficient.

### 3.3 The cost arithmetic

For a model with separate input and output rates, cost is:

```
input      × input_rate
+ cache_read  × input_rate × cache_read_mult
+ cache_w5m   × input_rate × cache_write_5m_mult
+ cache_w1h   × input_rate × cache_write_1h_mult
+ output      × output_rate
```

with token counts divided by one million and rates in dollars per million tokens. **Long-context tier:** if the model carries one and the call's input plus all cache classes (cache reads and both cache-write classes; output is not counted) is strictly greater than `context_threshold`, the over-tier input and output rates replace the base rates for the whole call, and the cache multipliers scale off the over-tier input rate. In the v12 table three rows carry such a tier, each with a 200,000-token threshold: `claude-sonnet-4-5` ($3.00 / $15.00 base, $6.00 / $22.50 over), `gemini-2.5-pro` ($1.25 / $10.00 base, $2.50 / $15.00 over) and `gemini-3.1-pro` ($2.00 / $12.00 base, $4.00 / $18.00 over). No other row has one, `claude-opus-4-8` included, so a large context on those models prices at the base rate however big it is. Self-hosted reference entries are `combined`: one rate for all token classes and no cache discount.

**Cache multipliers are per model,** as fractions of the input rate. A row that sets none takes its provider's default:

| Provider or rows | Cache read | 5-minute write | 1-hour write |
|---|---|---|---|
| Anthropic (default) | 0.10× | 1.25× | 2.00× |
| `claude-opus-5-5` | 0.05× | 1.25× | 2.00× |
| `claude-fable-5-1`, `claude-mythos-5-1` | 0.025× | 1.25× | 2.00× |
| OpenAI 4-era (default) | 0.50× | 1.0× | 1.0× |
| OpenAI `gpt-5.x` | 0.10× | 1.0× | 1.0× |
| OpenAI `gpt-6-*` | 0.10× | 1.25× | 1.25× |
| `gemini-2.5-pro`, `gemini-2.5-flash`, `gemini-3.1-pro` | 0.25× | 1.0× | 1.0× |
| `deepseek-r1` | 0.0509× ($0.028 ÷ $0.55 input) | 1.0× | 1.0× |
| `deepseek-v3` | 0.1× ($0.028 ÷ $0.28 input) | 1.0× | 1.0× |
| `glm-5.3` | $0.26 ÷ $1.40, stored at full precision | 1.0× | 1.0× |
| `glm-5.3-flash` | 0.2× ($0.03 ÷ $0.15) | 1.0× | 1.0× |
| `muse-spark-1.1`, `-1.2`, `-1.3` | 0.12× ($0.15 ÷ $1.25) | 1.0× | 1.0× |
| `muse-spark-1.2-contributor`, `-1.3-contributor` | 0.02× ($0.002 ÷ $0.10) | 1.0× | 1.0× |
| Every other Google, xAI, DeepSeek, Z.ai and self-hosted row | 1.0× | 1.0× | 1.0× |

Multipliers are used as written, unrounded: `0.0509 × $0.55` is a cache-read rate of $0.027995 per million, not exactly the published $0.028; only each call's total is rounded to the micro-dollar. Representative version-12 rows, in $ per million input / output tokens: `claude-opus-4-8` 5.00 / 25.00; `claude-opus-5` 5.00 / 25.00; `claude-opus-5-5` 4.00 / 20.00; `claude-sonnet-5` 2.00 / 10.00; `claude-fable-5-1` 10.00 / 50.00; `claude-haiku-4-5` 1.00 / 5.00; `gpt-6-astra` 10.00 / 50.00; `gpt-6-sol` 2.00 / 10.00; `gpt-5.6-sol` 5.00 / 30.00; `glm-5.3` 1.40 / 4.40; `glm-5.3-flash` 0.15 / 0.50; `muse-spark-1.3` 1.25 / 4.25; `self-hosted-medium` 0.50 for all tokens. (Fable and Mythos are Anthropic model names; Muse Spark is Meta's.)

**Model names are normalised** before lookup: lower-cased, the one recognised provider prefix (`zai-coding-plan/`) stripped, date and `-preview`/`-latest` suffixes removed (`claude-sonnet-4-20250514` becomes `claude-sonnet-4`). Minor versions have their own rows because each has its own price; normalisation does not collapse `opus-4-8` to `opus-4`. When a call carries a route or host (the proxy's upstream host, Opencode's provider id), the key `<model>@<host>` is tried first and the bare model name second.

An **unknown model** is priced by a guess: round rates the project chose, not measured ones. The tests run in this order of plain substring tests on the lower-cased name. Large (`self-hosted-large`, $2.00 per million, one rate for all tokens): `70b`, `72b`, `65b`, `90b`, `nemotron-ultra`, `deepseek-r1-full`. Small (`self-hosted-small`, $0.10): `3b`, `1b`, `phi-4-mini`, `qwen2.5-3b`, `embeddings`, `reranker`. Medium (`self-hosted-medium`, $0.50): any other `<digits>b`, or no pattern at all. Because the tests are substrings, a `13b` or `31b` model is classed small. Every guess logs a one-time warning per model and increments a counter on the Prometheus metrics endpoint (`/metrics`), so a new model that arrives before the table is updated is visible.

`tierd reprice --from-version N --commit` recomputes the cost of every stored row whose `price_version` is numerically N or higher (so an override numbered 1000 is included in any run below 1000) from its stored token counts, under the current table, and re-stamps `price_version`. It skips `POST /api/v1/costs` rows and rows that carry a cost but no token counts. It refuses to commit if any changed row would be priced by a guess and has a nonzero cost before or after, unless `--allow-guessed` is passed. Rows costing zero both before and after, including zero-token `<synthetic>` rows, do not trigger this refusal; the test is on cost, not the model name. Without `--commit` it is a dry run.

### 3.4 Price-table versions

The table carries `version` (an integer) and `effective_date` (YYYY-MM-DD) as one unit. The embedded table is **version 12, effective 2026-10-02, 91 models**; v12 added `claude-sonnet-5-5` at $2.00 per million input tokens and $10.00 per million output tokens, v11 added the five Meta Muse Spark rows and the `meta` provider, and v10 had added the Fable 5.1, Mythos 5.1, Opus 5.5, gpt-6 and Z.ai GLM rows and corrected `claude-sonnet-5` from $3/$15 to $2/$10. The effective date marks the table revision, not the day each row was captured. The rows are the maintainers' reading of each provider's published prices; this reference does not reproduce the provider pages, and the table file's comments record what each revision changed. No update schedule is published; a new release carries a new table, and until then an unpriced model is guessed and warned about (Section 3.3).

Each cost row is stamped with a `price_version`: the table that priced it, except for `POST /api/v1/costs` rows, which carry the loaded table's version although the caller supplied the dollars (Section 3.1). Only `tierd reprice` and an audited `/costs` correction (Section 3.1) rewrite a stored cost, so a window that spans a price change reports `data_quality.mixed_price_versions`. A `/scores` response carries a `price_table` block describing the table the server has loaded now: `version`, `effective_date`, `table_hash` (a digest of the resolved rates, prefixed `tierpt1:`, the name of TIER's digest scheme, defined in `internal/store/prices.go`) and `file_hash` (a SHA-256 digest of the YAML bytes, prefixed `sha256:`). The table hash answers "same prices": a change to a compiled-in default multiplier moves it with the file unchanged, and a comment-only edit moves `file_hash` alone. Section 2.1 gives the comparison rule built on it.

An administrator may override the table with `--prices <file.yaml>` (or `prices_file:` in config, or `TIER_PRICES`). The override **replaces** the whole table; it does not merge. It must contain the `self-hosted-large`, `self-hosted-medium` and `self-hosted-small` rows or it is refused at startup, and any model it omits is priced by the unknown-model guess. The version number is binding, and two guards enforce it:

1. **At load, before any database opens:** an override that declares this binary's embedded version (12 today) with different resolved rates is refused. No flag accepts the collision, so changed version-12 rates cannot be loaded on any database, fresh or not.
2. **At database open:** a version this database has already recorded under a different `table_hash` is refused. This binding alone can be retired: `tierd prices forget-version --db <path> --version <N> --commit` (a dry run without `--commit`) marks the recorded identity as forgotten, so a replacement table may reuse a non-embedded number.

So two installations can record different tables under the same non-embedded number, which is why the comparison rule uses the hash. Editing rates and bumping `version` are one action.

---

## 4. Where the outcomes come from: PRs, weights, quality floors, direct commits, outcomes API

### 4.1 What counts as an outcome

The default outcome is a **merged pull request**. Merge is the acceptance event; a PR that is opened, reviewed or closed without merging records nothing. Four producers can record an outcome. The webhook, the outcomes API and backfill resolve the weight the same way: a supplied weight (from a size label, or sent through the API) is stored with `weight_source: "label"`, and with none the diff heuristic gives `"git-heuristic"`. Push capture (Section 4.5) uses a fixed 0.5 (`"push"`). Quality starts at 1.0, except that the outcomes API accepts a caller-supplied `quality` in [0, 1] (Section 4.6).

1. **GitHub webhook** (`POST /webhook/github`): a `pull_request` event with `closed` + `merged` inserts one outcome, `source = "github-webhook"`.
2. **Provider-neutral API** (`POST /api/v1/outcomes`): for GitLab, Bitbucket, Gitea or any CI that can call an endpoint on merge, `source = "api-outcome"`.
3. **Backfill** (`tierd backfill`): walks merged-PR history through the GitHub REST API, reconstructing one outcome per merged PR, `source = "backfill"`.
4. **Push capture** (opt-in): direct commits to the default branch, `source = "push"` (Section 4.5).

A merged PR whose branch and body carry no recognisable issue reference records **no outcome at all**. The webhook logs "PR merged but no issue ID found" at debug level; backfill counts such PRs in its closing line (`unattributed=N`), which is the one place the number is reported. Section 5.3 says what this does to the figure.

Each PR or API outcome is keyed on its `merge_commit_sha`, across all repositories (push outcomes have no SHA and are keyed on repository, issue and day; Section 4.5), and **the first producer to record a merge commit hash (SHA) wins**: a later webhook delivery, backfill run or API post for the same SHA changes nothing (backfill counts it as skipped; the API answers `200` with `status: "duplicate"`). There is no correction path short of erasure (Section 6.4). Labels come from the producer that recorded the outcome: the webhook reads them from the merge event, backfill reads them as they are when it runs. Editing a label after that changes nothing, and two producers could have weighed the same merge differently; the first one wins. TIER creates no labels; the team adds them in GitHub. A PR merged into any branch counts, not only the default branch, so a feature PR merged into an integration branch, and that branch merged later, records two outcomes (two merge SHAs). CI is read only on the default branch, so a PR merged into another branch never receives the CI floor (Section 4.4). A PR that closes several issues is credited to one primary issue (the branch-derived id if there is one, else the leftmost `closes #N` in the body); the others are logged, not credited.

### 4.2 Size weights

The weight is the PR's size, read from a GitHub label, case-insensitive, with or without the `size/` prefix:

| Label | Weight | Canonical meaning |
|---|---|---|
| `size/xs`, `xs` | 0.5 | trivial: a one-line fix, a typo, a config flag, a dependency bump |
| `size/s`, `s` | 1 | small: a localised change to one function or file |
| `size/m`, `m` | 3 | medium: a self-contained feature or fix across a few files |
| `size/l`, `l` | 5 | large: a feature touching several components, or a refactor with migration |
| `size/xl`, `xl` | 8 | extra-large: a subsystem or multi-day effort landed as one PR |

**The fixed scale applies to label-derived and heuristic weights:** neither can produce any value but 0.5, 1, 3, 5 or 8. A weight sent through `POST /api/v1/outcomes` is taken as sent, anywhere in (0, 8] (Section 4.6). When a PR carries several recognised size labels, the first in the order the webhook payload (or, for backfill, the REST response) lists them wins. Label **names** can be remapped with the `outcomes.size_labels` config key. A custom table replaces the built-in one entirely, need not list all five sizes, and a label missing from it falls through to the heuristic; each value must be one of 0.5, 1, 3, 5, 8, or startup fails. `tierd backfill --config <file>` reads the same key, so backfilled and live outcomes weigh the same.

With no recognised label, TIER falls back to a bucketed **diff-size heuristic** on the PR's aggregate line and file counts:

```
effort = (additions + deletions) + changed_files × 10
effort ≤ 15   → 0.5
effort ≤ 60   → 1
effort ≤ 200  → 3
effort ≤ 1000 → 5
otherwise     → 8
```

A 50-line PR touching 3 files has effort 50 + 30 = 80, weight 3. The provenance is recorded as `weight_source = "label"` or `"git-heuristic"`. The heuristic is a fallback, not a measure of value. The webhook payload carries aggregate counts and no per-file data, and the webhook path makes no outbound GitHub call and keeps no clone, so generated or vendored churn inflates an unlabelled PR's weight. A size label replaces the diff entirely, which is the mitigation.

The rubric is versioned (`rubric.version: 1`), a number set by hand and reported on every `/scores` response and report manifest; it is not stored on outcomes. The project's tests tie it to the weight scale, the heuristic's output values, the work-type list and the quality floors, so changing any of those fails the tests until the number is bumped. The heuristic's thresholds, the work-type precedence, the default label names and the revert keyword lists are not tied to it: they can change with no new rubric number. The weights, floors and windows in this section are the project's rubric choices; it publishes no study behind them. The meaning column above is the rubric's definition of each size; `docs/rubric.md` adds worked examples and says nothing the table does not.

### 4.3 Work types

Each outcome carries a `work_type` from a closed taxonomy: `feature`, `bug`, `security`, `incident`, `tech-debt`, `research`, `compliance`. It is read from a PR label equal to a type name or prefixed `type:` / `kind:`; when several apply the precedence is security > incident > compliance > bug > tech-debt > research > feature; with no type label the default is `feature`. `GET /api/v1/scores` returns a `work_types[]` array scoring each type separately. A segment divides by the developer's cost on the issues that carry an outcome of that type (Section 2.1 says why that reads higher than the pooled figure). An issue with outcomes of two types has its whole cost counted in both segments, so the segments do not add up to the whole. Comparing a security TIER to a feature TIER is a category error the taxonomy exists to prevent. The pooled rows mix all types, so a shift in the mix of work moves them with no change in efficiency. Section 2.1 says which figure a decision uses.

### 4.4 Quality floors: what is enforced, and what is specified but not

After an outcome is recorded, its quality is derived from events, never edited by hand. Each signal is appended to an append-only `quality_events` log and the outcome's quality is recomputed as the **minimum** of the applicable floors, clamped to [0.1, 1.0]. The floors the shipped code enforces:

| Signal | Floor | Window | Source event |
|---|---|---|---|
| clean merge (`ci_pass`, or no signal) | 1.0 | — | — |
| CI failure on the merge commit (`ci_fail`): a completed `failure` run on the default branch whose head SHA is a recorded merge commit | 0.7 | up to 48 hours after the outcome; no lower bound | `workflow_run` webhook |
| strategic revert, business decision (`revert_strategic`) | 0.8 | up to 60 days after the outcome; no lower bound | `push` webhook |
| quality revert, code problem (`revert_quality`) | 0.1 | up to 60 days after the outcome; no lower bound | `push` webhook |

A strategic revert keeps most of the credit because the rubric reads it as a decision about work that was done, not a fault in it. The outcome's timestamp for a webhook-recorded PR is when TIER received the merge (Section 5.4); a revert is timed by its commit timestamp, a CI run by its `updated_at` from GitHub. The 48-hour window is an upper cutoff only: a failure whose `updated_at` is before the outcome's timestamp still counts, but the outcome must already be recorded when the delivery arrives, or the delivery is ignored. A CI failure is cleared (recorded as `ci_fail_flaky`, quality stays 1.0) by a success that is a re-run of the failing run: same merge commit, same `workflow_id`, a strictly later `run_attempt`, finishing within 30 minutes of the failure. A green run of a different workflow on the same commit does not clear a red one. Once one flaky re-run is recorded for a (commit, workflow), **every** failure of that workflow on that commit is cleared, including later failures with no re-run of their own. So a genuine failure that follows a flaky one on the same commit and workflow is not penalised.

A pushed commit counts as a revert only when its **first line** matches `(?i)^revert\s+["']?(.+?)["']?\s*$`: the word `Revert` (any case) followed by whitespace, as in git's own `Revert "feat: add login"` or a hand-written `Revert broke login`. `Revert: broke login`, `Reverted login change` and `revert(auth): …` do **not** match, receive no penalty, and, with `--push-capture` on, are not excluded as reverts, so such a commit can itself be captured as a 0.5 push outcome. A matching revert is linked to one original outcome: first by the `This reverts commit <sha>` footer git adds (a full 40-character lowercase hex SHA only); failing that, by the issue id found in the message, which picks the most recently recorded outcome for that issue, preferring the pushed repository. The developer is not part of that match, so when several outcomes share an issue only the latest is penalised. If neither link resolves, an info line is logged and nothing is degraded.

The revert is then classified by keyword over the **revert commit's message** (the PR body is not consulted). Each pattern is a case-insensitive regular expression with no anchors or word boundaries, so `.*` means "anything in between", `bug` matches "debug", `OOM` matches "room" and `break` matches "breaking change"; such accidental hits push a message toward the quality class. The strategic patterns are exactly: `product decision`, `PM requested`, `feature flag.*disable`, `business requirement.*changed`, `pivot`, `deprecat`, `sunset`, `removing feature`, `no longer needed`, `replaced by`. The quality patterns are: `broke`, `break`, `broken`, `crash`, `OOM`, `out of memory`, `regression`, `degradation`, `incident`, `outage`, `bug`, `defect`, `performance.*degrad`, `memory leak`, `data loss`, `corrupt`, `timeout`, `deadlock`, `security.*vuln`. A revert is strategic (floor 0.8) only when it hits at least one strategic pattern and no quality pattern; every other case, including no keyword at all, is a quality revert (floor 0.1). The penalty lands on the outcome that shipped the change, never on the developer who reverted it.

The project's quality-degradation specification describes a fuller eight-event model. The following are **specified but not enforced by the shipped code**: a follow-up-fix penalty (−0.15), partial reverts (`1.0 − 0.9 × fraction`), production-incident correlation, hotfix-branch detection (0.4), downstream-CI failures (−0.20 per service), the "no CI signal" 0.95 penalty, the cross-PR flaky registry, and the persisted provisional → observing → final phase state machine. The 48-hour and 60-day bounds are applied as event windows today; the lifecycle state is not materialised. On this event-derived path there is no route to a quality of 0.0, because the clamp floor is 0.1; the outcomes API (Section 4.6) is the exception, since it accepts an explicit `quality: 0` from the caller, though the next quality event on that merge commit replaces it with the event-derived value. There is no endpoint to correct or delete a single quality event; a wrongly classified revert or CI failure stays in the log. The only deletion is the erasure of a whole developer (Section 6.4).

### 4.5 Direct commits (opt-in push capture)

Trunk-based teams that commit straight to the default branch never produce a merged-PR event, so their spend would score near zero. With `--push-capture` (env `TIER_PUSH_CAPTURE`, config `outcomes.push_capture`; off by default), a direct commit to the default branch that carries a resolvable issue reference becomes an outcome with a fixed weight of **0.5**, `weight_source = "push"`, `source = "push"`. Commits are grouped to **one outcome per (repository, issue, day in Coordinated Universal Time, UTC)**, credited to the GitHub login of its earliest commit, ordered by GitHub's push time (`repository.pushed_at`, which GitHub sets), then commit time, then SHA (#938), so splitting work into many commits earns nothing extra. A commit with no GitHub author login is skipped.

Skipped as well: reverts (the same first-line test as above); commits whose first line looks like git's merge subject (`Merge pull request #N`, `Merge branch …`, `Merge remote-tracking branch …` and no other form, so `Merge tag …` is not skipped; push payloads carry no parent list, so this is a subject match); and commits whose changed files are all generated or vendored. "Generated or vendored" means every file matches the `outcomes.generated_paths` list: a trailing `/` is a directory at any depth, a leading `*` is a filename suffix, anything else is an exact filename. The default list is `vendor/`, `node_modules/`, `*.pb.go`, `*_generated.go`, `*.gen.go`, `go.sum` and the lock files of npm, Yarn, pnpm, Cargo, Poetry, Bundler and Composer; `[]` turns the exclusion off.

A squash merge arrives as both a PR event and a push, in no fixed order. When the PR event is processed first, the push commit is skipped because its SHA is already recorded. Each captured commit is also recorded in a per-commit ledger, and a PR outcome removes its merge commit from it in the same transaction: a push outcome that held only that commit is deleted, and one holding other commits stays, re-owned to its earliest remaining commit. So the squash merge counts once whatever the order (#849). Push outcomes stored before v0.5.2 are never deleted, so a double count already stored stays. Push outcomes carry no `merge_commit_sha`, so CI-failure floors do not reach them; revert degradation still applies through the issue id. Push-grain outcomes are not directly comparable with PR-grain outcomes. `/scores` cannot filter by producer; the raw export `GET /api/v1/outcomes` carries `source` on each row, so a split by producer has to be computed from the export.

### 4.6 The outcomes API

`POST /api/v1/outcomes` (write scope) records an outcome from any forge:

| Field | Required | Notes |
|---|---|---|
| `developer` | yes | canonical developer identifier |
| `issue_id` | yes | stored as sent, so it joins cost only when written the way TIER stores issue ids (`issue-42`, `TIER-99`); the `unattributed` sentinel is rejected |
| `pr_number` | yes | integer ≥ 1 |
| `merge_commit_sha` | yes | dedup key, any non-empty string up to 256 characters (not checked as hex); a replay returns 200 with `status: "duplicate"` |
| `merged_at` | yes | timestamp in the RFC 3339 format, year 2020–2050; becomes the outcome timestamp used for windowing |
| `weight` | no | taken as sent if 0 < weight ≤ 8, not checked against the scale: 2.7 is stored as 2.7 and stamped `weight_source: "label"`; 0 or above 8 is a 400 |
| `quality` | no | in [0, 1]; default 1.0. The API accepts an explicit 0; the event-derived path (Section 4.4) never goes below 0.1; the value holds only until the first quality event on that `merge_commit_sha` (a CI run, flaky re-run or revert), which recomputes quality from 1.0 over the event floors and overwrites it; a green CI run resets a posted 0.3 to 1.0 |
| `additions`, `deletions`, `changed_files` | no | used for the size heuristic when `weight` is omitted; with none of them the heuristic gives 0.5 |
| `work_type` | no | one of the seven types, default `feature`; an invalid value is 400 |
| `repo` | no | canonical `owner/repo` so the outcome joins cost from the same repository; omitted, it is stored as `unqualified` (Section 5.1) |

Outside GitHub no CI or revert signal ever arrives, because both come only from GitHub's webhooks, so an API-posted outcome keeps the quality the caller sent.

A fresh insert returns `201` with `{"status":"created","weight_source":...,"weight":...}`. The bearer token is the single authenticator; any holder can post an outcome as any developer, so it is an org-level secret, and the `api-outcome` source stamp keeps such rows distinguishable in audit.

### 4.7 Backfill

`tierd backfill --repo owner/name` reconstructs outcomes from merged-PR history over the last 90 days by default (`--since` reaches further back). It requires a GitHub token with read access (`TIER_GITHUB_TOKEN`, preferably as `@/path/to/file`). It calls two GitHub REST endpoints (Section 9.4), derives the issue id, weight and work type with the same code as the webhook, starts each outcome at quality 1.0, and does **not** reconstruct degradation signals: a revert or CI failure that already happened in history is not applied. A PR already in the store is skipped, not updated. Run it before `serve`. Both write one SQLite file, and SQLite lets one writer in at a time; a writer that waits more than 5 seconds for the other fails with a "database is busy" error. TIER takes no lock of its own to prevent this.

---

## 5. Attribution and windows: issue linkage, unattributed spend, the cost horizon, window matching

### 5.1 The shared key is the issue id

Cost and outcomes are joined by developer and by **issue id**, extracted by one package from the branch name or the PR body so all paths agree:

| Where | Format | Example | Resolves to |
|---|---|---|---|
| branch | first all-digit segment between `/`, `-` or `_`, any prefix or none (no leading zero) | `feature/42-auth`, `42-auth` | `issue-42` |
| branch | tracker key `[A-Z][A-Z0-9]+-<N>` | `fix/TIER-99-crash` | `TIER-99` |
| PR body / commit | `closes #N`, `fixes #N`, `resolves #N` (case-insensitive) | `closes #42` | `issue-42` |
| PR body / commit | tracker key | `part of PROJ-123` | `PROJ-123` |
| PR body / commit | whitespace-preceded `#N` | `see #42` | `issue-42` |

Branch precedence: a tracker key beats a bare number. Body precedence: `closes #N` beats a tracker key beats a bare `#N`. A bare four-digit branch segment in 1900–2099 is read as a calendar year and skipped (`release/2024-fix` attributes nothing; `release/2024-42` attributes to `issue-42`). `main`, `master` and `HEAD` never attribute. Markdown headings and hex colours do not match.

For Claude Code sessions the branch comes from the `gitBranch` field in the log. Every Claude Code path (`tierd score`, `tierd ship` and the live watcher) also reads the repository's git log. When the message was recorded on a feature branch (not `main`, `master` or a detached head), a branch-tip commit on a branch of the same name within ±30 minutes of the message supplies the issue id, and **that commit's issue id wins over the one in the branch name**; if several qualify, the first in `git log` order wins. Otherwise the branch name decides. The watcher reads the last 30 days of commits through a cache refreshed at most every 30 seconds. Attribution is fixed when a message is first stored: a commit that lands later does not re-attribute it, and a later `tierd score`, which re-reads everything, can differ. `tierd ship` sends events to a server that already holds the same keys, and the server keeps its first attribution.

**Worktree attribution (#823; opt-in, off by default).** Claude Code writes on every line of a session file the folder the session started in (`cwd`) and that folder's branch (`gitBranch`). A session started in the main checkout that then works in a git worktree (a second checkout of the same repository, on its own branch, made with `git worktree add`) still says `main` on every line, so its spend is booked to `unattributed:main`; subagents inherit their parent's `cwd` and branch and are booked the same way. The switch `--worktree-attribution` on `tierd serve` (its live watcher), `tierd ship` and `tierd score` (environment variable `TIER_WORKTREE_ATTRIBUTION`; on `serve` also the config key `watch.worktree_attribution`) attributes each message instead by the worktree its tool calls named, with the branch that worktree had checked out at that moment, read from git's own files; it never runs `git` for this. With it on, each Claude Code event records the rule that chose its issue, `branch`, `worktree-cwd`, `worktree-toolpath` or `carry`, exported as `attribution_rule` by `GET /api/v1/events`; rows stored with it off export as `legacy`. Spend in a worktree of a repository that is not configured goes to `unattributed:foreign-repo` (Section 5.3). It was released in v0.5.2 as a preview, off by default, because its measured accuracy fell short of the project's release gates; the measurement and its limits are in `docs/how-it-works.md`, section "Measured accuracy: read this before turning it on", which also gives the rules in order. Two consequences to weigh before turning it on: the server keeps the first issue it stores for each message and no command re-attributes a stored row, so a wrong attribution is permanent and turning the switch off later moves nothing back; and a `tierd` server older than v0.5.2 rejects with `400` every batch a `ship` with the switch on sends, so upgrade the server first. `tierd score --repo <path> --worktree-attribution` previews the effect and stores nothing: it scans the same session files with the switch off and on and prints what would change.

Because a GitHub issue number is unique within one repository, each row also stores a canonical `owner/repo` (from the webhook's `repository.full_name`, from `remote.origin.url` for local capture, or from the `X-Tier-Repo` header on the proxy). Rows whose repository cannot be determined store the sentinel `unqualified`, which joins tolerantly: an `unqualified` row matches the same issue id in any repository. So `issue-42` from two repositories can be joined to each other when one side is `unqualified`; naming the repository (`repo` on the API, `X-Tier-Repo` on the proxy) avoids that. `tierd repair-repo` moves one developer's cost rows off `unqualified` using a session-to-repository map the administrator supplies (dry run unless `--commit`). A contributor on a fork must name the upstream so their cost joins its outcomes: `tierd score --repo-slug owner/upstream`; `tierd ship --repo <path> --repo-slug <path>=owner/upstream` (repeatable, one per `--repo`); and for `serve --watch-repo`, the config map `watch.repo_slugs` (there is no `serve` flag).

### 5.2 Two identities that must be mapped

Cost is attributed to the shipping identity (the OS username, or `--developer <id>`); outcomes are attributed to the PR author's GitHub login. When these differ, `POST /api/v1/developer_alias {"alias":"alice-laptop","canonical":"alice"}` joins them. There is one alias per request and no bulk endpoint, so a team's aliases are posted by a script. Aliases are applied when a score is read, so an alias covers rows stored before it was created, and deleting it splits them again. Push-captured outcomes use the commit author's GitHub login and go through the same map. Without an alias, cost and outcomes land in separate rows: the cost row has no points and the outcome row has no cost, so each reads TIER 0 unless it has other matched activity. `/scores` reports the split as `data_quality.unjoined_developers`.

### 5.3 Unattributed spend

Spend that resolves to no issue stays in the denominator under a labelled bucket:

- `unattributed:main`: spend recorded on `main` or `master`. Its share of window cost is reported as `exploratory_cost_share`. The field has been computed this way since it was introduced; its name comes from an older description and does not mean the work was exploratory, overhead or never merged.
- `unattributed:branch-without-issue`: a named branch with no recognisable issue reference.
- `unattributed:detached-head`: no branch recorded, which is where Opencode spend lands, and Muse Code spend whose run recorded no branch.
- bare `unattributed`: producers that cannot see a branch, such as the org pollers and a proxy call without an issue header.
- `unattributed:foreign-repo`: with worktree attribution on (Section 5.1), Claude Code spend in a worktree of a repository that is not configured; its repository is stored as `unqualified`, so the other repository is never named.

`data_quality.attributed_cost_share` is the fraction of window cost that joined an issue; it and the buckets sum to 1.0. A large unattributed share means TIER could not see which issue the money went to, usually because branch names carried no issue number. It does not mean the money was wasted.

**When the headline reads low.** Linking spend to an issue does not move the pooled figure or a developer's figure by itself, because both divide all points by all cost. Among issue-linking gaps, the figure reads **low** in exactly one case: accepted work that recorded no outcome because its PR named no issue in the branch or the body (Section 4.1). (A separate, identity gap splits one person's cost and outcomes across two unjoined rows; Section 5.2.) Its cost is in the denominator with no points. That case and a low attribution share often share a cause, branch names without issue numbers: a PR from such a branch records an outcome only if its body names the issue (`closes #N` and the other forms in Section 5.1). So a low share is a reason to check, not a verdict. Backfill's closing line reports how many merged PRs recorded nothing (`unattributed=N` beside `inserted=N`). Spend recorded without an issue while its PR did record an outcome (for example sessions logged on `main` while the PR, on its own branch, named the issue; or Opencode) does not bias the figure. It loses the pairing instead: the segments leave that cost out, and the outcome may be flagged zero-token, which takes its row below the evidence floor (Section 7.1).

Run `tierd doctor --repo <path>` to check local issue attribution. Its issue-attribution check divides issue-attributed dollars by feature-branch dollars (attributed plus `unattributed:branch-without-issue`). Below the `--min-attribution` floor (default 0.5) the check fails with a non-zero exit; from 0.5 to below 0.9 it warns; 0.9 and above is healthy.

Spend on `main` or `master`, detached-head spend and bare-unattributed spend are left out of that ratio. A separate, informational check named `spend not on a feature branch` reports them. So the doctor's ratio is not the dashboard's `attributed_cost_share`, which divides by all window cost.

The API distinguishes three cases:

- **Unattributed spend** — cost the collector could not tie to any issue (no branch reference, mainline work, detached head). It has no issue id.
- **No-outcome spend** (`no_outcome_cost_usd` in `segment_reconciliation`) — cost that **did** resolve to an issue id, but for which **that developer** has no recorded outcome on that repository and issue inside the window: abandoned work, work still in flight, a PR that never merged, or an issue a colleague merged. When two people work one issue and one merges it, the other's spend is always no-outcome spend; TIER has no notion of pairing or shared credit. It counts as a cost with nothing to show for it yet.
- **A merged PR with no issue reference** — the outcome side of the same gap: the merge happened, but it records no outcome at all (Section 4.1). Its cost, if the branch had no reference either, is in the unattributed bucket.

`segment_reconciliation` accounts for the window with the identity `outcome_linked + no_outcome + unattributed == window_cost`, exact on the integer micro-dollar fields. The per-work-type segments divide by outcome-linked cost alone, so when there is any no-outcome or unattributed cost they can read higher than the pooled figure (an individual segment can still read lower, because its outcome mix differs). Section 2.1 says which figure a decision uses.

### 5.4 Two clocks, and why recent windows read low

This section and Section 5.6 describe free windows, which only `developer` mode serves; team and division mode serve sealed calendar months instead (Section 6.2). The denominator is windowed on the cost event's timestamp, which lands continuously as tokens are spent. The numerator is windowed on the outcome's timestamp, which lands at merge time (the webhook stamps `time.Now()` when it processes the merge; the API and backfill use `merged_at`; push capture uses the commit timestamp). The two halves are read independently over the same `[since, until)` and then joined by identity, not by time. So:

- **Trailing edge (near `until`):** cost has landed for work that has not merged yet. The ratio reads **low**. The default `/scores` window opens at the start of the UTC day 90 days ago with an open upper bound, so the newest weeks of a live deployment are always in this zone.
- **Leading edge (near `since`):** an outcome merges inside the window but its cost was spent before `since`. The ratio reads **high**.

In a window much longer than the time an issue usually takes from first spend to merge, both edges are a small part of the whole; this is a rule of thumb, and the project publishes no measured lead time. While a window's upper bound is still ahead of the clock (the default `/scores` window is open-ended), its number keeps moving as work merges and new cost arrives; it does not settle. Once `until` has passed, the window is closed to new PR merges: the webhook stamps a merge outcome at the time it processes the merge, so even a late delivery lands outside. A past window can still move: a `backfill` re-run or a `POST /api/v1/outcomes` (both stamp `merged_at`) and push capture (commit timestamp) can add outcomes; a revert up to 60 days after merge lowers an existing outcome's quality; and a late `ship` adds cost stamped at the original session time. The code defines no "settled" state and reports none. By the rules above, a window has mostly stopped moving when `until` is in the past, 60 days have passed since `until`, and no revert committed within 60 days of an outcome in the window is still waiting to be pushed (a revert is timed by its commit timestamp, so a late push can still lower a quality; Section 4.4), every laptop has run `ship` since `until`, and no backfill is pending. Administrative actions can still change it afterwards: an API post, a `/costs` correction, a reprice, an alias or hierarchy edit, or an erasure. To fix a figure for later comparison, record its manifest and check it with `tierd verify-report` (Section 9.6). Compare two such periods, never a mature quarter against the current open one.

### 5.5 The cost horizon

Every installation has a **cost horizon**: the timestamp of the earliest cost event in its store (`SELECT ts FROM token_events ORDER BY ts LIMIT 1`, scoped to the repository when `?repo=` is set). Outcomes backfill freely from tracker history; cost exists only from that earliest event forward.

The horizon is one timestamp for the whole installation, across every developer and every capture path (or one repository with `?repo=`). A developer or a tool whose capture started later is not reflected in it; `source_coverage_start` gives each capture path's own earliest event when there is more than one.

Loading history moves the horizon back. `tierd ship` writes events stamped with the session's original timestamps, so after a run the horizon moves back to the oldest session still on disk, at most 90 days by default (further with `--since`). Coding tools delete old session logs on their own schedule; the project's reading is that Claude Code keeps about 30 days by default, so for a Claude Code user a first `ship` usually reaches back about 30 days, not 90. On `serve`, the Codex, Opencode and Muse Code collectors' first pass loads every log on disk, and the Anthropic Admin and OpenAI Usage pollers write remainder events back to the first of the previous month. (`tierd score` only prints a report; it writes nothing and does not move the horizon.) A fresh install with nothing loaded has no horizon at all. One that has only tailed Claude Code live for a week has a horizon about a week old, or older if an older session file the watcher had not tracked before was appended to, because a file's first tracked write re-reads it whole (tracked files resume from their checkpointed offset). Extracted events outlive the logs they came from, so only an erasure (`DELETE /api/v1/developer/{id}`) that removes the earliest rows moves the horizon forward; it is a property of what was captured, not a retention setting.

A window reaching back past the horizon divides outcomes by cost that was never captured, and the number reads high. On one multi-repo installation, measured on 2026-07-26, the API's default 90-day window read about twice what the same installation reported for the last 30 days, roughly the period its cost capture covered. That is a single measurement, reported here as an illustration of size, not a rule.

A first trial is the common case. `tierd backfill` loads 90 days of merged PRs by default, while the session logs on disk may cover about 30. The older outcomes then have no recorded tokens against their issues, so they are flagged zero-token and take their developers below the evidence floor (Section 7.1), and a 90-day window reads high. Reading from `cost_coverage_safe_since` avoids both for the installation as a whole; it cannot show a developer or a tool whose capture started later, which `source_coverage_start` and the zero-token flags hint at.

`/scores` reports the horizon in `data_quality`: `cost_coverage_start` (the horizon timestamp), `window_predates_cost_capture` (an explicit `true` or `false`; absence means it could not be checked), `cost_coverage_safe_since` (the horizon's UTC date, or the next day when the horizon is not exactly midnight; the earliest `since` that clears it), and `source_coverage_start` per capture path. The dashboard shows the same as a banner, and `tierd doctor --repo <path> --server <url>` reports a named "cost horizon" check; that check reads the server's `/scores`, so it runs only when `--server` is given (Section 9.3 lists which checks run without one). A sealed team or division month never carries `cost_coverage_safe_since`; there the horizon sets the earliest month that can be sealed (Section 6.2).

### 5.6 Window matching in practice

`tierd backfill` and `tierd ship` both default to the last 90 days so cost and outcomes cover the same period; `GET /api/v1/scores` also defaults to 90 days, while the dashboard's "From" date defaults to 30 days ago. Whatever window you read, check it against `cost_coverage_safe_since`: a 90-day window over an installation whose cost capture began 30 days ago divides three months of outcomes by one month of cost and reads high.

The project's published snapshot, from its README: measured on the TIER repository over the trailing 30 days to 2026-07-21, 221 weighted points per $1,000 across 152 merged PRs, with 72.1% of spend not linked to an issue, so attribution coverage was about 28% (100 − 72.1 = 27.9). It is an illustration, not a benchmark, and four limits apply. The README records neither the price-table hash nor the mode, and the figure was priced under an earlier table than version 10, so by Section 2.1 it does not compare with v0.5.2 output. At 28% coverage the dashboard today labels such a headline provisional (Section 7.1). Its cost is Claude Code JSONL cost, so if the open doubt in Section 3.1 holds, the figure is too high. And whether it also read low depends on how many of its merged PRs recorded no outcome (Section 5.3), which the snapshot does not report.

---

## 6. Aggregation and privacy: modes, k-anonymity, suppression, what it must never be used for

### 6.1 Three modes, no default

`tierd serve --aggregation <mode>` is required. There is no default; `serve` refuses to start without it (env `TIER_AGGREGATION`, config key `aggregation`), so an existing deployment's reporting posture never changes on upgrade. **The mode changes only what is served, not what is stored**: every mode stores per-developer rows, and the mode is not recorded in the database. So restarting in `developer` mode shows named per-developer rows for all history, including anything collected under `team` mode. Anyone who controls the server's startup, or can read its database file, can see individual rows whatever the mode.

- **`developer`**: per-developer rows keyed by the developer identifier (the GitHub login, or the OS username a cost row was shipped under; Section 5.2), with no display names. This is the coaching mode of Section 6.3, which also says who can see these rows today and what the law of the European Union and European Economic Area (EU/EEA) is read to require. Two team views exist in this mode, and neither is k-anonymised: `team_rollups`, one row per team in the hierarchy plus an `unassigned` row, summing to the total; and `?team=<name>`, which adds a `team` rollup for that team's members without filtering `developers[]`.
- **`team`**: team-level aggregates from the `org_hierarchy` map, k-anonymised, never naming an individual, and served as sealed calendar months (Section 6.2).
- **`division`**: one level higher in the same hierarchy, under the same k floor and the same suppression guards.

Team and division modes read group labels from `org_hierarchy` (`PUT /api/v1/org_hierarchy/{developer}`, a bulk `POST`, or `tierd hierarchy import --server <url> --api-token @<file> <file.csv>`, which posts a CSV (comma-separated values) file with the header `developer,team,division,org` to that server in one all-or-nothing request and never removes anyone; `--dry-run` only validates). An entry can be overwritten with `PUT` but not removed (there is no delete route; only a developer's erasure removes it). Membership is dated (#886): every hierarchy write is dated by the server clock, each spend event counts toward the team its developer was in at the event's own timestamp, each merged outcome toward the team at merge time, and each invoice toward the team at the start of its month, so moving a developer affects only the future. History from before a developer's first assignment stays in `other`. Alias edits are dated the same way (#914): an alias still joins earlier spend to its person's row (Section 5.2), but that spend keeps counting toward the team its identifier was in when it was recorded. An empty hierarchy is not refused: `serve` starts, logs a warning at startup, and every developer resolves to the unnamed group, which folds the whole company into a single anonymous `other` row; and because only people on the hierarchy count toward k (Section 6.2), an empty hierarchy counts no one. Populate the hierarchy before reading team-mode output as team-level data. The local `tierd score` command reads only the invoking user's own logs, has no server, and takes no mode.

### 6.2 k-anonymity, suppression and sealed monthly reports

**k-anonymity** here means a group is shown by name only when at least k people count toward it. The floor is `--k-anonymity` (env `TIER_K_ANONYMITY`, config `k_anonymity`), **default 5, hard minimum 3**; `serve` refuses a smaller value. An identifier with spend or merged work in the window counts as a person only if all three hold (#856): it is on the hierarchy at some point during the window, after aliases are joined, so one person's identifiers count once (Section 5.2); it is not a bot (GitHub account type `Bot`, which the webhook records, a login ending in `[bot]`, or a known bot login such as `Copilot` or `dependabot`); and it has captured activity in the window, meaning spend a collector or the proxy recorded, or a merged pull request. Spend entered through `POST /api/v1/costs` and merged work captured only from direct pushes do not count on their own, and neither does an identifier whose only figure in a group is paid spend. An identifier that does not count keeps its figures in its group but does not make the group bigger; `data_quality.uncounted_active_ids` reports how many did not count, and why (`manual_only`, `push_only`, `not_on_roster`, `bot`).

Each figure also needs its own k. A group's cost is shown only if at least k counted people in it have captured spend, its points only if at least k have merged work, its paid spend (and so `spend_leverage`) only if at least k have paid spend, and each non-zero part of the `coverage_pct` split only if k counted people carry it. Captured spend is judged per published group, so a person who moved team during the window carries only what was captured while they were in each (#943). Spend under the `unattributed` pseudo-developer (an org poller's remainder, a proxy request with no developer header) is left out of every figure in these modes, and `data_quality.excludes_unattributed_spend` is always `true`. k-anonymity hides who is in a small group; it does not protect against a reader who already knows the other members. It protects what a read-token holder sees: anyone holding the write token can edit the hierarchy and the aliases that decide who counts, so treat that token as an admin credential (#908 is filed to separate the two).

**One breakdown per report (#864).** A team or division response carries only the group rows and the company `total`. It leaves out `work_types[]`, `segment_reconciliation`, `cost_composition` (with its per-model rows) and the `data_quality` fields `attributed_cost_share`, `unattributed_buckets`, `exploratory_cost_share` and `mixed_price_versions`; `data_quality.attribution_coverage` reads `"not_shown"` instead, and `?work_type=` is a `400`. Each of those was a second breakdown of the same data, and two breakdowns subtract to a group smaller than k even when every published row has k people: team totals minus work-type totals gave a 3-person group's spend, and a model only one developer used showed that developer's spend. #937 tracks an audit that could bring the work-type and per-model views back.

**Suppression.** A group with fewer than k counted people folds into a residual row labelled `other`, which also holds everyone with no team. When `other` has at least k counted people, or nothing at all was measured in it, it is emitted. Otherwise, even with no counted person in it, the **whole response is withheld**: every named team row, `other` and the `total`, because a view that folds a named team into its own `other` row (a comparison of two months does) would otherwise let a reader subtract that team's row and read the hidden group. The response says so in `data_quality.kanon_suppressed`, with `developers` (how many counted people were withheld), `k_anonymity` (the floor in force) and flags for what was withheld, among them `withheld_total` and `withheld_teams`. A consumer must key off this field to tell "withheld" from "no data". A group can be withheld with k or more people in it when one of its figures is carried by fewer than k of them.

**Sealed monthly reports (#913).** In team and division mode, `GET /api/v1/scores`, `GET /api/v1/report_manifest` and `GET /api/v1/scores/compare` answer only from sealed calendar months, because two overlapping windows subtract to a group smaller than k. `/scores` and `/report_manifest` take `?period=YYYY-MM`, or nothing for the latest sealed month; `/scores/compare` takes `?period_a=` and `?period_b=`, the earlier month first, or neither for the two latest sealed months. `since`, `until`, `before`, `team`, `repo` and `work_type` are `400`s. `/scores` returns the stored body byte for byte, so its SHA-256 digest equals the manifest's `body_digest`, and names the month in response headers such as `Tier-Period`. A comparison names a team only if it clears k in both months, and is withheld if either month's own `other` row, or the comparison's, does not reach k; two months sealed under different configs are a `409`.

**When a month seals.** A writable `serve` runs a seal pass at startup and then every hour. Each pass seals, oldest first, every owed month whose grace lag has ended: `--report-grace` (env `TIER_REPORT_GRACE`, config `report_grace`), default 14 days, minimum 24 hours. A month also waits, with the stall reason `source_behind`, until every source `serve` polls or scans (the organisation pollers, the subscription-fee reconciler and the scheduled log collectors such as Codex and Opencode) has settled past its end without a gap. Spend a source can never read again, such as an excluded Muse Code subagent call or a deleted log that held a call, is recorded as lost and holds every month it touches until `tierd seal --skip`. The live Claude Code watcher, webhooks, `tierd ship` and `POST /api/v1/costs` are covered by the grace lag alone. A month that fails to seal holds back every later one. Once sealed, a month is stored and served unchanged: cost a laptop ships after its month is sealed, and later alias or hierarchy edits, never change that month's report. A `--read-only` `serve` never seals; it serves what a writable server sealed in the same database.

**Arming, and the one-way upgrade.** Nothing is sealed until the administrator arms sealing, once, after any history is loaded. `tierd seal --arm YYYY-MM` (or `--arm earliest`, the first full month of cost data), run with the same config, flags and environment as `serve`, seals that month first and pins it as the earliest month ever sealed; setting `seal_from` (`--seal-from`, env `TIER_SEAL_FROM`) arms sealing too. Until then those three reads are a `404` whose error begins "sealing not armed". The earliest sealable month is the later of the armed month and the first full month after the cost horizon (Section 5.5), and the first seal pins it for good, so older data imported afterwards never appears in these modes. No month seals, `--arm` included, until `serve` has started once on the database and registered its sources, and a month before a source's first recorded coverage never seals automatically: arm a later month, or set the subscription's `active_since`. `tierd seal --skip YYYY-MM --reason TEXT` records a month that cannot seal as a permanent gap, so later months can seal; the reason is served to every reader and can never be erased, so it must not name a person. `tierd seal --status` (with the same config, flags and environment as `serve`), and `tierd doctor --server`, report a stalled month and why. A sealed month is never recomputed: a change of aggregation level or k applies from a later month, and `serve` warns at startup naming the first month the new config applies from. The upgrade moves the database to schema version 6, which an older `tierd` refuses to open. On the dashboard, a "Sealed month" picker replaces the date controls in these modes. `developer` mode is unchanged and still serves any window.

**Other endpoints in an anonymised mode.** `GET /api/v1/scores` returns `teams[]` and an empty `developers: []` with an `aggregation` discriminator; `GET /api/v1/scores/{developer}` returns a blanket `404` for every path value; the row-level exports (`/events`, `/outcomes`, `/quality_events`, `/quality_history`) and `/fidelity` return `403`. The read token also gets `403` on `/metrics` (#944), because two scrapes of its running counters a short interval apart give one person's spend and activity timeline. Scrape it with the write token or with a scrape-only metrics token (`--metrics-token`, env `TIER_METRICS_TOKEN`, config `http.metrics_token`), which opens `GET /metrics` and no other route. Its holder, and anything that can query the Prometheus store that keeps its scrapes, sees the same per-person timeline the read token is refused, so treat both as operator-only (`docs/security.md`); `serve` refuses to start when any two of the write, read and metrics tokens are equal, and an install with no token configured keeps `/metrics` open. The data-protection export and erasure endpoints (Section 6.4) remain available to the admin token in every mode, because they are compliance tooling.

### 6.3 What the numbers are for, and what they must never be used for

TIER is a diagnostic of how efficiently a body of work turned AI spend into accepted outcomes. It exists to help people improve how they use AI: which model they use for which work, how they reuse context and cache, and whether their spend starts from an issue. By the operator's ruling of 2026-09-26 (issue #826), a manager **may** look at an individual developer's numbers in `developer` mode in order to coach: to discuss model choice, cache reuse and issue-linked work with the developer. The same ruling sets the line: the numbers **must never be used to evaluate the developer: not as an input to pay, compensation, promotion, individual performance reviews, performance improvement plans (PIPs), forced ranking, discipline or dismissal.** Permitted use is discussing practice with the developer (model choice, cache reuse, issue-linked work); the number must not be treated as a verdict on the person. TIER cannot enforce that line; only an organisation's own policy or works agreement can.

There are two reasons for the line. The first is the project's reading of the law, stated here so a reader can check it with counsel; it is not legal advice, and the project's longer notes are in `docs/legal-and-privacy.md`. In Germany, per-developer measurement is subject to works-council co-determination (§87 of the Works Constitution Act, *Betriebsverfassungsgesetz*, BetrVG). In France it requires consultation of the works council (*comité social et économique*, CSE); in the Netherlands, the works council's consent (Article 27 of the Works Councils Act, *Wet op de ondernemingsraden*, WOR). The project reads most of the EU/EEA as similar. Using the output alone to drive a human-resources (HR) decision would likely engage Article 22 of the General Data Protection Regulation (GDPR), on decisions based solely on automated processing, and the project expects a data protection impact assessment (DPIA) for any per-developer deployment. For measured employees in the EU/EEA the project recommends team-only aggregation plus advance consultation. The second reason is method: a per-developer figure moves with task mix, seniority, pairing and review load, and a number tied to a person's evaluation invites gaming (Section 11).

**Who can see a developer's numbers today, and what is planned.** Access in `developer` mode is not per viewer today. Anyone holding the deployment's read or admin token sees every developer's row (`/scores`, `/scores/{developer}` and the row-level exports are read-scoped; Section 10), and on a loopback bind with no admin token, so does anyone who can reach the server. Cost events and outcomes are never deleted by age (Section 6.4). The dashboard's per-work-type panels still draw one bar per named developer on a shared scale, which invites comparing colleagues; the rows are listed by identifier, not by TIER. The #826 design adds the following, all **planned, not in v0.5.2**, with no release date published: per-viewer tokens (the developer, their declared manager and an optional skip-level viewer), with the shared read token limited to name-free pages; a log of who viewed each developer's page, which that developer can read; retention in `developer` mode, 90 days by default with a 365-day ceiling (the operator's stated reason: a full annual coaching cycle, not annual reviews), with `serve` refusing to start in that mode without it; the organisation's history kept in a name-free roll-up; and a per-developer coaching page to replace the shared-scale panels. Until they ship, treat the read token as access to every developer's data.

**Comparisons that are unsupported.** Three, by construction:

1. **Raw TIER across organisations.** List prices put both organisations' cost on one price basis, but the numbers still do not compare. Size labelling differs (a generous labeller scores the same work higher), task mix differs, and capture coverage differs (an uncaptured tool reads as free).
2. **Any two figures that fail the comparison rule of Section 2.1** (different `table_hash` or `rubric.version`, or mixed price versions).
3. **One work type against another** (Section 4.3).

### 6.4 What is stored, and what is never read

TIER's log parsers decode only an allowlist of metadata and usage fields, so prompt and completion text never reach a stored row from those paths. Two paths differ. The reverse proxy forwards complete request and response bodies to and from the provider (it records only usage). The webhook stores each raw GitHub delivery body, which can contain arbitrary PR and commit text, pasted code included.

Everything is stored in one local SQLite file. TIER sends no telemetry to the project or to anyone else. Beyond the `tierd` server the administrator names (which `ship`, `doctor`, `hierarchy import` and `healthcheck` contact), it makes outbound calls only when used that way: the proxy (mounted by default) forwards a client's own calls to the provider, `tierd backfill` reads the GitHub API, and the org pollers, when configured, read the providers' usage APIs. The file holds developer identifiers, issue ids, repository slugs, model names, token counts, list cost in micro-dollars, price versions, an opaque session UUID, PR metadata (number, author login, weight, quality, merge SHA), quality events and finance ledgers. In team and division mode it also holds the sealed months (Section 6.2): each month's served report, the per-group totals it was built from before small groups were folded (never served; a group with one member holds that person's totals), and, for each group and figure, a keyed hash of each person counted, computed from their canonical id under a random per-install secret, which anyone holding the database file can recompute from a guessed id. Two conditional stores hold personal data an auditor should note: the watcher's checkpoints of how far it has read each session file (live `--watch-repo` mode; they hold the working-directory path, branch and session id, and with worktree attribution on, the path of a worktree) and raw GitHub webhook bodies (webhook path enabled; commit author names and emails, PR titles and commit messages).

**Retention today.** Nothing is deleted by age except the raw webhook bodies, which are cut to the last 90 days and 50,000 rows each time a process opens the database and every 24 hours while `serve` runs. Cost events, outcomes and quality events are kept until erased. (Developer-mode retention is planned; Section 6.3.)

**Erasure and export.** `DELETE /api/v1/developer/{id}` erases, in one transaction, the developer's (and their aliases') rows in cost events, outcomes, per-developer paid spend, hierarchy and dated team membership, seat membership, quality events, quality history, the repository-repair log and the push-capture commit ledger and audit, then their alias entries and canonical-id history. It erases the watcher's tail-state for the person's sessions with a tombstone the live watcher obeys, deleting the row instead when the session file is gone (#919); `docs/privacy.md` lists its limits, among them that a session that stored no events cannot be found. It does not touch raw webhook bodies, organisation-level paid spend, or the reprice and correction audit logs, and apart from that tombstone it leaves no record that blocks re-import: a later `ship` or `backfill` can store the same rows again. No endpoint removes a person's raw webhook bodies; they go with the 90-day cut, and the repository documents no other procedure. Backups taken with `tierd backup` are separate files that erasure does not reach. A sealed month keeps an erased person's contribution: erasure changes no sealed report or total, because removing it would let a reader recover the person's figures by subtraction; it replaces with random tombstones the keyed hashes it can reach from the person's current ids and their recorded alias links. A month reached only through an alias since deleted or re-pointed keeps its key until that retired id is also erased by name (`docs/privacy.md`). `GET /api/v1/developer/{id}/export` produces the access-request file. TIER is single-tenant: developer identifiers are global, there is no `tenant_id`, two organisations' `alice` rows would collide, and it must not run as a shared multi-organisation service.

---

## 7. Reading the number: the evidence floor, confidence and `significant`, trends versus comparisons

### 7.1 The evidence floor (`ranked`)

A row clears the **evidence floor** when all three hold: at least **3** outcomes in the window; at least **$5.00** of list-price cost (all of the row's cost, including spend not linked to an issue); and **zero** flagged outcomes. The wire field for this is `ranked: true`, a name kept for compatibility; it marks the floor and nothing else. It is not a position in any list, and nothing in TIER sorts by it or by TIER. The floor never changes a number; it changes what the number may claim. Every row below the floor, developer or team, comes back from `/api/v1/scores` with its `tier`, `weighted_points` and `total_cost_usd` exactly as computed and `ranked: false`. A developer row that clears the floor gets a confidence interval (Section 7.2); one below it has `ci_low = ci_high = 0`, the wire spelling of "no interval". Team and division rows never carry an interval; for them the floor decides only what the dashboard draws and whether the headline shows a number. The floor exists because one 0.5-weight PR against $0.0004 of cost yields a TIER around 1,250,000, a figure that says nothing about the work. The thresholds are fixed in the code; no flag changes them. They are the project's judgement; it publishes no study showing they are sufficient, or how often they flag work that did use captured AI. AI work done more than 14 days before an issue's latest merge does not count toward the zero-token check, so long-running work can be flagged.

A team or division row applies the same three conditions to its **summed** inputs. Five developers with one outcome and $2 each are each below the floor; their team, with 5 outcomes and $10, clears it, and at the default k of 5 it is also named (Section 6.2).

**Order.** The API returns developer rows in identifier order and team rows in name order (the `other` row last). The dashboard keeps that order: "Rows are listed alphabetically, not by TIER." A developer row below the floor shows its TIER muted, tagged "below evidence floor", with no bar drawn. In the dashboard's per-work-type panels the floor is tested against that segment's own cost and outcomes.

**The dashboard headline** is the rollup over every row (the `total` block). It shows a word instead of a number in four cases and keeps the measured inputs on screen in each: "NO SCORE" when the window has no weighted points (nothing accepted is the absence of a score, not a score of zero); "FREE" when it has points at zero recorded cost; and, when the rollup is below the floor, "NOT ENOUGH SPEND TO SCORE" under $5.00 or "NOT ENOUGH EVIDENCE TO SCORE" otherwise. "NO SCORE" and "FREE" also appear on individual rows.

Separately from the floor, the dashboard qualifies the headline by attribution coverage: when `data_quality.attributed_cost_share` is below 0.5, the value is dimmed and labelled "provisional — N% attribution coverage", and the attribution banner turns to a warning. It says most of the cost joined no issue; Section 5.3 says what that does and does not imply. Team and division reports do not publish `attributed_cost_share` (Section 6.2), so their headline is labelled "provisional — coverage not shown", without the dimming. A row can clear the floor and be provisional, or the reverse.

The **zero-token check** is the third condition. An outcome is flagged when its developer recorded fewer than **1,000** tokens (input, output and every cache class summed) on its (repository, issue) in the **attributable window**: the 14 days up to and including the timestamp of the latest outcome on that (repository, issue) in the scored window, by any developer. That window may start before `since`; the check reads those tokens even though they are outside the scored window. Several outcomes on one issue (for example daily push-capture outcomes) share one window. The flagged outcome keeps its full points; the developer's row falls below the floor. The check sees only recorded tokens, so it cannot tell its causes apart: AI work TIER never captured (off-books usage, G-02 in Section 11), broken identity mapping, spend that cannot join an issue (Opencode, Section 3.1), session logs that are gone (Section 5.5), or a PR written without AI. The project accepts flagging the last case, because missing uncaptured AI use would be worse. In a large group, where almost any window holds one such outcome, this keeps the group row and headline below the floor; the project has no remedy for that beyond linking branches to issues and capturing every tool. The flag propagates: a team or division row is below the floor unless its members' flagged count is zero, so **one flagged outcome from any member takes the whole group row below the floor**, and the headline with it. In team mode the per-developer list is suppressed and a name-free `zero_token_outcome_count` is emitted.

### 7.2 Confidence intervals

For developer rows that clear the floor, `/scores` returns `ci_low` and `ci_high`, a 95% interval from a **percentile bootstrap** over the developer's own outcomes. Each of 1,000 replicates (a fixed number, not configurable) draws the developer's outcomes with replacement. Each draw carries its `weight × quality` and its share of cost: the developer's own spend on that (repository, issue), split evenly across the developer's outcomes on it. The developer's cost that joins none of their outcomes (spend not linked to an issue, and spend on issues where they have no outcome) is added back as a fixed term. TIER is recomputed per replicate, and a replicate whose cost comes to zero or less is dropped. The bounds are the 2.5th and 97.5th percentiles of the replicates kept, by nearest rank. The random seed is fixed, so the same stored rows give the same interval on every read. `cost_per_point_ci_low` is 1,000 ÷ `ci_high` and `cost_per_point_ci_high` is 1,000 ÷ `ci_low` (the bounds swap); if either TIER bound is 0, both read 0. The interval is self-relative: it says how much this developer's own number could move under resampling, not where it stands against anyone. In the rare case that no replicate is kept, the interval reads 0 to 0, as for a row below the floor. The method assumes the developer's outcomes are independent draws, which work on one issue may not be; the project has not studied how often the interval covers the true value. Team, division and total rows carry no interval. Developer rows inside each work-type segment carry their own interval, computed the same way over that segment's outcomes and cost.

### 7.3 `significant`

`GET /api/v1/scores/compare?since_a=&until_a=&since_b=&until_b=` compares two half-open windows ("A = before", "B = after"; every delta is B − A). Those four are the **only** query parameters it accepts; `work_type`, `team`, `repo` or anything else is a `400`, and the response carries no `work_types[]` segmentation. In team and division mode it compares two sealed months instead, named with `period_a` and `period_b`, and the four window parameters are a `400` (Section 6.2). A developer row's `significant` is `true` only when the developer is present and clears the floor (`ranked`) in both windows **and** the two 95% intervals do not overlap (`a.ci_high < b.ci_low` or `b.ci_high < a.ci_low`; touching ends count as overlap). Any overlap, an absent side, or a window below the floor gives `false`, which is not the same as showing there is no difference. The flag is a fixed rule over intervals whose coverage has not been studied (Section 7.2), not a validated statistical test. Nothing corrects for many comparisons, so across a large team a few rows can read `significant` by chance. In team or division mode `significant` is **always `false`**, because group rows have no interval, so a team-mode delta is directional only. Team, division and `total` compare rows carry `ranked = a.ranked && b.ranked`. A developer row has no combined field: read `a.ranked` and `b.ranked` with `present_a` and `present_b`.

### 7.4 When to read it

Check `window_predates_cost_capture` first; if it is `true`, set `since` to `cost_coverage_safe_since` and read again. Prefer long windows, and compare two windows that have stopped moving (Section 5.4). A rise in TIER (equivalently, when both windows have recorded cost, a fall in `cost_per_point`) with `significant: false` is "promising, keep watching", never proof that a change worked. Compare within one `work_type` segment: `/scores/compare` cannot filter by type, so read `GET /api/v1/scores?work_type=<type>&since=&until=` once per window and compare the two segments yourself, accepting that this two-call form has no `significant` flag. It is not available in team or division mode, which publishes no per-type segment and refuses `?work_type=` (Section 6.2). Trend `cost_per_point` over time, and compare teams only as far as Section 2.1 allows.

`coverage_pct` measures capture fidelity, not completeness: (labelled "Fidelity" in the dashboard) is the share of **captured** spend that arrived per request rather than as a daily or estimated total. It says nothing about spend TIER never saw, and it reads near 100% under JSONL capture even when most spend is linked to no issue. Attribution is `attributed_cost_share`.

---

## 8. Spend Leverage, and what the number supports in a spending decision

### 8.1 Spend Leverage

**Spend Leverage = `total_cost_usd / actual_paid_usd`**: the list-price value of the usage TIER recorded, divided by what finance recorded as paid. It is a separate figure and **never changes TIER**, which always divides by list-price cost whatever was paid. It is server-only, because it needs a finance figure.

**Which months count.** Paid spend is posted per calendar month (`YYYY-MM`) and never pro-rated. A window includes every month from the month containing `since` up to, but not including, the month containing `until`. So a mid-month `since` brings in its whole month and a mid-month `until` drops its whole month. With no `until`, every month from `since` onward counts, including months not yet over. Use month-aligned windows (`since=2026-04`, `until=2026-07`) whenever leverage matters. The dashboard's default window (30 days back, no end) does not line up: it sets the full paid amount of every month from the one 30 days back through the current month (usually two months, sometimes one or three) against 30 days of list cost.

**Reading it.** **Above 1**: the organisation paid less than list for the recorded usage (a discount, a commitment, a subscription). **Near 1**: what metered billing at list gives, though discounts, capture gaps and seat allocation can each move it. **Below 1**: it paid more than list, for example a flat fee that was under-used that month. As an illustration, usage worth $3,280 at list on a plan that cost $200 for the month is 3,280 / 200 = 16.4×. On the wire, a row with no finance entry has `actual_paid_usd` 0 and `spend_leverage` 0. A row whose net paid is zero or negative has `spend_leverage` 0 and carries the net in `actual_paid_usd`, so a negative net is visible but a net of exactly zero looks like no entry. The dashboard renders 0 as "—", and when the window's total paid is negative its leverage tile reads "(credit)" with the balance beneath.

**Posting what was paid.** Finance posts US-dollar amounts at one of two grains; the per-developer grain wins where both exist. Every post adds a row: there is no idempotency key, so posting the same invoice twice doubles it. TIER records whatever dollar figure is posted for a month; the accounting basis (cash, invoice or accrual) and the treatment of tax, currency and prepaid credit are finance's choice, and TIER does not check them. Credit memos and refunds are negative rows, corrections are deltas, and the net is summed when read; a multi-month window sums each month's net.

- **Per developer:** `POST /api/v1/actual_spend {"developer","period":"YYYY-MM","actual_paid_usd"}`, for tools billed per seat.
- **Org level:** `POST /api/v1/org_actual_spend {"org","period","actual_paid_usd"}`, for one contract covering many seats. Each seat's share is `org_total / seats`, where the seats are the developers whose `period_membership` is open in that month. Membership is kept by month: joining mid-month counts as a full seat for that month. A developer's first hierarchy entry with an organisation opens a membership reaching back to every earlier month, so they count as a seat on all past invoices; re-enrolling a developer whose organisation was cleared, or whose membership was ended, opens the new seat in the current month instead (#867). Moving to another organisation makes them a seat in both for that month. `POST /api/v1/period_membership/{developer}/end` closes a membership so leavers stop diluting the share. **With no hierarchy, or no organisation named in it, there are no seats, and an org-level invoice is allocated to no one.** When both grains exist in a month, the org-level seats split what is left after the per-developer rows; that remainder is clamped at zero, and if every seat has its own row it goes to no one.

Team leverage is `Σ list cost / Σ paid`, never an average of ratios. Under a `?repo=` scope, `spend_leverage` and `actual_paid_usd` are reported as 0 with `data_quality.spend_leverage_suppressed: true`, because an invoice carries no repository.

**Subscription plans.** Token value and subscription fees are recorded separately. The price table says what a subscription route's tokens are worth at a comparable per-token rate (a row marked `billing_mode: subscription`); the `subscriptions:` config block says what the flat fee was (`route_prefix`, `org`, `monthly_fee_usd`, optional `active_since`), and `serve` posts that fee to `org_actual_spend` under the source `subscription:<route_prefix>`: at startup and hourly, the full fee for each month (never pro-rated), back to `active_since` at startup, posting only the difference from what is already recorded for the current month, so restarts do not duplicate it and a fee change corrects the current month only. The fee never enters the denominator. A "subscription row" here means a row marked `billing_mode: subscription`; the embedded table ships none (its GLM rows are ordinary per-token rows), because for most plans a comparable rate is a judgement with no first-party source, and `serve` refuses to start when a `route_prefix` matches no subscription row in the active table. So on the embedded table, record a flat fee by posting it yourself with one of the two endpoints above. That covers a Claude subscription too. The Anthropic Admin API poller reads the organisation's API usage and cost, and by the project's reading a subscription plan's fee does not appear there, so post it by hand.

The Z.ai coding plan is one of two flat-fee routes the embedded table has per-token rates for; the other is a Muse subscription, whose calls are priced at Meta's per-token list rates (Section 3.1). Z.ai publishes per-token API rates for the same GLM models, and the operator ruled on 2026-09-25 that those published rates are the value of coding-plan tokens. That is a valuation choice, not a price the plan charges, and it is why the plan's `glm-5.3` spend needs no subscription row. Ollama's cloud tier publishes no per-token rate for its models at all, so there is nothing comparable to price it with, and it is not captured (Section 3.1).

**Leverage is not a saving.** On a flat or subsidised plan leverage can run far above 1 (the 16.4× illustration). That does not mean the organisation saved that multiple. The ratio says what the plan bought at list prices; it does not say what metered billing would have cost, because usage might have been different under it. The dashboard tile is labelled "metered cost / paid spend" for that reason. Leverage also inherits every gap in capture: usage TIER did not record lowers the numerator, and an invoice that covers tools TIER does not capture raises the denominator.

### 8.2 What the number supports in a spending decision

How to use TIER for the decisions the white paper says it may inform:

**What a change over time tells you.** Within one organisation, under the comparison rule (Section 2.1), for two windows that have stopped moving (Section 5.4): a falling `cost_per_point` in one work type means recorded AI cost per weighted point of that type fell. It does not say the work was the same, or why. Four ordinary causes move it with no change in efficiency: a shift in the mix of work, a change in how generously size labels are applied, a change in capture coverage (a newly used tool that is not captured makes work look cheaper), and a window edge. Rule each out before reading a change as an efficiency gain. For one developer's pooled figure, `significant` (Section 7.3) is the only test TIER offers. A team-level change, and any per-work-type change, is directional only, because `/scores/compare` cannot filter by type.

**How complete the cost side must be.** TIER cannot see spend on a tool it does not capture, and no field reports that spend. So first confirm by hand that every AI tool the team uses is captured by some path (Section 3.1). Then:

- `window_predates_cost_capture` must be `false`.
- `attributed_cost_share` should be at least 0.5 (it is published in `developer` mode only; Section 6.2), the code's only threshold: below it the dashboard labels the headline provisional. A low share weakens the segments and the evidence floor a decision would lean on, and is a reason to check for unrecorded outcomes (Section 5.3).
- `coverage_pct` (0 to 100) is not a completeness measure: it is the share of **captured** cost that arrived per call. A low value means more cost came from the org pollers or from imports. The code sets no threshold for it.
- For the open doubt on Claude Code's logs (Section 3.1, issue #837), TIER applies no correction. For usage billed through a provider's API console, the org pollers add the gap between the provider's daily totals and what capture recorded, as unattributed spend; they cannot see a subscription plan's usage. Before relying on an absolute dollar figure, compare one month of TIER's Claude Code cost with what the provider billed for the same usage. That check is possible only for API-billed usage: a flat subscription's bill has no token counts to compare. Prefer decisions that rest on a change within one capture path, which the doubt affects less if the error is stable; nothing in TIER tests that it is.

**Decisions it can inform:**

1. **Model or routing choice.** Change the default model or route for one kind of work, then compare `cost_per_point` for that work type across two matched windows, with `premium_model_share` (the share of dollars spent on models whose base input rate is $4.00 per million or more) and `cache_read_share` beside it (all three are served only in `developer` mode; Section 6.2).
2. **A tool trial.** Give a group a new AI tool for a window and compare its `cost_per_point` with its own previous window. This works only if the new tool is captured; if it is not, its spend reads as zero and the trial looks free.
3. **A team's trend.** Whether one team's cost per point moves after a change in practice, such as linking branches to issues or reusing context.
4. **Whether a flat plan is used.** Spend Leverage below 1 for several months means the fee was more than the list-price value of the usage TIER recorded. Whether metered billing would have cost less depends on usage under it, which TIER cannot know (Section 8.1).

---

## 9. How to use it: install, demo, see spend, full score

### 9.1 Install

Build from source (Go 1.26.9 or newer, `make`, `git`):

```sh
git clone --branch v0.5.2 https://github.com/tiermetric/tier.git
cd tier
make build                       # produces ./bin/tierd
```

Or install without cloning:

```sh
go install github.com/tiermetric/tier/cmd/tierd@v0.5.2
tierd version
```

Both commands pin the tag. `@latest` or a clone of `main` can give you a version whose flags and defaults differ from the ones documented here.

Each release on the `tiermetric/tier` releases page publishes per-platform tarballs (darwin and linux on amd64 and arm64, and windows on amd64), each with a `.sha256` checksum and a SLSA (Supply-chain Levels for Software Artifacts) build-provenance attestation you can check with `gh attestation verify <file> --repo tiermetric/tier`. It also publishes a container image to the GitHub Container Registry (GHCR) as `ghcr.io/tiermetric/tierd`, built for linux/amd64 only (on arm64, use the arm64 binaries).

The macOS binaries are not yet Developer-ID signed or notarized; by the operator's ruling of 2026-09-26 (issue #822), notarization and a Homebrew tap come in a later release. A binary downloaded with a web browser is marked as downloaded, and macOS's Gatekeeper may refuse to run it. Remove the mark once with `sudo xattr -d com.apple.quarantine /usr/local/bin/tierd` (use the path where you put it). A binary built from source or with `go install` carries no such mark. The download, checksum and extraction commands are in `docs/quickstart.md`. The Windows archive is built but untested: there is no Windows test run, and the database file-permission protection does not apply there. Treat Windows as unsupported. The default log locations are built from the home directory, so they resolve on Windows too.

The commands in this section write `tierd`, meaning the binary on your `PATH`. `go install` puts it in Go's binary directory (`$(go env GOPATH)/bin` unless `GOBIN` is set); add that directory to `PATH` if `tierd version` is not found. From a source checkout, use the full path to `bin/tierd` instead, for example `~/tier/bin/tierd`; the relative `./bin/tierd` works only while your current directory is the checkout.

### 9.2 See it in one command (synthetic data)

```sh
tierd demo                       # then open http://127.0.0.1:8080
```

The demo seeds a throwaway database (`tier-demo.db` in the system temporary directory, recreated on each run and left in place on exit) with `demo-*` developers, `DEMO-*` issues and an `ACME (DEMO)` organisation and prints a synthetic-data banner. It runs `serve` in `--read-only` mode: only the read, health and open routes (the dashboard and `/docs/`) are mounted, and the write, ingest, admin, webhook and proxy routes and every collector are structurally absent rather than token-gated. Because capture is disabled in that mode, `--read-only` also lifts the `--watch-repo` requirement: `--codex-rollout` or `--opencode` beside it produces a startup warning that the spend will not be recorded, rather than a refusal to start. `demo` is the one exception to the fail-closed bind rule of Section 9.5: because it serves invented data in that mode, it may bind a non-loopback address with no token (`tierd demo --addr 0.0.0.0:8124`). The exemption is an in-code signal set by the `demo` command alone; no `serve` flag or environment variable can reach it.

### 9.3 Check capture, then see where the money went (no server, no token)

```sh
tierd doctor --repo ~/src/your-repo                  # local checks: git repository, git remote, claude projects dir, recent sessions, issue attribution, spend not on a feature branch
tierd score --repo ~/src/your-repo                   # last 90 days of Claude Code spend for that repo
tierd score --repo ~/src/your-repo --since 2026-05-01
```

With no `--server`, `doctor` runs the local checks named above (plus collector-credential probes when `--config` names an Admin/Usage poller). Add `--server <url>` (and `--api-token` if the server has one) to also round-trip against a running `tierd`: server reachable, server auth, clock offset, price table (parity between the CLI's table and the server's), identity join, outcome coverage, the **cost horizon** check, which reads that server's `/scores`, and `sealing`, which reports a stalled sealed month (Section 6.2):

```sh
tierd doctor --repo ~/src/your-repo --server http://127.0.0.1:8080
```

`score` reads `~/.claude/projects/`, prints the price-table version and hashes, a per-developer token and cost table, and a cost-by-issue table with an `unattributed` row. It is cost attribution, not a TIER score, because it has no outcomes; it reads Claude Code sessions only. Point `--repo` at a repository you have actually worked in with Claude Code; pointing it at a fresh checkout prints "No Claude Code sessions found" and exits 0. Useful flags: `--since`, `--developer <id>` (default: OS username), `--prices <file.yaml>`, `--repo-slug owner/upstream` on a fork.

### 9.4 The full score

First create a GitHub token for `tierd backfill`, which calls two GitHub REST endpoints and nothing else: `GET /repos/{owner}/{repo}/pulls` and `GET /repos/{owner}/{repo}/pulls/{number}`. A fine-grained personal access token (PAT) with **Pull requests: Read-only** on that one repository is enough:

1. Create it at GitHub → Settings → Developer settings → Personal access tokens → Fine-grained tokens → Generate new token (`https://github.com/settings/personal-access-tokens/new`).
2. Repository access: only that repository. Permissions: Pull requests = Read-only (Metadata is added automatically).
3. Copy the `github_pat_…` value; GitHub shows it once.
4. Save it to a private file: run the line below, paste the token (nothing is shown) and press Enter: `mkdir -m 700 -p ~/.tier && chmod 700 ~/.tier && read -rs T && (umask 077; printf '%s' "$T" > ~/.tier/github-token) && chmod 600 ~/.tier/github-token; unset T`.

Then:

```sh
# 1. Reconstruct the last 90 days of merged-PR outcomes. Run this BEFORE serve:
#    the store is single-writer SQLite.
export TIER_GITHUB_TOKEN=@$HOME/.tier/github-token      # or pass --token @$HOME/.tier/github-token
tierd backfill --repo your-org/your-repo             # GitHub "owner/name" slug, not a path

# 2. Start the server. --aggregation is required; use developer for a solo trial.
tierd serve --aggregation developer --watch-repo ~/src/your-repo
# tierd listening addr=127.0.0.1:8080

# 3. In a second terminal, ship the last 90 days of local cost to it.
tierd ship --server http://127.0.0.1:8080 --repo ~/src/your-repo
```

A literal token on the command line leaks through `ps` and shell history; the `@file` form avoids that. A repository owned by an organisation may require an organisation owner to approve the token before it works. `gh auth token` from the GitHub CLI also works, but it hands `backfill` a token with far wider access than it needs. For GitHub Enterprise, add `--github-api-url <base URL>`.

Then open `http://127.0.0.1:8080`. The dashboard (a static HTML page with client-side JavaScript, served at `/`) now computes the score from your captured cost and merged outcomes. On a server with an admin token, the page asks for a token when a read returns `401`; it keeps the token for the browser session only and sends it as `Authorization: Bearer`. The `tierd` in these blocks is the binary on your `PATH`; from a checkout, substitute its full path as in Section 9.1.

Why both `serve --watch-repo` and `ship` on the same machine: `--watch-repo` captures Claude Code session files as they change after `serve` starts (a changed file the watcher has not tracked before is re-read whole, so its earlier messages come in too; files it has tracked resume from their checkpointed offset, even across restarts), while `ship` loads the **back catalog** (the last 90 days) that already exists on disk. They overlap on any session written during both, and that is harmless: both derive the same idempotency key from the message id, so the store keeps one row. In this laptop-only layout, add `--codex-rollout`, `--opencode` and/or `--muse` to `serve` (and send those tools' traffic through their own logs, not also through the proxy, or it is counted twice; Section 3.1) (whose first pass loads every such log on disk and then rescans) and to `ship` (which loads their history); the central-server layout in Section 9.5 puts those flags on `ship`, because that server holds no session logs.

New merges after this point reach the server in one of two ways. A GitHub webhook (Section 9.5) needs the server reachable from GitHub, which a loopback bind is not; the alternative for a local trial is to stop `serve`, re-run `tierd backfill`, and start `serve` again, since the store is single-writer. With no webhook, no CI or revert signal arrives, so every outcome in this layout keeps quality 1.0. A brand-new trial with fewer than 3 merged PRs or under $5 of captured spend (unattributed spend included) shows a row below the evidence floor until more data accrues. So does one whose backfilled PRs are older than the session logs on disk (Section 5.5): read from `cost_coverage_safe_since`. If your OS username differs from your GitHub login, map them:

```sh
curl -X POST http://127.0.0.1:8080/api/v1/developer_alias \
  -H "Content-Type: application/json" \
  -d '{"alias":"alice-laptop","canonical":"alice"}'
```

On a server with an admin token, send the header through curl's input so the token never appears in `ps`: start the command with `printf 'Authorization: Bearer %s\n' "$(cat /path/to/token)" |` and add `-H @-` to `curl` (`printf` is built into the shell, so it starts no process that would show the token), but only when nothing else in the command reads standard input (`-d @-`, `--data-binary @-`, `-T -`, `-K -`).

### 9.5 Run it for a team

```sh
export TIER_API_TOKEN=@/etc/tier/api-token
export TIER_WEBHOOK_SECRET=@/etc/tier/webhook-secret
tierd serve --aggregation team --addr 0.0.0.0:8080 --db /var/lib/tier/tier.db
```

`tierd` itself serves plain HTTP. The `https://` addresses in the webhook and `ship` examples assume TLS (Transport Layer Security) is terminated in front of it by a reverse proxy, load balancer or tunnel that forwards to `tierd`'s listener; TIER ships no certificate handling of its own. How to keep the per-IP lockout working behind such a proxy is in `docs/security.md`.

`--db` defaults to `~/.tier/tier.db` on every command that opens the store (`tier.db` in the current directory if the home directory cannot be resolved). A non-loopback `--addr` without an API token is refused at startup with the message "refusing to bind ... without an API token: ... set --api-token (or TIER_API_TOKEN) or bind to 127.0.0.1"; the remedy is exactly that: set the write token (preferably `TIER_API_TOKEN` or an `@file`), or bind to `127.0.0.1`. A `--read-token` on its own does **not** unlock a non-loopback bind, and it must differ from the write token. The one exemption is `tierd demo` (Section 9.2), which binds without a token because it serves synthetic data read-only; `serve` over captured data has no such path. Any secret flag accepts `@/path/to/file` so the value never appears in `ps` or shell history. A token is any long random string (for example the output of `openssl rand -hex 32`), stored in a file only the service account can read. Secrets are read when `serve` starts, so to rotate one, replace the file and restart. Configure the GitHub webhook at Settings → Webhooks: payload URL `https://<tierd-host>/webhook/github`, content type `application/json`, the secret from `TIER_WEBHOOK_SECRET`, and the individual events **Pull requests**, **Pushes** and **Workflow runs**. Without a secret the webhook route is not mounted at all. Populate `org_hierarchy` before relying on team mode, for example with `tierd hierarchy import` (Section 6.1). A team-mode server publishes no report until sealing is armed: once the history is loaded and `serve` has started once on the database, run `tierd seal --arm earliest` (or a `YYYY-MM` month) with the same config, flags and environment as `serve`. That command is refused until the month it would seal has closed and its grace lag (14 days by default) has ended, and the refusal names the date. To arm in advance, set `seal_from` to that month instead (`--seal-from YYYY-MM`, env `TIER_SEAL_FROM`, config `seal_from`). Arming is irreversible, and a new installation's first report appears no earlier than 14 days after the end of its first full calendar month of cost data (Section 6.2). Laptops feed the server on a schedule:

```sh
*/15 * * * * /absolute/path/to/tierd ship --server https://<tier-host> --repo ~/src/app --api-token @/path/to/token --codex-rollout --opencode --since 2026-09-01
```

Replace `/absolute/path/to/tierd` with the installed binary's absolute path; `git` must be on cron's `PATH` (default `/usr/bin:/bin`), or set `PATH=` in the crontab. In this layout `POST /api/v1/events` is admin-scoped, so every laptop's `ship` job holds the org-wide write token, and a leaked laptop token can post cost or outcomes under any developer name until it is rotated. There is no per-developer credential today. Keep only the collector flags for tools the developer uses. A fixed `--since` must be within the last 90 days and moved forward from time to time to bound how much each run resends (Section 3.1); a rolling date needs a wrapper script, because `%` must be escaped in a crontab line and `date` differs between Linux and macOS. The server does not know which laptops should report. In `developer` mode, `GET /api/v1/fidelity` shows each developer's captured spend over the last 7 and 30 days, the nearest check that every laptop is reporting. In `team` and `division` mode that endpoint answers `403`, and no endpoint shows per-developer activity; the team rows' cost, and `GET /api/v1/developer/{id}/export` for one named developer at a time, are what remain. `tierd ship -h` describes its output on each kind of failure.

In this layout the central server holds no developer's session logs, so it runs without `--watch-repo`, `--codex-rollout`, `--opencode` or `--muse`, and each laptop's `ship` carries those collector flags instead. The flags are not forbidden on a server: any `serve` that does have session logs and repositories on its own disk (for example a shared build host) may take `--watch-repo`, repeated per repository, with the collectors; `serve` refuses the collector flags only when no `--watch-repo` is given. Likewise point `backfill` at the server's database, or its outcomes land in `~/.tier/tier.db` where the central `serve` never reads them:

```sh
tierd backfill --repo your-org/your-repo --db /var/lib/tier/tier.db   # before serve starts
```

A config file (`tierd serve --config config.yaml`) mirrors the flags; precedence is CLI flag > environment variable > config file > built-in default. This reference does not reproduce the full schema: the repository's `config.example.yaml` lists every key, including the hierarchy, pollers, repository slugs and size-label overrides. For Docker, the image (linux/amd64 only, Section 9.1) runs `tierd` as non-root uid 65532 on a static base with no shell; it must bind `0.0.0.0` with a token. The image sets `HOME=/data` and declares `/data` a volume, so the default database is `/data/.tier/tier.db`:

```sh
docker run -d -p 8080:8080 -v tier-data:/data -v /etc/tier:/etc/tier:ro \
  -e TIER_API_TOKEN=@/etc/tier/api-token \
  ghcr.io/tiermetric/tierd:0.5.2 serve --aggregation team --addr 0.0.0.0:8080
```

The token file must be readable by uid 65532. It ships only `tierd` (no `git`, no host logs), so feed a stock container with `tierd ship` or the proxy; watching inside a container needs the session-log directory and the repository mounted, and commit-based attribution is unavailable without `git`.

### 9.6 The other commands

`tierd backup --db <path> --out <new-file>` takes a consistent snapshot with SQLite's `VACUUM INTO`, safe while `serve` runs. The snapshot is a complete database file that `--db` can point at; the repository documents no separate restore procedure. `tierd reprice --from-version N [--commit]` recomputes historical costs under the current table (dry run by default). `tierd score-log --format claude|codex --log <file>` prices one session log as JSON with no store or network. `tierd verify-report` checks a report against its **manifest**: the record, served by `GET /api/v1/report_manifest`, of the window, repository scope, mode, k, price table and its hashes, rubric version, tool version, and counts and digests of the rows the report was computed from. It exits `0` when every pinned input matches and the recomputed report equals the manifest, `1` when an input or a number has moved (and names which), and `2` when it could not check at all; `2` is never a pass. A sealed team or division month's manifest is replayed from the bytes it was sealed with, never recomputed, and adds two exit codes: `3` when this database holds no sealed row for the month, and `4` when every compared field is unchanged but the refold was not run, for example because the month was sealed under a fold rule other than this binary's (`docs/reproducibility.md`). `tierd seal` arms sealing, records a skipped month and reports sealing status (Section 6.2). `tierd healthcheck` probes `/api/v1/livez`. `tierd repair-repo` moves one developer's cost rows off the `unqualified` repository sentinel (Section 5.1). `tierd prices forget-version --db <path> --version <N> --commit` retires a registered price-table identity in that database (dry run without `--commit`; `tierd prices list --db <path>` and `tierd prices audit --db <path>` show the registry and its ledger); it cannot free the embedded version, which the load-time guard protects regardless. Run any command with `-h` for its full flag list.

**Upgrades.** A newer binary migrates the database when it opens it, and an older binary refuses to open a database a newer one has migrated, so take a `tierd backup` before upgrading; that backup is the only way back. Running `serve` as a system service is left to the administrator's own service manager; the repository ships a container image and an example systemd unit, `deploy/tierd.service`.

**Scale.** The project publishes no tested deployment size (developers, events or database size) for the single-writer SQLite store, and no figure for what running TIER itself costs. A warning fires when SQLite's write-ahead log (WAL) grows past 64 MiB (Section 10); it only warns, and the project documents no remedy.

---

## 10. API and interfaces

All routes are served by `tierd serve` under `/api/v1` (the Prometheus scrape is at `/metrics`, the webhook at `/webhook/github`, the dashboard at `/`, and the documentation, this reference and the white paper included, at `/docs/`). Routes fall into three token scopes, plus the webhook, which is checked by signature instead. **admin** requires the write token (`--api-token` / `TIER_API_TOKEN`, sent as `Authorization: Bearer`); **read** accepts either the read-only viewer token (`--read-token` / `TIER_READ_TOKEN`, which must differ from the admin token and is rejected on the write routes and on the proxies) or the admin token; **open** needs no token. The `serve --help` text lists fewer read-scoped routes than the router actually protects; the router's list follows: `/scores`, `/scores/{developer}`, `/scores/compare`, `/report_manifest`, `/events`, `/outcomes`, `/quality_events`, `/quality_history`, `/fidelity` and `/metrics` are all read-scoped in the router. In team and division mode `/metrics` refuses the read token and takes the admin token or the scrape-only metrics token (`--metrics-token`) instead (Section 6.2). `POST`, `PUT` and `PATCH` requests under `/api/v1` need `Content-Type: application/json` or get `415`. A cross-origin browser request gets `403` on `/api/` for every method, and on every other path, the proxies included, for any method but `GET`, `HEAD` and `OPTIONS`. A server with no token on a loopback address answers `403` to a `Host` that is not a loopback name or IP, so browse it at `127.0.0.1` or `localhost` (#893). Auth is enforced only when the admin token is configured. A `--read-token` without `--api-token` has no effect (`serve` logs a warning), and on a loopback bind with no admin token every route is reachable. A per-IP lockout returns `429` after 10 failed authentications within 60 seconds and holds for 15 minutes (`--auth-max-failures`, `--auth-failure-window`, `--auth-lockout`).

| Method and path | Purpose | Scope |
|---|---|---|
| `POST /api/v1/costs` | ingest one token-cost event (`source` must be `api` or omitted; Section 3.1) | admin |
| `POST /api/v1/events` | bulk ingest from `tierd ship` (sources `jsonl`, `codex-rollout`, `opencode`, `muse`); server re-prices | admin |
| `POST /api/v1/outcomes` | record a merged outcome (Section 4.6) | admin |
| `POST /api/v1/actual_spend` | per-developer invoice total for a `YYYY-MM` period | admin |
| `POST /api/v1/org_actual_spend`, `GET` | org-level invoice total; read back the ledger | admin |
| `GET /api/v1/scores` | all scores for a window; `?since=`, `?until=` (or its legacy alias `?before=`), `?team=` (adds a `team` rollup), `?work_type=`, `?repo=`; in team and division mode only `?period=YYYY-MM`, a sealed month (Section 6.2) | read |
| `GET /api/v1/scores/{developer}` | one developer with per-issue detail; blanket 404 in anonymised modes | read |
| `GET /api/v1/scores/compare` | two-window before/after deltas with `significant`; accepts `since_a`, `until_a`, `since_b`, `until_b` and nothing else; in team and division mode `period_a` and `period_b` instead | read |
| `GET /api/v1/report_manifest` | the reproducibility manifest a published report is verified against; `?period=YYYY-MM` in team and division mode | read |
| `GET /api/v1/events`, `GET /api/v1/outcomes` | keyset-paginated raw exports, JSON or CSV (`Accept: text/csv`); 403 in anonymised modes | read |
| `GET /api/v1/quality_events`, `GET /api/v1/quality_history` | the append-only quality signal and transition logs; 403 in anonymised modes | read |
| `GET /api/v1/fidelity` | per-developer capture fidelity over 7 and 30 days; 403 in anonymised modes | read |
| `POST`, `GET`, `DELETE /api/v1/developer_alias[/{alias}]` | identity map | admin |
| `PUT /api/v1/org_hierarchy/{developer}`, `POST`, `GET /api/v1/org_hierarchy` | developer → team/division/org map; `PUT` body `{"team","division","org"}` with `team` required, `POST` body an array of `{"developer","team","division","org"}` in one all-or-nothing transaction | admin |
| `POST /api/v1/period_membership/{developer}/end` | close a departed developer's seat | admin |
| `GET /api/v1/developer/{id}/export`, `DELETE /api/v1/developer/{id}` | GDPR access and erasure | admin |
| `GET /api/v1/health`, `/healthz`, `/livez` | smoke, readiness (503 while the watcher restarts), liveness (version, uptime) | open |
| `GET /api/v1/version` | build identity (version, commit, platform, price-table version, no digests); first released in v0.4.1, so v0.4.0 and earlier answer 404 | open |
| `GET /`, `GET /docs/` | the dashboard page, and the documentation as static HTML; both mounted in every mode, `--read-only` included | open |
| `GET /metrics` | Prometheus exposition | read (team and division mode: admin or metrics token) |
| `POST /webhook/github` | GitHub deliveries, HMAC-SHA256 (a keyed hash of the body) in `X-Hub-Signature-256`; mounted only with a secret | signature |

Windows on the read endpoints accept `YYYY-MM-DD`, `YYYY-MM` or `YYYY`, each meaning the first instant of that day, month or year in UTC; windows are half-open, so `until=2026` ends immediately before 2026-01-01T00:00Z, and `until` must be after `since`. `since` defaults to the start of the UTC day 90 days ago (the dashboard defaults its "From" date to 30 days ago). Team and division mode take no window parameters on `/scores`, `/report_manifest` and `/scores/compare`, only the sealed-month parameters above. The scores endpoints reject any query parameter outside their own list with `400`: in developer mode, for `/scores` that list is `since`, `until`, `before` (a legacy alias of `until`), `team`, `work_type` and `repo`; for `/scores/compare` it is the four window parameters alone. The project's compatibility rule for responses is additive-only: a field is not renamed, removed or given a new computation in place, and CSV columns are append-only. That is a rule the project holds itself to, not something the code can guarantee for future releases. (`exploratory_cost_share` is computed as it was when introduced; only the words describing it changed; Section 5.3. One exception in this release: the premium cutoff behind `premium_model_share` moved from $5 to $4, which changes that share for models priced between the two.) Feature detection reads `livez.version`. This reference does not reproduce request and response schemas, pagination parameters or error bodies; they are in the repository's `docs/api-compatibility.md` and `docs/outcomes-api.md`. Exports paginate by an opaque `next_cursor` (also in the `X-Next-Cursor` header), default page 1,000 rows, hard cap 10,000, and `cost_micro` is integer micro-dollars.

A developer row in `/scores` (`developers[]`, developer mode) carries: `developer`, `tier`, `weighted_points`, `total_cost_usd`, `actual_paid_usd`, `spend_leverage`, `coverage_pct`, `exploratory_cost_share`, `cost_per_point` (`null` on zero points), `sample_n` (the developer's outcomes in the window, flagged ones included), `ci_low`, `ci_high`, `cost_per_point_ci_low`, `cost_per_point_ci_high`, `ranked`, `flagged_outcomes` (outcomes that failed the zero-token check). A team or division row (`teams[]`) carries `team`, `tier`, `weighted_points`, `total_cost_usd`, `actual_paid_usd`, `spend_leverage`, `coverage_pct`, `cost_per_point`, `ranked`, and no interval. Rows come in identifier or name order (in team and division mode, the `other` row last), and the dashboard keeps that order (Section 7.1). In developer mode the response also carries `team_rollups` (Section 6.1).

The `/scores` response carries, beside the rows: `price_table {version, effective_date, table_hash, file_hash}`; `rubric {version}`; `total` (a pooled rollup); `work_types[]`; `cost_composition` (name-free: `cache_read_share`, `premium_model_share` for models whose base input rate is at or above $4.00 per million tokens (a constant in the code, lowered from $5 to $4 in this release so that `claude-opus-5-5` at $4.00 counts as premium; while Sonnet, Haiku and the flash/mini tiers do not; the over-threshold long-context rates play no part), `by_model`, and `by_class`, which holds token counts, not dollars, for input, output, cache read and cache write); `segment_reconciliation` (`outcome_linked + no_outcome + unattributed == window`, exact on the integer fields); and `data_quality` (attribution shares, unattributed buckets, `exploratory_cost_share`, `unjoined_developers`, zero-token flags, `mixed_price_versions`, the cost-horizon fields, `kanon_suppressed`, and `repo_scope`, which echoes the repository when `?repo=` is set). A team or division report leaves out `work_types[]`, `cost_composition`, `segment_reconciliation` and the `data_quality` fields Section 6.2 lists, and carries `attribution_coverage` and `excludes_unattributed_spend`, plus `uncounted_active_ids` when an identifier did not count toward k.

TIER emits two operational warnings. A **zero-outcome tripwire** checks hourly and warns (log plus `tier_zero_outcome_tripwire` gauge) when cost accrued but no outcome landed in the last `--zero-outcome-window-days` (default 7): a broken webhook or a trunk-based team without push capture. A **WAL-size tripwire** checks every 5 minutes and warns once when the SQLite write-ahead log (WAL) grows past 64 MiB (gauge `tier_sqlite_wal_bytes`).

---

## 11. Limits and known ways it can mislead or be gamed

The project publishes its own adversarial analysis (`docs/adversarial-analysis.md`), dated 26 March 2026: 28 scenarios, each with a code used below: G for individual gaming, U for unfair comparison, O for organisational gaming, M for mathematical edge cases and P for perverse incentives. It rated 7 critical, 14 high and 7 medium. It analysed an earlier formula that divided by tokens rather than dollars, on an older weight scale, and the ratings have not been redone for the current formula; read them as the project's judgement at that date. Project policy (Section 6.3) prohibits using the numbers in appraisal; the software cannot enforce that. Known ways the number misleads or can be gamed:

**Structural, present in normal use**

- **Task mix moves the number.** Greenfield features and production debugging will not score alike; a senior on a legacy module can show a lower number than a junior on greenfield. The project has not measured the relative effects of task mix and developer skill on TIER. Compare a team with its own history (Section 2.1).
- **Windowing.** Recent, open-ended windows read low (the trailing edge, Section 5.4). Windows that reach back past the cost horizon read high (Section 5.5); widening a window shrinks the edge effects but makes this one worse once it crosses the horizon.
- **Attribution coverage.** A merged PR with no derivable issue records no outcome at all (logged at debug level, so easy to miss), and TIER reads low. Spend recorded without an issue weakens the segments and trips the zero-token check (Section 5.3). Branch names that carry the issue number reduce both. They cannot help Opencode spend, because Opencode records no branch, and they do not stop a PR merged into a non-default branch from escaping the CI floor, which is read only on the default branch.
- **Identity mismatch** between OS username and GitHub login splits into zero-scoring rows until aliased (flagged in `unjoined_developers`).
- **Outcome timestamps differ by producer.** The webhook stamps an outcome with the time it processed the merge event, while backfill and the outcomes API use the PR's `merged_at`, and push capture uses the commit time. A late or redelivered webhook can therefore place an outcome in a later window than a backfill of the same PR would.
- **Cache-hit rates.** Identical workflows with different cache reuse get different scores by design; read `cache_read_share` beside the number.
- **The diff heuristic has no semantics.** A five-line concurrency fix and a five-line typo fix weigh the same unless labelled, and generated churn inflates unlabelled PRs.
- **Quality is partial.** Follow-up fixes, partial reverts, incidents, hotfixes and downstream failures are specified and not enforced; backfilled outcomes carry no historical degradation; CI floors do not reach push-captured outcomes; and if the "Workflow runs" webhook event is not subscribed, CI failures are invisible.
- **Coverage of tools.** TIER has no collector for Cursor, GitHub Copilot or ChatGPT Team, so their usage is not captured; the project's reading is that they report seat invoices rather than per-developer token counts. Gemini through the proxy is untested on live traffic. Opencode capture takes only the Z.ai coding plan (Section 3.1). Uncaptured usage reads as free and makes TIER read high whenever some cost is recorded (Section 2.2).

**Gaming vectors**

- **Off-books usage (G-02, critical).** Work done on a personal AI subscription that TIER never sees shrinks the denominator. The zero-token check takes a developer below the evidence floor when any outcome has under 1,000 tokens against its issue in the 14-day attributable window (Section 7.1); beyond that, the defence is policy.
- **Label inflation (the labelling side of O-01 manager weight manipulation, and the relabelling in G-03).** Calling a medium change `size/l` raises the numerator, and **no code defends against it.** The fixed scale constrains only labels and the heuristic; `POST /api/v1/outcomes` takes any weight in (0, 8] (Section 4.6). Nothing detects a mislabelled PR, and the rubric version stamp cannot see a generous labelling culture. The mitigation is human: review labels against the meanings in Section 4.2.
- **PR splitting (G-03).** The scale is concave at the top, so splitting large work pays. One `size/xl` PR is 8 points; the same work as two `size/l` PRs is 10, or as three `size/m` PRs is 9. The heuristic has the same shape: a 1,200-line, 10-file PR has effort 1,300 and weight 8, while the same diff as two 600-line, 5-file PRs has effort 650 each and weight 5 + 5 = 10. (The adversarial analysis states G-03 as issue splitting on an older weight scale: a 13-point epic relabelled into sub-issues totalling 21; the incentive is the same.) The one-outcome-per-merge rule does not stop this; push capture's one-outcome-per-(repository, issue, UTC day) rule closes only the commit-splitting form.
- **Cheap-work farming (P-02, P-03).** Avoiding hard problems, or avoiding AI where it would help, can both raise the ratio. Work-type segmentation and the "no absolute good band" rule make it easier to notice; nothing prevents it.
- **Forged rows (O-03).** The write scope is a single global bearer token with no subject; nothing binds it to the `developer` field, so a token holder can post cost under another name, through `/costs`, `/events` or the proxy's `X-Tier-Developer`, or shift spend out of their own row. The only value the write endpoints refuse is the reserved `unattributed` sentinel (the proxy, which never fails a call, stores a forged header as `unattributed` instead; Section 3.1): in the `developer` field of every write endpoint, and in `issue_id` on `/costs` and `/outcomes`; `POST /api/v1/events` accepts, from the `unattributed` family, only the five exact bucket names of Section 5.3 that its own collectors emit (ordinary issue ids pass as usual). Binding identity to credential needs an identity layer that is not built (#65).
- **Quality-window exploitation (G-04).** Reverts after 60 days do not degrade the original outcome.
- **Double outcomes.** One change can be stored twice: Sections 4.1 (integration branches) and 4.5 (push capture).
- **Goodhart (P-06, critical).** The risk: if TIER becomes a target, people will work to raise the number rather than improve the work, by the routes above.

**Operational**

- Double counting and the first-writer cost rule: Section 3.1. A `--prices` override replaces the whole table: Section 3.4.
- The store is single-writer SQLite; run `backfill` before `serve`, not beside it (Section 4.7).
- Everything is single-tenant; run one instance per organisation.

---

## 12. Glossary

Short definitions; the section named holds the rule.

- **TIER**: weighted outcome points per $1,000 of list-price AI cost (§2.1).
- **Accepted outcome**: a merged pull request, an opt-in direct commit, or an API-posted merge (§2.1, §4).
- **Weighted point**: one outcome's `weight × quality` (§2.1).
- **Weight**: an outcome's size; 0.5 / 1 / 3 / 5 / 8 from a label or the diff heuristic, or any value in (0, 8] sent through the API (§4.2, §4.6).
- **Quality multiplier**: 1.0 for a clean merge, lowered by a CI failure or a revert (§4.4).
- **List-price cost**: token counts priced by the reference price table, stored as integer micro-dollars (§3.2, §3.3).
- **Reference price table**: the embedded, versioned `prices.yaml`; `--prices` replaces it (§3.4).
- **price_version / table_hash / file_hash**: the table version stamped on each cost row; digests of the resolved rates and of the file (§3.4).
- **Rubric version**: the stamp on the weight scale, work-type taxonomy and quality floors (§4.2).
- **cost_per_point**: dollars per weighted point (§2.1).
- **Spend Leverage**: list cost ÷ paid (§8.1).
- **Operator / administrator**: the project maintainer who rules on design questions / whoever runs an installation (front matter).
- **Evidence floor (`ranked`)**: 3 outcomes, $5.00 of cost, no zero-token outcome (§7.1).
- **Zero-token outcome**: an outcome whose developer recorded under 1,000 tokens on its issue in the 14-day attributable window (§7.1).
- **Provisional**: the dashboard's label on the headline when `attributed_cost_share` is below 0.5 (§7.1).
- **Unattributed / no-outcome spend**: cost with no issue / cost on an issue with no outcome for that developer (§5.3).
- **attributed_cost_share / coverage_pct**: share of window cost joined to an issue / share of captured cost that arrived per call (§5.3, §7.4).
- **Issue id**: the join key (`issue-42`, `TIER-99`) read from a branch name, commit or PR body (§5.1).
- **Window**: the half-open interval `[since, until)` (§2.2, §10).
- **Cost horizon**: the timestamp of the earliest cost event in the store (§5.5).
- **Bootstrap interval**: the 95% interval from 1,000 resamples of a developer's outcomes (§7.2).
- **significant**: on `/scores/compare`, both windows clear the floor and their intervals do not overlap (§7.3).
- **Reporting mode**: `developer`, `team` or `division`, chosen at startup; changes what is served, not what is stored (§6.1).
- **k-anonymity / other / kanon_suppressed**: a group is named only when at least k people count toward it; smaller groups fold into `other`; a small `other` withholds the whole response (§6.2).
- **Sealed month**: in team and division mode, a report for one calendar month, sealed once after its grace lag and never recomputed (§6.2).
- **Worktree attribution**: the opt-in, off-by-default rule that attributes Claude Code spend by the git worktree a session's tool calls worked in (§5.1).
- **Work type**: one of seven categories; scores compare within a type (§4.3).
- **Push capture**: opt-in scoring of direct commits to the default branch (§4.5).
- **Laptop shipper**: `tierd ship` (§3.1).
- **Idempotency key**: the per-event key the store deduplicates on (§3.1).
- **developer_alias / org_hierarchy / period_membership**: identity map; developer → team, division, organisation map; paid-spend seats by month (§5.2, §6.1, §8.1).
- **Acronyms**: API (application programming interface), CI (continuous integration), CSV (comma-separated values), DPIA (data protection impact assessment), GDPR (General Data Protection Regulation), GHCR (GitHub Container Registry), HMAC (hash-based message authentication code), JSONL (JSON Lines), PAT (personal access token), PIP (performance improvement plan), PR (pull request), SLSA (Supply-chain Levels for Software Artifacts), TLS (Transport Layer Security), WAL (write-ahead log); BetrVG, CSE and WOR are expanded in §6.3.

---

## Appendix A: machine-readable summary

Constants and rules from the body, for tools. Where a value here and the body differ, the body is right and this appendix is a defect.

```yaml
document:
  title: "TIER — technical reference"
  version: "1.2"
  companion: "whitepaper.md (version 2.2)"
  date: "2026-10-01"
  describes: "tiermetric/tier v0.5.2"
  licence: "Apache-2.0"
formula:                                  # §2.1
  tier: "sum(weight * quality) / (total_cost_usd / 1000)"
  unit: "weighted outcome points per 1000 USD of list-price AI cost"
  cost_per_point: "total_cost_usd / weighted_points; null when points are 0; 0 when cost is 0 and points > 0"
  accepted_outcome: [merged_pull_request, push_capture_commit_opt_in, api_posted_outcome]
  denominator: "all recorded cost in the window, linked to an issue or not"
  team_rollup: "sum of points / (sum of cost_usd / 1000)"
  comparable_when: "same price_table.table_hash AND same rubric.version AND no data_quality.mixed_price_versions, and rows priced by the same price_version; own history first; two teams only within one work type after a label review; never two organisations"
  decision_figure: "per-work-type segment across two windows, read beside segment_reconciliation"
  segments: "divide by that type's outcome-linked cost only; an issue with two types counts in both"
  never_compare: [across_organisations, across_work_types]      # §6.3
weights:                                  # §4.2, §4.6
  labels: {xs: 0.5, s: 1, m: 3, l: 5, xl: 8}
  heuristic: {effort: "additions + deletions + changed_files * 10", buckets: {"<=15": 0.5, "<=60": 1, "<=200": 3, "<=1000": 5, "else": 8}}
  several_size_labels: "first in the webhook payload or REST response order wins"
  api_weight: "taken as sent, 0 < w <= 8"
  push_capture: {weight: 0.5, grain: "one outcome per (repository, issue, UTC day)", squash_merge: "counts once in any event order since v0.5.2 (#849); double counts stored before the upgrade stay"}
  rubric_version: {value: 1, set_by_hand: true, tied_by_tests: [scale, heuristic_outputs, work_types, quality_floors], not_tied: [heuristic_thresholds, work_type_precedence, default_label_names, revert_keywords]}
quality:                                  # §4.4
  floors: {clean: 1.0, ci_fail: 0.7, revert_strategic: 0.8, revert_quality: 0.1}
  windows: {ci_fail: "48h upper cutoff, run updated_at, default branch only", revert: "60d"}
  combination: "minimum of floors, clamped to [0.1, 1.0]"
  flaky: "same commit, same workflow_id, later run_attempt, success within 30 min; then every failure of that (commit, workflow) is cleared"
  keyword_match: "case-insensitive unanchored regular expressions over the revert commit message"
  specified_not_enforced: [followup_fix, partial_revert, incident_correlation, hotfix_branch, downstream_ci, no_ci_signal, flaky_registry, lifecycle_state_machine]
pricing:                                  # §3.2–§3.4
  price_table: {version: 12, effective_date: "2026-10-02", models: 91}
  cost_storage: "integer micro-dollars, one round-half-to-even per call"
  long_context_trigger: "input + cache_read + cache_write_5m + cache_write_1h > context_threshold"
  long_context_rows: [claude-sonnet-4-5, gemini-2.5-pro, gemini-3.1-pro]
  unknown_model: {large: 2.00, medium: 0.50, small: 0.10, unit: "USD per million, all tokens", note: "substring match; 13b and 31b class as small"}
  store_conflict: "same key: larger token counts kept, first row's cost kept"
  billing_mode_changes_cost: false
capture:                                  # §3.1
  producers: [claude_code_jsonl, codex_rollout, opencode, muse_code, proxy, post_costs, anthropic_admin_poller, openai_usage_poller]
  defaults: {claude_code: on, codex_rollout: off, opencode: off, muse_code: off, proxy: mounted, pollers: off}
  worktree_attribution: {default: off, flag: "--worktree-attribution", env: TIER_WORKTREE_ATTRIBUTION, config: "watch.worktree_attribution (serve only)", status: "opt-in preview; measured accuracy in docs/how-it-works.md", stored_attribution: permanent}   # §5.1
  opencode_routes: [zai-coding-plan]
  counted_twice: ["codex via /openai/ + --codex-rollout", "opencode via proxy + --opencode", "POST /costs re-posted without idempotency_key", "POST /costs rows declared billed_to=other for usage a running poller also reports, and manual rows for a poller-covered provider stored before v0.5.2"]
  open_doubt: "Claude Code JSONL may undercount tokens (issue #837, unsettled); if so TIER reads high"
evidence_floor:                           # §7.1
  wire_field: ranked
  min_outcomes: 3
  min_cost_usd: 5.00
  zero_token: {min_tokens: 1000, window_days: 14}
  configurable: false
  row_order: "identifier order; nothing sorts by TIER"
intervals:                                # §7.2, §7.3
  bootstrap: {replicates: 1000, configurable: false, seed: fixed, bounds: "2.5th and 97.5th percentile, nearest rank"}
  cost_per_point_ci: "low = 1000 / ci_high; high = 1000 / ci_low"
  significant: "both windows clear the floor AND intervals do not overlap; always false in team/division mode"
aggregation:                              # §6
  modes: [developer, team, division]
  required: true
  k_anonymity: {default: 5, minimum: 3, counts: "people: on the hierarchy in the window, not bots, with captured activity", per_figure: "cost, points and paid spend each need k carriers"}
  anonymised_reports: {period: "sealed calendar month", select: "?period=YYYY-MM", report_grace: {default: "336h", minimum: "24h"}, armed_by: "tierd seal --arm YYYY-MM|earliest, or seal_from", recomputed: never}
  anonymised_breakdowns: "group rows and total only"
  developer_mode_team_views_k_anonymised: false
  mode_changes_storage: false
  known_limits: ["the write token can edit the hierarchy and aliases that decide who counts (#908)"]
spend_leverage:                           # §8.1
  formula: "total_cost_usd / actual_paid_usd"
  months: "month(since) <= period < month(until); open until includes every later month"
  org_invoice_without_hierarchy: "allocated to no one"
  finance_posts_idempotent: false
use:                                      # §6.3
  permitted: "coaching conversations, including a manager viewing an individual's numbers in developer mode"
  never: [pay, compensation, promotion, performance reviews, performance improvement plans, forced ranking, discipline, dismissal]
  source: "operator ruling, issue #826, 2026-09-26"
  planned_not_built: [per_viewer_tokens, view_log, developer_mode_retention_90d_default_365d_ceiling, name_free_rollup, coaching_page]
retention_today: "nothing deleted by age except raw webhook bodies (90 days, 50,000 rows, at database open and every 24 hours while serve runs)"
windows:                                  # §5, §10
  interval: "[since, until), UTC"
  defaults_days: {api: 90, dashboard: 30, backfill: 90, ship: 90, score: 90}   # api and dashboard: developer mode
  read_first: [window_predates_cost_capture, cost_coverage_safe_since]
install:                                  # §9.1
  source: "git clone --branch v0.5.2 https://github.com/tiermetric/tier.git && cd tier && make build"
  go: "go install github.com/tiermetric/tier/cmd/tierd@v0.5.2"
  image: "ghcr.io/tiermetric/tierd:0.5.2 (linux/amd64)"
  go_version: "1.26.9 or newer"
```
