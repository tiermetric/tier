package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// newTeamModeHandler returns a handler wired for team-aggregation mode (#185)
// with the given k floor, plus its store. Auth is disabled (matches the other
// handler tests); the aggregation gate is orthogonal to auth.
func newTeamModeHandler(t *testing.T, k int) (*Handler, *store.DB) {
	t.Helper()
	h, db := newTestHandler(t)
	h.SetAggregation(scoring.AggregationTeam, k)
	return h, db
}

// seedTeamMember seeds a contributing developer (cost + outcome, tokens above the
// #136 tripwire) enrolled in a team, so it counts toward the k floor.
func seedTeamMember(t *testing.T, db *store.DB, dev, issue, team string, cost, weight float64) {
	t.Helper()
	seedCosts(t, db, dev, issue, cost)
	seedOutcome(t, db, dev, issue, weight, 1.0)
	if err := db.UpsertHierarchy(context.Background(), dev, team, "div", "org"); err != nil {
		t.Fatalf("UpsertHierarchy(%s,%s): %v", dev, team, err)
	}
}

// TestGetScores_TeamMode_NoDeveloperNames proves the flagship privacy guarantee:
// in team mode /scores emits team aggregates and NEVER an individual name, and
// the developers array is empty.
func TestGetScores_TeamMode_NoDeveloperNames(t *testing.T) {
	h, db := newTeamModeHandler(t, 3)
	members := []string{"alice", "bob", "carol"}
	for i, m := range members {
		seedTeamMember(t, db, m, "issue-"+m, "eng", 10, float64(i+3))
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores: %d body=%s", code, body)
	}
	raw := string(body)
	for _, m := range members {
		if strings.Contains(raw, m) {
			t.Errorf("team-mode /scores leaked developer name %q in body:\n%s", m, raw)
		}
	}
	var resp scoresResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Developers) != 0 {
		t.Errorf("team mode must emit no developer rows; got %d", len(resp.Developers))
	}
	if !teamPresent(resp.Teams, "eng") {
		t.Errorf("expected an 'eng' team aggregate; got %v", teamJSONNames(resp.Teams))
	}
	if resp.Total == nil {
		t.Errorf("team mode should still ship the grand total block")
	}
}

// TestGetScores_TeamMode_SubKCollapsesToOther_TotalsPreserved proves a sub-k
// cohort collapses into "other" with its cost/outcomes preserved, and that the
// team-mode grand total equals the developer-mode grand total (no data lost).
func TestGetScores_TeamMode_SubKCollapsesToOther_TotalsPreserved(t *testing.T) {
	// Build a fixture once, read it in developer mode and team mode.
	seed := func(h *Handler, db *store.DB) {
		// team "big": 3 contributing → named.
		seedTeamMember(t, db, "b1", "i-b1", "big", 10, 3)
		seedTeamMember(t, db, "b2", "i-b2", "big", 10, 3)
		seedTeamMember(t, db, "b3", "i-b3", "big", 10, 3)
		// team "small": 2 contributing → collapses to "other".
		seedTeamMember(t, db, "s1", "i-s1", "small", 7, 5)
		seedTeamMember(t, db, "s2", "i-s2", "small", 7, 5)
	}

	// Developer-mode grand total (the ground truth).
	hDev, dbDev := newTestHandler(t)
	seed(hDev, dbDev)
	_, devBody := doRequest(t, hDev, http.MethodGet, "/api/v1/scores", nil)
	var devResp scoresResponse
	if err := json.Unmarshal(devBody, &devResp); err != nil {
		t.Fatalf("dev unmarshal: %v", err)
	}
	if devResp.Total == nil {
		t.Fatalf("developer-mode total missing")
	}

	// Team-mode read of the identical fixture.
	hTeam, dbTeam := newTeamModeHandler(t, 3)
	seed(hTeam, dbTeam)
	_, teamBody := doRequest(t, hTeam, http.MethodGet, "/api/v1/scores", nil)
	var teamResp scoresResponse
	if err := json.Unmarshal(teamBody, &teamResp); err != nil {
		t.Fatalf("team unmarshal: %v", err)
	}

	// "small" must NOT be named; "other" and "big" present.
	if teamPresent(teamResp.Teams, "small") {
		t.Errorf("sub-k team 'small' must not be named; got %v", teamJSONNames(teamResp.Teams))
	}
	if !teamPresent(teamResp.Teams, "other") || !teamPresent(teamResp.Teams, "big") {
		t.Fatalf("expected 'big' + 'other'; got %v", teamJSONNames(teamResp.Teams))
	}
	other := teamByName(teamResp.Teams, "other")
	if other.WeightedPoints != 10 || other.TotalCostUSD != 14 {
		t.Errorf("'other' = points %v cost %v, want points 10 cost 14 (small's s1+s2 preserved, not dropped)",
			other.WeightedPoints, other.TotalCostUSD)
	}

	// Totals preserved: team-mode grand total == developer-mode grand total.
	if teamResp.Total == nil {
		t.Fatalf("team-mode total missing")
	}
	if teamResp.Total.TotalCostUSD != devResp.Total.TotalCostUSD ||
		teamResp.Total.WeightedPoints != devResp.Total.WeightedPoints {
		t.Errorf("team-mode total (cost %v, points %v) != developer-mode total (cost %v, points %v): suppression must not change totals",
			teamResp.Total.TotalCostUSD, teamResp.Total.WeightedPoints,
			devResp.Total.TotalCostUSD, devResp.Total.WeightedPoints)
	}
	// And the sum of the visible team rows must equal the grand total (nothing lost).
	var sumCost, sumPoints float64
	for _, ts := range teamResp.Teams {
		sumCost += ts.TotalCostUSD
		sumPoints += ts.WeightedPoints
	}
	if sumCost != teamResp.Total.TotalCostUSD || sumPoints != teamResp.Total.WeightedPoints {
		t.Errorf("sum of team rows (cost %v, points %v) != grand total (cost %v, points %v)",
			sumCost, sumPoints, teamResp.Total.TotalCostUSD, teamResp.Total.WeightedPoints)
	}
}

