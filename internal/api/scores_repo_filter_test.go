package api

// Tests for the #590 repository filter on GET /api/v1/scores and the per-developer
// detail, and for the strict unknown-query-parameter posture that makes the filter
// trustworthy.
//
// 🔴 WHY THE CONTROL ARM IS THE POINT OF THIS FILE.
//
// The original defect was not "there is no repo filter". It was that `?repo=` was
// SILENTLY IGNORED: net/http drops unrecognized parameters, so a caller who believed
// they had scoped a query received a WHOLE-INSTALL AGGREGATE that was byte-identical
// to a correctly scoped response. Under this project's standing rule that fleet
// absolutes are embargoed, that is the precise shape that leaks — someone asks for one
// repository, is handed the fleet total, and publishes it as a per-repo figure.
//
// A test that only asserts "scoped response returns 200 and has some numbers in it"
// would have PASSED against the broken server. So every test here that matters is
// built around a MULTI-REPO fixture in which the scoped answer and the unscoped answer
// MUST differ, and asserts the difference. If a store read is ever added to the scores
// path and not threaded with the scope, the fleet rows leak back into some response
// field and these assertions fail — which is why they assert on RESPONSE FIELDS rather
// than on the plumbing.
//
// Steve ruled the unqualified semantics (option C) on 2026-08-03: strict equality, plus
// an explicit disclosure of what the strictness excluded. Both halves are asserted.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/repoid"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

const (
	repoAlpha = "acme/alpha"
	repoBeta  = "acme/beta"
)

// seedRepoCost inserts a cost-bearing token_event attributed to a specific repository.
// Tokens are well above scoring.MinAttributableTokens so a cost-bearing issue does not
// also trip the #136 zero-token tripwire (mirrors seedCosts' reasoning).
func seedRepoCost(t *testing.T, db *store.DB, repo, dev, issue string, costUSD float64) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: dev,
		IssueID:   issue,
		Repo:      repo,
		Model:     "claude-sonnet-4",
		InputTok:  2000,
		CostMicro: store.DollarsToMicro(costUSD),
		Source:    "jsonl",
		Fidelity:  "realtime",
		Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertTokenEvent(repo=%s, %s, %s): %v", repo, dev, issue, err)
	}
}

