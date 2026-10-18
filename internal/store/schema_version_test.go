package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
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

// TestOpen_V1ToV2UpgradeIsOneWayAndAnnounced pins #886's schema bump: opening a
// version-1 database stamps a version above 1 — which a version-1 binary's
// refuse-if-newer gate rejects — and UpgradeNotice carries the #886 notice (and
// every later one it crossed) on that open only: not on the next open, and not
// on a fresh database.
func TestOpen_V1ToV2UpgradeIsOneWayAndAnnounced(t *testing.T) {
	if schemaVersion < 2 {
		t.Fatalf("schemaVersion = %d, want at least 2 (#886)", schemaVersion)
	}
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := db.UpgradeNotice(); n != "" {
		t.Errorf("fresh database: UpgradeNotice = %q, want empty", n)
	}
	_ = db.Close()
	setUserVersion(t, path, 1)

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if n, want := db.UpgradeNotice(), UpgradeNoticeV886+"\n"+UpgradeNoticeV913+"\n"+UpgradeNoticeV975+"\n"+UpgradeNoticeSealedGap+"\n"+UpgradeNoticeSourceWatermark+"\n"+UpgradeNoticeOutcomeExclusions+"\n"+UpgradeNoticeCorrections+"\n"+UpgradeNoticePricingObservations+"\n"+UpgradeNoticeCorrectionProjection+"\n"+UpgradeNoticeQualityTriggers+"\n"+UpgradeNoticeIssueRule; n != want {
		t.Errorf("v1 open: UpgradeNotice = %q, want every one-way notice %q", n, want)
	}
	_ = db.Close()
	// What a version-1 binary checks before it touches anything (#141): a stored
	// version above its own 1 is refused.
	if v := readUserVersion(t, path); v <= 1 {
		t.Errorf("user_version after the upgrade = %d, want > 1 so a version-1 binary refuses it", v)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if n := db.UpgradeNotice(); n != "" {
		t.Errorf("second open: UpgradeNotice = %q, want empty (printed once)", n)
	}
}

// TestOpen_UnstampedLegacyDBIsAnnounced: the one-way notice covers every
// existing database stored below version 2, including an unstamped pre-#141
// file (user_version 0 with tables), which a fresh file (0, no tables) is told
// apart from (#886).
func TestOpen_UnstampedLegacyDBIsAnnounced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v0.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := db.UpgradeNotice(); n != "" {
		t.Errorf("fresh database: UpgradeNotice = %q, want empty", n)
	}
	_ = db.Close()
	setUserVersion(t, path, 0)

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if n, want := db.UpgradeNotice(), UpgradeNoticeV886+"\n"+UpgradeNoticeV913+"\n"+UpgradeNoticeV975+"\n"+UpgradeNoticeSealedGap+"\n"+UpgradeNoticeSourceWatermark+"\n"+UpgradeNoticeOutcomeExclusions+"\n"+UpgradeNoticeCorrections+"\n"+UpgradeNoticePricingObservations+"\n"+UpgradeNoticeCorrectionProjection+"\n"+UpgradeNoticeQualityTriggers+"\n"+UpgradeNoticeIssueRule; n != want {
		t.Errorf("unstamped legacy open: UpgradeNotice = %q, want every one-way notice %q", n, want)
	}
}

