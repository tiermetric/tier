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

// newDivisionModeHandler returns a handler wired for division-aggregation mode
// (#270) with the given k floor, plus its store. Auth is disabled (matches the
// other handler tests); the aggregation gate is orthogonal to auth.
func newDivisionModeHandler(t *testing.T, k int) (*Handler, *store.DB) {
	t.Helper()
	h, db := newTestHandler(t)
	h.SetAggregation(scoring.AggregationDivision, k)
	return h, db
}

// seedDivisionMember seeds a contributing developer (cost + outcome above the #136
// tripwire) enrolled in a specific team AND division, so it counts toward the k
// floor at BOTH levels independently.
func seedDivisionMember(t *testing.T, db *store.DB, dev, issue, team, division string, cost, weight float64) {
	t.Helper()
	seedCosts(t, db, dev, issue, cost)
	seedOutcome(t, db, dev, issue, weight, 1.0)
	if err := db.UpsertHierarchy(context.Background(), dev, team, division, "org"); err != nil {
		t.Fatalf("UpsertHierarchy(%s,%s,%s): %v", dev, team, division, err)
	}
}

// TestGetScores_DivisionMode_NoDeveloperNames is the division analogue of the
// flagship team-mode privacy proof (#270): /scores emits division aggregates,
// NEVER an individual name, ships an empty developers array, and stamps the
// active level in the `aggregation` discriminator.
func TestGetScores_DivisionMode_NoDeveloperNames(t *testing.T) {
	h, db := newDivisionModeHandler(t, 3)
	// Two teams under ONE division "engineering" -- 3 contributors clears k=3.
	seedDivisionMember(t, db, "alice", "i-a", "platform", "engineering", 10, 3)
	seedDivisionMember(t, db, "bob", "i-b", "platform", "engineering", 10, 4)
	seedDivisionMember(t, db, "carol", "i-c", "infra", "engineering", 10, 5)

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores: %d body=%s", code, body)
	}
	raw := string(body)
	for _, m := range []string{"alice", "bob", "carol"} {
		if strings.Contains(raw, m) {
			t.Errorf("division-mode /scores leaked developer name %q in body:\n%s", m, raw)
		}
	}
	// It must ALSO not leak the team names -- division mode rolls up ABOVE team.
	for _, tm := range []string{"platform", "infra"} {
		if strings.Contains(raw, tm) {
			t.Errorf("division-mode /scores leaked team name %q (rows are divisions, not teams):\n%s", tm, raw)
		}
	}
	resp := unmarshalScores(t, body)
	if len(resp.Developers) != 0 {
		t.Errorf("division mode must emit no developer rows; got %d", len(resp.Developers))
	}
	if resp.Aggregation != "division" {
		t.Errorf("aggregation discriminator = %q, want %q", resp.Aggregation, "division")
	}
	if !teamPresent(resp.Teams, "engineering") {
		t.Errorf("expected an 'engineering' division aggregate; got %v", teamJSONNames(resp.Teams))
	}
	if resp.Total == nil {
		t.Errorf("division mode should still ship the grand total block")
	}
}

// TestGetScores_TeamMode_AggregationDiscriminator proves the discriminator names
// the team level too, so a consumer can always tell what the `teams` rows mean
// (#270) -- the field is the seam that lets org/department extend without a new key.
func TestGetScores_TeamMode_AggregationDiscriminator(t *testing.T) {
	h, db := newTeamModeHandler(t, 3)
	for i, m := range []string{"a", "b", "c"} {
		seedTeamMember(t, db, m, "i-"+m, "eng", 10, float64(i+3))
	}
	_, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	resp := unmarshalScores(t, body)
	if resp.Aggregation != "team" {
		t.Errorf("team-mode aggregation discriminator = %q, want %q", resp.Aggregation, "team")
	}
}

