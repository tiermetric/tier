package api

import (
	"bytes"
	"math"
	"net/http"
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// The tests below pin the post-rebase contract (#270 rebased onto #355/#357/#360):
// division mode is a THIRD aggregation level that must COMPOSE with every name-free
// honesty/meaning field main added while the branch was open. The single predicate
// that unifies team and division is scoring.AggregationMode.Anonymized(); each field
// that keys on it inherits division mode for free. These tests are the division
// analogues of the team-mode k-anon proofs in scores_honest_coverage_test.go and
// scores_cost_per_point_test.go.

// TestGetScores_DivisionMode_CostPerPointOnAggregate proves the #239 cost_per_point
// field is computed ON the division aggregate exactly as on a team aggregate: the
// division's summed cost / its summed weighted points, carried through the shared
// newTeamScoreJSON mapper. cost_per_point is a name-free ratio, so it must survive
// the anonymized rollup -- the "meaning" layer applies at division granularity too.
func TestGetScores_DivisionMode_CostPerPointOnAggregate(t *testing.T) {
	h, db := newDivisionModeHandler(t, 3)
	// One >= k division "engineering": cost 10+10+10 = 30, points 3+4+5 = 12.
	seedDivisionMember(t, db, "e1", "i-e1", "platform", "engineering", 10, 3)
	seedDivisionMember(t, db, "e2", "i-e2", "platform", "engineering", 10, 4)
	seedDivisionMember(t, db, "e3", "i-e3", "infra", "engineering", 10, 5)

	_, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	resp := unmarshalScores(t, body)

	eng := teamByName(resp.Teams, "engineering")
	if eng == nil {
		t.Fatalf("expected a named 'engineering' division; got %v", teamJSONNames(resp.Teams))
	}
	// 30 / 12 = 2.5 USD per weighted point, on the division aggregate.
	engCPP := cppVal(t, eng.CostPerPoint)
	if math.Abs(engCPP-2.5) > 1e-9 {
		t.Errorf("division cost_per_point = %v, want 2.5 ($30 / 12 points)", engCPP)
	}
	// It must equal total_cost_usd / weighted_points computed from the same row -- no
	// drift between the two, i.e. the mapper carried the engine's value verbatim.
	if math.Abs(engCPP-eng.TotalCostUSD/eng.WeightedPoints) > 1e-9 {
		t.Errorf("cost_per_point %v != total/points %v", engCPP, eng.TotalCostUSD/eng.WeightedPoints)
	}
	// The grand-total block carries it too (same mapper).
	if resp.Total == nil {
		t.Fatalf("division mode should still ship the grand-total block")
	}
	if totCPP := cppVal(t, resp.Total.CostPerPoint); math.Abs(totCPP-2.5) > 1e-9 {
		t.Errorf("division total cost_per_point = %v, want 2.5", totCPP)
	}
}

// TestGetScores_DivisionMode_SegmentCostPerPoint proves cost_per_point also carries on
// the DIVISION rows inside the #187 work-type segments, not just the pooled view. The
// segment path maps through the same newTeamScoreJSON, so a division row in a segment
// must carry the ratio on its summed totals -- the field flows through a mode-agnostic
// mapper with no division-specific pin otherwise, so this guards a silent regression.
func TestGetScores_DivisionMode_SegmentCostPerPoint(t *testing.T) {
	h, db := newDivisionModeHandler(t, 3)
	// One >= k division "engineering", all in the default work-type: cost 30, points 12.
	seedDivisionMember(t, db, "e1", "i-e1", "platform", "engineering", 10, 3)
	seedDivisionMember(t, db, "e2", "i-e2", "platform", "engineering", 10, 4)
	seedDivisionMember(t, db, "e3", "i-e3", "infra", "engineering", 10, 5)

	_, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	raw := string(body)
	resp := unmarshalScores(t, body)
	if len(resp.WorkTypes) == 0 {
		t.Fatalf("expected at least one work-type segment; body:\n%s", raw)
	}
	sawEng := false
	for _, seg := range resp.WorkTypes {
		if len(seg.Developers) != 0 {
			t.Errorf("segment %q emitted developer rows in division mode", seg.WorkType)
		}
		eng := teamByName(seg.Teams, "engineering")
		if eng == nil {
			continue
		}
		sawEng = true
		// 30 / 12 = 2.5 on the segment's division aggregate.
		if segCPP := cppVal(t, eng.CostPerPoint); math.Abs(segCPP-2.5) > 1e-9 {
			t.Errorf("segment %q division cost_per_point = %v, want 2.5", seg.WorkType, segCPP)
		}
		// The fixture seeds a non-zero denominator (cost 30, points 12), so the
		// consistency check asserts UNCONDITIONALLY. A `WeightedPoints > 0` guard here
		// would let a zero-denominator fixture silently skip the assertion -- a test
		// that skips is not testing. Pin the totals so the denominator stays non-zero.
		if eng.WeightedPoints != 12 {
			t.Errorf("segment division weighted_points = %v, want 12", eng.WeightedPoints)
		}
		if eng.TotalCostUSD != 30 {
			t.Errorf("segment division total_cost_usd = %v, want 30", eng.TotalCostUSD)
		}
		if segCPP := cppVal(t, eng.CostPerPoint); math.Abs(segCPP-eng.TotalCostUSD/eng.WeightedPoints) > 1e-9 {
			t.Errorf("segment cost_per_point %v != total/points %v", segCPP, eng.TotalCostUSD/eng.WeightedPoints)
		}
	}
	if !sawEng {
		t.Errorf("no segment carried an 'engineering' division row; body:\n%s", raw)
	}
}

// TestGetScores_DivisionMode_UnjoinedDevelopersCountOnly is the division analogue of
// TestGetScores_UnjoinedDevelopersTeamModeCountOnly and the direct regression test
// for the rebase hazard: the unjoined-developer name suppression keys on
// Anonymized(), NOT on == AggregationTeam. A `!= AggregationTeam` guard (main's
// pre-division shape) would treat division as "developer mode" and LEAK the names.
// Counts must carry; names must not.
func TestGetScores_DivisionMode_UnjoinedDevelopersCountOnly(t *testing.T) {
	h, db := newDivisionModeHandler(t, 2)
	// alice has cost but no outcome; bob the reverse -- one each side.
	seedCosts(t, db, "alice", "issue-1", 5.0)
	seedOutcome(t, db, "bob", "issue-1", 3, 1)

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores = %d, body %s", code, body)
	}
	// No identity may leak ANYWHERE in the body in an anonymized mode.
	if bytes.Contains(body, []byte("alice")) || bytes.Contains(body, []byte("bob")) {
		t.Fatalf("division-mode response leaked an unjoined developer identity: %s", body)
	}
	resp := decodeScoresBody(t, body)
	if resp.DataQuality == nil || resp.DataQuality.UnjoinedDevelopers == nil {
		t.Fatal("unjoined_developers absent in division mode; want the name-free counts")
	}
	uj := resp.DataQuality.UnjoinedDevelopers
	if len(uj.CostOnly) != 0 || len(uj.OutcomeOnly) != 0 {
		t.Errorf("division mode leaked names: cost_only=%v outcome_only=%v", uj.CostOnly, uj.OutcomeOnly)
	}
	if uj.CostOnlyCount != 1 || uj.OutcomeOnlyCount != 1 {
		t.Errorf("counts = (%d,%d), want (1,1)", uj.CostOnlyCount, uj.OutcomeOnlyCount)
	}
	// The name-free attribution shares must still carry in division mode. alice's
	// $5 is charged to a real issue with no unattributed spend, so the whole window cost
	// is attributed: attributed_cost_share is present and == 1.0. Pin the value (mirroring
	// the attributed_outcome_share == 0.0 assertion below); a nil-only check would pass
	// even if the share silently regressed to a wrong number.
	// #593: this fixture is two developers, below the effective floor of 3, so the
	// response IS suppressed and the cost-ratio shares are withheld — they invert to
	// the window total that suppression exists to hide. The unjoined COUNTS asserted
	// above are deliberately NOT withheld (review measured them non-invertible), which
	// is what this test is actually for. The fixture cannot be padded: every added
	// developer is cost-only, outcome-only, or joined, and each moves one of this
	// test's own assertions.
	if resp.DataQuality.KAnonSuppressed == nil {
		t.Fatal("a 2-developer fixture must suppress at the effective floor of 3")
	}
	if resp.DataQuality.AttributedCostShare != nil {
		t.Errorf("attributed_cost_share must be withheld under suppression; got %v",
			*resp.DataQuality.AttributedCostShare)
	}
	// The single outcome (bob on issue-1) has no matching cost row under that
	// identity, so it is unjoined: attributed_outcome_share is present and 0.0 -- a
	// name-free share that must carry in division mode exactly as in developer mode.
	if resp.DataQuality.AttributedOutcomeShare == nil {
		t.Fatal("attributed_outcome_share absent in division mode; the name-free share must carry")
	}
	if *resp.DataQuality.AttributedOutcomeShare != 0.0 {
		t.Errorf("attributed_outcome_share = %v, want 0.0 (the lone outcome is unjoined)", *resp.DataQuality.AttributedOutcomeShare)
	}
}

