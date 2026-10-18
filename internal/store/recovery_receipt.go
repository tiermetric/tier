package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

var recoveryReasonTags = [...]string{
	"already_assigned", "unique_recorded_pr", "unique_repo_branch_time",
	"missing_recorded_evidence", "backfill_without_branch_fill", "protected_sealed_band",
	"default_branch_without_worktree_evidence", "ambiguous_multi_claimant",
	"no_matching_recorded_outcome", "claimant_without_issue", "sessions_jsonl_gone",
}

func RecoveryReasonTags() []string   { return append([]string(nil), recoveryReasonTags[:]...) }
func RecoveryDispositions() []string { return append([]string(nil), recoveryDispositions[:]...) }

var recoveryDispositions = [...]string{"protected", "permitted_existing_fill", "unique_read_time_recovery", "ambiguous", "unresolved"}

type RecoveryTally struct {
	Events    int64 `json:"events"`
	CostMicro int64 `json:"cost_micro"`
}
type RecoveryReason struct {
	RecoveryTally
	Measured bool `json:"measured"`
}

func (r RecoveryReason) MarshalJSON() ([]byte, error) {
	if !r.Measured {
		return []byte(`{"measured":false}`), nil
	}
	type measuredReason RecoveryReason
	return json.Marshal(measuredReason(r))
}

type RecoveryReceiptEvent struct {
	EffectiveAttribution
	Disposition string   `json:"disposition"`
	ReasonTags  []string `json:"reason_tags"`
}
type RecoveryReceipt struct {
	Since              time.Time                 `json:"since"`
	Until              *time.Time                `json:"until,omitempty"`
	Repo               string                    `json:"repo,omitempty"`
	Population         string                    `json:"population"`
	Candidates         RecoveryTally             `json:"candidates"`
	AssignedReference  RecoveryTally             `json:"assigned_reference"`
	Dispositions       map[string]RecoveryTally  `json:"dispositions"`
	Reasons            map[string]RecoveryReason `json:"reason_tags"`
	Events             []RecoveryReceiptEvent    `json:"events,omitempty"`
	RecoveredCostShare float64                   `json:"recovered_cost_share"`
	Precision          string                    `json:"precision"`
	EvidenceLimit      string                    `json:"evidence_limit"`
}

// Validate reconciles both integer measures and requires one terminal per candidate.
func (r RecoveryReceipt) Validate() error {
	var sum RecoveryTally
	for _, name := range recoveryDispositions {
		v, ok := r.Dispositions[name]
		if !ok {
			return fmt.Errorf("missing disposition %s", name)
		}
		if v.Events < 0 || v.CostMicro < 0 || v.Events > math.MaxInt64-sum.Events || v.CostMicro > math.MaxInt64-sum.CostMicro {
			return fmt.Errorf("recovery disposition totals exceed integer range")
		}
		sum.Events += v.Events
		sum.CostMicro += v.CostMicro
	}
	if len(r.Dispositions) != len(recoveryDispositions) || sum != r.Candidates {
		return fmt.Errorf("recovery receipt does not reconcile: dispositions=%+v candidates=%+v", sum, r.Candidates)
	}
	seen := map[int64]bool{}
	counts := map[string]RecoveryTally{}
	var reference RecoveryTally
	var totalCost int64
	for _, e := range r.Events {
		if e.CostMicro < 0 || e.CostMicro > math.MaxInt64-totalCost {
			return fmt.Errorf("recovery event costs exceed integer range")
		}
		totalCost += e.CostMicro
		if seen[e.ID] {
			return fmt.Errorf("duplicate receipt event %d", e.ID)
		}
		seen[e.ID] = true
		if e.Disposition == "assigned_reference" {
			reference.Events++
			reference.CostMicro += e.CostMicro
			continue
		}
		if _, ok := r.Dispositions[e.Disposition]; !ok {
			return fmt.Errorf("invalid disposition %q", e.Disposition)
		}
		v := counts[e.Disposition]
		v.Events++
		v.CostMicro += e.CostMicro
		counts[e.Disposition] = v
	}
	if reference != r.AssignedReference {
		return fmt.Errorf("assigned reference does not reconcile")
	}
	for _, name := range recoveryDispositions {
		if counts[name] != r.Dispositions[name] {
			return fmt.Errorf("event accounting differs for %s", name)
		}
	}
	return nil
}

func (d *DB) RecoveryReceipt(ctx context.Context, since, until time.Time, scope RepoScope) (RecoveryReceipt, error) {
	var out RecoveryReceipt
	err := d.ReadSnapshot(ctx, func(s *Snapshot) error {
		var err error
		out, err = s.RecoveryReceipt(ctx, since, until, scope)
		return err
	})
	return out, err
}

