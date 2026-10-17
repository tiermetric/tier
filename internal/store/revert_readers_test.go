package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestRevertAmbiguous_ReopenScoringAndDigests(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "durable.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ts := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	_, err = db.InsertOutcome(ctx, Outcome{Developer: "carol", IssueID: "issue-42", Repo: "acme/app", MergeCommitSHA: "merge", Timestamp: ts, Weight: 3, Quality: 1})
	if err != nil {
		t.Fatal(err)
	}
	o, _, err := db.OutcomeByMergeCommit(ctx, "merge")
	if err != nil {
		t.Fatal(err)
	}
	since, until := ts.Add(-time.Hour), ts.Add(time.Hour)
	beforeEvents, err := db.EventsDigest(ctx, since, until, FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	beforeOutcomes, err := db.OutcomesDigest(ctx, since, until, FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.AppendQualityEvent(ctx, QualityEvent{OutcomeID: o.ID, Developer: o.Developer, IssueID: o.IssueID, EventType: "revert_ambiguous", SourceRef: "revert", EventTS: ts, Reason: "ambiguous"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	o, _, err = db.OutcomeByMergeCommit(ctx, "merge")
	if err != nil || o.Quality != 1 || o.QualityStatus != "unknown" {
		t.Fatalf("durable readback=%+v err=%v", o, err)
	}
	events, err := db.QualityEventsForOutcome(ctx, o.ID)
	if err != nil || len(events) != 1 || events[0].Reason != "ambiguous" || events[0].IsEvidence() {
		t.Fatalf("durable events=%+v err=%v", events, err)
	}
	// Scoring consumes numeric quality, never presence/count of quality rows.
	scored, err := db.AllOutcomesWindow(ctx, since, until, FleetWide)
	if err != nil || len(scored) != 1 || scored[0].Weight*scored[0].Quality != 3 {
		t.Fatalf("scoring rows=%+v err=%v", scored, err)
	}
	afterEvents, err := db.EventsDigest(ctx, since, until, FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	afterOutcomes, err := db.OutcomesDigest(ctx, since, until, FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	if beforeEvents != afterEvents || beforeOutcomes != afterOutcomes {
		t.Fatal("unresolved check changes report content digests")
	}
	page, _, err := db.ListOutcomes(ctx, since, until, PageCursor{}, 10)
	if err != nil || len(page) != 1 || page[0].QualityStatus != "unknown" {
		t.Fatalf("export outcomes=%+v err=%v", page, err)
	}
}

func TestRevertIssueCandidates_StrictWindowAndLegacy(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ts := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for i, o := range []Outcome{
		{Developer: "target", Repo: "acme/app", Timestamp: ts},
		{Developer: "future", Repo: "acme/app", Timestamp: ts.Add(time.Nanosecond)},
		{Developer: "old", Repo: "acme/app", Timestamp: ts.Add(-60*24*time.Hour - time.Nanosecond)},
		{Developer: "cross-repo", Repo: "acme/other", Timestamp: ts},
		{Developer: "legacy", Timestamp: ts},
	} {
		o.IssueID = "issue-42"
		o.Weight = 1
		o.Quality = 1
		o.PRNumber = i + 1
		if _, err := db.InsertOutcome(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	candidates, legacy, err := db.RevertIssueCandidates(ctx, "ACME/APP", "issue-42", ts.Add(-60*24*time.Hour), ts.In(time.FixedZone("offset", -4*3600)))
	if err != nil || legacy || len(candidates) != 1 || candidates[0].Developer != "target" {
		t.Fatalf("candidates=%+v legacy=%v err=%v", candidates, legacy, err)
	}
	candidates, legacy, err = db.RevertIssueCandidates(ctx, "acme/missing", "issue-42", ts.Add(-60*24*time.Hour), ts)
	if err != nil || !legacy || len(candidates) != 0 {
		t.Fatalf("legacy candidates=%+v legacy=%v err=%v", candidates, legacy, err)
	}
	candidates, _, err = db.RevertIssueCandidates(ctx, "", "issue-42", ts.Add(-60*24*time.Hour), ts)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("repo-blind candidates=%+v err=%v", candidates, err)
	}
}

func TestRevertAmbiguous_UpgradeAndUnknownTypesFailClosed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	id := seedOutcomeForQuality(t, db, "carol", "issue-42", "merge")
	ts := time.Now().UTC()
	_, err = db.AppendQualityEvent(ctx, QualityEvent{OutcomeID: id, Developer: "carol", IssueID: "issue-42", EventType: "ci_pass", SourceRef: "ci", EventTS: ts})
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the pre-#1228 quality_events shape without touching its rows.
	if _, err := db.db.Exec(`ALTER TABLE quality_events DROP COLUMN reason`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	events, err := db.QualityEventsForOutcome(ctx, id)
	if err != nil || len(events) != 1 || events[0].Reason != "" || events[0].EventType != "ci_pass" {
		t.Fatalf("upgrade changed history: %+v %v", events, err)
	}
	// Unknown future/imported types cannot enter through the append allowlist.
	if _, err := db.AppendQualityEvent(ctx, QualityEvent{OutcomeID: id, EventType: "future_type"}); err == nil {
		t.Fatal("unknown event write accepted")
	}
	// Even a raw imported row fails closed on every diagnostic/evidence read.
	if _, err := db.db.Exec(`INSERT INTO quality_events(outcome_id,developer,issue_id,event_type,source_ref,event_ts) VALUES (?,'carol','issue-42','future_type','future',?)`, id, normalizeTS(ts)); err != nil {
		t.Fatal(err)
	}
	events, err = db.QualityEventsForOutcome(ctx, id)
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%+v %v", events, err)
	}
	if events[1].IsEvidence() || events[1].Status() != "unknown" {
		t.Fatal("unknown imported type accepted as evidence")
	}
	o, _, err := db.OutcomeByMergeCommit(ctx, "merge")
	if err != nil || o.Quality != 1 || o.QualityStatus != "unknown" {
		t.Fatalf("unknown imported type fails open: %+v %v", o, err)
	}
}

func TestRevertIssueCandidates_MixedEncodingLowerBoundary(t *testing.T) {
	for _, nanos := range []int{0, 123456789} {
		t.Run(fmt.Sprint(nanos), func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			ctx := context.Background()
			until := time.Date(2026, 10, 14, 12, 0, 0, nanos, time.UTC)
			since := until.Add(-60 * 24 * time.Hour)
			// SQLite-default text and driver-bound time.Time must both count at the boundary.
			for i, dev := range []string{"bob", "carol"} {
				_, err := db.InsertOutcome(ctx, Outcome{Developer: dev, Repo: "acme/app", IssueID: "issue-42", PRNumber: i + 1, Weight: 3, Quality: 1, Timestamp: since})
				if err != nil {
					t.Fatal(err)
				}
			}
			textTS := since.Format("2006-01-02 15:04:05.999999999")
			if _, err := db.db.Exec(`UPDATE outcomes SET ts=? WHERE developer='bob'`, textTS); err != nil {
				t.Fatal(err)
			}
			var bound string
			var included bool
			if err := db.db.QueryRow(`SELECT CAST(? AS TEXT), ? >= ?`, normalizeTS(since), textTS, normalizeTS(since)).Scan(&bound, &included); err != nil {
				t.Fatal(err)
			}
			t.Logf("SQLite: stored=%q original bound=%q included=%v", textTS, bound, included)
			candidates, legacy, err := db.RevertIssueCandidates(ctx, "acme/app", "issue-42", since, until)
			if err != nil || legacy || len(candidates) != 2 {
				t.Fatalf("mixed boundary candidates=%+v legacy=%v err=%v; want both, never a sole penalty target", candidates, legacy, err)
			}
		})
	}
}
