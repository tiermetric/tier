package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tiermetric/tier/internal/repoid"

	_ "modernc.org/sqlite"
)

// schemaVersion is the schema end-state this binary's migration chain produces
// (#141). It is stamped into the SQLite header via `PRAGMA user_version` at the
// end of Open, and Open refuses to run against a database whose stored version
// is GREATER than this value (an older binary opening a DB a newer tierd
// migrated) — see Open. 0 is the reserved "unstamped" sentinel every pre-#141
// database and every freshly-created-but-not-yet-stamped file carries.
//
// Convention for bumping: increment this in the SAME PR as any schema change a
// binary at the current value would MISREAD — a column an old read path depends
// on being reshaped, a semantics change to an existing column, a unique-index
// reshape that changes dedup behavior. Do NOT bump for purely additive,
// ignorable changes (a new table or column an old binary simply never touches):
// those stay forward-compatible, so an old binary opening the newer DB must keep
// working rather than be locked out. The initial stamped version is 1: it
// establishes the refuse-if-newer mechanism against future BREAKING changes
// without locking out the prior release, since this PR adds no schema an old
// binary would misread (the stamp itself is header-only, not a table shape).
const schemaVersion = 1

// schemaTables creates all tables and the indexes that don't depend on columns
// added by later migrations. It must be safe to run repeatedly on any database.
const schemaTables = `
CREATE TABLE IF NOT EXISTS token_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    developer       TEXT    NOT NULL,
    issue_id        TEXT    NOT NULL,
    model           TEXT    NOT NULL,
    input_tok       INTEGER NOT NULL DEFAULT 0,
    output_tok      INTEGER NOT NULL DEFAULT 0,
    cache_read      INTEGER NOT NULL DEFAULT 0,
    -- cache writes are split by TTL bucket per Anthropic's pricing model
    -- (5m write = 1.25x input rate, 1h write = 2x input rate). Legacy DBs
    -- still carrying a flat cache_write column are migrated by
    -- migrateCacheWriteSplit, which backfills all legacy writes into the
    -- 5m bucket (matches Anthropic's pre-1h default).
    cache_write_5m  INTEGER NOT NULL DEFAULT 0,
    cache_write_1h  INTEGER NOT NULL DEFAULT 0,
    -- cost in integer micro-dollars (1 USD = 1,000,000), #69. INTEGER so SUM is
    -- exact and finance reconciliation is not subject to float accumulation
    -- error. Pre-#69 DBs carried this as cost_usd REAL and are converted by
    -- migrateCostUSDToMicro (Phase 2.65 in Open()).
    cost_micro      INTEGER NOT NULL,
    source          TEXT    NOT NULL,
    fidelity        TEXT    NOT NULL,
    -- #231: canonical "owner/repo" this cost was spent in, or the reserved
    -- 'unqualified' sentinel when the producer could not know it (the proxy sees
    -- only an HTTP request; pre-#231 rows never recorded it). NOT NULL with a
    -- convergent DEFAULT, mirroring weight_source='legacy': a NULL here would be
    -- DISTINCT-under-unique-index in SQLite and silently break dedup semantics.
    -- Canonicalized by internal/repoid so the webhook's repository.full_name and
    -- the collector's remote.origin.url land on the same byte string.
    repo            TEXT    NOT NULL DEFAULT 'unqualified',
    idempotency_key TEXT,
    -- #238: opaque upstream session identity (Claude Code sessionId), the
    -- grouping key behind the token-waste signatures — context bloat (per-message
    -- input_tok growing within one session) and rework loops (N sessions
    -- re-burning one issue). NULLABLE with no DEFAULT and no backfill: only the
    -- JSONL path knows a session, so proxy/poller rows and every pre-#238 row read
    -- back NULL — provenance-honest, exactly like fidelity. A fabricated id would
    -- be worse than an honest NULL (the repo/weight_source 'legacy' discipline).
    --
    -- Deliberately NOT part of any UNIQUE index (SQLite treats NULLs as DISTINCT,
    -- which would silently break dedup for exactly the session-blind rows), so the
    -- tenant-ordering constraint is untouched. A future grouping index for the
    -- cost-composition consumer (#234) would lead with tenant_id -> (tenant_id,
    -- session_id, ts) to keep that retrofit mechanical. Purely additive and
    -- ignorable to every current read path, so schemaVersion is NOT bumped (see
    -- the schemaVersion bump convention: an old binary opening this newer DB keeps
    -- working — it simply never selects the column — and must not be locked out).
    session_id      TEXT,
    -- #233: the price-table version that produced this row's cost_micro — the
    -- provenance stamp that makes cross-version pricing drift auditable. The
    -- server is the single pricing authority (/events re-prices raw token counts
    -- with ITS table), so this records which table version priced the (now
    -- immutable) cost. NOT NULL with a convergent DEFAULT 0 (the "unstamped"
    -- sentinel every pre-#233 row carries until backfillPriceVersion stamps it
    -- with the then-active version). Stamped on insert from the active table (see
    -- InsertTokenEvent) and, like repo/session_id, deliberately INSERT-only —
    -- absent from the ON CONFLICT DO UPDATE set so a replay never restamps it.
    -- Purely additive and ignorable to every current read path, so schemaVersion
    -- is NOT bumped (an old binary opening this newer DB never selects the column).
    price_version   INTEGER NOT NULL DEFAULT 0,
    -- #300: the SERVING HOST that priced this event, and how it bills. The cost of
    -- an open-weights model is a property of the host (OpenRouter vs Together vs
    -- Ollama vs self-hosted), not the weights, so these carry the pricing basis the
    -- flat 'model'-keyed rate used to collapse. host is 'unknown' (store.HostUnknown)
    -- on the first-party JSONL/poller paths and any host-blind producer; the proxy
    -- stamps its --target host. billing_mode is per_token | subscription |
    -- self_hosted_amortized, flagging a derived/approximate figure so it is never
    -- read as a canonical $/M. Both NOT NULL with convergent DEFAULTs (mirrors
    -- repo='unqualified'): a NULL would be DISTINCT-under-a-unique-index in SQLite,
    -- and though neither is indexed today the convergent default keeps fresh and
    -- migrated schemas identical. INSERT-only in the ON CONFLICT clause (like
    -- repo/session_id): a replay must never rewrite an established row's host. Purely
    -- additive and ignorable to every current read path, so schemaVersion is NOT
    -- bumped (an old binary opening this newer DB never selects these columns).
    host            TEXT    NOT NULL DEFAULT 'unknown',
    billing_mode    TEXT    NOT NULL DEFAULT 'per_token',
    ts              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS outcomes (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    developer         TEXT    NOT NULL,
    issue_id          TEXT    NOT NULL,
    pr_number         INTEGER,
    weight            REAL    NOT NULL,
    quality           REAL    NOT NULL DEFAULT 1.0,
    merge_commit_sha  TEXT,
    -- weight_source records WHICH scale produced weight (#132): 'label' from a
    -- GitHub size label, 'git-heuristic' from the diff-size fallback, 'legacy'
    -- for pre-#132 rows whose scale is unknowable (the old heuristic discarded
    -- its raw inputs, so no retroactive recompute is possible). The migration
    -- ADD COLUMN below carries the same DEFAULT for upgraded DBs — kept
    -- convergent here so fresh and migrated schemas match (mirrors cache_write_5m).
    weight_source     TEXT    NOT NULL DEFAULT 'legacy',
    -- raw diff stats from the PR payload, retained so a future recalibration
    -- CAN re-score new rows. NULL on legacy rows (never captured); populated on
    -- every insert going forward.
    additions         INTEGER,
    deletions         INTEGER,
    changed_files     INTEGER,
    -- source records the attribution PROVENANCE of the outcome (#188, extending
    -- the #34/#82 provenance discipline to outcomes): 'github-webhook' for a
    -- signature-verified GitHub delivery, 'api-outcome' for the bearer-gated
    -- provider-neutral POST /api/v1/outcomes any CI (GitLab/Bitbucket/Gitea) can
    -- call. Distinguishes a forged/manual outcome from a webhook-derived one in
    -- audit. DEFAULT 'github-webhook' is convergent with the migration ADD COLUMN
    -- below: the only pre-#188 outcome source was the GitHub webhook, so existing
    -- rows correctly read back as such.
    source            TEXT    NOT NULL DEFAULT 'github-webhook',
    -- push_day is the UTC calendar day ('YYYY-MM-DD') a push-captured outcome
    -- aggregates to (#196): all qualifying direct commits sharing an issue within
    -- one UTC day collapse into ONE outcome, keyed by (issue_id, push_day) via the
    -- partial unique index idx_outcomes_push_daily in schemaPostMigration. NULL on
    -- every non-push outcome (PR/api rows), so it stays outside that partial index
    -- and never constrains them. The migration ADD COLUMN below carries no DEFAULT
    -- (NULL), so fresh and upgraded schemas converge.
    push_day          TEXT,
    -- work_type is the work CATEGORY the outcome belongs to (#187), one of the
    -- FIXED taxonomy feature|bug|security|incident|tech-debt|research|compliance.
    -- It exists so security / SRE / on-call / research work is compared WITHIN its
    -- category, never against feature work (cross-type TIER comparison is a category
    -- error and is unsupported). DEFAULT 'feature' is convergent with the migration
    -- ADD COLUMN below: pre-#187 rows, push-captured rows, and any insert that
    -- resolves no type all read back 'feature', the honest baseline (mirrors the
    -- weight_source convergent-DEFAULT discipline). NOT a UNIQUE index column, so the
    -- tenant-ordering constraint on outcomes is untouched; the ?work_type filter and
    -- per-type segmentation partition the AllOutcomesSince result set in memory, so
    -- no per-type SQL index is needed (the scores read already scans every windowed
    -- row).
    work_type         TEXT    NOT NULL DEFAULT 'feature',
    -- work_type_source records the PROVENANCE of work_type (#187, #132 pattern):
    -- 'label' when derived from a PR label, 'api' when set explicitly via
    -- POST /api/v1/outcomes, 'default' when it fell back to 'feature' on a live
    -- insert (no label / no API value), and 'legacy' for pre-#187 rows the migration
    -- backfilled (their category is unknowable, exactly as weight_source 'legacy').
    -- DEFAULT 'legacy' is convergent with the migration ADD COLUMN below so fresh and
    -- upgraded schemas match (mirrors weight_source's 'legacy' default).
    work_type_source  TEXT    NOT NULL DEFAULT 'legacy',
    -- #231: canonical "owner/repo" this outcome was earned in, or the reserved
    -- 'unqualified' sentinel for pre-#231 rows and producers that cannot know it.
    -- Without it, repo A's issue #42 and repo B's issue #42 are ONE entity: the
    -- push-daily unique index silently drops one repo's outcome, a revert in A
    -- degrades B's quality, and token costs pool across both. Nearly every org
    -- but ours has many repos with overlapping low issue numbers.
    --
    -- NOT NULL with a convergent DEFAULT is mandatory, not stylistic: SQLite treats
    -- NULLs as DISTINCT inside a unique index, so a NULL repo in
    -- idx_outcomes_push_daily_repo would make every replayed push insert a fresh
    -- row and destroy the #196 idempotency guard.
    --
    -- No backfill: a pre-#231 row's repository is genuinely unknowable, and a
    -- fabricated slug is worse than an honest sentinel (the weight_source='legacy'
    -- rationale, verbatim).
    repo              TEXT    NOT NULL DEFAULT 'unqualified',
    ts                DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS org_hierarchy (
    developer   TEXT    PRIMARY KEY,
    team        TEXT    NOT NULL,
    division    TEXT,
    org         TEXT
);

-- period_membership records the billing-period window a developer belonged to an
-- org, so tier-2 seat counts reflect who was a member in the QUERIED period
-- rather than all-time org_hierarchy rows (#41). period_end NULL = still active;
-- a developer who left has period_end set to their last active YYYY-MM period and
-- stops diluting active members' org_total allocation for later periods. Seats
-- are counted over DISTINCT (developer, period) active tuples in the queries, so
-- duplicate membership rows for a developer collapse to one seat.
-- period_start='0000-01' is the "active since the beginning of time" sentinel
-- used to backfill pre-#41 org_hierarchy rows. The partial unique index below
-- enforces at most one OPEN (period_end IS NULL) row per (developer, org).
CREATE TABLE IF NOT EXISTS period_membership (
    developer    TEXT NOT NULL,
    org          TEXT NOT NULL,
    period_start TEXT NOT NULL CHECK (period_start GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]'),
    period_end   TEXT          CHECK (period_end IS NULL OR (period_end GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]' AND period_end >= period_start))
);

CREATE TABLE IF NOT EXISTS actual_spend (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    developer         TEXT    NOT NULL,
    period            TEXT    NOT NULL CHECK (period GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]'),
    -- actual paid in integer micro-dollars (#69); negative deltas allowed
    -- (credit memos / refunds, #24). Pre-#69 DBs carried actual_paid_usd REAL,
    -- converted by migrateActualSpendToMicro.
    actual_paid_micro INTEGER NOT NULL,
    ts                DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS org_actual_spend (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    org               TEXT    NOT NULL,
    period            TEXT    NOT NULL CHECK (period GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]'),
    actual_paid_micro INTEGER NOT NULL,
    -- source records WHICH feed produced this delta row (#138): 'anthropic-admin'
    -- for the Anthropic Admin cost-report poller, 'manual' for the provider-agnostic
    -- POST /api/v1/org_actual_spend endpoint (and any future provider poller under
    -- its own tag, e.g. 'openai-usage' for #139). Rows STORE per-source but the
    -- allocation read (the inv CTE) SUMs across sources, so multiple pollers +
    -- manual finance rows coexist and the org total is complete. A per-source poller
    -- reconciles its month-to-date figure ONLY against its own source's net, so it
    -- can never cannibalize another source's spend (#138 review R1). DEFAULT 'manual'
    -- is convergent with the migration ADD COLUMN below: the only pre-#138 writer was
    -- the manual POST endpoint, so existing rows correctly read back as 'manual'
    -- (mirrors the outcomes.source #188 default discipline).
    source            TEXT    NOT NULL DEFAULT 'manual',
    ts                DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- tier_migrations records which one-shot data migrations have been applied.
-- The schema migrations themselves are idempotent ALTER TABLE statements that
-- swallow "duplicate column"; this table exists only for migrations whose
-- WORK is expensive (e.g. recomputeKnownSourceCosts in #55, which scans the
-- entire token_events table) and that we want to skip on subsequent boots.
CREATE TABLE IF NOT EXISTS tier_migrations (
    name        TEXT PRIMARY KEY,
    applied_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- watcher_checkpoint persists the live watcher's per-file tail state (#71) so a
-- restart resumes from the last parsed byte offset instead of re-parsing every
-- JSONL from byte 0 (correct via dedup, but unbounded cost as logs grow). It is
-- a DERIVED cache: safe to drop and rebuild, and the idempotency_key dedup on
-- token_events backstops any stale or lost row. inode + head_crc/head_len are
-- the watcher's rotation/truncation guard; metadata is an opaque JSON blob owned
-- by the collector (session id, cwd, branch, and the parse-sequence counter that
-- keeps id-less message dedup keys stable on resume). PK is 'path' today; a
-- future tenant_id would LEAD the key — (tenant_id, path) — to keep the tenancy
-- retrofit mechanical.
CREATE TABLE IF NOT EXISTS watcher_checkpoint (
    path        TEXT PRIMARY KEY,
    inode       INTEGER NOT NULL,
    byte_offset INTEGER NOT NULL,
    head_crc    INTEGER NOT NULL,
    head_len    INTEGER NOT NULL,
    metadata    TEXT NOT NULL,
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- developer_alias maps a raw identifier as it appears on cost/outcome rows
-- (OS username, GitHub login) to the canonical developer identity used for
-- scoring. Resolution is SINGLE-HOP: a canonical value must never itself be
-- an alias (enforced by UpsertDeveloperAlias). Unmapped identifiers are their
-- own canonical. The unique index is single-column today; a future tenant_id
-- would LEAD it -- (tenant_id, alias) -- per the CLAUDE.md tenancy ordering
-- constraint, keeping the retrofit mechanical.
CREATE TABLE IF NOT EXISTS developer_alias (
    alias      TEXT NOT NULL,
    canonical  TEXT NOT NULL,
    ts         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_developer_alias_alias
    ON developer_alias(alias);

CREATE INDEX IF NOT EXISTS idx_token_events_issue     ON token_events(issue_id);
-- Keyset-pagination index for the bulk-export scan (ListTokenEvents, #191):
--   SELECT ... FROM token_events WHERE ts >= ? AND ts < ?
--     AND (ts > ? OR (ts = ? AND id > ?)) ORDER BY ts, id LIMIT ?
-- (ts, id) matches the export's total order, so the planner seeks straight to the
-- cursor position and walks in order — no full-window sort, no deep-offset scan.
-- id is the rowid, so the index physically stores (ts, rowid); listing it is
-- explicit intent, not redundancy. Distinct name from the #72-dropped
-- idx_token_events_ts (that single-column ts index served the developer-grouped
-- scores query and was superseded; this one serves a ts-ordered export the
-- developer-leading covering index cannot). Column order (ts, id) keeps the future
-- tenant retrofit mechanical: tenant_id LEADS -> (tenant_id, ts, id). ts and id
-- both exist at table creation, so this belongs in Phase 1.
CREATE INDEX IF NOT EXISTS idx_token_events_ts_id ON token_events(ts, id);
-- Serves the cost-horizon reads (#512), which run on EVERY /scores request:
-- SELECT DISTINCT source, and then the earliest ts per source. Without it both
-- degrade badly on the exact input the feature exists for. Measured on the real
-- 124k-row dogfood DB: DISTINCT was a full SCAN + temp B-tree (~10ms) and a
-- LATE-starting source walked the whole ts index with a heap lookup per row
-- (~26ms) before finding its first match — ~36ms added to every request, linear
-- in row count, on the endpoint the public demo serves. With this index both
-- become COVERING reads (~3ms and ~0ms): DISTINCT skip-scans the leading column
-- and the per-source minimum is a direct SEARCH ... (source=?) taking the first
-- row in ts order. Non-unique, so the tenant retrofit is a column-order note
-- rather than a key change: tenant_id LEADS -> (tenant_id, source, ts). source
-- and ts both exist at table creation, so this belongs in Phase 1.
CREATE INDEX IF NOT EXISTS idx_token_events_source_ts ON token_events(source, ts);
-- The scores covering index (idx_token_events_scores, #72) references cost_micro,
-- a column the #69 migration ADDs on upgrade — so it cannot live here in Phase 1
-- (it would fail on a pre-#69 DB whose token_events still has only cost_usd). It
-- is created in schemaPostMigration (Phase 3), after migrateCostUSDToMicro has
-- guaranteed cost_micro exists on both fresh and upgraded DBs. The supersession
-- DROPs of the old single-column indexes stay here: they are column-agnostic and
-- safe to run before the migration.
DROP INDEX IF EXISTS idx_token_events_ts;
DROP INDEX IF EXISTS idx_token_events_developer;
CREATE INDEX IF NOT EXISTS idx_outcomes_developer     ON outcomes(developer);
CREATE INDEX IF NOT EXISTS idx_outcomes_issue         ON outcomes(issue_id);
-- Keyset-pagination index for the bulk-export scan (ListOutcomes, #191); same
-- shape and rationale as idx_token_events_ts_id above. ts and id exist at table
-- creation, so it belongs here in Phase 1. Future tenant retrofit: tenant_id
-- LEADS -> (tenant_id, ts, id).
CREATE INDEX IF NOT EXISTS idx_outcomes_ts_id         ON outcomes(ts, id);
CREATE INDEX IF NOT EXISTS idx_org_hierarchy_org      ON org_hierarchy(org);
CREATE INDEX IF NOT EXISTS idx_period_membership_org  ON period_membership(org);
-- At most one OPEN membership per (developer, org): makes the UpsertHierarchy
-- NOT-EXISTS guard a structural invariant rather than a best-effort check (#41).
CREATE UNIQUE INDEX IF NOT EXISTS idx_period_membership_open
    ON period_membership(developer, org) WHERE period_end IS NULL;
-- Drop the pre-#24 unique indexes; under the credit-memo model multiple
-- rows can share (developer, period) or (org, period) and the SUM at query
-- time yields the net. DROP INDEX IF EXISTS is a no-op on fresh DBs and
-- additive on upgrades.
--
-- The replacement non-unique indexes are created in schemaPostMigration —
-- they MUST run AFTER the table-rebuild migration (Phase 2.5 in Open())
-- because the rebuild drops the table and would otherwise leave upgraded
-- DBs with no indexes at all.
DROP INDEX IF EXISTS idx_actual_spend_dev_period;
DROP INDEX IF EXISTS idx_org_actual_spend_org_period;

-- webhook_payloads retains the raw GitHub webhook body (gzipped) for every
-- PROCESSED delivery, so a TIER score input can be re-derived months later
-- (#137). The body is stored gzip(BestSpeed) — JSON up to 1 MB, typically a
-- ~30 KB PR payload compressing to ~5 KB. body_sha256 is the hex digest of the
-- RAW (pre-gzip) body: an integrity check and a dedup aid for ops tooling.
-- Retention is bounded (see PruneWebhookPayloads): 90-day age prune + a
-- 50,000-row cap, run at Open().
--
-- PII-AT-REST (#137): these raw bodies contain personal data — commit author
-- names and EMAIL ADDRESSES, PR titles/bodies, and commit messages — retained
-- for up to 90 days in this 0600-permissioned single-tenant DB (#130). This is a
-- new PII-at-rest surface introduced by #137; docs/privacy.md carries the
-- operator-facing disclosure (assigned to the #183 docs wave). The body is a
-- best-effort re-derivation convenience, NOT the evidence of record — see
-- PruneWebhookPayloads for the evictability / append-only-authority boundary.
CREATE TABLE IF NOT EXISTS webhook_payloads (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    event       TEXT    NOT NULL,           -- X-GitHub-Event
    delivery_id TEXT    NOT NULL DEFAULT '', -- X-GitHub-Delivery (empty for test clients)
    body_gz     BLOB    NOT NULL,           -- gzip(raw request body)
    body_sha256 TEXT    NOT NULL,           -- hex digest of the RAW body (integrity/dedup aid)
    received_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
-- Non-unique lookup index; (event, delivery_id) ordering chosen so a future
-- tenant_id can prepend: (tenant_id, event, delivery_id). NOT unique: GitHub
-- redeliveries legitimately reuse a GUID and we WANT both copies in the audit
-- trail (the processing dedup lives elsewhere, in the handler delivery set).
CREATE INDEX IF NOT EXISTS idx_webhook_payloads_event_delivery
    ON webhook_payloads(event, delivery_id);
CREATE INDEX IF NOT EXISTS idx_webhook_payloads_received ON webhook_payloads(received_at);

-- quality_events is the append-only signal log consumed by P2-03 (#134): each
-- row is one observed CI / revert signal against a specific outcome row. Quality
-- is DERIVED from this log (min of floors) rather than mutated in place, so it is
-- re-derivable and replay-safe. event_type is validated in Go (AppendQualityEvent
-- keeps the allowlist) rather than by a SQL CHECK, so adding Phase-2 event types
-- later is a code change, not a table rebuild.
CREATE TABLE IF NOT EXISTS quality_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    outcome_id   INTEGER NOT NULL,          -- outcomes.id (row-targeted, fixes C9 scoping)
    developer    TEXT    NOT NULL,          -- denormalised for audit reads
    issue_id     TEXT    NOT NULL,
    event_type   TEXT    NOT NULL,          -- ci_pass|ci_fail|ci_fail_flaky|revert_quality|revert_strategic (validated in Go)
    source_ref   TEXT    NOT NULL,          -- head_sha:attempt / revert commit sha
    event_ts     DATETIME NOT NULL,
    recorded_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
-- Idempotency for replayed webhooks. Column order (outcome_id, event_type,
-- source_ref) keeps the future tenant retrofit mechanical: tenant_id will
-- LEAD -> (tenant_id, outcome_id, event_type, source_ref). Its columns all
-- exist from table creation, so the unique index belongs here in Phase 1.
CREATE UNIQUE INDEX IF NOT EXISTS idx_quality_events_uq
    ON quality_events(outcome_id, event_type, source_ref);
-- Keyset-pagination index for the bulk export (ListQualityEvents, #242), same
-- (ts, id) shape and rationale as idx_token_events_ts_id / idx_outcomes_ts_id:
-- the planner seeks straight to the cursor and walks in order, no full-window
-- sort. The time column here is event_ts (the observed signal time the export
-- windows on), which AppendQualityEvent always writes in Go form. event_ts and
-- id exist from table creation, so this belongs in Phase 1. Future tenant
-- retrofit stays mechanical: tenant_id LEADS -> (tenant_id, event_ts, id).
CREATE INDEX IF NOT EXISTS idx_quality_events_ts_id
    ON quality_events(event_ts, id);

-- quality_history is the append-only transition log written on EVERY quality
-- write (#137). One row per affected outcome per mutation records the old and
-- new multiplier, so no quality change is ever silent and outcomes.quality is
-- re-derivable from the chain (quality == last new_quality, or 1.0 when empty).
-- reason is the driving event_type, or 'legacy-update-quality' for the pre-P2-03
-- issue-wide UpdateQuality path.
CREATE TABLE IF NOT EXISTS quality_history (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    outcome_id  INTEGER NOT NULL,
    developer   TEXT    NOT NULL,
    issue_id    TEXT    NOT NULL,
    old_quality REAL    NOT NULL,
    new_quality REAL    NOT NULL,
    reason      TEXT    NOT NULL,           -- event_type, or 'legacy-update-quality' for the old path
    source_ref  TEXT    NOT NULL DEFAULT '',
    ts          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_quality_history_outcome ON quality_history(outcome_id);
-- Keyset-pagination index for the bulk export (ListQualityHistory, #242), same
-- (ts, id) shape as the other export indexes. NOTE: quality_history.ts is written
-- by the SQLite DEFAULT CURRENT_TIMESTAMP (second-precision 'YYYY-MM-DD HH:MM:SS'),
-- NOT in Go form like every other table's ts — so ListQualityHistory binds its
-- window/cursor bounds as strings in that stored layout (a bound time.Time renders
-- as Go's "...+0000 UTC" text and would not lexically match). The comparison stays
-- plain string ordering, which this index serves directly. ts and id exist from
-- table creation (Phase 1). Future tenant retrofit: tenant_id LEADS -> (tenant_id, ts, id).
CREATE INDEX IF NOT EXISTS idx_quality_history_ts_id
    ON quality_history(ts, id);

-- reprice_audit is the append-only ledger of every tierd reprice operation
-- (#294). Repricing RETROACTIVELY rewrites historical token_events.cost_micro
-- (the TIER denominator), so it is the ONLY sanctioned mutator of a priced cost
-- (the normal insert path holds cost_micro immutable, #233) and every run MUST
-- leave a durable record of what it changed. One row is written PER distinct
-- old price_version affected by one operation: reprice_id groups the rows of a
-- single run, and (old_price_version -> new_price_version, row_count, the
-- before/after cost sums) captures how each version's cost shifted. Combined
-- with a tierd backup snapshot this makes a reprice reversible-in-principle:
-- the audit records WHICH versions moved and by how much, so the change can be
-- verified or re-derived. price_effective_date + tool_version stamp the table
-- and binary that produced the new figures (provenance).
--
-- schemaVersion is deliberately NOT bumped: this is a brand-new table that no
-- pre-#294 read path touches, so an old binary opening this newer DB simply
-- never selects it and keeps working (the schemaVersion bump convention). It
-- carries NO unique index: an audit ledger is purely append-only and needs no
-- uniqueness, which also sidesteps the tenant-ordering constraint (a future
-- tenant_id would LEAD any index later added here -- (tenant_id, reprice_id) --
-- per the CLAUDE.md tenancy ordering rule, keeping the retrofit mechanical).
CREATE TABLE IF NOT EXISTS reprice_audit (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    -- reprice_id groups every row written by ONE reprice operation (a random
    -- hex token minted per run), so a single run's per-version rows read back
    -- together and two runs never intermingle.
    reprice_id           TEXT    NOT NULL,
    -- from_version is the operator's --from-version N argument: the run repriced
    -- every token_events row with price_version >= N.
    from_version         INTEGER NOT NULL,
    -- old/new price_version for THIS group of rows. new_price_version is the
    -- active table version everything moved to; old == new is legitimate (a
    -- row already at the active version whose cost was reconciled, e.g. the
    -- placeholder-promotion edge in insertTokenEventSQL).
    old_price_version    INTEGER NOT NULL,
    new_price_version    INTEGER NOT NULL,
    row_count            INTEGER NOT NULL,
    -- SUM(cost_micro) across this group BEFORE and AFTER the reprice, in integer
    -- micro-dollars so the delta is exact.
    old_cost_micro_sum   INTEGER NOT NULL,
    new_cost_micro_sum   INTEGER NOT NULL,
    -- effective_date of the active price table that produced the new costs, and
    -- the tierd build that ran the operation -- provenance for "which prices /
    -- which binary rewrote this history".
    price_effective_date TEXT    NOT NULL,
    tool_version         TEXT    NOT NULL,
    ts                   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- reprice_row_audit is the per-ROW before-image ledger of every tierd reprice
-- (#294) -- the CANONICAL description of the mechanism; other reprice comments
-- point here. Where reprice_audit records per-old-version AGGREGATES (how each
-- version's summed cost shifted), this captures, for EACH token_events row a
-- reprice changed, that row's EXACT pre-reprice (cost_micro, price_version,
-- billing_mode) keyed by the same reprice_id. That is what makes a reprice
-- REVERSIBLE-IN-PRINCIPLE at row grain: a future inverse-undo (NOT yet built --
-- #294 ships the substrate, not the undo command) can restore each row to its
-- stored old values BY token_event_id, an inverse that survives continued
-- ingestion afterward (unlike a whole-file backup snapshot, which a later ingest
-- diverges from). Every before-image is written in the SAME transaction as the
-- row UPDATEs and the aggregate reprice_audit rows, so the whole reprice — cost
-- updates, aggregate ledger, and these per-row before-images — commits or rolls
-- back together (atomicity; a mid-run failure leaves no partial mutation and no
-- orphan before-image).
--
-- GROWTH: one row per CHANGED token_events row, so a large first-time reprice
-- writes a proportional burst (and holds the single-writer lock for its span --
-- run it in a maintenance window). Like reprice_audit it is append-only with no
-- pruning here; retention/GC of the audit ledgers is deferred (tracked with the
-- broader retention work, #141), not owned by this table.
--
-- schemaVersion is deliberately NOT bumped (same reasoning as reprice_audit and
-- the schemaVersion bump convention): a brand-new table that no pre-#294 read
-- path touches, so an old binary opening this newer DB simply never selects it
-- and keeps working. UNIQUE(reprice_id, token_event_id) both enforces exactly
-- one before-image per row per run AND honors the CLAUDE.md tenant-ordering rule
-- -- a future tenant_id would LEAD the key, (tenant_id, reprice_id,
-- token_event_id), keeping the eventual retrofit mechanical. No FOREIGN KEY to
-- token_events: an audit before-image must outlive the row it describes.
CREATE TABLE IF NOT EXISTS reprice_row_audit (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    -- reprice_id ties this before-image to the reprice_audit rows of the same
    -- run (the shared correlation token minted per operation).
    reprice_id        TEXT    NOT NULL,
    -- token_event_id is the token_events.id whose cost this run rewrote -- the
    -- key a surgical inverse restores by.
    token_event_id    INTEGER NOT NULL,
    -- the row's EXACT values BEFORE this reprice overwrote them.
    old_cost_micro    INTEGER NOT NULL,
    old_price_version INTEGER NOT NULL,
    old_billing_mode  TEXT    NOT NULL,
    ts                DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(reprice_id, token_event_id)
);
`

