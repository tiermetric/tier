package anthropicadmin

import (
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/store"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestAggregatePricingTwoSmallRequestsPositiveControl(t *testing.T) {
	p := &Poller{store: newFakeStore(), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	model := "claude-haiku-5-5"
	events, _, _, err := p.buildRemainderEvents(t.Context(), []usageBucket{dayBucket("2026-06-15", modelResult(model, 120000, 0, 0, 0, 0))}, fixedNow)
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	want := 2 * store.ComputeCost(model, store.CostUsage{Input: 60000})
	if events[0].CostMicro != want {
		t.Fatalf("daily aggregate=%d, known 2x60K request total=%d, overstatement=%d", events[0].CostMicro, want, events[0].CostMicro-want)
	}
}

// Real store + ingester, not a producer-only assertion: subtract every captured
// class first, then price the remainder without treating the day as one request.
func TestAggregatePricingRemainderCacheClassesAndUnchangedRetry(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "aggregate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	model := "claude-haiku-5-5"
	day := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	capture := store.TokenEvent{Developer: "alice", Model: model, InputTok: 60000, OutputTok: 100, CacheRead: 10000, CacheWrite5m: 10000, CacheWrite1h: 10000, Timestamp: day.Add(time.Hour), Source: "jsonl", TokenDerived: true, CacheWriteEvidence: "nested"}
	capture.CostMicro = store.ComputeCost(model, store.CostUsage{Input: 60000, Output: 100, CacheRead: 10000, CacheWrite5m: 10000, CacheWrite1h: 10000})
	if err := db.InsertTokenEvent(t.Context(), capture); err != nil {
		t.Fatal(err)
	}
	p := &Poller{store: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	buckets := []usageBucket{dayBucket("2026-06-15", modelResult(model, 120000, 300, 30000, 30000, 30000))}
	events, _, _, err := p.buildRemainderEvents(t.Context(), buckets, fixedNow)
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	e := events[0]
	if e.InputTok != 60000 || e.OutputTok != 200 || e.CacheRead != 20000 || e.CacheWrite5m != 20000 || e.CacheWrite1h != 20000 || e.PricingBasis != store.PricingAggregateApprox || e.CostMicro != 12800 {
		t.Fatalf("remainder %+v", e)
	}
	adapter := ingester.Store(db)
	if err := adapter.Ingest(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	before, _, err := db.ListTokenEvents(t.Context(), day, day.Add(48*time.Hour), store.PageCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range before {
		if row.Source == "anthropic-admin" && (row.PricingBasis != store.PricingAggregateApprox || row.RateSnapshot == "") {
			t.Fatalf("stored basis %+v", row)
		}
	}
	retry, _, _, err := p.buildRemainderEvents(t.Context(), buckets, fixedNow)
	if err != nil || len(retry) != 0 {
		t.Fatalf("completed repoll emitted %+v %v", retry, err)
	}
	// Direct duplicate ingestion remains frozen even with a changed provider total.
	e.InputTok *= 2
	e.CostMicro *= 2
	if err := adapter.Ingest(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	after, _, err := db.ListTokenEvents(t.Context(), day, day.Add(48*time.Hour), store.PageCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("retry mutated completed rows")
	}
}
