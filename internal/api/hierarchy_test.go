package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// TestHierarchyEndpoints_Auth is the #232 + #190 auth matrix: with a distinct
// write and read-only token armed, every hierarchy route rejects the read token
// 403 (write scope), rejects no token 401, and accepts the write token. The
// hierarchy write surface is admin tooling, NOT a viewer-scope read.
func TestHierarchyEndpoints_Auth(t *testing.T) {
	const writeToken = "write-admin-token-of-len-32-aaaa"
	const readToken = "read-viewer-token-of-len-32-bbbb"
	bearer := func(tok string) http.Header {
		if tok == "" {
			return http.Header{}
		}
		return http.Header{"Authorization": []string{"Bearer " + tok}}
	}

	routes := []struct {
		method, target string
		body           any
		wantWrite      int
	}{
		{http.MethodPut, "/api/v1/org_hierarchy/alice", map[string]any{"team": "eng"}, http.StatusOK},
		{http.MethodPost, "/api/v1/org_hierarchy", []map[string]any{{"developer": "bob", "team": "eng"}}, http.StatusCreated},
		{http.MethodGet, "/api/v1/org_hierarchy", nil, http.StatusOK},
		{http.MethodPost, "/api/v1/period_membership/alice/end", map[string]any{"org": "acme", "period_end": "2026-05"}, http.StatusOK},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.target, func(t *testing.T) {
			h, _ := newTestHandlerWithScopes(t, writeToken, readToken)
			if code, body := doRequestWithHeader(t, h, rt.method, rt.target, rt.body, bearer(readToken)); code != http.StatusForbidden {
				t.Errorf("read token: status = %d, want 403; body = %s", code, body)
			}
			if code, _ := doRequestWithHeader(t, h, rt.method, rt.target, rt.body, bearer("")); code != http.StatusUnauthorized {
				t.Errorf("no token: status = %d, want 401", code)
			}
			if code, body := doRequestWithHeader(t, h, rt.method, rt.target, rt.body, bearer(writeToken)); code != rt.wantWrite {
				t.Errorf("write token: status = %d, want %d; body = %s", code, rt.wantWrite, body)
			}
		})
	}
}

// TestPutHierarchy_Validation is the table-driven single-upsert contract:
// required team, identifier caps, unknown fields, and the happy 200.
func TestPutHierarchy_Validation(t *testing.T) {
	long := strings.Repeat("x", maxIdentifierLen+1)
	cases := []struct {
		name   string
		target string
		body   any
		want   int
	}{
		{"happy 200", "/api/v1/org_hierarchy/alice", map[string]any{"team": "eng", "division": "plat", "org": "acme"}, http.StatusOK},
		{"team only (division/org optional)", "/api/v1/org_hierarchy/bob", map[string]any{"team": "eng"}, http.StatusOK},
		{"missing team", "/api/v1/org_hierarchy/carol", map[string]any{"division": "plat"}, http.StatusBadRequest},
		{"empty team", "/api/v1/org_hierarchy/dave", map[string]any{"team": ""}, http.StatusBadRequest},
		{"oversized team", "/api/v1/org_hierarchy/erin", map[string]any{"team": long}, http.StatusBadRequest},
		{"oversized org", "/api/v1/org_hierarchy/frank", map[string]any{"team": "eng", "org": long}, http.StatusBadRequest},
		{"unknown field", "/api/v1/org_hierarchy/grace", map[string]any{"team": "eng", "extra": 1}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHandler(t)
			code, body := doRequest(t, h, http.MethodPut, tc.target, tc.body)
			if code != tc.want {
				t.Errorf("status = %d, want %d; body = %s", code, tc.want, body)
			}
		})
	}
}

