package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// seedCostAt inserts one realtime cost row at an EXACT instant, so the
// half-open [since, until) window tests (#276) can place events either side of
// a boundary. costUSD is charged whole to (developer, issueID); InputTok clears
// the #136 tripwire so the row is a realistic non-zero-token cost.
func seedCostAt(t *testing.T, db *store.DB, developer, issueID string, costUSD float64, ts time.Time) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: developer,
		IssueID:   issueID,
		Model:     "claude-sonnet-4",
		InputTok:  2000,
		CostMicro: store.DollarsToMicro(costUSD),
		Source:    "jsonl",
		Fidelity:  "realtime",
		Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent at %s: %v", ts.Format(time.RFC3339), err)
	}
}

// scoresTotalCostUSD GETs /scores with the given raw query and returns the
// server-computed total.total_cost_usd. The total block sums windowed cost
// across all developers, independent of the outcome join, so it is the cleanest
// probe for "which events fell inside the window".
func scoresTotalCostUSD(t *testing.T, h *Handler, query string) float64 {
	t.Helper()
	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores"+query, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores%s: status = %d; body = %s", query, code, body)
	}
	var resp struct {
		Total *struct {
			TotalCostUSD float64 `json:"total_cost_usd"`
		} `json:"total"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal /scores%s: %v; body = %s", query, err, body)
	}
	if resp.Total == nil {
		return 0
	}
	return resp.Total.TotalCostUSD
}

// TestGetScores_UntilBoundsWindow is the #276 acceptance test: with events at
// X-1s, X, Y-1s and Y, a request for [X, Y) must count exactly the X and Y-1s
// rows — the lower bound is inclusive, the upper bound exclusive. The same seed
// read WITHOUT until proves omission re-opens the upper bound (Y is then
// counted), i.e. until is what bounds the top.
func TestGetScores_UntilBoundsWindow(t *testing.T) {
	h, db := newTestHandler(t)

	x := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)                 // since (inclusive)
	y := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)                 // until (exclusive)
	seedCostAt(t, db, "alice", "issue-1", 10.0, x.Add(-time.Second)) // X-1s: excluded
	seedCostAt(t, db, "alice", "issue-2", 10.0, x)                   // X:    included
	seedCostAt(t, db, "alice", "issue-3", 10.0, y.Add(-time.Second)) // Y-1s: included
	seedCostAt(t, db, "alice", "issue-4", 10.0, y)                   // Y:    excluded

	// [X, Y): X and Y-1s only → $20.
	if got := scoresTotalCostUSD(t, h, "?since=2026-03-01&until=2026-04-01"); got != 20.0 {
		t.Errorf("bounded [X,Y) total_cost_usd = %v, want 20.0 (X and Y-1s only; X-1s below since, Y at/after until)", got)
	}
	// Omitted until re-opens the top: X, Y-1s and Y → $30 (X-1s still below since).
	if got := scoresTotalCostUSD(t, h, "?since=2026-03-01"); got != 30.0 {
		t.Errorf("open-ended total_cost_usd = %v, want 30.0 (until omitted must include Y)", got)
	}
	// `before=` is an accepted synonym for `until=` (before/after comparison).
	if got := scoresTotalCostUSD(t, h, "?since=2026-03-01&before=2026-04-01"); got != 20.0 {
		t.Errorf("before= alias total_cost_usd = %v, want 20.0 (must window identically to until=)", got)
	}
}

// TestGetScores_TeamMode_UntilBoundsWindow proves #276's until bound still
// applies in team-aggregation mode (#185) — the other honored mode in the
// acceptance criteria. Three seats (= the k floor) each carry an in-window and an
// out-of-window cost; the [X, Y) team rollup must sum only the in-window rows
// ($30, not $60), and the cohort must stay a NAMED "eng" team (3 contributors ≥
// k), so k-anonymity is exercised, not bypassed.
func TestGetScores_TeamMode_UntilBoundsWindow(t *testing.T) {
	h, db := newTeamModeHandler(t, 3)

	x := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	y := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	for _, dev := range []string{"alice", "bob", "carol"} {
		seedCostAt(t, db, dev, "in-"+dev, 10.0, x)  // in [X, Y)
		seedCostAt(t, db, dev, "out-"+dev, 10.0, y) // at Y: excluded
		if err := db.UpsertHierarchy(context.Background(), dev, "eng", "div", "org"); err != nil {
			t.Fatalf("UpsertHierarchy(%s): %v", dev, err)
		}
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?since=2026-03-01&until=2026-04-01", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /scores: status = %d; body = %s", code, body)
	}
	var resp scoresResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v; body = %s", err, body)
	}
	eng := teamByName(resp.Teams, "eng")
	if eng == nil {
		t.Fatalf("team mode with 3 contributors must emit a named 'eng' team (k floor 3); got %v", teamJSONNames(resp.Teams))
	}
	if eng.TotalCostUSD != 30.0 {
		t.Errorf("team 'eng' total_cost_usd = %v, want 30.0 (3×$10 in-window; each seat's Y row must be excluded by until)", eng.TotalCostUSD)
	}
}

// TestGetScores_UntilNotAfterSince_400 rejects a window that cannot hold any
// instant. until == since and until < since are both empty half-open ranges and
// must fail loud rather than return a silently-empty 200.
func TestGetScores_UntilNotAfterSince_400(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, q := range []string{
		"?since=2026-03-01&until=2026-03-01", // equal: empty range
		"?since=2026-04-01&until=2026-03-01", // inverted
	} {
		code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores"+q, nil)
		if code != http.StatusBadRequest {
			t.Errorf("GET /scores%s: status = %d, want 400; body = %s", q, code, body)
		}
	}
}

// TestGetScores_InvalidUntil_400 rejects an unparseable until with the same
// grammar contract as since.
func TestGetScores_InvalidUntil_400(t *testing.T) {
	h, _ := newTestHandler(t)
	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?until=not-a-date", nil)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", code, body)
	}
}

// TestGetDeveloperScore_UntilBoundsWindow mirrors the list-endpoint acceptance
// on the per-developer detail route: the same [since, until) window must bound
// that developer's total cost.
func TestGetDeveloperScore_UntilBoundsWindow(t *testing.T) {
	h, db := newTestHandler(t)

	x := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	y := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	seedCostAt(t, db, "alice", "issue-1", 10.0, x.Add(-time.Second)) // excluded
	seedCostAt(t, db, "alice", "issue-2", 10.0, x)                   // included
	seedCostAt(t, db, "alice", "issue-3", 10.0, y)                   // excluded

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores/alice?since=2026-03-01&until=2026-04-01", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", code, body)
	}
	var resp struct {
		TotalCostUSD float64 `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v; body = %s", err, body)
	}
	if resp.TotalCostUSD != 10.0 {
		t.Errorf("developer total_cost_usd = %v, want 10.0 (only the X row is in [X,Y))", resp.TotalCostUSD)
	}
}

