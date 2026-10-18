package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/issueref"
	"github.com/tiermetric/tier/internal/store"
)

func TestRecoveryConservationAcrossStoreAndAPI(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := t.Context()
	at := time.Date(2026, 6, 15, 12, 0, 0, 123456789, time.UTC)
	merge := at.Add(time.Hour)
	opened := at.Add(-time.Hour)
	for i, issue := range []string{"issue-10", store.UnattributedIssueID, store.UnattributedIssueID, store.UnattributedIssueID, "issue-wrong"} {
		e := store.TokenEvent{Developer: "alice", Repo: "acme/a", IssueID: issue, CostMicro: int64(i+1) * 1000000, InputTok: 2000, Source: "jsonl", Fidelity: "realtime", Timestamp: at}
		if i == 1 {
			pr := 20
			e.PRNumber = &pr
		}
		if i == 2 || i == 4 {
			e.BranchHash = issueref.BranchHash("acme/a", "feature/topic")
		}
		if err := db.InsertTokenEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	issueOutcome(t, db, "acme/a", "alice", "issue-10", 4, 10, merge)
	fill := store.Outcome{Developer: "alice", Repo: "acme/a", IssueID: "issue-20", Weight: 4, Quality: 1, PRNumber: 20, Timestamp: merge, MergedAt: &merge, CreatedAt: &opened, Source: "backfill", MergeCommitSHA: "fill"}
	if _, err := db.RecordPROutcome(ctx, fill); err != nil {
		t.Fatal(err)
	}
	recovery := fill
	recovery.PRNumber = 30
	recovery.IssueID = "issue-30"
	recovery.MergeCommitSHA = "recovery"
	recovery.HeadBranchHash = issueref.BranchHash("acme/a", "feature/topic")
	if _, err := db.RecordPROutcome(ctx, recovery); err != nil {
		t.Fatal(err)
	}
	var beforePoints float64
	for _, enabled := range []bool{false, true} {
		readctx := store.WithReadTimeRecovery(ctx, enabled)
		events, err := db.EffectiveAttributionsWindow(readctx, at.Add(-time.Hour), merge.Add(time.Hour), store.FleetWide)
		if err != nil {
			t.Fatal(err)
		}
		pooled, err := db.DeveloperCostsWindow(readctx, at.Add(-time.Hour), merge.Add(time.Hour), store.FleetWide)
		if err != nil {
			t.Fatal(err)
		}
		var pooledMicro int64
		for _, c := range pooled {
			pooledMicro += c.TotalCostMicro
		}
		var total int64
		layers := map[store.AttributionLayer]bool{}
		for _, e := range events {
			total += e.CostMicro
			layers[e.Layer] = true
			if e.StoredIssueID == "issue-wrong" && e.IssueID != "issue-wrong" {
				t.Fatal(e)
			}
		}
		if total != pooledMicro || total != 15000000 || !layers[store.AttributionStored] || !layers[store.AttributionFill] || !layers[store.AttributionUnresolved] || (enabled && !layers[store.AttributionRecovery]) {
			t.Fatal(total, layers)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/issues?since=2026-06-01&until=2026-07-01", nil).WithContext(readctx)
		rec := httptest.NewRecorder()
		h.handleGetIssueScores(rec, req)
		if rec.Code != 200 {
			t.Fatal(rec.Body.String())
		}
		var report IssueReport
		if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		var cost, points float64
		marked := false
		for _, r := range report.Reconciliation {
			cost += r.CostUSD
			points += r.WeightedPoints
			if r.IssueID == "issue-30" && r.AttributionLayer == string(store.AttributionRecovery) {
				marked = true
				if r.ReadTimeRecoveryCostUSD != 3 {
					t.Fatal(r)
				}
			}
		}
		if cost != 15 || report.TotalCostUSD != 15 || report.AttributedCostUSD+report.UnattributedCostUSD != 15 || marked != enabled {
			t.Fatal(report)
		}
		if !enabled {
			beforePoints = points
		} else if points != beforePoints || report.AttributionNotice == "" || report.AsOf == "" {
			t.Fatal("pooled points or disclosure", points, beforePoints, report)
		}
		scoresReq := httptest.NewRequest(http.MethodGet, "/api/v1/scores?since=2026-06-01&until=2026-07-01", nil).WithContext(readctx)
		scoresRec := httptest.NewRecorder()
		h.handleGetScores(scoresRec, scoresReq)
		if scoresRec.Code != 200 {
			t.Fatal(scoresRec.Body.String())
		}
		var scores scoresResponse
		if err := json.Unmarshal(scoresRec.Body.Bytes(), &scores); err != nil {
			t.Fatal(err)
		}
		if (scores.AttributionLayer != "") != enabled {
			t.Fatal(scores.AttributionLayer)
		}
		wantFlags := 1
		if enabled {
			wantFlags = 0
		}
		if len(scores.Developers) != 1 || scores.Developers[0].FlaggedOutcomes != wantFlags {
			t.Fatalf("recovery eligibility not propagated: %s", scoresRec.Body.String())
		}
		if scores.Total == nil || scores.Total.TotalCostUSD != 15 || scores.Total.WeightedPoints != points {
			t.Fatalf("scores: %s", scoresRec.Body.String())
		}
	}
}

func TestRecoveryDisclosureHeader(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, enabled := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/issues?since=2026", nil).WithContext(store.WithReadTimeRecovery(t.Context(), enabled))
		rec := httptest.NewRecorder()
		h.requireRead(h.handleGetIssueScores)(rec, req)
		if (rec.Header().Get("X-TIER-Attribution") != "") != enabled {
			t.Fatal(rec.Header())
		}
	}
}

func TestIssuesDiscloseRecoveryBeforeAnyMatch(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, enabled := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/issues?since=2026-06-01&until=2026-07-01", nil).WithContext(store.WithReadTimeRecovery(t.Context(), enabled))
		rec := httptest.NewRecorder()
		h.handleGetIssueScores(rec, req)
		var report IssueReport
		if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if rec.Code != 200 || (report.AsOf != "") != enabled || (report.AttributionNotice != "") != enabled {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
}
func TestRecoveryHeaderAfterAuthAndAbsentOnSealed(t *testing.T) {
	h, _, s := newSealedReadHandler(t)
	sealPass(t, s)
	rec := httptest.NewRecorder()
	h.requireRead(h.handleGetScores)(rec, httptest.NewRequest(http.MethodGet, "/api/v1/scores?period=2026-05", nil))
	if rec.Code != 200 || rec.Header().Get("X-TIER-Attribution") != "" {
		t.Fatal(rec.Code, rec.Header())
	}
	h.apiToken = "secret"
	rec = httptest.NewRecorder()
	h.requireRead(h.handleGetIssueScores)(rec, httptest.NewRequest(http.MethodGet, "/api/v1/issues?since=2026-06-01", nil))
	if rec.Code != 401 || rec.Header().Get("X-TIER-Attribution") != "" {
		t.Fatal(rec.Code, rec.Header())
	}
}
