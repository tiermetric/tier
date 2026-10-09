package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// storedCostMicro reads back the single developer-cost row for `developer` and
// returns its stored cost_micro. The #295 tests use it to assert re-posts do not
// double-count and that a rejected (409) divergent re-post leaves cost_micro
// untouched. The divergent-409 case itself lives in
// TestPostCosts_SameKeyDivergentCost_Returns409 (handler_test.go).
func storedCostMicro(t *testing.T, db *store.DB, developer string) int64 {
	t.Helper()
	costs, err := db.DeveloperCosts(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	for _, c := range costs {
		if c.Developer == developer {
			return c.TotalCostMicro
		}
	}
	t.Fatalf("no cost row for developer %q (rows: %+v)", developer, costs)
	return 0
}

// TestPostCosts_IdenticalKeyedRepost_StaysIdempotent confirms the divergence
// guard does NOT touch the honest-retry path: an identical re-post (same key,
// same cost) still returns 201 and produces exactly one row — no spurious 409.
func TestPostCosts_IdenticalKeyedRepost_StaysIdempotent(t *testing.T) {
	h, db := newTestHandler(t)
	payload := map[string]any{
		"developer":       "alice",
		"issue_id":        "issue-42",
		"model":           "claude-sonnet-4",
		"cost_usd":        0.0105,
		"input_tokens":    1000,
		"output_tokens":   500,
		"idempotency_key": "identical-retry-001",
	}
	for i := 0; i < 2; i++ {
		code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload)
		if code != http.StatusCreated {
			t.Fatalf("POST #%d: status = %d, want 201 (identical re-post must stay idempotent, not 409); body = %s", i, code, body)
		}
	}
	if got := storedCostMicro(t, db, "alice"); got != 10_500 {
		t.Errorf("stored cost_micro = %d, want 10_500 (identical re-post must not double-count)", got)
	}
}

// TestPostCosts_FloatJitterSameMicro_NoConflict guards the critical property:
// divergence is judged on the stored INTEGER cost_micro, not a reconstructed
// float. Two cost_usd values that differ in the float but round to the SAME
// cost_micro (honest FX/rounding jitter) must NOT 409.
func TestPostCosts_FloatJitterSameMicro_NoConflict(t *testing.T) {
	h, db := newTestHandler(t)
	const key = "jitter-retry-001"

	// Both round to 10_500 micro: 0.0105 * 1e6 = 10500; 0.01050004 * 1e6 =
	// 10500.04 -> RoundToEven -> 10500. Same stored micros, so no conflict.
	if store.DollarsToMicro(0.0105) != store.DollarsToMicro(0.01050004) {
		t.Fatalf("test premise broken: 0.0105 and 0.01050004 must map to the same cost_micro, got %d and %d",
			store.DollarsToMicro(0.0105), store.DollarsToMicro(0.01050004))
	}

	first := map[string]any{
		"developer":       "alice",
		"issue_id":        "issue-42",
		"model":           "claude-sonnet-4",
		"cost_usd":        0.0105,
		"input_tokens":    1000,
		"output_tokens":   500,
		"idempotency_key": key,
	}
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", first); code != http.StatusCreated {
		t.Fatalf("first POST: status = %d, want 201; body = %s", code, body)
	}

	jitter := map[string]any{
		"developer":       "alice",
		"issue_id":        "issue-42",
		"model":           "claude-sonnet-4",
		"cost_usd":        0.01050004, // float-differs, same micros
		"input_tokens":    1000,
		"output_tokens":   500,
		"idempotency_key": key,
	}
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", jitter); code != http.StatusCreated {
		t.Fatalf("float-jitter re-post: status = %d, want 201 (same cost_micro is idempotent, not a 409); body = %s", code, body)
	}
	if got := storedCostMicro(t, db, "alice"); got != 10_500 {
		t.Errorf("stored cost_micro = %d, want 10_500", got)
	}
}

// TestPostCosts_OffByOneMicro_Returns409 is the boundary companion to the
// float-jitter test: the guard is a strict integer inequality on cost_micro, so
// a re-post one micro apart ($0.010500 vs $0.010501) must 409. This rules out
// any epsilon/tolerance creeping into the comparison — same key, smallest
// possible divergence, still a conflict.
func TestPostCosts_OffByOneMicro_Returns409(t *testing.T) {
	h, db := newTestHandler(t)
	const key = "off-by-one-001"

	// Pin the premise: the two costs differ by exactly one micro.
	if store.DollarsToMicro(0.010501)-store.DollarsToMicro(0.010500) != 1 {
		t.Fatalf("test premise broken: want a 1-micro gap, got %d vs %d",
			store.DollarsToMicro(0.010500), store.DollarsToMicro(0.010501))
	}

	first := map[string]any{
		"developer":       "alice",
		"issue_id":        "issue-42",
		"model":           "claude-sonnet-4",
		"cost_usd":        0.010500,
		"input_tokens":    1000,
		"output_tokens":   500,
		"idempotency_key": key,
	}
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", first); code != http.StatusCreated {
		t.Fatalf("first POST: status = %d, want 201; body = %s", code, body)
	}

	off := map[string]any{
		"developer":       "alice",
		"issue_id":        "issue-42",
		"model":           "claude-sonnet-4",
		"cost_usd":        0.010501, // one micro higher
		"input_tokens":    1000,
		"output_tokens":   500,
		"idempotency_key": key,
	}
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", off); code != http.StatusConflict {
		t.Fatalf("one-micro-divergent re-post: status = %d, want 409; body = %s", code, body)
	}
	if got := storedCostMicro(t, db, "alice"); got != 10_500 {
		t.Errorf("stored cost_micro = %d, want 10_500 (409 must not overwrite)", got)
	}
}

