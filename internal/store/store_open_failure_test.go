package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// This file drives store.Open's ERROR half -- the corrupt-file and
// mid-migration failure branches that the happy-path TestOpen_Migrates* suite
// never reaches (#153). Each test hand-builds a specific failure and asserts
// Open fails LOUD (non-nil, phase-identifying, wrapped error) and returns NO
// usable *DB, rather than half-opening or panicking. This is the "house
// posture": a truncated copy or a foreign/hostile file at --db must make tierd
// refuse to start, not limp on against a shape it does not understand.
//
// All tests use real SQLite files under t.TempDir() (no fixtures, no network),
// mirroring the TestOpen_MigratesPreIssue6Schema raw-seed pattern.

// TestOpen_FailsOnNonSQLiteFile: a garbage / non-SQLite file at the DB path
// must make Open error (the driver surfaces "file is not a database" at the
// db.Ping probe -> the `connect db:` wrap) and return no handle. Pins the
// corrupt-legacy-file branch: a truncated backup or a foreign file at --db
// fails fast instead of mid-schema-apply.
//
// Negative proof: if Open ever returned the *DB despite the ping error (dropped
// the `_ = db.Close(); return nil, ...` at store.go:571-574), this test's
// non-nil-db check fails.
func TestOpen_FailsOnNonSQLiteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.db")
	// ~1.5KB of clearly-non-SQLite bytes. A real SQLite file begins with the
	// 16-byte "SQLite format 3\000" magic header; this deliberately does not.
	var garbage []byte
	for i := 0; i < 64; i++ {
		garbage = append(garbage, []byte("this is not a database\n")...)
	}
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatalf("seed garbage file: %v", err)
	}

	db, err := Open(path)
	if db != nil {
		_ = db.Close()
		t.Fatal("Open returned a usable *DB for a non-SQLite file; want nil")
	}
	if err == nil {
		t.Fatal("Open on a non-SQLite file succeeded; want error")
	}
	// The failing phase wrap is stable across the modernc driver (the ping
	// probe fires before any schema statement). Assert the phase, but do not
	// over-pin the driver's raw "file is not a database (26)" text.
	if !strings.Contains(err.Error(), "connect db") {
		t.Errorf("error %q missing phase wrap %q", err, "connect db")
	}
}

// TestOpen_FailsWhenOutcomesIsAView: a hostile pre-existing schema where the
// `outcomes` NAME is a VIEW, not a table. CREATE TABLE IF NOT EXISTS is a no-op
// over an existing same-name object, so Open proceeds into schemaTables' index
// creation -- and "CREATE INDEX ... ON outcomes(developer)" fails because a
// view may not be indexed. Open must surface this as the phase-identifying
// `apply schema:` wrap (store.go:607-609) and return no handle.
//
// NOTE (#153 spec deviation, verified empirically 2026-07-13): the spec
// anticipated this tripping addColumnIfMissing's `migrate outcomes.merge_commit_sha`
// wrap. It does not: schemaTables (Phase 1) creates indexes ON outcomes BEFORE
// any Phase-2 ALTER runs, and "views may not be indexed" fires first. Per the
// spec's own instruction ("if the view trips an EARLIER phase instead, pin
// whichever wrapped phase actually fires"), this pins `apply schema:` -- the
// point is a hostile pre-existing schema -> wrapped, phase-identifying error,
// no half-open DB, which holds.
//
// Negative proof: if Phase 1 stopped wrapping its error (`return db.Exec(...)`
// bare instead of `fmt.Errorf("apply schema: %w", ...)`), the phase assertion
// fails.
func TestOpen_FailsWhenOutcomesIsAView(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcomes-view.db")
	assertViewTripsApplySchema(t, path, "outcomes")
}

// TestOpen_FailsWhenTokenEventsIsAView: same injection on the token_events name.
// Documented in the same terms as the outcomes case: schemaTables indexes
// token_events (idx_token_events_issue) in Phase 1, so a view there also trips
// the `apply schema:` wrap before any ALTER. Kept as a distinct case because
// token_events and outcomes are seeded and indexed independently -- a
// regression that reorders Phase 1 to ALTER one table before indexing the other
// would change exactly one of these two tests.
func TestOpen_FailsWhenTokenEventsIsAView(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token-events-view.db")
	assertViewTripsApplySchema(t, path, "token_events")
}

// assertViewTripsApplySchema seeds `name` as a VIEW, then asserts Open fails
// with the `apply schema:` wrap and returns no handle.
func assertViewTripsApplySchema(t *testing.T, path, name string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// A view carrying a `developer` column so it is superficially table-like;
	// the failure is structural (indexing a view), not column-driven.
	if _, err := raw.Exec("CREATE VIEW " + name + " AS SELECT 1 AS developer"); err != nil {
		_ = raw.Close()
		t.Fatalf("seed view %s: %v", name, err)
	}
	_ = raw.Close()

	db, err := Open(path)
	if db != nil {
		_ = db.Close()
		t.Fatalf("Open returned a usable *DB when %q was a view; want nil", name)
	}
	if err == nil {
		t.Fatalf("Open with %q as a view succeeded; want error", name)
	}
	if !strings.Contains(err.Error(), "apply schema") {
		t.Errorf("error %q missing phase wrap %q", err, "apply schema")
	}
}

