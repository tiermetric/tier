package store

import (
	"testing"
	"time"
)

func TestCursorAdminReingestPreservesFrozenAggregate(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	const requestInput = 40_000
	e := TokenEvent{TokenDerived: true, Developer: "alice", Model: "claude-haiku-5-5",
		Source: "cursor-admin", Fidelity: "daily", IdempotencyKey: "cursor-row",
		Timestamp: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), InputTok: 2 * requestInput}
	// Two sub-threshold requests initially sum to less than the request tier.
	e.CostMicro = 2 * ComputeCost(e.Model, CostUsage{Input: requestInput})
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	before, _, _ := pricingRow(t, db, e.IdempotencyKey)
	if before.CostMicro != e.CostMicro || tokenVector(before) != tokenVector(e) {
		t.Fatalf("initial aggregate: cost=%d tokens=%v, want cost=%d tokens=%v", before.CostMicro, tokenVector(before), e.CostMicro, tokenVector(e))
	}
	// A larger replay crosses the tier only when priced as one request.
	e.InputTok = 2 * 60_000
	e.CostMicro = 2 * ComputeCost(e.Model, CostUsage{Input: 60_000})
	if aggregateCost := ComputeCost(e.Model, usage(e)); aggregateCost <= e.CostMicro {
		t.Fatalf("fixture must cross the request tier: per-request sum=%d aggregate=%d", e.CostMicro, aggregateCost)
	}
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	after, _, _ := pricingRow(t, db, e.IdempotencyKey)
	if after.CostMicro != before.CostMicro || tokenVector(after) != tokenVector(before) {
		t.Fatalf("re-ingest changed frozen aggregate: cost=%d tokens=%v, want cost=%d tokens=%v", after.CostMicro, tokenVector(after), before.CostMicro, tokenVector(before))
	}
}

// These fixtures exercise the stored-row contract, independently of the future
// pollers' ingestion paths. Cursor aggregates are frozen at source level;
// Copilot's cost-only rows are protected by row-level guards.
func TestRepriceNewPollerRowGuards(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		tokens       int
		excluded     bool
	}{
		{"copilot-authoritative", "copilot-billing", 0, true},
		{"cursor-list-priced", "cursor-admin", 1000, true},
		{"cursor-tokenless", "cursor-admin", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			const before = int64(42_000_000)
			e := TokenEvent{Developer: "alice", Model: "claude-sonnet-4", Source: tc.source,
				InputTok: tc.tokens, CostMicro: before, Fidelity: "daily", PriceVersion: 1,
				IdempotencyKey: "row", Timestamp: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			res, err := db.Reprice(t.Context(), RepriceOptions{FromVersion: 1, Commit: true})
			if err != nil {
				t.Fatal(err)
			}
			after, _, _ := pricingRow(t, db, e.IdempotencyKey)
			wantCost, wantExcluded := ComputeCost(e.Model, CostUsage{Input: tc.tokens}), int64(0)
			if tc.excluded {
				wantCost, wantExcluded = before, 1
			}
			if after.CostMicro != wantCost || res.ExcludedRowCount != wantExcluded {
				t.Fatalf("cost=%d excluded=%d, want cost=%d excluded=%d", after.CostMicro, res.ExcludedRowCount, wantCost, wantExcluded)
			}
		})
	}
}

func TestNewPollerSourcesUseEventPricing(t *testing.T) {
	for _, source := range []string{"copilot-billing", "cursor-admin"} {
		for _, operation := range []string{"Reprice", "RepairPricingBasis"} {
			t.Run(source+"/"+operation, func(t *testing.T) {
				db, cleanup := newTestDB(t)
				defer cleanup()
				if isPoller(source) || pollerBaseline(source) != "" {
					t.Fatal("new vendor feeds must not inherit Anthropic/OpenAI remainder policies")
				}
				ts := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
				e := TokenEvent{TokenDerived: true, Developer: "alice", Model: "claude-haiku-5-5",
					Source: source, Fidelity: "daily", IdempotencyKey: "row", Timestamp: ts}
				if source == "cursor-admin" {
					// Two sub-threshold requests cross the tier only when summed.
					// The daily row must retain the sum of their per-request prices.
					const requestInput = 60_000
					e.InputTok = 2 * requestInput
					e.CostMicro = 2 * ComputeCost(e.Model, CostUsage{Input: requestInput})
					if aggregateCost := ComputeCost(e.Model, usage(e)); aggregateCost <= e.CostMicro {
						t.Fatalf("fixture must cross the request tier: per-request sum=%d aggregate=%d", e.CostMicro, aggregateCost)
					}
				} else {
					e.CostMicro = 42_000_000
				}
				if err := db.InsertTokenEvent(t.Context(), e); err != nil {
					t.Fatal(err)
				}
				got, basis, _ := pricingRow(t, db, e.IdempotencyKey)
				wantBasis := "token_derived"
				if source == "copilot-billing" {
					wantBasis = "caller_authoritative"
				}
				if got.CostMicro != e.CostMicro || basis != wantBasis {
					t.Fatalf("cost=%d basis=%s, want cost=%d basis=%s", got.CostMicro, basis, e.CostMicro, wantBasis)
				}
				disclosure, err := db.AggregatePricingWindow(t.Context(), ts.Add(-time.Hour), ts.Add(time.Hour), FleetWide)
				if err != nil {
					t.Fatal(err)
				}
				if disclosure.ApproximateRows != 0 || disclosure.HistoricalRows != 0 || disclosure.UnverifiableRows != 0 {
					t.Fatalf("new vendor feed incorrectly disclosed as a daily pricing approximation: %+v", disclosure)
				}
				switch operation {
				case "Reprice":
					if _, err := db.Reprice(t.Context(), RepriceOptions{FromVersion: 1, Commit: true}); err != nil {
						t.Fatal(err)
					}
				case "RepairPricingBasis":
					if _, err := db.RepairPricingBasis(t.Context(), got.PriceVersion, true); err != nil {
						t.Fatal(err)
					}
				}
				after, _, _ := pricingRow(t, db, e.IdempotencyKey)
				if after.CostMicro != e.CostMicro {
					t.Fatalf("%s rewrote per-request daily cost: %d -> %d", operation, e.CostMicro, after.CostMicro)
				}
			})
		}
	}
}