// schemaPostMigration creates indexes that depend on columns added by ALTER
// TABLE migrations OR on tables rebuilt during Phase 2.5. It runs after the
// migration steps so the column / table is guaranteed to be in its final
// shape before the index references it.
const schemaPostMigration = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_token_events_idempotency
    ON token_events(idempotency_key) WHERE idempotency_key IS NOT NULL;
-- Covering index for the scores aggregation (DeveloperCosts, #72):
--   SELECT developer, SUM(cost_micro), <realtime-only sum>
--   FROM token_events WHERE ts >= ? GROUP BY developer
-- Column order is (developer, ts, cost_micro, fidelity), NOT ts-leading: developer
-- leads so the GROUP BY is satisfied by index order (no sort / temp b-tree), ts
-- follows for the window filter, and cost_micro + fidelity make the index cover
-- every referenced column so the query runs index-only (no table heap access).
-- EXPLAIN QUERY PLAN confirms "USING COVERING INDEX" (#72 tests). A ts-leading
-- order is deliberately rejected: the planner prefers developer order to avoid
-- the grouping sort, so a ts-leading index goes unused for this query.
-- Created here, not in schemaTables Phase 1, because cost_micro is added by the
-- #69 migration on upgraded DBs (see schemaTables note).
CREATE INDEX IF NOT EXISTS idx_token_events_scores
    ON token_events(developer, ts, cost_micro, fidelity);
-- #60: merge_commit_sha is UNIQUE (one merged PR ↔ one merge commit) so a
-- replayed merged-PR webhook cannot double-insert an outcome even when two
-- deliveries race — the webhook handler's read-then-insert guard alone is
-- TOCTOU under net/http concurrency. Upgrade path for pre-#60 DBs: drop the
-- old non-unique index, dedup any rows it tolerated keeping the NEWEST per
-- SHA (matches OutcomeByMergeCommit's ORDER BY id DESC read semantics), then
-- create the unique index. The DELETE is a no-op on every boot after the
-- index exists, because the index prevents new duplicates from landing.
DROP INDEX IF EXISTS idx_outcomes_merge_commit_sha;
DELETE FROM outcomes WHERE merge_commit_sha IS NOT NULL AND id NOT IN (
    SELECT MAX(id) FROM outcomes WHERE merge_commit_sha IS NOT NULL
    GROUP BY merge_commit_sha
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_outcomes_merge_commit_sha_uq
    ON outcomes(merge_commit_sha) WHERE merge_commit_sha IS NOT NULL;
-- #196: push-captured outcomes aggregate to ONE row per (repo, issue_id, UTC day).
-- This partial-unique index is the idempotency guard behind UpsertPushOutcome: a
-- replay of the same day's commits, or a second commit on the same issue that day,
-- hits the conflict and no-ops (ON CONFLICT DO NOTHING), so the issue earns exactly
-- one 0.5-weight outcome that day — never 0.5×N (the commit-splitting inflation
-- vector RULING B closes). It is DISJOINT from idx_outcomes_merge_commit_sha_uq:
-- every source='push' row carries a non-null push_day and a NULL merge_commit_sha,
-- so the two dedup domains never overlap and a push outcome (no single merge commit)
-- coexists cleanly with the #60 merge-commit UNIQUE.
--
-- #231 reshaped this from (issue_id, push_day) to (repo, issue_id, push_day). Before
-- that, two repos both pushing to their own issue #42 on the same UTC day collided,
-- and ON CONFLICT DO NOTHING SILENTLY DROPPED the second repo's outcome.
--
-- The index is RENAMED, not edited. SQLite's CREATE UNIQUE INDEX IF NOT EXISTS is a
-- no-op when an index of that NAME already exists, regardless of its columns — so
-- reusing 'idx_outcomes_push_daily' on an upgraded DB would leave the OLD, broken
-- two-column shape in place forever and this migration would appear to succeed. The
-- unconditional DROP of the old name plus a new name is the same play the #60
-- merge-commit unique used above.
--
-- repo is NOT NULL (sentinel 'unqualified', never NULL): SQLite treats NULLs as
-- DISTINCT in a unique index, so a nullable leading column would silently disable
-- dedup for exactly the rows that most need it. Column order (repo, issue_id,
-- push_day) keeps the future tenant retrofit mechanical: tenant_id LEADS ->
-- (tenant_id, repo, issue_id, push_day). Created here in Phase 3, not schemaTables,
-- because repo and push_day are added by ALTER TABLE on upgraded DBs.
DROP INDEX IF EXISTS idx_outcomes_push_daily;
CREATE UNIQUE INDEX IF NOT EXISTS idx_outcomes_push_daily_repo
    ON outcomes(repo, issue_id, push_day) WHERE source = 'push';
-- Non-unique indexes on actual_spend / org_actual_spend (#24). Created
-- here, not in schemaTables, because the Phase 2.5 rebuild migration in
-- Open() drops the table and these indexes must be recreated on the
-- rebuilt table. Fresh DBs and upgraded DBs converge to the same end
-- state via this single CREATE INDEX IF NOT EXISTS statement.
CREATE INDEX IF NOT EXISTS idx_actual_spend_dev_period_nu
    ON actual_spend(developer, period);
CREATE INDEX IF NOT EXISTS idx_org_actual_spend_org_period_nu
    ON org_actual_spend(org, period);
`

// DB wraps the SQLite connection.
type DB struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at the given path and runs
// migrations.
//
// Permission contract (#130): on return the database file and any existing
// -wal/-shm sidecars are mode 0600 (owner read/write only) on both fresh and
// pre-existing databases. The DB holds per-developer spend, issue
// attribution, and org invoice totals, so it must never be world-readable on
// a shared host. Fresh files are created tight (O_CREATE with mode 0600 —
// never a transient world-readable window); legacy 0644 files and their
// sidecars are repaired by an explicit chmod pass at the end of Open. A chmod
// failure other than not-exist fails Open loudly (fail-loud over silent
// fallback). The confidentiality guarantee is POSIX-only: on Windows
// os.Chmod(path, 0600) merely clears the read-only attribute and returns nil,
// so the mode-bit protection does not apply there.
//
// Shared-directory / backup caveat: after this change, group-readable backup
// schemes stop working by design. An operator who needs a backup agent or
// group access to read the DB must run it as the tierd user, or deliberately
// relax the permissions understanding that anyone with read access can then
// see all spend and attribution data. tierd re-tightens the three files to
// 0600 on every Open (i.e. every restart), so a manual chmod does not stick.
func Open(path string) (*DB, error) {
	// Create-tight, don't create-then-tighten: pre-create the DB file 0600
	// before sql.Open so it is never world-readable, even transiently (umask
	// can only clear bits from 0600, never add). For an existing file,
	// OpenFile with O_CREATE and no O_EXCL leaves the mode untouched — the
	// explicit chmod sweep at the end of Open tightens legacy 0644 files.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create db file: %w", err)
	}
	_ = f.Close()
	// O_CREATE without O_EXCL leaves an existing legacy 0644 file's mode
	// untouched, so tighten the main file NOW — before any migration write
	// creates a -wal/-shm sidecar. SQLite creates sidecars with the main
	// file's mode, so a 0600 main file yields 0600 sidecars from birth, and
	// the legacy main-file repair holds even if a migration below fails and
	// returns early (the end-of-Open sweep would otherwise never run). Fail
	// loud on anything but not-exist (impossible here: we just created it).
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("restrict db file permissions %s: %w", path, err)
	}

	// Pragmas ride in the DSN, not a post-Open Exec (#63): busy_timeout is
	// connection-scoped, and database/sql transparently discards and
	// recreates its pooled connection (bad conn, max lifetime), silently
	// shedding any Exec-applied pragma with it. modernc.org/sqlite
	// re-applies _pragma DSN params on EVERY new connection, which is the
	// only reliable way to keep per-connection state through database/sql.
	// journal_mode=WAL persists in the DB file anyway (concurrent reads
	// during writes); re-asserting it per-connection is free and keeps both
	// settings in one place. busy_timeout=5000 prevents immediate
	// SQLITE_BUSY under contention.
	//
	// Deliberately NOT a file: URI — the driver splits the bare path on
	// '?' and applies _pragma params either way, while the URI form would
	// percent-decode the path and drop '#' fragments, breaking paths that
	// work today. A path containing '?' itself is not supported (the
	// driver's split, pre-existing in the bare-path form too).
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// Close the freshly-opened handle on ANY error return below (#316). Once
	// sql.Open succeeds we own a *sql.DB whose pooled connection and WAL -wal/-shm
	// handles must be released if a later phase -- schema apply, an ALTER, a data
	// migration, the post-migration indexes, the prune -- fails and returns nil.
	// A single success-guarded defer makes that leak-proof for every branch,
	// including any added later, so no migration step has to remember to close;
	// the happy path clears the guard just before returning the *DB, keeping the
	// handle open for the caller. (On POSIX the leak was a benign idle connection
	// until GC, but on Windows the open handle blocks deletion of the DB file for
	// any caller that recovers-and-retries against the same path.)
	success := false
	defer func() {
		if !success {
			_ = db.Close()
		}
	}()
	db.SetMaxOpenConns(1) // SQLite is single-writer; serialise all writes.
	// sql.Open never dials; probe now so a locked/unwritable DB (or a bad
	// DSN) fails here with a clear message instead of mid-schema-apply.
	// This also restores the early-failure behavior of the removed
	// Exec-based pragma call.
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("connect db: %w", err)
	}
	// Refuse-if-newer schema-version gate (#141). Read the stamped
	// PRAGMA user_version BEFORE any schema statement runs: an old binary opening
	// a database a NEWER tierd migrated must fail loudly and mutate NOTHING, never
	// silently half-apply its own (older) migration chain against a shape it does
	// not understand. A stored value <= schemaVersion (including the 0 that every
	// pre-#141 / freshly-created file carries) is the normal path — run the chain,
	// then stamp the current version at the end. See schemaVersion for the bump
	// convention.
	//
	// "Mutate NOTHING" is scoped to SCHEMA and DATA: the steps above this gate
	// (0600 chmod, WAL journal_mode via the DSN, and its -wal/-shm sidecars) touch
	// only file mode and the journal, never a table or row — so a refused Open
	// leaves sqlite_master and every row byte-identical (asserted by
	// TestOpen_RefusesNewerSchemaVersion).
	var storedVersion int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&storedVersion); err != nil {
		return nil, fmt.Errorf("read schema version: %w", err)
	}
	if storedVersion > schemaVersion {
		return nil, fmt.Errorf("database %s has schema version %d, newer than this binary supports (%d) — it was created or migrated by a newer tierd; upgrade the binary or point --db at a different file", path, storedVersion, schemaVersion)
	}
	// Phase 1: create tables and column-stable indexes. CREATE TABLE IF NOT
	// EXISTS is a no-op on databases that pre-date issue #6 (and lack the
	// idempotency_key column on token_events).
	//
	// Cross-process note: WAL journal mode plus busy_timeout=5000 serialise
	// DDL at the SQLite file-lock level, so two tier processes opening the
	// same DB simultaneously don't corrupt the schema. SetMaxOpenConns(1)
	// above protects only the single-process case; the cross-process safety
	// argument rests entirely on SQLite's exclusive-write lock.
	if _, err := db.Exec(schemaTables); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Phase 2: migrate pre-existing tables. ALTER TABLE returns
	// "duplicate column name: <col>" when the column already exists — the
	// expected case on every run after the one-time upgrade, so we swallow it.
	// modernc.org/sqlite emits this message verbatim (lowercase, stable);
	// matching the full phrase avoids accidentally swallowing unrelated
	// errors whose messages happen to contain "duplicate column".
	if err := addColumnIfMissing(db, "outcomes", "merge_commit_sha", "TEXT"); err != nil {
		return nil, err
	}
	// weight provenance + raw diff stats (#132). Idempotent ALTER TABLE — no
	// table rebuild, no data migration: existing rows default weight_source to
	// 'legacy' (their scale is unrecoverable) and read back with NULL diff
	// stats. No new unique index is added, so the tenant-ordering constraint on
	// outcomes is untouched.
	if err := addColumnIfMissing(db, "outcomes", "weight_source", "TEXT NOT NULL DEFAULT 'legacy'"); err != nil {
		return nil, err
	}
	if err := addColumnIfMissing(db, "outcomes", "additions", "INTEGER"); err != nil {
		return nil, err
	}
	if err := addColumnIfMissing(db, "outcomes", "deletions", "INTEGER"); err != nil {
		return nil, err
	}
	if err := addColumnIfMissing(db, "outcomes", "changed_files", "INTEGER"); err != nil {
		return nil, err
	}
	// Attribution provenance (#188). Idempotent ALTER TABLE — no table rebuild,
	// no data migration. The DEFAULT backfills every pre-#188 row to
	// 'github-webhook', which is correct: the GitHub webhook was the only outcome
	// source before this column existed. Convergent with the CREATE TABLE default
	// so fresh and upgraded schemas match. No new unique index is added, so the
	// tenant-ordering constraint on outcomes is untouched.
	if err := addColumnIfMissing(db, "outcomes", "source", "TEXT NOT NULL DEFAULT 'github-webhook'"); err != nil {
		return nil, err
	}
	// push-captured outcome aggregation key (#196). Nullable (no DEFAULT): only
	// source='push' rows carry it, so pre-#196 rows read back NULL and stay outside
	// the partial unique index idx_outcomes_push_daily. Idempotent ALTER TABLE — no
	// table rebuild, no data migration. The unique index that enforces one-outcome-
	// per-(issue,UTC-day) is created in schemaPostMigration (Phase 3), after this
	// column is guaranteed to exist on both fresh and upgraded DBs.
	if err := addColumnIfMissing(db, "outcomes", "push_day", "TEXT"); err != nil {
		return nil, err
	}
	// work-type taxonomy (#187). Idempotent ALTER TABLE — no table rebuild, no data
	// migration. work_type DEFAULT 'feature' backfills every pre-#187 row to the
	// honest baseline (a pre-taxonomy outcome carried no category; 'feature' is the
	// same default a labelless insert takes), and work_type_source DEFAULT 'legacy'
	// marks those rows as category-unknowable exactly as weight_source 'legacy' does.
	// Both DEFAULTs are convergent with the CREATE TABLE defaults so fresh and upgraded
	// schemas match. No new UNIQUE index is added, so the tenant-ordering constraint on
	// outcomes is untouched.
	if err := addColumnIfMissing(db, "outcomes", "work_type", "TEXT NOT NULL DEFAULT 'feature'"); err != nil {
		return nil, err
	}
	if err := addColumnIfMissing(db, "outcomes", "work_type_source", "TEXT NOT NULL DEFAULT 'legacy'"); err != nil {
		return nil, err
	}
	// repository qualifier (#231). Idempotent ALTER TABLE on both capture tables —
	// no table rebuild, no data migration. DEFAULT 'unqualified' (repoid.Unqualified)
	// backfills every pre-#231 row to the honest "repository unknowable" sentinel;
	// we deliberately do NOT guess a repo for historical rows.
	//
	// NOT NULL is load-bearing on outcomes: SQLite treats NULLs as DISTINCT inside a
	// unique index, so a nullable repo would make idx_outcomes_push_daily_repo stop
	// deduplicating replayed pushes. Both DEFAULTs are convergent with the CREATE
	// TABLE defaults so fresh and upgraded schemas match. The reshaped unique index
	// is created in schemaPostMigration (Phase 3), after this column is guaranteed to
	// exist on both fresh and upgraded DBs — the same ordering push_day required.
	if err := addColumnIfMissing(db, "token_events", "repo", "TEXT NOT NULL DEFAULT 'unqualified'"); err != nil {
		return nil, err
	}
	if err := addColumnIfMissing(db, "outcomes", "repo", "TEXT NOT NULL DEFAULT 'unqualified'"); err != nil {
		return nil, err
	}
	// session identity on token_events (#238). Idempotent ALTER TABLE — no table
	// rebuild, no data migration. NULLABLE with no DEFAULT: pre-#238 rows and every
	// session-blind producer (proxy, org pollers) read back NULL, which is the
	// honest "session unknowable" value (mirrors idempotency_key's nullable
	// provenance). No new UNIQUE index references it, so the tenant-ordering
	// constraint on token_events is untouched. Purely additive and ignorable to
	// every current read path, so schemaVersion is intentionally NOT bumped — an
	// old binary opening this newer DB keeps working (it never selects session_id)
	// and must not be locked out (see the schemaVersion bump convention).
	if err := addColumnIfMissing(db, "token_events", "session_id", "TEXT"); err != nil {
		return nil, err
	}
	// price-table provenance stamp (#233). Idempotent ALTER TABLE — no table
	// rebuild, no data migration. NOT NULL DEFAULT 0 backfills every pre-#233 row to
	// the "unstamped" sentinel; backfillPriceVersion (Phase 2.85 below) then stamps
	// those 0 rows with the currently-active table version. Convergent with the
	// CREATE TABLE default so fresh and upgraded schemas match. No new UNIQUE index
	// references it, so the tenant-ordering constraint is untouched, and it is purely
	// additive/ignorable to every current read path, so schemaVersion is NOT bumped.
	if err := addColumnIfMissing(db, "token_events", "price_version", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, err
	}
	// host-aware pricing basis (#300). Idempotent ALTER TABLE — no table rebuild, no
	// data migration. host DEFAULT 'unknown' (store.HostUnknown) backfills every
	// pre-#300 row to the honest "serving host unknowable" sentinel — we deliberately
	// do NOT guess a host for historical rows — and billing_mode DEFAULT 'per_token'
	// matches the first-party metered basis those rows were priced under. Both NOT
	// NULL with DEFAULTs convergent with the CREATE TABLE defaults so fresh and
	// upgraded schemas match (mirrors repo/price_version). No new UNIQUE index
	// references them, so the tenant-ordering constraint on token_events is untouched,
	// and both are purely additive/ignorable to every current read path, so
	// schemaVersion is NOT bumped (see the bump convention).
	if err := addColumnIfMissing(db, "token_events", "host", "TEXT NOT NULL DEFAULT 'unknown'"); err != nil {
		return nil, err
	}
	if err := addColumnIfMissing(db, "token_events", "billing_mode", "TEXT NOT NULL DEFAULT 'per_token'"); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`ALTER TABLE token_events ADD COLUMN idempotency_key TEXT`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return nil, fmt.Errorf("migrate idempotency_key: %w", err)
		}
	}
	// Phase 2.5: drop the pre-#24 CHECK (actual_paid_usd >= 0) constraint on
	// actual_spend and org_actual_spend. SQLite has no ALTER TABLE DROP
	// CONSTRAINT, so we rebuild the table on existing DBs that still have
	// the constraint. Fresh DBs skip this (schemaTables already created
	// without CHECK).
	if err := dropActualSpendNonNegativeCheck(db, "actual_spend"); err != nil {
		return nil, fmt.Errorf("migrate actual_spend drop check: %w", err)
	}
	if err := dropActualSpendNonNegativeCheck(db, "org_actual_spend"); err != nil {
		return nil, fmt.Errorf("migrate org_actual_spend drop check: %w", err)
	}
	// Phase 2.55: convert actual_paid_usd REAL → actual_paid_micro INTEGER
	// (integer micro-dollars, #69). Runs AFTER the CHECK-drop rebuild above so the
	// CHECK that referenced actual_paid_usd is already gone before DROP COLUMN.
	// Idempotent: no-op once the table is on actual_paid_micro (fresh or migrated).
	if err := migrateActualSpendToMicro(db, "actual_spend"); err != nil {
		return nil, fmt.Errorf("migrate actual_spend to micro: %w", err)
	}
	if err := migrateActualSpendToMicro(db, "org_actual_spend"); err != nil {
		return nil, fmt.Errorf("migrate org_actual_spend to micro: %w", err)
	}
	// Phase 2.56: add org_actual_spend.source (#138). Idempotent ALTER TABLE — no
	// table rebuild, no data migration. Runs AFTER the CHECK-drop rebuild (Phase 2.5)
	// and the micro conversion (Phase 2.55) so it adds to the FINAL table shape (the
	// rebuild recreates the table without this column, so adding earlier would lose
	// it). The DEFAULT backfills every pre-#138 row to 'manual', which is correct:
	// the provider-agnostic POST /api/v1/org_actual_spend endpoint was the only
	// writer before this column existed. Convergent with the CREATE TABLE default so
	// fresh and upgraded schemas match. No new UNIQUE index is added (org_actual_spend
	// accumulates delta rows, #24), so the tenant-ordering constraint is untouched.
	if err := addColumnIfMissing(db, "org_actual_spend", "source", "TEXT NOT NULL DEFAULT 'manual'"); err != nil {
		return nil, err
	}
	// Phase 2.6: split the legacy cache_write column into cache_write_5m +
	// cache_write_1h (issue #55). Idempotent on fresh DBs (schemaTables
	// already created the split columns) and on already-migrated upgrades.
	if err := migrateCacheWriteSplit(db); err != nil {
		return nil, fmt.Errorf("migrate cache_write split: %w", err)
	}
	// Phase 2.65: convert cost_usd REAL → cost_micro INTEGER (integer
	// micro-dollars, #69). Runs AFTER the cache split (so recompute below sees the
	// final column shape) and BEFORE recomputeKnownSourceCosts (which now writes
	// cost_micro). Idempotent: no-op once token_events is on cost_micro.
	if err := migrateCostUSDToMicro(db); err != nil {
		return nil, fmt.Errorf("migrate cost_usd to micro: %w", err)
	}
	// Phase 2.7: one-shot cost recompute for rows owned by collectors we
	// control (JSONL/proxy/admin pollers). External /api/v1/costs rows are
	// preserved — their cost_usd is the caller's authoritative number. Runs
	// after Phase 2.6 so the recomputed cost reflects the TTL-aware
	// multipliers. Idempotent: re-running yields identical values.
	//
	// (#72) recomputeKnownSourceCosts is marker-gated in tier_migrations, so the
	// full-table scan runs only on the FIRST boot after #55 — every later boot
	// pays one indexed marker lookup, not a scan (same for backfillPeriodMembership
	// at Phase 2.8). The remaining per-boot migration cost is cheap idempotent
	// DDL. The concurrent-first-boot hardening the #72 review imagined (two
	// processes racing this one-time scan under busy_timeout) is deferred by
	// design: tier is a single-process, single-writer POC (SetMaxOpenConns(1))
	// with no multi-writer deployment to drive it.
	if err := recomputeKnownSourceCosts(db); err != nil {
		return nil, fmt.Errorf("recompute cost_micro: %w", err)
	}
	// Phase 2.8: backfill period_membership from org_hierarchy for DBs that
	// predate #41 — existing members become "active since the beginning of
	// time" (period_start='0000-01', open) so their seat count is unchanged
	// until finance closes a membership via EndMembership. Gated by a marker.
	if err := backfillPeriodMembership(db); err != nil {
		return nil, fmt.Errorf("backfill period_membership: %w", err)
	}
	// Phase 2.85: one-shot stamp of price_version on pre-#233 rows (#233). Runs after
	// the ADD COLUMN above and after any --prices override is applied (main.go loads
	// it before store.Open), so the active version it stamps is the table those rows
	// were last priced under. Marker-gated: a full-table UPDATE on the first boot
	// after #233, an indexed marker lookup every boot after.
	if err := backfillPriceVersion(db); err != nil {
		return nil, fmt.Errorf("backfill price_version: %w", err)
	}
	// Phase 3: indexes that reference migration-added columns. Safe to run now
	// that the column is guaranteed to exist on both fresh and upgraded DBs.
	if _, err := db.Exec(schemaPostMigration); err != nil {
		return nil, fmt.Errorf("apply post-migration schema: %w", err)
	}
	// Phase 4: bounded-retention prune of the raw webhook audit trail (#137).
	// Synchronous at boot, indexed on received_at + id. This is the ONLY prune
	// scheduling for now: the P2-04 serve-path daily ticker scaffold does not
	// exist in the tree yet, so a boot-time prune alone bounds the table for a
	// single-tenant, frequently-restarted deployment (a long-running server that
	// never restarts would defer pruning until its next boot — acceptable given
	// the 500 MB hard ceiling from the 50k-row cap; add a 24h ticker alongside
	// the P2-04 share ticker if/when it lands). Uses the same *DB the caller
	// receives, so it runs against the fully-migrated schema above.
	d := &DB{db: db}
	if _, err := d.PruneWebhookPayloads(context.Background()); err != nil {
		return nil, fmt.Errorf("prune webhook payloads: %w", err)
	}
	// Stamp the schema version LAST, after every migration phase has converged the
	// schema to its final shape (#141). Doing it here — not before the chain —
	// means a crash mid-migration leaves the stored version unchanged, so the next
	// boot re-runs the (idempotent) chain rather than believing a half-migrated DB
	// is complete. PRAGMA user_version cannot be parameter-bound, so the const is
	// spliced via Sprintf; it is a compile-time integer literal (not external
	// input), the same identifier-splice justification addColumnIfMissing relies on.
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return nil, fmt.Errorf("stamp schema version: %w", err)
	}
	// Tighten spend data to owner-only (#130). The -wal/-shm sidecars are
	// created during the schema/migration writes above; SQLite creates them
	// with the main file's mode, but pre-fix DBs on disk may carry 0644 on
	// all three — tighten explicitly (belt-and-braces + legacy repair).
	// ErrNotExist on a sidecar is fine (a checkpointed, cleanly-closed DB has
	// no -wal until the next write; recreation inherits the now-0600
	// main-file mode). Any other chmod failure fails Open loudly.
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("restrict db file permissions %s: %w", p, err)
		}
	}
	success = true // hand the open handle to the caller; skip the deferred Close.
	return d, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// dropActualSpendNonNegativeCheck rebuilds an actual_spend-shaped table so
// the pre-#24 `CHECK (actual_paid_usd >= 0)` constraint is dropped. SQLite
// has no ALTER TABLE DROP CONSTRAINT, so the only path is the documented
// rebuild dance: create a new table without the constraint, copy data,
// drop the old, rename the new.
//
// Idempotent: if the existing table doesn't have the CHECK (fresh DBs or
// already-migrated upgrades), this is a no-op. Detection works by
// inspecting sqlite_master.sql with whitespace-insensitive matching — a
// future schemaTables reformat (extra spaces, tabs) won't silently skip
// the migration.
//
// table must be either "actual_spend" or "org_actual_spend"; an allowlist
// guard rejects anything else because the rebuild SQL splices the value
// directly into DDL via fmt.Sprintf (SQLite doesn't allow placeholders for
// identifiers). Callers are package-internal and use string literals, but
// the guard means a future careless caller can't open up an injection
// surface.
func dropActualSpendNonNegativeCheck(db *sql.DB, table string) error {
	keyCol := ""
	switch table {
	case "actual_spend":
		keyCol = "developer"
	case "org_actual_spend":
		keyCol = "org"
	default:
		return fmt.Errorf("dropActualSpendNonNegativeCheck: unsupported table %q", table)
	}

	var schemaSQL string
	err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
	).Scan(&schemaSQL)
	if errors.Is(err, sql.ErrNoRows) {
		// Table not yet created (shouldn't happen since schemaTables runs
		// before this) — let the post-migration index step fail clearly.
		return nil
	}
	if err != nil {
		return err
	}
	// Normalise whitespace before substring-matching. SQLite stores the
	// CREATE statement verbatim, so a future schemaTables reformat would
	// otherwise change the byte pattern and silently disable this
	// migration. strings.Fields collapses any whitespace run to a single
	// space, defeating that.
	norm := strings.Join(strings.Fields(schemaSQL), " ")
	if !strings.Contains(norm, "CHECK (actual_paid_usd >= 0)") {
		return nil
	}

	// BEGIN IMMEDIATE (via LevelSerializable on modernc.org/sqlite) takes
	// the RESERVED lock up front, so busy_timeout=5000 can actually retry
	// instead of two processes racing the deferred-to-write upgrade and
	// failing with SQLITE_BUSY.
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	newTable := "__new_" + table
	createNew := fmt.Sprintf(`
		CREATE TABLE %s (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			%s              TEXT    NOT NULL,
			period          TEXT    NOT NULL CHECK (period GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]'),
			actual_paid_usd REAL    NOT NULL,
			ts              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, newTable, keyCol)
	if _, err := tx.Exec(createNew); err != nil {
		return fmt.Errorf("create %s: %w", newTable, err)
	}
	copySQL := fmt.Sprintf(
		`INSERT INTO %s (id, %s, period, actual_paid_usd, ts) SELECT id, %s, period, actual_paid_usd, ts FROM %s`,
		newTable, keyCol, keyCol, table,
	)
	if _, err := tx.Exec(copySQL); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", table, newTable, err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`DROP TABLE %s`, table)); err != nil {
		return fmt.Errorf("drop %s: %w", table, err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, newTable, table)); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", newTable, table, err)
	}
	return tx.Commit()
}

// addColumnIfMissing runs ALTER TABLE ADD COLUMN and swallows the
// "duplicate column name: <col>" error emitted by modernc.org/sqlite when the
// column already exists. Other errors propagate. Shared helper so each new
// migration doesn't re-implement the swallow pattern.
//
// IMPORTANT: table, column, and typeDecl MUST be hard-coded string literals
// from this package — they are spliced into a DDL statement via fmt.Sprintf
// with no quoting. SQLite (and standard SQL) does not allow parameter
// binding for identifiers, so this is unavoidable. If this helper is ever
// exported, add an allowlist validation step on the inputs.
func addColumnIfMissing(db *sql.DB, table, column, typeDecl string) error {
	stmt := fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, typeDecl)
	if _, err := db.Exec(stmt); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate %s.%s: %w", table, column, err)
		}
	}
	return nil
}

