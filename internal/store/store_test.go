package store

import (
	"context"
	"database/sql"
	"errors"
	"github.com/tiermetric/tier/internal/repoid"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) (*DB, func()) {
	t.Helper()
	f, err := os.CreateTemp("", "tier-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	db, err := Open(f.Name())
	if err != nil {
		_ = os.Remove(f.Name())
		t.Fatal(err)
	}
	return db, func() {
		_ = db.Close()
		_ = os.Remove(f.Name())
	}
}

func TestInsertAndQueryTokenEvent(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	ev := TokenEvent{
		Developer:    "alice",
		IssueID:      "issue-42",
		Model:        "claude-sonnet-4",
		InputTok:     1000,
		OutputTok:    500,
		CacheRead:    0,
		CacheWrite5m: 0,
		CacheWrite1h: 0,
		CostMicro:    10_500, // $0.0105
		Source:       "proxy",
		Fidelity:     "realtime",
		Timestamp:    now,
	}
	if err := db.InsertTokenEvent(ctx, ev); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}

	costs, err := db.DeveloperCosts(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	if len(costs) != 1 {
		t.Fatalf("expected 1 developer cost, got %d", len(costs))
	}
	if costs[0].Developer != "alice" {
		t.Errorf("developer = %q, want alice", costs[0].Developer)
	}
	if costs[0].TotalCostMicro != 10_500 {
		t.Errorf("total cost = %v, want 10_500 micro ($0.0105)", costs[0].TotalCostMicro)
	}
	if costs[0].RealtimeCostMicro != 10_500 {
		t.Errorf("realtime cost = %v, want 10_500 micro ($0.0105)", costs[0].RealtimeCostMicro)
	}
}

// TestOpen_MigratesPreIssue6Schema simulates an upgrade from the pre-#6 schema
// (no idempotency_key column, no partial unique index) by manually creating a
// database with the legacy shape and then calling Open(). The migration must
// add the column and the index without losing existing rows, and the new
// idempotent insert path must work after migration.
func TestOpen_MigratesPreIssue6Schema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	// Create the legacy schema directly — no idempotency_key column.
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE token_events (
		    id          INTEGER PRIMARY KEY AUTOINCREMENT,
		    developer   TEXT    NOT NULL,
		    issue_id    TEXT    NOT NULL,
		    model       TEXT    NOT NULL,
		    input_tok   INTEGER NOT NULL DEFAULT 0,
		    output_tok  INTEGER NOT NULL DEFAULT 0,
		    cache_read  INTEGER NOT NULL DEFAULT 0,
		    cache_write INTEGER NOT NULL DEFAULT 0,
		    cost_usd    REAL    NOT NULL,
		    source      TEXT    NOT NULL,
		    fidelity    TEXT    NOT NULL,
		    ts          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO token_events
		    (developer, issue_id, model, input_tok, output_tok, cache_read, cache_write, cost_usd, source, fidelity)
		VALUES ('alice', 'issue-1', 'claude-sonnet-4', 100, 50, 0, 0, 0.001, 'jsonl', 'realtime');
	`)
	if err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	_ = legacy.Close()

	// Re-open via the real Open() — should migrate cleanly without losing the row.
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open on legacy DB: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Pre-migration row is preserved.
	costs, err := db.DeveloperCosts(context.Background(), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	if len(costs) != 1 || costs[0].Developer != "alice" {
		t.Fatalf("expected pre-migration row preserved, got %+v", costs)
	}

	// Idempotent insert path works after migration.
	ev := TokenEvent{
		Developer: "alice", IssueID: "issue-2", Model: "claude-sonnet-4",
		InputTok: 200, OutputTok: 100, CostMicro: 2_000, // $0.002
		Source: "jsonl", Fidelity: "realtime",
		IdempotencyKey: "post-migration-key",
		Timestamp:      time.Now().UTC(),
	}
	if err := db.InsertTokenEvent(context.Background(), ev); err != nil {
		t.Fatalf("post-migration insert: %v", err)
	}
	if err := db.InsertTokenEvent(context.Background(), ev); err != nil {
		t.Fatalf("post-migration duplicate insert (expected no-op): %v", err)
	}
}

// TestOpen_MigratesCacheWriteSplit exercises the issue #55 schema migration:
// a pre-#55 DB with a single cache_write column gets ADD COLUMN'd into
// cache_write_5m + cache_write_1h, the legacy values backfill into the 5m
// bucket, the old column is dropped, and cost_usd is recomputed under the
// TTL-aware multipliers.
func TestOpen_MigratesCacheWriteSplit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pre55.db")

	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Pre-#55 schema — has cache_write, idempotency_key, no TTL split.
	_, err = legacy.Exec(`
		CREATE TABLE token_events (
		    id              INTEGER PRIMARY KEY AUTOINCREMENT,
		    developer       TEXT    NOT NULL,
		    issue_id        TEXT    NOT NULL,
		    model           TEXT    NOT NULL,
		    input_tok       INTEGER NOT NULL DEFAULT 0,
		    output_tok      INTEGER NOT NULL DEFAULT 0,
		    cache_read      INTEGER NOT NULL DEFAULT 0,
		    cache_write     INTEGER NOT NULL DEFAULT 0,
		    cost_usd        REAL    NOT NULL,
		    source          TEXT    NOT NULL,
		    fidelity        TEXT    NOT NULL,
		    idempotency_key TEXT,
		    ts              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		-- Row 1: JSONL source, 1M cache_write. Old cost_usd = $15 (flat 1.0x
		-- on cache_write at the legacy $15/M opus rate). Post-migration: 5m
		-- bucket, recomputed at the corrected $5/M Opus 4.7 rate (#80) to
		-- $6.25 (1M * $5/M * 1.25).
		INSERT INTO token_events (developer, issue_id, model, input_tok, output_tok,
		    cache_read, cache_write, cost_usd, source, fidelity)
		VALUES ('alice', 'issue-1', 'claude-opus-4-7', 0, 0, 0, 1000000, 15.0, 'jsonl', 'realtime');
		-- Row 2: api source, manually posted cost. Must NOT be recomputed.
		INSERT INTO token_events (developer, issue_id, model, input_tok, output_tok,
		    cache_read, cache_write, cost_usd, source, fidelity)
		VALUES ('bob', 'issue-2', 'claude-sonnet-4', 100, 50, 0, 200, 99.99, 'api', 'realtime');
		-- Row 3: JSONL source, cache_write = 0. The WHERE cache_write > 0
		-- guard in the backfill must NOT touch cache_write_5m; cost recompute
		-- still proceeds because the row is in a known source.
		INSERT INTO token_events (developer, issue_id, model, input_tok, output_tok,
		    cache_read, cache_write, cost_usd, source, fidelity)
		VALUES ('carol', 'issue-3', 'claude-sonnet-4', 100, 50, 0, 0, 1.0, 'jsonl', 'realtime');
		-- Row 4: JSONL source, retired/unknown model. Recompute uses the
		-- self-hosted-medium fallback ($0.50/M combined). 1M cache_write at
		-- the fallback rate = $0.50; documents that an unknown model in a
		-- known source DOES recompute (at the fallback rate, with the WARN
		-- log fired exactly once per process via warnUnknownModel).
		INSERT INTO token_events (developer, issue_id, model, input_tok, output_tok,
		    cache_read, cache_write, cost_usd, source, fidelity)
		VALUES ('dave', 'issue-4', 'definitely-not-a-real-model', 0, 0, 0, 1000000, 999.99, 'jsonl', 'realtime');
	`)
	if err != nil {
		t.Fatalf("create pre-#55 schema: %v", err)
	}
	_ = legacy.Close()

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open on pre-#55 DB: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Legacy column must be gone.
	if exists, err := columnExists(db.db, "token_events", "cache_write"); err != nil {
		t.Fatalf("columnExists: %v", err)
	} else if exists {
		t.Errorf("legacy cache_write column should be dropped after migration")
	}
	// New columns must be present.
	for _, col := range []string{"cache_write_5m", "cache_write_1h"} {
		exists, err := columnExists(db.db, "token_events", col)
		if err != nil {
			t.Fatalf("columnExists(%s): %v", col, err)
		}
		if !exists {
			t.Errorf("expected new column %s to exist after migration", col)
		}
	}

	// Row 1 (jsonl): cache_write_5m = backfilled 1_000_000, cache_write_1h = 0,
	// cost_usd recomputed to 1M * $5/M * 1.25 = $6.25 (corrected Opus 4.7 rate, #80).
	var w5, w1 int
	var cost int64 // cost_micro (#69)
	err = db.db.QueryRow(
		`SELECT cache_write_5m, cache_write_1h, cost_micro FROM token_events WHERE developer = 'alice'`,
	).Scan(&w5, &w1, &cost)
	if err != nil {
		t.Fatalf("query alice: %v", err)
	}
	if w5 != 1_000_000 || w1 != 0 {
		t.Errorf("alice: cache_write_5m=%d cache_write_1h=%d, want 1_000_000/0", w5, w1)
	}
	if cost != 6_250_000 {
		t.Errorf("alice cost_micro = %d, want 6_250_000 ($6.25, recomputed at 1.25x for 5m, $5/M Opus 4.7)", cost)
	}

	// Row 2 (api): cache_write_5m = backfilled 200, cache_write_1h = 0, but
	// cost_usd MUST be preserved at the originally-posted 99.99 (manual posts
	// are authoritative — recomputeKnownSourceCosts excludes source='api').
	err = db.db.QueryRow(
		`SELECT cache_write_5m, cache_write_1h, cost_micro FROM token_events WHERE developer = 'bob'`,
	).Scan(&w5, &w1, &cost)
	if err != nil {
		t.Fatalf("query bob: %v", err)
	}
	if w5 != 200 || w1 != 0 {
		t.Errorf("bob: cache_write_5m=%d cache_write_1h=%d, want 200/0", w5, w1)
	}
	// Migrated from cost_usd=99.99 REAL → 99_990_000 micro; source=api so
	// recomputeKnownSourceCosts must leave it untouched.
	if cost != 99_990_000 {
		t.Errorf("bob cost_micro = %d, want 99_990_000 ($99.99 preserved; source=api not recomputed)", cost)
	}

	// Row 3 (carol, cache_write=0): cache_write_5m stays 0, cache_write_1h
	// stays 0, cost_usd recomputed against the new ComputeCost (still uses
	// claude-sonnet-4 at $3/M input + $15/M output for 100 input + 50 output
	// = 0.0003 + 0.00075 = $0.00105).
	err = db.db.QueryRow(
		`SELECT cache_write_5m, cache_write_1h, cost_micro FROM token_events WHERE developer = 'carol'`,
	).Scan(&w5, &w1, &cost)
	if err != nil {
		t.Fatalf("query carol: %v", err)
	}
	if w5 != 0 || w1 != 0 {
		t.Errorf("carol: cache_write_5m=%d cache_write_1h=%d, want 0/0 (no backfill needed)", w5, w1)
	}
	if cost != 1_050 {
		t.Errorf("carol cost_micro = %d, want 1_050 ($0.00105, recomputed from input+output)", cost)
	}

	// Row 4 (dave, unknown model): backfill puts cache_write into 5m;
	// recompute uses the self-hosted-medium fallback ($0.50/M combined),
	// 1M cache_write_5m = $0.50. Documents the fallback behavior explicitly
	// so a future change is forced to surface the intent.
	err = db.db.QueryRow(
		`SELECT cache_write_5m, cache_write_1h, cost_micro FROM token_events WHERE developer = 'dave'`,
	).Scan(&w5, &w1, &cost)
	if err != nil {
		t.Fatalf("query dave: %v", err)
	}
	if w5 != 1_000_000 || w1 != 0 {
		t.Errorf("dave: cache_write_5m=%d cache_write_1h=%d, want 1_000_000/0", w5, w1)
	}
	if cost != 500_000 {
		t.Errorf("dave cost_micro = %d, want 500_000 ($0.50, unknown-model fallback rate)", cost)
	}

	// Idempotency check: re-running Open() must be a no-op (no DROP attempted,
	// no re-recompute changing values). With the migration-marker gate the
	// recompute SELECT shouldn't even fire on the second boot.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open on already-migrated DB: %v", err)
	}
	defer func() { _ = db2.Close() }()
	err = db2.db.QueryRow(
		`SELECT cost_micro FROM token_events WHERE developer = 'alice'`,
	).Scan(&cost)
	if err != nil {
		t.Fatalf("re-query alice: %v", err)
	}
	if cost != 6_250_000 {
		t.Errorf("after re-Open: alice cost_micro = %d, want 6_250_000 ($6.25, idempotent recompute)", cost)
	}
	// Marker row must be present after the first successful recompute.
	var markerName string
	err = db2.db.QueryRow(
		`SELECT name FROM tier_migrations WHERE name = ?`, migrationRecomputeCacheTTL,
	).Scan(&markerName)
	if err != nil {
		t.Errorf("tier_migrations marker for cache-TTL recompute not found: %v", err)
	}
}

// TestOpen_MigratesCostUSDToMicro_RoundsFractional pins migrateCostUSDToMicro
// (#69): a pre-#69 DB carrying cost_usd REAL is converted to cost_micro INTEGER
// via CAST(ROUND(cost_usd * 1e6) AS INTEGER). The row is source='api' so
// recomputeKnownSourceCosts does NOT overwrite it — isolating the migration's
// own ROUND conversion. ROUND is round-half-away-from-zero (SQLite), so a
// fractional micro value rounds to the NEAREST micro-dollar rather than
// truncating. The legacy cost_usd column must also be gone afterwards.
func TestOpen_MigratesCostUSDToMicro_RoundsFractional(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pre69-cost.db")

	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Pre-#69 token_events: cost_usd REAL. cost_usd=0.123456789 → micro =
	// ROUND(123456.789) = 123457 (rounds up to nearest, not truncated to 123456).
	if _, err = legacy.Exec(`
		CREATE TABLE token_events (
		    id              INTEGER PRIMARY KEY AUTOINCREMENT,
		    developer       TEXT    NOT NULL,
		    issue_id        TEXT    NOT NULL,
		    model           TEXT    NOT NULL,
		    input_tok       INTEGER NOT NULL DEFAULT 0,
		    output_tok      INTEGER NOT NULL DEFAULT 0,
		    cache_read      INTEGER NOT NULL DEFAULT 0,
		    cache_write_5m  INTEGER NOT NULL DEFAULT 0,
		    cache_write_1h  INTEGER NOT NULL DEFAULT 0,
		    cost_usd        REAL    NOT NULL,
		    source          TEXT    NOT NULL,
		    fidelity        TEXT    NOT NULL,
		    idempotency_key TEXT,
		    ts              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO token_events (developer, issue_id, model, cost_usd, source, fidelity)
		VALUES ('alice', 'issue-1', 'claude-sonnet-4', 0.123456789, 'api', 'estimated');
	`); err != nil {
		t.Fatalf("create pre-#69 schema: %v", err)
	}
	_ = legacy.Close()

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open on pre-#69 DB: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Legacy cost_usd column must be gone (DROP COLUMN ran).
	if exists, err := columnExists(db.db, "token_events", "cost_usd"); err != nil {
		t.Fatalf("columnExists: %v", err)
	} else if exists {
		t.Errorf("legacy cost_usd column should be dropped after #69 migration")
	}

	var costMicro int64
	if err := db.db.QueryRow(
		`SELECT cost_micro FROM token_events WHERE developer = 'alice'`,
	).Scan(&costMicro); err != nil {
		t.Fatalf("read cost_micro: %v", err)
	}
	if costMicro != 123457 {
		t.Errorf("cost_micro = %d, want 123457 (ROUND(0.123456789 * 1e6), nearest not truncated)", costMicro)
	}
}

// TestInsertTokenEvent_IdempotentOnRepeatKey covers the cross-source dedup
// guarantee added in issue #6: inserting the same event twice with the same
// non-empty IdempotencyKey results in exactly one row, with no error returned.
func TestInsertTokenEvent_IdempotentOnRepeatKey(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	ev := TokenEvent{
		Developer:      "alice",
		IssueID:        "issue-42",
		Model:          "claude-sonnet-4",
		InputTok:       1000,
		OutputTok:      500,
		CostMicro:      10_500, // $0.0105
		Source:         "jsonl",
		Fidelity:       "realtime",
		IdempotencyKey: "deadbeef-stable-key",
		Timestamp:      now,
	}
	if err := db.InsertTokenEvent(ctx, ev); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Second insert with same key should be a silent no-op.
	if err := db.InsertTokenEvent(ctx, ev); err != nil {
		t.Fatalf("second insert (expected silent no-op): %v", err)
	}

	costs, err := db.DeveloperCosts(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	if len(costs) != 1 {
		t.Fatalf("expected 1 developer cost row, got %d", len(costs))
	}
	if costs[0].TotalCostMicro != 10_500 {
		t.Errorf("total cost = %v, want 10_500 micro ($0.0105, idempotent re-insert; MAX of equal values)", costs[0].TotalCostMicro)
	}
}

// TestOutcomeByMergeCommit covers the lookup added in #20: the revert
// detector parses "This reverts commit <sha>" out of a revert commit and
// resolves the original outcome via merge_commit_sha.
func TestOutcomeByMergeCommit(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	// Seed an outcome with a known merge_commit_sha.
	want := Outcome{
		Developer:      "bob",
		IssueID:        "issue-7",
		PRNumber:       42,
		Weight:         5,
		Quality:        1.0,
		MergeCommitSHA: "abcdef0123456789abcdef0123456789abcdef01",
		Timestamp:      now,
	}
	if _, err := db.InsertOutcome(ctx, want); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}

	// Hit: exact-match SHA → found.
	got, found, err := db.OutcomeByMergeCommit(ctx, want.MergeCommitSHA)
	if err != nil {
		t.Fatalf("OutcomeByMergeCommit: %v", err)
	}
	if !found {
		t.Fatal("expected to find the seeded outcome")
	}
	if got.IssueID != want.IssueID || got.Developer != want.Developer {
		t.Errorf("got %+v, want issue=%q developer=%q", got, want.IssueID, want.Developer)
	}

	// Miss: unrelated SHA → not found, no error.
	_, found, err = db.OutcomeByMergeCommit(ctx, "0000000000000000000000000000000000000000")
	if err != nil {
		t.Errorf("unrelated SHA returned error: %v", err)
	}
	if found {
		t.Error("expected found=false for unrelated SHA")
	}

	// Empty SHA: short-circuits without a query.
	_, found, err = db.OutcomeByMergeCommit(ctx, "")
	if err != nil || found {
		t.Errorf("empty SHA: got err=%v found=%v, want err=nil found=false", err, found)
	}
}

// TestInsertOutcome_EmptyMergeCommitSHA: outcomes from sources that don't
// know the merge commit (older webhook payloads, manual inserts) store as
// SQL NULL via NULLIF, and the partial index excludes them — so the lookup
// path can't accidentally hit a NULL=NULL pseudo-match.
func TestInsertOutcome_EmptyMergeCommitSHA(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "alice", IssueID: "issue-1", Weight: 1, Quality: 1.0,
		Timestamp: time.Now().UTC(),
		// MergeCommitSHA intentionally empty.
	}); err != nil {
		t.Fatalf("InsertOutcome with empty SHA: %v", err)
	}

	// Looking up by empty SHA must not return that row.
	_, found, err := db.OutcomeByMergeCommit(ctx, "")
	if err != nil {
		t.Errorf("empty SHA returned error (should short-circuit cleanly): %v", err)
	}
	if found {
		t.Error("empty SHA must never return a row, even when matching rows exist with NULL")
	}
}

// TestInsertOutcome_WeightProvenanceRoundTrip: weight_source and the raw diff
// stats survive an insert->read cycle across every read path (#132), so reports
// can distinguish a label-derived weight from a heuristic one and a future
// recalibration can re-score new rows.
func TestInsertOutcome_WeightProvenanceRoundTrip(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	want := Outcome{
		Developer: "carol", IssueID: "issue-132", PRNumber: 5,
		Weight: 3, WeightSource: WeightSourceHeuristic, Quality: 1.0,
		MergeCommitSHA: "1111111111111111111111111111111111111111",
		Additions:      200, Deletions: 40, ChangedFiles: 6,
		Timestamp: time.Now().UTC(),
	}
	if _, err := db.InsertOutcome(ctx, want); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}

	check := func(name string, got Outcome, ok bool, err error) {
		t.Helper()
		if err != nil || !ok {
			t.Fatalf("%s: err=%v ok=%v", name, err, ok)
		}
		if got.WeightSource != WeightSourceHeuristic {
			t.Errorf("%s: WeightSource = %q, want %q", name, got.WeightSource, WeightSourceHeuristic)
		}
		if got.Additions != 200 || got.Deletions != 40 || got.ChangedFiles != 6 {
			t.Errorf("%s: diff stats = (%d,%d,%d), want (200,40,6)", name, got.Additions, got.Deletions, got.ChangedFiles)
		}
	}

	g1, ok1, e1 := db.LatestOutcomeByIssue(ctx, "", "issue-132")
	check("LatestOutcomeByIssue", g1, ok1, e1)
	g2, ok2, e2 := db.OutcomeByMergeCommit(ctx, want.MergeCommitSHA)
	check("OutcomeByMergeCommit", g2, ok2, e2)

	list, err := db.DeveloperOutcomes(ctx, "carol", want.Timestamp.Add(-time.Hour))
	if err != nil || len(list) != 1 {
		t.Fatalf("DeveloperOutcomes: err=%v n=%d", err, len(list))
	}
	check("DeveloperOutcomes", list[0], true, nil)
}

// TestOutcome_LegacyRowDefaults: a pre-#132 row (raw insert omitting the new
// columns) reads back with weight_source coalesced to 'legacy' and zeroed diff
// stats. The old heuristic discarded its inputs, so such rows are never
// re-scored — this pins that they classify cleanly rather than error.
func TestOutcome_LegacyRowDefaults(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// Insert bypassing InsertOutcome so the new columns take their schema
	// DEFAULT/NULL, exactly as a row written before #132 would.
	if _, err := db.db.ExecContext(ctx,
		`INSERT INTO outcomes (developer, issue_id, pr_number, weight, quality, ts)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"dave", "issue-legacy", 9, 5.0, 1.0, time.Now().UTC(),
	); err != nil {
		t.Fatalf("raw legacy insert: %v", err)
	}

	got, ok, err := db.LatestOutcomeByIssue(ctx, "", "issue-legacy")
	if err != nil || !ok {
		t.Fatalf("LatestOutcomeByIssue: err=%v ok=%v", err, ok)
	}
	if got.WeightSource != WeightSourceLegacy {
		t.Errorf("WeightSource = %q, want %q", got.WeightSource, WeightSourceLegacy)
	}
	if got.Additions != 0 || got.Deletions != 0 || got.ChangedFiles != 0 {
		t.Errorf("legacy diff stats = (%d,%d,%d), want all 0", got.Additions, got.Deletions, got.ChangedFiles)
	}
}

// TestInsertTokenEvent_DedupsAcrossSources is the end-to-end regression test
// for #19. Before the fix, a Claude call captured by both the JSONL collector
// (source=jsonl) and the proxy (source=proxy) produced distinct
// source-prefixed IdempotencyKeys and landed twice. After the fix, both paths
// emit the same MessageIdempotencyKey, so a single row reaches the index and
// the second insert is a no-op (MAX-on-conflict over equal values).
func TestInsertTokenEvent_DedupsAcrossSources(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Both events describe the SAME upstream Anthropic message msg_X.
	// In production they would come from the JSONL collector and the proxy;
	// here we construct them directly to keep the test scope at the store
	// layer.
	const sharedKey = "shared-cross-source-key"
	jsonlEvent := TokenEvent{
		Developer: "alice", IssueID: "issue-42", Model: "claude-sonnet-4",
		InputTok: 1000, OutputTok: 500, CostMicro: 10_500, // $0.0105
		Source: "jsonl", Fidelity: "realtime",
		IdempotencyKey: sharedKey, Timestamp: now,
	}
	proxyEvent := jsonlEvent
	proxyEvent.Source = "proxy"

	if err := db.InsertTokenEvent(ctx, jsonlEvent); err != nil {
		t.Fatalf("JSONL insert: %v", err)
	}
	if err := db.InsertTokenEvent(ctx, proxyEvent); err != nil {
		t.Fatalf("proxy insert (should be silent no-op): %v", err)
	}

	var count int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM token_events WHERE idempotency_key = ?`, sharedKey,
	).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("cross-source dedup failed: %d rows for shared key, want 1", count)
	}

	costs, _ := db.DeveloperCosts(ctx, now.Add(-time.Hour))
	if len(costs) != 1 || costs[0].TotalCostMicro != 10_500 {
		t.Errorf("total cost = %v, want 10_500 micro ($0.0105, single dedupped row)", costs)
	}
}

// TestInsertTokenEvent_TakesMaxOnConflict pins the per-field ON CONFLICT semantics.
// TOKEN COUNTS take MAX: the placeholder-promotion edge (an out-of-order partial
// flush overtaken by the definitive per-message entry) evolves the row toward the
// larger totals rather than freezing at a partial. cost_micro, however, is IMMUTABLE
// on conflict as of #233 — a replay must never re-price an already-priced row (the
// old cost = MAX silently ratcheted stored history upward across binary/table
// versions). Under the current per-message keying (#19) a replay carries identical
// totals anyway, so this exercises the defensive edge, not the steady state.
func TestInsertTokenEvent_TakesMaxOnConflict(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	key := "session-key-grows"

	first := TokenEvent{
		Developer: "alice", IssueID: "issue-42", Model: "claude-sonnet-4",
		InputTok: 1000, OutputTok: 500,
		CacheRead: 100, CacheWrite5m: 30, CacheWrite1h: 20, CostMicro: 10_500, // $0.0105
		Source: "jsonl", Fidelity: "realtime",
		IdempotencyKey: key, Timestamp: now,
	}
	if err := db.InsertTokenEvent(ctx, first); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Same key, larger totals (placeholder promotion) priced HIGHER. Token counts
	// must MAX up; cost must stay frozen at the first-writer's price (#233).
	second := first
	second.InputTok = 5000
	second.OutputTok = 2500
	second.CacheRead = 800
	second.CacheWrite5m = 150
	second.CacheWrite1h = 50
	second.CostMicro = 42_000 // $0.0420 — a higher-priced table; must NOT take effect
	second.Timestamp = now.Add(time.Minute)
	if err := db.InsertTokenEvent(ctx, second); err != nil {
		t.Fatalf("second insert: %v", err)
	}

	costs, _ := db.DeveloperCosts(ctx, now.Add(-time.Hour))
	if len(costs) != 1 {
		t.Fatalf("expected 1 row, got %d (key must dedup)", len(costs))
	}
	if costs[0].TotalCostMicro != 10_500 {
		t.Errorf("cost after higher-priced replay = %v, want 10_500 micro immutable (#233: no silent reprice)", costs[0].TotalCostMicro)
	}
	// Token counts still take MAX for the placeholder edge.
	events, _, err := db.ListTokenEvents(ctx, now.Add(-time.Hour), now.Add(time.Hour), PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].InputTok != 5000 || events[0].OutputTok != 2500 || events[0].CacheRead != 800 {
		t.Errorf("token counts = in %d/out %d/read %d, want 5000/2500/800 (counts still MAX)",
			events[0].InputTok, events[0].OutputTok, events[0].CacheRead)
	}

	// A smaller-totals replay must neither shrink the token counts (MAX holds) nor
	// disturb the frozen cost.
	third := first // smaller than second
	if err := db.InsertTokenEvent(ctx, third); err != nil {
		t.Fatalf("third insert: %v", err)
	}
	costs, _ = db.DeveloperCosts(ctx, now.Add(-time.Hour))
	if costs[0].TotalCostMicro != 10_500 {
		t.Errorf("cost after backward-going insert = %v, want 10_500 micro (immutable)", costs[0].TotalCostMicro)
	}
}

// TestInsertTokenEvent_EmptyKeyAllowsDuplicates ensures the partial unique
// index is partial: legacy or unkeyed events (empty IdempotencyKey) can be
// inserted multiple times without colliding. This preserves backwards
// compatibility for the proxy/manual paths that don't yet compute a key.
func TestInsertTokenEvent_EmptyKeyAllowsDuplicates(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	ev := TokenEvent{
		Developer: "bob",
		IssueID:   "issue-7",
		Model:     "claude-sonnet-4",
		InputTok:  500,
		OutputTok: 100,
		CostMicro: 5_000, // $0.005
		Source:    "proxy",
		Fidelity:  "realtime",
		// IdempotencyKey: "" — intentionally empty
		Timestamp: now,
	}
	for i := range 3 {
		if err := db.InsertTokenEvent(ctx, ev); err != nil {
			t.Fatalf("insert #%d: %v", i, err)
		}
	}

	costs, err := db.DeveloperCosts(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	if len(costs) != 1 {
		t.Fatalf("expected 1 developer cost row, got %d", len(costs))
	}
	wantTotal := int64(15_000) // three rows × 5_000 micro ($0.005)
	if costs[0].TotalCostMicro != wantTotal {
		t.Errorf("total cost = %v, want %v micro (empty keys must not collide)", costs[0].TotalCostMicro, wantTotal)
	}
}

// TestInsertTokenEvents_BulkIdempotent verifies the bulk path honors the same
// ON CONFLICT semantics — re-inserting an already-stored batch is a no-op.
func TestInsertTokenEvents_BulkIdempotent(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	batch := []TokenEvent{
		{
			Developer: "alice", IssueID: "issue-1", Model: "claude-sonnet-4",
			InputTok: 100, OutputTok: 50, CostMicro: 1_000, // $0.001
			Source: "jsonl", Fidelity: "realtime",
			IdempotencyKey: "k1", Timestamp: now,
		},
		{
			Developer: "alice", IssueID: "issue-2", Model: "claude-sonnet-4",
			InputTok: 200, OutputTok: 100, CostMicro: 2_000, // $0.002
			Source: "jsonl", Fidelity: "realtime",
			IdempotencyKey: "k2", Timestamp: now,
		},
	}
	if err := db.InsertTokenEvents(ctx, batch); err != nil {
		t.Fatalf("first bulk insert: %v", err)
	}
	if err := db.InsertTokenEvents(ctx, batch); err != nil {
		t.Fatalf("second bulk insert (expected no-op): %v", err)
	}

	costs, _ := db.DeveloperCosts(ctx, now.Add(-time.Hour))
	if len(costs) != 1 {
		t.Fatalf("expected 1 developer row, got %d", len(costs))
	}
	if costs[0].TotalCostMicro != 3_000 {
		t.Errorf("total cost = %v, want 3_000 micro ($0.003, bulk dedup)", costs[0].TotalCostMicro)
	}
}

func TestInsertAndQueryOutcome(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	o := Outcome{
		Developer: "bob",
		IssueID:   "issue-7",
		PRNumber:  99,
		Weight:    5.0,
		Quality:   1.0,
		Timestamp: now,
	}
	if _, err := db.InsertOutcome(ctx, o); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}

	outcomes, err := db.DeveloperOutcomes(ctx, "bob", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("DeveloperOutcomes: %v", err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("expected 1 outcome, got %d", len(outcomes))
	}
	if outcomes[0].Weight != 5.0 {
		t.Errorf("weight = %v, want 5.0", outcomes[0].Weight)
	}

	// Update quality (revert detection).
	if err := db.UpdateQuality(ctx, "bob", "issue-7", 0.5); err != nil {
		t.Fatalf("UpdateQuality: %v", err)
	}
	outcomes, _ = db.DeveloperOutcomes(ctx, "bob", now.Add(-time.Hour))
	if outcomes[0].Quality != 0.5 {
		t.Errorf("quality after revert = %v, want 0.5", outcomes[0].Quality)
	}
}

// TestActualSpend_InsertAndQuery covers the full lifecycle of the actual_spend
// ledger that powers Spend Leverage: insert per-developer per-month values,
// upsert on collision (finance corrections), and the two query shapes the
// /scores API uses (one developer, all developers).
func TestActualSpend_InsertAndQuery(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	// Insert: alice paid 400 for May, 450 for June. Bob paid 200 for May.
	for _, a := range []ActualSpend{
		{Developer: "alice", Period: "2026-05", ActualPaidMicro: 400 * MicroPerUSD, Timestamp: now},
		{Developer: "alice", Period: "2026-06", ActualPaidMicro: 450 * MicroPerUSD, Timestamp: now},
		{Developer: "bob", Period: "2026-05", ActualPaidMicro: 200 * MicroPerUSD, Timestamp: now},
	} {
		if err := db.InsertActualSpend(ctx, a); err != nil {
			t.Fatalf("InsertActualSpend(%+v): %v", a, err)
		}
	}

	// Per-developer query with since at start of May → both alice rows summed.
	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	got, err := db.ActualSpendForDeveloper(ctx, "alice", since)
	if err != nil {
		t.Fatalf("ActualSpendForDeveloper: %v", err)
	}
	if got != 850 {
		t.Errorf("alice total since May = %v, want 850", got)
	}

	// Bulk query for the same window.
	all, err := db.ActualSpendAll(ctx, since)
	if err != nil {
		t.Fatalf("ActualSpendAll: %v", err)
	}
	if all["alice"] != 850 || all["bob"] != 200 {
		t.Errorf("ActualSpendAll = %v, want alice=850 bob=200", all)
	}

	// since past June → only June counted for alice.
	since = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	got, _ = db.ActualSpendForDeveloper(ctx, "alice", since)
	if got != 450 {
		t.Errorf("alice total since June = %v, want 450", got)
	}
}

// TestActualSpend_MonthBoundary locks in the documented approximation:
// since-times mid-month still pull the entire month, since periods are stored
// as YYYY-MM strings. This is the right tradeoff for monthly enterprise
// billing — pro-rating within a month is deferred — but the behaviour must
// be exercised so a future change can't quietly break it.
func TestActualSpend_MonthBoundary(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	if err := db.InsertActualSpend(ctx, ActualSpend{
		Developer: "alice", Period: "2026-05", ActualPaidMicro: 400 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// since = 2026-05-15 mid-month → pulls the entire May row (approximation).
	since := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	got, _ := db.ActualSpendForDeveloper(ctx, "alice", since)
	if got != 400 {
		t.Errorf("since 2026-05-15 total = %v, want 400 (full-month approximation)", got)
	}

	// since = 2026-06-01 → May no longer overlaps; result is 0.
	since = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	got, _ = db.ActualSpendForDeveloper(ctx, "alice", since)
	if got != 0 {
		t.Errorf("since 2026-06-01 total = %v, want 0", got)
	}
}

// TestOpen_MigratesPreIssue24NegativeCheck verifies the rebuild path: a DB
// created with the pre-#24 schema (CHECK (actual_paid_usd >= 0)) is
// migrated on next Open() to drop the CHECK, preserving existing data.
// Without this migration, existing dev DBs would reject every credit memo
// post.
func TestOpen_MigratesPreIssue24NegativeCheck(t *testing.T) {
	f, err := os.CreateTemp("", "tier-mig-*.db")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	path := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(path) }()

	// Pre-create the table with the OLD schema (with CHECK).
	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := rawDB.Exec(`
		CREATE TABLE actual_spend (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			developer       TEXT    NOT NULL,
			period          TEXT    NOT NULL CHECK (period GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]'),
			actual_paid_usd REAL    NOT NULL CHECK (actual_paid_usd >= 0),
			ts              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`); err != nil {
		t.Fatalf("pre-create old schema: %v", err)
	}
	if _, err := rawDB.Exec(
		`INSERT INTO actual_spend (developer, period, actual_paid_usd, ts) VALUES (?, ?, ?, ?)`,
		"alice", "2026-04", 400.0, time.Now().UTC(),
	); err != nil {
		t.Fatalf("seed old row: %v", err)
	}
	_ = rawDB.Close()

	// Now open via the public path — migration runs.
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Existing data must be preserved AND converted to micro-dollars (#69): the
	// pre-#24 actual_paid_usd REAL column is dropped by migrateActualSpendToMicro,
	// which runs after the CHECK-drop rebuild, so read the new actual_paid_micro.
	var totalMicro int64
	if err := db.db.QueryRowContext(context.Background(),
		`SELECT COALESCE(SUM(actual_paid_micro), 0) FROM actual_spend WHERE developer = ?`,
		"alice",
	).Scan(&totalMicro); err != nil {
		t.Fatalf("read preserved row: %v", err)
	}
	if totalMicro != 400*MicroPerUSD {
		t.Errorf("post-migration total = %v micro, want %v ($400 preserved + converted)", totalMicro, 400*MicroPerUSD)
	}

	// And the CHECK must be gone — a negative credit memo posts cleanly.
	if err := db.InsertActualSpend(context.Background(), ActualSpend{
		Developer: "alice", Period: "2026-04", ActualPaidMicro: -50 * MicroPerUSD, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Errorf("post-migration negative insert: %v (CHECK should be gone)", err)
	}

	// The non-unique index must exist on the rebuilt table. The rebuild
	// migration drops the source table along with its indexes; without the
	// schemaPostMigration re-create step, query performance silently
	// degrades to full scans on upgraded DBs.
	var idxCount int
	if err := db.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = 'actual_spend' AND name = ?`,
		"idx_actual_spend_dev_period_nu",
	).Scan(&idxCount); err != nil {
		t.Fatalf("index count: %v", err)
	}
	if idxCount != 1 {
		t.Errorf("expected idx_actual_spend_dev_period_nu on rebuilt table, got %d", idxCount)
	}
}

// TestActualSpend_AccumulatesAcrossInserts is the #24 baseline: multiple
// inserts for the same (developer, period) accumulate rather than
// overwriting. Finance posts deltas — a $500 invoice + a $-100 credit memo
// nets to $400. This replaces the pre-#24 upsert test.
func TestActualSpend_AccumulatesAcrossInserts(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	// Initial invoice.
	if err := db.InsertActualSpend(ctx, ActualSpend{
		Developer: "alice", Period: "2026-05", ActualPaidMicro: 500 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("invoice insert: %v", err)
	}
	// Credit memo (negative delta).
	if err := db.InsertActualSpend(ctx, ActualSpend{
		Developer: "alice", Period: "2026-05", ActualPaidMicro: -100 * MicroPerUSD, Timestamp: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("credit memo insert: %v", err)
	}

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	got, _ := db.ActualSpendForDeveloper(ctx, "alice", since)
	if got != 400 {
		t.Errorf("net = %v, want 400 ($500 invoice + $-100 credit)", got)
	}

	// Both rows must exist — the audit trail lives in row history under #24.
	var rowCount int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM actual_spend WHERE developer = ? AND period = ?`,
		"alice", "2026-05",
	).Scan(&rowCount); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if rowCount != 2 {
		t.Errorf("rowCount = %d, want 2 (accumulating model preserves audit trail)", rowCount)
	}
}

// TestActualSpend_NegativeNet covers the over-credited case: when the credit
// memos exceed the invoice, the net is negative. The store stores it
// truthfully; downstream scoring renders SpendLeverage as 0 (dashboard "—").
func TestActualSpend_NegativeNet(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	if err := db.InsertActualSpend(ctx, ActualSpend{
		Developer: "alice", Period: "2026-05", ActualPaidMicro: 100 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("invoice: %v", err)
	}
	// Two credit memos that exceed the invoice.
	if err := db.InsertActualSpend(ctx, ActualSpend{
		Developer: "alice", Period: "2026-05", ActualPaidMicro: -80 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("credit 1: %v", err)
	}
	if err := db.InsertActualSpend(ctx, ActualSpend{
		Developer: "alice", Period: "2026-05", ActualPaidMicro: -50 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("credit 2: %v", err)
	}

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	got, _ := db.ActualSpendForDeveloper(ctx, "alice", since)
	if got != -30 {
		t.Errorf("net = %v, want -30 ($100 - $80 - $50)", got)
	}
}

// TestActualSpend_OrgFallback_PerDeveloperOnly is the baseline: when per-
// developer rows exist, the org fallback never engages. Confirms that
// adding the org table doesn't disturb the existing tier-1 behaviour.
func TestActualSpend_OrgFallback_PerDeveloperOnly(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	// alice has a per-developer row for May.
	if err := db.InsertActualSpend(ctx, ActualSpend{
		Developer: "alice", Period: "2026-05", ActualPaidMicro: 400 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertActualSpend: %v", err)
	}

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	got, err := db.ActualSpendForDeveloper(ctx, "alice", since)
	if err != nil {
		t.Fatalf("ActualSpendForDeveloper: %v", err)
	}
	if got != 400 {
		t.Errorf("got %v, want 400 (per-developer tier should match)", got)
	}
}

// TestActualSpend_OrgFallback_OrgOnly covers the headline #23 case: finance
// posts ONE org-level invoice; the system divides by the seat count in
// org_hierarchy to produce a per-developer slice.
func TestActualSpend_OrgFallback_OrgOnly(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	// 4 developers in org "acme".
	for _, dev := range []string{"alice", "bob", "carol", "dave"} {
		if err := db.UpsertHierarchy(ctx, dev, "platform", "", "acme"); err != nil {
			t.Fatalf("UpsertHierarchy(%s): %v", dev, err)
		}
	}
	// $4000 org-level invoice for May.
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "acme", Period: "2026-05", ActualPaidMicro: 4000 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend: %v", err)
	}

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	got, err := db.ActualSpendForDeveloper(ctx, "alice", since)
	if err != nil {
		t.Fatalf("ActualSpendForDeveloper: %v", err)
	}
	if got != 1000 {
		t.Errorf("alice slice = %v, want 1000 ($4000 / 4 seats)", got)
	}

	// Bulk query mirrors the per-call result for every developer in the org.
	all, err := db.ActualSpendAll(ctx, since)
	if err != nil {
		t.Fatalf("ActualSpendAll: %v", err)
	}
	for _, dev := range []string{"alice", "bob", "carol", "dave"} {
		if all[dev] != 1000 {
			t.Errorf("ActualSpendAll[%s] = %v, want 1000", dev, all[dev])
		}
	}
}

// TestActualSpend_OrgFallback_MixedTierReconciles (#40): when an org has BOTH a
// per-developer (tier-1) row AND an org-level (tier-2) invoice in the same
// period, the per-developer row is the dev's allocation AND the org-fallback
// members split the REMAINDER (org_total − tier-1 sum), so the slices reconcile
// to org_total rather than leaving the pre-#40 gap. alice's $300 is precise; bob
// absorbs $2000 − $300 = $1700; together they equal the $2000 org invoice.
func TestActualSpend_OrgFallback_MixedTierReconciles(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	for _, dev := range []string{"alice", "bob"} {
		if err := db.UpsertHierarchy(ctx, dev, "platform", "", "acme"); err != nil {
			t.Fatalf("UpsertHierarchy(%s): %v", dev, err)
		}
	}
	if err := db.InsertActualSpend(ctx, ActualSpend{
		Developer: "alice", Period: "2026-05", ActualPaidMicro: 300 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertActualSpend: %v", err)
	}
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "acme", Period: "2026-05", ActualPaidMicro: 2000 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend: %v", err)
	}

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	gotAlice, _ := db.ActualSpendForDeveloper(ctx, "alice", since)
	if gotAlice != 300 {
		t.Errorf("alice = %v, want 300 (precise per-developer invoice)", gotAlice)
	}
	// bob (org-fallback) absorbs the remainder after alice's tier-1: $2000-$300.
	gotBob, _ := db.ActualSpendForDeveloper(ctx, "bob", since)
	if gotBob != 1700 {
		t.Errorf("bob = %v, want 1700 (($2000 - $300 tier-1) / 1 remaining seat)", gotBob)
	}

	// Accounting identity: tier-1 + org-fallback reconcile to org_total ($2000).
	all, _ := db.ActualSpendAll(ctx, since)
	if all["alice"] != 300 || all["bob"] != 1700 {
		t.Errorf("ActualSpendAll = %v, want alice=300 bob=1700", all)
	}
	if all["alice"]+all["bob"] != 2000 {
		t.Errorf("allocations sum to %v, want 2000 (= org_total; #40 gap closed)", all["alice"]+all["bob"])
	}
}

// TestActualSpend_OrgFallback_NonEvenSplitReconciles guards the #69 REAL-cast in
// the org-fallback remainder division. With actual_paid_micro now an INTEGER
// column, the split (org_total − tier1) / seats would be TRUNCATING integer
// division without the CAST(... AS REAL) — losing up to ~1 micro-dollar per seat,
// so the allocated slices would no longer reconcile to org_total (the #40
// identity). An evenly-divisible invoice (every other org-fallback test) cannot
// catch a dropped CAST; this one uses $1000 across 7 seats ($142.857…/seat) so
// integer truncation would leave the sum ~$0.000006 short of $1000.
func TestActualSpend_OrgFallback_NonEvenSplitReconciles(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	const seats = 7
	devs := []string{"d1", "d2", "d3", "d4", "d5", "d6", "d7"}
	for _, dev := range devs {
		if err := db.UpsertHierarchy(ctx, dev, "platform", "", "acme"); err != nil {
			t.Fatalf("UpsertHierarchy(%s): %v", dev, err)
		}
	}
	// One org invoice, no per-developer tier-1 rows → all seats split it.
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "acme", Period: "2026-05", ActualPaidMicro: 1000 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend: %v", err)
	}

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	all, err := db.ActualSpendAll(ctx, since)
	if err != nil {
		t.Fatalf("ActualSpendAll: %v", err)
	}

	want := 1000.0 / seats
	var sum float64
	for _, dev := range devs {
		if math.Abs(all[dev]-want) > 1e-4 {
			t.Errorf("%s = %v, want ~%.6f (fractional remainder share)", dev, all[dev], want)
		}
		sum += all[dev]
	}
	// The key guard: the fractional shares must reconcile to org_total. Integer
	// (CAST-less) division would leave sum ≈ $999.999994 — off by ~6e-6. A 1e-6
	// tolerance passes the REAL division (~1e-9 error) and fails truncation.
	if math.Abs(sum-1000.0) > 1e-6 {
		t.Errorf("org-fallback shares sum to %.9f, want 1000 within 1e-6 — CAST(... AS REAL) likely dropped, division truncated to integer micro-dollars", sum)
	}
}

// mustInsertSpend inserts a tier-1 per-developer actual_spend row or fails.
func mustInsertSpend(t *testing.T, db *DB, ctx context.Context, dev, period string, paid float64, ts time.Time) {
	t.Helper()
	if err := db.InsertActualSpend(ctx, ActualSpend{
		Developer: dev, Period: period, ActualPaidMicro: DollarsToMicro(paid), Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertActualSpend(%s,%s): %v", dev, period, err)
	}
}

// mustInsertOrgSpend inserts a tier-2 org_actual_spend row or fails.
func mustInsertOrgSpend(t *testing.T, db *DB, ctx context.Context, org, period string, paid float64, ts time.Time) {
	t.Helper()
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: org, Period: period, ActualPaidMicro: DollarsToMicro(paid), Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend(%s,%s): %v", org, period, err)
	}
}

// seedOrgMember directly inserts an org_hierarchy row + an open membership so a
// test can set up org-fallback seats without UpsertHierarchy's at-now start.
// NOTE: the allocation queries count seats from period_membership ONLY — the
// org_hierarchy row exists for production-shape parity, not for seat counting;
// seedMembership (the open '2026-01' row) is what these tests actually consume.
func seedOrgMember(t *testing.T, db *DB, dev, org string) {
	t.Helper()
	if _, err := db.db.Exec(
		`INSERT INTO org_hierarchy (developer, team, division, org) VALUES (?, 'platform', '', ?)`, dev, org,
	); err != nil {
		t.Fatalf("seed org_hierarchy(%s): %v", dev, err)
	}
	seedMembership(t, db, dev, org, "2026-01", "")
}

// TestActualSpend_OrgFallback_TierEdges pins the two defensive #40 branches:
// (A) all active members are tier-1 → no org-fallback recipients (NULLIF→NULL),
// each keeps their tier-1, sum == org_total; (B) tier-1 sum exceeds org_total →
// the org-fallback share clamps to 0 (never negative).
func TestActualSpend_OrgFallback_TierEdges(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Org A: both members tier-1 ($800 + $1200), org invoice $2000.
	seedOrgMember(t, db, "alice", "orgA")
	seedOrgMember(t, db, "bob", "orgA")
	mustInsertSpend(t, db, ctx, "alice", "2026-05", 800, now)
	mustInsertSpend(t, db, ctx, "bob", "2026-05", 1200, now)
	mustInsertOrgSpend(t, db, ctx, "orgA", "2026-05", 2000, now)

	// Org B: alice tier-1 $2500 (> org_total), bob org-fallback. carol also a seat.
	seedOrgMember(t, db, "cara", "orgB")
	seedOrgMember(t, db, "dan", "orgB")
	mustInsertSpend(t, db, ctx, "cara", "2026-05", 2500, now)
	mustInsertOrgSpend(t, db, ctx, "orgB", "2026-05", 2000, now)

	all, err := db.ActualSpendAll(ctx, since)
	if err != nil {
		t.Fatalf("ActualSpendAll: %v", err)
	}
	// A: no fallback recipients; tier-1 amounts stand and sum to org_total.
	if all["alice"] != 800 || all["bob"] != 1200 {
		t.Errorf("orgA all-tier-1 = alice %v bob %v, want 800/1200", all["alice"], all["bob"])
	}
	// B: cara keeps $2500 tier-1; dan's fallback share clamps to 0 (not negative).
	if all["cara"] != 2500 {
		t.Errorf("cara = %v, want 2500 (tier-1)", all["cara"])
	}
	// dan must be PRESENT with a 0 slice — a clamped fallback seat still emits a
	// row (Part B SUMs one $0 period); a missing key would be a silent drop.
	if dan, ok := all["dan"]; !ok || dan != 0 {
		t.Errorf("dan = %v (present=%v), want present and 0 (clamped: tier-1 sum > org_total)", dan, ok)
	}
	if got, _ := db.ActualSpendForDeveloper(ctx, "dan", since); got != 0 {
		t.Errorf("ActualSpendForDeveloper(dan) = %v, want 0 (clamp)", got)
	}
}

// TestActualSpend_OrgFallback_MixedTierMultiPeriod pins the per-period claim: a
// developer who is tier-1 in one period and org-fallback in another is handled
// correctly in each, and each period reconciles to its org_total.
func TestActualSpend_OrgFallback_MixedTierMultiPeriod(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seedOrgMember(t, db, "alice", "acme")
	seedOrgMember(t, db, "bob", "acme")
	// 2026-03: alice tier-1 $300; org $1000 → bob org-fallback ($1000-$300)/1 = $700.
	mustInsertSpend(t, db, ctx, "alice", "2026-03", 300, now)
	mustInsertOrgSpend(t, db, ctx, "acme", "2026-03", 1000, now)
	// 2026-05: no tier-1; org $1000 → 2 seats × $500.
	mustInsertOrgSpend(t, db, ctx, "acme", "2026-05", 1000, now)

	all, _ := db.ActualSpendAll(ctx, since)
	// alice: $300 (tier-1, Mar) + $500 (fallback, May) = $800.
	// bob:   $700 (fallback, Mar) + $500 (fallback, May) = $1200.
	if all["alice"] != 800 || all["bob"] != 1200 {
		t.Errorf("multi-period mixed = alice %v bob %v, want 800/1200", all["alice"], all["bob"])
	}
	if all["alice"]+all["bob"] != 2000 {
		t.Errorf("sum = %v, want 2000 (= $1000 + $1000 org_totals)", all["alice"]+all["bob"])
	}
}

// seedMembership inserts a period_membership row directly (white-box) so tests
// can express historical/closed memberships that UpsertHierarchy (which only
// opens an at-now membership) cannot. periodEnd "" stores NULL (still active).
func seedMembership(t *testing.T, db *DB, developer, org, periodStart, periodEnd string) {
	t.Helper()
	var end any
	if periodEnd != "" {
		end = periodEnd
	}
	if _, err := db.db.Exec(
		`INSERT INTO period_membership (developer, org, period_start, period_end) VALUES (?, ?, ?, ?)`,
		developer, org, periodStart, end,
	); err != nil {
		t.Fatalf("seedMembership(%s): %v", developer, err)
	}
}

// TestActualSpend_OrgFallback_DepartedMemberExcluded is the #41 headline: a
// developer who left the org before the queried window no longer counts as a
// seat, so the org_total divides across only the active members — and the
// departed developer receives no slice (the per-developer allocations sum back
// to org_total).
func TestActualSpend_OrgFallback_DepartedMemberExcluded(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	// 4 developers in org_hierarchy, but dave departed at the end of 2026-03.
	for _, dev := range []string{"alice", "bob", "carol", "dave"} {
		if _, err := db.db.Exec(
			`INSERT INTO org_hierarchy (developer, team, division, org) VALUES (?, 'platform', '', 'acme')`, dev,
		); err != nil {
			t.Fatalf("seed org_hierarchy(%s): %v", dev, err)
		}
	}
	seedMembership(t, db, "alice", "acme", "2026-01", "")
	seedMembership(t, db, "bob", "acme", "2026-01", "")
	seedMembership(t, db, "carol", "acme", "2026-01", "")
	seedMembership(t, db, "dave", "acme", "2026-01", "2026-03") // left before the window

	// $3000 org invoice for 2026-05.
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "acme", Period: "2026-05", ActualPaidMicro: 3000 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend: %v", err)
	}

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	// 3 active seats, not 4: $3000 / 3 = $1000 (the pre-#41 all-time count gave $750).
	got, err := db.ActualSpendForDeveloper(ctx, "alice", since)
	if err != nil {
		t.Fatalf("ActualSpendForDeveloper: %v", err)
	}
	if got != 1000 {
		t.Errorf("alice slice = %v, want 1000 ($3000 / 3 active seats; departed dave excluded)", got)
	}
	// The departed developer gets no allocation.
	gotDave, _ := db.ActualSpendForDeveloper(ctx, "dave", since)
	if gotDave != 0 {
		t.Errorf("dave slice = %v, want 0 (departed before the window)", gotDave)
	}
	// ActualSpendAll: only the 3 active members, each $1000 — summing to org_total.
	all, err := db.ActualSpendAll(ctx, since)
	if err != nil {
		t.Fatalf("ActualSpendAll: %v", err)
	}
	var total float64
	for _, dev := range []string{"alice", "bob", "carol"} {
		if all[dev] != 1000 {
			t.Errorf("ActualSpendAll[%s] = %v, want 1000", dev, all[dev])
		}
		total += all[dev]
	}
	if _, ok := all["dave"]; ok {
		t.Errorf("ActualSpendAll included departed dave (%v); want absent", all["dave"])
	}
	if total != 3000 {
		t.Errorf("active allocations sum to %v, want 3000 (= org_total, accounting identity holds)", total)
	}
}

// TestEndMembership_DropsSeat exercises the close-a-membership path end-to-end:
// after EndMembership marks a developer's last active period before the window,
// the remaining seats absorb the full org_total.
func TestEndMembership_DropsSeat(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	for _, dev := range []string{"alice", "bob", "carol"} {
		seedMembership(t, db, dev, "acme", "2026-01", "")
		if _, err := db.db.Exec(
			`INSERT INTO org_hierarchy (developer, team, division, org) VALUES (?, 'platform', '', 'acme')`, dev,
		); err != nil {
			t.Fatalf("seed org_hierarchy(%s): %v", dev, err)
		}
	}
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "acme", Period: "2026-05", ActualPaidMicro: 3000 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend: %v", err)
	}
	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	// Baseline: 3 active seats → $1000 each.
	if got, _ := db.ActualSpendForDeveloper(ctx, "alice", since); got != 1000 {
		t.Fatalf("baseline alice = %v, want 1000", got)
	}
	// carol leaves at end of 2026-04 (before the 2026-05 window).
	if err := db.EndMembership(ctx, "carol", "acme", "2026-04"); err != nil {
		t.Fatalf("EndMembership: %v", err)
	}
	if got, _ := db.ActualSpendForDeveloper(ctx, "alice", since); got != 1500 {
		t.Errorf("after carol departs, alice = %v, want 1500 ($3000 / 2 active seats)", got)
	}
}

// TestBackfillPeriodMembership covers the #41 migration: pre-existing
// org_hierarchy rows (with no membership) are seeded as active-since-'0000-01'
// open memberships, and the backfill is idempotent via its marker.
func TestBackfillPeriodMembership(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	// Simulate a pre-#41 DB: an org_hierarchy row with no membership, and the
	// backfill marker cleared so the migration re-runs.
	if _, err := db.db.Exec(
		`INSERT INTO org_hierarchy (developer, team, division, org) VALUES ('zoe', 'platform', '', 'acme')`,
	); err != nil {
		t.Fatalf("seed org_hierarchy: %v", err)
	}
	if _, err := db.db.Exec(`DELETE FROM tier_migrations WHERE name = ?`, migrationPeriodMembershipBackfill); err != nil {
		t.Fatalf("clear marker: %v", err)
	}

	if err := backfillPeriodMembership(db.db); err != nil {
		t.Fatalf("backfillPeriodMembership: %v", err)
	}
	var start string
	var end sql.NullString
	if err := db.db.QueryRow(
		`SELECT period_start, period_end FROM period_membership WHERE developer = 'zoe' AND org = 'acme'`,
	).Scan(&start, &end); err != nil {
		t.Fatalf("query backfilled membership: %v", err)
	}
	if start != "0000-01" || end.Valid {
		t.Errorf("backfilled membership = (%q, valid=%v), want ('0000-01', NULL)", start, end.Valid)
	}

	// Idempotent: a second run is a no-op (marker set) and adds no duplicate.
	if err := backfillPeriodMembership(db.db); err != nil {
		t.Fatalf("backfillPeriodMembership (2nd): %v", err)
	}
	var n int
	if err := db.db.QueryRow(
		`SELECT COUNT(*) FROM period_membership WHERE developer = 'zoe'`,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("after idempotent re-run, zoe membership rows = %d, want 1", n)
	}
}

// TestActualSpend_OrgFallback_PerPeriodSeatCount is the multi-period case: the
// seat count is computed PER invoice period, so a member who left mid-window is
// counted (and allocated) only in the periods they were active, the boundary
// period (period_end == invoice period) is inclusive, and each period's slices
// sum back to that period's org_total. A single range-wide seat count would
// mis-allocate here.
func TestActualSpend_OrgFallback_PerPeriodSeatCount(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	for _, dev := range []string{"alice", "bob", "carol"} {
		if _, err := db.db.Exec(
			`INSERT INTO org_hierarchy (developer, team, division, org) VALUES (?, 'platform', '', 'acme')`, dev,
		); err != nil {
			t.Fatalf("seed org_hierarchy(%s): %v", dev, err)
		}
	}
	seedMembership(t, db, "alice", "acme", "2026-01", "")      // active throughout
	seedMembership(t, db, "carol", "acme", "2026-01", "")      // active throughout
	seedMembership(t, db, "bob", "acme", "2026-01", "2026-03") // left at end of March (boundary)

	// Two invoiced periods.
	for _, inv := range []struct {
		period string
		amount float64
	}{{"2026-03", 600}, {"2026-05", 600}} {
		if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
			Org: "acme", Period: inv.period, ActualPaidMicro: DollarsToMicro(inv.amount), Timestamp: now,
		}); err != nil {
			t.Fatalf("InsertOrgActualSpend(%s): %v", inv.period, err)
		}
	}

	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// March: 3 active seats → $200 each (bob included via period_end == 2026-03).
	// May:   2 active seats → $300 each (bob excluded).
	// alice/carol = 200 + 300 = 500; bob = 200 (March only).
	for dev, want := range map[string]float64{"alice": 500, "carol": 500, "bob": 200} {
		got, err := db.ActualSpendForDeveloper(ctx, dev, since)
		if err != nil {
			t.Fatalf("ActualSpendForDeveloper(%s): %v", dev, err)
		}
		if got != want {
			t.Errorf("%s = %v, want %v (per-period seat count)", dev, got, want)
		}
	}
	// Identity: all slices sum to the two invoices' total ($1200).
	all, err := db.ActualSpendAll(ctx, since)
	if err != nil {
		t.Fatalf("ActualSpendAll: %v", err)
	}
	var total float64
	for _, v := range all {
		total += v
	}
	if total != 1200 {
		t.Errorf("allocations sum to %v, want 1200 (= org_total across both periods)", total)
	}
}

// TestUpsertHierarchy_OrgChange: moving a developer to a new org closes the old
// org's open membership (effective this month) and opens one for the new org,
// so the developer never has two simultaneously-open memberships.
func TestUpsertHierarchy_OrgChange(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	period := time.Now().UTC().Format("2006-01")

	if err := db.UpsertHierarchy(ctx, "alice", "platform", "", "acme"); err != nil {
		t.Fatalf("UpsertHierarchy(acme): %v", err)
	}
	if err := db.UpsertHierarchy(ctx, "alice", "platform", "", "globex"); err != nil {
		t.Fatalf("UpsertHierarchy(globex): %v", err)
	}

	// acme membership is closed effective this month; globex is open.
	var acmeEnd sql.NullString
	if err := db.db.QueryRow(
		`SELECT period_end FROM period_membership WHERE developer = 'alice' AND org = 'acme'`,
	).Scan(&acmeEnd); err != nil {
		t.Fatalf("query acme membership: %v", err)
	}
	if !acmeEnd.Valid || acmeEnd.String != period {
		t.Errorf("acme period_end = %v, want closed at %q", acmeEnd, period)
	}
	var openCount int
	if err := db.db.QueryRow(
		`SELECT COUNT(*) FROM period_membership WHERE developer = 'alice' AND period_end IS NULL`,
	).Scan(&openCount); err != nil {
		t.Fatalf("count open: %v", err)
	}
	if openCount != 1 {
		t.Errorf("alice has %d open memberships, want exactly 1 (globex)", openCount)
	}
}

// TestUpsertHierarchy_IdempotentMembership: repeated UpsertHierarchy for the
// same (developer, org) leaves exactly one open membership row.
func TestUpsertHierarchy_IdempotentMembership(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := db.UpsertHierarchy(ctx, "alice", "platform", "", "acme"); err != nil {
			t.Fatalf("UpsertHierarchy #%d: %v", i, err)
		}
	}
	var n int
	if err := db.db.QueryRow(
		`SELECT COUNT(*) FROM period_membership WHERE developer = 'alice' AND org = 'acme' AND period_end IS NULL`,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("open memberships = %d, want 1 (NOT-EXISTS guard)", n)
	}
}

// TestEndMembership_Validation: a malformed period is rejected, and closing a
// non-existent membership is a silent no-op.
func TestEndMembership_Validation(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if err := db.EndMembership(ctx, "alice", "acme", "2026-13"); err == nil {
		t.Errorf("EndMembership with bad period = nil, want error")
	}
	if err := db.EndMembership(ctx, "alice", "acme", "nope"); err == nil {
		t.Errorf("EndMembership with non-period = nil, want error")
	}
	// No open membership for ghost/acme → valid period, no row, no error.
	if err := db.EndMembership(ctx, "ghost", "acme", "2026-05"); err != nil {
		t.Errorf("EndMembership no-op returned %v, want nil", err)
	}
	// Non-canonical month (accepted by time.Parse, breaks period ordering) rejected.
	if err := db.EndMembership(ctx, "alice", "acme", "2026-5"); err == nil {
		t.Errorf("EndMembership with non-canonical period = nil, want error")
	}
}

// TestEndMembership_BeforeStartTyped: ending a membership at a period earlier
// than its period_start is incoherent input (#232). The store returns the typed
// ErrEndBeforeStart (matchable with errors.Is) rather than surfacing the column
// CHECK as an opaque driver error, so the API can map it to 400 not 500. The
// membership start is forced to the current month via an org change (first
// enrollment uses the '0000-01' sentinel, against which no valid period is
// earlier).
func TestEndMembership_BeforeStartTyped(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if err := db.UpsertHierarchy(ctx, "mover", "eng", "plat", "org1"); err != nil {
		t.Fatalf("UpsertHierarchy org1: %v", err)
	}
	if err := db.UpsertHierarchy(ctx, "mover", "eng", "plat", "org2"); err != nil {
		t.Fatalf("UpsertHierarchy org2: %v", err)
	}
	err := db.EndMembership(ctx, "mover", "org2", "2020-01")
	if !errors.Is(err, ErrEndBeforeStart) {
		t.Fatalf("EndMembership before start: err = %v, want ErrEndBeforeStart", err)
	}
	// A period at or after the start succeeds and the CHECK is never provoked.
	if err := db.EndMembership(ctx, "mover", "org2", "2026-12"); err != nil {
		t.Errorf("EndMembership at valid period returned %v, want nil", err)
	}
}

// TestActualSpend_OrgFallback_DeveloperNotInHierarchy: a developer with no
// org_hierarchy entry can't be allocated from the org pool — we don't know
// what org they belong to. Returns 0 rather than guessing.
func TestActualSpend_OrgFallback_DeveloperNotInHierarchy(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	// alice has a hierarchy entry; bob does not.
	if err := db.UpsertHierarchy(ctx, "alice", "platform", "", "acme"); err != nil {
		t.Fatalf("UpsertHierarchy: %v", err)
	}
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "acme", Period: "2026-05", ActualPaidMicro: 2000 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend: %v", err)
	}

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	gotBob, err := db.ActualSpendForDeveloper(ctx, "bob", since)
	if err != nil {
		t.Fatalf("ActualSpendForDeveloper: %v", err)
	}
	if gotBob != 0 {
		t.Errorf("bob (no hierarchy) = %v, want 0", gotBob)
	}
}

// TestActualSpend_OrgFallback_EmptySeatCount: an org that has invoice rows
// but no developers in org_hierarchy can't allocate (divide-by-zero
// hazard). The store returns 0 — the dashboard renders this developer's
// SpendLeverage as "—" rather than NaN/Inf.
func TestActualSpend_OrgFallback_EmptySeatCount(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	// Subtle setup: alice is in org "acme", but the OrgActualSpend is for
	// "ghost-org" — alice can't be allocated.
	if err := db.UpsertHierarchy(ctx, "alice", "platform", "", "acme"); err != nil {
		t.Fatalf("UpsertHierarchy: %v", err)
	}
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "ghost-org", Period: "2026-05", ActualPaidMicro: 2000 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend: %v", err)
	}

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	got, err := db.ActualSpendForDeveloper(ctx, "alice", since)
	if err != nil {
		t.Fatalf("ActualSpendForDeveloper: %v", err)
	}
	if got != 0 {
		t.Errorf("alice (in 'acme', org_spend for 'ghost-org') = %v, want 0", got)
	}
}

// TestOrgActualSpend_Accumulates: org-level invoices accumulate too. An
// initial $4000 contract bill + a $-500 credit memo nets to $3500 across
// the org's seats.
func TestOrgActualSpend_Accumulates(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "acme", Period: "2026-05", ActualPaidMicro: 4000 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("invoice: %v", err)
	}
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "acme", Period: "2026-05", ActualPaidMicro: -500 * MicroPerUSD, Timestamp: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("credit memo: %v", err)
	}

	// One developer → full net amount.
	if err := db.UpsertHierarchy(ctx, "alice", "platform", "", "acme"); err != nil {
		t.Fatalf("UpsertHierarchy: %v", err)
	}
	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	got, _ := db.ActualSpendForDeveloper(ctx, "alice", since)
	if got != 3500 {
		t.Errorf("net = %v, want 3500 ($4000 - $500 credit)", got)
	}

	// Both rows must exist — audit trail.
	var rowCount int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM org_actual_spend WHERE org = ? AND period = ?`,
		"acme", "2026-05",
	).Scan(&rowCount); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if rowCount != 2 {
		t.Errorf("rowCount = %d, want 2 (accumulating preserves audit trail)", rowCount)
	}
}

// TestOpen_PragmasApplyPerConnection pins the #63 fix: busy_timeout is
// connection-scoped in SQLite, so it must ride in the DSN (re-applied by
// modernc.org/sqlite on every new connection), not a one-shot Exec that
// silently vanishes when database/sql discards and recreates its pooled
// connection.
func TestOpen_PragmasApplyPerConnection(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "pragma.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = d.Close() }()

	readPragmas := func() (busy int, journal string) {
		t.Helper()
		if err := d.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil {
			t.Fatalf("read busy_timeout: %v", err)
		}
		if err := d.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
			t.Fatalf("read journal_mode: %v", err)
		}
		return busy, journal
	}

	busy, journal := readPragmas()
	if busy != 5000 {
		t.Errorf("busy_timeout on first connection = %d, want 5000", busy)
	}
	if journal != "wal" {
		t.Errorf("journal_mode on first connection = %q, want wal", journal)
	}

	// Force the pool to discard its connection and dial a fresh one — the
	// pre-#63 Exec-applied busy_timeout vanished exactly here.
	d.db.SetConnMaxLifetime(time.Nanosecond)
	time.Sleep(10 * time.Millisecond)
	d.db.SetConnMaxLifetime(0)

	busy, journal = readPragmas()
	if busy != 5000 {
		t.Errorf("busy_timeout after connection recreation = %d, want 5000 (#63 regression)", busy)
	}
	if journal != "wal" {
		t.Errorf("journal_mode after connection recreation = %q, want wal", journal)
	}
}

// TestOpen_MigratesDuplicateMergeSHAs simulates a pre-#60 database whose
// non-unique merge_commit_sha index tolerated duplicate outcome rows
// (webhook replays). Open() must drop the old index, dedup keeping the
// NEWEST row per SHA (matching OutcomeByMergeCommit's ORDER BY id DESC),
// create the unique partial index, and make further duplicate inserts
// silent no-ops.
func TestOpen_MigratesDuplicateMergeSHAs(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pre60.db")

	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE outcomes (
		    id                INTEGER PRIMARY KEY AUTOINCREMENT,
		    developer         TEXT    NOT NULL,
		    issue_id          TEXT    NOT NULL,
		    pr_number         INTEGER,
		    weight            REAL    NOT NULL,
		    quality           REAL    NOT NULL DEFAULT 1.0,
		    merge_commit_sha  TEXT,
		    ts                DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX idx_outcomes_merge_commit_sha
		    ON outcomes(merge_commit_sha) WHERE merge_commit_sha IS NOT NULL;
		INSERT INTO outcomes (developer, issue_id, pr_number, weight, quality, merge_commit_sha)
		VALUES ('alice', 'issue-1', 1, 3.0, 1.0, 'sha-dup'),
		       ('alice', 'issue-1', 1, 5.0, 0.5, 'sha-dup'),
		       ('bob',   'issue-2', 2, 1.0, 1.0, 'sha-solo'),
		       ('carol', 'issue-3', 3, 2.0, 1.0, NULL),
		       ('carol', 'issue-3', 4, 2.0, 1.0, NULL);
	`)
	if err != nil {
		t.Fatalf("seed legacy db: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open after legacy seed: %v", err)
	}
	defer func() { _ = d.Close() }()

	// Newest duplicate (weight 5.0, quality 0.5) survives; older is gone.
	o, found, err := d.OutcomeByMergeCommit(context.Background(), "sha-dup")
	if err != nil || !found {
		t.Fatalf("OutcomeByMergeCommit(sha-dup): found=%v err=%v", found, err)
	}
	if o.Weight != 5.0 || o.Quality != 0.5 {
		t.Errorf("surviving row = weight %.1f quality %.1f, want 5.0/0.5 (newest per SHA)", o.Weight, o.Quality)
	}
	var nonNullRows, nullRows int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM outcomes WHERE merge_commit_sha IS NOT NULL`).Scan(&nonNullRows); err != nil {
		t.Fatalf("count non-null: %v", err)
	}
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM outcomes WHERE merge_commit_sha IS NULL`).Scan(&nullRows); err != nil {
		t.Fatalf("count null: %v", err)
	}
	if nonNullRows != 2 {
		t.Errorf("non-null SHA rows = %d, want 2 (sha-dup deduped, sha-solo kept)", nonNullRows)
	}
	// NULL SHAs are outside the partial index — both survive.
	if nullRows != 2 {
		t.Errorf("NULL SHA rows = %d, want 2 (partial index must not affect them)", nullRows)
	}

	// Post-migration, a replayed insert with an existing SHA is a no-op.
	if _, err := d.InsertOutcome(context.Background(), Outcome{
		Developer: "mallory", IssueID: "issue-1", PRNumber: 1, Weight: 8.0, Quality: 1.0,
		MergeCommitSHA: "sha-dup", Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("replay insert: %v", err)
	}
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM outcomes WHERE merge_commit_sha = 'sha-dup'`).Scan(&nonNullRows); err != nil {
		t.Fatalf("count sha-dup: %v", err)
	}
	if nonNullRows != 1 {
		t.Errorf("sha-dup rows after replay insert = %d, want 1 (ON CONFLICT DO NOTHING)", nonNullRows)
	}
}

// TestTeamsForDevelopers covers the #94 item-3 bulk team lookup: an empty table
// yields a non-nil empty map; a populated table returns one entry per developer
// with the right team; a developer with an empty-string team is a present key
// (distinct from an absent developer); and a developer with no row is omitted
// (caller treats missing as "no team").
func TestTeamsForDevelopers(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// Empty table → non-nil empty map. The handler indexes the result without a
	// nil-check, so the non-nil contract matters.
	empty, err := db.TeamsForDevelopers(ctx)
	if err != nil {
		t.Fatalf("TeamsForDevelopers (empty): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty org_hierarchy: got %v, want non-nil empty map", empty)
	}

	for _, r := range []struct{ dev, team string }{
		{"alice", "platform"},
		{"bob", "platform"},
		{"cara", "payments"},
		{"dan", ""}, // empty-string team is a real stored value, distinct from absence
	} {
		if _, err := db.db.Exec(
			`INSERT INTO org_hierarchy (developer, team, division, org) VALUES (?, ?, '', 'acme')`,
			r.dev, r.team,
		); err != nil {
			t.Fatalf("seed org_hierarchy(%s): %v", r.dev, err)
		}
	}

	got, err := db.TeamsForDevelopers(ctx)
	if err != nil {
		t.Fatalf("TeamsForDevelopers: %v", err)
	}
	want := map[string]string{"alice": "platform", "bob": "platform", "cara": "payments", "dan": ""}
	if len(got) != len(want) {
		t.Fatalf("got %d entries (%v), want %d", len(got), got, len(want))
	}
	for dev, team := range want {
		if got[dev] != team {
			t.Errorf("team[%s] = %q, want %q", dev, got[dev], team)
		}
	}
	// dan (empty-string team) must be PRESENT; zoe (no row) must be ABSENT —
	// even though both index to "". The handler distinguishes them only by the
	// query value being non-empty, so this present/absent boundary matters.
	if _, ok := got["dan"]; !ok {
		t.Errorf("developer with empty-string team should be present in the map: %v", got)
	}
	if _, ok := got["zoe"]; ok {
		t.Errorf("absent developer zoe should not be in the map: %v", got)
	}
}

// TestOverBudgetPeriods covers #94 item 2: a period whose active members' tier-1
// invoices exceed the org contract is returned (with the right overage), an
// under-budget org is not, and the `since` filter excludes earlier periods.
func TestOverBudgetPeriods(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// orgOver: active members' tier-1 $1500 + $1000 = $2500 > org_total $2000.
	seedOrgMember(t, db, "alice", "orgOver")
	seedOrgMember(t, db, "bob", "orgOver")
	mustInsertSpend(t, db, ctx, "alice", "2026-05", 1500, now)
	mustInsertSpend(t, db, ctx, "bob", "2026-05", 1000, now)
	mustInsertOrgSpend(t, db, ctx, "orgOver", "2026-05", 2000, now)

	// orgOk: tier-1 $800 < org_total $2000 — must NOT be reported.
	seedOrgMember(t, db, "cara", "orgOk")
	seedOrgMember(t, db, "dan", "orgOk")
	mustInsertSpend(t, db, ctx, "cara", "2026-05", 800, now)
	mustInsertOrgSpend(t, db, ctx, "orgOk", "2026-05", 2000, now)

	// orgEqual: tier-1 EXACTLY equals org_total ($2000 == $2000). The clamp is
	// remainder<0 (strict), so equality is a genuine $0 remainder, NOT over
	// budget — this guards the predicate against a `>` → `>=` regression.
	seedOrgMember(t, db, "eve", "orgEqual")
	mustInsertSpend(t, db, ctx, "eve", "2026-05", 2000, now)
	mustInsertOrgSpend(t, db, ctx, "orgEqual", "2026-05", 2000, now)

	over, err := db.OverBudgetPeriods(ctx, since)
	if err != nil {
		t.Fatalf("OverBudgetPeriods: %v", err)
	}
	if len(over) != 1 {
		t.Fatalf("got %d over-budget rows (%+v), want 1 (orgOver only; orgOk under, orgEqual exactly at budget)", len(over), over)
	}
	p := over[0]
	if p.Org != "orgOver" || p.Period != "2026-05" {
		t.Errorf("got (%s, %s), want (orgOver, 2026-05)", p.Org, p.Period)
	}
	if p.OrgTotal != 2000 || p.Tier1Sum != 2500 || p.Overage != 500 {
		t.Errorf("got org_total=%v tier1=%v overage=%v, want 2000/2500/500", p.OrgTotal, p.Tier1Sum, p.Overage)
	}

	// A `since` after the period excludes it (the CTE filters period >= since).
	none, err := db.OverBudgetPeriods(ctx, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("OverBudgetPeriods(since=2026-06): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("got %d rows for since=2026-06, want 0 (2026-05 excluded)", len(none))
	}
}

func TestUpsertDeveloperAlias_RoundTripAndUpdate(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if err := db.UpsertDeveloperAlias(ctx, "asmith-gh", "alice.smith"); err != nil {
		t.Fatalf("insert alias: %v", err)
	}
	got, err := db.DeveloperAliases(ctx)
	if err != nil {
		t.Fatalf("DeveloperAliases: %v", err)
	}
	if got["asmith-gh"] != "alice.smith" {
		t.Errorf("alias = %q, want alice.smith", got["asmith-gh"])
	}

	// Upsert a new canonical for the same alias overwrites (not duplicates).
	if err := db.UpsertDeveloperAlias(ctx, "asmith-gh", "alice.corp"); err != nil {
		t.Fatalf("update alias: %v", err)
	}
	got, err = db.DeveloperAliases(ctx)
	if err != nil {
		t.Fatalf("DeveloperAliases: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("alias count = %d, want 1 (update, not insert)", len(got))
	}
	if got["asmith-gh"] != "alice.corp" {
		t.Errorf("alias = %q, want alice.corp after update", got["asmith-gh"])
	}
}

func TestUpsertDeveloperAlias_RejectsChains(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// Seed a -> b so the chain cases have something to collide with.
	if err := db.UpsertDeveloperAlias(ctx, "a", "b"); err != nil {
		t.Fatalf("seed a->b: %v", err)
	}

	cases := []struct {
		name             string
		alias, canonical string
	}{
		{"self map", "x", "x"},
		{"empty alias", "", "y"},
		{"empty canonical", "y", ""},
		{"canonical already an alias", "c", "a"}, // a is an alias -> c->a->b chain
		{"alias already a canonical", "b", "d"},  // b is a canonical -> a->b->d chain
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := db.UpsertDeveloperAlias(ctx, tc.alias, tc.canonical); err == nil {
				t.Errorf("UpsertDeveloperAlias(%q,%q) = nil, want error", tc.alias, tc.canonical)
			}
		})
	}

	// The seed row must be untouched by the rejected writes.
	got, err := db.DeveloperAliases(ctx)
	if err != nil {
		t.Fatalf("DeveloperAliases: %v", err)
	}
	if len(got) != 1 || got["a"] != "b" {
		t.Errorf("aliases = %v, want only a->b intact", got)
	}
}

func TestDeleteDeveloperAlias_FoundAndNotFound(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	if err := db.UpsertDeveloperAlias(ctx, "gh", "os"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	found, err := db.DeleteDeveloperAlias(ctx, "gh")
	if err != nil {
		t.Fatalf("delete existing: %v", err)
	}
	if !found {
		t.Errorf("delete existing: found = false, want true")
	}
	found, err = db.DeleteDeveloperAlias(ctx, "gh")
	if err != nil {
		t.Fatalf("delete absent: %v", err)
	}
	if found {
		t.Errorf("delete absent: found = true, want false")
	}
}

func TestDeveloperAliases_EmptyIsNonNil(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	got, err := db.DeveloperAliases(context.Background())
	if err != nil {
		t.Fatalf("DeveloperAliases: %v", err)
	}
	if got == nil {
		t.Errorf("DeveloperAliases returned nil map, want non-nil empty")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

// assertNoGroupOtherBits stats path and fails if any group/other permission
// bit is set. Using perm&0o077 == 0 rather than == 0600 keeps the assertion
// robust to whatever umask the test host runs under while still catching the
// world-readable 0644 default (#130).
func assertNoGroupOtherBits(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("%s has group/other permission bits: mode %#o, want no group/other (perm&0o077 == 0)", path, perm)
	}
}

// TestOpen_NewDBFilesNotWorldReadable verifies a freshly created DB and its
// WAL sidecars carry no group/other permission bits (#130). Fails on main
// under the default umask 022, where sql.Open creates the file 0644.
func TestOpen_NewDBFilesNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits not meaningful on Windows")
	}
	dbPath := filepath.Join(t.TempDir(), "fresh.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// The main DB file must always exist after Open.
	assertNoGroupOtherBits(t, dbPath)

	// The schema writes during Open create the -wal sidecar; require it.
	// -shm may or may not linger depending on checkpoint state, so only
	// assert it when present.
	assertNoGroupOtherBits(t, dbPath+"-wal")
	if _, err := os.Stat(dbPath + "-shm"); err == nil {
		assertNoGroupOtherBits(t, dbPath+"-shm")
	} else if os.IsNotExist(err) {
		t.Logf("-shm sidecar absent after Open (checkpointed); skipping its assertion")
	} else {
		t.Fatalf("stat %s-shm: %v", dbPath, err)
	}
}

// TestOpen_TightensExistingWorldReadableDB verifies Open repairs a legacy
// 0644 database file to owner-only (#130). Fails on main, where Open leaves
// the pre-existing mode untouched.
func TestOpen_TightensExistingWorldReadableDB(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits not meaningful on Windows")
	}
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	// An empty file is a valid zero-length SQLite target; create it
	// world-readable to mimic a pre-fix DB on disk.
	if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
		t.Fatalf("seed legacy db: %v", err)
	}
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	assertNoGroupOtherBits(t, dbPath)
	for _, suffix := range []string{"-wal", "-shm"} {
		p := dbPath + suffix
		if _, err := os.Stat(p); err == nil {
			assertNoGroupOtherBits(t, p)
		}
	}
}

// TestOpen_ReopenPreservesTightPermissions pins the claim that a sidecar
// recreated after checkpointing inherits the now-0600 main-file mode.
// Reopen, insert one token event to force WAL recreation, then re-assert all
// three paths that exist have no group/other bits (#130).
func TestOpen_ReopenPreservesTightPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits not meaningful on Windows")
	}
	dbPath := filepath.Join(t.TempDir(), "reopen.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close (first): %v", err)
	}

	db, err = Open(dbPath)
	if err != nil {
		t.Fatalf("Open (reopen): %v", err)
	}
	defer func() { _ = db.Close() }()

	// A write forces the -wal/-shm sidecars to be (re)created if a
	// checkpoint removed them on close.
	if err := db.InsertTokenEvent(context.Background(), TokenEvent{
		Developer: "alice",
		IssueID:   "issue-1",
		Model:     "claude-sonnet-4",
		InputTok:  10,
		OutputTok: 5,
		CostMicro: 1_000,
		Source:    "proxy",
		Fidelity:  "realtime",
		Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := dbPath + suffix
		if _, err := os.Stat(p); err == nil {
			assertNoGroupOtherBits(t, p)
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", p, err)
		}
	}
}

