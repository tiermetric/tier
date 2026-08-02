# Webhook setup

TIER records PR outcomes from a GitHub webhook. Without it, `tierd serve` still
captures cost, but it computes no full TIER scores because it has no outcomes.

## Configure the webhook in GitHub

Repository (or organization) **Settings → Webhooks → Add webhook**:

- **Payload URL:** `https://<tierd-host>/webhook/github`
- **Content type:** `application/json`
- **Secret:** the value of `TIER_WEBHOOK_SECRET`. Generate one with:

  ```sh
  openssl rand -hex 32
  ```

- **Which events?** Choose **"Let me select individual events"** and tick
  **Pull requests**, **Pushes**, and **Workflow runs**.

  TIER processes exactly three event types (`internal/webhook/handler.go`):

  - `pull_request` — a closed + merged PR becomes an outcome (quality `1.0`).
  - `workflow_run` — a CI **failure** on the merge commit within the 48h window
    floors that outcome's quality to `0.7`; a success records a clean CI signal.
    **If you omit "Workflow runs" the CI-fail quality signal is silently
    disabled** — an outcome that broke CI still scores as a clean `1.0`.
  - `push` — revert detection degrades the reverted change's quality: a
    code-problem (quality) revert floors to `0.1`, a business-decision
    (strategic) revert to `0.8`, within a 60-day window.

  Every other event is ignored, so subscribing to more only adds noise.

## Configure the secret on the tierd side

Provide the same secret to `tierd serve`, ideally without putting it on the
command line:

```sh
export TIER_WEBHOOK_SECRET=@/etc/tier/webhook-secret   # read from a file
# or:  ./bin/tierd serve --webhook-secret @/etc/tier/webhook-secret ...
```

**Fail-closed:** if no secret is set, the `POST /webhook/github` route is **not
mounted at all** — an unauthenticated webhook would let anyone who can reach the
listener forge merged-PR outcomes or fake revert pushes. You will see this at
startup:

```
WARN TIER_WEBHOOK_SECRET is not set — POST /webhook/github is disabled (fail closed, #60)
```

Every delivery is verified with an HMAC-SHA256 signature over the raw body,
compared against the `X-Hub-Signature-256` header. A mismatch returns `403`.

## PR size labels and outcome weight

When a merged PR carries a size label, TIER weighs the outcome from that label
(`weight_source='label'`) instead of the diff-size heuristic. Out of the box it
recognizes GitHub's `size/xs`..`size/xl` labels and the bare `xs`..`xl` forms,
mapped onto the fixed outcome scale:

| Label (case-insensitive)        | Weight |
| ------------------------------- | ------ |
| `size/xs`, `xs`                 | 0.5    |
| `size/s`, `s`                   | 1      |
| `size/m`, `m`                   | 3      |
| `size/l`, `l`                   | 5      |
| `size/xl`, `xl`                 | 8      |

A PR whose labels don't match any entry falls through to the git-diff heuristic
(`weight_source='git-heuristic'`) — nothing is lost, but a label is the more
trustworthy signal.

### Remapping the label names (`outcomes.size_labels`)

If your org uses a different naming convention — `size: M` with a space, `size-l`,
or an `S`-prefixed scheme common to pull-request-size bots — remap the names in the
YAML config (`tierd serve --config`):

```yaml
outcomes:
  size_labels:
    "xs": 0.5
    "s": 1
    "m": 3
    "l": 5
    "xl": 8
    "xxl": 8   # extra names are fine — they just map onto the same fixed scale
```

Rules:

- **Only the names are configurable.** Every weight must be one of the fixed scale
  `0.5, 1, 3, 5, 8` so scores stay comparable across orgs — an off-scale value (or
  a blank label name) makes `tierd serve` fail loud at startup.
- **Matching is case-insensitive.**
- **A custom table replaces the built-ins** (it does not merge) — list every label
  you want recognized, including `size/*` forms if you still use them.
- **Absent or `{}`** keeps the built-in table above unchanged, so existing
  deployments are unaffected.
- A configured match still records `weight_source='label'` — no new provenance.
- **Live webhook path only (today).** `outcomes.size_labels` is honored when TIER scores
  a merged PR via the live webhook. `tierd backfill` (reconstructing historical outcomes)
  currently uses the **built-in** table, so an org with a custom map will see custom
  weights on live outcomes and built-in weights on backfilled ones. Unifying the two is
  tracked in #301; until then, run backfill before customizing, or expect that divergence
  on pre-config history.

Work-type labels (`#187`) are a deliberately fixed taxonomy and are **not**
configurable; this applies to *size* labels only.

## Delivery semantics

- **Signature:** `X-Hub-Signature-256: sha256=<hmac>` — required; a mismatch is
  `403`.
- **Dedup:** replays are deduped by the `X-GitHub-Delivery` GUID (combined with
  the event type), and merged-PR outcomes carry a durable merge-commit-SHA
  guard, so a redelivery is a safe no-op.
- **Retries:** a transient handler failure returns `500`, which prompts GitHub to
  retry. A successful (including duplicate) delivery returns `204`.

## Verifying it works

You can compute a signature by hand and post a synthetic merged-PR payload:

```sh
SECRET=your-secret
PAYLOAD='{"action":"closed","pull_request":{"number":42,"merged":true,"merge_commit_sha":"abc123","user":{"login":"alice"},"body":"closes #42","labels":[{"name":"size/m"}],"additions":10,"deletions":2,"changed_files":3}}'
SIG="sha256=$(printf '%s' "$PAYLOAD" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $2}')"
curl -i -X POST https://<tierd-host>/webhook/github \
  -H "X-GitHub-Event: pull_request" \
  -H "X-GitHub-Delivery: $(uuidgen)" \
  -H "X-Hub-Signature-256: $SIG" \
  -H "Content-Type: application/json" \
  -d "$PAYLOAD"
# expect HTTP 204
```

GitHub's **"Redeliver"** button on any recorded delivery does the same against a
real payload.

## Troubleshooting

- **"PR merged but no outcome recorded."** Almost always no issue reference was
  found on the branch or in the PR body, so the outcome could not be attributed
  to an issue. Check [conventions.md](conventions.md) — the branch name or PR
  body must carry a recognizable issue reference.
- **Every delivery returns 403.** Signature mismatch: the secret configured in
  GitHub differs from `TIER_WEBHOOK_SECRET` on the server.
- **Every delivery returns 404.** No secret is set on the server, so the
  `POST /webhook/github` route is not mounted at all (fail-closed). Set
  `TIER_WEBHOOK_SECRET` and restart.
