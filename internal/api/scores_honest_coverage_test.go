package api

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// TestGetScores_AttributedCostShareReflectsUnattributed is the #351 flagship for
// deliverable 1: a window where most spend is charged to the unattributed sentinel
// must report attributed_cost_share as the TRUE join fraction (~0.22 here), NOT the
// misleading ~1.0 the per-developer coverage_pct (capture fidelity) reads. Fails on
// main: the field does not exist.
func TestGetScores_AttributedCostShareReflectsUnattributed(t *testing.T) {
	h, db := newTestHandler(t)
	// $0.22 attributed to a real issue (with an outcome), $0.78 to the sentinel.
	seedCosts(t, db, "alice", "issue-1", 0.22)
	seedCosts(t, db, "alice", store.UnattributedIssueID, 0.78)
	seedOutcome(t, db, "alice", "issue-1", 3, 1)

	resp := decodeScores(t, h)
	if resp.DataQuality == nil {
		t.Fatal("data_quality absent; want the honest-coverage fields")
	}
	got := resp.DataQuality.AttributedCostShare
	if got == nil {
		t.Fatal("attributed_cost_share absent; want ~0.22")
	}
	if math.Abs(*got-0.22) > 1e-9 {
		t.Errorf("attributed_cost_share = %v, want 0.22 (NOT ~1.0)", *got)
	}
	// The per-developer coverage_pct is capture fidelity, NOT attribution coverage:
	// it still reads ~100% here, which is exactly why the new field is needed.
	if d, ok := scoresDevsFrom(resp)["alice"]; ok && d.CoveragePercent < 99 {
		t.Errorf("precondition: coverage_pct = %v, expected ~100 (fidelity, not attribution)", d.CoveragePercent)
	}
}

// TestGetScores_AttributedOutcomeShare pins the outcome-side join rate: two outcomes,
// one with matching token spend and one with none, must report
// attributed_outcome_share = 0.5.
func TestGetScores_AttributedOutcomeShare(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 1.0) // issue-1: 2000 tokens -> matched
	seedOutcome(t, db, "alice", "issue-1", 3, 1)
	seedOutcome(t, db, "alice", "issue-2", 3, 1) // issue-2: no token spend -> unmatched

	resp := decodeScores(t, h)
	if resp.DataQuality == nil || resp.DataQuality.AttributedOutcomeShare == nil {
		t.Fatal("attributed_outcome_share absent; want 0.5")
	}
	if got := *resp.DataQuality.AttributedOutcomeShare; math.Abs(got-0.5) > 1e-9 {
		t.Errorf("attributed_outcome_share = %v, want 0.5 (1 of 2 outcomes has matching cost)", got)
	}
}

// TestGetScores_UnjoinedDevelopersDeveloperMode is the #351 flagship for deliverable
// 2: cost keyed to an OS username and outcomes keyed to a GitHub login, with no alias
// mapping them, must surface LOUDLY as unjoined developers — named in developer mode
// so the operator can map them. This is the alice-has-cost / bob-has-
// outcomes mismatch that otherwise reads as a silent TIER=0.
func TestGetScores_UnjoinedDevelopersDeveloperMode(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 5.0)  // cost, no outcome
	seedOutcome(t, db, "bob", "issue-1", 3, 1) // outcome, no cost

	resp := decodeScores(t, h)
	if resp.DataQuality == nil || resp.DataQuality.UnjoinedDevelopers == nil {
		t.Fatal("unjoined_developers absent; want the identity mismatch flagged")
	}
	uj := resp.DataQuality.UnjoinedDevelopers
	if !containsStr(uj.CostOnly, "alice") {
		t.Errorf("cost_only = %v, want to contain alice (cost but no outcomes)", uj.CostOnly)
	}
	if !containsStr(uj.OutcomeOnly, "bob") {
		t.Errorf("outcome_only = %v, want to contain bob (outcomes but no cost)", uj.OutcomeOnly)
	}
	if uj.CostOnlyCount != 1 || uj.OutcomeOnlyCount != 1 {
		t.Errorf("counts = (%d,%d), want (1,1)", uj.CostOnlyCount, uj.OutcomeOnlyCount)
	}
}

