package api

import (
	"context"
	"errors"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// WithScoringPolicy selects the shared reader contract. Its zero value preserves
// legacy scoring. U4 supplies the atomically resolved persisted flag/rubric state.
func WithScoringPolicy(policy scoring.ScoringPolicy) Option {
	return func(h *Handler) { h.scoringPolicy = policy }
}

func scoringOutcomeFor(policy scoring.ScoringPolicy, o store.Outcome, developer string, zeroToken bool) (scoring.Outcome, error) {
	so := scoring.Outcome{Developer: developer, IssueID: o.IssueID, Repo: o.Repo, WorkType: o.WorkType, Weight: o.Weight, Quality: o.Quality, ZeroToken: zeroToken}
	if !policy.Event3 {
		return so, nil
	}
	var p *scoring.CorrectionProjection
	if o.Projection != nil {
		p = &scoring.CorrectionProjection{Points: o.Projection.AllocatedPoints, Quality: o.Projection.ProjectedQuality, Eligibility: o.Projection.RankingEligibility, RuleVersion: o.Projection.RuleVersion}
	}
	values, err := scoring.EffectiveCorrectionValues(policy, so, p)
	if err != nil {
		return so, err
	}
	so.Correction = &values
	return so, nil
}
func (h *Handler) correctionStamp() *scoring.CorrectionRuleStamp {
	if !h.scoringPolicy.Event3 {
		return nil
	}
	s, err := h.scoringPolicy.Stamp()
	if err != nil {
		return nil
	}
	return &s
}
func (h *Handler) rubricVersion() int {
	if s := h.correctionStamp(); s != nil {
		return s.RubricVersion
	}
	return scoring.RubricVersion
}

func rawOutcomeN(s scoring.DeveloperScore, enabled bool) *int {
	if !enabled && s.OutcomeN == 0 {
		return nil
	}
	n := s.RawOutcomeCount()
	return &n
}

// WithStoredScoringPolicy reads the policy in the SAME snapshot as the score.
// Options never flip the database; serve explicitly configures it at startup.
func WithStoredScoringPolicy() Option { return func(h *Handler) { h.storedScoringPolicy = true } }

type correctionPolicyReader interface {
	CorrectionScoringPolicy(context.Context) (store.CorrectionPolicyState, error)
}

// A seal has already validated this policy in the same snapshot. Reuse that
// result when loading its scores instead of checking all allocations twice.
type correctionPolicySnapshot struct {
	*store.Snapshot
	policy store.CorrectionPolicyState
}

func (s correctionPolicySnapshot) CorrectionScoringPolicy(context.Context) (store.CorrectionPolicyState, error) {
	return s.policy, nil
}

func (h *Handler) policyForReader(ctx context.Context, r any) (scoring.ScoringPolicy, error) {
	if !h.storedScoringPolicy {
		return h.scoringPolicy, nil
	}
	reader, ok := r.(correctionPolicyReader)
	if !ok {
		return scoring.ScoringPolicy{}, errors.New("stored correction policy reader unavailable")
	}
	state, err := reader.CorrectionScoringPolicy(ctx)
	if err != nil {
		return scoring.ScoringPolicy{}, err
	}
	return scoring.ScoringPolicy{Event3: state.Enabled, Resolved: true, RuleVersion: state.RuleVersion}, nil
}
func correctionStampFor(policy scoring.ScoringPolicy) *scoring.CorrectionRuleStamp {
	if !policy.Event3 {
		return nil
	}
	s, err := policy.Stamp()
	if err != nil {
		return nil
	}
	return &s
}
func rubricVersionFor(policy scoring.ScoringPolicy) int {
	if s := correctionStampFor(policy); s != nil {
		return s.RubricVersion
	}
	return scoring.RubricVersion
}

func detailProjectionFor(p scoring.ScoringPolicy, o store.Outcome) *store.OutcomeProjection {
	if p.Event3 {
		return o.Projection
	}
	return nil
}
