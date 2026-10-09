package api

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

var updateSealLiveGolden = flag.Bool("update-913-live-golden", false, "rewrite testdata/anonymised_live_913.golden.json")

// sealFixtureMonth is the fixed month every #913 sealer fixture seeds.
var sealFixtureMonth = time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC)

// seedSealFixture seeds May 2026: team "big" (6 people) clears k=5 on its own;
// "t1" (x1, x3, x5) and "t2" (x2, x4) pool into a residual of 5. Developer
// order interleaves the two teams, and these costs sum to 3.6999999999999997 in
// developer order but 3.7 team by team, so the live fold and AggregateFolded
// publish different residual figures.
func seedSealFixture(t *testing.T, db *store.DB) {
	t.Helper()
	registerGated(t, db) // serve has registered its sources, none of them gated (#913-D9)
	at := sealFixtureMonth.Add(9*24*time.Hour + 3*time.Hour)
	for i := 1; i <= 6; i++ {
		seedKAnonDev(t, db, "big", fmt.Sprintf("b%d", i), 10+0.1*float64(i), 2, at)
	}
	for i, cost := range []float64{0.1, 0.2, 2.7, 0.3, 0.4} {
		team := "t1"
		if i%2 == 1 {
			team = "t2"
		}
		seedKAnonDev(t, db, team, fmt.Sprintf("x%d", i+1), cost, 1, at)
	}
}

// TestScores_AnonymisedLiveOutputUnchanged pins the live anonymised /scores body
// (price_table masked, as it moves with every price-table release) against the
// output of e9fee42, the commit before the scoresForWindow split: team k=5 (a
// named team plus an emitted residual), division k=5, and team k=6 (a withheld
// residual).
func TestScores_AnonymisedLiveOutputUnchanged(t *testing.T) {
	h, db := newTestHandler(t)
	seedSealFixture(t, db)
	window := "?since=2026-05-01&until=2026-06-01"

	got := map[string]any{}
	for _, c := range []struct {
		name string
		mode scoring.AggregationMode
		k    int
	}{
		{"team-k5", scoring.AggregationTeam, 5},
		{"division-k5", scoring.AggregationDivision, 5},
		{"team-k6", scoring.AggregationTeam, 6},
	} {
		h.SetAggregation(c.mode, c.k)
		raw, _ := getScoresRaw(t, h, window)
		if _, ok := raw["price_table"]; !ok {
			t.Fatalf("%s: price_table stamp missing", c.name)
		}
		raw["price_table"] = "<price_table>"
		got[c.name] = raw
	}
	gotJSON, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	gotJSON = append(gotJSON, '\n')
	path := filepath.Join("testdata", "anonymised_live_913.golden.json")
	if *updateSealLiveGolden {
		if err := os.WriteFile(path, gotJSON, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	if !bytes.Equal(gotJSON, want) {
		t.Errorf("anonymised /scores changed since e9fee42; diff %s against the output below.\n%s", path, gotJSON)
	}

	// Control: on this fixture the folded body differs from the live one, so a
	// live path switched to AggregateFolded fails the comparison above.
	h.SetAggregation(scoring.AggregationTeam, 5)
	_, live := getScoresRaw(t, h, window)
	start, end := sealFixtureMonth, sealFixtureMonth.AddDate(0, 1, 0)
	folded, _, err := h.scoresForWindow(context.Background(), h.store, scoresQuery{since: start, until: end, scope: store.FleetWide}, true)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(folded); err != nil {
		t.Fatal(err)
	}
	if buf.String() == live {
		t.Error("control: the folded team-k5 body equals the live one, so this fixture cannot catch a switched live path")
	}
}
