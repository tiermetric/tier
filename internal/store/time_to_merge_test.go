package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestTimeToMergeOutcomesWindowEvidenceAndPredicate(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	until := since.AddDate(0, 1, 0)
	for i, at := range []time.Time{since.Add(-time.Second), since, since.Add(time.Second), until} {
		created, merged := at.Add(-2*time.Hour), at.Add(-time.Hour)
		repo := "acme/app"
		if i == 2 {
			repo = "acme/other"
		}
		o := Outcome{Developer: "dev", IssueID: at.String(), Repo: repo, Weight: 1, Quality: 1, Timestamp: at, CreatedAt: &created, MergedAt: &merged, Source: "backfill"}
		if _, err := db.InsertOutcome(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	for _, scope := range []RepoScope{FleetWide, "acme/app"} {
		var evidence []Outcome
		if err := db.ReadSnapshot(ctx, func(s *Snapshot) error {
			scores, err := s.AllOutcomesWindow(ctx, since, until, scope)
			if err != nil {
				return err
			}
			evidence, err = s.TimeToMergeOutcomesWindow(ctx, since, until, scope)
			if err != nil {
				return err
			}
			if len(scores) != len(evidence) {
				t.Fatalf("scope %s length: %d/%d", scope, len(scores), len(evidence))
			}
			for i, o := range evidence {
				if o.ID != scores[i].ID || o.CreatedAt == nil || o.MergedAt == nil || o.MergedAt.Sub(*o.CreatedAt) != time.Hour {
					t.Fatalf("scope %s evidence: %+v", scope, o)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		pool, err := db.TimeToMergeOutcomesWindow(ctx, since, until, scope)
		if err != nil || !reflect.DeepEqual(evidence, pool) {
			t.Fatalf("pool read differs: %+v %v", pool, err)
		}
	}
}
