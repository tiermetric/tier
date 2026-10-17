package api

import (
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/scoring"
)

func TestCorrectionU2_R4FlagOffDetailOmitsProjection(t *testing.T) {
	h, _ := correctionReaderFixture(t)
	h.scoringPolicy = scoring.ScoringPolicy{}
	r := correctionGet(t, h, "/api/v1/scores/alice?since=2026-10-01&until=2026-11-01")
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	if strings.Contains(r.Body.String(), `"projection"`) {
		t.Fatal("flag-off detail leaked projection", r.Body.String())
	}
	if !strings.Contains(r.Body.String(), `"issues":[{`) {
		t.Fatal("missing detail issues", r.Body.String())
	}
}
func TestCorrectionU2_R6FlagOffExportOmitsProjection(t *testing.T) {
	h, _ := correctionReaderFixture(t)
	h.scoringPolicy = scoring.ScoringPolicy{}
	for _, suffix := range []string{"", "&csv=true"} {
		t.Run(suffix, func(t *testing.T) {
			r := correctionGet(t, h, "/api/v1/outcomes?since=2026-10-01&until=2026-11-01"+suffix)
			if r.Code != 200 {
				t.Fatal(r.Code, r.Body.String())
			}
			for _, key := range []string{"raw_weight", "allocated_points", "ranking_eligibility", "status", "disputed", "reviewed_own_weight", "marginal_credit", "credited_weight", "projected_quality", "pending_reason", "rule_version", "provisional", "frozen"} {
				if suffix == "" {
					rows := decodeJSON(t, r)["outcomes"].([]any)
					for _, row := range rows {
						if _, ok := row.(map[string]any)[key]; ok {
							t.Errorf("flag-off JSON contains %s", key)
						}
					}
				} else {
					rows, err := csv.NewReader(strings.NewReader(r.Body.String())).ReadAll()
					if err != nil {
						t.Fatal(err)
					}
					for _, col := range rows[0] {
						if col == key || col == "correction_"+key {
							t.Errorf("flag-off CSV contains %s", key)
						}
					}
				}
			}
		})
	}
}
func TestCorrectionU2_Y2EvidenceRequiresWriteToken(t *testing.T) {
	h, _ := newTestHandlerWithScopes(t, "write-token-of-sufficient-length", "read-token-of-sufficient-length")
	if code, body := doRequestWithHeader(t, h, http.MethodGet, "/api/v1/corrections/relations", nil, http.Header{"Authorization": {"Bearer read-token-of-sufficient-length"}}); code != 403 {
		t.Fatal(code, body)
	}
	mux := http.NewServeMux()
	h.RegisterReadOnly(mux)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/corrections/relations", nil)
	mux.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal("demo exposed evidence", w.Code)
	}
}
func TestCorrectionU2_Y8ManifestUsesScoreRubric(t *testing.T) {
	h, _ := correctionReaderFixture(t)
	r := correctionGet(t, h, "/api/v1/report_manifest?since=2026-10-01&until=2026-11-01")
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	if decodeJSON(t, r)["rubric"].(map[string]any)["version"] != float64(h.rubricVersion()) {
		t.Fatal(r.Body.String())
	}
}
