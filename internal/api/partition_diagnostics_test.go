package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

func TestPartitionDiagnosticsFixture(t *testing.T) {
	w := windowScores{outcomes: []store.Outcome{
		{Repo: "r", IssueID: "a", PRNumber: 1, Weight: 8},
		{Repo: "r", IssueID: "a", PRNumber: 2, Weight: 3},
		{Repo: "r", IssueID: "b", PRNumber: 3, Weight: 1},
	}, scoredOutcomes: []scoring.Outcome{{Weight: 8, Quality: 0.5}, {Weight: 3, Quality: 1}, {Weight: 1, Quality: 1}}, devScores: []scoring.DeveloperScore{{TotalCostUSD: 20}}}
	d := partitionDiagnostics(w)
	if len(d.OutcomesPerIssue) != 2 || d.OutcomesPerIssue[0] != (issueMultiplicityJSON{1, 1}) || d.OutcomesPerIssue[1] != (issueMultiplicityJSON{2, 1}) {
		t.Fatalf("issue distribution: %+v", d)
	}
	if len(d.WeightBuckets) != 3 || d.WeightBuckets[2] != (weightBucketJSON{8, 1, 4}) || d.CappedPRCount != 1 || d.CappedPRPoints != 4 || *d.CappedPRCountShare != 1.0/3 || *d.CappedPRPointsShare != 0.5 || *d.PointsPerPR != 8.0/3 || d.PointsPer1000USD != 400 {
		t.Fatalf("diagnostics: %+v", d)
	}
}

