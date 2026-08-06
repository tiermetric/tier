package api

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// scoresCostComposition is a minimal decode target for the #234 sidecar.
type scoresCostComposition struct {
	CostComposition *struct {
		TotalCostUSD        float64 `json:"total_cost_usd"`
		AttributedCostUSD   float64 `json:"attributed_cost_usd"`
		UnattributedCostUSD float64 `json:"unattributed_cost_usd"`
		UnattributedShare   float64 `json:"unattributed_share"`
		CacheReadShare      float64 `json:"cache_read_share"`
		PremiumModelShare   float64 `json:"premium_model_share"`
		ByModel             []struct {
			Model   string  `json:"model"`
			Host    string  `json:"host"`
			CostUSD float64 `json:"cost_usd"`
			Share   float64 `json:"share"`
			Premium bool    `json:"premium"`
		} `json:"by_model"`
		ByClass struct {
			InputTok   int64 `json:"input_tok"`
			OutputTok  int64 `json:"output_tok"`
			CacheRead  int64 `json:"cache_read"`
			CacheWrite int64 `json:"cache_write"`
		} `json:"by_class"`
	} `json:"cost_composition"`
}

func seedComposition(t *testing.T, db *store.DB) {
	t.Helper()
	now := time.Now().UTC()
	events := []store.TokenEvent{
		{Developer: "alice", IssueID: "issue-1", Model: "claude-opus-4-8", InputTok: 1000, OutputTok: 200, CacheRead: 3000, CacheWrite5m: 500, CostMicro: 100_000, Source: "jsonl", Fidelity: "realtime", Timestamp: now},
		{Developer: "bob", IssueID: "issue-2", Model: "claude-sonnet-4", InputTok: 2000, OutputTok: 400, CacheRead: 1000, CostMicro: 40_000, Source: "jsonl", Fidelity: "realtime", Timestamp: now},
		{Developer: "alice", IssueID: store.UnattributedIssueID, Model: "claude-sonnet-4", InputTok: 500, CostMicro: 10_000, Source: "jsonl", Fidelity: "realtime", Timestamp: now},
	}
	if err := db.InsertTokenEvents(context.Background(), events); err != nil {
		t.Fatalf("InsertTokenEvents: %v", err)
	}
}

// TestScores_CostCompositionSidecar exercises the #234 sidecar end to end through
// the /scores handler: the block is present, its dollars reconcile
// (attributed + unattributed == total, by_model sums to total), and the two
// levers are correct.
func TestScores_CostCompositionSidecar(t *testing.T) {
	h, db := newTestHandler(t)
	seedComposition(t, db)

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?since=2000-01-01", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores = %d, body %s", code, body)
	}
	var resp scoresCostComposition
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cc := resp.CostComposition
	if cc == nil {
		t.Fatal("cost_composition is absent, want present")
	}

	if math.Abs(cc.TotalCostUSD-0.15) > 1e-9 {
		t.Errorf("total_cost_usd = %v, want 0.15", cc.TotalCostUSD)
	}
	if math.Abs((cc.AttributedCostUSD+cc.UnattributedCostUSD)-cc.TotalCostUSD) > 1e-9 {
		t.Errorf("attributed(%v)+unattributed(%v) != total(%v)", cc.AttributedCostUSD, cc.UnattributedCostUSD, cc.TotalCostUSD)
	}
	if math.Abs(cc.UnattributedCostUSD-0.01) > 1e-9 {
		t.Errorf("unattributed_cost_usd = %v, want 0.01", cc.UnattributedCostUSD)
	}
	if math.Abs(cc.PremiumModelShare-100_000.0/150_000.0) > 1e-9 {
		t.Errorf("premium_model_share = %v, want %v", cc.PremiumModelShare, 100_000.0/150_000.0)
	}
	if math.Abs(cc.CacheReadShare-0.5) > 1e-9 {
		t.Errorf("cache_read_share = %v, want 0.5", cc.CacheReadShare)
	}
	if len(cc.ByModel) != 2 {
		t.Fatalf("by_model has %d rows, want 2", len(cc.ByModel))
	}
	// Sorted desc by cost: opus (premium) first.
	if cc.ByModel[0].Model != "claude-opus-4-8" || !cc.ByModel[0].Premium {
		t.Errorf("by_model[0] = %+v, want claude-opus-4-8 premium", cc.ByModel[0])
	}
	var sum float64
	for _, m := range cc.ByModel {
		sum += m.CostUSD
	}
	if math.Abs(sum-cc.TotalCostUSD) > 1e-9 {
		t.Errorf("sum(by_model cost_usd) = %v, want %v (no residual)", sum, cc.TotalCostUSD)
	}
	if cc.ByClass.CacheRead != 4000 || cc.ByClass.InputTok != 3500 {
		t.Errorf("by_class = %+v, want cache_read 4000 / input 3500", cc.ByClass)
	}
}

