package scoring

import (
	"math/rand/v2"
	"strings"
	"testing"
)

func TestSampleN_PureRepairAllocatedOnePointStillExcluded(t *testing.T) {
	out := []Outcome{{Weight: 5, Quality: 1, Correction: &CorrectionValues{Points: 3.25, Eligible: true}}, {Weight: 8, Quality: 0, Correction: &CorrectionValues{Points: 1, Eligible: false}}}
	s := ComputeDeveloper("dev", out, 10, 10, 0)
	if s.WeightedPoints != 4.25 || s.SampleN != 1 {
		t.Fatal(s)
	}
}
func TestCorrectionBootstrap_ZeroGrowthShareIsFixedPoints(t *testing.T) {
	lo, hi := BootstrapCIWithFixedPoints([]float64{3.25}, []float64{5}, 1, 5, 100, rand.New(rand.NewPCG(1, 2)))
	if lo != 425 || hi != 425 {
		t.Fatal(lo, hi)
	}
}
func TestSampleN_ZeroGrowthRepairCannotUnlockRankingFloor(t *testing.T) {
	out := []Outcome{{Correction: &CorrectionValues{Points: 1, Eligible: true}}, {Correction: &CorrectionValues{Points: 1, Eligible: true}}, {Correction: &CorrectionValues{Points: 1, Eligible: false}}}
	s := ComputeDeveloper("dev", out, 10, 10, 0)
	if s.Ranked || s.SampleN != 2 || s.RawOutcomeCount() != 3 {
		t.Fatal(s)
	}
	repair := ComputeDeveloper("repair", []Outcome{{Correction: &CorrectionValues{Points: 0, Eligible: false}}}, 0, 0, 0)
	if !repair.HasActivity() || !repair.contributes() || !repair.carries()[measurePoints] {
		t.Fatal("repair raw activity lost", repair)
	}
	folded := PreFold([]LabeledScore{{Label: "team", Score: s}, {Label: "team", Score: repair}}, countAll, func(s string) string { return s })
	if len(folded) != 1 || folded[0].Sums.SampleN != 2 || folded[0].Sums.OutcomeN != 4 {
		t.Fatal(folded)
	}
}
func TestCorrectionReport_DescribesEligibleObservationCount(t *testing.T) {
	s := ComputeDeveloper("repair", []Outcome{{Correction: &CorrectionValues{Points: 1, Eligible: false}}}, 10, 10, 0)
	if report := FormatReport([]DeveloperScore{s}, "2026-10-01", AggregationDeveloper); !strings.Contains(report, "eligible growth outcomes") {
		t.Fatal(report)
	}
}