// columnExists reports whether a column is present on the given table. Uses
// PRAGMA table_info, which returns one row per existing column. Same
// identifier-safety constraints as addColumnIfMissing: callers must pass
// hard-coded literals because the table name is spliced into the PRAGMA
// statement (SQLite forbids parameter binding for identifiers).
func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, fmt.Errorf("table_info(%s): %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// migrateCacheWriteSplit upgrades a pre-#55 schema (single `cache_write`
// column) to the post-#55 shape (`cache_write_5m` + `cache_write_1h`).
//
// Steps:
//  1. ADD the two TTL-split columns (idempotent — addColumnIfMissing
//     swallows the "duplicate column name" error returned when the columns
//     already exist on fresh DBs whose schemaTables created them).
//  2. BEGIN IMMEDIATE so concurrent Open() calls in two processes can't both
//     race past the column-existence check. The lock is held for the rest
//     of this function.
//  3. Re-check whether the legacy `cache_write` column still exists, using
//     the tx (so we see the same schema view as the subsequent UPDATE/DROP).
//  4. Backfill its values into `cache_write_5m` (matches Anthropic's
//     pre-1h-feature default — legacy writes had no TTL annotation and 5m
//     was the only mode). The `cache_write_5m = 0` guard makes the backfill
//     safe to retry: a crash between ADD and DROP, followed by a second
//     Open() finding the legacy column still present, must not clobber data
//     that some newer binary wrote into cache_write_5m during the gap.
//  5. DROP the legacy column.
//
// The ADD COLUMN statements at step 1 run *outside* the tx because SQLite
// won't let ALTER TABLE ADD COLUMN run in a deferred-write transaction
// alongside data DML on the same table. The cost: a half-migrated DB
// (cache_write_5m added but cache_write_1h not) is possible if the process
// dies between the two ADDs. The next Open() repairs it: the first ADD is
// a no-op (column exists), the second proceeds. So this is degraded
// resilience, not corruption.
//
// ALTER TABLE DROP COLUMN requires SQLite 3.35+. modernc.org/sqlite vendors
// a recent SQLite (well past 3.35), so this is safe.
func migrateCacheWriteSplit(db *sql.DB) error {
	if err := addColumnIfMissing(db, "token_events", "cache_write_5m", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "token_events", "cache_write_1h", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// Lock first, check second — see step (2)/(3) in the doc comment for
	// the race-window argument.
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	legacyExists, err := columnExistsTx(tx, "token_events", "cache_write")
	if err != nil {
		return err
	}
	if !legacyExists {
		// Another process won the race and completed the migration. We hold
		// the lock but have nothing to do.
		return tx.Commit()
	}
	if _, err := tx.Exec(`UPDATE token_events SET cache_write_5m = cache_write WHERE cache_write_5m = 0 AND cache_write > 0`); err != nil {
		return fmt.Errorf("backfill cache_write_5m: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE token_events DROP COLUMN cache_write`); err != nil {
		return fmt.Errorf("drop legacy cache_write column: %w", err)
	}
	return tx.Commit()
}

// columnExistsTx is the tx-aware variant of columnExists. Same identifier-
// safety constraints — `table` must be a hard-coded literal.
func columnExistsTx(tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, fmt.Errorf("table_info(%s): %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// migrateCostUSDToMicro converts a pre-#69 token_events schema (cost_usd REAL,
// dollars) to the post-#69 shape (cost_micro INTEGER, micro-dollars). Same
// ADD / backfill / DROP shape as migrateCacheWriteSplit:
//
//  1. ADD cost_micro (idempotent — addColumnIfMissing swallows the duplicate-
//     column error on fresh DBs whose schemaTables already created it).
//  2. BEGIN IMMEDIATE, then re-check (under the lock) whether the legacy
//     cost_usd column still exists, so two racing Open() calls can't both run
//     the conversion.
//  3. Backfill cost_micro = round(cost_usd × 1e6). SQLite ROUND is half-away-
//     from-zero rather than the half-to-even DollarsToMicro uses for live
//     events; the ≤0.5 micro-dollar divergence on this one-time legacy
//     conversion is immaterial. Unconditional UPDATE is safe: cost_micro was
//     just added as 0 on every existing row and cost_usd is dropped below, so
//     this runs exactly once.
//  4. DROP the old idx_token_events_scores — it references cost_usd, and SQLite
//     refuses ALTER TABLE DROP COLUMN while an index references the column. The
//     replacement index on cost_micro is (re)created in schemaPostMigration.
//  5. DROP COLUMN cost_usd.
//
// ALTER TABLE ADD COLUMN runs outside the tx (SQLite won't mix it with data DML
// on the same table in a deferred-write tx); the half-migrated window it opens
// is repaired by the next Open() exactly as migrateCacheWriteSplit documents.
func migrateCostUSDToMicro(db *sql.DB) error {
	if err := addColumnIfMissing(db, "token_events", "cost_micro", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	legacyExists, err := columnExistsTx(tx, "token_events", "cost_usd")
	if err != nil {
		return err
	}
	if !legacyExists {
		// Fresh DB (schemaTables created cost_micro) or another process already
		// completed the conversion.
		return tx.Commit()
	}
	if _, err := tx.Exec(`UPDATE token_events SET cost_micro = CAST(ROUND(cost_usd * 1000000) AS INTEGER)`); err != nil {
		return fmt.Errorf("backfill cost_micro: %w", err)
	}
	if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_token_events_scores`); err != nil {
		return fmt.Errorf("drop legacy idx_token_events_scores: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE token_events DROP COLUMN cost_usd`); err != nil {
		return fmt.Errorf("drop legacy cost_usd column: %w", err)
	}
	return tx.Commit()
}

// migrateActualSpendToMicro converts a pre-#69 actual_spend-shaped table
// (actual_paid_usd REAL, dollars) to the post-#69 shape (actual_paid_micro
// INTEGER, micro-dollars). Same ADD / backfill / DROP shape as
// migrateCostUSDToMicro; no index juggling is needed because the money column
// is not indexed (the (developer|org, period) indexes don't reference it).
//
// For pre-#24 DBs this runs AFTER dropActualSpendNonNegativeCheck in Open(), so
// the `CHECK (actual_paid_usd >= 0)` constraint that referenced the old column
// is already gone before DROP COLUMN.
//
// table must be "actual_spend" or "org_actual_spend" — an allowlist guard
// because the name is spliced into DDL via fmt.Sprintf (SQLite forbids
// parameter binding for identifiers).
func migrateActualSpendToMicro(db *sql.DB, table string) error {
	switch table {
	case "actual_spend", "org_actual_spend":
		// allowed
	default:
		return fmt.Errorf("migrateActualSpendToMicro: unsupported table %q", table)
	}
	if err := addColumnIfMissing(db, table, "actual_paid_micro", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	legacyExists, err := columnExistsTx(tx, table, "actual_paid_usd")
	if err != nil {
		return err
	}
	if !legacyExists {
		return tx.Commit()
	}
	if _, err := tx.Exec(fmt.Sprintf(
		`UPDATE %s SET actual_paid_micro = CAST(ROUND(actual_paid_usd * 1000000) AS INTEGER)`, table,
	)); err != nil {
		return fmt.Errorf("backfill %s.actual_paid_micro: %w", table, err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE %s DROP COLUMN actual_paid_usd`, table)); err != nil {
		return fmt.Errorf("drop legacy %s.actual_paid_usd column: %w", table, err)
	}
	return tx.Commit()
}

// migrationRecomputeCacheTTL is the tier_migrations marker for the one-shot
// cost recompute done by recomputeKnownSourceCosts. Constant so a test can
// reference it without re-typing the magic string.
const migrationRecomputeCacheTTL = "cache_ttl_recompute_v55"

// migrationPeriodMembershipBackfill marks the one-shot backfill of
// period_membership from pre-#41 org_hierarchy rows.
const migrationPeriodMembershipBackfill = "period_membership_backfill_v41"

// migrationBackfillPriceVersion marks the one-shot backfill of price_version on
// pre-#233 token_events rows (stamped 0 by the ADD COLUMN default) with the
// currently-active table version. Constant so a test can reference it.
const migrationBackfillPriceVersion = "price_version_backfill_v233"

// backfillPeriodMembership seeds period_membership from org_hierarchy for DBs
// created before #41: each existing org member becomes one open membership
// active from the beginning of time ('0000-01'). Idempotent via a
// tier_migrations marker; on a fresh DB org_hierarchy is empty so it inserts
// nothing and just records the marker.
func backfillPeriodMembership(db *sql.DB) error {
	var marker string
	err := db.QueryRow(
		`SELECT name FROM tier_migrations WHERE name = ?`,
		migrationPeriodMembershipBackfill,
	).Scan(&marker)
	if err == nil {
		return nil // already applied
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check tier_migrations: %w", err)
	}
	// Seed + marker in one tx so a crash between them can't leave the table
	// seeded without the marker (which would re-seed duplicates on next boot) —
	// matches the recomputeKnownSourceCosts pattern.
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin period_membership backfill tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		INSERT INTO period_membership (developer, org, period_start, period_end)
		SELECT developer, org, '0000-01', NULL
		FROM org_hierarchy
		WHERE org IS NOT NULL AND org != ''`); err != nil {
		return fmt.Errorf("seed period_membership: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO tier_migrations (name) VALUES (?)`,
		migrationPeriodMembershipBackfill,
	); err != nil {
		return fmt.Errorf("record tier_migrations marker: %w", err)
	}
	return tx.Commit()
}

// backfillPriceVersion stamps pre-#233 token_events rows — the ADD COLUMN default
// leaves them at price_version 0 — with the CURRENTLY-ACTIVE table version. That is
// the honest value: those rows were last priced under whatever table this binary
// embeds (or the --prices override, already applied before Open), so recording it
// makes the provenance stamp truthful without pretending to know an earlier version
// history we never captured. Marker-gated so the one UPDATE runs on the FIRST boot
// after #233 and every later boot pays one indexed marker lookup.
//
// The active version is a Go value (not a DB column), so it is spliced as a bound
// parameter, not interpolated. Reading ActivePriceTableInfo here rides the same
// write-once-before-serve discipline as ComputeCost: main.go applies any --prices
// override before store.Open, so the table is settled by the time this runs. Only
// rows still at the 0 sentinel are touched — a genuinely 0-versioned future row (if
// the active version were ever 0, which parsePriceTable forbids: version >= 1) would
// be indistinguishable, but that cannot occur.
func backfillPriceVersion(db *sql.DB) error {
	var marker string
	err := db.QueryRow(
		`SELECT name FROM tier_migrations WHERE name = ?`,
		migrationBackfillPriceVersion,
	).Scan(&marker)
	if err == nil {
		return nil // already applied
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check tier_migrations: %w", err)
	}
	activeVersion := ActivePriceTableInfo().Version
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin price_version backfill tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`UPDATE token_events SET price_version = ? WHERE price_version = 0`,
		activeVersion,
	); err != nil {
		return fmt.Errorf("backfill token_events.price_version: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO tier_migrations (name) VALUES (?)`,
		migrationBackfillPriceVersion,
	); err != nil {
		return fmt.Errorf("record tier_migrations marker: %w", err)
	}
	return tx.Commit()
}

// recomputeKnownSourceCosts repairs cost_micro on every token_events row owned
// by an in-tree collector (JSONL, proxy, admin pollers). Pre-#55 rows were
// priced with a flat 1.0× multiplier on cache reads and cache writes; under
// the new TTL-aware ComputeCost the same token counts produce different
// values. Without this step, the migration would leave historical
// cost_micro values frozen at the wrong number.
//
// External /api/v1/costs rows are NOT touched. Their cost_micro is the
// caller's authoritative figure (e.g. a finance team posting reconciled
// numbers from an invoice); recomputing them from token counts would
// silently overwrite that authority. (Including the source='api' row in
// the SELECT predicate would also expand the recompute set unnecessarily.)
//
// Gated by a tier_migrations marker row so subsequent Open() calls don't
// re-scan token_events. On a fresh install (no rows yet) the first Open
// inserts the marker without doing any work; on an upgrade the recompute
// runs once and is then permanently skipped.
//
// Memory: we materialise all (id, cost) pairs into a slice before opening
// the write tx. This is intentional given SetMaxOpenConns(1) — issuing a
// write tx while a SELECT cursor is still iterating would deadlock on the
// single connection. A streaming alternative would have to either use the
// same tx for both query and writes (modernc.org/sqlite supports it but
// the semantics are subtle under WAL) or paginate. 16 bytes per row × <1M
// rows = <16MB peak; safe for any realistic local DB. If a user's DB grows
// to multi-million rows and startup gets slow, switch to a paginated
// streaming variant.
//
// Idempotent: re-running with the same priceTable yields identical values,
// so a crash mid-recompute followed by a retry converges.
func recomputeKnownSourceCosts(db *sql.DB) error {
	// Skip-if-already-applied gate. The cheapest possible early-exit; one
	// indexed lookup against a tiny table.
	var marker string
	err := db.QueryRow(
		`SELECT name FROM tier_migrations WHERE name = ?`,
		migrationRecomputeCacheTTL,
	).Scan(&marker)
	if err == nil {
		return nil // already applied
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check tier_migrations: %w", err)
	}

	rows, err := db.Query(`
		SELECT id, model, input_tok, output_tok, cache_read, cache_write_5m, cache_write_1h
		FROM token_events
		WHERE source IN ('jsonl', 'proxy', 'copilot-api', 'anthropic-admin')`)
	if err != nil {
		return err
	}
	type updateRow struct {
		id   int64
		cost int64 // micro-dollars (#69)
	}
	var updates []updateRow
	for rows.Next() {
		var id int64
		var model string
		var input, output, cr, w5, w1 int
		if err := rows.Scan(&id, &model, &input, &output, &cr, &w5, &w1); err != nil {
			_ = rows.Close()
			return err
		}
		// Intentionally host-BLIND (#300): this one-shot #55 repricer runs at Open()
		// before any insert and is marker-guarded, so it only ever touches pre-#300
		// rows (host backfilled to the 'unknown' sentinel), for which the model-only
		// ComputeCost is exactly correct. Do NOT wire host-qualified pricing in here
		// when #268 lands — a host-aware reprice belongs in the sanctioned `tierd
		// reprice` (#294) path, which must SELECT host and use ComputeCostHost.
		cost := ComputeCost(model, CostUsage{
			Input:        input,
			Output:       output,
			CacheRead:    cr,
			CacheWrite5m: w5,
			CacheWrite1h: w1,
		})
		updates = append(updates, updateRow{id, cost})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	// Open a single tx for both the cost updates and the marker insert so
	// a crash between the two leaves no half-finished state — either the
	// recompute ran and the marker is recorded, or neither.
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if len(updates) > 0 {
		stmt, err := tx.Prepare(`UPDATE token_events SET cost_micro = ? WHERE id = ?`)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		for _, u := range updates {
			if _, err := stmt.Exec(u.cost, u.id); err != nil {
				return fmt.Errorf("update id=%d: %w", u.id, err)
			}
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO tier_migrations (name) VALUES (?)`,
		migrationRecomputeCacheTTL,
	); err != nil {
		return fmt.Errorf("record tier_migrations marker: %w", err)
	}
	return tx.Commit()
}

// TokenEvent is a single recorded API call.
//
// Cache fields carry provider-specific semantics — see collector.TokenEvent
// for the canonical contract. CacheWrite5m / CacheWrite1h come from
// Anthropic's nested `cache_creation.ephemeral_{5m,1h}_input_tokens`; legacy
// rows pre-dating issue #55 are migrated with all writes in CacheWrite5m.
type TokenEvent struct {
	// ID is the token_events.id primary key. Populated only by the bulk-export
	// read query (ListTokenEvents, #191) that needs a stable keyset cursor
	// position; zero on rows built by callers or by other reads that don't
	// select it (mirrors Outcome.ID).
	ID             int64
	Developer      string
	IssueID        string
	Model          string
	InputTok       int
	OutputTok      int
	CacheRead      int
	CacheWrite5m   int
	CacheWrite1h   int
	CostMicro      int64  // cost in integer micro-dollars (1 USD = 1e6), #69
	Source         string // "api" (manual REST) | "jsonl" | "proxy" | "copilot-api" | "anthropic-admin"
	Fidelity       string // "realtime" | "daily" | "estimated"
	IdempotencyKey string // empty = legacy/unkeyed, may double-insert; non-empty enforces uniqueness
	// Repo is the canonical "owner/repo" this cost was spent in (#231), or
	// repoid.Unqualified when the producer could not determine it (the proxy).
	// Empty on a caller-built value is normalized to the sentinel at insert.
	Repo string
	// SessionID is the opaque upstream session identity (#238) — see
	// collector.TokenEvent for the canonical contract. Empty when the producer
	// could not know it (proxy / poller rows); stored as SQL NULL and read back
	// as "" (COALESCE), so empty and NULL are indistinguishable at this boundary.
	SessionID string
	// PriceVersion is the price-table version that produced CostMicro (#233). Zero
	// when the caller leaves it unset; InsertTokenEvent then stamps the active
	// table version (ActivePriceTableInfo().Version) so every write records the
	// table it was priced under. Read back by ListTokenEvents for the export
	// provenance column; INSERT-only, never rewritten by a replay.
	PriceVersion int
	// Host is the serving host that priced this event (#300) — the property that
	// determines an open-weights model's cost (OpenRouter vs Together vs Ollama vs
	// self-hosted all differ). Set by the proxy from its --target URL host; empty on
	// the first-party JSONL/poller paths and normalized to HostUnknown at insert so
	// the NOT NULL column never stores "". INSERT-only, like repo.
	Host string
	// BillingMode is how the host bills this event (#300): per_token, subscription
	// (#113 flat $/mo), or self_hosted_amortized (an APPROXIMATE estimate, never a
	// canonical $/M). Derived from the resolved price entry by ComputeCostHost; empty
	// normalizes to per_token at insert. INSERT-only.
	BillingMode string
	Timestamp   time.Time
}

// insertTokenEventSQL is the statement shared by single + bulk inserts.
// NULLIF(?, '') stores NULL when the key is empty so the partial unique index
// (idempotency_key WHERE idempotency_key IS NOT NULL) allows unbounded legacy
// rows. The same NULLIF wraps session_id (#238) so a session-blind producer's
// "" lands as SQL NULL, keeping the column provenance-honest.
//
// The ON CONFLICT clause repeats the same WHERE predicate as the partial index
// — SQLite's UPSERT requires the conflict target's WHERE to match the index's
// WHERE exactly, otherwise the UPSERT silently doesn't fire and SQLite returns
// "ON CONFLICT clause does not match any PRIMARY KEY or UNIQUE constraint."
//
// Conflict semantics: take the per-field MAX on the TOKEN-COUNT fields. After #19
// both JSONL and proxy emit per-message events keyed by
// MessageIdempotencyKey(provider, msg.id) — per-message totals are immutable, so
// MAX(x, x) = x and the operation is effectively DO NOTHING.
//
// cost_micro is DELIBERATELY NOT in the DO UPDATE set (#233): once the server has
// priced a row under its table, that cost is IMMUTABLE on replay. The old
// cost_micro = MAX(...) silently repriced stored history UPWARD (only ever up)
// whenever a laptop upgraded to a binary embedding a higher-priced table and
// re-shipped its 90-day window — a one-directional ratchet that made a developer's
// spend depend on which binary last touched it. The server is now the single
// pricing authority (/events re-prices raw token counts with ITS table on first
// insert), and repricing is an explicit operator action, never a side effect of a
// replay. price_version is likewise INSERT-only (it records the table that priced
// the immutable cost, so it must move only when the cost does — i.e. never here).
// NOTE (#235, DONE): ts is now INSERT-only — dropped from the DO UPDATE set for the
// same "immutable upstream fact" reason. Per-message capture time is a first-writer
// fact, not a placeholder that a later replay should promote (unlike the token
// counts below); the old MAX(ts, excluded.ts) let one skewed laptop clock pin a
// far-future ts a corrected re-ship could never pull back. The insert-time
// now+skew bound (api.maxEventClockSkew) is the paired guard that keeps a bad clock
// out in the first place.
//
// MAX is retained on the token counts as a belt-and-braces guard for one edge case:
// JSONL's
// dedup-by-message-id (`messages[id] = larger`) keeps the largest-total
// entry, but a placeholder partial could in principle be flushed first by an
// out-of-order writer; if a re-scan then sees the definitive entry, MAX
// promotes the row to the correct totals rather than freezing at the
// placeholder. For cross-source dedup (the proxy and JSONL converging on the
// same upstream msg_*) the values agree exactly, so MAX is a no-op.
//
// Consequence of the count-MAX + cost-freeze split (#233): if a placeholder
// partial is keyed FIRST and a later definitive entry MAX-promotes the token
// counts, cost_micro stays frozen at the placeholder's (under-priced) first-writer
// value — so such a row's cost_micro is NOT ComputeCost(its final counts). This is
// an intended, very low-probability edge (per-message keying makes replays
// identical); `tierd reprice` (#294) is the sanctioned path to reconcile it.
//
// Algebra caveat (#55): MAX is applied to cache_write_5m and cache_write_1h
// independently. If two sources somehow disagreed on the TTL split for the
// same msg_* (e.g. one parsed the nested cache_creation object and the
// other fell back to the legacy 5m bucket), the independent MAX would
// over-count by combining buckets from both readings. In practice this
// cannot happen: both sources parse the same upstream JSON bytes, so they
// observe the same nested-or-absent shape for any given msg_*. The
// invariant rests on Anthropic returning identical responses to the same
// upstream call regardless of how the client captures them. If a future
// data source begins producing per-message events with TTL information
// derived from a different upstream (e.g. an aggregated billing API), this
// MAX strategy will need to become a recompute-on-merge.
//
// repo (#231), session_id (#238), cost_micro + price_version (#233), ts (#235), and
// host + billing_mode (#300) are INSERTed but deliberately absent from the DO UPDATE
// set. A replay must never rewrite an established row's repository, session, PRICE,
// capture TIME, or serving HOST: the first writer to key a message wins, and a later
// producer that happens to be repo-blind or session-blind (the proxy) must not
// downgrade a row the collector already qualified — NULLing out a session_id the JSONL
// path stamped would destroy the exact grouping key #238 exists to preserve, re-MAXing
// cost_micro would silently reprice history (#233), MAXing ts would let a skewed clock
// ratchet a row into every future window (#235), and overwriting host would let a
// host-blind replay erase the pricing basis #300 records.
const insertTokenEventSQL = `
INSERT INTO token_events
    (developer, issue_id, model, input_tok, output_tok, cache_read,
     cache_write_5m, cache_write_1h,
     cost_micro, source, fidelity, repo, idempotency_key, session_id, price_version,
     host, billing_mode, ts)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?)
ON CONFLICT(idempotency_key) WHERE idempotency_key IS NOT NULL DO UPDATE SET
    input_tok      = MAX(input_tok,      excluded.input_tok),
    output_tok     = MAX(output_tok,     excluded.output_tok),
    cache_read     = MAX(cache_read,     excluded.cache_read),
    cache_write_5m = MAX(cache_write_5m, excluded.cache_write_5m),
    cache_write_1h = MAX(cache_write_1h, excluded.cache_write_1h)`

// normalizeRepo maps an empty or unset repo to the reserved sentinel so the NOT NULL
// column never sees "". Callers that KNOW the repository pass a repoid.Canonical
// slug; callers that cannot pass "" and get repoid.Unqualified.
func normalizeRepo(s string) string {
	if s == "" {
		return repoid.Unqualified
	}
	return s
}

// normalizeBillingMode maps an unset billing_mode to per_token so the NOT NULL
// column never sees "" (#300). Host uses normalizeHost (prices.go), which maps ""
// to the HostUnknown sentinel the same way.
//
// ⚠️ Every producer that DERIVES a cost now passes the mode ComputeCostHost
// resolved — the proxy, codexrollout, /events (#492), and as of #525 the JSONL
// collector and both org pollers. This doc used to name JSONL and the pollers as
// leaving it empty; that was true, and it was the #525 defect, so do not restore
// that sentence as a description of intended behaviour.
//
// What still reaches the default is the surfaces that IMPORT a cost they did not
// derive and therefore have no mode to state: /costs manual imports and the demo
// seeder. per_token is the least-surprising default for those, but it is a
// DEFAULT, not a measurement — a manually imported subscription cost still
// exports as per_token, which is a real gap, just not one #525 closes.
//
// It does NOT validate a non-empty value against validBillingModes: the only
// producer of a non-empty mode is ComputeCostHost, which returns a value already
// parse-validated by parsePriceTable, so this is a trust boundary, not an input
// gate. A future producer that fabricates a mode string should validate at its own
// boundary (or this should grow a check) rather than persist an unrecognized mode.
func normalizeBillingMode(s string) string {
	if s == "" {
		return BillingPerToken
	}
	return s
}

// stampPriceVersion resolves the price_version to store (#233): an explicit
// non-zero value from the caller is honored (e.g. a reprice writing the target
// version), and the zero "unset" sentinel is stamped with the currently-active
// table version — the table that priced this event. Reading ActivePriceTableInfo
// here rides the same write-once-before-serve discipline priceTable itself does
// (the active table is settled at startup before any insert), so no lock is needed.
func stampPriceVersion(pv int) int {
	if pv == 0 {
		return ActivePriceTableInfo().Version
	}
	return pv
}

// insertTokenEventArgs builds the positional argument list for
// insertTokenEventSQL, applying the same NULL/sentinel normalization every
// insert path relies on (repo, host, billing_mode, price_version). Shared by
// InsertTokenEvent, InsertTokenEvents, and InsertManualCostEvent so the column
// order lives in exactly one place — a mismatch between the SQL's VALUES list
// and the arg order is a silent data-corruption bug, so it must not be
// duplicated per caller.
func insertTokenEventArgs(e TokenEvent) []any {
	return []any{
		e.Developer, e.IssueID, e.Model,
		e.InputTok, e.OutputTok, e.CacheRead,
		e.CacheWrite5m, e.CacheWrite1h,
		e.CostMicro, e.Source, e.Fidelity, normalizeRepo(e.Repo), e.IdempotencyKey, e.SessionID,
		stampPriceVersion(e.PriceVersion), normalizeHost(e.Host), normalizeBillingMode(e.BillingMode), e.Timestamp,
	}
}

// InsertTokenEvent writes a single token event. Non-empty IdempotencyKey makes
// the insert idempotent — duplicate inserts return nil without re-inserting.
func (d *DB) InsertTokenEvent(ctx context.Context, e TokenEvent) error {
	_, err := d.db.ExecContext(ctx, insertTokenEventSQL, insertTokenEventArgs(e)...)
	return err
}

// ErrCostConflict reports that a keyed insert on the manual-import surface
// (POST /api/v1/costs) collided with an existing row that carries the SAME
// idempotency_key but a DIFFERENT cost_micro. It is returned by
// InsertManualCostEvent, which leaves the stored row entirely unchanged — the
// caller is expected to surface this as an HTTP 409 (#295). Comparison is on the
// stored integer cost_micro (exact-equal on micros), so an honest retry whose
// float cost_usd rounds to the same micro value is NOT a conflict.
var ErrCostConflict = errors.New("idempotency_key already recorded with a different cost")

// InsertManualCostEvent is InsertTokenEvent for the untrusted manual-import
// surface (POST /api/v1/costs). It preserves the existing idempotent behavior —
// a re-post whose cost_micro exactly matches the stored value is a no-op that
// MAX-merges token counts — but FAILS LOUD instead of silently first-writer-wins
// dropping a CHANGED cost: when a row already exists under the same
// idempotency_key with a different cost_micro, it writes NOTHING and returns
// ErrCostConflict (#295). cost_micro remains immutable (#233): a 409 rejects, it
// never overwrites. An unkeyed insert cannot collide, so it takes the plain path.
//
// The pre-check and the insert run in one transaction, and under
// SetMaxOpenConns(1) (see Open) that transaction owns the store's single
// connection for its whole lifetime — so no other in-process writer can slip a
// row in between the SELECT and the INSERT. That serialization, NOT the deferred
// transaction on its own (a nil-tx reads a snapshot and only takes the write lock
// at the INSERT), is what makes the divergence decision atomic; this is the same
// read-then-write-in-a-nil-tx pattern UpdateQuality relies on. On ErrCostConflict
// the deferred Rollback leaves the stored row — cost_micro AND token counts —
// untouched.
func (d *DB) InsertManualCostEvent(ctx context.Context, e TokenEvent) error {
	if e.IdempotencyKey == "" {
		return d.InsertTokenEvent(ctx, e)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var stored int64
	err = tx.QueryRowContext(ctx,
		`SELECT cost_micro FROM token_events WHERE idempotency_key = ?`,
		e.IdempotencyKey,
	).Scan(&stored)
	switch {
	case err == nil:
		// A row already owns this key. Exact-equal on the stored integer micros:
		// an identical re-post falls through to the idempotent upsert; a divergent
		// cost fails loud without mutating the row.
		if stored != e.CostMicro {
			return ErrCostConflict
		}
	case errors.Is(err, sql.ErrNoRows):
		// Brand-new key — first writer. Fall through to the insert.
	default:
		return err
	}

	if _, err := tx.ExecContext(ctx, insertTokenEventSQL, insertTokenEventArgs(e)...); err != nil {
		return err
	}
	return tx.Commit()
}

// InsertTokenEvents bulk-inserts token events within a single transaction.
func (d *DB) InsertTokenEvents(ctx context.Context, events []TokenEvent) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, insertTokenEventSQL)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, e := range events {
		if _, err := stmt.ExecContext(ctx, insertTokenEventArgs(e)...); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// RepriceOptions parameterizes a Reprice run (#294).
type RepriceOptions struct {
	// FromVersion selects the rows to reprice: every token_events row with
	// price_version >= FromVersion. It is REQUIRED to be >= 1 so a whole-table
	// reprice is never a default or an accident — the operator names the floor.
	FromVersion int
	// Commit gates the mutation. False (the default) is a DRY RUN: the result is
	// computed exactly as a commit would but NOTHING is written — no cost_micro
	// update, no price_version bump, no audit row. True applies the reprice and
	// writes the audit ledger in a single transaction.
	Commit bool
	// ToolVersion stamps the audit row with the binary identity that ran the
	// reprice (provenance: "which build rewrote this history"). Empty is allowed
	// (an unstamped dev build) and stored verbatim.
	ToolVersion string
	// AllowGuessed gates a Commit that would rewrite any real historical cost to a
	// size-class GUESS estimate (a model with no exact entry in the active price
	// table — GuessedRowCount > 0). False (the default) makes such a Commit FAIL
	// and mutate nothing: rewriting audited history to an estimate is destructive
	// and must be opted into explicitly. True proceeds (the loud GUESS warning
	// still stands). It has no effect on a dry run, which only ever reports.
	AllowGuessed bool
}

// RepriceVersionDelta is the per-old-price-version breakdown of a reprice: how
// many rows carried that version and how their summed cost shifted once repriced
// under the active table (#294). NewPriceVersion is the active table version they
// move to; OldPriceVersion == NewPriceVersion is legitimate (a row already at the
// active version whose cost is reconciled, e.g. the placeholder-promotion edge).
type RepriceVersionDelta struct {
	OldPriceVersion int
	NewPriceVersion int
	RowCount        int64
	OldCostMicroSum int64
	NewCostMicroSum int64
}

// RepriceResult reports what a reprice did (Committed) or WOULD do (dry run). The
// aggregate totals and the per-version breakdown are identical in both modes —
// the dry run runs the SAME selection and pricing the commit would, so its
// printed numbers are exactly what a subsequent --commit applies.
type RepriceResult struct {
	FromVersion   int    // the --from-version N floor that was requested
	ToVersion     int    // active price-table version every repriced row moves to
	EffectiveDate string // active table effective_date (provenance)
	Committed     bool   // true = the reprice was applied; false = dry run, nothing written

	RowCount        int64 // rows examined (price_version >= FromVersion)
	ChangedRowCount int64 // subset whose cost_micro/price_version/billing_mode actually changed
	OldCostMicroSum int64 // SUM(cost_micro) over the CHANGED rows, before
	NewCostMicroSum int64 // SUM(cost_micro) over the CHANGED rows, after (net delta = New - Old)
	// GuessedRowCount is the subset of CHANGED rows whose (host, model) has NO
	// exact entry in the active price table, so the recompute priced them at the
	// self-hosted GUESS fallback rather than an audited rate (#267/#294). A model
	// dropped from the table since the row was first priced lands here — its
	// audited historical cost would be rewritten to an estimate — so the caller
	// surfaces this loudly before an operator commits.
	GuessedRowCount int64

	// ByOldVersion is the per-old-price-version breakdown, ascending by
	// OldPriceVersion. One entry per distinct version among the CHANGED rows
	// (unchanged version groups are omitted), so a genuine no-op yields none.
	ByOldVersion []RepriceVersionDelta
	// Developers is the sorted set of distinct developers owning a CHANGED row —
	// the "who is affected" the dry run surfaces before an operator commits.
	Developers []string
	// GuessedModels is the sorted set of distinct "model" / "model@host" identifiers
	// among the CHANGED rows that priced at the GUESS fallback — the exact models an
	// operator must add to the price table (or knowingly override with
	// --allow-guessed). Empty when GuessedRowCount is 0. It names WHAT to act on, so
	// the guess warning and the --allow-guessed gate are actionable, not just a count.
	GuessedModels []string
	// RepriceID is the audit-ledger correlation token minted for a committed run
	// (empty on a dry run). Every reprice_audit row of this run carries it.
	RepriceID string
}

// repriceCandidate columns pulled per row for the recompute. Kept minimal — only
// what ComputeCostHost needs plus the identity/provenance fields — because the
// candidate set can be large (a window over the whole priced history).
const repriceSelectSQL = `
SELECT id, developer, host, model, input_tok, output_tok, cache_read,
       cache_write_5m, cache_write_1h, cost_micro, price_version, billing_mode
FROM token_events
WHERE price_version >= ?
ORDER BY id`

// Reprice recomputes token_events.cost_micro for every row priced at
// price_version >= opts.FromVersion using the CURRENTLY-ACTIVE price table, bumps
// those rows to the active version, and (on commit) writes an audit ledger of the
// change (#294). It is the ONLY sanctioned mutator of a priced cost — the normal
// insert path holds cost_micro immutable on replay (#233) — because a reprice
// RETROACTIVELY changes historical TIER scores and must be explicit and audited.
//
// SAFE BY DEFAULT: with opts.Commit false it is a DRY RUN — it computes the full
// result (row count, before/after cost totals, per-version breakdown, affected
// developers) and writes NOTHING. Only opts.Commit applies the change, and it
// does so atomically in a single transaction: the per-row cost/version updates,
// the per-row before-images (reprice_row_audit), AND the aggregate audit rows
// (reprice_audit) commit together or not at all, so a failure mid-run leaves no
// partial reprice and no orphan before-image.
//
// AUDIT: a committed run records BOTH ledgers in the same transaction — the
// aggregate reprice_audit (one row per old price_version, the summed shift) and
// the per-row reprice_row_audit (one before-image per changed row: its exact old
// cost_micro/price_version/billing_mode, keyed by token_event_id, the substrate
// for a row-grain inverse-undo). See the reprice_row_audit schema comment for the
// full mechanism.
//
// GUESS GATE: if any CHANGED row would be repriced to a size-class GUESS estimate
// (GuessedRowCount > 0), a Commit FAILS and mutates nothing UNLESS
// opts.AllowGuessed is set — rewriting audited history to an estimate must be
// opted into. A dry run is never gated; it only reports the guessed count.
//
// A row is "changed" when its recomputed cost differs from the stored cost, OR its
// version is below the active one (a version-only bump is still a provenance change
// worth recording), OR its billing_mode is re-resolved differently — all three are
// rewritten together on commit, so all three gate the change. Repricing rows already
// priced under the active table with a cost, version, and billing_mode that all
// recompute identically is a genuine no-op (ChangedRowCount 0) — the reconciliation
// this exists for is the placeholder-promotion edge, where a row at the active
// version nonetheless carries an under-priced first-writer cost.
//
// GuessedRowCount reports how many CHANGED rows priced at the self-hosted GUESS
// fallback (their model has no exact active-table entry, e.g. a model dropped since
// the row was first priced), so a caller can warn loudly before the reprice rewrites
// audited history to an estimate.
func (d *DB) Reprice(ctx context.Context, opts RepriceOptions) (RepriceResult, error) {
	if opts.FromVersion < 1 {
		return RepriceResult{}, fmt.Errorf("reprice: from-version must be >= 1, got %d (refusing to reprice the whole table by accident)", opts.FromVersion)
	}
	info := ActivePriceTableInfo()
	res := RepriceResult{
		FromVersion:   opts.FromVersion,
		ToVersion:     info.Version,
		EffectiveDate: info.EffectiveDate,
	}

	// One transaction spans BOTH the read and (on commit) every write, so the dry
	// run reads a consistent snapshot and a committed run is all-or-nothing. The
	// deferred Rollback is a no-op after a successful Commit; on the dry-run path
	// it is what discards the (read-only) transaction.
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return RepriceResult{}, fmt.Errorf("reprice: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// change is a row whose cost and/or version the commit will write. Only
	// changed rows are buffered; unchanged rows contribute to the aggregates and
	// are then dropped, bounding memory to the mutated subset. The old* fields
	// carry the row's pre-reprice values so the commit writes the before-image
	// (reprice_row_audit) without re-reading the already-overwritten row.
	type change struct {
		id         int64
		newCost    int64
		newBilling string
		newVersion int
		oldCost    int64
		oldVersion int
		oldBilling string
	}
	var changes []change
	deltaByVersion := map[int]*RepriceVersionDelta{}
	devSet := map[string]struct{}{}
	guessedSet := map[string]struct{}{}

	rows, err := tx.QueryContext(ctx, repriceSelectSQL, opts.FromVersion)
	if err != nil {
		return RepriceResult{}, fmt.Errorf("reprice: select candidates: %w", err)
	}
	for rows.Next() {
		var (
			id                               int64
			developer, host, model           string
			inTok, outTok, cRead, cw5m, cw1h int
			oldCost                          int64
			oldVersion                       int
			oldBilling                       string
		)
		if err := rows.Scan(&id, &developer, &host, &model, &inTok, &outTok, &cRead,
			&cw5m, &cw1h, &oldCost, &oldVersion, &oldBilling); err != nil {
			_ = rows.Close()
			return RepriceResult{}, fmt.Errorf("reprice: scan candidate: %w", err)
		}
		newCost, newBilling := ComputeCostHost(host, model, CostUsage{
			Input:        inTok,
			Output:       outTok,
			CacheRead:    cRead,
			CacheWrite5m: cw5m,
			CacheWrite1h: cw1h,
		})
		newBilling = normalizeBillingMode(newBilling)

		res.RowCount++ // every examined row (price_version >= FromVersion)

		// Only rows that actually move are recorded. A row is unchanged when its
		// cost recomputes identically, it is already at the active version, AND its
		// billing_mode is unchanged — all three are rewritten together on commit, so
		// all three gate the skip (a zero-cost row can change billing_mode without a
		// cost delta). Including an unchanged row in the audit/sums would misstate
		// the mutation, so the breakdown, the affected-developer set, and the
		// before/after sums cover the CHANGED rows exclusively — making
		// NewCostMicroSum - OldCostMicroSum the exact net change to stored spend.
		if newCost == oldCost && oldVersion == info.Version && newBilling == oldBilling {
			continue
		}
		res.ChangedRowCount++
		res.OldCostMicroSum += oldCost
		res.NewCostMicroSum += newCost
		devSet[developer] = struct{}{}
		// A changed row whose (host, model) has no exact table entry was repriced by
		// the self-hosted GUESS fallback, not an audited rate — count it AND record
		// the distinct model so the caller can warn (and the gate can fail) naming
		// exactly WHAT to add to the price table before this rewrites audited history.
		if !modelIsExactHost(host, model) {
			res.GuessedRowCount++
			guessedSet[guessedModelKey(host, model)] = struct{}{}
		}

		delta := deltaByVersion[oldVersion]
		if delta == nil {
			delta = &RepriceVersionDelta{OldPriceVersion: oldVersion, NewPriceVersion: info.Version}
			deltaByVersion[oldVersion] = delta
		}
		delta.RowCount++
		delta.OldCostMicroSum += oldCost
		delta.NewCostMicroSum += newCost

		changes = append(changes, change{
			id: id, newCost: newCost, newBilling: newBilling, newVersion: info.Version,
			oldCost: oldCost, oldVersion: oldVersion, oldBilling: oldBilling,
		})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return RepriceResult{}, fmt.Errorf("reprice: iterate candidates: %w", err)
	}
	// Close the cursor BEFORE issuing any UPDATE: the transaction holds a single
	// connection and database/sql cannot run a write on it while a read cursor is
	// still open.
	if err := rows.Close(); err != nil {
		return RepriceResult{}, fmt.Errorf("reprice: close candidate cursor: %w", err)
	}

	res.ByOldVersion = sortedDeltas(deltaByVersion)
	res.Developers = sortedKeys(devSet)
	res.GuessedModels = sortedKeys(guessedSet)

	if !opts.Commit {
		// Dry run: report only. The deferred Rollback discards the read tx.
		return res, nil
	}

	// GUESS gate: a commit that would rewrite real historical cost to a size-class
	// GUESS estimate (a changed row whose model has no exact active-table entry)
	// must be opted into explicitly. Without AllowGuessed this FAILS before any
	// write, mutating nothing — the deferred Rollback discards the read-only tx —
	// so audited history is never silently rewritten to an estimate. A dry run
	// never reaches here; it only reports the same GuessedRowCount as a warning.
	if res.GuessedRowCount > 0 && !opts.AllowGuessed {
		return RepriceResult{}, fmt.Errorf("reprice: refusing to commit: %d of %d changed row(s) would be repriced to a self-hosted GUESS estimate (model(s) not in the active price table: %s) — audited historical cost would be rewritten to an estimate. Add the missing model(s) to the price table for an accurate reprice, or re-run with --allow-guessed to proceed anyway", res.GuessedRowCount, res.ChangedRowCount, strings.Join(res.GuessedModels, ", "))
	}

	// A commit with nothing to change writes no rows and no audit — there is no
	// mutation to record. Committed reflects that the --commit request was honored;
	// RepriceID stays empty because no audit row was written (it is non-empty IFF
	// the ledger gained rows). The deferred Rollback discards the empty read tx.
	if len(changes) == 0 {
		res.Committed = true
		return res, nil
	}

	repriceID, err := newRepriceID()
	if err != nil {
		return RepriceResult{}, fmt.Errorf("reprice: mint reprice id: %w", err)
	}

	// Apply the per-row cost/version/billing_mode updates. cost_micro moves in
	// lockstep with price_version (and billing_mode, re-resolved from the same
	// table) so the row's price provenance stays coherent. newBilling was already
	// normalized during the scan.
	for _, c := range changes {
		// Capture the per-row BEFORE-IMAGE first, from the buffered pre-reprice
		// values (NOT a re-read — the UPDATE below overwrites them). This is the
		// durable record a surgical inverse-undo restores from, and it commits or
		// rolls back in lockstep with the UPDATE and the aggregate ledger.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO reprice_row_audit
			    (reprice_id, token_event_id, old_cost_micro, old_price_version, old_billing_mode)
			 VALUES (?, ?, ?, ?, ?)`,
			repriceID, c.id, c.oldCost, c.oldVersion, c.oldBilling,
		); err != nil {
			return RepriceResult{}, fmt.Errorf("reprice: write row before-image (row %d): %w", c.id, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE token_events SET cost_micro = ?, price_version = ?, billing_mode = ? WHERE id = ?`,
			c.newCost, c.newVersion, c.newBilling, c.id,
		); err != nil {
			return RepriceResult{}, fmt.Errorf("reprice: update row %d: %w", c.id, err)
		}
	}

	// Write one audit row per distinct old price_version touched — the durable
	// record of what this run changed, keyed by the shared reprice_id.
	for _, del := range res.ByOldVersion {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO reprice_audit
			    (reprice_id, from_version, old_price_version, new_price_version,
			     row_count, old_cost_micro_sum, new_cost_micro_sum,
			     price_effective_date, tool_version)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			repriceID, opts.FromVersion, del.OldPriceVersion, del.NewPriceVersion,
			del.RowCount, del.OldCostMicroSum, del.NewCostMicroSum,
			info.EffectiveDate, opts.ToolVersion,
		); err != nil {
			return RepriceResult{}, fmt.Errorf("reprice: write audit row (old_version %d): %w", del.OldPriceVersion, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return RepriceResult{}, fmt.Errorf("reprice: commit: %w", err)
	}
	committed = true
	res.Committed = true
	res.RepriceID = repriceID
	return res, nil
}

// sortedDeltas flattens the per-version accumulator into a slice ordered ascending
// by OldPriceVersion, so the reported breakdown and the audit rows are written in
// a stable, human-scannable order.
func sortedDeltas(m map[int]*RepriceVersionDelta) []RepriceVersionDelta {
	out := make([]RepriceVersionDelta, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OldPriceVersion < out[j].OldPriceVersion })
	return out
}

// sortedKeys returns the map's keys sorted ascending — the distinct affected
// developers, in a stable order for reporting.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// guessedModelKey renders a guessed row's model for the operator-facing warning
// and gate: "model" for an unhosted model (empty or the HostUnknown sentinel, which
// is not an actionable host), "model@host" when a real host qualifies it — the same
// identity an operator uses to add an entry to the price table. Deduped in a set, so
// each distinct model is named once.
func guessedModelKey(host, model string) string {
	if host == "" || host == HostUnknown {
		return model
	}
	return model + "@" + host
}

// newRepriceID mints the random hex token that correlates every reprice_audit row
// of one operation. crypto/rand makes it collision-free across runs without a
// sequence column; a rand failure is surfaced (the reprice aborts) rather than
// silently writing an unkeyed audit.
func newRepriceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Outcome is a resolved PR or closed issue with a measured outcome.
//
// MergeCommitSHA holds the squash/merge commit hash GitHub created when this
// PR was merged. Stored here so the revert detector can look up the
// originating outcome by parsing "This reverts commit <sha>" out of a future
// revert commit's message (#20). Empty for outcomes created before merge_
// commit_sha existed or from sources that don't have a SHA.
// Weight-source provenance values recorded on outcomes (#132). The weight now
// lives on one 0.5-8 scale regardless of source, but reports still need to tell
// a label-derived weight from a heuristic-derived one — a developer whose
// points are entirely heuristic carries no size labels and less-trustworthy
// weights.
const (
	// WeightSourceLabel — weight came from a GitHub size label (labelWeight).
	WeightSourceLabel = "label"
	// WeightSourceHeuristic — weight came from the diff-size fallback (gitHeuristic).
	WeightSourceHeuristic = "git-heuristic"
	// WeightSourceLegacy — pre-#132 row; scale unknowable and un-recomputable
	// because the old heuristic discarded its raw inputs. Never written by new
	// code — only the column DEFAULT and read-side COALESCE produce it.
	WeightSourceLegacy = "legacy"
	// WeightSourcePush — push-captured direct commit (#196, RULING B). The weight is
	// the degraded 0.5 heuristic floor (GitHeuristic(0,0)): a GitHub push payload
	// carries no diff stats and the webhook has no local clone, so 0.5 is the honest
	// floor with NO outbound GitHub API dependency. Distinct from WeightSourceHeuristic
	// so a report can tell a *measured* tiny diff (0.5 from a real 1-line PR) apart
	// from this *capture-fidelity* floor — the two must not be pooled in trust terms.
	// A future batch enricher may upgrade this to 'push_enriched' (Option C, deferred).
	WeightSourcePush = "push"
)

// Attribution-provenance values recorded on outcomes.source (#188), extending
// the #34/#82 provenance discipline from token events to outcomes. The value
// tells a forged/manual outcome apart from a webhook-derived one in audit.
const (
	// OutcomeSourceGitHubWebhook — outcome recorded by the signature-verified
	// GitHub webhook (internal/webhook). Also the column DEFAULT, so pre-#188
	// rows and any outcome inserted with an empty Source read back as this.
	OutcomeSourceGitHubWebhook = "github-webhook"
	// OutcomeSourceAPI — outcome recorded via the bearer-gated, provider-neutral
	// POST /api/v1/outcomes (#188), the path GitLab/Bitbucket/Gitea CI use. The
	// bearer token is the only authenticator here (no per-provider signature), so
	// this source marks an outcome whose authenticity rests on token custody.
	OutcomeSourceAPI = "api-outcome"
	// OutcomeSourcePush — outcome captured from a direct commit to the default
	// branch by the signature-verified GitHub push webhook (#196). Segments the
	// degraded, per-issue-per-UTC-day push grain from PR-grain outcomes so scoring
	// views can filter/segment on source. Push outcomes carry a NULL merge_commit_sha
	// and a non-null push_day; their idempotency rests on the (issue_id, push_day)
	// partial unique index rather than the merge-commit UNIQUE the PR path uses.
	OutcomeSourcePush = "push"
)

// Work-type taxonomy (#187). The FIXED set of work CATEGORIES an outcome can
// belong to. It exists because an output-per-token metric with only a feature
// notion scores security / SRE / on-call / research work near zero: their tokens
// are high and their "shipped feature" points are low, so their TIER craters
// against feature developers. Segmenting outcomes by type lets a security
// engineer be compared WITHIN security, never against feature work — comparing a
// security TIER to a feature TIER is a category error and is UNSUPPORTED. This set
// is closed: every ingress (webhook label, POST /api/v1/outcomes, migration
// backfill) validates against it, and adding a member is a schema+docs change, not
// a config toggle.
const (
	WorkTypeFeature    = "feature"
	WorkTypeBug        = "bug"
	WorkTypeSecurity   = "security"
	WorkTypeIncident   = "incident"
	WorkTypeTechDebt   = "tech-debt"
	WorkTypeResearch   = "research"
	WorkTypeCompliance = "compliance"
)

// canonicalWorkTypes is the taxonomy in its canonical order. It is the single
// source of truth ValidWorkType and WorkTypeList read, so the enum lives in one
// place. Order here is the DECLARATION order (used for the human-readable list);
// the label-derivation PRECEDENCE (a different, impact-ranked order) lives next to
// the webhook helper that needs it, deliberately not conflated with this list.
var canonicalWorkTypes = []string{
	WorkTypeFeature, WorkTypeBug, WorkTypeSecurity, WorkTypeIncident,
	WorkTypeTechDebt, WorkTypeResearch, WorkTypeCompliance,
}

// ValidWorkType reports whether s is exactly one of the canonical work types. It
// is the shared trust-boundary check every ingress uses (webhook label mapping,
// POST /api/v1/outcomes validation), so an invalid category can never reach a
// stored row — the column would accept any TEXT, so validation is enforced in Go,
// fail-loud, not by the schema.
func ValidWorkType(s string) bool {
	for _, wt := range canonicalWorkTypes {
		if s == wt {
			return true
		}
	}
	return false
}

// WorkTypeList renders the taxonomy as a comma-separated string for error
// messages (e.g. a 400 on an invalid POST /api/v1/outcomes work_type).
func WorkTypeList() string { return strings.Join(canonicalWorkTypes, ", ") }

// Work-type provenance values recorded on outcomes.work_type_source (#187),
// mirroring the weight_source (#132) discipline: a report must be able to tell a
// label-derived category from an API-asserted one from a mere default.
const (
	// WorkTypeSourceLabel — work_type derived from a PR label (workTypeFromLabels).
	WorkTypeSourceLabel = "label"
	// WorkTypeSourceAPI — work_type set explicitly via POST /api/v1/outcomes.
	WorkTypeSourceAPI = "api"
	// WorkTypeSourceDefault — no label and no API value on a LIVE insert, so
	// work_type fell back to 'feature'. Distinct from 'legacy' (which is only ever
	// the migration/read-COALESCE value for pre-#187 rows) so a defaulted new row is
	// never confused with a pre-taxonomy one.
	WorkTypeSourceDefault = "default"
	// WorkTypeSourceLegacy — pre-#187 row backfilled by the migration; its category
	// is unknowable. Never written by new code — only the column DEFAULT and the
	// read-side COALESCE produce it (mirrors WeightSourceLegacy).
	WorkTypeSourceLegacy = "legacy"
)

// MaxOutcomeWeight is the largest weight the label scale assigns (the "xl"
// bucket, gitHeuristic's top bucket, and labelWeight's "size/xl"). It is the
// upper bound POST /api/v1/outcomes accepts for a client-supplied
// label-equivalent weight: a value above the largest label is not
// "label-equivalent" and would let a caller inflate points past any real PR, so
// it is rejected at the trust boundary (fail-loud, mirroring the cost caps).
const MaxOutcomeWeight = 8.0

// GitHeuristic maps diff size onto the 0.5/1/3/5/8 label scale so an unlabeled
// PR is commensurate with a labeled one (#132/C1). It is the SINGLE source of
// truth for the diff-size fallback: both the GitHub webhook and the
// provider-neutral POST /api/v1/outcomes (#188) resolve weight through it (the
// latter via ResolveWeight), so a score is identical no matter how the outcome
// arrived. The buckets:
//
//	effort <=   15 -> 0.5  (xs: one-line typo = 1 + 10 = 11)
//	effort <=   60 -> 1.0  (s:  30 lines / 2 files = 50)
//	effort <=  200 -> 3.0  (m:  50 lines / 3 files = 80; 100/4 = 140)
//	effort <= 1000 -> 5.0  (l:  200 lines / 5 files = 250; 500/5 = 550)
//	effort >  1000 -> 8.0  (xl: 1000 lines / 10 files = 1100)
//
// where effort = linesChanged + 10*filesChanged (files proxy breadth). See the
// #132 history in internal/webhook for why this replaced the old log2 formula.
func GitHeuristic(linesChanged, filesChanged int) float64 {
	effort := linesChanged + filesChanged*10
	switch {
	case effort <= 15:
		return 0.5
	case effort <= 60:
		return 1.0
	case effort <= 200:
		return 3.0
	case effort <= 1000:
		return 5.0
	default:
		return 8.0
	}
}

// ResolveWeight picks an outcome's weight and its provenance from a
// label-derived (or, on POST /api/v1/outcomes, client-supplied label-equivalent)
// weight and the raw diff stats. A non-zero explicitWeight wins and is recorded
// as WeightSourceLabel; otherwise the diff-size GitHeuristic fallback is used and
// recorded as WeightSourceHeuristic. This is the shared branching both the GitHub
// webhook and the provider-neutral endpoint call, so the two paths cannot drift:
// given the same effective inputs they yield the same (weight, source).
func ResolveWeight(explicitWeight float64, additions, deletions, changedFiles int) (float64, string) {
	if explicitWeight != 0 {
		return explicitWeight, WeightSourceLabel
	}
	return GitHeuristic(additions+deletions, changedFiles), WeightSourceHeuristic
}

type Outcome struct {
	// ID is the outcomes.id primary key. Populated only by the read queries
	// that need row-level targeting (OutcomeByMergeCommit, LatestOutcomeByIssue)
	// for UpdateQualityForOutcome (#134); zero on rows built by callers or by
	// reads that don't select it.
	ID        int64
	Developer string
	IssueID   string
	PRNumber  int
	Weight    float64
	// WeightSource is one of WeightSource{Label,Heuristic,Legacy} (#132). Reads
	// COALESCE a NULL/absent column to "legacy" so pre-migration rows are
	// always classified.
	WeightSource   string
	Quality        float64
	MergeCommitSHA string
	// Additions/Deletions/ChangedFiles are the raw PR diff stats retained for
	// future re-scoring (#132). Zero on legacy rows (stored NULL, read via
	// COALESCE) — indistinguishable from a genuine zero-diff PR, which is
	// acceptable since legacy rows are never re-scored anyway.
	Additions    int
	Deletions    int
	ChangedFiles int
	// Source is the attribution provenance: one of OutcomeSource{GitHubWebhook,
	// API} (#188). Reads COALESCE a NULL/absent column to OutcomeSourceGitHubWebhook
	// so pre-migration rows are always classified; InsertOutcome coerces an empty
	// value to the same default so a caller that predates #188 (the webhook, which
	// leaves it unset) lands as a GitHub-webhook outcome.
	Source string
	// WorkType is the work category (#187): one of the WorkType* constants. Reads
	// COALESCE a NULL/absent column to WorkTypeFeature so pre-migration rows are
	// always classified; InsertOutcome coerces an empty value to the same default.
	WorkType string
	// WorkTypeSource is the provenance of WorkType (#187): one of WorkTypeSource*.
	// Reads COALESCE a NULL/absent column to WorkTypeSourceLegacy (pre-#187 rows);
	// InsertOutcome coerces an empty value to WorkTypeSourceDefault (a live insert
	// that resolved no type is a default, not a legacy row).
	WorkTypeSource string
	// Repo is the canonical "owner/repo" this outcome was earned in (#231), or
	// repoid.Unqualified for pre-#231 rows and producers that cannot determine it.
	// InsertOutcome coerces an empty value to the sentinel. It is the leading column
	// of idx_outcomes_push_daily_repo, so two repos' same-numbered issues no longer
	// collide, and it is the disambiguator in the tolerant cost<->outcome join.
	Repo string
	// PushDay is the UTC calendar day ('YYYY-MM-DD') a source='push' outcome
	// aggregates to — the leading (with repo, issue_id) column of the
	// idx_outcomes_push_daily_repo dedup index (#196). It is EMPTY ("") for every
	// non-push row (the column is NULL there). Populated by the bulk export read
	// (ListOutcomes) so a BI/reconciliation consumer can see the per-issue-per-day
	// aggregation key a push row is deduped on (#242); left zero by other reads.
	PushDay   string
	Timestamp time.Time
}

// InsertOutcome writes a single outcome record. inserted reports whether a row
// was actually written: false means the merge_commit_sha already existed and
// ON CONFLICT DO NOTHING skipped the write. The unique index — not any
// read-before-insert — is therefore the authoritative dedup signal, so a caller
// can distinguish a fresh insert from a replay even under a concurrent race
// (#188). A NULL SHA is outside the partial index and always inserts, so it
// reports inserted=true.
func (d *DB) InsertOutcome(ctx context.Context, o Outcome) (inserted bool, err error) {
	// NULLIF empties the SHA to SQL NULL so the partial index on
	// merge_commit_sha never carries the empty string — same pattern as
	// idempotency_key on token_events.
	//
	// ON CONFLICT DO NOTHING (#60): a second insert with the same
	// merge_commit_sha is a webhook replay/redelivery — silently keep the
	// first row. The DB constraint is the atomic dedup boundary. NULL SHAs (no
	// merge commit in the payload) are outside the partial index and
	// insert unconditionally, as before.
	// weight_source defaults to "label"/"git-heuristic" from the handler; an
	// empty value (a caller that predates #132) is coerced to "legacy" so the
	// NOT NULL column never rejects the insert.
	weightSource := o.WeightSource
	if weightSource == "" {
		weightSource = WeightSourceLegacy
	}
	// Coerce an empty Source to the GitHub-webhook default so the webhook (which
	// leaves Source unset) lands as 'github-webhook', matching the column DEFAULT
	// and the read-side COALESCE. The provider-neutral endpoint always sets
	// OutcomeSourceAPI explicitly (#188).
	source := o.Source
	if source == "" {
		source = OutcomeSourceGitHubWebhook
	}
	// work_type/work_type_source default to a resolved value from the handler; an
	// empty pair (a caller that predates #187) is coerced to the 'feature'/'default'
	// live-insert baseline so the NOT NULL columns never reject the insert. 'default'
	// (not 'legacy') is used here: a live insert that resolved no category is a
	// default, whereas 'legacy' is reserved for the migration/read-COALESCE of truly
	// pre-#187 rows.
	workType := o.WorkType
	if workType == "" {
		workType = WorkTypeFeature
	}
	workTypeSource := o.WorkTypeSource
	if workTypeSource == "" {
		workTypeSource = WorkTypeSourceDefault
	}
	res, err := d.db.ExecContext(ctx, `
		INSERT INTO outcomes (developer, issue_id, pr_number, weight, quality, merge_commit_sha,
		                      weight_source, additions, deletions, changed_files, source,
		                      work_type, work_type_source, repo, ts)
		VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`,
		o.Developer, o.IssueID, o.PRNumber, o.Weight, o.Quality, o.MergeCommitSHA,
		weightSource, o.Additions, o.Deletions, o.ChangedFiles, source,
		workType, workTypeSource, normalizeRepo(o.Repo), o.Timestamp,
	)
	if err != nil {
		return false, err
	}
	// RowsAffected is 0 when ON CONFLICT DO NOTHING skipped the write, 1 on a
	// real insert. modernc.org/sqlite reports this reliably for INSERT.
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpsertPushOutcome idempotently records AT MOST ONE push-captured outcome per
// (repo, issue_id, UTC calendar day) (#196 RULING B, repo-qualified by #231 — before
// that, two repos pushing to their own issue #42 on the same day collided and the
// second repo's outcome was silently dropped). day is the 'YYYY-MM-DD' UTC day
// the caller derived from the commit timestamp (compute it in UTC — do NOT hand it
// a local-zone date). inserted reports whether a NEW row was written: false means an
// outcome for this (issue_id, day) already existed and ON CONFLICT DO NOTHING skipped
// the write.
//
// Aggregation-not-summation is the whole point: the conflict is resolved DO NOTHING,
// so a second qualifying commit on the same issue that day — or a replay of the same
// push delivery — leaves the single 0.5-weight outcome untouched. Summing 0.5×N would
// re-open the commit-splitting inflation vector this grain exists to close. The
// (issue_id, push_day) partial unique index (WHERE source='push') is the atomic dedup
// boundary, so idempotency holds even under a concurrent race, exactly as the
// merge_commit_sha UNIQUE backs InsertOutcome.
//
// The row is written with source='push' and merge_commit_sha left NULL: a push
// outcome aggregates several commits and has no single merge commit, so it lives
// entirely outside the #60 merge-commit UNIQUE and cannot collide with a PR outcome.
func (d *DB) UpsertPushOutcome(ctx context.Context, o Outcome, day string) (inserted bool, err error) {
	if day == "" {
		// Fail loud: a NULL push_day would make the partial unique index treat every
		// push row as distinct (SQLite NULLs compare unequal), silently defeating the
		// one-outcome-per-day guard. The caller must always supply a UTC day.
		return false, errors.New("store: UpsertPushOutcome requires a non-empty UTC day (YYYY-MM-DD)")
	}
	weightSource := o.WeightSource
	if weightSource == "" {
		weightSource = WeightSourcePush
	}
	// source is forced to 'push' regardless of o.Source: the ON CONFLICT target below
	// is the partial index WHERE source='push', so a row inserted under any other
	// source would silently bypass the daily-dedup guard.
	//
	// work_type is forced to 'feature'/'default' (#187): a push captures a bare
	// default-branch commit, which carries no PR labels and no API-supplied category,
	// so there is nothing to derive a type from — 'feature'/'default' is the honest
	// baseline, identical to a labelless PR insert.
	res, err := d.db.ExecContext(ctx, `
		INSERT INTO outcomes (developer, issue_id, weight, quality, weight_source, source,
		                      work_type, work_type_source, push_day, repo, ts)
		VALUES (?, ?, ?, ?, ?, 'push', 'feature', 'default', ?, ?, ?)
		ON CONFLICT (repo, issue_id, push_day) WHERE source = 'push' DO NOTHING`,
		o.Developer, o.IssueID, o.Weight, o.Quality, weightSource, day, normalizeRepo(o.Repo), o.Timestamp,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpdateQuality sets the quality multiplier for every outcome matching
// (developer, issueID) and appends one quality_history row per affected outcome
// so no quality mutation is ever silent (#137). Same signature and issue-wide
// scope as before — all existing callers keep working — but the write now runs
// in a transaction: SELECT the affected (id, quality), UPDATE, then INSERT the
// history rows with reason 'legacy-update-quality'.
//
// A no-op write (new quality equal to old) still appends a history row: the
// audit trail records the fact that a write occurred, not only that a value
// changed. If no outcome matches, nothing is written.
//
// NOTE(#134): superseded for event-derived paths by UpdateQualityForOutcome
// (WHERE id = ?), which targets the specific reverted / CI-failed row instead of
// every outcome on the issue (the §C9 scoping fix). New quality writes go through
// that method with the real event_type as the reason; this issue-wide method is
// retained only for back-compat callers and the tests that pin its behavior. Do
// NOT use it for new event-driven degradation — it double-hits sibling outcomes.
func (d *DB) UpdateQuality(ctx context.Context, developer, issueID string, quality float64) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Snapshot the affected rows' old quality BEFORE the update so the history
	// row records the true prior value. Rows are fully drained and closed
	// before the UPDATE runs: under SetMaxOpenConns(1) the tx owns the single
	// connection, and an open cursor on it would block the subsequent Exec.
	rows, err := tx.QueryContext(ctx,
		`SELECT id, quality FROM outcomes WHERE developer = ? AND issue_id = ?`,
		developer, issueID,
	)
	if err != nil {
		return err
	}
	type affected struct {
		id  int64
		old float64
	}
	var affs []affected
	for rows.Next() {
		var a affected
		if err := rows.Scan(&a.id, &a.old); err != nil {
			_ = rows.Close()
			return err
		}
		affs = append(affs, a)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE outcomes SET quality = ? WHERE developer = ? AND issue_id = ?`,
		quality, developer, issueID,
	); err != nil {
		return err
	}

	for _, a := range affs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO quality_history
			    (outcome_id, developer, issue_id, old_quality, new_quality, reason)
			VALUES (?, ?, ?, ?, ?, ?)`,
			a.id, developer, issueID, a.old, quality, legacyUpdateQualityReason,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateQualityForOutcome sets the quality multiplier for exactly ONE outcome
// row (WHERE id = ?) and appends a quality_history row recording the transition
// (#134, P2-03). This is the row-targeted counterpart to UpdateQuality: it fixes
// the §C9 scoping defect where the issue-wide UPDATE degraded every outcome on
// an issue instead of the specific reverted / CI-failed PR.
//
// reason is the driving quality event_type (e.g. "revert_quality", "ci_fail")
// and sourceRef its source_ref, so the append-only history explains WHY quality
// changed and against which signal. Unlike the derived quality itself, this is
// the audit record of the write.
//
// The write runs in a transaction: SELECT the current (developer, issue_id,
// quality), and only if the target quality DIFFERS from the freshly-read current
// value does it UPDATE and append the history row. An unchanged value is a no-op
// (no UPDATE, no history row): quality here is DERIVED and this method is called
// on every event — including replays and clean CI passes — so suppressing
// no-ops keeps quality_history a log of real transitions, not audit noise. (This
// differs deliberately from the issue-wide UpdateQuality, which records every
// call.) The fresh in-tx read — not a caller-supplied snapshot — is what makes a
// replayed or reconciling call self-healing and safe against a stale value. An
// id that matches no row is an error.
func (d *DB) UpdateQualityForOutcome(ctx context.Context, outcomeID int64, quality float64, reason, sourceRef string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var developer, issueID string
	var old float64
	err = tx.QueryRowContext(ctx,
		`SELECT developer, issue_id, quality FROM outcomes WHERE id = ?`, outcomeID,
	).Scan(&developer, &issueID, &old)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("UpdateQualityForOutcome: no outcome with id %d", outcomeID)
	}
	if err != nil {
		return err
	}

	// No-op: the derived value already matches. The floor constants are exact
	// float64 literals round-tripped through SQLite REAL, so equality is
	// reliable. Skipping avoids a 1.0->1.0 history row on every ci_pass/replay.
	if old == quality {
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE outcomes SET quality = ? WHERE id = ?`, quality, outcomeID,
	); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quality_history
		    (outcome_id, developer, issue_id, old_quality, new_quality, reason, source_ref)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		outcomeID, developer, issueID, old, quality, reason, sourceRef,
	); err != nil {
		return err
	}

	return tx.Commit()
}

// OutcomeByMergeCommit returns the outcome whose merge_commit_sha matches the
// given SHA, plus a found flag. Empty sha returns found=false without a
// query. Used by the revert detector to find the original PR's outcome when
// the revert commit's message lacks an issue id but contains a
// "This reverts commit <sha>" footer (#20).
//
// ORDER BY id DESC + LIMIT 1 picks the most recently-inserted row when the
// (non-unique) partial index ever contains duplicates — e.g. if GitHub
// retries a PR webhook delivery and the outcomes table inserts twice. The
// AND merge_commit_sha != '' guard is belt-and-braces: NULLIF in
// InsertOutcome already keeps empty strings out, but a defensive WHERE
// clause means even if a NULL/empty row sneaks in, this query never returns
// it on a non-empty input.
func (d *DB) OutcomeByMergeCommit(ctx context.Context, sha string) (Outcome, bool, error) {
	if sha == "" {
		return Outcome{}, false, nil
	}
	var o Outcome
	err := d.db.QueryRowContext(ctx, `
		SELECT id, developer, issue_id, COALESCE(pr_number, 0), weight, quality,
		       COALESCE(merge_commit_sha, ''), COALESCE(weight_source, 'legacy'),
		       COALESCE(additions, 0), COALESCE(deletions, 0), COALESCE(changed_files, 0),
		       COALESCE(source, 'github-webhook'),
		       COALESCE(work_type, 'feature'), COALESCE(work_type_source, 'legacy'),
		       COALESCE(repo, 'unqualified'), ts
		FROM outcomes
		WHERE merge_commit_sha = ? AND merge_commit_sha != ''
		ORDER BY id DESC
		LIMIT 1`,
		sha,
	).Scan(&o.ID, &o.Developer, &o.IssueID, &o.PRNumber, &o.Weight, &o.Quality,
		&o.MergeCommitSHA, &o.WeightSource, &o.Additions, &o.Deletions, &o.ChangedFiles,
		&o.Source, &o.WorkType, &o.WorkTypeSource, &o.Repo, &o.Timestamp)
	if err == sql.ErrNoRows {
		return Outcome{}, false, nil
	}
	if err != nil {
		return Outcome{}, false, err
	}
	return o, true, nil
}

// LatestOutcomeByIssue returns the most recent outcome row for an issue id within
// a repository, regardless of which developer authored it. Used by the revert
// detector's tier-1 path: the revert commit's message names an issue (because the
// original commit's subject did), and we need to find the original PR's developer
// so the quality penalty lands on them — not on whoever pushed the revert.
//
// #231 — repo is the disambiguator, applied TOLERANTLY (ruling 2). Before it, a
// revert of repo B's issue #42 degraded the quality of repo A's issue #42.
//
// The predicate matches a real repo OR the 'unqualified' sentinel, and ORDER BY
// prefers an exact repo match ahead of a sentinel one. That is deliberate:
//
//   - Strict `repo = ?` would stop finding pre-#231 rows entirely, so every revert
//     against historical data would silently apply no penalty at all — a regression
//     dressed as a fix.
//   - Sentinel-only fallback keeps legacy reverts working, and once the repo has
//     accrued qualified rows they win the ORDER BY, so the tolerance decays to
//     strictness on its own as data rolls forward. No migration, no flag.
//
// Residual, documented rather than hidden: while an issue has ONLY sentinel rows,
// a same-numbered issue in another repo can still route a revert to it. That is the
// pre-#231 behavior, now confined to un-qualified rows instead of being universal.
//
// caller repo == "" (a producer with no repo context) behaves as the sentinel.
//
// NOTE the deliberate asymmetry with RepoMatch: a QUALIFIED caller reaches sentinel
// rows, but a SENTINEL caller reaches only sentinel rows — it does NOT fan out to
// every repo. That is intentional. A repo-blind revert must not pick "whichever repo
// merged this issue number most recently" and penalise it; that is precisely the bug
// #231 fixes. Do not "symmetrize" this with RepoMatch. In production the caller is
// always the push webhook, which has repository.full_name, so the sentinel branch is
// reached only by a malformed payload.
//
// ORDER BY id DESC picks the most recent outcome for that issue, which is what we
// want for the common "PR merged → reverted" timeline. Edge case: an issue with
// multiple successful PRs over time returns the latest; the penalty applies there,
// matching the most-recent-revert intuition.
func (d *DB) LatestOutcomeByIssue(ctx context.Context, repo, issueID string) (Outcome, bool, error) {
	if issueID == "" {
		return Outcome{}, false, nil
	}
	repo = normalizeRepo(repo)
	var o Outcome
	err := d.db.QueryRowContext(ctx, `
		SELECT id, developer, issue_id, COALESCE(pr_number, 0), weight, quality,
		       COALESCE(merge_commit_sha, ''), COALESCE(weight_source, 'legacy'),
		       COALESCE(additions, 0), COALESCE(deletions, 0), COALESCE(changed_files, 0),
		       COALESCE(source, 'github-webhook'), COALESCE(repo, 'unqualified'), ts
		FROM outcomes
		WHERE issue_id = ? AND (repo = ? OR repo = 'unqualified')
		ORDER BY (repo = ?) DESC, id DESC
		LIMIT 1`,
		issueID, repo, repo,
	).Scan(&o.ID, &o.Developer, &o.IssueID, &o.PRNumber, &o.Weight, &o.Quality,
		&o.MergeCommitSHA, &o.WeightSource, &o.Additions, &o.Deletions, &o.ChangedFiles,
		&o.Source, &o.Repo, &o.Timestamp)
	if err == sql.ErrNoRows {
		return Outcome{}, false, nil
	}
	if err != nil {
		return Outcome{}, false, err
	}
	return o, true, nil
}

// UpsertHierarchy sets or updates a developer's team/division/org assignment
// and maintains period_membership (#41), all in one transaction:
//   - first enrollment opens a membership active since '0000-01' (the
//     beginning-of-time sentinel), so the developer is allocated for ALL
//     invoiced periods — including retroactively-posted past invoices — until a
//     departure is recorded. UpsertHierarchy has no true join date, and assuming
//     "member unless told otherwise" matches the pre-#41 all-time behavior;
//   - on an ORG CHANGE (stored org differs from org) the NEW org's membership
//     instead starts this month, and the PRIOR org's open membership is closed
//     effective this month, so past invoices stay with the old org and future
//     ones go to the new org (a one-month overlap in the transition month is
//     accepted);
//   - the NOT-EXISTS guard makes the open-row insert idempotent.
//
// Clearing the org (org=="") is treated as a departure: the prior org's open
// membership is closed effective this month and no new one is opened.
//
// NOTE(#125): developer here must be the CANONICAL identity (the same value a
// developer_alias row points its canonical column at). The score-join resolves
// cost/outcome identifiers to canonical before looking up teams[canon], so an
// org_hierarchy row keyed by a raw alias would never match. Onboarding maps the
// alias first, then enrolls the canonical id.
func (d *DB) UpsertHierarchy(ctx context.Context, developer, team, division, org string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := upsertHierarchyTx(ctx, tx, developer, team, division, org); err != nil {
		return err
	}
	return tx.Commit()
}

// UpsertHierarchies applies a batch of assignments in ONE transaction: either
// every row lands or none does (#232). It is the store primitive behind the
// bulk-import endpoint, so a 50-developer onboarding is atomic — a failure on
// row N rolls back rows 0..N-1 rather than leaving a half-populated hierarchy
// that would silently mis-aggregate. Each row runs the identical per-developer
// logic as UpsertHierarchy (org-change handling, period_membership seat
// maintenance, NOT-EXISTS idempotency); see that method's doc for the seat
// semantics. developer on each row must already be the CANONICAL identity — the
// API layer resolves aliases before calling this, exactly as it does for the
// single upsert. An empty slice is a no-op that commits cleanly.
func (d *DB) UpsertHierarchies(ctx context.Context, rows []HierarchyRow) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for i, r := range rows {
		if err := upsertHierarchyTx(ctx, tx, r.Developer, r.Team, r.Division, r.Org); err != nil {
			return fmt.Errorf("UpsertHierarchies: row %d: %w", i, err)
		}
	}
	return tx.Commit()
}

// upsertHierarchyTx is the per-developer body shared by UpsertHierarchy (one
// row, its own transaction) and UpsertHierarchies (many rows, one transaction).
// It performs no Begin/Commit of its own — the caller owns the transaction so a
// batch stays all-or-nothing. See UpsertHierarchy's doc comment for the full
// seat / org-change semantics.
func upsertHierarchyTx(ctx context.Context, tx *sql.Tx, developer, team, division, org string) error {
	var prevOrg sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT org FROM org_hierarchy WHERE developer = ?`, developer,
	).Scan(&prevOrg); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO org_hierarchy (developer, team, division, org)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(developer) DO UPDATE SET team=excluded.team, division=excluded.division, org=excluded.org`,
		developer, team, division, org,
	); err != nil {
		return err
	}

	period := time.Now().UTC().Format("2006-01")

	// First enrollment → active since the beginning of time; org change → the
	// new org's membership starts this month (past invoices stay with the old org).
	orgChanged := prevOrg.Valid && prevOrg.String != "" && prevOrg.String != org
	newStart := "0000-01"
	if orgChanged {
		newStart = period
		if _, err := tx.ExecContext(ctx, `
			UPDATE period_membership SET period_end = ?
			WHERE developer = ? AND org = ? AND period_end IS NULL`,
			period, developer, prevOrg.String,
		); err != nil {
			return err
		}
	}

	if org != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO period_membership (developer, org, period_start, period_end)
			SELECT ?, ?, ?, NULL
			WHERE NOT EXISTS (
				SELECT 1 FROM period_membership
				WHERE developer = ? AND org = ? AND period_end IS NULL
			)`,
			developer, org, newStart, developer, org,
		); err != nil {
			return err
		}
	}
	return nil
}

// ErrEndBeforeStart is returned by EndMembership when periodEnd precedes the open
// membership's period_start (#232). It is a CALLER error (an incoherent request:
// a developer cannot leave an org before they joined it), so the API layer maps
// it to 400 rather than 500 — the column CHECK would otherwise surface the same
// violation only as an opaque driver constraint error. Match with errors.Is.
var ErrEndBeforeStart = errors.New("period_end precedes the membership's start period")

// EndMembership closes a developer's open membership in org by setting
// period_end to their last active YYYY-MM period (#41). After this, the
// developer no longer counts toward org seat allocation for periods after
// periodEnd, so active members' org_total slices stop being diluted by people
// who have left. No-op if there is no open membership for (developer, org).
//
// periodEnd must be canonical YYYY-MM and >= the open membership's period_start.
// The read + guard + update run in ONE transaction so the check is race-free
// against a concurrent write (belt-and-braces under SetMaxOpenConns(1)); the
// column CHECK remains the ultimate integrity guard, but this returns the typed
// ErrEndBeforeStart so the caller can distinguish the incoherent-input case from
// an infrastructure failure.
func (d *DB) EndMembership(ctx context.Context, developer, org, periodEnd string) error {
	// Round-trip to canonical form (rejects "2026-1", which time.Parse accepts but
	// breaks the lexicographic period ordering the guard below relies on).
	if t, err := time.Parse("2006-01", periodEnd); err != nil || t.Format("2006-01") != periodEnd {
		return fmt.Errorf("EndMembership: period must be canonical YYYY-MM, got %q", periodEnd)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("EndMembership: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// At most one open row per (developer, org) — the partial unique index
	// idx_period_membership_open guarantees it.
	var start string
	err = tx.QueryRowContext(ctx,
		`SELECT period_start FROM period_membership
		 WHERE developer = ? AND org = ? AND period_end IS NULL`,
		developer, org,
	).Scan(&start)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // no open membership: idempotent no-op
	}
	if err != nil {
		return fmt.Errorf("EndMembership: %w", err)
	}
	// Both operands are canonical YYYY-MM (periodEnd checked above; start is
	// guaranteed canonical by the column GLOB CHECK / the '0000-01' sentinel), so
	// a lexicographic compare is a chronological compare.
	if periodEnd < start {
		return fmt.Errorf("EndMembership: %w (period_end %q, start %q)", ErrEndBeforeStart, periodEnd, start)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE period_membership SET period_end = ?
		WHERE developer = ? AND org = ? AND period_end IS NULL`,
		periodEnd, developer, org,
	); err != nil {
		return fmt.Errorf("EndMembership: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("EndMembership: %w", err)
	}
	return nil
}

// DeveloperCost holds aggregated cost data for a developer over a period, in
// integer micro-dollars (#69). SUM(cost_micro) is exact integer arithmetic; the
// scoring boundary converts to float dollars via MicroToDollars.
type DeveloperCost struct {
	Developer         string
	TotalCostMicro    int64
	RealtimeCostMicro int64 // cost from events at fidelity='realtime' — per-request capture by the collector/proxy; manual /costs rows cannot claim it (#82)
}

// DeveloperCosts returns cost totals per developer since the given time, in
// micro-dollars.
func (d *DB) DeveloperCosts(ctx context.Context, since time.Time) ([]DeveloperCost, error) {
	return d.DeveloperCostsWindow(ctx, since, time.Time{})
}

// tsWindow returns the ts-column predicate and its bound args for a half-open
// [since, until) window (#276). A zero `until` means "no upper bound", yielding
// exactly the pre-#276 `ts >= ?` clause and a single arg so open-ended reads
// are unchanged byte-for-byte. Both bounds are normalized to UTC: modernc.org/
// sqlite compares DATETIME values as offset-bearing strings, so a non-UTC bound
// would window lexically (not temporally) against the UTC-stored ts column and
// silently mis-count rows near the boundary (#180).
func tsWindow(since, until time.Time) (clause string, args []any) {
	if until.IsZero() {
		return "ts >= ?", []any{since.UTC()}
	}
	return "ts >= ? AND ts < ?", []any{since.UTC(), until.UTC()}
}

// DeveloperCostsWindow is the half-open [since, until) form of DeveloperCosts
// (#276): the scores-path denominator, bounded above so a caller can score a
// closed period ("Team TIER for March") or the BEFORE leg of a before/after
// comparison. A zero `until` is open-ended and behaves exactly like the legacy
// DeveloperCosts.
func (d *DB) DeveloperCostsWindow(ctx context.Context, since, until time.Time) ([]DeveloperCost, error) {
	where, args := tsWindow(since, until)
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			developer,
			SUM(cost_micro)                                         AS total_cost,
			SUM(CASE WHEN fidelity = 'realtime' THEN cost_micro ELSE 0 END) AS realtime_cost
		FROM token_events
		WHERE `+where+`
		GROUP BY developer`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DeveloperCost
	for rows.Next() {
		var c DeveloperCost
		if err := rows.Scan(&c.Developer, &c.TotalCostMicro, &c.RealtimeCostMicro); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DevIssueCost is a per-(developer, issue) cost total (#187), the finer grain
// DeveloperCosts sums over. It exists so the work-type segmentation can attribute
// cost to the CATEGORY of work: a token event's cost is charged to the work_type of
// the outcome(s) sharing its (developer, issue), which requires cost at issue grain
// rather than the developer-total DeveloperCosts returns.
type DevIssueCost struct {
	Developer string
	// Repo is the canonical repository of these token_events (#231), or
	// repoid.Unqualified. Part of the grain: pooling repo A's and repo B's issue
	// #42 into one cost bucket was the cost half of the #231 corruption.
	Repo              string
	IssueID           string
	TotalCostMicro    int64
	RealtimeCostMicro int64
}

// DeveloperIssueCosts returns cost totals per (developer, issue) since the given
// time, in micro-dollars (#187). Same windowing and realtime-split as
// DeveloperCosts, only grouped one level finer; the caller maps each (developer,
// issue) onto the work_type of that issue's outcome to build per-type denominators.
// Issues with token spend but no outcome (unattributed exploration) appear here but
// map to no type segment — that is intentional, and the separate zero-token
// tripwire (#136) surfaces such gaps.
func (d *DB) DeveloperIssueCosts(ctx context.Context, since time.Time) ([]DevIssueCost, error) {
	return d.DeveloperIssueCostsWindow(ctx, since, time.Time{})
}

// DeveloperIssueCostsWindow is the half-open [since, until) form of
// DeveloperIssueCosts (#276): the per-(developer, issue) cost the work-type
// segmentation denominates on, bounded above by the same window as
// DeveloperCostsWindow. A zero `until` is open-ended.
func (d *DB) DeveloperIssueCostsWindow(ctx context.Context, since, until time.Time) ([]DevIssueCost, error) {
	where, args := tsWindow(since, until)
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			developer,
			repo,
			issue_id,
			SUM(cost_micro)                                                 AS total_cost,
			SUM(CASE WHEN fidelity = 'realtime' THEN cost_micro ELSE 0 END) AS realtime_cost
		FROM token_events
		WHERE `+where+`
		GROUP BY developer, repo, issue_id`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DevIssueCost
	for rows.Next() {
		var c DevIssueCost
		if err := rows.Scan(&c.Developer, &c.Repo, &c.IssueID, &c.TotalCostMicro, &c.RealtimeCostMicro); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UnattributedIssueID is the sentinel issue id token events carry when their cost
// cannot be tied to a real issue (#234 reads it to split attributed vs
// unattributed spend). It MUST equal collector.UnattributedIssueID — the two
// capture paths write it and this read splits on it, so a drift would silently
// mis-bucket every exploration dollar. store cannot import collector (collector
// imports store), so the value is duplicated here and pinned equal by a guard
// test in the collector package (TestUnattributedIssueIDMatchesStore).
const UnattributedIssueID = "unattributed"

// Labeled unattributed sub-buckets — the read-side mirror of the
// collector.Unattributed{Main,DetachedHEAD,NoIssue} sentinels the JSONL join
// writes (#refocus, Option B). store cannot import collector (collector imports
// store), so the strings are duplicated here and pinned equal by the collector's
// guard test (TestUnattributedIssueIDMatchesStore). Only the exploratory (main)
// label is needed by name in a read path today; the others are declared for
// symmetry and so the guard test can pin all three.
const (
	UnattributedMainBucket         = UnattributedIssueID + ":main"
	UnattributedDetachedHEADBucket = UnattributedIssueID + ":detached-head"
	UnattributedNoIssueBucket      = UnattributedIssueID + ":branch-without-issue"
)

// unattributedLikePattern matches the LABELED unattributed sub-buckets
// (collector.UnattributedMain/DetachedHEAD/NoIssue) — every issue_id of the form
// "unattributed:<reason>". The attributed/unattributed split (#234) and the bucket
// breakdown MUST treat the whole family as unattributed: a `= UnattributedIssueID`
// exact match would count the labeled buckets as ATTRIBUTED and silently inflate
// coverage. Kept adjacent to the sentinel so the two never drift. ':' is a literal
// in LIKE; only '%' and '_' are wildcards, and neither appears before the '%' here.
const unattributedLikePattern = UnattributedIssueID + ":%"

// ModelClassCost is one (host, model) group's windowed cost and per-class token
// totals — the raw, un-derived input to BuildCostComposition (#234). host and
// model are the stored columns (raw); the builder folds model via NormalizeModel
// and classifies premium host-aware via IsPremiumModel, so version/date variants
// of one model merge and an open-weights host is never collapsed onto the weights
// (#300). CostMicro is exact SUM(cost_micro); UnattributedCostMicro is the subset
// of that spend charged to UnattributedIssueID, so attributed = Cost − Unattributed
// reconciles to the group total with no residual bucket.
type ModelClassCost struct {
	Host                  string
	Model                 string
	CostMicro             int64
	UnattributedCostMicro int64
	InputTok              int64
	OutputTok             int64
	CacheRead             int64
	CacheWrite5m          int64
	CacheWrite1h          int64
}

// ModelCost is one normalized-model row of a CostComposition's by-model breakdown
// (#234): the model's windowed spend, its share of total window spend, and whether
// it prices in the premium tier (IsPremiumModel). Host is retained on the key so an
// open-weights model served from two hosts stays two rows (#300), never pooled onto
// the weights.
type ModelCost struct {
	Model     string // normalized (NormalizeModel)
	Host      string
	CostMicro int64
	Share     float64 // CostMicro / total window cost, 0 when total is 0
	Premium   bool
}

// ClassTokens is the per-class TOKEN composition of a window (#234): the exact
// summed counts from the token_events class columns. It is reported as token
// counts, NOT allocated cost — stored cost_micro is a single blended figure per
// event (cache multipliers and long-context over-tiers make a per-class dollar
// split non-recoverable without re-pricing), so counts are the honest exact
// primitive and directly drive CacheReadShare. Cost is decomposed instead along
// the by-model and attributed/unattributed axes, which ARE exact.
type ClassTokens struct {
	InputTok     int64
	OutputTok    int64
	CacheRead    int64
	CacheWrite5m int64
	CacheWrite1h int64
}

// CostComposition is the derived cost-composition sidecar for a window (#234): a
// name-free, whole-window aggregate that answers "where did the tokens/dollars go
// and what should we optimize?". It is served as the `cost_composition` block on
// /scores. The TIER formula is untouched — this is pure sidecar, same discipline
// as the #136 data_quality block.
//
// Reconciliation invariants (asserted by tests):
//   - AttributedCostMicro + UnattributedCostMicro == TotalCostMicro (exact int).
//   - sum(ByModel[i].CostMicro) == TotalCostMicro (every event has a model).
//   - sum(ByModel[i].Share) == 1.0 within rounding when TotalCostMicro > 0.
type CostComposition struct {
	TotalCostMicro        int64
	AttributedCostMicro   int64
	UnattributedCostMicro int64
	UnattributedShare     float64 // UnattributedCostMicro / total, 0 when total is 0
	// CacheReadShare is the input-side cache-hit share: cache_read /
	// (input_tok + cache_read + cache_write_5m + cache_write_1h). It is the
	// comparability signal docs/pricing-philosophy.md §4 demands, computed from the
	// cache columns exactly as that section says it must be. Output tokens are
	// excluded — caching is an input-side mechanic. 0 when there are no input-side
	// tokens.
	CacheReadShare float64
	// PremiumModelShare is the SPEND share billed to premium-tier models
	// (IsPremiumModel): premium cost_micro / total. The model-routing lever — a
	// high share on routine work is the "Opus where Haiku suffices" signal. 0 when
	// total is 0.
	PremiumModelShare float64
	ByModel           []ModelCost
	ByClass           ClassTokens
}

// BuildCostComposition folds raw per-(host, model) rows into the derived
// CostComposition (#234). Pure and DB-free so the derivation — folding by
// normalized model, premium classification, and every share — is unit-testable
// without a store. by-model rows are sorted by descending cost (ties broken by
// model then host) for a stable, operator-useful ordering. A nil/empty input
// yields a zero-value composition with an empty (non-nil) ByModel slice.
func BuildCostComposition(rows []ModelClassCost) CostComposition {
	comp := CostComposition{ByModel: []ModelCost{}}
	// Fold raw rows by (normalized model, host) so date/version variants merge
	// while distinct hosts stay distinct (#300). premiumMicro accumulates spend on
	// premium-tier models across the same fold.
	type modelKey struct{ model, host string }
	byKey := map[modelKey]*ModelCost{}
	order := []modelKey{}
	var premiumMicro int64
	for _, r := range rows {
		norm := NormalizeModel(r.Model)
		premium := IsPremiumModel(r.Host, r.Model)
		k := modelKey{model: norm, host: r.Host}
		mc, ok := byKey[k]
		if !ok {
			mc = &ModelCost{Model: norm, Host: r.Host, Premium: premium}
			byKey[k] = mc
			order = append(order, k)
		}
		mc.CostMicro += r.CostMicro

		comp.TotalCostMicro += r.CostMicro
		comp.UnattributedCostMicro += r.UnattributedCostMicro
		if premium {
			premiumMicro += r.CostMicro
		}
		comp.ByClass.InputTok += r.InputTok
		comp.ByClass.OutputTok += r.OutputTok
		comp.ByClass.CacheRead += r.CacheRead
		comp.ByClass.CacheWrite5m += r.CacheWrite5m
		comp.ByClass.CacheWrite1h += r.CacheWrite1h
	}
	comp.AttributedCostMicro = comp.TotalCostMicro - comp.UnattributedCostMicro

	for _, k := range order {
		mc := byKey[k]
		if comp.TotalCostMicro > 0 {
			mc.Share = float64(mc.CostMicro) / float64(comp.TotalCostMicro)
		}
		comp.ByModel = append(comp.ByModel, *mc)
	}
	sort.Slice(comp.ByModel, func(i, j int) bool {
		a, b := comp.ByModel[i], comp.ByModel[j]
		if a.CostMicro != b.CostMicro {
			return a.CostMicro > b.CostMicro
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Host < b.Host
	})

	if comp.TotalCostMicro > 0 {
		comp.UnattributedShare = float64(comp.UnattributedCostMicro) / float64(comp.TotalCostMicro)
		comp.PremiumModelShare = float64(premiumMicro) / float64(comp.TotalCostMicro)
	}
	inputSide := comp.ByClass.InputTok + comp.ByClass.CacheRead + comp.ByClass.CacheWrite5m + comp.ByClass.CacheWrite1h
	if inputSide > 0 {
		comp.CacheReadShare = float64(comp.ByClass.CacheRead) / float64(inputSide)
	}
	return comp
}

// CostCompositionWindow aggregates token_events over the half-open [since, until)
// window (#234) into the cost-composition sidecar: cost by normalized model, the
// per-class token composition, and attributed vs unattributed spend, with the
// cache-read and premium-model levers derived. A zero `until` is open-ended
// (mirrors DeveloperCostsWindow). One GROUP BY over (host, model) — the same
// idx_token_events index the scores path already scans — then BuildCost
// composition does the pure folding and share math in Go (premium classification
// is host-aware and lives in the price table, not SQL).
func (d *DB) CostCompositionWindow(ctx context.Context, since, until time.Time) (CostComposition, error) {
	where, args := tsWindow(since, until)
	// The unattributed-family binds come first in text order, so their args lead
	// the tsWindow bounds. Bound (not interpolated) even though they are trusted
	// consts — keeps the one query-shaping convention uniform. The family match
	// (exact base sentinel OR any "unattributed:<reason>" bucket) keeps the
	// attributed/unattributed split correct after the labeled-bucket split — an
	// exact `= ?` would count the buckets as attributed.
	args = append([]any{UnattributedIssueID, unattributedLikePattern}, args...)
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			host,
			model,
			SUM(cost_micro)                                                        AS cost,
			SUM(CASE WHEN issue_id = ? OR issue_id LIKE ? THEN cost_micro ELSE 0 END) AS unattributed_cost,
			SUM(input_tok), SUM(output_tok), SUM(cache_read),
			SUM(cache_write_5m), SUM(cache_write_1h)
		FROM token_events
		WHERE `+where+`
		GROUP BY host, model`,
		args...,
	)
	if err != nil {
		return CostComposition{}, err
	}
	defer func() { _ = rows.Close() }()
	var raw []ModelClassCost
	for rows.Next() {
		var r ModelClassCost
		if err := rows.Scan(&r.Host, &r.Model, &r.CostMicro, &r.UnattributedCostMicro,
			&r.InputTok, &r.OutputTok, &r.CacheRead, &r.CacheWrite5m, &r.CacheWrite1h); err != nil {
			return CostComposition{}, err
		}
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		return CostComposition{}, err
	}
	return BuildCostComposition(raw), nil
}

// UnattributedBucketCost is one (developer, bucket) group's windowed unattributed
// spend (#refocus, Option B). Bucket is the stored issue_id sentinel — the base
// "unattributed" (proxy/poller rows that saw no branch) or a labeled JSONL bucket
// ("unattributed:main", "unattributed:detached-head",
// "unattributed:branch-without-issue"). Developer is the canonical identity so the
// handler can derive BOTH the org-level split (sum over developers) and a
// per-developer exploratory share from one read.
type UnattributedBucketCost struct {
	Developer string
	Bucket    string
	CostMicro int64
}

// UnattributedBucketCostsWindow returns per-(developer, bucket) unattributed spend
// over the half-open [since, until) window (#refocus, Option B) — the honest split
// of the single unattributed mass the cost-composition sidecar reports as one
// number. A zero `until` is open-ended (mirrors CostCompositionWindow). Only rows
// in the unattributed family are scanned (base sentinel OR any "unattributed:%"
// bucket); attributed spend is excluded by construction. Name-carrying by design:
// the handler suppresses the Developer names in team-aggregation mode (#185),
// exactly as it does for the other per-developer data_quality fields.
func (d *DB) UnattributedBucketCostsWindow(ctx context.Context, since, until time.Time) ([]UnattributedBucketCost, error) {
	where, args := tsWindow(since, until)
	// Family binds lead the tsWindow bounds in text order (same convention as
	// CostCompositionWindow).
	args = append([]any{UnattributedIssueID, unattributedLikePattern}, args...)
	rows, err := d.db.QueryContext(ctx, `
		SELECT developer, issue_id, SUM(cost_micro) AS cost
		FROM token_events
		WHERE (issue_id = ? OR issue_id LIKE ?) AND `+where+`
		GROUP BY developer, issue_id`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UnattributedBucketCost
	for rows.Next() {
		var b UnattributedBucketCost
		if err := rows.Scan(&b.Developer, &b.Bucket, &b.CostMicro); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DistinctPriceVersionsWindow returns the ASCENDING, deduplicated set of
// price_table versions that priced token_events in the half-open [since, until)
// window (#293). cost_micro is immutable per row (#233), so a window legitimately
// spans multiple versions — historical rows keep the version that priced them
// while new rows land at the active version. /scores stamps a SINGLE top-level
// price_table.version (the active table at read time); this read lets the handler
// detect when that stamp hides a mix and raise the data_quality WARN instead of
// implying false uniformity. A zero `until` is open-ended (mirrors
// DeveloperCostsWindow); an empty window returns a nil slice (no versions, no WARN).
//
// The unstamped sentinel 0 (a pre-#233 row the backfill has not yet converted, or
// an insert before the active table settled — neither should persist post-migration,
// since stampPriceVersion resolves 0 to the active version and backfillPriceVersion
// converts legacy rows at Open) is filtered out: it is not a real pricing version,
// and letting it count would fabricate a "mixed" signal from a single genuine version.
//
// PERF: this filters on ts and reads price_version, which no index covers — the
// ts-window range comes from idx_token_events_ts_id, then each in-window row is a
// heap lookup for price_version (a window-bounded scan, same class as
// CostCompositionWindow, see #333). Fine at single-tenant, window-bounded scale;
// revisit alongside #333 if token_events grows.
func (d *DB) DistinctPriceVersionsWindow(ctx context.Context, since, until time.Time) ([]int, error) {
	where, args := tsWindow(since, until)
	rows, err := d.db.QueryContext(ctx, `
		SELECT DISTINCT price_version
		FROM token_events
		WHERE `+where+` AND price_version > 0
		ORDER BY price_version`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return versions, nil
}

// WindowActivity summarizes cost and outcome volume in the window [since, now]
// for the zero-outcome tripwire (#189). CostMicro is SUM(cost_micro) over
// token_events; Outcomes is COUNT(*) over outcomes. The tripwire fires when
// CostMicro > 0 but Outcomes == 0 — cost accrued yet nothing shipped, which on a
// trunk-based team (direct pushes behind flags) or a broken GitHub webhook
// silently craters team TIER to ~0 with no warning anywhere.
type WindowActivity struct {
	CostMicro int64 // SUM(token_events.cost_micro) in the window
	Outcomes  int64 // COUNT(outcomes) in the window
}

// CostCoverageStart returns the earliest instant for which ANY token event was
// captured — this installation's COST HORIZON (#512) — and ok=false when the
// store holds no token events at all.
//
// Why this exists. A score window that starts BEFORE the horizon divides a full
// window of outcomes by a partial window of cost: outcomes arrive by webhook and
// backfill regardless of when capture was installed, while cost only exists from
// the horizon forward. The result is a silently INFLATED TIER — measured at about
// twice on a real multi-repo installation, where most of the cost but well under
// half the outcomes sat inside the last 30 days.
//
// The horizon is NOT a log-retention artifact, and reading it as one leads to the
// wrong fix. token_events is append-only forever (the only DELETE reaching it is
// EraseDeveloper, the GDPR Art. 17 primitive), so extracted events long outlive the
// provider session logs they came from. The horizon is simply the date capture
// began, it is permanent per install, and no amount of raw-log retention moves it:
// a brand-new install has a horizon of today even with years of logs on disk,
// because TIER was not there to read them.
//
// Empty table is reported as ok=false rather than the zero time, so a caller
// cannot mistake "no data" for "the horizon is the epoch" and conclude every
// window is fully covered.
//
// ORDER BY ts LIMIT 1 rather than MIN(ts) — DELIBERATE, do not "simplify" it back.
// ts is written as a Go time.Time and stored in Go's time.String() layout
// ("2026-06-23 10:01:01.135 +0000 UTC"). modernc.org/sqlite maps a DIRECT column
// read back to a time.Time, but an AGGREGATE over that column bypasses the
// mapping and hands back the raw string, so `MIN(ts)` fails to scan with
// "unsupported Scan, storing driver.Value type string into type *time.Time".
// The ORDER BY form is a direct column read, keeps the driver's type mapping, and
// is equally cheap — it walks one row of the ts index rather than scanning.
func (d *DB) CostCoverageStart(ctx context.Context) (time.Time, bool, error) {
	var ts sql.NullTime
	err := d.db.QueryRowContext(ctx,
		`SELECT ts FROM token_events ORDER BY ts LIMIT 1`,
	).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	if !ts.Valid {
		return time.Time{}, false, nil
	}
	return ts.Time.UTC(), true, nil
}

// SourceCoverageStart is CostCoverageStart at per-source grain (#512): the earliest
// captured event for each source value present in the store.
//
// The global horizon alone is the loosest possible bound and stays wrong in the
// other direction. Sources begin at different dates — a JSONL collector installed
// in June and a Codex rollout reader enabled in July give one horizon each — so a
// window that clears the global MIN can still predate a given source entirely and
// count that source's outcomes against none of its cost. Reporting both lets a
// reader see which capture path is thin instead of inferring it from one number.
// Same ORDER-BY-not-MIN constraint as CostCoverageStart above, for the same
// driver reason: the per-source minimum is read as a direct column, one indexed
// row per source, rather than as a GROUP BY aggregate that would scan back as an
// unparseable string. The source set is tiny and bounded by the Source*
// constants, so the per-source round trip is cheaper than it looks.
func (d *DB) SourceCoverageStart(ctx context.Context) (map[string]time.Time, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT DISTINCT source FROM token_events`)
	if err != nil {
		return nil, err
	}
	// Collected in a closure so the rows handle is released by defer on every
	// path. The DB is single-writer SQLite: a cursor left open across the
	// per-source reads below would hold its read lock for the whole loop.
	sources, err := func() (out []string, err error) {
		defer func() {
			if cerr := rows.Close(); err == nil {
				err = cerr
			}
		}()
		for rows.Next() {
			var src string
			if err := rows.Scan(&src); err != nil {
				return nil, err
			}
			out = append(out, src)
		}
		return out, rows.Err()
	}()
	if err != nil {
		return nil, err
	}

	out := make(map[string]time.Time, len(sources))
	for _, src := range sources {
		var ts sql.NullTime
		err := d.db.QueryRowContext(ctx,
			`SELECT ts FROM token_events WHERE source = ? ORDER BY ts LIMIT 1`, src,
		).Scan(&ts)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if ts.Valid {
			out[src] = ts.Time.UTC()
		}
	}
	return out, nil
}

// WindowActivity returns the accrued cost and the number of recorded outcomes in
// [since, now] (#189). `since` is normalized to UTC in-method: modernc.org/sqlite
// compares DATETIME lexically, so a non-UTC bound would mis-window against the
// UTC-stored ts column (#180/#199). Two cheap COUNT/SUM queries against the
// existing ts indexes — no per-row scan.
func (d *DB) WindowActivity(ctx context.Context, since time.Time) (WindowActivity, error) {
	since = since.UTC()
	var a WindowActivity
	// SUM over an empty set is SQL NULL; COALESCE folds it to 0 so the scan
	// target stays a plain int64.
	if err := d.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_micro), 0) FROM token_events WHERE ts >= ?`, since,
	).Scan(&a.CostMicro); err != nil {
		return WindowActivity{}, err
	}
	if err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM outcomes WHERE ts >= ?`, since,
	).Scan(&a.Outcomes); err != nil {
		return WindowActivity{}, err
	}
	return a, nil
}

// AttributableWindow is the look-back window, ending at an outcome's merge
// timestamp, over which token_events are summed for the zero-token-outcome
// tripwire (#136). 14 days: long enough for any real issue's working sessions,
// short enough that stale exploration on a REUSED issue id (tokens spent weeks
// ago against the same id) cannot launder a fresh, effectively-tokenless PR.
// Kept as a package const with rationale so the query window can never drift
// from the value the tripwire's semantics assume.
const AttributableWindow = 14 * 24 * time.Hour

// DevIssue identifies a (developer, issue) attribution pair (#136). It is the
// key of the OutcomeTokenTotals result: the token total recorded under that
// developer's identity for that issue inside the outcome's window.
type DevIssue struct {
	Developer string
	// Repo is the canonical repository of the token_events rows in this bucket
	// (#231), or repoid.Unqualified for repo-blind producers. It is part of the
	// key because repo A's issue #42 and repo B's issue #42 are different issues;
	// pooling them was the cost half of the #231 corruption.
	Repo    string
	IssueID string
}

// tokenTotalsBatchSize caps how many (repo, issue) windows one OutcomeTokenTotals
// query carries. Each window contributes 4 bound parameters (issue_id, window low,
// window high, repo); 240 windows → 960 params, under SQLite's default 999-variable
// statement limit. Larger outcome sets are split across successive queries —
// bounded (ceil(windows/240)) round-trips, never a per-outcome N+1.
//
// This constant and the per-window parameter count move TOGETHER. #231 took the
// count from 3 to 4 by adding the repo predicate; at the old 300 that would be 1200
// params and SQLite would fail the statement on any sufficiently large outcome set —
// a latent crash reachable only at scale, i.e. only at a customer site. If you add a
// bound parameter to the OR-term below, lower this constant so
// tokenTotalsBatchSize * params_per_window stays < 999.
const tokenTotalsBatchSize = 240

// OutcomeTokenTotals returns, per (developer, repo, issue_id), the total tokens
// (input + output + cache_read + cache_write_5m + cache_write_1h) recorded in
// the AttributableWindow ending at each outcome's merge timestamp (#136). It is
// the cheap query behind the zero-token tripwire: an outcome whose canonical
// (developer, issue) total is below scoring.MinAttributableTokens was produced
// off-books (G-02) or is mis-attributed (identity mismatch B1) and must be
// surfaced.
//
// Design notes:
//   - Exact per-outcome window, in ONE query per batch. Each (repo, issue) window
//     contributes an OR-term — "(issue_id = ? AND ts >= ? AND ts <= ? AND
//     (repo = ? OR repo = 'unqualified'))" when the window names a real repo, and
//     the repo-free form when it does not (#231); the whole set is summed
//     with a single GROUP BY over the window union (the idx_token_events_issue
//     index covers the issue_id lookup). This is exact — no coarse/approximate
//     phase — and never issues a per-outcome query.
//   - The result is keyed by the RAW developer recorded on token_events and is
//     NOT filtered by the outcome's developer in SQL. That is deliberate: cost
//     and outcomes live in different identity spaces joined by the #125 alias
//     map (OS username on token_events vs. GitHub login on outcomes), so the
//     caller re-keys this map through canon() before comparing to an outcome's
//     canonical identity. Filtering on developer here would miss aliased tokens
//     and false-flag every mapped developer.
//   - When two outcomes REUSE the same issue id, the window is that of the
//     FRESHEST (latest-merged) outcome — [maxTs - 14d, maxTs] — never the union
//     of both windows. Unioning would extend the lower bound backward and pull a
//     prior PR's tokens into a fresh tokenless PR's total, laundering the exact
//     G-02 vector the tripwire exists to catch. Taking the freshest window fails
//     safe instead: it may over-flag an older reused-id outcome (an honest data-
//     quality signal — reusing an id is itself suspicious), but never launders.
//
// Returned map omits pairs with no matching events; the caller treats an absent
// key as a zero total.
func (d *DB) OutcomeTokenTotals(ctx context.Context, outcomes []Outcome) (map[DevIssue]int64, error) {
	totals := map[DevIssue]int64{}
	if len(outcomes) == 0 {
		return totals, nil
	}

	// One window per (repo, issue id), anchored on the freshest merge that references
	// it (see the anti-laundering note above). #231: the window key carries repo
	// because repo A's #42 and repo B's #42 are different issues — keying by issue
	// alone would apply the freshest window ACROSS repos, letting one repo's merge
	// timestamp define another repo's attributable window.
	//
	// All ts bounds are normalized to UTC: modernc.org/sqlite compares DATETIME as
	// offset-bearing strings, so every windowed bound must be UTC to window by
	// instant on a non-UTC host (#180).
	type repoIssue struct{ repo, issue string }
	type window struct{ low, high time.Time }
	windows := make(map[repoIssue]window, len(outcomes))
	for _, o := range outcomes {
		if o.IssueID == "" {
			continue
		}
		k := repoIssue{repo: normalizeRepo(o.Repo), issue: o.IssueID}
		high := o.Timestamp.UTC()
		if w, ok := windows[k]; ok && !high.After(w.high) {
			continue // an equal-or-fresher window for this (repo, issue) is already set
		}
		windows[k] = window{low: high.Add(-AttributableWindow), high: high}
	}

	// Stable ordering keeps batch boundaries deterministic (aids testing and
	// reasoning). Sorting by ISSUE FIRST is load-bearing, not cosmetic: batches are
	// cut on issue boundaries below, and every window for one issue must live in the
	// same batch.
	keys := make([]repoIssue, 0, len(windows))
	for k := range windows {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].issue != keys[j].issue {
			return keys[i].issue < keys[j].issue
		}
		return keys[i].repo < keys[j].repo
	})

	// Batch on ISSUE boundaries. A 'unqualified' token row for issue X satisfies the
	// window term of EVERY repo that has an outcome on X, so if two of those windows
	// landed in different batches, scanTokenTotals would add that sentinel bucket to
	// totals twice (it accumulates with +=). Keeping an issue's windows together means
	// one GROUP BY collapses its sentinel rows exactly once.
	for start := 0; start < len(keys); {
		end := start
		for end < len(keys) {
			// Extend to the end of the current issue's run.
			runEnd := end
			for runEnd < len(keys) && keys[runEnd].issue == keys[end].issue {
				runEnd++
			}
			// Take the run if it fits, or if we have taken nothing yet (a single
			// issue with more windows than the cap must still be issued as one
			// statement — splitting it would double-count. It would need >240 repos
			// sharing one issue number to breach the variable limit).
			if runEnd-start > tokenTotalsBatchSize && end > start {
				break
			}
			end = runEnd
		}
		batch := keys[start:end]
		start = end

		var sb strings.Builder
		sb.WriteString(`
			SELECT developer, repo, issue_id,
			       SUM(input_tok + output_tok + cache_read + cache_write_5m + cache_write_1h)
			FROM token_events
			WHERE `)
		// Up to four bound params per window — see tokenTotalsBatchSize.
		//
		// A repo-QUALIFIED window admits its own repo AND the 'unqualified' sentinel:
		// a repo-blind producer (the proxy) still contributes its cost to a qualified
		// outcome. That is ruling 2's tolerant join.
		//
		// A repo-BLIND window must not filter on repo at all. Binding k.repo would
		// make the predicate `repo='unqualified' OR repo='unqualified'`, fetching only
		// sentinel rows — so an outcome posted without a repo (every GitLab/Bitbucket
		// integration that omits the optional field) would see none of the real-repo
		// tokens its collector captured, and the zero-token tripwire would raise a
		// FALSE flag against a developer whose work was fully funded. Sum() promises a
		// repo-blind outcome matches every bucket for its issue; this is where that
		// promise is kept.
		//
		// GROUP BY separates rows into per-repo buckets either way, so the caller
		// (JoinIndex) decides how to combine them.
		args := make([]any, 0, len(batch)*4)
		for i, k := range batch {
			if i > 0 {
				sb.WriteString(" OR ")
			}
			w := windows[k]
			if repoid.IsReal(k.repo) {
				sb.WriteString("(issue_id = ? AND ts >= ? AND ts <= ? AND (repo = ? OR repo = 'unqualified'))")
				args = append(args, k.issue, w.low, w.high, k.repo)
			} else {
				sb.WriteString("(issue_id = ? AND ts >= ? AND ts <= ?)")
				args = append(args, k.issue, w.low, w.high)
			}
		}
		sb.WriteString(" GROUP BY developer, repo, issue_id")

		rows, err := d.db.QueryContext(ctx, sb.String(), args...)
		if err != nil {
			return nil, err
		}
		if err := scanTokenTotals(rows, totals); err != nil {
			return nil, err
		}
	}
	return totals, nil
}

// RepoMatch reports whether a cost row's repo and an outcome's repo join, under the
// TOLERANT rule (#231, ruling 2): repo disambiguates only when BOTH sides name a
// real repository. If either side is repo-blind ('unqualified'), the pair joins on
// issue id alone, as it did before #231.
//
// The rejected alternative was a strict composite key. Strict is "more correct" in
// the abstract and catastrophic in practice: the reverse proxy structurally cannot
// know a repository, and every pre-#231 row carries the sentinel, so a strict join
// would de-attribute all of that cost and drive those developers' scores toward zero
// with no error, no log, and no way for an operator to notice. Tolerant fails
// VISIBLY (an org sees its capture is under-qualified) instead of invisibly.
func RepoMatch(a, b string) bool {
	if !repoid.IsReal(a) || !repoid.IsReal(b) {
		return true
	}
	return a == b
}

// JoinIndex answers "what value joins to this outcome" in O(1) under the tolerant
// rule. Build it once per request from any map keyed by DevIssue — windowed token
// totals (OutcomeTokenTotals) or per-issue cost (DeveloperIssueCosts) — after the
// caller has re-keyed developers through the #125 alias map. A naive scan of the map
// per outcome would be quadratic on a large window.
type JoinIndex struct {
	exact   map[DevIssue]int64 // (developer, repo, issue) -> value
	anyRepo map[DevIssue]int64 // (developer, "", issue)   -> value summed across every repo
}

// BuildJoinIndex indexes a DevIssue-keyed map for tolerant lookup.
func BuildJoinIndex(vals map[DevIssue]int64) JoinIndex {
	ix := JoinIndex{
		exact:   make(map[DevIssue]int64, len(vals)),
		anyRepo: make(map[DevIssue]int64, len(vals)),
	}
	for k, v := range vals {
		ix.exact[k] += v
		ix.anyRepo[DevIssue{Developer: k.Developer, IssueID: k.IssueID}] += v
	}
	return ix
}

// Sum returns the value joined to an outcome identified by (developer, repo, issue),
// per RepoMatch.
//
//   - A repo-blind outcome matches every bucket for its issue, in any repo.
//   - A repo-qualified outcome matches its own repo's bucket PLUS the repo-blind
//     bucket — the proxy's cost still counts toward it.
//
// Sentinel value is therefore counted toward EACH qualified outcome sharing the issue
// id. That over-counts rather than under-counts, and both consumers want that
// direction:
//
//   - zero-token tripwire: over-counting tokens can only fail to raise a flag, never
//     raise a false one against an innocent developer.
//   - per-work-type cost: over-counting cost lowers TIER, so a developer is never
//     flattered by the ambiguity. The double-count needs a developer with the SAME
//     issue number in two repos AND repo-blind capture; pre-#231 that cost was pooled
//     unconditionally, so this is strictly narrower than the behavior it replaces.
func (ix JoinIndex) Sum(developer, repo, issue string) int64 {
	if !repoid.IsReal(repo) {
		return ix.anyRepo[DevIssue{Developer: developer, IssueID: issue}]
	}
	return ix.exact[DevIssue{Developer: developer, Repo: repo, IssueID: issue}] +
		ix.exact[DevIssue{Developer: developer, Repo: repoid.Unqualified, IssueID: issue}]
}

// scanTokenTotals drains an OutcomeTokenTotals batch into totals, closing rows.
// SUM over an empty group is SQL NULL, so the total is scanned through a
// sql.NullInt64 and absent/NULL sums contribute nothing (treated as 0).
func scanTokenTotals(rows *sql.Rows, totals map[DevIssue]int64) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k DevIssue
		var sum sql.NullInt64
		if err := rows.Scan(&k.Developer, &k.Repo, &k.IssueID, &sum); err != nil {
			return err
		}
		if sum.Valid {
			totals[k] += sum.Int64
		}
	}
	return rows.Err()
}

// DeveloperOutcomes returns all outcomes for a developer since the given time.
func (d *DB) DeveloperOutcomes(ctx context.Context, developer string, since time.Time) ([]Outcome, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT developer, issue_id, COALESCE(pr_number,0), weight, quality,
		       COALESCE(weight_source, 'legacy'), COALESCE(additions, 0),
		       COALESCE(deletions, 0), COALESCE(changed_files, 0),
		       COALESCE(source, 'github-webhook'), COALESCE(repo, 'unqualified'), ts
		FROM outcomes
		WHERE developer = ? AND ts >= ?`,
		developer, since,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Outcome
	for rows.Next() {
		var o Outcome
		if err := rows.Scan(&o.Developer, &o.IssueID, &o.PRNumber, &o.Weight, &o.Quality,
			&o.WeightSource, &o.Additions, &o.Deletions, &o.ChangedFiles, &o.Source,
			&o.Repo, &o.Timestamp); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// AllOutcomesSince returns all outcomes since the given time.
func (d *DB) AllOutcomesSince(ctx context.Context, since time.Time) ([]Outcome, error) {
	return d.AllOutcomesWindow(ctx, since, time.Time{})
}

// AllOutcomesWindow is the half-open [since, until) form of AllOutcomesSince
// (#276): the outcomes numerator for windowed scoring, bounded above by the same
// window as the cost reads so numerator and denominator span one consistent
// range. A zero `until` is open-ended and behaves exactly like AllOutcomesSince.
//
// The per-outcome attributable token window (OutcomeTokenTotals, the #136
// tripwire) needs no separate `until`: every outcome here already has ts < until,
// so its look-back anchor — and therefore every token it counts — is below the
// upper bound. Its 14-day look-back intentionally reaches BEFORE `since` (tokens
// spent last month can fund this month's merge); that is a data-quality window,
// not a reporting window, and must not be clamped to since.
func (d *DB) AllOutcomesWindow(ctx context.Context, since, until time.Time) ([]Outcome, error) {
	where, args := tsWindow(since, until)
	rows, err := d.db.QueryContext(ctx, `
		SELECT developer, issue_id, COALESCE(pr_number,0), weight, quality,
		       COALESCE(weight_source, 'legacy'), COALESCE(additions, 0),
		       COALESCE(deletions, 0), COALESCE(changed_files, 0),
		       COALESCE(source, 'github-webhook'),
		       COALESCE(work_type, 'feature'), COALESCE(work_type_source, 'legacy'),
		       COALESCE(repo, 'unqualified'), ts
		FROM outcomes WHERE `+where, args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Outcome
	for rows.Next() {
		var o Outcome
		if err := rows.Scan(&o.Developer, &o.IssueID, &o.PRNumber, &o.Weight, &o.Quality,
			&o.WeightSource, &o.Additions, &o.Deletions, &o.ChangedFiles, &o.Source,
			&o.WorkType, &o.WorkTypeSource, &o.Repo, &o.Timestamp); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// PageCursor is the keyset position of the last row a bulk-export page returned
// (#191): the (ts, id) of that row. The zero value means "start from the
// beginning of the window" — its zero ts (year 1) sorts before every real row,
// so the keyset predicate admits everything on the first page. id is the unique
// tiebreak (the rowid) that makes (ts, id) a TOTAL order, so paging never loses
// or duplicates a row even when many rows share an identical ts.
type PageCursor struct {
	TS time.Time
	ID int64
}

// keysetWindowSQL is the shared WHERE/ORDER/LIMIT tail for the two keyset
// exports (#191). The window is half-open [since, until); the keyset predicate
// `(ts > ? OR (ts = ? AND id > ?))` walks the total order (ts, id) strictly
// AFTER the cursor row. Every value is a bound `?` parameter — the cursor is
// decoded to a typed (time.Time, int64) by the caller and bound here, never
// interpolated, so a hostile cursor string cannot reach the SQL text. ORDER BY
// (ts, id) matches idx_{token_events,outcomes}_ts_id so the planner seeks to the
// cursor and walks in order (no sort, no deep-offset scan). LIMIT is bound to
// pageSize+1 by the caller so one extra row signals "more pages" without a
// second round trip.
const keysetWindowSQL = `
	WHERE ts >= ? AND ts < ?
	  AND (ts > ? OR (ts = ? AND id > ?))
	ORDER BY ts, id
	LIMIT ?`

// clampExportLimit bounds a requested page size to (0, maxExportPageSize]. It is
// the store-side backstop for the #191 read-cap discipline: even if a caller
// bypasses the handler, the store never materializes an unbounded page. A
// non-positive limit is coerced to the default rather than erroring — the
// handler already rejects a malformed limit loudly; this is defense in depth.
func clampExportLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultExportPageSize
	case limit > MaxExportPageSize:
		return MaxExportPageSize
	default:
		return limit
	}
}

// Export page-size bounds (#191), the read-side counterpart of the #144 input
// caps. DefaultExportPageSize applies when the caller requests none;
// MaxExportPageSize is the hard ceiling the store enforces regardless of the
// request, so a single page can never buffer more than this many rows in memory.
const (
	DefaultExportPageSize = 1000
	MaxExportPageSize     = 10000
)

// ListTokenEvents returns one keyset-paginated page of raw token_events rows in
// (ts, id) order within the half-open window [since, until), starting strictly
// after the `after` cursor (#191). It fetches clampExportLimit(limit) rows;
// hasMore reports whether at least one further row exists beyond the page (so the
// caller can emit a next cursor) — detected by over-fetching one row, never
// returned to the caller. Rows carry the id needed to form the next cursor.
func (d *DB) ListTokenEvents(ctx context.Context, since, until time.Time, after PageCursor, limit int) (events []TokenEvent, hasMore bool, err error) {
	page := clampExportLimit(limit)
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, developer, issue_id, model, input_tok, output_tok, cache_read,
		       cache_write_5m, cache_write_1h, cost_micro, source, fidelity,
		       COALESCE(idempotency_key, ''), COALESCE(repo, 'unqualified'),
		       COALESCE(session_id, ''), price_version, host, billing_mode, ts
		FROM token_events`+keysetWindowSQL,
		since, until, after.TS, after.TS, after.ID, page+1,
	)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]TokenEvent, 0, page)
	for rows.Next() {
		var e TokenEvent
		if err := rows.Scan(&e.ID, &e.Developer, &e.IssueID, &e.Model,
			&e.InputTok, &e.OutputTok, &e.CacheRead, &e.CacheWrite5m, &e.CacheWrite1h,
			&e.CostMicro, &e.Source, &e.Fidelity, &e.IdempotencyKey, &e.Repo, &e.SessionID,
			&e.PriceVersion, &e.Host, &e.BillingMode, &e.Timestamp); err != nil {
			return nil, false, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > page {
		// The over-fetched sentinel row proves another page exists; drop it so the
		// caller sees exactly `page` rows and forms the next cursor from the last.
		return out[:page], true, nil
	}
	return out, false, nil
}

// ListOutcomes returns one keyset-paginated page of raw outcomes rows in
// (ts, id) order within [since, until), starting strictly after `after` (#191).
// Same paging contract as ListTokenEvents: over-fetches one row to set hasMore,
// COALESCEs the nullable columns exactly as AllOutcomesSince does.
func (d *DB) ListOutcomes(ctx context.Context, since, until time.Time, after PageCursor, limit int) (outcomes []Outcome, hasMore bool, err error) {
	page := clampExportLimit(limit)
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, developer, issue_id, COALESCE(pr_number, 0), weight, quality,
		       COALESCE(weight_source, 'legacy'), COALESCE(additions, 0),
		       COALESCE(deletions, 0), COALESCE(changed_files, 0),
		       COALESCE(merge_commit_sha, ''), COALESCE(source, 'github-webhook'),
		       COALESCE(work_type, 'feature'), COALESCE(work_type_source, 'legacy'),
		       COALESCE(repo, 'unqualified'), COALESCE(push_day, ''), ts
		FROM outcomes`+keysetWindowSQL,
		since, until, after.TS, after.TS, after.ID, page+1,
	)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]Outcome, 0, page)
	for rows.Next() {
		var o Outcome
		// push_day COALESCEs NULL->"" so a non-push row exports an empty key, the
		// same convention merge_commit_sha uses (#242).
		if err := rows.Scan(&o.ID, &o.Developer, &o.IssueID, &o.PRNumber, &o.Weight, &o.Quality,
			&o.WeightSource, &o.Additions, &o.Deletions, &o.ChangedFiles,
			&o.MergeCommitSHA, &o.Source, &o.WorkType, &o.WorkTypeSource, &o.Repo, &o.PushDay, &o.Timestamp); err != nil {
			return nil, false, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > page {
		return out[:page], true, nil
	}
	return out, false, nil
}

// keysetWindowSQLFor builds the shared WHERE/ORDER/LIMIT keyset tail (see
// keysetWindowSQL) for a bulk export whose time column is tsCol — "event_ts" for
// quality_events, "ts" for quality_history (#242). tsCol is an internal constant
// literal chosen by the caller, never request input, so building the SQL text
// with it carries no injection risk; every value below is still a bound `?`.
func keysetWindowSQLFor(tsCol string) string {
	return `
		WHERE ` + tsCol + ` >= ? AND ` + tsCol + ` < ?
		  AND (` + tsCol + ` > ? OR (` + tsCol + ` = ? AND id > ?))
		ORDER BY ` + tsCol + `, id
		LIMIT ?`
}

// ListQualityEvents returns one keyset-paginated page of raw quality_events rows
// (#242) in (event_ts, id) order within the half-open window [since, until),
// starting strictly after `after`. Same paging contract as ListOutcomes:
// over-fetches one row to set hasMore. The keyset column is event_ts (the observed
// signal time), which AppendQualityEvent always writes in Go form — so the
// time.Time bounds bind and compare exactly as the token_events/outcomes exports
// do, backed by idx_quality_events_ts_id.
//
// This is the BI/reconciliation counterpart to the erasure-scoped ExportDeveloper
// quality read: it makes the quality signal log (from which the multiplier is
// derived, "quality == last new_quality") re-derivable from an external export,
// not just internally.
func (d *DB) ListQualityEvents(ctx context.Context, since, until time.Time, after PageCursor, limit int) (events []QualityEvent, hasMore bool, err error) {
	page := clampExportLimit(limit)
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, outcome_id, developer, issue_id, event_type, source_ref, event_ts, recorded_at
		FROM quality_events`+keysetWindowSQLFor("event_ts"),
		since, until, after.TS, after.TS, after.ID, page+1,
	)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]QualityEvent, 0, page)
	for rows.Next() {
		var e QualityEvent
		if err := rows.Scan(&e.ID, &e.OutcomeID, &e.Developer, &e.IssueID,
			&e.EventType, &e.SourceRef, &e.EventTS, &e.RecordedAt); err != nil {
			return nil, false, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > page {
		return out[:page], true, nil
	}
	return out, false, nil
}

// qualityHistoryTSLayout is the second-precision UTC layout quality_history.ts is
// stored in. quality_history.ts is written by the SQLite DEFAULT CURRENT_TIMESTAMP
// (NOT in Go like every other table's ts), so ListQualityHistory MUST bind its
// window/cursor bounds as strings in this exact layout: the modernc driver renders
// a bound time.Time as Go's "2006-01-02 15:04:05.999999999 -0700 MST" text, which
// does NOT lexically match the stored "2006-01-02 15:04:05" form (verified: an
// `= <time.Time>` predicate matches zero rows). Binding this layout keeps the
// comparison plain string ordering, which idx_quality_history_ts_id serves directly.
const qualityHistoryTSLayout = "2006-01-02 15:04:05"

// ListQualityHistory returns one keyset-paginated page of raw quality_history rows
// (#242) in (ts, id) order within [since, until), starting strictly after `after`.
// Same paging contract as ListOutcomes. See qualityHistoryTSLayout for why the
// bounds are bound as strings rather than time.Time. Rows are scanned into a
// time.Time (Timestamp) so the caller forms the next cursor the usual way.
//
// This makes the quality transition log — the append-only audit that renders
// every outcome's multiplier re-derivable (quality == last new_quality) — pullable
// as a BI/reconciliation export, not just via the erasure-scoped DSAR read.
func (d *DB) ListQualityHistory(ctx context.Context, since, until time.Time, after PageCursor, limit int) (history []QualityTransition, hasMore bool, err error) {
	page := clampExportLimit(limit)
	sinceStr := since.UTC().Format(qualityHistoryTSLayout)
	untilStr := until.UTC().Format(qualityHistoryTSLayout)
	cursorStr := after.TS.UTC().Format(qualityHistoryTSLayout)
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, outcome_id, developer, issue_id, old_quality, new_quality, reason, source_ref, ts
		FROM quality_history`+keysetWindowSQLFor("ts"),
		sinceStr, untilStr, cursorStr, cursorStr, after.ID, page+1,
	)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]QualityTransition, 0, page)
	for rows.Next() {
		var q QualityTransition
		if err := rows.Scan(&q.ID, &q.OutcomeID, &q.Developer, &q.IssueID,
			&q.OldQuality, &q.NewQuality, &q.Reason, &q.SourceRef, &q.Timestamp); err != nil {
			return nil, false, err
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > page {
		return out[:page], true, nil
	}
	return out, false, nil
}

// ActualSpend records the actual USD a developer was billed for a billing
// period — typically the enterprise-contract invoice line item for a month.
// Distinct from the list-price cost computed in TokenEvent.CostMicro via the
// Reference Price Table; ActualSpend lets us surface Spend Leverage
// (= list value ÷ paid value), the CFO-facing "how much cheaper is our
// enterprise contract than retail" number.
type ActualSpend struct {
	Developer       string
	Period          string // YYYY-MM
	ActualPaidMicro int64  // integer micro-dollars (#69); negative = credit memo (#24)
	Timestamp       time.Time
}

// InsertActualSpend appends a per-developer per-period actual-paid row.
// Multiple rows can share (developer, period); the SUM at query time yields
// the net (closes #24).
//
// Credit memos and refunds enter as negative deltas; revised invoices enter
// as a delta against the previous total. This shifts the "correction"
// workflow from "post the new total, overwrites the old" to "post the delta
// that nets to the new total" — finance keeps the audit trail in the row
// history rather than the row being overwritten.
func (d *DB) InsertActualSpend(ctx context.Context, a ActualSpend) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO actual_spend (developer, period, actual_paid_micro, ts)
		VALUES (?, ?, ?, ?)`,
		a.Developer, a.Period, a.ActualPaidMicro, a.Timestamp,
	)
	return err
}

