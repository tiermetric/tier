PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
CREATE TABLE token_events (
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
    pr_number       INTEGER,
    branch_hash     TEXT,
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
    -- with ITS table), so this records the original basis used for cost,
    -- including later atomic token-derived count/cost promotions. NOT NULL with
    -- a convergent DEFAULT 0 (the "unstamped"
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
    -- #854: what a POST /api/v1/costs row declared about who billed it. 'other'
    -- (the only value) means the spend is not billed to an org whose usage poller
    -- this server runs. NULL for every other producer and every undeclared /costs
    -- row. Written only by /costs, INSERT-only, in no unique index, and read by no
    -- spend query, so it is additive and schemaVersion is NOT bumped.
    billed_to       TEXT,
    -- #823: which AttributionRule assigned issue_id; NULL = none recorded. Written
    -- only by the INSERT (first writer wins, like issue_id), in no index, no
    -- DEFAULT, no CHECK: the closed set is enforced by ValidateAttributionRule.
    attribution_rule TEXT,
    -- Poller baseline cutover: NULL on old-rule/non-poller rows; new poll days
    -- carry 'per-token-host-v1'. INSERT-only, no backfill or schemaVersion bump.
    poller_baseline TEXT,
    ts              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
, cost_clamped INTEGER, pricing_basis TEXT, rate_snapshot TEXT, cache_write_evidence TEXT);
INSERT INTO token_events VALUES(12,'alice','issue-1','test',0,0,0,0,0,0,'jsonl','realtime','unqualified',NULL,NULL,'old',NULL,14,'unknown','self_hosted_amortized',NULL,NULL,NULL,'2026-10-17 00:00:00 +0000 UTC',NULL,'guessed_estimate','rate1:f2596330a72983eef6f484a1db57933c83cd135ead893334c0db9d1b803d3245',NULL);
CREATE TABLE outcomes (
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
    -- CAN re-score new rows. NULL means the producer supplied no value (legacy
    -- rows, push rows, or an absent/null input).
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
    -- one UTC day collapse into ONE outcome, keyed by (repo, issue_id, push_day) via
    -- the partial unique index idx_outcomes_push_daily_repo in schemaPostMigration. NULL on
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
    ts                DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    -- author_type is the GitHub account type of the PR author as the signed
    -- webhook delivered it (user.type: 'User', 'Bot', 'Organization',
    -- 'Mannequin'), #856. NULLABLE, no DEFAULT: NULL means "not captured" — every
    -- pre-#856 row, push rows (a commit author carries no type) and
    -- POST /api/v1/outcomes rows. It decides only whether the author counts toward
    -- the k-anonymity floor; no figure reads it.
    author_type       TEXT
, head_branch_hash TEXT, created_at TIMESTAMP, merged_at TIMESTAMP);
INSERT INTO outcomes VALUES(7,'alice','issue-1',0,1.0,1.0,'old','legacy',NULL,NULL,NULL,'github-webhook',NULL,'feature','default','unqualified','2026-10-17 00:00:00 +0000 UTC',NULL,NULL,NULL,NULL);
CREATE TABLE org_hierarchy (
    developer   TEXT    PRIMARY KEY,
    team        TEXT    NOT NULL,
    division    TEXT,
    org         TEXT
);
CREATE TABLE period_membership (
    developer    TEXT NOT NULL,
    org          TEXT NOT NULL,
    period_start TEXT NOT NULL CHECK (period_start GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]'),
    period_end   TEXT          CHECK (period_end IS NULL OR (period_end GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]' AND period_end >= period_start)),
    ended_reason TEXT          CHECK (ended_reason IN ('explicit','move','unknown'))
);
CREATE TABLE hierarchy_membership (
    id          INTEGER  PRIMARY KEY AUTOINCREMENT,
    developer   TEXT     NOT NULL,
    team        TEXT     NOT NULL,
    division    TEXT     NOT NULL DEFAULT '',
    valid_from  DATETIME NOT NULL,
    valid_to    DATETIME CHECK (valid_to IS NULL OR valid_to >= valid_from),
    written_by  TEXT     NOT NULL CHECK (written_by != '')
);
CREATE TABLE actual_spend (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    developer         TEXT    NOT NULL,
    period            TEXT    NOT NULL CHECK (period GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]'),
    -- actual paid in integer micro-dollars (#69); negative deltas allowed
    -- (credit memos / refunds, #24). Pre-#69 DBs carried actual_paid_usd REAL,
    -- converted by migrateActualSpendToMicro.
    actual_paid_micro INTEGER NOT NULL,
    ts                DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE org_actual_spend (
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
CREATE TABLE tier_migrations (
    name        TEXT PRIMARY KEY,
    applied_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
INSERT INTO tier_migrations VALUES('aggregate-approx-v1-2026-10-14','2026-10-18 00:48:15');
INSERT INTO tier_migrations VALUES('original-basis-history-preserved-v1-2026-10-14','2026-10-18 00:48:15');
INSERT INTO tier_migrations VALUES('cache_ttl_recompute_v55','2026-10-18 00:48:15');
INSERT INTO tier_migrations VALUES('price_version_backfill_v233','2026-10-18 00:48:15');
INSERT INTO tier_migrations VALUES('period_membership_backfill_v41','2026-10-18 00:48:15');
INSERT INTO tier_migrations VALUES('hierarchy_membership_baseline_v886','2026-10-18 00:48:15');
INSERT INTO tier_migrations VALUES('hierarchy_membership_alias_history_v914','2026-10-18 00:48:15');
INSERT INTO tier_migrations VALUES('membership_ended_reason_v945','2026-10-18 00:48:15');
INSERT INTO tier_migrations VALUES('quality-trigger-recording-v1','2026-10-18 00:48:15');
CREATE TABLE watcher_checkpoint (
    path        TEXT PRIMARY KEY,
    inode       INTEGER NOT NULL,
    byte_offset INTEGER NOT NULL,
    head_crc    INTEGER NOT NULL,
    head_len    INTEGER NOT NULL,
    metadata    TEXT NOT NULL,
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE developer_alias (
    alias      TEXT NOT NULL,
    canonical  TEXT NOT NULL,
    ts         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE webhook_payloads (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    event       TEXT    NOT NULL,           -- X-GitHub-Event
    delivery_id TEXT    NOT NULL DEFAULT '', -- X-GitHub-Delivery (empty for test clients)
    body_gz     BLOB    NOT NULL,           -- gzip(raw request body)
    body_sha256 TEXT    NOT NULL,           -- hex digest of the RAW body (integrity/dedup aid)
    received_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE quality_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    outcome_id   INTEGER NOT NULL,          -- outcomes.id (row-targeted, fixes C9 scoping)
    developer    TEXT    NOT NULL,          -- denormalised for audit reads
    issue_id     TEXT    NOT NULL,
    event_type   TEXT    NOT NULL,          -- ci_pass|ci_fail|ci_fail_flaky|revert_quality|revert_strategic|revert_ambiguous (validated in Go)
    source_ref   TEXT    NOT NULL,          -- head_sha:attempt:workflow_id (#687; pre-#687 rows are head_sha:attempt and still parse) / revert commit sha
    event_ts     DATETIME NOT NULL,
    recorded_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    reason       TEXT NOT NULL DEFAULT ''  -- checked-but-unresolved diagnostic, not a scoring rule
, trigger_event TEXT);
INSERT INTO quality_events VALUES(1,7,'alice','issue-1','ci_fail','old:1:42','2026-10-17 00:00:00 +0000 UTC','2026-10-18 00:48:15','',NULL);
CREATE TABLE quality_history (
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
CREATE TABLE reprice_audit (
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
CREATE TABLE reprice_row_audit (
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
    ts                DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, old_pricing_basis TEXT, old_rate_snapshot TEXT,
    UNIQUE(reprice_id, token_event_id)
);
CREATE TABLE repo_repair_audit (
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
CREATE TABLE repo_repair_row_audit (
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
CREATE TABLE cost_correction_audit (
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
CREATE TABLE price_table_registry (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    -- the price table's declared 'version:' -- the same integer stamped into
    -- token_events.price_version by every row priced under it.
    version        INTEGER NOT NULL,
    -- #713 content identity of the RESOLVED table, "tierpt1:<64 hex>". THE
    -- BINDING IDENTITY: this is what a second load of the same version is
    -- compared against.
    table_hash     TEXT    NOT NULL,
    -- #713 digest of the RAW source bytes, "sha256:<64 hex>". Recorded for
    -- forensics (which FILE produced this table) and never compared.
    file_hash      TEXT    NOT NULL,
    -- the table's declared effective_date and resolved model count, so a
    -- registry row is readable on its own without the file that made it.
    effective_date TEXT    NOT NULL,
    model_count    INTEGER NOT NULL,
    -- 'embedded' or the override path this table was read from. Provenance for
    -- a local operator; not compared (see the note above).
    source         TEXT    NOT NULL,
    -- the binary that first served this version, from SetToolVersion.
    tool_version   TEXT    NOT NULL,
    first_seen     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    -- SOFT DELETE (the maintainer's ruling on the #714 escalation). 'tierd prices
    -- forget-version --commit' does NOT remove this row; it stamps these two
    -- columns and writes a price_forget_audit ledger row. NULL means LIVE.
    --
    -- 🔴 THE RULING, AND THE REASON IT OVERRODE THE ISSUE'S OWN WORDING: this
    -- whole program exists so provenance is not lost, and an escape hatch that
    -- erases the record of its own use contradicts the thing it protects. A hard
    -- DELETE left the database holding the NEW hash under a reused version with
    -- no trace that another table ever claimed it -- the exact end state a
    -- --accept-rehash flag would have produced. After a forget, this database can
    -- still answer 'version N once meant table_hash X, forgotten at T by U'.
    --
    -- ⚠️ forgotten_by IS A SELF-ASSERTED CLAIM, NOT A VERIFIED IDENTITY -- the
    -- same honesty cost_correction_audit.actor carries. It defaults to the OS
    -- username of whoever ran the command and can be overridden with --actor;
    -- nothing authenticates it, because a local CLI has no principal to bind it
    -- to. Read it as a claim, corroborated by price_forget_audit.tool_version.
    forgotten_at   DATETIME,
    forgotten_by   TEXT
);
INSERT INTO price_table_registry VALUES(1,14,'tierpt1:ac926691fb6f6013d7e72f854b895b8dd122f7a5c731e50710e70434a578a5de','sha256:4a45c132c99cb2600d539d478513ccd7cdba93a2a9313241176e231e3a7c5529','2026-10-12',93,'embedded','unknown','2026-10-18 00:48:15',NULL,NULL);
CREATE TABLE price_forget_audit (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    -- forget_id groups every row written by ONE forget-version run, so two runs
    -- never intermingle. Same minting helper as reprice_id / repair_id.
    forget_id             TEXT    NOT NULL,
    -- WHAT WAS FORGOTTEN: the version, and the full before-image of what it
    -- meant. table_hash is copied rather than joined so this ledger still answers
    -- 'version N once meant X' if the registry row is later hard-deleted.
    version               INTEGER NOT NULL,
    table_hash            TEXT    NOT NULL,
    file_hash             TEXT    NOT NULL,
    price_effective_date  TEXT    NOT NULL,
    model_count           INTEGER NOT NULL,
    source                TEXT    NOT NULL,
    -- provenance of the RECORDING that was forgotten: which binary first served
    -- that table, and when it was recorded.
    recorded_tool_version TEXT    NOT NULL,
    first_seen            DATETIME NOT NULL,
    -- WHO FORGOT IT. actor is SELF-ASSERTED (see forgotten_by above);
    -- tool_version is the binary that ran the command and is not caller-chosen.
    actor                 TEXT    NOT NULL,
    tool_version          TEXT    NOT NULL,
    ts                    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE push_outcome_commits (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    repo        TEXT    NOT NULL,
    commit_sha  TEXT    NOT NULL,
    outcome_id  INTEGER NOT NULL,
    developer   TEXT    NOT NULL,
    ts          DATETIME NOT NULL,
    push_order  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE push_outcome_audit (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    outcome_id  INTEGER NOT NULL,
    repo        TEXT    NOT NULL,
    issue_id    TEXT    NOT NULL,
    push_day    TEXT    NOT NULL,
    action      TEXT    NOT NULL,
    developer   TEXT    NOT NULL,
    outcome_ts  DATETIME NOT NULL,
    commit_sha  TEXT    NOT NULL,
    ts          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    commit_developer TEXT NOT NULL DEFAULT ''
);
CREATE TABLE correction_declarations (
 id INTEGER PRIMARY KEY AUTOINCREMENT, outcome_id INTEGER REFERENCES outcomes(id) ON DELETE SET NULL,
 developer TEXT, repo TEXT CHECK(repo IS NULL OR (repo != 'unqualified' AND instr(repo,'/')>1)), pr_number INTEGER, merge_commit_sha TEXT, merged_at TIMESTAMP, opened_at TIMESTAMP,
 additions INTEGER, deletions INTEGER, changed_files INTEGER,
 declaration_state TEXT NOT NULL CHECK(declaration_state IN ('missing','invalid','none','parents')),
 supporting_text TEXT CHECK(length(CAST(supporting_text AS BLOB))<=4096), provenance TEXT CHECK(length(CAST(provenance AS BLOB))<=128),
 recorded_at TIMESTAMP NOT NULL, rule_version TEXT NOT NULL
);
CREATE TABLE corrective_relations (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 child_declaration_id INTEGER REFERENCES correction_declarations(id), parent_declaration_id INTEGER REFERENCES correction_declarations(id),
 child_outcome_id INTEGER REFERENCES outcomes(id) ON DELETE SET NULL, parent_outcome_id INTEGER REFERENCES outcomes(id) ON DELETE SET NULL,
 child_developer TEXT, parent_developer TEXT, child_repo TEXT, parent_repo TEXT, child_pr_number INTEGER, parent_pr_number INTEGER,
 child_merge_commit_sha TEXT, parent_merge_commit_sha TEXT, child_merged_at TIMESTAMP, parent_merged_at TIMESTAMP,
 state TEXT NOT NULL, resolution_reason TEXT CHECK(length(CAST(resolution_reason AS BLOB))<=4096), revision INTEGER NOT NULL,
 recorded_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL, rule_version TEXT NOT NULL,
 supporting_text TEXT CHECK(length(CAST(supporting_text AS BLOB))<=4096),
 disposition_text TEXT CHECK(length(CAST(disposition_text AS BLOB))<=4096), appeal_text TEXT CHECK(length(CAST(appeal_text AS BLOB))<=4096),
 CHECK(child_repo != parent_repo OR child_pr_number != parent_pr_number)
);
CREATE TABLE correction_audit (
 id INTEGER PRIMARY KEY AUTOINCREMENT, relation_id INTEGER REFERENCES corrective_relations(id), revision INTEGER NOT NULL, action TEXT NOT NULL,
 actor TEXT CHECK(length(CAST(actor AS BLOB))<=128), actor_role TEXT CHECK(length(CAST(actor_role AS BLOB))<=128), recorded_at TIMESTAMP NOT NULL, rule_version TEXT NOT NULL,
 child_declaration_id INTEGER REFERENCES correction_declarations(id), parent_declaration_id INTEGER REFERENCES correction_declarations(id),
 child_outcome_id INTEGER REFERENCES outcomes(id) ON DELETE SET NULL, parent_outcome_id INTEGER REFERENCES outcomes(id) ON DELETE SET NULL,
 child_developer TEXT, parent_developer TEXT, child_repo TEXT, parent_repo TEXT, child_pr_number INTEGER, parent_pr_number INTEGER,
 child_merge_commit_sha TEXT, parent_merge_commit_sha TEXT, child_merged_at TIMESTAMP, parent_merged_at TIMESTAMP,
 reason_text TEXT CHECK(length(CAST(reason_text AS BLOB))<=4096)
);
CREATE TABLE correction_stats_cutover (
 id INTEGER PRIMARY KEY CHECK(id=1), outcome_id INTEGER NOT NULL
);
INSERT INTO correction_stats_cutover VALUES(1,0);
CREATE TABLE pricing_rate_snapshots (
 id TEXT PRIMARY KEY, body TEXT NOT NULL);
INSERT INTO pricing_rate_snapshots VALUES('rate1:f2596330a72983eef6f484a1db57933c83cd135ead893334c0db9d1b803d3245','{"Rule":"request-v1","TableHash":"tierpt1:ac926691fb6f6013d7e72f854b895b8dd122f7a5c731e50710e70434a578a5de","Version":14,"Rate":{"InputPerM":0.5,"OutputPerM":0,"Combined":true,"Provider":"self-hosted","CacheReadMult":1,"CacheWrite5mMult":1,"CacheWrite1hMult":1,"ContextThreshold":0,"InputPerMOver":0,"OutputPerMOver":0,"BillingMode":"self_hosted_amortized"},"Guessed":true}');
CREATE TABLE pricing_observation_audit (
 id INTEGER PRIMARY KEY, developer TEXT NOT NULL, token_event_id INTEGER NOT NULL,
 kind TEXT NOT NULL, before_json TEXT NOT NULL, after_json TEXT NOT NULL, ts DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
INSERT INTO pricing_observation_audit VALUES(1,'alice',1,'initial_observation','{"Counts":[0,0,0,0,0],"CostMicro":0,"Evidence":"","Source":"jsonl","Snapshot":"rate1:f2596330a72983eef6f484a1db57933c83cd135ead893334c0db9d1b803d3245","Basis":"guessed_estimate"}','{"Counts":[0,0,0,0,0],"CostMicro":0,"Evidence":"","Source":"jsonl","Snapshot":"rate1:f2596330a72983eef6f484a1db57933c83cd135ead893334c0db9d1b803d3245","Basis":"guessed_estimate"}','2026-10-18 00:48:15');
CREATE TABLE outcome_exclusions (
 repo TEXT NOT NULL,
 exclusion_repo TEXT NOT NULL,
 author_developer TEXT NOT NULL,
 exclusion_author_type TEXT,
 pr_number INTEGER,
 merge_commit_sha TEXT,
 exclusion_pr_number INTEGER NOT NULL,
 exclusion_merge_commit_sha TEXT NOT NULL,
 id INTEGER,
 developer TEXT NOT NULL,
 issue_id TEXT NOT NULL DEFAULT '',
 weight REAL NOT NULL DEFAULT 0,
 quality REAL NOT NULL DEFAULT 1,
 weight_source TEXT NOT NULL DEFAULT 'legacy',
 additions INTEGER,
 deletions INTEGER,
 changed_files INTEGER,
 source TEXT NOT NULL DEFAULT 'github-webhook',
 push_day TEXT,
 work_type TEXT NOT NULL DEFAULT 'feature',
 work_type_source TEXT NOT NULL DEFAULT 'default',
 ts DATETIME NOT NULL,
 author_type TEXT,
 head_branch_hash TEXT,
 created_at DATETIME,
 merged_at DATETIME,
 reason TEXT NOT NULL,
 evidence TEXT NOT NULL,
 actor TEXT NOT NULL,
 excluded_at DATETIME NOT NULL,
 PRIMARY KEY (exclusion_repo, exclusion_pr_number, exclusion_merge_commit_sha)
);
INSERT INTO outcome_exclusions VALUES('acme/app','acme/app','alice',NULL,NULL,NULL,1,'old-archive',NULL,'alice','',0.0,1.0,'legacy',NULL,NULL,NULL,'github-webhook',NULL,'feature','default','2026-10-18 00:48:15',NULL,NULL,NULL,NULL,'github-revert-pr','evidence','operator','2026-10-18 00:48:15');
CREATE TABLE correction_assessments (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 relation_id INTEGER REFERENCES corrective_relations(id),
 revision INTEGER NOT NULL, chain_version INTEGER,
 participant_outcome_id INTEGER REFERENCES outcomes(id) ON DELETE SET NULL,
 basis TEXT NOT NULL CHECK(basis IN ('provisional','reviewed')),
 original_weight TEXT CHECK(length(original_weight)<=4096), original_quality TEXT CHECK(length(original_quality)<=4096),
 reviewed_own_weight TEXT CHECK(length(reviewed_own_weight)<=4096),
 before_weight TEXT CHECK(length(before_weight)<=4096), after_weight TEXT CHECK(length(after_weight)<=4096),
 marginal_credit TEXT CHECK(length(marginal_credit)<=4096), pure_repair INTEGER NOT NULL DEFAULT 0 CHECK(pure_repair IN (0,1)),
 comparison_base TEXT CHECK(length(CAST(comparison_base AS BLOB))<=128),
 comparison_before TEXT CHECK(length(CAST(comparison_before AS BLOB))<=128),
 comparison_after TEXT CHECK(length(CAST(comparison_after AS BLOB))<=128),
 scope TEXT CHECK(length(CAST(scope AS BLOB))<=4096), rationale TEXT CHECK(length(CAST(rationale AS BLOB))<=4096),
 reviewer TEXT CHECK(length(CAST(reviewer AS BLOB))<=128), reviewed_at TIMESTAMP,
 recorded_at TIMESTAMP NOT NULL, rule_version TEXT NOT NULL
);
CREATE TABLE correction_allocations (
 id INTEGER PRIMARY KEY AUTOINCREMENT, chain_version INTEGER NOT NULL,
 relation_id INTEGER REFERENCES corrective_relations(id), outcome_id INTEGER REFERENCES outcomes(id) ON DELETE SET NULL,
 allocation_parent_outcome_id INTEGER REFERENCES outcomes(id) ON DELETE SET NULL,
 original_weight TEXT NOT NULL CHECK(length(original_weight)<=4096),
 original_quality TEXT NOT NULL CHECK(length(original_quality)<=4096),
 role TEXT NOT NULL, raw_weight TEXT NOT NULL CHECK(length(raw_weight)<=4096),
 original_term TEXT NOT NULL CHECK(length(original_term)<=4096),
 reviewed_own_weight TEXT NOT NULL CHECK(length(reviewed_own_weight)<=4096),
 before_weight TEXT NOT NULL CHECK(length(before_weight)<=4096), after_weight TEXT NOT NULL CHECK(length(after_weight)<=4096),
 share TEXT NOT NULL CHECK(length(share)<=4096), projected_quality TEXT NOT NULL CHECK(length(projected_quality)<=4096),
 allocated_points TEXT NOT NULL CHECK(length(allocated_points)<=4096), marginal_credit TEXT NOT NULL CHECK(length(marginal_credit)<=4096),
 ranking_eligibility TEXT NOT NULL CHECK(ranking_eligibility IN ('eligible','ineligible','unknown')),
 status TEXT NOT NULL, pending_reason TEXT NOT NULL, provisional INTEGER NOT NULL CHECK(provisional IN (0,1)),
 disputed INTEGER NOT NULL DEFAULT 0 CHECK(disputed IN (0,1)),
 frozen INTEGER NOT NULL DEFAULT 0 CHECK(frozen IN (0,1)),
 recorded_at TIMESTAMP NOT NULL, rule_version TEXT NOT NULL
);
CREATE TABLE correction_policy (
 id INTEGER PRIMARY KEY CHECK(id=1), scoring_enabled INTEGER NOT NULL DEFAULT 0 CHECK(scoring_enabled IN (0,1)),
 rubric_version INTEGER NOT NULL DEFAULT 1, rule_version TEXT NOT NULL DEFAULT 'legacy-v1',
 projection_revision INTEGER NOT NULL DEFAULT 0
);
INSERT INTO correction_policy VALUES(1,0,1,'legacy-v1',0);
CREATE TABLE sealed_report (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    level         TEXT    NOT NULL,
    period_size   TEXT    NOT NULL,
    period_start  TEXT    NOT NULL,
    period_end    TEXT    NOT NULL,
    k             INTEGER NOT NULL,
    config_digest TEXT    NOT NULL,
    sealed_at     TEXT    NOT NULL,
    body          BLOB    NOT NULL,
    body_digest   TEXT    NOT NULL,
    tool_version  TEXT    NOT NULL,
    tool_commit   TEXT    NOT NULL, grace_ns INTEGER, revert_windows_open INTEGER, scoring_policy TEXT, correction_rule_version TEXT, rubric_version INTEGER, correction_revision INTEGER,
    CHECK (period_start < period_end)
);
CREATE TABLE sealed_rollup (
    report_id        INTEGER NOT NULL REFERENCES sealed_report(id),
    label            TEXT    NOT NULL,
    weighted_points  REAL    NOT NULL,
    total_cost_usd   REAL    NOT NULL,
    actual_paid_usd  REAL    NOT NULL,
    realtime_usd     REAL    NOT NULL,
    sample_n         INTEGER NOT NULL,
    flagged_outcomes INTEGER NOT NULL,
    has_points       INTEGER NOT NULL CHECK (has_points IN (0, 1)),
    has_cost         INTEGER NOT NULL CHECK (has_cost IN (0, 1)),
    has_realtime     INTEGER NOT NULL CHECK (has_realtime IN (0, 1)),
    has_non_realtime INTEGER NOT NULL CHECK (has_non_realtime IN (0, 1)),
    has_paid         INTEGER NOT NULL CHECK (has_paid IN (0, 1)),
    contributes      INTEGER NOT NULL CHECK (contributes IN (0, 1)), outcome_n INTEGER,
    PRIMARY KEY (report_id, label)
);
CREATE TABLE sealed_person (
    report_id  INTEGER NOT NULL REFERENCES sealed_report(id),
    label      TEXT    NOT NULL,
    measure    TEXT    NOT NULL,
    person_key BLOB    NOT NULL,
    tombstoned INTEGER NOT NULL DEFAULT 0 CHECK (tombstoned IN (0, 1)),
    PRIMARY KEY (report_id, label, measure, person_key)
);
CREATE TABLE erase_epoch (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    n  INTEGER NOT NULL
);
CREATE TABLE install_secret (
    id     INTEGER PRIMARY KEY CHECK (id = 1),
    secret BLOB    NOT NULL CHECK (length(secret) = 32)
);
INSERT INTO install_secret VALUES(1,X'16a90b2314607de122e3d93421b2267272fb61e7876e94ba4b7a880913521cbd');
CREATE TABLE seal_floor (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    period_start TEXT    NOT NULL
);
CREATE TABLE sealed_gap (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    period_size  TEXT    NOT NULL,
    period_start TEXT    NOT NULL,
    period_end   TEXT    NOT NULL,
    category     TEXT    NOT NULL CHECK (category <> ''),
    reason       TEXT    NOT NULL CHECK (reason <> ''),
    created_at   TEXT    NOT NULL,
    tool_version TEXT    NOT NULL,
    tool_commit  TEXT    NOT NULL,
    CHECK (period_start < period_end)
);
CREATE TABLE source_watermark (
    source          TEXT PRIMARY KEY CHECK (source <> ''),
    registered_at   TEXT NOT NULL,
    retired_at      TEXT,
    covered_from    TEXT,
    settled_through TEXT,
    gap_from        TEXT,
    CHECK ((covered_from IS NULL) = (settled_through IS NULL)),
    CHECK (covered_from IS NULL OR covered_from <= settled_through)
);
CREATE TABLE source_run (
    source          TEXT NOT NULL,
    covered_from    TEXT NOT NULL,
    settled_through TEXT NOT NULL,
    CHECK (covered_from <= settled_through),
    PRIMARY KEY (source, covered_from)
);
CREATE TABLE source_loss (
    source       TEXT NOT NULL,
    lost_from    TEXT NOT NULL,
    lost_through TEXT NOT NULL,
    CHECK (lost_from <= lost_through),
    PRIMARY KEY (source, lost_from)
);
CREATE TABLE source_registration (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    registered_at TEXT NOT NULL
);
CREATE TABLE canonical_id_history (
    alias      TEXT NOT NULL,
    canonical  TEXT NOT NULL,
    since_seal INTEGER,
    ended_seal INTEGER,
    CHECK (alias <> canonical)
);
-- Discard sequences populated by the inserts before restoring the originals.
DELETE FROM sqlite_sequence;
INSERT INTO sqlite_sequence VALUES('price_table_registry',1);
INSERT INTO sqlite_sequence VALUES('hierarchy_membership',0);
INSERT INTO sqlite_sequence VALUES('token_events',1);
INSERT INTO sqlite_sequence VALUES('outcomes',1);
INSERT INTO sqlite_sequence VALUES('quality_events',1);
CREATE TRIGGER pricing_snapshot_immutable BEFORE UPDATE ON pricing_rate_snapshots BEGIN SELECT RAISE(ABORT,'immutable pricing snapshot'); END;
CREATE TRIGGER pricing_snapshot_no_delete BEFORE DELETE ON pricing_rate_snapshots BEGIN SELECT RAISE(ABORT,'immutable pricing snapshot'); END;
CREATE TRIGGER trg_cost_correction_audit_no_update
BEFORE UPDATE ON cost_correction_audit
BEGIN
    SELECT RAISE(ABORT, 'cost_correction_audit is append-only: UPDATE is refused (#604)');
END;
CREATE TRIGGER trg_hierarchy_membership_close_only
BEFORE UPDATE ON hierarchy_membership
WHEN NOT (OLD.valid_to IS NULL AND NEW.valid_to IS NOT NULL
          AND NEW.id = OLD.id
          AND NEW.developer = OLD.developer
          AND NEW.team = OLD.team
          AND NEW.division = OLD.division
          AND NEW.valid_from = OLD.valid_from
          AND NEW.written_by = OLD.written_by)
BEGIN
    SELECT RAISE(ABORT, 'hierarchy_membership is append-only: only closing an open row is permitted (#886)');
END;
CREATE TRIGGER trg_hierarchy_membership_no_overlap
BEFORE INSERT ON hierarchy_membership
WHEN NEW.valid_to IS NOT NULL
  OR EXISTS (SELECT 1 FROM hierarchy_membership m
             WHERE m.developer = NEW.developer
               AND (m.valid_to IS NULL OR m.valid_to > NEW.valid_from))
BEGIN
    SELECT RAISE(ABORT, 'hierarchy_membership rows are appended open and after all existing rows (#886)');
END;
CREATE TRIGGER trg_seal_floor_no_update
BEFORE UPDATE ON seal_floor
BEGIN
    SELECT RAISE(ABORT, 'seal_floor is append-only: UPDATE is refused (#913)');
END;
CREATE TRIGGER trg_seal_floor_no_delete
BEFORE DELETE ON seal_floor
BEGIN
    SELECT RAISE(ABORT, 'seal_floor is append-only: DELETE is refused (#913)');
END;
CREATE TRIGGER trg_seal_floor_first_wins
BEFORE INSERT ON seal_floor
WHEN EXISTS (SELECT 1 FROM seal_floor)
BEGIN
    SELECT RAISE(IGNORE);
END;
CREATE TRIGGER trg_sealed_report_no_update
BEFORE UPDATE ON sealed_report
BEGIN
    SELECT RAISE(ABORT, 'sealed_report is append-only: UPDATE is refused (#913)');
END;
CREATE TRIGGER trg_sealed_rollup_no_update
BEFORE UPDATE ON sealed_rollup
BEGIN
    SELECT RAISE(ABORT, 'sealed_rollup is append-only: UPDATE is refused (#913)');
END;
CREATE TRIGGER trg_sealed_person_tombstone_only
BEFORE UPDATE ON sealed_person
WHEN NOT (OLD.tombstoned = 0 AND NEW.tombstoned = 1
          AND NEW.person_key <> OLD.person_key
          AND NEW.rowid = OLD.rowid
          AND NEW.report_id = OLD.report_id
          AND NEW.label = OLD.label
          AND NEW.measure = OLD.measure
          AND NOT EXISTS (SELECT 1 FROM sealed_person p
                          WHERE p.report_id = NEW.report_id AND p.label = NEW.label
                            AND p.measure = NEW.measure AND p.person_key = NEW.person_key))
BEGIN
    SELECT RAISE(ABORT, 'sealed_person is append-only: only tombstoning a person key is permitted (#914)');
END;
CREATE TRIGGER trg_install_secret_no_update
BEFORE UPDATE ON install_secret
BEGIN
    SELECT RAISE(ABORT, 'install_secret is append-only: UPDATE is refused (#913)');
END;
CREATE TRIGGER trg_install_secret_no_delete
BEFORE DELETE ON install_secret
BEGIN
    SELECT RAISE(ABORT, 'install_secret is append-only: DELETE is refused (#913)');
END;
CREATE TRIGGER trg_install_secret_first_wins
BEFORE INSERT ON install_secret
WHEN EXISTS (SELECT 1 FROM install_secret)
BEGIN
    SELECT RAISE(IGNORE);
END;
CREATE TRIGGER trg_sealed_report_first_wins
BEFORE INSERT ON sealed_report
WHEN EXISTS (SELECT 1 FROM sealed_report WHERE id = NEW.id
             OR (period_size = NEW.period_size AND period_start = NEW.period_start))
BEGIN
    SELECT RAISE(IGNORE);
END;
CREATE TRIGGER trg_sealed_rollup_no_replace
BEFORE INSERT ON sealed_rollup
WHEN EXISTS (SELECT 1 FROM sealed_rollup WHERE rowid = NEW.rowid
             OR (report_id = NEW.report_id AND label = NEW.label))
BEGIN
    SELECT RAISE(ABORT, 'sealed_rollup is append-only: a row with that key exists (#913)');
END;
CREATE TRIGGER trg_sealed_person_no_replace
BEFORE INSERT ON sealed_person
WHEN EXISTS (SELECT 1 FROM sealed_person WHERE rowid = NEW.rowid
             OR (report_id = NEW.report_id AND label = NEW.label
                 AND measure = NEW.measure AND person_key = NEW.person_key))
BEGIN
    SELECT RAISE(ABORT, 'sealed_person is append-only: a row with that key exists (#913)');
END;
CREATE TRIGGER trg_sealed_rollup_no_lone_delete
BEFORE DELETE ON sealed_rollup
WHEN EXISTS (SELECT 1 FROM sealed_report WHERE id = OLD.report_id)
BEGIN
    SELECT RAISE(ABORT, 'sealed_rollup is append-only: delete its sealed period instead (#913)');
END;
CREATE TRIGGER trg_sealed_person_no_lone_delete
BEFORE DELETE ON sealed_person
WHEN EXISTS (SELECT 1 FROM sealed_report WHERE id = OLD.report_id)
BEGIN
    SELECT RAISE(ABORT, 'sealed_person is append-only: tombstone, or delete its sealed period (#914)');
END;
CREATE TRIGGER trg_sealed_report_delete_children
AFTER DELETE ON sealed_report
BEGIN
    DELETE FROM sealed_rollup WHERE report_id = OLD.id;
    DELETE FROM sealed_person WHERE report_id = OLD.id;
END;
CREATE TRIGGER trg_sealed_gap_no_update
BEFORE UPDATE ON sealed_gap
BEGIN
    SELECT RAISE(ABORT, 'sealed_gap is append-only: UPDATE is refused (#913)');
END;
CREATE TRIGGER trg_sealed_gap_no_delete
BEFORE DELETE ON sealed_gap
BEGIN
    SELECT RAISE(ABORT, 'sealed_gap is append-only: DELETE is refused (#913)');
END;
CREATE TRIGGER trg_sealed_gap_no_replace
BEFORE INSERT ON sealed_gap
WHEN EXISTS (SELECT 1 FROM sealed_gap WHERE rowid = NEW.rowid
             OR (period_size = NEW.period_size AND period_start = NEW.period_start))
BEGIN
    SELECT RAISE(ABORT, 'sealed_gap is append-only: a row with that key exists (#913)');
END;
CREATE TRIGGER trg_sealed_gap_not_over_seal
BEFORE INSERT ON sealed_gap
WHEN EXISTS (SELECT 1 FROM sealed_report WHERE period_start < NEW.period_end AND period_end > NEW.period_start)
BEGIN
    SELECT RAISE(ABORT, 'sealed_gap: that period is sealed (#913)');
END;
CREATE TRIGGER trg_sealed_report_not_over_gap
BEFORE INSERT ON sealed_report
WHEN EXISTS (SELECT 1 FROM sealed_gap WHERE period_start < NEW.period_end AND period_end > NEW.period_start)
BEGIN
    SELECT RAISE(ABORT, 'sealed_report: that period is recorded as a gap and is never sealed (#913)');
END;
CREATE TRIGGER trg_canonical_id_history_end_only
BEFORE UPDATE ON canonical_id_history
WHEN NOT (OLD.ended_seal IS NULL AND NEW.ended_seal IS NOT NULL
          AND NEW.rowid = OLD.rowid
          AND NEW.alias = OLD.alias
          AND NEW.canonical = OLD.canonical
          AND NEW.since_seal IS OLD.since_seal)
BEGIN
    SELECT RAISE(ABORT, 'canonical_id_history is append-only: only ending an active link is permitted (#975)');
END;
CREATE TRIGGER trg_canonical_id_history_no_replace
BEFORE INSERT ON canonical_id_history
WHEN NEW.ended_seal IS NOT NULL
  OR EXISTS (SELECT 1 FROM canonical_id_history WHERE rowid = NEW.rowid
             OR (alias = NEW.alias AND ended_seal IS NULL))
BEGIN
    SELECT RAISE(ABORT, 'canonical_id_history is append-only: insert one active link per alias (#975)');
END;
CREATE TRIGGER trg_u4_correction_declarations_INSERT AFTER INSERT ON correction_declarations WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
 UPDATE correction_policy SET rule_version='unresolved' WHERE id=1 AND scoring_enabled=1; END;
CREATE TRIGGER trg_u4_correction_declarations_UPDATE AFTER UPDATE ON correction_declarations WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
 UPDATE correction_policy SET rule_version='unresolved' WHERE id=1 AND scoring_enabled=1; END;
CREATE TRIGGER trg_u4_correction_declarations_DELETE AFTER DELETE ON correction_declarations WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
 UPDATE correction_policy SET rule_version='unresolved' WHERE id=1 AND scoring_enabled=1; END;
CREATE TRIGGER trg_u4_corrective_relations_INSERT AFTER INSERT ON corrective_relations WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
 UPDATE correction_policy SET rule_version='unresolved' WHERE id=1 AND scoring_enabled=1; END;
CREATE TRIGGER trg_u4_corrective_relations_UPDATE AFTER UPDATE ON corrective_relations WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
 UPDATE correction_policy SET rule_version='unresolved' WHERE id=1 AND scoring_enabled=1; END;
CREATE TRIGGER trg_u4_corrective_relations_DELETE AFTER DELETE ON corrective_relations WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
 UPDATE correction_policy SET rule_version='unresolved' WHERE id=1 AND scoring_enabled=1; END;
CREATE TRIGGER trg_u4_correction_assessments_INSERT AFTER INSERT ON correction_assessments WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
 UPDATE correction_policy SET rule_version='unresolved' WHERE id=1 AND scoring_enabled=1 AND NEW.basis='reviewed'; END;
CREATE TRIGGER trg_u4_correction_assessments_UPDATE AFTER UPDATE ON correction_assessments WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
 UPDATE correction_policy SET rule_version='unresolved' WHERE id=1 AND scoring_enabled=1 AND NEW.basis='reviewed'; END;
CREATE TRIGGER trg_u4_correction_assessments_DELETE AFTER DELETE ON correction_assessments WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
 UPDATE correction_policy SET rule_version='unresolved' WHERE id=1 AND scoring_enabled=1 AND OLD.basis='reviewed'; END;
CREATE TRIGGER trg_u4_correction_allocations_INSERT AFTER INSERT ON correction_allocations WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
  END;
CREATE TRIGGER trg_u4_correction_allocations_UPDATE AFTER UPDATE ON correction_allocations WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
  END;
CREATE TRIGGER trg_u4_correction_allocations_DELETE AFTER DELETE ON correction_allocations WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
  END;
CREATE TRIGGER trg_u4_outcomes_INSERT AFTER INSERT ON outcomes WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
 UPDATE correction_policy SET rule_version='unresolved' WHERE id=1 AND scoring_enabled=1; END;
CREATE TRIGGER trg_u4_outcomes_UPDATE AFTER UPDATE ON outcomes WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
 UPDATE correction_policy SET rule_version='unresolved' WHERE id=1 AND scoring_enabled=1; END;
CREATE TRIGGER trg_u4_outcomes_DELETE AFTER DELETE ON outcomes WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
 UPDATE correction_policy SET rule_version='unresolved' WHERE id=1 AND scoring_enabled=1; END;
CREATE TRIGGER trg_u4_developer_alias_INSERT AFTER INSERT ON developer_alias WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
 UPDATE correction_policy SET rule_version='unresolved' WHERE id=1 AND scoring_enabled=1; END;
CREATE TRIGGER trg_u4_developer_alias_UPDATE AFTER UPDATE ON developer_alias WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
 UPDATE correction_policy SET rule_version='unresolved' WHERE id=1 AND scoring_enabled=1; END;
CREATE TRIGGER trg_u4_developer_alias_DELETE AFTER DELETE ON developer_alias WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 BEGIN
 INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1;
 UPDATE correction_policy SET rule_version='unresolved' WHERE id=1 AND scoring_enabled=1; END;
CREATE TRIGGER trg_u4_policy_epoch AFTER UPDATE ON correction_policy
 WHEN (OLD.scoring_enabled=1 OR NEW.scoring_enabled=1) AND (OLD.scoring_enabled IS NOT NEW.scoring_enabled OR OLD.rubric_version IS NOT NEW.rubric_version OR OLD.rule_version IS NOT NEW.rule_version OR OLD.projection_revision IS NOT NEW.projection_revision)
 BEGIN INSERT INTO erase_epoch(id,n) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET n=n+1; END;
CREATE TRIGGER trg_u4_seal_policy BEFORE INSERT ON sealed_report
 WHEN (SELECT scoring_enabled FROM correction_policy WHERE id=1)=1 AND
 (NEW.scoring_policy IS NOT 'event3' OR NEW.rubric_version IS NOT 2 OR NEW.correction_rule_version IS NOT 'event3-v2'
 OR NEW.correction_revision IS NOT (SELECT projection_revision FROM correction_policy WHERE id=1)
 OR (SELECT rule_version FROM correction_policy WHERE id=1) IS NOT 'event3-v2')
 BEGIN SELECT RAISE(ABORT,'Event 3 seal requires resolved pinned policy'); END;
CREATE UNIQUE INDEX idx_developer_alias_alias
    ON developer_alias(alias);
CREATE INDEX idx_token_events_issue     ON token_events(issue_id);
CREATE INDEX idx_token_events_ts_id ON token_events(ts, id);
CREATE INDEX idx_token_events_source_ts ON token_events(source, ts);
CREATE INDEX idx_outcomes_developer     ON outcomes(developer);
CREATE INDEX idx_outcomes_issue         ON outcomes(issue_id);
CREATE INDEX idx_outcomes_ts_id         ON outcomes(ts, id);
CREATE INDEX idx_org_hierarchy_org      ON org_hierarchy(org);
CREATE INDEX idx_period_membership_org  ON period_membership(org);
CREATE UNIQUE INDEX idx_period_membership_open
    ON period_membership(developer, org) WHERE period_end IS NULL;
CREATE INDEX idx_period_membership_developer
    ON period_membership(developer);
CREATE UNIQUE INDEX idx_hierarchy_membership_open
    ON hierarchy_membership(developer) WHERE valid_to IS NULL;
CREATE INDEX idx_hierarchy_membership_developer
    ON hierarchy_membership(developer, valid_from);
CREATE INDEX idx_webhook_payloads_event_delivery
    ON webhook_payloads(event, delivery_id);
CREATE INDEX idx_webhook_payloads_received ON webhook_payloads(received_at);
CREATE UNIQUE INDEX idx_quality_events_uq
    ON quality_events(outcome_id, event_type, source_ref);
CREATE INDEX idx_quality_events_ts_id
    ON quality_events(event_ts, id);
CREATE INDEX idx_quality_events_developer
    ON quality_events(developer);
CREATE INDEX idx_quality_history_outcome ON quality_history(outcome_id);
CREATE INDEX idx_quality_history_ts_id
    ON quality_history(ts, id);
CREATE INDEX idx_quality_history_developer
    ON quality_history(developer);
CREATE INDEX idx_repo_repair_audit_developer
    ON repo_repair_audit(developer);
CREATE UNIQUE INDEX idx_push_outcome_commits_repo_sha
    ON push_outcome_commits(repo, commit_sha);
CREATE INDEX idx_push_outcome_commits_outcome
    ON push_outcome_commits(outcome_id, ts, commit_sha);
CREATE INDEX idx_push_outcome_commits_developer
    ON push_outcome_commits(developer);
CREATE INDEX idx_push_outcome_commits_sha
    ON push_outcome_commits(commit_sha);
CREATE INDEX idx_push_outcome_audit_developer
    ON push_outcome_audit(developer);
CREATE UNIQUE INDEX idx_price_table_registry_live_version
		     ON price_table_registry(version) WHERE forgotten_at IS NULL;
CREATE INDEX idx_outcomes_repo_pr_number ON outcomes(repo, pr_number);
CREATE INDEX idx_token_events_repo_branch_ts ON token_events(repo, branch_hash, ts);
CREATE INDEX idx_outcomes_repo_branch_merge ON outcomes(repo, head_branch_hash, merged_at);
CREATE UNIQUE INDEX correction_declaration_identity ON correction_declarations(repo,pr_number,merge_commit_sha)
 WHERE repo IS NOT NULL AND pr_number IS NOT NULL AND merge_commit_sha IS NOT NULL;
CREATE INDEX correction_declaration_outcome ON correction_declarations(outcome_id);
CREATE INDEX correction_declaration_developer ON correction_declarations(developer);
CREATE INDEX correction_declaration_recorded ON correction_declarations(recorded_at,id);
CREATE UNIQUE INDEX correction_relation_edge ON corrective_relations(child_repo,child_pr_number,child_merge_commit_sha,parent_repo,parent_pr_number)
 WHERE child_repo IS NOT NULL AND child_pr_number IS NOT NULL AND child_merge_commit_sha IS NOT NULL AND parent_repo IS NOT NULL AND parent_pr_number IS NOT NULL;
CREATE INDEX correction_relation_parent_endpoint ON corrective_relations(parent_repo,parent_pr_number);
CREATE INDEX correction_relation_child_outcome ON corrective_relations(child_outcome_id);
CREATE INDEX correction_relation_parent_outcome ON corrective_relations(parent_outcome_id);
CREATE INDEX correction_relation_child_developer ON corrective_relations(child_developer);
CREATE INDEX correction_relation_parent_developer ON corrective_relations(parent_developer);
CREATE INDEX correction_relation_state ON corrective_relations(state,recorded_at,id);
CREATE UNIQUE INDEX correction_audit_action ON correction_audit(relation_id,revision,action);
CREATE INDEX correction_audit_relation ON correction_audit(relation_id,id);
CREATE INDEX idx_pricing_observation_event ON pricing_observation_audit(token_event_id,kind);
CREATE INDEX idx_pricing_observation_subject ON pricing_observation_audit(developer,token_event_id);
CREATE UNIQUE INDEX idx_token_events_idempotency
    ON token_events(idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX idx_token_events_scores
    ON token_events(developer, ts, cost_micro, fidelity);
CREATE UNIQUE INDEX idx_outcomes_merge_commit_sha_uq
    ON outcomes(merge_commit_sha) WHERE merge_commit_sha IS NOT NULL;
CREATE UNIQUE INDEX idx_outcomes_push_daily_repo
    ON outcomes(repo, issue_id, push_day) WHERE source = 'push';
CREATE INDEX idx_outcomes_author_bot
    ON outcomes(developer) WHERE author_type = 'Bot';
CREATE INDEX idx_actual_spend_dev_period_nu
    ON actual_spend(developer, period);
CREATE INDEX idx_org_actual_spend_org_period_nu
    ON org_actual_spend(org, period);
CREATE INDEX idx_outcome_exclusions_sha ON outcome_exclusions(exclusion_merge_commit_sha);
CREATE INDEX idx_outcome_exclusions_developer ON outcome_exclusions(developer);
CREATE INDEX idx_outcome_exclusions_author_developer ON outcome_exclusions(author_developer);
CREATE UNIQUE INDEX correction_assessment_review_revision ON correction_assessments(relation_id,revision,participant_outcome_id)
 WHERE basis='reviewed' AND relation_id IS NOT NULL AND participant_outcome_id IS NOT NULL;
CREATE UNIQUE INDEX correction_assessment_projection_version ON correction_assessments(chain_version,participant_outcome_id)
 WHERE basis='provisional' AND participant_outcome_id IS NOT NULL;
CREATE INDEX correction_assessment_outcome ON correction_assessments(participant_outcome_id);
CREATE INDEX correction_assessment_reviewer ON correction_assessments(reviewer);
CREATE INDEX correction_assessment_chain ON correction_assessments(chain_version);
CREATE UNIQUE INDEX correction_allocation_outcome ON correction_allocations(outcome_id) WHERE outcome_id IS NOT NULL;
CREATE INDEX correction_allocation_chain ON correction_allocations(chain_version);
CREATE INDEX correction_allocation_relation ON correction_allocations(relation_id);
CREATE UNIQUE INDEX idx_sealed_report_period
    ON sealed_report(period_size, period_start);
CREATE UNIQUE INDEX idx_sealed_gap_period
    ON sealed_gap(period_size, period_start);
CREATE UNIQUE INDEX idx_canonical_id_history_active
    ON canonical_id_history(alias) WHERE ended_seal IS NULL;
COMMIT;

-- The schema version is stored in the SQLite header, outside .dump.
PRAGMA user_version = 11;
