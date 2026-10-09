package opencode

import (
	"context"
	"testing"
	"time"
)

// S08-5: expanding the repo scope must not resume past the added repo's history.
func TestAuditS08OpencodeAddedRepoBackfills(t *testing.T) {
	withZaiPrices(t)
	first, added := repoDir(t), repoDir(t)
	newer, older := goldenCompleted, goldenCompleted-24*60*60*1000
	db := newFixtureDB(t, dbSpec{Migrations: 7, Rows: []msgRow{
		buildRow(t, msgSpec{ID: "old", SessionID: "added", Cwd: added, Completed: &older, TimeUpdated: older, Tokens: autoTotal(100, 10, 0, 0, 0)}),
		buildRow(t, msgSpec{ID: "new", SessionID: "first", Cwd: first, Completed: &newer, TimeUpdated: newer, Tokens: autoTotal(100, 10, 0, 0, 0)}),
	}})
	cp := &memCheckpoints{}
	makeCollector := func(repos ...RepoTarget) *Collector {
		logger, _ := newTestLogger()
		c, err := New(Config{DBPath: db, Repos: repos, Checkpoints: cp, Logger: logger, Now: func() time.Time { return testNow }})
		if err != nil {
			t.Fatal(err)
		}
		c.loadWatermark(context.Background())
		return c
	}
	c := makeCollector(RepoTarget{Path: first})
	if got := runOnePass(t, c); len(got) != 1 || got[0].SessionID != "first" {
		t.Fatalf("initial scan: %v", got)
	}
	// Unchanged scope still resumes normally, rather than always backfilling.
	same := makeCollector(RepoTarget{Path: first})
	if same.floor() == 0 {
		t.Fatal("unchanged repo scope did not resume")
	}
	expanded := makeCollector(RepoTarget{Path: first}, RepoTarget{Path: added})
	got := runOnePass(t, expanded)
	found := false
	for _, ev := range got {
		if ev.SessionID == "added" {
			found = true
		}
	}
	if !found {
		t.Fatalf("added repo's old row was skipped: %v", got)
	}
	reordered := makeCollector(RepoTarget{Path: added}, RepoTarget{Path: first})
	if reordered.floor() == 0 {
		t.Fatal("reordering repo targets discarded the checkpoint")
	}
	changedSlug := makeCollector(RepoTarget{Path: first, Slug: "owner/renamed"}, RepoTarget{Path: added})
	if changedSlug.floor() != 0 {
		t.Fatal("changed attribution scope reused the old checkpoint")
	}
}
