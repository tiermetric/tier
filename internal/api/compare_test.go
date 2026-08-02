package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// Two disjoint windows used across the compare tests. Absolute past dates so the
// (unconfigured) retention horizon never trips, and far enough apart that the
// attributable token window of one never bleeds into the other.
var (
	winAInstant = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	winBInstant = time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
)

const (
	compareWindowA = "since_a=2026-01-01&until_a=2026-02-01"
	compareWindowB = "since_b=2026-04-01&until_b=2026-05-01"
)

func compareURL() string {
	return "/api/v1/scores/compare?" + compareWindowA + "&" + compareWindowB
}

// seedCostAt (an instant-parameterized cost-bearing token_event that also clears
// the #136 zero-token tripwire) is defined in until_window_test.go and reused here.

// seedOutcomeAt inserts a merged-PR outcome at an explicit instant with a unique
// merge SHA so multiple outcomes never collapse under the dedup index.
func seedOutcomeAt(t *testing.T, db *store.DB, dev, issue string, weight, quality float64, ts time.Time) {
	t.Helper()
	if _, err := db.InsertOutcome(context.Background(), store.Outcome{
		Developer:      dev,
		IssueID:        issue,
		Weight:         weight,
		Quality:        quality,
		MergeCommitSHA: "sha-" + dev + "-" + issue,
		Timestamp:      ts,
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}
}

// getCompare drives the full mux and decodes the compare response.
func getCompare(t *testing.T, h *Handler, url string) (int, compareResponse) {
	t.Helper()
	code, body := doRequest(t, h, "GET", url, nil)
	var resp compareResponse
	if code == http.StatusOK {
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode compare response: %v (body=%s)", err, body)
		}
	}
	return code, resp
}

func devDelta(t *testing.T, resp compareResponse, dev string) developerDeltaJSON {
	t.Helper()
	for _, d := range resp.Developers {
		if d.Developer == dev {
			return d
		}
	}
	t.Fatalf("developer %q not found in compare response", dev)
	return developerDeltaJSON{}
}

// TestScoresCompare_DeltasKnownBeforeAfter pins exact deltas for a hand-computed
// before/after. alice moves TIER 300 -> 600; bob exists only in window A.
func TestScoresCompare_DeltasKnownBeforeAfter(t *testing.T) {
	h, db := newTestHandler(t)

	// Window A: alice points 3 over $10 -> TIER 300. bob points 2 over $10 -> 200.
	seedOutcomeAt(t, db, "alice", "a-1", 3.0, 1.0, winAInstant)
	seedCostAt(t, db, "alice", "a-1", 10.0, winAInstant)
	seedOutcomeAt(t, db, "bob", "a-2", 2.0, 1.0, winAInstant)
	seedCostAt(t, db, "bob", "a-2", 10.0, winAInstant)

	// Window B: alice points 6 over $10 -> TIER 600. bob absent.
	seedOutcomeAt(t, db, "alice", "b-1", 6.0, 1.0, winBInstant)
	seedCostAt(t, db, "alice", "b-1", 10.0, winBInstant)

	code, resp := getCompare(t, h, compareURL())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Mode != "developer" {
		t.Fatalf("mode = %q, want developer", resp.Mode)
	}

	alice := devDelta(t, resp, "alice")
	if !alice.PresentA || !alice.PresentB {
		t.Fatalf("alice present_a/b = %v/%v, want true/true", alice.PresentA, alice.PresentB)
	}
	if alice.A.TIER != 300 || alice.B.TIER != 600 {
		t.Fatalf("alice TIER a/b = %v/%v, want 300/600", alice.A.TIER, alice.B.TIER)
	}
	if alice.DeltaTIER != 300 {
		t.Fatalf("alice delta_tier = %v, want 300", alice.DeltaTIER)
	}
	if alice.DeltaWeightedPoints != 3 {
		t.Fatalf("alice delta_weighted_points = %v, want 3", alice.DeltaWeightedPoints)
	}
	if alice.DeltaTotalCostUSD != 0 {
		t.Fatalf("alice delta_total_cost_usd = %v, want 0", alice.DeltaTotalCostUSD)
	}

	bob := devDelta(t, resp, "bob")
	if !bob.PresentA || bob.PresentB {
		t.Fatalf("bob present_a/b = %v/%v, want true/false", bob.PresentA, bob.PresentB)
	}
	// A one-window developer must not have a fabricated delta against an absent side.
	if bob.DeltaTIER != 0 || bob.Significant {
		t.Fatalf("bob delta/significant = %v/%v, want 0/false", bob.DeltaTIER, bob.Significant)
	}

	if resp.Total == nil {
		t.Fatal("total delta missing")
	}
	// Grand total window A points = 3 (alice) + 2 (bob) = 5 over $20 -> TIER 250.
	// Window B points = 6 over $10 -> TIER 600. delta = 350.
	if resp.Total.A.TIER != 250 || resp.Total.B.TIER != 600 {
		t.Fatalf("total TIER a/b = %v/%v, want 250/600", resp.Total.A.TIER, resp.Total.B.TIER)
	}
	if resp.Total.DeltaTIER != 350 {
		t.Fatalf("total delta_tier = %v, want 350", resp.Total.DeltaTIER)
	}
}

