package ingester

import (
	"context"

	"github.com/tiermetric/tier/internal/collector"
)

// SourceTagger returns a collector.Ingester that stamps ev.Source = name on
// every event before forwarding it to next.
//
// Motivation (#46): each collector used to hand-set ev.Source = SourceXxx at
// its emit site — six-plus sites across the proxy parsers alone — and a v1.5
// collector that forgot the field would write a Source-less row that no read
// path could attribute to a producer. Stamping it once, at the shared sink the
// wiring already threads every collector through, removes that whole class of
// omission: the field is a property of which collector the event came from, so
// the wiring (which knows the collector) is the honest place to set it.
//
// The tag is authoritative: it OVERRIDES whatever Source the producer set,
// including an empty string. That is deliberate — the wiring's name is the
// source of truth, and a producer disagreeing with its own wiring is a bug the
// override papers over rather than propagates. Callers that still hand-set
// Source (the JSONL collector, whose Collect slice path has no Ingester to tag
// it) pass a matching name so the override is a no-op.
//
// name is normally one of the collector.Source* constants. A stamp of "" is
// pointless (it tags nothing) but harmless.
func SourceTagger(name string, next collector.Ingester) collector.Ingester {
	return &sourceTagger{name: name, next: next}
}

type sourceTagger struct {
	name string
	next collector.Ingester
}

func (t *sourceTagger) Ingest(ctx context.Context, ev collector.TokenEvent) error {
	ev.Source = t.name
	return t.next.Ingest(ctx, ev)
}
