package api

// #864: an anonymised install publishes ONE breakdown per window — its team (or
// division) rows and the grand total — and withholds the whole response when the
// "other" bucket does not reach k. Each test below failed on 43e0916 (the commit
// before #864); the failure line is quoted in the #864 commit message.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// anonymisedModes are the two modes #864 reshapes.
var anonymisedModes = []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision}

// seedTypedOutcomeCost gives an already-enrolled developer one more cost-bearing
// outcome of the given work type on its own issue.
func seedTypedOutcomeCost(t *testing.T, db *store.DB, dev, issue string, costUSD, weight float64, workType string, ts time.Time) {
	t.Helper()
	seedRepoCostAt(t, db, repoAlpha, dev, issue, costUSD, ts)
	if _, err := db.InsertOutcome(context.Background(), store.Outcome{
		Developer: dev, IssueID: issue, Repo: repoAlpha, Weight: weight, Quality: 1,
		WorkType: workType, WorkTypeSource: store.WorkTypeSourceLabel,
		MergeCommitSHA: "sha-864-" + issue, Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertOutcome(%s,%s): %v", dev, issue, err)
	}
}

// padResidual seeds n complete people (rostered, captured cost and outcome at
// each instant in at), each alone in a team and a division of their own, so in
// either anonymised mode they fold into "other" and lift it to k. A fixture that
// needs named rows published beside a residual carrying another group's figures
// uses it: since #864 a residual below k withholds the whole response.
func padResidual(t *testing.T, db *store.DB, n int, at ...time.Time) {
	t.Helper()
	for i := 1; i <= n; i++ {
		dev := fmt.Sprintf("pad-%d", i)
		if err := baselineHierarchy(db, context.Background(), dev, "pad-team-"+dev, "pad-div-"+dev, "acme"); err != nil {
			t.Fatalf("baselineHierarchy(%s): %v", dev, err)
		}
		for j, ts := range at {
			issue := fmt.Sprintf("i-%s-%d", dev, j)
			seedCostAt(t, db, dev, issue, 1, ts)
			seedOutcomeAt(t, db, dev, issue, 1, 1, ts)
		}
	}
}

// numbersIn collects every JSON number in a decoded document.
func numbersIn(v any, out *[]float64) {
	switch x := v.(type) {
	case float64:
		*out = append(*out, x)
	case map[string]any:
		for _, e := range x {
			numbersIn(e, out)
		}
	case []any:
		for _, e := range x {
			numbersIn(e, out)
		}
	}
}

// TestOneBreakdown_E2PooledRowsAndWorkTypeTotalsDoNotDifference is E2 from the
// #864 panel: with NOTHING suppressed and every published row at >= 5 people, the
// pooled team rows and the work-type totals difference to a 3-person group
// ($9.21 / 3 points) and a 2-person group ($3.33).
func TestOneBreakdown_E2PooledRowsAndWorkTypeTotalsDoNotDifference(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	for i := 1; i <= 5; i++ {
		seedKAnonDevTyped(t, db, "A", fmt.Sprintf("a%d", i), float64(10+i), 2, store.WorkTypeSecurity, now)
	}
	for i := 6; i <= 8; i++ { // A ∩ feature: 3 people, $3.06 + $3.07 + $3.08 = $9.21
		seedKAnonDevTyped(t, db, "A", fmt.Sprintf("a%d", i), 3.0+float64(i)/100, 1, store.WorkTypeFeature, now)
	}
	for i := 1; i <= 5; i++ {
		seedKAnonDevTyped(t, db, "C", fmt.Sprintf("c%d", i), float64(20+i), 2, store.WorkTypeSecurity, now)
	}
	for i := 1; i <= 5; i++ {
		seedKAnonDevTyped(t, db, "D", fmt.Sprintf("d%d", i), float64(30+i), 2, store.WorkTypeBug, now)
	}
	// D ∩ feature: 2 people, $1.11 + $2.22 = $3.33.
	seedTypedOutcomeCost(t, db, "d1", "f-d1", 1.11, 1, store.WorkTypeFeature, now)
	seedTypedOutcomeCost(t, db, "d2", "f-d2", 2.22, 1, store.WorkTypeFeature, now)
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp := getScores(t, h, "/api/v1/scores?since="+scoresSince())
	if resp.DataQuality != nil && resp.DataQuality.KAnonSuppressed != nil {
		t.Fatalf("E2 is the NOTHING-suppressed case; the fixture suppressed: %+v", resp.DataQuality.KAnonSuppressed)
	}
	pooled := map[string]teamScoreJSON{}
	for _, r := range resp.Teams {
		pooled[r.Team] = r
	}
	for _, team := range []string{"A", "C", "D"} {
		if _, ok := pooled[team]; !ok {
			t.Fatalf("control: pooled team %q must publish (every team has >= 5 people); teams=%+v", team, resp.Teams)
		}
	}
	segTotal := map[string]teamScoreJSON{}
	for _, s := range resp.WorkTypes {
		if s.Total != nil {
			segTotal[s.WorkType] = *s.Total
		}
	}
	if sec, ok := segTotal[store.WorkTypeSecurity]; ok {
		aFeatCost := pooled["A"].TotalCostUSD - (sec.TotalCostUSD - pooled["C"].TotalCostUSD)
		aFeatPts := pooled["A"].WeightedPoints - (sec.WeightedPoints - pooled["C"].WeightedPoints)
		if math.Abs(aFeatCost-9.21) < 1e-6 {
			t.Errorf("E2: pooled rows + work-type totals recovered the 3-person A∩feature group: cost=$%.2f points=%.0f", aFeatCost, aFeatPts)
		}
	}
	if bug, ok := segTotal[store.WorkTypeBug]; ok {
		if dFeat := pooled["D"].TotalCostUSD - bug.TotalCostUSD; math.Abs(dFeat-3.33) < 1e-6 {
			t.Errorf("E2: pooled rows + work-type totals recovered the 2-person D∩feature group: cost=$%.2f", dFeat)
		}
	}
	if len(resp.WorkTypes) != 0 {
		t.Errorf("an anonymised response publishes one breakdown (team rows + total); work_types present: %d segments", len(resp.WorkTypes))
	}
}

// TestOneBreakdown_WorkTypeParamRefusedInAnonymisedModes: ?work_type= selects a
// second breakdown, so an anonymised mode refuses it (400) the way it refuses
// ?repo=. Developer mode still honours it.
func TestOneBreakdown_WorkTypeParamRefusedInAnonymisedModes(t *testing.T) {
	for _, mode := range append([]scoring.AggregationMode{scoring.AggregationDeveloper}, anonymisedModes...) {
		t.Run(mode.String(), func(t *testing.T) {
			h, db := newTestHandler(t)
			now := time.Now().UTC()
			for i := 1; i <= 5; i++ {
				seedKAnonDevTyped(t, db, "eng", fmt.Sprintf("e%d", i), 10, 2, store.WorkTypeFeature, now)
			}
			h.SetAggregation(mode, 5)
			code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?work_type=feature&since="+scoresSince(), nil)
			want := http.StatusBadRequest
			if !mode.Anonymized() {
				want = http.StatusOK
			}
			if code != want {
				t.Errorf("%s mode: ?work_type=feature status = %d, want %d; body = %s", mode, code, want, body)
			}
		})
	}
}

// TestOneBreakdown_NoSegmentTeamsInAnonymisedResponses: the per-work-type team
// rows (and the work-type segments that carry them) leave anonymised responses.
func TestOneBreakdown_NoSegmentTeamsInAnonymisedResponses(t *testing.T) {
	for _, mode := range anonymisedModes {
		t.Run(mode.String(), func(t *testing.T) {
			h, db := newTestHandler(t)
			now := time.Now().UTC()
			for i := 1; i <= 5; i++ {
				seedKAnonDevTyped(t, db, "eng", fmt.Sprintf("e%d", i), 10, 2, store.WorkTypeFeature, now)
				seedKAnonDevTyped(t, db, "ops", fmt.Sprintf("o%d", i), 12, 2, store.WorkTypeBug, now)
			}
			h.SetAggregation(mode, 5)
			raw, body := getScoresRaw(t, h, "?since="+scoresSince())
			if _, ok := raw["teams"]; !ok {
				t.Fatalf("control: the %s rows must still publish; body = %s", mode, body)
			}
			if segs, ok := raw["work_types"]; ok {
				t.Errorf("%s mode: work_types (segments[].teams) must be absent; got %v", mode, segs)
			}
			if _, ok := raw["segment_reconciliation"]; ok {
				t.Errorf("%s mode: segment_reconciliation reconciles the absent segments and must be absent too", mode)
			}
		})
	}
}

// TestOneBreakdown_E3SingleUserModelPublishesNoPerModelFigure is E3 from the #864
// panel: a model only one developer used published that developer's spend in
// cost_composition.by_model. In anonymised modes no per-model figure — and no
// other per-category split of cost — may appear.
func TestOneBreakdown_E3SingleUserModelPublishesNoPerModelFigure(t *testing.T) {
	for _, mode := range anonymisedModes {
		t.Run(mode.String(), func(t *testing.T) {
			h, db := newTestHandler(t)
			now := time.Now().UTC()
			for i := 1; i <= 6; i++ {
				seedKAnonDev(t, db, "eng", fmt.Sprintf("e%d", i), float64(10+i), 2, now)
			}
			// e6 alone uses a premium model, on an issue-less mainline session too.
			if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
				Developer: "e6", IssueID: "i-e6", Repo: repoAlpha, Model: "claude-opus-4",
				InputTok: 2000, CostMicro: store.DollarsToMicro(4.44), Source: "jsonl", Fidelity: "realtime", Timestamp: now,
			}); err != nil {
				t.Fatal(err)
			}
			h.SetAggregation(mode, 5)
			raw, body := getScoresRaw(t, h, "?since="+scoresSince())
			if _, ok := raw["teams"]; !ok {
				t.Fatalf("control: the %s rows must still publish; body = %s", mode, body)
			}
			var nums []float64
			numbersIn(raw, &nums)
			for _, n := range nums {
				if math.Abs(n-4.44) < 1e-9 {
					t.Errorf("E3: %s mode published the single-user model's spend $4.44", mode)
				}
			}
			for _, key := range []string{"cost_composition", "by_model", "premium_model_share", "unattributed_buckets"} {
				if bytes.Contains([]byte(body), []byte(`"`+key+`"`)) {
					t.Errorf("%s mode: %q is a per-category split of cost and must be absent", mode, key)
				}
			}
		})
	}
}

