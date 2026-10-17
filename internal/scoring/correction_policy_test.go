package scoring

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"testing"
)

func TestCorrectionScore_FlagOffUsesLegacyValues(t *testing.T) {
	for _, w := range []float64{0, 0.5, 1, 3, 5, 8, 1.23456789} {
		for _, q := range []float64{0, 0.1, 0.55, 0.7, 0.8, 0.85, 1} {
			o := Outcome{Weight: w, Quality: q}
			p := &CorrectionProjection{Points: 999, Quality: 0, Eligibility: "ineligible", RuleVersion: "event3-v2"}
			got, err := EffectiveCorrectionValues(ScoringPolicy{}, o, p)
			if err != nil {
				t.Fatal(err)
			}
			if math.Float64bits(got.Points) != math.Float64bits(w*q) || math.Float64bits(got.Quality) != math.Float64bits(q) || !got.Eligible {
				t.Fatalf("legacy changed %v", got)
			}
		}
	}
	var scores []DeveloperScore
	for _, w := range []float64{0, 0.5, 1, 3, 5, 8, 1.23456789} {
		for _, q := range []float64{0, 0.1, 0.55, 0.7, 0.8, 0.85, 1} {
			scores = append(scores, ComputeDeveloper("legacy", []Outcome{{Weight: w, Quality: q}}, 5, 2, 1))
		}
	}
	actual, err := json.Marshal(scores)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := os.ReadFile("testdata/correction_legacy_scores.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, bytes.TrimSpace(legacy)) {
		t.Fatal("score bytes differ from 11eba0a golden")
	}

}
func TestCorrectionPolicy_SharedCountsAndRuleStamp(t *testing.T) {
	if got, err := (ScoringPolicy{}).Stamp(); err != nil || got.Policy != "legacy" || got.RuleVersion != "legacy-v1" || got.RubricVersion != 1 {
		t.Fatal(got)
	}
	if _, err := (ScoringPolicy{Event3: true}).Stamp(); err == nil {
		t.Fatal("unresolved state stamped rubric 2")
	}
	on := ScoringPolicy{Event3: true, Resolved: true, RuleVersion: "event3-v2"}
	if stamp, err := on.Stamp(); err != nil || stamp.RubricVersion != 2 {
		t.Fatal(stamp, err)
	}
	if _, err := EffectiveCorrectionValues(on, Outcome{}, nil); err == nil {
		t.Fatal("unresolved projection accepted")
	}
	p := &CorrectionProjection{Points: 1, Quality: 0, Eligibility: "ineligible", RuleVersion: "event3-v2"}
	got, err := EffectiveCorrectionValues(on, Outcome{Weight: 8, Quality: 0}, p)
	if err != nil || got.Points != 1 || got.Eligible {
		t.Fatal(got, err)
	}
	b, _ := json.Marshal(CorrectionCounts{OutcomeN: 2, SampleN: 1})
	if string(b) != `{"outcome_n":2,"sample_n":1}` {
		t.Fatal(string(b))
	}
}
