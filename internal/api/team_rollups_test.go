package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// rollupSumTolerance bounds the float drift between Σ team_rollups and `total`.
// Both are float64 sums of the same per-developer values, but grouped by team the
// additions re-associate, and IEEE-754 addition is not associative, so exact
// equality is not guaranteed. 1e-9 is far below one micro-dollar (1e-6).
const rollupSumTolerance = 1e-9

// seedRollupFixture seeds three teams, two unassigned developers (one cost-only)
// and a hierarchy-only "ghost" team with no activity in the window.
func seedRollupFixture(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := context.Background()
	member := func(dev, team string, cost, weight float64) {
		seedCosts(t, db, dev, "i-"+dev, cost)
		if weight > 0 {
			seedOutcome(t, db, dev, "i-"+dev, weight, 1.0)
		}
		if team != "" {
			if err := baselineHierarchy(db, ctx, dev, team, "div", "acme"); err != nil {
				t.Fatalf("UpsertHierarchy(%s,%s): %v", dev, team, err)
			}
		}
	}
	member("alice", "platform", 0.10, 3)
	member("bob", "platform", 0.20, 5)
	member("carol", "growth", 0.30, 2)
	member("dave", "infra", 1.70, 8)
	member("erin", "infra", 2.30, 1)
	member("frank", "", 0.70, 4)
	member("gina", "", 0.90, 0)
	if err := baselineHierarchy(db, ctx, "henry", "ghost", "div", "acme"); err != nil {
		t.Fatalf("UpsertHierarchy(henry): %v", err)
	}
}

// rawScoresKeys returns the top-level JSON keys of a /scores body, so absence is
// asserted on the wire rather than on a decoded nil.
func rawScoresKeys(t *testing.T, h *Handler, target string) map[string]json.RawMessage {
	t.Helper()
	code, body := doRequest(t, h, http.MethodGet, target, nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body = %s", target, code, body)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", target, err)
	}
	return m
}

func assertRollupsSumToTotal(t *testing.T, resp scoresResponse) {
	t.Helper()
	if resp.Total == nil {
		t.Fatal("total is absent; the reconciliation has nothing to reconcile against")
	}
	var points, cost float64
	for _, r := range resp.TeamRollups {
		points += r.WeightedPoints
		cost += r.TotalCostUSD
	}
	if math.Abs(points-resp.Total.WeightedPoints) > rollupSumTolerance {
		t.Errorf("Σ team_rollups.weighted_points = %v, total.weighted_points = %v", points, resp.Total.WeightedPoints)
	}
	if math.Abs(cost-resp.Total.TotalCostUSD) > rollupSumTolerance {
		t.Errorf("Σ team_rollups.total_cost_usd = %v, total.total_cost_usd = %v", cost, resp.Total.TotalCostUSD)
	}
}

// TestTeamRollups_DeveloperModeSumToTotal pins the #821 reconciliation invariant
// and the row set: one row per team with a scored developer, in ascending order,
// then exactly one unassigned row; a hierarchy-only team gets no row.
func TestTeamRollups_DeveloperModeSumToTotal(t *testing.T) {
	h, db := newTestHandler(t)
	seedRollupFixture(t, db)

	resp := getScores(t, h, "/api/v1/scores")
	assertRollupsSumToTotal(t, resp)

	var got []string
	for _, r := range resp.TeamRollups {
		label := r.Team
		if r.Unassigned {
			label = "<unassigned>"
		}
		got = append(got, label)
	}
	want := []string{"growth", "infra", "platform", "<unassigned>"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("team_rollups rows = %v, want %v", got, want)
	}
	un := resp.TeamRollups[len(resp.TeamRollups)-1]
	if un.Team != "" {
		t.Errorf("unassigned row carries team %q, want empty", un.Team)
	}
	if math.Abs(un.TotalCostUSD-1.60) > rollupSumTolerance || un.WeightedPoints != 4 {
		t.Errorf("unassigned row = {points %v, cost %v}, want {4, 1.60} (frank + cost-only gina)",
			un.WeightedPoints, un.TotalCostUSD)
	}
}

