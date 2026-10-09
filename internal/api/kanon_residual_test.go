package api

// Tests for #593: the k-anonymity residual floor and the aggregates that must be
// withheld with it.
//
// 🔴 THE POINT OF THIS FILE. Before #593 the residual "other" bucket was emitted
// whenever it was non-empty, with NO floor — so an anonymized deployment published a
// sub-k cohort's exact figures under a label that reads as anonymized.
//
// The subtle half, and the reason a row-only fix would have been a false green:
// removing the row does NOT remove the disclosure. `total` is an unfloored rollup of
// everyone and `cost_composition` an unfloored window sum, so
//
//	total - (named rows) == the suppressed cohort, exactly
//
// Every test here therefore asserts on BOTH the row and the derivable aggregates.
// the maintainer ruled this shape (option A, 2026-08-03) over complementary suppression and
// over merging the residual into a named group.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// seedKAnonDev enrols a developer in a team and gives them one cost-bearing outcome.
func seedKAnonDev(t *testing.T, db *store.DB, team, dev string, costUSD, weight float64, ts time.Time) {
	t.Helper()
	if err := baselineHierarchy(db, context.Background(), dev, team, "div-"+team, "acme"); err != nil {
		t.Fatalf("UpsertHierarchy(%s): %v", dev, err)
	}
	seedRepoCostAt(t, db, repoAlpha, dev, "i-"+dev, costUSD, ts)
	seedRepoOutcomeAt(t, db, repoAlpha, dev, "i-"+dev, weight, ts)
}

// TestKAnonResidual_TimeAxisReproduction is the reproduction the issue was filed on,
// and the one that proves #590's repo guard was NOT the fix.
//
// No ?repo= is involved. Narrowing the WINDOW alone until one developer was active
// collapses every cohort, and before #593 the response published that developer's
// figures three times over: as the "other" row, as `total`, and again inside
// `cost_composition`.
func TestKAnonResidual_TimeAxisReproduction(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -60)

	// Five contributors long ago — a healthy cohort in a wide window.
	for _, d := range []string{"a1", "a2", "a3", "a4", "a5"} {
		seedKAnonDev(t, db, "eng", d, 10, 2, old)
	}
	// One developer active recently, with a distinctive figure.
	seedKAnonDev(t, db, "eng", "solo", 7.77, 1, now)
	h.SetAggregation(scoring.AggregationTeam, 5)

	// WIDE window: 6 contributors, nothing suppressed — the control arm.
	wide := getScores(t, h, "/api/v1/scores?since="+old.AddDate(0, 0, -1).Format("2006-01-02"))
	if wide.Total == nil {
		t.Fatal("wide window: total missing, so the narrow-window assertion below proves nothing")
	}
	if wide.DataQuality != nil && wide.DataQuality.KAnonSuppressed != nil {
		t.Errorf("wide window must not suppress: %+v", wide.DataQuality.KAnonSuppressed)
	}

	// NARROW window: only `solo` is active. Everything must be withheld.
	narrow := getScores(t, h, "/api/v1/scores?since="+now.AddDate(0, 0, -1).Format("2006-01-02"))
	if len(narrow.Teams) != 0 {
		t.Errorf("narrow window: expected no cohort rows, got %+v", narrow.Teams)
	}
	if narrow.Total != nil {
		t.Errorf("narrow window: `total` IS the lone developer and must be withheld; got %+v", narrow.Total)
	}
	if narrow.CostComposition != nil {
		t.Errorf("narrow window: cost_composition restates the same figure and must be withheld; got %+v",
			narrow.CostComposition)
	}
	if narrow.DataQuality == nil || narrow.DataQuality.KAnonSuppressed == nil {
		t.Fatal("narrow window: the suppression must be DECLARED — otherwise it is indistinguishable " +
			"from an empty window, and those demand opposite reactions")
	}
	ks := narrow.DataQuality.KAnonSuppressed
	if ks.Developers != 1 {
		t.Errorf("kanon_suppressed.developers = %d, want 1", ks.Developers)
	}
	if !ks.WithheldTotal || !ks.WithheldTeams || ks.WithheldCostComposition {
		t.Errorf("kanon_suppressed must state what was withheld, and no block team mode "+
			"never builds (#864); got %+v", ks)
	}
}

