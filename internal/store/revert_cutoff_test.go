package store

import (
	"context"
	"testing"
	"time"
)

func TestLatestOutcomeByIssue_CutoffIncludesOnlyOutcomesAtOrBeforeIt(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := db.db.ExecContext(ctx, `INSERT INTO outcomes (developer, issue_id, weight, quality)
		VALUES ('default', 'issue-12', 3, 1)`); err != nil {
		t.Fatal(err)
	}
	var defaultTS string
	if err := db.db.QueryRowContext(ctx, `SELECT CAST(ts AS TEXT) FROM outcomes`).Scan(&defaultTS); err != nil {
		t.Fatal(err)
	}
	base, err := time.Parse("2006-01-02 15:04:05", defaultTS)
	if err != nil {
		t.Fatal(err)
	}
	exact := base.Add(100 * time.Millisecond)
	for i, dev := range []string{"exact", "future"} {
		if _, err := db.InsertOutcome(ctx, Outcome{
			Developer: dev, IssueID: "issue-12", Weight: 3, Quality: 1,
			Timestamp: exact.Add(time.Duration(i) * time.Nanosecond),
		}); err != nil {
			t.Fatal(err)
		}
	}
	var stored, bound string
	if err := db.db.QueryRowContext(ctx, `SELECT CAST(ts AS TEXT), CAST(? AS TEXT)
		FROM outcomes WHERE developer = 'exact'`, exact.UTC()).Scan(&stored, &bound); err != nil {
		t.Fatal(err)
	}
	t.Logf("SQLite default ts=%q; Go sub-second ts=%q; UTC-bound cutoff=%q", defaultTS, stored, bound)
	if stored != exact.UTC().String() || bound != stored {
		t.Fatalf("timestamp representations differ: stored=%q bound=%q", stored, bound)
	}
	for _, tc := range []struct {
		name   string
		cutoff time.Time
		want   string
	}{
		{"LatestAtOrBeforeCutoffExcludesSubsecondOutcomeAfterIt", exact.Add(-time.Nanosecond), "default"},
		{"IncludesOutcomeExactlyAtCutoffWithNonUTCOffset", exact.In(time.FixedZone("offset", -4*60*60)), "exact"},
		{"ReturnsNotFoundWhenEveryOutcomeIsAfterCutoff", base.Add(-time.Nanosecond), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := db.LatestOutcomeByIssue(ctx, "", "issue-12", tc.cutoff)
			if err != nil || ok != (tc.want != "") || got.Developer != tc.want {
				t.Fatalf("cutoff %s: developer=%q ok=%v err=%v, want %q", tc.cutoff, got.Developer, ok, err, tc.want)
			}
		})
	}
}