// TestTeamRollups_WireShape pins the JSON contract of a row: `unassigned` is always
// present, and the unassigned row ships no `team` key.
func TestTeamRollups_WireShape(t *testing.T) {
	h, db := newTestHandler(t)
	seedRollupFixture(t, db)

	keys := rawScoresKeys(t, h, "/api/v1/scores")
	raw, ok := keys["team_rollups"]
	if !ok {
		t.Fatal("developer-mode /scores has no team_rollups key")
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("unmarshal team_rollups: %v", err)
	}
	var unassigned int
	for _, r := range rows {
		u, ok := r["unassigned"]
		if !ok {
			t.Errorf("row %s has no unassigned key", r["team"])
			continue
		}
		if string(u) == "true" {
			unassigned++
			if _, hasTeam := r["team"]; hasTeam {
				t.Errorf("unassigned row ships a team key: %s", r["team"])
			}
		} else if _, hasTeam := r["team"]; !hasTeam {
			t.Errorf("assigned row has no team key")
		}
	}
	if unassigned != 1 {
		t.Errorf("unassigned rows = %d, want exactly 1", unassigned)
	}
}

// TestTeamRollups_TeamFilterLeavesRollupsUnchanged pins the ?team= decision: like
// `total`, team_rollups is not narrowed by ?team=, and each named row equals the
// `team` block the ?team= branch builds for that team.
func TestTeamRollups_TeamFilterLeavesRollupsUnchanged(t *testing.T) {
	h, db := newTestHandler(t)
	seedRollupFixture(t, db)

	unfiltered := getScores(t, h, "/api/v1/scores")
	for _, name := range []string{"platform", "growth", "infra"} {
		filtered := getScores(t, h, "/api/v1/scores?team="+name)
		if !reflect.DeepEqual(filtered.TeamRollups, unfiltered.TeamRollups) {
			t.Errorf("?team=%s changed team_rollups:\n got %+v\nwant %+v", name, filtered.TeamRollups, unfiltered.TeamRollups)
		}
		if filtered.Team == nil {
			t.Fatalf("?team=%s built no team block", name)
		}
		found := false
		for _, r := range filtered.TeamRollups {
			if r.Team == name {
				found = true
				if !reflect.DeepEqual(r.teamScoreJSON, *filtered.Team) {
					t.Errorf("team_rollups[%s] = %+v, ?team= block = %+v", name, r.teamScoreJSON, *filtered.Team)
				}
			}
		}
		if !found {
			t.Errorf("team_rollups has no %s row", name)
		}
	}
}

// TestTeamRollups_RepoScopeFollowsTotal pins that ?repo= narrows team_rollups
// exactly as it narrows `total`: the scoped rows reconcile to the scoped total.
func TestTeamRollups_RepoScopeFollowsTotal(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seed := func(dev, team, repo string, cost, weight float64) {
		seedRepoCostAt(t, db, repo, dev, "i-"+dev+"-"+repo, cost, now)
		if _, err := db.InsertOutcome(ctx, store.Outcome{
			Developer: dev, IssueID: "i-" + dev + "-" + repo, Repo: repo, Weight: weight, Quality: 1,
			MergeCommitSHA: "sha-" + dev + "-" + repo, Timestamp: now,
		}); err != nil {
			t.Fatalf("InsertOutcome(%s): %v", dev, err)
		}
		if team != "" {
			if err := baselineHierarchy(db, ctx, dev, team, "div", "acme"); err != nil {
				t.Fatalf("UpsertHierarchy(%s): %v", dev, err)
			}
		}
	}
	seed("alice", "platform", repoAlpha, 1.0, 3)
	seed("alice", "platform", repoBeta, 5.0, 2)
	seed("bob", "growth", repoBeta, 7.0, 6)
	seed("carol", "", repoAlpha, 2.0, 1)

	since := "?since=" + scoresSince()
	fleet := getScores(t, h, "/api/v1/scores"+since)
	scoped := getScores(t, h, "/api/v1/scores"+since+"&repo="+repoAlpha)
	if fleet.Total == nil || scoped.Total == nil || fleet.Total.TotalCostUSD == scoped.Total.TotalCostUSD {
		t.Fatalf("control arm: the repo scope did not narrow total (fleet %+v, scoped %+v)", fleet.Total, scoped.Total)
	}
	assertRollupsSumToTotal(t, fleet)
	assertRollupsSumToTotal(t, scoped)
}