// TestKAnonResidual_NoDifferencingChannel is the assertion a row-only fix fails.
// With one named team and a suppressed residual, `total` minus the named row would
// hand back the hidden cohort. The only safe answer is that `total` is not there,
// and since #864 the named row goes too: the whole response is withheld.
func TestKAnonResidual_NoDifferencingChannel(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	for _, d := range []string{"b1", "b2", "b3", "b4", "b5", "b6"} {
		seedKAnonDev(t, db, "big", d, 10, 2, now)
	}
	// Two developers in a sub-k team: the residual, and the target.
	seedKAnonDev(t, db, "small", "s1", 3, 1, now)
	seedKAnonDev(t, db, "small", "s2", 3, 1, now)
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp := getScores(t, h, "/api/v1/scores?since="+scoresSince())
	if len(resp.Teams) != 0 {
		t.Errorf("sub-k residual withholds the whole response, the k-clearing team included; got %v", teamJSONNames(resp.Teams))
	}
	if resp.DataQuality == nil || resp.DataQuality.KAnonSuppressed == nil || resp.DataQuality.KAnonSuppressed.Developers != 2 {
		t.Fatalf("the withhold of small's 2 people must be declared; got %+v", resp.DataQuality)
	}
	if resp.Total != nil {
		t.Fatalf("`total` present alongside a suppressed residual: total(%v) - big = the hidden "+
			"cohort. Row suppression without total suppression MOVES the disclosure, it does not "+
			"close it", resp.Total.TotalCostUSD)
	}
}

// TestKAnonResidual_ClearingFloorStillEmitted is the control arm for the whole file.
// An implementation that withheld the residual unconditionally — or withheld `total`
// unconditionally in anonymized mode — would pass every assertion above while
// destroying the normal #185 fold. This is what makes the suppression tests mean
// something.
func TestKAnonResidual_ClearingFloorStillEmitted(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	for _, d := range []string{"b1", "b2", "b3", "b4", "b5"} {
		seedKAnonDev(t, db, "big", d, 10, 2, now)
	}
	// Three sub-k teams pooling into a residual of 5 >= k.
	seedKAnonDev(t, db, "t1", "x1", 3, 1, now)
	seedKAnonDev(t, db, "t1", "x2", 3, 1, now)
	seedKAnonDev(t, db, "t2", "y1", 3, 1, now)
	seedKAnonDev(t, db, "t2", "y2", 3, 1, now)
	seedKAnonDev(t, db, "t3", "z1", 3, 1, now)
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp := getScores(t, h, "/api/v1/scores?since="+scoresSince())
	if !teamPresent(resp.Teams, "other") {
		t.Errorf("a residual of 5 contributors hides its members and MUST be emitted; got %v",
			teamJSONNames(resp.Teams))
	}
	if !teamPresent(resp.Teams, "big") {
		t.Errorf("the k-clearing team must be named when the residual reaches k; got %v", teamJSONNames(resp.Teams))
	}
	if resp.Total == nil {
		t.Error("`total` must survive when nothing was suppressed")
	}
	if resp.DataQuality != nil && resp.DataQuality.KAnonSuppressed != nil {
		t.Errorf("nothing was suppressed; kanon_suppressed must be absent, got %+v",
			resp.DataQuality.KAnonSuppressed)
	}
	// And no sub-k team name leaks through the fold.
	for _, n := range []string{"t1", "t2", "t3"} {
		if teamPresent(resp.Teams, n) {
			t.Errorf("sub-k team %q must not be named; got %v", n, teamJSONNames(resp.Teams))
		}
	}
}

