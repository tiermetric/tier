package store

import (
	"context"
	"strconv"
	"strings"
	"time"
)

type CorrectionDiagnostic struct {
	OriginalRawWeight       string `json:"original_raw_weight"`
	OriginalCreditedWeight  string `json:"original_credited_weight"`
	OriginalAllocatedPoints string `json:"original_allocated_points"`
	AllocatedPoints         string `json:"allocated_points"`
	RelationID              int64  `json:"relation_id"`
	ChildDeveloper          string `json:"child_developer"`
	ParentDeveloper         string `json:"parent_developer"`
	ChildRepo               string `json:"child_repo"`
	ParentRepo              string `json:"parent_repo"`
	ChildPR                 int64  `json:"child_pr"`
	ParentPR                int64  `json:"parent_pr"`
	State                   string `json:"state"`
	Status                  string `json:"status"`
	ScoringStatus           string `json:"scoring_status"`
	PendingReason           string `json:"pending_reason,omitempty"`
	Escalation              string `json:"escalation,omitempty"`
	OriginalPeriod          string `json:"original_period"`
	ChildPeriod             string `json:"child_period"`
	OriginalReport          string `json:"original_report"`
	ChildReport             string `json:"child_report"`
	RawWeight               string `json:"raw_weight"`
	CreditedWeight          string `json:"credited_weight"`
	Disputed                bool   `json:"disputed"`
}
type CorrectionDiagnostics struct {
	Status                string                 `json:"status"`
	DeclarationCoverage   string                 `json:"declaration_coverage"`
	HistoryCoverage       string                 `json:"history_coverage"`
	CapturedSpendFidelity string                 `json:"captured_spend_fidelity"`
	CIOrRevertEvidence    bool                   `json:"ci_or_revert_evidence"`
	RepairsPerformed      map[string]int         `json:"repairs_performed"`
	RepairsReceived       map[string]int         `json:"repairs_received"`
	Relations             []CorrectionDiagnostic `json:"relations"`
	ClaimLimit            string                 `json:"claim_limit"`
}