// TestTeamRollups_AbsentInTeamMode: an unfloored per-team rollup in an anonymized
// mode is the #593 differencing channel. Total is asserted PRESENT first, so the
// absence is the mode gate's doing, not the total gate's.
func TestTeamRollups_AbsentInTeamMode(t *testing.T) {
	h, db := newTeamModeHandler(t, 3)
	for _, d := range []string{"a1", "a2", "a3"} {
		seedTeamMember(t, db, d, "i-"+d, "eng", 10, 3)
	}
	for _, d := range []string{"b1", "b2", "b3"} {
		seedTeamMember(t, db, d, "i-"+d, "ops", 4, 2)
	}

	keys := rawScoresKeys(t, h, "/api/v1/scores")
	if _, ok := keys["total"]; !ok {
		t.Fatal("control arm: team-mode total is absent, so this test cannot see the mode gate")
	}
	if raw, ok := keys["team_rollups"]; ok {
		t.Errorf("team mode emitted team_rollups: %s", raw)
	}
	if raw, ok := rawScoresKeys(t, h, "/api/v1/scores?team=eng")["team_rollups"]; ok {
		t.Errorf("team mode with ?team= emitted team_rollups: %s", raw)
	}
}

// TestTeamRollups_AbsentInDivisionMode is the division-level (#270) analogue.
func TestTeamRollups_AbsentInDivisionMode(t *testing.T) {
	h, db := newDivisionModeHandler(t, 3)
	seedDivisionMember(t, db, "alice", "i-a", "platform", "engineering", 10, 3)
	seedDivisionMember(t, db, "bob", "i-b", "platform", "engineering", 10, 4)
	seedDivisionMember(t, db, "carol", "i-c", "infra", "engineering", 10, 5)

	keys := rawScoresKeys(t, h, "/api/v1/scores")
	if _, ok := keys["total"]; !ok {
		t.Fatal("control arm: division-mode total is absent, so this test cannot see the mode gate")
	}
	if raw, ok := keys["team_rollups"]; ok {
		t.Errorf("division mode emitted team_rollups: %s", raw)
	}
}

// TestTeamRollups_ModeGateHoldsOnSuppressedFixture pins the mode gate on a #593
// suppression fixture (a residual below k). k-anonymity suppression only ever fires
// in an anonymized mode, so the mode gate alone keeps team_rollups absent here.
func TestTeamRollups_ModeGateHoldsOnSuppressedFixture(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	for i, d := range []string{"f1", "f2", "f3", "f4", "f5"} {
		seedKAnonDevTyped(t, db, "eng", d, float64(10+i), 2, store.WorkTypeFeature, now)
	}
	seedKAnonDev(t, db, "solo", "s1", 7.77, 3, now) // alone in "other", below k
	h.SetAggregation(scoring.AggregationTeam, 5)

	keys := rawScoresKeys(t, h, "/api/v1/scores?since="+scoresSince())
	var dq struct {
		KAnonSuppressed *kanonSuppressedJSON `json:"kanon_suppressed"`
	}
	if err := json.Unmarshal(keys["data_quality"], &dq); err != nil || dq.KAnonSuppressed == nil {
		t.Fatalf("control arm: the fixture did not suppress (data_quality %s, err %v)", keys["data_quality"], err)
	}
	if raw, ok := keys["team_rollups"]; ok {
		t.Errorf("team_rollups survived a k-anonymity suppression: %s", raw)
	}
}

