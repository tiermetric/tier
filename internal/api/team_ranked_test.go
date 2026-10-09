package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"testing"
	"time"
)

// totalRaw pulls the "total" block out of a /scores response as raw keys, so a
// test can tell an ABSENT field from one that decoded to its zero value. For a
// bool that distinction is the whole assertion: `ranked: false` and "no ranked
// key at all" both unmarshal into false, and only one of them is the contract.
func totalRaw(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var resp struct {
		Total map[string]json.RawMessage `json:"total"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal /scores: %v (body=%s)", err, body)
	}
	if resp.Total == nil {
		t.Fatalf("no total block in /scores response: %s", body)
	}
	return resp.Total
}

func rawFloat(t *testing.T, m map[string]json.RawMessage, key string) float64 {
	t.Helper()
	raw, ok := m[key]
	if !ok {
		t.Fatalf("total has no %q key: %v", key, m)
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode total.%s: %v", key, err)
	}
	return f
}

// assertNothingFlagged pins the FIXTURE claim that these tests rest on: the
// developer rows carry no zero-token outcomes, so `ranked` is decided by the two
// #133 floors and nothing else. It is asserted rather than asserted-in-prose
// because the tripwire fires on an easy fixture slip — the attributable window
// ends AT the outcome's timestamp, so cost seeded even a microsecond later lands
// outside it and flags every outcome. The first draft of this file did exactly
// that, and the below-floor test still passed while proving the wrong thing.
func assertNothingFlagged(t *testing.T, body []byte) {
	t.Helper()
	var resp struct {
		Developers []struct {
			Developer string `json:"developer"`
			Flagged   int    `json:"flagged_outcomes"`
			SampleN   int    `json:"sample_n"`
		} `json:"developers"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal developers: %v", err)
	}
	if len(resp.Developers) == 0 {
		t.Fatal("no developer rows: the fixture never reached the scoring path")
	}
	for _, d := range resp.Developers {
		if d.Flagged != 0 {
			t.Fatalf("fixture broken: %s has %d zero-token-flagged outcomes of %d, so `ranked` "+
				"is decided by the #136 tripwire and this test no longer measures the #133 floors",
				d.Developer, d.Flagged, d.SampleN)
		}
	}
}

// TestGetScores_TotalCarriesRankedBelowTheFloor is the wire half of #502. The org
// rollup that #502 was filed over — 28 weighted points against $0.0001 of measured
// AI spend — must reach the dashboard declared UNRANKED, and must reach it still
// carrying its true TIER.
//
// Both halves are asserted deliberately:
//   - ranked:false is the field the KPI tile gates its headline on. It must be
//     PRESENT, not omitted, or "unranked" is indistinguishable from "an older
//     server that never said" — the exact ambiguity behind #603.
//   - tier is UNCHANGED at 2.8e8. The house rule from #136 is that the number is
//     never altered, only its ranking authority revoked (#502 acceptance criterion
//     2). A fix that scrubbed, floored or omitted the quotient here would be a
//     different — and rejected — option.
func TestGetScores_TotalCarriesRankedBelowTheFloor(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	// Two outcomes at weight 14 => 28 weighted points, against $0.0001 total spend
	// split across their two issues. Cost is seeded AT the outcome instant so each
	// issue keeps > MinAttributableTokens inside its attributable window: the row is
	// below the floor for SPEND and OUTCOME COUNT, never because of the #136
	// zero-token tripwire (pinned by assertNothingFlagged).
	seedOutcomeAt(t, db, "alice", "issue-1", 14.0, 1.0, now)
	seedOutcomeAt(t, db, "alice", "issue-2", 14.0, 1.0, now)
	seedCostAt(t, db, "alice", "issue-1", 0.00005, now)
	seedCostAt(t, db, "alice", "issue-2", 0.00005, now)

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", code, body)
	}
	assertNothingFlagged(t, body)
	total := totalRaw(t, body)

	raw, ok := total["ranked"]
	if !ok {
		t.Fatalf("total block carries no `ranked` key (#502): %s", body)
	}
	var ranked bool
	if err := json.Unmarshal(raw, &ranked); err != nil {
		t.Fatalf("decode total.ranked: %v", err)
	}
	if ranked {
		t.Errorf("total.ranked = true for %.1f points against $%.4f of spend; both the outcome "+
			"count and the spend are below the #133 floor",
			rawFloat(t, total, "weighted_points"), rawFloat(t, total, "total_cost_usd"))
	}

	// Criterion 2: the wire still carries the true quotient.
	const wantTIER = 28.0 / (0.0001 / 1000.0) // 2.8e8
	if got := rawFloat(t, total, "tier"); math.Abs(got-wantTIER) > 1.0 {
		t.Errorf("total.tier = %v, want %v — an unranked aggregate keeps its exact number; "+
			"only its ranking authority is withheld (#136)", got, wantTIER)
	}
	// And the measured inputs the tile keeps on screen are the real ones.
	if got := rawFloat(t, total, "weighted_points"); math.Abs(got-28.0) > 1e-9 {
		t.Errorf("total.weighted_points = %v, want 28", got)
	}
	if got := rawFloat(t, total, "total_cost_usd"); math.Abs(got-0.0001) > 1e-9 {
		t.Errorf("total.total_cost_usd = %v, want 0.0001", got)
	}
}

