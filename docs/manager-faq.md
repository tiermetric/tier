# TIER for engineering managers: common questions

TIER (Token Impact & Efficiency Ratio) measures how much merged work a team gets
for its AI spend. This page answers the questions engineering managers ask about
it, with the command for each answer. It assumes a TIER server is already
running; installing one, and getting spend and merged work into it, are covered
in the project [README](../README.md). For what the number itself means, start
with [understanding-tier.md](understanding-tier.md).

## What do I need before I run these commands?

**v0.6.2 availability:** Per-developer views and the per-issue report are not available on real data until #826 ships; `tierd demo` shows them read-only using its own invented data in a private temporary database and cannot load yours.

Three values. Ask the person who runs your TIER server for them.

- **The server address.** The web address your TIER server answers on, for
  example `https://tier.example.com`. If TIER runs on your own laptop, it is
  `http://127.0.0.1:8080`. Open that address in a browser to see the dashboard.
  Set it once in each terminal window you use:

  ```sh
  export TIER_HOST=https://tier.example.com   # your server's address, no trailing slash
  ```

- **The admin token.** A long random password that the person who runs the
  server chose when they started it (the `--api-token` flag or the
  `TIER_API_TOKEN` environment variable). You need it to load teams and identity
  mappings and to read those lists back. It also works for everything the
  read-only token does.
