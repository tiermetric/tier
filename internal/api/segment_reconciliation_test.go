package api

// #466: the work-type segments denominate on outcome-linked cost, so spend that
// produced no outcome lands in NO segment and every per-type TIER reads better than
// the pooled headline score — which correctly keeps that spend in its denominator.
// GET /api/v1/scores now ships a top-level `segment_reconciliation` block that
// partitions the window's cost three ways, exhaustively and disjointly:
//
//	outcome_linked_cost_micro + no_outcome_cost_micro + unattributed_cost_micro
//	    == window_cost_micro
//
// 🔴 WHAT THESE TESTS EXIST TO PREVENT, beyond "the numbers are right":
//
//   - The REPO dimension had zero coverage in the original segmentation tests — every
//     row there was repo `unqualified`, so a partition that ignored repo entirely (and
//     thereby reintroduced the #231 cross-repo cost fusion) passed the whole suite.
//     TestSegmentReconciliation_QualifiedCostDoesNotCrossRepos and
//     TestSegmentReconciliation_RepoBlindCostJoinsQualifiedOutcome are the two control
//     arms for that, one per direction of the tolerant rule.
//   - The repo rule inside the reconciliation is a RESTATEMENT of store.RepoMatch.
//     TestSegmentReconciliation_RepoJoinMatchesRepoMatch drives the real predicate and
//     store.RepoMatch over the full repo cross-product rather than a hand-built copy;
//     restating a rule in a test asserts nothing about the code.
//   - `no_outcome` and `unattributed` are DIFFERENT things and must never be merged:
//     one is cost tied to a real issue that produced nothing, the other is cost that
//     could not be tied to an issue at all.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// --- helpers ---

// assertPartition asserts the reconciliation invariant on one row, in EXACT integer
// micro-dollars with no tolerance. The dollar fields are deliberately not used: each
// goes through an independent float64 division, so `a+b+c == d` on them is not a
// property the server promises (see TestSegmentReconciliation_DollarsAreNotExact).
func assertPartition(t *testing.T, label string, row developerCostReconciliationJSON) {
	t.Helper()
	sum := row.OutcomeLinkedCostMicro + row.NoOutcomeCostMicro + row.UnattributedCostMicro
	if sum != row.WindowCostMicro {
		t.Errorf("%s: partition broken — outcome_linked(%d) + no_outcome(%d) + unattributed(%d) "+
			"= %d, want window_cost_micro = %d",
			label, row.OutcomeLinkedCostMicro, row.NoOutcomeCostMicro,
			row.UnattributedCostMicro, sum, row.WindowCostMicro)
	}
	for name, v := range map[string]int64{
		"window":         row.WindowCostMicro,
		"outcome_linked": row.OutcomeLinkedCostMicro,
		"no_outcome":     row.NoOutcomeCostMicro,
		"unattributed":   row.UnattributedCostMicro,
	} {
		if v < 0 {
			t.Errorf("%s: %s_cost_micro = %d, must never be negative", label, name, v)
		}
	}
}

// reconRow returns the named developer's reconciliation row, failing if absent.
func reconRow(t *testing.T, resp scoresResponse, dev string) developerCostReconciliationJSON {
	t.Helper()
	if resp.SegmentReconciliation == nil {
		t.Fatalf("no segment_reconciliation block on the response")
	}
	for _, r := range resp.SegmentReconciliation.Developers {
		if r.Developer == dev {
			return r
		}
	}
	t.Fatalf("no segment_reconciliation row for %q; got %d rows: %+v",
		dev, len(resp.SegmentReconciliation.Developers), resp.SegmentReconciliation.Developers)
	return developerCostReconciliationJSON{}
}

func micro(usd float64) int64 { return store.DollarsToMicro(usd) }

// discardLogger is the quiet logger the direct-builder tests pass; the overflow arm
// deliberately logs an error and that output is noise in a passing run.
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- the defect this issue was filed on ---