// mkTokenEvent is a compact token_events builder for the #136 tripwire tests.
func mkTokenEvent(dev, issue string, input, output, cacheRead, cw5m, cw1h int, ts time.Time) TokenEvent {
	return TokenEvent{
		Developer: dev, IssueID: issue, Model: "claude-sonnet-4",
		InputTok: input, OutputTok: output, CacheRead: cacheRead,
		CacheWrite5m: cw5m, CacheWrite1h: cw1h,
		CostMicro: 0, Source: "jsonl", Fidelity: "realtime", Timestamp: ts,
	}
}

// TestOutcomeTokenTotals_WindowedSum pins the core tripwire query (#136): events
// inside the 14-day pre-merge window are summed across ALL token classes; events
// outside the window are excluded; and each (developer, issue) is grouped
// independently so another developer's spend on the same issue lands in its own
// key, never the target's.
func TestOutcomeTokenTotals_WindowedSum(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	end := time.Now().UTC().Truncate(time.Second)

	events := []TokenEvent{
		// alice/issue-1, in-window: all five token classes summed = 168.
		mkTokenEvent("alice", "issue-1", 100, 50, 10, 5, 3, end.Add(-24*time.Hour)),
		// alice/issue-1, OUTSIDE window (20 days old) — excluded.
		mkTokenEvent("alice", "issue-1", 9999, 0, 0, 0, 0, end.Add(-20*24*time.Hour)),
		// alice/issue-1, AFTER the merge ts — excluded (window ends at o.ts).
		mkTokenEvent("alice", "issue-1", 7777, 0, 0, 0, 0, end.Add(24*time.Hour)),
		// bob/issue-1, in-window — same issue, DIFFERENT developer: its own key.
		mkTokenEvent("bob", "issue-1", 40, 0, 0, 0, 0, end.Add(-2*time.Hour)),
		// alice/issue-2, in-window — different issue, not requested below.
		mkTokenEvent("alice", "issue-2", 500, 0, 0, 0, 0, end.Add(-time.Hour)),
	}
	for _, ev := range events {
		if err := db.InsertTokenEvent(ctx, ev); err != nil {
			t.Fatalf("InsertTokenEvent: %v", err)
		}
	}

	// One outcome: alice merged issue-1 at end.
	totals, err := db.OutcomeTokenTotals(ctx, []Outcome{
		{Developer: "alice", IssueID: "issue-1", Timestamp: end},
	}, FleetWide)
	if err != nil {
		t.Fatalf("OutcomeTokenTotals: %v", err)
	}

	if got := totals[DevIssue{Developer: "alice", Repo: repoid.Unqualified, IssueID: "issue-1"}]; got != 168 {
		t.Errorf("alice/issue-1 total = %d, want 168 (in-window five-class sum only)", got)
	}
	if got := totals[DevIssue{Developer: "bob", Repo: repoid.Unqualified, IssueID: "issue-1"}]; got != 40 {
		t.Errorf("bob/issue-1 total = %d, want 40 (grouped under bob, not alice)", got)
	}
	// issue-2 was never requested (no OR-term), so it must be absent.
	if _, ok := totals[DevIssue{Developer: "alice", Repo: repoid.Unqualified, IssueID: "issue-2"}]; ok {
		t.Errorf("alice/issue-2 present, want absent (not among requested outcomes)")
	}
}

