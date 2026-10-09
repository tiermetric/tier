package api

// Tests for #856: the k-anonymity floor counts PEOPLE, not id strings.
//
// Every test here builds a group that clears the floor under the pre-#856 rule
// (k non-zero id strings) while holding fewer than k counted people under the
// ruled one (rostered, alias-deduped, not a bot, captured evidence in the window),
// and asserts the group is no longer published. Since #864 a residual below k
// withholds the whole response, the healthy control team included, so each
// verdict pins the census's count of the withheld people: a build that counted
// the padding names the group and withholds nothing.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
	"github.com/tiermetric/tier/internal/webhook"
)

// rosterOnly puts dev on the roster in team, with no activity.
func rosterOnly(t *testing.T, db *store.DB, team, dev string) {
	t.Helper()
	if err := baselineHierarchy(db, context.Background(), dev, team, "div-"+team, "acme"); err != nil {
		t.Fatalf("baselineHierarchy(%s): %v", dev, err)
	}
}

// postManualCost records a manual cost row for dev through POST /api/v1/costs,
// the source='api' path the ruling says is not captured evidence.
func postManualCost(t *testing.T, h *Handler, dev string, costUSD float64) {
	t.Helper()
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", map[string]any{
		"developer": dev, "issue_id": "fee-" + dev, "model": "claude-sonnet-4",
		"cost_usd": costUSD, "source": "api", "fidelity": "daily",
	})
	if code != http.StatusCreated {
		t.Fatalf("POST /costs for %s: status %d, body %s", dev, code, body)
	}
}

// seedPushOutcome records a push-captured outcome for dev, the other
// non-evidence the ruling names.
func seedPushOnlyOutcome(t *testing.T, db *store.DB, dev string, ts time.Time) {
	t.Helper()
	if _, err := db.UpsertPushOutcome(context.Background(), store.Outcome{
		Developer: dev, IssueID: "i-push-" + dev, Repo: repoAlpha,
		Weight: 0.5, WeightSource: store.WeightSourcePush, Quality: 1.0,
		Source: store.OutcomeSourcePush, Timestamp: ts,
	}, ts.UTC().Format("2006-01-02")); err != nil {
		t.Fatalf("UpsertPushOutcome(%s): %v", dev, err)
	}
}

const kanonWebhookSecret = "test-webhook-secret-856"

// deliverMergedPR runs a signed merged-PR delivery through the real webhook
// handler, so the stored author type is exactly what capture records.
func deliverMergedPR(t *testing.T, db *store.DB, login, userType string, pr int) {
	t.Helper()
	payload := map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number": pr, "merged": true, "body": fmt.Sprintf("closes #%d", pr),
			"merge_commit_sha": fmt.Sprintf("%040x", pr),
			"head":             map[string]any{"ref": "main"},
			"user":             map[string]any{"login": login, "type": userType},
		},
		"repository": map[string]any{"full_name": repoAlpha},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(kanonWebhookSecret))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("kanon-856-%d", pr))
	rec := httptest.NewRecorder()
	webhook.New(db, kanonWebhookSecret, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(rec, req)
	if rec.Code/100 != 2 {
		t.Fatalf("webhook delivery for %s: status %d, body %s", login, rec.Code, rec.Body.String())
	}
}

// healthyTeam seeds five complete people (roster, captured cost, captured
// outcome) in team: the control that must stay named.
func healthyTeam(t *testing.T, db *store.DB, team string, ts time.Time) {
	t.Helper()
	for i := 1; i <= 5; i++ {
		seedKAnonDev(t, db, team, fmt.Sprintf("%s-%d", team, i), 10, 2, ts)
	}
}

// uncountedActiveIDs reads data_quality.uncounted_active_ids from a raw
// /scores body; nil when the key is absent.
func uncountedActiveIDs(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	dq, _ := out["data_quality"].(map[string]any)
	u, _ := dq["uncounted_active_ids"].(map[string]any)
	return u
}

func assertUncounted(t *testing.T, raw []byte, reason string, want int) {
	t.Helper()
	u := uncountedActiveIDs(t, raw)
	if u == nil {
		t.Fatalf("data_quality.uncounted_active_ids absent; want %s=%d", reason, want)
	}
	if got, _ := u[reason].(float64); int(got) != want {
		t.Errorf("uncounted_active_ids.%s = %v, want %d (block %v)", reason, u[reason], want, u)
	}
}

// assertGroupWithheld is the shared verdict: the padded team holds fewer than k
// counted people, so it folds into a residual below k and the WHOLE response is
// withheld (#593, #864), the healthy control team "big" included. counted is how
// many counted people the declaration must report in that residual — the census's
// own count, which is what keeps this from passing against a build that withholds
// for any other reason or counts the padding (then the team is named and nothing
// is withheld).
func assertGroupWithheld(t *testing.T, resp scoresResponse, team string, counted int) {
	t.Helper()
	if len(resp.Teams) != 0 {
		t.Errorf("team %q cannot reach k, so the whole response must be withheld; got %v", team, teamJSONNames(resp.Teams))
	}
	if resp.Total != nil {
		t.Errorf("total published beside a withheld cohort: %+v", *resp.Total)
	}
	if resp.DataQuality == nil || resp.DataQuality.KAnonSuppressed == nil {
		t.Fatal("the withhold must be declared in data_quality.kanon_suppressed")
	}
	ks := resp.DataQuality.KAnonSuppressed
	if !ks.WithheldTeams || !ks.WithheldTotal {
		t.Errorf("declaration must say the rows and the total were withheld: %+v", ks)
	}
	if ks.Developers != counted {
		t.Errorf("kanon_suppressed.developers = %d, want %d counted people in %q's residual", ks.Developers, counted, team)
	}
}

