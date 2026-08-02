package store

import (
	"testing"

	"github.com/tiermetric/tier/internal/scoring"
)

// workTypeTaxonomyGoldens pins the canonical work-type TAXONOMY per
// scoring.RubricVersion (#239). canonicalWorkTypes lives in this package while the
// rubric stamp lives in scoring; this golden is the coupling that makes the stamp
// honest. Appending a work_type (or reordering the taxonomy) without bumping
// scoring.RubricVersion fails here — a segmented score is only comparable across
// responses under a matched rubric version, so a silent taxonomy change would
// break that guarantee invisibly.
var workTypeTaxonomyGoldens = map[int]string{
	1: "feature, bug, security, incident, tech-debt, research, compliance",
}

func TestWorkTypeTaxonomyMatchesRubricVersion(t *testing.T) {
	want, ok := workTypeTaxonomyGoldens[scoring.RubricVersion]
	if !ok {
		t.Fatalf("no work-type-taxonomy golden for scoring.RubricVersion=%d; when you bump the version, add its golden here", scoring.RubricVersion)
	}
	if got := WorkTypeList(); got != want {
		t.Errorf("work-type taxonomy = %q, but the golden for RubricVersion=%d is %q.\n"+
			"The taxonomy is part of the canonical rubric: if this change is intended, "+
			"bump scoring.RubricVersion and add a matching golden; otherwise revert.", got, scoring.RubricVersion, want)
	}
}

// weightHeuristicScaleGoldens pins the diff-size fallback scale (GitHeuristic) and
// MaxOutcomeWeight to the rubric version. GitHeuristic is the SINGLE source of the
// fallback weight, so an unlabeled PR is commensurate with a labeled one; if its
// buckets drift off the {0.5,1,3,5,8} scale, the rubric changed and the stamp must
// bump too.
var weightHeuristicScaleGoldens = map[int][]float64{
	1: {0.5, 1, 3, 5, 8},
}

func TestWeightHeuristicScaleMatchesRubricVersion(t *testing.T) {
	want, ok := weightHeuristicScaleGoldens[scoring.RubricVersion]
	if !ok {
		t.Fatalf("no weight-heuristic-scale golden for scoring.RubricVersion=%d; add one when you bump the version", scoring.RubricVersion)
	}
	// Recover the scale by sampling one input inside each GitHeuristic bucket
	// (effort = linesChanged + 10*filesChanged): <=15, <=60, <=200, <=1000, >1000.
	got := []float64{
		GitHeuristic(1, 0),
		GitHeuristic(30, 0),
		GitHeuristic(100, 0),
		GitHeuristic(500, 0),
		GitHeuristic(2000, 0),
	}
	if len(got) != len(want) {
		t.Fatalf("bucket count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("GitHeuristic scale = %v, golden for RubricVersion=%d is %v; bump scoring.RubricVersion if intended", got, scoring.RubricVersion, want)
			break
		}
	}
	if MaxOutcomeWeight != want[len(want)-1] {
		t.Errorf("MaxOutcomeWeight = %v, but the top of the rubric scale is %v", MaxOutcomeWeight, want[len(want)-1])
	}
}