// seedRepoOutcome inserts a merged outcome attributed to a specific repository.
func seedRepoOutcome(t *testing.T, db *store.DB, repo, dev, issue string, weight float64) {
	t.Helper()
	if _, err := db.InsertOutcome(context.Background(), store.Outcome{
		Developer:      dev,
		IssueID:        issue,
		Repo:           repo,
		Weight:         weight,
		Quality:        1.0,
		MergeCommitSHA: "sha-" + repo + "-" + dev + "-" + issue,
		Timestamp:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertOutcome(repo=%s, %s, %s): %v", repo, dev, issue, err)
	}
}

// seedTwoRepos builds the fixture every control arm in this file depends on: the SAME
// developer doing work in TWO repositories, with deliberately different magnitudes so
// no assertion can pass by coincidence.
//
// alpha: 2 outcomes, $3.00 total.  beta: 1 outcome, $10.00.
//
// The developer is shared on purpose. A fixture using a different developer per repo
// would let a filter that merely drops ROWS look correct while a filter that must also
// re-derive per-developer aggregates is broken — the shared identity forces the scoped
// read to recompute the developer's totals, not just omit a name.
func seedTwoRepos(t *testing.T, db *store.DB) {
	t.Helper()
	seedRepoCost(t, db, repoAlpha, "alice", "A-1", 1.00)
	seedRepoOutcome(t, db, repoAlpha, "alice", "A-1", 1.0)
	seedRepoCost(t, db, repoAlpha, "alice", "A-2", 2.00)
	seedRepoOutcome(t, db, repoAlpha, "alice", "A-2", 1.0)

	seedRepoCost(t, db, repoBeta, "alice", "B-1", 10.00)
	seedRepoOutcome(t, db, repoBeta, "alice", "B-1", 3.0)
}

func scoresSince() string { return time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02") }

// TestScoresRepoFilter_ScopedDiffersFromUnscoped is THE control arm — the assertion
// that would have failed against the pre-#590 server and passed against every naive
// "does it return 200" test.
func TestScoresRepoFilter_ScopedDiffersFromUnscoped(t *testing.T) {
	h, db := newTestHandler(t)
	seedTwoRepos(t, db)
	since := scoresSince()

	fleet := getScores(t, h, "/api/v1/scores?since="+since)
	alpha := getScores(t, h, "/api/v1/scores?since="+since+"&repo="+repoAlpha)
	beta := getScores(t, h, "/api/v1/scores?since="+since+"&repo="+repoBeta)

	// The defect, stated as an assertion: a scoped response that EQUALS the fleet
	// response is the bug, whether because the filter is missing or because it was
	// silently dropped.
	if fleet.Total.TotalCostUSD == alpha.Total.TotalCostUSD {
		t.Fatalf("scoped cost equals fleet cost (%.2f) — the repo filter is not being applied; "+
			"this is exactly the #590 defect", alpha.Total.TotalCostUSD)
	}
	if fleet.Total.WeightedPoints == alpha.Total.WeightedPoints {
		t.Fatalf("scoped points equal fleet points (%.2f) — outcomes are not being scoped",
			alpha.Total.WeightedPoints)
	}

	// Exact values, so a filter that is applied but WRONG (e.g. tolerant, or scoping
	// cost but not outcomes) also fails rather than merely differing.
	if got, want := fleet.Total.TotalCostUSD, 13.00; !floatNear(got, want) {
		t.Errorf("fleet cost = %.4f, want %.4f", got, want)
	}
	if got, want := alpha.Total.TotalCostUSD, 3.00; !floatNear(got, want) {
		t.Errorf("alpha cost = %.4f, want %.4f", got, want)
	}
	if got, want := beta.Total.TotalCostUSD, 10.00; !floatNear(got, want) {
		t.Errorf("beta cost = %.4f, want %.4f", got, want)
	}
	// Outcomes must be scoped on the SAME axis as cost. Scoping one and not the other
	// silently corrupts TIER — the metric is a ratio of the two.
	if got, want := alpha.Total.WeightedPoints, 2.0; !floatNear(got, want) {
		t.Errorf("alpha points = %.4f, want %.4f", got, want)
	}
	if got, want := beta.Total.WeightedPoints, 3.0; !floatNear(got, want) {
		t.Errorf("beta points = %.4f, want %.4f", got, want)
	}
	// The two scopes must partition the fleet, not overlap it. If either scope were
	// tolerant (admitting rows it cannot attribute) the parts would exceed the whole.
	if got, want := alpha.Total.TotalCostUSD+beta.Total.TotalCostUSD, fleet.Total.TotalCostUSD; !floatNear(got, want) {
		t.Errorf("alpha+beta = %.4f but fleet = %.4f: the scopes do not partition the window", got, want)
	}
}

// TestScoresRepoFilter_EchoesScopeOnTheWire pins the contract that makes a scoped
// figure auditable: a consumer must be able to tell a scoped response from an unscoped
// one FROM THE RESPONSE, never from the request it sent. Asserting on "I sent ?repo="
// is precisely what failed silently before #590.
func TestScoresRepoFilter_EchoesScopeOnTheWire(t *testing.T) {
	h, db := newTestHandler(t)
	seedTwoRepos(t, db)
	since := scoresSince()

	scoped := getScores(t, h, "/api/v1/scores?since="+since+"&repo="+repoAlpha)
	if scoped.DataQuality == nil {
		t.Fatal("scoped response has no data_quality block; the scope echo is mandatory on a scoped read")
	}
	if got := scoped.DataQuality.RepoScope; got != repoAlpha {
		t.Errorf("data_quality.repo_scope = %q, want %q", got, repoAlpha)
	}

	// The unscoped response must NOT carry the key — its absence is what makes the
	// presence meaningful.
	fleet := getScores(t, h, "/api/v1/scores?since="+since)
	if fleet.DataQuality != nil && fleet.DataQuality.RepoScope != "" {
		t.Errorf("fleet-wide response carries repo_scope = %q, want empty", fleet.DataQuality.RepoScope)
	}
}

// TestScoresRepoFilter_CanonicalizesCase guards the normalizer being wired in. GitHub
// preserves creation case in full_name while treating names case-insensitively, so a
// caller's "Acme/Alpha" and a stored "acme/alpha" are ONE repository. Without
// canonicalization this would silently return an empty scope — a zero-row answer that
// looks like "no data" rather than "you typed it differently".
func TestScoresRepoFilter_CanonicalizesCase(t *testing.T) {
	h, db := newTestHandler(t)
	seedTwoRepos(t, db)
	since := scoresSince()

	mixed := getScores(t, h, "/api/v1/scores?since="+since+"&repo=Acme/Alpha")
	if got, want := mixed.Total.TotalCostUSD, 3.00; !floatNear(got, want) {
		t.Fatalf("mixed-case scope cost = %.4f, want %.4f (canonicalization not applied)", got, want)
	}
	if got := mixed.DataQuality.RepoScope; got != repoAlpha {
		t.Errorf("repo_scope echo = %q, want the CANONICAL %q — the echo must report what was "+
			"actually queried, not what was typed", got, repoAlpha)
	}
}

// TestScoresRepoFilter_UnknownParamsRejected is the other half of the fix. Without it
// the filter is one typo away from silently returning fleet-wide again, with the
// caller's own assertion passing while asserting nothing.
func TestScoresRepoFilter_UnknownParamsRejected(t *testing.T) {
	h, db := newTestHandler(t)
	seedTwoRepos(t, db)
	since := scoresSince()

	// Each of these is a plausible near-miss for `repo`. Every one of them returned a
	// full fleet aggregate with a 200 before #590.
	for _, bad := range []string{"repos", "Repo", "REPO", "repo_id", "repository", "aggregation", "team_id"} {
		t.Run(bad, func(t *testing.T) {
			code, body := doRequest(t, h, http.MethodGet,
				"/api/v1/scores?since="+since+"&"+bad+"="+repoAlpha, nil)
			if code != http.StatusBadRequest {
				t.Fatalf("GET with unknown param %q: status = %d, want 400; body = %s", bad, code, body)
			}
			// The error must NAME the offending parameter — an operator debugging a
			// 400 on a long query string needs to know which one.
			if !strings.Contains(string(body), bad) {
				t.Errorf("400 body does not name the rejected parameter %q: %s", bad, body)
			}
		})
	}
}

// TestScoresRepoFilter_KnownParamsStillAccepted is the negative control for the
// allowlist: a gate that rejects everything would pass the test above while breaking
// the endpoint. Each accepted parameter is exercised, including the legacy `before`
// alias, which is honored by parseWindowUpperBound and would be easy to omit.
func TestScoresRepoFilter_KnownParamsStillAccepted(t *testing.T) {
	h, db := newTestHandler(t)
	seedTwoRepos(t, db)
	since := scoresSince()
	until := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")

	for _, q := range []string{
		"since=" + since,
		"since=" + since + "&until=" + until,
		"since=" + since + "&before=" + until,
		"since=" + since + "&work_type=feature",
		"since=" + since + "&repo=" + repoAlpha,
		"since=" + since + "&until=" + until + "&repo=" + repoAlpha + "&work_type=feature",
	} {
		code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?"+q, nil)
		if code != http.StatusOK {
			t.Errorf("GET /api/v1/scores?%s: status = %d, want 200; body = %s", q, code, body)
		}
	}
}

// TestScoresRepoFilter_InvalidRepoRejected: a slug that cannot canonicalize is a 400,
// not a zero-row result. An empty scoped window and a typo are indistinguishable to the
// caller otherwise, and the typo is far more likely.
func TestScoresRepoFilter_InvalidRepoRejected(t *testing.T) {
	h, db := newTestHandler(t)
	seedTwoRepos(t, db)
	since := scoresSince()

	for _, bad := range []string{
		"alpha",                  // single segment: collides across owners — bug #231
		"https://github.com/a/b", // scheme + host
		repoid.Unqualified,       // the reserved sentinel is not a selectable scope
		strings.Repeat("a", 300), // over repoid.MaxLen
		"/",                      // degenerate
	} {
		t.Run(bad, func(t *testing.T) {
			code, body := doRequest(t, h, http.MethodGet,
				"/api/v1/scores?since="+since+"&repo="+bad, nil)
			if code != http.StatusBadRequest {
				t.Fatalf("repo=%q: status = %d, want 400; body = %s", bad, code, body)
			}
		})
	}
}

// TestScoresRepoFilter_UnqualifiedExcludedAndDisclosed asserts BOTH halves of Steve's
// ruling C on one fixture: the strict scope does not absorb repo-blind spend, AND it
// says so rather than under-counting silently.
func TestScoresRepoFilter_UnqualifiedExcludedAndDisclosed(t *testing.T) {
	h, db := newTestHandler(t)
	seedTwoRepos(t, db)
	// A repo-blind producer (the reverse proxy structurally cannot know a repository).
	seedRepoCost(t, db, repoid.Unqualified, "alice", "U-1", 5.00)
	seedRepoOutcome(t, db, repoid.Unqualified, "alice", "U-1", 1.0)
	since := scoresSince()

	alpha := getScores(t, h, "/api/v1/scores?since="+since+"&repo="+repoAlpha)

	// STRICT: the $5.00 of repo-blind spend must NOT be folded into alpha. A tolerant
	// filter would report $8.00 here and would be over-attributing another
	// repository's spend to the one the caller named — #590's failure mode inverted.
	if got, want := alpha.Total.TotalCostUSD, 3.00; !floatNear(got, want) {
		t.Fatalf("scoped cost = %.4f, want %.4f — repo-blind rows are being absorbed into the "+
			"scope, which over-attributes spend the producer could not place", got, want)
	}

	// DISCLOSED: and the caller must be told what strictness cost them.
	if alpha.DataQuality == nil || alpha.DataQuality.RepoScopeExcluded == nil {
		t.Fatal("scoped response omits repo_scope_excluded despite repo-blind rows in the window; " +
			"a silent under-count is exactly what ruling C rejects")
	}
	ex := alpha.DataQuality.RepoScopeExcluded
	if got, want := ex.CostUSD, 5.00; !floatNear(got, want) {
		t.Errorf("repo_scope_excluded.cost_usd = %.4f, want %.4f", got, want)
	}
	if ex.TokenEvents != 1 {
		t.Errorf("repo_scope_excluded.token_events = %d, want 1", ex.TokenEvents)
	}
	if ex.Outcomes != 1 {
		t.Errorf("repo_scope_excluded.outcomes = %d, want 1", ex.Outcomes)
	}
}

// TestScoresRepoFilter_CleanWindowOmitsExclusion is the control arm for the disclosure:
// if the block were emitted unconditionally, the test above would pass on a broken
// implementation that always reports an exclusion. Absence must mean "nothing was
// excluded, this figure is a true total" — a distinct, load-bearing signal.
func TestScoresRepoFilter_CleanWindowOmitsExclusion(t *testing.T) {
	h, db := newTestHandler(t)
	seedTwoRepos(t, db) // every row names a real repository
	since := scoresSince()

	alpha := getScores(t, h, "/api/v1/scores?since="+since+"&repo="+repoAlpha)
	if alpha.DataQuality == nil {
		t.Fatal("scoped response has no data_quality block")
	}
	if alpha.DataQuality.RepoScopeExcluded != nil {
		t.Errorf("fully-qualified window still reports an exclusion: %+v",
			*alpha.DataQuality.RepoScopeExcluded)
	}
	// The scope echo is still present — it is unconditional on a scoped read.
	if alpha.DataQuality.RepoScope != repoAlpha {
		t.Errorf("repo_scope = %q, want %q", alpha.DataQuality.RepoScope, repoAlpha)
	}
}

// TestScoresRepoFilter_SpendLeverageSuppressed: actual_spend carries no repository and
// cannot be scoped, so a scoped read must suppress the derived figure and DECLARE the
// suppression. Emitting it anyway would divide org-wide dollars by one repository's
// list-price cost and inflate leverage by roughly the fleet-to-repo ratio.
func TestScoresRepoFilter_SpendLeverageSuppressed(t *testing.T) {
	h, db := newTestHandler(t)
	seedTwoRepos(t, db)
	since := scoresSince()

	scoped := getScores(t, h, "/api/v1/scores?since="+since+"&repo="+repoAlpha)
	if scoped.DataQuality == nil || scoped.DataQuality.SpendLeverageSuppressed == nil {
		t.Fatal("scoped response does not declare spend_leverage_suppressed; a suppressed figure " +
			"that is not declared is indistinguishable from an absent measurement")
	}
	if !*scoped.DataQuality.SpendLeverageSuppressed {
		t.Error("spend_leverage_suppressed = false on a scoped read, want true")
	}

	// And the fleet-wide read must NOT carry the flag at all — suppression is a
	// property of scoping, not a permanent caveat.
	fleet := getScores(t, h, "/api/v1/scores?since="+since)
	if fleet.DataQuality != nil && fleet.DataQuality.SpendLeverageSuppressed != nil {
		t.Error("fleet-wide response declares spend_leverage_suppressed; it should be absent")
	}
}

// --- per-developer detail (#590 names this endpoint explicitly) ---

func getDetail(t *testing.T, h *Handler, target string) developerDetailResponse {
	t.Helper()
	code, body := doRequest(t, h, http.MethodGet, target, nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body = %s", target, code, body)
	}
	var resp developerDetailResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal detail: %v; body = %s", err, body)
	}
	return resp
}

// TestDeveloperDetailRepoFilter_ScopedDiffersFromUnscoped is the detail endpoint's
// control arm. A single developer's cost is the number most likely to be quoted
// directly, so an unscoped figure masquerading as scoped is if anything worse here.
func TestDeveloperDetailRepoFilter_ScopedDiffersFromUnscoped(t *testing.T) {
	h, db := newTestHandler(t)
	seedTwoRepos(t, db)
	since := scoresSince()

	fleet := getDetail(t, h, "/api/v1/scores/alice?since="+since)
	alpha := getDetail(t, h, "/api/v1/scores/alice?since="+since+"&repo="+repoAlpha)

	if fleet.TotalCostUSD == alpha.TotalCostUSD {
		t.Fatalf("scoped detail cost equals fleet detail cost (%.2f) — filter not applied",
			alpha.TotalCostUSD)
	}
	if got, want := fleet.TotalCostUSD, 13.00; !floatNear(got, want) {
		t.Errorf("fleet detail cost = %.4f, want %.4f", got, want)
	}
	if got, want := alpha.TotalCostUSD, 3.00; !floatNear(got, want) {
		t.Errorf("alpha detail cost = %.4f, want %.4f", got, want)
	}
	// The issue list must be scoped too — it is the drill-down a human reads, and a
	// leaked out-of-scope issue id names work in another repository.
	for _, iss := range alpha.Issues {
		if strings.HasPrefix(iss.IssueID, "B-") {
			t.Errorf("scoped detail lists out-of-scope issue %q from %s", iss.IssueID, repoBeta)
		}
	}
	if got, want := len(alpha.Issues), 2; got != want {
		t.Errorf("alpha detail issues = %d, want %d", got, want)
	}
	if alpha.RepoScope != repoAlpha {
		t.Errorf("detail repo_scope = %q, want %q", alpha.RepoScope, repoAlpha)
	}
	if fleet.RepoScope != "" {
		t.Errorf("fleet detail carries repo_scope = %q, want empty", fleet.RepoScope)
	}
}

// TestDeveloperDetailRepoFilter_UnknownParamsRejected mirrors the /scores posture. The
// endpoints must agree: a parameter rejected on one and ignored on the other is how a
// caller learns to trust the wrong one.
func TestDeveloperDetailRepoFilter_UnknownParamsRejected(t *testing.T) {
	h, db := newTestHandler(t)
	seedTwoRepos(t, db)
	since := scoresSince()

	for _, bad := range []string{"repos", "Repo", "work_type"} {
		t.Run(bad, func(t *testing.T) {
			code, body := doRequest(t, h, http.MethodGet,
				"/api/v1/scores/alice?since="+since+"&"+bad+"=x", nil)
			if code != http.StatusBadRequest {
				t.Fatalf("detail with unknown param %q: status = %d, want 400; body = %s", bad, code, body)
			}
		})
	}
}

// TestScoresCompare_RejectsRepoParam: /scores/compare shares loadWindow and returns the
// same class of cost figure, so silently ignoring ?repo= there would reproduce #590 one
// endpoint over. It does not implement scoping, and says so, rather than handing back a
// fleet aggregate that looks scoped.
func TestScoresCompare_RejectsRepoParam(t *testing.T) {
	h, db := newTestHandler(t)
	seedTwoRepos(t, db)
	a := time.Now().UTC().AddDate(0, 0, -14).Format("2006-01-02")
	b := time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02")

	code, body := doRequest(t, h, http.MethodGet,
		"/api/v1/scores/compare?since_a="+a+"&until_a="+b+"&since_b="+b+"&repo="+repoAlpha, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("compare with repo=: status = %d, want 400 (not supported must not read as applied); body = %s",
			code, body)
	}

	// Negative control: the same request without repo= must still work, or the test
	// above proves only that the endpoint is broken.
	code, body = doRequest(t, h, http.MethodGet,
		"/api/v1/scores/compare?since_a="+a+"&until_a="+b+"&since_b="+b, nil)
	if code != http.StatusOK {
		t.Fatalf("compare without repo=: status = %d, want 200; body = %s", code, body)
	}
}

// floatNear compares dollar/point figures with a tolerance well below a cent. Costs are
// exact integer micro-dollars in the store (#69) and only become floats at the wire
// boundary, so this guards float formatting, not arithmetic drift.
func floatNear(got, want float64) bool {
	const eps = 1e-6
	d := got - want
	return d < eps && d > -eps
}

// --- Regressions found in review, each reproduced BEFORE it was fixed ---

// TestScoresRepoFilter_RefusedInAnonymizedMode is the k-anonymity guard (#185, #270).
//
// 🔴 THIS WAS A REAL, REPRODUCED BYPASS, not a hypothetical. ?repo= is a
// caller-controlled POPULATION SELECTOR: scoping narrows the cohort BEFORE k-anonymity
// is applied, so a repository only one person works in drops the whole team under the
// floor, everything folds into the residual "other" bucket — which is emitted with NO
// floor — and `total` carries no floor at all. Measured on this exact fixture before
// the guard existed: the scoped response's "other" row AND `total` were alice's
// individual figures, in the mode sold as the works-council / GDPR posture.
//
// /api/v1/scores/{developer} is blanket-404'd in these modes to stop precisely this
// read; ?repo= reintroduced it around the side, and the repository axis is a far
// better attack primitive than the time axis because repo-to-developer association is
// stable and knowable from outside.
//
// It must REJECT, not silently ignore: quietly dropping the filter would return an
// installation-wide aggregate that looks scoped, which is the #590 defect itself.
func TestScoresRepoFilter_RefusedInAnonymizedMode(t *testing.T) {
	for _, mode := range []struct {
		name string
		agg  scoring.AggregationMode
	}{
		{"team", scoring.AggregationTeam},
		{"division", scoring.AggregationDivision},
	} {
		t.Run(mode.name, func(t *testing.T) {
			h, db := newTestHandler(t)
			// A 5-developer team, k=5. Everyone works in beta; ONLY alice in alpha, so
			// scoping to alpha shrinks the cohort to one.
			for i, d := range []string{"alice", "bob", "carol", "dave", "erin"} {
				if err := db.UpsertHierarchy(context.Background(), d, "eng", "platform", "acme"); err != nil {
					t.Fatalf("UpsertHierarchy(%s): %v", d, err)
				}
				seedRepoCost(t, db, repoBeta, d, "B-"+d, float64(10+i))
				seedRepoOutcome(t, db, repoBeta, d, "B-"+d, 2.0)
			}
			seedRepoCost(t, db, repoAlpha, "alice", "A-1", 7.77)
			seedRepoOutcome(t, db, repoAlpha, "alice", "A-1", 1.0)
			h.SetAggregation(mode.agg, 5)
			since := scoresSince()

			code, body := doRequest(t, h, http.MethodGet,
				"/api/v1/scores?since="+since+"&repo="+repoAlpha, nil)
			if code != http.StatusBadRequest {
				t.Fatalf("scoped request in %s mode: status = %d, want 400 — ?repo= narrows the "+
					"cohort below the k floor and the residual bucket is unfloored; body = %s",
					mode.name, code, body)
			}
			// Belt and braces: whatever the status, alice's exact figures must not appear.
			if strings.Contains(string(body), "7.77") {
				t.Errorf("%s-mode response contains the single developer's cost: %s", mode.name, body)
			}

			// POSITIVE CONTROL — without this the assertion above would also pass if the
			// endpoint were simply broken in anonymized mode. The unscoped request must
			// still work and must still return a k-anonymized cohort.
			code, body = doRequest(t, h, http.MethodGet, "/api/v1/scores?since="+since, nil)
			if code != http.StatusOK {
				t.Fatalf("unscoped request in %s mode: status = %d, want 200; body = %s",
					mode.name, code, body)
			}
			if strings.Contains(string(body), `"developer"`) {
				t.Errorf("%s-mode unscoped response names an individual: %s", mode.name, body)
			}
		})
	}
}

// TestScoresRepoFilter_MalformedQueryStringRejected guards the hole that made the
// allowlist decorative.
//
// 🔴 (*url.URL).Query() DISCARDS its parse error and DROPS every pair it could not
// decode. Go 1.17+ rejects ';' as a separator, so one semicolon anywhere voids the
// whole query string — and the allowlist, reading Query(), then saw a CLEAN request
// and returned a FLEET-WIDE 200. Measured before the fix:
//
//	"since=X&repo=a/b;x=1"  -> Query() = {since};  repo vanished
//	"since=X&repos=a/b;x=1" -> Query() = {since};  the UNKNOWN key vanished too
//
// The second is the damning one: the parameter the allowlist exists to reject
// disappeared before the allowlist could see it. Reachable from an ordinary unquoted
// shell variable. A validator built on a parser that silently drops its own failures
// validates nothing.
func TestScoresRepoFilter_MalformedQueryStringRejected(t *testing.T) {
	h, db := newTestHandler(t)
	seedTwoRepos(t, db)
	since := scoresSince()

	for _, raw := range []string{
		"since=" + since + "&repo=" + repoAlpha + ";x=1",  // semicolon in a KNOWN param's value
		"since=" + since + "&repos=" + repoAlpha + ";x=1", // ...and in an UNKNOWN param's
		"since=" + since + "&repo=%zz",                    // invalid percent-encoding
	} {
		t.Run(raw, func(t *testing.T) {
			code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?"+raw, nil)
			if code != http.StatusBadRequest {
				t.Fatalf("malformed query %q: status = %d, want 400; body = %s", raw, code, body)
			}
		})
	}
}

// TestScoresRepoFilter_RepeatedParamRejected: every reader downstream takes the FIRST
// value via Get(), so "?repo=a&repo=b" would scope to a and discard b with no signal —
// a caller who believes they asked for b receives a's figures.
func TestScoresRepoFilter_RepeatedParamRejected(t *testing.T) {
	h, db := newTestHandler(t)
	seedTwoRepos(t, db)
	since := scoresSince()

	code, body := doRequest(t, h, http.MethodGet,
		"/api/v1/scores?since="+since+"&repo="+repoAlpha+"&repo="+repoBeta, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("repeated repo=: status = %d, want 400; body = %s", code, body)
	}
	// Control: one occurrence is fine.
	code, _ = doRequest(t, h, http.MethodGet, "/api/v1/scores?since="+since+"&repo="+repoAlpha, nil)
	if code != http.StatusOK {
		t.Fatalf("single repo=: status = %d, want 200", code)
	}
}

// TestScoresRepoFilter_CoverageHorizonIsScoped: the cost-horizon annotation must
// describe the SCOPED repository, not the installation.
//
// 🔴 Reproduced before the fix: an install capturing since January that onboarded a
// repo in June answered "January" for that repo and set window_predates_cost_capture
// FALSE — asserting a scoped window was fully covered on a DIFFERENT repository's
// evidence, and handing the operator a cost_coverage_safe_since months before the
// scope had any data. That field exists specifically to prevent silently-wrong
// coverage claims, so getting it wrong under a scope is this issue's defect class
// relocated into its own remedy.
func TestScoresRepoFilter_CoverageHorizonIsScoped(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	// alpha captured 100 days ago; beta only 3 days ago.
	seedRepoCostAt(t, db, repoAlpha, "alice", "A-old", 1.00, now.AddDate(0, 0, -100))
	seedRepoCostAt(t, db, repoBeta, "bob", "B-new", 1.00, now.AddDate(0, 0, -3))
	since := now.AddDate(0, 0, -50).Format("2006-01-02")

	beta := getScores(t, h, "/api/v1/scores?since="+since+"&repo="+repoBeta)
	if beta.DataQuality == nil {
		t.Fatal("no data_quality on the scoped response")
	}
	// The 50-day window starts BEFORE beta's 3-day-old horizon, so beta's scoped read
	// must say so. Reporting alpha's 100-day-old horizon would say "covered".
	if beta.DataQuality.WindowPredatesCostCapture == nil {
		t.Fatal("window_predates_cost_capture absent on a scoped read")
	}
	if !*beta.DataQuality.WindowPredatesCostCapture {
		t.Errorf("scoped window_predates_cost_capture = false, want true — the horizon is "+
			"reporting another repository's capture start (cost_coverage_start = %q)",
			beta.DataQuality.CostCoverageStart)
	}
	// Control arm: the same window against alpha IS covered, so a test that always
	// expected `true` would not prove the horizon is scoped.
	alpha := getScores(t, h, "/api/v1/scores?since="+since+"&repo="+repoAlpha)
	if alpha.DataQuality.WindowPredatesCostCapture == nil || *alpha.DataQuality.WindowPredatesCostCapture {
		t.Errorf("alpha scoped window_predates_cost_capture = %v, want false (alpha was captured 100d ago)",
			alpha.DataQuality.WindowPredatesCostCapture)
	}
}

// TestDeveloperDetail_NoOrgWideExclusionLeak: the exclusion measurement is ORG-grain
// and must not appear inside a single-developer response.
//
// 🔴 Reproduced before the fix: a request for alice's detail returned
// repo_scope_excluded = {cost_usd: 99} where all $99 was BOB's repo-blind spend. Wrong
// twice — a reader concludes alice's figure may understate by $99 when none of it is
// hers, AND an installation-wide absolute dollar figure rides inside a response that
// declares itself scoped, which is the embargoed shape.
func TestDeveloperDetail_NoOrgWideExclusionLeak(t *testing.T) {
	h, db := newTestHandler(t)
	seedTwoRepos(t, db)
	seedRepoCost(t, db, repoid.Unqualified, "bob", "U-1", 99.00)
	seedRepoOutcome(t, db, repoid.Unqualified, "bob", "U-1", 1.0)
	since := scoresSince()

	code, body := doRequest(t, h, http.MethodGet,
		"/api/v1/scores/alice?since="+since+"&repo="+repoAlpha, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", code, body)
	}
	if strings.Contains(string(body), "repo_scope_excluded") {
		t.Errorf("per-developer detail carries the ORG-WIDE exclusion block: %s", body)
	}
	if strings.Contains(string(body), "99") {
		t.Errorf("per-developer detail leaks another developer's repo-blind spend: %s", body)
	}
	// Control: the org-grain disclosure DOES belong on /scores, where the population
	// matches. If it were missing there too, the assertion above would be vacuous.
	scores := getScores(t, h, "/api/v1/scores?since="+since+"&repo="+repoAlpha)
	if scores.DataQuality == nil || scores.DataQuality.RepoScopeExcluded == nil {
		t.Fatal("/scores lost its exclusion disclosure — it belongs there, only the detail grain was wrong")
	}
}

// TestUnqualifiedExclusion_CoversTripwireLookback: "omit when clean" must actually
// mean nothing was excluded.
//
// 🔴 Reproduced before the fix: strict scoping also applies inside OutcomeTokenTotals,
// whose attributable window reaches 14 days BEFORE `since`. A repo-blind token row in
// that look-back — outside the reporting window — was excluded by the scope, flipped a
// zero-token tripwire against a NAMED developer, and was invisible to a disclosure
// that only measured [since, until). The response said "clean" while the scope had
// materially changed a data-quality verdict. The disclosure's lower bound is now
// widened by AttributableWindow so it cannot claim clean when it is not.
func TestUnqualifiedExclusion_CoversTripwireLookback(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	// An outcome in alpha merged yesterday, whose tokens were captured 5 days ago by a
	// repo-blind producer — inside the 14-day attributable look-back, but BEFORE a
	// window that starts 2 days ago.
	seedRepoOutcomeAt(t, db, repoAlpha, "alice", "A-7", 1.0, now.AddDate(0, 0, -1))
	seedRepoCostAt(t, db, repoid.Unqualified, "alice", "A-7", 20.00, now.AddDate(0, 0, -5))
	since := now.AddDate(0, 0, -2).Format("2006-01-02")

	scoped := getScores(t, h, "/api/v1/scores?since="+since+"&repo="+repoAlpha)
	if scoped.DataQuality == nil {
		t.Fatal("no data_quality on the scoped response")
	}
	if scoped.DataQuality.RepoScopeExcluded == nil {
		t.Fatal("scoped read reports CLEAN while the scope excluded a repo-blind row from the " +
			"tripwire's look-back — 'absence is the clean signal' must not be a lie")
	}
	if got := scoped.DataQuality.RepoScopeExcluded.CostUSD; !floatNear(got, 20.00) {
		t.Errorf("disclosed excluded cost = %.4f, want 20.00 (the look-back row)", got)
	}
}

// seedRepoCostAt / seedRepoOutcomeAt are the explicit-timestamp forms, for the
// horizon and look-back tests where WHEN a row landed is the thing under test.
func seedRepoCostAt(t *testing.T, db *store.DB, repo, dev, issue string, costUSD float64, ts time.Time) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: dev, IssueID: issue, Repo: repo, Model: "claude-sonnet-4",
		InputTok: 2000, CostMicro: store.DollarsToMicro(costUSD),
		Source: "jsonl", Fidelity: "realtime", Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent(%s,%s,%s,@%s): %v", repo, dev, issue, ts, err)
	}
}