// TestKAnonResidual_DeveloperModeUnaffected: developer mode names everyone by design,
// so there is nothing to reconstruct and nothing to suppress. Without this arm, a
// change that withheld totals globally would look correct.
func TestKAnonResidual_DeveloperModeUnaffected(t *testing.T) {
	h, db := newTestHandler(t)
	seedKAnonDev(t, db, "solo", "loner", 7.77, 1, time.Now().UTC())
	// No SetAggregation: developer mode.

	resp := getScores(t, h, "/api/v1/scores?since="+scoresSince())
	if resp.Total == nil {
		t.Error("developer mode must keep `total` — it suppresses nothing")
	}
	if resp.CostComposition == nil {
		t.Error("developer mode must keep cost_composition")
	}
	if resp.DataQuality != nil && resp.DataQuality.KAnonSuppressed != nil {
		t.Errorf("developer mode must never report k-anon suppression; got %+v",
			resp.DataQuality.KAnonSuppressed)
	}
	if len(resp.Developers) == 0 {
		t.Error("developer mode must name the developer")
	}
}

// TestKAnonResidual_CompareEndpoint: /scores/compare shares the same shape and had the
// same unfloored residual. Fixing /scores alone would have left the identical cohort
// published one endpoint over — and a DELTA is arguably sharper than a level, since it
// says how one small group's efficiency moved between two periods.
func TestKAnonResidual_CompareEndpoint(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	a := now.AddDate(0, 0, -20)
	b := now.AddDate(0, 0, -5)
	for _, d := range []string{"b1", "b2", "b3", "b4", "b5", "b6"} {
		seedKAnonDev(t, db, "big", d, 10, 2, a)
		seedKAnonDev(t, db, "big", d, 10, 2, now)
	}
	seedKAnonDev(t, db, "small", "s1", 3, 1, a)
	seedKAnonDev(t, db, "small", "s1", 3, 1, now)
	h.SetAggregation(scoring.AggregationTeam, 5)

	q := "/api/v1/scores/compare?since_a=" + a.AddDate(0, 0, -1).Format("2006-01-02") +
		"&until_a=" + b.Format("2006-01-02") +
		"&since_b=" + b.Format("2006-01-02")
	code, body := doRequest(t, h, http.MethodGet, q, nil)
	if code != http.StatusOK {
		t.Fatalf("compare: status %d, body %s", code, body)
	}
	var resp compareResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal compare: %v", err)
	}
	for _, row := range resp.Teams {
		if row.Team == scoring.OtherCohort {
			t.Errorf("compare published a sub-k residual delta: %+v", row)
		}
	}
	if resp.Total != nil {
		t.Errorf("compare must withhold the grand-total delta alongside a suppressed residual — "+
			"total minus the named deltas reconstructs it; got %+v", resp.Total)
	}
}

// TestKAnonResidual_ReportsEffectiveKNotRequestedK pins that the declared floor is the
// one ACTUALLY ENFORCED.
//
// ⚠️ This gap was found by mutation: every other test in this file configures k=3,
// where the requested and effective floors coincide, so swapping the reported value
// for h.kAnonymity changed nothing and the mutation survived. AggregateTeamsKAnon
// clamps anything below scoring.MinKAnonymity (3) up to it, so a server configured
// with k=2 enforces 3.
//
// Publishing the requested 2 would tell an operator their 2-person cohort should have
// been fine and leave them unable to explain the suppression. The number reported must
// be the number that decided the outcome.
func TestKAnonResidual_ReportsEffectiveKNotRequestedK(t *testing.T) {
	if scoring.MinKAnonymity <= 2 {
		t.Skip("this test needs MinKAnonymity > 2 for requested and effective k to differ")
	}
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	// Two contributors: >= the REQUESTED k of 2, but below the EFFECTIVE floor of 3,
	// so they are suppressed and the discrepancy becomes observable.
	seedKAnonDev(t, db, "duo", "d1", 5, 1, now)
	seedKAnonDev(t, db, "duo", "d2", 5, 1, now)
	h.SetAggregation(scoring.AggregationTeam, 2) // requested 2, clamped to 3

	resp := getScores(t, h, "/api/v1/scores?since="+scoresSince())
	if resp.DataQuality == nil || resp.DataQuality.KAnonSuppressed == nil {
		t.Fatalf("expected suppression: 2 contributors is below the effective floor of %d",
			scoring.MinKAnonymity)
	}
	got := resp.DataQuality.KAnonSuppressed.KAnonymity
	if got == 2 {
		t.Errorf("kanon_suppressed.k_anonymity = 2 — that is the REQUESTED value, not the floor "+
			"in force. AggregateTeamsKAnon clamped it to %d, which is why these 2 developers "+
			"were suppressed; reporting 2 makes the suppression inexplicable",
			scoring.MinKAnonymity)
	}
	if got != scoring.MinKAnonymity {
		t.Errorf("kanon_suppressed.k_anonymity = %d, want the effective floor %d",
			got, scoring.MinKAnonymity)
	}
}

