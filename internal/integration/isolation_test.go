//go:build integration

// Individual-score isolation regression (#280). Pins the invariant that an
// individual developer's TIER row (weighted_points, total_cost, TIER, sample_n,
// CI, ranked, flags) is a PURE FUNCTION of that developer's OWN rows: no other
// developer's costs, outcomes, org membership, or unattributed spend may move
// those fields. ComputeDeveloper (scoring/engine.go) has no cross-developer
// term, costs are GROUP BY developer, and the bootstrap CI reseeds per developer
// from a fixed constant, so a developer's CI is identical regardless of who else
// is in the window.
//
// It then pins the TWO DESIGNED EXCEPTIONS so a future refactor cannot "fix" them
// back into a silent regression:
//
//  1. SpendLeverage IS headcount-coupled by design (#23/#40/#41). ActualPaidUSD
//     comes from the org-invoice seat allocation (store.ActualSpendForDeveloper /
//     ActualSpendAll), which splits an org's invoice remainder across its ACTIVE
//     seats. When a second developer joins the org's period_membership, an
//     individual's allocated actual_paid drops and their SpendLeverage shifts —
//     while every core field above stays byte-identical. Test asserts core fields
//     immune AND the leverage shift matches the allocation formula exactly.
//
//  2. The zero-token ranked-flag window is keyed (repo, issue) and anchored to the
//     FRESHEST merge across ALL developers (store.OutcomeTokenTotals design note).
//     If developer B merges a PR that REUSES developer A's issue id at a later
//     timestamp, A's attributable window slides forward off A's tokens, A's
//     (developer, issue) token total falls below MinAttributableTokens, and A's
//     outcome is zero-token flagged — unranking A while never touching A's points.
//     This is the deliberate anti-laundering fail-safe (adversarial G-02); pinned
//     so a "fix" that unions the windows (and reopens the laundering vector) fails
//     loudly here.
package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// isoCoreFields is the subset of a developer's score that #280 pins as a pure
// function of that developer's OWN rows. SpendLeverage is deliberately excluded —
// it is designed exception 1. costMicro is read at the store layer (integer
// micro-dollars) so money is compared exactly, never as a float.
type isoCoreFields struct {
	weightedPoints  float64
	tier            float64
	sampleN         int
	ciLow           float64
	ciHigh          float64
	ranked          bool
	flaggedOutcomes int
	costMicro       int64
}

// isoCore snapshots the core (own-rows-only) fields for developer dev: the wire
// row from /scores plus the store-layer cost. It is captured before and after a
// second developer's data lands in the same DB and compared for byte-identity.
func isoCore(t *testing.T, sr scoresResponse, db *store.DB, dev string) isoCoreFields {
	t.Helper()
	ds := devByName(t, sr, dev)
	micro, ok := developerCostMicro(t, db, dev)
	if !ok {
		t.Fatalf("%s: no cost row persisted", dev)
	}
	return isoCoreFields{
		weightedPoints:  ds.WeightedPoints,
		tier:            ds.TIER,
		sampleN:         ds.SampleN,
		ciLow:           ds.CILow,
		ciHigh:          ds.CIHigh,
		ranked:          ds.Ranked,
		flaggedOutcomes: ds.FlaggedOutcomes,
		costMicro:       micro,
	}
}

// isoUnattributedCapture drives ONE proxy call with NO X-Tier-Developer header, so
// the proxy pools its cost under the "unattributed" pseudo-developer (proxy.go)
// rather than onto any real developer's rows. It mirrors e2eCapture minus the
// attribution headers — the point is to prove such an event never touches A.
func isoUnattributedCapture(t *testing.T, proxyBase, model string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyBase+"/v1/messages", http.NoBody)
	if err != nil {
		t.Fatalf("new unattributed request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Deliberately NO X-Tier-Developer and NO X-Tier-Issue: both fall back to the
	// "unattributed" sentinel.
	req.Header.Set("X-Stub-Model", model)
	req.Header.Set("X-Stub-Input", strconv.Itoa(e2eCallInput))
	req.Header.Set("X-Stub-Output", strconv.Itoa(e2eCallOutput))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unattributed capture: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unattributed capture: status %d, want 200", resp.StatusCode)
	}
}

