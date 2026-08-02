package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// seedPricedCost inserts a cost-bearing, token-backed event stamped at an EXPLICIT
// price_version (#293), so a test can build a /scores window that spans one or more
// price_table versions. Mirrors seedCosts (2000 input tokens, above the #136
// tripwire, so the issue is not zero-token flagged) but pins the version the store
// would otherwise stamp from the active table.
func seedPricedCost(t *testing.T, db *store.DB, developer, issueID string, costUSD float64, version int) {
	t.Helper()
	seedPricedCostAt(t, db, developer, issueID, costUSD, version, time.Now().UTC())
}

// seedPricedCostAt is seedPricedCost with an explicit timestamp, so a test can
// place a priced event inside or outside a chosen [since, until) score window.
func seedPricedCostAt(t *testing.T, db *store.DB, developer, issueID string, costUSD float64, version int, ts time.Time) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer:    developer,
		IssueID:      issueID,
		Model:        "claude-sonnet-4",
		InputTok:     2000,
		CostMicro:    store.DollarsToMicro(costUSD),
		Source:       "jsonl",
		Fidelity:     "realtime",
		PriceVersion: version,
		Timestamp:    ts.UTC(),
	}); err != nil {
		t.Fatalf("InsertTokenEvent (v%d): %v", version, err)
	}
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestGetScores_MixedPriceVersionsWarn is the #293 flagship: when the scored
// window's token_events were priced under MORE than one price_table version — the
// legitimate mix #233's immutable cost_micro creates (old rows at v1, new rows at
// v2) — /scores must surface the distinct versions in data_quality so the single
// top-level price_table.version stamp does not read as false uniformity.
func TestGetScores_MixedPriceVersionsWarn(t *testing.T) {
	h, db := newTestHandler(t)
	seedPricedCost(t, db, "alice", "issue-1", 4.0, 1)
	seedPricedCost(t, db, "alice", "issue-2", 4.0, 2)
	seedOutcome(t, db, "alice", "issue-1", 3, 1)
	seedOutcome(t, db, "alice", "issue-2", 3, 1)

	resp := decodeScores(t, h)

	if resp.DataQuality == nil {
		t.Fatalf("data_quality absent; want the mixed_price_versions WARN")
	}
	if want := []int{1, 2}; !sameInts(resp.DataQuality.MixedPriceVersions, want) {
		t.Errorf("mixed_price_versions = %v, want %v (ascending distinct versions; len is the count)",
			resp.DataQuality.MixedPriceVersions, want)
	}
}

// TestGetScores_SinglePriceVersionNoWarn is the negative control: a window whose
// token_events all priced under ONE version carries no mixed-version signal. The
// data_quality block still ships the always-present honest-coverage fields (#351);
// only the mixed-version signal is omit-when-clean.
func TestGetScores_SinglePriceVersionNoWarn(t *testing.T) {
	h, db := newTestHandler(t)
	seedPricedCost(t, db, "alice", "issue-1", 4.0, 1)
	seedPricedCost(t, db, "alice", "issue-2", 4.0, 1)
	seedOutcome(t, db, "alice", "issue-1", 3, 1)
	seedOutcome(t, db, "alice", "issue-2", 3, 1)

	resp := decodeScores(t, h)

	if resp.DataQuality != nil && len(resp.DataQuality.MixedPriceVersions) > 0 {
		t.Errorf("mixed-version signal present for a single-version window: %+v", resp.DataQuality)
	}
}

// TestGetScores_EmptyWindowNoMixedVersionWarn pins that an empty window neither
// crashes the distinct-version read nor emits a spurious WARN.
func TestGetScores_EmptyWindowNoMixedVersionWarn(t *testing.T) {
	h, _ := newTestHandler(t)

	resp := decodeScores(t, h)

	if resp.DataQuality != nil {
		t.Errorf("data_quality present for an empty window: %+v", resp.DataQuality)
	}
}