func getScoresBody(t *testing.T, h *Handler, target string) (scoresResponse, []byte) {
	t.Helper()
	code, body := doRequest(t, h, http.MethodGet, target, nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s: status %d, body %s", target, code, body)
	}
	var resp scoresResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp, body
}

// (a) Two people plus three ids whose only rows are manual /costs rows.
func TestKAnonPeople_ManualCostOnlyIDsDoNotCount(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	healthyTeam(t, db, "big", now)
	seedKAnonDev(t, db, "eng", "p1", 10, 2, now)
	seedKAnonDev(t, db, "eng", "p2", 10, 2, now)
	for _, d := range []string{"fee1", "fee2", "fee3"} {
		rosterOnly(t, db, "eng", d)
		postManualCost(t, h, d, 4)
	}
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp, raw := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince())
	assertGroupWithheld(t, resp, "eng", 2)
	assertUncounted(t, raw, "manual_only", 3)
}

// (b) Two people plus three ids whose only rows are push-captured outcomes.
func TestKAnonPeople_PushOnlyIDsDoNotCount(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	healthyTeam(t, db, "big", now)
	seedKAnonDev(t, db, "eng", "p1", 10, 2, now)
	seedKAnonDev(t, db, "eng", "p2", 10, 2, now)
	for _, d := range []string{"push1", "push2", "push3"} {
		rosterOnly(t, db, "eng", d)
		seedPushOnlyOutcome(t, db, d, now)
	}
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp, raw := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince())
	assertGroupWithheld(t, resp, "eng", 2)
	assertUncounted(t, raw, "push_only", 3)
}

// (c) A residual of two rostered people plus three captured ids that are not
// on the roster.
func TestKAnonPeople_OffRosterIDsDoNotCount(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	healthyTeam(t, db, "big", now)
	seedKAnonDev(t, db, "small", "s1", 3, 1, now)
	seedKAnonDev(t, db, "small", "s2", 3, 1, now)
	for _, d := range []string{"ghost1", "ghost2", "ghost3"} {
		seedRepoCostAt(t, db, repoAlpha, d, "i-"+d, 3, now)
		seedRepoOutcomeAt(t, db, repoAlpha, d, "i-"+d, 1, now)
	}
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp, raw := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince())
	assertGroupWithheld(t, resp, "small", 2)
	assertUncounted(t, raw, "not_on_roster", 3)
}

// (d) Four people plus one bot, on the roster with captured activity. The bot
// is recognised three ways: GitHub user.type "Bot" captured by the webhook
// (a login with no [bot] suffix and not on the fixed list), the [bot] suffix,
// and the fixed list for rows captured before user.type was stored.
func TestKAnonPeople_BotsDoNotCount(t *testing.T) {
	cases := []struct {
		name  string
		login string
		seed  func(t *testing.T, db *store.DB, login string, now time.Time)
	}{
		{"user.type Bot, no suffix", "release-agent", func(t *testing.T, db *store.DB, login string, now time.Time) {
			deliverMergedPR(t, db, login, "Bot", 4242)
		}},
		{"[bot] suffix", "dependabot[bot]", func(t *testing.T, db *store.DB, login string, now time.Time) {
			seedRepoOutcomeAt(t, db, repoAlpha, login, "i-"+login, 1, now)
		}},
		{"fixed list, untyped old row", "Copilot", func(t *testing.T, db *store.DB, login string, now time.Time) {
			seedRepoOutcomeAt(t, db, repoAlpha, login, "i-"+login, 1, now)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, db := newTestHandler(t)
			now := time.Now().UTC()
			healthyTeam(t, db, "big", now)
			for _, d := range []string{"p1", "p2", "p3", "p4"} {
				seedKAnonDev(t, db, "eng", d, 10, 2, now)
			}
			rosterOnly(t, db, "eng", tc.login)
			seedRepoCostAt(t, db, repoAlpha, tc.login, "i-"+tc.login, 5, now)
			tc.seed(t, db, tc.login, now)
			h.SetAggregation(scoring.AggregationTeam, 5)

			resp, raw := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince())
			assertGroupWithheld(t, resp, "eng", 4)
			assertUncounted(t, raw, "bot", 1)
		})
	}
}

