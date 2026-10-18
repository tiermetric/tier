# Adoption conventions

TIER silently depends on two conventions. Following them is the difference
between meaningful scores and a wall of `unattributed` cost with no explanation.

## PR size labels → outcome weight

> The weight scale below is **normative and versioned** — it is the canonical
> rubric, stamped as `rubric.version` in every `/scores` response. See
> [rubric.md](./rubric.md) for the versioned rubric, its worked per-size and
> per-`work_type` calibration examples, and the "what you may / may not compare"
> rules. This section documents how the label is *read*; rubric.md defines what
> each size *means*.
>
> **Operator override caveat.** `outcomes.size_labels` lets an operator rename
> which labels map onto the fixed `{0.5, 1, 3, 5, 8}` scale. Renaming a label is
> fine, but mapping a size to a *different* weight than rubric.md's calibration
> (e.g. everyday work as `l`/5 rather than `m`/3) is a **local divergence the
> stamp cannot see** — two deployments both stamp `rubric.version: 1` while
> scoring the same work differently. Cross-organisation `cost_per_point`
> comparison is not supported: matched versions and `size_labels` do not
> establish equivalence; same-organisation-over-time comparison is the supported
> reading because one deployment's config is stable.

The **weight** of a merged PR is its size. TIER reads it from a GitHub label. The
match is case-insensitive and accepts both the `size/` prefix and the bare form:

| Label | Weight |
|---|---|
| `size/xs` or `xs` | 0.5 |
| `size/s` or `s` | 1 |
| `size/m` or `m` | 3 |
| `size/l` or `l` | 5 |
| `size/xl` or `xl` | 8 |