// TestOpen_V2ToV3NoticeOnce pins #913's schema bump: opening a version-2
// database stamps a version above 2 — which a version-2 binary, which would erase
// a person and leave their sealed person keys behind, refuses — and UpgradeNotice
// is UpgradeNoticeV913 and every later notice on that open (not the #886 notice
// it already printed) and empty on the next one.
func TestOpen_V2ToV3NoticeOnce(t *testing.T) {
	if schemaVersion < 3 {
		t.Fatalf("schemaVersion = %d, want at least 3 (#913)", schemaVersion)
	}
	path := filepath.Join(t.TempDir(), "v2.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := db.UpgradeNotice(); n != "" {
		t.Errorf("fresh database: UpgradeNotice = %q, want empty", n)
	}
	_ = db.Close()
	setUserVersion(t, path, 2)

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if n, want := db.UpgradeNotice(), UpgradeNoticeV913+"\n"+UpgradeNoticeV975+"\n"+UpgradeNoticeSealedGap+"\n"+UpgradeNoticeSourceWatermark+"\n"+UpgradeNoticeOutcomeExclusions+"\n"+UpgradeNoticeCorrections+"\n"+UpgradeNoticePricingObservations+"\n"+UpgradeNoticeCorrectionProjection+"\n"+UpgradeNoticeQualityTriggers+"\n"+UpgradeNoticeIssueRule; n != want {
		t.Errorf("v2 open: UpgradeNotice = %q, want %q", n, want)
	}
	_ = db.Close()
	if v := readUserVersion(t, path); v <= 2 {
		t.Errorf("user_version after the upgrade = %d, want > 2 so a version-2 binary refuses it", v)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if n := db.UpgradeNotice(); n != "" {
		t.Errorf("second open: UpgradeNotice = %q, want empty (printed once)", n)
	}
}

func TestOpen_PricingLedgerRefusesOlderBinaries(t *testing.T) {
	for _, version := range []int{7, 8} {
		t.Run(fmt.Sprintf("schema-%d", version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pricing-ledger.db")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			setUserVersion(t, path, version)

			db, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			wantNotice := UpgradeNoticePricingObservations + "\n" + UpgradeNoticeCorrectionProjection + "\n" + UpgradeNoticeQualityTriggers + "\n" + UpgradeNoticeIssueRule
			if version < 8 {
				wantNotice = UpgradeNoticeCorrections + "\n" + wantNotice
			}
			if got := db.UpgradeNotice(); got != wantNotice {
				t.Errorf("upgrade notice = %q, want %q", got, wantNotice)
			}
			_ = db.Close()
			// The refuse-if-newer guard in either older binary must reject it.
			if stored := readUserVersion(t, path); stored <= version {
				t.Fatalf("schema-%d binary would open PII ledger: stored version=%d", version, stored)
			}
		})
	}
}

// restoreMainV11Fixture replays a text `.dump` (with user_version 11 appended) of a
// database created by store.Open from main at
// 9c4d58d261857a7a8e1688a71e0483ec332cf486 (PR #1302), with synthetic Alice
// outcome/token/exclusion rows and one legacy CI event. Unlike restamping the
// latest schema, this fixture has never had issue_rule columns or its cutover.
func restoreMainV11Fixture(t *testing.T, path string) {
	t.Helper()
	fixture, err := os.ReadFile("testdata/main-v11.sql")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(string(fixture)); err != nil {
		_ = raw.Close()
		t.Fatalf("restore main v11 fixture: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readUserVersion(t, path); got != 11 {
		t.Fatalf("main fixture version = %d, want 11", got)
	}
}

func TestOpen_V10AndMainV11ToV12(t *testing.T) {
	if qualityTriggerSchemaVersion != 11 || issueRuleSchemaVersion != 12 || schemaVersion != 12 {
		t.Fatalf("migration versions: quality=%d issue=%d current=%d", qualityTriggerSchemaVersion, issueRuleSchemaVersion, schemaVersion)
	}
	for _, version := range []int{10, 11} {
		t.Run(fmt.Sprintf("v%d-to-v12", version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "main.db")
			restoreMainV11Fixture(t, path)
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if version == 10 {
				if _, err := raw.Exec(`ALTER TABLE quality_events DROP COLUMN trigger_event; DELETE FROM tier_migrations WHERE name='quality-trigger-recording-v1'; PRAGMA user_version=10`); err != nil {
					t.Fatal(err)
				}
			}
			var oldCutover string
			if version == 11 {
				if err := raw.QueryRow(`SELECT applied_at FROM tier_migrations WHERE name=?`, QualityTriggerCutover).Scan(&oldCutover); err != nil {
					t.Fatal(err)
				}
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			want := UpgradeNoticeIssueRule
			if version == 10 {
				want = UpgradeNoticeQualityTriggers + "\n" + want
			}
			if got := db.UpgradeNotice(); got != want {
				t.Fatalf("notice=%q, want %q", got, want)
			}
			if readUserVersion(t, path) != 12 {
				t.Fatal("schema 12 not stamped")
			}
			// Both earlier binaries' refuse-if-newer guards must reject this database.
			for _, supported := range []int{10, 11} {
				if stored := readUserVersion(t, path); stored <= supported {
					t.Fatalf("v%d binary would accept schema %d", supported, stored)
				}
			}
			var cutover string
			if err := db.db.QueryRow(`SELECT applied_at FROM tier_migrations WHERE name=?`, QualityTriggerCutover).Scan(&cutover); err != nil {
				t.Fatal(err)
			}
			if version == 11 && cutover != oldCutover {
				t.Fatal("main's quality cutover moved")
			}
			var trigger sql.NullString
			if err := db.db.QueryRow(`SELECT trigger_event FROM quality_events`).Scan(&trigger); err != nil || trigger.Valid {
				t.Fatalf("legacy CI provenance = %v, err=%v", trigger, err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			before := dumpSchema(t, path)
			db, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if db.UpgradeNotice() != "" || dumpSchema(t, path) != before {
				t.Fatal("reopen changed schema or repeated notice")
			}
			var reopenedCutover string
			if err := db.db.QueryRow(`SELECT applied_at FROM tier_migrations WHERE name=?`, QualityTriggerCutover).Scan(&reopenedCutover); err != nil || reopenedCutover != cutover {
				t.Fatalf("quality cutover changed: %q -> %q, err=%v", cutover, reopenedCutover, err)
			}
		})
	}
}