// padCohort enrols N extra contributing developers into `team` with the given
// work_type, so a fixture whose SUBJECT is something else (coverage shares,
// unattributed buckets, compare intersection) is not incidentally suppressed by #593.
//
// ⚠️ WHY THIS IS NEEDED AT ALL, and it is worth understanding before using it. A
// work_type SEGMENT is scored over only the developers who did that kind of work, so a
// segment is systematically smaller than the window. A window that comfortably clears
// k can still contain a segment that does not — and a suppressed segment escalates to
// the whole response, because weighted points partition exactly across work types
// (measured: pooled 13 points - visible feature segment 10 = the suppressed segment's
// 3, exactly).
//
// So "enough developers" for #593 means enough IN EVERY SEGMENT THE FIXTURE CREATES,
// not just enough overall. Padding with the same work_type as the fixture under test is
// the cheapest way to satisfy that without changing what the test measures.
func padCohort(t *testing.T, db *store.DB, team, workType string, n int, costUSD float64) {
	t.Helper()
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		dev := "pad-" + team + "-" + workType + "-" + string(rune('a'+i))
		if err := baselineHierarchy(db, context.Background(), dev, team, "div-"+team, "acme"); err != nil {
			t.Fatalf("UpsertHierarchy(%s): %v", dev, err)
		}
		seedRepoCostAt(t, db, repoAlpha, dev, "pi-"+dev, costUSD, now)
		if _, err := db.InsertOutcome(context.Background(), store.Outcome{
			Developer: dev, IssueID: "pi-" + dev, Repo: repoAlpha,
			Weight: 1, Quality: 1, WorkType: workType,
			WorkTypeSource: store.WorkTypeSourceLabel,
			MergeCommitSHA: "sha-pad-" + dev, Timestamp: now,
		}); err != nil {
			t.Fatalf("InsertOutcome(%s): %v", dev, err)
		}
	}
}

// enrolIn puts existing developers into a team, so a fixture's own developers join the
// padded cohort instead of falling into the unnamed residual.
//
// ⚠️ padCohort ALONE is not enough, and this is the trap. Padding creates a named team
// that clears the floor — but a fixture's own developers, if they are in no hierarchy
// at all, land in the UNNAMED group, which is the residual. Two of them is a sub-k
// residual and the response suppresses anyway. Pad the cohort AND enrol the subjects.
func enrolIn(t *testing.T, db *store.DB, team string, devs ...string) {
	t.Helper()
	for _, d := range devs {
		if err := baselineHierarchy(db, context.Background(), d, team, "div-"+team, "acme"); err != nil {
			t.Fatalf("UpsertHierarchy(%s): %v", d, err)
		}
	}
}

// padProportional enrols N developers each carrying the SAME attributed/unattributed
// split as the fixture's subject, so cost-ratio fields (exploratory_cost_share,
// attributed_cost_share) keep their expected values while the cohort clears the floor.
// Padding with a different mix silently moves the number under test.
func padProportional(t *testing.T, db *store.DB, team string, n int, attributedUSD, mainUSD float64) {
	t.Helper()
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		dev := "padp-" + team + "-" + string(rune('a'+i))
		if err := baselineHierarchy(db, context.Background(), dev, team, "div-"+team, "acme"); err != nil {
			t.Fatalf("UpsertHierarchy(%s): %v", dev, err)
		}
		seedRepoCostAt(t, db, repoAlpha, dev, "ppi-"+dev, attributedUSD, now)
		seedRepoCostAt(t, db, repoAlpha, dev, store.UnattributedMainBucket, mainUSD, now)
	}
}

// --- Guards that review MUTATION-PROVED were untested ---

