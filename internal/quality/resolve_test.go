package quality

import (
	"testing"

	"github.com/tiermetric/tier/internal/store"
)

// ev is a terse constructor for a quality event in these table tests. Only the
// fields Resolve inspects (EventType, SourceRef) are set.
func ev(eventType, sourceRef string) store.QualityEvent {
	return store.QualityEvent{EventType: eventType, SourceRef: sourceRef}
}

// TestResolve mirrors quality-degradation-spec §10 test IDs where noted.
func TestResolve(t *testing.T) {
	const sha = "abc123"
	cases := []struct {
		name   string
		events []store.QualityEvent
		want   float64
	}{
		{"clean ship, no events (TEST 1)", nil, 1.0},
		{"ci_pass only", []store.QualityEvent{ev(EventCIPass, sha+":1")}, 1.0},
		{"ci_fail (TEST 2)", []store.QualityEvent{ev(EventCIFail, sha+":1")}, 0.7},
		{
			"flaky re-run neutralises failure (TEST 3)",
			[]store.QualityEvent{ev(EventCIFail, sha+":1"), ev(EventCIFailFlaky, sha+":2")},
			1.0,
		},
		{"quality revert (TEST 9)", []store.QualityEvent{ev(EventRevertQuality, "revsha")}, 0.1},
		{"strategic revert (TEST 10)", []store.QualityEvent{ev(EventRevertStrategic, "revsha")}, 0.8},
		{
			"worst-of floors: ci_fail + quality revert",
			[]store.QualityEvent{ev(EventCIFail, sha+":1"), ev(EventRevertQuality, "revsha")},
			0.1,
		},
		{
			"worst-of floors: ci_fail + strategic revert",
			[]store.QualityEvent{ev(EventCIFail, sha+":1"), ev(EventRevertStrategic, "revsha")},
			0.7,
		},
		{
			"flaky clears ci_fail but strategic revert still floors",
			[]store.QualityEvent{ev(EventCIFail, sha+":1"), ev(EventCIFailFlaky, sha+":2"), ev(EventRevertStrategic, "r")},
			0.8,
		},
		{
			"flaky for a different SHA does NOT clear this failure",
			[]store.QualityEvent{ev(EventCIFail, sha+":1"), ev(EventCIFailFlaky, "otherSHA:1")},
			0.7,
		},
		{
			"unknown/phase-2 event type has no effect",
			[]store.QualityEvent{ev("followup_fix", "x"), ev(EventCIPass, sha+":1")},
			1.0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Resolve(tc.events); got != tc.want {
				t.Errorf("Resolve = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResolve_CleanShip pins spec TEST 1 as a named case (acceptance criterion).
func TestResolve_CleanShip(t *testing.T) {
	if got := Resolve(nil); got != 1.0 {
		t.Errorf("Resolve(no events) = %v, want 1.0", got)
	}
}

// TestResolve_CIFailure pins spec TEST 2.
func TestResolve_CIFailure(t *testing.T) {
	if got := Resolve([]store.QualityEvent{ev(EventCIFail, "sha:1")}); got != 0.7 {
		t.Errorf("Resolve(ci_fail) = %v, want 0.7", got)
	}
}

// TestResolve_FlakyRerunNeutralisesFailure pins spec TEST 3.
func TestResolve_FlakyRerunNeutralisesFailure(t *testing.T) {
	got := Resolve([]store.QualityEvent{ev(EventCIFail, "sha:1"), ev(EventCIFailFlaky, "sha:2")})
	if got != 1.0 {
		t.Errorf("Resolve(ci_fail + ci_fail_flaky) = %v, want 1.0", got)
	}
}

// TestResolve_QualityRevert pins spec TEST 9.
func TestResolve_QualityRevert(t *testing.T) {
	if got := Resolve([]store.QualityEvent{ev(EventRevertQuality, "r")}); got != 0.1 {
		t.Errorf("Resolve(revert_quality) = %v, want 0.1", got)
	}
}

// TestResolve_StrategicRevert pins spec TEST 10.
func TestResolve_StrategicRevert(t *testing.T) {
	if got := Resolve([]store.QualityEvent{ev(EventRevertStrategic, "r")}); got != 0.8 {
		t.Errorf("Resolve(revert_strategic) = %v, want 0.8", got)
	}
}

// TestResolve_WorstOfFloors: ci_fail + revert_quality resolves to the worst.
func TestResolve_WorstOfFloors(t *testing.T) {
	got := Resolve([]store.QualityEvent{ev(EventCIFail, "sha:1"), ev(EventRevertQuality, "r")})
	if got != 0.1 {
		t.Errorf("Resolve(ci_fail + revert_quality) = %v, want 0.1", got)
	}
}

// TestClassifyRevert exercises the spec §3 Event 4 keyword classification,
// including the ambiguity default (quality) and mixed-hit default (quality).
func TestClassifyRevert(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    string
	}{
		{"quality keyword: caused OOM", `Revert "feat: cache"

This reverts commit deadbeef. caused OOM under load`, EventRevertQuality},
		{"quality keyword: broke prod", "Revert \"x\"\n\nbroke production", EventRevertQuality},
		{"quality keyword: regression", "Revert: introduced a regression", EventRevertQuality},
		{"strategic: product decision", "Revert \"feat: widget\"\n\nproduct decision to remove feature", EventRevertStrategic},
		{"strategic: PM requested", "Revert \"feat: x\"\n\nPM requested we pull this", EventRevertStrategic},
		{"strategic: deprecated", "Revert \"feat: x\"\n\nfeature is deprecated now", EventRevertStrategic},
		{"bare revert defaults to quality", `Revert "feat: add foo"`, EventRevertQuality},
		{"mixed strategic + quality defaults to quality", "Revert \"x\"\n\nproduct decision, but it also broke prod", EventRevertQuality},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyRevert(tc.message); got != tc.want {
				t.Errorf("ClassifyRevert(%q) = %q, want %q", tc.message, got, tc.want)
			}
		})
	}
}

// TestHeadSHA covers the source_ref parsing used by flaky matching.
func TestHeadSHA(t *testing.T) {
	cases := map[string]string{
		"abc:1":     "abc",
		"abc:12:34": "abc",
		"noattempt": "noattempt",
		"":          "",
	}
	for in, want := range cases {
		if got := HeadSHA(in); got != want {
			t.Errorf("HeadSHA(%q) = %q, want %q", in, got, want)
		}
	}
}
