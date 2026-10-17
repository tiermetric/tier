package store

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestComputeAggregateCostHostUnknownModelObservable(t *testing.T) {
	for _, model := range []string{"unlisted-aggregate-model", "unlisted-aggregate-70b"} {
		t.Run(model, func(t *testing.T) {
			resetUnknownModelDedupe(t)
			buf := captureUnknownModelLogger(t)
			events, cost, priced := &countingRecorder{}, &fakeCostRecorder{}, &fakeCostRecorder{}
			SetUnknownModelRecorder(events)
			SetUnknownModelCostRecorder(cost)
			SetPricedCostRecorder(priced)
			t.Cleanup(func() {
				SetUnknownModelRecorder(nil)
				SetUnknownModelCostRecorder(nil)
				SetPricedCostRecorder(nil)
			})
			got, _ := ComputeAggregateCostHost("", model, CostUsage{Input: 1_000_000})
			if got <= 0 || cost.calls != 1 || cost.sum != float64(got) || events.n != 1 {
				t.Errorf("unknown-model counters: events=%d cost=%+v priced=%d", events.n, cost, got)
			}
			if priced.calls != 1 || priced.sum != float64(got) {
				t.Errorf("total priced counter: %+v, want %d", priced, got)
			}
			if !strings.Contains(buf.String(), model) {
				t.Errorf("missing model warning: %q", buf.String())
			}
		})
	}
}

func TestAggregatePricingRequestThresholdAndCacheClasses(t *testing.T) {
	model := "claude-haiku-5-5"
	small := ComputeCost(model, CostUsage{Input: 60000})
	aggregate, _ := ComputeAggregateCostHost("", model, CostUsage{Input: 120000})
	request := ComputeCost(model, CostUsage{Input: 120000})
	if small != 6000 || aggregate != 12000 || request != 60000 {
		t.Fatalf("2x60K=%d aggregate=%d genuine120K=%d", 2*small, aggregate, request)
	}
	// Cache reads and both TTL classes count toward request context; output does not.
	u := CostUsage{Input: 30000, Output: 1000, CacheRead: 30000, CacheWrite5m: 30000, CacheWrite1h: 30000}
	agg, _ := ComputeAggregateCostHost("", model, u)
	if agg != 13550 || ComputeCost(model, u) != 67750 {
		t.Fatalf("five classes: base=%d request=%d", agg, ComputeCost(model, u))
	}
	if ComputeCost(model, CostUsage{Input: 100000}) != 10000 {
		t.Fatal("request threshold boundary changed")
	}
}

