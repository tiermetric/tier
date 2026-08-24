# TIER `/api/v1` compatibility contract

This document is the **stability contract** for TIER's HTTP API. It pins, for
every `/api/v1` endpoint: its stability guarantee, its response schema, and the
project's rules for what may change without a version bump and what may not.

It exists because a JSON field's **meaning** can change while it stays
parseable, and nothing in the wire bytes announces it. #187 demoted the
top-level `/scores` `developers`/`teams`/`total` fields to a pooled back-compat
summary that is explicitly **not** a cross-type ranking, with no version signal and no
changelog a consumer could detect. External orgs scripting against `/scores`
kept computing a comparison the project itself now calls a category error. This
contract makes that class of change **announced and detectable** instead of
silent.

The Go response structs in `internal/api/` are the authoritative field
definitions; this document must be kept in step with them (see
[Changing the API](#changing-the-api)).

- **Base path:** `/api/v1` (the Prometheus scrape at `/metrics` is the one
  documented exception, mounted at the root).
- **Scope:** single-process, single-tenant. There is no `/v2` today.
- **Out-of-scope HTTP surfaces:** the inbound `POST /webhook/github` seam (mounted
  at the root when `TIER_WEBHOOK_SECRET` is set) and the dashboard UI at `/` are
  **not** part of this `/api/v1` contract — the webhook is a GitHub-governed wire
  seam (see the JSONL ingestion contract) and the dashboard is UI, both governed
  separately.
- **Encoding:** UTF-8 JSON unless noted. The two bulk exports also emit CSV via
  `Accept: text/csv`.

## Feature detection

**The supported feature-detection mechanism is the `version` field of
`GET /api/v1/livez`.** It is the build version the binary was compiled with
(injected via `-ldflags`; falls back to `"dev"`). A client that needs to know
whether a server supports a given field or endpoint should read `livez.version`
rather than probing behaviour. `livez` is unauthenticated (a liveness probe must
not need credentials), so feature detection never requires a token.

### Identifying a deployment: `GET /api/v1/version`

Feature detection asks *"what can this server do?"* — a different question from
*"is this the build I published?"* For the second, use `GET /api/v1/version`,
also unauthenticated and also mounted in read-only mode.

> 🔴 **Not available in any release published so far.** This route was added after
> the most recent release was cut, so a server running **v0.4.0 or earlier answers
> `404` here** — verified against the published `0.4.0` container image. The
> response shown below is what a build *carrying* the route returns; it is not
> something v0.4.0 can produce.
>
> ⚠️ **`404` on this path does not mean the server is unhealthy or the wrong
> build** — it most likely means the build predates the route. Until your target
> ships it, identify a deployment with `GET /api/v1/livez`, which has always
> carried `version`. Note that reports the *release*, not the *build*: two
> binaries from a moved tag share a version string, which is exactly why this
> route exists.

```json
{
  "version": "0.4.0",
  "commit": "ca27d9f07c0838da15595e9fad97047b389866bd",
  "modified": false,
  "go_version": "go1.26.5",
  "platform": "linux/amd64",
  "price_table": { "version": 9, "effective_date": "2026-07-26" }
}
```

`commit` and `modified` come from the VCS stamps the Go toolchain embeds
automatically; they are absent (`commit` omitted, `modified: false`) on a binary
built with `-buildvcs=false`. `commit` is also carried on `livez` — additively,
so an existing liveness probe is unaffected.

🔴 **Why `version` alone is not enough, and why `price_table.version` is a trap.**
A tagged release reports the same `version` string however it was built, so two
binaries from a moved tag are indistinguishable by it; `commit` is what makes
"this deployment is the build I published" an assertion rather than a hope. And
`price_table.version` bumps only when *prices* change: measured 2026-08-06, a
deployment reported price table `9` while the newest source was also `9` — while
the running binary was a **full release behind**. It is useful next to the build
(it answers "which rates priced these numbers"), never as identity.

⚠️ **A version check is only meaningful if it can say NO.** Assert the expected
`commit`, and make sure your check fails when pointed at a different build —
otherwise "the version matches" is indistinguishable from "the check never ran".

Do **not** rely on the presence or absence of an individual response field for
feature detection at runtime: additive fields (below) can appear at any release,
and `omitempty` fields are absent whenever their value is empty even on a server
that fully supports them.

## Query parameters are STRICTLY validated on the scores endpoints (#590)

`GET /api/v1/scores`, `GET /api/v1/scores/{developer}` and
`GET /api/v1/scores/compare` **reject any query parameter they do not implement
with `400`**, naming the offending parameter and listing what is accepted.
Matching is exact and case-sensitive: `?Repo=` is not a synonym for `?repo=`, it
is an error.

> ⚠️ **This is a deliberate behavioural break, and it is recorded as one rather
> than filed under "additive".** Before #590 these endpoints silently ignored
> unrecognized parameters, as `net/http` does by default. A client that sends an
> extraneous parameter and previously got `200` now gets `400`.

It was taken because the alternative is worse than a break. `/api/v1/scores`
accepted `?repo=` and **ignored it**, returning a whole-installation aggregate
byte-for-byte identical to a correctly scoped one. A caller who believed they had
scoped a query received a figure spanning every repository, with no way to detect
it from the response. Adding the filter without this strictness would have left
the same trap one keystroke away -- `?repos=`, `?Repo=`, `?repo_id=` would each
silently widen a query back to installation-wide while the caller's own assertion
passed, asserting nothing.

The invariant, stated once: **"could not scope" must never share a response shape
with "scoped, and this is the result."** A silent wrong answer is worse than an
error.

This extends to query strings the strictness the write endpoints have always applied
to request bodies (`DisallowUnknownFields()`, see [the compatibility rule](#the-compatibility-rule)).
It is a **request-side** break, which is why it ships on `/v1` rather than forcing a
`/v2`: rules 2 and 3 above govern the RESPONSE schema, where a consumer's parsing
breaks silently. A rejected request fails loudly at the caller, in the one place the
mistake can still be corrected.

**What a client should do:** send only documented parameters, and detect scoping
from the **response** (`data_quality.repo_scope`), never from the request it
sent. Feature-detect via `livez.version` as always.

## The compatibility rule

**JSON responses are additive-only. A field's semantics are frozen once
shipped.**

1. **Adding a field is additive.** A new response field may be added at any
   release. Clients MUST ignore unknown fields.
2. **Renaming or removing a field is BREAKING.** It is not a refactor. It
   requires a new field name (additive) or a `/v2` (breaking), never a
   rename-in-place.
3. **Changing a field's SEMANTICS is BREAKING even when the type and name are
   unchanged.** If the number a field carries comes to mean something different
   -- a different population, unit, scope, or definition -- you MUST either
   introduce a **new field name** for the new meaning or bump to `/v2`. You may
   not repurpose an existing field in place. This is the #187 rule: the demotion
   should have shipped the new meaning under a new key (it did -- `work_types`)
   and the change to the OLD keys' meaning should have been **announced** (it
   was not).
4. **Closed-set (enum) values are additive too.** A new allowed value for a
   string enum (`source`, `fidelity`, `weight_source`, `work_type`,
   `work_type_source`, `billing_mode`, watcher `status`) may be appended; an
   existing value may not be renamed or removed.
5. **CSV column order is append-only.** The four exports
   ([`GET /events`](#get-apiv1events), [`GET /outcomes`](#get-apiv1outcomes),
   [`GET /quality_events`](#get-apiv1quality_events),
   [`GET /quality_history`](#get-apiv1quality_history))
   carry a positional CSV contract. Columns are only ever **appended** at the
   end, never reordered or removed, and an existing column's VALUES never change
   shape (`issue_id` stayed `"issue-42"` when `repo` arrived in its own new
   column, #231). This restates the contract already enforced in
   `internal/api/export.go` (`eventsCSVHeader`, `outcomesCSVHeader`,
   `qualityEventsCSVHeader`, `qualityHistoryCSVHeader`) and documented in
   `docs/how-it-works.md`.

Request bodies are strict in the other direction: the write endpoints decode
with `DisallowUnknownFields()`, so an **unknown request field is rejected 400**.
Adding a new **optional** request field is additive (old clients omit it); making
a previously-optional field required, or adding a required field, is breaking.

### Additive vs breaking at a glance

| Change | Classification |
|---|---|
| Add a new response field | Additive |
| Add a new endpoint | Additive |
| Add a new optional request field | Additive |
| Append a new CSV column at the end | Additive |
| Append a new enum value | Additive |
| Rename a response field | **Breaking** |
| Remove a response field | **Breaking** |
| Change a field's meaning/unit/scope in place | **Breaking** |
| Reorder or remove a CSV column | **Breaking** |
| Change an existing CSV column's value shape | **Breaking** |
| Add a required request field / make an optional one required | **Breaking** |
| Change a success status code | **Breaking** |
| Reject a previously-ignored query parameter | **Breaking** (see [#590](#query-parameters-are-strictly-validated-on-the-scores-endpoints-590)) |

## Announcing a deprecation or a semantic change

When a field or endpoint is being retired, or a field's meaning is changing
(which ships as a new field plus a deprecation of the old one), the change is
announced two ways:

1. **The `Deprecation` (RFC 9745) and `Sunset` (RFC 8594) response headers** on
   the affected endpoint. `Deprecation` carries the date the field/endpoint
   became deprecated; `Sunset` carries the date after which it may be removed (a
   `/v2` or a removal). Together they **replace** the single RFC 7234
   `Warning: 299` header precedent on `POST /costs` (deprecating
   `cache_write_tokens`): `Warning` was deprecated by RFC 9111 and is dropped by
   some intermediaries, so it is not a reliable deprecation signal.
   `Warning: 299` remains only for the one already-shipped `cache_write_tokens`
   case for back-compat; new deprecations use the `Deprecation`/`Sunset` pair.
2. **An entry in the [API changelog](#api-changelog) below**, naming the field,
   the change, the release, and the issue.

A **semantic demotion** (a field's meaning narrowing or changing, like #187)
MUST additionally:

- ship the new meaning under a **new field name** (never repurpose the old
  field), and
- document, in the old field's changelog entry and its Go doc comment, exactly
  what it now means and what a consumer should read instead.

## Endpoint catalog

Auth scopes (see `internal/api/handler.go`):

- **write** -- requires the write/admin bearer token (`Authorization: Bearer`).
  The read-only viewer token is rejected 403.
- **read** -- satisfied by EITHER the read-only viewer token or the write token.
- **open** -- no token (probes only; never exposes spend data).

When the server is started with no API token (loopback-only, fail-closed per
`cmd/tierd`), auth is disabled and all scopes are transparent.

Error bodies are uniformly `{"error": "<message>"}` with a 4xx/5xx status.

### Write endpoints

#### `POST /api/v1/costs`
- **Scope:** write. **Success:** `201 Created` (fresh key, or an identical/
  matching re-post), empty body -- **or** `200 OK` (a sanctioned override
  actually corrected a row, #346), body `{"corrected": true, "old_cost_usd":
  <float>, "new_cost_usd": <float>}`.
- Manual single-row cost import. Request: `costRequest` -- `developer`,
  `issue_id`, `model` (required); `input_tokens`, `output_tokens`,
  `cache_read_tokens`, `cache_write_5m_tokens`, `cache_write_1h_tokens`,
  `cost_usd`, `source`, `fidelity`, `idempotency_key` (optional);
  `override`, `override_actor`, `override_reason` (optional -- #346, see
  below).
- `cache_write_tokens` is the **deprecated** legacy single-bucket field
  (superseded by the 5m/1h split, #55); when present and non-zero it is routed
  to the 5m bucket and the response carries the legacy `Warning: 299` header.
- `source` accepts only `"api"` or omitted; `fidelity` accepts `"daily"`,
  `"estimated"`, or omitted (`"realtime"` is rejected -- reserved for automated
  capture).
- ⚠️ **`issue_id` may not be the reserved unattributed sentinel (#466).** The
  sentinel family -- the bare `unattributed` plus any `unattributed:<reason>`
  sub-bucket -- is **server-assigned**: the collector and the proxy write it when
  they genuinely could not resolve an issue. A client supplying it now gets `400`;
  it was previously written silently. The check is **case-insensitive** and
  trims surrounding whitespace, so `UNATTRIBUTED:main` is refused too -- ingest
  deliberately rejects more than the read side matches, because a case variant in
  the table would classify one way in SQL and another in Go. Ordinary ids that
  merely contain the word (`unattributed-work`, `not-unattributed`) are
  unaffected. Same rule on `POST /outcomes`; `POST /events` is the documented
  exception.
- ⚠️ **`developer` may not be the reserved unattributed sentinel either (#619).** The
  same reserved string is the sentinel for *two* columns, and this is now the same
  rule on both -- one predicate backs both checks, so they cannot drift. A client
  supplying `"developer": "unattributed"` (or `UNATTRIBUTED`, or
  `unattributed:main`, or any of them with surrounding whitespace) now gets `400`;
  it was previously written silently.

  **This half mattered more than the `issue_id` half.** TIER is
  `points / (cost/1000)`. Forging `issue_id` moves a dollar *between buckets inside
  your own denominator* and leaves your headline score unchanged. Forging `developer`
  moves it *out of your denominator entirely* -- onto the `unattributed`
  pseudo-developer -- so your cost falls and your score rises.

  ⚠️ **Scope, stated plainly:** this removes the *deniable* forgery, not the ability to
  raise your own score. TIER is single-tenant with one shared write token and a
  free-form `developer` column, so posting `"developer": "mallory-2"` still moves your
  spend out of your own row. What the sentinel added was *cover* — it is
  indistinguishable from honest server-assigned spend and lands in a bucket nobody
  audits. Binding `developer` to the authenticating credential is the control that
  would close the general case, and it is separate work.

  Ordinary identities that merely contain the word (`unattributed-bot`,
  `not-unattributed`) are unaffected, as is `unknown`, the real no-identity fallback
  the collector emits. Same rule on `POST /outcomes`, `POST /events`,
  `POST /actual_spend`, both columns of `POST /developer_alias`, and
  `PUT`/`POST /org_hierarchy`.
  Unlike `issue_id`, `developer` has **no allowlist anywhere**, including `/events`:
  the producers that legitimately assign the developer sentinel (the org-level
  Anthropic-Admin and OpenAI-Usage pollers, and the proxy's own missing-header
  fallback) write in-process and never cross an HTTP boundary, so there is nothing on
  the wire to allowlist.
- **Keyed re-post (`idempotency_key` present):** an **identical** re-post (same
  key, same cost) is idempotent -- `201`, no new row. A re-post with the same key
  but a **different** `cost_usd` is rejected with **`409 Conflict`** (#295): the
  stored cost is immutable (#233), so the correction is refused rather than
  applied. Divergence is judged on the stored **integer** `cost_micro`, so an
  honest retry whose float/FX/rounding jitter rounds to the same micro value is
  **not** a conflict. To change a recorded figure, either re-post under a
  **new** `idempotency_key`, or (#346) opt into the sanctioned override below
  (deleting a single row is operator-level, direct against the store -- there
  is no delete-by-key route).
- Dedup is keyed on `idempotency_key` alone, and (on the plain, non-override
  path) **only** a `cost_micro` divergence raises `409`. A re-post that reuses
  a key but changes a *non-cost* field (`developer`, `issue_id`, `model`,
  token counts) is still a silent no-op on those columns (they are immutable
  on conflict, #233) -- it is NOT a `409`. Reuse a key only for a genuine
  retry of the same event -- `idempotency_key` is a GLOBAL namespace across
  every producer (client-generated `/costs` keys and automated
  `MessageIdempotencyKey`-derived keys from `/events`/JSONL/proxy alike), so
  reusing someone else's key is a real, not hypothetical, way to collide with
  a row that isn't yours.
- **Sanctioned cost-correction override (#346, ruling C -- the follow-up to
  #295's ruling A above):** set `override: true` plus **required**
  `override_actor` and `override_reason` (both non-empty, `override_actor` <=
  256 chars, `override_reason` <= 1024 chars) to let a legitimate finance
  correction land on a DIVERGENT keyed re-post instead of 409ing.
  - `override` with a missing/empty `override_actor` or `override_reason`,
    or with an empty `idempotency_key`, is rejected `400` -- an override must
    be attributed and explained, never silent, and there is nothing to
    override without a key.
  - `override_actor`/`override_reason` set without `override: true` is
    rejected `400` rather than silently ignored.
  - The stored row's `(developer, issue_id, model, source, fidelity)` must
    match the request's -- a mismatch is rejected `409` (a DIFFERENT status
    detail than the plain divergent-cost 409, but the same status code).
    The 409 body deliberately does not echo which identity the key actually
    belongs to -- defense in depth against a BLIND collision, **not a secrecy
    guarantee** (see the stated trust model below). That tuple is
    exactly the set of stored columns a `/costs` request can vary; `repo`,
    `host`, `billing_mode`, `session_id`, and `ts` are forced by the endpoint
    and are therefore NOT compared. `idempotency_key` is a global namespace
    (see above), so this is a real collision case, not a theoretical one:
    without this check, reusing a copy-pasted key under a different identity
    could rewrite the WRONG row's cost.
  - ⚠️ **One row class cannot be corrected through this endpoint at all.**
    `fidelity` is INSERT-only, and #82 narrowed `/costs` to
    `daily`/`estimated`/omitted -- so a **pre-#82** `source='api'` row that
    still carries `realtime` can never be matched: stating it is a `400`,
    omitting it defaults to `estimated`, and both mismatch. Every possible
    request `409`s. The remedy is the same one #295 gives for any figure you
    cannot override: re-post under a **new** `idempotency_key`.
  - When it actually corrects a divergent row: the UPDATE touches **only**
    `cost_micro` on that one row (token counts, model, source, fidelity,
    price_version, billing_mode are all left exactly as first recorded --
    never a last-writer-wins upsert of the whole row), and an append-only
    audit row (old -> new, actor, reason) is written to the (internal,
    unexposed via any GET route today) `cost_correction_audit` table. `200`.
  - When there is nothing to correct (fresh key, or the cost already
    matches): behaves exactly like the non-override path -- `201`, no audit
    row.
  - **Reprice-safe.** `tierd reprice --commit` recomputes `cost_micro` from
    token counts, and it never reprices a `source='api'` row -- a manual
    import's cost is the caller's authoritative figure and re-deriving it from
    token counts (which may not exist) yields `$0.00`. Corrections therefore
    survive a reprice sweep, and the sweep reports how many rows it protected
    rather than omitting them silently.
- **Stated trust model for the override.** Read this before treating the
  identity check as an access control:
  - The write scope is a **single global bearer token with no subject**.
    Nothing binds the authenticating principal to the `developer` field, and
    no `tenant_id` column exists anywhere to put such a binding in -- TIER is
    single-tenant by design today.
  - The identity check therefore prevents an **accidental** key collision
    from landing a correction on the wrong row. It does **not** prevent a
    holder of the write token from deliberately correcting a row that is not
    theirs, because the tuple it compares is not secret: the read-scoped
    `GET /api/v1/events` export publishes every `idempotency_key` alongside
    its full identity tuple.
  - Deliberate misattribution by a write-token holder is **out of scope**
    until an identity layer exists (#65).
  - **What the endpoint does structurally guarantee:** it forces
    `source="api"` on every request and the identity check compares `source`,
    so the override can only ever touch a row whose stored `source` is
    `api` -- the manual-import lane. Automatically captured spend (every
    non-`api` source: `jsonl`, `proxy`, `codex-rollout`, `copilot-api`, and
    the org pollers) is unreachable from this endpoint.
  - `override_actor` is **a self-asserted claim, not a verified identity**.
    It is unvalidated free text the caller chooses, written verbatim into the
    audit ledger and the operator log. **The audit trail records who the
    caller says they are.** Nothing checks it against the credential that made
    the request, and nothing can while the write scope has no subject.
  - `cost_correction_audit` refuses `UPDATE` at the schema level (#604), so no
    code path can **silently** rewrite a recorded correction -- a mutation
    would have to drop the trigger first. `DELETE` is deliberately not
    refused -- erasure and retention are lawful deletes. This is not
    tamper-proofing against someone holding the database file.
- `503` + `Retry-After: 1` when another writer holds the database write lock.
  **Retryable, and nothing landed** -- the request runs in one all-or-nothing
  transaction that takes the write lock before it writes, so on `503` no row was
  inserted, no cost was corrected and no audit row was recorded; retry to complete
  the request. Distinguish it from the permanent statuses: a `409` means the key
  genuinely conflicts (divergent cost, or an identity mismatch) and retrying is
  pointless, and a `500` means an unexpected store failure -- including a
  read-only or full database, which is deliberately NOT reported as `503`
  because it will never clear on its own. Only the `503` should be retried
  automatically.
  ⚠️ **Since #610 this applies to the WHOLE endpoint, and that is a behavioural
  change on the plain path.** Both halves -- `override: true` and the plain
  insert, keyed or unkeyed -- now take the write lock before they write, bounded
  at the same 250ms. Before #610 only the override half did: a plain post that
  lost the race blocked for the DSN's full 5000ms and then answered `500` with no
  `Retry-After`, so one URL called the same transient condition retryable or
  permanent depending on the `override` field.
  **Two things move for a plain post, not one.** A `500` after ~5s becomes a `503`
  after ~250ms -- and, because the endpoint now gives up at 250ms instead of
  waiting out 5000ms, contention that used to resolve *into a `201`* can now
  return `503` as well. The endpoint no longer waits on the client's behalf; the
  retry is the client's. That is the deliberate trade: with a single write
  connection, a request that waits 5s does not stall itself, it stalls every other
  in-flight request behind it.

#### `POST /api/v1/events`
- **Scope:** write. **Success:** `201 Created`, body `{"accepted": <int>}`.
- Bulk repo-aware ingest (array body). `accepted` counts events processed, not
  rows newly created (the MAX-on-conflict UPSERT absorbs replays). Per-event
  fields mirror `costRequest` plus `repo`, `session_id`, and a **required**
  RFC3339 `timestamp`. Since #233 the server reprices token counts with its own
  price table; the client `cost_usd` is retained only as a cross-check.
- ⚠️ **`issue_id` follows an ALLOWLIST here, not the `/costs` rule (#466).** This
  endpoint is the JSONL collector's own transport, and the collector legitimately
  assigns the unattributed sentinel when a message resolves to no issue. It
  therefore **accepts the four exact canonical spellings** -- `unattributed`,
  `unattributed:main`, `unattributed:detached-head`,
  `unattributed:branch-without-issue` -- and `400`s every other member of the
  family, every case variant, and every near-miss.
  *Why the split rather than one rule:* the endpoint validates all-or-nothing, so
  one rejected event fails the whole batch; the shipper treats `4xx` as terminal
  with no retry; and it is stateless, so the next run rebuilds the identical batch
  and fails identically. Applying the strict `/costs` rule here would be permanent
  100% capture loss for anyone who has ever committed on `main` without an issue,
  including their well-formed attributed events.
- ⚠️ **`developer` gets the STRICT rule here, with NO allowlist (#619).** The
  asymmetry with `issue_id` directly above is deliberate, not an oversight: the two
  collectors this wire admits (`source` must be `jsonl` or `codex-rollout`) label
  every event with `--developer` or the OS username, whose own no-identity fallback
  is `unknown` -- never `unattributed`. The producers that *do* assign the developer
  sentinel are the org pollers, which are excluded from this endpoint twice over
  (their sources are not shippable, and they write in-process rather than over HTTP).
  There is therefore nothing legitimate to allowlist, and an allowlist would only make
  forging as effective as honesty.

#### `POST /api/v1/outcomes`
- **Scope:** write. **Success:** `201 Created` on insert, `200 OK` on duplicate.
- Response `outcomeResponse`: `status` (`"created"` or `"duplicate"`; always
  present) plus `weight_source`, `weight`, `work_type` (`omitempty`). Dedup is on
  `merge_commit_sha`; a replay returns `200` with `status: "duplicate"` and
  echoes the stored row's weight/source/work_type.
- ⚠️ **`issue_id` may not be the reserved unattributed sentinel (#466)** -- the
  same case-insensitive rule as `POST /costs`, `400` on any member of the family.
  This is the path where forging actually pays: an outcome on the sentinel earns
  weighted points whose cost no work-type segment denominator ever sees, inflating
  the numerator rather than merely shifting a denominator. The GitHub webhook
  derives ids via `issueref`, which can only emit `#\d+` / `ABC-123` shapes, so no
  legitimate producer is affected.
- ⚠️ **`developer` may not be the sentinel either (#619)** -- same rule, `400`. An
  outcome filed against the `unattributed` pseudo-developer credits weighted points
  to a pool of spend that by construction has no owner. The webhook takes its
  developer from the signed GitHub payload's PR author login, so no legitimate
  producer is affected here either.

#### `POST /api/v1/actual_spend`
- **Scope:** write. **Success:** `201 Created`, empty body.
- Finance per-developer invoice total. Request: `developer`, `period` (YYYY-MM),
  `actual_paid_usd`. Rows accumulate; negatives are accepted as credit memos.
- ⚠️ **`developer` may not be the reserved unattributed sentinel (#619)** -- `400`,
  same case-insensitive rule as `POST /costs`. This ledger is the tier-1 invoice
  input to Spend Leverage, so it is a denominator by another name: posting your own
  invoice under the sentinel drops your actual-paid out of your row exactly as
  forging `/costs` drops your metered cost.

#### `POST /api/v1/org_actual_spend`
- **Scope:** write. **Success:** `201 Created`, empty body.
- Org-level invoice total (one contract covering N developers). Request: `org`,
  `period` (YYYY-MM), `actual_paid_usd`. Same accumulate/credit-memo semantics.

#### `GET /api/v1/org_actual_spend`
- **Scope:** write (finance audit read; NOT granted to the viewer token).
- **Success:** `200 OK`, `orgActualSpendResponse`:
  - `since` (string, echoed window lower bound)
  - `orgs` (array of `{org, period, actual_paid_usd, entries}`)
- `?org=` filters to one org; `?since=` sets the window. `actual_paid_usd` is the
  net (source-scoped sum) roll-up per (org, period); `entries` is the row count
  behind that net.

#### `POST /api/v1/developer_alias`
- **Scope:** write. **Success:** `201 Created`, empty body.
- Maps a raw identifier to a canonical developer. Request: `alias`, `canonical`
  (both required). Chain/self-map violations are `400`.
- ⚠️ **Neither `alias` nor `canonical` may be the reserved unattributed sentinel
  (#619)** -- `400` on either, same case-insensitive rule. An alias is a *retroactive*
  rename of the identity space: the score join resolves every stored developer through
  this map before aggregating, so without this guard the checks on `/costs`,
  `/events`, `/outcomes` and `/actual_spend` could all be bypassed in one hop.
  Both directions are refused because they are two different attacks:
  `{"alias": "alice", "canonical": "unattributed"}` folds alice's whole history into
  the pseudo-developer (self-dealing), while
  `{"alias": "unattributed", "canonical": "bob"}` dumps every org-poller aggregate and
  every proxy-unresolved dollar into bob's denominator (sabotage).
- `503` + `Retry-After: 1` when another writer holds the database write lock.
  **Retryable** — it is a lost race for SQLite's single writer, not a bad request
  and not a broken database. Distinguish it from the `400`s above: those are
  permanent and retrying cannot help. A `500` here still means an unexpected
  store failure, including a database that cannot be written at all (read-only
  file, full disk) — deliberately NOT reported as retryable.

#### `DELETE /api/v1/developer_alias/{alias}`
- **Scope:** write. **Success:** `204 No Content`; `404` if not mapped.

#### `GET /api/v1/developer_alias`
- **Scope:** write (admin; discloses the identity map).
- **Success:** `200 OK`, `{"aliases": {<alias>: <canonical>, ...}}`.

#### `PUT /api/v1/org_hierarchy/{developer}`
- **Scope:** write. **Success:** `200 OK`, the stored `HierarchyRow`:
  `{developer, team, division, org}`. Request body: `{team, division, org}`.
- `503` + `Retry-After: 1` when another writer holds the database write lock.
  **Retryable** — it is a lost race for SQLite's single writer, not a bad request
  and not a broken database. A `500` here still means an unexpected store failure.

#### `POST /api/v1/org_hierarchy`
- **Scope:** write. **Success:** `201 Created`, `{"accepted": <int>}`.
- All-or-nothing bulk import (array of `{developer, team, division, org}`).
- `503` + `Retry-After: 1` when another writer holds the database write lock.
  **Retryable, and NOTHING was written** — the batch is one transaction, so the
  whole request is safe to replay verbatim. A `500` here still means an
  unexpected store failure.

#### `GET /api/v1/org_hierarchy`
- **Scope:** write (discloses the full developer->team map).
- **Success:** `200 OK`, `{"hierarchy": [{developer, team, division, org}, ...]}`.

#### `POST /api/v1/period_membership/{developer}/end`
- **Scope:** write. **Success:** `200 OK`,
  `{developer, org, period_end}` (the applied end record). Request: `{org,
  period_end}`.
- `503` + `Retry-After: 1` when another writer holds the database write lock.
  **Retryable.** Distinguish it from the `400` this route also returns when
  `period_end` precedes the membership's start: that one is permanent and
  retrying cannot help. The `503` is classified FIRST precisely so a transient
  lock conflict is never reported as bad input.

#### `DELETE /api/v1/developer/{id}` (GDPR Art. 17 erasure)
- **Scope:** write. **Success:** `200 OK`,
  `{"deleted": {<table>: <count>, ...}, "total_deleted": <int>}`; `404` when
  nothing matched (which makes a repeated erasure idempotent). Available in
  team-aggregation mode (compliance tooling, not a reporting surface).
- `503` + `Retry-After: 1` when another writer holds the database write lock.
  **Retryable, and the erasure did NOT happen** — the transaction is
  all-or-nothing, so nothing was partially deleted; retry to complete the
  request. A `404` means there was nothing to erase; a `500` means an unexpected
  store failure. Only the `503` should be retried automatically.

#### `GET /api/v1/developer/{id}/export` (GDPR Art. 15 access)
- **Scope:** write. **Success:** `200 OK`, `store.DeveloperExport`:
  `{developer, identifiers, token_events, outcomes, actual_spend, org_hierarchy,
  period_membership, quality_events, quality_history, developer_alias}` (each a
  row array grouped by table). `404` when the developer has no data. Available
  in team-aggregation mode.

### Read endpoints

#### `GET /api/v1/scores`
- **Scope:** read. **Success:** `200 OK`, `scoresResponse`.
- Query: `?since=` (YYYY-MM-DD / YYYY-MM / YYYY, default 90 days), `?until=`
  (or `?before=`) exclusive upper bound, `?team=`, `?work_type=`, `?repo=`.
  **Any other parameter is a `400`** -- see
  [strict query-parameter validation](#query-parameters-are-strictly-validated-on-the-scores-endpoints-590).
- `?repo=` (#590) narrows the response to ONE repository. The value must already be
  slug-shaped: two or more `/`-separated segments, no scheme and no host
  (`acme/alpha`, or `group/sub/proj` for a nested GitLab path). It is then
  canonicalized exactly as stored `repo` values are -- lowercased, a trailing `.git`
  and surrounding `/` trimmed -- so `Acme/Alpha.git` matches `acme/alpha`.
  - A value that cannot canonicalize *at all* is a `400`: fewer than two segments, an
    embedded URL, illegal characters, over-length, or the reserved `unqualified`.
  - ⚠️ A **well-formed but non-existent** slug is NOT a `400`. `acme/alpah` scopes
    normally and returns an empty window, and a host-qualified `github.com/acme/alpha`
    is accepted as a legal three-segment slug rather than stripped to `acme/alpha`
    (scheme/host stripping belongs to the collector's write path, not this filter).
    So the `400` buys "that is not a slug", not "you mistyped a repository name".
    Distinguish an empty result from a typo by reading the echoed
    `data_quality.repo_scope`, which reports the canonical slug actually queried.
  - `?repo=` with an **empty value** is treated as unscoped and returns an
    installation-wide read; fleet-wide is spelled by OMITTING the parameter. It stays
    distinguishable because no `repo_scope` key is emitted -- assert on that key.
  - **Scoping is STRICT.** Only rows naming that exact repository are counted. Rows
    carrying the reserved `unqualified` sentinel -- recorded by producers that
    structurally cannot know a repository, such as the reverse proxy -- are
    **excluded**, never folded in. Including them would attribute every repo-blind
    row in the installation to whichever single repository was named, which is the
    same over-counting defect `?repo=` exists to prevent, inverted.
  - Strictness can therefore **under-count**, so it is disclosed rather than left
    silent: see `data_quality.repo_scope_excluded`. A scoped figure over a window
    containing repo-blind rows is a **lower bound**, not a total.
  - `unqualified` itself **cannot be selected** as a scope. Those rows are surfaced
    as a disclosure, not offered as a queryable population.
  - **`spend_leverage` and `actual_paid_usd` are suppressed to `0` under a scope**, and the
    suppression is declared (`data_quality.spend_leverage_suppressed`). Actual spend
    is what the organization PAID a vendor over a period; it carries no repository
    and cannot be divided by one without inventing an allocation. Dividing it by one
    repository's list-price cost would inflate leverage by roughly the
    installation-to-repository ratio. The keys remain PRESENT and read `0`; read the
    `spend_leverage_suppressed` flag, not the zero.
  - 🔴 **`?repo=` is REFUSED with a `400` in any anonymized (`team`/`division`)
    aggregation mode.** Scoping narrows the cohort *before* the k-anonymity floor is
    applied, so a repository only one person works in can shrink a group below `k` and
    expose an individual's figures through the residual bucket -- the exact disclosure
    those modes exist to prevent, and the same reason `?team=` is not honored there. It
    is rejected rather than ignored: silently dropping it would return an
    installation-wide aggregate that looks scoped.
  - Query parameters are parsed STRICTLY, not via a lenient decoder: a malformed pair
    (bad percent-encoding, or a `;` -- which Go does not accept as a separator) is a
    `400`, and so is a REPEATED parameter. Both would otherwise be silently dropped or
    silently resolved to the first value, widening the result while looking filtered.
- Fields:
  - `since` (string) -- echoed window lower bound (UTC calendar day).
  - `price_table` (`{version, effective_date}`) -- **always present** price
    provenance stamp (#233).
  - `rubric` (`{version}`) -- **always present** canonical weight-rubric stamp
    (#239, `scoring.RubricVersion`), the weight-side analogue of `price_table`. A
    `weighted_points` count (hence `tier` and `cost_per_point`) is comparable
    across responses ONLY when `rubric.version` AND `price_table.version` both
    match. See [docs/rubric.md](./rubric.md) for the versioned rubric and its
    "what you may / may not compare" rules. There is deliberately **no** absolute
    good/ok/poor band.
  - `total` (`teamScoreJSON` or absent) -- rollup across all developers in the
    response. **POOLED back-compat summary (#187): NOT a cross-type ranking.**
    See `work_types` for the authoritative within-category comparison.
  - `developers` (array of `developerScoreJSON`) -- per-developer rows in
    developer mode; an explicit **empty array** in any anonymized mode
    (team/division). Same **pooled** caveat as `total`.
  - `aggregation` (string, `omitempty`, #270) -- discriminator naming the
    ANONYMIZED grouping level whose rows populate `teams`: `"team"` (#185) or
    `"division"`. Omitted in developer mode (which ships `developers`, no `teams`).
    It is the seam that lets a consumer tell what each `teams` label means, since a
    division-mode response is otherwise structurally identical to a team-mode one.
    A future org/department level would set its own name here with the SAME `teams`
    array -- no new response field.
  - `teams` (array, `omitempty`) -- populated **only** in an anonymized mode:
    k-anonymized GROUP aggregates, no individual names. In `--aggregation team`
    (#185) each row is a team; in `--aggregation division` (#270) each row is a
    division (one level up in `org_hierarchy`), labelled in the same `team` field.
    The `aggregation` discriminator above says which. A group with fewer than `k`
    contributing developers folds into the residual `"other"` row; a developer
    whose division is empty/unset folds into `"other"` too.
  - `team` (`teamScoreJSON`, `omitempty`) -- populated only when `?team=` filters
    to one team (developer mode only).
  - `data_quality` (`omitempty`) -- **presence contract (#351):** the block is now
    present whenever the window has spend or outcomes (it always carries the
    always-on attribution shares below); it is omitted ONLY for a truly empty window.
    So a consumer must key off the SPECIFIC field it cares about, NOT off block presence
    as a boolean "a warning fired" (the pre-#351 reading -- still true for the tripwire
    fields, which stay omit-when-clean). Fields: the zero-token-outcome tripwire (#136):
    `{zero_token_outcomes: [{developer, issue_id, tokens}]}` in developer mode,
    `{zero_token_outcome_count: <int>}` (name-free) in team mode. May additionally
    carry `mixed_price_versions` (`[<int>]`, `omitempty`, #293): the ascending
    distinct `price_table` versions that priced the window's `token_events`, present
    ONLY when that set has more than one element -- the mix `cost_micro`'s
    immutability (#233) creates, which the single top-level `price_table.version`
    stamp would otherwise mask. Name-free, so it carries in BOTH developer and
    team mode. It also carries the **true attribution-coverage** fields (#351):
    - `attributed_cost_share` (`<float>` in `[0,1]`, `omitempty`) -- the fraction of
      window `cost_micro` that joins to a REAL issue rather than the `unattributed`
      sentinel (`attributed / total`, exact from the same window's cost composition).
      **This is the honest coverage headline** an adopter must see up front -- but it
      measures issue-attribution, which is NECESSARY BUT NOT SUFFICIENT for a score:
      spend on a real issue with no OUTCOME counts here yet drives no TIER. Read it WITH
      `attributed_outcome_share` -- the two measure DIFFERENT joins (cost->issue here,
      outcome->cost there) and are NOT expected to reconcile. Present whenever the window
      has spend; `omitempty` fires ONLY when there is no spend (a real `0.0` -- all spend
      unattributed -- IS emitted, not dropped). Do NOT confuse it with `coverage_pct`
      (see caveat below). Name-free; carries in both modes.
    - `attributed_outcome_share` (`<float>` in `[0,1]`, `omitempty`) -- the fraction
      of the window's outcomes whose canonical `(developer, repo, issue)` has ANY
      matching token spend (`tokens > 0`, a looser bar than the zero-token tripwire).
      Falls to ~0 under the identity-mismatch failure mode below. Present whenever the
      window has outcomes; `omitempty` fires only when it has none. Name-free. A high
      `attributed_cost_share` with a low `attributed_outcome_share` and a non-empty
      `unjoined_developers` is the silent-TIER=0 signature the three fields exist to make
      loud.
    - `unjoined_developers` (`omitempty`) -- developers present on only ONE side of the
      cost/outcome join (#351/#125): cost keyed to an OS username, outcomes to a GitHub
      login, with no `developer_alias` mapping them, which otherwise reads as a silent
      TIER=0. Shape `{cost_only?: [<name>], outcome_only?: [<name>], cost_only_count,
      outcome_only_count}`. Present only when at least one side is non-empty. In
      developer mode the name lists are populated so the operator can map the aliases;
      in **team-aggregation mode (#185) the names are suppressed** and only the two
      counts carry, through the same k-anon guard as `zero_token_outcomes`.

    > **Caveat -- `coverage_pct` is NOT attribution coverage.** The per-developer /
    > `total` / `team` `coverage_pct` is **capture fidelity**: the share of the spend
    > tierd DID record that arrived per-request (realtime proxy/JSONL) rather than as
    > a coarse estimate. It reads ~100% even when most spend never attributes to an
    > issue, which is why it was mistaken for completeness. Its computation is
    > unchanged (#136 keeps the wire name stable; the dashboard labels it "Fidelity").
    > Attribution completeness is `data_quality.attributed_cost_share`, added here.
    - `unattributed_buckets` (array, `omitempty`) -- the labeled split of the single
      unattributed mass (attribution refocus, Option B): one row per reason the join
      could not tie spend to an issue. Shape `[{bucket, cost_usd, share}]`, sorted by
      descending cost. `bucket` is the stable label `unattributed:main` (exploratory
      on a mainline branch), `unattributed:detached-head`,
      `unattributed:branch-without-issue`, or the base `unattributed` (host-blind
      producers). `share` is of TOTAL window cost, so the buckets + `attributed_cost_share`
      sum to 1.0. Present only when the window has unattributed spend. Name-free; carries
      in both modes.
    - `exploratory_cost_share` (`<float>` in `[0,1]`, `omitempty`) -- the org headline
      for exploratory overhead: the `unattributed:main` bucket's share of total window
      cost. This overhead STAYS in the denominator (it is real planning/exploration
      spend); the field makes it *visible* rather than excluded. Pointer-emitted so a
      real `0.0` carries; present alongside `unattributed_buckets`. Name-free.
    - `kanon_suppressed` (`omitempty`, #593) -- present when a sub-k residual cohort
      was WITHHELD from an anonymized (`team`/`division`) response. Shape
      `{developers, k_anonymity, withheld_total, withheld_cost_composition,`
      `withheld_segment_reconciliation}`.
      Name-free: counts only, never an identity.
      - **When it is present, `total`, `cost_composition` and
        `segment_reconciliation` are ABSENT**, and the
        visible cohort rows no longer sum to the window. That is deliberate. Before
        #593 the residual bucket was emitted with no floor, publishing a sub-k cohort's
        exact figures; and merely removing that row would not have closed it, because
        `total` minus the named rows reconstructs the hidden cohort by subtraction.
        Both the row and every unfloored aggregate over the same population are
        withheld together.
      - ⚠️ This **retires the previous guarantee that k-anonymized rows always sum to
        the grand total.** That property is what makes the disclosure recoverable, so
        it and k-anonymity cannot both hold. Consumers that reconciled rows against
        `total` must treat a suppressed response as a distinct case rather than a
        reconciliation failure.
      - `k_anonymity` is the floor **actually in force**, after the library's
        `MinKAnonymity` clamp -- not the value a caller requested. Note `tierd serve`
        refuses to start below `MinKAnonymity` (3), so on a served deployment the
        configured and enforced values always agree; the clamp is defence in depth for
        direct library callers. The field reports the enforced value regardless, so a
        consumer never has to know which.
      - A consumer must key off this field to distinguish "withheld for anonymity" from
        "this window has no data". Those demand opposite reactions: the first is fixed
        by widening the window or querying a level with more people in it, the second
        by fixing capture.
      - Developer-aggregation mode never emits this: it names everyone by design, so
        there is nothing to reconstruct.
      - **Two further placements carry the same shape** (#593). `work_types[].kanon_suppressed`
        declares a suppression local to one work-type segment -- a segment is scored over
        only the developers who did that kind of work, so it is systematically smaller
        than the window and can be sub-k while the window is not. And
        `/scores/compare` carries a **top-level** `kanon_suppressed` rather than one
        inside a window's `data_quality`, because the two-window fold suppresses a
        cohort from BOTH sides together or from neither -- attaching it to one window
        would imply the other was unaffected.
      - ⚠️ **A suppression at either level withholds BOTH levels' totals.** Weighted
        points partition exactly across work types, so the pooled total and the segment
        totals reconstruct each other: `pooled - Σ(segments)` isolates a suppressed
        segment, and `Σ(segments) - (named pooled rows)` isolates a suppressed pooled
        cohort. Withholding one level and not the other closes nothing.
    - `repo_scope` (`<string>`, `omitempty`, #590) -- the canonical repository this
      response was narrowed to, echoed back **canonicalized** (what was queried, not
      what was typed). Present on every `?repo=` read; **absent on an unscoped one**.
      This is the field that makes "scoped" and "not scoped" distinguishable on the
      wire, which is the whole point of #590 -- the original defect was not that
      `?repo=` did nothing, it was that a caller could not TELL it did nothing. A
      consumer that requires a scoped figure must assert on this key, **never** on
      having sent the parameter. Name-free; carries in both modes.
    - `repo_scope_excluded` (`omitempty`, #590) -- what the strict scope DROPPED from
      the window as repo-blind. Shape `{token_events, cost_usd, outcomes}`. Present
      only on a scoped read that actually excluded something; a scoped read over a
      fully-qualified window omits it, and **that absence is the clean signal** --
      the scoped figures are a true total rather than a lower bound.
      Read it as: this much of the window could not be placed in ANY repository. The
      excluded rows are **not** claimed to belong to the scoped repository; they are
      unattributable by construction, which is what the sentinel means. `cost_usd` is
      the size of the hole in the scoped denominator and `outcomes` the hole in the
      numerator -- they move a TIER score in opposite directions. Name-free.
    - `spend_leverage_suppressed` (`<bool>`, `omitempty`, #590) -- `true` when a repo
      scope suppressed `spend_leverage` / `actual_paid_usd`, which are
      installation-wide by construction and cannot be scoped (see `?repo=` above).
      Absent entirely on an unscoped read: suppression is a property of scoping, not a
      standing caveat. Without this field a scoped response's `actual_paid_usd: 0`
      would be indistinguishable from "no actual spend has been recorded", which is a
      materially different statement. Name-free.
  - `work_types` (array of `workTypeSegmentJSON`, `omitempty`) -- the
    **authoritative** type-segmented view (#187): one entry per `work_type`, each
    `{work_type, aggregation?, developers?, teams?, total?}` scored over ONLY that
    category (the per-segment `aggregation` mirrors the top-level discriminator, #270).
    This is the surface for comparing developers/teams; the pooled
    `developers`/`teams`/`total` above are retained for back-compat and are not a
    cross-type ranking.
  - `segment_reconciliation` (`omitempty`, #466) -- accounts for the window's WHOLE
    spend against the `work_types` segments above, so a reader can see how much the
    segmented view leaves out and why. Omitted when the window has no cost rows, and
    withheld entirely under a k-anonymity suppression (see `kanon_suppressed`), since
    its window total restates the figure `cost_composition` just withheld. It is ALSO
    dropped — silently, with a server-side ERROR log — when the block is not fit to
    publish: an accumulator saturated at the int64 ceiling, or any figure came out
    negative. That third case is the one absence this API does NOT declare on the wire,
    because it signals a server fault rather than a policy withhold; its only reachable
    trigger is a window whose summed spend approaches ~9.2e12 USD. Shape:
    `{developers: [row], total: row}` where `row` is
    `{developer, window_cost_usd, window_cost_micro, outcome_linked_cost_usd,`
    `outcome_linked_cost_micro, no_outcome_cost_usd, no_outcome_cost_micro,`
    `unattributed_cost_usd, unattributed_cost_micro}`. `developers` is omitted in any
    anonymized mode (a named per-developer cost row would defeat the k floor);
    `total` is the always-present name-free rollup and carries no `developer`. ⚠️ A
    deployment running an org-usage poller will see a row whose `developer` is literally
    `"unattributed"` — org-level invoice aggregates that cannot honestly be split per
    person — with its cost wholly in `unattributed_cost_micro`. That is the developer
    sentinel, a different field from the issue sentinel discussed below.
    - **Why it exists.** A segment's TIER denominator is outcome-linked cost only:
      `work_type` is a property of the OUTCOME, so spend on an issue that produced
      none has no category to be filed under and appears in no segment. The pooled
      headline score keeps that spend in its denominator, so every per-type TIER read
      systematically better than the headline, invisibly.
    - **The invariant, and exactly where it holds:**
      `outcome_linked_cost_micro + no_outcome_cost_micro + unattributed_cost_micro`
      `== window_cost_micro`, EXACTLY, on the **integer** fields and only those. The
      `_usd` companions are independent float conversions, so `a + b + c === d` on
      them is false for roughly one triple in ten — 22601 of 216000 in a deterministic
      SYNTHETIC sweep spanning $0.008 to $78 per component
      (`TestSegmentReconciliation_DollarsAreNotExact`); the same sweep fails 0 of 216000
      on the integers. That rate is a property of the sweep's magnitudes, not a
      measurement of any production window. **Assert on the micros, display the
      dollars.**
    - The invariant is INTERNAL to this block. All four figures fold from ONE
      `DeveloperIssueCostsWindow` snapshot, which makes it an arithmetic identity no
      concurrent writer can break. It is **not** a cross-block promise:
      `window_cost_micro` and the pooled `total` come from separate non-transactional
      reads over a window whose upper bound is usually open, so on a live store they can
      differ by rows written between them. Do not assert across blocks.
    - ⚠️ **It does NOT reconcile against the segments' totals, and no invariant
      claims it does.** The segments can legitimately double-count a cost row -- an
      issue carrying two work types is charged to both segments, and under the
      tolerant repo join (#231) a repo-blind cost row is charged to every qualified
      outcome sharing its issue id. Both over-counts are deliberate (they lower TIER,
      so ambiguity never flatters a developer), which makes "segments + gap == window"
      false on ordinary data. This block partitions the underlying cost ROWS instead,
      each counted exactly once, under the identical repo rule the segments use.
    - ⚠️ **`no_outcome` and `unattributed` are DIFFERENT and are never merged.**
      `no_outcome` is cost tied to a REAL issue id that produced no outcome in the
      window (abandoned work, work in flight, a PR that never merged) -- the gap this
      block exists to surface. `unattributed` is cost the collector could not tie to
      any issue at all (the `unattributed` sentinel family), which is the established
      meaning of the word elsewhere in this API and matches `cost_composition`'s
      split. It is reported here only so the three parts sum to the window.
    - Not narrowed by `?work_type`: the gap is a property of the developer's window,
      not of the segment the caller asked for.
  - `cost_composition` (`omitempty`) -- cost-composition sidecar (#234): a
    whole-window, name-free breakdown of WHERE spend went, for optimization. Omitted
    when the window has no token spend. Shape:
    `{total_cost_usd, attributed_cost_usd, unattributed_cost_usd, unattributed_share,`
    `cache_read_share, premium_model_share, by_model: [{model, host, cost_usd, share,`
    `premium}], by_class: {input_tok, output_tok, cache_read, cache_write}}`. Dollar
    figures are USD; shares are fractions in `[0,1]`. **Reconciliation** (exact in
    the underlying integer micro-dollars; the USD floats reconcile to micro-dollar
    precision): `attributed_cost_usd + unattributed_cost_usd == total_cost_usd` and
    `sum(by_model[].cost_usd) == total_cost_usd` (no residual bucket). `cache_read_share`
    is `cache_read / (input + cache_read + cache_write)` (input-side hit share, docs/
    pricing-philosophy.md §4); `premium_model_share` is the SPEND share on models
    pricing >= $5/M base input (the frontier/reasoning tier). `by_class` is TOKEN
    counts, not allocated dollars (a stored blended `cost_micro` is not exactly
    splittable per class); `by_model.premium` and the model breakdown are host-aware
    (#300), so an open-weights model split across hosts stays multiple rows. Present
    identically in developer and team-aggregation mode -- it names no individual, so
    it does not defeat k-anonymity (#185).
- `developerScoreJSON`: `developer, tier, weighted_points, total_cost_usd,
  actual_paid_usd, spend_leverage, coverage_pct, exploratory_cost_share,
  cost_per_point, sample_n, ci_low, ci_high, cost_per_point_ci_low,
  cost_per_point_ci_high, ranked, flagged_outcomes`. `exploratory_cost_share` (attribution
  refocus, Option B) is this developer's `unattributed:main` cost / their total window
  cost -- the per-developer companion to `data_quality.exploratory_cost_share`, naturally
  k-anon-safe because developer rows are not emitted in team-aggregation mode. The array
  is **not pre-sorted**; the client applies the
  two-tier order using the `ranked` flag (#133). `cost_per_point` (#239) is
  `total_cost_usd / weighted_points` -- the inverse-unit dual of `tier`
  (numerically `1000/tier`); it is **`null` on a zero-point row** (#472) so "no
  accepted outcome" is not encoded as the most-efficient `0`, while a genuine
  zero-cost (FREE) row keeps its honest `0`. `cost_per_point_ci_low/high`
  are its 95% self-relative bootstrap interval, the reciprocal (ends swapped) of
  the `tier` CI -- `0` for unranked rows. See [docs/rubric.md](./rubric.md).
- `teamScoreJSON`: `team (omitempty), tier, weighted_points, total_cost_usd,
  actual_paid_usd, spend_leverage, coverage_pct, cost_per_point, ranked`. No CI at
  team grain (the bootstrap is a per-developer signal, #133). `ranked` (#502) is
  the #133/#136 evidence floor on the aggregate's SUMMED inputs -- outcomes >=
  `MinRankedOutcomes`, spend >= `MinRankedCostUSD`, no zero-token outcome among the
  members. `tier` is **unaffected** by it: an unranked aggregate still carries its
  true quotient, so a consumer must gate the headline on `ranked` rather than
  expect a scrubbed number (#136). There is deliberately **no team-level
  `sample_n`** -- see the #502 changelog entry.

#### `GET /api/v1/scores/{developer}`
- **Scope:** read. **Success:** `200 OK`, `developerDetailResponse`.
- **Blanket `404` in any anonymized (team/division) mode** (#185, #270): it names
  one individual by construction; the 404 is returned for every path value, before any lookup, so
  it is not an existence oracle.
- Fields: `developer, tier, weighted_points, total_cost_usd, actual_paid_usd,
  spend_leverage, coverage_pct, cost_per_point, sample_n, ci_low, ci_high,
  cost_per_point_ci_low, cost_per_point_ci_high, ranked, flagged_outcomes,
  issues`. `cost_per_point` and its CI mirror `developerScoreJSON` (#239).
  `issues` is an array of `{issue_id, weight, quality, pr_number (omitempty),
  zero_token}`. Note: this endpoint's top-level object has no `rubric`/`price_table`
  stamp of its own -- read those from `GET /api/v1/scores` for the same window.
- Query: `?since=`, `?until=` (or `?before=`), `?repo=`. **Any other parameter is a
  `400`** -- see
  [strict query-parameter validation](#query-parameters-are-strictly-validated-on-the-scores-endpoints-590).
- `?repo=` (#590) applies the SAME strict scoping as `/scores`, including the
  `unqualified` exclusion, and narrows the `issues` array too -- a scoped detail must
  not list work done in another repository. Adds three top-level fields mirroring the
  `/scores` `data_quality` ones: `repo_scope` (`omitempty`), `repo_scope_excluded`
  (`omitempty`), `spend_leverage_suppressed` (`omitempty`). Under a scope
  `actual_paid_usd` and `spend_leverage` are suppressed to `0` and the suppression is
  declared -- read the flag, not the zero.

#### `GET /api/v1/scores/compare`
- **Scope:** read. **Success:** `200 OK`, `compareResponse` (#277).
- Purpose: a before/after period comparison -- two half-open windows in, per-row
  deltas plus a CI-overlap significance flag out. Reuses the SAME windowed scores
  computation as [`GET /scores`](#get-apiv1scores), so a compared score never
  diverges from what `/scores` reports for the same window. It is an **aggregate
  view like `/scores`**, so in any anonymized (team/division) mode it returns `200`
  with k-anonymized group deltas -- it is NOT the single-developer `404` carve-out.
- Query (each window mirrors `/scores`' `since`/`until` grammar and validation,
  #276): `?since_a=`, `?until_a=` (window A = "before"); `?since_b=`, `?until_b=`
  (window B = "after"). Each `since_` defaults to 90 days ago; each `until_` is an
  optional exclusive upper bound that must be strictly after its `since_`
  (`400` otherwise) and is retention-checked (`422`). Every delta is **B - A**.
  **Any other parameter is a `400`** (#590) -- including `?repo=`, which this endpoint
  does **not** implement. It is rejected rather than ignored precisely because it
  returns the same class of cost figure from the same shared windowed computation as
  `/scores`, so silently accepting-and-dropping it would reproduce the #590 defect one
  endpoint over. Scoped comparison is a legitimate future feature; being quietly
  unscoped is not a feature.
- Fields:
  - `window_a`, `window_b` (`{since, until?, data_quality?}`) -- each window's
    echoed bounds and its **OWN** `data_quality` block (#277): the zero-token /
    `mixed_price_versions` signal is per window (a mixed-version WARN can apply to
    one window and not the other), same shape and mode-dependent name-suppression
    as [`/scores`' `data_quality`](#get-apiv1scores). Only the zero-token and
    `mixed_price_versions` signals are carried here; the #351 coverage shares are a
    `/scores`-only surface. `until` is omitted when the window is open-ended.
  - `price_table` (`{version, effective_date}`) -- **always present** active-table
    stamp (#233); per-window pricing mixes surface via each window's
    `data_quality.mixed_price_versions`.
  - `mode` (string) -- `"developer"`, `"team"`, or `"division"`, echoing the
    server's aggregation mode so a consumer knows whether `developers` or `teams`
    carries the rows (the same discriminator as `/scores`' `aggregation`).
  - `developers` (array of `developerDeltaJSON`) -- per-developer deltas in
    developer mode; an explicit **empty array** in any anonymized mode (#185, #270),
    never a named row. Each row: `{developer, present_a, present_b, a, b, delta_tier,
    delta_weighted_points, delta_total_cost_usd, significant}`. `a`/`b` are
    `scoreSideJSON` (`{tier, weighted_points, total_cost_usd, actual_paid_usd,
    spend_leverage, coverage_pct, cost_per_point, sample_n, ci_low, ci_high,
    ranked}`). `cost_per_point` (#239) is the same points-guarded
    `total_cost_usd / weighted_points` (`0` on a zero-point side) `/scores`'
    developer rows and the team compare sides carry, added for contract parity; no
    self-relative `cost_per_point` CI on a side. Rows are the
    **union** of developers across both windows; a delta and `significant` are
    computed **only** when the developer is present in BOTH windows (`0`/`false`
    otherwise, never fabricated against an absent side).
  - `teams` (array of `teamDeltaJSON`, `omitempty`) -- populated **only** in an
    anonymized mode (team #185 / division #270): each row `{team (omitempty), a, b,
    delta_tier, delta_weighted_points, delta_total_cost_usd, significant, ranked}` where
    `a`/`b` are `teamScoreJSON`. **k-anonymity is a two-window INTERSECTION (#277):**
    a group is a named row only if it independently clears the k-floor in BOTH
    windows; a group sub-k in EITHER window (including one present in only one window)
    folds into the single `other` residual on BOTH sides, so a group's PRESENCE never
    differs across windows and no sub-k aggregate can be recovered from a delta.
    `significant` is **always `false`** here: group aggregates carry no bootstrap CI
    (an interval is a per-developer signal, #133).
  - `total` (`teamDeltaJSON`, `omitempty`) -- name-free grand-rollup delta across
    every developer in each window, present in both modes; `nil` when both windows
    are empty. `significant` is always `false`.
  - `ranked` (bool, on every `teamDeltaJSON` -- both `teams[]` rows and `total`) --
    the **derived** ranking verdict of the comparison itself (#605): `a.ranked &&
    b.ranked`, a boolean AND over the two sides' own #133/#136 verdicts and never a
    third floor. The rule it encodes is one sentence: *anything derived from an
    unranked input is itself unranked.* It is an AND rather than an OR because a
    ranked baseline beside an unranked selected window lets a reader reconstruct the
    withheld ratio exactly (`selected = baseline + delta`), and `% change =
    delta/baseline` is a pure function of the withheld baseline ratio. Always present,
    never `omitempty`: `false` and "an older server that never said" must stay
    distinguishable on the wire (the ambiguity behind #603). Consumers **read** this
    field rather than re-deriving the conjunction -- one producer, so the floor
    reaches every consumer instead of being re-implemented at each.
- **Significance test:** `significant` is `true` for a developer row only when the
  developer is present AND `ranked` (#133) in BOTH windows and the two 95% bootstrap
  TIER confidence intervals do **not** overlap (`a.ci_high < b.ci_low` or
  `b.ci_high < a.ci_low`). Any overlap, or an unranked/below-floor window, renders it
  `false` -- the move is within sampling noise or the sample cannot support the claim.
- Unblocks the dashboard dumbbell comparison (#278).

#### `GET /api/v1/events`
- **Scope:** read. **Success:** `200 OK`. **403 in any anonymized (team/division) mode** (#185, #270:
  raw per-developer rows are suppressed). Keyset-paginated bulk export of
  `token_events`.
- Query: `?since=`, `?until=`, `?limit=` (rejected loudly above the store max),
  `?cursor=` (opaque, echo the previous page's cursor).
- **JSON** (default): `eventsExportResponse` -- `{next_cursor, events: [...]}`.
  Each event: `id, ts, developer, issue_id, model, input_tokens, output_tokens,
  cache_read_tokens, cache_write_5m_tokens, cache_write_1h_tokens, cost_micro,
  source, fidelity, idempotency_key, repo, session_id, price_version, host,
  billing_mode`.
- **CSV** (`Accept: text/csv`): same fields in the **append-only** column order
  of `eventsCSVHeader`. Empty `next_cursor` (body and the `X-Next-Cursor` header)
  means the window is exhausted.

#### `GET /api/v1/outcomes`
- **Scope:** read. **Success:** `200 OK`. **403 in any anonymized (team/division) mode** (#185, #270).
  Same pagination/CSV contract as `GET /events`.
- **JSON:** `outcomesExportResponse` -- `{next_cursor, outcomes: [...]}`. Each
  outcome: `id, ts, developer, issue_id, pr_number, weight, weight_source,
  quality, merge_commit_sha, additions, deletions, changed_files, source,
  work_type, work_type_source, repo, push_day`.
- **CSV:** the **append-only** `outcomesCSVHeader` order.
- `push_day` (#242) is the appended trailing column: the UTC calendar day a
  `source='push'` outcome aggregates to (the per-issue-per-day dedup key), and
  `""` for a PR outcome (NULL column), matching `merge_commit_sha`.

#### `GET /api/v1/quality_events`
- **Scope:** read. **Success:** `200 OK`. **403 in any anonymized (team/division) mode** (#185, #270:
  rows carry a per-developer `developer` column). Keyset-paginated bulk export of
  the append-only `quality_events` signal log (#242). Same `?since`/`?until`/
  `?limit`/`?cursor` query, pagination, and content-negotiation contract as
  `GET /events`.
- **JSON** (default): `qualityEventsExportResponse` -- `{next_cursor,
  quality_events: [...]}`. Each row: `id, outcome_id, developer, issue_id,
  event_type, source_ref, event_ts, recorded_at`.
- **CSV** (`Accept: text/csv`): the **append-only** `qualityEventsCSVHeader` order.

#### `GET /api/v1/quality_history`
- **Scope:** read. **Success:** `200 OK`. **403 in any anonymized (team/division) mode** (#185, #270).
  Keyset-paginated bulk export of the append-only `quality_history` transition log
  (#242). Same pagination/CSV contract as `GET /events`.
- **JSON:** `qualityHistoryExportResponse` -- `{next_cursor, quality_history:
  [...]}`. Each row: `id, outcome_id, developer, issue_id, old_quality,
  new_quality, reason, source_ref, ts`.
- **CSV:** the **append-only** `qualityHistoryCSVHeader` order.
- Together the two quality exports make an outcome's multiplier re-derivable
  (`quality == last new_quality`) from a BI export, not just the erasure-scoped
  DSAR export.

#### `GET /api/v1/fidelity`
- **Scope:** read (satisfied by EITHER the viewer token or the write token).
  **Success:** `200 OK`, `fidelityResponse`. **403 in any anonymized
  (team/division) mode** (#185, #270, same posture as `GET /events`/`GET
  /outcomes`): the body names individual developers, which the anonymized modes
  suppress, and it cannot be k-anonymized while staying a per-developer capture
  report -- the 403 is returned before the store is touched.
- Per-canonical-developer capture-fidelity signals (#236): the rollout view for
  "which developers are (not) capturing, and at what quality." Raw
  `token_events` developer keys are canonicalized through `developer_alias`
  (#125) and merged, so a developer mid-rename is one row, not two. Takes no
  query parameters; the windows are fixed at 7d and 30d.
- Top-level fields (`fidelityResponse`):
  - `now`, `since_7d`, `since_30d` (strings) -- RFC3339 UTC stamps of the exact
    window bounds the counts below are measured over, so a reader never has to
    assume a server-local clock for what "7d"/"30d" meant for this response.
  - `developers` (array of `developerFidelityJSON`) -- one row per canonical
    developer, sorted by `developer`. An **empty array** `[]` on an empty DB
    (never null).
- `developerFidelityJSON`:
  - `developer` (string) -- canonical developer identifier.
  - `event_count_7d`, `event_count_30d` (integers) -- `token_events` counts over
    the 7d and 30d windows.
  - `last_event_by_source` (object, `{source: RFC3339-UTC-string}`) -- the most
    recent event timestamp per capture source over **all history** (not just the
    30d window): the "is this source still delivering" signal. **Always present**
    (an empty object `{}` when the developer has no events), so a client can index
    it without a nil check; a source absent from the map has never delivered.
  - `fidelity_levels` (object, `{level: 30d-count}`) -- the 30d event count per
    fidelity level (`realtime`/`daily`/`estimated`). **Always present** (`{}`
    when empty); a developer with no `realtime` count is on a degraded capture
    path. Keys are the same `fidelity` closed set as elsewhere in this contract.
  - `unknown_model_cost_share` (number, `0..1`) -- the fraction of the developer's
    30d spend billed at the unknown-model pricing guess (#267): high share means
    TIER is pricing that spend at a `(host, model)` rate it cannot audit. `0` when
    30d spend is zero.
- **Additive** per the [matrix above](#additive-vs-breaking-at-a-glance): a new
  endpoint is additive, not a compat break. It was added in #320 (behind #236),
  after this contract doc (#241) was written, hence the catalog backfill (#322).

#### `GET /metrics`
- **Scope:** read. Mounted at the **root** (`/metrics`, not `/api/v1`), and
  **only** when a metrics registry is wired (`cmd/tierd`); absent otherwise.
- **Success:** `200 OK`, Prometheus text exposition
  (`text/plain; version=0.0.4`). Not a JSON contract; the additive-only rule
  applies at the level of metric/label names.

### Open endpoints (probes)

#### `GET /api/v1/version`

Build identity of the running process (#638). Open (no token) and mounted in
read-only mode, because the deployment hardest to identify by other means is the
public demo, and the demo runs read-only.

`{version: <string>, commit: <string, omitempty>, modified: <bool, omitempty>,
go_version: <string, omitempty>, platform: <string>, price_table: {...}}`

- `commit` is the build revision: ldflags-injected where available, otherwise the
  Go toolchain's `vcs.revision` stamp. Absent when neither exists.
- `modified` is a **tri-state**: absent means the binary carries no VCS stamps
  (the shipped container is built with `.git` excluded, so this is its normal
  state); `false` means stamped and clean. **Absent does NOT mean clean.**
- `price_table` is provenance for the figures, **not** identity — it bumps only
  when prices change, so it can agree across a full release gap.

#### `GET /api/v1/health`
- **Scope:** open. **Success:** `200 OK`, `{"status": "ok"}`.

#### `GET /api/v1/healthz`
- **Scope:** open. **Readiness** probe (#49). `200` when every subsystem is
  healthy, `503` when any subsystem is restarting/stopped; **same JSON body in
  either case**. As of #48 the body is extensible:
  `{"watcher": <health.WatcherSnapshot>, "subsystems": {"<name>":
  {"healthy": <bool>, "detail": <object, omitempty>}}, "healthy": <bool>}`.
  - `subsystems` is a map keyed by subsystem name (`watcher`, and future
    collectors). Each value is `{healthy, detail}` where `detail` is that
    subsystem's payload (for `watcher`, a `WatcherSnapshot`). New collectors
    append a key here — no consumer needs new per-subsystem code.
  - `healthy` (top level) is the aggregate: `true` iff every subsystem is
    healthy. It mirrors the `200`/`503` status code.
  - `watcher` (top level) is **retained for backward compatibility** and
    duplicates `subsystems.watcher.detail`; it is deprecated in favour of the
    `subsystems` map. `WatcherSnapshot`: `status, last_error (omitempty),
    last_event_ts (omitempty), started_at (omitempty), restart_count,
    next_retry_at (omitempty), watch_add_failures, last_watch_add_error
    (omitempty)`.
  - Do NOT wire a k8s liveness probe here.

#### `GET /api/v1/livez`
- **Scope:** open. **Liveness** probe (#49); always `200`. Body `livezResponse`:
  `{status: "alive", uptime_s: <int>, version: <string>, commit: <string, omitempty>}`.
  `commit` was added additively by #638; it is absent on a binary carrying no
  build identity. `version` is the
  documented [feature-detection](#feature-detection) signal.

## API changelog

Newest first. Every entry names the change, its classification, and the issue.
Backfilled entries (`#185`, `#187`, `#191`, and the additive-column history)
predate this discipline and are recorded here so the record is complete.

- **#638 -- `GET /api/v1/version` added, and `commit` added to `/livez`
  (ADDITIVE).** A new open probe endpoint reporting build identity, plus one new
  `omitempty` field on an existing body. No existing field changed name, type or
  semantics, so a client parsing `/livez` today is unaffected. Rationale: the
  `version` string alone cannot identify a build -- a tagged release reports the
  same string however it was built -- so a deployment could not be asserted to be
  the build it was believed to be.

- **#619 -- the reserved-sentinel ingest guard now covers `developer` (BREAKING on
  `POST /costs`, `/events`, `/outcomes`, `/actual_spend`, `/developer_alias`,
  `PUT`/`POST /org_hierarchy`; silent header drop on the proxy).** #466 closed forgery
  of the `unattributed` sentinel on `issue_id` and deliberately left the `developer`
  half open. This closes it, with the same predicate rather than a lookalike, so the
  two columns cannot drift.
  *Breaking:* `developer` may no longer carry the sentinel family — the bare
  `unattributed` plus any `unattributed:<reason>` sub-bucket — on any of the six
  endpoints above. `400` on all of them, case-insensitively and after trimming
  whitespace. It was previously written silently. On `POST /developer_alias` the rule
  applies to **both** `alias` and `canonical`.
  *Why the break was taken:* this is the worse half of the #466 vector, and the only
  half that pays. TIER is `points / (cost/1000)`. Forging `issue_id` moves a dollar
  between buckets inside the forger's own denominator and leaves the headline score
  unchanged; forging `developer` moves it out of that denominator entirely, onto the
  `unattributed` pseudo-developer, so the forger's cost falls and their score rises.
  It was also user-visible: `segment_reconciliation.developers[]` emitted a row whose
  `developer` was literally `"unattributed"`.
  `POST /developer_alias` is included because an alias is a *retroactive* rename of
  the identity space — the score join resolves stored developers through it before
  aggregating — so without that guard the spend endpoints are bypassable in one hop.
  ⚠️ `org_hierarchy` was **missed by the first pass of this work** and added after
  review, which is worth recording because of *why* it was missed: it reads as
  org-structure admin rather than a spend write. It is not. `upsertHierarchyTx` also
  opens a `period_membership` **seat** backdated to `0000-01`, and nothing downstream
  excludes the sentinel from a per-developer aggregate — so one authenticated write
  enrolling `unattributed` into a team drags the whole unattributed pool into that
  team's denominator. Every other surface here is self-dealing; that one is aimed
  outward.
  *No allowlist anywhere, unlike `issue_id`:* the `/events` allowlist exists because
  the JSONL collector legitimately ships the sentinel *family* as an `issue_id` on
  every exploratory session. Nothing legitimately ships it as a `developer`: the two
  producers that assign it — the `anthropicadmin` / `openaiusage` org pollers, for
  aggregates that cannot honestly be split per person — write in-process via
  `collector.Ingester` and never cross an HTTP boundary, and the sources they carry
  are not shippable over `/events` in the first place. The proxy's own missing-header
  fallback is likewise server-side. So the strict rule costs no capture.
  *Blast radius:* a client that was forging. `unknown` — the real no-identity fallback
  `collector.OSUsername()` emits — is unaffected, as are ordinary identities that
  merely contain the word (`unattributed-bot`, `not-unattributed`). `tierd ship
  --developer unattributed` will now fail its batch; that invocation was always a
  forgery.
  *Proxy (not an error, by design):* a forged `X-Tier-Developer` is treated as a
  **missing** header rather than rejected — the proxy sits on the request path and
  must never fail a provider call over attribution metadata. The stored row is
  identical to the missing-header case; what changes is the counter.
  *Also additive:* `tier_proxy_unattributed_total` gains a `developer-forged` value on
  its `header` label, mirroring `issue-forged`. No existing series changes meaning — a
  forged header previously incremented no counter at all. This is the label to alert
  on: it is the only one of the five that indicates a client raising its own score.

- **#466 -- `scores.segment_reconciliation` (additive) + the reserved-sentinel
  ingest guard (BREAKING on `POST /costs`, `/outcomes`, `/events`).**
  *Additive:* a top-level `segment_reconciliation` block on `GET /scores` partitions
  the window's cost into `outcome_linked` / `no_outcome` / `unattributed`, each as
  both `_usd` and `_cost_micro`, with the exact partition invariant holding on the
  integers. `kanon_suppressed` gains `withheld_segment_reconciliation`. Purely
  additive: no existing field changes meaning, and the block is `omitempty`.
  *Breaking:* `issue_id` may no longer carry the reserved unattributed sentinel.
  `POST /costs` and `POST /outcomes` `400` on the whole family, case-insensitively
  and after trimming whitespace; `POST /events` allows exactly the collector's four
  canonical spellings and `400`s everything else. It was previously written
  silently.
  *Why the break was taken:* the sentinel is server-assigned, and a client forging it
  moved its own dollars out of the `no_outcome` thrash signal and out of the
  attributed side of the #234 coverage split. Worse, a case variant classified
  DIFFERENTLY in SQL (`LIKE`, case-insensitive) and in Go (`HasPrefix`,
  case-sensitive), so one dollar was reported simultaneously as exploration by
  `cost_composition` and as abandoned real-issue work by `segment_reconciliation`, in
  one response, against a named developer. The read side is now `GLOB`
  (case-sensitive) so the two engines agree on stored rows; ingest is deliberately
  WIDER than the read side so a variant never becomes a row at all.
  *Blast radius:* no legitimate producer sets the sentinel as an `issue_id` on these
  HTTP surfaces. The GitHub webhook derives ids via `issueref`, which can only emit
  `#[1-9]\d*` / `ABC-123` shapes. The org pollers (`anthropicadmin`, `openaiusage`) DO
  assign the bare sentinel for aggregates they cannot split per developer — they are
  unaffected because they write in-process via `collector.Ingester`, never over
  `/costs`. The `/events` allowlist exists precisely because the JSONL collector assigns
  it too, and a `4xx` there is terminal for the stateless shipper.
  ⚠️ *Scope, stated precisely:* this guard covered **`issue_id` only**. The same string
  is also the sentinel for the **`developer`** field; that half was closed separately
  by **#619**, below.
  *Also additive:* `tier_proxy_unattributed_total` gains an `issue-forged` value on its
  `header` label. No existing series changes meaning — a forged header previously
  incremented no counter at all, since the guard did not exist.
  *Today's readers:* none. This ships the DATA only; `internal/dashboard` does not
  render `segment_reconciliation`, so the segmented panel a reader actually looks at
  still shows outcome-linked cost alone. The UI pass is separate work.

- **#668 -- three org-hierarchy routes answer write-lock contention as `503`
  instead of `500` (BREAKING status change).** `PUT /api/v1/org_hierarchy/{developer}`,
  `POST /api/v1/org_hierarchy` and `POST /api/v1/period_membership/{developer}/end`
  now take the SQLite write lock before they write, bounded at the same 250ms the
  other request-path writers have used since #346/#598/#610. Contention on any of
  the three is therefore `503` + `Retry-After: 1` instead of `500`.
  *Breaking, and recorded as such rather than filed under "a better error":* a
  status code changes for an input a client can actually hit, and a client with a
  `500`-is-fatal rule now sees `503`. Retry the `503`; do not retry the `400` or
  the `500`.
  ⚠️ *But the shape of this break differs from #610's, and the difference is worth
  stating because the obvious reading is wrong.* #610 also shortened a wait, so
  contention it had previously **waited out and completed** began failing instead.
  That does NOT apply here. Measured: the DEFERRED read-then-write these three
  used to run did not wait at all -- SQLite runs no busy handler for a
  deadlock-prone upgrade, so it returned `SQLITE_BUSY` in ~65us and the handler
  answered `500`. So these routes never completed under contention; they failed
  immediately with an unretryable error and said `500`. **No client loses a
  success it used to get.** The change is `500` -> `503` on a request that failed
  either way, plus a `Retry-After` telling the client what to do about it.
  *One route deserves its own note:* `POST /api/v1/period_membership/{developer}/end`
  also returns `400` when `period_end` precedes the membership's start. Contention
  is classified BEFORE that check, so a transient lock conflict is never reported
  as incoherent input -- which would tell a client to fix a `period_end` that was
  perfectly valid.
  *Why the change was made at all:* it is the precondition for raising
  `SetMaxOpenConns` (#669). These sites' in-process atomicity came from the single
  connection, so raising the pool without converting them would have turned a
  latency defect into a correctness one -- an unretried `SQLITE_BUSY_SNAPSHOT`
  (517) race -- with no test failing.

- **#610 -- `POST /costs` answers write-lock contention one way (BREAKING status
  change on the plain path).** The plain, non-override half of the endpoint now
  takes the SQLite write lock before it writes, bounded at the same 250ms the
  `override: true` half has used since #346. Contention there is therefore
  `503` + `Retry-After: 1` after ~250ms, where it was previously a `500` after the
  DSN's full 5000ms block with no `Retry-After`. Applies to keyed and unkeyed
  posts alike.
  *Breaking, and recorded as such rather than filed under "a better error":* a
  status code changes for an input that a client can actually hit, and a client
  with a `500`-is-fatal rule now sees `503`. The correct handling is the one the
  override path already documents -- retry the `503`, do not retry the `409` or
  the `500`.
  ⚠️ *And the break is wider than the `500` -> `503` swap, so do not read it as
  error-text polish:* the wait itself shrank from 5000ms to 250ms, so contention
  the endpoint previously **waited out and completed as a `201`** now returns
  `503` instead. Any client posting into a contended store sees more failures than
  before -- each of them fast, retryable, and carrying `Retry-After`. A client with
  no retry on `/costs` at all is the one that regresses, and it is the one that
  must change.
  *Why the break was taken:* one route answered the same transient condition two
  different ways depending on one request field, and no caller can reasonably
  retry one and not the other. The `500` was also the worse of the two answers --
  it reads as a broken database, so an operator goes looking for corruption when
  another writer simply held the lock. And the 5000ms wait was never free: with a
  single write connection, a request that blocks that long stalls every other
  in-flight request behind it, so the endpoint was buying one client's `201` with
  every other client's latency. It is the same defect class #598 fixed across the
  store, here on the busiest write path.
  *What does NOT change:* the `201`/`200` status codes and bodies themselves, the
  #295 divergent-cost `409`, the #346 override behaviour, and the `500` for a
  genuinely permanent store failure (read-only or full database -- deliberately
  NOT reported as `503`). On `503` nothing was written: the insert runs in one
  all-or-nothing transaction that takes the lock before it writes.

- **#605 -- `ranked` on every compare DELTA row (additive field).** `teamDeltaJSON`
  -- the shape behind `/scores/compare`'s `teams[]` rows and its name-free `total`
  -- now carries a DERIVED `ranked`: `a.ranked && b.ranked`, computed once
  server-side. The rule it encodes is one sentence: *anything derived from an
  unranked input is itself unranked.*
  *Why an AND and not an OR:* a ranked baseline beside an unranked selected window
  lets a reader reconstruct the withheld ratio exactly (`selected = baseline +
  delta`), and `% change = delta/baseline` is a pure function of the withheld
  baseline ratio -- it leaks directly rather than additively. The
  one-ranked-one-unranked case is precisely the one worth withholding, so it is the
  case the AND catches.
  *Not `omitempty`:* a `false` is the load-bearing value here, so omitting it would
  drop the verdict entirely rather than encode it. (Do **not** read this as a
  licence to feature-detect on field presence -- see the compatibility rules above;
  `livez`'s `version` remains the supported mechanism.)
  *Derived, with a single producer:* consumers READ this field instead of
  re-deriving the conjunction. #502 added `ranked` to the aggregate and it reached
  only one of the org headline's three consumers, because the other two re-derived
  the quotient from raw sums rather than reading the struct (#605 fixed the compare
  view, #606 the CLI report). A rule re-implemented at each consumer drifts at each
  consumer.
  *Today's readers:* the dashboard's compare card gates its delta and % change on
  `total.ranked`. The `teams[]` rows carry the same derived field for contract
  uniformity -- one struct, one producer -- but the dashboard's team dumbbell reads
  the two sides' own `a.ranked`/`b.ranked` directly, so nothing consumes `ranked` on
  a `teams[]` row yet.

- **#502 -- `ranked` on every group aggregate (additive field).** `teamScoreJSON`
  -- the shape behind `total`, the `?team=` filter, the k-anonymized `teams` array,
  each `work_types` segment, and both sides of `/scores/compare` -- now carries
  `ranked`, the #133/#136 evidence floor applied to the aggregate's SUMMED inputs
  (outcomes >= 3, spend >= $5.00, no zero-token outcome). It previously existed only
  on `developerScoreJSON`, so a group aggregate reached every consumer with no
  ranking verdict at all and was rendered as evidence by default.
  *Not `omitempty`:* a `false` is the load-bearing value, and omitting it makes
  "unranked" indistinguishable from "a server that never said".
  *No field's meaning changes, and `tier` is NOT one of them:* an unranked aggregate
  still carries its exact quotient (the #502 case ships `tier: 2.8e8`), per #136 --
  the number is never altered, only its ranking authority revoked. **Consumers must
  gate the headline on `ranked`; do not expect a scrubbed or floored number.**
  *Deliberately NOT accompanied by a team-level `sample_n`:* that count is the
  missing denominator that keeps `data_quality.attributed_outcome_share`
  non-invertible in the anonymized modes. A boolean discloses a threshold crossing,
  not a count.
- **#593 -- k-anonymity residual floor (BREAKING behaviour in anonymized modes;
  additive field).** The residual `other` cohort was emitted with **no k-floor**, so an
  anonymized deployment published a sub-k cohort's exact figures -- reproduced at k=5
  as one developer's TIER, cost, points and cost-per-point. Reachable by narrowing any
  axis; the `?repo=` guard added in #590 closed only one of them.
  *Fix:* a sub-k residual is withheld, and `total` plus `cost_composition` are withheld
  with it -- necessary because `total` minus the named rows reconstructs the hidden
  cohort exactly. Applies to `/scores`, each `work_types` segment, and
  `/scores/compare`. Declared via the additive `data_quality.kanon_suppressed`.
  *Breaking:* in anonymized modes a response may now omit `total` and
  `cost_composition`, and visible rows no longer sum to the window. The previously
  documented "totals are preserved exactly" property is **retired** -- it is the
  property that made the disclosure recoverable. Developer mode is unaffected.
- **#590 -- `?repo=` scoping on the scores endpoints (additive fields) + strict
  query-parameter validation (BREAKING behaviour, deliberately).** Two halves that
  must ship together.
  *Additive:* `?repo=` on `GET /scores` and `GET /scores/{developer}`, plus the
  `data_quality` fields `repo_scope`, `repo_scope_excluded` and
  `spend_leverage_suppressed` (the last three mirrored as top-level fields on the
  developer detail). No existing field changes meaning.
  *Breaking:* all three scores endpoints now reject unrecognized query parameters
  with `400` instead of ignoring them. Classified as breaking and recorded as such
  rather than quietly filed under "additive" -- a client sending an extraneous
  parameter goes from `200` to `400`.
  *Why the break was taken:* `/scores` previously accepted `?repo=` and ignored it,
  returning a whole-installation aggregate indistinguishable from a scoped one. The
  filter alone would have left the trap one keystroke away (`?repos=`, `?Repo=`),
  where a caller's assertion passes while asserting nothing. The governing invariant
  is that **"could not scope" must never share a response shape with "scoped, and
  this is the result"**; a silent wrong answer is worse than an error.
  *Scoping semantics:* strict equality. Rows carrying the `unqualified` sentinel are
  excluded, never folded in -- tolerant matching would attribute every repo-blind row
  in the installation to whichever single repository was named. Strictness can
  under-count, so it is disclosed (`repo_scope_excluded`) rather than left silent: a
  scoped figure over a window containing repo-blind rows is a lower bound.
  `spend_leverage`/`actual_paid_usd` are suppressed under a scope (actual spend has no
  repository and cannot be divided by one) and the suppression is declared.
  🔴 **What #593 does NOT close, stated plainly so nobody reads it as more than it is:**
  it is a per-RESPONSE rule, and **cross-request differencing remains open**. Two
  separately k-safe windows -- neither suppressed, neither declaring anything -- still
  subtract to the cohort active only in the wider one, recovering points as well as
  cost. No per-response rule can close that; it is the query-composition problem and
  needs a different control (rate limiting, query logging, or a privacy budget).
  Treat anonymized aggregation as raising the cost of identifying an individual, not as
  a guarantee against a determined caller who can issue arbitrary windows.
- **#277 -- `GET /api/v1/scores/compare` (additive: new endpoint).** A before/after
  period comparison: two half-open windows (`since_a`/`until_a`, `since_b`/`until_b`)
  in, per-developer or per-group deltas plus a CI-overlap `significant` flag out. Read
  scope. Reuses the `/scores` windowed computation (extracted into a shared
  `loadWindow`), so a compared score matches `/scores` for the same window. Three
  invariants are enforced server-side (the reason it is an endpoint, not a client
  two-fetch): **(1)** anonymized-mode k-anonymity is a two-window INTERSECTION -- a
  group is named only if it clears the k-floor in BOTH windows, else it folds to
  `other` in both, so presence never differs across windows and no sub-k aggregate
  leaks through a delta; **(2)** the same anonymized guard as `/scores` -- never a
  named per-developer delta in team/division mode; **(3)** per-window `data_quality`.
  A new endpoint breaks no existing consumer. Unblocks the dashboard dumbbell (#278).
- **#239 -- `scores.cost_per_point` + `scores.rubric` version stamp (additive).**
  Added the inverse-unit `cost_per_point` (`total_cost_usd / weighted_points`,
  `0` on a zero-point row) and its self-relative bootstrap CI
  (`cost_per_point_ci_low/high`, the reciprocal of the `tier` CI, no second
  resample) to `developerScoreJSON` and `developerDetailResponse`; `cost_per_point`
  (no CI) to `teamScoreJSON`; and an always-present top-level `rubric` (`{version}`,
  `scoring.RubricVersion`) stamping which canonical weight-rubric produced the
  `weighted_points`, the weight-side analogue of `price_table`. Purpose: give the
  TIER number a MEANING without an absolute band. `cost_per_point` is the
  constant-dollar benchmarking/trend unit. What closes the generous-vs-strict
  labeling exploit is NORMATIVITY -- the single canonical rubric compiled into the
  binary, so everyone weights against the same calibration instead of house
  habits; the `rubric` version stamp is PROVENANCE, not the closer: it does not
  set the weights, it records WHICH rubric produced a `weighted_points` so two
  numbers can be trusted to share a matched rubric (and a mismatch surfaces as
  non-comparable AND visible instead of a silent category error). **No absolute good/ok/poor band is defined anywhere** -- comparison is
  self-relative and valid only within a matched `rubric.version` + `price_table.version`
  (see [docs/rubric.md](./rubric.md)). Existing fields and the TIER formula are
  untouched; pinned consumers unbroken. A cross-org opt-in benchmark distribution
  is a forward-compatible follow-up that needs a second org's data to exist first.
- **#295 -- `POST /costs` divergent keyed re-post now `409` (bug-fix; narrowly
  breaking).** Fixes silent financial data loss: after #233 a keyed re-post with
  the SAME `idempotency_key` but a CHANGED `cost_usd` was silently dropped
  (first-writer-wins) yet still returned `201`, so a finance correction vanished
  with a success code. Such a divergent re-post now returns **`409 Conflict`**
  with a JSON error body; the stored `cost_micro` remains immutable (#233) -- the
  `409` REJECTS the write, it never overwrites. **Strictly scoped to the divergent
  case:** an IDENTICAL re-post (same key, same cost) is unchanged -- still `201`,
  still idempotent -- and an unkeyed post is unchanged. Divergence is judged on
  the stored INTEGER `cost_micro`, so an honest retry whose float/FX/rounding
  jitter rounds to the same micro value does NOT `409`. Classified breaking only
  for a client that depended on the silent-drop-returns-`201` behavior -- i.e. on
  losing its own correction; the correction path is a NEW `idempotency_key` (or
  delete + re-post), or the sanctioned audited override below (ruling C).
- **#346 -- `POST /costs` sanctioned cost-correction override, ruling C
  (additive).** The follow-up #295's changelog entry above named as a separate
  piece of work: `override: true` + required `override_actor` +
  `override_reason` lets a legitimate finance correction land on a divergent
  keyed re-post instead of 409ing, as a narrow, audited, single-column
  (`cost_micro`-only) UPDATE with an append-only `cost_correction_audit` row
  (old -> new, actor, reason) -- never a last-writer-wins upsert. Purely
  additive: every pre-#346 request (no `override` field) behaves identically
  to before, including the #295 409. See the full contract above, including
  the endpoint's stated trust model: the identity check closes the
  **accidental** key collision, not deliberate misattribution by a holder of
  the write token, and `override_actor` is a self-asserted claim.
- **#351 -- `scores.data_quality` true-attribution-coverage + unjoined-developer
  flag (additive; presence-contract clarified).** Added three `data_quality` fields
  so the headline `/scores` number is honest about how much of the window it actually
  covers: `attributed_cost_share` (fraction of window `cost_micro` joined to a real
  issue, not the `unattributed` sentinel), `attributed_outcome_share` (fraction of
  outcomes with matching token spend), and `unjoined_developers` (developers with cost
  but no outcomes, or outcomes but no cost -- the silent-TIER=0 identity mismatch;
  named in developer mode, name-free counts only in team-aggregation mode, #185). No
  existing field changed meaning or computation. **`coverage_pct` is NOT touched** and
  was NOT measuring attribution coverage: it is per-developer capture FIDELITY
  (realtime vs. estimated share of recorded spend) and correctly reads ~100% even when
  most spend is unattributed -- the mismatch the PM surfaced was a reading error, now
  documented, not a bug in `coverage_pct`. **Presence-contract note:** the two coverage
  shares are ALWAYS present when the window has the relevant data (spend / outcomes),
  so a non-empty window now ships a `data_quality` block even when nothing is flagged
  (previously the block was omitted unless a tripwire fired). A truly empty window
  still ships no key; a consumer that ignores unknown keys is unaffected. The dashboard
  trust strip keys only off the zero-token fields, so it is visually unchanged.
- **#293 -- `scores.data_quality.mixed_price_versions` (additive).** Added an
  `omitempty` `[<int>]` field to the [`data_quality`](#get-apiv1scores) block: the
  ascending distinct `price_table` versions that priced the window's `token_events`,
  present ONLY when more than one version is spanned. `scores.price_table.version`
  stamps a single active table, but `cost_micro` is immutable per row (#233) so a
  window legitimately mixes historical and active pricing; this WARN keeps the stamp
  from reading as false uniformity. Name-free (version integers only), so it carries
  identically in developer and team-aggregation mode. No existing field changed
  meaning; a consumer that ignores the key is unaffected.
- **#242 -- `push_day` on the outcomes export + quality audit bulk exports
  (additive).** Appended `push_day` as the trailing column of the
  [`GET /outcomes`](#get-apiv1outcomes) JSON and CSV (the UTC per-issue-per-day
  dedup key a `source='push'` row aggregates to; `""` for a PR row) -- a pinned
  positional consumer is unbroken. Added two new read endpoints,
  [`GET /api/v1/quality_events`](#get-apiv1quality_events) and
  [`GET /api/v1/quality_history`](#get-apiv1quality_history), reusing the #191
  keyset-pagination / limit / `Accept` machinery and inheriting the #185
  team-mode `403`. Together they make an outcome's multiplier re-derivable
  (`quality == last new_quality`) from a BI export, not just the erasure-scoped
  DSAR export.
- **#234 -- `scores.cost_composition` sidecar (additive).** A whole-window,
  name-free breakdown of where spend went -- cost by normalized model, per-class
  token composition, attributed vs unattributed spend, and the two optimization
  levers (`cache_read_share`, `premium_model_share`). `omitempty`, so a window with
  no token spend ships no key. Pure sidecar: the TIER formula and every existing
  `/scores` field are untouched. Pinned consumers unbroken.
- **#48 -- `GET /api/v1/healthz` extensible subsystem body (additive).** Added
  a top-level `subsystems` map (`{"<name>": {healthy, detail}}`) and an
  aggregate `healthy` bool. The legacy top-level `watcher` block is retained
  (duplicates `subsystems.watcher.detail`) so pre-#48 consumers are unaffected;
  it is now deprecated in favour of the map. `200`/`503` semantics unchanged.
  Subsystems now register into a `health.Registry` instead of the handler
  hard-coding one key, so v1.5 collectors extend the body without a schema
  break.
- **#236 -- `GET /api/v1/fidelity` endpoint added (additive).** Per-canonical-
  developer capture-fidelity signals (`now`/`since_7d`/`since_30d` window stamps;
  per developer `event_count_7d`, `event_count_30d`, `last_event_by_source`,
  `fidelity_levels`, `unknown_model_cost_share`). Read-scoped; `403` in
  team-aggregation mode like the bulk exports. Shipped in #320 on a branch that
  predated this contract doc (#241); the catalog entry was backfilled in #322. A
  new endpoint changes no existing surface.
- **#304 -- `host` + `billing_mode` columns (additive).** Appended to the
  `token_events` JSON/CSV export. `billing_mode`
  (`per_token`|`subscription`|`self_hosted_amortized`) discriminates whether
  `cost_micro` is a canonical per-token figure or a derived/approximate one.
  Pinned consumers unbroken.
- **#238 -- `session_id` column (additive).** Appended to the `token_events`
  export (and accepted on `POST /events`). Opaque Claude Code session UUID; NULL
  for session-blind producers.
- **#233 -- `price_table` stamp + `price_version` column (additive).**
  `scores.price_table` (`{version, effective_date}`) is now always present;
  `price_version` was appended to the `token_events` export. The server became
  the single pricing authority (`POST /events` reprices; client `cost_usd` is a
  cross-check).
- **#231 -- `repo` column (additive).** Appended to BOTH exports and accepted as
  an optional field on `POST /costs` and `POST /events`. `issue_id` values were
  deliberately left unchanged so a consumer pinned to `issue_id` keeps reading
  the same value; the repository arrived in its own new column rather than
  changing an existing column's shape in place.
- **#191 -- bulk exports added (additive).** `GET /api/v1/events` and
  `GET /api/v1/outcomes` introduced (keyset-paginated, JSON default + CSV). Both
  return `403` in team-aggregation mode. New endpoints, no existing surface
  changed.
- **#187 -- work-type segmentation; `/scores` top-level fields SEMANTICALLY
  DEMOTED.** Added `scores.work_types` (the authoritative within-category
  comparison) and `work_type`/`work_type_source` columns on the outcomes export
  (additive). **In the same change, the top-level `developers`/`teams`/`total`
  became a POOLED population summary that is explicitly NOT a cross-type
  ranking.** The numbers stayed parseable while their meaning narrowed; that
  semantic change shipped without a version signal or deprecation header, and is
  the motivating defect for this contract (#241). Under the rule above it should
  have been announced via a changelog entry and the field doc comments (it now
  is); the new meaning correctly shipped under a new key (`work_types`) rather
  than repurposing the old fields.
- **#270 -- division-aggregation mode + `aggregation` discriminator (additive
  field; new opt-in mode).** Adds `--aggregation division`, a second ANONYMIZED
  level that rolls `org_hierarchy` up one step past team to division, reusing the
  identical k-anonymity floor and every suppression guard team mode has (the
  per-developer `/scores/{developer}` `404`, the bulk-export/fidelity `403`s). Its
  rows ride the SAME `teams` array as team mode; the new top-level `aggregation`
  string field (`omitempty`, absent in developer mode) says which level the rows
  are. Adding the field is additive and back-compatible; the mode itself is
  deploy-time and opt-in like #185. k-anonymity holds independently at each level
  because every level is a flat partition of the same developer set, so a sub-k
  team suppressed at team level is absorbed into a `>= k` division at division
  level, never re-exposed.
- **#185 -- team-aggregation mode (behavioural, opt-in).** When the server runs
  in `AggregationTeam` mode, `GET /scores` replaces named `developers` with
  k-anonymized `teams` and emits `developers` as an empty array, and
  `GET /scores/{developer}` blanket-`404`s. This is a deploy-time mode, not a
  wire-shape change to a given response, but it changes which fields are
  populated -- consumers must handle the empty-`developers`/`teams` shape. (The
  bulk exports landed later in #191 and inherit this policy, returning `403` in
  team mode; see that entry. Division mode #270 rides the same policy.)
- **#136 -- `data_quality` block + `zero_token`/`flagged_outcomes` fields
  (additive).** Zero-token-outcome tripwire surfaced on `/scores` and
  `/scores/{developer}`.
- **#133 -- ranking fields (additive).** `sample_n`, `ci_low`, `ci_high`,
  `ranked` added to per-developer rows; the array is not pre-sorted.
- **#55 -- `cache_write_5m_tokens`/`cache_write_1h_tokens` (additive);
  `cache_write_tokens` deprecated.** The legacy single-bucket request field is
  accepted with a `Warning: 299` header (the last use of the RFC 7234 precedent;
  new deprecations use the RFC 9745 `Deprecation` / RFC 8594 `Sunset` pair).

## Changing the API

When you add a field, add it in the same PR to (1) the Go response struct in
`internal/api/`, (2) this document's endpoint catalog, (3) the
[API changelog](#api-changelog), and -- for the CSV exports -- (4) the relevant
CSV header slice (`eventsCSVHeader`, `outcomesCSVHeader`, `qualityEventsCSVHeader`,
`qualityHistoryCSVHeader`) and `docs/how-it-works.md`. When you
deprecate or change a field's meaning, follow
[Announcing a deprecation](#announcing-a-deprecation-or-a-semantic-change):
new field name (never repurpose), the RFC 9745 `Deprecation` / RFC 8594 `Sunset`
headers, and a changelog entry.

## Relationship to the ingestion seam contract

TIER's cross-service ingestion seam, `tier-jsonl-ingestion` (Claude Code session
JSONL -> tier collector), is pinned by the JSON schema at
`docs/contracts/tier-jsonl-ingestion.schema.json`. That seam is the external
Claude Code producer format tier consumes; tier owns and publishes the seam
contract even though the data flows inbound, because tier, not the producer,
defines which fields it requires. This document, by contrast, governs the HTTP
API tier itself PRODUCES.
The `/api/v1` REST surface is not yet pinned as its own formally versioned seam
contract; that is a possible follow-up once an external consumer of the REST
contract needs a stability guarantee.
