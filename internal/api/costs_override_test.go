package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// overridePayload builds a /costs body with the override fields set, so each
// test only has to vary what it's actually testing.
func overridePayload(key string, costUSD float64, override bool, actor, reason string) map[string]any {
	p := map[string]any{
		"developer":       "alice",
		"issue_id":        "issue-42",
		"model":           "claude-sonnet-4",
		"cost_usd":        costUSD,
		"input_tokens":    1000,
		"output_tokens":   500,
		"idempotency_key": key,
	}
	if override {
		p["override"] = true
	}
	if actor != "" {
		p["override_actor"] = actor
	}
	if reason != "" {
		p["override_reason"] = reason
	}
	return p
}

// TestPostCosts_Override_CorrectsDivergentCost is the #346 core contract at
// the HTTP layer: a divergent keyed re-post 409s by DEFAULT (#295, unchanged
// — asserted here as the control arm), and the SAME divergent re-post with
// override=true + actor + reason instead lands as 200 and the stored cost
// actually changes.
func TestPostCosts_Override_CorrectsDivergentCost(t *testing.T) {
	h, db := newTestHandler(t)
	const key = "override-001"

	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", overridePayload(key, 0.0105, false, "", "")); code != http.StatusCreated {
		t.Fatalf("first POST: status = %d, want 201; body = %s", code, body)
	}

	// Control arm: the SAME divergence WITHOUT override must still 409 —
	// #346 must not have weakened #295's default.
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", overridePayload(key, 0.0206, false, "", "")); code != http.StatusConflict {
		t.Fatalf("un-overridden divergent re-post: status = %d, want 409; body = %s", code, body)
	}
	if got := storedCostMicro(t, db, "alice"); got != 10_500 {
		t.Fatalf("stored cost_micro after the 409 = %d, want 10_500 unchanged", got)
	}

	code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs",
		overridePayload(key, 0.0206, true, "finance-alice", "Q3 invoice correction"))
	if code != http.StatusOK {
		t.Fatalf("overridden divergent re-post: status = %d, want 200; body = %s", code, body)
	}
	if got := storedCostMicro(t, db, "alice"); got != 20_600 {
		t.Errorf("stored cost_micro = %d, want 20_600 (the override must actually land)", got)
	}

	// The 200 body echoes what actually changed, so a finance client can
	// confirm the correction without a second read.
	var resp struct {
		Corrected  bool    `json:"corrected"`
		OldCostUSD float64 `json:"old_cost_usd"`
		NewCostUSD float64 `json:"new_cost_usd"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode 200 body: %v; body = %s", err, body)
	}
	if !resp.Corrected || resp.OldCostUSD != 0.0105 || resp.NewCostUSD != 0.0206 {
		t.Errorf("200 body = %+v, want corrected=true old=0.0105 new=0.0206", resp)
	}

	// The append-only ledger must carry EXACTLY one row for this correction —
	// the HTTP-layer proof (not just the store-level tests in
	// internal/store/cost_correction_test.go) that the handler is actually
	// wired to CorrectManualCostEvent and not some other path that happens to
	// change cost_micro without leaving a trail.
	if n, err := db.CostCorrectionAuditCount(context.Background()); err != nil {
		t.Fatalf("CostCorrectionAuditCount: %v", err)
	} else if n != 1 {
		t.Errorf("cost_correction_audit row count = %d, want 1", n)
	}
}

// TestPostCosts_Override_IdentityMismatchRefused is the #346 R1 fix at the
// HTTP layer: idempotency_key is globally unique with no other column in its
// index, so a request that reuses someone else's key under a DIFFERENT
// developer/issue_id/model/source must be refused — never silently correct a
// row this request does not actually identify. Asserted alongside the
// correct-identity positive control so the refusal is provably about
// identity, not a blanket rejection of this key.
func TestPostCosts_Override_IdentityMismatchRefused(t *testing.T) {
	h, db := newTestHandler(t)
	const key = "override-identity-001"

	owner := overridePayload(key, 0.0105, false, "", "")
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", owner); code != http.StatusCreated {
		t.Fatalf("first POST (alice/issue-42): status = %d, want 201; body = %s", code, body)
	}

	mismatched := overridePayload(key, 0.0206, true, "finance-mallory", "trying to correct a row I don't own")
	mismatched["developer"] = "mallory"
	mismatched["issue_id"] = "issue-999"
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", mismatched)
	if code != http.StatusConflict {
		t.Fatalf("identity-mismatched override: status = %d, want 409; body = %s", code, body)
	}
	if got := storedCostMicro(t, db, "alice"); got != 10_500 {
		t.Errorf("stored cost_micro = %d, want 10_500 unchanged (an identity-mismatched override must not mutate)", got)
	}
	if n, err := db.CostCorrectionAuditCount(context.Background()); err != nil {
		t.Fatalf("CostCorrectionAuditCount: %v", err)
	} else if n != 0 {
		t.Errorf("cost_correction_audit row count = %d, want 0", n)
	}

	// Positive control: the CORRECT identity, same divergent cost, succeeds.
	correct := overridePayload(key, 0.0206, true, "finance-alice", "legitimate correction")
	code, body = doRequest(t, h, http.MethodPost, "/api/v1/costs", correct)
	if code != http.StatusOK {
		t.Fatalf("correct-identity override: status = %d, want 200; body = %s", code, body)
	}
	if got := storedCostMicro(t, db, "alice"); got != 20_600 {
		t.Errorf("stored cost_micro = %d, want 20_600", got)
	}
}

// TestPostCosts_Override_RequiresActorAndReason covers the "never silent"
// validation at the HTTP boundary: override=true with a missing actor or
// reason must 400, and must NOT touch the stored row.
func TestPostCosts_Override_RequiresActorAndReason(t *testing.T) {
	h, db := newTestHandler(t)
	const key = "override-002"
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", overridePayload(key, 0.0105, false, "", "")); code != http.StatusCreated {
		t.Fatalf("first POST: status = %d, want 201; body = %s", code, body)
	}

	cases := []struct {
		name, actor, reason string
	}{
		{"missing actor", "", "a reason"},
		{"missing reason", "an actor", ""},
		{"missing both", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", overridePayload(key, 0.0206, true, tc.actor, tc.reason))
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", code, body)
			}
			if got := storedCostMicro(t, db, "alice"); got != 10_500 {
				t.Errorf("stored cost_micro = %d, want 10_500 unchanged (a rejected override must not mutate)", got)
			}
		})
	}
}

// TestPostCosts_Override_RequiresIdempotencyKey: override=true with no
// idempotency_key has nothing to override against and must 400.
func TestPostCosts_Override_RequiresIdempotencyKey(t *testing.T) {
	h, _ := newTestHandler(t)
	payload := overridePayload("", 0.0105, true, "finance-alice", "a reason")
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", code, body)
	}
}

// TestPostCosts_Override_FieldsWithoutFlagRejected: override_actor/
// override_reason set without override=true must 400, not be silently
// dropped — a client that filled in both almost certainly meant to
// authorize an override.
func TestPostCosts_Override_FieldsWithoutFlagRejected(t *testing.T) {
	h, _ := newTestHandler(t)
	payload := overridePayload("override-003", 0.0105, false, "finance-alice", "a reason")
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", code, body)
	}
}

// TestPostCosts_Override_NoExistingRow_Returns201 proves overriding a BRAND
// NEW key is not a "correction" — there is nothing to correct — so it must
// behave exactly like a normal insert (201), not 200.
func TestPostCosts_Override_NoExistingRow_Returns201(t *testing.T) {
	h, db := newTestHandler(t)
	payload := overridePayload("override-004-fresh", 0.0105, true, "finance-alice", "seeding a new figure")
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (a fresh key is a create, not a correction); body = %s", code, body)
	}
	if got := storedCostMicro(t, db, "alice"); got != 10_500 {
		t.Errorf("stored cost_micro = %d, want 10_500", got)
	}
}

// TestPostCosts_Override_MatchingCost_Returns201Idempotent: an override flag
// on a re-post that turns out NOT to diverge is the ordinary idempotent path
// (201), not a correction (200) — nothing to correct.
func TestPostCosts_Override_MatchingCost_Returns201Idempotent(t *testing.T) {
	h, db := newTestHandler(t)
	const key = "override-005"
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", overridePayload(key, 0.0105, false, "", "")); code != http.StatusCreated {
		t.Fatalf("first POST: status = %d, want 201; body = %s", code, body)
	}
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", overridePayload(key, 0.0105, true, "finance-alice", "no-op re-post"))
	if code != http.StatusCreated {
		t.Fatalf("matching-cost overridden re-post: status = %d, want 201 (idempotent, not a correction); body = %s", code, body)
	}
	if got := storedCostMicro(t, db, "alice"); got != 10_500 {
		t.Errorf("stored cost_micro = %d, want 10_500 unchanged", got)
	}
}

// TestPostCosts_Override_LengthCaps covers the storage-DoS caps on the two
// new free-text fields.
func TestPostCosts_Override_LengthCaps(t *testing.T) {
	h, db := newTestHandler(t)
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs",
		overridePayload("override-006", 0.0105, true, "finance-alice", "a reason")); code != http.StatusCreated {
		t.Fatalf("first POST: status = %d, want 201; body = %s", code, body)
	}

	longActor := make([]byte, maxIdentifierLen+1)
	for i := range longActor {
		longActor[i] = 'a'
	}
	code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs",
		overridePayload("override-006", 0.0206, true, string(longActor), "a reason"))
	if code != http.StatusBadRequest {
		t.Fatalf("over-long override_actor: status = %d, want 400; body = %s", code, body)
	}
	if got := storedCostMicro(t, db, "alice"); got != 10_500 {
		t.Errorf("stored cost_micro = %d, want 10_500 unchanged (a rejected override must not mutate)", got)
	}

	longReason := make([]byte, maxOverrideReasonLen+1)
	for i := range longReason {
		longReason[i] = 'a'
	}
	code, body = doRequest(t, h, http.MethodPost, "/api/v1/costs",
		overridePayload("override-006", 0.0206, true, "finance-alice", string(longReason)))
	if code != http.StatusBadRequest {
		t.Fatalf("over-long override_reason: status = %d, want 400; body = %s", code, body)
	}
	if got := storedCostMicro(t, db, "alice"); got != 10_500 {
		t.Errorf("stored cost_micro = %d, want 10_500 unchanged (a rejected override must not mutate)", got)
	}
}
