package store

import (
	"context"
	"testing"
	"time"
)

// TestTokenEventHostBillingModeRoundTrip pins the #300 schema round-trip: a
// host-aware producer (the proxy) stores host + billing_mode verbatim, while a
// producer that leaves them unset has them normalized to the HostUnknown sentinel
// + per_token at insert so the NOT NULL columns never store "".
//
// The "unset" arm no longer stands for the JSONL/poller paths — #525 made all
// three store the mode ComputeCostHost resolved. It now stands for the surfaces
// that import a cost they did not derive (/costs, the demo seeder). Note those
// paths still leave HOST unset too, and the JSONL/poller paths deliberately
// continue to: a producer that cannot know the serving host can still know how
// the price table billed the row, so the two fields' provenance has diverged.
func TestTokenEventHostBillingModeRoundTrip(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer: "alice", IssueID: "1", Model: "llama-3.1-70b",
		InputTok: 100, CostMicro: 5, Source: "proxy", Fidelity: "realtime",
		Host: "openrouter.ai", BillingMode: BillingSelfHostedAmortized,
		IdempotencyKey: "k-host", Timestamp: now,
	}); err != nil {
		t.Fatalf("insert host-aware event: %v", err)
	}
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer: "alice", IssueID: "1", Model: "claude-opus-4-8",
		InputTok: 100, CostMicro: 5, Source: "jsonl", Fidelity: "realtime",
		IdempotencyKey: "k-firstparty", Timestamp: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("insert first-party event: %v", err)
	}

	events, _, err := db.ListTokenEvents(ctx, now.Add(-time.Hour), now.Add(time.Hour), PageCursor{}, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byKey := map[string]TokenEvent{}
	for _, e := range events {
		byKey[e.IdempotencyKey] = e
	}
	if got := byKey["k-host"]; got.Host != "openrouter.ai" || got.BillingMode != BillingSelfHostedAmortized {
		t.Errorf("host event round-trip: host=%q billing=%q, want openrouter.ai / %s", got.Host, got.BillingMode, BillingSelfHostedAmortized)
	}
	if got := byKey["k-firstparty"]; got.Host != HostUnknown || got.BillingMode != BillingPerToken {
		t.Errorf("first-party round-trip: host=%q billing=%q, want %s / %s", got.Host, got.BillingMode, HostUnknown, BillingPerToken)
	}
}
