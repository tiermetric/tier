package integration

import (
	"encoding/json"
	"testing"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// TestPartitionReplay keeps total diff, captured cost and quality fixed.
func TestPartitionReplay(t *testing.T) {
	const costUSD = 20.0
	type receipt struct {
		Scenario         string  `json:"scenario"`
		Lines            int     `json:"lines"`
		Files            int     `json:"files"`
		PRs              int     `json:"prs"`
		Issues           int     `json:"issues"`
		Points           float64 `json:"points"`
		PointsPer1000USD float64 `json:"points_per_1000_usd"`
		Amplification    float64 `json:"amplification"`
	}
	cases := []struct {
		name        string
		prs, issues int
		want        float64
	}{
		{"unsplit", 1, 1, 8},
		{"same_issue_split", 4, 1, 20},
		{"cross_issue_split", 4, 4, 20},
	}
	for _, tc := range cases {
		outcomes := make([]scoring.Outcome, tc.prs)
		for i := range outcomes {
			outcomes[i] = scoring.Outcome{IssueID: "issue-a", Weight: store.GitHeuristic(1200/tc.prs, 8/tc.prs), Quality: 1}
			if tc.issues > 1 {
				outcomes[i].IssueID = string(rune('a' + i))
			}
		}
		s := scoring.ComputeDeveloper("synthetic", outcomes, costUSD, costUSD, 0)
		r := receipt{tc.name, 1200, 8, tc.prs, tc.issues, s.WeightedPoints, s.TIER, s.WeightedPoints / 8}
		if r.Points != tc.want || r.PointsPer1000USD != tc.want/0.02 || r.Amplification != tc.want/8 {
			t.Fatalf("replay: %+v", r)
		}
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(string(data))
	}
}