// TestGetScores_ZeroAttributedCostShareEmitted is the pointer-design guard: a window
// whose spend is ENTIRELY charged to the unattributed sentinel must EMIT
// attributed_cost_share as a real 0.0 — the loudest honest signal — not drop it via
// omitempty. A refactor to a non-pointer float64 would silently hide "we can account
// for 0% of your spend"; this test fails if that regression lands.
func TestGetScores_ZeroAttributedCostShareEmitted(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", store.UnattributedIssueID, 1.0) // all spend, no real issue

	resp := decodeScores(t, h)
	if resp.DataQuality == nil || resp.DataQuality.AttributedCostShare == nil {
		t.Fatal("attributed_cost_share dropped; want an explicit 0.0")
	}
	if got := *resp.DataQuality.AttributedCostShare; got != 0.0 {
		t.Errorf("attributed_cost_share = %v, want 0.0 (all spend unattributed)", got)
	}
}

// TestGetScores_EmptyWindowNoDataQuality pins the presence floor: a truly empty window
// (no spend, no outcomes, nothing flagged) still ships NO data_quality key, so the
// always-present coverage fields do not spuriously materialize the block.
func TestGetScores_EmptyWindowNoDataQuality(t *testing.T) {
	h, _ := newTestHandler(t)

	resp := decodeScores(t, h)
	if resp.DataQuality != nil {
		t.Errorf("data_quality present for an empty window: %+v", resp.DataQuality)
	}
}

// TestGetScores_UnjoinedDevelopersTeamModeCountOnly pins the k-anon contract: in
// team-aggregation mode the unjoined flag emits COUNTS ONLY — never a developer name,
// mirroring how zero_token_outcomes is suppressed to a count in team mode.
func TestGetScores_UnjoinedDevelopersTeamModeCountOnly(t *testing.T) {
	h, db := newTeamModeHandler(t, 2)
	seedCosts(t, db, "alice", "issue-1", 5.0)
	seedOutcome(t, db, "bob", "issue-1", 3, 1)

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores = %d, body %s", code, body)
	}
	// No identity may leak in team mode, anywhere in the body.
	if bytes.Contains(body, []byte("alice")) || bytes.Contains(body, []byte("bob")) {
		t.Fatalf("team-mode response leaks an unjoined developer identity: %s", body)
	}
	resp := decodeScoresBody(t, body)
	if resp.DataQuality == nil || resp.DataQuality.UnjoinedDevelopers == nil {
		t.Fatal("unjoined_developers absent in team mode; want the name-free counts")
	}
	uj := resp.DataQuality.UnjoinedDevelopers
	if len(uj.CostOnly) != 0 || len(uj.OutcomeOnly) != 0 {
		t.Errorf("team mode leaked names: cost_only=%v outcome_only=%v", uj.CostOnly, uj.OutcomeOnly)
	}
	if uj.CostOnlyCount != 1 || uj.OutcomeOnlyCount != 1 {
		t.Errorf("counts = (%d,%d), want (1,1)", uj.CostOnlyCount, uj.OutcomeOnlyCount)
	}
	// The attribution shares are name-free, so they must carry in team mode too: the
	// alice cost is charged to a real issue, so attributed_cost_share == 1.0.
	if resp.DataQuality.AttributedCostShare == nil {
		t.Error("attributed_cost_share absent in team mode; the name-free share must carry")
	}
}

// TestGetScores_FullyJoinedWindowCleanCoverage is the negative control: a window
// where every dollar attributes to a real issue and every outcome has matching cost
// reports high coverage (both shares == 1.0) and an EMPTY unjoined flag.
func TestGetScores_FullyJoinedWindowCleanCoverage(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 2.0)
	seedOutcome(t, db, "alice", "issue-1", 3, 1)

	resp := decodeScores(t, h)
	if resp.DataQuality == nil {
		t.Fatal("data_quality absent; want the always-present coverage fields")
	}
	if resp.DataQuality.AttributedCostShare == nil || math.Abs(*resp.DataQuality.AttributedCostShare-1.0) > 1e-9 {
		t.Errorf("attributed_cost_share = %v, want 1.0", resp.DataQuality.AttributedCostShare)
	}
	if resp.DataQuality.AttributedOutcomeShare == nil || math.Abs(*resp.DataQuality.AttributedOutcomeShare-1.0) > 1e-9 {
		t.Errorf("attributed_outcome_share = %v, want 1.0", resp.DataQuality.AttributedOutcomeShare)
	}
	if resp.DataQuality.UnjoinedDevelopers != nil {
		t.Errorf("unjoined_developers present for a fully-joined window: %+v", resp.DataQuality.UnjoinedDevelopers)
	}
}

