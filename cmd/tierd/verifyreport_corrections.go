package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tiermetric/tier/internal/logsafe"
	"github.com/tiermetric/tier/internal/scoring"
	"github.com/tiermetric/tier/internal/store"
)

// Pointers distinguish an explicitly pinned zero from an omitted denominator.
type manifestOutcomeCounts struct {
	OutcomeN *int64 `json:"outcome_n"`
	SampleN  *int64 `json:"sample_n"`
}
type manifestCorrectionsDigest struct {
	Value  string                    `json:"value"`
	Counts *manifestCorrectionCounts `json:"counts"`
}
type manifestCorrectionCounts struct {
	Declarations       *int64 `json:"declarations"`
	Relations          *int64 `json:"relations"`
	Audit              *int64 `json:"audit"`
	Assessments        *int64 `json:"assessments"`
	Allocations        *int64 `json:"allocations"`
	DependencyOutcomes *int64 `json:"dependency_outcomes"`
}

func (c *manifestCorrectionCounts) values() (store.CorrectionDigestCounts, bool) {
	if c == nil || c.Declarations == nil || c.Relations == nil || c.Audit == nil || c.Assessments == nil || c.Allocations == nil || c.DependencyOutcomes == nil {
		return store.CorrectionDigestCounts{}, false
	}
	return store.CorrectionDigestCounts{Declarations: *c.Declarations, Relations: *c.Relations, Audit: *c.Audit, Assessments: *c.Assessments, Allocations: *c.Allocations, DependencyOutcomes: *c.DependencyOutcomes}, true
}

func manifestScoringPolicy(m reportManifest) (scoring.ScoringPolicy, verifyDim, error) {
	p := scoring.ScoringPolicy{}
	d := verifyDim{Name: "scoring policy", Status: dimNotPinned, Detail: "older manifest; replay uses legacy-v1 (rubric 1)"}
	if m.ScoringPolicy == "" && m.CorrectionRuleVersion == "" && m.RubricVersion == 0 {
		if m.Rubric.Version == 2 {
			return p, d, fmt.Errorf("rubric 2 requires an explicit Event 3 policy pin")
		}
		return p, d, nil
	}
	switch m.ScoringPolicy {
	case "legacy":
	case "event3":
		p = scoring.ScoringPolicy{Event3: true, Resolved: true, RuleVersion: m.CorrectionRuleVersion}
	default:
		return p, d, fmt.Errorf("unsupported scoring policy %s", logsafe.Str(m.ScoringPolicy))
	}
	stamp, err := p.Stamp()
	if err != nil {
		return p, d, err
	}
	if m.CorrectionRuleVersion != stamp.RuleVersion || m.RubricVersion != m.Rubric.Version {
		return p, d, fmt.Errorf("manifest policy/rule/rubric pins disagree; cannot replay a guessed policy")
	}
	// R-2026-10-14-20 retains CHANGED for an unsupported rubric. Both known
	// versions use their own rule; contradictory known tuples remain uncheckable.
	if m.RubricVersion != 1 && m.RubricVersion != 2 {
		d.Status = dimChanged
		d.Detail = fmt.Sprintf("unsupported rubric %d; replay used %s under its supported rule", m.RubricVersion, stamp.Policy)
		return p, d, nil
	}
	if m.RubricVersion != stamp.RubricVersion {
		return p, d, fmt.Errorf("manifest policy/rule/rubric pins disagree; cannot replay a guessed policy")
	}
	d.Status = dimUnchanged
	d.Detail = fmt.Sprintf("replayed %s, %s, rubric %d", stamp.Policy, stamp.RuleVersion, stamp.RubricVersion)
	return p, d, nil
}

var errReplayPolicyMismatch = errors.New("replayed policy/rubric NOT CHECKED")