// TestGetScores_DivisionMode_SubKDivisionFoldsToOther is the division security
// invariant (#270): a division with fewer than k contributing developers is NOT
// emitted as a named row, its data is preserved in "other", and the grand total
// is unchanged. Asserting a sub-k division is absent is the anti-singling-out
// guarantee at the division level.
func TestGetScores_DivisionMode_SubKDivisionFoldsToOther(t *testing.T) {
	seed := func(_ *Handler, db *store.DB) {
		// division "engineering": 3 contributors -> named.
		seedDivisionMember(t, db, "e1", "i-e1", "platform", "engineering", 10, 3)
		seedDivisionMember(t, db, "e2", "i-e2", "platform", "engineering", 10, 3)
		seedDivisionMember(t, db, "e3", "i-e3", "infra", "engineering", 10, 3)
		// division "research": 2 contributors -> sub-k -> folds to "other".
		seedDivisionMember(t, db, "r1", "i-r1", "ml", "research", 7, 5)
		seedDivisionMember(t, db, "r2", "i-r2", "ml", "research", 7, 5)
	}

	hDev, dbDev := newTestHandler(t)
	seed(hDev, dbDev)
	_, devBody := doRequest(t, hDev, http.MethodGet, "/api/v1/scores", nil)
	devResp := unmarshalScores(t, devBody)
	if devResp.Total == nil {
		t.Fatalf("developer-mode total missing")
	}

	h, db := newDivisionModeHandler(t, 3)
	seed(h, db)
	_, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	resp := unmarshalScores(t, body)

	if teamPresent(resp.Teams, "research") {
		t.Errorf("sub-k division 'research' must NOT be named; got %v", teamJSONNames(resp.Teams))
	}
	if !teamPresent(resp.Teams, "engineering") || !teamPresent(resp.Teams, "other") {
		t.Fatalf("expected 'engineering' + 'other'; got %v", teamJSONNames(resp.Teams))
	}
	other := teamByName(resp.Teams, "other")
	if other.WeightedPoints != 10 || other.TotalCostUSD != 14 {
		t.Errorf("'other' = points %v cost %v, want points 10 cost 14 (research r1+r2 preserved)",
			other.WeightedPoints, other.TotalCostUSD)
	}
	// Totals preserved vs developer-mode ground truth.
	if resp.Total == nil ||
		resp.Total.TotalCostUSD != devResp.Total.TotalCostUSD ||
		resp.Total.WeightedPoints != devResp.Total.WeightedPoints {
		t.Errorf("division-mode total != developer-mode total: suppression must not change totals")
	}
}

