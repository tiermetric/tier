package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestHierarchyReimportPersonHistory(t *testing.T) {
	for _, tc := range []struct {
		name, org, start, end, reason string
		wantOpen, wantKept            int
	}{
		{"later elsewhere", "globex", "2025-07", "2025-08", "move", 1, 0},
		{"same month elsewhere", "globex", "2025-06", "2025-07", "move", 1, 0},
		{"short overlap elsewhere", "globex", "2025-02", "2025-03", "move", 0, 1},
		{"short overlap same org", "acme", "2025-02", "2025-03", "move", 0, 1},
		{"later ending overlap same org", "acme", "2025-02", "2025-07", "move", 0, 1},
		{"same end later start", "acme", "2025-02", "2025-06", "move", 0, 1},
		{"open alias supersedes departure", "acme", "2025-06", "", "", 1, 0},
	} {
		for _, reverse := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/forward", true: "/reverse"}[reverse], func(t *testing.T) {
				db, cleanup := newTestDB(t)
				defer cleanup()
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				_, err := db.db.Exec(`INSERT INTO developer_alias (alias, canonical) VALUES ('alias-a','person'), ('alias-b','person')`)
				must(err)
				_, err = db.db.Exec(`INSERT INTO org_hierarchy (developer, team, org) VALUES ('person','eng',?)`, tc.org)
				must(err)
				rows := [][]string{{"alias-a", "acme", "2025-01", "2025-06", "explicit"}, {"alias-b", tc.org, tc.start, tc.end, tc.reason}}
				if reverse {
					rows[0], rows[1] = rows[1], rows[0]
				}
				for _, row := range rows {
					_, err = db.db.Exec(`INSERT INTO period_membership (developer, org, period_start, period_end, ended_reason) VALUES (?, ?, ?, NULLIF(?, ''), NULLIF(?, ''))`, row[0], row[1], row[2], row[3], row[4])
					must(err)
				}
				result, err := db.UpsertHierarchiesWithResult(context.Background(), []HierarchyRow{{Developer: "person", Team: "eng", Org: "acme"}}, "test:fixture")
				must(err)
				var open int
				must(db.db.QueryRow(`SELECT COUNT(*) FROM period_membership WHERE org = 'acme' AND period_end IS NULL`).Scan(&open))
				if open != tc.wantOpen || len(result.KeptDeparted) != tc.wantKept || result.KeptDepartedCount != tc.wantKept {
					t.Errorf("open=%d kept=%v; want open=%d kept=%d", open, result.KeptDeparted, tc.wantOpen, tc.wantKept)
				}
				if len(result.KeptDeparted) == 1 && result.KeptDeparted[0] != (HierarchyImportMember{Developer: "person", Org: "acme"}) {
					t.Errorf("kept-departed=%v; want person/acme", result.KeptDeparted)
				}
			})
		}
	}
}

func TestHierarchyReimportAliasOrgMove(t *testing.T) {
	for _, multiple := range []bool{false, true} {
		t.Run(map[bool]string{false: "one previous org", true: "multiple previous orgs"}[multiple], func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(db.UpsertHierarchy(ctx, "alice-laptop", "eng", "", "acme", "test:fixture"))
			must(db.UpsertDeveloperAlias(ctx, "alice-laptop", "alice", "test:fixture"))
			if multiple {
				must(db.UpsertHierarchy(ctx, "alice-phone", "eng", "", "initech", "test:fixture"))
				must(db.UpsertDeveloperAlias(ctx, "alice-phone", "alice", "test:fixture"))
			}
			must(db.UpsertHierarchy(ctx, "alice", "eng", "", "globex", "test:fixture"))
			var end, reason string
			must(db.db.QueryRow(`SELECT COALESCE(period_end, ''), COALESCE(ended_reason, '') FROM period_membership WHERE developer = 'alice-laptop' AND org = 'acme'`).Scan(&end, &reason))
			if end != db.clock().UTC().Format("2006-01") || reason != "move" {
				t.Errorf("acme end=%q reason=%q; want this month/move", end, reason)
			}
			var open, moved int
			must(db.db.QueryRow(`SELECT COUNT(*) FROM period_membership WHERE period_end IS NULL`).Scan(&open))
			must(db.db.QueryRow(`SELECT COUNT(*) FROM period_membership WHERE ended_reason = 'move'`).Scan(&moved))
			wantMoved := 1
			if multiple {
				wantMoved = 2
			}
			if open != 1 || moved != wantMoved {
				t.Errorf("open=%d moved=%d; want 1/%d", open, moved, wantMoved)
			}
			if got := openStart(t, db, "alice", "globex"); got != db.clock().UTC().Format("2006-01") {
				t.Errorf("globex start=%q; want this month", got)
			}
		})
	}
}

func TestEndMembershipPersonWideStartGuard(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	_, err := db.db.Exec(`INSERT INTO developer_alias (alias, canonical) VALUES ('alias-a', 'person'), ('alias-b', 'person')`)
	if err != nil {
		t.Fatal(err)
	}
	seedMembership(t, db, "alias-a", "acme", "2025-01", "")
	seedMembership(t, db, "alias-b", "acme", "2025-07", "")
	if err := db.EndMembership(ctx, "alias-a", "acme", "2025-06"); !errors.Is(err, ErrEndBeforeStart) {
		t.Fatalf("end before alias-b start: %v; want ErrEndBeforeStart", err)
	}
	var open int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM period_membership WHERE period_end IS NULL`).Scan(&open); err != nil || open != 2 {
		t.Fatalf("open=%d err=%v; want 2 (atomic refusal)", open, err)
	}
	if err := db.EndMembership(ctx, "person", "acme", "2025-07"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.db.Query(`SELECT period_end, ended_reason FROM period_membership`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var end sql.NullString
		var reason string
		if err := rows.Scan(&end, &reason); err != nil {
			t.Fatal(err)
		}
		if end.String != "2025-07" || reason != "explicit" {
			t.Errorf("end=%v reason=%q; want 2025-07/explicit", end, reason)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