// (e) One person split across two ids (#856 case B): the collector's OS
// username and the GitHub login. Only the login is on the roster, so with no
// alias the OS username never counts. With an alias the two ids are one counted
// person, and the alias is what carries the cost evidence to the login.
func TestKAnonPeople_SplitIdentityCountsOnce(t *testing.T) {
	setup := func(t *testing.T) (*Handler, *store.DB) {
		h, db := newTestHandler(t)
		now := time.Now().UTC()
		healthyTeam(t, db, "big", now)
		for _, d := range []string{"s1", "s2", "s3"} {
			seedKAnonDev(t, db, "small", d, 3, 1, now)
		}
		rosterOnly(t, db, "small", "alice-gh")
		return h, db
	}

	t.Run("no alias: the unrostered id does not count", func(t *testing.T) {
		h, db := setup(t)
		now := time.Now().UTC()
		seedRepoOutcomeAt(t, db, repoAlpha, "alice-gh", "i-alice", 1, now)
		seedRepoCostAt(t, db, repoAlpha, "alice-gh", "i-alice", 3, now)
		seedRepoCostAt(t, db, repoAlpha, "alice-os", "i-alice-os", 3, now)
		// Three people plus alice-gh are four counted people; alice-os is her
		// second id, off the roster. Counting id strings made five.
		h.SetAggregation(scoring.AggregationTeam, 5)
		resp, raw := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince())
		assertGroupWithheld(t, resp, "small", 4)
		assertUncounted(t, raw, "not_on_roster", 1)
	})
	t.Run("alias: cost evidence merges through the alias", func(t *testing.T) {
		h, db := setup(t)
		now := time.Now().UTC()
		// alice-gh's outcome is push-only, so her only captured evidence is the
		// collector cost filed under alice-os, on the same issue so it joins the
		// outcome in the work-type segment too. alice-os holds its own row in
		// small, since each raw id is placed only by its own rows (#914).
		seedPushOnlyOutcome(t, db, "alice-gh", now)
		seedRepoCostAt(t, db, repoAlpha, "alice-os", "i-push-alice-gh", 3, now)
		rosterOnly(t, db, "small", "alice-os")
		h.SetAggregation(scoring.AggregationTeam, 4)
		// Control: unaliased, alice-os and alice-gh are two ids, and alice-gh's
		// push-only points have no counted carrier beside s1..s3.
		if resp, _ := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince()); teamPresent(resp.Teams, "small") {
			t.Fatalf("small published before the alias: points carried by 3 counted people at k=4")
		}
		if err := db.UpsertDeveloperAlias(context.Background(), "alice-os", "alice-gh", "test:fixture"); err != nil {
			t.Fatal(err)
		}
		resp, raw := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince())
		if !teamPresent(resp.Teams, "small") {
			t.Fatalf("aliased person must count once: small has 4 counted people at k=4; got %v", teamJSONNames(resp.Teams))
		}
		if resp.Total == nil {
			t.Errorf("nothing withheld, so total must be published; body %s", raw)
		}
		if u := uncountedActiveIDs(t, raw); u != nil {
			t.Errorf("every active id counted; uncounted_active_ids = %v", u)
		}
	})
}

// (f) E1, per-measure floor: five counted people of whom one has cost. The
// named row's cost is that one person's exact figure. Points likewise.
func TestKAnonPeople_PerMeasureFloor(t *testing.T) {
	t.Run("cost carried by one person", func(t *testing.T) {
		h, db := newTestHandler(t)
		now := time.Now().UTC()
		healthyTeam(t, db, "big", now)
		for _, d := range []string{"o1", "o2", "o3", "o4"} {
			rosterOnly(t, db, "eng", d)
			seedRepoOutcomeAt(t, db, repoAlpha, d, "i-"+d, 2, now)
		}
		rosterOnly(t, db, "eng", "spender")
		seedRepoCostAt(t, db, repoAlpha, "spender", "i-spender", 123.45, now)
		h.SetAggregation(scoring.AggregationTeam, 5)

		resp, _ := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince())
		for _, row := range resp.Teams {
			if math.Abs(row.TotalCostUSD-123.45) < 1e-9 {
				t.Errorf("row %q publishes one person's exact cost $123.45", row.Team)
			}
		}
		assertGroupWithheld(t, resp, "eng", 5)
	})
	t.Run("points carried by one person", func(t *testing.T) {
		h, db := newTestHandler(t)
		now := time.Now().UTC()
		healthyTeam(t, db, "big", now)
		for _, d := range []string{"c1", "c2", "c3", "c4"} {
			rosterOnly(t, db, "eng", d)
			seedRepoCostAt(t, db, repoAlpha, d, "i-"+d, 10, now)
		}
		rosterOnly(t, db, "eng", "shipper")
		seedRepoOutcomeAt(t, db, repoAlpha, "shipper", "i-shipper", 7, now)
		h.SetAggregation(scoring.AggregationTeam, 5)

		resp, _ := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince())
		for _, row := range resp.Teams {
			if math.Abs(row.WeightedPoints-7) < 1e-9 {
				t.Errorf("row %q publishes one person's exact points 7", row.Team)
			}
		}
		assertGroupWithheld(t, resp, "eng", 5)
	})
}