// TestPostCosts_NewKey_Returns201 confirms a brand-new idempotency_key is a
// plain first-writer insert — 201, no conflict path.
func TestPostCosts_NewKey_Returns201(t *testing.T) {
	h, db := newTestHandler(t)
	payload := map[string]any{
		"developer":       "alice",
		"issue_id":        "issue-42",
		"model":           "claude-sonnet-4",
		"cost_usd":        0.0105,
		"input_tokens":    1000,
		"output_tokens":   500,
		"idempotency_key": "brand-new-key-001",
	}
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload); code != http.StatusCreated {
		t.Fatalf("new-key POST: status = %d, want 201; body = %s", code, body)
	}
	if got := storedCostMicro(t, db, "alice"); got != 10_500 {
		t.Errorf("stored cost_micro = %d, want 10_500", got)
	}
}

// costRows lists every token_events row stamped within an hour of now, with
// UTC bounds (the write path stamps ts in UTC).
func costRows(t *testing.T, db *store.DB) []store.TokenEvent {
	t.Helper()
	now := time.Now().UTC()
	events, _, err := db.ListTokenEvents(context.Background(), now.Add(-time.Hour), now.Add(time.Hour), store.PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	return events
}

// TestPostCosts_KeyOfAnotherIdentity_Returns409 is #871's measured scenario
// end to end: bob's captured jsonl row owns msg-abc at $0.001 with input_tok 10,
// and alice POSTs /costs under msg-abc at the same $0.001 with 9,000,000 input
// tokens. Before #871 the answer was 201, bob's input_tok became 9,000,000 and
// alice had no row. It must be the identity-mismatch 409, the same body the
// override path returns, with bob's row untouched and nothing written for alice.
func TestPostCosts_KeyOfAnotherIdentity_Returns409(t *testing.T) {
	h, db := newTestHandler(t)
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: "bob", IssueID: "issue-7", Model: "claude-sonnet-4",
		InputTok: 10, OutputTok: 5, CostMicro: 1_000,
		Source: "jsonl", Fidelity: "realtime", IdempotencyKey: "msg-abc",
		Timestamp: time.Now().UTC().Truncate(time.Second),
	}); err != nil {
		t.Fatalf("seed bob: %v", err)
	}

	code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", map[string]any{
		"developer": "alice", "issue_id": "issue-42", "model": "claude-sonnet-4",
		"cost_usd": 0.001, "input_tokens": 9_000_000, "idempotency_key": "msg-abc",
	})
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", code, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("409 body is not a JSON object: %v (body = %s)", err, body)
	}
	if errResp["error"] != costIdentityMismatchMsg {
		t.Errorf("409 error = %q, want the identity-mismatch body %q", errResp["error"], costIdentityMismatchMsg)
	}

	rows := costRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want only bob's (alice must have none)", rows)
	}
	if got := rows[0]; got.Developer != "bob" || got.InputTok != 10 || got.OutputTok != 5 || got.CostMicro != 1_000 {
		t.Errorf("bob's row = %+v, want it unchanged (input 10, output 5, cost 1000)", got)
	}
}

// TestPostCosts_SameIdentityRepost_LeavesCountsUnchanged pins that an identical
// keyed re-post (same identity and cost) carrying different token counts is a
// 201 that changes nothing: docs/api-compatibility.md calls non-cost fields a
// silent no-op, immutable on conflict (#871; before it the counts MAX-merged).
func TestPostCosts_SameIdentityRepost_LeavesCountsUnchanged(t *testing.T) {
	h, db := newTestHandler(t)
	post := map[string]any{
		"developer": "alice", "issue_id": "issue-42", "model": "claude-sonnet-4",
		"cost_usd": 1.5, "input_tokens": 100, "output_tokens": 50, "idempotency_key": "k-871",
	}
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", post); code != http.StatusCreated {
		t.Fatalf("first POST: status = %d, want 201; body = %s", code, body)
	}
	post["input_tokens"], post["output_tokens"] = 9_000_000, 9_000
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", post); code != http.StatusCreated {
		t.Fatalf("re-post: status = %d, want 201 (idempotent); body = %s", code, body)
	}
	rows := costRows(t, db)
	if len(rows) != 1 || rows[0].InputTok != 100 || rows[0].OutputTok != 50 {
		t.Errorf("rows = %+v, want one row with input 100, output 50 unchanged", rows)
	}
}