// TestOutcomeTokenTotals_MatchesBruteForceWindowedSum is the property-style
// check: over a seeded dataset of events and outcomes with distinct issues, the
// single windowed-union query must agree, key-for-key, with a per-outcome
// brute-force reference computation. (The spec sketched a two-phase coarse+exact
// query; this implementation uses one exact windowed-union query instead — see
// OutcomeTokenTotals — so the reference is the exact per-outcome window.)
func TestOutcomeTokenTotals_MatchesBruteForceWindowedSum(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	devs := []string{"alice", "bob", "carol"}
	// Distinct issue per outcome so no window-widening ambiguity; each outcome
	// merges at a different offset from base.
	outcomes := make([]Outcome, 0, 12)
	type ev struct {
		dev, issue string
		tokens     int
		ts         time.Time
	}
	var evs []ev

	for i := 0; i < 12; i++ {
		dev := devs[i%len(devs)]
		issue := "iss-" + string(rune('a'+i))
		mergeTS := base.Add(-time.Duration(i) * 24 * time.Hour)
		outcomes = append(outcomes, Outcome{Developer: dev, IssueID: issue, Timestamp: mergeTS})
		// Seed a spread of events around each merge: some in-window, some out.
		for j := 0; j < 5; j++ {
			offsetDays := (i*7 + j*11) % 30 // 0..29 days before merge
			evs = append(evs, ev{
				dev:    dev,
				issue:  issue,
				tokens: 100 + i*10 + j,
				ts:     mergeTS.Add(-time.Duration(offsetDays) * 24 * time.Hour),
			})
		}
		// A same-issue event by a DIFFERENT developer — must group separately.
		evs = append(evs, ev{dev: devs[(i+1)%len(devs)], issue: issue, tokens: 42, ts: mergeTS.Add(-2 * time.Hour)})
	}
	for _, e := range evs {
		if err := db.InsertTokenEvent(ctx, mkTokenEvent(e.dev, e.issue, e.tokens, 0, 0, 0, 0, e.ts)); err != nil {
			t.Fatalf("InsertTokenEvent: %v", err)
		}
	}

	got, err := db.OutcomeTokenTotals(ctx, outcomes, FleetWide)
	if err != nil {
		t.Fatalf("OutcomeTokenTotals: %v", err)
	}

	// Brute-force reference: for each outcome window [ts-14d, ts], sum every
	// event on that issue grouped by the event's developer.
	want := map[DevIssue]int64{}
	for _, o := range outcomes {
		low, high := o.Timestamp.Add(-AttributableWindow), o.Timestamp
		for _, e := range evs {
			if e.issue != o.IssueID {
				continue
			}
			if e.ts.Before(low) || e.ts.After(high) {
				continue
			}
			want[DevIssue{Developer: e.dev, Repo: repoid.Unqualified, IssueID: e.issue}] += int64(e.tokens)
		}
	}

	if len(got) != len(want) {
		t.Fatalf("key count: got %d, want %d\ngot=%v\nwant=%v", len(got), len(want), got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("total[%+v] = %d, want %d", k, got[k], v)
		}
	}
}

