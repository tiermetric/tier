# TIER — Quickstart

**Audience:** anyone who wants to run TIER against a repository for the first time.

**Every command on this page was run against the released binary.** Where a step has a caveat, it is stated inline rather than discovered later.

---

## Table of contents

1. [Install](#1-install)
2. [See it work in one command](#2-see-it-work-in-one-command)
3. [Point it at your own repository](#3-point-it-at-your-own-repository)
4. [The free cost view](#4-the-free-cost-view)
5. [The full TIER score](#5-the-full-tier-score)
6. [Capturing OpenAI Codex spend](#6-capturing-openai-codex-spend)
   - [Capturing Opencode](#capturing-opencode)
   - [Capturing Meta Muse Code](#capturing-meta-muse-code)
7. [Viewing the dashboard from another machine](#7-viewing-the-dashboard-from-another-machine)
8. [Running on a different port](#8-running-on-a-different-port)
9. [What each command actually needs](#9-what-each-command-actually-needs)

---

## 1. Install

```sh
go install github.com/tiermetric/tier/cmd/tierd@latest
```

`@latest` resolves to the newest tagged release — it does **not** track `main`,
so you always get a published, reproducible version. Pin an exact tag instead
(`@v0.5.2`, the release used as the example below) when you need a build that never moves, e.g. in CI.

`go install` puts the binary in `$(go env GOPATH)/bin` — usually `~/go/bin` —
or in `GOBIN` if you have set it. If the next command says `command not found`,
add that folder to your `PATH` (e.g. `export PATH="$HOME/go/bin:$PATH"` in your
shell profile) and open a new shell.

Or download a signed release archive from
[github.com/tiermetric/tier/releases](https://github.com/tiermetric/tier/releases).
Each release carries one archive per platform, named
`tierd-<version>-<os>-<arch>.tar.gz`: `darwin-arm64` (Apple Silicon Mac),
`darwin-amd64` (Intel Mac), `linux-amd64`, `linux-arm64`, and `windows-amd64`,
each with a `.sha256` checksum beside it. The archive unpacks to a folder of the
same name holding a single file, `tierd` (`tierd.exe` on Windows); move it
somewhere on your `PATH`. `<version>` is the newest tag on the Releases page —
`v0.5.2` at the time of writing, used below as the example:

```sh
tar -xzf tierd-v0.5.2-darwin-arm64.tar.gz
sudo mv tierd-v0.5.2-darwin-arm64/tierd /usr/local/bin/
# optional: gh attestation verify tierd-v0.5.2-darwin-arm64.tar.gz --repo tiermetric/tier
```

**macOS:** if you downloaded the archive with a web browser, macOS marks the
binary as downloaded, and because it is not notarized the system stops it from
running. Remove that mark once (if it says `No such xattr`, there was no mark
and nothing to do):

```sh
sudo xattr -d com.apple.quarantine /usr/local/bin/tierd
```

**Windows:** a `windows-amd64` archive is published, but TIER is not tested on
Windows — there is no Windows CI, the store's file-permission tests skip there,
and the `0600` database-permission guarantee does not apply
([security.md §4](security.md)). Treat it as unsupported until someone reports
it working.

Confirm the build:

```sh
tierd version        # also: tierd -version / --version / -v
```

## 2. See it work in one command

No configuration, no database, no repository — synthetic data:

```sh
tierd demo           # then open http://127.0.0.1:8080
```

Everything on that dashboard is invented (developers named `demo-*`, issues
`DEMO-*`). It is the fastest way to see what TIER produces before pointing it at
anything real.

## 3. Point it at your own repository

Before measuring anything, check that capture works on your machine:

```sh
cd ~/src/your-repo
tierd doctor --repo .
```

`doctor` reports what it can and cannot capture. A common first result is an
**attribution FAIL** — TIER telling you that a large share of your AI spend
cannot be tied to any issue. That is the tool working, not breaking: name
branches `feature/<issue-number>-slug` so cost can be linked to an outcome, or
lower `--min-attribution` if partial coverage is acceptable for this install.

## 4. The free cost view

```sh
tierd score --repo . --since 2026-05-01
```

This reads your local Claude Code session logs and prints **cost per issue** —
no server, no token, no GitHub access. It is the day-one view: where the money
went, before any outcomes are recorded.

**`score` shows cost, not a full TIER score.** A TIER score is *accepted*
outcome points per $1,000 of AI spend, and outcomes come from merged-PR
history — the next step.

## 5. The full TIER score

A TIER score divides merged work by AI spend, so it needs both loaded. Four
steps: load your merged work, start the server, load the spend already on your
disk, and join your two names if they differ.

```sh
# 1. Reconstruct outcomes from merged-PR history (do this BEFORE serve —
#    SQLite is single-writer). Needs a GitHub token that can read this repo's
#    pull requests: a fine-grained token with "Pull requests: Read-only" on
#    the one repo, saved to a file. Steps: README → "What you need before you
#    start" (https://github.com/tiermetric/tier#what-you-need-before-you-start).
tierd backfill --repo owner/name --token @$HOME/.tier/github-token

# 2. Run the server: it captures new spend and serves the dashboard.
#    Leave it running. It does NOT record PRs merged from now on: it sets
#    no webhook secret (no --webhook-secret flag, TIER_WEBHOOK_SECRET or
#    config value), so the GitHub webhook is off, and GitHub cannot reach a
#    laptop's 127.0.0.1 anyway. To add later merges, stop serve, re-run the
#    backfill above (repeats are skipped), restart, then re-run step 3's
#    `tierd ship` below.
tierd serve --db ~/.tier/tier.db --aggregation developer --watch-repo ~/src/your-repo
```

`--aggregation` is **required and has no default** — `serve` refuses to start
without it, so an existing deployment's privacy posture never changes silently:

- `developer` — named per-developer rows (solo use, or where that is permitted).
- `team` — team-level aggregates only, k-anonymized, never naming an individual.
- `division` — one level higher again.

**3. Load past spend.** In a second terminal, while `serve` keeps running:

```sh
tierd ship --server http://127.0.0.1:8080 --repo ~/src/your-repo
```

This step exists because `--watch-repo` only picks up new activity: it reads
session files written to after `serve` starts, not the ones already on your
disk. `backfill` loaded up to 90 days of merged work, so without this step the
score divides that work by only the spend recorded since step 2, and reads too
high: the same work looks cheaper than it was. `ship` sends the last 90 days of
session files still on your disk.

- `--server` is the address the step 2 server listens on:
  `http://127.0.0.1:8080`, unless you changed it with `--addr` (§8).
- `--repo` is the folder of your local checkout, the same folder you passed to
  `--watch-repo`.
- No `--api-token` is needed if the server has no token. `serve` takes its
  token from the `TIER_API_TOKEN` variable when `--api-token` is not passed,
  so if that variable was exported in the step 2 terminal, the server has one:
  export the same value here before running `ship`.
- A server with no token is safe only on a single-user machine: anyone who can
  reach `127.0.0.1:8080` can use every route, including erase. If anyone else
  can reach it, such as other users of a shared machine, or anyone coming
  through an SSH tunnel or a reverse proxy that forwards the port, set a token
  ([README § The API token](../README.md#the-api-token-tier_api_token---api-token)).
- Running it again is safe: every record carries a key, and the server keeps
  one copy.

`ship` reads Claude Code sessions only, unless you add `--codex-rollout` for
Codex ([§6](#6-capturing-openai-codex-spend)), `--opencode` for Opencode
([Capturing Opencode](#capturing-opencode); v0.5.2 or later, since v0.4.1
rejects the flag) or `--muse` for Meta's Muse Code
([Capturing Meta Muse Code](#capturing-meta-muse-code); also v0.5.2 or later).
The step 2 `serve` records the shipped events whether or
not it has that flag, so this step needs no restart. For `serve` to keep
capturing that tool's new activity on this machine afterwards, stop it with
Ctrl-C and start it again with the same flag added. Without the flag, a
run on a repository where you used only Codex, Opencode or Muse finds nothing
and stops with an error that blames the `--repo` path.

**4. Join your two names.** Spend from steps 2 and 3 is recorded under your
computer's login name, which `echo $USER` prints. Merged work from step 1 is
recorded under your GitHub login, which your GitHub profile page shows, or
which `gh api user -q .login` prints if you have the GitHub CLI. If the two are
the same, skip this step. If they differ, you appear twice in the scores: once
with spend and no merged work, once with merged work and no spend.

Check before you join them. Open the scores with
`curl -f http://127.0.0.1:8080/api/v1/scores` and find the row whose
`"developer"` is your computer login. It should show spend
(`"total_cost_usd"` above 0) and no merged work (`"weighted_points"` of 0). If
that row already carries merged work, your computer login is also somebody
else's GitHub login, and those PRs are theirs. Stop here: the join moves both
spend and merged work, so it would put that person's work into your score.

If the check passes, tell the server the two names are one person:

```sh
curl --fail-with-body -X POST http://127.0.0.1:8080/api/v1/developer_alias \
  -H "Content-Type: application/json" \
  -d '{"alias":"'"$USER"'","canonical":"<github-login>"}'
```

The shell fills in your computer login from `$USER`; replace `<github-login>`
with your GitHub login. The server answers `201 Created` with an empty body, so
the command prints nothing when it works. If the server refuses the join, for
example because one of the names is already part of another join, it prints the
reason. The join also applies to spend and work already loaded. To undo it,
delete it by your computer login, or post it again with the right GitHub login:

```sh
curl --fail-with-body -X DELETE "http://127.0.0.1:8080/api/v1/developer_alias/$USER"
```

None of these commands needs a token header if the server has no token. If it
has one (step 3), send it the way
[README § The API token](../README.md#the-api-token-tier_api_token---api-token)
shows, reading the token from its file (`~/.tier/api-token` there) so it never
appears on the command line: start the command with
`printf 'Authorization: Bearer %s\n' "$(cat ~/.tier/api-token)" |` and add
`-H @-` to the `curl` part. The team-server form is in
[README § Identity mapping](../README.md#identity-mapping).

Then open `http://127.0.0.1:8080`, or read the same scores as JSON with
`curl -f http://127.0.0.1:8080/api/v1/scores`.

## 6. Capturing OpenAI Codex spend

TIER captures Codex, but through a different path than Claude Code, and it is
**off by default**:

```sh
tierd serve --aggregation developer \
  --watch-repo ~/src/your-repo \
  --codex-rollout
```

Two things to know:

- **`--codex-rollout` requires `--watch-repo`.** Codex spend is attributed to
  the watched repositories; with no watched repo there is nothing to attribute
  it to, and `serve` now refuses to start rather than silently capturing
  nothing.
- **`tierd score` does not capture Codex** — it reads Claude Code sessions
  only. Codex is captured by `serve` and by `ship` (below), from Codex's own
  local rollout logs. The reverse proxy can parse the OpenAI Responses API that
  Codex speaks (#459), but only for traffic you deliberately point at it with
  API-key auth, and that path has not been verified against live traffic — the
  rollout logs are the supported way to capture Codex.

**If the server runs centrally and Codex runs on a laptop,** the same flag goes
on the shipper — it is off by default there too:

> `https://<your-server>` stands for your central server's address, and
> `@$HOME/.tier/api-token` is the file holding that server's API token — the
> value you started `serve --api-token` with. Both are explained in the README
> under [What you need before you start](../README.md#what-you-need-before-you-start).

```sh
tierd ship --server https://<your-server> --repo ~/src/your-repo \
  --codex-rollout --api-token @$HOME/.tier/api-token
```

Without that flag, a central deployment records the laptop's Codex **outcomes**
(they arrive by webhook, whichever model did the work) while none of its
**per-developer cost** ever lands. That does not read as "Codex is missing" — it
reads as "Codex work was free", which inflates the score of exactly the
developers moving onto the cheaper path (#492).

## Capturing Opencode

Opencode gets the same treatment and the same reasoning, with one extra
precondition. It is **off by default**:

```sh
tierd serve --aggregation developer \
  --watch-repo ~/src/your-repo \
  --opencode
```

`--opencode` requires v0.5.2 or later; v0.4.1 rejects the flag.

Three things to know:

- **`--opencode` requires `--watch-repo`,** for the same reason `--codex-rollout`
  does: with no watched repo there is nothing to attribute the spend to, so
  `serve` refuses to start rather than capturing nothing quietly.
- **GLM-5.3 needs no price-table override.** The embedded table prices `glm-5.3`
  and `glm-5.3-flash` per token at Z.ai's list price (#786). A GLM model with no
  embedded row, such as `glm-5.2`, prices at the guessed `self-hosted-medium`
  fallback ($0.50/M), and `serve` logs a WARN naming it the first time it prices one.
- **Only routes with an audited rate are captured, and the rest are named.**
  Today that means the Z.ai coding plan and nothing else. Ollama's cloud tier is
  deliberately excluded because it publishes no per-token rate; capturing it
  would price the spend at that same guessed fallback and inject invented dollars
  into a cost-per-outcome metric. The exclusion, its reason, and its per-scan row
  count are logged, so what is left out is visible rather than inferred.

The shipper mirrors it, off by default there too:

```sh
tierd ship --server https://<your-server> --repo ~/src/your-repo \
  --opencode --api-token @$HOME/.tier/api-token
```

⚠️ **Do not also route Opencode through TIER's reverse proxy.** Its spend would be
captured twice and the two rows cannot dedup — this collector keys on Opencode's
local `message.id`, the proxy keys on the upstream response id, and neither side
can compute the other's value, so the rows **add** rather than colliding. `serve`
warns at startup when both are enabled.

⚠️ **Pass `--repo` explicitly on `ship`.** It defaults to `.`, and a cron or
launchd job usually runs from `$HOME`. If `$HOME` is not a git checkout the run
fails loudly — exit 1, *"does not appear to be a git repository"*. And if `$HOME`
**is** a checkout, such as a dotfiles repo, every session belonging to another
repository falls outside the scope and nothing ships — which as of #549 is also
**exit 1**, with a per-repo summary naming the zero rows:

```
Per-repo summary:
  "/Users/you": sessions_with_events=0 events_shipped=0
ship: every --repo target kept 0 sessions since 2026-05-05 — this is almost
always a wrong --repo path, not a legitimately idle repo. Pass --allow-empty if
zero is genuinely expected.
```

That case used to exit 0 silently, so "ship reported success" and "ship recovered
nothing" were indistinguishable without diffing the store by hand. If a repo is
*legitimately* idle for the whole window, pass `--allow-empty` (env
`TIER_SHIP_ALLOW_EMPTY`) to get exit 0 back.

The guard measures the **whole run**, not just Claude Code: a machine that only
produces Codex spend exits 0 normally, because the Codex events count even
though they are scanned once across all repos rather than per repo.

## Capturing Meta Muse Code

Muse Code is Meta's coding agent, run as the `muse` command. TIER reads the
session log Muse writes for each session, and it is **off by default**:

```sh
tierd serve --aggregation developer \
  --watch-repo ~/src/your-repo \
  --muse
```

`--muse` requires v0.5.2 or later; v0.4.1 rejects the flag.

Where the input comes from: Muse writes one log per session to
`~/.local/share/muse/sessions/YYYY/MM/DD/<session-id>/session.jsonl`, with no
setup on your part. `ls ~/.local/share/muse/sessions` shows whether this machine
has any. If your Muse data lives elsewhere, set `home:` in the `collectors.muse`
block of the config file (`config.example.yaml` shows it) or pass `--muse-home`
to `ship`; TIER reads the `sessions` folder inside it.

Five things to know:

- **`--muse` requires `--watch-repo`,** as `--codex-rollout` does. A Muse
  session run in a folder outside every watched repository is skipped, not
  attributed.
- **TIER decodes counts, never text.** From each session log it decodes token
  counts, model names, the session id (which it stores), each run's recorded
  branch, the folder Muse ran in (used to pick the repository, then
  dropped) and the ids linking a subagent to the run that started it. It never decodes your prompts, Muse's replies or tool output
  ([privacy.md](privacy.md)).
- **A run is recorded after it finishes.** Muse writes a run's git branch only
  when the run ends, so TIER waits for that before recording the run's calls:
  expect a run's cost to appear after the run completes, not while it works. If
  a run's session log goes 6 hours untouched with no branch recorded, TIER
  records that run as `unattributed:detached-head`. That includes a run still
  waiting on you at a tool-approval prompt after 6 hours, and TIER cannot fix
  its issue afterwards.
- **Subagent spend counts toward the run that started it.** Calls made by a
  Muse subagent are recorded under the repository and branch of the run that
  started it, once that run has finished. Three kinds are still left out, each
  with a warning in the log: a subagent that Muse's log never links to one
  run (TIER waits until neither log has changed for 6 hours, then leaves it
  out rather than guess), a subagent started by another subagent, and a
  subagent whose parent session recorded no folder. For those, TIER's Muse
  figure is lower than what Muse actually spent.
- **Muse is priced at Meta's published per-token API rates,** even if you pay
  for Muse by subscription (see
  [reference-price-table.md](reference-price-table.md)). `tierd score` does not
  read Muse; `serve` and `ship` do.

**To confirm it works,** look for these lines in the `serve` output:
`Muse collector enabled` with the `sessions_dir` it is reading, then
`muse scan complete` with an `events=` count (`subagent_events=` is the part
that came from subagents). That line is written only for a scan that had a
session log to read; a scan with none writes nothing, so a quiet log on an idle
machine is normal. If the sessions folder does not exist, it logs
`muse sessions root does not exist; nothing to scan` instead.

`tier_muse_events_total` at `http://127.0.0.1:8080/metrics` counts events handed
to the store, repeats included: a session log TIER reads again (a changed one, one
still waiting on a run, or every one after a restart) re-sends its calls, and the
store keeps one copy. A rising value shows capture is alive; it is not a count of
stored rows. If the server has a token, send it as
`Authorization: Bearer <token>`; in `developer` mode the read-only token
(`--read-token`) is enough for `/metrics`, and in `team` or `division` mode use
the metrics token (`--metrics-token`) instead. Read it from its file so it stays
off the command line, as in step 4 of §5:

```sh
printf 'Authorization: Bearer %s\n' "$(cat ~/.tier/api-token)" |
  curl -sf -H @- http://127.0.0.1:8080/metrics | grep tier_muse_events_total
```

Use your metrics token's file instead if you have one (in developer mode, the
read token's file also works). With no token, drop the
`printf … |` line and `-H @-`. `tierd doctor` does not check Muse.

The shipper mirrors it, off by default there too. `https://<your-server>` and
`@$HOME/.tier/api-token` mean what the note in [§6](#6-capturing-openai-codex-spend) says:

```sh
tierd ship --server https://<your-server> --repo ~/src/your-repo \
  --muse --api-token @$HOME/.tier/api-token
```

Its summary prints a `muse (all repos): events_shipped=<n>` line. A run still
in progress is not shipped; the next `ship` picks it up once it finishes.

## 7. Viewing the dashboard from another machine

**The demo is safe to expose directly** — it is read-only and every row is
synthetic:

```sh
tierd demo --addr 0.0.0.0:8124      # then http://<this-host>:8124
```

**A real `serve` deployment must not be exposed unauthenticated.** It carries
real spend, so a non-loopback bind requires an API token. The token is a
password you make up once and keep in a file:

```sh
mkdir -m 700 -p ~/.tier
[ -s ~/.tier/api-token ] || (umask 077; openssl rand -hex 32 > ~/.tier/api-token)   # keeps an existing token
[ -s ~/.tier/read-token ] || (umask 077; openssl rand -hex 32 > ~/.tier/read-token) # viewers' token
tierd serve --addr 0.0.0.0:8124 \
  --db "$HOME/.tier/tier.db" \
  --api-token @$HOME/.tier/api-token \
  --read-token @$HOME/.tier/read-token \
  --aggregation team \
  --watch-repo ~/src/your-repo
```

A script or CI job calling the API from another machine then sends
`Authorization: Bearer <the value in that file>`. People who only view the
dashboard should not get this token: give them the read-only token instead —
the value in `~/.tier/read-token` (`cat ~/.tier/read-token`), which can read
but never write — and they paste it into the dashboard's token box. Hand it
over through a password manager or another private channel, never chat, email
or a ticket. The two files must hold different values; `serve` refuses to
start if they match. Full detail:
[What you need before you start](../README.md#what-you-need-before-you-start).

**Team and division modes show closed, sealed calendar months only.** They do
not show live spend. This preserves the privacy floor: overlapping date windows
could be subtracted to isolate one person's figures even when each group clears
k-anonymity. Team/division mode withholds numbers when fewer than k contributing
developers are present (default k = 5, minimum 3). A solo user or a team smaller
than k sees no numbers; use `--aggregation developer` to see them. Sealing does
not remove that floor.

Before the first seal, `GET /api/v1/scores` returns `404` with this `error`:

```text
sealing not armed: no month is sealed until the operator arms sealing with seal_from or `tierd seal --arm` (#913)
```

The dashboard shows `YYYY-MM is not published:` followed by that reason, with
no scores. Opening or refreshing it never creates a seal.

On the server, finish backfilling cost and outcomes and loading the team and
alias maps **before arming**: a sealed month never changes, and the first seal
permanently pins the earliest published month. Leave `serve` running so it has
registered its sources. In another terminal, under the same account and with
the same environment, preview and then confirm the first seal:

**`tierd seal` requires v0.5.2 or newer**, so these commands are an exception
to the released-binary check at the top of this page.

```sh
tierd seal --arm earliest --dry-run --aggregation team --db "$HOME/.tier/tier.db"
tierd seal --arm earliest --aggregation team --db "$HOME/.tier/tier.db"
tierd seal --status --aggregation team --db "$HOME/.tier/tier.db" \
  --server http://127.0.0.1:8124 --api-token @$HOME/.tier/read-token
```

The second command prompts for confirmation, seals the first eligible month,
and arms future sealing. `earliest` selects the first full UTC month of cost
coverage; use `--arm YYYY-MM` to start later. Arming is possible only once that
full month has closed plus `--report-grace` (default 336h, **14 days**), with
configured sources settled through its end. For example, cost data starting
2026-09-03 makes October the first full UTC month: it closes at 2026-11-01
00:00Z and is armable from **2026-11-15 00:00Z** with the default grace.
With no eligible history yet, the command refuses: backfill the missing history
or wait until a full covered month and its grace have elapsed. It also refuses
a month whose start predates the retention horizon. If you customized `serve`,
also pass its same `--config`, `--prices`,
`--k-anonymity`, `--report-grace` and database settings to `seal`.

After success, a read with no month (`GET /api/v1/scores`) returns the newest
sealed month, so the armed month shows immediately. To select the month printed
by `seal` explicitly, use `GET /api/v1/scores?period=YYYY-MM`. The writable server
seals subsequent eligible months at startup and on the next hourly pass
(within the hour, once sources are settled); refresh the dashboard to see them.
See the [sealed-read contract](api-compatibility.md)
for unavailable months and source holds.

If you would rather not open a port at all, tunnel over SSH from the machine you
are browsing on — nothing is exposed to the network:

```sh
ssh -N -L 8080:127.0.0.1:8080 you@the-host   # then http://127.0.0.1:8080 locally
```

## 8. Running on a different port

Every server command takes `--addr host:port`:

```sh
tierd demo  --addr 127.0.0.1:8123
tierd serve --addr 127.0.0.1:9000 --aggregation developer --watch-repo ~/src/your-repo
```

## 9. What each command actually needs

| Command | Needs a repo? | Needs a token? | Produces |
|---|---|---|---|
| `tierd demo` | no | no | synthetic dashboard |
| `tierd doctor` | yes (`--repo`) | no | a capture health check |
| `tierd score` | yes (`--repo`) | no | cost per issue (Claude Code only) |
| `tierd backfill` | yes (`--repo owner/name`) | GitHub token ([how](../README.md#what-you-need-before-you-start)) | reconstructed outcomes |
| `tierd serve` | for live capture (`--watch-repo`) | for non-loopback binds | the full TIER score + dashboard |

Run any command with `-h` for its full flag list.