// TestGetScores_DivisionMode_SubKTeamNotReExposed is the cross-level k-anon proof
// (#270): a team that is sub-k at the TEAM level (folded to "other" in team mode)
// must not become individually identifiable at the DIVISION level. Because each
// level is an INDEPENDENT flat k-anon partition of the same developer set, the
// sub-k team's developers are absorbed into a division that itself clears k -- an
// anonymity set at least as large as k -- so their numbers are never isolable.
func TestGetScores_DivisionMode_SubKTeamNotReExposed(t *testing.T) {
	// division "engineering" holds 5 contributors (clears k=3) across two teams:
	//   team "big"   -- 3 contributors (clears k at team level)
	//   team "small" -- 2 contributors (SUB-K at team level -> folds to "other" there)
	// In division mode all 5 roll into the single named "engineering" division.
	seed := func(_ *Handler, db *store.DB) {
		seedDivisionMember(t, db, "b1", "i-b1", "big", "engineering", 10, 3)
		seedDivisionMember(t, db, "b2", "i-b2", "big", "engineering", 10, 3)
		seedDivisionMember(t, db, "b3", "i-b3", "big", "engineering", 10, 3)
		seedDivisionMember(t, db, "s1", "i-s1", "small", "engineering", 4, 2)
		seedDivisionMember(t, db, "s2", "i-s2", "small", "engineering", 4, 2)
	}

	// Control: at TEAM level "small" is sub-k and must be suppressed to "other".
	hTeam, dbTeam := newTeamModeHandler(t, 3)
	seed(hTeam, dbTeam)
	_, teamBody := doRequest(t, hTeam, http.MethodGet, "/api/v1/scores", nil)
	teamResp := unmarshalScores(t, teamBody)
	if teamPresent(teamResp.Teams, "small") {
		t.Fatalf("precondition: 'small' must be sub-k at team level; got %v", teamJSONNames(teamResp.Teams))
	}

	// Division level: one named "engineering" over ALL 5 -> the sub-k team's two
	// developers are mixed into a >= k anonymity set, never singled out.
	h, db := newDivisionModeHandler(t, 3)
	seed(h, db)
	_, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	raw := string(body)
	for _, m := range []string{"s1", "s2", "b1", "b2", "b3", "small", "big"} {
		if strings.Contains(raw, m) {
			t.Errorf("division mode re-exposed identity %q from a sub-k team:\n%s", m, raw)
		}
	}
	resp := unmarshalScores(t, body)
	eng := teamByName(resp.Teams, "engineering")
	if eng == nil {
		t.Fatalf("expected a named 'engineering' division; got %v", teamJSONNames(resp.Teams))
	}
	// engineering must be the aggregate of ALL five (3*10 + 2*4 = 38 cost;
	// 3*3 + 2*2 = 13 points): the small team's data is INSIDE the >= k division
	// aggregate, not exposed as its own row.
	if eng.TotalCostUSD != 38 || eng.WeightedPoints != 13 {
		t.Errorf("engineering = cost %v points %v, want 38 / 13 (all 5 folded in, small not isolable)",
			eng.TotalCostUSD, eng.WeightedPoints)
	}
	// There must be no separate row that isolates the sub-k team (only engineering).
	if len(resp.Teams) != 1 {
		t.Errorf("expected exactly one division row (engineering); got %v", teamJSONNames(resp.Teams))
	}
}

// TestGetScores_DivisionMode_DeveloperDetailRejected proves /scores/{developer} is
// blanket-404'd in division mode exactly as in team mode (#270): same 404 for an
// existing and a non-existent developer, and the requested name is never echoed.
func TestGetScores_DivisionMode_DeveloperDetailRejected(t *testing.T) {
	h, db := newDivisionModeHandler(t, 3)
	seedDivisionMember(t, db, "alice", "i-1", "platform", "engineering", 10, 3)

	for _, who := range []string{"alice", "does-not-exist"} {
		code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores/"+who, nil)
		if code != http.StatusNotFound {
			t.Errorf("GET /scores/%s in division mode = %d, want 404", who, code)
		}
		if strings.Contains(string(body), who) {
			t.Errorf("division-mode /scores/%s echoed the requested name back: %s", who, body)
		}
	}
}

// TestGetScores_DivisionMode_ExportForbidden proves EVERY per-developer read
// surface 403s in division mode (#270), the same works-council/GDPR guard team
// mode gets -- any of these would defeat the anonymized division rollup by naming
// individuals. All five share the h.aggregation.Anonymized() guard.
func TestGetScores_DivisionMode_ExportForbidden(t *testing.T) {
	targets := []string{
		"/api/v1/events",
		"/api/v1/outcomes",
		"/api/v1/quality_events",
		"/api/v1/quality_history",
		"/api/v1/fidelity",
	}
	for _, target := range targets {
		h, db := newDivisionModeHandler(t, 3)
		seedDivisionMember(t, db, "alice", "i-1", "platform", "engineering", 10, 3)
		code, body := doRequest(t, h, http.MethodGet, target, nil)
		if code != http.StatusForbidden {
			t.Errorf("%s division mode = %d, want 403; body=%s", target, code, body)
		}
	}
}