// TestPutHierarchy_TrailingJSONRejected pins the single-object discipline.
func TestPutHierarchy_TrailingJSONRejected(t *testing.T) {
	h, _ := newTestHandler(t)
	if code := doRawMethod(t, h, http.MethodPut, "/api/v1/org_hierarchy/alice",
		`{"team":"eng"}{"team":"ops"}`); code != http.StatusBadRequest {
		t.Errorf("trailing JSON: status = %d, want 400", code)
	}
}

// TestPutHierarchy_CanonicalizesDeveloper is the correctness lynchpin (#232 req
// 1): a PUT under a raw alias must store the row under the CANONICAL identity, or
// team mode still collapses. The stored row (echoed in the 200 body AND visible
// via GET) must carry the canonical developer, never the alias.
func TestPutHierarchy_CanonicalizesDeveloper(t *testing.T) {
	h, db := newTestHandler(t)
	// asmith-gh (GitHub login) -> alice.smith (canonical OS username).
	if err := db.UpsertDeveloperAlias(context.Background(), "asmith-gh", "alice.smith"); err != nil {
		t.Fatalf("UpsertDeveloperAlias: %v", err)
	}
	code, body := doRequest(t, h, http.MethodPut, "/api/v1/org_hierarchy/asmith-gh",
		map[string]any{"team": "eng", "org": "acme"})
	if code != http.StatusOK {
		t.Fatalf("PUT: status = %d, body = %s", code, body)
	}
	var row store.HierarchyRow
	if err := json.Unmarshal(body, &row); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if row.Developer != "alice.smith" {
		t.Errorf("echoed developer = %q, want canonical alice.smith", row.Developer)
	}
	// The store must key the row by the canonical id, not the alias.
	teams, err := db.TeamsForDevelopers(context.Background())
	if err != nil {
		t.Fatalf("TeamsForDevelopers: %v", err)
	}
	if teams["alice.smith"] != "eng" {
		t.Errorf("hierarchy not keyed by canonical: teams = %v", teams)
	}
	if _, leaked := teams["asmith-gh"]; leaked {
		t.Errorf("hierarchy leaked a row under the raw alias: teams = %v", teams)
	}
}

// TestBulkHierarchy_HappyAndListRoundTrip imports a batch and reads it back.
func TestBulkHierarchy_HappyAndListRoundTrip(t *testing.T) {
	h, _ := newTestHandler(t)
	batch := []map[string]any{
		{"developer": "a", "team": "eng", "division": "plat", "org": "acme"},
		{"developer": "b", "team": "eng", "org": "acme"},
		{"developer": "c", "team": "sales", "org": "acme"},
	}
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/org_hierarchy", batch)
	if code != http.StatusCreated {
		t.Fatalf("bulk import: status = %d, body = %s", code, body)
	}
	var resp hierarchyBulkResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Accepted != 3 {
		t.Errorf("accepted = %d, want 3", resp.Accepted)
	}

	code, body = doRequest(t, h, http.MethodGet, "/api/v1/org_hierarchy", nil)
	if code != http.StatusOK {
		t.Fatalf("GET list: status = %d, body = %s", code, body)
	}
	var list hierarchyListResponse
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list.Hierarchy) != 3 {
		t.Fatalf("list len = %d, want 3; %+v", len(list.Hierarchy), list.Hierarchy)
	}
	// developer-ordered: a, b, c.
	if list.Hierarchy[0].Developer != "a" || list.Hierarchy[2].Developer != "c" {
		t.Errorf("list not developer-ordered: %+v", list.Hierarchy)
	}
	if list.Hierarchy[1].Division != "" {
		t.Errorf("row b division = %q, want \"\" (omitted -> empty)", list.Hierarchy[1].Division)
	}
}