// TestScoresCompare_MatchesScoresForSameWindow pins the loadWindow-sharing
// invariant behind #277: a developer's A-side on /scores/compare must equal the
// row /scores returns for the SAME window (TIER, points, cost, CI, ranked). This is
// the behavior-preserving guarantee of the loadWindow extraction — the compare
// endpoint and /scores compute a window through one code path, so their numbers can
// never drift.
func TestScoresCompare_MatchesScoresForSameWindow(t *testing.T) {
	h, db := newTestHandler(t)
	// A ranked developer (>= MinRankedOutcomes, non-trivial spend, no zero-token) so
	// the row carries a real bootstrap CI to cross-check.
	seedRankedDeveloper(t, db, "alice", "wa", 5, 1.0, 2.0, winAInstant)

	// /scores over exactly window A.
	code, body := doRequest(t, h, "GET", "/api/v1/scores?since=2026-01-01&until=2026-02-01", nil)
	if code != http.StatusOK {
		t.Fatalf("/scores status = %d, want 200; body=%s", code, body)
	}
	var scores struct {
		Developers []struct {
			Developer      string  `json:"developer"`
			TIER           float64 `json:"tier"`
			WeightedPoints float64 `json:"weighted_points"`
			TotalCostUSD   float64 `json:"total_cost_usd"`
			CostPerPoint   float64 `json:"cost_per_point"`
			SampleN        int     `json:"sample_n"`
			CILow          float64 `json:"ci_low"`
			CIHigh         float64 `json:"ci_high"`
			Ranked         bool    `json:"ranked"`
		} `json:"developers"`
	}
	if err := json.Unmarshal(body, &scores); err != nil {
		t.Fatalf("decode /scores: %v; body=%s", err, body)
	}
	if len(scores.Developers) != 1 {
		t.Fatalf("/scores developers = %d, want 1", len(scores.Developers))
	}
	sd := scores.Developers[0]

	// The compare A-side over the same window A (window B empty is irrelevant here).
	code, resp := getCompare(t, h, compareURL())
	if code != http.StatusOK {
		t.Fatalf("compare status = %d, want 200", code)
	}
	alice := devDelta(t, resp, "alice")
	if !alice.PresentA {
		t.Fatal("alice must be present in window A")
	}
	a := alice.A
	if a.TIER != sd.TIER || a.WeightedPoints != sd.WeightedPoints || a.TotalCostUSD != sd.TotalCostUSD {
		t.Fatalf("compare A-side score = {tier %v, wp %v, cost %v}, /scores = {tier %v, wp %v, cost %v}",
			a.TIER, a.WeightedPoints, a.TotalCostUSD, sd.TIER, sd.WeightedPoints, sd.TotalCostUSD)
	}
	if a.SampleN != sd.SampleN || a.Ranked != sd.Ranked {
		t.Fatalf("compare A-side sample_n/ranked = %d/%v, /scores = %d/%v", a.SampleN, a.Ranked, sd.SampleN, sd.Ranked)
	}
	// cost_per_point (#239) parity: the compare A-side carries the SAME points-guarded
	// value /scores does. alice has accepted points here, so it is non-nil and equals
	// the engine's cost_per_point verbatim (#472 nulls it only for a zero-point row).
	if a.CostPerPoint == nil {
		t.Fatal("compare A-side cost_per_point is nil for a row with accepted points")
	}
	if *a.CostPerPoint != sd.CostPerPoint {
		t.Fatalf("compare A-side cost_per_point = %v, /scores engine = %v -- must be identical", *a.CostPerPoint, sd.CostPerPoint)
	}
	// The bootstrap CI is a pure function of the developer's own outcomes+cost with a
	// fixed seed, so it must be BIT-identical across the two endpoints (#133/#277).
	if a.CILow != sd.CILow || a.CIHigh != sd.CIHigh {
		t.Fatalf("compare A-side CI = [%v,%v], /scores CI = [%v,%v] — must be identical",
			a.CILow, a.CIHigh, sd.CILow, sd.CIHigh)
	}
}