// TestGetScores_MixedPriceVersionsCoexistWithZeroToken proves the WARN is ADDITIVE
// to the existing #136 zero-token block: a window that is both mixed-version AND
// carries a zero-token outcome must surface both signals in the one data_quality
// block, neither clobbering the other.
func TestGetScores_MixedPriceVersionsCoexistWithZeroToken(t *testing.T) {
	h, db := newTestHandler(t)
	// Two price_table versions in the window.
	seedPricedCost(t, db, "alice", "issue-1", 4.0, 1)
	seedPricedCost(t, db, "alice", "issue-2", 4.0, 2)
	seedOutcome(t, db, "alice", "issue-1", 3, 1)
	seedOutcome(t, db, "alice", "issue-2", 3, 1)
	// Plus a zero-token outcome (its issue recorded no tokens).
	seedOutcome(t, db, "alice", "issue-off", 5, 1)

	resp := decodeScores(t, h)

	if resp.DataQuality == nil {
		t.Fatalf("data_quality absent; want BOTH the zero-token and mixed-version signals")
	}
	if want := []int{1, 2}; !sameInts(resp.DataQuality.MixedPriceVersions, want) {
		t.Errorf("mixed_price_versions = %v, want %v", resp.DataQuality.MixedPriceVersions, want)
	}
	found := false
	for _, z := range resp.DataQuality.ZeroTokenOutcomes {
		if z.Developer == "alice" && z.IssueID == "issue-off" {
			found = true
		}
	}
	if !found {
		t.Errorf("zero_token_outcomes missing {alice, issue-off}; got %+v (mixed-version WARN must not clobber it)",
			resp.DataQuality.ZeroTokenOutcomes)
	}
}

// TestGetScores_MixedPriceVersionsWindowScoped proves the WARN describes the
// SCORED [since, until) window, not all history: a v1 event OUTSIDE the requested
// window must not raise a spurious mixed-version banner, while widening `since` to
// include it does. This pins that DistinctPriceVersionsWindow is threaded the same
// window as every scoring read. It also asserts the top-level price_table.version
// stamp stays populated alongside the mix — the "false uniformity" the WARN exists
// to qualify.
func TestGetScores_MixedPriceVersionsWindowScoped(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	// v1 far in the past (outside the default ~90-day window); v2 today (inside it).
	seedPricedCostAt(t, db, "alice", "issue-old", 4.0, 1, now.AddDate(0, 0, -200))
	seedPricedCostAt(t, db, "alice", "issue-new", 4.0, 2, now)

	// Default window: only v2 is in range → uniform → no mixed-version WARN. (The
	// honest-coverage fields still ship, #351 — this cost-only window even flags an
	// unjoined developer; only the mixed-version signal is asserted absent here.)
	resp := decodeScores(t, h)
	if resp.DataQuality != nil && len(resp.DataQuality.MixedPriceVersions) > 0 {
		t.Errorf("mixed-version signal present for a default window that contains only v2: %+v", resp.DataQuality)
	}

	// Widened window (since=2000): both v1 and v2 in range → mixed → WARN.
	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?since=2000-01-01", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, body)
	}
	var wide scoresResponse
	if err := json.Unmarshal(body, &wide); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wide.DataQuality == nil {
		t.Fatalf("data_quality absent for a window spanning v1 and v2; want the WARN")
	}
	if want := []int{1, 2}; !sameInts(wide.DataQuality.MixedPriceVersions, want) {
		t.Errorf("mixed_price_versions = %v, want %v", wide.DataQuality.MixedPriceVersions, want)
	}
	// The single top-level stamp is still emitted (the active table) next to the mix
	// — that coexistence is precisely what the WARN qualifies.
	if wide.PriceTable.Version == 0 {
		t.Errorf("price_table.version is unset alongside a mixed-version WARN; want the active version stamped")
	}
}

// TestGetScores_MixedPriceVersionsTeamMode proves the WARN carries in
// team-aggregation mode (#185) — the field is name-free (version integers only),
// so it does not defeat k-anonymity — and that no developer name leaks alongside it.
func TestGetScores_MixedPriceVersionsTeamMode(t *testing.T) {
	h, db := newTeamModeHandler(t, 3)
	ctx := context.Background()
	members := []struct {
		dev, issue string
		ver        int
	}{
		{"alice", "issue-a", 1},
		{"bob", "issue-b", 1},
		{"carol", "issue-c", 2},
	}
	for _, m := range members {
		seedPricedCost(t, db, m.dev, m.issue, 4.0, m.ver)
		seedOutcome(t, db, m.dev, m.issue, 3, 1)
		if err := db.UpsertHierarchy(ctx, m.dev, "platform", "div", "org"); err != nil {
			t.Fatalf("UpsertHierarchy(%s): %v", m.dev, err)
		}
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, body)
	}
	var resp scoresResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.DataQuality == nil {
		t.Fatalf("data_quality absent in team mode; the name-free mixed-version WARN must carry")
	}
	if want := []int{1, 2}; !sameInts(resp.DataQuality.MixedPriceVersions, want) {
		t.Errorf("mixed_price_versions = %v, want %v", resp.DataQuality.MixedPriceVersions, want)
	}
	for _, name := range []string{"alice", "bob", "carol"} {
		if strings.Contains(string(body), name) {
			t.Errorf("team-mode /scores leaked developer name %q: %s", name, body)
		}
	}
}