func TestPartitionDiagnosticsScoreSurface(t *testing.T) {
	h, db := newTestHandler(t)
	for i, weight := range []float64{8, 3, 1} {
		issue := "shared"
		if i == 2 {
			issue = "other"
		}
		if _, err := db.InsertOutcome(context.Background(), store.Outcome{Developer: "alice", Repo: "r", IssueID: issue, PRNumber: i + 1, Weight: weight, Quality: 1, MergeCommitSHA: fmt.Sprintf("sha-%d", i), Timestamp: winAInstant}); err != nil {
			t.Fatal(err)
		}
	}
	seedCostAt(t, db, "alice", "shared", 10, winAInstant)
	seedCostAt(t, db, "alice", "other", 10, winAInstant)
	code, body := doRequest(t, h, "GET", "/api/v1/scores?since=2026-01-01&until=2026-02-01", nil)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var obj struct {
		DataQuality struct {
			Partition partitionDiagnosticsJSON `json:"partition_diagnostics"`
		} `json:"data_quality"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	d := obj.DataQuality.Partition
	if d.CappedPRCount != 1 || d.PointsPerPR == nil || *d.PointsPerPR != 4 || d.PointsPer1000USD != 600 || len(d.OutcomesPerIssue) != 2 || *d.CappedPRPointsShare != 2.0/3 {
		t.Fatalf("wire diagnostics: %s", body)
	}
}

func TestPartitionDiagnosticsEmptyAndPush(t *testing.T) {
	d := partitionDiagnostics(windowScores{})
	if d.PointsPerPR != nil || d.CappedPRCountShare != nil || d.CappedPRPointsShare != nil || d.PointsPer1000USD != 0 {
		t.Fatalf("empty: %+v", d)
	}
	d = partitionDiagnostics(windowScores{outcomes: []store.Outcome{{IssueID: "push", Weight: 0.5, WeightSource: store.WeightSourcePush}}, scoredOutcomes: []scoring.Outcome{{Weight: 0.5, Quality: 1}}, devScores: []scoring.DeveloperScore{{TotalCostUSD: 1}}})
	if d.PointsPerPR != nil || d.CappedPRCountShare != nil || d.PointsPer1000USD != 500 {
		t.Fatalf("push: %+v", d)
	}
}

func TestPartitionLegacyPRWithoutNumber(t *testing.T) {
	d := partitionDiagnostics(windowScores{outcomes: []store.Outcome{{IssueID: "legacy", Weight: 8, WeightSource: store.WeightSourceLegacy}}, scoredOutcomes: []scoring.Outcome{{Weight: 8, Quality: 0.5}}, devScores: []scoring.DeveloperScore{{TotalCostUSD: 20}}})
	if d.CappedPRCount != 1 || *d.CappedPRCountShare != 1 || *d.CappedPRPointsShare != 1 || *d.PointsPerPR != 4 {
		t.Fatalf("legacy PR: %+v", d)
	}
}

func TestIntervalEvidenceFloorPrecedesShareFloor(t *testing.T) {
	for _, ranked := range []bool{false, true} {
		s := scoring.DeveloperScore{Developer: "alice", TotalCostUSD: 100, Ranked: ranked}
		index := store.BuildJoinIndex(map[store.DevIssue]int64{{Developer: "alice", IssueID: "a"}: 49_000_000})
		want := "withheld: below evidence floor"
		if ranked {
			want = intervalWithheldNote
		}
		if got := intervalNote(s, []scoring.Outcome{{IssueID: "a", Weight: 1, Quality: 1}}, index); got != want {
			t.Fatalf("ranked=%v: got %q, want %q", ranked, got, want)
		}
	}
}

func TestCompareAbsentSideOmitsIntervalNote(t *testing.T) {
	h, db := newTestHandler(t)
	seedRankedDeveloper(t, db, "only-a", "a", 3, 1, 2, winAInstant)
	seedRankedDeveloper(t, db, "only-b", "b", 3, 1, 2, winBInstant)
	code, body := doRequest(t, h, "GET", compareURL(), nil)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var obj struct {
		Developers []struct {
			PresentA bool           `json:"present_a"`
			PresentB bool           `json:"present_b"`
			A        map[string]any `json:"a"`
			B        map[string]any `json:"b"`
		} `json:"developers"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.Developers) != 2 {
		t.Fatalf("want two one-sided rows: %s", body)
	}
	for _, row := range obj.Developers {
		if row.PresentA == row.PresentB {
			t.Fatalf("want one present side: %+v", row)
		}
		for i, present := range []bool{row.PresentA, row.PresentB} {
			side := []map[string]any{row.A, row.B}[i]
			note, exists := side["ci_note"]
			if exists != present || (present && note != intervalAttributionNote) {
				t.Fatalf("present=%v, side=%v", present, side)
			}
		}
	}
}

func TestIntervalFloorUsesBootstrapFixedCost(t *testing.T) {
	outcomes := []scoring.Outcome{
		{IssueID: "shared", Weight: 3, Quality: 1},
		{IssueID: "shared", Weight: 3, Quality: 1},
		{IssueID: "repair", Weight: 3, Quality: 1, Correction: &scoring.CorrectionValues{Points: 1, Eligible: false}},
	}
	index := store.BuildJoinIndex(map[store.DevIssue]int64{
		{Developer: "alice", IssueID: "shared"}:     6_000_000,
		{Developer: "alice", IssueID: "repair"}:     2_000_000,
		{Developer: "alice", IssueID: "no-outcome"}: 4_000_000,
	})
	s := scoring.DeveloperScore{Developer: "alice", TotalCostUSD: 12, Ranked: true}
	if note := intervalNote(s, outcomes, index); note != intervalAttributionNote {
		t.Fatal(note)
	}
	s.TotalCostUSD = 12.000001
	if note := intervalNote(s, outcomes, index); note != intervalWithheldNote {
		t.Fatal(note)
	}
}

