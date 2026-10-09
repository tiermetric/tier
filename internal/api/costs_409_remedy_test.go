package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// remedyFor returns the sentence of a divergent-cost 409 body that a caller in
// the given case acts on. The body's remedy is conditional because the plain
// /costs path cannot tell whose row the key belongs to (#860): the sentence
// naming "your own" is the owner's, the one naming "not yours" is everyone
// else's. A body with no sentence for the case leaves that caller only the
// override sentence, which is what the caller would then follow.
func remedyFor(t *testing.T, body string, owner bool) string {
	t.Helper()
	marker := "not yours"
	if owner {
		marker = "your own"
	}
	var fallback string
	for _, s := range strings.Split(body, ". ") {
		if strings.Contains(s, marker) {
			return s
		}
		if fallback == "" && strings.Contains(s, "override=true") {
			fallback = s
		}
	}
	if fallback == "" {
		t.Fatalf("409 body names no remedy at all: %q", body)
	}
	return fallback
}

// followRemedy builds the request a caller sends after reading remedy: the
// rejected request, re-keyed unless remedy says SAME idempotency_key, plus every
// costRequest field remedy names — a bool only as `tag=true`, a string by its
// bare tag. A field named by anything other than its real JSON tag is not set,
// so the handler refuses the follow-up.
func followRemedy(rejected map[string]any, remedy, distinctKey string) map[string]any {
	next := make(map[string]any, len(rejected)+3)
	for k, v := range rejected {
		next[k] = v
	}
	if !regexp.MustCompile(`(?i)\bsame\s+idempotency_key\b`).MatchString(remedy) {
		next["idempotency_key"] = distinctKey
	}
	rt := reflect.TypeOf(costRequest{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if _, set := next[tag]; set {
			continue
		}
		switch f.Type.Kind() {
		case reflect.Bool:
			if strings.Contains(remedy, tag+"=true") {
				next[tag] = true
			}
		case reflect.String:
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(tag) + `\b`).MatchString(remedy) {
				next[tag] = "remedy-" + tag
			}
		}
	}
	return next
}

// TestPostCosts_409Remedy_Followed pins what the divergent-cost 409 body's
// advice DOES when a caller follows it literally (#860), for both cases the
// body distinguishes:
//   - owner: the earlier row is the caller's own and it is correcting it. The
//     advice must land the #346 correction: one row under the key, carrying the
//     new cost, one cost_correction_audit row, and the old figure NOT summed in.
//   - not owner: the key collided with someone else's row and the cost is
//     genuinely distinct. The advice must record the new cost as its own row and
//     leave the other developer's row and the audit ledger untouched.
func TestPostCosts_409Remedy_Followed(t *testing.T) {
	const key = "remedy-key"
	alice := map[string]any{
		"developer": "alice", "issue_id": "issue-42", "model": "claude-sonnet-4",
		"cost_usd": 100.0, "input_tokens": 100, "output_tokens": 50, "idempotency_key": key,
	}
	rows := func(t *testing.T, db *store.DB) []store.TokenEvent {
		t.Helper()
		events, _, err := db.ListTokenEvents(context.Background(),
			time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Hour), store.PageCursor{}, 100)
		if err != nil {
			t.Fatalf("ListTokenEvents: %v", err)
		}
		return events
	}
	auditRows := func(t *testing.T, db *store.DB) int {
		t.Helper()
		n, err := db.CostCorrectionAuditCount(context.Background())
		if err != nil {
			t.Fatalf("CostCorrectionAuditCount: %v", err)
		}
		return n
	}
	// collide seeds alice's row, posts req under the same key, and returns the
	// 409's error text.
	collide := func(t *testing.T, h *Handler, req map[string]any) string {
		t.Helper()
		if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", alice); code != http.StatusCreated {
			t.Fatalf("seed POST: status = %d, want 201; body = %s", code, body)
		}
		code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", req)
		if code != http.StatusConflict {
			t.Fatalf("colliding POST: status = %d, want 409; body = %s", code, body)
		}
		var errResp map[string]string
		if err := json.Unmarshal(body, &errResp); err != nil {
			t.Fatalf("409 body is not a JSON object: %v (body = %s)", err, body)
		}
		return errResp["error"]
	}

	t.Run("owner corrects own row", func(t *testing.T) {
		h, db := newTestHandler(t)
		correction := map[string]any{}
		for k, v := range alice {
			correction[k] = v
		}
		correction["cost_usd"] = 80.0
		msg := collide(t, h, correction)

		remedy := remedyFor(t, msg, true)
		// Pins that the owner remedy offers no new-key alternative (#860): a
		// re-post under another key adds a second row that every spend read sums.
		if newKey := regexp.MustCompile(`(?i)\b(new|different|another|other|distinct|fresh)\s+idempotency_key\b`).FindString(remedy); newKey != "" {
			t.Fatalf("owner remedy advises %q, which double-counts the corrected cost; it must name only the SAME idempotency_key: %q", newKey, remedy)
		}
		next := followRemedy(correction, remedy, "remedy-key-2")
		code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", next)
		if code != http.StatusOK {
			t.Fatalf("following the owner remedy %q: status = %d, want 200 (an audited correction); body = %s", next, code, body)
		}
		got := rows(t, db)
		if len(got) != 1 || got[0].IdempotencyKey != key || got[0].CostMicro != 80_000_000 {
			t.Fatalf("rows = %+v, want exactly one row under %q at 80_000_000 micro (corrected in place, not $100 + $80)", got, key)
		}
		if n := auditRows(t, db); n != 1 {
			t.Errorf("cost_correction_audit rows = %d, want 1", n)
		}
	})

	t.Run("non-owner records a distinct cost", func(t *testing.T) {
		h, db := newTestHandler(t)
		bob := map[string]any{}
		for k, v := range alice {
			bob[k] = v
		}
		bob["developer"], bob["issue_id"], bob["cost_usd"] = "bob", "issue-7", 30.0
		msg := collide(t, h, bob)

		next := followRemedy(bob, remedyFor(t, msg, false), "remedy-key-bob")
		code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", next)
		if code != http.StatusCreated {
			t.Fatalf("following the non-owner remedy %q: status = %d, want 201 (a new row for a distinct cost); body = %s", next, code, body)
		}
		if got := costMicroByKey(t, db, key); got != 100_000_000 {
			t.Errorf("alice's row under %q = %d micro, want 100_000_000 untouched", key, got)
		}
		if got := storedCostMicro(t, db, "bob"); got != 30_000_000 {
			t.Errorf("bob's spend = %d micro, want 30_000_000", got)
		}
		if n := len(rows(t, db)); n != 2 {
			t.Errorf("rows = %d, want 2 (two distinct costs)", n)
		}
		if n := auditRows(t, db); n != 0 {
			t.Errorf("cost_correction_audit rows = %d, want 0 (nothing was corrected)", n)
		}
	})
}