// TestOneBreakdown_SubKOtherWithholdsWholeResponse: when the "other" bucket does
// not reach k, the WHOLE response is withheld — the named rows too, not only the
// residual and the total — and the withhold is declared.
func TestOneBreakdown_SubKOtherWithholdsWholeResponse(t *testing.T) {
	seed := func(t *testing.T, otherPeople int) *Handler {
		h, db := newTestHandler(t)
		now := time.Now().UTC()
		for i := 1; i <= 5; i++ {
			seedKAnonDev(t, db, "big", fmt.Sprintf("b%d", i), 10, 2, now)
		}
		for i := 1; i <= otherPeople; i++ { // each in its own sub-k team: all fold to "other"
			seedKAnonDev(t, db, fmt.Sprintf("small%d", i), fmt.Sprintf("s%d", i), 3, 1, now)
		}
		h.SetAggregation(scoring.AggregationTeam, 5)
		return h
	}

	t.Run("other below k", func(t *testing.T) {
		h := seed(t, 2)
		resp := getScores(t, h, "/api/v1/scores?since="+scoresSince())
		if len(resp.Teams) != 0 {
			t.Errorf("other bucket below k: the whole response must be withheld; teams published: %+v", resp.Teams)
		}
		if resp.Total != nil {
			t.Errorf("other bucket below k: total must be withheld; got %+v", resp.Total)
		}
		raw, body := getScoresRaw(t, h, "?since="+scoresSince())
		dq, _ := raw["data_quality"].(map[string]any)
		decl, ok := dq["kanon_suppressed"].(map[string]any)
		if !ok {
			t.Fatalf("other bucket below k: the withhold must be declared in data_quality.kanon_suppressed; body = %s", body)
		}
		if decl["withheld_teams"] != true || decl["withheld_total"] != true {
			t.Errorf("declaration must say the teams and the total were withheld: %v", decl)
		}
	})
	t.Run("control: other reaches k", func(t *testing.T) {
		h := seed(t, 5)
		resp := getScores(t, h, "/api/v1/scores?since="+scoresSince())
		if len(resp.Teams) != 2 || resp.Total == nil {
			t.Errorf("other bucket at k: big + other and the total must publish; teams=%+v total=%v", resp.Teams, resp.Total)
		}
		if resp.DataQuality != nil && resp.DataQuality.KAnonSuppressed != nil {
			t.Errorf("other bucket at k: nothing may be declared withheld: %+v", resp.DataQuality.KAnonSuppressed)
		}
	})
}

