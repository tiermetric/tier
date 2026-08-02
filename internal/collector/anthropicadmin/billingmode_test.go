package anthropicadmin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The #525 regression for the Anthropic org poller. Mirror of the OpenAI one —
// same defect, same producer shape, same narrow scope.
//
// The remainder event was built with store.ComputeCost, which resolves a
// billing_mode and then DISCARDS it, so every org-poller row reached the store
// with an empty BillingMode and was defaulted to "per_token" at insert.
//
// Both halves of the honest scope are pinned:
//
//  1. Every model reaching the event builder is an exact price-table hit — an
//     unpriced model is skipped upstream and never ingested. The size-class
//     fallback is therefore unreachable from this producer and the resolved mode
//     is always per_token today, so the stored VALUE does not change. What changes
//     is that it is DERIVED from the entry that priced the row, not defaulted.
//  2. That matters because prices.yaml can declare an explicit billing_mode,
//     which the old code would have silently overwritten with per_token. (Every
//     explicit mode today sits on a host-qualified row and is set to per_token by
//     #268, so it is unreachable from a host-blind caller; the first mode a bare
//     model key could carry is subscription, for the Ollama Cloud / GLM rows #113
//     anticipates and #304 gates.)
//
// Assertion (2) below exists so the "unreachable" claim in poller.go's comment is
// measured rather than asserted — the same class of unmeasured documentation claim
// the project has been bitten by before.
func TestPoll_RemainderEventCarriesResolvedBillingMode(t *testing.T) {
	usage := usageReport{Data: []usageBucket{
		dayBucket("2026-06-15",
			// In the shipped price table → ingested, metered per token.
			modelResult("claude-sonnet-4", 1000, 500, 0, 0, 0),
			// Absent from the table → skipped entirely, never ingested.
			modelResult("totally-unknown-model-xyz", 2000, 100, 0, 0, 0)),
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, usagePath):
			_, _ = w.Write(mustJSON(t, usage))
		case strings.HasPrefix(r.URL.Path, costPath):
			_, _ = w.Write(mustJSON(t, costReport{}))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
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
		t.Fatalf("emitted %d events, want exactly 1 (claude-sonnet-4 only; the unpriced "+
			"model must be skipped) — the keyed assertions below cannot see a mis-keyed "+
			"emission", len(ing.events))
	}

	idx := indexEvents(ing.events)

	// (1) The priced model is ingested and carries the DERIVED mode.
	ev, ok := idx[eventKey{"claude-sonnet-4", "2026-06-15"}]
	if !ok {
		t.Fatalf("no remainder event for claude-sonnet-4 — the fixture stopped exercising " +
			"this path, so a pass here would be vacuous")
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
	// claude-sonnet-4 is $3.00/M in, $15.00/M out (prices.yaml). No captured
	// baseline, so the remainder is the full bucket: 1000 in, 500 out.
	//   1000 × $3.00/M = $0.0030 ; 500 × $15.00/M = $0.0075 ; total $0.0105
	const wantCost int64 = 10_500
	if ev.CostMicro != wantCost {
		t.Errorf("CostMicro = %d, want %d ($0.0105 at the published claude-sonnet-4 rate). "+
			"#525 must not move cost; check the claude-sonnet-4 row in "+
			"internal/store/prices.yaml before suspecting the host argument", ev.CostMicro, wantCost)
	}

	// (2) The unpriced model is NOT ingested — the measured basis for the
	// "size-class fallback is unreachable here" claim in poller.go.
	if _, ok := idx[eventKey{"totally-unknown-model-xyz", "2026-06-15"}]; ok {
		t.Errorf("an unpriced model produced a remainder event. poller.go documents the " +
			"size-class fallback as UNREACHABLE from this producer on the strength of the " +
			"upstream skip — if that changed, the comment is now false and this row's " +
			"billing_mode is a guess that must be labelled as one")
	}
}
