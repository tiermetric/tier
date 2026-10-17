package api

import (
	"github.com/tiermetric/tier/internal/store"
	"testing"
	"time"
)

func TestEventsCacheWriteProvenanceValidationAndPromotion(t *testing.T) {
	h, db := newTestHandler(t)
	e := validEvent("provenance")
	e["cache_write_evidence"] = "legacy"
	e["cache_write_5m_tokens"] = 1000
	if code, body := postEvents(t, h, []map[string]any{e}); code != 201 {
		t.Fatalf("legacy %d %s", code, body)
	}
	e["cache_write_evidence"] = "nested"
	e["cache_write_5m_tokens"] = 0
	e["cache_write_1h_tokens"] = 1000
	e["output_tokens"] = 600
	if code, body := postEvents(t, h, []map[string]any{e}); code != 201 {
		t.Fatalf("nested %d %s", code, body)
	}
	rows, _, err := db.ListTokenEvents(t.Context(), time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), store.PageCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].CacheWriteEvidence != "nested" || rows[0].CacheWrite5m != 0 || rows[0].CacheWrite1h != 1000 || rows[0].CostMicro != 18000 || rows[0].RateSnapshot == "" {
		t.Fatalf("provenance lost at central ingest: %+v", rows)
	}
	for _, proof := range []string{"legacy", "invented"} {
		e["idempotency_key"] = proof
		e["cache_write_evidence"] = proof
		if code, body := postEvents(t, h, []map[string]any{e}); code != 400 {
			t.Errorf("invalid %s accepted: %d %s", proof, code, body)
		}
	}
	old := validEvent("old-agent")
	if code, body := postEvents(t, h, []map[string]any{old}); code != 201 {
		t.Fatalf("old agent incompatible %d %s", code, body)
	}
}