// TestOneBreakdown_TwoRequestsCannotDifference: across separate requests — the
// team rows, then any other allowed view — no two published figures over the same
// window difference to a sub-k group. The sub-k group is S (2 people, $9.21) in
// window W. /compare folds a team into "other" when it is sub-k in the OTHER
// window, so compare(W, W2)'s "other" would be S+T beside a T published elsewhere.
// Here /scores(W) and compare(W, W3) are withheld by their own residuals; the
// per-window check in compareTeams is pinned by
// TestOneBreakdown_CompareOwnOtherBothWindows.
func TestOneBreakdown_TwoRequestsCannotDifference(t *testing.T) {
	h, db := newTestHandler(t)
	day := func(n int) time.Time {
		return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, n)
	}
	date := func(n int) string { return day(n).Format("2006-01-02") }
	inW, inW2, inW3 := day(-25).Add(12*time.Hour), day(-15).Add(12*time.Hour), day(-5).Add(12*time.Hour)

	for i := 1; i <= 5; i++ {
		dev := fmt.Sprintf("t%d", i)
		seedKAnonDevIssue(t, db, "T", dev, dev+"-w", float64(10+i), 2, inW)
		seedKAnonDevIssue(t, db, "T", dev, dev+"-w3", float64(10+i), 2, inW3)
		u := fmt.Sprintf("u%d", i)
		seedKAnonDevIssue(t, db, "U", u, u+"-w2", float64(20+i), 2, inW2)
		seedKAnonDevIssue(t, db, "U", u, u+"-w3", float64(20+i), 2, inW3)
	}
	seedKAnonDevIssue(t, db, "T", "t1", "t1-w2", 1, 1, inW2) // T is sub-k in W2
	seedKAnonDevIssue(t, db, "S", "s1", "s1-w", 4.21, 1, inW)
	seedKAnonDevIssue(t, db, "S", "s2", "s2-w", 5.00, 1, inW)
	h.SetAggregation(scoring.AggregationTeam, 5)

	// figures maps a window to every cost figure published over it, by request.
	type figure struct {
		request, label string
		cost           float64
	}
	figures := map[string][]figure{}
	add := func(win, req, label string, cost float64) {
		figures[win] = append(figures[win], figure{req, label, cost})
	}
	wKey := date(-30) + ".." + date(-20)
	w3Key := date(-10) + ".."

	scoresW := getScores(t, h, "/api/v1/scores?since="+date(-30)+"&until="+date(-20))
	for _, r := range scoresW.Teams {
		add(wKey, "scores(W)", r.Team, r.TotalCostUSD)
	}
	if scoresW.Total != nil {
		add(wKey, "scores(W)", "total", scoresW.Total.TotalCostUSD)
	}
	for _, c := range []struct{ name, sinceB, untilB string }{
		{"compare(W,W2)", date(-20), date(-10)},
		{"compare(W,W3)", date(-10), ""},
	} {
		url := "/api/v1/scores/compare?since_a=" + date(-30) + "&until_a=" + date(-20) + "&since_b=" + c.sinceB
		if c.untilB != "" {
			url += "&until_b=" + c.untilB
		}
		code, resp := getCompare(t, h, url)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d", c.name, code)
		}
		for _, r := range resp.Teams {
			add(wKey, c.name, r.Team, r.A.TotalCostUSD)
		}
		if resp.Total != nil {
			add(wKey, c.name, "total", resp.Total.A.TotalCostUSD)
		}
	}
	// Control: an allowed view over a window whose "other" bucket reaches k still
	// publishes, so an all-empty world cannot pass this test.
	scoresW3 := getScores(t, h, "/api/v1/scores?since="+date(-10))
	if len(scoresW3.Teams) != 2 || scoresW3.Total == nil {
		t.Fatalf("control: /scores(W3) has T and U at 5 people each and must publish; teams=%+v", scoresW3.Teams)
	}
	for _, r := range scoresW3.Teams {
		add(w3Key, "scores(W3)", r.Team, r.TotalCostUSD)
	}

	const hidden = 9.21 // S in W: 2 people
	for win, fs := range figures {
		for _, a := range fs {
			if math.Abs(a.cost-hidden) < 1e-6 {
				t.Errorf("%s: %s %q publishes the 2-person group's $%.2f", win, a.request, a.label, a.cost)
			}
			for _, b := range fs {
				if d := a.cost - b.cost; math.Abs(d-hidden) < 1e-6 {
					t.Errorf("%s: %s %q ($%.2f) − %s %q ($%.2f) = $%.2f, the 2-person group S",
						win, a.request, a.label, a.cost, b.request, b.label, b.cost, d)
				}
			}
		}
	}
}

