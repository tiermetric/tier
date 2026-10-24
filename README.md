# TIER — Token Impact & Efficiency Ratio

![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue)
![Go 1.27.3+](https://img.shields.io/badge/go-1.27.3%2B-00ADD8)
![Status: pre-v1](https://img.shields.io/badge/status-pre--v1-orange)

**Merged work per $1,000 of captured AI usage, priced at list rates: an observational ratio, not a causal estimate.**

> **What TIER does not measure. Read this before relying on a number.**
> - **Capture depends on the account and settings.** New in v0.6.2, experimental `collectors.copilot_billing` can capture GitHub Copilot organisation AI-credit gross spend, and `collectors.cursor_admin` can capture usage on a Cursor team plan with Admin API access (Teams or Enterprise; Cursor decides which plans include it). Individual Copilot Pro and individual Cursor accounts are not covered by these pollers; ChatGPT Team is still not captured. See the [source requirements](docs/technical-reference.md#what-each-source-needs). Work done with them still earns points, so partial capture makes the ratio read high: 20 points over $200 of usage is a TIER of 100, but if only $50 of that usage is captured it reads 400. ([white paper §4.2](docs/whitepaper.md#42-what-is-missing-and-in-which-direction-it-moves-the-number))
> - **Size, not value.** Points are size-weighted accepted work. With no size label, a 5-line fix in one file earns 0.5 and a change of 1,000 lines across 10 files earns 8, whatever each is worth to the business. ([§3](docs/whitepaper.md#3-what-a-point-is-and-is-not-worth))
> - **How work is split.** That 1,000-line change earns 8 as one PR and 25 as five PRs of 200 lines across 2 files each. Size labels are trusted, not checked. ([§10](docs/whitepaper.md#10-known-limits))
> - **AI cost only.** Human time is not in the denominator, so doing more by hand and less with AI raises the ratio. ([P-03](docs/adversarial-analysis.md#p-03-token-free-approaches-incentivized-even-when-ai-is-superior))
> - **Not causal.** TIER links recorded usage to merged work by developer, repository and issue reference. It does not detect whether AI wrote a change, or how much AI helped. ([§7.3](docs/whitepaper.md#73-limits-on-interpretation))

**Is TIER right for you?**
Working alone, use `tierd score --outcomes` to join your local Claude Code costs to your own merged GitHub PRs and see your personal TIER. The default `tierd score` still shows the cost table; `tierd demo` shows the dashboard on invented data. Team reports show a group's TIER, never one person's, once at least k people in the group (default 5, minimum 3) have captured AI spend and at least k have merged work in a sealed calendar month.
TIER helps a team follow merged work per $1,000 of captured AI usage over time, priced at list rates so every plan reads on one scale. It counts accepted work, mainly merged pull requests that reference an issue (plus opt-in direct commits and outcomes posted through its API), weighted by size and adjusted for CI failures and reverts where TIER observes them.
Beside the ratio it shows time to merge (PR created → merged), a separate figure that is not a quality measure.
Check the [per-plan source requirements](docs/technical-reference.md#what-each-source-needs) before a pilot; the Copilot and Cursor pollers are experimental and require organisation or team credentials.

TIER does not, today:
- credit results to individual models. It reports spend by model; the separate Model Report compares models on declared tasks.
- measure review rounds or human interventions.
- give a useful ratio when most of your AI use runs through tools it does not capture; the ratio then reads high.

TIER measures size-weighted outcome points, adjusted for quality where TIER
observes CI failures and reverts, per $1,000 of captured AI usage priced at list
rates. The cost side is in US dollars, not tokens. The work side is in *outcome
points*, not dollars: TIER does not put a money value on the work.
It gives an engineering lead or a CFO one observational ratio to follow over
time. Its positioning is **grade the work, not the worker**. That is a policy stance,
not a mechanism that prevents evaluation.
TIER is built for coaching, not for evaluating developers. Per-person figures
are not accurate enough to judge an individual.
Per-developer rows exist in `developer` mode for previewing coaching
conversations (developer: synthetic demo only from v0.6.1 until #826);
[docs/legal-and-privacy.md](docs/legal-and-privacy.md) describes access limits.
The repo also documents the ways this number can mislead:
[failure-mode analysis](docs/adversarial-analysis.md).

By default, a follow-up fix PR earns its own points and does not reduce the
original. Since v0.6.0, the default-off, operator opt-in Event 3 rule charges an
enabled correction against the original under [rubric 2](docs/rubric.md#rubric-2-corrective-work-operator-opt-in)
and, within 14 days of the original's merge, also lowers the original's quality;
same-author corrections count before confirmation.
Splitting the same work into more PRs can change points, on the same issue or
across issues; developer-mode partition diagnostics accompany the score.

TIER does not detect whether a PR was written with AI. It counts every merged PR
that carries an issue reference, and it counts all the AI spend it captured.

A merged PR contributes points only when TIER derives an issue reference from its branch or description. A merge without one records no outcome. Any AI cost captured for the work still counts. Holding captured spend and all other outcomes fixed, omitting an otherwise eligible PR cannot increase the numeric ratio; it can reduce it.

Bot- and agent-produced changes can earn points even when their AI usage is not captured. Account type does not establish whether a change used AI, and missing usage can make the ratio read high.

TIER reports captured spending by model; it does not assign merged outcomes to individual models or determine which model caused a result. Same-task comparisons appear in the separate Model Report, which measures model-and-tool combinations on declared tasks. Its benchmark points are not repository TIER.

> Historical self-measurement; original inputs and run settings are not publicly reproducible.
> The maintainer reported **221** size-weighted outcome points per $1,000 on
> TIER's own repository, trailing 30 days to 2026-07-21, across 152 merged PRs,
> with **72.1%** of spend unlinked to an issue. It was computed from backfilled
> history; no quality events were recorded for it, so it is size-weighted only.

The maintainer has exercised TIER's CI and revert rules in manual test runs; the project's automated webhook tests exercise the same rules. The inspected historical 221 figure has no recorded quality-event receipt.

Claude costs are derived from deduplicated Claude Code usage logs. Their completeness against independent provider accounting remains unvalidated, including for subscriptions such as Claude Max seats. Missing usage would understate those costs. Codex costs use logged cumulative counters; those counters do not independently establish complete capture. If the logs undercount, costs are understated, TIER scores read high, and Claude looks relatively cheaper in cross-vendor comparisons.

How to read that: one *outcome point* is one unit of `weight × quality` (defined
below). Points are the project's size conventions, derived from labels or changed lines and files. They are not validated measures of difficulty, maintainability, customer value, engineering time or the contribution made by AI. The preserved 152-outcome snapshot averaged 4.7599 points per PR; all stored weights have `weight_source=git-heuristic`. These facts have not yet been established for the exact published window.
The published rounded totals decompose to about 4.76 points per recorded PR and 46.5 recorded PRs per $1,000. The preserved snapshot's relationship to those historical inputs remains unverified.
221 points per $1,000 is the same as about $4.52 of list-price spend per point
(1000 ÷ 221), not money paid. The reported unlinked 72.1% remains in that
historical denominator. This is reported history of one repository, not
independent validation. TIER defines no "good" or "bad" value; compare a team
with its own earlier windows under matched measurement definitions
([docs/interpreting-the-number.md](docs/interpreting-the-number.md)).

**Complement, not replacement.** DORA, SPACE and DX Core 4 are existing
frameworks for measuring software delivery and developer experience. TIER adds
a measure of merged work per $1,000 of captured AI usage, priced at list rates:
an observational ratio, not a causal estimate. The page linked below compares
it with each of them. Run it *beside* your existing
metrics framework, never fused into a single performance number. See
[How TIER relates to DORA / SPACE / DX Core 4](docs/how-tier-relates.md).

The core metric:

```
TIER = Σ(weight × quality) / (list-price cost / $1000)
```

- **weight** — the size of a merged outcome, from 0.5 to 8 (from a PR size
  label, or a lines/files heuristic when no label is set; table under
  [Recording outcomes](#recording-outcomes)). The values are a convention of the
  rubric, explained in [docs/rubric.md](docs/rubric.md).
- **quality** — 1.0 for a clean merge. It drops to **0.7** if a GitHub Actions
  workflow run on the merge commit, on the default branch, fails within 48 hours
  (unless a re-run of the same workflow on the same commit passes within 30
  minutes of the failure, which marks it as flaky), and to 0.8 or 0.1 if the
  change is reverted within 60 days. TIER tells the two revert kinds apart from
  words in the revert's commit message: words such as "product decision" or "no
  longer needed" with no words such as "bug" or "regression" give 0.8 (a
  business decision); anything else gives 0.1 (a code problem). The lowest
  applicable value wins. The 48-hour and 60-day clocks use GitHub's `merged_at`
  for a PR; the webhook falls back to receipt time when that value is missing,
  malformed, zero or future. 1.0
  means only that no penalty signal arrived: it is not a code review, and a repo
  with no CI never loses points for CI. These signals arrive through the GitHub
  webhook. A late one changes the pull request's own window, not the window the
  signal arrives in: an eligible recognised September revert of an August merge lowers August's
  live score the next time it is read. Sealed monthly reports remain unchanged. `tierd backfill` records past PRs at 1.0 and does not
  rebuild their CI or revert history, so a score built only from backfilled PRs
  is size-weighted but not yet quality-weighted.
- **list-price cost** — the eligible captured spend in the window, in US dollars,
  whether or not it could be linked to an issue. Developer views include that
  developer's captured spend. Group reports cover identified developers' captured
  spend and exclude pseudo-developer spend, including poller remainders.
  Repository scopes also exclude spend without a qualifying repository. It is priced from a versioned reference price table (list price, not
  your negotiated invoice). Each event keeps the price it was recorded at; a new
  table version changes past costs only if you run `tierd reprice`.

A team's TIER is the team's summed points divided by its summed cost, not an
average of its members' scores.

TIER also reports two supporting metrics:

- **Coverage %** (the dashboard's *Capture Fidelity*; `coverage_pct` in the API)
  — of the spend TIER **did** capture, the share recorded request by request
  (proxy / JSONL / Codex / Opencode / Muse) rather than as a coarse total: the daily
  figures from the optional Anthropic or OpenAI usage pollers, or a cost you
  post by hand to `POST /api/v1/costs`. It measures how precise the captured
  spend is, not how much spend was captured: AI use TIER never saw does not
  lower it. It is not the share of cost linked to an
  issue.
- **Spend Leverage** — list-price cost ÷ what you actually paid. It shows how
  far list price is from your real bill, for example because of a negotiated
  discount or a flat-rate subscription: 2.0 means list price was twice what you
  paid. Server-only: someone in finance posts what was paid for each calendar
  month, per developer (`POST /api/v1/actual_spend`) or for the whole org
  (`POST /api/v1/org_actual_spend`). A developer's own figure wins for that
  month; the org figure, less those, is split evenly across the org's other
  active members. Rendered as `—` until an entry exists.

Capture completeness is unknown unless independently reconciled for the stated engines, routes, versions and reporting window. A healthy collector or a 100% Fidelity reading does not establish complete capture. See [capture and pricing limits](docs/technical-reference.md#32-pricing-list-prices-and-why).

---

## ⚠️ Status & scope

TIER is **pre-v1** and **single-tenant**. It runs as a single Go binary
(`tierd`) over a single local SQLite file. There is no clustering, no external
datastore, and no message bus.

> **No tenant isolation exists.** Developer identifiers are global — there is no
> `tenant_id`/org column, so two organizations' `alice` rows would collide.
> **Do not deploy this as a shared multi-org service.**

Run it for one team or one organization at a time.

**Who this release is for (from v0.6.1).** This release requires that every credential holder and database owner be you, or someone every participant wholly trusts. “Wholly trusted” means every participant knowingly accepts the documented identity and administrator-trust limits, and every read, ingest or admin credential holder and database owner is trusted with all retained developer data. Serve real data only in name-free team/division modes; shared-team promotion waits for #826 and #1145. From v0.6.1, the server refuses all non-synthetic developer mode, including loopback and read-only operation, until #826's named-viewer, audit and retention controls ship. The synthetic `tierd demo` remains exempt and shows the dashboard on invented data. Solo users get their own Claude Code cost table with `tierd score`, or their own local TIER with `tierd score --outcomes`. The latter joins only the GitHub token owner’s merged PRs, stores nothing after exit and shows no team band.

The team/division k-floor is not robust against an untrusted ingest holder until #1145 binds uploads to identities. Measured at k=3 in both modes: invented unrostered IDs did not count, but uploads impersonating two rostered IDs with existing outcomes and no spend caused a previously withheld one-spender cohort to publish its aggregate. This pins the current limitation; it does not repair #1145.

---

## Prerequisites

TIER builds from source. You need **Go 1.27.3+**
(the floor `go.mod` sets, for standard-library security fixes), **make**, and **git**. Some examples also use
`openssl`, `curl`, Docker or cron. Clone
the repo and build the `tierd` binary once:

```sh
git clone https://github.com/tiermetric/tier.git
cd tier
make build                       # produces ./bin/tierd
```

The commands below assume you are in the repo root with `./bin/tierd` built.
Where an example calls plain `tierd`, it means the same binary: use
`./bin/tierd` from the repo root, or put it on your `PATH`.

**Three ways to skip the clone.**
`go install github.com/tiermetric/tier/cmd/tierd@latest` still needs Go (it
compiles for you) and puts `tierd` in `$(go env GOPATH)/bin` (usually
`~/go/bin`; `GOBIN` overrides it) — if the shell says `command not found`, add
that folder to your `PATH`. Or download a release archive from
<https://github.com/tiermetric/tier/releases>: each
`tierd-<version>-<os>-<arch>.tar.gz` unpacks to a folder holding one file,
`tierd`, which you move somewhere on your `PATH`; this route needs no Go.
Details, including which archive to pick, are in
[docs/quickstart.md §1](docs/quickstart.md#1-install). A `windows-amd64` archive
(`tierd.exe` inside) is published. A native command-line smoke test of tierd
v0.6.2 on Windows 11 Enterprise x64 (2026-10-22, synthetic data only) passed;
scoring populated data, the dashboard against a real server, a browser download
(SmartScreen) and these docs' bash/zsh commands are not yet tested there, and
there is no Windows CI. The owner-only (0600) database guarantee does not hold
there ([docs/security.md §4](docs/security.md#4-filesystem-hardening)); what was
observed is in [docs/quickstart.md §1](docs/quickstart.md#1-install). Treat
Windows as experimental.

Homebrew (macOS): `brew install tiermetric/tap/tierd` builds tierd from source.

## What you need before you start

TIER generates no secrets for you, and it contacts GitHub only when you run
`tierd backfill`. The commands on this page use a few values that **you** create
or look up. Here is each one, what it looks like, and how to get it. Make a
folder to keep them in, readable only by you (`chmod` also fixes the permissions
of a folder that already existed):

```sh
mkdir -p ~/.tier && chmod 700 ~/.tier
```

### The server address (`<your-server>`, `TIER_HOST`)

`<your-server>` is the host name (and port, if it has one) where your
`tierd serve` is listening: for a team server, wherever you deployed it, e.g.
`tier.example.com`. Wherever the docs say `https://<your-server>`, put yours in
its place — no trailing slash, no path. On your own laptop it is
`127.0.0.1:8080`, and you use `http://`, not `https://`:
`http://127.0.0.1:8080`. `TIER_HOST` (used in CI examples) holds the whole
address, `https://` included.

`tierd` itself speaks plain HTTP only. To reach it at an `https://` address, put
a TLS-terminating reverse proxy (nginx, Caddy, or your cloud's load balancer) in
front of it and use the proxy's address as `<your-server>`. Keep the API token
set behind a proxy: tierd bound to `127.0.0.1` with no token trusts every
request, and the proxy would pass anyone's request through to it. Bind tierd to
`127.0.0.1`, or firewall its port, so only the proxy can reach it.

### The API token (`TIER_API_TOKEN`, `--api-token`)

A password you make up. It protects the server. Once it is set, writes
need an admin credential (event uploads also accept the ingest token), and
every request through the optional reverse proxy (see
[Capturing tokens](#capturing-tokens)) needs it, and every score read needs it
or the read-only token below. `serve` refuses to listen on a non-loopback
address without one. Generate one and keep it in a file:

```sh
(umask 077; openssl rand -hex 32 > ~/.tier/api-token)   # file is 0600 from the start
```

It looks like 64 hex characters: `3f9a0c…`. If the file is readable by other
users, `tierd` warns once at startup (not on Windows). If you own the file,
`chmod 600` clears it; for a container secret mount, set the mount's file mode
instead, and a warning on a mount `tierd` reads as another user is expected.
Give the **file** to `tierd`:

```sh
export TIER_API_TOKEN=@$HOME/.tier/api-token     # every tierd command reads @file
./bin/tierd serve --aggregation team --addr 0.0.0.0:8080   # --aggregation: see "Run it for a team"
```

Give the **value** to anything that is not `tierd` (curl, a CI job, a scraper).
The authenticated alias-list endpoint works before any month is sealed:

```sh
printf 'Authorization: Bearer %s\n' "$(cat ~/.tier/api-token)" |
  curl -f -H @- http://127.0.0.1:8080/api/v1/developer_alias
```

(`-H @-` makes curl read the header from its input, so the token never appears
in curl's command line, where `ps` could show it to other users.)

> **The `@/path` form is understood by `tierd` only.** `curl` would send the
> literal text `@/home/you/.tier/api-token` as the token and get `401`. When a
> curl example says `$TIER_API_TOKEN`, that variable must hold the value, not
> the `@` path.

On a single-user machine, you do not need a token while everything runs on
`127.0.0.1`: with no token, anyone who can reach that address can use every
route, including erase. But every
`tierd` command in a shell where `TIER_API_TOKEN` is exported uses it: `serve`
then turns auth on, clients must send a credential with the appropriate scope,
and the dashboard asks for a read or admin token. For `ship`, use the ingest
token described below; the admin token also works.

### The read-only token (`TIER_READ_TOKEN`, `--read-token`) — optional

A second password, same recipe, different file, for people who should see the
dashboard but never write or erase:

```sh
(umask 077; openssl rand -hex 32 > ~/.tier/read-token)
```

It must differ from the API token (`serve` refuses to start if they match),
and it does nothing on its own: without an API token, auth is off and `serve`
logs a warning. Give the file to `serve` next to the API token:

```sh
export TIER_READ_TOKEN=@$HOME/.tier/read-token   # or: serve --read-token @$HOME/.tier/read-token
```

Then send each viewer the **value** (`cat ~/.tier/read-token`). The dashboard
page itself loads without a token, but the numbers on it do not: when a viewer
opens it, it asks for a token; they paste that value into the token box and
click **Refresh**.

### The ingest token (`TIER_INGEST_TOKEN`, `--ingest-token`) — recommended for shippers

Since v0.6.0, the ingest token is the recommended credential for `tierd ship`
(#1144). Generate a distinct credential for laptop shippers:

```sh
(umask 077; openssl rand -hex 32 > ~/.tier/ingest-token)
export TIER_INGEST_TOKEN=@$HOME/.tier/ingest-token   # server: restart serve with this set
# On each shipping laptop, provision that ingest-token file, then:
tierd ship --server https://<your-server> --repo ~/src/app --api-token @$HOME/.tier/ingest-token
```

It opens only `POST /api/v1/events`. It must differ from the admin, read and
metrics tokens; alone it does not enable auth or permit a non-loopback bind.
The shipper's existing `--api-token` / `TIER_API_TOKEN` input carries this
credential; the admin token still works. The server also accepts the config key
`http.ingest_token`. See [security.md](docs/security.md#the-ingest-token-1144)
for the remaining impersonation and pre-claim limits until #1145 (gated on #826).

### The metrics token (`TIER_METRICS_TOKEN`, `--metrics-token`) — optional

A third password, for Prometheus. It opens `GET /metrics` and nothing else. In
`team` and `division` mode the read-only token cannot scrape `/metrics`, so give
your scraper this token (or the API token). Same recipe, its own file:

```sh
(umask 077; openssl rand -hex 32 > ~/.tier/metrics-token)
export TIER_METRICS_TOKEN=@$HOME/.tier/metrics-token   # or: serve --metrics-token @$HOME/.tier/metrics-token
```

It must differ from the API and read-only tokens (`serve` refuses to start if
any two match), and, like the read-only token, it does nothing without an API
token. It is for the operator only, never for viewers: see
[docs/security.md § The metrics token](docs/security.md#the-metrics-token-944).

### The GitHub token (`TIER_GITHUB_TOKEN`, `backfill --token`, `score --outcomes --token`)

`tierd backfill` and `tierd score --outcomes` use it to read your merged pull
requests via `GET /repos/<owner>/<name>/pulls` and
`GET /repos/<owner>/<name>/pulls/<n>`. `tierd score --outcomes` first calls
`GET /user` to bind the report to the token owner. Create a fine-grained
personal access token at <https://github.com/settings/personal-access-tokens/new>:

- **Resource owner:** the account that owns the repo — your own name, or the
  organization for an org repo (the org's owners may have to approve the token
  before it works).
- **Repository access:** *Only select repositories* → pick the repo.
- **Permissions → Repository permissions → Pull requests:** *Read-only*.
  (Note: GitHub normally adds *Metadata: Read-only* on its own when you do
  this — standard GitHub behaviour, not something TIER asks you to set.)

It looks like `github_pat_11A…`. Paste it into a file without putting it on the
command line — type the command, paste the token, press Enter, then Ctrl-D:

```sh
(umask 077; cat > ~/.tier/github-token)
```

Then `export TIER_GITHUB_TOKEN=@$HOME/.tier/github-token`.

### The webhook secret (`TIER_WEBHOOK_SECRET`, `--webhook-secret`) — team servers only

A shared password between GitHub and your server so nobody can forge a merged PR.
Same recipe:

```sh
(umask 077; openssl rand -hex 32 > ~/.tier/webhook-secret)
```

Give the file to `tierd` (`export TIER_WEBHOOK_SECRET=@$HOME/.tier/webhook-secret`)
and paste the same value into the **Secret** box of the GitHub webhook — see
[docs/webhook-setup.md](docs/webhook-setup.md).

### Identifiers you will see in examples

| You see | It means | Example |
|---|---|---|
| `alice`, `<id>`, `--developer` | A developer's name. On the **cost** side it is your OS username (`whoami`) unless you pass `--developer`; on the **outcome** side it is the PR author's GitHub login. If those differ, map them once — see [Identity mapping](#identity-mapping). | `alice` |
| `issue-42`, `<issue-id>`, `TIER-99` | The issue a piece of work belongs to, read from the branch name (`feature/42-login` → `issue-42`) or PR body (`closes #42`). A configured tracker key like `TIER-99` is kept as-is; `OP` is built in. | `issue-42` |
| `--repo ~/src/app` | A **local path** to a git checkout (`score`, `ship`, `doctor`). | `~/src/app` |
| `--repo owner/name` | The GitHub repository, as it appears in the URL after `github.com/` (`backfill` only). | `acme/api` |
| `org`, `team`, `division` | Names you choose for your organisation's structure, used consistently in `org_hierarchy` and `org_actual_spend`. | `acme`, `platform` |
| `period` | A calendar month, `YYYY-MM`, used when you post what was actually paid (see Spend Leverage above). | `2026-09` |
| `--prices file.yaml` | A price-table override. You almost never need one; see [docs/reference-price-table.md §9](docs/reference-price-table.md#9-version-is-binding--one-number-one-table-714). | — |

## Three ways in

Pick the one that matches what you want right now:

| Path | You get | Effort |
|---|---|---|
| [See it in 60 seconds](#see-it-in-60-seconds) | A populated dashboard on synthetic data | One command |
| [Score your own repo](#score-your-own-repo) | Your own cost table or opt-in local personal TIER; team TIER after a sealed month with at least k people | One score command; GitHub token for `--outcomes` ([how to get one](#what-you-need-before-you-start)) |
| [Run it for a team](#run-it-for-a-team) | A shared server recording outcomes live | Server or container deployment |

**[docs/quickstart.md](docs/quickstart.md)** has the same steps as copy-paste
commands, plus installing without cloning
(`go install github.com/tiermetric/tier/cmd/tierd@latest`), capturing Codex, and
viewing the dashboard from another machine. The running binary also serves it,
at `http://127.0.0.1:8080/docs/quickstart.html` once `serve` is running.

New to the metric itself? Read
[docs/understanding-tier.md](docs/understanding-tier.md) — what TIER measures
and why, in three layers.

---

## See it in 60 seconds

To preview TIER before wiring up your own data, run `tierd demo`. It seeds an
obviously synthetic dataset and serves the dashboard — no setup, no Claude Code
history required:

```sh
./bin/tierd demo                 # then open http://127.0.0.1:8080
```

Every name on it is marked as fake: `demo-*` developers, `DEMO-*` issues, an
`ACME (DEMO)` org, and a `SYNTHETIC DATA` banner in the console. From v0.6.1,
the database is fresh SQLite in a random private temporary directory (mode 0700), removed
on exit, including Ctrl-C and SIGTERM. The demo refuses `--db`; neither flags,
environment nor config can select its database, and concurrent demos are
isolated. If `TIER_API_TOKEN` is exported in that shell, the demo dashboard asks for
that token too.

---

## Score your own repo

**Working alone, use `tierd score --outcomes` for your own local TIER.**
Team reports still withhold groups smaller than k (default 5, minimum 3).

**For the team server path below:** four commands across two terminals (`export`,
`backfill`, `serve`, `ship`), one mandatory GitHub token, and three pitfalls,
marked GOTCHA 1–3 below.
(Plus the one-time `make build` under [Prerequisites](#prerequisites).)

### First, a free look at the cost side

No server, no token, no config. `tierd score` reads your local Claude Code
session files under `~/.claude/projects/` and prints where the AI money went for
a given repository:

```sh
# Point --repo at a repo you have actually been working in with Claude Code.
# `--repo .` here would attribute the TIER checkout you just cloned, where you
# have no session history yet -- it prints "No Claude Code sessions found" and
# exits 0, which reads as success.
./bin/tierd score --repo ~/code/your-project    # the last 90 days for that repo
```

To include your own merged PRs:

```sh
# Set TIER_GITHUB_TOKEN privately, or use --token @/path/to/token.
./bin/tierd score --repo ~/code/your-project --outcomes
```

This binds outcomes to the token’s `GET /user` login and joins your OS username
for this run only. It uses the server’s scoring window and prints points, cost,
TIER and the same data-quality flags. It shows no team band. The private temporary
SQLite directory is removed on exit, including errors and Ctrl-C. HTTPS is required;
`--developer`, `--author`, `--db`, `--server` and shipping flags are refused.
Only Claude Code costs are captured; historical PRs start at quality 1.0, without
reconstructing later CI failures or reverts. Every outcomes run prints:

> TIER is built for coaching, not for evaluating developers. Per-person figures are not accurate enough to judge an individual.

> **`--repo` is a local path** for `score`, `ship` and `doctor` — `backfill` alone
> takes the GitHub repository as `owner/name`, the part of its URL after
> `github.com/`. On a fork, your checkout's `origin` names the fork, but outcomes
> are recorded against the upstream repository, so tell TIER the upstream:
> `score --repo-slug upstream-owner/name`, or for `ship`,
> `--repo-slug /path/to/checkout=upstream-owner/name`. Without it your cost never
> joins your outcomes.

Output shape (values will differ; `YYYY-MM-DD` is the start of the 90-day
window, and the issue table is trimmed here to two rows, so they do not add up to
the total). This example uses v0.6.0 pricing (table v14, effective 2026-10-12,
with 93 models):

```
price table: embedded default (version 14, 2026-10-12, 93 models)

TIER Cost Attribution — since YYYY-MM-DD
Source: Claude Code JSONL (real-time, per-request)
───────────────────────────────────────────────────────────────────────────────
Developer              Input tok  Output tok    Cache rd   Cache w5m   Cache w1h    Cost ($)
───────────────────────────────────────────────────────────────────────────────
alice                    1898883     2197887   349279392           0    17615403    496.3612
───────────────────────────────────────────────────────────────────────────────
TOTAL                                                                              496.3612

Cost by Issue
──────────────────────────────────────────────────────────────────
Issue                               Model                   Cost ($)
──────────────────────────────────────────────────────────────────
unattributed: branch, no issue #    claude-opus-4-8         344.5618
issue-127                           claude-opus-4-8          67.8453
...

Tip: use `tierd score --outcomes` for your local personal TIER. To record PR outcomes and compute full TIER scores, follow the quickstart (it gives the flags `tierd backfill` and `tierd serve` need). Read it now with `tierd demo`, then open http://127.0.0.1:8080/docs/quickstart.html (the default address).
```

This is cost-attribution **only** — it has no PR outcomes, so it is not a TIER
score. In the token columns, *Cache rd* is tokens read back from the provider's
prompt cache, and *Cache w5m* / *Cache w1h* are tokens written into it to be kept
for 5 minutes or 1 hour; each kind has its own price. Useful flags:
`--since 2026-01-01`, `--developer <id>` (default: your OS username),
`--prices <file.yaml>` (a price-table override — you almost never need one; see [What you need](#what-you-need-before-you-start)).

That large unattributed row is spend from sessions whose branch carried no
recognizable issue reference. Other unattributed rows name their own cause, such
as a detached HEAD or work recorded on `main` or `master`. See [docs/conventions.md](docs/conventions.md) to
fix it.

### Then, the full score

A TIER score also needs **outcomes** (merged PRs). Both sides can be
reconstructed from history, so you don't have to wait for new activity:

- **Outcomes (the numerator)** — `tierd backfill` walks your repo's merged-PR
  history via the GitHub API and reconstructs one outcome per merged PR that
  carries an issue reference (a PR without one is counted in its summary and
  skipped; see [Recording outcomes](#recording-outcomes)). It covers the **last
  90 days** by default; `--since` reaches further back. Re-running it is safe: it
  skips any PR it already has (matched on the merge commit), so it never
  overwrites a quality penalty the webhook recorded.
- **Cost (the denominator)** — `tierd ship` forwards the last 90 days of your
  local Claude Code JSONL to the server. (`serve --watch-repo` only tails *new*
  activity; `ship` is how you load the back catalog.)

Both commands default to 90 days, but that does not make cost and outcomes cover
the same period. `ship` can only send the session logs still on your disk, while
backfill finds every merged PR in the window that carries an issue reference.
Cost exists only from your **cost horizon** forward: the earliest cost event in
the database (or in one repository, when a score is filtered to it). It marks
where capture began; it cannot see a gap in capture later on. **The rule: a score window must not start before the cost horizon.**
If it does, outcomes with no cost beside them push the score up. The opposite
bias also exists: in a short or very recent window, spend on work that has not
merged yet is counted while its outcome is not, so the score reads low.

The dashboard defaults to a 30-day window and `GET /api/v1/scores` to 90 days;
either can reach past the horizon. To check, read
`data_quality.window_predates_cost_capture` in the `/api/v1/scores` response
(the dashboard shows a banner, and `tierd doctor` a `cost horizon` check). If it
is `true`, set the dashboard's **From** date (or `?since=` on the API) to
`data_quality.cost_coverage_safe_since`. More in
[docs/interpreting-the-number.md](docs/interpreting-the-number.md).
`tierd doctor --repo <path>` is an install self-check: it also confirms your
session files are found, branches map to issues and the server is reachable,
prints OK / WARN / FAIL for each check, and exits non-zero on any FAIL.

```sh
# 1. Reconstruct the last 90 days of outcomes into the default DB (~/.tier/tier.db).
#
#    ⚠️ GOTCHA 1 — a GitHub token is REQUIRED here. Create a fine-grained token
#    with only "Pull requests: Read-only" on this repo and save it to a file —
#    exact steps under "The GitHub token" in What you need before you start.
#    Never paste the token on the command line; it leaks via ps and history.
#
#    ⚠️ GOTCHA 2 — run backfill BEFORE serve. The store is SQLite, which lets
#    only one process write at a time; a backfill write that waits more than
#    5 seconds for serve's lock fails with "database is locked". To backfill
#    again later, stop serve, run backfill, start serve. Pass the same --db
#    to backfill and serve (or neither, as here).
export TIER_GITHUB_TOKEN=@$HOME/.tier/github-token   # the file you saved it in
./bin/tierd backfill --repo your-org/your-repo        # GitHub "owner/name", not a path

# 2. Start the server (see GOTCHA 3 immediately below this block).
./bin/tierd serve --aggregation team
# tierd listening addr=127.0.0.1:8080

# 3. In a SECOND terminal, ship your last 90 days of cost to it. If the first
#    terminal has TIER_API_TOKEN exported, export the same value here too.
./bin/tierd ship --server http://127.0.0.1:8080 --repo /path/to/local/checkout
# Per-repo summary:
#   "/path/to/local/checkout": sessions_with_events=12 events_shipped=3481
# Shipped N events ... Re-running is safe: the server dedups on idempotency keys.
#
# A run that recovers NOTHING exits 1 rather than reporting success —
# the usual cause is a --repo path you never worked in. Pass --allow-empty if
# zero is expected.
```

> ⚠️ **GOTCHA 3 — solo or small cohort?** Team/division reports withhold
> numbers below the k floor (default 5, hard minimum 3). From v0.6.1, developer
> mode on real data is refused. Use `tierd score --repo /path/to/local/checkout`
> for your own Claude Code cost table and the issue-attributed share of that spend,
> plus `tierd demo` for the dashboard on invented data. Neither gives a TIER number
> on your own real data.

Team/division dashboards publish closed, sealed calendar months only, after
sealing is armed and the grace period has elapsed. Follow the
[quickstart's sealing steps](docs/quickstart.md#7-viewing-the-dashboard-from-another-machine)
to publish an eligible month; backfill and shipping alone do not publish it.

**A brand-new trial** will probably sit below the evidence floor. A score with
fewer than 3 merged PRs, or under $5 of list-price spend, is still shown but is
marked below the floor and never ranked, because with so little data the ratio
means little (one 0.5-point PR against $0.0004 of spend would read 1,250,000).
A PR whose issue shows fewer than 1,000 tokens of recorded AI use in the 14 days
before it merged is flagged the same way: its work was probably done where TIER
could not see it. The first two marks clear as data accrues. Use name-free team/division reports within the trusted-use scope above (see
[docs/legal-and-privacy.md](docs/legal-and-privacy.md)).

**One identity caveat.** Cost is attributed to your shipping identity (your OS
username, or `ship --developer <id>`); outcomes to the PR author's GitHub login.
If those differ, map them to one developer so the two sides combine — see
[Identity mapping](#identity-mapping). Otherwise cost and outcomes land in
separate rows and TIER can't pair them.

---

## Run it for a team

A team server records PR outcomes (via the GitHub webhook), computes full TIER
scores, and serves a dashboard at `/`.

### Server mode

**Local / loopback (no token needed):**

```sh
./bin/tierd serve --aggregation team --watch-repo ~/src/app
# tierd listening addr=127.0.0.1:8080
curl -fs http://127.0.0.1:8080/api/v1/livez
# {"status":"alive","uptime_s":1,"version":"..."}
```

This loopback server records cost from the watched repo, but no webhook
outcomes: GitHub cannot reach `127.0.0.1`, and the webhook route is mounted only
when a webhook secret is set. Use `tierd backfill` for outcomes here, or the
production setup below.

**Team and division reports show closed, sealed UTC months only.** The privacy
floor would be bypassed if readers could subtract overlapping live windows to
isolate one person's figures. Before arming, `/api/v1/scores` returns `404`
(`sealing not armed`) and the dashboard shows that the month is not published.
After backfilling cost and outcomes and loading the team and alias maps, run on
the server under the same account and environment as the loopback command above:

```sh
./bin/tierd seal --arm earliest --dry-run --aggregation team
./bin/tierd seal --arm earliest --aggregation team
```

Use the same database and any `--config`, `--prices`, `--k-anonymity` and
`--report-grace` settings as `serve`; for the production example below, add
`--db /var/lib/tier/tier.db` to both commands. The second command prompts, seals
the first eligible month and irreversibly pins it. It needs a full covered month
past the default 14-day grace and settled sources; a fresh install must backfill
or wait. Keep writable `serve` running: it registers sources and seals subsequent
due months hourly. Read the sealed month with `?period=YYYY-MM`; the k-anonymity
floor still applies. See [quickstart §7](docs/quickstart.md#7-viewing-the-dashboard-from-another-machine)
for prerequisites, status checks and the dashboard's first-seal state.

`--aggregation` is **required** — `serve` refuses to start without it (there is
no default, so an existing deployment's reporting mode never changes silently).
Choose `team` or `division` for real data; both emit aggregates that never name
an individual. From v0.6.1, `developer` is reserved for the synthetic `tierd demo`
until #826; real-data startup is refused, including loopback and read-only operation.
The mode is also settable via `TIER_AGGREGATION` or the `aggregation` config key.
`division` rolls teams up one level. See
[docs/legal-and-privacy.md](docs/legal-and-privacy.md) for deployment guidance.
`team` and `division` both enforce the same k-anonymity floor (`--k-anonymity`, default 5,
hard minimum 3; it counts developer identifiers, so load the alias map first or
one person with two identifiers counts twice): a group with fewer developers contributing inside the score
window is folded into an anonymous "other" row, and if "other" itself is below
the floor it is withheld too. Until you load a team map
([below](#team-map-from-a-spreadsheet)), `team` mode has no teams, so everyone
lands in "other" and `serve` warns about it at startup. In these two modes one
developer's score (`GET /api/v1/scores/{developer}`) answers `404` and the raw
per-developer exports (`GET /api/v1/events`, `/outcomes`, `/quality_events`,
`/quality_history`, `/fidelity`) answer `403`. Admin-token routes that exist to
manage people (the team map, the alias map, the GDPR export and erase) still
name them. See
[docs/legal-and-privacy.md](docs/legal-and-privacy.md) before you show named
developers' numbers to anyone.

`--watch-repo` tails that repo's Claude Code JSONL live. It is repeatable; omit
it to disable live ingestion.

**Production (exposed bind — a token is mandatory):**

The API token is a password you choose — make one with `openssl rand -hex 32`
as shown under [What you need before you start](#what-you-need-before-you-start)
— and the webhook secret is a second one shared with GitHub. Put each in its
own file:

```sh
export TIER_API_TOKEN=@/etc/tier/api-token          # tierd reads the file; never a literal
export TIER_WEBHOOK_SECRET=@/etc/tier/webhook-secret
./bin/tierd serve --aggregation team --addr 0.0.0.0:8080 --db /var/lib/tier/tier.db
```

`/etc/tier` and `/var/lib/tier` are example paths: create them yourself, owned
by the account that runs `tierd`, with the secret files readable only by that
account. `tierd` serves plain HTTP, so put a TLS proxy in front of it (see
[The server address](#the-server-address-your-server-tier_host)), and run it
under a service manager such as systemd if it must survive a reboot.

Two safety rules the binary enforces at startup:

- **Fail-closed bind.** A non-loopback `--addr` without an API token is refused —
  it would expose unauthenticated spend data and an open provider relay:

  ```
  refusing to bind "0.0.0.0:8080" without an API token: a non-loopback listener
  would expose unauthenticated spend data and an open provider relay; set
  --api-token (or TIER_API_TOKEN) or bind to 127.0.0.1
  ```

- **`@file` secret indirection.** Any secret flag (`--api-token`,
  `--webhook-secret`) accepts `@/path/to/file`, so the secret is read from disk
  and never appears in `ps`, shell history, or process-accounting logs.

For a config file instead of flags, copy
[`config.example.yaml`](config.example.yaml) to `config.yaml`, edit it, and run
with `tierd serve --config config.yaml`. Precedence is **CLI flag > env var >
config file > builtin default**.

### Docker mode

Build the image, then run it. A container serving traffic OUT of the container
must bind `0.0.0.0` **and** set a token (the fail-closed rule above applies), and
must point `--db` at the writable `/data` volume (the default `~/.tier` path is
not writable under the non-root runtime user):

```sh
make docker                      # builds tierd:latest

# one time: put the token (and, to receive webhooks, the webhook secret) in a
# file only you can read, in docker's KEY=value form
(umask 077; printf 'TIER_API_TOKEN=%s\nTIER_WEBHOOK_SECRET=%s\n' \
  "$(cat ~/.tier/api-token)" "$(cat ~/.tier/webhook-secret)" > ~/.tier/docker.env)

docker run --read-only --cap-drop=ALL --security-opt no-new-privileges \
  -p 8080:8080 --env-file ~/.tier/docker.env -v tier-data:/data tierd \
  serve --aggregation team --addr 0.0.0.0:8080 --db /data/tier.db

curl -fs http://localhost:8080/api/v1/livez
```

`--read-only` is safe — tierd needs no writable rootfs, only the `/data` volume.
The image runs as non-root uid 65532 on a minimal static base (no shell, no
package manager). The container cannot read your laptop's token file, so the
example passes the **value** in with `--env-file`. That keeps it off the
command line: `-e TIER_API_TOKEN="$(cat …)"` would put the token in `docker`'s
arguments, where `ps` shows it to every user on the machine. It does not hide
the value from anyone allowed to run `docker inspect` on that host, which
prints a container's environment. If you mount the secret as a file instead,
the `@/path` indirection form works inside the container too
(`-e TIER_API_TOKEN=@/run/secrets/tier-token`), and only the path shows.

> **JSONL watching does not work in Docker** — there is no `~/.claude` inside the
> container. Container mode is API / webhook / proxy only. Feed laptop-captured
> token usage to a containerized server with `tierd ship` (the laptop-shipper
> topology, see [Capturing tokens](#capturing-tokens)) or the reverse proxy.

---

## Capturing tokens

Here "tokens" means the units AI providers bill by, not passwords. TIER is
**JSONL-first** (it reads the JSON-lines session files Claude Code already
writes, so Claude Code needs no setup). Three capture paths, used together or
separately:

**1. Claude Code session files (default).** `tierd score` reads them directly;
`tierd serve --watch-repo` ingests them live on the same machine.

**2. `tierd ship` — the laptop shipper.** When developers work on their laptops
but the server runs centrally, a small cron/launchd job forwards each laptop's
JSONL to the central server's `POST /api/v1/events`:

```sh
tierd ship --server https://<your-server> --repo ~/src/app
# e.g. every 15 minutes, as a crontab line:
# */15 * * * * $HOME/go/bin/tierd ship --server https://<your-server> --repo $HOME/src/app --api-token @$HOME/.tier/ingest-token
```

cron does not read your shell profile, so the crontab line cannot see
`TIER_API_TOKEN` or your `PATH`. That is why it passes `--api-token @file` and
calls `tierd` by its full path. `$HOME/go/bin/tierd` is where `go install` puts
it; if you installed it somewhere else, `command -v tierd` prints the path to use.

`ship` is stateless and idempotent — every event carries an idempotency key and
the server dedups on it, so re-shipping the same window on every tick is a no-op.
The key comes from the provider's message id, so a session that is both watched
by `serve --watch-repo` and shipped is still stored once. `ship` exits non-zero
when a run fails or recovers nothing, so a job runner that checks exit codes
can flag a laptop that stopped shipping.
The recommended credential is the **server's ingest token**, configured with
`serve --ingest-token` / `TIER_INGEST_TOKEN` / `http.ingest_token` (#1144).
On the laptop, set `TIER_API_TOKEN=@$HOME/.tier/ingest-token` or pass
`ship --api-token @$HOME/.tier/ingest-token`. The shipper's flag name and Bearer
wire contract stay the same; the admin token still works. The ingest token opens
only `POST /api/v1/events`, with no read, outcome, erasure or proxy access.
Provider proxies still require the admin token. See
[the ingest-token identity limits](docs/security.md#the-ingest-token-1144): a
holder can post under any developer id, rostered or not (no roster check), or
pre-claim a teammate's key until per-developer binding (#1145, gated on #826).
A holder who knows a key can replay it under the same developer with larger
counts, raising that row's token counts and cost (recomputed at the row's original
rates, #1231). Replaying a teammate's key this way raises that teammate's cost
(the TIER denominator); keys are visible to read-token holders via
`GET /api/v1/events` in developer mode.
Cross-developer key collisions leave the original row unchanged and produce a
warning with the rejected count; `ship` completes without retrying them.

Add `--codex-rollout` to also forward Codex CLI spend from
`~/.codex/sessions/**/rollout-*.jsonl` (off by default, mirroring
`serve --codex-rollout`). Without it, PRs built with Codex still count as
outcomes on the central server but their spend does not, so Codex work reads
as free.

Add `--opencode` to also forward Opencode spend from its local SQLite session
store at `~/.local/share/opencode/opencode.db` (off by default, mirroring
`serve --opencode`). The same applies: without it, PRs built with Opencode count
and their spend does not. The store is opened read-only. Only providers whose
published per-token rates are in TIER's price table are shipped — today the
Z.ai coding plan. Ollama's cloud tier is left out because it is sold as a flat
plan with no per-token rate to price it by; the exclusion is named at startup
with a row count.

Add `--muse` (newer than v0.4.1, which rejects the flag) to also forward Meta
Muse Code spend from its session logs at
`~/.local/share/muse/sessions/**/session.jsonl` (off by default, mirroring
`serve --muse`). The same applies: without it, PRs built with Muse count and
their spend does not. A run is shipped once it has finished, and a subagent's
spend ships with the run that started it (#901), except in the cases the
quickstart lists; see
[docs/quickstart.md](docs/quickstart.md#capturing-meta-muse-code).

**3. Reverse proxy (optional).** Point a provider SDK at tierd and it captures
token usage from the response as it passes through:

```sh
export ANTHROPIC_BASE_URL=https://<your-server>/anthropic   # http://127.0.0.1:8080/anthropic on a laptop
# per-request headers, set on the SDK client:
#   X-Tier-Token:     the API token value (the same one serve was started with)
#   X-Tier-Developer: who the spend belongs to, e.g. alice   (optional; else "unattributed")
#   X-Tier-Issue:     e.g. issue-42 or TIER-99                (optional)
```

Your provider API key stays in your SDK as usual: tierd adds no key of its own
and passes your client's key on to the provider unchanged. No `X-Tier-*` header
reaches the provider ([docs/security.md §7](docs/security.md#7-proxy-header-hygiene)).
`/anthropic` forwards to `https://api.anthropic.com` and `/openai` to
`https://api.openai.com`; any other OpenAI-compatible provider works through
`/openai` once you point `--openai-target` at it. A client example that sets
these headers, and how to retarget `/openai`, are in
[docs/open-weights-capture.md §1](docs/open-weights-capture.md#1-there-is-no-separate-capture-route--you-retarget-the-openai-proxy).

A Gemini client can point at `https://<your-server>/gemini` the same way, but
that route has not yet been tested against live Gemini traffic (see the note
under [API reference](#api-reference)). Gemini takes its API key in an
`x-goog-api-key` header or a `?key=` URL parameter, not as a bearer token. Use
the header. tierd logs only the request path (not headers, not the query
string), so the key never reaches its logs. A `?key=` value is part of the URL,
so any proxy, load balancer or CDN in front of tierd (nginx, Cloudflare) that
logs full URLs will record the key.

The proxy extracts only token-usage fields from responses (see
[Privacy](#privacy)), never prompt or completion content.

---

## Recording outcomes

Full TIER scores need PR outcomes, which arrive via the GitHub webhook (or,
for past PRs, `tierd backfill`). In the repository's GitHub settings, under
*Webhooks*, the webhook points at `https://<your-server>/webhook/github`, uses
content type `application/json`, carries the webhook secret you made under
[What you need](#what-you-need-before-you-start), and sends *Pull requests*,
*Pushes* and *Workflow runs* events. GitHub must be able to reach that address,
so a server listening only on `127.0.0.1` records no webhook outcomes.
Step-by-step screens and a delivery test:
[docs/webhook-setup.md](docs/webhook-setup.md).

Two conventions decide what TIER can attribute, and nothing warns you on the
spot when they are missed. Both are documented in full in
[docs/conventions.md](docs/conventions.md).

- **An issue reference is required.** A merged PR without one records no
  outcome at all, and spend on a branch without one lands in an `unattributed`
  bucket.
- **A size label is optional.** Without one, a lines/files heuristic sets the
  weight.

**PR size labels** set the outcome weight; with no label a lines/files heuristic
applies:

| Label (case-insensitive) | Weight |
|---|---|
| `size/xs` or `xs` | 0.5 |
| `size/s` or `s` | 1 |
| `size/m` or `m` | 3 |
| `size/l` or `l` | 5 |
| `size/xl` or `xl` | 8 |
| _(none)_ | bucketed on `effort = lines + files×10`: `<=15` → 0.5, `<=60` → 1.0, `<=200` → 3.0, `<=1000` → 5.0, else 8.0 |

In the heuristic, *lines* is lines added plus lines deleted and *files* is the
number of files the PR changed. Nothing is left out: generated files, vendored
code and lockfiles count in full, so label a PR whose size they inflate.

**Issue references** attribute a branch / PR to an issue:

| Where | Format | Example | Resolves to |
|---|---|---|---|
| branch | first slot numeric, no leading zero; year/date guard | `feature/42-auth`, `42-auth` | `issue-42` |
| branch | first slot configured key or built-in OP | `fix/TIER-99-crash` with TIER configured | `TIER-99` |
| PR body / commit subject | close directive | `closes #42` | `issue-42` |
| PR body | explicit relation or trailer | `refs #42`, `Issue: #42` | `issue-42` |
| PR body | configured key in a structured location | `implements PROJ-123` with PROJ configured | `PROJ-123` |
| commit subject | configured key anywhere, word-bounded | `fix: PROJ-123` with PROJ configured | `PROJ-123` |

Configure comma-separated uppercase prefixes with `--tracker-keys`, `TIER_TRACKER_KEYS`, or `attribution.tracker_keys` (CLI > env > config > empty). OP is always built in. Invalid entries stop startup. Serve, ship, score and backfill use the same grammar. Startup and `tierd doctor --db <existing-db>` suggest unconfigured prefixes found in stored outcomes without enabling them.

The branch wins over the PR body. A PR closing several issues still records one outcome under that primary issue. Only the first branch slot counts; later numbers, date/year slots and release, bot and revert branches supply no issue. PR bodies ignore code examples, quoted lines and Corrects declarations. Bare `#N`, free-prose tracker keys and unconfigured tokens such as `UTF-8` do not attribute PRs. Commit subjects accept closing directives and configured keys; a bare `(#N)` can still supply PR metadata for recorded-PR resolution. The change is prospective and rewrites no stored attribution. See [docs/conventions.md](docs/conventions.md) for the exact grammar.

---

## Identity mapping

A developer's JSONL is attributed to their **OS username**, but PR outcomes are
attributed to their **GitHub login**. When these differ, tell the server they are
one person. `alias` is the name on the cost side — what `whoami` prints on the
laptop, or the `--developer` you passed to `ship`. `canonical` is the GitHub
login the outcomes carry. `https://<your-server>` is the address where your
`tierd serve` is listening (`http://127.0.0.1:8080` on a laptop), and the token
is the API token you started `serve` with, read here from its file — both are
described under [What you need before you start](#what-you-need-before-you-start):

```sh
printf 'Authorization: Bearer %s\n' "$(cat ~/.tier/api-token)" |
curl -f -X POST https://<your-server>/api/v1/developer_alias \
  -H @- \
  -H "Content-Type: application/json" \
  -d '{"alias":"alice-laptop","canonical":"alice"}'
# 201 Created
```

> `curl` does not understand tierd's `@/path/to/file` shorthand. If you exported
> `TIER_API_TOKEN=@$HOME/.tier/api-token` for `tierd`, do not reuse that variable
> in a curl header — it would send the literal `@` path and get `401`. Read the
> file, as above.

**On a laptop** — `serve` bound to `127.0.0.1` with no `--api-token` — auth is
off and every route is open to the local machine, so the same call needs no
header at all:

```sh
curl -f -X POST http://127.0.0.1:8080/api/v1/developer_alias \
  -H "Content-Type: application/json" \
  -d '{"alias":"alice-laptop","canonical":"alice"}'
# 201 Created
```

`GET /api/v1/developer_alias` lists the current map;
`DELETE /api/v1/developer_alias/{alias}` removes one. All three require the
**admin** (write) token — an alias edit retroactively re-joins spend to
outcomes, so it is an administrative operation, not a read-scope one.

### Team map from a spreadsheet

Team views need to know who is on which team. Keep that list in any
spreadsheet, export it as CSV, and load it with `tierd hierarchy import`, which
sends it through `POST /api/v1/org_hierarchy` (so aliases resolve and rows are
validated exactly as for a direct API call):

```csv
developer,team,division,org
alice,platform,engineering,acme
bob,payments,,
```

`developer` is the id the dashboard shows (an alias also works). `team` is
the name this import defines: it is what `?team=` and the dashboards show.
Names are case-sensitive (`Platform` and `platform` are two teams), so spell
each one the same way every time. `division` and `org` may be empty; `org` must
be the same name you use when posting `org_actual_spend`. The server address
and admin token are the same ones as above; with a token the address must be
`https://` unless it is loopback (`127.0.0.1`, `::1`, `localhost`):

```sh
TIER_API_TOKEN=@$HOME/.tier/api-token \
  tierd hierarchy import --server https://<your-server> team.csv
# hierarchy import: 2 rows written
```

The import only adds and updates. A developer left out of the file keeps their
current team; it never removes anyone. Team and division memberships are dated
by the server clock, so a later move preserves earlier attribution. Org seats
use inclusive calendar months, so an org move counts in both orgs that month.
To end a developer's membership,
use `POST /api/v1/period_membership/{developer}/end` with the admin token and a
JSON body naming the org and the last month they count in:
`{"org":"acme","period_end":"2026-09"}`. Re-import keeps an explicitly ended
membership ended across all aliases and reports it as **kept-departed**, unless
a membership in any org started at or after that explicit end month. To
re-enrol this month, add the optional CSV column `rejoin` with `true` (values
are case-insensitive `true`/`false`; empty or omitted means false; invalid
values are errors). Single PUT and bulk API writes accept the same boolean
`rejoin` field. Both return `kept_departed` and `reseated_unknown` lists of
`{developer, org}`, each limited to the first 100 entries, plus total counts
`kept_departed_count` and `reseated_unknown_count`. The CLI prints the same
bounded lists and gives the total when a list is truncated.

A membership ended by a move reopens this month. Legacy ends classified as
`unknown` (including closed rows with NULL reason) also reopen this month;
the import reports them as **re-seated** with guidance to end them again if
they left. This includes unclassified legacy closes whose org was cleared.
On upgrade, a closed membership is classified as explicit only when the
person's effective org matches, no alias has an open row in that org, and no
other membership anywhere starts at or after the closed row's start. The
canonical placement wins, falling back to the first assigned alias in name
order; other legacy ends are unknown. This history classification intentionally
differs from the live import's end-month supersession rule.

The whole file is sent as one all-or-nothing request, so it may hold at most
1000 rows and at most 1 MiB once encoded (1000 rows of very long names can
exceed the byte limit). A larger file is refused before anything is sent:
split it into smaller files and import each one, and never put the same person
(under any id or alias) in two files, because a later file would silently
overwrite the earlier one. `--dry-run` checks the file and sends nothing. If an
import fails, the message says whether nothing was written or the outcome is
unknown; re-running the same file is safe either way. `tierd hierarchy --help`
explains every input.

---

## Privacy

**TIER never stores prompt or completion content, and never sends it anywhere
you have not already sent it.** Its JSONL parser is an allowlist: the only fields
deserialized are `type`, `timestamp`, `gitBranch`, `cwd`, `sessionId`, and
`message.{id,model,role,usage.*}` — token counts, not text. The optional reverse
proxy is the one path that handles content: it passes your prompts on to the
provider you chose, and reads each response as it passes back to you to find
`id`/`model`/`usage`. Only those fields are kept.
Unknown JSON fields are dropped. The Codex, Opencode and Muse Code readers work
the same way: they decode model and provider names, timestamps, session, folder and
branch metadata, and token counts, and nothing else. One switch, off by default,
widens the Claude Code read: `--worktree-attribution` (#823) also reads the file or
folder paths in tool calls and uses the one that seven tools name (`Read`, `Edit`,
`MultiEdit`, `Write`, `NotebookEdit`, `Glob`, `Grep`), never file contents or command
lines, and stores no path in an event ([docs/privacy.md](docs/privacy.md#worktree-attribution-off-by-default)).
Parents no longer carry (#1016); only message groups labelled `isSidechain:true` can use carry. Worktree attribution stays opt-in and **off by default**. Activation requires recorded gates, the operator's security rulings and an explicit operator decision. No new accuracy figures have been measured.

What **is** stored, in one SQLite file on the machine that runs `tierd serve`:
developer identifier, issue id, repository name, branch-derived issue refs,
model names, token counts, micro-dollar list costs, the session id of each
captured Claude Code, Codex, Opencode and Muse Code session, and PR metadata
(number, author login, weight, merge SHA). The live watcher's
resume checkpoint also keeps each session file's working-directory path. With
worktree attribution on, each Claude Code event also records the fixed label of
the rule that chose its issue (`attribution_rule`), and the watcher's checkpoint
also keeps the absolute path of the git worktree it is carrying from one message
to the next. If you
enable the GitHub webhook, TIER also keeps each processed delivery's raw body,
which carries commit author names and email addresses, PR titles and
descriptions, and commit messages, for up to 90 days (50,000 rows at most). The
per-developer erase does not rewrite those bodies; they age out, or you delete
them directly (see [docs/privacy.md](docs/privacy.md)). TIER
sends none of this to anyone else. The only network hops are ones you set up:
`tierd ship` sends a laptop's token records to your own server, and the proxy
forwards requests to the provider you chose. Full detail and code grounding:
[docs/privacy.md](docs/privacy.md).

Per-developer measurement can be **restricted by law** in parts of the EU
(German Betriebsrat co-determination, French and Dutch works councils, GDPR
rules on profiling). From v0.6.1, `developer` is synthetic demo only until #826;
real-data startup is refused. Before deploying real data in team or division mode, read
[docs/legal-and-privacy.md](docs/legal-and-privacy.md) — works-council, DPIA, and
team-only vs per-developer deployment guidance (not legal advice).

---

## API reference

All routes are served by `tierd serve`. The **Auth** column uses the five categories below (including `ingest or admin` for event ingestion), plus `HMAC signature` for the GitHub webhook:

- **admin** — requires the write API token (`--api-token` / `TIER_API_TOKEN`).
- **read** — accepts the read-only viewer token (`--read-token` /
  `TIER_READ_TOKEN`) **or** the admin token. The viewer token is a second
  password you generate the same way, kept in its own file; it must differ from
  the admin token ([What you need](#what-you-need-before-you-start)). It is
  rejected on every `admin` route and on the proxies, so a dashboard/BI reader
  cannot write, erase, or read raw invoice/identity data.
- **metrics** — accepts the metrics token (`--metrics-token` /
  `TIER_METRICS_TOKEN`) or the admin token, and, in `developer` mode only, the
  read-only token ([The metrics token](#the-metrics-token-tier_metrics_token---metrics-token--optional)).
- **ingest** — accepts the ingest token (`--ingest-token` / `TIER_INGEST_TOKEN`)
  or the admin token on `POST /api/v1/events` only.
- **open** — no auth (status only, never spend data).

Auth is enforced only when the admin token is configured; on a loopback dev bind with no
token, all routes are reachable.

Request and response bodies for every route are in
[docs/api-compatibility.md](docs/api-compatibility.md#endpoint-catalog); the
GitLab / Bitbucket CI recipe for `POST /outcomes` is in
[docs/outcomes-api.md](docs/outcomes-api.md).

| Method & path | Purpose | Auth |
|---|---|---|
| `POST /api/v1/costs` | ingest a single token-cost event | admin |
| `POST /api/v1/events` | bulk-ingest collector token events (used by `tierd ship`) | ingest or admin |
| `POST /api/v1/outcomes` | record a PR outcome (weight / quality) | admin |
| `POST /api/v1/actual_spend` | post a finance-supplied per-developer invoice total | admin |
| `POST /api/v1/org_actual_spend` | post an org-level invoice total | admin |
| `GET /api/v1/org_actual_spend` | read back recorded org actual-paid spend | admin |
| `GET /api/v1/scores` | all developer scores | read |
| `GET /api/v1/scores/{developer}` | one developer's score | read |
| `GET /api/v1/scores/compare` | two-window comparison, k-anonymized in team/division mode | read |
| `GET /api/v1/issues` | per-repository/issue cost, points and TIER lower bound; developer mode only | read |
| `GET /api/v1/diagnostics/captured-dollars` | captured-dollar join-readiness diagnostics; developer mode only | read |
| `GET /api/v1/report_manifest` | the identity of the report `/scores` returns for the same window: price-table digests, as-of watermarks (row ids and counts), content digests; window counts and digests withheld in team/division mode | read |
| `GET /api/v1/events` | paginated bulk export of raw token events | read |
| `GET /api/v1/outcomes` | paginated bulk export of raw outcomes | read |
| `POST /api/v1/developer_alias` | map an alias to a canonical developer | admin |
| `GET /api/v1/developer_alias` | list alias → canonical map | admin |
| `DELETE /api/v1/developer_alias/{alias}` | remove an alias | admin |
| `PUT /api/v1/org_hierarchy/{developer}` | upsert one developer's team mapping | admin |
| `POST /api/v1/org_hierarchy` | bulk-import the developer → team map | admin |
| `GET /api/v1/org_hierarchy` | read the developer → team map | admin |
| `POST /api/v1/period_membership/{developer}/end` | end a developer's team-membership period | admin |
| `GET /api/v1/developer/{id}/export` | GDPR: export one developer's full record | admin |
| `DELETE /api/v1/developer/{id}` | GDPR: erase one developer's data | admin |
| `GET /api/v1/health` | always `200 {"status":"ok"}` while the process answers | open |
| `GET /api/v1/healthz` | readiness: `503` while a background subsystem (such as the JSONL watcher) is restarting or has stopped with an error | open |
| `GET /api/v1/livez` | liveness probe: version and uptime (**+ commit** from v0.4.1 on) | open |
| `GET /api/v1/version` | build identity (version, commit, platform, price table). First released in **v0.4.1**; v0.4.0 and earlier answer 404 | open |
| `GET /api/v1/quality_events` | raw quality signals behind the outcome weights | read |
| `GET /api/v1/quality_history` | quality signal history over time | read |
| `GET /api/v1/fidelity` | capture fidelity: recorded vs estimated spend | read |
| `GET /metrics` | Prometheus exposition (always mounted by `serve`) | metrics |
| `POST /webhook/github` | GitHub webhook (mounted only when a secret is set) | HMAC signature |
| `GET /` | dashboard page (static HTML + vanilla JS); the page is open, the data it loads needs a read token | open |

The proxy routes (`/anthropic/`, `/openai/`, `/gemini/`) authenticate with the
`X-Tier-Token` header instead of Bearer, and are mounted only when their target
URL is configured (non-empty). `/gemini/` defaults to
`https://generativelanguage.googleapis.com` (`--gemini-target` /
`proxy.gemini_target`) — mounted by default, but
**not yet verified against live Gemini traffic**: see
[docs/how-it-works.md](docs/how-it-works.md)'s ‡ note before relying on it.

---

## Operations

- **Checks (no test/lint CI).** Nothing gates a pull request — build, test and lint all run
  locally via `make check` / `make check-full`. (A scheduled workflow for re-scanning published
  container images is included, but it runs only in the private development repository; in
  the public repository it is present and skipped. `make cve-rescan` runs the same code
  locally. It is not a build or test pipeline.)
  `make check` runs lint + build + fast tests; `make check-full` adds
  integration tests. Run them before pushing.
- **Metrics scrape.** Configure your scraper with an `Authorization: Bearer`
  header carrying the metrics token (`--metrics-token`). In `team` and
  `division` mode the read-only token gets `403` on `GET /metrics`; in
  `developer` mode it still scrapes. The metrics token is operator-only:
  [docs/security.md § The metrics token](docs/security.md#the-metrics-token-944).
  Recipe: [deploy/README.md § 5](deploy/README.md#5-authenticated-prometheus-scrape).
- **Backups.** Use the built-in `tierd backup` — a WAL-aware `VACUUM INTO`
  snapshot. It only reads the database, which SQLite allows while `serve` is
  writing, so unlike `backfill` it can run against a live database:

  ```sh
  # Use the --db path you gave serve if it differs from the default below.
  mkdir -p ~/.tier/backups
  tierd backup --db ~/.tier/tier.db --out ~/.tier/backups/tier-$(date +%F).db
  # Prints the backup path and size in bytes; the date and size vary.
  ```

  `--out` is required and must not already exist. A naïve `cp` of the DB file
  while tierd is running can miss in-flight WAL writes — prefer `tierd backup`,
  or otherwise stop tierd first.
- **Logs.** `--log-format` (`auto`/`json`/`text`, env `TIER_LOG_FORMAT`) and
  `--log-level` (`debug`/`info`/`warn`/`error`, env `TIER_LOG_LEVEL`) control
  structured logging.

---

## Docs

[**docs/README.md**](docs/README.md) is the full documentation index, grouped by
what you are trying to do. A shortlist:

**Start here:**
[**docs/interpreting-the-number.md**](docs/interpreting-the-number.md) -- read
this before you trust a TIER score. It explains why recent or short windows read
low (spend is booked as it happens, the outcome only at merge time), and what
to do about it.

Understanding the measurement:

- [docs/how-it-works.md](docs/how-it-works.md) -- the full measurement model, capture -> attribution -> score.
- [docs/rubric.md](docs/rubric.md) -- the versioned scoring rubric: outcome weights and quality multipliers, and why it deliberately defines no absolute good/bad band.
- [docs/conventions.md](docs/conventions.md) -- size labels and issue-ref formats.
- [docs/how-tier-relates.md](docs/how-tier-relates.md) -- how TIER complements DORA / SPACE / DX Core 4.
- [docs/peer-learning.md](docs/peer-learning.md) -- how a team runs a retrospective on its AI working practices, then tries one change and compares the score before and after.

Operating and integrating:

- [config.example.yaml](config.example.yaml) -- every config key, annotated.
- [docs/webhook-setup.md](docs/webhook-setup.md) -- GitHub webhook configuration.
- [docs/api-compatibility.md](docs/api-compatibility.md) -- the /api/v1 compatibility contract: what is pinned and what may change.
- [docs/security.md](docs/security.md) -- the operator-facing security model and data-handling guide.
- [docs/privacy.md](docs/privacy.md) -- what is stored, where, and what never is.
- [docs/legal-and-privacy.md](docs/legal-and-privacy.md) -- EU labor-law, works-council, GDPR, and DPIA deployment guidance for per-developer measurement.
