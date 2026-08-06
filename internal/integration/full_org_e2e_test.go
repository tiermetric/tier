//go:build integration

// Full-org, multi-vendor end-to-end rig (#266). The rest of the integration
// suite covers ONE seam per file; the 2026-07-12 PM audit ran the entire chain —
// multi-vendor proxy capture -> org_hierarchy -> webhook merge/revert ->
// actual_spend -> developer AND team-mode /scores — by hand with a throwaway
// harness. This promotes that run into a repeatable test.
//
// What it composes that no single existing test does:
//   - /anthropic/ AND /openai/ reverse-proxy capture through the REAL proxy
//     against local stub upstreams (no network egress), including one open-weights
//     model ("llama-3.1-70b-instruct") that exercises the size-class pricing path
//     (self-hosted-large, combined rate) rather than a named table entry.
//   - An 8-developer / 2-team / 1-org fixture wired over POST /api/v1/org_hierarchy.
//   - 23 merged-PR webhooks + 1 quality revert (which floors one outcome to 0.1).
//   - A per-developer actual_spend (Spend Leverage) and an org invoice (seat
//     allocation).
//   - ONE shared on-disk store read back first in developer-aggregation mode
//     (named rows, exact TIER) and then in team-aggregation mode (k=5), proving the
//     sub-k team folds into "other" with totals preserved.
//
// Gotcha the live run hit (issue #266): the proxy dedups on
// hash(provider, response.id) (proxy.go idempotencyKeyForProxy ->
// collector.MessageIdempotencyKey). A stub that reused a response id would silently
// dedup the second call. Both stubs below emit a UNIQUE id per call via an atomic
// counter, so every capture is a distinct row.
//
// Expected numbers (encoded and asserted below). Weights: size/XS 0.5, S 1.0,
// M 3.0, L 5.0, XL 8.0 (webhook labelWeight). Quality: 1.0 clean, 0.1 quality
// revert. Per-call list cost: Anthropic claude-sonnet-4 (1,000,000 in / 200,000
// out) = $6.00; OpenAI gpt-4o = $4.50; open-weights llama-3.1-70b-instruct
// (self-hosted-large combined $2/M over 1,200,000 tokens) = $2.40.
//
//	dev    team        vendor        PRs                        points  cost($)  TIER      ranked
//	alice  team-alpha  anthropic     3×M (1 reverted)           6.3     18.00    350.000   yes
//	bob    team-alpha  anthropic     3×L                        15.0    18.00    833.333   yes
//	carol  team-alpha  openai        3×M                        9.0     13.50    666.667   yes
//	dave   team-alpha  openai        3×S                        3.0     13.50    222.222   yes
//	erin   team-alpha  open-weights  3×M                        9.0      7.20   1250.000   yes
//	frank  team-beta   anthropic     3×M                        9.0     18.00    500.000   yes
//	grace  team-beta   openai        3×L                        15.0    13.50   1111.111   yes
//	heidi  team-beta   open-weights  2×XS                       1.0      4.80    208.333   no (n<3)
//
// Team rollup (sum-then-divide): team-alpha points 42.3 / $70.20; team-beta
// points 25.0 / $36.30; grand total points 67.3 / $106.50. Under k=5 team-alpha
// (5 contributing devs) is named and team-beta (3 contributing) folds into
// "other" — totals preserved.
package integration

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/api"
	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/proxy"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
	"github.com/tiermetric/tier/internal/webhook"
)

// e2e per-call token usage — the same block for every capture so per-dev cost is
// exactly (per-call cost × number of calls). Large enough that every multi-PR
// developer clears the $5 ranking floor and every issue clears the 1,000-token
// zero-token tripwire by a wide margin.
const (
	e2eCallInput  = 1_000_000
	e2eCallOutput = 200_000

	e2eAnthModel  = "claude-sonnet-4"        // $3/$15 per M
	e2eOAModel    = "gpt-4o"                 // $2.50/$10 per M
	e2eOWModel    = "llama-3.1-70b-instruct" // unknown key -> self-hosted-large (combined $2/M)
	e2eOrg        = "e2e-org"
	e2eOrgInvoice = 800.0 // org actual_spend, allocated across the 8 imported seats
)

