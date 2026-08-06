// This file is package store_test, not store, on purpose: it imports
// internal/collector to read the real source enum, and collector imports store,
// so an internal test file here would be an import cycle. Everything it needs is
// exported, so nothing is lost.
package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"

	_ "modernc.org/sqlite"
)

// repriceableBySource is the EXPLICIT classification of every token-event source:
// true = its cost_micro is derived by the server from token counts, so a reprice
// may legitimately re-derive it; false = its cost is caller-authoritative and
// re-deriving it would destroy the figure.
//
// This map is the point of the test. repriceExcludedWhereSQL is an open DENYLIST
// (`source = 'api'`), which fails DESTRUCTIVE: a future source whose cost is
// caller-authoritative would be repriced — and, if its rows carry no token
// counts, zeroed — purely by omission. That shape is right for reprice, whose
// default must stay "reprice it", but it needs a tripwire, and this is it.
//
// TestRepriceClassifiesEverySource cross-checks this map against
// collector.AllSources(), so ADDING A SOURCE CONSTANT FAILS THIS TEST until
// someone states which side it is on. That is the whole mechanism: the omission
// becomes loud instead of silent.
var repriceableBySource = map[string]bool{
	// Server-derived: every one of these prices raw token counts with the
	// server's own table (the collectors via ComputeCost/ComputeCostHost, the
	// org pollers likewise, and POST /api/v1/events re-prices what a shipper
	// posts rather than trusting its cost_usd, #233).
	collector.SourceJSONL:          true,
	collector.SourceProxy:          true,
	collector.SourceCodexRollout:   true,
	collector.SourceAnthropicAdmin: true,
	collector.SourceOpenAIUsage:    true,
	// Declared constant with no producer in the tree yet. Classified in advance
	// precisely so it cannot land on the destructive side by default: whatever
	// eventually writes it must revisit this line if it imports a
	// provider-reported dollar figure rather than deriving cost from tokens.
	collector.SourceCopilotAPI: true,
	// The one caller-authoritative source: POST /api/v1/costs carries a finance
	// figure (an invoice number), and its token counts are optional.
	"api": false,
}

// TestRepriceClassifiesEverySource is the compensating control for the denylist.
// It fails in TWO ways, and both matter:
//
//  1. A new collector.Source* constant that nobody classified — the map lookup
//     misses and the test says so by name.
//  2. A classification that no longer matches what Reprice actually does — each
//     source is exercised against a real DB and the behaviour is compared to the
//     map, so the map cannot rot into a comment that merely claims things.
func TestRepriceClassifiesEverySource(t *testing.T) {
	known := append(collector.AllSources(), "api")
	for _, src := range known {
		want, classified := repriceableBySource[src]
		if !classified {
			t.Errorf("source %q is in collector.AllSources() but is NOT classified in "+
				"repriceableBySource — decide whether a reprice may re-derive its cost. "+
				"If its cost_micro is server-derived from token counts, it is repriceable "+
				"(true); if it carries a caller-authoritative figure, it MUST be added to "+
				"store.repriceExcludedWhereSQL and classified false here, or a reprice "+
				"sweep will silently rewrite it.", src)
			continue
		}
		t.Run(src, func(t *testing.T) {
			assertRepriceable(t, src, want)
		})
	}

	// The reverse direction: a classification for a source that no longer
	// exists is dead weight that would outlive its subject.
	for src := range repriceableBySource {
		if src == "api" {
			continue // not a collector source; has no constant
		}
		found := false
		for _, s := range collector.AllSources() {
			if s == src {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("repriceableBySource classifies %q, which is no longer in "+
				"collector.AllSources() — remove the stale entry", src)
		}
	}
}

// assertRepriceable seeds one deliberately-mispriced row under src and checks
// whether a committed reprice re-derives it. Token counts are NON-ZERO so the
// zero-token backstop cannot be what decides the outcome — this isolates the
// source filter, which is the thing under test.
func assertRepriceable(t *testing.T, src string, want bool) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "src.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	const wrongCost = 42_000_000
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer: "alice", IssueID: "issue-" + src, Model: "claude-sonnet-4",
		InputTok: 1000, OutputTok: 500, CostMicro: wrongCost,
		Source: src, Fidelity: "estimated", IdempotencyKey: "key-" + src,
		PriceVersion: 1, Timestamp: time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed %s row: %v", src, err)
	}

	res, err := db.Reprice(ctx, store.RepriceOptions{FromVersion: 1, Commit: true})
	if err != nil {
		t.Fatalf("Reprice: %v", err)
	}

	events, _, err := db.ListTokenEvents(ctx,
		time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC), store.PageCursor{}, 10)
	if err != nil {
		t.Fatalf("ListTokenEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 row, got %d", len(events))
	}
	moved := events[0].CostMicro != wrongCost

	switch {
	case want && !moved:
		t.Errorf("source %q is classified REPRICEABLE but the reprice left its cost at %d — "+
			"either the classification is wrong or repriceExcludedWhereSQL over-excludes",
			src, events[0].CostMicro)
	case !want && moved:
		t.Errorf("source %q is classified CALLER-AUTHORITATIVE but the reprice rewrote its cost "+
			"%d -> %d — repriceExcludedWhereSQL does not cover it, and a sweep destroys real money",
			src, wrongCost, events[0].CostMicro)
	}
	if wantExcluded := int64(0); want && res.ExcludedRowCount != wantExcluded {
		t.Errorf("source %q: ExcludedRowCount = %d, want %d", src, res.ExcludedRowCount, wantExcluded)
	}
	if wantExcluded := int64(1); !want && res.ExcludedRowCount != wantExcluded {
		t.Errorf("source %q: ExcludedRowCount = %d, want %d", src, res.ExcludedRowCount, wantExcluded)
	}
}