// verifyReplayPolicy checks the serving reader's receipt, independently of the
// manifest's requested policy. U2 emits correction_policy only when enabled, so
// legacy /scores bytes retain their historical rubric-only shape.
func verifyReplayPolicy(scores json.RawMessage, policy scoring.ScoringPolicy) error {
	var replay struct {
		Rubric manifestRubric `json:"rubric"`
		scoring.CorrectionRuleStamp
		CorrectionPolicy *scoring.CorrectionRuleStamp `json:"correction_policy"`
	}
	if err := json.Unmarshal(scores, &replay); err != nil {
		return fmt.Errorf("replayed policy/rubric NOT CHECKED: %w", err)
	}
	want, err := policy.Stamp()
	if err != nil {
		return err
	}
	got := replay.CorrectionRuleStamp
	if replay.CorrectionPolicy != nil {
		// Never let a second, contradictory receipt hide behind the accepted one.
		if got != (scoring.CorrectionRuleStamp{}) && got != *replay.CorrectionPolicy {
			return fmt.Errorf("%w: conflicting correction_policy and top-level stamps", errReplayPolicyMismatch)
		}
		got = *replay.CorrectionPolicy
		if got.RubricVersion != want.RubricVersion {
			return fmt.Errorf("%w: correction_policy rubric %d; manifest requires %d", errReplayPolicyMismatch, got.RubricVersion, want.RubricVersion)
		}
	} else {
		// Older legacy readers stamp only rubric.version. It cannot stand in
		// for an Event 3 policy/rule stamp.
		legacy, _ := (scoring.ScoringPolicy{}).Stamp()
		if got.Policy == "" && got.RuleVersion == "" && replay.Rubric.Version == legacy.RubricVersion {
			got.Policy, got.RuleVersion = legacy.Policy, legacy.RuleVersion
		}
	}
	if replay.Rubric.Version != want.RubricVersion || got.Policy != want.Policy || got.RuleVersion != want.RuleVersion || (got.RubricVersion != 0 && got.RubricVersion != want.RubricVersion) {
		return fmt.Errorf("%w: reader returned %s/%s/rubric %d; manifest requires %s/%s/rubric %d", errReplayPolicyMismatch, logsafe.Str(got.Policy), logsafe.Str(got.RuleVersion), replay.Rubric.Version, want.Policy, want.RuleVersion, want.RubricVersion)
	}
	return nil
}