// TestSegmentReconciliation_PartitionsWindowCost is the regression the issue asked
// for: a developer with spend on an issue that produced NO outcome. Before #466 that
// spend appeared in no segment and nowhere else in the response — it was simply gone.
// Now it is reported, in its own bucket, and the three parts add up to the window.
func TestSegmentReconciliation_PartitionsWindowCost(t *testing.T) {
	h, db := newTestHandler(t)
	// alice: $0.10 of feature work that SHIPPED, $0.25 on an issue that produced
	// nothing (the thrash the segments cannot see), and $0.05 the collector could not
	// tie to any issue at all.
	seedTypedOutcome(t, db, "alice", "issue-shipped", 5, 0.10, store.WorkTypeFeature)
	seedCosts(t, db, "alice", "issue-abandoned", 0.25)
	seedCosts(t, db, "alice", store.UnattributedMainBucket, 0.05)
	// bob: entirely outcome-linked, so the two developers are not the same shape.
	seedTypedOutcome(t, db, "bob", "issue-bob", 3, 0.20, store.WorkTypeBug)

	resp := getScores(t, h, "/api/v1/scores")

	alice := reconRow(t, resp, "alice")
	assertPartition(t, "alice", alice)
	if got, want := alice.WindowCostMicro, micro(0.40); got != want {
		t.Errorf("alice window_cost_micro = %d, want %d ($0.10 + $0.25 + $0.05)", got, want)
	}
	if got, want := alice.OutcomeLinkedCostMicro, micro(0.10); got != want {
		t.Errorf("alice outcome_linked_cost_micro = %d, want %d", got, want)
	}
	// 🔴 THE DEFECT. Before #466 this $0.25 was in no segment and in no other field.
	if got, want := alice.NoOutcomeCostMicro, micro(0.25); got != want {
		t.Errorf("alice no_outcome_cost_micro = %d, want %d — this is the spend the "+
			"work-type segments structurally cannot categorize (#466)", got, want)
	}
	// 🔴 DISJOINT FROM no_outcome, and never merged with it: this dollar has no issue
	// id at all, the one above has a real one that produced nothing.
	if got, want := alice.UnattributedCostMicro, micro(0.05); got != want {
		t.Errorf("alice unattributed_cost_micro = %d, want %d", got, want)
	}

	bob := reconRow(t, resp, "bob")
	assertPartition(t, "bob", bob)
	if bob.NoOutcomeCostMicro != 0 || bob.UnattributedCostMicro != 0 {
		t.Errorf("bob is entirely outcome-linked; got no_outcome=%d unattributed=%d",
			bob.NoOutcomeCostMicro, bob.UnattributedCostMicro)
	}

	// The name-free rollup is the sum of the rows, in integers.
	total := resp.SegmentReconciliation.Total
	assertPartition(t, "total", total)
	if got, want := total.WindowCostMicro, alice.WindowCostMicro+bob.WindowCostMicro; got != want {
		t.Errorf("total window_cost_micro = %d, want %d (sum of the rows)", got, want)
	}
	if total.Developer != "" {
		t.Errorf("the rollup must carry no developer name; got %q", total.Developer)
	}

	// And the gap is REAL relative to the segments: the segmented view can account for
	// strictly less than the window. That is the whole point of the block, and it is
	// asserted rather than assumed.
	if alice.OutcomeLinkedCostMicro >= alice.WindowCostMicro {
		t.Errorf("fixture does not exercise the defect: alice's segmented cost (%d) is not "+
			"less than her window cost (%d)", alice.OutcomeLinkedCostMicro, alice.WindowCostMicro)
	}
}

// TestSegmentReconciliation_HeadlineDenominatorMatchesWindowCost pins the claim that
// makes the block meaningful: window_cost_micro really IS the denominator of the
// pooled headline score, not some other total. If the two ever diverge, the block
// reconciles against a number the reader is not looking at.
func TestSegmentReconciliation_HeadlineDenominatorMatchesWindowCost(t *testing.T) {
	h, db := newTestHandler(t)
	seedTypedOutcome(t, db, "alice", "issue-shipped", 5, 0.10, store.WorkTypeFeature)
	seedCosts(t, db, "alice", "issue-abandoned", 0.25)
	seedCosts(t, db, "alice", store.UnattributedMainBucket, 0.05)

	resp := getScores(t, h, "/api/v1/scores")
	row := reconRow(t, resp, "alice")

	var headline float64
	var found bool
	for _, d := range resp.Developers {
		if d.Developer == "alice" {
			headline, found = d.TotalCostUSD, true
		}
	}
	if !found {
		t.Fatal("no pooled developer row for alice")
	}
	if got := store.DollarsToMicro(headline); got != row.WindowCostMicro {
		t.Errorf("pooled total_cost_usd = %v ($%d micro) but segment_reconciliation "+
			"window_cost_micro = %d — the block reconciles against a different total "+
			"than the score a reader sees", headline, got, row.WindowCostMicro)
	}
}

// --- the repo dimension: two control arms, one per direction of the tolerant rule ---

// TestSegmentReconciliation_RepoBlindCostJoinsQualifiedOutcome is the SABOTAGE ARM for
// dropping the repo-blind arm of the join. The proxy structurally cannot know a
// repository, so its cost rows carry repoid.Unqualified while the webhook's outcome
// carries a real one. Under store.RepoMatch that pair JOINS. A reconciliation that
// required an exact repo match would file every proxy dollar as "no outcome" and
// invent a thrash signal for a team that shipped everything it started.
func TestSegmentReconciliation_RepoBlindCostJoinsQualifiedOutcome(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	// Cost captured by the proxy: repo-blind.
	seedRepoCostAt(t, db, repoid.Unqualified, "alice", "#42", 0.30, now)
	// Outcome captured by the webhook: repo-QUALIFIED.
	seedRepoOutcomeAt(t, db, "acme/alpha", "alice", "#42", 5, now)

	resp := getScores(t, h, "/api/v1/scores")
	row := reconRow(t, resp, "alice")
	assertPartition(t, "alice", row)
	if got, want := row.OutcomeLinkedCostMicro, micro(0.30); got != want {
		t.Errorf("repo-blind cost did not join the qualified outcome: outcome_linked = %d, want %d",
			got, want)
	}
	if row.NoOutcomeCostMicro != 0 {
		t.Errorf("repo-blind cost was filed as no_outcome (%d micro); the tolerant join "+
			"(store.RepoMatch, #231) matches whenever EITHER side is unqualified — this "+
			"would fabricate a thrash signal for every proxy-captured dollar",
			row.NoOutcomeCostMicro)
	}
}

