package qualityrule

import (
	"math"
	"math/big"
	"testing"
	"time"
)

var originalMerge = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func correctionAt(at time.Time) Correction {
	return Correction{Active: true, Confirmed: true, ChildMergedAt: at}
}

func confirmedCorrection() []Correction {
	return []Correction{correctionAt(originalMerge.Add(time.Hour))}
}

func assertQuality(t *testing.T, got Result, want float64, numerator, denominator int64) {
	t.Helper()
	if math.Float64bits(got.Float64) != math.Float64bits(want) {
		t.Errorf("float = %.17g (%x), want %.17g (%x)", got.Float64, math.Float64bits(got.Float64), want, math.Float64bits(want))
	}
	if got.Rational == nil || got.Rational.Cmp(big.NewRat(numerator, denominator)) != 0 {
		t.Errorf("rational = %v, want %d/%d", got.Rational, numerator, denominator)
	}
}

func TestQualityLevelsWithinBounds(t *testing.T) {
	for i, l := range levels {
		if !(l.value >= 0.1 && l.value <= 1.0) {
			t.Errorf("level %d: float = %v, want within [0.1, 1.0]", i, l.value)
		}
		rational := big.NewRat(l.numerator, l.denominator)
		if rational.Cmp(big.NewRat(1, 10)) < 0 || rational.Cmp(big.NewRat(1, 1)) > 0 {
			t.Errorf("level %d: rational = %v, want within [1/10, 1]", i, rational)
		}
	}
}

func TestResolveWithCorrection_ExactConstants(t *testing.T) {
	for _, tc := range []struct {
		name             string
		fail, correction bool
		want             float64
		n, d             int64
	}{
		{"clean", false, false, 1.0, 1, 1},
		{"clean corrected", false, true, 0.85, 17, 20},
		{"CI failure", true, false, 0.7, 7, 10},
		{"CI failure corrected", true, true, 0.55, 11, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []Event
			if tc.fail {
				events = []Event{{EventType: EventCIFail}}
			}
			var corrections []Correction
			if tc.correction {
				corrections = confirmedCorrection()
			}
			assertQuality(t, ResolveWithCorrection(events, originalMerge, corrections), tc.want, tc.n, tc.d)
		})
	}
}

func TestCorrectionQuality_InclusiveFourteenDayBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Time
		want float64
		n, d int64
	}{
		{"before original", originalMerge.Add(-time.Nanosecond), 1, 1, 1},
		{"same instant", originalMerge, 0.85, 17, 20},
		{"just inside", originalMerge.Add(14*24*time.Hour - time.Nanosecond), 0.85, 17, 20},
		{"exactly fourteen days", originalMerge.Add(14 * 24 * time.Hour), 0.85, 17, 20},
		{"one ns outside", originalMerge.Add(14*24*time.Hour + time.Nanosecond), 1, 1, 1},
		{"missing child occurrence", time.Time{}, 1, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertQuality(t, ResolveWithCorrection(nil, originalMerge, []Correction{correctionAt(tc.at)}), tc.want, tc.n, tc.d)
		})
	}
	assertQuality(t, ResolveWithCorrection(nil, time.Time{}, confirmedCorrection()), 1, 1, 1)
	// Different offsets represent the same occurrence, not local calendar days.
	child := originalMerge.Add(14 * 24 * time.Hour).In(time.FixedZone("offset", -7*60*60))
	assertQuality(t, ResolveWithCorrection(nil, originalMerge, []Correction{correctionAt(child)}), 0.85, 17, 20)
}

func TestCorrectionQuality_RequiresActiveConfirmation(t *testing.T) {
	for _, c := range []Correction{
		{ChildMergedAt: originalMerge},
		{Active: true, ChildMergedAt: originalMerge},
		{Confirmed: true, ChildMergedAt: originalMerge},
	} {
		assertQuality(t, ResolveWithCorrection(nil, originalMerge, []Correction{c}), 1, 1, 1)
	}
}

func TestCorrectionQuality_MissingMergeOccurrences(t *testing.T) {
	zero := time.Time{}
	for _, tc := range []struct {
		name            string
		original, child time.Time
	}{
		{"missing original", zero, zero.Add(time.Hour)},
		{"missing child", zero.Add(-time.Hour), zero},
		{"both missing", zero, zero},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertQuality(t, ResolveWithCorrection(nil, tc.original, []Correction{correctionAt(tc.child)}), 1, 1, 1)
		})
	}
}

func TestCorrectionQuality_RepeatedCorrectionsApplyOnce(t *testing.T) {
	corrections := []Correction{
		correctionAt(originalMerge.Add(-time.Hour)),
		correctionAt(originalMerge.Add(time.Hour)),
		correctionAt(originalMerge.Add(time.Hour)),
		correctionAt(originalMerge.Add(2 * time.Hour)),
		correctionAt(originalMerge.Add(30 * 24 * time.Hour)),
	}
	assertQuality(t, ResolveWithCorrection(nil, originalMerge, corrections), 0.85, 17, 20)
	assertQuality(t, ResolveWithCorrection([]Event{{EventType: EventCIFail}}, originalMerge, corrections), 0.55, 11, 20)
	for i, j := 0, len(corrections)-1; i < j; i, j = i+1, j-1 {
		corrections[i], corrections[j] = corrections[j], corrections[i]
	}
	assertQuality(t, ResolveWithCorrection(nil, originalMerge, corrections), 0.85, 17, 20)
}