// TestOpen_FailsAndRollsBackWhenActualSpendRebuildCannotCopy drives a genuine
// mid-MIGRATION failure with transaction rollback (#153 point b), a distinct
// branch from the Phase-1 `apply schema:` cases above.
//
// Seed a legacy pre-#24 `actual_spend` that (a) still carries the
// `CHECK (actual_paid_usd >= 0)` constraint the #24 migration drops -- so
// dropActualSpendNonNegativeCheck's rebuild path is entered -- and (b) has NO
// period GLOB check, so we can seed a row whose period the REBUILT table (which
// re-adds the GLOB check) rejects. Every other table is created fresh by
// schemaTables, so Phase 1 passes and Open reaches Phase 2.5, where the rebuild
// COPY hits the period CHECK and the wrapped `migrate actual_spend drop check:`
// error (store.go:733-735) fires.
//
// Then assert the rebuild's `defer tx.Rollback()` (store.go:916) actually
// rolled back: the original row survives and no half-built `__new_actual_spend`
// table leaks. This pins fail-loud-AND-atomic: a mid-migration abort must leave
// the DB byte-consistent for the next boot's idempotent retry.
//
// Negative proof: if the rebuild committed unconditionally (or Phase 2.5
// dropped its error wrap), either the phase assertion or the rollback/leftover
// assertions fail.
func TestOpen_FailsAndRollsBackWhenActualSpendRebuildCannotCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hostile-actual-spend.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Legacy shape: the usd CHECK the migration targets, and a period column
	// with NO GLOB check (so 'not-a-period' is insertable here but rejected by
	// the rebuilt table).
	_, err = raw.Exec(`
		CREATE TABLE actual_spend (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			developer         TEXT    NOT NULL,
			period            TEXT    NOT NULL,
			actual_paid_usd   REAL    NOT NULL CHECK (actual_paid_usd >= 0),
			ts                DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO actual_spend (developer, period, actual_paid_usd)
		VALUES ('alice', 'not-a-period', 1.0);
	`)
	if err != nil {
		_ = raw.Close()
		t.Fatalf("seed legacy actual_spend: %v", err)
	}
	_ = raw.Close()

	db, err := Open(path)
	if db != nil {
		_ = db.Close()
		t.Fatal("Open returned a usable *DB despite a failed migration; want nil")
	}
	if err == nil {
		t.Fatal("Open succeeded despite an uncopyable actual_spend rebuild; want error")
	}
	if !strings.Contains(err.Error(), "migrate actual_spend drop check") {
		t.Errorf("error %q missing phase wrap %q", err, "migrate actual_spend drop check")
	}

	// Rollback proof: re-open raw and confirm the transaction unwound.
	verify, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen for rollback check: %v", err)
	}
	defer func() { _ = verify.Close() }()

	var rows int
	if err := verify.QueryRow(`SELECT count(*) FROM actual_spend`).Scan(&rows); err != nil {
		t.Fatalf("count original actual_spend after rollback: %v", err)
	}
	if rows != 1 {
		t.Errorf("original actual_spend row count = %d after rollback; want 1 (data lost)", rows)
	}
	var leftover string
	err = verify.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='__new_actual_spend'`,
	).Scan(&leftover)
	if err == nil {
		t.Errorf("half-built %q table leaked; rebuild tx did not roll back", leftover)
	} else if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("check for __new_actual_spend leftover: %v", err)
	}
}

// TestOpen_FailsOnUnwritablePath: a DB path inside a directory that does not
// exist makes the very first step -- os.OpenFile(path, O_RDWR|O_CREATE, 0600)
// -- fail, so Open returns the `create db file:` wrap (store.go:529-532) and no
// handle. Covers Open's earliest error branch (create-tight-before-sql.Open,
// #130). Chosen over an os.Chmod(0400) read-only-file case because a
// nonexistent parent dir fails deterministically for EVERY uid INCLUDING root,
// whereas 0400 is a no-op for root and would need a uid==0 skip -- and both
// trip the SAME `create db file:` branch anyway (verified: O_RDWR on a 0400
// file returns "permission denied" at the same OpenFile), so the read-only
// variant would add root-flakiness without covering a new branch.
//
// Negative proof: if Open stopped checking os.OpenFile's error (or unwrapped
// it), the phase assertion or the nil-db check fails.
func TestOpen_FailsOnUnwritablePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist", "tier.db")

	db, err := Open(path)
	if db != nil {
		_ = db.Close()
		t.Fatal("Open returned a usable *DB for an uncreatable path; want nil")
	}
	if err == nil {
		t.Fatal("Open on an uncreatable path succeeded; want error")
	}
	if !strings.Contains(err.Error(), "create db file") {
		t.Errorf("error %q missing phase wrap %q", err, "create db file")
	}
}
