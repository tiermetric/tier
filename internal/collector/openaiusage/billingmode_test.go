package openaiusage

import (
	"context"
	"testing"
)

// The #525 regression for the OpenAI org poller.
//
// The remainder event was built with store.ComputeCost, which resolves a
// billing_mode and then DISCARDS it, so every org-poller row reached the store
// with an empty BillingMode and was defaulted to "per_token" at insert.
//
// The honest scope here is NARROWER than on the JSONL path, and this test pins
// both halves of it:
//
//  1. Every model that reaches the event builder is an exact price-table hit —
//     an unpriced model is skipped upstream and never ingested at all. So the
//     size-class fallback is unreachable from this producer, and the resolved
//     mode is always per_token today. The stored VALUE is therefore unchanged;
//     what changes is that it is DERIVED from the entry that priced the row
//     rather than defaulted by the store.
//  2. That is worth fixing because prices.yaml can declare an explicit
//     billing_mode, which the old code would have silently overwritten with
//     per_token. (Every explicit mode today sits on a host-qualified row and is
//     set to per_token by #268, so it is unreachable from a host-blind caller;
//     the first mode a bare model key could carry is subscription, for the
//     Ollama Cloud / GLM rows #113 anticipates and #304 gates.)
//
// The first assertion below would have been written as a self_hosted_amortized
// expectation had it not been measured: the poller's own WARN says the remainder
// was NOT ingested. Pinning the skip keeps the unreachability claim in the source
// comments honest, instead of asserting it and hoping.
//
// Expected values are literals rather than reads of the pricing code, so a change
// in what the export publishes fails here instead of agreeing with itself.
func TestPoll_RemainderEventCarriesResolvedBillingMode(t *testing.T) {
	usage := usageReport{Data: []usageBucket{
		dayBucket("2026-06-15",
			// In the shipped price table → ingested, metered per token.
			modelResult("gpt-4o", 1000, 0, 500),
			// Absent from the table → skipped entirely, never ingested.
			modelResult("totally-unknown-model-xyz", 2000, 0, 100)),
	}}
	srv := usageServer(t, usage)
	defer srv.Close()

	poller := newTestPoller(newTestClient(t, srv, nil), newFakeStore(), nil)
	ing := &recordingIngester{}
	if err := poller.pollOnce(context.Background(), ing); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	// Pin the TOTAL first. Both assertions below key on an exact (model, day)
	// tuple, so an emission under a differently-shaped key would slip past the
	// absence check in (2) while the presence check in (1) still passed.
	if len(ing.events) != 1 {
		t.Fatalf("emitted %d events, want exactly 1 (gpt-4o only; the unpriced model must "+
			"be skipped) — the keyed assertions below cannot see a mis-keyed emission", len(ing.events))
	}

	idx := indexEvents(ing.events)

	// (1) The priced model is ingested and carries the DERIVED mode.
	ev, ok := idx[eventKey{"gpt-4o", "2026-06-15"}]
	if !ok {
		t.Fatalf("no remainder event for gpt-4o — the fixture stopped exercising this " +
			"path, so a pass here would be vacuous")
	}
	if ev.BillingMode == "" {
		t.Fatalf("BillingMode is empty — the poller discarded the resolved mode, so the " +
			"store defaults this row to per_token (#525). Want \"per_token\", derived from " +
			"the price-table entry rather than assumed")
	}
	if ev.BillingMode != "per_token" {
		t.Errorf("BillingMode = %q, want %q (an exact price-table entry IS metered per token)",
			ev.BillingMode, "per_token")
	}
	// Host stays empty: the usage report bills the account and names no serving
	// host. This is also what holds the fix cost-neutral.
	if ev.Host != "" {
		t.Errorf("Host = %q, want empty — the org report names no serving host, and a "+
			"non-empty host would change the price key and move cost", ev.Host)
	}

	// Cost pinned to a LITERAL derived from the published rate card, not from the
	// pricing code. The pre-existing assertions in poller_test.go compute their
	// expectation with store.ComputeCost — the sibling of the function under test —
	// so they agree with any repricing, including a wrong one. #525 changed the
	// pricing call on exactly those lines, so at least one arm here has to be
	// independent of it.
	//
	// gpt-4o is $2.50/M in, $10.00/M out (prices.yaml). No captured baseline, so
	// the remainder is the full bucket: 1000 in, 500 out.
	//   1000 × $2.50/M = $0.0025 ; 500 × $10.00/M = $0.0050 ; total $0.0075
	const wantCost int64 = 7_500
	if ev.CostMicro != wantCost {
		t.Errorf("CostMicro = %d, want %d ($0.0075 at the published gpt-4o rate). "+
			"#525 must not move cost; check the gpt-4o row in internal/store/prices.yaml "+
			"before suspecting the host argument", ev.CostMicro, wantCost)
	}

	// (2) The unpriced model is NOT ingested. This is the measured basis for the
	// "size-class fallback is unreachable here" claim in poller.go's comment; if
	// this ever starts emitting an event, that comment is wrong and the row's mode
	// needs deciding rather than inheriting.
	if _, ok := idx[eventKey{"totally-unknown-model-xyz", "2026-06-15"}]; ok {
		t.Errorf("an unpriced model produced a remainder event. poller.go documents the " +
			"size-class fallback as UNREACHABLE from this producer on the strength of the " +
			"upstream skip — if that changed, the comment is now false and this row's " +
			"billing_mode is a guess that must be labelled as one")
	}
}