// TestSegmentReconciliation_QualifiedCostDoesNotCrossRepos is the SABOTAGE ARM for
// ignoring repo entirely — the #231 corruption. Two repos, the SAME issue number
// "#42", one developer. Only acme/alpha shipped an outcome. acme/beta's spend on its
// own, unrelated "#42" must be reported as no-outcome; a partition keyed on
// (developer, issue) alone would fuse the two repos and report it as explained.
func TestSegmentReconciliation_QualifiedCostDoesNotCrossRepos(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	seedRepoCostAt(t, db, "acme/alpha", "alice", "#42", 0.10, now)
	seedRepoCostAt(t, db, "acme/beta", "alice", "#42", 0.70, now)
	// Outcome ONLY in alpha.
	seedRepoOutcomeAt(t, db, "acme/alpha", "alice", "#42", 5, now)

	resp := getScores(t, h, "/api/v1/scores")
	row := reconRow(t, resp, "alice")
	assertPartition(t, "alice", row)
	if got, want := row.OutcomeLinkedCostMicro, micro(0.10); got != want {
		t.Errorf("outcome_linked = %d, want %d (ONLY acme/alpha's spend joins). "+
			"If this is %d the partition ignored repo and re-fused two repositories' "+
			"issue #42 — the #231 corruption",
			got, want, micro(0.80))
	}
	if got, want := row.NoOutcomeCostMicro, micro(0.70); got != want {
		t.Errorf("no_outcome = %d, want %d (acme/beta's #42 shipped nothing)", got, want)
	}
}

// TestSegmentReconciliation_RepoJoinMatchesRepoMatch drives THE REAL predicate —
// buildOutcomeCoverage + outcomeCoverage.linked, the exact functions
// buildSegmentReconciliation calls — over the full repo cross-product and requires it
// to agree with store.RepoMatch, the rule store.JoinIndex.Sum uses to charge a cost row
// into a work-type segment.
//
// This is not a restatement check. A test that rebuilt the rule and compared its own
// copy to itself would pass with the production code arbitrarily broken; the assertion
// here is between the shipped predicate and the shipped RepoMatch.
func TestSegmentReconciliation_RepoJoinMatchesRepoMatch(t *testing.T) {
	repos := []string{"acme/alpha", "acme/beta", repoid.Unqualified, ""}
	identity := func(s string) string { return s }

	for _, outcomeRepo := range repos {
		for _, costRepo := range repos {
			name := fmt.Sprintf("outcome=%s/cost=%s", orBlank(outcomeRepo), orBlank(costRepo))
			t.Run(name, func(t *testing.T) {
				// One outcome, in outcomeRepo. Built through the real indexer.
				cover := buildOutcomeCoverage([]store.Outcome{{
					Developer: "alice", IssueID: "#42", Repo: outcomeRepo,
				}}, identity)

				got := cover.linked("alice", costRepo, "#42")
				want := store.RepoMatch(outcomeRepo, costRepo)
				if got != want {
					t.Errorf("outcomeCoverage.linked(alice, %q, #42) = %v, but "+
						"store.RepoMatch(%q, %q) = %v — the reconciliation and the "+
						"segments would charge this cost row differently (#466/#231)",
						costRepo, got, outcomeRepo, costRepo, want)
				}
			})
		}
	}

	// Non-vacuity: RepoMatch must have produced BOTH answers across the grid, or the
	// agreement above is satisfied by a predicate that always returns the same thing.
	var trues, falses int
	for _, a := range repos {
		for _, b := range repos {
			if store.RepoMatch(a, b) {
				trues++
			} else {
				falses++
			}
		}
	}
	if trues == 0 || falses == 0 {
		t.Fatalf("vacuous cross-product: RepoMatch returned %d true / %d false", trues, falses)
	}

	// An issue with NO outcome at all is never linked, in any repo.
	empty := buildOutcomeCoverage(nil, identity)
	for _, r := range repos {
		if empty.linked("alice", r, "#42") {
			t.Errorf("linked(alice, %q, #42) = true with no outcomes indexed", r)
		}
	}
}

func orBlank(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}

// --- scope: the block describes the WINDOW, not the requested segment ---

