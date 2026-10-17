package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPricingBasisPartialFinalWatcherReplay(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	e := TokenEvent{Developer: "alice", IssueID: "1231", Model: "claude-sonnet-4", Source: "jsonl", IdempotencyKey: "watcher", InputTok: 100, Timestamp: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	e.CostMicro = ComputeCost(e.Model, CostUsage{Input: e.InputTok})
	if err := db.InsertTokenEvent(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	e.InputTok = 200
	e.OutputTok = 30
	e.CostMicro = ComputeCost(e.Model, CostUsage{Input: e.InputTok, Output: e.OutputTok})
	if err := db.InsertTokenEvent(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	var cost int64
	if err := db.db.QueryRow(`SELECT cost_micro FROM token_events WHERE idempotency_key='watcher'`).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost != e.CostMicro {
		t.Fatalf("final cost = %d, want %d (partial cost frozen)", cost, e.CostMicro)
	}
	rows, err := db.CostConsistency(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Status != "consistent" {
		t.Fatalf("E2 post-fix re-query %+v", rows)
	}
	t.Log("E2 synthetic control: understated before=1, after=0; new rows=1 (not a live-dogfood receipt)")
}

func TestRepricePricingBasisMismatchedTokenCostIsNotOverride(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	e := basisEvent("mismatched-token-cost")
	want := e.CostMicro
	e.CostMicro = 9_000_000
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	before, basis, snapshot := pricingRow(t, db, e.IdempotencyKey)
	if before.CostMicro != e.CostMicro || basis != "token_derived" || snapshot == "" {
		t.Fatalf("mismatch inferred an override or changed stored dollars: %+v basis=%s snapshot=%s", before, basis, snapshot)
	}
	res, err := db.Reprice(t.Context(), RepriceOptions{FromVersion: before.PriceVersion, Commit: true})
	if err != nil {
		t.Fatal(err)
	}
	after, basis, _ := pricingRow(t, db, e.IdempotencyKey)
	if res.ChangedRowCount != 1 || res.ExcludedRowCount != 0 || after.CostMicro != want || basis != "token_derived" {
		t.Fatalf("explicit reprice did not repair token cost: result=%+v row=%+v basis=%s", res, after, basis)
	}
}

func TestPricingBasisSuppliedCostPromotesCountsUntilExplicitRepair(t *testing.T) {
	for _, operation := range []string{"reprice", "original-basis-repair"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "supplied-cost.db")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			e := basisEvent("supplied")
			e.InputTok, e.OutputTok, e.CacheRead, e.CacheWrite5m, e.CacheWrite1h = 100, 10, 20, 30, 40
			e.CostMicro = 9_000_000
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			before, basis, snapshot := pricingRow(t, db, e.IdempotencyKey)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			// Even a trusted later writer must honor the retained first-write
			// policy, across both single and bulk inserts and after reopening.
			e.TokenDerived = true
			e.InputTok, e.OutputTok, e.CacheRead, e.CacheWrite5m, e.CacheWrite1h = 200, 20, 40, 60, 80
			e.CostMicro = ComputeCost(e.Model, usage(e))
			if err := db.InsertTokenEvents(t.Context(), []TokenEvent{e}); err != nil {
				t.Fatal(err)
			}
			promoted, gotBasis, gotSnapshot := pricingRow(t, db, e.IdempotencyKey)
			if tokenVector(promoted) != tokenVector(e) || promoted.CostMicro != before.CostMicro || promoted.PriceVersion != before.PriceVersion || promoted.BillingMode != before.BillingMode || gotBasis != basis || gotSnapshot != snapshot {
				t.Fatalf("supplied-cost replay changed price or lost count promotion: before=%+v after=%+v", before, promoted)
			}
			if operation == "reprice" {
				_, err = db.Reprice(t.Context(), RepriceOptions{FromVersion: before.PriceVersion, Commit: true})
			} else {
				_, err = db.RepairPricingBasis(t.Context(), before.PriceVersion, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			repaired, _, repairedSnapshot := pricingRow(t, db, e.IdempotencyKey)
			if repaired.CostMicro != e.CostMicro || repairedSnapshot == snapshot {
				t.Fatalf("explicit repair did not establish derived cost: %+v snapshot=%s", repaired, repairedSnapshot)
			}
			e.InputTok = 300
			e.CostMicro = ComputeCost(e.Model, usage(e))
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			final, _, _ := pricingRow(t, db, e.IdempotencyKey)
			if tokenVector(final) != tokenVector(e) || final.CostMicro != e.CostMicro {
				t.Fatalf("promotion after repair did not use original rates: %+v", final)
			}
		})
	}
}

func TestPricingBasisIncomparableTTL(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy-first", true: "nested-first"}[reverse], func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			e := TokenEvent{Developer: "alice", Model: "claude-sonnet-4", Source: "jsonl", IdempotencyKey: "ttl", CacheWrite5m: 1000, Timestamp: time.Now()}
			if reverse {
				e.CacheWrite5m = 0
				e.CacheWrite1h = 1000
			}
			e.CostMicro = ComputeCost(e.Model, CostUsage{CacheWrite5m: e.CacheWrite5m, CacheWrite1h: e.CacheWrite1h})
			if err := db.InsertTokenEvent(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			e.CacheWrite5m, e.CacheWrite1h = e.CacheWrite1h, e.CacheWrite5m
			e.CostMicro = ComputeCost(e.Model, CostUsage{CacheWrite5m: e.CacheWrite5m, CacheWrite1h: e.CacheWrite1h})
			if err := db.InsertTokenEvent(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			var a, b int
			if err := db.db.QueryRow(`SELECT cache_write_5m,cache_write_1h FROM token_events`).Scan(&a, &b); err != nil {
				t.Fatal(err)
			}
			if a+b != 1000 {
				t.Fatalf("synthesized TTL vector (%d,%d)", a, b)
			}
		})
	}
}

func basisEvent(key string) TokenEvent {
	e := TokenEvent{Developer: "alice", IssueID: "1231", Model: "claude-sonnet-4", Source: "jsonl", IdempotencyKey: key, InputTok: 100, Timestamp: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	e.CostMicro = ComputeCost(e.Model, usage(e))
	return e
}
func pricingRow(t *testing.T, db *DB, key string) (TokenEvent, string, string) {
	t.Helper()
	tx, err := db.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	e, basis, id, err := readPricingRow(t.Context(), tx, key)
	if err != nil {
		t.Fatal(err)
	}
	return e, basis, id
}
func pricingCount(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM pricing_observation_audit`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPricingBasisOriginalRateHostReplayAndClamp(t *testing.T) {
	for _, host := range []string{"", "openrouter.ai"} {
		t.Run(host, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			e := basisEvent("original")
			e.Host = host
			if host != "" {
				e.Model = "meta-llama/llama-3.3-70b-instruct"
			}
			e.CostMicro, e.BillingMode = ComputeCostHost(host, e.Model, usage(e))
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			old, basis, id := pricingRow(t, db, e.IdempotencyKey)
			if id == "" || basis != "token_derived" {
				t.Fatalf("basis %s %s", basis, id)
			}
			p, ok := premiumLookup(host, e.Model)
			if !ok {
				t.Fatal("fixture rate missing")
			}
			table := priceTable
			info := activePriceTableInfo
			priceTable = make(map[string]modelPrice, len(table))
			for k, v := range table {
				priceTable[k] = v
			}
			key := NormalizeModel(e.Model)
			if host != "" {
				key = hostQualifiedKey(key, host)
			}
			higher := p
			higher.inputPerM *= 10
			priceTable[key] = higher
			activePriceTableInfo.Version++
			defer func() { priceTable = table; activePriceTableInfo = info }()
			e.InputTok = 200
			e.CostMicro, e.BillingMode = ComputeCostHost(host, e.Model, usage(e))
			if err := db.InsertTokenEvents(t.Context(), []TokenEvent{e}); err != nil {
				t.Fatal(err)
			}
			got, _, afterID := pricingRow(t, db, e.IdempotencyKey)
			want, _ := clampTokenEventCost(priceCallV1(p, usage(e)))
			if got.CostMicro != want || got.PriceVersion != old.PriceVersion || afterID != id || got.Host != old.Host {
				t.Fatalf("promoted %+v; want cost %d under %s", got, want, id)
			}
			audits := pricingCount(t, db)
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			if pricingCount(t, db) != audits {
				t.Fatal("90-day replay wrote audit")
			}
			e.InputTok = 1_000_000_000
			e.OutputTok = 1_000_000_000
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			got, _, _ = pricingRow(t, db, e.IdempotencyKey)
			want, _ = clampTokenEventCost(priceCallV1(p, usage(e)))
			if got.CostMicro != want {
				t.Fatalf("clamped %d want %d", got.CostMicro, want)
			}
		})
	}
}

func TestPricingBasisAuthorityBothOrdersAndConflictReplay(t *testing.T) {
	for _, nestedFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy_first", true: "nested_first"}[nestedFirst], func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			legacy := basisEvent("authority")
			legacy.InputTok = 0
			legacy.CacheWrite5m = 1000
			legacy.CacheWriteEvidence = "legacy"
			nested := legacy
			nested.CacheWrite5m = 0
			nested.CacheWrite1h = 1000
			nested.CacheWriteEvidence = "nested"
			first, last := legacy, nested
			if nestedFirst {
				first, last = nested, legacy
			}
			for _, e := range []TokenEvent{first, last, last} {
				e.CostMicro = ComputeCost(e.Model, usage(e))
				if err := db.InsertTokenEvent(t.Context(), e); err != nil {
					t.Fatal(err)
				}
			}
			got, _, _ := pricingRow(t, db, "authority")
			if got.CacheWrite5m != 0 || got.CacheWrite1h != 1000 || got.CacheWriteEvidence != "nested" {
				t.Fatalf("authority %+v", got)
			}
			rows, err := db.CostConsistency(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if rows[0].Status != "consistent" {
				t.Fatalf("doctor %+v", rows)
			}
		})
	}
	db, cleanup := newTestDB(t)
	defer cleanup()
	e := basisEvent("conflict")
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	e.InputTok = 50
	e.OutputTok = 100
	for i := 0; i < 2; i++ {
		if err := db.InsertTokenEvent(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	if pricingCount(t, db) != 2 {
		t.Fatal("conflict replay duplicated audit")
	}
	rows, err := db.CostConsistency(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Class != "ttl_split_unverifiable" {
		t.Fatalf("conflict not classified %+v", rows)
	}
}

func TestPricingBasisUnavailableRatesAndLegacy(t *testing.T) {
	for _, corruption := range []string{"missing", "rule", "legacy"} {
		t.Run(corruption, func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			e := basisEvent("unavailable")
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			old, _, id := pricingRow(t, db, e.IdempotencyKey)
			if corruption == "rule" {
				var raw string
				if err := db.db.QueryRow(`SELECT body FROM pricing_rate_snapshots WHERE id=?`, id).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var s rateSnapshot
				if err := json.Unmarshal([]byte(raw), &s); err != nil {
					t.Fatal(err)
				}
				s.Rule = "unimplemented-v99"
				b, _ := json.Marshal(s)
				h := sha256.Sum256(b)
				id = "rate1:" + hex.EncodeToString(h[:])
				if _, err := db.db.Exec(`INSERT INTO pricing_rate_snapshots(id,body) VALUES (?,?)`, id, string(b)); err != nil {
					t.Fatal(err)
				}
			} else {
				id = "missing"
			}
			if corruption == "legacy" {
				if _, err := db.db.Exec(`UPDATE token_events SET rate_snapshot=NULL,pricing_basis=NULL WHERE id=?`, old.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := db.db.Exec(`UPDATE token_events SET rate_snapshot=? WHERE id=?`, id, old.ID); err != nil {
					t.Fatal(err)
				}
			}
			e.InputTok = 200
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			got, _, _ := pricingRow(t, db, e.IdempotencyKey)
			if got.InputTok != old.InputTok || got.CostMicro != old.CostMicro {
				t.Fatal("unavailable basis mutated counts/cost")
			}
			rows, err := db.RepairPricingBasis(t.Context(), old.PriceVersion, true)
			if err != nil {
				t.Fatal(err)
			}
			if rows[0].Status != "unverifiable" {
				t.Fatalf("doctor %+v", rows)
			}
		})
	}
}

func TestPricingBasisManualOverrideAndExplicitRepair(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	e := basisEvent("repair")
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	old, _, _ := pricingRow(t, db, e.IdempotencyKey)
	if _, err := db.db.Exec(`UPDATE token_events SET cost_micro=1 WHERE id=?`, old.ID); err != nil {
		t.Fatal(err)
	}
	before := pricingCount(t, db)
	rows, err := db.RepairPricingBasis(t.Context(), old.PriceVersion, false)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Status != "inconsistent" || rows[0].ExpectedMicro != old.CostMicro {
		t.Fatalf("dry run %+v", rows)
	}
	got, _, _ := pricingRow(t, db, e.IdempotencyKey)
	if got.CostMicro != 1 || pricingCount(t, db) != before {
		t.Fatal("dry run wrote")
	}
	if _, err := db.RepairPricingBasis(t.Context(), old.PriceVersion+1, true); err != nil {
		t.Fatal(err)
	}
	got, _, _ = pricingRow(t, db, e.IdempotencyKey)
	if got.CostMicro != 1 {
		t.Fatal("version selection was a floor")
	}
	if _, err := db.RepairPricingBasis(t.Context(), old.PriceVersion, true); err != nil {
		t.Fatal(err)
	}
	got, _, _ = pricingRow(t, db, e.IdempotencyKey)
	if got.CostMicro != old.CostMicro {
		t.Fatal("repair did not use original rate")
	}
	// The sanctioned caller override prevents subsequent count growth repricing dollars.
	e.CostMicro = 42_000_000
	if _, err := db.CorrectManualCostEvent(t.Context(), e, "operator", "invoice"); err != nil {
		t.Fatal(err)
	}
	e.InputTok = 200
	e.CostMicro = 10
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	got, basis, _ := pricingRow(t, db, e.IdempotencyKey)
	if got.CostMicro != 42_000_000 || basis != "caller_authoritative" {
		t.Fatalf("override lost %+v %s", got, basis)
	}
	manual := basisEvent("manual")
	manual.Source = "api"
	manual.CostMicro = 123
	if err := db.InsertManualCostEvent(t.Context(), manual); err != nil {
		t.Fatal(err)
	}
	manual.InputTok = 200
	if err := db.InsertTokenEvent(t.Context(), manual); err != nil {
		t.Fatal(err)
	}
	got, _, _ = pricingRow(t, db, "manual")
	if got.CostMicro != 123 {
		t.Fatal("manual dollars repriced")
	}
}

func TestPricingBasisSealedMonthAndInputBand(t *testing.T) {
	for _, ts := range []time.Time{time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC), time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)} {
		t.Run(ts.Format("2006-01-02"), func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			e := basisEvent("seal")
			e.Timestamp = ts
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			old, _, _ := pricingRow(t, db, e.IdempotencyKey)
			if _, err := db.db.Exec(`INSERT INTO sealed_report (level,period_size,period_start,period_end,k,config_digest,sealed_at,body,body_digest,tool_version,tool_commit) VALUES ('team','month','2026-10-01T00:00:00Z','2026-11-01T00:00:00Z',2,'x','2026-11-02T00:00:00Z','{}','x','dev','x')`); err != nil {
				t.Fatal(err)
			}
			e.InputTok = 200
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			got, _, _ := pricingRow(t, db, e.IdempotencyKey)
			if got.InputTok != old.InputTok || got.CostMicro != old.CostMicro || pricingCount(t, db) != 1 {
				t.Fatal("sealed promotion wrote")
			}
			if _, err := db.db.Exec(`UPDATE token_events SET cost_micro=1 WHERE id=?`, old.ID); err != nil {
				t.Fatal(err)
			}
			rows, err := db.RepairPricingBasis(t.Context(), old.PriceVersion, true)
			if err != nil {
				t.Fatal(err)
			}
			if rows[0].Class != "sealed_cost_mismatch" {
				t.Fatalf("sealed %+v", rows)
			}
			got, _, _ = pricingRow(t, db, e.IdempotencyKey)
			if got.CostMicro != 1 {
				t.Fatal("sealed repair wrote")
			}
		})
	}
}

func TestPricingBasisConcurrentDeliveryAndDSAR(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	e := basisEvent("concurrent")
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, input := range []int{200, 300} {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			next := e
			next.InputTok = n
			errs <- db.InsertTokenEvent(context.Background(), next)
		}(input)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, _, _ := pricingRow(t, db, e.IdempotencyKey)
	if got.InputTok != 300 || got.CostMicro != ComputeCost(e.Model, usage(got)) {
		t.Fatalf("concurrent %+v", got)
	}
	exp, err := db.ExportDeveloper(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.PricingObservations) == 0 || exp.TokenEvents[0].RateSnapshot == nil {
		t.Fatal("DSAR omitted pricing evidence")
	}
	counts, err := db.EraseDeveloper(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if counts["pricing_observation_audit"] == 0 || pricingCount(t, db) != 0 {
		t.Fatal("DSAR retained observations")
	}
}

func TestRepricePricingBasisProtectsSealsPollersAndOverrides(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	for _, key := range []string{"sealed", "polled", "override", "control"} {
		e := basisEvent(key)
		e.TokenDerived = true
		if key == "polled" {
			e.Source = "anthropic-admin"
			e.PricingBasis = PricingAggregateApprox
		}
		if key == "sealed" {
			e.Timestamp = time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
		}
		if err := db.InsertTokenEvent(t.Context(), e); err != nil {
			t.Fatal(err)
		}
		if key == "override" {
			e.CostMicro = 42_000_000
			if _, err := db.CorrectManualCostEvent(t.Context(), e, "operator", "invoice"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.db.Exec(`INSERT INTO sealed_report (level,period_size,period_start,period_end,k,config_digest,sealed_at,body,body_digest,tool_version,tool_commit) VALUES ('team','month','2026-06-01T00:00:00Z','2026-07-01T00:00:00Z',2,'x','2026-07-02T00:00:00Z','{}','x','dev','x')`); err != nil {
		t.Fatal(err)
	}
	before := map[string]TokenEvent{}
	for _, key := range []string{"sealed", "polled", "override"} {
		e, _, _ := pricingRow(t, db, key)
		before[key] = e
	}
	oldTable, oldInfo := priceTable, activePriceTableInfo
	priceTable = make(map[string]modelPrice, len(oldTable))
	for k, v := range oldTable {
		priceTable[k] = v
	}
	p := priceTable["claude-sonnet-4"]
	p.inputPerM *= 2
	priceTable["claude-sonnet-4"] = p
	activePriceTableInfo.Version++
	activePriceTableInfo.TableHash = priceTableHash(priceTable)
	defer func() { priceTable = oldTable; activePriceTableInfo = oldInfo }()
	if _, err := db.Reprice(t.Context(), RepriceOptions{FromVersion: oldInfo.Version, Commit: true}); err != nil {
		t.Fatal(err)
	}
	for key, want := range before {
		got, _, _ := pricingRow(t, db, key)
		if got.CostMicro != want.CostMicro || got.PriceVersion != want.PriceVersion {
			t.Errorf("protected %s changed from %+v to %+v", key, want, got)
		}
	}
	got, _, _ := pricingRow(t, db, "control")
	if got.CostMicro != 600 {
		t.Errorf("positive control=%d want600", got.CostMicro)
	}
}

func TestPricingObservationQueriesUseSearch(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	for _, query := range []string{
		`SELECT 1 FROM pricing_observation_audit WHERE token_event_id=1 AND kind='promotion' AND before_json='x' AND after_json='x'`,
		`SELECT COUNT(*) FROM pricing_observation_audit WHERE token_event_id=1 AND kind='observation_conflict'`,
		`SELECT COUNT(*) FROM pricing_observation_audit WHERE token_event_id=1 AND kind IN ('initial_observation','promotion')`,
	} {
		rows, err := db.db.Query("EXPLAIN QUERY PLAN " + query)
		if err != nil {
			t.Fatal(err)
		}
		var plan string
		for rows.Next() {
			var a, b, c int
			var detail string
			if err := rows.Scan(&a, &b, &c, &detail); err != nil {
				t.Fatal(err)
			}
			plan += detail
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		t.Log(plan)
		if !strings.Contains(plan, "SEARCH") || strings.Contains(plan, "SCAN") {
			t.Errorf("audit query must SEARCH, got %s", plan)
		}
	}
}

func TestInsertTokenEventWaitsForOneSecondWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contention.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	release := holdWriteLockRaw(t, path)
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- db.InsertTokenEvent(context.Background(), basisEvent("wait")) }()
	time.Sleep(time.Second)
	release()
	if err := <-done; err != nil {
		t.Fatalf("capture after %v: %v", time.Since(start), err)
	}
	got, _, _ := pricingRow(t, db, "wait")
	if got.InputTok != 100 {
		t.Fatal(got)
	}
	t.Logf("capture persisted after %v", time.Since(start))
}

func TestPricingBasisKeepsNestedEvidence(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(fmt.Sprint(upgrade), func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			e := basisEvent("nested")
			e.TokenDerived = true
			e.CacheWrite5m = 1000
			e.CacheWriteEvidence = "nested"
			if upgrade {
				e.CacheWriteEvidence = "legacy"
			}
			e.CostMicro = ComputeCost(e.Model, usage(e))
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			if upgrade {
				e.CacheWriteEvidence = "nested"
				if err := db.InsertTokenEvent(t.Context(), e); err != nil {
					t.Fatal(err)
				}
			}
			e.OutputTok = 1
			e.CacheWriteEvidence = ""
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			before, _, _ := pricingRow(t, db, "nested")
			if before.CacheWriteEvidence != "nested" {
				t.Errorf("nested authority dropped: %+v", before)
			}
			e.CacheWriteEvidence = "nested"
			e.CacheWrite5m = 0
			e.CacheWrite1h = 1000
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			after, _, _ := pricingRow(t, db, "nested")
			if after != before {
				t.Fatalf("contradictory TTL replaced retained split: before=%+v after=%+v", before, after)
			}
			var n int
			if err := db.db.QueryRow(`SELECT COUNT(*) FROM pricing_observation_audit WHERE kind='observation_conflict'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("conflicts=%d", n)
			}
		})
	}
}

