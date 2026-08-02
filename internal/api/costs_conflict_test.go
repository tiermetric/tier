package api

import (
	"context"
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
