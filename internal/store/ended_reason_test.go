package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func assertEndedReason(t *testing.T, db *DB, developer, start, want string) {
	t.Helper()
	var got sql.NullString
	if err := db.db.QueryRow(`SELECT ended_reason FROM period_membership WHERE developer = ? AND period_start = ?`, developer, start).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got.String != want || got.Valid != (want != "") {
		t.Errorf("%s/%s ended_reason = %v, want %q", developer, start, got, want)
	}
}

func assertEndedReasonCheck(t *testing.T, db *DB) {
	t.Helper()
	_, err := db.db.Exec(`INSERT INTO period_membership (developer, org, period_start, period_end, ended_reason) VALUES ('invalid', 'acme', '2025-01', '2025-02', 'other')`)
	if err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("invalid ended_reason: got %v, want CHECK constraint failure", err)
	}
}

func TestEndedReason_LiveClosersNeverWriteUnknown(t *testing.T) {
	for _, action := range []string{"explicit", "move", "clear"} {
		t.Run(action, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			upsertAtMonth(t, db, "alice", "acme")
			assertEndedReason(t, db, "alice", "0000-01", "")
			want := "move"
			switch action {
			case "explicit":
				want = "explicit"
				if err := db.EndMembership(context.Background(), "alice", "acme", "2025-03"); err != nil {
					t.Fatal(err)
				}
			case "move":
				upsertAtMonth(t, db, "alice", "globex")
				assertEndedReason(t, db, "alice", openStart(t, db, "alice", "globex"), "")
			case "clear":
				upsertAtMonth(t, db, "alice", "")
			}
			assertEndedReason(t, db, "alice", "0000-01", want)
			assertEndedReasonCheck(t, db)
		})
	}
}