// TestCIDisjoint covers the pure significance predicate at the boundaries.
func TestCIDisjoint(t *testing.T) {
	cases := []struct {
		name               string
		aLo, aHi, bLo, bHi float64
		want               bool
	}{
		{"a strictly below b", 1, 2, 3, 4, true},
		{"b strictly below a", 3, 4, 1, 2, true},
		{"touching (aHi == bLo) overlaps", 1, 2, 2, 3, false},
		{"overlapping", 1, 3, 2, 4, false},
		{"identical point intervals", 5, 5, 5, 5, false},
		{"nested", 1, 10, 3, 4, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ciDisjoint(tc.aLo, tc.aHi, tc.bLo, tc.bHi); got != tc.want {
				t.Fatalf("ciDisjoint(%v,%v,%v,%v) = %v, want %v",
					tc.aLo, tc.aHi, tc.bLo, tc.bHi, got, tc.want)
			}
		})
	}
}

// seedRankedDeveloper gives a developer n outcomes (each its own issue, weight w,
// quality 1, plus cost so the issue clears the token tripwire) at ts. With n >= 3
// and total cost >= $5 and no zero-token issues, the developer is RANKED.
func seedRankedDeveloper(t *testing.T, db *store.DB, dev, tag string, n int, w, costPer float64, ts time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		issue := tag + "-" + dev + "-" + string(rune('a'+i))
		seedOutcomeAt(t, db, dev, issue, w, 1.0, ts)
		seedCostAt(t, db, dev, issue, costPer, ts)
	}
}

// TestScoresCompare_SignificanceFiresWhenCIsDisjoint: a large ranked move (TIER
// 250 -> 2500) has non-overlapping CIs and is flagged significant.
func TestScoresCompare_SignificanceFiresWhenCIsDisjoint(t *testing.T) {
	h, db := newTestHandler(t)
	// Window A: 5 outcomes weight 0.5 -> points 2.5 over $10 -> TIER 250.
	seedRankedDeveloper(t, db, "alice", "wa", 5, 0.5, 2.0, winAInstant)
	// Window B: 5 outcomes weight 5 -> points 25 over $10 -> TIER 2500.
	seedRankedDeveloper(t, db, "alice", "wb", 5, 5.0, 2.0, winBInstant)

	code, resp := getCompare(t, h, compareURL())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	alice := devDelta(t, resp, "alice")
	if !alice.A.Ranked || !alice.B.Ranked {
		t.Fatalf("alice ranked a/b = %v/%v, want true/true", alice.A.Ranked, alice.B.Ranked)
	}
	if !alice.Significant {
		t.Fatalf("expected significant delta for disjoint CIs (a=[%v,%v] b=[%v,%v])",
			alice.A.CILow, alice.A.CIHigh, alice.B.CILow, alice.B.CIHigh)
	}
}

// TestScoresCompare_SignificanceSilentWhenCIsOverlap: identical windows -> equal
// CIs overlap -> not significant, delta zero.
func TestScoresCompare_SignificanceSilentWhenCIsOverlap(t *testing.T) {
	h, db := newTestHandler(t)
	seedRankedDeveloper(t, db, "alice", "wa", 5, 1.0, 2.0, winAInstant)
	seedRankedDeveloper(t, db, "alice", "wb", 5, 1.0, 2.0, winBInstant)

	code, resp := getCompare(t, h, compareURL())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	alice := devDelta(t, resp, "alice")
	if !alice.A.Ranked || !alice.B.Ranked {
		t.Fatalf("alice ranked a/b = %v/%v, want true/true", alice.A.Ranked, alice.B.Ranked)
	}
	if alice.DeltaTIER != 0 {
		t.Fatalf("alice delta_tier = %v, want 0 (identical windows)", alice.DeltaTIER)
	}
	if alice.Significant {
		t.Fatalf("overlapping CIs must not be significant (a=[%v,%v] b=[%v,%v])",
			alice.A.CILow, alice.A.CIHigh, alice.B.CILow, alice.B.CIHigh)
	}
}