// TestSegmentReconciliation_NotFilteredByWorkType pins that ?work_type does not narrow
// the reconciliation. Narrowing the outcome set to the requested type would report
// every OTHER type's spend as "no outcome" — manufacturing a gap that does not exist,
// in the exact field a reader consults to size the gap.
func TestSegmentReconciliation_NotFilteredByWorkType(t *testing.T) {
	h, db := newTestHandler(t)
	seedTypedOutcome(t, db, "alice", "issue-feat", 5, 0.20, store.WorkTypeFeature)
	seedTypedOutcome(t, db, "alice", "issue-sec", 3, 0.30, store.WorkTypeSecurity)
	seedCosts(t, db, "alice", "issue-abandoned", 0.50)

	unfiltered := reconRow(t, getScores(t, h, "/api/v1/scores"), "alice")
	filtered := reconRow(t, getScores(t, h, "/api/v1/scores?work_type=feature"), "alice")

	if filtered != unfiltered {
		t.Errorf("?work_type=feature changed the reconciliation:\n got %+v\nwant %+v\n"+
			"the gap is a property of the developer's WINDOW, not of the segment the "+
			"caller asked for", filtered, unfiltered)
	}
	// And it is the honest number: only the abandoned issue is unexplained, even
	// though the filtered response shows just one of the two segments.
	if got, want := filtered.NoOutcomeCostMicro, micro(0.50); got != want {
		t.Errorf("no_outcome under ?work_type=feature = %d, want %d. %d would mean the "+
			"security spend was reclassified as unexplained",
			got, want, micro(0.80))
	}
	assertPartition(t, "alice (?work_type=feature)", filtered)
}

// TestSegmentReconciliation_AliasResolvesToOneRow pins the #125 canonicalization: a
// cost row written under an OS username and an outcome written under a GitHub login
// must collapse to one identity. Without it, every aliased developer's spend reports
// as no-outcome — the block would swallow precisely the spend it exists to explain.
func TestSegmentReconciliation_AliasResolvesToOneRow(t *testing.T) {
	h, db := newTestHandler(t)
	if err := db.UpsertDeveloperAlias(context.Background(), "dl", "devlead"); err != nil {
		t.Fatalf("UpsertAlias: %v", err)
	}
	seedCosts(t, db, "dl", "#7", 0.40)
	if _, err := db.InsertOutcome(context.Background(), store.Outcome{
		Developer: "devlead", IssueID: "#7", Weight: 5, Quality: 1.0,
		MergeCommitSHA: "sha-alias-7", Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}

	resp := getScores(t, h, "/api/v1/scores")
	if n := len(resp.SegmentReconciliation.Developers); n != 1 {
		t.Fatalf("alias did not collapse: %d reconciliation rows, want 1: %+v",
			n, resp.SegmentReconciliation.Developers)
	}
	row := resp.SegmentReconciliation.Developers[0]
	assertPartition(t, "aliased developer", row)
	if row.NoOutcomeCostMicro != 0 {
		t.Errorf("aliased spend reported as no_outcome (%d micro); the cost row and the "+
			"outcome are the same person (#125)", row.NoOutcomeCostMicro)
	}
	if got, want := row.OutcomeLinkedCostMicro, micro(0.40); got != want {
		t.Errorf("outcome_linked = %d, want %d", got, want)
	}
}

// --- privacy: the block is a whole-window cost figure and travels with the others ---

// TestSegmentReconciliation_AnonymizedDropsDeveloperRows pins that an anonymized mode
// (#185/#270) ships the name-free rollup and NO named rows. A per-developer cost row
// under a k-anonymity floor would defeat the floor outright.
func TestSegmentReconciliation_AnonymizedDropsDeveloperRows(t *testing.T) {
	// Both anonymized levels, not just team: the suppression is gated on
	// aggregation.Anonymized(), so a mode added or reclassified later must be covered
	// by construction rather than by remembering to add a case.
	for _, mode := range []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision} {
		t.Run(mode.String(), func(t *testing.T) {
			assertAnonymizedReconciliation(t, mode)
		})
	}
}

func assertAnonymizedReconciliation(t *testing.T, mode scoring.AggregationMode) {
	t.Helper()
	h, db := newTestHandler(t)
	h.SetAggregation(mode, 3)
	for i, m := range []string{"alice", "bob", "carol"} {
		seedTeamMember(t, db, m, "issue-"+m, "eng", 10, float64(i+3))
		seedCosts(t, db, m, "issue-abandoned-"+m, 5)
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores: %d body=%s", code, body)
	}
	var resp scoresResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.SegmentReconciliation == nil {
		t.Fatalf("%s mode dropped the whole block; the name-free rollup is safe and "+
			"should still ship", mode)
	}
	if n := len(resp.SegmentReconciliation.Developers); n != 0 {
		t.Errorf("%s mode emitted %d NAMED reconciliation rows: %+v", mode,
			n, resp.SegmentReconciliation.Developers)
	}
	assertPartition(t, mode.String()+"-mode total", resp.SegmentReconciliation.Total)
	if resp.SegmentReconciliation.Total.WindowCostMicro == 0 {
		t.Errorf("%s-mode rollup is zero; the fixture spent money", mode)
	}
	// Assert WHERE the abandoned spend landed, not merely that the rollup is non-zero:
	// a rollup that filed all $15 as outcome_linked would satisfy a non-zero check
	// while erasing the very gap this block exists to report, in the mode where a
	// reader has no per-developer row to cross-check against.
	if got, want := resp.SegmentReconciliation.Total.NoOutcomeCostMicro, micro(15); got != want {
		t.Errorf("%s-mode no_outcome = %d, want %d ($5 abandoned x 3 developers)", mode, got, want)
	}
	// Belt and braces: no developer name anywhere in the serialized block.
	blk, err := json.Marshal(resp.SegmentReconciliation)
	if err != nil {
		t.Fatalf("marshal block: %v", err)
	}
	for _, m := range []string{"alice", "bob", "carol"} {
		if strings.Contains(string(blk), m) {
			t.Errorf("%s-mode segment_reconciliation leaked %q: %s", mode, m, blk)
		}
	}
}