// TestKAnonResidual_PooledRowsMinusSegmentsIsolateNobody is #864's evidence. Before
// #864 the pooled team `eng` published 13 points / $67.77 and the `feature` segment's
// `eng` row 10 points / $60, so pooled − Σsegments per team was the withheld security
// developer's exact 3 points / $7.77. An anonymised response now publishes one
// breakdown — the team rows and the total — so no published figure, alone or as a
// difference of two, is that developer's.
func TestKAnonResidual_PooledRowsMinusSegmentsIsolateNobody(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	for i, d := range []string{"f1", "f2", "f3", "f4", "f5"} {
		seedKAnonDevTyped(t, db, "eng", d, float64(10+i), 2, store.WorkTypeFeature, now)
	}
	seedKAnonDevTyped(t, db, "eng", "sec1", 7.77, 3, store.WorkTypeSecurity, now)
	h.SetAggregation(scoring.AggregationTeam, 5)

	raw, body := getScoresRaw(t, h, "?since="+scoresSince())
	resp := getScores(t, h, "/api/v1/scores?since="+scoresSince())
	if !teamPresent(resp.Teams, "eng") || resp.Total == nil {
		t.Fatalf("control: eng has 6 people and must publish with the total; body %s", body)
	}
	if _, ok := raw["work_types"]; ok {
		t.Errorf("work_types present in team mode: %s", body)
	}
	var nums []float64
	numbersIn(raw, &nums)
	for _, x := range nums {
		if math.Abs(x-7.77) < 1e-6 {
			t.Errorf("sec1's $7.77 is published: %s", body)
		}
		for _, y := range nums {
			if math.Abs(x-y-7.77) < 1e-6 {
				t.Errorf("%v − %v is sec1's $7.77: %s", x, y, body)
			}
		}
	}
}

// TestKAnonResidual_SegmentOtherRowsCannotReconstructASmallTeam is codex-auditor's
// worked example on #868 (finding ea7dfe25), k=5: team A has 8 developers split 4
// and 4 across two work types; team B has 1 developer working in both. Each segment
// used to publish a 5-person `other` row (4 of A's plus B), and summing both minus
// pooled A gave B's exact points. B alone is the residual now, below k, so the
// whole response is withheld and declared.
func TestKAnonResidual_SegmentOtherRowsCannotReconstructASmallTeam(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	for i := 1; i <= 4; i++ {
		seedKAnonDevTyped(t, db, "A", fmt.Sprintf("af%d", i), 10, 2, store.WorkTypeFeature, now)
		seedKAnonDevTyped(t, db, "A", fmt.Sprintf("ab%d", i), 10, 2, store.WorkTypeBug, now)
	}
	seedKAnonDevTyped(t, db, "B", "b1", 3, 7, store.WorkTypeFeature, now)
	seedTypedOutcomeCost(t, db, "b1", "bug-b1", 3, 7, store.WorkTypeBug, now)
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp := getScores(t, h, "/api/v1/scores?since="+scoresSince())
	if len(resp.Teams) != 0 || resp.Total != nil || len(resp.WorkTypes) != 0 {
		t.Errorf("B alone is the residual, below k: the whole response must be withheld; teams=%v total=%v work_types=%d",
			teamJSONNames(resp.Teams), resp.Total, len(resp.WorkTypes))
	}
	if resp.DataQuality == nil || resp.DataQuality.KAnonSuppressed == nil || resp.DataQuality.KAnonSuppressed.Developers != 1 {
		t.Fatalf("the withhold of B's 1 person must be declared; got %+v", resp.DataQuality)
	}
}