// TestOutcomeTokenTotals_EmptyOutcomes: no outcomes → empty map, no query, no
// error (the len==0 fast path).
func TestOutcomeTokenTotals_EmptyOutcomes(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	totals, err := db.OutcomeTokenTotals(context.Background(), nil, FleetWide)
	if err != nil {
		t.Fatalf("OutcomeTokenTotals(nil): %v", err)
	}
	if len(totals) != 0 {
		t.Errorf("totals = %v, want empty", totals)
	}
}

// TestOutcomeTokenTotals_WindowLowerBoundInclusive pins the >= lower edge: an
// event at EXACTLY mergeTS - AttributableWindow is inside the window, while one
// a second earlier is outside.
func TestOutcomeTokenTotals_WindowLowerBoundInclusive(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	end := time.Now().UTC().Truncate(time.Second)
	low := end.Add(-AttributableWindow)

	events := []TokenEvent{
		mkTokenEvent("alice", "issue-1", 100, 0, 0, 0, 0, low),                   // exactly on the edge → included
		mkTokenEvent("alice", "issue-1", 500, 0, 0, 0, 0, low.Add(-time.Second)), // one second before → excluded
	}
	for _, ev := range events {
		if err := db.InsertTokenEvent(ctx, ev); err != nil {
			t.Fatalf("InsertTokenEvent: %v", err)
		}
	}
	totals, err := db.OutcomeTokenTotals(ctx, []Outcome{
		{Developer: "alice", IssueID: "issue-1", Timestamp: end},
	}, FleetWide)
	if err != nil {
		t.Fatalf("OutcomeTokenTotals: %v", err)
	}
	if got := totals[DevIssue{Developer: "alice", Repo: repoid.Unqualified, IssueID: "issue-1"}]; got != 100 {
		t.Errorf("total = %d, want 100 (lower bound inclusive, second-before excluded)", got)
	}
}