// TestGetScores_DivisionMode_UnattributedBucketsNameFree is the division analogue of
// TestGetScores_UnattributedBucketsTeamModeNameFree: the labeled unattributed buckets
// (#360) and the scalar exploratory_cost_share are org-wide name-free aggregates, so
// they carry in division mode -- while NO per-developer row (and thus no per-developer
// exploratory share) is emitted.
func TestGetScores_DivisionMode_UnattributedBucketsNameFree(t *testing.T) {
	h, db := newDivisionModeHandler(t, 2)
	// #593: proportional padding — see the team-mode twin for why the mix must match.
	padProportional(t, db, "paddiv", 3, 0.50, 0.50)
	enrolIn(t, db, "paddiv", "alice")
	seedCosts(t, db, "alice", "issue-1", 0.50)                    // attributed
	seedCosts(t, db, "alice", store.UnattributedMainBucket, 0.50) // exploratory main

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores = %d, body %s", code, body)
	}
	if bytes.Contains(body, []byte("alice")) {
		t.Fatalf("division-mode response leaks a developer identity: %s", body)
	}
	resp := decodeScoresBody(t, body)
	if resp.DataQuality == nil || resp.DataQuality.ExploratoryCostShare == nil {
		t.Fatal("exploratory_cost_share absent in division mode; the name-free share must carry")
	}
	if math.Abs(*resp.DataQuality.ExploratoryCostShare-0.50) > 1e-9 {
		t.Errorf("exploratory_cost_share = %v, want 0.50", *resp.DataQuality.ExploratoryCostShare)
	}
	if len(resp.DataQuality.UnattributedBuckets) != 1 {
		t.Errorf("unattributed_buckets = %+v, want the single main bucket", resp.DataQuality.UnattributedBuckets)
	}
	if len(resp.Developers) != 0 {
		t.Errorf("division mode emitted %d developer rows, want 0", len(resp.Developers))
	}
}