// TestGetScores_TotalRankedAboveTheFloor is the paired arm: an org that clears
// both floors must come back ranked:true. Without it, a mapper that hardcoded
// false — or an engine gate that never returned true — would satisfy the test
// above and silently withhold every headline the dashboard should publish.
func TestGetScores_TotalRankedAboveTheFloor(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	for _, issue := range []string{"issue-1", "issue-2", "issue-3"} {
		seedOutcomeAt(t, db, "alice", issue, 2.0, 1.0, now)
		seedCostAt(t, db, "alice", issue, 4.0, now) // $12 total, clear of MinRankedCostUSD
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", code, body)
	}
	assertNothingFlagged(t, body)
	total := totalRaw(t, body)
	var ranked bool
	if err := json.Unmarshal(total["ranked"], &ranked); err != nil {
		t.Fatalf("decode total.ranked: %v", err)
	}
	if !ranked {
		t.Errorf("total.ranked = false for %.1f points, 3 outcomes and $%.2f of spend — "+
			"both floors are cleared and nothing is flagged",
			rawFloat(t, total, "weighted_points"), rawFloat(t, total, "total_cost_usd"))
	}
}

// teamsRaw pulls the anonymized `teams[]` rows out of a /scores response as raw
// key maps, for the same reason totalRaw exists: `ranked: false` and "no ranked
// key at all" both unmarshal into false, and only one of them is the contract.
func teamsRaw(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	var resp struct {
		Teams []map[string]json.RawMessage `json:"teams"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal /scores: %v (body=%s)", err, body)
	}
	if len(resp.Teams) == 0 {
		t.Fatalf("no teams[] rows in the anonymized /scores response: %s", body)
	}
	return resp.Teams
}

func rawTeamNamed(t *testing.T, rows []map[string]json.RawMessage, name string) map[string]json.RawMessage {
	t.Helper()
	for _, row := range rows {
		// The key must be PRESENT. `var got string` outside this guard would let a
		// row with no "team" key silently match a lookup for "". No such row reaches
		// teams[] today — AggregateTeamsKAnon folds the empty label into "other" and
		// the unnamed rollup lives in `total` — but teamScoreJSON.Team IS omitempty,
		// so the shape permits it and a lookup must not depend on it not happening.
		raw, ok := row["team"]
		if !ok {
			continue
		}
		var got string
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode team name: %v", err)
		}
		if got == name {
			return row
		}
	}
	// %s, not %v: a json.RawMessage is a []byte, and %v prints it as a decimal byte
	// array — unreadable at exactly the moment the diagnostic is needed.
	t.Fatalf("no team row named %q in %s", name, rawRowsString(rows))
	return nil
}

// rawRowsString renders raw JSON rows readably for a failure message.
func rawRowsString(rows []map[string]json.RawMessage) string {
	out, err := json.Marshal(rows)
	if err != nil {
		return fmt.Sprintf("%d rows (unprintable: %v)", len(rows), err)
	}
	return string(out)
}

func rawBool(t *testing.T, m map[string]json.RawMessage, key string) bool {
	t.Helper()
	raw, ok := m[key]
	if !ok {
		t.Fatalf("row has no %q key: %s", key, rawRowsString([]map[string]json.RawMessage{m}))
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	return b
}

// TestGetScores_KAnonTeamRowCarriesRanked covers #603's OWN surface — the
// k-anonymized `teams[]` rows, the exact rows the issue says "rendered as
// ranked-green evidence at any cost level".
//
// Every other ranked assertion on this branch lands on the `total` block or on
// engine-level TeamScore values. Forcing `ranked = true` at the anonymized teams
// build site therefore survived the entire suite: nothing read the field HERE.
// The dashboard's #603 fix reads exactly this field on exactly these rows, so a
// server that stopped populating it would silently restore the defect the fix
// removed, with the client failing closed to "unranked" and no test objecting.
//
// Both arms are asserted, because a mapper hardcoded to false would satisfy the
// below-floor arm alone.
func TestGetScores_KAnonTeamRowCarriesRanked(t *testing.T) {
	h, db := newTeamModeHandler(t, 3)
	// "thin": 3 contributing developers, so the row is named (k=3) — but $0.30 of
	// summed spend, far below MinRankedCostUSD. The outcome count clears (3 >= 3),
	// so the SPEND floor is what decides, and nothing is zero-token flagged.
	for _, dev := range []string{"t1", "t2", "t3"} {
		seedTeamMember(t, db, dev, "i-"+dev, "thin", 0.10, 3)
	}
	// "solid": 3 contributing developers, $6 each — clear of both floors.
	for _, dev := range []string{"s1", "s2", "s3"} {
		seedTeamMember(t, db, dev, "i-"+dev, "solid", 6.0, 3)
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", code, body)
	}
	// Pin the FIXTURE claim this test rests on, the anonymized-mode equivalent of
	// assertNothingFlagged (which cannot be used here: team mode emits no developer
	// rows, so it would Fatal on an empty list). Without it, a future change that
	// flags outcomes on thin-spend issues would keep `thin.ranked` false for the
	// #136 zero-token reason while this test still passed — measuring the tripwire
	// instead of the #133 spend floor it claims to measure. The key is omitempty, so
	// its presence at all means something was flagged.
	var dq struct {
		DataQuality map[string]json.RawMessage `json:"data_quality"`
	}
	if err := json.Unmarshal(body, &dq); err != nil {
		t.Fatalf("unmarshal data_quality: %v", err)
	}
	if raw, ok := dq.DataQuality["zero_token_outcome_count"]; ok {
		t.Fatalf("fixture broken: the window has zero-token-flagged outcomes (%s), so `ranked` "+
			"is decided by the #136 tripwire and this test no longer measures the #133 floors",
			raw)
	}

	rows := teamsRaw(t, body)

	thin := rawTeamNamed(t, rows, "thin")
	if _, ok := thin["ranked"]; !ok {
		t.Fatalf("k-anon team row carries no `ranked` key (#603) — the dashboard cannot tell "+
			"unranked from an older server that never said: %s", body)
	}
	if rawBool(t, thin, "ranked") {
		t.Errorf("teams[thin].ranked = true on $%.2f of summed spend; the #133 spend floor is "+
			"$%.2f and this is the row #603 was filed over",
			rawFloat(t, thin, "total_cost_usd"), 5.00)
	}
	// Criterion 2 holds on this surface too: the number is never altered, only its
	// ranking authority revoked (#136).
	if got := rawFloat(t, thin, "tier"); got <= 0 {
		t.Errorf("teams[thin].tier = %v — an unranked aggregate keeps its exact quotient", got)
	}

	solid := rawTeamNamed(t, rows, "solid")
	if !rawBool(t, solid, "ranked") {
		t.Errorf("teams[solid].ranked = false on $%.2f of summed spend across 3 outcomes — "+
			"both floors are cleared, so a mapper hardcoded to false would pass the arm above "+
			"and publish nothing", rawFloat(t, solid, "total_cost_usd"))
	}
}

// TestGetScores_Anonymized_PublishesNoSampleN pins the k-anonymity invariant that
// the strip block in handler.go RELIES ON but never tested.
//
// That block deliberately keeps `data_quality.attributed_outcome_share` in
// anonymized mode, and the entire justification — stated in three separate comment
// blocks — is that the share's denominator is published nowhere, because
// teamScoreJSON carries no sample_n. Nothing enforced it. Adding a SampleN field
// to teamScoreJSON compiled and left the whole suite green while the response
// leaked the count outright — a published sample_n is the denominator, so any
// attributed_outcome_share resolves to a joined-outcome count by one
// multiplication (share 0.667 over sample_n 3 is 2 outcomes, exactly).
//
// The assertion is on the KEY SET, not on a decoded struct: a struct assertion
// would be blind to precisely the field being guarded against.
func TestGetScores_Anonymized_PublishesNoSampleN(t *testing.T) {
	h, db := newTeamModeHandler(t, 3)
	for _, dev := range []string{"a1", "a2", "a3"} {
		seedTeamMember(t, db, dev, "i-"+dev, "eng", 6.0, 3)
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", code, body)
	}

	// The invariant is only load-bearing while the NUMERATOR is on the wire. If the
	// share is ever dropped, this test must be re-derived rather than left passing
	// vacuously — so its absence is a failure here, not a silent skip.
	var dq struct {
		DataQuality map[string]json.RawMessage `json:"data_quality"`
	}
	if err := json.Unmarshal(body, &dq); err != nil {
		t.Fatalf("unmarshal data_quality: %v", err)
	}
	if _, ok := dq.DataQuality["attributed_outcome_share"]; !ok {
		t.Fatalf("anonymized data_quality no longer publishes attributed_outcome_share; the "+
			"no-sample_n invariant exists to protect exactly that field and must be re-derived "+
			"if it is gone: %s", body)
	}

	const forbidden = "sample_n"
	for _, raw := range append(teamsRaw(t, body), totalRaw(t, body)) {
		if _, ok := raw[forbidden]; ok {
			keys := make([]string, 0, len(raw))
			for k := range raw {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			t.Errorf("an anonymized teamScoreJSON block publishes %q — it is the denominator of "+
				"data_quality.attributed_outcome_share, which is kept in anonymized mode ONLY "+
				"because that denominator is published nowhere (#185/#593). Block keys: %v",
				forbidden, keys)
		}
	}
}
