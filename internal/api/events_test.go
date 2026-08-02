package api

// Tests for POST /api/v1/events (#126) — the batched collector-event ingest
// endpoint the laptop shipper posts to. The provenance rules here are the
// inverse of /costs: source must be a LOCAL-COLLECTOR source
// (collector.ShippableSource — "jsonl" or "codex-rollout"), fidelity MUST be
// "realtime", and idempotency_key is mandatory (replay safety is the whole
// contract). The /costs guards (#34/#82) have their own tests in
// handler_test.go and are deliberately untouched by this endpoint.
//
// The source rule was a bare `!= "jsonl"` literal until #492. That silently
// closed the central path to Codex: its outcomes arrived by webhook while its
// cost was 400'd, so Codex work read as FREE and inflated TIER for anyone using
// it. TestPostEvents_SourceAllowlist below now asserts BOTH arms — accepted and
// rejected — for every enumerated source, because the bug was a missing accept,
// and a suite that only tests rejections cannot see one.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// validEvent returns one well-formed /events element. key differentiates
// events within a batch; every other field is a realistic collector value.
func validEvent(key string) map[string]any {
	return map[string]any{
		"developer":       "alice",
		"issue_id":        "issue-42",
		"model":           "claude-sonnet-4",
		"input_tokens":    1000,
		"output_tokens":   500,
		"cost_usd":        0.0105,
		"source":          "jsonl",
		"fidelity":        "realtime",
		"idempotency_key": key,
		"timestamp":       "2026-05-19T10:00:00Z",
	}
}

// postEvents marshals batch and POSTs it to /api/v1/events through the mux.
func postEvents(t *testing.T, h *Handler, batch []map[string]any) (int, []byte) {
	t.Helper()
	return doRequest(t, h, http.MethodPost, "/api/v1/events", batch)
}

// developerCosts fetches the per-developer micro-dollar aggregates straight
// from the store — the integer-money assertion surface for these tests.
func developerCosts(t *testing.T, db *store.DB) map[string]store.DeveloperCost {
	t.Helper()
	costs, err := db.DeveloperCosts(context.Background(), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	out := map[string]store.DeveloperCost{}
	for _, c := range costs {
		out[c.Developer] = c
	}
	return out
}

// TestPostEvents_AuthRequired pins /events behind the same bearer gate (and
// therefore the same shared #36 lockout) as every other write endpoint.
func TestPostEvents_AuthRequired(t *testing.T) {
	const token = "s3cret-test-token-9f3a"
	h, _ := newTestHandlerWithToken(t, token)
	batch := []map[string]any{validEvent("k-auth-1")}

	code, _ := postEvents(t, h, batch)
	if code != http.StatusUnauthorized {
		t.Errorf("no header: status = %d, want 401", code)
	}

	header := http.Header{"Authorization": []string{"Bearer " + token}}
	code, body := doRequestWithHeader(t, h, http.MethodPost, "/api/v1/events", batch, header)
	if code != http.StatusCreated {
		t.Errorf("with token: status = %d, want 201; body = %s", code, body)
	}
}

// TestPostEvents_RequiresIdempotencyKey is the replay-safety contract:
// keyless events are rejected with 400 (naming the offending index), and the
// whole batch is discarded — nothing inserts.
func TestPostEvents_RequiresIdempotencyKey(t *testing.T) {
	h, db := newTestHandler(t)

	cases := []struct {
		name  string
		batch []map[string]any
	}{
		{"missing key field", func() []map[string]any {
			e := validEvent("")
			delete(e, "idempotency_key")
			return []map[string]any{validEvent("k-ok-1"), e}
		}()},
		{"empty key", []map[string]any{validEvent("k-ok-2"), validEvent("")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := postEvents(t, h, tc.batch)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", code, body)
			}
			if !strings.Contains(string(body), "idempotency_key") {
				t.Errorf("error should name idempotency_key; body = %s", body)
			}
			if !strings.Contains(string(body), "events[1]") {
				t.Errorf("error should name the offending index events[1]; body = %s", body)
			}
			if costs := developerCosts(t, db); len(costs) != 0 {
				t.Errorf("batch with a keyless event must insert nothing; got %v", costs)
			}
		})
	}
}

