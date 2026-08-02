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

Do **not** rely on the presence or absence of an individual response field for
feature detection at runtime: additive fields (below) can appear at any release,
and `omitempty` fields are absent whenever their value is empty even on a server
that fully supports them.

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
- **Scope:** write. **Success:** `201 Created`, empty body.
- Manual single-row cost import. Request: `costRequest` -- `developer`,
  `issue_id`, `model` (required); `input_tokens`, `output_tokens`,
  `cache_read_tokens`, `cache_write_5m_tokens`, `cache_write_1h_tokens`,
  `cost_usd`, `source`, `fidelity`, `idempotency_key` (optional).
- `cache_write_tokens` is the **deprecated** legacy single-bucket field
  (superseded by the 5m/1h split, #55); when present and non-zero it is routed
  to the 5m bucket and the response carries the legacy `Warning: 299` header.
- `source` accepts only `"api"` or omitted; `fidelity` accepts `"daily"`,
  `"estimated"`, or omitted (`"realtime"` is rejected -- reserved for automated
  capture).
- **Keyed re-post (`idempotency_key` present):** an **identical** re-post (same
  key, same cost) is idempotent -- `201`, no new row. A re-post with the same key
  but a **different** `cost_usd` is rejected with **`409 Conflict`** (#295): the
  stored cost is immutable (#233), so the correction is refused rather than
  applied. Divergence is judged on the stored **integer** `cost_micro`, so an
  honest retry whose float/FX/rounding jitter rounds to the same micro value is
  **not** a conflict. To change a recorded figure, re-post under a **new**
  `idempotency_key` (deleting a single row is operator-level, direct against the
  store — there is no delete-by-key route).
- Dedup is keyed on `idempotency_key` alone, and **only** a `cost_micro`
  divergence raises `409`. A re-post that reuses a key but changes a *non-cost*
  field (`developer`, `issue_id`, `model`, token counts) is still a silent no-op
  on those columns (they are immutable on conflict, #233) -- it is NOT a `409`.
  Reuse a key only for a genuine retry of the same event.

#### `POST /api/v1/events`
- **Scope:** write. **Success:** `201 Created`, body `{"accepted": <int>}`.
- Bulk repo-aware ingest (array body). `accepted` counts events processed, not
  rows newly created (the MAX-on-conflict UPSERT absorbs replays). Per-event
  fields mirror `costRequest` plus `repo`, `session_id`, and a **required**
  RFC3339 `timestamp`. Since #233 the server reprices token counts with its own
  price table; the client `cost_usd` is retained only as a cross-check.

#### `POST /api/v1/outcomes`
- **Scope:** write. **Success:** `201 Created` on insert, `200 OK` on duplicate.
- Response `outcomeResponse`: `status` (`"created"` or `"duplicate"`; always
  present) plus `weight_source`, `weight`, `work_type` (`omitempty`). Dedup is on
  `merge_commit_sha`; a replay returns `200` with `status: "duplicate"` and
  echoes the stored row's weight/source/work_type.

#### `POST /api/v1/actual_spend`
- **Scope:** write. **Success:** `201 Created`, empty body.
- Finance per-developer invoice total. Request: `developer`, `period` (YYYY-MM),
  `actual_paid_usd`. Rows accumulate; negatives are accepted as credit memos.

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

#### `DELETE /api/v1/developer_alias/{alias}`
- **Scope:** write. **Success:** `204 No Content`; `404` if not mapped.

#### `GET /api/v1/developer_alias`
- **Scope:** write (admin; discloses the identity map).
- **Success:** `200 OK`, `{"aliases": {<alias>: <canonical>, ...}}`.

#### `PUT /api/v1/org_hierarchy/{developer}`
- **Scope:** write. **Success:** `200 OK`, the stored `HierarchyRow`:
  `{developer, team, division, org}`. Request body: `{team, division, org}`.

#### `POST /api/v1/org_hierarchy`
- **Scope:** write. **Success:** `201 Created`, `{"accepted": <int>}`.
- All-or-nothing bulk import (array of `{developer, team, division, org}`).

#### `GET /api/v1/org_hierarchy`
- **Scope:** write (discloses the full developer->team map).
- **Success:** `200 OK`, `{"hierarchy": [{developer, team, division, org}, ...]}`.

#### `POST /api/v1/period_membership/{developer}/end`
- **Scope:** write. **Success:** `200 OK`,
  `{developer, org, period_end}` (the applied end record). Request: `{org,
  period_end}`.

#### `DELETE /api/v1/developer/{id}` (GDPR Art. 17 erasure)
- **Scope:** write. **Success:** `200 OK`,
  `{"deleted": {<table>: <count>, ...}, "total_deleted": <int>}`; `404` when
  nothing matched (which makes a repeated erasure idempotent). Available in
  team-aggregation mode (compliance tooling, not a reporting surface).

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
  (or `?before=`) exclusive upper bound, `?team=`, `?work_type=`.
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
  - `work_types` (array of `workTypeSegmentJSON`, `omitempty`) -- the
    **authoritative** type-segmented view (#187): one entry per `work_type`, each
    `{work_type, aggregation?, developers?, teams?, total?}` scored over ONLY that
    category (the per-segment `aggregation` mirrors the top-level discriminator, #270).
    This is the surface for comparing developers/teams; the pooled
    `developers`/`teams`/`total` above are retained for back-compat and are not a
    cross-type ranking.
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
  actual_paid_usd, spend_leverage, coverage_pct, cost_per_point`. No CI at team
  grain (the bootstrap is a per-developer signal, #133).

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
    delta_tier, delta_weighted_points, delta_total_cost_usd, significant}` where
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
  `{status: "alive", uptime_s: <int>, version: <string>}`. `version` is the
  documented [feature-detection](#feature-detection) signal.

## API changelog

Newest first. Every entry names the change, its classification, and the issue.
Backfilled entries (`#185`, `#187`, `#191`, and the additive-column history)
predate this discipline and are recorded here so the record is complete.

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
  delete + re-post). A sanctioned audited override (ruling C) is a separate
  follow-up.
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