// TestKAnonResidual_CompareKSafeControlArm. Review mutated compareTeams to suppress
// UNCONDITIONALLY and the whole suite passed — TestKAnonResidual_CompareEndpoint only
// asserts ABSENCE, which an implementation that destroys every compare's total also
// satisfies. This is the arm that makes that test mean something.
func TestKAnonResidual_CompareKSafeControlArm(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	a := now.AddDate(0, 0, -20)
	b := now.AddDate(0, 0, -5)
	// Six developers active in BOTH windows, each with a DISTINCT issue per window so
	// the second outcome is not dropped by the merge-commit unique index.
	for i, d := range []string{"b1", "b2", "b3", "b4", "b5", "b6"} {
		seedKAnonDevIssue(t, db, "big", d, "a-"+d, float64(10+i), 2, a)
		seedKAnonDevIssue(t, db, "big", d, "b-"+d, float64(10+i), 2, now)
	}
	h.SetAggregation(scoring.AggregationTeam, 5)

	q := "/api/v1/scores/compare?since_a=" + a.AddDate(0, 0, -1).Format("2006-01-02") +
		"&until_a=" + b.Format("2006-01-02") + "&since_b=" + b.Format("2006-01-02")
	code, body := doRequest(t, h, http.MethodGet, q, nil)
	if code != http.StatusOK {
		t.Fatalf("compare: status %d, body %s", code, body)
	}
	var resp compareResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.KAnonSuppressed != nil {
		t.Errorf("a k-safe comparison must not suppress; got %+v", resp.KAnonSuppressed)
	}
	if resp.Total == nil {
		t.Error("a k-safe comparison must KEEP its grand-total delta — without this arm, an " +
			"implementation that suppressed unconditionally would pass the suppression test")
	}
	if !teamDeltaPresent(resp, "big") {
		t.Errorf("the k-clearing team must be named; got %+v", resp.Teams)
	}
}

// TestKAnonResidual_SuppressedCountExcludesIdleSeats. Review mutated
// `Developers: n` to `len(otherDevs)` and it survived: no fixture put a #39
// zero-activity seat in a SUPPRESSED residual, so "contributing, not seats" was
// unpinned on this path.
func TestKAnonResidual_SuppressedCountExcludesIdleSeats(t *testing.T) {
	rows := []scoring.LabeledScore{
		{Score: scoring.DeveloperScore{Developer: "real1", SampleN: 1, WeightedPoints: 1, TotalCostUSD: 10}},
		{Score: scoring.DeveloperScore{Developer: "real2", SampleN: 1, WeightedPoints: 1, TotalCostUSD: 10}},
		{Score: scoring.DeveloperScore{Developer: "idle1", SampleN: 0}}, // allocated but never used
		{Score: scoring.DeveloperScore{Developer: "idle2", SampleN: 0}},
	}
	_, sup := scoring.AggregateLabeledKAnon(rows, 5, scoring.Census{Uncounted: store.ResemblesUnattributed, UncapturedCost: func(row scoring.DeveloperScore, _ bool) bool { return store.ResemblesUnattributed(row.Developer) }, Pseudo: store.ResemblesUnattributed})
	if !sup.Any() {
		t.Fatal("2 contributors is below k=5 and must suppress")
	}
	if sup.Developers != 2 {
		t.Errorf("kanon_suppressed.developers = %d, want 2 — idle seats must not inflate the "+
			"count; padding a cohort with them must never make it look k-sized", sup.Developers)
	}
}