// TestScoresCompare_BelowFloorNeverSignificant: window A is below the ranking floor
// (2 outcomes < MinRankedOutcomes). Even though window B is ranked and the TIER
// moves hugely, an unranked window can never be significant (#133 / issue Q2).
func TestScoresCompare_BelowFloorNeverSignificant(t *testing.T) {
	h, db := newTestHandler(t)
	// Window A: only 2 outcomes -> unranked.
	seedRankedDeveloper(t, db, "alice", "wa", 2, 1.0, 2.0, winAInstant)
	// Window B: 5 outcomes weight 5 -> ranked, very different TIER.
	seedRankedDeveloper(t, db, "alice", "wb", 5, 5.0, 2.0, winBInstant)

	code, resp := getCompare(t, h, compareURL())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	alice := devDelta(t, resp, "alice")
	if alice.A.Ranked {
		t.Fatalf("window A should be below floor (unranked), got ranked")
	}
	if !alice.B.Ranked {
		t.Fatalf("window B should be ranked")
	}
	if alice.Significant {
		t.Fatal("an unranked window must render the delta not-significant")
	}
}

func teamDelta(resp compareResponse, team string) (teamDeltaJSON, bool) {
	for _, tr := range resp.Teams {
		if tr.Team == team {
			return tr, true
		}
	}
	return teamDeltaJSON{}, false
}

// TestScoresCompare_TeamKAnonIntersection is the security crux (#277 caveats 1+2):
// team "blue" clears k=3 in BOTH windows and is emitted named; team "red" clears k
// in window A but is sub-k in window B, so it must fold into "other" in BOTH windows
// and NEVER appear as a named row — its window-A aggregate cannot leak through the
// delta. No named developer is ever emitted.
func TestScoresCompare_TeamKAnonIntersection(t *testing.T) {
	h, db := newTestHandler(t)
	h.SetAggregation(scoring.AggregationTeam, 3)
	ctx := context.Background()

	assign := func(dev, team string) {
		if err := db.UpsertHierarchy(ctx, dev, team, "", ""); err != nil {
			t.Fatalf("UpsertHierarchy(%s,%s): %v", dev, team, err)
		}
	}
	// blue: 3 devs contributing in BOTH windows.
	for _, dev := range []string{"b1", "b2", "b3"} {
		assign(dev, "blue")
		seedOutcomeAt(t, db, dev, "blueA-"+dev, 1.0, 1.0, winAInstant)
		seedCostAt(t, db, dev, "blueA-"+dev, 4.0, winAInstant)
		seedOutcomeAt(t, db, dev, "blueB-"+dev, 1.0, 1.0, winBInstant)
		seedCostAt(t, db, dev, "blueB-"+dev, 4.0, winBInstant)
	}
	// red: 3 devs contribute in window A ($10 each -> $30 total), only r1 in window B.
	for _, dev := range []string{"r1", "r2", "r3"} {
		assign(dev, "red")
		seedOutcomeAt(t, db, dev, "redA-"+dev, 1.0, 1.0, winAInstant)
		seedCostAt(t, db, dev, "redA-"+dev, 10.0, winAInstant)
	}
	seedOutcomeAt(t, db, "r1", "redB-r1", 1.0, 1.0, winBInstant)
	seedCostAt(t, db, "r1", "redB-r1", 10.0, winBInstant)

	code, resp := getCompare(t, h, compareURL())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Mode != "team" {
		t.Fatalf("mode = %q, want team", resp.Mode)
	}
	// Team-aggregation guard: never a named per-developer delta.
	if len(resp.Developers) != 0 {
		t.Fatalf("team mode leaked %d named developer deltas, want 0", len(resp.Developers))
	}
	// red must NOT appear as a named team in either window — it is sub-k in B, so the
	// intersection folds it away. A per-window k-anon (naming red in window A, where
	// it has 3 contributors) would have leaked it; the two-window compare must not.
	if _, ok := teamDelta(resp, "red"); ok {
		t.Fatal("SECURITY: team 'red' is sub-k in window B and must never be a named row")
	}
	// blue clears k in both windows -> named, present on both sides.
	blue, ok := teamDelta(resp, "blue")
	if !ok {
		t.Fatal("team 'blue' should be a named row (clears k in both windows)")
	}
	if blue.A.TIER == 0 || blue.B.TIER == 0 {
		t.Fatalf("blue should have data on both sides, got a/b TIER %v/%v", blue.A.TIER, blue.B.TIER)
	}
	// Team aggregates carry no CI; significance is never asserted in team mode.
	if blue.Significant {
		t.Fatal("team-mode rows must never be flagged significant (no bootstrap CI)")
	}
	// red's data is preserved in the 'other' residual, not dropped: other.A carries
	// red's window-A cost ($30) and other.B carries r1's window-B cost ($10).
	other, ok := teamDelta(resp, scoring.OtherCohort)
	if !ok {
		t.Fatal("'other' residual row should be present (red folded into it)")
	}
	if other.A.TotalCostUSD != 30 {
		t.Fatalf("other.A cost = %v, want 30 (red's window-A spend folded in)", other.A.TotalCostUSD)
	}
	if other.B.TotalCostUSD != 10 {
		t.Fatalf("other.B cost = %v, want 10 (r1's window-B spend)", other.B.TotalCostUSD)
	}
}