// TestRetentionHorizon_FailsLoud pre-registers the #252 contract (#276): a
// window whose lower bound predates the retention horizon must FAIL LOUD (422),
// never silently underreport. With the horizon at its zero-value default (no
// retention configured — today's state) the same request succeeds.
func TestRetentionHorizon_FailsLoud(t *testing.T) {
	h, _ := newTestHandler(t)

	// Default (zero horizon): a deep-history window is answerable.
	if code, _ := doRequest(t, h, http.MethodGet, "/api/v1/scores?since=2026-01-01", nil); code != http.StatusOK {
		t.Fatalf("zero-horizon deep window: status = %d, want 200", code)
	}

	// Arm the horizon. A window reaching before it is unanswerable.
	h.SetRetentionHorizon(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	code, body := doRequest(t, h, http.MethodGet, "/api/v1/scores?since=2026-01-01", nil)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("window predating horizon: status = %d, want 422; body = %s", code, body)
	}

	// A window at or after the horizon stays answerable.
	if code, _ := doRequest(t, h, http.MethodGet, "/api/v1/scores?since=2026-07-01", nil); code != http.StatusOK {
		t.Errorf("window after horizon: status = %d, want 200", code)
	}
}

// TestParseUntil covers the grammar: empty means unbounded (zero Time), the
// same three date layouts as since parse to a UTC start-of-period instant, and
// anything else errors.
func TestParseUntil(t *testing.T) {
	if got, err := parseUntil(""); err != nil || !got.IsZero() {
		t.Errorf("parseUntil(\"\") = (%v, %v), want (zero, nil) — empty means unbounded", got, err)
	}
	want := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, s := range []string{"2026-03-01", "2026-03"} {
		got, err := parseUntil(s)
		if err != nil {
			t.Fatalf("parseUntil(%q) err = %v", s, err)
		}
		if !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("parseUntil(%q) = %v (loc %v), want %v UTC", s, got, got.Location(), want)
		}
	}
	if _, err := parseUntil("nonsense"); err == nil {
		t.Errorf("parseUntil(\"nonsense\") err = nil, want a parse error")
	}
}
