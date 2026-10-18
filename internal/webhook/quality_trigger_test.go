package webhook

import (
	"context"
	"github.com/tiermetric/tier/internal/store"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestWorkflowScheduledFailureDoesNotPenalise(t *testing.T) {
	st := newFakeStore()
	seedMergedOutcome(t, st)
	p := wfRunPayload(wfMergeSHA, "main", "failure", 1, time.Now().UTC())
	p["workflow_run"].(map[string]any)["event"] = "schedule"
	if got := doWorkflowRun(t, newTestHandler(st), p, "scheduled-failure"); got != http.StatusNoContent {
		t.Fatal(got)
	}
	if q, _ := st.qualityBySHA(wfMergeSHA); q != 1 {
		t.Fatalf("scheduled failure penalised unrelated merge: quality=%v", q)
	}
}

func TestWorkflowTriggerAllowlistAndAudit(t *testing.T) {
	for _, trigger := range []string{"push", "schedule", "workflow_dispatch", "merge_group", "pull_request", "pull_request_target", "release", "workflow_run", "repository_dispatch", "bad\ntrigger", ""} {
		t.Run(trigger, func(t *testing.T) {
			st := newFakeStore()
			seedMergedOutcome(t, st)
			p := wfRunPayload(wfMergeSHA, "main", "failure", 1, time.Now().UTC())
			p["workflow_run"].(map[string]any)["event"] = trigger
			if code := doWorkflowRun(t, newTestHandler(st), p, "trigger"); code != http.StatusNoContent {
				t.Fatal(code)
			}
			o, _, _ := st.OutcomeByMergeCommit(context.Background(), wfMergeSHA)
			events, err := st.QualityEventsForOutcome(context.Background(), o.ID)
			if err != nil || len(events) != 1 {
				t.Fatalf("%+v %v", events, err)
			}
			want := trigger
			if trigger == "bad\ntrigger" || trigger == "" {
				want = "other"
			}
			if events[0].TriggerEvent == nil || *events[0].TriggerEvent != want {
				t.Fatalf("%+v", events[0])
			}
			q, _ := st.qualityBySHA(wfMergeSHA)
			if (trigger == "push" && q != 0.7) || (trigger != "push" && q != 1) {
				t.Fatal(q)
			}
		})
	}
}

func TestLegacyCIKeepsEffectAndIsFlagged(t *testing.T) {
	st := newFakeStore()
	seedMergedOutcome(t, st)
	ctx := context.Background()
	o, _, _ := st.OutcomeByMergeCommit(ctx, wfMergeSHA)
	if _, err := st.AppendQualityEvent(ctx, store.QualityEvent{OutcomeID: o.ID, Developer: o.Developer, IssueID: o.IssueID, EventType: "ci_fail", SourceRef: wfMergeSHA + ":1:42", EventTS: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateQualityForOutcome(ctx, o.ID, 0.7, "ci_fail", wfMergeSHA+":1:42"); err != nil {
		t.Fatal(err)
	}
	p := wfRunPayload(wfMergeSHA, "main", "failure", 1, time.Now().UTC())
	p["workflow_run"].(map[string]any)["event"] = "schedule"
	if code := doWorkflowRun(t, newTestHandler(st), p, "legacy-plus-schedule"); code != http.StatusNoContent {
		t.Fatal(code)
	}
	p = wfRunPayload(wfMergeSHA, "main", "success", 1, time.Now().UTC())
	p["workflow_run"].(map[string]any)["workflow_id"] = 99
	if code := doWorkflowRun(t, newTestHandler(st), p, "legacy-plus-other-push"); code != http.StatusNoContent {
		t.Fatal(code)
	}
	if q, _ := st.qualityBySHA(wfMergeSHA); q != 0.7 {
		t.Fatalf("legacy silently re-scored: %v", q)
	}
	events, err := st.QualityEventsForOutcome(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Status() != "CI trigger unknown" || events[0].IsEvidence() {
		t.Fatalf("legacy not flagged: %+v", events[0])
	}
}

func TestScheduledRunPreservesExistingQuality(t *testing.T) {
	st := newFakeStore()
	seedMergedOutcome(t, st)
	o, _, _ := st.OutcomeByMergeCommit(context.Background(), wfMergeSHA)
	if err := st.UpdateQualityForOutcome(context.Background(), o.ID, 0.5, "manual", "fixture"); err != nil {
		t.Fatal(err)
	}
	p := wfRunPayload(wfMergeSHA, "main", "failure", 1, time.Now().UTC())
	p["workflow_run"].(map[string]any)["event"] = "schedule"
	if code := doWorkflowRun(t, newTestHandler(st), p, "schedule-existing"); code != http.StatusNoContent {
		t.Fatal(code)
	}
	if q, _ := st.qualityBySHA(wfMergeSHA); q != 0.5 {
		t.Fatalf("scheduled audit changed existing quality: %v", q)
	}
}

func TestWorkflowScheduleFirstThenPushFailurePenalises(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "quality.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.InsertOutcome(ctx, store.Outcome{Developer: "alice", IssueID: "issue-42", Repo: "acme/app", Weight: 3, Quality: 1, MergeCommitSHA: wfMergeSHA, Timestamp: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(db)
	for i, trigger := range []string{"schedule", "push", "push", "schedule"} {
		p := wfRunPayload(wfMergeSHA, "main", "failure", 1, time.Now().UTC())
		p["workflow_run"].(map[string]any)["event"] = trigger
		if code := doWorkflowRun(t, h, p, "collision-"+trigger+strconv.Itoa(i)); code != http.StatusNoContent {
			t.Fatal(code)
		}
		o, _, err := db.OutcomeByMergeCommit(ctx, wfMergeSHA)
		if err != nil {
			t.Fatal(err)
		}
		want := 0.7
		if i == 0 {
			want = 1
		}
		if o.Quality != want {
			t.Fatalf("%s: quality=%v, want %v", trigger, o.Quality, want)
		}
	}
	o, _, _ := db.OutcomeByMergeCommit(ctx, wfMergeSHA)
	events, err := db.QualityEventsForOutcome(ctx, o.ID)
	if err != nil || len(events) != 1 || events[0].TriggerEvent == nil || *events[0].TriggerEvent != "push" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestWorkflowPushCollisionUsesPushFailureTimeForFlakyRerun(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "quality.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	base := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	if _, err := db.InsertOutcome(ctx, store.Outcome{Developer: "alice", IssueID: "issue-42", Repo: "acme/app", Weight: 3, Quality: 1, MergeCommitSHA: wfMergeSHA, Timestamp: base}); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(db)
	for i, run := range []struct {
		trigger, conclusion string
		attempt, minute     int
		quality             float64
	}{
		{"schedule", "failure", 1, 0, 1},
		{"push", "failure", 1, 60, 0.7},
		{"push", "success", 2, 70, 1},
	} {
		p := wfRunPayload(wfMergeSHA, "main", run.conclusion, run.attempt, base.Add(time.Duration(run.minute)*time.Minute))
		p["workflow_run"].(map[string]any)["event"] = run.trigger
		if code := doWorkflowRun(t, h, p, "collision-flaky-"+strconv.Itoa(i)); code != http.StatusNoContent {
			t.Fatal(code)
		}
		o, ok, err := db.OutcomeByMergeCommit(ctx, wfMergeSHA)
		if err != nil || !ok {
			t.Fatalf("outcome: found=%v err=%v", ok, err)
		}
		if o.Quality != run.quality {
			t.Errorf("minute %d: quality=%v, want %v", run.minute, o.Quality, run.quality)
		}
	}
	o, _, _ := db.OutcomeByMergeCommit(ctx, wfMergeSHA)
	events, err := db.QualityEventsForOutcome(ctx, o.ID)
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	if !events[0].EventTS.Equal(base.Add(time.Hour)) {
		t.Errorf("failure timestamp=%s, want push failure at minute 60", events[0].EventTS)
	}
	if events[1].EventType != "ci_fail_flaky" {
		t.Errorf("rerun type=%s, want ci_fail_flaky", events[1].EventType)
	}
}
