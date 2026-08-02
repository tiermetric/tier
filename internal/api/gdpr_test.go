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

// seedDeveloperData populates several PII tables for dev through the store's
// public writers, so the GDPR handler tests exercise a realistic multi-table
// record (not just token_events).
func seedDeveloperData(t *testing.T, db *store.DB, dev string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer: dev, IssueID: "issue-1", Model: "claude-sonnet-4",
		InputTok: 2000, CostMicro: store.DollarsToMicro(0.01), Source: "jsonl",
		Fidelity: "realtime", Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertTokenEvent: %v", err)
	}
	if _, err := db.InsertOutcome(ctx, store.Outcome{
		Developer: dev, IssueID: "issue-1", Weight: 1.0, Quality: 1.0,
		MergeCommitSHA: "sha-" + dev, Source: "api-outcome", Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertOutcome: %v", err)
	}
	if err := db.InsertActualSpend(ctx, store.ActualSpend{
		Developer: dev, Period: "2026-05", ActualPaidMicro: 2000, Timestamp: now,
	}); err != nil {
		t.Fatalf("InsertActualSpend: %v", err)
	}
}

func TestHandleExportDeveloper(t *testing.T) {
	h, db := newTestHandler(t)
	seedDeveloperData(t, db, "alice")

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/developer/alice/export", nil)
	if code != http.StatusOK {
		t.Fatalf("export status = %d, want 200; body = %s", code, body)
	}
	var exp store.DeveloperExport
	if err := json.Unmarshal(body, &exp); err != nil {
		t.Fatalf("unmarshal export: %v", err)
	}
	if exp.Developer != "alice" {
		t.Errorf("Developer = %q, want alice", exp.Developer)
	}
	if len(exp.TokenEvents) == 0 || len(exp.Outcomes) == 0 || len(exp.ActualSpend) == 0 {
		t.Errorf("export missing rows: token_events=%d outcomes=%d actual_spend=%d",
			len(exp.TokenEvents), len(exp.Outcomes), len(exp.ActualSpend))
	}
}

func TestHandleExportDeveloper_NonExistent(t *testing.T) {
	h, _ := newTestHandler(t)
	code, _ := doRequest(t, h, http.MethodGet, "/api/v1/developer/ghost/export", nil)
	if code != http.StatusNotFound {
		t.Errorf("export of non-existent developer: status = %d, want 404", code)
	}
}

func TestHandleEraseDeveloper(t *testing.T) {
	h, db := newTestHandler(t)
	seedDeveloperData(t, db, "alice")

	code, body := doRequest(t, h, http.MethodDelete, "/api/v1/developer/alice", nil)
	if code != http.StatusOK {
		t.Fatalf("erase status = %d, want 200; body = %s", code, body)
	}
	var resp eraseDeveloperResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal erase response: %v", err)
	}
	if resp.TotalDeleted == 0 {
		t.Errorf("total_deleted = 0, want > 0; body = %s", body)
	}
	if resp.Deleted["token_events"] != 1 || resp.Deleted["outcomes"] != 1 || resp.Deleted["actual_spend"] != 1 {
		t.Errorf("per-table counts wrong: %+v", resp.Deleted)
	}

	// Export after erase must be 404 (nothing left).
	if code, _ := doRequest(t, h, http.MethodGet, "/api/v1/developer/alice/export", nil); code != http.StatusNotFound {
		t.Errorf("export after erase: status = %d, want 404", code)
	}

	// Second erase is idempotent: 404, no error.
	if code, _ := doRequest(t, h, http.MethodDelete, "/api/v1/developer/alice", nil); code != http.StatusNotFound {
		t.Errorf("second erase: status = %d, want 404 (idempotent)", code)
	}
}

func TestHandleEraseDeveloper_NonExistent(t *testing.T) {
	h, _ := newTestHandler(t)
	code, _ := doRequest(t, h, http.MethodDelete, "/api/v1/developer/ghost", nil)
	if code != http.StatusNotFound {
		t.Errorf("erase of non-existent developer: status = %d, want 404", code)
	}
}

// TestGDPREndpoints_AvailableInTeamMode pins the #185 carve-out: the erase and
// export endpoints are admin compliance tooling, so they STAY available in
// team-aggregation mode — unlike GET /scores/{developer}, which blanket-404s
// there. An operator must be able to fulfil a DSAR/erasure regardless of the
// dashboard's reporting mode.
func TestGDPREndpoints_AvailableInTeamMode(t *testing.T) {
	h, db := newTestHandler(t)
	h.SetAggregation(scoring.AggregationTeam, 5)
	seedDeveloperData(t, db, "alice")

	// The reporting surface IS suppressed in team mode (contrast control).
	if code, _ := doRequest(t, h, http.MethodGet, "/api/v1/scores/alice", nil); code != http.StatusNotFound {
		t.Errorf("GET /scores/alice in team mode: status = %d, want 404 (reporting suppressed)", code)
	}

	// Export remains available.
	if code, body := doRequest(t, h, http.MethodGet, "/api/v1/developer/alice/export", nil); code != http.StatusOK {
		t.Errorf("export in team mode: status = %d, want 200; body = %s", code, body)
	}
	// Erase remains available.
	if code, body := doRequest(t, h, http.MethodDelete, "/api/v1/developer/alice", nil); code != http.StatusOK {
		t.Errorf("erase in team mode: status = %d, want 200; body = %s", code, body)
	}
}

// TestGDPREndpoints_RejectReadToken pins the #190 boundary: both endpoints
// disclose/destroy an individual PII record, so the read-only viewer token is
// rejected 403, while the write/admin token is accepted. No token → 401.
func TestGDPREndpoints_RejectReadToken(t *testing.T) {
	const writeToken = "write-admin-token-of-len-32-aaaa"
	const readToken = "read-viewer-token-of-len-32-bbbb"
	bearer := func(tok string) http.Header {
		if tok == "" {
			return http.Header{}
		}
		return http.Header{"Authorization": []string{"Bearer " + tok}}
	}

	routes := []struct {
		method    string
		target    string
		wantWrite int
	}{
		{http.MethodDelete, "/api/v1/developer/alice", http.StatusOK},
		{http.MethodGet, "/api/v1/developer/alice/export", http.StatusOK},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.target, func(t *testing.T) {
			h, db := newTestHandlerWithScopes(t, writeToken, readToken)
			seedDeveloperData(t, db, "alice")

			if code, body := doRequestWithHeader(t, h, rt.method, rt.target, nil, bearer(readToken)); code != http.StatusForbidden {
				t.Errorf("read token: status = %d, want 403; body = %s", code, body)
			}
			if code, _ := doRequestWithHeader(t, h, rt.method, rt.target, nil, bearer("")); code != http.StatusUnauthorized {
				t.Errorf("no token: status = %d, want 401", code)
			}
			if code, body := doRequestWithHeader(t, h, rt.method, rt.target, nil, bearer(writeToken)); code != rt.wantWrite {
				t.Errorf("write token: status = %d, want %d; body = %s", code, rt.wantWrite, body)
			}
		})
	}
}
