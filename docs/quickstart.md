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
(`@v0.2.0`) when you need a build that never moves, e.g. in CI.

Or download a signed release binary from
[github.com/tiermetric/tier/releases](https://github.com/tiermetric/tier/releases).

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

**`score` shows cost, not a full TIER score.** A TIER score is cost per
*accepted* outcome, and outcomes come from merged-PR history — the next step.

## 5. The full TIER score

A TIER score needs outcomes. Reconstruct them from merged PRs, then serve:

```sh
# 1. Reconstruct outcomes from merged-PR history (do this BEFORE serve —
#    SQLite is single-writer).
tierd backfill --repo owner/name --token @/path/to/github-token

# 2. Run the server: it records new PR outcomes and serves the dashboard.
tierd serve --db ~/.tier/tier.db --aggregation developer --watch-repo ~/src/your-repo
```

`--aggregation` is **required and has no default** — `serve` refuses to start
without it, so an existing deployment's privacy posture never changes silently:

- `developer` — named per-developer rows (solo use, or where that is permitted).
- `team` — team-level aggregates only, k-anonymized, never naming an individual.
- `division` — one level higher again.

Then open `http://127.0.0.1:8080`.

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
  local rollout logs. The reverse proxy cannot capture it at all, because Codex
  speaks the OpenAI Responses API rather than Chat Completions.

**If the server runs centrally and Codex runs on a laptop,** the same flag goes
on the shipper — it is off by default there too:

```sh
tierd ship --server https://tier.example --repo ~/src/your-repo \
  --codex-rollout --api-token @/path/to/token
```

Without that flag, a central deployment records the laptop's Codex **outcomes**
(they arrive by webhook, whichever model did the work) while none of its
**per-developer cost** ever lands. That does not read as "Codex is missing" — it
reads as "Codex work was free", which inflates the score of exactly the
developers moving onto the cheaper path (#492).

⚠️ **Pass `--repo` explicitly on `ship`.** It defaults to `.`, and a cron or
launchd job usually runs from `$HOME`. If `$HOME` is not a git checkout the run
fails loudly — exit 1, *"does not appear to be a git repository"*. But if `$HOME`
**is** a checkout, such as a dotfiles repo, every Codex session belonging to
another repository falls outside the scope, nothing ships, and the run **exits
0**. Unlike `serve`, which refuses to start when `--codex-rollout` has no
repository to attribute to, that case is silent.

## 7. Viewing the dashboard from another machine

**The demo is safe to expose directly** — it is read-only and every row is
synthetic:

```sh
tierd demo --addr 0.0.0.0:8124      # then http://<this-host>:8124
```

**A real `serve` deployment must not be exposed unauthenticated.** It carries
real spend, so a non-loopback bind requires a token:

```sh
tierd serve --addr 0.0.0.0:8124 \
  --api-token @/path/to/token \
  --aggregation team \
  --watch-repo ~/src/your-repo
```

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
| `tierd backfill` | yes (`--repo owner/name`) | GitHub token | reconstructed outcomes |
| `tierd serve` | for live capture (`--watch-repo`) | for non-loopback binds | the full TIER score + dashboard |

Run any command with `-h` for its full flag list.
