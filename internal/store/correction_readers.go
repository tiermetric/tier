package store

import (
	"context"
	"strings"

	"github.com/tiermetric/tier/internal/event3"
)

// OutcomeProjection keeps raw outcome evidence separate from final allocation.
// Rational strings preserve the exact stored inputs in exports.
type OutcomeProjection struct {
	RawWeight          string  `json:"raw_weight"`
	ReviewedOwnWeight  string  `json:"reviewed_own_weight"`
	MarginalCredit     string  `json:"marginal_credit"`
	CreditedWeight     string  `json:"credited_weight"`
	ProjectedQuality   float64 `json:"projected_quality"`
	AllocatedPoints    float64 `json:"allocated_points"`
	RankingEligibility string  `json:"ranking_eligibility"`
	Status             string  `json:"status"`
	PendingReason      string  `json:"pending_reason"`
	RuleVersion        string  `json:"rule_version"`
	Provisional        bool    `json:"provisional"`
	Disputed           bool    `json:"disputed"`
	Frozen             bool    `json:"frozen"`
}

func (r reader) attachCorrectionProjections(ctx context.Context, out []Outcome) error {
	if len(out) == 0 {
		return nil
	}

	for start := 0; start < len(out); start += 400 {
		end := start + 400
		if end > len(out) {
			end = len(out)
		}
		if err := r.attachCorrectionProjectionBatch(ctx, out[start:end]); err != nil {
			return err
		}
	}
	return nil
}
func (r reader) attachCorrectionProjectionBatch(ctx context.Context, out []Outcome) error {
	// Reuse the reader's snapshot and fetch only this bounded batch's projections.
	args := make([]any, len(out))
	for i, o := range out {
		args[i] = o.ID
	}
	// The persisted projection was resolved on complete chains before filtering.
	// Reuse this reader's connection, including when it belongs to a Snapshot.
	rows, err := r.q.QueryContext(ctx, `SELECT outcome_id,raw_weight,reviewed_own_weight,marginal_credit,share,projected_quality,allocated_points,ranking_eligibility,status,pending_reason,rule_version,provisional,disputed,frozen FROM correction_allocations WHERE outcome_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(out)), ",")+`)`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	positions := make(map[int64]int, len(out))
	for i := range out {
		positions[out[i].ID] = i
	}
	for rows.Next() {
		var id int64
		var p OutcomeProjection
		var quality, points string
		if err = rows.Scan(&id, &p.RawWeight, &p.ReviewedOwnWeight, &p.MarginalCredit, &p.CreditedWeight, &quality, &points, &p.RankingEligibility, &p.Status, &p.PendingReason, &p.RuleVersion, &p.Provisional, &p.Disputed, &p.Frozen); err != nil {
			return err
		}
		i, ok := positions[id]
		if !ok {
			continue
		}
		q, e := event3.DecodeRat(quality)
		if e != nil {
			return e
		}
		v, e := event3.DecodeRat(points)
		if e != nil {
			return e
		}
		p.ProjectedQuality, _ = q.Float64()
		p.AllocatedPoints, _ = v.Float64()
		out[i].Projection = &p
	}
	return rows.Err()
}