// TestScoresCompare_DivisionKAnonIntersection is the division-level (#270) analogue
// of the team intersection proof (#277 caveats 1+2): division "blue" clears k=3 in
// BOTH windows and is emitted named; division "red" clears k in window A but is
// sub-k in window B (only r1 contributes there), so it must fold into "other" in
// BOTH windows and NEVER appear as a named row -- its window-A aggregate cannot leak
// through the delta. No named developer is ever emitted. Division mode is the SAME
// two-window k-anon fold as team mode; only the label map (DivisionsForDevelopers)
// differs, so this pins that the intersection holds one level up.
func TestScoresCompare_DivisionKAnonIntersection(t *testing.T) {
	h, db := newTestHandler(t)
	h.SetAggregation(scoring.AggregationDivision, 3)
	ctx := context.Background()

	assign := func(dev, division string) {
		// Team is irrelevant in division mode (the label resolves to the division);
		// a distinct team per dev keeps the hierarchy well-formed without affecting
		// the division rollup.
		if err := db.UpsertHierarchy(ctx, dev, "t-"+dev, division, "org"); err != nil {
			t.Fatalf("UpsertHierarchy(%s,%s): %v", dev, division, err)
		}
	}
	// blue: 3 devs contributing in BOTH windows.
	for _, dev := range []string{"b1", "b2", "b3"} {
		assign(dev, "blue")
		seedOutcomeAt(t, db, dev, "blueA-"+dev, 1.0, 1.0, winAInstant)
		seedCostAt(t, db, dev, "blueA-"+dev, 4.0, winAInstant)
		seedOutcomeAt(t, db, dev, "blueB-"+dev, 1.0, 1.0, winBInstant)
		seedCostAt(t, db, dev, "blueB-"+dev, 4.0, winBInstant)
	}
	// red: 3 devs contribute in window A ($10 each -> $30 total), only r1 in window B.
	for _, dev := range []string{"r1", "r2", "r3"} {
		assign(dev, "red")
		seedOutcomeAt(t, db, dev, "redA-"+dev, 1.0, 1.0, winAInstant)
		seedCostAt(t, db, dev, "redA-"+dev, 10.0, winAInstant)
	}
	seedOutcomeAt(t, db, "r1", "redB-r1", 1.0, 1.0, winBInstant)
	seedCostAt(t, db, "r1", "redB-r1", 10.0, winBInstant)

	code, resp := getCompare(t, h, compareURL())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Mode != "division" {
		t.Fatalf("mode = %q, want division", resp.Mode)
	}
	// Division-aggregation guard: never a named per-developer delta.
	if len(resp.Developers) != 0 {
		t.Fatalf("division mode leaked %d named developer deltas, want 0", len(resp.Developers))
	}
	// red must NOT appear as a named division in either window -- it is sub-k in B, so
	// the intersection folds it away. A per-window k-anon (naming red in window A,
	// where it has 3 contributors) would have leaked it; the two-window compare must not.
	if _, ok := teamDelta(resp, "red"); ok {
		t.Fatal("SECURITY: division 'red' is sub-k in window B and must never be a named row")
	}
	// blue clears k in both windows -> named, present on both sides.
	blue, ok := teamDelta(resp, "blue")
	if !ok {
		t.Fatal("division 'blue' should be a named row (clears k in both windows)")
	}
	if blue.A.TIER == 0 || blue.B.TIER == 0 {
		t.Fatalf("blue should have data on both sides, got a/b TIER %v/%v", blue.A.TIER, blue.B.TIER)
	}
	// Division aggregates carry no CI; significance is never asserted in an anonymized mode.
	if blue.Significant {
		t.Fatal("division-mode rows must never be flagged significant (no bootstrap CI)")
	}
	// red's data is preserved in the 'other' residual, not dropped: other.A carries
	// red's window-A cost ($30) and other.B carries r1's window-B cost ($10).
	other, ok := teamDelta(resp, scoring.OtherCohort)
	if !ok {
		t.Fatal("'other' residual row should be present (red folded into it)")
	}
	if other.A.TotalCostUSD != 30 {
		t.Fatalf("other.A cost = %v, want 30 (red's window-A spend folded in)", other.A.TotalCostUSD)
	}
	if other.B.TotalCostUSD != 10 {
		t.Fatalf("other.B cost = %v, want 10 (r1's window-B spend)", other.B.TotalCostUSD)
	}
}

