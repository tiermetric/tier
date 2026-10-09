// Package store_test, not store: it reads collector.AllSources(), and collector
// imports store, so an internal test file here would be an import cycle.
package store_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/collector"
	"github.com/tiermetric/tier/internal/store"

	_ "modernc.org/sqlite"
)

// inPollerBaselineBySource classifies every token-event source against the org
// pollers' subtraction baseline (store.CapturedTokensByDayModel, #875):
// true = its rows are per-call spend that can sit inside an Anthropic or OpenAI
// org aggregate, so the pollers must subtract them; false = they must not be
// subtracted.
//
// collector.AllSources() is itself pinned to every collector.Source* constant by
// TestAllSourcesEnumerated, so a new collector fails
// TestPollerBaselineClassifiesEverySource until it is classified here.
var inPollerBaselineBySource = map[string]bool{
	// Per-call capture that can run on a polled Anthropic or OpenAI account.
	collector.SourceJSONL:        true,
	collector.SourceProxy:        true,
	collector.SourceCodexRollout: true, // #464
	collector.SourceOpencode:     true, // #875
	// Per-call, but every Muse call is billed by Meta and no poller reads a
	// Meta account; a Muse row is never inside an Anthropic/OpenAI aggregate.
	// If a Meta poller is ever added, its baseline must include muse.
	collector.SourceMuse: false,
	// The pollers' own remainder feeds: subtracting them would make a poller
	// subtract its own prior output and converge to zero.
	collector.SourceAnthropicAdmin: false,
	collector.SourceOpenAIUsage:    false,
	// Org-level daily aggregates, not per-call capture (no producer yet).
	collector.SourceCopilotAPI: false,
	// POST /api/v1/costs: reconciled invoice cost, not per-request capture
	// (#854 tracks whether pollers should net these rows out).
	"api": false,
}

// TestPollerBaselineClassifiesEverySource fails when a source is unclassified,
// when a classification names a source that no longer exists, and when the
// classification disagrees with what CapturedTokensByDayModel actually returns
// for a row of that source.
func TestPollerBaselineClassifiesEverySource(t *testing.T) {
	known := append(collector.AllSources(), "api")
	for _, src := range known {
		want, classified := inPollerBaselineBySource[src]
		if !classified {
			t.Errorf("source %q is in collector.AllSources() but is NOT classified in "+
				"inPollerBaselineBySource. If it writes one row per API call on an account "+
				"an org poller reads, add it to CapturedTokensByDayModel's source list and "+
				"classify it true, or that poller double-counts it (#875).", src)
			continue
		}
		t.Run(src, func(t *testing.T) {
			assertInPollerBaseline(t, src, want)
		})
	}
	for src := range inPollerBaselineBySource {
		if !slices.Contains(known, src) {
			t.Errorf("inPollerBaselineBySource classifies %q, which is no longer in "+
				"collector.AllSources() — remove the stale entry", src)
		}
	}
}

// assertInPollerBaseline seeds one row of src for each polled provider and
// checks whether it appears in that provider's baseline.
func assertInPollerBaseline(t *testing.T, src string, want bool) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "baseline.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	day := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct{ provider, model string }{
		{"anthropic", "claude-sonnet-4"},
		{"openai", "gpt-4o"},
	} {
		if err := db.InsertTokenEvent(ctx, store.TokenEvent{
			Developer: "alice", IssueID: "issue-1", Model: tc.model,
			InputTok: 700, OutputTok: 30, CostMicro: 1,
			Source: src, Fidelity: "realtime", Timestamp: day.Add(3 * time.Hour),
			IdempotencyKey: "key-" + src + "-" + tc.provider,
		}); err != nil {
			t.Fatalf("InsertTokenEvent(%s/%s): %v", src, tc.model, err)
		}
		got, err := db.CapturedTokensByDayModel(ctx, day, tc.provider)
		if err != nil {
			t.Fatalf("CapturedTokensByDayModel(%s): %v", tc.provider, err)
		}
		_, present := got[tc.model]
		if present != want {
			t.Errorf("%s baseline: a %q row present = %v, want %v", tc.provider, src, present, want)
		}
		if present && got[tc.model] != (store.CostUsage{Input: 700, Output: 30}) {
			t.Errorf("%s baseline for %q = %+v, want the seeded 700/30", tc.provider, src, got[tc.model])
		}
	}
}
