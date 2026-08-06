# Changelog

All notable changes to TIER are documented in this file. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project will
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) once it
reaches v1.

## [Unreleased]

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

[Unreleased]: https://github.com/tiermetric/tier/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/tiermetric/tier/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/tiermetric/tier/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/tiermetric/tier/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/tiermetric/tier/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/tiermetric/tier/releases/tag/v0.1.0