func TestIntervalFloorSharedIssueBoundary(t *testing.T) {
	for _, n := range []int{3, 10, 100} {
		outcomes := make([]scoring.Outcome, n)
		for i := range outcomes {
			outcomes[i] = scoring.Outcome{IssueID: "shared", Weight: 1, Quality: 1}
		}
		index := store.BuildJoinIndex(map[store.DevIssue]int64{{Developer: "alice", IssueID: "shared"}: 3_000_000})
		s := scoring.DeveloperScore{Developer: "alice", TotalCostUSD: 6, Ranked: true}
		if note := intervalNote(s, outcomes, index); note != intervalAttributionNote {
			t.Fatalf("%d shared outcomes: %s", n, note)
		}
		s.TotalCostUSD = 6.000001
		if note := intervalNote(s, outcomes, index); note != intervalWithheldNote {
			t.Fatalf("%d shared outcomes below floor: %s", n, note)
		}
	}
}

func TestIntervalOwnRowFloorWire(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fixed float64
		shown bool
	}{
		{"below", 6.000001, false}, {"boundary", 6, true}, {"above", 5.999999, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, db := newTestHandler(t)
			seedRankedDeveloper(t, db, "alice", "a", 3, 1, 2, winAInstant)
			seedRankedDeveloper(t, db, "alice", "b", 3, 8, 2, winBInstant)
			seedCostAt(t, db, "alice", "unattributed", tc.fixed, winAInstant)
			seedCostAt(t, db, "alice", "unattributed", 1, winBInstant)
			seedRankedDeveloper(t, db, "bob", "control", 3, 1, 100, winAInstant)
			for _, path := range []string{"/api/v1/scores?since=2026-01-01&until=2026-02-01", "/api/v1/scores/alice?since=2026-01-01&until=2026-02-01"} {
				code, body := doRequest(t, h, "GET", path, nil)
				if code != 200 {
					t.Fatalf("%d %s", code, body)
				}
				var obj map[string]any
				if err := json.Unmarshal(body, &obj); err != nil {
					t.Fatal(err)
				}
				row := obj
				if rows, ok := obj["developers"].([]any); ok {
					row = rows[0].(map[string]any)
				}
				if row["ranked"] != true {
					t.Fatalf("floor must not change ranked: %s", body)
				}
				for _, k := range []string{"ci_low", "ci_high", "cost_per_point_ci_low", "cost_per_point_ci_high"} {
					v, ok := row[k].(float64)
					if !ok || (v > 0) != tc.shown {
						t.Fatalf("%s: %v; %s", k, row[k], body)
					}
				}
				want := intervalWithheldNote
				if tc.shown {
					want = intervalAttributionNote
				}
				if row["ci_note"] != want {
					t.Fatalf("note: %v", row)
				}
				if rows, ok := obj["developers"].([]any); ok && rows[1].(map[string]any)["ci_note"] != intervalAttributionNote {
					t.Fatal("window share must not gate bob")
				}
			}
			code, resp := getCompare(t, h, compareURL())
			if code != 200 {
				t.Fatal(code)
			}
			d := devDelta(t, resp, "alice")
			if d.B.CINote != intervalAttributionNote || d.B.CIHigh <= 0 {
				t.Fatalf("control side must keep its interval: %+v", d.B)
			}
			if d.Significant != tc.shown {
				t.Fatalf("significant=%v want %v: %+v", d.Significant, tc.shown, d)
			}
		})
	}
}