// TestOutcomeTokenTotals_ReusedIssueUsesFreshestWindow is the anti-laundering
// (G-02) regression for a REUSED issue id: an old outcome banked 50k tokens 20
// days ago, then the same issue id is reused for a fresh tokenless PR today. The
// window must anchor on the FRESHEST merge ([today-14d, today]) so the stale
// tokens are NOT pulled in to launder the fresh PR — the total stays 0.
func TestOutcomeTokenTotals_ReusedIssueUsesFreshestWindow(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-20 * 24 * time.Hour)

	// 50k tokens spent around the OLD merge — outside the fresh 14-day window.
	if err := db.InsertTokenEvent(ctx, mkTokenEvent("alice", "issue-x", 50000, 0, 0, 0, 0, old)); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}

	// Two outcomes reuse issue-x: one merged 20d ago, one merged now (tokenless).
	totals, err := db.OutcomeTokenTotals(ctx, []Outcome{
		{Developer: "alice", IssueID: "issue-x", Timestamp: old},
		{Developer: "alice", IssueID: "issue-x", Timestamp: now},
	}, FleetWide)
	if err != nil {
		t.Fatalf("OutcomeTokenTotals: %v", err)
	}
	if got := totals[DevIssue{Developer: "alice", Repo: repoid.Unqualified, IssueID: "issue-x"}]; got != 0 {
		t.Errorf("total = %d, want 0 (freshest window must exclude stale 20-day-old tokens; unioning would launder)", got)
	}
}

