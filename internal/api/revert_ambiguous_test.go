package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

func TestRevertAmbiguous_ExportsUnknownNeverEvidence(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	id := seedQualityOutcome(t, db, "carol", "issue-42", "merge-ambiguous")
	_, err := db.AppendQualityEvent(ctx, store.QualityEvent{OutcomeID: id, Developer: "carol", IssueID: "issue-42", EventType: "revert_ambiguous", SourceRef: "revert-sha", Reason: "ambiguous", EventTS: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	rec := doExport(t, h, "/api/v1/quality_events?since=2020-01-01", nil)
	var resp qualityEventsExportResponse
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.QualityEvents) != 1 {
		t.Fatalf("events=%+v", resp.QualityEvents)
	}
	e := resp.QualityEvents[0]
	if e.Status != "unknown" || e.Evidence || e.Reason != "ambiguous" || e.SourceRef != "revert-sha" {
		t.Fatalf("event=%+v", e)
	}
	rec = doExport(t, h, "/api/v1/outcomes?since=2020-01-01", nil)
	var outcomes outcomesExportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &outcomes); err != nil {
		t.Fatal(err)
	}
	if len(outcomes.Outcomes) != 1 || outcomes.Outcomes[0].Quality != 1 || outcomes.Outcomes[0].QualityStatus != "unknown" {
		t.Fatalf("outcome=%s", rec.Body.String())
	}
	rec = doExport(t, h, "/api/v1/quality_events?since=2020-01-01", http.Header{"Accept": []string{"text/csv"}})
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil || len(records) != 2 {
		t.Fatalf("CSV=%v err=%v", records, err)
	}
	fields := map[string]string{}
	for i, key := range records[0] {
		fields[key] = records[1][i]
	}
	if fields["status"] != "unknown" || fields["evidence"] != "false" || fields["reason"] != "ambiguous" {
		t.Fatalf("CSV event=%+v", fields)
	}
	rec = doExport(t, h, "/api/v1/outcomes?since=2020-01-01", http.Header{"Accept": []string{"text/csv"}})
	records, err = csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil || len(records) != 2 {
		t.Fatalf("outcomes CSV=%v err=%v", records, err)
	}
	if records[0][17] != "quality_status" || records[1][17] != "unknown" {
		t.Fatalf("outcomes CSV status=%v", records)
	}
	exp, err := db.ExportDeveloper(ctx, "carol")
	if err != nil || len(exp.QualityEvents) != 1 {
		t.Fatalf("DSAR=%+v %v", exp.QualityEvents, err)
	}
	if e := exp.QualityEvents[0]; e.EventType != "revert_ambiguous" || e.Reason != "ambiguous" {
		t.Fatalf("DSAR event=%+v", e)
	}
}
