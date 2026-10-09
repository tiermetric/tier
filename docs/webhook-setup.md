# Webhook setup

TIER records PR outcomes from a GitHub webhook. Without it, `tierd serve` still
captures cost, but it computes no full TIER scores because it has no outcomes.

## Configure the webhook in GitHub

Repository (or organization) **Settings → Webhooks → Add webhook**:

- **Payload URL:** `https://<your-server>/webhook/github` — `<your-server>` must be
  a hostname GitHub can reach from the internet (a public server, or a tunnel such
  as ngrok or Tailscale funnel in front of a laptop). GitHub cannot reach
  `127.0.0.1`; on a laptop, skip the webhook and pick up newly merged PRs with
  `tierd backfill` instead: stop `serve`, re-run `backfill`, then start `serve`
  again (the database takes one writer at a time).
- **Content type:** `application/json`
- **Secret:** a password you generate once and give to both sides. If you
  already made `~/.tier/webhook-secret` in the README steps, just `cat` it —
  don't make a second one. Otherwise make it and keep it in a file:

  ```sh
  mkdir -m 700 -p ~/.tier
  [ -s ~/.tier/webhook-secret ] || (umask 077; openssl rand -hex 32 > ~/.tier/webhook-secret)
  cat ~/.tier/webhook-secret        # paste this value into GitHub's Secret box
  ```

- **Which events?** Choose **"Let me select individual events"** and tick
  **Pull requests**, **Pushes**, and **Workflow runs**.

  TIER processes exactly three event types (`internal/webhook/handler.go`):

  - `pull_request` — a closed + merged PR becomes an outcome (quality `1.0`).
    Its timestamp comes from `pull_request.merged_at`, with receipt time used
    only when that value is missing, malformed (including zero), or in the future.
    Delayed or redelivered events therefore land in the merge's reporting window,
    and the CI and revert observation windows start at the merge time.
  - `workflow_run` — a CI **failure** on the merge commit within the 48h window
    floors that outcome's quality to `0.7`; a success records a clean CI signal.
    **If you omit "Workflow runs" the CI-fail quality signal is silently
    disabled** — an outcome that broke CI still scores as a clean `1.0`.
  - `push` — revert detection applies to pushes to the repository's **default
    branch only** and degrades the reverted change's quality: a
    code-problem (quality) revert floors to `0.1`, a business-decision
    (strategic) revert to `0.8`, within a 60-day window.

  Every other event is ignored, so subscribing to more only adds noise.

## Configure the secret on the tierd side

Provide the same secret to `tierd serve`, ideally without putting it on the
command line:

```sh
export TIER_WEBHOOK_SECRET=@$HOME/.tier/webhook-secret   # read from a file
# or:  ./bin/tierd serve --webhook-secret @$HOME/.tier/webhook-secret ...
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
- **Give `tierd backfill` the same `--config`.** Backfill (rebuilding outcomes from past
  merged PRs) reads `outcomes.size_labels` from its `--config` file (#301), so old and
  live outcomes get the same weights. Without `--config`, backfill uses the built-in
  table above and prints a warning saying so. Backfill never re-weighs a PR that is
  already stored: it skips any merge commit it has seen before.

Work-type labels (`#187`) are a deliberately fixed taxonomy and are **not**
configurable; this applies to *size* labels only.

## Delivery semantics

- **Signature:** `X-Hub-Signature-256: sha256=<hmac>` — required; a mismatch is
  `403`.
- **Dedup:** replays are deduped by the `X-GitHub-Delivery` GUID (combined with
  the event type), and merged-PR outcomes carry a durable merge-commit-SHA
  guard, so a redelivery is a safe no-op.
- **Failures:** a successful (including duplicate) delivery returns `204`. A
  failed one returns `503` (the database stayed locked past its 5-second wait)
  or `500` (any other error, which can also be a lock held past that wait), or
  times out with no response. Redeliver it either way: **GitHub does not
  redeliver a failed delivery on its own**, so that event is lost until you
  redeliver it — see
  [Redelivering a failed delivery](#redelivering-a-failed-delivery).

## Redelivering a failed delivery

GitHub can redeliver only deliveries from the past 3 days, so act promptly. A
redelivery carries the same `X-GitHub-Delivery` GUID, and tierd
reprocesses it because a failed delivery is never recorded as seen.

**In the GitHub UI:** open the repository (or organization) **Settings →
Webhooks**, click the TIER webhook's URL, open the **Recent Deliveries** tab,
click a failed delivery (red icon), and click **Redeliver**.

**With the REST API** (for example with the `gh` CLI, signed in as a repository
admin; for an organization webhook, see below):

```sh
# 1. The hook id: the number at the end of the webhook's settings page URL
#    (.../settings/hooks/<hook_id>), or from this list.
gh api repos/<owner>/<repo>/hooks --jq '.[] | {id, url: .config.url}'

# 2. The failed deliveries, including ones that timed out or got no response.
#    `id` is the delivery id used below; `guid` is the X-GitHub-Delivery value
#    tierd logs as `delivery`.
gh api --paginate 'repos/<owner>/<repo>/hooks/<hook_id>/deliveries?per_page=100' \
  --jq '.[] | select(.status != "OK") | {id, guid, event, status, status_code, delivered_at}'

# 3. Redeliver one of them.
gh api -X POST repos/<owner>/<repo>/hooks/<hook_id>/deliveries/<id>/attempts
```

For an organization webhook, replace `repos/<owner>/<repo>` with `orgs/<org>`.
This needs an organization owner and the `admin:org_hook` token scope, which a
default `gh auth login` does not grant: add it with
`gh auth refresh -s admin:org_hook`.

## Verifying it works

You can compute a signature by hand and post a synthetic merged-PR payload
(the key file must be the same file given to `tierd serve --webhook-secret`):

```sh
PAYLOAD='{"action":"closed","pull_request":{"number":42,"merged":true,"merge_commit_sha":"abc123","user":{"login":"alice"},"body":"closes #42","labels":[{"name":"size/m"}],"additions":10,"deletions":2,"changed_files":3}}'
SIG="sha256=$(printf '%s' "$PAYLOAD" | perl -MDigest::SHA=hmac_sha256_hex -e '
  local $/;
  open my $fh, "<", "$ENV{HOME}/.tier/webhook-secret" or die $!;
  my $key = <$fh>;
  $key =~ s/[\r\n]+\z//;
  die "empty webhook secret\n" unless length $key;
  print hmac_sha256_hex(scalar <STDIN>, $key);
')"
[ "$SIG" != "sha256=" ] || { echo "could not compute a signature: check ~/.tier/webhook-secret" >&2; false; } &&
curl -i -X POST https://<your-server>/webhook/github \
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
