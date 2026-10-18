package api

import (
	"encoding/csv"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

func TestEngineFixExportLegacyBytes(t *testing.T) {
	h, _ := newTestHandler(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	o := store.Outcome{ID: 7, Timestamp: at, Developer: "alice", IssueID: "issue-9", PRNumber: 22, Weight: 3, WeightSource: "label", Quality: 1, MergeCommitSHA: "merge-sha", Additions: 10, Deletions: 2, ChangedFiles: 1, Source: "github", WorkType: "bug", WorkTypeSource: "label", Repo: "acme/app", QualityStatus: "final"}
	a, b, c := 10, 2, 1
	o.SetDiffStats(&a, &b, &c)
	o.Projection = &store.OutcomeProjection{RawWeight: "3/1", AllocatedPoints: 1, RuleVersion: "event3-v2"}
	// Exact export fixtures preserve the 18-column prefix and append issue_rule,
	// including field order, empty push_day and the trailing newline.
	const legacyCSV = "id,ts,developer,issue_id,pr_number,weight,weight_source,quality,merge_commit_sha,additions,deletions,changed_files,source,work_type,work_type_source,repo,push_day,quality_status,issue_rule\n7,2026-10-01T12:00:00Z,alice,issue-9,22,3,label,1,merge-sha,10,2,1,github,bug,label,acme/app,,final,legacy\n"
	const legacyJSON = "{\"next_cursor\":\"\",\"outcomes\":[{\"id\":7,\"ts\":\"2026-10-01T12:00:00Z\",\"developer\":\"alice\",\"issue_id\":\"issue-9\",\"pr_number\":22,\"weight\":3,\"weight_source\":\"label\",\"quality\":1,\"merge_commit_sha\":\"merge-sha\",\"additions\":10,\"deletions\":2,\"changed_files\":1,\"source\":\"github\",\"work_type\":\"bug\",\"work_type_source\":\"label\",\"repo\":\"acme/app\",\"push_day\":\"\",\"quality_status\":\"final\",\"issue_rule\":\"legacy\"}]}\n"
	for _, tc := range []struct {
		name, want string
		write      func(*httptest.ResponseRecorder)
	}{
		{"csv", legacyCSV, func(w *httptest.ResponseRecorder) {
			h.writeOutcomesCSV(w, []store.Outcome{o}, "", scoring.ScoringPolicy{})
		}},
		{"json", legacyJSON, func(w *httptest.ResponseRecorder) {
			h.writeOutcomesJSON(w, []store.Outcome{o}, "", scoring.ScoringPolicy{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tc.write(w)
			if w.Body.String() != tc.want {
				t.Fatalf("legacy bytes differ:\ngot  %s\nwant %s", w.Body.String(), tc.want)
			}
		})
	}
}

func TestEngineFixExportEnabledRawEvidence(t *testing.T) {
	h, _ := newTestHandler(t)
	policy := scoring.ScoringPolicy{Event3: true, Resolved: true, RuleVersion: "event3-v2"}
	for _, available := range []bool{true, false} {
		o := store.Outcome{}
		if available {
			a, b, c := 10, 2, 1
			o.SetDiffStats(&a, &b, &c)
		}
		w := httptest.NewRecorder()
		h.writeOutcomesJSON(w, []store.Outcome{o}, "", policy)
		var body struct {
			Outcomes []map[string]any `json:"outcomes"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		row := body.Outcomes[0]
		for i, key := range []string{"raw_additions", "raw_deletions", "raw_changed_files", "diff_stats_available"} {
			v, ok := row[key]
			if !ok {
				t.Fatalf("missing %s: %s", key, w.Body.String())
			}
			if i == 3 {
				if v != available {
					t.Fatal(row)
				}
			} else if available {
				if v != []float64{10, 2, 1}[i] {
					t.Fatal(row)
				}
			} else if v != nil {
				t.Fatal(row)
			}
		}
		w = httptest.NewRecorder()
		h.writeOutcomesCSV(w, []store.Outcome{o}, "", policy)
		rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"10", "2", "1", "true"}
		if !available {
			want = []string{"", "", "", "false"}
		}
		for i, key := range []string{"raw_additions", "raw_deletions", "raw_changed_files", "diff_stats_available"} {
			if rows[0][18+i] != key || rows[1][18+i] != want[i] {
				t.Fatal(rows)
			}
		}
	}
}

func TestEngineFixManifestRejectsUnresolvedPolicy(t *testing.T) {
	h, _ := correctionReaderFixture(t)
	for _, policy := range []scoring.ScoringPolicy{{Event3: true}, {Event3: true, Resolved: true, RuleVersion: "wrong"}} {
		h.scoringPolicy = policy
		for _, route := range []string{"scores", "report_manifest"} {
			r := correctionGet(t, h, "/api/v1/"+route+"?since=2026-10-01&until=2026-11-01")
			if r.Code != 500 {
				t.Errorf("%s policy %+v: got %d, want 500: %s", route, policy, r.Code, r.Body.String())
			}
		}
	}
}