// TestGetScores_AliasHealsUnjoinedFlag proves the flag is a live signal, not a static
// warning: once an alias maps the GitHub login to the OS username, the two sides join
// and the unjoined flag clears.
func TestGetScores_AliasHealsUnjoinedFlag(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 5.0)
	seedOutcome(t, db, "bob", "issue-1", 3, 1)
	if err := db.UpsertDeveloperAlias(context.Background(), "bob", "alice"); err != nil {
		t.Fatalf("UpsertDeveloperAlias: %v", err)
	}

	resp := decodeScores(t, h)
	if resp.DataQuality != nil && resp.DataQuality.UnjoinedDevelopers != nil {
		t.Errorf("unjoined_developers still present after aliasing: %+v", resp.DataQuality.UnjoinedDevelopers)
	}
}

// TestGetScores_UnattributedBucketsAndExploratoryShare is the #refocus Option B
// flagship: the single unattributed mass is split into LABELED buckets on
// data_quality, the exploratory (main) bucket surfaces as both a bucket row and the
// scalar exploratory_cost_share, the buckets reconcile with attributed_cost_share to
// 1.0, and the per-developer exploratory share is emitted.
func TestGetScores_UnattributedBucketsAndExploratoryShare(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 0.40)                            // attributed
	seedCosts(t, db, "alice", store.UnattributedMainBucket, 0.30)         // exploratory
	seedCosts(t, db, "alice", store.UnattributedNoIssueBucket, 0.20)      // named branch, no issue
	seedCosts(t, db, "alice", store.UnattributedDetachedHEADBucket, 0.10) // detached HEAD
	seedOutcome(t, db, "alice", "issue-1", 3, 1)

	resp := decodeScores(t, h)
	if resp.DataQuality == nil {
		t.Fatal("data_quality absent; want the bucket split")
	}
	dq := resp.DataQuality

	// attributed_cost_share reconciles against the whole unattributed family (0.40).
	if dq.AttributedCostShare == nil || math.Abs(*dq.AttributedCostShare-0.40) > 1e-9 {
		t.Errorf("attributed_cost_share = %v, want 0.40", dq.AttributedCostShare)
	}
	// exploratory_cost_share is the main bucket alone (0.30).
	if dq.ExploratoryCostShare == nil || math.Abs(*dq.ExploratoryCostShare-0.30) > 1e-9 {
		t.Errorf("exploratory_cost_share = %v, want 0.30", dq.ExploratoryCostShare)
	}
	// Three labeled buckets, summing to 0.60, sorted by descending cost (main first).
	if len(dq.UnattributedBuckets) != 3 {
		t.Fatalf("unattributed_buckets has %d rows, want 3: %+v", len(dq.UnattributedBuckets), dq.UnattributedBuckets)
	}
	if dq.UnattributedBuckets[0].Bucket != store.UnattributedMainBucket {
		t.Errorf("top bucket = %q, want %q (largest cost first)", dq.UnattributedBuckets[0].Bucket, store.UnattributedMainBucket)
	}
	var bucketSum, shareSum float64
	for _, b := range dq.UnattributedBuckets {
		bucketSum += b.CostUSD
		shareSum += b.Share
	}
	if math.Abs(bucketSum-0.60) > 1e-6 {
		t.Errorf("bucket cost sum = %v, want 0.60", bucketSum)
	}
	// buckets + attributed reconcile to the whole window (no double-count, no gap).
	if math.Abs((shareSum+*dq.AttributedCostShare)-1.0) > 1e-9 {
		t.Errorf("bucket shares (%v) + attributed (%v) = %v, want 1.0", shareSum, *dq.AttributedCostShare, shareSum+*dq.AttributedCostShare)
	}
	// Per-developer exploratory share: alice's main (0.30) / alice total (1.00).
	if d, ok := scoresDevsFrom(resp)["alice"]; !ok {
		t.Fatal("alice missing from developer rows")
	} else if math.Abs(d.ExploratoryCostShare-0.30) > 1e-9 {
		t.Errorf("alice exploratory_cost_share = %v, want 0.30", d.ExploratoryCostShare)
	}
}

