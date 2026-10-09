package api

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

func TestExportCSVFormulaInjection(t *testing.T) {
	for _, tt := range []struct {
		name, value string
		dangerous   bool
	}{
		{"equals", "=1+1", true},
		{"plus", "+1+1", true},
		{"minus", "-1.5", true},
		{"at", "@SUM(1,2)", true},
		{"tab", "\t=1+1", true},
		{"carriage_return", "\r=1+1", true},
		{"empty", "", false},
		{"ordinary", "alice", false},
		{"quoted", "'=1+1", false},
		{"embedded", "alice,\"x\"\n=1+1", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.value
			h := &Handler{}
			events := []store.TokenEvent{{
				ID: -1, Developer: s, IssueID: s, Model: s,
				InputTok: -1, OutputTok: -1, CacheRead: -1, CacheWrite5m: -1, CacheWrite1h: -1,
				CostMicro: -1, Source: s, Fidelity: s, IdempotencyKey: s, Repo: s, SessionID: s,
				PriceVersion: -1, Host: s, BillingMode: s, BilledTo: s,
			}}
			outcomes := []store.Outcome{{
				ID: -1, Developer: s, IssueID: s, PRNumber: -1, Weight: -1.5, WeightSource: s,
				Quality: -1.5, MergeCommitSHA: s, Additions: -1, Deletions: -1, ChangedFiles: -1,
				Source: s, WorkType: s, WorkTypeSource: s, Repo: s, PushDay: s,
			}}
			qualityEvents := []store.QualityEvent{{
				ID: -1, OutcomeID: -1, Developer: s, IssueID: s, EventType: s, SourceRef: s,
			}}
			history := []store.QualityTransition{{
				ID: -1, OutcomeID: -1, Developer: s, IssueID: s,
				OldQuality: -1.5, NewQuality: -1.5, Reason: s, SourceRef: s,
			}}
			for _, export := range []struct {
				name, text, integers, decimals string
				writeCSV, writeJSON            func(http.ResponseWriter)
			}{
				{
					"events", "developer issue_id model source fidelity idempotency_key repo session_id host billing_mode billed_to",
					"id input_tokens output_tokens cache_read_tokens cache_write_5m_tokens cache_write_1h_tokens cost_micro price_version", "",
					func(w http.ResponseWriter) { h.writeEventsCSV(w, events, "") },
					func(w http.ResponseWriter) { h.writeEventsJSON(w, events, "") },
				},
				{
					"outcomes", "developer issue_id weight_source merge_commit_sha source work_type work_type_source repo push_day",
					"id pr_number additions deletions changed_files", "weight quality",
					func(w http.ResponseWriter) { h.writeOutcomesCSV(w, outcomes, "") },
					func(w http.ResponseWriter) { h.writeOutcomesJSON(w, outcomes, "") },
				},
				{
					"quality_events", "developer issue_id event_type source_ref", "id outcome_id", "",
					func(w http.ResponseWriter) { h.writeQualityEventsCSV(w, qualityEvents, "") },
					func(w http.ResponseWriter) { h.writeQualityEventsJSON(w, qualityEvents, "") },
				},
				{
					"quality_history", "developer issue_id reason source_ref", "id outcome_id", "old_quality new_quality",
					func(w http.ResponseWriter) { h.writeQualityHistoryCSV(w, history, "") },
					func(w http.ResponseWriter) { h.writeQualityHistoryJSON(w, history, "") },
				},
			} {
				t.Run(export.name, func(t *testing.T) {
					rec := httptest.NewRecorder()
					export.writeCSV(rec)
					rows, err := csv.NewReader(rec.Body).ReadAll()
					if err != nil || len(rows) != 2 {
						t.Fatalf("CSV rows = %v, err = %v", rows, err)
					}
					cells := make(map[string]string)
					for i, name := range rows[0] {
						cells[name] = rows[1][i]
					}
					rec = httptest.NewRecorder()
					export.writeJSON(rec)
					var page map[string]json.RawMessage
					if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
						t.Fatal(err)
					}
					var objects []map[string]any
					if err := json.Unmarshal(page[export.name], &objects); err != nil || len(objects) != 1 {
						t.Fatalf("JSON rows = %v, err = %v", objects, err)
					}
					check := func(columns string, jsonWant any, csvWant string) {
						t.Helper()
						for _, name := range strings.Fields(columns) {
							if got, ok := cells[name]; !ok || got != csvWant {
								t.Errorf("CSV %s = %q, want %q", name, got, csvWant)
							}
							if got := objects[0][name]; got != jsonWant {
								t.Errorf("JSON %s = %v, want %v", name, got, jsonWant)
							}
						}
					}
					want := s
					if tt.dangerous {
						want = "'" + s
					}
					check(export.text, s, want)
					check(export.integers, float64(-1), "-1")
					check(export.decimals, -1.5, "-1.5")
				})
			}
		})
	}
}