func (r reader) RecoveryReceipt(ctx context.Context, since, until time.Time, scope RepoScope) (RecoveryReceipt, error) {
	ctx = WithReadTimeRecovery(ctx, true)
	out := RecoveryReceipt{Since: since.UTC(), Population: "current unattributed events plus server-marked #1074 fills; other assigned events are a separate reference population", Dispositions: map[string]RecoveryTally{}, Reasons: map[string]RecoveryReason{}, Events: []RecoveryReceiptEvent{}, Precision: "not measured; unique evidence is not a ground-truth precision audit", EvidenceLimit: "Missing evidence does not prove insertion before #1074. Session JSONL availability is not recorded; sessions_jsonl_gone is unmeasured. No filesystem, cwd or current checkout evidence is used."}
	if !until.IsZero() {
		u := until.UTC()
		out.Until = &u
	}
	if !scope.IsFleetWide() {
		out.Repo = scope.String()
	}
	for _, name := range recoveryDispositions {
		out.Dispositions[name] = RecoveryTally{}
	}
	for _, name := range recoveryReasonTags {
		out.Reasons[name] = RecoveryReason{Measured: name != "sessions_jsonl_gone"}
	}
	where, args := tsWindow(since, until)
	clause, scopeArgs := scope.clause()
	storedRows, err := r.q.QueryContext(ctx, `SELECT COUNT(*),COALESCE(SUM(cost_micro),0) FROM token_events WHERE `+where+clause, append(args, scopeArgs...)...)
	if err != nil {
		return out, err
	}
	var stored RecoveryTally
	if !storedRows.Next() {
		_ = storedRows.Close()
		return out, fmt.Errorf("stored recovery totals missing")
	}
	err = storedRows.Scan(&stored.Events, &stored.CostMicro)
	closeErr := storedRows.Close()
	if err != nil {
		return out, err
	}
	if closeErr != nil {
		return out, closeErr
	}

	rows, err := r.queryEffectiveWindow(ctx, since, until, scope, `SELECT id,ts,developer,repo,cost_micro,stored_issue_id,issue_id,attribution_layer,
 COALESCE(attribution_rule,''),pr_number IS NOT NULL,branch_hash IS NOT NULL,recovery_claimants,recovery_backfill,recovery_pr_match,recovery_protected,recovery_claimant_without_issue
 FROM token_events WHERE `+where+clause+` ORDER BY ts,id`, append(args, scopeArgs...)...)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	var capturedCost int64
	for rows.Next() {
		var e RecoveryReceiptEvent
		var rule string
		var pr, branch, backfill, prMatch, protected, claimantWithoutIssue bool
		var claims int
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.Developer, &e.Repo, &e.CostMicro, &e.StoredIssueID, &e.IssueID, &e.Layer, &rule, &pr, &branch, &claims, &backfill, &prMatch, &protected, &claimantWithoutIssue); err != nil {
			return out, err
		}
		if e.CostMicro < 0 || e.CostMicro > math.MaxInt64-capturedCost {
			return out, fmt.Errorf("captured cost exceeds recovery receipt integer range")
		}
		capturedCost += e.CostMicro
		fill := e.Layer == AttributionFill
		assigned := e.StoredIssueID != "" && !IsUnattributed(e.StoredIssueID) && !fill
		tag := func(name string, yes bool) {
			if !yes {
				return
			}
			e.ReasonTags = append(e.ReasonTags, name)
			v := out.Reasons[name]
			v.Events++
			v.CostMicro += e.CostMicro
			out.Reasons[name] = v
		}
		tag("already_assigned", assigned)
		tag("unique_recorded_pr", (e.Layer == AttributionRecovery && prMatch) || rule == string(AttributionRulePRNumberAtInsert))
		tag("unique_repo_branch_time", (e.Layer == AttributionRecovery && branch) || rule == string(AttributionRuleBranchAtMerge))
		tag("missing_recorded_evidence", !pr && !branch)
		tag("backfill_without_branch_fill", backfill && !fill)
		tag("protected_sealed_band", protected)
		tag("default_branch_without_worktree_evidence", e.StoredIssueID == UnattributedMainBucket)
		tag("ambiguous_multi_claimant", claims > 1)
		tag("no_matching_recorded_outcome", claims == 0 && !assigned && !fill)
		tag("claimant_without_issue", claimantWithoutIssue)
		switch {
		case assigned:
			e.Disposition = "assigned_reference"
			out.AssignedReference.Events++
			out.AssignedReference.CostMicro += e.CostMicro
		default:
			out.Candidates.Events++
			out.Candidates.CostMicro += e.CostMicro
			switch {
			case protected:
				e.Disposition = "protected"
			case fill:
				e.Disposition = "permitted_existing_fill"
			case e.Layer == AttributionRecovery:
				e.Disposition = "unique_read_time_recovery"
			case claims > 1:
				e.Disposition = "ambiguous"
			default:
				e.Disposition = "unresolved"
			}
			v := out.Dispositions[e.Disposition]
			v.Events++
			v.CostMicro += e.CostMicro
			out.Dispositions[e.Disposition] = v
		}
		if e.ReasonTags == nil {
			e.ReasonTags = []string{}
		}
		out.Events = append(out.Events, e)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if out.Candidates.Events+out.AssignedReference.Events != stored.Events || capturedCost != stored.CostMicro || out.Candidates.CostMicro+out.AssignedReference.CostMicro != stored.CostMicro {
		return out, fmt.Errorf("recovery receipt differs from stored totals: candidates=%+v assigned=%+v stored=%+v", out.Candidates, out.AssignedReference, stored)
	}
	if out.Candidates.CostMicro > 0 {
		out.RecoveredCostShare = float64(out.Dispositions["unique_read_time_recovery"].CostMicro) / float64(out.Candidates.CostMicro)
	}
	return out, out.Validate()
}
