# Privacy

TIER is built so that adopting it does not mean handing over your source code or
your prompts. This document states exactly what TIER reads, what it stores, and
what it never touches — each claim grounded in the code so a security reviewer
can audit it.

## The core guarantee

> **TIER never reads, transmits, or stores prompt or completion content.** Every
> capture path parses with an allowlist. The Claude Code JSONL parser deserializes
> only `type`, `timestamp`, `gitBranch`, `cwd`, `sessionId`, and
> `message.{id,model,role,usage.*}` — token counts, not text; the proxy parsers
> extract only `id`/`model`/`usage` from response bodies; and the Codex
> rollout-log parser reads six fields, none of which is content.

## Why the guarantee holds

TIER parses inputs with **allowlist structs**. Only the fields named in the
struct are deserialized; every other JSON field — including any prompt or
completion text — is silently dropped by the JSON decoder and never enters
memory as a named value, let alone the database.

- **JSONL capture** (`internal/collector/jsonl.go`, the `jsonlEntry` struct):
  reads `type`, `timestamp`, `gitBranch`, `cwd`, `sessionId`, and under
  `message`: `id`, `model`, `role`, and the `usage` token counters
  (`input_tokens`, `output_tokens`, `cache_creation_input_tokens`,
  `cache_read_input_tokens`, and the 5m/1h cache-creation split). There is **no
  content or text field** in the struct.

