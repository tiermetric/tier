package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
)

// #1228 / #1145: an ingest token authenticates the endpoint, not the
// developer field. Probe unrostered IDs separately from impersonating inactive
// rostered identities with existing outcomes but no spend. No outcome or
// hierarchy write is available to this token. The original suppression probe
// failed for rostered IDs; pin that measured limitation until #1145 repairs it.
func TestIngestToken_FabricatedIDsKFloor(t *testing.T) {
	for _, mode := range []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision} {
		for _, rostered := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/rostered=%t", mode, rostered), func(t *testing.T) {
				h, db := newTestHandlerWithScopes(t, scopeTestWrite, scopeTestRead)
				h.SetIngestToken(scopeTestIngest)
				h.SetAggregation(mode, 3)
				now := time.Now().UTC().Add(-time.Hour)
				seedKAnonDev(t, db, "eng", "real", 10, 2, now)
				if rostered {
					for _, dev := range []string{"inactive-a", "inactive-b"} {
						rosterOnly(t, db, "eng", dev)
						seedRepoOutcomeAt(t, db, repoAlpha, dev, "i-"+dev, 2, now)
					}
				}
				read := func() scoresResponse {
					t.Helper()
					code, body := doRequestWithHeader(t, h, http.MethodGet, "/api/v1/scores", nil, http.Header{"Authorization": []string{"Bearer " + scopeTestRead}})
					if code != http.StatusOK {
						t.Fatalf("scores=%d: %s", code, body)
					}
					var resp scoresResponse
					if err := json.Unmarshal(body, &resp); err != nil {
						t.Fatal(err)
					}
					return resp
				}
				count := 1
				if rostered {
					count = 3
				}
				assertGroupWithheld(t, read(), "eng", count)
				post := func(dev string) {
					t.Helper()
					ev := validEvent("fabricated-" + dev)
					ev["developer"] = dev
					ev["timestamp"] = now.Format(time.RFC3339)
					ev["cost_usd"] = 1.0
					ev["repo"] = repoAlpha
					ev["issue_id"] = "i-" + dev
					code, body := doRequestWithHeader(t, h, http.MethodPost, "/api/v1/events", []map[string]any{ev}, http.Header{"Authorization": []string{"Bearer " + scopeTestIngest}})
					if code != http.StatusCreated {
						t.Fatalf("ingest %s=%d: %s", dev, code, body)
					}
				}
				post("inactive-a")
				assertGroupWithheld(t, read(), "eng", count)
				post("inactive-b")
				resp := read()
				if !rostered {
					assertGroupWithheld(t, resp, "eng", 1)
					return
				}
				name := "eng"
				if mode == scoring.AggregationDivision {
					name = "div-eng"
				}
				if len(resp.Teams) != 1 || !teamPresent(resp.Teams, name) || len(resp.Developers) != 0 {
					t.Fatalf("want one name-free %s aggregate, got %+v", name, resp)
				}
				if resp.Total == nil || resp.Total.WeightedPoints != 6 || math.Abs(resp.Total.TotalCostUSD-10.021) > 1e-9 {
					t.Fatalf("want published 6 points / $10.021 (server-priced uploads), got %+v", resp.Total)
				}
				if resp.DataQuality != nil && resp.DataQuality.KAnonSuppressed != nil {
					t.Fatalf("unexpected suppression after impersonated rostered IDs cleared k: %+v", resp.DataQuality.KAnonSuppressed)
				}
			})
		}
	}
}
