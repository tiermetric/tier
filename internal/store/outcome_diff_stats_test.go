package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// The optional interface lets this regression compile before nullable evidence
// exists: the old Outcome exposes only numeric zeros, hence unavailable stats.
func nullableDiffStats(o Outcome) [3]*int {
	if v, ok := any(o).(interface{ DiffStats() (*int, *int, *int) }); ok {
		a, d, f := v.DiffStats()
		return [3]*int{a, d, f}
	}
	return [3]*int{}
}

func TestOutcomeDiffStats_AbsentNullAndZeroRemainDistinct(t *testing.T) {
	ctx := context.Background()
	ts := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		stats [3]any
	}{
		{"absent", [3]any{}}, {"null", [3]any{}},
		{"zero", [3]any{0, 0, 0}}, {"partial", [3]any{42, nil, 0}},
		{"present", [3]any{200, 40, 6}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			query := `INSERT INTO outcomes (developer, issue_id, weight, quality, merge_commit_sha, repo, ts, additions, deletions, changed_files) VALUES ('alice', 'issue-42', 3, 1, 'merge', 'acme/app', ?, ?, ?, ?)`
			args := []any{ts, tc.stats[0], tc.stats[1], tc.stats[2]}
			if tc.name == "absent" {
				query = `INSERT INTO outcomes (developer, issue_id, weight, quality, merge_commit_sha, repo, ts) VALUES ('alice', 'issue-42', 3, 1, 'merge', 'acme/app', ?)`
				args = args[:1]
			}
			if _, err := db.db.ExecContext(ctx, query, args...); err != nil {
				t.Fatal(err)
			}
			check := func(name string, o Outcome) {
				t.Helper()
				got := nullableDiffStats(o)
				values := [3]int{o.Additions, o.Deletions, o.ChangedFiles}
				for i, want := range tc.stats {
					if want == nil {
						if got[i] != nil || values[i] != 0 {
							t.Errorf("%s stat %d: got %v / %d, want unavailable / 0", name, i, got[i], values[i])
						}
					} else if got[i] == nil || *got[i] != want.(int) || values[i] != want.(int) {
						t.Errorf("%s stat %d: got %v / %d, want available / %d", name, i, got[i], values[i], want)
					}
				}
				// Read -> insert must retain each SQL NULL, including partial evidence.
				o.MergeCommitSHA = "roundtrip-" + name
				if _, err := db.InsertOutcome(ctx, o); err != nil {
					t.Fatal(err)
				}
				var raw [3]sql.NullInt64
				if err := db.db.QueryRowContext(ctx, `SELECT additions, deletions, changed_files FROM outcomes WHERE merge_commit_sha = ?`, o.MergeCommitSHA).Scan(&raw[0], &raw[1], &raw[2]); err != nil {
					t.Fatal(err)
				}
				for i, want := range tc.stats {
					if raw[i].Valid != (want != nil) || (want != nil && raw[i].Int64 != int64(want.(int))) {
						t.Errorf("%s roundtrip stat %d = %v, want %v", name, i, raw[i], want)
					}
				}
			}
			o, found, err := db.OutcomeByMergeCommit(ctx, "merge")
			if err != nil || !found {
				t.Fatalf("merge lookup: %v, found %v", err, found)
			}
			check("OutcomeByMergeCommit", o)
			o, found, err = db.LatestOutcomeByIssue(ctx, "acme/app", "issue-42", ts)
			if err != nil || !found {
				t.Fatalf("issue lookup: %v, found %v", err, found)
			}
			check("LatestOutcomeByIssue", o)
			list, err := db.DeveloperOutcomes(ctx, "alice", ts)
			if err != nil || len(list) == 0 {
				t.Fatalf("developer read: %v, rows %d", err, len(list))
			}
			check("DeveloperOutcomes", list[0])
			list, err = db.AllOutcomesWindow(ctx, ts, ts.Add(time.Hour), FleetWide)
			if err != nil || len(list) == 0 {
				t.Fatalf("window read: %v, rows %d", err, len(list))
			}
			check("AllOutcomesWindow", list[0])
			if err := db.ReadSnapshot(ctx, func(s *Snapshot) error {
				list, err := s.AllOutcomesWindow(ctx, ts, ts.Add(time.Hour), FleetWide)
				if err != nil {
					return err
				}
				if len(list) == 0 {
					t.Fatal("empty snapshot")
				}
				// Avoid a pooled write while holding the snapshot transaction.
				got := nullableDiffStats(list[0])
				for i, want := range tc.stats {
					if (got[i] != nil) != (want != nil) || (want != nil && *got[i] != want.(int)) {
						t.Errorf("snapshot stat %d = %v, want %v", i, got[i], want)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			list, _, err = db.ListOutcomes(ctx, ts, ts.Add(time.Hour), PageCursor{}, 100)
			if err != nil || len(list) == 0 {
				t.Fatalf("list read: %v, rows %d", err, len(list))
			}
			check("ListOutcomes", list[0])
		})
	}
}