// TestPostEvents_RejectsSourceProxyAndApi pins the reject arm of the
// provenance allowlist: "proxy" events are born server-side, "api" belongs to
// /costs, and the org pollers emit daily aggregates; anything unrecognised
// (including omitted) is a 400. The companion fidelity allowlist admits
// exactly "realtime". The accept arm lives in TestPostEvents_SourceAllowlist.
func TestPostEvents_RejectsSourceProxyAndApi(t *testing.T) {
	h, db := newTestHandler(t)

	cases := []struct {
		name     string
		mutate   func(e map[string]any)
		wantWord string
	}{
		{"source proxy", func(e map[string]any) { e["source"] = "proxy" }, "source"},
		{"source api", func(e map[string]any) { e["source"] = "api" }, "source"},
		{"source omitted", func(e map[string]any) { delete(e, "source") }, "source"},
		{"source copilot-api", func(e map[string]any) { e["source"] = "copilot-api" }, "source"},
		{"source case-variant Jsonl", func(e map[string]any) { e["source"] = "Jsonl" }, "source"},
		// Case-variant of the source #492 ADDED, not just the original one:
		// the widened check must not have gone case-insensitive on the way.
		{"source case-variant Codex-Rollout", func(e map[string]any) { e["source"] = "Codex-Rollout" }, "source"},
		{"fidelity daily", func(e map[string]any) { e["fidelity"] = "daily" }, "fidelity"},
		{"fidelity estimated", func(e map[string]any) { e["fidelity"] = "estimated" }, "fidelity"},
		{"fidelity omitted", func(e map[string]any) { delete(e, "fidelity") }, "fidelity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := validEvent("k-src-" + tc.name)
			tc.mutate(e)
			code, body := postEvents(t, h, []map[string]any{e})
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", code, body)
			}
			if !strings.Contains(string(body), tc.wantWord) {
				t.Errorf("error should mention %q; body = %s", tc.wantWord, body)
			}
		})
	}
	if costs := developerCosts(t, db); len(costs) != 0 {
		t.Errorf("no rejected event may insert; got %v", costs)
	}
}

// TestPostEvents_SourceAllowlist walks EVERY enumerated collector source and
// asserts the endpoint's answer matches collector.ShippableSource — accepted
// sources reach the store, rejected ones 400 with the source named.
//
// It iterates collector.AllSources() rather than a list copied into this file, so
// adding a seventh collector fails HERE too, not only in the collector package.
// The bug this closes (#492) was a missing ACCEPT, which is invisible to a
// suite that only ever asserts rejections.
func TestPostEvents_SourceAllowlist(t *testing.T) {
	// Control arm: a "check every source" loop over an empty list passes
	// vacuously. Prove the set is real and contains both answers before
	// trusting anything below.
	all := collector.AllSources()
	if len(all) < 2 {
		t.Fatalf("collector.AllSources() returned %d entries — this loop would prove nothing", len(all))
	}
	var accepted, rejected int
	for _, source := range all {
		if collector.ShippableSource(source) {
			accepted++
		} else {
			rejected++
		}
	}
	if accepted == 0 || rejected == 0 {
		t.Fatalf("AllSources yields %d accepted / %d rejected — one arm of this test would never run", accepted, rejected)
	}

	// LITERALS, not the predicate. The loop below asks collector.ShippableSource
	// what to expect and then checks the handler agrees — which verifies the
	// handler agrees with itself and nothing more. If someone narrowed
	// ShippableSource back to jsonl-only (#492 again, expressed through the new
	// single source of truth instead of the old bare literal), every derived
	// assertion would flip along with it and stay green. These two names are the
	// contract, written down independently of the code implementing it.
	for _, source := range []string{"jsonl", "codex-rollout"} {
		t.Run("literal/"+source, func(t *testing.T) {
			h, db := newTestHandler(t)
			e := validEvent("k-literal-" + source)
			e["source"] = source
			code, body := postEvents(t, h, []map[string]any{e})
			if code != http.StatusCreated {
				t.Fatalf("source %q MUST be accepted on /api/v1/events; got status %d; body = %s", source, code, body)
			}
			if costs := developerCosts(t, db); len(costs) != 1 {
				t.Errorf("source %q returned 201 but no cost row landed; costs = %v", source, costs)
			}
		})
	}

	for _, source := range all {
		t.Run(source, func(t *testing.T) {
			h, db := newTestHandler(t)
			e := validEvent("k-allow-" + source)
			e["source"] = source

			code, body := postEvents(t, h, []map[string]any{e})
			costs := developerCosts(t, db)

			if collector.ShippableSource(source) {
				if code != http.StatusCreated {
					t.Fatalf("source %q is shippable but got status %d; body = %s", source, code, body)
				}
				// Landing in the store is the assertion, not the 201: #492 was
				// exactly a case where the outcome side worked and the cost
				// side did not arrive.
				if len(costs) != 1 {
					t.Errorf("source %q returned 201 but no cost row landed; costs = %v", source, costs)
				}
				return
			}
			if code != http.StatusBadRequest {
				t.Fatalf("source %q is not shippable but got status %d; body = %s", source, code, body)
			}
			if !strings.Contains(string(body), source) {
				t.Errorf("rejection should name the offending source %q; body = %s", source, body)
			}
			if len(costs) != 0 {
				t.Errorf("rejected source %q must insert nothing; got %v", source, costs)
			}
		})
	}
}

