package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/metrics"
	"github.com/tiermetric/tier/internal/store"
)

func TestPostCosts_TokenCountCap(t *testing.T) {
	if strconv.IntSize == 32 {
		t.Skip("MaxTokenCount (1e12) exceeds the 32-bit int limit (2,147,483,647) of costRequest token fields")
	}
	for _, field := range []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_5m_tokens", "cache_write_1h_tokens", "cache_write_tokens"} {
		for _, count := range []int64{store.MaxTokenCount, store.MaxTokenCount + 1} {
			t.Run(fmt.Sprintf("%s/%d", field, count), func(t *testing.T) {
				h, db := newTestHandler(t)
				payload := map[string]any{"developer": "alice", "issue_id": "limit", "model": "claude-sonnet-4", "cost_usd": 1, field: count}
				code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload)
				want := http.StatusCreated
				if count > store.MaxTokenCount {
					want = http.StatusBadRequest
				}
				if code != want {
					t.Fatalf("status = %d, want %d: %s", code, want, body)
				}
				if want == http.StatusBadRequest {
					if !strings.Contains(string(body), "1000000000000") || !strings.Contains(string(body), "split a large import across rows") {
						t.Fatalf("missing cap/remedy: %s", body)
					}
					if len(developerCosts(t, db)) != 0 {
						t.Fatal("rejected request persisted")
					}
				}
			})
		}
	}
}

func TestPostEvents_ResolvedCostCeiling(t *testing.T) {
	if strconv.IntSize == 32 {
		t.Skip("20,000,000,002 tokens exceed the 32-bit int limit (2,147,483,647) of eventRequest.InputTok")
	}
	for _, tokens := range []int64{20_000_000_000, 20_000_000_002} {
		h, db := newTestHandler(t)
		e := validEvent("resolved-limit")
		e["model"], e["input_tokens"], e["output_tokens"] = "self-hosted-medium", tokens, 0
		if code, body := postEvents(t, h, []map[string]any{e}); code != http.StatusCreated {
			t.Fatalf("tokens %d: %d %s", tokens, code, body)
		}
		if got := developerCosts(t, db)["alice"].TotalCostMicro; got != store.MaxTokenEventCostMicro {
			t.Fatalf("stored = %d", got)
		}
		var marked bool
		if err := rawStore(t, db).QueryRow(`SELECT COALESCE(cost_clamped, 0) FROM token_events`).Scan(&marked); err != nil {
			t.Fatal(err)
		}
		if marked != (tokens > 20_000_000_000) {
			t.Fatalf("tokens %d: marked = %v", tokens, marked)
		}
	}
}

// Exercise the real handler with the shipper's wire shape: one costly capture
// must not discard the normal events that share its batch.
func TestPostEvents_ShippedBatchClampsAndContinues(t *testing.T) {
	if strconv.IntSize == 32 {
		t.Skip("20,000,000,002 tokens exceed the 32-bit int limit (2,147,483,647) of eventRequest.InputTok")
	}
	h, db := newTestHandler(t)
	reg := metrics.NewRegistry()
	store.SetCostClampRecorder(reg.NewCounter("tier_token_event_cost_clamps_total", "test"))
	defer store.SetCostClampRecorder(nil)
	big := validEvent("big")
	big["model"], big["input_tokens"], big["output_tokens"], big["cost_usd"] = "self-hosted-medium", int64(20_000_000_002), 0, 10000.000001
	batch := []map[string]any{validEvent("normal-before"), big, validEvent("normal-after")}
	code, body := postEvents(t, h, batch)
	if code != http.StatusCreated || !strings.Contains(string(body), `"accepted":3`) {
		t.Fatalf("shipped batch: %d %s", code, body)
	}
	raw := rawStore(t, db)
	for _, key := range []string{"normal-before", "big", "normal-after"} {
		var cost int64
		var marked bool
		if err := raw.QueryRow(`SELECT cost_micro, COALESCE(cost_clamped, 0) FROM token_events WHERE idempotency_key = ?`, key).Scan(&cost, &marked); err != nil {
			t.Fatal(err)
		}
		want := int64(10_500)
		if key == "big" {
			want = store.MaxTokenEventCostMicro
		}
		if cost != want || marked != (key == "big") {
			t.Fatalf("%s: cost=%d marked=%v", key, cost, marked)
		}
	}
	if got := developerCosts(t, db)["alice"].TotalCostMicro; got != store.MaxTokenEventCostMicro+21_000 {
		t.Fatalf("batch total = %d", got)
	}
	var out strings.Builder
	reg.Render(&out)
	if !strings.Contains(out.String(), "tier_token_event_cost_clamps_total 1\n") {
		t.Fatalf("clamp metric: %s", out.String())
	}
}

