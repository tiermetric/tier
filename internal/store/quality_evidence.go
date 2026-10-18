package store

import (
	"context"
	"time"

	"github.com/tiermetric/tier/internal/qualityrule"
)

type OutcomeQualityEvidence struct {
	CI             bool
	Revert         bool
	Unknown        int
	UnknownFailure bool
	RecoveredRows  int
	RecordedRows   int
	Workflows      map[string]bool
}

type QualityEvidenceRead struct {
	Cutover  time.Time
	Outcomes map[int64]OutcomeQualityEvidence
}

func (d *DB) QualityEvidenceWindow(ctx context.Context, since, until time.Time, scope RepoScope) (QualityEvidenceRead, error) {
	return (reader{d.db}).QualityEvidenceWindow(ctx, since, until, scope)
}

// Only events still joined to current outcomes can contribute evidence.
func (r reader) QualityEvidenceWindow(ctx context.Context, since, until time.Time, scope RepoScope) (QualityEvidenceRead, error) {
	out := QualityEvidenceRead{Outcomes: map[int64]OutcomeQualityEvidence{}}
	if err := r.q.QueryRowContext(ctx, `SELECT applied_at FROM tier_migrations WHERE name=?`, QualityTriggerCutover).Scan(&out.Cutover); err != nil {
		return out, err
	}
	where, args := digestPredicate(since, until, scope)
	rows, err := r.q.QueryContext(ctx, `SELECT q.outcome_id,q.event_type,q.source_ref,q.trigger_event,
 EXISTS(SELECT 1 FROM quality_history h WHERE h.outcome_id=q.outcome_id AND h.reason LIKE 'resolve-triggers:%' AND h.source_ref='quality-event:'||q.id)
 FROM quality_events q JOIN (SELECT id FROM outcomes WHERE `+where+`) o ON o.id=q.outcome_id ORDER BY q.id`, args...)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var e QualityEvent
		var recovered bool
		if err := rows.Scan(&e.OutcomeID, &e.EventType, &e.SourceRef, &e.TriggerEvent, &recovered); err != nil {
			return out, err
		}
		v := out.Outcomes[e.OutcomeID]
		if v.Workflows == nil {
			v.Workflows = map[string]bool{}
		}
		switch e.EventType {
		case "ci_pass", "ci_fail", "ci_fail_flaky":
			if e.TriggerEvent == nil {
				v.Unknown++
				if e.EventType == qualityrule.EventCIFail || e.EventType == qualityrule.EventCIFailFlaky {
					v.UnknownFailure = true
				}
			} else if *e.TriggerEvent == "push" {
				v.CI = true
				wf := qualityrule.ParseCIRef(e.SourceRef).WorkflowID
				if wf == "" {
					wf = "unknown"
				}
				v.Workflows[wf] = true
			}
		case "revert_quality", "revert_strategic":
			v.Revert = true
		}
		if e.IsEvidence() {
			if recovered {
				v.RecoveredRows++
			} else {
				v.RecordedRows++
			}
		}
		out.Outcomes[e.OutcomeID] = v
	}
	return out, rows.Err()
}
