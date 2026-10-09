package collector

import (
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// The #525 regression for the PRIMARY capture path.
//
// joinSessionsToCommits used store.ComputeCost, which resolves a billing_mode and
// then DISCARDS it. Every emitted event therefore carried an empty BillingMode,
// and the store's normalizeBillingMode turned that into the "per_token" default at
// insert. The row claimed a metered per-token basis regardless of how its cost was
// actually derived.
//
// The sharp edge: a model ABSENT from the price table is priced at the size-class
// / self-hosted-medium fallback — a guess — while reporting per_token. prices.yaml
// deliberately anticipates unreleased models, so "not in the table yet" is a
// routine state, not an exotic one. And billing_mode is published by BOTH /export
// surfaces, so the wrong basis leaves the building.
//
// This is the same defect #492 fixed on /events. These assertions pin the WIRE
// VALUES as literals rather than reusing store.BillingPerToken /
// store.BillingSelfHostedAmortized: the export contract is the string, so a
// constant that changed value must fail here rather than quietly re-labelling
// every published row. (Ledger: a tautological accept arm asked the code under
// test what to expect and verified it agreed with itself.)
func TestJoinSessionsToCommits_StoresResolvedBillingMode(t *testing.T) {
	ts := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		model string
		want  string
		why   string
	}{
		{
			name:  "priced model reports the metered basis",
			model: "claude-sonnet-4",
			want:  "per_token",
			why:   "an exact price-table entry IS metered per token",
		},
		{
			name:  "unpriced model admits the amortized guess",
			model: llamaModel,
			want:  "self_hosted_amortized",
			why: "no MODEL-ONLY table entry → the size-class fallback priced it, and the " +
				"row must say so instead of claiming a per-token basis it never earned",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := joinSessionsToCommits(sessionForModel(tc.model, ts, 1000, 500), nil, "alice", "")
			if len(events) != 1 {
				t.Fatalf("emitted %d events, want 1", len(events))
			}
			got := events[0]

			// The defect's signature is the EMPTY string — that is what the store
			// silently defaults to per_token. Calling it out separately keeps the
			// pre-fix failure message honest about what actually went wrong.
			if got.BillingMode == "" {
				t.Fatalf("BillingMode is empty — the collector discarded the resolved mode, "+
					"so the store will default this row to per_token (#525). Want %q: %s",
					tc.want, tc.why)
			}
			if got.BillingMode != tc.want {
				t.Errorf("BillingMode = %q, want %q: %s", got.BillingMode, tc.want, tc.why)
			}

			// Host must stay EMPTY on the EVENT. Note this is a weaker statement
			// than it looks: jsonl.go never sets Host at all, so this assertion
			// cannot detect a changed host ARGUMENT to ComputeCostHost. That is
			// what TestJoinSessionsToCommits_BillingModeFixIsCostNeutral is for.
			if got.Host != "" {
				t.Errorf("Host = %q, want empty — a session file cannot know the serving host, "+
					"and a non-empty host would change the price key and move cost", got.Host)
			}
		})
	}
}

// llamaModel has a MODEL-ONLY miss but a real HOST-QUALIFIED row
// ("…@openrouter.ai"), which is what makes it the only usable discriminator for
// the host argument. See the cost-neutrality test below.
const llamaModel = "meta-llama/llama-3.3-70b-instruct"