var updateGolden = flag.Bool("update-864-golden", false, "rewrite testdata/developer_mode_864.golden.json")

// volatileValue matches the date and instant strings a fixture seeded relative to
// time.Now() puts in a response.
var volatileValue = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}([T ].*)?$`)

func normalizeVolatile(v any) any {
	switch x := v.(type) {
	case string:
		if volatileValue.MatchString(x) {
			return "<time>"
		}
	case map[string]any:
		for k, e := range x {
			x[k] = normalizeVolatile(e)
		}
	case []any:
		for i, e := range x {
			x[i] = normalizeVolatile(e)
		}
	}
	return v
}

// TestOneBreakdown_DeveloperModeUnchanged pins developer mode byte-for-byte (after
// masking the fixture's clock-relative dates and the price_table stamp, which moves
// with every price-table release) against the response 43e0916 produced, the commit
// before #864: work types, the ?work_type= filter, the cost composition with its
// by-model rows, the unattributed buckets and the segment reconciliation all still
// ship there.
func TestOneBreakdown_DeveloperModeUnchanged(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC().Add(-time.Hour)
	for i := 1; i <= 3; i++ {
		seedKAnonDevTyped(t, db, "eng", fmt.Sprintf("e%d", i), float64(10+i), 2, store.WorkTypeFeature, now)
	}
	seedTypedOutcomeCost(t, db, "e1", "b-e1", 2.5, 1, store.WorkTypeBug, now)
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: "e2", IssueID: "i-e2", Repo: repoAlpha, Model: "claude-opus-4",
		InputTok: 2000, CostMicro: store.DollarsToMicro(4.44), Source: "jsonl", Fidelity: "realtime", Timestamp: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: "e3", IssueID: store.UnattributedMainBucket, Repo: repoAlpha, Model: "claude-sonnet-4",
		InputTok: 1000, CostMicro: store.DollarsToMicro(1.25), Source: "jsonl", Fidelity: "realtime", Timestamp: now,
	}); err != nil {
		t.Fatal(err)
	}
	h.SetAggregation(scoring.AggregationDeveloper, 5)

	got := map[string]any{}
	for _, q := range []string{"?since=" + scoresSince(), "?work_type=feature&since=" + scoresSince()} {
		raw, _ := getScoresRaw(t, h, q)
		if _, ok := raw["price_table"]; !ok {
			t.Fatalf("%s: price_table stamp missing", q)
		}
		raw["price_table"] = "<price_table>"
		got[q[:len(q)-len(scoresSince())]] = normalizeVolatile(raw)
	}
	gotJSON, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	gotJSON = append(gotJSON, '\n')
	path := filepath.Join("testdata", "developer_mode_864.golden.json")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, gotJSON, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	if !bytes.Equal(gotJSON, want) {
		t.Errorf("developer-mode /scores changed since 43e0916; diff %s against the output below. "+
			"Regenerate with -update-864-golden only for a change that is meant to alter developer mode.\n%s",
			path, gotJSON)
	}
}

// TestOneBreakdown_CompareOwnOtherBothWindows pins the per-window own-"other"
// check in compareTeams. Window W holds T (5 people), V (5 people) and S (2
// people, $9.21). T is active again only in X1 and V only in X2, so the
// intersection names T in compare(W,X1) and V in compare(W,X2), each time folding
// the other five-person team into "other" beside S. Without the check the two
// comparisons publish other = S+V and V for W, and S = $9.21 falls out. Run with W
// as window A and as window B.
func TestOneBreakdown_CompareOwnOtherBothWindows(t *testing.T) {
	h, db := newTestHandler(t)
	day := func(n int) time.Time { return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, n) }
	date := func(n int) string { return day(n).Format("2006-01-02") }
	inW, inX1, inX2 := day(-25).Add(12*time.Hour), day(-15).Add(12*time.Hour), day(-5).Add(12*time.Hour)
	for i := 1; i <= 5; i++ {
		tdev, vdev := fmt.Sprintf("t%d", i), fmt.Sprintf("v%d", i)
		seedKAnonDevIssue(t, db, "T", tdev, tdev+"-w", float64(10+i), 2, inW)
		seedKAnonDevIssue(t, db, "T", tdev, tdev+"-x1", float64(10+i), 2, inX1)
		seedKAnonDevIssue(t, db, "V", vdev, vdev+"-w", float64(30+i), 2, inW)
		seedKAnonDevIssue(t, db, "V", vdev, vdev+"-x2", float64(30+i), 2, inX2)
	}
	seedKAnonDevIssue(t, db, "S", "s1", "s1-w", 4.21, 1, inW)
	seedKAnonDevIssue(t, db, "S", "s2", "s2-w", 5.00, 1, inW)
	h.SetAggregation(scoring.AggregationTeam, 5)

	type window struct{ since, until string }
	w := window{date(-30), date(-20)}
	others := map[string]window{"X1": {date(-20), date(-10)}, "X2": {date(-10), ""}}
	span := func(p string, win window) string {
		q := "&since_" + p + "=" + win.since
		if win.until != "" {
			q += "&until_" + p + "=" + win.until
		}
		return q
	}

	// Control: each other window publishes on its own, so the fixture is not empty.
	if x1 := getScores(t, h, "/api/v1/scores?since="+date(-20)+"&until="+date(-10)); len(x1.Teams) == 0 {
		t.Fatalf("control: /scores(X1) must publish team T")
	}

	const hidden = 9.21 // S in W: 2 people
	for _, wAsB := range []bool{false, true} {
		t.Run(fmt.Sprintf("W_as_B=%v", wAsB), func(t *testing.T) {
			var figs []float64
			var names []string
			add := func(name string, v float64) { figs, names = append(figs, v), append(names, name) }
			scoresW := getScores(t, h, "/api/v1/scores?since="+w.since+"&until="+w.until)
			for _, r := range scoresW.Teams {
				add("scores(W) "+r.Team, r.TotalCostUSD)
			}
			if scoresW.Total != nil {
				add("scores(W) total", scoresW.Total.TotalCostUSD)
			}
			for name, x := range others {
				q := "/api/v1/scores/compare?" + span("a", w)[1:] + span("b", x)
				if wAsB {
					q = "/api/v1/scores/compare?" + span("a", x)[1:] + span("b", w)
				}
				code, resp := getCompare(t, h, q)
				if code != http.StatusOK {
					t.Fatalf("compare with %s: status %d", name, code)
				}
				side := func(d teamDeltaJSON) float64 {
					if wAsB {
						return d.B.TotalCostUSD
					}
					return d.A.TotalCostUSD
				}
				for _, r := range resp.Teams {
					add("compare(W,"+name+") "+r.Team, side(r))
				}
				if resp.Total != nil {
					add("compare(W,"+name+") total", side(*resp.Total))
				}
			}
			for i := range figs {
				if math.Abs(figs[i]-hidden) < 1e-6 {
					t.Errorf("%s publishes the 2-person group's $%.2f", names[i], figs[i])
				}
				for j := range figs {
					if d := figs[i] - figs[j]; math.Abs(d-hidden) < 1e-6 {
						t.Errorf("%s ($%.2f) − %s ($%.2f) = $%.2f, the 2-person group S", names[i], figs[i], names[j], figs[j], d)
					}
					for k := range figs {
						if d := figs[i] - figs[j] - figs[k]; j != k && math.Abs(d-hidden) < 1e-6 {
							t.Errorf("%s − %s − %s = $%.2f, the 2-person group S", names[i], names[j], names[k], d)
						}
					}
				}
			}
		})
	}
}

// seedPseudoSpend stores spend under the `unattributed` pseudo-developer from both
// writers: an org poller's daily remainder and a header-less proxy request.
func seedPseudoSpend(t *testing.T, db *store.DB, pollerUSD, proxyUSD float64, ts time.Time) {
	t.Helper()
	seedPollerRemainder(t, db, pollerUSD, ts)
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: store.UnattributedIssueID, IssueID: store.UnattributedIssueID,
		Model: "claude-sonnet-4", InputTok: 2000, CostMicro: store.DollarsToMicro(proxyUSD),
		Source: "proxy", Fidelity: "realtime", Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent(proxy pseudo @%s): %v", ts, err)
	}
}

// sameFigures reports whether two published rows carry identical figures.
func sameFigures(a, b teamScoreJSON) bool {
	return a.TIER == b.TIER && a.WeightedPoints == b.WeightedPoints && a.TotalCostUSD == b.TotalCostUSD &&
		a.ActualPaidUSD == b.ActualPaidUSD && a.SpendLeverage == b.SpendLeverage && a.CoveragePercent == b.CoveragePercent
}

// TestOneBreakdown_PseudoSpendIsOutsideEveryFigure pins #864 D′. Every real person
// is on one named team, and both pseudo-developer writers (a poller daily row and
// a proxy realtime row) spend in both windows. The total must equal the team's
// figures exactly on /scores, on each /compare window and on its delta, so no
// difference of two published figures is pseudo spend; and every anonymised
// response says, as a constant, that pseudo spend is excluded.
func TestOneBreakdown_PseudoSpendIsOutsideEveryFigure(t *testing.T) {
	for _, mode := range anonymisedModes {
		t.Run(mode.String(), func(t *testing.T) {
			h, db := newTestHandler(t)
			day := func(n int) time.Time { return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, n) }
			date := func(n int) string { return day(n).Format("2006-01-02") }
			inA, inB := day(-25).Add(12*time.Hour), day(-5).Add(12*time.Hour)
			for i := 1; i <= 5; i++ {
				dev := fmt.Sprintf("e%d", i)
				seedKAnonDevIssue(t, db, "eng", dev, dev+"-a", float64(10+i), 2, inA)
				seedKAnonDevIssue(t, db, "eng", dev, dev+"-b", float64(20+i), 3, inB)
			}
			seedPseudoSpend(t, db, 12.34, 5.67, inA)
			seedPseudoSpend(t, db, 7.89, 1.23, inB)
			h.SetAggregation(mode, 5)

			scoresA := getScores(t, h, "/api/v1/scores?since="+date(-30)+"&until="+date(-20))
			if len(scoresA.Teams) != 1 || scoresA.Total == nil {
				t.Fatalf("/scores: want the one team row and the total; teams=%v total=%v", teamJSONNames(scoresA.Teams), scoresA.Total)
			}
			if !sameFigures(*scoresA.Total, scoresA.Teams[0]) || scoresA.Total.TotalCostUSD != 65 {
				t.Errorf("/scores: total %+v must equal the team row %+v exactly ($65, no pseudo spend)", *scoresA.Total, scoresA.Teams[0])
			}
			if dq := scoresA.DataQuality; dq == nil || !dq.ExcludesUnattributedSpend {
				t.Errorf("/scores: data_quality.excludes_unattributed_spend must be true")
			} else if dq.UnjoinedDevelopers != nil {
				t.Errorf("/scores: the pseudo-developer must not reach unjoined_developers; got %+v", dq.UnjoinedDevelopers)
			}

			code, cmp := getCompare(t, h, "/api/v1/scores/compare?since_a="+date(-30)+"&until_a="+date(-20)+"&since_b="+date(-10))
			if code != http.StatusOK || len(cmp.Teams) != 1 || cmp.Total == nil {
				t.Fatalf("/compare: want status 200, one team delta and the total; status %d teams %d total %v", code, len(cmp.Teams), cmp.Total)
			}
			tot, eng := *cmp.Total, cmp.Teams[0]
			if !sameFigures(tot.A, eng.A) || !sameFigures(tot.B, eng.B) {
				t.Errorf("/compare: total A %+v / B %+v must equal the team's A %+v / B %+v exactly", tot.A, tot.B, eng.A, eng.B)
			}
			if tot.DeltaTIER != eng.DeltaTIER || tot.DeltaWeightedPoints != eng.DeltaWeightedPoints || tot.DeltaTotalCostUSD != eng.DeltaTotalCostUSD {
				t.Errorf("/compare: total delta %+v must equal the team's delta %+v", tot, eng)
			}
			for name, meta := range map[string]compareWindowMeta{"window_a": cmp.WindowA, "window_b": cmp.WindowB} {
				if meta.DataQuality == nil || !meta.DataQuality.ExcludesUnattributedSpend {
					t.Errorf("/compare %s: data_quality.excludes_unattributed_spend must be true", name)
				}
			}
		})
	}
}

// TestOneBreakdown_PseudoOnlyWindowReadsAsEmpty: after the #864 D′ drop, a window
// holding only pseudo-developer spend returns exactly what an empty window does,
// on /scores and on each side of /compare, so no response says which windows held
// header-less or uncaptured spend.
func TestOneBreakdown_PseudoOnlyWindowReadsAsEmpty(t *testing.T) {
	h, db := newTestHandler(t)
	day := func(n int) time.Time { return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, n) }
	date := func(n int) string { return day(n).Format("2006-01-02") }
	for i := 1; i <= 5; i++ { // people before both windows, so the cost horizon precedes them
		dev := fmt.Sprintf("e%d", i)
		seedKAnonDevIssue(t, db, "eng", dev, dev+"-old", 10, 2, day(-40).Add(12*time.Hour))
	}
	seedPseudoSpend(t, db, 12.34, 5.67, day(-25).Add(12*time.Hour)) // pseudo-only window P
	h.SetAggregation(scoring.AggregationTeam, 5)
	p, e := [2]string{date(-30), date(-20)}, [2]string{date(-20), date(-10)} // E is empty

	strip := func(raw map[string]any, keys ...string) string {
		for _, k := range keys {
			delete(raw, k)
		}
		b, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	rawP, _ := getScoresRaw(t, h, "?since="+p[0]+"&until="+p[1])
	rawE, _ := getScoresRaw(t, h, "?since="+e[0]+"&until="+e[1])
	if dq, _ := rawE["data_quality"].(map[string]any); dq["excludes_unattributed_spend"] != true {
		t.Fatalf("control: an empty anonymised window still carries the constant flag; got %v", rawE["data_quality"])
	}
	if gp, ge := strip(rawP, "since", "until"), strip(rawE, "since", "until"); gp != ge {
		t.Errorf("/scores: a pseudo-only window differs from an empty one:\npseudo %s\nempty  %s", gp, ge)
	}
	other := [2]string{date(-10), ""}
	compareRaw := func(a, b [2]string) map[string]any {
		q := "/api/v1/scores/compare?since_a=" + a[0] + "&until_a=" + a[1] + "&since_b=" + b[0]
		if b[1] != "" {
			q += "&until_b=" + b[1]
		}
		code, body := doRequest(t, h, http.MethodGet, q, nil)
		if code != http.StatusOK {
			t.Fatalf("compare: status %d body %s", code, body)
		}
		raw := map[string]any{}
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	windowOf := func(raw map[string]any, key string) string {
		m, _ := raw[key].(map[string]any)
		return strip(m, "since", "until")
	}
	if gp, ge := windowOf(compareRaw(p, other), "window_a"), windowOf(compareRaw(e, other), "window_a"); gp != ge {
		t.Errorf("/compare window_a: pseudo-only differs from empty:\npseudo %s\nempty  %s", gp, ge)
	}
	if gp, ge := windowOf(compareRaw(other, p), "window_b"), windowOf(compareRaw(other, e), "window_b"); gp != ge {
		t.Errorf("/compare window_b: pseudo-only differs from empty:\npseudo %s\nempty  %s", gp, ge)
	}
}

// TestOneBreakdown_AttributionCoverageNotShown pins #864: an anonymised mode never
// publishes attributed_cost_share, and a window with spend says so with
// attribution_coverage "not_shown", whatever shape the spend takes and whether or
// not the response is withheld. Developer mode publishes the share with no marker.
func TestOneBreakdown_AttributionCoverageNotShown(t *testing.T) {
	type arm struct {
		name           string
		people         int // p1..pN on team eng, each with $10 linked cost and an outcome
		unlinkedBy     int // p1..pN also carry $2 on an unlinked issue
		onlyUnlinkedBy int // extra people with an outcome but only unlinked cost
		subK           int // extra people, each alone in a team: a sub-k "other"
	}
	for _, a := range []arm{
		{"unlinked half below k", 5, 2, 0, 0},
		{"linked half below k", 2, 2, 3, 0},
		{"fully attributed", 5, 0, 0, 0},
		{"no linked spend", 0, 0, 5, 0},
		{"response withheld", 5, 5, 0, 2},
	} {
		t.Run(a.name, func(t *testing.T) {
			h, db := newTestHandler(t)
			now := time.Now().UTC().Add(-time.Hour)
			for i := 1; i <= a.people; i++ {
				dev := fmt.Sprintf("p%d", i)
				seedKAnonDev(t, db, "eng", dev, 10, 2, now)
				if i <= a.unlinkedBy {
					seedCostAt(t, db, dev, store.UnattributedMainBucket, 2, now)
				}
			}
			for i := 1; i <= a.subK; i++ {
				seedKAnonDev(t, db, fmt.Sprintf("solo%d", i), fmt.Sprintf("s%d", i), 3, 1, now)
			}
			for i := 1; i <= a.onlyUnlinkedBy; i++ {
				dev := fmt.Sprintf("q%d", i)
				enrolIn(t, db, "eng", dev)
				seedCostAt(t, db, dev, store.UnattributedMainBucket, 2, now)
				seedOutcomeAt(t, db, dev, "i-"+dev, 2, 1, now)
			}
			seedPseudoSpend(t, db, 12.34, 5.67, now)
			h.SetAggregation(scoring.AggregationTeam, 5)

			resp := getScores(t, h, "/api/v1/scores?since="+scoresSince())
			dq := resp.DataQuality
			if dq == nil {
				t.Fatal("an anonymised response always carries data_quality")
			}
			if withheld := dq.KAnonSuppressed != nil; withheld != (a.name == "response withheld") {
				t.Fatalf("fixture: kanon_suppressed = %+v, want set only on the withheld arm", dq.KAnonSuppressed)
			}
			if dq.AttributedCostShare != nil || dq.AttributionCoverage != "not_shown" {
				t.Errorf("share %v, attribution_coverage %q: want the share omitted and \"not_shown\"",
					dq.AttributedCostShare, dq.AttributionCoverage)
			}
		})
	}

	t.Run("developer mode unchanged", func(t *testing.T) {
		h, db := newTestHandler(t)
		now := time.Now().UTC().Add(-time.Hour)
		seedKAnonDev(t, db, "eng", "p1", 10, 2, now)
		seedCostAt(t, db, "p1", store.UnattributedMainBucket, 2, now)
		h.SetAggregation(scoring.AggregationDeveloper, 5)
		resp := getScores(t, h, "/api/v1/scores?since="+scoresSince())
		if dq := resp.DataQuality; dq == nil || dq.AttributedCostShare == nil || dq.AttributionCoverage != "" || dq.ExcludesUnattributedSpend {
			t.Errorf("developer mode publishes the share with no marker and no exclusion flag; got %+v", dq)
		}
	})
}

// TestOneBreakdown_AttributionShareNotPublishedAtKCarriers pins the #864 engine
// review's RED: the share differences against the published rows and total to one
// person's spend even when each half is carried by k counted people. Team a is
// five people with $10 linked each and one with $3 unlinked; team b is five people
// with $10 unlinked each. Linked has 5 carriers and unlinked 6, both at k = 5, and
// total × (1 − share) − b = $3. On /scores and on both /compare windows, in team
// and division mode, the share is absent and attribution_coverage is "not_shown".
func TestOneBreakdown_AttributionShareNotPublishedAtKCarriers(t *testing.T) {
	h, db := newTestHandler(t)
	day := func(n int) time.Time { return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, n) }
	date := func(n int) string { return day(n).Format("2006-01-02") }
	for w, ts := range []time.Time{day(-25).Add(12 * time.Hour), day(-5).Add(12 * time.Hour)} {
		for i := 1; i <= 5; i++ {
			dev := fmt.Sprintf("a%d", i)
			seedKAnonDevIssue(t, db, "a", dev, fmt.Sprintf("%s-%d", dev, w), 10, 2, ts)
			enrolIn(t, db, "b", fmt.Sprintf("b%d", i))
			seedCostAt(t, db, fmt.Sprintf("b%d", i), store.UnattributedMainBucket, 10, ts)
		}
		enrolIn(t, db, "a", "a6")
		seedCostAt(t, db, "a6", store.UnattributedMainBucket, 3, ts)
	}
	scoresQ := "?since=" + date(-30) + "&until=" + date(-20)
	compareQ := "/api/v1/scores/compare?since_a=" + date(-30) + "&until_a=" + date(-20) + "&since_b=" + date(-10)

	for _, mode := range anonymisedModes {
		h.SetAggregation(mode, 5)
		code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores"+scoresQ, nil)
		if code != http.StatusOK {
			t.Fatalf("%s mode /scores: status %d body %s", mode, code, body)
		}
		resp := decodeScoresBody(t, body)
		if resp.Total == nil || resp.Total.TotalCostUSD != 103 || len(resp.Teams) != 2 {
			t.Fatalf("%s mode fixture: want both groups and the $103 total published; body %s", mode, body)
		}
		if dq := resp.DataQuality; dq == nil || dq.AttributedCostShare != nil || dq.AttributionCoverage != "not_shown" {
			t.Errorf("%s mode /scores: want no attributed_cost_share and attribution_coverage \"not_shown\"; body %s", mode, body)
		}

		code, raw := doRequest(t, h, http.MethodGet, compareQ, nil)
		if code != http.StatusOK {
			t.Fatalf("%s mode /compare: status %d body %s", mode, code, raw)
		}
		var cmp compareResponse
		if err := json.Unmarshal(raw, &cmp); err != nil {
			t.Fatal(err)
		}
		if cmp.Total == nil {
			t.Fatalf("%s mode /compare fixture: want the total published; body %s", mode, raw)
		}
		for name, win := range map[string]compareWindowMeta{"window_a": cmp.WindowA, "window_b": cmp.WindowB} {
			if dq := win.DataQuality; dq == nil || dq.AttributedCostShare != nil || dq.AttributionCoverage != "not_shown" {
				t.Errorf("%s mode /compare %s: want no attributed_cost_share and attribution_coverage \"not_shown\"; body %s", mode, name, raw)
			}
		}
		if bytes.Contains(raw, []byte(`"attributed_cost_share"`)) {
			t.Errorf("%s mode /compare carries attributed_cost_share: %s", mode, raw)
		}
	}
}

// TestOneBreakdown_NoMixedPriceVersionsInAnonymisedModes pins #864's removal of
// data_quality.mixed_price_versions from team and division responses (/scores and
// both /compare windows): it reads every row of the window, pseudo-developer spend
// included. Developer mode, over the same store, still carries it.
func TestOneBreakdown_NoMixedPriceVersionsInAnonymisedModes(t *testing.T) {
	h, db := newTestHandler(t)
	day := func(n int) time.Time { return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, n) }
	date := func(n int) string { return day(n).Format("2006-01-02") }
	for w, ts := range []time.Time{day(-25).Add(12 * time.Hour), day(-5).Add(12 * time.Hour)} {
		for i := 1; i <= 5; i++ {
			dev := fmt.Sprintf("e%d", i)
			seedKAnonDevIssue(t, db, "eng", dev, fmt.Sprintf("%s-%d", dev, w), 10, 2, ts)
			seedPricedCostAt(t, db, dev, fmt.Sprintf("%s-%d-v2", dev, w), 1, 2, ts) // a second price version
		}
	}
	scoresQ := "?since=" + date(-30) + "&until=" + date(-20)
	compareQ := "/api/v1/scores/compare?since_a=" + date(-30) + "&until_a=" + date(-20) + "&since_b=" + date(-10)

	h.SetAggregation(scoring.AggregationDeveloper, 5)
	if _, body := getScoresRaw(t, h, scoresQ); !strings.Contains(body, `"mixed_price_versions"`) {
		t.Fatalf("control: developer mode must carry mixed_price_versions for a two-version window; body %s", body)
	}
	for _, mode := range anonymisedModes {
		h.SetAggregation(mode, 5)
		if _, body := getScoresRaw(t, h, scoresQ); strings.Contains(body, `"mixed_price_versions"`) {
			t.Errorf("%s mode /scores carries mixed_price_versions: %s", mode, body)
		}
		code, body := doRequest(t, h, http.MethodGet, compareQ, nil)
		if code != http.StatusOK {
			t.Fatalf("%s mode /compare: status %d", mode, code)
		}
		if strings.Contains(string(body), `"mixed_price_versions"`) {
			t.Errorf("%s mode /compare carries mixed_price_versions: %s", mode, body)
		}
	}
}
