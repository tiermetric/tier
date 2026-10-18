package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

func TestEffectiveProjectionStoreToAPIConservation(t *testing.T) {
	h, db := newTestHandler(t)
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	for _, issue := range []string{"issue-1", store.UnattributedIssueID, store.UnattributedMainBucket, store.UnattributedNoIssueBucket, ""} {
		issueCost(t, db, "acme/a", "alice", issue, 1, at)
	}
	issueOutcome(t, db, "acme/a", "alice", "issue-1", 4, 1, at.Add(time.Hour))
	pr := 20
	if err := db.InsertTokenEvent(t.Context(), store.TokenEvent{Developer: "alice", Repo: "acme/a", IssueID: store.UnattributedIssueID, CostMicro: 2000000, InputTok: 2000, Source: "jsonl", Fidelity: "realtime", Timestamp: at, PRNumber: &pr}); err != nil {
		t.Fatal(err)
	}
	merged := at.Add(time.Hour)
	if _, err := db.RecordPROutcome(t.Context(), store.Outcome{Developer: "alice", Repo: "acme/a", IssueID: "issue-20", PRNumber: pr, Weight: 4, Quality: 1, Timestamp: merged, MergedAt: &merged, Source: "backfill", MergeCommitSHA: "fill"}); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		ctx := store.WithReadTimeRecovery(t.Context(), enabled)
		events, err := db.EffectiveAttributionsWindow(ctx, at.Add(-time.Hour), at.Add(2*time.Hour), store.FleetWide)
		if err != nil {
			t.Fatal(err)
		}
		var micro int64
		layers := map[store.AttributionLayer]bool{}
		for _, e := range events {
			micro += e.CostMicro
			layers[e.Layer] = true
		}
		if !layers[store.AttributionStored] || !layers[store.AttributionFill] || !layers[store.AttributionUnresolved] {
			t.Fatal(layers)
		}
		pooled, err := db.DeveloperCostsWindow(ctx, at.Add(-time.Hour), at.Add(2*time.Hour), store.FleetWide)
		if err != nil {
			t.Fatal(err)
		}
		var pooledMicro int64
		for _, c := range pooled {
			pooledMicro += c.TotalCostMicro
		}
		if micro != pooledMicro {
			t.Fatalf("projection=%d pooled=%d", micro, pooledMicro)
		}

		req := httptest.NewRequest(http.MethodGet, "/api/v1/issues?since=2026-06-01&until=2026-07-01", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.handleGetIssueScores(rec, req)
		if rec.Code != 200 {
			t.Fatal(rec.Body.String())
		}
		var report issueTestReport
		if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		var dollars, points float64
		for _, r := range report.Rows {
			dollars += r.Cost
			points += r.Points
		}
		if store.DollarsToMicro(dollars) != micro || report.Total != 7 || points != 8 {
			t.Fatalf("store=%d report=%+v points=%v", micro, report, points)
		}
	}
}

func TestEffectiveProjectionSealedBytesIndependentOfSwitch(t *testing.T) {
	h, db, s := newSealedReadHandler(t)
	sealPass(t, s)
	first := sealedGet(t, h, "/api/v1/scores?period=2026-05")
	if first.Code != 200 {
		t.Fatal(first.Body.String())
	}
	var stored []byte
	if err := rawSealStore(t, db).QueryRow(`SELECT body FROM sealed_report WHERE period_start LIKE '2026-05%'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/scores?period=2026-05", nil).WithContext(store.WithReadTimeRecovery(t.Context(), enabled))
		rec := httptest.NewRecorder()
		h.handleGetScores(rec, req)
		if rec.Code != 200 || !bytes.Equal(stored, rec.Body.Bytes()) {
			t.Fatalf("enabled=%v: %d %s", enabled, rec.Code, rec.Body.String())
		}
	}
}