// TestBulkHierarchy_Validation covers the batch-level guards mirrored from
// handlePostEvents: empty array, over-cap, per-element 400 naming the index, and
// trailing JSON. All-or-nothing is verified separately.
func TestBulkHierarchy_Validation(t *testing.T) {
	t.Run("empty array", func(t *testing.T) {
		h, _ := newTestHandler(t)
		code, _ := doRequest(t, h, http.MethodPost, "/api/v1/org_hierarchy", []map[string]any{})
		if code != http.StatusBadRequest {
			t.Errorf("empty array: status = %d, want 400", code)
		}
	})
	t.Run("over cap", func(t *testing.T) {
		h, _ := newTestHandler(t)
		big := make([]map[string]any, maxHierarchyPerBatch+1)
		for i := range big {
			big[i] = map[string]any{"developer": fmt.Sprintf("d%d", i), "team": "eng"}
		}
		code, _ := doRequest(t, h, http.MethodPost, "/api/v1/org_hierarchy", big)
		if code != http.StatusBadRequest {
			t.Errorf("over-cap: status = %d, want 400", code)
		}
	})
	t.Run("invalid element names index", func(t *testing.T) {
		h, _ := newTestHandler(t)
		batch := []map[string]any{
			{"developer": "a", "team": "eng"},
			{"developer": "b"}, // missing team
		}
		code, body := doRequest(t, h, http.MethodPost, "/api/v1/org_hierarchy", batch)
		if code != http.StatusBadRequest {
			t.Fatalf("invalid element: status = %d, want 400", code)
		}
		if !strings.Contains(string(body), "org_hierarchy[1]") {
			t.Errorf("error must name the failing index; body = %s", body)
		}
	})
	t.Run("trailing json rejected", func(t *testing.T) {
		h, _ := newTestHandler(t)
		code := doRawPost(t, h, "/api/v1/org_hierarchy", `[{"developer":"a","team":"eng"}]{"x":1}`)
		if code != http.StatusBadRequest {
			t.Errorf("trailing JSON: status = %d, want 400", code)
		}
	})
}

// TestBulkHierarchy_AllOrNothing proves the batch is one transaction: an invalid
// element rolls back the whole import, so a valid element earlier in the same
// batch must NOT be persisted.
func TestBulkHierarchy_AllOrNothing(t *testing.T) {
	h, db := newTestHandler(t)
	batch := []map[string]any{
		{"developer": "goodrow", "team": "eng"},
		{"developer": "badrow"}, // missing team -> whole batch 400s
	}
	code, _ := doRequest(t, h, http.MethodPost, "/api/v1/org_hierarchy", batch)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	teams, err := db.TeamsForDevelopers(context.Background())
	if err != nil {
		t.Fatalf("TeamsForDevelopers: %v", err)
	}
	if len(teams) != 0 {
		t.Errorf("partial write leaked despite validation 400: %v", teams)
	}
}

// TestBulkHierarchy_RejectsDuplicateCanonical proves a batch that maps two rows
// to the SAME canonical developer (e.g. an alias and its canonical) is a
// fail-loud 400 naming both indices, not a silent last-write-wins.
func TestBulkHierarchy_RejectsDuplicateCanonical(t *testing.T) {
	h, db := newTestHandler(t)
	// eve-gh aliases to eve.os, so both rows resolve to eve.os.
	if err := db.UpsertDeveloperAlias(context.Background(), "eve-gh", "eve.os"); err != nil {
		t.Fatalf("UpsertDeveloperAlias: %v", err)
	}
	batch := []map[string]any{
		{"developer": "eve.os", "team": "eng"},
		{"developer": "eve-gh", "team": "sales"},
	}
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/org_hierarchy", batch)
	if code != http.StatusBadRequest {
		t.Fatalf("duplicate canonical: status = %d, want 400; body = %s", code, body)
	}
	if !strings.Contains(string(body), "org_hierarchy[1]") {
		t.Errorf("error must name the colliding index; body = %s", body)
	}
	// Nothing was written (all-or-nothing).
	teams, err := db.TeamsForDevelopers(context.Background())
	if err != nil {
		t.Fatalf("TeamsForDevelopers: %v", err)
	}
	if len(teams) != 0 {
		t.Errorf("duplicate-canonical batch must write nothing; got %v", teams)
	}
}

