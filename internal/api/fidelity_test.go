package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

func seedFidelityEvent(t *testing.T, db *store.DB, dev, source, fidelity, model string, cost int64, ts time.Time) {
	t.Helper()
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: dev, IssueID: "issue-1", Model: model,
		InputTok: 100, CostMicro: cost, Source: source, Fidelity: fidelity, Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}
}

// TestGetFidelity_EmptyDB proves the endpoint answers 200 with an empty developer
// list on a fresh install — the "nothing captured yet" case must not error.
func TestGetFidelity_EmptyDB(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doExport(t, h, "/api/v1/fidelity", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp fidelityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Developers) != 0 {
		t.Errorf("want no developers on empty DB, got %+v", resp.Developers)
	}
	if resp.Now == "" || resp.Since7d == "" || resp.Since30d == "" {
		t.Errorf("window bounds must be stamped even when empty: %+v", resp)
	}
}

// TestGetFidelity_Shape proves the 200 response carries per-developer counts,
// last-event-by-source, fidelity levels, and the unknown-model cost share.
func TestGetFidelity_Shape(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	seedFidelityEvent(t, db, "alice", "jsonl", "realtime", "claude-sonnet-4", 1_000_000, now.Add(-1*time.Hour))
	seedFidelityEvent(t, db, "alice", "jsonl", "realtime", "mystery-model", 3_000_000, now.Add(-2*time.Hour))
	seedFidelityEvent(t, db, "alice", "proxy", "estimated", "claude-sonnet-4", 500_000, now.Add(-10*24*time.Hour))

	rec := doExport(t, h, "/api/v1/fidelity", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp fidelityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Developers) != 1 {
		t.Fatalf("want 1 developer, got %+v", resp.Developers)
	}
	d := resp.Developers[0]
	if d.Developer != "alice" {
		t.Errorf("developer = %q, want alice", d.Developer)
	}
	if d.EventCount7d != 2 || d.EventCount30d != 3 {
		t.Errorf("counts 7d=%d 30d=%d, want 2/3", d.EventCount7d, d.EventCount30d)
	}
	if len(d.LastEventBySource) != 2 {
		t.Errorf("last_event_by_source = %v, want 2 sources", d.LastEventBySource)
	}
	// The jsonl value must be the MAX of alice's two jsonl events (-1h, not -2h).
	assertNear(t, d.LastEventBySource["jsonl"], now.Add(-1*time.Hour), "alice jsonl last")
	if d.FidelityLevels["realtime"] != 2 || d.FidelityLevels["estimated"] != 1 {
		t.Errorf("fidelity_levels = %v", d.FidelityLevels)
	}
	// unknown 3M / total 4.5M = 0.6667.
	if d.UnknownModelCostShare < 0.66 || d.UnknownModelCostShare > 0.67 {
		t.Errorf("unknown_model_cost_share = %v, want ~0.667", d.UnknownModelCostShare)
	}
}

// TestGetFidelity_AliasMerge proves raw developer keys sharing a canonical identity
// (#125) collapse into ONE row with summed counts.
func TestGetFidelity_AliasMerge(t *testing.T) {
	h, db := newTestHandler(t)
	now := time.Now().UTC()
	seedFidelityEvent(t, db, "alice", "jsonl", "realtime", "claude-sonnet-4", 1_000_000, now.Add(-1*time.Hour))
	seedFidelityEvent(t, db, "alice@old", "jsonl", "realtime", "claude-sonnet-4", 1_000_000, now.Add(-2*time.Hour))
	if err := db.UpsertDeveloperAlias(context.Background(), "alice@old", "alice"); err != nil {
		t.Fatalf("UpsertDeveloperAlias: %v", err)
	}

	rec := doExport(t, h, "/api/v1/fidelity", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp fidelityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Developers) != 1 {
		t.Fatalf("alias should merge into 1 row, got %+v", resp.Developers)
	}
	if resp.Developers[0].Developer != "alice" || resp.Developers[0].EventCount7d != 2 {
		t.Errorf("merged row = %+v, want alice with 2 events", resp.Developers[0])
	}
	// The merged jsonl last-seen must be the MAX across the two raw keys (alice at
	// -1h wins over alice@old at -2h), proving the cross-key merge takes the max.
	assertNear(t, resp.Developers[0].LastEventBySource["jsonl"], now.Add(-1*time.Hour), "merged jsonl last")
}

// TestGetFidelity_AuthScope proves the route is read-scoped (#190): an anonymous
// request is 401'd and the read-only viewer token is accepted, so a registration
// mistake (requireAuth instead of requireRead, or no wrap) is caught here.
func TestGetFidelity_AuthScope(t *testing.T) {
	h, _ := newTestHandlerWithScopes(t, "write-tok", "read-tok")
	// No credential → 401.
	if rec := doExport(t, h, "/api/v1/fidelity", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous status = %d, want 401", rec.Code)
	}
	// Read-only viewer token → 200 (read scope suffices).
	hdr := http.Header{"Authorization": {"Bearer read-tok"}}
	if rec := doExport(t, h, "/api/v1/fidelity", hdr); rec.Code != http.StatusOK {
		t.Errorf("read-token status = %d, want 200", rec.Code)
	}
}

// assertNear checks a RFC3339 timestamp string is within 2s of want — tolerant of
// the microsecond storage rounding and the second-precision RFC3339 formatting,
// while still distinguishing a ~1h-apart max from min.
func assertNear(t *testing.T, gotRFC3339 string, want time.Time, label string) {
	t.Helper()
	got, err := time.Parse(time.RFC3339, gotRFC3339)
	if err != nil {
		t.Fatalf("%s: unparseable timestamp %q: %v", label, gotRFC3339, err)
	}
	if d := got.Sub(want); d > 2*time.Second || d < -2*time.Second {
		t.Errorf("%s = %v, want ~%v (off by %v)", label, got, want, d)
	}
}

// TestGetFidelity_TeamModeForbidden proves the endpoint 403s in team-aggregation
// mode (#185): it names individuals, which team mode must suppress.
func TestGetFidelity_TeamModeForbidden(t *testing.T) {
	h, db := newTestHandler(t)
	h.SetAggregation(scoring.AggregationTeam, scoring.DefaultKAnonymity)
	seedFidelityEvent(t, db, "alice", "jsonl", "realtime", "claude-sonnet-4", 1_000_000, time.Now().UTC())

	rec := doExport(t, h, "/api/v1/fidelity", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}
