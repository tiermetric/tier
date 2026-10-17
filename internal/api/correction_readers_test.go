package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/corrects"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

func correctionReaderFixture(t *testing.T) (*Handler, *store.DB) {
	t.Helper()
	h, db := newTestHandler(t)
	ctx := context.Background()
	for i := 1; i <= 2; i++ {
		at := time.Date(2026, 10, i, 0, 0, 0, 0, time.UTC)
		o := store.Outcome{Developer: "alice", Repo: "acme/app", PRNumber: i, IssueID: "repair", Weight: 5, Quality: 1, Timestamp: at, MergedAt: &at, MergeCommitSHA: strings.Repeat("s", i), WorkType: store.WorkTypeFeature}
		body := "Corrects: none"
		if i == 2 {
			o.Weight = 1
			body = "Corrects: #1"
			o.WorkType = store.WorkTypeBug
		}
		a, b, c := 10, 0, 1
		o.SetDiffStats(&a, &b, &c)
		if _, err := db.InsertOutcome(ctx, o); err != nil {
			t.Fatal(err)
		}
		if _, err := db.RecordCorrectionDeclaration(ctx, o, corrects.Parse(body), "=bounded evidence", "test"); err != nil {
			t.Fatal(err)
		}
	}
	child, _, err := db.OutcomeByMergeCommit(ctx, "ss")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReconcileCorrectionChain(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	h.scoringPolicy = scoring.ScoringPolicy{Event3: true, Resolved: true, RuleVersion: "event3-v2"}
	return h, db
}
func TestCorrectionReaders_HeadlineDetailSegmentsAgree(t *testing.T) {
	h, _ := correctionReaderFixture(t)
	for _, path := range []string{"/api/v1/scores?since=2026-10-01&until=2026-11-01", "/api/v1/scores/alice?since=2026-10-01&until=2026-11-01"} {
		r := correctionGet(t, h, path)
		if r.Code != http.StatusOK {
			t.Fatal(r.Code, r.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		row := body
		if ds, ok := body["developers"].([]any); ok {
			row = ds[0].(map[string]any)
		}
		if segments, ok := body["work_types"].([]any); ok {
			for _, entry := range segments {
				seg := entry.(map[string]any)
				sr := seg["developers"].([]any)[0].(map[string]any)
				want, eligible := 3.25, float64(1)
				if seg["work_type"] == "bug" {
					want = 1
					eligible = 0
				}
				if sr["weighted_points"] != want || sr["sample_n"] != eligible || sr["outcome_n"] != float64(1) {
					t.Fatal(seg)
				}
			}
		}
		if row["weighted_points"] != 4.25 || row["sample_n"] != float64(1) || row["outcome_n"] != float64(2) {
			t.Fatal(r.Body.String())
		}
	}
	// A parent outside the selected window must already have affected this child.
	r := correctionGet(t, h, "/api/v1/scores/alice?since=2026-10-02&until=2026-11-01")
	var b map[string]any
	_ = json.Unmarshal(r.Body.Bytes(), &b)
	if b["weighted_points"] != float64(1) || b["sample_n"] != float64(0) {
		t.Fatal(r.Body.String())
	}
}

func TestCorrectionReaders_EngineF5SegmentOutcomeCountUsesPolicy(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			h, db := correctionReaderFixture(t)
			if err := db.ConfigureCorrectionScoring(t.Context(), enabled); err != nil {
				t.Fatal(err)
			}
			h.storedScoringPolicy = true
			r := correctionGet(t, h, "/api/v1/scores?since=2026-10-01&until=2026-11-01")
			if r.Code != http.StatusOK {
				t.Fatal(r.Code, r.Body.String())
			}
			body := decodeJSON(t, r)
			pooled := body["developers"].([]any)[0].(map[string]any)
			if _, present := pooled["outcome_n"]; present != enabled {
				t.Fatal("pooled outcome_n", pooled)
			}
			for _, entry := range body["work_types"].([]any) {
				row := entry.(map[string]any)["developers"].([]any)[0].(map[string]any)
				if _, present := row["outcome_n"]; present != enabled {
					t.Fatal("segment outcome_n", row)
				}
			}
		})
	}
}
func TestCorrectionExport_RawAndProjectedValuesAreDistinct(t *testing.T) {
	h, _ := correctionReaderFixture(t)
	r := correctionGet(t, h, "/api/v1/outcomes?since=2026-10-01&until=2026-11-01")
	if !strings.Contains(r.Body.String(), `"allocated_points":3.25`) || !strings.Contains(r.Body.String(), `"raw_additions":10`) {
		t.Fatal(r.Body.String())
	}
	r = correctionGet(t, h, "/api/v1/outcomes?since=2026-10-01&until=2026-11-01&csv=true")
	rows, err := csv.NewReader(strings.NewReader(r.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows[0]) <= 18 || rows[0][16] != "push_day" || rows[0][17] != "quality_status" || rows[0][18] != "raw_additions" {
		t.Fatal(rows)
	}
	r = correctionGet(t, h, "/api/v1/corrections/relations?since=2026-10-01&until=2027-11-01")
	if r.Code != 200 || !strings.Contains(r.Body.String(), "=bounded evidence") {
		t.Fatal(r.Code, r.Body.String())
	}
}

func correctionGet(t *testing.T, h *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.Register(mux)
	r := httptest.NewRequest("GET", path, nil)
	if strings.Contains(path, "csv=true") {
		r.Header.Set("Accept", "text/csv")
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestCorrectionBootstrap_SharedIssueRepairCostAndPermutation(t *testing.T) {
	out := []scoring.Outcome{
		{Developer: "alice", Repo: "acme/app", IssueID: "shared", Weight: 8, Quality: 0, Correction: &scoring.CorrectionValues{Points: 1, Eligible: false}},
		{Developer: "alice", Repo: "acme/app", IssueID: "shared", Correction: &scoring.CorrectionValues{Points: 3.25, Eligible: true}},
		{Developer: "alice", Repo: "acme/app", IssueID: "growth", Correction: &scoring.CorrectionValues{Points: 5, Eligible: true}},
	}
	idx := store.BuildJoinIndex(map[store.DevIssue]int64{{Developer: "alice", Repo: "acme/app", IssueID: "shared"}: 10 * usdMicro, {Developer: "alice", Repo: "acme/app", IssueID: "growth"}: 4 * usdMicro})
	points, costs, fixedPoints, fixedCost := projectedCIInputs("alice", out, idx, 16)
	if len(points) != 2 || fixedPoints != 1 || fixedCost != 7 {
		t.Fatal(points, costs, fixedPoints, fixedCost)
	}
	sumP, sumC := fixedPoints, fixedCost
	for i := range points {
		sumP += points[i]
		sumC += costs[i]
	}
	if sumP != 9.25 || sumC != 16 {
		t.Fatal(sumP, sumC)
	}
	out[1], out[2] = out[2], out[1]
	p2, c2, f2, k2 := projectedCIInputs("alice", out, idx, 16)
	if !reflect.DeepEqual(points, p2) || !reflect.DeepEqual(costs, c2) || f2 != fixedPoints || k2 != fixedCost {
		t.Fatal("projected inputs changed with permutation", points, p2, costs, c2)
	}
}

type correctionQueryTrap struct{ Store }

func (correctionQueryTrap) ListCorrectionEvidence(context.Context, string, time.Time, time.Time, store.PageCursor, int) ([]map[string]any, bool, error) {
	panic("anonymized evidence query")
}
func TestCorrectionExport_AnonymizedRejectsBeforeQuery(t *testing.T) {
	for _, mode := range []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision} {
		h, _ := newTestHandler(t)
		h.aggregation = mode
		h.store = correctionQueryTrap{h.store}
		r := correctionGet(t, h, "/api/v1/corrections/relations?since=invalid")
		if r.Code != 403 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
}
func TestCorrectionExport_TextCSVSafeAndEvidencePagination(t *testing.T) {
	h, _ := correctionReaderFixture(t)
	r := correctionGet(t, h, "/api/v1/corrections/relations?since=2020-01-01&csv=true")
	rows, err := csv.NewReader(strings.NewReader(r.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i, key := range rows[0] {
		if key == "supporting_text" {
			found = true
			if rows[1][i] != "'=bounded evidence" {
				t.Fatal(rows[1][i])
			}
		}
	}
	if !found {
		t.Fatal(rows)
	}
	r = correctionGet(t, h, "/api/v1/corrections/allocations?since=2020-01-01&limit=1")
	b := decodeJSON(t, r)
	next, ok := b["next_cursor"].(string)
	if !ok || next == "" {
		t.Fatal(r.Body.String())
	}
	r2 := correctionGet(t, h, "/api/v1/corrections/allocations?since=2020-01-01&limit=1&cursor="+url.QueryEscape(next))
	b2 := decodeJSON(t, r2)
	if b2["next_cursor"] != "" || b["allocations"].([]any)[0].(map[string]any)["id"] == b2["allocations"].([]any)[0].(map[string]any)["id"] {
		t.Fatal(r2.Body.String())
	}
}
func TestCorrectionReaders_UnresolvedPolicyRefusesEmptyWindow(t *testing.T) {
	h, _ := newTestHandler(t)
	h.scoringPolicy = scoring.ScoringPolicy{Event3: true}
	for _, path := range []string{"/api/v1/scores?since=2020-01-01", "/api/v1/scores/alice?since=2020-01-01"} {
		r := correctionGet(t, h, path)
		if r.Code != 500 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
}
func TestCorrectionCompare_UsesSameProjectionAndCI(t *testing.T) {
	h, db := correctionReaderFixture(t)
	ctx := context.Background()
	for i, w := range []float64{1, 3} {
		at := time.Date(2026, 10, 4+i, 0, 0, 0, 0, time.UTC)
		issue := fmt.Sprint("normal-", i)
		sha := fmt.Sprint("normal-sha-", i)
		if _, err := db.InsertOutcome(ctx, store.Outcome{Developer: "alice", Repo: "acme/app", IssueID: issue, Weight: w, Quality: 1, Timestamp: at, MergeCommitSHA: sha}); err != nil {
			t.Fatal(err)
		}
		o, _, err := db.OutcomeByMergeCommit(ctx, sha)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.ReconcileCorrectionChain(ctx, o.ID); err != nil {
			t.Fatal(err)
		}
		seedCostAt(t, db, "alice", issue, float64(2+2*i), at)
	}
	seedCostAt(t, db, "alice", "repair", 10, time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC))
	list := correctionGet(t, h, "/api/v1/scores?since=2026-10-01&until=2026-11-01")
	b := decodeJSON(t, list)
	row := b["developers"].([]any)[0].(map[string]any)
	if row["ranked"] != true || row["sample_n"] != float64(3) || row["outcome_n"] != float64(4) {
		t.Fatal(list.Body.String())
	}
	compare := correctionGet(t, h, "/api/v1/scores/compare?since_a=2026-10-01&until_a=2026-11-01&since_b=2026-10-01&until_b=2026-11-01")
	if compare.Code != 200 {
		t.Fatal(compare.Code, compare.Body.String())
	}
	cb := decodeJSON(t, compare)
	side := cb["developers"].([]any)[0].(map[string]any)["a"].(map[string]any)
	for _, key := range []string{"weighted_points", "tier", "sample_n", "outcome_n", "ci_low", "ci_high", "ranked"} {
		if row[key] != side[key] {
			t.Fatalf("%s differs: %v %v", key, row[key], side[key])
		}
	}
	detail := correctionGet(t, h, "/api/v1/scores/alice?since=2026-10-01&until=2026-11-01")
	dr := decodeJSON(t, detail)
	for _, key := range []string{"weighted_points", "tier", "sample_n", "outcome_n", "ci_low", "ci_high", "ranked"} {
		if row[key] != dr[key] {
			t.Fatalf("detail %s differs: %v %v", key, row[key], dr[key])
		}
	}
}
func TestCorrectionExport_NullStatsRemainDisclosed(t *testing.T) {
	h, db := newTestHandler(t)
	h.scoringPolicy = scoring.ScoringPolicy{Event3: true, Resolved: true, RuleVersion: "event3-v2"}
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	o := store.Outcome{Developer: "alice", Repo: "acme/app", IssueID: "missing", Weight: 1, Quality: 1, Timestamp: at}
	if _, err := db.InsertOutcome(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	r := correctionGet(t, h, "/api/v1/outcomes?since=2026-10-01&until=2026-11-01")
	b := decodeJSON(t, r)
	row := b["outcomes"].([]any)[0].(map[string]any)
	if row["raw_additions"] != nil || row["raw_deletions"] != nil || row["raw_changed_files"] != nil || row["diff_stats_available"] != false || row["additions"] != float64(0) {
		t.Fatal(row)
	}
}