// (g) ...and the two-window compare path, on each window's own evidence.
func TestKAnonPeople_ComparePathUsesTheSamePredicate(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	aTS := now.AddDate(0, 0, -10)
	for _, ts := range []time.Time{aTS, now} {
		for i := 1; i <= 5; i++ {
			d := fmt.Sprintf("big-%d", i)
			seedKAnonDev(t, db, "big", d, 10, 2, ts)
		}
		seedKAnonDev(t, db, "eng", "p1", 10, 2, ts)
		seedKAnonDev(t, db, "eng", "p2", 10, 2, ts)
	}
	for _, d := range []string{"fee1", "fee2", "fee3"} {
		rosterOnly(t, db, "eng", d)
		postManualCost(t, h, d, 4) // server-stamped now: window B
		// Window A: the same ids with manual rows only, written at aTS.
		if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
			Developer: d, IssueID: "fee-a-" + d, Model: "claude-sonnet-4",
			CostMicro: store.DollarsToMicro(4), Source: "api", Fidelity: "daily", Timestamp: aTS,
		}); err != nil {
			t.Fatal(err)
		}
	}
	h.SetAggregation(scoring.AggregationTeam, 5)

	q := fmt.Sprintf("/api/v1/scores/compare?since_a=%s&until_a=%s&since_b=%s",
		aTS.AddDate(0, 0, -1).Format("2006-01-02"), now.AddDate(0, 0, -2).Format("2006-01-02"),
		now.AddDate(0, 0, -1).Format("2006-01-02"))
	code, resp := getCompare(t, h, q)
	if code != http.StatusOK {
		t.Fatalf("compare status %d", code)
	}
	// #864: eng's residual is below k in each window, so the whole comparison is
	// withheld, "big" included; the declared count is eng's 2 counted people.
	if len(resp.Teams) != 0 {
		t.Errorf("compare publishes rows beside eng's withheld residual: %+v", resp.Teams)
	}
	if resp.Total != nil {
		t.Errorf("compare total published beside a withheld cohort: %+v", *resp.Total)
	}
	if ks := resp.KAnonSuppressed; ks == nil || ks.Developers != 2 || !ks.WithheldTeams {
		t.Errorf("compare must declare the withhold of eng's 2 counted people; got %+v", ks)
	}
}

// (h) uncounted_active_ids is stated once per window, org-wide: exactly one
// occurrence in a /scores body, the same value under ?team= (?work_type= is
// refused in anonymised modes, #864),
// never inside a row, a team or a segment, and never on /compare.
func TestKAnonPeople_UncountedActiveIDsOncePerWindow(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	healthyTeam(t, db, "big", now)
	seedKAnonDev(t, db, "eng", "p1", 10, 2, now)
	for _, d := range []string{"fee1", "fee2"} {
		rosterOnly(t, db, "eng", d)
		postManualCost(t, h, d, 4)
	}
	seedPushOnlyOutcome(t, db, "push1", now)
	rosterOnly(t, db, "eng", "push1")
	seedRepoCostAt(t, db, repoAlpha, "ghost", "i-ghost", 3, now)
	// Reason precedence: a bot off the roster is a bot, and a rostered id with
	// both manual and push rows is manual_only.
	seedRepoCostAt(t, db, repoAlpha, "renovate[bot]", "i-renovate", 3, now)
	rosterOnly(t, db, "eng", "mixed")
	postManualCost(t, h, "mixed", 4)
	seedPushOnlyOutcome(t, db, "mixed", now)
	// eng's p1 and the manual rows sit in "other"; the padding (with captured
	// non-realtime cost, since the manual rows put a figure in that share) lifts
	// it to k so the rows publish and "never inside a row or a team" is checked
	// against rows that exist (#864).
	padResidual(t, db, 5, now)
	for i := 1; i <= 5; i++ {
		if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
			Developer: fmt.Sprintf("pad-%d", i), IssueID: fmt.Sprintf("i-pad-%d-est", i), Model: "claude-sonnet-4",
			InputTok: 2000, CostMicro: store.DollarsToMicro(1), Source: "jsonl", Fidelity: "estimated", Timestamp: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	h.SetAggregation(scoring.AggregationTeam, 5)

	var first map[string]any
	for _, q := range []string{"", "&team=eng", "&team=big"} {
		_, raw := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince()+q)
		var published scoresResponse
		if err := json.Unmarshal(raw, &published); err != nil || len(published.Teams) < 2 {
			t.Fatalf("%q: control: big and other must publish; body %s", q, raw)
		}
		if n := strings.Count(string(raw), `"uncounted_active_ids"`); n != 1 {
			t.Fatalf("%q: uncounted_active_ids appears %d times, want exactly once; body %s", q, n, raw)
		}
		u := uncountedActiveIDs(t, raw)
		if u == nil {
			t.Fatalf("%q: uncounted_active_ids is not under data_quality", q)
		}
		want := map[string]any{"manual_only": 3.0, "push_only": 1.0, "not_on_roster": 1.0, "bot": 1.0}
		for k, v := range want {
			if u[k] != v {
				t.Errorf("%q: uncounted_active_ids.%s = %v, want %v", q, k, u[k], v)
			}
		}
		if first == nil {
			first = u
		} else if fmt.Sprint(first) != fmt.Sprint(u) {
			t.Errorf("%q: the org-wide count moved with the filter: %v vs %v", q, u, first)
		}
	}

	code, raw := doRequest(t, h, http.MethodGet, fmt.Sprintf("/api/v1/scores/compare?since_a=%s&until_a=%s&since_b=%s",
		now.AddDate(0, 0, -20).Format("2006-01-02"), now.AddDate(0, 0, -10).Format("2006-01-02"),
		now.AddDate(0, 0, -1).Format("2006-01-02")), nil)
	if code != http.StatusOK {
		t.Fatalf("compare status %d", code)
	}
	if strings.Contains(string(raw), "uncounted_active_ids") {
		t.Errorf("uncounted_active_ids must never appear per /compare window; body %s", raw)
	}
}

