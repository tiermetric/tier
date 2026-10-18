package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type AttributionLayer string

const (
	AttributionStored     AttributionLayer = "stored_assignment"
	AttributionFill       AttributionLayer = "existing_fill"
	AttributionRecovery   AttributionLayer = "read_time_recovery"
	AttributionUnresolved AttributionLayer = "unresolved"
)

// EffectiveAttribution preserves event identity, money and time at every layer.
type EffectiveAttribution struct {
	ID            int64            `json:"id"`
	Timestamp     time.Time        `json:"timestamp"`
	Developer     string           `json:"developer"`
	Repo          string           `json:"repo"`
	CostMicro     int64            `json:"cost_micro"`
	StoredIssueID string           `json:"stored_issue_id"`
	IssueID       string           `json:"effective_issue_id"`
	Layer         AttributionLayer `json:"attribution_layer"`
}

// AttributionOverlay may replace only an issue on an unsealed event. No
// implementation or activation is provided under U93 option A.
type AttributionOverlay interface {
	Lookup(context.Context, EffectiveAttribution) (issueID string, found bool, err error)
}

type recoverySwitchKey struct{}

// WithReadTimeRecovery selects recovery for a read without changing stored facts.
func WithReadTimeRecovery(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, recoverySwitchKey{}, enabled)
}

func recoveryEnabled(ctx context.Context) bool {
	enabled, ok := ctx.Value(recoverySwitchKey{}).(bool)
	return !ok || enabled
}

const effectiveEventColumns = "id, ts, developer, repo, cost_micro, model, host, source, fidelity, session_id, input_tok, output_tok, cache_read, cache_write_5m, cache_write_1h, pr_number, branch_hash, attribution_rule"

var unattributedIssueSQL = "issue_id='' OR (" + strings.Replace(strings.Replace(unattributedFamilyPredicate, "?", "'"+UnattributedIssueID+"'", 1), "?", "'"+unattributedGlobPattern+"'", 1) + ")"

const fillRulesSQL = "'" + string(AttributionRulePRNumberAtInsert) + "','" + string(AttributionRuleBranchAtMerge) + "'"

var storedAttributionLayerSQL = `CASE
 WHEN ` + unattributedIssueSQL + ` THEN 'unresolved'
 WHEN attribution_rule IN (` + fillRulesSQL + `) THEN 'existing_fill'
 ELSE 'stored_assignment' END`

func effectiveAttributionCTE(ctx context.Context, index string) string {
	if recoveryEnabled(ctx) {
		return recoveryAttributionCTE(ctx, index)
	}
	return `WITH effective_events AS (SELECT ` + effectiveEventColumns + `, issue_id, issue_id AS stored_issue_id, ` + storedAttributionLayerSQL + ` AS attribution_layer
 FROM token_events` + index + `) `
}

// queryEffective is the common read seam. Raw exports and input digests remain
// stored evidence; issue-cost folds and token eligibility use this projection.
func (r reader) queryEffective(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	statement, err := effectiveStatement(ctx, query)
	if err != nil {
		return nil, err
	}

	if recoveryEnabled(ctx) {
		if window, ok := ctx.Value(effectiveWindowKey{}).(effectiveWindow); ok {
			args = append(append([]any{}, window.args...), args...)
		}
	}
	return r.q.QueryContext(ctx, statement, args...)
}

var tokenEventsReference = regexp.MustCompile(`\btoken_events\b`)

func effectiveStatement(ctx context.Context, query string) (string, error) {
	if len(tokenEventsReference.FindAllStringIndex(query, -1)) != 1 {
		return "", fmt.Errorf("effective attribution requires exactly one token_events reference")
	}
	index := ""
	const hint = " INDEXED BY idx_token_events_ts_id"
	if strings.Contains(query, hint) {
		index = hint
		query = strings.ReplaceAll(query, hint, "")
	}
	query = strings.ReplaceAll(query, "token_events", "effective_events")
	return effectiveAttributionCTE(ctx, index) + query, nil
}

func (d *DB) EffectiveAttributionsWindow(ctx context.Context, since, until time.Time, scope RepoScope) ([]EffectiveAttribution, error) {
	var out []EffectiveAttribution
	err := d.ReadSnapshot(ctx, func(s *Snapshot) error {
		var err error
		out, err = s.EffectiveAttributionsWindow(ctx, since, until, scope)
		return err
	})
	return out, err
}

func (r reader) EffectiveAttributionsWindow(ctx context.Context, since, until time.Time, scope RepoScope) ([]EffectiveAttribution, error) {
	where, args := tsWindow(since, until)
	clause, scopeArgs := scope.clause()
	rows, err := r.queryEffectiveWindow(ctx, since, until, scope, `SELECT id,ts,developer,repo,cost_micro,stored_issue_id,issue_id,attribution_layer FROM token_events WHERE `+where+clause+` ORDER BY ts,id`, append(args, scopeArgs...)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []EffectiveAttribution
	for rows.Next() {
		var e EffectiveAttribution
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.Developer, &e.Repo, &e.CostMicro, &e.StoredIssueID, &e.IssueID, &e.Layer); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type effectiveWindowKey struct{}
type effectiveWindow struct {
	where string
	args  []any
}

func (r reader) queryEffectiveWindow(ctx context.Context, since, until time.Time, scope RepoScope, query string, args ...any) (*sql.Rows, error) {
	where, binds := tsWindow(since, until)
	clause, scopeArgs := scope.clause()
	ctx = context.WithValue(ctx, effectiveWindowKey{}, effectiveWindow{where + clause, append(binds, scopeArgs...)})
	return r.queryEffective(ctx, query, args...)
}
