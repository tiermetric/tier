# The reproducibility contract

TIER publishes figures that state what AI-assisted work cost. This page states
**exactly what can be re-derived from a published figure, and what cannot** — no
more.

> **The one-sentence version.** Every report can be bound to a *manifest* naming
> the price table, the rows and the binary it was computed from — plus, on a
> **scoped** report, the repo-blind rows the scope dropped; `tierd
> verify-report` then either reproduces it, or **names which of those inputs
> moved**. It does **not** replay history, and there are inputs it does not
> watch. Both limits are printed on every run, including the successful ones.

---

## 1. What a report is bound to

`GET /api/v1/report_manifest` returns a `tiermanifest1` document. It is the
receipt for a figure. On a developer-mode server with a token, pipe the header
in so the token stays out of `ps` (read scope, so the viewer token also works).
Save the output as `manifest.json` for §2:

```sh
printf 'Authorization: Bearer %s\n' "$(cat ~/.tier/api-token)" |
  curl --fail-with-body -sS -H @- \
    'http://127.0.0.1:8080/api/v1/report_manifest?since=2026-08-01&until=2026-08-20' > manifest.json
```

Illustrative output from an older version-9 table; identities and row data vary:

```console
$ cat manifest.json
{
  "manifest_schema": "tiermanifest1",
  "since": "2026-08-01T00:00:00Z",
  "until": "2026-08-20T00:00:00Z",
  "token_since": "2026-07-18T00:00:00Z",
  "aggregation": "developer",
  "price_table": {
    "version": 9,
    "effective_date": "2026-07-26",
    "table_hash": "tierpt1:7acfb82b76f518d25b81d64a1065e29208feaa3dc821e15c636dba4a517db29c",
    "file_hash": "sha256:96719948a59cd5b379baadbf6ed2fb451bb8a62349dc38cb5875d6ed94ef2094"
  },
  "rubric": { "version": 1 },
  "tool_version": "dev",
  "commit": "b105344d6c1f6fd306f0b64c36a96c32cf99a691",
  "watermarks": {
    "window":  { "max_token_event_id": 12, "token_event_count": 11,
                 "max_outcome_id": 12, "outcome_count": 10 },
    "ledgers": { "max_quality_history_id": 0, "quality_history_count": 0, "...": "..." }
  },
  "events_digest":   { "value": "tierdig1:e01f105557bb8182…", "rows": 11 },
  "outcomes_digest": { "value": "tierdig1:ea54f57d041fe2c3…", "rows": 10 }
}
```

