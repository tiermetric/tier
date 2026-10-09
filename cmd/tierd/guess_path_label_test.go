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
	// warnUnknownModel/warnHeuristicModel emit through slog.Default(); silence
	// them so the two expected WARNs don't clutter test output. Restored on
	// cleanup.
	//
	// ⚠️ SILENCING THE STDLIB log PACKAGE STILL WORKS, BUT NO LONGER FOR THE
	// REASON THIS COMMENT USED TO GIVE (#689). It said the two warnings "log to
	// log.Default()" — true then, false now. It works today because slog's
	// built-in defaultHandler bridges BACK OUT through the stdlib log package, so
	// io.Discard there still swallows them. That holds only while slog.Default()
	// is the built-in handler: if any earlier test in this package leaks a
	// SetDefault'd logger, these WARNs go to that logger instead and this
	// silencing quietly stops working. It is order-dependent, which is why
	// unknown_model_warn_level_test.go asserts that its helper restores
	// log.Writer()/log.Flags() as well as slog.Default().
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