// TestScoresCompare_PerWindowMixedPriceVersions: two DIFFERENT price-table versions
// (#233/#293) priced inside ONE window must raise that window's
// data_quality.mixed_price_versions signal and NOT the other window's (#277 caveat
// 3 -- each window carries its own data_quality). Window A spans v1+v2; window B is
// single-version, so only A carries the mixed-price WARN.
func TestScoresCompare_PerWindowMixedPriceVersions(t *testing.T) {
	h, db := newTestHandler(t)

	// Window A: two token_events priced under DIFFERENT versions (v1 and v2) -> mixed.
	seedPricedCostAt(t, db, "alice", "a-1", 4.0, 1, winAInstant)
	seedOutcomeAt(t, db, "alice", "a-1", 3.0, 1.0, winAInstant)
	seedPricedCostAt(t, db, "alice", "a-2", 4.0, 2, winAInstant)
	seedOutcomeAt(t, db, "alice", "a-2", 3.0, 1.0, winAInstant)

	// Window B: a single price version (v1) -> clean, no mixed-price signal.
	seedPricedCostAt(t, db, "alice", "b-1", 4.0, 1, winBInstant)
	seedOutcomeAt(t, db, "alice", "b-1", 3.0, 1.0, winBInstant)

	code, resp := getCompare(t, h, compareURL())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	// Window A carries the mixed-version signal (ascending distinct versions).
	if resp.WindowA.DataQuality == nil {
		t.Fatal("window A should carry a mixed_price_versions data_quality signal")
	}
	if want := []int{1, 2}; !sameInts(resp.WindowA.DataQuality.MixedPriceVersions, want) {
		t.Fatalf("window A mixed_price_versions = %v, want %v",
			resp.WindowA.DataQuality.MixedPriceVersions, want)
	}
	// Window B is single-version: the per-window signal must NOT populate on it.
	if resp.WindowB.DataQuality != nil && len(resp.WindowB.DataQuality.MixedPriceVersions) != 0 {
		t.Fatalf("window B should be single-version, got mixed_price_versions %v",
			resp.WindowB.DataQuality.MixedPriceVersions)
	}
}

// TestScoresCompare_PerWindowDataQuality: a mixed-version / zero-token signal in one
// window must attach to THAT window's data_quality and not the other (#277 caveat 3).
func TestScoresCompare_PerWindowDataQuality(t *testing.T) {
	h, db := newTestHandler(t)

	// Window A: a clean cost-bearing outcome (clears the tripwire).
	seedOutcomeAt(t, db, "alice", "a-1", 1.0, 1.0, winAInstant)
	seedCostAt(t, db, "alice", "a-1", 10.0, winAInstant)

	// Window B: an outcome whose issue has NO attributable tokens -> zero-token
	// flagged. No token_event seeded for this issue in window B.
	seedOutcomeAt(t, db, "alice", "b-zero", 1.0, 1.0, winBInstant)

	code, resp := getCompare(t, h, compareURL())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.WindowA.DataQuality != nil && len(resp.WindowA.DataQuality.ZeroTokenOutcomes) != 0 {
		t.Fatalf("window A should be clean, got zero-token outcomes: %+v",
			resp.WindowA.DataQuality.ZeroTokenOutcomes)
	}
	if resp.WindowB.DataQuality == nil || len(resp.WindowB.DataQuality.ZeroTokenOutcomes) == 0 {
		t.Fatal("window B should carry a zero-token data_quality signal")
	}
	if got := resp.WindowB.DataQuality.ZeroTokenOutcomes[0].IssueID; got != "b-zero" {
		t.Fatalf("window B zero-token issue = %q, want b-zero", got)
	}
}