// TestCapturedTokensByDayModel_GroupsAndFilters proves the remainder baseline
// (#138): jsonl/proxy rows are counted, api/anthropic-admin rows are excluded,
// per-class sums are grouped by Normalized model, the day boundary is UTC, and a
// non-Anthropic captured model is filtered out by provider.
func TestCapturedTokensByDayModel_GroupsAndFilters(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	// The target UTC day.
	day := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)

	insert := func(model, source string, ts time.Time, in, out, cr, w5, w1 int) {
		t.Helper()
		if err := db.InsertTokenEvent(ctx, TokenEvent{
			Developer: "alice", IssueID: "issue-1", Model: model,
			InputTok: in, OutputTok: out, CacheRead: cr, CacheWrite5m: w5, CacheWrite1h: w1,
			CostMicro: 1, Source: source, Fidelity: "realtime", Timestamp: ts,
		}); err != nil {
			t.Fatalf("InsertTokenEvent(%s/%s): %v", model, source, err)
		}
	}

	// Two captured rows for the same Anthropic model on the target day, one jsonl
	// one proxy, differing date-suffix so NormalizeModel must merge them.
	insert("claude-sonnet-4-20250514", "jsonl", day.Add(2*time.Hour), 1000, 200, 50, 10, 5)
	insert("claude-sonnet-4", "proxy", day.Add(9*time.Hour), 500, 100, 25, 0, 0)
	// A different Anthropic model on the same day.
	insert("claude-opus-4", "proxy", day.Add(3*time.Hour), 300, 60, 0, 0, 0)
	// EXCLUDED: manual api row and the poller's own anthropic-admin row.
	insert("claude-sonnet-4", "api", day.Add(4*time.Hour), 9999, 9999, 0, 0, 0)
	insert("claude-sonnet-4", "anthropic-admin", day.Add(5*time.Hour), 8888, 8888, 0, 0, 0)
	// EXCLUDED: a non-Anthropic captured model (provider filter).
	insert("gpt-4o", "proxy", day.Add(6*time.Hour), 7777, 7777, 0, 0, 0)
	// EXCLUDED by the UTC day window: 23:30 on the PRIOR day and 00:30 the NEXT day.
	insert("claude-sonnet-4", "proxy", day.Add(-30*time.Minute), 1, 1, 1, 1, 1)
	insert("claude-sonnet-4", "proxy", day.Add(24*time.Hour+30*time.Minute), 2, 2, 2, 2, 2)

	got, err := db.CapturedTokensByDayModel(ctx, day, "anthropic")
	if err != nil {
		t.Fatalf("CapturedTokensByDayModel: %v", err)
	}
	wantSonnet := CostUsage{Input: 1500, Output: 300, CacheRead: 75, CacheWrite5m: 10, CacheWrite1h: 5}
	if got["claude-sonnet-4"] != wantSonnet {
		t.Errorf("claude-sonnet-4 = %+v, want %+v (merged jsonl+proxy, api/admin/off-day excluded)", got["claude-sonnet-4"], wantSonnet)
	}
	wantOpus := CostUsage{Input: 300, Output: 60}
	if got["claude-opus-4"] != wantOpus {
		t.Errorf("claude-opus-4 = %+v, want %+v", got["claude-opus-4"], wantOpus)
	}
	if _, ok := got["gpt-4o"]; ok {
		t.Errorf("gpt-4o present; a non-Anthropic model must be filtered out by provider")
	}
	if len(got) != 2 {
		t.Errorf("got %d models, want 2 (sonnet, opus)", len(got))
	}
}