// OrgActualSpend records the actual USD an org was billed for a billing
// period — the single enterprise-contract invoice line item for the whole
// team. Two-tier resolution (closes #23):
//
//   - Per-developer rows in actual_spend take precedence (Cursor Business
//     and similar tools that emit per-seat invoices land here).
//   - Org rows in org_actual_spend are the fallback (the common Anthropic /
//     OpenAI enterprise case: one bill, N seats). The developer's slice is
//     computed as org_total / distinct seat count in org_hierarchy.
//
// This avoids forcing finance to fabricate a per-developer allocation that
// the system would then report with false per-developer precision.
type OrgActualSpend struct {
	Org             string
	Period          string // YYYY-MM
	ActualPaidMicro int64  // integer micro-dollars (#69); negative = credit memo (#24)
	// Source is the provenance feed for this delta row (#138). Empty is stored as
	// OrgSpendSourceManual (the provider-agnostic POST endpoint sets no source); a
	// provider poller sets its own tag (e.g. collector.SourceAnthropicAdmin) so its
	// reconciliation nets only against its own rows and never cannibalizes another
	// source's spend. The allocation read SUMs across sources regardless.
	Source    string
	Timestamp time.Time
}

// OrgSpendSourceManual is the default/provenance tag for org_actual_spend rows
// written by the provider-agnostic POST /api/v1/org_actual_spend endpoint (and the
// backfill value for every pre-#138 row). It MUST match the SQL DEFAULT and the
// COALESCE fallback in InsertOrgActualSpend and the schema.
const OrgSpendSourceManual = "manual"

