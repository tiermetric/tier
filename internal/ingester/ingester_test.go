package ingester

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// recStore records every InsertTokenEvent call so the test can assert the
// per-field copy. Mirrors the StoreInserter shape used by the real *store.DB.
type recStore struct {
	events []store.TokenEvent
	fail   error
}

func (r *recStore) InsertTokenEvent(_ context.Context, ev store.TokenEvent) error {
	if r.fail != nil {
		return r.fail
	}
	r.events = append(r.events, ev)
	return nil
}

// recIngester records every Ingest call. Stands in for the wrapped
// collector.Ingester the RecordingIngester decorator forwards to.
type recIngester struct {
	events []collector.TokenEvent
	fail   error
}

func (r *recIngester) Ingest(_ context.Context, ev collector.TokenEvent) error {
	if r.fail != nil {
		return r.fail
	}
	r.events = append(r.events, ev)
	return nil
}

// TestStore_ForwardsAllFields confirms the adapter copies every field from
// collector.TokenEvent to store.TokenEvent. The two structs are deliberately
// distinct (see ingester.go doc comment) and a missed field would silently
// drop data — exactly the kind of refactor hazard #27 is supposed to prevent.
// Host/BillingMode (#300) are in this set precisely because #46 routes the
// proxy — the only producer that sets them — through this adapter, so a missing
// copy here would drop open-weights pricing provenance.
func TestStore_ForwardsAllFields(t *testing.T) {
	rec := &recStore{}
	ing := Store(rec)
	now := time.Now().UTC().Truncate(time.Second)

	ev := collector.TokenEvent{
		Developer:      "alice",
		IssueID:        "issue-42",
		Model:          "claude-sonnet-4",
		InputTok:       100,
		OutputTok:      50,
		CacheRead:      10,
		CacheWrite5m:   3,
		CacheWrite1h:   2,
		CostMicro:      10_500, // $0.0105
		Source:         "jsonl",
		Fidelity:       "realtime",
		IdempotencyKey: "deadbeef-key",
		Repo:           "tiermetric/tier",
		SessionID:      "sess-forward",
		Host:           "openrouter.ai",
		BillingMode:    "self_hosted_amortized",
		Timestamp:      now,
	}
	if err := ing.Ingest(context.Background(), ev); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(rec.events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.events))
	}
	got := rec.events[0]
	if got.Developer != ev.Developer || got.IssueID != ev.IssueID ||
		got.Model != ev.Model || got.InputTok != ev.InputTok ||
		got.OutputTok != ev.OutputTok || got.CacheRead != ev.CacheRead ||
		got.CacheWrite5m != ev.CacheWrite5m || got.CacheWrite1h != ev.CacheWrite1h ||
		got.CostMicro != ev.CostMicro ||
		got.Source != ev.Source || got.Fidelity != ev.Fidelity ||
		got.IdempotencyKey != ev.IdempotencyKey || got.Repo != ev.Repo ||
		got.SessionID != ev.SessionID ||
		got.Host != ev.Host || got.BillingMode != ev.BillingMode ||
		!got.Timestamp.Equal(ev.Timestamp) {
		t.Errorf("field-by-field copy diverged:\n got = %+v\nwant = %+v", got, ev)
	}
}

// TestStore_PropagatesError ensures the calling collector sees the underlying
// store error verbatim (no wrapping that would obscure errors.Is checks
// against sql sentinels).
func TestStore_PropagatesError(t *testing.T) {
	sentinel := errors.New("store: synthetic failure")
	rec := &recStore{fail: sentinel}
	ing := Store(rec)
	err := ing.Ingest(context.Background(), collector.TokenEvent{Developer: "x"})
	if !errors.Is(err, sentinel) {
		t.Errorf("error not propagated; got %v, want %v", err, sentinel)
	}
}

// countingRecorder is a minimal EventRecorder; it counts RecordEvent calls so
// the decorator tests can assert stamping happened exactly when it should.
type countingRecorder struct{ n int }

func (c *countingRecorder) RecordEvent() { c.n++ }

