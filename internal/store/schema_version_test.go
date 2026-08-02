package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// readUserVersion opens a raw connection (bypassing Open, so no migration runs)
// and returns the stamped PRAGMA user_version.
func readUserVersion(t *testing.T, path string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var v int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

// setUserVersion stamps a version via a raw connection (bypassing Open).
func setUserVersion(t *testing.T, path string, v int) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
}

// dumpSchema returns a stable string of every object in sqlite_master, so a test
// can assert the on-disk schema was NOT mutated across a refused Open.
func dumpSchema(t *testing.T, path string) string {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	defer func() { _ = raw.Close() }()
	rows, err := raw.Query(`SELECT type, name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var typ, name, sqlText string
		if err := rows.Scan(&typ, &name, &sqlText); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\n", typ, name, sqlText)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master: %v", err)
	}
	return b.String()
}

// TestOpen_StampsUserVersion pins that a fresh Open stamps the current
// schemaVersion into the SQLite header (#141).
func TestOpen_StampsUserVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := readUserVersion(t, path); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}
}

// TestOpen_RefusesNewerSchemaVersion pins the refuse-if-newer gate (#141): an
// older binary opening a DB a newer tierd stamped must fail loudly AND mutate
// nothing.
func TestOpen_RefusesNewerSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "newer.db")
	// Create + migrate + stamp at the current version, then forge a newer stamp.
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = db.Close()
	setUserVersion(t, path, schemaVersion+1)

	before := dumpSchema(t, path)

	_, err = Open(path)
	if err == nil {
		t.Fatal("Open on a newer-stamped DB succeeded, want refusal")
	}
	msg := err.Error()
	for _, want := range []string{"newer", fmt.Sprintf("%d", schemaVersion+1), fmt.Sprintf("%d", schemaVersion)} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing substring %q", msg, want)
		}
	}
	// The refused Open must not have run any schema statement.
	if after := dumpSchema(t, path); after != before {
		t.Errorf("schema mutated by a refused Open:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if got := readUserVersion(t, path); got != schemaVersion+1 {
		t.Errorf("user_version = %d, want unchanged %d", got, schemaVersion+1)
	}
}

// TestOpen_LegacyUnstampedDBMigratesAndStamps pins that a pre-#141 database
// (user_version 0, legacy schema) migrates cleanly and ends up stamped (#141).
func TestOpen_LegacyUnstampedDBMigratesAndStamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	// Build a pre-#6-style legacy schema with an unstamped (0) user_version, the
	// same fixture shape TestOpen_MigratesPreIssue6Schema uses.
	legacy, err := sql.Open("sqlite", path)
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
	if got := readUserVersion(t, path); got != 0 {
		t.Fatalf("precondition: legacy user_version = %d, want 0", got)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open on legacy DB: %v", err)
	}
	// Pre-migration row is preserved (guards against a mid-migration wipe).
	costs, err := db.DeveloperCosts(context.Background(), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	if len(costs) != 1 || costs[0].Developer != "alice" {
		t.Fatalf("expected pre-migration row preserved, got %+v", costs)
	}
	_ = db.Close()

	if got := readUserVersion(t, path); got != schemaVersion {
		t.Errorf("post-migration user_version = %d, want %d", got, schemaVersion)
	}
}
