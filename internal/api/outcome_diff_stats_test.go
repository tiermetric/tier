package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestOutcomeRequest_OmittedStatsAreNotSizingEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stats  map[string]any
		known  [3]bool
		weight float64
	}{
		{"omitted", nil, [3]bool{}, 0.5},
		{"null", map[string]any{"additions": nil, "deletions": nil, "changed_files": nil}, [3]bool{}, 0.5},
		{"zero", map[string]any{"additions": 0, "deletions": 0, "changed_files": 0}, [3]bool{true, true, true}, 0.5},
		{"partial", map[string]any{"additions": 40, "deletions": nil, "changed_files": 0}, [3]bool{true, false, true}, 1},
		{"label", nil, [3]bool{}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, db := newTestHandler(t)
			body := validOutcome(tc.name, map[string]any{"additions": nil, "deletions": nil, "changed_files": nil})
			for k, v := range tc.stats {
				body[k] = v
			}
			if tc.name == "label" {
				body["weight"] = 3
			}
			data, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			var req outcomeRequest
			if err := json.Unmarshal(data, &req); err != nil {
				t.Fatal(err)
			}
			validated, err := validateOutcomeRequest(req, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			code, response := postOutcome(t, h, body)
			if code != http.StatusCreated {
				t.Fatalf("status = %d, body %s", code, response)
			}
			stored := outcomeBySHA(t, db, tc.name)
			exported, err := db.ExportDeveloper(context.Background(), "alice")
			if err != nil || len(exported.Outcomes) != 1 {
				t.Fatalf("export: %v, rows %d", err, len(exported.Outcomes))
			}
			e := exported.Outcomes[0]
			for i, stat := range [3]*int64{e.Additions, e.Deletions, e.ChangedFiles} {
				if (stat != nil) != tc.known[i] {
					t.Errorf("persisted stat %d availability = %v, want %v", i, stat != nil, tc.known[i])
				}
			}
			for _, o := range []any{validated, stored} {
				var got [3]*int
				if v, ok := o.(interface{ DiffStats() (*int, *int, *int) }); ok {
					got[0], got[1], got[2] = v.DiffStats()
				}
				for i, known := range tc.known {
					if (got[i] != nil) != known {
						t.Errorf("%T stat %d availability = %v, want %v", o, i, got[i] != nil, known)
					}
					if got[i] != nil && tc.stats != nil {
						fields := [3]string{"additions", "deletions", "changed_files"}
						if *got[i] != tc.stats[fields[i]].(int) {
							t.Errorf("stat %d = %d", i, *got[i])
						}
					}
				}
			}
			if stored.Weight != tc.weight || validated.Weight != tc.weight {
				t.Errorf("weights = %v,%v, want %v", stored.Weight, validated.Weight, tc.weight)
			}
			if code, _ := postOutcome(t, h, body); code != http.StatusOK {
				t.Errorf("replay status = %d", code)
			}
		})
	}
}