// TestIsolation_IndividualScoreIsPureFunctionOfOwnRows pins the #280 invariant and
// designed exception 1 (SpendLeverage seat allocation) in one shared-DB scenario:
// compute alice alone, then add bob's captures/outcomes + org membership + one
// unattributed proxy event, and assert alice's core fields are byte-identical
// while her SpendLeverage shifts by exactly the seat-allocation formula.
func TestIsolation_IndividualScoreIsPureFunctionOfOwnRows(t *testing.T) {
	const (
		isoOrg     = "iso-org"
		isoInvoice = 100.0 // org actual_spend, allocated across active seats
	)
	period := time.Now().UTC().Format("2006-01")
	since := time.Now().UTC().AddDate(0, -1, 0)

	db, err := store.Open(filepath.Join(t.TempDir(), "iso-invariant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	anthStub := newE2EAnthropicStub(t)
	oaStub := newE2EOpenAIStub(t)
	srv := e2eDevServer(t, db, anthStub.URL, oaStub.URL)

	// importSeat imports one developer into isoOrg over POST /org_hierarchy, which
	// opens a period_membership seat (the unit the invoice remainder splits across).
	importSeat := func(dev string) {
		batch := []map[string]string{{"developer": dev, "team": "iso-team", "org": isoOrg}}
		if code, body := postHierarchyJSON(t, http.MethodPost, srv.URL+"/api/v1/org_hierarchy", batch); code != http.StatusCreated {
			t.Fatalf("org_hierarchy import %s: status %d, body %s", dev, code, body)
		}
	}

	var sha int
	nextSHA := func() string { sha++; return fmt.Sprintf("%040d", sha) }

	// seedDev drives a developer's OWN rows over the real HTTP surface: three
	// size/M anthropic PRs (points 9.0, cost 3x $6 = $18, every issue captured with
	// 1.2M tokens so nothing zero-token flags). Captures precede merges so each
	// token event lands inside its outcome's attributable window.
	seedDev := func(dev string, baseIssue int) {
		for i := 0; i < 3; i++ {
			e2eCapture(t, srv.URL+"/anthropic", dev, baseIssue+i, e2eAnthModel)
		}
		for i := 0; i < 3; i++ {
			issue := baseIssue + i
			e2eMergePR(t, srv.URL, dev, issue, "size/M", nextSHA(), fmt.Sprintf("iso-%s-%d", dev, issue))
		}
	}

	// --- Phase 1: alice ALONE, with an org invoice she is the sole seat for -----
	seedDev("alice", 1101)
	importSeat("alice")
	seedOrgSpend(t, db, isoOrg, period, isoInvoice)

	before := getScores(t, srv.URL)
	aBefore := isoCore(t, before, db, "alice")
	if !aBefore.ranked || aBefore.flaggedOutcomes != 0 || aBefore.sampleN != 3 {
		t.Fatalf("alice-alone precondition: ranked=%v flagged=%d sampleN=%d, want true/0/3",
			aBefore.ranked, aBefore.flaggedOutcomes, aBefore.sampleN)
	}
	aliceBeforeRow := devByName(t, before, "alice")
	leverageBefore := aliceBeforeRow.SpendLeverage
	allocBefore := devAlloc(t, db, "alice", since)
	// As the sole active seat, alice is allocated the whole invoice remainder.
	if !approxEq(allocBefore, isoInvoice) {
		t.Fatalf("alice-alone allocation = %v, want %v (sole seat gets the whole invoice)", allocBefore, isoInvoice)
	}
	if leverageBefore == 0 || !approxEq(leverageBefore, aliceBeforeRow.TotalCostUSD/isoInvoice) {
		t.Fatalf("alice-alone spend_leverage = %v, want %v (cost/invoice)", leverageBefore, aliceBeforeRow.TotalCostUSD/isoInvoice)
	}

	// --- Phase 2: add bob's OWN rows + org membership + one unattributed event --
	seedDev("bob", 1201)
	importSeat("bob")
	isoUnattributedCapture(t, srv.URL+"/anthropic", e2eAnthModel)

	after := getScores(t, srv.URL)
	aAfter := isoCore(t, after, db, "alice")

	// INVARIANT: every core field is BYTE-IDENTICAL. bob's costs/outcomes/seat and
	// the unattributed event moved none of alice's own-rows-derived numbers.
	if aAfter != aBefore {
		t.Errorf("alice core fields changed when bob's data was added:\n before = %+v\n after  = %+v", aBefore, aAfter)
	}
	// Field-level messages so a regression names the exact coupled field.
	if aAfter.weightedPoints != aBefore.weightedPoints {
		t.Errorf("weighted_points moved: %v -> %v", aBefore.weightedPoints, aAfter.weightedPoints)
	}
	if aAfter.tier != aBefore.tier {
		t.Errorf("TIER moved: %v -> %v", aBefore.tier, aAfter.tier)
	}
	if aAfter.costMicro != aBefore.costMicro {
		t.Errorf("cost moved: %d -> %d micro", aBefore.costMicro, aAfter.costMicro)
	}
	if aAfter.ciLow != aBefore.ciLow || aAfter.ciHigh != aBefore.ciHigh {
		t.Errorf("CI moved: [%v,%v] -> [%v,%v]", aBefore.ciLow, aBefore.ciHigh, aAfter.ciLow, aAfter.ciHigh)
	}
	if aAfter.ranked != aBefore.ranked || aAfter.flaggedOutcomes != aBefore.flaggedOutcomes {
		t.Errorf("ranked/flags moved: ranked %v->%v flagged %d->%d",
			aBefore.ranked, aAfter.ranked, aBefore.flaggedOutcomes, aAfter.flaggedOutcomes)
	}

	// The unattributed event pooled under its own pseudo-developer, never onto alice.
	if _, ok := findDevOpt(after, collector.UnattributedIssueID); !ok {
		t.Errorf("unattributed proxy event did not surface as the %q pseudo-developer", collector.UnattributedIssueID)
	}

	// EXCEPTION 1: SpendLeverage IS headcount-coupled. bob is now a second active,
	// non-tier-1 seat, so the invoice remainder splits two ways: alice's allocation
	// halves and her leverage doubles. Pin the shift to the allocation formula
	// exactly, so nobody "isolates" leverage into a regression.
	aliceAfterRow := devByName(t, after, "alice")
	leverageAfter := aliceAfterRow.SpendLeverage
	allocAfter := devAlloc(t, db, "alice", since)
	wantAlloc := isoInvoice / 2.0 // (invoice - 0 tier-1) / 2 active seats
	if !approxEq(allocAfter, wantAlloc) {
		t.Errorf("alice allocation after bob joins = %v, want %v (invoice / 2 seats)", allocAfter, wantAlloc)
	}
	if leverageAfter == leverageBefore {
		t.Errorf("spend_leverage did NOT shift when bob joined (%v) — the headcount coupling regressed", leverageAfter)
	}
	if wantLev := aliceAfterRow.TotalCostUSD / wantAlloc; !approxEq(leverageAfter, wantLev) {
		t.Errorf("alice spend_leverage after = %v, want %v (cost / (invoice/2))", leverageAfter, wantLev)
	}
	if !approxEq(leverageAfter, leverageBefore*2.0) {
		t.Errorf("alice spend_leverage %v -> %v, want an exact doubling (one seat -> two)", leverageBefore, leverageAfter)
	}
}

// TestIsolation_SharedIssueIDUnranksViaFreshestWindow pins designed exception 2:
// a later developer REUSING an individual's issue id slides that issue's
// attributable window forward (freshest merge across developers), zero-token
// flags the individual's outcome, and unranks them — WITHOUT altering their
// points, cost, or TIER. It uses direct store inserts because the effect turns on
// precise merge timestamps 30 days apart, which the webhook (now-stamped) cannot
// express.
func TestIsolation_SharedIssueIDUnranksViaFreshestWindow(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	t1 := now.AddDate(0, 0, -30) // alice's merges + tokens, 30 days ago
	shared := "iso-shared"       // the issue id bob will reuse

	db, err := store.Open(filepath.Join(t.TempDir(), "iso-window.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	anthStub := newE2EAnthropicStub(t)
	oaStub := newE2EOpenAIStub(t)
	srv := e2eDevServer(t, db, anthStub.URL, oaStub.URL)

	// seedOwn writes one developer's (issue) as a token event inside the merge
	// window plus the merged outcome at mergeTS. 2000 tokens clears the 1000-token
	// tripwire; $10 clears the $5 ranking floor when three issues sum to $30.
	seedOwn := func(dev, issue string, mergeTS time.Time) {
		if err := db.InsertTokenEvent(ctx, store.TokenEvent{
			Developer: dev, IssueID: issue, Model: "claude-sonnet-4",
			InputTok: 2000, CostMicro: store.DollarsToMicro(10),
			Source: "jsonl", Fidelity: "realtime",
			Timestamp: mergeTS.Add(-time.Hour), // spent just before merge, inside window
		}); err != nil {
			t.Fatalf("InsertTokenEvent(%s,%s): %v", dev, issue, err)
		}
		if _, err := db.InsertOutcome(ctx, store.Outcome{
			Developer: dev, IssueID: issue, Weight: 3, Quality: 1, Timestamp: mergeTS,
		}); err != nil {
			t.Fatalf("InsertOutcome(%s,%s): %v", dev, issue, err)
		}
	}

	// --- alice ALONE: 3 outcomes, all funded, all ranked -----------------------
	seedOwn("alice", shared, t1)
	seedOwn("alice", "iso-y", t1)
	seedOwn("alice", "iso-z", t1)

	before := getScores(t, srv.URL)
	aBefore := devByName(t, before, "alice")
	if !aBefore.Ranked || aBefore.FlaggedOutcomes != 0 || aBefore.SampleN != 3 {
		t.Fatalf("alice-alone precondition: ranked=%v flagged=%d sampleN=%d, want true/0/3",
			aBefore.Ranked, aBefore.FlaggedOutcomes, aBefore.SampleN)
	}
	beforeMicro, _ := developerCostMicro(t, db, "alice")

	// --- bob REUSES alice's issue id, merged 30 days later ---------------------
	// The (repo, issue) window for `shared` now anchors on bob's fresher merge
	// [now-14d, now]; alice's tokens (30 days ago) fall outside it.
	seedOwn("bob", shared, now)

	after := getScores(t, srv.URL)
	aAfter := devByName(t, after, "alice")
	afterMicro, _ := developerCostMicro(t, db, "alice")

	// EXCEPTION 2: the ranked flag flips and exactly one outcome is now flagged.
	if aAfter.Ranked {
		t.Errorf("alice still ranked after bob reused issue %q — the freshest-window fail-safe regressed", shared)
	}
	if aAfter.FlaggedOutcomes != 1 {
		t.Errorf("alice flagged_outcomes = %d, want 1 (the reused-id outcome slid out of its window)", aAfter.FlaggedOutcomes)
	}

	// ...but ONLY the ranking authority moves. Points, cost, TIER, and sample_n are
	// still a pure function of alice's own rows.
	if aAfter.WeightedPoints != aBefore.WeightedPoints {
		t.Errorf("weighted_points moved on unrank: %v -> %v (a flagged outcome must keep its full points)",
			aBefore.WeightedPoints, aAfter.WeightedPoints)
	}
	if aAfter.TIER != aBefore.TIER {
		t.Errorf("TIER moved on unrank: %v -> %v", aBefore.TIER, aAfter.TIER)
	}
	if afterMicro != beforeMicro {
		t.Errorf("cost moved on unrank: %d -> %d micro", beforeMicro, afterMicro)
	}
	if aAfter.SampleN != aBefore.SampleN {
		t.Errorf("sample_n moved on unrank: %d -> %d", aBefore.SampleN, aAfter.SampleN)
	}

	// The data-quality panel names alice's flagged (developer, issue) so an operator
	// can investigate the off-books/laundering signal.
	if after.DataQuality == nil {
		t.Fatal("expected a data_quality block naming the zero-token outcome")
	}
	var found bool
	for _, z := range after.DataQuality.ZeroTokenOutcomes {
		if z.Developer == "alice" && z.IssueID == shared {
			found = true
		}
	}
	if !found {
		t.Errorf("data_quality did not name alice's flagged issue %q; got %+v", shared, after.DataQuality.ZeroTokenOutcomes)
	}
}
