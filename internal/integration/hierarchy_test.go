//go:build integration

// End-to-end wire test for the org-hierarchy write surface (#232). It reproduces
// the exact failure the structural audit found and proves the fix over real
// HTTP: with --aggregation team (the shipped EU-safe default) and NO hierarchy
// writer, every developer folded into a single anonymous "other" row and seat
// allocation read 0. This drives the new PUT/POST/GET hierarchy endpoints through
// the same mux cmd/tierd mounts, then reads /scores back.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// teamRowJSON mirrors the api.teamScoreJSON wire shape (unexported there) so this
// external-package test can decode /scores.
type teamRowJSON struct {
	Team           string  `json:"team"`
	WeightedPoints float64 `json:"weighted_points"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
}

type scoresRespJSON struct {
	Developers []json.RawMessage `json:"developers"`
	Teams      []teamRowJSON     `json:"teams"`
	Total      *teamRowJSON      `json:"total"`
}

// newTeamModeServer builds the tierd API composition in team-aggregation mode
// with the given k floor, returning the running server and the backing store so
// the test can seed contributing developers directly.
func newTeamModeServer(t *testing.T, k int) (*httptest.Server, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "hierarchy.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := api.New(db, quiet, apiToken, nil, "integration", api.RateLimitConfig{})
	h.SetAggregation(scoring.AggregationTeam, k)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, db
}

// seedContributor seeds a developer who contributes to a cohort: a cost row and a
// merged outcome, so it counts toward the k-anonymity floor.
func seedContributor(t *testing.T, db *store.DB, dev string) {
	t.Helper()
	ctx := context.Background()
	issue := "issue-" + dev
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer: dev,
		IssueID:   issue,
		Model:     "claude-sonnet-4",
		InputTok:  2000,
		CostMicro: store.DollarsToMicro(10),
		Source:    "jsonl",
		Fidelity:  "realtime",
		Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertTokenEvent(%s): %v", dev, err)
	}
	if _, err := db.InsertOutcome(ctx, store.Outcome{
		Developer: dev,
		IssueID:   issue,
		Weight:    3,
		Quality:   1,
		Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertOutcome(%s): %v", dev, err)
	}
}

// postJSON sends a JSON body with the write token and returns status + body.
func postHierarchyJSON(t *testing.T, method, url string, body any) (int, []byte) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func getTeamScores(t *testing.T, srvURL string) scoresRespJSON {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srvURL+"/api/v1/scores", nil)
	req.Header.Set("Authorization", "Bearer "+apiToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /scores: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /scores: status = %d, body = %s", resp.StatusCode, body)
	}
	var out scoresRespJSON
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal /scores: %v; body = %s", err, body)
	}
	// No developer name may ever appear in a team-mode response.
	if len(out.Developers) != 0 {
		t.Errorf("team mode leaked %d developer rows", len(out.Developers))
	}
	return out
}

// TestHierarchyWriteSurface_TeamModeNamesTeams is the #232 regression: the same
// 12 contributing developers render as ONE anonymous "other" row with no
// hierarchy (the audited bug), and as named team aggregates once the bulk import
// runs — proving the write surface actually unblocks the required team mode.
func TestHierarchyWriteSurface_TeamModeNamesTeams(t *testing.T) {
	// Fixture: 12 developers across 3 teams under k=5.
	//   alpha:   5 contributing -> clears the floor -> named
	//   charlie: 5 contributing -> clears the floor -> named
	//   bravo:   2 contributing -> below the floor -> folds into "other"
	teams := map[string]int{"alpha": 5, "bravo": 2, "charlie": 5}
	type item struct {
		Developer string `json:"developer"`
		Team      string `json:"team"`
		Org       string `json:"org"`
	}

	period := time.Now().UTC().Format("2006-01")
	since := time.Now().UTC().AddDate(0, -1, 0)

	t.Run("no hierarchy -> single anonymous row + zero seat allocation (the bug)", func(t *testing.T) {
		srv, db := newTeamModeServer(t, 5)
		for team, n := range teams {
			for i := 0; i < n; i++ {
				seedContributor(t, db, fmt.Sprintf("nohier-%s-%d", team, i))
			}
		}
		// An org invoice exists, but with no hierarchy there are no seats to
		// allocate it across.
		seedOrgSpend(t, db, "acme", period, 1200)
		scores := getTeamScores(t, srv.URL)
		// The audited failure: every developer maps to the unnamed team "" and the
		// whole population renders as ONE anonymous aggregate row — no per-team
		// breakdown, forever. (Its label is "" here rather than "other" only because
		// the single cohort itself clears k; the defect is the collapse to one row.)
		if len(scores.Teams) != 1 {
			t.Fatalf("without hierarchy, expected exactly one anonymous row; got %+v", scores.Teams)
		}
		if paid := devAlloc(t, db, "nohier-alpha-0", since); paid != 0 {
			t.Errorf("without hierarchy, seat allocation must read 0; got %v", paid)
		}
	})

	t.Run("bulk import -> named teams, sub-k folds to other", func(t *testing.T) {
		srv, db := newTeamModeServer(t, 5)
		var batch []item
		for team, n := range teams {
			for i := 0; i < n; i++ {
				dev := fmt.Sprintf("%s-%d", team, i)
				seedContributor(t, db, dev)
				batch = append(batch, item{Developer: dev, Team: team, Org: "acme"})
			}
		}
		seedOrgSpend(t, db, "acme", period, 1200)
		// Bulk-import all 12 in one call over the wire.
		code, body := postHierarchyJSON(t, http.MethodPost, srv.URL+"/api/v1/org_hierarchy", batch)
		if code != http.StatusCreated {
			t.Fatalf("bulk import: status = %d, body = %s", code, body)
		}

		scores := getTeamScores(t, srv.URL)
		byTeam := map[string]teamRowJSON{}
		for _, ts := range scores.Teams {
			byTeam[ts.Team] = ts
		}
		// Three rows: alpha + charlie named (5 contributing each, clearing k=5),
		// bravo folded into "other" (2 contributing, below the floor) — NOT the
		// single-"other" collapse the bug produced.
		if len(scores.Teams) != 3 {
			t.Fatalf("expected 3 team rows (alpha, charlie, other); got %+v", scores.Teams)
		}
		if _, ok := byTeam["alpha"]; !ok {
			t.Errorf("team 'alpha' (5 contributing) must be named; got %+v", scores.Teams)
		}
		if _, ok := byTeam["charlie"]; !ok {
			t.Errorf("team 'charlie' (5 contributing) must be named; got %+v", scores.Teams)
		}
		if _, ok := byTeam["bravo"]; ok {
			t.Errorf("sub-k team 'bravo' (2 contributing) must fold into 'other', not be named; got %+v", scores.Teams)
		}
		other, ok := byTeam[scoring.OtherCohort]
		if !ok {
			t.Fatalf("expected an 'other' bucket for the sub-k team; got %+v", scores.Teams)
		}
		// bravo's 2 contributors: 2 outcomes x weight 3 = 6 points, 2 x $10 = $20.
		if other.WeightedPoints != 6 || other.TotalCostUSD != 20 {
			t.Errorf("'other' = points %v cost %v, want points 6 cost 20 (bravo's 2 devs preserved)",
				other.WeightedPoints, other.TotalCostUSD)
		}

		// Second half of the audited bug: the bulk import OPENS a period_membership
		// seat per developer (UpsertHierarchies runs the #41 seat path), so the
		// $1200 org invoice now allocates across 12 seats — $100 each — instead of
		// reading 0. This is the CFO-facing Spend Leverage org path the audit called
		// dead code for a stranger.
		paid := devAlloc(t, db, "alpha-0", since)
		if paid <= 0 {
			t.Errorf("after import, seat allocation must be non-zero; got %v", paid)
		}
		if paid != 100 {
			t.Errorf("allocated spend = %v, want 100 ($1200 / 12 seats)", paid)
		}
	})
}

// seedOrgSpend records an org-level invoice for the period.
func seedOrgSpend(t *testing.T, db *store.DB, org, period string, usd float64) {
	t.Helper()
	if err := db.InsertOrgActualSpend(context.Background(), store.OrgActualSpend{
		Org:             org,
		Period:          period,
		ActualPaidMicro: store.DollarsToMicro(usd),
		Timestamp:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertOrgActualSpend: %v", err)
	}
}

// devAlloc reads a developer's allocated actual spend — the seat-allocation read
// that returns 0 until period_membership has an open seat for them (#41/#232).
func devAlloc(t *testing.T, db *store.DB, dev string, since time.Time) float64 {
	t.Helper()
	paid, err := db.ActualSpendForDeveloper(context.Background(), dev, since)
	if err != nil {
		t.Fatalf("ActualSpendForDeveloper(%s): %v", dev, err)
	}
	return paid
}
