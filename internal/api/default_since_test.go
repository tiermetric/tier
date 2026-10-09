package api

import (
	"testing"
	"time"
)

// The #746 default-window bound, tested as a PROPERTY of a pure function rather
// than as an observation of the wall clock.
//
// 🔴 WHY THIS FILE EXISTS. The bound is `now − 90d`, snapped back to the start of
// its UTC day. Written the obvious way — `time.Now().AddDate(0,0,-90).UTC()` —
// the subtraction runs on the HOST'S wall clock, so it removes 90 entries from
// the host's calendar rather than 90×24h. On a DST host those differ by an hour,
// and after the snap that hour decides which side of a midnight the bound lands
// on: a whole DAY of window, on a cost metric. It is invisible in review (the
// two orderings read identically) and nearly invisible in CI, because it only
// bites near UTC midnight — ~2% of the year. A test that reached this through
// time.Now() would be a coin flip: green 98% of the time, and when it finally
// went red it would accuse whatever changed most recently.
//
// So `defaultSince` takes `now` as a parameter and these tests hand it the
// instants that matter.

// dstZones are the transitions that actually move the answer. Both hemispheres,
// because the offset delta reverses sign — a northern-only fixture would pin the
// bug in one direction and leave the other free.
var dstZones = []string{
	"America/New_York",
	"Europe/London",
	"Australia/Sydney",    // southern: DST runs the other way round
	"Australia/Lord_Howe", // 30-minute DST step
	"Asia/Kolkata",        // fixed +05:30 — offset without DST, the control zone
	"UTC",                 //
	"Pacific/Kiritimati",  // +14, the extreme east
	"Pacific/Niue",        // -11, the extreme west
}

// TestDefaultSince_IsIndependentOfHostZone is the guard. The bound must be the
// same INSTANT no matter what Location the caller's clock happens to carry —
// that is the #180 property, and #746 is what made a violation cost a whole day
// instead of an hour.
func TestDefaultSince_IsIndependentOfHostZone(t *testing.T) {
	// Instants chosen to land either side of a UTC midnight, since that is the
	// only place the two orderings can disagree, and spread across the year so a
	// DST transition falls inside the 90-day reach for the northern zones (an
	// October/November `now`), the southern ones (an April `now`), and neither.
	instants := []time.Time{
		time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC),
		time.Date(2026, 1, 1, 23, 59, 0, 0, time.UTC),
		time.Date(2026, 4, 15, 0, 0, 30, 0, time.UTC),
		time.Date(2026, 4, 15, 23, 30, 0, 0, time.UTC),
		time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 2, 0, 5, 0, 0, time.UTC),
		time.Date(2026, 11, 2, 23, 55, 0, 0, time.UTC),
		time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
	}
	for _, name := range dstZones {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatalf("load %s: %v", name, err) // a missing tzdata makes this suite vacuous
		}
		for _, inst := range instants {
			wantBound := defaultSince(inst)
			gotBound := defaultSince(inst.In(loc))
			if !gotBound.Equal(wantBound) {
				t.Errorf("defaultSince at %s: zone %s gives %s, UTC gives %s — the default window bound "+
					"depends on the HOST'S timezone, which is #180 with a 24-hour blast radius instead of a "+
					"one-hour one. Do the arithmetic in UTC: now.UTC().AddDate(...), never "+
					"now.AddDate(...).UTC()",
					inst.Format(time.RFC3339), name,
					gotBound.Format(time.RFC3339), wantBound.Format(time.RFC3339))
			}
			// Same call, checked for the two properties the bound must ALSO hold,
			// so a zone-independent-but-wrong answer cannot pass.
			if gotBound.Location() != time.UTC {
				t.Errorf("defaultSince(%s in %s).Location() = %v, want UTC (#180)",
					inst.Format(time.RFC3339), name, gotBound.Location())
			}
			if !gotBound.Equal(gotBound.Truncate(24 * time.Hour)) {
				t.Errorf("defaultSince(%s in %s) = %s, not midnight UTC (#746)",
					inst.Format(time.RFC3339), name, gotBound.Format(time.RFC3339Nano))
			}
			// BACKWARD, and by 90 days ± the snap. Never forward: widening can only
			// add spend that really occurred, narrowing drops a partial day of cost
			// out of a cost metric.
			if !gotBound.After(inst.Add(-91 * 24 * time.Hour)) {
				t.Errorf("defaultSince(%s) = %s is more than 91 days back", inst.Format(time.RFC3339),
					gotBound.Format(time.RFC3339))
			}
			if !gotBound.Before(inst.Add(-89 * 24 * time.Hour)) {
				t.Errorf("defaultSince(%s) = %s is less than 89 days back", inst.Format(time.RFC3339),
					gotBound.Format(time.RFC3339))
			}
		}
	}
}

// TestDefaultSince_LocalFirstOrderingReallyDiverges is the POSITIVE CONTROL, and
// without it the test above is a tautology dressed as a guard.
//
// 🔴 A property test that passes proves nothing on its own — the same assertions
// would pass against an implementation where the hazard could not arise, and a
// reader has no way to tell "this is protected" from "this was never at risk".
// This arm demonstrates the hazard is REAL by computing the rejected ordering
// over the same inputs and REQUIRING it to disagree. If Go's time package or the
// tzdata ever changed such that the two orderings coincided, this arm fails and
// says so, rather than the guard above silently becoming decoration.
func TestDefaultSince_LocalFirstOrderingReallyDiverges(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	// 2026-01-01T00:01:00Z on a New York host: `now` is in EST (-05:00) but
	// `now − 90d` lands in EDT (-04:00), so wall-clock subtraction yields an
	// instant one hour earlier — which crosses the UTC midnight and drops the
	// bound onto the PREVIOUS UTC day.
	now := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC).In(loc)

	correct := defaultSince(now)                                      // UTC first
	rejected := now.AddDate(0, 0, -90).UTC().Truncate(24 * time.Hour) // local first
	if correct.Equal(rejected) {
		t.Fatalf("the local-first ordering agrees with the UTC-first one at %s (%s) — this control no longer "+
			"demonstrates anything, so TestDefaultSince_IsIndependentOfHostZone is now guarding a hazard that "+
			"cannot occur. Re-derive the divergent instant before deleting either test",
			now.Format(time.RFC3339), loc)
	}
	if got := rejected.Sub(correct); got != -24*time.Hour {
		t.Errorf("local-first bound is %s off the correct one, want exactly -24h (a whole day of window). "+
			"correct=%s rejected=%s", got, correct.Format(time.RFC3339), rejected.Format(time.RFC3339))
	}
}