- **The read-only token.** An optional second password (`--read-token` or
  `TIER_READ_TOKEN`). It can read scores and open the dashboard. Raw-record
  access will use #826's named-viewer controls, not the shared read-only token
  (see [Can I see my developers' names?](#can-i-see-my-developers-names)). The server refuses it for every admin request, including loading
  teams and reading the team list back. If your server has no read-only token,
  use the admin token wherever this page reads `~/.tier/read-token`.

Save each token in a file that only you can read. These commands work in bash
or zsh. The `read` line asks for the token without showing it on screen or
saving it in your shell history: it waits with nothing on screen while you
paste, then you press Enter:

```sh
mkdir -p ~/.tier && chmod 700 ~/.tier
( umask 077; read -rs t; printf '%s\n' "$t" > ~/.tier/api-token )
chmod 600 ~/.tier/api-token
```

Do the same for the read-only token, saving it as `~/.tier/read-token`.

On the dashboard, if the server needs a token, a password box appears next to
the Refresh button. Paste the read-only token there and click Refresh. The page
keeps it until you close the browser tab.

The examples pass the token to `curl` through a pipe (`printf … | curl -H @-`)
rather than on the command line, because on most systems anyone else logged in
to the same machine can see a program's command line in the list of running
programs (`ps`). `printf` is built into bash and zsh, so it never shows up in
that list. With sign-in turned off, `tierd serve` is restricted to loopback IP
addresses (such as `127.0.0.1` or `::1`, without a zone suffix) or the exact
hostname `localhost`; any other address needs an admin token. If a request with
no token works instead of failing with HTTP 401, sign-in is off: drop the
`printf` line and the `-H @-`.

Every `curl` here fails on an HTTP error (`-f`, or `--fail-with-body` where
the error text matters). When the server answers with an error, such as 401
for a wrong token or 400 for a bad request, curl prints
`The requested URL returned error: 401` and passes nothing on to `jq`, so an
error cannot be read as an empty answer. `-f` also drops the server's own
explanation of the error. To see it, keep the `printf` line, run the `curl`
part with `-sS --fail-with-body` in place of `-fsS` (curl 7.76 or later), and
leave off the `| jq …` stage: `jq` would swallow the explanation, and a wrong
token can then print something that looks like a real, empty answer.

The examples also use `jq`, a free tool that picks fields out of JSON
(`brew install jq` on a Mac, `sudo apt install jq` on Debian or Ubuntu). The
full setup is in the project README under
[What you need before you start](../README.md#what-you-need-before-you-start).

## How do I pilot TIER on my team?

Start with one question: **which tools do the engineers use?** TIER can read
local usage records from the **Claude Code, Codex CLI, Opencode and Meta Muse
Code CLIs** on each laptop. Claude Code, Codex and Muse write session logs;
Opencode keeps its per-call records in a local SQLite store. These readers do
not capture Claude usage inside **Cursor, Copilot or the claude.ai website**.
New in v0.6.2, two **experimental**, separately configured server pollers cover
GitHub Copilot organisation AI-credit gross spend (`collectors.copilot_billing`)
and usage on a Cursor team plan with Admin API access (Teams or Enterprise;
Cursor decides which plans include it) (`collectors.cursor_admin`). Copilot
needs
an installed GitHub App with **Administration: Read-only** (preferred), or an
org-admin token; Cursor needs a team Admin API key. Individual Copilot Pro and
individual Cursor accounts are not covered by these pollers; ChatGPT Team
remains uncaptured. Copilot maps GitHub logins to developers; Cursor maps emails
through aliases and never stores those emails. Neither poller links spend to
a PR or issue. Cursor skips BYO-key requests and their Cursor fee; provider
spend is captured by `anthropic_admin` or `openai_usage` only when the matching
poller is configured for the billed organisation. See the
[source requirements and pricing limits](technical-reference.md#what-each-source-needs).
Missing usage can make the ratio read high when some cost is recorded.
Your Anthropic account's billed totals per engineer cannot, on their own, be
split by PR, issue or feature: they contain no work linkage.

Plan the first report around a **full calendar month**. Recovery works only
while the source files still exist. Claude Code deletes a session file after
`cleanupPeriodDays`, a Claude Code setting that is 30 days unless you change it,
so check yours ([Claude Code data retention](https://code.claude.com/docs/en/data-usage#data-retention)). A first pilot
with default Claude Code retention can usually recover only about a month of
cost; the other tools depend on their own retained files. `ship` and `backfill`
default to 90 days, which does not recover deleted logs. Set the same recent
`--since` when loading costs and merged work, and check the report's cost-capture
horizon; reading a team report requires a sealed calendar month (step 6).

**v0.6.2 is for a trusted team.** Every laptop gets one shared, send-only ingest
token: it opens only `POST /api/v1/events`. The server does not yet bind a laptop
to one developer; a holder can post, inflate or suppress spend under any
developer id (#1145, gated on #826; [trust limits](security.md#the-ingest-token-1144)). Real-data
`--aggregation developer` startup is refused with exit 1 until #826's named-viewer,
audit and retention controls ship (#1253). Team mode has no per-issue view.
Per-developer views and the per-issue report are not available on real data in v0.6.2 (#826); to see what they look like, run the synthetic demo (`tierd demo`), which generates its own invented data in a private temporary database and cannot load yours. TIER is
built for coaching, not for evaluating developers. Per-person figures are not
accurate enough to judge an individual. Agree to coach the practice, never rank
people ([peer learning](peer-learning.md#coaching-not-appraisal)).

Install `tierd` via [quickstart](quickstart.md#1-install). On the server, create three different
tokens, one per file, with the README recipes: `(umask 077; openssl rand -hex 32 > ~/.tier/api-token)`,
then the same for `read-token` and `ingest-token` (`serve` refuses two equal tokens). Replace the example inputs.

1. **Start one reachable server.** Inputs: the three token files above (the
   [README ingest-token recipe](../README.md#the-ingest-token-tier_ingest_token---ingest-token--recommended-for-shippers)
   explains the send-only one); set `TIER_HOST` to the HTTPS address you will put in front of this server.

   ```sh
   tierd serve --aggregation team --addr 127.0.0.1:8080 --db "$HOME/.tier/tier.db" \
     --api-token @"$HOME/.tier/api-token" --read-token @"$HOME/.tier/read-token" \
     --ingest-token @"$HOME/.tier/ingest-token" --trusted-proxy-cidr 127.0.0.1/32
   ```

   `serve` speaks plain HTTP, so run an HTTPS reverse proxy on the same machine and make
   it the only way in; the loopback bind keeps the plain listener off the network.
   `--trusted-proxy-cidr` lets the failed-login lockout see each client's own address
   instead of the proxy's, so one laptop with a wrong token cannot lock out the team.

2. **Load the team roster.** Export a CSV with header
   `developer,team,division,org`, one row per engineer. Use the same GitHub login
   for `developer` as in backfill and shipping; `division` and `org` may be empty.
   Import through the running server with the admin token:

   ```sh
   tierd hierarchy import --server "$TIER_HOST" \
     --api-token @"$HOME/.tier/api-token" roster.csv
   ```

   A group is shown only when it has at least k counted people (default 5,
   minimum 3) and every figure it would publish (captured spend, merged-work
   points and, where present, paid spend and capture shares) is carried by at
   least k of them. They need not be the same people for each figure. Otherwise
   the group folds into `other`; if `other` also falls short, the response is
   withheld. The roster must be loaded before sealing; a sealed report does not
   change after an import.

3. **Load merged PRs.** Inputs: create a GitHub token in **GitHub Settings →
   Developer settings → Personal access tokens → Fine-grained tokens**, scoped to
   the repository with **Pull requests: Read-only**, and save it without echoing:

   ```sh
   (umask 077; read -rs t; printf '%s\n' "$t" > ~/.tier/github-token); chmod 600 ~/.tier/github-token
   tierd backfill --repo acme/app --since 2026-09-17 \
     --token @"$HOME/.tier/github-token" --db "$HOME/.tier/tier.db"
   ```

   Run this on the server machine, stopping `serve` first and restarting it after:
   backfill writes the same database directly. Repeat for each repository. For
   live merges instead, follow [webhook setup](webhook-setup.md): configure the
   shared secret with `serve --webhook-secret @file` and send GitHub deliveries
   to `POST /webhook/github`. Branch names or PR bodies must name the issue
   ([conventions](conventions.md)); issue ids come from branch and PR text and
   can be misfiled ([technical reference §5.1](technical-reference.md#51-the-shared-key-is-the-issue-id)).

4. **Ship each laptop's usage.** Inputs: the operator provisions the same ingest
   token as `~/.tier/ingest-token` on each laptop; `TIER_HOST` is the server's HTTPS
   base address, `~/src/app` is the checkout, and `jsmith` is the engineer's GitHub
   login so spend joins their merged PRs.

   ```sh
   tierd ship --server "$TIER_HOST" --repo "$HOME/src/app" --since 2026-09-17 \
     --developer jsmith --api-token @"$HOME/.tier/ingest-token"
   ```

   Claude Code is read by default. Add `--codex-rollout`, `--opencode` or `--muse`
   only for tools that engineer uses; those collectors are off by default.
   Repeat `--repo` for more checkouts. On forks, set the upstream identity with
   `--repo-slug "$HOME/src/app=acme/app"`. Opencode currently captures only the
   Z.ai coding plan; it records no branch, so its spend remains unattributed to
   issues. Run every 15 minutes via the [documented cron/launchd setup](../README.md#capturing-tokens),
   preserving these flags; move a fixed `--since` forward over time. Re-sends
   deduplicate. Check exit codes and the per-repository shipping summaries.

5. **Optionally record billed totals.** Inputs: the account's actual billed USD
   per engineer and calendar month, using the same developer id as above. This
   is a manual admin POST ([API reference](api-compatibility.md#post-apiv1actual_spend)):

   ```sh
   printf 'Authorization: Bearer %s\n' "$(cat ~/.tier/api-token)" |
     curl -fsS -X POST -H @- -H 'Content-Type: application/json' \
       -d '{"developer":"jsmith","period":"2026-09","actual_paid_usd":100}' \
       "$TIER_HOST/api/v1/actual_spend"
   ```

   Posts add together; do not re-post a bill. Spend Leverage is captured list-price cost
   divided by actual paid spend; use whole calendar months for this cross-check.
   It does not allocate the bill to issues or prove completeness.

6. **Seal the first calendar month.** Results come from a **SEALED calendar
   month**; a month returns `404` until it is sealed. Finish loading cost, merged
   work, roster and any identity aliases before arming: the first seal pins the
   earliest published month permanently, and sealed reports never change.
   Leave `serve` running to register its sources, then on the server preview
   and confirm with the same database, configuration and environment:

   ```sh
   tierd seal --arm earliest --dry-run --aggregation team --db "$HOME/.tier/tier.db"
   tierd seal --arm earliest --aggregation team --db "$HOME/.tier/tier.db"
   ```

   The second command prompts for confirmation. The first readable month is the
   first full UTC calendar month of cost coverage, after it closes plus the
   default 14-day grace (`--report-grace`), once configured sources have settled
   through its end and the seal succeeds. With cost coverage starting
   2026-09-17, October is the first full month; it becomes sealable on
   **2026-11-15 at 00:00 UTC**, not when September's backfill finishes. If the
   retained cost horizon starts later, so does the first full month. With no
   eligible history, wait for a full covered month and its grace or backfill
   available history. See [the sealing procedure](quickstart.md#7-viewing-the-dashboard-from-another-machine)
   for source gates, custom settings and sealing status. Later eligible months
   seal at writable-server startup and on its hourly pass.

Then read the **team report for a sealed month**, using the read token
(replace `2026-10` with the month actually sealed):

```sh
printf 'Authorization: Bearer %s\n' "$(cat ~/.tier/read-token)" |
  curl -fsS -H @- "$TIER_HOST/api/v1/scores?period=2026-10"
```

The report gives group cost at list prices, weighted points and TIER (points
per $1,000), subject to the k floor above. Open `TIER_HOST` for the dashboard,
enter the read token and select the same **Sealed month**.
**Capture Fidelity is not completeness**: it measures how captured spend arrived,
and says nothing about usage TIER never saw.

## Can I see my developers' names?

**v0.6.2 availability:** Per-developer views and the per-issue report are not available on real data until #826 ships; `tierd demo` shows them read-only using its own invented data in a private temporary database and cannot load yours.

No, not on real data in v0.6.2. A real-data server runs `team` or `division`;
`tierd serve --aggregation developer` exits 1, with no bypass. Developer mode
is planned with #826's named-viewer, audit and retention controls and can be
previewed only in the synthetic demo (`tierd demo`). `tierd serve` requires
`--aggregation`, with no default
([quickstart.md](quickstart.md#5-the-full-tier-score)):

| Mode | What reports show |
|---|---|
| `developer` | Planned with #826; preview only in the synthetic demo. Named rows on the dashboard and in `GET /api/v1/scores`, subject to the planned viewer controls below. |
| `team` | Teams only. No individual appears in any report. |
| `division` | Divisions only, one level above teams. No individual appears. |

When developer mode ships (#826), named-viewer controls will determine who sees
which developer's rows; the shared read-only token will keep only name-free
reports. In v0.6.2 team and division mode, the read-only token cannot be limited
to one team: one server holds one set of data for everyone it records.
The synthetic demo currently shows every invented developer's row.
`GET /api/v1/events` exposes the raw records behind those rows: each recorded AI
message's time, model, repository, issue and session id, per developer. Real-data
access to these records will need #826's named-viewer controls, not the shared
read-only token. In v0.6.2 team and division mode, `/api/v1/events` returns
HTTP 403. TIER is built for coaching, not for evaluating developers. Per-person
figures are not accurate enough to judge an individual. TIER cannot enforce
that stance; your policy or works agreement (a written agreement with your
works council or other employee representatives) has to support it (see
[How do I use this to help my team get better?](#how-do-i-use-this-to-help-my-team-get-better)).

**Planned with #826, not yet built.** Developer mode will use named-viewer
access rather than one shared read token. Each developer's page will open only
to that developer, their declared manager and, if the operator turns it on, a skip-level manager, and the
developer will be able to read the log of who viewed it. The shared token will
keep only the pages with no names on them. Per-developer rows will be kept for
90 days by default, and an operator will be able to raise that to at most 365
days, which covers an annual coaching cycle. Company history older than that
will live in a roll-up with no names in it. Each developer's page will show the
levers (the cost figures a team can change, listed in the last section of this
page) first and the TIER number last. None of this exists today, and the design
may change before it ships. Today TIER deletes no score data automatically:
per-developer spend and merged-work rows stay until an admin erases that person.
The one exception is raw GitHub webhook bodies (if the webhook is on), which can
include a contributor's name or email address. They are deleted automatically
once they are about 90 days old, or sooner if more than 50,000 are stored, and
an erase does not rewrite them
([privacy.md](privacy.md#data-subject-rights-access-and-erasure-gdpr-art-15--art-17)).

In `team` and `division` mode there is a minimum group size, called *k*: the
fewest people a group must have before TIER shows it by name. *k* is set with
`--k-anonymity`; the default is 5 and the server refuses anything below 3.
TIER counts people, not identifiers (#856). An identifier with spend or merged
work in the window you asked for counts toward *k* only if all of these hold:

- it is on the roster (loaded with `tierd hierarchy import`,
  `POST /api/v1/org_hierarchy` or `PUT /api/v1/org_hierarchy/{developer}`)
  at some point during the window, after aliases
  are joined (next section), so two identifiers of one person count once;
- it is not a bot: GitHub's account type `Bot` on a pull request the webhook
  recorded in the window, a login ending in `[bot]`, or a known bot login such
  as `Copilot` or `dependabot`;
- it has *captured* activity in the window: spend a collector or the proxy
  recorded, or merged work from a pull request. Spend entered by hand through
  `POST /api/v1/costs` and merged work captured only from direct pushes do not
  count on their own.

Spend recorded under `unattributed` is left out of every figure (see below).
Any other identifier that does not count keeps its spend and merged work in
its group's figures; it just does not make the group bigger. On top of that, each figure needs its own *k*: a group's
cost, when non-zero, is shown only if at least *k* counted people in it have captured spend
(hand-entered spend does not count here either), its merged-work points, when non-zero, only if
at least *k* have merged work, and its paid spend (`actual_paid_usd`), when non-zero, only if at
least *k* have paid spend. So five people where only one has spend, or only one
has an invoice, is not shown. `tier` and `cost_per_point` need cost and points;
`spend_leverage` needs cost and paid spend. `coverage_pct` splits cost into
per-request and daily or hand-entered parts, and each part that is not zero
needs *k* counted people with captured spend behind it.

This means more groups are withheld than before #856. The response says how many
identifiers did not count, and why, in `data_quality.uncounted_active_ids`
(`manual_only`, `push_only`, `not_on_roster`, `bot`), once for the whole window.
To bring those people back into the count, capture their work through a
collector or the proxy and put them on the roster. This protects what someone
holding the read-only token can see. Anyone holding the admin token can still
edit the roster and aliases, so treat that token as an admin credential (#908).
A group smaller than *k* is folded into a row called `other`. That row also holds
everyone with no team (in `division` mode, no division) and any team you named
`other`, and it does not say which is which. If `other` is itself smaller than
*k*, TIER withholds the whole response: every team row, `other` and the company
`total`. It says so in `data_quality.kanon_suppressed`, an object that gives how
many counted people were withheld and the *k* in force. A group can be withheld
with *k* or more people in it when one of its figures is carried by fewer than *k*
of them. The named rows go too because another view that folds one of those teams
into its own `other` row (a period comparison does) would let anyone subtract the
team's row and read the hidden group's numbers. In these modes each report is
one calendar month (`?period=YYYY-MM`), and whether a month's rows are shown is
decided once, when the month is sealed.
They are shown if `other` then holds at least *k* counted people (people with
no team, or in a team too small for its own row) and every figure it shows is
carried by at least *k* of them, or if no identifier in it has any spend or
merged work. Until it is sealed the month is not served at all (a `404`,
carrying `sealable_at` when the month will become sealable); once sealed it
never changes, so a withheld month stays
withheld, and there is no wider window to ask for (#913; see the
[API changelog](api-compatibility.md#api-changelog)). Putting the people in
`other` on teams does not help unless each of those teams reaches *k* on its
own.

In these modes the response shows **one breakdown** of the window (#864): the
team (or division) rows and the company `total`. It leaves out the per-work-type
`work_types` view, the spend breakdown `cost_composition` (by model, by token
type, linked and unlinked), and the attribution figures `unattributed_buckets`
and `exploratory_cost_share`, and it refuses `?work_type=`. It does not show
`attributed_cost_share`: beside the team rows and the total it could reveal one
person's spend. Spend TIER cannot tie to any person (an org-usage poller's
remainder, proxy requests with no developer header) is left out of every figure
in these modes; see `excludes_unattributed_spend` in
[api-compatibility.md](api-compatibility.md#get-apiv1scores). A second breakdown can be subtracted from the first: team totals
minus work-type totals recovered a 3-person group's spend in testing, and a model
only one developer used showed that developer's spend in the model breakdown.
Issue #937 tracks an audit that would let those views come back. The per-developer endpoint
`GET /api/v1/scores/{developer}` answers not found (HTTP 404) for every name in
these modes. In the synthetic demo it returns that invented developer's score;
real-data per-developer views are planned with #826.

Every mode stores per-developer identifiers in the database. When developer
mode ships (#826), its displayed "name" will be an identifier, not a person's
real name. On the spend side it is the developer's OS username (what `whoami` prints on their
machine), unless they send spend with `tierd ship --developer <name>`. Two
people with the same username on different machines would share one
identifier, so give one of them a different `--developer`. On the merged-work
side it is the GitHub login of the pull request's author. When those differ,
map them once (next section) so each person is counted once in group reports
and will appear as one row when developer mode ships (#826).

To see which mode a server is running:

```sh
printf 'Authorization: Bearer %s\n' "$(cat ~/.tier/read-token)" |
  curl -fsS -H @- "$TIER_HOST/api/v1/scores" |
  jq '{error, aggregation, developers: (.developers | length)}'
```

An `error` that is not `null` means the request failed; read it before the
other two fields. On a v0.6.2 real-data server, expect `"team"` or `"division"`.
`"aggregation": null` is the developer-mode response shape seen in the synthetic
demo, not a real-data setting you can enable. In that demo, a `developers` count
of 0 means no invented data in the API's default 90-day window. `"team"` or `"division"` means
names are off; in those modes the `developers` count is always 0, because no
individual rows are sent. If the command prints nothing, read curl's error
line: the request failed. On a `team` or `division` server a 404 there means no
month is sealed yet (or sealing is not armed); run it with `--fail-with-body` in
place of `-f` (`curl -sS --fail-with-body`) and `jq` prints the 404's `error`
and `aggregation`.

Team mode exists for organisations where employee-representation rules, such as
works councils in Germany, France and the Netherlands, or data-protection law
such as GDPR, may restrict measuring named employees.
[legal-and-privacy.md](legal-and-privacy.md#deployment-decision-table-team-only-vs-per-developer-by-jurisdiction)
sets out that reasoning, a table of recommended modes by country, and the
consultation steps before go-live. It is not legal advice, and neither is this
page. Two things to know before you decide:

- Team mode removes names from reports. The database still stores
  per-developer identifiers, and admin requests such as
  `GET /api/v1/org_hierarchy` still list them.
- Changing between `team` and `division` means restarting the server with a
  different flag; both keep the roster, *k* floor and sealed-month rules.
  Restarting with `--aggregation developer` is refused on real data in v0.6.2.
  Any move to the developer mode planned with #826 will need consultation with
  the people it covers first; the steps are in the same document.

## Can I keep names and teams in a file and load them?

You can keep teams in a spreadsheet and load them as a CSV file. There is no
field for a real name: TIER stores only the identifiers it is given, so keep the
mapping from login to real name in your HR system or your own spreadsheet. (If
someone's username is their real name, such as `jane.smith`, that is what TIER
stores.)

What exists today (all of these need the admin token):

| Request | What it does |
|---|---|
| `POST /api/v1/developer_alias` | Tells TIER that two identifiers are the same person. |
| `GET /api/v1/developer_alias` | Lists every alias. |
| `DELETE /api/v1/developer_alias/{alias}` | Removes one alias. |
| `POST /api/v1/org_hierarchy` | Loads a list of developer → team, division, org in one call. `tierd hierarchy import` reads a CSV file and sends its rows here as JSON. |
| `PUT /api/v1/org_hierarchy/{developer}` | Adds or changes one developer. |
| `GET /api/v1/org_hierarchy` | Returns everything loaded so far. |

**Step 1: join each person's two identities.** `alias` is the identifier on the
spend side: the OS username, which the developer can get by running `whoami` on
their machine. `canonical` is their GitHub login, shown on their GitHub profile
page. Do this once for each person whose two identifiers differ:

```sh
printf 'Authorization: Bearer %s\n' "$(cat ~/.tier/api-token)" |
  curl -fsS -X POST -H @- -H "Content-Type: application/json" \
    -d '{"alias":"jane-laptop","canonical":"jsmith"}' \
    "$TIER_HOST/api/v1/developer_alias"
# prints nothing when it works (HTTP 201)
```

One GitHub login can have several aliases, for example one per laptop; send one
request for each. An alias applies to spend recorded before you added it when
TIER recomputes a score or creates a new seal, because it joins identities when
computing scores. Already-sealed team/division months stay as sealed: they return
stored results and are not recomputed. An alias does not change which team that
earlier spend counts toward (see below).

If you skip this, that person shows up as two identifiers: one with spend and no
merged work, the other with merged work and no spend, and neither score means
anything. `GET /api/v1/scores` lists identifiers found on only one side under
`data_quality.unjoined_developers` (team and division reports show only counts;
the synthetic demo shows invented identifiers). The per-identifier view is
planned with #826. Missing merged work may simply mean the person has not merged
anything in the window yet, so check your source records before adding an alias.

**Step 2: write the team file.** Make a spreadsheet with this header row and one
row per developer, then export it as CSV (Google Sheets: File > Download > CSV;
Excel: Save As > CSV UTF-8):

```csv
developer,team,division,org
jsmith,platform,infrastructure,acme
bdiaz,payments,product,acme
```

- `developer` is the canonical identifier, the GitHub login from step 1. An
  alias also works; the server resolves it when you load.
- `team` is required. `division` is optional and is what `division` mode groups
  by; in that mode, anyone with no division is grouped into `other`. `org` is
  optional; it matters only if finance records an org-level invoice (what the
  organisation paid a provider, sent to `POST /api/v1/org_actual_spend`; see the
  README's [API reference](../README.md#api-reference)), and then it must match
  that invoice's `org` exactly. An empty cell is the same as leaving the field
  out: on re-import, it closes the person's open org memberships at the current
  UTC month, so those seats receive no org-level invoice allocation for later
  months. Keep the existing `org` when updating a team unless you mean to end
  the membership.
- Each `developer`, `team`, `division`, and `org` value can be up to 256 UTF-8
  bytes after leading and trailing whitespace is trimmed from CSV cells. Names
  are case-sensitive, so `Platform` and `platform` are two different teams.
- The header must contain these four columns, in any order, with an optional
  `rejoin` column. `true` re-enrols the person in the org from this UTC month
  after an explicit departure. `false` or an empty cell keeps an explicit
  departure in place, unless a later membership started at or after it ended;
  then the import reopens the org as usual. `rejoin` accepts `true`, `false`, or
  an empty cell, case-insensitively; any other value is refused. Any other
  column is refused, so a name column cannot slip in.

**Step 3: load it.** `tierd hierarchy import` reads the CSV, checks it, and sends
it to the server. It needs the `tierd` program on your machine (see the README)
and the admin token:

```sh
TIER_API_TOKEN=@$HOME/.tier/api-token \
  tierd hierarchy import --server "$TIER_HOST" teams.csv
# hierarchy import: 2 rows written
```

Add `--dry-run` to check the file without sending it. The load is all or
nothing. If one row is bad, nothing is written and the error names the row with
its CSV line, for example `line 4: developer is empty`. The same
person on two rows, under any id or alias, is rejected the same way. One load
takes up to 1,000 rows and 1 MiB; split a bigger file into several, and never
put one person in two files. Loading the same file again is safe; it
overwrites. A load only adds and updates: a developer left out of the file keeps
their current team, and no request just takes a developer off a team; you can
only move them to another. If the command says the outcome is unknown, or a request fails with HTTP
503 (another write held the database for a moment), run the same command again.

Without `tierd`, send the same rows to `POST /api/v1/org_hierarchy` as a JSON
array of objects with the same four fields. Save them in a file called
`teams.json`; leave out `division` or `org` if you do not use them:

```json
[
  {"developer": "jsmith", "team": "platform", "division": "infrastructure", "org": "acme"},
  {"developer": "bdiaz", "team": "payments", "division": "product", "org": "acme"}
]
```

Then send it from the same directory:

```sh
printf 'Authorization: Bearer %s\n' "$(cat ~/.tier/api-token)" |
  curl -sS --fail-with-body -X POST -H @- -H "Content-Type: application/json" \
    --data-binary @teams.json "$TIER_HOST/api/v1/org_hierarchy"
# success response (HTTP 201) includes "accepted":2 plus membership-decision lists and counts
```

This uses `--fail-with-body` rather than `-f` so that an error prints the
server's explanation. The explanation names the row by its position counting
from 0 (`org_hierarchy[3]` is the fourth object). The request and response
shapes are in [api-compatibility.md](api-compatibility.md).

The server resolves each `developer` through the aliases it already knows at the
moment you load, so run step 1 before step 3. To move one person later (this
resolves aliases the same way):

```sh
printf 'Authorization: Bearer %s\n' "$(cat ~/.tier/api-token)" |
  curl -fsS -X PUT -H @- -H "Content-Type: application/json" \
    -d '{"team":"payments","division":"product","org":"acme"}' \
    "$TIER_HOST/api/v1/org_hierarchy/jsmith"
# prints the stored row
```

TIER keeps a dated history of team membership. Every load and every move is
dated by the server's clock at the moment it is made; no request field, file
column or flag can set that date. Team and division rows are computed from the
membership each developer had at the time of each event: spend at the event's
own timestamp, merged work at the time it merged, an invoice at the start of its
month. So a move affects only the future. A person's earlier spend and merged
work stay with the team they were in when it happened, in every window and in
both periods of a comparison. Aliases follow the same rule. Each identifier
keeps its own dated team history: adding an alias puts that identifier in its
person's team from that moment, pointing it at someone else moves it to their
team from that moment, and deleting it places it by its own assignment from
that moment: the team you assigned that identifier itself, if it has an
assignment of its own (for example one you loaded before you made it an
alias), and no team if it has none. Spend and merged work recorded under the identifier before the change
stay in the team it was in then, or in `other` if it had none. When you move a
person, all of their aliases move with them, also from that moment. A server
clock that has been set back is the one exception: a move made then is dated
at the earlier time, so spend and merged work already recorded after that time
move to the new team. TIER refuses a move (`409`) only when the clock reads
earlier than the person's current assignment began, so keep the host's clock
synchronised. If an issue's spend came before a
move and it merged after, the old team shows the spend and the new team shows
the merged work. History from before a developer's first assignment stays in
`other` (the no-team row) even after you assign them. When you upgrade to the
version that added this, the assignments you had already loaded are kept for all
past data; only people you assign after the upgrade start on the day you assign
them. Request and response shapes for all of these requests are in
[api-compatibility.md](api-compatibility.md).

## Where are my teams' scores?

In `team` or `division` mode, they are the main view. The dashboard shows one row
per team (or division), and `GET /api/v1/scores` returns them in the `teams`
array. Teams under the *k* floor appear inside `other`.

In these modes, choose a sealed calendar month with `?period=YYYY-MM`; leave
the parameter out to read the latest sealed month. Arbitrary `since=` and
`until=` windows are available only in the synthetic demo; real-data developer
mode is planned with #826. For September 2026:

```sh
printf 'Authorization: Bearer %s\n' "$(cat ~/.tier/read-token)" |
  curl -fsS -G -H @- "$TIER_HOST/api/v1/scores" --data-urlencode "period=2026-09" |
  jq -c '{predates: .data_quality.window_predates_cost_capture},
    ((.teams // [])[] | {team, tier, weighted_points, total_cost_usd, ranked})'
```

The first line printed is about the month. `predates: true` means the month
starts before TIER began recording spend: merged work from before that point
counts but its spend does not, so the rows can read too high. Pick a later sealed
month. `predates` is `null` when TIER had no coverage start to report — no spend
recorded yet, or the server could not read it. Each line after it is one team
(or division); if none follow, the
month has no data or its rows were withheld (see `data_quality.kanon_suppressed`
above). A sealed month
carries no `data_quality.cost_coverage_safe_since`. If curl reports a 404, that
month is not sealed; run the curl part with `-sS --fail-with-body` in place of
`-fsS`, with the `| jq` stage left off, to read the reason.

For one team, select its row by the name exactly as you loaded it; in `division`
mode, use the division name instead:

```sh
printf 'Authorization: Bearer %s\n' "$(cat ~/.tier/read-token)" |
  curl -fsS -G -H @- "$TIER_HOST/api/v1/scores" --data-urlencode "period=2026-09" |
  jq '{row: ((.teams // [])[] | select(.team == "platform")), predates: .data_quality.window_predates_cost_capture}'
```

The `row` holds that team's `tier`, `weighted_points`, `total_cost_usd`
(at list price), `cost_per_point` and `ranked`. If the team has no named row that
month, `jq` prints nothing: it may have no data, be folded into `other`, or be
withheld. `predates` means the same as above.

In the synthetic demo, the dashboard lists invented developers and has a table headed
"Where the points and spend went, by team". It splits the company total by team,
in team-name order, with a "No team" row for invented developers without a team.
The demo API exposes the same split in `team_rollups` and rejects `?period=`
with HTTP 400. This is a preview of developer mode planned with #826, not an
alternative for real-data servers. The following command is for that synthetic
demo only; point `TIER_HOST` at it, replacing `since` with the
dashboard's **From** date; if **To** has a date, add `--data-urlencode "until=…"`
in `YYYY-MM-DD` form:

```sh
curl -fsS -G "$TIER_HOST/api/v1/scores" --data-urlencode "since=2026-08-28" |
  jq -c '{window_predates_cost_capture: .data_quality.window_predates_cost_capture,
    cost_coverage_safe_since: .data_quality.cost_coverage_safe_since},
    ((.team_rollups // [])[] | {team, unassigned, tier, weighted_points, total_cost_usd, ranked})'
```

The dashboard defaults to 30 days back; the API defaults to midnight UTC 90 days
ago through now. Match the window to compare figures. Spend counts when it was
spent, merged work when it merged. In the synthetic demo only, add
`--data-urlencode "team=Platform"` to the curl command and read `.team` in `jq`
for that team's roll-up; `team_rollups` stays unchanged. If
`data_quality.window_predates_cost_capture` is `true`, move `since` to
`data_quality.cost_coverage_safe_since` or later. Both fields are absent (printed
as `null` above) when TIER had no coverage start to report — no spend recorded
yet, or the server could not read it. The no-team row has `"unassigned": true`
and no team name. It also holds spend that could not be tied to any person, such
as proxy calls that named no developer, recorded under `unattributed`.

In `team` and `division` mode `?team=` is refused with a 400. It rolls up a single team with
no minimum group size, so honouring it there would expose a one-person team. In
the synthetic demo every invented developer is already named, so it reveals
nothing new. Real-data developer mode is planned with #826.

Read `ranked` before you quote a team's number. Despite its name, it does not put
anyone in order; it says whether there is enough evidence to read the number.
`ranked: false` means the team has too little evidence behind it in this window:
fewer than 3 merged outcomes (normally a merged pull request that names an
issue), less than $5 of spend at list price, or any one merged outcome whose
developer recorded fewer than 1,000 AI tokens (input, output and cache tokens
together) against that issue in the 14 days before it merged. The number is
still reported, but do not present it as a headline figure.

## What happens when TIER cannot tell which merge a revert undid?

When a footerless revert's issue matches several eligible outcomes in the same
repository, TIER penalises no one for that check and marks each
candidate outcome `unknown` (revert ambiguous). Its numeric quality stays as it
was; a clean outcome stays at 1.0. The JSON outcomes export and quality-event
exports expose the unresolved status; the dashboard does not display it.
An unresolved commit footer never falls back to guessing by issue. If there is
no qualified candidate, TIER logs a warning and increments a diagnostic counter
but has no outcome to mark unknown. Unresolved checks are never quality evidence
and do not count toward evidence share. These checks do not establish complete
quality coverage. [The technical reference](technical-reference.md#44-quality-floors-what-is-enforced-and-what-is-specified-but-not)
gives the repository, timestamp and merge-push limits. Corrective-work accounting
(Event 3) is built and off by default; it affects live scores when the operator enables it on their installation. See [enabled operation](how-it-works.md#event-3-enabled-operation).

## How do the team scores add up to one company score?

They don't add up, and they aren't averaged. TIER is weighted points of merged
work per $1,000 of AI spend. Each merged outcome earns points from the size
label on its pull request (from 0.5 for `size/xs` to 8 for `size/xl`), or from
an estimate when there is no label, times a quality factor that can drop, for
example when the change is reverted for a code problem ([rubric.md](rubric.md)).
Spend is priced at published list prices
([pricing-philosophy.md](pricing-philosophy.md)), not at what you were billed:
discounts and subscription fees are not applied. What you actually paid can be
recorded separately and shows beside the score as `actual_paid_usd` and
`spend_leverage` (list-price cost divided by the amount paid); neither changes
TIER. For a team, and for the company `total`, TIER adds up everyone's points,
adds up everyone's cost, and divides once. Every developer counts, whether
their own row is `ranked` or not.

Here is why that matters, with made-up numbers:

| (illustrative) | Weighted points | List-price cost | TIER (points ÷ cost in thousands of dollars) |
|---|---|---|---|
| Team A | 60 | $2,000 | 60 ÷ 2.00 = 30 |
| Team B | 10 | $250 | 10 ÷ 0.25 = 40 |
| Average of the two scores | | | (30 + 40) ÷ 2 = 35 |
| Pooled, which TIER reports | 70 | $2,250 | 70 ÷ 2.25 = 31.1 |

The company spent $2,250 and got 70 points, which is 31.1 points per $1,000.
Applying the average of 35 to the $2,250 spent would imply 35 × 2.25 = 78.75
points; the teams delivered 70. The average gives Team B, which spent one eighth
as much, the same say as Team A. Pooling counts each team in proportion to what
it spent. A team's own score is built the same way from its developers: summed
points over summed cost, not an average of developer scores.

The company `total` covers every developer in the window, including anyone not
assigned to a team. In `team` or `division` mode, whenever
`data_quality.kanon_suppressed` is present (see
[Can I see my developers' names?](#can-i-see-my-developers-names)), the `total`
is withheld along with every team row.

The pooled `total` mixes every kind of work, so it is not a fair way to compare
teams that do different work. Per-work-type comparisons are a post-#826
practice on real data: `work_types` is available only in the synthetic demo
today; team and division reports do not publish it. It scores each category (`feature`, `bug`, `security`, `incident`,
`tech-debt`, `research`, `compliance`) separately; compare bug work with bug
work. How a pull request gets its category is in [rubric.md](rubric.md). There
is also no absolute "good" TIER. The fair comparison is a team against itself
over time, under the same rules. Those rules have version numbers, stamped on
every `/api/v1/scores` response as `rubric.version` and `price_table.version`:
the rubric version fixes how merged work turns into points, and the price-table
version fixes what each AI model's tokens cost. A change in either moves the
number without any change in the work ([rubric.md](rubric.md)).

## Why does most of our spend say "no issue attached"? Did we never merge that work?

Not necessarily. "Unattributed" means TIER could not connect that spend to an
issue number. It does not mean the work never merged, unless the pull request
named no issue; see below.

Spend and merged work meet on the issue id (together with the repository, so
issue 42 in two repositories stays two issues):

- Spend: every Claude Code message is recorded with its git branch. A Codex CLI
  session is recorded with the branch it started on, for every message in it. TIER
  first looks for a commit on that branch, made within 30 minutes either side of
  the message, and takes the issue from the branch names attached to that commit
  first, then from a closing directive (`closes #42`) or a configured tracker key (OP is built in) such as
  Jira's `TIER-99` in its subject. A bare `#42` in the subject does not
  attach spend to an issue. If no matching commit supplies an issue, it reads the
  issue from the recorded branch name: `feature/42-login` becomes issue 42, `fix/TIER-99-crash`
  becomes `TIER-99` only when TIER is configured as a tracker key (`--tracker-keys TIER`). Only the first branch slot counts; later numbers, dates and excluded release, bot and revert branches do not supply an issue. Commits are matched on the last part of the branch name, so
  `feature/42-login` and `42-login` count as the same branch.
- Merged work: a merged pull request is tied to an issue from its branch name
  or from its description (`closes #42`).

**Which branch gets recorded.** With worktree attribution off (the default),
TIER uses the branch Claude Code records: the branch of the directory the
session was started in, not of the directory where the work happens.
Switching branches in that same checkout (`git checkout -b feature/42-login`) is
picked up from the next message. Moving the work elsewhere is not: a session
started in the main checkout that edits a sibling worktree
(`git worktree add ../app-42 -b feature/42-login`) keeps recording `main`, and
so does every subagent it starts, because a subagent takes its parent's branch.
That spend lands in `unattributed:main` even after the pull request merges and
scores for issue 42. The fix is to start the session inside the worktree on the
issue branch (`cd ../app-42 && claude`); the subagents it starts then record
that branch too. `--worktree-attribution` is an opt-in preview that infers the
worktree from the file paths a session's tool calls name, so it reads more of
the session file than the default
([privacy.md](privacy.md#worktree-attribution-off-by-default)); read
[how-it-works.md](how-it-works.md#measured-accuracy-read-this-before-turning-it-on)
before turning it on. Parents no longer carry (#1016); only message groups labelled `isSidechain:true` can use carry. Worktree attribution stays opt-in and **off by default**. Activation requires recorded gates, the operator's security rulings and an explicit operator decision. No new accuracy figures have been measured.

Spend that finds no issue is kept, labelled with the reason:

| Label | What happened |
|---|---|
| `unattributed:main` | The recorded branch was `main` or `master`. TIER does not guess an issue here, and skips the commit check too, even if a pull request merged minutes later: a guess could credit the spend to the wrong issue, and TIER treats that as worse than leaving it unlinked. |
| `unattributed:branch-without-issue` | A named branch with no issue number in it (`scratch`, `wip-refactor`) and no matching commit. |
| `unattributed:detached-head` | Git was not on any branch (a "detached HEAD", which is what you get after checking out a single commit or tag), or no branch was recorded at all. Opencode sessions always land here, because Opencode does not record a branch. |
| `unattributed:foreign-repo` | Only with `--worktree-attribution` on: the work was in a worktree of a repository TIER was not configured to watch. The other repository is never named. |
| `unattributed` | Plain `unattributed`, with no reason after it: spend from a source that never sees a branch. That is TIER's proxy (a relay an app can send its AI calls through so TIER meters them) when the app sends no `X-Tier-Issue` header naming the issue, or an org-level billing feed (a poller that reads Anthropic's or OpenAI's organisation-wide usage report). Setup for both is in the README under [Capturing tokens](../README.md#capturing-tokens). |

So a pull request can merge, count as finished work, and still leave the spend
that produced it in one of these buckets. The reverse also happens, and it does
lose merged work: a pull request with no issue reference in its branch or
description records no outcome at all
([conventions.md](conventions.md#consequence-of-a-miss)).

Unattributed spend stays in the cost and earns no points. The pooled scores (a
developer's, a team's, and the company `total`) divide all points by all spend,
so for them it makes no difference which issue the spend is tied to. What can
move them is the merged work behind it. If the spend paid for a pull request that merged with
no issue number, TIER has no record of that work, so the score reads lower than
it should. If the pull request did name an issue and only the spend was recorded
on `main`, the merged work still counts in full and the pooled score is not
skewed; the spend just is not tied to its issue, so that issue's own cost reads
low. The dashboard's low-coverage note says the same: if some unlinked spend paid
for merged work with no issue number, read the score as a floor. The per-type
scores in `work_types` are different: they count only spend tied to an issue of
that type, so they read high while much spend is unlinked.

Where to see it:

- The dashboard's **Attribution coverage** banner turns red below 50%, and the
  panel under it breaks the unlinked spend down by the labels above. The panel
  is available only in the synthetic demo today; real-data developer mode is
  planned with #826. In `team` and `division` mode the share is not published, so the banner says coverage is not shown, and the headline reads
  "provisional"; that means unknown, not low.
- In the API, `data_quality.attributed_cost_share` is the linked fraction,
  `data_quality.unattributed_buckets` is the breakdown, and
  `data_quality.exploratory_cost_share` is the `main` and `master` share on its
  own; all are shares of the window's list-price cost. The last two appear in
  the synthetic demo today, with real-data developer mode planned with #826,
  and are left out when the window has no unattributed spend. In `team` and
  `division` mode the first does not appear either, and
  `data_quality.attribution_coverage` reads `"not_shown"` (see
  [api-compatibility.md](api-compatibility.md#get-apiv1scores)).
- A developer can check their own machine with
  `tierd doctor --repo ~/src/your-repo` (the path to their checkout). It reads
  the last 7 days; in the synthetic demo the banner reads the dashboard's window,
  30 days unless you change the From date. Its "issue attribution" line reports
  how much feature-branch spend is tied to an issue. That is a different share
  from the banner's, which is taken over all spend. It fails below 50%, warns between 50%
  and 90%, and passes at 90% or more. Change the failure line with `--min-attribution`, which
  takes a fraction between 0 and 1 (`--min-attribution 0.4` for 40%); the 90%
  line does not move. Spend recorded on `main` or `master`, on a detached HEAD,
  or with no branch is left out of that figure and reported on a separate line,
  "spend not on a feature branch".

What raises attribution:

1. Put the issue number in every branch name: `feature/42-login`,
   `fix/42`, `fix/TIER-99-crash` (the tracker form works only with TIER configured
   as a tracker key, for example `--tracker-keys TIER`). The accepted patterns are in
   [conventions.md](conventions.md#issue-references--attribution). Avoid a bare
   four-digit number from 1900 to 2099; TIER reads it as a year.
2. Start the AI session in the checkout that is on the issue branch: switch
   branches there first, or `cd` into that branch's worktree before you start.
   A session started in the main checkout keeps recording `main` for work it
   does in another worktree, as described above.
3. Put `closes #42` in the pull request description, so the merged work is
   linked too.
4. Decide how much work on `main` you want. Some of it can be planning and
   review. TIER keeps it in the cost and shows it as its own share, so the team
   can decide whether that amount is intended.

These practice changes affect future capture. There is no supported correction of an already assigned issue; renaming a branch does not move a wrong assignment. Server-side PR and branch fills can assign previously unlinked rows without overwriting an assigned issue or revising sealed reports.

## How do I use this to help my team get better?

See [v0.6.2 availability](#what-do-i-need-before-i-run-these-commands). The levers retro below is a post-#826 practice for real data; today use team or division reports over sealed months.

TIER is built for coaching, not for evaluating developers. Per-person figures are not accurate enough to judge an individual. The positioning is **grade the work, not the worker**, by policy rather than by mechanism. When developer mode ships (#826), named-viewer, audit and retention controls will govern access to individual rows; the coaching stance will still depend on your policy.

Do not rank developers against each other or build a leaderboard from these
figures. In the synthetic demo the dashboard lists invented developers in
identifier order and never sorts them by TIER; a row with too little evidence
stays in its place with a "below evidence floor" tag. For real data today,
compare a team with its own sealed months, accounting for work mix; per-person
and per-work-type comparisons are a post-#826 practice. Before the number
itself, read the available coverage warnings and the bounds' limitations. In
the synthetic demo, #1233 withholds intervals when the row's own attributed-cost
share is below 50%. Each ranked invented developer row at or above that share carries bootstrap bounds (`ci_low` and `ci_high`, drawn as a
whisker on the dashboard bar; nominal 95% percentile bootstrap; coverage unvalidated), and team rows and the company `total` carry none.

Nominal 95% bounds. Coverage is unvalidated: in constructed tests where several outcomes share one issue, they covered the true value less often than 95%. When shown, the bounds are the 2.5th and 97.5th percentiles of the stated bootstrap resampling procedure. Their 95% repeated-sampling coverage has not been validated. They exclude uncertainty from uncaptured usage, incorrect attribution and unattributed spend. Spend is counted when usage happened and points when the work merged, so work that crosses a period boundary can move the ratio either way. Team scores carry no interval, and month-to-month noise is large: TIER has not established how large a change must be to stand out from ordinary variation. Treat a small difference as no evidence either way, and a large one as a prompt to investigate, not a measured effect. Group reports do not disclose exact cost-attribution or quality-evidence shares. Capture-fidelity and cost-horizon warnings are different signals; they do not establish complete capture or observed quality coverage. 'Not shown' is neither a clean bill of health nor evidence that coverage is low. Ask the operator which diagnostics were run, what they found, and which capture or attribution gaps remain. When shown in the v0.6.1 synthetic demo (#1233), the interval is labelled "excludes uncertainty from unattributed spend". The floor uses the row's cost paired with resampled outcomes; exactly 50% passes. Team and division rows carry no interval and reveal no floor status.

[peer-learning.md](peer-learning.md) is the full method for a team. In four
steps:

1. After developer mode ships (#826), hold a team review of the levers. Preview
   this practice only with invented data in the synthetic demo today. Read the
   name-free figures on the dashboard
   or in `/api/v1/scores`: cache reuse (`cost_composition.cache_read_share`, the
   share of input tokens read from the provider's cache), spend on premium
   models (`cost_composition.premium_model_share`, the share of spend on models
   whose list input price is $4 or more per million tokens), work on `main`
   (`data_quality.exploratory_cost_share`), and yield within each work type
   (`work_types`). These figures are available only in the synthetic demo today;
   their real-data use awaits developer mode planned with #826. A real-data
   server in `team` or `division` mode shows the team rows and the total. Ask the team
   what explains each one. Some can be missing, for
   the reasons given under "Where to see it" in
   [Why does most of our spend say "no issue attached"? Did we never merge that work?](#why-does-most-of-our-spend-say-no-issue-attached-did-we-never-merge-that-work).
   If attribution is low, fix that first: the per-type scores in `work_types`
   count only spend tied to an issue, so they read high while much spend is
   unlinked.
2. Treat a move to a cheaper model as a hypothesis to investigate. Compare similar work over finished periods, record other changes (task mix, acceptance criteria, pricing and capture), and inspect acceptance and repair evidence beside cost. A before/after change in TIER cannot show that the model change caused the difference, and TIER does not credit merged work to a model, so it cannot say whether the cheaper model did the work as well.
3. Compare the team's score before and after the change, over two periods that
   have both ended and are the same length. Spend and merged work are each
   counted by their own date, so a period's edges can move its score either way.
   Work paid for before the start that merges inside the period adds points
   without its spend, and reads high. Spend near the end whose work merges after
   the period adds cost without points, and reads low. A short period has less
   in the middle to outweigh its edges, so it moves more
   ([interpreting-the-number.md](interpreting-the-number.md)). Arbitrary-window
   team roll-ups are a post-#826 practice on real data. The following command
   is for the synthetic demo only, with `TIER_HOST` pointing at that demo. The
   demo's merges sit 7 to 19 days before the day it started, so with that day
   as `S`, replace `BEFORE_START` with `S` minus 22 days, `CHANGE_DATE` with
   `S` minus 14 days and `AFTER_END` with `S` minus 6 days, all as UTC dates:

   ```sh
   for period in BEFORE_START,CHANGE_DATE CHANGE_DATE,AFTER_END; do
     curl -fsS -G "$TIER_HOST/api/v1/scores" --data-urlencode "team=Platform" \
         --data-urlencode "since=${period%,*}" --data-urlencode "until=${period#*,}" |
       jq -c '{tier: .team.tier, ranked: .team.ranked, predates: .data_quality.window_predates_cost_capture, safe_since: .data_quality.cost_coverage_safe_since}'
   done
   ```

   Each period is a start date and an end date, written `YYYY-MM-DD`, in UTC.
   The start date is included and the end date is not, so
   `CHANGE_DATE,AFTER_END` covers 8 days, the same length as the first period.

   In `team` or `division` mode the server refuses `?team=`, `since` and `until`
   with a 400: it answers one sealed calendar month per call. Compare two
   months instead, and read the team's row:

   ```sh
   for period in 2026-05 2026-06; do
     printf 'Authorization: Bearer %s\n' "$(cat ~/.tier/read-token)" |
       curl -fsS -G -H @- "$TIER_HOST/api/v1/scores" --data-urlencode "period=$period" |
       jq -c '{row: ((.teams // [])[] | select(.team == "platform") | {tier, ranked}), predates: .data_quality.window_predates_cost_capture}'
   done
   ```

   Sealed months freeze the recorded observation; they do not align each merge
   with all of its spending. Two complete months can still have different
   boundary-crossing work. Before interpreting a change, document differences in repository scope, membership, work mix, capture and measurement rules. If those differences are unknown or substantial, report the two observations without calling their difference an improvement or deterioration.
   Months differ in length by up to three days, the nearest a sealed read comes
   to equal periods. A sealed month carries no `safe_since`; if `predates` is
   `true`, pick a later month. If curl reports a 404, that month is not sealed;
   `--fail-with-body` in place of `-f`, with the `| jq` stage left off, prints
   the reason.
   If curl printed no error and `jq` printed nothing, there is no `platform` row
   that month. To see why, run the same `curl` with
   `jq -c '{teams: [(.teams // [])[].team], total: (.total != null), suppressed: .data_quality.kanon_suppressed}'`.
   If `teams` lists `other`, `platform` may have had fewer than *k* counted
   developers that month (or none at all), or a non-zero measure with fewer than
   *k* counted contributors; anything it had is folded into `other`. If
   `suppressed` is not `null`, figures were withheld, as described in
   [Can I see my developers' names?](#can-i-see-my-developers-names). If `teams` is `[]`, `total` is `false` and `suppressed` is `null`,
   the month has no data at all.
   In `division` mode the row's `team` field holds the division name.

   Compare the two `tier` values as a direction, not a measured effect: team
   roll-ups carry no bootstrap bounds. Check that `ranked` is `true` in both
   periods; if either is `false`, there is too little evidence to read the
   change. If `predates` is `true` for the earlier period, it started before
   TIER was recording spend: its merged work counts but the spend before that
   point does not, so its score reads too high. In the synthetic demo only, pick a
   start date on or after the `safe_since` date the command prints
   (`data_quality.cost_coverage_safe_since`); it is `null` when no coverage
   horizon was available, including when no spend was recorded or the coverage
   lookup failed.
   `GET /api/v1/scores/compare` does not help here. It covers the whole company
   with no team filter, and its `total` and `teams` rows always say
   `significant: false`. Its per-developer rows retain a raw unvalidated non-overlap field; neither value establishes an inference. The dashboard shows “inference not established”. TIER is built for coaching, not for evaluating developers. Per-person figures are not accurate enough to judge an individual.
4. Write the practice down in your team's own runbook: what changed, which
   figure you watched, what happened, keep or drop. TIER does not record
   practice changes, so the note is where another team can find and copy one.


**Seal timing disclosure.** Sealed reports show the actual UTC seal timestamp,
configured grace (or “not recorded” for legacy seals) and elapsed days after
period end. In team and division mode, revert-window status is `withheld` because
outcome-dependent observations can identify a person in an anonymized cohort.
Only developer-level disclosures can state that some outcomes' 60-day revert
windows were still open, when recorded as true at sealing. The default 14-day grace can
leave those windows open; longer grace does not establish delivery completeness.
Late evidence never changes sealed bytes. See the [seal disclosure wire
contract](api-compatibility.md#seal-disclosure-1233).