// e2eLabelWeight mirrors webhook labelWeight so the test can independently derive
// the points it asserts against, instead of trusting the same literal twice.
func e2eLabelWeight(size string) float64 {
	switch size {
	case "size/XS":
		return 0.5
	case "size/S":
		return 1.0
	case "size/M":
		return 3.0
	case "size/L":
		return 5.0
	case "size/XL":
		return 8.0
	}
	panic("e2e: unknown size label " + size)
}

// e2ePR is one merged pull request in the fixture.
type e2ePR struct {
	issue  int
	size   string
	revert bool // a quality revert floors this outcome's quality to 0.1
}

// e2eDev is one developer: their vendor (proxy path + model) and their PRs, plus
// the exact points and ranked verdict this rig pins.
type e2eDev struct {
	name      string
	team      string
	proxyPath string // "/anthropic" or "/openai"
	model     string
	prs       []e2ePR
	wantPts   float64
	wantRank  bool
}

// e2eFixture is the 8-developer / 2-team org. team-alpha has 5 contributing
// developers (clears k=5, named in team mode); team-beta has 3 (folds into
// "other"). heidi has only 2 outcomes, so she is the one below-ranking-floor dev.
var e2eFixture = []e2eDev{
	{"alice", "team-alpha", "/anthropic", e2eAnthModel,
		[]e2ePR{{101, "size/M", false}, {102, "size/M", false}, {103, "size/M", true}}, 6.3, true},
	{"bob", "team-alpha", "/anthropic", e2eAnthModel,
		[]e2ePR{{111, "size/L", false}, {112, "size/L", false}, {113, "size/L", false}}, 15.0, true},
	{"carol", "team-alpha", "/openai", e2eOAModel,
		[]e2ePR{{121, "size/M", false}, {122, "size/M", false}, {123, "size/M", false}}, 9.0, true},
	{"dave", "team-alpha", "/openai", e2eOAModel,
		[]e2ePR{{131, "size/S", false}, {132, "size/S", false}, {133, "size/S", false}}, 3.0, true},
	{"erin", "team-alpha", "/openai", e2eOWModel,
		[]e2ePR{{141, "size/M", false}, {142, "size/M", false}, {143, "size/M", false}}, 9.0, true},
	{"frank", "team-beta", "/anthropic", e2eAnthModel,
		[]e2ePR{{201, "size/M", false}, {202, "size/M", false}, {203, "size/M", false}}, 9.0, true},
	{"grace", "team-beta", "/openai", e2eOAModel,
		[]e2ePR{{211, "size/L", false}, {212, "size/L", false}, {213, "size/L", false}}, 15.0, true},
	{"heidi", "team-beta", "/openai", e2eOWModel,
		[]e2ePR{{221, "size/XS", false}, {222, "size/XS", false}}, 1.0, false},
}

