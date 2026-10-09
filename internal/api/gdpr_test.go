package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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

// TestHandleExportDeveloper_CarriesEveryTokenEventAndOutcomeColumn pins #805
// end to end through GET /api/v1/developer/{id}/export: a token_events row and
// an outcomes row stored with every column set to a non-default value (outcomes'
// push_day excepted — only the push path writes it) come back
// in the DSAR JSON carrying those values — in particular token_events' repo,
// session_id, price_version, host and billing_mode and outcomes' repo, which the
// export used to drop. It decodes to raw JSON objects so a missing key fails
// rather than decoding to a zero value. A second, session-blind alice row (no
// session_id, no idempotency_key — what the proxy and pollers store) must export
// both keys as JSON null, not "". A second developer's row, stored under a
// different session and repo, must not appear.
func TestHandleExportDeveloper_CarriesEveryTokenEventAndOutcomeColumn(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()
	ts := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	for _, e := range []store.TokenEvent{
		{Developer: "alice", IssueID: "issue-805", Model: "claude-sonnet-4",
			InputTok: 11, OutputTok: 12, CacheRead: 13, CacheWrite5m: 14, CacheWrite1h: 15,
			CostMicro: 16, Source: "jsonl", Fidelity: "realtime", IdempotencyKey: "idem-alice",
			Repo: "acme/widgets", SessionID: "sess-alice", PriceVersion: 7,
			Host: "openrouter.ai", BillingMode: "subscription", Timestamp: ts},
		{Developer: "alice", IssueID: "issue-805", Model: "claude-sonnet-4",
			CostMicro: 2, Source: "proxy", Fidelity: "realtime", Timestamp: ts},
		{Developer: "bob", IssueID: "issue-805", Model: "claude-sonnet-4",
			CostMicro: 1, Source: "jsonl", Fidelity: "realtime", IdempotencyKey: "idem-bob",
			Repo: "acme/bob-only", SessionID: "sess-bob", PriceVersion: 9,
			Host: "together.ai", BillingMode: "per_token", Timestamp: ts},
	} {
		if err := db.InsertTokenEvent(ctx, e); err != nil {
			t.Fatalf("InsertTokenEvent(%s): %v", e.Developer, err)
		}
	}
	for _, o := range []store.Outcome{
		{Developer: "alice", IssueID: "issue-805", PRNumber: 805, Weight: 2, WeightSource: store.WeightSourceLabel,
			Quality: 0.5, MergeCommitSHA: "sha-alice", Additions: 21, Deletions: 22, ChangedFiles: 23,
			Source: "api-outcome", WorkType: store.WorkTypeSecurity, WorkTypeSource: store.WorkTypeSourceLabel,
			Repo: "acme/widgets", Timestamp: ts},
		{Developer: "bob", IssueID: "issue-805", Weight: 1, Quality: 1,
			MergeCommitSHA: "sha-bob", Source: "api-outcome", Repo: "acme/bob-only", Timestamp: ts},
	} {
		if _, err := db.InsertOutcome(ctx, o); err != nil {
			t.Fatalf("InsertOutcome(%s): %v", o.Developer, err)
		}
	}

	code, body := doRequest(t, h, http.MethodGet, "/api/v1/developer/alice/export", nil)
	if code != http.StatusOK {
		t.Fatalf("export status = %d, want 200; body = %s", code, body)
	}
	var exp struct {
		TokenEvents []map[string]any `json:"token_events"`
		Outcomes    []map[string]any `json:"outcomes"`
	}
	if err := json.Unmarshal(body, &exp); err != nil {
		t.Fatalf("unmarshal export: %v", err)
	}
	if len(exp.TokenEvents) != 2 || len(exp.Outcomes) != 1 {
		t.Fatalf("export rows: token_events=%d outcomes=%d, want 2 and 1 (alice only); body = %s",
			len(exp.TokenEvents), len(exp.Outcomes), body)
	}

	// JSON numbers decode as float64.
	wantTE := map[string]any{
		"developer": "alice", "issue_id": "issue-805", "model": "claude-sonnet-4",
		"input_tok": float64(11), "output_tok": float64(12), "cache_read": float64(13),
		"cache_write_5m": float64(14), "cache_write_1h": float64(15), "cost_micro": float64(16),
		"source": "jsonl", "fidelity": "realtime", "idempotency_key": "idem-alice",
		"repo": "acme/widgets", "session_id": "sess-alice", "price_version": float64(7),
		"host": "openrouter.ai", "billing_mode": "subscription",
		"ts": ts.Format(time.RFC3339Nano),
	}
	assertExportRow(t, "token_events", exp.TokenEvents[0], wantTE)
	// Nullable columns stored as NULL must render as JSON null, never "".
	assertExportRow(t, "token_events (session-blind)", exp.TokenEvents[1],
		map[string]any{"session_id": nil, "idempotency_key": nil})

	wantOutcome := map[string]any{
		"developer": "alice", "issue_id": "issue-805", "pr_number": float64(805),
		"weight": float64(2), "weight_source": store.WeightSourceLabel, "quality": 0.5,
		"merge_commit_sha": "sha-alice", "additions": float64(21), "deletions": float64(22),
		"changed_files": float64(23), "source": "api-outcome",
		"work_type": store.WorkTypeSecurity, "work_type_source": store.WorkTypeSourceLabel,
		// push_day is written only by the source='push' insert path, so this
		// merged-PR row stores NULL and the key must be present as JSON null.
		"repo": "acme/widgets", "push_day": nil, "ts": ts.Format(time.RFC3339Nano),
	}
	assertExportRow(t, "outcomes", exp.Outcomes[0], wantOutcome)

	for _, leaked := range []string{"sess-bob", "acme/bob-only", "together.ai", "idem-bob", "sha-bob"} {
		if strings.Contains(string(body), leaked) {
			t.Errorf("alice's export contains bob's value %q; body = %s", leaked, body)
		}
	}
}

func assertExportRow(t *testing.T, table string, got, want map[string]any) {
	t.Helper()
	// id is server-assigned, so pin presence and a positive value, not a number.
	if id, ok := got["id"].(float64); !ok || id <= 0 {
		t.Errorf("%s export row id = %#v, want a positive integer; row = %v", table, got["id"], got)
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Errorf("%s export row has no %q key; row = %v", table, k, got)
			continue
		}
		if g != w {
			t.Errorf("%s export %q = %#v, want %#v", table, k, g, w)
		}
	}
}
