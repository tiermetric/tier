package main

import (
	"io"
	"log"
	"strings"
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// TestUnknownModelCounter_GuessPathWiring pins the store->metrics seam for #326
// end-to-end against the REAL counter wired in runServe (main.go:
// SetUnknownModelRecorder(srvMetrics.unknownModels)). The store-level test uses
// an arity-agnostic fake, so it cannot catch the one failure that actually takes
// down tierd: metrics.CounterVec.Add panics via mustKey if the label-value count
// disagrees with the counter's declared label set. This test installs the
// concrete *metrics.CounterVec, drives ComputeCost down BOTH guess branches, and
// asserts the rendered exposition — so it simultaneously pins (a) the arity
// contract (no panic on the first production fallback), (b) the rendered label
// name guess_path, and (c) the two bounded values from store.GuessPath*.
func TestUnknownModelCounter_GuessPathWiring(t *testing.T) {
	// warnUnknownModel/warnHeuristicModel log to log.Default(); silence it so the
	// two expected WARNs don't clutter test output. Restored on cleanup.
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	sm := newServeMetrics("v1.2.3")
	store.SetUnknownModelRecorder(sm.unknownModels)
	t.Cleanup(func() { store.SetUnknownModelRecorder(nil) })

	// Size-class branch ("70b" -> self-hosted-large) and flat fallback branch
	// (nothing matches). No panic here is itself the arity assertion.
	store.ComputeCost("llama-3.1-70b", store.CostUsage{Input: 1_000_000})
	store.ComputeCost("acme-mystery-z", store.CostUsage{Input: 1_000_000})

	var sb strings.Builder
	sm.reg.Render(&sb)
	out := sb.String()
	for _, want := range []string{
		`tier_unknown_model_events_total{guess_path="` + store.GuessPathSizeClass + `"} 1`,
		`tier_unknown_model_events_total{guess_path="` + store.GuessPathFlat + `"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered metrics missing %q:\n%s", want, out)
		}
	}
}
