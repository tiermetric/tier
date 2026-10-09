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
	ActualPaidUSD  float64 `json:"actual_paid_usd"`
}

type scoresRespJSON struct {
	Developers  []json.RawMessage `json:"developers"`
	Teams       []teamRowJSON     `json:"teams"`
	Total       *teamRowJSON      `json:"total"`
	DataQuality struct {
		KAnonSuppressed    *json.RawMessage `json:"kanon_suppressed"`
		UncountedActiveIDs struct {
			NotOnRoster int `json:"not_on_roster"`
		} `json:"uncounted_active_ids"`
	} `json:"data_quality"`
}

// withheldDeclaration decodes data_quality.kanon_suppressed; ok is false when
// the response declared no withhold.
func withheldDeclaration(t *testing.T, s scoresRespJSON) (developers int, withheldTeams, ok bool) {
	t.Helper()
	if s.DataQuality.KAnonSuppressed == nil {
		return 0, false, false
	}
	var d struct {
		Developers    int  `json:"developers"`
		WithheldTeams bool `json:"withheld_teams"`
	}
	if err := json.Unmarshal(*s.DataQuality.KAnonSuppressed, &d); err != nil {
		t.Fatalf("decode kanon_suppressed: %v", err)
	}
	return d.Developers, d.WithheldTeams, true
}

// padResidual rosters n contributors, each alone in a team of their own, so they
// fold into "other" and lift it to k: a readback that must publish named rows
// beside a sub-k group uses it, since #864 withholds the whole response otherwise.
func padResidual(t *testing.T, db *store.DB, org string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		dev := fmt.Sprintf("pad-%d", i)
		if err := db.UpsertHierarchy(context.Background(), dev, "pad-team-"+dev, "", org, "test:fixture"); err != nil {
			t.Fatalf("UpsertHierarchy(%s): %v", dev, err)
		}
		seedContributor(t, db, dev)
	}
}

// newTeamModeServer builds the tierd API composition in team-aggregation mode
// with the given k floor, returning the running server, its handler and the
// backing store so the test can seed contributing developers directly.
func newTeamModeServer(t *testing.T, k int) (*httptest.Server, *api.Handler, *store.DB) {
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
	return srv, h, db
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

// getTeamScores is month's anonymised body as a seal computes it (#913): a team
// or division serve publishes only sealed months, and these fixtures read the
// open month twice around a change, which a sealed month never reflects.
func getTeamScores(t *testing.T, h *api.Handler, month string) scoresRespJSON {
	t.Helper()
	p, err := api.ParsePeriod(month)
	if err != nil {
		t.Fatalf("parse period %q: %v", month, err)
	}
	body, err := h.RecomputeSealedBody(context.Background(), p)
	if err != nil {
		t.Fatalf("seal computation of %s: %v", month, err)
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
		_, h, db := newTeamModeServer(t, 5)
		for team, n := range teams {
			for i := 0; i < n; i++ {
				seedContributor(t, db, fmt.Sprintf("nohier-%s-%d", team, i))
			}
		}
		// An org invoice exists, but with no hierarchy there are no seats to
		// allocate it across.
		seedOrgSpend(t, db, "acme", period, 1200)
		scores := getTeamScores(t, h, period)
		// The audited failure: every developer maps to the unnamed team "" — no
		// per-team breakdown, forever. Before #856 that population rendered as ONE
		// anonymous aggregate row. Since #856 an id off the roster does not count
		// toward k, so with no hierarchy nobody counts and the whole population is
		// withheld, declared, with all 12 ids reported as not on the roster.
		if len(scores.Teams) != 0 {
			t.Fatalf("without hierarchy, expected no row (nobody is on the roster, #856); got %+v", scores.Teams)
		}
		if scores.DataQuality.KAnonSuppressed == nil {
			t.Error("without hierarchy, the withhold must be declared in data_quality.kanon_suppressed")
		}
		if got := scores.DataQuality.UncountedActiveIDs.NotOnRoster; got != 12 {
			t.Errorf("uncounted_active_ids.not_on_roster = %d, want 12", got)
		}
		if paid := devAlloc(t, db, "nohier-alpha-0", since); paid != 0 {
			t.Errorf("without hierarchy, seat allocation must read 0; got %v", paid)
		}
	})

	t.Run("bulk import -> named teams, sub-k folds to other", func(t *testing.T) {
		srv, h, db := newTeamModeServer(t, 5)
		var batch []item
		for team, n := range teams {
			for i := 0; i < n; i++ {
				batch = append(batch, item{Developer: fmt.Sprintf("%s-%d", team, i), Team: team, Org: "acme"})
			}
		}
		// The invoice's month began before the import, so under #886 every seat's
		// allocation reaches the residual as a paid-only row — which fills no k seat,
		// so the bravo residual below stays withheld.
		seedOrgSpend(t, db, "acme", period, 1200)
		// Bulk-import all 12 in one call over the wire, BEFORE their activity:
		// membership is dated by the server clock at the import (#886), and activity
		// from before a developer's first assignment stays in "other".
		code, body := postHierarchyJSON(t, http.MethodPost, srv.URL+"/api/v1/org_hierarchy", batch)
		if code != http.StatusCreated {
			t.Fatalf("bulk import: status = %d, body = %s", code, body)
		}
		for _, it := range batch {
			seedContributor(t, db, it.Developer)
		}

		scores := getTeamScores(t, h, period)
		// alpha and charlie (5 contributing each) clear k=5 and bravo's 2 fold into
		// "other", which is below the floor, so the WHOLE response is withheld (#593,
		// #864), the named rows included.
		//
		// The bug this test guards is unchanged and still asserted: the import must not
		// collapse EVERYTHING into a single residual. The declared count is the proof:
		// a collapsed import leaves one 12-person residual, which clears k and is
		// published with nothing withheld; a correct one withholds bravo's 2 alone.
		if len(scores.Teams) != 0 || scores.Total != nil {
			t.Fatalf("bravo's residual is below k=5, so the whole response must be withheld; got teams %+v total %+v",
				scores.Teams, scores.Total)
		}
		devs, withheldTeams, declared := withheldDeclaration(t, scores)
		if !declared || !withheldTeams || devs != 2 {
			t.Fatalf("want the withhold of bravo's 2 people declared with withheld_teams; got declared=%v developers=%d withheld_teams=%v",
				declared, devs, withheldTeams)
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

		// Padded readback: three more people in "other" lift it to k, so the named
		// teams publish through the whole composition and bravo stays folded.
		padResidual(t, db, "acme", 3)
		padded := getTeamScores(t, h, period)
		names := map[string]bool{}
		for _, row := range padded.Teams {
			names[row.Team] = true
		}
		if !names["alpha"] || !names["charlie"] || !names[scoring.OtherCohort] || names["bravo"] || padded.Total == nil {
			t.Errorf("padded: want alpha, charlie and other named with the total, bravo folded; got %+v total %v",
				padded.Teams, padded.Total)
		}
		if padded.DataQuality.KAnonSuppressed != nil {
			t.Errorf("padded: nothing may be declared withheld; got %s", *padded.DataQuality.KAnonSuppressed)
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