// InsertOrgActualSpend appends an org-level invoice row. Same accumulation
// semantics as InsertActualSpend (#24): multiple rows per (org, period, source)
// sum at query time, so credit memos and revisions enter as deltas rather than
// overwriting.
//
// An empty o.Source is stored as OrgSpendSourceManual via COALESCE(NULLIF(...))
// so the existing provider-agnostic POST caller needs no change and its rows are
// tagged 'manual' — the correct provenance for a hand-posted finance figure.
func (d *DB) InsertOrgActualSpend(ctx context.Context, o OrgActualSpend) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO org_actual_spend (org, period, actual_paid_micro, source, ts)
		VALUES (?, ?, ?, COALESCE(NULLIF(?, ''), 'manual'), ?)`,
		o.Org, o.Period, o.ActualPaidMicro, o.Source, o.Timestamp,
	)
	return err
}

// OrgActualSpendNet returns the net recorded actual spend for an (org, period)
// SCOPED TO ONE SOURCE, in integer micro-dollars — the SUM of every
// org_actual_spend delta row for that (org, period, source) triple (#24
// accumulation model), so credit memos and revisions net out. Returns 0 (no error)
// when no such row exists.
//
// The source scope is load-bearing (#138 review R1): a provider poller reconciles
// its month-to-date total against ONLY its own source's net, so it can never post a
// negative delta that cannibalizes another provider's (or a manual) row sharing the
// same (org, period). The allocation read path (the `inv` CTE) deliberately does
// NOT filter by source — it sums across all sources so the org total stays complete
// once multiple pollers + manual rows coexist. period/source are bound as
// parameters (no interpolation).
func (d *DB) OrgActualSpendNet(ctx context.Context, org, period, source string) (int64, error) {
	var net sql.NullInt64
	if err := d.db.QueryRowContext(ctx, `
		SELECT SUM(actual_paid_micro) FROM org_actual_spend
		WHERE org = ? AND period = ? AND source = ?`,
		org, period, source,
	).Scan(&net); err != nil {
		return 0, err
	}
	// SUM over zero rows is SQL NULL, not 0 — NullInt64 maps that to a 0 net.
	if !net.Valid {
		return 0, nil
	}
	return net.Int64, nil
}

// OrgActualSpendTotal is one (org, period) net actual-paid roll-up returned by
// OrgActualSpendTotals for the finance read-back endpoint (#42). ActualPaidUSD
// is dollars (micro→dollars at the store boundary); Entries is the number of
// accumulation rows behind the net — an audit hint (1 = a single invoice, >1 =
// corrections / credit memos / poller deltas).
type OrgActualSpendTotal struct {
	Org           string
	Period        string
	ActualPaidUSD float64
	Entries       int
}

// OrgActualSpendTotals returns the net actual-paid roll-up per (org, period) at
// or after since's calendar month, for the finance read-back endpoint (#42,
// #24). It SUMs the accumulation rows within each (org, period) — SUMMED ACROSS
// ALL SOURCES (manual + every provider poller) — so credit memos and poller
// deltas net out and the figure is the org's true recorded actual spend. This
// deliberately does NOT filter by source (unlike OrgActualSpendNet, whose
// source scope exists only to keep a poller's reconciliation from cannibalizing
// another feed): finance reads the complete cross-source total.
//
// since is windowed by month: a row's period (YYYY-MM) is included when it is
// lexicographically >= since's month (YYYY-MM). Lexicographic and chronological
// order coincide for the canonical zero-padded YYYY-MM form that validatePeriod
// enforces on writes. org, when non-empty, filters to an exact org match;
// empty returns every org. Results are ordered by (period, org) for a stable
// timeline. The org-filtered path seeks idx_org_actual_spend_org_period_nu (org
// leads the index); the all-orgs path cannot seek it (org unconstrained) and
// sorts — negligible here, as org_actual_spend is an org×month-scale table.
//
// Returns an empty (non-nil) slice when nothing matches. since/org are bound as
// parameters (no interpolation).
func (d *DB) OrgActualSpendTotals(ctx context.Context, since time.Time, org string) ([]OrgActualSpendTotal, error) {
	sincePeriod := since.UTC().Format("2006-01")
	// org filter is applied via a (? = '' OR org = ?) guard so the single query
	// serves both the all-orgs and single-org cases without string-built SQL.
	rows, err := d.db.QueryContext(ctx, `
		SELECT org, period, SUM(actual_paid_micro), COUNT(*)
		FROM org_actual_spend
		WHERE period >= ? AND (? = '' OR org = ?)
		GROUP BY org, period
		ORDER BY period, org`,
		sincePeriod, org, org,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	// Non-nil so an empty result marshals to [] (the API contract), not null.
	out := make([]OrgActualSpendTotal, 0)
	for rows.Next() {
		var t OrgActualSpendTotal
		var netMicro int64
		if err := rows.Scan(&t.Org, &t.Period, &netMicro, &t.Entries); err != nil {
			return nil, err
		}
		// micro→dollars ONCE at the boundary (#69), after the exact integer SUM.
		t.ActualPaidUSD = MicroToDollars(netMicro)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// CapturedTokensByDayModel returns, per normalized model, the per-class token
// totals ALREADY captured per-request for one UTC calendar day by the realtime
// paths (source IN ('jsonl','proxy','codex-rollout')) — the baseline the org
// pollers subtract from the org-level daily aggregate so they ingest only the
// REMAINDER (#138), never re-counting spend the watcher/proxy/Codex collector
// already recorded.
//
// EVERY per-request capture source belongs in this list. 'codex-rollout' (#464)
// is here because Codex runs on an OpenAI API key: its per-call rows and the
// OpenAI Usage poller's org aggregate describe the SAME spend, so omitting it
// would make the poller's remainder re-ingest every Codex token a second time.
// The rule for a future capture source: if it writes one row per API call, it
// MUST be added here, or the org poller for its provider will double-count it.
//
// day is snapped to its UTC midnight; the window is [dayStart, dayStart+24h),
// bound as explicit UTC timestamps so a non-UTC host cannot mis-window the day
// (matches the #180/#197 UTC-binding discipline). Rows are grouped by RAW model
// in SQL, then folded by NormalizeModel in Go so date/version variants of the
// same model merge onto one key.
//
// provider filters the result to a single provider's models (e.g. "anthropic"):
// a captured row whose normalized model is not in the price table, or belongs to
// a different provider, is dropped via ProviderOf. This keeps the subtraction
// honest — only Anthropic captured tokens offset the Anthropic Admin aggregate.
// Admin-poller (source='anthropic-admin') and manual (source='api') rows are
// EXCLUDED from the baseline: the former is the remainder feed itself (including
// it would make the poller subtract its own prior output and converge to zero),
// the latter is reconciled invoice cost, not per-request capture.
//
// Returns an empty (non-nil) map when the day has no captured Anthropic events.
func (d *DB) CapturedTokensByDayModel(ctx context.Context, day time.Time, provider string) (map[string]CostUsage, error) {
	dayStart := day.UTC().Truncate(24 * time.Hour)
	dayEnd := dayStart.Add(24 * time.Hour)
	rows, err := d.db.QueryContext(ctx, `
		SELECT model,
		       SUM(input_tok), SUM(output_tok), SUM(cache_read),
		       SUM(cache_write_5m), SUM(cache_write_1h)
		FROM token_events
		WHERE source IN ('jsonl', 'proxy', 'codex-rollout') AND ts >= ? AND ts < ?
		GROUP BY model`,
		dayStart, dayEnd,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]CostUsage)
	for rows.Next() {
		var model string
		var in, o, cr, w5, w1 int
		if err := rows.Scan(&model, &in, &o, &cr, &w5, &w1); err != nil {
			return nil, err
		}
		norm := NormalizeModel(model)
		if ProviderOf(norm) != provider {
			continue
		}
		u := out[norm]
		u.Input += in
		u.Output += o
		u.CacheRead += cr
		u.CacheWrite5m += w5
		u.CacheWrite1h += w1
		out[norm] = u
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ActualSpendForDeveloper returns the developer's allocated actual spend for all
// periods at or after since's month. Resolution is PER BILLING PERIOD (#23, #41,
// #40). Accounting identity (Option A): within an org and period, the tier-1
// allocations of its ACTIVE MEMBERS plus the org-fallback slices sum to that
// org's org_total. (A per-developer invoice from someone who is NOT an active
// member of the org — e.g. a contractor, or a row posted before the developer
// was enrolled — is independent spend, counted for that developer but outside
// any org's identity. That asymmetry is DELIBERATE (#94 item 1, decision A):
// actual_spend carries no org column, so a non-member's invoice has no org to
// reconcile against — it is independent spend by design, not a gap. Strict
// whole-org reconciliation would require adding org linkage to actual_spend,
// deferred until a real requirement appears per the correct-from-start mandate.)
// For each period p:
//
//  1. Tier 1 — if the developer has a per-developer actual_spend row in p, that
//     row IS their allocation (precise per-seat invoice). "Presence wins": a
//     zero-amount row (credit memo / correction) still counts as tier-1, so it
//     does not fall through to the org fallback.
//  2. Tier 2 — otherwise, if the developer is an active period_membership seat of
//     an org with an invoice in p, they receive a share of the REMAINDER after
//     tier-1: (org_total[p] - Σ tier-1 of active members in p) / (active seats in
//     p - tier-1 members in p). So per-dev rows and org-fallback slices in the
//     same org/period reconcile to org_total (#40) instead of leaving a gap.
//
// Returns 0 when the developer matches neither tier in any invoiced period.
// MAX(remainder, 0) and NULLIF(seats, 0) keep the result non-negative and
// division-safe; when active members' tier-1 sum exceeds org_total the
// org-fallback share clamps to 0 rather than erroring. That clamp is currently
// silent at the SQL layer; OverBudgetPeriods + a WARN at ingestion surface it
// as a finance data-quality signal (#94 item 2).
func (d *DB) ActualSpendForDeveloper(ctx context.Context, developer string, since time.Time) (float64, error) {
	sincePeriod := since.UTC().Format("2006-01")
	// allocMicro is in (possibly fractional) micro-dollars: the tier-1 term is an
	// exact integer-micro SUM, while the org-fallback remainder split is a real
	// division (the dividend is CAST to REAL so SQLite does fractional, not
	// truncating-integer, division). Converted to dollars at the boundary.
	var allocMicro float64
	if err := d.db.QueryRowContext(ctx, actualSpendCTE+`
		SELECT
			COALESCE((SELECT SUM(paid_micro) FROM as1 WHERE developer = ?), 0)
			+
			COALESCE((
				SELECT SUM(CAST(MAX(inv.total_micro - t1agg.t1sum, 0) AS REAL) / NULLIF(seats.n - t1agg.t1cnt, 0))
				FROM active a
				JOIN inv   ON inv.org = a.org AND inv.period = a.period
				JOIN seats ON seats.org = a.org AND seats.period = a.period
				JOIN t1agg ON t1agg.org = a.org AND t1agg.period = a.period
				WHERE a.developer = ?
				  AND NOT EXISTS (SELECT 1 FROM as1 WHERE as1.developer = a.developer AND as1.period = a.period)
			), 0)`,
		sincePeriod, sincePeriod, developer, developer,
	).Scan(&allocMicro); err != nil {
		return 0, err
	}
	return allocMicro / float64(MicroPerUSD), nil
}

// actualSpendCTE is the shared per-period CTE chain for the two allocation
// queries (#40/#41): as1 (per-developer per-period tier-1 invoices), inv
// (per-(org,period) org invoice totals), active (DISTINCT active membership
// seats per invoiced (org,period)), seats (active seat count per (org,period)),
// and t1agg (tier-1 sum + count among active seats per (org,period), used to
// subtract tier-1 before splitting the remainder). Both consuming queries bind
// (sincePeriod /*as1*/, sincePeriod /*inv*/) first, then their own params.
const actualSpendCTE = `
	WITH as1 AS (
		SELECT developer, period, SUM(actual_paid_micro) AS paid_micro
		FROM actual_spend WHERE period >= ? GROUP BY developer, period
	),
	inv AS (
		SELECT org, period, SUM(actual_paid_micro) AS total_micro
		FROM org_actual_spend WHERE period >= ? GROUP BY org, period
	),
	active AS (
		SELECT DISTINCT pm.developer, inv.org, inv.period
		FROM inv
		JOIN period_membership pm ON pm.org = inv.org
		  AND pm.org IS NOT NULL AND pm.org != ''
		  AND pm.period_start <= inv.period
		  AND (pm.period_end IS NULL OR pm.period_end >= inv.period)
	),
	seats AS (
		SELECT org, period, COUNT(*) AS n FROM active GROUP BY org, period
	),
	t1agg AS (
		SELECT a.org, a.period,
		       COALESCE(SUM(s.paid_micro), 0) AS t1sum,
		       COUNT(s.developer)        AS t1cnt
		FROM active a
		LEFT JOIN as1 s ON s.developer = a.developer AND s.period = a.period
		GROUP BY a.org, a.period
	)`

// actualSpendCTEBounded is actualSpendCTE with an exclusive upper period bound
// added to the two period-filtered leaves (as1, inv), so ActualSpendAllWindow
// can window [sincePeriod, untilPeriod) (#276). It is DERIVED from the shared
// constant, not copied, so the allocation logic can never drift between the
// bounded and open-ended forms; the derivation adds exactly one `AND period < ?`
// bound param to each of the two `WHERE period >= ?` leaves — see the arg order
// documented at ActualSpendAllWindow. OverBudgetPeriods keeps using the
// unbounded actualSpendCTE and is unaffected.
var actualSpendCTEBounded = strings.ReplaceAll(
	actualSpendCTE, "WHERE period >= ?", "WHERE period >= ? AND period < ?")

// ActualSpendAll returns the per-developer total for all developers reachable
// via actual_spend (tier 1) or via org_actual_spend + active-in-window
// period_membership (tier 2, #41), with per-developer tier-1 rows subtracted
// from the org total before the remainder is split (#40), so a developer's
// per-period slices and the org-fallback slices reconcile to org_total.
//
// Resolution is PER PERIOD (matches ActualSpendForDeveloper): part A sums each
// developer's own tier-1 invoices; part B adds the org-fallback remainder share
// for periods where the developer is an active seat WITHOUT a tier-1 row. There
// is no window-level "tier-1 wins entirely" shortcut — a developer who is tier-1
// in one period and org-fallback in another is handled correctly in each.
//
// Mirrors DeveloperCosts in shape so callers can build per-developer Spend
// Leverage without an N+1 query.
func (d *DB) ActualSpendAll(ctx context.Context, since time.Time) (map[string]float64, error) {
	return d.ActualSpendAllWindow(ctx, since, time.Time{})
}

// ActualSpendAllWindow is the half-open [since, until) form of ActualSpendAll
// (#276): the SpendLeverage denominator, bounded above so windowed scoring pairs
// spend with the same period range as its cost and outcomes. A zero `until` is
// open-ended and behaves exactly like ActualSpendAll.
//
// actual_spend is MONTHLY, so the upper bound is applied at month grain:
// untilPeriod = until's month ("2006-01"), and a period row is in-window when
// `period >= sincePeriod AND period < untilPeriod`. For the month-aligned
// windows this endpoint exists to serve (until=2026-04-01 → "Team spend through
// March") this is exact. A non-month-aligned `until` (mid-month) drops the whole
// partial month rather than pro-rating it — monthly invoices cannot be split —
// which is documented, coarse, and consistent with the ts reads' exclusivity.
//
// Bound-arg order for the bounded Part B (actualSpendCTEBounded): the two
// period-filtered leaves are as1 then inv, each now `period >= ? AND period < ?`,
// so the CTE consumes (sincePeriod, untilPeriod, sincePeriod, untilPeriod) before
// the outer query — which adds none.
func (d *DB) ActualSpendAllWindow(ctx context.Context, since, until time.Time) (map[string]float64, error) {
	sincePeriod := since.UTC().Format("2006-01")
	bounded := !until.IsZero()
	var untilPeriod string
	if bounded {
		untilPeriod = until.UTC().Format("2006-01")
	}
	out := map[string]float64{}

	// Part A: each developer's own tier-1 invoices (per-period sum). GROUP BY
	// emits one row per developer regardless of whether the sum is positive.
	partAWhere := "period >= ?"
	partAArgs := []any{sincePeriod}
	if bounded {
		partAWhere = "period >= ? AND period < ?"
		partAArgs = []any{sincePeriod, untilPeriod}
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT developer, COALESCE(SUM(actual_paid_micro), 0)
		FROM actual_spend
		WHERE `+partAWhere+`
		GROUP BY developer`,
		partAArgs...,
	)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var dev string
		var totalMicro int64
		if err := rows.Scan(&dev, &totalMicro); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out[dev] = MicroToDollars(totalMicro)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Part B: org-fallback remainder share, per (active seat, org, period), only
	// for periods where the developer has NO tier-1 row (NOT EXISTS as1). Each
	// period's remainder (org_total - tier-1 sum among active seats) is split
	// across the non-tier-1 active seats. NULLIF guards the all-tier-1 case
	// (NULL slice → skipped). Summed onto part A per developer.
	cte := actualSpendCTE
	// as1 and inv each bind sincePeriod; the bounded CTE adds untilPeriod to each.
	cteArgs := []any{sincePeriod, sincePeriod}
	if bounded {
		cte = actualSpendCTEBounded
		cteArgs = []any{sincePeriod, untilPeriod, sincePeriod, untilPeriod}
	}
	rows2, err := d.db.QueryContext(ctx, cte+`
		SELECT a.developer,
		       CAST(MAX(inv.total_micro - t1agg.t1sum, 0) AS REAL) / NULLIF(seats.n - t1agg.t1cnt, 0)
		FROM active a
		JOIN inv   ON inv.org = a.org AND inv.period = a.period
		JOIN seats ON seats.org = a.org AND seats.period = a.period
		JOIN t1agg ON t1agg.org = a.org AND t1agg.period = a.period
		WHERE NOT EXISTS (SELECT 1 FROM as1 WHERE as1.developer = a.developer AND as1.period = a.period)`,
		cteArgs...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows2.Close() }()
	for rows2.Next() {
		var dev string
		// sliceMicro is the developer's fractional micro-dollar remainder share.
		var sliceMicro sql.NullFloat64
		if err := rows2.Scan(&dev, &sliceMicro); err != nil {
			return nil, err
		}
		if sliceMicro.Valid {
			out[dev] += sliceMicro.Float64 / float64(MicroPerUSD)
		}
	}
	return out, rows2.Err()
}

