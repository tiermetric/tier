package api

// Tests for the #187 work-type segmentation on GET /api/v1/scores: the response
// groups scores by work_type, ?work_type filters (fail-loud on an invalid value),
// each segment denominates TIER on per-(developer, issue) cost, and the whole thing
// composes with team-aggregation mode (#185) WITHOUT re-exposing an individual a
// team-mode k-anonymity floor would suppress.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// seedTypedOutcome seeds a cost-bearing outcome of a specific work_type (source
// 'label'), so /scores has a categorized row with an attributable per-issue cost.
func seedTypedOutcome(t *testing.T, db *store.DB, dev, issue string, weight, cost float64, workType string) {
	t.Helper()
	seedCosts(t, db, dev, issue, cost)
	if _, err := db.InsertOutcome(context.Background(), store.Outcome{
		Developer:      dev,
		IssueID:        issue,
		Weight:         weight,
		Quality:        1.0,
		WorkType:       workType,
		WorkTypeSource: store.WorkTypeSourceLabel,
		MergeCommitSHA: "sha-" + dev + "-" + issue,
		Timestamp:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertOutcome(%s,%s,%s): %v", dev, issue, workType, err)
	}
}

func getScores(t *testing.T, h *Handler, target string) scoresResponse {
	t.Helper()
	code, body := doRequest(t, h, http.MethodGet, target, nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body = %s", target, code, body)
	}
	var resp scoresResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal scores: %v; body = %s", err, body)
	}
	return resp
}

func segmentByType(segs []workTypeSegmentJSON, wt string) (workTypeSegmentJSON, bool) {
	for _, s := range segs {
		if s.WorkType == wt {
			return s, true
		}
	}
	return workTypeSegmentJSON{}, false
}

// TestGetScores_SegmentsByWorkType pins the core #187 behavior: outcomes are
// grouped into per-type segments and each segment's TIER denominator is only the
// cost of THAT category's issues, not the developer's whole-window cost.
func TestGetScores_SegmentsByWorkType(t *testing.T) {
	h, db := newTestHandler(t)
	// alice: one security issue ($0.10) and one feature issue ($0.20).
	seedTypedOutcome(t, db, "alice", "issue-sec", 3, 0.10, store.WorkTypeSecurity)
	seedTypedOutcome(t, db, "alice", "issue-feat", 5, 0.20, store.WorkTypeFeature)
	// bob: one security issue.
	seedTypedOutcome(t, db, "bob", "issue-secb", 3, 0.10, store.WorkTypeSecurity)

	resp := getScores(t, h, "/api/v1/scores")

	// Segments present and sorted (feature, security).
	if len(resp.WorkTypes) != 2 {
		t.Fatalf("want 2 segments, got %d: %+v", len(resp.WorkTypes), resp.WorkTypes)
	}
	if resp.WorkTypes[0].WorkType != store.WorkTypeFeature || resp.WorkTypes[1].WorkType != store.WorkTypeSecurity {
		t.Errorf("segments not sorted: got %q, %q", resp.WorkTypes[0].WorkType, resp.WorkTypes[1].WorkType)
	}

	sec, ok := segmentByType(resp.WorkTypes, store.WorkTypeSecurity)
	if !ok {
		t.Fatal("no security segment")
	}
	// security segment holds alice + bob only.
	got := map[string]developerScoreJSON{}
	for _, d := range sec.Developers {
		got[d.Developer] = d
	}
	if len(got) != 2 {
		t.Fatalf("security segment devs = %d, want 2", len(got))
	}
	// alice's security cost is issue-sec ($0.10) ONLY — not her pooled $0.30.
	if a := got["alice"]; a.TotalCostUSD < 0.099 || a.TotalCostUSD > 0.101 {
		t.Errorf("alice security cost = %v, want ~0.10 (per-type attribution)", a.TotalCostUSD)
	}
	if a := got["alice"]; a.WeightedPoints != 3.0 {
		t.Errorf("alice security points = %v, want 3.0", a.WeightedPoints)
	}

	feat, ok := segmentByType(resp.WorkTypes, store.WorkTypeFeature)
	if !ok {
		t.Fatal("no feature segment")
	}
	fgot := map[string]developerScoreJSON{}
	for _, d := range feat.Developers {
		fgot[d.Developer] = d
	}
	if a := fgot["alice"]; a.TotalCostUSD < 0.199 || a.TotalCostUSD > 0.201 {
		t.Errorf("alice feature cost = %v, want ~0.20 (per-type attribution)", a.TotalCostUSD)
	}
	if _, ok := fgot["bob"]; ok {
		t.Errorf("bob has no feature work but appears in the feature segment")
	}
}