// The roster is its own flag, never read off the group label: at division
// level a rostered person with no division sits in the "" group, which folds
// into the residual, and must still count there. Five such people form a
// publishable residual and no id is reported uncounted.
func TestKAnonPeople_RosterFlagIsNotInferredFromDivision(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	for i := 1; i <= 5; i++ {
		d := fmt.Sprintf("nodiv-%d", i)
		if err := baselineHierarchy(db, context.Background(), d, "eng", "", "acme"); err != nil {
			t.Fatal(err)
		}
		seedRepoCostAt(t, db, repoAlpha, d, "i-"+d, 10, now)
		seedRepoOutcomeAt(t, db, repoAlpha, d, "i-"+d, 2, now)
	}
	h.SetAggregation(scoring.AggregationDivision, 5)

	resp, raw := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince())
	if !teamPresent(resp.Teams, scoring.OtherCohort) {
		t.Errorf("five rostered people with no division must publish as the residual; got %v, kanon %+v",
			teamJSONNames(resp.Teams), resp.DataQuality)
	}
	if u := uncountedActiveIDs(t, raw); u != nil {
		t.Errorf("no id should be uncounted; got %v", u)
	}
}

// assertNoFigure fails when any published team row or the total carries want in
// the field pick reads.
func assertNoFigure(t *testing.T, resp scoresResponse, what string, want float64, pick func(teamScoreJSON) float64) {
	t.Helper()
	rows := append([]teamScoreJSON(nil), resp.Teams...)
	if resp.Total != nil {
		rows = append(rows, *resp.Total)
	}
	for _, row := range rows {
		if math.Abs(pick(row)-want) < 1e-6 {
			t.Errorf("row %q publishes %s %v, one id's exact figure", row.Team, what, want)
		}
	}
}

// (i) E1 for paid spend: five counted people of whom one holds an invoice. The
// row's actual_paid_usd would be that person's exact paid figure, and
// spend_leverage divides by it.
func TestKAnonPeople_PaidSpendNeedsKPayers(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	healthyTeam(t, db, "big", now)
	for _, d := range []string{"p1", "p2", "p3", "p4", "p5"} {
		seedKAnonDev(t, db, "eng", d, 10, 2, now)
	}
	if err := db.InsertActualSpend(context.Background(), store.ActualSpend{
		Developer: "p1", Period: now.Format("2006-01"),
		ActualPaidMicro: store.DollarsToMicro(777.25), Timestamp: now,
	}); err != nil {
		t.Fatal(err)
	}
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp, raw := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince())
	assertNoFigure(t, resp, "actual_paid_usd", 777.25, func(r teamScoreJSON) float64 { return r.ActualPaidUSD })
	if strings.Contains(string(raw), "777.25") {
		t.Errorf("the one payer's paid figure appears in the body: %s", raw)
	}
	assertGroupWithheld(t, resp, "eng", 5)
}

// (j) coverage_pct splits a row's cost into its realtime and non-realtime
// shares, so each non-zero share is a figure of its own. Five counted people
// with realtime cost plus one uncounted id's $4 manual fee: total × (1 −
// coverage/100) is that $4.
func TestKAnonPeople_CoverageSharesNeedKCarriers(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	healthyTeam(t, db, "big", now)
	for _, d := range []string{"p1", "p2", "p3", "p4", "p5"} {
		seedKAnonDev(t, db, "eng", d, 10, 2, now)
	}
	rosterOnly(t, db, "eng", "fee")
	postManualCost(t, h, "fee", 4)
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp, raw := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince())
	assertNoFigure(t, resp, "a non-realtime share of", 4, func(r teamScoreJSON) float64 {
		return r.TotalCostUSD * (1 - r.CoveragePercent/100)
	})
	assertGroupWithheld(t, resp, "eng", 5)
	assertUncounted(t, raw, "manual_only", 1)
}