**No recognized label?** TIER falls back to a lines/files heuristic
(`store.GitHeuristic`). It is a **bucketed step function** on a single effort
proxy — not a continuous formula — and it returns only the five weights the label
table above uses, so labeled and unlabeled outcomes stay on one scale (#132):

```
effort = lines_changed + files_changed × 10

effort ≤ 15    → 0.5
effort ≤ 60    → 1
effort ≤ 200   → 3
effort ≤ 1000  → 5
otherwise      → 8
```

Explicit labels let reviewers apply the rubric using context the diff heuristic lacks. TIER has not established that labels are more accurate or consistent; comparability depends on shared calibration.

## Issue references → attribution

Cost and outcomes use the same strict grammar, with different accepted locations on each surface. `OP` is built in. Other tracker prefixes must be configured with `--tracker-keys`, `TIER_TRACKER_KEYS`, or `attribution.tracker_keys` (comma-separated; CLI > env > config > empty). Each prefix must match `^[A-Z][A-Z0-9]{0,15}$`; surrounding whitespace on each comma-separated entry is trimmed (`TIER, JIRA` is valid), and an empty or invalid entry stops startup and names the entry.

| Where | Grammar | Example | Resolves to |
|---|---|---|---|
| branch | first slot: `<N>` followed by `-`, `_`, `.`, or end; no leading zero | `feature/42-auth`, `fix/42`, `42-auth` | `issue-42` |
| branch | first slot: configured `<KEY>-<N>` or built-in `OP`, followed by `-`, `_`, `.`, `/`, or end | `fix/TIER-99-crash` with TIER configured; `OP-2024-x` | `TIER-99`; `OP-2024` |
| PR body | whole-word close directive + `#N`, case-insensitive | `closes #42`, `fixes:#42` | `issue-42` |
| PR body | configured key or OP immediately after a close directive or relation verb, or at the start of a trailer value | `implements PROJ-123`, `Issue: PROJ-123` with PROJ configured | `PROJ-123` |
| PR body | explicit relation or trailer + `#N` | `refs #42`, `Issue: #42` | `issue-42` |
| commit subject | close directive + `#N`, then configured key or OP anywhere, word-bounded | `fix: closes #42`; `fix: PROJ-123` with PROJ configured | `issue-42`; `PROJ-123` |

The first slot is the component immediately after the first `/`, or the whole branch when there is no `/`. Later components and later numbers never count. The first component is excluded when it is `release`, `releases`, `revert`, `dependabot`, `renovate`, `backup`, `archive`, or `gh-pages` (case-insensitive); `revert-*` branches are also excluded. `main`, `master`, `HEAD`, and an empty branch carry no issue evidence. Revert-branch spend stays unattributed even when a nearby commit names an issue; the collector logs an INFO count per run.

A numeric first slot is rejected if it is a four-digit year in 1900–2099 or starts with a date run `YYYY-MM`, `YYYY-MM-DD`, or `YYYYMMDD` followed by `-`, `_`, `.`, or end. There is no fallback to a later number: `release/2024-42`, `feature/007-42`, `docs/notes-2026-10-14`, `fix/435b-auth-50051-cutover`, and `dependabot/docker/node-26-alpine` yield no branch issue. Configured keys and built-in `OP` accept zero-padded positive numbers and preserve the digits (`ACME-006`, `OP-0042`); bare `#007` and numeric branch `007-x` remain invalid. To name issue 2024, use `OP-2024` or a configured key, or `closes #2024` in the PR body.

Close directives are `close`, `closes`, `closed`, `fix`, `fixes`, `fixed`, `resolve`, `resolves`, and `resolved`, followed by whitespace or a colon and optional whitespace, then `#N`. The number ends at whitespace, punctuation, or end of text; `fixed #12abc` does not match. Relation verbs are `refs`, `references`, `part of`, `relates to`, `tracks`, and `implements` (case-insensitive, whole words). Key trailers are `Issue`, `Issues`, `Ref`, `Refs`, `References`, `Ticket`, `Tracker`, `Jira`, or `OpenProject`, followed by `:`. The PR-body relation phrase `half of #N` also attributes, including `the GET half of #1944` and `second half of #829`; `behalf of #5` does not. Hash-reference trailers are `Issue`, `Ref`, `Refs`, or `References`, followed by `:`.

Balanced `*`, `**`, `_`, or `__` wrappers immediately around a reference token are stripped: `Implements **ACME-006**`, `closes **#42**`, `Closes *#42*`, and `refs __#7__` attribute in their accepted locations. Emphasis on the verb (`**closes** #42`) does not attribute.

PR bodies exclude Corrects declaration lines, fenced code (backticks or tildes), quoted lines starting with `>`, and inline code spans. A branch-derived ID wins over the body; otherwise the body chooses close directives, then structured configured keys, then explicit relation references, first match within each class. `see #42 for context`, `Fixes an error in PR #446`, `Follow-up to #733`, free-prose keys, and unconfigured `UTF-8`, `SHA-256`, `HTTP-2`, or `CVE-2026-1234` do not attribute an issue. `Corrects: #733` is correction metadata only.

Commit subjects accept close directives and configured keys; `Refs: #42`, relation references to `#N`, and bare `#N` do not attribute an issue. A bare `(#N)` can still supply PR metadata when the subject supplies no issue, for resolution against an already recorded PR. The push webhook reads only git's `%s` subject, excluding the commit body.

The grammar applies prospectively. Existing outcome and token-event attributions are not rewritten. Strict `issue_rule` values identify new extraction paths; old legacy rules or NULL remain the cutover record. The authorised one-way recovery of previously unattributed events retains its ambiguity, overwrite, default-branch, time-window and sealed-period guards.

At startup and in `tierd doctor --db <existing-db>`, TIER prints `tracker keys: none configured (OP built in)` or the configured list, plus up to ten unconfigured tracker-shaped prefixes found in stored outcome IDs, ordered by count descending then name. These are suggestions only and never change the configuration.

### Consequence of a miss

If none of the branch, PR body, or commit subjects yields an issue reference:

- **Cost** from that session lands in an `unattributed` bucket instead of against
  an issue.
- **PR outcome** is skipped — a merged PR with no derivable issue records no
  outcome, so it contributes nothing to the TIER numerator.

Name branches `<prefix>/<issue-number>-<slug>` (for example
`fix/127-webhook-guide`) and reference the issue in the PR body with `closes #<n>`; the body is only a
fallback when the branch yields no ID.

## Multi-repo organizations

A GitHub issue number is only unique **within one repository**. Repo A's issue #42
and repo B's issue #42 are different work. TIER therefore stores a repository
qualifier alongside every cost row and every outcome row, in its own `repo` column
(#231).

The `issue_id` value itself is unchanged — it is still `issue-42` — so nothing that
reads `issue_id` needs to change. The repository lives beside it.

| Producer | Where `repo` comes from |
|---|---|
| GitHub webhook | `repository.full_name` on the delivery |
| JSONL collector / `tierd ship` | `watch.repo_slugs` override, else the repo's `remote.origin.url` |
| `POST /api/v1/events`, `POST /api/v1/outcomes` | the optional `repo` field |
| Reverse proxy | the optional `X-Tier-Repo` request header |

All of them are normalized to a canonical, lowercase `owner/repo` (no scheme, no
host, no trailing `.git`). GitLab nested groups keep their full path
(`group/subgroup/project`).

**Tracker keys are not qualified.** `TIER-99` and `PROJ-9` are unique across an
organization by construction, so they are stored verbatim.

**When the repository cannot be determined** the row stores the reserved value
`unqualified`. That happens for rows captured before this feature existed, for the
reverse proxy when a client omits `X-Tier-Repo`, and for a checkout with no
`origin` remote. Such rows are not lost: cost and outcomes join **tolerantly** —
the repository disambiguates only when *both* sides name a real one. A repo-blind
cost row still counts toward its issue's outcome, exactly as it did before.

**Forks need an explicit override.** A contributor working from a fork has
`origin = alice/tier`, while the upstream webhook reports `tiermetric/tier`. Nothing
can reconcile those automatically, so name the upstream repository in the YAML you
pass to `tierd serve --config` (see `config.example.yaml`):

```yaml
watch:
  repos:
    - /Users/alice/src/tier
  repo_slugs:
    /Users/alice/src/tier: tiermetric/tier
```

For the one-shot commands use the flag instead: `tierd score --repo-slug tiermetric/tier`
and `tierd ship --server https://<your-server> --repo /Users/alice/src/tier --repo-slug /Users/alice/src/tier=tiermetric/tier`
(`https://<your-server>` is your central server's address — see
[What you need before you start](../README.md#what-you-need-before-you-start)).

Without it, that contributor's cost is attributed to `alice/tier` and never joins
the outcomes recorded against `tiermetric/tier`.