func TestPartitionAndIntervalAnonymous(t *testing.T) {
	for _, mode := range []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision} {
		t.Run(mode.String(), func(t *testing.T) {
			var baseline map[string]any
			// Reallocating two dollars within one $100 row leaves all group totals identical.
			for _, share := range []int{49, 51} {
				h, db := newTestHandler(t)
				registerGated(t, db)
				seedCostAt(t, db, "member-0", "early", 1, time.Date(2026, time.April, 15, 0, 0, 0, 0, time.UTC))
				for i := 0; i < 5; i++ {
					dev := fmt.Sprintf("member-%d", i)
					if err := baselineHierarchy(db, t.Context(), dev, "eng", "division", "org"); err != nil {
						t.Fatal(err)
					}
					for _, at := range []time.Time{compareMay, compareJune} {
						tag := at.Month().String()
						seedRankedDeveloper(t, db, dev, tag, 3, 1, 1, at)
						if i == 0 {
							seedCostAt(t, db, dev, tag+"-"+dev+"-a", float64(share-3), at)
							seedCostAt(t, db, dev, "unattributed", float64(100-share), at)
						}
					}
				}
				code, own := doRequest(t, h, "GET", "/api/v1/scores/member-0?since=2026-05-01&until=2026-06-01", nil)
				want := intervalWithheldNote
				if share == 51 {
					want = intervalAttributionNote
				}
				var control developerDetailResponse
				if err := json.Unmarshal(own, &control); err != nil {
					t.Fatal(err)
				}
				if code != 200 || !control.Ranked || control.CINote != want || control.TotalCostUSD != 100 {
					t.Fatalf("share=%d control: %d %s", share, code, own)
				}
				h.SetAggregation(mode, 5)
				results := map[string]any{}
				check := func(name, path string) {
					t.Helper()
					code, body := doRequest(t, h, "GET", path, nil)
					if code != 200 {
						t.Fatalf("%s: %d %s", name, code, body)
					}
					var obj map[string]any
					if err := json.Unmarshal(body, &obj); err != nil {
						t.Fatal(err)
					}
					teams, ok := obj["teams"].([]any)
					if !ok || len(teams) != 1 || obj["kanon_suppressed"] != nil {
						t.Fatalf("%s must publish a k=5 cohort: %s", name, body)
					}
					if strings.Contains(string(body), "ci_note") || strings.Contains(string(body), "below 50%") || strings.Contains(string(body), "below evidence floor") {
						t.Fatalf("%s anonymous disclosure: %s", name, body)
					}
					result := map[string]any{"teams": teams}
					if dq, ok := obj["data_quality"].(map[string]any); ok {
						if dq["partition_diagnostics"] != "not_shown" || dq["interval_status"] != "not_shown" {
							t.Fatalf("%s missing anonymous policy: %s", name, body)
						}
						if dq["kanon_suppressed"] != nil {
							t.Fatalf("%s suppressed its cohort: %s", name, body)
						}
						result["data_quality"] = dq
					} else {
						for _, key := range []string{"window_a", "window_b"} {
							dq := obj[key].(map[string]any)["data_quality"].(map[string]any)
							if _, ok := dq["interval_status"]; ok {
								t.Fatalf("%s compare exposes /scores-only interval status: %s", name, body)
							}
							if _, ok := dq["partition_diagnostics"]; ok {
								t.Fatalf("%s compare exposes /scores-only diagnostics: %s", name, body)
							}
							result[key] = dq
						}
					}
					results[name] = result
					if name == "sealed scores" {
						stored, _, err := h.sealer.load(t.Context(), sealMay)
						if err != nil {
							t.Fatal(err)
						}
						if !bytes.Equal(body, stored) {
							t.Fatal("sealed scores must serve stored bytes verbatim")
						}
					}
				}
				check("live scores", "/api/v1/scores?since=2026-05-01&until=2026-06-01")
				check("live compare", "/api/v1/scores/compare"+compareLiveWindows)
				h.sealer = newTestSealer(h)
				h.sealer.now = func() time.Time { return compareNow }
				sealPass(t, h.sealer)
				stored := sealedState(t, db)
				check("sealed scores", "/api/v1/scores?period=2026-05")
				check("sealed compare", "/api/v1/scores/compare?period_a=2026-05&period_b=2026-06")
				if sealedState(t, db) != stored {
					t.Fatal("sealed reads changed stored bytes or fold inputs")
				}
				if baseline != nil && !reflect.DeepEqual(results, baseline) {
					t.Fatalf("anonymous team rows or data_quality depend on own-row floor: 49%%=%v; 51%%=%v", baseline, results)
				}
				baseline = results
			}
		})
	}
}