func TestEndedReason_LegacyMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	// Rewind only the membership table to its pre-#945 shape.
	for _, stmt := range []string{
		`DROP TABLE period_membership`,
		`CREATE TABLE period_membership (developer TEXT NOT NULL, org TEXT NOT NULL, period_start TEXT NOT NULL, period_end TEXT)`,
		`DELETE FROM tier_migrations WHERE name = 'membership_ended_reason_v945'`,
		`INSERT INTO developer_alias (alias, canonical) VALUES ('alias', 'person'), ('open-alias', 'active'), ('older-alias', 'latest')`,
		`INSERT INTO org_hierarchy (developer, team, org) VALUES ('ended','t','acme'), ('moved','t','globex'), ('cleared','t',''), ('alias','t','acme'), ('active','t','acme'), ('latest','t','acme')`,
		`INSERT INTO period_membership VALUES
		 ('ended','acme','2025-01','2025-02'), ('ended','acme','2025-03','2025-04'),
		 ('moved','acme','2025-01','2025-02'), ('moved','globex','2025-02',NULL),
		 ('cleared','acme','2025-01','2025-02'), ('person','acme','2025-01','2025-02'),
		 ('active','acme','2025-01','2025-02'), ('open-alias','acme','2025-03',NULL),
		 ('older-alias','acme','2025-01','2025-02'), ('latest','acme','2025-03','2025-04')`,
	} {
		if _, err := db.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	for boot := 0; boot < 3; boot++ {
		_ = db.Close()
		db, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range []struct{ developer, start, want string }{
			{"ended", "2025-01", "unknown"}, {"ended", "2025-03", "explicit"},
			{"moved", "2025-01", "unknown"}, {"moved", "2025-02", ""},
			{"cleared", "2025-01", "unknown"}, {"person", "2025-01", "explicit"},
			{"active", "2025-01", "unknown"}, {"open-alias", "2025-03", ""},
			{"older-alias", "2025-01", "unknown"}, {"latest", "2025-03", "explicit"},
		} {
			assertEndedReason(t, db, row.developer, row.start, row.want)
		}
		assertEndedReasonCheck(t, db)
		var markers int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM tier_migrations WHERE name = 'membership_ended_reason_v945'`).Scan(&markers); err != nil || markers != 1 {
			t.Fatalf("migration marker count = %d, err = %v", markers, err)
		}
		switch boot {
		case 0:
			// A marked migration must not classify newly introduced NULL rows.
			if _, err := db.db.Exec(`INSERT INTO period_membership (developer, org, period_start, period_end) VALUES ('later','acme','2025-01','2025-02')`); err != nil {
				t.Fatal(err)
			}
		case 1:
			assertEndedReason(t, db, "later", "2025-01", "")
			// Even an unmarked rerun must preserve classified rows after org changes.
			if _, err := db.db.Exec(`DELETE FROM tier_migrations WHERE name = 'membership_ended_reason_v945'; UPDATE org_hierarchy SET org = ''`); err != nil {
				t.Fatal(err)
			}
		default:
			assertEndedReason(t, db, "later", "2025-01", "unknown")
		}
	}
}

func TestEndedReason_EffectivePlacementAndAmbiguousHistory(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	for _, stmt := range []string{
		`DELETE FROM tier_migrations WHERE name = 'membership_ended_reason_v945'`,
		`INSERT INTO developer_alias (alias, canonical) VALUES
		 ('x','p'), ('tie-alias','tie'), ('open-alias','active'),
		 ('a-first','fallback'), ('z-last','fallback'), ('empty-alias','empty'), ('away-alias','returning')`,
		`INSERT INTO org_hierarchy (developer, team, org) VALUES
		 ('x','t','acme'), ('p','t','globex'), ('tie','t','acme'), ('active','t','acme'),
		 ('a-first','t','globex'), ('z-last','t','acme'), ('empty','t',''), ('empty-alias','t','acme'),
		 ('returning','t','acme'), ('moved-elsewhere','t','acme')`,
		`INSERT INTO period_membership (developer, org, period_start, period_end) VALUES
		 ('x','acme','2025-01','2025-02'), ('p','acme','2025-03','2025-04'),
		 ('tie','acme','2025-01','2025-02'), ('tie-alias','acme','2025-01','2025-03'),
		 ('open-alias','acme','0000-01',NULL), ('active','acme','2025-03','2025-04'),
		 ('fallback','acme','2025-01','2025-02'), ('fallback','globex','2025-03','2025-04'),
		 ('empty','acme','2025-01','2025-02'),
		 ('returning','acme','2025-01','2025-02'), ('away-alias','globex','2025-02','2025-03'),
		 ('returning','acme','2025-03','2025-04'), ('away-alias','globex','2025-04','2025-05'),
		 ('moved-elsewhere','acme','2025-01','2025-02'), ('moved-elsewhere','globex','2025-03',NULL)`,
	} {
		if _, err := db.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := backfillMembershipEndedReason(db.db); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ developer, start, want string }{
		{"x", "2025-01", "unknown"}, {"p", "2025-03", "unknown"},
		{"tie", "2025-01", "unknown"}, {"tie-alias", "2025-01", "unknown"},
		{"open-alias", "0000-01", ""}, {"active", "2025-03", "unknown"},
		{"fallback", "2025-01", "unknown"}, {"fallback", "2025-03", "explicit"},
		{"empty", "2025-01", "unknown"},
		{"returning", "2025-01", "unknown"}, {"away-alias", "2025-02", "unknown"},
		{"returning", "2025-03", "unknown"}, {"away-alias", "2025-04", "unknown"},
		{"moved-elsewhere", "2025-01", "unknown"}, {"moved-elsewhere", "2025-03", ""},
	} {
		assertEndedReason(t, db, row.developer, row.start, row.want)
	}
}

func BenchmarkEndedReason_Backfill20K(b *testing.B) {
	db, err := Open(filepath.Join(b.TempDir(), "backfill.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.db.Exec(`
		WITH RECURSIVE people(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM people WHERE n < 20000)
		INSERT INTO org_hierarchy (developer, team, org) SELECT 'person-' || n, 't', 'acme' FROM people;
		INSERT INTO period_membership (developer, org, period_start, period_end)
		SELECT developer, org, '2025-01', '2025-02' FROM org_hierarchy`); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if _, err := db.db.Exec(`DELETE FROM tier_migrations WHERE name = 'membership_ended_reason_v945';
			UPDATE period_membership SET ended_reason = NULL`); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := backfillMembershipEndedReason(db.db); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		var count int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM period_membership WHERE ended_reason = 'explicit'`).Scan(&count); err != nil || count != 20000 {
			b.Fatalf("explicit rows = %d, err = %v", count, err)
		}
	}
}