// TestPostEvents_PreservesBillingMode: the re-pricer must STORE the billing
// basis it resolves, not discard it.
//
// codexrollout is the one collector that derives a real billing_mode, and #492
// is what first lets its events onto this endpoint. The re-pricer called
// store.ComputeCost, which throws the mode away, so the row fell to
// normalizeBillingMode's per_token default — and the SAME Codex session
// recorded self_hosted_amortized under `serve` and per_token under `ship`.
// billing_mode is published by both /export surfaces, so the divergence was
// visible to consumers while being invisible here.
//
// `self-hosted-large` is a real self-hosted-provider row in the embedded table;
// its resolved mode is self_hosted_amortized, so a re-pricer that discards the
// mode reads back as per_token and fails this test.
func TestPostEvents_PreservesBillingMode(t *testing.T) {
	h, db := newTestHandler(t)

	e := validEvent("k-billing-mode")
	e["model"] = "self-hosted-large"
	e["source"] = collector.SourceCodexRollout
	e["cost_usd"] = 0.003 // 1000 in + 500 out, combined @ $2.00/M
	if code, body := postEvents(t, h, []map[string]any{e}); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", code, body)
	}

	rows, _, err := db.ListTokenEvents(context.Background(),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), store.PageCursor{}, 10)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 stored row, got %d", len(rows))
	}
	// CONTROL ARM, and the exact cost is the whole point of it. The unpriced-model
	// fallback resolves to the `self-hosted-medium` row, which ALSO reports
	// self_hosted_amortized — so a test naming that model cannot tell "resolved
	// through the real table entry" from "fell to the unaudited guess", which is
	// the very distinction billing_mode exists to carry. `self-hosted-large`
	// prices at $2.00/M combined = 3000 µ$, where the fallback would price the
	// same tokens at 750 µ$, so this assertion fires exactly when the lookup
	// silently degraded.
	if rows[0].CostMicro != 3000 {
		t.Fatalf("row priced at %d µ$, want 3000 — %q did not resolve to its real table entry, so the billing_mode below would be the unpriced fallback's rather than the table's", rows[0].CostMicro, "self-hosted-large")
	}
	if got := rows[0].BillingMode; got != store.BillingSelfHostedAmortized {
		t.Errorf("stored billing_mode = %q, want %q. The re-pricer discarded the mode it resolved, so this row claims a per-token basis it does not have (#492/#300)", got, store.BillingSelfHostedAmortized)
	}
}

// TestPostEvents_BatchAllOrNothing: one invalid element anywhere in the batch
// fails the WHOLE batch with 400 naming its index — valid siblings before and
// after it must not land (single-transaction bulk insert).
func TestPostEvents_BatchAllOrNothing(t *testing.T) {
	h, db := newTestHandler(t)

	bad := validEvent("k-bad")
	bad["input_tokens"] = -5
	batch := []map[string]any{validEvent("k-a"), bad, validEvent("k-b")}

	code, body := postEvents(t, h, batch)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", code, body)
	}
	if !strings.Contains(string(body), "events[1]") {
		t.Errorf("error should name events[1]; body = %s", body)
	}
	if costs := developerCosts(t, db); len(costs) != 0 {
		t.Errorf("all-or-nothing violated: valid siblings inserted; got %v", costs)
	}
}

