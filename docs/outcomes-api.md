# Outcome ingest API (provider-neutral)

TIER computes a full score only when it has **outcomes** — a merged PR/MR with a
weight and quality — joined to captured **cost**. The GitHub webhook
(`docs/webhook-setup.md`) is one way to record outcomes, but it is
GitHub-specific: it verifies an `X-Hub-Signature-256` HMAC over a GitHub payload
shape. Shops on GitLab, Bitbucket, Gitea, or any other forge use
`POST /api/v1/outcomes` instead — one bearer-gated endpoint any CI can call when
a change merges.

An outcome recorded through this endpoint scores **identically** to one recorded
through the GitHub webhook for the same inputs: both resolve weight through the
same shared logic (`store.ResolveWeight`) and default quality to `1.0`.

## Authentication

The endpoint is gated by the server's API token — the value you started
`tierd serve --api-token` with ([what that is](../README.md#what-you-need-before-you-start)),
the same token that gates `POST /api/v1/costs` and the other write routes.
`<your-server>` is your server's host name and port — `127.0.0.1:8080` on a
laptop, where you use `http://` in place of `https://`. Send the token's **value** as a bearer
header (curl does not understand tierd's `@file` shorthand, so `$TIER_API_TOKEN`
here must hold the value, not an `@` path):

```sh
curl -sS -X POST "https://<your-server>/api/v1/outcomes" \
  -H "Authorization: Bearer $TIER_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "developer": "alice",
    "issue_id": "GL-42",
    "pr_number": 128,
    "merge_commit_sha": "9fceb02d0ae598e95dc970b74767f19372d61af8",
    "merged_at": "2026-07-06T14:12:00Z",
    "additions": 40,
    "deletions": 10,
    "changed_files": 3
  }'
```

> **The bearer token is the only authenticator.** Unlike the GitHub webhook,
> there is no per-provider signature. Any holder of the token can post an outcome
> as any developer, so treat it as an org-level secret. Every outcome recorded
> this way is stamped `source = "api-outcome"` (vs `"github-webhook"`) so a
> manual or forged outcome is distinguishable from a webhook-derived one in
> audit.

## Request body

| Field              | Type    | Required | Notes |
|--------------------|---------|----------|-------|
| `developer`        | string  | yes      | The developer's name as the **cost** side records it (OS username or `ship --developer`), or one you have alias-mapped to it — otherwise the outcome never joins any spend. ≤ 256 chars. |
| `issue_id`         | string  | yes      | Issue/ticket the change resolves. Must match what TIER derives from branches: `issue-42` for a GitHub number, or a tracker key like `GL-42`. ≤ 256 chars. |
| `pr_number`        | integer | yes      | The PR/MR number (GitLab MR IID, Bitbucket PR id). Must be ≥ 1. |
| `merge_commit_sha` | string  | yes      | The merge/squash commit hash. **Dedup key** — a replay with the same value is a no-op. ≤ 256 chars. |
| `merged_at`        | string  | yes      | RFC 3339 merge time (e.g. `2026-07-06T14:12:00Z`). Becomes the outcome's timestamp for score windows. |
| `weight`           | number  | no       | A label-equivalent size weight (`0.5`, `1`, `3`, `5`, `8`). When set it wins and is recorded with `weight_source: "label"`. Must be in `(0, 8]`. |
| `quality`          | number  | no       | Quality multiplier in `[0, 1]`. Defaults to `1.0`. |
| `additions`        | integer | no       | Lines added. Used for the size heuristic when `weight` is omitted; retained for future re-scoring. ≥ 0. |
| `deletions`        | integer | no       | Lines deleted. ≥ 0. |
| `changed_files`    | integer | no       | Files changed. ≥ 0. |
| `work_type`        | string  | no       | The kind of work: `feature`, `bug`, `security`, `incident`, `tech-debt`, `research` or `compliance`. Any other non-empty value is a `400`. Omitted → `feature`, recorded as a default rather than as your choice. |
| `repo`             | string  | no       | The repository, as `owner/repo` (on GitLab, the full `group/subgroup/project` path). Stored in lower case. Send it if you have more than one repository: without it, issue 42 in two repos counts as one issue. Omitted → `unqualified`; sending `unqualified` yourself is a `400`. ≤ 256 chars. |

### Weight resolution (parity with the webhook)

- **If you send `weight`**, it is used verbatim and recorded as `weight_source:
  "label"` — the same as a GitHub size label (`size/xs`…`size/xl` → `0.5`…`8`).
- **If you omit `weight`**, TIER derives it from the diff-size heuristic using
  `additions + deletions` and `changed_files` — the same buckets the webhook
  applies to an unlabeled PR — and records `weight_source: "git-heuristic"`.

Send the size labels your team already uses as the numeric `weight`, or send the
diff stats and let TIER bucket them. Either way the resulting score matches what
the GitHub webhook would have produced.

## Responses

| Status | Meaning |
|--------|---------|
| `201 Created` | Outcome recorded. Body: `{"status":"created","weight_source":"...","weight":...}`. |
| `200 OK` | The `merge_commit_sha` was already recorded — this was a replay, nothing was inserted. Body: `{"status":"duplicate",...}`. |
| `400 Bad Request` | Missing/invalid field. Body: `{"error":"..."}`. |
| `401 Unauthorized` | Missing or wrong bearer token. |

Re-posting the same `merge_commit_sha` is always safe: dedup is enforced by a
unique index, so a CI job that retries never double-counts points.

## CI examples

Both examples run in the pipeline that starts when a change lands on your
default branch. That pipeline knows the commit, but not which merge request
(MR) or pull request (PR) it came from: GitLab sets `CI_MERGE_REQUEST_IID` only
in merge request pipelines while the MR is still open, and Bitbucket sets
`BITBUCKET_PR_ID` only in pull-request builds. So each recipe asks the forge's
API which merged MR or PR produced the commit, and builds every field from that
answer.

You create two CI variables yourself:

- `TIER_API_TOKEN`: the token value `tierd serve` uses (the file's contents if
  you passed `@/path`) ([what that is](../README.md#what-you-need-before-you-start)).
- `TIER_HOST`: your server's address, scheme and host with no trailing slash
  (e.g. `https://tier.example.com`).

On GitLab, add them under **Settings → CI/CD → Variables** (you need the
Maintainer role) and tick **Masked** for the token. On Bitbucket, add them under
**Repository settings → Pipelines → Repository variables** and tick **Secured**
for the token. Every other `$CI_*` or `$BITBUCKET_*` variable in the recipes is
set by GitLab or Bitbucket; you do not create it. Each recipe writes every token
into a header file readable only by the job (`umask 077`, the shell's own
`printf`) and hands curl the file (`-H @file`), so no token appears on a command
line.

Where each field comes from:

| Field | Source |
|---|---|
| `developer` | The MR/PR **author**, from the API. Not the person who clicked merge, and not whoever started the pipeline. If it differs from the name on the cost side, map it with `POST /api/v1/developer_alias`. |
| `issue_id` | Worked out from the MR/PR **source branch** by the same branch-name rule TIER's collector uses, so it matches the spend: a tracker key wins (`fix/TIER-99-crash` → `TIER-99`); otherwise the first all-digit part that does not start with `0` and is not a year from 1900 to 2099 becomes `issue-<n>` (`feature/42-auth` → `issue-42`). If the branch names no issue, the recipe posts nothing. |
| `pr_number` | The MR's IID or the PR's id, from the API. |
| `merge_commit_sha` | The commit the pipeline runs for. |
| `merged_at` | GitLab: the MR's `merged_at`. Bitbucket: the merge commit's date. |
| `repo` | The project path (`CI_PROJECT_PATH` or `BITBUCKET_REPO_FULL_NAME`), the same `owner/repo` your git remote names. |

Neither recipe sends `weight` or diff stats, so every outcome gets the smallest
weight, `0.5`. Add `weight` from your size label, or `additions` / `deletions` /
`changed_files` (e.g. from `git diff --shortstat`), if you want sizes.

### GitLab CI (`.gitlab-ci.yml`)

Runs on the default branch after a merge. It calls
`GET /projects/:id/repository/commits/:sha/merge_requests` with the job's own
`CI_JOB_TOKEN`, an endpoint GitLab's
[job token page](https://docs.gitlab.com/ci/jobs/ci_job_token/) lists as
allowed. GitLab has matched that lookup against merge and squash commits since
[!49968](https://gitlab.com/gitlab-org/gitlab/-/merge_requests/49968). The job
keeps only a merged MR that merged as this very commit. It does not rely on the
endpoint's `state` parameter, which GitLab added in 18.2.

A push straight to the default branch has no MR, so the job prints a line and
posts nothing. But if the commit message starts `Merge branch` (GitLab's merge
commit) and no merged MR comes back, the job fails, so a broken lookup cannot
pass as a quiet no-op.

If your admins have narrowed what the job token may call, use an access token
instead:

- It needs the `read_api` scope and at least the **Reporter** role on the
  project.
- Store it as a masked variable named `TIER_GITLAB_TOKEN`.
- Change the header line to
  `printf 'PRIVATE-TOKEN: %s\n' "$TIER_GITLAB_TOKEN" > "$H_GL"`.

A project access token (**Settings → Access tokens**) works on GitLab
Self-Managed. On GitLab.com, project and group access tokens need Premium or
Ultimate, so on the Free tier use a personal access token, created under your
avatar → **Edit profile → Access → Personal access tokens**.

```yaml
tier-outcome:
  stage: .post
  image: alpine:3.24
  rules:
    - if: '$CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH'
  script:
    - apk add --no-cache curl jq
    - |
      set -eu
      umask 077
      H_GL=$(mktemp)
      H_TIER=$(mktemp)
      trap 'rm -f "$H_GL" "$H_TIER"' EXIT
      printf 'JOB-TOKEN: %s\n' "$CI_JOB_TOKEN" > "$H_GL"
      printf 'Authorization: Bearer %s\n' "$TIER_API_TOKEN" > "$H_TIER"
      # 1. Which MR merged as this commit?
      MRS=$(curl -sS --fail -H @"$H_GL" \
        "$CI_API_V4_URL/projects/$CI_PROJECT_ID/repository/commits/$CI_COMMIT_SHA/merge_requests")
      MR=$(printf '%s' "$MRS" | jq -c --arg sha "$CI_COMMIT_SHA" \
        'map(select(.state == "merged" and (.merge_commit_sha == $sha or .squash_commit_sha == $sha or .sha == $sha)))[0] // empty')
      if [ -z "$MR" ]; then
        case "$CI_COMMIT_MESSAGE" in
          "Merge branch "*)
            echo "tier: $CI_COMMIT_SHA is a merge commit but GitLab returned no MR merged as it" >&2
            exit 1 ;;
        esac
        echo "tier: no merged MR for $CI_COMMIT_SHA; nothing posted"
        exit 0
      fi
      # 2. issue_id from the MR's source branch, by TIER's branch-name rule.
      BRANCH=$(printf '%s' "$MR" | jq -r .source_branch)
      ISSUE=$(printf '%s\n' "$BRANCH" | grep -oE '[A-Z][A-Z0-9]+-[0-9]+' | head -n 1 || true)
      if [ -z "$ISSUE" ]; then
        for seg in $(printf '%s' "$BRANCH" | tr '/_-' '   '); do
          case "$seg" in ''|0*|*[!0-9]*) continue ;; esac
          case "$seg" in 19[0-9][0-9]|20[0-9][0-9]) continue ;; esac
          ISSUE="issue-$seg"
          break
        done
      fi
      if [ -z "$ISSUE" ]; then
        echo "tier: branch '$BRANCH' names no issue; nothing posted"
        exit 0
      fi
      # 3. Post the outcome. The MR JSON goes in on stdin, never on a command line.
      printf '%s' "$MR" | jq --arg issue "$ISSUE" --arg sha "$CI_COMMIT_SHA" \
        --arg repo "$CI_PROJECT_PATH" --arg ts "$CI_COMMIT_TIMESTAMP" '{
          developer: .author.username,
          issue_id: $issue,
          pr_number: .iid,
          merge_commit_sha: $sha,
          merged_at: (.merged_at // $ts),
          repo: $repo
        }' |
      curl -sS --fail-with-body -X POST "$TIER_HOST/api/v1/outcomes" \
        -H @"$H_TIER" \
        -H "Content-Type: application/json" \
        --data-binary @-
```

### Bitbucket Pipelines (`bitbucket-pipelines.yml`)

Runs on the main branch after a merge. Bitbucket gives a pipeline step no API
token of its own. Create a repository access token instead:

- Create it under **Repository settings → Security → Access tokens**.
- Give it the **Pull requests: Read** and **Repositories: Read** scopes.
  Bitbucket keeps these separate, and the step needs both.
- Store it as a secured repository variable named `TIER_BITBUCKET_TOKEN`.

The PR number comes from the first line of the merge commit's message.
Bitbucket's default message for the merge-commit and squash strategies starts
`Merged in <branch> (pull request #<id>)`. A commit whose first line does not
match, such as a direct push, a revert of a merge, or any commit under the
fast-forward strategy, is not a PR merge, so the step prints a line and posts
nothing. The step also checks that the PR it found really merged as this
commit, and fails if not.

```yaml
pipelines:
  branches:
    main:
      - step:
          name: Record TIER outcome
          image: alpine:3.24
          script:
            - apk add --no-cache curl jq
            - |
              set -eu
              umask 077
              H_BB=$(mktemp)
              H_TIER=$(mktemp)
              trap 'rm -f "$H_BB" "$H_TIER"' EXIT
              printf 'Authorization: Bearer %s\n' "$TIER_BITBUCKET_TOKEN" > "$H_BB"
              printf 'Authorization: Bearer %s\n' "$TIER_API_TOKEN" > "$H_TIER"
              API="https://api.bitbucket.org/2.0/repositories/$BITBUCKET_REPO_FULL_NAME"
              # 1. The first line of the merge commit's message names the PR.
              COMMIT=$(curl -sS --fail -H @"$H_BB" "$API/commit/$BITBUCKET_COMMIT")
              PR_ID=$(printf '%s' "$COMMIT" | jq -r .message | head -n 1 \
                | sed -n 's/^Merged in .* (pull request #\([0-9][0-9]*\)).*/\1/p')
              if [ -z "$PR_ID" ]; then
                echo "tier: $BITBUCKET_COMMIT is not a pull request merge; nothing posted"
                exit 0
              fi
              # 2. Read the PR and check it merged as this commit.
              PR=$(curl -sS --fail -H @"$H_BB" "$API/pullrequests/$PR_ID")
              MERGED=$(printf '%s' "$PR" | jq -r '.merge_commit.hash // ""')
              if [ -z "$MERGED" ] || [ "${BITBUCKET_COMMIT#"$MERGED"}" = "$BITBUCKET_COMMIT" ]; then
                echo "tier: PR #$PR_ID did not merge as $BITBUCKET_COMMIT" >&2
                exit 1
              fi
              # 3. issue_id from the PR's source branch, by TIER's branch-name rule.
              BRANCH=$(printf '%s' "$PR" | jq -r .source.branch.name)
              ISSUE=$(printf '%s\n' "$BRANCH" | grep -oE '[A-Z][A-Z0-9]+-[0-9]+' | head -n 1 || true)
              if [ -z "$ISSUE" ]; then
                for seg in $(printf '%s' "$BRANCH" | tr '/_-' '   '); do
                  case "$seg" in ''|0*|*[!0-9]*) continue ;; esac
                  case "$seg" in 19[0-9][0-9]|20[0-9][0-9]) continue ;; esac
                  ISSUE="issue-$seg"
                  break
                done
              fi
              if [ -z "$ISSUE" ]; then
                echo "tier: branch '$BRANCH' names no issue; nothing posted"
                exit 0
              fi
              # 4. Post the outcome. The PR JSON goes in on stdin, never on a command line.
              TS=$(printf '%s' "$COMMIT" | jq -r .date)
              printf '%s' "$PR" | jq --arg issue "$ISSUE" --arg sha "$BITBUCKET_COMMIT" \
                --arg repo "$BITBUCKET_REPO_FULL_NAME" --arg ts "$TS" '{
                  developer: .author.uuid,
                  issue_id: $issue,
                  pr_number: .id,
                  merge_commit_sha: $sha,
                  merged_at: $ts,
                  repo: $repo
                }' |
              curl -sS --fail-with-body -X POST "$TIER_HOST/api/v1/outcomes" \
                -H @"$H_TIER" \
                -H "Content-Type: application/json" \
                --data-binary @-
```

> The author's `uuid` is a stable identity but not a readable one; map it to
> the developer's cost-side name with `POST /api/v1/developer_alias`.

## Verifying

After posting, the developer should appear in the scores API:

```sh
curl -sS "https://<your-server>/api/v1/scores" \
  -H "Authorization: Bearer $TIER_API_TOKEN" | jq '.developers[] | {developer, weighted_points}'
```

`weighted_points` reflects the outcome's `weight × quality`. Re-post the same
`merge_commit_sha` and you get `{"status":"duplicate"}` with no change to the
count — confirming replay safety.
