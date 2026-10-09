package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// insertEventAt puts one priced event in the store at a chosen instant, which is
// what makes an installation's cost horizon deterministic in a test.
func insertEventAt(t *testing.T, db *store.DB, source string, ts time.Time) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: "alice",
		IssueID:   "org/repo#1",
		Model:     "claude-sonnet-4",
		InputTok:  2000,
		CostMicro: store.DollarsToMicro(1.0),
		Source:    source,
		Fidelity:  "realtime",
		Timestamp: ts.UTC(),
	}); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}
}

func getScoresRaw(t *testing.T, h *Handler, query string) (map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scores"+query, nil)
	rec := httptest.NewRecorder()
	h.handleGetScores(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out, raw
}

func dataQualityOf(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	dq, ok := resp["data_quality"].(map[string]any)
	if !ok {
		t.Fatalf("no data_quality block in response: %v", resp)
	}
	return dq
}

// TestCostHorizonWireContract asserts what the SERVER actually puts on the wire
// (#512). The doctor-side decode test proves a struct tag round-trips a payload
// written by hand; it cannot notice if the handler stops producing that payload.
// Flipping WindowPredatesCostCapture from *bool to bool, or adding omitempty,
// leaves every other test in this change passing while the entire premise —
// "covered" must be distinguishable from "no signal" — silently breaks. This is
// the test that fails.
func TestCostHorizonWireContract(t *testing.T) {
	h, db := newTestHandler(t)
	horizon := time.Date(2026, 6, 23, 10, 1, 1, 0, time.UTC)
	insertEventAt(t, db, "jsonl", horizon)

	t.Run("covered window emits an EXPLICIT false, not an omitted field", func(t *testing.T) {
		resp, raw := getScoresRaw(t, h, "?since=2026-07-01")
		// The literal bytes matter, not the decoded value: `false` and "absent"
		// both decode to the zero value, and only the raw form tells them apart.
		if !strings.Contains(raw, `"window_predates_cost_capture":false`) {
			t.Errorf("covered window must emit an explicit false on the wire; body: %s", raw)
		}
		dq := dataQualityOf(t, resp)
		if got := dq["cost_coverage_start"]; got != "2026-06-23T10:01:01Z" {
			t.Errorf("cost_coverage_start = %v, want the horizon instant", got)
		}
	})

	t.Run("window predating the horizon reports true", func(t *testing.T) {
		resp, _ := getScoresRaw(t, h, "?since=2026-01-01")
		if got := dataQualityOf(t, resp)["window_predates_cost_capture"]; got != true {
			t.Errorf("window_predates_cost_capture = %v, want true", got)
		}
	})

	t.Run("safe_since is a value that actually CLEARS the warning", func(t *testing.T) {
		resp, _ := getScoresRaw(t, h, "?since=2026-01-01")
		safe, _ := dataQualityOf(t, resp)["cost_coverage_safe_since"].(string)
		if safe == "" {
			t.Fatal("no cost_coverage_safe_since emitted")
		}
		// The horizon falls at 10:01, so its own day is still too early. This is
		// the whole reason the field exists — assert the remedy by USING it.
		if safe == "2026-06-23" {
			t.Errorf("safe_since echoed the horizon's own day, which does not clear the flag")
		}
		follow, _ := getScoresRaw(t, h, "?since="+safe)
		if got := dataQualityOf(t, follow)["window_predates_cost_capture"]; got != false {
			t.Errorf("following the remedy (since=%s) still reports predates=%v — the advice does not work", safe, got)
		}
	})

	t.Run("source_coverage_start is omitted at one source and present at two", func(t *testing.T) {
		resp, _ := getScoresRaw(t, h, "?since=2026-07-01")
		if _, present := dataQualityOf(t, resp)["source_coverage_start"]; present {
			t.Error("a single-source store must omit source_coverage_start — it says nothing the global horizon did not")
		}

		insertEventAt(t, db, "codex-rollout", time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC))
		resp2, _ := getScoresRaw(t, h, "?since=2026-07-01")
		per, ok := dataQualityOf(t, resp2)["source_coverage_start"].(map[string]any)
		if !ok {
			t.Fatal("two sources must emit source_coverage_start")
		}
		if per["codex-rollout"] != "2026-07-15T00:00:00Z" || per["jsonl"] != "2026-06-23T10:01:01Z" {
			t.Errorf("per-source horizons wrong: %v", per)
		}
	})
}

// TestCostHorizonAbsentOnEmptyStore pins the one case where silence is correct:
// with no captured cost there is no horizon, and inventing one would assert
// coverage that does not exist. Distinct from a horizon QUERY FAILURE, which is
// also silent on the wire but is reported by doctor and the dashboard.
func TestCostHorizonAbsentOnEmptyStore(t *testing.T) {
	h, _ := newTestHandler(t)
	_, raw := getScoresRaw(t, h, "?since=2026-01-01")
	for _, field := range []string{"cost_coverage_start", "window_predates_cost_capture", "cost_coverage_safe_since"} {
		if strings.Contains(raw, field) {
			t.Errorf("empty store must not emit %s; body: %s", field, raw)
		}
	}
}

// TestSafeSinceDay covers the boundary the remedy turns on directly.
func TestSafeSinceDay(t *testing.T) {
	tests := []struct {
		name    string
		horizon time.Time
		want    string
	}{
		{
			// The common case: capture began part-way through a day, so that day is
			// only partly covered and the first FULLY covered day is the next one.
			name:    "mid-day horizon rolls to the next day",
			horizon: time.Date(2026, 6, 23, 10, 1, 1, 0, time.UTC),
			want:    "2026-06-24",
		},
		{
			// Exactly midnight is already fully covered — rolling forward here would
			// throw away a complete day of real data for no reason.
			name:    "midnight horizon keeps its own day",
			horizon: time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC),
			want:    "2026-06-23",
		},
		{
			name:    "one nanosecond past midnight still rolls",
			horizon: time.Date(2026, 6, 23, 0, 0, 0, 1, time.UTC),
			want:    "2026-06-24",
		},
		{
			// A non-UTC instant must be normalized before the day is taken, or the
			// remedy lands on the wrong date for anyone east or west of UTC.
			name:    "non-UTC input is normalized before truncation",
			horizon: time.Date(2026, 6, 23, 23, 30, 0, 0, time.FixedZone("plus2", 2*60*60)),
			want:    "2026-06-24", // 21:30Z on the 23rd
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeSinceDay(tt.horizon); got != tt.want {
				t.Errorf("safeSinceDay(%s) = %s, want %s", tt.horizon, got, tt.want)
			}
		})
	}
}
