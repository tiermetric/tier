package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestScoresIndex_SwappedInForSingleColumn is the deterministic,
// planner-independent guard: the #72 covering index exists and the two
// single-column indexes it supersedes (idx_token_events_ts and
// idx_token_events_developer) have been dropped (on fresh AND upgraded DBs,
// since the DROPs ride in schemaTables).
func TestScoresIndex_SwappedInForSingleColumn(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	idx := indexNames(t, db.db)
	if !idx["idx_token_events_scores"] {
		t.Errorf("idx_token_events_scores missing; indexes=%v", idx)
	}
	for _, superseded := range []string{"idx_token_events_ts", "idx_token_events_developer"} {
		if idx[superseded] {
			t.Errorf("%s should have been dropped (superseded by the covering index); indexes=%v", superseded, idx)
		}
	}
}

// indexNames returns the set of indexes on token_events.
func indexNames(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(
		`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='token_events'`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer func() { _ = rows.Close() }()
	idx := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		idx[name] = true
	}
	return idx
}

// TestScoresIndex_UpgradeDropsOldIndexes proves the DROP actually fires on an
// UPGRADED DB. The fresh-DB swap test above can't: there the old indexes never
// existed, so DROP INDEX IF EXISTS is a no-op and "they're absent" is trivially
// true. Here we seed a pre-#72 DB that DOES carry the two single-column indexes,
// reopen through the real Open(), and assert they were dropped and the covering
// index created — so a regression that removed the DROP statements (leaving
// stale indexes on every deployed DB) is caught.
func TestScoresIndex_UpgradeDropsOldIndexes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pre72.db")

	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Legacy (pre-#6-shaped) token_events PLUS the two single-column indexes a
	// pre-#72 deployment would carry. Open()'s column migrations bring the table
	// up to date; the index swap is what we assert.
	if _, err := legacy.Exec(`
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
		CREATE INDEX idx_token_events_developer ON token_events(developer);
		CREATE INDEX idx_token_events_ts        ON token_events(ts);
	`); err != nil {
		t.Fatalf("create pre-72 schema: %v", err)
	}
	_ = legacy.Close()

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open on pre-72 DB: %v", err)
	}
	defer func() { _ = db.Close() }()

	idx := indexNames(t, db.db)
	if !idx["idx_token_events_scores"] {
		t.Errorf("upgrade did not create idx_token_events_scores; indexes=%v", idx)
	}
	for _, old := range []string{"idx_token_events_developer", "idx_token_events_ts"} {
		if idx[old] {
			t.Errorf("upgrade did not drop pre-existing %s; indexes=%v", old, idx)
		}
	}
}

// TestScoresIndex_UpgradeRebuildsOnCostMicro is the #69 counterpart: a pre-#69
// DB that ALREADY carries an idx_token_events_scores built on cost_usd must (a)
// open cleanly — migrateCostUSDToMicro has to DROP that index before ALTER TABLE
// DROP COLUMN cost_usd, since SQLite refuses to drop a column an index
// references — and (b) end up with the covering index rebuilt on cost_micro, not
// the stale cost_usd one. The other upgrade test seeds only the two single-column
// indexes, so the cost_usd-based scores index DROP path is never exercised there.
func TestScoresIndex_UpgradeRebuildsOnCostMicro(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pre69-idx.db")

	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Pre-#69 token_events (cost_usd REAL) PLUS a covering index built on
	// cost_usd — the exact shape a #72-but-pre-#69 deployment carries.
	if _, err := legacy.Exec(`
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
		CREATE INDEX idx_token_events_scores
		    ON token_events(developer, ts, cost_usd, fidelity);
		INSERT INTO token_events (developer, issue_id, model, cost_usd, source, fidelity)
		VALUES ('alice', 'issue-1', 'claude-sonnet-4', 1.5, 'jsonl', 'realtime');
	`); err != nil {
		t.Fatalf("create pre-#69 schema with cost_usd scores index: %v", err)
	}
	_ = legacy.Close()

	// Open MUST succeed: if the migration failed to DROP the cost_usd-based index
	// before DROP COLUMN cost_usd, the ALTER would error and Open would fail here.
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open on pre-#69 DB with cost_usd scores index: %v", err)
	}
	defer func() { _ = db.Close() }()

	if !indexNames(t, db.db)["idx_token_events_scores"] {
		t.Fatal("idx_token_events_scores missing after upgrade")
	}
	// The rebuilt index must reference cost_micro, not the dropped cost_usd.
	var indexSQL string
	if err := db.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_token_events_scores'`,
	).Scan(&indexSQL); err != nil {
		t.Fatalf("read index sql: %v", err)
	}
	if !strings.Contains(indexSQL, "cost_micro") || strings.Contains(indexSQL, "cost_usd") {
		t.Errorf("scores index not rebuilt on cost_micro after upgrade: %q", indexSQL)
	}
}

// TestDeveloperCosts_UsesCoveringIndex pins the actual #72 win: the scores
// aggregation must be served index-only by idx_token_events_scores, not a table
// scan. EXPLAIN QUERY PLAN is what proves the optimizer engages the covering
// index — a mere "index exists" check wouldn't catch a regression that made the
// query stop using it (e.g. selecting a column outside the covered set). Rows
// are spread over a wide time range and queried with a selective window so the
// planner clearly prefers the index.
func TestDeveloperCosts_UsesCoveringIndex(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now().UTC()
	var batch []TokenEvent
	for i := 0; i < 100; i++ {
		dev := "alice"
		if i%2 == 0 {
			dev = "bob"
		}
		batch = append(batch, TokenEvent{
			Developer: dev, IssueID: "i1", Model: "claude-sonnet-4", CostMicro: 1_500_000, // $1.50
			Source: "jsonl", Fidelity: "realtime",
			Timestamp: now.Add(-time.Duration(i) * 24 * time.Hour), // spread over 100 days
		})
	}
	if err := db.InsertTokenEvents(ctx, batch); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}

	rows, err := db.db.QueryContext(ctx, `
		EXPLAIN QUERY PLAN
		SELECT developer, SUM(cost_micro),
		       SUM(CASE WHEN fidelity = 'realtime' THEN cost_micro ELSE 0 END)
		FROM token_events WHERE ts >= ? GROUP BY developer`,
		now.Add(-2*24*time.Hour)) // selective: only the last ~2 days match
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	got := plan.String()
	if !strings.Contains(got, "idx_token_events_scores") {
		t.Errorf("scores query does not use idx_token_events_scores; plan:\n%s", got)
	}
	if !strings.Contains(got, "COVERING INDEX") {
		t.Errorf("scores query is not index-only (no COVERING INDEX in plan); plan:\n%s", got)
	}
}
