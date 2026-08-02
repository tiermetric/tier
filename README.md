# TIER — Token Impact & Efficiency Ratio

![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue)
![Go 1.26.5+](https://img.shields.io/badge/go-1.26.5%2B-00ADD8)
![Status: pre-v1](https://img.shields.io/badge/status-pre--v1-orange)

**Measure what your AI spend produces, not what it burns.**

TIER measures the **yield** of AI-assisted engineering: quality-weighted
outcomes per $1,000 of spend -- dollar value, not token count. Think "DORA for
AI-augmented engineering" -- a number a CFO or VP-Eng can reason about. It grades
the *work*, not the worker, and ships its own
[failure-mode analysis](docs/adversarial-analysis.md) to prove it.

> Measured on TIER's own repository, trailing 30 days to 2026-07-21: **221**
> outcome points per $1,000 across 152 merged PRs, and **72.1%** of spend in that
> window joined no tracked outcome. Measuring yield beats measuring burn.

**Complement, not replacement.** TIER measures one axis — the dollar efficiency
of AI spend — that DORA, SPACE, and DX Core 4 do not. Run it *beside* your
existing metrics framework, never fused into a single performance number. See
[How TIER relates to DORA / SPACE / DX Core 4](docs/how-tier-relates.md).

The core metric:

```
TIER = Σ(weight × quality) / (list-price cost / $1000)
```

- **weight** — the size of a merged outcome (from a PR size label, or a
  lines/files heuristic when no label is set).
- **quality** — 1.0 for a clean merge, reduced when a change is later reverted.
- **list-price cost** — the AI spend attributed to that work, priced from a
  versioned reference price table (list price, not your negotiated invoice).

Two supporting metrics travel alongside it:

- **Coverage %** — the share of attributed cost that came from real-time capture
  (proxy / JSONL) rather than after-the-fact estimation.
- **Spend Leverage** — list-price cost ÷ actually-paid invoice. It surfaces the
  discount your committed spend is buying. Server-only (it needs the
  finance-supplied invoice total); rendered as `—` until that ledger entry
  exists.

---

## ⚠️ Status & scope

TIER is **pre-v1** and **single-tenant**. It runs as a single Go binary
(`tierd`) over a single local SQLite file. There is no clustering, no external
datastore, and no message bus.

> **No tenant isolation exists.** Developer identifiers are global — there is no
> `tenant_id`/org column, so two organizations' `alice` rows would collide.
> **Do not deploy this as a shared multi-org service.**

Run it for one team or one organization at a time.

---

## Prerequisites

TIER builds from source. You need **Go 1.26.5+**, **make**, and **git**. Clone
the repo and build the `tierd` binary once:

```sh
git clone https://github.com/tiermetric/tier.git
cd tier
make build                       # produces ./bin/tierd
```

The commands below assume you are in the repo root with `./bin/tierd` built.

## Three ways in

Pick the one that matches what you want right now:

| Path | You get | Effort |
|---|---|---|
| [See it in 60 seconds](#see-it-in-60-seconds) | A populated dashboard on synthetic data | One command |
| [Score your own repo](#score-your-own-repo) | A real TIER score for your own history | Five commands, two terminals, a GitHub token |
| [Run it for a team](#run-it-for-a-team) | A shared server recording outcomes live | Server or container deployment |

In a hurry? The copy-paste-ready version of everything below — including
installing without cloning (`go install github.com/tiermetric/tier/cmd/tierd@latest`),
capturing Codex, and viewing the dashboard from another machine — is in
**[docs/quickstart.md](docs/quickstart.md)** (served by the running binary at
`/docs/quickstart`).

New to the metric itself? Read
[docs/understanding-tier.md](docs/understanding-tier.md) — what TIER measures
and why, in three layers.

---

## See it in 60 seconds

Want to see what TIER looks like before wiring up your own data? `tierd demo`
seeds an obviously-synthetic dataset and serves the dashboard — no setup, no
Claude Code history required:

```sh
./bin/tierd demo                 # then open http://127.0.0.1:8080
```

The board is unmistakably fake — `demo-*` developers, `DEMO-*` issues, an
`ACME (DEMO)` org, and a `SYNTHETIC DATA` banner in the console — so it can never
be mistaken for real scores. The database is a throwaway file, recreated on every
run.

This is genuinely one command, once `./bin/tierd` is built. The next path is
not.

---

## Score your own repo

**What this costs you:** five commands across two terminals, one mandatory
GitHub token, and three gotchas — each called out at the step where it bites.
(Plus the one-time `make build` under [Prerequisites](#prerequisites).)

### First, a free look at the cost side

No server, no token, no config. `tierd score` reads your local Claude Code
session files under `~/.claude/projects/` and prints where the AI money went for
a given repository:

```sh
./bin/tierd score --repo .       # attribute the last 90 days for this repo
```

Output shape (values will differ):

```
price table: embedded default (version 8, 2026-07-22, 76 models)

TIER Cost Attribution — since 2026-06-25
Source: Claude Code JSONL (real-time, per-request)
───────────────────────────────────────────────────────────────────────────────
Developer              Input tok  Output tok    Cache rd   Cache w5m   Cache w1h    Cost ($)
───────────────────────────────────────────────────────────────────────────────
alice                    1898883     2197887   349279392           0    17615403    496.3612
───────────────────────────────────────────────────────────────────────────────
TOTAL                                                                              496.3612

Cost by Issue
─────────────────────────────────────────────────────
Issue                 Model                   Cost ($)
─────────────────────────────────────────────────────
issue-127             claude-opus-4-8          67.8453
unattributed          claude-opus-4-8         344.5618

Tip: run `tierd serve` to record PR outcomes and compute full TIER scores.
```

This is cost-attribution **only** — it has no PR outcomes, so it is not a TIER
score. Useful flags: `--since 2026-01-01`, `--developer <id>` (default: your OS
username), `--prices <file.yaml>`.

That large `unattributed` bucket is spend from sessions whose branch carried no
recognizable issue reference. See [docs/conventions.md](docs/conventions.md) to
fix it.

### Then, the full score

A TIER score also needs **outcomes** (merged PRs). Both sides can be
reconstructed from history, so you don't have to wait for new activity:

- **Outcomes (the numerator)** — `tierd backfill` walks your repo's merged-PR
  history via the GitHub API and reconstructs one outcome per merged PR. It
  covers the **last 90 days** by default; `--since` reaches further back.
- **Cost (the denominator)** — `tierd ship` forwards the last 90 days of your
  local Claude Code JSONL to the server. (`serve --watch-repo` only tails *new*
  activity; `ship` is how you load the back catalog.) The two 90-day windows
  line up on purpose, so cost and outcomes cover the same period.

```sh
# 1. Reconstruct the last 90 days of outcomes into the default DB (~/.tier/tier.db).
#
#    ⚠️ GOTCHA 1 — a GitHub token is REQUIRED here. Read-only repo access is
#    enough. Prefer TIER_GITHUB_TOKEN or an @/path/to/token file over a literal;
#    a literal leaks via ps and shell history.
#
#    ⚠️ GOTCHA 2 — run backfill BEFORE serve. The store is single-writer SQLite,
#    so the two processes must not both hold the DB file open.
export TIER_GITHUB_TOKEN=@~/.tier/github-token
./bin/tierd backfill --repo your-org/your-repo        # GitHub "owner/name" slug, not a path

# 2. Start the server (see GOTCHA 3 immediately below this block).
./bin/tierd serve --aggregation developer
# tierd listening addr=127.0.0.1:8080

# 3. In a SECOND terminal, ship your last 90 days of cost to it.
./bin/tierd ship --server http://127.0.0.1:8080 --repo /path/to/local/checkout
# Shipped N events ... Re-running is safe: the server dedups on idempotency keys.
```

> ⚠️ **GOTCHA 3 — evaluating solo or on a small team? You must pass
> `--aggregation developer`, or your own row vanishes.** The `team` setting
> applies a k-anonymity floor (default 5, hard minimum 3): a trial with fewer
> contributing developers than the floor folds every named row into a single
> anonymous "other" row, so you will not see your own score. `developer` mode
> keeps your named row.

Open <http://127.0.0.1:8080> — the dashboard now shows a TIER score backed by
real cost **and** real outcomes.

**A brand-new trial** with only a couple of merged PRs, or under ~$5 of
attributed spend, shows a provisional below-threshold row until more data
accrues. Switch to `--aggregation team` before sharing reports across a real org
(see [docs/legal-and-privacy.md](docs/legal-and-privacy.md)).

**One identity caveat.** Cost is attributed to your shipping identity (your OS
username, or `ship --developer <id>`); outcomes to the PR author's GitHub login.
If those differ, map them to one developer so the two sides combine — see
[Identity mapping](#identity-mapping). Otherwise cost and outcomes land in
separate rows and TIER can't pair them.

---

## Run it for a team

The server records PR outcomes (via a GitHub webhook), computes full TIER
scores, and serves a dashboard at `/`.

### Server mode

**Local / loopback (no token needed):**

```sh
./bin/tierd serve --aggregation team --watch-repo ~/src/app
# tierd listening addr=127.0.0.1:8080
curl -s http://127.0.0.1:8080/api/v1/livez
# {"status":"alive","uptime_s":1,"version":"..."}
```

`--aggregation` is **required** — `serve` refuses to start without it (there is
no default, so an existing deployment's reporting mode never changes silently).
Pick `team` to emit only team-level aggregates that never name an individual —
the safe posture under EU works-council / GDPR Art. 22 co-determination — or
`developer` to keep named per-developer rows. It is also settable via
`TIER_AGGREGATION` or the `aggregation` config key. `team` mode enforces a
k-anonymity floor (`--k-anonymity`, default 5, hard minimum 3); see
[docs/legal-and-privacy.md](docs/legal-and-privacy.md) before ranking named
individuals.

`--watch-repo` tails that repo's Claude Code JSONL live. It is repeatable; omit
it to disable live ingestion.

**Production (exposed bind — a token is mandatory):**

```sh
export TIER_API_TOKEN=@/etc/tier/api-token        # read from a file, never a literal
export TIER_WEBHOOK_SECRET=@/etc/tier/webhook-secret
./bin/tierd serve --aggregation team --addr 0.0.0.0:8080 --db /var/lib/tier/tier.db
```

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

docker run --read-only --cap-drop=ALL --security-opt no-new-privileges \
  -p 8080:8080 -e TIER_API_TOKEN=… -v tier-data:/data tierd \
  serve --aggregation team --addr 0.0.0.0:8080 --db /data/tier.db

curl -s http://localhost:8080/api/v1/livez
```

`--read-only` is safe — tierd needs no writable rootfs, only the `/data` volume.
The image runs as non-root uid 65532 on a minimal static base (no shell, no
package manager). Passing the token via `-e TIER_API_TOKEN` keeps it off the
command line; if you mount the secret as a file, the `@/path` indirection form
works inside the container too (`-e TIER_API_TOKEN=@/run/secrets/tier-token`).

> **JSONL watching does not work in Docker** — there is no `~/.claude` inside the
> container. Container mode is API / webhook / proxy only. Feed laptop-captured
> tokens to a containerized server with `tierd ship` (the laptop-shipper
> topology, see [Capturing tokens](#capturing-tokens)) or the reverse proxy.

---

## Capturing tokens

TIER is **JSONL-first**. Two capture paths, used together or separately:

**1. Claude Code session files (default).** `tierd score` reads them directly;
`tierd serve --watch-repo` ingests them live on the same machine.

**2. `tierd ship` — the laptop shipper.** When developers work on their laptops
but the server runs centrally, a small cron/launchd job forwards each laptop's
JSONL to the central server's `POST /api/v1/events`:

```sh
tierd ship --server https://tier.example --repo ~/src/app
# e.g. every 15 minutes:  */15 * * * * tierd ship --server https://tier.example --repo ~/src/app
```

`ship` is stateless and idempotent — every event carries an idempotency key and
the server dedups on it, so re-shipping the same window on every tick is a no-op.
It sends the token as `Authorization: Bearer` (use `TIER_API_TOKEN` or `@file`).

Add `--codex-rollout` to also forward Codex CLI spend from
`~/.codex/sessions/**/rollout-*.jsonl` (off by default, mirroring
`serve --codex-rollout`). Without it a central server counts the laptop's Codex
outcomes but none of its cost, so Codex work reads as free (#492).

**3. Reverse proxy (optional).** Point a provider SDK at tierd and it captures
token usage from the response as it passes through:

```sh
export ANTHROPIC_BASE_URL=http://tier-host:8080/anthropic
# per-request headers: X-Tier-Token: <api token>, and for attribution
#   X-Tier-Developer: <id>   X-Tier-Issue: <issue-id>
```

The proxy extracts only token-usage fields from responses (see
[Privacy](#privacy)), never prompt or completion content.

---

## Recording outcomes

Full TIER scores need PR outcomes, which arrive via the GitHub webhook. See
[docs/webhook-setup.md](docs/webhook-setup.md) for the exact GitHub
configuration.

TIER silently depends on two conventions. Miss them and work becomes
unattributable — spend lands in an `unattributed` bucket and merged PRs record
no outcome. Both are documented in full in
[docs/conventions.md](docs/conventions.md).

**PR size labels** set the outcome weight; with no label a lines/files heuristic
applies:

| Label (case-insensitive) | Weight |
|---|---|
| `size/xs` or `xs` | 0.5 |
| `size/s` or `s` | 1 |
| `size/m` or `m` | 3 |
| `size/l` or `l` | 5 |
| `size/xl` or `xl` | 8 |
| _(none)_ | `min(8, max(0.5, ceil(log2(lines + files×10 + 1))))` |

**Issue references** attribute a branch / PR to an issue:

| Where | Format | Example | Resolves to |
|---|---|---|---|
| branch | `<prefix>/<N>-<slug>` — trailing `-`/`_` optional; `N` has no leading zero | `feature/42-auth`, `feature/42` | `issue-42` |
| branch | tracker key `[A-Z][A-Z0-9]+-<N>` | `fix/TIER-99-crash` | `TIER-99` |
| PR body / commit | `closes #N`, `fixes #N`, `resolves #N` (case-insensitive) | `closes #42` | `issue-42` |
| PR body / commit | whitespace-preceded `#N` | `see #42` | `issue-42` |

Non-matches: `## heading`, hex colors like `#FF0000`, and branches named
`main`/`master`/`HEAD`. A bare 4-digit segment in the year band `1900`–`2099`
(e.g. `release/2024-fix`) is read as a calendar year, not an issue — disambiguate
with a tracker key or a PR-body `#N`. See
[docs/conventions.md](docs/conventions.md) for the full rules and the year-guard
tradeoff.

---

## Identity mapping

A developer's JSONL is attributed to their **OS username**, but PR outcomes are
attributed to their **GitHub login**. When these differ, map the alias to the
canonical identifier so spend and outcomes join:

```sh
curl -X POST https://tier.example/api/v1/developer_alias \
  -H "Authorization: Bearer $TIER_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"alias":"alice-laptop","canonical":"alice"}'
# 201 Created
```

`GET /api/v1/developer_alias` lists the current map;
`DELETE /api/v1/developer_alias/{alias}` removes one. All three require the
**admin** (write) token — an alias edit retroactively re-joins spend to
outcomes, so it is an administrative operation, not a read-scope one.

---

## Privacy

**TIER never reads, transmits, or stores prompt or completion content.** Its
JSONL parser is an allowlist: the only fields deserialized are `type`,
`timestamp`, `gitBranch`, `cwd`, `sessionId`, and `message.{id,model,role,
usage.*}` — token counts, not text — and the proxy parsers extract only
`id`/`model`/`usage` from response bodies. Unknown JSON fields are dropped.

What **is** stored, in a single local SQLite file with no external transmission:
developer identifier, issue id, branch-derived issue refs, model names, token
counts, micro-dollar list costs, and PR metadata (number, author login, weight,
merge SHA). Full detail and code grounding: [docs/privacy.md](docs/privacy.md).

Per-developer measurement is **legally constrained** in TIER's EU target markets
(DE Betriebsrat co-determination, FR/NL works councils, GDPR profiling). Before
deploying in a way that ranks named individuals, read
[docs/legal-and-privacy.md](docs/legal-and-privacy.md) — works-council, DPIA, and
team-only vs per-developer deployment guidance (not legal advice).

---

## API reference

All routes are served by `tierd serve`. The **Auth** column has three values:

- **admin** — requires the write API token (`--api-token` / `TIER_API_TOKEN`).
- **read** — accepts the read-only viewer token (`--read-token` /
  `TIER_READ_TOKEN`, #190) **or** the admin token. The viewer token is rejected
  on every `admin` route and on the proxies, so a dashboard/BI reader cannot
  write, erase, or read raw invoice/identity data.
- **open** — no auth (status only, never spend data).

Auth is enforced only when a token is configured; on a loopback dev bind with no
token, all routes are reachable.

| Method & path | Purpose | Auth |
|---|---|---|
| `POST /api/v1/costs` | ingest a single token-cost event | admin |
| `POST /api/v1/events` | bulk-ingest collector token events (used by `tierd ship`) | admin |
| `POST /api/v1/outcomes` | record a PR outcome (weight / quality) | admin |
| `POST /api/v1/actual_spend` | post a finance-supplied per-developer invoice total | admin |
| `POST /api/v1/org_actual_spend` | post an org-level invoice total | admin |
| `GET /api/v1/org_actual_spend` | read back recorded org actual-paid spend | admin |
| `GET /api/v1/scores` | all developer scores | read |
| `GET /api/v1/scores/{developer}` | one developer's score | read |
| `GET /api/v1/events` | paginated bulk export of raw token events (#191) | read |
| `GET /api/v1/outcomes` | paginated bulk export of raw outcomes (#191) | read |
| `POST /api/v1/developer_alias` | map an alias to a canonical developer | admin |
| `GET /api/v1/developer_alias` | list alias → canonical map | admin |
| `DELETE /api/v1/developer_alias/{alias}` | remove an alias | admin |
| `PUT /api/v1/org_hierarchy/{developer}` | upsert one developer's team mapping (#232) | admin |
| `POST /api/v1/org_hierarchy` | bulk-import the developer → team map (#232) | admin |
| `GET /api/v1/org_hierarchy` | read the developer → team map (#232) | admin |
| `POST /api/v1/period_membership/{developer}/end` | end a developer's team-membership period (#232) | admin |
| `GET /api/v1/developer/{id}/export` | GDPR: export one developer's full record (#184) | admin |
| `DELETE /api/v1/developer/{id}` | GDPR: erase one developer's data (#184) | admin |
| `GET /api/v1/health` | liveness/status | open |
| `GET /api/v1/healthz` | readiness (watcher subsystem state) | open |
| `GET /api/v1/livez` | liveness (version + uptime) | open |
| `GET /metrics` | Prometheus exposition (mounted when metrics are wired) | read |
| `POST /webhook/github` | GitHub webhook (mounted only when a secret is set) | HMAC signature |
| `GET /` | dashboard (static HTML + vanilla JS) | open |

The proxy routes (`/anthropic/`, `/openai/`) authenticate with the
`X-Tier-Token` header instead of Bearer, and are mounted only when their target
URL is configured (non-empty).

---

## Operations

- **Checks (no test/lint CI).** Nothing gates a pull request — build, test and lint all run
  locally via `make check` / `make check-full`. (A scheduled workflow for re-scanning published
  container images is included, but it is guarded to the development repository and does not
  run here; `make cve-rescan` runs the same code locally. It is not a build or test pipeline.)
  `make check` runs lint + build + fast tests; `make check-full` adds
  integration tests. Run them before pushing.
- **Metrics scrape.** `GET /metrics` is **read**-scoped — configure your scraper
  with an `Authorization: Bearer` header carrying the read-only viewer token
  (`--read-token`) or the admin token. Its labels carry operational signal about
  a single-tenant deployment's spend activity, so it is not world-readable.
- **Backups.** Use the built-in `tierd backup` — a WAL-aware `VACUUM INTO`
  snapshot that is safe to run against a live database:

  ```sh
  tierd backup --db /var/lib/tier/tier.db --out /backup/tier-$(date +%F).db
  # backup written: /backup/tier-2026-07-12.db (1234567 bytes)
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
what you are trying to do. The shortlist below is the front door.

**Start here:**
[**docs/interpreting-the-number.md**](docs/interpreting-the-number.md) -- read
this before you trust a TIER score. The honest-reading guide: why recent or
short windows read artificially low (the windowing skew -- spend is booked up
front, the outcome lands at merge time), and how to read the number honestly.

Understanding the measurement:

- [docs/how-it-works.md](docs/how-it-works.md) -- the full measurement model, capture -> attribution -> score.
- [docs/rubric.md](docs/rubric.md) -- the versioned scoring rubric: outcome weights and quality multipliers, and why it deliberately defines no absolute good/bad band.
- [docs/conventions.md](docs/conventions.md) -- size labels and issue-ref formats.
- [docs/how-tier-relates.md](docs/how-tier-relates.md) -- how TIER complements DORA / SPACE / DX Core 4.
- [docs/peer-learning.md](docs/peer-learning.md) -- run a team levers retro and a before/after practice experiment on shipped surfaces; move practices, not scores.

Operating and integrating:

- [config.example.yaml](config.example.yaml) -- every config key, annotated.
- [docs/webhook-setup.md](docs/webhook-setup.md) -- GitHub webhook configuration.
- [docs/api-compatibility.md](docs/api-compatibility.md) -- the /api/v1 compatibility contract: what is pinned and what may change.
- [docs/security.md](docs/security.md) -- the operator-facing security model and data-handling guide.
- [docs/privacy.md](docs/privacy.md) -- what is stored, where, and what never is.
- [docs/legal-and-privacy.md](docs/legal-and-privacy.md) -- EU labor-law, works-council, GDPR, and DPIA deployment guidance for per-developer measurement.