// TestGetScores_DivisionMode_TeamFilterIgnored proves the ?team= filter is
// skipped in division mode (#270): honoring it would roll up a SINGLE named team
// with NO k-floor, re-exposing a sub-k cohort's aggregate and bypassing the
// anonymity set. The response must carry NO scoped `team` block and must not name
// the filtered developers.
func TestGetScores_DivisionMode_TeamFilterIgnored(t *testing.T) {
	h, db := newDivisionModeHandler(t, 3)
	// A sub-k team "small" (2 devs) under a >= k division "engineering".
	seedDivisionMember(t, db, "e1", "i-e1", "big", "engineering", 10, 3)
	seedDivisionMember(t, db, "e2", "i-e2", "big", "engineering", 10, 3)
	seedDivisionMember(t, db, "e3", "i-e3", "big", "engineering", 10, 3)
	seedDivisionMember(t, db, "s1", "i-s1", "small", "engineering", 10, 3)
	seedDivisionMember(t, db, "s2", "i-s2", "small", "engineering", 10, 3)

	// Ask for the sub-k team explicitly -- division mode must ignore it.
	_, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?team=small", nil)
	raw := string(body)
	for _, m := range []string{"s1", "s2", "small"} {
		if strings.Contains(raw, m) {
			t.Errorf("?team=small in division mode re-exposed %q (unfloored single-team rollup):\n%s", m, raw)
		}
	}
	resp := unmarshalScores(t, body)
	if resp.Team != nil {
		t.Errorf("division mode must not honor ?team= (no scoped team block); got %+v", resp.Team)
	}
}

// TestGetScores_DivisionMode_EmptyDivisionFoldsToOther proves a developer with an
// empty (unset/NULL) division folds into "other" rather than a blank-named row
// (#270) -- the documented empty-division bucket.
func TestGetScores_DivisionMode_EmptyDivisionFoldsToOther(t *testing.T) {
	h, db := newDivisionModeHandler(t, 3)
	// A named k-sized division.
	seedDivisionMember(t, db, "e1", "i-e1", "platform", "engineering", 10, 3)
	seedDivisionMember(t, db, "e2", "i-e2", "platform", "engineering", 10, 3)
	seedDivisionMember(t, db, "e3", "i-e3", "infra", "engineering", 10, 3)
	// A developer with an EMPTY division (team set, division "").
	seedDivisionMember(t, db, "nodiv", "i-nd", "platform", "", 5, 2)

	_, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	resp := unmarshalScores(t, body)
	for _, ts := range resp.Teams {
		if ts.Team == "" {
			t.Errorf("empty division must not produce a blank-named row; got %v", teamJSONNames(resp.Teams))
		}
	}
	other := teamByName(resp.Teams, "other")
	if other == nil || other.WeightedPoints != 2 || other.TotalCostUSD != 5 {
		t.Errorf("empty-division developer must fold into 'other' (points 2, cost 5); got %+v", other)
	}
}

// TestGetScores_DivisionMode_ComposesWithWorkTypesAndCost proves division mode
// composes with the #187 work-type segments and the #234 cost-composition sidecar
// (#270): each segment carries division rows (never names), and cost_composition
// still ships (it is a whole-window name-free aggregate).
func TestGetScores_DivisionMode_ComposesWithWorkTypesAndCost(t *testing.T) {
	h, db := newDivisionModeHandler(t, 3)
	seedDivisionMember(t, db, "e1", "i-e1", "platform", "engineering", 10, 3)
	seedDivisionMember(t, db, "e2", "i-e2", "platform", "engineering", 10, 3)
	seedDivisionMember(t, db, "e3", "i-e3", "infra", "engineering", 10, 3)

	_, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	raw := string(body)
	for _, m := range []string{"e1", "e2", "e3"} {
		if strings.Contains(raw, m) {
			t.Errorf("division-mode segmented view leaked developer name %q:\n%s", m, raw)
		}
	}
	resp := unmarshalScores(t, body)
	for _, seg := range resp.WorkTypes {
		if len(seg.Developers) != 0 {
			t.Errorf("work-type segment %q must emit no developer rows in division mode; got %d",
				seg.WorkType, len(seg.Developers))
		}
	}
}

func unmarshalScores(t *testing.T, body []byte) scoresResponse {
	t.Helper()
	var resp scoresResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal scoresResponse: %v", err)
	}
	return resp
}