// TestRecordingIngester_StampsOnSuccess is the #50 acceptance test, now on the
// #46 seam: an event flowing through the decorator both reaches the wrapped
// collector.Ingester AND stamps the recorder (wiring last_event_ts to the live
// watcher path).
func TestRecordingIngester_StampsOnSuccess(t *testing.T) {
	rec := &recIngester{}
	cr := &countingRecorder{}
	ing := RecordingIngester(cr, rec)

	if err := ing.Ingest(context.Background(), collector.TokenEvent{Developer: "alice"}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(rec.events) != 1 {
		t.Fatalf("wrapped ingester got %d events, want 1", len(rec.events))
	}
	if cr.n != 1 {
		t.Errorf("RecordEvent called %d times, want 1", cr.n)
	}
}

// TestRecordingIngester_NoStampOnError pins the load-bearing ordering: a failed
// ingest must NOT stamp the recorder, otherwise /healthz last_event_ts would
// claim an event arrived when it never persisted.
func TestRecordingIngester_NoStampOnError(t *testing.T) {
	sentinel := errors.New("store: synthetic failure")
	rec := &recIngester{fail: sentinel}
	cr := &countingRecorder{}
	ing := RecordingIngester(cr, rec)

	err := ing.Ingest(context.Background(), collector.TokenEvent{Developer: "x"})
	if !errors.Is(err, sentinel) {
		t.Errorf("error not propagated; got %v, want %v", err, sentinel)
	}
	if cr.n != 0 {
		t.Errorf("RecordEvent called %d times on ingest failure, want 0", cr.n)
	}
}

// TestRecordingIngester_NilRecorderPassthrough confirms a nil recorder returns
// the wrapped ingester unchanged (no allocation, no panic) — the path cmd/tierd
// would hit if it ever wired the decorator without a watcher state.
func TestRecordingIngester_NilRecorderPassthrough(t *testing.T) {
	rec := &recIngester{}
	ing := RecordingIngester(nil, rec)
	if ing != collector.Ingester(rec) {
		t.Fatalf("nil recorder should return the wrapped ingester unwrapped")
	}
	if err := ing.Ingest(context.Background(), collector.TokenEvent{Developer: "x"}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(rec.events) != 1 {
		t.Errorf("wrapped ingester got %d events, want 1", len(rec.events))
	}
}

// TestSourceTagger_OverridesProducerSource is the #46 acceptance for the
// decorator: the wiring's name wins over whatever Source the producer set —
// including a wrong one — so a v1.5 collector cannot mislabel or forget its
// source once the tagger is in the chain.
func TestSourceTagger_OverridesProducerSource(t *testing.T) {
	rec := &recIngester{}
	ing := SourceTagger(collector.SourceProxy, rec)

	// Producer sets a DIFFERENT (wrong) source; the tagger must overwrite it.
	if err := ing.Ingest(context.Background(), collector.TokenEvent{Source: "totally-wrong"}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	// And an empty source must be filled, not left blank.
	if err := ing.Ingest(context.Background(), collector.TokenEvent{Source: ""}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(rec.events) != 2 {
		t.Fatalf("got %d events, want 2", len(rec.events))
	}
	for i, got := range rec.events {
		if got.Source != collector.SourceProxy {
			t.Errorf("event %d: Source = %q, want %q", i, got.Source, collector.SourceProxy)
		}
	}
}

// TestSourceTagger_DoesNotMutateCaller confirms the decorator stamps a copy: the
// event value the caller still holds is unchanged after Ingest, because Ingest
// takes TokenEvent by value. Guards against a future change to a pointer sink
// that would let the stamp leak back into the producer's own event.
func TestSourceTagger_DoesNotMutateCaller(t *testing.T) {
	rec := &recIngester{}
	ing := SourceTagger(collector.SourceProxy, rec)
	ev := collector.TokenEvent{Source: "original"}
	if err := ing.Ingest(context.Background(), ev); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if ev.Source != "original" {
		t.Errorf("caller's event mutated: Source = %q, want %q", ev.Source, "original")
	}
}