// TestGetScores_WorkTypeFilter pins ?work_type: a valid value restricts the
// segments to that one type; an invalid value is a fail-loud 400.
func TestGetScores_WorkTypeFilter(t *testing.T) {
	h, db := newTestHandler(t)
	seedTypedOutcome(t, db, "alice", "issue-sec", 3, 0.10, store.WorkTypeSecurity)
	seedTypedOutcome(t, db, "alice", "issue-feat", 5, 0.20, store.WorkTypeFeature)

	resp := getScores(t, h, "/api/v1/scores?work_type=security")
	if len(resp.WorkTypes) != 1 || resp.WorkTypes[0].WorkType != store.WorkTypeSecurity {
		t.Fatalf("filtered response should carry only the security segment; got %+v", resp.WorkTypes)
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?work_type=bogus", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid work_type: status = %d, want 400; body = %s", code, body)
	}
	if !strings.Contains(string(body), "work_type") {
		t.Errorf("400 body should name work_type; got %s", body)
	}
}

// TestGetScores_WorkTypeSegments_TeamMode_NoNameLeak is the flagship #185 × #187
// composition: in team-aggregation mode every per-type segment carries k-anonymized
// TEAM aggregates, never an individual name, and a sub-k cohort within a type
// collapses into "other" rather than being re-exposed by the segmentation.
func TestGetScores_WorkTypeSegments_TeamMode_NoNameLeak(t *testing.T) {
	// k=3 is the hard minimum floor (#185): a named cohort needs >= 3 contributors.
	h, db := newTeamModeHandler(t, 3)
	// Team "eng": alice + bob + carol all do security work (>= k=3 -> named team).
	seedTypedOutcome(t, db, "alice", "issue-a", 3, 0.10, store.WorkTypeSecurity)
	seedTypedOutcome(t, db, "bob", "issue-b", 5, 0.10, store.WorkTypeSecurity)
	seedTypedOutcome(t, db, "carol", "issue-c", 4, 0.10, store.WorkTypeSecurity)
	for _, dev := range []string{"alice", "bob", "carol"} {
		if err := db.UpsertHierarchy(context.Background(), dev, "eng", "div", "org"); err != nil {
			t.Fatalf("hierarchy %s: %v", dev, err)
		}
	}
	// Team "solo": loner alone does security work (< k -> collapses to "other").
	seedTypedOutcome(t, db, "loner", "issue-l", 3, 0.10, store.WorkTypeSecurity)
	if err := db.UpsertHierarchy(context.Background(), "loner", "solo", "div", "org"); err != nil {
		t.Fatalf("hierarchy loner: %v", err)
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores: %d body=%s", code, body)
	}
	var resp scoresResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	sec, ok := segmentByType(resp.WorkTypes, store.WorkTypeSecurity)
	if !ok {
		t.Fatal("no security segment in team mode")
	}
	// No individual names anywhere in the segment.
	if len(sec.Developers) != 0 {
		t.Errorf("team-mode segment must carry no developer rows; got %d", len(sec.Developers))
	}
	segBytes, _ := json.Marshal(sec)
	for _, name := range []string{"alice", "bob", "carol", "loner"} {
		if strings.Contains(string(segBytes), name) {
			t.Errorf("team-mode security segment leaked individual name %q: %s", name, segBytes)
		}
	}
	// The named team is present; the sub-k solo cohort collapsed to "other".
	teamNames := map[string]bool{}
	for _, ts := range sec.Teams {
		teamNames[ts.Team] = true
	}
	if !teamNames["eng"] {
		t.Errorf("expected named 'eng' team in security segment; got %v", teamNames)
	}
	if teamNames["solo"] {
		t.Errorf("sub-k 'solo' team must NOT appear as a named cohort; got %v", teamNames)
	}
	if !teamNames[scoring.OtherCohort] {
		t.Errorf("sub-k cohort must collapse into %q; got %v", scoring.OtherCohort, teamNames)
	}
}