// e2eQuiet returns a discard logger for the proxy/API wiring.
func e2eQuiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newE2EAnthropicStub is a local "Anthropic API": it echoes the model and token
// counts the client passes via X-Stub-* headers and stamps a UNIQUE msg_ id per
// call (the #266 dedup gotcha — a reused id would silently drop the second event).
func newE2EAnthropicStub(t *testing.T) *httptest.Server {
	t.Helper()
	var seq int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := r.Header.Get("X-Stub-Model")
		in, _ := strconv.Atoi(r.Header.Get("X-Stub-Input"))
		out, _ := strconv.Atoi(r.Header.Get("X-Stub-Output"))
		id := fmt.Sprintf("msg_e2e_%d", atomic.AddInt64(&seq, 1))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":%q,"model":%q,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`,
			id, model, in, out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newE2EOpenAIStub is the OpenAI-shape twin: prompt_tokens / completion_tokens and
// a unique chatcmpl- id per call. It also serves the open-weights model (the model
// string comes from the header), so the same upstream drives gpt-4o and
// llama-3.1-70b captures.
func newE2EOpenAIStub(t *testing.T) *httptest.Server {
	t.Helper()
	var seq int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := r.Header.Get("X-Stub-Model")
		in, _ := strconv.Atoi(r.Header.Get("X-Stub-Input"))
		out, _ := strconv.Atoi(r.Header.Get("X-Stub-Output"))
		id := fmt.Sprintf("chatcmpl-e2e_%d", atomic.AddInt64(&seq, 1))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":%q,"model":%q,"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`,
			id, model, in, out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// e2eDevServer mounts the tierd developer-mode composition over db: the REST API,
// the HMAC webhook, and BOTH reverse proxies (StripPrefix'd exactly as cmd/tierd's
// registerProxy mounts them). It is the composition the client drives.
func e2eDevServer(t *testing.T, db *store.DB, anthURL, oaURL string) *httptest.Server {
	t.Helper()
	quiet := e2eQuiet()
	anthTarget, err := url.Parse(anthURL)
	if err != nil {
		t.Fatalf("parse anthropic stub URL: %v", err)
	}
	oaTarget, err := url.Parse(oaURL)
	if err != nil {
		t.Fatalf("parse openai stub URL: %v", err)
	}
	// Bare sink; proxy.New builds its own collector.SourceProxy tagger internally
	// (#337), mirroring the production wiring in cmd/tierd — no external tagger.
	eventSink := ingester.Store(db)
	anthProxy := proxy.New(anthTarget, proxy.ProviderAnthropic, collector.SourceProxy, eventSink, nil, nil, quiet)
	oaProxy := proxy.New(oaTarget, proxy.ProviderOpenAI, collector.SourceProxy, eventSink, nil, nil, quiet)

	mux := http.NewServeMux()
	api.New(db, quiet, apiToken, nil, "integration", api.RateLimitConfig{}).Register(mux)
	mux.Handle("POST /webhook/github", webhook.New(db, webhookSecret, quiet))
	mux.Handle("/anthropic/", http.StripPrefix("/anthropic", anthProxy))
	mux.Handle("/openai/", http.StripPrefix("/openai", oaProxy))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// e2eTeamServer mounts a SECOND API composition over the SAME db in team-
// aggregation mode with the given k floor — the "serve restart into team mode"
// leg. Reusing one store proves the two modes read the identical data.
func e2eTeamServer(t *testing.T, db *store.DB, k int) *httptest.Server {
	t.Helper()
	h := api.New(db, e2eQuiet(), apiToken, nil, "integration", api.RateLimitConfig{})
	h.SetAggregation(scoring.AggregationTeam, k)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// e2eCapture drives one proxy call: it POSTs through proxyBase (srv.URL + the
// provider prefix), carrying the TIER attribution headers the client sets and the
// X-Stub-* headers the local upstream echoes into its usage block. The proxy
// parses the response synchronously in ModifyResponse, so the TokenEvent is
// persisted by the time Do returns.
func e2eCapture(t *testing.T, proxyBase, dev string, issue int, model string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyBase+"/v1/messages", http.NoBody)
	if err != nil {
		t.Fatalf("new capture request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tier-Developer", dev)
	req.Header.Set("X-Tier-Issue", fmt.Sprintf("issue-%d", issue))
	req.Header.Set("X-Stub-Model", model)
	req.Header.Set("X-Stub-Input", strconv.Itoa(e2eCallInput))
	req.Header.Set("X-Stub-Output", strconv.Itoa(e2eCallOutput))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("capture (dev=%s issue=%d): %v", dev, issue, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("capture (dev=%s issue=%d): status %d, want 200", dev, issue, resp.StatusCode)
	}
}

// e2eMergePR posts a merged-PR webhook. The head ref feature/<issue>-e2e maps to
// issue-<issue> (issueref.FromBranch), matching the X-Tier-Issue of the capture,
// and user.login matches the capture's developer — so the cost and the outcome
// join on (developer, issue).
func e2eMergePR(t *testing.T, srvURL, dev string, issue int, size, sha, delivery string) {
	t.Helper()
	postWebhook(t, srvURL, "pull_request", delivery, map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number": issue, "merged": true, "merge_commit_sha": sha,
			"head":   map[string]any{"ref": fmt.Sprintf("feature/%d-e2e", issue)},
			"user":   map[string]any{"login": dev},
			"labels": []map[string]any{{"name": size}},
		},
	})
}

// TestE2E_FullOrgMultiVendorRig is the #266 repeatable full-chain run.
func TestE2E_FullOrgMultiVendorRig(t *testing.T) {
	// Per-call list costs, priced through the SAME function the proxy uses, so the
	// pins below track any table/formula change instead of hard-coding dollars.
	usage := store.CostUsage{Input: e2eCallInput, Output: e2eCallOutput}
	anthCall := store.ComputeCost(e2eAnthModel, usage)
	oaCall := store.ComputeCost(e2eOAModel, usage)
	owCall := store.ComputeCost(e2eOWModel, usage)

	// Pin the per-call costs so a silent pricing regression fails HERE with a clear
	// message, not obscurely inside a TIER ratio. $6.00 / $4.50 / $2.40 in micros.
	if anthCall != store.DollarsToMicro(6.00) {
		t.Fatalf("anthropic per-call cost = %d micro, want %d ($6.00)", anthCall, store.DollarsToMicro(6.00))
	}
	if oaCall != store.DollarsToMicro(4.50) {
		t.Fatalf("openai per-call cost = %d micro, want %d ($4.50)", oaCall, store.DollarsToMicro(4.50))
	}
	if owCall != store.DollarsToMicro(2.40) {
		t.Fatalf("open-weights per-call cost = %d micro, want %d ($2.40)", owCall, store.DollarsToMicro(2.40))
	}
	// The open-weights model must price via the size-class path (self-hosted-large,
	// $2/M combined), NOT the self-hosted-medium unknown-model fallback ($0.50/M).
	if fb := store.ComputeCost("model-that-does-not-exist-xyz", usage); owCall == fb {
		t.Fatalf("open-weights cost %d equals the unknown-model fallback %d — 70b did not hit the size-class path", owCall, fb)
	}

	// callMicro returns the per-call cost for a model.
	callMicro := func(model string) int64 {
		switch model {
		case e2eAnthModel:
			return anthCall
		case e2eOAModel:
			return oaCall
		default:
			return owCall
		}
	}

	// ONE on-disk store, shared across the developer-mode and team-mode serves.
	db, err := store.Open(filepath.Join(t.TempDir(), "full-org-e2e.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	anthStub := newE2EAnthropicStub(t)
	oaStub := newE2EOpenAIStub(t)
	srv := e2eDevServer(t, db, anthStub.URL, oaStub.URL)

	// --- Phase 1: multi-vendor proxy capture ---------------------------------
	// Drive EVERY capture before any webhook, so each token event's timestamp
	// precedes its outcome's merge timestamp and lands inside the 14-day
	// attributable window (otherwise the zero-token tripwire would false-flag).
	for _, d := range e2eFixture {
		for _, pr := range d.prs {
			e2eCapture(t, srv.URL+d.proxyPath, d.name, pr.issue, d.model)
		}
	}

	// --- Phase 2: merged-PR outcomes -----------------------------------------
	n := 0
	var aliceRevertSHA string
	for _, d := range e2eFixture {
		for _, pr := range d.prs {
			n++
			sha := fmt.Sprintf("%040d", n) // deterministic, valid 40-hex sha
			e2eMergePR(t, srv.URL, d.name, pr.issue, pr.size, sha, fmt.Sprintf("e2e-pr-%d", n))
			if pr.revert {
				aliceRevertSHA = sha
			}
		}
	}
	if n != 23 {
		t.Fatalf("posted %d merged PRs, want 23 (fixture drift)", n)
	}
	if aliceRevertSHA == "" {
		t.Fatal("fixture has no revert-marked PR — the quality-revert leg would not run")
	}

	// --- Phase 3: quality revert ---------------------------------------------
	// A revert push referencing alice's issue-103 merge commit, with a quality
	// keyword ("broke prod"), floors that outcome's quality to 0.1 (#134).
	postWebhook(t, srv.URL, "push", "e2e-revert-1", map[string]any{
		"commits": []map[string]any{{
			"id":      fmt.Sprintf("%040d", 9999),
			"message": "Revert \"feat: alice work\"\n\nThis reverts commit " + aliceRevertSHA + ". broke prod",
			"author":  map[string]any{"username": "revert-bot"},
		}},
	})

	// --- Phase 4: org hierarchy + finance ------------------------------------
	period := time.Now().UTC().Format("2006-01")
	since := time.Now().UTC().AddDate(0, -1, 0)

	type hierItem struct {
		Developer string `json:"developer"`
		Team      string `json:"team"`
		Org       string `json:"org"`
	}
	var batch []hierItem
	for _, d := range e2eFixture {
		batch = append(batch, hierItem{Developer: d.name, Team: d.team, Org: e2eOrg})
	}
	if code, body := postHierarchyJSON(t, http.MethodPost, srv.URL+"/api/v1/org_hierarchy", batch); code != http.StatusCreated {
		t.Fatalf("org_hierarchy import: status %d, body %s", code, body)
	}

	// Finance: alice's actual invoice is half her list cost -> Spend Leverage 2.0.
	aliceMicro := 3 * anthCall
	aliceListUSD := store.MicroToDollars(aliceMicro)
	postJSON(t, srv.URL+"/api/v1/actual_spend", map[string]any{
		"developer": "alice", "period": period, "actual_paid_usd": aliceListUSD / 2.0,
	}, http.StatusCreated)

	// $800 org invoice: an even split would be $100/seat, but alice's $9 tier-1
	// actual_spend row is carved out first, so the remaining 7 seats get
	// (800-9)/7 = $113.00 each.
	seedOrgSpend(t, db, e2eOrg, period, e2eOrgInvoice)

	// --- Phase 5: developer-mode readback ------------------------------------
	var alphaMicro, betaMicro int64
	var alphaPts, betaPts float64
	t.Run("developer-mode", func(t *testing.T) {
		sr := getScores(t, srv.URL)
		for _, d := range e2eFixture {
			ds := devByName(t, sr, d.name)

			// Points: derived independently from weights×quality, and pinned to the
			// fixture literal, so both must agree with the API.
			var wantPts float64
			for _, pr := range d.prs {
				q := 1.0
				if pr.revert {
					q = 0.1
				}
				wantPts += e2eLabelWeight(pr.size) * q
			}
			if !approxEq(wantPts, d.wantPts) {
				t.Fatalf("%s: fixture points literal %v disagrees with derived %v", d.name, d.wantPts, wantPts)
			}
			if !approxEq(ds.WeightedPoints, wantPts) {
				t.Errorf("%s: weighted_points = %v, want %v", d.name, ds.WeightedPoints, wantPts)
			}

			// Cost: exact at the store layer, in integer micro-dollars.
			wantMicro := callMicro(d.model) * int64(len(d.prs))
			gotMicro, ok := developerCostMicro(t, db, d.name)
			if !ok {
				t.Fatalf("%s: no cost row persisted", d.name)
			}
			if gotMicro != wantMicro {
				t.Errorf("%s: cost = %d micro, want %d", d.name, gotMicro, wantMicro)
			}

			// TIER = points / (cost_USD / 1000), against the SAME micro the store
			// returned so the comparison is exact division, not a hard-coded ratio.
			wantTIER := wantPts / (store.MicroToDollars(wantMicro) / 1000.0)
			if !approxEq(ds.TIER, wantTIER) {
				t.Errorf("%s: TIER = %v, want %v", d.name, ds.TIER, wantTIER)
			}

			// Ranked verdict (floors + zero-token). No outcome may be zero-token
			// flagged: every issue was captured with 1.2M tokens.
			if ds.Ranked != d.wantRank {
				t.Errorf("%s: ranked = %v, want %v", d.name, ds.Ranked, d.wantRank)
			}
			if ds.FlaggedOutcomes != 0 {
				t.Errorf("%s: flagged_outcomes = %d, want 0 (every issue has 1.2M tokens)", d.name, ds.FlaggedOutcomes)
			}

			// Accumulate team totals for the team-mode cross-check.
			if d.team == "team-alpha" {
				alphaMicro += wantMicro
				alphaPts += wantPts
			} else {
				betaMicro += wantMicro
				betaPts += wantPts
			}
		}

		// The below-floor dev is listed, not hidden.
		if _, ok := findDevOpt(sr, "heidi"); !ok {
			t.Error("heidi (below-floor) missing from /scores — below-floor rows must be listed, not hidden")
		}

		// Spend Leverage: list $18.00 / actual $9.00 = 2.0.
		a := alice(t, sr)
		if !approxEq(a.SpendLeverage, 2.0) {
			t.Errorf("alice spend_leverage = %v, want 2.0", a.SpendLeverage)
		}

		// Seat allocation across the 8 imported seats, reconciling exactly to the
		// $800 invoice (#40/#41). alice has a direct actual_spend row ($9), which is a
		// tier-1 slice carved out before the remainder is split; the other 7 seats
		// share ($800 − $9) = $791, i.e. $113.00 each. bob (no direct row) reads that.
		aliceActual := aliceListUSD / 2.0
		wantBobAlloc := (e2eOrgInvoice - aliceActual) / 7.0
		if paid := devAlloc(t, db, "bob", since); !approxEq(paid, wantBobAlloc) {
			t.Errorf("bob allocated spend = %v, want %v (($800 − $%.2f alice tier-1) / 7 seats)", paid, wantBobAlloc, aliceActual)
		}
		// The whole allocation must reconcile to the invoice: alice's $9 tier-1 slice
		// plus the 7 remainder slices sum back to $800 — data reallocated, never lost.
		var allocTotal float64
		for _, d := range e2eFixture {
			allocTotal += devAlloc(t, db, d.name, since)
		}
		if !approxEq(allocTotal, e2eOrgInvoice) {
			t.Errorf("allocated spend across 8 seats = %v, want %v (must reconcile to the org invoice)", allocTotal, e2eOrgInvoice)
		}

		// Pin the accumulated team totals so the team-mode assertions rest on
		// known-good numbers.
		if !approxEq(alphaPts, 42.3) {
			t.Fatalf("team-alpha points = %v, want 42.3", alphaPts)
		}
		if !approxEq(betaPts, 25.0) {
			t.Fatalf("team-beta points = %v, want 25.0", betaPts)
		}
	})

	// --- Phase 6: team-mode readback (k=5) over the SAME store ----------------
	t.Run("team-mode-k-anon", func(t *testing.T) {
		teamSrv := e2eTeamServer(t, db, scoring.DefaultKAnonymity)
		ts := getTeamScores(t, teamSrv.URL) // also asserts no developer rows leak

		byTeam := map[string]teamRowJSON{}
		for _, row := range ts.Teams {
			byTeam[row.Team] = row
		}
		// #593: team-beta's 3 contributors are BELOW k=5, so the residual is WITHHELD
		// rather than folded into a published "other" row. Exactly ONE row survives.
		//
		// This assertion previously required two rows and then proved "grand total
		// (named + folded) reproduces the whole-org sum ... data is suppressed by
		// identity, never dropped". That property was real and it was the disclosure:
		// with it, total - team-alpha = team-beta, so a 3-person cohort was recoverable
		// whether or not its row was printed. the maintainer retired it (option A, 2026-08-03).
		if len(ts.Teams) != 1 {
			t.Fatalf("team-mode returned %d rows, want 1 (team-alpha only; the sub-k residual is "+
				"withheld); got %+v", len(ts.Teams), ts.Teams)
		}
		alpha, ok := byTeam["team-alpha"]
		if !ok {
			t.Fatalf("team-alpha (5 contributing) must be named; got %+v", ts.Teams)
		}
		if _, named := byTeam["team-beta"]; named {
			t.Errorf("sub-k team-beta (3 contributing) must never be named; got %+v", ts.Teams)
		}
		if _, folded := byTeam[scoring.OtherCohort]; folded {
			t.Errorf("a sub-k residual must be withheld, not published as 'other'; got %+v", ts.Teams)
		}

		// The named cohort's own figures are unaffected by the suppression.
		alphaCostUSD := store.MicroToDollars(alphaMicro)
		if !approxEq(alpha.WeightedPoints, 42.3) || !approxEq(alpha.TotalCostUSD, alphaCostUSD) {
			t.Errorf("team-alpha = points %v cost %v, want 42.3 / %v", alpha.WeightedPoints, alpha.TotalCostUSD, alphaCostUSD)
		}
		// 🔴 And the grand total must be ABSENT: publishing it here would hand back
		// total(67.3) - alpha(42.3) = 25.0, team-beta's points exactly.
		if ts.Total != nil {
			t.Errorf("team-mode grand total must be withheld alongside a suppressed residual — it "+
				"is the differencing channel; got %+v", ts.Total)
		}
	})
}
