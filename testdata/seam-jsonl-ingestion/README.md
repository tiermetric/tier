# tier-jsonl-ingestion seam — captured sample

This directory holds the captured sample backing the **tier-jsonl-ingestion**
contract check (**captured-sample** exercise class).

## What's here

| File | Purpose |
|---|---|
| `sample.jsonl` | A scrubbed, **real** current-format Claude Code session (13 lines). |
| `last_captured` | `YYYY-MM-DD` the sample was captured — drives the staleness guard. |

## Why a captured *real* sample (not a synthetic fixture)

The seam's contract is the Claude Code session-JSONL format, which tier consumes
but does **not** control. The #96 regression — where the upstream format moved
`cwd` off the first line and every current-format session silently recorded `$0`
— passed synthetic fixtures, CodeQL, and review precisely because synthetic
fixtures don't reflect the live format. So the wire_check replays a **captured
real** session: it preserves the structural contract (line types + ordering, the
`cwd`-appears-on-a-later-line trait, the verbatim `message.usage` token
accounting).

`scripts/seam-capture.py` scrubs the source with an **allowlist** — only the
fields the collector actually consumes survive, so a future Claude Code release
that adds a content field is dropped automatically rather than silently leaking.
It also replaces `cwd` with the `__TIER_REPO__` placeholder (bound to the real
checkout at exercise time) and the real `gitBranch` with a synthetic,
issue-resolvable placeholder (so no branch/project context lands in the repo). A
post-scrub assertion aborts loudly if any surviving string still looks like
content (multi-line, or long-and-spaced).

A captured sample only catches drift while it's **fresh**, so it carries a
freshness obligation: `last_captured` + `max_sample_age` (90d) let the exercise
reject a stale sample rather than let it false-green.

## Running the wire_check

```sh
make seam-exercise        # or: ./scripts/seam-exercise.sh
```

Builds `tierd`, binds the placeholder `cwd` to this checkout, runs `tierd score`
against the sample in a temp `--claude-dir`, and asserts:

- `TOTAL != $0` (a current-format session must score non-zero),
- `empty_cwd == 0` (no session dropped for an empty cwd — the #96 trap), and
- the sample is not stale (`age <= max_sample_age`).

Exits non-zero on any failure, so it drives a CI step or the Phase-2 e2e gate.

## Refreshing the sample (tier owns this)

When the Claude Code session format changes (or the sample ages past
`max_sample_age`), re-capture from a recent real session and bump the marker:

```sh
scripts/seam-capture.py ~/.claude/projects/<encoded-repo>/<session>.jsonl \
    testdata/seam-jsonl-ingestion/sample.jsonl   # also writes last_captured=today
make seam-exercise        # confirm it still passes
```

The capture script writes `last_captured` itself, so the marker and the bytes
can never diverge. Pick a source session that has at least one assistant turn
with billable usage and a model present in the price table.

**Also update the exact-cost pin:** `internal/integration/watcher_wire_test.go`
hard-codes the usage constants (input/output/cache tokens and the pinned dollar
total) copied verbatim from the single assistant turn in `sample.jsonl`. A
re-capture changes those numbers, so update that test's constants in the **same**
PR as the new sample — this coupling is deliberate (a re-capture that silently
skipped it would leave a green test pinning the *old* cost).

## When the gate goes stale

The staleness guard is wired into two gates with **different** severities, on
purpose:

- **`make seam-exercise` (standalone)** treats a
  sample older than `max_sample_age` (90d) as a **hard failure** — fail-loud, no
  bypass. A stale sample no longer reflects the live format, so letting it
  false-green would defeat the guard.
- **`make check-full`** (the mandatory pre-push gate) runs the exercise with
  `SEAM_STALE_POLICY=warn`, so an aged-out sample prints a loud **warning** and
  the gate stays green. Rationale: age-out is a wall-clock condition, not a code
  regression in the PR under test — it must not hard-break every contributor's
  `make check-full` the day the fixture ages out (the current capture ages out
  ~2026-09-20). The actual drift assertions (`TOTAL != $0`, `empty_cwd == 0`)
  remain **hard failures** in both gates, so a real #96-class regression is still
  caught by `make check-full` regardless of staleness.
- **Running from a git worktree** (`.git` is a file, not a directory): `tierd
  score`'s `validateGitRepo` needs a real `.git` **directory**, so the exercise
  cannot run there. Under `SEAM_STALE_POLICY=warn` (i.e. inside `make
  check-full`) it **skips with a warning** rather than hard-failing — run `make
  check-full` in a fresh clone to actually exercise the seam. Standalone `make
  seam-exercise` (default fail mode) still hard-fails in a worktree.

When you see the staleness warning (or the standalone hard failure), the fix is
the same: someone with a recent real Claude Code session against this repo
re-captures per "Refreshing the sample" above, confirms `make seam-exercise`
passes, updates the `watcher_wire_test.go` constants, and commits the new
`sample.jsonl` + `last_captured` in a `chore:` PR.
