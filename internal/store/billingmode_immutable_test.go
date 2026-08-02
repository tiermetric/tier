package store

import (
	"context"
	"testing"
	"time"
)

// TestTokenEvent_BillingModeImmutableOnConflict pins the property that makes
// #525's fix FORWARD-ONLY, and it is the half of that story with no other
// coverage.
//
// insertTokenEventSQL's ON CONFLICT ... DO UPDATE SET touches ONLY the five
// token-count columns. billing_mode is INSERT-only by design (as are repo,
// session_id, cost_micro, price_version, ts and host), so a replay of an
// already-keyed event CANNOT repair a stored mode. The rationale is that a replay
// must never let a less-qualified producer downgrade a row the collector already
// qualified — but the same property is why every row captured BEFORE #525 keeps
// its per_token default forever, no matter how many times the shipper re-ships its
// window.
//
// That matters operationally: `tier ship` is stateless and re-ships its whole
// window every run, so the obvious assumption is "run it again and history heals".
// It does not. The repair path is `tierd reprice`, which UPDATEs by id and is
// covered by TestReprice_BillingModeReconciled.
//
// Both arms below are deliberate:
//   - the SECOND writer supplies a DIFFERENT, valid mode, so a passing test proves
//     the first value survived a real attempt to change it rather than surviving
//     because nothing tried;
//   - the token counts still promote, proving the row was genuinely UPSERTED and
//     not simply ignored — without that, "billing_mode unchanged" would also be
//     satisfied by the conflict silently dropping the whole replay, which is a
//     different behaviour with different consequences.
func TestTokenEvent_BillingModeImmutableOnConflict(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	ts := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	const key = "msg-billingmode-immutable"

	// First writer stores an honest self-hosted amortized basis — the value a
	// post-#525 JSONL capture of an unpriced model produces.
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer:      "carol",
		IssueID:        "issue-525",
		Model:          "some-unpriced-model",
		InputTok:       1000,
		CostMicro:      10_000,
		Source:         "jsonl",
		Fidelity:       "realtime",
		IdempotencyKey: key,
		BillingMode:    BillingSelfHostedAmortized,
		Timestamp:      ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent (first writer): %v", err)
	}

	// A replay of the SAME key asserts a different basis, with larger counts.
	if err := db.InsertTokenEvent(ctx, TokenEvent{
		Developer:      "carol",
		IssueID:        "issue-525",
		Model:          "some-unpriced-model",
		InputTok:       2000,
		CostMicro:      10_000,
		Source:         "jsonl",
		Fidelity:       "realtime",
		IdempotencyKey: key,
		BillingMode:    BillingPerToken,
		Timestamp:      ts,
	}); err != nil {
		t.Fatalf("InsertTokenEvent (replay): %v", err)
	}

	events, _, err := db.ListTokenEvents(ctx, ts.Add(-time.Hour), ts.Add(time.Hour), PageCursor{}, 100)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (replay must upsert, not insert a second row)", len(events))
	}
	if got := events[0].BillingMode; got != BillingSelfHostedAmortized {
		t.Errorf("BillingMode = %q after a replay asserting %q, want %q immutable.\n"+
			"If this now changes, re-shipping can rewrite a stored billing basis — which "+
			"also means #525's 'forward-only, use tierd reprice' guidance is obsolete and "+
			"the ComputeCost doc comment needs updating with it.",
			got, BillingPerToken, BillingSelfHostedAmortized)
	}
	// Proves the conflict really did UPSERT. Without this, the assertion above
	// would pass just as well if the replay had been dropped entirely.
	if got := events[0].InputTok; got != 2000 {
		t.Errorf("InputTok = %d, want 2000 — the replay must still promote token counts, "+
			"otherwise this test is not exercising the DO UPDATE path at all", got)
	}
}