func verifyCorrectionPins(ctx context.Context, db *store.DB, m reportManifest, w manifestWindow, scope string, policy scoring.ScoringPolicy) ([]verifyDim, error) {
	dims := []verifyDim{}
	err := db.ReadSnapshot(ctx, func(s *store.Snapshot) error {
		d := verifyDim{Name: "corrections_digest", Status: dimNotPinned, Detail: "manifest carries no corrective evidence pin"}
		if m.DigestsOmitted != "" {
			d.Detail += ". Emitter says: " + logsafe.Str(m.DigestsOmitted)
		} else if m.CorrectionsDigest == nil && ((m.EventsDigest != nil && strings.HasPrefix(m.EventsDigest.Value, "tierdig2:")) || (m.OutcomesDigest != nil && strings.HasPrefix(m.OutcomesDigest.Value, "tierdig2:"))) {
			d.Status = dimNotCheckable
			d.Detail = "tierdig2 window pins require corrections_digest or an explicit omission reason"
		}
		if policy.Event3 && m.CorrectionsDigest == nil && (m.Results == nil || len(m.Results.Scores) == 0) {
			d.Status = dimNotCheckable
			d.Detail = "Event 3 window allocations require corrections_digest or results.scores; raw window pins do not cover allocations"
		}
		if m.CorrectionsDigest != nil {
			counts, present := m.CorrectionsDigest.Counts.values()
			if !present {
				d.Status = dimNotCheckable
				d.Detail = "corrective typed row counts missing; absent is not zero"
			} else if !strings.HasPrefix(m.CorrectionsDigest.Value, "tierdig2:") {
				d.Status = dimNotCheckable
				d.Detail = "unsupported corrective digest scheme"
			} else {
				got, err := s.CorrectionsDigest(ctx, w.Since, w.Until, store.RepoScope(scope))
				if err != nil {
					return err
				}
				d.Status = dimUnchanged
				d.Detail = "tierdig2 corrective evidence and typed counts unchanged"
				if got.Value != m.CorrectionsDigest.Value {
					d.Status = dimChanged
					d.Detail = "corrective evidence, disposition, assessment, allocation or dependency outcome changed"
				} else if got.Counts != counts {
					d.Status = dimNotCheckable
					d.Detail = "equal corrective digest with contradictory typed counts"
				}
				if !policy.Event3 && d.Status != dimNotCheckable {
					// Legacy scoring never consumes corrective evidence. Keep its
					// movement visible without counting it as a window comparison.
					d.Name = "corrections context"
					d.Status = dimUnchanged
					d.Detail = "informational corrections_digest: corrective evidence unchanged; legacy scoring does not read it"
					if got.Value != m.CorrectionsDigest.Value {
						d.Detail = "informational corrections_digest: corrective evidence moved; legacy scoring does not read it"
					}
				}
			}
		}
		dims = append(dims, d)
		d = verifyDim{Name: "outcome_counts", Status: dimNotPinned, Detail: "raw outcome and eligible sample counts were not pinned"}
		if m.OutcomeCounts != nil {
			got, err := s.CorrectionReportCounts(ctx, w.Since, w.Until, store.RepoScope(scope), policy.Event3)
			if err != nil {
				return err
			}
			d.Status = dimUnchanged
			d.Detail = fmt.Sprintf("outcome_n=%d sample_n=%d", got.OutcomeN, got.SampleN)
			if m.OutcomeCounts.OutcomeN == nil || m.OutcomeCounts.SampleN == nil {
				d.Status = dimNotCheckable
				d.Detail = "raw/eligible count missing; absent is not zero"
			} else if got.OutcomeN != *m.OutcomeCounts.OutcomeN || got.SampleN != *m.OutcomeCounts.SampleN {
				d.Status = dimChanged
				d.Detail += " (differs from pinned counts)"
			}
		}
		dims = append(dims, d)
		led := m.ledgers()
		d = verifyDim{Name: "corrective revisions", Status: dimNotPinned, Detail: "corrective audit/revision pins absent"}
		if led.MaxCorrectionAuditID != nil || led.CorrectionAuditCount != nil || led.CorrectionProjectionRevision != nil {
			// R9 intentionally withholds this revision on anonymized manifests.
			// Audit pins still need a complete max/count pair; undeclared gaps
			// remain uncheckable in every mode.
			revisionWithheld := (m.Aggregation == "team" || m.Aggregation == "division") &&
				m.KAnonSuppressed != nil && m.KAnonSuppressed.WithheldWindow && led.CorrectionProjectionRevision == nil
			if led.MaxCorrectionAuditID == nil || led.CorrectionAuditCount == nil || (led.CorrectionProjectionRevision == nil && !revisionWithheld) {
				d.Status = dimNotCheckable
				d.Detail = "corrective audit/revision tuple incomplete" +
					pinnedButUnexamined("max_correction_audit_id", led.MaxCorrectionAuditID) +
					pinnedButUnexamined("correction_audit_count", led.CorrectionAuditCount) +
					pinnedButUnexamined("correction_projection_revision", led.CorrectionProjectionRevision)
				dims = append(dims, d)
				return nil
			}
			marks, err := s.ReportWatermarks(ctx, w.Since, w.Until, store.RepoScope(scope))
			if err != nil {
				return err
			}
			got := marks.Ledgers
			d.Status = dimUnchanged
			d.Detail = "install-wide corrective audit unchanged"
			if !revisionWithheld {
				d.Detail = "install-wide corrective audit and projection revision unchanged"
			}
			if *led.MaxCorrectionAuditID != got.MaxCorrectionAuditID || *led.CorrectionAuditCount != got.CorrectionAuditCount ||
				(led.CorrectionProjectionRevision != nil && *led.CorrectionProjectionRevision != got.CorrectionProjectionRevision) {
				d.Detail = "install-wide corrective audit/revision moved; window coverage depends on corrections_digest or results.scores under Event 3 (informational under legacy)"
			}
			if revisionWithheld {
				d.Detail += "; projection revision withheld for anonymized aggregation"
			}
		}
		dims = append(dims, d)
		return nil
	})
	return dims, err
}