// TestGetScores_TeamMode_DeveloperDetailRejected proves /scores/{developer} is
// blanket-rejected in team mode: the same 404 for an existing and a non-existent
// developer (no existence oracle), and the requested name is never echoed back.
func TestGetScores_TeamMode_DeveloperDetailRejected(t *testing.T) {
	h, db := newTeamModeHandler(t, 3)
	seedTeamMember(t, db, "alice", "issue-1", "eng", 10, 3)

	for _, who := range []string{"alice", "does-not-exist"} {
		code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores/"+who, nil)
		if code != http.StatusNotFound {
			t.Errorf("GET /scores/%s in team mode = %d, want 404", who, code)
		}
		if strings.Contains(string(body), who) {
			t.Errorf("team-mode /scores/%s echoed the requested name back: %s", who, body)
		}
	}
}

// TestGetScores_DeveloperMode_Unchanged is the regression guard: the default
// (developer) mode still names developers and ships no teams array.
func TestGetScores_DeveloperMode_Unchanged(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 10)
	seedOutcome(t, db, "alice", "issue-1", 3, 1)

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("GET: %d body=%s", code, body)
	}
	var resp scoresResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Developers) != 1 || resp.Developers[0].Developer != "alice" {
		t.Errorf("developer mode must still name developers; got %+v", resp.Developers)
	}
	if len(resp.Teams) != 0 {
		t.Errorf("developer mode must ship no teams array; got %v", teamJSONNames(resp.Teams))
	}
	// And the per-developer detail endpoint is reachable in developer mode.
	if code, _ := doRequest(t, h, http.MethodGet, "/api/v1/scores/alice", nil); code != http.StatusOK {
		t.Errorf("developer-mode /scores/alice = %d, want 200", code)
	}
}

// TestGetScores_TeamMode_ComposesWithLowSample proves #133 (low-sample
// suppression) and #185 (sub-k identity suppression) compose: a lone low-sample
// developer is BOTH unranked (dev-mode) AND sub-k, so in team mode their data
// lands in "other" (preserved in totals) while their name never appears.
func TestGetScores_TeamMode_ComposesWithLowSample(t *testing.T) {
	h, db := newTeamModeHandler(t, 3)
	// A named k-sized team.
	seedTeamMember(t, db, "b1", "i-b1", "big", 10, 3)
	seedTeamMember(t, db, "b2", "i-b2", "big", 10, 3)
	seedTeamMember(t, db, "b3", "i-b3", "big", 10, 3)
	// A lone developer with a single small outcome: low sample (#133 unranked in
	// developer mode) AND a sub-k cohort of one (#185) → must collapse to "other".
	seedCosts(t, db, "loner", "i-loner", 1.0)
	seedOutcome(t, db, "loner", "i-loner", 1, 1)
	if err := db.UpsertHierarchy(context.Background(), "loner", "solo", "div", "org"); err != nil {
		t.Fatalf("UpsertHierarchy(loner): %v", err)
	}

	_, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if strings.Contains(string(body), "loner") {
		t.Errorf("low-sample lone developer name leaked in team mode: %s", body)
	}
	var resp scoresResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if teamPresent(resp.Teams, "solo") {
		t.Errorf("one-developer team 'solo' must collapse to 'other'; got %v", teamJSONNames(resp.Teams))
	}
	other := teamByName(resp.Teams, "other")
	if other == nil || other.WeightedPoints != 1 || other.TotalCostUSD != 1 {
		t.Errorf("loner's data must be preserved in 'other' (points 1, cost 1); got %+v", other)
	}
}

// TestGetScores_TeamMode_DataQualityCountOnly proves the #136 zero-token signal
// survives in team mode as a name-free aggregate count — the per-developer/issue
// list (which names individuals) is suppressed.
func TestGetScores_TeamMode_DataQualityCountOnly(t *testing.T) {
	h, db := newTeamModeHandler(t, 3)
	// An outcome with no token events → below the #136 tripwire → flagged.
	seedOutcome(t, db, "ghost", "i-ghost", 2, 1)

	_, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if strings.Contains(string(body), "ghost") {
		t.Errorf("team-mode data-quality leaked developer name 'ghost': %s", body)
	}
	var resp scoresResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.DataQuality == nil {
		t.Fatalf("expected a data_quality block for the zero-token outcome")
	}
	if resp.DataQuality.ZeroTokenOutcomeCount != 1 {
		t.Errorf("zero_token_outcome_count = %d, want 1", resp.DataQuality.ZeroTokenOutcomeCount)
	}
	if len(resp.DataQuality.ZeroTokenOutcomes) != 0 {
		t.Errorf("team mode must suppress the named zero-token list; got %d entries", len(resp.DataQuality.ZeroTokenOutcomes))
	}
}

func teamPresent(ts []teamScoreJSON, name string) bool { return teamByName(ts, name) != nil }

func teamByName(ts []teamScoreJSON, name string) *teamScoreJSON {
	for i := range ts {
		if ts[i].Team == name {
			return &ts[i]
		}
	}
	return nil
}

func teamJSONNames(ts []teamScoreJSON) []string {
	out := make([]string, len(ts))
	for i := range ts {
		out[i] = ts[i].Team
	}
	return out
}