// TestGetScores_UnattributedBucketsTeamModeNameFree pins the k-anon contract for the
// bucket split: the labeled buckets and exploratory share are name-free, so they
// carry in team-aggregation mode — while no per-developer row (and thus no per-
// developer exploratory share) is emitted.
func TestGetScores_UnattributedBucketsTeamModeNameFree(t *testing.T) {
	h, db := newTeamModeHandler(t, 2)
	seedCosts(t, db, "alice", "issue-1", 0.50)
	seedCosts(t, db, "alice", store.UnattributedMainBucket, 0.50)

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores = %d, body %s", code, body)
	}
	if bytes.Contains(body, []byte("alice")) {
		t.Fatalf("team-mode response leaks a developer identity: %s", body)
	}
	resp := decodeScoresBody(t, body)
	if resp.DataQuality == nil || resp.DataQuality.ExploratoryCostShare == nil {
		t.Fatal("exploratory_cost_share absent in team mode; the name-free share must carry")
	}
	if math.Abs(*resp.DataQuality.ExploratoryCostShare-0.50) > 1e-9 {
		t.Errorf("exploratory_cost_share = %v, want 0.50", *resp.DataQuality.ExploratoryCostShare)
	}
	if len(resp.DataQuality.UnattributedBuckets) != 1 {
		t.Errorf("unattributed_buckets = %+v, want the single main bucket", resp.DataQuality.UnattributedBuckets)
	}
	if len(resp.Developers) != 0 {
		t.Errorf("team mode emitted %d developer rows, want 0", len(resp.Developers))
	}
}

// TestGetScores_NoBucketsWhenFullyAttributed pins the omit-when-clean contract: a
// window whose every dollar attributes to a real issue ships NO unattributed_buckets
// and NO exploratory_cost_share — the bucket split is an exception signal, not an
// always-present field. Mirrors how attributed_cost_share stays present but the
// bucket detail is suppressed on a clean window.
func TestGetScores_NoBucketsWhenFullyAttributed(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 1.0) // all attributed
	seedOutcome(t, db, "alice", "issue-1", 3, 1)

	resp := decodeScores(t, h)
	if resp.DataQuality == nil {
		t.Fatal("data_quality absent; want attributed_cost_share")
	}
	if resp.DataQuality.UnattributedBuckets != nil {
		t.Errorf("unattributed_buckets present for a fully-attributed window: %+v", resp.DataQuality.UnattributedBuckets)
	}
	if resp.DataQuality.ExploratoryCostShare != nil {
		t.Errorf("exploratory_cost_share present for a fully-attributed window: %v", *resp.DataQuality.ExploratoryCostShare)
	}
	// The per-developer share is a plain float64, so it is 0 (not nil) here.
	if d, ok := scoresDevsFrom(resp)["alice"]; ok && d.ExploratoryCostShare != 0 {
		t.Errorf("alice exploratory_cost_share = %v, want 0 (no exploratory spend)", d.ExploratoryCostShare)
	}
}

// TestGetScores_GenuineZeroExploratoryShare pins the pointer design for the
// exploratory share: a window with unattributed spend that is NONE of it exploratory
// main (only detached-head + branch-without-issue) must emit exploratory_cost_share
// as a real 0.0 — present because there IS unattributed spend, zero because none is
// main — not drop it. The buckets still list the two non-main reasons.
func TestGetScores_GenuineZeroExploratoryShare(t *testing.T) {
	h, db := newTestHandler(t)
	seedCosts(t, db, "alice", "issue-1", 0.50)
	seedCosts(t, db, "alice", store.UnattributedDetachedHEADBucket, 0.30)
	seedCosts(t, db, "alice", store.UnattributedNoIssueBucket, 0.20)

	resp := decodeScores(t, h)
	if resp.DataQuality == nil || resp.DataQuality.ExploratoryCostShare == nil {
		t.Fatal("exploratory_cost_share dropped; want an explicit 0.0 (unattributed exists but none is main)")
	}
	if *resp.DataQuality.ExploratoryCostShare != 0.0 {
		t.Errorf("exploratory_cost_share = %v, want 0.0", *resp.DataQuality.ExploratoryCostShare)
	}
	if len(resp.DataQuality.UnattributedBuckets) != 2 {
		t.Errorf("unattributed_buckets = %+v, want 2 (detached-head + branch-without-issue)", resp.DataQuality.UnattributedBuckets)
	}
	if d, ok := scoresDevsFrom(resp)["alice"]; ok && d.ExploratoryCostShare != 0 {
		t.Errorf("alice exploratory_cost_share = %v, want 0", d.ExploratoryCostShare)
	}
}

// decodeScoresBody unmarshals a raw /scores body captured by the caller (used when a
// test needs the raw bytes for a leak assertion before decoding).
func decodeScoresBody(t *testing.T, body []byte) scoresResponse {
	t.Helper()
	var resp scoresResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal scores: %v", err)
	}
	return resp
}

// scoresDevsFrom keys an already-decoded response's developer rows by name.
func scoresDevsFrom(resp scoresResponse) map[string]developerScoreJSON {
	out := map[string]developerScoreJSON{}
	for _, d := range resp.Developers {
		out[d.Developer] = d
	}
	return out
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