// OverBudgetPeriod is one (org, period) whose ACTIVE MEMBERS' tier-1 invoices
// sum to more than the org's contract total — the condition under which the
// org-fallback share clamps to 0 (#94 item 2). Overage is Tier1Sum - OrgTotal
// and is always > 0 for a returned row.
type OverBudgetPeriod struct {
	Org      string
	Period   string
	OrgTotal float64
	Tier1Sum float64
	Overage  float64
}

// OverBudgetPeriods returns the (org, period) pairs at or after since's month
// where active members' tier-1 per-developer invoices exceed the org's
// org_total — i.e. where ActualSpendForDeveloper/ActualSpendAll silently clamp
// the org-fallback remainder to 0 via MAX(remainder, 0). Surfacing this is a
// finance data-quality signal: recorded per-seat invoices exceeding the org
// contract is usually a data-entry error (#94 item 2).
//
// Detection reuses the SAME inv (org invoice total) and t1agg (tier-1 sum among
// active seats) aggregates as the allocation queries via actualSpendCTE, so the
// overage predicate (t1agg.t1sum > inv.total_micro) can never drift from the clamp it
// describes. Only active-member tier-1 counts toward t1sum, so a non-member's
// independent invoice (the #94 item-1 orphan) correctly does NOT trip this.
func (d *DB) OverBudgetPeriods(ctx context.Context, since time.Time) ([]OverBudgetPeriod, error) {
	sincePeriod := since.UTC().Format("2006-01")
	rows, err := d.db.QueryContext(ctx, actualSpendCTE+`
		SELECT t1agg.org, t1agg.period, inv.total_micro, t1agg.t1sum
		FROM t1agg
		JOIN inv ON inv.org = t1agg.org AND inv.period = t1agg.period
		WHERE t1agg.t1sum > inv.total_micro
		ORDER BY t1agg.period, t1agg.org`,
		sincePeriod, sincePeriod,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []OverBudgetPeriod
	for rows.Next() {
		var p OverBudgetPeriod
		var orgTotalMicro, tier1SumMicro int64
		if err := rows.Scan(&p.Org, &p.Period, &orgTotalMicro, &tier1SumMicro); err != nil {
			return nil, err
		}
		p.OrgTotal = MicroToDollars(orgTotalMicro)
		p.Tier1Sum = MicroToDollars(tier1SumMicro)
		p.Overage = p.Tier1Sum - p.OrgTotal
		out = append(out, p)
	}
	return out, rows.Err()
}

// HierarchyRow is one org_hierarchy assignment: a developer's full org-structure
// placement (#232). developer is the canonical identity (the org_hierarchy
// PRIMARY KEY). division and org are nullable in the schema; on read a SQL NULL
// is normalized to "" so callers never handle a null pointer, and on the write
// path the API layer already sends "" for an unset division/org.
type HierarchyRow struct {
	Developer string `json:"developer"`
	Team      string `json:"team"`
	Division  string `json:"division"`
	Org       string `json:"org"`
}

// ListHierarchy returns every org_hierarchy row ordered by developer (#232),
// backing GET /api/v1/org_hierarchy — the read half of the write surface that
// lets an operator confirm what team mode will aggregate on. It is the full-row
// counterpart to TeamsForDevelopers (which returns only developer→team for the
// score join). Nullable division/org columns are COALESCEd to "". Always
// returns a non-nil (possibly empty) slice.
func (d *DB) ListHierarchy(ctx context.Context) ([]HierarchyRow, error) {
	// NOTE(multi-tenancy): this reads all of org_hierarchy with no WHERE
	// clause. When tenant_id lands (see CLAUDE.md), this MUST gain
	// `WHERE tenant_id = ?` or it becomes a cross-tenant leak.
	rows, err := d.db.QueryContext(ctx,
		`SELECT developer, team, COALESCE(division, ''), COALESCE(org, '')
		 FROM org_hierarchy ORDER BY developer`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []HierarchyRow{}
	for rows.Next() {
		var r HierarchyRow
		if err := rows.Scan(&r.Developer, &r.Team, &r.Division, &r.Org); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TeamsForDevelopers returns a developer→team map for every row in
// org_hierarchy, so a caller resolving teams for a set of developers does it
// in one query instead of a per-developer N+1 (#94 item 3). Mirrors the
// bulk-fetch shape of ActualSpendAll / DeveloperCosts. developer is the
// org_hierarchy PRIMARY KEY, so each appears at most once; developers absent
// from org_hierarchy are simply not in the map — callers treat a missing key
// as "no team". An empty-string team IS a stored value and is returned as a
// present key with value "", distinct from absence. Always returns a non-nil
// (possibly empty) map.
func (d *DB) TeamsForDevelopers(ctx context.Context) (map[string]string, error) {
	// NOTE(multi-tenancy): this reads all of org_hierarchy with no WHERE
	// clause. When tenant_id lands (see CLAUDE.md), this MUST gain
	// `WHERE tenant_id = ?` or it becomes a cross-tenant leak.
	rows, err := d.db.QueryContext(ctx, `SELECT developer, team FROM org_hierarchy`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var dev, team string
		if err := rows.Scan(&dev, &team); err != nil {
			return nil, err
		}
		out[dev] = team
	}
	return out, rows.Err()
}

// DivisionsForDevelopers returns a developer->division map for every row in
// org_hierarchy, the division-level (#270) counterpart to TeamsForDevelopers.
// It has the identical bulk-fetch contract so the k-anonymized score join can
// swap one map for the other (the fold is level-agnostic, see
// scoring.AggregateTeamsKAnon): developer is the org_hierarchy PRIMARY KEY so
// each appears at most once, and a developer with no row is simply absent from
// the map (callers treat a missing key as the unnamed group).
//
// division is NULLABLE in the schema (unlike team), so it is COALESCEd to "" on
// read: a developer whose division is NULL or empty is a PRESENT key with value
// "" (distinct from an absent developer). The k-anon fold treats "" as the
// unnamed group and rolls those developers into the "other" cohort rather than a
// spurious blank-named division row -- the documented empty-division bucket.
// Always returns a non-nil (possibly empty) map.
func (d *DB) DivisionsForDevelopers(ctx context.Context) (map[string]string, error) {
	// NOTE(multi-tenancy): this reads all of org_hierarchy with no WHERE
	// clause. When tenant_id lands (see CLAUDE.md), this MUST gain
	// `WHERE tenant_id = ?` or it becomes a cross-tenant leak.
	rows, err := d.db.QueryContext(ctx,
		`SELECT developer, COALESCE(division, '') FROM org_hierarchy`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var dev, division string
		if err := rows.Scan(&dev, &division); err != nil {
			return nil, err
		}
		out[dev] = division
	}
	return out, rows.Err()
}

// UpsertDeveloperAlias maps a raw identifier (OS username, GitHub login) to the
// canonical developer identity used for scoring (#125). Resolution is
// SINGLE-HOP: a canonical must never itself be an alias, and an alias must never
// itself be a canonical. This method is the sole enforcement point for that
// invariant, so the score-join read path can trust one lookup resolves fully.
//
// BEGIN IMMEDIATE (via LevelSerializable, matching the schema-rebuild and
// backfill transactions elsewhere in this file) takes the write lock up front
// so the chain check and the upsert are one atomic unit -- two racing calls
// can't each pass the check and then jointly form a chain. SetMaxOpenConns(1)
// already serialises writers, so this is belt-and-braces, not the sole guard.
func (d *DB) UpsertDeveloperAlias(ctx context.Context, alias, canonical string) error {
	if alias == "" {
		return errors.New("developer_alias: alias must not be empty")
	}
	if canonical == "" {
		return errors.New("developer_alias: canonical must not be empty")
	}
	if alias == canonical {
		return errors.New("developer_alias: alias and canonical must differ")
	}

	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Chain guard, single-hop invariant:
	//   1. canonical must not already be an alias (else alias -> canonical -> X).
	//   2. alias must not already be a canonical (else Y -> alias -> canonical).
	// The update case (re-pointing an existing alias row to a new canonical)
	// passes both checks naturally: check 1 keys on the new canonical, and in
	// check 2 the existing row's canonical can never equal the new alias because
	// the alias == canonical self-map is already rejected above.
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM developer_alias WHERE alias = ?`, canonical,
	).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("developer_alias: mapping would create a chain; aliases must resolve in one hop")
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM developer_alias WHERE canonical = ?`, alias,
	).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("developer_alias: mapping would create a chain; aliases must resolve in one hop")
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO developer_alias (alias, canonical, ts)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(alias) DO UPDATE SET canonical = excluded.canonical, ts = excluded.ts`,
		alias, canonical,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteDeveloperAlias removes the mapping for alias and reports whether a row
// existed (#125). A missing alias is not an error — the found flag lets the API
// layer return 404 without a second lookup.
func (d *DB) DeleteDeveloperAlias(ctx context.Context, alias string) (bool, error) {
	res, err := d.db.ExecContext(ctx, `DELETE FROM developer_alias WHERE alias = ?`, alias)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// DeveloperAliases returns the full alias->canonical map in one query (#125),
// mirroring TeamsForDevelopers' bulk shape so the score join resolves identities
// without an N+1. Always returns a non-nil (possibly empty) map.
func (d *DB) DeveloperAliases(ctx context.Context) (map[string]string, error) {
	// NOTE(multi-tenancy): this reads all of developer_alias with no WHERE
	// clause. When tenant_id lands (see CLAUDE.md), this MUST gain
	// `WHERE tenant_id = ?` or it becomes a cross-tenant leak.
	rows, err := d.db.QueryContext(ctx, `SELECT alias, canonical FROM developer_alias`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var alias, canonical string
		if err := rows.Scan(&alias, &canonical); err != nil {
			return nil, err
		}
		out[alias] = canonical
	}
	return out, rows.Err()
}

// --- GDPR Art. 15 (access) / Art. 17 (erasure) — #184 ---

// developerPIITables is the compile-time allowlist of tables that carry a
// per-developer `developer` column and therefore hold personal data subject to
// erasure/export (#184). It is a FIXED literal, never derived from request
// input, so interpolating a name from this slice into a DELETE/SELECT statement
// (below) is not SQL injection — the developer identifiers themselves are ALWAYS
// bound as `?` parameters. Keep this list complete: a table omitted here is a
// silent compliance hole (a DSAR that fails to erase or disclose real PII).
//
// Deliberately EXCLUDED:
//   - org_actual_spend — org-level, no developer column (not personal data).
//   - webhook_payloads — raw GitHub bodies that MAY embed a contributor name or
//     email; erasing structured rows there is out of scope for #184 and the
//     residual is documented in docs/privacy.md, mitigated by the 90-day/50k-row
//     retention bound (PruneWebhookPayloads). Do not "fix" that by adding it here
//     without a real re-derivation story — the bodies are keyed by delivery, not
//     developer, and carry third-party (not data-subject) identifiers.
//   - watcher_checkpoint — derived operational tail-state, no developer column.
//
// developer_alias is handled separately (it is keyed by alias/canonical, not a
// `developer` column) but is equally covered by both erase and export.
var developerPIITables = []string{
	"token_events",
	"outcomes",
	"actual_spend",
	"org_hierarchy",
	"period_membership",
	"quality_events",
	"quality_history",
}

// rowQuerier is the read surface shared by *sql.DB and *sql.Tx, so the
// identifier-set resolution runs identically inside or outside a transaction.
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// developerIdentifierSet resolves the FULL set of raw identifiers that map to
// the same person as id, honoring the single-hop developer_alias contract (#125,
// #184):
//
//   - canonical is id resolved through the alias map (id itself if it is not an
//     alias — an unmapped identifier is its own canonical);
//   - ids is {canonical} ∪ {every alias whose canonical == canonical} — the
//     reverse lookup, so rows stored under EITHER the canonical id OR any raw
//     login that aliases to it are all covered.
//
// A request naming a raw login and a request naming its canonical therefore
// resolve to the identical set, which is what makes erasure/export
// alias-correct. ids is returned in deterministic (sorted) order so the bound
// IN-clause and any test assertion are stable.
func developerIdentifierSet(ctx context.Context, q rowQuerier, id string) (canonical string, ids []string, err error) {
	rows, err := q.QueryContext(ctx, `SELECT alias, canonical FROM developer_alias`)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = rows.Close() }()

	aliasToCanon := map[string]string{}
	canonToAliases := map[string][]string{}
	for rows.Next() {
		var alias, canon string
		if err := rows.Scan(&alias, &canon); err != nil {
			return "", nil, err
		}
		aliasToCanon[alias] = canon
		canonToAliases[canon] = append(canonToAliases[canon], alias)
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}

	canonical = id
	if c, ok := aliasToCanon[id]; ok {
		canonical = c
	}
	set := map[string]struct{}{canonical: {}}
	for _, a := range canonToAliases[canonical] {
		set[a] = struct{}{}
	}
	ids = make([]string, 0, len(set))
	for k := range set {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	return canonical, ids, nil
}

// inClause builds a parameter-placeholder list (`?,?,?`) and the matching []any
// args for a bound IN (...) predicate. The identifier values are NEVER
// interpolated into SQL text — only `?` placeholders are — so an identifier
// containing SQL metacharacters is inert.
func inClause(ids []string) (placeholders string, args []any) {
	args = make([]any, len(ids))
	marks := make([]string, len(ids))
	for i, id := range ids {
		marks[i] = "?"
		args[i] = id
	}
	return strings.Join(marks, ","), args
}

// EraseDeveloper deletes EVERY stored row for the person named by id — the GDPR
// Art. 17 right-to-erasure primitive (#184). It resolves id through the alias
// map first (single-hop), computes the full identifier set (canonical + every
// alias pointing to it), then deletes rows keyed by any of those identifiers
// across all developer-PII tables AND the developer_alias rows themselves, in
// ONE transaction — a partial erasure is a compliance failure, so it is
// all-or-nothing. It returns accurate per-table deleted-row counts gathered
// from within the transaction.
//
// Idempotent: a second call (or a call for a never-seen id) resolves to a set
// with no rows, deletes nothing, and returns all-zero counts with no error. The
// API layer maps an all-zero result to 404.
//
// Isolation LevelSerializable (BEGIN IMMEDIATE) takes the write lock up front so
// the alias read and the cascade are one atomic unit, matching UpsertDeveloperAlias.
func (d *DB) EraseDeveloper(ctx context.Context, id string) (map[string]int64, error) {
	if id == "" {
		return nil, errors.New("EraseDeveloper: id must not be empty")
	}
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	canonical, ids, err := developerIdentifierSet(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	placeholders, args := inClause(ids)

	counts := make(map[string]int64, len(developerPIITables)+1)
	for _, table := range developerPIITables {
		// table is from the compile-time allowlist above (never request input);
		// ids are bound via ? placeholders. See developerPIITables / inClause.
		res, err := tx.ExecContext(ctx,
			"DELETE FROM "+table+" WHERE developer IN ("+placeholders+")", args...)
		if err != nil {
			return nil, fmt.Errorf("EraseDeveloper: delete %s: %w", table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("EraseDeveloper: rows affected %s: %w", table, err)
		}
		counts[table] = n
	}

	// developer_alias: remove every alias row that points at this canonical
	// identity. This covers the row for the requested raw login (when id was an
	// alias) and every sibling alias, so no dangling mapping to an erased person
	// survives.
	res, err := tx.ExecContext(ctx,
		`DELETE FROM developer_alias WHERE canonical = ?`, canonical)
	if err != nil {
		return nil, fmt.Errorf("EraseDeveloper: delete developer_alias: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("EraseDeveloper: rows affected developer_alias: %w", err)
	}
	counts["developer_alias"] = n

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("EraseDeveloper: commit: %w", err)
	}
	return counts, nil
}

// DeveloperExport is the GDPR Art. 15 data-subject-access artifact (#184): every
// stored row for one person, grouped by table. Identifiers is the full resolved
// set (canonical + aliases) the rows were gathered under, so the recipient can
// see which raw logins were merged. Slice fields are nil (JSON null) when a table
// holds nothing for the subject.
type DeveloperExport struct {
	Developer        string                   `json:"developer"` // canonical identity
	Identifiers      []string                 `json:"identifiers"`
	TokenEvents      []ExportTokenEvent       `json:"token_events"`
	Outcomes         []ExportOutcome          `json:"outcomes"`
	ActualSpend      []ExportActualSpend      `json:"actual_spend"`
	OrgHierarchy     []ExportOrgHierarchy     `json:"org_hierarchy"`
	PeriodMembership []ExportPeriodMembership `json:"period_membership"`
	QualityEvents    []ExportQualityEvent     `json:"quality_events"`
	QualityHistory   []ExportQualityHistory   `json:"quality_history"`
	DeveloperAlias   []ExportDeveloperAlias   `json:"developer_alias"`
}

// RowCount is the total number of stored rows in the export across every table.
// The API layer maps a zero total to 404 (no data / already erased).
func (e DeveloperExport) RowCount() int {
	return len(e.TokenEvents) + len(e.Outcomes) + len(e.ActualSpend) +
		len(e.OrgHierarchy) + len(e.PeriodMembership) + len(e.QualityEvents) +
		len(e.QualityHistory) + len(e.DeveloperAlias)
}

// Export row types mirror the STORED columns of each PII table (not the insert
// structs), so the DSAR discloses exactly what is at rest. Nullable columns are
// pointers so a genuine NULL renders as JSON null rather than a zero value.

type ExportTokenEvent struct {
	ID             int64     `json:"id"`
	Developer      string    `json:"developer"`
	IssueID        string    `json:"issue_id"`
	Model          string    `json:"model"`
	InputTok       int64     `json:"input_tok"`
	OutputTok      int64     `json:"output_tok"`
	CacheRead      int64     `json:"cache_read"`
	CacheWrite5m   int64     `json:"cache_write_5m"`
	CacheWrite1h   int64     `json:"cache_write_1h"`
	CostMicro      int64     `json:"cost_micro"`
	Source         string    `json:"source"`
	Fidelity       string    `json:"fidelity"`
	IdempotencyKey *string   `json:"idempotency_key"`
	Timestamp      time.Time `json:"ts"`
}

type ExportOutcome struct {
	ID             int64     `json:"id"`
	Developer      string    `json:"developer"`
	IssueID        string    `json:"issue_id"`
	PRNumber       *int64    `json:"pr_number"`
	Weight         float64   `json:"weight"`
	Quality        float64   `json:"quality"`
	MergeCommitSHA *string   `json:"merge_commit_sha"`
	WeightSource   string    `json:"weight_source"`
	Additions      *int64    `json:"additions"`
	Deletions      *int64    `json:"deletions"`
	ChangedFiles   *int64    `json:"changed_files"`
	Source         string    `json:"source"`
	WorkType       string    `json:"work_type"`
	WorkTypeSource string    `json:"work_type_source"`
	PushDay        *string   `json:"push_day"`
	Timestamp      time.Time `json:"ts"`
}

type ExportActualSpend struct {
	ID              int64     `json:"id"`
	Developer       string    `json:"developer"`
	Period          string    `json:"period"`
	ActualPaidMicro int64     `json:"actual_paid_micro"`
	Timestamp       time.Time `json:"ts"`
}

type ExportOrgHierarchy struct {
	Developer string  `json:"developer"`
	Team      string  `json:"team"`
	Division  *string `json:"division"`
	Org       *string `json:"org"`
}

type ExportPeriodMembership struct {
	Developer   string  `json:"developer"`
	Org         string  `json:"org"`
	PeriodStart string  `json:"period_start"`
	PeriodEnd   *string `json:"period_end"`
}

type ExportQualityEvent struct {
	ID         int64     `json:"id"`
	OutcomeID  int64     `json:"outcome_id"`
	Developer  string    `json:"developer"`
	IssueID    string    `json:"issue_id"`
	EventType  string    `json:"event_type"`
	SourceRef  string    `json:"source_ref"`
	EventTS    time.Time `json:"event_ts"`
	RecordedAt time.Time `json:"recorded_at"`
}

type ExportQualityHistory struct {
	ID         int64     `json:"id"`
	OutcomeID  int64     `json:"outcome_id"`
	Developer  string    `json:"developer"`
	IssueID    string    `json:"issue_id"`
	OldQuality float64   `json:"old_quality"`
	NewQuality float64   `json:"new_quality"`
	Reason     string    `json:"reason"`
	SourceRef  string    `json:"source_ref"`
	Timestamp  time.Time `json:"ts"`
}

type ExportDeveloperAlias struct {
	Alias     string    `json:"alias"`
	Canonical string    `json:"canonical"`
	Timestamp time.Time `json:"ts"`
}

// ExportDeveloper gathers every stored row for the person named by id — the GDPR
// Art. 15 access artifact (#184). It resolves the identifier set exactly as
// EraseDeveloper does (single-hop alias + reverse lookup), so a request by a raw
// login and by its canonical return the identical record. A never-seen id
// returns an all-empty export (RowCount()==0); the API layer maps that to 404.
func (d *DB) ExportDeveloper(ctx context.Context, id string) (DeveloperExport, error) {
	if id == "" {
		return DeveloperExport{}, errors.New("ExportDeveloper: id must not be empty")
	}
	canonical, ids, err := developerIdentifierSet(ctx, d.db, id)
	if err != nil {
		return DeveloperExport{}, err
	}
	placeholders, args := inClause(ids)
	exp := DeveloperExport{Developer: canonical, Identifiers: ids}

	// token_events
	if err := d.queryRows(ctx,
		`SELECT id, developer, issue_id, model, input_tok, output_tok, cache_read,
		        cache_write_5m, cache_write_1h, cost_micro, source, fidelity,
		        idempotency_key, ts
		 FROM token_events WHERE developer IN (`+placeholders+`) ORDER BY id`, args,
		func(rows *sql.Rows) error {
			var r ExportTokenEvent
			var key sql.NullString
			if err := rows.Scan(&r.ID, &r.Developer, &r.IssueID, &r.Model, &r.InputTok,
				&r.OutputTok, &r.CacheRead, &r.CacheWrite5m, &r.CacheWrite1h, &r.CostMicro,
				&r.Source, &r.Fidelity, &key, &r.Timestamp); err != nil {
				return err
			}
			r.IdempotencyKey = nullStr(key)
			exp.TokenEvents = append(exp.TokenEvents, r)
			return nil
		}); err != nil {
		return DeveloperExport{}, err
	}

	// outcomes
	if err := d.queryRows(ctx,
		`SELECT id, developer, issue_id, pr_number, weight, quality, merge_commit_sha,
		        weight_source, additions, deletions, changed_files, source,
		        COALESCE(work_type, 'feature'), COALESCE(work_type_source, 'legacy'),
		        push_day, ts
		 FROM outcomes WHERE developer IN (`+placeholders+`) ORDER BY id`, args,
		func(rows *sql.Rows) error {
			var r ExportOutcome
			var pr, add, del, cf sql.NullInt64
			var sha, pushDay sql.NullString
			if err := rows.Scan(&r.ID, &r.Developer, &r.IssueID, &pr, &r.Weight, &r.Quality,
				&sha, &r.WeightSource, &add, &del, &cf, &r.Source,
				&r.WorkType, &r.WorkTypeSource, &pushDay, &r.Timestamp); err != nil {
				return err
			}
			r.PRNumber, r.Additions, r.Deletions, r.ChangedFiles = nullInt(pr), nullInt(add), nullInt(del), nullInt(cf)
			r.MergeCommitSHA, r.PushDay = nullStr(sha), nullStr(pushDay)
			exp.Outcomes = append(exp.Outcomes, r)
			return nil
		}); err != nil {
		return DeveloperExport{}, err
	}

	// actual_spend
	if err := d.queryRows(ctx,
		`SELECT id, developer, period, actual_paid_micro, ts
		 FROM actual_spend WHERE developer IN (`+placeholders+`) ORDER BY id`, args,
		func(rows *sql.Rows) error {
			var r ExportActualSpend
			if err := rows.Scan(&r.ID, &r.Developer, &r.Period, &r.ActualPaidMicro, &r.Timestamp); err != nil {
				return err
			}
			exp.ActualSpend = append(exp.ActualSpend, r)
			return nil
		}); err != nil {
		return DeveloperExport{}, err
	}

	// org_hierarchy
	if err := d.queryRows(ctx,
		`SELECT developer, team, division, org
		 FROM org_hierarchy WHERE developer IN (`+placeholders+`) ORDER BY developer`, args,
		func(rows *sql.Rows) error {
			var r ExportOrgHierarchy
			var div, org sql.NullString
			if err := rows.Scan(&r.Developer, &r.Team, &div, &org); err != nil {
				return err
			}
			r.Division, r.Org = nullStr(div), nullStr(org)
			exp.OrgHierarchy = append(exp.OrgHierarchy, r)
			return nil
		}); err != nil {
		return DeveloperExport{}, err
	}

	// period_membership
	if err := d.queryRows(ctx,
		`SELECT developer, org, period_start, period_end
		 FROM period_membership WHERE developer IN (`+placeholders+`) ORDER BY developer, period_start`, args,
		func(rows *sql.Rows) error {
			var r ExportPeriodMembership
			var end sql.NullString
			if err := rows.Scan(&r.Developer, &r.Org, &r.PeriodStart, &end); err != nil {
				return err
			}
			r.PeriodEnd = nullStr(end)
			exp.PeriodMembership = append(exp.PeriodMembership, r)
			return nil
		}); err != nil {
		return DeveloperExport{}, err
	}

	// quality_events
	if err := d.queryRows(ctx,
		`SELECT id, outcome_id, developer, issue_id, event_type, source_ref, event_ts, recorded_at
		 FROM quality_events WHERE developer IN (`+placeholders+`) ORDER BY id`, args,
		func(rows *sql.Rows) error {
			var r ExportQualityEvent
			if err := rows.Scan(&r.ID, &r.OutcomeID, &r.Developer, &r.IssueID, &r.EventType,
				&r.SourceRef, &r.EventTS, &r.RecordedAt); err != nil {
				return err
			}
			exp.QualityEvents = append(exp.QualityEvents, r)
			return nil
		}); err != nil {
		return DeveloperExport{}, err
	}

	// quality_history
	if err := d.queryRows(ctx,
		`SELECT id, outcome_id, developer, issue_id, old_quality, new_quality, reason, source_ref, ts
		 FROM quality_history WHERE developer IN (`+placeholders+`) ORDER BY id`, args,
		func(rows *sql.Rows) error {
			var r ExportQualityHistory
			if err := rows.Scan(&r.ID, &r.OutcomeID, &r.Developer, &r.IssueID, &r.OldQuality,
				&r.NewQuality, &r.Reason, &r.SourceRef, &r.Timestamp); err != nil {
				return err
			}
			exp.QualityHistory = append(exp.QualityHistory, r)
			return nil
		}); err != nil {
		return DeveloperExport{}, err
	}

	// developer_alias: every alias row pointing at this canonical identity.
	if err := d.queryRows(ctx,
		`SELECT alias, canonical, ts FROM developer_alias WHERE canonical = ? ORDER BY alias`,
		[]any{canonical},
		func(rows *sql.Rows) error {
			var r ExportDeveloperAlias
			if err := rows.Scan(&r.Alias, &r.Canonical, &r.Timestamp); err != nil {
				return err
			}
			exp.DeveloperAlias = append(exp.DeveloperAlias, r)
			return nil
		}); err != nil {
		return DeveloperExport{}, err
	}

	return exp, nil
}

// queryRows runs one read and applies scan to each row, closing the rows set and
// propagating any iteration error. Keeps the per-table export blocks free of
// repeated rows.Close()/rows.Err() boilerplate.
func (d *DB) queryRows(ctx context.Context, query string, args []any, scan func(*sql.Rows) error) error {
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func nullStr(n sql.NullString) *string {
	if n.Valid {
		s := n.String
		return &s
	}
	return nil
}

func nullInt(n sql.NullInt64) *int64 {
	if n.Valid {
		v := n.Int64
		return &v
	}
	return nil
}