// TestScoresCompare_RequiresReadScope: the compare endpoint is READ-scoped — a
// read-only token is accepted, and no token at all (when scopes are configured) is
// a 401.
func TestScoresCompare_RequiresReadScope(t *testing.T) {
	h, db := newTestHandlerWithScopes(t, "write-token-aaaaaaaaaaaaaaaaaaaa", "read-token-bbbbbbbbbbbbbbbbbbbb")
	seedOutcomeAt(t, db, "alice", "a-1", 1.0, 1.0, winAInstant)
	seedCostAt(t, db, "alice", "a-1", 10.0, winAInstant)

	header := http.Header{}
	header.Set("Authorization", "Bearer read-token-bbbbbbbbbbbbbbbbbbbb")
	code, _ := doRequestWithHeader(t, h, "GET", compareURL(), nil, header)
	if code != http.StatusOK {
		t.Fatalf("read token status = %d, want 200", code)
	}

	// No credential at all -> 401 (a scope is required when tokens are configured).
	code, _ = doRequest(t, h, "GET", compareURL(), nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", code)
	}
}

// TestScoresCompare_RejectsInvertedWindow: until <= since is a fail-loud 400, per window.
func TestScoresCompare_RejectsInvertedWindow(t *testing.T) {
	h, _ := newTestHandler(t)
	code, _ := doRequest(t, h, "GET",
		"/api/v1/scores/compare?since_a=2026-02-01&until_a=2026-01-01&"+compareWindowB, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("inverted window A status = %d, want 400", code)
	}
}

// TestScoresCompare_RejectsInvertedWindowB: window B validation must fire
// independently of window A (the two legs are parsed by the same helper with
// different key names — a copy-paste that validated A twice would pass otherwise).
func TestScoresCompare_RejectsInvertedWindowB(t *testing.T) {
	h, _ := newTestHandler(t)
	code, _ := doRequest(t, h, "GET",
		"/api/v1/scores/compare?"+compareWindowA+"&since_b=2026-05-01&until_b=2026-04-01", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("inverted window B status = %d, want 400", code)
	}
}

// TestScoresCompare_RejectsMalformedDate: a malformed date in either leg is a 400.
func TestScoresCompare_RejectsMalformedDate(t *testing.T) {
	h, _ := newTestHandler(t)
	code, _ := doRequest(t, h, "GET",
		"/api/v1/scores/compare?since_a=not-a-date&"+compareWindowB, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("malformed since_a status = %d, want 400", code)
	}
	code, _ = doRequest(t, h, "GET",
		"/api/v1/scores/compare?"+compareWindowA+"&until_b=2026-13-99", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("malformed until_b status = %d, want 400", code)
	}
}

// TestScoresCompare_RetentionFailsLoud: a window whose lower bound predates the
// retention horizon fails loud with 422 (#252), enforced for BOTH legs.
func TestScoresCompare_RetentionFailsLoud(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetRetentionHorizon(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	// Window A starts 2026-01-01, before the horizon -> 422.
	code, _ := doRequest(t, h, "GET", compareURL(), nil)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("pre-retention window A status = %d, want 422", code)
	}
	// Only window B predates the horizon: A after horizon, B before.
	code, _ = doRequest(t, h, "GET",
		"/api/v1/scores/compare?since_a=2026-04-01&until_a=2026-05-01&since_b=2026-01-01&until_b=2026-02-01", nil)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("pre-retention window B status = %d, want 422", code)
	}
}