Three kinds of identity are pinned — and, on a **scoped** report, one further
published figure that no identity can reach
([the exclusion](#the-exclusion--the-one-published-figure-no-digest-can-cover)).
⚠️ It is listed apart from the three deliberately: it is a **pin**, not an
identity, and an enumeration that quietly absorbs it would be the same defect
as the undercounted list §4 corrects.

### The price table — two hashes, and they answer different questions

`table_hash` (`tierpt1:`) is over the **resolved in-memory table**: every model,
every rate, after provider defaults have been applied. `file_hash` (`sha256:`) is
over the **raw source bytes**.

They differ deliberately. Provider default multipliers are baked in from *code*,
so the same YAML under two binaries can price differently — `table_hash` catches
that and `file_hash` cannot. Conversely a comment-only edit changes `file_hash`
and leaves `table_hash` equal, which is what lets source-URL comments stay
auditable without invalidating every prior report.

`tierd prices` lists what the local registry knows. This is a historical example
from an older version-9 table with 77 models; your registry entries will vary:

```console
$ tierd prices list --db ~/.tier/tier.db        # --db is the path you gave serve
price_table_registry version 9:
  table_hash     "tierpt1:7acfb82b76f518d25b81d64a1065e29208feaa3dc821e15c636dba4a517db29c"
  file_hash      "sha256:96719948a59cd5b379baadbf6ed2fb451bb8a62349dc38cb5875d6ed94ef2094"
  effective_date "2026-07-26"
  model_count    77
  source         "embedded"
  tool_version   "tierd v0.0.0-20260829045422-b105344d6c1f+dirty go1.26.6 darwin/arm64"
  first_seen     "2026-08-29T04:57:38Z"
note: a recorded identity is authoritative for rows written at or after its first_seen; rows
already stamped with that version beforehand are not covered by it.
```

⚠️ Read that closing note. A registry row is authoritative **from its `first_seen`
onward**; rows already stamped with that version before it was recorded are not
covered by it. `tierd prices audit` shows the append-only ledger of retired
identities — a retired row is **stamped, never deleted**.

> **`UNKNOWN` stays `UNKNOWN`.** A version that was served before the registry
> existed has no row and never gets one retroactively. TIER does not mint an
> identity for a table it never hashed.

### The rows — watermarks *and* digests, because one of them is weaker than it looks

`watermarks` pins `MAX(id)` and `COUNT(*)` for the window and for five
append-only audit ledgers. **A `MAX(id)`/`COUNT(*)` pair cannot see an in-place
`UPDATE`** — the row count does not move and neither does the maximum id.

`events_digest` and `outcomes_digest` (`tierdig1:`) close that: they are content
hashes over every row the report was computed from, covering each row's
figure-bearing columns and their evidence but not every column (`session_id`,
`idempotency_key` and `push_day` are among those left out; `tokenEventDigestRow`
and `outcomeDigestRow` in `internal/store/eventsdigest.go` list them all, with
the reason for each). §3 shows an
edit that every watermark reports as `UNCHANGED` and the digest catches.

⚠️ **The two digests use different windows, and that is not a bug.** Outcomes are
digested over `[since, until)`; token events over `[since − 14d, until)`, the
**attribution band**, because spend that precedes a merged outcome is attributed
forward to it. `token_since` in the manifest is that lower bound. If it
disagrees with the band the verifying binary derives (or cannot be parsed),
`verify-report` prints an `attribution band` line and marks `token_events`,
`events_digest` and `repo_scope_excluded`'s token leg `NOT CHECKED` rather than
comparing pins taken over a different row set. A band difference never makes
those band-dependent pins read `CHANGED`; the band-independent ones, including
`repo_scope_excluded`'s outcome leg, are still compared and can. Attached
results are re-run over *this* binary's band, so they may still differ. And because the withheld pins are the ones that could
see a token-side change, a run where any of them was withheld and nothing moved
exits `2` (COULD NOT CHECK), never `0`; anything that did move still exits `1`.

🔑 **A `?repo=`-scoped manifest gets a *scoped* digest** — it covers only the rows
that report read. Without that, another team's ingestion would move your digest
and `verify-report` would tell you your untouched report is unreproducible; *a
verifier that cries wolf on other people's writes is one people stop running.*
Three consequences worth knowing:

- The **predicate is hashed in**, so a scoped digest and a fleet-wide digest are
  **different values even over identical rows**. Never compare one against the
  other — a scope change is meant to read as a change.
- Scoping is **strict**, matching a scoped `/scores`: rows whose repository could
  not be determined (the reserved `unqualified` sentinel) are **excluded**, and
  the under-count that causes is disclosed in `/scores`' `data_quality` block as
  `repo_scope_excluded`.
  ✅ **That disclosure figure now has a pin of its own** — see
  [the exclusion pin](#the-exclusion--the-one-published-figure-no-digest-can-cover)
  below (#751). It used to be unattested: it is computed *from* the sentinel rows,
  which no scoped digest covers and which only a fleet-wide digest would — and a
  scoped manifest publishes no fleet-wide digest. Measured then: an in-place
  reprice of a repo-blind row moved `repo_scope_excluded.cost_usd` from `9` to
  `14` while the scoped digests **and** the scoped watermarks held still.
- `rows` is still the denominator that proves the digest was earned. A scope that
  matched nothing yields a well-formed digest over an empty set; `rows: 0` is the
  only thing that tells you so.

### The exclusion — the one published figure no digest can cover

`repo_scope_excluded` on a **scoped** manifest: `token_events`, `cost_micro` and
`outcomes` for the repo-blind rows the strict scope dropped (#751). The sample
above is fleet-wide and therefore carries no such key; a scoped one adds:

```json
  "repo": "acme/tier",
  "repo_scope_excluded": { "token_events": 2, "cost_micro": 13000000, "outcomes": 1 }
```

🔴 **Why a separate quantity rather than a wider digest.** It is not the only
published figure a manifest fails to cover — the **cost horizon** in §4 is
another, and is still open. It is the only one that no *scoped digest could ever*
cover, which is what the heading means and why the fix had to be a separate
quantity. A scoped `/scores`
*reads* the `unqualified` sentinel rows — it has to, in order to disclose the
under-count — but a scoped digest deliberately excludes them, because a digest
that covered them would attest rows no published score was computed from, and a
scoped *predicate* that reached them would attribute every repo-blind row in the
fleet to whichever repository you named. So the figure sat outside every identity
a scoped manifest shipped. This pin closes that without touching either
predicate.

Three things about its shape are deliberate, and each is the answer to a
tempting alternative:

- **It pins counts, not a digest over the sentinel rows.** A digest would attest
  every digested column of every repo-blind row — a strictly larger claim than the figure
  `/scores` published — so it would report `CHANGED` for an edit that moved no
  published number. On a tamper-evidence surface a false alarm is the one failure
  mode you cannot have. The residual is stated rather than hidden: an in-place
  edit to a sentinel row's non-cost, non-timestamp columns moves nothing here,
  and it moves no published figure either. `cost_micro` is a `SUM`, so a reprice
  *is* caught; `ts` moves a row in or out of the window, so that is caught too.
- **It appears on scoped manifests only, and on every one of them** — including
  when all three counts are `0`. A fleet-wide report excluded nothing (the
  sentinel rows are inside its own digests), so there is nothing to pin. But an
  *omit-when-clean* pin would be vacuous exactly when it matters most: a window
  clean at publish time into which a sentinel row later lands would read `NOT
  PINNED`, and an absent check that reads like a passing one is the defect the
  digests exist to close. On a manifest **this server emitted**, `repo` present ⟺
  this object present — an emitter rule, not a refusal: the verifier tolerates a
  hand-written file that breaks it and says `NOT CHECKED` rather than erroring.
- **Money is pinned in micro-dollars** where `/scores` publishes `cost_usd`. A
  pin is compared for *equality*, and float equality over a JSON-round-tripped
  dollar figure can manufacture a divergence.

⚠️ **`token_events` and `cost_micro` count the attribution band**
`[since − 14d, until)`, while `outcomes` counts `[since, until)` — the same
asymmetry `token_since` exists for, because a scope can suppress a repo-blind row
inside an outcome's look-back. Recompute it over the narrower window and you are
comparing two different populations.

⚠️ **No `manifest_schema` bump.** Adding a field is additive: an older consumer
ignores the key and every field it does read still means what it meant, and a
manifest written before #751 verifies rather than erroring.

**What `verify-report` prints, in full** — four outcomes and a silent fifth,
because "this manifest never pinned it" and "there is nothing here to pin" are
different facts:

| manifest | pin | line |
|---|---|---|
| fleet-wide | absent | **no line at all** — nothing was excluded, so there is no pin to miss |
| scoped | absent | `NOT PINNED` — a pre-#751 emitter, or a hand-written file |
| fleet-wide | present | `NOT CHECKED` — reported, never agreement; the figure corresponds to no served number |
| scoped | present, equal | `UNCHANGED` |
| scoped | present, differs | `CHANGED` — and the detail names *which* of the three moved |
| scoped | present, `token_since` disagrees | `NOT CHECKED` — the token leg was pinned over a different band; `CHANGED` only if the outcome count moved |

### The binary

`tool_version` and `commit`. See §4 for why a drift here is *reported* but does
not on its own count as a divergence.

---

## 2. Verifying a report

Save the manifest you fetched in §1 (`curl … > manifest.json`), then:

```console
$ tierd verify-report manifest.json --db ~/.tier/tier.db   # --db is the path you gave serve
```

**Exit codes — three for a live window, and the third is not a pass** (a sealed
month's manifest adds `3` and `4`, [below](#a-sealed-months-manifest-913)):

| code | meaning |
|---:|---|
| `0` | **REPRODUCED** — every pinned input is unchanged |
| `1` | **DIVERGED** — at least one pinned input moved, and the report names which |
| `2` | **COULD NOT CHECK** — the verification did not happen, or a pinned input was `NOT CHECKED` because of a differing attribution band, an unsupported digest scheme, or a digest contradicting its pinned row count, and nothing else moved |

⛔ **Never wrap this in a `make` target.** GNU make replaces a recipe's exit code,
so `1` and `2` collapse into the same number and the distinction the tool exists
to draw is destroyed. Invoke the binary.

`2` is deliberately not folded into `1`. *"We checked and it moved"* and *"we did
not check"* are different facts and an operator must act differently on each.

It never writes to the database you name. Every read runs against a `VACUUM INTO`
snapshot which is deleted afterwards, because opening the store directly would run
migrations — and one of those backfills `token_events.price_version`, **the exact
provenance column an audit is about**. An archived file is byte-identical after a
verification run.

### A sealed month's manifest (#913)

Once an operator has armed sealing (`tierd seal --arm YYYY-MM|earliest`, or
`seal_from`), in an anonymised mode `/api/v1/report_manifest?period=YYYY-MM`
serves a `tiersealedmanifest1` manifest for one sealed calendar month. `verify-report`
takes it the same way. A sealed month is **replayed from the bytes it was sealed
with, never recomputed**, so the checks are different from a live window's:

- the stored body's sha256 must equal the manifest's `body_digest`, and the row's
  own `body_digest` column;
- the stored row must carry the manifest's `period_start`, `period_end`,
  `sealed_at`, config (`aggregation`, `period_size`, `k`, `fold_rule`, `digest`),
  `tool_version` and `commit`;
- the stored fold inputs must refold to the stored body's `teams`, or to its
  `kanon_suppressed` when the residual was withheld. This is not the seal's own
  check, which compared its fold with the refold before it committed: this one
  compares the refold with the stored body's bytes, and covers `teams` and
  `kanon_suppressed` only. The body's `total` and other `data_quality` fields
  rest on the digest alone;
- a `/scores` body attached under `results.scores` must equal the stored body.

`verify-report` checks the database, not the `/scores` body you hold. To check
that body, save it verbatim and compare its sha256 with `body_digest`
(`shasum -a 256 scores.json`), or attach it under `results.scores`. Without an
attachment the `results` line reads NOT PINNED, and the report's LIMITS block
lists every NOT PINNED and NOT CHECKED line.

It also computes the month from today's rows under the month's sealed
aggregation level and k, not this install's current config, and prints whether
a seal now under that config would be identical. That line is information only: a
sealed month keeps its bytes when rows arrive late, so it never changes the
verdict.

It reads the same snapshot as a live verification and **never seals**. A month
this database has not sealed is reported, not sealed on the spot.

A sealed manifest has two outcomes a live one does not, so it has two more exit
codes. `0`, `1` and `2` keep their meanings above:

| code | meaning |
|---:|---|
| `3` | **NOT SEALED IN THIS DB** — this database holds no sealed row for the month; point `--db` at the database that served the manifest |
| `4` | **UNKNOWN FOLD RULE** — every compared field is unchanged, but the refold was not run: either the month was sealed by a fold rule other than this binary's, or the row's `aggregation`, `period_size` or `k` no longer match its own config digest. Only a `tierd` whose fold rule is the month's own refolds it; a later `tierd` with a higher fold rule also exits `4` |

A difference in any field is `1`, and the report names the field; `1` wins over
`4`. The report prints the version and commit of the `tierd` that ran it. The install
markers the manifest also carries (`next_seal_at`, `earliest_period`,
`latest_period`, `config_gap`, and `stalled_period`, `stall_reason` and
`owed_since` while sealing is stalled) describe the install when it was served, not the
month, and are not checked.

### The default window — and why a manifest can still be refused

A manifest requested with **no `since` and no `until`** is re-runnable. That is
worth stating because it was not always true, and the reason is instructive.

The manifest publishes RFC3339 **instants**; `/api/v1/scores` accepts only
whole-day bounds. Until **#746** the default window was `now − 90d` *carrying the
request's time of day*, so no `?since=` value reproduced it and the verifier
refused:

```console
$ tierd verify-report default-manifest.json --db tier.db ; echo "EXIT=$?"   # BEFORE #746
verify-report: "since: \"2026-05-31T05:00:58Z\" is not midnight UTC, and /api/v1/scores
accepts only whole-day bounds (YYYY-MM-DD, YYYY-MM, YYYY) — so this report CANNOT be
re-run through the serving path. Truncating the bound would recompute a different window
and compare its numbers against this manifest's. Re-request the manifest with an explicit
?since= (and ?until=) on a date boundary" (NOTHING was verified)
EXIT=2
```

🔑 **The fix was in the SERVER, not the verifier.** The default lower bound is now
snapped BACKWARD to the start of its UTC day, so the manifest publishes
`2026-05-31T00:00:00Z` and `?since=2026-05-31` reproduces exactly that window.

⚠️ **"Re-runnable" is not "returns 0".** A default manifest has an OPEN upper
bound, so re-running it recomputes over `[since, ∞)` **as the database stands
now** — ingest anything after the manifest was taken and the honest answer is
`1` (DIVERGED), naming what moved. That is true of any open-ended manifest and is
the tool working, not failing. What #746 changed is that the run HAPPENS.

⛔ **The refusal above was not deleted, and must not be.** Truncating a mid-day
bound would recompute a *different* window and compare its numbers against this
manifest's — a green verdict for a report nobody ran. A hand-written manifest, or
one emitted by a pre-#746 build, still carries a non-midnight instant and still
exits `2`. That arm is asserted directly:
`TestVerifyReport_DefaultWindowIsReplayable` re-runs its own fixture with
`since: "2026-05-31T05:00:58Z"` hand-written in and requires exit `2`, so the
distinction cannot be quietly optimised away.

⚠️ **One sharp edge the snap introduces.** Building a verifiable report takes two
requests (`/report_manifest`, then `/scores`), and each resolves the default
independently. Their bounds are now identical all day — but differ by a **full
24 hours** if a UTC midnight falls between the two calls. Take both on the same
UTC day, or pass explicit bounds.

---

## 3. What a divergence looks like

A verifier that says only *"different"* is close to useless. Late ingestion, an
audited reprice, a repo repair, a quality revision and a GDPR erasure are all
**legitimate** — and without attribution each is indistinguishable from
corruption.

Below is output captured from a local `dev` build against a sample manifest and
database. One row's `billing_mode` was edited in place. No row was added or removed:

```console
$ bin/engine-pass/tierd verify-report bin/engine-pass/m.json --db bin/engine-pass/tier.db ; echo "EXIT=$?"
verify-report: "tiermanifest1"  window "2026-08-01T00:00:00Z" .. "2026-09-01T00:00:00Z"  scope fleet-wide  aggregation "developer" k=(none)

FAIL: a pinned INPUT moved. ⚠ The manifest pinned no results, so whether the
      published numbers moved with it is UNKNOWN to this run — see LIMITS.

  price_table:          UNCHANGED   tierpt1:3a27fb89…, version 12, effective "2026-10-02"
  rubric:               UNCHANGED   version 1
  token_events:         UNCHANGED   no rows above watermark 4, and the counted population still holds 3 rows at or below it, 3 pinned (count only)
  outcomes:             UNCHANGED   no rows above watermark 3, and the counted population still holds 3 rows at or below it, 3 pinned (count only)
  quality revisions:    UNCHANGED   none since quality_history id 0
  reprice:              UNCHANGED   none touching this window since reprice_row_audit id 0
  cost corrections:     UNCHANGED   none touching this window since cost_correction_audit id 0
  repo repairs:         UNCHANGED   none touching this window since repo_repair_row_audit id 0
  push reconciliations: UNCHANGED   none touching this window since push_outcome_audit id 0
  events_digest:        CHANGED     "tierdig1:d6519360…" (3 rows) -> tierdig1:59fb9287… (3 rows): the CONTENTS of the rows this report was computed over have changed. The row COUNT is unchanged, so this is most likely an in-place edit, or compensating changes — see the watermark lines. ⚠ Check the tool_version line first: an Open()-time migration on a newer binary rewrites rows in place and would move this digest with no edit having been made
  outcomes_digest:      UNCHANGED   tierdig1:23124fdc… over 3 row(s) — every digested column of every row is unchanged as the digest reads it; columns outside the digest were not compared
  tool_version:         UNCHANGED   "dev"

LIMITS
  - This run RECOMPUTED the report over the manifest's window against the database
    AS IT IS NOW. It did NOT replay the historical row population — as-of bounded
    reads (#717) are not in this build — so it ATTRIBUTES a difference to a named
    input; it does not reconstruct the manifest's original numbers.
  - Every read above ran against a VACUUM INTO SNAPSHOT, now deleted. The database
    you named was never opened for writing and never migrated, so an archived file
    is byte-identical after this run. The snapshot needs free space alongside it.
  - COMPENSATING CHANGES are the case attribution cannot resolve. A repair that moved
    $X out of this window and a late ingest that brought $X in are reported as two
    separate lines; whether they cancel HERE cannot be answered without replaying
    the window as it stood (#717).
  - This manifest pinned NO results, so NO published number was compared. Only the
    inputs above were checked.
EXIT=1
```

⭐ **Every watermark reads `UNCHANGED` and the report still diverges.** That is
the case the digests exist for.

---

## 4. What this does **not** establish

The limits that apply to a given run are printed by the tool itself, in a
`LIMITS` block, on **every run including the successful ones** — a limitation an
operator sees only on failure is one they will forget on success. ⚠️ *That is the
run's limits, not this whole section:* `printVerifyLimits` emits three fixed
bullets, a fourth when the manifest pinned no `results`, and one line per
`NOT PINNED` / `NOT CHECKED` dimension. The uncovered-inputs table and the
cost-horizon note below are documented here and nowhere else.

### It attributes; it does not replay history

`verify-report` recomputes the report **against the database as it is now**. When
nothing moved, that recomputation *is* the original computation over the same
rows, so `REPRODUCED` is a true statement. When something moved, it names the
input — it does **not** reconstruct the manifest's original numbers.

### Compensating changes cannot be resolved

A repair that moved $200 out of a window and a late ingest that brought $200 in
are reported as **two separate lines**. Whether they cancel *in this window*
cannot be answered without replaying the window as it stood.

That replay is **not planned**, and the reason is worth stating: it would require
append-only transition ledgers for `developer_alias`, `org_hierarchy` and
`period_membership` first. Those tables are mutated in place today, so an
"as-of" bound reconstructs nothing about them, and a replay built on them would
be byte-exact *only if they happened not to have changed* — a premise the system
cannot check. A conditional replay presented as a replay would be worse than no
replay at all.

### Five report inputs are not watched

| input | why it is uncovered |
|---|---|
| `developer_alias` | upserted/deleted in place; **re-keys the entire cost-to-outcome join**, and an aliased identifier's team rows are dated from the write (#914) |
| `hierarchy_membership` | dated and append-only (#886), so a write cannot move a window that ended before it; not watermarked, so a window still open at the write can move unseen |
| `period_membership` | `UPDATE`d in place; selects the cohort in aggregation |
| `actual_spend` | feeds per-developer Spend Leverage on a fleet-wide read |
| `Open()`-time migrations | rewrite `token_events` with no ledger row at all |

Two of the first four share one root cause: they are **identity-and-grouping
tables that this schema mutates in place and keeps no transition log for**.
Covering them is not two more `MAX(id)` reads — a `MAX` cannot see an in-place
`UPDATE` at all. `hierarchy_membership` is its own transition log, and only the
watermark is missing. `actual_spend` is not mutated in place either: its rows
are only inserted (`InsertActualSpend`), never updated, and are removed by
erasure.
Migrations are detectable only through the `tool_version`/`commit` stamp.

> The number is **five**, and it is stated because an earlier draft of the
> equivalent note in the source said *two*. An enumeration that undercounts is
> the same defect as a watermark that misses a ledger, one level up.

Also uncovered: `/api/v1/scores` embeds the installation's **global** cost
horizon (the earliest captured event in the whole database), so an event inserted
*outside* the window changes the response body while every window-scoped
watermark correctly holds still.

✅ **The scoped-exclusion gap that stood here is CLOSED (#751), and the entry is
kept as a pointer rather than deleted** so a reader arriving from an older copy
of this file is not left hunting for it. On a **scoped** report,
`data_quality.repo_scope_excluded` is computed from the `unqualified` sentinel
rows, which a scoped digest deliberately does not cover — measured, an in-place
reprice of a repo-blind row moved `repo_scope_excluded.cost_usd` from `9` to `14`
while the scoped digests and watermarks were byte-identical, and `verify-report`
reported `REPRODUCED` over changed served bytes. The manifest now pins that
figure in its own `repo_scope_excluded` object and `verify-report` recomputes and
compares it; the same probe is now `repo_scope_excluded: CHANGED` and `EXIT=1`.
See [The exclusion](#the-exclusion--the-one-published-figure-no-digest-can-cover).

⚠️ **The cost-horizon entry directly above is the same shape and is still open** —
a published figure drawn from rows outside the predicate the manifest pins. Do
not read #751's closure as closing that one; they were named together and only
one moved.

### `tool_version` drift is reported but does not diverge; `price_table` drift does

The guarantee is scoped to *"the same binary"*, and failing every cross-version
verification would kill the audit use case outright. The asymmetry is that **the
price stamp is inside the published body and the tool version is not**.

⚠️ A binary upgrade can move a digest with no edit having been made, because an
`Open()`-time migration rewrites rows in place. The tool says so on the `CHANGED`
line: check `tool_version` first.

### "Identical" means canonically identical, not byte-identical

A reproduced report is compared in **canonical form** — keys sorted, whitespace
normalised, numbers re-rendered. A re-indented or re-key-ordered manifest is the
same report, and failing it would be a fabricated divergence.

Where bit-identical output *is* claimed, it is scoped to **the same binary on the
same architecture**. Go may fuse a multiply-add on arm64, which is a 1-ULP
difference; TIER does not claim cross-architecture bit-identity.

### Neither shape that could not be verified is still open

⚠️ **This section listed TWO manifest shapes that could not be verified. Both are
now closed, by two different fixes, and the entry is kept as a pointer rather
than deleted so a reader arriving from an older copy of this file is not left
looking for it.** *(A third item, the scoped `repo_scope_excluded` gap, was a
different thing — an unattested FIGURE on a manifest that verified fine — and is
closed above by #751.)*

- **The *repo-scoped* (`?repo=`) manifest** carried no digests, because the digest
  reads had no repo predicate. They do now (**#747**) — see
  [The rows](#the-rows--watermarks-and-digests-because-one-of-them-is-weaker-than-it-looks).
  A scoped manifest publishes a **scoped** digest and verifies on it. The
  `digests_omitted` field remains, for the anonymized-mode withhold.
- **The DEFAULT-window manifest** (no `since`, no `until`) could not be re-run at
  all: it published an RFC3339 instant carrying the request's time of day, and
  `/api/v1/scores` accepts only whole-day bounds, so `verify-report` exited `2`.
  The default bound is now snapped back to the start of its UTC day (**#746**) —
  see [§2, The default window](#the-default-window--and-why-a-manifest-can-still-be-refused),
  which also keeps the `EXIT=2` transcript and explains why the refusal it came
  from is still live for hand-written and pre-#746 manifests.

⛔ **Closed is not the same as unlimited.** The live limits in this section are
the ones above it **except the two ✅ pointers** (#751, and the two manifest
shapes just above) — and note in particular the **cost horizon**: `/scores` embeds
the earliest captured event in the *whole* database, so an insert outside the
window changes the served bytes while every window-scoped pin correctly holds
still. That one is still open. *(This sentence named `repo_scope_excluded` until
#751 pinned it; do not restate that limit.)*

---

## 5. Determinism — why the same inputs give the same number

Reproducibility needs the computation to be order-independent, and float addition
is **not associative**. Three prerequisites make it so:

- every scoring read is totally ordered by `(ts, id)`;
- every float rollup is over a **sorted** slice — Go randomises map iteration per
  range, so four sites that summed over maps could return different totals for
  identical data;
- outcome timestamps are written in **UTC**. `modernc.org/sqlite` stores a
  `time.Time` as TEXT *including the zone suffix*, so `ORDER BY ts` is a binary
  string sort and mixed zones invert: `08:00 -0400` sorts before `10:00 +0000`
  despite being the later instant.

Money is unaffected either way — costs are integer micro-dollars, so those sums
are exact regardless of order.

---

## See also

- [api-compatibility.md](api-compatibility.md) — the `/api/v1` contract, including
  the manifest fields and what may change without notice.
- [pricing-philosophy.md](pricing-philosophy.md) — what the denominator measures,
  and why list prices rather than your invoice.
- [reference-price-table.md](reference-price-table.md) — the narrative companion
  to the embedded price table.
