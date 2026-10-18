package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

func qualityEvidenceBody(t *testing.T, h *Handler) map[string]any {
	t.Helper()
	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?since=2020-01-01", nil)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		t.Fatal(err)
	}
	e, ok := data["quality_evidence"].(map[string]any)
	if !ok {
		t.Fatalf("missing quality_evidence: %s", body)
	}
	return e
}

func TestQualityEvidenceMixedWeights(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	now := time.Now().UTC()
	push, schedule := "push", "schedule"
	for i, f := range []struct {
		weight  float64
		kind    string
		trigger *string
	}{{5, "ci_fail", &push}, {3, "revert_quality", nil}, {2, "ci_fail", nil}, {7, "ci_pass", &schedule}, {1, "", nil}} {
		issue := []string{"issue-a", "issue-b", "issue-c", "issue-d", "issue-e"}[i]
		q := 1.0
		if f.kind == "ci_fail" {
			q = 0.7
		}
		if f.kind == "revert_quality" {
			q = 0.1
		}
		seedOutcome(t, db, "alice", issue, f.weight, q)
		os, err := db.AllOutcomesWindow(ctx, time.Time{}, time.Time{}, store.RepoScope(""))
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range os {
			if o.IssueID == issue && f.kind != "" {
				if _, err := db.AppendQualityEvent(ctx, store.QualityEvent{OutcomeID: o.ID, Developer: o.Developer, IssueID: issue, EventType: f.kind, TriggerEvent: f.trigger, SourceRef: "sha:1:42", EventTS: now}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	os, err := db.AllOutcomesWindow(ctx, time.Time{}, time.Time{}, store.RepoScope(""))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range os {
		if o.IssueID == "issue-a" {
			for _, ref := range []string{"sha:2:42", "sha:1:99"} {
				if _, err := db.AppendQualityEvent(ctx, store.QualityEvent{OutcomeID: o.ID, Developer: o.Developer, IssueID: o.IssueID, EventType: "ci_pass", TriggerEvent: &push, SourceRef: ref, EventTS: now}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.AppendQualityEvent(ctx, store.QualityEvent{OutcomeID: o.ID, Developer: o.Developer, IssueID: o.IssueID, EventType: "revert_quality", SourceRef: "revert", EventTS: now}); err != nil {
				t.Fatal(err)
			}
		}
	}
	e := qualityEvidenceBody(t, h)
	if e["denominator_weight"] != float64(18) || e["numerator_weight"] != float64(8) || math.Abs(e["quality_evidence_share"].(float64)-8.0/18) > 1e-9 {
		t.Fatal(e)
	}
	if e["trigger_unknown"] != float64(1) || e["completeness"] != "unknown" || e["cutover"] == "" {
		t.Fatal(e)
	}
	if e["ci"].(map[string]any)["matched"] != float64(1) || e["revert"].(map[string]any)["matched"] != float64(2) {
		t.Fatal(e)
	}
	workflows := e["workflows"].([]any)
	if len(workflows) != 2 {
		t.Fatal(e)
	}
	for _, w := range workflows {
		v := w.(map[string]any)
		if v["numerator_weight"] != float64(5) || v["quality_evidence_share"] != 5.0/18 || v["matched"] != float64(1) {
			t.Fatal(e)
		}
	}

}

func TestQualityEvidenceNotShown(t *testing.T) {
	h, db := newTeamModeHandler(t, 2)
	seedPrivateEvidence(t, db)
	e := qualityEvidenceBody(t, h)
	if len(e) != 1 || e["status"] != "not_shown" {
		t.Fatal(e)
	}
}

func TestQualityEvidenceWindowsAtRequestTime(t *testing.T) {
	now := time.Now().UTC()
	win := windowScores{canon: func(s string) string { return s }}
	for i, age := range []time.Duration{time.Hour, 48 * time.Hour, 49 * time.Hour, 60 * 24 * time.Hour, 61 * 24 * time.Hour} {
		win.outcomes = append(win.outcomes, store.Outcome{ID: int64(i + 1), Developer: "alice", Weight: 1, Timestamp: now.Add(-age)})
		win.scoredOutcomes = append(win.scoredOutcomes, scoring.Outcome{Weight: 1, Quality: 1})
	}
	e := evidenceForWindow(win, store.QualityEvidenceRead{Cutover: now}, now, "")
	if e.CI.WindowOpen != 2 || e.Revert.WindowOpen != 4 || e.CI.Matched != 0 || e.Revert.Matched != 0 {
		t.Fatal(e)
	}
}

func TestQualityEvidenceDivisionNotShown(t *testing.T) {
	h, db := newTestHandler(t)
	seedPrivateEvidence(t, db)
	h.aggregation = scoring.AggregationDivision
	e := qualityEvidenceBody(t, h)
	if len(e) != 1 || e["status"] != "not_shown" {
		t.Fatal(e)
	}
}

func TestQualityEvidenceEmptyIsNull(t *testing.T) {
	h, _ := newTestHandler(t)
	e := qualityEvidenceBody(t, h)
	if e["quality_evidence_share"] != nil || e["label"] != "evidence incomplete" || e["cutover"] == "" {
		t.Fatal(e)
	}
}

func TestQualityEvidenceRepoScopeAndPerDeveloper(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	push := "push"
	for i, repo := range []string{"acme/alpha", "acme/beta"} {
		sha := []string{"alpha", "beta"}[i]
		dev := []string{"alice", "bob"}[i]
		if _, err := db.InsertOutcome(ctx, store.Outcome{Developer: dev, IssueID: "issue-1", Repo: repo, Weight: []float64{5, 3}[i], Quality: 1, MergeCommitSHA: sha, Timestamp: time.Now().UTC().Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			o, _, err := db.OutcomeByMergeCommit(ctx, sha)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.AppendQualityEvent(ctx, store.QualityEvent{OutcomeID: o.ID, Developer: dev, IssueID: o.IssueID, EventType: "ci_fail", SourceRef: sha + ":1:42", TriggerEvent: &push, EventTS: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}
	}
	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?since=2020-01-01&repo=acme/alpha", nil)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var scoped scoresResponse
	if err := json.Unmarshal(body, &scoped); err != nil {
		t.Fatal(err)
	}
	e := scoped.QualityEvidence.(map[string]any)
	if e["quality_evidence_share"] != float64(1) || e["denominator_weight"] != float64(5) {
		t.Fatal(e)
	}
	global := qualityEvidenceBody(t, h)
	if global["quality_evidence_share"] != 5.0/8 {
		t.Fatal(global)
	}
	for _, d := range decodeScores(t, h).Developers {
		if d.QualityEvidence == nil || d.QualityEvidence.Cutover == "" {
			t.Fatal(d)
		}
		if d.Developer == "alice" && *d.QualityEvidence.Share != 1 {
			t.Fatal(d)
		}
		if d.Developer == "bob" && *d.QualityEvidence.Share != 0 {
			t.Fatal(d)
		}
	}
}

func seedPrivateEvidence(t *testing.T, db *store.DB) {
	t.Helper()
	id := seedQualityOutcome(t, db, "alice", "issue-privacy", "privacy-sha")
	push := "push"
	o, _, err := db.OutcomeByMergeCommit(context.Background(), "privacy-sha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendQualityEvent(context.Background(), store.QualityEvent{OutcomeID: id, Developer: "alice", IssueID: o.IssueID, EventType: "ci_pass", SourceRef: "privacy-sha:1:42", TriggerEvent: &push, EventTS: o.Timestamp.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

func TestQualityEvidenceUnknownFlagsOnlyFailures(t *testing.T) {
	h, db := newTestHandler(t)
	for i, kind := range []string{"ci_pass", "ci_fail", "ci_fail_flaky"} {
		sha := []string{"pass", "fail", "flaky"}[i]
		id := seedQualityOutcome(t, db, "alice", "issue-"+sha, sha)
		if _, err := db.AppendQualityEvent(context.Background(), store.QualityEvent{OutcomeID: id, Developer: "alice", IssueID: "issue-" + sha, EventType: kind, SourceRef: sha + ":1:42", EventTS: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	e := qualityEvidenceBody(t, h)
	if e["trigger_unknown"] != float64(3) {
		t.Fatal(e)
	}
	flags := e["trigger_unknown_outcomes"].([]any)
	if len(flags) != 2 {
		t.Fatalf("want only failure flags: %+v", flags)
	}
	os, err := db.AllOutcomesWindow(context.Background(), time.Time{}, time.Time{}, store.RepoScope(""))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range os {
		found := false
		for _, f := range flags {
			if f.(map[string]any)["outcome_id"] == float64(o.ID) {
				found = true
			}
		}
		if found != (o.IssueID != "issue-pass") {
			t.Fatalf("outcome=%+v flags=%+v", o, flags)
		}
	}
}