// TestPostEvents_ReplayIsNoOp is the over-shipping guarantee the stateless
// laptop shipper depends on: POSTing the identical batch twice leaves the
// per-developer micro-dollar SUMs (total and realtime) exactly unchanged —
// the MAX-on-conflict UPSERT absorbs the replay instead of duplicating rows.
// A failed dedup would double the SUM, so the integer equality is the
// duplicate-row detector.
func TestPostEvents_ReplayIsNoOp(t *testing.T) {
	h, db := newTestHandler(t)

	e1 := validEvent("k-replay-1") // alice: sonnet 1000in/500out -> server price $0.0105
	e2 := validEvent("k-replay-2")
	e2["developer"] = "bob"
	// bob gets DISTINCT token counts so his server price differs from alice's; the
	// SERVER prices from these counts (#233), so cost_usd is set to the matching
	// value only to avoid a spurious cross-check divergence. sonnet 1000in/1500out
	// = 1000*$3/M + 1500*$15/M = $0.0255.
	e2["output_tokens"] = 1500
	e2["cost_usd"] = 0.0255
	batch := []map[string]any{e1, e2}

	code, body := postEvents(t, h, batch)
	if code != http.StatusCreated {
		t.Fatalf("first post: status = %d, want 201; body = %s", code, body)
	}
	var resp struct {
		Accepted int `json:"accepted"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response %q: %v", body, err)
	}
	if resp.Accepted != 2 {
		t.Errorf("accepted = %d, want 2", resp.Accepted)
	}

	want := map[string]store.DeveloperCost{
		"alice": {Developer: "alice", TotalCostMicro: store.DollarsToMicro(0.0105), RealtimeCostMicro: store.DollarsToMicro(0.0105)},
		"bob":   {Developer: "bob", TotalCostMicro: store.DollarsToMicro(0.0255), RealtimeCostMicro: store.DollarsToMicro(0.0255)},
	}
	first := developerCosts(t, db)
	if len(first) != len(want) {
		t.Fatalf("after first post: %d developers, want %d: %v", len(first), len(want), first)
	}
	for dev, w := range want {
		if first[dev] != w {
			t.Errorf("after first post: %s = %+v, want %+v", dev, first[dev], w)
		}
	}

	// Replay the identical batch — a stateless shipper re-running its scan.
	code, body = postEvents(t, h, batch)
	if code != http.StatusCreated {
		t.Fatalf("replay post: status = %d, want 201; body = %s", code, body)
	}
	second := developerCosts(t, db)
	if len(second) != len(want) {
		t.Fatalf("after replay: %d developers, want %d: %v", len(second), len(want), second)
	}
	for dev, w := range want {
		if second[dev] != w {
			t.Errorf("after replay: %s = %+v, want %+v (replay must be a no-op)", dev, second[dev], w)
		}
	}
}

// TestPostEvents_BatchLimits covers the DoS caps: more than 500 events is a
// 400, an empty array is a 400 (fail-loud — a shipper never posts nothing),
// and a non-array body is a 400.
func TestPostEvents_BatchLimits(t *testing.T) {
	h, db := newTestHandler(t)

	t.Run("501 events rejected", func(t *testing.T) {
		batch := make([]map[string]any, 0, 501)
		for i := 0; i < 501; i++ {
			batch = append(batch, validEvent(fmt.Sprintf("k-cap-%d", i)))
		}
		code, body := postEvents(t, h, batch)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", code, body)
		}
		if !strings.Contains(string(body), "500") {
			t.Errorf("error should state the 500-event cap; body = %s", body)
		}
	})

	t.Run("empty array rejected", func(t *testing.T) {
		code, body := postEvents(t, h, []map[string]any{})
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", code, body)
		}
	})

	t.Run("single object body rejected", func(t *testing.T) {
		code, body := doRequest(t, h, http.MethodPost, "/api/v1/events", validEvent("k-obj"))
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", code, body)
		}
	})

	t.Run("oversize body reports size not invalid JSON", func(t *testing.T) {
		// A valid-JSON array larger than the 1 MiB MaxBytesReader must 400
		// with a size message, not the misleading "invalid JSON" prefix.
		e := validEvent("k-big")
		e["developer"] = strings.Repeat("a", 2<<20) // 2 MiB single field
		buf, err := json.Marshal([]map[string]any{e})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/events", bytes.NewReader(buf))
		rec := httptest.NewRecorder()
		mux := http.NewServeMux()
		h.Register(mux)
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.Bytes())
		}
		if !strings.Contains(rec.Body.String(), "body exceeds") {
			t.Errorf("error should report the body-size cap; body = %s", rec.Body.String())
		}
	})

	t.Run("trailing JSON rejected", func(t *testing.T) {
		buf, err := json.Marshal([]map[string]any{validEvent("k-trail")})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		buf = append(buf, []byte(`[]`)...)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/events", bytes.NewReader(buf))
		rec := httptest.NewRecorder()
		mux := http.NewServeMux()
		h.Register(mux)
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.Bytes())
		}
	})

	if costs := developerCosts(t, db); len(costs) != 0 {
		t.Errorf("no rejected batch may insert; got %v", costs)
	}
}

// TestPostEvents_Validation is the element-level validation table (mirrors
// the /costs bounds: required identifiers, 256-char caps, finite non-negative
// money, plus the /events-only required RFC3339 timestamp).
func TestPostEvents_Validation(t *testing.T) {
	h, db := newTestHandler(t)

	long := strings.Repeat("x", 257)
	cases := []struct {
		name     string
		mutate   func(e map[string]any)
		wantWord string
	}{
		{"missing developer", func(e map[string]any) { e["developer"] = "" }, "developer"},
		{"missing issue_id", func(e map[string]any) { e["issue_id"] = "" }, "issue_id"},
		{"missing model", func(e map[string]any) { e["model"] = "" }, "model"},
		{"developer too long", func(e map[string]any) { e["developer"] = long }, "256"},
		{"idempotency_key too long", func(e map[string]any) { e["idempotency_key"] = long }, "256"},
		// json.Encoder HTML-escapes the greater-than sign (to a
		// unicode escape) in the body, so match on the message prefix
		// rather than the ">= 0" suffix.
		{"negative cost", func(e map[string]any) { e["cost_usd"] = -0.01 }, "token counts and cost_usd"},
		{"negative output tokens", func(e map[string]any) { e["output_tokens"] = -1 }, "token counts and cost_usd"},
		{"negative cache read", func(e map[string]any) { e["cache_read_tokens"] = -1 }, "token counts and cost_usd"},
		{"missing timestamp", func(e map[string]any) { delete(e, "timestamp") }, "timestamp"},
		{"empty timestamp", func(e map[string]any) { e["timestamp"] = "" }, "timestamp"},
		{"non-RFC3339 timestamp", func(e map[string]any) { e["timestamp"] = "2026-05-19 10:00:00" }, "timestamp"},
		{"far-future timestamp", func(e map[string]any) { e["timestamp"] = "9999-01-01T00:00:00Z" }, "future"},
		{"far-past timestamp", func(e map[string]any) { e["timestamp"] = "1999-01-01T00:00:00Z" }, "earliest"},
		// The deprecated /costs field never existed on /events — new
		// contract, new clients only. DisallowUnknownFields rejects it.
		{"legacy cache_write_tokens", func(e map[string]any) { e["cache_write_tokens"] = 10 }, "cache_write_tokens"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := validEvent("k-val-" + tc.name)
			tc.mutate(e)
			code, body := postEvents(t, h, []map[string]any{e})
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", code, body)
			}
			if !strings.Contains(string(body), tc.wantWord) {
				t.Errorf("error should mention %q; body = %s", tc.wantWord, body)
			}
		})
	}
	if costs := developerCosts(t, db); len(costs) != 0 {
		t.Errorf("no rejected event may insert; got %v", costs)
	}
}

// TestPostEvents_RejectsFarFutureTimestamp is the #235 clock-skew guard: an
// event dated beyond now+maxEventClockSkew is a 400 (naming its batch index)
// and inserts nothing, so one skewed laptop clock can no longer pin a
// far-future ts that sits in every future ?since window. A timestamp within
// the tolerance (a small forward skew) is still accepted — the bound is a
// skew allowance, not a "must be in the past" rule.
func TestPostEvents_RejectsFarFutureTimestamp(t *testing.T) {
	t.Run("beyond tolerance rejected", func(t *testing.T) {
		h, db := newTestHandler(t)
		e := validEvent("k-future-reject")
		e["timestamp"] = time.Now().UTC().Add(maxEventClockSkew + time.Hour).Format(time.RFC3339)
		code, body := postEvents(t, h, []map[string]any{validEvent("k-future-ok-sibling"), e})
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", code, body)
		}
		if !strings.Contains(string(body), "future") {
			t.Errorf("error should flag the far-future timestamp; body = %s", body)
		}
		if !strings.Contains(string(body), "events[1]") {
			t.Errorf("error should name the offending index events[1]; body = %s", body)
		}
		if costs := developerCosts(t, db); len(costs) != 0 {
			t.Errorf("a batch with a far-future event must insert nothing; got %v", costs)
		}
	})

	t.Run("within tolerance accepted", func(t *testing.T) {
		h, _ := newTestHandler(t)
		e := validEvent("k-future-ok")
		// A modest forward skew (well inside the tolerance) must still land —
		// laptop clocks run a little fast; only gross skew is rejected.
		e["timestamp"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
		code, body := postEvents(t, h, []map[string]any{e})
		if code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", code, body)
		}
	})
}

// TestPostEvents_TimestampFirstWriterWins pins the #235 conflict-semantics
// change: ts is INSERT-only, so replaying an event under the same
// idempotency_key with a LATER timestamp must NOT advance the stored ts. The
// old MAX(ts, excluded.ts) let a corrected re-ship pull a row forward forever;
// dropping it makes capture time an immutable first-writer fact.
func TestPostEvents_TimestampFirstWriterWins(t *testing.T) {
	h, db := newTestHandler(t)

	const key = "k-fww-1"
	first := validEvent(key)
	first["timestamp"] = "2026-05-19T10:00:00Z" // T1

	if code, body := postEvents(t, h, []map[string]any{first}); code != http.StatusCreated {
		t.Fatalf("first post: status = %d, want 201; body = %s", code, body)
	}

	// Replay the same key with a strictly later timestamp.
	later := validEvent(key)
	later["timestamp"] = "2026-06-19T10:00:00Z" // T2 > T1
	if code, body := postEvents(t, h, []map[string]any{later}); code != http.StatusCreated {
		t.Fatalf("replay post: status = %d, want 201; body = %s", code, body)
	}

	// A ?since window that opens AFTER T1 but BEFORE T2 must exclude the row:
	// the stored ts stayed at T1. If ts had been MAX-advanced to T2 the row
	// would reappear here, inflating every future window — exactly the bug.
	ctx := context.Background()
	between, err := db.DeveloperCosts(ctx, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("DeveloperCosts(between): %v", err)
	}
	if len(between) != 0 {
		t.Errorf("ts advanced on conflict — row visible in a post-T1 window: %v", between)
	}

	// Sanity: the row is still there when the window opens before T1.
	all, err := db.DeveloperCosts(ctx, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("DeveloperCosts(all): %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected exactly one row after replay, got %v", all)
	}
}

// TestPostEvents_RejectsCostAboveMagnitudeCap pins the #118 magnitude reject
// on /events, the fourth money-write endpoint: a cost_usd exactly at
// store.MaxCostUSD (1e12) is accepted, the first representable value above it
// is a 400 that never inserts. Without this, /events would be the only
// money-write path lacking the trust-boundary cap the other three carry.
func TestPostEvents_RejectsCostAboveMagnitudeCap(t *testing.T) {
	cases := []struct {
		name     string
		cost     float64
		wantCode int
	}{
		{"exactly at cap accepted", store.MaxCostUSD, http.StatusCreated},
		{"just above cap rejected", math.Nextafter(store.MaxCostUSD, math.Inf(1)), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, db := newTestHandler(t)
			e := validEvent("k-cap-" + tc.name)
			e["cost_usd"] = tc.cost
			code, body := postEvents(t, h, []map[string]any{e})
			if code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body = %s", code, tc.wantCode, body)
			}
			if tc.wantCode == http.StatusBadRequest {
				if !strings.Contains(string(body), "1e12") {
					t.Errorf("error should state the 1e12 cap; body = %s", body)
				}
				if costs := developerCosts(t, db); len(costs) != 0 {
					t.Errorf("over-cap event must insert nothing; got %v", costs)
				}
			}
		})
	}
}

// TestPostEvents_StoresShippedTimestamp verifies the event lands with its
// original JSONL capture time, not the server arrival time — shipped history
// must stay attributed to when the tokens were actually spent.
func TestPostEvents_StoresShippedTimestamp(t *testing.T) {
	h, db := newTestHandler(t)

	e := validEvent("k-ts-1")
	e["timestamp"] = "2026-05-19T10:00:00Z"
	code, body := postEvents(t, h, []map[string]any{e})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", code, body)
	}

	// DeveloperCosts filters on ts >= since: a since AFTER the shipped
	// timestamp must exclude the row. If the handler had stamped arrival
	// time (now), the row would still be visible and this test would fail.
	costs, err := db.DeveloperCosts(context.Background(), time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("DeveloperCosts: %v", err)
	}
	if len(costs) != 0 {
		t.Errorf("row visible after its shipped timestamp window — arrival time was stored instead: %v", costs)
	}
}