// TestTeamRollups_ResolvesAliasLikeTeamMode pins the canonical fold of the
// developer-mode team map (#821): a hierarchy row registered under a raw id that
// LATER becomes an alias puts the canonical developer in that team in both
// team_rollups and ?team=, as the anonymized modes do, never in unassigned.
func TestTeamRollups_ResolvesAliasLikeTeamMode(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	seedCosts(t, db, "alice.smith", "i-1", 1.0)
	seedOutcome(t, db, "alice.smith", "i-1", 3, 1)
	if err := baselineHierarchy(db, ctx, "asmith-gh", "eng", "div", "acme"); err != nil {
		t.Fatalf("UpsertHierarchy: %v", err)
	}
	// A legacy alias (#914): alice.smith carries the eng placement she held
	// through asmith-gh before the upgrade.
	if err := baselineAlias(db, ctx, "asmith-gh", "alice.smith"); err != nil {
		t.Fatalf("baselineAlias: %v", err)
	}

	resp := getScores(t, h, "/api/v1/scores?team=eng")
	if resp.Team == nil || resp.Team.WeightedPoints != 3 {
		t.Fatalf("?team=eng block = %+v, want alice.smith's 3 points", resp.Team)
	}
	if len(resp.TeamRollups) != 1 || resp.TeamRollups[0].Unassigned || resp.TeamRollups[0].Team != "eng" {
		t.Fatalf("team_rollups = %+v, want exactly one eng row", resp.TeamRollups)
	}
	if !reflect.DeepEqual(resp.TeamRollups[0].teamScoreJSON, *resp.Team) {
		t.Errorf("team_rollups[eng] = %+v, ?team= block = %+v", resp.TeamRollups[0].teamScoreJSON, *resp.Team)
	}
}

// TestMembershipTimeline_EachRawIDReadsOnlyItsOwnRows pins #914's read rule:
// a raw id is placed by its own rows and no one else's. Three raw ids of one
// person carry three different teams and each reads its own; an alias with no
// row reads unassigned even though its canonical has one — there is no
// canonical fallback, and so no tie-break between one person's ids.
func TestMembershipTimeline_EachRawIDReadsOnlyItsOwnRows(t *testing.T) {
	from := store.MembershipBaselineFrom
	tl := newMembershipTimeline([]store.MembershipRow{
		{Developer: "zz-alias", Team: "sre", ValidFrom: from},
		{Developer: "asmith-gh", Team: "ops", ValidFrom: from},
		{Developer: "alice.smith", Team: "eng", ValidFrom: from},
	})
	at := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for raw, want := range map[string]string{
		"alice.smith": "eng", "asmith-gh": "ops", "zz-alias": "sre", "rowless-alias": "",
	} {
		if got := tl.labelAt(raw, at, teamLabel); got != want {
			t.Errorf("labelAt(%s) = %q, want %q", raw, got, want)
		}
	}
}

// TestBuildTeamRollups_EmptyTeamIsUnassigned pins that rows labeled "" (no
// membership at the event's time) land in the one unassigned row.
func TestBuildTeamRollups_EmptyTeamIsUnassigned(t *testing.T) {
	rows := buildTeamRollups([]scoring.LabeledScore{
		{Label: "", Score: scoring.DeveloperScore{Developer: "a"}},
		{Label: "", Score: scoring.DeveloperScore{Developer: "b"}},
	})
	if len(rows) != 1 || !rows[0].Unassigned || rows[0].Team != "" {
		t.Fatalf("rows = %+v, want one unassigned row holding a and b", rows)
	}
}
