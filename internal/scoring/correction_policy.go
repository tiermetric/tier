package scoring

import (
	"errors"
	"math"
)

// ScoringPolicy is an explicit reader input. Its zero value is always legacy.
// Production flag/state transitions and all-reader propagation belong to U4/U2;
// constructing an option never changes persisted policy or the legacy engine.
type ScoringPolicy struct {
	Event3 bool
	// U4 supplies these only from its atomically resolved stored state.
	Resolved    bool
	RuleVersion string
}
type CorrectionRuleStamp struct {
	Policy        string `json:"scoring_policy"`
	RuleVersion   string `json:"correction_rule_version"`
	RubricVersion int    `json:"rubric_version"`
}

func (p ScoringPolicy) Stamp() (CorrectionRuleStamp, error) {
	if !p.Event3 {
		return CorrectionRuleStamp{"legacy", "legacy-v1", RubricVersion}, nil
	}
	if !p.Resolved || p.RuleVersion != "event3-v2" {
		return CorrectionRuleStamp{}, errors.New("event 3 policy is not resolved under stored state")
	}
	return CorrectionRuleStamp{"event3", p.RuleVersion, 2}, nil
}

// CorrectionCounts deliberately distinguishes raw activity/coverage from eligible
// ranking observations. Later readers must carry both through every rollup.
type CorrectionCounts struct {
	OutcomeN int `json:"outcome_n"`
	SampleN  int `json:"sample_n"`
}
type CorrectionProjection struct {
	Points, Quality float64
	Eligibility     string
	RuleVersion     string
}
type CorrectionValues struct {
	Points, Quality float64
	Eligible        bool
	Eligibility     string
	RuleVersion     string
}

// EffectiveCorrectionValues is the shared reader boundary. Stored points are
// consumed once, including when own quality is zero. No quality division or
// weight reconstruction is permitted. A flag-on missing projection refuses.
func EffectiveCorrectionValues(policy ScoringPolicy, legacy Outcome, projection *CorrectionProjection) (CorrectionValues, error) {
	if !policy.Event3 {
		return CorrectionValues{Points: legacy.Weight * legacy.Quality, Quality: legacy.Quality, Eligible: true, Eligibility: "eligible", RuleVersion: "legacy-v1"}, nil
	}
	if _, err := policy.Stamp(); err != nil {
		return CorrectionValues{}, err
	}
	if projection == nil || (projection.RuleVersion != "event3-v1" && projection.RuleVersion != "event3-v2") {
		return CorrectionValues{}, errors.New("event 3 projection unresolved")
	}
	if math.IsNaN(projection.Points) || math.IsInf(projection.Points, 0) || projection.Points < 0 || math.IsNaN(projection.Quality) || math.IsInf(projection.Quality, 0) || projection.Quality < 0 || projection.Quality > 1 {
		return CorrectionValues{}, errors.New("invalid Event 3 projection")
	}
	switch projection.Eligibility {
	case "eligible", "ineligible", "unknown":
	default:
		return CorrectionValues{}, errors.New("invalid ranking eligibility")
	}
	return CorrectionValues{Points: projection.Points, Quality: projection.Quality, Eligible: projection.Eligibility == "eligible", Eligibility: projection.Eligibility, RuleVersion: projection.RuleVersion}, nil
}

// Points returns stored final points directly; it never reconstructs a weight.
func (o Outcome) Points() float64 {
	if o.Correction != nil {
		return o.Correction.Points
	}
	return o.Weight * o.Quality
}
func (o Outcome) RankingEligible() bool { return o.Correction == nil || o.Correction.Eligible }
func (d DeveloperScore) RawOutcomeCount() int {
	if d.OutcomeN != 0 {
		return d.OutcomeN
	}
	return d.SampleN
}