// TestSegmentReconciliation_WithheldWhenKAnonSuppresses pins that the block goes with
// cost_composition under the #593 strip pass, and declares that it did.
//
// 🔴 IT HAS TO. segment_reconciliation.total.window_cost_micro is the window's WHOLE
// cost — the same quantity as cost_composition.total_cost_usd, folded from the same
// token_events rows. Publishing it while cost_composition is withheld hands back the
// exact figure the suppression just removed, in a field the pass had not been taught
// about.
func TestSegmentReconciliation_WithheldWhenKAnonSuppresses(t *testing.T) {
	h, db := newTeamModeHandler(t, 3)
	// ONE developer in the window: every cohort is sub-k, so the residual floor fires.
	seedTeamMember(t, db, "alice", "issue-a", "eng", 12, 5)

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores: %d body=%s", code, body)
	}
	var resp scoresResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Precondition: the suppression actually fired. Without this the test passes
	// vacuously on any response that simply had no block to withhold.
	if resp.DataQuality == nil || resp.DataQuality.KAnonSuppressed == nil {
		t.Fatalf("k-anon suppression did not fire; fixture no longer reproduces (#593). body=%s", body)
	}
	if resp.CostComposition != nil {
		t.Fatalf("cost_composition was not withheld; this test's premise is gone")
	}

	if resp.SegmentReconciliation != nil {
		t.Errorf("segment_reconciliation survived a k-anon suppression that withheld "+
			"cost_composition: window_cost_micro = %d republishes the withheld whole-window "+
			"total exactly (#593). block = %+v",
			resp.SegmentReconciliation.Total.WindowCostMicro, resp.SegmentReconciliation)
	}
	if !resp.DataQuality.KAnonSuppressed.WithheldSegmentReconciliation {
		t.Error("withheld_segment_reconciliation = false; absent must never be " +
			"confusable with \"the window had no spend\"")
	}
	// The raw body must not carry the key at all — a `"segment_reconciliation": null`
	// would still be a shape change but no leak; assert the omitempty path holds.
	if strings.Contains(string(body), `"segment_reconciliation"`) {
		t.Errorf("suppressed response still carries a segment_reconciliation key: %s", body)
	}
}

// --- the wire contract ---

// TestSegmentReconciliation_OmittedWhenWindowHasNoCostRows pins the omit-when-empty
// discipline: a window with no token_events ships no key at all.
func TestSegmentReconciliation_OmittedWhenWindowHasNoCostRows(t *testing.T) {
	h, db := newTestHandler(t)
	// An outcome with NO token events: outcomes exist, cost rows do not.
	if _, err := db.InsertOutcome(context.Background(), store.Outcome{
		Developer: "alice", IssueID: "#1", Weight: 5, Quality: 1.0,
		MergeCommitSHA: "sha-empty", Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}
	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores: %d body=%s", code, body)
	}
	if strings.Contains(string(body), `"segment_reconciliation"`) {
		t.Errorf("a window with no cost rows shipped a segment_reconciliation key: %s", body)
	}
}

// TestSegmentReconciliation_MicroAndUSDDescribeTheSameNumber pins that the integer and
// dollar companions are two renderings of ONE value, not two independently computed
// figures that could disagree about which quantity they describe.
func TestSegmentReconciliation_MicroAndUSDDescribeTheSameNumber(t *testing.T) {
	h, db := newTestHandler(t)
	seedTypedOutcome(t, db, "alice", "issue-shipped", 5, 0.10, store.WorkTypeFeature)
	seedCosts(t, db, "alice", "issue-abandoned", 0.25)
	seedCosts(t, db, "alice", store.UnattributedMainBucket, 0.05)

	resp := getScores(t, h, "/api/v1/scores")
	for _, row := range append([]developerCostReconciliationJSON{resp.SegmentReconciliation.Total},
		resp.SegmentReconciliation.Developers...) {
		pairs := []struct {
			name  string
			micro int64
			usd   float64
		}{
			{"window", row.WindowCostMicro, row.WindowCostUSD},
			{"outcome_linked", row.OutcomeLinkedCostMicro, row.OutcomeLinkedCostUSD},
			{"no_outcome", row.NoOutcomeCostMicro, row.NoOutcomeCostUSD},
			{"unattributed", row.UnattributedCostMicro, row.UnattributedCostUSD},
		}
		for _, p := range pairs {
			if want := store.MicroToDollars(p.micro); math.Abs(p.usd-want) > 1e-12 {
				t.Errorf("%s %s: usd = %v but micro = %d renders as %v",
					orBlank(row.Developer), p.name, p.usd, p.micro, want)
			}
		}
	}
}

