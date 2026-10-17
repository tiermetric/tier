package openaiusage

import (
	"github.com/tiermetric/tier/internal/ingester"
	"github.com/tiermetric/tier/internal/store"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestAggregatePricingRemainderCachedInputAndUnchangedRetry(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "aggregate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	day := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	capture := store.TokenEvent{Developer: "alice", Source: "proxy", Model: "gpt-4o", InputTok: 60000, CacheRead: 10000, OutputTok: 100, Timestamp: day.Add(time.Hour), TokenDerived: true}
	capture.CostMicro = store.ComputeCost(capture.Model, store.CostUsage{Input: 60000, CacheRead: 10000, Output: 100})
	if err := db.InsertTokenEvent(t.Context(), capture); err != nil {
		t.Fatal(err)
	}
	srv := usageServer(t, usageReport{Data: []usageBucket{dayBucket("2026-06-15", modelResult("gpt-4o", 150000, 30000, 300))}})
	defer srv.Close()
	p := NewPoller(PollerConfig{Client: newTestClient(t, srv, nil), Store: db, Org: "acme", Now: func() time.Time { return fixedNow }})
	adapter := ingester.Store(db)
	if err := p.pollOnce(t.Context(), adapter); err != nil {
		t.Fatal(err)
	}
	before, _, err := db.ListTokenEvents(t.Context(), day, day.Add(48*time.Hour), store.PageCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for _, r := range before {
		if r.Source == "openai-usage" {
			n++
			if r.InputTok != 60000 || r.CacheRead != 20000 || r.OutputTok != 200 || r.PricingBasis != store.PricingAggregateApprox || r.RateSnapshot == "" || r.CostMicro != 177000 {
				t.Fatalf("stored remainder %+v", r)
			}
		}
	}
	if n != 1 {
		t.Fatalf("poller rows=%d", n)
	}
	// Late capture changes the computed remainder, but a completed poller row is immutable.
	capture.Timestamp = day.Add(2 * time.Hour)
	capture.IdempotencyKey = "late-capture"
	if err := db.InsertTokenEvent(t.Context(), capture); err != nil {
		t.Fatal(err)
	}
	if err := p.pollOnce(t.Context(), adapter); err != nil {
		t.Fatal(err)
	}
	after, _, err := db.ListTokenEvents(t.Context(), day, day.Add(48*time.Hour), store.PageCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	pollerRows := func(rows []store.TokenEvent) []store.TokenEvent {
		var result []store.TokenEvent
		for _, r := range rows {
			if r.Source == "openai-usage" {
				result = append(result, r)
			}
		}
		return result
	}
	if !reflect.DeepEqual(pollerRows(before), pollerRows(after)) {
		t.Fatal("completed poller row changed on retry")
	}
}
