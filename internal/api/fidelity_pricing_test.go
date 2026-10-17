package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

const unknownPricingLabel = "share of tokens with an unknown pricing basis or without an exact entry in the current price table"

func loadFidelityPrices(t *testing.T, version int, extra string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := store.LoadPriceTable("../store/prices.yaml"); err != nil {
			t.Fatal(err)
		}
	})
	path := filepath.Join(t.TempDir(), "prices.yaml")
	data := fmt.Sprintf(`version: %d
effective_date: 2026-10-14
models:
  known: {input_per_m: 3, output_per_m: 15, provider: anthropic}
  "host-only@openrouter.ai": {input_per_m: 1, output_per_m: 2, provider: openai}
  self-hosted-medium: {input_per_m: 0.5, combined: true, provider: self-hosted}
  self-hosted-large: {input_per_m: 2, combined: true, provider: self-hosted}
  self-hosted-small: {input_per_m: 0.1, combined: true, provider: self-hosted}
%s`, version, extra)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadPriceTable(path); err != nil {
		t.Fatal(err)
	}
}

func fidelityPricingRow(t *testing.T, h *Handler, dev string) map[string]any {
	t.Helper()
	rec := doExport(t, h, "/api/v1/fidelity", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("fidelity: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Developers []map[string]any `json:"developers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, row := range body.Developers {
		if row["developer"] == dev {
			return row
		}
	}
	t.Fatalf("developer %q absent: %s", dev, rec.Body.String())
	return nil
}

func assertFidelityPricing(t *testing.T, row map[string]any, share float64) {
	t.Helper()
	if got, ok := row["unknown_pricing_token_share"].(float64); !ok || got != share {
		t.Errorf("unknown_pricing_token_share = %v, want number %v", row["unknown_pricing_token_share"], share)
	}
	if row["unknown_pricing_token_share_label"] != unknownPricingLabel {
		t.Errorf("share label = %v, want %q", row["unknown_pricing_token_share_label"], unknownPricingLabel)
	}
	label, present := row["cost_label"]
	if share > 0 {
		if label != "estimated cost" {
			t.Errorf("cost_label = %v, want estimated cost", label)
		}
	} else if present {
		t.Errorf("zero share must omit cost_label, got %v", label)
	}
}

func TestGetFidelity_UnknownPricingShareAtReadTime(t *testing.T) {
	loadFidelityPrices(t, 901, "")
	h, db := newTestHandler(t)
	ctx, now := context.Background(), time.Now().UTC()
	for _, e := range []store.TokenEvent{
		{Developer: "alice", Model: "known", InputTok: 100, CostMicro: 9_000_000},
		{Developer: "alice", Model: "mystery", CacheWrite1h: 300, CostMicro: 1},
	} {
		e.Source, e.Fidelity, e.Timestamp = "jsonl", "realtime", now
		if err := db.InsertTokenEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	assertFidelityPricing(t, fidelityPricingRow(t, h, "alice"), 0.75)
	before, _, err := db.ListTokenEvents(ctx, now.Add(-time.Hour), now.Add(time.Hour), store.PageCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	// A new exact entry cannot certify the historical basis. Even the formerly
	// exact row now has an older version, so both rows count as unknown.
	loadFidelityPrices(t, 902, "  mystery: {input_per_m: 3, output_per_m: 15, provider: anthropic}\n")
	row := fidelityPricingRow(t, h, "alice")
	assertFidelityPricing(t, row, 1)
	if row["unknown_model_cost_share"] != float64(0) {
		t.Errorf("existing current-lookup dollar share changed semantics: %v", row)
	}
	after, _, err := db.ListTokenEvents(ctx, now.Add(-time.Hour), now.Add(time.Hour), store.PageCursor{}, 100)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("fidelity read mutated stored rows: err=%v before=%+v after=%+v", err, before, after)
	}
	// The existing explicit audited reprice clears the basis-unknown condition;
	// the read itself needs neither a migration nor a persisted status column.
	if _, err := db.Reprice(ctx, store.RepriceOptions{FromVersion: 901, Commit: true}); err != nil {
		t.Fatal(err)
	}
	assertFidelityPricing(t, fidelityPricingRow(t, h, "alice"), 0)
}

func TestGetFidelity_UnknownPricingLegacyVersionsAndAliasMerge(t *testing.T) {
	loadFidelityPrices(t, 901, "")
	h, db := newTestHandler(t)
	ctx, now := context.Background(), time.Now().UTC()
	for _, e := range []store.TokenEvent{
		{Developer: "alice", Model: "known", InputTok: 100, PriceVersion: 901},
		{Developer: "alice@old", Model: "known", OutputTok: 100, CacheRead: 100, CacheWrite5m: 100, PriceVersion: 900},
		{Developer: "alice", Model: "mystery", InputTok: 10_000, Timestamp: now.Add(-31 * 24 * time.Hour)},
	} {
		e.Source, e.Fidelity = "jsonl", "realtime"
		if e.Timestamp.IsZero() {
			e.Timestamp = now
		}
		if err := db.InsertTokenEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpsertDeveloperAlias(ctx, "alice@old", "alice", "test:fixture"); err != nil {
		t.Fatal(err)
	}
	assertFidelityPricing(t, fidelityPricingRow(t, h, "alice"), 0.75)
}

func TestGetFidelity_UnknownPricingPersistedZeroVersion(t *testing.T) {
	loadFidelityPrices(t, 901, "")
	h, db := newTestHandler(t)
	ctx := context.Background()
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer: "legacy", Model: "known", InputTok: 100,
		Source: "jsonl", Fidelity: "realtime", Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// InsertTokenEvent stamps zero with the active version. Recreate the
	// persisted legacy sentinel directly so the read exercises version zero.
	raw := rawStore(t, db)
	if _, err := raw.ExecContext(ctx, `UPDATE token_events SET price_version = 0 WHERE developer = ?`, "legacy"); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := raw.QueryRowContext(ctx, `SELECT price_version FROM token_events WHERE developer = ?`, "legacy").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 0 {
		t.Fatalf("persisted price_version = %d, want 0", version)
	}
	assertFidelityPricing(t, fidelityPricingRow(t, h, "legacy"), 1)
}

func TestGetFidelity_UnknownPricingMixedVersionsSameGroup(t *testing.T) {
	loadFidelityPrices(t, 901, "")
	h, db := newTestHandler(t)
	for _, e := range []store.TokenEvent{
		{InputTok: 100, PriceVersion: 901},
		{OutputTok: 100, CacheRead: 100, CacheWrite5m: 100, PriceVersion: 900},
	} {
		e.Developer, e.Host, e.Model = "mixed", "openrouter.ai", "host-only"
		e.Source, e.Fidelity, e.Timestamp = "jsonl", "realtime", time.Now().UTC()
		if err := db.InsertTokenEvent(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	assertFidelityPricing(t, fidelityPricingRow(t, h, "mixed"), 0.75)
}

func TestGetFidelity_UnknownPricingHigherVersion(t *testing.T) {
	loadFidelityPrices(t, 901, "")
	h, db := newTestHandler(t)
	if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
		Developer: "override", Model: "known", InputTok: 100, PriceVersion: 902,
		Source: "jsonl", Fidelity: "realtime", Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	assertFidelityPricing(t, fidelityPricingRow(t, h, "override"), 1)
}

func TestGetFidelity_UnknownPricingJSONShapeAndLabel(t *testing.T) {
	loadFidelityPrices(t, 901, "")
	h, db := newTestHandler(t)
	for _, tc := range []struct {
		dev, host, model string
		tokens           int
		want             float64
	}{
		{"exact", "", "known", 100, 0},
		{"host-exact", "OpenRouter.AI", "host-only", 100, 0},
		{"wrong-host", "together.ai", "host-only", 100, 1},
		{"forged-host", "", "host-only@openrouter.ai", 100, 1},
		{"explicit-estimate", "", "self-hosted-medium", 100, 0},
		{"heuristic", "", "mystery-70b", 100, 1},
		{"zero-tokens", "", "mystery", 0, 0},
	} {
		if err := db.InsertTokenEvent(context.Background(), store.TokenEvent{
			Developer: tc.dev, Host: tc.host, Model: tc.model, InputTok: tc.tokens,
			CostMicro: 1_000_000, Source: "jsonl", Fidelity: "realtime", Timestamp: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		assertFidelityPricing(t, fidelityPricingRow(t, h, tc.dev), tc.want)
	}
	// Every nonzero share qualifies, including far below the older proposed 5%
	// threshold. The classification is independent of a row's dollar amount.
	for _, e := range []store.TokenEvent{
		{Developer: "tiny-share", Model: "known", InputTok: 9_999, CostMicro: 1_000_000},
		{Developer: "tiny-share", Model: "mystery", InputTok: 1},
	} {
		e.Source, e.Fidelity, e.Timestamp = "jsonl", "realtime", time.Now().UTC()
		if err := db.InsertTokenEvent(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	assertFidelityPricing(t, fidelityPricingRow(t, h, "tiny-share"), 0.0001)
	for _, mode := range []scoring.AggregationMode{scoring.AggregationTeam, scoring.AggregationDivision} {
		h.SetAggregation(mode, scoring.DefaultKAnonymity)
		if rec := doExport(t, h, "/api/v1/fidelity", nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s mode: %d, want 403", mode, rec.Code)
		}
	}
}

func TestGetFidelity_UnknownPricingStoredBasis(t *testing.T) {
	loadFidelityPrices(t, 901, "")
	h, db := newTestHandler(t)
	for _, tc := range []struct {
		name, model, source, basis string
		version                    int
		want                       float64
	}{
		{"exact", "known", "jsonl", "token_derived", 901, 0},
		{"guessed", "mystery", "jsonl", "guessed_estimate", 901, 1},
		{"old-version", "known", "jsonl", "basis_unavailable", 900, 1},
		{"aggregate-anthropic", "known", "anthropic-admin", store.PricingAggregateApprox, 901, 1},
		{"aggregate-openai", "known", "openai-usage", store.PricingAggregateApprox, 901, 1},
		{"historical-aggregate", "known", "anthropic-admin", store.PricingLegacyAggregate, 901, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := store.TokenEvent{
				Developer: tc.name, Model: tc.model, Source: tc.source,
				InputTok: 100, TokenDerived: true, PriceVersion: tc.version,
				Fidelity: "realtime", Timestamp: time.Now().UTC(),
			}
			if tc.basis == store.PricingLegacyAggregate {
				// Simulate the pre-cutover poller path on a separate poll day.
				e.TokenDerived = false
				e.Timestamp = e.Timestamp.Add(-24 * time.Hour)
			}
			if tc.basis == store.PricingAggregateApprox {
				e.PricingBasis = tc.basis
				e.CostMicro, e.BillingMode = store.ComputeAggregateCostHost(e.Host, e.Model, store.CostUsage{Input: e.InputTok})
			} else {
				e.CostMicro, e.BillingMode = store.ComputeCostHost(e.Host, e.Model, store.CostUsage{Input: e.InputTok})
			}
			if err := db.InsertTokenEvent(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			var basis string
			if err := rawStore(t, db).QueryRow(`SELECT pricing_basis FROM token_events WHERE developer=?`, tc.name).Scan(&basis); err != nil {
				t.Fatal(err)
			}
			if basis != tc.basis {
				t.Fatalf("stored basis = %q, want %q", basis, tc.basis)
			}
			assertFidelityPricing(t, fidelityPricingRow(t, h, tc.name), tc.want)
		})
	}
}

func TestGetFidelity_UnknownPricingMixedBasesSameGroup(t *testing.T) {
	loadFidelityPrices(t, 901, "")
	h, db := newTestHandler(t)
	raw := rawStore(t, db)
	for _, basis := range []any{"token_derived", "guessed_estimate", store.PricingAggregateApprox, store.PricingLegacyAggregate, "basis_unavailable", nil, "future-unknown-basis"} {
		e := store.TokenEvent{
			Developer: "mixed-bases", Model: "known", Source: "jsonl", InputTok: 100,
			TokenDerived: true, Fidelity: "realtime", Timestamp: time.Now().UTC(),
		}
		e.CostMicro = store.ComputeCost(e.Model, store.CostUsage{Input: e.InputTok})
		if err := db.InsertTokenEvent(t.Context(), e); err != nil {
			t.Fatal(err)
		}
		// Recreate persisted bases independently of current insertion policy, all
		// with the same developer/host/model/version. Only the first is known.
		if _, err := raw.Exec(`UPDATE token_events SET pricing_basis=? WHERE id=(SELECT MAX(id) FROM token_events)`, basis); err != nil {
			t.Fatal(err)
		}
	}
	assertFidelityPricing(t, fidelityPricingRow(t, h, "mixed-bases"), 6.0/7)
}
