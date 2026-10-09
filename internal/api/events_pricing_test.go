package api

// Tests for the server-side pricing authority on POST /api/v1/events (#233).
// The laptop shipper posts a client-computed cost_usd, but the SERVER is the
// single pricing authority: it re-prices every event from the raw token counts
// with its own table and stores THAT, keeping the client value only as a
// cross-check. Otherwise a mixed-version fleet prices identical usage
// differently per laptop.

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/metrics"
	"github.com/tiermetric/tier/internal/store"
)

// divergenceCount renders reg and returns the tier_pricing_divergence_total value.
func divergenceCount(t *testing.T, reg *metrics.Registry) float64 {
	t.Helper()
	var sb bytes.Buffer
	reg.Render(&sb)
	const want = "tier_pricing_divergence_total"
	for _, line := range strings.Split(sb.String(), "\n") {
		if strings.HasPrefix(line, want+" ") {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, want+" ")), 64)
			if err != nil {
				t.Fatalf("parse counter line %q: %v", line, err)
			}
			return v
		}
	}
	return 0
}

// TestPostEvents_ServerPricesAuthoritatively proves the stored cost is the
// server's ComputeCost of the token counts, NOT the client's posted cost_usd.
// A hostile/stale client sends a wildly wrong cost_usd; the row must land at the
// server price.
func TestPostEvents_ServerPricesAuthoritatively(t *testing.T) {
	h, db := newTestHandler(t)
	ctx := context.Background()

	const model = "claude-sonnet-4"
	const inTok, outTok = 1000, 500
	wantMicro := store.ComputeCost(model, store.CostUsage{Input: inTok, Output: outTok})

	ev := map[string]any{
		"developer":       "alice",
		"issue_id":        "issue-233",
		"model":           model,
		"input_tokens":    inTok,
		"output_tokens":   outTok,
		"cost_usd":        999.99, // deliberately wrong — must be ignored for storage
		"source":          "jsonl",
		"fidelity":        "realtime",
		"idempotency_key": "k-server-price-1",
		"timestamp":       "2026-05-19T10:00:00Z",
	}
	code, body := postEvents(t, h, []map[string]any{ev})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", code, body)
	}

	since := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	events, _, err := db.ListTokenEvents(ctx, since, until, store.PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	got := events[0].CostMicro
	if got != wantMicro {
		t.Errorf("stored CostMicro = %d, want server price %d (not the client cost_usd)", got, wantMicro)
	}
	if clientMicro := store.DollarsToMicro(999.99); got == clientMicro {
		t.Errorf("stored CostMicro = %d equals the client-posted cost; server must be the pricing authority", got)
	}
}

// TestPostEvents_DivergenceCounter proves a client cost_usd that disagrees with the
// server price bumps tier_pricing_divergence_total (the mixed-version-fleet signal),
// while a matching cost_usd does not — and neither changes what the server stores.
func TestPostEvents_DivergenceCounter(t *testing.T) {
	h, _ := newTestHandler(t)
	reg := metrics.NewRegistry()
	c := reg.NewCounter("tier_pricing_divergence_total", "test")
	h.SetPricingDivergenceCounter(c)

	// Matching cost (server price for sonnet 1000/500 = $0.0105) — no divergence.
	match := map[string]any{
		"developer": "alice", "issue_id": "issue-233", "model": "claude-sonnet-4",
		"input_tokens": 1000, "output_tokens": 500, "cost_usd": 0.0105,
		"source": "jsonl", "fidelity": "realtime",
		"idempotency_key": "k-match", "timestamp": "2026-05-19T10:00:00Z",
	}
	if code, body := postEvents(t, h, []map[string]any{match}); code != http.StatusCreated {
		t.Fatalf("match post: status = %d, want 201; body = %s", code, body)
	}
	if got := divergenceCount(t, reg); got != 0 {
		t.Errorf("divergence count after matching cost = %v, want 0", got)
	}

	// Divergent cost — the shipper priced under a different table.
	diverge := map[string]any{
		"developer": "bob", "issue_id": "issue-233", "model": "claude-sonnet-4",
		"input_tokens": 1000, "output_tokens": 500, "cost_usd": 0.5,
		"source": "jsonl", "fidelity": "realtime",
		"idempotency_key": "k-diverge", "timestamp": "2026-05-19T10:00:00Z",
	}
	if code, body := postEvents(t, h, []map[string]any{diverge}); code != http.StatusCreated {
		t.Fatalf("diverge post: status = %d, want 201; body = %s", code, body)
	}
	if got := divergenceCount(t, reg); got != 1 {
		t.Errorf("divergence count after divergent cost = %v, want 1", got)
	}
}
