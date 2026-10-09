package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestInsertOutcome_SourceProvenance pins the #188 provenance discipline at the
// store boundary: an outcome inserted with an empty Source (the webhook, which
// never sets it) reads back as 'github-webhook', while one that sets
// OutcomeSourceAPI round-trips verbatim. This is the load-bearing guarantee that
// a forged/manual API outcome is distinguishable from a webhook one in audit.
func TestInsertOutcome_SourceProvenance(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "src.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cases := []struct {
		name       string
		source     string
		sha        string
		wantSource string
	}{
		{"empty source coerces to github-webhook", "", "sha-empty", OutcomeSourceGitHubWebhook},
		{"explicit github-webhook round-trips", OutcomeSourceGitHubWebhook, "sha-gh", OutcomeSourceGitHubWebhook},
		{"explicit api-outcome round-trips", OutcomeSourceAPI, "sha-api", OutcomeSourceAPI},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inserted, err := db.InsertOutcome(ctx, Outcome{
				Developer:      "alice",
				IssueID:        "issue-1",
				Weight:         3.0,
				WeightSource:   WeightSourceHeuristic,
				Quality:        1.0,
				MergeCommitSHA: tc.sha,
				Source:         tc.source,
				Timestamp:      time.Now().UTC(),
			})
			if err != nil {
				t.Fatalf("InsertOutcome: %v", err)
			}
			if !inserted {
				t.Fatalf("first insert reported inserted=false, want true")
			}
			got, found, err := db.OutcomeByMergeCommit(ctx, tc.sha)
			if err != nil {
				t.Fatalf("OutcomeByMergeCommit: %v", err)
			}
			if !found {
				t.Fatalf("outcome not found for sha %q", tc.sha)
			}
			if got.Source != tc.wantSource {
				t.Errorf("source = %q, want %q", got.Source, tc.wantSource)
			}
		})
	}
}

// TestInsertOutcome_InsertedFlag pins the #188 dedup signal: the first insert of
// a merge_commit_sha reports inserted=true, a replay reports false (ON CONFLICT
// DO NOTHING), and exactly one row survives.
func TestInsertOutcome_InsertedFlag(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "flag.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	o := Outcome{
		Developer:      "bob",
		IssueID:        "issue-2",
		Weight:         1.0,
		WeightSource:   WeightSourceLabel,
		Quality:        1.0,
		MergeCommitSHA: "sha-dedup",
		Source:         OutcomeSourceAPI,
		Timestamp:      time.Now().UTC(),
	}
	first, err := db.InsertOutcome(ctx, o)
	if err != nil {
		t.Fatalf("first InsertOutcome: %v", err)
	}
	if !first {
		t.Errorf("first insert = false, want true")
	}
	second, err := db.InsertOutcome(ctx, o)
	if err != nil {
		t.Fatalf("second InsertOutcome: %v", err)
	}
	if second {
		t.Errorf("replay insert = true, want false (dedup no-op)")
	}

	all, err := db.AllOutcomesSince(ctx, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AllOutcomesSince: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("rows = %d, want 1", len(all))
	}
}

// TestInsertOutcome_NullSHAAlwaysInserts confirms a NULL/empty merge_commit_sha
// is outside the partial unique index and therefore always reports inserted=true
// (the webhook's tolerated no-SHA case), never colliding with another empty-SHA
// row.
func TestInsertOutcome_NullSHAAlwaysInserts(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "nullsha.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	o := Outcome{Developer: "carol", IssueID: "issue-3", Weight: 1.0, Quality: 1.0, Timestamp: time.Now().UTC()}
	for i := 0; i < 2; i++ {
		inserted, err := db.InsertOutcome(ctx, o)
		if err != nil {
			t.Fatalf("InsertOutcome %d: %v", i, err)
		}
		if !inserted {
			t.Errorf("insert %d = false, want true (empty SHA is unindexed)", i)
		}
	}
}
