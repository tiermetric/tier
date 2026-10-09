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
- **Encoding:** UTF-8 JSON unless noted. The four bulk exports also emit CSV via
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

> 🔴 **First released in v0.4.1.** A server running **v0.4.0 or earlier answers
> `404` here** — verified against the published `0.4.0` container image. The
> published v0.4.1 release binary answers `200` with its commit (measured
> 2026-09-25); the response below is the exact body it returned.
>
> ⚠️ **`404` on this path does not mean the server is unhealthy or the wrong
> build** — it most likely means the build predates the route. On a v0.4.0 or
> older target, identify a deployment with `GET /api/v1/livez`, which has always
> carried `version`. Note that reports the *release*, not the *build*: two
> binaries from a moved tag share a version string, which is exactly why this
> route exists.

```json
{
  "version": "v0.4.1",
  "commit": "ec6ff60fd72c6824c22d2c01e58cdc444d49fcce",
  "modified": false,
  "go_version": "go1.26.6",
  "platform": "darwin/arm64",
  "price_table": { "version": 9, "effective_date": "2026-07-26" }
}
```

`commit` is the revision injected at build time (`make build`, the release
binaries and the published container image set it); without that, it falls back to the `vcs.revision` stamp the Go toolchain
embeds. `modified` comes only from the stamp. On a binary built with
`-buildvcs=false`, or from a tree with no `.git` like the shipped container,
there is no stamp and `modified` is omitted, never `false`. Absent does NOT mean
clean (see [`GET /api/v1/version`](#get-apiv1version)). `commit` is also carried on `livez` — additively,
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

### Price-table content identity (#713)

On `GET /api/v1/scores` and `GET /api/v1/scores/compare` — and in `tierd
score-log`'s JSON — the `price_table` block carries two digests alongside
`version` and `effective_date`:

| field | covers | shape |
|---|---|---|
| `table_hash` | the **RESOLVED in-memory table** | `tierpt1:<64 lowercase hex>` |
| `file_hash` | the **raw source bytes** of the price YAML | `sha256:<64 lowercase hex>` |

🔑 **Why the resolved table, not the file.** The server bakes provider-default
cache multipliers into every model at parse time, and those defaults come from
**code**, not from the YAML. Change the compiled-in Anthropic cache-read
multiplier and every cached-token cost in the system changes while the YAML is
byte-identical — a hash over the file would certify "same rates" across a real
rate change, which is worse than no hash. `table_hash` moves; `file_hash` does
not.

The pair runs the other way too: a **comment-only** edit to the price YAML moves
`file_hash` and leaves `table_hash` alone, so source-URL comments stay auditable
without re-identifying the prices.

**What each answers.**

- `version` answers "which published revision of the reference table is this".
  It is a human-assigned label; two installs can both report `9` while one runs a
  `--prices` override that moved a rate.
- `table_hash` answers "do these two installs hold the same resolved price
  **inputs**". It deliberately **excludes** `version` and `effective_date`, so
  "content unchanged, version bumped" and "version unchanged, content changed"
  stay distinguishable.
- `file_hash` answers "is this the same source document".

⚠️ **The scheme tag is load-bearing — compare the whole string, never the hex
alone.** `tierpt1` names the CANONICALIZATION, and it will be bumped if the
serialization ever changes. A consumer that strips the tag cannot tell "computed
by a different scheme version" from "these two disagree about prices", and a
guard built that way would reject every install on the older binary the first
time the scheme is legitimately fixed.

🔴 **These are UNKEYED digests, self-reported by the server. They are not a
signature.** Against an operator who controls the deployment they prove nothing —
such an operator can change the table and report any hash they like. What they
detect is what they are for: accidental drift between installs, an edit to the
price YAML on a host whose *binary* you already trust, and a serializer that
cannot tell two different tables apart. Do not build an attestation on them.

✅ **Since #714 `table_hash` is also load-bearing at STARTUP, not only for
comparison.** A deployment records the identity of every price table it serves and
refuses to open a database in which one `version:` integer would come to denote two
different tables. See
[reference-price-table.md §9](reference-price-table.md#9-version-is-binding--one-number-one-table-714).
That does not strengthen any claim below — the digest is still unkeyed and
self-reported — it just means a mismatch now stops a boot instead of only informing
a comparison.

🔴 **`table_hash` equality is NECESSARY, not sufficient, for comparing costs
across installs — and the omission that matters most is the binary.** Equal
digests mean equal price *inputs*. The pricing **logic** is outside the digest
and cannot be inside it: which row a given model resolves to is decided by code
(model-name normalization, the host-qualified key, and the self-hosted size-class
patterns — that pattern list has already changed once, in #267, and altered
pricing). So two installs on **different builds** can report the same
`table_hash` and still price the same event differently.

Before comparing figures across installs, match all of:

| stamp | where | what it holds constant |
|---|---|---|
| `price_table.table_hash` | `/scores`, `/scores/compare`, `/report_manifest` | the resolved price inputs |
| `commit` | `GET /api/v1/version` | the pricing **logic** |
| `rubric.version` | `/scores` | the outcome weights |
| aggregation mode + window | `/scores` | what is being summed |

⚠️ **Do not treat a published digest as a trust anchor.** The value changes on
every price-table edit, and no gate keeps a copy pasted into a document (this one
included, which is why the sample above shows the shape rather than a value).
Read it from the deployment you are asking about.

🔴 **The digests are NOT on `GET /api/v1/version`, deliberately.** That endpoint
is unauthenticated — identifying a build is a probe concern — and it returns
`price_table: {version, effective_date}` only.

An unkeyed digest over a low-entropy price table is a **confirmation oracle**:
someone who can guess a private `--prices` file confirms the guess in one hash,
and an install running the shipped table with one negotiated rate edited is
grid-searchable on that rate. Publishing that from an unauthenticated endpoint
would have pushed the mitigation onto ingress configuration that many
self-hosted installs do not have.

The convenience it would have bought — answering "same build **and** same
prices" from one probe — is not needed: a third party verifying a published
report reads the digests from **that report's own manifest**, never from a live
endpoint. So this costs nothing and closes the oracle outright.

⇒ Read `table_hash` from `/scores`, `/scores/compare`, or
`GET /api/v1/report_manifest` (#715 -- the report manifest this paragraph
anticipated, now a real endpoint). Read `commit` from `/api/v1/version`. The two
questions have two sets of endpoints, and that is the intended shape.

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
   was not). The narrowly scoped [#1095 security exception](#api-changelog)
   below permits replacing raw healthz error text with classes in place.
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
| Change the DEFAULT VALUE of an optional query parameter | **Announced** -- behavioural, not a field break (see [#746](#api-changelog)) |

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
- **metrics** -- `GET /metrics` only: the metrics token or the write token, and
  in `developer` mode also the read-only viewer token (#944). The metrics token
  is rejected 403 on every other route.
- **open** -- no token (probes only; never exposes spend data).

When the server is started with no API token (loopback-only, fail-closed per
`cmd/tierd`), auth is disabled and all scopes are transparent.

Error bodies are uniformly `{"error": "<message>"}` with a 4xx/5xx status.

### Write endpoints

Every `POST`, `PUT` and `PATCH` below requires `Content-Type: application/json`
(parameters such as `charset` are allowed) and answers `415` otherwise, in token mode
too (#893). Authentication runs first, so an unauthenticated request still gets `401`.
A `DELETE` carries no body and needs no `Content-Type`.

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
  below); `billed_to` (optional -- #854, see below).
- Per row, `cost_usd` must be finite and in `0..10000` ($10,000), including
  audited overrides. Every token field must be in `0..1000000000000` (1e12),
  after legacy `cache_write_tokens` normalization. An over-limit request returns
  `400` naming the limit and asking callers to split a large import across rows
  (#1082). `/actual_spend` and organization invoice limits are unchanged.
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
- ⚠️ **A row for a provider whose org usage poller this server runs is refused
  `400` unless it declares `billed_to: "other"` (#854).** The Anthropic Admin and
  OpenAI Usage pollers bring the org's recorded usage for their provider up to
  the provider's own total, and they never subtract a `/costs` row, so a manual
  row for that org's usage would be counted twice. **Manual posting and the
  poller are alternatives for one org: use one or the other.** The refusal
  applies to every row whose `model` is priced under that provider (`claude-*`
  with the Anthropic poller, `gpt-*` and the other OpenAI models with the OpenAI
  poller), with tokens or cost only, after the same case, whitespace and
  date-suffix normalisation the poller applies (`claude-sonnet-4-20250514` is
  refused too). A `model` the price table does not recognise is admitted and is
  not counted by the startup warning, even when it names a polled model another
  way, such as `anthropic/claude-sonnet-4` or `Claude Sonnet 4`: the poller
  counted that usage under the canonical id, so if the row is the polled org's
  usage it is counted twice unnoticed. Post canonical model ids, the names in the price table.
  - **The remedy.** The `400` body names the provider and carries this text,
    which is also what the startup warning below prints. It is
    `PolledProviderOverlapRemedy` in `internal/api/handler.go`; that constant is
    the source, and this copy follows it:

    > This server runs the org usage poller for this provider, which already
    > counts the polled org's usage, so a manual row for that usage is counted
    > twice. To post spend billed where the poller cannot see it (Claude Max
    > seats, Bedrock or Vertex, another org), send "billed_to": "other"; do not
    > post the polled org's own usage. For a row already stored without
    > billed_to: if it is Claude Max, Bedrock, Vertex or another org's usage, no
    > action is needed; if it has an idempotency_key and repeats the polled
    > org's usage, correct it to cost_usd 0 with the audited override, sending
    > the stored row's idempotency_key, developer, issue_id, model and fidelity
    > with override=true, override_actor and override_reason. The override
    > corrects the cost only: the row's token counts stay counted. A row with no
    > idempotency_key, or whose stored fidelity is neither daily nor estimated,
    > cannot be corrected through the product.
  - **`override: true` without `billed_to`** is admitted when a row already owns
    the `idempotency_key`: it corrects that row (the remedy above). When no row
    owns the key, the override would insert a new undeclared row, so it gets the
    same `400` and nothing is written. Whether a row owns the key is decided in
    the same write-locked transaction as the correction.
  - `billed_to` accepts only `"other"`, or omitted; any other value is `400`,
    with or without a poller. A server with no poller for the row's provider
    accepts the field, stores it and otherwise behaves as before.
  - The declaration is stored on the row (`billed_to`, NULL when not declared) and
    appears in the [`GET /events`](#get-apiv1events) export and the DSAR export.
    It is **not** part of the keyed re-post comparison: a re-post that differs
    from the stored row only in `billed_to` is the idempotent `201`, and the
    stored value is left as first recorded. So a row stored without `billed_to`
    can never gain one.
  - ⚠️ **A keyed retry of a row stored before the upgrade now gets `400`** on a
    server running that provider's poller, if the retry omits `billed_to`. The
    row already exists and nothing is written either way; add
    `"billed_to": "other"` to the retry only if that spend really is billed
    elsewhere.
  - **Startup warning.** For each running poller, `tierd serve` logs one `WARN`
    at startup, "undeclared manual rows that may be counted twice", with the
    number of `source='api'` rows that have no `billed_to`, are for that
    provider's models, and fall on a UTC day that poller covered, their summed
    cost (`cost_usd`), and the remedy. A poller covers a day when it has stored
    a row for it. The day match is approximate, because a manual row's time is
    when it was posted, not the day the usage happened. Nothing is logged when
    the count is 0. The count never falls to 0 for legitimate Claude Max,
    Bedrock or other-org rows stored before the upgrade, because those can never
    gain a `billed_to`.
  - **Listing rows stored before the upgrade.** Use either route. Both only
    read.
    - **Over the API** (developer aggregation mode only: `GET /api/v1/events`
      returns `403` in `team` or `division` mode). The endpoint filters only by
      `since`, `until`, `cursor` and `limit`, so filter the rows yourself.
      `TIER_URL` is where your tierd is served (for example
      `http://127.0.0.1:8080`) and `TIER_TOKEN` is your read or write token
      (`--read-token` or `--api-token`):

      ```sh
      cursor=
      while :; do
        page=$(curl -fsS -H "Authorization: Bearer $TIER_TOKEN" \
          "$TIER_URL/api/v1/events?since=2020-01-01&limit=10000&cursor=$cursor") ||
          { echo "request failed; the listing is incomplete" >&2; break; }
        printf '%s\n' "$page" |
          jq -r '.events[] | select(.source == "api" and .billed_to == "")
                 | [.ts[0:7], .model, .developer, .issue_id, .fidelity,
                    .idempotency_key, .cost_micro] | @tsv'
        cursor=$(printf '%s\n' "$page" | jq -r .next_cursor)
        [ -n "$cursor" ] || break
      done
      ```

      It follows `next_cursor` page by page until the export is exhausted; the
      rows it lists can be on any page, so do not stop at the first. Each line
      is month, model, developer, issue id, fidelity, key (empty for an unkeyed
      row) and cost in millionths of a dollar: the fields the override needs.
      For the sum per provider per month, add up `cost_micro` for that
      provider's models in each month and divide by 1,000,000.
    - **In SQL** (any aggregation mode), on the machine running `tierd`, with
      `DB` set to the path you gave `--db` (default `~/.tier/tier.db`):

      Prerequisite: the separate `sqlite3` CLI (`sudo apt install sqlite3` on Ubuntu/Debian; preinstalled on macOS).

      ```sh
      sqlite3 -cmd '.timeout 5000' "$DB" <<'SQL'
      SELECT substr(ts, 1, 7) AS month, model, COUNT(*) AS rows,
             SUM(idempotency_key IS NULL) AS unkeyed,
             SUM(cost_micro) / 1e6 AS usd
        FROM token_events
       WHERE source = 'api' AND billed_to IS NULL
       GROUP BY month, model
       ORDER BY month, model;
      SQL
      ```

      Add up the rows for the provider's models (`claude-*` for Anthropic; the
      OpenAI models for OpenAI) to get its sum for each month. This lists every
      undeclared manual row, not only those on days a poller covered.
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
  key, same cost, same `(developer, issue_id, model, source, fidelity)`) is
  idempotent -- `201`, no new row, and the stored row is left exactly as first
  recorded, token counts included. A re-post with the same key
  but a **different** `cost_usd` is rejected with **`409 Conflict`** (#295): the
  stored cost is immutable (#233), so the correction is refused rather than
  applied. Divergence is judged on the stored **integer** `cost_micro`, so an
  honest retry whose float/FX/rounding jitter rounds to the same micro value is
  **not** a conflict. To change a recorded figure, use the sanctioned override
  below (#346): it replaces the stored cost on the same row and writes an audit
  row. ⚠️ **Do not re-post under a new `idempotency_key` to correct a figure.**
  A new key is a new row, and every spend read adds both, so a correction from
  $100 to $80 is recorded as $180. A distinct key is right only for spend that
  is genuinely additional, such as when the earlier row is not yours. For the
  one row class the override cannot reach, see the pre-#82 `realtime` note below.
- A re-post whose cost matches but whose `(developer, issue_id, model, source,
  fidelity)` differs from the stored row's is rejected **`409`** (#871) -- the
  same identity-mismatch response the override path below returns, and it
  likewise does not say whose row the key belongs to. Nothing is written for
  the caller and the stored row is unchanged. The cost is compared first, so a
  re-post whose cost AND identity both differ gets the divergent-cost `409`
  above. A re-post that changes only token counts is a silent no-op on those
  columns (they are immutable on conflict, #233, #871) -- `201`, NOT a `409`.
  One consequence: a keyed re-post of a **pre-#82** row whose stored fidelity
  is anything outside `daily`/`estimated` (`realtime` is the live example; see
  below) now `409`s even at the same cost, because no request can state that
  fidelity; nothing is written either way. Reuse a key only for a genuine
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
    detail than the plain divergent-cost 409, but the same status code; the
    plain path returns this same response on a same-cost mismatch, #871).
    The 409 body deliberately does not echo which identity the key actually
    belongs to -- defense in depth against a BLIND collision, **not a secrecy
    guarantee** (see the stated trust model below). That tuple is
    the set of stored columns a `/costs` request can vary, except `billed_to`
    (#854), a declaration that is deliberately not compared; `repo`,
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
    request `409`s. No API route or `tierd` command corrects or deletes a
    single row (`DELETE /api/v1/developer/{id}` erases everything that
    developer owns, which is not a correction), so the fix is made directly in
    the database. Do not delete the row and re-post it: `/costs` takes no
    timestamp and stamps the new row with the time it arrives, so the spend
    would move from its original date to today. Re-posting under a new key
    without removing the old row would add the new figure to the old one.

    Instead, change the cost on the row itself, the same one-column change the
    override makes. It keeps the row's date and every other column. Run it on
    the machine running `tierd`; it is safe while `tierd serve` is running.
    Set `DB` to the path you gave `--db` (default `~/.tier/tier.db`), then back
    up and look the row up by its key:

    ```sh
    DB=~/.tier/tier.db
    tierd backup --db "$DB" --out ~/tier-before-cost-fix.db
    sqlite3 -cmd '.timeout 5000' "$DB" <<'SQL'
    SELECT id, developer, issue_id, cost_micro, fidelity, ts
      FROM token_events
     WHERE idempotency_key = '<old key>' AND source = 'api' AND fidelity = 'realtime';
    SQL
    ```

    Replace `<old key>` with the key before you run it. The `SELECT` must show
    exactly the one row you mean to change. Note its `id` (a whole number).
    `cost_micro` is the cost in millionths of a dollar, so $80 is `80000000`.
    Then change that row by its `id`, never by pasting the key again, with the
    `id` and the new figure in place of `<id>` and `<new cost_micro>`:

    ```sh
    sqlite3 -cmd '.timeout 5000' "$DB" <<'SQL'
    UPDATE token_events SET cost_micro = <new cost_micro>
     WHERE id = <id> AND source = 'api' AND fidelity = 'realtime';
    SELECT changes();
    SQL
    ```

    It must print `1`. A `0` means the `id` was not a `realtime` `api` row, and
    nothing changed. The `source` and `fidelity` terms are there so a mistyped
    `id` cannot touch a captured row. Every spend read sums `cost_micro`
    directly, so the new figure shows at once with nothing to rebuild. Unlike
    the override, this writes no `cost_correction_audit` row, so record who
    made the change and why somewhere you keep. The backup is your copy of the
    old figure. Changing the row also changes the digest of every published
    report whose window covered it, so a `tierd verify-report` manifest saved
    before the change will no longer match.
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
    non-`api` source: `jsonl`, `proxy`, `codex-rollout`, `opencode`, `muse`,
    `copilot-api`, and the org pollers) is unreachable from this endpoint.
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
- Each token field has the same 1e12 cap as `/costs`; both client `cost_usd`
  and server-resolved cost must be <= $10,000 per event. Over-limit events
  return `400` naming the limit and the split-across-rows remedy; the whole
  batch is refused. Capture and reprice clamp oversized costs, preserve token
  counts, and retain `cost_clamped=1` clamp history (also in DSAR); the counter counts capture attempts
  and committed reprice changes. Existing data migrates without this ceiling.
  Exact integer `SUM(cost_micro)` is unchanged (#1082).
- ⚠️ **`issue_id` follows an ALLOWLIST here, not the `/costs` rule (#466).** This
  endpoint is the JSONL collector's own transport, and the collector legitimately
  assigns the unattributed sentinel when a message resolves to no issue. It
  therefore **accepts the five exact canonical spellings** -- `unattributed`,
  `unattributed:main`, `unattributed:detached-head`,
  `unattributed:branch-without-issue`, `unattributed:foreign-repo` (#823) -- and
  `400`s every other member of the family, every case variant, and every
  near-miss. An `unattributed:foreign-repo` row must omit `repo` (it is stored as
  `unqualified`, so no foreign repository name is kept); any other `repo` value is
  a `400` that does not echo it.
  *Why the split rather than one rule:* the endpoint validates all-or-nothing, so
  one rejected event fails the whole batch; the shipper treats `4xx` as terminal
  with no retry; and it is stateless, so the next run rebuilds the identical batch
  and fails identically. Applying the strict `/costs` rule here would be permanent
  100% capture loss for anyone who has ever committed on `main` without an issue,
  including their well-formed attributed events.
- **`host` is OPTIONAL and is a PRICING INPUT (#719).** Omitted, an event prices at
  the model-only rate -- exactly how every pre-#719 client behaves, so adding the
  field breaks no existing integration. Supplied, the server prices at the
  host-qualified rate `<model>@<host>` when the active table has one.
  It exists because a source can have a rate that lives ONLY under a host key: the
  `opencode` collector stamps its provider id (`zai-coding-plan`) as the host
  because #712's GLM override rates are keyed on it (the embedded table has carried
  model-only `glm-5.3` rows only since v10, #786). Before this field those events re-priced at the guessed
  `self-hosted-medium` fallback ($0.50/M) with **no signal at all** -- the
  pricing-divergence detector compares the server's figure against the shipper's,
  and the shipper's was the same guess.
  ⚠️ It is client-controlled input selecting a price-table key, so a caller can
  choose which audited rate its tokens bill at. That is the same latitude `model`
  already gives, and it sits inside this endpoint's existing trust model (the
  bearer token is attribution-grade; deliberate misattribution is out of scope
  until an identity layer exists, #65). The endpoint enforces the SHAPE: the
  256-char identifier cap, and `ComputeCostHost`'s rejection of a `model` string
  containing the host separator, so a forged `"model@host"` cannot bypass the host
  lookup.
  ⚠️ **Version skew:** the endpoint uses `DisallowUnknownFields`, so a shipper
  sending `host` to a pre-#719 server 400s the whole batch (all-or-nothing, and a
  4xx is terminal with no retry). The field is `omitempty`, so a jsonl/codex batch
  is byte-identical to what a pre-#719 shipper sent and is unaffected; only a
  batch actually carrying a host is at risk, and it fails loudly.
- **`attribution_rule` is OPTIONAL (#823):** the rule that assigned `issue_id`,
  empty or one of the closed set `store.AttributionRules()` (`branch`,
  `worktree-cwd`, `worktree-toolpath`, `carry`). Omitted, the row stores no rule --
  exactly how every pre-#823 shipper behaves. Any other value is a `400` naming the
  allowed set; the refused value is not echoed. Stored on insert only; a replay
  never rewrites it. Same version skew as `host`: a pre-#823 server `400`s a batch
  that carries the field, so upgrade tierd before the shippers.
- ⚠️ **`developer` gets the STRICT rule here, with NO allowlist (#619).** The
  asymmetry with `issue_id` directly above is deliberate, not an oversight: the local
  collectors this wire admits (`source` must be `jsonl`, `codex-rollout`,
  `opencode` or `muse`) label
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
- **`org` must be the exact string you set as `org` on `POST /api/v1/org_hierarchy`
  or `PUT /api/v1/org_hierarchy/{developer}`.**
  The allocation joins `org_actual_spend.org` to `period_membership.org` (which
  the hierarchy write populates) byte-for-byte — no trimming, no case folding, on
  either write path (`internal/store/store.go`, `actualSpendCTE`;
  `upsertHierarchyTx`). An invoice posted under `Acme` when the hierarchy says
  `acme` allocates to nobody: it is accepted with `201`, but no developer receives
  a slice and Spend Leverage keeps reading `—`.

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

- The alias is joined to its canonical developer for every window, but its **team
  placement changes only from the write on** (#914): the write dates the change
  by the server clock, and events recorded under the alias before it stay in the
  team the alias was in then. `409` when the server clock reads earlier than a
  membership row the write would close.

#### `DELETE /api/v1/developer_alias/{alias}`
- **Scope:** write. **Success:** `204 No Content`; `404` if not mapped.
- Team placement changes from the delete on (#914), as for `POST`. `409` as for
  `POST`; `503` + `Retry-After: 1` when another writer holds the write lock.

#### `GET /api/v1/developer_alias`
- **Scope:** write (admin; discloses the identity map).
- **Success:** `200 OK`, `{"aliases": {<alias>: <canonical>, ...}}`.

#### `PUT /api/v1/org_hierarchy/{developer}`
- **Scope:** write. **Success:** `200 OK`, the stored `HierarchyRow`:
  `{developer, team, division, org}`. Request body: `{team, division, org}`.
- A changed team or division is dated by the server clock (#886); a
  `valid_from`/`valid_to` field is refused `400`, like any unknown field. `409`
  when the server clock reads earlier than the developer's current membership
  began (the host clock stepped back): nothing was written, and the request
  succeeds once the clock is right.
- `503` + `Retry-After: 1` when another writer holds the database write lock.
  **Retryable** — it is a lost race for SQLite's single writer, not a bad request
  and not a broken database. A `500` here still means an unexpected store failure.

#### `POST /api/v1/org_hierarchy`
- **Scope:** write. **Success:** `201 Created`, `{"accepted": <int>}`.
- All-or-nothing bulk import (array of `{developer, team, division, org}`).
  Every row is dated by the same server instant (#886); `409` exactly as on the
  `PUT` above, with nothing written.
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
  nothing matched (which makes a repeated erasure idempotent).
  Each count is the rows this erasure changed in that table.
  `deleted.watcher_checkpoint`, and so `total_deleted`, counts rows tombstoned
  plus rows deleted (#919). `deleted.sealed_person` is always present and counts
  sealed person keys tombstoned, never deleted (#913, #914), including keys of a
  canonical id the person's alias was linked to when the period was sealed,
  which another person may hold now (#975); `total_deleted` includes it, so an
  erasure that only tombstones sealed keys returns `200`, not `404`.
  `deleted.canonical_id_history` is always present and counts history rows
  deleted, including a row recording that another person's alias once pointed at
  the erased id (#975). Available in
  team-aggregation mode (compliance tooling, not a reporting surface).
- `503` + `Retry-After: 1` when another writer holds the database write lock.
  **Retryable, and the erasure did NOT happen** — the transaction is
  all-or-nothing, so nothing was partially deleted; retry to complete the
  request. A `404` means there was nothing to erase; a `500` means an unexpected
  store failure. Only the `503` should be retried automatically.

#### `GET /api/v1/developer/{id}/export` (GDPR Art. 15 access)
- **Scope:** write. **Success:** `200 OK`, `store.DeveloperExport`:
  `{developer, identifiers, token_events, outcomes, actual_spend, org_hierarchy,
  period_membership, hierarchy_membership, quality_events, quality_history,
  repo_repair_audit, push_outcome_commits, push_outcome_audit, developer_alias,
  watcher_checkpoint}` (each a row array grouped by table). `push_outcome_commits`
  and `push_outcome_audit` (#849) are the push-capture commit ledger and its
  audit; a `commit_sha` starting `pre-ledger:` is a marker, not a commit. A
  `push_outcome_commits` row's `push_order` (#938) is its ownership sort key. The
  audit's stored `commit_developer` (another developer, the author of that
  commit) is left out. `hierarchy_membership` (#886) rows are `{id, developer,
  team, division, valid_from, valid_to}`; the stored `written_by` credential
  fingerprint is left out. `outcomes` rows carry `author_type` (#856): the PR
  author's GitHub account type (`User`, `Bot`, `Organization`, `Mannequin`) as
  the webhook recorded it, or `null` when not captured. `404` when the developer
  has no data. Available in team-aggregation mode.

### Read endpoints

#### `GET /api/v1/scores`
- **Scope:** read. **Success:** `200 OK`, `scoresResponse`.
- Query in **developer mode**: `?since=` (YYYY-MM-DD / YYYY-MM / YYYY; default: the **start of the UTC
  day** 90 days back, #746), `?until=` (or `?before=`) exclusive upper bound,
  `?team=`, `?work_type=`, `?repo=`.
  **Any other parameter is a `400`** -- see
  [strict query-parameter validation](#query-parameters-are-strictly-validated-on-the-scores-endpoints-590).
- In **team/division mode**, only `?period=YYYY-MM` is accepted (omit it for the
  newest sealed month). **Any other parameter is a `400`**, including the
  developer-mode filters above.
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
  - 🔴 **`?work_type=` is REFUSED with a `400` in any anonymized (`team`/`division`)
    aggregation mode (#864).** An anonymized install publishes ONE breakdown per
    window: its group rows and the grand total. A second breakdown differences
    against the first to a group below `k` even when every published row has `k`
    people (measured: pooled team rows minus work-type totals recovered a 3-person
    group's $9.21). A value that is not a work type is still the ordinary
    `invalid work_type` `400`. Developer mode honors the filter as before.
  - Query parameters are parsed STRICTLY, not via a lenient decoder: a malformed pair
    (bad percent-encoding, or a `;` -- which Go does not accept as a separator) is a
    `400`, and so is a REPEATED parameter. Both would otherwise be silently dropped or
    silently resolved to the first value, widening the result while looking filtered.
- Fields:
  - `since` (string) -- echoed window lower bound (UTC calendar day). Since
    **#746** this is exact rather than approximate: the default bound is snapped
    back to the start of its UTC day, so the day printed here is the day the
    window actually opens on. Before that it read `2026-05-31` for a window that
    began at `2026-05-31T05:00:58Z`.
  - `price_table` (`{version, effective_date, table_hash, file_hash}`) -- **always
    present** price provenance stamp (#233, extended by #713). `table_hash`
    ("tierpt1:<64 hex>") is a digest of the RESOLVED in-memory table; `file_hash`
    ("sha256:<64 hex>") is a digest of the raw source bytes. See
    [content identity](#price-table-content-identity-713) below.
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
    The `aggregation` discriminator above says which. Groups are **dated**
    (#886): each cost event, outcome and invoice counts toward the group its
    developer held at that event's own time (an invoice at the start of its
    month), read from the append-only `hierarchy_membership` ledger, so a later
    move never changes a past window. A group with fewer than `k`
    contributing developers folds into the residual `"other"` row; a developer
    whose division is empty/unset folds into `"other"` too. A developer counts
    toward a group's floor once, however many rows they carry, and only with
    activity: a developer whose only figure there is paid spend fills no seat.
    🔴 **If the `"other"` row does not reach `k`, the WHOLE response is withheld
    (#864):** `teams` is absent, named rows included, as is `total`, and
    `data_quality.kanon_suppressed` says so. A named row published beside a
    withheld residual differences against any other view that folds that group into
    a residual of its own.
  - `team` (`teamScoreJSON`, `omitempty`) -- populated only when `?team=` filters
    to one team (developer mode only).
  - `team_rollups` (array, `omitempty`, #821) -- **developer mode only.** Partitions
    `total` by dated team membership (#886): each event counts toward the team its
    developer held at that event's own time. One row per team that has at least one
    event in this response, in byte-wise ascending team-name order, then at most one
    **unassigned** row for events whose developer had no team at that time (or one
    mapped to `""`). An event is placed by the dated membership of the identifier it
    was recorded under, as the anonymized modes place it (#914): an alias carries its
    person's team from the moment it was added, re-pointed or deleted, and events
    recorded under it before then stay where they were. Each row is `teamScoreJSON` plus `unassigned`
    (bool, **always present**); the unassigned row has `unassigned: true` and **no
    `team` key**, so it can never collide with a team a customer happens to name.
    Every event is in exactly one row (a developer who moved inside the window
    can appear in two), so the rows' `weighted_points` and
    `total_cost_usd` **sum to `total`'s** (to float64 rounding: the grouped sum
    re-associates the same additions). `tier`, `cost_per_point`, `coverage_pct`,
    `spend_leverage` and `ranked` are per-row ratios and do **not** sum. A team with
    an `org_hierarchy` row but no developer in the window has no row.
    - **The mode rule alone decides presence: developer mode, whenever `total` is
      present**, over the same population: the window and `?repo=` narrow it;
      `?team=` and `?work_type=` do **not** (`?team=` fills `team`, `?work_type=`
      narrows only `work_types`). Under `?team=X` the `X` row equals the `team` block.
    - **Top level only.** `work_types` segments carry no `team_rollups`.
    - 🔴 **ABSENT in any anonymized mode (`team`/`division`).** A per-team rollup has
      no k-floor; in those modes it would re-expose every sub-k team the k-anonymized
      `teams` array folds into `"other"`, and `total - Σ named teams` is the
      differencing channel #593 closes. It is therefore also absent whenever
      `data_quality.kanon_suppressed` is set: k-anonymity suppression only ever
      fires in an anonymized mode, and the #593 strip pass withholds `team_rollups`
      together with `total` regardless.
    - ⚠️ **Read scope sees team structure.** `/scores` is read-scoped (#190), so in
      developer mode the read-only viewer token now sees every team name and each
      team's totals, from which membership can be inferred. This is operator ruling
      #821 = B: the dashboard's team panel needs it. `GET /org_hierarchy`, the
      developer->team map itself, stays write-scoped.
    - Not named `teams`: that key is frozen as the k-anonymized array whose rows are
      labelled by the `aggregation` discriminator.
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
    stamp would otherwise mask. **Developer mode only (#864):** it reads every row
    of the window, pseudo-developer spend included, so in `team` and `division`
    mode it is omitted, and there `price_table.version` names only the active
    table, not every table that priced the window. A floored warning is to return
    through a follow-up issue. It also carries the **true attribution-coverage**
    fields (#351):
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
      (see caveat below). **Developer mode only (#864):** `team` and `division`
      mode never publish it, because beside the group rows and `total` it bounds
      one person's spend even when *k* counted people carry the linked and the
      unlinked cost.
    - `attribution_coverage` (string, `omitempty`, #864) -- `"not_shown"` on every
      anonymized window that has spend, here and in each `/scores/compare`
      window's `data_quality`; absent in developer mode and on a window with no
      spend. It has that one value only. In these modes treat an absent share as
      "not shown", never as full coverage, whether or not this field is present.
    - `excludes_unattributed_spend` (`<bool>`, `omitempty`, #864) -- `true` on every
      anonymized response, here and in each `/scores/compare` window's
      `data_quality`; absent in developer mode. Spend recorded under the
      `unattributed` pseudo-developer (an org-usage poller's remainder, proxy
      requests with no developer header) is dropped before any row, `total`, count
      or share is computed, so no difference of published figures is that spend.
      It is constant: it does not say whether a window held such spend. The amount
      is visible in developer mode, and `GET /api/v1/org_actual_spend` (write
      token) carries the invoices. The pseudo-developer never holds paid spend, so
      `spend_leverage` here is captured list-price spend divided by paid spend over
      the same people.
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
      descending cost. `bucket` is the stable label `unattributed:main` (spend
      recorded on `main` or `master`, so TIER could not link it to an issue),
      `unattributed:detached-head`,
      `unattributed:branch-without-issue`, `unattributed:foreign-repo` (#823: a
      worktree of a repository this install does not configure; not counted in
      `exploratory_cost_share`; its rows always carry `repo` = `unqualified`, and
      `/events` refuses any other value), or the base `unattributed` (host-blind
      producers). `share` is of TOTAL window cost, so the buckets + `attributed_cost_share`
      sum to 1.0. Present only when the window has unattributed spend. **Developer mode
      only (#864):** a bucket one person's spend fills publishes that spend.
    - `exploratory_cost_share` (`<float>` in `[0,1]`, `omitempty`) -- the
      `unattributed:main` bucket's share of total window cost: spend recorded on
      `main` or `master`, so TIER could not link it to an issue. This spend stays in
      the denominator. The field reports how large it is; it does not remove it. The
      field name is historical and is kept because it is the wire contract: the value
      does not say whether the work was exploration, planning or anything else. A
      common cause: the session was started in the main checkout, and the work ran
      in a separate worktree. Claude Code usually records the branch checked out in
      the directory where the session started (measured 2026-09-26 across one
      install's session files); a later `cd` into another worktree is not recorded,
      and for harness worktree agents TIER substitutes the session's last named
      branch. Start the session inside the worktree, or on a branch named with the
      issue number, and the spend is attributed to the issue. That helps only if the
      worktree's branch names the issue; otherwise the spend lands in
      `unattributed:branch-without-issue`. Pointer-emitted so a real `0.0` carries; present
      alongside `unattributed_buckets`, so **developer mode only (#864)**.
    - **Cost horizon (#512)** -- four fields that say whether this window starts
      before TIER began recording cost. They describe the requested scope: the whole
      installation without `?repo=`, and only that repository with it. All four are
      absent when the scope holds no cost at all (on a `?repo=` read, when that
      repository has no cost rows, even if other repositories do), and also when the
      server could not look the horizon up (it logs the error and still returns the
      scores), so absence never means "covered". Otherwise the first three are
      always present. Name-free; they carry in both modes. See
      [interpreting-the-number.md](./interpreting-the-number.md) for how to read them.
      - `cost_coverage_start` (RFC 3339 string) -- the time of the earliest cost
        event in the scope: the installation's cost horizon without `?repo=`, or
        that repository's with it.
      - `window_predates_cost_capture` (`<bool>`) -- `true` when `since` is earlier
        than `cost_coverage_start`, which means the window counts outcomes against
        cost that was never recorded and the TIER reads too high. An explicit
        `false` is sent when the window is covered, so silence never stands in for
        "checked and clean".
      - `cost_coverage_safe_since` (`YYYY-MM-DD`) -- the earliest `since` value
        that clears the horizon. When the horizon falls partway through a day, this
        is the following day, because `since` always starts at midnight.
      - `source_coverage_start` (`{<source>: <RFC 3339 string>}`, `omitempty`) --
        each capture source's own earliest cost event in the scope. Sent only when
        more than one source has recorded cost in the scope: a window can clear the
        overall horizon and still start before one source began.
    - `uncounted_active_ids` (`omitempty`, #856) -- anonymized (`team`/`division`)
      modes only: how many identifiers with spend or merged work in the window did
      NOT count toward the k-anonymity floor, by reason. Shape
      `{manual_only, push_only, not_on_roster, bot}`, all integers; each identifier
      is counted once, under the first reason that applies in the order `bot`,
      `not_on_roster`, then `manual_only` (it has a hand-entered `POST /costs` row
      and no captured activity) or `push_only` (its only activity is push-captured
      merged work). Stated **once per window, for the whole org**: it does not move
      with `?work_type=` or `?team=`, and it never appears on a `teams` row, a
      `work_types[]` segment or `/scores/compare`. Absent when every active
      identifier counted, and in developer mode. The `unattributed` pseudo-developer
      (#853) never counts and is never reported here: it is not a person anyone
      could put on the roster.
      - **The counting rule it reports on (#856).** An identifier counts toward *k*
        only when it has outcomes or cost in the window, one of the identifiers
        its person's cost or outcomes in the window were recorded under (aliases
        join identifiers into one person) holds its own roster membership
        overlapping the window, so an identifier idle in the window never rosters
        another's activity (#914), it is not
        a bot (GitHub account type `Bot` on an outcome the webhook recorded in the
        window, a `[bot]` login suffix, or a fixed list of bot logins for older
        rows), and it has captured evidence in the window: a cost
        row from any source but `api`, or an outcome whose `source` is not `push`.
        An uncounted identifier's figures stay summed in its row and in `total`,
        except a pseudo-developer's, which an anonymized mode drops (#864, see
        `excludes_unattributed_spend`).
      - **Per-measure floor.** A group is named, and a residual published, only when
        at least *k* counted people carry each figure it publishes: *k* with
        captured cost for `total_cost_usd` (a hand-entered `POST /costs` row fills no
        cost seat), *k* with outcomes for `weighted_points`, *k* with paid spend for
        `actual_paid_usd`, and both populations for each ratio: `tier` and
        `cost_per_point` (cost and outcomes), `spend_leverage` (cost and paid).
        `coverage_pct` splits cost into a realtime and a non-realtime share, and
        each non-zero share needs *k* counted carriers with captured cost in that
        share (a hand-entered fee is no carrier of the non-realtime share). A
        figure that no row carries (it is exactly `0`) needs none.
      - **Remedy:** capture those people's work through a collector or the proxy,
        and put them on the roster. The floor protects what a **read-token** holder
        sees; the write token can edit the roster and aliases and is an admin
        credential until #908.
    - `kanon_suppressed` (`omitempty`, #593) -- present when a sub-k residual cohort
      was WITHHELD from an anonymized (`team`/`division`) response. Shape
      `{developers, k_anonymity, withheld_total, withheld_cost_composition,`
      `withheld_teams, withheld_segment_reconciliation}`. `withheld_teams` (#864) is
      `true` whenever the block is present: the whole response is withheld, named
      rows included. `withheld_cost_composition` and `withheld_segment_reconciliation`
      are `false`: an anonymized mode never builds either block (#864).
      Name-free: counts only, never an identity.
      - `developers` counts people under the counting rule above (#853, #856) and may
        be `0`, when the withheld group holds only uncounted identifiers. It may also be `k_anonymity`
        or more, when the residual was withheld because one of its figures is carried
        by fewer than *k* counted people (the per-measure floor). Widening the window
        clears a suppression only if it brings enough counted people into the
        residual (people with no team, or in a team too small to have its own row).
      - **When it is present, `teams` and `total` are ABSENT** (#864; before it the
        named rows stayed and only `total`, `cost_composition` and
        `segment_reconciliation` went, and the visible rows no longer summed to the
        window). That is deliberate. Before
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
      - **One further placement carries the same shape** (#593): `/scores/compare`
        carries a **top-level** `kanon_suppressed` rather than one inside a window's
        `data_quality`, because the two-window fold suppresses a cohort from BOTH
        sides together or from neither -- attaching it to one window would imply the
        other was unaffected. (`work_types[].kanon_suppressed` was a second placement
        until #864 removed work-type segments from anonymized responses.)
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
      ⚠️ **The two sides do not share a lower bound.** `token_events`/`cost_usd`
      count the ATTRIBUTION BAND `[since - 14d, until)` and `outcomes` counts
      `[since, until)`, because a scope can suppress a repo-blind row inside an
      outcome's 14-day look-back (`store.UnqualifiedExclusionWindow`).
      🔑 **The manifest pins this same quantity** as
      [`report_manifest.repo_scope_excluded`](#get-apiv1report_manifest) (#751) --
      but in **micro-dollars** (`cost_micro`), and it is emitted on **every**
      scoped manifest including an all-zero one, where this block is omitted when
      clean. Two surfaces, two jobs; do not diff them field-for-field.
    - `spend_leverage_suppressed` (`<bool>`, `omitempty`, #590) -- `true` when a repo
      scope suppressed `spend_leverage` / `actual_paid_usd`, which are
      installation-wide by construction and cannot be scoped (see `?repo=` above).
      Absent entirely on an unscoped read: suppression is a property of scoping, not a
      standing caveat. Without this field a scoped response's `actual_paid_usd: 0`
      would be indistinguishable from "no actual spend has been recorded", which is a
      materially different statement. Name-free.
  - `work_types` (array of `workTypeSegmentJSON`, `omitempty`) -- the
    **authoritative** type-segmented view (#187): one entry per `work_type`, each
    `{work_type, developers?, total?}` scored over ONLY that category. This is the
    surface for comparing developers; the pooled `developers`/`total` above are
    retained for back-compat and are not a cross-type ranking. 🔴 **Developer mode
    only (#864):** absent in any anonymized mode, where the group rows are the one
    breakdown; the per-segment `aggregation`, `teams` and `kanon_suppressed` keys
    existed only there and are gone. #937
    (an audit that solves the published sums) is the route to bring a work-type view
    back to anonymized modes.
  - `segment_reconciliation` (`omitempty`, #466) -- accounts for the window's WHOLE
    spend against the `work_types` segments above, so a reader can see how much the
    segmented view leaves out and why. Omitted when the window has no cost rows. It is ALSO
    dropped — silently, with a server-side ERROR log — when the block is not fit to
    publish: an accumulator saturated at the int64 ceiling, or any figure came out
    negative. That third case is the one absence this API does NOT declare on the wire,
    because it signals a server fault rather than a policy withhold; its only reachable
    trigger is a window whose summed spend approaches ~9.2e12 USD. Shape:
    `{developers: [row], total: row}` where `row` is
    `{developer, window_cost_usd, window_cost_micro, outcome_linked_cost_usd,`
    `outcome_linked_cost_micro, no_outcome_cost_usd, no_outcome_cost_micro,`
    `unattributed_cost_usd, unattributed_cost_micro}`. 🔴 **Developer mode only
    (#864):** the whole block is absent in any anonymized mode, which publishes no
    segments to reconcile, and whose no-outcome and unattributed figures would be a
    second breakdown of the window's cost. `total` is the always-present name-free
    rollup and carries no `developer`. ⚠️ A
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
    pricing >= $4/M base input (the frontier/reasoning tier;
    `PremiumInputRateThresholdPerM`, lowered from $5/M with price table v10;
    releases up to v0.4.1 still use $5/M). `by_class` is TOKEN
    counts, not allocated dollars (a stored blended `cost_micro` is not exactly
    splittable per class); `by_model.premium` and the model breakdown are host-aware
    (#300), so an open-weights model split across hosts stays multiple rows.
    🔴 **Developer mode only (#864):** absent in any anonymized mode. Naming no
    individual is not enough: every field but `total_cost_usd` splits the window's
    cost a second way, and a model one developer used published that developer's
    spend in `by_model` (measured: $4.44). `total_cost_usd` restates `total`, which
    stays. #937 is the route to bring
    a per-model view back to anonymized modes.
- `developerScoreJSON`: `developer, tier, weighted_points, total_cost_usd,
  actual_paid_usd, spend_leverage, coverage_pct, exploratory_cost_share,
  cost_per_point, sample_n, ci_low, ci_high, cost_per_point_ci_low,
  cost_per_point_ci_high, ranked, flagged_outcomes`. `exploratory_cost_share` (attribution
  refocus, Option B) is this developer's `unattributed:main` cost / their total window
  cost -- the per-developer companion to `data_quality.exploratory_cost_share` (the name
  is historical; it does not say the work was exploration), naturally
  k-anon-safe because developer rows are not emitted in team-aggregation mode. The array
  is **sorted by developer identifier, never by TIER**; the `ranked` flag (#133) tells
  a client which rows clear the evidence floor. The dashboard keeps that order and
  never sorts the rows by TIER (#830); the v0.4.1 dashboard put ranked rows first,
  highest TIER first. `cost_per_point` (#239) is
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
  deltas plus a CI-overlap significance flag out. In developer mode it reuses the
  SAME windowed scores computation as [`GET /scores`](#get-apiv1scores), so a
  compared score matches `/scores` for the same window. It is an **aggregate
  view like `/scores`**; in anonymized (team/division) mode it compares sealed
  calendar months, refolding rows with the two-window intersection, so group
  rows can differ from each month's standalone `/scores` report.
- **Developer-mode only:** free-window queries (each window mirrors `/scores`'
  `since`/`until` grammar and validation, #276): `?since_a=`, `?until_a=` (window A = "before"); `?since_b=`, `?until_b=`
  (window B = "after"). Each `since_` defaults to the start of the UTC day 90 days
  ago (#746, the same default `/scores` uses); each `until_` is an optional
  exclusive upper bound that must be strictly after its `since_`
  (`400` otherwise). Each `since_` is retention-checked (`422` if it predates
  the retention horizon), not `until_`. Every delta is **B - A**.
- **Team/division mode:** use `?period_a=2026-05&period_b=2026-06` for two closed,
  sealed months. Give both parameters, with A earlier than B, or omit both for
  the two latest sealed months. An unsealed month returns `404`; incompatible
  sealed configurations return `409`. Reads never seal a month. Free bounds
  are refused with `400`, even when they describe whole months or are empty.
  For the developer-mode example
  `?since_a=2026-05-01&until_a=2026-06-01&since_b=2026-06-01&until_b=2026-07-01`,
  the exact `error` is:

  ```text
  ?since_a=, ?until_a=, ?since_b=, ?until_b= not accepted for anonymised reports (#913): they are served per sealed calendar month, so two overlapping windows cannot be differenced to isolate one person's figures. Remove ?since_a=, ?until_a=, ?since_b=, ?until_b= and select two months with ?period_a=YYYY-MM and ?period_b=YYYY-MM (neither: the two latest sealed months)
  ```

- **Any parameter outside the mode's allowlist is a `400`** (#590) -- including `?repo=`, which this endpoint
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
    as [`/scores`' `data_quality`](#get-apiv1scores). Each block also carries the
    cost-horizon fields (`cost_coverage_start`, `cost_coverage_safe_since`,
    `window_predates_cost_capture`, and `source_coverage_start` when more than one
    source has recorded cost), checked against that window's own `since`; they are
    omitted when the store holds no cost. Sealed team/division comparisons omit
    `cost_coverage_safe_since`, as do the stored monthly bodies. The #351 coverage shares are a
    `/scores`-only surface; in an anonymized mode each block carries
    `attribution_coverage` as on `/scores`. `until` is omitted when the window is
    open-ended.
  - `price_table` (`{version, effective_date, table_hash, file_hash}`) -- **always
    present** active-table stamp (#233, extended by #713); per-window pricing mixes
    surface via each window's `data_quality.mixed_price_versions` (developer mode
    only).
  - `mode` (string) -- `"developer"` in developer mode; in team/division mode,
    echoes the level both months were sealed under (`"team"` or `"division"`).
    It tells a consumer whether `developers` or `teams` carries the rows (the
    same discriminator as `/scores`' `aggregation`).
  - `developers` (array of `developerDeltaJSON`) -- per-developer deltas in
    developer mode; an explicit **empty array** in any anonymized mode (#185, #270),
    never a named row. Each row: `{developer, present_a, present_b, a, b, delta_tier,
    delta_weighted_points, delta_total_cost_usd, significant}`. `a`/`b` are
    `scoreSideJSON` (`{tier, weighted_points, total_cost_usd, actual_paid_usd,
    spend_leverage, coverage_pct, cost_per_point, sample_n, ci_low, ci_high,
    ranked}`). `cost_per_point` (#239) is the same points-guarded
    `total_cost_usd / weighted_points` (`null` on a zero-point side, #472) `/scores`'
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
    (an interval is a per-developer signal, #133). 🔴 **Absent, with
    `kanon_suppressed` declared, when the comparison's `other` residual or EITHER
    window's own `other` bucket (the one `/scores` folds for that window alone) does
    not reach `k` (#864).** The intersection folds into `other` a group that is
    sub-k in the other window, so without the per-window check a comparison's
    `other` or `total` minus that group's row from `/scores` or a second comparison
    was the window's own sub-k residual.
  - `total` (`teamDeltaJSON`, `omitempty`) -- name-free grand-rollup delta across
    every developer in each window. `significant` is always `false`. ⚠️ **ABSENT in
    two distinct cases:** both windows are empty, OR a sub-k residual was suppressed
    (#593) -- there the delta minus the named group deltas would reconstruct the
    hidden cohort. `kanon_suppressed` is what tells the two apart. Developer mode
    never suppresses, so there it is absent only for two empty windows.
  - `kanon_suppressed` (`omitempty`, #593) -- **top-level**, present when a sub-k
    residual was withheld from the comparison. Same shape as
    [`/scores`' `data_quality.kanon_suppressed`](#get-apiv1scores):
    `{developers, k_anonymity, withheld_total, withheld_cost_composition,`
    `withheld_teams, withheld_segment_reconciliation}`. It sits at the top level
    because the two-window fold withholds a residual from BOTH sides or from
    neither; the residual is withheld when EITHER side is unsafe, and since #864
    the whole comparison goes with it (`withheld_teams` is `true`).
    - `developers` counts people under #856's rule and **may be `0`**, when the
      withheld group holds only uncounted identifiers. It is the largest count among
      the withheld residuals (the comparison's two sides and each window's own), a
      magnitude hint and not a union.
    - `withheld_total` is `true`. `withheld_cost_composition` and
      `withheld_segment_reconciliation` are always `false` here: compare ships
      neither sidecar, so none was withheld.
    - Never emitted in developer mode.
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

#### `GET /api/v1/report_manifest`
- **Scope:** read (satisfied by EITHER the viewer token or the write token).
  **Success:** `200 OK`, `reportManifestJSON`.
- The `(predicate, as-of)` identity of the report `GET /scores` returns for the
  same window and scope (#715). A window predicate ALONE is an **unstable
  identity**: late JSONL ingestion adds rows carrying yesterday's `ts` that
  legitimately join "the same window", so two runs of the same query over the
  same `[since, until)` can return different numbers and both be correct.
- Query parameters: `since` (optional, defaults as on `/scores`), `until` (with
  the legacy `before` alias), `repo`. The allowlist is **narrower than
  `/scores`'**: `work_type` and `team` are **rejected with `400`**, not ignored,
  because neither changes any field below -- accepting them would let a caller
  believe they held a manifest for a filtered report.
- **`?repo=` is refused with `400` in any anonymized (team/division) mode**
  (#185, #270), by the same shared guard `/scores` uses: this endpoint publishes
  per-repo ROW COUNTS over a caller-chosen window, and a cohort count is exactly
  the quantity a k-anonymity floor exists to protect.
- Top-level fields:
  - `manifest_schema` (string) -- the scheme tag, currently `"tiermanifest1"`.
    It is the rollback seam, not decoration: it lets a consumer tell "produced by
    a DIFFERENT scheme" apart from "describes a different database state". Bumped
    when an existing field's MEANING changes; **adding** a field does not bump it.
  - `since`, `until` (`omitempty`), `token_since` (strings) and `repo`
    (string, `omitempty`) -- the predicate. The three timestamps are **RFC3339
    UTC instants, not calendar dates**. An **absent `until` means open-ended**,
    which is a different report from one bounded at today's date, so it is never
    backfilled with a synthetic "now".
    ⚠️ **The reason the instant is load-bearing MOVED with #746, it did not go
    away.** It used to be that an omitted `?since=` resolved to `now - 90d`
    carrying a time of day, so stamping a bare date would give two windows eight
    hours apart one identity. That case no longer arises from this server: the
    default is snapped to the start of its UTC day and every explicit bound
    already parses to midnight, so **every bound this emitter publishes is
    midnight UTC**. The field stays an instant because its type is shipped, and
    because the consumer side must keep handling instants a *different* producer
    -- a hand-written or pre-#746 manifest -- can carry.
  - ✅ **Since #746 the default shape is re-runnable.** A manifest requested with
    no window at all now carries a midnight-UTC `since`, which `/scores` can be
    re-queried with, so `tierd verify-report` re-runs it normally. Before #746
    the default was the one shape that could not be fed to the verifier at all.
    ⚠️ *Re-runnable is not "returns 0"* — an open `until` means the re-run covers
    `[since, ∞)` as the database stands now, so later ingestion is a legitimate
    `1` (DIVERGED). Take the manifest and the `/scores` body on the same UTC day:
    each request resolves the default separately, and the two bounds differ by a
    full 24h across a midnight.
    🔴 **The refusal is still live and still correct for any OTHER producer.**
    `/scores` accepts only whole-day bounds, so a manifest whose `since` is not
    midnight UTC -- hand-written, or emitted by a pre-#746 build -- has no query
    that reproduces its window, and `verify-report` exits **2 (could not check)**
    rather than truncating the bound, because truncating would re-run a
    *different* window and compare its numbers against this manifest's.
  - ⚠️ **`token_since` is `since - 14d` and is NOT decoration.** The scoring
    path does not read `token_events` only inside `[since, until)`: the #136
    zero-token tripwire builds a per-outcome window
    `[merge - AttributableWindow, merge]`, so an outcome near the lower edge is
    funded by events up to 14 days before `since`. `watermarks.window` therefore
    covers `token_events` over `[token_since, until)` and `outcomes` over
    `[since, until)` -- **the two sides do not share a lower bound**, and
    `token_event_count` is a count over the attribution band, legitimately larger
    than the report window alone suggests. A watermark bounded at `since` would
    hold still while a late-ingested row in that band cleared the tripwire and
    changed `/scores`.
  - `aggregation` (string) -- `developer` / `team` / `division`. **Always
    present**, unlike `/scores`, which omits it in developer mode: a manifest
    exists to be diffed, and a key that is absent in one mode makes "developer"
    and "the key was dropped" the same reading.
  - `k` (integer, `omitempty`) -- the k-anonymity cohort floor, emitted **only**
    in an anonymized mode, because that is the only mode in which any floor is
    applied. Publishing the configured integer in developer mode would assert a
    protection that is not in force.
  - `price_table` (object) -- `version`, `effective_date`, `table_hash`,
    `file_hash` (#713). ⚠️ **`source` is deliberately absent.** It is a local
    filesystem path; the 2026-08-28 ruling on the #713 review confines it to
    `tierd score-log` (a local CLI printing to the invoking operator's own
    terminal), because a served surface must not disclose the server's directory
    layout. `table_hash` already answers what reproducibility asks -- "are these
    the same PRICES" -- and answers it better than a path.
  - `rubric` (object) -- `version`, the weight rubric (#239).
  - `tool_version` (string), `commit` (string, `omitempty`) -- the binary.
    **Not redundant** with the two blocks above: `Open()`-time migrations rewrite
    `token_events` in place with no ledger row at all, so a binary upgrade is a
    report input that no watermark can see, and this is what makes it visible.
  - `watermarks` (object) -- the as-of half, split into two sub-objects
    because they are read over different predicates and carry different
    disclosure risk:
    - `watermarks.window` -- `max_token_event_id`, `token_event_count`,
      `max_outcome_id`, `outcome_count`. Windowed and repo-scoped.
      🔴 **OMITTED ENTIRELY in any anonymized (team/division) aggregation mode**,
      with a top-level `kanon_suppressed` object declaring the withhold. These
      are unfloored **counts of work** over a caller-chosen window, and #593
      established that narrowing `?since=` alone -- with no `?repo=` involved --
      shrinks a cohort below the k-anonymity floor. `/scores` withholds `teams` and
      `total` for exactly those windows;
      publishing row counts there would hand back the activity volume of the one
      contributor `/scores` declined to name, sweepable day by day. The withhold
      is deliberately blunt (every anonymized request, not only sub-k windows):
      the precise alternative needs the real `KAnonSuppression` signal, which
      costs a full window load, and a cheaper approximation would be a second,
      divergent floor that errs toward disclosure.
    - `watermarks.ledgers` -- `max_<ledger>_id` and `<ledger>_count` for
      `quality_history`, `reprice_row_audit`, `cost_correction_audit`,
      `repo_repair_row_audit` and `push_outcome_audit` (#849). **Unwindowed and unscoped** (see below), and
      present in every mode: install-wide and window-independent, so it carries
      no count of the caller's chosen population.
    **Every key within a present sub-object is always present**; a `0` is a real
    reading ("nothing here yet") and is never dropped.
  - `events_digest`, `outcomes_digest` (objects, `omitempty`) -- `value` and
    `rows` (#716, wired by #740). `value` is `"tierdig1:"` plus 64 hex
    characters: a content identity over the ROWS the report was computed over.
    🔴 **This is the only pin that can see an in-place `UPDATE`**, which a
    `MAX(id)`/`COUNT(*)` watermark pair structurally cannot -- a reprice, a
    fidelity flip or an edited `merge_commit_sha` inside an already-published
    window moves no id and no count. `rows` is the **denominator that proves the
    digest was earned**: an empty window yields a perfectly well-formed `value`,
    and only `rows` separates "this window genuinely holds nothing" from "this
    measured nothing", so the two are always published together.
  - ⚠️ **The two digests do not cover the same window, for the same reason
    `token_since` exists.** `events_digest` covers `token_events` over
    `[token_since, until)` -- the attribution band -- and `outcomes_digest`
    covers `outcomes` over `[since, until)`. `events_digest.rows` therefore
    equals `watermarks.window.token_event_count` and `outcomes_digest.rows`
    equals `outcome_count`. Digesting both over `[since, until)` would leave a
    late-ingested band row outside the identity; digesting both over the band
    would make an outcome the report never read a false alarm.
  - 🔑 **Both digests honour `?repo=` (#747), and the scope is part of the hashed
    identity.** A scoped manifest publishes digests covering only the rows that
    report read -- so another repository's ingestion cannot move them, and the
    two `rows` equalities above hold under a scope exactly as they do
    fleet-wide. Three consequences:
    - A **scoped** digest and a **fleet-wide** digest are **different values even
      over identical rows**; the predicate is hashed in so that a scope change
      reads as a change rather than as silent agreement. ⛔ Never compare one
      against the other.
    - Scoping is **strict**, matching a scoped `/scores`: rows carrying the
      reserved `unqualified` repository sentinel are **excluded**, and the
      under-count that causes is disclosed by `/scores`' `data_quality` block as
      `repo_scope_excluded`. ✅ **That disclosure figure now has a pin of its
      own** — the top-level `repo_scope_excluded` object below (**#751**). It is
      *not* covered by these digests and never will be: widening the predicate to
      reach the sentinel would over-attribute every repo-blind row in the fleet
      to whichever repository the caller named, which is exactly what #590 closed.
    - `?repo=` is canonicalized (lowercased, `.git` and stray slashes stripped)
      **before** it reaches the digest, so `?repo=Acme/Tier.git` and
      `?repo=acme/tier` return the same `value`. `manifest.repo` echoes the
      canonical slug, never the caller's spelling.
  - `digests_omitted` (string, `omitempty`) -- present **exactly when** the two
    digests are absent, naming the reason. An undeclared absence would read as
    "this server publishes no digests", which is a different and false
    statement. One reason remains:
    - **anonymized mode** -- `rows` is an unfloored count of work over a
      caller-chosen window, the same quantity `watermarks.window` is withheld
      for (#593). `kanon_suppressed.withheld_digests` is `true` in this case.

    ⚠️ A second reason -- *a repo-scoped manifest* -- applied until #747, when
    the digest gained a repo predicate. The field is **not** removed: it is what
    separates "withheld, here is why" from "this server publishes none", and
    that distinction is independent of how many reasons exist.
  - `repo_scope_excluded` (object, `omitempty`) -- `token_events`, `cost_micro`,
    `outcomes` (**#751**). The repo-blind rows a strict `?repo=` dropped: the
    pinned form of what `/scores` discloses under the same key in `data_quality`.
    🔴 **It exists because it is the one published figure the digests above
    structurally cannot cover.** A scoped `/scores` still *reads* the
    `unqualified` sentinel rows in order to publish that disclosure, while a
    scoped digest excludes them by design and a scoped manifest carries no
    fleet-wide digest. Measured before this field: an in-place reprice of a
    repo-blind row moved `repo_scope_excluded.cost_usd` from `9` to `14` while
    the scoped digests **and** the scoped watermarks stayed byte-identical, and
    `tierd verify-report` printed `REPRODUCED` over changed served bytes.
    - **Present on every scoped manifest, absent on every fleet-wide one.**
      `repo` present ⟺ this object present. A fleet-wide report excluded nothing
      -- the sentinel rows sit inside its own digests -- so there is no such
      published figure to pin, and emitting an install-wide repo-blind cost for a
      request that named no scope would add disclosure rather than remove a gap.
    - ⚠️ **It is emitted even when every count is zero**, which deliberately
      differs from `/scores`, where the block is *omitted* on a clean window
      because there the absence is the signal to a human reader. Here it is a
      **pin**: an omit-when-clean pin is vacuous exactly when it matters most,
      because a window clean at publish time into which a sentinel row later
      lands would verify as `NOT PINNED`, and an absent check reading as a
      passing one is the defect the digests were added to close.
    - ⚠️ **`cost_micro`, not `cost_usd`.** `/scores` publishes dollars; the
      manifest pins the stored integer micro-dollars, because a pin is compared
      for **equality** and float equality over a JSON-round-tripped dollar figure
      can manufacture a divergence. `cost_usd == cost_micro / 1_000_000`.
    - ⚠️ **`token_events`/`cost_micro` count the ATTRIBUTION BAND**
      `[token_since, until)`, while `outcomes` counts `[since, until)` -- the
      same asymmetry `token_since` exists for, because a scope can suppress a
      repo-blind row inside an outcome's 14-day look-back. A consumer
      recomputing over the narrower window compares two different populations.
    - It cannot appear in an anonymized mode: `?repo=` is refused there, so a
      scoped manifest is always a developer-aggregation one.
    - **What `tierd verify-report` prints** -- four outcomes and a silent fifth:
      no line at all (fleet-wide, absent -- there is no pin to miss);
      `NOT PINNED` (scoped, absent -- a pre-#751 emitter or a hand-written file);
      `NOT CHECKED` (fleet-wide but present -- reported, never counted as
      agreement, because the value corresponds to no served number);
      `UNCHANGED`; and `CHANGED`, whose detail names *which* of the three moved.
      When `token_since` disagrees with the verifier's band, the token leg is
      not compared: `NOT CHECKED`, or `CHANGED` only if the outcome count moved.
      A run with such a withheld pin and nothing `CHANGED` exits `2`, not `0`.
      ⚠️ The presence rule above is an **emitter** rule -- the verifier tolerates
      a file that breaks it rather than refusing.
  - ⚠️ **Cost, because this is a request path.** The digests are **two full-window
    scans**. Measured (Apple M5 Max, 20,000 in-window `token_events` + 2,000
    outcomes, 2026-08-29): the watermark read is 1.42--1.49 ms/op, the digests
    add 28.5--34.6 ms/op -- so this endpoint went from ~1.4 ms to ~30--36 ms, a
    ~20x increase scaling linearly with the window. That is affordable because a
    manifest is fetched once per published report. It is **why the digests are
    on this endpoint and not on `/scores`**, which already pays four full-window
    scans and is polled.
    ⚠️ **#751 added a third read, and it was measured rather than assumed.** The
    exclusion counts are two window aggregates whose `repo = ?` filter **no index
    can serve here**, so both touch the table for every in-window row. ⛔ Not
    because there is no repo index -- `outcomes` carries one with `repo` leading
    (`idx_outcomes_push_daily_repo`), unusable only because it is **partial** on
    `source = 'push'`, which this query does not imply. The plans are pinned by a
    test rather than argued in prose.
    🔑 **Measured in ONE interleaved run (watermark, exclusion, digests per
    round), not appended to the two figures above** -- which is why its watermark
    and digest legs read slightly differently from the #740 numbers, and why the
    ratios below are computed within that run rather than across the seam:
    watermark 1.44--1.78 ms/op, **exclusion 2.98--4.56 ms/op**, digests
    28.7--36.5 ms/op (37--48% CPU idle across the rounds -- read as ceilings).
    That is **two to three times** the whole watermark read and **+9.9% to
    +11.9%** on the endpoint, and it is the same query a scoped `/scores` already
    runs for the same window.
  - `kanon_suppressed` (object, `omitempty`) -- `withheld_window`,
    `withheld_digests`, `k_anonymity`, `aggregation`, `reason`. Present **only**
    when a withhold happened. A manifest that went silently quiet would make
    "absent" and "this install has no rows" the same reading, which is the
    failure the `/scores` `kanon_suppressed` block exists to prevent. Both
    `withheld_*` flags are written explicitly rather than inferred from a missing
    key, so this object is a **complete** statement of what k-anonymity removed.
- **Why five mutation ledgers and not one.** `outcomes.quality` is **mutated in place**, so
  an as-of stamp keyed on `token_events.id` alone CANNOT bound a report -- an
  outcome's quality, a direct multiplier on every weighted point published, can
  change with **no new row** in either obvious sequence. The five append-only
  mutation ledgers are what make such a revision visible. Note also that
  `token_events.id` and `outcomes.id` are **separate `AUTOINCREMENT` counters**;
  neither bounds the other.
- **The two windowed sequences are scoped to `[since, until)` and `?repo=`; the
  five mutation ledgers are NOT.** Their `ts` is the instant of the MUTATION, not
  of the row mutated, so a revision made today against a June outcome would be
  invisible to a June-windowed ledger read. Unwindowed is also the conservative
  direction: it can report a change that did not touch this window, never miss
  one that did.
- ⚠️ **What equal manifests do and do not prove.** They mean no input this
  structure watches has moved. They are **not** an attestation of byte-identical
  output. **FIVE** report inputs are uncovered, and the count is stated because
  an earlier draft said "two": `period_membership` and `developer_alias` are
  mutated **in place** with no transition log -- and a `MAX(id)`/`COUNT(*)` pair
  cannot see an in-place `UPDATE` at all, so covering them needs ledgers those
  tables do not have. `actual_spend` is not mutated in place: its rows are only
  inserted (`InsertActualSpend`), never updated, and removed only by erasure
  (`EraseDeveloper`); it is uncovered because nothing watches it. `hierarchy_membership` (#886),
  which selects team and division rows, is dated and append-only, so a write
  cannot move a window that ended before it, but it is not watermarked, so a
  window still open at the write can move unseen. An alias edit is dated the
  same way (#914).
  (`developer_alias` is the highest-leverage of them: it re-keys the entire
  cost-to-outcome join.) The fifth is `Open()`-time migrations, which rewrite
  `token_events` with no ledger row and are covered by the `tool_version` stamp
  rather than by a watermark. Read the manifest as "no row-level evidence I watch
  has changed", not as "this report is reproducible".
- **Additive** per the [matrix above](#additive-vs-breaking-at-a-glance): a new
  endpoint is additive, not a compat break.

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
  billing_mode, billed_to, attribution_rule`.
- **CSV** (`Accept: text/csv`): same fields in the **append-only** column order
  of `eventsCSVHeader`. Empty `next_cursor` (body and the `X-Next-Cursor` header)
  means the window is exhausted.
- **CSV formula protection (#1095):** all four bulk exports prefix a single
  quote (`'`) to text cells beginning with `=`, `+`, `-`, `@`, tab or carriage
  return, following OWASP CSV-injection guidance. Numeric columns remain numeric
  (including negative values such as `-1.5`); JSON values are unchanged. Use JSON
  when the original text is required without the CSV safety prefix.
- `attribution_rule` (#823) is an **additive** field and the appended trailing
  CSV column: the rule that assigned the row's `issue_id`. The values are
  `branch`, `worktree-cwd`, `worktree-toolpath` or `carry`. A row with no rule
  recorded (every row stored before #823, and any row whose producer records
  none) exports as `legacy`, and any other stored value exports as `unknown`
  (`exportAttributionRule`). The DSAR export shows the stored value as stored:
  `null` where none was recorded, otherwise the stored text.

#### `GET /api/v1/outcomes`
- **Scope:** read. **Success:** `200 OK`. **403 in any anonymized (team/division) mode** (#185, #270).
  Same pagination/CSV contract as `GET /events`.
- **JSON:** `outcomesExportResponse` -- `{next_cursor, outcomes: [...]}`. Each
  outcome: `id, ts, developer, issue_id, pr_number, weight, weight_source,
  quality, merge_commit_sha, additions, deletions, changed_files, source,
  work_type, work_type_source, repo, push_day`.
- **CSV:** the **append-only** `outcomesCSVHeader` order.
- `push_day` (#242) is the appended trailing column: the UTC calendar day a
  `source='push'` outcome aggregates to (part of the `(repo, issue_id,
  push_day)` dedup key), and
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
- ⚠️ **`source_ref`'s value shape for CI rows changed in #687 (BREAKING)** --
  `head_sha:run_attempt:workflow_id`, was `head_sha:run_attempt`. Same column,
  different bytes; see the [changelog entry](#api-changelog). The identical
  change applies to `source_ref` on `GET /api/v1/quality_history`, which carries
  the CI ref through `UpdateQualityForOutcome`.
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
- 🔴 **`source_ref`'s VALUE SHAPE changed for CI rows (#687) — BREAKING.** The
  column set is unchanged, but ["Change an existing CSV column's value
  shape"](#additive-vs-breaking-at-a-glance) is Breaking on its own, and this is that.
  CI events are now `head_sha:run_attempt:workflow_id`; they were
  `head_sha:run_attempt`. Reverts are still the bare revert commit SHA.
  A consumer that splits and reads `parts[0]`/`parts[1]` is unaffected; one that
  asserts `len(parts) == 2`, or that treats a third component as impossible,
  breaks. See the [changelog entry](#api-changelog) for the full rule, including
  what happens to rows written before #687.

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
- **Scope:** metrics (#944). In `team` and `division` mode the read-only viewer
  token is rejected `403` with a body naming `--metrics-token`; with no metrics
  token configured, only the write token scrapes. Operator-only:
  [security.md § The metrics token](security.md#the-metrics-token-944).
  Mounted at the **root** (`/metrics`, not `/api/v1`), and
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
- `price_table` here is `{version, effective_date}` and is provenance for the
  figures, **not** build identity — its `version` bumps only when prices change,
  so it can agree across a full release gap. It deliberately carries **no**
  content digests on this unauthenticated endpoint; read those from `/scores` or
  a report manifest instead. See
  [content identity](#price-table-content-identity-713).

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
    `subsystems` map. Public watcher fields: `status, last_error (omitempty),
    last_event_ts (omitempty), started_at (omitempty), next_retry_at (omitempty),
    restart_count, watch_add_failures, last_watch_add_error (omitempty)`.
    Nonempty errors are classes: `last_error` is `watch_failed` and
    `last_watch_add_error` is `watch_add_failed`. Full errors remain in logs.
    This route is outside auth, so the same redaction applies even when a
    valid viewer or admin token is supplied.
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

- **#1095 -- S03-3 requires an explicit paid amount (BREAKING request behaviour).**
  `POST /api/v1/actual_spend` and `POST /api/v1/org_actual_spend` now return
  `400` when `actual_paid_usd` is omitted or `null`, instead of returning `201`
  and storing a $0 row. Clients must supply `actual_paid_usd`; an explicit `0`
  remains valid and returns `201`.

- **#1082 -- token-event cost ceiling (BREAKING request behaviour; ADDITIVE schema/metric).**
  `POST /api/v1/costs` imports with `cost_usd > $10,000` or any token field
  `> 1e12` now return `400` instead of `2xx`; split the import across rows.
  An identical keyed replay of a legacy over-ceiling cost remains an idempotent `201`.
  `POST /api/v1/events` remains `2xx`: over-ceiling events are stored with cost
  clamped to $10,000 and marked. New nullable `token_events.cost_clamped` records
  the mark, and `tier_token_event_cost_clamps_total` counts clamp attempts.

- **#1095 -- healthz watcher errors are classes (BREAKING value change).**
  **Explicit exception to rule 3:** raw error text exposed filesystem paths to
  unauthenticated callers. Keeping that text under the old fields while adding
  new fields, or waiting for `/v2`, would preserve the security exposure.
  Therefore `last_error` and `last_watch_add_error` keep their field names for
  compatibility, but their values become error classes in place; no new fields
  are added. This exception is limited to these two fields on `/healthz`.
  In both `watcher` and `subsystems.watcher.detail`, nonempty `last_error`
  is now `watch_failed` and nonempty `last_watch_add_error` is now
  `watch_add_failed`, replacing raw error text. Empty errors remain omitted.
  Full errors remain in logs; consumers parsing error text must use the classes
  instead. This applies to every caller, including authenticated callers.
  `last_event_ts`, `started_at`, and `next_retry_at` retain their existing values
  and omission rules.

- **#1095 -- CSV formula-leading text is neutralised (BREAKING CSV value shape;
  security fix).** The four bulk exports now prefix a single quote to text cells
  starting with `=`, `+`, `-`, `@`, tab or carriage return. Numeric columns, JSON
  output and CSV column order are unchanged.
- **#1042 -- write bodies must end after exactly one JSON value (BREAKING
  request-side behaviour; no field changes).** Every write endpoint that decodes
  a JSON body now refuses anything after its one value except whitespace: a
  second value, trailing garbage, or a stray `]` or `}` is a `400` with the
  existing "request must contain exactly one JSON object" (or "array") message.
  Before, a stray `]` or `}` (`{...}]garbage`, `{...}}`) was accepted and stored
  with a `2xx`. A body whose value fits the size cap but whose trailing bytes do
  not is now a `400`; on `/events`, `/outcomes` and the bulk `/org_hierarchy`
  import it carries the same "request body exceeds N bytes" message as an
  oversized value. *Breaking:* a client that sent such a body goes from `2xx` to
  `400`. Well-formed clients (the shipper, `tierd hierarchy import`, curl with
  one JSON value) are unaffected.

- **#913 -- sealing waits for every configured source (ADDITIVE; schema version
  6).** A new `stall_reason` code, `source_behind`: the first owed month is held
  because a source `tierd serve` pulls from has not settled past its end. Serve
  records each source it starts in `source_watermark` and retires the rest; a
  source's successful pass advances its settled-through time only over coverage
  with no gap. A source certifies only data it recorded: a pass that skipped
  in-scope spend a later pass re-reads settles nothing, and spend it can never
  re-read (a damaged or failing log its cursor moved past, a skipped row, an
  excluded subagent call, a deleted log holding a call) is stored as lost, so
  every month the lost span touches stays held until `tierd seal --skip`. The
  404 for a held month names each source and its settled-through time, the gap
  it cannot fetch, or the span it lost. `sealable_at` and `next_seal_at` keep their
  meaning: a held month reads as awaiting its seal, never as not sealable.
  Until serve has registered its sources on the database, every month is held.
  A month before a source's first recorded coverage never seals automatically;
  its 404 says so and names the remedy (arm a later month, or set
  `active_since` for subscription fees).
  `tierd seal --arm` refuses such a month; `tierd seal --skip` records it as a
  gap with category `source_behind`, once confirmed. The session watcher,
  webhooks, `tierd ship` and `/costs` pushes are covered by `--report-grace`
  only. The database moves to schema version 6, one-way: a version-5 tierd
  refuses it.

- **#913 -- `tierd serve` seals and serves monthly reports (BREAKING in
  `team`/`division` mode).** The sealed reads described below are now served. In
  `team` and `division` mode, `/scores`, `/report_manifest` and `/scores/compare`
  answer only from sealed calendar months. A request that was a live window
  (`since`/`until`/`before`, `?team=`, `?work_type=`, `?repo=`, or the `_a`/`_b` bounds) is now a
  `400`. Until the operator arms sealing (`tierd seal --arm YYYY-MM|earliest`, or
  `seal_from`), every read is a `404` beginning `sealing not armed`. A writable
  serve runs a seal pass at startup and then every hour; each pass seals every
  owed month whose grace lag (`--report-grace`, default 14 days) has ended, oldest
  first. A `--read-only` serve never seals: it serves the months a writable server
  sealed in the same database, and names a month that is overdue. Every
  `team`/`division` `tierd serve` builds its sealer; an anonymised server without one would answer the
  three reads with a `503` whose `error` says it has no sealer, never a live
  body. At startup serve logs `report_grace`, and it WARNs when the current
  aggregation, k or fold rule differs from the config the newest sealed month was
  sealed under, naming the first month the current config applies
  from. `tierd verify-report` still replays a live-window manifest over its window.
  Developer mode is unchanged. **One-way:** a sealed month is never recomputed
  after its seal, and the first seal pins the earliest month for good. The
  database is already at schema version 5, so an older tierd refuses it.

- **#913 -- a sealed read names a stalled month (ADDITIVE to the sealed reads
  below).** While sealing is stalled, the sealed manifest and every
  sealed `404` (including `/scores/compare`'s) carry `stalled_period`
  (`YYYY-MM`), `stall_reason` and `owed_since` (the month's `sealable_at`), and
  the manifest's `next_seal_at` and the `Tier-Next-Seal-At` header are absent.
  Sealing is stalled at the first month owed after the newest sealed or gapped
  month when the last seal pass failed there, or, while no seal pass is
  running, when it is more than two seal passes past its `sealable_at`, so a
  restarted or read-only server still names it. `stall_reason` is a failure category code, never an error's text. The
  codes are `sealFailures`, `sealFailureStorage`, `sealFailureInternal` and
  `sealUnreported` in `internal/api/seal.go`: `write_lock_busy`, `erase_raced`,
  `clock_behind`, `refold_mismatch`, `floor_moved`, `not_next`, `overlap`,
  `gapped`, `not_sealable`, `cancelled`, `storage`, `internal`, and
  `unreported`, which means no seal pass has reported on the month while none
  is running, so check that `tierd serve` is running and not read-only. A code
  is never reassigned, and a later release may add codes, so treat an unknown
  code as an unnamed failure. All three fields are absent while nothing is
  stalled, and none appears on `/healthz`, `/livez` or `/health`.
  `tierd doctor --server` reads them from `/report_manifest` and reports a
  stalled month as a WARN, with the `tierd seal --skip` command only for a
  `stalled_period` that is exactly `YYYY-MM`. `tierd seal --status --aggregation team --db ~/.tier/tier.db --server http://127.0.0.1:8080` prints the
  sealing state from the database, opened read-only, and asks `--server` for
  the reason; use the same flags, config file and environment as
  `serve`. `tierd seal --help` lists its exit codes.

- **#913 -- permanent seal gaps (ADDITIVE; schema version 5).** No endpoint
  changes yet. The database gains `sealed_gap`, an append-only record of a month
  the operator marks as never to be sealed, so the months after it seal. The
  database moves to schema version 5, one-way: a version-4 tierd refuses it.

- **#913 -- sealed monthly reads (BREAKING in `team`/`division` mode; served
  from the `tierd serve` entry above).**
  In `team`/`division` mode `/scores` and `/report_manifest` answer one
  sealed calendar month: `?period=YYYY-MM`, or none for the latest sealed month
  (a grace raised later never hides a month already sealed). `/scores` returns the stored
  body **byte for byte**, so its sha256 equals the manifest's `body_digest`; the
  month and its markers ride in response headers, not a wrapper: `Tier-Period`,
  `Tier-Sealed-At`, `Tier-Next-Seal-At`, `Tier-Earliest-Period` and
  `Tier-Latest-Period` (the last three equal the manifest's `next_seal_at`,
  `earliest_period` and `latest_period`). The manifest (`manifest_schema:
  tiersealedmanifest1`) carries `period`, `period_start`, `period_end`,
  `sealed_at`, `next_seal_at`, `earliest_period`, `latest_period`, `config`
  (`aggregation`, `period_size`, `k`, `fold_rule`, `digest`), `body_digest`,
  `tool_version`, `commit`, and `config_gap` when the current config differs.
  **A GET never writes:** once the operator arms sealing, a background pass
  seals each month after its grace lag, permanently, oldest first, and a month
  it fails to seal holds back every later one. A sealed body always carries `data_quality.attribution_coverage:
  "not_shown"` and never `cost_coverage_safe_since`. A sealable month with no
  data is a `200` with the same top-level and `data_quality` keys as a
  k-suppressed month, except `kanon_suppressed` and `attributed_outcome_share`,
  which an empty month never carries, and the conditional quality signals (such
  as `unjoined_developers` or `zero_token_outcome_count`), which appear only when
  their condition holds, so a k-suppressed month can carry ones an empty month
  lacks. A month not sealed is a `404`
  with `period`, `aggregation` and `error`: while sealing is not armed, `error`
  begins `sealing not armed`; for a month sealable but not yet sealed it begins
  `month is sealable but not yet sealed` and, while the background pass is failing
  at that month or an earlier one, names that month and why; otherwise it says
  why the month is not sealable, and `sealable_at` is present when the month
  will become sealable. With nothing sealed yet, the default read's `404` names
  the latest sealable month, or the earliest when that is later.
  `since`/`until`/`before`, `team`, `work_type` and
  `repo` are `400`s, and any other parameter is an unknown-parameter `400`
  listing `period` alone. Sealing is armed by the operator setting `seal_from`, a
  month; until then nothing is sealed. The earliest sealable month is the later
  of `seal_from` and the first full month after the cost horizon (a month
  starting exactly at the horizon counts as full), pinned by the first seal:
  older data imported after the first seal never appears in anonymised modes. `/scores/compare` compares two sealed months:
  `?period_a=YYYY-MM&period_b=YYYY-MM` with `period_a` earlier, or neither for
  the two latest sealed months: the month the default `/scores` read serves and
  the month before it or, while that month is inside a raised grace lag, the
  latest sealed month before it.
  The rows are refolded at the months' sealed k from their stored fold inputs,
  never from a live window or from the two stored bodies, so a team is named only
  if it clears k in both months, and each month's own residual must reach k or
  the whole comparison is withheld. The body keeps the live shape; `mode` is the
  level both months were sealed under, each window's `since`/`until` are the
  month's bounds and its `data_quality` carries the live compare window's fields
  from the stored body, except `cost_coverage_safe_since`, which a sealed body
  omits; the two sides of `total` are the stored bodies' `total`s (a zero side
  for a month with no rows), so they equal `/scores?period=`; and
  `price_table` is the active table, as on the live read. The months ride in
  headers: `Tier-Period-A`, `Tier-Period-B`, `Tier-Sealed-At-A`,
  `Tier-Sealed-At-B`, plus `Tier-Next-Seal-At`, `Tier-Earliest-Period` and
  `Tier-Latest-Period`. A reversed or equal pair, one period alone, and any
  `since`/`until`/`before`/`_a`/`_b` bound are `400`s naming `?period_a=` and
  `?period_b=`. A month not sealed is a `404` naming it, with the `error`
  and `sealable_at` of `/scores?period=` for that month; with neither period
  given, fewer than two sealed months is such a `404`. Two months sealed under different configs,
  or under a fold rule this binary does not refold, are a `409` carrying
  `period_a`, `period_b`, `config_a` and `config_b`. Developer mode is unchanged.

- **#823 -- the `/events` export names the rule that assigned each row's issue
  (ADDITIVE; one export column).** `GET /api/v1/events` appends
  `attribution_rule` as the last JSON field and the last CSV column: `branch`,
  `worktree-cwd`, `worktree-toolpath` or `carry`; `legacy` where no rule was
  recorded (every row stored before #823); `unknown` for a stored value outside
  that set. The DSAR export (`GET /developer/{id}/export`) shows the stored
  value as stored: `null` where none was recorded. No field is retired; see
  [`GET /api/v1/events`](#get-apiv1events).

- **#975 -- erasure follows retired canonical ids (ADDITIVE; schema version 4).**
  `DELETE /api/v1/developer/{id}` adds `deleted.canonical_id_history` (always
  present), and `deleted.sealed_person` now also counts keys computed from a
  canonical id an alias edit retired, in periods sealed while the link was
  active. The database moves to schema version 4,
  one-way: a version-3 tierd refuses it.

- **#823 -- `POST /events` accepts `attribution_rule` and the
  `unattributed:foreign-repo` bucket (ADDITIVE on the server).** Both are new
  accepted inputs; every pre-#823 request is accepted unchanged. The new bucket
  can appear in `data_quality.unattributed_buckets`, and is not exploratory spend.
  An older tierd `400`s either, so the server ships first.

- **#913 -- erasure tombstones sealed person keys (ADDITIVE; schema version 3).**
  `DELETE /api/v1/developer/{id}` adds `deleted.sealed_person` (always present:
  sealed person keys tombstoned, never deleted), and `total_deleted` includes it,
  so an erasure whose only effect is tombstoning returns `200` rather than `404`.
  The database moves to schema version 3, one-way: a version-2 tierd refuses it.

- **#864 -- anonymized modes publish one breakdown per install and window
  (BREAKING behaviour in anonymized modes; three additive fields).** Before, a
  `team`/`division` response published the group rows beside a second and third
  breakdown of the same window -- the work-type segments and the cost composition
  -- and they differenced to a group below *k* even when every published row had
  *k* people: pooled team rows minus work-type totals recovered a 3-person group's
  $9.21, and a model one developer used published that developer's $4.44 in
  `cost_composition.by_model`.
  *Now,* in `team` and `division` mode only:
  `?work_type=` on `/scores` is a `400`; `work_types` (with its per-segment `teams`,
  `aggregation` and `kanon_suppressed`), `segment_reconciliation`,
  `cost_composition`, `data_quality.unattributed_buckets`,
  `data_quality.exploratory_cost_share` and `data_quality.mixed_price_versions`
  (on `/scores` and both `/compare` windows; `price_table.version` then names only
  the active table, and a floored warning is to return through a follow-up issue)
  are absent, and so is `data_quality.attributed_cost_share`; spend under the `unattributed`
  pseudo-developer is dropped from every figure, which narrows #856's "an uncounted
  identifier's figures stay summed" for pseudo-developers; and when the `other`
  bucket does not reach *k* under #856's counting rule and per-measure floor, the
  WHOLE response is withheld -- `teams` (named rows included) and `total` -- and
  declared in `kanon_suppressed`, whose `withheld_cost_composition` and
  `withheld_segment_reconciliation` are now `false`. `/scores/compare` withholds its
  whole comparison when its own residual or either window's own `other` bucket does
  not reach *k*.
  *Fields (additive):* `kanon_suppressed.withheld_teams` (always present in the
  object): `true` when the rows were withheld with the residual;
  `data_quality.excludes_unattributed_spend` and `data_quality.attribution_coverage`,
  both described under [`GET /api/v1/scores`](#get-apiv1scores).
  *Breaking:* anonymized responses lose the keys above, and a response that used to
  carry named rows beside a withheld residual now carries none. No key leaves the
  schema developer mode reads, and developer mode is unchanged. No
  `Deprecation`/`Sunset` header is sent: the keys are withdrawn to stop a
  disclosure, which a sunset period would prolong. #937 (an audit that solves
  every published sum) is the route to bring a work-type or per-model view back to
  anonymized modes. Ships in the first tagged release after this merge.

- **#944 -- the read token no longer scrapes `/metrics` in anonymized modes
  (BREAKING auth behaviour; one new credential; no schema bump).** Two scrapes of
  `/metrics` a short interval apart difference into one person's spend and
  activity, below the *k* floor. *Now:* in `team` and `division` mode `GET
  /metrics` answers the read-only viewer token with `403` (body names
  `--metrics-token`) and accepts the new metrics token (`--metrics-token`,
  `TIER_METRICS_TOKEN`, `@/path/to/file`, or `metrics_token` in YAML) or the
  write token; with no metrics token configured, only the write token scrapes.
  The metrics token is refused `403` on every other route and on the proxies,
  and `serve` refuses to start when any two of the write, read and metrics tokens
  are equal. `developer` mode is unchanged, and with no token configured
  `/metrics` stays open in every mode. *Breaking:* a Prometheus job scraping an
  anonymized install with the read token gets `403`; point it at the metrics
  token ([security.md § The metrics token](security.md#the-metrics-token-944)).
  The flag, env var and config key exist only from this release; an older binary
  refuses `--metrics-token` and a config with `metrics_token` at startup, so
  upgrade tierd with the metrics token set, then repoint Prometheus.
  #908 (splitting the token scopes) may later subsume the metrics token.

- **#854 -- `POST /costs` refuses rows a running org poller already counts
  (BREAKING behaviour; one additive request field; one additive export column;
  no schema version bump).** Before, a manual row for the provider of a running
  Anthropic Admin or OpenAI Usage poller was accepted `201`, and when it was the
  polled org's usage it was counted twice: the poller brings the org's recorded
  usage up to the provider's total and never subtracts `/costs` rows. *Now:* such a row is refused `400`
  unless it declares `"billed_to": "other"`; see
  [`POST /api/v1/costs`](#post-apiv1costs). The `201`-to-`400` change happens
  only on a server that runs that provider's poller; every other install behaves
  as before. A keyed retry of a row stored before the upgrade, sent without
  `billed_to`, now gets `400` on such a server.
  *Field:* `billed_to` (optional request field, additive; only `"other"`).
  ⚠️ **Version skew: upgrade the server first.** An older server rejects the
  unknown field with `400` (`DisallowUnknownFields`), so a client that sends
  `billed_to` to it fails on every row.
  An `override: true` request without `billed_to` still corrects a row that
  already owns its key, which is how a stored double count is fixed; one whose
  key owns no row is refused like any other. At startup `tierd serve` warns, per
  running poller, about undeclared manual rows on days that poller covered. How to
  list rows stored before the upgrade, by API or SQL, is under
  [`POST /api/v1/costs`](#post-apiv1costs).
  *Export:* `billed_to` is appended to the `GET /events` JSON and CSV (empty when
  not declared) and added to `token_events` rows in `GET /developer/{id}/export`
  (null when not declared). No field is retired.
  *Schema:* the schema version is unchanged, because `billed_to` is an added
  column, so a binary older than #854 can still open the database. Running one
  against it admits undeclared manual rows again (it has no refusal), and the
  #854 startup warning will count them.

- **#938 -- a push outcome's owner is ordered by GitHub's push time (behaviour
  fix; one additive export column; no schema version bump).** A push outcome was
  re-owned to its earliest commit by commit time, which the committer sets, so a
  later push of a commit dated earlier the same UTC day took another developer's
  row. *Now:* the order is `repository.pushed_at` from the push payload, then
  commit time, then SHA. Entries stored before the upgrade and pre-ledger markers
  sort first, so no stored outcome changes owner. A push with no usable
  `pushed_at` is recorded but sorts after every push time and, among such
  pushes, by arrival. This holds only while no binary older than #938 writes to
  the database: the schema version did not change, so such a binary can still
  open a database a #938 binary has opened, and every entry it writes gets
  `push_order` 0 and keeps precedence permanently. Do not run a pre-#938 binary
  against that database.
  *Export:* each `push_outcome_commits` row in `GET /developer/{id}/export` gains
  `push_order` (integer, additive): the commit's earliest push time in Unix seconds,
  `9223372036854775807` for a push without one, and `0` for a pre-ledger marker
  or an entry stored before #938. Erasure is unchanged.
  *Metrics:* `tier_push_missing_pushed_at_total` counts commits recorded without
  a usable `pushed_at`. No field is retired.

- **#856 -- the k-anonymity floor counts people, not identifiers (BREAKING
  behaviour in anonymized modes; one additive response field; one additive export
  key).** Before, any identifier with spend or merged work filled a seat, so
  hand-entered `/costs` rows, push-only commit authors, identifiers nobody put on
  the roster, and bots could lift a group to *k*, and a five-person row whose cost
  was carried by one person published that person's exact cost.
  *Now:* the rule under
  [`data_quality.uncounted_active_ids`](#get-apiv1scores) applies to `teams`,
  every `work_types[].teams`, and each window of `/scores/compare`, with the
  per-measure floor on top.
  *Field:* `data_quality.uncounted_active_ids` (additive, `omitempty`, `/scores`
  only). `kanon_suppressed` gains no field; `developers` now counts people under
  the new rule.
  *Export:* `GET /developer/{id}/export` `outcomes` rows gain `author_type`
  (additive).
  *Breaking:* in anonymized modes a group or residual that was published may now
  be withheld, with `total`, `cost_composition` and `segment_reconciliation`
  (on `/scores/compare`, `total`) withheld alongside it as in #593. No field is
  retired, so no `Deprecation`/`Sunset` header is sent. Ships in the first tagged
  release after this merge.
- **#914 -- alias edits are dated (BREAKING behaviour; no schema bump).** Team and
  division rows now place each event by the dated membership of the identifier it
  was recorded under, not its canonical developer's. `POST` and `DELETE
  /developer_alias` and every `org_hierarchy` write append membership rows, dated
  by the server clock, for each identifier whose placement they change, so no
  alias edit moves an identifier's past between teams. Events under an alias from
  before it was added now stay in the team the identifier was in then (usually
  `other`). Both alias routes can now answer `409` (clock behind) and `DELETE`
  can answer `503`. The upgrade gives each existing alias its person's dated
  history, so no past window changes. Both guarantees hold only while no binary
  older than #914 writes to the database: because the schema version did not
  change, a binary built from `main` between #912 and #914 can still open a
  database a #914 binary has opened, and its alias edits append no membership
  rows, which the one-time upgrade does not repair, so events under those
  aliases then place as unassigned. Do not run such a binary against that
  database; no released version is affected, since v0.5.0 and v0.5.1 were never
  tagged. Until #913's sealed periods ship, an alias
  merge or unmerge can still change whether a past team clears the *k* floor;
  #913 closes it.
- **#849 -- a squash merge counts once whatever order its events arrive in
  (behaviour fix; additive export and manifest keys; no schema version bump).**
  With push capture on, a squash merge whose `push` arrived before its
  `pull_request` was stored twice: a push outcome and the PR outcome.
  *Now:* every captured push commit is recorded in a per-commit ledger, and the
  PR outcome — from the webhook, `tierd backfill` or `POST /api/v1/outcomes` — removes
  its merge commit's contribution in the same transaction. A
  push outcome that held only that commit is deleted; one that holds other
  commits stays, and its `developer` and `ts` are re-derived from its earliest
  remaining commit (commit time, then SHA) — so a push outcome's owner can change
  after it was first written. Push outcomes written before the upgrade are never
  deleted. The push outcome keeps its `(repo, issue, UTC day)` grain, `0.5` weight
  and every export column.
  *Export:* `GET /developer/{id}/export` gains `push_outcome_commits` and
  `push_outcome_audit` arrays (additive); erasure clears the subject's rows in
  both and re-owns a push outcome that also holds another developer's commit.
  *Manifest:* `watermarks.ledgers` gains `max_push_outcome_audit_id` and
  `push_outcome_audit_count` (additive, `tiermanifest1` unchanged);
  `tierd verify-report` reports them as `push reconciliations`.
  *Metrics:* `tier_push_merge_commit_captures_total` counts pushes that carry a
  `Merge pull request #N` commit beside captured commits (the #934 leak).
  No field is retired. Ships in the first tagged release after this merge.

- **#886 -- team and division rows are dated (BREAKING behaviour; one additive
  export key; schema version 2).** Before, every window was grouped by today's
  team map, so reading `/scores`, moving one developer and reading again showed
  that developer's figures in the difference of two team rows.
  *Now:* `teams` (team and division modes, including `work_types[].teams`),
  `/scores/compare`, `team_rollups` and `?team=` group each event by the
  membership its developer held at the event's own time, from an append-only
  ledger dated by the server clock. `team_rollups` rows are per event, not per
  person: a developer who moved inside the window appears in two rows. Alias
  edits are dated too (#914, above). A developer
  whose only figure in a group is paid spend no longer counts toward its *k*
  floor.
  *Requests:* `PUT`/`POST /org_hierarchy` answer `409` when the server clock
  reads earlier than the developer's current membership began; `valid_from` and
  `valid_to` are refused `400` like any unknown field.
  *Export:* `GET /developer/{id}/export` gains a `hierarchy_membership` array
  (additive), without the stored `written_by` fingerprint.
  *Breaking:* past team rows change once on upgrade only for people first
  assigned after it (their earlier history stays in `other`), and a group
  padded by paid-only seats may now be withheld. The database moves to schema
  version 2, which an older tierd refuses to open; binaries older than #141 do
  not check the schema version, so the bump cannot stop them — back up before
  upgrading and never run them against an upgraded DB.
  No field is retired, so no `Deprecation`/`Sunset` header is sent. Ships in
  the first tagged release after this merge.

- **#871 -- plain `POST /costs` refuses another identity's key (BREAKING
  status; no field changes).** A keyed post whose `cost_usd` matched the stored
  row's was accepted with `201` whatever its identity, and MAX-merged its token
  counts into that row -- so a post reusing another developer's key raised
  their captured row's counts and wrote nothing for the caller. It now returns
  the override path's identity-mismatch `409` when the stored `(developer,
  issue_id, model, source, fidelity)` differs, and a same-identity re-post no
  longer changes the stored token counts on either path. *Breaking:* a client
  re-posting such a key goes from `201` to `409`; its `201` never recorded its
  spend. The owner of a row can now get that `409` on a same-cost retry too:
  when the stored fidelity is a **pre-#82** value outside `daily`/`estimated`
  (pre-#82 `/costs` accepted any fidelity string), or when the first post sent
  `fidelity: "daily"` and the retry omits it (omitted defaults to
  `estimated`). Nothing is written either way; the remedy is to resend with
  the stored fidelity, which for a pre-#82 value is not possible (see the
  pre-#82 note under `POST /costs` above).
- **#919 -- erasure and export cover watcher checkpoints (ADDITIVE).**
  `DELETE /api/v1/developer/{id}` adds `deleted.watcher_checkpoint` (rows
  tombstoned plus rows deleted), and `total_deleted` now includes it;
  `GET /api/v1/developer/{id}/export` adds a `watcher_checkpoint` array.
- **#893 -- browser-shaped requests refused (BREAKING request-side behaviour; no
  field changes).** Three refusals close cross-site and DNS-rebinding writes to a
  tierd the operator's own browser can reach.
  *Content-Type:* every `POST`, `PUT` and `PATCH` under `/api/v1` answers `415`
  unless its media type is `application/json`, in token mode too. `curl -d` without
  `-H 'Content-Type: application/json'` sends a form type and now gets `415`.
  *Cross-origin:* a request whose `Sec-Fetch-Site` is not `same-origin` or `none`
  (or, without that header, whose `Origin` host differs from `Host`) gets `403` on
  `/api/` for every method, including a clicked link, and on every other path for
  any method but `GET`, `HEAD` and `OPTIONS`, so the provider proxies too (#903).
  *Host:* a tokenless tierd on loopback answers `403` to any request whose `Host`
  is not a loopback name or IP, except on `/webhook/github`.
  *Breaking:* a client that wrote with another `Content-Type` goes from `2xx` to
  `415`. The shipper, `tierd hierarchy import`, curl and SDK clients send no
  `Origin` or `Sec-Fetch-Site` and address the server by the name they dialled, so
  the two `403`s do not reach them. Ships in the first tagged release after this merge.

- **#853 -- the `unattributed` pseudo-developer no longer counts toward the k-floor
  (BREAKING behaviour in anonymized modes; no new field).** A residual cohort of
  k-1 real developers plus the `unattributed` pseudo-developer (the org billing
  pollers' remainder) counted as k and was **published**, so a sub-k group of real
  people cleared the floor on the strength of spend no person is behind.
  *Fix:* only real people count toward the floor. That residual is now **withheld**,
  and withholding it takes `total`, `cost_composition` and `segment_reconciliation`
  with it (on `/scores/compare`, `total` alone -- it ships no sidecars), exactly as
  any #593 suppression does. (A residual of the pseudo-developer
  alone was already withheld; it now reports `developers: 0` rather than `1`.)
  *Field:* `kanon_suppressed.developers` now counts **real people only** and **can
  be `0`** -- the case where the only group withheld is the pseudo-developer's spend.
  The field keeps its name: it was always documented as a count of developers
  withheld, and the pseudo-developer is not one, so counting it was the defect, not
  the meaning.
  *Applies to* `/scores` (`data_quality.kanon_suppressed`), each `work_types[]`
  segment (`work_types[].kanon_suppressed`), and `/scores/compare` (top-level
  `kanon_suppressed`). Developer mode is unaffected.
  *Breaking:* in anonymized modes a window that previously returned `total` and its
  sidecars may now return them absent with `kanon_suppressed` set, and a consumer
  that assumed `developers >= 1` must handle `0`. No field is retired, so no
  `Deprecation`/`Sunset` header is sent. Ships in the first tagged release after
  this merge.

- **#821 -- `scores.team_rollups` (additive; developer mode only).** A top-level
  array that partitions `total` by `org_hierarchy` team, plus one `unassigned: true`
  row for developers with no team, so the rows' `weighted_points` and
  `total_cost_usd` sum to `total`'s. It answers "where did the org's points and
  spend go, by team" without a `?team=` request per team.
  *Classification:* [additive](#additive-vs-breaking-at-a-glance) -- a new
  `omitempty` response field; no existing field changes meaning.
  *Why not `teams`:* `teams` is frozen as the **k-anonymized** array of the
  anonymized modes, labelled by `aggregation`. Filling it in developer mode with
  unfloored rows would repurpose it in place, which rule 3 forbids, and would make
  `aggregation` absent beside a populated `teams` for the first time.
  *Shape rules that are easy to get wrong:* the mode rule alone decides presence --
  developer mode, beside `total`; **absent** in `team` and `division` mode (an
  unfloored per-team rollup is the #593 differencing channel), and so under any
  k-anonymity suppression, which only those modes raise; rows in byte-wise
  ascending team-name order; not narrowed by `?team=` or `?work_type=`; not on
  `work_types` segments. *Scope:* the read-only viewer token now sees every team
  name and its totals in developer mode (ruling #821 = B); `GET /org_hierarchy`
  stays write-scoped. See the
  [endpoint section](#get-apiv1scores).

- **#824 -- `exploratory_cost_share` description corrected (documentation
  only).** The value is unchanged, and so is the name, which is historical. The
  field description in this document, and the docs changed with it, no longer
  call this spend exploratory or planning overhead: it is spend recorded on
  `main` or `master`, which TIER could not link to an issue. Other places may
  still use the old wording until they are updated. Both
  `data_quality.exploratory_cost_share` and the per-developer
  `exploratory_cost_share` are affected. See the [`GET /scores` field
  list](#get-apiv1scores).

- **#751 -- `report_manifest.repo_scope_excluded` (additive).** A scoped manifest
  now pins the repo-blind rows a strict `?repo=` dropped -- `token_events`,
  `cost_micro`, `outcomes` -- which is the pinned form of what `/scores`
  discloses under the same key in `data_quality`.
  *Why:* that figure was the one published number a scoped manifest's own
  identities structurally could not cover. A scoped `/scores` reads the
  `unqualified` sentinel rows to publish it, while the #747 scoped digests
  exclude those rows by design (widening them would reintroduce the #590
  over-attribution) and a scoped manifest carries no fleet-wide digest. Measured:
  an in-place reprice of a repo-blind row moved `repo_scope_excluded.cost_usd`
  from `9` to `14` while the scoped digests and watermarks were byte-identical,
  and `tierd verify-report` printed `REPRODUCED` over changed served bytes.
  *Classification:* [additive](#additive-vs-breaking-at-a-glance) -- a new
  `omitempty` response field. **No `manifest_schema` bump**: no existing field
  changed meaning, and an old consumer ignoring the key still reads every other
  field exactly as before. A **scoped** manifest without it verifies as
  `NOT PINNED`, never an error; a **fleet-wide** one prints no line at all,
  because a fleet-wide report excluded nothing and there is no pin to miss.
  *Shape rules that are easy to get wrong:* present on **every** scoped manifest
  including an all-zero one (an omit-when-clean pin is vacuous exactly when it
  matters), absent on every fleet-wide one, and denominated in **micro-dollars**
  rather than the dollars `/scores` publishes. See the
  [endpoint section](#get-apiv1report_manifest).
  ⚠️ *This endpoint's earlier additions (#715, #740, #747) have no changelog
  entry of their own; they are recorded in the endpoint section above. Do not
  read their absence here as "nothing changed".*

- **#746 -- the DEFAULT `?since=` window now opens at the start of its UTC day
  (BEHAVIOURAL, not opt-in; a request-parameter DEFAULT moved, no response field
  was renamed, removed or repurposed).** When `?since=` is omitted the lower
  bound was `now - 90d` *carrying the current time of day*; it is now that
  instant snapped BACKWARD to `00:00:00Z`. One definition (`api.defaultSince`,
  called from `parseSince`'s empty branch) feeds every windowed read: **six call
  sites, NINE endpoints**, because `parseExportParams` is a single call site
  serving four routes.
  [`/scores`](#get-apiv1scores), `/scores/{developer}`,
  [`/scores/compare`](#get-apiv1scorescompare),
  [`/report_manifest`](#get-apiv1report_manifest), `/org_actual_spend`, and the
  bulk exports [`/events`](#get-apiv1events),
  [`/outcomes`](#get-apiv1outcomes),
  [`/quality_events`](#get-apiv1quality_events) and
  [`/quality_history`](#get-apiv1quality_history).
  **Explicit bounds are untouched** -- `YYYY-MM-DD` / `YYYY-MM` / `YYYY` already
  parse to midnight UTC.
  *What moves:* a default window is up to 24h WIDER, so it can include events
  timestamped between the UTC midnight that opens the window and the `now - 90d`
  instant the old bound started at. On a populated install that is a real change
  in the numbers. Measured in-process on the fixture in
  `cmd/tierd/verifyreport_defaultwindow_test.go` -- three $4 events (and their
  outcomes) at the window-opening midnight, alongside three $3 events deep inside
  the window: `total.total_cost_usd` 9 -> 21, plus a developer row the old bound
  excluded. **Both figures are asserted by that test**, so this entry cannot
  drift away from the fixture it cites.
  *Classification, stated plainly because the obvious label is wrong:* this is
  NOT [rule 3](#the-compatibility-rule). Rule 3 governs a RESPONSE FIELD whose
  number comes to mean something different, and its remedy is a new field name or
  a `/v2`. No response field changed meaning here -- `since` was always specified
  as the "echoed window lower bound (UTC calendar day)", and the snap is what
  makes that description literally true. What changed is the DEFAULT VALUE of a
  request parameter, which the rules did not previously cover; the table above
  now has a row for it, and this entry is its precedent. The obligation that does
  apply is announcement, which is what this entry is.
  *Direction is not arbitrary:* widening can only ever add spend that really
  occurred, where snapping forward would silently drop a partial day of cost out
  of a **cost** metric.
  *Also worth knowing:* an install whose FIRST captured event falls in that
  widened day will newly report `window_predates_cost_capture` on the default
  view -- honest (the window really does now predate capture), but new. And
  `checkWindowRetention` compares `since` against the retention horizon, so once
  #252 pruning is armed the no-parameters request becomes the shape most likely
  to trip its `422`; it is inert today (`SetRetentionHorizon` has no non-test
  caller).
  *One new sharp edge:* the two-request workflow that builds a verifiable report
  (`GET /report_manifest` then `GET /scores`, merged) resolves the default
  SEPARATELY in each request. Before the snap the two bounds differed by the
  milliseconds between the calls -- always slightly, harmlessly. They are now
  byte-identical all day, **but differ by a full 24h if a UTC midnight falls
  between the two calls.** Take both requests on the same UTC day, or pass
  explicit bounds.
  *Why it was worth a population change:* `/scores` echoes `since` as a bare
  calendar day while `/report_manifest` publishes the honest instant, so a
  default report claimed `"2026-05-31"` for a window that opened at
  `2026-05-31T05:00:58Z` -- and `tierd verify-report` refused that manifest with
  **exit 2**, because `/scores` has no query that reproduces a mid-day bound. The
  DEFAULT report shape was the one shape that could not be verified. The
  verifier's refusal is deliberately UNCHANGED and still fires for hand-written
  and pre-#746 manifests.
  *Not affected:* `tierd score` / `ship` / `backfill` keep their own unsnapped
  `parseSince` -- those feed JSONL scans, not a windowed store read, where a
  wider scan is merely redundant work.
- **#687 -- `source_ref`'s value shape for CI rows (BREAKING on
  `GET /api/v1/quality_events` and `GET /api/v1/quality_history`).** CI quality
  events now encode `head_sha:run_attempt:workflow_id`; they encoded
  `head_sha:run_attempt`. No column was added, renamed or reordered, so the JSON
  and CSV *shapes* are untouched -- but the VALUE shape of an existing column
  changed, which the table above classifies as Breaking on its own.
  Rationale: flaky-rerun matching carried no workflow identity at all, so any
  successful workflow run on a merge commit within 30 minutes of any failing one
  neutralised the failure and a `0.7` outcome silently read `1.0`. In a repo with
  CI + lint + docs + CodeQL on the default branch those finish seconds apart, and
  anyone with push rights could make it deterministic by adding one
  trivially-green workflow -- no webhook secret required.
  *Breaking:* a consumer re-deriving the multiplier must now pair a
  `ci_fail_flaky` with a `ci_fail` on the **same head SHA AND the same
  `workflow_id`**, not the same head SHA alone. A consumer that reads
  `parts[0]`/`parts[1]` positionally is unaffected; one that asserts exactly two
  components, or treats a third as impossible, breaks.
  *Rows written before #687* keep the two-component form and are treated as
  carrying NO workflow identity: they pair only with each other, never with an
  identified row. That is what makes a pre-#687 outcome re-derive to the value it
  always had rather than being silently re-judged. Reverts are unchanged (the
  bare revert commit SHA).

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
  every session message it could not link to an issue. Nothing legitimately ships it as a `developer`: the two
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
  case-sensitive), so one dollar was reported simultaneously as unattributed spend by
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
  `0` on a zero-point row at #239; `null` since #472) and its self-relative bootstrap CI
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
  untouched; pinned consumers unbroken. (No cross-org comparison is planned.)
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
  losing its own correction. The correction path is the sanctioned audited
  override below (ruling C); a NEW `idempotency_key` is a second row that every
  spend read adds to the first (#860).
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
- **#713 -- `price_table.table_hash` + `price_table.file_hash` (additive).**
  Both are always present on `/scores` and `/scores/compare` (and in `tierd
  score-log`'s JSON). They are **deliberately absent from `/api/v1/version`**,
  which is unauthenticated -- see
  [content identity](#price-table-content-identity-713). A client that decodes
  `price_table` into a closed struct is unaffected (both are new keys); one that
  rejects unknown keys must be updated.
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
  `ranked` added to per-developer rows; the array is not pre-sorted (at #133;
  the array has since been sorted by developer identifier -- see the `/scores`
  catalog).
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
