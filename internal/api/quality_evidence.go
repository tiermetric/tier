package api

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/tiermetric/tier/internal/qualityrule"
	"github.com/tiermetric/tier/internal/store"
)

type evidenceWindowJSON struct {
	Matched    int `json:"matched"`
	WindowOpen int `json:"window_open"`
}
type workflowEvidenceJSON struct {
	WorkflowID string   `json:"workflow_id"`
	Numerator  float64  `json:"numerator_weight"`
	Share      *float64 `json:"quality_evidence_share"`
	Matched    int      `json:"matched"`
}
type unknownCIOutcomeJSON struct {
	OutcomeID int64  `json:"outcome_id"`
	Label     string `json:"label"`
}
type qualityEvidenceJSON struct {
	Status          string                 `json:"status"`
	Cutover         string                 `json:"cutover"`
	Completeness    string                 `json:"completeness"`
	Label           string                 `json:"label"`
	Share           *float64               `json:"quality_evidence_share"`
	Numerator       float64                `json:"numerator_weight"`
	Denominator     float64                `json:"denominator_weight"`
	TriggerUnknown  int                    `json:"trigger_unknown"`
	UnknownOutcomes []unknownCIOutcomeJSON `json:"trigger_unknown_outcomes"`
	RecordedRows    int                    `json:"recorded_numerator_rows"`
	RecoveredRows   int                    `json:"recovered_numerator_rows"`
	CI              evidenceWindowJSON     `json:"ci"`
	Revert          evidenceWindowJSON     `json:"revert"`
	Workflows       []workflowEvidenceJSON `json:"workflows"`
}

func evidenceForWindow(win windowScores, read store.QualityEvidenceRead, now time.Time, developer string) qualityEvidenceJSON {
	e := qualityEvidenceJSON{Status: "shown", Cutover: read.Cutover.UTC().Format(time.RFC3339), Completeness: "unknown", Label: "evidence incomplete", Workflows: []workflowEvidenceJSON{}, UnknownOutcomes: []unknownCIOutcomeJSON{}}
	workflows := map[string]*workflowEvidenceJSON{}
	for i, o := range win.outcomes {
		if developer != "" && win.canon(o.Developer) != developer {
			continue
		}
		v := read.Outcomes[o.ID]
		e.TriggerUnknown += v.Unknown
		if v.UnknownFailure {
			e.UnknownOutcomes = append(e.UnknownOutcomes, unknownCIOutcomeJSON{o.ID, "CI trigger unknown"})
		}
		if !win.scoredOutcomes[i].RankingEligible() {
			continue
		}
		e.Denominator += o.Weight
		if v.CI || v.Revert {
			e.Numerator += o.Weight
		}
		if v.CI {
			e.CI.Matched++
		}
		if v.Revert {
			e.Revert.Matched++
		}
		if !now.After(o.Timestamp.Add(qualityrule.CIObservationWindow)) {
			e.CI.WindowOpen++
		}
		if !now.After(o.Timestamp.Add(store.RevertObservationWindow)) {
			e.Revert.WindowOpen++
		}
		e.RecordedRows += v.RecordedRows
		e.RecoveredRows += v.RecoveredRows
		for wf := range v.Workflows {
			w := workflows[wf]
			if w == nil {
				w = &workflowEvidenceJSON{WorkflowID: wf}
				workflows[wf] = w
			}
			w.Numerator += o.Weight
			w.Matched++
		}
	}
	if e.Denominator > 0 {
		share := e.Numerator / e.Denominator
		e.Share = &share
		e.Label = fmt.Sprintf("size-weighted points, adjusted for observed quality signals; evidence: %.0f%%", share*100)
	}
	for _, w := range workflows {
		if e.Denominator > 0 {
			s := w.Numerator / e.Denominator
			w.Share = &s
		}
		e.Workflows = append(e.Workflows, *w)
	}
	sort.Slice(e.Workflows, func(i, j int) bool { return e.Workflows[i].WorkflowID < e.Workflows[j].WorkflowID })
	return e
}

func (h *Handler) loadQualityEvidence(ctx context.Context, r windowReader, win windowScores) (store.QualityEvidenceRead, error) {
	if h.aggregation.Anonymized() {
		return store.QualityEvidenceRead{}, nil
	}
	return r.QualityEvidenceWindow(ctx, win.since, win.until, win.scope)
}

func evidenceByDeveloper(win windowScores, read store.QualityEvidenceRead, now time.Time) map[string]qualityEvidenceJSON {
	groups := map[string]windowScores{}
	for i, o := range win.outcomes {
		dev := win.canon(o.Developer)
		g := groups[dev]
		g.canon = win.canon
		g.outcomes = append(g.outcomes, o)
		g.scoredOutcomes = append(g.scoredOutcomes, win.scoredOutcomes[i])
		groups[dev] = g
	}
	out := map[string]qualityEvidenceJSON{}
	for _, s := range win.devScores {
		out[s.Developer] = evidenceForWindow(groups[s.Developer], read, now, "")
	}
	return out
}
