package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/repoid"

	// Named, not blank: isPromoteContention needs *sqlite.Error.Code() to tell a
	// LOCK from a permanently unwritable database. The import still registers the
	// driver, so the "sqlite" DSN name is unaffected.
	sqlite "modernc.org/sqlite"
)

// maxModelsNamedInError bounds how many model names an error may interpolate.
// The names are producer-controlled and unbounded in cardinality, so the LIST is
// as much of a flood vector as any single element; logsafe.Str bounds the
// elements, this bounds the list. Matches reprice's report cap so the CLI's
// error and its report agree on how much they will name.
const maxModelsNamedInError = 20

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
-- APPEND-ONLY BY CONSTRUCTION -- no code path mutates it; the table itself is
-- not protected against direct DB access (#604). That is the honest claim for
-- every ledger in this family; only cost_correction_audit carries a schema-level
-- guard, and only against UPDATE (see its comment).
--
-- schemaVersion is deliberately NOT bumped: this is a brand-new table that no
-- pre-#294 read path touches, so an old binary opening this newer DB simply
-- never selects it and keeps working (the schemaVersion bump convention). It
-- carries NO unique index: an audit ledger is append-only and needs no
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
-- APPEND-ONLY BY CONSTRUCTION -- no code path mutates it; the table itself is
-- not protected against direct DB access (#604).
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

-- repo_repair_audit is the append-only AGGREGATE ledger of every
-- tierd repair-repo run (#493) -- the repo-identity twin of reprice_audit.
--
-- WHY IT EXISTS: #491 fixed tierd ship dropping repo on the wire, but that
-- fix is FORWARD-ONLY. InsertTokenEvents deliberately excludes repo from its
-- ON CONFLICT DO UPDATE clause (see insertTokenEventSQL) so that a repo-blind
-- producer replaying a message can never DOWNGRADE a row another producer
-- already qualified. The consequence is that an operator who upgrades and
-- re-ships sees no change at all: every row collides on idempotency_key and
-- keeps repo='unqualified' while the server happily returns 201. Repairing that
-- history therefore needs its own explicit, audited mutator -- exactly the shape
-- tierd reprice already established for cost. repo is a JOIN KEY (the
-- cost<->outcome join in #231), so rewriting it retroactively moves spend
-- between per-repository TIER scores and must leave a durable record.
--
-- One row per distinct TARGET repo slug of one run: repair_id groups the rows of
-- a single operation, developer records the required --developer selector that
-- bounded it, and (from_repo -> to_repo, row_count, cost_micro_sum) captures how
-- much spend moved into that repository's identity. tool_version stamps the
-- binary that ran it (provenance).
--
-- schemaVersion is deliberately NOT bumped: this is a brand-new table that no
-- pre-#493 read path touches, so an old binary opening this newer DB simply
-- never selects it and keeps working (the schemaVersion bump convention -- bump
-- ONLY for a change an old binary would MISREAD, never for a purely additive
-- table). No ALTER TABLE ADD COLUMN is involved either, so the convergent-DEFAULT
-- rule has nothing to converge here: CREATE TABLE IF NOT EXISTS is the whole
-- migration and it runs identically on a fresh and an upgraded database.
--
-- APPEND-ONLY BY CONSTRUCTION -- no code path mutates it; the table itself is
-- not protected against direct DB access (#604). Note it is also the one ledger
-- in this family that is legitimately DELETEd from: it is listed in
-- developerPIITables and EraseDeveloper hard-deletes its rows for a GDPR Art. 17
-- request. That is exactly why #604 ruled out a BEFORE DELETE trigger anywhere --
-- no trigger can tell lawful erasure from tampering.
--
-- It carries NO unique index: an audit ledger is append-only and needs no
-- uniqueness, which also sidesteps the tenant-ordering constraint (a future
-- tenant_id would LEAD any index later added here -- (tenant_id, repair_id) --
-- per the CLAUDE.md tenancy ordering rule, keeping the retrofit mechanical).
CREATE TABLE IF NOT EXISTS repo_repair_audit (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    -- repair_id groups every row written by ONE repair operation (a random hex
    -- token minted per run), so a single run's per-repo rows read back together
    -- and two runs never intermingle. Shared with repo_repair_row_audit.
    repair_id      TEXT    NOT NULL,
    -- developer is the operator's REQUIRED --developer selector: the run only
    -- ever considered token_events rows owned by this developer. Recorded so the
    -- ledger says what the run was ALLOWED to touch, not just what it did touch.
    developer      TEXT    NOT NULL,
    -- from_repo is always the 'unqualified' sentinel today (the only value the
    -- repair is permitted to overwrite). Stored explicitly rather than implied so
    -- the ledger stays readable if the permitted source value ever widens.
    from_repo      TEXT    NOT NULL,
    -- to_repo is the canonical owner/repo slug this group of rows was repaired to
    -- (already normalized through internal/repoid, so it is byte-identical to what
    -- the collector would have written).
    to_repo        TEXT    NOT NULL,
    row_count      INTEGER NOT NULL,
    -- SUM(cost_micro) of the repaired rows, in integer micro-dollars: how much
    -- spend this run moved into to_repo's identity.
    cost_micro_sum INTEGER NOT NULL,
    tool_version   TEXT    NOT NULL,
    ts             DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- repo_repair_row_audit is the per-ROW before-image ledger of every
-- tierd repair-repo (#493), mirroring reprice_row_audit -- see that table's
-- comment for the canonical description of the before-image mechanism and why a
-- row-grain ledger beats a whole-file backup snapshot (it survives continued
-- ingestion; a snapshot diverges the moment the next event lands).
--
-- Where repo_repair_audit records per-target-repo AGGREGATES, this captures, for
-- EACH token_events row the repair changed, that row's EXACT pre-repair repo and
-- the slug it moved to. That is what makes the repair REVERSIBLE-IN-PRINCIPLE at
-- row grain: a future inverse-undo restores each row BY token_event_id to its
-- stored old_repo. (The undo command is NOT built here -- #493 ships the
-- substrate, exactly as #294 did.)
--
-- 🔴 IT DELIBERATELY DOES NOT STORE session_id, AND THAT IS A PRIVACY DECISION,
-- NOT AN OMISSION. Recording the mapping key that resolved each row would be
-- better provenance -- it would make a mis-keyed mapping diagnosable after the
-- fact. But docs/privacy.md classifies token_events.session_id as personal data,
-- and this ledger is designed to OUTLIVE the row it describes (see the no-FK note
-- below). Copying a session id in here would therefore create personal data that
-- survives a GDPR Art. 17 erasure with NO developer column for EraseDeveloper to
-- reach it by -- a silent compliance hole of exactly the kind developerPIITables'
-- comment warns about. While the token_events row still exists its session_id is
-- reachable through token_event_id, which is precisely as long as it should be.
--
-- Every before-image is written in the SAME transaction as the row UPDATEs and
-- the aggregate rows, so a mid-run failure leaves no partial repair and no orphan
-- before-image.
--
-- APPEND-ONLY BY CONSTRUCTION -- no code path mutates it; the table itself is
-- not protected against direct DB access (#604).
--
-- GROWTH: one row per CHANGED token_events row, so a large first-time repair
-- writes a proportional burst and holds the single-writer lock for its span --
-- run it in a maintenance window. Append-only with no pruning here; retention/GC
-- of the audit ledgers is deferred with the broader retention work (#141).
--
-- schemaVersion is deliberately NOT bumped (same reasoning as repo_repair_audit).
-- UNIQUE(repair_id, token_event_id) both enforces exactly one before-image per row
-- per run AND honors the CLAUDE.md tenant-ordering rule -- a future tenant_id would
-- LEAD the key, (tenant_id, repair_id, token_event_id), keeping the eventual
-- retrofit mechanical. No FOREIGN KEY to token_events: an audit before-image must
-- outlive the row it describes.
CREATE TABLE IF NOT EXISTS repo_repair_row_audit (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    repair_id      TEXT    NOT NULL,
    -- token_event_id is the token_events.id whose repo this run rewrote -- the key
    -- a surgical inverse restores by.
    token_event_id INTEGER NOT NULL,
    -- the row's EXACT repo BEFORE this repair overwrote it, and the slug it was
    -- set to.
    old_repo       TEXT    NOT NULL,
    new_repo       TEXT    NOT NULL,
    ts             DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(repair_id, token_event_id)
);

-- cost_correction_audit is the append-only ledger of every sanctioned
-- POST /api/v1/costs cost-correction override (#346, ruling C -- the
-- follow-up to #295's ruling A). #295 made a divergent KEYED re-post 409
-- fail-loud instead of silently landing (cost_micro is immutable, #233); this
-- is the ONE narrow, audited exception -- a caller that explicitly flags
-- override=true (with a required actor + reason) may correct an existing
-- row's cost_micro, and every such correction is recorded here BEFORE it is
-- forgotten. See CorrectManualCostEvent, the sole writer.
--
-- IT DOES NOT MERGE INTO A PLAIN UPSERT. This table -- and the fact that the
-- UPDATE it accompanies touches ONLY cost_micro on ONE already-identified row
-- -- is what keeps the override from becoming the last-writer-wins semantics
-- #233 and the org_actual_spend per-source model both depend on NOT existing
-- (an architecture review flagged exactly this risk on #295). A
-- correction is a targeted, attributed, reversible-in-principle edit to one
-- row; it is never a second insert path.
--
-- One row per override request that ACTUALLY changed a stored cost -- i.e.
-- corrected=true from CorrectManualCostEvent. A bare re-post that lands as a
-- normal insert (no prior row) or that matches the stored value (the existing
-- idempotent no-op) writes nothing here, because nothing was corrected.
--
-- schemaVersion is deliberately NOT bumped (same reasoning as reprice_audit /
-- repo_repair_audit and the schemaVersion bump convention above them): a
-- brand-new table that no pre-#346 read path touches, so an old binary
-- opening this newer DB simply never selects it and keeps working.
--
-- APPEND-ONLY, AND HERE THE SCHEMA ENFORCES HALF OF IT (#604). UPDATE is
-- REFUSED by trg_cost_correction_audit_no_update (see schemaPostMigration):
-- an audit row's content is a historical fact and rewriting it is never
-- legitimate, so the guard holds even against a future code path that tries.
-- DELETE is NOT refused, deliberately and by ruling: erasure and retention are
-- lawful reasons to remove a ledger row and no trigger can tell them from
-- tampering (repo_repair_audit, this table's sibling, is hard-deleted by
-- EraseDeveloper today). This table itself is not in developerPIITables and has
-- no developer column, so EraseDeveloper does not currently reach it.
--
-- 🔴 THIS IS NOT TAMPER-PROOFING, AND MUST NOT BE DESCRIBED AS SUCH. DROP
-- TRIGGER is one line for anyone holding the DB file. What the trigger buys is
-- narrow and real: a future code path cannot silently mutate the money-rewrite
-- ledger. Nothing more.
--
-- It carries NO unique index: an audit ledger is append-only (a row
-- may legitimately be corrected more than once over its life, e.g. a second
-- finance revision), which also sidesteps the tenant-ordering constraint (a
-- future tenant_id would LEAD any index later added here --
-- (tenant_id, token_event_id, ts) -- per the CLAUDE.md tenancy ordering
-- rule, keeping the retrofit mechanical). No FOREIGN KEY to token_events: an
-- audit row must outlive the row it describes (mirrors reprice_row_audit /
-- repo_repair_row_audit).
--
-- 🔴 IT DELIBERATELY DOES NOT STORE idempotency_key, AND THAT IS A PRIVACY
-- DECISION, NOT AN OMISSION -- the SAME shape as repo_repair_row_audit's
-- deliberate exclusion of session_id (see that table's schema comment). An
-- earlier version of this table copied idempotency_key in "so the ledger
-- stays human-readable without a join back to a row that may have been
-- erased" -- that reasoning has it backwards: idempotency_key is frequently
-- CLIENT-CHOSEN and can embed or resemble personal data (an email, a ticket
-- reference), and this ledger is designed to OUTLIVE the row it describes
-- (no FOREIGN KEY, above). Copying it in would create personal data that
-- survives a GDPR Art. 17 erasure with no developer column for
-- EraseDeveloper to reach it by -- exactly the silent compliance hole
-- developerPIITables' own comment warns about. token_event_id is
-- SERVER-GENERATED and carries no client content, so it is safe to keep as
-- the (now sole) key a future inverse would restore by; it simply stops
-- resolving once the row it names has been erased or pruned, which is
-- correct, not a bug. See developerPIITables' exclusion entry for this table
-- and docs/privacy.md.
--
-- actor and reason are likewise excluded from erasure: they name and explain
-- the OPERATOR who made the correction (a third party to the data subject
-- whose spend was corrected, not the data subject), the same class as
-- webhook_payloads' third-party identifiers (see developerPIITables). A
-- caller MUST NOT put the data subject's own name/email in the reason field --
-- nothing enforces that today; it is a documented expectation, not a
-- technical guarantee.
CREATE TABLE IF NOT EXISTS cost_correction_audit (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    -- token_event_id is the token_events.id whose cost_micro this override
    -- rewrote -- the key a surgical inverse would restore by. Server-
    -- generated, not client content -- see the privacy note above for why
    -- this is the only row-identifying column this table carries.
    token_event_id INTEGER NOT NULL,
    -- the row's EXACT cost_micro BEFORE and AFTER this correction.
    old_cost_micro INTEGER NOT NULL,
    new_cost_micro INTEGER NOT NULL,
    -- actor and reason are REQUIRED client-supplied fields (validated non-
    -- empty and length-capped by the API handler before this is ever called),
    -- so ruling C's guarantee is that an override always carries an
    -- attribution and an explanation -- never a silent overwrite. Free text;
    -- no enum, since a finance correction's reason is not a closed set.
    --
    -- 🔴 actor IS A SELF-ASSERTED CLAIM, NOT A VERIFIED IDENTITY. THE LEDGER
    -- RECORDS WHO THE CALLER SAYS THEY ARE. It is unvalidated free text the
    -- caller chooses; nothing checks it against the credential that made the
    -- request, and nothing can today -- /costs is gated by ONE global write
    -- token with no subject (internal/api requireAuth), so there is no
    -- principal for the server to record instead. A caller authenticated with
    -- that token can write actor = mallory and the ledger will say mallory.
    -- This is a design consequence of single-tenancy, not a coding defect;
    -- binding actor to a verified principal needs the identity layer tracked
    -- as #65. Read every row here as a claim, and corroborate it against the
    -- operator log line the handler emits if it matters. Same for reason.
    --
    -- See the privacy note above: these identify the OPERATOR, not the data
    -- subject.
    actor          TEXT    NOT NULL,
    reason         TEXT    NOT NULL,
    ts             DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
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
-- #604: refuse UPDATE on the money-rewrite ledger at the schema level.
--
-- cost_correction_audit records what a sanctioned POST /api/v1/costs override
-- did to a stored cost (old -> new, actor, reason). Its rows are historical
-- facts, so UPDATE is never legitimate -- unlike DELETE, which is (erasure and
-- retention/GC are lawful reasons to remove a ledger row, and no trigger can
-- distinguish them from tampering). That asymmetry is the whole of the #604
-- ruling: BEFORE UPDATE here, and BEFORE DELETE nowhere. A BEFORE DELETE trigger
-- on the sibling repo_repair_audit would break EraseDeveloper (GDPR Art. 17),
-- which hard-deletes from it today; TestEraseDeveloper_StillDeletesAuditRows
-- exists so that cannot be re-litigated by accident.
--
-- 🔴 IT IS NOT TAMPER-PROOFING. DROP TRIGGER is one line for anyone holding the
-- DB file. What it buys: a future code path cannot silently mutate this ledger.
-- Do not describe it as anything stronger.
--
-- RAISE(ABORT), never ROLLBACK or FAIL. ABORT undoes the STATEMENT and hands Go
-- an ordinary constraint error, so a caller inside a transaction unwinds through
-- its normal rollback path with the real cause intact. RAISE(ROLLBACK) would end
-- the transaction underneath the caller, and the deferred Rollback/Commit would
-- then report "no transaction" -- masking the actual error with a bookkeeping
-- one at every one of the store's deferred-Rollback sites. (That holds
-- whether a site is DEFERRED or IMMEDIATE, so it does not depend on #598's
-- BEGIN IMMEDIATE sweep, which is NOT on this branch.)
--
-- Created here in Phase 3 rather than alongside the CREATE TABLE in Phase 1
-- because SQLite drops a table's triggers with the table: Phase 2.5 rebuilds
-- would silently take it with them. Phase 3 runs after every rebuild, which is
-- exactly the guarantee this phase exists to provide. CREATE TRIGGER IF NOT
-- EXISTS is metadata-only -- no table rewrite, no scan, O(1) on a populated DB,
-- and idempotent across every boot.
CREATE TRIGGER IF NOT EXISTS trg_cost_correction_audit_no_update
BEFORE UPDATE ON cost_correction_audit
BEGIN
    SELECT RAISE(ABORT, 'cost_correction_audit is append-only: UPDATE is refused (#604)');
END;
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
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)", path, dsnBusyTimeoutMS)
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

// ErrWriteLockUnavailable marks the ONE failure mode of beginImmediate that means
// "somebody else is writing this database": the promotion could not take the
// write lock.
//
// It exists so a caller can tell that apart from the helper's OTHER failure —
// BeginTx itself failing, which is a cancelled context or a dead handle, not
// contention. Without the sentinel a caller matching on text (or, worse, wrapping
// unconditionally) tells an operator who just pressed Ctrl-C that a `tierd serve`
// is ingesting into their database. A diagnosis that names the wrong cause is
// worse than none: it sends the operator to check the wrong thing.
//
// ⚠️ The split is drawn at BeginTx, not at the cause, so it is not perfect: a
// cancellation landing in the window between BeginTx returning and the promote
// Exec starting is labelled contention. That window is sub-microsecond and the
// case is NOT guarded here on purpose. The obvious guard —
// `if ctx.Err() != nil && errors.Is(err, ctx.Err())` — was written, measured, and
// removed for two reasons: no test can reach the window deterministically (so it
// is a guard whose deletion fails nothing, which this tree treats as dead code),
// and it is not clearly safe. The SQLite busy handler ignores ctx and returns
// only after the full busy_timeout, so a deadline that expires while the handler
// spins is REAL contention — and if the driver's error there wraps the deadline,
// that guard would relabel the one case the sentinel exists to catch. Prefer this
// stated imprecision to an untestable branch that can invert the answer.
var ErrWriteLockUnavailable = errors.New("acquire write lock")

// dsnBusyTimeoutMS is the busy_timeout (milliseconds) carried in every
// connection's DSN — how long SQLite spins waiting for a lock before giving up.
//
// 🔴 IT IS A CONSTANT BECAUSE beginImmediateBounded MUST RESTORE IT. That helper
// lowers busy_timeout on one pooled connection and has to put it back exactly as
// the DSN set it. A literal in the DSN and a second literal in the restore would
// be free to drift, and the drift is SILENT: the restore would quietly leave the
// process running at whatever the stale literal said, for every later caller on
// that connection. One constant makes that class of bug unrepresentable.
const dsnBusyTimeoutMS = 5000

// requestPathBusyTimeout is the busy_timeout beginImmediateBounded installs for
// HTTP-request-path transactions.
//
// WHY A SEPARATE, SHORTER VALUE. beginImmediate's promote is not bounded by ctx
// (see below), so at the DSN's 5000ms a contended request-path promote blocks the
// full five seconds — and with SetMaxOpenConns(1) it does not stall one request,
// it stalls EVERY in-flight request in the process behind the single connection,
// with a client disconnect unable to shorten it.
//
// 250ms is a POLICY choice, not a measurement: an order of magnitude below the
// DSN default, comfortably inside normal HTTP client patience, and still long
// enough to ride out a short maintenance write rather than failing on contact.
// MEASURED at this value: a contended promote returns in ~263ms (the ~13ms over
// is SQLite's busy-handler granularity), versus ~5.05s at the DSN default.
const requestPathBusyTimeout = 250 * time.Millisecond

// beginImmediatePromoteSQL is the no-op write beginImmediate issues to promote a
// DEFERRED transaction to RESERVED. It matches zero rows by construction, so it
// changes nothing and writes no WAL frames.
//
// 🔴 THE COLUMN CHOICE IS LOAD-BEARING, AND `repo` WOULD BE WRONG. #598 converts
// the migration call sites to this helper, so the statement must be valid at
// EVERY point in the migration sequence — including before Phase 2 runs
// addColumnIfMissing(token_events, "repo") (see Open). A promote on `repo` would
// fail "no such column" on a pre-#231 database, turning a lock acquisition into a
// migration abort. `id` is in schemaTables' base CREATE TABLE and no migration
// adds, renames or drops it, so `SET id = id` is valid against every schema
// version this package can open. (It is also never executed: WHERE 0 matches
// nothing, so the rowid is not rewritten.)
//
// It must stay a statement SQLite classifies as a WRITE. `SELECT` would not
// promote, and neither would a `PRAGMA` read — the lock comes from the statement
// being a write, not from the rows it happens to touch.
//
// Guard coverage: TestBeginImmediateTakesTheWriteLock drives this directly —
// swap it for a SELECT and the test fails.
const beginImmediatePromoteSQL = `UPDATE token_events SET id = id WHERE 0`

// beginImmediate opens a transaction holding the SQLite write lock from its
// first statement — the BEGIN IMMEDIATE semantics, obtained honestly.
//
// 🔴 WHY THIS EXISTS AT ALL. modernc.org/sqlite IGNORES sql.TxOptions.Isolation:
// its newTx reads only opts.ReadOnly and the connection-global beginMode, and
// beginMode comes solely from the `_txlock` DSN parameter, which this store's DSN
// does not set. So db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
// emits a plain `BEGIN` — a DEFERRED transaction — and the isolation level is a
// NO-OP. Verified against the pinned driver version, both by reading tx.go and by
// measurement: a second connection took `BEGIN IMMEDIATE` in 17.6µs while a
// LevelSerializable tx was open, i.e. no lock was held.
//
// What that costs a read-then-write transaction: a DEFERRED tx takes its read
// snapshot at its first statement, and if another connection commits before the
// first write, SQLite fails the deferred-to-write upgrade with
// SQLITE_BUSY_SNAPSHOT (517). busy_timeout does NOT retry that code — measured
// returning in ~19µs against this store's 5000ms busy_timeout. The whole
// transaction is thrown away, and the operator sees an opaque "database is
// locked" attributed to a statement far from the real cause.
//
// A no-op write is the portable promotion. A DEFERRED BEGIN takes no snapshot
// until its first statement, so this establishes the write lock AND the read
// snapshot atomically — there is no window between BeginTx and the promote in
// which another writer could slip in.
//
// ⚠️ Do NOT "fix" the ignored isolation by setting `_txlock=immediate` in the DSN:
// it is CONNECTION-GLOBAL, so it would promote every read transaction in the
// serve path to a write lock too. The promotion has to be per-transaction, which
// is what this helper is.
//
// ⚠️ ctx DOES NOT BOUND THE PROMOTE — READ THIS BEFORE CONVERTING A CALL SITE.
// Under contention this call blocks for the FULL busy_timeout carried in the DSN
// (5000ms, see Open) regardless of the caller's deadline. The driver fires
// sqlite3_interrupt on ctx-done, but that cannot break SQLite's busy loop, so a
// deadline changes only the error TEXT, never the duration. MEASURED on
// darwin/arm64 against this store's DSN, with a second connection holding BEGIN
// IMMEDIATE:
//
//	contended, context.Background() -> 5.0497s, "database is locked (5) (SQLITE_BUSY)"
//	contended, 300ms deadline       -> 5.0819s, "context deadline exceeded"
//	UNCONTENDED, 300ms deadline     -> 59µs
//
// TestBeginImmediateIsNotBoundedByContext is the harness for rows 2 and 3, and
// FAILS if this stops being true — so a driver upgrade that made ctx actually
// bound the promote cannot leave this block (or the two caveats below) quietly
// stale. Row 1 was measured the same way and is not separately pinned: it is the
// benign direction, and pinning it would cost a second 5s test to assert that a
// deadline-free call waits.
//
// 🔴 WHICH HELPER A CALL SITE WANTS (#598 converted all nine on this split, and
// it is not cosmetic):
//
//   - The SEVEN Open()-time migration/backfill sites
//     (dropActualSpendNonNegativeCheck, migrateCacheWriteSplit,
//     migrateCostUSDToMicro, migrateActualSpendToMicro, backfillPeriodMembership,
//     backfillPriceVersion, recomputeKnownSourceCosts) use THIS helper. Every one
//     passes context.Background() and runs during startup, before the process
//     serves anything; a 5s wait there is a slow boot, which is the correct
//     behaviour when another process holds the write lock.
//   - The TWO request-path sites — UpsertDeveloperAlias and EraseDeveloper, both
//     called with r.Context() from HTTP handlers — use beginImmediateBounded
//     instead, which caps the wait at requestPathBusyTimeout. With
//     SetMaxOpenConns(1) a 5s uninterruptible block does not stall one request, it
//     stalls EVERY in-flight request in the process behind the single connection,
//     and the client's disconnect cannot shorten it.
//
// THREE MORE SITES HAVE SINCE BEEN CONVERTED ON THE SAME SPLIT, by #346 and #610
// rather than #598, and they are the worked example of the rule rather than
// exceptions to it:
//
//   - CorrectManualCostEvent (the /costs sanctioned override) -> BOUNDED. Reached
//     from an HTTP handler with r.Context(), so it lands on the request-path side
//     exactly as the two above do.
//   - InsertManualCostEvent (the /costs PLAIN branch, keyed and unkeyed) ->
//     BOUNDED (#610). Same route, same r.Context(), same SELECT-then-upsert shape
//     as the override branch — and while it was the one half left DEFERRED, the
//     endpoint answered a lost race for the write lock two different ways
//     depending on the `override` field: a bounded 503 + Retry-After on one, an
//     unbounded 5000ms block then a permanent-looking 500 on the other.
//   - Reprice -> UNBOUNDED, and only when opts.Commit is true. Its sole caller is
//     `tierd reprice`, never a handler, so a 250ms cap would protect no request and
//     would fail an operator's history rewrite over a momentary lock. Its DRY RUN —
//     the default — stays DEFERRED because it writes nothing and must not contend
//     with a live `tierd serve`; RepairRepo splits the same way for the same reason.
//
// So, for a site that takes the write lock: Open()-time or another non-request
// caller -> beginImmediate. Anything reachable from an HTTP handler ->
// beginImmediateBounded.
//
// ⚠️ THAT RULE IS NOT A DESCRIPTION OF THE TREE. "All nine" is exhaustive only
// over the sites that previously passed sql.TxOptions{Isolation:
// sql.LevelSerializable} — the ones whose comments claimed a lock they did not
// take. Plenty of transactions here still open plain DEFERRED and are reachable
// from a handler: InsertTokenEvents (POST /api/v1/events), UpdateQuality and
// UpdateQualityForOutcome (the webhook path, and a read-then-write — the
// SQLITE_BUSY_SNAPSHOT shape), UpsertHierarchy /
// UpsertHierarchies / EndMembership (the org-hierarchy and period-membership
// routes), and both subscription.go sites. (Reprice and InsertManualCostEvent
// were on this list and have since been converted — see above.) They are knowingly
// unconverted, NOT audited-and-cleared: #598 deliberately scoped itself to the
// sites carrying a FALSE claim, because those were actively misleading, and left
// the rest to be judged on their own read-then-write risk. Do not read this rule
// as "the tree already complies".
//
// The returned tx is caller-owned: the caller must Commit or Rollback it. On any
// error nothing is returned and any partially-opened tx has been rolled back, so
// the caller never has to clean up after a failure.
//
// CONCURRENCY: safe for concurrent use; it is a plain database/sql call.
func beginImmediate(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx, beginImmediatePromoteSQL); err != nil {
		// Roll back here rather than leaking the transaction (and, with
		// SetMaxOpenConns(1), the process's only connection) to a caller that
		// has no handle to close.
		_ = tx.Rollback()
		if !isPromoteContention(ctx, err) {
			return nil, fmt.Errorf("promote to write lock: %w", err)
		}
		return nil, fmt.Errorf("%w: %w", ErrWriteLockUnavailable, err)
	}
	return tx, nil
}

// isPromoteContention reports whether a FAILED promote means "another writer
// holds the lock right now" — retryable — rather than "this database cannot be
// written at all" — permanent.
//
// 🔴 WHY THE DISTINCTION IS NOT COSMETIC. Every request-path caller turns
// ErrWriteLockUnavailable into 503 + `Retry-After: 1` + "database is busy …
// retry shortly" (internal/api/handler.go). Wrapping the sentinel around EVERY
// promote failure therefore tells a client to retry forever through conditions
// that will never clear, and tells the operator not to look at the database —
// the exact inversion of the harm the 503 exists to prevent, and on a GDPR
// erasure endpoint and the sanctioned cost-correction endpoint at that.
// MEASURED against modernc.org/sqlite v1.48.0 with this store's DSN, driving the
// real helpers:
//
//	genuine contention (bounded)     -> *sqlite.Error code 5  (SQLITE_BUSY)
//	contention under a ctx deadline  -> NOT a *sqlite.Error; plain ctx error
//	read-only database file          -> *sqlite.Error code 8  (SQLITE_READONLY)
//
// Before this narrowing the third case was reported identically to the first.
// SQLITE_FULL (13) and the SQLITE_IOERR family behave the same way: permanent,
// and previously sold as "retry shortly".
//
// 🔴 ORDER IS LOAD-BEARING HERE TOO, FOR THE SAME REASON IT IS IN THE ALIAS
// HANDLER. The SQLite result code is an EXACT signal from the engine about what
// went wrong; ctx.Err() is a HEURISTIC about the caller's clock that knows nothing
// about the failure. So the code is consulted FIRST, and the clock only when the
// error carries no code at all. Reversed, a read-only or full database that failed
// while the request's context happened to already be expired would be classified
// as contention — reintroducing, one layer down, exactly the defect this function
// exists to remove. Pinned by the expired-context arms of
// TestPromoteFailureThatIsNotContentionIsNotRetryable.
//
// The ctx fallback is REQUIRED, not belt-and-braces: the promote's only reason to
// block is waiting for the write lock, so a caller deadline reached inside it is
// contention by construction — and MEASURED, it arrives as a context error
// carrying no SQLite code at all. A ctx CANCELLED before the promote never reaches
// here (it fails at BeginTx, which is why the cancelled-context control arm in
// TestBeginImmediateTakesTheWriteLock still gets a non-contention error).
//
// Extended result codes are masked to their primary code, so BUSY_SNAPSHOT (517)
// — the read-then-write failure this whole helper exists to prevent — and
// BUSY_TIMEOUT (773) / BUSY_RECOVERY (261) classify with plain BUSY. That mask is
// pinned by TestBusySnapshotClassifiesAsContention, which produces a REAL 517
// rather than asserting the arithmetic.
func isPromoteContention(ctx context.Context, err error) bool {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		return serr.Code()&0xff == sqliteBusy
	}
	// No SQLite code: the only shape that reaches here is a context error from a
	// promote that was blocked waiting for the lock.
	return ctx.Err() != nil
}

// sqliteBusy is the primary SQLite result code for write-lock contention.
// Declared here rather than pulled from modernc.org/sqlite/lib, which is a
// cgo-free translation unit whose import would drag the whole amalgamation into
// this package's dependency graph.
//
// SQLITE_LOCKED (6) is deliberately ABSENT. It signals a table locked by another
// connection *in the same process via shared cache*, which this store never
// enables (the DSN sets only busy_timeout and journal_mode), so nothing in this
// tree can produce it — and an unreachable case is an unfalsifiable one. If a 6
// ever does appear it classifies as non-contention and answers 500, which is the
// conservative direction: a permanent-looking answer for a condition we have never
// observed, rather than "retry shortly" for one that might not clear.
const sqliteBusy = 5

// beginImmediateBounded is beginImmediate for a REQUEST-PATH caller: same write
// lock up front, but the wait for it is capped at busy instead of the DSN's
// dsnBusyTimeoutMS.
//
// 🔴 WHY THIS EXISTS RATHER THAN JUST CALLING beginImmediate. The promote is not
// bounded by ctx — SQLite's busy handler does not consult it, so a deadline
// changes only the error text (see beginImmediate). With SetMaxOpenConns(1) a
// blocked promote holds the process's ONLY connection, so an uncapped 5s wait on
// an HTTP handler stalls every other in-flight request for those 5 seconds and
// the client hanging up cannot shorten it. Capping busy_timeout is the only lever
// that actually shortens the block, because it is the thing the busy handler
// obeys.
//
// It is a per-CONNECTION pragma, which is why this reserves a *sql.Conn: with the
// DSN-level value lowered instead, every read transaction in the serve path would
// inherit the short timeout too.
//
// ⚠️ THE RESTORE IS LOAD-BEARING, NOT HOUSEKEEPING. MEASURED: a lowered
// busy_timeout SURVIVES conn.Close() back into the pool — a connection set to 111
// and returned unrestored still reports 111 on the next two acquisitions. Since
// SetMaxOpenConns(1) means the pool hands the SAME connection to everything that
// follows, skipping the restore would silently drop the whole process to the
// short timeout, including the Open()-time migrations that must wait out a
// competing process. So release() restores unconditionally, and if the restore
// itself fails it POISONS the connection (driver.ErrBadConn via Conn.Raw) so the
// pool discards it and the next caller gets a fresh one built from the DSN.
// Leaving a connection of unknown busy_timeout in a one-connection pool is the
// one outcome this must never produce — see restoreAndReleaseConn, which owns
// both halves and is pinned by
// TestRestoreAndReleaseConnDiscardsAConnectionItCannotRestore.
//
// The returned release func must be called exactly once, and is safe to defer
// immediately: it rolls the tx back (a no-op after a successful Commit), restores
// the pragma, and returns the connection. On error nothing is returned, the
// connection is already released, and the caller has nothing to clean up.
func beginImmediateBounded(ctx context.Context, db *sql.DB, busy time.Duration) (*sql.Tx, func(), error) {
	// Acquiring the connection IS ctx-bounded (unlike the promote), so a caller
	// whose client has already gone away fails here rather than queueing.
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("reserve conn: %w", err)
	}
	// restoreAndRelease is the single cleanup path, used by every failure return
	// below as well as by the caller's release func, so no branch can return the
	// connection with the lowered timeout still on it.
	restoreAndRelease := func() { restoreAndReleaseConn(ctx, conn, busyTimeoutRestoreSQL) }
	if _, err := conn.ExecContext(ctx, busyTimeoutPragma(busy)); err != nil {
		restoreAndRelease()
		return nil, nil, fmt.Errorf("lower busy_timeout: %w", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		restoreAndRelease()
		return nil, nil, fmt.Errorf("begin tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx, beginImmediatePromoteSQL); err != nil {
		_ = tx.Rollback()
		restoreAndRelease()
		// Only a genuine lock outcome is the retryable sentinel — see
		// isPromoteContention. A read-only or full database answering "retry
		// shortly" with a Retry-After is a permanent condition sold as transient.
		if !isPromoteContention(ctx, err) {
			return nil, nil, fmt.Errorf("promote to write lock: %w", err)
		}
		return nil, nil, fmt.Errorf("%w: %w", ErrWriteLockUnavailable, err)
	}
	return tx, func() {
		_ = tx.Rollback() // no-op (ErrTxDone) after a successful Commit
		restoreAndRelease()
	}, nil
}

// busyTimeoutRestoreSQL puts the DSN's busy_timeout back on a connection
// beginImmediateBounded borrowed and lowered.
var busyTimeoutRestoreSQL = fmt.Sprintf("PRAGMA busy_timeout = %d", dsnBusyTimeoutMS)

// busyTimeoutPragma renders the statement that installs busy as a connection's
// busy_timeout, floored at 1ms.
//
// 🔴 THE FLOOR IS THE REASON THIS IS A FUNCTION. busy.Milliseconds() TRUNCATES,
// so any sub-millisecond duration renders as `PRAGMA busy_timeout = 0` — and 0
// does not mean "wait very briefly", it DISABLES the busy handler entirely and
// turns every concurrent write into an instant SQLITE_BUSY. A caller passing
// 500*time.Microsecond would get the exact opposite of what it asked for, and a
// negative duration the same. Today's only caller passes a 250ms constant, so
// this is unreachable in production — which is precisely why it is split out as a
// pure function with a table test (TestBusyTimeoutPragmaFloorsAtOneMillisecond)
// instead of an inline `if` that nothing could falsify.
func busyTimeoutPragma(busy time.Duration) string {
	ms := busy.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	return fmt.Sprintf("PRAGMA busy_timeout = %d", ms)
}

// restoreAndReleaseConn returns conn to the pool with the DSN's busy_timeout back
// on it — or, if it cannot, discards the connection instead of returning one whose
// timeout is unknown.
//
// 🔴 THE POISON FALLBACK IS THE POINT OF THIS FUNCTION, AND IT IS NOT DEFENSIVE
// PADDING. With SetMaxOpenConns(1) the pool hands the SAME connection to
// everything that follows, and a lowered busy_timeout is MEASURED to survive
// conn.Close() back into the pool (the control arm of
// TestBeginImmediateBoundedRestoresBusyTimeout). So a connection released with a
// failed restore does not degrade one request — it silently drops the WHOLE
// PROCESS to the 250ms request-path timeout, including the Open()-time migrations
// that must be able to wait out a competing process. Returning driver.ErrBadConn
// from Conn.Raw is the documented way to mark a *sql.Conn dead; the pool then
// closes it and builds a replacement from the DSN, which carries the right value.
//
// restoreSQL is a parameter for ONE reason: it is the only way to reach the
// failure path from a test. A `PRAGMA busy_timeout` on a healthy connection does
// not fail, so with the statement hardcoded the fallback was unreachable and
// MEASURED unguarded — deleting it left the entire tree green.
// TestRestoreAndReleaseConnDiscardsAConnectionItCannotRestore passes a statement
// that does fail. Production has exactly one caller and it passes
// busyTimeoutRestoreSQL.
//
// ⚠️ AND THAT PARAMETER IS WHY THIS VERIFIES THE OUTCOME RATHER THAN THE ERROR.
// Trusting restoreSQL would reopen exactly the drift class dsnBusyTimeoutMS was
// introduced to close: a statement that SUCCEEDS while restoring nothing takes the
// happy path and returns the process's only connection at the short timeout. Two
// such statements are easy to write — a wrong-but-valid `PRAGMA busy_timeout =
// 250`, or a typo'd `PRAGMA busy_timout = 5000`, because SQLite silently IGNORES
// unknown pragmas rather than erroring. Reading the value back and comparing it to
// dsnBusyTimeoutMS covers all three failure modes (errored, wrong, ignored) for
// one extra round-trip on a path that has just run a write transaction.
//
// ctx is used only for its VALUES (deadline stripped): the restore must run even
// when the request's ctx is already cancelled or past its deadline, which is
// precisely the case where the transaction failed and the connection is going back
// to the pool. A skipped restore there is the leak this exists to prevent.
func restoreAndReleaseConn(ctx context.Context, conn *sql.Conn, restoreSQL string) {
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	restored := false
	if _, restoreErr := conn.ExecContext(restoreCtx, restoreSQL); restoreErr == nil {
		var ms int
		if err := conn.QueryRowContext(restoreCtx, `PRAGMA busy_timeout`).Scan(&ms); err == nil {
			restored = ms == dsnBusyTimeoutMS
		}
	}
	if !restored {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	// Unconditional, and it is the SUCCESS path that needs it. MEASURED: a Raw
	// that returned driver.ErrBadConn has ALREADY released the connection, so this
	// Close is a no-op (ErrConnDone) on the poison path — do not read a passing
	// poison-path test as cover for this line. Drop it and the restored connection
	// is never returned; with SetMaxOpenConns(1) the next caller blocks forever.
	_ = conn.Close()
}

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

	// The write lock is held from the transaction's first statement, so the
	// read-then-rebuild below cannot lose a race to another PROCESS opening the
	// same file (in-process, SetMaxOpenConns(1) already serialises it). Under
	// contention this blocks for the DSN's full busy_timeout, which is correct
	// here: this runs during Open(), before the process serves anything, so the
	// cost of another process holding the lock is a slow boot rather than a
	// failed migration. See beginImmediate — and do NOT "restore" the old
	// sql.TxOptions{Isolation: sql.LevelSerializable} form, which the driver
	// ignores entirely (it yielded a DEFERRED tx and an unretried
	// SQLITE_BUSY_SNAPSHOT).
	tx, err := beginImmediate(context.Background(), db)
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
//  2. Open a transaction via beginImmediate and re-check under it, so two
//     processes calling Open() concurrently cannot both race past the
//     column-existence check: the loser blocks on the write lock instead of
//     proceeding. (⚠️ This step long claimed that property while passing
//     sql.TxOptions{Isolation: sql.LevelSerializable}, which the driver ignores
//     — that tx was DEFERRED, held no lock until the UPDATE at step 4, and the
//     loser got an unretried SQLITE_BUSY_SNAPSHOT (517). SetMaxOpenConns(1) was
//     covering it in-PROCESS only. Do not reintroduce that form.)
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
	// Take the write lock, then check under it — see step (2)/(3) in the doc
	// comment. context.Background() is deliberate: this is Open()-time, so
	// waiting out another process's lock is the correct behaviour.
	tx, err := beginImmediate(context.Background(), db)
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
//  2. Open a transaction via beginImmediate — under the write lock, genuinely —
//     then re-check under it whether the legacy cost_usd column still exists.
//     Two racing Open() calls in the SAME process were already excluded by
//     SetMaxOpenConns(1); the lock is what stops two racing PROCESSES from both
//     reaching step 3. (⚠️ This step previously claimed the lock while passing
//     sql.TxOptions{Isolation: sql.LevelSerializable}, which the driver ignores:
//     the tx was DEFERRED and the loser got an unretried SQLITE_BUSY_SNAPSHOT
//     (517). Do not reintroduce that form.)
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
	// Holds the write lock from the first statement, so this is atomic against
	// another PROCESS as well as against this one (SetMaxOpenConns(1) covers only
	// the latter). Open()-time, so context.Background() and a possible full
	// busy_timeout wait are correct: a slow boot beats a failed migration.
	tx, err := beginImmediate(context.Background(), db)
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
	// Holds the write lock from the first statement, so this is atomic against
	// another PROCESS as well as against this one (SetMaxOpenConns(1) covers only
	// the latter). Open()-time, so context.Background() and a possible full
	// busy_timeout wait are correct: a slow boot beats a failed migration.
	tx, err := beginImmediate(context.Background(), db)
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
	//
	// beginImmediate adds the write lock to that crash-atomicity: the seed reads
	// and the marker insert are now also excluded against a second PROCESS, not
	// just against this one. Open()-time, so context.Background() is correct.
	tx, err := beginImmediate(context.Background(), db)
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
	// Holds the write lock from the first statement, so this is atomic against
	// another PROCESS as well as against this one (SetMaxOpenConns(1) covers only
	// the latter). Open()-time, so context.Background() and a possible full
	// busy_timeout wait are correct: a slow boot beats a failed migration.
	tx, err := beginImmediate(context.Background(), db)
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
	//
	// This method is read-then-write (the SELECT above, the UPDATEs below), which
	// is exactly the shape a DEFERRED tx loses to an unretried
	// SQLITE_BUSY_SNAPSHOT (517) cross-process. beginImmediate takes the write
	// lock before the read, so the loser waits out busy_timeout instead — the
	// correct trade at Open() time, where a slow boot beats a failed recompute.
	tx, err := beginImmediate(context.Background(), db)
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

// ErrCostCorrectionIdentityMismatch reports that a #346 override request's
// idempotency_key resolves to an EXISTING row, but that row's (developer,
// issue_id, model, source, fidelity) does not match what the request claims.
// idempotency_key is globally unique across every producer (the partial
// unique index carries no other column — see insertTokenEventSQL), so a key
// collision across identities is not hypothetical: two different clients (or
// one client with a copy-pasted key) can reuse the same string. Without this
// check, CorrectManualCostEvent would locate a row by key ALONE and rewrite
// its cost regardless of whose row it actually is — turning a key collision
// that used to be a safe 409 (#295) into a correction landing on someone
// else's money. Returned by CorrectManualCostEvent; the caller is expected to
// surface this as an HTTP 409, same status family as ErrCostConflict, and
// MUST NOT echo the stored identity back to the caller (a client that merely
// guessed a key should not learn whose row it belongs to).
//
// 🔴 WHAT THIS IS AND IS NOT. It is a CONSISTENCY check against an ACCIDENTAL
// key collision — a copy-pasted or coincidentally-reused key landing a
// correction on a row the request did not mean. It is NOT an ownership or
// authorization boundary, and must never be described as one:
//
//   - The endpoint's credential is a SINGLE GLOBAL write token with no subject
//     (internal/api requireAuth). Nothing binds the authenticating principal to
//     the `developer` field, and there is no tenant_id column anywhere to put
//     such a binding in — TIER is single-tenant by design today (CLAUDE.md).
//   - The tuple this compares is not secret. The read-scoped
//     GET /api/v1/events export publishes every idempotency_key ALONGSIDE its
//     full identity tuple, so a read token is enough to learn any key's exact
//     (developer, issue_id, model, source, fidelity).
//
// So this check stops a blind collision; it does NOT stop a holder of the write
// token who states the correct tuple deliberately. Closing that requires an
// identity layer that binds principal to developer, which is #65 and is out of
// scope for this endpoint. The "MUST NOT echo the stored identity" rule above
// stands regardless — it is defense in depth against the blind case, not a
// secrecy guarantee the export already breaks.
var ErrCostCorrectionIdentityMismatch = errors.New("idempotency_key belongs to a different developer/issue/model/source/fidelity; refusing to correct a row this request does not identify")

// InsertManualCostEvent is InsertTokenEvent for the untrusted manual-import
// surface (POST /api/v1/costs). It preserves the existing idempotent behavior —
// a re-post whose cost_micro exactly matches the stored value is a no-op that
// MAX-merges token counts — but FAILS LOUD instead of silently first-writer-wins
// dropping a CHANGED cost: when a row already exists under the same
// idempotency_key with a different cost_micro, it writes NOTHING and returns
// ErrCostConflict (#295). cost_micro remains immutable (#233): a 409 rejects, it
// never overwrites. An unkeyed insert cannot collide, so it skips the pre-check.
//
// The pre-check and the insert run in one transaction, and under
// SetMaxOpenConns(1) (see Open) that transaction owns the store's single
// connection for its whole lifetime — so no other in-process writer can slip a
// row in between the SELECT and the INSERT. That serialization is what makes the
// divergence decision atomic in-process; the BEGIN IMMEDIATE below is what makes
// it atomic against another PROCESS. On ErrCostConflict the deferred rollback
// leaves the stored row — cost_micro AND token counts — untouched.
//
// 🔴 BOUNDED BEGIN IMMEDIATE, AND BOTH HALVES OF THAT ARE LOAD-BEARING (#610).
//
// IMMEDIATE, because this is a read-then-write whose read is the guard: SELECT
// the stored cost_micro, decide from it, then INSERT. Under a DEFERRED begin
// (which is what a plain BeginTx gives — modernc.org/sqlite ignores
// opts.Isolation outright, see beginImmediate) the SELECT takes a read snapshot
// and any other CONNECTION committing before the INSERT fails the
// deferred-to-write upgrade with SQLITE_BUSY_SNAPSHOT (517), which busy_timeout
// does NOT retry. The #295 divergence verdict would then have been decided
// against a snapshot the write no longer applies to.
//
// BOUNDED, because internal/api handlePostCosts is the only caller and it passes
// r.Context(). With SetMaxOpenConns(1) an unbounded acquisition blocks for the
// DSN's full 5000ms busy_timeout — uninterruptibly, since ctx does not bound the
// promote — and that does not stall one request, it stalls EVERY in-flight
// request in the process behind the single connection.
//
// ⚠️ THIS SITE CARRIES THE #598 DEFECT CLASS AND IT CLOSES #610's ASYMMETRY, so
// do not convert it back "for symmetry with InsertTokenEvent". (It was described
// here as "the tenth #598 site". Do not restore an ordinal: #598 converted nine,
// but RepairRepo predates it and #346 converted two more, so no count of "sites
// like this one" is stable enough to be worth maintaining. The list that IS
// maintained is the one in beginImmediate's doc block.) Before #610 the
// override=true half of POST /api/v1/costs (CorrectManualCostEvent) answered
// contention with a 250ms-bounded 503 + Retry-After while THIS half — the same
// SELECT-then-upsert shape, the same route, the same r.Context() — blocked the
// full 5000ms and then answered 500. One endpoint cannot tell a caller that
// contention is retryable on one request field and permanent on another.
//
// ⚠️ THE UNKEYED BRANCH IS BOUNDED TOO, and that is why it no longer delegates
// to InsertTokenEvent. It needs no pre-check, but it is reached from the same
// handler on the same connection, so leaving it as a bare ExecContext would just
// move #610's split from keyed-vs-override to unkeyed-vs-everything-else.
//
// ⚠️ INSERTTOKENEVENT ITSELF IS LEFT ALONE, AND ITS EXEMPTION IS NARROWER THAN IT
// LOOKS — do not cite it as "the capture path is background". It has exactly ONE
// production caller, ingester's storeAdapter.Ingest, and that adapter is the sink
// for BOTH the JSONL collector (genuinely background) and the reverse proxy,
// whose writes are SYNCHRONOUS inside modifyResponse (see internal/proxy
// recordWrite's note). So the unbounded 5000ms wait IS reachable from a live
// response path, with the same stall-everyone-behind-the-single-connection
// consequence described above. It stays unbounded because bounding it would drop
// a captured event rather than fail a request the client can retry — a 503 is an
// answer, a discarded token record is silent data loss — and because the two
// callers want opposite things through one method. That is a real gap, not a
// cleared one: it wants its own issue and its own measurement, and #610 does not
// close it. This comment used to say the other callers were "the watcher, the
// bulk ingester"; the bulk ingester is InsertTokenEvents, a different method, and
// the proxy was missing entirely. Do not restore that sentence.
func (d *DB) InsertManualCostEvent(ctx context.Context, e TokenEvent) error {
	// release rolls the tx back (a no-op after a successful Commit) AND restores
	// the borrowed connection's busy_timeout, so it replaces the plain deferred
	// Rollback this site used to carry — it must not be doubled up.
	tx, release, err := beginImmediateBounded(ctx, d.db, requestPathBusyTimeout)
	if err != nil {
		return err
	}
	defer release()

	if e.IdempotencyKey != "" {
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
	}

	if _, err := tx.ExecContext(ctx, insertTokenEventSQL, insertTokenEventArgs(e)...); err != nil {
		return err
	}
	return tx.Commit()
}

// CostCorrection describes what CorrectManualCostEvent did, so the API
// handler can tell an actual correction (200, audited) apart from a request
// that happened to need no correction at all (which InsertManualCostEvent's
// ordinary semantics already cover — 201 on a fresh key, idempotent no-op on
// a matching re-post).
type CostCorrection struct {
	// Corrected is true iff an existing row's cost_micro DIFFERED from e's and
	// was rewritten — the only case that appends a cost_correction_audit row.
	Corrected    bool
	TokenEventID int64
	OldCostMicro int64
	NewCostMicro int64
}

// CorrectManualCostEvent implements POST /api/v1/costs's sanctioned override
// path (#346, ruling C — the follow-up to #295's ruling A, which made a
// divergent KEYED re-post 409 instead of silently landing; see ErrCostConflict
// and cost_correction_audit's schema comment). It is the ONLY path — besides
// `tierd reprice`, an offline batch tool — that may ever rewrite an existing
// token_events row's cost_micro.
//
// e.IdempotencyKey, actor, and reason MUST all be non-empty — checked here
// (not left to the caller) because a store method silently accepting an
// unattributed correction would be exactly the "silent overwrite" #295/#346
// exist to prevent, and this is cheap enough to assert as a hard contract
// rather than trust every future caller to remember.
//
// Behavior, in order:
//  1. No row exists for this key: inserts normally (the same upsert
//     InsertManualCostEvent uses). Nothing to correct — Corrected=false, no
//     audit row.
//  2. A row exists, but its (developer, issue_id, model, source, fidelity)
//     does not match e's: refuses with ErrCostCorrectionIdentityMismatch and
//     mutates NOTHING. idempotency_key is globally unique with no other column
//     in its partial index (see insertTokenEventSQL), so a key collision across
//     identities is real, not hypothetical — locating the row by key ALONE
//     would let a correction land on a row this request does not actually
//     identify. That tuple is every column a /costs request can vary, and no
//     more. This catches an ACCIDENTAL collision and is not an ownership
//     boundary — see ErrCostCorrectionIdentityMismatch for what it does and
//     does not defend against.
//  3. A row exists, identity matches, SAME cost_micro: the ordinary
//     idempotent path (token counts MAX-merge), identical to an un-overridden
//     matching re-post. Not a correction — Corrected=false, no audit row. An
//     override flag on a request that turns out not to diverge is not an
//     error; it is simply a no-op.
//  4. A row exists, identity matches, DIFFERENT cost_micro: atomically
//     UPDATEs ONLY cost_micro on that row — token counts, model, source,
//     fidelity, price_version, and billing_mode are all left exactly as first
//     recorded, so this is a surgical one-column correction, never a
//     last-writer-wins upsert of the whole row (the risk architecture-review
//     flagged during #295's review) — and appends one cost_correction_audit
//     row (old → new, actor, reason) in the SAME transaction, so the update
//     and its audit trail commit or roll back together. Corrected=true.
//
// REPRICE-SAFE. `tierd reprice --from-version N --commit` recomputes
// cost_micro from CURRENT token counts for the rows it examines, and a
// corrected row is not one of them: Reprice excludes source='api' outright
// (see repriceExcludedWhereSQL), the same rule recomputeKnownSourceCosts has
// always applied, because a manual-import cost is the caller's authoritative
// figure and re-deriving it from token counts that may not exist yields $0.00.
// Before that exclusion existed, a reprice sweep silently rewrote a corrected
// $42.00 to $0.00 while cost_correction_audit went on asserting the old→new
// pair — a ledger left affirmatively false with no marker. The exclusion is
// pinned by TestReprice_NeverRepricesManualCostRows and
// TestReprice_SanctionedCorrectionSurvivesReprice.
//
// The guarantee is bounded, and the bound is checkable. FOUR statements in this
// package write token_events.cost_micro: this function, Reprice (excluded
// above), and two one-shot Open() migrations -- migrateCostUSDToMicro and
// recomputeKnownSourceCosts. Neither migration can reach a correction:
// migrateCostUSDToMicro runs only while the pre-#69 cost_usd column still
// exists, and a #346 correction can only be created on a post-#69 schema; and
// recomputeKnownSourceCosts excludes source='api' outright. `tierd repair-repo`
// (#493) rewrites repo, not cost. Re-derive that list with
// `grep 'UPDATE token_events SET cost_micro'` before trusting this sentence --
// it is a claim about a grep, not a law.
//
// EVERYTHING below runs inside ONE transaction that owns the store's single
// connection for its whole lifetime (SetMaxOpenConns(1), see Open) — cases
// 1/3/4 all commit through the SAME tx that ran the identity lookup, so the
// divergence decision is atomic against any other in-process writer with no
// window where the connection is released and reacquired. (An earlier version
// of this function delegated cases 1/3 to InsertManualCostEvent after an
// explicit Rollback, which reopened exactly that window: a concurrent writer
// could land between the Rollback and the delegated call's own BeginTx, and
// InsertManualCostEvent's ErrCostConflict — meant for a different caller —
// would surface here as an unmapped error. Inlining removes both problems.)
func (d *DB) CorrectManualCostEvent(ctx context.Context, e TokenEvent, actor, reason string) (CostCorrection, error) {
	if e.IdempotencyKey == "" {
		return CostCorrection{}, fmt.Errorf("cost correction requires a non-empty idempotency_key to identify which row to correct")
	}
	if actor == "" || reason == "" {
		return CostCorrection{}, fmt.Errorf("cost correction requires both actor and reason (an override must be attributed and explained, never silent)")
	}

	// 🔴 THIS TRANSACTION MUST HOLD THE WRITE LOCK FROM ITS FIRST STATEMENT. It is
	// the read-then-write shape in its purest form — SELECT the row by
	// idempotency_key, decide from what it read, then UPDATE that same row's
	// cost_micro — and it is the ONLY path in this project that rewrites already-
	// captured money rather than appending to it. Under a DEFERRED begin (which is
	// what a plain BeginTx gives, including one passed sql.LevelSerializable:
	// modernc.org/sqlite ignores opts.Isolation outright — see beginImmediate) the
	// SELECT takes a read snapshot, and any other connection committing before the
	// UPDATE fails the deferred-to-write upgrade with SQLITE_BUSY_SNAPSHOT (517),
	// which busy_timeout does NOT retry. The correction is thrown away under an
	// opaque "database is locked" — and the identity check below, which is what
	// stops one developer's key rewriting another's money (#346), would have been
	// decided against a snapshot the write no longer applies to.
	//
	// ⚠️ BOUNDED, NOT PLAIN beginImmediate, BECAUSE THIS IS REQUEST PATH. The sole
	// caller is POST /api/v1/costs with override=true, which passes r.Context()
	// (internal/api/handler.go). With SetMaxOpenConns(1) an unbounded acquisition
	// blocks for the DSN's full 5000ms busy_timeout — uninterruptibly, since ctx
	// does not bound the promote — and that does not stall one request, it stalls
	// EVERY in-flight request in the process behind the single connection. The
	// 250ms cap turns contention into a retryable 503 + Retry-After instead;
	// handlePostCosts maps ErrWriteLockUnavailable to writeStoreContention, and that
	// errors.Is check runs BEFORE any other classification for the reason spelled
	// out at the alias site.
	//
	// ⚠️ THE OTHER BRANCH IS NOW CONVERTED TOO (#610), AND THEY MUST STAY IN STEP.
	// This conversion originally covered only override=true, leaving the
	// non-override InsertManualCostEvent on a plain DEFERRED BeginTx — so one
	// endpoint answered contention two ways: a bounded 503 + Retry-After here, and
	// a full 5000ms block then 500 there. #610 converted that half to the same
	// helper with the same requestPathBusyTimeout. Do not re-split them: both
	// halves of POST /api/v1/costs are request-path read-then-writes on the same
	// connection, and a caller cannot reasonably retry one and not the other.
	//
	// release rolls the tx back (a no-op after a successful Commit) AND restores
	// the borrowed connection's busy_timeout, so it replaces the plain deferred
	// Rollback this site used to carry — it must not be doubled up.
	tx, release, err := beginImmediateBounded(ctx, d.db, requestPathBusyTimeout)
	if err != nil {
		return CostCorrection{}, err
	}
	defer release()

	var (
		tokenEventID                 int64
		stored                       int64
		storedDeveloper, storedIssue string
		storedModel, storedSource    string
		storedFidelity               string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, developer, issue_id, model, source, fidelity, cost_micro FROM token_events WHERE idempotency_key = ?`,
		e.IdempotencyKey,
	).Scan(&tokenEventID, &storedDeveloper, &storedIssue, &storedModel, &storedSource, &storedFidelity, &stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Brand-new key: nothing exists to correct. Insert normally, IN THIS
		// tx — no delegation, no released-and-reacquired connection.
		if _, err := tx.ExecContext(ctx, insertTokenEventSQL, insertTokenEventArgs(e)...); err != nil {
			return CostCorrection{}, err
		}
		return CostCorrection{}, tx.Commit()
	case err != nil:
		return CostCorrection{}, err
	}

	// Identity check (case 2 above): a key match alone is not enough. Compare
	// every column the client's request itself claims — a mismatch on ANY of
	// them means this key does not identify the row the client thinks it does.
	//
	// The tuple is the set of stored columns a /costs request can vary, and it
	// is derived, not chosen: developer/issue_id/model come straight off the
	// request body, and the handler validates BOTH source (only "api" or
	// omitted) and fidelity ("daily"/"estimated"/omitted) as client-asserted
	// enums. fidelity belongs here for the same reason source does — a client
	// that asserts the wrong one has not identified this row.
	//
	// ⚠️ ONE ROW CLASS IS UNCORRECTABLE BY THIS PATH, and it is a real
	// consequence of adding fidelity, not a hypothetical. #82 narrowed /costs to
	// daily|estimated|omitted; fidelity is INSERT-only (absent from the ON
	// CONFLICT DO UPDATE set), so a PRE-#82 source='api' row can still carry a
	// value no request can now express — 'realtime' being the live example.
	// Stating it is a 400, omitting it defaults to "estimated", and both
	// mismatch, so every possible request 409s. For those rows the remedy is the
	// one #295 already documents: re-post under a NEW idempotency_key. Pinned by
	// TestCorrectManualCostEvent_LegacyFidelityIsUncorrectable so the dead end is
	// visible behaviour rather than a surprise in production, and documented in
	// docs/api-compatibility.md.
	//
	// It is deliberately NOT wider. repo, host, billing_mode, session_id, and
	// ts are FORCED by the /costs handler (repo is left unset to the
	// 'unqualified' sentinel, host/billing_mode are stamped at insert, ts is
	// server clock) — a client cannot vary them from this surface, so comparing
	// them would reject on a column the caller was never asked for. Widen this
	// tuple if and only if /costs starts accepting the column.
	//
	// Read ErrCostCorrectionIdentityMismatch before describing this as a
	// security control: it catches an ACCIDENTAL collision, not a deliberate
	// one. Auth here is a single global write token with no subject, and the
	// read-scoped /api/v1/events export publishes every key next to its full
	// identity tuple, so nothing about this tuple is secret.
	if storedDeveloper != e.Developer || storedIssue != e.IssueID ||
		storedModel != e.Model || storedSource != e.Source || storedFidelity != e.Fidelity {
		return CostCorrection{}, ErrCostCorrectionIdentityMismatch
	}

	if stored == e.CostMicro {
		// Already matches, same identity: the ordinary idempotent path
		// (token-count MAX-merge), identical to what an un-overridden
		// matching re-post already does. Not a correction. Same in-tx
		// reasoning as the fresh-key case above.
		if _, err := tx.ExecContext(ctx, insertTokenEventSQL, insertTokenEventArgs(e)...); err != nil {
			return CostCorrection{}, err
		}
		return CostCorrection{}, tx.Commit()
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE token_events SET cost_micro = ? WHERE id = ?`,
		e.CostMicro, tokenEventID,
	); err != nil {
		return CostCorrection{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO cost_correction_audit
		    (token_event_id, old_cost_micro, new_cost_micro, actor, reason)
		 VALUES (?, ?, ?, ?, ?)`,
		tokenEventID, stored, e.CostMicro, actor, reason,
	); err != nil {
		return CostCorrection{}, err
	}
	if err := tx.Commit(); err != nil {
		return CostCorrection{}, err
	}
	return CostCorrection{
		Corrected:    true,
		TokenEventID: tokenEventID,
		OldCostMicro: stored,
		NewCostMicro: e.CostMicro,
	}, nil
}

// CostCorrectionAuditCount returns the total number of #346 sanctioned
// cost-correction override rows ever recorded, across every key — a cheap
// operator-visibility primitive ("has anyone ever used the override, and how
// often") and the seam a future GET surface over cost_correction_audit would
// build on (none exists yet — see that table's schema comment). Unfiltered
// by design: this is a COUNT(*), not a report.
func (d *DB) CostCorrectionAuditCount(ctx context.Context) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cost_correction_audit`).Scan(&n)
	return n, err
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

	RowCount        int64 // rows examined (matched the floor AND are repriceable)
	ChangedRowCount int64 // subset whose cost_micro/price_version/billing_mode actually changed
	// ExcludedRowCount is the count of rows that MATCHED price_version >=
	// FromVersion but were deliberately NOT REPRICED, because their cost is not
	// derived from their token counts and re-deriving it would destroy the
	// figure — caller-authoritative manual imports (source='api') and
	// money-carrying rows with no token counts. See repriceExcludedWhereSQL.
	// They ARE counted (that is this field), so "excluded" means excluded from
	// the recompute, not invisible.
	//
	// It is reported rather than silently dropped so the operator is never told
	// "nothing matched --from-version" about rows that matched and were
	// protected. Present identically on a dry run and a commit.
	ExcludedRowCount int64
	OldCostMicroSum  int64 // SUM(cost_micro) over the CHANGED rows, before
	NewCostMicroSum  int64 // SUM(cost_micro) over the CHANGED rows, after (net delta = New - Old)
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

// repriceExcludedWhereSQL is the predicate for token_events rows a reprice must
// NEVER re-derive, because their stored cost_micro is NOT a function of their
// token counts. Repricing such a row does not correct it — it recomputes it to
// $0.00 and destroys the figure.
//
// It serves the SAME INTENT as recomputeKnownSourceCosts -- an /api/v1/costs
// row's caller-authoritative cost is never silently overwritten -- but it is
// deliberately the OPPOSITE SHAPE, and that difference matters:
// recomputeKnownSourceCosts is a closed ALLOWLIST (source IN ('jsonl','proxy',
// 'copilot-api','anthropic-admin')), which fails SAFE -- a new source is simply
// not touched. This is an open DENYLIST, which fails DESTRUCTIVE -- a future
// source whose cost is caller-authoritative gets repriced unless someone adds
// it here. A denylist is nonetheless right for reprice, whose whole purpose is
// that the default must stay "reprice it"; an allowlist would silently stop
// repricing every new collector. TestRepriceClassifiesEverySource is the
// compensating control: it iterates collector.AllSources() and fails when the
// enum grows, so a new source cannot land unclassified. (The two lists already
// disagree -- codex-rollout and openai-usage postdate the allowlist and were
// never added to it -- which is exactly why "the same rule" would have been the
// wrong thing to write.) Reprice simply never got the treatment — a defect that pre-dates #346 but became money-
// destroying once /costs became the sanctioned durable correction surface: a
// $42.00 finance correction reprices to $0.00 while cost_correction_audit goes
// on asserting the old→new pair as though it still held, leaving the ledger
// affirmatively false with no marker.
//
// Two disjunctions, each measured, each independently load-bearing:
//
//   - source = 'api' — the manual-import surface (POST /api/v1/costs). Its
//     cost_usd is the CALLER's authoritative figure (a finance team posting a
//     reconciled invoice number); token counts are optional there and routinely
//     absent. This is the same exclusion recomputeKnownSourceCosts makes, and
//     the ONLY source in the tree whose cost is not server-derived: every other
//     producer prices from raw token counts with the server's own table
//     ('jsonl'/'proxy'/'codex-rollout' collectors, the 'anthropic-admin' /
//     'openai-usage' org pollers via ComputeCostHost, and POST /api/v1/events,
//     which re-prices what a shipper posts, #233). So this filter excludes
//     exactly the rows that cannot be repriced and nothing that should be.
//
//   - a MONEY-CARRYING row with no tokens at all — cost_micro <> 0 while every
//     token column is 0. Whatever produced that cost, it was not the token
//     counts, so re-deriving it can only zero it. This is not redundant with the
//     source filter: store.InsertTokenEvent is exported and takes CostMicro
//     verbatim, so such a row is reachable under ANY source, and the measured
//     behavior was the same $42 → $0.
//
// The cost_micro <> 0 qualifier is deliberate and was ALSO settled by
// measurement: an unqualified zero-token guard would over-exclude. A zero-token
// row whose cost is legitimately 0 still needs its price_version and
// billing_mode reconciled (measured: such a row correctly moves v1 → the active
// version), and skipping it would be a real behavioral regression. Repricing it
// is a no-op on the cost by construction, so it is safe to keep.
//
// Shared verbatim by the candidate SELECT (negated) and the excluded-row COUNT
// (asserted), so the set reprice skips and the set it reports as skipped cannot
// drift apart. TestReprice_ExaminedAndExcludedPartitionTheFloor pins that as a
// behavioral invariant: examined + excluded == every row at or above the floor.
const repriceExcludedWhereSQL = `(
       source = 'api'
    OR (cost_micro <> 0 AND input_tok = 0 AND output_tok = 0
        AND cache_read = 0 AND cache_write_5m = 0 AND cache_write_1h = 0)
  )`

// repriceCandidate columns pulled per row for the recompute. Kept minimal — only
// what ComputeCostHost needs plus the identity/provenance fields — because the
// candidate set can be large (a window over the whole priced history).
const repriceSelectSQL = `
SELECT id, developer, host, model, input_tok, output_tok, cache_read,
       cache_write_5m, cache_write_1h, cost_micro, price_version, billing_mode
FROM token_events
WHERE price_version >= ? AND NOT ` + repriceExcludedWhereSQL + `
ORDER BY id`

// repriceExcludedCountSQL counts the rows that MATCHED the operator's
// --from-version floor but are excluded from the recompute by
// repriceExcludedWhereSQL. Without it the report would lie: a floor whose only
// matches are manual-import rows would print "no rows have price_version >= vN
// — nothing examined (check --from-version)", telling the operator their flag
// was wrong when in fact the rows exist and were deliberately protected.
const repriceExcludedCountSQL = `
SELECT COUNT(*) FROM token_events
WHERE price_version >= ? AND ` + repriceExcludedWhereSQL

// Reprice recomputes token_events.cost_micro for every REPRICEABLE row priced at
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
//
// NOT EVERY ROW AT OR ABOVE THE FLOOR IS A CANDIDATE. A row whose cost_micro is
// not a function of its token counts is never re-derived — re-deriving it does
// not correct it, it zeroes it. repriceExcludedWhereSQL defines that set
// (caller-authoritative source='api' manual imports, and money-carrying rows
// with no token counts); ExcludedRowCount reports how many the floor matched, so
// a protected row is visibly protected rather than silently missing. This is why
// a sanctioned #346 cost correction now SURVIVES a reprice sweep.
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
	//
	// 🔴 A COMMIT MUST TAKE THE WRITE LOCK BEFORE IT READS — the same rule, and the
	// same shape, as RepairRepo (internal/store/repairrepo.go); see beginImmediate
	// for why a plain BeginTx does not, including one passed sql.LevelSerializable,
	// which modernc.org/sqlite ignores outright. This function is the widest
	// read-then-write in the package: it scans EVERY token_events row at or above
	// the floor, buffers the changed ones, and only then issues its first UPDATE.
	// Under a DEFERRED begin that whole scan runs on a read snapshot, and any other
	// connection committing before the first write fails the deferred-to-write
	// upgrade with SQLITE_BUSY_SNAPSHOT (517) — which busy_timeout does NOT retry.
	// The entire scan is discarded, after all the work, under an opaque "database
	// is locked". Fail at acquisition instead, where a failure costs nothing.
	//
	// ⚠️ UNBOUNDED beginImmediate, NOT beginImmediateBounded, AND THAT IS THE
	// CORRECT SIDE. Reprice has exactly one caller, `tierd reprice`
	// (cmd/tierd/reprice.go) — it is never reachable from an HTTP handler, so the
	// 250ms requestPathBusyTimeout would not be protecting a request. It would just
	// convert a working operator command into a spurious failure whenever a live
	// tierd happened to hold the lock for a quarter second. An operator rewriting
	// priced history should wait out the DSN's busy_timeout, which is what an
	// unbounded acquisition does.
	//
	// A DRY RUN deliberately does NOT promote. Commit is false by DEFAULT, so the
	// dry run is the common invocation; it writes nothing, and promoting it would
	// hold the exclusive write lock across a full-table scan and stall every writer
	// in a live `tierd serve`. A diagnostic an operator cannot run without taking
	// the database down is a diagnostic nobody runs.
	var tx *sql.Tx
	var err error
	if opts.Commit {
		tx, err = beginImmediate(ctx, d.db)
		// The contention hint is attached ONLY to the contention error.
		// beginImmediate also fails when BeginTx itself fails — a cancelled context
		// (this command installs a SIGINT handler), a closed handle — and telling an
		// operator who just pressed Ctrl-C that a `tierd serve` is ingesting sends
		// them to check the wrong thing. A diagnosis naming the wrong cause is worse
		// than none.
		switch {
		case errors.Is(err, ErrWriteLockUnavailable):
			return RepriceResult{}, fmt.Errorf("reprice: %w (is a tierd serve ingesting into this database? run the reprice against a quiesced database)", err)
		case err != nil:
			// NO STAGE LABEL HERE, and that matches RepairRepo deliberately.
			// beginImmediate has two non-contention failures and labels them
			// itself — "begin tx: …" when BeginTx fails, "promote to write lock:
			// …" when the promote fails permanently (read-only file, disk full).
			// Stamping "begin tx:" over both would render the second as
			// "reprice: begin tx: promote to write lock: attempt to write a
			// readonly database", naming a stage that did not fail. The else
			// branch below DOES label, because there BeginTx is the only thing
			// that can fail.
			return RepriceResult{}, fmt.Errorf("reprice: %w", err)
		}
	} else {
		tx, err = d.db.BeginTx(ctx, nil)
		if err != nil {
			return RepriceResult{}, fmt.Errorf("reprice: begin tx: %w", err)
		}
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

	// Counted in the SAME transaction (and therefore the same snapshot) as the
	// candidate scan below, so "examined + excluded" is a consistent partition of
	// the rows at or above the floor, never two reads of a moving table.
	if err := tx.QueryRowContext(ctx, repriceExcludedCountSQL, opts.FromVersion).
		Scan(&res.ExcludedRowCount); err != nil {
		return RepriceResult{}, fmt.Errorf("reprice: count excluded rows: %w", err)
	}

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
	//
	// The model names go through logsafe.Join, not strings.Join (#321 review,
	// 2026-08-04). They are the models ABSENT from the price table — i.e. by
	// construction the ones nothing in this repo has ever validated — and they
	// arrive from POST /api/v1/events, which applies no charset check. This error
	// is Fprintf'd straight to the reprice CLI's stderr, so an unsanitized name
	// carrying CR/LF forges a standalone line in an operator's maintenance log.
	// Same barrier, same reasoning, as repairrepo.go's errors two files away.
	if res.GuessedRowCount > 0 && !opts.AllowGuessed {
		return RepriceResult{}, fmt.Errorf("reprice: refusing to commit: %d of %d changed row(s) would be repriced to a self-hosted GUESS estimate (model(s) not in the active price table: %s) — audited historical cost would be rewritten to an estimate. Add the missing model(s) to the price table for an accurate reprice, or re-run with --allow-guessed to proceed anyway", res.GuessedRowCount, res.ChangedRowCount, logsafe.Join(res.GuessedModels, maxModelsNamedInError))
	}

	// A commit with nothing to change writes no rows and no audit — there is no
	// mutation to record. Committed reflects that the --commit request was honored;
	// RepriceID stays empty because no audit row was written (it is non-empty IFF
	// the ledger gained rows). The deferred Rollback discards the empty read tx.
	if len(changes) == 0 {
		res.Committed = true
		return res, nil
	}

	repriceID, err := newAuditID()
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

// newAuditID mints the random hex token that correlates every audit-ledger row of
// one maintenance operation — reprice_audit/reprice_row_audit (#294) and
// repo_repair_audit/repo_repair_row_audit (#493) alike. crypto/rand makes it
// collision-free across runs without a sequence column; a rand failure is
// surfaced (the operation aborts) rather than silently writing an unkeyed audit,
// which would leave a mutation no operator could correlate back to a run.
func newAuditID() (string, error) {
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
	return d.DeveloperCostsWindow(ctx, since, time.Time{}, FleetWide)
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

// RepoScope narrows a windowed read to ONE repository (#590). The zero value,
// FleetWide, adds no predicate and reads every repository — the pre-#590 behavior,
// byte-for-byte.
//
// 🔴 SCOPING IS STRICT, AND IT DELIBERATELY DIVERGES FROM RepoMatch. The tolerant
// join rule treats 'unqualified' as matching anything, because for JOINING, strict
// would silently de-attribute the cost of every repo-blind producer (the reverse
// proxy structurally cannot know a repository). That reasoning does NOT transfer to
// FILTERING, where it inverts into the exact defect #590 exists to close: a tolerant
// `repo = ? OR repo = 'unqualified'` would attribute EVERY repo-blind row in the
// fleet to whichever single repository the caller happened to name, and hand back a
// confidently over-counted figure that looks scoped. A scoped read must never be
// able to over-attribute.
//
// The cost of strictness is that genuinely-in-scope spend recorded blind is dropped.
// That is real, so it is not left silent: UnqualifiedExclusion reports exactly what a
// scope excluded, and the /scores data_quality block surfaces it. Strict + disclosed
// under-counts VISIBLY; tolerant over-counts INVISIBLY. the maintainer ruled this shape
// (option C) on 2026-08-03.
//
// The value is a canonical slug — callers validate at the trust boundary with
// repoid.Canonical, which also refuses the reserved 'unqualified' sentinel, so a
// caller cannot ask to be scoped to repo-blindness itself.
type RepoScope string

// FleetWide is the unscoped RepoScope: every repository, no predicate.
const FleetWide RepoScope = ""

// IsFleetWide reports whether this scope adds no repository predicate.
func (s RepoScope) IsFleetWide() bool { return s == FleetWide }

// String returns the scoped slug, or "" when fleet-wide.
func (s RepoScope) String() string { return string(s) }

// clause returns the trailing repository predicate and its bound arg for a scoped
// read, or ("", nil) when fleet-wide. It is written to be APPENDED to an existing
// WHERE — it leads with " AND " and its arg must therefore be appended to the arg
// slice in the same text order, after every bind that appears earlier in the query.
func (s RepoScope) clause() (clause string, args []any) {
	if s.IsFleetWide() {
		return "", nil
	}
	return " AND repo = ?", []any{string(s)}
}

// UnqualifiedExclusion is what a strict repo scope DROPPED from a window because the
// rows carry the 'unqualified' sentinel rather than a real repository (#590). It is
// the disclosure half of ruling C: strict scoping cannot over-attribute, but it can
// under-count, and an under-count nobody can see is exactly the kind of confidently
// wrong number this project refuses to publish.
//
// These counts are NOT a claim that the excluded rows belong to the scoped repository.
// They are unattributable by construction — that is what the sentinel means. The
// honest reading is "this much of the window could not be placed, so a scoped figure
// drawn from it is a lower bound, not a total".
type UnqualifiedExclusion struct {
	// TokenEvents is the number of token_events rows in the window carrying the
	// sentinel, and CostMicro their summed cost in integer micro-dollars (#69).
	TokenEvents    int64
	CostMicro      int64
	OutcomeRecords int64 // outcomes rows in the window carrying the sentinel
}

// Any reports whether the scope excluded anything at all. A false here is the clean
// case: every row in the window named a real repository, so the scoped figure is a
// true total rather than a lower bound.
func (u UnqualifiedExclusion) Any() bool {
	return u.TokenEvents > 0 || u.CostMicro != 0 || u.OutcomeRecords > 0
}

// UnqualifiedExclusionWindow measures what a strict repo scope excluded from the
// half-open [since, until) window (#590) — the repo-blind rows that a fleet-wide read
// would have counted and a scoped read drops.
//
// It deliberately takes the WINDOW ONLY and no scope: the sentinel rows are the same
// set whichever repository the caller scoped to, because 'unqualified' means the
// producer could not determine ANY repository. Passing a scope would imply these rows
// were weighed against it and found not to match, which is not what happened.
//
// 🔴 THE LOWER BOUND IS DELIBERATELY WIDENED BY AttributableWindow, and the reason is
// a real defect this nearly shipped with. The token-side count reaches back
// `since - AttributableWindow`, NOT `since`.
//
// Why: strict scoping also applies inside OutcomeTokenTotals, whose per-outcome
// attributable window is [merge-14d, merge] and therefore intentionally reaches
// BEFORE `since` ("tokens spent last month can fund this month's merge"). So a
// repo-blind token row sitting in that look-back — outside the reporting window — can
// be excluded by a scope, flip a zero-token tripwire against a NAMED developer, and be
// entirely invisible to a disclosure that only measured [since, until). Measured
// before this fix: a scoped read produced a new zero_token_outcomes entry while
// repo_scope_excluded was ABSENT, i.e. reporting "clean".
//
// That is precisely the hole ruling C exists to close — an exclusion nobody can see —
// so the disclosure is widened to cover every row the scope could have suppressed.
// The cost is slight OVER-reporting: sentinel rows in the look-back are counted even
// when no outcome's window actually reached them. That direction is the safe one. A
// disclosure that overstates makes a reader check; one that understates makes them
// trust a number they should not.
//
// The OUTCOME count keeps the plain [since, until) bound — outcomes are the reporting
// population itself, not a look-back input, so widening it would inflate the figure
// with records that were never in scope to begin with.
//
// Two statements rather than a UNION: the tables carry different grains (cost-bearing
// events vs. outcome records) and fusing them would invite summing a row count against
// a dollar figure.
func (d *DB) UnqualifiedExclusionWindow(ctx context.Context, since, until time.Time) (UnqualifiedExclusion, error) {
	tokenWhere, tokenArgs := tsWindow(since.Add(-AttributableWindow), until)
	where, args := tsWindow(since, until)
	var u UnqualifiedExclusion
	// COALESCE on the SUM: an empty match set yields NULL, which will not scan into
	// int64. COUNT is already 0-valued.
	err := d.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(cost_micro), 0)
		FROM token_events
		WHERE `+tokenWhere+` AND repo = ?`,
		append(append([]any{}, tokenArgs...), repoid.Unqualified)...,
	).Scan(&u.TokenEvents, &u.CostMicro)
	if err != nil {
		return UnqualifiedExclusion{}, fmt.Errorf("token_events exclusion: %w", err)
	}
	err = d.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM outcomes
		WHERE `+where+` AND repo = ?`,
		append(append([]any{}, args...), repoid.Unqualified)...,
	).Scan(&u.OutcomeRecords)
	if err != nil {
		return UnqualifiedExclusion{}, fmt.Errorf("outcomes exclusion: %w", err)
	}
	return u, nil
}

// DeveloperCostsWindow is the half-open [since, until) form of DeveloperCosts
// (#276): the scores-path denominator, bounded above so a caller can score a
// closed period ("Team TIER for March") or the BEFORE leg of a before/after
// comparison. A zero `until` is open-ended and behaves exactly like the legacy
// DeveloperCosts.
func (d *DB) DeveloperCostsWindow(ctx context.Context, since, until time.Time, scope RepoScope) ([]DeveloperCost, error) {
	where, args := tsWindow(since, until)
	scopeSQL, scopeArgs := scope.clause()
	args = append(args, scopeArgs...)
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			developer,
			SUM(cost_micro)                                         AS total_cost,
			SUM(CASE WHEN fidelity = 'realtime' THEN cost_micro ELSE 0 END) AS realtime_cost
		FROM token_events
		WHERE `+where+scopeSQL+`
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
//
// Issues with token spend but NO outcome appear here and map to no type segment,
// because work_type is a property of the outcome — a spend-only issue has no category
// to be filed under. That exclusion is structural, but it must never be silent: left
// unreported it makes every per-type TIER better than the pooled score (#466), and
// hides the thrash it is evidence of precisely where a reader would go looking for it.
// The API reconciles the gap explicitly via
// segment_reconciliation.no_outcome_cost_usd. Note this is NOT the same thing as
// unattributed spend (IsUnattributed): that is cost which could not be tied to any
// issue at all, whereas this is cost tied to a real issue that produced no outcome in
// the window.
func (d *DB) DeveloperIssueCosts(ctx context.Context, since time.Time) ([]DevIssueCost, error) {
	return d.DeveloperIssueCostsWindow(ctx, since, time.Time{}, FleetWide)
}

// DeveloperIssueCostsWindow is the half-open [since, until) form of
// DeveloperIssueCosts (#276): the per-(developer, issue) cost the work-type
// segmentation denominates on, bounded above by the same window as
// DeveloperCostsWindow. A zero `until` is open-ended.
func (d *DB) DeveloperIssueCostsWindow(ctx context.Context, since, until time.Time, scope RepoScope) ([]DevIssueCost, error) {
	where, args := tsWindow(since, until)
	scopeSQL, scopeArgs := scope.clause()
	args = append(args, scopeArgs...)
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			developer,
			repo,
			issue_id,
			SUM(cost_micro)                                                 AS total_cost,
			SUM(CASE WHEN fidelity = 'realtime' THEN cost_micro ELSE 0 END) AS realtime_cost
		FROM token_events
		WHERE `+where+scopeSQL+`
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

// UnattributedBuckets enumerates the LABELED sub-buckets, so a consumer can iterate
// the family instead of hardcoding a copy of it (#466).
//
// 🔴 THIS EXISTS BECAUSE AN ALLOWLIST HARDCODED THE FAMILY. api.validateShippedIssueID
// permits exactly the collector's canonical spellings on POST /api/v1/events; adding a
// FIFTH bucket to internal/collector without touching that switch compiles cleanly, and
// the collector then ships an id the endpoint 400s. That is not a degraded number: the
// endpoint validates all-or-nothing, the shipper treats 4xx as terminal with no retry,
// and it is stateless — so the next run rebuilds the identical batch and fails
// identically, forever, losing 100% of that developer's capture. Everything that needs
// the family iterates this slice, and TestUnattributedIssueIDMatchesStore compares it
// element-wise with collector's mirror, so a one-sided addition fails the build.
var UnattributedBuckets = []string{
	UnattributedMainBucket,
	UnattributedDetachedHEADBucket,
	UnattributedNoIssueBucket,
}

// UnattributedFamily is the bare sentinel followed by every labeled sub-bucket — the
// complete set of ids a legitimate producer may assign. Returned as a fresh slice so a
// caller cannot mutate the package's own state.
func UnattributedFamily() []string {
	out := make([]string, 0, len(UnattributedBuckets)+1)
	out = append(out, UnattributedIssueID)
	return append(out, UnattributedBuckets...)
}

// unattributedGlobPattern matches the LABELED unattributed sub-buckets
// (collector.UnattributedMain/DetachedHEAD/NoIssue) — every issue_id of the form
// "unattributed:<reason>". The attributed/unattributed split (#234) and the bucket
// breakdown MUST treat the whole family as unattributed: a `= UnattributedIssueID`
// exact match would count the labeled buckets as ATTRIBUTED and silently inflate
// coverage. Kept adjacent to the sentinel so the two never drift.
//
// GLOB, NOT LIKE — this is load-bearing (#466). SQLite's LIKE is ASCII
// case-INSENSITIVE by default, so `LIKE 'unattributed:%'` also matches
// "UNATTRIBUTED:MAIN" and "Unattributed:Main". GLOB is case-SENSITIVE and so agrees
// exactly with the Go matcher IsUnattributed below. When the two disagreed, a forged
// mixed-case sentinel was counted as unattributed by the SQL-side split (#234) and
// simultaneously as spend-on-a-real-issue by the Go-side reconciliation (#466) — the
// same dollar reported as two different, non-additive things in one response, with
// the fabricated figure attached to a named developer. Case-blind sentinel matching
// was never intended; the fix is to make SQL exact rather than to loosen Go. GLOB
// metacharacters are '*', '?' and '['— none appears in this pattern, and ':' is a
// literal.
const unattributedGlobPattern = UnattributedIssueID + ":*"

// unattributedFamilyPredicate is THE SQL family match, in one place. Every read that
// splits attributed from unattributed spend embeds this exact string, and it binds two
// args in order: UnattributedIssueID, then unattributedGlobPattern.
//
// It is a shared constant rather than two copies so a guard test can build its SQL
// from the SAME text the queries use (TestUnattributedMatcherAgreesWithSQL), instead
// of restating the operator and merely claiming to track it. Restating is how the
// LIKE/GLOB drift survived its first guard: the constants matched, so a test that
// shared only the constants stayed green while the queries diverged.
const unattributedFamilyPredicate = `issue_id = ? OR issue_id GLOB ?`

// IsUnattributed reports whether an issue id is the base unattributed sentinel or any
// of its labeled sub-buckets — the in-Go mirror of the SQL family predicate
// (`issue_id = ? OR issue_id GLOB ?`), for read paths that classify already-fetched
// rows rather than filtering in SQL (the #466 segment reconciliation). Callers MUST
// use this rather than an exact `== UnattributedIssueID` compare, or the labeled
// buckets are mis-counted as attributed.
//
// This must stay semantically identical to that SQL predicate, not merely share its
// constants. Constant equality is NOT sufficient: LIKE-vs-GLOB drift changed the
// MATCHER while every constant still matched (#466). TestUnattributedMatcherAgreesWithSQL
// pins the two by running a table of edge-case ids through the live SQL predicate and
// this function, failing on any disagreement. It is also byte-identical to
// collector.IsUnattributed, which cannot share code because collector imports store.
func IsUnattributed(issueID string) bool {
	return issueID == UnattributedIssueID ||
		strings.HasPrefix(issueID, UnattributedIssueID+":")
}

// ResemblesUnattributed is the WRITE-side counterpart of IsUnattributed, and the one
// predicate every ingest guard in the tree calls (#619). It reports whether a
// client-supplied identifier merely LOOKS like the server-assigned sentinel family:
// case-INSENSITIVELY, after trimming surrounding whitespace.
//
// 🔴 THE CONTAINMENT INVARIANT, which is what makes every caller sound: this must
// reject a STRICT SUPERSET of what IsUnattributed matches. Rejecting more at ingest
// than you match at read is the safe direction — the reverse is a hole, because a
// value the read paths count as unattributed could then be written by a client.
// api.TestWriteGuardStrictlyContainsTheReadMatcher pins it.
//
// IT LIVES HERE, BESIDE IsUnattributed, DELIBERATELY. It was three near-copies —
// internal/api's, and internal/proxy's, each restating the rule on the theory that
// "proxy cannot import internal/api". That was a false dichotomy: the shared rule
// belongs to neither consumer, it belongs next to the sentinel it is about. store
// imports only logsafe and repoid and is ALREADY in both packages' dependency graphs,
// so there is no cycle. Copies of a security predicate drift, and the #466 postmortem
// is precisely a story about a matcher drifting from its twin while every constant
// still matched.
//
// The byte-slice is safe for a structural reason: fold-matching the 13-byte ASCII
// prefix "unattributed:" requires 13 runes in 13 bytes, which forces all 13 to be
// ASCII, so no multi-byte rune can ever fold INTO the prefix. Measured exhaustively —
// no rune of "unattributed:" has a non-ASCII simple-fold partner in either direction.
func ResemblesUnattributed(s string) bool {
	t := strings.TrimSpace(s)
	if strings.EqualFold(t, UnattributedIssueID) {
		return true
	}
	prefix := UnattributedIssueID + ":"
	return len(t) >= len(prefix) && strings.EqualFold(t[:len(prefix)], prefix)
}

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

// costCompositionStmt builds the ENTIRE cost-composition read — predicates,
// statement text, and binds — for the [since, until) window under scope.
//
// It exists as one named function, rather than as inline construction in
// CostCompositionWindow, so that a test can EXPLAIN QUERY PLAN exactly what
// production executes. The seam has to sit HERE, above predicate construction,
// not merely around the SQL literal: a plan test that rebuilds the `where` clause
// itself is still pinning a copy. Measured, not assumed — with the seam one level
// lower (a costCompositionSQL(where, scopeSQL) taking an already-built predicate),
// injecting `+ts >= ?` into the real predicate degraded the production plan from
// SEARCH to SCAN and the plan test still PASSED, because the test built its own
// `where`. SQLite's unary plus preserves the value exactly and strips index
// affinity, so no behavioural test sees it either.
//
// Anything that shapes the read must therefore be inside this function.
func costCompositionStmt(since, until time.Time, scope RepoScope) (string, []any) {
	where, args := tsWindow(since, until)
	scopeSQL, scopeArgs := scope.clause()
	// The unattributed-family binds come first in text order, so their args lead
	// the tsWindow bounds. Bound (not interpolated) even though they are trusted
	// consts — keeps the one query-shaping convention uniform. The family match
	// (exact base sentinel OR any "unattributed:<reason>" bucket) keeps the
	// attributed/unattributed split correct after the labeled-bucket split — an
	// exact `= ?` would count the buckets as attributed. GLOB (not LIKE) so the match
	// is case-SENSITIVE and agrees with Go's IsUnattributed — see
	// unattributedGlobPattern (#466).
	args = append([]any{UnattributedIssueID, unattributedGlobPattern}, args...)
	// The scope predicate is the LAST term in text order, so its bind trails every
	// other arg — including the unattributed-family binds prepended just above.
	args = append(args, scopeArgs...)
	return `
		SELECT
			host,
			model,
			SUM(cost_micro)                                                        AS cost,
			SUM(CASE WHEN ` + unattributedFamilyPredicate + ` THEN cost_micro ELSE 0 END) AS unattributed_cost,
			SUM(input_tok), SUM(output_tok), SUM(cache_read),
			SUM(cache_write_5m), SUM(cache_write_1h)
		FROM token_events
		WHERE ` + where + scopeSQL + `
		GROUP BY host, model`, args
}

// CostCompositionWindow aggregates token_events over the half-open [since, until)
// window (#234) into the cost-composition sidecar: cost by normalized model, the
// per-class token composition, and attributed vs unattributed spend, with the
// cache-read and premium-model levers derived. A zero `until` is open-ended
// (mirrors DeveloperCostsWindow). One GROUP BY over (host, model), then
// BuildCostComposition does the pure folding and share math in Go (premium
// classification is host-aware and lives in the price table, not SQL).
//
// Query plan, measured (#333, 2026-08-04) — NOT the same index DeveloperCostsWindow
// uses, which an earlier revision of this comment claimed:
//
//	SEARCH token_events USING INDEX idx_token_events_ts_id (ts>?)
//	USE TEMP B-TREE FOR GROUP BY
//
// i.e. a ts-window seek plus a per-row heap lookup for the summed columns, and a
// sort to group (only ~9 distinct (host, model) pairs, so the sort — not the
// grouping — is the cost). DeveloperCostsWindow instead rides
// idx_token_events_scores, which leads with `developer`. The cost here is
// WINDOW-proportional, and the #333 DECISION is to keep it that way, taken on the
// measurement that it beats every index option. (The issue is still OPEN pending
// the evidence being posted; this comment records the measurement, not a closure.)
// See the handler's call site for the numbers before adding an index that leads
// with (host, model).
func (d *DB) CostCompositionWindow(ctx context.Context, since, until time.Time, scope RepoScope) (CostComposition, error) {
	stmt, args := costCompositionStmt(since, until, scope)
	rows, err := d.db.QueryContext(ctx, stmt, args...)
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
func (d *DB) UnattributedBucketCostsWindow(ctx context.Context, since, until time.Time, scope RepoScope) ([]UnattributedBucketCost, error) {
	where, args := tsWindow(since, until)
	// Family binds lead the tsWindow bounds in text order (same convention as
	// CostCompositionWindow); the scope predicate trails both.
	args = append([]any{UnattributedIssueID, unattributedGlobPattern}, args...)
	scopeSQL, scopeArgs := scope.clause()
	args = append(args, scopeArgs...)
	rows, err := d.db.QueryContext(ctx, `
		SELECT developer, issue_id, SUM(cost_micro) AS cost
		FROM token_events
		WHERE (`+unattributedFamilyPredicate+`) AND `+where+scopeSQL+`
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
func (d *DB) DistinctPriceVersionsWindow(ctx context.Context, since, until time.Time, scope RepoScope) ([]int, error) {
	where, args := tsWindow(since, until)
	scopeSQL, scopeArgs := scope.clause()
	args = append(args, scopeArgs...)
	rows, err := d.db.QueryContext(ctx, `
		SELECT DISTINCT price_version
		FROM token_events
		WHERE `+where+` AND price_version > 0`+scopeSQL+`
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
// Scoping (#590): under a non-FleetWide scope this reports the horizon of THAT
// repository, not the installation's. Threading it is not cosmetic — an installation
// capturing since January that onboarded a repo in June would otherwise answer
// "January" for that repo and set window_predates_cost_capture=false, asserting a
// scoped window is fully covered on the strength of a DIFFERENT repository's data,
// and handing the operator a cost_coverage_safe_since months before the scope has any
// data at all. That is this issue's own defect class relocated from the headline
// figure into the annotation that exists to prevent silently-wrong coverage claims.
func (d *DB) CostCoverageStart(ctx context.Context, scope RepoScope) (time.Time, bool, error) {
	scopeSQL, scopeArgs := scope.clause()
	// clause() is written to follow an existing predicate, so this read — which has
	// no WHERE of its own — supplies a vacuous one rather than special-casing the
	// fragment. `1 = 1` costs nothing and keeps a single clause shape in the package.
	var ts sql.NullTime
	err := d.db.QueryRowContext(ctx,
		`SELECT ts FROM token_events WHERE 1 = 1`+scopeSQL+` ORDER BY ts LIMIT 1`,
		scopeArgs...,
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
// Scoping (#590): same reasoning as CostCoverageStart. Left unscoped, a scoped
// response would list capture sources that contributed nothing to the scope — I
// measured a scoped response advertising a `proxy` horizon when every proxy row had
// been strictly excluded by that same scope.
func (d *DB) SourceCoverageStart(ctx context.Context, scope RepoScope) (map[string]time.Time, error) {
	scopeSQL, scopeArgs := scope.clause()
	rows, err := d.db.QueryContext(ctx,
		`SELECT DISTINCT source FROM token_events WHERE 1 = 1`+scopeSQL, scopeArgs...)
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
		// Scope trails the source bind, matching text order.
		err := d.db.QueryRowContext(ctx,
			`SELECT ts FROM token_events WHERE source = ?`+scopeSQL+` ORDER BY ts LIMIT 1`,
			append([]any{src}, scopeArgs...)...,
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
// Scoping (#590): under a non-FleetWide scope the per-window predicate goes STRICT —
// the 'unqualified' sentinel is no longer admitted. This matters because the tripwire
// is a data-quality signal, not just a cost figure: leaving the join tolerant inside a
// scoped read would let a repo-blind token row belonging to some OTHER repository
// "fund" a scoped outcome and suppress a zero-token flag that should have fired. A
// false negative on a tripwire is worse than a missing one, so a scoped read tolerates
// nothing it cannot attribute.
func (d *DB) OutcomeTokenTotals(ctx context.Context, outcomes []Outcome, scope RepoScope) (map[DevIssue]int64, error) {
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
			switch {
			case !scope.IsFleetWide():
				// Scoped (#590): strict, and bound to the SCOPE rather than to k.repo.
				// Binding the scope is what makes this defensive rather than merely
				// consistent — a caller that hands us an out-of-scope outcome cannot
				// pull that repository's tokens in through the back door.
				sb.WriteString("(issue_id = ? AND ts >= ? AND ts <= ? AND repo = ?)")
				args = append(args, k.issue, w.low, w.high, scope.String())
			case repoid.IsReal(k.repo):
				sb.WriteString("(issue_id = ? AND ts >= ? AND ts <= ? AND (repo = ? OR repo = 'unqualified'))")
				args = append(args, k.issue, w.low, w.high, k.repo)
			default:
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
	return d.AllOutcomesWindow(ctx, since, time.Time{}, FleetWide)
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
func (d *DB) AllOutcomesWindow(ctx context.Context, since, until time.Time, scope RepoScope) ([]Outcome, error) {
	where, args := tsWindow(since, until)
	scopeSQL, scopeArgs := scope.clause()
	args = append(args, scopeArgs...)
	rows, err := d.db.QueryContext(ctx, `
		SELECT developer, issue_id, COALESCE(pr_number,0), weight, quality,
		       COALESCE(weight_source, 'legacy'), COALESCE(additions, 0),
		       COALESCE(deletions, 0), COALESCE(changed_files, 0),
		       COALESCE(source, 'github-webhook'),
		       COALESCE(work_type, 'feature'), COALESCE(work_type_source, 'legacy'),
		       COALESCE(repo, 'unqualified'), ts
		FROM outcomes WHERE `+where+scopeSQL, args...,
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
// 🔴 THE CHECK-THEN-ACT IS ATOMIC ACROSS PROCESSES ONLY BECAUSE OF THE WRITE
// LOCK. In-process, SetMaxOpenConns(1) is what stops two racing calls from each
// passing the chain check and jointly forming a chain; it says nothing about a
// second process. This site formerly passed
// sql.TxOptions{Isolation: sql.LevelSerializable} and its comment claimed that
// took the lock up front — it did not. modernc.org/sqlite ignores
// sql.TxOptions.Isolation entirely (newTx reads only opts.ReadOnly and the
// connection-global beginMode, which comes solely from a `_txlock` DSN parameter
// this store does not set), so that transaction was DEFERRED and the
// deferred-to-write upgrade failed with an unretried SQLITE_BUSY_SNAPSHOT (517).
//
// ⚠️ IT USES beginImmediateBounded, NOT beginImmediate, AND THAT IS DELIBERATE —
// this is REQUEST PATH. ctx here is r.Context() (handlePostDeveloperAlias,
// internal/api/handler.go), and the promote is NOT bounded by ctx: at the DSN's
// busy_timeout a contended promote blocks ~5s (measured 5.08s even under a 300ms
// deadline), and with SetMaxOpenConns(1) that stalls every OTHER in-flight
// request in the process too, with a client disconnect unable to shorten it.
// The bounded helper caps that wait at requestPathBusyTimeout. Do not "simplify"
// this to beginImmediate — and that is no longer a bare instruction:
// TestRequestPathWritersTakeTheBoundedWriteLock/UpsertDeveloperAlias fails on
// BOTH the reverts that were measured to survive the whole tree — back to
// `BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})` (it stops
// wrapping ErrWriteLockUnavailable) and sideways to unbounded beginImmediate (it
// stops returning inside the cap).
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

	tx, release, err := beginImmediateBounded(ctx, d.db, requestPathBusyTimeout)
	if err != nil {
		return err
	}
	defer release()

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
//   - reprice_audit / reprice_row_audit — cost-only ledgers with no developer
//     column and no other identifier (#294).
//   - repo_repair_row_audit — the #493 per-row before-image ledger. It carries
//     only (repair_id, token_event_id, old_repo, new_repo): no developer column
//     and, deliberately, no session_id — see its schema comment for why copying
//     one in would have created erasure-proof personal data. Its parent
//     repo_repair_audit rows ARE erased below, so nothing developer-identifying
//     survives; what remains is a dangling integer row id, exactly like
//     reprice_row_audit.
//   - cost_correction_audit — the #346 sanctioned cost-correction ledger. No
//     developer column and, deliberately, no idempotency_key (client-chosen
//     and potentially personal-data-bearing) — see its schema comment, the
//     SAME reasoning as repo_repair_row_audit's session_id exclusion above.
//     token_event_id is server-generated and simply stops resolving once the
//     row it names is erased or pruned. actor/reason name and explain the
//     OPERATOR who made the correction — a third party to the data subject
//     whose spend was corrected, the same class as webhook_payloads' third-
//     party identifiers, not personal data ABOUT the subject.
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
	// #493: repo_repair_audit names the developer whose cost rows a
	// `tierd repair-repo` run re-attributed, alongside the repositories their
	// spend moved into — personal data by the same reasoning as every table
	// above, and reachable by the same `developer IN (…)` shape. Added in the
	// SAME change that created the table, because a table that lands here late
	// is a compliance hole for exactly as long as it takes someone to notice.
	"repo_repair_audit",
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
// 🔴 The alias read and the cascade are one atomic unit against another PROCESS
// only because beginImmediateBounded holds the write lock from the first
// statement; in-process, SetMaxOpenConns(1) is what serialises it. This site
// formerly passed sql.TxOptions{Isolation: sql.LevelSerializable} and claimed
// that was "BEGIN IMMEDIATE ... takes the write lock up front". It was not:
// modernc.org/sqlite never reads sql.TxOptions.Isolation, so the transaction was
// DEFERRED and took no lock until its first DELETE, and cross-process the
// read-then-write degraded to an unretried SQLITE_BUSY_SNAPSHOT (517) — a FAILED
// erasure rather than a partial one (the tx was still all-or-nothing), but an
// opaque one.
//
// ⚠️ BOUNDED, NOT PLAIN beginImmediate, BECAUSE THIS IS REQUEST PATH. ctx is
// r.Context() (handleEraseDeveloper, internal/api/handler.go) and the promote is
// NOT bounded by ctx: at the DSN's busy_timeout a contended promote blocks ~5s
// (measured 5.08s even under a 300ms deadline), and with SetMaxOpenConns(1) that
// stalls every OTHER in-flight request in the process behind the single
// connection. requestPathBusyTimeout caps it. Do not "simplify" to beginImmediate
// — TestRequestPathWritersTakeTheBoundedWriteLock/EraseDeveloper fails on BOTH the
// reverts that were measured to survive the whole tree: back to
// `BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})` (it stops
// wrapping ErrWriteLockUnavailable) and sideways to unbounded beginImmediate (it
// stops returning inside the cap).
func (d *DB) EraseDeveloper(ctx context.Context, id string) (map[string]int64, error) {
	if id == "" {
		return nil, errors.New("EraseDeveloper: id must not be empty")
	}
	tx, release, err := beginImmediateBounded(ctx, d.db, requestPathBusyTimeout)
	if err != nil {
		return nil, err
	}
	defer release()

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
	// RepoRepairAudit is the #493 ledger of every `tierd repair-repo` run that
	// re-attributed THIS person's cost rows to a repository. Disclosed because a
	// record saying "N of your rows and $X of your spend were moved into
	// owner/repo by this binary on this date" is personal data about the subject,
	// and because a DSAR that omitted it would leave the subject unable to see a
	// retroactive change to how their own spend is attributed.
	RepoRepairAudit []ExportRepoRepairAudit `json:"repo_repair_audit"`
	DeveloperAlias  []ExportDeveloperAlias  `json:"developer_alias"`
}

// RowCount is the total number of stored rows in the export across every table.
// The API layer maps a zero total to 404 (no data / already erased).
func (e DeveloperExport) RowCount() int {
	return len(e.TokenEvents) + len(e.Outcomes) + len(e.ActualSpend) +
		len(e.OrgHierarchy) + len(e.PeriodMembership) + len(e.QualityEvents) +
		len(e.QualityHistory) + len(e.RepoRepairAudit) + len(e.DeveloperAlias)
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

// ExportRepoRepairAudit mirrors one stored repo_repair_audit row (#493): one
// `tierd repair-repo` run's effect on ONE target repository for this subject.
// RepairID correlates it with the per-row before-images in repo_repair_row_audit
// (which hold no personal data of their own — see that table's schema comment).
type ExportRepoRepairAudit struct {
	ID           int64     `json:"id"`
	RepairID     string    `json:"repair_id"`
	Developer    string    `json:"developer"`
	FromRepo     string    `json:"from_repo"`
	ToRepo       string    `json:"to_repo"`
	RowCount     int64     `json:"row_count"`
	CostMicroSum int64     `json:"cost_micro_sum"`
	ToolVersion  string    `json:"tool_version"`
	Timestamp    time.Time `json:"ts"`
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

	// repo_repair_audit (#493)
	if err := d.queryRows(ctx,
		`SELECT id, repair_id, developer, from_repo, to_repo, row_count,
		        cost_micro_sum, tool_version, ts
		 FROM repo_repair_audit WHERE developer IN (`+placeholders+`) ORDER BY id`, args,
		func(rows *sql.Rows) error {
			var r ExportRepoRepairAudit
			if err := rows.Scan(&r.ID, &r.RepairID, &r.Developer, &r.FromRepo, &r.ToRepo,
				&r.RowCount, &r.CostMicroSum, &r.ToolVersion, &r.Timestamp); err != nil {
				return err
			}
			exp.RepoRepairAudit = append(exp.RepoRepairAudit, r)
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
