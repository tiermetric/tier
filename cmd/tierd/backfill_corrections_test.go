package main

import (
	"bytes"
	"context"
	"strings"

	"github.com/tiermetric/tier/internal/corrects"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

func TestBackfill_CorrectsRecordedBeforeNoIssueReturn(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	srv := newFakeGitHub(t, []fakePR{{number: 22, body: "Corrects: #11", mergeSHA: "child-merge", login: "alice", mergedAt: "2026-10-01T12:00:00Z"}})
	defer srv.Close()
	gh := &githubClient{http: srv.Client(), baseURL: srv.URL, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	before := time.Now().UTC()
	for i := 0; i < 2; i++ {
		inserted, unattributed, excluded, err := gh.reconstructOutcome(ctx, db, "owner/repo", 22, nil)
		if err != nil || inserted || !unattributed || excluded {
			t.Fatalf("reconstruct: %v %v %v %v", inserted, unattributed, excluded, err)
		}
	}
	d, found, err := db.CorrectionDeclarationByPR(ctx, "owner/repo", 22, "child-merge")
	if err != nil || !found {
		t.Fatalf("declaration lost before no-issue return: %v %v", found, err)
	}
	recorded, ok := d["recorded_at"].(time.Time)
	if !ok || recorded.Before(before) || recorded.After(time.Now().UTC()) || d["provenance"] != "backfill:fetch-time" || d["outcome_id"] != nil {
		t.Fatalf("fetch-time provenance: %+v", d)
	}
	rels, err := db.CorrectionRelationsByPR(ctx, "owner/repo", 22)
	if err != nil || len(rels) != 1 || rels[0]["resolution_reason"] != "missing-parent" {
		t.Fatalf("unresolved reference: %+v %v", rels, err)
	}
	if _, found, err := db.OutcomeByMergeCommit(ctx, "child-merge"); err != nil || found {
		t.Fatalf("invented outcome: %v %v", found, err)
	}
}

func TestBackfillCorrectionReplay_ExistingOutcomeGetsFetchEvidence(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	// Preserve the stored score and primary issue even if the current fetched
	// body no longer attributes an issue and would previously have returned early.
	o := store.Outcome{Developer: "alice", Repo: "owner/repo", PRNumber: 22, MergeCommitSHA: "child-merge", IssueID: "issue-99", Weight: 5, Quality: 0.7, Timestamp: time.Now().UTC()}
	if inserted, err := db.RecordPROutcome(ctx, o); err != nil || !inserted {
		t.Fatalf("seed: %v %v", inserted, err)
	}
	srv := newFakeGitHub(t, []fakePR{{number: 22, body: "Corrects: #11", mergeSHA: "child-merge", login: "alice", mergedAt: "2026-10-01T12:00:00Z"}})
	defer srv.Close()
	gh := &githubClient{http: srv.Client(), baseURL: srv.URL, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for i := 0; i < 2; i++ {
		if inserted, _, _, err := gh.reconstructOutcome(ctx, db, o.Repo, o.PRNumber, nil); err != nil || inserted {
			t.Fatalf("replay: %v %v", inserted, err)
		}
	}
	stored, found, err := db.OutcomeByMergeCommit(ctx, o.MergeCommitSHA)
	if err != nil || !found || stored.Weight != o.Weight || stored.Quality != o.Quality || stored.IssueID != o.IssueID {
		t.Fatalf("replay changed outcome: %+v %v", stored, err)
	}
	d, found, err := db.CorrectionDeclarationByPR(ctx, o.Repo, o.PRNumber, o.MergeCommitSHA)
	if err != nil || !found || d["outcome_id"] != stored.ID || d["provenance"] != "backfill:fetch-time" {
		t.Fatalf("missing replay evidence: %+v %v", d, err)
	}
}

func TestBackfillCorrectionRevert_DeclarationSurvivesExclusion(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	srv := newFakeGitHub(t, []fakePR{{number: 22, body: "Corrects: #11", branch: "revert-11-repair", mergeSHA: "child-merge", login: "alice", mergedAt: "2026-10-01T12:00:00Z"}})
	defer srv.Close()
	gh := &githubClient{http: srv.Client(), baseURL: srv.URL, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		inserted, unattributed, excluded, err := gh.reconstructOutcome(ctx, db, "owner/repo", 22, nil)
		if err != nil || inserted || unattributed || !excluded {
			t.Fatalf("revert: %v %v %v %v", inserted, unattributed, excluded, err)
		}
	}
	d, found, err := db.CorrectionDeclarationByPR(ctx, "owner/repo", 22, "child-merge")
	if err != nil || !found || d["outcome_id"] != nil || d["provenance"] != "backfill:fetch-time" {
		t.Fatalf("excluded declaration: %+v %v", d, err)
	}
	rels, err := db.CorrectionRelationsByPR(ctx, "owner/repo", 22)
	if err != nil || len(rels) != 1 || rels[0]["state"] != "excluded" || rels[0]["resolution_reason"] != "excluded-child" {
		t.Fatalf("excluded relation: %+v %v", rels, err)
	}
}

func TestBackfillCorrectionReplay_ChangedDeclarationContinues(t *testing.T) {
	for _, branch := range []string{"repair", "revert-11-repair"} {
		t.Run(branch, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "tier.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			ctx := context.Background()
			o := store.Outcome{Developer: "alice", Repo: "owner/repo", PRNumber: 22, MergeCommitSHA: "child-merge"}
			id, err := db.RecordCorrectionDeclaration(ctx, o, corrects.Parse("Corrects: #11"), "first evidence", "first-fetch")
			if err != nil {
				t.Fatal(err)
			}
			srv := newFakeGitHub(t, []fakePR{
				{number: 22, body: "Closes #5\nCorrects: #12", branch: branch, mergeSHA: "child-merge", login: "alice", mergedAt: "2026-10-01T12:00:00Z", updatedAt: "2026-10-02T12:00:00Z"},
				{number: 23, body: "Closes #6", mergeSHA: "later-merge", login: "bob", mergedAt: "2026-10-01T12:00:00Z", updatedAt: "2026-10-01T12:00:00Z"},
			})
			defer srv.Close()
			var logs bytes.Buffer
			gh := &githubClient{http: srv.Client(), baseURL: srv.URL, logger: slog.New(slog.NewTextHandler(&logs, nil))}
			stats, err := backfillOutcomes(ctx, gh, db, "owner/repo", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), nil)
			if err != nil {
				t.Fatalf("backfill aborted: %+v %v", stats, err)
			}
			wantInserted, wantExcluded := 2, 0
			if branch != "repair" {
				wantInserted, wantExcluded = 1, 1
			}
			if stats.merged != 2 || stats.inserted != wantInserted || stats.excluded != wantExcluded {
				t.Fatalf("stats=%+v", stats)
			}
			for _, sha := range []string{"child-merge", "later-merge"} {
				outcome, found, err := db.OutcomeByMergeCommit(ctx, sha)
				wantFound := sha == "later-merge" || branch == "repair"
				if err != nil || found != wantFound || (found && outcome.Quality != 1) {
					t.Fatalf("%s outcome=%+v found=%v err=%v", sha, outcome, found, err)
				}
			}
			d, found, err := db.CorrectionDeclarationByPR(ctx, o.Repo, o.PRNumber, o.MergeCommitSHA)
			if err != nil || !found || d["id"] != id || d["supporting_text"] != "first evidence" || d["provenance"] != "first-fetch" {
				t.Fatalf("first evidence lost: %+v %v", d, err)
			}
			rels, err := db.CorrectionRelationsByPR(ctx, o.Repo, o.PRNumber)
			if err != nil || len(rels) != 1 || rels[0]["parent_pr_number"] != int64(11) {
				t.Fatalf("first parents lost: %+v %v", rels, err)
			}
			if branch != "repair" && rels[0]["state"] != "excluded" {
				t.Fatalf("revert not excluded: %+v", rels)
			}
			if !strings.Contains(logs.String(), `pr="\"22\""`) {
				t.Fatalf("missing PR log: %s", logs.String())
			}
		})
	}
}