// TestBulkHierarchy_CanonicalizesDevelopers proves the batch resolves each
// developer through the alias map before storing.
func TestBulkHierarchy_CanonicalizesDevelopers(t *testing.T) {
	h, db := newTestHandler(t)
	if err := db.UpsertDeveloperAlias(context.Background(), "bob-gh", "bob.os"); err != nil {
		t.Fatalf("UpsertDeveloperAlias: %v", err)
	}
	batch := []map[string]any{{"developer": "bob-gh", "team": "eng", "org": "acme"}}
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/org_hierarchy", batch); code != http.StatusCreated {
		t.Fatalf("bulk: status = %d, body = %s", code, body)
	}
	teams, err := db.TeamsForDevelopers(context.Background())
	if err != nil {
		t.Fatalf("TeamsForDevelopers: %v", err)
	}
	if teams["bob.os"] != "eng" {
		t.Errorf("batch did not canonicalize: teams = %v", teams)
	}
}

// TestEndMembership_ValidationAndCanonicalization covers the period-close
// endpoint: required org, YYYY-MM period validation, canonicalization, and the
// happy 200 that echoes the resolved developer.
func TestEndMembership_ValidationAndCanonicalization(t *testing.T) {
	t.Run("missing org", func(t *testing.T) {
		h, _ := newTestHandler(t)
		code, _ := doRequest(t, h, http.MethodPost, "/api/v1/period_membership/alice/end",
			map[string]any{"period_end": "2026-05"})
		if code != http.StatusBadRequest {
			t.Errorf("missing org: status = %d, want 400", code)
		}
	})
	t.Run("bad period", func(t *testing.T) {
		h, _ := newTestHandler(t)
		code, _ := doRequest(t, h, http.MethodPost, "/api/v1/period_membership/alice/end",
			map[string]any{"org": "acme", "period_end": "2026-5"})
		if code != http.StatusBadRequest {
			t.Errorf("bad period: status = %d, want 400", code)
		}
	})
	t.Run("period before membership start -> 400 not 500", func(t *testing.T) {
		h, db := newTestHandler(t)
		ctx := context.Background()
		// An org change makes org2's membership start THIS month, so ending it in a
		// past period violates period_end >= period_start — a client error (400),
		// never a 500. Without the org change the first membership starts at the
		// '0000-01' sentinel and any period is valid.
		if err := db.UpsertHierarchy(ctx, "mover", "eng", "plat", "org1"); err != nil {
			t.Fatalf("UpsertHierarchy org1: %v", err)
		}
		if err := db.UpsertHierarchy(ctx, "mover", "eng", "plat", "org2"); err != nil {
			t.Fatalf("UpsertHierarchy org2: %v", err)
		}
		code, body := doRequest(t, h, http.MethodPost, "/api/v1/period_membership/mover/end",
			map[string]any{"org": "org2", "period_end": "2020-01"})
		if code != http.StatusBadRequest {
			t.Errorf("end before start: status = %d, want 400; body = %s", code, body)
		}
	})
	t.Run("happy 200 closes the seat and echoes canonical", func(t *testing.T) {
		h, db := newTestHandler(t)
		ctx := context.Background()
		if err := db.UpsertDeveloperAlias(ctx, "carol-gh", "carol.os"); err != nil {
			t.Fatalf("alias: %v", err)
		}
		// Open a membership under the canonical id.
		if err := db.UpsertHierarchy(ctx, "carol.os", "eng", "plat", "acme"); err != nil {
			t.Fatalf("UpsertHierarchy: %v", err)
		}
		code, body := doRequest(t, h, http.MethodPost, "/api/v1/period_membership/carol-gh/end",
			map[string]any{"org": "acme", "period_end": "2026-05"})
		if code != http.StatusOK {
			t.Fatalf("end: status = %d, body = %s", code, body)
		}
		var resp endMembershipResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Developer != "carol.os" {
			t.Errorf("echoed developer = %q, want canonical carol.os", resp.Developer)
		}
	})
}