// IsCIOrRevertEvidence is deliberately closed over the original five types.
// Neither corrective_relations nor an unresolved revert check is quality evidence.
func IsCIOrRevertEvidence(kind string) bool {
	switch kind {
	case "ci_pass", "ci_fail", "ci_fail_flaky", "revert_quality", "revert_strategic":
		return true
	}
	return false
}
func (r reader) CorrectionDiagnosticsWindow(ctx context.Context, since, until, now time.Time, scope RepoScope) (CorrectionDiagnostics, error) {
	d := CorrectionDiagnostics{Status: "shown", DeclarationCoverage: "incomplete", HistoryCoverage: "not_established", CapturedSpendFidelity: "reported_separately", Relations: []CorrectionDiagnostic{}, RepairsPerformed: map[string]int{}, RepairsReceived: map[string]int{}, ClaimLimit: "Declarations cover captured outcomes only. History coverage and captured-spend fidelity are separate; missing declarations or pending relations prevent a complete declaration claim. Allocation figures are per outcome; relation rows repeat them and must not be summed."}
	repo := ""
	if !scope.IsFleetWide() {
		repo = string(scope)
	}
	where, args := tsWindow(since, until)
	outcomeWhere := strings.ReplaceAll(where, "ts", "o.ts")
	var total, declared int64
	if err := r.q.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(EXISTS(SELECT 1 FROM correction_declarations d WHERE d.outcome_id=o.id AND d.declaration_state IN ('none','parents'))),0) FROM outcomes o WHERE `+outcomeWhere+` AND (?='' OR o.repo=?)`, append(args, repo, repo)...).Scan(&total, &declared); err != nil {
		return d, err
	}
	if total > 0 && declared == total {
		d.DeclarationCoverage = "complete_for_captured_outcomes"
	}
	var enabled bool
	var rule string
	if err := r.q.QueryRowContext(ctx, `SELECT scoring_enabled,rule_version FROM correction_policy WHERE id=1`).Scan(&enabled, &rule); err != nil {
		return d, err
	}
	relationWhere, relationArgs := correctionRelationWindow(since, until)
	rows, err := correctionRows(ctx, r.q, `SELECT r.*,a.status AS projection_status,a.provisional,a.pending_reason,a.raw_weight,a.share,a.allocated_points,pa.raw_weight AS original_raw_weight,pa.share AS original_share,pa.allocated_points AS original_allocated_points,
 EXISTS(SELECT 1 FROM correction_assessments x WHERE x.relation_id=r.id AND x.revision=r.revision AND x.participant_outcome_id=r.child_outcome_id AND x.basis='reviewed') AS reviewed
 FROM corrective_relations r LEFT JOIN correction_allocations a ON a.outcome_id=r.child_outcome_id LEFT JOIN correction_allocations pa ON pa.outcome_id=r.parent_outcome_id
 WHERE `+relationWhere+`
 AND (?='' OR r.child_repo=? OR r.parent_repo=?) ORDER BY r.id`, append(relationArgs, repo, repo, repo))
	if err != nil {
		return d, err
	}
	aliases, err := r.DeveloperAliases(ctx)
	if err != nil {
		return d, err
	}
	canonical := func(login string) string {
		if c, ok := aliases[login]; ok {
			return c
		}
		return login
	}
	performed := map[string]bool{}
	received := map[string]bool{}
	for _, row := range rows {
		c := CorrectionDiagnostic{RelationID: rowInt(row, "id"), ChildDeveloper: rowString(row, "child_developer"), ParentDeveloper: rowString(row, "parent_developer"), ChildRepo: rowString(row, "child_repo"), ParentRepo: rowString(row, "parent_repo"), ChildPR: rowInt(row, "child_pr_number"), ParentPR: rowInt(row, "parent_pr_number"), State: rowString(row, "state"), Status: rowString(row, "projection_status"), RawWeight: rowString(row, "raw_weight"), CreditedWeight: rowString(row, "share"), PendingReason: rowString(row, "pending_reason")}
		c.ChildDeveloper = canonical(c.ChildDeveloper)
		c.ParentDeveloper = canonical(c.ParentDeveloper)
		c.OriginalRawWeight = rowString(row, "original_raw_weight")
		c.OriginalCreditedWeight = rowString(row, "original_share")
		c.OriginalAllocatedPoints = rowString(row, "original_allocated_points")
		c.AllocatedPoints = rowString(row, "allocated_points")
		c.Disputed = c.State == "disputed" || c.State == "disputed-declared" || c.State == "withdrawn-confirmed"
		active := c.State != "removed" && c.State != "unrelated" && c.State != "withdrawn"
		if !active {
			c.Status = c.State
		} else if c.Status == "" || c.State == "pending" {
			c.Status = "pending"
		}
		if c.State == "excluded" {
			c.Status = "excluded"
		}
		if c.PendingReason == "" {
			c.PendingReason = rowString(row, "resolution_reason")
		}
		confirmed := c.State == "confirmed" || c.State == "disputed" || c.State == "withdrawn-confirmed"
		if confirmed && rowInt(row, "reviewed") == 0 && c.Status != "pending" {
			c.Status = "confirmed_with_provisional_sizing"
		}
		c.ScoringStatus = "recorded, not yet scored"
		if enabled {
			c.ScoringStatus = "event3"
			if rule != "event3-v2" {
				c.ScoringStatus = "unresolved, scores refused"
			}
		}
		if c.Status == "pending" || c.Status == "excluded" {
			d.DeclarationCoverage = "incomplete"
		}
		if recorded, ok := row["recorded_at"].(time.Time); ok && active && !confirmed && !now.Before(recorded.Add(30*24*time.Hour)) {
			c.Escalation = "unadjudicated_30_days"
		}
		if at, ok := row["parent_merged_at"].(time.Time); ok {
			c.OriginalPeriod = at.UTC().Format("2006-01")
		}
		if at, ok := row["child_merged_at"].(time.Time); ok {
			c.ChildPeriod = at.UTC().Format("2006-01")
		}
		if active {
			childKey := c.ChildRepo + "#" + strconv.FormatInt(c.ChildPR, 10)
			if c.ChildDeveloper != "" && !performed[childKey] {
				d.RepairsPerformed[c.ChildDeveloper]++
				performed[childKey] = true
			}
			parentKey := childKey + "->" + c.ParentRepo + "#" + strconv.FormatInt(c.ParentPR, 10)
			if c.ParentDeveloper != "" && !received[parentKey] {
				d.RepairsReceived[c.ParentDeveloper]++
				received[parentKey] = true
			}
		}
		c.OriginalReport = "unknown"
		c.ChildReport = "unknown"
		for _, entry := range []struct {
			key    string
			target *string
		}{{"parent_merged_at", &c.OriginalReport}, {"child_merged_at", &c.ChildReport}} {
			if at, ok := row[entry.key].(time.Time); ok {
				var sealed bool
				if err := r.q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sealed_report WHERE period_start<=? AND period_end>?)`, sealTime(at), sealTime(at)).Scan(&sealed); err != nil {
					return d, err
				}
				*entry.target = "live"
				if sealed {
					*entry.target = "sealed_preserved"
				}
			}
		}

		d.Relations = append(d.Relations, c)
	}
	evidence, err := r.q.QueryContext(ctx, `SELECT DISTINCT q.event_type FROM quality_events q JOIN outcomes o ON o.id=q.outcome_id WHERE `+outcomeWhere+` AND (?='' OR o.repo=?)`, append(args, repo, repo)...)
	if err != nil {
		return d, err
	}
	defer func() { _ = evidence.Close() }()
	for evidence.Next() {
		var kind string
		if err = evidence.Scan(&kind); err != nil {
			return d, err
		}
		d.CIOrRevertEvidence = d.CIOrRevertEvidence || IsCIOrRevertEvidence(kind)
	}
	return d, evidence.Err()
}

