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

**No recognized label?** TIER falls back to a lines/files heuristic:

```
weight = min(8, max(0.5, ceil(log2(lines_changed + files_changed × 10 + 1))))
```

The heuristic is a reasonable default, but explicit labels are more accurate and
consistent — a two-line change to a critical path and a two-line typo fix get the
same heuristic weight, which labels let you correct.

## Issue references → attribution

Cost and outcomes are attributed to an **issue id** derived from the branch name
or the PR/commit body. These are the exact accepted formats:

| Where | Format | Example | Resolves to |
|---|---|---|---|
| branch | `<prefix>/<N>-<slug>` — trailing `-`/`_` optional; `N` has no leading zero | `feature/42-auth`, `feature/42` | `issue-42` |
| branch | tracker key `[A-Z][A-Z0-9]+-<N>` | `fix/TIER-99-crash` | `TIER-99` |
| PR body / commit | `closes #N`, `fixes #N`, `resolves #N` (case-insensitive) | `closes #42` | `issue-42` |
| PR body / commit | tracker key `[A-Z][A-Z0-9]+-<N>` | `part of PROJ-123` | `PROJ-123` |
| PR body / commit | whitespace-preceded `#N` | `see #42` | `issue-42` |

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
- `## Section` — a Markdown heading; the `#` is not whitespace-preceded by a
  digit, so it never matches an issue.
- `#FF0000` — a hex color, not a numeric issue reference.
- `main`, `master`, `HEAD` — excluded by name; work on these branches is not
  attributed to an issue.

> **Caveat for PR bodies:** because tracker keys are now read from free-form
> body text, prose tokens shaped like a key — `UTF-8`, `SHA-256`, `HTTP-2` — can
> be mis-read as an issue when the body has no other reference. An explicit
> `closes #N` overrides this, so prefer it whenever a PR closes a GitHub issue.

### Consequence of a miss

If neither the branch nor the PR body yields an issue reference:

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
can reconcile those automatically, so name the upstream slug in config:

```yaml
watch:
  repos:
    - /Users/alice/src/tier
  repo_slugs:
    /Users/alice/src/tier: tiermetric/tier
```

Without it, that contributor's cost is attributed to `alice/tier` and never joins
the outcomes recorded against `tiermetric/tier`.
