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
// Steve ruled this shape (option A, 2026-08-03) over complementary suppression and
// over merging the residual into a named group.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// seedKAnonDev enrols a developer in a team and gives them one cost-bearing outcome.
func seedKAnonDev(t *testing.T, db *store.DB, team, dev string, costUSD, weight float64, ts time.Time) {
	t.Helper()
	if err := db.UpsertHierarchy(context.Background(), dev, team, "div-"+team, "acme"); err != nil {
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
	if !ks.WithheldTotal || !ks.WithheldCostComposition {
		t.Errorf("kanon_suppressed must state what else was withheld; got %+v", ks)
	}
}

// TestKAnonResidual_NoDifferencingChannel is the assertion a row-only fix fails.
// With one named team and a suppressed residual, `total` minus the named row would
// hand back the hidden cohort. The only safe answer is that `total` is not there.
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
	if !teamPresent(resp.Teams, "big") {
		t.Fatalf("the k-clearing team must still be named; got %v", teamJSONNames(resp.Teams))
	}
	if teamPresent(resp.Teams, "other") {
		t.Errorf("sub-k residual must be withheld; got %v", teamJSONNames(resp.Teams))
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
	if resp.Total == nil {
		t.Error("`total` must survive when nothing was suppressed")
	}
	if resp.CostComposition == nil {
		t.Error("cost_composition must survive when nothing was suppressed")
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
		if err := db.UpsertHierarchy(context.Background(), dev, team, "div-"+team, "acme"); err != nil {
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
		if err := db.UpsertHierarchy(context.Background(), d, team, "div-"+team, "acme"); err != nil {
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
		if err := db.UpsertHierarchy(context.Background(), dev, team, "div-"+team, "acme"); err != nil {
			t.Fatalf("UpsertHierarchy(%s): %v", dev, err)
		}
		seedRepoCostAt(t, db, repoAlpha, dev, "ppi-"+dev, attributedUSD, now)
		seedRepoCostAt(t, db, repoAlpha, dev, store.UnattributedMainBucket, mainUSD, now)
	}
}

// --- Guards that review MUTATION-PROVED were untested ---

// TestKAnonResidual_SegmentTotalWithheldAndDeclared. Review deleted the
// `&& !segSup.Any()` gate on the segment total and the ENTIRE suite passed — nothing
// anywhere asserted a segment's own total is withheld, only that its "other" row was
// absent. Same mutation class as the pooled guard, one nesting level down.
func TestKAnonResidual_SegmentTotalWithheldAndDeclared(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	// One team of 6 so the WINDOW residual is empty (no pooled suppression from the
	// pooled population itself); 5 do feature work, 1 does security. The security
	// segment is therefore sub-k on its own.
	for i, d := range []string{"f1", "f2", "f3", "f4", "f5"} {
		seedKAnonDevTyped(t, db, "eng", d, float64(10+i), 2, store.WorkTypeFeature, now)
	}
	seedKAnonDevTyped(t, db, "eng", "sec1", 7.77, 3, store.WorkTypeSecurity, now)
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp := getScores(t, h, "/api/v1/scores?since="+scoresSince())
	var security, feature *workTypeSegmentJSON
	for i := range resp.WorkTypes {
		switch resp.WorkTypes[i].WorkType {
		case store.WorkTypeSecurity:
			security = &resp.WorkTypes[i]
		case store.WorkTypeFeature:
			feature = &resp.WorkTypes[i]
		}
	}
	if security == nil || feature == nil {
		t.Fatalf("expected both segments; got %+v", resp.WorkTypes)
	}
	if security.Total != nil {
		t.Errorf("a sub-k segment must withhold its OWN total; got %+v", security.Total)
	}
	if security.KAnonSuppressed == nil {
		t.Error("a suppressed segment must DECLARE it — otherwise a missing segment total " +
			"reads as 'no work of this type', which is a different statement")
	}
}

// TestKAnonResidual_SegmentSuppressionEscalatesBothWays is the leak review found that
// per-segment flooring alone does NOT close.
//
// 🔴 Weighted points partition EXACTLY across work types (every outcome carries one
// work_type), so the segments are a complete disjoint cover. That makes the two levels
// mutually reconstructive, in BOTH directions:
//
//	pooled total - Σ(visible segment totals)  == the suppressed segment
//	Σ(visible segment totals) - (named pooled rows) == the suppressed pooled cohort
//
// Measured before the fix: pooled 13 points / $67.77 minus a visible feature segment of
// 10 / $60.00 returned 3 points / $7.77 — the suppressed developer, exactly. So a
// suppression at EITHER level must withhold BOTH levels' totals.
func TestKAnonResidual_SegmentSuppressionEscalatesBothWays(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	for i, d := range []string{"f1", "f2", "f3", "f4", "f5"} {
		seedKAnonDevTyped(t, db, "eng", d, float64(10+i), 2, store.WorkTypeFeature, now)
	}
	seedKAnonDevTyped(t, db, "eng", "sec1", 7.77, 3, store.WorkTypeSecurity, now)
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp := getScores(t, h, "/api/v1/scores?since="+scoresSince())

	// The pooled total must be gone even though the POOLED population is k-safe —
	// it is the escalation, not the pooled floor, that removes it.
	if resp.Total != nil {
		t.Errorf("a suppressed SEGMENT must escalate to the pooled total: pooled - Σsegments "+
			"reconstructs the hidden segment; got %+v", resp.Total)
	}
	// And every segment total must be gone, or Σsegments - named rows leaks the same
	// cohort in reverse.
	for _, seg := range resp.WorkTypes {
		if seg.Total != nil {
			t.Errorf("segment %q kept its total under suppression; Σsegments - named rows "+
				"reconstructs the hidden cohort in reverse: %+v", seg.WorkType, seg.Total)
		}
		if seg.KAnonSuppressed == nil {
			t.Errorf("segment %q withheld its total without declaring it", seg.WorkType)
		}
	}
	if resp.DataQuality == nil || resp.DataQuality.KAnonSuppressed == nil {
		t.Fatal("the escalated suppression must be declared at the top level")
	}
	// 🔴 THE ESCALATION PATH BUILDS ITS OWN DECLARATION, and it is a DIFFERENT literal
	// from the pooled-first path's. The pooled path sets every flag at an earlier
	// attach site; this path flips suppression on AFTER that site was skipped, so the
	// strip-pass fallback is the only declaration the response gets. A flag missing
	// there withholds the figure and then reports that it was not withheld — the exact
	// confusion these flags exist to prevent, in the branch that needed them most.
	sup := resp.DataQuality.KAnonSuppressed
	if !sup.WithheldTotal || !sup.WithheldCostComposition || !sup.WithheldSegmentReconciliation {
		t.Errorf("escalated suppression under-declares what it withheld: "+
			"withheld_total=%v withheld_cost_composition=%v withheld_segment_reconciliation=%v; "+
			"all three are stripped on this path",
			sup.WithheldTotal, sup.WithheldCostComposition, sup.WithheldSegmentReconciliation)
	}
	// And the figures really are gone — without this the assertion above is satisfied
	// by a response that declares a withhold it never performed.
	if resp.CostComposition != nil {
		t.Errorf("cost_composition survived the escalated suppression: %+v", resp.CostComposition)
	}
	if resp.SegmentReconciliation != nil {
		t.Errorf("segment_reconciliation survived the escalated suppression; its "+
			"window_cost_micro (%d) is the whole-window total the strip pass just withheld",
			resp.SegmentReconciliation.Total.WindowCostMicro)
	}
}

// TestKAnonResidual_WorkTypeFilterCannotBypassEscalation is the measured leak that
// ?work_type opened, and it is the reason buildWorkTypeSegments builds every type and
// narrows only its OUTPUT.
//
// 🔴 THE CHANNEL. The #593 escalation reads the EMITTED segments. Before this fix a
// filter built only the requested type, so a sub-k segment of another type was never
// CONSTRUCTED, nothing escalated, and the three whole-window aggregates — `total`,
// `cost_composition` and `segment_reconciliation` — shipped in full, because none of
// them is narrowed by ?work_type.
//
// Measured on this exact fixture before the fix:
//
//	GET /scores                      -> total, cost_composition, segment_reconciliation ALL withheld
//	GET /scores?work_type=feature    -> all three PRESENT
//	  total.weighted_points 13 - visible feature segment 10        == 3   (the hidden dev's points)
//	  outcome_linked_cost_micro 67_770_000 - segment $60.00        == $7.77 (their cost, to the cent)
//
// A filter is a VIEW. It must never be a way to opt out of a suppression the same
// data triggers without it.
func TestKAnonResidual_WorkTypeFilterCannotBypassEscalation(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	for i, d := range []string{"f1", "f2", "f3", "f4", "f5"} {
		seedKAnonDevTyped(t, db, "eng", d, float64(10+i), 2, store.WorkTypeFeature, now)
	}
	// One sub-k developer in a DIFFERENT type — the cohort the filter used to hide.
	seedKAnonDevTyped(t, db, "eng", "sec1", 7.77, 3, store.WorkTypeSecurity, now)
	h.SetAggregation(scoring.AggregationTeam, 5)

	// Control arm FIRST: the unfiltered request must suppress. If it does not, the
	// fixture no longer reproduces and every assertion below is vacuous.
	unfiltered := getScores(t, h, "/api/v1/scores?since="+scoresSince())
	if unfiltered.Total != nil || unfiltered.CostComposition != nil || unfiltered.SegmentReconciliation != nil {
		t.Fatalf("fixture no longer reproduces: the UNFILTERED request did not suppress "+
			"(total=%v cost_composition=%v segment_reconciliation=%v)",
			unfiltered.Total != nil, unfiltered.CostComposition != nil,
			unfiltered.SegmentReconciliation != nil)
	}

	// The filtered request must reach the SAME conclusion on the SAME data.
	for _, wt := range []string{store.WorkTypeFeature, store.WorkTypeSecurity} {
		t.Run(wt, func(t *testing.T) {
			resp := getScores(t, h, "/api/v1/scores?work_type="+wt+"&since="+scoresSince())
			if resp.Total != nil {
				t.Errorf("?work_type=%s shipped the pooled total (%v points / $%v); minus the "+
					"visible segment it returns the suppressed cohort exactly",
					wt, resp.Total.WeightedPoints, resp.Total.TotalCostUSD)
			}
			if resp.CostComposition != nil {
				t.Errorf("?work_type=%s shipped cost_composition (total $%v)",
					wt, resp.CostComposition.TotalCostUSD)
			}
			if resp.SegmentReconciliation != nil {
				t.Errorf("?work_type=%s shipped segment_reconciliation; its "+
					"outcome_linked_cost_micro (%d) minus the visible segment's cost returns "+
					"the suppressed developer's spend to the cent (#466 x #593)",
					wt, resp.SegmentReconciliation.Total.OutcomeLinkedCostMicro)
			}
			for _, seg := range resp.WorkTypes {
				if seg.Total != nil {
					t.Errorf("?work_type=%s: segment %q kept its total under an escalated "+
						"suppression", wt, seg.WorkType)
				}
			}
			if resp.DataQuality == nil || resp.DataQuality.KAnonSuppressed == nil {
				t.Fatalf("?work_type=%s: nothing was declared; a filtered response that "+
					"withholds must say so", wt)
			}
			if !resp.DataQuality.KAnonSuppressed.WithheldSegmentReconciliation {
				t.Errorf("?work_type=%s: withheld_segment_reconciliation = false beside an "+
					"absent block", wt)
			}
		})
	}
}

// TestKAnonResidual_WorkTypeFilterStillNarrowsWhenKSafe is the CONTROL ARM for the
// test above. Building every type internally must not turn the filter into a no-op:
// on k-SAFE data a filtered response still returns exactly one segment, and still
// ships the whole-window aggregates. Without this, "suppress everything, always"
// would pass the leak test perfectly.
func TestKAnonResidual_WorkTypeFilterStillNarrowsWhenKSafe(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	// Every cohort k-safe in BOTH types, so nothing suppresses anywhere.
	for i, d := range []string{"f1", "f2", "f3", "f4", "f5"} {
		seedKAnonDevTyped(t, db, "eng", d, float64(10+i), 2, store.WorkTypeFeature, now)
	}
	for i, d := range []string{"s1", "s2", "s3", "s4", "s5"} {
		seedKAnonDevTyped(t, db, "eng", d, float64(20+i), 3, store.WorkTypeSecurity, now)
	}
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp := getScores(t, h, "/api/v1/scores?work_type=feature&since="+scoresSince())
	if len(resp.WorkTypes) != 1 || resp.WorkTypes[0].WorkType != store.WorkTypeFeature {
		t.Fatalf("?work_type=feature must return exactly the feature segment; got %d: %+v",
			len(resp.WorkTypes), resp.WorkTypes)
	}
	if resp.WorkTypes[0].Total == nil {
		t.Error("k-safe filtered segment lost its total; the filter must narrow, not suppress")
	}
	if resp.SegmentReconciliation == nil {
		t.Error("k-safe filtered response lost segment_reconciliation")
	}
	if resp.Total == nil {
		t.Error("k-safe filtered response lost the pooled total")
	}
	// A filter naming a type with NO outcomes still gets a well-formed empty segment.
	empty := getScores(t, h, "/api/v1/scores?work_type=incident&since="+scoresSince())
	if len(empty.WorkTypes) != 1 || empty.WorkTypes[0].WorkType != store.WorkTypeIncident {
		t.Errorf("a filter on an absent type must still yield one well-formed segment; got %+v",
			empty.WorkTypes)
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
	devs := []scoring.DeveloperScore{
		{Developer: "real1", SampleN: 1, WeightedPoints: 1, TotalCostUSD: 10},
		{Developer: "real2", SampleN: 1, WeightedPoints: 1, TotalCostUSD: 10},
		{Developer: "idle1", SampleN: 0}, // allocated but never used
		{Developer: "idle2", SampleN: 0},
	}
	_, sup := scoring.AggregateTeamsKAnon(devs, map[string]string{}, 5)
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
	if err := db.UpsertHierarchy(context.Background(), dev, team, "div-"+team, "acme"); err != nil {
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
	if err := db.UpsertHierarchy(context.Background(), dev, team, "div-"+team, "acme"); err != nil {
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