// Report dispute disclosure depends only on stored relations and the window.
// Escalation clocks and seal availability belong exclusively to diagnostics.
type CorrectionDispute struct {
	RelationID int64  `json:"relation_id"`
	State      string `json:"state"`
	Disputed   bool   `json:"disputed"`
}
type CorrectionDisputes struct {
	Status            string              `json:"status"`
	DisputedRelations int                 `json:"disputed_relations"`
	Relations         []CorrectionDispute `json:"relations"`
}

func correctionRelationWindow(since, until time.Time) (string, []any) {
	where, args := tsWindow(since, until)
	var clauses []string
	var bound []any
	for _, column := range []string{"r.child_merged_at", "r.parent_merged_at", "r.recorded_at"} {
		clause := strings.ReplaceAll(where, "ts", column)
		if column == "r.recorded_at" {
			clause = "r.child_merged_at IS NULL AND " + clause
		}
		clauses = append(clauses, "("+clause+")")
		bound = append(bound, args...)
	}
	return "(" + strings.Join(clauses, " OR ") + ")", bound
}
func (r reader) CorrectionDisputesWindow(ctx context.Context, since, until time.Time, scope RepoScope) (CorrectionDisputes, error) {
	d := CorrectionDisputes{Status: "shown", Relations: []CorrectionDispute{}}
	where, args := correctionRelationWindow(since, until)
	repo := ""
	if !scope.IsFleetWide() {
		repo = string(scope)
	}
	rows, err := r.q.QueryContext(ctx, `SELECT r.id,r.state FROM corrective_relations r WHERE `+where+` AND (?='' OR r.child_repo=? OR r.parent_repo=?) ORDER BY r.id`, append(args, repo, repo, repo)...)
	if err != nil {
		return d, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var v CorrectionDispute
		if err = rows.Scan(&v.RelationID, &v.State); err != nil {
			return d, err
		}
		v.Disputed = v.State == "disputed" || v.State == "disputed-declared" || v.State == "withdrawn-confirmed"
		if v.Disputed {
			d.DisputedRelations++
		}
		d.Relations = append(d.Relations, v)
	}
	return d, rows.Err()
}