// TestCapturedTokensByDayModel_IncludesCodexRollout is the #464 R1 regression:
// the Codex rollout collector writes ONE ROW PER API CALL against an OpenAI key,
// so its tokens must appear in the baseline the OpenAI Usage poller subtracts
// from the org aggregate. If 'codex-rollout' ever falls out of the source list,
// the poller's remainder re-ingests every Codex token a second time and org
// spend is silently DOUBLED.
func TestCapturedTokensByDayModel_IncludesCodexRollout(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	day := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	insert := func(model, source string, ts time.Time, in, out, cr int) {
		t.Helper()
		if err := db.InsertTokenEvent(ctx, TokenEvent{
			Developer: "alice", IssueID: "issue-1", Model: model,
			InputTok: in, OutputTok: out, CacheRead: cr,
			CostMicro: 1, Source: source, Fidelity: "realtime", Timestamp: ts,
		}); err != nil {
			t.Fatalf("InsertTokenEvent(%s/%s): %v", model, source, err)
		}
	}

	// Two per-call Codex rows for one model — the shape the collector emits.
	insert("gpt-5.6-terra", "codex-rollout", day.Add(2*time.Hour), 400, 80, 20)
	insert("gpt-5.6-terra", "codex-rollout", day.Add(3*time.Hour), 100, 20, 5)
	// EXCLUDED: the poller's own remainder feed. Including it would make the
	// poller subtract its own prior output and converge to zero.
	insert("gpt-5.6-terra", "openai-usage", day.Add(4*time.Hour), 9999, 9999, 0)

	got, err := db.CapturedTokensByDayModel(ctx, day, "openai")
	if err != nil {
		t.Fatalf("CapturedTokensByDayModel: %v", err)
	}
	want := CostUsage{Input: 500, Output: 100, CacheRead: 25}
	if got["gpt-5.6-terra"] != want {
		t.Errorf("gpt-5.6-terra baseline = %+v, want %+v — codex-rollout rows MUST be in the poller's subtraction baseline or Codex spend is double-counted (#464 R1)",
			got["gpt-5.6-terra"], want)
	}
}

