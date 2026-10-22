package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

func TestTimeToMergeStatistics(t *testing.T) {
	for _, tc := range []struct {
		name        string
		values      []time.Duration
		median, p90 int64
	}{
		{"odd", []time.Duration{9 * time.Second, time.Second, 4 * time.Second}, 4, -1},
		{"even exact", []time.Duration{8 * time.Second, time.Second, 2 * time.Second, 6 * time.Second}, 4, -1},
		{"even fractional inputs", []time.Duration{1500 * time.Millisecond, 2500 * time.Millisecond, 3500 * time.Millisecond, 4500 * time.Millisecond}, 3, -1},
		{"nearest rank ten", []time.Duration{10 * time.Second, 9 * time.Second, 8 * time.Second, 7 * time.Second, 6 * time.Second, 5 * time.Second, 4 * time.Second, 3 * time.Second, 2 * time.Second, time.Second}, 5, 9},
		{"nearest rank eleven", []time.Duration{11 * time.Second, 10 * time.Second, 9 * time.Second, 8 * time.Second, 7 * time.Second, 6 * time.Second, 5 * time.Second, 4 * time.Second, 3 * time.Second, 2 * time.Second, time.Second}, 6, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sample mergeSample
			for i, d := range tc.values {
				sample.add(d, fmt.Sprint(i), true)
			}
			f := sample.figure("eng", 3)
			if f.Count == nil || *f.Count != len(tc.values) || f.MedianSeconds == nil || *f.MedianSeconds != tc.median {
				t.Fatalf("figure: %+v", f)
			}
			if tc.p90 < 0 {
				if f.P90Seconds != nil || f.NullReason != "n<10" {
					t.Fatalf("short sample p90: %+v", f)
				}
			} else if f.P90Seconds == nil || *f.P90Seconds != tc.p90 || f.NullReason != "" {
				t.Fatalf("p90: %+v", f)
			}
		})
	}
	var one mergeSample
	for i := 0; i < 20; i++ {
		one.add(time.Hour, "one", true)
	}
	if f := one.figure("eng", 1); f.Count != nil || f.MedianSeconds != nil || f.P90Seconds != nil || f.NullReason != "authors<k" {
		t.Fatalf("author floor: %+v", f)
	}
}

var ttmAt = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

const ttmQuery = "?since=2026-10-01&until=2026-11-01"

func seedTTM(t *testing.T, db *store.DB, dev, issue string, created, merged *time.Time, source string) {
	t.Helper()
	if _, err := db.InsertOutcome(context.Background(), store.Outcome{
		Developer: dev, IssueID: issue, Repo: repoAlpha, PRNumber: 42,
		MergeCommitSHA: "ttm-" + issue, Weight: 1, Quality: 1, Timestamp: ttmAt,
		CreatedAt: created, MergedAt: merged, Source: source,
	}); err != nil {
		t.Fatal(err)
	}
}