// TestScores_CostCompositionTeamMode pins the #185 privacy contract: the
// cost_composition block ships in team-aggregation mode when nothing was suppressed
// (it is a whole-window, name-free aggregate), and the response is genuinely in team
// mode (named `developers` suppressed to an empty array). A regression that gated the
// sidecar behind developer mode, or leaked a per-developer identity into it, is caught
// here.
//
// ⚠️ The original comment justified shipping the sidecar with "so it re-exposes no
// sub-k cohort". #593 measured that claim FALSE: name-free is not the same as safe,
// because the composition total restates whatever the named rows omit, and the
// difference is the suppressed cohort. The sidecar is now withheld alongside `total`
// whenever a residual is suppressed — see TestGetScores_KAnonSuppression_*. This test
// therefore uses a fixture where NOTHING is suppressed, which is what it was really
// about all along.
//
// Note the fixture needs >= scoring.MinKAnonymity (3) contributors: newTeamModeHandler
// takes a requested k, but AggregateTeamsKAnon clamps anything below MinKAnonymity up
// to it, so a 2-developer "k=2" cohort is enforced at 3 and collapses.
func TestScores_CostCompositionTeamMode(t *testing.T) {
	h, db := newTeamModeHandler(t, 3)
	seedComposition(t, db)
	// A third contributor so the single team clears the effective floor of 3 and no
	// residual exists to suppress.
	seedCosts(t, db, "carol", "issue-carol", 1.00)
	seedOutcome(t, db, "carol", "issue-carol", 1.0, 1.0)
	// #593: the work_type SEGMENT is scored over only developers with outcomes in that
	// type, so it is smaller than the window — carol alone would suppress the segment
	// and escalate to the whole response, stripping the sidecar under test.
	padCohort(t, db, "team-x", store.WorkTypeFeature, 3, 1.0)
	for _, dev := range []string{"alice", "bob", "carol"} {
		if err := db.UpsertHierarchy(context.Background(), dev, "team-x", "div", "org"); err != nil {
			t.Fatalf("UpsertHierarchy(%s): %v", dev, err)
		}
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?since=2000-01-01", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores = %d, body %s", code, body)
	}

	// Team mode: named per-developer rows are suppressed. Proven directly on the raw
	// body so a leak anywhere (including inside cost_composition) is caught.
	if bytes.Contains(body, []byte(`"developer":"alice"`)) || bytes.Contains(body, []byte(`"developer":"bob"`)) {
		t.Errorf("team-mode response leaks a developer identity: %s", body)
	}

	var resp scoresCostComposition
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cc := resp.CostComposition
	if cc == nil {
		t.Fatal("cost_composition absent in team mode, want present (it is name-free)")
	}
	if math.Abs((cc.AttributedCostUSD+cc.UnattributedCostUSD)-cc.TotalCostUSD) > 1e-9 {
		t.Errorf("team-mode composition does not reconcile: attributed(%v)+unattributed(%v) != total(%v)",
			cc.AttributedCostUSD, cc.UnattributedCostUSD, cc.TotalCostUSD)
	}
	// The by-model rows carry model/host names, never developer identities.
	for _, m := range cc.ByModel {
		if m.Model == "alice" || m.Model == "bob" {
			t.Errorf("by_model row keyed by a developer identity: %+v", m)
		}
	}
}

// TestScores_CostCompositionOmittedWhenEmpty pins the omit-when-empty rule: a
// window with no token spend ships no cost_composition key (the dashboard panel
// stays hidden), mirroring data_quality (#136).
func TestScores_CostCompositionOmittedWhenEmpty(t *testing.T) {
	h, _ := newTestHandler(t)

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?since=2000-01-01", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores = %d, body %s", code, body)
	}
	var resp scoresCostComposition
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.CostComposition != nil {
		t.Errorf("cost_composition present on empty window, want omitted: %+v", resp.CostComposition)
	}
}
