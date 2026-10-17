package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPricingBasisOriginalTableRecoveryDryApplyExactVersionGrossAndProtections(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	for i, key := range []string{"up", "down", "sealed", "poller", "manual", "other-version", "missing-model", "unproven-cache"} {
		e := basisEvent(key)
		e.TokenDerived = true
		e.Timestamp = time.Date(2026, 10, 14, 12, i, 0, 0, time.UTC)
		if key == "sealed" {
			e.Timestamp = time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
		}
		if key == "poller" {
			e.Source = "anthropic-admin"
			e.PricingBasis = PricingAggregateApprox
		}
		if key == "manual" {
			e.Source = "api"
		}
		if key == "unproven-cache" {
			e.CacheWrite5m = 1000
		}
		if err := db.InsertTokenEvent(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.Exec(`INSERT INTO sealed_report(level,period_size,period_start,period_end,k,config_digest,sealed_at,body,body_digest,tool_version,tool_commit) VALUES ('team','month','2026-06-01T00:00:00Z','2026-07-01T00:00:00Z',2,'x','2026-07-02T00:00:00Z','{}','x','dev','x');UPDATE token_events SET pricing_basis=NULL,rate_snapshot=NULL,cost_micro=100 WHERE idempotency_key IN ('up','sealed','unproven-cache');UPDATE token_events SET pricing_basis=NULL,rate_snapshot=NULL,cost_micro=500 WHERE idempotency_key='down';UPDATE token_events SET pricing_basis=NULL,rate_snapshot=NULL,price_version=price_version-1 WHERE idempotency_key='other-version';UPDATE token_events SET pricing_basis=NULL,rate_snapshot=NULL,model='missing-model' WHERE idempotency_key='missing-model';DELETE FROM pricing_observation_audit`); err != nil {
		t.Fatal(err)
	}
	evidence := OriginalBasisEvidence{Table: defaultPriceTableYAML, Rule: PricingRuleV1}
	version := ActivePriceTableInfo().Version
	old, _, _ := pricingRow(t, db, "up")
	rows, err := db.RepairPricingBasis(t.Context(), version, false, evidence)
	if err != nil {
		t.Fatal(err)
	}
	var inc, dec int64
	for _, r := range rows {
		if r.Status == "inconsistent" && r.Class != "sealed_cost_mismatch" {
			delta := r.ExpectedMicro - r.StoredMicro
			if delta > 0 {
				inc += delta
			} else {
				dec -= delta
			}
		}
	}
	if inc != 200 || dec != 200 {
		t.Fatalf("separate gross changes inc=%d dec=%d %+v", inc, dec, rows)
	}
	after, b, id := pricingRow(t, db, "up")
	if after != old || b != "" || id != "" || pricingCount(t, db) != 0 {
		t.Fatal("dry-run persisted recovery or audit")
	}
	if _, err := db.RepairPricingBasis(t.Context(), version, true, evidence); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"up", "down"} {
		e, b, id := pricingRow(t, db, key)
		if e.CostMicro != 300 || b != "token_derived" || id == "" {
			t.Fatalf("recovered %s %+v %s", key, e, b)
		}
	}
	for _, key := range []string{"sealed", "other-version", "missing-model"} {
		_, _, id := pricingRow(t, db, key)
		if id != "" {
			t.Fatalf("protected or unavailable %s acquired snapshot", key)
		}
	}
	e, _, _ := pricingRow(t, db, "unproven-cache")
	if e.CostMicro != 100 {
		t.Fatal("unknown cache contributor repaired by guessing")
	}
	bad := evidence
	bad.Rule = "today"
	if _, err := db.RepairPricingBasis(t.Context(), version, false, bad); err == nil {
		t.Fatal("unknown rule accepted")
	}
	if _, err := db.RepairPricingBasis(t.Context(), version-1, false, evidence); err == nil {
		t.Fatal("version floor used instead of exact version")
	}
	if _, err := db.db.Exec(`UPDATE price_table_registry SET forgotten_at='2026-10-14',forgotten_by='test';INSERT INTO price_table_registry(version,table_hash,file_hash,effective_date,model_count,source,tool_version) VALUES (?, 'other','other','2026-10-14',1,'embedded','test')`, version); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RepairPricingBasis(t.Context(), version, false, evidence); err == nil {
		t.Fatal("ambiguous registry accepted")
	}
}

func TestPricingBasisExistingMismatchNeedsExplicitRepairBeforePromotion(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	e := basisEvent("mismatch")
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE token_events SET cost_micro=1`); err != nil {
		t.Fatal(err)
	}
	e.InputTok = 200
	e.CostMicro = 600
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	got, _, _ := pricingRow(t, db, e.IdempotencyKey)
	if got.InputTok != 100 || got.CostMicro != 1 {
		t.Fatal("existing mismatch implicitly repaired")
	}
	if _, err := db.RepairPricingBasis(t.Context(), ActivePriceTableInfo().Version, true); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	got, _, _ = pricingRow(t, db, e.IdempotencyKey)
	if got.InputTok != 200 || got.CostMicro != 600 {
		t.Fatalf("promotion after explicit repair %+v", got)
	}
}

func TestPricingBasisOriginalTableCannotForgeServingHostInModel(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	e := basisEvent("model-host-forgery")
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE token_events SET rate_snapshot=NULL,pricing_basis=NULL,model='meta-llama/llama-3.3-70b-instruct@openrouter.ai'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RepairPricingBasis(t.Context(), ActivePriceTableInfo().Version, true, OriginalBasisEvidence{Table: defaultPriceTableYAML, Rule: PricingRuleV1}); err != nil {
		t.Fatal(err)
	}
	got, _, id := pricingRow(t, db, e.IdempotencyKey)
	if id != "" || got.CostMicro != e.CostMicro {
		t.Fatalf("upstream model forged a serving-host rate: %+v snapshot=%s", got, id)
	}
}

func TestOpenPricingBasisDoesNotRepriceUnavailableHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, source := range []string{"jsonl", "proxy", "anthropic-admin", "openai-usage"} {
		e := basisEvent(source)
		e.Source = source
		e.TokenDerived = true
		if err := db.InsertTokenEvent(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.Exec(`UPDATE token_events SET cost_micro=1,price_version=10,pricing_basis=NULL,rate_snapshot=NULL;DELETE FROM tier_migrations WHERE name=?`, migrationRecomputeCacheTTL); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	var changed int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM token_events WHERE cost_micro<>1`).Scan(&changed); err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Fatalf("Open silently repriced %d unavailable historical rows using today's table", changed)
	}
	var policies int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM tier_migrations WHERE name IN (?,?,?)`, OriginalBasisHistoryPolicy, migrationRecomputeCacheTTL, migrationBackfillPriceVersion).Scan(&policies); err != nil || policies != 3 {
		t.Fatalf("retired migration policy markers=%d err=%v", policies, err)
	}
}