func getTTM(t *testing.T, h *Handler, query string) timeToMergeResponse {
	t.Helper()
	w := httptest.NewRecorder()
	h.handleGetTimeToMerge(w, httptest.NewRequest(http.MethodGet, "/api/v1/time-to-merge"+query, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("time to merge: %d %s", w.Code, w.Body.String())
	}
	var resp timeToMergeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestTimeToMergeAuthorFloorKeepsRowAndResidual(t *testing.T) {
	for _, mode := range anonymisedModes {
		t.Run(mode.String(), func(t *testing.T) {
			h, db := newTestHandler(t)
			for i := 1; i <= 5; i++ {
				seedKAnonDev(t, db, "eng", fmt.Sprintf("e%d", i), 10, 1, ttmAt)
			}
			padResidual(t, db, 5, ttmAt)
			created := ttmAt.Add(-4 * time.Hour)
			seedTTM(t, db, "e1", "only-author", &created, &ttmAt, "")
			for i := 1; i <= 5; i++ {
				seedTTM(t, db, fmt.Sprintf("pad-%d", i), fmt.Sprintf("residual-%d", i), &created, &ttmAt, "")
			}
			h.SetAggregation(mode, 5)
			scores := getScores(t, h, "/api/v1/scores"+ttmQuery)
			if len(scores.Teams) != 2 {
				t.Fatalf("scores control: %+v", scores.Teams)
			}
			got := getTTM(t, h, ttmQuery)
			if len(got.Groups) != 1 || got.Groups[0].Team != scores.Teams[0].Team || got.Groups[0].Count != nil || got.Groups[0].MedianSeconds != nil || got.Groups[0].P90Seconds != nil || got.Groups[0].NullReason != "authors<k" {
				t.Fatalf("author floor row: %+v", got)
			}
			if got.Residual == nil || got.Residual.Count == nil || *got.Residual.Count != 5 || *got.Residual.MedianSeconds != 14400 {
				t.Fatalf("residual changed: %+v", got.Residual)
			}
			assertTimeToMergeTotalWithheld(t, got.Total, "withheld_with_group")
		})
	}
}

func TestTimeToMergeScoresSuppressionParity(t *testing.T) {
	for _, residualN := range []int{2, 5} {
		t.Run(fmt.Sprint(residualN), func(t *testing.T) {
			h, db := newTestHandler(t)
			created := ttmAt.Add(-time.Hour)
			for i := 1; i <= 5; i++ {
				dev := fmt.Sprintf("e%d", i)
				seedKAnonDev(t, db, "eng", dev, 10, 1, ttmAt)
				seedTTM(t, db, dev, "eng-"+dev, &created, &ttmAt, "")
			}
			padResidual(t, db, residualN, ttmAt)
			for i := 1; i <= residualN; i++ {
				seedTTM(t, db, fmt.Sprintf("pad-%d", i), fmt.Sprintf("res-%d", i), &created, &ttmAt, "")
			}
			h.SetAggregation(scoring.AggregationTeam, 5)
			scores := getScores(t, h, "/api/v1/scores"+ttmQuery)
			got := getTTM(t, h, ttmQuery)
			if residualN < 5 {
				if scores.DataQuality == nil || scores.DataQuality.KAnonSuppressed == nil {
					t.Fatal("scores control must suppress")
				}
				if len(got.Groups) != 0 || got.Residual != nil {
					t.Fatalf("scores suppression escaped: %+v", got)
				}
				assertTimeToMergeTotalWithheld(t, got.Total, "withheld_with_group")
				if got.Excluded.Count != nil || got.Excluded.Reasons != nil || got.Excluded.NullReason != "authors<k" {
					t.Fatalf("unsafe scores residual exposed exclusions: %+v", got.Excluded)
				}
			} else if len(got.Groups) != 1 || got.Groups[0].Team != "eng" || *got.Groups[0].Count != 5 || *got.Residual.Count != 5 {
				t.Fatalf("fold parity: %+v", got)
			}
		})
	}
}

func TestTimeToMergeTotalWithheldForResidualPerMeasureFloor(t *testing.T) {
	for _, mode := range anonymisedModes {
		t.Run(mode.String(), func(t *testing.T) {
			h, db := newTestHandler(t)
			created := ttmAt.Add(-time.Hour)
			for i := 1; i <= 5; i++ {
				dev := fmt.Sprintf("e%d", i)
				seedKAnonDev(t, db, "eng", dev, 10, 1, ttmAt)
				seedTTM(t, db, dev, "eng-"+dev, &created, &ttmAt, "")
			}
			padResidual(t, db, 5)
			for i := 1; i <= 5; i++ {
				seedTTM(t, db, fmt.Sprintf("pad-%d", i), fmt.Sprintf("res-%d", i), &created, &ttmAt, "")
			}
			// The residual clears the PR-author floor, but only one author
			// carries captured spend, so the shared per-measure floor fails.
			seedCostAt(t, db, "pad-1", "res-1", 10, ttmAt)
			h.SetAggregation(mode, 5)
			scores := getScores(t, h, "/api/v1/scores"+ttmQuery)
			if len(scores.Teams) != 0 || scores.DataQuality == nil || scores.DataQuality.KAnonSuppressed == nil {
				t.Fatal("scores control must suppress the unsafe per-measure residual")
			}
			got := getTTM(t, h, ttmQuery)
			if len(got.Groups) != 0 || got.Residual != nil {
				t.Fatalf("scores suppression escaped: %+v", got)
			}
			assertTimeToMergeTotalWithheld(t, got.Total, "withheld_with_group")
		})
	}
}

func assertTimeToMergeTotalWithheld(t *testing.T, total timeToMergeFigure, reason string) {
	t.Helper()
	if total.Count != nil || total.MedianSeconds != nil || total.P90Seconds != nil || total.NullReason != reason {
		t.Fatalf("total must withhold all figures (%s): %+v", reason, total)
	}
}

func TestTimeToMergeTotalBelowOrgK(t *testing.T) {
	h, db := newTestHandler(t)
	for i := 1; i <= 5; i++ {
		seedKAnonDev(t, db, "eng", fmt.Sprintf("e%d", i), 10, 1, ttmAt)
	}
	created := ttmAt.Add(-time.Hour)
	// n >= k is insufficient: the org needs k distinct counted PR authors.
	for i := 0; i < 10; i++ {
		seedTTM(t, db, "e1", fmt.Sprintf("one-%d", i), &created, &ttmAt, "")
	}
	h.SetAggregation(scoring.AggregationTeam, 5)
	got := getTTM(t, h, ttmQuery)
	assertTimeToMergeTotalWithheld(t, got.Total, "authors<k")
	// The excluded floor counts authors of every population PR, including the
	// five seed PRs without timestamps, rather than only included PR authors.
	if got.Excluded.Count == nil || *got.Excluded.Count != 5 || got.Excluded.NullReason != "" {
		t.Fatalf("excluded population author floor: %+v", got.Excluded)
	}
}

func TestTimeToMergeTotalExactWhenNothingWithheld(t *testing.T) {
	for _, mode := range anonymisedModes {
		t.Run(mode.String(), func(t *testing.T) {
			h, db := newTestHandler(t)
			merged := ttmAt.Add(-time.Hour) // ts must never stand in for merged_at
			for i := 1; i <= 5; i++ {
				dev := fmt.Sprintf("e%d", i)
				seedKAnonDev(t, db, "eng", dev, 10, 1, ttmAt)
				created := merged.Add(-time.Duration(2*i) * time.Second)
				seedTTM(t, db, dev, "named-"+dev, &created, &merged, "")
			}
			padResidual(t, db, 5, ttmAt)
			for i := 1; i <= 5; i++ {
				created := merged.Add(-time.Duration(20*i) * time.Second)
				seedTTM(t, db, fmt.Sprintf("pad-%d", i), fmt.Sprintf("res-%d", i), &created, &merged, "backfill")
			}
			h.SetAggregation(mode, 5)
			got := getTTM(t, h, ttmQuery)
			if got.Total.Count == nil || *got.Total.Count != 10 || got.Total.MedianSeconds == nil || *got.Total.MedianSeconds != 15 || got.Total.P90Seconds == nil || *got.Total.P90Seconds != 80 || got.Total.NullReason != "" {
				t.Fatalf("exact org fixture: %+v", got.Total)
			}
			// Both cohorts have n<10 p90s. That sample-size rule must not
			// suppress a total whose cohorts' counts/medians are published.
			if got.Groups[0].NullReason != "n<10" || got.Residual.NullReason != "n<10" {
				t.Fatalf("cohort fixture: %+v", got)
			}
			wire, err := json.Marshal(got.Total)
			if err != nil {
				t.Fatal(err)
			}
			if string(wire) != `{"count":10,"median_seconds":15,"p90_seconds":80,"null_reason":""}` {
				t.Fatalf("total schema: %s", wire)
			}
		})
	}
}

func TestTimeToMergeTotalShortSampleP90(t *testing.T) {
	h, db := newTestHandler(t)
	padResidual(t, db, 5, ttmAt)
	created := ttmAt.Add(-time.Hour)
	for i := 1; i <= 5; i++ {
		seedTTM(t, db, fmt.Sprintf("pad-%d", i), fmt.Sprintf("res-%d", i), &created, &ttmAt, "")
	}
	h.SetAggregation(scoring.AggregationTeam, 5)
	got := getTTM(t, h, ttmQuery)
	if got.Total.Count == nil || *got.Total.Count != 5 || *got.Total.MedianSeconds != 3600 || got.Total.P90Seconds != nil || got.Total.NullReason != "n<10" {
		t.Fatalf("short org sample: %+v", got.Total)
	}
}

func TestTimeToMergeTotalWithNullResidual(t *testing.T) {
	h, db := newTestHandler(t)
	created := ttmAt.Add(-time.Hour)
	for i := 1; i <= 5; i++ {
		dev := fmt.Sprintf("e%d", i)
		seedKAnonDev(t, db, "eng", dev, 10, 1, ttmAt)
		seedTTM(t, db, dev, "named-"+dev, &created, &ttmAt, "")
	}
	h.SetAggregation(scoring.AggregationTeam, 5)
	got := getTTM(t, h, ttmQuery)
	if got.Groups[0].Count == nil || got.Total.Count == nil || *got.Total.Count != 5 {
		t.Fatalf("absent residual must not withhold total: %+v", got)
	}
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(wire, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["residual"]; exists {
		t.Fatalf("phantom other row: %s", wire)
	}
}

func TestTimeToMergeExclusionsAndBackfillEvidence(t *testing.T) {
	h, db := newTestHandler(t)
	created := ttmAt.Add(-2 * time.Hour)
	for i := 1; i <= 5; i++ {
		dev := fmt.Sprintf("e%d", i)
		seedKAnonDev(t, db, "eng", dev, 10, 1, ttmAt)
		seedTTM(t, db, dev, "captured-"+dev, &created, &ttmAt, store.OutcomeSourceGitHubWebhook)
	}
	seedTTM(t, db, "e1", "backfill", &created, &ttmAt, "backfill")
	seedTTM(t, db, "e1", "missing-merge", &created, nil, "")
	seedTTM(t, db, "e1", "api", nil, nil, store.OutcomeSourceAPI)
	after := ttmAt.Add(time.Hour)
	seedTTM(t, db, "e1", "invalid-created", &after, &ttmAt, "")
	seedTTM(t, db, "renovate[bot]", "bot", &created, &ttmAt, "")
	seedTTM(t, db, "typed-bot", "typed-bot", &created, &ttmAt, "")
	for _, bot := range []string{"renovate[bot]", "typed-bot"} {
		if err := baselineHierarchy(db, context.Background(), bot, "eng", "div-eng", "acme"); err != nil {
			t.Fatal(err)
		}
	}
	raw := rawSealStore(t, db)
	if _, err := raw.Exec("UPDATE outcomes SET author_type='Bot' WHERE developer='typed-bot'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertOutcome(context.Background(), store.Outcome{Developer: "e1", IssueID: "before", PRNumber: 1, Weight: 1, Quality: 1, Timestamp: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	h.SetAggregation(scoring.AggregationTeam, 5)
	got := getTTM(t, h, ttmQuery)
	if len(got.Groups) != 1 || got.Groups[0].Count == nil || *got.Groups[0].Count != 6 || *got.Groups[0].MedianSeconds != 7200 {
		t.Fatalf("captured and backfill population: %+v", got)
	}
	want := map[string]int{"no_created_at_before_capture": 1, "no_created_at": 7, "no_merged_at": 1, "bot": 2}
	if !reflect.DeepEqual(got.Excluded.Reasons, want) || got.Excluded.Count == nil || *got.Excluded.Count != 11 {
		t.Fatalf("exclusions: %+v, want %v", got.Excluded, want)
	}
}

func TestTimeToMergeReadScopeAndDeveloperRefusal(t *testing.T) {
	h, _ := newTestHandlerWithScopes(t, "admin", "viewer")
	for _, register := range []func(routeMux){h.Register, h.RegisterReadOnly} {
		mux := http.NewServeMux()
		register(mux)
		for _, tc := range []struct {
			token string
			code  int
		}{{"", 401}, {"viewer", 403}, {"admin", 403}} {
			r := httptest.NewRequest("GET", "/api/v1/time-to-merge"+ttmQuery, nil)
			r.Header.Set("Authorization", "Bearer "+tc.token)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != tc.code {
				t.Fatalf("developer token %q: %d %s", tc.token, w.Code, w.Body.String())
			}
		}
		h.SetAggregation(scoring.AggregationTeam, 5)
		r := httptest.NewRequest("GET", "/api/v1/time-to-merge"+ttmQuery, nil)
		r.Header.Set("Authorization", "Bearer viewer")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("read scope: %d %s", w.Code, w.Body.String())
		}
		h.SetAggregation(scoring.AggregationDeveloper, 5)
	}
	WithSyntheticDemo(true)(h)
	if got := getTTM(t, h, ttmQuery); got.Window.Aggregation != "team" {
		t.Fatalf("demo must echo row aggregation: %+v", got.Window)
	}
}

func TestTimeToMergeWindowValidation(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetAggregation(scoring.AggregationTeam, 5)
	for _, query := range []string{"?since=no", "?since=2026-10-02&until=2026-10-01", "?mode=developer", "?repo=acme/app", "?period=2026-10"} {
		w := httptest.NewRecorder()
		h.handleGetTimeToMerge(w, httptest.NewRequest("GET", "/api/v1/time-to-merge"+query, nil))
		if w.Code != 400 {
			t.Errorf("%s: %d", query, w.Code)
		}
	}
	h.SetRetentionHorizon(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	w := httptest.NewRecorder()
	h.handleGetTimeToMerge(w, httptest.NewRequest("GET", "/api/v1/time-to-merge"+ttmQuery, nil))
	if w.Code != 422 {
		t.Fatalf("retention: %d", w.Code)
	}
}

func TestTimeToMergeCanonicalAuthorsAndBotAlias(t *testing.T) {
	h, db := newTestHandler(t)
	created := ttmAt.Add(-time.Hour)
	for i := 1; i <= 5; i++ {
		dev := fmt.Sprintf("e%d", i)
		seedKAnonDev(t, db, "eng", dev, 10, 1, ttmAt)
	}
	// Five raw author ids for one canonical person must not fill five seats.
	for i := 1; i <= 5; i++ {
		alias := fmt.Sprintf("alias%d", i)
		if err := baselineAlias(db, context.Background(), alias, "e1"); err != nil {
			t.Fatal(err)
		}
		seedTTM(t, db, alias, alias, &created, &ttmAt, "")
	}
	h.SetAggregation(scoring.AggregationTeam, 5)
	got := getTTM(t, h, ttmQuery)
	if len(got.Groups) != 1 || got.Groups[0].Count != nil {
		t.Fatalf("aliases filled PR author seats: %+v", got)
	}
	// A bot's spelling survives canonicalization. It excludes even rows recorded
	// under the canonical id, exactly as the shared census classifies the person.
	if err := baselineHierarchy(db, context.Background(), "agent", "eng", "div-eng", "acme"); err != nil {
		t.Fatal(err)
	}
	if err := baselineAlias(db, context.Background(), "renovate[bot]", "agent"); err != nil {
		t.Fatal(err)
	}
	seedTTM(t, db, "renovate[bot]", "bot-alias", &created, &ttmAt, "")
	seedTTM(t, db, "agent", "bot-canonical", &created, &ttmAt, "")
	got = getTTM(t, h, ttmQuery)
	if got.Excluded.Reasons["bot"] != 2 {
		t.Fatalf("bot alias escaped: %+v", got.Excluded)
	}
}

func TestTimeToMergeScoresBytesAndSealUnchanged(t *testing.T) {
	// The pre-existing golden is the receipt for the unchanged live body.
	TestScores_AnonymisedLiveOutputUnchanged(t)
	h, db, s := newSealFixture(t)
	_, liveBefore := getScoresRaw(t, h, "?since=2026-05-01&until=2026-06-01")
	foldedBefore, _ := foldedMayBody(t, h)
	sealPass(t, s)
	before, err := db.SealedReport(context.Background(), "month", sealFixtureMonth)
	if err != nil {
		t.Fatal(err)
	}
	h.sealer = s
	getTTM(t, h, "?period=2026-05")
	h.sealer = nil
	_, liveAfter := getScoresRaw(t, h, "?since=2026-05-01&until=2026-06-01")
	foldedAfter, _ := foldedMayBody(t, h)
	after, err := db.SealedReport(context.Background(), "month", sealFixtureMonth)
	if err != nil {
		t.Fatal(err)
	}
	if liveBefore != liveAfter || !bytes.Equal(foldedBefore, foldedAfter) || !reflect.DeepEqual(before, after) {
		t.Fatal("descriptive read changed scores bytes or seal")
	}
	// Pin the folded body and configuration digest against a golden generated
	// from origin/main, before this endpoint existed. Mask only moving prices.
	var body map[string]any
	if err := json.Unmarshal(foldedAfter, &body); err != nil {
		t.Fatal(err)
	}
	body["price_table"] = "<price_table>"
	got, err := json.MarshalIndent(map[string]any{"body": body, "config_digest": before.ConfigDigest}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "time_to_merge_scores_seal.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(got, '\n'), want) {
		t.Fatalf("scores/seal golden changed:\n%s", got)
	}
}

func TestTimeToMergeSealedMonthGate(t *testing.T) {
	for _, mode := range anonymisedModes {
		t.Run(mode.String(), func(t *testing.T) {
			h, _, s := newSealedReadHandler(t)
			h.SetAggregation(mode, 5)
			sealPass(t, s)
			for _, query := range []string{ttmQuery, "?since=", "?until=2026-06-01", "?before=2026-06-01", "?period=2026-05", "", "?period=2026-06"} {
				want := sealedGet(t, h, "/api/v1/scores"+query)
				got := httptest.NewRecorder()
				h.handleGetTimeToMerge(got, httptest.NewRequest("GET", "/api/v1/time-to-merge"+query, nil))
				if got.Code != want.Code {
					t.Errorf("%s: time-to-merge %d %s; scores %d", query, got.Code, got.Body, want.Code)
				}
				if got.Code == http.StatusOK {
					var resp timeToMergeResponse
					if err := json.Unmarshal(got.Body.Bytes(), &resp); err != nil || resp.Window.Since != "2026-05-01" || resp.Window.Until != "2026-06-01" {
						t.Fatalf("sealed month bounds: %+v %v", resp, err)
					}
				}
			}
			h.sealer, h.unsealedRecompute = nil, false
			got := httptest.NewRecorder()
			h.handleGetTimeToMerge(got, httptest.NewRequest("GET", "/api/v1/time-to-merge?period=2026-05", nil))
			if got.Code != http.StatusServiceUnavailable {
				t.Fatalf("no sealer: %d %s", got.Code, got.Body)
			}
		})
	}
}

func TestTimeToMergeExcludedOneAuthorScoresParity(t *testing.T) {
	h, db := newTestHandler(t)
	seedKAnonDev(t, db, "eng", "one", 10, 1, ttmAt)
	for i := 0; i < 7; i++ {
		seedTTM(t, db, "one", fmt.Sprintf("excluded-%d", i), nil, &ttmAt, "")
	}
	h.SetAggregation(scoring.AggregationTeam, 5)
	scores := getScores(t, h, "/api/v1/scores"+ttmQuery)
	if len(scores.Teams) != 0 || scores.DataQuality == nil || scores.DataQuality.KAnonSuppressed == nil {
		t.Fatal("scores control must suppress the one-author population")
	}
	got := getTTM(t, h, ttmQuery)
	wire, err := json.Marshal(got.Excluded)
	if err != nil {
		t.Fatal(err)
	}
	var excluded map[string]any
	if err := json.Unmarshal(wire, &excluded); err != nil {
		t.Fatal(err)
	}
	if excluded["count"] != nil || excluded["reasons"] != nil || excluded["null_reason"] != "authors<k" {
		t.Fatalf("one-author exclusions escaped scores floor: %s", wire)
	}
}

func TestTimeToMergeEmptyCohortDoesNotWithholdTotal(t *testing.T) {
	for _, empty := range []string{"team", "residual"} {
		t.Run(empty, func(t *testing.T) {
			h, db := newTestHandler(t)
			created := ttmAt.Add(-time.Hour)
			for i := 1; i <= 5; i++ {
				dev := fmt.Sprintf("e%d", i)
				seedKAnonDev(t, db, "eng", dev, 10, 1, ttmAt)
				if empty == "residual" {
					seedTTM(t, db, dev, "eng-"+dev, &created, &ttmAt, "")
				}
			}
			padResidual(t, db, 5, ttmAt)
			if empty == "team" {
				for i := 1; i <= 5; i++ {
					seedTTM(t, db, fmt.Sprintf("pad-%d", i), fmt.Sprintf("res-%d", i), &created, &ttmAt, "")
				}
			}
			h.SetAggregation(scoring.AggregationTeam, 5)
			got := getTTM(t, h, ttmQuery)
			if got.Total.Count == nil || *got.Total.Count != 5 || got.Total.MedianSeconds == nil || *got.Total.MedianSeconds != 3600 {
				t.Fatalf("empty %s withheld total: %+v", empty, got)
			}
			if empty == "team" && (len(got.Groups) != 1 || got.Groups[0].NullReason != "no_prs" || got.Groups[0].Count != nil) {
				t.Fatalf("empty team: %+v", got.Groups)
			}
			if empty == "residual" && (got.Residual == nil || got.Residual.NullReason != "no_prs" || got.Residual.Count != nil) {
				t.Fatalf("empty residual: %+v", got.Residual)
			}
		})
	}
}

func TestTimeToMergeEmptyWindow(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetAggregation(scoring.AggregationTeam, 5)
	got := getTTM(t, h, ttmQuery)
	if got.Total.Count != nil || got.Total.MedianSeconds != nil || got.Total.P90Seconds != nil || got.Total.NullReason != "no_prs" || got.Excluded.Count != nil || got.Excluded.Reasons != nil || got.Excluded.NullReason != "no_prs" || got.Residual != nil || len(got.Groups) != 0 {
		t.Fatalf("empty window: %+v", got)
	}
}
