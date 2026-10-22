package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"github.com/tiermetric/tier/internal/store"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestDoctorCostConsistencyClassificationAndDryRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e := store.TokenEvent{Developer: "alice", Model: "claude-sonnet-4", InputTok: 100, CostMicro: 300, Source: "jsonl", IdempotencyKey: "doctor", Timestamp: time.Now()}
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"cost-consistency", "--db", path}, {"cost-consistency", "--db", path, "--version", "14", "--dry-run"}} {
		var out, stderr bytes.Buffer
		if rc := runDoctor(args, &out, &stderr); rc != 0 {
			t.Fatalf("doctor rc=%d %s", rc, stderr.String())
		}
		var result struct {
			Rows    []store.CostConsistencyRow
			Applied bool
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Rows) != 1 || result.Rows[0].Status != "consistent" || result.Applied {
			t.Fatalf("doctor %s", out.String())
		}
	}
	for _, args := range [][]string{{"cost-consistency"}, {"cost-consistency", "--db", path, "--apply"}, {"cost-consistency", "--db", path, "--version", "14", "--dry-run", "--apply"}} {
		var out, stderr bytes.Buffer
		if rc := runDoctor(args, &out, &stderr); rc != 2 {
			t.Fatalf("bad flags rc %d: %s", rc, stderr.String())
		}
	}
}

func TestDoctorCostConsistencyExplicitOriginalTableDryApplyAndGross(t *testing.T) {
	path := filepath.Join(t.TempDir(), "original.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"up", "down"} {
		if err := db.InsertTokenEvent(t.Context(), store.TokenEvent{Developer: "alice", Model: "claude-sonnet-4", InputTok: 100, CostMicro: 300, Source: "jsonl", IdempotencyKey: key, Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`UPDATE token_events SET rate_snapshot=NULL,pricing_basis=NULL,cost_micro=CASE WHEN idempotency_key='up' THEN 100 ELSE 500 END`); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--dry-run", "--apply"} {
		var out, stderr bytes.Buffer
		rc := runDoctor([]string{"cost-consistency", "--db", path, "--version", strconv.Itoa(store.ActivePriceTableInfo().Version), flag, "--original-table", "../../internal/store/prices.yaml", "--original-rule", store.PricingRuleV1}, &out, &stderr)
		if rc != 0 {
			t.Fatalf("%s rc=%d %s", flag, rc, stderr.String())
		}
		var result struct {
			GrossIncreaseMicro, GrossDecreaseMicro int64
			Applied                                bool
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.GrossIncreaseMicro != 200 || result.GrossDecreaseMicro != 200 || result.Applied != (flag == "--apply") {
			t.Fatalf("gross receipt %s", out.String())
		}
		var n int
		if err := raw.QueryRow(`SELECT COUNT(*) FROM token_events WHERE cost_micro=300 AND rate_snapshot IS NOT NULL`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		want := 0
		if flag == "--apply" {
			want = 2
		}
		if n != want {
			t.Fatalf("%s changed rows=%d want %d", flag, n, want)
		}
	}
}

func TestDoctorCostConsistencyRefusesMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	var out, stderr bytes.Buffer
	if rc := runCostConsistency([]string{"--db", path}, &out, &stderr); rc != 2 {
		t.Fatalf("missing DB rc=%d out=%s stderr=%s", rc, out.String(), stderr.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("doctor created missing database: %v", err)
	}
}

func TestDoctorCostConsistencyTotalsExcludeUnrepairablePoller(t *testing.T) {
	path := filepath.Join(t.TempDir(), "poller.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e := store.TokenEvent{Developer: "alice", Model: "gpt-4o", Source: "openai-usage", InputTok: 100, TokenDerived: true, Timestamp: time.Now()}
	e.CostMicro, e.BillingMode = store.ComputeAggregateCostHost("", e.Model, store.CostUsage{Input: 100})
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`UPDATE token_events SET cost_micro=1`); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"--dry-run", "--apply"} {
		var out, stderr bytes.Buffer
		rc := runCostConsistency([]string{"--db", path, "--version", strconv.Itoa(store.ActivePriceTableInfo().Version), mode}, &out, &stderr)
		if rc != 0 {
			t.Fatalf("doctor rc=%d %s", rc, stderr.String())
		}
		var result struct {
			Rows                                   []store.CostConsistencyRow
			GrossIncreaseMicro, GrossDecreaseMicro int64
			Applied                                bool
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.GrossIncreaseMicro != 0 || result.GrossDecreaseMicro != 0 || len(result.Rows) != 1 || result.Rows[0].Class != "poller_cost_mismatch" {
			t.Fatalf("unapplied poller counted as repair: %s", out.String())
		}
	}
	var cost int64
	if err := raw.QueryRow(`SELECT cost_micro FROM token_events`).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost != 1 {
		t.Fatalf("poller repaired: %d", cost)
	}
}

func TestDoctorCostConsistencyCursorAdminExcludedFromRepairTotals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e := store.TokenEvent{Developer: "alice", Model: "claude-sonnet-4-5", Source: "cursor-admin", InputTok: 300000, CostMicro: 900000, PricingBasis: store.PricingTokenDerived, Timestamp: time.Now()}
	if err := db.InsertTokenEvent(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"", "--dry-run", "--apply"} {
		var out, stderr bytes.Buffer
		args := []string{"--db", path}
		if mode != "" {
			args = append(args, "--version", strconv.Itoa(store.ActivePriceTableInfo().Version), mode)
		}
		if rc := runCostConsistency(args, &out, &stderr); rc != 0 {
			t.Fatalf("rc=%d %s", rc, stderr.String())
		}
		var result struct {
			Rows                                   []store.CostConsistencyRow
			GrossIncreaseMicro, GrossDecreaseMicro int64
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Rows) != 1 || result.Rows[0].Class != "cursor_admin_cost_mismatch" || result.Rows[0].StoredMicro != 900000 || result.Rows[0].ExpectedMicro != 1800000 || result.GrossIncreaseMicro != 0 || result.GrossDecreaseMicro != 0 {
			t.Fatalf("cursor per-request sum counted as repair (%s): %s", mode, out.String())
		}
	}
}
