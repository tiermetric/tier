package openaiusage

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// Seed the remainder computed by the old model-only, all-billing-mode rule,
// then re-poll through the real store after the baseline cutover.
func TestPollerBaselineNewDaysOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "poller.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	day := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	model := "gpt-4o"
	for _, offset := range []time.Duration{0, 24 * time.Hour} {
		for _, capturedModel := range []string{model, "gpt-4.1"} {
			for _, capture := range []struct{ host, mode string }{
				{"api." + provider + ".com", store.BillingSubscription},
				{"openrouter.ai", store.BillingPerToken},
			} {
				if err := db.InsertTokenEvent(ctx, store.TokenEvent{
					Developer: "alice", IssueID: "1", Model: capturedModel, InputTok: 100, OutputTok: 20,
					CacheRead: 10, CacheWrite5m: 5, CacheWrite1h: 3, CostMicro: 1,
					Source: "proxy", Host: capture.host, BillingMode: capture.mode,
					Timestamp: day.Add(offset + time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	old := newFakeStore()
	old.captured["2026-06-15"] = map[string]store.CostUsage{model: {Input: 200, Output: 40, CacheRead: 20, CacheWrite5m: 10, CacheWrite1h: 6}}
	p := &Poller{store: old, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	bucket := dayBucket("2026-06-15", modelResult("gpt-4o", 1000, 100, 200))
	events, _, _, err := p.buildRemainderEvents(ctx, []usageBucket{bucket}, fixedNow)
	if err != nil || len(events) != 1 {
		t.Fatalf("old-rule events = %v, err %v", events, err)
	}
	ev := events[0]
	// Raw insert models an older binary: no new provenance stamp, next-day ts.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	_, err = raw.ExecContext(ctx, `INSERT INTO token_events
  (developer, issue_id, model, input_tok, output_tok, cache_read, cache_write_5m, cache_write_1h,
   cost_micro, source, fidelity, idempotency_key, ts)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.Developer, ev.IssueID, ev.Model, ev.InputTok, ev.OutputTok, ev.CacheRead,
		ev.CacheWrite5m, ev.CacheWrite1h, ev.CostMicro, ev.Source, ev.Fidelity, ev.IdempotencyKey, day.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := db.ListTokenEvents(ctx, day, day.Add(48*time.Hour), store.PageCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	p.store = db
	// Freeze the existing model while recovering a missing model with the
	// legacy day's model-only, all-billing-mode subtraction rule.
	bucket.Results = append(bucket.Results, modelResult("gpt-4.1", 500, 100, 100))
	events, _, _, err = p.buildRemainderEvents(ctx, []usageBucket{bucket}, fixedNow)
	if err != nil || len(events) != 1 {
		t.Fatalf("late-model events = %v, err %v; want one missing model", events, err)
	}
	late := events[0]
	usage := store.CostUsage{Input: late.InputTok, Output: late.OutputTok, CacheRead: late.CacheRead,
		CacheWrite5m: late.CacheWrite5m, CacheWrite1h: late.CacheWrite1h}
	want := store.CostUsage{Input: 200, Output: 60, CacheRead: 80}
	if late.Model != "gpt-4.1" || usage != want {
		t.Fatalf("late-model remainder = %s %+v, want gpt-4.1 %+v", late.Model, usage, want)
	}
	cost, _ := store.ComputeCostHost("", late.Model, want)
	if late.CostMicro != cost {
		t.Fatalf("late-model cost = %d, want legacy remainder cost %d", late.CostMicro, cost)
	}
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer: late.Developer, IssueID: late.IssueID, Model: late.Model,
		InputTok: late.InputTok, OutputTok: late.OutputTok, CacheRead: late.CacheRead,
		CacheWrite5m: late.CacheWrite5m, CacheWrite1h: late.CacheWrite1h, CostMicro: late.CostMicro,
		Source: late.Source, Fidelity: late.Fidelity, IdempotencyKey: late.IdempotencyKey,
		BillingMode: late.BillingMode, Timestamp: late.Timestamp,
	}); err != nil {
		t.Fatal(err)
	}
	after, _, err := db.ListTokenEvents(ctx, day, day.Add(48*time.Hour), store.PageCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("stored rows = %d, want %d after late-model recovery", len(after), len(before)+1)
	}
	byID := make(map[int64]store.TokenEvent)
	for _, row := range after {
		byID[row.ID] = row
	}
	for _, row := range before {
		if !reflect.DeepEqual(row, byID[row.ID]) {
			t.Errorf("re-poll changed stored row: before %+v, after %+v", row, byID[row.ID])
		}
	}
	markers, err := db.PollerModelsByDay(ctx, day, collector.SourceOpenAIUsage)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(markers, map[string]string{model: "", "gpt-4.1": ""}) {
		t.Errorf("legacy-day markers = %v, want NULL for both models", markers)
	}
	events, _, _, err = p.buildRemainderEvents(ctx, []usageBucket{bucket}, fixedNow)
	if err != nil || len(events) != 0 {
		t.Fatalf("complete legacy day emitted %d events, err %v", len(events), err)
	}
	// A new day uses the corrected baseline: neither capture belongs to this API invoice.
	fresh := dayBucket("2026-06-16", modelResult("gpt-4o", 1000, 100, 200))
	events, _, _, err = p.buildRemainderEvents(ctx, []usageBucket{fresh}, fixedNow)
	if err != nil || len(events) != 1 {
		t.Fatalf("new-day events = %v, err %v", events, err)
	}
	if events[0].InputTok != 900 || events[0].OutputTok != 200 {
		t.Errorf("new-day remainder = %+v, want full 900/200 uncached aggregate", events[0])
	}
	if events[0].Source != collector.SourceOpenAIUsage {
		t.Fatal("wrong poller source")
	}
}
