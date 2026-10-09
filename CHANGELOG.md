# Changelog

All notable changes to TIER are documented in this file. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project will
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) once it
reaches v1.

## 0.5.2 - 2026-10-09

TIER 0.5.2 — public launch

**ONE-WAY UPGRADE — upgrading from v0.4.1.** Before upgrading, run
`mkdir -p ~/.tier/backups` and
`tierd backup --db ~/.tier/tier.db --out ~/.tier/backups/tier-$(date +%F).db`
(use the --db path you gave serve if it differs)
with **either binary** (see [README backups](README.md#operations)). The
v0.5.2 binary is safer: it refuses a missing or empty `--db` and does not migrate
the database. Check that the reported backup size is not ~4096 bytes.
The database moves from schema version 1 to 6. These schema upgrades cannot be
rolled back in place:

- **Version 2 — dated team membership (#886):** `hierarchy_membership` replaces
  the current hierarchy map for historical team and division reads. Older
  binaries neither maintain nor erase that ledger.
- **Version 3 — sealed reporting periods (#913):** erasure tombstones sealed
  person keys. Older binaries would leave those keys behind, still matchable
  using the install secret.
- **Version 4 — canonical-id history (#975):** alias edits record retired
  canonical ids in `canonical_id_history` so erasure can find their sealed keys.
  Older binaries neither record those edits nor erase the history rows.
- **Version 5 — permanent seal gaps (#913):** `sealed_gap` records months that
  must never seal, and sealing advances past them. Older sealers would keep
  trying the gap and never seal later months.
- **Version 6 — source watermarks (#913):** sealing waits for every registered
  source's settled coverage. Older sealers would ignore `source_watermark` and
  could permanently seal a month before a source had reported it.

Older binaries with the schema-version guard refuse the upgraded database;
binaries from before that guard must never open it. To roll back, stop `tierd`
and restore the backup taken before upgrading. That backup does not contain
data recorded after the upgrade; re-running the shipper recovers only what
local session files still hold.

0.5.0 and 0.5.1 were internal milestones and were never published.

### Security

- **Building with Go 1.26.9 closes nine standard-library advisories (#1136).**
  `GO-2026-6617` (HTTP/2 HPACK encoder race crash); `-6613`, `-6612`, `-6611`,
  `-6610`, `-6605`, `-6603` (net/http); `-6608` (net/textproto); `-6607` (crypto/tls).
  ⚠️ **The published image still carries all nine until v0.5.2 is published.**
  `ghcr.io/tiermetric/tierd:0.4.1` / `latest` was built with go1.26.6;
  measured 2026-10-08, `govulncheck -mode=binary` on that image reports all nine.

- Cap new client API token-event writes at $10,000 and apply /events token limits to /costs; capture and reprice clamp and mark oversized costs while preserving tokens and continuing. Existing data migrates without the ceiling (#1082).

- **Healthz watcher error values are now classes (#1095).** In both watcher blocks,
  nonempty `last_error` and `last_watch_add_error` return `watch_failed` and
  `watch_add_failed` instead of raw text, for every caller. Empty errors remain
  omitted, timestamps are preserved, and full errors remain in logs. Consumers
  parsing error text must use the classes instead.

- **BREAKING: team and division reports are sealed calendar months (#913).** In `team` and
  `division` mode, `/api/v1/scores`, `/api/v1/report_manifest` and `/api/v1/scores/compare` no
  longer answer an arbitrary window. Two overlapping windows subtract to a group smaller than *k*,
  so each report is now one calendar month, computed once after the month closes and its grace lag
  (`--report-grace`, default 14 days) ends, stored, and served byte for byte from then on. A month
  is chosen with `?period=YYYY-MM` (`?period_a=`/`?period_b=` on `/compare`); `since`, `until`,
  `before`, `team`, `repo` and `work_type` are `400`s. `tierd serve` runs a seal pass at startup
  and every hour. Nothing is sealed until you arm sealing once, after any history backfill:
  `tierd seal --arm YYYY-MM|earliest` with serve's config (or `seal_from`); until then the reads
  are `404` "sealing not armed". A `--read-only` serve never seals and serves what a writable
  server sealed. `tierd seal --status` and `tierd doctor` report a month that is not sealing, and
  startup warns when a config change leaves earlier months under the old config. Developer mode is
  unchanged. **The upgrade is one-way:** a sealed month is never recomputed, and the first seal
  pins the earliest month for good. See `docs/api-compatibility.md`.

- **A month is sealed only once every configured source has reported it (#913).** Each source
  `tierd serve` pulls from (the Anthropic Admin and OpenAI pollers, the subscription-fee
  reconciler, the Codex, Opencode and Muse collectors) records how far it has settled, and a month
  waits, as the stall `source_behind`, until every one has settled past its end without a gap.
  Until `tierd serve` has started once on the database and registered its sources, no month
  seals, `tierd seal --arm` included. A month before a source's first recorded coverage (for a
  poller, the first day of the month before its first pass; for subscription fees, the current
  month unless `active_since` is set) never seals automatically: arm a later month, or set
  `active_since`. `tierd seal --skip` records a month a source can never cover as a gap. The session watcher,
  webhooks and pushed costs are covered by the grace lag only. The database moves to schema
  version 6, one-way.

- **Team and division mode publish one breakdown per window (#864).** A `team` or
  `division` response now carries only the group rows and the company `total`. It
  no longer carries the per-work-type `work_types` view, `segment_reconciliation`,
  `cost_composition` (with its per-model rows), or `data_quality.unattributed_buckets`,
  `exploratory_cost_share` and `mixed_price_versions` (on `/scores` and both
  `/compare` windows; `price_table.version` names only the active table, and a
  floored warning is to return through a follow-up issue), and `?work_type=` is
  refused with a `400`. Each was a
  second breakdown of the same window, and two breakdowns subtract to a group
  smaller than *k* even when every published row has *k* people: team totals minus
  work-type totals gave a 3-person group's spend, and a model only one developer
  used showed that developer's spend. `data_quality.attributed_cost_share` is not
  published either: beside the group rows and the `total` it bounds one person's
  spend. `data_quality.attribution_coverage` is `"not_shown"` instead, and the
  dashboard says coverage is not shown. Spend under the `unattributed`
  pseudo-developer (an org-usage poller's remainder, proxy requests with no
  developer header) is left out of every figure, and
  `data_quality.excludes_unattributed_spend` is always `true`. When the `other` row
  does not reach *k*, the whole response is now withheld, named team rows included,
  and `data_quality.kanon_suppressed.withheld_teams` says so;
  `/api/v1/scores/compare` withholds a comparison when either window's own `other`
  row, or the comparison's own, does not reach *k*, and the dashboard now says so
  instead of "No data". **More responses are withheld after upgrading.** Developer
  mode is unchanged. #937 tracks an audit that would let the work-type and
  per-model views come back. See `docs/api-compatibility.md`.

- **BREAKING: the read token no longer scrapes `/metrics` in `team` or `division` mode (#944).**
  `/metrics` exports running spend and activity counters, and two scrapes a short interval apart
  give one person's spend and activity timeline, below the *k* floor. In anonymized modes the
  read-only token now gets `403` there, with a body naming the fix. A new scrape-only **metrics
  token** (`--metrics-token`, `TIER_METRICS_TOKEN`, `@/path/to/file`, or `metrics_token` in YAML)
  opens `GET /metrics` and no other route; the write token still scrapes too, and with no metrics
  token configured it is the only one that does. `serve` refuses to start when any two of the
  write, read and metrics tokens are equal. `developer` mode is unchanged, and an install with no
  token configured keeps `/metrics` open in every mode. **Action:** repoint a Prometheus job that
  scrapes an anonymized install with the read token at the metrics token. It is operator-only
  (`docs/security.md`, "The metrics token"). The flag, env var and config key exist only from this
  release, and an older binary refuses `--metrics-token` and a config with `metrics_token` at
  startup, so upgrade tierd with the metrics token set first, then repoint Prometheus.

- **A later push can no longer take another developer's push outcome (#938).** A push outcome
  belonged to its earliest commit by commit time, which the committer sets, so a commit dated
  earlier the same UTC day and pushed later re-owned the row. It is now ordered by GitHub's push
  time (`repository.pushed_at`), then commit time, then SHA; two pushes in the same second tie
  and fall back to commit time. The commit's own date still picks which day's row it joins.
  Stored outcomes keep their owners. A push with no usable `pushed_at` is still recorded, ordered
  after every push time and after every such commit recorded before it, so it can never take a
  row over, and is counted in `tier_push_missing_pushed_at_total`. The DSAR export's `push_outcome_commits` rows
  gain `push_order`. Push credit still follows the commit's author, which the committer asserts:
  make the default branch PR-only for strong attribution (`docs/how-it-works.md`). Entries a
  pre-#938 binary writes get `push_order` 0 and keep precedence permanently, and the schema
  version did not change, so do not run a pre-#938 binary against a database a #938 binary has
  opened.

- **The k-anonymity floor counts people, not identifiers (#856).** In `team` and
  `division` mode an identifier now fills one of a group's *k* seats only if it is
  on the roster during the window (after aliases are joined, so one person's
  identifiers count once),
  is not a bot (GitHub account type `Bot`, which the webhook now records, a `[bot]`
  login, or a known bot login such as `Copilot`), and has activity a collector, the
  proxy or a pull request captured in the window. Hand-entered `POST /api/v1/costs`
  spend and push-only merged work no longer count on their own. Each figure also
  needs its own *k*: a group's cost is shown only if *k* counted people in it have
  captured spend, its points only if *k* have merged work, its paid spend (and so
  `spend_leverage`) only if *k* have paid spend, and each non-zero part of the
  `coverage_pct` split only if *k* carry it. Before, bookkeeping rows, bots and
  unrostered identifiers could lift a group to *k*, and a five-person group where one
  person had spend, or an invoice, published that person's exact figure. A pull
  request whose author login is spelled like `unattributed` is no longer recorded. Uncounted identifiers keep
  their figures in their group. **More groups are withheld after upgrading**; the
  response reports how many identifiers did not count, and why, in
  `data_quality.uncounted_active_ids`. To count those people, capture their work
  through a collector or the proxy and put them on the roster. The floor protects
  what a read-token holder sees; the write token can edit the roster and is an admin
  credential until #908. See `docs/manager-faq.md` and `docs/api-compatibility.md`.

- **The proxies never forward `X-Tier-Token` to the provider (#865).** Every `X-Tier-*` header
  is now stripped from the outbound request with or without `--api-token`; before, a client's
  `X-Tier-Token` reached the provider when tierd ran without one.

- **Erasure and export now cover the watcher's tail-state (#919).** `DELETE
  /api/v1/developer/{id}` finds the person's `watcher_checkpoint` rows through the session ids
  of their stored events. It deletes a row whose session file is gone, and otherwise replaces
  its `cwd`, branch and session id with an `{"erased":true}` tombstone that the live watcher
  obeys: the watcher never reads that file again, appends, restarts and byte-identical rewrites
  included, until a file with different first bytes sits at the path. `tierd ship` does not read
  the tombstone. `GET /api/v1/developer/{id}/export` now returns these rows. The row's path key
  (which names the OS username, the session's working directory and the session UUID) is kept,
  and rows whose session stored no events cannot be found; `docs/privacy.md` states these and
  the other limits, and why a tombstone must not be deleted by hand while its file exists.

- **Browser-shaped requests are refused (#893, #903).** Writes under `/api/v1` need
  `Content-Type: application/json` or get `415`, in token mode too, so `curl -d` needs
  `-H 'Content-Type: application/json'`. A cross-origin browser request gets `403` on
  `/api/` for every method, and on every other path, the provider proxies included, for
  any method but `GET`, `HEAD` and `OPTIONS`. A tokenless tierd on loopback answers `403`
  to a `Host` that is not a loopback name or IP: browse via `127.0.0.1` or `localhost`,
  or set `--api-token`. See `docs/api-compatibility.md`, API changelog.
- **A loose `@file` secret is warned about (#920).** When a token or secret file given as
  `@/path` has any group or other permission bit, `tierd` logs one WARN with the setting
  and mode (never the path or the contents) and how to restrict it: `chmod 600` if `tierd`'s user owns the
  file, else the secret mount's file mode. It never refuses the file, and does not check on
  Windows. A WARN on a container mount `tierd` reads as another user is expected.

- **An alias edit never moves an identifier's past between teams (#914).** Before, team
  and division rows placed each event by the membership of the developer its identifier
  resolved to, so adding, re-pointing or deleting an alias moved that identifier's past
  figures to another team, including figures that were held out of every team row for
  being below *k*. A read token was enough to see the difference. Now each identifier
  keeps its own dated team history: an alias edit, and every move of a person, adds rows
  dated by the server clock for each identifier whose team it changes, so it affects only
  the future. Spend recorded under an alias before the alias was added stays in the team
  the identifier was in then, usually `other`. Both alias routes can answer `409` when the
  server clock reads earlier than a membership row they would close. On upgrade each
  existing alias gets a copy of its person's dated history, so no past window changes;
  where two identifiers of one person held different teams, each person also moves to
  their current `org_hierarchy` team from the moment of the upgrade. To do this the
  upgrade rewrites, once, the membership rows of each identifier of an aliased person
  that does not already hold that history: a span the identifier already held keeps its
  row's `written_by`, and a span copied from another of the person's identifiers is
  written as `migration:alias-history-v914`. This holds only while no binary older
  than #914 writes to the database: the schema version did not change, so a binary
  built from `main` between #912 and #914 can still open a database a #914 binary has
  opened, and its alias edits append no membership rows, which the one-time upgrade
  does not repair, so events under those aliases then place as unassigned. Do not run
  such a binary against that database; no released version is affected, since v0.5.0
  and v0.5.1 were never tagged. Erasure deletes the
  rows under every alias of the person; the export includes them. Until #913's sealed
  periods ship, an alias merge or unmerge can still change whether a past team clears
  the *k* floor (merging two identifiers of one team can hide it, deleting an alias can
  show it); #913 closes that.

- **Team rows are computed from dated membership, so moving a developer no longer
  rewrites history (#886).** Before, every score applied today's team list to every
  window. Reading `/scores`, moving one developer, and reading again showed that
  developer's exact figures in the difference of two team rows, whatever *k* was. Now
  each spend event counts toward the team its developer was in at the event's own
  timestamp, each merged outcome toward the team at merge time, and each invoice toward
  the team at the start of its month. This applies to team and division rows in `/scores` (including
  `work_types[].teams`), `team_rollups`, `?team=` and `/scores/compare`. A move affects
  only the future: an issue whose spend came before a move and merged after shows the
  spend in the old team and the merged work in the new one. History from before a
  developer's first assignment stays in `other` (the no-team row). On upgrade, every
  assignment already loaded is kept for all past data; only people assigned after the
  upgrade start on the day they are assigned. Every write through
  `PUT`/`POST /api/v1/org_hierarchy` or `tierd hierarchy import` is dated by the server
  clock; a `valid_from` or `valid_to` field or column is refused. Membership rows are
  only appended, never rewritten, and each records a fingerprint of the token that wrote
  it (never the token). Erasure deletes all of a developer's membership rows; the export
  includes them, without the fingerprint. Alias edits are dated too (#914, below). A developer whose only figure in a group
  is paid spend (an idle invoiced seat, or an invoice from a month that began before the
  seat was assigned) no longer counts toward that group's *k* floor, so a small group
  padded by such seats is now withheld. A move the server clock cannot date (the clock reads earlier than the
  developer's current membership began) is refused with `409`. `tierd demo` starts its
  synthetic teams at the upgrade baseline, so its team panel still shows them.
  ⚠️ **The upgrade is one-way.** The database moves to schema version 2 and an older
  tierd refuses to open it. Binaries older than #141 do not check the schema version, so
  the bump cannot stop them — back up before upgrading and never run them against an
  upgraded DB (every tagged release, from v0.1.0 on, has the check). Take the backup
  with `tierd backup --db <path> --out <file>`; the first command to open an existing
  database stored below version 2 prints this once. Rolling back
  means restoring that backup: data recorded after the upgrade is not in it, re-running
  the shipper recovers only what local session files still hold (about 30 days), and
  anything older is lost.
- **Erasure tombstones sealed person keys; schema version 3 (#913, #914).** The sealed
  reporting-period tables (not yet written by any report path) keep, per sealed period, a
  keyed hash of each person counted. `DELETE /api/v1/developer/{id}` now replaces the
  person's hashes with random tombstones in the same transaction, and its response carries
  a `deleted.sealed_person` count (rows tombstoned, included in `total_deleted`). What a
  sealed period keeps after an erase is in [docs/privacy.md](docs/privacy.md).
  ⚠️ **The upgrade is one-way.** The database moves to schema version 3, and a version-2
  tierd refuses to open it, because it would erase a person and leave their sealed keys
  behind. The first command to open an existing database stored below version 3 prints
  this once. Back up first with `tierd backup --db <path> --out <file>`; rolling back
  means restoring that backup, with the same data-loss limits as the version 2 upgrade
  above.

### Changed

- 🔴 **BUILD REQUIREMENT: Go 1.26.9+ (#1136).** `GOTOOLCHAIN=local` refuses an
  older toolchain. Go 1.27.0–1.27.1 carry the same nine advisories (fixed in 1.27.2).

- **BREAKING (#1095, S03-3):** `POST /api/v1/actual_spend` and `POST /api/v1/org_actual_spend` return `400` for omitted or null `actual_paid_usd` (previously `201` storing a $0 row); explicit `0` remains accepted.

- **`POST /api/v1/costs` refuses rows a running org poller already counts (#854).** The Anthropic
  Admin and OpenAI Usage pollers bring their org's recorded usage up to the provider's total and
  never subtract manual rows, so
  posting that org's usage by hand as well counted it twice, silently inflating total cost, the
  unattributed share and Spend Leverage. On a server that runs a provider's poller, a `/costs` row
  for that provider's models (tokened or cost only) is now refused `400` unless it declares
  `"billed_to": "other"`, for spend billed where the poller cannot see it (Claude Max seats, Bedrock
  or Vertex, another org). Installs without that poller are unchanged. The declaration is stored and
  exported. **Upgrade the server before any client sends `billed_to`**: an older server rejects the
  field. A keyed retry of a row stored before the upgrade, sent without `billed_to`, now gets `400`
  on such a server. The audited override (`override: true`) still corrects an existing row without
  `billed_to`; one that would insert a new row is refused. At startup, `tierd serve` logs one WARN
  per running poller with the count and dollar sum of undeclared manual rows on days that poller
  covered, and the remedy for each kind of row. **Upgrade note:** to list rows stored before the
  upgrade, with a sum per provider per month, use `GET /api/v1/events` (developer mode only; it
  returns 403 in team or division mode) or the read-only SQL in `docs/api-compatibility.md`
  (`POST /api/v1/costs`). The schema version is unchanged, because `billed_to` is an added column,
  so a binary older than #854 can still open the database; running one against it admits
  undeclared manual rows again (it has no refusal), and the #854 startup warning will count them.
  See `docs/api-compatibility.md`.

- **GLM-5.3 only (#921).** `docs/examples/prices-zai-coding-plan.example.yaml` is deleted (its rows re-labelled GLM spend `subscription`); `--opencode` now points at the embedded per-token `glm-5.3` row (#786), and no `glm-5.2` row is added. **Upgraders:** if you were using the deleted example file, or any `--prices` table that carries a `glm-*@zai-coding-plan` row, either drop `--prices` entirely (the embedded table prices GLM-5.3 per token), or rebuild your override from the current embedded table (`internal/store/prices.yaml`) with a fresh `version:` and no `glm-*@zai-coding-plan` row. Removing only that row is not enough: `--prices` replaces the whole embedded table, so an override built from the deleted example would leave GLM-5.3 priced at the guessed fallback rate. A host-qualified row outranks the embedded per-token `glm-5.3` row and keeps labelling GLM-5.3 spend `subscription`. `serve --opencode` no longer WARNs about `glm-5.2` at startup.

- **Price table v10: `claude-sonnet-5` is $2/$10, down from $3/$15 (#786).** Anthropic made the
  $2/$10 introductory price the standard price and cancelled the increase planned for 2026-09-01,
  so tables v7 to v9 overstated Sonnet 5 spend by 50%. Rows already stored keep the price and
  `price_version` stamp they were captured under. To re-price them against v10, run
  `tierd reprice --db <path> --from-version 7` (a dry run that prints what would change), then
  the same command with `--commit`. `--from-version N` covers every row stamped N or later, so
  one run with 7 covers v7, v8 and v9, and it re-prices every model whose rate moved, not only
  Sonnet 5 (Fable 5.1 and Opus 5.5 rows move from the guessed rate to their audited rate).
  `--commit` refuses if any changed row would land on a guessed rate; read the dry run before
  adding `--allow-guessed`. The version bump changes the embedded table hash recorded in
  `price_table_registry`, which is expected.

- **`premium_model_share` threshold lowered from $5/M to $4/M input (#786).** Opus 5.5 lists at
  $4/M, so at $5 it counted as non-premium while every other Opus did. No other row lists in
  [$4, $5), so Opus 5.5 is the only model this reclassifies. `premium_model_share` rises by
  Opus 5.5's cost share wherever it is used.

- **The DEFAULT `?since=` window now opens at the start of its UTC day (#746).** With `?since=`
  omitted the lower bound was `now − 90d` *carrying the current time of day*; it is now snapped
  BACKWARD to `00:00:00Z`. The snap lives in the single shared `parseSince` default branch, so every
  windowed read inherits it identically — **six call sites, NINE endpoints**, because
  `parseExportParams` is one call site serving four routes:
  `GET /scores`, `/scores/{developer}`, `/scores/compare`, `/report_manifest`, `/org_actual_spend`,
  and the four bulk exports `/events`, `/outcomes`, `/quality_events`, `/quality_history`.
  **Explicit bounds are unchanged** — `YYYY-MM-DD` / `YYYY-MM` / `YYYY` already parsed to
  midnight UTC.
  🔑 **It removes a lossy truncation rather than adding one.** `/scores` has always ECHOED the
  bound as a bare calendar day, so a default response said `"since":"2026-05-31"` for a window that
  actually opened at `2026-05-31T05:00:58Z`. The report manifest published the honest instant, and
  `tierd verify-report` then refused it with **exit 2 (could not check)** — `/scores` accepts only
  whole-day bounds, so the DEFAULT report shape was the one shape that could not be re-run. It is
  now re-runnable. ⚠️ *Re-runnable is not "returns 0"*: a default manifest has an OPEN upper bound,
  so re-running it recomputes over `[since, ∞)` against the database as it stands, and any ingest
  since the manifest was taken is an honest `1` (DIVERGED) naming what moved. What changed is that
  the run happens at all.
  ⚠️ **A default window is up to 24h wider, so the numbers can move on a populated install.**
  Measured on a fixture (`cmd/tierd/verifyreport_defaultwindow_test.go`) carrying three $4 events at
  the UTC midnight that opens the window, alongside three $3 events deep inside it: the default
  report's `total_cost_usd` goes 9 → 21 and a developer row the old bound excluded appears. Both
  figures are asserted by that test, so this entry cannot drift away from the fixture. Widening
  never hides spend;
  snapping forward would have dropped a partial day of cost out of a cost metric, which is why the
  snap is backward.
  🔑 **The bound is now host-timezone-INDEPENDENT, which the snap made load-bearing.** The
  subtraction is done in UTC (`now.UTC().AddDate(0,0,-90)`), not on the host's wall clock. Doing it
  the other way removes 90 entries from the LOCAL calendar — 90×24h ± a DST offset — and before the
  snap that hour of slop merely shifted a mid-day bound by an hour. After the snap it decides which
  side of a midnight the bound falls on, i.e. a whole day of window. Measured over 2026 at
  10-minute resolution: the two orderings pick different UTC days for **2.07% of samples** on
  `America/New_York`, `Europe/London` and `Australia/Sydney` (in opposite directions north and
  south), and **0.00%** on UTC and on fixed-offset `Asia/Kolkata` — which is what identifies DST,
  not the offset, as the cause. Two installs in different zones would otherwise have served default
  windows a day apart.
  ⛔ **`verify-report`'s refusal is deliberately untouched.** A hand-written or pre-#746 manifest
  carrying a non-midnight instant still exits 2 rather than being truncated into a different
  window. 📖 `docs/api-compatibility.md` (API changelog), `docs/reproducibility.md` §4.

- 🔴 **BEHAVIOUR BREAK: a `--prices` override that reuses the embedded table's `version:` with
  different rates is now a startup FAILURE (#714).** This was previously accepted. It is the
  intended fail-closed posture, not a regression: `price_version` is stamped INSERT-only on every
  event and is the only record of which rates produced a cost, but it is just a *number* — it
  resolves to actual rates through a file that may not be version-controlled. An operator who
  edited a rate without bumping `version:` made one integer denote two different price tables, and
  the rows stamped under each were **indistinguishable forever**.
  ✅ **The error IS the remedy** — the open-time refusal names the database, the version, both
  `table_hash` values, where the recorded one came from, and a copy-pasteable command; the
  load-time one names the override path. **The fix is always to bump `version:`.**
  ⚠️ **A refused open is a total service outage**, not degraded pricing: the guard runs before
  anything is served, so dashboard, API and ingestion all stop. That is the deliberate
  integrity-over-availability choice — size your change control for a price edit accordingly.
  ⛔ **There is deliberately no `--accept-rehash` flag and there will not be one:** it would
  silently rebind the meaning of every historical `price_version` stamp, with a green startup log.
  📖 `docs/reference-price-table.md` §9. Overrides conventionally start at `version: 1000`.

### Added

- **Worktree attribution for Claude Code spend, off by default (#823). UPGRADE TIERD (THE
  SERVER) FIRST.** A Claude Code session file records the branch of the folder a session
  *started* in, so work done in a git worktree after starting in the main checkout (and the
  subagents that inherit that start) was booked to `unattributed:main`. With
  `--worktree-attribution` on, TIER attributes each message to the worktree its tool calls
  worked in, using the branch that worktree had checked out at that moment, read from git's own
  files (the `.git/worktrees/<name>/` admin folder and its reflog); it never runs `git` to find
  the branch. It decodes the `file_path`, `notebook_path` and `path` values of each tool call's
  input and keeps one path per call for seven tools (`Read`, `Edit`, `MultiEdit`, `Write`,
  `NotebookEdit`, `Glob`, `Grep`); it never reads file contents, the tools' other arguments, or
  command lines. `tierd ship` and `tierd score` read the tool paths of every Claude Code session
  in the `--since` window, and the git files of the worktrees they name, before dropping the
  sessions outside the configured repositories. No path is stored in an event or sent; with the
  switch on, the live watcher's local checkpoint row also keeps the absolute path of the
  worktree it is carrying between messages (`docs/privacy.md`, "Worktree attribution"). The
  switch is on `tierd serve` (its live watcher), `tierd ship` and `tierd score`; precedence is
  the flag, then `TIER_WORKTREE_ATTRIBUTION` (any `strconv.ParseBool` spelling; any other value
  stops the command), then the `watch.worktree_attribution` config key (serve only: `ship` reads
  no config file), then off. `ship` and `score` print a startup line only when it is on; `serve`
  always logs it. `tierd doctor` ignores the switch. Stored rows keep their issue and rule.
  - **Server side (additive).** `token_events` gains a nullable `attribution_rule` column naming
    the rule that chose each row's issue: `branch`, `worktree-cwd`, `worktree-toolpath` or
    `carry`, written once on insert and never updated. `POST /api/v1/events` accepts it and the
    new issue label `unattributed:foreign-repo` (spend in a worktree of a repository you did not
    configure; its repository is stored as `unqualified`, so that repository is never named).
    `GET /api/v1/events` appends `attribution_rule` as the last JSON field and CSV column, with
    `legacy` for a row that recorded no rule (every row stored before this release) and
    `unknown` for any other stored value. See `docs/api-compatibility.md`.
  - **An opt-in preview in this release, not switched on.** It failed two of its three release
    gates: carry in a parent session (any top-level session, interactive ones included) was wrong
    in 12 of 14 hand-checked messages, while attribution from a tool-call path held up. A stored
    attribution is permanent, so read `docs/how-it-works.md`, "Measured accuracy" (the three
    gates and their results), before turning it on. Limiting carry to subagent session files is
    #1016.
  - **Upgrade `tierd` (the server) before turning the switch on for `tierd ship`.** (A server's
    own watcher writes its own store, so `serve`'s switch needs no upgrade order.) Check the
    server with `tierd version` on its machine, or `GET /api/v1/version`. A `tierd` without
    #823 rejects with HTTP `400` every batch that carries `attribution_rule` (every Claude Code
    event does, with the switch on) or `unattributed:foreign-repo`, and `tierd ship` treats a
    `4xx` as final: it prints `ship <repo path>: … server returned 400 …` and exits 1, and the
    whole run stops there, so its later `--repo` targets and its Codex, Opencode and Muse passes
    do not ship either. With the switch off nothing changes on the wire. **If the order went
    wrong,** upgrade the server and re-run the host's usual full `tierd ship` command, with all
    its flags, plus `--worktree-attribution`, with a `--since` date on or before the last
    `tierd ship` run that succeeded (or omit `--since`: the default, 90 days ago, covers it).
    Recovery works while the source files exist: Claude Code deletes session files after its
    `cleanupPeriodDays` setting, 30 days unless changed. A message more than 30 days old at the
    re-ship is booked by the branch rule, and keeps that issue.
    `TestRunShip_WorktreeAttribution_WrongOrderUpgradeRecovers` pins that recovery.
  - **Rolling back `tierd serve`:** a `tierd` older than #823 refuses to start with
    `watch.worktree_attribution` in its config file (it rejects unknown keys); delete that line
    first. `config.example.yaml` ships it as `worktree_attribution: false`.
  - **Preview it first:** `tierd score --repo <path> --worktree-attribution` reads the same
    session files with the switch off and on, prints the report as it would be with it on, and
    then a dry-run audit: counts per rule and per repository, the `unattributed:foreign-repo`
    count, and up to 20 changed messages (time, session id, old and new repository and issue,
    rule). It prints no path and no message content, and stores nothing. It compares two local
    scans: the server keeps the first issue, repository and rule it stored for a message, so
    only messages it has not stored yet change. `score` indexes only its one `--repo`, so a
    worktree of another repository you ship shows there as `unattributed:foreign-repo`.
  - With the switch on, the live watcher holds back the message still being written at the end
    of a session file and releases it after 600 seconds with no new line. A line that arrives
    after its message was released is stored again under the same key; the store keeps the
    larger token counts and the first row's issue, rule, cost and price version. An erasure
    can miss a message held back in a session that has no stored event yet; it is stored when
    released, so erase again after the hold (`docs/privacy.md`, erasure limits).
  - Turning the switch on by default is a later, separate change.

- **A `--prices` override that changes a built-in per-token model's `billing_mode` is WARNed at startup (#921).** Every command that loads `--prices` logs one WARN per such model and host (its own row, or a `<model>@<host>` row that outranks it) naming both modes and the remedy; the override still loads and prices as written.

- **Price table v11: Meta Muse Spark rows and the `meta` provider (#895).** New rows, from Meta's
  own pricing page (dev.meta.ai/docs/pricing-rate-limits, undated, read 2026-09-27):
  `muse-spark-1.3`, `muse-spark-1.2` and `muse-spark-1.1` ($1.25/$4.25, cached input $0.15), and
  the Contributor tier `muse-spark-1.3-contributor` and `muse-spark-1.2-contributor`
  ($0.10/$0.20, cached input $0.002), which Meta discounts in exchange for training on the
  prompts and completions. Meta publishes no cache-write rate, so the new `meta` provider bills
  cache writes at 1.0x input. Reasoning tokens are part of Meta's output total, so they carry no rate of their
  own. Muse is priced per token at list price, also when it runs on a Muse subscription.
  Web-search grounding ($2.50 per 1,000 queries) is not a token charge, so Muse cost is a floor
  wherever it searches. The table hash recorded in `price_table_registry` changes, which is
  expected.

- **Price table v10 rows and the `zai` provider (#786).** New rows: `claude-fable-5-1` and
  `claude-mythos-5-1` ($10/$50, cache read 0.025x), `claude-opus-5-5` ($4/$20, cache read 0.05x),
  `gpt-6-astra` ($10/$50), `gpt-6-sol` ($2/$10) and `gpt-6-luna` ($0.10/$0.50) with cache read
  0.1x and cache write 1.25x, and `glm-5.3` ($1.40/$4.40, cache read $0.26) and `glm-5.3-flash`
  ($0.15/$0.50, cache read $0.03). Z.ai publishes no per-token cache-write rate, so the new
  `zai` provider bills cache writes at 1.0x input. GLM is priced per token at list price, also on
  the Z.ai coding plan. `NormalizeModel` now strips opencode's `zai-coding-plan/` prefix, so
  `zai-coding-plan/glm-5.3` resolves to `glm-5.3`. Fable 5.1 and Opus 5.5 previously priced at the
  guessed $0.50/M fallback.

- **Opencode capture (#719).** `tierd serve --opencode` and `tierd ship --opencode` read
  the SQLite session store Opencode writes to `~/.local/share/opencode/opencode.db`,
  READ-ONLY (`mode=ro`, never `immutable=1` -- Opencode holds a live WAL, and
  `immutable=1` returns stale or torn data with no error). It is the third local
  per-call source, alongside the Claude Code JSONL watcher and the Codex rollout-log
  collector, and it is `ShippableSource`.
  🔴 **Its token arithmetic is the OPPOSITE of Codex's.** Opencode reports `reasoning`
  tokens BESIDE `output` rather than inside it, so billable output is
  `output + reasoning`. Measured across the maintainer's entire GLM-5.3 corpus,
  `total == input + output + reasoning + cache.read` on 3,943 of 3,943 rows and
  reasoning EXCEEDS output on 77% of them -- mapping it the Codex way drops 84.8% of
  the output-side bill, and reusing Codex's `reasoning <= output` check rejects 77%
  of the corpus.
  It captures only routes with an AUDITED per-token rate (today the Z.ai coding plan,
  #712) and names every exclusion at startup with a per-scan row count; Ollama's cloud
  tier is excluded because it publishes no per-token rate. The embedded table's
  `glm-5.3` and `glm-5.3-flash` rows price it per token (#786), so no price-table
  override is needed; `tierd serve` WARNs at startup if a `--prices` table drops them.
- **`tierd ship --prices` (#719).** Mirrors `tierd score --prices`. The server re-prices
  every shipped event authoritatively, but the shipper also computes a local `cost_usd`
  the server compares against to detect a mixed-version fleet -- so a shipper on a
  different table reports a divergence on every event. It matters most for a route whose
  rate lives only in an operator override.

- **Meta Muse Code capture (#895, #898, #901).** `tierd serve --muse` (env `TIER_MUSE`, or the
  `collectors.muse` config block, which also sets `home` and `scan_interval`) and
  `tierd ship --muse` (`--muse-home` overrides the Muse home) read the log Meta's Muse Code CLI
  writes per session, at `~/.local/share/muse/sessions/YYYY/MM/DD/<id>/session.jsonl`. It is
  off by default. Under `serve`, `--muse` needs `--watch-repo`: with no watched repository
  `serve` refuses to start (`--read-only` instead logs a WARN that Muse spend will not
  be recorded and disables all capture). The proxy has no Meta route.
  The stored Muse session id is the log's opaque `stream.id`, with no prompt, reply
  or file content; see [docs/privacy.md](docs/privacy.md) for the stored-field inventory.
  Two record kinds are billed, one token event each: `model_completed` (an agent model call)
  and `automated_review_completed` (Muse asking a model whether a pending tool call is safe).
  The token mapping was measured on Muse Code 1.4.0, on one session's 15 `model_completed`
  records: cache read is inside `input_tokens`, so it is carved out and priced once at the
  cache-read rate, and reasoning is inside `output_tokens` (as with Codex, unlike Opencode).
  `cache_write_tokens` was 0 on all 15, so where it sits is unmeasured: it is assumed to be
  inside input and priced at the input rate. A call that breaks an invariant is refused,
  counted by reason in `refused_calls` and WARNed -- never guessed.
  ⚠️ **A call is emitted only after its run settles,** because Muse writes a run's branch
  after the run's last model call and a stored row's issue id is never rewritten. A session
  file unwritten for 6 h counts as settled, so a **live** run waiting more than 6 h at a
  tool-approval prompt is filed as `unattributed:detached-head`, permanently.
  Subagent spend is filed under the parent run that spawned it (#901). Three cases are
  excluded rather than guessed onto a branch: a subagent no single parent run is linked to
  (held, then excluded once both logs go 6 h unwritten; WARN `Muse subagent calls held or
  excluded: no parent run is linked to this subagent`), a subagent of a subagent, and a
  subagent whose parent recorded no `workspace_root` (WARN `Muse calls excluded: session has
  no workspace_root`; the nested case raises it too, or the first WARN when its intermediate
  log is missing). For those, recorded spend is lower than real spend.
  Calls are priced at Meta's published per-token API rates (the v11 `muse-spark-*` rows),
  even on a Muse subscription. Web search is billed per query and is not counted, so Muse
  cost is a floor for any session that searched.

- **`price_table_registry` — the content identity of every price table this deployment has served
  (#714, building on #713's `table_hash`/`file_hash`).** One row per `version:` integer, recording
  both hashes, the effective date, the model count, the source, and which binary first served it.
  Two fail-closed guards refuse a second, *different* table claiming a version already in use: one
  at **load time** (inside `store.LoadPriceTable`, so `tierd score-log` — which opens no database
  — is covered too), and one at **open time**, placed before any phase that prices or stamps a row,
  so a disputed version can never reach the data.
  ⚠️ Only `table_hash` is compared. A comment-only edit moves `file_hash` and is **not** a
  collision; the same table loaded from a second path is not one either.
  🔑 Uniqueness on `version` is a **partial** unique index (`WHERE forgotten_at IS NULL`), so a
  retired identity stays readable as evidence without blocking re-registration of that version.
  Both halves depend on that one predicate.
  ⚠️ A hash recorded under an older *canonicalization scheme* is treated as **not comparable**
  rather than as a collision, so a future fix to the serialization cannot brick every deployment
  simultaneously.
  ✅ **Downgrade-safe:** `schemaVersion` is not bumped — an additive table no older *data* read
  path touches. One caveat, stated because the next additive table will inherit it: a generic
  `sqlite_master` enumerator selects everything, so a pre-#714 `tierd demo` opening a post-#714
  database refuses to recreate its demo data (fail-closed — it over-refuses, never over-deletes).
  ✅ **UNKNOWN stays UNKNOWN, with a stated limit:** a `price_version` with no registry row simply
  has no recorded hash, and TIER does not mint one for a table nobody possesses. A recorded row is
  authoritative only for rows written at or after its `first_seen`. **Nothing consumes the registry
  as a verifier today** — it records identity and refuses collisions; it does not certify a row.
- **`tierd prices list` — read the registry.** Read-only, works on a database `store.Open` is
  currently refusing, prints `first_seen` with the scoping note, and marks retired identities.
  *A guard you cannot inspect is a guard you can only escape.*
- **`tierd prices forget-version` — the documented escape hatch for that refusal, and it RETIRES
  rather than deletes.** DRY RUN unless `--commit`; it prints (or `--json`-emits) the full row
  **before** writing anything, and a failed write **aborts the operation**, so the record cannot be
  lost silently. It reaches the database directly because the whole point is to fix a database
  `store.Open` is refusing. Its legitimate use is narrow: the *recorded* row is the wrong one.
  *A fail-closed guard with no documented remedy is how a guard gets commented out.*
  🔴 **The row is KEPT and stamped** (`forgotten_at`, `forgotten_by`), and a `price_forget_audit`
  ledger entry records the act — the same family `reprice` and `repair-repo` write into. After
  retiring version 9, the database can still answer *"version 9 once meant `tierpt1:7acfb82b…`, and
  that record was retired at T by U."* An earlier draft hard-deleted, which preserved the remedy but
  destroyed the evidence — the same end state a bypass flag produces. **This program exists so
  provenance is not lost; an escape hatch that erases the record of its own use contradicts the
  thing it protects.**
  ⚠️ `forgotten_by` defaults to the OS username, is settable with `--actor`, and is **self-asserted**
  — a local CLI has no principal to bind it to. Do not automate the command: running it every boot
  converts fail-closed into fail-open.
- **`tierd prices audit` — the append-only ledger of every retirement**, readable with `--json` and,
  like `list`, on a database `store.Open` is refusing. It carries its own copy of `table_hash`, so it
  still answers what a version meant even if the registry row is later removed by hand.

### Fixed

- **Self-hosted model sizes are parsed as whole parameter counts (#1083).** Names such as
  `13b` and `31b` no longer match `3b` or `1b`, and quantization labels such as `4bit` are
  not treated as model sizes. Guessed prices for some self-hosted model names change by
  about 4–5× with the default reference rates. The fallback warning and help text name
  the `self-hosted-medium` class so they remain accurate with a `--prices` override.

- **Claude Sonnet 5.5 is priced at its published rate; price table v12 (#1047).** Claude Code
  writes `claude-sonnet-5-5`, which had no row, so its spend was priced by the self-hosted-medium
  guess ($0.50/M combined) behind a `ship` WARN, and the dashboard and `/scores` showed that guess
  as measured cost. The new row is Anthropic's published rate (pricing page read 2026-10-02):
  $2 input, $10 output, $0.20 cache read, $2.50 5-minute and $4 1-hour cache write per million
  tokens. Rows already stored at the guessed rate keep it on re-ship. To re-price them against
  v12, run `tierd reprice --db <path> --from-version 10` (a dry run that prints what would
  change), then the same command with `--commit`. `--from-version N` covers every row stamped N
  or later, so 10 covers Sonnet 5.5 rows captured under v10 or v11; use a lower N if yours were
  captured under an older table. v11 only added Muse Spark rows, so the only other rows 10
  re-prices that 11 would not are Muse Spark rows captured under v10, which move from the
  guessed rate to their audited rate. `--commit` refuses if any changed row would land on a
  guessed rate; read the dry run before adding `--allow-guessed`. The zero-token `<synthetic>`
  rows Claude Code writes no longer trigger that refusal (#1057). The table hash recorded in
  `price_table_registry` changes, which is expected.

- **A squash merge counts once, whatever order its events arrive in (#849).** With push capture
  on, a squash merge whose `push` event arrived before its `pull_request` was stored twice. Each
  captured commit is now recorded in a per-commit ledger, and the PR outcome removes its merge
  commit in the same transaction: a push outcome that held only that commit is deleted, and one
  that holds other commits stays, re-owned to its earliest remaining commit. A redelivered or
  concurrent event writes nothing extra. Push outcomes stored before the upgrade are never
  deleted, so a double count already stored stays. Each change is audited in
  `push_outcome_audit`, which the report manifest now watermarks; the DSAR export and erasure
  cover the ledger and the audit; erasing one developer keeps a co-contributor's commit on the same
  push outcome. `tierd backfill` and `POST /api/v1/outcomes` reconcile too. The webhook's writes
  wait out SQLite's busy timeout, since GitHub does not redeliver a failed webhook. A push that carries a `Merge pull request #N` commit beside captured
  commits is now logged and counted in `tier_push_merge_commit_captures_total` (#934).
- **`POST /api/v1/costs` no longer writes to another identity's row (#871).** A keyed post whose cost matched the stored row but whose developer, issue, model, source or fidelity did not returned `201`, wrote nothing for the caller, and raised that row's token counts; it now returns the override path's identity-mismatch `409`, and a same-identity re-post no longer changes stored token counts. An owner's same-cost retry can also `409` now, when its fidelity differs from the stored row's (a pre-#82 value outside `daily`/`estimated`, or `daily` first and omitted on the retry); nothing is written either way.
- **Re-enrolling a developer no longer re-splits the org's past invoices (#867).** Assigning an
  org to a developer whose org was cleared, or whose membership was ended, used to open the new
  seat at `0000-01`, so every past invoice of that org was re-split and the other members'
  historical Spend Leverage allocation fell. Such a seat now opens in the current month; only a
  developer with no membership history, under any alias, still opens at `0000-01`. Seats already
  opened at `0000-01` by an earlier re-enrolment are not repaired: find them as an open
  `period_membership` row with `period_start = '0000-01'` for a developer who also has a closed
  row. A roster re-import still reopens a membership ended through `/end` (#945).
- **The Anthropic Admin and OpenAI Usage pollers now subtract Opencode's per-call rows (#875)**
  from the org aggregate. The collector captures only the Z.ai route today, so no install was
  double-counting; without this, adding an Anthropic or OpenAI route would have.

## [0.4.1] - 2026-08-24

### Security

- **The build toolchain floor is Go 1.26.6 (#694).** `go.mod`, `tools/docgen/go.mod`, the
  `Dockerfile` builder pin and `release.yml`'s `setup-go` all move together. This closes eight
  fixable HIGH stdlib CVEs -- `CVE-2026-33818`, `-39821`, `-46600`, `-56853`, `-56858`, `-56859`,
  `-56860`, `-56862` -- in every future build.
  ⚠️ **It does NOT retroactively fix an already-published artifact.** A container image is
  immutable: `ghcr.io/tiermetric/tierd:latest` keeps all eight until it is rebuilt and re-released
  (#683). The same eight are in the v0.4.0 Release-page tarballs, which no gate scans at all.
  🔑 **The artifact never changed -- the vulnerability database did:** the identical digest measured
  0 fixable HIGH on 2026-08-06..08-13, 2 on 08-14 and 8 on 08-22, across zero commits.
- **`scripts/image-cve-rescan.sh` now asserts the four Go pins are in lockstep**, run by
  `make check`. Previously the only detector for a half-bump was the nightly CVE re-scan -- i.e.
  *after* publication.

### Changed

- 🔴 **BUILD REQUIREMENT: you now need Go 1.26.6+** (was 1.26.5+). There is deliberately no
  `toolchain` directive, so under `GOTOOLCHAIN=local` an older toolchain is **refused**
  (`go.mod requires go >= 1.26.6`) rather than silently building a vulnerable binary. Under the
  default `GOTOOLCHAIN=auto`, `go install` fetches 1.26.6 automatically.

### Added

- **`tier_sqlite_wal_bytes` -- the size of the SQLite `-wal` sidecar, sampled
  every 5 minutes by `tierd serve`, with a `WARN` past 64MiB (#669).** It exists
  because raising the connection pool (below) makes WAL checkpoint starvation
  reachable: a passive checkpoint can only RESET the write-ahead log when no
  reader holds an older snapshot, so a client polling a read endpoint with no gap
  can keep the WAL growing indefinitely. **The first symptom of that is DISK, not
  latency** -- there was previously no signal at all. A missing `-wal` reads as
  `0` and is the healthy state (the sidecar does not exist until the first write
  and is removed on a clean close).

### Changed

- **#669 -- the store's connection pool is 4, not 1.** A request-path write used
  to block at CONNECTION ACQUISITION rather than at the write lock: measured, a
  bounded write issued while a 1.5s read was in flight took **1.450s against a
  250ms bound**, because the reader held the process's only connection. At a pool
  of 4 the same write takes **0.514ms**. `SetMaxIdleConns` is set equal to the
  maximum -- nothing set it before, so `database/sql`'s default of 2 applied,
  which at a pool of 4 continuously destroys and rebuilds two connections and
  re-runs the DSN pragmas with a cold page cache in front of a 138ms scan.
  ⚠️ *This buys LATENCY ISOLATION, not read throughput.* Four concurrent readers
  running window scans went 298ms -> 274ms, i.e. **1.09x**: `modernc.org/sqlite`
  reads are pure-Go and CPU-bound. Do not size this pool as a throughput knob.
  *Depends on #668* -- above a pool of 1, every DEFERRED read-then-write becomes
  an unretried `SQLITE_BUSY_SNAPSHOT` (517) race, so those sites had to take the
  write lock up front first.
- **#668 -- the last DEFERRED read-then-write store sites take the write lock up
  front.** `UpsertHierarchy`, `UpsertHierarchies`, `EndMembership` and
  `UpdateQualityForOutcome` now use the bounded `BEGIN IMMEDIATE` helper; the two
  `subscription.go` reconcilers and `UpdateQuality` use the unbounded one. Their
  atomicity previously came from `SetMaxOpenConns(1)` handing the transaction the
  process's only connection -- a correctness property resting on a pool-size
  constant. This is the precondition for raising that constant (#669).
- **BREAKING (status code):** the three org-hierarchy write routes answer
  write-lock contention with `503` + `Retry-After: 1` instead of `500`. No client
  loses a success it used to get -- measured, the previous shape failed in ~65us
  with an unretryable `SQLITE_BUSY` rather than waiting. See
  `docs/api-compatibility.md` for the full entry.
- The GitHub webhook endpoint answers the same condition with `503` +
  `Retry-After` instead of `500`, so a transient lock conflict no longer logs as
  a handler error pointing at a broken database.

## [0.4.0] - 2026-08-05

**The anonymity release. If you run TIER with `--aggregation team` or `division`,
upgrade.** Three defects let a k-anonymized cohort's spend be recovered, and all three
were fixed after v0.3.0 — so v0.3.0 is the last release that carries them:

- **#593** — the k-anonymity floor was applied only to *named* groups. The residual
  `"other"` bucket and the response `total` were never floored, so a cohort below the
  floor could be recovered by subtraction.
- **#466** — `?work_type=` bypassed that escalation entirely, which made the recovery
  **exact** rather than approximate. Measured on a real fixture: a suppressed
  developer's spend came back to the cent.
- **#619** — the reserved `unattributed` sentinel could be supplied as a `developer`,
  removing the forger's own spend from the denominator every other figure is computed
  against.

⚠️ **The compare-view withholding fixed by `#613` below was never live in a published
release, but not for the reason you might assume.** The compare view itself DOES exist
in v0.3.0 and does print below-floor readings there — that was `#603`'s deliberate
"muted but printed" treatment at the time, not a defect. What `#613` fixes is the
*contradiction* introduced when `#605` made the org card withhold its headline: a card
withholding a number while the row beneath republished it. `#605` landed 2026-08-04,
after v0.3.0, so no published release ever had a withheld value to contradict.

### Added

- **`segment_reconciliation` on `GET /api/v1/scores`** (#466) — accounts for the
  window's whole spend against the work-type segments, so the spend they
  structurally cannot categorize is reported instead of silently dropped. Three
  disjoint buckets per developer plus a name-free rollup:
  `outcome_linked_cost_micro + no_outcome_cost_micro + unattributed_cost_micro ==
  window_cost_micro`, exactly.

  Every figure ships as both `_usd` and `_cost_micro`, and the invariant holds on
  the integers ONLY. Each dollar figure is an independent float conversion, so a
  consumer evaluating `a + b + c === d` on them gets false for a substantial
  fraction of realistic magnitudes — 22601 of 216000 triples in a deterministic
  SYNTHETIC sweep spanning $0.008 to $78 per component
  (`TestSegmentReconciliation_DollarsAreNotExact`), against 0 of 216000 on the integers.
  That rate is a property of the sweep, not a measurement of a production window.
  Assert on the micros; display the dollars.

  It reconciles against the underlying cost **rows**, each counted exactly once —
  **not** against the sum of the segments' totals, which is not an invariant and was
  the issue's own stated acceptance criterion. The segments legitimately
  double-count: an issue carrying two work types is charged to both, and under the
  tolerant repo join (#231) a repo-blind cost row is charged to every qualified
  outcome sharing its issue id. Both over-counts are deliberate — they lower TIER,
  so ambiguity never flatters a developer — which is exactly why a subtractive gap
  (window minus the segment totals) could have come out NEGATIVE on ordinary data. That
  consequence, not the double-count itself, is what settles the question.

  `no_outcome` and `unattributed` are separate buckets and are never merged.
  `unattributed` already means "cost that could not be tied to an issue"; this gap
  is the other way round — cost successfully tied to a real issue that produced no
  outcome. The two are disjoint, never opposites, and never merged.

  Per-developer rows are dropped entirely in an anonymized mode (team/division); the
  name-free rollup always ships. The block ships the NUMBER — `internal/dashboard` does
  not render it yet, so today the gap is visible over the API only.

- **`tierd repair-repo`** (#493) — repairs the `repo` column on cost rows already
  stored as `unqualified`. Nothing in the tree could do this before: `repo` is
  deliberately excluded from the ingest path's `ON CONFLICT … DO UPDATE` clause so
  a repo-blind producer can never downgrade a row another producer qualified,
  which also made the #491 shipper fix invisible on re-ship. Dry-run by default;
  `--commit` applies in one transaction with a per-row before-image ledger.

  Its report names the two ways a repair can be silently PARTIAL, because both
  otherwise printed as a clean run: unqualified rows sitting under a sibling
  identity in `developer_alias` (`token_events.developer` stores the raw producer
  id, so one person's rows routinely sit under two names — measured 3 repaired,
  4 left, no warning), and mapping entries that matched no row at all (a stale
  session export, a typo, or a UTF-8 BOM on line 1 of `--map-file`, which
  `TrimSpace` does not strip). The repair's `--developer` scoping stays EXACT in
  both cases: widening it would risk re-attributing another person's spend.

### Changed

- ⚠️ **The compare view now publishes a TIER digit only when its own side is ranked**
  (#613) — **this amends #603, and it
  amends #613's own first answer.** It is not a bug fix; it is a ruling.

  The rule, and the whole rule:

  > **A TIER digit is printed iff its OWN side is ranked. A derived figure (Δ) is
  > printed iff BOTH sides are ranked.**

  There is no row count, no grain and no builder name in that sentence, which is why
  one rule now governs every row in the view.

  **What #603 ruled, and why it was superseded.** #603 treated a below-floor row as
  "muted but printed": a row sits in a list, and the muting plus the tag beside it
  carry the verdict. That broke when the k-anonymity fold yields a **single cohort**,
  because then the one row **is** the org total — the card above withheld the org
  headline as `—` (#605) and the row republished it in full one line down, including
  a Δ cell that printed unconditionally and so reconstructed the withheld
  `Δ Org TIER`. Muting is a styling hint, not a semantic; it cannot propagate through
  arithmetic, which is why #605 had to exist at all.

  **What #613's first answer got wrong.** It withheld the whole value column on the
  row-level conjunction — stricter than the card above it, which has always gated
  each `Org TIER` cell on that side's own verdict and only the Δ on both. That excess
  strictness became a leak: publication gated on the conjunction while **plotting**
  gated per side, so a `—`/`—` row still placed its ranked dot, and dot positions are
  written as inline percentages to three decimals. The withheld value was recoverable
  exactly, and — through the shared denominator every dot divides by — recoverable
  from *other* rows' dots as well.

  Publication granularity now equals plotting granularity, so the two channels cannot
  disagree: every dot on the track has its own number printed beside it.

  **Three states per side, and the third is new.** `n/a` means the side has no data;
  `—` means it has data that is below the ranking floor and is being withheld;
  digits mean it is ranked. Collapsing the first two would tell a reader we are
  holding back a number that does not exist. The withholding reaches assistive tech
  as off-screen text that **names which period** it applies to — per-side withholding
  is asymmetric — rather than an `aria-label` (inert on a `role="generic"` element),
  and it reuses the existing `below ranking floor` vocabulary.

  The label, track and tag stay, so the row keeps its shape. The value column's grid
  track is now a fixed width rather than content-sized: withholding changes what that
  column contains, and a content-sized track made a *withheld* row's chart area wider,
  so the same TIER landed at a different horizontal position than on the row above it.
  The dumbbells are sold as a shared scale; a per-row track quietly made them several.

  **`#136` is untouched.** The stored number is unchanged and still on the wire; what
  is revoked is its display authority.

  **Scope is the compare view, at BOTH grains — team rows and developer rows.** The
  developer grain is what the second half of the ruling settled: on a solo-developer
  install, the common self-hosted case, that single row *is* the org total, so a card
  withholding the headline above a row printing it is the same contradiction one
  grain down.

  - The main panel's per-developer bars and the KPI tile are governed by **#502/#603**
    and are deliberately unchanged — #502 keeps the measured inputs on screen and
    suppresses only the ratio, so widening this ruling into them would collide with it.
  - Row **order** is alphabetical, at both grains, and is now pinned by tests. This is
    not cosmetic: ordering rows by TIER would bound every withheld value between its
    two visible neighbours, and no change to what the view *prints* could close that.

- ⚠️ **The reserved unattributed sentinel can no longer be supplied as an `issue_id`**
  (#466). `issue_id` on `POST /api/v1/costs` and `POST /api/v1/outcomes` now
  returns `400` for the whole sentinel family — the bare `unattributed` plus any
  `unattributed:<reason>` sub-bucket — matched **case-insensitively** and after
  trimming whitespace, so `UNATTRIBUTED:main` is refused too. It was previously
  written silently. `POST /api/v1/events` is the deliberate exception: it is the
  JSONL collector's own transport and accepts the four exact canonical spellings,
  rejecting every case variant and near-miss.

  The split is not fussiness. `/events` validates all-or-nothing, the shipper treats
  `4xx` as terminal with no retry, and it is stateless — so applying the strict rule
  there would be permanent, total capture loss for anyone who has ever committed on
  `main` without an issue, including their well-formed attributed events.

  The proxy applies the same rule to the `X-Tier-Issue` header but treats a forged
  value as **missing** rather than as an error: it sits on the request path and must
  never fail a provider call over attribution metadata. Such a request is counted
  under a distinct `issue-forged` label on `tier_proxy_unattributed_total`, so
  forging stays distinguishable from omission in `/metrics`.

  No legitimate producer sets the sentinel as an issue id on these HTTP surfaces — the
  GitHub webhook derives ids via `issueref`, which can only emit `#[1-9]\d*` /
  `ABC-123` shapes. The org pollers (`anthropicadmin`, `openaiusage`) do assign the bare
  sentinel for aggregates they cannot split per developer, but they write in-process via
  `collector.Ingester`, never over the API.

  ⚠️ **Scope, stated precisely: this covers `issue_id` only.** The `developer` half was
  closed separately, by #619 below.

- ⚠️ **The reserved unattributed sentinel can no longer be supplied as a `developer`
  either** (#619). This is the half #466 deliberately left open, and it is the worse
  half — the only one that pays.

  TIER is `points / (cost/1000)`. Forging `issue_id` moves a dollar *between buckets
  inside the forger's own denominator* and leaves the headline score unchanged. Forging
  `developer` moves it *out of that denominator entirely*, onto the `unattributed`
  pseudo-developer: the forger's cost falls and their score rises. It was also
  user-visible — `segment_reconciliation.developers[]` emitted a row whose `developer`
  was literally `"unattributed"`.

  `developer` now returns `400` for the whole sentinel family on `POST /api/v1/costs`,
  `/events`, `/outcomes` and `/actual_spend`, matched **case-insensitively** and after
  trimming whitespace, exactly as `issue_id` is. `POST /api/v1/developer_alias` applies
  the same rule to **both** `alias` and `canonical`: an alias is a *retroactive* rename
  of the identity space — the score join resolves stored developers through it before
  aggregating — so without that guard the other four are bypassable in one hop.
  `{"alias": "alice", "canonical": "unattributed"}` would fold alice's whole history
  into the pseudo-developer; `{"alias": "unattributed", "canonical": "bob"}` would dump
  every org-poller aggregate into bob's denominator instead.

  **Unlike `issue_id`, there is no allowlist anywhere — including `/events`.** That
  asymmetry is the whole per-producer analysis #466 deferred, and it comes out the
  other way: the `/events` allowlist exists because the JSONL collector legitimately
  ships the sentinel *family* as an `issue_id` on every exploratory session, whereas
  nothing legitimately ships it as a `developer`. The two producers that assign it —
  the `anthropicadmin` and `openaiusage` org pollers, for org invoice aggregates that
  cannot honestly be split per person — write in-process via `collector.Ingester` and
  never cross an HTTP boundary; their sources are not shippable over `/events` in any
  case. The proxy's own missing-header fallback is likewise server-side. So the strict
  rule costs no capture, and an allowlist would only have made forging as effective as
  honesty.

  The proxy applies the same rule to the `X-Tier-Developer` header and, like the
  `X-Tier-Issue` half, treats a forged value as **missing** rather than as an error —
  it sits on the request path and must never fail a provider call over attribution
  metadata. Such a request is counted under a distinct `developer-forged` label on
  `tier_proxy_unattributed_total`. **This is the label to alert on:** it is the only
  one of the five that indicates a client raising its own score.

  Ordinary identities are unaffected, including `unknown` — the real no-identity
  fallback `collector.OSUsername()` emits when a container has no `/etc/passwd`
  entry — and names that merely contain the word (`unattributed-bot`,
  `not-unattributed`). `tierd ship --developer unattributed` will now fail its batch;
  that invocation was always a forgery.

  ⚠️ **What this does NOT do, stated plainly.** TIER is single-tenant with one shared
  write token and a free-form `developer` column, so a client that wants its spend out
  of its own denominator can still post `"developer": "mallory-2"` and get the same
  arithmetic effect. #619 removes the *deniable* forgery — asserting the server's own
  sentinel, which is indistinguishable from honest server-assigned spend and lands in
  a bucket nobody audits — but it does **not** mean a forger's score can no longer be
  raised. Binding `developer` to the credential that authenticated the write is the
  control that would, and it is separate work.

- **An empty repeatable flag value is now rejected instead of silently
  discarded.** Passing an empty string to `--map` (`repair-repo`), `--watch-repo`
  or `--trusted-proxy-cidr` (`serve`), or `--repo` / `--repo-slug` (`ship`) now
  fails with a message naming the flag. It previously vanished with no output and
  no non-zero exit, so a shell that ate a quote — or an unset variable in
  `--map "$SESSION=$SLUG"` — could shrink a repair mapping and the run would
  report the smaller result as a complete one.

  ⚠️ **This reaches `tierd serve` startup.** `watch.repos` and
  `http.trusted_proxy_cidrs` are fed from the config file through the same flag
  type, so a list containing an **explicit** empty string (`- ""`, typically a
  rendered template whose variable came out empty) now refuses to start the
  server rather than silently watching one fewer repository. A bare `-` is
  unaffected — the YAML decoder drops null sequence elements before they reach
  the flag.

### Fixed

- **The k-anonymity floor now covers the residual bucket and the response total**
  (#593). `AggregateTeamsKAnon` applied the floor only to named groups, so the
  `"other"` bucket and `total` were published unfloored — a cohort below the floor
  could be recovered by subtracting the named rows from the total.
- **`?work_type=` no longer bypasses the k-anonymity escalation** (#466). The filter
  ran before the floor, so a caller could narrow a window until a single developer
  remained and read their spend directly. This made recovery exact rather than
  approximate; measured on a fixture, a suppressed developer's spend returned to the
  cent.
- **`--text-faint` meets WCAG AA contrast in both themes** (#534). The dashboard's
  faintest text tier failed AA at 2.94:1 (dark) and 2.39:1 (light) across seven text
  roles, including the provenance stamp naming the rubric and price table behind every
  number on the page. Five non-text uses of the same token also failed their 3.0:1 bar.

- **Work-type segments silently excluded spend that produced no outcome** (#466).
  The pooled headline score is survivorship-free — `DeveloperCostsWindow` sums every
  token event with no join to outcomes, so spend on never-shipped work stays in the
  denominator and correctly lowers the score. The segmentation path did not: it
  summed cost only over outcome-linked issues, so that spend appeared in **no**
  segment and every per-type TIER read systematically better than the headline.

  Invisible and backwards. A team that thrashes sees the damage in the headline
  number, goes to the segment view for the cause, and finds the evidence removed.
  A caveat in the docs would not have fixed it; only a number that adds up does, so
  the fix is the `segment_reconciliation` block above rather than a label.

- 🔴 **The same dollar could be reported as two different, non-additive things in
  one response** (#466). `store.IsUnattributed` matched the sentinel family with
  case-**sensitive** `HasPrefix`, while the SQL family match used `LIKE` — which
  SQLite evaluates case-**insensitively**. A forged `issue_id` of
  `UNATTRIBUTED:main` was therefore *unattributed* to SQL and *spend on a real
  issue* to Go: `cost_composition` called it exploration and the reconciliation
  called it abandoned real-issue work, simultaneously, against a named developer.
  Reachable by any client via the proxy's `X-Tier-Issue` header or `POST /costs`.

  The SQL side is now `GLOB` (case-sensitive) rather than Go being loosened —
  case-blind sentinel matching was itself the accident — and both engines are pinned
  to agree by a guard that builds its SQL from the *same predicate constant* the
  production queries embed. Sharing only the CONSTANTS is how the drift survived its
  first guard: the constants matched while the operator diverged.

- **The reconciliation's overflow tripwire could never fire** (#466). It clamped each
  accumulator at `math.MaxInt64` and then tested the FINAL sums for negativity — but
  clamping is precisely what keeps them positive, so the check was unreachable by
  construction while the block published ~$9.2e12 as if it were a measurement, with
  the partition invariant intact and every figure non-negative. Saturation is now
  reported at the moment it happens and latched. The same helper also missed
  `MinInt64 + MinInt64`, which wraps to exactly `0` and so slipped past a `sum > 0`
  underflow test.

- **Every team row on the dashboard rendered as ranked evidence, whatever it cost**
  (#603). `dashboard.js` hardcoded `isRanked = teamMode ? true : !!d.ranked`, so a
  team with two outcomes and $0.30 of measured spend drew the same green bar, at
  the same authority, as one with months of evidence behind it. The row's own
  `ranked` value is now honoured in both modes, and a missing field reads as
  unranked — a server that does not vouch for a row never gets the green. The
  ranking-floor divider stays developer-only, because it marks a boundary in a
  ranked-first ordering that team rows (server-ordered) do not have.

- **The team rollup never carried the evidence floor, so an org could headline a
  yield built on a rounding error** (#502). `RollupTeam` computed TIER but never
  set a `Ranked` field — it had none — so the #133/#136 floor that governs every
  developer row stopped at the aggregate. An org that merged 28 weighted points
  against $0.0001 of measured spend published **TIER 280,000,000** as its headline
  number, with full ranking authority.

  `TeamScore.Ranked` now applies exactly the developer rule to the **summed**
  inputs: total outcomes ≥ 3, total spend ≥ $5.00, and no zero-token outcome
  anywhere in the team. No new constants — a team-only floor would drift against
  the developer one, and the quantity being gated is the same one. It is summed
  rather than ANDed over members: three developers with one outcome and $2 each
  are each below the floor, but the team number is computed from their sums, and
  the sums are the evidence standing behind it. `ranked` is now on the wire for
  every group aggregate (additive; **not** `omitempty`, because a `false` is the
  load-bearing value). No team-level `sample_n` accompanies it — that count is the
  denominator that would make `data_quality.attributed_outcome_share` invertible in
  the anonymized modes.

  ⚠️ **The arithmetic is untouched, deliberately.** TIER is still
  `points / (cost/1000)` at every site, and the below-floor aggregate above still
  ships `tier: 2.8e8` on the wire. This is the house rule from #136: *the number is
  never altered, only its ranking authority revoked.* Flooring the denominator was
  considered and rejected — it breaks the documented dual `CostPerPointCI =
  1000/TIER` against an unfloored `CostPerPoint`, and it flattens every org in the
  $0–$5 band onto one plateau of fabricated numbers that render exactly like real
  measurements. Consumers must gate the headline on `ranked`; they must not expect
  a scrubbed number.

  The org KPI tile therefore **withholds the ratio** below the floor rather than
  dimming it. The distinction is the whole point: the per-row bars mute a
  below-floor number and print it anyway, which works inside a ranked list, but a
  KPI tile is read alone and quoted onward — a muted `280,000,000.0` is still a
  published `280,000,000.0`. The tile shows `NOT ENOUGH SPEND TO SCORE` in the same
  faint treatment as `NO SCORE` (#500) and `FREE` (#499) — one badge vocabulary for
  "there is no headline number here", never a second — with a **distinct cause
  line**, because the reader's next action differs: "no accepted outcomes" means
  nothing shipped; this means the meter is not reading. The measured inputs stay on
  screen (points and spend in the caption, spend in its own tile, every row still
  listed), since hiding those would be its own dishonesty. Where the client cannot
  verify which floor was missed it names the remaining possibilities instead of
  asserting one — a fabricated cause is worse than a vague one.

  Seven smaller corrections travel with it, all found in review of the above:

  - The cause line's **"Check token capture."** now appears only on the
    below-spend-floor arm. On the other arm the org has cleared $5.00 of measured,
    captured spend and is unranked only because fewer than three outcomes merged,
    so a $120 window with two merged PRs was sending the reader to debug the one
    part of the system demonstrably working.
  - The spend figure is **truncated to the cent and never printed as `$0.00`**,
    via a shared `usdText`. On the canonical $0.0001 window the evidence the reader
    is meant to weigh rendered as `$0.00` — invisible, and indistinguishable from
    the FREE band one gate above, where cost is genuinely zero and yield is
    unbounded. Sub-cent amounts now render `<$0.01`.
  - **The cause line can no longer contradict itself at the floor.** Rounding made
    $4.995 print as "$5.00 of measured AI spend — below the $5.00 evidence floor";
    truncation fixes that for every value a human would type, but *not* for a
    float64 sum landing a few ulps under the floor — `total_cost_usd` is a sum of
    micro-USD-exact values, and a sum is not on that grid (`2.918582 + 0.956618 +
    0.420133 + 0.704667` is `4.999999999999999`). A `usdUnder` helper renders such
    a value as `<$5.00`, making the property structural rather than a tolerance.
  - A **below-floor team row states its verdict in text**, not in colour alone
    (WCAG 2.1 SC 1.4.1). The explanatory `title` was gated on developer mode and
    the ranking-floor divider is developer-only, so once #603 made below-floor team
    rows reachable, muted colour was the row's only signal on every channel. The
    row now carries off-screen text (`.ybar-sr`, clipped rather than
    `display:none`) and the TIER reading a title, reusing the compare view's
    wording (`below ranking floor`).

    Deliberately *not* an `aria-label`: a `.ybar-row` is a bare `<div>`, and the
    accessible-name computation refuses to name a `role="generic"` element, so a
    label there is computed and discarded. Giving the row a naming role would also
    switch on the three labels added in #274, which were written for a row whose
    contents were assumed unreadable and now duplicate the value cell — those three
    being inert is a **separate, pre-existing finding** and needs its own issue.
  - **Neither row title asserts a cause it cannot verify.** `ranked` is a three-way
    conjunction, and the developer title said "insufficient sample to rank: 20
    outcomes / $500.00 cost" for a row held back by a single zero-token outcome —
    blaming the sample with both cleared numbers printed beside it. Developer rows
    carry all three inputs, so the title now names the conditions actually failing;
    team rows carry only spend, so they claim that cause only where it holds and
    otherwise name the remaining possibilities, exactly as the KPI cause line does.
  - **The org's measured spend has one rendering, not two.** `renderKPIs` prints
    `total_cost_usd` twice — the SPEND tile and the below-floor cause line — both
    on screen at once. The tile rounded while the caption truncated, so at $4.997
    the tile read `$5.00` beside a sentence asserting the spend was below the
    $5.00 floor, and on the canonical $0.0001 window the tile read `$0.00` in the
    larger typeface while the caption read `<$0.01`.

    Both now call **one function**, `spendTextFor`. Routing them through the same
    *formatter* was not enough: the caption also needed `usdUnder` (so it cannot
    print as having reached the floor named in the same sentence) and the tile did
    not have it, which reproduced the identical contradiction on a float64 sum a
    few ulps under $5.00 — a window `RollupTeam` can produce. Two call sites of one
    function cannot disagree.

    Every other USD **amount** on the page goes through `usdText` too — the
    cost-composition total, its per-model and per-bucket costs, the unattributed
    figure, the net-credit-balance sub-label, and the `$5.00` floor itself, which
    is printed in the same sentence as the spend it is compared against. Two
    renderings are deliberately left out, listed with their reasons in
    `TestDashboard_MoneyHasOneFormatter`: `cost_per_point` is a **rate**, not an
    amount (#239), and the compare view renders signed **deltas** whose semantics
    are under separate review (#605).
  - **A below-floor row's TIER never becomes a bar length.** The panel scale is
    computed over ranked rows only *and* an unranked row draws no proportional fill
    — excluding it from the scale alone was not enough, because `pct()` clamps up,
    so the withheld 2.8e8 came straight back as the longest bar on the panel. There
    is deliberately no all-rows fallback: one was tried and re-imported the same
    distortion inside an all-below-floor panel, which per-work-type splitting makes
    the common case in team mode. This predates #502/#603 — the scale never
    consulted `ranked` — but it is the same value on the same page.

- **`repoid.Canonical` was not idempotent, and the non-idempotence was
  reachable.** It trimmed `.git` and then `/` in a single pass, so one trailing
  slash defeated the `.git` strip entirely and `owner/repo.git/` canonicalized to
  `owner/repo.git` — a join key nothing in the capture path can emit, under which
  cost would never meet its outcomes. Silent, permanent, and not self-correcting.
  It now trims to a fixed point, `repair-repo` refuses to write a value that is
  not one, and a property test pins `Canonical(Canonical(x)) == Canonical(x)`.

- **Comments in `internal/store` claimed a transaction safety property that does
  not exist.** `modernc.org/sqlite` ignores `sql.TxOptions.Isolation` entirely, so
  every `BeginTx(…, sql.LevelSerializable)` in the store is a plain DEFERRED
  `BEGIN` and the isolation level is a no-op — while ~9 comments asserted it took
  the write lock up front. What actually provides in-process atomicity is
  `SetMaxOpenConns(1)`; cross-process the check-then-act degrades to an unretried
  `SQLITE_BUSY_SNAPSHOT` (517), which `busy_timeout` does not cover. The comments
  now say what is true, a `beginImmediate` helper does the promotion honestly, and
  `repair-repo` uses it. The remaining call sites are converted in #598, below.

- **Every check-then-act transaction in the store now takes the write lock up
  front** (#598). All nine sites promote honestly via `beginImmediate`; the two
  reachable from an HTTP request (`POST /api/v1/developer_alias`,
  `DELETE /api/v1/developer/{id}`) use the bounded variant, because the promote is
  not bounded by the request context and with `SetMaxOpenConns(1)` a 5s wait stalls
  every other in-flight request behind the single connection.

  **Those two endpoints now answer `503` with `Retry-After` when another writer
  holds the lock**, instead of `500 "store error"`. The condition is transient and
  retryable; `500` is neither, and it sent operators looking for a corrupt database
  after what was really a lost lock race. The 250ms cap they fail past is
  deliberately generous for the **serving** path, whose longest lock hold is
  `repair-repo` at ~3.46 µs/row (`BenchmarkRepairRepoCommit`, 5000 rows,
  `-benchtime 5x`, 17,289,550 ns/op, Apple M5 Max, measured 2026-08-04) — so 250ms
  rides out a repair of roughly 72,000 rows before a request-path writer gives up.
  It is **not** the longest hold in the tree: the `Open()`-time whole-table
  migrations (`migrateCostUSDToMicro`, `migrateActualSpendToMicro`,
  `recomputeKnownSourceCosts`) each rewrite an entire table inside ONE unbounded
  transaction. They are marker-gated to run once, but that once is the first
  upgrade of an already-populated database — every `token_events` row, not
  `repair-repo`'s repo-filtered subset — and it is precisely the window in which a
  `503` is the honest answer rather than a five-second stall.

- **The sanctioned `/costs` override and `reprice --commit` now take the write
  lock up front too** (#346), on the same split #598 drew — they were added after
  that sweep and so were never covered by it.

  `POST /api/v1/costs` with `override: true` is the only path in the project that
  **rewrites already-captured money** rather than appending to it, and it decides
  what to rewrite by reading the row first. It is reached from an HTTP handler, so
  it uses the **bounded** variant and **now answers `503` with `Retry-After`** when
  another writer holds the lock, joining the two endpoints above. On that `503`
  nothing was written and no audit row was recorded. A read-only or full database
  still answers `500`, not `503` — it will never clear on its own.

  `tierd reprice --commit` uses the **unbounded** variant instead: it has no HTTP
  caller, so a 250ms cap would protect no request and would merely fail an
  operator's history rewrite whenever a live `tierd serve` held the lock for a
  moment. Its **dry run** — the default — deliberately stays DEFERRED, because it
  writes nothing and must not contend with a live server. ⚠️ A committing reprice
  now holds the write lock across its whole scan, which makes it, not
  `repair-repo`, the longest lock hold reachable while serving.

- **`POST /api/v1/costs` now answers write-lock contention ONE way, not two**
  (#610). The `override: true` half took the bounded write lock (#346, above); the
  plain half did not, so the same URL called a lost race for the single write lock
  retryable or permanent depending on the `override` field — a fast `503` +
  `Retry-After` on one, a five-second block then `500 "store error"` on the other.
  No client can reasonably retry one and not the other. Both halves now use the
  same helper and the same 250ms cap, keyed and unkeyed alike.

  ⚠️ **This is a BREAKING status change on the busiest write endpoint, and it is
  wider than `500` -> `503`.** The wait also shrank from 5000ms to 250ms, so
  contention the endpoint previously **waited out and completed as a `201`** now
  returns `503` instead. Clients posting into a contended store see more failures
  than before — each fast, retryable, and carrying `Retry-After`. The endpoint no
  longer waits on the client's behalf, and that is the point: with one write
  connection, a request blocking for 5s stalls every other in-flight request behind
  it, so the old behaviour bought one client's `201` with everyone else's latency.
  A client with no retry on `/costs` is the one that regresses.

  On that `503` nothing was written. A read-only or full database still answers
  `500`, not `503` — a permanent condition must never be advertised as transient,
  and the site passes the classifier's verdict through rather than re-deciding it
  (`TestRequestPathWritersDoNotSellAPermanentFailureAsRetryable`).

- **Spend by a worktree-isolated agent can attribute to an issue again** (#490).
  The agent harness INVENTS the branch name for such an agent
  (`worktree-agent-<hex>`), so no human ever had the opportunity to name it
  `<prefix>/<issue>-slug` and its spend could never resolve — it landed in
  `unattributed:branch-without-issue` beside genuine naming sloppiness, making that
  bucket's remedy wrong for a large slice of its contents. A harness-named message
  now inherits the most recent preceding human-named branch in the session file.
  The match is anchored hex on purpose: an ordinary branch that merely mentions the
  words (`fix/512-worktree-agent-naming`) carries a real issue number, and matching
  it would discard that number and inherit someone else's issue.

  ⚠️ **FRESH PARSES ONLY — no stored row changes.** Attribution is decided at parse
  time and the ingest UPSERT never re-stamps it (`ON CONFLICT(idempotency_key) DO
  UPDATE SET` touches the five token counters and nothing else), so spend already
  ingested under `unattributed:branch-without-issue` stays there. Re-running
  `tierd score` over the same files will not move it. Retroactive re-attribution of
  stored rows is tracked as #489.

  ⚠️ **Attribution-coverage figures from before this change are not comparable to
  figures from after it.** Measured on this machine 2026-08-04 across 5,927 session
  files: of 8,368 spend-bearing messages on a harness-invented branch, **2,253**
  become newly resolvable to a real issue (41 distinct issues). Any coverage
  percentage quoted across that boundary is mixing two different definitions of the
  unattributed bucket. No percentage is stated here on purpose — one is only
  meaningful against a stated window, and cost and outcomes have different
  retention.

## [0.3.0] - 2026-08-02

The container could not report whether it was healthy. That is the headline.

### Added

- **`tierd healthcheck` — a container health probe that works on a distroless
  image** (#571). The runtime image is Chainguard Wolfi static: no shell, no
  `wget`, no `curl`. Every shell-form `HEALTHCHECK` and every `CMD curl …` form
  is therefore unavailable, so the image declared no `HEALTHCHECK` at all —
  `docker inspect` reported none, `docker ps` could only ever show a bare `Up`,
  and operators had to assert liveness externally. `tierd` is already in the
  image, so it is the one thing a probe can call. The Dockerfile now declares an
  exec-form `HEALTHCHECK`.

  It asserts **liveness only** — that the port is bound and the server answers.
  Deliberately not `tierd version` (which proves the binary runs, not that the
  server listens) and not `tierd doctor` (which wants a git repo and an
  attribution floor, and would report unhealthy for reasons unrelated to
  serving). A healthcheck that fails for the wrong reason is worse than none.

  It probes `/api/v1/livez`, which stays open for probes and reports the build
  version, so a passing probe also identifies *which* binary answered. It does
  **not** gate on `/healthz`, whose 503 reflects subsystem health — restarting a
  container does not fix a degraded capture path. `--path` selects it, but note
  that for the shipped image you must replace the whole `HEALTHCHECK`
  instruction — exec form does no shell expansion, so a flag cannot be injected
  into it; the Dockerfile carries the full override line. `TIER_HEALTHCHECK_ADDR` retargets the probe when the server binds a
  non-default address, since an exec-form `HEALTHCHECK` does no shell expansion.

  A 2xx whose response never *completes* **fails** — deadline blown, or the
  connection dropped mid-response. A handler wedged after writing its status
  line still emits 200, and treating that as healthy would let `docker ps`
  report healthy forever while no response ever finishes. (A legitimately empty
  body, such as a `204`, still passes: truncation reads as a clean EOF, so only
  a genuinely broken exchange fails.)

- **A CVE re-scan of published images, on a schedule** (#560). A published
  digest is immutable, so it goes from clean to critical with no commit and no
  signal. Pinning answers "same bytes?", attestation answers "was it gated?";
  neither answers "is it vulnerable today?".

  ⚠️ **Read where it runs before relying on it.** The scheduled workflow is
  guarded to the development repository and is an explicit **no-op here** — it
  is shipped for reference, not running against this repository. It scans this
  project's own published tip (`ghcr.io/tiermetric/tierd:latest`) only: older
  tags are not re-scanned, and it does **not** scan any image *you* publish.
  `make cve-rescan` runs the identical code locally against targets you
  configure, which is the form an adopter would actually use.

- **A scope assertion for the demo tunnel** (#540). The demo's accepted
  `cloudflared` risk is bounded by the tunnel routing exactly one hostname, and
  that bound is now asserted by `scripts/demo-tunnel-scope-gate.sh` rather than
  stated in prose. See `deploy/DEMO.md` → "Accepted risk".

## [0.2.1] - 2026-07-30

The honesty UI did not render in 0.2.0. That is the headline of this release.

### Fixed

- **🔴 Eight dashboard elements rendered their content and were never visible**
  (#516). Every one is a caveat surface: the provenance stamp naming the price
  table, the attribution-coverage warning, the trust strip, the unjoined-developer
  strip, the unattributed-spend breakdown, the per-developer detail card, and BOTH
  halves of the compare view. Each carried a stylesheet `display: none` and was
  revealed with `el.style.display = ''`, which drops the inline override and hands
  control straight back to the rule that says `none`. 0.2.0 therefore showed
  confident numbers with every hedge suppressed, which is the opposite of what
  this project is for. Reveals now assign an explicit box, and a guard derives the
  hidden-element set from the assets rather than a hand-maintained list.
- **The dashboard's first paint defaulted to a 90-day window** (#497), the
  configuration that reads roughly twice too high in the flattering direction on
  an installation whose cost capture began recently. Now 30 days.
- **A window starting before cost capture began is now stated, not silently
  priced** (#512). `data_quality` carries the cost horizon, an explicit `false`
  when the window is covered (so "checked and clean" stays distinguishable from
  "no signal"), and the earliest `since` that clears the warning. `tierd doctor`
  gained a cost-horizon check.
- **`cost_per_point` is now null rather than `0` for a zero-point row** (#472). A
  lower-is-better field serialised its "no accepted outcome" case as the best
  possible value.
- **`billing_mode` was discarded by the JSONL collector and both org pollers**
  (#525), so stored rows claimed a per-token basis they had not earned in a column
  both `/export` surfaces publish. Cost is unchanged; only the discarded mode is
  recovered. Forward-only — `tierd reprice` repairs existing rows.
- **A damaged Codex rollout log read as an idle session** (#526). Malformed lines
  are still tolerated (the logs are appended live), but the loss is now counted
  and reported instead of reaching only a log line.
- **`tierd ship` silently dropped the repository on every event** (#491). The
  shipper's wire payload carried no `repo` field, so cost forwarded to a central
  tierd was stored under the `unqualified` sentinel and could never be joined to
  that repository's outcomes. `--repo-slug` was parsed and validated and then
  discarded, despite its help text stating that omitting it means "your cost
  never joins your outcomes". Multi-repo installs were additionally exposed to
  issue-number collisions across repositories — the exact fusion the `repo`
  column exists to prevent. Measured on one real multi-repo installation: every
  shipped event was unqualified.

  **Forward-only.** `repo` is intentionally excluded from the token-event upsert
  so a repo-blind producer can never downgrade a row another producer already
  qualified. Re-shipping an existing window therefore collides on the
  idempotency key and leaves stored rows unqualified — already-captured history
  is NOT repaired by upgrading. A repair path is tracked separately (#493).

### Changed

- **The four data-quality banners are one framed band** (#520) carrying a
  collective line ("TIER ran 4 data-quality checks on this window…"), ordered by
  observed severity. Same caveats, nothing hidden. Mobile is deliberately a
  scroll-to-the-number experience: no caveat is folded to fit a viewport.
- **Packaging fixes that made 0.2.0 unbuildable from the published tree.** `tools/`
  is now shipped, so `make check` passes on a clean clone; the docs index no longer
  links a file the export does not carry; and the internal `CLAUDE.md` is replaced
  by the slim public variant as the publish runbook always intended.
- **`serve --codex-rollout` without `--watch-repo` now refuses to start** rather
  than warning and continuing with Codex capture silently disabled (#464). An
  explicit request that cannot capture anything is a misconfiguration, and a
  startup warning was not enough to stop an operator believing Codex spend was
  being recorded. The error names the remedy. Note the asymmetry that motivated
  the change: outcomes still arrive by webhook and backfill, so uncaptured spend
  inflates TIER rather than lowering it.

## [0.2.0] - 2026-07-23

First release after the initial public tag. Everything here is a first-run /
remote-access fix: v0.1.0 shipped a dashboard you could not reach from another
machine, and a `demo --db` that could delete a real database.

### Added
- `docs/quickstart.md`, served by the running binary at `/docs/quickstart` and
  linked from the README — verified command-by-command against the binary,
  including how to reach the dashboard from another machine.
- `tierd demo --addr 0.0.0.0:PORT` now works. The synthetic read-only demo is
  exempt from the non-loopback bind guard via a structural, flag-unreachable
  signal; `serve` on real data still refuses a non-loopback bind without a token.

### Fixed
- **`tierd demo --db <path>` could delete a real capture database.** The guard
  is now fail-closed: it enumerates every user table and refuses any database
  holding rows outside the demo seeder's own tables — including tables with no
  developer/org column, such as `webhook_payloads`.
- `tierd -version` (and `-help`) work; previously only the bare `version`
  subcommand did.
- Every subcommand's `-h` exits 0 instead of 1.
- `go install`ed binaries report the module version instead of `dev`.
- `tierd score` outside a git repository now names the remedy (`--repo <path>`),
  and its closing tip points at the correct full-score path (`backfill`, then
  `serve`).

### Changed
- **`serve --codex-rollout` with no `--watch-repo` now fails at startup** rather
  than silently capturing nothing. `--read-only` warns instead of aborting.
- Documentation installs via `@latest` rather than a pinned tag, so the
  quickstart always matches the newest published release.


## [0.1.0] - 2026-07-19

The first public release.

### Added
- Deterministic TIER scoring — outcome per $1,000 of list-price AI spend — with
  Coverage % and cost-per-point companion metrics.
- Zero-setup laptop mode (`tierd score`), server mode (`tierd serve`: dashboard,
  GitHub-webhook outcomes, reverse proxy, live JSONL watching), and history
  reconstruction (`tierd backfill` for outcomes, `tierd ship` for 90-day cost).
- `tierd doctor` install-fidelity checks and `GET /api/v1/fidelity`.
- Honesty-first presentation: sub-50%-coverage rows dimmed, windowing skew
  documented, no absolute good/bad band.
- Team/developer/division aggregation with a k-anonymity floor for team mode.

[0.4.1]: https://github.com/tiermetric/tier/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/tiermetric/tier/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/tiermetric/tier/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/tiermetric/tier/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/tiermetric/tier/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/tiermetric/tier/releases/tag/v0.1.0