func TestPostCosts_LegacyOverCeilingKeyedReplay(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprintf("override=%v", override), func(t *testing.T) {
			h, db := newTestHandler(t)
			raw := rawStore(t, db)
			const cost = store.MaxTokenEventCostMicro + 1
			if _, err := raw.Exec(`INSERT INTO token_events (developer, issue_id, model, input_tok, cost_micro, source, fidelity, idempotency_key) VALUES ('alice', 'limit', 'claude-sonnet-4', 1000, ?, 'api', 'estimated', 'legacy-limit')`, cost); err != nil {
				t.Fatal(err)
			}
			payload := map[string]any{"developer": "alice", "issue_id": "limit", "model": "claude-sonnet-4", "cost_usd": store.MicroToDollars(cost), "input_tokens": 2000, "idempotency_key": "legacy-limit"}
			if override {
				payload["override"], payload["override_actor"], payload["override_reason"] = true, "operator", "retry"
			}
			for range 2 {
				if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload); code != http.StatusCreated {
					t.Fatalf("legacy replay: %d %s", code, body)
				}
			}
			payload["developer"] = "bob"
			if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload); code != http.StatusConflict {
				t.Fatalf("identity collision: %d %s", code, body)
			}
			payload["developer"] = "alice"
			payload["cost_usd"] = store.MicroToDollars(cost + 1)
			if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload); code != http.StatusBadRequest {
				t.Fatalf("divergent over-ceiling cost: %d %s", code, body)
			}
			payload["cost_usd"], payload["idempotency_key"] = store.MicroToDollars(cost), "fresh-limit"
			if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload); code != http.StatusBadRequest {
				t.Fatalf("fresh over-ceiling import: %d %s", code, body)
			}
			delete(payload, "idempotency_key")
			delete(payload, "override")
			delete(payload, "override_actor")
			delete(payload, "override_reason")
			if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload); code != http.StatusBadRequest {
				t.Fatalf("unkeyed import: %d %s", code, body)
			}
			var n, tokens, audits int
			var total int64
			if err := raw.QueryRow(`SELECT COUNT(*), SUM(input_tok), SUM(cost_micro) FROM token_events`).Scan(&n, &tokens, &total); err != nil {
				t.Fatal(err)
			}
			if err := raw.QueryRow(`SELECT COUNT(*) FROM cost_correction_audit`).Scan(&audits); err != nil {
				t.Fatal(err)
			}
			if n != 1 || tokens != 1000 || total != cost || audits != 0 {
				t.Fatalf("legacy row changed: rows=%d tokens=%d cost=%d audits=%d", n, tokens, total, audits)
			}
		})
	}
}

func TestPostCosts_OverrideCostCeiling(t *testing.T) {
	for _, cost := range []float64{10000, 10000.000001} {
		h, db := newTestHandler(t)
		payload := map[string]any{"developer": "alice", "issue_id": "limit", "model": "claude-sonnet-4", "cost_usd": 1, "idempotency_key": "override-limit"}
		if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload); code != http.StatusCreated {
			t.Fatalf("seed: %d %s", code, body)
		}
		payload["cost_usd"], payload["override"], payload["override_actor"], payload["override_reason"] = cost, true, "operator", "large import"
		code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload)
		if cost == 10000 {
			if code != http.StatusOK || developerCosts(t, db)["alice"].TotalCostMicro != store.MaxTokenEventCostMicro {
				t.Fatalf("at ceiling: %d %s", code, body)
			}
		} else {
			if code != http.StatusBadRequest || !strings.Contains(string(body), "10000") || !strings.Contains(string(body), "split") {
				t.Fatalf("over ceiling: %d %s", code, body)
			}
			if got := developerCosts(t, db)["alice"].TotalCostMicro; got != store.MicroPerUSD {
				t.Fatalf("rejected correction changed cost to %d", got)
			}
		}
	}
}

func TestPostCosts_CeilingAcrossWriteBranches(t *testing.T) {
	for _, keyed := range []bool{false, true} {
		for _, override := range []bool{false, true} {
			if override && !keyed {
				continue
			} // The HTTP override requires a key.
			t.Run(fmt.Sprintf("keyed=%v/override=%v", keyed, override), func(t *testing.T) {
				h, db := newTestHandler(t)
				payload := map[string]any{"developer": "alice", "issue_id": "limit", "model": "claude-sonnet-4", "cost_usd": 10000.000001}
				if keyed {
					payload["idempotency_key"] = "fresh-limit"
				}
				if override {
					payload["override"] = true
					payload["override_actor"] = "operator"
					payload["override_reason"] = "import"
				}
				code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload)
				if code != http.StatusBadRequest || !strings.Contains(string(body), "10000") {
					t.Fatalf("status=%d: %s", code, body)
				}
				if len(developerCosts(t, db)) != 0 {
					t.Fatal("over-ceiling request persisted")
				}
			})
		}
	}
}

func TestHTTPNegativeCostsRemainRejected(t *testing.T) {
	h, db := newTestHandler(t)
	e := validEvent("negative")
	e["cost_usd"] = -20000
	if code, body := postEvents(t, h, []map[string]any{e}); code != http.StatusBadRequest {
		t.Fatalf("events: %d %s", code, body)
	}
	payload := map[string]any{"developer": "alice", "issue_id": "limit", "model": "claude-sonnet-4", "cost_usd": -20000}
	if code, body := doRequest(t, h, http.MethodPost, "/api/v1/costs", payload); code != http.StatusBadRequest {
		t.Fatalf("costs: %d %s", code, body)
	}
	if len(developerCosts(t, db)) != 0 {
		t.Fatal("negative HTTP cost persisted")
	}
}