func TestRepricePricingProvenanceAndInverse(t *testing.T) {
	for _, guessed := range []bool{false, true} {
		t.Run(fmt.Sprint(guessed), func(t *testing.T) {
			db, cleanup := newTestDB(t)
			defer cleanup()
			e := basisEvent("inverse")
			e.TokenDerived = true
			if guessed {
				e.Model = "unknown-inverse-model"
				e.CostMicro, e.BillingMode = ComputeCostHost(e.Host, e.Model, usage(e))
			}
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			old, basis, id := pricingRow(t, db, e.IdempotencyKey)
			// Retain a valid old snapshot, then advance the active version without changing its rates.
			info := activePriceTableInfo
			activePriceTableInfo.Version++
			defer func() { activePriceTableInfo = info }()
			res, err := db.Reprice(t.Context(), RepriceOptions{FromVersion: old.PriceVersion, Commit: true, AllowGuessed: true})
			if err != nil {
				t.Fatal(err)
			}
			got, newBasis, newID := pricingRow(t, db, e.IdempotencyKey)
			if newBasis != basis || newID == id || got.PriceVersion != ActivePriceTableInfo().Version {
				t.Errorf("repriced basis=%s id=%s version=%d; want %s new snapshot v%d", newBasis, newID, got.PriceVersion, basis, ActivePriceTableInfo().Version)
			}
			tx, err := db.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = resolveRate(t.Context(), tx, got, newID)
			_ = tx.Rollback()
			if err != nil {
				t.Errorf("repriced snapshot does not resolve: %v", err)
			}
			exported, _, err := db.ListTokenEvents(t.Context(), e.Timestamp.Add(-time.Hour), e.Timestamp.Add(time.Hour), PageCursor{}, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(exported) != 1 || exported[0].PricingBasis != basis || exported[0].RateSnapshot != newID {
				t.Errorf("exported provenance: %+v", exported)
			}
			var oldBasis, oldID string
			if err := db.db.QueryRow(`SELECT COALESCE(old_pricing_basis,''),COALESCE(old_rate_snapshot,'') FROM reprice_row_audit WHERE reprice_id=?`, res.RepriceID).Scan(&oldBasis, &oldID); err != nil {
				t.Fatal(err)
			}
			if oldBasis != basis || oldID != id {
				t.Fatalf("before image=%s/%s want %s/%s", oldBasis, oldID, basis, id)
			}
			if _, err := db.db.Exec(`UPDATE token_events SET (cost_micro,price_version,billing_mode,pricing_basis,rate_snapshot)=(SELECT old_cost_micro,old_price_version,old_billing_mode,old_pricing_basis,old_rate_snapshot FROM reprice_row_audit WHERE reprice_id=? AND token_event_id=token_events.id)`, res.RepriceID); err != nil {
				t.Fatal(err)
			}
			restored, _, _ := pricingRow(t, db, e.IdempotencyKey)
			if restored != old {
				t.Fatalf("inverse mismatch old=%+v restored=%+v", old, restored)
			}
			e.InputTok = 200
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			promoted, b, id2 := pricingRow(t, db, e.IdempotencyKey)
			if promoted.InputTok != 200 || b != basis || id2 != id {
				t.Fatalf("inverse stopped promotion: %+v %s %s", promoted, b, id2)
			}
		})
	}
}

func TestPricingPromotionAndRepairRollback(t *testing.T) {
	for _, mode := range []string{"single", "bulk", "repair"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "atomic.db")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			for _, key := range []string{"a", "b"} {
				if err := db.InsertTokenEvent(t.Context(), basisEvent(key)); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "repair" {
				if _, err := db.db.Exec(`UPDATE token_events SET cost_micro=1`); err != nil {
					t.Fatal(err)
				}
			}
			a, _, _ := pricingRow(t, db, "a")
			b, _, _ := pricingRow(t, db, "b")
			n := pricingCount(t, db)
			blocked := b.ID
			if mode == "single" {
				blocked = a.ID
			}
			if _, err := db.db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_pricing_update BEFORE UPDATE ON token_events WHEN OLD.id=%d BEGIN SELECT RAISE(ABORT,'injected pricing failure'); END`, blocked)); err != nil {
				t.Fatal(err)
			}
			nextA, nextB := basisEvent("a"), basisEvent("b")
			nextA.InputTok = 200
			nextB.InputTok = 200
			switch mode {
			case "single":
				err = db.InsertTokenEvent(t.Context(), nextA)
			case "bulk":
				err = db.InsertTokenEvents(t.Context(), []TokenEvent{nextA, nextB})
			case "repair":
				_, err = db.RepairPricingBasis(t.Context(), a.PriceVersion, true)
			}
			if err == nil || !strings.Contains(err.Error(), "injected pricing failure") {
				t.Fatalf("injection did not fire: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			afterA, _, _ := pricingRow(t, db, "a")
			afterB, _, _ := pricingRow(t, db, "b")
			if afterA != a || afterB != b || pricingCount(t, db) != n {
				t.Fatalf("partial commit: a=%+v b=%+v audits=%d want %d", afterA, afterB, pricingCount(t, db), n)
			}
		})
	}
}

func TestPricingBasisLegacyTimestampAndStorageError(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	e := basisEvent("legacy-ts")
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE token_events SET ts='2026-10-14 08:00:00-04:00'`); err != nil {
		t.Fatal(err)
	}
	rows, err := db.CostConsistency(t.Context())
	if err != nil || len(rows) != 1 || rows[0].Status != "consistent" {
		t.Fatalf("legacy timestamp rows=%+v err=%v", rows, err)
	}
	if _, err := db.db.Exec(`DROP TABLE pricing_rate_snapshots`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CostConsistency(t.Context()); err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("storage failure became unavailable basis: %v", err)
	}
	e.InputTok = 200
	if err := db.InsertTokenEvent(t.Context(), e); err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("promotion hid storage failure: %v", err)
	}
}
