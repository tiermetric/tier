package main

import (
	"testing"
	"time"
)

// TestParseSince_DefaultBoundIsUTC guards the cmd/tierd copy of parseSince
// against the #180 non-UTC-bound defect. time.Now() carries the host's local
// zone; a bound that leaks a non-UTC offset mis-windows any ts >= ? comparison
// against UTC-stored rows because modernc.org/sqlite compares DATETIME values
// as offset-bearing strings. This CLI's since currently feeds only instant-safe
// paths, but the default bound must stay UTC so a future store-backed caller
// inherits a correct window. Fails on pre-fix main (Location was Local).
func TestParseSince_DefaultBoundIsUTC(t *testing.T) {
	got, err := parseSince("")
	if err != nil {
		t.Fatalf("parseSince(\"\") err = %v", err)
	}
	if loc := got.Location(); loc != time.UTC {
		t.Fatalf("default since bound Location = %v, want UTC (#180)", loc)
	}
}

// TestParseSince_CLIDefaultIsDeliberatelyNotSnapped pins a DIVERGENCE, which is
// an unusual thing to assert and is exactly the point.
//
// #746 snapped the SERVED default window (api.defaultSince) back to the start of
// its UTC day. This CLI copy was deliberately left unsnapped, and parseSince
// carries a long comment explaining why. Nothing enforced that: snapping here
// would silently widen `tierd score`'s JSONL scan (changing the totals it
// PRINTS) and `tierd backfill`'s merged-PR window (INSERTING extra outcome
// rows), and every other test in this package would stay green.
//
// ⛔ THIS IS NOT AN ARGUMENT THAT THE CLI MUST NEVER SNAP. It is the requirement
// that snapping be a DECISION: if a future issue rules the CLI should match the
// server, delete this test in that commit, alongside the changelog entry naming
// the two behaviour changes above. Silent harmonization is what it prevents.
func TestParseSince_CLIDefaultIsDeliberatelyNotSnapped(t *testing.T) {
	// Bracket the call and require the bound to be the RAW now-90d instant. A
	// snapped implementation returns the start of that day, which is at or before
	// `lo` and therefore outside the bracket for all but the first instants of a
	// UTC day — where the two are legitimately indistinguishable and this arm
	// passes either way rather than flaking.
	lo := time.Now().AddDate(0, 0, -90)
	got, err := parseSince("")
	hi := time.Now().AddDate(0, 0, -90)
	if err != nil {
		t.Fatalf("parseSince(\"\") err = %v", err)
	}
	if got.Before(lo) || got.After(hi) {
		t.Errorf("the CLI's default since bound is %s, outside [%s, %s] — it is no longer the raw now-90d "+
			"instant. If it was snapped to the start of the UTC day to match the server (#746), that is a "+
			"user-visible change to `tierd score` (different printed totals) and `tierd backfill` (extra "+
			"inserted rows): it needs its own issue and changelog entry. If that decision HAS been made, "+
			"delete this test in the same commit",
			got.UTC().Format(time.RFC3339Nano), lo.UTC().Format(time.RFC3339Nano),
			hi.UTC().Format(time.RFC3339Nano))
	}
}