// TestOrgActualSpendNet_SumsDeltasBySource proves the source-scoped reconciliation
// read side (#138 review R1): multiple delta rows for one (org, period, source) net
// out; a different period, a different source, and an absent triple do not bleed
// in. The cross-source isolation is the R1 regression — the Anthropic poller's net
// must NOT include a 'manual' (other-provider) row.
func TestOrgActualSpendNet_SumsDeltasBySource(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	post := func(org, period, source string, micro int64) {
		t.Helper()
		if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
			Org: org, Period: period, Source: source, ActualPaidMicro: micro, Timestamp: now,
		}); err != nil {
			t.Fatalf("InsertOrgActualSpend: %v", err)
		}
	}
	const srcAdmin = "anthropic-admin"
	post("acme", "2026-06", srcAdmin, 80_000_000)             // $80
	post("acme", "2026-06", srcAdmin, 20_000_000)             // +$20 delta
	post("acme", "2026-06", srcAdmin, -5_000_000)             // -$5 credit memo
	post("acme", "2026-06", OrgSpendSourceManual, 40_000_000) // other-provider manual row — must NOT bleed in
	post("acme", "2026-07", srcAdmin, 100_000_000)            // different period, must not bleed in

	net, err := db.OrgActualSpendNet(ctx, "acme", "2026-06", srcAdmin)
	if err != nil {
		t.Fatalf("OrgActualSpendNet: %v", err)
	}
	if net != 95_000_000 {
		t.Errorf("anthropic-admin net = %d, want 95000000 ($95 = 80+20-5; the $40 manual row must be excluded)", net)
	}

	// The manual source nets independently — untouched by the anthropic-admin rows.
	manualNet, err := db.OrgActualSpendNet(ctx, "acme", "2026-06", OrgSpendSourceManual)
	if err != nil {
		t.Fatalf("OrgActualSpendNet(manual): %v", err)
	}
	if manualNet != 40_000_000 {
		t.Errorf("manual net = %d, want 40000000", manualNet)
	}

	zero, err := db.OrgActualSpendNet(ctx, "acme", "2020-01", srcAdmin)
	if err != nil {
		t.Fatalf("OrgActualSpendNet(absent): %v", err)
	}
	if zero != 0 {
		t.Errorf("net for absent (org,period,source) = %d, want 0", zero)
	}
}

// TestOrgActualSpend_ReadSumsAcrossSources proves the allocation read path (the
// `inv` CTE behind ActualSpendForDeveloper) SUMs org_actual_spend across sources
// (#138): once an Anthropic poller row and a manual/other-provider row coexist for
// one (org, period), the org total the developer allocation sees is their SUM, so
// the org's actual-paid stays complete. This is the counterpart to the source-
// scoped reconciliation: scoped on write, summed on read.
func TestOrgActualSpend_ReadSumsAcrossSources(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	// Sole member of org "acme", so the org total allocates entirely to alice.
	if err := db.UpsertHierarchy(ctx, "alice", "platform", "", "acme"); err != nil {
		t.Fatalf("UpsertHierarchy: %v", err)
	}
	// $60 Anthropic (poller) + $40 other-provider (manual), same (org, period).
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "acme", Period: "2026-05", Source: "anthropic-admin", ActualPaidMicro: 60 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend(anthropic-admin): %v", err)
	}
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "acme", Period: "2026-05", Source: OrgSpendSourceManual, ActualPaidMicro: 40 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend(manual): %v", err)
	}

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	got, err := db.ActualSpendForDeveloper(ctx, "alice", since)
	if err != nil {
		t.Fatalf("ActualSpendForDeveloper: %v", err)
	}
	if got != 100 {
		t.Errorf("alice slice = %v, want 100 ($60 anthropic-admin + $40 manual, summed across sources)", got)
	}
}

// TestOrgActualSpendTotals_NetsCreditMemos (#42): the read-back SUMs the
// accumulation rows within each (org, period), so a credit memo nets against the
// original invoice, and the org filter isolates one org.
func TestOrgActualSpendTotals_NetsCreditMemos(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	// acme: $500 invoice + (−$100) credit memo in 2026-06 → net $400, 2 entries.
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "acme", Period: "2026-06", ActualPaidMicro: 500 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend(acme +500): %v", err)
	}
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "acme", Period: "2026-06", ActualPaidMicro: -100 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend(acme -100): %v", err)
	}
	// A SECOND org that must be excluded by the org filter.
	if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
		Org: "globex", Period: "2026-06", ActualPaidMicro: 999 * MicroPerUSD, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend(globex): %v", err)
	}

	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got, err := db.OrgActualSpendTotals(ctx, since, "acme")
	if err != nil {
		t.Fatalf("OrgActualSpendTotals: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1 (org filter excludes globex)", len(got))
	}
	if got[0].Org != "acme" || got[0].Period != "2026-06" {
		t.Errorf("row key = %s/%s, want acme/2026-06", got[0].Org, got[0].Period)
	}
	if got[0].ActualPaidUSD != 400.0 {
		t.Errorf("ActualPaidUSD = %v, want 400 ($500 − $100 credit memo)", got[0].ActualPaidUSD)
	}
	if got[0].Entries != 2 {
		t.Errorf("Entries = %d, want 2 (invoice + credit memo)", got[0].Entries)
	}

	// Empty org returns every org (both acme and globex).
	all, err := db.OrgActualSpendTotals(ctx, since, "")
	if err != nil {
		t.Fatalf("OrgActualSpendTotals(all): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d rows for all-orgs, want 2 (acme + globex)", len(all))
	}
}

// TestOrgActualSpendTotals_SinceMonthBoundary (#42): a row whose period equals
// since's month is INCLUDED; an earlier period is excluded. Windowing is by
// YYYY-MM lexicographic compare, which coincides with chronological order for
// the canonical zero-padded form.
func TestOrgActualSpendTotals_SinceMonthBoundary(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	for _, p := range []string{"2026-03", "2026-04", "2026-05"} {
		if err := db.InsertOrgActualSpend(ctx, OrgActualSpend{
			Org: "acme", Period: p, ActualPaidMicro: 100 * MicroPerUSD, Timestamp: now,
		}); err != nil {
			t.Fatalf("InsertOrgActualSpend(%s): %v", p, err)
		}
	}

	// since = 2026-04-15 → month 2026-04. Expect 2026-04 and 2026-05, NOT 2026-03.
	since := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
	got, err := db.OrgActualSpendTotals(ctx, since, "acme")
	if err != nil {
		t.Fatalf("OrgActualSpendTotals: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (2026-04, 2026-05; 2026-03 excluded)", len(got))
	}
	if got[0].Period != "2026-04" || got[1].Period != "2026-05" {
		t.Errorf("periods = %s,%s, want 2026-04,2026-05 (ordered by period)", got[0].Period, got[1].Period)
	}
}

// TestOrgActualSpendTotals_EmptyReturnsNonNilSlice (#42): the API contract is
// "orgs": [] (not null) on an empty result — the store must return a non-nil
// zero-length slice.
func TestOrgActualSpendTotals_EmptyReturnsNonNilSlice(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	got, err := db.OrgActualSpendTotals(ctx, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "")
	if err != nil {
		t.Fatalf("OrgActualSpendTotals: %v", err)
	}
	if got == nil {
		t.Errorf("got nil slice, want non-nil empty slice (so it marshals to [])")
	}
	if len(got) != 0 {
		t.Errorf("got %d rows on an empty table, want 0", len(got))
	}
}

// --- work-type taxonomy (#187) ---

// TestOpen_MigratesWorkTypeColumns pins the #187 convergent migration: an
// outcomes table predating work_type / work_type_source is ADD COLUMN'd on Open()
// without losing the pre-existing row, which reads back work_type='feature' (the
// honest baseline) and work_type_source='legacy' (category unknowable, mirroring
// weight_source). AllOutcomesSince must surface the migrated row cleanly.
func TestOpen_MigratesWorkTypeColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-worktype.db")

	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Pre-#187 outcomes shape: no work_type / work_type_source columns.
	if _, err = legacy.Exec(`
		CREATE TABLE outcomes (
		    id                INTEGER PRIMARY KEY AUTOINCREMENT,
		    developer         TEXT    NOT NULL,
		    issue_id          TEXT    NOT NULL,
		    pr_number         INTEGER,
		    weight            REAL    NOT NULL,
		    quality           REAL    NOT NULL DEFAULT 1.0,
		    merge_commit_sha  TEXT,
		    ts                DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO outcomes (developer, issue_id, pr_number, weight, quality, ts)
		VALUES ('alice', 'issue-old', 3, 5.0, 1.0, ` + "'2026-01-01 00:00:00'" + `);
	`); err != nil {
		t.Fatalf("create legacy outcomes schema: %v", err)
	}
	_ = legacy.Close()

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open on legacy DB: %v", err)
	}
	defer func() { _ = db.Close() }()

	got, err := db.AllOutcomesSince(context.Background(), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AllOutcomesSince: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 migrated row, got %d", len(got))
	}
	if got[0].WorkType != WorkTypeFeature {
		t.Errorf("migrated WorkType = %q, want %q", got[0].WorkType, WorkTypeFeature)
	}
	if got[0].WorkTypeSource != WorkTypeSourceLegacy {
		t.Errorf("migrated WorkTypeSource = %q, want %q", got[0].WorkTypeSource, WorkTypeSourceLegacy)
	}
}

// TestInsertOutcome_PersistsWorkType pins that InsertOutcome round-trips an
// explicit work_type/source and coerces an empty pair to the 'feature'/'default'
// live-insert baseline (NOT 'legacy', which is reserved for migrated rows).
func TestInsertOutcome_PersistsWorkType(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	since := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "sec", IssueID: "issue-sec", PRNumber: 1, Weight: 3, Quality: 1.0,
		MergeCommitSHA: "sha-sec", WorkType: WorkTypeSecurity, WorkTypeSource: WorkTypeSourceLabel,
		Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert security outcome: %v", err)
	}
	// Empty work-type pair -> feature/default coercion.
	if _, err := db.InsertOutcome(ctx, Outcome{
		Developer: "dev", IssueID: "issue-plain", PRNumber: 2, Weight: 1, Quality: 1.0,
		MergeCommitSHA: "sha-plain", Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert plain outcome: %v", err)
	}

	got, err := db.AllOutcomesSince(ctx, since)
	if err != nil {
		t.Fatalf("AllOutcomesSince: %v", err)
	}
	byIssue := map[string]Outcome{}
	for _, o := range got {
		byIssue[o.IssueID] = o
	}
	if o := byIssue["issue-sec"]; o.WorkType != WorkTypeSecurity || o.WorkTypeSource != WorkTypeSourceLabel {
		t.Errorf("security outcome = (%q,%q), want (security,label)", o.WorkType, o.WorkTypeSource)
	}
	if o := byIssue["issue-plain"]; o.WorkType != WorkTypeFeature || o.WorkTypeSource != WorkTypeSourceDefault {
		t.Errorf("plain outcome = (%q,%q), want (feature,default)", o.WorkType, o.WorkTypeSource)
	}
}

// TestUpsertPushOutcome_WorkTypeFeatureDefault pins that a push-captured outcome
// (commits carry no labels) always lands as feature/default (#187).
func TestUpsertPushOutcome_WorkTypeFeatureDefault(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	since := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	if _, err := db.UpsertPushOutcome(ctx, Outcome{
		Developer: "carol", IssueID: "issue-push", Weight: 0.5, Quality: 1.0,
		Timestamp: time.Now().UTC(),
	}, "2026-02-02"); err != nil {
		t.Fatalf("UpsertPushOutcome: %v", err)
	}
	got, err := db.AllOutcomesSince(ctx, since)
	if err != nil || len(got) != 1 {
		t.Fatalf("AllOutcomesSince: err=%v len=%d", err, len(got))
	}
	if got[0].WorkType != WorkTypeFeature || got[0].WorkTypeSource != WorkTypeSourceDefault {
		t.Errorf("push outcome = (%q,%q), want (feature,default)", got[0].WorkType, got[0].WorkTypeSource)
	}
}

// TestDeveloperIssueCosts groups cost at (developer, issue) grain and splits out
// the realtime subset (#187), the denominator source for work-type segmentation.
func TestDeveloperIssueCosts(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	mk := func(dev, issue string, micro int64, fidelity, key string) TokenEvent {
		return TokenEvent{
			Developer: dev, IssueID: issue, Model: "claude-sonnet-4",
			InputTok: 1000, CostMicro: micro, Source: "jsonl", Fidelity: fidelity,
			IdempotencyKey: key, Timestamp: now,
		}
	}
	for _, e := range []TokenEvent{
		mk("alice", "issue-a", 1_000, "realtime", "k1"),
		mk("alice", "issue-a", 500, "daily", "k2"),
		mk("alice", "issue-b", 2_000, "realtime", "k3"),
	} {
		if err := db.InsertTokenEvent(ctx, e); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}

	rows, err := db.DeveloperIssueCosts(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("DeveloperIssueCosts: %v", err)
	}
	got := map[string]DevIssueCost{}
	for _, r := range rows {
		got[r.Developer+"/"+r.IssueID] = r
	}
	if a := got["alice/issue-a"]; a.TotalCostMicro != 1_500 || a.RealtimeCostMicro != 1_000 {
		t.Errorf("issue-a = total %d realtime %d, want 1500/1000", a.TotalCostMicro, a.RealtimeCostMicro)
	}
	if b := got["alice/issue-b"]; b.TotalCostMicro != 2_000 || b.RealtimeCostMicro != 2_000 {
		t.Errorf("issue-b = total %d realtime %d, want 2000/2000", b.TotalCostMicro, b.RealtimeCostMicro)
	}
}