func TestCorrectionQuality_FlakyNeutralizationPreserved(t *testing.T) {
	fail := Event{EventType: EventCIFail, HeadSHA: "sha", WorkflowID: "42"}
	flaky := Event{EventType: EventCIFailFlaky, HeadSHA: "sha", WorkflowID: "42"}
	for _, tc := range []struct {
		name   string
		events []Event
		want   float64
		n, d   int64
	}{
		{"matching", []Event{fail, flaky}, 0.85, 17, 20},
		{"flaky first", []Event{flaky, fail}, 0.85, 17, 20},
		{"later failure also neutralised", []Event{fail, flaky, fail}, 0.85, 17, 20},
		{"different workflow", []Event{fail, {EventType: EventCIFailFlaky, HeadSHA: "sha", WorkflowID: "99"}}, 0.55, 11, 20},
		{"different SHA", []Event{fail, {EventType: EventCIFailFlaky, HeadSHA: "other", WorkflowID: "42"}}, 0.55, 11, 20},
		{"legacy mismatch", []Event{fail, {EventType: EventCIFailFlaky, HeadSHA: "sha"}}, 0.55, 11, 20},
		{"legacy pair", []Event{{EventType: EventCIFail, HeadSHA: "sha"}, {EventType: EventCIFailFlaky, HeadSHA: "sha"}}, 0.85, 17, 20},
		{"other failure survives", []Event{fail, flaky, {EventType: EventCIFail, HeadSHA: "sha", WorkflowID: "99"}}, 0.55, 11, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertQuality(t, ResolveWithCorrection(tc.events, originalMerge, confirmedCorrection()), tc.want, tc.n, tc.d)
		})
	}
}

func TestCorrectionQuality_RevertFloorsPreserved(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []Event
		want   float64
		n, d   int64
	}{
		{"strategic", []Event{{EventType: EventRevertStrategic}}, 0.8, 4, 5},
		{"quality", []Event{{EventType: EventRevertQuality}}, 0.1, 1, 10},
		{"CI and strategic", []Event{{EventType: EventCIFail}, {EventType: EventRevertStrategic}}, 0.55, 11, 20},
		{"CI and quality", []Event{{EventType: EventCIFail}, {EventType: EventRevertQuality}}, 0.1, 1, 10},
		{"both reverts", []Event{{EventType: EventRevertStrategic}, {EventType: EventRevertQuality}}, 0.1, 1, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertQuality(t, ResolveWithCorrection(tc.events, originalMerge, confirmedCorrection()), tc.want, tc.n, tc.d)
		})
	}
	// CI pass and unknown future event types remain audit-only.
	assertQuality(t, ResolveWithCorrection([]Event{{EventType: EventCIPass}, {EventType: "future"}}, originalMerge, confirmedCorrection()), 0.85, 17, 20)
}

func TestResolve_RevertPrecedenceBothOrders(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []Event
	}{
		{"quality then strategic", []Event{{EventType: EventRevertQuality}, {EventType: EventRevertStrategic}}},
		{"strategic then quality", []Event{{EventType: EventRevertStrategic}, {EventType: EventRevertQuality}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("legacy", func(t *testing.T) {
				assertQuality(t, Resolve(tc.events), 0.1, 1, 10)
			})
			t.Run("correction", func(t *testing.T) {
				assertQuality(t, ResolveWithCorrection(tc.events, originalMerge, confirmedCorrection()), 0.1, 1, 10)
			})
		})
	}
}

func TestCorrectionQuality_RationalsAreExact(t *testing.T) {
	for _, tc := range []struct {
		events      []Event
		corrections []Correction
		want        float64
		n, d        int64
	}{
		{nil, nil, 1, 1, 1},
		{nil, confirmedCorrection(), 0.85, 17, 20},
		{[]Event{{EventType: EventCIFail}}, nil, 0.7, 7, 10},
		{[]Event{{EventType: EventCIFail}}, confirmedCorrection(), 0.55, 11, 20},
		{[]Event{{EventType: EventRevertStrategic}}, confirmedCorrection(), 0.8, 4, 5},
		{[]Event{{EventType: EventRevertQuality}}, confirmedCorrection(), 0.1, 1, 10},
	} {
		got := ResolveWithCorrection(tc.events, originalMerge, tc.corrections)
		assertQuality(t, got, tc.want, tc.n, tc.d)
		if got.Rational.Num().Int64() != tc.n || got.Rational.Denom().Int64() != tc.d {
			t.Errorf("noncanonical rational: %v, want %d/%d", got.Rational, tc.n, tc.d)
		}
		// Callers may use the returned rational as an accumulator without
		// corrupting later resolutions or other callers' results.
		got.Rational.SetInt64(999)
		assertQuality(t, ResolveWithCorrection(tc.events, originalMerge, tc.corrections), tc.want, tc.n, tc.d)
	}
}
