package config

import (
	"fmt"
	"testing"

	"github.com/tiermetric/tier/internal/scoring"
)

// sizeWeightScaleGoldens pins the canonical outcome-weight SCALE per
// scoring.RubricVersion (#239). validSizeWeights is one of the tables the rubric
// stamp versions, but it lives in this package while the stamp lives in scoring —
// nothing mechanically couples them. This golden is that coupling: an edit to the
// scale that does not also bump scoring.RubricVersion (and add a golden entry)
// fails the build, turning rubric.go's "MUST bump" comment into an enforced gate.
var sizeWeightScaleGoldens = map[int]string{
	1: "[0.5 1 3 5 8]",
}

func TestSizeWeightScaleMatchesRubricVersion(t *testing.T) {
	want, ok := sizeWeightScaleGoldens[scoring.RubricVersion]
	if !ok {
		t.Fatalf("no size-weight-scale golden for scoring.RubricVersion=%d; when you bump the version, add its golden here", scoring.RubricVersion)
	}
	if got := fmt.Sprint(validSizeWeights); got != want {
		t.Errorf("validSizeWeights = %s, but the golden for RubricVersion=%d is %s.\n"+
			"The weight scale is part of the canonical rubric: if this change is intended, "+
			"bump scoring.RubricVersion and add a matching golden; otherwise revert.", got, scoring.RubricVersion, want)
	}
}