// TestGetScores_DivisionMode_SubKDivisionNoPerDevFieldReExposure proves a sub-k
// division folds to "other" AND that the fold does not re-expose any per-developer
// honesty field. The "other" bucket carries the aggregate cost_per_point (a name-free
// ratio on summed totals), but the body ships NO per-developer exploratory_cost_share
// key and no developer/team names -- the sub-k cohort's per-dev signals stay dissolved
// in the anonymity set.
func TestGetScores_DivisionMode_SubKDivisionNoPerDevFieldReExposure(t *testing.T) {
	h, db := newDivisionModeHandler(t, 3)
	// Named >= k division "engineering" (3 contributors), plus a sub-k division
	// "research" (2 contributors) that must fold to "other". Research carries some
	// exploratory-main spend so a per-dev leak, if any, would show a nonzero share.
	// Identities use distinctive multi-char tokens so the whole-body substring leak
	// scan below cannot false-positive against a rendered float (e.g. "e1" inside
	// "1e10") -- the same discipline as the sibling name-free proofs.
	seedDivisionMember(t, db, "eng_dev_one", "i-e1", "platform_team", "engineering", 10, 3)
	seedDivisionMember(t, db, "eng_dev_two", "i-e2", "platform_team", "engineering", 10, 4)
	seedDivisionMember(t, db, "eng_dev_three", "i-e3", "infra_team", "engineering", 10, 5)
	seedDivisionMember(t, db, "res_dev_one", "i-r1", "research_team", "research", 8, 4)
	seedDivisionMember(t, db, "res_dev_two", "i-r2", "research_team", "research", 8, 4)
	// #593: a THIRD sub-k division so the residual pools to 3 contributors and CLEARS
	// the floor. Without it the residual would be withheld entirely and this test could
	// no longer exercise its actual subject — that a folded aggregate re-exposes no
	// per-developer field. The suppression path itself is covered by
	// TestGetScores_DivisionMode_SubKDivisionFoldsToOther and the scoring-level tests.
	seedDivisionMember(t, db, "lab_dev_one", "i-l1", "lab_team", "labs", 8, 4)
	seedCosts(t, db, "res_dev_one", store.UnattributedMainBucket, 4.0) // exploratory main for a sub-k dev

	_, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	raw := string(body)
	// No developer, team, or sub-k division identity may appear anywhere in the body.
	for _, id := range []string{
		"eng_dev_one", "eng_dev_two", "eng_dev_three", "res_dev_one", "res_dev_two",
		"lab_dev_one", "platform_team", "infra_team", "research_team", "research",
		"lab_team", "labs",
	} {
		if bytes.Contains(body, []byte(id)) {
			t.Errorf("division mode re-exposed identity %q from a sub-k cohort:\n%s", id, raw)
		}
	}
	// The exploratory_cost_share key must appear EXACTLY once -- the single org-wide
	// name-free scalar inside data_quality. A per-developer re-exposure would emit it
	// again on a developer row, pushing the count to >= 2; the anonymized body has no
	// developer rows, so 1 is the only correct count. (A `&& !contains(data_quality)`
	// form would be dead here: this window always ships a data_quality block.)
	if n := bytes.Count(body, []byte(`"exploratory_cost_share"`)); n != 1 {
		t.Errorf("exploratory_cost_share key appears %d times, want exactly 1 (the org scalar, no per-dev leak):\n%s", n, raw)
	}
	resp := unmarshalScores(t, body)
	if len(resp.Developers) != 0 {
		t.Errorf("division mode emitted %d developer rows, want 0", len(resp.Developers))
	}
	if teamPresent(resp.Teams, "research") {
		t.Errorf("sub-k division 'research' must fold to 'other', not be named; got %v", teamJSONNames(resp.Teams))
	}
	other := teamByName(resp.Teams, "other")
	if other == nil {
		t.Fatalf("expected an 'other' fold: the residual pools research+labs = 3 contributors, "+
			"which CLEARS the floor and must still be emitted (#593 withholds only a sub-k "+
			"residual); got %v", teamJSONNames(resp.Teams))
	}
	// The folded aggregate carries a name-free cost_per_point on its own summed totals.
	// The fold's totals are deterministic. Two sub-k divisions pool into the residual:
	//   research: 2 devs x (points 4, cost 8) = points 8,  cost 16
	//   labs:     1 dev  x (points 4, cost 8) = points 4,  cost  8
	//   plus res_dev_one's 4.0 exploratory-main spend, which rolls into window cost
	//   => points 12, cost 28, cost_per_point = 28/12
	// Pin the denominator so the consistency check asserts UNCONDITIONALLY; a
	// `WeightedPoints > 0` guard would let a zero-denominator fixture silently skip it.
	if other.WeightedPoints != 12 {
		t.Errorf("'other' weighted_points = %v, want 12", other.WeightedPoints)
	}
	if other.TotalCostUSD != 28 {
		t.Errorf("'other' total_cost_usd = %v, want 28", other.TotalCostUSD)
	}
	otherCPP := cppVal(t, other.CostPerPoint)
	if math.Abs(otherCPP-28.0/12.0) > 1e-9 {
		t.Errorf("'other' cost_per_point = %v, want %v ($28 / 12 points)", otherCPP, 28.0/12.0)
	}
	if math.Abs(otherCPP-other.TotalCostUSD/other.WeightedPoints) > 1e-9 {
		t.Errorf("'other' cost_per_point %v != total/points %v", otherCPP, other.TotalCostUSD/other.WeightedPoints)
	}
}