func seedRepoOutcomeAt(t *testing.T, db *store.DB, repo, dev, issue string, weight float64, ts time.Time) {
	t.Helper()
	if _, err := db.InsertOutcome(context.Background(), store.Outcome{
		Developer: dev, IssueID: issue, Repo: repo, Weight: weight, Quality: 1.0,
		MergeCommitSHA: "sha-at-" + repo + "-" + issue, Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertOutcome(%s,%s,%s,@%s): %v", repo, dev, issue, ts, err)
	}
}

// --- Sidecar scoping: gaps a review MUTATION-PROVED, not inferred ---
//
// 🔴 WHY THESE EXIST. The original fixture (seedTwoRepos) is entirely ATTRIBUTED,
// single-price-version spend, which makes every sidecar field IDENTICAL in the scoped
// and unscoped arms: attributed_cost_share is 1.0 either way, no mixed-version warn
// fires either way, and no unattributed split is emitted at all. So the assertions
// could not see those reads. Review flipped each of these call sites to FleetWide
// individually and the FULL SUITE PASSED four times over.
//
// That is the same false-green shape this file's header warns about, one layer up: the
// store tests proved the SQL narrows, and nothing proved the HANDLER passed the scope.
// The loadWindow comment claiming "an unthreaded read shows up as a field that stayed
// fleet-wide" was aspirational until these landed.
//
// A separate fixture rather than perturbing seedTwoRepos: its exact-value and
// partition assertions are already mutation-verified, and changing its totals to serve
// a different test would put verified control arms at risk to save a helper.

// seedSidecarRepos adds UNATTRIBUTED spend to each repo, so the composition sidecar,
// the attributed-share, and the labeled unattributed split all differ between arms.
// alpha: $3.00 attributed + $0.50 unattributed.  beta: $10.00 attributed + $6.00.
func seedSidecarRepos(t *testing.T, db *store.DB) {
	t.Helper()
	seedTwoRepos(t, db)
	seedRepoCost(t, db, repoAlpha, "alice", store.UnattributedIssueID, 0.50)
	seedRepoCost(t, db, repoBeta, "alice", store.UnattributedIssueID, 6.00)
}

// TestScoresRepoFilter_CostCompositionIsScoped pins handler.go's CostCompositionWindow
// call site. Mutating it to FleetWide previously passed the whole suite.
func TestScoresRepoFilter_CostCompositionIsScoped(t *testing.T) {
	h, db := newTestHandler(t)
	seedSidecarRepos(t, db)
	since := scoresSince()

	fleet := getScores(t, h, "/api/v1/scores?since="+since)
	alpha := getScores(t, h, "/api/v1/scores?since="+since+"&repo="+repoAlpha)
	if fleet.CostComposition == nil || alpha.CostComposition == nil {
		t.Fatal("cost_composition missing; the fixture no longer exercises this sidecar")
	}
	if got, want := alpha.CostComposition.TotalCostUSD, 3.50; !floatNear(got, want) {
		t.Errorf("scoped cost_composition total = %.4f, want %.4f", got, want)
	}
	if got, want := fleet.CostComposition.TotalCostUSD, 19.50; !floatNear(got, want) {
		t.Errorf("fleet cost_composition total = %.4f, want %.4f", got, want)
	}
	// attributed_cost_share now genuinely differs: 3.00/3.50 vs 13.00/19.50.
	if alpha.DataQuality == nil || alpha.DataQuality.AttributedCostShare == nil ||
		fleet.DataQuality == nil || fleet.DataQuality.AttributedCostShare == nil {
		t.Fatal("attributed_cost_share missing on one arm")
	}
	if *alpha.DataQuality.AttributedCostShare == *fleet.DataQuality.AttributedCostShare {
		t.Errorf("attributed_cost_share identical across arms (%.6f) — the composition read is "+
			"not scoped", *alpha.DataQuality.AttributedCostShare)
	}
}

// TestScoresRepoFilter_UnattributedSplitIsScoped pins the
// UnattributedBucketCostsWindow call site — also previously mutation-survivable.
func TestScoresRepoFilter_UnattributedSplitIsScoped(t *testing.T) {
	h, db := newTestHandler(t)
	seedSidecarRepos(t, db)
	since := scoresSince()

	sum := func(r scoresResponse) float64 {
		var tot float64
		if r.DataQuality == nil {
			return 0
		}
		for _, b := range r.DataQuality.UnattributedBuckets {
			tot += b.CostUSD
		}
		return tot
	}
	fleet := getScores(t, h, "/api/v1/scores?since="+since)
	alpha := getScores(t, h, "/api/v1/scores?since="+since+"&repo="+repoAlpha)
	if got, want := sum(alpha), 0.50; !floatNear(got, want) {
		t.Errorf("scoped unattributed split = %.4f, want %.4f (beta's $6.00 must not appear)", got, want)
	}
	if got, want := sum(fleet), 6.50; !floatNear(got, want) {
		t.Errorf("fleet unattributed split = %.4f, want %.4f", got, want)
	}
}

// TestScoresRepoFilter_MixedPriceVersionsAreScoped pins DistinctPriceVersionsWindow at
// BOTH layers — the handler call site and the store predicate each survived mutation,
// because the old fixtures priced every row under one version in both arms.
func TestScoresRepoFilter_MixedPriceVersionsAreScoped(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	seedPricedRepoEvent(t, db, repoAlpha, "alice", "A-1", 1, now)
	seedPricedRepoEvent(t, db, repoBeta, "alice", "B-1", 2, now)
	since := scoresSince()

	fleet := getScores(t, h, "/api/v1/scores?since="+since)
	alpha := getScores(t, h, "/api/v1/scores?since="+since+"&repo="+repoAlpha)

	// Fleet spans two price tables and must WARN.
	if fleet.DataQuality == nil || len(fleet.DataQuality.MixedPriceVersions) < 2 {
		t.Fatalf("fleet mixed_price_versions = %v, want >= 2 entries — fixture is not exercising the read",
			fleet.DataQuality)
	}
	// Scoped to one repo there is exactly ONE version, so the warn must NOT fire.
	if alpha.DataQuality != nil && len(alpha.DataQuality.MixedPriceVersions) > 1 {
		t.Errorf("scoped mixed_price_versions = %v, want at most one version — the price-version "+
			"read is returning fleet rows", alpha.DataQuality.MixedPriceVersions)
	}
}

func seedPricedRepoEvent(t *testing.T, db *store.DB, repo, dev, issue string, version int, ts time.Time) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: dev, IssueID: issue, Repo: repo, Model: "claude-sonnet-4",
		InputTok: 2000, CostMicro: 10_000, Source: "jsonl", Fidelity: "realtime",
		PriceVersion: version, Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent(v%d): %v", version, err)
	}
}