// seedKAnonDevTyped / seedKAnonDevIssue are the work-type-aware and distinct-issue
// variants of seedKAnonDev.
//
// ⚠️ seedKAnonDev is NOT safe to call twice for the same developer: seedRepoOutcomeAt
// derives MergeCommitSHA from repo+issue only, and that column is UNIQUE, so the second
// outcome is silently dropped. Review caught a compare fixture that did exactly this
// and shipped window B with weighted_points 0 — suppression still fired via cost, so
// the test passed while exercising a different scenario than its comment described.
func seedKAnonDevTyped(t *testing.T, db *store.DB, team, dev string, costUSD, weight float64, workType string, ts time.Time) {
	t.Helper()
	if err := baselineHierarchy(db, context.Background(), dev, team, "div-"+team, "acme"); err != nil {
		t.Fatalf("UpsertHierarchy(%s): %v", dev, err)
	}
	seedRepoCostAt(t, db, repoAlpha, dev, "i-"+dev, costUSD, ts)
	if _, err := db.InsertOutcome(context.Background(), store.Outcome{
		Developer: dev, IssueID: "i-" + dev, Repo: repoAlpha, Weight: weight, Quality: 1,
		WorkType: workType, WorkTypeSource: store.WorkTypeSourceLabel,
		MergeCommitSHA: "sha-typed-" + dev, Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertOutcome(%s): %v", dev, err)
	}
}

func seedKAnonDevIssue(t *testing.T, db *store.DB, team, dev, issue string, costUSD, weight float64, ts time.Time) {
	t.Helper()
	if err := baselineHierarchy(db, context.Background(), dev, team, "div-"+team, "acme"); err != nil {
		t.Fatalf("UpsertHierarchy(%s): %v", dev, err)
	}
	seedRepoCostAt(t, db, repoAlpha, dev, issue, costUSD, ts)
	if _, err := db.InsertOutcome(context.Background(), store.Outcome{
		Developer: dev, IssueID: issue, Repo: repoAlpha, Weight: weight, Quality: 1,
		MergeCommitSHA: "sha-" + issue, Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertOutcome(%s,%s): %v", dev, issue, err)
	}
}

func teamDeltaPresent(resp compareResponse, team string) bool {
	for _, r := range resp.Teams {
		if r.Team == team {
			return true
		}
	}
	return false
}

// seedPollerRemainder stores spend the way the org pollers do (#853): under the
// server-assigned `unattributed` pseudo-developer, with no repo and source
// anthropic-admin. The pseudo-developer cannot be enrolled in a team, so it always
// lands in the unlabelled group and folds into the "other" residual.
func seedPollerRemainder(t *testing.T, db *store.DB, costUSD float64, ts time.Time) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: store.UnattributedIssueID, IssueID: store.UnattributedIssueID,
		Model: "claude-sonnet-4", InputTok: 2000, CostMicro: store.DollarsToMicro(costUSD),
		Source: "anthropic-admin", Fidelity: "daily", Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent(poller remainder @%s): %v", ts, err)
	}
}

// TestKAnonResidual_PollerPseudoDeveloperDoesNotCompleteTheFloor is the #853
// reproduction: four real developers plus the pollers' `unattributed` row reached
// k=5, so the residual was published — its points were the four people's alone —
// while kanon_suppressed still said four developers were withheld.
func TestKAnonResidual_PollerPseudoDeveloperDoesNotCompleteTheFloor(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	for _, d := range []string{"f1", "f2", "f3", "f4"} {
		seedKAnonDev(t, db, "four", d, 10, 2, now)
	}
	seedPollerRemainder(t, db, 12.34, now)
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp := getScores(t, h, "/api/v1/scores?since="+scoresSince())
	if teamPresent(resp.Teams, scoring.OtherCohort) {
		t.Errorf("the residual holds 4 real developers (< k=5) and must be withheld; the "+
			"poller pseudo-developer is not a person and must not complete the floor; got %+v",
			teamByName(resp.Teams, scoring.OtherCohort))
	}
	if resp.Total != nil {
		t.Errorf("`total` must be withheld with the residual; got %+v", resp.Total)
	}
	if resp.DataQuality == nil || resp.DataQuality.KAnonSuppressed == nil {
		t.Fatal("the suppression must be declared")
	}
	if ks := resp.DataQuality.KAnonSuppressed; ks.Developers != 4 || ks.KAnonymity != 5 {
		t.Errorf("kanon_suppressed = %+v, want developers 4 (the pseudo-developer is not "+
			"counted) at k_anonymity 5", ks)
	}
}

// TestKAnonResidual_PollerPseudoDeveloperSpendIsDropped is the control arm for
// #853 under #864 D′: five real developers clear k=5 on their own, so the residual
// is shown, and the poller's remainder spend is in neither it nor the total. An
// anonymised response covers people only.
func TestKAnonResidual_PollerPseudoDeveloperSpendIsDropped(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	// Two sub-k teams pooling into a residual of 5 real contributors.
	for _, d := range []string{"x1", "x2", "x3"} {
		seedKAnonDev(t, db, "t1", d, 10, 2, now)
	}
	for _, d := range []string{"y1", "y2"} {
		seedKAnonDev(t, db, "t2", d, 10, 2, now)
	}
	seedPollerRemainder(t, db, 12.34, now)
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp := getScores(t, h, "/api/v1/scores?since="+scoresSince())
	other := teamByName(resp.Teams, scoring.OtherCohort)
	if other == nil {
		t.Fatalf("a residual of 5 real contributors clears k=5 and must be shown; got %v",
			teamJSONNames(resp.Teams))
	}
	if math.Abs(other.TotalCostUSD-50) > 1e-9 || other.WeightedPoints != 10 {
		t.Errorf("other = $%v / %v points, want $50 / 10: the pseudo-developer's $12.34 is dropped",
			other.TotalCostUSD, other.WeightedPoints)
	}
	if resp.DataQuality != nil && resp.DataQuality.KAnonSuppressed != nil {
		t.Errorf("nothing was suppressed; kanon_suppressed must be absent, got %+v",
			resp.DataQuality.KAnonSuppressed)
	}
	if resp.Total == nil || math.Abs(resp.Total.TotalCostUSD-50) > 1e-9 {
		t.Errorf("`total` must publish the people's $50 alone; got %+v", resp.Total)
	}
}