// (g) ...and each /compare window is counted on ITS OWN census. Three ids are
// full people in one window and push-only in the other, where eng carries no
// cost, so their only way to a seat there is the other window's census.
// Manual-only ids cannot play this part: the per-measure floor withholds any
// window whose cost they carry, whichever census judged them.
func TestKAnonPeople_CompareCountsEachWindowOnItsOwnEvidence(t *testing.T) {
	for _, pushOnlyIn := range []string{"A", "B"} {
		t.Run("push-only in window "+pushOnlyIn, func(t *testing.T) {
			h, db := newTestHandler(t)
			now := time.Now().UTC()
			aTS := now.AddDate(0, 0, -10)
			full, thin := aTS, now
			if pushOnlyIn == "A" {
				full, thin = now, aTS
			}
			for w, ts := range []time.Time{aTS, now} {
				for i := 1; i <= 5; i++ {
					d := fmt.Sprintf("big-%d", i)
					seedKAnonDevIssue(t, db, "big", d, fmt.Sprintf("i-%s-%d", d, w), 10, 2, ts)
				}
			}
			for _, d := range []string{"p1", "p2", "x1", "x2", "x3"} {
				seedKAnonDevIssue(t, db, "eng", d, "i-"+d+"-full", 10, 2, full)
			}
			for _, d := range []string{"p1", "p2"} {
				seedRepoOutcomeAt(t, db, repoAlpha, d, "i-"+d+"-thin", 2, thin)
			}
			for _, d := range []string{"x1", "x2", "x3"} {
				seedPushOnlyOutcome(t, db, d, thin)
			}
			h.SetAggregation(scoring.AggregationTeam, 5)

			q := fmt.Sprintf("/api/v1/scores/compare?since_a=%s&until_a=%s&since_b=%s",
				aTS.AddDate(0, 0, -1).Format("2006-01-02"), now.AddDate(0, 0, -2).Format("2006-01-02"),
				now.AddDate(0, 0, -1).Format("2006-01-02"))
			code, resp := getCompare(t, h, q)
			if code != http.StatusOK {
				t.Fatalf("compare status %d", code)
			}
			// #864: eng is below k in the push-only window, so the whole comparison
			// is withheld. Judged on the other window's census, x1..x3 would count
			// there, eng would clear k in both windows and be named.
			if len(resp.Teams) != 0 {
				t.Errorf("compare publishes rows with eng at 2 counted people in window %s: %+v", pushOnlyIn, resp.Teams)
			}
			if resp.Total != nil {
				t.Errorf("compare total published beside a withheld cohort: %+v", *resp.Total)
			}
			// developers is the larger side's count: eng's five in the full window.
			if ks := resp.KAnonSuppressed; ks == nil || !ks.WithheldTeams || ks.Developers != 5 {
				t.Errorf("compare must declare the withhold with the larger side's count, 5; got %+v", ks)
			}
		})
	}
}

// (d) A bot's typed author row is recognised through the alias map: the PR
// arrives under the bot login, which is an alias of the id on the roster.
func TestKAnonPeople_AliasedTypedBotDoesNotCount(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	healthyTeam(t, db, "big", now)
	for _, d := range []string{"p1", "p2", "p3", "p4"} {
		seedKAnonDev(t, db, "eng", d, 10, 2, now)
	}
	deliverMergedPR(t, db, "release-agent", "Bot", 4243)
	if err := db.UpsertDeveloperAlias(context.Background(), "release-agent", "agent-canon", "test:fixture"); err != nil {
		t.Fatal(err)
	}
	rosterOnly(t, db, "eng", "agent-canon")
	seedRepoCostAt(t, db, repoAlpha, "agent-canon", "issue-4243", 5, now)
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp, raw := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince())
	assertGroupWithheld(t, resp, "eng", 4)
	assertUncounted(t, raw, "bot", 1)
}

// (f) A manual /costs row fills no cost seat. Four people whose cost is a known
// $20 manual fee each plus one person with captured (non-realtime) cost: the
// row's cost minus the fees is that one person's figure.
func TestKAnonPeople_ManualCostFillsNoCostSeat(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	healthyTeam(t, db, "big", now)
	for _, d := range []string{"f1", "f2", "f3", "f4", "cap"} {
		rosterOnly(t, db, "eng", d)
		seedRepoOutcomeAt(t, db, repoAlpha, d, "i-"+d, 2, now)
	}
	for _, d := range []string{"f1", "f2", "f3", "f4"} {
		postManualCost(t, h, d, 20)
	}
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: "cap", IssueID: "i-cap", Repo: repoAlpha, Model: "claude-sonnet-4",
		InputTok: 2000, CostMicro: store.DollarsToMicro(13.37),
		Source: "anthropic-admin", Fidelity: "daily", Timestamp: now,
	}); err != nil {
		t.Fatal(err)
	}
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp, _ := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince())
	assertNoFigure(t, resp, "cost net of the known fees", 13.37, func(r teamScoreJSON) float64 { return r.TotalCostUSD - 80 })
	assertGroupWithheld(t, resp, "eng", 5)
}

