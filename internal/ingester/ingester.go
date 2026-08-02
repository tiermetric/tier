// Package ingester wires collector.Ingester (the abstract sink shared by
// every token-event collector) to a concrete backing store.
//
// Lives in its own package as a deliberate dependency-hygiene choice: the
// store package stays free of any knowledge of TokenEvent collectors, and
// the collector package can satisfy its own interface in tests without
// pulling in SQLite. The two could be merged (today, store doesn't import
// collector, so the adapter could move into store), but keeping it
// separate means a future change that makes store import collector — e.g.
// to bake the source allow-list into the schema — won't have to refactor
// the adapter's location.
package ingester

import (
	"context"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"
)

// StoreInserter is the subset of store.DB methods the adapter calls. Defined
// as an interface so tests can substitute a recording mock without spinning
// up a SQLite file.
type StoreInserter interface {
	InsertTokenEvent(ctx context.Context, ev store.TokenEvent) error
}

// Store returns a collector.Ingester backed by a StoreInserter. The returned
// value is a small struct (one pointer field) so passing it by value is
// fine; constructing a fresh one per request is cheap.
func Store(s StoreInserter) collector.Ingester {
	return &storeAdapter{s: s}
}

type storeAdapter struct {
	s StoreInserter
}

// Ingest forwards each event to InsertTokenEvent after a field-by-field
// copy from collector.TokenEvent to store.TokenEvent. The conversion is
// explicit rather than a type alias because the two structs may diverge as
// v1.5 admin-API collectors carry metadata the schema doesn't store
// (e.g. raw billing line numbers, contract id).
func (a *storeAdapter) Ingest(ctx context.Context, ev collector.TokenEvent) error {
	return a.s.InsertTokenEvent(ctx, store.TokenEvent{
		Developer:      ev.Developer,
		IssueID:        ev.IssueID,
		Model:          ev.Model,
		InputTok:       ev.InputTok,
		OutputTok:      ev.OutputTok,
		CacheRead:      ev.CacheRead,
		CacheWrite5m:   ev.CacheWrite5m,
		CacheWrite1h:   ev.CacheWrite1h,
		CostMicro:      ev.CostMicro,
		Source:         ev.Source,
		Fidelity:       ev.Fidelity,
		IdempotencyKey: ev.IdempotencyKey,
		Repo:           ev.Repo,
		SessionID:      ev.SessionID,
		Host:           ev.Host,
		BillingMode:    ev.BillingMode,
		Timestamp:      ev.Timestamp,
	})
}

// EventRecorder is the one-method hook RecordingIngester calls after each
// event that successfully lands. *health.WatcherState satisfies it (RecordEvent
// stamps last_event_ts). It is declared here as a local one-method interface
// so the data-plane ingester does not have to import package health — the
// dependency points the harmless way (cmd/tierd knows both), and tests can
// supply a trivial counter.
type EventRecorder interface {
	RecordEvent()
}

// RecordingIngester wraps a collector.Ingester so every event the watcher
// successfully ingests also stamps rec.RecordEvent(). This is what wires
// health.WatcherState.RecordEvent into the live watcher path (#50): without it,
// last_event_ts in /healthz would stay zero because nothing calls RecordEvent.
//
// As of #46 the watcher ingests through the shared collector.Ingester seam (the
// same one JSONL and the proxy use), so the stamping decorator now sits on that
// seam rather than the old InsertTokenEvent-only WatcherStore it wrapped before.
// Checkpoint persistence (#71) is a separate concern the watcher reaches via its
// own CheckpointStore field and never flows through here — RecordEvent stamps
// event arrivals, not checkpoint writes, exactly as before.
//
// Recording happens only after a successful ingest: an event that failed to
// persist never "arrived", so stamping it would make /healthz lie about
// last_event_ts. An idempotent re-insert that the store absorbs (the MAX-on-
// conflict UPSERT returns nil) DOES stamp — last_event_ts is a freshness
// signal ("the watcher wrote to the store recently"), not a unique-event
// counter. A nil rec returns next unwrapped, so callers without a watcher
// state pay nothing.
func RecordingIngester(rec EventRecorder, next collector.Ingester) collector.Ingester {
	if rec == nil {
		return next
	}
	return &recordingIngester{rec: rec, next: next}
}

type recordingIngester struct {
	rec  EventRecorder
	next collector.Ingester
}

func (r *recordingIngester) Ingest(ctx context.Context, ev collector.TokenEvent) error {
	if err := r.next.Ingest(ctx, ev); err != nil {
		return err
	}
	r.rec.RecordEvent()
	return nil
}
