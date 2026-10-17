package store

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/qualityrule"
)

func TestRevertAmbiguous_IdempotentDurable(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	id := seedOutcomeForQuality(t, db, "carol", "issue-42", "merge")
	ctx := context.Background()
	e := QualityEvent{OutcomeID: id, Developer: "carol", IssueID: "issue-42", EventType: "revert_ambiguous", SourceRef: "revert-sha", Reason: "ambiguous", EventTS: time.Now().UTC()}
	for i := 0; i < 2; i++ {
		inserted, err := db.AppendQualityEvent(ctx, e)
		if err != nil || inserted != (i == 0) {
			t.Fatalf("append %d inserted=%v err=%v", i, inserted, err)
		}
	}
	events, err := db.QualityEventsForOutcome(ctx, id)
	if err != nil || len(events) != 1 {
		t.Fatalf("readback=%+v err=%v", events, err)
	}
	o, _, err := db.OutcomeByMergeCommit(ctx, "merge")
	if err != nil || o.Quality != 1 || o.QualityStatus != "unknown" {
		t.Fatalf("stored quality=%v err=%v", o.Quality, err)
	}
}

func TestRevertAmbiguous_EraseBetweenLookupAndAppend(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	ts := time.Now().UTC()
	for i, dev := range []string{"carol", "dave"} {
		if _, err := db.InsertOutcome(ctx, Outcome{Developer: dev, IssueID: "issue-42", Repo: "acme/app", PRNumber: i + 1, Timestamp: ts, Weight: 1, Quality: 1}); err != nil {
			t.Fatal(err)
		}
	}
	candidates, _, err := db.RevertIssueCandidates(ctx, "acme/app", "issue-42", ts.Add(-time.Hour), ts)
	if err != nil || len(candidates) != 2 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	// The handler still holds both candidates after this erasure commits.
	counts, err := db.EraseDeveloper(ctx, "carol")
	if err != nil || counts["outcomes"] != 1 {
		t.Fatalf("erase counts=%v err=%v", counts, err)
	}
	for _, candidate := range candidates {
		inserted, err := db.AppendQualityEvent(ctx, QualityEvent{OutcomeID: candidate.ID, Developer: candidate.Developer, IssueID: candidate.IssueID, EventType: "revert_ambiguous", SourceRef: "revert-sha", Reason: "ambiguous", EventTS: ts})
		if err != nil || inserted != (candidate.Developer == "dave") {
			t.Errorf("append %s inserted=%v err=%v", candidate.Developer, inserted, err)
		}
	}
	if n := countDeveloperRows(t, db, "quality_events", "carol"); n != 0 {
		t.Errorf("erased developer has %d orphan quality events", n)
	}
	exp, err := db.ExportDeveloper(ctx, "carol")
	if err != nil || len(exp.QualityEvents) != 0 {
		t.Errorf("erased developer export quality events=%+v err=%v", exp.QualityEvents, err)
	}
	if n := countDeveloperRows(t, db, "quality_events", "dave"); n != 1 {
		t.Errorf("surviving developer has %d quality events, want 1", n)
	}
}

func TestRevertAmbiguous_RequiresOutcomeOwner(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	id := seedOutcomeForQuality(t, db, "dave", "issue-42", "merge")
	inserted, err := db.AppendQualityEvent(context.Background(), QualityEvent{OutcomeID: id, Developer: "carol", IssueID: "issue-42", EventType: "revert_ambiguous", SourceRef: "revert-sha", Reason: "ambiguous", EventTS: time.Now().UTC()})
	if err != nil || inserted {
		t.Fatalf("mismatched owner inserted=%v err=%v", inserted, err)
	}
	if n := countDeveloperRows(t, db, "quality_events", "carol"); n != 0 {
		t.Fatalf("mismatched owner has %d quality events", n)
	}
}

func TestRevertAmbiguous_EvidenceAndDistinctSources(t *testing.T) {
	events := []QualityEvent{
		{OutcomeID: 1, EventType: "revert_ambiguous", SourceRef: "same-revert"},
		{OutcomeID: 2, EventType: "revert_ambiguous", SourceRef: "same-revert"},
		{OutcomeID: 2, EventType: "revert_ambiguous", SourceRef: "second-revert"},
		{OutcomeID: 3, EventType: "future_type", SourceRef: "not-evidence"},
	}
	for _, e := range events {
		if e.IsEvidence() || e.Status() != "unknown" {
			t.Fatalf("fail-open event classification: %+v", e)
		}
	}
	if got := DistinctAmbiguousReverts(events); got != 2 {
		t.Fatalf("distinct reverts=%d want 2", got)
	}
}

func TestRevertUnknownSQL_EvidenceAllowlistMatches(t *testing.T) {
	sqlEvidence := map[string]bool{}
	for _, match := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(revertUnknownSQL, -1) {
		if match[1] != "unknown" {
			sqlEvidence[match[1]] = true
		}
	}
	types := map[string]bool{"future_type": true}
	for eventType := range validQualityEventTypes {
		types[eventType] = true
	}
	for eventType := range sqlEvidence {
		types[eventType] = true
	}
	for eventType := range types {
		if got, want := sqlEvidence[eventType], qualityrule.IsEvidence(eventType); got != want {
			t.Errorf("SQL evidence[%q]=%v, IsEvidence=%v", eventType, got, want)
		}
	}
}