// Roster membership is dated to the window: an id first put on the roster
// after a window ended was not on it during that window.
func TestKAnonPeople_RosterIsDatedToTheWindow(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	past := now.AddDate(0, 0, -15)
	healthyTeam(t, db, "big", past)
	for _, d := range []string{"p1", "p2", "p3", "p4"} {
		seedKAnonDev(t, db, "eng", d, 10, 2, past)
	}
	if err := db.UpsertHierarchy(context.Background(), "late", "eng", "div-eng", "acme", "test:fixture"); err != nil {
		t.Fatal(err)
	}
	seedRepoCostAt(t, db, repoAlpha, "late", "i-late", 10, past)
	seedRepoOutcomeAt(t, db, repoAlpha, "late", "i-late", 2, past)
	h.SetAggregation(scoring.AggregationTeam, 5)

	q := "/api/v1/scores?since=" + now.AddDate(0, 0, -20).Format("2006-01-02") + "&until=" + now.AddDate(0, 0, -10).Format("2006-01-02")
	resp, raw := getScoresBody(t, h, q)
	assertGroupWithheld(t, resp, "eng", 4)
	assertUncounted(t, raw, "not_on_roster", 1)
}

// A bot's typed PR merged today does not change how a past window counted.
func TestKAnonPeople_BotPRTodayDoesNotChangeAPastWindow(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	past := now.AddDate(0, 0, -15)
	healthyTeam(t, db, "big", past)
	for _, d := range []string{"p1", "p2", "p3", "p4", "release-agent"} {
		seedKAnonDev(t, db, "eng", d, 10, 2, past)
	}
	h.SetAggregation(scoring.AggregationTeam, 5)
	q := "/api/v1/scores?since=" + now.AddDate(0, 0, -20).Format("2006-01-02") + "&until=" + now.AddDate(0, 0, -10).Format("2006-01-02")

	before, _ := getScoresBody(t, h, q)
	if !teamPresent(before.Teams, "eng") {
		t.Fatalf("eng must be named before the bot PR (non-vacuous); got %v", teamJSONNames(before.Teams))
	}
	_, beforeRaw := getScoresBody(t, h, q)
	deliverMergedPR(t, db, "release-agent", "Bot", 4244)
	if _, afterRaw := getScoresBody(t, h, q); !bytes.Equal(beforeRaw, afterRaw) {
		t.Errorf("a past window changed after a bot PR merged today:\nbefore %s\nafter  %s", beforeRaw, afterRaw)
	}
}

// The unattributed pseudo-developer is never reported as an uncounted active
// id: it is not a person the operator could put on the roster. Since #864 D′ its
// rows leave the anonymised window before the census reads it.
func TestKAnonPeople_PseudoDeveloperIsNotReportedUncounted(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	for _, d := range []string{"x1", "x2", "x3"} {
		seedKAnonDev(t, db, "t1", d, 10, 2, now)
	}
	for _, d := range []string{"y1", "y2"} {
		seedKAnonDev(t, db, "t2", d, 10, 2, now)
	}
	seedPollerRemainder(t, db, 12.34, now)
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp, raw := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince())
	if other := teamByName(resp.Teams, scoring.OtherCohort); other == nil || math.Abs(other.TotalCostUSD-50) > 1e-9 {
		t.Fatalf("the residual must publish the five people's $50 without the pseudo-developer's $12.34; got %v", teamJSONNames(resp.Teams))
	}
	if u := uncountedActiveIDs(t, raw); u != nil {
		t.Errorf("uncounted_active_ids = %v, want the key absent: unattributed is excluded", u)
	}
}

// A bot login recognised only by its spelling (an outcome with no captured
// user.type) is still a bot when it is an alias of an ordinary id: the
// spelling is read on the raw id that carries the activity, not on the
// canonical id the alias map folds it into.
func TestKAnonPeople_AliasedBotSpellingDoesNotCount(t *testing.T) {
	for _, login := range []string{"dependabot[bot]", "Copilot"} {
		t.Run(login, func(t *testing.T) {
			h, db := newTestHandler(t)
			now := time.Now().UTC()
			healthyTeam(t, db, "big", now)
			for _, d := range []string{"p1", "p2", "p3", "p4"} {
				seedKAnonDev(t, db, "eng", d, 10, 2, now)
			}
			seedRepoOutcomeAt(t, db, repoAlpha, login, "i-bot", 2, now)
			if err := db.UpsertDeveloperAlias(context.Background(), login, "release-agent", "test:fixture"); err != nil {
				t.Fatal(err)
			}
			rosterOnly(t, db, "eng", "release-agent")
			seedRepoCostAt(t, db, repoAlpha, "release-agent", "i-bot", 5, now)
			h.SetAggregation(scoring.AggregationTeam, 5)

			resp, raw := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince())
			assertGroupWithheld(t, resp, "eng", 4)
			assertUncounted(t, raw, "bot", 1)
		})
	}
}