// TestKAnonResidual_PollerPseudoDeveloperAloneLeavesNoOther: with every real
// developer in a named team and the poller's remainder the only other spend, the
// residual is empty once the pseudo-developer is dropped (#864 D′), so the named
// team and the total publish and the total is that team's figures exactly.
func TestKAnonResidual_PollerPseudoDeveloperAloneLeavesNoOther(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	for _, d := range []string{"b1", "b2", "b3", "b4", "b5"} {
		seedKAnonDev(t, db, "big", d, 10, 2, now)
	}
	seedPollerRemainder(t, db, 12.34, now)
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp := getScores(t, h, "/api/v1/scores?since="+scoresSince())
	if names := teamJSONNames(resp.Teams); len(names) != 1 || names[0] != "big" {
		t.Fatalf("an 'other' left empty by the drop publishes; want [big], got %v", names)
	}
	if resp.DataQuality != nil && resp.DataQuality.KAnonSuppressed != nil {
		t.Errorf("nothing was suppressed; got %+v", resp.DataQuality.KAnonSuppressed)
	}
	if resp.Total == nil || resp.Total.TotalCostUSD != resp.Teams[0].TotalCostUSD {
		t.Errorf("total must equal the one team's cost exactly; total %+v team %+v", resp.Total, resp.Teams[0])
	}
}

// TestKAnonResidual_CompareEndpointPollerPseudoDeveloper is the #853 reproduction
// through /scores/compare: four real developers plus the poller row in BOTH windows.
// Each side of the residual has 4 real contributors (< k=5), so it is withheld from
// both sides and declared.
func TestKAnonResidual_CompareEndpointPollerPseudoDeveloper(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	a := now.AddDate(0, 0, -20)
	b := now.AddDate(0, 0, -5)
	for i, d := range []string{"f1", "f2", "f3", "f4"} {
		seedKAnonDevIssue(t, db, "four", d, "a-"+d, float64(10+i), 2, a)
		seedKAnonDevIssue(t, db, "four", d, "b-"+d, float64(10+i), 2, now)
	}
	seedPollerRemainder(t, db, 12.34, a)
	seedPollerRemainder(t, db, 12.34, now)
	h.SetAggregation(scoring.AggregationTeam, 5)

	q := "/api/v1/scores/compare?since_a=" + a.AddDate(0, 0, -1).Format("2006-01-02") +
		"&until_a=" + b.Format("2006-01-02") + "&since_b=" + b.Format("2006-01-02")
	code, body := doRequest(t, h, http.MethodGet, q, nil)
	if code != http.StatusOK {
		t.Fatalf("compare: status %d, body %s", code, body)
	}
	var resp compareResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if teamDeltaPresent(resp, scoring.OtherCohort) {
		t.Errorf("compare published a residual of 4 real developers (< k=5) because the "+
			"poller pseudo-developer completed the floor; got %+v", resp.Teams)
	}
	if resp.Total != nil {
		t.Errorf("the grand-total delta must be withheld with the residual; got %+v", resp.Total)
	}
	if resp.KAnonSuppressed == nil {
		t.Fatal("the suppression must be declared")
	}
	if ks := resp.KAnonSuppressed; ks.Developers != 4 || ks.KAnonymity != 5 {
		t.Errorf("kanon_suppressed = %+v, want developers 4 at k_anonymity 5", ks)
	}
}
