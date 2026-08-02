package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// TestParseSince_DefaultBoundIsUTC guards the root cause of #180: the default
// (empty ?since=) branch of parseSince must return a UTC-zoned bound. On a
// non-UTC host, time.Now() carries the local zone, and modernc.org/sqlite binds
// that time.Time as an offset-bearing DATETIME string that compares lexically
// (not temporally) against the UTC-stored ts column — silently mis-windowing
// rows near the 90-day boundary. This test fails on pre-fix main (Location was
// the host's Local) and passes once the default branch returns .UTC().
func TestParseSince_DefaultBoundIsUTC(t *testing.T) {
	got, err := parseSince("")
	if err != nil {
		t.Fatalf("parseSince(\"\") err = %v", err)
	}
	if loc := got.Location(); loc != time.UTC {
		t.Fatalf("default since bound Location = %v, want UTC (non-UTC bound mis-windows ts >= ? — #180)", loc)
	}
}

// TestSinceWindow_UTCNormalizedAcrossOffsetZones is the store-backed regression
// for #180. It inserts one cost row and one outcome row at a known UTC instant,
// then windows them with a `since` bound that denotes an instant either side of
// that row but is expressed in a non-UTC fixed-offset zone. Correct behavior is
// inclusion/exclusion by INSTANT, independent of the zone the bound carries.
//
// The query path mirrors production: handleGetScores / handleGetDeveloperScore
// pass since through sinceUTC before binding it. Pre-fix (raw, non-normalized
// bound) the negative-offset case is wrongly INCLUDED and the positive-offset
// case is wrongly EXCLUDED — both assertions below fail. Post-fix they pass.
func TestSinceWindow_UTCNormalizedAcrossOffsetZones(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tier-since-tz.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const developer = "alice"
	// The row sits at exactly 12:00:00Z. Rendered by SQLite as
	// "2026-03-01 12:00:00+00:00" — the "12" is what the buggy lexical compare
	// weighs against the bound's hour field.
	rowInstant := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	if err := db.InsertTokenEvent(ctx, store.TokenEvent{
		Developer: developer, IssueID: "1", Model: "claude-3-5-sonnet",
		CostMicro: 1_000_000, Source: "api", Fidelity: "realtime",
		Timestamp: rowInstant,
	}); err != nil {
		t.Fatalf("insert token event: %v", err)
	}
	if _, err := db.InsertOutcome(ctx, store.Outcome{
		Developer: developer, IssueID: "1", PRNumber: 1,
		Weight: 1, Quality: 1, Timestamp: rowInstant,
	}); err != nil {
		t.Fatalf("insert outcome: %v", err)
	}

	// A negative and a positive fixed-offset zone, chosen so the bound's
	// wall-clock hour field straddles the row's "12" in the wrong direction
	// under a naive string compare.
	negZone := time.FixedZone("UTC-7", -7*3600) // e.g. US Mountain
	posZone := time.FixedZone("UTC+8", 8*3600)  // e.g. China Standard

	cases := []struct {
		name         string
		since        time.Time // bound as the caller would express it (non-UTC)
		wantIncluded bool      // expected result, judged by instant vs rowInstant
	}{
		{
			// since instant = 18:00Z, six hours AFTER the row → exclude.
			// Expressed as 11:00:00-07:00. Raw lexical compare sees "12" >= "11"
			// and wrongly INCLUDES the row.
			name:         "negative_offset_after_row_excludes",
			since:        rowInstant.Add(6 * time.Hour).In(negZone),
			wantIncluded: false,
		},
		{
			// since instant = 06:00Z, six hours BEFORE the row → include.
			// Expressed as 14:00:00+08:00. Raw lexical compare sees "12" < "14"
			// and wrongly EXCLUDES the row.
			name:         "positive_offset_before_row_includes",
			since:        rowInstant.Add(-6 * time.Hour).In(posZone),
			wantIncluded: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			since := sinceUTC(tc.since) // exactly what the handlers now apply

			costs, err := db.DeveloperCosts(ctx, since)
			if err != nil {
				t.Fatalf("DeveloperCosts: %v", err)
			}
			if included := len(costs) > 0; included != tc.wantIncluded {
				t.Errorf("DeveloperCosts included=%v, want %v (since instant %s vs row %s)",
					included, tc.wantIncluded,
					tc.since.UTC().Format(time.RFC3339), rowInstant.Format(time.RFC3339))
			}

			all, err := db.AllOutcomesSince(ctx, since)
			if err != nil {
				t.Fatalf("AllOutcomesSince: %v", err)
			}
			if included := len(all) > 0; included != tc.wantIncluded {
				t.Errorf("AllOutcomesSince included=%v, want %v", included, tc.wantIncluded)
			}

			devOut, err := db.DeveloperOutcomes(ctx, developer, since)
			if err != nil {
				t.Fatalf("DeveloperOutcomes: %v", err)
			}
			if included := len(devOut) > 0; included != tc.wantIncluded {
				t.Errorf("DeveloperOutcomes included=%v, want %v", included, tc.wantIncluded)
			}
		})
	}
}
