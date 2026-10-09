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
> scoring the same work differently. Cross-org `cost_per_point` comparison
> therefore requires matched `size_labels` on top of matched versions;
> same-org-over-time is unaffected because one deployment's config is stable.

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

The heuristic is a reasonable default, but explicit labels are more accurate and
consistent — a two-line change to a critical path and a two-line typo fix get the
same heuristic weight, which labels let you correct.

## Issue references → attribution

Cost and outcomes are attributed to an **issue id** derived from the branch name
or the PR/commit body. Accepted formats include:

| Where | Format | Example | Resolves to |
|---|---|---|---|
| branch | `<prefix>/<N>-<slug>` — trailing `-`/`_` optional; `N` has no leading zero | `feature/42-auth`, `feature/42` | `issue-42` |
| branch | tracker key `[A-Z][A-Z0-9]+-<N>` | `fix/TIER-99-crash` | `TIER-99` |
| PR/commit body | whole-word `close`, `closes`, `closed`, `fix`, `fixes`, `fixed`, `resolve`, `resolves`, or `resolved` followed by whitespace or a colon and optional whitespace, then `#N` (case-insensitive) | `closes #42`, `fixes:#42` | `issue-42` |
| PR/commit body | tracker key `[A-Z][A-Z0-9]+-<N>` | `part of PROJ-123` | `PROJ-123` |
| PR/commit body | whitespace-preceded `#N` | `see #42` | `issue-42` |

The collector's commit-**subject** path attributes cost through tracker keys (`PROJ-123` → `PROJ-123`)
or a whole-word close keyword (`close`, `closes`, `closed`, `fix`, `fixes`,
`fixed`, `resolve`, `resolves`, or `resolved`, case-insensitive) followed by
whitespace or a colon and optional whitespace, then `#N` (`closes: #12` → `issue-12`).
The number must end at the end of the text, whitespace, or punctuation;
`fixed #12abc` does not match. Close directives take precedence
over tracker keys. A bare `#N` (`title (#909)`) stays unattributed, never booked
as `issue-N` in this collector path. The webhook push path now uses the same
commit-subject rule (#1069): only the git `%s` subject (first paragraph, folded)
is read for attribution; the body is excluded.

**Branch precedence:** a tracker key (e.g. `TIER-99`) takes priority over a bare
numeric segment when both appear in a branch.

**PR-body precedence:** `closes #N` (highest confidence) > tracker key
(`PROJ-123`) > bare `#N` (lowest confidence). This lets a Jira/Linear shop that
references a tracker key in the PR body — with a generic branch name — still get
attribution, while an explicit `closes #N` always wins when present.

### The year guard (a deliberate tradeoff)

A **bare** 4-digit branch segment in the range **1900–2099** is treated as a
calendar year, not an issue number, and is **dropped**:

- `release/2024-fix` → *no attribution* (2024 read as a year, not issue #2024)
- `release/2024` → *no attribution*

This biases toward the far more common release/date-stamp branch over an issue
whose number happens to land in the year band. Numbers with 1–3 digits or 5+
digits are never guarded (`feature/42`, `bugfix/1234`, `feature/12345` all
attribute normally; `feature/2100` attributes because it is above the window).

The guard **skips** a year segment rather than aborting, so a branch that
carries both a date stamp and an issue number still attributes: `release/2024-42`
→ `issue-42` (the `2024` is skipped, `42` is used).

If your issue numbers genuinely reach 1900–2099, disambiguate with an **explicit
marker**, where no guard applies: use a tracker key (`fix/PROJ-2024-…` →
`PROJ-2024`) or reference `#2024` in the PR body.

### Things that do NOT match (and why)

- `release/2024-fix` — a bare 4-digit year-band segment; guarded as a date
  stamp (see above). Use a tracker key or a `#N` body reference for issue 2024.
- `## Section` — a Markdown heading; neither `#` is immediately followed by
  an issue number, so it never matches an issue.
- `#FF0000` — a hex color, not a numeric issue reference.
- `main`, `master`, `HEAD` — excluded by name; work on these branches is not
  attributed to an issue.

> **Caveat for PR bodies and commit subjects:** tracker keys are read from
> free-form text, so prose tokens shaped like a key — `UTF-8`, `SHA-256`,
> `HTTP-2`, or the `CVE-2026` prefix in `CVE-2026-1234` — can be mis-read as
> an issue when no close directive is present. An explicit `closes #N`
> overrides this, so prefer it whenever a PR or commit closes a GitHub issue.

### Consequence of a miss

If none of the branch, PR body, or commit subjects yields an issue reference:

- **Cost** from that session lands in an `unattributed` bucket instead of against
  an issue.
- **PR outcome** is skipped — a merged PR with no derivable issue records no
  outcome, so it contributes nothing to the TIER numerator.

The fix is always the same: name branches `<prefix>/<issue-number>-<slug>` (for
example `fix/127-webhook-guide`) and reference the issue in the PR body with
`closes #<n>`.

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