- **Codex rollout-log capture** (`internal/collector/codexrollout/parse.go`, the
  `rolloutLine` struct, #464): reads exactly six fields — the line `type` and
  `timestamp`, and under `payload`: `id` (the session UUID), `cwd`, `git.branch`,
  `model`, plus the `token_count` event's `info.total_token_usage` counters.
  Codex rollout logs contain a great deal more than that — the agent's full system
  prompt (`base_instructions`), your prompts, the model's messages and reasoning
  traces, tool output, and patch bodies — and **none of it is deserialized**: those
  fields have no counterpart in the struct, so the JSON decoder discards them. The
  `cwd` is read to decide which repository a session belongs to (a session outside
  every watched repo is dropped) and is **not stored** by this path — and, since it
  is never stored, it is never forwarded either, including by `tierd ship
  --codex-rollout` (#492). What that flag sends is the same usage-event shape as
  every other collector, listed under "Where it goes" below; enabling Codex capture
  adds no new *category* of data to what leaves the machine.

- **Reverse-proxy capture** (`internal/proxy/proxy.go`): the provider response
  parsers read only the message `id`, the `model`, and the `usage` token counts
  from the response body. The response is passed through to the client
  byte-for-byte; TIER extracts usage and forwards the rest untouched.

Because the parsers are allowlists, adding no new fields is the default —
capturing content would require deliberately adding a content field, which does
not exist.

## What IS stored

TIER stores, in a **single local SQLite file** with **no external
transmission**:

- **Developer identifier** — an OS username (JSONL) or GitHub login (PR
  outcomes), and any alias→canonical mapping you configure.
- **Issue id** — derived from the branch name or PR/commit body (e.g.
  `issue-42`, `TIER-99`).
- **Model names** — e.g. `claude-opus-4-8`.
- **Token counts** — input, output, and the cache read/write counters.
- **Cost** — list-price cost in integer micro-dollars, computed from the token
  counts via a versioned reference price table.
- **Session id** (`token_events.session_id`, #238) — the **opaque agent session
  UUID** an event belongs to (Claude Code's `sessionId`, or the Codex rollout
  log's `session_meta.id`), so context-bloat and rework-loop token-waste can be
  diagnosed at session grain. It is a random identifier only; it **carries no
  prompt content, no completion text, and no file contents**, and is **NULL** for
  rows a session-blind producer (the reverse proxy or an Admin-API poller)
  captured.
- **PR metadata** — PR number, author login, size-derived weight, quality, and
  the merge commit SHA.
- **Watcher tail-state** (**live watch mode only** — `--watch-repo` or the
  `watch.repos` config key) — when live ingestion is enabled, TIER persists one
  operational checkpoint row per watched
  JSONL file (`watcher_checkpoint`) so a restart resumes tailing from the last
  parsed byte instead of re-reading each file from the start. Each row's
  `metadata` column holds a small JSON blob describing the session being tailed:
  the **absolute working-directory path** (`cwd`), the **git branch** (and, when
  an isolated-agent session inherits one, the most recent human-named git branch
  seen so far in that file), and the **session id** (a UUID), plus the session
  start time, the first-observed model name, and an internal parse-sequence
  counter. Nothing here is captured unless you run the watcher; a one-shot
  `tierd score` writes no checkpoint.
- **Raw GitHub webhook payloads** (**only when the GitHub webhook path is
  enabled** — i.e. you set `webhook_secret` / `TIER_WEBHOOK_SECRET` and point
  GitHub deliveries at `POST /webhook/github`; that endpoint is fail-closed and
  stays disabled without a secret) — for each *processed* delivery
  (`pull_request`, `push`, `workflow_run`), TIER retains the **raw GitHub webhook
  request body**, gzipped, in the `webhook_payloads` table so a score input can
  be re-derived months later (#137). Unlike every token-capture path above, this
  raw body **does contain personal data**: commit author **names and email
  addresses**, **PR titles and descriptions**, and **commit messages** — whatever
  GitHub places in a standard webhook payload (up to a 1 MB body). It remains
  **not prompt text, not completion text, and not the contents of your source
  files** — it is the same PR/commit metadata GitHub itself already stores.
  Retention is **bounded**: rows are pruned at process start (`Open()`) by two
  limits — a **90-day age cap** and a **50,000-row cap** (oldest evicted first),
  enforced by `PruneWebhookPayloads` (`internal/store/audit.go`). Both bounds are
  currently compile-time constants (`webhookPayloadRetentionDays` /
  `webhookPayloadMaxRows` in `internal/store/audit.go`), **not config keys** —
  there is no setting to change the window or cap without a code change, and no
  flag to keep the webhook path enabled while suppressing retention. The one
  operator control is the path itself: **to store none of this, run TIER without
  the webhook path** (JSONL capture and `tierd ship` never populate
  `webhook_payloads`).

None of this is prompt text, completion text, code, file contents, or diffs — the
raw webhook payloads included: they carry the PR/commit metadata GitHub already
stores (names, emails, titles, messages), never your prompts or the contents of
your files. The `session_id` above is likewise an **opaque UUID** — it groups a
session's events without persisting anything the session said or produced. Two of the entries above are conditional and hold data an auditor
should note. The watcher tail-state is **derived operational state, safe to delete
at any time** — the watcher self-heals from the live file on the next change — and
it too contains no prompt or completion text and no file contents; its `cwd` is an
absolute directory *path* (a filesystem location, not the contents of any file),
which can reveal an OS username or an internal project name. The raw webhook
payloads are the one place TIER holds third-party personal data (contributor names
and emails) **at rest**, bounded to ~90 days. Because of both, operators should
treat the local database file (mode `0600`, single-tenant) accordingly — and may
decline the webhook path entirely to store none of the webhook PII.

## Data-subject rights: access and erasure (GDPR Art. 15 / Art. 17)

Because TIER stores personal data keyed to a named developer, it ships two
bearer-gated (write/admin-scoped) admin endpoints so an operator can honour a
data-subject access or erasure request without hand-editing the database (#184):

- **Erasure** — `DELETE /api/v1/developer/{id}` deletes **every** stored row for
  that person in **one transaction** (all-or-nothing). It first resolves `{id}`
  through the alias map (single-hop) and then removes rows stored under the
  canonical id *or* any raw login that aliases to it, across **all** developer-PII
  tables:

  | Table | What it holds |
  |---|---|
  | `token_events` | per-developer token counts and list-price cost |
  | `outcomes` | per-developer PR/issue outcomes, weight, quality |
  | `actual_spend` | per-developer actual-paid amounts |
  | `org_hierarchy` | the developer's team/division/org |
  | `period_membership` | the developer's org-membership windows |
  | `quality_events` | per-developer CI/revert quality signals |
  | `quality_history` | per-developer quality-transition log |
  | `repo_repair_audit` | which repositories a `tierd repair-repo` run moved this developer's stored spend into |
  | `developer_alias` | the developer's alias→canonical mappings |

  The companion `repo_repair_row_audit` ledger is **not** listed because it holds
  no personal data: only a repair id, a row id, and the before/after repository
  slug. It deliberately does not copy the resolving `session_id`, precisely so
  that an audit record designed to outlive the row it describes cannot become
  personal data that survives an erasure.

  **All five audit ledgers** — `reprice_audit`, `reprice_row_audit`,
  `repo_repair_audit`, `repo_repair_row_audit`, `cost_correction_audit` — are
  **append-only by construction: no code path mutates them; the tables
  themselves are not protected against direct database access.** One of them
  carries more than that, and it is narrow: `cost_correction_audit` (the
  money-rewrite ledger) refuses `UPDATE` at the schema level, so no code path
  can **silently** rewrite a recorded correction — a mutation would have to
  drop the trigger first. `DELETE` is deliberately **not** refused on any of them — erasure
  (below) and future retention/GC are lawful deletes, and no database
  constraint can tell a lawful delete from tampering. None of this is
  tamper-proofing against someone holding the database file.

  The `cost_correction_audit` ledger (#346, the sanctioned `POST /costs`
  cost-correction override) follows the SAME pattern and is **also not
  listed**: it holds no `developer` column, and — after an earlier draft got
  this backwards — deliberately does **not** copy the correction's
  `idempotency_key` either, precisely because that field is often
  client-chosen and could embed personal data (an email, a ticket
  reference), and this ledger is designed to outlive the row it describes.
  It carries only a server-generated `token_event_id` (which simply stops
  resolving once the row it names is erased or pruned — correct, not a bug)
  plus `old_cost_micro`/`new_cost_micro` and the operator-supplied `actor`/
  `reason` that explain the correction. `actor`/`reason` name the OPERATOR
  who made the correction, not the data subject whose spend was corrected —
  the same third-party-identifier class as the `webhook_payloads` residual
  below — so they are not erased by this endpoint. Operators must not put a
  data subject's own name or email in `reason`; nothing technical enforces
  that today.

  ⚠️ **`actor` is a self-asserted claim, not a verified identity — the audit
  trail records who the caller says they are.** It is unvalidated free text
  the caller chooses (length-capped only), and nothing checks it against the
  credential that made the request. Nothing can today: `POST /api/v1/costs` is
  gated — when a token is configured at all — by a single global write token
  with no subject, so there is no
  principal for the server to record instead. Treat every `actor` value as a
  claim to be corroborated, not as an attribution. Binding it to a verified
  principal requires the identity layer tracked as #65.

  The same holds for the corrected row's `developer`: nothing binds the
  authenticating principal to it either, which is the half that matters for
  data accuracy (Art. 5(1)(d)). What *is* bounded is **reach** —
  `POST /api/v1/costs` forces `source="api"` and the correction path compares
  it, so an override can only ever touch a manually imported row.
  Automatically captured spend cannot be corrected through this endpoint at
  all.

  The erasure endpoint returns per-table deleted-row counts and is
  **idempotent** (a second call, or a call for an unknown developer, deletes
  nothing and returns `404`).

- **Access (DSAR)** — `GET /api/v1/developer/{id}/export` returns every stored
  row for the same resolved identifier set as JSON — the portable artifact you
  hand to the data subject.

**Known residual — `webhook_payloads`.** The structured erasure above does **not**
rewrite the raw GitHub webhook bodies retained in `webhook_payloads` (only present
when the webhook path is enabled). As described in *What IS stored* above, those
raw bodies may embed a contributor's name or email address. Surgically editing a
gzipped raw audit body to redact one identifier is out of scope for the erasure
endpoint; the **mitigation is the retention bound** — every such row is pruned at
process start by a 90-day age cap and a 50,000-row cap (`PruneWebhookPayloads`),
so any identifier embedded there ages out within ~90 days. An operator who must
guarantee immediate removal of that residual can run TIER without the webhook path
(which stores none of it) or delete the affected `webhook_payloads` rows directly.
`org_actual_spend` is org-level (no developer column) and holds no personal data,
so it is correctly outside the erasure/export scope.

## Where it goes

Nowhere by default. The database is a local file (SQLite). The only outbound
network traffic TIER makes is:

- the **reverse proxy** forwarding your API calls to the upstream provider you
  configured (Anthropic / OpenAI), which happens only if you route traffic
  through it; and
- `tierd ship` forwarding **captured usage events** (the fields listed above,
  never content) to a central `tierd` you operate — from Claude Code sessions by
  default, and from Codex rollout logs as well when `--codex-rollout` is passed.
  Both produce the same event shape; neither forwards a `cwd` or any content.

TIER does not call home, and it does not transmit your data to the project
maintainers or any third party.

## Legal deployment guidance

This document bounds what TIER stores as *content*. It does not resolve the
*labor-law* question: per-developer cost attribution is personal data about a
named individual, and in TIER's EU target markets (DE Betriebsrat
co-determination, FR/NL works councils, GDPR Art. 22 profiling) measuring or
ranking named individuals is co-determined or restricted. For deployment
guidance — works-council prerequisites, DPIA pointers, and a team-only vs
per-developer decision table by jurisdiction — see
[legal-and-privacy.md](legal-and-privacy.md). (Positioning guidance, not legal
advice.)