// TestJoinSessionsToCommits_BillingModeFixIsCostNeutral is the CONTROL ARM for the
// safety claim in #525: the fix recovers the discarded mode and moves NO cost.
//
// ⚠️ THE FIXTURE IS THE WHOLE TEST. A first version of this used claude-sonnet-4
// to "prove" that jsonl.go still passes an empty host — and it could not have
// failed for that reason. Every `model@host` row in prices.yaml is an open-weights
// model with provider: self-hosted; there is NO host-qualified row for any
// Anthropic or OpenAI first-party model. So ComputeCostHost("openrouter.ai",
// "claude-sonnet-4", …) misses the host key, falls through to the SAME model-only
// entry, and returns a byte-identical cost AND mode. Someone could change the ""
// literal in jsonl.go to "openrouter.ai" and every assertion would have stayed
// green — the exact ledger shape where a control arm used the fallback row to
// prove a real table lookup.
//
// llamaModel fixes that: model-only MISS (→ size-class large fallback, $2.00/M
// combined, self_hosted_amortized) versus an exact @openrouter.ai row ($0.10 in /
// $0.32 out, per_token). Both cost and mode flip, so a leaked host is caught.
//
// Expected costs are pinned as LITERAL micro-dollars from the table's published
// rates rather than by calling the pricing code, because a test that asked
// ComputeCost what the answer should be would agree with any repricing.
func TestJoinSessionsToCommits_BillingModeFixIsCostNeutral(t *testing.T) {
	ts := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		model    string
		in, out  int
		wantCost int64
		wantMode string
		rate     string
	}{
		{
			// 100k+100k, deliberately BELOW any context threshold: the sibling row
			// claude-sonnet-4-5 already carries context_threshold: 200000 with a
			// higher over-tier rate, so a fixture at 1M tokens would break here for
			// an unrelated reason if that tier were ever extended to this model.
			name: "exact model-only hit", model: "claude-sonnet-4",
			in: 100_000, out: 100_000,
			wantCost: 1_800_000, wantMode: "per_token",
			rate: "$3.00/M in + $15.00/M out on 100k each = $0.30 + $1.50 = $1.80",
		},
		{
			name: "model-only miss, size-class fallback", model: llamaModel,
			in: 1_000_000, out: 1_000_000,
			wantCost: 4_000_000, wantMode: "self_hosted_amortized",
			rate: "self-hosted-large, combined $2.00/M on 2M tokens = $4.00",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := joinSessionsToCommits(sessionForModel(tc.model, ts, tc.in, tc.out), nil, "alice", "")
			if len(events) != 1 {
				t.Fatalf("emitted %d events, want 1", len(events))
			}
			if got := events[0].CostMicro; got != tc.wantCost {
				t.Errorf("CostMicro = %d, want %d (%s).\n"+
					"#525 must recover the billing_mode WITHOUT moving cost. Before blaming the "+
					"host argument, check whether the %s row in internal/store/prices.yaml "+
					"changed — a rate edit is the likelier cause and fails identically.",
					got, tc.wantCost, tc.rate, tc.model)
			}
			if got := events[0].BillingMode; got != tc.wantMode {
				t.Errorf("BillingMode = %q, want %q", got, tc.wantMode)
			}
		})
	}

	// THE DISCRIMINATOR PROOF. Everything above pins the host-blind values; this
	// proves those values would actually CHANGE if jsonl.go leaked a host, i.e.
	// that the arms above can fail for the reason they claim. Without it they are
	// just two more numbers.
	blindCost, blindMode := store.ComputeCostHost("", llamaModel,
		store.CostUsage{Input: 1_000_000, Output: 1_000_000})
	hostCost, hostMode := store.ComputeCostHost("openrouter.ai", llamaModel,
		store.CostUsage{Input: 1_000_000, Output: 1_000_000})

	if blindCost == hostCost {
		t.Errorf("host-blind and host-qualified pricing agree for %s (%d µ$ both ways). "+
			"The fixture no longer discriminates, so the cost arm above cannot detect a "+
			"leaked host and this test has quietly stopped guarding what it claims to. "+
			"Pick a model that still has a host-qualified row in prices.yaml.",
			llamaModel, blindCost)
	}
	if blindMode == hostMode {
		t.Errorf("host-blind and host-qualified billing_mode agree for %s (%q both ways); "+
			"the mode arm above cannot detect a leaked host either", llamaModel, blindMode)
	}
}

// sessionForModel builds a one-message session for the given model and counts.
func sessionForModel(model string, ts time.Time, in, out int) []sessionSummary {
	return []sessionSummary{{
		SessionID: "sess-525",
		GitBranch: "fix/525",
		Model:     model,
		StartTime: ts,
		EndTime:   ts,
		Messages: []messageUsage{{
			messageID: "m1", timestamp: ts,
			model: model, input: in, output: out,
		}},
	}}
}