// TestSegmentReconciliation_DollarsAreNotExact is the reason the integer companions
// are on the wire at all, asserted rather than claimed. Each dollar figure is an
// independent float64 division, and float division does not distribute over addition,
// so a JS-style consumer evaluating `a + b + c === d` on the dollar fields gets false
// for a substantial fraction of realistic inputs — while the SAME assertion on the
// micro fields is exact for every one of them.
//
// The rate is measured here, not quoted, so it cannot rot into folklore.
func TestSegmentReconciliation_DollarsAreNotExact(t *testing.T) {
	// A deterministic sweep of realistic micro-dollar triples (fractions of a cent up
	// to tens of dollars — the grain a single developer's window actually lands on).
	var trials, floatViolations int
	for a := int64(1); a <= 60; a++ {
		for b := int64(1); b <= 60; b++ {
			for c := int64(1); c <= 60; c++ {
				la, lb, lc := a*7919, b*104_729, c*1_299_709
				window := la + lb + lc
				trials++
				// Integer arithmetic: exact, always. This is what the block promises.
				if la+lb+lc != window {
					t.Fatalf("integer identity broken at (%d,%d,%d)", la, lb, lc)
				}
				// Float arithmetic: what a consumer reading the *_usd fields would do.
				if store.MicroToDollars(la)+store.MicroToDollars(lb)+store.MicroToDollars(lc) !=
					store.MicroToDollars(window) {
					floatViolations++
				}
			}
		}
	}
	if floatViolations == 0 {
		t.Fatal("no float violations across the sweep — the rationale for shipping the " +
			"integer companions no longer reproduces; re-measure before deleting them")
	}
	t.Logf("exact `a+b+c == d` on the _usd fields fails for %d/%d triples (%.1f%%); "+
		"on the _cost_micro fields it fails for 0",
		floatViolations, trials, 100*float64(floatViolations)/float64(trials))
}

// --- concurrency: why window_cost is folded from one snapshot ---

// TestSegmentReconciliation_InvariantHoldsUnderConcurrentWrites is the measurement
// behind the design decision, and the guard against reverting it.
//
// The response is assembled from several non-transactional reads. If window_cost were
// anchored on DeveloperCostsWindow while the three parts were folded from
// DeveloperIssueCostsWindow — two different queries, several statements apart, over a
// window whose upper bound is open — a row written between them would land in one and
// not the other, and the PUBLISHED invariant would fail. Not as corruption: as ordinary
// concurrency the consumer cannot distinguish from corruption, in the exact fields the
// documentation tells it to assert on.
//
// Folding all four figures from ONE snapshot makes the invariant an arithmetic
// identity no writer can break. This test hammers /scores against a live writer and
// requires ZERO violations. It FAILS if window_cost is moved back onto
// DeveloperCostsWindow.
func TestSegmentReconciliation_InvariantHoldsUnderConcurrentWrites(t *testing.T) {
	h, db := newTestHandler(t)
	seedTypedOutcome(t, db, "alice", "issue-shipped", 5, 0.10, store.WorkTypeFeature)

	// 60, not 400. Measured: re-anchoring window_cost on a second DeveloperCostsWindow
	// read violates the invariant on 100% of responses, so the mutation dies in the
	// first handful of iterations — while 400 requests cost ~118s under -race, which
	// `make check` runs, and again in check-full. Buying nothing twice.
	const requests = 60
	stop := make(chan struct{})
	// 🔴 writes PROVES THE WRITER IS ALIVE, and without it this test is a tautology.
	// The goroutine below discards its error, and the row-count guard counts
	// RECONCILIATION rows, which the seeded fixture supplies on its own — so replacing
	// the writer body with `continue` left this test PASSING (measured: 800 rows
	// checked, 0 violations; the only tell was runtime dropping 4.0s to 0.13s, which
	// nothing asserted). A future required TokenEvent field or a tightened ingest rule
	// would silently convert the repo's flagship concurrency test into `a+b+c==d` over
	// a static fixture.
	var writes atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			// Alternate the issue so BOTH the outcome-linked and the no-outcome arms
			// move under the reader — a writer that only grew one bucket could leave a
			// broken partition looking self-consistent.
			issue := "issue-shipped"
			if i%2 == 1 {
				issue = fmt.Sprintf("issue-churn-%d", i)
			}
			if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
				Developer: "alice", IssueID: issue, Model: "claude-sonnet-4",
				InputTok: 2000, CostMicro: 1_000, Source: "jsonl",
				Fidelity: "realtime", Timestamp: time.Now().UTC(),
			}); err == nil {
				writes.Add(1)
			}
		}
	}()
	t.Cleanup(func() { close(stop); wg.Wait() })

	var violations, checked int
	for i := 0; i < requests; i++ {
		code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
		if code != http.StatusOK {
			t.Fatalf("GET /scores: %d body=%s", code, body)
		}
		var resp scoresResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.SegmentReconciliation == nil {
			continue
		}
		for _, row := range append([]developerCostReconciliationJSON{resp.SegmentReconciliation.Total},
			resp.SegmentReconciliation.Developers...) {
			checked++
			if row.OutcomeLinkedCostMicro+row.NoOutcomeCostMicro+row.UnattributedCostMicro !=
				row.WindowCostMicro {
				violations++
			}
		}
	}
	if checked == 0 {
		t.Fatal("vacuous: no reconciliation rows were checked")
	}
	// The concurrency half must be real. This is the assertion whose absence let the
	// whole test pass against a writer that did nothing.
	if n := writes.Load(); n == 0 {
		t.Fatal("vacuous: the concurrent writer landed ZERO rows, so this test asserted " +
			"the partition over a STATIC fixture — which is an arithmetic identity and " +
			"proves nothing about the race this test exists to rule out")
	}
	if violations != 0 {
		t.Errorf("the published partition broke %d times in %d rows under a concurrent "+
			"writer. window_cost_micro must be folded from the SAME "+
			"DeveloperIssueCostsWindow snapshot as the three parts, never read from "+
			"DeveloperCostsWindow (#466)", violations, checked)
	}
	t.Logf("%d rows checked against %d concurrent writes, %d violations",
		checked, writes.Load(), violations)
}