// TestScoresCompare_AliasPairsAcrossWindows: the same human recorded under an OS
// username in window A and their canonical login in window B must collapse to ONE
// delta row present in both windows — not two phantom one-window rows (#125).
func TestScoresCompare_AliasPairsAcrossWindows(t *testing.T) {
	h, db := newTestHandler(t)
	if err := db.UpsertDeveloperAlias(context.Background(), "alice-os", "alice"); err != nil {
		t.Fatalf("UpsertDeveloperAlias: %v", err)
	}
	// Window A recorded under the OS username; window B under the canonical login.
	seedOutcomeAt(t, db, "alice-os", "a-1", 3.0, 1.0, winAInstant)
	seedCostAt(t, db, "alice-os", "a-1", 10.0, winAInstant)
	seedOutcomeAt(t, db, "alice", "b-1", 6.0, 1.0, winBInstant)
	seedCostAt(t, db, "alice", "b-1", 10.0, winBInstant)

	code, resp := getCompare(t, h, compareURL())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	// Exactly one row, keyed by the canonical identity, present in both windows.
	if len(resp.Developers) != 1 {
		names := make([]string, 0, len(resp.Developers))
		for _, d := range resp.Developers {
			names = append(names, d.Developer)
		}
		t.Fatalf("got %d developer rows %v, want 1 (alias must collapse the two windows)", len(resp.Developers), names)
	}
	alice := devDelta(t, resp, "alice")
	if !alice.PresentA || !alice.PresentB {
		t.Fatalf("aliased developer present_a/b = %v/%v, want true/true", alice.PresentA, alice.PresentB)
	}
	if alice.DeltaTIER != 300 { // 600 - 300
		t.Fatalf("aliased delta_tier = %v, want 300", alice.DeltaTIER)
	}
}

// TestScoresCompare_PresentInWindowBOnly: the symmetric case of the bob (A-only)
// coverage — a developer with data only in window B has present_a=false,
// present_b=true, zero delta, not significant.
func TestScoresCompare_PresentInWindowBOnly(t *testing.T) {
	h, db := newTestHandler(t)
	seedOutcomeAt(t, db, "carol", "b-1", 2.0, 1.0, winBInstant)
	seedCostAt(t, db, "carol", "b-1", 10.0, winBInstant)

	code, resp := getCompare(t, h, compareURL())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	carol := devDelta(t, resp, "carol")
	if carol.PresentA || !carol.PresentB {
		t.Fatalf("carol present_a/b = %v/%v, want false/true", carol.PresentA, carol.PresentB)
	}
	if carol.DeltaTIER != 0 || carol.Significant {
		t.Fatalf("carol delta/significant = %v/%v, want 0/false", carol.DeltaTIER, carol.Significant)
	}
}

// TestScoresCompare_TeamModeDataQualityNameSuppressed: in team mode a per-window
// data_quality must carry only the name-free zero-token COUNT, never the named
// per-(developer, issue) list (#185 k-anon boundary on the compare surface).
func TestScoresCompare_TeamModeDataQualityNameSuppressed(t *testing.T) {
	h, db := newTestHandler(t)
	h.SetAggregation(scoring.AggregationTeam, 3)
	// Window B: an outcome with no attributable tokens -> zero-token flagged.
	seedOutcomeAt(t, db, "dave", "b-zero", 1.0, 1.0, winBInstant)

	code, resp := getCompare(t, h, compareURL())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	dq := resp.WindowB.DataQuality
	if dq == nil {
		t.Fatal("window B should carry a zero-token data_quality signal")
	}
	if dq.ZeroTokenOutcomeCount != 1 {
		t.Fatalf("zero_token_outcome_count = %d, want 1", dq.ZeroTokenOutcomeCount)
	}
	if len(dq.ZeroTokenOutcomes) != 0 {
		t.Fatalf("team mode leaked %d named zero-token outcomes, want 0", len(dq.ZeroTokenOutcomes))
	}
}

// TestScoresCompare_EmptyWindows pins the whole-window-empty behavior: an empty
// database yields 200, a non-nil empty developers array, and a nil total. This also
// pins that omitting all window params is a valid (defaulted) request, not a 400.
func TestScoresCompare_EmptyWindows(t *testing.T) {
	h, _ := newTestHandler(t)
	code, resp := getCompare(t, h, "/api/v1/scores/compare")
	if code != http.StatusOK {
		t.Fatalf("no-param empty-db status = %d, want 200", code)
	}
	if resp.Mode != "developer" {
		t.Fatalf("mode = %q, want developer", resp.Mode)
	}
	if resp.Developers == nil {
		t.Fatal("developers must be a non-nil empty array, not null")
	}
	if len(resp.Developers) != 0 {
		t.Fatalf("developers = %d rows, want 0 on an empty db", len(resp.Developers))
	}
	if resp.Total != nil {
		t.Fatalf("total = %+v, want nil when both windows are empty", resp.Total)
	}
}