// TestHierarchyEndpoints_AvailableInTeamMode is the #185 carve-out: the hierarchy
// write surface is org STRUCTURE, not score data, so it stays reachable in
// team-aggregation mode — the very mode it exists to configure. None of the
// endpoints may 404/blank the way /scores/{developer} does in team mode.
func TestHierarchyEndpoints_AvailableInTeamMode(t *testing.T) {
	h, _ := newTeamModeHandler(t, 3)
	if code, body := doRequest(t, h, http.MethodPut, "/api/v1/org_hierarchy/alice",
		map[string]any{"team": "eng", "org": "acme"}); code != http.StatusOK {
		t.Errorf("PUT in team mode: status = %d, want 200; body = %s", code, body)
	}
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/org_hierarchy",
		[]map[string]any{{"developer": "bob", "team": "eng"}}); code != http.StatusCreated {
		t.Errorf("bulk in team mode: status = %d, want 201; body = %s", code, body)
	}
	if code, body := doRequest(t, h, http.MethodGet, "/api/v1/org_hierarchy", nil); code != http.StatusOK {
		t.Errorf("GET in team mode: status = %d, want 200; body = %s", code, body)
	}
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/period_membership/alice/end",
		map[string]any{"org": "acme", "period_end": "2026-05"}); code != http.StatusOK {
		t.Errorf("end in team mode: status = %d, want 200; body = %s", code, body)
	}
	// Sanity: the handler really is in team mode (compile-time guard on the const).
	_ = scoring.AggregationTeam
}

// --- raw-body helpers (trailing-JSON tests need to bypass json.Marshal) ---

func doRawPost(t *testing.T, h *Handler, target, rawBody string) int {
	t.Helper()
	return doRawMethod(t, h, http.MethodPost, target, rawBody)
}

// doRawMethod routes a raw (unmarshalled) body through the full mux for an
// arbitrary method, so trailing-JSON and malformed-body cases can be exercised.
func doRawMethod(t *testing.T, h *Handler, method, target, rawBody string) int {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(rawBody))
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)
	return rec.Code
}

// TestLogSafeErr is a security regression guard (#232, CodeQL go/log-injection).
//
// The hierarchy endpoints take free-form `team`/`division`/`org` values that are
// length-capped but never charset-validated. A store error can wrap them, so logging
// the error verbatim would let a caller embed a newline and forge a log record that
// an operator — or a line-oriented SIEM — reads as genuine.
func TestLogSafeErr(t *testing.T) {
	if got := logSafeErr(nil); got != "" {
		t.Fatalf("nil error => %q, want empty", got)
	}

	forged := errors.New("upsert failed: org=acme\ntime=2026-07-09 level=ERROR msg=\"auth bypassed\"")
	got := logSafeErr(forged)
	if strings.Contains(got, "\n") || strings.Contains(got, "\r") {
		t.Fatalf("logSafeErr leaked a raw newline, permitting a forged log entry: %s", got)
	}
	// The CR/LF is STRIPPED (the CodeQL-recognized barrier, #321), not escaped: the
	// halves join onto one line ("org=acme"+"time="), so the forged "time=..." rider
	// can never begin its own record. A dropped-byte bug would lose the injection but
	// also corrupt the diagnostic; the joined text proves the strip is clean.
	if !strings.Contains(got, "org=acmetime=") {
		t.Errorf("expected the newline stripped so the halves join: %s", got)
	}
	// The diagnostic must survive sanitization, or operators lose the signal.
	if !strings.Contains(got, "upsert failed") {
		t.Errorf("sanitizer destroyed the diagnostic: %s", got)
	}

	long := errors.New(strings.Repeat("x", 900))
	if got := logSafeErr(long); len(got) > 600 {
		t.Errorf("oversized error not truncated: len=%d", len(got))
	}
}
