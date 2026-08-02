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

The endpoint is gated by the same bearer token as `POST /api/v1/costs` and the
other write routes. Send it as an `Authorization: Bearer <token>` header:

```sh
curl -sS -X POST "https://<tierd-host>/api/v1/outcomes" \
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
| `developer`        | string  | yes      | Canonical developer identifier. ≤ 256 chars. |
| `issue_id`         | string  | yes      | Issue/ticket the change resolves. ≤ 256 chars. |
| `pr_number`        | integer | yes      | The PR/MR number (GitLab MR IID, Bitbucket PR id). Must be ≥ 1. |
| `merge_commit_sha` | string  | yes      | The merge/squash commit hash. **Dedup key** — a replay with the same value is a no-op. ≤ 256 chars. |
| `merged_at`        | string  | yes      | RFC 3339 merge time (e.g. `2026-07-06T14:12:00Z`). Becomes the outcome's timestamp for score windows. |
| `weight`           | number  | no       | A label-equivalent size weight (`0.5`, `1`, `3`, `5`, `8`). When set it wins and is recorded with `weight_source: "label"`. Must be in `(0, 8]`. |
| `quality`          | number  | no       | Quality multiplier in `[0, 1]`. Defaults to `1.0`. |
| `additions`        | integer | no       | Lines added. Used for the size heuristic when `weight` is omitted; retained for future re-scoring. ≥ 0. |
| `deletions`        | integer | no       | Lines deleted. ≥ 0. |
| `changed_files`    | integer | no       | Files changed. ≥ 0. |

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

Both examples assume the tierd bearer token is stored as a masked CI variable
`TIER_API_TOKEN` and the tierd host as `TIER_HOST`.

### GitLab CI (`.gitlab-ci.yml`)

Runs on the default branch after a merge; GitLab exposes the merge commit and
MR metadata as predefined variables.

```yaml
tier-outcome:
  stage: .post
  image: curlimages/curl:latest
  rules:
    - if: '$CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH'
  script:
    - |
      curl -sS --fail -X POST "$TIER_HOST/api/v1/outcomes" \
        -H "Authorization: Bearer $TIER_API_TOKEN" \
        -H "Content-Type: application/json" \
        -d "{
          \"developer\": \"$GITLAB_USER_LOGIN\",
          \"issue_id\": \"$CI_MERGE_REQUEST_IID\",
          \"pr_number\": ${CI_MERGE_REQUEST_IID:-0},
          \"merge_commit_sha\": \"$CI_COMMIT_SHA\",
          \"merged_at\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"
        }"
```

> Populate `issue_id` with your ticket reference where you have it (e.g. the
> Jira/issue key from the MR title or a `TIER_ISSUE` variable). Add
> `additions` / `deletions` / `changed_files` from `git diff --shortstat` if you
> want the size heuristic, or send an explicit `weight`.

### Bitbucket Pipelines (`bitbucket-pipelines.yml`)

Runs on the main branch after merge; Bitbucket exposes the commit and PR context
as `BITBUCKET_*` variables.

```yaml
pipelines:
  branches:
    main:
      - step:
          name: Record TIER outcome
          image: curlimages/curl:latest
          script:
            - >
              curl -sS --fail -X POST "$TIER_HOST/api/v1/outcomes"
              -H "Authorization: Bearer $TIER_API_TOKEN"
              -H "Content-Type: application/json"
              -d "{
                \"developer\": \"$BITBUCKET_STEP_TRIGGERER_UUID\",
                \"issue_id\": \"$BITBUCKET_PR_ID\",
                \"pr_number\": ${BITBUCKET_PR_ID:-0},
                \"merge_commit_sha\": \"$BITBUCKET_COMMIT\",
                \"merged_at\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"
              }"
```

> `BITBUCKET_STEP_TRIGGERER_UUID` is a stable identity but not human-readable;
> map it to a canonical developer via `POST /api/v1/developer_alias`, or send a
> friendlier identifier your pipeline already has.

## Verifying

After posting, the developer should appear in the scores API:

```sh
curl -sS "https://<tierd-host>/api/v1/scores" \
  -H "Authorization: Bearer $TIER_API_TOKEN" | jq '.developers[] | {developer, weighted_points}'
```

`weighted_points` reflects the outcome's `weight × quality`. Re-post the same
`merge_commit_sha` and you get `{"status":"duplicate"}` with no change to the
count — confirming replay safety.