func TestAggregatePricingProspectiveCutoverLegacyExposureRetryAndMissingBasis(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ts := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	for _, source := range []string{"anthropic-admin", "openai-usage"} {
		legacy := TokenEvent{Developer: "alice", Repo: "acme/app", Model: "claude-haiku-5-5", Source: source, InputTok: 120000, Timestamp: ts, IdempotencyKey: source + "old"}
		legacy.CostMicro = ComputeCost(legacy.Model, usage(legacy))
		if err := db.InsertTokenEvent(t.Context(), legacy); err != nil {
			t.Fatal(err)
		}
		old, _, oldID := pricingRow(t, db, legacy.IdempotencyKey)
		fresh := legacy
		fresh.Timestamp = ts.Add(24 * time.Hour)
		fresh.IdempotencyKey = source + "new"
		fresh.TokenDerived = true
		fresh.PricingBasis = PricingAggregateApprox
		fresh.CostMicro, _ = ComputeAggregateCostHost("", fresh.Model, usage(fresh))
		if err := db.InsertTokenEvent(t.Context(), fresh); err != nil {
			t.Fatal(err)
		}
		before, basis, id := pricingRow(t, db, fresh.IdempotencyKey)
		if basis != PricingAggregateApprox || id == oldID || before.CostMicro != 12000 {
			t.Fatalf("cutover basis=%s %+v", basis, before)
		}
		// A newly appearing model on an old day inherits the old policy even if an
		// agent proposed the new rule. The store arbitrates under the write lock.
		late := fresh
		late.Timestamp = ts
		late.Model = "claude-haiku-5-5-20261001"
		late.IdempotencyKey = source + "late"
		if err := db.InsertTokenEvent(t.Context(), late); err != nil {
			t.Fatal(err)
		}
		_, b, _ := pricingRow(t, db, late.IdempotencyKey)
		if b != PricingLegacyAggregate {
			t.Fatalf("legacy day acquired %s", b)
		}
		audits := pricingCount(t, db)
		for _, e := range []TokenEvent{legacy, fresh} {
			e.InputTok = 240000
			e.CostMicro = math.MaxInt64
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
		}
		after, _, afterID := pricingRow(t, db, fresh.IdempotencyKey)
		afterOld, _, _ := pricingRow(t, db, legacy.IdempotencyKey)
		if after != before || afterID != id || afterOld != old || pricingCount(t, db) != audits {
			t.Fatal("completed rows changed on repoll")
		}
	}
	rows, err := db.CostConsistency(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var historical, approx int
	for _, r := range rows {
		if r.Class == "poller_aggregate_tiered" && r.Status == "unverifiable" {
			historical++
		}
		if r.Class == PricingAggregateApprox && r.Status == "consistent" {
			approx++
		}
	}
	if historical != 4 || approx != 2 {
		t.Fatalf("pre/post classifications %+v", rows)
	}
	info, err := db.AggregatePricingWindow(t.Context(), ts.Add(-24*time.Hour), ts.Add(3*24*time.Hour), RepoScope(""))
	if err != nil {
		t.Fatal(err)
	}
	if info.HistoricalRows != 4 || info.ApproximateRows != 2 || info.PossibleIncreaseMicro != 96000 || info.CeilingStatus != "available" {
		t.Fatalf("disclosure %+v", info)
	}
	// With a missing original snapshot, neither the diagnostic nor the ceiling
	// reconstructs yesterday using the active table.
	if _, err := db.db.Exec(`UPDATE token_events SET rate_snapshot='missing'`); err != nil {
		t.Fatal(err)
	}
	info, err = db.AggregatePricingWindow(t.Context(), ts.Add(-24*time.Hour), ts.Add(3*24*time.Hour), RepoScope(""))
	if err != nil {
		t.Fatal(err)
	}
	if info.UnverifiableRows != 6 || info.CeilingStatus != "unverifiable_original_basis" {
		t.Fatalf("missing basis %+v", info)
	}
	rows, err = db.CostConsistency(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Class != "basis_unavailable" {
			t.Fatalf("active-table fallback %+v", r)
		}
	}
	var cutover int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM tier_migrations WHERE name=?`, AggregatePricingCutover).Scan(&cutover); err != nil || cutover != 1 {
		t.Fatalf("named cutover %d %v", cutover, err)
	}
}

func TestAggregatePricingCeilingIncludesAllCacheRatesAndOriginalRule(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	e := basisEvent("cache-aggregate")
	e.Source = "anthropic-admin"
	e.Model = "claude-haiku-5-5"
	e.InputTok = 30000
	e.OutputTok = 1000
	e.CacheRead = 30000
	e.CacheWrite5m = 30000
	e.CacheWrite1h = 30000
	e.PricingBasis = PricingAggregateApprox
	e.TokenDerived = true
	e.CostMicro, _ = ComputeAggregateCostHost("", e.Model, usage(e))
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	info, err := db.AggregatePricingWindow(t.Context(), e.Timestamp.Add(-time.Hour), e.Timestamp.Add(time.Hour), RepoScope(""))
	if err != nil {
		t.Fatal(err)
	}
	if info.PossibleIncreaseMicro != 54200 {
		t.Fatalf("cache-weighted ceiling %+v", info)
	}
	_, _, id := pricingRow(t, db, e.IdempotencyKey)
	var raw string
	if err := db.db.QueryRow(`SELECT body FROM pricing_rate_snapshots WHERE id=?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var snapshot rateSnapshot
	_ = json.Unmarshal([]byte(raw), &snapshot)
	if snapshot.Rule != AggregateRuleV1 {
		t.Fatalf("rule %+v", snapshot)
	}
	snapshot.Rule = "future-rule"
	bad, _ := json.Marshal(snapshot)
	if _, err := decodeRateSnapshot(TokenEvent{PriceVersion: snapshot.Version, BillingMode: snapshot.Rate.BillingMode, PricingBasis: PricingAggregateApprox}, id, string(bad)); err == nil {
		t.Fatal("unknown rule resolved")
	}
}

func TestAggregatePricingCeilingHandlesMixedTierRateDirections(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	key := "claude-haiku-5-5"
	original := priceTable[key]
	defer func() { priceTable[key] = original }()
	p := original
	p.inputPerM = 1
	p.outputPerM = 1
	p.inputPerMOver = 0.1
	p.outputPerMOver = 10
	p.contextThreshold = 100000
	priceTable[key] = p
	// Valid override rates can discount tiered input while increasing output.
	// One long request + nine small ones costs more than pricing everything at
	// either tier. A ceiling must use each class's highest retained rate.
	e := basisEvent("mixed-tier")
	e.Model = key
	e.Source = "anthropic-admin"
	e.InputTok = 1000000
	e.OutputTok = 100000
	e.TokenDerived = true
	e.PricingBasis = PricingAggregateApprox
	e.CostMicro, _ = ComputeAggregateCostHost("", key, usage(e))
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	actual := ComputeCost(key, CostUsage{Input: 100001, Output: 100000}) + 8*ComputeCost(key, CostUsage{Input: 100000}) + ComputeCost(key, CostUsage{Input: 99999})
	if actual <= e.CostMicro {
		t.Fatal("positive control does not expose mixed-tier underpricing")
	}
	info, err := db.AggregatePricingWindow(t.Context(), e.Timestamp.Add(-time.Hour), e.Timestamp.Add(time.Hour), FleetWide)
	if err != nil {
		t.Fatal(err)
	}
	if info.PossibleIncreaseMicro != 900000 || info.PossibleIncreaseMicro < actual-e.CostMicro {
		t.Fatalf("ceiling=%d actual increase=%d", info.PossibleIncreaseMicro, actual-e.CostMicro)
	}
}

func TestAggregatePricingSingleRowOverflowAndPrecedence(t *testing.T) {
	for _, order := range []string{"overflow", "overflow-estimated", "estimated-overflow", "overflow-estimated-missing", "missing-estimated-overflow"} {
		t.Run(order, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			for i, kind := range strings.Split(order, "-") {
				e := basisEvent(fmt.Sprint(i))
				e.Source = "anthropic-admin"
				e.PricingBasis = PricingAggregateApprox
				e.TokenDerived = true
				if err := db.InsertTokenEvent(t.Context(), e); err != nil {
					t.Fatal(err)
				}
				got, _, _ := pricingRow(t, db, e.IdempotencyKey)
				if kind == "missing" {
					if _, err := db.db.Exec(`UPDATE token_events SET rate_snapshot='missing' WHERE id=?`, got.ID); err != nil {
						t.Fatal(err)
					}
					continue
				}
				p, _ := initialPrice(e.Host, e.Model)
				if kind == "overflow" {
					p.inputPerM = 1e10
					p.cacheWrite1hMult = 1e10
				}
				tx, err := db.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				id, err := saveRateSnapshot(t.Context(), tx, rateSnapshot{Rule: AggregateRuleV1, Version: got.PriceVersion, Rate: snapshotPrice(p), Guessed: kind == "estimated"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(`UPDATE token_events SET rate_snapshot=?,cache_write_1h=1 WHERE id=?`, id, got.ID); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			e := basisEvent("")
			info, err := db.AggregatePricingWindow(t.Context(), e.Timestamp.Add(-time.Hour), e.Timestamp.Add(time.Hour), FleetWide)
			if err != nil {
				t.Fatal(err)
			}
			want := "ceiling_overflow"
			if strings.Contains(order, "estimated") {
				want = "estimated_rates_no_billing_bound"
			}
			if strings.Contains(order, "missing") {
				want = "unverifiable_original_basis"
			}
			if info.CeilingStatus != want {
				t.Fatalf("ceiling status=%s want %s (%+v)", info.CeilingStatus, want, info)
			}
		})
	}
}