// TestScoresRepoFilter_ScopedSuppressesActualPaidFigures tests the spend-leverage
// suppression BEHAVIOUR, not just the flag announcing it.
//
// 🔴 Review replaced BOTH `scope.IsFleetWide()` guards on ActualSpendAllWindow with
// `if true` — i.e. actually dividing org-wide dollars by one repository's cost, the
// precise inflation the code comment warns about — and the full suite PASSED. The old
// test only asserted the boolean, and no fixture ever seeded actual_spend, so
// actual_paid_usd was 0 in both arms no matter what the code did. A flag that says
// "suppressed" while nothing is suppressed is worse than no flag.
func TestScoresRepoFilter_ScopedSuppressesActualPaidFigures(t *testing.T) {
	h, db := newTestHandler(t)
	seedTwoRepos(t, db)
	if err := db.InsertActualSpend(context.Background(), store.ActualSpend{
		Developer:       "alice",
		Period:          time.Now().UTC().Format("2006-01"),
		ActualPaidMicro: store.DollarsToMicro(400),
		Timestamp:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertActualSpend: %v", err)
	}
	since := scoresSince()

	findDev := func(r scoresResponse) developerScoreJSON {
		t.Helper()
		for _, d := range r.Developers {
			if d.Developer == "alice" {
				return d
			}
		}
		t.Fatal("alice absent from response")
		return developerScoreJSON{}
	}

	// POSITIVE CONTROL FIRST: unscoped, the figures must actually be present. Without
	// this arm, asserting 0.0 below would pass against a build that never computes
	// spend leverage at all.
	fleet := findDev(getScores(t, h, "/api/v1/scores?since="+since))
	if got, want := fleet.ActualPaidUSD, 400.00; !floatNear(got, want) {
		t.Fatalf("fleet actual_paid_usd = %.4f, want %.4f — the control arm is not exercising "+
			"actual_spend, so the suppression assertion below would be vacuous", got, want)
	}
	if fleet.SpendLeverage <= 0 {
		t.Fatalf("fleet spend_leverage = %.6f, want > 0", fleet.SpendLeverage)
	}

	// Scoped: both must be zeroed, because org-wide paid dollars cannot be divided by
	// one repository's cost without inventing an allocation.
	alpha := findDev(getScores(t, h, "/api/v1/scores?since="+since+"&repo="+repoAlpha))
	if got := alpha.ActualPaidUSD; !floatNear(got, 0) {
		t.Errorf("scoped actual_paid_usd = %.4f, want 0 — org-wide actual spend is leaking into a "+
			"scoped response", got)
	}
	if got := alpha.SpendLeverage; !floatNear(got, 0) {
		t.Errorf("scoped spend_leverage = %.6f, want 0 — this is org dollars over one repo's cost, "+
			"inflated by roughly the fleet-to-repo ratio", got)
	}

	// The per-developer DETAIL is a SEPARATE call site and survives independently.
	detail := getDetail(t, h, "/api/v1/scores/alice?since="+since)
	if got, want := detail.ActualPaidUSD, 400.00; !floatNear(got, want) {
		t.Fatalf("fleet detail actual_paid_usd = %.4f, want %.4f (control)", got, want)
	}
	scopedDetail := getDetail(t, h, "/api/v1/scores/alice?since="+since+"&repo="+repoAlpha)
	if got := scopedDetail.ActualPaidUSD; !floatNear(got, 0) {
		t.Errorf("scoped detail actual_paid_usd = %.4f, want 0", got)
	}
	if got := scopedDetail.SpendLeverage; !floatNear(got, 0) {
		t.Errorf("scoped detail spend_leverage = %.6f, want 0", got)
	}
}

// TestDeveloperDetailRepoFilter_TripwireGoesStrict ports the SHARED-1 collision up to
// the API. The store-level twin proves the SQL; this proves the handler passes the
// scope to it. Mutating that call site to FleetWide previously passed the suite — and
// this is the false-NEGATIVE case: a repo-blind row from elsewhere silently funding a
// scoped outcome and suppressing a flag that should fire, at the grain where a single
// developer's number gets quoted.
func TestDeveloperDetailRepoFilter_TripwireGoesStrict(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	seedRepoOutcomeAt(t, db, repoAlpha, "alice", "SHARED-1", 1.0, now)
	// Repo-blind tokens on the SAME issue id — enough to clear the tripwire if wrongly admitted.
	seedRepoCostAt(t, db, repoid.Unqualified, "alice", "SHARED-1", 20.00, now)
	since := scoresSince()

	scoped := getDetail(t, h, "/api/v1/scores/alice?since="+since+"&repo="+repoAlpha)
	var scopedFlagged bool
	for _, iss := range scoped.Issues {
		if iss.IssueID == "SHARED-1" && iss.ZeroToken {
			scopedFlagged = true
		}
	}
	if !scopedFlagged {
		t.Errorf("scoped detail did NOT flag SHARED-1 as zero-token — repo-blind tokens belonging "+
			"to no repository are funding a scoped outcome and suppressing the tripwire; issues = %+v",
			scoped.Issues)
	}

	// CONTROL: fleet-wide, the tolerant join legitimately finds those tokens, so the
	// flag must NOT fire. Without this, a build that flags everything would pass above.
	fleet := getDetail(t, h, "/api/v1/scores/alice?since="+since)
	for _, iss := range fleet.Issues {
		if iss.IssueID == "SHARED-1" && iss.ZeroToken {
			t.Errorf("fleet-wide detail flagged SHARED-1 — the tolerant join should find the " +
				"repo-blind tokens, so this test cannot distinguish strict from broken")
		}
	}
}

// TestDeveloperDetailRepoFilter_DeclaresSuppression pins the detail response's
// disclosure fields, whose deletion previously passed the suite.
func TestDeveloperDetailRepoFilter_DeclaresSuppression(t *testing.T) {
	h, db := newTestHandler(t)
	seedTwoRepos(t, db)
	since := scoresSince()

	scoped := getDetail(t, h, "/api/v1/scores/alice?since="+since+"&repo="+repoAlpha)
	if scoped.SpendLeverageSuppressed == nil || !*scoped.SpendLeverageSuppressed {
		t.Errorf("scoped detail does not declare spend_leverage_suppressed (got %v)",
			scoped.SpendLeverageSuppressed)
	}
	if scoped.RepoScope != repoAlpha {
		t.Errorf("scoped detail repo_scope = %q, want %q", scoped.RepoScope, repoAlpha)
	}
	fleet := getDetail(t, h, "/api/v1/scores/alice?since="+since)
	if fleet.SpendLeverageSuppressed != nil {
		t.Errorf("fleet detail declares spend_leverage_suppressed; it should be absent")
	}
}

// TestScoresRepoFilter_DisclosureHonorsUntil: buildScopeDisclosure takes `until`, and
// replacing it with the zero value previously passed the suite.
func TestScoresRepoFilter_DisclosureHonorsUntil(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	seedRepoCostAt(t, db, repoAlpha, "alice", "A-1", 1.00, now.AddDate(0, 0, -10))
	seedRepoOutcomeAt(t, db, repoAlpha, "alice", "A-1", 1.0, now.AddDate(0, 0, -10))
	// A repo-blind row TODAY — inside an open window, outside one closed yesterday.
	seedRepoCostAt(t, db, repoid.Unqualified, "alice", "U-1", 99.00, now)
	since := now.AddDate(0, 0, -20).Format("2006-01-02")
	until := now.AddDate(0, 0, -1).Format("2006-01-02")

	open := getScores(t, h, "/api/v1/scores?since="+since+"&repo="+repoAlpha)
	if open.DataQuality == nil || open.DataQuality.RepoScopeExcluded == nil {
		t.Fatal("open window: expected the repo-blind row to be disclosed")
	}
	bounded := getScores(t, h, "/api/v1/scores?since="+since+"&until="+until+"&repo="+repoAlpha)
	if bounded.DataQuality != nil && bounded.DataQuality.RepoScopeExcluded != nil {
		t.Errorf("bounded window still discloses a row outside it: %+v — `until` is not reaching "+
			"the disclosure read", *bounded.DataQuality.RepoScopeExcluded)
	}
}

// TestScoresRepoFilter_SegmentReconciliationIsScoped pins the #466 block against the
// same class of false green as the two sidecars above.
//
// 🔴 It is the NEWEST sidecar and was the only one absent from this fixture, which is
// precisely how the other three came to be mutation-survivable: the store tests proved
// the SQL narrows, nothing proved the HANDLER passed the scope down. The block is built
// from `issueCosts`, which loadWindow reads with the request's scope — so a call site
// reverted to FleetWide shows up here as a scoped response reporting fleet-wide money.
func TestScoresRepoFilter_SegmentReconciliationIsScoped(t *testing.T) {
	h, db := newTestHandler(t)
	seedSidecarRepos(t, db)
	// Spend on a real issue that shipped nothing, in ONE repo only, so the no_outcome
	// bucket differs between arms too and not merely the window total.
	seedRepoCost(t, db, repoBeta, "alice", "issue-beta-abandoned", 4.00)
	since := scoresSince()

	fleet := getScores(t, h, "/api/v1/scores?since="+since)
	alpha := getScores(t, h, "/api/v1/scores?since="+since+"&repo="+repoAlpha)
	if fleet.SegmentReconciliation == nil || alpha.SegmentReconciliation == nil {
		t.Fatal("segment_reconciliation missing on an arm; the fixture no longer exercises it")
	}
	fleetTotal := fleet.SegmentReconciliation.Total
	alphaTotal := alpha.SegmentReconciliation.Total
	assertPartition(t, "fleet", fleetTotal)
	assertPartition(t, "alpha", alphaTotal)

	// alpha: $3.00 attributed + $0.50 unattributed. Strict scope excludes beta entirely.
	if got, want := alphaTotal.WindowCostMicro, micro(3.50); got != want {
		t.Errorf("scoped window_cost_micro = %d, want %d. %d would mean the reconciliation "+
			"read stayed FLEET-WIDE while the response claimed a repo scope",
			got, want, fleetTotal.WindowCostMicro)
	}
	if got, want := fleetTotal.WindowCostMicro, micro(23.50); got != want {
		t.Errorf("fleet window_cost_micro = %d, want %d", got, want)
	}
	// The gap itself must be scoped, not just the total: beta's $4.00 of abandoned
	// spend belongs to no arm but the fleet one.
	if alphaTotal.NoOutcomeCostMicro != 0 {
		t.Errorf("scoped no_outcome = %d, want 0 (the abandoned issue is in beta)",
			alphaTotal.NoOutcomeCostMicro)
	}
	if got, want := fleetTotal.NoOutcomeCostMicro, micro(4.00); got != want {
		t.Errorf("fleet no_outcome = %d, want %d", got, want)
	}
	// And the unattributed bucket, which has a non-zero value on BOTH arms — so an
	// unscoped read cannot hide behind a zero.
	if got, want := alphaTotal.UnattributedCostMicro, micro(0.50); got != want {
		t.Errorf("scoped unattributed = %d, want %d", got, want)
	}
	if got, want := fleetTotal.UnattributedCostMicro, micro(6.50); got != want {
		t.Errorf("fleet unattributed = %d, want %d", got, want)
	}
}