// Each coverage share needs k carriers with CAPTURED cost in THAT share. Five
// people with captured realtime spend; four of them hold a known $20 manual
// fee (non-realtime) and the fifth $13.37 of captured daily spend. The
// non-realtime share net of the fees is the fifth person's exact figure.
func TestKAnonPeople_ShareCarriersNeedCapturedCostInThatShare(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	healthyTeam(t, db, "big", now)
	for _, d := range []string{"f1", "f2", "f3", "f4", "cap"} {
		seedKAnonDev(t, db, "eng", d, 10, 2, now)
	}
	for _, d := range []string{"f1", "f2", "f3", "f4"} {
		postManualCost(t, h, d, 20)
	}
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: "cap", IssueID: "i-cap", Repo: repoAlpha, Model: "claude-sonnet-4",
		InputTok: 2000, CostMicro: store.DollarsToMicro(13.37),
		Source: "anthropic-admin", Fidelity: "daily", Timestamp: now,
	}); err != nil {
		t.Fatal(err)
	}
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp, _ := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince())
	assertNoFigure(t, resp, "a non-realtime share net of the known fees of", 13.37, func(r teamScoreJSON) float64 {
		return r.TotalCostUSD*(1-r.CoveragePercent/100) - 80
	})
	assertGroupWithheld(t, resp, "eng", 5)
}

// The realtime share also needs k carriers with CAPTURED realtime cost. p1..p4
// have captured realtime and daily cost; p5 has captured daily cost and only a
// legacy manual realtime row (source='api', fidelity='realtime', the pre-#82
// shape POST /costs now refuses). The realtime share has four captured
// carriers, so eng is withheld.
func TestKAnonPeople_RealtimeShareCarriersNeedCapturedRealtimeCost(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	healthyTeam(t, db, "big", now)
	ins := func(dev, source, fidelity string, costUSD float64) {
		t.Helper()
		if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
			Developer: dev, IssueID: "i-" + dev + "-" + source, Repo: repoAlpha, Model: "claude-sonnet-4",
			InputTok: 2000, CostMicro: store.DollarsToMicro(costUSD),
			Source: source, Fidelity: fidelity, Timestamp: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"p1", "p2", "p3", "p4"} {
		seedKAnonDev(t, db, "eng", d, 10, 2, now)
		ins(d, "anthropic-admin", "daily", 3)
	}
	rosterOnly(t, db, "eng", "p5")
	seedRepoOutcomeAt(t, db, repoAlpha, "p5", "i-p5", 2, now)
	ins("p5", "anthropic-admin", "daily", 3)
	ins("p5", "api", "realtime", 20)
	h.SetAggregation(scoring.AggregationTeam, 5)

	resp, _ := getScoresBody(t, h, "/api/v1/scores?since="+scoresSince())
	assertGroupWithheld(t, resp, "eng", 5)
}

// A share's carriers are judged per published group, not per person-window
// (#943). mover has captured non-realtime cost in alpha before a mid-window move
// to beta, and in beta only a known $20 manual fee on top of captured realtime
// cost. beta's non-realtime share is then b1..b4's captured cost plus the fee:
// four real carriers, so beta cannot reach k and the response is withheld. The
// control arm gives mover captured non-realtime cost in beta instead, and beta
// is published.
func TestKAnonPeople_ShareCarriersAreJudgedPerGroup(t *testing.T) {
	for _, feeInBeta := range []bool{true, false} {
		t.Run(fmt.Sprintf("fee in beta=%v", feeInBeta), func(t *testing.T) {
			h, db := newTestHandler(t)
			h.SetAggregation(scoring.AggregationTeam, 5)
			c := newDatedClock()
			daily := func(dev, issue string, costUSD float64, ts time.Time) {
				t.Helper()
				if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
					Developer: dev, IssueID: issue, Model: "claude-sonnet-4",
					InputTok: 2000, CostMicro: store.DollarsToMicro(costUSD),
					Source: "anthropic-admin", Fidelity: "daily", Timestamp: ts,
				}); err != nil {
					t.Fatal(err)
				}
			}
			for _, team := range []string{"alpha", "beta"} {
				devs := []string{team[:1] + "1", team[:1] + "2", team[:1] + "3", team[:1] + "4"}
				if team == "alpha" {
					devs = append(devs, "mover")
				}
				for _, d := range devs {
					if err := baselineHierarchy(db, context.Background(), d, team, "div-"+team, "acme"); err != nil {
						t.Fatal(err)
					}
					seedCostAt(t, db, d, "i-"+d, 10, c.past)
					seedOutcomeAt(t, db, d, "i-"+d, 2, 1, c.past)
					daily(d, "i-"+d, 3, c.past)
				}
			}

			moveDeveloper(t, h, "mover", "beta", "div-beta")
			post := time.Now().UTC().Add(time.Millisecond)
			seedCostAt(t, db, "mover", "i-mover-post", 10, post)
			seedOutcomeAt(t, db, "mover", "i-mover-post", 2, 1, post)
			if feeInBeta {
				postManualCost(t, h, "mover", 20)
			} else {
				daily("mover", "i-mover-post", 3, post)
			}

			resp, _ := getScoresBody(t, h, c.query(c.spanEnd))
			if !feeInBeta {
				if got := teamJSONNames(resp.Teams); len(got) != 2 {
					t.Fatalf("with captured non-realtime cost in beta, alpha and beta both clear k; got %v", got)
				}
				return
			}
			assertGroupWithheld(t, resp, "beta", 5)
		})
	}
}