// --- overflow ---

// TestAddSaturating pins the accumulator's clamp AND its saturation report. The
// reconciliation sums cost_micro across every row in a window; each row is bounded at
// ingest but the SUM is not, and a bare `a + b` would wrap silently and
// self-concealingly — all four accumulators wrap together, so the partition still
// holds and a consumer following the documented advice sees a perfectly consistent
// reconciliation built on wrapped values. Clamping fixes the wrap but HIDES THE EVENT:
// the clamped totals are non-negative and self-consistent too. Hence the second return
// value, which is the only place saturation is observable.
func TestAddSaturating(t *testing.T) {
	cases := []struct {
		a, b, want int64
		wantSat    bool
	}{
		{0, 0, 0, false},
		{1, 2, 3, false},
		{math.MaxInt64, 0, math.MaxInt64, false},
		{math.MaxInt64, 1, math.MaxInt64, true},
		{math.MaxInt64, math.MaxInt64, math.MaxInt64, true},
		{math.MaxInt64 - 5, 10, math.MaxInt64, true},
		{math.MinInt64, -1, math.MinInt64, true},
		// 🔴 MinInt64 + MinInt64 wraps to exactly 0, so an underflow test written as
		// `sum > 0` misses it. This row is the regression for that off-by-one.
		{math.MinInt64, math.MinInt64, math.MinInt64, true},
		{math.MaxInt64, math.MinInt64, -1, false}, // mixed signs cannot overflow
	}
	for _, c := range cases {
		got, gotSat := addSaturating(c.a, c.b)
		if got != c.want {
			t.Errorf("addSaturating(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
		if gotSat != c.wantSat {
			t.Errorf("addSaturating(%d, %d) saturated = %v, want %v — the caller CANNOT "+
				"re-derive this from the result, so a missed report is a silently "+
				"published non-measurement", c.a, c.b, gotSat, c.wantSat)
		}
	}
}

// TestSegmentReconciliation_SuppressedOnOverflow pins the SATURATION arm of the
// fitness tripwire. The fixture is MaxInt64 + MaxInt64, which CLAMPS — so the published
// figures stay non-negative, mutually consistent, and completely meaningless (~$9.2e12).
// That is exactly why saturation has to be reported at the add: no property of the
// final numbers can reveal it, which is what made the original negativity-only check
// unreachable by construction. Driven through the real builder.
func TestSegmentReconciliation_SuppressedOnOverflow(t *testing.T) {
	identity := func(s string) string { return s }
	rows := []store.DevIssueCost{
		{Developer: "alice", Repo: repoid.Unqualified, IssueID: "#1", TotalCostMicro: math.MaxInt64},
		{Developer: "bob", Repo: repoid.Unqualified, IssueID: "#2", TotalCostMicro: math.MaxInt64},
	}
	got := buildSegmentReconciliation(nil, identity, rows, false, discardLogger())
	if got != nil {
		t.Errorf("overflowing window still published a block: %+v", got)
	}

	// Control arm: the SAME shape without the overflow must publish, so the test above
	// is not passing because buildSegmentReconciliation refuses this fixture generally.
	rows[0].TotalCostMicro, rows[1].TotalCostMicro = 5, 7
	ok := buildSegmentReconciliation(nil, identity, rows, false, discardLogger())
	if ok == nil {
		t.Fatal("non-overflowing control arm also returned nil; the overflow assertion above is vacuous")
	}
	if ok.Total.WindowCostMicro != 12 {
		t.Errorf("control arm total = %d, want 12", ok.Total.WindowCostMicro)
	}
	// With no outcomes at all, every dollar is no-outcome — not unattributed, and not
	// outcome-linked.
	if ok.Total.NoOutcomeCostMicro != 12 {
		t.Errorf("control arm no_outcome = %d, want 12 (real issue ids, no outcomes)",
			ok.Total.NoOutcomeCostMicro)
	}
}

// TestSegmentReconciliation_SuppressedOnNegativePerDeveloperRow pins the OTHER arm of
// the fitness tripwire, which had no test at all and whose doc claimed more than the
// code delivered.
//
// 🔴 THE ROLLUP IS NOT ENOUGH. A negative cost row for one developer nets against every
// other developer's positive spend, so a rollup-only check fires only if the fault
// drags the WHOLE FLEET negative — while the affected developer's own published row
// ships a negative window_cost_micro with the partition invariant perfectly intact.
// The fixture below is exactly that shape: alice is negative, bob more than covers her,
// and the rollup stays healthy.
func TestSegmentReconciliation_SuppressedOnNegativePerDeveloperRow(t *testing.T) {
	identity := func(s string) string { return s }
	rows := []store.DevIssueCost{
		{Developer: "alice", Repo: repoid.Unqualified, IssueID: "#1", TotalCostMicro: -500},
		{Developer: "bob", Repo: repoid.Unqualified, IssueID: "#2", TotalCostMicro: 10_000},
	}
	if got := buildSegmentReconciliation(nil, identity, rows, false, discardLogger()); got != nil {
		t.Errorf("a negative per-developer row was published; the rollup (%d micro) is "+
			"positive, so a rollup-only check cannot see it: %+v",
			got.Total.WindowCostMicro, got.Developers)
	}

	// Control arm: the SAME shape, all non-negative, must publish — otherwise the
	// assertion above passes because the builder refuses this fixture generally.
	rows[0].TotalCostMicro = 500
	ok := buildSegmentReconciliation(nil, identity, rows, false, discardLogger())
	if ok == nil {
		t.Fatal("non-negative control arm also returned nil; the assertion above is vacuous")
	}
	if ok.Total.WindowCostMicro != 10_500 {
		t.Errorf("control arm total = %d, want 10500", ok.Total.WindowCostMicro)
	}
}

// TestSegmentReconciliation_NilLoggerDoesNotPanic pins the guard on a package-level
// function that takes its logger as a parameter: a caller can pass nil where the
// Handler's constructor would have defaulted it, and dying on the /scores path because
// a diagnostic sink was unset would be far worse than a missing log line.
func TestSegmentReconciliation_NilLoggerDoesNotPanic(t *testing.T) {
	identity := func(s string) string { return s }
	rows := []store.DevIssueCost{
		{Developer: "alice", Repo: repoid.Unqualified, IssueID: "#1", TotalCostMicro: 500},
	}
	got := buildSegmentReconciliation(nil, identity, rows, false, nil)
	if got == nil || got.Total.WindowCostMicro != 500 {
		t.Fatalf("nil logger changed the result: %+v", got)
	}
	// And on the path that ACTUALLY logs — the guard is worthless if it only covers
	// the branch that never reaches the logger.
	rows[0].TotalCostMicro = -1
	if bad := buildSegmentReconciliation(nil, identity, rows, false, nil); bad != nil {
		t.Errorf("nil logger skipped the fitness tripwire: %+v", bad)
	}
}

// TestSegmentReconciliation_SentinelBeatsAForgedOutcome pins the arm ordering. The
// ingest guards (validateIssueID on /costs and /outcomes, the proxy header guard)
// stop a sentinel outcome being written, but rows that predate them or arrive out of
// band must not inflate the spend the segments appear to explain. Sentinel-first can
// only move cost OUT of outcome_linked, never into it.
func TestSegmentReconciliation_SentinelBeatsAForgedOutcome(t *testing.T) {
	identity := func(s string) string { return s }
	// An outcome sitting on the sentinel — the shape ingest now refuses.
	outcomes := []store.Outcome{{
		Developer: "alice", IssueID: store.UnattributedMainBucket, Repo: repoid.Unqualified,
	}}
	rows := []store.DevIssueCost{{
		Developer: "alice", Repo: repoid.Unqualified,
		IssueID: store.UnattributedMainBucket, TotalCostMicro: 900,
	}}
	got := buildSegmentReconciliation(outcomes, identity, rows, false, discardLogger())
	if got == nil {
		t.Fatal("no block")
	}
	if got.Total.UnattributedCostMicro != 900 {
		t.Errorf("unattributed = %d, want 900; a legacy or forged sentinel outcome must "+
			"not reclassify exploration spend as explained",
			got.Total.UnattributedCostMicro)
	}
	if got.Total.OutcomeLinkedCostMicro != 0 {
		t.Errorf("outcome_linked = %d, want 0", got.Total.OutcomeLinkedCostMicro)
	}
	assertPartition(t, "sentinel-with-outcome", got.Total)
}
