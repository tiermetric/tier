package quality

import (
	"testing"

	"github.com/tiermetric/tier/internal/scoring"
)

// qualityFloorGoldens pins the canonical quality MULTIPLIER floors per
// scoring.RubricVersion (#239). The floors live in this package (resolve.go)
// while the rubric stamp lives in scoring; nothing mechanically couples them.
// This golden is that coupling: the floors directly scale weighted_points
// (weighted_points = Sum(weight * quality)), so editing one silently changes
// every cost_per_point and TIER number while rubric.version stays put -- the
// exact version-matched-comparison failure rubric.version exists to prevent. An
// edit to a floor that does not also bump scoring.RubricVersion (and add a
// golden entry) fails the build, turning docs/rubric.md's "changing them also
// bumps rubric.version" from prose into an enforced gate. The guard is
// bidirectional: bumping the version without updating the golden also fails (the
// map lookup misses), mirroring the size-weight and work-type taxonomy guards in
// internal/config and internal/store.
//
// The golden is written as float LITERALS, deliberately NOT as the Floor*
// constants: a golden that referenced the constants would track any edit to
// resolve.go and so pin nothing. The order is {Clean, CIFail, RevertStrategic,
// RevertQuality}.
var qualityFloorGoldens = map[int][4]float64{
	1: {1.0, 0.7, 0.8, 0.1},
}

func TestQualityFloorsMatchRubricVersion(t *testing.T) {
	want, ok := qualityFloorGoldens[scoring.RubricVersion]
	if !ok {
		t.Fatalf("no quality-floor golden for scoring.RubricVersion=%d; when you bump the version, add its golden here", scoring.RubricVersion)
	}
	got := [4]float64{FloorClean, FloorCIFail, FloorRevertStrategic, FloorRevertQuality}
	if got != want {
		t.Errorf("quality floors {Clean, CIFail, RevertStrategic, RevertQuality} = %v, "+
			"but the golden for RubricVersion=%d is %v.\n"+
			"The quality floors are part of the canonical rubric: they scale weighted_points, "+
			"so changing one alters every cost_per_point and TIER number. If this change is "+
			"intended, bump scoring.RubricVersion and add a matching golden; otherwise revert.",
			got, scoring.RubricVersion, want)
	}
}
